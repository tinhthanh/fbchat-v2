package main

// Chuyển sự kiện sang fb-bridge-web của omnichannel, cùng hợp đồng với extension
// của fbweb-browser (sw.js): POST /ingest {account, page_id, page_name, version,
// event} với file đã tải sẵn (base64), POST /heartbeat 30 giây một lần khi socket
// còn nhận. Hàng đợi tuần tự giữ thứ tự tin; 502 hoặc lỗi mạng thì gửi lại
// (fb-bridge-web và gateway chống trùng theo id), 4xx thì bỏ.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/imroc/req/v3"
)

const (
	ingestVersion = "ls-1"
	maxFileBytes  = 20 << 20
	likeSticker   = "369239263222822"
)

var retryDelays = []time.Duration{2 * time.Second, 5 * time.Second, 15 * time.Second, 30 * time.Second, 60 * time.Second}

type Forwarder struct {
	URL, Token, Account, PageID string
	PageName                    func() string
	DryRun                      bool

	ingest   *http.Client // fb-bridge-web qua SSH tunnel
	download *req.Client  // CDN của Facebook, TLS giả Chrome
	jar      *CookieJar
	ua       UserAgent
	queue    chan Event
	log      *slog.Logger
}

func NewForwarder(f Forwarder, download *req.Client, jar *CookieJar, ua UserAgent, log *slog.Logger) *Forwarder {
	f.ingest = &http.Client{Timeout: 2 * time.Minute}
	f.download, f.jar, f.ua, f.log = download, jar, ua, log
	f.queue = make(chan Event, 1000)
	return &f
}

func (f *Forwarder) Push(e Event) {
	select {
	case f.queue <- e:
	default:
		f.log.Warn("forward queue full, event dropped", "kind", e.Kind, "mid", e.Mid)
	}
}

func (f *Forwarder) Run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case e := <-f.queue:
			f.forward(ctx, e)
		}
	}
}

func (f *Forwarder) forward(ctx context.Context, e Event) {
	if f.DryRun {
		f.log.Info("event (dry run)", "kind", e.Kind, "mid", e.Mid, "files", len(e.Attachments))
		return
	}
	e = f.withFiles(ctx, e)
	status := 0
	for i := 0; ; i++ {
		status = f.post(ctx, "/ingest", map[string]any{"event": e})
		if (status >= 200 && status < 500) || i >= len(retryDelays) {
			break
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(retryDelays[i]):
		}
	}
	// Chỉ loại, id và số file: không ghi nội dung tin hay tên khách vào log.
	fileErrors := 0
	for _, a := range e.Attachments {
		if a.Error != "" {
			fileErrors++
		}
	}
	if status == http.StatusOK {
		f.log.Info("ingest forwarded", "kind", e.Kind, "mid", e.Mid, "files", len(e.Attachments), "file_errors", fileErrors)
	} else {
		f.log.Warn("ingest rejected", "status", status, "kind", e.Kind, "mid", e.Mid)
	}
}

func (f *Forwarder) Heartbeat(ctx context.Context) {
	if f.DryRun {
		return
	}
	if status := f.post(ctx, "/heartbeat", nil); status != http.StatusOK {
		f.log.Warn("heartbeat failed", "status", status)
	}
}

// post trả mã HTTP, 0 nếu lỗi mạng.
func (f *Forwarder) post(ctx context.Context, path string, extra map[string]any) int {
	status, _ := f.postRead(ctx, path, extra, 64<<10)
	return status
}

// postRead như post nhưng trả cả thân (tối đa limit byte), cho /outbox.
func (f *Forwarder) postRead(ctx context.Context, path string, extra map[string]any, limit int64) (int, []byte) {
	body := map[string]any{"account": f.Account, "page_id": f.PageID, "page_name": f.PageName(), "version": ingestVersion}
	for k, v := range extra {
		body[k] = v
	}
	data, err := json.Marshal(body)
	if err != nil {
		return 0, nil
	}
	r, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(f.URL, "/")+path, bytes.NewReader(data))
	if err != nil {
		return 0, nil
	}
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer "+f.Token)
	resp, err := f.ingest.Do(r)
	if err != nil {
		return 0, nil
	}
	out, _ := io.ReadAll(io.LimitReader(resp.Body, limit))
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	return resp.StatusCode, out
}

// withFiles tải file ngay: URL CDN có chữ ký và hết hạn, server không có cookie.
func (f *Forwarder) withFiles(ctx context.Context, e Event) Event {
	if (e.Kind != "message" && e.Kind != "page_message") || len(e.Attachments) == 0 {
		return e
	}
	out := make([]Attachment, 0, len(e.Attachments))
	for _, a := range e.Attachments {
		if a.Kind == "link" || (a.Kind == "sticker" && a.StickerID == likeSticker) || a.URL == "" {
			out = append(out, a)
			continue
		}
		data, err := f.fetchFile(ctx, a.URL)
		if err != nil {
			a.Error = err.Error()
		} else {
			a.Data = data
		}
		out = append(out, a)
	}
	e.Attachments = out
	return e
}

func (f *Forwarder) fetchFile(ctx context.Context, rawURL string) ([]byte, error) {
	u, err := url.Parse(rawURL)
	if err != nil || u.Scheme != "https" {
		return nil, fmt.Errorf("URL không hợp lệ")
	}
	r := f.download.R().SetContext(ctx).DisableAutoReadResponse().
		SetHeader("user-agent", f.ua.UA).
		SetHeader("accept-language", acceptLanguage)
	// Cookie chỉ gửi cho facebook.com, như trình duyệt; CDN fbcdn.net không cần.
	if host := u.Hostname(); host == "facebook.com" || strings.HasSuffix(host, ".facebook.com") {
		r.SetHeader("cookie", f.jar.Header())
	}
	resp, err := r.Get(rawURL)
	if err != nil {
		return nil, fmt.Errorf("lỗi mạng")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxFileBytes+1))
	if err != nil {
		return nil, fmt.Errorf("lỗi mạng")
	}
	if len(data) > maxFileBytes {
		return nil, fmt.Errorf("file quá 20 MiB")
	}
	return data, nil
}
