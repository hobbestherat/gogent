package lsp

import (
	"context"
	"encoding/json"
	"net"
	"sync"
	"testing"
	"time"

	"go.lsp.dev/jsonrpc2"
	"go.lsp.dev/uri"
)

// fakeServer is an in-memory LSP server for unit tests. It speaks the same
// Content-Length-framed JSON-RPC the real transport uses, over a net.Pipe, but
// answers a handful of methods with canned JSON (raw payloads pass through the
// default jsonrpc2 codec verbatim). Tests drive its responses and capture the
// notifications the Client sends, exercising every Client code path without a real
// language server.
type fakeServer struct {
	conn jsonrpc2.Conn

	mu          sync.Mutex
	capsJSON    string
	results     map[string]string // method -> canned result JSON
	openVersion map[string]int32  // uri -> last didOpen/didChange version seen
	methodCalls map[string]int    // method -> number of requests received
	didChange   int
	didSave     int
	didClose    int
	watched     int
	// pushOnSync, when set, makes the server publish diagnostics (with the synced
	// version) whenever it receives a didOpen/didChange, exercising version
	// correlation.
	pushOnSync      bool
	pushDiagnostics string // diagnostics array JSON pushed on sync

	// async, when set, dispatches each inbound request on its own goroutine (like a
	// real LSP server), so a deliberately blocking method does not wedge the read
	// loop and a concurrent $/cancelRequest is still observed.
	async bool
	// blockMethod, when set, makes that request method block until a $/cancelRequest
	// (or Close) releases it, recording the in-flight request id so a test can prove
	// cancellation emits $/cancelRequest for the right id (§9).
	blockMethod  string
	inflightID   int64
	cancelledIDs []int64
	release      chan struct{}
	releaseOnce  sync.Once
}

// defaultCapsJSON advertises every provider the curated ops gate on, with the
// resolve/prepare sub-flags set, so capability gating passes by default.
const defaultCapsJSON = `{"capabilities":{
	"textDocumentSync":{"openClose":true,"change":1,"save":{"includeText":false}},
	"definitionProvider":true,"declarationProvider":true,"typeDefinitionProvider":true,
	"implementationProvider":true,"referencesProvider":true,"hoverProvider":true,
	"documentSymbolProvider":true,"workspaceSymbolProvider":true,
	"callHierarchyProvider":true,"renameProvider":{"prepareProvider":true},
	"codeActionProvider":{"resolveProvider":true},"documentFormattingProvider":true,
	"executeCommandProvider":{"commands":["gopls.tidy"]}
}}`

func newFakeServer() *fakeServer {
	return &fakeServer{
		capsJSON:    defaultCapsJSON,
		results:     map[string]string{},
		openVersion: map[string]int32{},
		methodCalls: map[string]int{},
		release:     make(chan struct{}),
	}
}

// connectClient wires a Client to the fake server over a net.Pipe and returns the
// ready Client. The handshake runs during newClient, so the fake server's read
// loop is started first.
func (fs *fakeServer) connectClient(t *testing.T, cfg ServerConfig, host Host) *Client {
	t.Helper()
	c1, c2 := net.Pipe()
	ctx := context.Background()
	fs.conn = jsonrpc2.NewConn(jsonrpc2.NewStream(c2))
	fs.conn.Go(ctx, fs.handler())

	client, err := newClient(ctx, cfg, uri.File("/ws"), "/ws", host,
		jsonrpc2.NewStream(c1), func() error { _ = c1.Close(); return nil })
	if err != nil {
		t.Fatalf("connect client: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return client
}

// handler returns the read-loop handler, wrapped for concurrent dispatch when
// async is set so a blocking method does not wedge the loop (§9).
func (fs *fakeServer) handler() jsonrpc2.Handler {
	if fs.async {
		return jsonrpc2.AsyncHandler(fs.handle)
	}
	return fs.handle
}

// releaseBlocked unblocks any handler waiting on the blockMethod gate (once).
func (fs *fakeServer) releaseBlocked() {
	fs.releaseOnce.Do(func() { close(fs.release) })
}

// inFlight returns the id of the currently blocked request (0 if none).
func (fs *fakeServer) inFlight() int64 {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	return fs.inflightID
}

// cancelled returns the ids the client asked to cancel via $/cancelRequest.
func (fs *fakeServer) cancelled() []int64 {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	return append([]int64(nil), fs.cancelledIDs...)
}

// handle answers requests and records notifications. Results are returned as raw
// JSON (passed through verbatim by the default codec).
func (fs *fakeServer) handle(ctx context.Context, req *jsonrpc2.Request) (any, error) {
	method := req.Method()
	fs.mu.Lock()
	fs.methodCalls[method]++
	fs.mu.Unlock()
	if method == "$/cancelRequest" {
		var p struct {
			ID int64 `json:"id"`
		}
		_ = json.Unmarshal(req.Params(), &p)
		fs.mu.Lock()
		fs.cancelledIDs = append(fs.cancelledIDs, p.ID)
		fs.mu.Unlock()
		fs.releaseBlocked()
		return nil, nil
	}
	if fs.blockMethod != "" && method == fs.blockMethod {
		if n, ok := req.ID().Number(); ok {
			fs.mu.Lock()
			fs.inflightID = n
			fs.mu.Unlock()
		}
		<-fs.release
	}
	switch method {
	case "initialize":
		fs.mu.Lock()
		caps := fs.capsJSON
		fs.mu.Unlock()
		return jsonrpc2.RawMessage(caps), nil
	case "initialized", "shutdown", "exit",
		"workspace/didChangeConfiguration", "textDocument/didClose":
		if method == "textDocument/didClose" {
			fs.bump(&fs.didClose)
		}
		return nil, nil
	case "textDocument/didOpen", "textDocument/didChange":
		fs.onSync(ctx, method, req)
		return nil, nil
	case "textDocument/didSave":
		fs.bump(&fs.didSave)
		return nil, nil
	case "workspace/didChangeWatchedFiles":
		fs.bump(&fs.watched)
		return nil, nil
	default:
		fs.mu.Lock()
		res, ok := fs.results[method]
		fs.mu.Unlock()
		if ok {
			return jsonrpc2.RawMessage(res), nil
		}
		return jsonrpc2.RawMessage("null"), nil
	}
}

// onSync records the synced version and optionally pushes diagnostics correlated
// on that version.
func (fs *fakeServer) onSync(ctx context.Context, method string, req *jsonrpc2.Request) {
	var p struct {
		TextDocument struct {
			URI     string `json:"uri"`
			Version int32  `json:"version"`
		} `json:"textDocument"`
	}
	_ = json.Unmarshal(req.Params(), &p)
	fs.mu.Lock()
	if method == "textDocument/didChange" {
		fs.didChange++
	}
	fs.openVersion[p.TextDocument.URI] = p.TextDocument.Version
	push := fs.pushOnSync
	diags := fs.pushDiagnostics
	if diags == "" {
		diags = "[]"
	}
	uriStr := p.TextDocument.URI
	version := p.TextDocument.Version
	fs.mu.Unlock()

	if push {
		body := `{"uri":"` + uriStr + `","version":` + itoa(int(version)) + `,"diagnostics":` + diags + `}`
		_ = fs.conn.Notify(ctx, "textDocument/publishDiagnostics", jsonrpc2.RawMessage(body))
	}
}

// pushDiag publishes diagnostics for uriStr at version out-of-band (e.g. before a
// version is sent, to test the register-before-send race).
func (fs *fakeServer) pushDiag(uriStr string, version int, diags string) {
	body := `{"uri":"` + uriStr + `","version":` + itoa(version) + `,"diagnostics":` + diags + `}`
	_ = fs.conn.Notify(context.Background(), "textDocument/publishDiagnostics", jsonrpc2.RawMessage(body))
}

// registerCapability sends a client/registerCapability the Client must honor.
func (fs *fakeServer) registerCapability(method, registerOptions string) {
	if registerOptions == "" {
		registerOptions = "null"
	}
	body := `{"registrations":[{"id":"r1","method":"` + method + `","registerOptions":` + registerOptions + `}]}`
	_ = fs.conn.Notify(context.Background(), "client/registerCapability", jsonrpc2.RawMessage(body))
}

func (fs *fakeServer) bump(p *int) {
	fs.mu.Lock()
	*p++
	fs.mu.Unlock()
}

// callCount returns how many requests the server received for method.
func (fs *fakeServer) callCount(method string) int {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	return fs.methodCalls[method]
}

func (fs *fakeServer) setResult(method, json string) {
	fs.mu.Lock()
	fs.results[method] = json
	fs.mu.Unlock()
}

func (fs *fakeServer) setCaps(json string) {
	fs.mu.Lock()
	fs.capsJSON = json
	fs.mu.Unlock()
}

func (fs *fakeServer) counts() (change, save, closed, watched int) {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	return fs.didChange, fs.didSave, fs.didClose, fs.watched
}

// waitFor polls cond up to a short deadline; it keeps pipe-timing flakes out of
// the assertions.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}
