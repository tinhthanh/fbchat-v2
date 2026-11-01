package main

// Gửi tin nhân viên trả lời trong Libredesk, đúng cách Business Suite gửi (bắt
// frame 01/11/2026, README "Gửi tin"): mỗi tin là một stream dùng một lần trên
// chính socket DGW, data type 3 với task label 46 {thread_id, otid, source,
// send_type 1, sync_group 205, mark_thread_read, text, reply_metadata?}. Server
// trả replaceOptimsiticMessage [otid, mid] trên stream đó khi đã nhận tin.
//
// fbweb-ls hỏi hàng đợi gửi của fb-bridge-web (POST /outbox, chờ tối đa vài chục
// giây, qua SSH tunnel sẵn có) rồi báo kết quả (POST /outbox/result). Chỉ chạy
// khi FBLS_SEND=1. Chỉ gửi chữ (có thể trả lời một tin); tệp chưa hỗ trợ.

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
	sendTaskLabel = "46"
	sendSource    = 4456448          // source của tin chữ gửi từ Business Suite
	sendTimeout   = 15 * time.Second // fb-bridge-web chờ kết quả 19 giây
)

// outJob: một tin trong hàng đợi gửi của fb-bridge-web.
type outJob struct {
	ID       string `json:"id"`
	ThreadID string `json:"thread_id"`
	Text     string `json:"text"`
	ReplyTo  string `json:"reply_to,omitempty"`
	Otid     string `json:"otid"`
}

// outResult: OK kèm mid; hoặc Error. Retry: chắc chắn chưa gửi, gửi lại an toàn.
// Unknown: đã đưa lên socket mà không có xác nhận, có thể đã tới khách.
type outResult struct {
	ID      string `json:"id"`
	OK      bool   `json:"ok"`
	Mid     string `json:"mid,omitempty"`
	Error   string `json:"error,omitempty"`
	Retry   bool   `json:"retry,omitempty"`
	Unknown bool   `json:"unknown,omitempty"`
}

type sendTask struct {
	ThreadID       int64          `json:"thread_id"`
	Otid           string         `json:"otid"`
	Source         int64          `json:"source"`
	SendType       int            `json:"send_type"`
	SyncGroup      json.Number    `json:"sync_group"`
	MarkThreadRead int            `json:"mark_thread_read"`
	Text           string         `json:"text"`
	ReplyMetadata  *replyMetadata `json:"reply_metadata,omitempty"`
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
		if res.OK {
			l.log.Info("message sent", "job", res.ID, "mid", res.Mid)
		} else {
			l.log.Warn("message not sent", "job", res.ID, "retry", res.Retry, "unknown", res.Unknown, "error", res.Error)
		}
		l.fwd.Report(ctx, res)
	}
}

func (l *Listener) sendJob(ctx context.Context, j outJob) outResult {
	res := outResult{ID: j.ID}
	thread, err := strconv.ParseInt(j.ThreadID, 10, 64)
	if err != nil || thread <= 0 || strings.TrimSpace(j.Text) == "" || j.Otid == "" {
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
	payload, err := l.sendRequest(thread, j)
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
	for _, c := range calls {
		if c.Name == "replaceOptimsiticMessage" && str(c.Args, 0) == j.Otid && str(c.Args, 1) != "" {
			res.OK, res.Mid = true, str(c.Args, 1)
			return res
		}
	}
	names := procedureNames(calls)
	res.Error, res.Unknown = fmt.Sprintf("Facebook không xác nhận đã gửi (%s). Xem trong Business Suite trước khi gửi lại.", names), true
	return res
}

// sendRequest dựng request type 3 với một task 46, thứ tự khoá như trang gửi.
func (l *Listener) sendRequest(thread int64, j outJob) ([]byte, error) {
	t := sendTask{ThreadID: thread, Otid: j.Otid, Source: sendSource, SendType: 1,
		SyncGroup: json.Number(lsDatabase), MarkThreadRead: 1, Text: j.Text}
	if j.ReplyTo != "" {
		t.ReplyMetadata = &replyMetadata{ReplySourceID: j.ReplyTo, ReplySourceType: 1}
	}
	inner, err := json.Marshal(t)
	if err != nil {
		return nil, err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.info == nil {
		return nil, errors.New("no inbox page info")
	}
	l.taskID++
	outer, err := json.Marshal(taskPayload{EpochID: epochID(), VersionID: l.info.Version, Tasks: []taskData{{
		Label: sendTaskLabel, Payload: string(inner), QueueName: j.ThreadID, TaskID: l.taskID,
	}}})
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
	status, body := f.postRead(ctx, "/outbox", nil)
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
