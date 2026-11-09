package bridge

import (
	"context"
	"testing"
)

func TestGetE2EEGroupInfoValidatesJID(t *testing.T) {
	c := &Client{ctx: context.Background()}
	for _, jid := range []string{"", "123@msgr", "@g.us", "not a jid"} {
		if _, err := c.GetE2EEGroupInfo(&GetE2EEGroupInfoOptions{ChatJID: jid}); err == nil {
			t.Errorf("chatJid %q accepted", jid)
		}
	}
	if _, err := c.GetE2EEGroupInfo(nil); err == nil {
		t.Error("nil options accepted")
	}
	// Valid JID but no E2EE connection: an error, not a panic.
	if _, err := c.GetE2EEGroupInfo(&GetE2EEGroupInfoOptions{ChatJID: "123@g.us"}); err == nil {
		t.Error("lookup without an E2EE client succeeded")
	}
}
