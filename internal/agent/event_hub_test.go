package agent

import (
	"fmt"
	"testing"
	"time"
)

func TestEventQueueKeepsEveryEventForASlowReader(t *testing.T) {
	hub := NewEventHub()
	queue, unsubscribe := hub.SubscribeQueue("u", "a", "s")
	defer unsubscribe()
	lossy, unsubscribeLossy := hub.Subscribe("u", "a", "s")
	defer unsubscribeLossy()

	const total = 500 // far beyond a plain subscriber's 32-slot buffer
	published := make(chan struct{})
	go func() {
		defer close(published)
		for i := 0; i < total-1; i++ {
			hub.Publish("u", "a", "s", EventEnvelope{Seq: -1, Event: ChatEvent{Type: "content_delta", Data: map[string]any{"delta": fmt.Sprint(i)}}})
		}
		hub.Publish("u", "a", "s", EventEnvelope{Seq: 0, Event: ChatEvent{Type: "done"}})
	}()

	var received []EventEnvelope
	deadline := time.After(5 * time.Second)
	for len(received) < total {
		select {
		case <-queue.Ready():
			time.Sleep(2 * time.Millisecond) // a reader stalled on a slow network write
			received = append(received, queue.Take()...)
		case <-deadline:
			t.Fatalf("received %d of %d events", len(received), total)
		}
	}
	<-published
	for i, env := range received[:total-1] {
		if env.Event.Data["delta"] != fmt.Sprint(i) {
			t.Fatalf("event %d out of order: %v", i, env.Event.Data)
		}
	}
	if received[total-1].Event.Type != "done" {
		t.Fatalf("last event = %q, want done", received[total-1].Event.Type)
	}
	if extra := queue.Take(); len(extra) != 0 {
		t.Fatalf("queue kept %d events after Take", len(extra))
	}
	if len(lossy) > 32 {
		t.Fatalf("plain subscription grew past its buffer: %d", len(lossy))
	}
}

func TestEventQueueStopsReceivingAfterUnsubscribe(t *testing.T) {
	hub := NewEventHub()
	queue, unsubscribe := hub.SubscribeQueue("u", "a", "s")
	hub.Publish("u", "a", "s", EventEnvelope{Seq: 0, Event: ChatEvent{Type: "content"}})
	unsubscribe()
	hub.Publish("u", "a", "s", EventEnvelope{Seq: 1, Event: ChatEvent{Type: "done"}})
	if got := queue.Take(); len(got) != 1 || got[0].Event.Type != "content" {
		t.Fatalf("queue = %+v", got)
	}
	if len(hub.queues) != 0 {
		t.Fatalf("hub kept %d queue keys", len(hub.queues))
	}
}
