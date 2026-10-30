package gateway

import (
	"context"
	"testing"
	"time"

	"github.com/fastclaw-ai/fastclaw/internal/bus"
	"github.com/fastclaw-ai/fastclaw/internal/store"
)

func newPairingFixture(t *testing.T) (*Gateway, *store.DBStore, *bus.MessageBus) {
	t.Helper()
	db, err := store.NewDBStore("sqlite", "file:"+t.Name()+"?mode=memory&cache=shared")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if err := db.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	mb := bus.New()
	return &Gateway{store: db, bus: mb}, db, mb
}

func saveTestChannel(t *testing.T, db *store.DBStore, userID string) *store.ChannelRecord {
	t.Helper()
	ctx := context.Background()
	if err := db.SaveChannel(ctx, &store.ChannelRecord{
		UserID: userID, AgentID: "agt_1", Type: "telegram", AccountID: "bot-a", Enabled: true,
	}); err != nil {
		t.Fatalf("save channel: %v", err)
	}
	ch, err := db.LookupChannel(ctx, "telegram", "bot-a")
	if err != nil {
		t.Fatalf("lookup channel: %v", err)
	}
	return ch
}

func reload(t *testing.T, db *store.DBStore) *store.ChannelRecord {
	t.Helper()
	ch, err := db.LookupChannel(context.Background(), "telegram", "bot-a")
	if err != nil {
		t.Fatalf("lookup channel: %v", err)
	}
	return ch
}

func lastReply(t *testing.T, mb *bus.MessageBus) string {
	t.Helper()
	select {
	case out := <-mb.Outbound:
		return out.Text
	default:
		t.Fatal("expected a reply")
		return ""
	}
}

func dmFrom(userID, text string) bus.InboundMessage {
	return bus.InboundMessage{
		Channel: "telegram", AccountID: "bot-a", ChatID: "c-" + userID,
		UserID: userID, PeerKind: "dm", Text: text,
	}
}

func TestPairingFlow(t *testing.T) {
	g, db, mb := newPairingFixture(t)
	ctx := context.Background()
	ch := saveTestChannel(t, db, "u_owner")

	// Unpaired: ordinary messages are answered with a notice, not routed.
	if _, handled := g.gatePairing(ctx, dmFrom("111", "hi"), ch); !handled {
		t.Fatal("unpaired channel must not route messages")
	}
	if got := lastReply(t, mb); got != pairMsgNotPaired {
		t.Fatalf("reply = %q", got)
	}

	if err := db.SetChannelPairCode(ctx, ch.ID, "ABCD2345", time.Now().Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	ch = reload(t, db)

	// /pair in a group is refused even with the right code.
	group := dmFrom("111", "@bot /pair ABCD2345")
	group.PeerKind = "group"
	g.gatePairing(ctx, group, ch)
	if got := lastReply(t, mb); got != pairMsgInGroup {
		t.Fatalf("group reply = %q", got)
	}

	// Right code in a DM binds the sender (case/separator-insensitive).
	g.gatePairing(ctx, dmFrom("111", "/pair abcd-2345"), ch)
	if got := lastReply(t, mb); got != pairMsgOK {
		t.Fatalf("pair reply = %q", got)
	}
	ch = reload(t, db)
	if ch.BoundUserID != "111" || ch.PairCode != "" {
		t.Fatalf("after pairing: bound=%q code=%q", ch.BoundUserID, ch.PairCode)
	}

	// Paired: everyone is routed, only the bound sender is the owner.
	if fromOwner, handled := g.gatePairing(ctx, dmFrom("111", "hi"), ch); handled || !fromOwner {
		t.Fatalf("owner: fromOwner=%v handled=%v", fromOwner, handled)
	}
	if fromOwner, handled := g.gatePairing(ctx, dmFrom("222", "hi"), ch); handled || fromOwner {
		t.Fatalf("stranger: fromOwner=%v handled=%v", fromOwner, handled)
	}
	g.gatePairing(ctx, dmFrom("222", "/pair ABCD2345"), ch)
	if got := lastReply(t, mb); got != pairMsgAlreadySet {
		t.Fatalf("re-pair reply = %q", got)
	}
}

func TestPairingBurnsCodeAfterRepeatedFailures(t *testing.T) {
	g, db, mb := newPairingFixture(t)
	ctx := context.Background()
	ch := saveTestChannel(t, db, "u_owner")
	if err := db.SetChannelPairCode(ctx, ch.ID, "ABCD2345", time.Now().Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	ch = reload(t, db)

	for i := 1; i < maxPairAttempts; i++ {
		g.gatePairing(ctx, dmFrom("666", "/pair WRONG000"), ch)
		if got := lastReply(t, mb); got != pairMsgBadCode {
			t.Fatalf("attempt %d reply = %q", i, got)
		}
	}
	g.gatePairing(ctx, dmFrom("666", "/pair WRONG000"), ch)
	if got := lastReply(t, mb); got != pairMsgBurned {
		t.Fatalf("final reply = %q", got)
	}
	if ch = reload(t, db); ch.PairCode != "" {
		t.Fatal("code should be burned")
	}
	// The real code no longer works either.
	g.gatePairing(ctx, dmFrom("111", "/pair ABCD2345"), ch)
	if got := lastReply(t, mb); got != pairMsgBadCode {
		t.Fatalf("burned code reply = %q", got)
	}
}

func TestPairingExpiredCode(t *testing.T) {
	g, db, mb := newPairingFixture(t)
	ctx := context.Background()
	ch := saveTestChannel(t, db, "u_owner")
	if err := db.SetChannelPairCode(ctx, ch.ID, "ABCD2345", time.Now().Add(-time.Second)); err != nil {
		t.Fatal(err)
	}
	g.gatePairing(ctx, dmFrom("111", "/pair ABCD2345"), reload(t, db))
	if got := lastReply(t, mb); got != pairMsgBadCode {
		t.Fatalf("reply = %q", got)
	}
	if reload(t, db).BoundUserID != "" {
		t.Fatal("expired code must not bind")
	}
}

// Re-saving a channel keeps its pairing; another user taking over the
// same bot starts unpaired.
func TestSaveChannelPairingSurvivesOnlySameBinder(t *testing.T) {
	_, db, _ := newPairingFixture(t)
	ctx := context.Background()
	ch := saveTestChannel(t, db, "u_owner")
	if err := db.SetChannelBinding(ctx, ch.ID, "111", "Alice"); err != nil {
		t.Fatal(err)
	}

	saveTestChannel(t, db, "u_owner")
	if got := reload(t, db); got.BoundUserID != "111" || got.BoundUserName != "Alice" {
		t.Fatalf("same binder re-save lost pairing: %+v", got)
	}

	saveTestChannel(t, db, "u_other")
	if got := reload(t, db); got.BoundUserID != "" {
		t.Fatalf("new binder inherited pairing: %q", got.BoundUserID)
	}
}

func TestParsePairCommand(t *testing.T) {
	cases := []struct {
		text   string
		isPair bool
		code   string
	}{
		{"/pair ABCD2345", true, "ABCD2345"},
		{"/pair@my_bot abcd2345", true, "abcd2345"},
		{"@_user_1 /pair ABCD2345", true, "ABCD2345"},
		{"/PAIR　ABCD2345", true, "ABCD2345"},
		{"/pair", true, ""},
		{"/pairing ABCD2345", false, ""},
		{"hello", false, ""},
	}
	for _, c := range cases {
		isPair, code := parsePairCommand(c.text)
		if isPair != c.isPair || code != c.code {
			t.Errorf("parsePairCommand(%q) = (%v, %q), want (%v, %q)", c.text, isPair, code, c.isPair, c.code)
		}
	}
}
