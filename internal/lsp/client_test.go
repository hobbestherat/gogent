package lsp

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"go.lsp.dev/jsonrpc2"
)

// stubHost records the server→client callbacks the Client makes.
type stubHost struct {
	mu          sync.Mutex
	edits       []WorkspaceEdit
	applyResult bool
	applyReason string
	config      map[string]any
	logs        []string
}

func (h *stubHost) ApplyEdit(_ string, edit WorkspaceEdit) (bool, string, error) {
	h.mu.Lock()
	h.edits = append(h.edits, edit)
	res, reason := h.applyResult, h.applyReason
	h.mu.Unlock()
	return res, reason, nil
}

func (h *stubHost) Configuration(_, section, _ string) (any, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	v, ok := h.config[section]
	return v, ok
}

func (h *stubHost) Logf(_, format string, _ ...any) {
	h.mu.Lock()
	h.logs = append(h.logs, format)
	h.mu.Unlock()
}

func goCfg() ServerConfig {
	return ServerConfig{Name: "fake", LanguageID: "go", Extensions: []string{".go"},
		AllowedCommands: []string{"gopls.tidy"}}
}

// writeFile creates a file under a temp dir and returns its absolute path.
func writeFile(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "a.go")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestDiagnosticsVersionCorrelation is the headline Tier 1 path: opening a file
// correlates the settled diagnostics on its didOpen version, so a fresh, unedited
// file returns the server's findings rather than a stale-empty set (§11.4).
func TestDiagnosticsVersionCorrelation(t *testing.T) {
	fs := newFakeServer()
	fs.pushOnSync = true
	fs.pushDiagnostics = `[{"range":{"start":{"line":0,"character":0},"end":{"line":0,"character":3}},"severity":1,"code":"E1","source":"go","message":"boom"}]`
	c := fs.connectClient(t, goCfg(), &stubHost{})

	path := writeFile(t, "package a\n")
	diags, err := c.Diagnostics(context.Background(), path)
	if err != nil {
		t.Fatalf("Diagnostics: %v", err)
	}
	if len(diags) != 1 {
		t.Fatalf("want 1 diagnostic, got %d (%+v)", len(diags), diags)
	}
	d := diags[0]
	if d.Message != "boom" || d.Severity != 1 || d.Code != "E1" || d.Source != "go" {
		t.Errorf("unexpected diagnostic: %+v", d)
	}
	if d.Range.Start.Line != 1 || d.Range.Start.Character != 1 {
		t.Errorf("range should be 1-based at the edge, got %+v", d.Range)
	}
}

// TestDiagnosticsRegisterBeforeSendRace pushes the diagnostic the instant the
// sync arrives (pushOnSync), which only yields correct results if the freshness
// waiter was registered before didOpen was sent (§9). A second call to an
// unchanged file returns the cached set without re-opening.
func TestDiagnosticsReSyncOnAccess(t *testing.T) {
	fs := newFakeServer()
	fs.pushOnSync = true
	c := fs.connectClient(t, goCfg(), &stubHost{})
	path := writeFile(t, "package a\n")

	if _, err := c.Diagnostics(context.Background(), path); err != nil {
		t.Fatalf("first Diagnostics: %v", err)
	}
	change0, _, _, _ := fs.counts()

	// An out-of-band edit to the open file must be re-synced (didChange) before the
	// next request (§11.1).
	if err := os.WriteFile(path, []byte("package a\nvar X int\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Diagnostics(context.Background(), path); err != nil {
		t.Fatalf("second Diagnostics: %v", err)
	}
	waitFor(t, "didChange after out-of-band edit", func() bool {
		change, _, _, _ := fs.counts()
		return change > change0
	})
}

// TestDefinitionKinds covers the definition family and the Location/LocationLink
// union normalization (§7.2).
func TestDefinitionKinds(t *testing.T) {
	fs := newFakeServer()
	fs.setResult("textDocument/definition", `[{"uri":"file:///ws/b.go","range":{"start":{"line":4,"character":0},"end":{"line":4,"character":5}}}]`)
	fs.setResult("textDocument/typeDefinition", `{"uri":"file:///ws/t.go","range":{"start":{"line":1,"character":2},"end":{"line":1,"character":3}}}`)
	c := fs.connectClient(t, goCfg(), &stubHost{})
	path := writeFile(t, "package a\n")

	locs, err := c.Definition(context.Background(), path, Position{1, 1}, DefDefinition)
	if err != nil {
		t.Fatalf("Definition: %v", err)
	}
	if len(locs) != 1 || locs[0].Range.Start.Line != 5 {
		t.Fatalf("definition: %+v", locs)
	}
	locs, err = c.Definition(context.Background(), path, Position{1, 1}, DefTypeDefinition)
	if err != nil {
		t.Fatalf("TypeDefinition: %v", err)
	}
	if len(locs) != 1 || filepath.Base(locs[0].Path) != "t.go" {
		t.Fatalf("typeDefinition: %+v", locs)
	}
}

// TestHover returns the contents of a markup hover.
func TestHover(t *testing.T) {
	fs := newFakeServer()
	fs.setResult("textDocument/hover", `{"contents":{"kind":"markdown","value":"func F()"}}`)
	c := fs.connectClient(t, goCfg(), &stubHost{})
	path := writeFile(t, "package a\n")
	h, err := c.Hover(context.Background(), path, Position{1, 1})
	if err != nil {
		t.Fatalf("Hover: %v", err)
	}
	if h.Contents != "func F()" {
		t.Fatalf("hover contents = %q", h.Contents)
	}
}

// TestDocumentSymbolsBothShapes proves the edge normalizes BOTH the hierarchical
// DocumentSymbol tree and the flat SymbolInformation list to the same []Symbol
// shape, so the tree is never lost on a flat-only server (§7.2).
func TestDocumentSymbolsBothShapes(t *testing.T) {
	hierarchical := `[{"name":"Foo","kind":12,"range":{"start":{"line":0,"character":0},"end":{"line":2,"character":0}},"selectionRange":{"start":{"line":0,"character":5},"end":{"line":0,"character":8}},"children":[{"name":"bar","kind":13,"range":{"start":{"line":1,"character":1},"end":{"line":1,"character":4}},"selectionRange":{"start":{"line":1,"character":1},"end":{"line":1,"character":4}}}]}]`
	flat := `[{"name":"Foo","kind":12,"location":{"uri":"file:///ws/a.go","range":{"start":{"line":0,"character":0},"end":{"line":2,"character":0}}}}]`

	t.Run("hierarchical", func(t *testing.T) {
		fs := newFakeServer()
		fs.setResult("textDocument/documentSymbol", hierarchical)
		c := fs.connectClient(t, goCfg(), &stubHost{})
		syms, err := c.DocumentSymbols(context.Background(), writeFile(t, "package a\n"))
		if err != nil {
			t.Fatal(err)
		}
		if len(syms) != 1 || syms[0].Name != "Foo" || len(syms[0].Children) != 1 || syms[0].Children[0].Name != "bar" {
			t.Fatalf("hierarchical symbols not mapped: %+v", syms)
		}
		if syms[0].Kind != "function" || syms[0].Children[0].Kind != "variable" {
			t.Fatalf("symbol kinds: %+v", syms)
		}
	})

	t.Run("flat", func(t *testing.T) {
		fs := newFakeServer()
		fs.setResult("textDocument/documentSymbol", flat)
		c := fs.connectClient(t, goCfg(), &stubHost{})
		syms, err := c.DocumentSymbols(context.Background(), writeFile(t, "package a\n"))
		if err != nil {
			t.Fatal(err)
		}
		if len(syms) != 1 || syms[0].Name != "Foo" || syms[0].Kind != "function" {
			t.Fatalf("flat symbols not synthesized: %+v", syms)
		}
	})
}

// TestUnsupportedReturnsErrUnsupported confirms a capability the server does not
// advertise yields the clean ErrUnsupported result, not an assumption (§7.2).
func TestUnsupportedReturnsErrUnsupported(t *testing.T) {
	fs := newFakeServer()
	// Capabilities advertise nothing.
	fs.setCaps(`{"capabilities":{}}`)
	c := fs.connectClient(t, goCfg(), &stubHost{})
	path := writeFile(t, "package a\n")
	if _, err := c.Hover(context.Background(), path, Position{1, 1}); err != ErrUnsupported {
		t.Fatalf("Hover unsupported = %v, want ErrUnsupported", err)
	}
}

// TestDynamicRegistrationEnablesOp confirms an op unsupported at initialize
// becomes supported after a client/registerCapability (§7.2).
func TestDynamicRegistrationEnablesOp(t *testing.T) {
	fs := newFakeServer()
	fs.setCaps(`{"capabilities":{}}`)
	fs.setResult("textDocument/hover", `{"contents":"hi"}`)
	c := fs.connectClient(t, goCfg(), &stubHost{})
	path := writeFile(t, "package a\n")
	if _, err := c.Hover(context.Background(), path, Position{1, 1}); err != ErrUnsupported {
		t.Fatalf("pre-registration Hover = %v, want ErrUnsupported", err)
	}
	fs.registerCapability("textDocument/hover", "")
	waitFor(t, "hover capability registered", func() bool { return c.supports(methodHover) })
	h, err := c.Hover(context.Background(), path, Position{1, 1})
	if err != nil {
		t.Fatalf("post-registration Hover: %v", err)
	}
	if h.Contents != "hi" {
		t.Fatalf("hover contents = %q", h.Contents)
	}
}

// TestRenamePreview returns a proposed WorkspaceEdit with both text edits and a
// rename resource op, normalized to the boundary type (§12).
func TestRenamePreview(t *testing.T) {
	fs := newFakeServer()
	fs.setResult("textDocument/prepareRename", `{"start":{"line":0,"character":0},"end":{"line":0,"character":3}}`)
	fs.setResult("textDocument/rename", `{"documentChanges":[{"textDocument":{"uri":"file:///ws/a.go","version":1},"edits":[{"range":{"start":{"line":0,"character":0},"end":{"line":0,"character":3}},"newText":"Bar"}]},{"kind":"rename","oldUri":"file:///ws/a.go","newUri":"file:///ws/b.go"}]}`)
	c := fs.connectClient(t, goCfg(), &stubHost{})
	path := writeFile(t, "package a\n")
	edit, err := c.Rename(context.Background(), path, Position{1, 1}, "Bar")
	if err != nil {
		t.Fatalf("Rename: %v", err)
	}
	if len(edit.Changes) != 1 {
		t.Fatalf("rename changes: %+v", edit.Changes)
	}
	if len(edit.ResourceOps) != 1 || edit.ResourceOps[0].Kind != "rename" {
		t.Fatalf("rename resource op missing: %+v", edit.ResourceOps)
	}
}

// TestCodeActionResolvesLazyEdit confirms an action returned with only a data
// payload is resolved via codeAction/resolve so the preview shows a real edit
// (§12).
func TestCodeActionResolvesLazyEdit(t *testing.T) {
	fs := newFakeServer()
	fs.setResult("textDocument/codeAction", `[{"title":"Fix","kind":"quickfix","data":{"id":1}}]`)
	fs.setResult("codeAction/resolve", `{"title":"Fix","kind":"quickfix","edit":{"changes":{"file:///ws/a.go":[{"range":{"start":{"line":0,"character":0},"end":{"line":0,"character":1}},"newText":"X"}]}}}`)
	c := fs.connectClient(t, goCfg(), &stubHost{})
	path := writeFile(t, "package a\n")
	actions, err := c.CodeActions(context.Background(), path, Range{Position{1, 1}, Position{1, 2}})
	if err != nil {
		t.Fatalf("CodeActions: %v", err)
	}
	if len(actions) != 1 || actions[0].Edit == nil {
		t.Fatalf("lazy code action not resolved: %+v", actions)
	}
}

// TestExecuteCommandAllowList enforces the executeCommand allow-list: an off-list
// command never runs (§12).
func TestExecuteCommandAllowList(t *testing.T) {
	fs := newFakeServer()
	fs.setResult("workspace/executeCommand", `"ok"`)
	c := fs.connectClient(t, goCfg(), &stubHost{})
	if _, err := c.ExecuteCommand(context.Background(), "danger.rm", nil); err != ErrCommandNotAllowed {
		t.Fatalf("off-list command = %v, want ErrCommandNotAllowed", err)
	}
	if _, err := c.ExecuteCommand(context.Background(), "gopls.tidy", nil); err != nil {
		t.Fatalf("allow-listed command: %v", err)
	}
}

// TestApplyEditCallback routes a server-driven workspace/applyEdit through the
// Host and returns its applied result to the server (§10, §12).
func TestApplyEditCallback(t *testing.T) {
	fs := newFakeServer()
	host := &stubHost{applyResult: true}
	c := fs.connectClient(t, goCfg(), host)
	_ = c

	var res struct {
		Applied bool `json:"applied"`
	}
	params := jsonrpc2.RawMessage(`{"edit":{"changes":{"file:///ws/a.go":[{"range":{"start":{"line":0,"character":0},"end":{"line":0,"character":1}},"newText":"Z"}]}}}`)
	if _, err := fs.conn.Call(context.Background(), "workspace/applyEdit", params, &res); err != nil {
		t.Fatalf("applyEdit call: %v", err)
	}
	if !res.Applied {
		t.Fatal("applyEdit should report applied:true from the host")
	}
	host.mu.Lock()
	n := len(host.edits)
	host.mu.Unlock()
	if n != 1 {
		t.Fatalf("host.ApplyEdit called %d times, want 1", n)
	}
}

// TestFileChangedEmitsWatchedAndSync confirms a gogent write to an open, watched
// file both re-syncs the document and emits didChangeWatchedFiles (§11.2, §11.5).
func TestFileChangedEmitsWatchedAndSync(t *testing.T) {
	fs := newFakeServer()
	c := fs.connectClient(t, goCfg(), &stubHost{})
	path := writeFile(t, "package a\n")
	// Open the document first.
	if _, err := c.Diagnostics(context.Background(), path); err != nil {
		t.Fatal(err)
	}
	// The server registers a watcher for **/*.go.
	fs.registerCapability("workspace/didChangeWatchedFiles", `{"watchers":[{"globPattern":"**/*.go"}]}`)
	waitFor(t, "watcher registered", func() bool {
		c.mu.Lock()
		defer c.mu.Unlock()
		return len(c.caps.watcherGlobs) > 0
	})

	if err := os.WriteFile(path, []byte("package a\nvar Y int\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	c.FileChanged(context.Background(), path, FileChanged)
	waitFor(t, "didChange + didSave + watched", func() bool {
		change, save, _, watched := fs.counts()
		return change >= 1 && save >= 1 && watched >= 1
	})
}

// TestCancellationEmitsCancelRequest proves the concurrency model's cancellation
// contract (§9): when a caller's context is cancelled while a request is in flight,
// the client emits $/cancelRequest for that exact request id and unblocks the
// caller with context.Canceled. The fake server dispatches asynchronously and
// blocks the references handler so the cancel races a genuinely outstanding call.
func TestCancellationEmitsCancelRequest(t *testing.T) {
	fs := newFakeServer()
	fs.async = true
	fs.blockMethod = "textDocument/references"
	fs.setResult("textDocument/references", "[]")
	c := fs.connectClient(t, goCfg(), &stubHost{})
	path := writeFile(t, "package a\n")

	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() {
		_, err := c.References(ctx, path, Position{1, 1}, false)
		errc <- err
	}()

	waitFor(t, "references in flight on the server", func() bool { return fs.inFlight() != 0 })
	cancel()
	waitFor(t, "$/cancelRequest received", func() bool { return len(fs.cancelled()) > 0 })

	wantID := fs.inFlight()
	if got := fs.cancelled(); got[0] != wantID {
		t.Fatalf("$/cancelRequest id = %d, want the in-flight request id %d", got[0], wantID)
	}
	if err := <-errc; !errors.Is(err, context.Canceled) {
		t.Fatalf("References after cancel = %v, want context.Canceled", err)
	}
}

// TestManagerRoutingAndLazySpawn checks extension routing, ErrNoServer, and that
// concurrent first-touches share one single-flight spawn (§9).
func TestManagerRoutingAndLazySpawn(t *testing.T) {
	var spawns int
	var mu sync.Mutex
	fs := newFakeServer()
	mgr := NewManager("/ws", []ServerConfig{goCfg()}, &stubHost{})
	mgr.Spawn = func(cfg ServerConfig) (jsonrpc2.Stream, func() error, error) {
		mu.Lock()
		spawns++
		mu.Unlock()
		c1, c2 := net.Pipe()
		fs.conn = jsonrpc2.NewConn(jsonrpc2.NewStream(c2))
		fs.conn.Go(context.Background(), fs.handle)
		return jsonrpc2.NewStream(c1), func() error { _ = c1.Close(); return nil }, nil
	}
	t.Cleanup(mgr.Shutdown)

	if _, err := mgr.ClientForFile(context.Background(), "/ws/x.txt"); err != ErrNoServer {
		t.Fatalf("unrouted file = %v, want ErrNoServer", err)
	}

	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := mgr.ClientForFile(context.Background(), "/ws/pkg/a.go"); err != nil {
				t.Errorf("ClientForFile: %v", err)
			}
		}()
	}
	wg.Wait()
	mu.Lock()
	got := spawns
	mu.Unlock()
	if got != 1 {
		t.Fatalf("single-flight spawn ran %d times, want 1", got)
	}
}

// TestManagerLaunchGate confirms the lazy-launch gate (ActionLSP equivalent) can
// veto a spawn, leaving no client (§9).
func TestManagerLaunchGate(t *testing.T) {
	mgr := NewManager("/ws", []ServerConfig{goCfg()}, &stubHost{})
	mgr.LaunchGate = func(ServerConfig) error { return context.Canceled }
	mgr.Spawn = func(ServerConfig) (jsonrpc2.Stream, func() error, error) {
		t.Fatal("spawn must not run when the launch gate denies")
		return nil, nil, nil
	}
	t.Cleanup(mgr.Shutdown)
	if _, err := mgr.ClientForFile(context.Background(), "/ws/a.go"); err != context.Canceled {
		t.Fatalf("gated launch = %v, want context.Canceled", err)
	}
}

// TestConfigurationPull answers a workspace/configuration request from the Host's
// settings, scope-aware (§10).
func TestConfigurationPull(t *testing.T) {
	fs := newFakeServer()
	host := &stubHost{config: map[string]any{"gopls": map[string]any{"usePlaceholders": true}}}
	c := fs.connectClient(t, goCfg(), host)
	_ = c

	var got []map[string]any
	params := jsonrpc2.RawMessage(`{"items":[{"section":"gopls"},{"section":"missing"}]}`)
	if _, err := fs.conn.Call(context.Background(), "workspace/configuration", params, &got); err != nil {
		t.Fatalf("configuration call: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 config items, got %d", len(got))
	}
	if got[0] == nil || got[0]["usePlaceholders"] != true {
		t.Fatalf("gopls section not answered from settings: %+v", got[0])
	}
	if got[1] != nil {
		t.Fatalf("missing section should be null, got %+v", got[1])
	}
}
