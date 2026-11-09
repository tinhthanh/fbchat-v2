package bridge

import (
	"context"
	"errors"
	"fmt"

	"go.mau.fi/whatsmeow/types"
)

// GetE2EEGroupInfoOptions names the E2EE group to look up.
type GetE2EEGroupInfoOptions struct {
	ChatJID string `json:"chatJid"`
}

// E2EEGroupInfo is the part of a Messenger E2EE group's metadata the bot
// needs. It carries no member IDs.
type E2EEGroupInfo struct {
	Name             string `json:"name"`
	ParticipantCount int    `json:"participantCount"`
}

// GetE2EEGroupInfo reads an E2EE group's name from the server over the open
// E2EE socket (the same query mautrix-meta uses). The LightSpeed thread table
// only names a group when Messenger happens to sync that thread.
func (c *Client) GetE2EEGroupInfo(opts *GetE2EEGroupInfoOptions) (*E2EEGroupInfo, error) {
	if opts == nil {
		return nil, errors.New("chatJid is required")
	}
	jid, err := types.ParseJID(opts.ChatJID)
	if err != nil || jid.Server != types.GroupServer || jid.User == "" {
		return nil, fmt.Errorf("chatJid must be an E2EE group JID (<id>@%s)", types.GroupServer)
	}
	e2eeClient := c.snapshotE2EEClient()
	if e2eeClient == nil {
		return nil, errors.New("E2EE client not connected")
	}
	ctx, cancel := context.WithTimeout(c.ctx, userInfoTimeout)
	defer cancel()
	info, err := e2eeClient.GetGroupInfo(ctx, jid)
	if err != nil {
		return nil, err
	}
	return &E2EEGroupInfo{Name: info.Name, ParticipantCount: len(info.Participants)}, nil
}
