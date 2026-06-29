package lsp

import (
	"context"
	"testing"
	"time"
)

// TestFreshnessIdleGatedByVersioning pins the §11.4 ranking: the work-done idle
// signal is only a fallback "for servers that omit the version field," so it must
// not settle a read for a server that DOES version its pushes (gopls). Otherwise a
// momentarily-idle server could settle a post-edit read on the stale prior version
// while the awaited version is still pending — weakening the design's central
// "version-correlated, not stale" promise for the edit-then-recheck loop.
func TestFreshnessIdleGatedByVersioning(t *testing.T) {
	restore := freshnessCeiling
	freshnessCeiling = 120 * time.Millisecond
	defer func() { freshnessCeiling = restore }()

	s := newDiagnosticsStore()
	const path = "/ws/a.go"

	// The server versioned a push at version 1 (so sawVersioned is set); that push is
	// older than the debounce window and the server is idle. Then an edit bumped the
	// document to version 2 (awaited), for which no push has yet arrived.
	s.mu.Lock()
	e := s.entry(path)
	e.diags = []Diagnostic{{Message: "stale-from-v1", Severity: 1}}
	e.version = 1
	e.hasVersion = true
	e.pushSeq = 1
	e.lastPush = time.Now().Add(-time.Second) // debounce already satisfied
	e.awaited = 2                             // an edit bumped the awaited version
	s.sawVersioned = true                     // this server versions its pushes
	s.mu.Unlock()

	// Before the gating fix, idleFallback (pushSeq>0 && idle) would settle this on the
	// stale v1 set. With the fix the read stays uncorrelated and the ceiling fires,
	// so settled is false (a pull-capable caller would then re-pull).
	if _, settled := s.wait(path); settled {
		t.Fatal("a versioning server must not settle a post-edit read on the stale prior version via the idle fallback")
	}
}

// TestCodeActionBareCommandArm covers the §7.2/§12 union normalization of a
// textDocument/codeAction response shaped as a bare Command[] — the arm a server
// that does not honor codeActionLiteralSupport returns. It must normalize to a
// CodeAction that carries the command name and no edit, so the tool layer surfaces
// it as a gated execute-command candidate rather than a (missing) edit, never
// running it implicitly.
func TestCodeActionBareCommandArm(t *testing.T) {
	fs := newFakeServer()
	fs.setResult("textDocument/codeAction", `[{"title":"Run X","command":"cmd.id","arguments":[1]}]`)
	c := fs.connectClient(t, goCfg(), &stubHost{})
	path := writeFile(t, "package a\n")

	actions, err := c.CodeActions(context.Background(), path, Range{Position{1, 1}, Position{1, 2}})
	if err != nil {
		t.Fatalf("CodeActions: %v", err)
	}
	if len(actions) != 1 {
		t.Fatalf("want 1 action from the bare Command arm, got %d (%+v)", len(actions), actions)
	}
	if a := actions[0]; a.Command != "cmd.id" || a.Edit != nil {
		t.Fatalf("bare Command not normalized: want {Command:cmd.id, Edit:nil}, got %+v", a)
	}
}
