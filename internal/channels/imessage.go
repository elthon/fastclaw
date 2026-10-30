package channels

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	_ "modernc.org/sqlite"

	"github.com/fastclaw-ai/fastclaw/internal/bus"
)

// iMessage adapter for self-hosted fastclaw on macOS. Apple has no bot
// API, so the Mac fastclaw runs on *is* the bot: inbound is read by
// polling Messages.app's local database (~/Library/Messages/chat.db,
// needs Full Disk Access), outbound is sent by scripting Messages.app
// via osascript (needs Automation → Messages, prompted once).
//
// One account per machine (accountID "local") — it speaks as whatever
// Apple ID is signed into Messages. Direct messages only: iMessage has
// no @-mention for bots, so in group chats the agent would answer every
// message.

const (
	IMessageLocalAccountID = "local"

	imessagePollInterval   = 1500 * time.Millisecond
	imessageSendTimeout    = 60 * time.Second
	imessageMaxAttachBytes = 25 * 1024 * 1024
	// Incoming attachments may still be downloading when the message
	// row appears; wait this long for the file before giving up on it.
	imessageAttachWait = 15 * time.Second
	// chat.style for 1:1 chats (43 = group).
	imessageDMStyle = 45
	// U+FFFC marks where an inline attachment sits in message text.
	objectReplacementChar = "￼"
)

// IMessageSupported reports whether this process can host the local
// iMessage channel at all (macOS only).
func IMessageSupported() bool { return runtime.GOOS == "darwin" }

// IMessageChatDBPath is Messages.app's database for the current user.
func IMessageChatDBPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, "Library", "Messages", "chat.db")
}

type IMessage struct {
	bus       *bus.MessageBus
	accountID string
	dbPath    string
	// sendScript runs an AppleScript with argv; swapped out in tests.
	sendScript func(ctx context.Context, script string, args ...string) error
}

func NewIMessage(accountID string, mb *bus.MessageBus) (*IMessage, error) {
	if !IMessageSupported() {
		return nil, errors.New("imessage: only available when fastclaw runs on macOS")
	}
	return &IMessage{
		bus:        mb,
		accountID:  accountID,
		dbPath:     IMessageChatDBPath(),
		sendScript: runOSAScript,
	}, nil
}

func (m *IMessage) Name() string        { return "imessage" }
func (m *IMessage) AccountID() string   { return m.accountID }
func (m *IMessage) BotUsername() string { return "" }

// Start tails chat.db from the current newest message — history is
// never replayed — and blocks until ctx is cancelled. Messages that
// arrive while fastclaw is down are not picked up after restart.
func (m *IMessage) Start(ctx context.Context) error {
	db, err := openIMessageDB(m.dbPath)
	if err != nil {
		return err
	}
	defer db.Close()

	var last int64
	if err := db.QueryRowContext(ctx, `SELECT COALESCE(MAX(ROWID), 0) FROM message`).Scan(&last); err != nil {
		return fmt.Errorf("imessage: read chat.db (Full Disk Access granted?): %w", err)
	}
	slog.Info("imessage watching chat.db", "account", m.accountID, "from_rowid", last)

	ticker := time.NewTicker(imessagePollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			next, err := m.poll(ctx, db, last)
			if err != nil {
				slog.Warn("imessage poll failed", "account", m.accountID, "error", err)
				continue
			}
			last = next
		}
	}
}

type imessageRow struct {
	rowID          int64
	guid           string
	text           sql.NullString
	attributedBody []byte
	handle         string
	chatGUID       string
	chatStyle      int
	hasAttachments bool
}

// poll dispatches every inbound DM with ROWID > after and returns the
// new high-water mark.
func (m *IMessage) poll(ctx context.Context, db *sql.DB, after int64) (int64, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT m.ROWID, m.guid, m.text, m.attributedBody, COALESCE(h.id, ''),
		       c.guid, c.style, m.cache_has_attachments, m.is_from_me,
		       m.associated_message_type, m.item_type
		FROM message m
		JOIN chat_message_join cmj ON cmj.message_id = m.ROWID
		JOIN chat c ON c.ROWID = cmj.chat_id
		LEFT JOIN handle h ON h.ROWID = m.handle_id
		WHERE m.ROWID > ?
		ORDER BY m.ROWID
		LIMIT 200`, after)
	if err != nil {
		return after, err
	}
	var batch []imessageRow
	last := after
	for rows.Next() {
		var r imessageRow
		var fromMe, assocType, itemType int
		if err := rows.Scan(&r.rowID, &r.guid, &r.text, &r.attributedBody, &r.handle,
			&r.chatGUID, &r.chatStyle, &r.hasAttachments, &fromMe, &assocType, &itemType); err != nil {
			rows.Close()
			return last, err
		}
		if r.rowID > last {
			last = r.rowID
		}
		// Skip our own sends (also prevents reply loops), tapbacks /
		// associated events, group system events, and group chats.
		if fromMe != 0 || assocType != 0 || itemType != 0 || r.chatStyle != imessageDMStyle {
			continue
		}
		batch = append(batch, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return after, err
	}
	for _, r := range batch {
		m.dispatch(ctx, db, r)
	}
	return last, nil
}

func (m *IMessage) dispatch(ctx context.Context, db *sql.DB, r imessageRow) {
	text := r.text.String
	if text == "" && len(r.attributedBody) > 0 {
		text = decodeAttributedBody(r.attributedBody)
	}
	text = strings.TrimSpace(strings.ReplaceAll(text, objectReplacementChar, ""))

	var media []bus.MediaItem
	var imageURLs []string
	if r.hasAttachments {
		// Images go to the model as vision input (PhotoURLs, same as
		// WeChat); everything else is saved to the workspace as a file.
		for _, item := range m.loadAttachments(ctx, db, r.rowID) {
			if dataURL, ok := imessageImageDataURL(ctx, item); ok {
				imageURLs = append(imageURLs, dataURL)
				continue
			}
			media = append(media, item)
		}
	}
	if text == "" && len(media) == 0 && len(imageURLs) == 0 {
		return
	}
	// Image-only messages: give the model a text cue so it acts on the
	// image rather than seeing an empty turn.
	if text == "" && len(imageURLs) > 0 {
		text = "[image]"
	}
	sender := r.handle
	if sender == "" {
		sender = imessageHandleFromChatGUID(r.chatGUID)
	}
	slog.Info("imessage message received", "account", m.accountID, "from", sender,
		"chat", r.chatGUID, "len", len(text), "images", len(imageURLs), "files", len(media))
	m.bus.Inbound <- bus.InboundMessage{
		Channel:    "imessage",
		AccountID:  m.accountID,
		ChatID:     r.chatGUID,
		UserID:     sender,
		MessageID:  r.guid,
		Text:       text,
		PhotoURLs:  imageURLs,
		MediaItems: media,
		PeerKind:   "dm",
	}
}

// imessageVisionMaxBytes caps images passed through untouched; larger
// ones (and every format models can't read, e.g. iPhone HEIC) are
// re-encoded as JPEG.
const imessageVisionMaxBytes = 4 * 1024 * 1024

// imessageImageDataURL turns an image attachment into a data URL a
// vision model accepts. PNG/JPEG/WebP/GIF under the size cap pass
// through; anything else (HEIC/HEIF/TIFF, oversized photos) is
// converted with macOS's built-in `sips` to a ≤2048px JPEG. Returns
// false for non-images or when conversion fails, so the caller falls
// back to treating it as a plain file.
func imessageImageDataURL(ctx context.Context, item bus.MediaItem) (string, bool) {
	ct := strings.ToLower(item.ContentType)
	if ct == "" {
		ct = http.DetectContentType(item.Bytes)
	}
	ext := strings.ToLower(filepath.Ext(item.Filename))
	if !strings.HasPrefix(ct, "image/") && ext != ".heic" && ext != ".heif" {
		return "", false
	}
	switch ct {
	case "image/png", "image/jpeg", "image/webp", "image/gif":
		if len(item.Bytes) <= imessageVisionMaxBytes {
			return "data:" + ct + ";base64," + base64.StdEncoding.EncodeToString(item.Bytes), true
		}
	}
	jpeg, err := convertToJPEG(ctx, item.Bytes, ext)
	if err != nil {
		slog.Warn("imessage image conversion failed", "filename", item.Filename, "type", ct, "error", err)
		return "", false
	}
	return "data:image/jpeg;base64," + base64.StdEncoding.EncodeToString(jpeg), true
}

// convertToJPEG re-encodes an image with /usr/bin/sips (ships with
// macOS), downscaling so the longest edge is at most 2048px.
func convertToJPEG(ctx context.Context, data []byte, ext string) ([]byte, error) {
	dir, err := os.MkdirTemp("", "fastclaw-imessage-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	if ext == "" {
		ext = ".img"
	}
	in := filepath.Join(dir, "in"+ext)
	out := filepath.Join(dir, "out.jpg")
	if err := os.WriteFile(in, data, 0o600); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "/usr/bin/sips", "-s", "format", "jpeg",
		"-s", "formatOptions", "85", "-Z", "2048", in, "--out", out)
	if b, err := cmd.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("sips: %w: %s", err, strings.TrimSpace(string(b)))
	}
	return os.ReadFile(out)
}

func (m *IMessage) loadAttachments(ctx context.Context, db *sql.DB, messageRowID int64) []bus.MediaItem {
	rows, err := db.QueryContext(ctx, `
		SELECT COALESCE(a.filename, ''), COALESCE(a.mime_type, ''),
		       COALESCE(a.transfer_name, ''), COALESCE(a.total_bytes, 0)
		FROM message_attachment_join maj
		JOIN attachment a ON a.ROWID = maj.attachment_id
		WHERE maj.message_id = ?`, messageRowID)
	if err != nil {
		slog.Warn("imessage attachment query failed", "account", m.accountID, "error", err)
		return nil
	}
	type att struct {
		path, mime, name string
		size             int64
	}
	var atts []att
	for rows.Next() {
		var a att
		if err := rows.Scan(&a.path, &a.mime, &a.name, &a.size); err == nil {
			atts = append(atts, a)
		}
	}
	rows.Close()

	var out []bus.MediaItem
	for _, a := range atts {
		// Rich-link previews ship opaque .pluginPayloadAttachment blobs
		// alongside the URL text — not user content.
		if a.path == "" || a.size > imessageMaxAttachBytes ||
			strings.HasSuffix(strings.ToLower(a.path), ".pluginpayloadattachment") {
			continue
		}
		path := expandHome(a.path)
		data, err := waitAndReadFile(ctx, path, imessageAttachWait)
		if err != nil {
			slog.Warn("imessage attachment unreadable", "account", m.accountID, "path", path, "error", err)
			continue
		}
		name := a.name
		if name == "" {
			name = filepath.Base(path)
		}
		out = append(out, bus.MediaItem{Filename: name, ContentType: a.mime, Bytes: data})
	}
	return out
}

func waitAndReadFile(ctx context.Context, path string, wait time.Duration) ([]byte, error) {
	deadline := time.Now().Add(wait)
	for {
		info, err := os.Stat(path)
		if err == nil && info.Size() > 0 {
			if info.Size() > imessageMaxAttachBytes {
				return nil, errors.New("attachment too large")
			}
			return os.ReadFile(path)
		}
		if time.Now().After(deadline) {
			if err == nil {
				err = errors.New("attachment still empty")
			}
			return nil, err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// --- Outbound ---

func (m *IMessage) Send(chatID, text string) error {
	return m.SendMessage(bus.OutboundMessage{ChatID: chatID, Text: text})
}

// SendMessage sends plain text (iMessage renders no markdown) and then
// each media item as a file. ChatID is the chat.db chat GUID.
func (m *IMessage) SendMessage(msg bus.OutboundMessage) error {
	if msg.Text == "" && len(msg.MediaItems) == 0 {
		return nil
	}
	plain := wechatStripMarkdown(FlattenMarkdownTables(msg.Text))
	if strings.TrimSpace(plain) != "" {
		if err := m.send(msg.ChatID, plain, ""); err != nil {
			return fmt.Errorf("imessage send: %w", err)
		}
	}
	for _, item := range msg.MediaItems {
		if len(item.Bytes) == 0 {
			continue
		}
		path, err := stageIMessageAttachment(item)
		if err != nil {
			slog.Warn("imessage stage attachment failed", "account", m.accountID, "error", err)
			continue
		}
		if err := m.send(msg.ChatID, "", path); err != nil {
			slog.Warn("imessage send attachment failed", "account", m.accountID,
				"chat", msg.ChatID, "filename", item.Filename, "error", err)
		}
	}
	return nil
}

// SendTyping is a no-op: Messages.app's AppleScript dictionary has no
// typing indicator.
func (m *IMessage) SendTyping(_ string) error { return nil }

// imessageSendScript resolves the chat by GUID and falls back to the
// 1:1 buddy on the iMessage service when Messages can't find the chat
// (-1728), mirroring steipete/imsg. Text and paths travel as argv so
// nothing needs AppleScript escaping.
const imessageSendScript = `on run argv
	set chatGUID to item 1 of argv
	set handle to item 2 of argv
	set theMessage to item 3 of argv
	set theFilePath to item 4 of argv
	tell application "Messages"
		try
			set target to chat id chatGUID
		on error number errNum
			if errNum is not -1728 or handle is "" then error number errNum
			set target to buddy handle of (first service whose service type is iMessage)
		end try
		if theMessage is not "" then send theMessage to target
		if theFilePath is not "" then send (POSIX file theFilePath as alias) to target
	end tell
end run`

func (m *IMessage) send(chatGUID, text, filePath string) error {
	ctx, cancel := context.WithTimeout(context.Background(), imessageSendTimeout)
	defer cancel()
	return m.sendScript(ctx, imessageSendScript, chatGUID, imessageHandleFromChatGUID(chatGUID), text, filePath)
}

// stageIMessageAttachment writes item under ~/Library/Messages/
// Attachments — Messages.app (sandboxed) reliably reads files there,
// unlike arbitrary temp dirs.
func stageIMessageAttachment(item bus.MediaItem) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	name := filepath.Base(item.Filename)
	if name == "" || name == "." || name == "/" {
		name = "attachment"
		if exts := mimeExtension(item); exts != "" {
			name += exts
		}
	}
	dir := filepath.Join(home, "Library", "Messages", "Attachments", "fastclaw", uuid.NewString())
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, item.Bytes, 0o644); err != nil {
		return "", err
	}
	return path, nil
}

func mimeExtension(item bus.MediaItem) string {
	ct := item.ContentType
	if ct == "" {
		ct = http.DetectContentType(item.Bytes)
	}
	switch {
	case strings.HasPrefix(ct, "image/png"):
		return ".png"
	case strings.HasPrefix(ct, "image/jpeg"):
		return ".jpg"
	case strings.HasPrefix(ct, "image/gif"):
		return ".gif"
	case strings.HasPrefix(ct, "application/pdf"):
		return ".pdf"
	}
	return ""
}

func runOSAScript(ctx context.Context, script string, args ...string) error {
	cmd := exec.CommandContext(ctx, "/usr/bin/osascript", append([]string{"-"}, args...)...)
	cmd.Stdin = strings.NewReader(script)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return fmt.Errorf("%w: %s", err, msg)
		}
		return err
	}
	return nil
}

// --- Permission probes (used by the connect handler) ---

// ErrIMessageNoDiskAccess means chat.db exists but can't be read —
// macOS Full Disk Access hasn't been granted to this process.
var ErrIMessageNoDiskAccess = errors.New("full disk access not granted")

// IMessageCheckDiskAccess verifies chat.db is readable.
func IMessageCheckDiskAccess(ctx context.Context) error {
	db, err := openIMessageDB(IMessageChatDBPath())
	if err != nil {
		return err
	}
	defer db.Close()
	var n int
	if err := db.QueryRowContext(ctx, `SELECT 1 FROM message LIMIT 1`).Scan(&n); err != nil && !errors.Is(err, sql.ErrNoRows) {
		if strings.Contains(err.Error(), "authorization denied") || strings.Contains(err.Error(), "unable to open") {
			return ErrIMessageNoDiskAccess
		}
		return err
	}
	return nil
}

// IMessageCheckAutomation asks Messages.app for a trivial property,
// which makes macOS show the one-time "fastclaw wants to control
// Messages" prompt now (while the user is at the dialog) rather than on
// the first reply.
func IMessageCheckAutomation(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	return runOSAScript(ctx, `tell application "Messages" to get name`)
}

func openIMessageDB(path string) (*sql.DB, error) {
	if _, err := os.Stat(filepath.Dir(path)); err != nil {
		return nil, fmt.Errorf("imessage: Messages data not found (%s): %w", filepath.Dir(path), err)
	}
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro&_pragma=busy_timeout(3000)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	return db, nil
}

// --- chat.db helpers ---

// imessageHandleFromChatGUID extracts the peer handle from a 1:1 chat
// GUID ("any;-;+15551234567" / "iMessage;-;a@b.c").
func imessageHandleFromChatGUID(guid string) string {
	parts := strings.SplitN(guid, ";", 3)
	if len(parts) == 3 && parts[1] == "-" {
		return parts[2]
	}
	return ""
}

func expandHome(p string) string {
	if strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, p[2:])
		}
	}
	return p
}

// decodeAttributedBody pulls the plain text out of message.attributedBody,
// an NSAttributedString archived as a typedstream. Newer macOS versions
// often leave message.text NULL and store the body only here. The first
// NSString in the stream is the body: marker 0x01 0x2B, then a length
// (one byte, or 0x81 + 2 bytes / 0x82 + 4 bytes), then UTF-8 bytes.
// Port of steipete/imsg's TypedStreamParser.
func decodeAttributedBody(b []byte) string {
	if len(b) < 13 || b[0] != 4 || b[1] != 11 {
		return ""
	}
	sig := string(b[2:13])
	little := sig == "streamtyped"
	if !little && sig != "typedstream" {
		return ""
	}
	for i := 13; i+1 < len(b); i++ {
		if b[i] != 0x01 || b[i+1] != 0x2b {
			continue
		}
		j := i + 2
		if j >= len(b) {
			return ""
		}
		head := b[j]
		j++
		var n int
		switch head {
		case 0x81, 0x82:
			width := 2
			if head == 0x82 {
				width = 4
			}
			if j+width > len(b) {
				return ""
			}
			if width == 2 {
				if little {
					n = int(binary.LittleEndian.Uint16(b[j:]))
				} else {
					n = int(binary.BigEndian.Uint16(b[j:]))
				}
			} else {
				if little {
					n = int(binary.LittleEndian.Uint32(b[j:]))
				} else {
					n = int(binary.BigEndian.Uint32(b[j:]))
				}
			}
			j += width
		default:
			if head >= 0x80 && head <= 0x91 {
				return ""
			}
			n = int(head)
		}
		if n < 0 || j+n > len(b) || !utf8.Valid(b[j:j+n]) {
			return ""
		}
		return string(b[j : j+n])
	}
	return ""
}
