package bridge

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"go.mau.fi/libsignal/signalerror"
	"go.mau.fi/whatsmeow/proto/waMsgTransport"
	waTypes "go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
)

func TestDecryptFailureLogReportsSenderKeyLines(t *testing.T) {
	var got []SenderKeyEvent
	log := newDecryptFailureLog(&recordingWaLog{}, newDecryptFailures(), func(event SenderKeyEvent) {
		got = append(got, event)
	})
	from := waTypes.NewJID("456", waTypes.MessengerServer)
	from.Device = 3

	log.Debugf(skdmProcessedLogFormat, "456:3", "123@g.us")
	log.Errorf(skdmStoreFailedLogFormat, from, "123@g.us", signalerror.ErrIncompleteMessage)
	log.Warnf(skdmNonGroupLogFormat, from)
	log.Errorf(retryReceiptFailedFormat, "MSG1", errors.New("timeout"))
	log.Warnf(retryReceiptLimitFormat, "MSG1")
	log.Debugf("Decrypting message from %s", from)

	want := []SenderKeyEvent{
		{Kind: SenderKeyStored, Sender: "456:3@msgr"},
		{Kind: SenderKeyFailed, Sender: "456:3@msgr", Error: DecryptReasonInvalidMessage},
		{Kind: SenderKeyNonGroup, Sender: "456:3@msgr"},
		{Kind: SenderKeyRetryLost, Error: DecryptReasonOther},
		{Kind: SenderKeyRetryLost, Error: "limit"},
	}
	if len(got) != len(want) {
		t.Fatalf("events = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("event %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestGroupMessageSenderKeyEvent(t *testing.T) {
	sender := waTypes.NewJID("456", waTypes.MessengerServer)
	msg := &events.FBMessage{
		Info:       waTypes.MessageInfo{MessageSource: waTypes.MessageSource{Sender: sender, IsGroup: true}},
		RetryCount: 1,
		Transport: &waMsgTransport.MessageTransport{Protocol: &waMsgTransport.MessageTransport_Protocol{
			Ancillary: &waMsgTransport.MessageTransport_Protocol_Ancillary{
				Skdm: &waMsgTransport.MessageTransport_Protocol_Ancillary_SenderKeyDistributionMessage{},
			},
		}},
	}
	got, ok := groupMessageSenderKeyEvent(msg)
	want := SenderKeyEvent{Kind: SenderKeyMessage, Sender: "456@msgr", RetryCount: 1, HasSKDM: true}
	if !ok || got != want {
		t.Fatalf("event = %+v (%v), want %+v", got, ok, want)
	}
	msg.Transport = nil
	if got, _ := groupMessageSenderKeyEvent(msg); got.HasSKDM {
		t.Fatal("nil transport reported an SKDM")
	}
	msg.Info.IsGroup = false
	if _, ok := groupMessageSenderKeyEvent(msg); ok {
		t.Fatal("1-1 message produced a sender key event")
	}
}

func TestWhatsmeowStillLogsSenderKeyFormats(t *testing.T) {
	out, err := exec.Command("go", "list", "-m", "-f", "{{.Dir}}", "go.mau.fi/whatsmeow").Output()
	if err != nil {
		t.Skipf("go list unavailable: %v", err)
	}
	dir := strings.TrimSpace(string(out))
	var source strings.Builder
	for _, name := range []string{"message.go", "armadillomessage.go", "retry.go"} {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Skipf("whatsmeow source unavailable: %v", err)
		}
		source.Write(data)
	}
	for _, format := range []string{skdmProcessedLogFormat, skdmParseFailedLogFormat, skdmStoreFailedLogFormat,
		skdmNonGroupLogFormat, retryReceiptFailedFormat, retryReceiptLimitFormat} {
		if !strings.Contains(source.String(), `("`+format+`"`) {
			t.Errorf("whatsmeow no longer logs %q; update sender_key_diag.go", format)
		}
	}
}
