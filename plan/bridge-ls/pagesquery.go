package main

// Tìm lại query danh sách Trang khi Facebook đổi bản phát hành (doc_id đổi, hoặc
// thêm biến relay provider bắt buộc). Cách làm giống lần tìm tay 01/11/2026: trang
// Hộp thư nạp các tệp JS công khai trên static.xx.fbcdn.net; trong đó có
//   __d("<query>_facebookRelayOperation",[],(function(t,n,r,o,a,i){a.exports="<doc_id>"}),null)
//   __d("<query>$Parameters",[...],(function(...){... providedVariables:{__relay_internal__pv__X:n("..."),...}
// Tải tệp không kèm cookie (tệp công khai), song song, dừng khi thấy đủ hai module.
// Bản tìm được lưu ở FBLS_DATA/pages-query.json để lần sau dùng ngay.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/imroc/req/v3"
)

const (
	maxScriptBytes   = 8 << 20
	discoverWorkers  = 8
	discoverTimeout  = 3 * time.Minute
	maxScriptsToScan = 3000
)

var (
	// Tệp JS của trang, trong HTML có dạng "https:\/\/static.xx.fbcdn.net\/rsrc.php\/...js?...".
	scriptURLRe = regexp.MustCompile(`https:\\?/\\?/static\.xx\.fbcdn\.net\\?/rsrc\.php[^"'\s?]+\.js`)
	docIDRe     = regexp.MustCompile(`__d\("` + regexp.QuoteMeta(pagesQueryName) + `_facebookRelayOperation",\[\],\(function\([^)]*\)\{[A-Za-z_$]+\.exports="(\d+)"`)
	paramsHead  = []byte(`__d("` + pagesQueryName + `$Parameters"`)
	providedRe  = regexp.MustCompile(`providedVariables:\{([^}]*)\}`)
	providerRe  = regexp.MustCompile(`(__relay_internal__pv__[A-Za-z0-9_]+)\s*:`)
)

type pagesQuery struct {
	DocID     string    `json:"doc_id"`
	Providers []string  `json:"providers"`
	FoundAt   time.Time `json:"found_at,omitempty"`
}

func defaultPagesQuery() pagesQuery {
	return pagesQuery{DocID: env("FBLS_PAGES_DOC_ID", pagesDocID), Providers: []string{portfolioProvider}}
}

// queryCache: nil dùng mặc định và không lưu (lệnh fbweb-ls pages).
type queryCache struct {
	path string
	mu   sync.Mutex
}

func (c *queryCache) get() pagesQuery {
	if c == nil {
		return defaultPagesQuery()
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	var q pagesQuery
	raw, err := os.ReadFile(c.path)
	if err != nil || json.Unmarshal(raw, &q) != nil || q.DocID == "" {
		return defaultPagesQuery()
	}
	return q
}

func (c *queryCache) set(q pagesQuery) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	q.FoundAt = time.Now().UTC()
	if raw, err := json.Marshal(q); err == nil {
		_ = writeFileAtomic(c.path, raw)
	}
}

// scriptURLs lấy các tệp JS công khai trong HTML (bỏ query: có tham số theo người dùng).
func scriptURLs(html []byte) []string {
	seen := map[string]bool{}
	var out []string
	for _, m := range scriptURLRe.FindAll(html, -1) {
		u := strings.ReplaceAll(string(m), `\/`, "/")
		if !seen[u] {
			seen[u] = true
			out = append(out, u)
		}
	}
	return out
}

// findPagesQuery đọc doc_id và tên biến provider trong một tệp JS (có thể chỉ có một phần).
func findPagesQuery(js []byte) (docID string, providers []string, hasParams bool) {
	if m := docIDRe.FindSubmatch(js); m != nil {
		docID = string(m[1])
	}
	// Module $Parameters ngắn: providedVariables nằm trong khoảng 4 KB sau tên module.
	if i := bytes.Index(js, paramsHead); i >= 0 {
		hasParams = true
		window := js[i:min(len(js), i+4096)]
		if m := providedRe.FindSubmatch(window); m != nil {
			for _, p := range providerRe.FindAllSubmatch(m[1], -1) {
				providers = append(providers, string(p[1]))
			}
		}
	}
	return docID, providers, hasParams
}

func discoverPagesQuery(ctx context.Context, dl *req.Client, ua UserAgent, urls []string) (pagesQuery, error) {
	if len(urls) == 0 {
		return pagesQuery{}, errors.New("trang Hộp thư không có tệp JS nào")
	}
	if len(urls) > maxScriptsToScan {
		urls = urls[:maxScriptsToScan]
	}
	ctx, cancel := context.WithTimeout(ctx, discoverTimeout)
	defer cancel()
	jobs := make(chan string)
	var (
		mu        sync.Mutex
		q         pagesQuery
		hasParams bool
		failed    int
	)
	var wg sync.WaitGroup
	for i := 0; i < discoverWorkers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for u := range jobs {
				js, err := fetchScript(ctx, dl, ua, u)
				mu.Lock()
				if err != nil {
					failed++
				} else {
					doc, prov, params := findPagesQuery(js)
					if doc != "" {
						q.DocID = doc
					}
					if params {
						hasParams, q.Providers = true, prov
					}
					if q.DocID != "" && hasParams {
						cancel()
					}
				}
				mu.Unlock()
			}
		}()
	}
feed:
	for _, u := range urls {
		select {
		case jobs <- u:
		case <-ctx.Done():
			break feed
		}
	}
	close(jobs)
	wg.Wait()
	switch {
	case q.DocID == "":
		return pagesQuery{}, fmt.Errorf("không thấy module %s_facebookRelayOperation trong %d tệp JS (%d tệp tải lỗi)", pagesQueryName, len(urls), failed)
	case !hasParams:
		return pagesQuery{}, fmt.Errorf("thấy doc_id %s nhưng không thấy module $Parameters", q.DocID)
	}
	sort.Strings(q.Providers)
	return q, nil
}

func fetchScript(ctx context.Context, dl *req.Client, ua UserAgent, u string) ([]byte, error) {
	resp, err := dl.R().SetContext(ctx).DisableAutoReadResponse().
		SetHeaders(map[string]string{
			"user-agent": ua.UA, "sec-ch-ua": ua.SecCHUA, "sec-ch-ua-mobile": "?0", "sec-ch-ua-platform": ua.Platform,
			"accept": "*/*", "accept-language": acceptLanguage, "origin": lsOrigin, "referer": lsOrigin + "/",
			"sec-fetch-dest": "script", "sec-fetch-mode": "cors", "sec-fetch-site": "cross-site",
		}).Get(u)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, maxScriptBytes))
}

func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	x, y := append([]string(nil), a...), append([]string(nil), b...)
	sort.Strings(x)
	sort.Strings(y)
	for i := range x {
		if x[i] != y[i] {
			return false
		}
	}
	return true
}
