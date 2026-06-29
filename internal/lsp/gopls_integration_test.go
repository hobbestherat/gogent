package lsp

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// findGopls resolves the gopls binary from PATH or the GOPATH/bin fallback, so
// the integration test RUNS when gopls is installed (even if ~/go/bin is not on
// the test shell's PATH) and SKIPS cleanly when it is genuinely absent.
func findGopls(t *testing.T) string {
	t.Helper()
	if p, err := exec.LookPath("gopls"); err == nil {
		return p
	}
	out, err := exec.Command("go", "env", "GOPATH").Output()
	if err == nil {
		p := filepath.Join(strings.TrimSpace(string(out)), "bin", "gopls")
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	t.Skip("gopls not found on PATH or in GOPATH/bin; skipping LSP integration test")
	return ""
}

// newGoModule writes a minimal Go module with one source file and returns the
// module dir and the file path.
func newGoModule(t *testing.T, src string) (dir, file string) {
	t.Helper()
	dir = t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example.com/x\n\ngo 1.21\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	file = filepath.Join(dir, "a.go")
	if err := os.WriteFile(file, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir, file
}

// pollDiagnostics calls Diagnostics until match returns true or the deadline
// passes; gopls indexes a fresh module asynchronously, so the first pushes can lag
// the initial open.
func pollDiagnostics(t *testing.T, c *Client, file string, budget time.Duration, match func([]Diagnostic) bool) []Diagnostic {
	t.Helper()
	deadline := time.Now().Add(budget)
	var last []Diagnostic
	for time.Now().Before(deadline) {
		diags, err := c.Diagnostics(context.Background(), file)
		if err != nil {
			t.Fatalf("Diagnostics: %v", err)
		}
		last = diags
		if match(diags) {
			return diags
		}
		time.Sleep(300 * time.Millisecond)
	}
	return last
}

// TestGoplsEndToEnd exercises the real gopls server end to end: the initialize
// handshake (including post-initialized dynamic registration), version-keyed
// diagnostics that reflect a live error and clear after a fix, and a hover. It is
// the PoC checklist's core (the LSP support design §14).
func TestGoplsEndToEnd(t *testing.T) {
	gopls := findGopls(t)

	const broken = "package x\n\nfunc F() int {\n\treturn bogusUndefinedSymbol\n}\n"
	// The fixed source has F call G so definition/references/rename have real
	// cross-symbol targets to resolve against the live server (§14).
	const fixed = "package x\n\nfunc F() int {\n\treturn G()\n}\n\nfunc G() int {\n\treturn 1\n}\n"
	dir, file := newGoModule(t, broken)

	cfg := ServerConfig{
		Name:        "gopls",
		LanguageID:  "go",
		Extensions:  []string{".go"},
		Command:     gopls,
		Args:        []string{"serve"},
		RootMarkers: []string{"go.mod"},
	}
	mgr := NewManager(dir, []ServerConfig{cfg}, &stubHost{})
	t.Cleanup(mgr.Shutdown)

	c, err := mgr.ClientForFile(context.Background(), file)
	if err != nil {
		t.Fatalf("ClientForFile: %v", err)
	}

	// Live error: the undefined symbol must surface as a diagnostic.
	diags := pollDiagnostics(t, c, file, 30*time.Second, func(d []Diagnostic) bool { return len(d) > 0 })
	if len(diags) == 0 {
		t.Fatal("expected at least one diagnostic for the undefined symbol")
	}
	found := false
	for _, d := range diags {
		if strings.Contains(strings.ToLower(d.Message), "undefined") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected an 'undefined' diagnostic, got %+v", diags)
	}

	// Fix the file out-of-band: re-sync-on-access must pick up the new content and
	// the diagnostics must clear.
	if err := os.WriteFile(file, []byte(fixed), 0o644); err != nil {
		t.Fatal(err)
	}
	cleared := pollDiagnostics(t, c, file, 30*time.Second, func(d []Diagnostic) bool { return len(d) == 0 })
	if len(cleared) != 0 {
		t.Fatalf("expected diagnostics to clear after the fix, got %+v", cleared)
	}

	// Hover on the function name returns a signature. gopls answers hover only after
	// it has indexed the package, so poll briefly, but the contents must ultimately
	// be non-empty — a t.Log here could not regression-detect a broken hover (§14).
	var hover Hover
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		hover, err = c.Hover(context.Background(), file, Position{Line: 3, Character: 6})
		if err != nil {
			t.Fatalf("Hover: %v", err)
		}
		if strings.TrimSpace(hover.Contents) != "" {
			break
		}
		time.Sleep(300 * time.Millisecond)
	}
	if strings.TrimSpace(hover.Contents) == "" {
		t.Fatal("expected non-empty hover contents for func F")
	}

	// Document symbols return a tree containing F.
	syms, err := c.DocumentSymbols(context.Background(), file)
	if err != nil {
		t.Fatalf("DocumentSymbols: %v", err)
	}
	hasF := false
	for _, s := range syms {
		if s.Name == "F" {
			hasF = true
		}
	}
	if !hasF {
		t.Fatalf("expected document symbol F, got %+v", syms)
	}

	// Definition: the G() call inside F (line 4, col 9) resolves to G's declaration.
	locs, err := c.Definition(context.Background(), file, Position{Line: 4, Character: 9}, DefDefinition)
	if err != nil {
		t.Fatalf("Definition: %v", err)
	}
	if len(locs) == 0 || filepath.Base(locs[0].Path) != "a.go" {
		t.Fatalf("definition of G: %+v", locs)
	}

	// References: G is declared and called once; with the declaration included there
	// must be at least two locations.
	refs, err := c.References(context.Background(), file, Position{Line: 7, Character: 6}, true)
	if err != nil {
		t.Fatalf("References: %v", err)
	}
	if len(refs) < 2 {
		t.Fatalf("references of G: want >= 2, got %+v", refs)
	}

	// Tier 3 preview against the real server: renaming G must yield a non-empty
	// WorkspaceEdit touching this file (declaration + call site).
	edit, err := c.Rename(context.Background(), file, Position{Line: 7, Character: 6}, "G2")
	if err != nil {
		t.Fatalf("Rename: %v", err)
	}
	if len(edit.Changes) == 0 && len(edit.Ordered) == 0 {
		t.Fatalf("rename produced an empty WorkspaceEdit: %+v", edit)
	}
}
