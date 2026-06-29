package lsp

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

// pullCapsJSON advertises a pull diagnostics provider (textDocument/diagnostic)
// and nothing else, modelling a pure pull-only server that never pushes.
const pullCapsJSON = `{"capabilities":{
	"textDocumentSync":{"openClose":true,"change":1},
	"diagnosticProvider":{"interFileDependencies":false,"workspaceDiagnostics":false}
}}`

// TestDiagnosticsPullFallback covers the Tier 1 pull transport (§3, §11.4): a
// server that advertises textDocument/diagnostic but never pushes must, after the
// freshness wait fails to settle, fall back to a synchronous pull and return the
// deduped report items. A second call short-circuits straight to the pull (the
// connection is now marked pull-only) without paying the ceiling again.
func TestDiagnosticsPullFallback(t *testing.T) {
	// Shorten the freshness ceiling so the no-push wait does not block ~3s.
	restore := freshnessCeiling
	freshnessCeiling = 80 * time.Millisecond
	defer func() { freshnessCeiling = restore }()

	fs := newFakeServer()
	fs.setCaps(pullCapsJSON)
	// A full report carrying a duplicate item, so the pull path is shown to dedup.
	fs.setResult("textDocument/diagnostic", `{"kind":"full","items":[
		{"range":{"start":{"line":0,"character":0},"end":{"line":0,"character":3}},"severity":1,"code":"E1","source":"go","message":"boom"},
		{"range":{"start":{"line":0,"character":0},"end":{"line":0,"character":3}},"severity":1,"code":"E1","source":"go","message":"boom"}
	]}`)
	c := fs.connectClient(t, goCfg(), &stubHost{})
	path := writeFile(t, "package a\n")

	diags, err := c.Diagnostics(context.Background(), path)
	if err != nil {
		t.Fatalf("Diagnostics: %v", err)
	}
	if len(diags) != 1 {
		t.Fatalf("want 1 deduped diagnostic from the pull, got %d (%+v)", len(diags), diags)
	}
	if d := diags[0]; d.Message != "boom" || d.Severity != 1 || d.Code != "E1" || d.Source != "go" {
		t.Fatalf("unexpected pulled diagnostic: %+v", d)
	}
	if d := diags[0]; d.Range.Start.Line != 1 || d.Range.Start.Character != 1 {
		t.Fatalf("pulled range should be 1-based at the edge, got %+v", d.Range)
	}
	if n := fs.callCount("textDocument/diagnostic"); n != 1 {
		t.Fatalf("textDocument/diagnostic issued %d times, want 1", n)
	}

	// The connection is now known pull-only: a second read pulls directly (issuing a
	// second pull) instead of waiting out the ceiling again.
	start := time.Now()
	if _, err := c.Diagnostics(context.Background(), path); err != nil {
		t.Fatalf("second Diagnostics: %v", err)
	}
	if elapsed := time.Since(start); elapsed >= freshnessCeiling {
		t.Fatalf("second pull-only read took %s, should have short-circuited the ceiling", elapsed)
	}
	if n := fs.callCount("textDocument/diagnostic"); n != 2 {
		t.Fatalf("textDocument/diagnostic issued %d times after second read, want 2", n)
	}
}

// TestCallHierarchyBothDirections covers Tier 2 call hierarchy end to end (§14):
// prepareCallHierarchy resolves the item, then each Direction arm resolves through
// the matching incoming/outgoing request and maps the From/To items to CallItem.
func TestCallHierarchyBothDirections(t *testing.T) {
	fs := newFakeServer()
	fs.setResult("textDocument/prepareCallHierarchy", `[{"name":"F","kind":12,"uri":"file:///ws/a.go","range":{"start":{"line":0,"character":5},"end":{"line":0,"character":6}},"selectionRange":{"start":{"line":0,"character":5},"end":{"line":0,"character":6}}}]`)
	fs.setResult("callHierarchy/incomingCalls", `[{"from":{"name":"Caller","kind":12,"detail":"pkg.Caller","uri":"file:///ws/b.go","range":{"start":{"line":2,"character":0},"end":{"line":2,"character":1}},"selectionRange":{"start":{"line":2,"character":0},"end":{"line":2,"character":1}}},"fromRanges":[{"start":{"line":3,"character":1},"end":{"line":3,"character":2}}]}]`)
	fs.setResult("callHierarchy/outgoingCalls", `[{"to":{"name":"Callee","kind":12,"uri":"file:///ws/c.go","range":{"start":{"line":4,"character":0},"end":{"line":4,"character":1}},"selectionRange":{"start":{"line":4,"character":0},"end":{"line":4,"character":1}}},"fromRanges":[{"start":{"line":5,"character":1},"end":{"line":5,"character":2}}]}]`)
	c := fs.connectClient(t, goCfg(), &stubHost{})
	path := writeFile(t, "package a\n")

	in, err := c.CallHierarchy(context.Background(), path, Position{1, 6}, Incoming)
	if err != nil {
		t.Fatalf("CallHierarchy incoming: %v", err)
	}
	if len(in) != 1 || in[0].Name != "Caller" || in[0].Detail != "pkg.Caller" {
		t.Fatalf("incoming calls: %+v", in)
	}
	if in[0].Kind != "function" || filepath.Base(in[0].Location.Path) != "b.go" {
		t.Fatalf("incoming call item mapping: %+v", in[0])
	}
	if in[0].Location.Range.Start.Line != 3 {
		t.Fatalf("incoming call range should be 1-based at the edge, got %+v", in[0].Location.Range)
	}

	out, err := c.CallHierarchy(context.Background(), path, Position{1, 6}, Outgoing)
	if err != nil {
		t.Fatalf("CallHierarchy outgoing: %v", err)
	}
	if len(out) != 1 || out[0].Name != "Callee" || filepath.Base(out[0].Location.Path) != "c.go" {
		t.Fatalf("outgoing calls: %+v", out)
	}
}

// TestWorkspaceSymbolsBothShapes covers Tier 2 workspace symbols and the
// SymbolInformation vs WorkspaceSymbol(uri-only location) union normalization
// (§7.2): both result shapes must normalize to []Symbol.
func TestWorkspaceSymbolsBothShapes(t *testing.T) {
	t.Run("symbol information", func(t *testing.T) {
		fs := newFakeServer()
		fs.setResult("workspace/symbol", `[{"name":"Foo","kind":12,"location":{"uri":"file:///ws/a.go","range":{"start":{"line":1,"character":0},"end":{"line":1,"character":3}}}}]`)
		c := fs.connectClient(t, goCfg(), &stubHost{})
		syms, err := c.WorkspaceSymbols(context.Background(), "Foo")
		if err != nil {
			t.Fatalf("WorkspaceSymbols: %v", err)
		}
		if len(syms) != 1 || syms[0].Name != "Foo" || syms[0].Kind != "function" {
			t.Fatalf("symbol-information shape not normalized: %+v", syms)
		}
		if filepath.Base(syms[0].Location.Path) != "a.go" || syms[0].Location.Range.Start.Line != 2 {
			t.Fatalf("symbol-information location: %+v", syms[0].Location)
		}
	})

	t.Run("workspace symbol uri-only", func(t *testing.T) {
		fs := newFakeServer()
		// The `data` field plus a uri-only location forces the WorkspaceSymbol arm.
		fs.setResult("workspace/symbol", `[{"name":"Bar","kind":5,"location":{"uri":"file:///ws/b.go"},"data":{"id":1}}]`)
		c := fs.connectClient(t, goCfg(), &stubHost{})
		syms, err := c.WorkspaceSymbols(context.Background(), "Bar")
		if err != nil {
			t.Fatalf("WorkspaceSymbols: %v", err)
		}
		if len(syms) != 1 || syms[0].Name != "Bar" || syms[0].Kind != "class" {
			t.Fatalf("workspace-symbol shape not normalized: %+v", syms)
		}
		if filepath.Base(syms[0].Location.Path) != "b.go" {
			t.Fatalf("workspace-symbol uri-only location: %+v", syms[0].Location)
		}
	})
}
