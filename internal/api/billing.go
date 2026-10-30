package api

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"github.com/fastclaw-ai/fastclaw/internal/auth"
	"github.com/fastclaw-ai/fastclaw/internal/usage"
)

// HandleGetUsage handles GET /v1/usage.
//
// Returns per-day token consumption so integrating apps can bill their
// own users. Each daily row carries agentId, userId and — for app_users —
// endUser (the app's external id), so an app can roll usage up by agent
// and by end-user.
//
// Query params:
//
//	days     — lookback window (default 30, max 90)
//	agent_id — only usage of this agent
//	end_user — only usage of this end-user (the app's external id, as
//	           sent in X-Fastclaw-End-User / `user`)
//	scope    — "app": the app owner plus every end-user it minted
//	user_id  — legacy: a FastClaw user id; must be the app itself or one
//	           of its end-users
//
// Without end_user / scope / user_id the result covers the caller's own
// namespace: the app, or the end-user named by X-Fastclaw-End-User.
func (s *Server) HandleGetUsage(w http.ResponseWriter, r *http.Request) {
	if s.meter == nil {
		writeAPIError(w, http.StatusServiceUnavailable, errTypeServer, codeNotConfigured, "usage metering not configured")
		return
	}

	ident, ok := auth.FromContext(r.Context())
	if !ok {
		writeUnauth(w, "authentication required")
		return
	}
	qs := r.URL.Query()

	days := 30
	if d := qs.Get("days"); d != "" {
		n, err := strconv.Atoi(d)
		if err != nil || n < 1 || n > 90 {
			writeBadRequest(w, "days must be an integer between 1 and 90")
			return
		}
		days = n
	}
	q := usage.Query{AgentID: strings.TrimSpace(qs.Get("agent_id")), Range: usage.LastN(days)}
	resp := map[string]any{"days": days}
	if q.AgentID != "" {
		resp["agentId"] = q.AgentID
	}

	switch {
	case strings.TrimSpace(qs.Get("end_user")) != "":
		endUser := strings.TrimSpace(qs.Get("end_user"))
		resp["endUser"] = endUser
		uid, found := s.lookupEndUser(r, ident.AccountID(), endUser)
		if !found {
			// Never seen → no usage. Don't mint a user just to say so.
			resp["daily"] = []usage.DailyUsage{}
			resp["totals"] = usage.Totals{}
			writeJSON(w, http.StatusOK, resp)
			return
		}
		q.UserID = uid
		resp["userId"] = uid
	case qs.Get("scope") == "app":
		q.AppOwnerID = ident.AccountID()
		resp["scope"] = "app"
		resp["userId"] = q.AppOwnerID
	case qs.Get("user_id") != "":
		uid := qs.Get("user_id")
		if !s.userInApp(r, ident, uid) {
			writeAPIError(w, http.StatusForbidden, errTypePermission, codeForbidden, "user_id is not this app or one of its end-users")
			return
		}
		q.UserID = uid
		resp["userId"] = uid
	default:
		q.UserID = ident.EffectiveUserID()
		resp["userId"] = q.UserID
		if ident.EndUser != "" {
			resp["endUser"] = ident.EndUser
		}
	}

	daily, totals, err := s.meter.Query(r.Context(), q)
	if err != nil {
		writeServerError(w, err)
		return
	}
	if daily == nil {
		daily = []usage.DailyUsage{}
	}
	resp["daily"] = daily
	resp["totals"] = totals
	writeJSON(w, http.StatusOK, resp)
}

// lookupEndUser returns the app_user id for (app, externalID) without
// creating one.
func (s *Server) lookupEndUser(r *http.Request, appID, externalID string) (string, bool) {
	if s.store == nil {
		return "", false
	}
	u, err := s.store.GetUserByExternal(r.Context(), appID, externalID)
	if err != nil || u == nil {
		return "", false
	}
	return u.ID, true
}

// userInApp reports whether userID is the caller's app account or one of
// the end-users (app_users) it minted. Platform admins may name anyone.
func (s *Server) userInApp(r *http.Request, ident auth.Identity, userID string) bool {
	if userID == "" {
		return false
	}
	if ident.CanAdminPlatform() || userID == ident.AccountID() || userID == ident.EffectiveUserID() {
		return true
	}
	if s.store == nil {
		return false
	}
	u, err := s.store.GetUser(r.Context(), userID)
	return err == nil && u != nil && u.OwnerUserID == ident.AccountID()
}

// HandleSetQuota handles PUT /v1/quota.
//
// Sets the monthly token/request ceiling for a user. Called by upstream
// SaaS apps when a user subscribes/upgrades/downgrades. The agent loop
// checks this before every LLM call.
//
// Request body:
//
//	{
//	  "user_id": "u_xxx",
//	  "monthly_token_limit": 5000000,
//	  "monthly_request_limit": 10000,
//	  "reset_day": 1
//	}
func (s *Server) HandleSetQuota(w http.ResponseWriter, r *http.Request) {
	if s.quotaStore == nil {
		writeAPIError(w, http.StatusServiceUnavailable, errTypeServer, codeNotConfigured, "quota management not configured")
		return
	}

	ident, ok := auth.FromContext(r.Context())
	if !ok {
		writeUnauth(w, "authentication required")
		return
	}

	var req struct {
		UserID              string `json:"user_id"`
		MonthlyTokenLimit   int64  `json:"monthly_token_limit"`
		MonthlyRequestLimit int64  `json:"monthly_request_limit"`
		ResetDay            int    `json:"reset_day"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeAPIError(w, http.StatusBadRequest, errTypeInvalidRequest, codeInvalidRequest, "invalid request body")
		return
	}
	if req.UserID == "" {
		writeAPIError(w, http.StatusBadRequest, errTypeInvalidRequest, codeInvalidRequest, "user_id is required")
		return
	}
	if !s.userInApp(r, ident, req.UserID) {
		writeAPIError(w, http.StatusForbidden, errTypePermission, codeForbidden, "user_id is not this app or one of its end-users")
		return
	}
	if req.ResetDay < 1 || req.ResetDay > 28 {
		req.ResetDay = 1
	}

	q := &usage.Quota{
		UserID:              req.UserID,
		MonthlyTokenLimit:   req.MonthlyTokenLimit,
		MonthlyRequestLimit: req.MonthlyRequestLimit,
		ResetDay:            req.ResetDay,
	}
	if err := s.quotaStore.SetQuota(r.Context(), q); err != nil {
		writeAPIError(w, http.StatusInternalServerError, errTypeServer, codeInternal, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"ok":    true,
		"quota": q,
	})
}

// HandleGetQuota handles GET /v1/quota.
//
// Returns the current quota for a user.
// Query params: user_id (required).
func (s *Server) HandleGetQuota(w http.ResponseWriter, r *http.Request) {
	if s.quotaStore == nil {
		writeAPIError(w, http.StatusServiceUnavailable, errTypeServer, codeNotConfigured, "quota management not configured")
		return
	}

	ident, ok := auth.FromContext(r.Context())
	if !ok {
		writeUnauth(w, "authentication required")
		return
	}

	userID := r.URL.Query().Get("user_id")
	if userID == "" {
		writeAPIError(w, http.StatusBadRequest, errTypeInvalidRequest, codeInvalidRequest, "user_id query param is required")
		return
	}
	if !s.userInApp(r, ident, userID) {
		writeAPIError(w, http.StatusForbidden, errTypePermission, codeForbidden, "user_id is not this app or one of its end-users")
		return
	}

	q, err := s.quotaStore.GetQuota(r.Context(), userID)
	if err != nil {
		writeAPIError(w, http.StatusNotFound, errTypeNotFound, codeQuotaNotFound, "no quota configured for this user")
		return
	}

	// Also return current usage status.
	if s.meter != nil {
		status, err := usage.CheckQuota(r.Context(), s.quotaStore, s.meter, userID)
		if err == nil {
			writeJSON(w, http.StatusOK, map[string]any{
				"quota":  q,
				"status": status,
			})
			return
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"quota": q,
	})
}

// HandleDeleteQuota handles DELETE /v1/quota.
//
// Removes the quota for a user (reverts to unlimited).
// Query params: user_id (required).
func (s *Server) HandleDeleteQuota(w http.ResponseWriter, r *http.Request) {
	if s.quotaStore == nil {
		writeAPIError(w, http.StatusServiceUnavailable, errTypeServer, codeNotConfigured, "quota management not configured")
		return
	}

	ident, ok := auth.FromContext(r.Context())
	if !ok {
		writeUnauth(w, "authentication required")
		return
	}

	userID := r.URL.Query().Get("user_id")
	if userID == "" {
		writeAPIError(w, http.StatusBadRequest, errTypeInvalidRequest, codeInvalidRequest, "user_id query param is required")
		return
	}
	if !s.userInApp(r, ident, userID) {
		writeAPIError(w, http.StatusForbidden, errTypePermission, codeForbidden, "user_id is not this app or one of its end-users")
		return
	}

	if err := s.quotaStore.DeleteQuota(r.Context(), userID); err != nil {
		writeAPIError(w, http.StatusInternalServerError, errTypeServer, codeInternal, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}
