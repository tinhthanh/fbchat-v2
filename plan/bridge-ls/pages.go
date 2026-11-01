package main

// Liệt kê các Trang của tài khoản đã đăng nhập, như ô chọn Trang ở góc trên bên
// trái Hộp thư Business Suite (bắt frame 01/11/2026, README.md mục "Danh sách
// Trang"): GraphQL NorthStarBusinessUnifiedScopingSelector...AllZeroLevelScopesQuery,
// trả personal_scopes.edges[].node{scope_id, scope_name, scope_type "PAGE"}.
// scope_id chính là asset_id dùng cho socket. Trang thuộc portfolio doanh nghiệp
// (business_scopes) chưa có: tài khoản bắt frame không có portfolio.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"github.com/imroc/req/v3"
)

const (
	pagesQueryName = "NorthStarBusinessUnifiedScopingSelectorFirstAndZeroLevelScopesSectionAllZeroLevelScopesQuery"
	pagesDocID     = "37857269907251700" // đổi theo bản phát hành của Facebook; FBLS_PAGES_DOC_ID để thay
	graphqlURL     = "https://business.facebook.com/api/graphql/"
)

var errGraphQLNetwork = errors.New("lỗi mạng")

// PageItem: một Trang trong danh sách. Picture là URL fbcdn có chữ ký, hết hạn sau vài ngày.
type PageItem struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Picture string `json:"picture,omitempty"`
}

// Biến relay provider của query: thiếu thì server báo missing_required_variable_value
// mà không nói tên. Tên lấy từ module JS công khai "<query>$Parameters" (01/11/2026);
// trang gửi true. Facebook đổi bản phát hành thì doc_id và tên biến có thể đổi:
// listPages tự tìm lại trong JS của trang (pagesquery.go).
const portfolioProvider = "__relay_internal__pv__IsBusinessPortfolioOverviewLinkEnabledrelayprovider"

var missingVarRe = regexp.MustCompile(`__relay_internal__pv__[A-Za-z0-9_]+`)

// listPages tải Hộp thư một lần lấy token form, rồi hỏi danh sách Trang. Query hỏng
// (không phải hết phiên hay lỗi mạng) thì tìm lại doc_id và biến trong JS của trang
// một lần (dl: client tải tệp công khai, không gửi cookie); cache nhớ bản tìm được.
func listPages(ctx context.Context, client, dl *req.Client, jar *CookieJar, ua UserAgent, cache *queryCache) ([]PageItem, error) {
	info, err := fetchSession(ctx, client, jar, ua)
	if err != nil {
		return nil, err
	}
	if info.UserID != jar.Get("c_user") {
		return nil, errors.New("business page USER_ID does not match the c_user cookie")
	}
	q := cache.get()
	pages, err := queryPages(ctx, client, jar, ua, info, q)
	if err == nil || errors.Is(err, errLoggedOut) || errors.Is(err, errGraphQLNetwork) || dl == nil {
		return pages, err
	}
	found, derr := discoverPagesQuery(ctx, dl, ua, info.Scripts)
	if derr != nil {
		return nil, fmt.Errorf("%w (không tìm lại được query trong JS của trang: %v)", err, derr)
	}
	if found.DocID == q.DocID && sameStrings(found.Providers, q.Providers) {
		return nil, err
	}
	pages, err2 := queryPages(ctx, client, jar, ua, info, found)
	if err2 != nil {
		return nil, fmt.Errorf("%w (doc_id mới %s cũng lỗi: %v)", err, found.DocID, err2)
	}
	cache.set(found)
	return pages, nil
}

// queryPages gửi query; server nêu tên biến thiếu thì gửi lại một lần với biến đó.
func queryPages(ctx context.Context, client *req.Client, jar *CookieJar, ua UserAgent, info *PageInfo, q pagesQuery) ([]PageItem, error) {
	providers := append([]string(nil), q.Providers...)
	for attempt := 0; ; attempt++ {
		raw, err := postGraphQL(ctx, client, jar, ua, info, pagesQueryName, q.DocID, pagesVariables(info.UserID, providers))
		if err != nil {
			return nil, err
		}
		pages, err := parsePages(raw)
		if err == nil {
			return pages, nil
		}
		names := missingVarRe.FindAllString(err.Error(), -1)
		if attempt > 0 || len(names) == 0 {
			return nil, err
		}
		providers = append(providers, names...)
	}
}

func pagesVariables(userID string, providers []string) map[string]any {
	v := map[string]any{
		"businessToolName":  "MBS_INBOX",
		"contextUri":        inboxURL,
		"countLimit":        201,
		"filterParams":      map[string]any{"business_tool_name": "MBS_INBOX", "should_check_inbox_onboarding": true},
		"firstLevelScopeId": userID, "searchInput": "", "zeroLevelScopeId": nil,
		"bagScopeIDs": nil, "businessIdForAddAA": userID,
		"fetchNumberForBagChildrenScopes": 0, "fetchNumberForBagScopes": 6,
		"fetchNumberForBusinessChildrenScopes": 40, "fetchNumberForPersonalChildrenScopes": 40,
		"isPersonalAccount": true, "scopeIDs": []string{userID},
		"shouldFetchBusinessRecommendationInfo": false,
	}
	for _, p := range providers {
		v[p] = true
	}
	return v
}

// postGraphQL gửi form như Relay của trang. Lỗi chỉ ghi tên query và mã HTTP.
func postGraphQL(ctx context.Context, client *req.Client, jar *CookieJar, ua UserAgent, info *PageInfo, name, docID string, variables map[string]any) ([]byte, error) {
	vars, err := json.Marshal(variables)
	if err != nil {
		return nil, err
	}
	form := url.Values{
		"av": {info.UserID}, "__user": {info.UserID}, "__a": {"1"}, "__req": {"1"},
		"fb_dtsg": {info.DTSG}, "jazoest": {jazoest(info.DTSG)}, "lsd": {info.LSD},
		"fb_api_caller_class": {"RelayModern"}, "fb_api_req_friendly_name": {name},
		"variables": {string(vars)}, "server_timestamps": {"true"}, "doc_id": {docID},
	}
	resp, err := client.R().SetContext(ctx).DisableAutoReadResponse().
		SetHeaders(map[string]string{
			"user-agent": ua.UA, "sec-ch-ua": ua.SecCHUA, "sec-ch-ua-mobile": "?0", "sec-ch-ua-platform": ua.Platform,
			"accept": "*/*", "accept-language": acceptLanguage, "origin": lsOrigin, "referer": inboxURL,
			"sec-fetch-dest": "empty", "sec-fetch-mode": "cors", "sec-fetch-site": "same-origin",
			"content-type": "application/x-www-form-urlencoded", "x-fb-friendly-name": name, "x-fb-lsd": info.LSD,
			"cookie": jar.Header(),
		}).
		SetBodyString(form.Encode()).
		Post(graphqlURL)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", name, errGraphQLNetwork)
	}
	defer resp.Body.Close()
	jar.Update(resp.Header)
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: HTTP %d", name, resp.StatusCode)
	}
	return raw, nil
}

// parsePages đọc phản hồi; GraphQL có thể trả 200 kèm errors.
func parsePages(raw []byte) ([]PageItem, error) {
	// Có lúc Facebook nối nhiều dòng JSON (phần defer): chỉ dòng đầu là dữ liệu chính.
	if i := strings.IndexByte(string(raw), '\n'); i > 0 {
		raw = raw[:i]
	}
	var r struct {
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
		Error        any    `json:"error"`
		ErrorSummary string `json:"errorSummary"`
		Data         struct {
			Viewer *struct {
				Scoping *struct {
					Personal *struct {
						Edges []struct {
							Node struct {
								ID      string `json:"scope_id"`
								Name    string `json:"scope_name"`
								Type    string `json:"scope_type"`
								Picture *struct {
									URI string `json:"uri"`
								} `json:"scope_profile_picture"`
							} `json:"node"`
						} `json:"edges"`
					} `json:"personal_scopes"`
				} `json:"meta_business_scoping"`
			} `json:"viewer"`
		} `json:"data"`
	}
	if err := decodeJSON(raw, &r); err != nil {
		return nil, errors.New("danh sách Trang: phản hồi không đọc được")
	}
	if r.Error != nil && fmt.Sprint(r.Error) != "0" {
		if fmt.Sprint(r.Error) == "1357001" {
			return nil, errLoggedOut
		}
		return nil, fmt.Errorf("danh sách Trang: Facebook trả lỗi %v %s", r.Error, r.ErrorSummary)
	}
	if v := r.Data.Viewer; v == nil || v.Scoping == nil || v.Scoping.Personal == nil {
		msgs := make([]string, 0, len(r.Errors))
		for _, e := range r.Errors {
			msgs = append(msgs, e.Message)
		}
		if len(msgs) == 0 {
			msgs = append(msgs, "thiếu personal_scopes")
		}
		return nil, fmt.Errorf("danh sách Trang: %s", strings.Join(msgs, "; "))
	}
	pages := []PageItem{}
	seen := map[string]bool{}
	for _, e := range r.Data.Viewer.Scoping.Personal.Edges {
		n := e.Node
		if n.Type != "PAGE" || seen[n.ID] {
			continue
		}
		if _, err := strconv.ParseUint(n.ID, 10, 64); err != nil {
			continue
		}
		seen[n.ID] = true
		item := PageItem{ID: n.ID, Name: n.Name}
		if n.Picture != nil {
			item.Picture = n.Picture.URI
		}
		pages = append(pages, item)
	}
	return pages, nil
}
