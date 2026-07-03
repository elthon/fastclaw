package mcp

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSSEClientListToolsAndCallTool(t *testing.T) {
	requests := make(chan jsonRPCRequest, 4)
	responses := make(chan string, 4)

	mux := http.NewServeMux()
	mux.HandleFunc("/sse", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Fatal("response writer is not a flusher")
		}
		fmt.Fprintf(w, "event: endpoint\ndata: /messages\n\n")
		flusher.Flush()
		for msg := range responses {
			fmt.Fprintf(w, "event: message\ndata: %s\n\n", msg)
			flusher.Flush()
		}
	})
	mux.HandleFunc("/messages", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Fatalf("method = %s, want POST", r.Method)
		}
		var req jsonRPCRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		requests <- req
		w.WriteHeader(http.StatusAccepted)
	})

	srv := httptest.NewServer(mux)
	defer srv.Close()
	defer close(responses)

	done := make(chan error, 1)
	go func() {
		client := NewSSEClient(srv.URL+"/sse", nil)
		if err := client.Connect(); err != nil {
			done <- err
			return
		}
		tools, err := client.ListTools()
		if err != nil {
			done <- err
			return
		}
		if len(tools) != 1 || tools[0].Name != "search_url" {
			done <- fmt.Errorf("tools = %+v", tools)
			return
		}
		out, err := client.CallTool("search_url", json.RawMessage(`{"url":"https://example.com"}`))
		if err != nil {
			done <- err
			return
		}
		if out != "ok" {
			done <- fmt.Errorf("tool result = %q", out)
			return
		}
		_ = client.Close()
		done <- nil
	}()

	for i := 0; i < 3; i++ {
		req := <-requests
		var result string
		switch req.Method {
		case "initialize":
			result = `{}`
		case "tools/list":
			result = `{"tools":[{"name":"search_url","description":"Search URL","inputSchema":{"type":"object"}}]}`
		case "tools/call":
			params, _ := json.Marshal(req.Params)
			if !strings.Contains(string(params), "example.com") {
				t.Fatalf("tool params = %s", params)
			}
			result = `{"content":[{"type":"text","text":"ok"}]}`
		default:
			t.Fatalf("unexpected method %q", req.Method)
		}
		responses <- fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"result":%s}`, req.ID, result)
	}

	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
