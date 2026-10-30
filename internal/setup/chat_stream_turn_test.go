package setup

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/fastclaw-ai/fastclaw/internal/agent"
	"github.com/fastclaw-ai/fastclaw/internal/store"
)

func TestTurnForwarderRecoversPersistedEventsTheHubDropped(t *testing.T) {
	ctx := context.Background()
	db, err := store.NewDBStore("sqlite", "file:"+filepath.Join(t.TempDir(), "events.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	before, err := db.LatestSessionEventSeq(ctx, "u", "a", "s")
	if err != nil {
		t.Fatal(err)
	}
	envelope := func(kind, data string) agent.EventEnvelope {
		seq, err := db.AppendSessionEvent(ctx, "u", "a", "s", kind, []byte(data))
		if err != nil {
			t.Fatal(err)
		}
		return agent.EventEnvelope{Seq: seq, Event: agent.ChatEvent{Type: kind}}
	}
	call := envelope("tool_call", `{"name":"list_agents"}`)
	result := envelope("tool_result", `{"result":"ok"}`)
	content := envelope("content", `{"content":"Two agents."}`)
	done := envelope("done", `{}`)

	newForwarder := func() (*turnForwarder, *[]agent.EventEnvelope, *bool) {
		var forwarded []agent.EventEnvelope
		failed := false
		return &turnForwarder{ctx: ctx, events: db, userID: "u", agentID: "a", sessionKey: "s", lastSeq: before,
			forward: func(env agent.EventEnvelope) { forwarded = append(forwarded, env) }, failed: &failed}, &forwarded, &failed
	}
	types := func(events []agent.EventEnvelope) string {
		out := ""
		for _, env := range events {
			out += env.Event.Type + " "
		}
		return out
	}

	t.Run("fills a gap before the next live event", func(t *testing.T) {
		turn, forwarded, _ := newForwarder()
		if turn.deliver(call) || !turn.deliver(done) {
			t.Fatal("the turn should finish at done")
		}
		if got := types(*forwarded); got != "tool_call tool_result content done " {
			t.Fatalf("forwarded %q", got)
		}
		if (*forwarded)[2].Event.Data["content"] != "Two agents." {
			t.Fatalf("recovered content data = %v", (*forwarded)[2].Event.Data)
		}
	})

	t.Run("recovers a lost done after the turn ends and skips duplicates", func(t *testing.T) {
		turn, forwarded, _ := newForwarder()
		turn.deliver(call)
		turn.deliver(result)
		if !turn.recoverTail() {
			t.Fatal("recoverTail should find done")
		}
		if turn.deliver(content) {
			t.Fatal("an already recovered event must not be forwarded again")
		}
		if got := types(*forwarded); got != "tool_call tool_result content done " {
			t.Fatalf("forwarded %q", got)
		}
	})

	t.Run("does not replay earlier turns and marks persisted errors", func(t *testing.T) {
		turn, forwarded, failed := newForwarder()
		turn.lastSeq = done.Seq
		failure := envelope("error", `{"message":"invalid api key"}`)
		if turn.recoverTail() {
			t.Fatal("an error is not done")
		}
		if got := types(*forwarded); got != "error " || !*failed || (*forwarded)[0].Seq != failure.Seq {
			t.Fatalf("forwarded %q failed=%v", got, *failed)
		}
	})
}
