package channels

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fastclaw-ai/fastclaw/internal/bus"
)

// fakeLINE records Messaging API calls and serves message content.
type fakeLINE struct {
	srv     *httptest.Server
	mu      sync.Mutex
	calls   []fakeLINECall
	content map[string][]byte // message id → bytes from the data API
}

type fakeLINECall struct {
	Path string
	Body map[string]any
}

func newFakeLINE(t *testing.T) *fakeLINE {
	f := &fakeLINE{content: map[string][]byte{}}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/v2/bot/message/") && strings.HasSuffix(r.URL.Path, "/content") {
			id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v2/bot/message/"), "/content")
			f.mu.Lock()
			data, ok := f.content[id]
			f.mu.Unlock()
			if !ok {
				http.NotFound(w, r)
				return
			}
			_, _ = w.Write(data)
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		f.calls = append(f.calls, fakeLINECall{Path: r.URL.Path, Body: body})
		f.mu.Unlock()
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeLINE) take() []fakeLINECall {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := f.calls
	f.calls = nil
	return out
}

func newTestLINE(t *testing.T, f *fakeLINE, secret string) (*LINE, *bus.MessageBus) {
	t.Helper()
	t.Setenv("FASTCLAW_HOME", t.TempDir())
	mb := bus.New()
	ln, err := NewLINE("tok", secret, "Ubot", mb)
	if err != nil {
		t.Fatal(err)
	}
	ln.apiBase, ln.dataBase = f.srv.URL, f.srv.URL
	return ln, mb
}

func lineSign(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

func lineEvents(t *testing.T, events ...map[string]any) []byte {
	t.Helper()
	b, err := json.Marshal(map[string]any{"destination": "Ubot", "events": events})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func lineTextEvent(source map[string]any, id, text string, mention map[string]any) map[string]any {
	msg := map[string]any{"type": "text", "id": id, "text": text}
	if mention != nil {
		msg["mention"] = mention
	}
	return map[string]any{"type": "message", "replyToken": "rt-" + id, "source": source, "message": msg}
}

func nextInbound(t *testing.T, mb *bus.MessageBus) bus.InboundMessage {
	t.Helper()
	select {
	case m := <-mb.Inbound:
		return m
	case <-time.After(3 * time.Second):
		t.Fatal("no inbound message")
	}
	return bus.InboundMessage{}
}

func TestLINEWebhookRequiresSecretAndSignature(t *testing.T) {
	f := newFakeLINE(t)
	body := lineEvents(t, lineTextEvent(map[string]any{"type": "user", "userId": "Ualice"}, "1", "hi", nil))

	noSecret, _ := newTestLINE(t, f, "")
	if _, status, err := noSecret.HandleWebhook(body, lineSign("", body), ""); status != http.StatusUnauthorized || err == nil {
		t.Fatalf("no secret: status %d err %v", status, err)
	}

	ln, mb := newTestLINE(t, f, "sec")
	if _, status, _ := ln.HandleWebhook(body, lineSign("wrong", body), ""); status != http.StatusUnauthorized {
		t.Fatalf("bad signature: status %d", status)
	}
	if _, status, err := ln.HandleWebhook(body, lineSign("sec", body), "https://claw.example"); status != http.StatusOK || err != nil {
		t.Fatalf("good signature: status %d err %v", status, err)
	}
	in := nextInbound(t, mb)
	if in.ChatID != "Ualice" || in.PeerKind != "dm" || in.Text != "hi" || len(in.Mentions) != 0 {
		t.Fatalf("inbound = %+v", in)
	}
}

func TestLINEGroupMentions(t *testing.T) {
	f := newFakeLINE(t)
	ln, mb := newTestLINE(t, f, "sec")
	group := map[string]any{"type": "group", "groupId": "Cgrp", "userId": "Ualice"}

	// "😀 @Bot hi": the emoji is two UTF-16 units, so the mention starts
	// at index 3 — a rune-based strip would cut the wrong span.
	text := "😀 @Bot hi"
	self := map[string]any{"mentionees": []map[string]any{{"index": 3, "length": 4, "type": "user", "isSelf": true}}}
	body := lineEvents(t, lineTextEvent(group, "1", text, self))
	if _, status, err := ln.HandleWebhook(body, lineSign("sec", body), ""); status != http.StatusOK {
		t.Fatalf("status %d err %v", status, err)
	}
	in := nextInbound(t, mb)
	if in.PeerKind != "group" || in.ChatID != "Cgrp" || in.Text != "😀  hi" ||
		len(in.Mentions) != 1 || in.Mentions[0] != ln.BotUsername() {
		t.Fatalf("self mention inbound = %+v (bot %q)", in, ln.BotUsername())
	}

	// Mentioning someone else (or @All) doesn't address the bot.
	other := map[string]any{"mentionees": []map[string]any{
		{"index": 0, "length": 4, "type": "user", "userId": "Ubob"},
		{"index": 5, "length": 4, "type": "all"},
	}}
	body = lineEvents(t, lineTextEvent(group, "2", "@Bob @All yo", other))
	_, _, _ = ln.HandleWebhook(body, lineSign("sec", body), "")
	if in := nextInbound(t, mb); len(in.Mentions) != 0 || in.Text != "@Bob @All yo" {
		t.Fatalf("other mention inbound = %+v", in)
	}
}

func TestLINEInboundImage(t *testing.T) {
	f := newFakeLINE(t)
	ln, mb := newTestLINE(t, f, "sec")
	png := testPNG(t, 4, 4)
	f.content["img1"] = png
	body := lineEvents(t, map[string]any{
		"type": "message", "replyToken": "rt", "source": map[string]any{"type": "user", "userId": "Ualice"},
		"message": map[string]any{"type": "image", "id": "img1", "contentProvider": map[string]any{"type": "line"}},
	})
	if _, status, err := ln.HandleWebhook(body, lineSign("sec", body), ""); status != http.StatusOK {
		t.Fatalf("status %d err %v", status, err)
	}
	in := nextInbound(t, mb)
	if len(in.MediaItems) != 1 || !bytes.Equal(in.MediaItems[0].Bytes, png) || in.MediaItems[0].Filename != "image.png" {
		t.Fatalf("media = %+v", in.MediaItems)
	}
}

func TestLINEOutboundReplyPushAndMedia(t *testing.T) {
	f := newFakeLINE(t)
	ln, mb := newTestLINE(t, f, "sec")
	body := lineEvents(t, lineTextEvent(map[string]any{"type": "user", "userId": "Ualice"}, "1", "hi", nil))
	_, _, _ = ln.HandleWebhook(body, lineSign("sec", body), "https://claw.example")
	nextInbound(t, mb)

	// Six bubbles + an image + a PDF: the reply carries the first five
	// messages, the rest is pushed.
	img := testPNG(t, 8, 8)
	err := ln.SendMessage(bus.OutboundMessage{
		ChatID:     "Ualice",
		Text:       strings.Join([]string{"a", "b", "c", "d", "e", "f"}, SplitMessageMarker),
		AllowSplit: true,
		MediaItems: []bus.MediaItem{
			{Filename: "chart.png", Bytes: img},
			{Filename: "report.pdf", Bytes: []byte("%PDF-1.4 test")},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	calls := f.take()
	if len(calls) != 2 || calls[0].Path != "/v2/bot/message/reply" || calls[1].Path != "/v2/bot/message/push" {
		t.Fatalf("calls = %+v", calls)
	}
	if calls[0].Body["replyToken"] != "rt-1" || len(calls[0].Body["messages"].([]any)) != 5 {
		t.Fatalf("reply = %+v", calls[0].Body)
	}
	pushed := calls[1].Body["messages"].([]any)
	if calls[1].Body["to"] != "Ualice" || len(pushed) != 3 {
		t.Fatalf("push = %+v", calls[1].Body)
	}

	// The image message points at our media route under the webhook's
	// public base, and the URL serves the original bytes.
	imgMsg := pushed[1].(map[string]any)
	orig, _ := imgMsg["originalContentUrl"].(string)
	if imgMsg["type"] != "image" || !strings.HasPrefix(orig, "https://claw.example/api/line/media/Ubot/") || imgMsg["previewImageUrl"] != orig {
		t.Fatalf("image message = %+v", imgMsg)
	}
	u, _ := url.Parse(orig)
	data, ct, err := ln.ServeMedia(u.Path[strings.LastIndex(u.Path, "/")+1:])
	if err != nil || !bytes.Equal(data, img) || ct != "image/png" {
		t.Fatalf("ServeMedia = %d bytes %q %v", len(data), ct, err)
	}

	// Bots can't send files: the PDF becomes a download link.
	fileMsg := pushed[2].(map[string]any)
	if text, _ := fileMsg["text"].(string); fileMsg["type"] != "text" || !strings.HasPrefix(text, "📎 report.pdf\nhttps://claw.example/api/line/media/Ubot/") {
		t.Fatalf("file message = %+v", fileMsg)
	}

	// Names that aren't a stored token never resolve.
	for _, bad := range []string{"../../etc/passwd", "abc.png", ""} {
		if _, _, err := ln.ServeMedia(bad); err == nil {
			t.Fatalf("ServeMedia(%q) succeeded", bad)
		}
	}

	// The token was used up: a later message is pushed.
	if err := ln.SendMessage(bus.OutboundMessage{ChatID: "Ualice", Text: "later"}); err != nil {
		t.Fatal(err)
	}
	if calls := f.take(); len(calls) != 1 || calls[0].Path != "/v2/bot/message/push" {
		t.Fatalf("later calls = %+v", calls)
	}
}

func TestLINELargeImageGetsPreview(t *testing.T) {
	f := newFakeLINE(t)
	ln, _ := newTestLINE(t, f, "sec")
	ln.publicBase = "https://claw.example"
	big := testNoisyPNG(t, 1200, 900)
	if len(big) <= lineMaxPreviewBytes {
		t.Fatalf("test image only %d bytes", len(big))
	}
	m, err := ln.mediaMessage(bus.MediaItem{Bytes: big})
	if err != nil {
		t.Fatal(err)
	}
	preview, _ := m["previewImageUrl"].(string)
	if preview == m["originalContentUrl"] || !strings.HasSuffix(preview, ".jpg") {
		t.Fatalf("message = %+v", m)
	}
	data, _, err := ln.ServeMedia(preview[strings.LastIndex(preview, "/")+1:])
	if err != nil || len(data) > lineMaxPreviewBytes {
		t.Fatalf("preview %d bytes, %v", len(data), err)
	}
}

func TestLINELoadingAnimation(t *testing.T) {
	f := newFakeLINE(t)
	ln, _ := newTestLINE(t, f, "sec")
	_ = ln.SendTyping("Cgroup") // groups have no loading animation
	_ = ln.SendTyping("Ualice")
	_ = ln.SendTyping("Ualice") // throttled
	calls := f.take()
	if len(calls) != 1 || calls[0].Path != "/v2/bot/chat/loading/start" || calls[0].Body["chatId"] != "Ualice" {
		t.Fatalf("calls = %+v", calls)
	}
}

func TestLINESplitText(t *testing.T) {
	long := strings.Repeat("あ", lineMaxTextRunes+10)
	parts := lineSplitText(long)
	if len(parts) != 2 || strings.Join(parts, "") != long {
		t.Fatalf("split into %d parts", len(parts))
	}
}

func testPNG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// testNoisyPNG is incompressible enough to exceed the 1 MB preview cap.
func testNoisyPNG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	seed := uint32(1)
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			seed = seed*1664525 + 1013904223
			img.Set(x, y, color.RGBA{uint8(seed >> 24), uint8(seed >> 16), uint8(seed >> 8), 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}
