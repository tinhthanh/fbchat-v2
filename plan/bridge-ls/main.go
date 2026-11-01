// fbweb-ls: nghe tin Fanpage thẳng trên socket Lightspeed của Business Suite,
// không cần trình duyệt, rồi chuyển sang fb-bridge-web của omnichannel như
// extension của fbweb-browser. Dùng phiên (cookie) đã đăng nhập trong
// fbweb-browser. Xem README.md.
//
//	fbweb-ls          chạy một Trang (env FBWEB_PAGE_ID)
//	fbweb-ls agent    nhiều phiên, nhiều Trang, cấu hình từ fb-bridge-web (agent.go)
//	fbweb-ls check    chỉ tải trang Hộp thư một lần, in những gì đọc được (không mở socket)
//	fbweb-ls pages    liệt kê các Trang của phiên (id, tên), không mở socket
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/imroc/req/v3"
)

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func main() {
	level := slog.LevelInfo
	if os.Getenv("FBLS_DEBUG") == "1" {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: level}))
	if err := run(log); err != nil {
		log.Error("fbweb-ls stopped", "error", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	if len(os.Args) > 1 && os.Args[1] == "agent" {
		return runAgent(log)
	}
	cfg := Config{
		PageID:    os.Getenv("FBWEB_PAGE_ID"),
		PageName:  os.Getenv("FBLS_PAGE_NAME"),
		DataDir:   env("FBLS_DATA", "/data"),
		StateFile: env("FBLS_STATE", "state.json"),
		Send:      os.Getenv("FBLS_SEND") == "1",
		UserAgent: defaultUserAgent,
	}
	if ua := os.Getenv("FBLS_USER_AGENT"); ua != "" {
		cfg.UserAgent.UA = ua
	}
	if cfg.PageID == "" {
		return fmt.Errorf("FBWEB_PAGE_ID is required")
	}
	jar, err := OpenCookieJar(filepath.Join(cfg.DataDir, "cookies.json"))
	if err != nil {
		return err
	}
	httpClient := newHTTPClient()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if len(os.Args) > 1 && os.Args[1] == "check" {
		return check(ctx, httpClient, jar, cfg)
	}
	if len(os.Args) > 1 && os.Args[1] == "pages" {
		pages, err := listPages(ctx, httpClient, newDownloadClient(), jar, cfg.UserAgent, nil)
		if err != nil {
			return err
		}
		for _, p := range pages {
			fmt.Printf("%s\t%s\n", p.ID, p.Name)
		}
		return nil
	}

	l := NewListener(cfg, jar, httpClient, log)
	fwd := Forwarder{
		URL:      env("FBWEB_URL", "http://host.docker.internal:7008"),
		Token:    os.Getenv("FBWEB_TOKEN"),
		Account:  os.Getenv("FBWEB_ACCOUNT"),
		PageID:   cfg.PageID,
		PageName: l.PageName,
		DryRun:   os.Getenv("FBLS_DRY_RUN") == "1",
	}
	if !fwd.DryRun && (len(fwd.Token) < 32 || fwd.Account == "") {
		return fmt.Errorf("FBWEB_TOKEN (32+ characters) and FBWEB_ACCOUNT are required unless FBLS_DRY_RUN=1")
	}
	l.fwd = NewForwarder(fwd, newDownloadClient(), jar, cfg.UserAgent, log)
	log.Info("fbweb-ls started", "page", cfg.PageID, "dry_run", fwd.DryRun, "send", cfg.Send && !fwd.DryRun)
	l.Run(ctx)
	return nil
}

// runAgent: nhiều phiên, nhiều Trang, cấu hình từ fb-bridge-web (agent.go).
func runAgent(log *slog.Logger) error {
	cfg := AgentConfig{
		Name:      os.Getenv("FBLS_AGENT"),
		LoginsDir: env("FBLS_LOGINS", "/logins"),
		DataDir:   env("FBLS_DATA", "/data"),
		URL:       env("FBWEB_URL", "http://fb-bridge-web:7008"),
		Token:     os.Getenv("FBWEB_TOKEN"),
		UserAgent: defaultUserAgent,
	}
	if ua := os.Getenv("FBLS_USER_AGENT"); ua != "" {
		cfg.UserAgent.UA = ua
	}
	if !loginNamePattern.MatchString(cfg.Name) {
		return fmt.Errorf("FBLS_AGENT is required (letters, digits, . _ -)")
	}
	if len(cfg.Token) < 32 {
		return fmt.Errorf("FBWEB_TOKEN (32+ characters) is required")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	log.Info("fbweb-ls agent started", "agent", cfg.Name)
	NewAgent(cfg, log).Run(ctx)
	return nil
}

// check in độ dài hoặc so khớp, không in giá trị (cursor và id là dữ liệu phiên).
func check(ctx context.Context, httpClient *req.Client, jar *CookieJar, cfg Config) error {
	info, err := fetchPage(ctx, httpClient, jar, cfg.UserAgent, cfg.PageID)
	if err != nil {
		return err
	}
	fmt.Printf("appId: %d chữ số\n", len(info.AppID))
	fmt.Printf("USER_ID khớp c_user: %v\n", info.UserID == jar.Get("c_user"))
	fmt.Printf("version: %d chữ số\n", len(info.Version))
	fmt.Printf("cursor db %s: %d ký tự\n", lsDatabase, len(info.Cursor))
	fmt.Printf("region: %s\n", info.RegionHint)
	fmt.Printf("fb_dtsg: %d ký tự, lsd: %d ký tự\n", len(info.DTSG), len(info.LSD))
	fmt.Printf("contacts: %d, có tên Trang: %v\n", len(info.Contacts), info.Contacts[cfg.PageID] != "")
	return nil
}
