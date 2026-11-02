package bridge

import (
	"context"
	"errors"
	"testing"

	"go.mau.fi/mautrix-meta/pkg/messagix"
	"go.mau.fi/whatsmeow/proto/waConsumerApplication"
	"go.mau.fi/whatsmeow/proto/waMediaTransport"
	"google.golang.org/protobuf/proto"
)

func stickerMessage(t *testing.T, transport *waMediaTransport.StickerTransport) *waConsumerApplication.ConsumerApplication_StickerMessage {
	t.Helper()
	msg := &waConsumerApplication.ConsumerApplication_StickerMessage{}
	if err := msg.Set(transport); err != nil {
		t.Fatal(err)
	}
	return msg
}

func stubReceiverFetch(t *testing.T, fn func(id string) (*messagix.ReceiverFetchMedia, error)) *[]string {
	t.Helper()
	var calls []string
	orig := fetchReceiverFetchMedia
	fetchReceiverFetchMedia = func(_ context.Context, _ *messagix.Client, id string) (*messagix.ReceiverFetchMedia, error) {
		calls = append(calls, id)
		return fn(id)
	}
	t.Cleanup(func() { fetchReceiverFetchMedia = orig })
	return &calls
}

func TestE2EECatalogStickerResolvedToURL(t *testing.T) {
	calls := stubReceiverFetch(t, func(string) (*messagix.ReceiverFetchMedia, error) {
		return &messagix.ReceiverFetchMedia{URL: "https://scontent.xx.fbcdn.net/sticker.png", MimeType: "image/png", Width: 240, Height: 240}, nil
	})
	c := &Client{Messagix: &messagix.Client{}}
	att := c.extractE2EEStickerAttachment(stickerMessage(t, &waMediaTransport.StickerTransport{
		Ancillary: &waMediaTransport.StickerTransport_Ancillary{ReceiverFetchID: proto.String("rf-1"), Width: proto.Uint32(120), Height: proto.Uint32(120)},
	}))
	if len(*calls) != 1 || (*calls)[0] != "rf-1" {
		t.Fatalf("calls %v", *calls)
	}
	if att.Type != "sticker" || att.URL != "https://scontent.xx.fbcdn.net/sticker.png" || att.MimeType != "image/png" || att.Width != 240 || att.Height != 240 {
		t.Fatalf("attachment %+v", att)
	}
	if _, err := validateMediaURL(att.URL); err != nil {
		t.Fatalf("downloadMedia would refuse %s: %v", att.URL, err)
	}
}

func TestE2EECatalogStickerFetchFailureKeepsPlaceholder(t *testing.T) {
	stubReceiverFetch(t, func(string) (*messagix.ReceiverFetchMedia, error) { return nil, errors.New("boom") })
	c := &Client{Messagix: &messagix.Client{}}
	att := c.extractE2EEStickerAttachment(stickerMessage(t, &waMediaTransport.StickerTransport{
		Ancillary: &waMediaTransport.StickerTransport_Ancillary{ReceiverFetchID: proto.String("rf-2"), Width: proto.Uint32(120), Height: proto.Uint32(96)},
	}))
	if att.URL != "" || att.DirectPath != "" || att.Width != 120 || att.Height != 96 {
		t.Fatalf("attachment %+v", att)
	}
}

func TestE2EEUploadedStickerUsesMediaKeys(t *testing.T) {
	calls := stubReceiverFetch(t, func(string) (*messagix.ReceiverFetchMedia, error) {
		t.Fatal("receiver fetch must not run without an id")
		return nil, nil
	})
	c := &Client{Messagix: &messagix.Client{}}
	att := c.extractE2EEStickerAttachment(stickerMessage(t, &waMediaTransport.StickerTransport{
		Integral: &waMediaTransport.StickerTransport_Integral{Transport: &waMediaTransport.WAMediaTransport{
			Integral:  &waMediaTransport.WAMediaTransport_Integral{MediaKey: []byte{1}, DirectPath: proto.String("/v/t62/x")},
			Ancillary: &waMediaTransport.WAMediaTransport_Ancillary{Mimetype: proto.String("image/webp"), FileLength: proto.Uint64(10)},
		}},
	}))
	if len(*calls) != 0 || att.DirectPath != "/v/t62/x" || att.MimeType != "image/webp" || att.URL != "" {
		t.Fatalf("attachment %+v calls %v", att, *calls)
	}
}
