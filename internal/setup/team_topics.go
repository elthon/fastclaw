package setup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/fastclaw-ai/fastclaw/internal/store"
)

const teamTopicKind = "team_topic"

var errTeamTopicDeleted = errors.New("topic was deleted")

// Older member sessions used the injected sender envelope as their opening
// preview. Only normalize this legacy fallback; explicit topic titles are kept.
func legacyTeamTopicTitle(title, preview string) string {
	if title == "" {
		title = preview
	}
	for _, prefix := range []string{`\[User\]: `, "[User]: "} {
		if strings.HasPrefix(title, prefix) && strings.HasPrefix(preview, prefix) {
			return strings.TrimSpace(strings.TrimPrefix(title, prefix))
		}
	}
	return title
}

type teamTopicRecord struct {
	TeamID   string               `json:"teamId"`
	Snapshot teamRunSnapshot      `json:"snapshot"`
	Private  []teamPrivateMessage `json:"private,omitempty"`
	Deleted  bool                 `json:"deleted,omitempty"`
}

func teamTopicName(team, session string) string {
	b, _ := json.Marshal([]string{team, session})
	return string(b)
}
func (s *Server) saveTeamTopic(uid, team string, run *teamRun) error {
	if s.dataStore == nil {
		return nil
	}
	run.mu.Lock()
	record := teamTopicRecord{TeamID: team, Snapshot: run.snapshot, Private: run.private}
	b, err := json.Marshal(record)
	run.mu.Unlock()
	if err != nil {
		return err
	}
	var data map[string]interface{}
	if err = json.Unmarshal(b, &data); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return s.dataStore.SaveConfig(ctx, &store.ConfigRecord{Kind: teamTopicKind, Name: teamTopicName(team, record.Snapshot.SessionID), UserID: uid, Data: data, Enabled: true})
}
func (s *Server) persistTeamTopic(uid, team string, run *teamRun) {
	if err := s.saveTeamTopic(uid, team, run); err != nil {
		slog.Error("save group topic", "error", err)
		run.notice("话题保存失败，请稍后重试。")
	}
}
func (s *Server) loadTeamTopic(ctx context.Context, uid, team, session string) (*teamRun, error) {
	if s.dataStore == nil {
		return nil, nil
	}
	record, err := s.dataStore.GetConfigByName(ctx, teamTopicKind, uid, "", teamTopicName(team, session))
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	b, _ := json.Marshal(record.Data)
	var data teamTopicRecord
	if err = json.Unmarshal(b, &data); err != nil {
		return nil, err
	}
	if data.Deleted {
		return nil, errTeamTopicDeleted
	}
	if data.Snapshot.Status == "running" || len(data.Snapshot.ActiveAgents) > 0 {
		data.Snapshot.Status = "stopped"
		data.Snapshot.Phase = "服务重启，本轮已停止"
		data.Snapshot.PhaseDetail = &teamPhase{Kind: "restarted"}
		data.Snapshot.ActiveAgents = []string{}
	}
	return &teamRun{finished: true, snapshot: data.Snapshot, private: data.Private, open: map[string]int{}, cancel: func() {}}, nil
}
func (s *Server) legacyTeamHistory(members []resolvedTeamMember) []teamRunMessage {
	var result []teamRunMessage
	seen := map[string]bool{}
	for _, m := range members {
		for i, item := range m.Handle.WebChatHistory(m.SessionID) {
			role, _ := item["role"].(string)
			text, _ := item["content"].(string)
			sender, _ := item["senderName"].(string)
			if text == "" || (role != "user" && role != "assistant") || sender != "" {
				continue
			}
			turn, _ := item["groupTurnId"].(string)
			stamp := int64(0)
			switch v := item["timestamp"].(type) {
			case int64:
				stamp = v
			case float64:
				stamp = int64(v)
			case int:
				stamp = int64(v)
			}
			if role == "user" {
				key := turn
				if key == "" {
					key = fmt.Sprintf("%d/%s", stamp/5000, text)
				}
				if seen[key] {
					continue
				}
				seen[key] = true
			}
			text, _, _ = parseTeamPrivate(text)
			id := fmt.Sprintf("history-%s-%d", m.AgentID, i)
			aid := ""
			if role == "assistant" {
				role = "agent"
				aid = m.AgentID
			}
			result = append(result, teamRunMessage{ID: id, Role: role, AgentID: aid, Content: text, Timestamp: stamp, GroupTurnID: turn})
		}
	}
	sort.SliceStable(result, func(i, j int) bool { return result[i].Timestamp < result[j].Timestamp })
	return result
}
func (s *Server) handleTeamTopic(w http.ResponseWriter, r *http.Request) {
	if !s.requireWritable(w, r) {
		return
	}
	req := teamChatRequest{TeamID: r.URL.Query().Get("teamId"), SessionID: r.URL.Query().Get("sessionId")}
	members, err := s.teamRequest(r, &req)
	if err != nil {
		jsonResponse(w, 404, map[string]any{"error": err.Error()})
		return
	}
	uid := s.effectiveUserID(r)
	key := teamRunKey{uid, req.TeamID, req.SessionID}
	s.teamRunsMu.Lock()
	defer s.teamRunsMu.Unlock()
	run := s.teamRuns[key]
	if run == nil {
		run, err = s.loadTeamTopic(r.Context(), uid, req.TeamID, req.SessionID)
		if err != nil {
			jsonResponse(w, 404, map[string]any{"error": err.Error()})
			return
		}
	}
	if run == nil {
		run = &teamRun{finished: true, snapshot: teamRunSnapshot{SessionID: req.SessionID, Status: "idle", Messages: s.legacyTeamHistory(members), CompleteHistory: true}, cancel: func() {}}
	}
	snapshot := run.read()
	run.mu.Lock()
	finished := run.finished
	run.mu.Unlock()
	if !finished || snapshot.Status == "running" || len(snapshot.ActiveAgents) > 0 {
		jsonResponse(w, 409, map[string]any{"error": "请先停止当前话题"})
		return
	}
	if r.Method == http.MethodDelete {
		if s.dataStore != nil {
			err = s.dataStore.SaveConfig(r.Context(), &store.ConfigRecord{Kind: teamTopicKind, UserID: uid, Name: teamTopicName(req.TeamID, req.SessionID), Data: map[string]interface{}{"teamId": req.TeamID, "deleted": true, "snapshot": map[string]interface{}{"sessionId": req.SessionID}}})
		}
		if err == nil {
			delete(s.teamRuns, key)
		}
	} else {
		var body struct {
			Title string `json:"title"`
		}
		if json.NewDecoder(r.Body).Decode(&body) != nil || strings.TrimSpace(body.Title) == "" {
			jsonResponse(w, 400, map[string]any{"error": "话题名称不能为空"})
			return
		}
		run.mu.Lock()
		run.snapshot.Title = string([]rune(strings.TrimSpace(body.Title))[:min(120, len([]rune(strings.TrimSpace(body.Title))))])
		run.snapshot.UpdatedAt = time.Now().UnixMilli()
		run.mu.Unlock()
		err = s.saveTeamTopic(uid, req.TeamID, run)
		if err == nil {
			if s.teamRuns == nil {
				s.teamRuns = map[teamRunKey]*teamRun{}
			}
			s.teamRuns[key] = run
		}
	}
	if err != nil {
		jsonResponse(w, 500, map[string]any{"error": err.Error()})
		return
	}
	jsonResponse(w, 200, map[string]any{"ok": true})
}

// Only human-directed delivery envelopes are exposed. Member-to-member bodies
// stay in the authenticated server-side topic record.
func (s *Server) handleTeamInbox(w http.ResponseWriter, r *http.Request) {
	result := []map[string]any{}
	if s.dataStore == nil {
		jsonResponse(w, 200, map[string]any{"messages": result})
		return
	}
	uid := s.effectiveUserID(r)
	cfg, err := s.loadConfigForUserID(r, uid)
	if err != nil {
		jsonResponse(w, 500, map[string]any{"error": err.Error()})
		return
	}
	rows, err := s.dataStore.ListConfigs(r.Context(), teamTopicKind, uid, "")
	if err != nil {
		jsonResponse(w, 500, map[string]any{"error": err.Error()})
		return
	}
	for _, row := range rows {
		b, _ := json.Marshal(row.Data)
		var record teamTopicRecord
		if json.Unmarshal(b, &record) != nil || record.Deleted {
			continue
		}
		if _, exists := cfg.Teams[record.TeamID]; !exists {
			continue
		}
		for _, message := range record.Private {
			if message.Recipient != "human" {
				continue
			}
			if s.resolveAgent(r, message.Sender) == nil {
				continue
			}
			// Delivered into the sender's direct chat; legacy rows predate
			// that and still point at the old per-group inbox session.
			sessionID := message.Session
			if sessionID == "" {
				sessionID = "group-inbox-" + record.TeamID
			}
			result = append(result, map[string]any{"id": message.ID, "agentId": message.Sender, "sessionId": sessionID, "timestamp": message.Timestamp})
		}
	}
	jsonResponse(w, 200, map[string]any{"messages": result})
}
