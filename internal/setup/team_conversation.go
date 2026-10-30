package setup

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/fastclaw-ai/fastclaw/internal/agent"
)

const teamMaxTurns = 24
const teamDecisionTimeout = 30 * time.Second
const teamInactivityTimeout = 90 * time.Second

type teamDecision struct {
	Mode       string   `json:"mode"`
	MemberIDs  []string `json:"memberIds"`
	TriggerIDs []string `json:"triggerMessageIds"`
}
type teamTurn struct {
	MemberID   string   `json:"memberId"`
	Round      int      `json:"round"`
	TriggerIDs []string `json:"triggerMessageIds"`
	MessageIDs []string `json:"messageIds"`
}
type teamCoordinator interface {
	DecideGroup(context.Context, string, string) (string, error)
}
type teamPrivateDeliverer interface {
	DeliverPrivate(ctx context.Context, sessionID, userID string, source map[string]any, content string)
}
type teamPending struct {
	id       string
	triggers []string
}
type teamOutcome struct {
	member   resolvedTeamMember
	messages []teamRunMessage
	private  []teamPrivateMessage
	failed   bool
	round    int
	triggers []string
}

func (run *teamRun) notice(text string) {
	run.mu.Lock()
	defer run.mu.Unlock()
	run.snapshot.Messages = append(run.snapshot.Messages, teamRunMessage{ID: fmt.Sprintf("notice-%d", time.Now().UnixNano()), Role: "status", Content: text, Timestamp: time.Now().UnixMilli(), GroupTurnID: run.snapshot.TurnID})
}

type teamPhase struct {
	Kind    string `json:"kind"`
	Name    string `json:"name,omitempty"`
	Attempt int    `json:"attempt,omitempty"`
	Total   int    `json:"total,omitempty"`
}

func (run *teamRun) phase(text string, detail teamPhase) {
	run.mu.Lock()
	defer run.mu.Unlock()
	run.snapshot.Phase = text // compatibility with older clients
	run.snapshot.PhaseDetail = &detail
}
func teamJSON(value any) string { b, _ := json.Marshal(value); return string(b) }
func teamProfiles(members []resolvedTeamMember) []map[string]string {
	var profiles []map[string]string
	for _, m := range members {
		profiles = append(profiles, map[string]string{"id": m.AgentID, "name": m.Name, "description": m.Description})
	}
	return profiles
}

// Keep the latest request and triggering messages before filling the remaining
// context with recent history. Use a fresh bounded view for every member turn.
func teamSharedMessages(messages []teamRunMessage, triggers []string) []teamRunMessage {
	remaining := 48000
	selected := map[string]teamRunMessage{}
	include := func(m teamRunMessage) {
		if _, ok := selected[m.ID]; ok || remaining <= 0 {
			return
		}
		if len(m.ImageURLs) > 0 {
			m.Content += fmt.Sprintf("\n[Attached images: %d]", len(m.ImageURLs))
			m.ImageURLs = nil
		}
		if len(m.Attachments) > 0 {
			for _, a := range m.Attachments {
				m.Content += "\n[Attached file: " + a.Name + "]"
			}
			m.Attachments = nil
		}
		// Private bodies reach members only through their own privateInbox.
		m.Deliveries = nil
		r := []rune(m.Content)
		r = r[:min(len(r), min(12000, remaining))]
		m.Content = string(r)
		remaining -= len(r)
		selected[m.ID] = m
	}
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == "user" {
			include(messages[i])
			break
		}
	}
	for i := len(messages) - 1; i >= 0; i-- {
		for _, id := range triggers {
			if messages[i].ID == id {
				include(messages[i])
			}
		}
	}
	for i := len(messages) - 1; i >= 0; i-- {
		include(messages[i])
	}
	result := []teamRunMessage{}
	for _, m := range messages {
		if v, ok := selected[m.ID]; ok {
			result = append(result, v)
		}
	}
	return result
}
func validateTeamDecision(raw string, members []resolvedTeamMember, messages []teamRunMessage, private []teamPrivateMessage) (teamDecision, error) {
	raw = strings.TrimSpace(raw)
	if strings.HasPrefix(raw, "```") {
		parts := strings.SplitN(raw, "\n", 2)
		if len(parts) == 2 {
			raw = strings.TrimSuffix(strings.TrimSpace(parts[1]), "```")
		}
	}
	var d teamDecision
	if err := json.Unmarshal([]byte(raw), &d); err != nil {
		return d, fmt.Errorf("invalid coordination JSON: %w", err)
	}
	if d.Mode == "none" {
		if len(d.MemberIDs)+len(d.TriggerIDs) != 0 {
			return d, fmt.Errorf("invalid empty coordination")
		}
		return d, nil
	}
	if d.Mode != "single" && d.Mode != "parallel" && d.Mode != "sequential" {
		return d, fmt.Errorf("invalid coordination mode")
	}
	if len(d.MemberIDs) == 0 || len(d.TriggerIDs) == 0 || (d.Mode == "single" && len(d.MemberIDs) != 1) {
		return d, fmt.Errorf("invalid coordination members or triggers")
	}
	seen := map[string]bool{}
	for _, id := range d.MemberIDs {
		known := false
		for _, m := range members {
			if m.AgentID == id {
				known = true
			}
		}
		if !known || (d.Mode == "parallel" && seen[id]) {
			return d, fmt.Errorf("unknown or repeated member")
		}
		seen[id] = true
	}
	visible := map[string]bool{}
	for _, m := range messages {
		visible[m.ID] = true
	}
	for _, p := range private {
		allowed := true
		for _, id := range d.MemberIDs {
			if p.Sender != id && p.Recipient != id {
				allowed = false
			}
		}
		if allowed {
			visible[p.ID] = true
		}
	}
	for _, id := range d.TriggerIDs {
		if !visible[id] {
			return d, fmt.Errorf("inaccessible coordination trigger")
		}
	}
	return d, nil
}
func teamDecisionPrompt(req teamChatRequest, members []resolvedTeamMember, messages []teamRunMessage, private []teamPrivateMessage, turns []teamTurn, unavailable []string) string {
	envelopes := append([]teamPrivateMessage{}, private...)
	for i := range envelopes {
		envelopes[i].Content = ""
	}
	return `You are the hidden coordinator supervising a group conversation. Choose single for one worker, parallel for independent work, sequential when later members need earlier results, or none only when the task is complete and a user-facing result already exists (or no reply is appropriate). Never impersonate other members. Requests addressed to the group as a whole, such as a roll call, introductions, opinions from everyone, or counting off, need actual replies from all available members. Counting off is sequential so each member sees the preceding number. Do not have only the lead answer on their behalf. For collaborative tasks schedule specialists and the lead for consolidation. Inspect completed turns; do not repeat finished work. An earlier speaker may return to review new results. Quarantined members must not be selected. Use only this context; do not call tools. Private envelopes are visible but their bodies are not.
Return ONLY JSON: {"mode":"none|single|parallel|sequential","memberIds":["exact member IDs"],"triggerMessageIds":["accessible message IDs"]}. none requires both arrays empty.
` + teamJSON(map[string]any{"group": map[string]string{"name": req.Name, "description": req.Description}, "human": req.HumanName, "leadMemberId": members[0].AgentID, "members": teamProfiles(members), "messages": teamSharedMessages(messages, nil), "privateDeliveries": envelopes, "completedTurns": turns, "unavailableMemberIds": unavailable})
}
func (s *Server) decideTeam(ctx context.Context, req teamChatRequest, members []resolvedTeamMember, run *teamRun, turns []teamTurn, unavailable map[string]bool) (teamDecision, error) {
	snapshot := run.read()
	run.mu.Lock()
	private := append([]teamPrivateMessage{}, run.private...)
	run.mu.Unlock()
	missing := []string{}
	for _, m := range members {
		if unavailable[m.AgentID] {
			missing = append(missing, m.AgentID)
		}
	}
	prompt := teamDecisionPrompt(req, members, snapshot.Messages, private, turns, missing)
	attempts := 0
	var last error
	for _, m := range members {
		if ctx.Err() != nil {
			return teamDecision{}, ctx.Err()
		}
		if unavailable[m.AgentID] {
			continue
		}
		coordinator, ok := m.Handle.(teamCoordinator)
		if !ok {
			last = fmt.Errorf("member has no coordinator capability")
			continue
		}
		attempts++
		if attempts > 3 {
			break
		}
		run.phase(fmt.Sprintf("%s 正在协调（%d/3）", m.Name, attempts), teamPhase{Kind: "coordinating", Name: m.Name, Attempt: attempts, Total: 3})
		attempt, cancel := context.WithTimeout(ctx, teamDecisionTimeout)
		type answer struct {
			text string
			err  error
		}
		done := make(chan answer, 1)
		go func() {
			defer func() {
				if recover() != nil {
					done <- answer{err: fmt.Errorf("coordinator failed")}
				}
			}()
			text, err := coordinator.DecideGroup(attempt, req.SessionID+"-controller", prompt)
			done <- answer{text, err}
		}()
		var result answer
		select {
		case result = <-done:
		case <-attempt.Done():
			result.err = attempt.Err()
		}
		cancel()
		if result.err == nil {
			var d teamDecision
			d, result.err = validateTeamDecision(result.text, members, snapshot.Messages, private)
			if result.err == nil {
				for _, id := range d.MemberIDs {
					if unavailable[id] {
						result.err = fmt.Errorf("coordinator chose unavailable member")
					}
				}
				if result.err == nil {
					return d, nil
				}
			}
		}
		last = result.err
	}
	return teamDecision{}, fmt.Errorf("群聊协调失败，未随意分配成员：%v", last)
}
func teamMemberPrompt(req teamChatRequest, member resolvedTeamMember, members []resolvedTeamMember, messages []teamRunMessage, private []teamPrivateMessage, round int, triggers []string, unavailable map[string]bool) string {
	inbox := []teamPrivateMessage{}
	remaining := 24000
	for i := len(private) - 1; i >= 0 && remaining > 0; i-- {
		p := private[i]
		if p.Sender == member.AgentID || p.Recipient == member.AgentID {
			r := []rune(p.Content)
			p.Content = string(r[:min(len(r), remaining)])
			remaining -= len([]rune(p.Content))
			inbox = append(inbox, p)
		}
	}
	return `You are the current member of a group conversation and have been explicitly scheduled to respond now. Use your own identity and skills. Do not impersonate another member or answer a roll call on their behalf. Follow the triggering messages and the original request using this shared transcript. Continue prior work instead of restarting it. Mention @Name only to hand concrete work to that member; other members may hand results back to you for review. If members are unavailable, explain that honestly and take over or reassign their unfinished work. Ordinary text is public; use <|split|> on its own line for separate messages. Your normal skills and tools remain available.
Private delivery: [[private:MEMBER_ID]]message[[/private]] sends to a member id; [[private:human]]message[[/private]] sends to the human in your direct chat with an unread notification. Private blocks are removed from the public stream; multiple private blocks and private-only replies are supported. Use [[private-info:MEMBER_ID]]information[[/private]] for information-only delivery that must NOT activate the recipient; use ordinary private blocks only when requesting a response or action. privateInbox bodies are visible only to their sender and recipient; keep them within that audience unless disclosure is authorized. Tool input and output are not private delivery channels.
Confidentiality: interpret it semantically in any language. For confidential setup or assignments (secret words or roles in a game, hidden information, personal data), deliver each secret ONLY to its intended recipient, including the human via [[private:human]], and keep it out of public text entirely — never state a secret publicly and then also send it privately. A recipient may disclose private information only when the task authorizes it.
Normal project contributions and assignments belong in the PUBLIC group so later workers can build on them; do not send private duplicates of public assignments. Use private delivery only when the human or the task calls for confidentiality or private contact.
When the human asks you to contact another member, actually send the request with private delivery; do not impersonate their answer or claim delivery failed based on old chat text. A member who receives a private request replies privately to its sender unless the request asks for a public response or a direct message to the human.
The member roster below is authoritative: every entry in members is an AI agent reachable by private delivery at its exact id; only human is the human participant. Disregard earlier conversation claims that these channels are unavailable.
` + teamJSON(map[string]any{"group": map[string]string{"name": req.Name, "description": req.Description}, "human": map[string]string{"id": "human", "name": req.HumanName}, "members": teamProfiles(members), "currentBot": map[string]string{"id": member.AgentID, "name": member.Name}, "turn": map[string]any{"round": round, "triggerMessageIds": triggers, "unavailableMemberIds": unavailable}, "originalRequest": req.Message, "messages": teamSharedMessages(messages, triggers), "privateInbox": inbox})
}
func (s *Server) executeTeamMember(r *http.Request, uid string, req teamChatRequest, m resolvedTeamMember, members []resolvedTeamMember, run *teamRun, visible []teamRunMessage, round int, triggers []string, unavailable map[string]bool) teamOutcome {
	run.mu.Lock()
	private := append([]teamPrivateMessage{}, run.private...)
	delete(run.open, m.AgentID)
	run.snapshot.ActiveAgents = append(run.snapshot.ActiveAgents, m.AgentID)
	run.mu.Unlock()
	defer func() {
		run.mu.Lock()
		for i, id := range run.snapshot.ActiveAgents {
			if id == m.AgentID {
				run.snapshot.ActiveAgents = append(run.snapshot.ActiveAgents[:i], run.snapshot.ActiveAgents[i+1:]...)
				break
			}
		}
		delete(run.open, m.AgentID)
		run.mu.Unlock()
	}()
	params := teamMemberParams(req.Params, m, members)
	params["__fastclawGroupChat"].(map[string]any)["instruction"] = teamMemberPrompt(req, m, members, visible, private, round, triggers, unavailable)
	req.Params = params
	out := teamOutcome{member: m, round: round, triggers: triggers}
	raw := ""
	allRaw := ""
	var emitted []string
	var callbackMu sync.Mutex
	emit := func(env agent.EventEnvelope) {
		callbackMu.Lock()
		defer callbackMu.Unlock()
		e := env.Event
		if e.Type == "tool_call" || e.Type == "tool_result" {
			run.toolEvent(m.AgentID, round, e)
			return
		}
		if e.Type == "error" {
			out.failed = true
			return
		}
		if e.Type == "content_delta" {
			delta, _ := e.Data["delta"].(string)
			raw += delta
		} else if e.Type == "content" {
			raw, _ = e.Data["content"].(string)
		} else {
			return
		}
		public, _, _ := parseTeamPrivate(raw)
		kind := "group_partial"
		if e.Type == "content" {
			kind = "content"
			allRaw += raw + "\n"
			emitted = append(emitted, public)
			raw = ""
		}
		run.event(m.AgentID, agent.EventEnvelope{Event: agent.ChatEvent{Type: kind, Data: map[string]any{"content": public}}})
	}
	success, returned := s.runTeamAgentTurn(r, uid, req, m, members, emit)
	callbackMu.Lock()
	defer callbackMu.Unlock()
	if !success {
		out.failed = true
	}
	if len(emitted) == 0 {
		if returned != "" {
			raw = returned
		}
		public, _, _ := parseTeamPrivate(raw)
		if public != "" {
			run.event(m.AgentID, agent.EventEnvelope{Event: agent.ChatEvent{Type: "content", Data: map[string]any{"content": public}}})
		}
		allRaw = raw
	}
	out.private, _ = resolveTeamPrivate(allRaw, m, members, fmt.Sprintf("%s-%s-%d", run.read().TurnID, m.AgentID, round))
	if _, err := resolveTeamPrivate(allRaw, m, members, "check"); err != nil {
		run.notice(m.Name + "：" + err.Error())
	}
	// Only the messages produced in this invocation are passed to handoff routing.
	existing := map[string]bool{}
	for _, message := range visible {
		existing[message.ID] = true
	}
	for _, message := range run.read().Messages {
		if message.AgentID == m.AgentID && !existing[message.ID] {
			out.messages = append(out.messages, message)
		}
	}
	return out
}
func teamHandoffs(messages []teamRunMessage, private []teamPrivateMessage, members []resolvedTeamMember, scheduled map[string]bool) []teamPending {
	result := []teamPending{}
	positions := map[string]int{}
	add := func(id, trigger, sender string) {
		if id == sender || scheduled[id] {
			return
		}
		if i, ok := positions[id]; ok {
			result[i].triggers = append(result[i].triggers, trigger)
		} else {
			positions[id] = len(result)
			result = append(result, teamPending{id, []string{trigger}})
		}
	}
	for _, message := range messages {
		if message.Role != "agent" {
			continue
		}
		mentioned, all := teamMentions(message.Content, members)
		if all {
			mentioned = members
		}
		for _, m := range mentioned {
			add(m.AgentID, message.ID, message.AgentID)
		}
	}
	for _, p := range private {
		// Information-only deliveries never activate the recipient.
		if p.Recipient != "human" && p.Intent != "inform" {
			add(p.Recipient, p.ID, p.Sender)
		}
	}
	return result
}

func (s *Server) runTeamConversation(r *http.Request, uid string, req teamChatRequest, members []resolvedTeamMember, run *teamRun) {
	userID := run.read().TurnID + "-user"
	mentioned, all := teamMentions(req.Message, members)
	if all {
		mentioned = members
	}
	supervised := len(mentioned) == 0 && len(members) > 1
	turns := []teamTurn{}
	unavailable := map[string]bool{}
	monitored := map[string]bool{}
	decision := teamDecision{Mode: "parallel", TriggerIDs: []string{userID}}
	for _, m := range mentioned {
		decision.MemberIDs = append(decision.MemberIDs, m.AgentID)
	}
	var deferred *teamDecision
	fail := func(err error) {
		run.notice(err.Error())
		run.mu.Lock()
		run.snapshot.Status = "failed"
		run.mu.Unlock()
	}
	if supervised {
		var err error
		decision, err = s.decideTeam(r.Context(), req, members, run, turns, unavailable)
		if err != nil {
			if r.Context().Err() == nil {
				fail(err)
			}
			return
		}
		if decision.Mode != "none" && decision.MemberIDs[0] != members[0].AgentID {
			copy := decision
			deferred = &copy
			decision = teamDecision{Mode: "single", MemberIDs: []string{members[0].AgentID}, TriggerIDs: decision.TriggerIDs}
		}
	} else if len(mentioned) == 0 {
		decision.MemberIDs = []string{members[0].AgentID}
	}
	pending := []teamPending{}
	for _, id := range decision.MemberIDs {
		pending = append(pending, teamPending{id, decision.TriggerIDs})
	}
	mode := decision.Mode
	attempts := 0
	for len(pending) > 0 && r.Context().Err() == nil {
		if attempts >= teamMaxTurns {
			run.mu.Lock()
			run.snapshot.Limited = true
			run.mu.Unlock()
			run.notice("自动回复已达到 24 轮，发送消息可继续。")
			return
		}
		batch := pending[:min(len(pending), teamMaxTurns-attempts)]
		truncated := len(batch) < len(pending)
		pending = nil
		run.phase("成员正在回复", teamPhase{Kind: "replying"})
		scheduled := map[string]bool{}
		for _, p := range batch {
			scheduled[p.id] = true
		}
		batchMessages := []teamRunMessage{}
		batchPrivate := []teamPrivateMessage{}
		record := func(out teamOutcome) {
			ids := []string{}
			for _, message := range out.messages {
				ids = append(ids, message.ID)
			}
			turns = append(turns, teamTurn{out.member.AgentID, out.round, out.triggers, ids})
			batchMessages = append(batchMessages, out.messages...)
			for i, p := range out.private {
				if p.Recipient == "human" {
					// Like a proactive message: it lands in the sender's
					// direct chat (its latest session), labelled with the
					// group it came from, and raises an unread notice.
					if deliverer, ok := out.member.Handle.(teamPrivateDeliverer); ok {
						sessionID := latestDirectSession(out.member.Handle)
						source := map[string]any{"kind": "group", "id": req.TeamID, "name": req.Name}
						deliverer.DeliverPrivate(r.Context(), sessionID, uid, source, p.Content)
						out.private[i].Session = sessionID
						s.chatEventHub().Publish(uid, out.member.AgentID, sessionID, agent.EventEnvelope{Seq: -1, Event: agent.ChatEvent{Type: "content", Data: map[string]any{"content": p.Content, "metadata": map[string]any{"privateFrom": source}}}})
						s.chatEventHub().Publish(uid, out.member.AgentID, sessionID, agent.EventEnvelope{Seq: -1, Event: agent.ChatEvent{Type: "done"}})
					} else {
						run.notice(out.member.Name + " 的私信未能投递")
					}
				}
			}
			// Recorded after delivery so human-addressed entries carry the
			// session their unread notice opens.
			batchPrivate = append(batchPrivate, out.private...)
			run.mu.Lock()
			run.private = append(run.private, out.private...)
			if len(run.private) > 200 {
				run.private = append([]teamPrivateMessage{}, run.private[len(run.private)-200:]...)
			}
			run.snapshot.Rounds = attempts
			run.mu.Unlock()
			run.attachDeliveries(out, members, req.HumanName)
			scheduled[out.member.AgentID] = true
			s.persistTeamTopic(uid, req.TeamID, run)
		}
		find := func(id string) resolvedTeamMember {
			for _, m := range members {
				if m.AgentID == id {
					return m
				}
			}
			return members[0]
		}
		recoverTurn := func(item teamPending, initial *teamOutcome) bool {
			if initial != nil && !initial.failed {
				record(*initial)
				return true
			}
			if initial != nil {
				unavailable[initial.member.AgentID] = true
				run.notice(initial.member.Name + " 回复失败，正在安排其他成员接替。")
			}
			candidates := append([]resolvedTeamMember{find(item.id)}, members...)
			for _, m := range candidates {
				if unavailable[m.AgentID] {
					continue
				}
				if r.Context().Err() != nil {
					return false
				}
				if attempts >= teamMaxTurns {
					return false
				}
				attempts++
				out := s.executeTeamMember(r, uid, req, m, members, run, run.read().Messages, attempts, item.triggers, unavailable)
				if !out.failed {
					record(out)
					return true
				}
				unavailable[m.AgentID] = true
				run.notice(m.Name + " 回复失败，正在安排其他成员接替。")
			}
			return false
		}
		ok := true
		if mode == "parallel" {
			visible := run.read().Messages
			results := make([]teamOutcome, len(batch))
			var wg sync.WaitGroup
			for i, p := range batch {
				attempts++
				round := attempts
				wg.Add(1)
				go func(i int, p teamPending, round int) {
					defer wg.Done()
					if unavailable[p.id] {
						results[i] = teamOutcome{member: find(p.id), failed: true}
						return
					}
					results[i] = s.executeTeamMember(r, uid, req, find(p.id), members, run, visible, round, p.triggers, unavailable)
				}(i, p, round)
			}
			wg.Wait()
			if r.Context().Err() != nil {
				return
			}
			for _, out := range results {
				if out.failed {
					unavailable[out.member.AgentID] = true
				} else {
					record(out)
				}
			}
			for i, out := range results {
				if out.failed && !recoverTurn(batch[i], &out) {
					ok = false
				}
			}
		} else {
			for i, item := range batch {
				if !recoverTurn(item, nil) {
					ok = false
					break
				}
				// Fold handoffs into the planned later turn rather than losing its trigger.
				completed := map[string]bool{}
				for _, p := range batch[:i+1] {
					completed[p.id] = true
				}
				for _, handoff := range teamHandoffs(batchMessages, batchPrivate, members, completed) {
					for j := i + 1; j < len(batch); j++ {
						if batch[j].id == handoff.id {
							batch[j].triggers = append(batch[j].triggers, handoff.triggers...)
						}
					}
				}
			}
		}
		if r.Context().Err() != nil {
			return
		}
		if !ok {
			if attempts >= teamMaxTurns {
				run.mu.Lock()
				run.snapshot.Limited = true
				run.mu.Unlock()
				run.notice("自动回复已达到 24 轮，发送消息可继续。")
				return
			}
			fail(fmt.Errorf("所有可用成员均未能完成回复，请检查模型配置后重试。"))
			return
		}
		if truncated {
			run.mu.Lock()
			run.snapshot.Limited = true
			run.mu.Unlock()
			run.notice("自动回复已达到 24 轮，发送消息可继续。")
			return
		}
		pending = teamHandoffs(batchMessages, batchPrivate, members, scheduled)
		if deferred != nil {
			plan := deferred
			deferred = nil
			next := []teamPending{}
			planned := map[string]bool{}
			for _, id := range plan.MemberIDs {
				triggers := append([]string{}, plan.TriggerIDs...)
				for _, handoff := range pending {
					if handoff.id == id {
						triggers = append(triggers, handoff.triggers...)
					}
				}
				next = append(next, teamPending{id, triggers})
				planned[id] = true
			}
			for _, p := range pending {
				if !planned[p.id] {
					next = append(next, p)
				}
			}
			pending = next
			mode = plan.Mode
			if mode == "single" && len(pending) > 1 {
				mode = "parallel"
			}
			continue
		}
		failedUnmonitored := false
		for id := range unavailable {
			if !monitored[id] {
				failedUnmonitored = true
				monitored[id] = true
			}
		}
		if len(pending) == 0 && failedUnmonitored && !unavailable[members[0].AgentID] {
			pending = []teamPending{{members[0].AgentID, []string{userID}}}
			mode = "single"
			continue
		}
		if len(pending) == 0 && (supervised || failedUnmonitored) {
			next, err := s.decideTeam(r.Context(), req, members, run, turns, unavailable)
			if err != nil {
				if r.Context().Err() == nil {
					fail(err)
				}
				return
			}
			if next.Mode == "none" {
				return
			}
			for _, id := range next.MemberIDs {
				pending = append(pending, teamPending{id, next.TriggerIDs})
			}
			mode = next.Mode
			continue
		}
		mode = "single"
		if len(pending) > 1 {
			mode = "parallel"
		}
	}
}

func (run *teamRun) toolEvent(member string, round int, e agent.ChatEvent) {
	run.mu.Lock()
	defer run.mu.Unlock()
	id, _ := e.Data["id"].(string)
	id = fmt.Sprintf("%s-%s-%d-tool-%s", run.snapshot.TurnID, member, round, id)
	text := teamJSON(e.Data)
	text, _, _ = parseTeamPrivate(text)
	runes := []rune(text)
	text = string(runes[:min(12000, len(runes))])
	for i, message := range run.snapshot.Messages {
		if message.ID == id {
			run.snapshot.Messages[i].Content += "\n" + text
			return
		}
	}
	run.snapshot.Messages = append(run.snapshot.Messages, teamRunMessage{ID: id, Role: "tool", AgentID: member, Content: text, Timestamp: time.Now().UnixMilli(), GroupTurnID: run.snapshot.TurnID})
}
