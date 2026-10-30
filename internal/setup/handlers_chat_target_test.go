package setup

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/fastclaw-ai/fastclaw/internal/auth"
	"github.com/fastclaw-ai/fastclaw/internal/scope"
	"github.com/fastclaw-ai/fastclaw/internal/store"
	"github.com/fastclaw-ai/fastclaw/internal/users"
)

// /chat/<sessionId> resolves the id among the caller's own sessions to an
// agent chat or a group topic.
func TestResolveChatTarget(t *testing.T) {
	ctx := context.Background()
	st, err := store.NewDBStore("sqlite", "file:"+filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	for _, a := range []store.AgentRecord{{ID: "agt_a", UserID: "u_alice"}, {ID: "agt_b", UserID: "u_alice"}, {ID: "agt_x", UserID: "u_bob"}} {
		if err := st.SaveAgent(ctx, &a); err != nil {
			t.Fatal(err)
		}
	}
	if err := scope.SaveSetting(ctx, st, "u_alice", "", "teams", map[string]interface{}{
		"tm-1": map[string]interface{}{"name": "g", "agents": []string{"agt_a", "agt_b"}},
	}); err != nil {
		t.Fatal(err)
	}
	for _, sess := range []struct{ user, agent, key, project string }{
		{"u_alice", "agt_a", "s-1-loose", ""},
		{"u_alice", "agt_a", "s-2-proj", "p_1"},
		{"u_alice", "agt_a", "g-3-topic-agent-agt_a", "tm-1"},
		{"u_alice", "agt_b", "g-3-topic-agent-agt_b", "tm-1"},
		{"u_alice", "agt_b", "group-inbox-tm-1", ""},
		{"u_bob", "agt_x", "s-9-bobs", ""},
		{"u_enduser", "agt_a", "snapok:edit-image:abc", ""}, // API end-user on alice's agent
		{"u_enduser", "agt_x", "snapok:other", ""},          // end-user on bob's agent
	} {
		if err := st.SaveSession(ctx, sess.user, sess.agent, sess.key, &store.SessionRecord{Channel: "web", ChatID: sess.key, ProjectID: sess.project}); err != nil {
			t.Fatal(err)
		}
	}
	s := &Server{dataStore: st}
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r = r.WithContext(auth.WithIdentity(r.Context(), auth.Identity{UserID: "u_alice", Role: users.RoleUser, AuthMethod: "session"}))

	cases := []struct {
		id   string
		want chatTarget
		ok   bool
	}{
		{"s-1-loose", chatTarget{Kind: "agent", AgentID: "agt_a"}, true},
		{"s-2-proj", chatTarget{Kind: "agent", AgentID: "agt_a"}, true},
		{"g-3-topic", chatTarget{Kind: "team", TeamID: "tm-1"}, true},
		{"group-inbox-tm-1", chatTarget{Kind: "agent", AgentID: "agt_b"}, true},
		{"team-tm-1", chatTarget{Kind: "team", TeamID: "tm-1"}, true},
		{"s-9-bobs", chatTarget{}, false},                                            // another user's session
		{"snapok:edit-image:abc", chatTarget{Kind: "agent", AgentID: "agt_a"}, true}, // alice owns agt_a
		{"snapok:other", chatTarget{}, false},                                        // agt_x isn't alice's
		{"g-3", chatTarget{}, false},                                                 // a prefix of a topic id
		{"g_3-topic", chatTarget{}, false},                                           // "_" is not a LIKE wildcard
		{"../etc/passwd", chatTarget{}, false},
	}
	for _, c := range cases {
		got, ok := s.resolveChatTarget(r, c.id)
		if ok != c.ok || got != c.want {
			t.Errorf("%s: got (%+v, %v), want (%+v, %v)", c.id, got, ok, c.want, c.ok)
		}
	}
}
