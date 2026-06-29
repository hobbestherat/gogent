package gogent

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"gogent/internal/config"
	"gogent/internal/lsp"
	"gogent/internal/permission"

	"go.lsp.dev/jsonrpc2"
)

// wiringFakeServer is a minimal in-memory LSP server for the gogent wiring test. It
// answers the handshake and records the document-sync notifications the fileops
// subscription drives, so the OnMutation/OnRemove -> Manager.FileChanged path is
// observable without a real language server.
type wiringFakeServer struct {
	conn jsonrpc2.Conn

	mu        sync.Mutex
	didChange int
	didClose  int
}

const wiringCaps = `{"capabilities":{"textDocumentSync":{"openClose":true,"change":1},"hoverProvider":true}}`

func (fs *wiringFakeServer) handle(_ context.Context, req *jsonrpc2.Request) (any, error) {
	switch req.Method() {
	case "initialize":
		return jsonrpc2.RawMessage(wiringCaps), nil
	case "textDocument/didChange":
		fs.bump(&fs.didChange)
		return nil, nil
	case "textDocument/didClose":
		fs.bump(&fs.didClose)
		return nil, nil
	default:
		return jsonrpc2.RawMessage("null"), nil
	}
}

func (fs *wiringFakeServer) bump(p *int) {
	fs.mu.Lock()
	*p++
	fs.mu.Unlock()
}

func (fs *wiringFakeServer) counts() (change, closed int) {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	return fs.didChange, fs.didClose
}

// wiringPrompter records the permission requests the launch gate makes.
type wiringPrompter struct {
	mu       sync.Mutex
	requests []permission.Request
}

func (p *wiringPrompter) AskPermission(r permission.Request) permission.Decision {
	p.mu.Lock()
	p.requests = append(p.requests, r)
	p.mu.Unlock()
	return permission.DecisionAllow
}
func (p *wiringPrompter) seen() []permission.Request {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]permission.Request(nil), p.requests...)
}

func waitForCond(t *testing.T, what string, cond func() bool) {
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

// TestStartLSPServersWiring asserts the gogent-side adapter: the launch gate maps to
// ActionLSP (resource = server name), and the fileops OnMutation/OnRemove callbacks
// reach Manager.FileChanged with the right FileChangeKind (re-sync on change, close
// on removal) (LSP design §8, §11.2).
func TestStartLSPServersWiring(t *testing.T) {
	tempDir := t.TempDir()
	ws := filepath.Join(tempDir, "ws")
	if err := os.MkdirAll(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	srcPath := filepath.Join(ws, "a.go")
	if err := os.WriteFile(srcPath, []byte("package a\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	g := NewGogentWithWorkspace(tempDir, ws)
	prompter := &wiringPrompter{}
	g.GetPermissionService().SetPrompter(prompter)
	// `go` is on PATH in the test environment, so the server survives the LookPath
	// guard; its real launch is replaced with an in-memory fake below.
	g.config = &config.Config{LSPServers: []config.LSPServerConfig{{
		Name:       "gopls",
		Language:   "go",
		Extensions: []string{".go"},
		Command:    "go",
	}}}

	g.StartLSPServers()
	t.Cleanup(g.CloseLSPServers)

	mgr := g.lspManager
	if mgr == nil {
		t.Fatal("StartLSPServers did not configure a Manager")
	}
	if g.fileMutation == nil || g.fileMutation.OnMutation == nil || g.fileMutation.OnRemove == nil {
		t.Fatal("StartLSPServers did not wire the fileops OnMutation/OnRemove subscription")
	}

	// Replace the real stdio launch with an in-memory fake before the first touch.
	fs := &wiringFakeServer{}
	mgr.Spawn = func(lsp.ServerConfig) (jsonrpc2.Stream, func() error, error) {
		c1, c2 := net.Pipe()
		fs.conn = jsonrpc2.NewConn(jsonrpc2.NewStream(c2))
		fs.conn.Go(context.Background(), fs.handle)
		return jsonrpc2.NewStream(c1), func() error { _ = c1.Close(); return nil }, nil
	}

	// First touch spawns the server, firing the launch gate exactly once.
	client, err := mgr.ClientForFile(context.Background(), srcPath)
	if err != nil {
		t.Fatalf("ClientForFile: %v", err)
	}
	// Open the document so a subsequent change re-syncs.
	if _, err := client.Hover(context.Background(), srcPath, lsp.Position{Line: 1, Character: 1}); err != nil {
		t.Fatalf("Hover (to open doc): %v", err)
	}

	gate := prompter.seen()
	if len(gate) != 1 {
		t.Fatalf("launch gate prompted %d times, want 1", len(gate))
	}
	if gate[0].Action != permission.ActionLSP {
		t.Errorf("launch gate action = %q, want %q", gate[0].Action, permission.ActionLSP)
	}
	if gate[0].Resource != "gopls" {
		t.Errorf("launch gate resource = %q, want the server name 'gopls'", gate[0].Resource)
	}

	// A gogent write to the open file (OnMutation, created=false -> FileChanged) must
	// re-sync the document.
	if err := os.WriteFile(srcPath, []byte("package a\nvar X int\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	g.fileMutation.OnMutation(srcPath, false)
	waitForCond(t, "didChange after OnMutation", func() bool {
		change, _ := fs.counts()
		return change >= 1
	})

	// A removal (OnRemove -> FileDeleted) must close the document.
	g.fileMutation.OnRemove(srcPath)
	waitForCond(t, "didClose after OnRemove", func() bool {
		_, closed := fs.counts()
		return closed >= 1
	})
}
