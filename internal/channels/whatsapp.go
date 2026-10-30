package channels

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
	"google.golang.org/protobuf/proto"
	_ "modernc.org/sqlite" // whatsmeow device store

	"github.com/fastclaw-ai/fastclaw/internal/bus"
	"github.com/fastclaw-ai/fastclaw/internal/config"
)

// WhatsApp adapter for a personal (or dedicated) WhatsApp number linked
// as a companion device — the same mechanism as WhatsApp Web: the user
// scans a QR code under Settings → Linked devices. Built on whatsmeow,
// an unofficial implementation of the multi-device protocol, so it can
// break when WhatsApp changes the protocol and automated use can get a
// number banned; the connect dialog says so.
//
// Device keys/sessions live in their own sqlite file
// ($FASTCLAW_HOME/whatsapp.db), not the main database: whatsmeow owns
// that schema and migrates it itself.
//
// AccountID is the phone number (JID user); AccountConfig.UserID stores
// the full device JID ("<number>:<device>@s.whatsapp.net") used to load
// the device from the store.

const (
	whatsappMaxBacklog   = 10 * time.Minute // skip older messages delivered after an outage
	whatsappMaxTextRunes = 60000
	whatsappLoginTimeout = 3 * time.Minute
)

var (
	waStoreOnce sync.Once
	waStore     *sqlstore.Container
	waStoreErr  error

	// WhatsApp formatting: *bold* _italic_ ~strike~ ```mono```.
	waBoldRe    = regexp.MustCompile(`\*\*([^*\n]+)\*\*`)
	waStrikeRe  = regexp.MustCompile(`~~([^~\n]+)~~`)
	waHeadingRe = regexp.MustCompile(`(?m)^#{1,6}\s+(.+)$`)
	waLinkRe    = regexp.MustCompile(`\[([^\]\n]+)\]\((https?://[^)\s]+)\)`)
)

// WhatsAppStore opens (once) the whatsmeow device store.
func WhatsAppStore(ctx context.Context) (*sqlstore.Container, error) {
	waStoreOnce.Do(func() {
		home, err := config.HomeDir()
		if err != nil {
			waStoreErr = err
			return
		}
		if err := os.MkdirAll(home, 0o755); err != nil {
			waStoreErr = err
			return
		}
		dsn := "file:" + filepath.Join(home, "whatsapp.db") +
			"?_pragma=foreign_keys(1)&_pragma=busy_timeout(10000)&_pragma=journal_mode(WAL)"
		db, err := sql.Open("sqlite", dsn)
		if err != nil {
			waStoreErr = err
			return
		}
		c := sqlstore.NewWithDB(db, "sqlite3", waLogger("store"))
		if err := c.Upgrade(ctx); err != nil {
			_ = db.Close()
			waStoreErr = fmt.Errorf("whatsapp store: %w", err)
			return
		}
		waStore = c
	})
	return waStore, waStoreErr
}

// WhatsApp implements Channel for one linked WhatsApp number.
type WhatsApp struct {
	accountID string    // phone number
	deviceJID types.JID // companion device in the store
	bus       *bus.MessageBus

	mu          sync.Mutex
	client      *whatsmeow.Client
	typing      map[string]bool // chats with a composing presence up
	onLoggedOut func(accountID string)
}

// NewWhatsApp builds the adapter for a stored device JID.
func NewWhatsApp(deviceJID, accountID string, mb *bus.MessageBus) (*WhatsApp, error) {
	jid, err := types.ParseJID(deviceJID)
	if err != nil || jid.User == "" {
		return nil, fmt.Errorf("whatsapp: invalid device id %q", deviceJID)
	}
	return &WhatsApp{accountID: accountID, deviceJID: jid, bus: mb, typing: map[string]bool{}}, nil
}

// SetOnLoggedOut registers a callback for when the phone unlinks this
// device (or WhatsApp revokes it) — the session is gone for good.
func (w *WhatsApp) SetOnLoggedOut(fn func(accountID string)) { w.onLoggedOut = fn }

func (w *WhatsApp) Name() string      { return "whatsapp" }
func (w *WhatsApp) AccountID() string { return w.accountID }

// BotUsername is the phone number; group messages that @-mention it (or
// reply to it) carry the same value in Mentions.
func (w *WhatsApp) BotUsername() string { return w.accountID }

// Start connects the linked device and blocks until ctx is cancelled.
// whatsmeow reconnects on its own after network drops.
func (w *WhatsApp) Start(ctx context.Context) error {
	container, err := WhatsAppStore(ctx)
	if err != nil {
		return err
	}
	device, err := container.GetDevice(ctx, w.deviceJID)
	if err != nil {
		return fmt.Errorf("whatsapp: load device: %w", err)
	}
	if device == nil {
		return fmt.Errorf("whatsapp: device %s is no longer linked; reconnect by scanning the QR code again", w.deviceJID)
	}
	client := whatsmeow.NewClient(device, waLogger("client"))
	client.AddEventHandler(w.handleEvent)
	w.mu.Lock()
	w.client = client
	w.mu.Unlock()
	if err := client.Connect(); err != nil {
		return fmt.Errorf("whatsapp: connect: %w", err)
	}
	<-ctx.Done()
	client.Disconnect()
	return nil
}

// Unlink logs the device out: it disappears from the phone's linked
// devices and its keys are deleted. Called when the channel is
// disconnected from an agent.
func (w *WhatsApp) Unlink() {
	w.mu.Lock()
	client := w.client
	w.mu.Unlock()
	if client == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := client.Logout(ctx); err != nil {
		slog.Warn("whatsapp logout failed", "account", w.accountID, "error", err)
	}
	client.Disconnect()
}

func (w *WhatsApp) handleEvent(evt any) {
	switch v := evt.(type) {
	case *events.Message:
		go w.handleMessage(v)
	case *events.Connected:
		slog.Info("whatsapp connected", "account", w.accountID)
		// Sets the push name for others; "unavailable" keeps the phone
		// receiving notifications (an "available" companion mutes them).
		if c := w.currentClient(); c != nil {
			_ = c.SendPresence(context.Background(), types.PresenceUnavailable)
		}
	case *events.LoggedOut:
		slog.Warn("whatsapp device logged out", "account", w.accountID, "reason", v.Reason.String())
		if w.onLoggedOut != nil {
			w.onLoggedOut(w.accountID)
		}
	case *events.StreamReplaced:
		slog.Warn("whatsapp session opened elsewhere for this device", "account", w.accountID)
	}
}

func (w *WhatsApp) currentClient() *whatsmeow.Client {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.client
}

func (w *WhatsApp) handleMessage(evt *events.Message) {
	client := w.currentClient()
	if client == nil || evt.Info.IsFromMe || evt.IsEdit || evt.Message == nil {
		return
	}
	chat := evt.Info.Chat
	if chat.Server == types.BroadcastServer || chat.Server == types.NewsletterServer {
		return // status updates, channels
	}
	if time.Since(evt.Info.Timestamp) > whatsappMaxBacklog {
		return
	}
	ctx := context.Background()
	m := evt.Message

	var text string
	var info *waE2E.ContextInfo
	var media []bus.MediaItem
	download := func(kind string, msg whatsmeow.DownloadableMessage, name, mime string) {
		data, err := client.Download(ctx, msg)
		if err != nil {
			slog.Warn("whatsapp media download failed", "account", w.accountID, "type", kind, "error", err)
			return
		}
		if mime == "" {
			mime = http.DetectContentType(data)
		}
		if name == "" {
			name = kind + lineMediaExt(strings.SplitN(mime, ";", 2)[0])
		}
		media = append(media, bus.MediaItem{Filename: name, ContentType: mime, Bytes: data})
	}
	switch {
	case m.GetConversation() != "":
		text = m.GetConversation()
	case m.GetExtendedTextMessage() != nil:
		text, info = m.GetExtendedTextMessage().GetText(), m.GetExtendedTextMessage().GetContextInfo()
	case m.GetImageMessage() != nil:
		im := m.GetImageMessage()
		text, info = im.GetCaption(), im.GetContextInfo()
		download("image", im, "", im.GetMimetype())
	case m.GetVideoMessage() != nil:
		vm := m.GetVideoMessage()
		text, info = vm.GetCaption(), vm.GetContextInfo()
		download("video", vm, "", vm.GetMimetype())
	case m.GetAudioMessage() != nil:
		am := m.GetAudioMessage()
		info = am.GetContextInfo()
		download("audio", am, "", am.GetMimetype())
	case m.GetDocumentMessage() != nil:
		dm := m.GetDocumentMessage()
		text, info = dm.GetCaption(), dm.GetContextInfo()
		download("file", dm, dm.GetFileName(), dm.GetMimetype())
	default:
		return // stickers, reactions, polls, protocol messages, …
	}

	peerKind := "dm"
	var mentions []string
	if chat.Server == types.GroupServer {
		peerKind = "group"
		var addressed bool
		text, addressed = w.stripSelfMentions(client, text, info)
		if addressed {
			mentions = []string{w.BotUsername()}
		}
	}
	text = strings.TrimSpace(text)
	if text == "" && len(media) > 0 {
		text = "请查看我发送的附件。"
	}
	if text == "" {
		return
	}

	if peerKind == "dm" || len(mentions) > 0 {
		_ = client.MarkRead(ctx, []types.MessageID{evt.Info.ID}, evt.Info.Timestamp, chat, evt.Info.Sender)
	}
	slog.Info("whatsapp message received",
		"account", w.accountID, "chat", chat.String(), "peer_kind", peerKind, "mentioned", len(mentions) > 0)
	w.bus.Inbound <- bus.InboundMessage{
		Channel:    "whatsapp",
		AccountID:  w.accountID,
		ChatID:     chat.String(),
		UserID:     evt.Info.Sender.ToNonAD().String(),
		MessageID:  evt.Info.ID,
		Text:       text,
		PeerKind:   peerKind,
		SenderName: evt.Info.PushName,
		Mentions:   mentions,
		MediaItems: media,
	}
}

// stripSelfMentions reports whether a group message addresses this
// number — an @-mention (by phone or LID) or a reply to one of its
// messages — and removes the "@<number>" tokens from the text.
func (w *WhatsApp) stripSelfMentions(client *whatsmeow.Client, text string, info *waE2E.ContextInfo) (string, bool) {
	self := map[string]bool{}
	if id := client.Store.GetJID(); id.User != "" {
		self[id.User] = true
	}
	if lid := client.Store.GetLID(); lid.User != "" {
		self[lid.User] = true
	}
	return waStripMentions(text, info, self)
}

func waStripMentions(text string, info *waE2E.ContextInfo, self map[string]bool) (string, bool) {
	if info == nil {
		return text, false
	}
	addressed := false
	for _, raw := range info.GetMentionedJID() {
		jid, err := types.ParseJID(raw)
		if err != nil || !self[jid.User] {
			continue
		}
		addressed = true
		text = strings.ReplaceAll(text, "@"+jid.User, "")
	}
	if p := info.GetParticipant(); p != "" && info.GetStanzaID() != "" {
		if jid, err := types.ParseJID(p); err == nil && self[jid.User] {
			addressed = true
		}
	}
	return text, addressed
}

// Send sends a text message to a chat JID.
func (w *WhatsApp) Send(chatID, text string) error {
	return w.SendMessage(bus.OutboundMessage{ChatID: chatID, Text: text})
}

// SendMessage sends text bubbles (markdown converted to WhatsApp
// formatting) and media, then clears the composing presence.
func (w *WhatsApp) SendMessage(msg bus.OutboundMessage) error {
	client := w.currentClient()
	if client == nil {
		return errors.New("whatsapp: not connected")
	}
	jid, err := types.ParseJID(msg.ChatID)
	if err != nil {
		return fmt.Errorf("whatsapp: bad chat id %q: %w", msg.ChatID, err)
	}
	ctx := context.Background()
	defer w.stopTyping(client, msg.ChatID, jid)

	var bubbles []string
	if msg.AllowSplit {
		bubbles = SplitOutboundText(msg.Text)
	} else if t := strings.TrimSpace(strings.ReplaceAll(msg.Text, SplitMessageMarker, "\n")); t != "" {
		bubbles = []string{t}
	}
	var firstErr error
	note := func(err error) {
		if err != nil {
			slog.Warn("whatsapp send failed", "account", w.accountID, "chat", msg.ChatID, "error", err)
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	for _, b := range bubbles {
		for _, part := range whatsappSplit(whatsappFormat(b)) {
			_, err := client.SendMessage(ctx, jid, &waE2E.Message{Conversation: proto.String(part)})
			note(err)
		}
	}
	for _, item := range msg.MediaItems {
		note(w.sendMedia(ctx, client, jid, item))
	}
	return firstErr
}

func (w *WhatsApp) sendMedia(ctx context.Context, client *whatsmeow.Client, jid types.JID, item bus.MediaItem) error {
	if len(item.Bytes) == 0 {
		return errors.New("empty media")
	}
	mime := item.ContentType
	if mime == "" {
		mime = http.DetectContentType(item.Bytes)
	}
	kind := whatsmeow.MediaDocument
	switch {
	case mime == "image/jpeg" || mime == "image/png":
		kind = whatsmeow.MediaImage
	case strings.HasPrefix(mime, "video/mp4"):
		kind = whatsmeow.MediaVideo
	}
	up, err := client.Upload(ctx, item.Bytes, kind)
	if err != nil {
		return fmt.Errorf("upload %s: %w", item.Filename, err)
	}
	var m *waE2E.Message
	switch kind {
	case whatsmeow.MediaImage:
		m = &waE2E.Message{ImageMessage: &waE2E.ImageMessage{
			URL: proto.String(up.URL), DirectPath: proto.String(up.DirectPath), MediaKey: up.MediaKey,
			Mimetype: proto.String(mime), FileEncSHA256: up.FileEncSHA256, FileSHA256: up.FileSHA256,
			FileLength: proto.Uint64(up.FileLength),
		}}
	case whatsmeow.MediaVideo:
		m = &waE2E.Message{VideoMessage: &waE2E.VideoMessage{
			URL: proto.String(up.URL), DirectPath: proto.String(up.DirectPath), MediaKey: up.MediaKey,
			Mimetype: proto.String(mime), FileEncSHA256: up.FileEncSHA256, FileSHA256: up.FileSHA256,
			FileLength: proto.Uint64(up.FileLength),
		}}
	default:
		name := item.Filename
		if name == "" {
			name = "file" + lineMediaExt(mime)
		}
		m = &waE2E.Message{DocumentMessage: &waE2E.DocumentMessage{
			URL: proto.String(up.URL), DirectPath: proto.String(up.DirectPath), MediaKey: up.MediaKey,
			Mimetype: proto.String(mime), FileEncSHA256: up.FileEncSHA256, FileSHA256: up.FileSHA256,
			FileLength: proto.Uint64(up.FileLength), FileName: proto.String(name), Title: proto.String(name),
		}}
	}
	_, err = client.SendMessage(ctx, jid, m)
	return err
}

// SendTyping shows "typing…" in the chat. The companion goes
// "available" only while a turn runs (stopTyping reverts it), because an
// available companion mutes the phone's notifications.
func (w *WhatsApp) SendTyping(chatID string) error {
	client := w.currentClient()
	if client == nil {
		return nil
	}
	jid, err := types.ParseJID(chatID)
	if err != nil {
		return nil
	}
	ctx := context.Background()
	w.mu.Lock()
	first := !w.typing[chatID]
	w.typing[chatID] = true
	w.mu.Unlock()
	if first {
		_ = client.SendPresence(ctx, types.PresenceAvailable)
	}
	return client.SendChatPresence(ctx, jid, types.ChatPresenceComposing, types.ChatPresenceMediaText)
}

func (w *WhatsApp) stopTyping(client *whatsmeow.Client, chatID string, jid types.JID) {
	w.mu.Lock()
	was := w.typing[chatID]
	delete(w.typing, chatID)
	idle := len(w.typing) == 0
	w.mu.Unlock()
	if !was {
		return
	}
	ctx := context.Background()
	_ = client.SendChatPresence(ctx, jid, types.ChatPresencePaused, "")
	if idle {
		_ = client.SendPresence(ctx, types.PresenceUnavailable)
	}
}

// whatsappFormat converts the markdown agents write into WhatsApp's own
// formatting: **bold** → *bold*, ~~x~~ → ~x~, headings → bold lines,
// [text](url) → "text (url)". Tables are flattened; code fences already
// match.
func whatsappFormat(text string) string {
	text = FlattenMarkdownTables(text)
	text = waHeadingRe.ReplaceAllString(text, "*$1*")
	text = waBoldRe.ReplaceAllString(text, "*$1*")
	text = waStrikeRe.ReplaceAllString(text, "~$1~")
	text = waLinkRe.ReplaceAllString(text, "$1 ($2)")
	return text
}

func whatsappSplit(text string) []string {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil
	}
	runes := []rune(text)
	var out []string
	for len(runes) > whatsappMaxTextRunes {
		out = append(out, string(runes[:whatsappMaxTextRunes]))
		runes = runes[whatsappMaxTextRunes:]
	}
	return append(out, string(runes))
}

// --- QR login ---

// WhatsAppLogin is one in-progress QR pairing. WhatsApp rotates the
// code every ~20s, so callers poll Status and re-render.
type WhatsAppLogin struct {
	client *whatsmeow.Client
	cancel context.CancelFunc

	mu        sync.Mutex
	code      string
	status    string // "wait" | "confirmed" | "expired" | "error"
	errMsg    string
	deviceJID types.JID
}

// StartWhatsAppLogin creates a fresh companion device and starts
// pairing. The device is only written to the store once pairing succeeds.
func StartWhatsAppLogin(ctx context.Context) (*WhatsAppLogin, error) {
	container, err := WhatsAppStore(ctx)
	if err != nil {
		return nil, err
	}
	client := whatsmeow.NewClient(container.NewDevice(), waLogger("login"))
	qrCtx, cancel := context.WithTimeout(context.Background(), whatsappLoginTimeout)
	qrs, err := client.GetQRChannel(qrCtx)
	if err != nil {
		cancel()
		return nil, err
	}
	if err := client.Connect(); err != nil {
		cancel()
		return nil, fmt.Errorf("whatsapp: connect: %w", err)
	}
	l := &WhatsAppLogin{client: client, cancel: cancel, status: "wait"}
	go l.run(qrs)
	// Hand back once the first code is in, so the dialog has something
	// to show immediately.
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if code, status, _, _ := l.Status(); code != "" || status != "wait" {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	return l, nil
}

func (l *WhatsAppLogin) run(qrs <-chan whatsmeow.QRChannelItem) {
	for item := range qrs {
		l.mu.Lock()
		switch item.Event {
		case whatsmeow.QRChannelEventCode:
			l.code = item.Code
		case whatsmeow.QRChannelSuccess.Event:
			l.status = "confirmed"
			if id := l.client.Store.ID; id != nil {
				l.deviceJID = *id
			}
		case whatsmeow.QRChannelTimeout.Event:
			l.status = "expired"
		case whatsmeow.QRChannelEventPasskeyRequest:
			l.status, l.errMsg = "error", "this WhatsApp account asks for passkey verification when linking devices, which FastClaw doesn't support yet"
		case whatsmeow.QRChannelScannedWithoutMultidevice.Event:
			l.errMsg = "enable multi-device in WhatsApp, then scan again"
		default:
			if l.status == "wait" {
				l.status = "error"
				if item.Error != nil {
					l.errMsg = item.Error.Error()
				} else {
					l.errMsg = item.Event
				}
			}
		}
		done := l.status != "wait"
		l.mu.Unlock()
		if done {
			break
		}
	}
	l.mu.Lock()
	if l.status == "wait" {
		l.status = "expired"
	}
	l.mu.Unlock()
	// The adapter opens its own connection for the paired device.
	l.client.Disconnect()
	l.cancel()
}

// Status reports the current code, state, error message and — once
// confirmed — the paired device JID.
func (l *WhatsAppLogin) Status() (code, status, errMsg string, device types.JID) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.code, l.status, l.errMsg, l.deviceJID
}

// Abort stops a pending login; if pairing already succeeded the new
// device is logged out again (used when it can't be saved).
func (l *WhatsAppLogin) Abort() {
	l.cancel()
	_, status, _, device := l.Status()
	if status != "confirmed" || device.User == "" {
		l.client.Disconnect()
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if container, err := WhatsAppStore(ctx); err == nil {
		if d, err := container.GetDevice(ctx, device); err == nil && d != nil {
			c := whatsmeow.NewClient(d, waLogger("login"))
			if c.Connect() == nil {
				_ = c.Logout(ctx)
				c.Disconnect()
			}
		}
	}
}

// waLogger forwards whatsmeow's warnings and errors to slog; its info
// and debug output is far too chatty for the gateway log.
func waLogger(module string) waLog.Logger { return waSlog{module: module} }

type waSlog struct{ module string }

func (l waSlog) Errorf(msg string, args ...any) {
	slog.Error("whatsmeow: "+fmt.Sprintf(msg, args...), "module", l.module)
}
func (l waSlog) Warnf(msg string, args ...any) {
	slog.Warn("whatsmeow: "+fmt.Sprintf(msg, args...), "module", l.module)
}
func (l waSlog) Infof(string, ...any)           {}
func (l waSlog) Debugf(string, ...any)          {}
func (l waSlog) Sub(module string) waLog.Logger { return waSlog{module: l.module + "/" + module} }
