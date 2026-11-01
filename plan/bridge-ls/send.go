package main

// Gửi tin nhân viên trả lời trong Libredesk, đúng cách Business Suite gửi (bắt
// frame 01/11/2026, README "Gửi tin"): mỗi tin là một stream dùng một lần trên
// chính socket DGW, data type 3 với task label 46 {thread_id, otid, source,
// send_type 1, sync_group 205, mark_thread_read, text, reply_metadata?}. Server
// trả replaceOptimsiticMessage [otid, mid] trên stream đó khi đã nhận tin.
//
// fbweb-ls hỏi hàng đợi gửi của fb-bridge-web (POST /outbox, chờ tối đa vài chục
// giây, qua SSH tunnel sẵn có) rồi báo kết quả (POST /outbox/result). Chỉ chạy
// khi FBLS_SEND=1. Chữ (có thể trả lời một tin) và tệp (upload.go, tải lên trước,
// mỗi tệp một task send_type 3 trong cùng request).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"go.mau.fi/mautrix-meta/pkg/messagix/dgw"
)

const (
	maxOutboxBody = 96 << 20 // tin kèm tệp (base64)
	sendTaskLabel = "46"
	sendSource    = 4456448          // source của tin chữ gửi từ Business Suite
	sendTimeout   = 15 * time.Second // fb-bridge-web chờ kết quả 19 giây
)

// outJob: một tin trong hàng đợi gửi của fb-bridge-web.
type outJob struct {
	ID       string    `json:"id"`
	Kind     string    `json:"kind,omitempty"` // "" = gửi tin, "react" = thả/gỡ cảm xúc (react.go)
	ThreadID string    `json:"thread_id"`
	Text     string    `json:"text"`
	ReplyTo  string    `json:"reply_to,omitempty"`
	Otid     string    `json:"otid"` // của task chữ
	Files    []outFile `json:"files,omitempty"`
	// react: tin được thả cảm xúc và emoji ("" = gỡ).
	MessageID string `json:"message_id,omitempty"`
	Emoji     string `json:"emoji,omitempty"`
}

// outFile: tệp nhân viên gửi (base64 trong JSON), mỗi tệp một otid riêng để
// fb-bridge-web nhận ra tin quay lại trên socket.
type outFile struct {
	Name        string `json:"name"`
	ContentType string `json:"content_type"`
	Data        []byte `json:"data"`
	Otid        string `json:"otid"`
}

// outResult: OK kèm mid; hoặc Error. Retry: chắc chắn chưa gửi, gửi lại an toàn.
// Unknown: đã đưa lên socket mà không có xác nhận, có thể đã tới khách.
type outResult struct {
	ID      string   `json:"id"`
	OK      bool     `json:"ok"`
	Mid     string   `json:"mid,omitempty"`  // mid đầu tiên
	Mids    []string `json:"mids,omitempty"` // mỗi tệp rồi chữ, theo thứ tự gửi
	Error   string   `json:"error,omitempty"`
	Retry   bool     `json:"retry,omitempty"`
	Unknown bool     `json:"unknown,omitempty"`
}

type sendTask struct {
	ThreadID        int64          `json:"thread_id"`
	Otid            string         `json:"otid"`
	Source          int64          `json:"source"`
	SendType        int            `json:"send_type"`
	SyncGroup       json.Number    `json:"sync_group"`
	MarkThreadRead  int            `json:"mark_thread_read"`
	Text            *string        `json:"text"` // null với task tệp
	AttachmentFBIDs []int64        `json:"attachment_fbids,omitempty"`
	ReplyMetadata   *replyMetadata `json:"reply_metadata,omitempty"`
}

type replyMetadata struct {
	ReplySourceID           string  `json:"reply_source_id"`
	ReplySourceType         int     `json:"reply_source_type"`
	ReplyType               int     `json:"reply_type"`
	ReplySourceAttachmentID *string `json:"reply_source_attachment_id"`
}

type taskData struct {
	FailureCount *int   `json:"failure_count"`
	Label        string `json:"label"`
	Payload      string `json:"payload"`
	QueueName    string `json:"queue_name"`
	TaskID       int    `json:"task_id"`
}

type taskPayload struct {
	EpochID   int64      `json:"epoch_id"`
	Tasks     []taskData `json:"tasks"`
	VersionID string     `json:"version_id"`
}

// sendLoop chỉ hỏi hàng đợi khi socket đã theo kịp server; lúc khác fb-bridge-web
// thấy không ai nhận tin và báo gateway thử lại.
func (l *Listener) sendLoop(ctx context.Context) {
	for ctx.Err() == nil {
		if !l.connected.Load() || !l.caughtUp.Load() {
			sleepCtx(ctx, 2*time.Second)
			continue
		}
		job, err := l.fwd.Poll(ctx)
		if err != nil {
			l.log.Warn("outbox poll failed", "error", err)
			sleepCtx(ctx, 5*time.Second)
			continue
		}
		if job == nil {
			continue
		}
		res := l.sendJob(ctx, *job)
		if res.OK && job.Kind == "react" {
			l.log.Info("reaction sent", "job", res.ID)
		} else if res.OK {
			l.log.Info("message sent", "job", res.ID, "mid", res.Mid)
		} else {
			l.log.Warn("message not sent", "job", res.ID, "retry", res.Retry, "unknown", res.Unknown, "error", res.Error)
		}
		l.fwd.Report(ctx, res)
	}
}

func (l *Listener) sendJob(ctx context.Context, j outJob) outResult {
	if j.Kind == "react" {
		return l.reactJob(ctx, j)
	}
	res := outResult{ID: j.ID}
	thread, err := strconv.ParseInt(j.ThreadID, 10, 64)
	valid := err == nil && thread > 0 && (strings.TrimSpace(j.Text) == "" || j.Otid != "") &&
		(strings.TrimSpace(j.Text) != "" || len(j.Files) > 0)
	for _, f := range j.Files {
		valid = valid && f.Otid != ""
	}
	if !valid {
		res.Error = "Yêu cầu gửi không hợp lệ (thiếu hội thoại, nội dung hoặc otid)."
		return res
	}
	l.mu.Lock()
	sock := l.sock
	ready := l.info != nil
	l.mu.Unlock()
	if sock == nil || !ready || !l.connected.Load() || !l.caughtUp.Load() {
		res.Error, res.Retry = "fbweb-ls chưa kết nối Facebook.", true
		return res
	}
	// Tải tệp lên trước: lỗi ở đây nghĩa là chưa gửi gì tới khách.
	fbids := make([]int64, 0, len(j.Files))
	for _, f := range j.Files {
		id, err := l.upload(ctx, f)
		if err != nil {
			res.Error = fmt.Sprintf("Không tải được tệp «%s» lên Facebook: %s. Chưa gửi gì tới khách.", f.Name, err)
			return res
		}
		fbids = append(fbids, id)
	}
	tasks := tasksFor(thread, j, fbids)
	payload, err := l.sendRequest(sendTaskLabel, j.ThreadID, taskPayloads(tasks))
	if err != nil {
		res.Error = "Không tạo được yêu cầu gửi."
		return res
	}
	sctx, cancel := context.WithTimeout(ctx, sendTimeout)
	defer cancel()
	resp, err := sock.DoOneOffStream(sctx, payload, false)
	if err != nil {
		if errors.Is(err, dgw.ErrSocketNotOpen) {
			res.Error, res.Retry = "fbweb-ls mất kết nối Facebook trước khi gửi.", true
			return res
		}
		l.log.Warn("send stream failed", "job", j.ID, "error", err)
		res.Error, res.Unknown = "Không rõ tin đã tới khách chưa (Facebook không trả lời). Xem trong Business Suite trước khi gửi lại.", true
		return res
	}
	_, calls, _ := decodeFrame(resp)
	mids := map[string]string{}
	for _, c := range calls {
		if c.Name == "replaceOptimsiticMessage" && str(c.Args, 1) != "" {
			mids[str(c.Args, 0)] = str(c.Args, 1)
		}
	}
	for _, t := range tasks {
		if mid := mids[t.otid()]; mid != "" {
			res.Mids = append(res.Mids, mid)
		}
	}
	switch {
	case len(res.Mids) == len(tasks):
		res.OK, res.Mid = true, res.Mids[0]
	case len(res.Mids) > 0:
		res.Mid, res.Unknown = res.Mids[0], true
		res.Error = fmt.Sprintf("Facebook chỉ xác nhận %d/%d phần của tin (tệp gửi trước, chữ sau). Xem trong Business Suite trước khi gửi lại.", len(res.Mids), len(tasks))
	default:
		res.Error, res.Unknown = fmt.Sprintf("Facebook không xác nhận đã gửi (%s). Xem trong Business Suite trước khi gửi lại.", procedureNames(calls)), true
	}
	return res
}

// sendRequest dựng request type 3, mỗi payload một task cùng label và hàng đợi,
// thứ tự khoá như trang gửi.
func (l *Listener) sendRequest(label, queue string, payloads []any) ([]byte, error) {
	data := make([]taskData, 0, len(payloads))
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.info == nil {
		return nil, errors.New("no inbox page info")
	}
	for _, p := range payloads {
		inner, err := json.Marshal(p)
		if err != nil {
			return nil, err
		}
		l.taskID++
		data = append(data, taskData{Label: label, Payload: string(inner), QueueName: queue, TaskID: l.taskID})
	}
	outer, err := json.Marshal(taskPayload{EpochID: epochID(), VersionID: l.info.Version, Tasks: data})
	if err != nil {
		return nil, err
	}
	l.requestID++
	return json.Marshal(lsRequest{AppID: l.info.AppID, Payload: string(outer), RequestID: l.requestID, Type: 3})
}

func procedureNames(calls []call) string {
	seen := map[string]bool{}
	var names []string
	for _, c := range calls {
		if !seen[c.Name] {
			seen[c.Name] = true
			names = append(names, c.Name)
		}
	}
	sort.Strings(names)
	if len(names) == 0 {
		return "không có phản hồi Lightspeed"
	}
	return strings.Join(names, ", ")
}

// Poll hỏi một tin cần gửi; nil khi hết thời gian chờ mà không có tin.
func (f *Forwarder) Poll(ctx context.Context) (*outJob, error) {
	status, body := f.postRead(ctx, "/outbox", nil, maxOutboxBody)
	switch status {
	case http.StatusOK:
		var j outJob
		if err := json.Unmarshal(body, &j); err != nil || j.ID == "" {
			return nil, fmt.Errorf("invalid outbox job")
		}
		return &j, nil
	case http.StatusNoContent:
		return nil, nil
	}
	return nil, fmt.Errorf("outbox HTTP %d", status)
}

// Report báo kết quả; thử lại vài lần vì fb-bridge-web đang giữ yêu cầu của gateway.
func (f *Forwarder) Report(ctx context.Context, r outResult) {
	for i := 0; i < 3; i++ {
		if f.post(ctx, "/outbox/result", map[string]any{"result": r}) == http.StatusOK {
			return
		}
		sleepCtx(ctx, 2*time.Second)
	}
	f.log.Warn("outbox result not delivered", "job", r.ID)
}

func sleepCtx(ctx context.Context, d time.Duration) {
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}
