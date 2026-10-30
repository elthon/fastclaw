package api

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestACPAgentName(t *testing.T) {
	tests := []struct {
		id       string
		wantSame bool
	}{
		{id: "researcher", wantSame: true},
		{id: "agt_123", wantSame: false},
		{id: "My Agent", wantSame: false},
		{id: strings.Repeat("a", 80), wantSame: false},
	}
	for _, tt := range tests {
		got := acpAgentName(tt.id)
		if !acpAgentNamePattern.MatchString(got) || len(got) > 63 {
			t.Fatalf("acpAgentName(%q) = %q, not a valid ACP agent name", tt.id, got)
		}
		if (got == tt.id) != tt.wantSame {
			t.Fatalf("acpAgentName(%q) = %q, wantSame=%v", tt.id, got, tt.wantSame)
		}
		if got != acpAgentName(tt.id) {
			t.Fatalf("acpAgentName(%q) is not deterministic", tt.id)
		}
	}
	if acpAgentName("agt_one") == acpAgentName("agt-one") {
		t.Fatal("normalized and already-valid agent IDs must not collide")
	}
}

func TestPrepareACPInput(t *testing.T) {
	image := base64.StdEncoding.EncodeToString([]byte("image bytes"))
	got, err := prepareACPInput([]acpMessage{
		{
			Role: "agent/planner",
			Parts: []acpMessagePart{
				{ContentType: "text/plain", Content: "Use this plan"},
				{ContentType: "image/png", Content: image, ContentEncoding: "base64", Name: "diagram.png"},
			},
		},
		{
			Role: "user",
			Parts: []acpMessagePart{
				{ContentType: "text/markdown", Content: "Please implement it"},
				{ContentType: "application/pdf", ContentURL: "https://example.com/spec.pdf", Name: "spec.pdf"},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got.text, "[Message from agent/planner]") || !strings.Contains(got.text, "Please implement it") {
		t.Fatalf("prepared text lost message context: %q", got.text)
	}
	if len(got.images) != 1 || !strings.HasPrefix(got.images[0], "data:image/png;base64,") {
		t.Fatalf("images = %#v", got.images)
	}
	if len(got.attachments) != 2 || got.attachments[1].Name != "spec.pdf" {
		t.Fatalf("attachments = %#v", got.attachments)
	}
}

func TestPrepareACPInputRejectsInvalidParts(t *testing.T) {
	_, err := prepareACPInput([]acpMessage{{
		Role: "user",
		Parts: []acpMessagePart{{
			ContentType: "text/plain",
			Content:     "inline",
			ContentURL:  "https://example.com/text",
		}},
	}})
	if err == nil {
		t.Fatal("expected content/content_url conflict")
	}

	_, err = prepareACPInput([]acpMessage{{
		Role:  "system",
		Parts: []acpMessagePart{{ContentType: "text/plain", Content: "nope"}},
	}})
	if err == nil {
		t.Fatal("expected invalid role error")
	}
}

func TestACPSessionID(t *testing.T) {
	generated, err := acpSessionID(acpRunCreateRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := uuid.Parse(generated); err != nil {
		t.Fatalf("generated session ID %q is not a UUID", generated)
	}

	id := uuid.NewString()
	got, err := acpSessionID(acpRunCreateRequest{SessionID: id, Session: &acpSession{ID: id}})
	if err != nil || got != id {
		t.Fatalf("acpSessionID = %q, %v", got, err)
	}
	if _, err := acpSessionID(acpRunCreateRequest{SessionID: id, Session: &acpSession{ID: uuid.NewString()}}); err == nil {
		t.Fatal("expected mismatched session IDs to fail")
	}
}

func TestACPPingDoesNotRequireAuthentication(t *testing.T) {
	s := NewServer(nil, nil, nil)
	mux := http.NewServeMux()
	s.RegisterRoutes(mux)
	req := httptest.NewRequest(http.MethodGet, "/acp/ping", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"protocol":"acp/0.2"`) {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestACPRunCancellation(t *testing.T) {
	state := &acpRunState{run: acpRun{Status: acpStatusCreated}}
	ctx, cancel := context.WithCancel(context.Background())
	state.setCancel(cancel)
	if !state.requestCancel() {
		t.Fatal("active run cancellation was rejected")
	}
	if state.snapshot().Status != acpStatusCancelling {
		t.Fatalf("status = %q", state.snapshot().Status)
	}
	if ctx.Err() == nil {
		t.Fatal("run context was not cancelled")
	}
	state.finish(acpStatusCancelled, nil)
	if state.requestCancel() {
		t.Fatal("terminal run cancellation was accepted")
	}
	if state.snapshot().Status != acpStatusCancelled {
		t.Fatalf("terminal status changed to %q", state.snapshot().Status)
	}
}
