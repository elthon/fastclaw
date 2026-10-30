package setup

import (
	_ "embed"
	"net/http"
)

// integrationDoc is skills/agent-integration/SKILL.md, the guide for
// applications that use FastClaw as their agent runtime. `make bundle-docs`
// copies it here; TestIntegrationDocMatchesSkill keeps the two in sync.
//
//go:embed apidocs/agent-integration.md
var integrationDoc string

// IntegrationDocPath is where the integration skill is served — the same
// path it has in the repository, so agents can install it as a skill.
const IntegrationDocPath = "/skills/agent-integration/SKILL.md"

// handleIntegrationDoc serves GET /skills/agent-integration/SKILL.md: the integration guide for
// the coding agent (or developer) wiring an app to FastClaw. It is public —
// documentation only, no secrets — so an agent can read it from a link
// before it has a key. It is served verbatim: the base URL and API key are
// configured by the integrating app, and the guide tells it where to find
// them.
func (s *Server) handleIntegrationDoc(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	_, _ = w.Write([]byte(integrationDoc))
}
