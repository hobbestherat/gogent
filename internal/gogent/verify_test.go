package gogent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gogent/internal/tool"
)

// seedVerifyModule writes a minimal Go module into workspace so the registered
// verify tool has a real test suite to run. files maps a file name (e.g.
// "math_test.go") to its source.
func seedVerifyModule(t *testing.T, workspace string, files map[string]string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(workspace, "go.mod"),
		[]byte("module example.com/verifytest\n\ngo 1.21\n"), 0o644); err != nil {
		t.Fatalf("write go.mod: %v", err)
	}
	for name, src := range files {
		if err := os.WriteFile(filepath.Join(workspace, name), []byte(src), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
}

const verifyPassingTest = `package main

import "testing"

func TestAdd(t *testing.T) {
	if 2+2 != 4 {
		t.Errorf("expected 4")
	}
}
`

const verifyFailingTest = `package main

import "testing"

func TestAdd(t *testing.T) {
	if 2+2 != 5 {
		t.Errorf("expected 5, got %d", 2+2)
	}
}
`

// TestVerifyToolRegistered confirms the verify tool is advertised to the model
// (registered and enabled).
func TestVerifyToolRegistered(t *testing.T) {
	g, _ := newCheckpointGogent(t)
	reg := g.GetToolRegistry()
	if reg.Get("verify") == nil {
		t.Fatal("verify tool is not registered")
	}
	for _, tl := range reg.ListEnabled() {
		if tl.Name == "verify" {
			return
		}
	}
	t.Error("verify tool is registered but not enabled")
}

// TestVerifyTool drives the registered tool via the registry exactly as a model
// call would, against a passing and a failing test suite.
func TestVerifyTool(t *testing.T) {
	skipIfGoMissing(t)

	t.Run("passing suite", func(t *testing.T) {
		g, workspace := newCheckpointGogent(t)
		seedVerifyModule(t, workspace, map[string]string{"math_test.go": verifyPassingTest})

		out := callTool(t, g, "verify", map[string]interface{}{})
		if out["pass"] != true {
			t.Errorf("pass: got %v want true; output=%v", out["pass"], out["output"])
		}
		if out["count"] != 0 {
			t.Errorf("count: got %v want 0", out["count"])
		}
	})

	t.Run("failing test reports the failure", func(t *testing.T) {
		g, workspace := newCheckpointGogent(t)
		seedVerifyModule(t, workspace, map[string]string{"math_test.go": verifyFailingTest})

		out := callTool(t, g, "verify", map[string]interface{}{})
		if out["pass"] != false {
			t.Errorf("pass: got %v want false (failing test)", out["pass"])
		}
		failures, ok := out["failures"].([]map[string]interface{})
		if !ok {
			t.Fatalf("failures: want []map, got %T", out["failures"])
		}
		var found bool
		for _, f := range failures {
			if f["test"] == "TestAdd" && strings.Contains(f["message"].(string), "expected 5") {
				if f["package"] != "example.com/verifytest" {
					t.Errorf("package: got %v want example.com/verifytest", f["package"])
				}
				found = true
			}
		}
		if !found {
			t.Errorf("want a failure for TestAdd mentioning the assertion, got %+v", failures)
		}
	})
}

// TestVerifyToolGated confirms the run is gated through ActionVerify: without an
// allow rule (and no interactive prompter), the call is refused and the suite is
// never reached.
func TestVerifyToolGated(t *testing.T) {
	// A fresh Gogent only auto-allows workspace read/write; verify falls through
	// to "ask", which denies when headless (no prompter).
	g := NewGogentWithWorkspace(t.TempDir(), t.TempDir())

	resp, err := g.GetToolRegistry().ExecuteToolCall(
		&tool.ToolCall{Tool: "verify", Args: map[string]interface{}{}},
		tool.ToolContext{SessionID: "verify"},
	)
	if err == nil {
		t.Fatal("want a permission error, got nil")
	}
	if resp.Success {
		t.Errorf("want the call refused, got success: %+v", resp.Result)
	}
	if !strings.Contains(resp.Error, "verify") {
		t.Errorf("error: got %q, want it to mention verify", resp.Error)
	}
}
