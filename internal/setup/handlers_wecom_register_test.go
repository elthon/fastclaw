package setup

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/fastclaw-ai/fastclaw/internal/store"
)

func TestWeComRegisterQRFlow(t *testing.T) {
	ctx := context.Background()
	s, resolver, _, owner := newAuthTestServer(t, ctx)
	agent := &store.AgentRecord{ID: "agt_wecom_qr", UserID: owner.ID, Name: "WeCom QR"}
	if err := s.dataStore.SaveAgent(ctx, agent); err != nil {
		t.Fatalf("SaveAgent: %v", err)
	}

	// Fake work.weixin.qq.com: the first query is still "init", then the
	// scan succeeds and returns the minted bot.
	var queries atomic.Int32
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/generate":
			if r.URL.Query().Get("source") != wecomQRSource {
				t.Errorf("source = %q", r.URL.Query().Get("source"))
			}
			_, _ = w.Write([]byte(`{"data":{"scode":"SC1","auth_url":"https://work.weixin.qq.com/ai/qc/c?s=SC1"}}`))
		case "/query_result":
			if r.URL.Query().Get("scode") != "SC1" {
				t.Errorf("scode = %q", r.URL.Query().Get("scode"))
			}
			if queries.Add(1) == 1 {
				_, _ = w.Write([]byte(`{"data":{"status":"init"}}`))
				return
			}
			_, _ = w.Write([]byte(`{"data":{"status":"success","bot_info":{"botid":"aibQR","secret":"sec"}}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer fake.Close()
	prev := wecomQRBase
	wecomQRBase = fake.URL
	defer func() { wecomQRBase = prev }()

	call := func(h http.HandlerFunc, method, path string) map[string]any {
		t.Helper()
		req := authTestRequest(t, ctx, resolver, method, path, owner.ID)
		req.SetPathValue("id", agent.ID)
		rr := httptest.NewRecorder()
		s.authMiddleware(h)(rr, req)
		var out map[string]any
		if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
			t.Fatalf("%s %s: status %d body %q", method, path, rr.Code, rr.Body.String())
		}
		return out
	}

	start := call(s.handleStartAgentWeComRegister, http.MethodPost, "/api/agents/"+agent.ID+"/channels/wecom/register")
	sessionID, _ := start["sessionId"].(string)
	if sessionID == "" || start["qrUrl"] != "https://work.weixin.qq.com/ai/qc/c?s=SC1" {
		t.Fatalf("start = %v", start)
	}
	statusPath := "/api/agents/" + agent.ID + "/channels/wecom/register/status?session=" + sessionID

	if got := call(s.handleAgentWeComRegisterStatus, http.MethodGet, statusPath); got["status"] != "pending" {
		t.Fatalf("first poll = %v", got)
	}
	if got := call(s.handleAgentWeComRegisterStatus, http.MethodGet, statusPath); got["status"] != "confirmed" || got["botId"] != "aibQR" {
		t.Fatalf("second poll = %v", got)
	}

	ch, err := s.dataStore.LookupChannel(ctx, "wecom", "aibQR")
	if err != nil || ch == nil {
		t.Fatalf("LookupChannel: %v %v", ch, err)
	}
	if ch.AgentID != agent.ID {
		t.Fatalf("channel bound to %q, want %q", ch.AgentID, agent.ID)
	}
	accts, _ := ch.Data["accounts"].(map[string]any)
	acct, _ := accts["aibQR"].(map[string]any)
	if acct["botToken"] != "sec" {
		t.Fatalf("stored account = %v", ch.Data)
	}

	// The session is single-use once the bot is persisted.
	if got := call(s.handleAgentWeComRegisterStatus, http.MethodGet, statusPath); got["error"] == nil {
		t.Fatalf("reused session = %v", got)
	}
}
