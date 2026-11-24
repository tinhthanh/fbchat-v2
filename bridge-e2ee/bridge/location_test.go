package bridge

import (
	"testing"

	"go.mau.fi/whatsmeow/proto/waArmadilloXMA"
	"google.golang.org/protobuf/proto"
)

func locationXMA(nativeURL, title, subtitle string) *waArmadilloXMA.ExtendedContentMessage {
	return &waArmadilloXMA.ExtendedContentMessage{
		TargetType: waArmadilloXMA.ExtendedContentMessage_MSG_LOCATION_SHARING_V2.Enum(),
		Ctas: []*waArmadilloXMA.ExtendedContentMessage_CTA{{
			ButtonType: waArmadilloXMA.ExtendedContentMessage_OPEN_NATIVE.Enum(),
			NativeURL:  proto.String(nativeURL),
		}},
		TitleText:    proto.String(title),
		SubtitleText: proto.String(subtitle),
	}
}

// Vị trí ghim trên app Messenger (thử thật 24/11/2026): trước đây bridge bỏ cả tin.
func TestArmadilloLocationReadsPinnedLocation(t *testing.T) {
	att := armadilloLocation(locationXMA("messenger://location_share?lat=-0.153000&long=29.241000", "Vị trí đã ghim", "Lubero, Cộng hoà dân chủ Congo"))
	if att == nil {
		t.Fatal("pinned location dropped")
	}
	if att.Type != "location" || att.Latitude != -0.153 || att.Longitude != 29.241 {
		t.Fatalf("att = %+v", att)
	}
	if att.Description != "Lubero, Cộng hoà dân chủ Congo" || att.FileName != "Vị trí đã ghim" {
		t.Fatalf("labels = %q / %q", att.Description, att.FileName)
	}
	// Không có phụ đề: nhãn là tiêu đề.
	if att := armadilloLocation(locationXMA("messenger://location_share?lat=10&long=106", "Shared location", "")); att == nil || att.Description != "Shared location" {
		t.Fatalf("att = %+v", att)
	}
}

func TestArmadilloLocationSkipsEventsWithoutCoordinates(t *testing.T) {
	for _, u := range []string{
		"",
		"https://www.facebook.com/maps?lat=1&long=2",
		"messenger://location_share",
		"messenger://location_share?lat=abc&long=2",
		"messenger://location_share?lat=91&long=2",
		"messenger://location_share?lat=1&long=181",
	} {
		if att := armadilloLocation(locationXMA(u, "Vị trí trực tiếp", "")); att != nil {
			t.Errorf("%q: got %+v, want nil", u, att)
		}
	}
	if att := armadilloLocation(&waArmadilloXMA.ExtendedContentMessage{}); att != nil {
		t.Errorf("no CTA: got %+v", att)
	}
}
