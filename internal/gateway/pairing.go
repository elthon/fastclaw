package gateway

import (
	"context"
	"crypto/subtle"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/fastclaw-ai/fastclaw/internal/bus"
	"github.com/fastclaw-ai/fastclaw/internal/store"
)

// Channel pairing.
//
// Connecting a bot (token or QR) proves the binder controls the bot
// account, not which platform user is them — anyone who finds the bot
// can DM it. Pairing closes that gap: the console shows the binder a
// one-time code, and sending `/pair <code>` to the bot from their IM
// account records that sender as the channel's owner
// (channels.bound_user_id). Both halves are needed — only the logged-in
// binder can see the code, and only the real platform account can send
// it from there.
//
// Until a channel is paired the gateway answers every inbound with a
// "not paired" notice and routes nothing to the agent. Once paired,
// everyone may chat; only the paired sender is flagged FromChannelOwner,
// which is what host access keys on (Agent.isAdminChatter).

// maxPairAttempts burns the outstanding code after this many wrong
// guesses so it can't be brute-forced within its lifetime.
const maxPairAttempts = 5

const (
	pairMsgNotPaired = "This bot hasn't been paired with its owner yet, so it can't take messages. " +
		"Owner: open this channel in the FastClaw console and send the /pair command it shows to this bot in a direct message."
	pairMsgOK          = "Paired. This account is now the owner of this bot."
	pairMsgBadCode     = "That pairing code is invalid or has expired. Get a fresh one from the channel card in the FastClaw console."
	pairMsgBurned      = "Too many wrong pairing codes — the current code is now void. Generate a new one in the FastClaw console."
	pairMsgInGroup     = "For safety, send /pair to the bot in a direct message, not in a group."
	pairMsgAlreadyMine = "This bot is already paired with your account."
	pairMsgAlreadySet  = "This bot is already paired with its owner."
)

// gatePairing applies the pairing rules to an inbound IM message. It
// must run before msg.UserID is rewritten to a fastclaw chatter id —
// pairing compares the raw platform sender id. handled=true means the
// message was answered (or dropped) here and must not be routed.
func (g *Gateway) gatePairing(ctx context.Context, msg bus.InboundMessage, ch *store.ChannelRecord) (fromOwner, handled bool) {
	isPair, code := parsePairCommand(msg.Text)
	dm := msg.PeerKind != "group"

	if ch.BoundUserID != "" {
		fromOwner = msg.UserID != "" && msg.UserID == ch.BoundUserID
		if isPair && !msg.IsBotMessage {
			if fromOwner {
				g.replyInbound(ctx, msg, pairMsgAlreadyMine)
			} else {
				g.replyInbound(ctx, msg, pairMsgAlreadySet)
			}
			return fromOwner, true
		}
		return fromOwner, false
	}

	// Unpaired. Never talk back to other bots — two unpaired bots in one
	// group would otherwise ping-pong notices forever.
	if msg.IsBotMessage {
		return false, true
	}
	switch {
	case isPair && !dm:
		g.replyInbound(ctx, msg, pairMsgInGroup)
	case isPair:
		g.replyInbound(ctx, msg, g.tryPair(ctx, msg, ch, code))
	case dm || len(msg.Mentions) > 0:
		// In groups only answer when addressed; platforms that deliver
		// every group message (Discord guild channels) would otherwise
		// get a notice under each line.
		g.replyInbound(ctx, msg, pairMsgNotPaired)
	}
	return false, true
}

// tryPair checks code against the channel's outstanding pair code and,
// on a match, binds the sender. Returns the reply to send.
func (g *Gateway) tryPair(ctx context.Context, msg bus.InboundMessage, ch *store.ChannelRecord, code string) string {
	want := ch.PairCode
	if want == "" || time.Now().After(ch.PairCodeExpiresAt) || msg.UserID == "" {
		return pairMsgBadCode
	}
	// Count per outstanding code: a freshly generated code starts clean.
	failKey := ch.ID + ":" + want
	if subtle.ConstantTimeCompare([]byte(normalizePairCode(code)), []byte(want)) != 1 {
		n := 1
		if v, ok := g.pairFails.Load(failKey); ok {
			n = v.(int) + 1
		}
		if n < maxPairAttempts {
			g.pairFails.Store(failKey, n)
			return pairMsgBadCode
		}
		g.pairFails.Delete(failKey)
		if err := g.store.SetChannelPairCode(ctx, ch.ID, "", time.Time{}); err != nil {
			slog.Warn("pairing: burn code failed", "channel", ch.Type, "account", ch.AccountID, "error", err)
		}
		return pairMsgBurned
	}
	g.pairFails.Delete(failKey)
	if err := g.store.SetChannelBinding(ctx, ch.ID, msg.UserID, msg.SenderName); err != nil {
		slog.Warn("pairing: bind failed", "channel", ch.Type, "account", ch.AccountID, "error", err)
		return "Pairing failed on the server — please try again."
	}
	slog.Info("channel paired", "channel", ch.Type, "account", ch.AccountID, "owner", ch.UserID)
	return pairMsgOK
}

// parsePairCommand finds a `/pair <code>` command in text. Tolerates a
// leading @mention (group messages carry one) and Telegram's
// `/pair@botname` form.
func parsePairCommand(text string) (isPair bool, code string) {
	fields := strings.Fields(text)
	for i, f := range fields {
		cmd := strings.ToLower(f)
		if at := strings.IndexByte(cmd, '@'); at > 0 {
			cmd = cmd[:at]
		}
		if cmd != "/pair" {
			continue
		}
		if i+1 < len(fields) {
			code = fields[i+1]
		}
		return true, code
	}
	return false, ""
}

// normalizePairCode upper-cases and strips separators so "abcd-2345"
// and "ABCD2345" match the stored form.
func normalizePairCode(code string) string {
	code = strings.ToUpper(strings.TrimSpace(code))
	return strings.NewReplacer("-", "", " ", "").Replace(code)
}

// replyInbound sends a gateway-authored notice back to the chat msg came
// from. Bounded so a wedged outbound loop can't stall inbound routing.
func (g *Gateway) replyInbound(ctx context.Context, msg bus.InboundMessage, text string) {
	if g.bus == nil {
		return
	}
	out := bus.OutboundMessage{
		Channel:      msg.Channel,
		AccountID:    msg.AccountID,
		ChatID:       msg.ChatID,
		Text:         text,
		ReplyToMsgID: msg.MessageID,
	}
	select {
	case g.bus.Outbound <- out:
	case <-ctx.Done():
	case <-time.After(5 * time.Second):
		slog.Warn("pairing notice dropped: outbound busy",
			"channel", msg.Channel, "chat_id", msg.ChatID, "text", fmt.Sprintf("%.40s", text))
	}
}
