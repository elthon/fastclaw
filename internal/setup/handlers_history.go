package setup

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"

	"github.com/fastclaw-ai/fastclaw/internal/config"
	"github.com/fastclaw-ai/fastclaw/internal/workspace"
)

// Workspace version history handlers. The feature is opt-in at the WRITE
// side (workspaceHistory.enabled gates the turn-boundary snapshot in the
// agent loop); reading and restoring existing history needs no toggle —
// rollback is a deliberate user action.
//
// Scope keys mirror the agent loop's commit path: chatID for loose chats,
// "<projectID>-<chatID>" for project chats.

// workspaceHistoryScope resolves the URL sessionId to the history scope key
// and the on-disk worktree. Only LocalFS-backed workspaces have on-disk
// scope dirs (probed via the LocalScoper marker, so a Metered wrapper still
// counts), so S3 deployments get a stable 503 (never a 404 that looks
// like "no history yet").
func (s *Server) workspaceHistoryScope(w http.ResponseWriter, r *http.Request, agentID, urlToken string) (scope, workTree string, ok bool) {
	ls, isLocal := s.workspaceStore.(workspace.LocalScoper)
	if !isLocal {
		jsonResponse(w, http.StatusServiceUnavailable, map[string]any{"error": "workspace history requires a local workspace store"})
		return "", "", false
	}
	// Resolved like the file list: the caller's own session, or for the
	// agent's owner any session of the agent (e.g. an API end-user's).
	projectID, chatID, found := s.workspaceFolderScope(r, agentID, urlToken, "")
	if !found || chatID == "" {
		jsonResponse(w, http.StatusNotFound, map[string]any{"error": "session not found"})
		return "", "", false
	}
	scope, workTree = historyScopeFor(ls, agentID, projectID, chatID)
	return scope, workTree, true
}

// historyScopeFor maps a chat folder to its history scope key and worktree.
func historyScopeFor(ls workspace.LocalScoper, agentID, projectID, chatID string) (scope, workTree string) {
	scope = chatID
	if projectID != "" {
		scope = projectID + "-" + chatID
	}
	workTree, _ = ls.LocalScopeDir(agentID, projectID, chatID)
	return scope, workTree
}

// parseChatFolder reads a chat folder given as an agent-relative path —
// "sessions/<chat>" or "projects/<pid>/<chat>" — as the agent-wide file
// view knows it. Each id must be one plain path segment.
func parseChatFolder(dir string) (projectID, chatID string, ok bool) {
	parts := strings.Split(strings.Trim(dir, "/"), "/")
	plain := func(seg string) bool {
		return seg != "" && seg != "." && seg != ".." && !strings.ContainsAny(seg, "\\\x00")
	}
	switch {
	case len(parts) == 2 && parts[0] == "sessions" && plain(parts[1]):
		return "", parts[1], true
	case len(parts) == 3 && parts[0] == "projects" && plain(parts[1]) && plain(parts[2]):
		return parts[1], parts[2], true
	}
	return "", "", false
}

// workspaceFolderHistoryScope resolves ?dir= (or the restore body's dir)
// for the agent-wide file view, which only the agent's owner may use.
func (s *Server) workspaceFolderHistoryScope(w http.ResponseWriter, r *http.Request, agentID, dir string) (scope, workTree string, ok bool) {
	ls, isLocal := s.workspaceStore.(workspace.LocalScoper)
	if !isLocal {
		jsonResponse(w, http.StatusServiceUnavailable, map[string]any{"error": "workspace history requires a local workspace store"})
		return "", "", false
	}
	if !s.callerOwnsAgent(r, agentID) {
		jsonResponse(w, http.StatusForbidden, map[string]any{"error": "only the agent's owner can browse its whole workspace"})
		return "", "", false
	}
	projectID, chatID, valid := parseChatFolder(dir)
	if !valid {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"error": "dir must be sessions/<chat> or projects/<project>/<chat>"})
		return "", "", false
	}
	scope, workTree = historyScopeFor(ls, agentID, projectID, chatID)
	return scope, workTree, true
}

// handleAgentFolderHistory lists one chat folder's snapshots by path
// (?dir=sessions/<chat>), for the owner's agent-wide file view, where a
// previewed file is known by its folder rather than a session id.
func (s *Server) handleAgentFolderHistory(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !s.requireAgentReadable(w, r, id) {
		return
	}
	scope, _, ok := s.workspaceFolderHistoryScope(w, r, id, r.URL.Query().Get("dir"))
	if !ok {
		return
	}
	s.writeHistoryList(w, r, scope)
}

// handleAgentFolderHistoryRestore is the restore counterpart:
// body {"dir": "sessions/<chat>", "commit": "<hash>"}.
func (s *Server) handleAgentFolderHistoryRestore(w http.ResponseWriter, r *http.Request) {
	if !s.requireWritable(w, r) {
		return
	}
	id := r.PathValue("id")
	if !s.requireAgentReadable(w, r, id) {
		return
	}
	var body struct {
		Dir    string `json:"dir"`
		Commit string `json:"commit"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"error": "invalid json: " + err.Error()})
		return
	}
	scope, workTree, ok := s.workspaceFolderHistoryScope(w, r, id, body.Dir)
	if !ok {
		return
	}
	s.restoreHistory(w, r, scope, workTree, body.Commit)
}

func (s *Server) workspaceHistoryRoot() (string, error) {
	home, err := config.HomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "workspace-history"), nil
}

// handleAgentSessionHistory lists the snapshot commits of one chat's
// workspace, newest first: {"history": [{hash, message, time}]}.
func (s *Server) handleAgentSessionHistory(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !s.requireAgentReadable(w, r, id) {
		return
	}
	scope, _, ok := s.workspaceHistoryScope(w, r, id, r.PathValue("sessionId"))
	if !ok {
		return
	}
	s.writeHistoryList(w, r, scope)
}

func (s *Server) writeHistoryList(w http.ResponseWriter, r *http.Request, scope string) {
	root, err := s.workspaceHistoryRoot()
	if err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	entries, err := workspace.NewHistory(root).List(r.Context(), scope)
	if err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	if entries == nil {
		entries = []workspace.Entry{}
	}
	jsonResponse(w, http.StatusOK, map[string]any{"history": entries})
}

// handleAgentSessionHistoryRestore checks the chat's workspace out to one
// snapshot commit. The commit hash is validated in workspace.History.
func (s *Server) handleAgentSessionHistoryRestore(w http.ResponseWriter, r *http.Request) {
	if !s.requireWritable(w, r) {
		return
	}
	id := r.PathValue("id")
	if !s.requireAgentReadable(w, r, id) {
		return
	}
	scope, workTree, ok := s.workspaceHistoryScope(w, r, id, r.PathValue("sessionId"))
	if !ok {
		return
	}
	var body struct {
		Commit string `json:"commit"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"error": "invalid json: " + err.Error()})
		return
	}
	s.restoreHistory(w, r, scope, workTree, body.Commit)
}

func (s *Server) restoreHistory(w http.ResponseWriter, r *http.Request, scope, workTree, commit string) {
	root, err := s.workspaceHistoryRoot()
	if err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	if err := workspace.NewHistory(root).Restore(r.Context(), scope, workTree, commit); err != nil {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	jsonResponse(w, http.StatusOK, map[string]any{"ok": true})
}
