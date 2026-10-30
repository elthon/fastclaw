package setup

import (
	"strings"
	"testing"
)

func TestParseTeamPrivateInfoIntent(t *testing.T) {
	public, blocks, valid := parseTeamPrivate("hi [[private-info:bob]]fyi[[/private]] [[private:human]]secret[[/private]]")
	if !valid || len(blocks) != 2 {
		t.Fatalf("valid=%v blocks=%+v", valid, blocks)
	}
	if strings.Contains(public, "fyi") || strings.Contains(public, "secret") {
		t.Fatalf("private body leaked to public text: %q", public)
	}
	if blocks[0].to != "bob" || blocks[0].intent != "inform" || blocks[1].to != "human" || blocks[1].intent != "" {
		t.Fatalf("unexpected blocks: %+v", blocks)
	}
}

func TestTeamHandoffsSkipInformOnlyPrivate(t *testing.T) {
	members := []resolvedTeamMember{{AgentID: "a", Name: "A"}, {AgentID: "b", Name: "B"}, {AgentID: "c", Name: "C"}}
	private := []teamPrivateMessage{
		{ID: "p1", Sender: "a", Recipient: "b", Intent: "inform"},
		{ID: "p2", Sender: "a", Recipient: "c"},
	}
	got := teamHandoffs(nil, private, members, map[string]bool{})
	if len(got) != 1 || got[0].id != "c" {
		t.Fatalf("handoffs = %+v, want only c", got)
	}
}

func TestAttachDeliveriesKeepsBodiesOutOfGroup(t *testing.T) {
	run := &teamRun{open: map[string]int{}}
	run.snapshot.TurnID = "t"
	run.snapshot.Messages = []teamRunMessage{{ID: "m1", Role: "agent", AgentID: "a", Content: "public"}}
	members := []resolvedTeamMember{{AgentID: "a", Name: "A"}, {AgentID: "b", Name: "B"}}
	out := teamOutcome{member: members[0], messages: run.snapshot.Messages, private: []teamPrivateMessage{
		{ID: "p1", Sender: "a", Recipient: "b", Content: "the secret word"},
		{ID: "p2", Sender: "a", Recipient: "human", Content: "your word", Session: "s-1"},
	}}
	run.attachDeliveries(out, members, "doubi")
	d := run.snapshot.Messages[0].Deliveries
	if len(d) != 2 || d[0].RecipientName != "B" || d[1].RecipientName != "doubi" || d[1].Session != "s-1" {
		t.Fatalf("deliveries = %+v", d)
	}
	for _, m := range teamSharedMessages(run.snapshot.Messages, nil) {
		if len(m.Deliveries) > 0 {
			t.Fatal("deliveries leaked into member-visible transcript")
		}
	}

	// A private-only reply gets an empty receipt message of its own.
	run.attachDeliveries(teamOutcome{member: members[1], private: []teamPrivateMessage{{ID: "p3", Sender: "b", Recipient: "a"}}}, members, "")
	last := run.snapshot.Messages[len(run.snapshot.Messages)-1]
	if last.AgentID != "b" || last.Content != "" || len(last.Deliveries) != 1 {
		t.Fatalf("receipt-only message = %+v", last)
	}
}
