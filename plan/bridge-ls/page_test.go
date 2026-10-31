package main

// HTML giả dựng theo cấu trúc trang Hộp thư thật (đối chiếu 31/10/2026): config
// define, biến preload là chuỗi JSON, kết quả preload là payload Lightspeed lồng.

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func jsonString(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func scriptTag(t *testing.T, v any) string {
	return `<script type="application/json" data-content-len="1" data-sjs>` + jsonString(t, v) + `</script>`
}

func fakeInboxHTML(t *testing.T, userID string, withCursor bool) []byte {
	defines := map[string]any{"require": []any{[]any{"ScheduledServerJS", "handle", nil, []any{map[string]any{"__bbox": map[string]any{"define": []any{
		[]any{"DGWWebConfig", []any{}, map[string]any{"appId": "111222333444555", "appVersion": "0", "dgwVersion": "2", "endpoint": "", "fbId": "1", "authType": ""}, 5508},
		[]any{"CurrentUserInitialData", []any{}, map[string]any{"ACCOUNT_ID": userID, "USER_ID": userID, "NAME": "Người dùng"}, 58},
	}}}}}}}
	syncParams := jsonString(t, map[string]string{"locale": "vi_VN", "bloks_version": "abc"})
	preload := func(db int) map[string]any {
		return map[string]any{"variables": map[string]any{
			"requestPayload": jsonString(t, map[string]any{"database": db, "epoch_id": 0, "last_applied_cursor": nil, "sync_params": syncParams, "version": json.Number("9876543210123456")}),
			"requestType":    1, "pageAssetId": testPageID,
		}}
	}
	first := []any{5, "executeFirstBlockForSyncTransactionV4", []any{19, "205"}, []any{19, "0"}, []any{9}, "HCwRVtCURSOR", []any{19, "11113"}}
	steps := []any{1,
		[]any{5, "verifyContactRowExists", []any{19, testPageID}, []any{19, "0"}, nil, "VetGo"},
		[]any{5, "verifyContactRowExists", []any{19, "100024107767037"}, []any{19, "0"}, nil, "Khách"},
	}
	if withCursor {
		steps = append(steps, []any{1, []any{4, 0, 1, first}})
	}
	payload := jsonString(t, map[string]any{"name": nil, "step": steps})
	regionPayload := jsonString(t, map[string]any{"name": nil, "step": []any{1, []any{5, "setRegionHint", []any{19, "2"}, "PRN"}}})
	relay := func(p string) map[string]any {
		return map[string]any{"require": []any{[]any{"RelayPrefetchedStreamCache@8ca74e", "next", []any{}, []any{"adp_x",
			map[string]any{"__bbox": map[string]any{"complete": true, "result": map[string]any{"data": map[string]any{"viewer": map[string]any{
				"lightspeed_web_request_for_page": map[string]any{"payload": p, "dependencies": []any{}}}}}}}}}}}
	}
	return []byte("<html><head>" +
		`<script type="application/json">{"unrelated":true}</script>` +
		scriptTag(t, defines) +
		scriptTag(t, map[string]any{"a": preload(2), "b": preload(205)}) +
		scriptTag(t, relay(regionPayload)) +
		scriptTag(t, relay(payload)) +
		"</head><body></body></html>")
}

func TestParsePage(t *testing.T) {
	info, err := parsePage(fakeInboxHTML(t, "100012345678901", true))
	if err != nil {
		t.Fatal(err)
	}
	if info.AppID != "111222333444555" || info.UserID != "100012345678901" || info.Version != "9876543210123456" ||
		info.Cursor != "HCwRVtCURSOR" || info.RegionHint != "PRN" {
		t.Fatalf("got %+v", info)
	}
	if info.SyncParams != `{"bloks_version":"abc","locale":"vi_VN"}` {
		t.Fatalf("sync params %q", info.SyncParams)
	}
	if info.Contacts[testPageID] != "VetGo" || info.Contacts["100024107767037"] != "Khách" {
		t.Fatalf("contacts %v", info.Contacts)
	}
}

func TestParsePageLoggedOut(t *testing.T) {
	if _, err := parsePage(fakeInboxHTML(t, "0", true)); !errors.Is(err, errLoggedOut) {
		t.Fatalf("got %v", err)
	}
}

func TestParsePageMissingCursor(t *testing.T) {
	_, err := parsePage(fakeInboxHTML(t, "100012345678901", false))
	if err == nil || !strings.Contains(err.Error(), "cursor") {
		t.Fatalf("got %v", err)
	}
	// Chỉ tên trường, không có giá trị.
	if strings.Contains(err.Error(), "111222333444555") {
		t.Fatalf("error leaks a value: %v", err)
	}
}
