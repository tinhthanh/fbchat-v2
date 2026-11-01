package main

// Tải tệp lên trước khi gửi, như Business Suite (bắt frame 01/11/2026):
// POST upload-business.facebook.com/ajax/mercury/upload.php?request_user_id=<Trang>
// &__user=<c_user>&__a=1&__req=..&fb_dtsg=..&jazoest=..&lsd=.., thân multipart chỉ
// một trường "upload_<số>" là tệp. Trả "for (;;);{payload:{metadata:{"0":{image_id
// | file_id | video_id | audio_id | gif_id}}}}"; id đó vào attachment_fbids của task 46.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"strconv"
	"strings"
)

const (
	uploadURL      = "https://upload-business.facebook.com/ajax/mercury/upload.php"
	maxUploadBytes = 25 << 20 // giới hạn tệp của Messenger
)

var uploadIDKeys = []string{"image_id", "file_id", "video_id", "audio_id", "gif_id", "fbid"}

// jazoest như trang: "2" + tổng mã ký tự của fb_dtsg.
func jazoest(dtsg string) string {
	sum := 0
	for _, r := range dtsg {
		sum += int(r)
	}
	return "2" + strconv.Itoa(sum)
}

// upload trả id tệp trên Facebook. Lỗi chỉ ghi tên trường, không ghi token.
func (l *Listener) upload(ctx context.Context, f outFile) (int64, error) {
	if len(f.Data) == 0 {
		return 0, errors.New("tệp rỗng")
	}
	if len(f.Data) > maxUploadBytes {
		return 0, errors.New("tệp quá 25 MB")
	}
	l.mu.Lock()
	info := l.info
	l.uploadSeq++
	seq := l.uploadSeq
	l.mu.Unlock()
	if info == nil || info.DTSG == "" || info.LSD == "" {
		return 0, errors.New("trang Hộp thư chưa cho fb_dtsg, lsd")
	}
	// Thứ tự tham số như trang gửi (url.Values.Encode sẽ sắp xếp lại).
	params := [][2]string{
		{"request_user_id", l.cfg.PageID}, {"__user", l.jar.Get("c_user")}, {"__a", "1"},
		{"__req", strconv.FormatInt(int64(seq), 36)}, {"fb_dtsg", info.DTSG}, {"jazoest", jazoest(info.DTSG)}, {"lsd", info.LSD},
	}
	parts := make([]string, len(params))
	for i, p := range params {
		parts[i] = url.QueryEscape(p[0]) + "=" + url.QueryEscape(p[1])
	}
	target := l.uploadURL + "?" + strings.Join(parts, "&")

	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	h := textproto.MIMEHeader{}
	h.Set("Content-Disposition", fmt.Sprintf(`form-data; name="upload_%d"; filename="%s"`, 1023+seq, escapeQuotes(f.Name)))
	ct := f.ContentType
	if ct == "" {
		ct = http.DetectContentType(f.Data)
	}
	h.Set("Content-Type", ct)
	w, err := mw.CreatePart(h)
	if err != nil {
		return 0, err
	}
	if _, err := w.Write(f.Data); err != nil {
		return 0, err
	}
	if err := mw.Close(); err != nil {
		return 0, err
	}

	ua := l.cfg.UserAgent
	resp, err := l.http.R().SetContext(ctx).DisableAutoReadResponse().
		SetHeaders(map[string]string{
			"user-agent": ua.UA, "sec-ch-ua": ua.SecCHUA, "sec-ch-ua-mobile": "?0", "sec-ch-ua-platform": ua.Platform,
			"accept": "*/*", "accept-language": acceptLanguage, "origin": lsOrigin, "referer": lsOrigin + "/",
			"sec-fetch-dest": "empty", "sec-fetch-mode": "cors", "sec-fetch-site": "same-site",
			"content-type": mw.FormDataContentType(), "cookie": l.jar.Header(),
		}).
		SetBodyBytes(body.Bytes()).
		Post(target)
	if err != nil {
		return 0, errors.New("lỗi mạng khi tải lên")
	}
	defer resp.Body.Close()
	l.jar.Update(resp.Header)
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("Facebook trả HTTP %d khi tải lên", resp.StatusCode)
	}
	return parseUploadResponse(raw)
}

func parseUploadResponse(raw []byte) (int64, error) {
	raw = bytes.TrimPrefix(bytes.TrimSpace(raw), []byte("for (;;);"))
	var r struct {
		Error        any    `json:"error"`
		ErrorSummary string `json:"errorSummary"`
		Payload      struct {
			Metadata map[string]map[string]any `json:"metadata"`
		} `json:"payload"`
	}
	if err := decodeJSON(raw, &r); err != nil {
		return 0, errors.New("Facebook trả phản hồi tải lên không đọc được")
	}
	if r.Error != nil && fmt.Sprint(r.Error) != "0" {
		msg := strings.TrimSpace(r.ErrorSummary)
		if msg == "" {
			msg = "mã " + fmt.Sprint(r.Error)
		}
		return 0, fmt.Errorf("Facebook từ chối tệp: %s", msg)
	}
	item := r.Payload.Metadata["0"]
	for _, k := range uploadIDKeys {
		// Id có lúc là số, có lúc là chuỗi (bắt frame 01/11/2026).
		if v, ok := item[k]; ok && v != nil {
			if id, err := strconv.ParseInt(fmt.Sprint(v), 10, 64); err == nil && id > 0 {
				return id, nil
			}
		}
	}
	return 0, errors.New("Facebook không trả id tệp")
}

func escapeQuotes(s string) string {
	return strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\r", "", "\n", "").Replace(s)
}

// tasksFor: mỗi tệp một task 46 (send_type 3, text null) rồi task chữ, cùng một
// request như trang gửi ảnh kèm chữ. Trích dẫn gắn vào task cuối (task chữ nếu có).
func tasksFor(thread int64, j outJob, fbids []int64) []sendTask {
	var tasks []sendTask
	for i, id := range fbids {
		tasks = append(tasks, sendTask{ThreadID: thread, Otid: j.Files[i].Otid, Source: 0, SendType: 3,
			SyncGroup: json.Number(lsDatabase), AttachmentFBIDs: []int64{id}})
	}
	if strings.TrimSpace(j.Text) != "" {
		text := j.Text
		tasks = append(tasks, sendTask{ThreadID: thread, Otid: j.Otid, Source: sendSource, SendType: 1,
			SyncGroup: json.Number(lsDatabase), MarkThreadRead: 1, Text: &text})
	}
	if j.ReplyTo != "" && len(tasks) > 0 {
		tasks[len(tasks)-1].ReplyMetadata = &replyMetadata{ReplySourceID: j.ReplyTo, ReplySourceType: 1}
	}
	return tasks
}
