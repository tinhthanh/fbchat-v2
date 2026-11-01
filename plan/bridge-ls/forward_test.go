package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func testForwarder(t *testing.T, url string) *Forwarder {
	t.Helper()
	jar := testJar(t, t.TempDir())
	return NewForwarder(Forwarder{URL: url, Token: testToken, Account: "vetgo", PageID: testPageID, PageName: func() string { return "VetGo" }},
		newDownloadClient().EnableInsecureSkipVerify(), jar, defaultUserAgent, testLog())
}

func shortRetries(t *testing.T) {
	old := retryDelays
	retryDelays = []time.Duration{time.Millisecond, time.Millisecond}
	t.Cleanup(func() { retryDelays = old })
}

func TestForwardRetriesOn502(t *testing.T) {
	shortRetries(t)
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) < 3 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	testForwarder(t, srv.URL).forward(context.Background(), Event{Kind: "message", Mid: "m"})
	if n := calls.Load(); n != 3 {
		t.Fatalf("calls %d", n)
	}
}

func TestForwardDropsOn4xx(t *testing.T) {
	shortRetries(t)
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()
	testForwarder(t, srv.URL).forward(context.Background(), Event{Kind: "message", Mid: "m"})
	if n := calls.Load(); n != 1 {
		t.Fatalf("calls %d", n)
	}
}

func TestWithFiles(t *testing.T) {
	big := bytes.Repeat([]byte("x"), maxFileBytes+1)
	cdn := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Cookie") != "" {
			t.Errorf("cookie sent to CDN")
		}
		switch r.URL.Path {
		case "/ok.jpg":
			_, _ = w.Write([]byte("JPEG"))
		case "/video":
			http.Redirect(w, r, "/ok.jpg", http.StatusFound)
		case "/away":
			http.Redirect(w, r, "https://example.com/x", http.StatusFound)
		case "/big.mp4":
			_, _ = w.Write(big)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer cdn.Close()
	old := downloadHosts
	downloadHosts = append(downloadHosts, "127.0.0.1")
	t.Cleanup(func() { downloadHosts = old })
	f := testForwarder(t, "")
	e := f.withFiles(context.Background(), Event{Kind: "message", Attachments: []Attachment{
		{Kind: "image", URL: cdn.URL + "/ok.jpg"},
		{Kind: "video", URL: cdn.URL + "/big.mp4"},
		{Kind: "file", URL: cdn.URL + "/gone"},
		{Kind: "sticker", StickerID: likeSticker, URL: cdn.URL + "/like"},
		{Kind: "link", URL: "https://vetgo.tech"},
		{Kind: "video", URL: cdn.URL + "/video"},
		{Kind: "video", URL: cdn.URL + "/away"},
	}})
	a := e.Attachments
	if string(a[0].Data) != "JPEG" || a[1].Error != "file quá 20 MiB" || a[2].Error != "HTTP 404" || a[3].Data != nil || a[4].Data != nil ||
		string(a[5].Data) != "JPEG" || a[6].Error != "lỗi mạng" {
		t.Fatalf("got %+v", a)
	}
	// data đi dưới dạng base64 trong JSON, như extension gửi.
	b, _ := json.Marshal(a[0])
	if !bytes.Contains(b, []byte(`"data":"SlBFRw=="`)) {
		t.Fatalf("json %s", b)
	}
}
