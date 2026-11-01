package main

// Thả, đổi, gỡ cảm xúc dưới danh nghĩa Trang (bắt frame 01/11/2026): task label 29
// trên hàng đợi "reaction:<mid>" {thread_key, timestamp_ms, message_id, actor_id =
// id Trang, reaction, reaction_style null, sync_group 205, send_attribution 4456448,
// ...null}. Server trả replaceOptimisticReaction. Gỡ: reaction "" như mautrix-meta
// (Business Suite chưa bắt được frame gỡ).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"go.mau.fi/mautrix-meta/pkg/messagix/dgw"
)

const (
	reactTaskLabel = "29"
	likeStickerID  = 369239263222822 // nút Like của Messenger (sticker)
)

// outTask: một task 46 (tin chữ, tệp hoặc sticker); otid để đối chiếu xác nhận.
type outTask interface{ otid() string }

func (t *sendTask) otid() string { return t.Otid }

// stickerTask: như trang gửi sticker, không có khoá text.
type stickerTask struct {
	ThreadID       int64       `json:"thread_id"`
	Otid           string      `json:"otid"`
	Source         int64       `json:"source"`
	SendType       int         `json:"send_type"`
	SyncGroup      json.Number `json:"sync_group"`
	MarkThreadRead int         `json:"mark_thread_read"`
	StickerID      int64       `json:"sticker_id"`
}

func (t *stickerTask) otid() string { return t.Otid }

func taskPayloads(tasks []outTask) []any {
	out := make([]any, len(tasks))
	for i, t := range tasks {
		out[i] = t
	}
	return out
}

type reactTask struct {
	ThreadKey          int64       `json:"thread_key"`
	TimestampMs        int64       `json:"timestamp_ms"`
	MessageID          string      `json:"message_id"`
	ActorID            int64       `json:"actor_id"`
	Reaction           string      `json:"reaction"`
	ReactionStyle      *string     `json:"reaction_style"`
	SyncGroup          json.Number `json:"sync_group"`
	SendAttribution    int64       `json:"send_attribution"`
	DataclassParams    *string     `json:"dataclass_params"`
	AttachmentFBID     *string     `json:"attachment_fbid"`
	OfflineThreadingID *string     `json:"offline_threading_id"`
}

func (l *Listener) reactJob(ctx context.Context, j outJob) outResult {
	res := outResult{ID: j.ID}
	thread, err1 := strconv.ParseInt(j.ThreadID, 10, 64)
	page, err2 := strconv.ParseInt(l.cfg.PageID, 10, 64)
	if err1 != nil || err2 != nil || thread <= 0 || !strings.HasPrefix(j.MessageID, "mid.") {
		res.Error = "Yêu cầu thả cảm xúc không hợp lệ."
		return res
	}
	l.mu.Lock()
	sock := l.sock
	l.mu.Unlock()
	if sock == nil || !l.connected.Load() || !l.caughtUp.Load() {
		res.Error, res.Retry = "fbweb-ls chưa kết nối Facebook.", true
		return res
	}
	t := reactTask{ThreadKey: thread, TimestampMs: time.Now().UnixMilli(), MessageID: j.MessageID, ActorID: page,
		Reaction: j.Emoji, SyncGroup: json.Number(lsDatabase), SendAttribution: sendSource}
	payload, err := l.sendRequest(reactTaskLabel, "reaction:"+j.MessageID, []any{t})
	if err != nil {
		res.Error = "Không tạo được yêu cầu thả cảm xúc."
		return res
	}
	sctx, cancel := context.WithTimeout(ctx, sendTimeout)
	defer cancel()
	resp, err := sock.DoOneOffStream(sctx, payload, false)
	if err != nil {
		if errors.Is(err, dgw.ErrSocketNotOpen) {
			res.Error, res.Retry = "fbweb-ls mất kết nối Facebook trước khi gửi.", true
			return res
		}
		l.log.Warn("react stream failed", "job", j.ID, "error", err)
		res.Error, res.Unknown = "Không rõ cảm xúc đã tới khách chưa (Facebook không trả lời).", true
		return res
	}
	_, calls, _ := decodeFrame(resp)
	for _, c := range calls {
		if c.Name == "replaceOptimisticReaction" {
			res.OK = true
			return res
		}
	}
	res.Error, res.Unknown = fmt.Sprintf("Facebook không xác nhận cảm xúc (%s).", procedureNames(calls)), true
	return res
}
