package agent

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fastclaw-ai/fastclaw/internal/store"
)

// A held account's agents refuse new turns on every channel; the hold
// follows the agent's real owner (who pays), not the chatter's space.
func TestCheckQuotaHonorsBillingHold(t *testing.T) {
	ctx := context.Background()
	st, err := store.NewDBStore("sqlite", "file:"+filepath.Join(t.TempDir(), "hold.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateUser(ctx, &store.UserRecord{ID: "u_owner", Username: "o", Email: "o@x", Role: "user", Status: "active"}); err != nil {
		t.Fatal(err)
	}
	a := &Agent{dataStore: st, ownerUserID: "u_enduser", agentOwnerID: "u_owner"}
	if msg := a.checkQuota(ctx); msg != "" {
		t.Fatalf("no hold: %q", msg)
	}
	if err := st.SetBillingHold(ctx, "u_owner", true, "internal note"); err != nil {
		t.Fatal(err)
	}
	msg := a.checkQuota(ctx)
	if msg == "" || strings.Contains(msg, "internal note") {
		t.Fatalf("held: %q", msg)
	}
}
