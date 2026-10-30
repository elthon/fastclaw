package setup

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/fastclaw-ai/fastclaw/internal/channels"
)

// --- Feishu one-click bot registration (QR scan) ---
//
// Feishu's accounts service exposes an OAuth device-code style flow
// that creates a "PersonalAgent" custom app on the scanner's tenant and
// hands back its app_id/app_secret — the same flow the official
// @larksuite/openclaw-lark installer uses. Two-step, mirroring WeChat:
//
//   POST /api/agents/{id}/channels/feishu/register
//     → action=begin; returns {sessionId, qrUrl, interval, expiresIn}.
//       The client renders qrUrl as a QR code for the Feishu app.
//
//   GET  /api/agents/{id}/channels/feishu/register/status?session=<id>
//     → one action=poll round-trip. Returns {status: pending|confirmed|
//       denied|expired, connected, appId?, botName?}. On confirmed the
//       app is persisted in long-connection mode + bound + hot-registered.

const (
	feishuAccountsBase     = "https://accounts.feishu.cn"
	feishuRegistrationPath = "/oauth/v1/app/registration"
)

type feishuRegisterSession struct {
	deviceCode string
	agentID    string
	userID     string // initiating caller — verified on every status poll
	scope      string // (userID, agentID) storage scope captured at start,
	scopeID    string // same convention as wechatLoginSession
	expiresAt  time.Time
}

type feishuRegisterRegistry struct {
	mu       sync.Mutex
	sessions map[string]*feishuRegisterSession
}

var feishuRegistrations = &feishuRegisterRegistry{sessions: map[string]*feishuRegisterSession{}}

func (r *feishuRegisterRegistry) put(id string, s *feishuRegisterSession) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sessions[id] = s
	now := time.Now()
	for k, v := range r.sessions {
		if now.After(v.expiresAt) {
			delete(r.sessions, k)
		}
	}
}

func (r *feishuRegisterRegistry) get(id string) *feishuRegisterSession {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.sessions[id]
}

func (r *feishuRegisterRegistry) delete(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.sessions, id)
}

func (s *Server) handleStartAgentFeishuRegister(w http.ResponseWriter, r *http.Request) {
	if !s.requireWritable(w, r) {
		return
	}
	id := r.PathValue("id")
	uid, aid, ok := s.resolveChannelBindingScope(w, r, id)
	if !ok {
		return
	}

	begin, err := feishuRegistrationBegin(r.Context())
	if err != nil {
		jsonResponse(w, http.StatusBadGateway, map[string]any{"error": err.Error()})
		return
	}
	qrURL, err := url.Parse(begin.VerificationURIComplete)
	if err != nil {
		jsonResponse(w, http.StatusBadGateway, map[string]any{"error": "feishu returned an invalid verification url"})
		return
	}
	q := qrURL.Query()
	q.Set("from", "onboard")
	qrURL.RawQuery = q.Encode()

	interval := begin.Interval
	if interval <= 0 {
		interval = 5
	}
	expiresIn := begin.ExpiresIn
	if expiresIn <= 0 {
		expiresIn = 600
	}

	buf := make([]byte, 16)
	_, _ = rand.Read(buf)
	sessionID := hex.EncodeToString(buf)
	feishuRegistrations.put(sessionID, &feishuRegisterSession{
		deviceCode: begin.DeviceCode,
		agentID:    id,
		userID:     uid,
		scope:      uid,
		scopeID:    aid,
		expiresAt:  time.Now().Add(time.Duration(expiresIn) * time.Second),
	})
	jsonResponse(w, http.StatusOK, map[string]any{
		"sessionId": sessionID,
		"qrUrl":     qrURL.String(),
		"interval":  interval,
		"expiresIn": expiresIn,
	})
}

func (s *Server) handleAgentFeishuRegisterStatus(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, _, ok := s.resolveChannelBindingScope(w, r, id); !ok {
		return
	}
	uid := s.effectiveUserID(r)
	sessionID := r.URL.Query().Get("session")
	if sessionID == "" {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"error": "session required"})
		return
	}
	sess := feishuRegistrations.get(sessionID)
	if sess == nil || sess.userID != uid || sess.agentID != id {
		jsonResponse(w, http.StatusNotFound, map[string]any{"error": "session not found or expired"})
		return
	}
	if time.Now().After(sess.expiresAt) {
		feishuRegistrations.delete(sessionID)
		jsonResponse(w, http.StatusOK, map[string]any{"status": "expired", "connected": false})
		return
	}

	poll, err := feishuRegistrationPoll(r.Context(), sess.deviceCode)
	if err != nil {
		jsonResponse(w, http.StatusBadGateway, map[string]any{"error": err.Error()})
		return
	}

	if poll.ClientID != "" && poll.ClientSecret != "" {
		// fastclaw's Feishu adapter only talks to open.feishu.cn; an app
		// created on a Lark (international) tenant can't authenticate there.
		if poll.UserInfo.TenantBrand == "lark" {
			feishuRegistrations.delete(sessionID)
			jsonResponse(w, http.StatusOK, map[string]any{
				"status":    "error",
				"connected": false,
				"error":     "Lark (international) tenants are not supported yet; please use a Feishu account",
			})
			return
		}
		appID, appSecret := poll.ClientID, poll.ClientSecret
		// The app was just minted by Feishu, so the credentials are known
		// good; bot info is only for display and may lag a moment behind
		// app creation — don't fail the connect over it.
		botName, _, verr := channels.FeishuValidateCredentials(r.Context(), appID, appSecret)
		if verr != nil {
			slog.Warn("feishu register: fetch bot info", "appId", appID, "err", verr)
		}
		if status, err := s.persistFeishuAccount(r, sess.scope, sess.scopeID, id, appID, appSecret, "", "", true); err != nil {
			jsonResponse(w, status, map[string]any{"error": err.Error()})
			return
		}
		// Pre-pair with the scanner. Assumes the registration's open_id is
		// scoped to the app it just created (open_ids are per-app), i.e.
		// the sender id their DMs to this bot carry. If it ever isn't, the
		// owner simply sees no host access and can unpair + /pair.
		s.pairChannelToScanner(r.Context(), "feishu", appID, poll.UserInfo.OpenID)
		feishuRegistrations.delete(sessionID)
		jsonResponse(w, http.StatusOK, map[string]any{
			"status":    "confirmed",
			"connected": true,
			"appId":     appID,
			"botName":   botName,
		})
		return
	}

	switch poll.Error {
	case "", "authorization_pending", "slow_down":
		jsonResponse(w, http.StatusOK, map[string]any{"status": "pending", "connected": false})
	case "access_denied":
		feishuRegistrations.delete(sessionID)
		jsonResponse(w, http.StatusOK, map[string]any{"status": "denied", "connected": false})
	case "expired_token", "invalid_grant":
		feishuRegistrations.delete(sessionID)
		jsonResponse(w, http.StatusOK, map[string]any{"status": "expired", "connected": false})
	default:
		msg := poll.Error
		if poll.ErrorDescription != "" {
			msg += ": " + poll.ErrorDescription
		}
		jsonResponse(w, http.StatusBadGateway, map[string]any{"error": msg})
	}
}

// --- accounts.feishu.cn registration helpers ---

type feishuRegistrationBeginResp struct {
	DeviceCode              string `json:"device_code"`
	UserCode                string `json:"user_code"`
	VerificationURIComplete string `json:"verification_uri_complete"`
	ExpiresIn               int    `json:"expires_in"`
	Interval                int    `json:"interval"`
}

type feishuRegistrationPollResp struct {
	ClientID         string `json:"client_id"`
	ClientSecret     string `json:"client_secret"`
	Error            string `json:"error"`
	ErrorDescription string `json:"error_description"`
	UserInfo         struct {
		OpenID      string `json:"open_id"`
		TenantBrand string `json:"tenant_brand"`
	} `json:"user_info"`
}

func feishuRegistrationCall(ctx context.Context, form url.Values, out any) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, feishuAccountsBase+feishuRegistrationPath, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("contact feishu: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	// Pending/denied/expired polls come back as HTTP 400 with an OAuth
	// error body, so decode regardless of status and let callers inspect.
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("feishu registration HTTP %d: %s", resp.StatusCode, string(body))
	}
	return nil
}

func feishuRegistrationBegin(ctx context.Context) (*feishuRegistrationBeginResp, error) {
	var out feishuRegistrationBeginResp
	if err := feishuRegistrationCall(ctx, url.Values{
		"action":            {"begin"},
		"archetype":         {"PersonalAgent"},
		"auth_method":       {"client_secret"},
		"request_user_info": {"open_id"},
	}, &out); err != nil {
		return nil, err
	}
	if out.DeviceCode == "" || out.VerificationURIComplete == "" {
		return nil, fmt.Errorf("feishu registration returned no device code")
	}
	return &out, nil
}

func feishuRegistrationPoll(ctx context.Context, deviceCode string) (*feishuRegistrationPollResp, error) {
	var out feishuRegistrationPollResp
	if err := feishuRegistrationCall(ctx, url.Values{
		"action":      {"poll"},
		"device_code": {deviceCode},
	}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}
