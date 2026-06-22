package channels

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/fastclaw-ai/fastclaw/internal/bus"
)

func TestDingTalkParseTextMessage(t *testing.T) {
	dt, err := NewDingTalk("client", "secret", "robot", true, "robot", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	payload := []byte(`{
		"headers":{"messageId":"frame-1"},
		"data":{
			"conversationId":"cid-1",
			"conversationType":"2",
			"msgId":"msg-1",
			"senderStaffId":"staff-1",
			"senderNick":"Alice",
			"msgtype":"text",
			"text":{"content":"hello"},
			"sessionWebhook":"https://example.test/session",
			"isInAtList":true,
			"atUsers":[{"staffId":"robot"}]
		}
	}`)
	ackID, msg, err := dt.parseStreamPayload(payload)
	if err != nil {
		t.Fatalf("parseStreamPayload error: %v", err)
	}
	if ackID != "frame-1" {
		t.Fatalf("ackID = %q, want frame-1", ackID)
	}
	if msg.Channel != "dingtalk" || msg.AccountID != "robot" || msg.ChatID != "cid-1" || msg.UserID != "staff-1" || msg.MessageID != "msg-1" || msg.Text != "hello" || msg.PeerKind != "group" || msg.SenderName != "Alice" {
		t.Fatalf("unexpected inbound: %+v", msg)
	}
	if got := dt.sessionWebhook("cid-1"); got != "https://example.test/session" {
		t.Fatalf("cached webhook = %q", got)
	}
}

func TestDingTalkSkipsUnmentionedGroup(t *testing.T) {
	dt, err := NewDingTalk("client", "secret", "robot", true, "robot", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	payload := []byte(`{
		"conversationId":"cid-1",
		"conversationType":"2",
		"msgId":"msg-1",
		"senderId":"user-1",
		"msgtype":"text",
		"text":{"content":"hello"}
	}`)
	_, _, err = dt.parseStreamPayload(payload)
	if err == nil {
		t.Fatal("expected unmentioned group message to be skipped")
	}
}

func TestDingTalkSendMessageUsesMarkdownWebhook(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		_, _ = w.Write([]byte(`{"errcode":0,"errmsg":"ok"}`))
	}))
	defer srv.Close()

	dt, err := NewDingTalk("client", "secret", "robot", true, "robot", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	dt.cacheWebhook("cid-1", srv.URL)

	text := "## Summary\n\n- **ok**\n\n```go\nfmt.Println(\"hi\")\n```"
	if err := dt.SendMessage(bus.OutboundMessage{ChatID: "cid-1", Text: text}); err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	if got["msgtype"] != "markdown" {
		t.Fatalf("msgtype = %v, want markdown; payload=%#v", got["msgtype"], got)
	}
	md, ok := got["markdown"].(map[string]any)
	if !ok {
		t.Fatalf("markdown payload missing: %#v", got)
	}
	if md["title"] != "Summary" {
		t.Fatalf("title = %v, want Summary", md["title"])
	}
	if md["text"] != text {
		t.Fatalf("markdown text changed: %q", md["text"])
	}
}

func TestDingTalkThinkingUsesPlainTextWebhook(t *testing.T) {
	seen := make(chan map[string]any, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var got map[string]any
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		seen <- got
		_, _ = w.Write([]byte(`{"errcode":0,"errmsg":"ok"}`))
	}))
	defer srv.Close()

	dt, err := NewDingTalk("client", "secret", "robot", true, "robot", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	dt.cacheWebhook("cid-1", srv.URL)
	dt.sendThinking("cid-1")

	select {
	case got := <-seen:
		if got["msgtype"] != "text" {
			t.Fatalf("msgtype = %v, want text; payload=%#v", got["msgtype"], got)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for thinking webhook")
	}
}
