package api

import (
	"context"
	"encoding/base64"
	"io"
	"mime"
	"net/http"
	"net/url"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/fastclaw-ai/fastclaw/internal/workspace"
)

// Files an agent produces in a /v1 conversation come back with the reply.
// The conversation's workspace (sessions/<session key>/, or the project's
// folder) is listed before and after the turn; files the reply links as
// /workspace/<path>, plus files created or changed during the turn, are
// returned with a download URL — or inline as a data URL on request.

const (
	maxTurnFiles        = 20
	maxInlineFileBytes  = 10 << 20
	maxInlineTotalBytes = 25 << 20
	codeFileNotFound    = "file_not_found"
)

// turnFile is one file in a /v1 chat response (FastClaw extension).
type turnFile struct {
	Name        string `json:"name"`
	Path        string `json:"path"`
	Size        int64  `json:"size"`
	ContentType string `json:"content_type"`
	URL         string `json:"url"`
	DataURL     string `json:"data_url,omitempty"`
}

// workspaceRefPattern finds /workspace/<path> references in a reply —
// markdown links and images as well as bare mentions.
var workspaceRefPattern = regexp.MustCompile(`/workspace/([^\s)"'<>\]` + "`" + `]+)`)

// workspaceSnapshot is a conversation workspace's file list at one moment.
type workspaceSnapshot map[string]workspace.ObjectInfo

func (s *Server) snapshotWorkspace(ctx context.Context, agentID, projectID, sessionKey string) workspaceSnapshot {
	if s.workspace == nil {
		return nil
	}
	objs, err := s.workspace.List(ctx, agentID, projectID, sessionKey)
	if err != nil {
		return nil
	}
	out := make(workspaceSnapshot, len(objs))
	for _, o := range objs {
		out[filepath.ToSlash(o.Path)] = o
	}
	return out
}

// turnFiles lists the files to return for a finished turn: those the reply
// links (in order), then files created or modified during the turn. The
// request's own attachments are not echoed back.
func (s *Server) turnFiles(ctx context.Context, r *http.Request, agentID, projectID, sessionKey string, before workspaceSnapshot, reply string, attachments []string, inline bool) []turnFile {
	if s.workspace == nil {
		return nil
	}
	after := s.snapshotWorkspace(ctx, agentID, projectID, sessionKey)
	if len(after) == 0 {
		return nil
	}
	skip := make(map[string]bool, len(attachments))
	for _, a := range attachments {
		skip[filepath.ToSlash(a)] = true
	}
	var paths []string
	seen := map[string]bool{}
	add := func(p string) {
		if !seen[p] && !skip[p] {
			seen[p] = true
			paths = append(paths, p)
		}
	}
	for _, m := range workspaceRefPattern.FindAllStringSubmatch(reply, -1) {
		p := strings.TrimRight(path.Clean(m[1]), ".,;:!?")
		if p, err := url.PathUnescape(p); err == nil {
			if _, ok := after[p]; ok {
				add(p)
			}
		}
	}
	var changed []string
	for p, o := range after {
		prev, existed := before[p]
		if !existed || prev.Size != o.Size || !prev.ModTime.Equal(o.ModTime) {
			changed = append(changed, p)
		}
	}
	sort.Strings(changed)
	for _, p := range changed {
		add(p)
	}
	if len(paths) > maxTurnFiles {
		paths = paths[:maxTurnFiles]
	}

	base := requestBaseURL(r)
	inlineBudget := int64(maxInlineTotalBytes)
	out := make([]turnFile, 0, len(paths))
	for _, p := range paths {
		o := after[p]
		ct := o.ContentType
		if ct == "" || ct == "application/octet-stream" {
			if guess := mime.TypeByExtension(path.Ext(p)); guess != "" {
				ct = guess
			}
		}
		if ct == "" {
			ct = "application/octet-stream"
		}
		f := turnFile{
			Name:        path.Base(p),
			Path:        p,
			Size:        o.Size,
			ContentType: ct,
			URL:         fileURL(base, agentID, projectID, sessionKey, p),
		}
		if inline && o.Size >= 0 && o.Size <= maxInlineFileBytes && o.Size <= inlineBudget {
			if data := s.readWorkspaceFile(ctx, agentID, projectID, sessionKey, p); data != nil {
				f.DataURL = "data:" + ct + ";base64," + base64.StdEncoding.EncodeToString(data)
				inlineBudget -= int64(len(data))
			}
		}
		out = append(out, f)
	}
	return out
}

func (s *Server) readWorkspaceFile(ctx context.Context, agentID, projectID, sessionKey, p string) []byte {
	rc, err := s.workspace.Get(ctx, agentID, projectID, sessionKey, p)
	if err != nil {
		return nil
	}
	defer rc.Close()
	data, err := io.ReadAll(io.LimitReader(rc, maxInlineFileBytes+1))
	if err != nil || len(data) > maxInlineFileBytes {
		return nil
	}
	return data
}

// fileURL is the download URL for a conversation file.
func fileURL(base, agentID, projectID, sessionKey, p string) string {
	segs := strings.Split(p, "/")
	for i, seg := range segs {
		segs[i] = url.PathEscape(seg)
	}
	u := base + "/v1/agents/" + url.PathEscape(agentID) + "/sessions/" + url.PathEscape(sessionKey) +
		"/files/" + strings.Join(segs, "/")
	if projectID != "" {
		u += "?project_id=" + url.QueryEscape(projectID)
	}
	return u
}

// HandleGetSessionFile handles
// GET /v1/agents/{id}/sessions/{session}/files/{path...}: download a file
// from a conversation's workspace. {session} is the conversation's
// X-Fastclaw-Session-Key; send the same end-user (X-Fastclaw-End-User) as
// the conversation, since end-users can only read their own conversations.
// ?project_id= selects a project's shared folder instead.
func (s *Server) HandleGetSessionFile(w http.ResponseWriter, r *http.Request) {
	ident, ok := s.requireAgentAPI(w, r)
	if !ok {
		return
	}
	if s.workspace == nil {
		writeAPIError(w, http.StatusServiceUnavailable, errTypeServer, codeNotConfigured, "workspace storage not configured")
		return
	}
	rec, err := s.appAgent(r, r.PathValue("id"))
	if err != nil {
		writeAgentNotFound(w)
		return
	}
	sessionKey := r.PathValue("session")
	projectID := strings.TrimSpace(r.URL.Query().Get("project_id"))
	p := path.Clean("/" + r.PathValue("path"))[1:]
	notFound := func() {
		writeAPIError(w, http.StatusNotFound, errTypeNotFound, codeFileNotFound, "file not found")
	}
	if sessionKey == "" || p == "" || p == "." || strings.HasPrefix(p, "..") {
		notFound()
		return
	}
	// The conversation must belong to the caller's namespace — the
	// account, or the end-user named on this request.
	ns := ident.EffectiveUserID()
	ctx := r.Context()
	if projectID != "" {
		if proj, err := s.store.GetProject(ctx, ns, rec.ID, projectID); err != nil || proj == nil {
			notFound()
			return
		}
	} else if owned, err := s.store.HasChatSession(ctx, ns, rec.ID, sessionKey); err != nil || !owned {
		notFound()
		return
	}
	info, err := s.workspace.Stat(ctx, rec.ID, projectID, sessionKey, p)
	if err != nil || info == nil {
		notFound()
		return
	}
	rc, err := s.workspace.Get(ctx, rec.ID, projectID, sessionKey, p)
	if err != nil {
		notFound()
		return
	}
	defer rc.Close()
	ct := info.ContentType
	if ct == "" || ct == "application/octet-stream" {
		if guess := mime.TypeByExtension(path.Ext(p)); guess != "" {
			ct = guess
		}
	}
	if ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	w.Header().Set("Content-Disposition", mime.FormatMediaType("inline", map[string]string{"filename": path.Base(p)}))
	w.Header().Set("Cache-Control", "private, max-age=0")
	if !info.ModTime.IsZero() {
		w.Header().Set("Last-Modified", info.ModTime.UTC().Format(http.TimeFormat))
	}
	_, _ = io.Copy(w, rc)
}

// requestBaseURL reconstructs the externally visible origin of a request,
// honoring the usual reverse-proxy headers.
func requestBaseURL(r *http.Request) string {
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
