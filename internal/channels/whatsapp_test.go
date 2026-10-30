package channels

import (
	"testing"

	"go.mau.fi/whatsmeow/proto/waE2E"
	"google.golang.org/protobuf/proto"
)

func TestWhatsAppFormat(t *testing.T) {
	in := "# Title\nSome **bold** and ~~gone~~, see [docs](https://x.dev/a).\n```\ncode **stays**\n```"
	want := "*Title*\nSome *bold* and ~gone~, see docs (https://x.dev/a).\n```\ncode *stays*\n```"
	if got := whatsappFormat(in); got != want {
		t.Fatalf("got  %q\nwant %q", got, want)
	}
}

func TestWhatsAppMentions(t *testing.T) {
	self := map[string]bool{"8613800000000": true, "123456789": true} // phone + LID

	text, ok := waStripMentions("@8613800000000 hello", &waE2E.ContextInfo{
		MentionedJID: []string{"8613800000000@s.whatsapp.net"},
	}, self)
	if !ok || text != " hello" {
		t.Fatalf("phone mention: %q %v", text, ok)
	}

	// Groups increasingly mention by LID.
	if text, ok := waStripMentions("hi @123456789", &waE2E.ContextInfo{
		MentionedJID: []string{"123456789@lid"},
	}, self); !ok || text != "hi " {
		t.Fatalf("lid mention: %q %v", text, ok)
	}

	// Replying to one of the bot's messages addresses it too.
	if _, ok := waStripMentions("thanks", &waE2E.ContextInfo{
		StanzaID: proto.String("ABC"), Participant: proto.String("8613800000000@s.whatsapp.net"),
	}, self); !ok {
		t.Fatal("reply to bot not treated as addressed")
	}

	// Mentions of someone else don't.
	if text, ok := waStripMentions("@447700900000 hi", &waE2E.ContextInfo{
		MentionedJID: []string{"447700900000@s.whatsapp.net"},
	}, self); ok || text != "@447700900000 hi" {
		t.Fatalf("other mention: %q %v", text, ok)
	}
	if _, ok := waStripMentions("plain", nil, self); ok {
		t.Fatal("nil context addressed")
	}
}

func TestNewWhatsAppRejectsBadDevice(t *testing.T) {
	if _, err := NewWhatsApp("", "x", nil); err == nil {
		t.Fatal("empty device id accepted")
	}
	wa, err := NewWhatsApp("8613800000000:12@s.whatsapp.net", "8613800000000", nil)
	if err != nil || wa.BotUsername() != "8613800000000" {
		t.Fatalf("%v %v", wa, err)
	}
}
