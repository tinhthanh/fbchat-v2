package bridge

import (
	"context"
	"time"

	"go.mau.fi/libsignal/keys/prekey"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/util/keys"
)

// EventTypeE2EEKeyCheck reports whether the keys the server hands out for the
// bot's own device match the private keys the bot holds. Senders build new
// sessions from these keys: a mismatch makes every prekey message from them
// fail with no_valid_session while messages the bot sends still work.
const EventTypeE2EEKeyCheck EventType = "e2eeKeyCheck"

const keyCheckTimeout = 30 * time.Second

// KeyCheckEvent carries only match flags, never key material.
type KeyCheckEvent struct {
	Fetched           bool   `json:"fetched"`
	Error             string `json:"error,omitempty"`
	IdentityMatch     bool   `json:"identityMatch"`
	RegistrationMatch bool   `json:"registrationMatch"`
	SignedPreKeyMatch bool   `json:"signedPreKeyMatch"`
	// The server returned a one-time prekey, the bot still holds that ID, and
	// its public key matches.
	PreKeyPresent bool `json:"preKeyPresent"`
	PreKeyKnown   bool `json:"preKeyKnown"`
	PreKeyMatch   bool `json:"preKeyMatch"`
}

// compareOwnBundle checks a prekey bundle fetched for the bot's own device
// against the device's keys. local is the bot's prekey with the bundle's
// one-time prekey ID (nil if unknown or the bundle has none).
func compareOwnBundle(device *store.Device, bundle *prekey.Bundle, local *keys.PreKey) KeyCheckEvent {
	result := KeyCheckEvent{Fetched: true}
	if identity := bundle.IdentityKey(); identity != nil && device.IdentityKey != nil {
		result.IdentityMatch = identity.PublicKey().PublicKey() == *device.IdentityKey.Pub
	}
	result.RegistrationMatch = bundle.RegistrationID() == device.RegistrationID
	if signed := bundle.SignedPreKey(); signed != nil && device.SignedPreKey != nil {
		result.SignedPreKeyMatch = bundle.SignedPreKeyID() == device.SignedPreKey.KeyID &&
			signed.PublicKey() == *device.SignedPreKey.Pub
	}
	if id := bundle.PreKeyID(); id != nil && !id.IsEmpty && bundle.PreKey() != nil {
		result.PreKeyPresent = true
		if local != nil {
			result.PreKeyKnown = true
			result.PreKeyMatch = bundle.PreKey().PublicKey() == *local.Pub
		}
	}
	return result
}

// checkOwnKeys fetches the bot's own prekey bundle from the server (this uses
// up one one-time prekey) and compares it with the local device.
func (c *Client) checkOwnKeys(e2eeClient *whatsmeow.Client) KeyCheckEvent {
	device := e2eeClient.Store
	if device == nil || device.ID == nil {
		return KeyCheckEvent{Error: "no device"}
	}
	ctx, cancel := context.WithTimeout(c.ctx, keyCheckTimeout)
	defer cancel()
	bundles := e2eeClient.DangerousInternals().FetchPreKeysNoError(ctx, []types.JID{*device.ID})
	var bundle *prekey.Bundle
	for _, b := range bundles {
		bundle = b
	}
	if bundle == nil {
		return KeyCheckEvent{Error: "no bundle returned"}
	}
	var local *keys.PreKey
	if id := bundle.PreKeyID(); id != nil && !id.IsEmpty {
		local, _ = device.PreKeys.GetPreKey(ctx, id.Value)
	}
	return compareOwnBundle(device, bundle, local)
}
