package bridge

import (
	"testing"

	"go.mau.fi/libsignal/ecc"
	"go.mau.fi/libsignal/keys/identity"
	"go.mau.fi/libsignal/keys/prekey"
	"go.mau.fi/libsignal/util/optional"
	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/util/keys"
)

func ownBundle(device *store.Device, preKey *keys.PreKey, signedPub [32]byte) *prekey.Bundle {
	preKeyID := optional.NewEmptyUint32()
	var preKeyPub ecc.ECPublicKeyable
	if preKey != nil {
		preKeyID = optional.NewOptionalUint32(preKey.KeyID)
		preKeyPub = ecc.NewDjbECPublicKey(*preKey.Pub)
	}
	return prekey.NewBundle(device.RegistrationID, 181, preKeyID, device.SignedPreKey.KeyID,
		preKeyPub, ecc.NewDjbECPublicKey(signedPub), *device.SignedPreKey.Signature,
		identity.NewKey(ecc.NewDjbECPublicKey(*device.IdentityKey.Pub)))
}

func TestCompareOwnBundle(t *testing.T) {
	identityKey := keys.NewKeyPair()
	device := &store.Device{
		IdentityKey:    identityKey,
		SignedPreKey:   identityKey.CreateSignedPreKey(1),
		RegistrationID: 42,
	}
	preKey := keys.NewPreKey(7)

	got := compareOwnBundle(device, ownBundle(device, preKey, *device.SignedPreKey.Pub), preKey)
	want := KeyCheckEvent{Fetched: true, IdentityMatch: true, RegistrationMatch: true,
		SignedPreKeyMatch: true, PreKeyPresent: true, PreKeyKnown: true, PreKeyMatch: true}
	if got != want {
		t.Fatalf("matching bundle = %+v, want %+v", got, want)
	}

	// Server holds another signed prekey under the same ID.
	other := keys.NewKeyPair()
	got = compareOwnBundle(device, ownBundle(device, preKey, *other.Pub), preKey)
	if got.SignedPreKeyMatch || !got.IdentityMatch {
		t.Fatalf("stale signed prekey = %+v", got)
	}

	// Same one-time prekey ID, different key (regenerated locally).
	got = compareOwnBundle(device, ownBundle(device, preKey, *device.SignedPreKey.Pub), keys.NewPreKey(7))
	if !got.PreKeyKnown || got.PreKeyMatch {
		t.Fatalf("regenerated prekey = %+v", got)
	}

	// Server has no one-time prekeys left; bot no longer has that ID.
	got = compareOwnBundle(device, ownBundle(device, nil, *device.SignedPreKey.Pub), nil)
	if got.PreKeyPresent || got.PreKeyKnown {
		t.Fatalf("no one-time prekey = %+v", got)
	}
	got = compareOwnBundle(device, ownBundle(device, preKey, *device.SignedPreKey.Pub), nil)
	if !got.PreKeyPresent || got.PreKeyKnown {
		t.Fatalf("unknown one-time prekey = %+v", got)
	}
}
