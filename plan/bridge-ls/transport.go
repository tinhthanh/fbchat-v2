package main

// Danh tính trình duyệt và kết nối. Phiên đăng nhập là của Chromium trong
// fbweb-browser (Linux), nên mọi yêu cầu mang đúng User-Agent và client hints
// của trình duyệt đó (đọc qua CDP 31/10/2026): một phiên mang nhiều trình duyệt
// là dấu hiệu Facebook dùng để huỷ phiên. TLS giả Chrome bằng uTLS như
// mautrix-meta (httpclient.forceHTTP1ChromeFingerprint).

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/imroc/req/v3"
	utls "github.com/refraction-networking/utls"
)

type UserAgent struct {
	UA       string // navigator.userAgent
	SecCHUA  string // sec-ch-ua
	Platform string // sec-ch-ua-platform
}

// Chromium 142 trên Alpine trong fbweb-browser.
var defaultUserAgent = UserAgent{
	UA:       "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/142.0.0.0 Safari/537.36",
	SecCHUA:  `"Not_A Brand";v="99", "Chromium";v="142"`,
	Platform: `"Linux"`,
}

const acceptLanguage = "en-US,en;q=0.9"

func (u UserAgent) navigationHeaders() map[string]string {
	return map[string]string{
		"user-agent":                u.UA,
		"sec-ch-ua":                 u.SecCHUA,
		"sec-ch-ua-mobile":          "?0",
		"sec-ch-ua-platform":        u.Platform,
		"accept":                    "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,image/apng,*/*;q=0.8",
		"accept-language":           acceptLanguage,
		"cache-control":             "no-cache",
		"pragma":                    "no-cache",
		"sec-fetch-dest":            "document",
		"sec-fetch-mode":            "navigate",
		"sec-fetch-site":            "none",
		"sec-fetch-user":            "?1",
		"upgrade-insecure-requests": "1",
	}
}

// newHTTPClient: tải trang Hộp thư và file đính kèm. Cookie do CookieJar quản lý,
// không theo redirect (redirect sang trang đăng nhập nghĩa là hết phiên).
func newHTTPClient() *req.Client {
	return req.C().
		ImpersonateChrome().
		SetCookieJar(nil).
		SetRedirectPolicy(req.NoRedirectPolicy()).
		SetTimeout(60 * time.Second)
}

// Link file trong tin (video, có khi cả tin thoại) trỏ tới facebook.com rồi chuyển
// hướng sang CDN, như trình duyệt vẫn theo. Chỉ theo tới facebook.com hoặc fbcdn.net
// qua https; Go tự bỏ cookie khi chuyển sang tên miền khác.
var downloadHosts = []string{"facebook.com", "fbcdn.net"}

func newDownloadClient() *req.Client {
	return newHTTPClient().SetRedirectPolicy(func(r *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return fmt.Errorf("stopped after %d redirects", len(via))
		}
		host := r.URL.Hostname()
		for _, d := range downloadHosts {
			if r.URL.Scheme == "https" && (host == d || strings.HasSuffix(host, "."+d)) {
				return nil
			}
		}
		return fmt.Errorf("redirect to %s not allowed", host)
	})
}

// newWebsocketClient: client HTTP/1.1 cho bắt tay WebSocket, TLS giả Chrome,
// URL và header viết lại theo đúng thứ tự trình duyệt gửi.
func newWebsocketClient(ua UserAgent, pageID string, regionHint func() string) *http.Client {
	base := &http.Transport{
		DialTLSContext:      dialChromeTLS,
		TLSHandshakeTimeout: 20 * time.Second,
		ForceAttemptHTTP2:   false,
	}
	return &http.Client{
		Transport: &browserTransport{base: base, ua: ua, pageID: pageID, regionHint: regionHint},
		Timeout:   30 * time.Second,
	}
}

type browserTransport struct {
	base       http.RoundTripper
	ua         UserAgent
	pageID     string
	regionHint func() string
}

func (t *browserTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.URL.RawQuery = browserQuery(r.URL.Query(), t.pageID, t.regionHint())
	// Header như Chrome gửi lúc bắt tay (CDP Network.webSocketWillSendHandshakeRequest);
	// dgw thêm sec-fetch-* mà Chrome không có trong danh sách đó.
	for _, h := range []string{"Sec-Fetch-Dest", "Sec-Fetch-Mode", "Sec-Fetch-Site"} {
		r.Header.Del(h)
	}
	r.Header.Set("User-Agent", t.ua.UA)
	r.Header.Set("Accept-Language", acceptLanguage)
	r.Header.Set("Cache-Control", "no-cache")
	r.Header.Set("Pragma", "no-cache")
	return t.base.RoundTrip(r)
}

// browserQuery dựng query theo thứ tự trang Business Suite, thêm hai tham số
// mautrix-meta không có: x-dgw-regionhint và x-dgw-app-asset_ids (chọn hộp thư Trang).
func browserQuery(q url.Values, pageID, regionHint string) string {
	if regionHint != "" {
		q.Set("x-dgw-regionhint", regionHint)
	}
	q.Set("x-dgw-app-asset_ids", fmt.Sprintf(`{"PAGE":"%s"}`, pageID))
	order := []string{
		"x-dgw-appid", "x-dgw-appversion", "x-dgw-authtype", "x-dgw-version", "x-dgw-uuid",
		"x-dgw-tier", "x-dgw-loggingid", "x-dgw-regionhint", "x-dgw-deviceid", "x-dgw-app-asset_ids",
	}
	var parts []string
	for _, k := range order {
		if q.Has(k) {
			parts = append(parts, url.QueryEscape(k)+"="+url.QueryEscape(q.Get(k)))
			q.Del(k)
		}
	}
	if rest := q.Encode(); rest != "" {
		parts = append(parts, rest)
	}
	return strings.Join(parts, "&")
}

func dialChromeTLS(ctx context.Context, network, addr string) (net.Conn, error) {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	plain, err := (&net.Dialer{Timeout: 20 * time.Second, KeepAlive: 30 * time.Second}).DialContext(ctx, network, addr)
	if err != nil {
		return nil, err
	}
	spec, err := utls.UTLSIdToSpec(utls.HelloChrome_120)
	if err != nil {
		plain.Close()
		return nil, fmt.Errorf("chrome uTLS spec: %w", err)
	}
	// WebSocket cần HTTP/1.1: bỏ ALPS và chỉ quảng cáo http/1.1.
	exts := spec.Extensions[:0]
	for _, ext := range spec.Extensions {
		switch e := ext.(type) {
		case *utls.ApplicationSettingsExtension:
			continue
		case *utls.ALPNExtension:
			e.AlpnProtocols = []string{"http/1.1"}
		}
		exts = append(exts, ext)
	}
	spec.Extensions = exts
	conn := utls.UClient(plain, &utls.Config{ServerName: host, NextProtos: []string{"http/1.1"}, MinVersion: tls.VersionTLS12}, utls.HelloCustom)
	if err := conn.ApplyPreset(&spec); err != nil {
		plain.Close()
		return nil, fmt.Errorf("apply chrome uTLS spec: %w", err)
	}
	if err := conn.HandshakeContext(ctx); err != nil {
		plain.Close()
		return nil, err
	}
	return conn, nil
}
