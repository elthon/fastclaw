package provider

import (
	"context"
	"net/http"
	"testing"
)

// Both providers must tag outbound calls with the FastClaw app identity so
// model platforms can attribute traffic back to us.
func TestBuildRequestSetsAppIdentityHeaders(t *testing.T) {
	msgs := []Message{{Role: "user", Content: "hi"}}

	anthropicReq, err := NewAnthropic("k", "https://example.com").
		buildRequest(context.Background(), msgs, nil, "claude-opus-5", 16, 0.7, true)
	if err != nil {
		t.Fatalf("anthropic buildRequest: %v", err)
	}
	openaiReq, err := NewOpenAI("k", "https://example.com/v1").
		buildRequest(context.Background(), msgs, nil, "gpt-4o-mini", 16, 0.7, true, openAIRequestMode{})
	if err != nil {
		t.Fatalf("openai buildRequest: %v", err)
	}

	want := map[string]string{
		"X-Title":      "FastClaw",
		"HTTP-Referer": "https://fastclaw.ai",
	}
	for name, req := range map[string]*http.Request{"anthropic": anthropicReq, "openai": openaiReq} {
		for k, v := range want {
			if got := req.Header.Get(k); got != v {
				t.Errorf("%s: header %s = %q, want %q", name, k, got, v)
			}
		}
	}
}
