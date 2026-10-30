package setup

import (
	"encoding/json"
	"net/http"
	"regexp"
	"strings"
)

// chatSessionID bounds what /chat/<sessionId> accepts as an id.
var chatSessionID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9:._-]{0,199}$`)

// chatTarget is what a /chat/<sessionId> URL opens: one agent's chat, or a
// group chat topic.
type chatTarget struct {
	Kind    string `json:"kind"` // "agent" | "team"
	AgentID string `json:"agentId,omitempty"`
	TeamID  string `json:"teamId,omitempty"`
}

// handleChatTarget resolves a URL session id to its agent or group for the
// caller: GET /api/chat/sessions/{sessionId}/target. The /chat/<sessionId>
// route no longer names the agent or group, so the page asks here first.
func (s *Server) handleChatTarget(w http.ResponseWriter, r *http.Request) {
	target, ok := s.resolveChatTarget(r, r.PathValue("sessionId"))
	if !ok {
		jsonResponse(w, http.StatusNotFound, map[string]any{"error": "session not found"})
		return
	}
	jsonResponse(w, http.StatusOK, target)
}

// resolveChatTarget looks the id up among the caller's sessions:
//   - a session keyed by the id → that agent's chat (loose or project),
//     including another user's session on an agent the caller owns (an
//     API end-user's chat, which the owner opens from the agent panel);
//   - group member sessions keyed "<id>-agent-<agent>" whose project is one
//     of the caller's groups → that group's topic;
//   - a group's default topic (its configured sessionId, or team-<team>), or
//     a topic recorded for a group that has no member sessions yet.
func (s *Server) resolveChatTarget(r *http.Request, sessionID string) (chatTarget, bool) {
	if s.dataStore == nil || !chatSessionID.MatchString(sessionID) {
		return chatTarget{}, false
	}
	uid := s.effectiveUserID(r)
	if uid == "" {
		return chatTarget{}, false
	}
	cfg, err := s.loadConfigForUserID(r, uid)
	if err != nil {
		return chatTarget{}, false
	}
	isTeam := func(id string) bool {
		_, ok := cfg.Teams[id]
		return id != "" && ok
	}
	locs, err := s.dataStore.FindSessionLocations(r.Context(), uid, sessionID)
	if err != nil {
		return chatTarget{}, false
	}
	for _, loc := range locs {
		if loc.SessionKey == sessionID && !isTeam(loc.ProjectID) {
			return chatTarget{Kind: "agent", AgentID: loc.AgentID}, true
		}
	}
	for _, loc := range locs {
		if loc.SessionKey == sessionID+"-agent-"+loc.AgentID && isTeam(loc.ProjectID) {
			return chatTarget{Kind: "team", TeamID: loc.ProjectID}, true
		}
	}
	for teamID, team := range cfg.Teams {
		if sessionID == team.SessionID || sessionID == "team-"+teamID {
			return chatTarget{Kind: "team", TeamID: teamID}, true
		}
	}
	rows, err := s.dataStore.ListConfigs(r.Context(), teamTopicKind, uid, "")
	if err != nil {
		return chatTarget{}, false
	}
	for _, row := range rows {
		b, _ := json.Marshal(row.Data)
		var record teamTopicRecord
		if json.Unmarshal(b, &record) != nil || record.Deleted || !isTeam(record.TeamID) {
			continue
		}
		if strings.TrimSpace(record.Snapshot.SessionID) == sessionID {
			return chatTarget{Kind: "team", TeamID: record.TeamID}, true
		}
	}
	return chatTarget{}, false
}
