package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"go.mau.fi/mautrix-meta/pkg/messagix/dgw"
)

const testToken = "0123456789abcdef0123456789abcdef"

func testLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func testJar(t *testing.T, dir string) *CookieJar {
	t.Helper()
	path := filepath.Join(dir, "cookies.json")
	if err := os.WriteFile(path, []byte(`{"c_user":"100012345678901","xs":"secret-xs","datr":"d"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	jar, err := OpenCookieJar(path)
	if err != nil {
		t.Fatal(err)
	}
	return jar
}

func testListener(t *testing.T, ingestURL string, dryRun bool) *Listener {
	t.Helper()
	dir := t.TempDir()
	jar := testJar(t, dir)
	cfg := Config{PageID: testPageID, DataDir: dir, UserAgent: defaultUserAgent}
	l := NewListener(cfg, jar, newHTTPClient(), testLog())
	l.fwd = NewForwarder(Forwarder{URL: ingestURL, Token: testToken, Account: "vetgo", PageID: testPageID, PageName: l.PageName, DryRun: dryRun},
		newHTTPClient(), jar, cfg.UserAgent, testLog())
	l.applyPage(&PageInfo{AppID: "111222333444555", UserID: "100012345678901", Version: "9876543210123456",
		Cursor: "page-cursor", RegionHint: "PRN", Contacts: map[string]string{testPageID: "VetGo"}})
	return l
}

func TestSyncRequestShape(t *testing.T) {
	l := testListener(t, "", true)
	data, err := l.syncRequest(false)
	if err != nil {
		t.Fatal(err)
	}
	// Thứ tự khoá như trang gửi (frame ghi 31/10/2026).
	if !strings.HasPrefix(string(data), `{"app_id":"111222333444555","payload":"{\"database\":205,\"epoch_id\":`) ||
		!strings.HasSuffix(string(data), `"request_id":1,"type":2}`) {
		t.Fatalf("outer %s", data)
	}
	var outer lsRequest
	_ = json.Unmarshal(data, &outer)
	if !strings.Contains(outer.Payload, `"failure_count":null,"last_applied_cursor":"page-cursor","sync_params":null,"version":9876543210123456}`) {
		t.Fatalf("inner %s", outer.Payload)
	}
}

func TestApplyPagePrefersSavedCursor(t *testing.T) {
	dir := t.TempDir()
	if err := saveState(filepath.Join(dir, "state.json"), State{Cursor: "saved", SavedAt: time.Now().Add(-time.Hour)}); err != nil {
		t.Fatal(err)
	}
	l := NewListener(Config{PageID: testPageID, DataDir: dir}, testJar(t, dir), newHTTPClient(), testLog())
	l.applyPage(&PageInfo{Cursor: "page"})
	if l.cursor != "saved" {
		t.Fatalf("cursor %q", l.cursor)
	}
	// Đang chạy thì giữ cursor hiện tại khi tải lại trang.
	l.cursor = "live"
	l.applyPage(&PageInfo{Cursor: "page2"})
	if l.cursor != "live" {
		t.Fatalf("cursor %q", l.cursor)
	}
	l.resetCursor = true
	l.applyPage(&PageInfo{Cursor: "page3"})
	if l.cursor != "page3" {
		t.Fatalf("cursor %q", l.cursor)
	}
}

func TestApplyPageIgnoresStaleState(t *testing.T) {
	dir := t.TempDir()
	_ = saveState(filepath.Join(dir, "state.json"), State{Cursor: "old", SavedAt: time.Now().Add(-stateMaxAge - time.Minute)})
	l := NewListener(Config{PageID: testPageID, DataDir: dir}, testJar(t, dir), newHTTPClient(), testLog())
	l.applyPage(&PageInfo{Cursor: "page"})
	if l.cursor != "page" {
		t.Fatalf("cursor %q", l.cursor)
	}
}

func pushStep(cursor string, extra ...any) []any {
	step := []any{1, []any{5, "executeFirstBlockForSyncTransactionV4", []any{19, "205"}, []any{19, "0"}, "page-cursor", cursor}}
	return append(step, extra...)
}

func customerMessage(mid, text string) []any {
	ts := strconv.FormatInt(time.Now().UnixMilli(), 10)
	return []any{5, "insertMessage", text, []any{9}, []any{19, "80"}, []any{19, "100024107767037"}, []any{19, "0"},
		[]any{19, ts}, []any{19, ts}, []any{9}, mid, "otid", []any{19, "100024107767037"}}
}

func TestHandleFrame(t *testing.T) {
	l := testListener(t, "", true)
	frame := frameOf(t, pushStep("c1", customerMessage("mid.1", "xin chào")))
	for range 2 { // Facebook gửi lặp: chỉ một sự kiện
		if err := l.handleFrame(context.Background(), frame); err != nil {
			t.Fatal(err)
		}
	}
	if l.cursor != "c1" || loadState(l.statePath()).Cursor != "c1" {
		t.Fatalf("cursor %q state %q", l.cursor, loadState(l.statePath()).Cursor)
	}
	if n := len(l.fwd.queue); n != 1 {
		t.Fatalf("queued %d", n)
	}
	if e := <-l.fwd.queue; e.Mid != "mid.1" || e.Text != "xin chào" || e.SenderID != "100024107767037" {
		t.Fatalf("event %+v", e)
	}
}

func TestFullResyncRequest(t *testing.T) {
	l := testListener(t, "", true)
	l.info.SyncParams = `{"locale":"vi_VN","bloks_version":"abc"}`
	data, err := l.syncRequest(true)
	if err != nil {
		t.Fatal(err)
	}
	var outer lsRequest
	_ = json.Unmarshal(data, &outer)
	if outer.Type != 1 || !strings.Contains(outer.Payload, `"last_applied_cursor":null,"sync_params":"{\"locale\":\"vi_VN\",\"bloks_version\":\"abc\"}"`) {
		t.Fatalf("got %s", data)
	}
	if l.snapshotReq != int64(outer.RequestID) {
		t.Fatalf("snapshotReq %d", l.snapshotReq)
	}
	// Phản hồi của request type 1 là lịch sử: cập nhật cursor, không phát sự kiện.
	step := []any{1, []any{5, "executeFirstBlockForSyncTransactionV4", []any{19, "205"}, []any{19, "0"}, "c-snap", "c-snap"}, customerMessage("mid.old", "cũ")}
	payload, _ := json.Marshal(map[string]any{"step": step})
	frame, _ := json.Marshal(map[string]any{"request_id": outer.RequestID, "payload": string(payload)})
	if err := l.handleFrame(context.Background(), frame); err != nil {
		t.Fatal(err)
	}
	if len(l.fwd.queue) != 0 || l.cursor != "c-snap" {
		t.Fatalf("queued %d cursor %q", len(l.fwd.queue), l.cursor)
	}
}

func TestSendSyncParamsDoesNotSaveCursor(t *testing.T) {
	l := testListener(t, "", true)
	// executeFirstBlock: 205, epoch, cur, next, seq -1, status 3, sendSyncParams true (giống phản hồi thật 31/10).
	step := []any{5, "executeFirstBlockForSyncTransactionV4", []any{19, "205"}, []any{19, "1"}, "page-cursor", "tmp", []any{19, "-1"}, []any{19, "3"}, true}
	err := l.handleFrame(context.Background(), frameOf(t, step))
	if err != nil || l.cursor != "page-cursor" || loadState(l.statePath()).Cursor != "" {
		t.Fatalf("err %v cursor %q", err, l.cursor)
	}
}

func TestHandleFrameSyncFailure(t *testing.T) {
	l := testListener(t, "", true)
	err := l.handleFrame(context.Background(), frameOf(t, []any{5, "handleSyncFailure", []any{19, "205"}, "cursor expired"}))
	if !errors.Is(err, errSyncFailure) || !l.resetCursor || l.info != nil {
		t.Fatalf("err %v reset %v info %v", err, l.resetCursor, l.info)
	}
}

func TestBrowserQuery(t *testing.T) {
	q := url.Values{}
	for _, k := range []string{"x-dgw-deviceid", "x-dgw-tier", "x-dgw-uuid", "x-dgw-appid", "x-dgw-appversion", "x-dgw-authtype", "x-dgw-version", "x-dgw-loggingid"} {
		q.Set(k, "v")
	}
	got := browserQuery(q, testPageID, "PRN")
	want := "x-dgw-appid=v&x-dgw-appversion=v&x-dgw-authtype=v&x-dgw-version=v&x-dgw-uuid=v&x-dgw-tier=v&x-dgw-loggingid=v" +
		"&x-dgw-regionhint=PRN&x-dgw-deviceid=v&x-dgw-app-asset_ids=" + url.QueryEscape(`{"PAGE":"`+testPageID+`"}`)
	if got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
}

// fakeGateway: server DGW giả. Trả lời mở stream {"code":200}, ack dữ liệu,
// nhận request đồng bộ rồi đẩy một tin của khách.
type fakeGateway struct {
	mu       sync.Mutex
	query    string
	header   http.Header
	params   string
	syncReqs []lsRequest
	tasks    []lsRequest // request type 3 (gửi tin), mỗi cái trên một stream riêng
	pushAckd bool
	push     []byte
}

func (g *fakeGateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	g.query, g.header = r.URL.RawQuery, r.Header.Clone()
	g.mu.Unlock()
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
	if err != nil {
		return
	}
	defer conn.CloseNow()
	ctx := r.Context()
	write := func(f dgw.Frame) { _ = conn.Write(ctx, websocket.MessageBinary, f.MarshalAppend(nil)) }
	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			return
		}
		for len(data) > 0 {
			frame := dgw.CheckFrameType(data)
			if data, err = frame.Unmarshal(data); err != nil {
				return
			}
			switch f := frame.(type) {
			case *dgw.EstablishStreamFrame:
				g.mu.Lock()
				g.params = string(f.RawParameters)
				g.mu.Unlock()
				write(&dgw.EstablishStreamFrame{StreamID: f.StreamID, RawParameters: json.RawMessage(`{"code":200}`)})
			case *dgw.DataFrame:
				if f.RequiresAck {
					write(&dgw.AckFrame{StreamID: f.StreamID, AckID: f.AckID})
				}
				var req lsRequest
				_ = json.Unmarshal(f.Payload, &req)
				if req.Type == 3 {
					g.mu.Lock()
					g.tasks = append(g.tasks, req)
					g.mu.Unlock()
					write(&dgw.DataFrame{StreamID: f.StreamID, AckID: 1, RequiresAck: true, Payload: taskReply(req)})
					continue
				}
				g.mu.Lock()
				g.syncReqs = append(g.syncReqs, req)
				n := len(g.syncReqs)
				g.mu.Unlock()
				// Lần 1: chưa theo kịp (page-cursor -> c1); lần 2: theo kịp, rồi đẩy một tin.
				block := func(cur, next string) []byte {
					payload, _ := json.Marshal(map[string]any{"step": []any{5, "executeFirstBlockForSyncTransactionV4", []any{19, "205"}, []any{19, "0"}, cur, next}})
					b, _ := json.Marshal(map[string]any{"request_id": req.RequestID, "payload": string(payload)})
					return b
				}
				switch n {
				case 1:
					write(&dgw.DataFrame{StreamID: f.StreamID, AckID: 5, RequiresAck: true, Payload: block("page-cursor", "c1")})
				case 2:
					write(&dgw.DataFrame{StreamID: f.StreamID, AckID: 6, RequiresAck: true, Payload: block("c1", "c1")})
					write(&dgw.DataFrame{StreamID: f.StreamID, AckID: 7, RequiresAck: true, Payload: g.push})
				}
			case *dgw.AckFrame:
				if f.AckID == 7 {
					g.mu.Lock()
					g.pushAckd = true
					g.mu.Unlock()
				}
			case *dgw.PingFrame:
				write(&dgw.PongFrame{})
			}
		}
	}
}

func TestEndToEndFakeGateway(t *testing.T) {
	type ingestReq struct {
		path string
		body map[string]any
		auth string
	}
	got := make(chan ingestReq, 10)
	ingest := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		got <- ingestReq{r.URL.Path, body, r.Header.Get("Authorization")}
		w.WriteHeader(http.StatusOK)
	}))
	defer ingest.Close()

	gw := &fakeGateway{}
	gw.push = frameOf(t, pushStep("c-after", customerMessage("mid.e2e", "hello từ khách")))
	srv := httptest.NewServer(gw)
	defer srv.Close()

	l := testListener(t, ingest.URL, false)
	l.wsURL = "ws" + strings.TrimPrefix(srv.URL, "http")
	l.ws = &http.Client{Transport: &browserTransport{base: http.DefaultTransport, ua: defaultUserAgent, pageID: testPageID, regionHint: l.regionHint}}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { l.Run(ctx); close(done) }()
	defer func() { cancel(); <-done }()

	var msg ingestReq
	deadline := time.After(10 * time.Second)
	for msg.path != "/ingest" {
		select {
		case msg = <-got:
		case <-deadline:
			t.Fatal("no ingest")
		}
	}
	ev := msg.body["event"].(map[string]any)
	if ev["mid"] != "mid.e2e" || ev["text"] != "hello từ khách" || msg.body["account"] != "vetgo" ||
		msg.body["page_id"] != testPageID || msg.body["page_name"] != "VetGo" || msg.body["version"] != ingestVersion ||
		msg.auth != "Bearer "+testToken {
		t.Fatalf("ingest %+v", msg)
	}

	gw.mu.Lock()
	defer gw.mu.Unlock()
	q, _ := url.ParseQuery(gw.query)
	if q.Get("x-dgw-appid") != "111222333444555" || q.Get("x-dgw-uuid") != "100012345678901" || q.Get("x-dgw-authtype") != "1:0" ||
		q.Get("x-dgw-regionhint") != "PRN" || q.Get("x-dgw-app-asset_ids") != `{"PAGE":"`+testPageID+`"}` {
		t.Fatalf("query %s", gw.query)
	}
	if !strings.HasPrefix(gw.query, "x-dgw-appid=") {
		t.Fatalf("query order %s", gw.query)
	}
	h := gw.header
	if !strings.Contains(h.Get("Cookie"), "c_user=100012345678901") || h.Get("User-Agent") != defaultUserAgent.UA ||
		h.Get("Origin") != lsOrigin || h.Get("Sec-Fetch-Mode") != "" || h.Get("Accept-Language") != acceptLanguage {
		t.Fatalf("headers %v", h)
	}
	if gw.params != "{}" {
		t.Fatalf("establish params %q", gw.params)
	}
	if len(gw.syncReqs) != 2 {
		t.Fatalf("sync requests %+v", gw.syncReqs)
	}
	first, second := gw.syncReqs[0], gw.syncReqs[1]
	if first.Type != 2 || !strings.Contains(first.Payload, `"database":205`) || !strings.Contains(first.Payload, `"last_applied_cursor":"page-cursor"`) {
		t.Fatalf("first sync request %+v", first)
	}
	// Đồng bộ tiếp để theo kịp: type 2 với nextCursor của phản hồi trước.
	if second.Type != 2 || !strings.Contains(second.Payload, `"last_applied_cursor":"c1"`) || second.RequestID != first.RequestID+1 {
		t.Fatalf("catch-up request %+v", second)
	}
	if !gw.pushAckd {
		// Ack đi sau khi xử lý frame; chờ thêm một chút.
		gw.mu.Unlock()
		time.Sleep(300 * time.Millisecond)
		gw.mu.Lock()
		if !gw.pushAckd {
			t.Fatal("pushed frame not acked")
		}
	}
	if s := loadState(l.statePath()); s.Cursor != "c-after" {
		t.Fatalf("state cursor %q", s.Cursor)
	}
}

func upsertAt(mid, sender string, ts time.Time) []any {
	ms := strconv.FormatInt(ts.UnixMilli(), 10)
	return []any{5, "upsertMessage", "x", []any{9}, []any{19, "80"}, []any{19, sender}, []any{19, "0"},
		[]any{19, ms}, []any{19, ms}, []any{9}, mid, "otid", []any{19, sender}}
}

func TestGapRecoveryFromSnapshot(t *testing.T) {
	l := testListener(t, "", true)
	now := time.Now()
	l.liveUntilMs = now.Add(-5 * time.Minute).UnixMilli() // mất kết nối 5 phút trước
	l.info.SyncParams = `{"locale":"vi_VN"}`
	data, _ := l.syncRequest(true)
	var req lsRequest
	_ = json.Unmarshal(data, &req)
	step := []any{1,
		[]any{5, "executeFirstBlockForSyncTransactionV4", []any{19, "205"}, []any{19, "0"}, "s", "s"},
		upsertAt("mid.gap", "100024107767037", now.Add(-2*time.Minute)),
		upsertAt("mid.margin", "100024107767037", now.Add(-5*time.Minute-30*time.Second)),
		upsertAt("mid.old", "100024107767037", now.Add(-time.Hour)),
		upsertAt("mid.page", testPageID, now.Add(-time.Minute)),
	}
	payload, _ := json.Marshal(map[string]any{"step": step})
	frame, _ := json.Marshal(map[string]any{"request_id": req.RequestID, "payload": string(payload)})
	if err := l.handleFrame(context.Background(), frame); err != nil {
		t.Fatal(err)
	}
	var mids []string
	for len(l.fwd.queue) > 0 {
		mids = append(mids, (<-l.fwd.queue).Mid)
	}
	// Ảnh chụp liệt kê tin mới nhất trước; gửi đi theo thứ tự thời gian.
	if strings.Join(mids, ",") != "mid.margin,mid.gap" {
		t.Fatalf("recovered %v", mids)
	}
	// Frame vừa báo theo kịp: mốc còn sống được ghi lại.
	if s := loadState(l.statePath()); s.LiveUntilMs < now.UnixMilli() {
		t.Fatalf("live_until_ms %d", s.LiveUntilMs)
	}
}

// taskReply trả lời task gửi tin như server thật (bắt frame 01/11/2026):
// replaceOptimsiticMessage [otid, mid] cho từng task; task chữ "không xác nhận" thì không.
func taskReply(req lsRequest) []byte {
	var tp taskPayload
	_ = json.Unmarshal([]byte(req.Payload), &tp)
	step := []any{1, []any{5, "taskExists", []any{19, "1"}}, []any{5, "removeTask", []any{19, "1"}}}
	for _, t := range tp.Tasks {
		if t.Label == reactTaskLabel {
			step = append(step, []any{5, "replaceOptimisticReaction", []any{19, "1"}})
			continue
		}
		var st sendTask
		_ = json.Unmarshal([]byte(t.Payload), &st)
		if st.Text != nil && *st.Text == "không xác nhận" {
			continue
		}
		step = append(step, []any{5, "replaceOptimsiticMessage", st.Otid, "mid.sent-" + st.Otid})
	}
	payload, _ := json.Marshal(map[string]any{"step": step})
	b, _ := json.Marshal(map[string]any{"request_id": req.RequestID, "payload": string(payload)})
	return b
}

func TestSendEndToEndFakeGateway(t *testing.T) {
	jobs := make(chan outJob, 6)
	jobs <- outJob{ID: "j1", ThreadID: "42", Text: "chào khách", ReplyTo: "mid.q", Otid: "7522540858541379169"}
	jobs <- outJob{ID: "j2", ThreadID: "42", Text: "không xác nhận", Otid: "7522540858541379170"}
	// Ảnh và PDF kèm chữ: tải lên trước, rồi một request với ba task (tệp trước, chữ sau).
	jobs <- outJob{ID: "j3", ThreadID: "42", Text: "gửi tệp", Otid: "300", Files: []outFile{
		{Name: "anh.png", ContentType: "image/png", Data: []byte("PNG"), Otid: "301"},
		{Name: "bao-gia.pdf", ContentType: "application/pdf", Data: []byte("%PDF"), Otid: "302"},
	}}
	// Facebook từ chối tải lên: chưa gửi gì, không có task.
	jobs <- outJob{ID: "j4", ThreadID: "42", Otid: "400", Files: []outFile{{Name: "hong.exe", ContentType: "application/x-msdownload", Data: []byte("MZ"), Otid: "401"}}}
	jobs <- outJob{ID: "j5", Kind: "react", ThreadID: "42", MessageID: "mid.$khach", Emoji: "😆"}
	jobs <- outJob{ID: "j6", ThreadID: "42", Text: " 👍 ", Otid: "600"}
	type upload struct {
		query    url.Values
		field    string
		filename string
		ctype    string
		data     string
		cookie   string
	}
	uploads := make(chan upload, 4)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseMultipartForm(1 << 20)
		var u upload
		u.query, u.cookie = r.URL.Query(), r.Header.Get("Cookie")
		for field, fhs := range r.MultipartForm.File {
			f, _ := fhs[0].Open()
			b, _ := io.ReadAll(f)
			u.field, u.filename, u.ctype, u.data = field, fhs[0].Filename, fhs[0].Header.Get("Content-Type"), string(b)
		}
		uploads <- u
		switch u.filename {
		case "anh.png":
			io.WriteString(w, `for (;;);{"__ar":1,"payload":{"uploadID":null,"metadata":{"0":{"image_id":"1718704036626264","filename":"anh.png","fbid":"1718704036626264"}}}}`)
		case "bao-gia.pdf":
			io.WriteString(w, `for (;;);{"__ar":1,"payload":{"uploadID":null,"metadata":{"0":{"file_id":1109895998427739,"filename":"bao-gia.pdf"}}}}`)
		default:
			io.WriteString(w, `for (;;);{"__ar":1,"error":1545012,"errorSummary":"Loại tệp không được hỗ trợ"}`)
		}
	}))
	defer up.Close()
	results := make(chan outResult, 6)
	ingest := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/outbox":
			select {
			case j := <-jobs:
				_ = json.NewEncoder(w).Encode(j)
			case <-time.After(50 * time.Millisecond):
				w.WriteHeader(http.StatusNoContent)
			}
		case "/outbox/result":
			var body struct{ Result outResult }
			_ = json.NewDecoder(r.Body).Decode(&body)
			results <- body.Result
		}
	}))
	defer ingest.Close()
	gw := &fakeGateway{push: frameOf(t, pushStep("c-after"))}
	srv := httptest.NewServer(gw)
	defer srv.Close()

	l := testListener(t, ingest.URL, false)
	l.cfg.Send = true
	l.uploadURL = up.URL
	l.info.DTSG, l.info.LSD = "dtsg-token", "lsd-token"
	l.wsURL = "ws" + strings.TrimPrefix(srv.URL, "http")
	l.ws = &http.Client{Transport: &browserTransport{base: http.DefaultTransport, ua: defaultUserAgent, pageID: testPageID, regionHint: l.regionHint}}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { l.Run(ctx); close(done) }()
	defer func() { cancel(); <-done }()

	got := map[string]outResult{}
	for len(got) < 6 {
		select {
		case r := <-results:
			got[r.ID] = r
		case <-time.After(10 * time.Second):
			t.Fatalf("results %+v", got)
		}
	}
	if r := got["j1"]; !r.OK || r.Mid != "mid.sent-7522540858541379169" || r.Unknown || r.Retry {
		t.Fatalf("j1 %+v", r)
	}
	// Không có replaceOptimsiticMessage: có thể đã gửi, không được gửi lại.
	if r := got["j2"]; r.OK || !r.Unknown || r.Retry || !strings.Contains(r.Error, "removeTask") {
		t.Fatalf("j2 %+v", r)
	}

	if r := got["j3"]; !r.OK || strings.Join(r.Mids, ",") != "mid.sent-301,mid.sent-302,mid.sent-300" || r.Mid != "mid.sent-301" {
		t.Fatalf("j3 %+v", r)
	}
	if r := got["j4"]; r.OK || r.Unknown || r.Retry || !strings.Contains(r.Error, "Loại tệp không được hỗ trợ") || !strings.Contains(r.Error, "Chưa gửi gì") {
		t.Fatalf("j4 %+v", r)
	}
	close(uploads)
	var ups []upload
	for u := range uploads {
		ups = append(ups, u)
	}
	if len(ups) != 3 || ups[0].filename != "anh.png" || ups[0].ctype != "image/png" || ups[0].data != "PNG" || ups[0].field != "upload_1024" ||
		ups[1].filename != "bao-gia.pdf" || ups[1].field != "upload_1025" {
		t.Fatalf("uploads %+v", ups)
	}
	q := ups[0].query
	if q.Get("request_user_id") != testPageID || q.Get("__user") != "100012345678901" || q.Get("__a") != "1" ||
		q.Get("fb_dtsg") != "dtsg-token" || q.Get("lsd") != "lsd-token" || q.Get("jazoest") != jazoest("dtsg-token") ||
		!strings.Contains(ups[0].cookie, "xs=secret-xs") {
		t.Fatalf("upload query %v cookie set %v", q, ups[0].cookie != "")
	}

	gw.mu.Lock()
	defer gw.mu.Unlock()
	if len(gw.tasks) != 5 {
		t.Fatalf("tasks %+v", gw.tasks)
	}
	if r := got["j5"]; !r.OK || r.Unknown {
		t.Fatalf("j5 %+v", r)
	}
	if r := got["j6"]; !r.OK || r.Mid != "mid.sent-600" {
		t.Fatalf("j6 %+v", r)
	}
	var react, like taskPayload
	_ = json.Unmarshal([]byte(gw.tasks[3].Payload), &react)
	_ = json.Unmarshal([]byte(gw.tasks[4].Payload), &like)
	if len(react.Tasks) != 1 || react.Tasks[0].Label != "29" || react.Tasks[0].QueueName != "reaction:mid.$khach" ||
		!strings.HasPrefix(react.Tasks[0].Payload, `{"thread_key":42,"timestamp_ms":`) ||
		!strings.HasSuffix(react.Tasks[0].Payload, `"message_id":"mid.$khach","actor_id":`+testPageID+`,"reaction":"😆","reaction_style":null,"sync_group":205,"send_attribution":4456448,"dataclass_params":null,"attachment_fbid":null,"offline_threading_id":null}`) {
		t.Fatalf("react task %+v", react.Tasks)
	}
	// Chỉ "👍": nút Like (sticker), payload như trang gửi, không có khoá text.
	if want := `{"thread_id":42,"otid":"600","source":4456448,"send_type":2,"sync_group":205,"mark_thread_read":1,"sticker_id":369239263222822}`; len(like.Tasks) != 1 || like.Tasks[0].Payload != want {
		t.Fatalf("like task %+v", like.Tasks)
	}
	var fileReq taskPayload
	_ = json.Unmarshal([]byte(gw.tasks[2].Payload), &fileReq)
	if len(fileReq.Tasks) != 3 {
		t.Fatalf("file request %+v", fileReq)
	}
	wantFile := `{"thread_id":42,"otid":"301","source":0,"send_type":3,"sync_group":205,"mark_thread_read":0,"text":null,"attachment_fbids":[1718704036626264]}`
	if fileReq.Tasks[0].Payload != wantFile || !strings.Contains(fileReq.Tasks[1].Payload, `"attachment_fbids":[1109895998427739]`) ||
		!strings.Contains(fileReq.Tasks[2].Payload, `"send_type":1`) || !strings.Contains(fileReq.Tasks[2].Payload, `"text":"gửi tệp"`) {
		t.Fatalf("file tasks %+v", fileReq.Tasks)
	}
	req := gw.tasks[0]
	var tp taskPayload
	if err := json.Unmarshal([]byte(req.Payload), &tp); err != nil {
		t.Fatal(err)
	}
	if req.Type != 3 || req.AppID != "111222333444555" || tp.VersionID != "9876543210123456" || len(tp.Tasks) != 1 ||
		tp.Tasks[0].Label != "46" || tp.Tasks[0].QueueName != "42" || tp.Tasks[0].FailureCount != nil {
		t.Fatalf("task request %+v %+v", req, tp)
	}
	// Payload của task đúng thứ tự khoá và giá trị như Business Suite gửi.
	want := `{"thread_id":42,"otid":"7522540858541379169","source":4456448,"send_type":1,"sync_group":205,"mark_thread_read":1,"text":"chào khách",` +
		`"reply_metadata":{"reply_source_id":"mid.q","reply_source_type":1,"reply_type":0,"reply_source_attachment_id":null}}`
	if tp.Tasks[0].Payload != want {
		t.Fatalf("task payload\n got %s\nwant %s", tp.Tasks[0].Payload, want)
	}
	if strings.Contains(gw.tasks[1].Payload, "reply_metadata") {
		t.Fatalf("no reply expected: %s", gw.tasks[1].Payload)
	}
}

func TestSendJobRefusesWhenNotConnected(t *testing.T) {
	l := testListener(t, "", false)
	r := l.sendJob(context.Background(), outJob{ID: "j", ThreadID: "42", Text: "x", Otid: "1"})
	if r.OK || !r.Retry || r.Unknown {
		t.Fatalf("got %+v", r)
	}
	if r := l.sendJob(context.Background(), outJob{ID: "j", ThreadID: "abc", Text: "x", Otid: "1"}); r.Retry || r.OK || r.Error == "" {
		t.Fatalf("invalid thread %+v", r)
	}
}
