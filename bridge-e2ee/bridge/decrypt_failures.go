package bridge

import (
	"errors"
	"fmt"
	"sync"

	"github.com/rs/zerolog"
	signallogger "go.mau.fi/libsignal/logger"
	"go.mau.fi/libsignal/signalerror"
	waLog "go.mau.fi/whatsmeow/util/log"
)

// whatsmeow (message.go, decryptMessages) logs this right before dispatching
// events.UndecryptableMessage on the same goroutine. The event itself carries
// no error, so the log call is the only place the cause is visible.
const decryptErrorLogFormat = "Error decrypting message %s from %s: %v"

// whatsmeow (decryptDM) logs this when a prekey message carries state="false":
// it decrypts the message but drops the new Signal session. If the sender keeps
// using that session, every later message fails (no_session or bad_mac).
const statelessPrekeyLogFormat = "Not storing session from stateless prekey message from %s"

const maxPendingDecryptFailures = 256

// Decrypt failure reasons reported in the error event (code 2).
const (
	DecryptReasonBadMAC            = "bad_mac"
	DecryptReasonNoSession         = "no_session"
	DecryptReasonNoSenderKey       = "no_sender_key"
	DecryptReasonUntrustedIdentity = "untrusted_identity"
	DecryptReasonPreKey            = "prekey"
	DecryptReasonTooFarInFuture    = "too_far_in_future"
	DecryptReasonInvalidMessage    = "invalid_message"
	DecryptReasonUnavailable       = "unavailable"
	DecryptReasonOther             = "other"
	DecryptReasonUnknown           = "unknown"
)

func classifyDecryptError(err error) string {
	switch {
	case errors.Is(err, signalerror.ErrBadMAC):
		return DecryptReasonBadMAC
	case errors.Is(err, signalerror.ErrNoSessionForUser),
		errors.Is(err, signalerror.ErrNoValidSessions),
		errors.Is(err, signalerror.ErrUninitializedSession):
		return DecryptReasonNoSession
	case errors.Is(err, signalerror.ErrNoSenderKeyForUser),
		errors.Is(err, signalerror.ErrNoSenderKeyStatesInRecord),
		errors.Is(err, signalerror.ErrNoSenderKeyStateForID):
		return DecryptReasonNoSenderKey
	case errors.Is(err, signalerror.ErrUntrustedIdentity):
		return DecryptReasonUntrustedIdentity
	case errors.Is(err, signalerror.ErrNoSignedPreKey),
		errors.Is(err, signalerror.ErrNoOneTimeKeyFound),
		errors.Is(err, signalerror.ErrInvalidSignature):
		return DecryptReasonPreKey
	case errors.Is(err, signalerror.ErrTooFarIntoFuture):
		return DecryptReasonTooFarInFuture
	case errors.Is(err, signalerror.ErrWrongMessageVersion),
		errors.Is(err, signalerror.ErrOldMessageVersion),
		errors.Is(err, signalerror.ErrUnknownMessageVersion),
		errors.Is(err, signalerror.ErrIncompleteMessage):
		return DecryptReasonInvalidMessage
	}
	return DecryptReasonOther
}

// decryptFailures maps message ID -> reason between the whatsmeow log call and
// the UndecryptableMessage event. Bounded: a reason whose event never comes
// (e.g. a cancelled context) is evicted oldest first.
type decryptFailures struct {
	mu      sync.Mutex
	reasons map[string]string
	order   []string
	// sender JID -> stateless prekey messages seen (sessions not stored)
	stateless      map[string]int
	statelessOrder []string
}

func newDecryptFailures() *decryptFailures {
	return &decryptFailures{reasons: make(map[string]string), stateless: make(map[string]int)}
}

func (d *decryptFailures) markStateless(sender string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, ok := d.stateless[sender]; !ok {
		d.statelessOrder = append(d.statelessOrder, sender)
	}
	d.stateless[sender]++
	for len(d.statelessOrder) > maxPendingDecryptFailures {
		delete(d.stateless, d.statelessOrder[0])
		d.statelessOrder = d.statelessOrder[1:]
	}
}

// statelessCount returns how many stateless prekey messages came from sender.
func (d *decryptFailures) statelessCount(sender string) int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.stateless[sender]
}

func (d *decryptFailures) put(messageID, reason string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, ok := d.reasons[messageID]; !ok {
		d.order = append(d.order, messageID)
	}
	d.reasons[messageID] = reason
	for len(d.order) > maxPendingDecryptFailures {
		delete(d.reasons, d.order[0])
		d.order = d.order[1:]
	}
}

// take returns and forgets the reason recorded for messageID ("" if none).
func (d *decryptFailures) take(messageID string) string {
	d.mu.Lock()
	defer d.mu.Unlock()
	reason, ok := d.reasons[messageID]
	if !ok {
		return ""
	}
	delete(d.reasons, messageID)
	for i, id := range d.order {
		if id == messageID {
			d.order = append(d.order[:i], d.order[i+1:]...)
			break
		}
	}
	return reason
}

// decryptFailureLog wraps whatsmeow's client logger to capture decrypt errors.
// It records the reason even when the bridge log level hides the line.
type decryptFailureLog struct {
	waLog.Logger
	failures *decryptFailures
}

func newDecryptFailureLog(inner waLog.Logger, failures *decryptFailures) waLog.Logger {
	if inner == nil {
		inner = waLog.Noop
	}
	return &decryptFailureLog{Logger: inner, failures: failures}
}

func (l *decryptFailureLog) Warnf(msg string, args ...any) {
	if msg == decryptErrorLogFormat && len(args) == 3 {
		if err, ok := args[2].(error); ok {
			l.failures.put(fmt.Sprint(args[0]), classifyDecryptError(err))
		}
	}
	l.Logger.Warnf(msg, args...)
}

func (l *decryptFailureLog) Debugf(msg string, args ...any) {
	if msg == statelessPrekeyLogFormat && len(args) == 1 {
		l.failures.markStateless(fmt.Sprint(args[0]))
	}
	l.Logger.Debugf(msg, args...)
}

// signalLog sends libsignal's logs to the bridge logger. Its default logger
// prints to stdout, which is the JSON-RPC channel to Python.
type signalLog struct {
	log zerolog.Logger
}

func (s signalLog) Debug(caller, msg string)   {}
func (s signalLog) Info(caller, msg string)    { s.log.Info().Str("caller", caller).Msg(msg) }
func (s signalLog) Warning(caller, msg string) { s.log.Warn().Str("caller", caller).Msg(msg) }
func (s signalLog) Error(caller, msg string)   { s.log.Error().Str("caller", caller).Msg(msg) }
func (s signalLog) Configure(string)           {}

var installSignalLogOnce sync.Once

// installSignalLog replaces libsignal's global logger once per process, before
// any session is used, so no goroutine reads the global while it is written.
func installSignalLog(log zerolog.Logger) {
	installSignalLogOnce.Do(func() {
		var loggable signallogger.Loggable = signalLog{log: log.With().Str("component", "libsignal").Logger()}
		signallogger.Setup(&loggable)
	})
}
