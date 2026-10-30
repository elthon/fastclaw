package setup

import "testing"

func TestTeamTopicID(t *testing.T) {
	for id, want := range map[string]bool{
		"g-1788943408788-7mmo3x":        true,
		"s-1788943408788-7mmo3x":        false, // a private chat's id
		"g-1788943408788-x-agent-agt_1": false,
		"g-abc-7mmo3x":                  false,
		"team-tm-1-topic-uuid":          false, // legacy ids are checked by prefix instead
		"":                              false,
	} {
		if got := teamTopicID.MatchString(id); got != want {
			t.Errorf("%q: got %v, want %v", id, got, want)
		}
	}
}
