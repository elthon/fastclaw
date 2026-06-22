package channels

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"github.com/fastclaw-ai/fastclaw/internal/bus"
)

const (
	dingtalkDefaultAPIBase       = "https://api.dingtalk.com"
	dingtalkOpenConnectionPath   = "/v1.0/gateway/connections/open"
	dingtalkSessionWebhookLimit  = 20000
	dingtalkSendTimeout          = 15 * time.Second
	dingtalkConnectTimeout       = 15 * time.Second
	dingtalkBackoffInitial       = 2 * time.Second
	dingtalkBackoffMax           = 60 * time.Second
	dingtalkSessionWebhookMaxAge = 90 * time.Minute
	dingtalkThinkingText         = "正在思考..."
)

// DingTalk is the stream-mode adapter for DingTalk internal robots.
type DingTalk struct {
	bus            *bus.MessageBus
	accountID      string
	clientID       string
	clientSecret   string
	robotCode      string
	requireMention bool
	apiBase        string
	httpClient     *http.Client

	mu       sync.Mutex
	webhooks map[string]dingtalkSessionWebhook
	dedup    map[string]time.Time
}

type dingtalkSessionWebhook struct {
	URL      string
	ExpireAt time.Time
}

func NewDingTalk(clientID, clientSecret, robotCode string, requireMention bool, accountID string, mb *bus.MessageBus) (*DingTalk, error) {
	clientID = strings.TrimSpace(clientID)
	clientSecret = strings.TrimSpace(clientSecret)
	robotCode = strings.TrimSpace(robotCode)
	accountID = strings.TrimSpace(accountID)
	if clientID == "" || clientSecret == "" {
		return nil, errors.New("dingtalk: clientId and clientSecret required")
	}
	if accountID == "" {
		if robotCode != "" {
			accountID = robotCode
		} else {
			accountID = clientID
		}
	}
	return &DingTalk{
		bus:            mb,
		accountID:      accountID,
		clientID:       clientID,
		clientSecret:   clientSecret,
		robotCode:      robotCode,
		requireMention: requireMention,
		apiBase:        dingtalkDefaultAPIBase,
		httpClient:     &http.Client{Timeout: dingtalkSendTimeout},
		webhooks:       make(map[string]dingtalkSessionWebhook),
		dedup:          make(map[string]time.Time),
	}, nil
}

func (d *DingTalk) Name() string        { return "dingtalk" }
func (d *DingTalk) AccountID() string   { return d.accountID }
func (d *DingTalk) BotUsername() string { return d.accountID }

func (d *DingTalk) Start(ctx context.Context) error {
	backoff := dingtalkBackoffInitial
	for ctx.Err() == nil {
		if err := d.runOnce(ctx); err != nil && ctx.Err() == nil {
			slog.Warn("dingtalk stream disconnected", "account", d.accountID, "error", err, "retryIn", backoff)
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(backoff):
			}
			backoff *= 2
			if backoff > dingtalkBackoffMax {
				backoff = dingtalkBackoffMax
			}
			continue
		}
		backoff = dingtalkBackoffInitial
	}
	return nil
}

func (d *DingTalk) runOnce(ctx context.Context) error {
	endpoint, err := d.openStreamEndpoint(ctx)
	if err != nil {
		return err
	}
	conn, _, err := websocket.DefaultDialer.DialContext(ctx, endpoint, nil)
	if err != nil {
		return fmt.Errorf("dingtalk websocket dial: %w", err)
	}
	defer conn.Close()
	slog.Info("dingtalk stream connected", "account", d.accountID)

	done := make(chan struct{})
	go func() {
		defer close(done)
		<-ctx.Done()
		_ = conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""), time.Now().Add(time.Second))
		_ = conn.Close()
	}()

	for {
		msgType, payload, err := conn.ReadMessage()
		if err != nil {
			select {
			case <-done:
				return nil
			default:
				return err
			}
		}
		if msgType != websocket.TextMessage && msgType != websocket.BinaryMessage {
			continue
		}
		if handled, err := d.handleSystemFrame(conn, payload); handled {
			if err != nil {
				slog.Warn("dingtalk system frame handling failed", "account", d.accountID, "error", err)
			}
			continue
		}
		ackID, inbound, err := d.parseStreamPayload(payload)
		if ackID != "" {
			if err := d.writeAck(conn, ackID); err != nil {
				slog.Warn("dingtalk ack failed", "account", d.accountID, "ackId", ackID, "error", err)
			}
		}
		if err != nil {
			slog.Debug("dingtalk payload skipped", "account", d.accountID, "error", err, "payload", string(payload))
			continue
		}
		if inbound.MessageID != "" && d.seen(inbound.MessageID) {
			continue
		}
		d.sendThinking(inbound.ChatID)
		d.bus.Inbound <- inbound
	}
}

func (d *DingTalk) openStreamEndpoint(ctx context.Context) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, dingtalkConnectTimeout)
	defer cancel()
	payload := map[string]any{
		"clientId":     d.clientID,
		"clientSecret": d.clientSecret,
		"subscriptions": []map[string]string{
			{"type": "CALLBACK", "topic": "/v1.0/im/bot/messages/get"},
		},
	}
	body, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(d.apiBase, "/")+dingtalkOpenConnectionPath, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := d.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("dingtalk open stream: %w", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("dingtalk open stream HTTP %d: %s", resp.StatusCode, string(respBody))
	}
	endpoint := dingtalkFindString(respBody, "endpoint")
	if endpoint == "" {
		endpoint = dingtalkFindString(respBody, "url")
	}
	ticket := dingtalkFindString(respBody, "ticket")
	if endpoint == "" || ticket == "" {
		return "", fmt.Errorf("dingtalk open stream returned no endpoint/ticket: %s", string(respBody))
	}
	sep := "?"
	if strings.Contains(endpoint, "?") {
		sep = "&"
	}
	return endpoint + sep + "ticket=" + ticket, nil
}

func dingtalkFindString(body []byte, key string) string {
	var v any
	if err := json.Unmarshal(body, &v); err != nil {
		return ""
	}
	return findStringInJSON(v, key)
}

func findStringInJSON(v any, key string) string {
	switch x := v.(type) {
	case map[string]any:
		if s, ok := x[key].(string); ok {
			return s
		}
		for _, child := range x {
			if s := findStringInJSON(child, key); s != "" {
				return s
			}
		}
	case []any:
		for _, child := range x {
			if s := findStringInJSON(child, key); s != "" {
				return s
			}
		}
	}
	return ""
}

func (d *DingTalk) handleSystemFrame(conn *websocket.Conn, payload []byte) (bool, error) {
	var frame struct {
		Type    string `json:"type"`
		Headers struct {
			Topic     string `json:"topic"`
			MessageID string `json:"messageId"`
		} `json:"headers"`
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(payload, &frame); err != nil || frame.Type != "SYSTEM" {
		return false, nil
	}
	slog.Info("dingtalk system frame", "account", d.accountID, "topic", frame.Headers.Topic)
	if frame.Headers.Topic == "ping" && frame.Headers.MessageID != "" {
		ack := map[string]any{
			"code":    200,
			"message": "ok",
			"headers": map[string]string{
				"contentType": "application/json",
				"messageId":   frame.Headers.MessageID,
			},
			"data": string(frame.Data),
		}
		_ = conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
		return true, conn.WriteJSON(ack)
	}
	return true, nil
}

func (d *DingTalk) writeAck(conn *websocket.Conn, ackID string) error {
	ack := map[string]any{
		"code":    200,
		"message": "",
		"headers": map[string]string{
			"contentType": "application/json",
			"messageId":   ackID,
		},
		"data": "",
	}
	_ = conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	return conn.WriteJSON(ack)
}

func (d *DingTalk) sendThinking(chatID string) {
	webhook := d.sessionWebhook(chatID)
	if webhook == "" {
		return
	}
	go func() {
		if err := d.sendSessionWebhookText(webhook, dingtalkThinkingText); err != nil {
			slog.Debug("dingtalk thinking send failed", "account", d.accountID, "chat", chatID, "error", err)
		}
	}()
}

func (d *DingTalk) Send(chatID, text string) error {
	return d.SendMessage(bus.OutboundMessage{ChatID: chatID, Text: text})
}

func (d *DingTalk) SendMessage(msg bus.OutboundMessage) error {
	text := strings.TrimSpace(msg.Text)
	if text == "" && len(msg.MediaItems) == 0 {
		return nil
	}
	if text == "" {
		slog.Debug("dingtalk send: media-only message dropped", "account", d.accountID, "chat", msg.ChatID)
		return nil
	}
	webhook := d.sessionWebhook(msg.ChatID)
	if webhook == "" {
		return fmt.Errorf("dingtalk: no sessionWebhook cached for chat %s", msg.ChatID)
	}
	for _, chunk := range dingtalkSplitText(text, dingtalkSessionWebhookLimit) {
		if err := d.sendSessionWebhookMarkdown(webhook, dingtalkMarkdownTitle(chunk), chunk); err != nil {
			slog.Warn("dingtalk markdown send failed, falling back to text", "account", d.accountID, "chat", msg.ChatID, "error", err)
			if err := d.sendSessionWebhookText(webhook, FlattenMarkdownTables(chunk)); err != nil {
				return err
			}
		}
	}
	return nil
}

func (d *DingTalk) sendSessionWebhookMarkdown(webhook, title, text string) error {
	if strings.TrimSpace(title) == "" {
		title = "FastClaw"
	}
	payload := map[string]any{
		"msgtype": "markdown",
		"markdown": map[string]string{
			"title": title,
			"text":  text,
		},
	}
	return d.postSessionWebhook(webhook, payload)
}

func (d *DingTalk) sendSessionWebhookText(webhook, text string) error {
	payload := map[string]any{
		"msgtype": "text",
		"text": map[string]string{
			"content": text,
		},
	}
	return d.postSessionWebhook(webhook, payload)
}

func (d *DingTalk) postSessionWebhook(webhook string, payload map[string]any) error {
	body, _ := json.Marshal(payload)
	req, err := http.NewRequest(http.MethodPost, webhook, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := d.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("dingtalk send: %w", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("dingtalk send HTTP %d: %s", resp.StatusCode, string(respBody))
	}
	var out struct {
		ErrCode *int   `json:"errcode"`
		ErrMsg  string `json:"errmsg"`
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	if len(respBody) > 0 && json.Unmarshal(respBody, &out) == nil {
		if out.ErrCode != nil && *out.ErrCode != 0 {
			return fmt.Errorf("dingtalk send: errcode=%d errmsg=%s", *out.ErrCode, out.ErrMsg)
		}
		if out.Code != "" && out.Code != "0" {
			return fmt.Errorf("dingtalk send: code=%s message=%s", out.Code, out.Message)
		}
	}
	return nil
}

func dingtalkMarkdownTitle(text string) string {
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		line = strings.TrimLeft(line, "#>*-+ `\t")
		line = strings.TrimSpace(strings.ReplaceAll(line, "**", ""))
		if line == "" {
			continue
		}
		r := []rune(line)
		if len(r) > 48 {
			return string(r[:48])
		}
		return line
	}
	return "FastClaw"
}

func (d *DingTalk) SendTyping(_ string) error { return nil }

func (d *DingTalk) cacheWebhook(chatID, webhook string) {
	if chatID == "" || webhook == "" {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.webhooks[chatID] = dingtalkSessionWebhook{URL: webhook, ExpireAt: time.Now().Add(dingtalkSessionWebhookMaxAge)}
}

func (d *DingTalk) sessionWebhook(chatID string) string {
	d.mu.Lock()
	defer d.mu.Unlock()
	entry := d.webhooks[chatID]
	if entry.URL == "" || time.Now().After(entry.ExpireAt) {
		delete(d.webhooks, chatID)
		return ""
	}
	return entry.URL
}

func (d *DingTalk) seen(messageID string) bool {
	now := time.Now()
	d.mu.Lock()
	defer d.mu.Unlock()
	for id, ts := range d.dedup {
		if now.Sub(ts) > 10*time.Minute {
			delete(d.dedup, id)
		}
	}
	if _, ok := d.dedup[messageID]; ok {
		return true
	}
	d.dedup[messageID] = now
	return false
}

type dingtalkRobotMessage struct {
	ConversationID   string `json:"conversationId"`
	ConversationType string `json:"conversationType"`
	MsgID            string `json:"msgId"`
	SenderStaffID    string `json:"senderStaffId"`
	SenderID         string `json:"senderId"`
	SenderNick       string `json:"senderNick"`
	MsgType          string `json:"msgtype"`
	Text             *struct {
		Content string `json:"content"`
	} `json:"text"`
	Markdown *struct {
		Text  string `json:"text"`
		Title string `json:"title"`
	} `json:"markdown"`
	RichText []struct {
		Text string `json:"text"`
	} `json:"richText"`
	SessionWebhook string           `json:"sessionWebhook"`
	IsInAtList     bool             `json:"isInAtList"`
	AtUsers        []dingtalkAtUser `json:"atUsers"`
	Raw            json.RawMessage  `json:"-"`
}

type dingtalkAtUser struct {
	DingTalkID string `json:"dingtalkId"`
	StaffID    string `json:"staffId"`
	UserID     string `json:"userId"`
	Nick       string `json:"nick"`
}

func (d *DingTalk) parseStreamPayload(payload []byte) (string, bus.InboundMessage, error) {
	ackID := dingtalkFindString(payload, "messageId")
	if ackID == "" {
		ackID = dingtalkFindString(payload, "msgId")
	}
	body := unwrapDingTalkPayload(payload)
	var evt dingtalkRobotMessage
	var frame struct {
		Type    string `json:"type"`
		Headers struct {
			Topic     string `json:"topic"`
			MessageID string `json:"messageId"`
		} `json:"headers"`
	}
	_ = json.Unmarshal(payload, &frame)
	if frame.Type != "" {
		slog.Info("dingtalk stream frame", "account", d.accountID, "type", frame.Type, "topic", frame.Headers.Topic, "messageId", frame.Headers.MessageID)
	}
	if err := json.Unmarshal(body, &evt); err != nil {
		return ackID, bus.InboundMessage{}, fmt.Errorf("parse robot message: %w", err)
	}
	evt.Raw = append([]byte(nil), body...)
	if evt.ConversationID == "" || evt.MsgID == "" {
		return ackID, bus.InboundMessage{}, errors.New("not a DingTalk robot message")
	}
	text := strings.TrimSpace(evt.messageText())
	if text == "" {
		return ackID, bus.InboundMessage{}, fmt.Errorf("unsupported or empty DingTalk msgtype %q", evt.MsgType)
	}
	peerKind := "dm"
	if evt.ConversationType == "2" {
		peerKind = "group"
	}
	mentions := evt.mentions()
	if evt.IsInAtList {
		mentions = appendDingTalkMention(mentions, d.accountID)
		mentions = appendDingTalkMention(mentions, d.robotCode)
		mentions = appendDingTalkMention(mentions, d.clientID)
	}
	if peerKind == "group" && d.requireMention && !evt.mentionsBot(d.robotCode, d.clientID) {
		return ackID, bus.InboundMessage{}, errors.New("group message without bot mention")
	}
	d.cacheWebhook(evt.ConversationID, evt.SessionWebhook)
	userID := evt.SenderStaffID
	if userID == "" {
		userID = evt.SenderID
	}
	return ackID, bus.InboundMessage{
		Channel:    "dingtalk",
		AccountID:  d.accountID,
		ChatID:     evt.ConversationID,
		UserID:     userID,
		MessageID:  evt.MsgID,
		Text:       text,
		PeerKind:   peerKind,
		SenderName: evt.SenderNick,
		Mentions:   mentions,
	}, nil
}

func unwrapDingTalkPayload(payload []byte) []byte {
	var root map[string]json.RawMessage
	if err := json.Unmarshal(payload, &root); err != nil {
		return payload
	}
	for _, key := range []string{"data", "event", "payload"} {
		raw := root[key]
		if len(raw) == 0 {
			continue
		}
		var s string
		if json.Unmarshal(raw, &s) == nil && strings.TrimSpace(s) != "" {
			return []byte(s)
		}
		return raw
	}
	return payload
}

func (m dingtalkRobotMessage) messageText() string {
	switch strings.ToLower(m.MsgType) {
	case "text", "":
		if m.Text != nil {
			return m.Text.Content
		}
	case "markdown":
		if m.Markdown != nil {
			return m.Markdown.Text
		}
	case "richtext", "rich_text":
		parts := make([]string, 0, len(m.RichText))
		for _, p := range m.RichText {
			if strings.TrimSpace(p.Text) != "" {
				parts = append(parts, strings.TrimSpace(p.Text))
			}
		}
		return strings.Join(parts, "\n")
	}
	return ""
}

func (m dingtalkRobotMessage) mentions() []string {
	out := make([]string, 0, len(m.AtUsers))
	seen := map[string]bool{}
	for _, u := range m.AtUsers {
		for _, id := range []string{u.StaffID, u.UserID, u.DingTalkID, u.Nick} {
			id = strings.TrimSpace(id)
			if id != "" && !seen[id] {
				seen[id] = true
				out = append(out, id)
			}
		}
	}
	return out
}

func appendDingTalkMention(mentions []string, value string) []string {
	value = strings.TrimSpace(value)
	if value == "" {
		return mentions
	}
	for _, m := range mentions {
		if m == value {
			return mentions
		}
	}
	return append(mentions, value)
}

func (m dingtalkRobotMessage) mentionsBot(robotCode, clientID string) bool {
	if m.IsInAtList {
		return true
	}
	needles := map[string]bool{}
	for _, id := range []string{robotCode, clientID} {
		id = strings.TrimSpace(id)
		if id != "" {
			needles[id] = true
		}
	}
	if len(needles) == 0 {
		return false
	}
	for _, id := range m.mentions() {
		if needles[id] {
			return true
		}
	}
	return false
}

func dingtalkSplitText(s string, max int) []string {
	if max <= 0 || len(s) <= max {
		return []string{s}
	}
	var out []string
	for len(s) > max {
		cut := strings.LastIndex(s[:max], "\n")
		if cut < max/2 {
			cut = max
		}
		out = append(out, strings.TrimSpace(s[:cut]))
		s = strings.TrimSpace(s[cut:])
	}
	if s != "" {
		out = append(out, s)
	}
	return out
}
