package setup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/fastclaw-ai/fastclaw/internal/agent"
	"github.com/fastclaw-ai/fastclaw/internal/channels"
	"golang.org/x/text/unicode/norm"
)

// A run belongs to a user, group and topic, never to an HTTP connection.
type teamRunKey struct{ User, Team, Session string }
type teamRunMessage struct {
	Attachments []attachmentRequest `json:"attachments,omitempty"`
	ImageURLs   []string            `json:"imageUrls,omitempty"`
	ID          string              `json:"id"`
	Role        string              `json:"role"`
	Content     string              `json:"content"`
	Timestamp   int64               `json:"timestamp"`
	AgentID     string              `json:"agentId,omitempty"`
	GroupTurnID string              `json:"groupTurnId"`
	// Deliveries are envelopes of the private messages this member sent
	// alongside the message: the group shows "sent privately to …" but
	// never the body, which reaches only the sender and recipient (the
	// recipient's privateInbox, or the human's direct chat).
	Deliveries []teamDelivery `json:"deliveries,omitempty"`
}

type teamDelivery struct {
	ID            string `json:"id"`
	RecipientID   string `json:"recipientId"`
	RecipientName string `json:"recipientName"`
	Intent        string `json:"intent,omitempty"`
	// Session is set for human-addressed deliveries: the direct chat the
	// body landed in, so the receipt can link there.
	Session string `json:"session,omitempty"`
}
type teamRunSnapshot struct {
	CompleteHistory bool       `json:"completeHistory"`
	Phase           string     `json:"phase,omitempty"`
	PhaseDetail     *teamPhase `json:"phaseDetail,omitempty"`
	Rounds          int        `json:"rounds,omitempty"`
	Limited         bool       `json:"limited,omitempty"`
	SessionID       string     `json:"sessionId"`
	TurnID          string     `json:"turnId"`
	Status          string     `json:"status"`
	Title           string     `json:"title"`
	// Preview is the latest message ("Name: text"), set only on topic
	// listings so the chat list can show it without the transcript.
	Preview      string           `json:"preview,omitempty"`
	UpdatedAt    int64            `json:"updatedAt"`
	ActiveAgents []string         `json:"activeAgents"`
	Messages     []teamRunMessage `json:"messages"`
}
type teamRun struct {
	finished bool // completion checkpoint is committed before a new turn may start
	private  []teamPrivateMessage
	mu       sync.Mutex
	snapshot teamRunSnapshot
	cancel   context.CancelFunc
	// Each member may send several bubbles in a single turn.
	open map[string]int
}

func (run *teamRun) read() teamRunSnapshot {
	run.mu.Lock()
	defer run.mu.Unlock()
	result := run.snapshot
	result.Messages = append([]teamRunMessage{}, result.Messages...)
	for i := range result.Messages {
		if result.Messages[i].Role == "agent" {
			_, streaming := run.open[result.Messages[i].AgentID]
			result.Messages[i].Content = strings.Join(splitTeamReplyText(result.Messages[i].Content, streaming), "\n\n")
		}
	}
	result.ActiveAgents = append([]string{}, result.ActiveAgents...)
	return result
}

// attachDeliveries records a member's private messages as a receipt on
// the last message it produced this turn — or on an empty receipt-only
// message when the reply was entirely private.
func (run *teamRun) attachDeliveries(out teamOutcome, members []resolvedTeamMember, humanName string) {
	if len(out.private) == 0 {
		return
	}
	deliveries := make([]teamDelivery, 0, len(out.private))
	for _, p := range out.private {
		name := humanName
		if p.Recipient != "human" {
			for _, m := range members {
				if m.AgentID == p.Recipient {
					name = m.Name
				}
			}
		}
		deliveries = append(deliveries, teamDelivery{ID: p.ID, RecipientID: p.Recipient, RecipientName: name, Intent: p.Intent, Session: p.Session})
	}
	run.mu.Lock()
	defer run.mu.Unlock()
	if len(out.messages) > 0 {
		last := out.messages[len(out.messages)-1].ID
		for i := range run.snapshot.Messages {
			if run.snapshot.Messages[i].ID == last {
				run.snapshot.Messages[i].Deliveries = append(run.snapshot.Messages[i].Deliveries, deliveries...)
				return
			}
		}
	}
	run.snapshot.Messages = append(run.snapshot.Messages, teamRunMessage{
		ID:          fmt.Sprintf("%s-%s-private-%d", run.snapshot.TurnID, out.member.AgentID, len(run.snapshot.Messages)),
		Role:        "agent",
		AgentID:     out.member.AgentID,
		Timestamp:   time.Now().UnixMilli(),
		GroupTurnID: run.snapshot.TurnID,
		Deliveries:  deliveries,
	})
}

// latestDirectSession is the agent's direct chat in the sense of the
// sidebar: its most recently active plain web session (no project, not a
// group-member or inbox session). With none yet, a new session id.
func latestDirectSession(handle AgentHandle) string {
	best, bestAt := "", int64(-1)
	for _, entry := range handle.WebChatSessions() {
		id := entry.ChatID
		if id == "" {
			id = entry.ID
		}
		if entry.ProjectID != "" || (entry.Channel != "" && entry.Channel != "web") ||
			strings.HasPrefix(id, "team-") || strings.HasPrefix(id, "group-inbox-") {
			continue
		}
		if at := entry.UpdatedAt; at > bestAt {
			best, bestAt = id, at
		}
	}
	if best != "" {
		return best
	}
	return fmt.Sprintf("s-%d-%s", time.Now().UnixMilli(), strconv.FormatInt(rand.Int63n(36*36*36*36*36*36), 36))
}

func (run *teamRun) event(member string, env agent.EventEnvelope) {
	run.mu.Lock()
	defer run.mu.Unlock()
	e := env.Event
	if e.Type == "error" {
		run.snapshot.Status = "failed"
	}
	text, _ := e.Data["content"].(string)
	if e.Type == "content_delta" {
		text, _ = e.Data["delta"].(string)
	}
	if e.Type == "error" {
		text, _ = e.Data["message"].(string)
	}
	if e.Type != "content" && e.Type != "content_delta" && e.Type != "group_partial" && e.Type != "error" {
		return
	}
	index, exists := run.open[member]
	if !exists && text == "" {
		return
	}
	if !exists || e.Type == "error" {
		index = len(run.snapshot.Messages)
		run.snapshot.Messages = append(run.snapshot.Messages, teamRunMessage{
			ID: fmt.Sprintf("%s-%s-%d", run.snapshot.TurnID, member, index), Role: "agent", AgentID: member,
			Timestamp: time.Now().UnixMilli(), GroupTurnID: run.snapshot.TurnID,
		})
		run.open[member] = index
	}
	if e.Type == "content_delta" {
		run.snapshot.Messages[index].Content += text
	} else {
		run.snapshot.Messages[index].Content = text
		if e.Type != "group_partial" {
			delete(run.open, member)
		}
	}
	if e.Type == "content" {
		parts := splitTeamReply(text)
		run.snapshot.Messages[index].Content = ""
		if len(parts) > 0 {
			run.snapshot.Messages[index].Content = parts[0]
			for _, part := range parts[1:] {
				message := run.snapshot.Messages[index]
				message.ID = fmt.Sprintf("%s-%s-%d", run.snapshot.TurnID, member, len(run.snapshot.Messages))
				message.Content = part
				run.snapshot.Messages = append(run.snapshot.Messages, message)
			}
		}
	}
	run.snapshot.UpdatedAt = time.Now().UnixMilli()
}

func (s *Server) teamRequest(r *http.Request, req *teamChatRequest) ([]resolvedTeamMember, error) {
	cfg, err := s.loadConfigForUserID(r, s.effectiveUserID(r))
	if err != nil {
		return nil, err
	}
	team, exists := cfg.Teams[req.TeamID]
	if !exists {
		return nil, fmt.Errorf("group chat not found")
	}
	if req.SessionID == "" {
		req.SessionID = team.SessionID
		if req.SessionID == "" {
			req.SessionID = "team-" + req.TeamID
		}
	}
	// New topics use a short id shaped like private chats' (g-<ms>-<rand>);
	// the group is tied to its member sessions by project_id, not by the id.
	// Older ids (team-<team>[-topic-<uuid>]) stay valid. Anything else is
	// refused: client-supplied member session IDs are never trusted.
	if req.SessionID != team.SessionID && req.SessionID != "team-"+req.TeamID &&
		!strings.HasPrefix(req.SessionID, "team-"+req.TeamID+"-topic-") && !teamTopicID.MatchString(req.SessionID) {
		return nil, fmt.Errorf("topic does not belong to this group")
	}
	req.Name = team.Name
	req.Description = team.Description
	req.HumanName = team.HumanName
	if req.HumanName == "" {
		req.HumanName = "用户"
	}
	req.Members = nil
	for _, id := range team.Agents {
		req.Members = append(req.Members, teamChatMember{AgentID: id})
	}
	members := s.resolveTeamMembers(r, *req)
	if len(members) == 0 {
		return nil, fmt.Errorf("no accessible team agents")
	}
	// The configured lead coordinates unaddressed tasks and final review.
	for i, m := range members {
		if m.AgentID == team.DefaultAgent {
			members[0], members[i] = members[i], members[0]
			break
		}
	}
	return members, nil
}
func (s *Server) startTeamRun(r *http.Request, req teamChatRequest) (*teamRun, int, error) {
	uid := s.effectiveUserID(r)
	if uid == "" {
		return nil, http.StatusUnauthorized, fmt.Errorf("unauthorized")
	}
	if strings.TrimSpace(req.Message) == "" {
		return nil, http.StatusBadRequest, fmt.Errorf("message required")
	}
	members, err := s.teamRequest(r, &req)
	if err != nil {
		return nil, http.StatusBadRequest, err
	}
	key := teamRunKey{uid, req.TeamID, req.SessionID}
	s.teamRunsMu.Lock()
	defer s.teamRunsMu.Unlock()
	if s.teamRuns == nil {
		s.teamRuns = make(map[teamRunKey]*teamRun)
	}
	for cachedKey, cached := range s.teamRuns {
		cached.mu.Lock()
		expired := cached.finished && time.Now().UnixMilli()-cached.snapshot.UpdatedAt > time.Hour.Milliseconds()
		cached.mu.Unlock()
		if expired && s.dataStore != nil {
			delete(s.teamRuns, cachedKey)
		}
	}
	previous := s.teamRuns[key]
	if previous == nil {
		previous, err = s.loadTeamTopic(r.Context(), uid, req.TeamID, req.SessionID)
		if err != nil {
			return nil, http.StatusBadRequest, err
		}
	}
	if previous != nil {
		snapshot := previous.read()
		previous.mu.Lock()
		finished := previous.finished
		previous.mu.Unlock()
		if !finished || snapshot.Status == "running" || len(snapshot.ActiveAgents) > 0 {
			return nil, http.StatusConflict, fmt.Errorf("this topic is already running")
		}
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), agentTurnTimeout)
	turnID := fmt.Sprintf("%s-%d", req.TeamID, time.Now().UnixNano())
	params := make(map[string]any, len(req.Params)+1)
	for k, v := range req.Params {
		params[k] = v
	}
	params["__fastclawGroupTurnId"] = turnID
	req.Params = params
	now := time.Now().UnixMilli()
	run := &teamRun{cancel: cancel, open: make(map[string]int), snapshot: teamRunSnapshot{
		SessionID: req.SessionID, TurnID: turnID, Status: "running", Title: req.Message, UpdatedAt: now,
		ActiveAgents: []string{}, Messages: []teamRunMessage{{ID: turnID + "-user", Role: "user", Attachments: append([]attachmentRequest{}, req.Attachments...), ImageURLs: append(append([]string{}, req.ImageURLs...), req.Images...), Content: req.Message, Timestamp: now, GroupTurnID: turnID}},
	}}
	run.snapshot.CompleteHistory = true
	if previous != nil {
		old := previous.read()
		run.snapshot.Title = old.Title
		run.snapshot.Messages = append(old.Messages, run.snapshot.Messages...)
		previous.mu.Lock()
		run.private = append([]teamPrivateMessage{}, previous.private...)
		previous.mu.Unlock()
	} else {
		run.snapshot.Messages = append(s.legacyTeamHistory(members), run.snapshot.Messages...)
	}
	reserved := make([]teamRunKey, 0, len(members))
	for _, member := range members {
		memberKey := teamRunKey{uid, member.AgentID, member.SessionID}
		if !s.beginChatTurn(memberKey, cancel) {
			cancel()
			for _, acquired := range reserved {
				s.endChatTurn(acquired, ctx, false)
			}
			return nil, http.StatusConflict, fmt.Errorf("a member session in this topic is already running")
		}
		reserved = append(reserved, memberKey)
	}
	if err = s.saveTeamTopic(uid, req.TeamID, run); err != nil {
		cancel()
		for _, key := range reserved {
			s.endChatTurn(key, ctx, true)
		}
		return nil, http.StatusInternalServerError, err
	}
	s.teamRuns[key] = run
	go s.executeTeamRun(r.Clone(ctx), req, members, uid, run)
	return run, http.StatusAccepted, nil
}
func (s *Server) executeTeamRun(r *http.Request, req teamChatRequest, members []resolvedTeamMember, uid string, run *teamRun) {
	defer run.cancel()
	defer func() {
		recovered := recover()
		// Serialize the final checkpoint with admission for the next turn;
		// an older completion must never overwrite a newer running snapshot.
		s.teamRunsMu.Lock()
		defer s.teamRunsMu.Unlock()
		run.mu.Lock()
		if recovered != nil {
			run.snapshot.Status = "failed"
		}
		switch r.Context().Err() {
		case context.Canceled:
			run.snapshot.Status = "stopped"
		case context.DeadlineExceeded:
			run.snapshot.Status = "failed"
		default:
			if run.snapshot.Status == "running" {
				run.snapshot.Status = "completed"
			}
		}

		for _, member := range members {
			s.endChatTurn(teamRunKey{uid, member.AgentID, member.SessionID}, r.Context(), run.snapshot.Status == "failed")
		}
		run.snapshot.ActiveAgents = []string{}
		run.snapshot.UpdatedAt = time.Now().UnixMilli()
		run.snapshot.Phase = ""
		run.snapshot.PhaseDetail = nil
		run.mu.Unlock()
		s.persistTeamTopic(uid, req.TeamID, run)
		run.mu.Lock()
		run.finished = true
		run.mu.Unlock()
	}()
	s.runTeamConversation(r, uid, req, members, run)
}

func (s *Server) handleTeamChatRun(w http.ResponseWriter, r *http.Request) {
	if !s.requireWritable(w, r) {
		return
	}
	var req teamChatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonResponse(w, 400, map[string]any{"error": err.Error()})
		return
	}
	run, status, err := s.startTeamRun(r, req)
	if err != nil {
		jsonResponse(w, status, map[string]any{"error": err.Error()})
		return
	}
	jsonResponse(w, status, run.read())
}
func (s *Server) lookupTeamRun(r *http.Request) (*teamRun, error) {
	req := teamChatRequest{TeamID: r.URL.Query().Get("teamId"), SessionID: r.URL.Query().Get("sessionId")}
	if _, err := s.teamRequest(r, &req); err != nil {
		return nil, err
	}
	s.teamRunsMu.Lock()
	defer s.teamRunsMu.Unlock()
	key := teamRunKey{s.effectiveUserID(r), req.TeamID, req.SessionID}
	if run := s.teamRuns[key]; run != nil {
		return run, nil
	}
	return s.loadTeamTopic(r.Context(), key.User, key.Team, key.Session)
}
func (s *Server) handleTeamRun(w http.ResponseWriter, r *http.Request) {
	run, err := s.lookupTeamRun(r)
	if err != nil {
		if errors.Is(err, errTeamTopicDeleted) {
			jsonResponse(w, http.StatusGone, map[string]any{"error": err.Error(), "code": "team_topic_deleted"})
			return
		}
		jsonResponse(w, 404, map[string]any{"error": err.Error()})
		return
	}
	if run == nil {
		jsonResponse(w, 200, map[string]any{"run": nil})
		return
	}
	jsonResponse(w, 200, map[string]any{"run": run.read()})
}
func (s *Server) handleTeamStop(w http.ResponseWriter, r *http.Request) {
	if !s.requireWritable(w, r) {
		return
	}
	run, err := s.lookupTeamRun(r)
	if err != nil {
		jsonResponse(w, 404, map[string]any{"error": err.Error()})
		return
	}
	if run != nil {
		run.cancel()
	}
	jsonResponse(w, 200, map[string]any{"ok": true})
}
func (s *Server) handleTeamTopics(w http.ResponseWriter, r *http.Request) {
	req := teamChatRequest{TeamID: r.URL.Query().Get("teamId")}
	members, err := s.teamRequest(r, &req)
	if err != nil {
		jsonResponse(w, 404, map[string]any{"error": err.Error()})
		return
	}
	topics := map[string]teamRunSnapshot{}
	for _, m := range members {
		for _, entry := range m.Handle.WebChatSessions() {
			memberSession := entry.ChatID
			if memberSession == "" {
				memberSession = entry.ID
			}
			if entry.ProjectID != req.TeamID || !strings.HasSuffix(memberSession, "-agent-"+m.AgentID) {
				continue
			}
			id := strings.TrimSuffix(memberSession, "-agent-"+m.AgentID)
			if old, ok := topics[id]; !ok || entry.UpdatedAt > old.UpdatedAt {
				title := legacyTeamTopicTitle(entry.Title, entry.Preview)
				preview := entry.LastMessage
				if preview == "" {
					preview = entry.Preview
				}
				topics[id] = teamRunSnapshot{SessionID: id, Title: title, Preview: compactTeamPreview(preview), Status: "idle", UpdatedAt: entry.UpdatedAt, ActiveAgents: []string{}}
			}
		}
	}
	if s.dataStore != nil {
		rows, loadErr := s.dataStore.ListConfigs(r.Context(), teamTopicKind, s.effectiveUserID(r), "")
		if loadErr != nil {
			jsonResponse(w, 500, map[string]any{"error": loadErr.Error()})
			return
		}
		for _, row := range rows {
			b, _ := json.Marshal(row.Data)
			var record teamTopicRecord
			if json.Unmarshal(b, &record) != nil || record.TeamID != req.TeamID {
				continue
			}
			id := record.Snapshot.SessionID
			if record.Deleted {
				delete(topics, id)
				continue
			}
			snapshot := record.Snapshot
			snapshot.Preview = teamMessagesPreview(snapshot.Messages, members)
			snapshot.Messages = nil
			if snapshot.Status == "running" || len(snapshot.ActiveAgents) > 0 {
				snapshot.Status = "stopped"
				snapshot.ActiveAgents = []string{}
			}
			topics[id] = snapshot
		}
	}
	s.teamRunsMu.Lock()
	for k, run := range s.teamRuns {
		if k.User == s.effectiveUserID(r) && k.Team == req.TeamID {
			snapshot := run.read()
			snapshot.Preview = teamMessagesPreview(snapshot.Messages, members)
			snapshot.Messages = nil
			topics[k.Session] = snapshot
		}
	}
	s.teamRunsMu.Unlock()
	result := make([]teamRunSnapshot, 0, len(topics))
	for _, topic := range topics {
		result = append(result, topic)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].UpdatedAt > result[j].UpdatedAt })
	jsonResponse(w, 200, map[string]any{"topics": result})
}

// teamMessagesPreview summarizes a topic's latest visible message for the
// chat list: agent turns are prefixed with the member's name.
func teamMessagesPreview(messages []teamRunMessage, members []resolvedTeamMember) string {
	for i := len(messages) - 1; i >= 0; i-- {
		m := messages[i]
		text := compactTeamPreview(m.Content)
		if text == "" || (m.Role != "agent" && m.Role != "user") {
			continue
		}
		if m.Role == "agent" {
			for _, member := range members {
				if member.AgentID == m.AgentID && member.Name != "" {
					return member.Name + ": " + text
				}
			}
		}
		return text
	}
	return ""
}

// compactTeamPreview flattens a message into one short line, folding
// multi-bubble split markers into spaces.
func compactTeamPreview(text string) string {
	text = strings.Join(strings.Fields(strings.ReplaceAll(text, channels.SplitMessageMarker, " ")), " ")
	if runes := []rune(text); len(runes) > 100 {
		return string(runes[:100]) + "..."
	}
	return text
}

// Backwards-compatible SSE transport; disconnecting only drops this observer.
func (s *Server) handleTeamChatStream(w http.ResponseWriter, r *http.Request) {
	if !s.requireWritable(w, r) {
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		jsonResponse(w, 500, map[string]any{"error": "streaming unsupported"})
		return
	}
	var req teamChatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonResponse(w, 400, map[string]any{"error": err.Error()})
		return
	}
	run, status, err := s.startTeamRun(r, req)
	if err != nil {
		jsonResponse(w, status, map[string]any{"error": err.Error()})
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	flusher.Flush()
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	sent := map[string]string{}
	for {
		snapshot := run.read()
		for _, m := range snapshot.Messages {
			if m.Role != "agent" || sent[m.ID] == m.Content {
				continue
			}
			sent[m.ID] = m.Content
			data, _ := json.Marshal(map[string]any{"type": "content", "agentId": m.AgentID, "data": map[string]any{"content": m.Content}})
			fmt.Fprintf(w, "data: %s\n\n", data)
		}
		if snapshot.Status != "running" && len(snapshot.ActiveAgents) == 0 {
			fmt.Fprint(w, "data: {\"type\":\"done\"}\n\n")
			flusher.Flush()
			return
		}
		flusher.Flush()
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
		}
	}
}

// teamTopicID matches a group topic id the web client mints:
// g-<unix ms>-<base36>, like a private chat's s-<unix ms>-<base36>.
var teamTopicID = regexp.MustCompile(`^g-[0-9]+-[a-z0-9]+$`)

var teamFence = regexp.MustCompile("^ {0,3}(`{3,}|~{3,})")

var teamInlineCode = regexp.MustCompile("`[^`\\n]*`|\\[[^\\]]*\\]\\([^)]*\\)|<!--[\\s\\S]*?-->")

func teamMentions(content string, members []resolvedTeamMember) ([]resolvedTeamMember, bool) {
	// Route only prose: quoted messages, examples, URLs and email addresses
	// must not unexpectedly summon a teammate.
	var prose []string
	fence := ""
	for _, line := range strings.Split(content, "\n") {
		trim := strings.TrimSpace(line)
		if match := teamFence.FindStringSubmatch(line); len(match) > 0 {
			marker := match[1]
			if fence == "" {
				fence = marker
			} else if marker[0] == fence[0] && len(marker) >= len(fence) {
				fence = ""
			}
			continue
		}
		if fence != "" || strings.HasPrefix(trim, ">") {
			continue
		}
		prose = append(prose, line)
	}
	text := []rune(strings.ToLower(norm.NFKC.String(teamInlineCode.ReplaceAllString(strings.Join(prose, "\n"), ""))))
	boundary := func(r rune) bool {
		return unicode.IsSpace(r) || strings.ContainsRune(",，.。!！?？:：;；、()（）[]{}<>\"“”'‘’「」", r)
	}
	type label struct {
		name  []rune
		index int
	}
	labels := []label{}
	for i, m := range members {
		for _, name := range []string{m.Name, m.AgentID} {
			if name != "" {
				labels = append(labels, label{[]rune(strings.ToLower(norm.NFKC.String(name))), i})
			}
		}
	}
	for _, name := range []string{"all", "everyone", "所有人", "所有成员", "全体成员", "大家"} {
		labels = append(labels, label{[]rune(name), -1})
	}
	sort.SliceStable(labels, func(i, j int) bool { return len(labels[i].name) > len(labels[j].name) })
	found := map[int]bool{}
	result := []resolvedTeamMember{}
	all := false
	for i, r := range text {
		if r != '@' || (i > 0 && !boundary(text[i-1])) {
			continue
		}
		for _, label := range labels {
			end := i + 1 + len(label.name)
			if end > len(text) || string(text[i+1:end]) != string(label.name) || (end < len(text) && !boundary(text[end])) {
				continue
			}
			if label.index < 0 {
				all = true
			} else if !found[label.index] {
				found[label.index] = true
				result = append(result, members[label.index])
			}
			break
		}
	}
	return result, all
}
