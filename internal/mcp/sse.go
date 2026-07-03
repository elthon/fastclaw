package mcp

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// SSEClient implements the legacy MCP SSE transport. It opens a long-lived
// SSE stream, reads the server-provided message endpoint, then POSTs JSON-RPC
// requests to that endpoint and waits for matching responses on the stream.
type SSEClient struct {
	url      string
	headers  map[string]string
	client   *http.Client
	endpoint string

	mu      sync.Mutex
	nextID  int
	pending map[int]chan *jsonRPCResponse

	ready   chan error
	done    chan struct{}
	resp    *http.Response
	closeMu sync.Once
}

// NewSSEClient creates a new MCP client for legacy SSE servers.
func NewSSEClient(url string, headers map[string]string) *SSEClient {
	return &SSEClient{
		url:     url,
		headers: expandHeaders(headers),
		client:  &http.Client{},
		nextID:  1,
		pending: make(map[int]chan *jsonRPCResponse),
		ready:   make(chan error, 1),
		done:    make(chan struct{}),
	}
}

// Connect opens the SSE stream, waits for the endpoint event, and initializes
// the MCP session.
func (c *SSEClient) Connect() error {
	req, err := http.NewRequest("GET", c.url, nil)
	if err != nil {
		return fmt.Errorf("create SSE request: %w", err)
	}
	req.Header.Set("Accept", "text/event-stream")
	for k, v := range c.headers {
		req.Header.Set(k, v)
	}

	resp, err := c.client.Do(req)
	if err != nil {
		return fmt.Errorf("open SSE stream: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return fmt.Errorf("open SSE stream HTTP %d: %s", resp.StatusCode, string(body))
	}
	c.resp = resp
	go c.readLoop(resp.Body)

	select {
	case err := <-c.ready:
		if err != nil {
			_ = c.Close()
			return err
		}
	case <-time.After(15 * time.Second):
		_ = c.Close()
		return fmt.Errorf("SSE endpoint not received")
	}

	_, err = c.sendRequest("initialize", initializeParams{
		ProtocolVersion: "2024-11-05",
		ClientInfo:      clientInfo{Name: "fastclaw", Version: "0.1.0"},
	})
	return err
}

func (c *SSEClient) sendRequest(method string, params interface{}) (*jsonRPCResponse, error) {
	c.mu.Lock()
	id := c.nextID
	c.nextID++
	ch := make(chan *jsonRPCResponse, 1)
	c.pending[id] = ch
	endpoint := c.endpoint
	c.mu.Unlock()

	if endpoint == "" {
		c.forgetPending(id)
		return nil, fmt.Errorf("SSE endpoint not ready")
	}

	req := jsonRPCRequest{JSONRPC: "2.0", ID: id, Method: method, Params: params}
	body, err := json.Marshal(req)
	if err != nil {
		c.forgetPending(id)
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	httpReq, err := http.NewRequest("POST", endpoint, bytes.NewReader(body))
	if err != nil {
		c.forgetPending(id)
		return nil, fmt.Errorf("create POST request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	for k, v := range c.headers {
		httpReq.Header.Set(k, v)
	}

	resp, err := c.client.Do(httpReq)
	if err != nil {
		c.forgetPending(id)
		return nil, fmt.Errorf("send request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		respBody, _ := io.ReadAll(resp.Body)
		c.forgetPending(id)
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(respBody))
	}

	select {
	case rpcResp := <-ch:
		if rpcResp.Error != nil {
			return nil, fmt.Errorf("RPC error %d: %s", rpcResp.Error.Code, rpcResp.Error.Message)
		}
		return rpcResp, nil
	case <-c.done:
		return nil, fmt.Errorf("SSE stream closed before response")
	case <-time.After(60 * time.Second):
		c.forgetPending(id)
		return nil, fmt.Errorf("timeout waiting for SSE response")
	}
}

func (c *SSEClient) forgetPending(id int) {
	c.mu.Lock()
	delete(c.pending, id)
	c.mu.Unlock()
}

func (c *SSEClient) readLoop(r io.Reader) {
	defer close(c.done)
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 1024*1024), 1024*1024)
	eventType := "message"
	var dataLines []string
	endpointSent := false

	dispatch := func() {
		if len(dataLines) == 0 {
			eventType = "message"
			return
		}
		data := strings.Join(dataLines, "\n")
		switch eventType {
		case "endpoint":
			endpoint, err := resolveEndpoint(c.url, data)
			if err != nil {
				select {
				case c.ready <- err:
				default:
				}
			} else {
				c.mu.Lock()
				c.endpoint = endpoint
				c.mu.Unlock()
				select {
				case c.ready <- nil:
				default:
				}
			}
			endpointSent = true
		case "message", "":
			var rpcResp jsonRPCResponse
			if err := json.Unmarshal([]byte(data), &rpcResp); err == nil && rpcResp.ID != 0 {
				c.mu.Lock()
				ch := c.pending[rpcResp.ID]
				delete(c.pending, rpcResp.ID)
				c.mu.Unlock()
				if ch != nil {
					ch <- &rpcResp
				}
			}
		}
		eventType = "message"
		dataLines = nil
	}

	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			dispatch()
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue
		}
		field, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		value = strings.TrimPrefix(value, " ")
		switch field {
		case "event":
			eventType = value
		case "data":
			dataLines = append(dataLines, value)
		}
	}
	dispatch()
	if !endpointSent {
		select {
		case c.ready <- fmt.Errorf("SSE stream ended before endpoint event"):
		default:
		}
	}
}

func resolveEndpoint(baseURL, endpoint string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(endpoint))
	if err != nil {
		return "", fmt.Errorf("parse SSE endpoint: %w", err)
	}
	if u.IsAbs() {
		return u.String(), nil
	}
	base, err := url.Parse(baseURL)
	if err != nil {
		return "", fmt.Errorf("parse SSE URL: %w", err)
	}
	return base.ResolveReference(u).String(), nil
}

// ListTools returns the list of tools available on the MCP server.
func (c *SSEClient) ListTools() ([]ToolDef, error) {
	resp, err := c.sendRequest("tools/list", struct{}{})
	if err != nil {
		return nil, err
	}
	var result toolsListResult
	if err := json.Unmarshal(resp.Result, &result); err != nil {
		return nil, fmt.Errorf("parse tools list: %w", err)
	}
	return result.Tools, nil
}

// CallTool calls a tool on the MCP server.
func (c *SSEClient) CallTool(name string, args json.RawMessage) (string, error) {
	resp, err := c.sendRequest("tools/call", toolCallParams{Name: name, Arguments: args})
	if err != nil {
		return "", err
	}
	var result toolCallResult
	if err := json.Unmarshal(resp.Result, &result); err != nil {
		return "", fmt.Errorf("parse tool result: %w", err)
	}
	var texts []string
	for _, ct := range result.Content {
		if ct.Type == "text" {
			texts = append(texts, ct.Text)
		}
	}
	return strings.Join(texts, "\n"), nil
}

// Close closes the SSE response body.
func (c *SSEClient) Close() error {
	c.closeMu.Do(func() {
		if c.resp != nil && c.resp.Body != nil {
			_ = c.resp.Body.Close()
		}
	})
	return nil
}
