package agent

import (
	"testing"

	"github.com/fastclaw-ai/fastclaw/internal/bus"
)

// On IM channels the owner is recognised by pairing: the gateway flags
// FromChannelOwner when the sender is the account paired to the channel,
// and the channel's binder (OwnerUserID) must own the agent. Shared
// identity rewriting UserID to the owner id must not grant anything by
// itself — a stranger DMing the owner's bot carries the same rewrite.
func TestAdminChatterPairedOwner(t *testing.T) {
	a := &Agent{ownerUserID: "u_owner"}

	owner := bus.InboundMessage{
		Channel:          "feishu",
		UserID:           "u_chatter",
		OwnerUserID:      "u_owner",
		PeerKind:         "dm",
		FromChannelOwner: true,
	}
	if !a.isAdminChatter(owner) {
		t.Fatal("paired owner on the agent owner's channel should be admin")
	}
	inGroup := owner
	inGroup.PeerKind = "group"
	if !a.isAdminChatter(inGroup) {
		t.Fatal("paired owner speaking in a group is still verified by sender id")
	}

	// Someone else connected their own bot to this (shared) agent and
	// paired it: they own the channel, not the agent.
	foreign := owner
	foreign.OwnerUserID = "u_other"
	if a.isAdminChatter(foreign) {
		t.Fatal("paired owner of another user's channel must not be admin")
	}

	stranger := bus.InboundMessage{
		Channel:        "feishu",
		UserID:         "u_owner", // shared-identity rewrite
		OwnerUserID:    "u_owner",
		PeerKind:       "dm",
		SharedIdentity: true,
	}
	if a.isAdminChatter(stranger) {
		t.Fatal("shared-identity rewrite without pairing must not be admin")
	}
}

// Regular (non-shared) IM chatters are minted as app_users — never equal
// to the owner id — so they stay guests unless the admins allowlist
// names their platform id.
func TestAdminChatterRegularIMChannel(t *testing.T) {
	a := &Agent{
		ownerUserID: "u_owner",
		admins:      map[string][]string{"telegram": {"u_app_listed"}},
	}

	stranger := bus.InboundMessage{Channel: "telegram", UserID: "u_app_stranger", PeerKind: "dm"}
	if a.isAdminChatter(stranger) {
		t.Fatal("minted app_user chatter must not be admin")
	}

	listed := bus.InboundMessage{Channel: "telegram", UserID: "u_app_listed", PeerKind: "dm"}
	if !a.isAdminChatter(listed) {
		t.Fatal("allowlisted chatter should be admin")
	}
}
