package setup

import (
	"context"
	"crypto/rand"
	"log/slog"
	"net/http"
	"time"

	"github.com/fastclaw-ai/fastclaw/internal/buildinfo"
	"github.com/fastclaw-ai/fastclaw/internal/store"
)

// --- Channel pairing (console side) ---
//
// The gateway half lives in gateway/pairing.go. Here the binder fetches a
// one-time code to send as `/pair <code>` from their IM account, or
// unpairs to hand the bot to a different account.
//
//   POST   /api/agents/{id}/channels/{type}/{accountId}/pair-code
//     → {code, command, expiresAt}. 409 when already paired.
//   DELETE /api/agents/{id}/channels/{type}/{accountId}/pairing
//     → clears the paired account; the channel goes back to answering
//       "not paired" until it is paired again.

const pairCodeTTL = 10 * time.Minute

// pairCodeAlphabet drops 0/O/1/I/L so a code read off the screen and
// typed on a phone can't be mistyped into another valid character.
const pairCodeAlphabet = "ABCDEFGHJKMNPQRSTUVWXYZ23456789"

func newPairCode() (string, error) {
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	for i, b := range buf {
		buf[i] = pairCodeAlphabet[int(b)%len(pairCodeAlphabet)]
	}
	return string(buf), nil
}

// callerChannel resolves the (type, accountId) channel row the caller
// bound on this agent. Writes 4xx and returns nil on failure.
func (s *Server) callerChannel(w http.ResponseWriter, r *http.Request) *store.ChannelRecord {
	uid, aid, ok := s.resolveChannelBindingScope(w, r, r.PathValue("id"))
	if !ok {
		return nil
	}
	chs, err := s.dataStore.ListChannels(r.Context(), uid, aid)
	if err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return nil
	}
	channelType, accountID := r.PathValue("type"), r.PathValue("accountId")
	for i := range chs {
		if chs[i].Type == channelType && chs[i].AccountID == accountID {
			return &chs[i]
		}
	}
	jsonResponse(w, http.StatusNotFound, map[string]any{"error": "channel not found"})
	return nil
}

func (s *Server) handleCreateChannelPairCode(w http.ResponseWriter, r *http.Request) {
	if !s.requireWritable(w, r) {
		return
	}
	ch := s.callerChannel(w, r)
	if ch == nil {
		return
	}
	if ch.BoundUserID != "" {
		jsonResponse(w, http.StatusConflict, map[string]any{"error": "channel is already paired; unpair it first"})
		return
	}
	code, err := newPairCode()
	if err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	expiresAt := time.Now().Add(pairCodeTTL).UTC()
	if err := s.dataStore.SetChannelPairCode(r.Context(), ch.ID, code, expiresAt); err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	jsonResponse(w, http.StatusOK, map[string]any{
		"code":      code,
		"command":   "/pair " + code,
		"expiresAt": expiresAt.Format(time.RFC3339),
	})
}

func (s *Server) handleDeleteChannelPairing(w http.ResponseWriter, r *http.Request) {
	if !s.requireWritable(w, r) {
		return
	}
	ch := s.callerChannel(w, r)
	if ch == nil {
		return
	}
	if err := s.dataStore.SetChannelBinding(r.Context(), ch.ID, "", ""); err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	jsonResponse(w, http.StatusOK, map[string]any{"ok": true})
}

// pairChannelToScanner pre-pairs a freshly connected channel with the
// platform account that scanned its connect QR (Feishu open_id, iLink
// user id). The scan already proves who the owner is on that platform,
// so they skip the /pair step. Best-effort: on failure the channel just
// stays unpaired and the console offers a code instead.
func (s *Server) pairChannelToScanner(ctx context.Context, channelType, accountID, platformUserID string) {
	if platformUserID == "" || s.dataStore == nil {
		return
	}
	ch, err := s.dataStore.LookupChannel(ctx, channelType, accountID)
	if err != nil || ch == nil {
		return
	}
	if err := s.dataStore.SetChannelBinding(ctx, ch.ID, platformUserID, ""); err != nil {
		slog.Warn("pair channel to scanner", "channel", channelType, "account", accountID, "error", err)
	}
}

// applyChannelPairing fills the pairing fields of a channel card.
// exposeCode is false for rows the caller didn't bind themselves — the
// outstanding code is a credential for claiming the bot.
func applyChannelPairing(out *channelOut, rec store.ChannelRecord, agentOwnerID string, exposeCode bool) {
	out.Paired = rec.BoundUserID != ""
	if out.Paired {
		out.PairedName = rec.BoundUserName
		// Mirrors Agent.isAdminChatter: the paired sender gets host access
		// only when the channel's binder is the agent's owner, and never
		// on a hosted deploy (everything runs in the sandbox there).
		out.HostAccess = rec.UserID != "" && rec.UserID == agentOwnerID && !buildinfo.IsHostedDeploy()
		return
	}
	if exposeCode && rec.PairCode != "" && time.Now().Before(rec.PairCodeExpiresAt) {
		out.PairCommand = "/pair " + rec.PairCode
		out.PairCodeExpiresAt = rec.PairCodeExpiresAt.Format(time.RFC3339)
	}
}
