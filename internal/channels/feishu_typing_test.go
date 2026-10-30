package channels

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestFeishuMessageTypingAddsAndRemovesReaction(t *testing.T) {
	deleted := make(chan string, 1)
	var emoji string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/open-apis/im/v1/messages/om_1/reactions":
			var body struct {
				ReactionType struct {
					EmojiType string `json:"emoji_type"`
				} `json:"reaction_type"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			emoji = body.ReactionType.EmojiType
			_, _ = w.Write([]byte(`{"code":0,"data":{"reaction_id":"r_1"}}`))
		case r.Method == http.MethodDelete && r.URL.Path == "/open-apis/im/v1/messages/om_1/reactions/r_1":
			_, _ = w.Write([]byte(`{"code":0}`))
			deleted <- "r_1"
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	l := &Feishu{
		accountID:    "app_1",
		httpClient:   server.Client(),
		apiBaseURL:   server.URL,
		accessTok:    "token",
		accessTokExp: time.Now().Add(time.Hour),
	}
	stop := l.StartMessageTyping("chat_1", "om_1")
	stop()
	stop() // idempotent

	select {
	case <-deleted:
	case <-time.After(3 * time.Second):
		t.Fatal("typing reaction was not removed")
	}
	if emoji != feishuTypingEmoji {
		t.Fatalf("emoji_type = %q", emoji)
	}
}

func TestFeishuMessageTypingSkipsSyntheticIDs(t *testing.T) {
	l := &Feishu{apiBaseURL: "http://127.0.0.1:1"}
	l.StartMessageTyping("chat_1", "1712345678")() // must not panic or call out
}
