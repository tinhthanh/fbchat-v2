package bridge

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rs/zerolog"
	"go.mau.fi/libsignal/signalerror"
	waTypes "go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
)

func TestClassifyDecryptErrorUnwrapsLibsignalErrors(t *testing.T) {
	// Same wrapping as whatsmeow decryptDM -> libsignal SessionCipher.
	wrap := func(err error) error {
		return fmt.Errorf("failed to decrypt normal message: %w",
			fmt.Errorf("failed to verify ciphertext MAC: %w", err))
	}
	cases := map[error]string{
		wrap(signalerror.ErrBadMAC):                DecryptReasonBadMAC,
		wrap(signalerror.ErrNoSessionForUser):      DecryptReasonNoSession,
		wrap(signalerror.ErrNoValidSessions):       DecryptReasonNoValidSession,
		wrap(signalerror.ErrUninitializedSession):  DecryptReasonNoValidSession,
		wrap(signalerror.ErrNoSenderKeyForUser):    DecryptReasonNoSenderKey,
		wrap(signalerror.ErrNoSenderKeyStateForID): DecryptReasonNoSenderKey,
		wrap(signalerror.ErrUntrustedIdentity):     DecryptReasonUntrustedIdentity,
		wrap(signalerror.ErrNoOneTimeKeyFound):     DecryptReasonPreKey,
		wrap(signalerror.ErrTooFarIntoFuture):      DecryptReasonTooFarInFuture,
		wrap(signalerror.ErrIncompleteMessage):     DecryptReasonInvalidMessage,
		errors.New("something else"):               DecryptReasonOther,
	}
	for err, want := range cases {
		if got := classifyDecryptError(err); got != want {
			t.Errorf("classifyDecryptError(%v) = %q, want %q", err, got, want)
		}
	}
}

type recordingWaLog struct {
	warnings []string
}

func (r *recordingWaLog) Warnf(msg string, args ...any) {
	r.warnings = append(r.warnings, fmt.Sprintf(msg, args...))
}
func (r *recordingWaLog) Errorf(string, ...any)   {}
func (r *recordingWaLog) Infof(string, ...any)    {}
func (r *recordingWaLog) Debugf(string, ...any)   {}
func (r *recordingWaLog) Sub(string) waLog.Logger { return r }

func TestDecryptFailureLogRecordsReasonAndForwards(t *testing.T) {
	inner := &recordingWaLog{}
	failures := newDecryptFailures()
	log := newDecryptFailureLog(inner, failures)

	log.Warnf(decryptErrorLogFormat, waTypes.MessageID("MSG1"), "sender", fmt.Errorf("x: %w", signalerror.ErrBadMAC))
	log.Warnf("Unhandled encrypted message (type %s) from %s", "foo", "bar")

	if len(inner.warnings) != 2 {
		t.Fatalf("inner logger got %d warnings, want 2", len(inner.warnings))
	}
	if got := failures.take("MSG1"); got != DecryptReasonBadMAC {
		t.Fatalf("take(MSG1) = %q, want %q", got, DecryptReasonBadMAC)
	}
	if got := failures.take("MSG1"); got != "" {
		t.Fatalf("second take(MSG1) = %q, want empty", got)
	}
}

func TestDecryptFailureLogCountsStatelessPrekeys(t *testing.T) {
	inner := &recordingWaLog{}
	failures := newDecryptFailures()
	log := newDecryptFailureLog(inner, failures)
	sender := waTypes.NewADJID("456", 0, 57)
	sender.Server = waTypes.MessengerServer

	log.Debugf(statelessPrekeyLogFormat, sender)
	log.Debugf(statelessPrekeyLogFormat, sender)
	log.Debugf("Decrypting message from %s", sender)

	if got := failures.statelessCount(sender.String()); got != 2 {
		t.Fatalf("statelessCount = %d, want 2", got)
	}
	c := &Client{decryptFailures: failures}
	info := waTypes.MessageInfo{MessageSource: waTypes.MessageSource{Sender: sender}, ID: "MSG3"}
	if got := c.undecryptableErrorEvent(&events.UndecryptableMessage{Info: info}); got.StatelessPrekeys != 2 {
		t.Fatalf("event StatelessPrekeys = %d, want 2", got.StatelessPrekeys)
	}
}

func TestDecryptFailuresIsBounded(t *testing.T) {
	failures := newDecryptFailures()
	for i := 0; i < maxPendingDecryptFailures+10; i++ {
		failures.put(fmt.Sprintf("m%d", i), DecryptReasonBadMAC)
	}
	if len(failures.reasons) != maxPendingDecryptFailures || len(failures.order) != maxPendingDecryptFailures {
		t.Fatalf("size = %d/%d, want %d", len(failures.reasons), len(failures.order), maxPendingDecryptFailures)
	}
	if got := failures.take("m0"); got != "" {
		t.Fatalf("oldest entry should be evicted, got %q", got)
	}
	last := fmt.Sprintf("m%d", maxPendingDecryptFailures+9)
	if got := failures.take(last); got != DecryptReasonBadMAC {
		t.Fatalf("newest entry = %q", got)
	}
}

func TestUndecryptableErrorEventCarriesReason(t *testing.T) {
	c := &Client{decryptFailures: newDecryptFailures()}
	chat := waTypes.NewJID("123", waTypes.GroupServer)
	sender := waTypes.NewJID("456", waTypes.MessengerServer)
	info := waTypes.MessageInfo{
		MessageSource: waTypes.MessageSource{Chat: chat, Sender: sender, IsGroup: true},
		ID:            "MSG2",
	}
	c.decryptFailures.put("MSG2", DecryptReasonBadMAC)

	got := c.undecryptableErrorEvent(&events.UndecryptableMessage{Info: info})
	if got.Code != 2 || got.Reason != DecryptReasonBadMAC || got.MessageID != "MSG2" ||
		got.ChatJID != chat.String() || got.SenderJID != sender.String() || !got.IsGroup {
		t.Fatalf("unexpected event: %+v", got)
	}

	if got := c.undecryptableErrorEvent(&events.UndecryptableMessage{Info: info}); got.Reason != DecryptReasonUnknown {
		t.Fatalf("reason without log = %q, want %q", got.Reason, DecryptReasonUnknown)
	}
	if got := c.undecryptableErrorEvent(&events.UndecryptableMessage{Info: info, IsUnavailable: true}); got.Reason != DecryptReasonUnavailable {
		t.Fatalf("reason for unavailable = %q, want %q", got.Reason, DecryptReasonUnavailable)
	}
}

func TestSignalLogWritesToBridgeLogger(t *testing.T) {
	var buf bytes.Buffer
	log := signalLog{log: zerolog.New(&buf)}
	log.Debug("SessionCipher.go:1", "hidden")
	log.Error("SessionCipher.go:319", "Unable to verify ciphertext mac")
	out := buf.String()
	if strings.Contains(out, "hidden") {
		t.Fatalf("debug should be dropped: %s", out)
	}
	if !strings.Contains(out, "Unable to verify ciphertext mac") || !strings.Contains(out, `"level":"error"`) {
		t.Fatalf("error not logged: %s", out)
	}
}

// The capture depends on whatsmeow's exact log format: fail loudly when an
// upgrade changes it instead of silently reporting "unknown".
func TestWhatsmeowStillLogsDecryptErrorFormat(t *testing.T) {
	out, err := exec.Command("go", "list", "-m", "-f", "{{.Dir}}", "go.mau.fi/whatsmeow").Output()
	if err != nil {
		t.Skipf("go list unavailable: %v", err)
	}
	source, err := os.ReadFile(filepath.Join(strings.TrimSpace(string(out)), "message.go"))
	if err != nil {
		t.Skipf("whatsmeow source unavailable: %v", err)
	}
	if !strings.Contains(string(source), `cli.Log.Warnf("`+decryptErrorLogFormat+`", info.ID, info.SourceString(), err)`) {
		t.Fatal("whatsmeow no longer logs decrypt errors with decryptErrorLogFormat; update decrypt_failures.go")
	}
	if !strings.Contains(string(source), `cli.Log.Debugf("`+statelessPrekeyLogFormat+`", from)`) {
		t.Fatal("whatsmeow no longer logs stateless prekey messages with statelessPrekeyLogFormat; update decrypt_failures.go")
	}
}
