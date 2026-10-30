package setup

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/fastclaw-ai/fastclaw/internal/auth"
	"github.com/fastclaw-ai/fastclaw/internal/store"
	"github.com/fastclaw-ai/fastclaw/internal/users"
)

// The first message of a new chat uploads before the chat request creates
// the session; the upload must land where that chat will look, without
// letting a caller write into a session someone else already has.
func TestNewChatUploadScope(t *testing.T) {
	ctx := context.Background()
	st, err := store.NewDBStore("sqlite", "file:"+filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if err := st.SaveSession(ctx, "u_bob", "agt_1", "s-1700000000000-taken", &store.SessionRecord{Channel: "web", ChatID: "s-1700000000000-taken"}); err != nil {
		t.Fatal(err)
	}
	if err := st.SaveProject(ctx, &store.ProjectRecord{UserID: "u_alice", AgentID: "agt_1", ID: "p_mine", Name: "mine"}); err != nil {
		t.Fatal(err)
	}
	if err := st.SaveProject(ctx, &store.ProjectRecord{UserID: "u_bob", AgentID: "agt_1", ID: "p_bobs", Name: "bob's"}); err != nil {
		t.Fatal(err)
	}
	s := &Server{dataStore: st}
	alice := httptest.NewRequest(http.MethodPost, "/", nil)
	alice = alice.WithContext(auth.WithIdentity(alice.Context(), auth.Identity{UserID: "u_alice", Role: users.RoleUser, AuthMethod: "session"}))

	cases := []struct {
		name, key, project, wantSession, wantProject string
	}{
		{"new loose chat", "s-1700000000001-abc123", "", "s-1700000000001-abc123", ""},
		{"new chat in own project", "s-1700000000001-abc123", "p_mine", "", "p_mine"},
		{"someone else's project", "s-1700000000001-abc123", "p_bobs", "", ""},
		{"key already in use", "s-1700000000000-taken", "", "", ""},
		{"not a web session key", "../../etc", "", "", ""},
		{"empty key", "", "", "", ""},
	}
	for _, c := range cases {
		sid, pid := s.newChatUploadScope(alice, "agt_1", c.key, c.project)
		if sid != c.wantSession || pid != c.wantProject {
			t.Errorf("%s: got (%q, %q), want (%q, %q)", c.name, sid, pid, c.wantSession, c.wantProject)
		}
	}
}

// "Open folder" opens exactly the folder the panel lists: the session's
// own folder (also for the owner looking at an API end-user's session),
// never the agent root as a fallback, and the agent root only for the
// owner.
func TestRevealScope(t *testing.T) {
	ctx := context.Background()
	st, err := store.NewDBStore("sqlite", "file:"+filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if err := st.SaveAgent(ctx, &store.AgentRecord{ID: "agt_1", UserID: "u_owner", Name: "a", IsPublic: true}); err != nil {
		t.Fatal(err)
	}
	for _, sess := range []struct{ user, key, chat, project string }{
		{"u_owner", "s-1-own", "s-1-own", ""},
		{"u_owner", "s-2-proj", "s-2-proj", "p_1"},
		{"u_enduser", "snapok:edit-image:abc", "edit-image-abc", ""},
	} {
		if err := st.SaveSession(ctx, sess.user, "agt_1", sess.key, &store.SessionRecord{Channel: "web", ChatID: sess.chat, ProjectID: sess.project}); err != nil {
			t.Fatal(err)
		}
	}
	s := &Server{dataStore: st}
	as := func(uid string) *http.Request {
		r := httptest.NewRequest(http.MethodPost, "/", nil)
		return r.WithContext(auth.WithIdentity(r.Context(), auth.Identity{UserID: uid, Role: users.RoleUser, AuthMethod: "session"}))
	}

	cases := []struct {
		name, user, session, project string
		wantProject, wantChat        string
		wantOK                       bool
	}{
		{"own loose session", "u_owner", "s-1-own", "", "", "s-1-own", true},
		{"own project session", "u_owner", "s-2-proj", "", "p_1", "s-2-proj", true},
		{"owner opens an end-user's session", "u_owner", "snapok:edit-image:abc", "", "", "edit-image-abc", true},
		{"project landing", "u_owner", "", "p_1", "p_1", "", true},
		{"agent root for the owner", "u_owner", "", "", "", "", true},
		{"agent root for a viewer", "u_viewer", "", "", "", "", false},
		{"viewer can't open someone else's session", "u_viewer", "snapok:edit-image:abc", "", "", "", false},
		{"unknown session doesn't widen to the root", "u_owner", "s-9-missing", "", "", "", false},
	}
	for _, c := range cases {
		pid, chat, ok := s.workspaceFolderScope(as(c.user), "agt_1", c.session, c.project)
		if ok != c.wantOK || pid != c.wantProject || chat != c.wantChat {
			t.Errorf("%s: got (%q, %q, %v), want (%q, %q, %v)", c.name, pid, chat, ok, c.wantProject, c.wantChat, c.wantOK)
		}
	}
}

func TestParseChatFolder(t *testing.T) {
	for dir, want := range map[string][3]string{
		"sessions/s-1-a":           {"", "s-1-a", "ok"},
		"sessions/s-1-a/":          {"", "s-1-a", "ok"},
		"projects/p_1/s-1-a":       {"p_1", "s-1-a", "ok"},
		"sessions/..":              {"", "", ""},
		"sessions/s-1-a/sub":       {"", "", ""},
		"projects/p_1":             {"", "", ""},
		"skills/foo":               {"", "", ""},
		"projects/../sessions/s-1": {"", "", ""},
	} {
		pid, chat, ok := parseChatFolder(dir)
		if pid != want[0] || chat != want[1] || ok != (want[2] == "ok") {
			t.Errorf("%q: got (%q, %q, %v), want %v", dir, pid, chat, ok, want)
		}
	}
}
