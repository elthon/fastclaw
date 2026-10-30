package setup

import (
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"sync"
	"time"

	"github.com/fastclaw-ai/fastclaw/internal/channels"
	"github.com/fastclaw-ai/fastclaw/internal/config"
)

// --- WhatsApp (linked device via QR) ---
//
// Same two-step shape as the WeChat / WeCom QR flows:
//
//   POST /api/agents/{id}/channels/whatsapp/login
//     → starts pairing a new companion device; returns {sessionId, qrCode}.
//
//   GET  /api/agents/{id}/channels/whatsapp/login/status?session=<id>
//     → {status: wait|confirmed|expired|error, qrCode, connected, accountId?}.
//       WhatsApp rotates the code every ~20s, so the client re-renders
//       whatever qrCode comes back. On the first confirmed poll the
//       number is persisted, bound and hot-registered.

type whatsappLoginSession struct {
	login     *channels.WhatsAppLogin
	agentID   string
	userID    string // initiating caller — verified on every status poll
	scope     string
	scopeID   string
	expiresAt time.Time
	persisted string // accountId once saved; later polls just report it
}

var whatsappLogins = struct {
	sync.Mutex
	sessions map[string]*whatsappLoginSession
}{sessions: map[string]*whatsappLoginSession{}}

func (s *Server) handleStartAgentWhatsAppLogin(w http.ResponseWriter, r *http.Request) {
	if !s.requireWritable(w, r) {
		return
	}
	id := r.PathValue("id")
	uid, aid, ok := s.resolveChannelBindingScope(w, r, id)
	if !ok {
		return
	}
	login, err := channels.StartWhatsAppLogin(r.Context())
	if err != nil {
		jsonResponse(w, http.StatusBadGateway, map[string]any{"error": err.Error()})
		return
	}
	buf := make([]byte, 16)
	_, _ = rand.Read(buf)
	sessionID := hex.EncodeToString(buf)

	whatsappLogins.Lock()
	now := time.Now()
	for k, v := range whatsappLogins.sessions {
		if now.After(v.expiresAt) {
			if v.persisted == "" {
				v.login.Abort()
			}
			delete(whatsappLogins.sessions, k)
		}
	}
	whatsappLogins.sessions[sessionID] = &whatsappLoginSession{
		login: login, agentID: id, userID: uid, scope: uid, scopeID: aid,
		expiresAt: now.Add(5 * time.Minute),
	}
	whatsappLogins.Unlock()

	code, status, errMsg, _ := login.Status()
	jsonResponse(w, http.StatusOK, map[string]any{"sessionId": sessionID, "qrCode": code, "status": status, "error": errMsg})
}

func (s *Server) handleAgentWhatsAppLoginStatus(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, _, ok := s.resolveChannelBindingScope(w, r, id); !ok {
		return
	}
	uid := s.effectiveUserID(r)
	sessionID := r.URL.Query().Get("session")

	whatsappLogins.Lock()
	defer whatsappLogins.Unlock()
	sess := whatsappLogins.sessions[sessionID]
	if sess == nil || sess.userID != uid || sess.agentID != id {
		jsonResponse(w, http.StatusNotFound, map[string]any{"error": "session not found or expired"})
		return
	}
	if sess.persisted != "" {
		jsonResponse(w, http.StatusOK, map[string]any{"status": "confirmed", "connected": true, "accountId": sess.persisted})
		return
	}
	code, status, errMsg, device := sess.login.Status()
	switch status {
	case "confirmed":
		accountID := device.User
		cc := config.ChannelConfig{
			Enabled:  true,
			Accounts: map[string]config.AccountConfig{accountID: {UserID: device.String()}},
		}
		if err := s.assertChannelCredentialUniqueOpt(r, "whatsapp", accountID, "", sess.scope, sess.scopeID, true); err != nil {
			go sess.login.Abort() // don't leave an unsaved device linked to the phone
			delete(whatsappLogins.sessions, sessionID)
			jsonResponse(w, http.StatusOK, map[string]any{"status": "error", "connected": false, "error": err.Error()})
			return
		}
		if err := s.saveChannelRecord(r.Context(), sess.scope, sess.scopeID, "whatsapp", accountID, true, cc); err != nil {
			go sess.login.Abort()
			delete(whatsappLogins.sessions, sessionID)
			jsonResponse(w, http.StatusOK, map[string]any{"status": "error", "connected": false, "error": err.Error()})
			return
		}
		// Pre-pair with the number itself: whoever scanned owns it, and
		// its own messages never reach the agent (they're IsFromMe), so
		// no outside account can claim owner rights via /pair.
		s.pairChannelToScanner(r.Context(), "whatsapp", accountID, device.ToNonAD().String())
		s.invalidateOwner(sess.scope, sess.scopeID)
		if ch, err := s.dataStore.LookupChannel(r.Context(), "whatsapp", accountID); err == nil && ch != nil {
			s.hotRegisterChannelRecord(*ch)
		}
		sess.persisted = accountID
		jsonResponse(w, http.StatusOK, map[string]any{"status": "confirmed", "connected": true, "accountId": accountID})
	case "expired", "error":
		delete(whatsappLogins.sessions, sessionID)
		jsonResponse(w, http.StatusOK, map[string]any{"status": status, "connected": false, "error": errMsg})
	default:
		jsonResponse(w, http.StatusOK, map[string]any{"status": "wait", "connected": false, "qrCode": code, "error": errMsg})
	}
}
