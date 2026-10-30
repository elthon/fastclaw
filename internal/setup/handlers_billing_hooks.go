package setup

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/fastclaw-ai/fastclaw/internal/store"
)

// Billing hooks: the primitives an external billing system — e.g. a
// hosted FastClaw Cloud built on top of the open-source runtime — needs to
// charge for usage, without FastClaw itself knowing about prices, balances
// or payments. All routes are platform-admin only (super_admin session or
// an admin API key):
//
//	GET  /api/admin/usage/events?after=<id>&limit=<n>  incremental usage export
//	GET  /api/admin/users/{id}/billing-hold            read an account's hold
//	PUT  /api/admin/users/{id}/billing-hold            hold / release an account
//	POST /api/admin/users/{id}/login-link              one-time console sign-in link
//
// plus the public GET /auth/login-link that redeems a link.

const (
	defaultLoginLinkTTL = 5 * time.Minute
	maxLoginLinkTTL     = 30 * time.Minute
)

// handleUsageEvents exports model-call usage after a cursor. A billing
// system polls it, prices each event, debits the paying account
// (account_id) and stores next_after as its cursor. Events become visible
// a few seconds after the call, so polling never skips one.
func (s *Server) handleUsageEvents(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	after, err := strconv.ParseInt(strings.TrimSpace(q.Get("after")), 10, 64)
	if q.Get("after") != "" && (err != nil || after < 0) {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"error": "after must be a non-negative integer"})
		return
	}
	limit := 500
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 1000 {
			jsonResponse(w, http.StatusBadRequest, map[string]any{"error": "limit must be 1-1000"})
			return
		}
		limit = n
	}
	events, err := s.dataStore.ListUsageEvents(r.Context(), after, limit)
	if err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	if events == nil {
		events = []store.UsageEvent{}
	}
	next := after
	if len(events) > 0 {
		next = events[len(events)-1].ID
	}
	jsonResponse(w, http.StatusOK, map[string]any{
		"events":     events,
		"next_after": next,
		"has_more":   len(events) == limit,
	})
}

func (s *Server) handleGetBillingHold(w http.ResponseWriter, r *http.Request) {
	hold, reason, err := s.dataStore.GetBillingHold(r.Context(), r.PathValue("id"))
	if err != nil {
		writeStoreError(w, err)
		return
	}
	jsonResponse(w, http.StatusOK, map[string]any{"hold": hold, "reason": reason})
}

// handleSetBillingHold holds or releases an account. While held, its
// agents refuse new turns on every channel and /v1 chat returns
// 402 payment_required. Body: {"hold": true, "reason": "balance exhausted"}.
func (s *Server) handleSetBillingHold(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Hold   *bool  `json:"hold"`
		Reason string `json:"reason"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Hold == nil {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"error": `body must be {"hold": true|false, "reason": "..."}`})
		return
	}
	id := r.PathValue("id")
	if err := s.dataStore.SetBillingHold(r.Context(), id, *req.Hold, strings.TrimSpace(req.Reason)); err != nil {
		writeStoreError(w, err)
		return
	}
	hold, reason, _ := s.dataStore.GetBillingHold(r.Context(), id)
	jsonResponse(w, http.StatusOK, map[string]any{"hold": hold, "reason": reason})
}

// handleCreateLoginLink mints a single-use link that signs the browser
// into the console as the account — how a billing site sends its users
// to FastClaw without a second password. Body (optional):
// {"redirect": "/console/", "ttl_seconds": 300}.
func (s *Server) handleCreateLoginLink(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Redirect   string `json:"redirect"`
		TTLSeconds int    `json:"ttl_seconds"`
	}
	if r.ContentLength != 0 {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			jsonResponse(w, http.StatusBadRequest, map[string]any{"error": "invalid body"})
			return
		}
	}
	id := r.PathValue("id")
	u, err := s.dataStore.GetUser(r.Context(), id)
	if err != nil || u == nil {
		writeStoreError(w, store.ErrNotFound)
		return
	}
	if u.Role != "user" && u.Role != "super_admin" {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"error": "only console accounts can sign in"})
		return
	}
	redirect := safeRedirect(req.Redirect)
	ttl := defaultLoginLinkTTL
	if req.TTLSeconds > 0 {
		ttl = time.Duration(req.TTLSeconds) * time.Second
		if ttl > maxLoginLinkTTL {
			ttl = maxLoginLinkTTL
		}
	}
	var buf [32]byte
	if _, err := rand.Read(buf[:]); err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	token := hex.EncodeToString(buf[:])
	expires := time.Now().UTC().Add(ttl)
	if err := s.dataStore.CreateLoginToken(r.Context(), hashLoginToken(token), id, expires); err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	link := requestOrigin(r) + "/auth/login-link?" + url.Values{"token": {token}, "redirect": {redirect}}.Encode()
	jsonResponse(w, http.StatusOK, map[string]any{"url": link, "expires_at": expires})
}

// handleRedeemLoginLink signs the browser in with a login-link token and
// sends it on. Single use; an invalid or expired token lands on the
// sign-in page.
func (s *Server) handleRedeemLoginLink(w http.ResponseWriter, r *http.Request) {
	token := r.URL.Query().Get("token")
	redirect := safeRedirect(r.URL.Query().Get("redirect"))
	if token == "" || s.authResolver == nil {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	uid, err := s.dataStore.ConsumeLoginToken(r.Context(), hashLoginToken(token))
	if err != nil {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	cookie, err := s.authResolver.IssueSession(r.Context(), uid)
	if err != nil {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	http.SetCookie(w, cookie)
	w.Header().Set("Cache-Control", "no-store")
	http.Redirect(w, r, redirect, http.StatusFound)
}

func hashLoginToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// safeRedirect keeps a post-login redirect on this site: a path, never a
// URL or protocol-relative "//host".
func safeRedirect(p string) string {
	p = strings.TrimSpace(p)
	if p == "" || !strings.HasPrefix(p, "/") || strings.HasPrefix(p, "//") || strings.Contains(p, "\\") {
		return "/"
	}
	return p
}

// requestOrigin is the externally visible origin of a request, honoring
// the usual reverse-proxy headers.
func requestOrigin(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if p := strings.TrimSpace(strings.Split(r.Header.Get("X-Forwarded-Proto"), ",")[0]); p == "http" || p == "https" {
		scheme = p
	}
	host := r.Host
	if h := strings.TrimSpace(strings.Split(r.Header.Get("X-Forwarded-Host"), ",")[0]); h != "" {
		host = h
	}
	return scheme + "://" + host
}

func writeStoreError(w http.ResponseWriter, err error) {
	if errors.Is(err, store.ErrNotFound) {
		jsonResponse(w, http.StatusNotFound, map[string]any{"error": "not found"})
		return
	}
	jsonResponse(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
}
