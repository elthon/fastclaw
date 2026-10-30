package setup

import "testing"

func TestSelectTeamMembers(t *testing.T) {
	members := []resolvedTeamMember{
		{AgentID: "research", Name: "研究员", Description: "搜索资料 行业调研 竞品分析"},
		{AgentID: "writer", Name: "文案", Description: "写作 编辑 品牌文案"},
		{AgentID: "engineer", Name: "工程师", Description: "编程 代码 API 开发"},
	}
	server := &Server{}

	t.Run("explicit mention", func(t *testing.T) {
		got := server.selectTeamMembers("@文案 帮我改一下标题", members)
		if len(got) != 1 || got[0].AgentID != "writer" {
			t.Fatalf("selected = %#v, want writer", got)
		}
	})

	t.Run("all", func(t *testing.T) {
		got := server.selectTeamMembers("@all 一起看看这个方案", members)
		if len(got) != len(members) {
			t.Fatalf("selected %d members, want %d", len(got), len(members))
		}
	})

	t.Run("unaddressed messages require coordination", func(t *testing.T) {
		got := server.selectTeamMembers("帮我写一段品牌文案", members)
		if len(got) != 0 {
			t.Fatalf("unexpected heuristic route: %#v", got)
		}
	})
}

func TestTeamAgentSessionID(t *testing.T) {
	got := teamAgentSessionID("team-tm-123", "tm-123", "writer")
	if got != "team-tm-123-agent-writer" {
		t.Fatalf("session id = %q", got)
	}
	if again := teamAgentSessionID(got, "tm-123", "writer"); again != got {
		t.Fatalf("session id should be idempotent: got %q", again)
	}
}

func TestTeamMemberParams(t *testing.T) {
	base := map[string]any{"planMode": true}
	members := []resolvedTeamMember{
		{AgentID: "writer", Name: "Writer"},
		{AgentID: "research", Name: "Researcher"},
	}
	got := teamMemberParams(base, members[0], members)
	if _, mutated := base["__fastclawGroupChat"]; mutated {
		t.Fatal("teamMemberParams mutated caller params")
	}
	if got["planMode"] != true {
		t.Fatalf("public params were not preserved: %#v", got)
	}
	group, ok := got["__fastclawGroupChat"].(map[string]any)
	if !ok || group["botUsername"] != "Writer" {
		t.Fatalf("group params = %#v", got["__fastclawGroupChat"])
	}
}
