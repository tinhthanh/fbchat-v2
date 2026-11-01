package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/imroc/req/v3"
)

// Hai module thật (rút gọn) trong JS của Business Suite, 01/11/2026.
const (
	jsOperation = `;__d("NorthStarBusinessUnifiedScopingSelectorFirstAndZeroLevelScopesSectionAllZeroLevelScopesQuery_facebookRelayOperation",[],(function(t,n,r,o,a,i){a.exports="37857269907251700"}),null);`
	jsParams    = `__d("NorthStarBusinessUnifiedScopingSelectorFirstAndZeroLevelScopesSectionAllZeroLevelScopesQuery$Parameters",["IsBusinessPortfolioOverviewLinkEnabled.relayprovider","NorthStarBusinessUnifiedScopingSelectorFirstAndZeroLevelScopesSectionAllZeroLevelScopesQuery_facebookRelayOperation"],(function(t,n,r,o,a,i){"use strict";var e={kind:"PreloadableConcreteRequest",params:{id:n("NorthStarBusinessUnifiedScopingSelectorFirstAndZeroLevelScopesSectionAllZeroLevelScopesQuery_facebookRelayOperation"),metadata:{},name:"NorthStarBusinessUnifiedScopingSelectorFirstAndZeroLevelScopesSectionAllZeroLevelScopesQuery",operationKind:"query",text:null,providedVariables:{__relay_internal__pv__IsBusinessPortfolioOverviewLinkEnabledrelayprovider:n("IsBusinessPortfolioOverviewLinkEnabled.relayprovider")}}};a.exports=e}),null);`
)

func TestFindPagesQuery(t *testing.T) {
	doc, prov, params := findPagesQuery([]byte("var x=1" + jsOperation + "var y=2"))
	if doc != "37857269907251700" || params || prov != nil {
		t.Fatalf("operation: %q %v %v", doc, prov, params)
	}
	doc, prov, params = findPagesQuery([]byte(jsParams))
	if doc != "" || !params || len(prov) != 1 || prov[0] != portfolioProvider {
		t.Fatalf("params: %q %v %v", doc, prov, params)
	}
	// Bản phát hành sau có thể không còn biến nào: vẫn là thấy module.
	none := strings.Replace(jsParams, `__relay_internal__pv__IsBusinessPortfolioOverviewLinkEnabledrelayprovider:n("IsBusinessPortfolioOverviewLinkEnabled.relayprovider")`, "", 1)
	if _, prov, params := findPagesQuery([]byte(none)); !params || len(prov) != 0 {
		t.Fatalf("no providers: %v %v", prov, params)
	}
	if doc, _, params := findPagesQuery([]byte(`__d("OtherQuery_facebookRelayOperation",[],(function(t,n,r,o,a,i){a.exports="1"}),null);`)); doc != "" || params {
		t.Fatal("matched another query")
	}
}

func TestScriptURLs(t *testing.T) {
	html := `<script src="https://static.xx.fbcdn.net/rsrc.php/v4/y6/r/mR6LA8E7mEx.js?_nc_eui2=AeHZ" ></script>` +
		`{"url":"https:\/\/static.xx.fbcdn.net\/rsrc.php\/v4i8Ds4\/yX\/l\/vi_VN-j\/mBzfomz9h4J.js?_nc_eui2=x"}` +
		`<script src="https://static.xx.fbcdn.net/rsrc.php/v4/y6/r/mR6LA8E7mEx.js?other"></script>` +
		`<script src="https://evil.example/rsrc.php/a.js"></script>`
	got := scriptURLs([]byte(html))
	want := []string{"https://static.xx.fbcdn.net/rsrc.php/v4/y6/r/mR6LA8E7mEx.js", "https://static.xx.fbcdn.net/rsrc.php/v4i8Ds4/yX/l/vi_VN-j/mBzfomz9h4J.js"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("got %v", got)
	}
}

func TestDiscoverPagesQuery(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.Header.Get("cookie") != "" {
			t.Error("cookie sent to a public script")
		}
		switch r.URL.Path {
		case "/s7.js":
			fmt.Fprint(w, "x"+jsOperation)
		case "/s12.js":
			fmt.Fprint(w, jsParams)
		case "/s3.js":
			http.Error(w, "gone", http.StatusNotFound)
		default:
			fmt.Fprint(w, "__d(\"Unrelated\",[],function(){});")
		}
	}))
	defer srv.Close()
	var urls []string
	for i := 0; i < 400; i++ {
		urls = append(urls, fmt.Sprintf("%s/s%d.js", srv.URL, i))
	}
	q, err := discoverPagesQuery(t.Context(), req.C(), defaultUserAgent, urls)
	if err != nil {
		t.Fatal(err)
	}
	if q.DocID != "37857269907251700" || len(q.Providers) != 1 || q.Providers[0] != portfolioProvider {
		t.Fatalf("query %+v", q)
	}
	if n := hits.Load(); n > 100 {
		t.Fatalf("did not stop early: %d requests", n)
	}
	if _, err := discoverPagesQuery(t.Context(), req.C(), defaultUserAgent, urls[13:20]); err == nil || !strings.Contains(err.Error(), "không thấy module") {
		t.Fatalf("missing: %v", err)
	}
	if _, err := discoverPagesQuery(t.Context(), req.C(), defaultUserAgent, nil); err == nil {
		t.Fatal("no scripts must fail")
	}
}

func TestQueryCache(t *testing.T) {
	var nilCache *queryCache
	if q := nilCache.get(); q.DocID != pagesDocID || !sameStrings(q.Providers, []string{portfolioProvider}) {
		t.Fatalf("default %+v", q)
	}
	nilCache.set(pagesQuery{DocID: "1"}) // không lưu, không panic
	c := &queryCache{path: filepath.Join(t.TempDir(), "pages-query.json")}
	if c.get().DocID != pagesDocID {
		t.Fatal("empty cache must give the default")
	}
	c.set(pagesQuery{DocID: "42", Providers: []string{"__relay_internal__pv__B", "__relay_internal__pv__A"}})
	q := c.get()
	if q.DocID != "42" || !sameStrings(q.Providers, []string{"__relay_internal__pv__A", "__relay_internal__pv__B"}) || q.FoundAt.IsZero() {
		t.Fatalf("cached %+v", q)
	}
	if sameStrings([]string{"a"}, []string{"a", "b"}) || !sameStrings(nil, []string{}) {
		t.Fatal("sameStrings")
	}
}
