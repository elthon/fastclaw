package channels

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/md5"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"

	"github.com/fastclaw-ai/fastclaw/internal/bus"
)

// WeCom (企业微信) smart-bot adapter over the bot's WebSocket long
// connection — no public URL needed, same posture as Feishu long-conn.
// The protocol mirrors the official @wecom/aibot-node-sdk: every frame
// is JSON {cmd, headers:{req_id}, body}; acks come back as
// {headers:{req_id}, errcode, errmsg} with no cmd.
//
//   - auth:      aibot_subscribe {bot_id, secret}
//   - heartbeat: ping every 30s; two missed acks = dead connection
//   - inbound:   aibot_msg_callback (messages), aibot_event_callback
//   - reply:     aibot_respond_msg on the *inbound* req_id (stream body)
//   - push:      aibot_send_msg {chatid, msgtype, ...} — works without a
//     pending inbound, used for extra bubbles, media and late replies
//
// WeCom keeps one live connection per bot: a new subscribe kicks the
// old one with disconnected_event. Reconnecting after that would just
// kick the other client back (e.g. a second fastclaw instance using the
// same bot), so a superseded adapter parks until the process restarts.

const (
	wecomDefaultWSURL     = "wss://openws.work.weixin.qq.com"
	wecomHeartbeat        = 30 * time.Second
	wecomRequestTimeout   = 10 * time.Second
	wecomMaxBackoff       = 30 * time.Second
	wecomAuthRetryBackoff = time.Minute
	wecomMaxContentBytes  = 20480 // stream / markdown content cap
	wecomUploadChunkBytes = 512 * 1024
	wecomMaxUploadChunks  = 100
	wecomMaxDownloadBytes = 50 << 20
	wecomReplyTTL         = time.Hour

	// "Thinking" stream: the official plugin's placeholder renders as
	// WeCom's native thinking state. A stream must be finished with
	// visible content (whitespace is ignored) or it spins until WeCom
	// gives up on the message (~6 minutes).
	wecomThinking       = "<think></think>"
	wecomStreamMaxAge   = 5*time.Minute + 30*time.Second // close before WeCom's 6-minute cutoff
	wecomTypingIdle     = 20 * time.Second               // typing ticks every 5s while a turn runs
	wecomStreamWatch    = 5 * time.Second
	wecomCloseNoReply   = "✅" // turn ended without a reply
	wecomCloseLongTurn  = "⏳" // still working; the result is pushed later
	wecomCloseMediaOnly = "📎" // reply was attachments only
)

var (
	errWeComSuperseded  = errors.New("wecom: another connection took over this bot")
	errWeComAuth        = errors.New("wecom: authentication failed")
	wecomLeadingMention = regexp.MustCompile(`^\s*@\S+\s*`)
)

type wecomFrame struct {
	Cmd     string          `json:"cmd,omitempty"`
	Headers wecomHeaders    `json:"headers"`
	Body    json.RawMessage `json:"body,omitempty"`
	ErrCode *int            `json:"errcode,omitempty"`
	ErrMsg  string          `json:"errmsg,omitempty"`
}

type wecomHeaders struct {
	ReqID string `json:"req_id"`
}

type wecomMedia struct {
	URL     string `json:"url"`
	AESKey  string `json:"aeskey"`
	Content string `json:"content"` // voice: speech-to-text
}

type wecomMessage struct {
	MsgID    string `json:"msgid"`
	ChatID   string `json:"chatid"`
	ChatType string `json:"chattype"`
	From     struct {
		UserID string `json:"userid"`
	} `json:"from"`
	MsgType string     `json:"msgtype"`
	Text    wecomMedia `json:"text"`
	Voice   wecomMedia `json:"voice"`
	Image   wecomMedia `json:"image"`
	File    wecomMedia `json:"file"`
	Video   wecomMedia `json:"video"`
	Mixed   struct {
		Items []struct {
			MsgType string     `json:"msgtype"`
			Text    wecomMedia `json:"text"`
			Image   wecomMedia `json:"image"`
		} `json:"msg_item"`
	} `json:"mixed"`
	Quote *struct {
		MsgType string     `json:"msgtype"`
		Text    wecomMedia `json:"text"`
		Voice   wecomMedia `json:"voice"`
	} `json:"quote"`
	Event struct {
		EventType string `json:"eventtype"`
	} `json:"event"`
}

// wecomPendingReply is the newest unanswered inbound of a chat. Its
// req_id carries exactly one reply stream: opened as "thinking" by
// SendTyping, finished by the reply (or by watchStream as a fallback).
type wecomPendingReply struct {
	reqID      string
	at         time.Time
	streamID   string // set once the thinking stream is open
	streamAt   time.Time
	lastTyping time.Time
	noStream   bool // opening the stream failed; don't retry every tick
}

// WeCom implements Channel for a WeCom smart bot (智能机器人, API mode).
type WeCom struct {
	botID     string
	secret    string
	accountID string
	wsURL     string
	bus       *bus.MessageBus
	http      *http.Client

	writeMu sync.Mutex // gorilla allows one concurrent writer
	connMu  sync.Mutex
	conn    *websocket.Conn

	ackMu sync.Mutex
	acks  map[string]chan wecomFrame

	replyMu sync.Mutex
	replies map[string]*wecomPendingReply // chatID → newest unanswered inbound

	missedPongs atomic.Int32

	// Thinking-stream timings (wecomStreamMaxAge etc.); fields so tests
	// can shrink them per adapter.
	streamMaxAge, typingIdle, streamWatch time.Duration
}

// NewWeCom builds the adapter. botID doubles as the accountID.
func NewWeCom(botID, secret string, mb *bus.MessageBus) (*WeCom, error) {
	if botID == "" || secret == "" {
		return nil, errors.New("wecom: bot id and secret are required")
	}
	return &WeCom{
		botID:     botID,
		secret:    secret,
		accountID: botID,
		wsURL:     wecomDefaultWSURL,
		bus:       mb,
		http:      &http.Client{Timeout: 60 * time.Second},
		acks:      map[string]chan wecomFrame{},
		replies:   map[string]*wecomPendingReply{},

		streamMaxAge: wecomStreamMaxAge,
		typingIdle:   wecomTypingIdle,
		streamWatch:  wecomStreamWatch,
	}, nil
}

func (w *WeCom) Name() string      { return "wecom" }
func (w *WeCom) AccountID() string { return w.accountID }

// BotUsername is the bot id: WeCom only delivers group messages that
// @-mention the bot, so inbound group messages carry it in Mentions and
// the gateway's mention routing matches on it.
func (w *WeCom) BotUsername() string { return w.botID }

// SendTyping shows WeCom's native "thinking" state: the first call of a
// turn opens a stream on the pending inbound with the thinking
// placeholder; the reply later finishes that same stream. The gateway
// calls this every 5s while a turn runs, so later calls only refresh
// lastTyping, which watchStream uses to detect a turn that ended
// without a reply.
func (w *WeCom) SendTyping(chatID string) error {
	w.replyMu.Lock()
	p := w.replies[chatID]
	if p == nil {
		w.replyMu.Unlock()
		return nil
	}
	now := time.Now()
	p.lastTyping = now
	if p.streamID != "" || p.noStream || now.Sub(p.at) > w.streamMaxAge {
		w.replyMu.Unlock()
		return nil
	}
	p.streamID, p.streamAt = wecomReqID("stream"), now
	reqID, streamID := p.reqID, p.streamID
	w.replyMu.Unlock()

	if err := w.respondStream(reqID, streamID, wecomThinking, false); err != nil {
		w.replyMu.Lock()
		p.streamID, p.noStream = "", true
		w.replyMu.Unlock()
		return err
	}
	go w.watchStream(chatID, p)
	return nil
}

// watchStream closes a thinking stream nobody finished: the turn ended
// without a reply (typing stopped), or it's about to outlive WeCom's
// 6-minute window — then the real reply is pushed when it arrives.
func (w *WeCom) watchStream(chatID string, p *wecomPendingReply) {
	t := time.NewTicker(w.streamWatch)
	defer t.Stop()
	for range t.C {
		w.replyMu.Lock()
		if w.replies[chatID] != p {
			w.replyMu.Unlock()
			return // answered, or superseded (rememberReply closed it)
		}
		var mark string
		switch now := time.Now(); {
		case now.Sub(p.streamAt) > w.streamMaxAge:
			mark = wecomCloseLongTurn
		case now.Sub(p.lastTyping) > w.typingIdle:
			mark = wecomCloseNoReply
		}
		if mark == "" {
			w.replyMu.Unlock()
			continue
		}
		delete(w.replies, chatID)
		w.replyMu.Unlock()
		if err := w.respondStream(p.reqID, p.streamID, mark, true); err != nil {
			slog.Debug("wecom: closing thinking stream failed", "account", w.accountID, "error", err)
		}
		return
	}
}

// Start keeps the long connection up until ctx is cancelled.
func (w *WeCom) Start(ctx context.Context) error {
	backoff := time.Second
	for {
		authed, err := w.runOnce(ctx)
		if ctx.Err() != nil {
			return nil
		}
		switch {
		case errors.Is(err, errWeComSuperseded):
			slog.Warn("wecom connection taken over by another client; staying idle until restart", "account", w.accountID)
			<-ctx.Done()
			return nil
		case errors.Is(err, errWeComAuth):
			backoff = wecomAuthRetryBackoff
		case authed:
			backoff = time.Second
		}
		slog.Warn("wecom connection lost, reconnecting", "account", w.accountID, "in", backoff, "error", err)
		if !sleepOrDone(ctx, backoff) {
			return nil
		}
		if backoff < wecomMaxBackoff {
			backoff = min(backoff*2, wecomMaxBackoff)
		}
	}
}

// runOnce dials, authenticates and serves one connection. authed
// reports whether the subscribe succeeded, so Start can reset backoff.
func (w *WeCom) runOnce(ctx context.Context) (authed bool, err error) {
	conn, err := wecomDial(ctx, w.wsURL, w.botID, w.secret)
	if err != nil {
		return false, err
	}
	slog.Info("wecom bot connected", "account", w.accountID)
	w.connMu.Lock()
	w.conn = conn
	w.connMu.Unlock()
	w.missedPongs.Store(0)

	connCtx, cancel := context.WithCancel(ctx)
	defer func() {
		cancel()
		w.connMu.Lock()
		w.conn = nil
		w.connMu.Unlock()
		_ = conn.Close()
	}()
	go func() {
		<-connCtx.Done()
		_ = conn.Close() // unblocks ReadMessage
	}()
	go w.heartbeat(connCtx, conn)

	for {
		_, data, err := conn.ReadMessage()
		if err != nil {
			return true, err
		}
		var f wecomFrame
		if err := json.Unmarshal(data, &f); err != nil {
			slog.Debug("wecom: unparseable frame", "error", err)
			continue
		}
		switch f.Cmd {
		case "aibot_msg_callback":
			go w.handleMessage(f)
		case "aibot_event_callback":
			var m wecomMessage
			_ = json.Unmarshal(f.Body, &m)
			if m.Event.EventType == "disconnected_event" {
				return true, errWeComSuperseded
			}
		case "":
			if strings.HasPrefix(f.Headers.ReqID, "ping") {
				w.missedPongs.Store(0)
				continue
			}
			w.ackMu.Lock()
			ch := w.acks[f.Headers.ReqID]
			delete(w.acks, f.Headers.ReqID)
			w.ackMu.Unlock()
			if ch != nil {
				ch <- f
			}
		}
	}
}

func (w *WeCom) heartbeat(ctx context.Context, conn *websocket.Conn) {
	t := time.NewTicker(wecomHeartbeat)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if w.missedPongs.Load() >= 2 {
				slog.Warn("wecom heartbeat lost, dropping connection", "account", w.accountID)
				_ = conn.Close()
				return
			}
			w.missedPongs.Add(1)
			w.writeMu.Lock()
			err := conn.WriteJSON(wecomFrame{Cmd: "ping", Headers: wecomHeaders{ReqID: wecomReqID("ping")}})
			w.writeMu.Unlock()
			if err != nil {
				_ = conn.Close()
				return
			}
		}
	}
}

// wecomDial opens a connection and completes the subscribe handshake.
func wecomDial(ctx context.Context, url, botID, secret string) (*websocket.Conn, error) {
	dialCtx, cancel := context.WithTimeout(ctx, wecomRequestTimeout)
	defer cancel()
	conn, _, err := websocket.DefaultDialer.DialContext(dialCtx, url, nil)
	if err != nil {
		return nil, fmt.Errorf("wecom: dial: %w", err)
	}
	reqID := wecomReqID("aibot_subscribe")
	body, _ := json.Marshal(map[string]string{"bot_id": botID, "secret": secret})
	if err := conn.WriteJSON(wecomFrame{Cmd: "aibot_subscribe", Headers: wecomHeaders{ReqID: reqID}, Body: body}); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("wecom: send auth: %w", err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(wecomRequestTimeout))
	for {
		var f wecomFrame
		if err := conn.ReadJSON(&f); err != nil {
			_ = conn.Close()
			return nil, fmt.Errorf("wecom: waiting for auth: %w", err)
		}
		if f.Headers.ReqID != reqID {
			continue
		}
		if f.ErrCode != nil && *f.ErrCode != 0 {
			_ = conn.Close()
			return nil, fmt.Errorf("%w: %s (code %d)", errWeComAuth, f.ErrMsg, *f.ErrCode)
		}
		_ = conn.SetReadDeadline(time.Time{})
		return conn, nil
	}
}

// WeComValidateCredentials checks a bot id + secret by completing the
// subscribe handshake once. Only call it for a bot that isn't connected
// yet — a successful subscribe kicks any live connection for the bot.
func WeComValidateCredentials(ctx context.Context, botID, secret string) error {
	conn, err := wecomDial(ctx, wecomDefaultWSURL, botID, secret)
	if err != nil {
		if errors.Is(err, errWeComAuth) {
			return fmt.Errorf("invalid Bot ID or Secret: %w", err)
		}
		return err
	}
	return conn.Close()
}

// request sends one frame and waits for its ack.
func (w *WeCom) request(cmd, reqID string, body any) (wecomFrame, error) {
	w.connMu.Lock()
	conn := w.conn
	w.connMu.Unlock()
	if conn == nil {
		return wecomFrame{}, errors.New("wecom: not connected")
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return wecomFrame{}, err
	}
	ch := make(chan wecomFrame, 1)
	w.ackMu.Lock()
	w.acks[reqID] = ch
	w.ackMu.Unlock()
	defer func() {
		w.ackMu.Lock()
		delete(w.acks, reqID)
		w.ackMu.Unlock()
	}()

	w.writeMu.Lock()
	err = conn.WriteJSON(wecomFrame{Cmd: cmd, Headers: wecomHeaders{ReqID: reqID}, Body: raw})
	w.writeMu.Unlock()
	if err != nil {
		return wecomFrame{}, fmt.Errorf("wecom: write %s: %w", cmd, err)
	}
	select {
	case f := <-ch:
		if f.ErrCode != nil && *f.ErrCode != 0 {
			return f, fmt.Errorf("wecom: %s: %s (code %d)", cmd, f.ErrMsg, *f.ErrCode)
		}
		return f, nil
	case <-time.After(wecomRequestTimeout):
		return wecomFrame{}, fmt.Errorf("wecom: %s: ack timeout", cmd)
	}
}

func (w *WeCom) handleMessage(f wecomFrame) {
	var m wecomMessage
	if err := json.Unmarshal(f.Body, &m); err != nil {
		slog.Warn("wecom: bad message body", "error", err)
		return
	}
	peerKind, chatID := "dm", m.From.UserID
	if m.ChatType == "group" {
		peerKind, chatID = "group", m.ChatID
	}
	if chatID == "" {
		return
	}

	var text string
	var media []bus.MediaItem
	addMedia := func(kind string, src wecomMedia) {
		item, err := w.download(src)
		if err != nil {
			slog.Warn("wecom media download failed", "account", w.accountID, "type", kind, "error", err)
			return
		}
		if item.Filename == "" {
			item.Filename = kind + wecomExt(item.ContentType)
		}
		media = append(media, item)
	}
	switch m.MsgType {
	case "text":
		text = m.Text.Content
	case "voice":
		text = m.Voice.Content
	case "image":
		addMedia("image", m.Image)
	case "file":
		addMedia("file", m.File)
	case "video":
		addMedia("video", m.Video)
	case "mixed":
		var parts []string
		for _, it := range m.Mixed.Items {
			switch it.MsgType {
			case "text":
				parts = append(parts, it.Text.Content)
			case "image":
				addMedia("image", it.Image)
			}
		}
		text = strings.Join(parts, "\n")
	default:
		slog.Debug("wecom unsupported message skipped", "account", w.accountID, "type", m.MsgType)
		return
	}
	if peerKind == "group" {
		// Group text arrives as "@BotName question"; the mention is
		// implied by delivery, so drop it from what the agent reads.
		text = wecomLeadingMention.ReplaceAllString(text, "")
	}
	text = strings.TrimSpace(text)
	if text == "" && len(media) > 0 {
		text = "请查看我发送的附件。"
	}
	if text == "" {
		return
	}
	if q := m.Quote; q != nil {
		quoted := q.Text.Content
		if quoted == "" {
			quoted = q.Voice.Content
		}
		if quoted = strings.TrimSpace(quoted); quoted != "" {
			text = "> " + strings.ReplaceAll(quoted, "\n", "\n> ") + "\n\n" + text
		}
	}

	w.rememberReply(chatID, f.Headers.ReqID)

	var mentions []string
	if peerKind == "group" {
		mentions = []string{w.botID}
	}
	slog.Info("wecom message received",
		"account", w.accountID, "from", m.From.UserID, "chat", chatID,
		"peer_kind", peerKind, "type", m.MsgType)
	w.bus.Inbound <- bus.InboundMessage{
		Channel:    "wecom",
		AccountID:  w.accountID,
		ChatID:     chatID,
		UserID:     m.From.UserID,
		MessageID:  m.MsgID,
		Text:       text,
		PeerKind:   peerKind,
		SenderName: m.From.UserID,
		Mentions:   mentions,
		MediaItems: media,
	}
}

func (w *WeCom) rememberReply(chatID, reqID string) {
	if reqID == "" {
		return
	}
	now := time.Now()
	w.replyMu.Lock()
	for k, p := range w.replies {
		if now.Sub(p.at) > wecomReplyTTL {
			delete(w.replies, k)
		}
	}
	prev := w.replies[chatID]
	w.replies[chatID] = &wecomPendingReply{reqID: reqID, at: now}
	w.replyMu.Unlock()
	// A newer message takes over the reply slot; an open thinking stream
	// on the older one would otherwise spin until WeCom times it out.
	if prev != nil && prev.streamID != "" {
		go func() {
			if err := w.respondStream(prev.reqID, prev.streamID, wecomCloseNoReply, true); err != nil {
				slog.Debug("wecom: closing superseded stream failed", "account", w.accountID, "error", err)
			}
		}()
	}
}

// takeReply claims the newest unanswered inbound of chatID. Each req_id
// carries exactly one reply; later bubbles go out as pushes.
func (w *WeCom) takeReply(chatID string) *wecomPendingReply {
	w.replyMu.Lock()
	defer w.replyMu.Unlock()
	p := w.replies[chatID]
	delete(w.replies, chatID)
	if p == nil || time.Since(p.at) > wecomReplyTTL {
		return nil
	}
	return p
}

// finishReply sends content as the final frame of p's reply stream —
// the open thinking stream if there is one, else a fresh one.
func (w *WeCom) finishReply(p *wecomPendingReply, content string) error {
	w.replyMu.Lock()
	streamID := p.streamID
	w.replyMu.Unlock()
	if streamID == "" {
		streamID = wecomReqID("stream")
	}
	return w.respondStream(p.reqID, streamID, content, true)
}

// Send pushes a markdown message to a chat (user id for DMs, chatid for groups).
func (w *WeCom) Send(chatID, text string) error {
	for _, part := range wecomSplitContent(text) {
		body := map[string]any{"chatid": chatID, "msgtype": "markdown", "markdown": map[string]string{"content": part}}
		if _, err := w.request("aibot_send_msg", wecomReqID("aibot_send_msg"), body); err != nil {
			return err
		}
	}
	return nil
}

// SendMessage answers the chat's pending inbound when there is one (a
// finished stream reply, threaded to the user's message), and pushes
// everything else: extra bubbles, overflow, media.
func (w *WeCom) SendMessage(msg bus.OutboundMessage) error {
	text := msg.Text
	var bubbles []string
	if msg.AllowSplit {
		bubbles = SplitOutboundText(text)
	} else if t := strings.TrimSpace(strings.ReplaceAll(text, SplitMessageMarker, "\n")); t != "" {
		bubbles = []string{t}
	}

	var firstErr error
	if len(bubbles) == 0 {
		// Attachments only: still close the thinking stream with a
		// visible mark so it doesn't spin; the files are pushed below.
		if p := w.takeReply(msg.ChatID); p != nil && len(msg.MediaItems) > 0 {
			if err := w.finishReply(p, wecomCloseMediaOnly); err != nil {
				slog.Debug("wecom: closing thinking stream failed", "account", w.accountID, "error", err)
			}
		}
	}
	for i, bubble := range bubbles {
		parts := wecomSplitContent(FlattenMarkdownTables(bubble))
		for j, part := range parts {
			if i == 0 && j == 0 {
				if p := w.takeReply(msg.ChatID); p != nil {
					err := w.finishReply(p, part)
					if err == nil {
						continue
					}
					slog.Warn("wecom reply failed, pushing instead", "account", w.accountID, "error", err)
				}
			}
			if err := w.Send(msg.ChatID, part); err != nil {
				slog.Warn("wecom send failed", "account", w.accountID, "chat", msg.ChatID, "error", err)
				if firstErr == nil {
					firstErr = err
				}
			}
		}
	}
	for _, item := range msg.MediaItems {
		if err := w.sendMedia(msg.ChatID, item); err != nil {
			slog.Warn("wecom media send failed", "account", w.accountID, "filename", item.Filename, "error", err)
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
}

// respondStream sends one frame of a stream reply on an inbound req_id.
// Frames of one stream share streamID; finish=true closes it.
func (w *WeCom) respondStream(reqID, streamID, content string, finish bool) error {
	body := map[string]any{
		"msgtype": "stream",
		"stream":  map[string]any{"id": streamID, "finish": finish, "content": content},
	}
	_, err := w.request("aibot_respond_msg", reqID, body)
	return err
}

func (w *WeCom) sendMedia(chatID string, item bus.MediaItem) error {
	kind := "file"
	ct := item.ContentType
	if ct == "" {
		ct = http.DetectContentType(item.Bytes)
	}
	if ct == "image/png" || ct == "image/jpeg" {
		kind = "image"
	}
	mediaID, err := w.uploadMedia(kind, item.Filename, item.Bytes)
	if err != nil {
		return err
	}
	body := map[string]any{"chatid": chatID, "msgtype": kind, kind: map[string]string{"media_id": mediaID}}
	_, err = w.request("aibot_send_msg", wecomReqID("aibot_send_msg"), body)
	return err
}

// uploadMedia runs the three-step temp-media upload (init → chunks →
// finish) and returns the media_id (valid for 3 days).
func (w *WeCom) uploadMedia(kind, filename string, data []byte) (string, error) {
	if len(data) == 0 {
		return "", errors.New("wecom: empty media")
	}
	if filename == "" {
		filename = kind
	}
	chunks := (len(data) + wecomUploadChunkBytes - 1) / wecomUploadChunkBytes
	if chunks > wecomMaxUploadChunks {
		return "", fmt.Errorf("wecom: %s too large (%d bytes)", filename, len(data))
	}
	sum := md5.Sum(data)
	init, err := w.request("aibot_upload_media_init", wecomReqID("aibot_upload_media_init"), map[string]any{
		"type": kind, "filename": filename, "total_size": len(data), "total_chunks": chunks, "md5": hex.EncodeToString(sum[:]),
	})
	if err != nil {
		return "", err
	}
	var initBody struct {
		UploadID string `json:"upload_id"`
	}
	if err := json.Unmarshal(init.Body, &initBody); err != nil || initBody.UploadID == "" {
		return "", errors.New("wecom: upload init returned no upload_id")
	}
	for i := 0; i < chunks; i++ {
		chunk := data[i*wecomUploadChunkBytes : min((i+1)*wecomUploadChunkBytes, len(data))]
		if _, err := w.request("aibot_upload_media_chunk", wecomReqID("aibot_upload_media_chunk"), map[string]any{
			"upload_id": initBody.UploadID, "chunk_index": i, "base64_data": base64.StdEncoding.EncodeToString(chunk),
		}); err != nil {
			return "", err
		}
	}
	fin, err := w.request("aibot_upload_media_finish", wecomReqID("aibot_upload_media_finish"), map[string]any{"upload_id": initBody.UploadID})
	if err != nil {
		return "", err
	}
	var finBody struct {
		MediaID string `json:"media_id"`
	}
	if err := json.Unmarshal(fin.Body, &finBody); err != nil || finBody.MediaID == "" {
		return "", errors.New("wecom: upload finish returned no media_id")
	}
	return finBody.MediaID, nil
}

// download fetches an inbound attachment (URL valid ~5 minutes) and
// decrypts it with the per-file aeskey.
func (w *WeCom) download(src wecomMedia) (bus.MediaItem, error) {
	if src.URL == "" {
		return bus.MediaItem{}, errors.New("no url")
	}
	resp, err := w.http.Get(src.URL)
	if err != nil {
		return bus.MediaItem{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return bus.MediaItem{}, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, wecomMaxDownloadBytes+1))
	if err != nil {
		return bus.MediaItem{}, err
	}
	if len(data) > wecomMaxDownloadBytes {
		return bus.MediaItem{}, errors.New("attachment too large")
	}
	if src.AESKey != "" {
		if data, err = wecomDecrypt(data, src.AESKey); err != nil {
			return bus.MediaItem{}, err
		}
	}
	var filename string
	if _, params, err := mime.ParseMediaType(resp.Header.Get("Content-Disposition")); err == nil {
		filename = params["filename"]
	}
	return bus.MediaItem{Filename: filename, ContentType: http.DetectContentType(data), Bytes: data}, nil
}

// wecomDecrypt is AES-256-CBC with the IV = first 16 key bytes and
// PKCS#7 padding to a 32-byte block (not AES's 16).
func wecomDecrypt(data []byte, aesKey string) ([]byte, error) {
	key, err := base64.StdEncoding.DecodeString(aesKey)
	if err != nil {
		key, err = base64.RawStdEncoding.DecodeString(strings.TrimRight(aesKey, "="))
	}
	if err != nil || len(key) != 32 {
		return nil, errors.New("wecom: invalid aeskey")
	}
	if len(data) == 0 || len(data)%aes.BlockSize != 0 {
		return nil, errors.New("wecom: ciphertext is not block-aligned")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	out := make([]byte, len(data))
	cipher.NewCBCDecrypter(block, key[:16]).CryptBlocks(out, data)
	pad := int(out[len(out)-1])
	if pad < 1 || pad > 32 || pad > len(out) || !bytes.Equal(out[len(out)-pad:], bytes.Repeat([]byte{byte(pad)}, pad)) {
		return nil, errors.New("wecom: bad padding")
	}
	return out[:len(out)-pad], nil
}

// wecomSplitContent cuts text into pieces under the 20480-byte content
// cap, preferring line breaks and never splitting a UTF-8 sequence.
func wecomSplitContent(text string) []string {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil
	}
	var out []string
	for len(text) > wecomMaxContentBytes {
		cut := wecomMaxContentBytes
		for cut > 0 && (text[cut]&0xC0) == 0x80 {
			cut--
		}
		if nl := strings.LastIndexByte(text[:cut], '\n'); nl > cut/2 {
			cut = nl
		}
		out = append(out, strings.TrimSpace(text[:cut]))
		text = strings.TrimSpace(text[cut:])
	}
	if text != "" {
		out = append(out, text)
	}
	return out
}

func wecomExt(contentType string) string {
	if exts, _ := mime.ExtensionsByType(contentType); len(exts) > 0 {
		return exts[0]
	}
	return ""
}

func wecomReqID(prefix string) string {
	var b [6]byte
	_, _ = rand.Read(b[:])
	return fmt.Sprintf("%s_%d_%s", prefix, time.Now().UnixMilli(), hex.EncodeToString(b[:]))
}
