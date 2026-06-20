package gogent

import (
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"

	"gogent/internal/tool"
)

// seedSearchFixture writes a small file tree into workspace for the search-tool
// tests:
//
//	<a.go>      package main + Foo/Bar funcs
//	sub/b.go    package sub + Foo func
//	sub/note.md "# Foo" / "foo bar"
func seedSearchFixture(t *testing.T, workspace string) {
	t.Helper()
	write := func(rel, content string) {
		t.Helper()
		full := filepath.Join(workspace, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", filepath.Dir(full), err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", rel, err)
		}
	}
	write("a.go", "package main\n\nfunc Foo() {}\nfunc Bar() {}\n")
	write("sub/b.go", "package sub\n\nfunc Foo() {}\n")
	write("sub/note.md", "# Foo\nfoo bar\n")
}

// callTool runs a registered tool through the registry so its arguments are
// validated and counted exactly as a model call would be.
func callTool(t *testing.T, g *Gogent, name string, args map[string]interface{}) map[string]interface{} {
	t.Helper()
	resp, err := g.GetToolRegistry().ExecuteToolCall(&tool.ToolCall{Tool: name, Args: args}, tool.ToolContext{SessionID: "search"})
	if err != nil {
		t.Fatalf("%s ExecuteToolCall: %v", name, err)
	}
	if !resp.Success {
		t.Fatalf("%s failed: %s", name, resp.Error)
	}
	out, ok := resp.Result.(map[string]interface{})
	if !ok {
		t.Fatalf("%s returned %T, want map", name, resp.Result)
	}
	return out
}

// TestSearchToolsRegistered confirms grep/glob/list are advertised to the model
// (they appear in the enabled tool set).
func TestSearchToolsRegistered(t *testing.T) {
	g, _ := newCheckpointGogent(t)
	reg := g.GetToolRegistry()
	for _, name := range []string{"grep", "glob", "list"} {
		if reg.Get(name) == nil {
			t.Errorf("tool %q is not registered", name)
		}
	}
	enabled := make(map[string]bool, 64)
	for _, tl := range reg.ListEnabled() {
		enabled[tl.Name] = true
	}
	for _, name := range []string{"grep", "glob", "list"} {
		if !enabled[name] {
			t.Errorf("tool %q is registered but not enabled", name)
		}
	}
}

// TestGrepTool drives the registered "grep" tool across its output modes.
func TestGrepTool(t *testing.T) {
	g, workspace := newCheckpointGogent(t)
	seedSearchFixture(t, workspace)

	t.Run("content", func(t *testing.T) {
		out := callTool(t, g, "grep", map[string]interface{}{
			"pattern": "Foo",
		})
		if out["mode"] != "content" {
			t.Errorf("mode: got %v want content", out["mode"])
		}
		matches, ok := out["matches"].([]map[string]interface{})
		if !ok {
			t.Fatalf("matches: want []map, got %T", out["matches"])
		}
		// a.go:3, sub/b.go:3, sub/note.md:1
		type ref struct {
			path string
			line int
		}
		got := make([]ref, 0, len(matches))
		for _, m := range matches {
			line, _ := m["line"].(int)
			got = append(got, ref{m["path"].(string), line})
		}
		want := []ref{{"a.go", 3}, {"sub/b.go", 3}, {"sub/note.md", 1}}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("matches: got %+v want %+v", got, want)
		}
		if out["count"] != len(matches) {
			t.Errorf("count %v != len(matches) %d", out["count"], len(matches))
		}
	})

	t.Run("files_with_matches", func(t *testing.T) {
		out := callTool(t, g, "grep", map[string]interface{}{
			"pattern":     "Foo",
			"output_mode": "files_with_matches",
		})
		files, ok := out["files"].([]string)
		if !ok {
			t.Fatalf("files: want []string, got %T", out["files"])
		}
		want := []string{"a.go", "sub/b.go", "sub/note.md"}
		if !reflect.DeepEqual(files, want) {
			t.Errorf("files: got %v want %v", files, want)
		}
	})

	t.Run("count", func(t *testing.T) {
		out := callTool(t, g, "grep", map[string]interface{}{
			"pattern":     "func",
			"output_mode": "count",
		})
		counts, ok := out["counts"].([]map[string]interface{})
		if !ok {
			t.Fatalf("counts: want []map, got %T", out["counts"])
		}
		// a.go has 2 (Foo, Bar), sub/b.go has 1.
		total, ok := out["total"].(int)
		if !ok || total != 3 {
			t.Errorf("total: got %v (%T) want 3", out["total"], out["total"])
		}
		if len(counts) != 2 {
			t.Errorf("counts entries: got %d want 2", len(counts))
		}
	})

	t.Run("include filter", func(t *testing.T) {
		out := callTool(t, g, "grep", map[string]interface{}{
			"pattern":     "Foo",
			"output_mode": "files_with_matches",
			"include":     "*.go",
		})
		files, _ := out["files"].([]string)
		want := []string{"a.go", "sub/b.go"}
		if !reflect.DeepEqual(files, want) {
			t.Errorf("files: got %v want %v", files, want)
		}
	})

	t.Run("rejects missing pattern", func(t *testing.T) {
		resp, err := g.GetToolRegistry().ExecuteToolCall(&tool.ToolCall{
			Tool: "grep", Args: map[string]interface{}{},
		}, tool.ToolContext{SessionID: "search"})
		if err != nil {
			t.Fatalf("ExecuteToolCall: %v", err)
		}
		if resp.Success {
			t.Errorf("want failure for missing pattern, got success: %+v", resp.Result)
		}
	})
}

// TestGlobTool drives the registered "glob" tool.
func TestGlobTool(t *testing.T) {
	g, workspace := newCheckpointGogent(t)
	seedSearchFixture(t, workspace)

	out := callTool(t, g, "glob", map[string]interface{}{"pattern": "*.go"})
	matches, ok := out["matches"].([]string)
	if !ok {
		t.Fatalf("matches: want []string, got %T", out["matches"])
	}
	// Only top-level .go files match a non-recursive glob.
	want := []string{"a.go"}
	if !reflect.DeepEqual(matches, want) {
		t.Errorf("matches: got %v want %v", matches, want)
	}

	// Missing pattern must fail validation rather than panic.
	resp, err := g.GetToolRegistry().ExecuteToolCall(&tool.ToolCall{
		Tool: "glob", Args: map[string]interface{}{},
	}, tool.ToolContext{SessionID: "search"})
	if err != nil {
		t.Fatalf("ExecuteToolCall: %v", err)
	}
	if resp.Success {
		t.Error("want failure for missing pattern, got success")
	}
}

// TestListTool drives the registered "list" tool.
func TestListTool(t *testing.T) {
	g, workspace := newCheckpointGogent(t)
	seedSearchFixture(t, workspace)

	out := callTool(t, g, "list", map[string]interface{}{"path": "sub"})
	entries, ok := out["entries"].([]map[string]interface{})
	if !ok {
		t.Fatalf("entries: want []map, got %T", out["entries"])
	}

	var names []string
	for _, e := range entries {
		names = append(names, e["name"].(string))
	}
	sort.Strings(names)
	want := []string{"b.go", "note.md"}
	if !reflect.DeepEqual(names, want) {
		t.Errorf("names: got %v want %v", names, want)
	}

	// list defaults to the workspace root when no path is given.
	rootOut := callTool(t, g, "list", map[string]interface{}{})
	if rootOut["path"] != "." {
		t.Errorf("default path: got %v want .", rootOut["path"])
	}
	rootEntries, _ := rootOut["entries"].([]map[string]interface{})
	if len(rootEntries) == 0 {
		t.Error("expected entries at workspace root")
	}
}
