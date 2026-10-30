package setup

import (
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// The served guide is a build-time copy of skills/agent-integration/SKILL.md;
// run `make bundle-docs` after editing the skill.
func TestIntegrationDocMatchesSkill(t *testing.T) {
	src, err := os.ReadFile("../../skills/agent-integration/SKILL.md")
	if err != nil {
		t.Fatal(err)
	}
	if string(src) != integrationDoc {
		t.Fatal("internal/setup/apidocs/agent-integration.md is stale: run `make bundle-docs`")
	}
}

func TestIntegrationDocServedVerbatim(t *testing.T) {
	s := NewServer(0)
	rec := httptest.NewRecorder()
	s.handleIntegrationDoc(rec, httptest.NewRequest(http.MethodGet, IntegrationDocPath, nil))
	if rec.Code != http.StatusOK || rec.Body.String() != integrationDoc ||
		!strings.HasPrefix(rec.Header().Get("Content-Type"), "text/markdown") {
		t.Fatalf("unexpected response: %d %q", rec.Code, rec.Header().Get("Content-Type"))
	}
}

func TestIntegrationDocRoutes(t *testing.T) {
	s := NewServer(0)
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+IntegrationDocPath, s.handleIntegrationDoc)
	mux.HandleFunc("GET /integration.md", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, IntegrationDocPath, http.StatusMovedPermanently)
	})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/skills/agent-integration/SKILL.md", nil))
	if rec.Code != http.StatusOK || !strings.HasPrefix(rec.Body.String(), "---\nname: agent-integration") {
		t.Fatalf("skill path: %d", rec.Code)
	}
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/integration.md", nil))
	if rec.Code != http.StatusMovedPermanently || rec.Header().Get("Location") != IntegrationDocPath {
		t.Fatalf("old path: %d %q", rec.Code, rec.Header().Get("Location"))
	}
}
