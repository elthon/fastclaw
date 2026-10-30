package setup

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/fastclaw-ai/fastclaw/internal/auth"
	"github.com/fastclaw-ai/fastclaw/internal/users"
)

func skillZip(t *testing.T, name string) *bytes.Buffer {
	t.Helper()
	var zbuf bytes.Buffer
	zw := zip.NewWriter(&zbuf)
	f, err := zw.Create(name + "/SKILL.md")
	if err != nil {
		t.Fatal(err)
	}
	f.Write([]byte("---\nname: " + name + "\ndescription: test skill\n---\n# " + name + "\n"))
	zw.Close()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	part, _ := mw.CreateFormFile("file", name+".zip")
	part.Write(zbuf.Bytes())
	mw.Close()
	return &body
}

// A regular user can install, list and delete skills of their own
// (~/.fastclaw/users/<uid>/skills) without touching the global set.
func TestUserScopeSkillsLifecycle(t *testing.T) {
	home := t.TempDir()
	t.Setenv("FASTCLAW_HOME", home)
	s := NewServer(0)
	alice := auth.Identity{UserID: "u_alice", Role: users.RoleUser, AuthMethod: "session"}
	as := func(r *http.Request, id auth.Identity) *http.Request {
		return r.WithContext(auth.WithIdentity(r.Context(), id))
	}

	body := skillZip(t, "my-skill")
	req := httptest.NewRequest(http.MethodPost, "/api/skills/upload?scope=user", body)
	req.Header.Set("Content-Type", "multipart/form-data; boundary="+multipartBoundary(body))
	rec := httptest.NewRecorder()
	s.handleUploadSkill(rec, as(req, alice))
	if rec.Code != http.StatusOK {
		t.Fatalf("upload: %d %s", rec.Code, rec.Body.String())
	}
	if _, err := os.Stat(filepath.Join(home, "users", "u_alice", "skills", "my-skill", "SKILL.md")); err != nil {
		t.Fatalf("skill not in the user's dir: %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, "skills", "my-skill")); err == nil {
		t.Fatal("a user-scope upload must not land in the global skills")
	}

	rec = httptest.NewRecorder()
	s.handleListSkills(rec, as(httptest.NewRequest(http.MethodGet, "/api/skills?scope=user", nil), alice))
	var list []map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &list)
	if len(list) != 1 || list[0]["name"] != "my-skill" {
		t.Fatalf("user list = %s", rec.Body.String())
	}
	bob := auth.Identity{UserID: "u_bob", Role: users.RoleUser, AuthMethod: "session"}
	rec = httptest.NewRecorder()
	s.handleListSkills(rec, as(httptest.NewRequest(http.MethodGet, "/api/skills?scope=user", nil), bob))
	if rec.Body.String() != "[]\n" {
		t.Fatalf("bob must not see alice's skills: %s", rec.Body.String())
	}

	for _, bad := range []string{"..", "a\\b"} {
		del := httptest.NewRequest(http.MethodDelete, "/api/skills/x?scope=user", nil)
		del.SetPathValue("name", bad)
		rec = httptest.NewRecorder()
		s.handleDeleteSkill(rec, as(del, alice))
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("delete %q: %d", bad, rec.Code)
		}
	}
	// Without scope=user a regular user is still refused (global skills).
	del := httptest.NewRequest(http.MethodDelete, "/api/skills/my-skill", nil)
	del.SetPathValue("name", "my-skill")
	rec = httptest.NewRecorder()
	s.handleDeleteSkill(rec, as(del, alice))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("global delete by regular user: %d", rec.Code)
	}
	del = httptest.NewRequest(http.MethodDelete, "/api/skills/my-skill?scope=user", nil)
	del.SetPathValue("name", "my-skill")
	rec = httptest.NewRecorder()
	s.handleDeleteSkill(rec, as(del, alice))
	if rec.Code != http.StatusOK {
		t.Fatalf("user delete: %d %s", rec.Code, rec.Body.String())
	}
	if _, err := os.Stat(filepath.Join(home, "users", "u_alice", "skills", "my-skill")); !os.IsNotExist(err) {
		t.Fatal("skill should be removed")
	}
}

func TestUserScopeSkillsRejectEndUsers(t *testing.T) {
	t.Setenv("FASTCLAW_HOME", t.TempDir())
	s := NewServer(0)
	for _, ident := range []auth.Identity{
		{UserID: "u_app", Role: users.RoleAppUser, AuthMethod: "apikey"},
		{UserID: "u_admin", Role: users.RoleSuperAdmin, AuthMethod: "session", ActAsUserID: "u_x"},
	} {
		rec := httptest.NewRecorder()
		_, ok := s.skillInstallTarget(rec, skillInstallRequest(ident), "", true)
		if ok || rec.Code != http.StatusForbidden {
			t.Fatalf("%+v: ok=%v status=%d", ident, ok, rec.Code)
		}
	}
}

func multipartBoundary(body *bytes.Buffer) string {
	b := body.Bytes()
	end := bytes.IndexByte(b, '\r')
	return string(b[2:end])
}
