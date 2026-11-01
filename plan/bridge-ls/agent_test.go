package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Phản hồi thật (rút gọn, đổi tên) của query danh sách Trang, bắt 01/11/2026.
const pagesFixture = `{"data":{"viewer":{"meta_business_scoping":{"personal_scopes":{"count":3,"page_info":{"end_cursor":"x","has_next_page":false},"edges":[` +
	`{"node":{"scope_description":"Trang Facebook","scope_id":"1364936880042409","scope_name":"VetGo Test","scope_type":"PAGE","is_meta_verified":false,"scope_redirect_uri":"https:\/\/business.facebook.com\/latest\/inbox\/all\/?page_id=1364936880042409&asset_id=1364936880042409","scope_profile_picture":{"uri":"https:\/\/scontent.xx.fbcdn.net\/a.png"},"scope_owner_description":null},"cursor":"a"},` +
	`{"node":{"scope_id":"17841400000000000","scope_name":"vetgo.ig","scope_type":"IG_ACCOUNT"},"cursor":"b"},` +
	`{"node":{"scope_id":"109282408574091","scope_name":"VetGo phần mềm","scope_type":"PAGE","scope_profile_picture":null},"cursor":"c"}]}}}},"extensions":{"is_final":true}}`

func TestParsePages(t *testing.T) {
	pages, err := parsePages([]byte(pagesFixture))
	if err != nil {
		t.Fatal(err)
	}
	if len(pages) != 2 || pages[0].ID != "1364936880042409" || pages[0].Name != "VetGo Test" ||
		pages[0].Picture != "https://scontent.xx.fbcdn.net/a.png" || pages[1].Name != "VetGo phần mềm" || pages[1].Picture != "" {
		t.Fatalf("pages = %+v", pages)
	}
}

func TestParsePagesErrors(t *testing.T) {
	_, err := parsePages([]byte(`{"errors":[{"message":"Variable \"$__relay_internal__pv__FooBarrelayprovider\" of required type \"Boolean!\" was not provided."}],"data":null}`))
	if err == nil || missingVarRe.FindString(err.Error()) != "__relay_internal__pv__FooBarrelayprovider" {
		t.Fatalf("err = %v", err)
	}
	if _, err := parsePages([]byte(`for (;;);{"error":1357001,"errorSummary":"Not logged in"}`)); err == nil {
		t.Fatal("want error for prefixed body")
	}
	if _, err := parsePages([]byte(`{"error":1357001,"errorSummary":"Not logged in"}`)); !errors.Is(err, errLoggedOut) {
		t.Fatalf("err = %v, want errLoggedOut", err)
	}
	if _, err := parsePages([]byte(`{"data":{"viewer":{}}}`)); err == nil || !strings.Contains(err.Error(), "personal_scopes") {
		t.Fatalf("err = %v", err)
	}
	if _, err := parsePages([]byte(`<html>`)); err == nil {
		t.Fatal("want error for HTML")
	}
}

func TestPagesVariablesExtra(t *testing.T) {
	v := pagesVariables("100", map[string]any{"__relay_internal__pv__X": true})
	if v["firstLevelScopeId"] != "100" || v["__relay_internal__pv__X"] != true || v["contextUri"] != inboxURL {
		t.Fatalf("variables = %v", v)
	}
}

type fakeRunner struct {
	as      Assignment
	jar     *CookieJar
	running atomic.Bool
	out     atomic.Bool
}

func (f *fakeRunner) Run(ctx context.Context) {
	f.running.Store(true)
	<-ctx.Done()
	f.running.Store(false)
}

func (f *fakeRunner) LoggedOut() bool { return f.out.Load() }

type fakeBridge struct {
	mu       sync.Mutex
	reports  []syncRequest
	resp     syncResponse
	changed  chan struct{}
	requests atomic.Int32
}

func (b *fakeBridge) handler(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/agent/sync" || r.Header.Get("Authorization") != "Bearer "+strings.Repeat("t", 32) {
			http.Error(w, "bad", http.StatusUnauthorized)
			return
		}
		b.requests.Add(1)
		var req syncRequest
		raw, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(raw, &req); err != nil {
			t.Errorf("bad body: %v", err)
		}
		b.mu.Lock()
		b.reports = append(b.reports, req)
		resp := b.resp
		ch := b.changed
		b.mu.Unlock()
		if req.Version == resp.Version && len(resp.Relist) == 0 {
			select {
			case <-ch:
			case <-time.After(200 * time.Millisecond):
			case <-r.Context().Done():
				return
			}
			b.mu.Lock()
			resp = b.resp
			b.mu.Unlock()
		}
		b.mu.Lock()
		b.resp.Relist = nil
		b.mu.Unlock()
		json.NewEncoder(w).Encode(resp)
	}
}

func (b *fakeBridge) set(resp syncResponse) {
	b.mu.Lock()
	b.resp = resp
	close(b.changed)
	b.changed = make(chan struct{})
	b.mu.Unlock()
}

func (b *fakeBridge) last() syncRequest {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.reports) == 0 {
		return syncRequest{}
	}
	return b.reports[len(b.reports)-1]
}

func writeLogin(t *testing.T, dir, name, cookies string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, name), 0o700); err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(map[string]any{"botName": "x", "cookies": cookies})
	if err := os.WriteFile(filepath.Join(dir, name, "config.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timeout waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestAgentEndToEnd(t *testing.T) {
	logins, data := t.TempDir(), t.TempDir()
	writeLogin(t, logins, "owner", "c_user=100; xs=secret1; fr=a")
	writeLogin(t, logins, "half", "c_user=200") // chưa có xs: bỏ qua
	bridge := &fakeBridge{changed: make(chan struct{}), resp: syncResponse{Version: 1}}
	srv := httptest.NewServer(bridge.handler(t))
	defer srv.Close()

	a := NewAgent(AgentConfig{Name: "server3", LoginsDir: logins, DataDir: data, URL: srv.URL, Token: strings.Repeat("t", 32)},
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	var lists atomic.Int32
	var listErr atomic.Value
	a.list = func(ctx context.Context, jar *CookieJar) ([]PageItem, error) {
		lists.Add(1)
		if e, _ := listErr.Load().(error); e != nil {
			return nil, e
		}
		return []PageItem{{ID: "1364936880042409", Name: "VetGo Test"}, {ID: "109282408574091", Name: "VetGo"}}, nil
	}
	var mu sync.Mutex
	runners := map[string][]*fakeRunner{}
	a.newRunner = func(as Assignment, jar *CookieJar) pageRunner {
		r := &fakeRunner{as: as, jar: jar}
		mu.Lock()
		runners[as.Account] = append(runners[as.Account], r)
		mu.Unlock()
		return r
	}
	get := func(name string) []*fakeRunner {
		mu.Lock()
		defer mu.Unlock()
		return append([]*fakeRunner(nil), runners[name]...)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { a.Run(ctx); close(done) }()

	// Phiên được chép cookie, liệt kê Trang và báo lên; cookie không lên server.
	waitFor(t, "pages reported", func() bool {
		r := bridge.last()
		return len(r.Logins) == 1 && r.Logins[0].State == "ok" && len(r.Logins[0].Pages) == 2
	})
	r := bridge.last()
	if r.Agent != "server3" || r.Logins[0].Name != "owner" || r.Logins[0].UserID != "100" || r.Logins[0].PagesAt == 0 {
		t.Fatalf("report = %+v", r)
	}
	raw, _ := json.Marshal(r)
	if strings.Contains(string(raw), "secret1") {
		t.Fatal("cookie value sent to the server")
	}
	if b, err := os.ReadFile(filepath.Join(data, "logins", "owner", "cookies.json")); err != nil || !strings.Contains(string(b), `"xs":"secret1"`) {
		t.Fatalf("imported cookies = %s, %v", b, err)
	}

	// Server giao một Trang: chạy đúng một runner với cookie của phiên.
	bridge.set(syncResponse{Version: 2, Pages: []Assignment{{Account: "vetgo-test", PageID: "1364936880042409", Login: "owner", Send: true}}})
	waitFor(t, "runner started", func() bool { rs := get("vetgo-test"); return len(rs) == 1 && rs[0].running.Load() })
	if rs := get("vetgo-test"); rs[0].jar.Get("xs") != "secret1" || !rs[0].as.Send {
		t.Fatalf("runner = %+v", rs[0].as)
	}

	// Đổi cờ gửi: dừng bản cũ rồi chạy bản mới.
	bridge.set(syncResponse{Version: 3, Pages: []Assignment{{Account: "vetgo-test", PageID: "1364936880042409", Login: "owner"}}})
	waitFor(t, "runner restarted", func() bool { rs := get("vetgo-test"); return len(rs) == 2 && rs[1].running.Load() })
	if get("vetgo-test")[0].running.Load() {
		t.Fatal("old runner still running")
	}

	// Trang báo hết phiên: phiên hiện logged_out.
	get("vetgo-test")[1].out.Store(true)
	waitFor(t, "logged_out reported", func() bool {
		r := bridge.last()
		return len(r.Logins) == 1 && r.Logins[0].State == "logged_out"
	})

	// Đăng nhập lại (cookie nguồn đổi): chép lại, liệt kê lại, Trang chạy lại với cookie mới.
	before := lists.Load()
	writeLogin(t, logins, "owner", "c_user=100; xs=secret2")
	bridge.set(syncResponse{Version: 4, Pages: []Assignment{{Account: "vetgo-test", PageID: "1364936880042409", Login: "owner"}}})
	waitFor(t, "runner on new cookies", func() bool {
		rs := get("vetgo-test")
		return len(rs) == 3 && rs[2].running.Load() && rs[2].jar.Get("xs") == "secret2"
	})
	waitFor(t, "relisted", func() bool { return lists.Load() > before })
	waitFor(t, "ok again", func() bool { r := bridge.last(); return len(r.Logins) == 1 && r.Logins[0].State == "ok" })

	// Server đòi liệt kê lại; lỗi thì báo error kèm lý do.
	listErr.Store(errors.New("danh sách Trang: HTTP 500"))
	bridge.set(syncResponse{Version: 5, Pages: []Assignment{{Account: "vetgo-test", PageID: "1364936880042409", Login: "owner"}}, Relist: []string{"owner"}})
	waitFor(t, "error reported", func() bool {
		r := bridge.last()
		return len(r.Logins) == 1 && r.Logins[0].State == "error" && strings.Contains(r.Logins[0].Error, "HTTP 500")
	})

	// Bỏ Trang: dừng. Xoá phiên: không còn báo.
	bridge.set(syncResponse{Version: 6})
	waitFor(t, "runner stopped", func() bool { return !get("vetgo-test")[2].running.Load() })
	os.RemoveAll(filepath.Join(logins, "owner"))
	waitFor(t, "login removed", func() bool { r := bridge.last(); return r.Version == 6 && len(r.Logins) == 0 })

	cancel()
	<-done
}

func TestAgentApplySkipsUnknownLoginAndBadInput(t *testing.T) {
	a := NewAgent(AgentConfig{Name: "a"}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	started := 0
	a.newRunner = func(as Assignment, jar *CookieJar) pageRunner { started++; return &fakeRunner{} }
	a.apply(context.Background(), syncResponse{Version: 1, Pages: []Assignment{
		{Account: "x", PageID: "1", Login: "missing"},
		{Account: "../bad", PageID: "1", Login: "missing"},
		{Account: "y", PageID: "12a", Login: "missing"},
	}})
	if started != 0 || len(a.running) != 0 || a.version != 1 {
		t.Fatalf("started = %d running = %d", started, len(a.running))
	}
}
