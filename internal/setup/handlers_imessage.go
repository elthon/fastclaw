package setup

import (
	"errors"
	"net/http"
	"os"
	"strings"

	"github.com/fastclaw-ai/fastclaw/internal/auth"
	"github.com/fastclaw-ai/fastclaw/internal/channels"
	"github.com/fastclaw-ai/fastclaw/internal/config"
)

// --- iMessage (local macOS) ---
//
// Only offered when fastclaw runs on macOS and the caller is a platform
// admin: the channel speaks as the Apple ID signed into this Mac's
// Messages app, so it's a machine-level resource, not a per-user one.

func imessageAllowed(r *http.Request) bool {
	ident, _ := auth.FromContext(r.Context())
	return channels.IMessageSupported() && ident.CanAdminPlatform()
}

// handleAgentIMessageStatus tells the UI whether to show the iMessage
// card at all and, if so, whether Full Disk Access is already granted.
func (s *Server) handleAgentIMessageStatus(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, _, ok := s.resolveChannelBindingScope(w, r, id); !ok {
		return
	}
	if !imessageAllowed(r) {
		jsonResponse(w, http.StatusOK, map[string]any{"available": false})
		return
	}
	resp := map[string]any{
		"available":      true,
		"fullDiskAccess": channels.IMessageCheckDiskAccess(r.Context()) == nil,
	}
	if exe, err := os.Executable(); err == nil {
		resp["binaryPath"] = exe
	}
	jsonResponse(w, http.StatusOK, resp)
}

// handleConnectAgentIMessage checks both macOS permissions (triggering
// the Automation prompt if it hasn't been answered yet), then stores
// the single "local" account and starts tailing chat.db.
func (s *Server) handleConnectAgentIMessage(w http.ResponseWriter, r *http.Request) {
	if !s.requireWritable(w, r) {
		return
	}
	id := r.PathValue("id")
	uid, aid, ok := s.resolveChannelBindingScope(w, r, id)
	if !ok {
		return
	}
	if !imessageAllowed(r) {
		jsonResponse(w, http.StatusForbidden, map[string]any{"error": "iMessage is only available to admins of a self-hosted fastclaw running on macOS"})
		return
	}
	if err := channels.IMessageCheckDiskAccess(r.Context()); err != nil {
		code := "chat_db_unreadable"
		if errors.Is(err, channels.ErrIMessageNoDiskAccess) {
			code = "no_disk_access"
		}
		jsonResponse(w, http.StatusBadRequest, map[string]any{"error": err.Error(), "code": code})
		return
	}
	if err := channels.IMessageCheckAutomation(r.Context()); err != nil {
		code := "messages_unavailable"
		// -1743: "Not authorized to send Apple events to Messages."
		if strings.Contains(err.Error(), "-1743") {
			code = "no_automation"
		}
		jsonResponse(w, http.StatusBadRequest, map[string]any{"error": err.Error(), "code": code})
		return
	}

	accountID := channels.IMessageLocalAccountID
	cc := config.ChannelConfig{
		Enabled:  true,
		Accounts: map[string]config.AccountConfig{accountID: {}},
	}
	if err := s.assertChannelCredentialUniqueOpt(r, "imessage", accountID, "", uid, aid, true); err != nil {
		jsonResponse(w, http.StatusConflict, map[string]any{"error": err.Error()})
		return
	}
	if err := s.saveChannelRecord(r.Context(), uid, aid, "imessage", accountID, true, cc); err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	if err := s.appendBinding(r, "", "", config.Binding{
		AgentID: id,
		Match:   config.Match{Channel: "imessage", AccountID: accountID},
	}); err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	// Pre-pair: the bot *is* this Mac's Apple ID, whose own messages are
	// never delivered (is_from_me), so the owner can't send /pair — and
	// letting an outside sender pair would hand them owner rights. The
	// sentinel never matches a sender: everyone may chat, nobody is owner.
	s.pairChannelToScanner(r.Context(), "imessage", accountID, "local-apple-id")
	s.invalidateOwner(uid, aid)
	if ch, err := s.dataStore.LookupChannel(r.Context(), "imessage", accountID); err == nil && ch != nil {
		s.hotRegisterChannelRecord(*ch)
	}
	jsonResponse(w, http.StatusOK, map[string]any{"ok": true, "accountId": accountID})
}
