package setup

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/fastclaw-ai/fastclaw/internal/agent"
	"github.com/fastclaw-ai/fastclaw/internal/bus"
	"github.com/fastclaw-ai/fastclaw/internal/config"
	"github.com/fastclaw-ai/fastclaw/internal/provider"
)

type rollCallProvider struct {
	t          *testing.T
	id         string
	mu         sync.Mutex
	groupCalls int
}

func (p *rollCallProvider) answer(messages []provider.Message) string {
	var system strings.Builder
	for _, m := range messages {
		if m.Role == "system" {
			system.WriteString(m.Content)
		}
	}
	if !strings.Contains(system.String(), "currentBot") {
		return "private reply"
	}
	for _, m := range messages {
		if strings.Contains(m.Content, "DIRECT_SECRET") {
			p.t.Error("private history leaked into group")
		}
	}
	if p.id == "b" && !strings.Contains(system.String(), "1 — A") {
		p.t.Error("real Agent did not receive previous member's answer")
	}
	p.mu.Lock()
	p.groupCalls++
	p.mu.Unlock()
	if p.id == "a" {
		return "1 — A"
	}
	return "2 — B"
}
func (p *rollCallProvider) Chat(_ context.Context, messages []provider.Message, tools []provider.Tool, model string, max int, temp float64) (*provider.Response, error) {
	if len(messages) == 1 && strings.HasPrefix(messages[0].Content, "You are the hidden coordinator") {
		if len(tools) != 0 {
			p.t.Error("controller was given tools")
		}
		raw := messages[0].Content[strings.LastIndex(messages[0].Content, "\n")+1:]
		var data struct {
			CompletedTurns []json.RawMessage `json:"completedTurns"`
			Messages       []teamRunMessage  `json:"messages"`
		}
		if err := json.Unmarshal([]byte(raw), &data); err != nil {
			return nil, err
		}
		decision := teamDecision{Mode: "none", MemberIDs: []string{}, TriggerIDs: []string{}}
		if len(data.CompletedTurns) == 0 {
			decision = teamDecision{Mode: "sequential", MemberIDs: []string{"a", "b"}, TriggerIDs: []string{data.Messages[len(data.Messages)-1].ID}}
		}
		return &provider.Response{Content: teamJSON(decision)}, nil
	}
	return &provider.Response{Content: p.answer(messages)}, nil
}
func (p *rollCallProvider) ChatStream(_ context.Context, messages []provider.Message, _ []provider.Tool, _ string, _ int, _ float64) (*provider.StreamReader, error) {
	chunks := make(chan provider.StreamChunk, 1)
	chunks <- provider.StreamChunk{Content: p.answer(messages), Done: true}
	close(chunks)
	return provider.NewStreamReader(chunks), nil
}
func TestTeamRollCallThroughRealAgentExecutionAndSessionHistory(t *testing.T) {
	s := &Server{}
	var members []resolvedTeamMember
	for _, id := range []string{"a", "b"} {
		p := &rollCallProvider{t: t, id: id}
		a := agent.NewAgent(config.ResolvedAgent{ID: id, Home: t.TempDir(), Model: "test/" + id, MaxTokens: 512, MaxToolIterations: 2}, p, bus.New(), t.TempDir())
		a.HandleWebChatStream(context.Background(), "private-"+id, "", "user", "DIRECT_SECRET", nil, nil, nil)
		members = append(members, resolvedTeamMember{AgentID: id, Name: strings.ToUpper(id), SessionID: "topic-agent-" + id, Handle: a})
	}
	run, ctx := testRun("topic")
	run.snapshot.Messages = []teamRunMessage{{ID: "topic-turn-user", Role: "user", Content: "报个数", GroupTurnID: run.snapshot.TurnID}}
	req := teamChatRequest{TeamID: "group", SessionID: "topic", Name: "Team", Message: "报个数", Params: map[string]any{"__fastclawGroupTurnId": run.snapshot.TurnID}}
	s.executeTeamRun(httptest.NewRequest("POST", "/", nil).WithContext(ctx), req, members, "user", run)
	snapshot := run.read()
	if snapshot.Status != "completed" {
		t.Fatalf("execution failed: %s", teamJSON(snapshot))
	}
	public := teamJSON(snapshot.Messages)
	for i, m := range members {
		if !strings.Contains(public, fmt.Sprintf("%d — %s", i+1, m.Name)) {
			t.Fatalf("missing actual member response: %s", public)
		}
		if len(m.Handle.WebChatHistory(m.SessionID)) < 2 {
			t.Fatal("Agent session did not persist user and answer")
		}
	}
}
