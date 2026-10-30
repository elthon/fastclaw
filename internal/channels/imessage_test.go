package channels

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"image"
	"image/color"
	"image/png"
	"os/exec"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/fastclaw-ai/fastclaw/internal/bus"
)

// typedstreamBody builds a minimal attributedBody blob the way
// Messages archives it: header, then 0x01 0x2B + length + UTF-8.
func typedstreamBody(s string) []byte {
	b := []byte{4, 11}
	b = append(b, "streamtyped"...)
	b = append(b, 0x81, 0xe8, 0x03, 0x84, 0x01, 0x40, 0x84, 0x84, 0x84, 0x12)
	b = append(b, "NSAttributedString"...)
	b = append(b, 0x00, 0x84, 0x84, 0x08, 'N', 'S', 'O', 'b', 'j', 'e', 'c', 't', 0x00, 0x85, 0x92, 0x84, 0x84, 0x84, 0x08)
	b = append(b, "NSString"...)
	b = append(b, 0x01, 0x94, 0x84, 0x01, 0x2b)
	if n := len(s); n < 0x80 {
		b = append(b, byte(n))
	} else {
		b = append(b, 0x81, byte(n), byte(n>>8))
	}
	b = append(b, s...)
	return append(b, 0x86, 0x84)
}

func TestDecodeAttributedBody(t *testing.T) {
	long := strings.Repeat("长", 100) // 300 bytes → 0x81 two-byte length
	for _, s := range []string{"hello", "你好 👋", long} {
		if got := decodeAttributedBody(typedstreamBody(s)); got != s {
			t.Fatalf("decode(%q) = %q", s, got)
		}
	}
	if decodeAttributedBody([]byte("garbage")) != "" {
		t.Fatal("garbage should decode to empty")
	}
}

func TestIMessageHandleFromChatGUID(t *testing.T) {
	cases := map[string]string{
		"any;-;+15551234567":      "+15551234567",
		"iMessage;-;a@icloud.com": "a@icloud.com",
		"any;+;chat123":           "",
	}
	for in, want := range cases {
		if got := imessageHandleFromChatGUID(in); got != want {
			t.Errorf("%q → %q, want %q", in, got, want)
		}
	}
}

func newFakeChatDB(t *testing.T) (*sql.DB, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "chat.db")
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	for _, stmt := range []string{
		`CREATE TABLE handle (ROWID INTEGER PRIMARY KEY, id TEXT)`,
		`CREATE TABLE chat (ROWID INTEGER PRIMARY KEY, guid TEXT, style INTEGER)`,
		`CREATE TABLE message (ROWID INTEGER PRIMARY KEY, guid TEXT, text TEXT, attributedBody BLOB,
			handle_id INTEGER, is_from_me INTEGER, cache_has_attachments INTEGER,
			associated_message_type INTEGER, item_type INTEGER)`,
		`CREATE TABLE chat_message_join (chat_id INTEGER, message_id INTEGER)`,
		`CREATE TABLE attachment (ROWID INTEGER PRIMARY KEY, filename TEXT, mime_type TEXT, transfer_name TEXT, total_bytes INTEGER)`,
		`CREATE TABLE message_attachment_join (message_id INTEGER, attachment_id INTEGER)`,
		`INSERT INTO handle VALUES (1, '+15550001111')`,
		`INSERT INTO chat VALUES (1, 'any;-;+15550001111', 45), (2, 'any;+;chat99', 43)`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
	return db, path
}

func addMessage(t *testing.T, db *sql.DB, rowid int64, chat int, text any, body []byte, fromMe, hasAtt, assoc int) {
	t.Helper()
	if _, err := db.Exec(`INSERT INTO message VALUES (?, ?, ?, ?, 1, ?, ?, ?, 0)`,
		rowid, "g"+string(rune('0'+rowid)), text, body, fromMe, hasAtt, assoc); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO chat_message_join VALUES (?, ?)`, chat, rowid); err != nil {
		t.Fatal(err)
	}
}

func TestIMessagePollDispatchesOnlyInboundDMs(t *testing.T) {
	db, _ := newFakeChatDB(t)
	attPath := filepath.Join(t.TempDir(), "photo.png")
	if err := os.WriteFile(attPath, []byte("png-data"), 0o644); err != nil {
		t.Fatal(err)
	}

	addMessage(t, db, 1, 1, "plain text", nil, 0, 0, 0)
	addMessage(t, db, 2, 1, nil, typedstreamBody("from attributedBody"), 0, 0, 0)
	addMessage(t, db, 3, 1, "my own reply", nil, 1, 0, 0)   // is_from_me
	addMessage(t, db, 4, 2, "group chatter", nil, 0, 0, 0)  // group chat
	addMessage(t, db, 5, 1, "Loved “hi”", nil, 0, 0, 2000) // tapback
	addMessage(t, db, 6, 1, objectReplacementChar, nil, 0, 1, 0)
	if _, err := db.Exec(`INSERT INTO attachment VALUES (1, ?, 'image/png', 'photo.png', 8)`, attPath); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO message_attachment_join VALUES (6, 1)`); err != nil {
		t.Fatal(err)
	}

	mb := bus.New()
	m := &IMessage{bus: mb, accountID: "local"}
	done := make(chan int64, 1)
	go func() {
		last, err := m.poll(context.Background(), db, 0)
		if err != nil {
			t.Error(err)
		}
		done <- last
	}()

	var got []bus.InboundMessage
	for len(got) < 3 {
		select {
		case msg := <-mb.Inbound:
			got = append(got, msg)
		case <-time.After(3 * time.Second):
			t.Fatalf("got %d messages, want 3: %#v", len(got), got)
		}
	}
	if last := <-done; last != 6 {
		t.Fatalf("high-water mark = %d, want 6", last)
	}
	if got[0].Text != "plain text" || got[1].Text != "from attributedBody" {
		t.Fatalf("texts = %q, %q", got[0].Text, got[1].Text)
	}
	for _, msg := range got {
		if msg.Channel != "imessage" || msg.ChatID != "any;-;+15550001111" || msg.UserID != "+15550001111" || msg.PeerKind != "dm" {
			t.Fatalf("inbound = %#v", msg)
		}
	}
	// "png-data" sniffs as text/plain, but the attachment row says
	// image/png, so it's routed to vision input as-is.
	if got[2].Text != "[image]" || len(got[2].MediaItems) != 0 || len(got[2].PhotoURLs) != 1 ||
		got[2].PhotoURLs[0] != "data:image/png;base64,"+base64.StdEncoding.EncodeToString([]byte("png-data")) {
		t.Fatalf("attachment message = %#v", got[2])
	}
	select {
	case extra := <-mb.Inbound:
		t.Fatalf("unexpected inbound %#v", extra)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestIMessageSendPassesArgsToScript(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	var calls [][]string
	m := &IMessage{accountID: "local", sendScript: func(_ context.Context, script string, args ...string) error {
		if !strings.Contains(script, `tell application "Messages"`) {
			t.Fatalf("unexpected script %q", script)
		}
		calls = append(calls, args)
		return nil
	}}
	err := m.SendMessage(bus.OutboundMessage{
		ChatID:     "any;-;+15550001111",
		Text:       "**hi** there",
		MediaItems: []bus.MediaItem{{Filename: "r.pdf", Bytes: []byte("%PDF")}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(calls) != 2 {
		t.Fatalf("calls = %v", calls)
	}
	if calls[0][0] != "any;-;+15550001111" || calls[0][1] != "+15550001111" || calls[0][2] != "hi there" || calls[0][3] != "" {
		t.Fatalf("text call = %q", calls[0])
	}
	staged := calls[1][3]
	if !strings.Contains(staged, filepath.Join("Library", "Messages", "Attachments", "fastclaw")) || filepath.Base(staged) != "r.pdf" {
		t.Fatalf("staged path = %q", staged)
	}
	if b, err := os.ReadFile(staged); err != nil || string(b) != "%PDF" {
		t.Fatalf("staged content = %q, %v", b, err)
	}
}

func TestIMessageImageDataURLConvertsHEIC(t *testing.T) {
	if _, err := exec.LookPath("/usr/bin/sips"); err != nil {
		t.Skip("sips not available (non-macOS)")
	}
	img := image.NewRGBA(image.Rect(0, 0, 64, 48))
	for x := 0; x < 64; x++ {
		for y := 0; y < 48; y++ {
			img.Set(x, y, color.RGBA{uint8(x * 4), uint8(y * 5), 120, 255})
		}
	}
	dir := t.TempDir()
	pngPath := filepath.Join(dir, "in.png")
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pngPath, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	heicPath := filepath.Join(dir, "IMG_0001.HEIC")
	if out, err := exec.Command("/usr/bin/sips", "-s", "format", "heic", pngPath, "--out", heicPath).CombinedOutput(); err != nil {
		t.Skipf("sips cannot write HEIC here: %v %s", err, out)
	}
	heic, err := os.ReadFile(heicPath)
	if err != nil {
		t.Fatal(err)
	}

	url, ok := imessageImageDataURL(t.Context(), bus.MediaItem{Filename: "IMG_0001.HEIC", ContentType: "image/heic", Bytes: heic})
	if !ok || !strings.HasPrefix(url, "data:image/jpeg;base64,") {
		t.Fatalf("ok=%v url prefix=%.40q", ok, url)
	}
	jpeg, _ := base64.StdEncoding.DecodeString(strings.TrimPrefix(url, "data:image/jpeg;base64,"))
	if len(jpeg) < 3 || jpeg[0] != 0xFF || jpeg[1] != 0xD8 {
		t.Fatal("converted bytes are not a JPEG")
	}

	if _, ok := imessageImageDataURL(t.Context(), bus.MediaItem{Filename: "r.pdf", ContentType: "application/pdf", Bytes: []byte("%PDF-1.4")}); ok {
		t.Fatal("pdf should not become vision input")
	}
}
