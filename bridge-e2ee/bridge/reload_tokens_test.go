package bridge

import (
	"errors"
	"fmt"
	"testing"

	"go.mau.fi/mautrix-meta/pkg/messagix/types"
)

// Facebook's "please reload the page" as the upload returns it (wrapped, like mercury.go).
var reloadErr = fmt.Errorf("error in mercury upload: %w", &types.ErrorResponse{ErrorCode: 1357004, ErrorSummary: "Vui lòng thử đóng và mở lại cửa sổ trình duyệt của bạn."})

func TestRetryOnReloadRefreshesOnceAndUploadsAgain(t *testing.T) {
	uploads, refreshes := 0, 0
	v, err := retryOnReload(
		func() (int, error) {
			uploads++
			if uploads == 1 {
				return 0, reloadErr
			}
			return 42, nil
		},
		func() error { refreshes++; return nil },
	)
	if err != nil || v != 42 || uploads != 2 || refreshes != 1 {
		t.Fatalf("v=%d err=%v uploads=%d refreshes=%d", v, err, uploads, refreshes)
	}
}

func TestRetryOnReloadLeavesOtherErrorsAlone(t *testing.T) {
	other := errors.New("network down")
	refreshes := 0
	_, err := retryOnReload(func() (int, error) { return 0, other }, func() error { refreshes++; return nil })
	if !errors.Is(err, other) || refreshes != 0 {
		t.Fatalf("err=%v refreshes=%d", err, refreshes)
	}
	v, err := retryOnReload(func() (int, error) { return 7, nil }, func() error { refreshes++; return nil })
	if err != nil || v != 7 || refreshes != 0 {
		t.Fatalf("v=%d err=%v refreshes=%d", v, err, refreshes)
	}
}

func TestRetryOnReloadReportsAFailedRefreshAndDoesNotRetry(t *testing.T) {
	uploads := 0
	_, err := retryOnReload(func() (int, error) { uploads++; return 0, reloadErr }, func() error { return errors.New("page load failed") })
	if !errors.Is(err, types.ErrPleaseReloadPage) || uploads != 1 {
		t.Fatalf("err=%v uploads=%d", err, uploads)
	}
}

func TestRetryOnReloadGivesUpAfterOneRetry(t *testing.T) {
	uploads, refreshes := 0, 0
	_, err := retryOnReload(func() (int, error) { uploads++; return 0, reloadErr }, func() error { refreshes++; return nil })
	if !errors.Is(err, types.ErrPleaseReloadPage) || uploads != 2 || refreshes != 1 {
		t.Fatalf("err=%v uploads=%d refreshes=%d", err, uploads, refreshes)
	}
}
