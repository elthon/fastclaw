package api

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/fastclaw-ai/fastclaw/internal/auth"
)

// HandleProvisionAppUser handles POST /v1/users.
//
// Authenticated by api_key only. Mints (or returns) the fastclaw user
// representing the calling app's end-user identified by external_id.
// Idempotent: repeated calls with the same external_id return the same
// fastclaw user_id, regardless of whether the row already existed.
//
// Request body: { "external_id": "...", "display_name": "..." (optional) }
// Response:     { "user_id": "u_…", "external_id": "...", "created": bool }
//
// Sessions, agent_files, and scope=user configs all key off the returned
// user_id, so once the calling app has it, every downstream interaction
// for that end-user partitions cleanly. Apps that prefer not to
// pre-provision can skip this endpoint entirely and pass `user` in the
// /v1/chat/completions body (or the X-Fastclaw-End-User header) on
// every call — the auth layer lazy-mints on first sight either way.
func (s *Server) HandleProvisionAppUser(w http.ResponseWriter, r *http.Request) {
	ident, ok := auth.FromContext(r.Context())
	if !ok || ident.AuthMethod != "apikey" || ident.APIKeyID == "" {
		writeAPIError(w, http.StatusUnauthorized, errTypeAuthentication, codeUnauthorized, "api_key required")
		return
	}

	var req struct {
		ExternalID  string `json:"external_id"`
		DisplayName string `json:"display_name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeAPIError(w, http.StatusBadRequest, errTypeInvalidRequest, codeInvalidRequest, "invalid request body")
		return
	}
	req.ExternalID = strings.TrimSpace(req.ExternalID)
	if req.ExternalID == "" {
		writeAPIError(w, http.StatusBadRequest, errTypeInvalidRequest, codeInvalidRequest, "external_id is required")
		return
	}

	// Going through SwitchToAppUser keeps the mint logic in one place
	// — the same code path the request-time switch uses. The returned
	// identity carries the resolved app_user user_id; we don't write it
	// back onto the request context here because this endpoint is a
	// pure provisioning call, not a passthrough.
	switched, err := s.authResolver.SwitchToAppUser(r.Context(), ident, req.ExternalID)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, errTypeServer, codeInternal, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"user_id":     switched.UserID,
		"external_id": req.ExternalID,
	})
}
