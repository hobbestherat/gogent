package tool

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"gogent/internal/lsp"
	"gogent/internal/permission"

	"go.lsp.dev/jsonrpc2"
)

// lspFakeServer is a minimal in-memory LSP server for the tool-layer tests. It
// answers the handshake and the Tier 3 methods the lsp_* tools exercise with canned
// JSON over a net.Pipe, so the whole tool → Manager → Client → server path runs
// without a real language server.
type lspFakeServer struct {
	conn jsonrpc2.Conn

	mu       sync.Mutex
	commands int // workspace/executeCommand calls actually reached the server
}

const lspToolCaps = `{"capabilities":{
	"textDocumentSync":{"openClose":true,"change":1},
	"renameProvider":true,"documentFormattingProvider":true,
	"codeActionProvider":true,
	"executeCommandProvider":{"commands":["gopls.tidy"]}
}}`

func (fs *lspFakeServer) handle(_ context.Context, req *jsonrpc2.Request) (any, error) {
	switch req.Method() {
	case "initialize":
		return jsonrpc2.RawMessage(lspToolCaps), nil
	case "textDocument/rename":
		return jsonrpc2.RawMessage(`{"changes":{"file:///ws/a.go":[{"range":{"start":{"line":0,"character":0},"end":{"line":0,"character":3}},"newText":"Bar"}]}}`), nil
	case "textDocument/formatting":
		return jsonrpc2.RawMessage(`[{"range":{"start":{"line":0,"character":0},"end":{"line":0,"character":0}},"newText":"// fmt\n"}]`), nil
	case "textDocument/codeAction":
		return jsonrpc2.RawMessage(`[{"title":"Fix","kind":"quickfix","edit":{"changes":{"file:///ws/a.go":[{"range":{"start":{"line":0,"character":0},"end":{"line":0,"character":1}},"newText":"X"}]}}}]`), nil
	case "workspace/executeCommand":
		fs.mu.Lock()
		fs.commands++
		fs.mu.Unlock()
		return jsonrpc2.RawMessage(`"ok"`), nil
	default:
		// initialized, didOpen, didChange, shutdown, exit, ... are no-ops.
		return jsonrpc2.RawMessage("null"), nil
	}
}

func (fs *lspFakeServer) commandCount() int {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	return fs.commands
}

// recordingHost records ApplyEdit calls so the preview-vs-apply routing is
// observable. It applies nothing — it only proves the apply path reaches the Host.
type recordingHost struct {
	mu     sync.Mutex
	edits  []lsp.WorkspaceEdit
	result bool
}

func (h *recordingHost) ApplyEdit(_ string, edit lsp.WorkspaceEdit) (bool, string, error) {
	h.mu.Lock()
	h.edits = append(h.edits, edit)
	res := h.result
	h.mu.Unlock()
	return res, "", nil
}
func (h *recordingHost) Configuration(string, string, string) (any, bool) { return nil, false }
func (h *recordingHost) Logf(string, string, ...any)                      {}
func (h *recordingHost) applyCalls() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.edits)
}

// recordingPrompter captures every permission Request and answers with a fixed
// decision, so a test can assert the action/resource a tool gated on.
type recordingPrompter struct {
	mu       sync.Mutex
	requests []permission.Request
	decision permission.Decision
}

func (p *recordingPrompter) AskPermission(r permission.Request) permission.Decision {
	p.mu.Lock()
	p.requests = append(p.requests, r)
	p.mu.Unlock()
	return p.decision
}
func (p *recordingPrompter) seen() []permission.Request {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]permission.Request(nil), p.requests...)
}

// newLSPToolRegistry wires a ToolRegistry whose lsp_* tools route to an in-memory
// fake server, with the given Host and permission service.
func newLSPToolRegistry(t *testing.T, host lsp.Host, perm *permission.Service) (*ToolRegistry, *lspFakeServer) {
	t.Helper()
	// The Tier 3 ops open/sync the file, so it must exist on disk under the
	// workspace root the tools resolve relative paths against.
	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "a.go"), []byte("package a\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	fs := &lspFakeServer{}
	mgr := lsp.NewManager(ws, []lsp.ServerConfig{{
		Name:            "gopls",
		LanguageID:      "go",
		Extensions:      []string{".go"},
		Command:         "gopls",
		AllowedCommands: []string{"gopls.tidy"},
	}}, host)
	mgr.Spawn = func(lsp.ServerConfig) (jsonrpc2.Stream, func() error, error) {
		c1, c2 := net.Pipe()
		fs.conn = jsonrpc2.NewConn(jsonrpc2.NewStream(c2))
		fs.conn.Go(context.Background(), fs.handle)
		return jsonrpc2.NewStream(c1), func() error { _ = c1.Close(); return nil }, nil
	}
	t.Cleanup(mgr.Shutdown)

	tr := NewToolRegistry()
	tr.WorkspaceRoot = ws
	tr.Permission = perm
	tr.RegisterLSPTools(mgr)
	return tr, fs
}

func lspToolResult(t *testing.T, tr *ToolRegistry, name string, args map[string]interface{}) map[string]interface{} {
	t.Helper()
	tool := tr.Get(name)
	if tool == nil {
		t.Fatalf("%s not registered", name)
	}
	res, err := tool.Execute(args, ToolContext{})
	if err != nil {
		t.Fatalf("%s: Execute() error = %v", name, err)
	}
	out, ok := res.(map[string]interface{})
	if !ok {
		t.Fatalf("%s result is %T, want map", name, res)
	}
	return out
}

// TestLSPExecuteCommandGatesActionLSPCommand proves the higher-risk command path is
// gated on ActionLSPCommand (resource = server:command), distinct from ActionWrite,
// and that a denial blocks execution before the command reaches the server (§12).
func TestLSPExecuteCommandGatesActionLSPCommand(t *testing.T) {
	prompter := &recordingPrompter{decision: permission.DecisionDeny}
	perm := permission.New("")
	perm.SetPrompter(prompter)
	tr, fs := newLSPToolRegistry(t, &recordingHost{}, perm)

	tool := tr.Get("lsp_execute_command")
	if tool == nil {
		t.Fatal("lsp_execute_command not registered")
	}
	if _, err := tool.Execute(map[string]interface{}{
		"path": "a.go", "command": "gopls.tidy",
	}, ToolContext{}); err == nil {
		t.Fatal("a denied ActionLSPCommand must abort lsp_execute_command with an error")
	}
	reqs := prompter.seen()
	if len(reqs) != 1 {
		t.Fatalf("permission prompted %d times, want 1", len(reqs))
	}
	if reqs[0].Action != permission.ActionLSPCommand {
		t.Errorf("gated action = %q, want %q (NOT ActionWrite)", reqs[0].Action, permission.ActionLSPCommand)
	}
	if reqs[0].Resource != "gopls:gopls.tidy" {
		t.Errorf("gated resource = %q, want server:command", reqs[0].Resource)
	}
	if fs.commandCount() != 0 {
		t.Fatalf("command reached the server %d times despite denial, want 0", fs.commandCount())
	}
}

// TestLSPExecuteCommandAllowListAndRun confirms an allow-listed command runs once
// approved, while an off-list command returns the structured not-allowed result
// without running (§12).
func TestLSPExecuteCommandAllowListAndRun(t *testing.T) {
	prompter := &recordingPrompter{decision: permission.DecisionAllow}
	perm := permission.New("")
	perm.SetPrompter(prompter)
	tr, fs := newLSPToolRegistry(t, &recordingHost{}, perm)

	out := lspToolResult(t, tr, "lsp_execute_command", map[string]interface{}{
		"path": "a.go", "command": "gopls.tidy",
	})
	if out["executed"] != true {
		t.Fatalf("allow-listed command not executed: %+v", out)
	}
	if fs.commandCount() != 1 {
		t.Fatalf("command ran %d times, want 1", fs.commandCount())
	}

	off := lspToolResult(t, tr, "lsp_execute_command", map[string]interface{}{
		"path": "a.go", "command": "danger.rm",
	})
	if off["executed"] != false {
		t.Fatalf("off-list command should report executed:false: %+v", off)
	}
	if fs.commandCount() != 1 {
		t.Fatalf("off-list command should not run; count = %d, want 1", fs.commandCount())
	}
}

// TestLSPRenamePreviewVsApply confirms rename previews without touching the Host and
// applies through the Host (ActionWrite + Checkpointer) only when apply:true (§12).
func TestLSPRenamePreviewVsApply(t *testing.T) {
	host := &recordingHost{result: true}
	tr, _ := newLSPToolRegistry(t, host, permission.New(""))

	preview := lspToolResult(t, tr, "lsp_rename", map[string]interface{}{
		"path": "a.go", "line": 1, "column": 1, "new_name": "Bar",
	})
	if preview["preview"] != true || preview["applied"] != false {
		t.Fatalf("rename without apply should be a preview: %+v", preview)
	}
	if host.applyCalls() != 0 {
		t.Fatalf("preview must not route through Host.ApplyEdit (calls = %d)", host.applyCalls())
	}

	applied := lspToolResult(t, tr, "lsp_rename", map[string]interface{}{
		"path": "a.go", "line": 1, "column": 1, "new_name": "Bar", "apply": true,
	})
	if applied["applied"] != true {
		t.Fatalf("rename apply:true should report applied:true: %+v", applied)
	}
	if host.applyCalls() != 1 {
		t.Fatalf("apply must route through Host.ApplyEdit exactly once (calls = %d)", host.applyCalls())
	}
}

// TestLSPFormatAndCodeActionApplyRouteThroughHost confirms lsp_format and
// lsp_code_actions apply:true both reach Host.ApplyEdit, while their previews do not.
func TestLSPFormatAndCodeActionApplyRouteThroughHost(t *testing.T) {
	host := &recordingHost{result: true}
	tr, _ := newLSPToolRegistry(t, host, permission.New(""))

	if out := lspToolResult(t, tr, "lsp_format", map[string]interface{}{"path": "a.go"}); out["preview"] != true {
		t.Fatalf("format without apply should preview: %+v", out)
	}
	if host.applyCalls() != 0 {
		t.Fatalf("format preview must not apply (calls = %d)", host.applyCalls())
	}
	if out := lspToolResult(t, tr, "lsp_format", map[string]interface{}{"path": "a.go", "apply": true}); out["applied"] != true {
		t.Fatalf("format apply should apply: %+v", out)
	}

	// Preview code actions to learn the index, then apply it.
	preview := lspToolResult(t, tr, "lsp_code_actions", map[string]interface{}{"path": "a.go", "line": 1, "column": 1})
	if preview["count"].(int) != 1 {
		t.Fatalf("want 1 code action, got %+v", preview)
	}
	applied := lspToolResult(t, tr, "lsp_code_actions", map[string]interface{}{
		"path": "a.go", "line": 1, "column": 1, "apply": true, "action_index": 0,
	})
	if applied["applied"] != true {
		t.Fatalf("code action apply should apply: %+v", applied)
	}
	if host.applyCalls() != 2 {
		t.Fatalf("format+codeAction applies should reach Host twice (calls = %d)", host.applyCalls())
	}
}
