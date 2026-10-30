package setup

import (
	"net/http"
	"os"

	"github.com/fastclaw-ai/fastclaw/internal/buildinfo"
	"github.com/fastclaw-ai/fastclaw/internal/daemon"
)

// handleRevealLogs opens the gateway's log directory (~/.fastclaw/logs)
// in the operator's native file browser, for the About page. Like the
// workspace reveal it is self-hosted only: on a hosted deploy the
// server's filesystem isn't the operator's machine.
func (s *Server) handleRevealLogs(w http.ResponseWriter, r *http.Request) {
	if buildinfo.IsHostedDeploy() {
		jsonResponse(w, http.StatusForbidden, map[string]any{"error": "opening the log directory is disabled on hosted deployments"})
		return
	}
	_, _, dir, err := daemon.Paths()
	if err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	// A foreground gateway may not have written a log yet; an empty
	// folder still opens instead of erroring.
	if err := os.MkdirAll(dir, 0o755); err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	if err := openInFileBrowser(dir); err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	jsonResponse(w, http.StatusOK, map[string]any{"ok": true, "path": dir})
}
