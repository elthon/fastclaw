package channels

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	_ "image/png" // register PNG for preview decoding
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf16"

	"github.com/fastclaw-ai/fastclaw/internal/bus"
	"github.com/fastclaw-ai/fastclaw/internal/config"
)

// LINE Messaging API adapter. Webhook-driven inbound + REST outbound,
// same shape as the Feishu adapter but with key differences:
//
//  1. Inbound auth is HMAC-SHA256(channel_secret, raw_body) compared
//     against the `x-line-signature` header, so the webhook handler
//     hands us the raw body bytes alongside the signature. The secret
//     is mandatory: the webhook URL is public, and without it anyone
//     who knows the bot's userId could post fabricated user messages.
//  2. Outbound has TWO send endpoints — `reply` (per-event replyToken,
//     free, single-use, short-lived) and `push` (no token, consumes the
//     bot's monthly quota). The first outbound after an inbound goes
//     through reply; the rest, and anything after expiry, is pushed.
//  3. Bots can't upload media: image messages carry HTTPS URLs LINE
//     fetches. We serve outbound media ourselves from
//     /api/line/media/<account>/<token>, under the public base URL the
//     latest verified webhook arrived on (that's the address LINE can
//     reach). Bots can't send files at all, so non-images go out as a
//     download link on the same route.
//
// AccountID is the bot's `userId` (stable per channel, returned by
// /v2/bot/info). AccountConfig.BotToken stores channel_access_token,
// AccountConfig.UserID stores channel_secret (matches the field's
// "extra account-scoped identifier" comment).

const (
	lineAPIBase       = "https://api.line.me"
	lineDataAPIBase   = "https://api-data.line.me"
	lineSendTimeout   = 15 * time.Second
	lineReplyTokenTTL = 4 * time.Minute // refresh under the server-side limit; reply failure falls back to push

	lineMaxMessagesPerCall = 5
	lineMaxTextRunes       = 4900 // API cap is 5000 characters
	lineMaxImageBytes      = 10 << 20
	lineMaxPreviewBytes    = 1 << 20
	linePreviewMaxSide     = 640
	lineMaxDownloadBytes   = 50 << 20
	lineMediaTTL           = 30 * 24 * time.Hour
	lineLoadingSeconds     = 20 // loading animation; multiple of 5, 5–60
	lineLoadingEvery       = 15 * time.Second
)

var lineMediaTokenRe = regexp.MustCompile(`^[0-9a-f]{32}$`)

// LINE implements the Channel interface for a LINE Messaging API bot.
type LINE struct {
	bus           *bus.MessageBus
	accountID     string // == bot userId (Uxxxxxxxxxxxxxxxx)
	channelToken  string
	channelSecret string

	httpClient *http.Client
	apiBase    string // overridable in tests
	dataBase   string
	mediaDir   string // outbound media served at /api/line/media/<account>/<token>

	mu         sync.Mutex
	botName    string
	basicID    string // "@xxx" handle, surfaced for display
	publicBase string // scheme://host the latest verified webhook arrived on
	// replyTokens caches the most recent inbound replyToken per chat.
	// Single-use. First outbound after an inbound pops the token;
	// subsequent messages in the same turn use the push API.
	replyTokens map[string]lineReplyToken
	lastLoading map[string]time.Time
	lastPrune   time.Time
}

type lineReplyToken struct {
	token   string
	expires time.Time
}

// NewLINE creates a LINE channel adapter from a stored credential pair.
func NewLINE(channelToken, channelSecret, accountID string, mb *bus.MessageBus) (*LINE, error) {
	if channelToken == "" {
		return nil, errors.New("line: channelToken required")
	}
	mediaDir := ""
	if home, err := config.HomeDir(); err == nil {
		mediaDir = filepath.Join(home, "line-media", accountID)
	}
	return &LINE{
		bus:           mb,
		accountID:     accountID,
		channelToken:  channelToken,
		channelSecret: channelSecret,
		httpClient:    &http.Client{Timeout: lineSendTimeout},
		apiBase:       lineAPIBase,
		dataBase:      lineDataAPIBase,
		mediaDir:      mediaDir,
		replyTokens:   make(map[string]lineReplyToken),
		lastLoading:   make(map[string]time.Time),
	}, nil
}

func (l *LINE) Name() string      { return "line" }
func (l *LINE) AccountID() string { return l.accountID }

// BotUsername is the bot's basicId ("@xxx"), or its userId until
// /v2/bot/info has answered. Group messages that @-mention the bot carry
// the same value in Mentions, so the gateway's mention routing matches.
func (l *LINE) BotUsername() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.basicID != "" {
		return l.basicID
	}
	return l.accountID
}

// Start fetches /v2/bot/info to surface the bot's display name + basicId,
// then blocks until ctx is done — events arrive via webhook, not poll.
// Failure of /v2/bot/info doesn't break the channel: outbound still
// works, the username just falls back to the userId.
func (l *LINE) Start(ctx context.Context) error {
	if l.channelSecret == "" {
		slog.Warn("line channel has no channel secret; its webhook rejects every event until it is reconnected with one",
			"account", l.accountID)
	}
	if name, basicID, err := l.fetchBotInfo(ctx); err != nil {
		slog.Warn("line bot info fetch failed", "account", l.accountID, "error", err)
	} else {
		l.mu.Lock()
		l.botName = name
		l.basicID = basicID
		l.mu.Unlock()
		slog.Info("line bot connected", "account", l.accountID, "name", name, "basic_id", basicID)
	}
	<-ctx.Done()
	return nil
}

// Send is the simple text path used by tools / tests.
func (l *LINE) Send(chatID, text string) error {
	return l.SendMessage(bus.OutboundMessage{ChatID: chatID, Text: text})
}

// SendMessage delivers text and media to a LINE chat. Up to five
// message objects go out through the cached replyToken (free); the
// rest — and everything when there is no usable token — is pushed in
// batches of five.
func (l *LINE) SendMessage(msg bus.OutboundMessage) error {
	msgs := l.buildMessages(msg)
	if len(msgs) == 0 {
		return nil
	}
	if tok := l.popReplyToken(msg.ChatID); tok != "" {
		n := min(len(msgs), lineMaxMessagesPerCall)
		if err := l.postReply(tok, msgs[:n]); err == nil {
			msgs = msgs[n:]
		} else {
			// The token can be consumed by a parallel reply or expire in
			// flight; push so the user still gets the message.
			slog.Debug("line reply failed, falling back to push",
				"account", l.accountID, "chat", msg.ChatID, "error", err)
		}
	}
	for len(msgs) > 0 {
		n := min(len(msgs), lineMaxMessagesPerCall)
		if err := l.postPush(msg.ChatID, msgs[:n]); err != nil {
			return err
		}
		msgs = msgs[n:]
	}
	return nil
}

// buildMessages turns an outbound into LINE message objects: text
// bubbles (plain text — LINE renders no markdown), then images as image
// messages and other files as download links.
func (l *LINE) buildMessages(msg bus.OutboundMessage) []map[string]any {
	var out []map[string]any
	var bubbles []string
	if msg.AllowSplit {
		bubbles = SplitOutboundText(msg.Text)
	} else if t := strings.TrimSpace(strings.ReplaceAll(msg.Text, SplitMessageMarker, "\n")); t != "" {
		bubbles = []string{t}
	}
	for _, b := range bubbles {
		for _, part := range lineSplitText(FlattenMarkdownTables(b)) {
			out = append(out, map[string]any{"type": "text", "text": part})
		}
	}
	for _, item := range msg.MediaItems {
		m, err := l.mediaMessage(item)
		if err != nil {
			slog.Warn("line media skipped", "account", l.accountID, "filename", item.Filename, "error", err)
			continue
		}
		out = append(out, m)
	}
	return out
}

func (l *LINE) mediaMessage(item bus.MediaItem) (map[string]any, error) {
	if len(item.Bytes) == 0 {
		return nil, errors.New("empty media")
	}
	ct := item.ContentType
	if ct == "" {
		ct = http.DetectContentType(item.Bytes)
	}
	if (ct == "image/jpeg" || ct == "image/png") && len(item.Bytes) <= lineMaxImageBytes {
		original, err := l.storeMedia(item.Bytes, ct)
		if err != nil {
			return nil, err
		}
		preview := original
		if len(item.Bytes) > lineMaxPreviewBytes {
			small, perr := linePreview(item.Bytes)
			if perr != nil {
				return nil, fmt.Errorf("preview: %w", perr)
			}
			if preview, err = l.storeMedia(small, "image/jpeg"); err != nil {
				return nil, err
			}
		}
		return map[string]any{"type": "image", "originalContentUrl": original, "previewImageUrl": preview}, nil
	}
	link, err := l.storeMedia(item.Bytes, ct)
	if err != nil {
		return nil, err
	}
	name := item.Filename
	if name == "" {
		name = "file"
	}
	return map[string]any{"type": "text", "text": "📎 " + name + "\n" + link}, nil
}

// storeMedia writes bytes under mediaDir and returns their public URL.
func (l *LINE) storeMedia(data []byte, contentType string) (string, error) {
	l.mu.Lock()
	base := l.publicBase
	l.mu.Unlock()
	if base == "" {
		return "", errors.New("no public URL known yet (it is learned from the first webhook LINE delivers)")
	}
	if l.mediaDir == "" {
		return "", errors.New("no media directory")
	}
	if err := os.MkdirAll(l.mediaDir, 0o755); err != nil {
		return "", err
	}
	l.pruneMedia()
	var b [16]byte
	_, _ = rand.Read(b[:])
	token := hex.EncodeToString(b[:])
	ext := lineMediaExt(contentType)
	if err := os.WriteFile(filepath.Join(l.mediaDir, token+ext), data, 0o644); err != nil {
		return "", err
	}
	return base + "/api/line/media/" + l.accountID + "/" + token + ext, nil
}

// ServeMedia returns a stored outbound file by its public name
// ("<token><ext>"). The 128-bit random token is the only access check —
// LINE's servers and the chat's members fetch these without credentials.
func (l *LINE) ServeMedia(name string) ([]byte, string, error) {
	ext := filepath.Ext(name)
	token := strings.TrimSuffix(name, ext)
	if !lineMediaTokenRe.MatchString(token) || l.mediaDir == "" {
		return nil, "", os.ErrNotExist
	}
	data, err := os.ReadFile(filepath.Join(l.mediaDir, token+ext))
	if err != nil {
		return nil, "", err
	}
	return data, http.DetectContentType(data), nil
}

// pruneMedia drops stored media older than lineMediaTTL, at most hourly.
func (l *LINE) pruneMedia() {
	l.mu.Lock()
	if time.Since(l.lastPrune) < time.Hour {
		l.mu.Unlock()
		return
	}
	l.lastPrune = time.Now()
	l.mu.Unlock()
	entries, err := os.ReadDir(l.mediaDir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if info, err := e.Info(); err == nil && time.Since(info.ModTime()) > lineMediaTTL {
			_ = os.Remove(filepath.Join(l.mediaDir, e.Name()))
		}
	}
}

// SendTyping shows LINE's loading animation. It only exists for 1:1
// chats (chatId = a user id); the gateway calls this every 5s, so it is
// throttled and each call keeps the animation up for a while — sending
// the reply clears it.
func (l *LINE) SendTyping(chatID string) error {
	if !strings.HasPrefix(chatID, "U") {
		return nil
	}
	l.mu.Lock()
	if time.Since(l.lastLoading[chatID]) < lineLoadingEvery {
		l.mu.Unlock()
		return nil
	}
	l.lastLoading[chatID] = time.Now()
	l.mu.Unlock()
	body, _ := json.Marshal(map[string]any{"chatId": chatID, "loadingSeconds": lineLoadingSeconds})
	return l.postJSON(l.apiBase+"/v2/bot/chat/loading/start", body)
}

// --- Inbound webhook ---

// LINEEventEnvelope is the webhook body shape.
type LINEEventEnvelope struct {
	Destination string      `json:"destination"`
	Events      []LINEEvent `json:"events"`
}

type LINEEvent struct {
	Type           string       `json:"type"` // "message" | "follow" | "join" | "leave" | ...
	Mode           string       `json:"mode,omitempty"`
	Timestamp      int64        `json:"timestamp"`
	ReplyToken     string       `json:"replyToken,omitempty"`
	Source         LINESource   `json:"source"`
	Message        *LINEMessage `json:"message,omitempty"`
	WebhookEventID string       `json:"webhookEventId,omitempty"`
}

type LINESource struct {
	Type    string `json:"type"` // "user" | "group" | "room"
	UserID  string `json:"userId,omitempty"`
	GroupID string `json:"groupId,omitempty"`
	RoomID  string `json:"roomId,omitempty"`
}

type LINEMessage struct {
	Type     string `json:"type"` // "text" | "image" | "video" | "audio" | "file" | "sticker" | ...
	ID       string `json:"id"`
	Text     string `json:"text,omitempty"`
	FileName string `json:"fileName,omitempty"`
	Mention  *struct {
		Mentionees []LINEMentionee `json:"mentionees"`
	} `json:"mention,omitempty"`
	ContentProvider *struct {
		Type               string `json:"type"` // "line" | "external"
		OriginalContentURL string `json:"originalContentUrl,omitempty"`
	} `json:"contentProvider,omitempty"`
}

// LINEMentionee is one @-mention inside a text message. Index/Length
// count UTF-16 code units, like the JavaScript strings LINE builds them
// from.
type LINEMentionee struct {
	Index  int    `json:"index"`
	Length int    `json:"length"`
	Type   string `json:"type"` // "user" | "all"
	UserID string `json:"userId,omitempty"`
	IsSelf bool   `json:"isSelf,omitempty"`
}

// HandleWebhook validates the HMAC signature against `body` (raw bytes
// — Go's json.Decode would re-encode and break the comparison) and
// dispatches each event. publicBase is the scheme://host the request
// arrived on; once the signature checks out it becomes the base for
// outbound media URLs. LINE expects a quick 200 (non-2xx is retried).
func (l *LINE) HandleWebhook(body []byte, signature, publicBase string) (responseBody []byte, status int, err error) {
	if l.channelSecret == "" {
		return nil, http.StatusUnauthorized,
			errors.New("line webhook rejected: no channel secret configured — reconnect the bot with its Channel secret")
	}
	mac := hmac.New(sha256.New, []byte(l.channelSecret))
	mac.Write(body)
	expected := base64.StdEncoding.EncodeToString(mac.Sum(nil))
	if !hmac.Equal([]byte(expected), []byte(signature)) {
		return nil, http.StatusUnauthorized, errors.New("line signature mismatch")
	}
	if publicBase != "" {
		l.mu.Lock()
		l.publicBase = strings.TrimRight(publicBase, "/")
		l.mu.Unlock()
	}
	var env LINEEventEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		return nil, http.StatusBadRequest, fmt.Errorf("parse: %w", err)
	}
	for _, ev := range env.Events {
		if ev.Type != "message" || ev.Message == nil {
			continue // follow, unfollow, join, postback, …
		}
		if ev.Message.Type == "text" {
			l.dispatchEvent(ev)
		} else {
			// Media has to be downloaded first; don't hold LINE's request.
			go l.dispatchEvent(ev)
		}
	}
	return []byte(`{"ok":true}`), http.StatusOK, nil
}

// dispatchEvent translates a LINE message event into a
// bus.InboundMessage. ChatID resolution prefers the most-specific
// identifier (groupId / roomId / userId) so DMs and groups end up in
// distinct session keys.
func (l *LINE) dispatchEvent(ev LINEEvent) {
	chatID, peerKind := lineChatKey(ev.Source)
	if chatID == "" {
		slog.Debug("line event without identifiable source", "account", l.accountID)
		return
	}

	var text string
	var mentions []string
	var media []bus.MediaItem
	switch m := ev.Message; m.Type {
	case "text":
		var addressed bool
		text, addressed = lineStripSelfMentions(m.Text, m)
		if addressed {
			mentions = []string{l.BotUsername()}
		}
	case "image", "video", "audio", "file":
		item, err := l.downloadContent(m)
		if err != nil {
			slog.Warn("line media download failed", "account", l.accountID, "type", m.Type, "error", err)
			return
		}
		media = []bus.MediaItem{item}
		text = "请查看我发送的附件。"
	default:
		slog.Debug("line unsupported message skipped", "account", l.accountID, "type", m.Type)
		return
	}
	text = strings.TrimSpace(text)
	if text == "" && len(media) == 0 {
		return
	}

	// Stash the replyToken so the FIRST outbound for this chat uses the
	// free reply path. Per-chat slot — each inbound rolls it forward.
	if ev.ReplyToken != "" {
		l.mu.Lock()
		l.replyTokens[chatID] = lineReplyToken{
			token:   ev.ReplyToken,
			expires: time.Now().Add(lineReplyTokenTTL),
		}
		l.mu.Unlock()
	}

	slog.Info("line message received",
		"account", l.accountID,
		"from", ev.Source.UserID,
		"chat", chatID,
		"type", ev.Message.Type,
		"mentioned", len(mentions) > 0)

	l.bus.Inbound <- bus.InboundMessage{
		Channel:    "line",
		AccountID:  l.accountID,
		ChatID:     chatID,
		UserID:     ev.Source.UserID,
		MessageID:  ev.Message.ID,
		Text:       text,
		PeerKind:   peerKind,
		Mentions:   mentions,
		MediaItems: media,
	}
}

// lineStripSelfMentions removes the bot's own @-mentions from text and
// reports whether there were any. LINE only marks a mentionee as isSelf
// when it is this bot; @All doesn't address it.
func lineStripSelfMentions(text string, m *LINEMessage) (string, bool) {
	if m.Mention == nil {
		return text, false
	}
	var self []LINEMentionee
	for _, mn := range m.Mention.Mentionees {
		if mn.IsSelf {
			self = append(self, mn)
		}
	}
	if len(self) == 0 {
		return text, false
	}
	units := utf16.Encode([]rune(text))
	sort.Slice(self, func(i, j int) bool { return self[i].Index > self[j].Index })
	for _, mn := range self {
		if mn.Index < 0 || mn.Length <= 0 || mn.Index+mn.Length > len(units) {
			continue
		}
		units = append(units[:mn.Index], units[mn.Index+mn.Length:]...)
	}
	return string(utf16.Decode(units)), true
}

// downloadContent fetches an inbound attachment. LINE-hosted content
// comes from the data API (video/audio may answer 202 while still being
// prepared); "external" images carry their own URL.
func (l *LINE) downloadContent(m *LINEMessage) (bus.MediaItem, error) {
	url, auth := l.dataBase+"/v2/bot/message/"+m.ID+"/content", true
	if p := m.ContentProvider; p != nil && p.Type == "external" && p.OriginalContentURL != "" {
		url, auth = p.OriginalContentURL, false
	}
	var data []byte
	for attempt := 0; ; attempt++ {
		req, err := http.NewRequest(http.MethodGet, url, nil)
		if err != nil {
			return bus.MediaItem{}, err
		}
		if auth {
			req.Header.Set("Authorization", "Bearer "+l.channelToken)
		}
		resp, err := (&http.Client{Timeout: 60 * time.Second}).Do(req)
		if err != nil {
			return bus.MediaItem{}, err
		}
		if resp.StatusCode == http.StatusAccepted && attempt < 5 {
			resp.Body.Close()
			time.Sleep(2 * time.Second)
			continue
		}
		data, err = io.ReadAll(io.LimitReader(resp.Body, lineMaxDownloadBytes+1))
		resp.Body.Close()
		if err != nil {
			return bus.MediaItem{}, err
		}
		if resp.StatusCode != http.StatusOK {
			return bus.MediaItem{}, fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(data))
		}
		break
	}
	if len(data) > lineMaxDownloadBytes {
		return bus.MediaItem{}, errors.New("attachment too large")
	}
	ct := http.DetectContentType(data)
	name := m.FileName
	if name == "" {
		name = m.Type + lineMediaExt(ct)
	}
	return bus.MediaItem{Filename: name, ContentType: ct, Bytes: data}, nil
}

// lineChatKey picks the most-specific chat identifier from a source
// block and returns it alongside fastclaw's peerKind tag. LINE has
// three chat scopes: 1:1 with a user, multi-person room, and group.
// We collapse room/group → "group" since fastclaw doesn't distinguish
// the two further down the pipeline.
func lineChatKey(s LINESource) (chatID, peerKind string) {
	switch s.Type {
	case "group":
		return s.GroupID, "group"
	case "room":
		return s.RoomID, "group"
	case "user":
		return s.UserID, "dm"
	}
	return "", ""
}

func (l *LINE) popReplyToken(chatID string) string {
	l.mu.Lock()
	defer l.mu.Unlock()
	t, ok := l.replyTokens[chatID]
	if !ok {
		return ""
	}
	delete(l.replyTokens, chatID)
	if time.Now().After(t.expires) {
		return ""
	}
	return t.token
}

// --- HTTP plumbing ---

func (l *LINE) postReply(replyToken string, messages []map[string]any) error {
	body, _ := json.Marshal(map[string]any{"replyToken": replyToken, "messages": messages})
	return l.postJSON(l.apiBase+"/v2/bot/message/reply", body)
}

func (l *LINE) postPush(chatID string, messages []map[string]any) error {
	body, _ := json.Marshal(map[string]any{"to": chatID, "messages": messages})
	return l.postJSON(l.apiBase+"/v2/bot/message/push", body)
}

func (l *LINE) postJSON(url string, body []byte) error {
	ctx, cancel := context.WithTimeout(context.Background(), lineSendTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+l.channelToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := l.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("contact line: %w", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusAccepted {
		return fmt.Errorf("line %s HTTP %d: %s", url, resp.StatusCode, string(respBody))
	}
	return nil
}

// fetchBotInfo calls /v2/bot/info to capture the bot's display name +
// basicId. Best-effort.
func (l *LINE) fetchBotInfo(ctx context.Context) (name, basicID string, err error) {
	_, name, basicID, err = lineBotInfo(ctx, l.httpClient, l.apiBase, l.channelToken)
	return name, basicID, err
}

// LINEValidateCredentials is the connect-handler validation step:
// hits /v2/bot/info to confirm the channel access token is good and
// captures the bot's userId + display name. Returns (userId,
// displayName, basicId, error).
func LINEValidateCredentials(ctx context.Context, channelToken string) (userID, displayName, basicID string, err error) {
	userID, displayName, basicID, err = lineBotInfo(ctx, &http.Client{Timeout: lineSendTimeout}, lineAPIBase, channelToken)
	if err == nil && userID == "" {
		err = errors.New("line /bot/info returned empty userId")
	}
	return userID, displayName, basicID, err
}

func lineBotInfo(ctx context.Context, client *http.Client, apiBase, token string) (userID, name, basicID string, err error) {
	ctx, cancel := context.WithTimeout(ctx, lineSendTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiBase+"/v2/bot/info", nil)
	if err != nil {
		return "", "", "", err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := client.Do(req)
	if err != nil {
		return "", "", "", fmt.Errorf("contact line: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return "", "", "", fmt.Errorf("line bot info HTTP %d: %s", resp.StatusCode, string(body))
	}
	var out struct {
		UserID      string `json:"userId"`
		BasicID     string `json:"basicId"`
		DisplayName string `json:"displayName"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return "", "", "", err
	}
	return out.UserID, out.DisplayName, out.BasicID, nil
}

// --- helpers ---

// lineSplitText cuts text into message-sized pieces (LINE caps a text
// message at 5000 characters), preferring line breaks.
func lineSplitText(text string) []string {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil
	}
	runes := []rune(text)
	var out []string
	for len(runes) > lineMaxTextRunes {
		cut := lineMaxTextRunes
		for i := cut; i > cut/2; i-- {
			if runes[i] == '\n' {
				cut = i
				break
			}
		}
		out = append(out, strings.TrimSpace(string(runes[:cut])))
		runes = []rune(strings.TrimSpace(string(runes[cut:])))
	}
	if len(runes) > 0 {
		out = append(out, string(runes))
	}
	return out
}

// linePreview downsizes an image to a JPEG under LINE's 1 MB preview
// cap. Nearest-neighbour is plenty for a chat thumbnail.
func linePreview(data []byte) ([]byte, error) {
	src, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	b := src.Bounds()
	w, h := b.Dx(), b.Dy()
	if w <= 0 || h <= 0 {
		return nil, errors.New("empty image")
	}
	scale := float64(linePreviewMaxSide) / float64(max(w, h))
	if scale > 1 {
		scale = 1
	}
	nw, nh := max(1, int(float64(w)*scale)), max(1, int(float64(h)*scale))
	dst := image.NewRGBA(image.Rect(0, 0, nw, nh))
	for y := 0; y < nh; y++ {
		for x := 0; x < nw; x++ {
			dst.Set(x, y, src.At(b.Min.X+x*w/nw, b.Min.Y+y*h/nh))
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, dst, &jpeg.Options{Quality: 75}); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func lineMediaExt(contentType string) string {
	switch contentType {
	case "image/jpeg":
		return ".jpg"
	case "image/png":
		return ".png"
	case "image/gif":
		return ".gif"
	case "video/mp4":
		return ".mp4"
	case "audio/mp4", "audio/x-m4a":
		return ".m4a"
	case "application/pdf":
		return ".pdf"
	}
	return ".bin"
}
