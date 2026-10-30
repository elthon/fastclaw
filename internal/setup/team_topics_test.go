package setup

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/fastclaw-ai/fastclaw/internal/agent"
	"github.com/fastclaw-ai/fastclaw/internal/store"
)

func TestLegacyTeamTopicTitle(t *testing.T) {
	for _, tc := range []struct{ title, preview, want string }{
		{`\[User\]: 报个数`, `\[User\]: 报个数`, "报个数"},
		{"", `[User]: 你们好啊`, "你们好啊"},
		{"新名字", `\[User\]: 报个数`, "新名字"},
		{"[User]: 文档示例", "真实用户输入", "[User]: 文档示例"},
		{"[设计] 讨论", "[设计] 讨论", "[设计] 讨论"},
	} {
		if got := legacyTeamTopicTitle(tc.title, tc.preview); got != tc.want {
			t.Errorf("title %q, preview %q: got %q, want %q", tc.title, tc.preview, got, tc.want)
		}
	}
}

func TestTeamReplyDelimitersInStreamingFinalAndHistory(t *testing.T) {
	run, _ := testRun("delimiters")
	defer run.cancel()
	emit := func(kind, content string) {
		run.event("a", agent.EventEnvelope{Event: agent.ChatEvent{Type: kind, Data: map[string]any{"content": content}}})
	}
	for _, suffix := range []string{"<|", "<|sp", "<|split|", "<|split|>"} {
		emit("group_partial", "已收到\n"+suffix)
		if got := run.read().Messages[0].Content; got != "已收到" {
			t.Fatalf("streaming delimiter leaked: %q", got)
		}
	}
	emit("content", "已收到\n<|split|>")
	// Check the persisted buffer as well as the sanitized read path: a trailing
	// delimiter yields only one part, which must still replace the original.
	if got := run.snapshot.Messages[0].Content; got != "已收到" {
		t.Fatalf("trailing delimiter persisted: %q", got)
	}
	emit("content", "<|split|>")
	if got := run.snapshot.Messages[1].Content; got != "" {
		t.Fatalf("delimiter-only reply persisted: %q", got)
	}
	emit("content", "第一段\n<|split|>\n第二段")
	if len(run.snapshot.Messages) != 4 || run.snapshot.Messages[2].Content != "第一段" || run.snapshot.Messages[3].Content != "第二段" {
		t.Fatalf("message splitting failed: %+v", run.snapshot.Messages)
	}
	legacy := "历史消息\n<|split|>"
	run.snapshot.Messages = []teamRunMessage{{Role: "agent", Content: legacy}, {Role: "user", Content: legacy}, {Role: "agent", Content: "```text\n<|split|>\n```"}}
	got := run.read().Messages
	if got[0].Content != "历史消息" || got[1].Content != legacy || got[2].Content != "```text\n<|split|>\n```" || run.snapshot.Messages[0].Content != legacy {
		t.Fatalf("history normalization changed literal content or the source: %+v", got)
	}
}

func TestTeamInlineReplySeparators(t *testing.T) {
	for _, tc := range []struct {
		text string
		want []string
	}{
		{"1，FastClaw 在线。<|split|>随时干活，说吧。", []string{"1，FastClaw 在线。", "随时干活，说吧。"}},
		{"<|split|>你好<|split|><|split|>再见<|split|>", []string{"你好", "再见"}},
		{"示例 `<|split|>` 保留<|split|>下一条", []string{"示例 `<|split|>` 保留", "下一条"}},
		{"``含 ` 和 <|split|> 的代码``<|split|>下一条", []string{"``含 ` 和 <|split|> 的代码``", "下一条"}},
		{"```text\n行内<|split|>代码\n```\n正文<|split|>后文", []string{"```text\n行内<|split|>代码\n```\n正文", "后文"}},
		{"~~~text\n<|split|>\n~~~\n正文<|split|>后文", []string{"~~~text\n<|split|>\n~~~\n正文", "后文"}},
	} {
		if got := splitTeamReply(tc.text); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%q: got %q, want %q", tc.text, got, tc.want)
		}
	}
	run, _ := testRun("inline")
	defer run.cancel()
	const marker = "<|split|>"
	for n := 1; n <= len(marker); n++ {
		run.event("a", agent.EventEnvelope{Event: agent.ChatEvent{Type: "group_partial", Data: map[string]any{"content": "1，FastClaw 在线。" + marker[:n]}}})
		if got := run.read().Messages[0].Content; got != "1，FastClaw 在线。" {
			t.Fatalf("inline partial marker leaked at byte %d: %q", n, got)
		}
	}
	run.event("a", agent.EventEnvelope{Event: agent.ChatEvent{Type: "content", Data: map[string]any{"content": "1，FastClaw 在线。<|split|>随时干活，说吧。"}}})
	if got := run.read().Messages; len(got) != 2 || got[0].Content != "1，FastClaw 在线。" || got[1].Content != "随时干活，说吧。" {
		t.Fatalf("inline final reply was not split: %+v", got)
	}
}

func TestTeamTopicPersistenceIsolationAndRestart(t *testing.T) {
	db, err := store.NewDBStore("sqlite", "file:"+filepath.Join(t.TempDir(), "topics.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	if err = db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	s := &Server{dataStore: db}
	run, _ := testRun("topic")
	run.snapshot.CompleteHistory = true
	run.snapshot.Status = "completed"
	run.snapshot.Title = "kept title"
	run.snapshot.Messages = []teamRunMessage{{ID: "public", Role: "agent", Content: "hello"}}
	run.private = []teamPrivateMessage{{ID: "secret", Sender: "a", Recipient: "b", Content: "PRIVATE"}}
	if err = s.saveTeamTopic("alice", "group", run); err != nil {
		t.Fatal(err)
	}
	restarted := &Server{dataStore: db}
	loaded, err := restarted.loadTeamTopic(ctx, "alice", "group", "topic")
	if err != nil {
		t.Fatal(err)
	}
	if loaded.read().Status != "completed" || loaded.read().Title != "kept title" || len(loaded.private) != 1 || strings.Contains(teamJSON(loaded.read()), "PRIVATE") {
		t.Fatalf("bad persisted state: %#v", loaded)
	}
	for _, scope := range [][3]string{{"bob", "group", "topic"}, {"alice", "other", "topic"}, {"alice", "group", "another"}} {
		other, err := restarted.loadTeamTopic(ctx, scope[0], scope[1], scope[2])
		if err != nil || other != nil {
			t.Fatal("topic crossed scope")
		}
	}
	run.snapshot.Status = "running"
	run.snapshot.ActiveAgents = []string{"a"}
	if err = s.saveTeamTopic("alice", "group", run); err != nil {
		t.Fatal(err)
	}
	loaded, err = restarted.loadTeamTopic(ctx, "alice", "group", "topic")
	if err != nil || loaded.read().Status != "stopped" || len(loaded.read().ActiveAgents) != 0 {
		t.Fatal("restart incorrectly claims execution is still running")
	}
	if err = db.SaveConfig(ctx, &store.ConfigRecord{Kind: teamTopicKind, UserID: "alice", Name: teamTopicName("group", "topic"), Data: map[string]interface{}{"deleted": true}}); err != nil {
		t.Fatal(err)
	}
	if _, err = restarted.loadTeamTopic(ctx, "alice", "group", "topic"); !errors.Is(err, errTeamTopicDeleted) {
		t.Fatalf("deleted topic must be distinguishable from missing topics and access errors: %v", err)
	}
}
func TestTeamSplitMessagesPreservesMarkdownCode(t *testing.T) {
	parts := splitTeamReply("First\n\nparagraph\n<|split|>\n```text\n<|split|>\n```\nEnd")
	if len(parts) != 2 || !strings.Contains(parts[0], "\n\n") || !strings.Contains(parts[1], "<|split|>") {
		t.Fatalf("bad message split: %v", parts)
	}
}
func TestTeamSharedContextBoundsImagesAndRetainsTrigger(t *testing.T) {
	messages := []teamRunMessage{{ID: "old", Role: "agent", Content: "important trigger", ImageURLs: []string{"secret-base64"}}}
	for i := 0; i < 20; i++ {
		messages = append(messages, teamRunMessage{ID: string(rune('a' + i)), Role: "agent", Content: strings.Repeat("界", 12000)})
	}
	messages = append(messages, teamRunMessage{ID: "latest", Role: "user", Content: "latest request"})
	result := teamSharedMessages(messages, []string{"old"})
	text := teamJSON(result)
	if !strings.Contains(text, "important trigger") || !strings.Contains(text, "latest request") || strings.Contains(text, "secret-base64") {
		t.Fatal("context priorities or attachment filtering failed")
	}
	n := 0
	for _, m := range result {
		n += len([]rune(m.Content))
	}
	if n > 48000 {
		t.Fatal("context is unbounded")
	}
}
