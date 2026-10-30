package setup

import (
	"context"
	"encoding/json"
	"log/slog"

	"github.com/fastclaw-ai/fastclaw/internal/agent"
	"github.com/fastclaw-ai/fastclaw/internal/store"
)

// turnForwarder writes one turn's events to its SSE connection in seq order.
// The live queue is lossless; as a safety net, persisted events missing
// from it are recovered from session_events, so a lost `done`, `content`
// or `error` cannot end the turn early.
type turnForwarder struct {
	ctx                         context.Context
	events                      store.Store
	userID, agentID, sessionKey string
	// lastSeq is the highest persisted seq delivered; before the turn it
	// is the session's latest seq, so earlier turns are never replayed.
	lastSeq      int64
	forward      func(agent.EventEnvelope)
	failed       *bool
	forwardedAny bool
	turnPending  bool
}

// deliver forwards env and reports whether the turn finished.
func (f *turnForwarder) deliver(env agent.EventEnvelope) bool {
	if env.Seq >= 0 {
		if env.Seq <= f.lastSeq {
			return false // already forwarded from the persisted log
		}
		if f.lastSeq >= 0 && env.Seq > f.lastSeq+1 {
			for _, missed := range persistedEventsSince(f.ctx, f.events, f.userID, f.agentID, f.sessionKey, f.lastSeq) {
				if missed.Seq >= env.Seq {
					break
				}
				if f.deliver(missed) {
					return true
				}
			}
		}
		f.lastSeq = env.Seq
	}
	switch env.Event.Type {
	case "error":
		*f.failed = true
	case "turn_pending":
		f.turnPending = true
		return false
	}
	f.forward(env)
	f.forwardedAny = true
	return env.Event.Type == "done"
}

// recoverTail delivers persisted events after the last one forwarded and
// reports whether they include the turn's `done`.
func (f *turnForwarder) recoverTail() bool {
	for _, env := range persistedEventsSince(f.ctx, f.events, f.userID, f.agentID, f.sessionKey, f.lastSeq) {
		if f.deliver(env) {
			return true
		}
	}
	return false
}

// persistedEventsSince returns the persisted events of one session after
// afterSeq as envelopes. Used as the last resort when the turn ended
// without `done` reaching this connection; content deltas are never
// persisted, but the full `content`, tool events, `error` and `done` are.
func persistedEventsSince(ctx context.Context, events store.Store, userID, agentID, sessionKey string, afterSeq int64) []agent.EventEnvelope {
	if events == nil {
		return nil
	}
	rows, err := events.ListSessionEventsSince(ctx, userID, agentID, sessionKey, afterSeq)
	if err != nil {
		slog.Warn("chat stream backfill failed", "agent", agentID, "session", sessionKey, "since", afterSeq, "error", err)
		return nil
	}
	envelopes := make([]agent.EventEnvelope, 0, len(rows))
	for _, row := range rows {
		var data map[string]any
		if len(row.Data) > 0 {
			_ = json.Unmarshal(row.Data, &data)
		}
		envelopes = append(envelopes, agent.EventEnvelope{Seq: row.Seq, Event: agent.ChatEvent{Type: row.Type, Data: data}})
	}
	return envelopes
}
