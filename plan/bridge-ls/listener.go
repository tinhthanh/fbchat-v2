package main

// Nghe hộp thư Trang thẳng trên wss://gateway.facebook.com/ws/lightspeed, không
// cần trình duyệt. Giao thức DGW (khung frame, stream, ack, ping) dùng lại
// messagix/dgw của mautrix-meta; phần khác so với Messenger cá nhân:
//   - URL thêm x-dgw-regionhint và x-dgw-app-asset_ids={"PAGE":"<id>"} (transport.go)
//   - chỉ một stream: sync db 205, type 2 với cursor lấy từ trang (page.go) hoặc
//     cursor đã lưu; server đẩy tin mới trên chính stream đó.
// Frame mở đầu của trình duyệt ghi lại 31/10/2026 (README.md).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
	"github.com/imroc/req/v3"
	"github.com/rs/zerolog"

	"go.mau.fi/mautrix-meta/pkg/messagix/dgw"
)

const (
	lsURL        = "wss://gateway.facebook.com/ws/lightspeed"
	lsOrigin     = "https://business.facebook.com"
	pageRefresh  = 6 * time.Hour  // tải lại trang (LSVersion, appId) khi nối lại sau chừng này
	stateMaxAge  = 12 * time.Hour // cursor đã lưu cũ hơn thì lấy cursor mới của trang
	gapMargin    = time.Minute    // lấy lại tin từ (mốc còn sống - gapMargin)
	gapMaxAge    = 24 * time.Hour // không lấy lại tin cũ hơn chừng này
	heartbeatGap = 30 * time.Second
)

var backoffSteps = []time.Duration{2 * time.Second, 5 * time.Second, 15 * time.Second, 30 * time.Second, time.Minute, 2 * time.Minute, 5 * time.Minute}

var (
	errStreamClosed = errors.New("lightspeed stream closed by server")
	errSyncFailure  = errors.New("lightspeed sync failure")
)

type Config struct {
	PageID    string
	PageName  string // dự phòng khi trang chưa cho biết tên Trang
	DataDir   string
	StateFile string // mặc định state.json; mỗi Trang một file khi chạy nhiều bản chung cookie
	Send      bool   // FBLS_SEND=1: nhận tin cần gửi từ fb-bridge-web (send.go)
	UserAgent UserAgent
}

type Listener struct {
	cfg  Config
	log  *slog.Logger
	jar  *CookieJar
	http *req.Client
	ws   *http.Client
	fwd  *Forwarder

	wsURL string // lsURL; test đổi sang server giả

	mu          sync.Mutex
	info        *PageInfo
	infoAt      time.Time
	cursor      string
	resetCursor bool
	contacts    map[string]string
	seen        *dedupe
	requestID   int // chung cho request đồng bộ và task gửi tin, như trang
	syncReq     int // request đồng bộ gần nhất: chỉ phản hồi của nó quyết định đã theo kịp chưa
	taskID      int
	uploadSeq   int
	uploadURL   string // uploadURL; test đổi sang server giả
	sock        *dgw.Socket
	stream      *dgw.PersistentStream
	streamReady chan struct{} // đóng khi stream của kết nối hiện tại đã có (onConnect)
	resyncs     int           // số lần đồng bộ lại (type 1) trong kết nối này
	catchups    int           // số lần đồng bộ tiếp (type 2) để theo kịp trong kết nối này
	snapshotReq int64         // request type 1 gần nhất: phản hồi của nó là ảnh chụp lịch sử
	liveUntilMs int64         // mốc cuối cùng còn theo kịp server (state.json)

	caughtUp atomic.Bool

	connected atomic.Bool
	loggedOut atomic.Bool // trang Hộp thư báo hết phiên; agent báo phiên cần đăng nhập lại
}

func NewListener(cfg Config, jar *CookieJar, httpClient *req.Client, log *slog.Logger) *Listener {
	l := &Listener{cfg: cfg, log: log, jar: jar, http: httpClient, wsURL: lsURL, uploadURL: uploadURL, contacts: map[string]string{}, seen: newDedupe(2000)}
	l.liveUntilMs = loadState(l.statePath()).LiveUntilMs
	l.ws = newWebsocketClient(cfg.UserAgent, cfg.PageID, l.regionHint)
	return l
}

func (l *Listener) statePath() string {
	name := l.cfg.StateFile
	if name == "" {
		name = "state.json"
	}
	return filepath.Join(l.cfg.DataDir, name)
}

func (l *Listener) regionHint() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.info != nil {
		return l.info.RegionHint
	}
	return ""
}

func (l *Listener) PageName() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	if name := l.contacts[l.cfg.PageID]; name != "" {
		return name
	}
	return l.cfg.PageName
}

func (l *Listener) Run(ctx context.Context) {
	go l.fwd.Run(ctx)
	go l.heartbeatLoop(ctx)
	if l.cfg.Send && !l.fwd.DryRun {
		go l.sendLoop(ctx)
	}
	step := 0
	wait := func() {
		d := backoffSteps[min(step, len(backoffSteps)-1)]
		step++
		select {
		case <-ctx.Done():
		case <-time.After(d):
		}
	}
	for ctx.Err() == nil {
		l.mu.Lock()
		needPage := l.info == nil || time.Since(l.infoAt) > pageRefresh
		l.mu.Unlock()
		if needPage {
			if err := l.loadPage(ctx); err != nil {
				if errors.Is(err, errLoggedOut) {
					// Không thử dồn dập: đăng nhập lại là việc của người dùng.
					l.loggedOut.Store(true)
					l.log.Error("facebook session is logged out; log in again")
					select {
					case <-ctx.Done():
					case <-time.After(30 * time.Minute):
					}
					continue
				}
				l.log.Warn("load inbox page failed", "error", err)
				wait()
				continue
			}
		}
		start := time.Now()
		err := l.connect(ctx)
		if ctx.Err() != nil {
			return
		}
		if time.Since(start) > time.Minute {
			step = 0
		} else {
			// Hỏng ngay khi nối: có thể LSVersion hay phiên đã đổi, lần sau tải lại trang.
			l.mu.Lock()
			l.info = nil
			l.mu.Unlock()
		}
		l.log.Warn("socket closed", "error", err, "unauthorized", dgw.IsUnauthorized(err))
		wait()
	}
}

func (l *Listener) loadPage(ctx context.Context) error {
	info, err := fetchPage(ctx, l.http, l.jar, l.cfg.UserAgent, l.cfg.PageID)
	if err != nil {
		return err
	}
	if info.UserID != l.jar.Get("c_user") {
		return errors.New("inbox page USER_ID does not match the c_user cookie")
	}
	l.loggedOut.Store(false)
	l.applyPage(info)
	return nil
}

// applyPage nhận thông tin trang và chọn cursor: cursor đang chạy, rồi cursor đã
// lưu (còn mới), cuối cùng mới tới cursor của trang (bỏ qua tin trong lúc dừng).
func (l *Listener) applyPage(info *PageInfo) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.info, l.infoAt = info, time.Now()
	for id, name := range info.Contacts {
		l.contacts[id] = name
	}
	switch {
	case l.resetCursor:
		l.cursor, l.resetCursor = info.Cursor, false
		l.log.Info("cursor reset to the inbox page cursor")
	case l.cursor == "":
		st := loadState(l.statePath())
		if st.Cursor != "" && time.Since(st.SavedAt) < stateMaxAge {
			l.cursor = st.Cursor
			l.log.Info("resuming from saved cursor", "age", time.Since(st.SavedAt).Round(time.Second))
		} else {
			l.cursor = info.Cursor
		}
	}
	l.log.Info("inbox page loaded", "contacts", len(info.Contacts), "region", info.RegionHint)
}

func (l *Listener) connect(ctx context.Context) error {
	l.mu.Lock()
	info := *l.info
	l.mu.Unlock()
	var sock *dgw.Socket
	sock = dgw.NewSocket(dgw.SocketOptions{
		GetCookies: l.jar.Header,
		OnConnect: func(ctx context.Context, fatal func(error)) error {
			return l.onConnect(ctx, sock, fatal)
		},
		Origin:    lsOrigin,
		WSURL:     l.wsURL,
		DialOpts:  websocket.DialOptions{HTTPClient: l.ws},
		Log:       zerolog.New(os.Stderr).Level(zerolog.WarnLevel).With().Timestamp().Str("component", "dgw").Logger(),
		Facebook:  true,
		LoggingID: true,
		AppID:     info.AppID,
		UserID:    info.UserID,
	})
	defer l.connected.Store(false)
	return sock.Connect(ctx)
}

func (l *Listener) onConnect(ctx context.Context, sock *dgw.Socket, fatal func(error)) error {
	payload, err := l.syncRequest(false)
	if err != nil {
		return err
	}
	ready := make(chan struct{})
	l.mu.Lock()
	l.resyncs, l.catchups, l.snapshotReq = 0, 0, 0
	l.stream, l.streamReady, l.sock = nil, ready, sock
	l.caughtUp.Store(false)
	l.mu.Unlock()
	stream, err := sock.EstablishStream(ctx, dgw.StreamInit{
		Parameters:   json.RawMessage(`{}`), // trình duyệt gửi {}, mautrix-meta để trống
		InitPayload:  payload,
		LogName:      "db " + lsDatabase,
		FrameHandler: l.handleFrame,
		OnClose:      func() { fatal(errStreamClosed) },
	})
	if err != nil {
		return err
	}
	l.mu.Lock()
	l.stream = stream
	l.mu.Unlock()
	close(ready)
	l.connected.Store(true)
	l.log.Info("listening", "page", l.cfg.PageID)
	go l.fwd.Heartbeat(ctx)
	return nil
}

// dbQuery giữ thứ tự khoá như trang gửi.
type dbQuery struct {
	Database          json.Number `json:"database"`
	EpochID           int64       `json:"epoch_id"`
	FailureCount      *int        `json:"failure_count"`
	LastAppliedCursor *string     `json:"last_applied_cursor"`
	SyncParams        *string     `json:"sync_params"`
	Version           json.Number `json:"version"`
}

type lsRequest struct {
	AppID     string `json:"app_id"`
	Payload   string `json:"payload"`
	RequestID int    `json:"request_id"`
	Type      int    `json:"type"`
}

// epochID như methods.GenerateEpochID của messagix.
func epochID() int64 { return time.Now().UnixMilli()<<22 | 42 }

// syncRequest: đồng bộ tiếp (type 2) từ cursor hiện có, hoặc đồng bộ lại từ đầu
// (type 1, cursor null, sync_params của trang) khi server yêu cầu (SendSyncParams).
func (l *Listener) syncRequest(fresh bool) ([]byte, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.syncRequestLocked(fresh)
}

func (l *Listener) syncRequestLocked(fresh bool) ([]byte, error) {
	q := dbQuery{Database: lsDatabase, EpochID: epochID(), Version: json.Number(l.info.Version)}
	t := 2
	if fresh {
		t = 1
		params := l.info.SyncParams
		if params == "" {
			params = `{"locale":"vi_VN"}`
		}
		q.SyncParams = &params
	} else {
		cursor := l.cursor
		q.LastAppliedCursor = &cursor
	}
	inner, err := json.Marshal(q)
	if err != nil {
		return nil, err
	}
	l.requestID++
	l.syncReq = l.requestID
	if fresh {
		l.snapshotReq = int64(l.requestID)
	}
	return json.Marshal(lsRequest{AppID: l.info.AppID, Payload: string(inner), RequestID: l.requestID, Type: t})
}

// handleFrame chạy trên goroutine frame của dgw; trả lỗi là đóng socket để nối lại.
func (l *Listener) handleFrame(_ context.Context, data []byte) error {
	env, calls, ok := decodeFrame(data)
	if !ok {
		l.log.Debug("non-lightspeed frame", "len", len(data))
		return nil
	}
	cursor, hasCursor := syncCursor(calls, lsDatabase)
	block, _ := syncBlock(calls, lsDatabase)
	for _, c := range calls {
		if c.Name == "handleSyncFailure" && !hasCursor {
			// Cursor đã lưu quá cũ hoặc bị từ chối: lấy cursor mới của trang.
			l.log.Warn("sync failure, restarting from the inbox page cursor", "database", str(c.Args, 0))
			l.mu.Lock()
			l.info, l.resetCursor = nil, true
			l.mu.Unlock()
			return errSyncFailure
		}
	}
	l.mu.Lock()
	snapshot := env.RequestID != nil && l.snapshotReq != 0 && *env.RequestID == l.snapshotReq
	liveBefore := l.liveUntilMs // mốc trước frame này: frame có thể vừa là ảnh chụp vừa báo theo kịp
	if block.Args != nil && arg(block.Args, 6) == true {
		// SendSyncParams: server không nhận cursor này (SyncStatus 3), đồng bộ lại
		// từ đầu như trang làm lúc tải. nextCursor kèm theo không dùng tiếp được.
		if l.resyncs >= 2 {
			l.mu.Unlock()
			return errors.New("server keeps asking for a full resync")
		}
		l.resyncs++
		req, err := l.syncRequestLocked(true)
		ready := l.streamReady
		l.mu.Unlock()
		if err != nil {
			return err
		}
		l.log.Info("server asked for a full resync", "sync_status", str(block.Args, 5))
		l.send(ready, req)
		return nil
	}
	if hasCursor && cursor != l.cursor {
		l.cursor = cursor
		l.saveStateLocked()
	}
	// Phản hồi của request mới nhất mà nextCursor khác currentCursor: chưa theo kịp,
	// gửi tiếp type 2 với cursor mới như trình duyệt (và mautrix) làm. Server chỉ
	// đẩy tin mới sau khi client đã theo kịp (thử thật 31/10/2026).
	var catchup []byte
	latest := env.RequestID != nil && *env.RequestID == int64(l.syncReq)
	if latest && block.Args != nil && str(block.Args, 3) != "" && str(block.Args, 3) != str(block.Args, 2) {
		if l.catchups < 10 {
			l.catchups++
			var err error
			if catchup, err = l.syncRequestLocked(false); err != nil {
				l.mu.Unlock()
				return err
			}
		} else {
			l.log.Warn("still not caught up after 10 sync requests")
		}
	} else if latest && block.Args != nil {
		if !l.caughtUp.Swap(true) {
			l.log.Info("caught up", "seq", str(block.Args, 4), "requests", l.catchups+l.resyncs+1)
		}
		l.liveUntilMs = time.Now().UnixMilli()
		l.saveStateLocked()
	}
	ready := l.streamReady
	var events []Event
	if snapshot {
		events = l.gapMessages(calls, liveBefore)
	} else {
		events = parseCalls(calls, l.cfg.PageID, time.Now().UnixMilli()-upsertFreshMs, l.contacts)
	}
	var fresh []Event
	for _, e := range events {
		if l.seen.add(eventKey(e)) {
			fresh = append(fresh, e)
		}
	}
	l.mu.Unlock()
	if catchup != nil {
		l.send(ready, catchup)
	}
	if l.log.Enabled(context.Background(), slog.LevelDebug) {
		var rid int64
		if env.RequestID != nil {
			rid = *env.RequestID
		}
		l.log.Debug("frame", "request_id", rid, "snapshot", snapshot, "calls", len(calls), "events", len(fresh), "cursor", hasCursor, "procs", describeCalls(calls))
	}
	for _, e := range fresh {
		l.fwd.Push(e)
	}
	return nil
}

func (l *Listener) heartbeatLoop(ctx context.Context) {
	t := time.NewTicker(heartbeatGap)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			// Chỉ báo sống khi socket còn mở: mất phiên thì fb-bridge-web báo dropped.
			if l.connected.Load() {
				l.fwd.Heartbeat(ctx)
			}
			if l.caughtUp.Load() && l.connected.Load() {
				l.mu.Lock()
				l.liveUntilMs = time.Now().UnixMilli()
				l.saveStateLocked()
				l.mu.Unlock()
			}
		}
	}
}

// dedupe nhớ tối đa n khoá gần nhất.
type dedupe struct {
	n     int
	keys  map[string]struct{}
	order []string
}

func newDedupe(n int) *dedupe { return &dedupe{n: n, keys: map[string]struct{}{}} }

func (d *dedupe) add(key string) bool {
	if _, ok := d.keys[key]; ok {
		return false
	}
	d.keys[key] = struct{}{}
	d.order = append(d.order, key)
	if len(d.order) > d.n {
		delete(d.keys, d.order[0])
		d.order = d.order[1:]
	}
	return true
}

// describeCalls cho log debug: tên procedure kèm số lần gọi (name×n); khối đồng bộ
// thì kèm tham số, chuỗi dài (cursor) chỉ ghi độ dài. Không ghi nội dung tin.
func describeCalls(calls []call) []string {
	var out []string
	counts := map[string]int{}
	var order []string
	for _, c := range calls {
		if !strings.HasPrefix(c.Name, "execute") && c.Name != "handleSyncFailure" {
			if counts[c.Name] == 0 {
				order = append(order, c.Name)
			}
			counts[c.Name]++
			continue
		}
		parts := []string{c.Name}
		for _, a := range c.Args {
			s := fmt.Sprint(a)
			if len(s) > 20 {
				s = fmt.Sprintf("<%d>", len(s))
			}
			parts = append(parts, s)
		}
		out = append(out, strings.Join(parts, " "))
	}
	for _, name := range order {
		out = append(out, fmt.Sprintf("%s×%d", name, counts[name]))
	}
	return out
}

// send gửi request trên stream db 205 mà không chặn goroutine frame của dgw
// (SendData chờ ack, ack đến qua chính vòng đọc).
func (l *Listener) send(ready <-chan struct{}, req []byte) {
	go func() {
		// Frame trả lời InitPayload có thể đến trước khi EstablishStream trả về
		// (thử thật 01/11/2026): chờ stream của kết nối này rồi mới gửi.
		select {
		case <-ready:
		case <-time.After(30 * time.Second):
			l.log.Warn("send sync request failed", "error", "stream not ready")
			return
		}
		l.mu.Lock()
		stream := l.stream
		l.mu.Unlock()
		if err := stream.SendData(context.Background(), req); err != nil {
			l.log.Warn("send sync request failed", "error", err)
		}
	}()
}

// gapMessages: ảnh chụp lịch sử lúc đồng bộ lại chỉ cho ra tin của khách đến sau
// mốc còn sống cuối cùng (tức là lúc mất kết nối), không đổ lịch sử vào Libredesk.
// Lần chạy đầu (chưa có mốc) thì không lấy gì. Cảm xúc và sửa tin trong lúc mất
// kết nối không lấy lại được (ảnh chụp không cho biết thời điểm thả cảm xúc).
func (l *Listener) gapMessages(calls []call, liveUntilMs int64) []Event {
	events := parseCalls(calls, l.cfg.PageID, 0, l.contacts)
	if liveUntilMs == 0 {
		return nil
	}
	since := liveUntilMs - gapMargin.Milliseconds()
	if floor := time.Now().Add(-gapMaxAge).UnixMilli(); since < floor {
		since = floor
	}
	var out []Event
	for _, e := range events {
		if e.Kind == "message" && e.TimestampMs >= since {
			out = append(out, e)
		}
	}
	// Ảnh chụp liệt kê tin mới nhất trước (thử thật 01/11/2026): gửi theo thứ tự thời gian.
	sort.SliceStable(out, func(i, j int) bool { return out[i].TimestampMs < out[j].TimestampMs })
	if len(out) > 0 {
		l.log.Info("recovered messages from the resync snapshot", "count", len(out))
	}
	return out
}

func (l *Listener) saveStateLocked() {
	if err := saveState(l.statePath(), State{Cursor: l.cursor, SavedAt: time.Now(), LiveUntilMs: l.liveUntilMs}); err != nil {
		l.log.Warn("save state failed", "error", err)
	}
}
