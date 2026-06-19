package gogent

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"gogent/internal/tool"
)

// seedDiagnosticsModule writes a minimal Go module into workspace with the
// given main.go source, so the registered diagnostics tool has something real
// to compile/vet.
func seedDiagnosticsModule(t *testing.T, workspace, mainSrc string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(workspace, "go.mod"),
		[]byte("module example.com/diagtest\n\ngo 1.21\n"), 0o644); err != nil {
		t.Fatalf("write go.mod: %v", err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "main.go"), []byte(mainSrc), 0o644); err != nil {
		t.Fatalf("write main.go: %v", err)
	}
}

// skipIfGoMissing skips when the go toolchain is unavailable, since the default
// diagnostics command (`go vet ./...`) and these fixtures rely on it.
func skipIfGoMissing(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain not installed")
	}
}

// TestDiagnosticsToolRegistered confirms the diagnostics tool is advertised to
// the model (registered and enabled).
func TestDiagnosticsToolRegistered(t *testing.T) {
	g, _ := newCheckpointGogent(t)
	reg := g.GetToolRegistry()
	if reg.Get("diagnostics") == nil {
		t.Fatal("diagnostics tool is not registered")
	}
	for _, tl := range reg.ListEnabled() {
		if tl.Name == "diagnostics" {
			return
		}
	}
	t.Error("diagnostics tool is registered but not enabled")
}

// TestDiagnosticsTool drives the registered tool via the registry exactly as a
// model call would, against a clean and a broken Go module.
func TestDiagnosticsTool(t *testing.T) {
	skipIfGoMissing(t)

	t.Run("clean module", func(t *testing.T) {
		g, workspace := newCheckpointGogent(t)
		seedDiagnosticsModule(t, workspace, "package main\n\nfunc main() {}\n")

		out := callTool(t, g, "diagnostics", map[string]interface{}{})
		if out["ok"] != true {
			t.Errorf("ok: got %v want true (clean module); output=%v", out["ok"], out["output"])
		}
		if out["count"] != 0 {
			diags, _ := out["diagnostics"].([]map[string]interface{})
			t.Errorf("count: got %v want 0; diagnostics=%+v", out["count"], diags)
		}
	})

	t.Run("broken module reports the error", func(t *testing.T) {
		g, workspace := newCheckpointGogent(t)
		seedDiagnosticsModule(t, workspace, "package main\n\nimport \"fmt\"\n\nfunc main() {\n\tfmt.Println(undef)\n}\n")

		out := callTool(t, g, "diagnostics", map[string]interface{}{})
		if out["ok"] != false {
			t.Errorf("ok: got %v want false (broken module)", out["ok"])
		}
		diags, ok := out["diagnostics"].([]map[string]interface{})
		if !ok {
			t.Fatalf("diagnostics: want []map, got %T", out["diagnostics"])
		}
		var found bool
		for _, d := range diags {
			if d["path"] == "main.go" && strings.Contains(d["message"].(string), "undef") {
				if d["severity"] != "error" {
					t.Errorf("severity: got %v want error", d["severity"])
				}
				found = true
			}
		}
		if !found {
			t.Errorf("want a diagnostic in main.go mentioning undef, got %+v", diags)
		}
	})
}

// TestDiagnosticsToolGated confirms the run is gated through ActionDiagnostics:
// without an allow rule (and no interactive prompter), the call is refused and
// the compiler is never reached.
func TestDiagnosticsToolGated(t *testing.T) {
	// A fresh Gogent only auto-allows workspace read/write; diagnostics falls
	// through to "ask", which denies when headless (no prompter).
	g := NewGogentWithWorkspace(t.TempDir(), t.TempDir())

	resp, err := g.GetToolRegistry().ExecuteToolCall(
		&tool.ToolCall{Tool: "diagnostics", Args: map[string]interface{}{}},
		tool.ToolContext{SessionID: "diag"},
	)
	if err == nil {
		t.Fatal("want a permission error, got nil")
	}
	if resp.Success {
		t.Errorf("want the call refused, got success: %+v", resp.Result)
	}
	if !strings.Contains(resp.Error, "diagnostics") {
		t.Errorf("error: got %q, want it to mention diagnostics", resp.Error)
	}
}
