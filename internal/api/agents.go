package api

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/fastclaw-ai/fastclaw/internal/agent"
	"github.com/fastclaw-ai/fastclaw/internal/auth"
	"github.com/fastclaw-ai/fastclaw/internal/scope"
	"github.com/fastclaw-ai/fastclaw/internal/store"
	"github.com/fastclaw-ai/fastclaw/internal/users"
)

// Agents created through /v1 belong to the app — the account that owns
// the api key. The runtime never records which of the app's end-users an
// agent "belongs to"; apps keep that relationship themselves, optionally
// mirrored into `metadata` for filtering.

const (
	maxMetadataKeys     = 16
	maxMetadataKeyLen   = 64
	maxMetadataValueLen = 512

	defaultAgentPageSize = 20
	maxAgentPageSize     = 100

	// maxSystemFileBytes caps a single identity file written through
	// /v1. KNOWLEDGE.md uses the same 256KB ceiling as the console.
	maxSystemFileBytes = 256 * 1024

	agentDefaultsNamespace = "agents.defaults"
)

// v1SystemFiles are the identity files an app may write through
// PUT /v1/agents/{id}/system-files/{name}. They are shared by every
// end-user of the agent. Per-user files (USER.md, MEMORY.md) are written
// by the agent itself during conversations and are not exposed here.
var v1SystemFiles = map[string]bool{
	"SOUL.md": true, "IDENTITY.md": true, "AGENTS.md": true,
	"BOOTSTRAP.md": true, "TOOLS.md": true, "HEARTBEAT.md": true,
	"KNOWLEDGE.md": true,
}

// errAgentNotFound is returned by agent lookups for both "no such agent"
// and "agent belongs to another app", so responses never reveal whether
// an out-of-scope agent exists.
var errAgentNotFound = errors.New("agent not found")

// appAgent loads agentID and verifies the caller's app may use it: the
// agent must be owned by the app (Identity.AppID) and pass the api key's
// agent ACL. Platform-admin keys are scoped to their own app here too —
// /v1 is the app API, not the admin API.
func (s *Server) appAgent(r *http.Request, agentID string) (*store.AgentRecord, error) {
	ident, ok := auth.FromContext(r.Context())
	if !ok {
		return nil, errors.New("unauthorized")
	}
	if s.store == nil {
		return nil, errors.New("agent store not configured")
	}
	agentID = strings.TrimSpace(agentID)
	if agentID == "" {
		return nil, errAgentNotFound
	}
	rec, err := s.store.GetAgent(r.Context(), agentID)
	if err != nil || rec == nil {
		return nil, errAgentNotFound
	}
	if rec.UserID != ident.AccountID() || !ident.CanAccessAgent(rec.ID) {
		return nil, errAgentNotFound
	}
	return rec, nil
}

// resolveChatAgent picks the agent for a /v1 conversation.
//
//   - agentID set: strict. The agent must belong to the caller's app;
//     otherwise errAgentNotFound — never a fallback to another agent.
//   - agentID empty: the app's default agent (the only agent when the
//     app has exactly one), kept for backward compatibility.
//
// The agent runs in the caller's data namespace: the app's own space,
// or — when the request names an end-user — that end-user's app_user
// space, so sessions and personal memory stay per end-user while the
// agent itself is the app's.
func (s *Server) resolveChatAgent(r *http.Request, agentID string) (*agent.Agent, error) {
	ident, ok := auth.FromContext(r.Context())
	if !ok {
		return nil, errors.New("unauthorized")
	}
	nsUser := ident.EffectiveUserID()
	space, err := s.resolver.UserSpaceFor(nsUser)
	if err != nil {
		return nil, err
	}

	// An "agent" key granted exactly one agent talks to it.
	if agentID == "" && ident.APIKeyType == users.APIKeyTypeAgent && len(ident.APIKeyAgents) == 1 {
		agentID = ident.APIKeyAgents[0]
	}
	if agentID == "" {
		appSpace := space
		if appID := ident.AccountID(); appID != nsUser {
			if appSpace, err = s.resolver.UserSpaceFor(appID); err != nil {
				return nil, err
			}
		}
		def := defaultAgent(appSpace)
		if def == nil || !ident.CanAccessAgent(def.Name()) {
			return nil, errAgentNotFound
		}
		if appSpace == space {
			return def, nil
		}
		agentID = def.Name()
	} else if s.store != nil {
		if _, err := s.appAgent(r, agentID); err != nil {
			return nil, err
		}
	} else if ag := space.Agents.AgentByID(agentID); ag == nil || !ident.CanAccessAgent(agentID) {
		// No store (unit tests): only agents already in the space count.
		return nil, errAgentNotFound
	}

	if ag := space.Agents.AgentByID(agentID); ag != nil {
		return ag, nil
	}
	// The agent belongs to the app but isn't loaded in this namespace
	// yet: an on-demand app space, or an end-user space that has never
	// talked to it. Attach it now.
	injector, ok := s.resolver.(AgentInjector)
	if !ok {
		return nil, errAgentNotFound
	}
	if err := injector.EnsureAgent(r.Context(), nsUser, agentID); err != nil {
		return nil, errAgentNotFound
	}
	if ag := space.Agents.AgentByID(agentID); ag != nil {
		return ag, nil
	}
	// EnsureAgent may have attached into a freshly reloaded space.
	if space, err = s.resolver.UserSpaceFor(nsUser); err == nil {
		if ag := space.Agents.AgentByID(agentID); ag != nil {
			return ag, nil
		}
	}
	return nil, errAgentNotFound
}

// defaultAgent returns the space's default agent, falling back to the
// first loaded one — the pre-existing behavior for requests that don't
// name an agent.
func defaultAgent(space *UserSpaceView) *agent.Agent {
	if space == nil || space.Agents == nil {
		return nil
	}
	if def := space.Agents.DefaultAgent(); def != nil {
		return def
	}
	if all := space.Agents.All(); len(all) > 0 {
		return all[0]
	}
	return nil
}

// --- /v1/agents ---

type createAgentRequest struct {
	Name         string            `json:"name"`
	Description  string            `json:"description,omitempty"`
	Instructions string            `json:"instructions,omitempty"`
	Model        string            `json:"model,omitempty"`
	Metadata     map[string]string `json:"metadata,omitempty"`
}

type updateAgentRequest struct {
	Name         *string `json:"name,omitempty"`
	Description  *string `json:"description,omitempty"`
	Instructions *string `json:"instructions,omitempty"`
	Model        *string `json:"model,omitempty"`
	// Metadata is merged into the existing map. A key set to null or ""
	// is removed.
	Metadata map[string]*string `json:"metadata,omitempty"`
}

// HandleCreateAgent handles POST /v1/agents.
func (s *Server) HandleCreateAgent(w http.ResponseWriter, r *http.Request) {
	ident, ok := s.requireAgentAPI(w, r)
	if !ok {
		return
	}
	if ident.ReadOnly() || !ident.CanCreateAgent() {
		writeAPIError(w, http.StatusForbidden, errTypePermission, codeForbidden, "this api key cannot create agents")
		return
	}
	var req createAgentRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeBadRequest(w, "invalid request body")
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" {
		writeBadRequest(w, "name is required")
		return
	}
	if err := validateMetadata(req.Metadata); err != nil {
		writeBadRequest(w, err.Error())
		return
	}
	owner := ident.AccountID()
	ctx := r.Context()
	if u, err := s.store.GetUser(ctx, owner); err == nil && u != nil && u.AgentQuota >= 0 {
		owned, err := s.store.ListAgentIDs(ctx, owner)
		if err != nil {
			writeServerError(w, err)
			return
		}
		if int64(len(owned)) >= u.AgentQuota {
			writeAPIError(w, http.StatusForbidden, errTypePermission, codeAgentQuotaExceeded,
				fmt.Sprintf("agent quota reached (%d)", u.AgentQuota))
			return
		}
	}
	id, err := newAgentID()
	if err != nil {
		writeServerError(w, err)
		return
	}
	rec := &store.AgentRecord{ID: id, UserID: owner, Name: req.Name, Config: map[string]interface{}{}}
	if d := strings.TrimSpace(req.Description); d != "" {
		rec.Config["description"] = d
	}
	if len(req.Metadata) > 0 {
		rec.Config["metadata"] = metadataToConfig(req.Metadata)
	}
	if err := s.store.SaveAgent(ctx, rec); err != nil {
		writeServerError(w, err)
		return
	}
	if m := strings.TrimSpace(req.Model); m != "" {
		if err := s.patchAgentDefaults(r, id, map[string]interface{}{"model": m}); err != nil {
			writeServerError(w, err)
			return
		}
	}
	if req.Instructions != "" {
		if err := s.store.SaveAgentFile(ctx, id, owner, "SOUL.md", []byte(req.Instructions)); err != nil {
			writeServerError(w, err)
			return
		}
	}
	// Reload the app's space so consoles listing loaded agents see the
	// new one. On-demand spaces reload only their pinned agents.
	s.invalidateUser(owner)
	if fresh, err := s.store.GetAgent(ctx, id); err == nil && fresh != nil {
		rec = fresh
	}
	writeJSON(w, http.StatusOK, map[string]any{"agent": s.agentView(r, rec)})
}

// HandleListAgents handles GET /v1/agents.
//
// Lists the app's agents straight from the database — independent of
// which agents happen to be loaded — newest first.
//
// Query params:
//
//	limit       page size (1–100). Omit to get every agent in one page
//	            (the pre-pagination behavior).
//	cursor      `next_cursor` from the previous page
//	metadata[k] only agents whose metadata k equals the value; also
//	            accepted as metadata.k. Multiple filters are ANDed.
func (s *Server) HandleListAgents(w http.ResponseWriter, r *http.Request) {
	ident, ok := s.requireAgentAPI(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	limit := 0
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			writeBadRequest(w, "limit must be a positive integer")
			return
		}
		limit = min(n, maxAgentPageSize)
	}
	filter := metadataFilter(q)
	recs, err := s.store.ListAgents(r.Context(), ident.AccountID())
	if err != nil {
		writeServerError(w, err)
		return
	}
	matched := make([]store.AgentRecord, 0, len(recs))
	for _, rec := range recs {
		if !ident.CanAccessAgent(rec.ID) || !metadataMatches(agentMetadata(&rec), filter) {
			continue
		}
		matched = append(matched, rec)
	}
	if cursor := q.Get("cursor"); cursor != "" {
		idx := -1
		for i := range matched {
			if matched[i].ID == cursor {
				idx = i
				break
			}
		}
		if idx < 0 {
			writeBadRequest(w, "invalid cursor")
			return
		}
		matched = matched[idx+1:]
	}
	hasMore := false
	if limit > 0 && len(matched) > limit {
		matched = matched[:limit]
		hasMore = true
	}
	out := make([]map[string]any, 0, len(matched))
	for i := range matched {
		out = append(out, s.agentView(r, &matched[i]))
	}
	resp := map[string]any{"agents": out, "has_more": hasMore}
	if hasMore {
		resp["next_cursor"] = matched[len(matched)-1].ID
	}
	writeJSON(w, http.StatusOK, resp)
}

// HandleGetAgent handles GET /v1/agents/{id}.
func (s *Server) HandleGetAgent(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireAgentAPI(w, r); !ok {
		return
	}
	rec, err := s.appAgent(r, r.PathValue("id"))
	if err != nil {
		writeAgentNotFound(w)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"agent": s.agentView(r, rec)})
}

// HandleUpdateAgent handles PATCH /v1/agents/{id}.
func (s *Server) HandleUpdateAgent(w http.ResponseWriter, r *http.Request) {
	ident, ok := s.requireAgentAPI(w, r)
	if !ok {
		return
	}
	if ident.ReadOnly() {
		writeAPIError(w, http.StatusForbidden, errTypePermission, codeForbidden, "read-only request")
		return
	}
	rec, err := s.appAgent(r, r.PathValue("id"))
	if err != nil {
		writeAgentNotFound(w)
		return
	}
	var req updateAgentRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeBadRequest(w, "invalid request body")
		return
	}
	if rec.Config == nil {
		rec.Config = map[string]interface{}{}
	}
	if req.Name != nil {
		name := strings.TrimSpace(*req.Name)
		if name == "" {
			writeBadRequest(w, "name cannot be empty")
			return
		}
		rec.Name = name
	}
	if req.Description != nil {
		if d := strings.TrimSpace(*req.Description); d == "" {
			delete(rec.Config, "description")
		} else {
			rec.Config["description"] = d
		}
	}
	if req.Metadata != nil {
		md := agentMetadata(rec)
		for k, v := range req.Metadata {
			if v == nil || *v == "" {
				delete(md, k)
			} else {
				md[k] = *v
			}
		}
		if err := validateMetadata(md); err != nil {
			writeBadRequest(w, err.Error())
			return
		}
		if len(md) == 0 {
			delete(rec.Config, "metadata")
		} else {
			rec.Config["metadata"] = metadataToConfig(md)
		}
	}
	ctx := r.Context()
	if err := s.store.SaveAgent(ctx, rec); err != nil {
		writeServerError(w, err)
		return
	}
	if req.Model != nil {
		var model interface{}
		if m := strings.TrimSpace(*req.Model); m != "" {
			model = m
		}
		if err := s.patchAgentDefaults(r, rec.ID, map[string]interface{}{"model": model}); err != nil {
			writeServerError(w, err)
			return
		}
	}
	if req.Instructions != nil {
		if err := s.store.SaveAgentFile(ctx, rec.ID, rec.UserID, "SOUL.md", []byte(*req.Instructions)); err != nil {
			writeServerError(w, err)
			return
		}
	}
	// Drop every space holding the agent — the app's and each end-user's
	// — so the next turn runs with the new model and identity.
	s.invalidateAgent(rec.ID)
	if fresh, err := s.store.GetAgent(ctx, rec.ID); err == nil && fresh != nil {
		rec = fresh
	}
	writeJSON(w, http.StatusOK, map[string]any{"agent": s.agentView(r, rec)})
}

// HandleDeleteAgent handles DELETE /v1/agents/{id}. A missing agent is a
// 404 agent_not_found, which apps can treat as "already deleted".
func (s *Server) HandleDeleteAgent(w http.ResponseWriter, r *http.Request) {
	ident, ok := s.requireAgentAPI(w, r)
	if !ok {
		return
	}
	if ident.ReadOnly() || !ident.CanCreateAgent() {
		writeAPIError(w, http.StatusForbidden, errTypePermission, codeForbidden, "this api key cannot delete agents")
		return
	}
	rec, err := s.appAgent(r, r.PathValue("id"))
	if err != nil {
		writeAgentNotFound(w)
		return
	}
	if err := s.store.DeleteAgent(r.Context(), rec.ID); err != nil {
		writeServerError(w, err)
		return
	}
	s.invalidateAgent(rec.ID)
	writeJSON(w, http.StatusOK, map[string]any{"id": rec.ID, "deleted": true})
}

// HandlePutAgentSystemFile handles PUT /v1/agents/{id}/system-files/{name}.
// Body: {"content": "..."}. Writes one of the agent's shared identity
// files (SOUL.md, IDENTITY.md, AGENTS.md, …).
func (s *Server) HandlePutAgentSystemFile(w http.ResponseWriter, r *http.Request) {
	ident, ok := s.requireAgentAPI(w, r)
	if !ok {
		return
	}
	if ident.ReadOnly() {
		writeAPIError(w, http.StatusForbidden, errTypePermission, codeForbidden, "read-only request")
		return
	}
	name := r.PathValue("name")
	if !v1SystemFiles[name] {
		writeBadRequest(w, "unsupported system file "+strconv.Quote(name))
		return
	}
	rec, err := s.appAgent(r, r.PathValue("id"))
	if err != nil {
		writeAgentNotFound(w)
		return
	}
	var body struct {
		Content *string `json:"content"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 2*maxSystemFileBytes)).Decode(&body); err != nil || body.Content == nil {
		writeBadRequest(w, "body must be {\"content\": \"...\"}")
		return
	}
	if len(*body.Content) > maxSystemFileBytes {
		writeAPIError(w, http.StatusRequestEntityTooLarge, errTypeInvalidRequest, codeInvalidRequest,
			fmt.Sprintf("%s is too large; maximum size is %dKB", name, maxSystemFileBytes/1024))
		return
	}
	if err := s.store.SaveAgentFile(r.Context(), rec.ID, rec.UserID, name, []byte(*body.Content)); err != nil {
		writeServerError(w, err)
		return
	}
	s.invalidateAgent(rec.ID)
	writeJSON(w, http.StatusOK, map[string]any{"id": rec.ID, "name": name, "ok": true})
}

// requireAgentAPI checks the preconditions shared by /v1/agents handlers.
func (s *Server) requireAgentAPI(w http.ResponseWriter, r *http.Request) (auth.Identity, bool) {
	ident, ok := auth.FromContext(r.Context())
	if !ok || ident.AccountID() == "" {
		writeUnauth(w, "authentication required")
		return auth.Identity{}, false
	}
	if s.store == nil {
		writeAPIError(w, http.StatusServiceUnavailable, errTypeServer, codeNotConfigured, "agent store not configured")
		return auth.Identity{}, false
	}
	return ident, true
}

// agentView renders an agent for /v1 responses.
func (s *Server) agentView(r *http.Request, rec *store.AgentRecord) map[string]any {
	desc, _ := rec.Config["description"].(string)
	md := agentMetadata(rec)
	return map[string]any{
		"id":           rec.ID,
		"display_name": rec.Name,
		// name is the agent id, as /v1/agents has always returned it.
		// Deprecated: use id, and display_name for the human name.
		"name":        rec.ID,
		"description": desc,
		"model":       s.agentModel(r, rec.ID),
		"metadata":    md,
		"created_at":  rec.CreatedAt.UTC().Format(time.RFC3339),
		"updated_at":  rec.UpdatedAt.UTC().Format(time.RFC3339),
	}
}

// agentModel returns the agent's own model override ("" = app default).
func (s *Server) agentModel(r *http.Request, agentID string) string {
	rec, err := s.store.GetConfigByName(r.Context(), store.KindSetting, "", agentID, agentDefaultsNamespace)
	if err != nil || rec == nil {
		return ""
	}
	m, _ := rec.Data["model"].(string)
	return m
}

// patchAgentDefaults merges patch into the agent-scope agents.defaults
// row. A nil value removes that key; an empty result deletes the row.
func (s *Server) patchAgentDefaults(r *http.Request, agentID string, patch map[string]interface{}) error {
	data := map[string]interface{}{}
	if rec, err := s.store.GetConfigByName(r.Context(), store.KindSetting, "", agentID, agentDefaultsNamespace); err == nil && rec != nil {
		for k, v := range rec.Data {
			data[k] = v
		}
	}
	for k, v := range patch {
		if v == nil {
			delete(data, k)
		} else {
			data[k] = v
		}
	}
	if len(data) == 0 {
		data = nil
	}
	return scope.SaveSettingByScope(r.Context(), s.store, scope.Agent, agentID, agentDefaultsNamespace, data)
}

func (s *Server) invalidateUser(userID string) {
	if inv, ok := s.resolver.(interface{ InvalidateUser(string) }); ok {
		inv.InvalidateUser(userID)
	}
}

func (s *Server) invalidateAgent(agentID string) {
	if inv, ok := s.resolver.(interface{ InvalidateAgent(string) }); ok {
		inv.InvalidateAgent(agentID)
	}
}

// --- metadata ---

// validateMetadata enforces the documented limits: at most 16 keys,
// keys up to 64 characters, values up to 512 characters.
func validateMetadata(md map[string]string) error {
	if len(md) > maxMetadataKeys {
		return fmt.Errorf("metadata can have at most %d keys", maxMetadataKeys)
	}
	for k, v := range md {
		if strings.TrimSpace(k) == "" {
			return errors.New("metadata keys cannot be empty")
		}
		if utf8.RuneCountInString(k) > maxMetadataKeyLen {
			return fmt.Errorf("metadata key %q exceeds %d characters", k, maxMetadataKeyLen)
		}
		if utf8.RuneCountInString(v) > maxMetadataValueLen {
			return fmt.Errorf("metadata value for %q exceeds %d characters", k, maxMetadataValueLen)
		}
	}
	return nil
}

// agentMetadata returns a copy of the agent's metadata map (never nil).
func agentMetadata(rec *store.AgentRecord) map[string]string {
	out := map[string]string{}
	if rec == nil {
		return out
	}
	switch md := rec.Config["metadata"].(type) {
	case map[string]interface{}:
		for k, v := range md {
			if s, ok := v.(string); ok {
				out[k] = s
			}
		}
	case map[string]string:
		for k, v := range md {
			out[k] = v
		}
	}
	return out
}

func metadataToConfig(md map[string]string) map[string]interface{} {
	out := make(map[string]interface{}, len(md))
	for k, v := range md {
		out[k] = v
	}
	return out
}

// metadataFilter collects metadata[k]=v and metadata.k=v query params.
func metadataFilter(q map[string][]string) map[string]string {
	out := map[string]string{}
	for key, vals := range q {
		if len(vals) == 0 {
			continue
		}
		var k string
		switch {
		case strings.HasPrefix(key, "metadata[") && strings.HasSuffix(key, "]"):
			k = key[len("metadata[") : len(key)-1]
		case strings.HasPrefix(key, "metadata."):
			k = key[len("metadata."):]
		default:
			continue
		}
		if k != "" {
			out[k] = vals[0]
		}
	}
	return out
}

func metadataMatches(md, filter map[string]string) bool {
	for k, v := range filter {
		if md[k] != v {
			return false
		}
	}
	return true
}

// newAgentID mints an id in the same agt_<20 hex> shape the console uses.
func newAgentID() (string, error) {
	var buf [10]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", err
	}
	return "agt_" + hex.EncodeToString(buf[:]), nil
}
