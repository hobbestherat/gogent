package mcp

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// httpTransport speaks MCP over streamable-HTTP: each JSON-RPC message is POSTed
// to a single endpoint. A server may answer either with a plain application/json
// body or with a text/event-stream (SSE) carrying the response, so both are
// handled. The Mcp-Session-Id header returned by initialize is echoed on
// subsequent requests.
type httpTransport struct {
	url     string
	client  *http.Client
	headers map[string]string

	mu      sync.Mutex
	id      int64
	session string
}

func newHTTPTransport(url string, headers map[string]string) *httpTransport {
	return &httpTransport{
		url:     url,
		headers: headers,
		client:  &http.Client{Timeout: 60 * time.Second},
	}
}

func (t *httpTransport) post(body []byte) (*http.Response, error) {
	req, err := http.NewRequest(http.MethodPost, t.url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("mcp http: new request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	t.mu.Lock()
	sess := t.session
	t.mu.Unlock()
	if sess != "" {
		req.Header.Set("Mcp-Session-Id", sess)
	}
	for k, v := range t.headers {
		req.Header.Set(k, v)
	}
	resp, err := t.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("mcp http: do request: %w", err)
	}
	return resp, nil
}

func (t *httpTransport) call(method string, params interface{}) (json.RawMessage, error) {
	t.mu.Lock()
	t.id++
	id := t.id
	t.mu.Unlock()

	body, err := json.Marshal(rpcRequest{JSONRPC: "2.0", ID: id, Method: method, Params: params})
	if err != nil {
		return nil, fmt.Errorf("mcp http: marshal request: %w", err)
	}
	resp, err := t.post(body)
	if err != nil {
		return nil, fmt.Errorf("mcp http: %s: %w", method, err)
	}
	defer func() { _ = resp.Body.Close() }()

	// Capture the session id assigned by initialize so later calls present it.
	if s := resp.Header.Get("Mcp-Session-Id"); s != "" {
		t.mu.Lock()
		t.session = s
		t.mu.Unlock()
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, fmt.Errorf("mcp http: %s: %s: %s", method, resp.Status, bytes.TrimSpace(snippet))
	}

	if strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		return readEventStream(resp.Body, id)
	}
	return decodeResponse(resp.Body, id)
}

func (t *httpTransport) notify(method string, params interface{}) error {
	body, err := json.Marshal(rpcRequest{JSONRPC: "2.0", Method: method, Params: params})
	if err != nil {
		return fmt.Errorf("mcp http: marshal request: %w", err)
	}
	resp, err := t.post(body)
	if err != nil {
		return fmt.Errorf("mcp http: notify %s: %w", method, err)
	}
	// A notification has no response body to consume; drain and close so the
	// connection can be reused.
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	_ = resp.Body.Close()
	return nil
}

func (t *httpTransport) close() error { return nil }

// decodeResponse reads a single JSON-RPC response from a plain JSON body.
func decodeResponse(r io.Reader, id int64) (json.RawMessage, error) {
	var resp rpcResponse
	if err := json.NewDecoder(r).Decode(&resp); err != nil {
		return nil, fmt.Errorf("mcp http: decode response: %w", err)
	}
	if resp.Error != nil {
		return nil, resp.Error
	}
	return resp.Result, nil
}

// readEventStream scans an SSE body for the JSON-RPC response carrying the given
// id. Each event's data field (SSE allows it to span multiple data: lines) is
// reassembled and parsed; unrelated events (server notifications, keep-alives)
// are skipped.
func readEventStream(r io.Reader, id int64) (json.RawMessage, error) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)

	var data strings.Builder
	dispatch := func() (json.RawMessage, bool, error) {
		if data.Len() == 0 {
			return nil, false, nil
		}
		payload := data.String()
		data.Reset()
		var resp rpcResponse
		if err := json.Unmarshal([]byte(payload), &resp); err != nil {
			return nil, false, nil // not a JSON-RPC message; skip the event
		}
		if resp.ID != id {
			return nil, false, nil
		}
		if resp.Error != nil {
			return nil, false, resp.Error
		}
		return resp.Result, true, nil
	}

	for scanner.Scan() {
		line := scanner.Text()
		if line == "" { // blank line terminates an event
			if res, ok, err := dispatch(); err != nil || ok {
				return res, err
			}
			continue
		}
		if strings.HasPrefix(line, "data:") {
			data.WriteString(strings.TrimSpace(line[len("data:"):]))
		}
		// Other SSE fields (event:, id:, retry:, comments) are ignored.
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("mcp http: read event stream: %w", err)
	}
	// A final event may end at EOF without a trailing blank line.
	if res, ok, err := dispatch(); err != nil || ok {
		return res, err
	}
	return nil, fmt.Errorf("mcp http: no response for id %d in event stream", id)
}
