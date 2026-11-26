package usage

import (
	"math"
	"sync"
	"testing"
)

func TestCollectorSetLastContextAndSnapshot(t *testing.T) {
	var nilCollector *Collector
	nilCollector.SetLastContext(100, "m")
	if snap := nilCollector.Snapshot(); snap.Calls != 0 || snap.LastContextTokens != 0 || snap.Model != "" || snap.CostUSD != nil {
		t.Fatalf("nil snapshot = %+v", snap)
	}

	c := &Collector{}
	c.Add(Tokens{Input: 100, Output: 20, CacheRead: 300, CacheCreation: 50}, 0.0015, true)
	c.SetLastContext(450, "qwen3.5-plus")
	// Helper calls (e.g. compaction / subagent) add to totals & cost without
	// calling SetLastContext, so watermark and model stay on the main turn.
	c.Add(Tokens{Input: 2000, Output: 100}, 0.0025, true)
	// Zero contextTokens and empty model must not clobber recorded values.
	c.SetLastContext(0, "")

	snap := c.Snapshot()
	if snap.Calls != 2 {
		t.Fatalf("Calls = %d, want 2", snap.Calls)
	}
	if snap.Tokens.Input != 2100 || snap.Tokens.Output != 120 || snap.Tokens.CacheRead != 300 || snap.Tokens.CacheCreation != 50 {
		t.Fatalf("Tokens = %+v", snap.Tokens)
	}
	if snap.Tokens.Total() != 2570 {
		t.Fatalf("Total = %d, want 2570", snap.Tokens.Total())
	}
	if snap.LastContextTokens != 450 {
		t.Fatalf("LastContextTokens = %d, want 450", snap.LastContextTokens)
	}
	if snap.Model != "qwen3.5-plus" {
		t.Fatalf("Model = %q, want qwen3.5-plus", snap.Model)
	}
	if snap.CostUSD == nil || math.Abs(*snap.CostUSD-0.004) > 1e-12 {
		t.Fatalf("CostUSD = %v, want 0.004", snap.CostUSD)
	}

	// Subsequent main-loop round updates watermark and model.
	c.SetLastContext(980, "gpt-4o")
	c.Add(Tokens{Input: 50, Output: 10}, 0, false) // unpriced call latches costUnknown
	snap = c.Snapshot()
	if snap.LastContextTokens != 980 || snap.Model != "gpt-4o" {
		t.Fatalf("updated watermark = (%d, %q)", snap.LastContextTokens, snap.Model)
	}
	if snap.CostUSD != nil {
		t.Fatalf("CostUSD = %v, want nil when any call is unpriced", *snap.CostUSD)
	}
}

func TestCollectorConcurrentAccess(t *testing.T) {
	c := &Collector{}
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			c.Add(Tokens{Input: 10, Output: 5}, 0.001, true)
			if i%2 == 0 {
				c.SetLastContext(100+int64(i), "m")
			}
			_ = c.Snapshot()
		}(i)
	}
	wg.Wait()
	snap := c.Snapshot()
	if snap.Calls != 16 || snap.Tokens.Input != 160 || snap.Tokens.Output != 80 {
		t.Fatalf("concurrent snapshot = %+v", snap)
	}
}
