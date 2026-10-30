package setup

import (
	"context"
	"net/http"
	"time"
)

type chatTurnState struct {
	cancel  context.CancelFunc
	status  string
	updated time.Time
}

func (s *Server) beginChatTurn(key teamRunKey, cancel context.CancelFunc) bool {
	s.chatTurnsMu.Lock()
	defer s.chatTurnsMu.Unlock()
	if s.chatTurns == nil {
		s.chatTurns = make(map[teamRunKey]*chatTurnState)
	}
	for k, v := range s.chatTurns {
		if v.status != "running" && time.Since(v.updated) > time.Hour {
			delete(s.chatTurns, k)
		}
	}
	if current := s.chatTurns[key]; current != nil && current.status == "running" {
		return false
	}
	s.chatTurns[key] = &chatTurnState{cancel: cancel, status: "running", updated: time.Now()}
	return true
}
func (s *Server) endChatTurn(key teamRunKey, ctx context.Context, failed bool) {
	s.chatTurnsMu.Lock()
	defer s.chatTurnsMu.Unlock()
	if run := s.chatTurns[key]; run != nil {
		run.status = "completed"
		if ctx.Err() == context.Canceled {
			run.status = "stopped"
		} else if failed || ctx.Err() == context.DeadlineExceeded {
			run.status = "failed"
		}
		run.updated = time.Now()
	}
}
func (s *Server) handleChatStop(w http.ResponseWriter, r *http.Request) {
	if !s.requireWritable(w, r) {
		return
	}
	ag := s.resolveAgent(r, r.URL.Query().Get("agentId"))
	if ag == nil {
		jsonResponse(w, 404, map[string]any{"error": "agent not found"})
		return
	}
	key := teamRunKey{s.effectiveUserID(r), ag.Name(), s.chatEventSessionID(r, ag.Name(), r.URL.Query().Get("sessionId"))}
	s.chatTurnsMu.Lock()
	run := s.chatTurns[key]
	if run != nil && run.status == "running" {
		run.cancel()
	}
	s.chatTurnsMu.Unlock()
	jsonResponse(w, 200, map[string]any{"ok": true})
}

// Bookmarked URLs may use the opaque DB key while a new chat starts with
// its web chat ID. Both must observe and cancel the same executing turn.
func (s *Server) chatEventSessionID(r *http.Request, agentID, sessionID string) string {
	if s.dataStore != nil {
		channel, _, chatID, err := s.dataStore.LookupSessionTriple(r.Context(), s.effectiveUserID(r), agentID, sessionID)
		if err == nil && channel == "web" && chatID != "" {
			return chatID
		}
	}
	return sessionID
}
