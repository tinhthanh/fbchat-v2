package bridge

import (
	"context"
	"errors"
	"fmt"
	"time"

	"go.mau.fi/mautrix-meta/pkg/messagix/httpclient"
	"go.mau.fi/mautrix-meta/pkg/messagix/types"
)

// Facebook answers a media upload (mercury) with 1357004, "please reload the page"
// (types.ErrPleaseReloadPage), once the page tokens the bridge loaded at connect time
// (fb_dtsg, lsd) are too old: seen from the third day after fb-bot started (canary,
// omnichannel STATUS 28/11/2026), while texts over the socket keep working.
// mautrix-meta does a full reconnect and uploads again; the bridge cannot (Disconnect is
// final and the socket and E2EE sessions should stay up), so it reloads only the page
// tokens the HTTP client uses and uploads once more.

// tokenRefreshMinInterval: uploads failing together (a reply with several files) reload the
// page once; the others reuse the fresh tokens.
const tokenRefreshMinInterval = time.Minute

// retryOnReload runs upload, and when Facebook asks to reload the page, refreshes the tokens
// and runs it once more. Any other error is returned as is.
func retryOnReload[T any](upload func() (T, error), refresh func() error) (T, error) {
	v, err := upload()
	if !errors.Is(err, types.ErrPleaseReloadPage) {
		return v, err
	}
	if rerr := refresh(); rerr != nil {
		return v, fmt.Errorf("%w (reloading the page tokens failed: %v)", err, rerr)
	}
	return upload()
}

// refreshHTTPTokens loads the messages page again into fresh configs and gives them to the
// HTTP client. The socket and its sync state keep the configs they have.
func (c *Client) refreshHTTPTokens(ctx context.Context) error {
	c.tokenRefreshMu.Lock()
	defer c.tokenRefreshMu.Unlock()
	if !c.tokensRefreshedAt.IsZero() && time.Since(c.tokensRefreshedAt) < tokenRefreshMinInterval {
		return nil
	}
	if c.Messagix == nil {
		return errors.New("messenger client not set up")
	}
	hc := c.Messagix.GetHTTP()
	cfg := httpclient.NewConfigs(c.Messagix)
	if err := httpclient.NewModuleParser(c.Messagix, hc, cfg).Load(ctx, c.Messagix.GetEndpoint("messages")); err != nil {
		return fmt.Errorf("load messages page: %w", err)
	}
	if cfg.BrowserConfigTable == nil || cfg.BrowserConfigTable.DTSGInitData.Token == "" {
		return errors.New("messages page has no fb_dtsg")
	}
	cfg.Setup(true)
	hc.SetConfigs(cfg)
	c.tokensRefreshedAt = time.Now()
	c.Logger.Info().Msg("Reloaded the page tokens for HTTP after Facebook asked to reload the page")
	return nil
}

// mercuryUpload uploads media, reloading the page tokens once if Facebook asks to.
func (c *Client) mercuryUpload(threadID int64, media *httpclient.MercuryUploadMedia) (*types.MercuryUploadResponse, error) {
	return retryOnReload(
		func() (*types.MercuryUploadResponse, error) {
			return c.Messagix.GetHTTP().SendMercuryUploadRequest(c.ctx, threadID, media)
		},
		func() error { return c.refreshHTTPTokens(c.ctx) },
	)
}
