package setup

import (
	"context"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fastclaw-ai/fastclaw/internal/agent"
	"github.com/fastclaw-ai/fastclaw/internal/bus"
)

type teamTestAgent struct {
	AgentHandle
	id       string
	server   *Server
	started  chan string
	release  <-chan struct{}
	reply    string
	fail     bool
	mu       sync.Mutex
	injected []bus.InboundMessage
}

func (a *teamTestAgent) DecideGroup(context.Context, string, string) (string, error) {
	return `{"mode":"none","memberIds":[],"triggerMessageIds":[]}`, nil
}
func (a *teamTestAgent) WriteSessionAttachments(context.Context, string, string, []agent.Attachment) []string {
	return nil
}
func (a *teamTestAgent) InjectGroupMessage(_ context.Context, msg bus.InboundMessage) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.injected = append(a.injected, msg)
}
func (a *teamTestAgent) HandleWebChatStream(ctx context.Context, sid, project, uid, text string, images []string, params map[string]any, events chan<- agent.ChatEvent) string {
	a.started <- sid
	select {
	case <-ctx.Done():
		return ""
	case <-a.release:
	}
	kind, data := "content", map[string]any{"content": a.reply + " " + sid}
	if a.fail {
		kind = "error"
		data = map[string]any{"message": "provider failed"}
	}
	events <- agent.ChatEvent{Type: kind, Data: data}
	events <- agent.ChatEvent{Type: "done"}
	if a.fail {
		return ""
	}
	return a.reply + " " + sid
}
func testRun(sid string) (*teamRun, context.Context) {
	ctx, cancel := context.WithCancel(context.Background())
	return &teamRun{cancel: cancel, open: map[string]int{}, snapshot: teamRunSnapshot{SessionID: sid, TurnID: sid + "-turn", Status: "running", Messages: []teamRunMessage{}}}, ctx
}
func awaitStarted(t *testing.T, ch <-chan string) string {
	t.Helper()
	select {
	case sid := <-ch:
		return sid
	case <-time.After(3 * time.Second):
		t.Fatal("member did not start concurrently")
		return ""
	}
}
func awaitRun(t *testing.T, run *teamRun) teamRunSnapshot {
	t.Helper()
	deadline := time.After(3 * time.Second)
	for {
		result := run.read()
		if result.Status != "running" && len(result.ActiveAgents) == 0 {
			return result
		}
		select {
		case <-deadline:
			t.Fatal("run did not finish")
			return result
		case <-time.After(time.Millisecond):
		}
	}
}
func TestTeamTopicsRunConcurrentlyAndStopIndependently(t *testing.T) {
	s := &Server{}
	release := make(chan struct{})
	started := make(chan string, 4)
	a := &teamTestAgent{id: "writer", server: s, started: started, release: release, reply: "reply"}
	runA, ctxA := testRun("a")
	runB, ctxB := testRun("b")
	for _, item := range []struct {
		run *teamRun
		ctx context.Context
	}{{runA, ctxA}, {runB, ctxB}} {
		sid := item.run.snapshot.SessionID
		member := resolvedTeamMember{AgentID: "writer", Name: "Writer", SessionID: sid, Handle: a}
		go s.executeTeamRun(httptest.NewRequest("POST", "/", nil).WithContext(item.ctx), teamChatRequest{TeamID: "team", SessionID: sid, Message: "hello"}, []resolvedTeamMember{member}, "user", item.run)
	}
	first, second := awaitStarted(t, started), awaitStarted(t, started)
	if first == second {
		t.Fatal("topics shared a member session")
	}
	runA.cancel()
	if result := awaitRun(t, runA); result.Status != "stopped" {
		t.Fatalf("status %s", result.Status)
	}
	if runB.read().Status != "running" {
		t.Fatal("stopping A stopped B")
	}
	close(release)
	result := awaitRun(t, runB)
	if result.Status != "completed" || len(result.Messages) != 1 || !strings.HasSuffix(result.Messages[0].Content, " b") {
		t.Fatalf("wrong topic output: %#v", result)
	}
}
func TestTeamParallelMemberFailureAndHandoff(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "handoff", true: "failure"}[fail], func(t *testing.T) {
			s := &Server{}
			release := make(chan struct{})
			started := make(chan string, 64)
			a := &teamTestAgent{id: "a", server: s, started: started, release: release, reply: "@C continue", fail: fail}
			b := &teamTestAgent{id: "b", server: s, started: started, release: release, reply: "B done"}
			c := &teamTestAgent{id: "c", server: s, started: started, release: release, reply: "@A loop must stop"}
			members := []resolvedTeamMember{{AgentID: "a", Name: "A", SessionID: "topic-a", Handle: a}, {AgentID: "b", Name: "B", SessionID: "topic-b", Handle: b}, {AgentID: "c", Name: "C", SessionID: "topic-c", Handle: c}}
			run, ctx := testRun("topic")
			go s.executeTeamRun(httptest.NewRequest("POST", "/", nil).WithContext(ctx), teamChatRequest{TeamID: "team", Message: "@A @B begin"}, members, "user", run)
			awaitStarted(t, started)
			awaitStarted(t, started)
			close(release)
			result := awaitRun(t, run)
			if fail {
				if result.Status != "completed" || len(result.Messages) < 2 {
					t.Fatalf("healthy member was lost: %#v", result)
				}
			} else {
				if result.Status != "completed" || !result.Limited || result.Rounds != 24 {
					t.Fatalf("handoff result: %#v", result)
				}
				if len(started) != 22 {
					t.Fatal("handoff loop was not bounded")
				}
			}
		})
	}
}
func TestTeamMentionBoundaries(t *testing.T) {
	members := []resolvedTeamMember{{AgentID: "ann", Name: "Ann"}, {AgentID: "anna", Name: "Anna"}, {AgentID: "writer", Name: "文案"}}
	for _, test := range []struct {
		content string
		want    string
		all     bool
	}{
		{"@Anna hello", "anna", false}, {"foo@ann.example.com", "", false}, {"`@Ann`\n> @Anna\n```\n@all\n```", "", false},
		{"@alligator", "", false}, {"大家看看", "", false}, {"@文案，请继续", "writer", false}, {"@all hello", "", true}, {"[link](https://example.com/@Ann)", "", false},
	} {
		got, all := teamMentions(test.content, members)
		ids := []string{}
		for _, m := range got {
			ids = append(ids, m.AgentID)
		}
		if strings.Join(ids, ",") != test.want || all != test.all {
			t.Errorf("%q => %v, all %v", test.content, ids, all)
		}
	}
}
func TestChatRunAdmissionAndDetachedLifetime(t *testing.T) {
	s := &Server{}
	parent, disconnect := context.WithCancel(context.Background())
	ctx, cancel := context.WithCancel(context.WithoutCancel(parent))
	defer cancel()
	a, b := teamRunKey{"user", "agent", "a"}, teamRunKey{"user", "agent", "b"}
	if !s.beginChatTurn(a, cancel) || s.beginChatTurn(a, cancel) || !s.beginChatTurn(b, func() {}) {
		t.Fatal("admission must serialize only the same topic")
	}
	disconnect()
	if ctx.Err() != nil {
		t.Fatal("navigation canceled execution")
	}
	cancel()
	s.endChatTurn(a, ctx, false)
	if s.chatTurns[a].status != "stopped" || s.chatTurns[b].status != "running" {
		t.Fatal("status crossed topics")
	}
}
