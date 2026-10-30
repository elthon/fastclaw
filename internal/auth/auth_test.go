package auth

import "testing"

func TestSessionCookieNameScopedByPort(t *testing.T) {
	for port, want := range map[string]string{
		"":      "fastclaw_session",
		"18953": "fastclaw_session",
		"18955": "fastclaw_session_18955",
	} {
		if got := sessionCookieName(port); got != want {
			t.Errorf("sessionCookieName(%q) = %q, want %q", port, got, want)
		}
	}
}
