package bridge

import (
	"fmt"
	"strings"

	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
)

// EventTypeE2EESenderKey reports what happened to group sender keys: whether a
// decrypted group message carried a sender key distribution message (SKDM) and
// whether whatsmeow stored it. A group whose messages always need a retry
// before they decrypt points at SKDMs that never arrive or never get stored.
const EventTypeE2EESenderKey EventType = "e2eeSenderKey"

// Kinds of sender key events.
const (
	SenderKeyMessage   = "message"    // group message decrypted
	SenderKeyStored    = "stored"     // SKDM processed and saved
	SenderKeyFailed    = "failed"     // SKDM could not be parsed or saved
	SenderKeyNonGroup  = "non_group"  // SKDM in a 1-1 chat, ignored
	SenderKeyRetryLost = "retry_lost" // retry receipt not sent or given up
)

// whatsmeow log lines around sender keys and retries (message.go,
// armadillomessage.go, retry.go). They are the only signal for these steps.
const (
	skdmProcessedLogFormat   = "Processed sender key distribution message from %s in %s"
	skdmParseFailedLogFormat = "Failed to parse sender key distribution message from %s for %s: %v"
	skdmStoreFailedLogFormat = "Failed to process sender key distribution message from %s for %s: %v"
	skdmNonGroupLogFormat    = "Got sender key distribution message in non-group chat from %s"
	retryReceiptFailedFormat = "Failed to send retry receipt for %s: %v"
	retryReceiptLimitFormat  = "Not sending any more retry receipts for %s"
)

// SenderKeyEvent carries no key material and no message content.
type SenderKeyEvent struct {
	Kind string `json:"kind"`
	// Sender device JID. fb-bot only shows a hash of it.
	Sender     string `json:"sender,omitempty"`
	IsFromMe   bool   `json:"isFromMe,omitempty"`
	RetryCount int    `json:"retryCount,omitempty"`
	HasSKDM    bool   `json:"hasSkdm,omitempty"`
	Error      string `json:"error,omitempty"`
}

// senderKeyLogEvent maps a whatsmeow log call to a sender key event.
func senderKeyLogEvent(msg string, args []any) (SenderKeyEvent, bool) {
	arg := func(i int) string {
		if i < len(args) {
			return fmt.Sprint(args[i])
		}
		return ""
	}
	errClass := func(i int) string {
		if i < len(args) {
			if err, ok := args[i].(error); ok {
				return classifyDecryptError(err)
			}
		}
		return DecryptReasonUnknown
	}
	// SKDM lines print a Signal address ("user:device"); for Messenger JIDs
	// (no agent) that is the JID without its server.
	address := func(i int) string {
		sender := arg(i)
		if sender != "" && !strings.Contains(sender, "@") {
			sender += "@" + types.MessengerServer
		}
		return sender
	}
	switch msg {
	case skdmProcessedLogFormat:
		return SenderKeyEvent{Kind: SenderKeyStored, Sender: address(0)}, true
	case skdmParseFailedLogFormat, skdmStoreFailedLogFormat:
		return SenderKeyEvent{Kind: SenderKeyFailed, Sender: arg(0), Error: errClass(2)}, true
	case skdmNonGroupLogFormat:
		return SenderKeyEvent{Kind: SenderKeyNonGroup, Sender: arg(0)}, true
	case retryReceiptFailedFormat:
		return SenderKeyEvent{Kind: SenderKeyRetryLost, Error: errClass(1)}, true
	case retryReceiptLimitFormat:
		return SenderKeyEvent{Kind: SenderKeyRetryLost, Error: "limit"}, true
	}
	return SenderKeyEvent{}, false
}

// groupMessageSenderKeyEvent describes a decrypted group message.
func groupMessageSenderKeyEvent(e *events.FBMessage) (SenderKeyEvent, bool) {
	if !e.Info.IsGroup {
		return SenderKeyEvent{}, false
	}
	return SenderKeyEvent{
		Kind:       SenderKeyMessage,
		Sender:     e.Info.Sender.String(),
		IsFromMe:   e.Info.IsFromMe,
		RetryCount: e.RetryCount,
		HasSKDM:    e.Transport.GetProtocol().GetAncillary().GetSkdm() != nil,
	}, true
}
