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
}

var errLoggedOut = errors.New("facebook session is logged out")

var jsonScriptRe = regexp.MustCompile(`(?s)<script type="application/json"[^>]*>(.*?)</script>`)

// Chỉ giải mã script có một trong các dấu này: trang khoảng 2,8 MB, phần lớn không cần.
var scriptMarkers = []string{"DGWWebConfig", "CurrentUserInitialData", "executeFirstBlockForSyncTransactionV4", "setRegionHint", `database\`}

type pageScan struct {
	info       PageInfo
	versionAny string
}

func parsePage(html []byte) (*PageInfo, error) {
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
	url := "https://business.facebook.com/latest/inbox/all/?asset_id=" + pageID
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
		return nil, fmt.Errorf("fetch inbox page: redirect to %s", redactURL(loc))
	case resp.StatusCode != http.StatusOK:
		return nil, fmt.Errorf("fetch inbox page: HTTP %d", resp.StatusCode)
	}
	return parsePage(resp.Bytes())
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
