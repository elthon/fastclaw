package agent

import (
	"context"
	"fmt"
	"testing"

	"github.com/fastclaw-ai/fastclaw/internal/provider"
)

type groupDecisionProvider struct {
	t     *testing.T
	calls int
}

func (p *groupDecisionProvider) Chat(_ context.Context, messages []provider.Message, tools []provider.Tool, model string, max int, temp float64) (*provider.Response, error) {
	p.calls++
	if len(messages) != 1 || messages[0].Content != "coordination context" || len(tools) > 0 || model != "configured/model" {
		p.t.Fatal("controller reused member history, tools, or wrong model")
	}
	return &provider.Response{Content: `{"mode":"none","memberIds":[],"triggerMessageIds":[]}`}, nil
}
func (p *groupDecisionProvider) ChatStream(context.Context, []provider.Message, []provider.Tool, string, int, float64) (*provider.StreamReader, error) {
	return nil, fmt.Errorf("unexpected stream")
}
func TestGroupCoordinatorUsesIsolatedToolFreeConfiguredModel(t *testing.T) {
	p := &groupDecisionProvider{t: t}
	a := &Agent{provider: p, model: "configured/model"}
	for _, topic := range []string{"a-controller", "b-controller"} {
		if _, err := a.DecideGroup(context.Background(), topic, "coordination context"); err != nil {
			t.Fatal(err)
		}
	}
	if p.calls != 2 {
		t.Fatal("missing controller call")
	}
}
