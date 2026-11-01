package main

// Cùng mã TC với plan/bridge-web/test-case (bản JS), chạy trên cùng fixture frame thật.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

const testPageID = "109282408574091"

type fixtureFrame struct {
	T     int64   `json:"t"`
	Calls [][]any `json:"calls"`
}

func loadFixture(t *testing.T, name string) []fixtureFrame {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "bridge-web", "fixtures", name))
	if err != nil {
		t.Fatal(err)
	}
	var frames []fixtureFrame
	if err := decodeJSON(data, &frames); err != nil {
		t.Fatal(err)
	}
	return frames
}

func toCalls(raw [][]any) []call {
	out := make([]call, 0, len(raw))
	for _, c := range raw {
		out = append(out, call{Name: c[0].(string), Args: c[1:]})
	}
	return out
}

// replay chạy mọi frame như một phiên: "hiện tại" là lúc frame đến, danh bạ dùng chung, khử trùng.
func replay(t *testing.T, name string) []Event {
	contacts := map[string]string{}
	seen := newDedupe(2000)
	var events []Event
	for _, f := range loadFixture(t, name) {
		for _, e := range parseCalls(toCalls(f.Calls), testPageID, f.T-upsertFreshMs, contacts) {
			if seen.add(eventKey(e)) {
				events = append(events, e)
			}
		}
	}
	return events
}

func messageByMid(events []Event, mid string) *Event {
	for i := range events {
		if events[i].Kind == "message" && events[i].Mid == mid {
			return &events[i]
		}
	}
	return nil
}

func mustMessage(t *testing.T, events []Event, mid string) Event {
	t.Helper()
	m := messageByMid(events, mid)
	if m == nil {
		t.Fatalf("no message %s", mid)
	}
	return *m
}

func i64(n int64) *int64 { return &n }

func TestExistingThread(t *testing.T) {
	events := replay(t, "frames-2026-10-31.json")

	t.Run("TC-01 tin chữ", func(t *testing.T) {
		m := mustMessage(t, events, "mid.$cAAA5nOZNOHanwiaX5WhkOFOkWzUU")
		if m.Text != "sua-tin" || m.ThreadID != "100024107767037" || m.SenderID != "100024107767037" || len(m.Attachments) != 0 {
			t.Fatalf("got %+v", m)
		}
	})
	t.Run("TC-02 một ảnh", func(t *testing.T) {
		a := mustMessage(t, events, "mid.$cAAA5nOZNOHanwiANImhkNrBJn_Ua").Attachments[0]
		if a.Kind != "image" || a.MimeType != "image/jpeg" || a.Filename != "image-29564380916583879" || *a.Size != 195964 {
			t.Fatalf("got %+v", a)
		}
	})
	t.Run("TC-03 nhiều ảnh trong một tin", func(t *testing.T) {
		list := mustMessage(t, events, "mid.$cAAA5nOZNOHanwiEH2GhkNu2kjWGV").Attachments
		if len(list) != 3 {
			t.Fatalf("got %d attachments", len(list))
		}
		for _, a := range list {
			if a.Kind != "image" {
				t.Fatalf("got %+v", a)
			}
		}
	})
	t.Run("TC-04 video", func(t *testing.T) {
		a := mustMessage(t, events, "mid.$cAAA5nOZNOHanwiHSX2hkNyHOghzr").Attachments[0]
		if a.Kind != "video" || a.MimeType != "video/mp4" || *a.DurationMs != 10000 {
			t.Fatalf("got %+v", a)
		}
	})
	t.Run("TC-05 tin thoại", func(t *testing.T) {
		a := mustMessage(t, events, "mid.$cAAA5nOZNOHanwiJYpGhkN0FgEOam").Attachments[0]
		if a.Kind != "audio" || a.MimeType != "audio/opus" || *a.DurationMs != 8600 {
			t.Fatalf("got %+v", a)
		}
	})
	t.Run("TC-06 file PDF", func(t *testing.T) {
		a := mustMessage(t, events, "mid.$cAAA5nOZNOHanwiP2hWhkN6o6DZPt").Attachments[0]
		if a.Kind != "file" || a.MimeType != "application/pdf" || a.Filename != "Infectious_Diseases_of_the_Dog_and_Cat.pdf" || a.Subtitle != "172 KiB" {
			t.Fatalf("got %+v", a)
		}
	})
	t.Run("TC-07 sticker", func(t *testing.T) {
		m := mustMessage(t, events, "mid.$cAAA5nOZNOHanwiRT3mhkN8KO6NE5")
		if m.StickerID != "1148798091806176" || len(m.Attachments) != 1 {
			t.Fatalf("got %+v", m)
		}
		a := m.Attachments[0]
		if a.Kind != "sticker" || a.StickerID != "1148798091806176" || a.Label != "Realistic portrait of Superman" {
			t.Fatalf("got %+v", a)
		}
	})
	t.Run("TC-09 link", func(t *testing.T) {
		m := mustMessage(t, events, "mid.$cAAA5nOZNOHanwiWaNGhkOBLAOs2g")
		a := m.Attachments[0]
		// URL thật lấy từ insertAttachmentCta, fixture đã che.
		if a.Kind != "link" || a.Title != "vetgo.tech" || a.URL != "<url>" {
			t.Fatalf("got %+v", a)
		}
	})
	t.Run("TC-10 trả lời tin của khách", func(t *testing.T) {
		got := mustMessage(t, events, "mid.$cAAA5nOZNOHanwiXwK2hkOCmp3Jg0").ReplyTo
		want := &ReplyTo{Mid: "mid.$cAAA5nOZNOHanweGTkWhkJxJKi2ue", Text: "hihi", SenderID: "100024107767037"}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("got %+v", got)
		}
	})
	t.Run("TC-10b trả lời tin của Trang", func(t *testing.T) {
		if r := mustMessage(t, events, "mid.$cAAA5nOZNOHanwiYLvGhkODCMG3jR").ReplyTo; r == nil || r.SenderID != testPageID {
			t.Fatalf("got %+v", r)
		}
	})
	t.Run("TC-11 thả cảm xúc", func(t *testing.T) {
		var got [][3]string
		for _, e := range events {
			if e.Kind == "reaction" {
				got = append(got, [3]string{e.Mid, e.Reaction, e.ActorID})
			}
		}
		want := [][3]string{{"mid.$cAAA5nOZNOHanwL-HL2hj3o9Xg7q3", "❤", "100024107767037"}}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("got %v", got)
		}
	})
	t.Run("lịch sử tải lại (upsertMessage cũ) không phải tin mới", func(t *testing.T) {
		if m := messageByMid(events, "mid.$cAAA5ktf3J8mnnkW5qGhbQBuKjTXa"); m != nil {
			t.Fatalf("old upsertMessage emitted")
		}
	})
	t.Run("frame gửi lặp chỉ ra một sự kiện", func(t *testing.T) {
		seen := map[string]bool{}
		for _, e := range events {
			if e.Kind == "message" {
				if seen[e.Mid] {
					t.Fatalf("duplicate %s", e.Mid)
				}
				seen[e.Mid] = true
			}
		}
	})
}

func TestNewThread(t *testing.T) {
	events := replay(t, "frames-2026-10-31-newthread.json")
	t.Run("TC-12 + TC-14 tin đầu tiên (Like) qua upsertMessage, có tên khách", func(t *testing.T) {
		first := events[0]
		if first.Kind != "message" || first.SenderName != "Huỳnh Tính Thành" || first.StickerID != likeSticker {
			t.Fatalf("got %+v", first)
		}
		if a := first.Attachments; len(a) != 1 || a[0].Kind != "sticker" || a[0].Label != "Like, thumbs up" {
			t.Fatalf("got %+v", a)
		}
	})
	t.Run("TC-08 GIF là blob type 3, mime video/mp4", func(t *testing.T) {
		for _, e := range events {
			for _, a := range e.Attachments {
				if a.Kind == "gif" {
					if a.MimeType != "video/mp4" {
						t.Fatalf("got %+v", a)
					}
					return
				}
			}
		}
		t.Fatal("no gif")
	})
	t.Run("tin cũ của Trang bị bỏ qua, tên khách giữ cho các tin sau", func(t *testing.T) {
		var kinds []string
		for _, e := range events {
			if e.Kind == "page_message" {
				continue // tin trả lời tự động, xem test dưới
			}
			k := "text"
			if len(e.Attachments) > 0 {
				k = e.Attachments[0].Kind
			}
			kinds = append(kinds, k)
			if e.SenderID == testPageID || e.SenderName != "Huỳnh Tính Thành" {
				t.Fatalf("got %+v", e)
			}
		}
		if want := []string{"sticker", "audio", "video", "gif"}; !reflect.DeepEqual(kinds, want) {
			t.Fatalf("got %v", kinds)
		}
	})
	t.Run("tin trả lời tự động của Trang (insertMessage) là page_message có otid", func(t *testing.T) {
		var page []Event
		for _, e := range events {
			if e.Kind == "page_message" {
				page = append(page, e)
			}
		}
		if len(page) != 1 || page[0].SenderID != testPageID || page[0].Otid == "" || page[0].ThreadID != "100005036221206" || page[0].Text == "" {
			t.Fatalf("got %+v", page)
		}
	})
}

func TestEditAndReactions(t *testing.T) {
	const mid = "mid.$cAAA5kHeMT52nwlJIPmhkQz-pseO6"
	events := replay(t, "frames-2026-10-31-edit.json")
	byKind := func(kind string) Event {
		for _, e := range events {
			if e.Kind == kind {
				return e
			}
		}
		t.Fatalf("no %s", kind)
		return Event{}
	}
	if got, want := byKind("edit"), (Event{Kind: "edit", Mid: mid, Text: "Tin nhắn gửi, đã chỉnh sửa", EditCount: 1}); !reflect.DeepEqual(got, want) {
		t.Errorf("TC-13 got %+v", got)
	}
	want := Event{Kind: "reaction", Mid: mid, ThreadID: "100005036221206", ActorID: "100005036221206", Reaction: "👍", TimestampMs: 1793434954603}
	if got := byKind("reaction"); !reflect.DeepEqual(got, want) {
		t.Errorf("TC-11 got %+v", got)
	}
	if got, want := byKind("unreaction"), (Event{Kind: "unreaction", Mid: mid, ThreadID: "100005036221206", ActorID: "100005036221206"}); !reflect.DeepEqual(got, want) {
		t.Errorf("TC-15 got %+v", got)
	}
}

// frameOf: envelope như dữ liệu một frame data của stream.
func frameOf(t *testing.T, step any) []byte {
	t.Helper()
	payload, _ := json.Marshal(map[string]any{"step": step})
	data, _ := json.Marshal(map[string]any{"request_id": nil, "payload": string(payload)})
	return data
}

func TestDecodeFrame(t *testing.T) {
	step := []any{1, []any{5, "editMessage", "mid.x", []any{19, "80"}, "mới", []any{19, "2"}}, []any{5, "noop", []any{9}}}
	_, calls, ok := decodeFrame(append([]byte("\r\x03\x00 "), frameOf(t, step)...))
	want := []call{{"editMessage", []any{"mid.x", "80", "mới", "2"}}, {"noop", []any{nil}}}
	if !ok || !reflect.DeepEqual(calls, want) {
		t.Fatalf("got %v %v", ok, calls)
	}
	for _, bad := range []string{"\x00\x01ping", `{"request_id":1}`, `{"payload":"không phải json"}`} {
		if _, _, ok := decodeFrame([]byte(bad)); ok {
			t.Errorf("%q decoded", bad)
		}
	}
	tree := []any{1, []any{23, []any{2, 0}, []any{1, []any{1, []any{5, "a"}}, []any{26, []any{5, "b", 1}}}}}
	_, calls, _ = decodeFrame(frameOf(t, tree))
	if len(calls) != 2 || calls[0].Name != "a" || calls[1].Name != "b" || calls[1].Args[0] != json.Number("1") {
		t.Fatalf("nested got %v", calls)
	}
}

// insertMessage tối thiểu: 0 text, 3 thread, 5 ts, 8 mid, 10 sender.
func insertMessage(proc, ts, mid, sender string) call {
	return call{proc, []any{"x", nil, "80", "42", "0", ts, ts, nil, mid, "otid", sender}}
}

func TestFilters(t *testing.T) {
	// Tin Trang gửi: insertMessage thành page_message (ghi chú cho nhân viên, nhận ra tin
	// fbweb-ls tự gửi theo otid); upsertMessage của Trang là lịch sử, bỏ.
	ev := parseCalls([]call{insertMessage("insertMessage", "1000", "mid.a", testPageID)}, testPageID, 0, map[string]string{})
	if len(ev) != 1 || ev[0].Kind != "page_message" || ev[0].Otid != "otid" || ev[0].Mid != "mid.a" {
		t.Errorf("TC-17 page message: %+v", ev)
	}
	if ev := parseCalls([]call{insertMessage("upsertMessage", "1000", "mid.a", testPageID)}, testPageID, 0, map[string]string{}); len(ev) != 0 {
		t.Errorf("page upsert emitted: %+v", ev)
	}
	page := []call{
		{"upsertReaction", []any{"t", "1", "mid.a", testPageID, "❤", "80"}},
		{"deleteReaction", []any{"t", "mid.a", testPageID}},
	}
	if ev := parseCalls(page, testPageID, 0, map[string]string{}); len(ev) != 0 {
		t.Errorf("page reaction emitted: %+v", ev)
	}
	const ts = 10_000_000
	up := []call{insertMessage("upsertMessage", "10000000", "mid.b", "42")}
	if n := len(parseCalls(up, testPageID, ts, map[string]string{})); n != 1 {
		t.Errorf("fresh upsert: %d events", n)
	}
	if n := len(parseCalls(up, testPageID, ts+1, map[string]string{})); n != 0 {
		t.Errorf("stale upsert: %d events", n)
	}
}

func TestEventKey(t *testing.T) {
	a := Event{Kind: "reaction", Mid: "m", ActorID: "u", Reaction: "❤", TimestampMs: 5}
	b := a
	b.Reaction = "👍"
	if eventKey(a) == eventKey(b) {
		t.Error("different reactions share a key")
	}
	if eventKey(Event{Kind: "edit", Mid: "m", EditCount: 1}) == eventKey(Event{Kind: "edit", Mid: "m", EditCount: 2}) {
		t.Error("different edits share a key")
	}
}

func TestSyncCursor(t *testing.T) {
	calls := []call{
		{"executeFirstBlockForSyncTransactionV4", []any{"2", "0", nil, "other"}},
		{"executeFirstBlockForSyncTransactionV4", []any{"205", "0", "old", "new"}},
	}
	if c, ok := syncCursor(calls, "205"); !ok || c != "new" {
		t.Fatalf("got %q %v", c, ok)
	}
	if _, ok := syncCursor(calls[:1], "205"); ok {
		t.Fatal("cursor of another database")
	}
}
