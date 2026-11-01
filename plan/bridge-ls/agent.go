package main

// Chế độ agent (omnichannel D18): một tiến trình giữ nhiều phiên Facebook, mỗi
// Trang một Listener, cấu hình lấy từ fb-bridge-web thay cho env.
//
//   - Phiên: FBLS_LOGINS/<tên>/config.json do fb-login-ui ghi (trường "cookies"
//     dạng "a=b; c=d"). Agent chép sang FBLS_DATA/logins/<tên>/cookies.json và
//     dùng một CookieJar cho mọi Trang của phiên đó; đăng nhập lại (cookie nguồn
//     đổi) thì chép lại và khởi động lại các Trang của phiên.
//   - Đồng bộ: POST /agent/sync {agent, version, logins[]} (long-poll). Phản hồi
//     {version, pages[], relist[]}: Trang nào chạy bằng phiên nào, gửi tin hay
//     không, và phiên nào cần liệt kê lại Trang.
//   - Cookie không rời máy agent: server chỉ nhận tên phiên, id người dùng, trạng
//     thái và danh sách Trang.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/imroc/req/v3"
)

const (
	pagesRefresh  = 6 * time.Hour    // liệt kê lại Trang định kỳ (tên, ảnh đổi)
	pagesRetry    = 10 * time.Minute // liệt kê lỗi thì thử lại sau chừng này
	listTimeout   = time.Minute
	syncWaitLimit = 40 * time.Second // server giữ long-poll tối đa 25 giây
)

var loginNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$`)

type AgentConfig struct {
	Name      string // FBLS_AGENT
	LoginsDir string // FBLS_LOGINS: thư mục fb-login-ui ghi cookie
	DataDir   string // FBLS_DATA
	URL       string // fb-bridge-web, cổng ingest
	Token     string
	UserAgent UserAgent
}

// Assignment: một Trang agent phải nghe (do admin chọn trên trang Tài khoản kênh).
type Assignment struct {
	Account  string `json:"account"`
	PageID   string `json:"page_id"`
	PageName string `json:"page_name,omitempty"`
	Login    string `json:"login"`
	Send     bool   `json:"send"`
}

type loginReport struct {
	Name    string     `json:"name"`
	UserID  string     `json:"user_id,omitempty"`
	State   string     `json:"state"` // listing | ok | logged_out | error
	Error   string     `json:"error,omitempty"`
	Pages   []PageItem `json:"pages"`
	PagesAt int64      `json:"pages_at,omitempty"` // Unix ms
}

type syncRequest struct {
	Agent   string        `json:"agent"`
	Version int64         `json:"version"`
	Logins  []loginReport `json:"logins"`
}

type syncResponse struct {
	Version int64        `json:"version"`
	Pages   []Assignment `json:"pages"`
	Relist  []string     `json:"relist"`
}

type agentLogin struct {
	name    string
	hash    string // sha256 chuỗi cookie nguồn: đổi là đã đăng nhập lại
	jar     *CookieJar
	userID  string
	pages   []PageItem
	pagesAt time.Time
	tried   time.Time // lần liệt kê gần nhất
	listing bool
	err     error
}

// pageRunner chạy một Trang tới khi ctx bị huỷ; loggedOut cho biết trang báo hết phiên.
type pageRunner interface {
	Run(ctx context.Context)
	LoggedOut() bool
}

type runningPage struct {
	as     Assignment
	hash   string // phiên lúc khởi động
	r      pageRunner
	cancel context.CancelFunc
	done   chan struct{}
}

type Agent struct {
	cfg      AgentConfig
	log      *slog.Logger
	http     *req.Client // Facebook
	download *req.Client
	sync     *http.Client

	// Thay được trong test.
	list      func(ctx context.Context, jar *CookieJar) ([]PageItem, error)
	newRunner func(as Assignment, jar *CookieJar) pageRunner

	mu      sync.Mutex
	logins  map[string]*agentLogin
	running map[string]*runningPage // theo tên tài khoản fb-bridge-web
	version int64
	kick    context.CancelFunc // cắt long-poll đang chờ khi agent có tin mới
}

func NewAgent(cfg AgentConfig, log *slog.Logger) *Agent {
	a := &Agent{
		cfg: cfg, log: log, http: newHTTPClient(), download: newDownloadClient(),
		sync:   &http.Client{Timeout: syncWaitLimit},
		logins: map[string]*agentLogin{}, running: map[string]*runningPage{},
	}
	cache := &queryCache{path: filepath.Join(cfg.DataDir, "pages-query.json")}
	a.list = func(ctx context.Context, jar *CookieJar) ([]PageItem, error) {
		return listPages(ctx, a.http, a.download, jar, a.cfg.UserAgent, cache)
	}
	a.newRunner = a.listenerFor
	return a
}

func (a *Agent) listenerFor(as Assignment, jar *CookieJar) pageRunner {
	log := a.log.With("account", as.Account)
	l := NewListener(Config{
		PageID: as.PageID, PageName: as.PageName, DataDir: a.cfg.DataDir,
		StateFile: "state-" + as.PageID + ".json", Send: as.Send, UserAgent: a.cfg.UserAgent,
	}, jar, a.http, log)
	l.fwd = NewForwarder(Forwarder{URL: a.cfg.URL, Token: a.cfg.Token, Account: as.Account, PageID: as.PageID, PageName: l.PageName},
		a.download, jar, a.cfg.UserAgent, log)
	return l
}

func (l *Listener) LoggedOut() bool { return l.loggedOut.Load() }

func (a *Agent) Run(ctx context.Context) {
	defer a.stopAll()
	step := 0
	for ctx.Err() == nil {
		a.scan(ctx)
		req := a.report()
		sctx, cancel := context.WithCancel(ctx)
		a.mu.Lock()
		a.kick = cancel
		a.mu.Unlock()
		resp, err := a.syncOnce(sctx, req)
		kicked := sctx.Err() != nil && ctx.Err() == nil
		cancel()
		switch {
		case ctx.Err() != nil:
			return
		case kicked:
			continue
		case err != nil:
			a.log.Warn("agent sync failed", "error", err)
			sleepCtx(ctx, backoffSteps[min(step, len(backoffSteps)-1)])
			step++
			continue
		}
		step = 0
		a.apply(ctx, resp)
	}
}

// wake cắt long-poll để báo ngay trạng thái mới (liệt kê xong, hết phiên).
func (a *Agent) wake() {
	a.mu.Lock()
	k := a.kick
	a.mu.Unlock()
	if k != nil {
		k()
	}
}

func (a *Agent) syncOnce(ctx context.Context, body syncRequest) (syncResponse, error) {
	var out syncResponse
	data, err := json.Marshal(body)
	if err != nil {
		return out, err
	}
	r, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(a.cfg.URL, "/")+"/agent/sync", bytes.NewReader(data))
	if err != nil {
		return out, err
	}
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer "+a.cfg.Token)
	resp, err := a.sync.Do(r)
	if err != nil {
		return out, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return out, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return out, errors.New("invalid sync response")
	}
	return out, nil
}

// scan đọc các phiên fb-login-ui đã ghi; phiên mới hoặc vừa đăng nhập lại thì
// chép cookie và liệt kê Trang.
func (a *Agent) scan(ctx context.Context) {
	entries, err := os.ReadDir(a.cfg.LoginsDir)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		a.log.Warn("read logins dir failed", "error", err)
		return
	}
	found := map[string]bool{}
	for _, e := range entries {
		name := e.Name()
		if !e.IsDir() || !loginNamePattern.MatchString(name) {
			continue
		}
		cookies, err := readLoginCookies(filepath.Join(a.cfg.LoginsDir, name, "config.json"))
		if err != nil {
			continue // chưa đăng nhập xong hoặc thư mục của việc khác
		}
		found[name] = true
		sum := sha256.Sum256([]byte(cookies))
		hash := hex.EncodeToString(sum[:])
		a.mu.Lock()
		cur := a.logins[name]
		a.mu.Unlock()
		if cur != nil && cur.hash == hash {
			a.mu.Lock()
			every := pagesRefresh
			if cur.err != nil {
				every = pagesRetry
			}
			due := !cur.listing && cur.jar != nil && time.Since(cur.tried) > every
			a.mu.Unlock()
			if due {
				a.relist(ctx, name)
			}
			continue
		}
		jar, err := importCookies(filepath.Join(a.cfg.DataDir, "logins", name), cookies)
		a.mu.Lock()
		l := &agentLogin{name: name, hash: hash, jar: jar, err: err}
		if jar != nil {
			l.userID = jar.Get("c_user")
		}
		a.logins[name] = l
		a.mu.Unlock()
		if err != nil {
			a.log.Warn("login cookies unusable", "login", name, "error", err)
			continue
		}
		a.log.Info("login loaded", "login", name, "reloaded", cur != nil)
		a.relist(ctx, name)
	}
	a.mu.Lock()
	for name := range a.logins {
		if !found[name] {
			delete(a.logins, name)
			a.log.Info("login removed", "login", name)
		}
	}
	a.mu.Unlock()
}

// readLoginCookies trả chuỗi cookie của config.json; không có c_user, xs là lỗi.
func readLoginCookies(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	var c struct {
		Cookies string `json:"cookies"`
	}
	if err := json.Unmarshal(data, &c); err != nil {
		return "", err
	}
	m := parseCookieString(c.Cookies)
	if m["c_user"] == "" || m["xs"] == "" {
		return "", errors.New("missing c_user or xs")
	}
	return c.Cookies, nil
}

func parseCookieString(s string) map[string]string {
	m := map[string]string{}
	for _, part := range strings.Split(s, ";") {
		name, value, ok := strings.Cut(strings.TrimSpace(part), "=")
		if ok && name != "" && value != "" {
			m[name] = value
		}
	}
	return m
}

func importCookies(dir string, cookies string) (*CookieJar, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	data, err := json.Marshal(parseCookieString(cookies))
	if err != nil {
		return nil, err
	}
	path := filepath.Join(dir, "cookies.json")
	if err := writeFileAtomic(path, data); err != nil {
		return nil, err
	}
	return OpenCookieJar(path)
}

func (a *Agent) relist(ctx context.Context, name string) {
	a.mu.Lock()
	l := a.logins[name]
	if l == nil || l.jar == nil || l.listing {
		a.mu.Unlock()
		return
	}
	l.listing, l.tried = true, time.Now()
	jar := l.jar
	a.mu.Unlock()
	go func() {
		lctx, cancel := context.WithTimeout(ctx, listTimeout)
		pages, err := a.list(lctx, jar)
		cancel()
		a.mu.Lock()
		if cur := a.logins[name]; cur == l {
			l.listing = false
			l.err = err
			if err == nil {
				l.pages, l.pagesAt = pages, time.Now()
			}
		}
		a.mu.Unlock()
		if err != nil {
			a.log.Warn("list pages failed", "login", name, "error", err)
		} else {
			a.log.Info("pages listed", "login", name, "pages", len(pages))
		}
		a.wake()
	}()
	a.wake()
}

func (a *Agent) report() syncRequest {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := syncRequest{Agent: a.cfg.Name, Version: a.version, Logins: []loginReport{}}
	names := make([]string, 0, len(a.logins))
	for n := range a.logins {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		l := a.logins[n]
		r := loginReport{Name: n, UserID: l.userID, State: "ok", Pages: l.pages}
		if r.Pages == nil {
			r.Pages = []PageItem{}
		}
		if !l.pagesAt.IsZero() {
			r.PagesAt = l.pagesAt.UnixMilli()
		}
		switch {
		case l.listing:
			r.State = "listing"
		case errors.Is(l.err, errLoggedOut):
			r.State, r.Error = "logged_out", "Phiên Facebook đã hết, cần đăng nhập lại."
		case l.err != nil:
			r.State, r.Error = "error", l.err.Error()
		}
		for _, p := range a.running {
			if p.as.Login == n && p.hash == l.hash && p.r.LoggedOut() {
				r.State, r.Error = "logged_out", "Phiên Facebook đã hết, cần đăng nhập lại."
			}
		}
		out.Logins = append(out.Logins, r)
	}
	return out
}

// apply chạy đúng các Trang server giao: Trang mới thì khởi động, đổi cấu hình
// hoặc phiên đăng nhập lại thì khởi động lại, Trang bị bỏ thì dừng.
func (a *Agent) apply(ctx context.Context, resp syncResponse) {
	want := map[string]Assignment{}
	for _, as := range resp.Pages {
		if accountPattern(as.Account) && as.PageID != "" && strings.Trim(as.PageID, "0123456789") == "" {
			want[as.Account] = as
		}
	}
	var stop []*runningPage
	a.mu.Lock()
	a.version = resp.Version
	for name, p := range a.running {
		as, ok := want[name]
		login := a.logins[p.as.Login]
		if !ok || as != p.as || login == nil || login.hash != p.hash {
			stop = append(stop, p)
			delete(a.running, name)
		}
	}
	a.mu.Unlock()
	// Dừng hẳn bản cũ trước khi chạy bản mới: hai socket cùng một Trang sẽ ghi
	// chung file cursor và gửi trùng sự kiện.
	for _, p := range stop {
		p.cancel()
		<-p.done
		a.log.Info("page stopped", "account", p.as.Account)
	}
	a.mu.Lock()
	for name, as := range want {
		if a.running[name] != nil {
			continue
		}
		login := a.logins[as.Login]
		if login == nil || login.jar == nil {
			continue // phiên chưa có trên agent này: Trang hiện "chưa kết nối"
		}
		pctx, cancel := context.WithCancel(ctx)
		p := &runningPage{as: as, hash: login.hash, r: a.newRunner(as, login.jar), cancel: cancel, done: make(chan struct{})}
		a.running[name] = p
		go func() {
			defer close(p.done)
			p.r.Run(pctx)
		}()
		a.log.Info("page started", "account", name, "page", as.PageID, "login", as.Login, "send", as.Send)
	}
	a.mu.Unlock()
	for _, name := range resp.Relist {
		a.relist(ctx, name)
	}
}

func (a *Agent) stopAll() {
	a.mu.Lock()
	running := a.running
	a.running = map[string]*runningPage{}
	a.mu.Unlock()
	for _, p := range running {
		p.cancel()
		<-p.done
	}
}

func accountPattern(name string) bool { return loginNamePattern.MatchString(name) }
