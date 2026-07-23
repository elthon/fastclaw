package provider

import (
	"net/http"
	"testing"
	"time"
)

func TestLLMResponseHeaderTimeoutDefault(t *testing.T) {
	t.Setenv("FASTCLAW_LLM_RESPONSE_HEADER_TIMEOUT_SEC", "")

	if got := llmResponseHeaderTimeout(); got != 60*time.Second {
		t.Fatalf("llmResponseHeaderTimeout() = %v, want 60s", got)
	}
}

func TestLLMResponseHeaderTimeoutFromEnv(t *testing.T) {
	t.Setenv("FASTCLAW_LLM_RESPONSE_HEADER_TIMEOUT_SEC", "180")

	client := newLLMHTTPClient()
	transport, ok := client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("client.Transport = %T, want *http.Transport", client.Transport)
	}
	if got := transport.ResponseHeaderTimeout; got != 180*time.Second {
		t.Fatalf("ResponseHeaderTimeout = %v, want 180s", got)
	}
}

func TestLLMResponseHeaderTimeoutInvalidEnvFallsBack(t *testing.T) {
	for _, value := range []string{"abc", "0", "-1"} {
		t.Run(value, func(t *testing.T) {
			t.Setenv("FASTCLAW_LLM_RESPONSE_HEADER_TIMEOUT_SEC", value)

			if got := llmResponseHeaderTimeout(); got != 60*time.Second {
				t.Fatalf("llmResponseHeaderTimeout() = %v, want 60s", got)
			}
		})
	}
}
