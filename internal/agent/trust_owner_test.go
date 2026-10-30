package agent

import (
	"testing"

	"github.com/fastclaw-ai/fastclaw/internal/bus"
)

// An agent attached into another user's space (an app's end-user, a
// public-link visitor) runs with that user as ownerUserID. That user must
// not become the operator: trust follows the agent's real owner.
func TestOperatorTrustFollowsRealOwner(t *testing.T) {
	a := &Agent{ownerUserID: "u_enduser", agentOwnerID: "u_owner"}
	for _, ch := range []string{"api", "web"} {
		if a.isAdminChatter(bus.InboundMessage{Channel: ch, UserID: "u_enduser"}) {
			t.Fatalf("%s: the space user was treated as the operator", ch)
		}
		if !a.isAdminChatter(bus.InboundMessage{Channel: ch, UserID: "u_owner"}) {
			t.Fatalf("%s: the real owner lost operator trust", ch)
		}
	}
	if a.isAdminChatter(bus.InboundMessage{Channel: "api", UserID: "api-user"}) {
		t.Fatal("anonymous API callers must not be the operator")
	}
	// Legacy installs without agents.user_id keep the space-user rule.
	legacy := &Agent{ownerUserID: "u_owner"}
	if !legacy.isAdminChatter(bus.InboundMessage{Channel: "web", UserID: "u_owner"}) {
		t.Fatal("legacy owner lost operator trust")
	}
}
