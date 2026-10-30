package mcp

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type testSSESession struct {
	messages chan string
	stop     chan struct{}
	once     sync.Once
}

func (s *testSSESession) close() { s.once.Do(func() { close(s.stop) }) }

type testSSEServer struct {
	mu           sync.Mutex
	sessions     []*testSSESession
	toolCalls    atomic.Int32
	closeOnCall  atomic.Bool
	responseText string
}

func (s *testSSEServer) serve(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/sse":
		sess := &testSSESession{messages: make(chan string, 20), stop: make(chan struct{})}
		s.mu.Lock()
		s.sessions = append(s.sessions, sess)
		id := len(s.sessions) - 1
		s.mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "event: endpoint\ndata: /messages?session=%d\n\n", id)
		w.(http.Flusher).Flush()
		for {
			select {
			case msg := <-sess.messages:
				fmt.Fprintf(w, "event: message\ndata: %s\n\n", msg)
				w.(http.Flusher).Flush()
			case <-sess.stop:
				return
			case <-r.Context().Done():
				return
			}
		}
	case "/messages":
		id, err := strconv.Atoi(r.URL.Query().Get("session"))
		if err != nil {
			http.Error(w, "bad session", 400)
			return
		}
		s.mu.Lock()
		sess := s.sessions[id]
		s.mu.Unlock()
		var req jsonRPCRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad request", 400)
			return
		}
		w.WriteHeader(http.StatusAccepted)
		result := any(map[string]any{})
		if req.Method == "tools/call" {
			s.toolCalls.Add(1)
			if s.closeOnCall.Swap(false) {
				sess.close()
				return
			}
			s.mu.Lock()
			text := s.responseText
			s.mu.Unlock()
			result = map[string]any{"content": []any{map[string]string{"type": "text", "text": text}}}
		}
		payload, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result})
		select {
		case sess.messages <- string(payload):
		case <-sess.stop:
		case <-r.Context().Done():
		}
	default:
		http.NotFound(w, r)
	}
}
func (s *testSSEServer) latest() *testSSESession {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sessions[len(s.sessions)-1]
}
func (s *testSSEServer) count() int { s.mu.Lock(); defer s.mu.Unlock(); return len(s.sessions) }
func waitClosed(t *testing.T, c *SSEClient) {
	t.Helper()
	deadline := time.After(3 * time.Second)
	for {
		c.mu.Lock()
		sess := c.session
		c.mu.Unlock()
		select {
		case <-sess.done:
			return
		case <-deadline:
			t.Fatal("stream did not close")
		default:
			time.Sleep(time.Millisecond)
		}
	}
}
func TestSSELargeResponse(t *testing.T) {
	service := &testSSEServer{responseText: strings.Repeat("x", 2*1024*1024)}
	srv := httptest.NewServer(http.HandlerFunc(service.serve))
	defer srv.Close()
	c := NewSSEClient(srv.URL+"/sse", nil)
	defer c.Close()
	if err := c.Connect(); err != nil {
		t.Fatal(err)
	}
	out, err := c.CallTool("search", json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 2*1024*1024 {
		t.Fatalf("got %d bytes", len(out))
	}
}
func TestSSEReconnectAfterIdleDisconnect(t *testing.T) {
	service := &testSSEServer{responseText: "ok"}
	srv := httptest.NewServer(http.HandlerFunc(service.serve))
	defer srv.Close()
	c := NewSSEClient(srv.URL+"/sse", nil)
	defer c.Close()
	if err := c.Connect(); err != nil {
		t.Fatal(err)
	}
	service.latest().close()
	waitClosed(t, c)
	const n = 8
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		go func() {
			out, err := c.CallTool("search", json.RawMessage(`{}`))
			if err == nil && out != "ok" {
				err = fmt.Errorf("unexpected output %q", out)
			}
			errs <- err
		}()
	}
	for i := 0; i < n; i++ {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	if got := service.count(); got != 2 {
		t.Fatalf("sessions = %d, want 2", got)
	}
	if got := service.toolCalls.Load(); got != n {
		t.Fatalf("tool calls = %d, want %d", got, n)
	}
}
func TestSSEDisconnectDoesNotReplayTool(t *testing.T) {
	service := &testSSEServer{responseText: "ok"}
	service.closeOnCall.Store(true)
	srv := httptest.NewServer(http.HandlerFunc(service.serve))
	defer srv.Close()
	c := NewSSEClient(srv.URL+"/sse", nil)
	defer c.Close()
	if err := c.Connect(); err != nil {
		t.Fatal(err)
	}
	if _, err := c.CallTool("write", json.RawMessage(`{}`)); err == nil {
		t.Fatal("expected error after disconnect")
	}
	if got := service.toolCalls.Load(); got != 1 {
		t.Fatalf("first call executed %d times", got)
	}
	if out, err := c.CallTool("search", json.RawMessage(`{}`)); err != nil || out != "ok" {
		t.Fatalf("next call: %q, %v", out, err)
	}
	if got := service.toolCalls.Load(); got != 2 {
		t.Fatalf("total calls = %d, want 2", got)
	}
}
func TestSSEClosedClientDoesNotReconnect(t *testing.T) {
	service := &testSSEServer{responseText: "ok"}
	srv := httptest.NewServer(http.HandlerFunc(service.serve))
	defer srv.Close()
	c := NewSSEClient(srv.URL+"/sse", nil)
	if err := c.Connect(); err != nil {
		t.Fatal(err)
	}
	c.Close()
	if _, err := c.CallTool("search", json.RawMessage(`{}`)); err == nil {
		t.Fatal("expected error after close")
	}
	if got := service.count(); got != 1 {
		t.Fatalf("sessions = %d, want 1", got)
	}
}

func TestSSEOversizedResponseReportsErrorAndReconnects(t *testing.T) {
	service := &testSSEServer{responseText: strings.Repeat("x", 3*1024*1024)}
	srv := httptest.NewServer(http.HandlerFunc(service.serve))
	defer srv.Close()
	c := NewSSEClient(srv.URL+"/sse", nil)
	defer c.Close()
	if err := c.Connect(); err != nil {
		t.Fatal(err)
	}
	if _, err := c.CallTool("write", json.RawMessage(`{}`)); err == nil || !strings.Contains(err.Error(), "token too long") {
		t.Fatalf("expected size error, got %v", err)
	}
	if got := service.toolCalls.Load(); got != 1 {
		t.Fatalf("first call executed %d times", got)
	}
	service.mu.Lock()
	service.responseText = "ok"
	service.mu.Unlock()
	if out, err := c.CallTool("search", json.RawMessage(`{}`)); err != nil || out != "ok" {
		t.Fatalf("next call: %q, %v", out, err)
	}
	if got := service.toolCalls.Load(); got != 2 {
		t.Fatalf("total calls = %d, want 2", got)
	}
}
