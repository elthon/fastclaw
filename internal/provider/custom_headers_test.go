package provider

import (
	"context"
	"net/http"
	"testing"
)

func TestOpenAICustomHeadersApplied(t *testing.T) {
	p := NewOpenAIWithHeaders("test-key", "https://example.com/v1", map[string]string{
		"X-No-Filter":   "true",
		"Authorization": "Bearer gateway-key",
	})
	req, err := p.buildRequest(context.Background(), []Message{{Role: "user", Content: "hi"}}, nil, "test-model", 16, 0, true, initialOpenAIRequestMode("test-model"))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	if got := req.Header.Get("X-No-Filter"); got != "true" {
		t.Fatalf("X-No-Filter = %q, want true", got)
	}
	if got := req.Header.Get("Authorization"); got != "Bearer gateway-key" {
		t.Fatalf("Authorization = %q, want custom override", got)
	}
}

func TestAnthropicCustomHeadersApplied(t *testing.T) {
	p := NewAnthropicWithHeaders("test-key", "https://example.com", map[string]string{
		"X-Provider-Feature": "enabled",
	})
	req, err := p.buildRequest(context.Background(), []Message{{Role: "user", Content: "hi"}}, nil, "test-model", 16, 0, true)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	if got := req.Header.Get("X-Provider-Feature"); got != "enabled" {
		t.Fatalf("X-Provider-Feature = %q, want enabled", got)
	}
	if got := req.Header.Get("x-api-key"); got != "test-key" {
		t.Fatalf("x-api-key = %q, want test-key", got)
	}
}

func TestApplyCustomHeadersSkipsUnsafeValues(t *testing.T) {
	h := http.Header{}
	ApplyCustomHeaders(h, map[string]string{
		"X-Good":      " ok ",
		"Bad Header":  "value",
		"X-Bad-Value": "a\nb",
	})
	if got := h.Get("X-Good"); got != "ok" {
		t.Fatalf("X-Good = %q, want ok", got)
	}
	if got := h.Get("Bad Header"); got != "" {
		t.Fatalf("Bad Header = %q, want empty", got)
	}
	if got := h.Get("X-Bad-Value"); got != "" {
		t.Fatalf("X-Bad-Value = %q, want empty", got)
	}
}
