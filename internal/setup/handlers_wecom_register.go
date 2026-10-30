package setup

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"runtime"
	"sync"
	"time"
)

// --- WeCom one-click bot creation (QR scan) ---
//
// WeCom exposes a QR flow that creates a smart bot (API mode, long
// connection) for the scanner and hands back its bot id + secret — the
// same flow the official @wecom/wecom-openclaw-cli installer uses. It is
// not in WeCom's public docs, so the manual Bot ID/Secret path stays.
// Two-step, mirroring the Feishu flow:
//
//   POST /api/agents/{id}/channels/wecom/register
//     → returns {sessionId, qrUrl, interval, expiresIn}; the client
//       renders qrUrl as a QR code for the WeCom app.
//
//   GET  /api/agents/{id}/channels/wecom/register/status?session=<id>
//     → one query_result round-trip. Returns {status: pending|confirmed|
//       expired, connected, botId?}. On confirmed the bot is persisted,
//       bound and hot-registered.

// wecomQRBase is a var so tests can point it at a fake server.
var wecomQRBase = "https://work.weixin.qq.com/ai/qc"

const (
	// wecomQRSource is the source tag WeCom's own CLI sends; the flow
	// has no documented value for third parties.
	wecomQRSource   = "wecom-cli"
	wecomQRInterval = 3   // seconds, same cadence as the official CLI
	wecomQRExpires  = 300 // seconds; the CLI gives up after 5 minutes
)

type wecomRegisterSession struct {
	scode     string
	agentID   string
	userID    string // initiating caller — verified on every status poll
	scope     string // (userID, agentID) storage scope captured at start,
	scopeID   string // same convention as feishuRegisterSession
	expiresAt time.Time
}

type wecomRegisterRegistry struct {
	mu       sync.Mutex
	sessions map[string]*wecomRegisterSession
}

var wecomRegistrations = &wecomRegisterRegistry{sessions: map[string]*wecomRegisterSession{}}

func (r *wecomRegisterRegistry) put(id string, s *wecomRegisterSession) {
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

func (r *wecomRegisterRegistry) get(id string) *wecomRegisterSession {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.sessions[id]
}

func (r *wecomRegisterRegistry) delete(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.sessions, id)
}

func (s *Server) handleStartAgentWeComRegister(w http.ResponseWriter, r *http.Request) {
	if !s.requireWritable(w, r) {
		return
	}
	id := r.PathValue("id")
	uid, aid, ok := s.resolveChannelBindingScope(w, r, id)
	if !ok {
		return
	}

	gen, err := wecomQRGenerate(r.Context())
	if err != nil {
		jsonResponse(w, http.StatusBadGateway, map[string]any{"error": err.Error()})
		return
	}

	buf := make([]byte, 16)
	_, _ = rand.Read(buf)
	sessionID := hex.EncodeToString(buf)
	wecomRegistrations.put(sessionID, &wecomRegisterSession{
		scode:     gen.Scode,
		agentID:   id,
		userID:    uid,
		scope:     uid,
		scopeID:   aid,
		expiresAt: time.Now().Add(wecomQRExpires * time.Second),
	})
	jsonResponse(w, http.StatusOK, map[string]any{
		"sessionId": sessionID,
		"qrUrl":     gen.AuthURL,
		"interval":  wecomQRInterval,
		"expiresIn": wecomQRExpires,
	})
}

func (s *Server) handleAgentWeComRegisterStatus(w http.ResponseWriter, r *http.Request) {
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
	sess := wecomRegistrations.get(sessionID)
	if sess == nil || sess.userID != uid || sess.agentID != id {
		jsonResponse(w, http.StatusNotFound, map[string]any{"error": "session not found or expired"})
		return
	}
	if time.Now().After(sess.expiresAt) {
		wecomRegistrations.delete(sessionID)
		jsonResponse(w, http.StatusOK, map[string]any{"status": "expired", "connected": false})
		return
	}

	res, err := wecomQRQuery(r.Context(), sess.scode)
	if err != nil {
		jsonResponse(w, http.StatusBadGateway, map[string]any{"error": err.Error()})
		return
	}
	if res.Status != "success" {
		// "init" until scanned; the CLI treats every non-success state
		// as "keep waiting" and relies on its own timeout, as do we.
		jsonResponse(w, http.StatusOK, map[string]any{"status": "pending", "connected": false})
		return
	}
	botID, secret := res.BotInfo.BotID, res.BotInfo.Secret
	if botID == "" || secret == "" {
		wecomRegistrations.delete(sessionID)
		jsonResponse(w, http.StatusOK, map[string]any{
			"status":    "error",
			"connected": false,
			"error":     "WeCom confirmed the scan but returned no bot credentials",
		})
		return
	}
	// Freshly minted by WeCom, so no subscribe handshake to validate —
	// the adapter's first connect is the check.
	// A persist failure (e.g. the bot is already bound elsewhere) ends
	// this session: report it as a final status, not a transient error.
	if _, err := s.persistWeComAccount(r, sess.scope, sess.scopeID, id, botID, secret); err != nil {
		wecomRegistrations.delete(sessionID)
		jsonResponse(w, http.StatusOK, map[string]any{"status": "error", "connected": false, "error": err.Error()})
		return
	}
	wecomRegistrations.delete(sessionID)
	jsonResponse(w, http.StatusOK, map[string]any{"status": "confirmed", "connected": true, "botId": botID})
}

// --- work.weixin.qq.com QR helpers ---

type wecomQRGenerateResp struct {
	Scode   string `json:"scode"`
	AuthURL string `json:"auth_url"`
}

type wecomQRQueryResp struct {
	Status  string `json:"status"`
	BotInfo struct {
		BotID  string `json:"botid"`
		Secret string `json:"secret"`
	} `json:"bot_info"`
}

// wecomQRPlat mirrors the CLI's plat code: 1 macOS, 2 Windows, 3 Linux.
func wecomQRPlat() string {
	switch runtime.GOOS {
	case "darwin":
		return "1"
	case "windows":
		return "2"
	case "linux":
		return "3"
	}
	return "0"
}

func wecomQRGet(ctx context.Context, u string, out any) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("contact wecom: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var env struct {
		Data json.RawMessage `json:"data"`
	}
	if resp.StatusCode != http.StatusOK || json.Unmarshal(body, &env) != nil || len(env.Data) == 0 {
		return fmt.Errorf("wecom QR HTTP %d: %s", resp.StatusCode, string(body))
	}
	return json.Unmarshal(env.Data, out)
}

func wecomQRGenerate(ctx context.Context) (*wecomQRGenerateResp, error) {
	var out wecomQRGenerateResp
	q := url.Values{"source": {wecomQRSource}, "plat": {wecomQRPlat()}}
	if err := wecomQRGet(ctx, wecomQRBase+"/generate?"+q.Encode(), &out); err != nil {
		return nil, err
	}
	if out.Scode == "" || out.AuthURL == "" {
		return nil, fmt.Errorf("wecom returned no QR code")
	}
	return &out, nil
}

func wecomQRQuery(ctx context.Context, scode string) (*wecomQRQueryResp, error) {
	var out wecomQRQueryResp
	if err := wecomQRGet(ctx, wecomQRBase+"/query_result?scode="+url.QueryEscape(scode), &out); err != nil {
		return nil, err
	}
	return &out, nil
}
