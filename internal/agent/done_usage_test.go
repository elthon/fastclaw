package agent

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"path/filepath"
	"sync"
	"testing"

	"github.com/fastclaw-ai/fastclaw/internal/bus"
	"github.com/fastclaw-ai/fastclaw/internal/config"
	"github.com/fastclaw-ai/fastclaw/internal/provider"
	"github.com/fastclaw-ai/fastclaw/internal/store"
	"github.com/fastclaw-ai/fastclaw/internal/usage"
)

type scriptedStreamStep struct {
	content   string
	toolCalls []provider.ToolCall
	usage     provider.Usage
	err       error
}

type scriptedStreamProvider struct {
	mu    sync.Mutex
	steps []scriptedStreamStep
	idx   int
}

func (p *scriptedStreamProvider) Chat(context.Context, []provider.Message, []provider.Tool, string, int, float64) (*provider.Response, error) {
	return nil, errors.New("unexpected nonstreaming Chat")
}

func (p *scriptedStreamProvider) ChatStream(context.Context, []provider.Message, []provider.Tool, string, int, float64) (*provider.StreamReader, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.idx >= len(p.steps) {
		return nil, errors.New("no scripted step left")
	}
	step := p.steps[p.idx]
	p.idx++
	if step.err != nil {
		return nil, step.err
	}
	chunks := make(chan provider.StreamChunk, 2)
	if step.content != "" {
		chunks <- provider.StreamChunk{Content: step.content}
	}
	chunks <- provider.StreamChunk{
		ToolCalls: step.toolCalls,
		Usage:     step.usage,
		Done:      true,
	}
	close(chunks)
	return provider.NewStreamReader(chunks), nil
}

func collectStreamEvents(t *testing.T, fn func(ctx context.Context)) []ChatEvent {
	t.Helper()
	ch := make(chan ChatEvent, 64)
	ctx := ContextWithStream(context.Background(), ch, nil, nil, "u1", "writer", "s1")
	fn(ctx)
	close(ch)
	var out []ChatEvent
	for evt := range ch {
		out = append(out, evt)
	}
	return out
}

func findDoneEvent(t *testing.T, events []ChatEvent) ChatEvent {
	t.Helper()
	for i := len(events) - 1; i >= 0; i-- {
		if events[i].Type == "done" {
			return events[i]
		}
	}
	t.Fatalf("no done event emitted; events=%+v", events)
	return ChatEvent{}
}

func newDoneUsageTestAgent(t *testing.T, model string, prov provider.Provider, providers map[string]config.ProviderConfig) *Agent {
	t.Helper()
	a := NewAgent(config.ResolvedAgent{
		ID:                "writer",
		Home:              t.TempDir(),
		Model:             model,
		MaxTokens:         1024,
		MaxToolIterations: 5,
		Providers:         providers,
	}, prov, bus.New(), t.TempDir())
	a.providers = providers
	return a
}

func TestDoneUsageSingleTurnPriced(t *testing.T) {
	prov := &scriptedStreamProvider{
		steps: []scriptedStreamStep{{
			content: "hello",
			usage: provider.Usage{
				InputTokens:         1000,
				OutputTokens:        250,
				CacheReadTokens:     4000,
				CacheCreationTokens: 500,
			},
		}},
	}
	providers := map[string]config.ProviderConfig{
		"dashscope": {
			APIKey: "k",
			Models: []config.ModelEntry{pricedModel("qwen3.5-plus", 2, 10, 0.2, 2.5)},
		},
	}
	a := newDoneUsageTestAgent(t, "dashscope/qwen3.5-plus", prov, providers)
	events := collectStreamEvents(t, func(ctx context.Context) {
		a.HandleWebChatStream(ctx, "s1", "", "u1", "hi", nil, nil, nil)
	})
	done := findDoneEvent(t, events)
	u, ok := done.Data["usage"].(map[string]any)
	if !ok {
		t.Fatalf("done.Data[usage] missing: %+v", done.Data)
	}
	if u["inputTokens"] != int64(1000) || u["outputTokens"] != int64(250) ||
		u["cacheReadTokens"] != int64(4000) || u["cacheCreationTokens"] != int64(500) ||
		u["totalTokens"] != int64(5750) || u["requestCount"] != 1 ||
		u["lastContextTokens"] != int64(5500) || u["model"] != "qwen3.5-plus" {
		t.Fatalf("unexpected usage payload: %+v", u)
	}
	wantCost := usd(1000, 250, 4000, 500, usage.Price{Input: 2, Output: 10, CacheRead: 0.2, CacheWrite: 2.5})
	gotCost, _ := u["costUsd"].(*float64)
	if gotCost == nil || math.Abs(*gotCost-wantCost) > 1e-12 {
		t.Fatalf("costUsd = %v, want %v", gotCost, wantCost)
	}
}

func TestDoneUsageMultiRoundToolLoop(t *testing.T) {
	prov := &scriptedStreamProvider{
		steps: []scriptedStreamStep{
			{
				toolCalls: []provider.ToolCall{{
					ID:       "tc_1",
					Type:     "function",
					Function: provider.FunctionCall{Name: "echo_test", Arguments: `{"x":"1"}`},
				}},
				usage: provider.Usage{InputTokens: 1200, OutputTokens: 80, CacheReadTokens: 300},
			},
			{
				content: "all done",
				usage:   provider.Usage{InputTokens: 2400, OutputTokens: 120, CacheReadTokens: 1500, CacheCreationTokens: 200},
			},
		},
	}
	a := newDoneUsageTestAgent(t, "main/unpriced-model", prov, nil)
	a.registry.Register("echo_test", "test tool", map[string]any{"type": "object"}, func(context.Context, json.RawMessage) (string, error) {
		return "ok", nil
	})

	events := collectStreamEvents(t, func(ctx context.Context) {
		a.HandleWebChatStream(ctx, "s2", "", "u1", "run tool", nil, nil, nil)
	})
	done := findDoneEvent(t, events)
	u, ok := done.Data["usage"].(map[string]any)
	if !ok {
		t.Fatalf("done.Data[usage] missing: %+v", done.Data)
	}
	// Cumulative across both rounds; lastContextTokens is round 2 only (2400+1500+200=4100).
	if u["inputTokens"] != int64(3600) || u["outputTokens"] != int64(200) ||
		u["cacheReadTokens"] != int64(1800) || u["cacheCreationTokens"] != int64(200) ||
		u["totalTokens"] != int64(5800) || u["requestCount"] != 2 ||
		u["lastContextTokens"] != int64(4100) || u["model"] != "unpriced-model" {
		t.Fatalf("unexpected multi-round usage: %+v", u)
	}
	// Unpriced model must serialize costUsd as JSON null, never 0.
	raw, err := json.Marshal(done)
	if err != nil {
		t.Fatal(err)
	}
	var parsed struct {
		Data struct {
			Usage map[string]json.RawMessage `json:"usage"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatal(err)
	}
	if got := string(parsed.Data.Usage["costUsd"]); got != "null" {
		t.Fatalf("costUsd JSON = %s, want null", got)
	}
}

func TestDoneUsageHelperCallsDoNotClobberContextOrModel(t *testing.T) {
	prov := &scriptedStreamProvider{
		steps: []scriptedStreamStep{
			{
				toolCalls: []provider.ToolCall{{
					ID:       "tc_sub",
					Type:     "function",
					Function: provider.FunctionCall{Name: "helper_call", Arguments: `{}`},
				}},
				usage: provider.Usage{InputTokens: 3000, OutputTokens: 50, CacheReadTokens: 1000},
			},
			{
				content: "final",
				usage:   provider.Usage{InputTokens: 5000, OutputTokens: 150, CacheReadTokens: 2000},
			},
		},
	}
	providers := map[string]config.ProviderConfig{
		"main": {
			APIKey: "k",
			Models: []config.ModelEntry{
				pricedModel("big", 3, 15, 0.3, 3.75),
				pricedModel("small", 1, 5, 0.1, 1.25),
			},
		},
	}
	a := newDoneUsageTestAgent(t, "main/big", prov, providers)
	a.registry.Register("helper_call", "simulates subagent + message_agent metering", map[string]any{"type": "object"}, func(ctx context.Context, _ json.RawMessage) (string, error) {
		// Compaction / subagent call on another model and smaller context:
		a.meterTokens(ctx, "s-sub", "main/small", provider.Usage{InputTokens: 400, OutputTokens: 40}, 0)
		// message_agent handoff (depth=1) must not overwrite main turn context/model even if recordMainTurnContext is called:
		handoffCtx := context.WithValue(ctx, messageAgentDepthKey{}, 1)
		a.meterTokens(handoffCtx, "s-handoff", "main/small", provider.Usage{InputTokens: 600, OutputTokens: 60}, 0)
		a.recordMainTurnContext(handoffCtx, "main/small", provider.Usage{InputTokens: 600, OutputTokens: 60})
		return "done", nil
	})

	events := collectStreamEvents(t, func(ctx context.Context) {
		a.HandleWebChatStream(ctx, "s3", "", "u1", "delegate", nil, nil, nil)
	})
	done := findDoneEvent(t, events)
	u := done.Data["usage"].(map[string]any)
	// Totals include all 4 calls; lastContextTokens & model stay on main turn's round 2 (5000+2000=7000, "big").
	if u["inputTokens"] != int64(9000) || u["outputTokens"] != int64(300) ||
		u["cacheReadTokens"] != int64(3000) || u["requestCount"] != 4 ||
		u["lastContextTokens"] != int64(7000) || u["model"] != "big" {
		t.Fatalf("unexpected usage with helpers: %+v", u)
	}
}

func TestDoneUsageZeroLLMCallsOmitsData(t *testing.T) {
	a := newDoneUsageTestAgent(t, "main/big", nil, nil)
	// No provider configured -> zero LLM calls.
	events := collectStreamEvents(t, func(ctx context.Context) {
		a.HandleWebChatStream(ctx, "s4", "", "u1", "hello", nil, nil, nil)
	})
	done := findDoneEvent(t, events)
	if done.Data != nil {
		t.Fatalf("zero-LLM done.Data = %+v, want nil", done.Data)
	}

	// Slash command (/status) -> zero LLM calls.
	events = collectStreamEvents(t, func(ctx context.Context) {
		a.HandleWebChatStream(ctx, "s4", "", "u1", "/status", nil, nil, nil)
	})
	done = findDoneEvent(t, events)
	if done.Data != nil {
		t.Fatalf("slash command done.Data = %+v, want nil", done.Data)
	}
}

func TestDoneUsagePartialTurnErrorStillReportsCompletedCalls(t *testing.T) {
	prov := &scriptedStreamProvider{
		steps: []scriptedStreamStep{
			{
				toolCalls: []provider.ToolCall{{
					ID:       "tc_err",
					Type:     "function",
					Function: provider.FunctionCall{Name: "echo_test", Arguments: `{}`},
				}},
				usage: provider.Usage{InputTokens: 700, OutputTokens: 30},
			},
			{
				err: errors.New("API error 400: bad request"),
			},
		},
	}
	a := newDoneUsageTestAgent(t, "main/big", prov, nil)
	a.registry.Register("echo_test", "test tool", map[string]any{"type": "object"}, func(context.Context, json.RawMessage) (string, error) {
		return "ok", nil
	})
	events := collectStreamEvents(t, func(ctx context.Context) {
		a.HandleWebChatStream(ctx, "s5", "", "u1", "fail on step 2", nil, nil, nil)
	})
	done := findDoneEvent(t, events)
	u, ok := done.Data["usage"].(map[string]any)
	if !ok {
		t.Fatalf("partial turn done.Data[usage] missing: %+v", done.Data)
	}
	if u["requestCount"] != 1 || u["inputTokens"] != int64(700) || u["outputTokens"] != int64(30) || u["lastContextTokens"] != int64(700) {
		t.Fatalf("partial turn usage = %+v", u)
	}
}

func TestDoneUsagePersistsAndReplaysCleanly(t *testing.T) {
	ctx := context.Background()
	db, err := store.NewDBStore("sqlite", "file:"+filepath.Join(t.TempDir(), "events.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}

	prov := &scriptedStreamProvider{
		steps: []scriptedStreamStep{{
			content: "persisted reply",
			usage:   provider.Usage{InputTokens: 420, OutputTokens: 80, CacheReadTokens: 100},
		}},
	}
	a := newDoneUsageTestAgent(t, "dashscope/qwen3.5-plus", prov, nil)
	ch := make(chan ChatEvent, 16)
	streamCtx := ContextWithStream(ctx, ch, db, nil, "u1", "writer", "s_persist")
	a.HandleWebChatStream(streamCtx, "s_persist", "", "u1", "hi", nil, nil, nil)
	close(ch)

	rows, err := db.ListSessionEventsSince(ctx, "u1", "writer", "s_persist", -1)
	if err != nil {
		t.Fatal(err)
	}
	var doneData map[string]any
	for _, r := range rows {
		if r.Type == "done" {
			if err := json.Unmarshal(r.Data, &doneData); err != nil {
				t.Fatalf("unmarshal persisted done: %v", err)
			}
		}
	}
	u, ok := doneData["usage"].(map[string]any)
	if !ok {
		t.Fatalf("persisted done missing usage: %+v", doneData)
	}
	if u["inputTokens"] != float64(420) || u["outputTokens"] != float64(80) ||
		u["cacheReadTokens"] != float64(100) || u["totalTokens"] != float64(600) ||
		u["requestCount"] != float64(1) || u["lastContextTokens"] != float64(520) ||
		u["model"] != "qwen3.5-plus" {
		t.Fatalf("persisted usage = %+v", u)
	}
	if _, exists := u["costUsd"]; !exists {
		t.Fatalf("persisted usage missing costUsd key: %+v", u)
	}
}
