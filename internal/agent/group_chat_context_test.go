package agent

import (
	"strings"
	"testing"
)

func TestGroupContextFromParams(t *testing.T) {
	params := map[string]any{
		"__fastclawGroupChat": map[string]any{
			"botUsername": "Writer",
			"teammates":   []any{"Researcher", "Engineer"},
		},
	}
	got := groupContextFromParams(params)
	if got == nil || got.BotUsername != "Writer" {
		t.Fatalf("group context = %#v", got)
	}
	if len(got.Teammates) != 2 || got.Teammates[0] != "Researcher" {
		t.Fatalf("teammates = %#v", got.Teammates)
	}
}

func TestRenderClientParamsHidesInternalGroupFields(t *testing.T) {
	result := renderClientParams(map[string]any{
		"language":              "zh-CN",
		"__fastclawGroupTurnId": "private-turn-id",
	})
	if strings.Contains(result, "private-turn-id") || strings.Contains(result, "__fastclaw") {
		t.Fatalf("internal group metadata leaked into client params: %s", result)
	}
	if !strings.Contains(result, "zh-CN") {
		t.Fatalf("public client parameter missing: %s", result)
	}
}
