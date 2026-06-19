package mcp

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeServer plays an MCP server over an io.ReadWriter (one connection). It reads
// newline-delimited JSON-RPC requests and answers initialize/tools/list/tools/call
// from a fixed tool set, recording the calls it saw for assertions.
type fakeServer struct {
	tools     []Tool
	callReply *CallResult
	rpcErr    *rpcError // when set, tools/call returns this protocol error

	gotInitialize bool
	gotInitNotif  bool
	lastCallName  string
	lastCallArgs  map[string]interface{}
}

func (s *fakeServer) serve(rw io.ReadWriter) {
	r := bufio.NewReader(rw)
	for {
		line, err := r.ReadBytes('\n')
		if len(line) == 0 && err != nil {
			return
		}
		line = []byte(strings.TrimSpace(string(line)))
		if len(line) == 0 {
			if err != nil {
				return
			}
			continue
		}
		var req struct {
			ID     int64                  `json:"id"`
			Method string                 `json:"method"`
			Params map[string]interface{} `json:"params"`
		}
		if jerr := json.Unmarshal(line, &req); jerr != nil {
			continue
		}
		result, isNotification := s.handle(&req)
		if isNotification {
			if err != nil {
				return
			}
			continue
		}
		resp := map[string]interface{}{"jsonrpc": "2.0", "id": req.ID}
		if s.rpcErr != nil && req.Method == "tools/call" {
			resp["error"] = map[string]interface{}{"code": s.rpcErr.Code, "message": s.rpcErr.Message}
		} else {
			resp["result"] = result
		}
		b, _ := json.Marshal(resp)
		rw.Write(append(b, '\n'))
		if err != nil {
			return
		}
	}
}

func (s *fakeServer) handle(req *struct {
	ID     int64                  `json:"id"`
	Method string                 `json:"method"`
	Params map[string]interface{} `json:"params"`
}) (interface{}, bool) {
	switch req.Method {
	case "initialize":
		s.gotInitialize = true
		return map[string]interface{}{
			"protocolVersion": protocolVersion,
			"capabilities":    map[string]interface{}{"tools": map[string]interface{}{}},
			"serverInfo":      map[string]interface{}{"name": "fake", "version": "1.0"},
		}, false
	case "notifications/initialized":
		s.gotInitNotif = true
		return nil, true
	case "tools/list":
		return map[string]interface{}{"tools": s.tools}, false
	case "tools/call":
		s.lastCallName, _ = req.Params["name"].(string)
		s.lastCallArgs, _ = req.Params["arguments"].(map[string]interface{})
		reply := s.callReply
		if reply == nil {
			reply = &CallResult{Content: []Content{{Type: "text", Text: "ok"}}}
		}
		return reply, false
	}
	return nil, false
}

// dialStream wires a Client to an in-memory fakeServer over net.Pipe, exercising
// the stream (stdio) transport's framing without launching a subprocess.
func dialStream(t *testing.T, srv *fakeServer) *Client {
	t.Helper()
	clientConn, serverConn := net.Pipe()
	go srv.serve(serverConn)
	tr := newStreamTransport(clientConn, clientConn, clientConn.Close)
	c := &Client{t: tr, name: "fake"}
	if err := c.initialize(); err != nil {
		t.Fatalf("initialize: %v", err)
	}
	return c
}

func TestClientStreamHandshakeAndTools(t *testing.T) {
	srv := &fakeServer{
		tools: []Tool{
			{Name: "echo", Description: "Echo text", InputSchema: map[string]interface{}{"type": "object"}},
			{Name: "add", Description: "Add numbers"},
		},
		callReply: &CallResult{Content: []Content{{Type: "text", Text: "hello"}, {Type: "text", Text: "world"}}},
	}
	c := dialStream(t, srv)
	defer c.Close()

	if !srv.gotInitialize || !srv.gotInitNotif {
		t.Fatalf("handshake incomplete: initialize=%v initialized=%v", srv.gotInitialize, srv.gotInitNotif)
	}

	tools, err := c.ListTools()
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if len(tools) != 2 || tools[0].Name != "echo" || tools[1].Name != "add" {
		t.Fatalf("unexpected tools: %+v", tools)
	}

	res, err := c.CallTool("echo", map[string]interface{}{"text": "hi"})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if srv.lastCallName != "echo" {
		t.Fatalf("server saw call to %q", srv.lastCallName)
	}
	if got := srv.lastCallArgs["text"]; got != "hi" {
		t.Fatalf("server saw args %v", srv.lastCallArgs)
	}
	if got := res.Text(); got != "hello\nworld" {
		t.Fatalf("Text() = %q", got)
	}
}

// TestCallToolNilArgsSendsEmptyObject verifies CallTool substitutes an empty
// arguments object so servers that require the field do not see a null.
func TestCallToolNilArgsSendsEmptyObject(t *testing.T) {
	srv := &fakeServer{}
	c := dialStream(t, srv)
	defer c.Close()

	if _, err := c.CallTool("noargs", nil); err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if srv.lastCallArgs == nil {
		t.Fatal("expected an empty arguments object, got nil")
	}
}

// TestCallToolProtocolError surfaces a JSON-RPC error from tools/call as a Go
// error rather than a result.
func TestCallToolProtocolError(t *testing.T) {
	srv := &fakeServer{rpcErr: &rpcError{Code: -32000, Message: "boom"}}
	c := dialStream(t, srv)
	defer c.Close()

	_, err := c.CallTool("x", nil)
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("expected rpc error, got %v", err)
	}
}

// TestListToolsPaginates follows the nextCursor across pages.
func TestListToolsPaginates(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	// A bespoke server: first tools/list returns a cursor, the second does not.
	go func() {
		r := bufio.NewReader(serverConn)
		page := 0
		for {
			line, err := r.ReadBytes('\n')
			if len(line) == 0 && err != nil {
				return
			}
			var req struct {
				ID     int64  `json:"id"`
				Method string `json:"method"`
			}
			if json.Unmarshal([]byte(strings.TrimSpace(string(line))), &req) != nil {
				continue
			}
			switch req.Method {
			case "notifications/initialized":
				continue
			case "tools/list":
				var result map[string]interface{}
				if page == 0 {
					result = map[string]interface{}{"tools": []Tool{{Name: "a"}}, "nextCursor": "c1"}
				} else {
					result = map[string]interface{}{"tools": []Tool{{Name: "b"}}}
				}
				page++
				b, _ := json.Marshal(map[string]interface{}{"jsonrpc": "2.0", "id": req.ID, "result": result})
				serverConn.Write(append(b, '\n'))
			default:
				b, _ := json.Marshal(map[string]interface{}{"jsonrpc": "2.0", "id": req.ID, "result": map[string]interface{}{}})
				serverConn.Write(append(b, '\n'))
			}
		}
	}()
	tr := newStreamTransport(clientConn, clientConn, clientConn.Close)
	c := &Client{t: tr, name: "fake"}
	if err := c.initialize(); err != nil {
		t.Fatalf("initialize: %v", err)
	}
	tools, err := c.ListTools()
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if len(tools) != 2 || tools[0].Name != "a" || tools[1].Name != "b" {
		t.Fatalf("pagination failed: %+v", tools)
	}
}

// rpcHTTPHandler answers a single JSON-RPC request from an HTTP body, used by the
// HTTP transport tests. asSSE controls whether the reply is sent as SSE.
func rpcHTTPHandler(t *testing.T, asSSE bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			ID     int64                  `json:"id"`
			Method string                 `json:"method"`
			Params map[string]interface{} `json:"params"`
		}
		_ = json.Unmarshal(body, &req)
		if req.Method == "initialize" {
			w.Header().Set("Mcp-Session-Id", "sess-123")
		}
		// A notification (no id) just gets a 202.
		if req.ID == 0 {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		var result interface{}
		switch req.Method {
		case "initialize":
			result = map[string]interface{}{"protocolVersion": protocolVersion}
		case "tools/list":
			result = map[string]interface{}{"tools": []Tool{{Name: "weather"}}}
		case "tools/call":
			result = CallResult{Content: []Content{{Type: "text", Text: "sunny"}}}
		}
		resp, _ := json.Marshal(map[string]interface{}{"jsonrpc": "2.0", "id": req.ID, "result": result})
		if asSSE {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			fmt.Fprintf(w, "event: message\ndata: %s\n\n", resp)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(resp)
	}
}

func TestClientHTTPJSON(t *testing.T) {
	var sawSession string
	mux := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s := r.Header.Get("Mcp-Session-Id"); s != "" {
			sawSession = s
		}
		rpcHTTPHandler(t, false)(w, r)
	})
	ts := httptest.NewServer(mux)
	defer ts.Close()

	c, err := Dial(ServerConfig{Name: "w", Transport: "http", URL: ts.URL})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()

	tools, err := c.ListTools()
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if len(tools) != 1 || tools[0].Name != "weather" {
		t.Fatalf("unexpected tools: %+v", tools)
	}
	// The session id handed out by initialize must be echoed on later requests.
	if sawSession != "sess-123" {
		t.Fatalf("session id not propagated, saw %q", sawSession)
	}

	res, err := c.CallTool("weather", map[string]interface{}{"city": "NYC"})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if res.Text() != "sunny" {
		t.Fatalf("Text() = %q", res.Text())
	}
}

func TestClientHTTPSSE(t *testing.T) {
	ts := httptest.NewServer(rpcHTTPHandler(t, true))
	defer ts.Close()

	c, err := Dial(ServerConfig{Name: "w", Transport: "streamable-http", URL: ts.URL})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()

	res, err := c.CallTool("weather", nil)
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if res.Text() != "sunny" {
		t.Fatalf("Text() = %q", res.Text())
	}
}

func TestDialValidatesConfig(t *testing.T) {
	cases := []ServerConfig{
		{Name: "a", Transport: "stdio"},          // missing command
		{Name: "b", Transport: "http"},           // missing url
		{Name: "c", Transport: "carrier-pigeon"}, // unknown transport
	}
	for _, cfg := range cases {
		if _, err := Dial(cfg); err == nil {
			t.Fatalf("expected error for %+v", cfg)
		}
	}
}

// TestHTTPErrorStatus surfaces a non-2xx HTTP status as an error.
func TestHTTPErrorStatus(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusInternalServerError)
	}))
	defer ts.Close()

	if _, err := Dial(ServerConfig{Name: "x", Transport: "http", URL: ts.URL}); err == nil {
		t.Fatal("expected error from 500 response")
	}
}
