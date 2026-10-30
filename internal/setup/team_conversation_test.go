package setup

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fastclaw-ai/fastclaw/internal/agent"
)

type scriptedTeamAgent struct {
	AgentHandle
	server    *Server
	id        string
	mu        sync.Mutex
	prompts   []string
	decisions []string
	decide    func(string) (string, error)
	reply     func(int, string) (string, bool)
}

func (a *scriptedTeamAgent) WriteSessionAttachments(context.Context, string, string, []agent.Attachment) []string {
	return nil
}
func (a *scriptedTeamAgent) DecideGroup(_ context.Context, _ string, prompt string) (string, error) {
	a.mu.Lock()
	a.decisions = append(a.decisions, prompt)
	a.mu.Unlock()
	if a.decide != nil {
		return a.decide(prompt)
	}
	return `{"mode":"none","memberIds":[],"triggerMessageIds":[]}`, nil
}
func (a *scriptedTeamAgent) HandleWebChatStream(ctx context.Context, sid, project, uid, text string, images []string, params map[string]any, events chan<- agent.ChatEvent) string {
	prompt := params["__fastclawGroupChat"].(map[string]any)["instruction"].(string)
	a.mu.Lock()
	a.prompts = append(a.prompts, prompt)
	n := len(a.prompts)
	a.mu.Unlock()
	content, failed := a.reply(n, prompt)
	if ctx.Err() != nil {
		return ""
	}
	// Deliberately fragment private delimiters to test fail-closed public output.
	for _, r := range content {
		events <- agent.ChatEvent{Type: "content_delta", Data: map[string]any{"delta": string(r)}}
	}
	kind := "content"
	data := map[string]any{"content": content}
	if failed {
		kind = "error"
		data = map[string]any{"message": "failed"}
	}
	events <- agent.ChatEvent{Type: kind, Data: data}
	events <- agent.ChatEvent{Type: "done"}
	return content
}
func conversationFixture() (*Server, *teamRun, context.Context, []resolvedTeamMember, []*scriptedTeamAgent) {
	s := &Server{}
	run, ctx := testRun("topic")
	run.snapshot.Messages = []teamRunMessage{{ID: "topic-turn-user", Role: "user", Content: "报个数"}}
	run.snapshot.CompleteHistory = true
	var members []resolvedTeamMember
	var agents []*scriptedTeamAgent
	for _, id := range []string{"a", "b", "c"} {
		a := &scriptedTeamAgent{server: s, id: id, reply: func(int, string) (string, bool) { return "done", false }}
		agents = append(agents, a)
		members = append(members, resolvedTeamMember{AgentID: id, Name: strings.ToUpper(id), SessionID: "topic-" + id, Handle: a})
	}
	return s, run, ctx, members, agents
}
func executeFixture(s *Server, run *teamRun, ctx context.Context, members []resolvedTeamMember, message string) {
	s.executeTeamRun(httptest.NewRequest("POST", "/", nil).WithContext(ctx), teamChatRequest{TeamID: "team", SessionID: "topic", Message: message, Name: "Team"}, members, "user", run)
}
func TestTeamRollCallUsesSequentialSharedTranscript(t *testing.T) {
	s, run, ctx, members, agents := conversationFixture()
	calls := 0
	agents[0].decide = func(string) (string, error) {
		calls++
		if calls == 1 {
			return `{"mode":"sequential","memberIds":["a","b","c"],"triggerMessageIds":["topic-turn-user"]}`, nil
		}
		return `{"mode":"none","memberIds":[],"triggerMessageIds":[]}`, nil
	}
	for i, a := range agents {
		number := i + 1
		a.reply = func(_ int, prompt string) (string, bool) {
			if number > 1 && !strings.Contains(prompt, "NUMBER_") {
				t.Error("later participant cannot see preceding count")
			}
			return []string{"NUMBER_1", "NUMBER_2", "NUMBER_3"}[number-1], false
		}
	}
	executeFixture(s, run, ctx, members, "报个数")
	if calls != 2 || run.read().Status != "completed" {
		t.Fatalf("coordinator did not verify completion: %#v", run.read())
	}
	for _, a := range agents {
		if len(a.prompts) != 1 {
			t.Fatalf("member %s did not participate", a.id)
		}
	}
}
func TestTeamHandoffReturnsToEarlierSpeaker(t *testing.T) {
	s, run, ctx, members, agents := conversationFixture()
	agents[0].reply = func(n int, prompt string) (string, bool) {
		if n == 1 {
			return "@B check this", false
		}
		if !strings.Contains(prompt, "checked result") {
			t.Error("handoff result absent")
		}
		return "final verified result", false
	}
	agents[1].reply = func(int, string) (string, bool) { return "@A checked result", false }
	executeFixture(s, run, ctx, members, "@A start")
	if len(agents[0].prompts) != 2 || len(agents[1].prompts) != 1 || len(agents[2].prompts) != 0 || run.read().Limited {
		t.Fatalf("handoff did not return: %#v", run.read())
	}
}
func TestTeamPrivateDeliveryIsolationAndHandoff(t *testing.T) {
	s, run, ctx, members, agents := conversationFixture()
	agents[0].reply = func(int, string) (string, bool) { return "public [[private:b]]SECRET_B[[/private]]", false }
	agents[1].reply = func(_ int, prompt string) (string, bool) {
		if !strings.Contains(prompt, "SECRET_B") {
			t.Error("recipient did not receive private body")
		}
		return "private task completed", false
	}
	executeFixture(s, run, ctx, members, "@A start")
	if strings.Contains(teamJSON(run.read()), "SECRET_B") {
		t.Fatal("private body leaked to public snapshot")
	}
	if len(agents[1].prompts) != 1 || len(agents[2].prompts) != 0 {
		t.Fatal("private handoff was not routed")
	}
	prompt := teamDecisionPrompt(teamChatRequest{}, members, run.read().Messages, run.private, nil, nil)
	if strings.Contains(prompt, "SECRET_B") {
		t.Fatal("coordinator saw private body")
	}
	observer := teamMemberPrompt(teamChatRequest{}, members[2], members, run.read().Messages, run.private, 1, nil, nil)
	if strings.Contains(observer, "SECRET_B") {
		t.Fatal("observer saw private body")
	}
}
func TestTeamCoordinatorFailoverAndValidation(t *testing.T) {
	s, run, ctx, members, agents := conversationFixture()
	agents[0].decide = func(string) (string, error) { return "", errors.New("offline") }
	agents[1].decide = func(string) (string, error) {
		return `{"mode":"single","memberIds":["intruder"],"triggerMessageIds":["topic-turn-user"]}`, nil
	}
	agents[2].decide = func(string) (string, error) { return `{"mode":"none","memberIds":[],"triggerMessageIds":[]}`, nil }
	d, err := s.decideTeam(ctx, teamChatRequest{}, members, run, nil, map[string]bool{})
	if err != nil || d.Mode != "none" || len(agents[2].decisions) != 1 {
		t.Fatalf("failover failed: %v %v", d, err)
	}
	_, err = validateTeamDecision(`{"mode":"single","memberIds":["c"],"triggerMessageIds":["private"]}`, members, run.read().Messages, []teamPrivateMessage{{ID: "private", Sender: "a", Recipient: "b"}})
	if err == nil {
		t.Fatal("private trigger authorized an unrelated recipient")
	}
}
func TestTeamMemberFailureRecoveredWithoutConcurrentReuse(t *testing.T) {
	s, run, ctx, members, agents := conversationFixture()
	agents[1].reply = func(int, string) (string, bool) { return "", true }
	executeFixture(s, run, ctx, members, "@all start")
	if run.read().Status != "completed" || len(agents[0].prompts) < 2 || len(agents[1].prompts) != 1 {
		t.Fatalf("failure not recovered: %#v", run.read())
	}
}
func TestTeamStopDuringCoordinationDoesNotStartMembers(t *testing.T) {
	s, run, ctx, members, agents := conversationFixture()
	started := make(chan struct{})
	release := make(chan struct{})
	agents[0].decide = func(string) (string, error) {
		close(started)
		<-release
		return `{"mode":"single","memberIds":["a"],"triggerMessageIds":["topic-turn-user"]}`, nil
	}
	done := make(chan struct{})
	go func() { defer close(done); executeFixture(s, run, ctx, members, "task") }()
	<-started
	run.cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("cancellation did not stop coordination")
	}
	close(release)
	if run.read().Status != "stopped" || len(agents[0].prompts) > 0 {
		t.Fatal("member ran after cancellation")
	}
}
func TestTeamPrivateParserNeverPublishesPartialMarkers(t *testing.T) {
	full := "hello [[private:b]]SECRET[[/private]] world"
	for i := 0; i <= len(full); i++ {
		public, _, _ := parseTeamPrivate(full[:i])
		if strings.Contains(public, "SECRET") || strings.Contains(public, "[[") {
			t.Fatalf("leak at %d: %q", i, public)
		}
	}
	for _, bad := range []string{"[[private:no-one]]secret[[/private]]", "[[private:b]]unfinished"} {
		_, err := resolveTeamPrivate(bad, resolvedTeamMember{AgentID: "a"}, []resolvedTeamMember{{AgentID: "b"}}, "reply")
		if err == nil {
			t.Fatal("invalid private delivery accepted")
		}
	}
}
