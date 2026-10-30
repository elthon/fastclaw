package channels

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/fastclaw-ai/fastclaw/internal/bus"
)

// fakeWeCom is a minimal stand-in for openws.work.weixin.qq.com: it
// acks every request, records what the adapter sent, and lets the test
// push callback frames.
type fakeWeCom struct {
	t      *testing.T
	srv    *httptest.Server
	conns  chan *websocket.Conn
	frames chan wecomFrame
	secret string
	wmu    sync.Mutex // gorilla allows one concurrent writer per conn
}

// push writes a server-originated frame (callback / event) to conn.
func (f *fakeWeCom) push(conn *websocket.Conn, v any) {
	f.t.Helper()
	f.wmu.Lock()
	defer f.wmu.Unlock()
	if err := conn.WriteJSON(v); err != nil {
		f.t.Fatal(err)
	}
}

func newFakeWeCom(t *testing.T, secret string) *fakeWeCom {
	f := &fakeWeCom{t: t, conns: make(chan *websocket.Conn, 4), frames: make(chan wecomFrame, 64), secret: secret}
	up := websocket.Upgrader{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		f.conns <- c
		write := func(v any) {
			f.wmu.Lock()
			_ = c.WriteJSON(v)
			f.wmu.Unlock()
		}
		for {
			var fr wecomFrame
			if err := c.ReadJSON(&fr); err != nil {
				return
			}
			code := 0
			if fr.Cmd == "aibot_subscribe" {
				var b struct {
					Secret string `json:"secret"`
				}
				_ = json.Unmarshal(fr.Body, &b)
				if b.Secret != f.secret {
					code = 40001
				}
			}
			var body json.RawMessage
			switch fr.Cmd {
			case "aibot_upload_media_init":
				body = json.RawMessage(`{"upload_id":"up1"}`)
			case "aibot_upload_media_finish":
				body = json.RawMessage(`{"media_id":"m1","type":"image"}`)
			}
			write(map[string]any{"headers": fr.Headers, "errcode": code, "errmsg": "ok", "body": body})
			if fr.Cmd != "ping" {
				f.frames <- fr
			}
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeWeCom) url() string { return "ws" + strings.TrimPrefix(f.srv.URL, "http") }

func (f *fakeWeCom) next(cmd string) wecomFrame {
	f.t.Helper()
	timeout := time.After(3 * time.Second)
	for {
		select {
		case fr := <-f.frames:
			if fr.Cmd == cmd {
				return fr
			}
		case <-timeout:
			f.t.Fatalf("timed out waiting for %s", cmd)
		}
	}
}

func TestWeComEndToEnd(t *testing.T) {
	fake := newFakeWeCom(t, "s3cret")
	mb := bus.New()
	wc, err := NewWeCom("bot1", "s3cret", mb)
	if err != nil {
		t.Fatal(err)
	}
	wc.wsURL = fake.url()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { _ = wc.Start(ctx); close(done) }()

	auth := fake.next("aibot_subscribe")
	if !strings.Contains(string(auth.Body), `"bot_id":"bot1"`) {
		t.Fatalf("auth body = %s", auth.Body)
	}
	conn := <-fake.conns

	// Group message: leading @mention stripped, bot id stamped into Mentions.
	fake.push(conn, map[string]any{
		"cmd":     "aibot_msg_callback",
		"headers": map[string]string{"req_id": "cb-1"},
		"body": map[string]any{
			"msgid": "m-1", "aibotid": "bot1", "chatid": "grp", "chattype": "group",
			"from": map[string]string{"userid": "alice"}, "msgtype": "text",
			"text": map[string]string{"content": "@FastClaw hello there"},
		},
	})
	var in bus.InboundMessage
	select {
	case in = <-mb.Inbound:
	case <-time.After(3 * time.Second):
		t.Fatal("no inbound message")
	}
	if in.Channel != "wecom" || in.ChatID != "grp" || in.UserID != "alice" || in.PeerKind != "group" ||
		in.Text != "hello there" || len(in.Mentions) != 1 || in.Mentions[0] != "bot1" {
		t.Fatalf("inbound = %+v", in)
	}

	// Typing opens a "thinking" stream on the callback's req_id; later
	// ticks don't open another one.
	if err := wc.SendTyping("grp"); err != nil {
		t.Fatal(err)
	}
	thinking := fake.next("aibot_respond_msg")
	think := streamOf(t, thinking)
	if thinking.Headers.ReqID != "cb-1" || think.Content != wecomThinking || think.Finish || think.ID == "" {
		t.Fatalf("thinking = %s %s", thinking.Headers.ReqID, thinking.Body)
	}
	_ = wc.SendTyping("grp")

	// First bubble finishes that same stream; the second is pushed.
	if err := wc.SendMessage(bus.OutboundMessage{ChatID: "grp", Text: "one" + SplitMessageMarker + "two", AllowSplit: true}); err != nil {
		t.Fatal(err)
	}
	reply := fake.next("aibot_respond_msg")
	if st := streamOf(t, reply); reply.Headers.ReqID != "cb-1" || st.ID != think.ID || st.Content != "one" || !st.Finish {
		t.Fatalf("reply = %s %s", reply.Headers.ReqID, reply.Body)
	}
	push := fake.next("aibot_send_msg")
	if !strings.Contains(string(push.Body), `"chatid":"grp"`) || !strings.Contains(string(push.Body), `"content":"two"`) {
		t.Fatalf("push = %s", push.Body)
	}

	// No pending inbound left: a later message is pushed; media goes
	// through the upload flow.
	png := append([]byte("\x89PNG\r\n\x1a\n"), bytes.Repeat([]byte{0}, 64)...)
	if err := wc.SendMessage(bus.OutboundMessage{ChatID: "grp", Text: "later", MediaItems: []bus.MediaItem{{Filename: "a.png", Bytes: png}}}); err != nil {
		t.Fatal(err)
	}
	fake.next("aibot_send_msg")
	fake.next("aibot_upload_media_init")
	chunk := fake.next("aibot_upload_media_chunk")
	if !strings.Contains(string(chunk.Body), `"chunk_index":0`) {
		t.Fatalf("chunk = %s", chunk.Body)
	}
	fake.next("aibot_upload_media_finish")
	img := fake.next("aibot_send_msg")
	if !strings.Contains(string(img.Body), `"msgtype":"image"`) || !strings.Contains(string(img.Body), `"media_id":"m1"`) {
		t.Fatalf("image push = %s", img.Body)
	}

	// Superseded: the adapter must not reconnect and kick the new client.
	fake.push(conn, map[string]any{
		"cmd":     "aibot_event_callback",
		"headers": map[string]string{"req_id": "ev-1"},
		"body":    map[string]any{"msgtype": "event", "event": map[string]string{"eventtype": "disconnected_event"}},
	})
	select {
	case <-fake.conns:
		t.Fatal("adapter reconnected after disconnected_event")
	case <-time.After(1500 * time.Millisecond):
	}
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Start did not return after cancel")
	}
}

type wecomStreamBody struct {
	ID      string `json:"id"`
	Finish  bool   `json:"finish"`
	Content string `json:"content"`
}

func streamOf(t *testing.T, f wecomFrame) wecomStreamBody {
	t.Helper()
	var b struct {
		Stream wecomStreamBody `json:"stream"`
	}
	if err := json.Unmarshal(f.Body, &b); err != nil {
		t.Fatalf("stream body %s: %v", f.Body, err)
	}
	return b.Stream
}

func sendCallback(fake *fakeWeCom, conn *websocket.Conn, reqID, msgID, user, text string) {
	fake.push(conn, map[string]any{
		"cmd":     "aibot_msg_callback",
		"headers": map[string]string{"req_id": reqID},
		"body": map[string]any{
			"msgid": msgID, "aibotid": "bot1", "chattype": "single",
			"from": map[string]string{"userid": user}, "msgtype": "text",
			"text": map[string]string{"content": text},
		},
	})
}

// A thinking stream nobody answers must still be closed with visible
// content: when a newer message takes the reply slot, and when typing
// stops without a reply.
func TestWeComThinkingStreamClosedWithoutReply(t *testing.T) {
	fake := newFakeWeCom(t, "s3cret")
	mb := bus.New()
	wc, _ := NewWeCom("bot1", "s3cret", mb)
	wc.wsURL = fake.url()
	wc.typingIdle, wc.streamWatch = 300*time.Millisecond, 50*time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = wc.Start(ctx) }()
	fake.next("aibot_subscribe")
	conn := <-fake.conns

	inbound := func() {
		t.Helper()
		select {
		case <-mb.Inbound:
		case <-time.After(3 * time.Second):
			t.Fatal("no inbound message")
		}
	}

	sendCallback(fake, conn, "cb-a", "m-a", "bob", "first")
	inbound()
	_ = wc.SendTyping("bob")
	first := streamOf(t, fake.next("aibot_respond_msg"))

	// A second message in the same chat supersedes the first: its
	// thinking stream is closed right away.
	sendCallback(fake, conn, "cb-b", "m-b", "bob", "second")
	inbound()
	closed := fake.next("aibot_respond_msg")
	if st := streamOf(t, closed); closed.Headers.ReqID != "cb-a" || st.ID != first.ID || !st.Finish || st.Content != wecomCloseNoReply {
		t.Fatalf("superseded close = %s %s", closed.Headers.ReqID, closed.Body)
	}

	// The turn for the second message ends with no reply: once typing
	// goes idle the watchdog closes its stream.
	_ = wc.SendTyping("bob")
	second := streamOf(t, fake.next("aibot_respond_msg"))
	idle := fake.next("aibot_respond_msg")
	if st := streamOf(t, idle); idle.Headers.ReqID != "cb-b" || st.ID != second.ID || !st.Finish || st.Content != wecomCloseNoReply {
		t.Fatalf("idle close = %s %s", idle.Headers.ReqID, idle.Body)
	}

	// The slot is gone, so a late reply is pushed instead of answering.
	if err := wc.SendMessage(bus.OutboundMessage{ChatID: "bob", Text: "late"}); err != nil {
		t.Fatal(err)
	}
	if push := fake.next("aibot_send_msg"); !strings.Contains(string(push.Body), `"content":"late"`) {
		t.Fatalf("late push = %s", push.Body)
	}
}

func TestWeComDialRejectsBadSecret(t *testing.T) {
	fake := newFakeWeCom(t, "right")
	_, err := wecomDial(context.Background(), fake.url(), "bot1", "wrong")
	if err == nil || !strings.Contains(err.Error(), "authentication failed") {
		t.Fatalf("err = %v", err)
	}
}

func TestWeComDecrypt(t *testing.T) {
	key := bytes.Repeat([]byte{7}, 32)
	plain := []byte("hello wecom attachment")
	pad := 32 - len(plain)%32
	padded := append(append([]byte{}, plain...), bytes.Repeat([]byte{byte(pad)}, pad)...)
	block, _ := aes.NewCipher(key)
	enc := make([]byte, len(padded))
	cipher.NewCBCEncrypter(block, key[:16]).CryptBlocks(enc, padded)

	got, err := wecomDecrypt(enc, base64.StdEncoding.EncodeToString(key))
	if err != nil || string(got) != string(plain) {
		t.Fatalf("decrypt = %q, %v", got, err)
	}
	if _, err := wecomDecrypt(enc[:16], base64.StdEncoding.EncodeToString(key)); err == nil {
		t.Fatal("expected error on truncated ciphertext")
	}
}

func TestWeComSplitContent(t *testing.T) {
	long := strings.Repeat("企业微信", 3000) // 36000 bytes, no newlines
	parts := wecomSplitContent(long)
	if len(parts) != 2 || strings.Join(parts, "") != long {
		t.Fatalf("split into %d parts, rejoin ok=%v", len(parts), strings.Join(parts, "") == long)
	}
	for _, p := range parts {
		if len(p) > wecomMaxContentBytes {
			t.Fatalf("part too long: %d", len(p))
		}
	}
	if got := wecomSplitContent("  "); got != nil {
		t.Fatalf("blank = %v", got)
	}
}
