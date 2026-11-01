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
