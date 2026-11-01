package main

// Đọc trang Hộp thư Business Suite một lần (như trình duyệt tải trang) để lấy
// những gì socket cần. Mọi thứ nằm trong các thẻ <script type="application/json">:
//   - ["DGWWebConfig",[],{"appId":...}]          -> x-dgw-appid, app_id của request
//   - ["CurrentUserInitialData",[],{"USER_ID":...}] -> x-dgw-uuid
//   - biến preload {"database":205,...,"version":N} -> version (LSVersion)
//   - RelayPrefetchedStreamCache: kết quả đồng bộ đầu của db 205 (payload Lightspeed),
//     có executeFirstBlockForSyncTransactionV4 [205, 0, null, nextCursor, ...] và
//     contact (tên khách, tên Trang). Trang dùng chính cursor này cho request type 2.
// Đối chiếu với trang thật 31/10/2026 (plan/bridge-ls/README.md).

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"

	"github.com/imroc/req/v3"
)

const lsDatabase = "205" // sync db của hộp thư Trang

type PageInfo struct {
	AppID      string
	UserID     string
	Version    string
	Cursor     string
	SyncParams string // sync_params của preload db 205 (locale, bloks_version), cho request type 1
	RegionHint string
	Contacts   map[string]string
	// DTSG, LSD: token form của trang, chỉ cho upload tệp (upload.go). Là bí mật
	// phiên: không ghi log, không lưu file.
	DTSG string
	LSD  string
}

var errLoggedOut = errors.New("facebook session is logged out")

var jsonScriptRe = regexp.MustCompile(`(?s)<script type="application/json"[^>]*>(.*?)</script>`)

// Chỉ giải mã script có một trong các dấu này: trang khoảng 2,8 MB, phần lớn không cần.
var scriptMarkers = []string{"DGWWebConfig", "CurrentUserInitialData", "DTSGInitialData", `"LSD"`, "executeFirstBlockForSyncTransactionV4", "setRegionHint", `database\`}

type pageScan struct {
	info       PageInfo
	versionAny string
}

func parsePage(html []byte) (*PageInfo, error) {
	info, err := scanPage(html)
	if err != nil {
		return nil, err
	}
	// Chỉ báo tên trường thiếu, không in giá trị.
	var missing []string
	for _, f := range []struct{ name, value string }{
		{"appId", info.AppID}, {"USER_ID", info.UserID}, {"version", info.Version}, {"cursor", info.Cursor},
	} {
		if f.value == "" {
			missing = append(missing, f.name)
		}
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("inbox page is missing %s", strings.Join(missing, ", "))
	}
	return info, nil
}

// parseSession chỉ cần token form và USER_ID (liệt kê Trang, pages.go).
func parseSession(html []byte) (*PageInfo, error) {
	info, err := scanPage(html)
	if err != nil {
		return nil, err
	}
	var missing []string
	for _, f := range []struct{ name, value string }{
		{"USER_ID", info.UserID}, {"fb_dtsg", info.DTSG}, {"lsd", info.LSD},
	} {
		if f.value == "" {
			missing = append(missing, f.name)
		}
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("business page is missing %s", strings.Join(missing, ", "))
	}
	return info, nil
}

func scanPage(html []byte) (*PageInfo, error) {
	s := &pageScan{info: PageInfo{Contacts: map[string]string{}}}
	for _, m := range jsonScriptRe.FindAllSubmatch(html, -1) {
		body := m[1]
		if !containsAny(body, scriptMarkers) {
			continue
		}
		var v any
		if err := decodeJSON(body, &v); err != nil {
			continue
		}
		s.walk(v, 0)
	}
	info := &s.info
	if info.Version == "" {
		info.Version = s.versionAny
	}
	if info.UserID == "0" {
		return nil, errLoggedOut
	}
	return info, nil
}

func (s *pageScan) walk(v any, depth int) {
	switch v := v.(type) {
	case []any:
		if len(v) >= 3 {
			if name, ok := v[0].(string); ok {
				if obj, ok := v[2].(map[string]any); ok {
					s.config(name, obj)
				}
			}
		}
		for _, child := range v {
			s.walk(child, depth)
		}
	case map[string]any:
		if step, ok := v["step"]; ok {
			s.lightspeed(collectCalls(step, nil))
		}
		if db, ok := v["database"]; ok {
			if ver := fmt.Sprint(v["version"]); v["version"] != nil {
				if fmt.Sprint(db) == lsDatabase {
					s.info.Version = ver
					if p, ok := v["sync_params"].(string); ok {
						s.info.SyncParams = p
					}
				} else if s.versionAny == "" {
					s.versionAny = ver
				}
			}
		}
		for _, child := range v {
			s.walk(child, depth)
		}
	case string:
		// Payload Lightspeed và biến preload là chuỗi JSON lồng trong JSON.
		if depth < 4 && len(v) > 2 && v[0] == '{' && (strings.Contains(v, `"step"`) || strings.Contains(v, `"database"`)) {
			var inner any
			if decodeJSON([]byte(v), &inner) == nil {
				s.walk(inner, depth+1)
			}
		}
	}
}

func (s *pageScan) config(name string, obj map[string]any) {
	switch name {
	case "DGWWebConfig":
		if id, ok := obj["appId"].(string); ok {
			s.info.AppID = id
		}
	case "CurrentUserInitialData":
		if id, ok := obj["USER_ID"].(string); ok {
			s.info.UserID = id
		}
	case "DTSGInitialData":
		if t, ok := obj["token"].(string); ok {
			s.info.DTSG = t
		}
	case "LSD":
		if t, ok := obj["token"].(string); ok {
			s.info.LSD = t
		}
	}
}

func (s *pageScan) lightspeed(calls []call) {
	if cursor, ok := syncCursor(calls, lsDatabase); ok {
		s.info.Cursor = cursor
	}
	for _, c := range calls {
		if c.Name == "setRegionHint" && str(c.Args, 1) != "" {
			s.info.RegionHint = str(c.Args, 1)
		}
	}
	updateContacts(calls, s.info.Contacts)
}

// fetchPage tải trang Hộp thư bằng cookie phiên, header như Chrome mở trang.
func fetchPage(ctx context.Context, client *req.Client, jar *CookieJar, ua UserAgent, pageID string) (*PageInfo, error) {
	return fetchParsed(ctx, client, jar, ua, inboxURL+"?asset_id="+pageID, parsePage)
}

// fetchSession tải Hộp thư không chọn Trang, chỉ lấy token form (pages.go).
func fetchSession(ctx context.Context, client *req.Client, jar *CookieJar, ua UserAgent) (*PageInfo, error) {
	return fetchParsed(ctx, client, jar, ua, inboxURL, parseSession)
}

const inboxURL = "https://business.facebook.com/latest/inbox/all/"

func fetchParsed(ctx context.Context, client *req.Client, jar *CookieJar, ua UserAgent, url string, parse func([]byte) (*PageInfo, error)) (*PageInfo, error) {
	return fetchParsedHop(ctx, client, jar, ua, url, parse, 1)
}

// Hộp thư không có asset_id thì Business Suite chuyển sang Trang mặc định
// (thử 01/11/2026): đi theo tối đa một lần, chỉ trong /latest/ của business.facebook.com.
func fetchParsedHop(ctx context.Context, client *req.Client, jar *CookieJar, ua UserAgent, url string, parse func([]byte) (*PageInfo, error), hops int) (*PageInfo, error) {
	resp, err := client.R().SetContext(ctx).
		SetHeaders(ua.navigationHeaders()).
		SetHeader("cookie", jar.Header()).
		Get(url)
	if err != nil {
		return nil, fmt.Errorf("fetch inbox page: %w", err)
	}
	jar.Update(resp.Header)
	switch {
	case resp.StatusCode >= 300 && resp.StatusCode < 400:
		// Hết phiên thì Facebook chuyển sang trang đăng nhập hoặc checkpoint.
		loc := resp.Header.Get("Location")
		if strings.Contains(loc, "login") || strings.Contains(loc, "checkpoint") {
			return nil, errLoggedOut
		}
		if hops > 0 && strings.HasPrefix(loc, lsOrigin+"/latest/") {
			return fetchParsedHop(ctx, client, jar, ua, loc, parse, hops-1)
		}
		return nil, fmt.Errorf("fetch inbox page: redirect to %s", redactURL(loc))
	case resp.StatusCode != http.StatusOK:
		return nil, fmt.Errorf("fetch inbox page: HTTP %d", resp.StatusCode)
	}
	info, err := parse(resp.Bytes())
	if err != nil {
		return nil, fmt.Errorf("%w (%s)", err, describeHTML(resp.Bytes()))
	}
	return info, nil
}

var (
	titleRe    = regexp.MustCompile(`(?is)<title[^>]*>(.*?)</title>`)
	scriptRe   = regexp.MustCompile(`(?is)<(script|style|noscript)[^>]*>.*?</(script|style|noscript)>`)
	tagRe      = regexp.MustCompile(`(?s)<[^>]*>`)
	redirectRe = regexp.MustCompile(`location\.(?:replace|href)\s*(?:\(|=)\s*"([^"]+)"`)
)

// describeHTML tóm tắt trang không đọc được: kích thước, tiêu đề, số khối JSON và
// đường dẫn chuyển hướng bằng JavaScript (bỏ query), không in dữ liệu phiên.
func describeHTML(html []byte) string {
	title := ""
	if m := titleRe.FindSubmatch(html); m != nil {
		title = strings.TrimSpace(string(m[1]))
	}
	redirect := ""
	if m := redirectRe.FindSubmatch(html); m != nil {
		redirect = redactURL(strings.ReplaceAll(string(m[1]), `\/`, "/"))
	}
	text := strings.Join(strings.Fields(tagRe.ReplaceAllString(scriptRe.ReplaceAllString(string(html), " "), " ")), " ")
	if r := []rune(text); len(r) > 300 {
		text = string(r[:300])
	}
	return fmt.Sprintf("bytes=%d title=%q json_scripts=%d js_redirect=%q text=%q",
		len(html), title, len(jsonScriptRe.FindAll(html, -1)), redirect, text)
}

// redactURL bỏ query (có thể chứa token) khi ghi log.
func redactURL(u string) string {
	if i := strings.IndexAny(u, "?#"); i >= 0 {
		return u[:i]
	}
	return u
}

func containsAny(b []byte, needles []string) bool {
	for _, n := range needles {
		if bytes.Contains(b, []byte(n)) {
			return true
		}
	}
	return false
}
