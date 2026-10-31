package main

// Bản Go của parseCalls trong plan/bridge-web/bs-inbox-listener.js: biến lời gọi
// stored procedure Lightspeed thành sự kiện cho fb-bridge-web. Chỉ số tham số
// lấy từ bridge-e2ee/meta/pkg/messagix/table, đã đối chiếu với frame thật
// (bảng trong plan/bridge-web/README.md). Sửa ở đây thì sửa cả bản JS.

import (
	"bytes"
	"encoding/json"
	"strconv"
	"strings"
)

// call là một [5, "tênProcedure", ...tham số]; tham số là string, nil, bool hoặc json.Number.
type call struct {
	Name string
	Args []any
}

// [19,"123"] là int64, [9] là null; còn lại giữ nguyên.
func decodeVal(v any) any {
	if arr, ok := v.([]any); ok && len(arr) > 0 {
		switch op, _ := arr[0].(json.Number); op {
		case "19":
			if len(arr) > 1 {
				return arr[1]
			}
		case "9":
			return nil
		}
	}
	return v
}

// collectCalls gom mọi lời gọi [5, tên, ...] trong cây bytecode.
func collectCalls(node any, out []call) []call {
	arr, ok := node.([]any)
	if !ok {
		return out
	}
	if len(arr) >= 2 {
		if op, _ := arr[0].(json.Number); op == "5" {
			if name, ok := arr[1].(string); ok {
				args := make([]any, len(arr)-2)
				for i, a := range arr[2:] {
					args[i] = decodeVal(a)
				}
				return append(out, call{Name: name, Args: args})
			}
		}
	}
	for _, child := range arr {
		out = collectCalls(child, out)
	}
	return out
}

func decodeJSON(data []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	return dec.Decode(v)
}

// decodeStep giải mã chuỗi payload {"name":null,"step":[...]}.
func decodeStep(payload string) ([]call, error) {
	var p struct {
		Step any `json:"step"`
	}
	if err := decodeJSON([]byte(payload), &p); err != nil {
		return nil, err
	}
	return collectCalls(p.Step, nil), nil
}

// envelope là dữ liệu một frame data của stream Lightspeed.
type envelope struct {
	RequestID *int64   `json:"request_id"`
	Payload   string   `json:"payload"`
	Sp        []string `json:"sp"`
}

// decodeFrame bỏ phần header nhị phân (nếu có) rồi giải mã envelope và payload.
func decodeFrame(data []byte) (envelope, []call, bool) {
	var env envelope
	start, end := bytes.IndexByte(data, '{'), bytes.LastIndexByte(data, '}')
	if start < 0 || end <= start {
		return env, nil, false
	}
	if err := json.Unmarshal(data[start:end+1], &env); err != nil || env.Payload == "" {
		return env, nil, false
	}
	calls, err := decodeStep(env.Payload)
	if err != nil {
		return env, nil, false
	}
	return env, calls, true
}

func arg(a []any, i int) any {
	if i < len(a) {
		return a[i]
	}
	return nil
}

// str: chuỗi của tham số i, "" nếu null hoặc không có.
func str(a []any, i int) string {
	switch v := arg(a, i).(type) {
	case string:
		return v
	case json.Number:
		return v.String()
	}
	return ""
}

// num: Number(a[i]) của bản JS; 0 nếu không đọc được.
func num(a []any, i int) int64 {
	s := str(a, i)
	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		return n
	}
	if f, err := strconv.ParseFloat(s, 64); err == nil {
		return int64(f)
	}
	return 0
}

func numPtr(a []any, i int) *int64 {
	if n := num(a, i); n != 0 {
		return &n
	}
	return nil
}

// AttachmentType của Lightspeed (messagix/table/enums.go).
var blobKinds = map[string]string{"1": "sticker", "2": "image", "3": "gif", "4": "video", "5": "audio", "6": "file"}

// upsertMessage vừa là tin đầu tiên của khách mới, vừa là lịch sử tải lại khi
// mở hội thoại; chỉ coi là tin mới nếu đủ gần hiện tại.
const upsertFreshMs = 2 * 60 * 1000

type Attachment struct {
	Kind       string `json:"kind"`
	ID         string `json:"id,omitempty"`
	MimeType   string `json:"mimeType,omitempty"`
	Filename   string `json:"filename,omitempty"`
	Title      string `json:"title,omitempty"`
	Subtitle   string `json:"subtitle,omitempty"`
	Size       *int64 `json:"size,omitempty"`
	URL        string `json:"url,omitempty"`
	PreviewURL string `json:"previewUrl,omitempty"`
	DurationMs *int64 `json:"durationMs,omitempty"`
	StickerID  string `json:"stickerId,omitempty"`
	Label      string `json:"label,omitempty"`
	Data       []byte `json:"data,omitempty"`  // base64 trong JSON, file đã tải
	Error      string `json:"error,omitempty"` // không tải được
}

type ReplyTo struct {
	Mid      string `json:"mid"`
	Text     string `json:"text"`
	SenderID string `json:"senderId"`
}

// Event cùng dạng với sự kiện [BS-INBOX] của bản JS (ingestEvent của fb-bridge-web).
type Event struct {
	Kind        string       `json:"kind"` // message | reaction | unreaction | edit
	Mid         string       `json:"mid"`
	ThreadID    string       `json:"threadId,omitempty"`
	SenderID    string       `json:"senderId,omitempty"`
	SenderName  string       `json:"senderName,omitempty"`
	Text        string       `json:"text,omitempty"`
	TimestampMs int64        `json:"timestampMs,omitempty"`
	StickerID   string       `json:"stickerId,omitempty"`
	ReplyTo     *ReplyTo     `json:"replyTo,omitempty"`
	Attachments []Attachment `json:"attachments,omitempty"`
	ActorID     string       `json:"actorId,omitempty"`
	Reaction    string       `json:"reaction,omitempty"`
	EditCount   int64        `json:"editCount,omitempty"`
}

// updateContacts ghi tên từ các procedure contact (id -> tên).
func updateContacts(calls []call, contacts map[string]string) {
	for _, c := range calls {
		switch c.Name {
		case "verifyContactRowExists":
			if id, name := str(c.Args, 0), str(c.Args, 3); id != "" && name != "" {
				contacts[id] = name
			}
		case "deleteThenInsertContact":
			if id, name := str(c.Args, 0), str(c.Args, 9); id != "" && name != "" {
				contacts[id] = name
			}
		}
	}
}

// parseCalls biến lời gọi của một frame thành sự kiện; contacts được cập nhật
// từ frame và dùng để điền senderName. upsertMessage chỉ được nhận nếu thời điểm
// tin >= minUpsertMs (bản JS: nowMs - upsertFreshMs).
func parseCalls(calls []call, pageID string, minUpsertMs int64, contacts map[string]string) []Event {
	updateContacts(calls, contacts)
	attachments := map[string][]Attachment{}
	ctaURLs := map[string]string{}
	for _, c := range calls {
		a := c.Args
		switch c.Name {
		case "insertBlobAttachment":
			kind := blobKinds[str(a, 29)]
			if kind == "" {
				kind = "file"
			}
			mime := str(a, 30)
			if mime == "" {
				mime = str(a, 6)
			}
			attachments[str(a, 32)] = append(attachments[str(a, 32)], Attachment{
				Kind: kind, MimeType: mime, Filename: str(a, 0), Size: numPtr(a, 1),
				URL: str(a, 3), PreviewURL: str(a, 8), DurationMs: numPtr(a, 22),
			})
		case "insertXmaAttachment":
			// File dạng tài liệu có playableUrl; link chia sẻ thì không (URL nằm ở CTA).
			att := Attachment{Kind: "link", ID: str(a, 32), MimeType: str(a, 7), Title: str(a, 58), Subtitle: str(a, 59)}
			if u := str(a, 4); u != "" {
				att.Kind, att.Filename, att.URL = "file", str(a, 58), u
			}
			attachments[str(a, 30)] = append(attachments[str(a, 30)], att)
		case "insertAttachmentCta":
			if str(a, 7) == "xma_web_url" && str(a, 9) != "" {
				ctaURLs[str(a, 1)] = str(a, 9)
			}
		case "insertStickerAttachment":
			attachments[str(a, 18)] = append(attachments[str(a, 18)], Attachment{
				Kind: "sticker", StickerID: str(a, 19), Label: str(a, 13), URL: str(a, 4),
			})
		}
	}
	for _, list := range attachments {
		for i := range list {
			if list[i].Kind == "link" {
				list[i].URL = ctaURLs[list[i].ID]
			}
		}
	}

	var events []Event
	for _, c := range calls {
		a := c.Args
		switch c.Name {
		case "insertMessage", "upsertMessage":
			sender := str(a, 10)
			if pageID != "" && sender == pageID {
				continue // tin Trang tự gửi
			}
			ts := num(a, 5)
			if c.Name == "upsertMessage" && ts < minUpsertMs {
				continue
			}
			ev := Event{
				Kind: "message", Mid: str(a, 8), ThreadID: str(a, 3), SenderID: sender,
				SenderName: contacts[sender], Text: str(a, 0), TimestampMs: ts, StickerID: str(a, 11),
				Attachments: attachments[str(a, 8)],
			}
			if r := str(a, 23); r != "" {
				ev.ReplyTo = &ReplyTo{Mid: r, Text: str(a, 28), SenderID: str(a, 29)}
			}
			events = append(events, ev)
		case "upsertReaction":
			if pageID != "" && str(a, 3) == pageID {
				continue
			}
			// a[1] là thời điểm của tin được thả; a[6] (nếu có) là lúc thả cảm xúc.
			ts := num(a, 6)
			if ts == 0 {
				ts = num(a, 1)
			}
			events = append(events, Event{Kind: "reaction", Mid: str(a, 2), ThreadID: str(a, 0),
				ActorID: str(a, 3), Reaction: str(a, 4), TimestampMs: ts})
		case "deleteReaction":
			if pageID != "" && str(a, 2) == pageID {
				continue
			}
			events = append(events, Event{Kind: "unreaction", Mid: str(a, 1), ThreadID: str(a, 0), ActorID: str(a, 2)})
		case "editMessage":
			events = append(events, Event{Kind: "edit", Mid: str(a, 0), Text: str(a, 2), EditCount: num(a, 3)})
		}
	}
	return events
}

// eventKey: Facebook gửi lặp cùng một sự kiện vài lần; mỗi khoá chỉ phát một lần.
func eventKey(e Event) string {
	switch e.Kind {
	case "message":
		return e.Mid
	case "edit":
		return "edit:" + e.Mid + ":" + strconv.FormatInt(e.EditCount, 10)
	}
	ts := ""
	if e.TimestampMs != 0 {
		ts = strconv.FormatInt(e.TimestampMs, 10)
	}
	return strings.Join([]string{e.Kind, e.Mid, e.ActorID, e.Reaction, ts}, ":")
}

// syncBlock: khối executeFirstBlockForSyncTransactionV4 cuối của database
// (4 currentSeqId, 5 syncStatus, 6 sendSyncParams, 9 syncChannel; messagix/table/sync_groups.go).
func syncBlock(calls []call, database string) (call, bool) {
	var found call
	ok := false
	for _, c := range calls {
		if c.Name == "executeFirstBlockForSyncTransactionV4" && str(c.Args, 0) == database {
			found, ok = c, true
		}
	}
	return found, ok
}

// syncCursor lấy nextCursor mới nhất của database từ executeFirstBlockForSyncTransactionV4
// (0 database, 2 currentCursor, 3 nextCursor).
func syncCursor(calls []call, database string) (string, bool) {
	cursor, found := "", false
	for _, c := range calls {
		if c.Name == "executeFirstBlockForSyncTransactionV4" && str(c.Args, 0) == database {
			if next := str(c.Args, 3); next != "" {
				cursor, found = next, true
			}
		}
	}
	return cursor, found
}
