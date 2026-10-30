package agent

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fastclaw-ai/fastclaw/internal/bus"
	"github.com/fastclaw-ai/fastclaw/internal/config"
	"github.com/fastclaw-ai/fastclaw/internal/provider"
)

type concurrentTopicProvider struct {
	started chan string
	release <-chan struct{}
}

func (p *concurrentTopicProvider) Chat(context.Context, []provider.Message, []provider.Tool, string, int, float64) (*provider.Response, error) {
	return nil, fmt.Errorf("unexpected nonstreaming request")
}
func (p *concurrentTopicProvider) ChatStream(ctx context.Context, messages []provider.Message, _ []provider.Tool, _ string, _ int, _ float64) (*provider.StreamReader, error) {
	var users []string
	for _, m := range messages {
		if m.Role == "user" {
			users = append(users, m.Content)
		}
	}
	if len(users) != 1 {
		return nil, fmt.Errorf("topic history leaked: %v", users)
	}
	p.started <- users[0]
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-p.release:
	}
	chunks := make(chan provider.StreamChunk, 1)
	chunks <- provider.StreamChunk{Content: "Reply to " + users[0], Done: true}
	close(chunks)
	return provider.NewStreamReader(chunks), nil
}
func TestConcurrentAgentTurnsKeepHistoryAndBindings(t *testing.T) {
	release := make(chan struct{})
	prov := &concurrentTopicProvider{started: make(chan string, 2), release: release}
	home := t.TempDir()
	a := NewAgent(config.ResolvedAgent{ID: "writer", Home: home, Model: "test", MaxTokens: 1024, MaxToolIterations: 2}, prov, bus.New(), t.TempDir())
	var wg sync.WaitGroup
	for _, topic := range []string{"topic-a", "topic-b"} {
		wg.Add(1)
		go func(topic string) {
			defer wg.Done()
			reply := a.HandleWebChatStream(context.Background(), topic, "", "user", topic, nil, map[string]any{"__fastclawGroupTurnId": topic + "-turn"}, nil)
			if !strings.Contains(reply, topic) {
				t.Errorf("%s received %q", topic, reply)
			}
		}(topic)
	}
	for i := 0; i < 2; i++ {
		select {
		case <-prov.started:
		case <-time.After(3 * time.Second):
			close(release)
			wg.Wait()
			t.Fatal("topics did not run concurrently")
		}
	}
	close(release)
	wg.Wait()
	if a.registry.SessionID() != "" {
		t.Fatalf("shared tool registry was rebound to %s", a.registry.SessionID())
	}
	for _, topic := range []string{"topic-a", "topic-b"} {
		history := a.WebChatHistory(topic)
		if len(history) != 2 {
			t.Fatalf("%s history: %#v", topic, history)
		}
		for _, message := range history {
			if message["groupTurnId"] != topic+"-turn" {
				t.Fatalf("history lost turn identity: %#v", message)
			}
		}
	}
}
