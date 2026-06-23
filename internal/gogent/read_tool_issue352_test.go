package gogent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gogent/internal/fileops"
	"gogent/internal/tool"
)

func writeIssue352File(t *testing.T, workspace, rel, content string) {
	t.Helper()
	full := filepath.Join(workspace, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(full), err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", rel, err)
	}
}

func readToolContent(t *testing.T, out map[string]interface{}) string {
	t.Helper()
	content, ok := out["content"].(string)
	if !ok {
		t.Fatalf("content has type %T, want string", out["content"])
	}
	return content
}

func TestReadToolIssue352SchemaDocumentsBounds(t *testing.T) {
	g, _ := newCheckpointGogent(t)
	read := g.GetToolRegistry().Get("read")
	if read == nil {
		t.Fatal("read tool is not registered")
	}
	if !read.ReadOnly {
		t.Fatal("read tool should remain read-only")
	}

	schema, ok := read.InputSchema.(map[string]interface{})
	if !ok {
		t.Fatalf("schema type = %T, want map", read.InputSchema)
	}
	props, ok := schema["properties"].(map[string]interface{})
	if !ok {
		t.Fatalf("properties type = %T, want map", schema["properties"])
	}
	for _, name := range []string{"path", "offset", "limit", "max_length"} {
		prop, ok := props[name].(map[string]interface{})
		if !ok {
			t.Fatalf("property %q missing or wrong type: %T", name, props[name])
		}
		desc, _ := prop["description"].(string)
		if desc == "" {
			t.Fatalf("property %q has empty description", name)
		}
	}
	for _, want := range []string{"offset", "limit", "max_length", "truncated", "total_lines", "total_bytes", "next_offset"} {
		if !strings.Contains(read.Description, want) {
			t.Fatalf("description should mention %q: %q", want, read.Description)
		}
	}
}

func TestReadToolIssue352LineRangeMaxLengthAndPaging(t *testing.T) {
	g, workspace := newCheckpointGogent(t)
	writeIssue352File(t, workspace, "notes.txt", "one\ntwo\nthree\nfour\nfive\n")

	t.Run("line range returns only requested lines", func(t *testing.T) {
		out := callTool(t, g, "read", map[string]interface{}{
			"path":       "notes.txt",
			"offset":     2,
			"limit":      2,
			"max_length": 1024,
		})
		content := readToolContent(t, out)
		if !strings.HasPrefix(content, "two\nthree\n") {
			t.Fatalf("content = %q, want requested line range prefix", content)
		}
		if strings.Contains(content, "one\n") || strings.Contains(content, "four\n") {
			t.Fatalf("content contains lines outside requested range: %q", content)
		}
		if out["offset"] != 2 || out["limit"] != 2 || out["lines_shown"] != 2 {
			t.Fatalf("range metadata = offset %v limit %v lines %v, want 2/2/2", out["offset"], out["limit"], out["lines_shown"])
		}
		if out["truncated"] != true || out["next_offset"] != 4 {
			t.Fatalf("paging = truncated %v next %v, want true/4", out["truncated"], out["next_offset"])
		}
		if out["total_lines"] != 5 || out["total_bytes"] != len("one\ntwo\nthree\nfour\nfive\n") {
			t.Fatalf("totals = lines %v bytes %v", out["total_lines"], out["total_bytes"])
		}
		if !strings.Contains(content, "truncated") || !strings.Contains(content, "offset=4") {
			t.Fatalf("truncation marker missing paging guidance: %q", content)
		}
	})

	t.Run("max_length truncates with marker", func(t *testing.T) {
		out := callTool(t, g, "read", map[string]interface{}{
			"path":       "notes.txt",
			"offset":     1,
			"limit":      5,
			"max_length": len("one\nt"),
		})
		content := readToolContent(t, out)
		if !strings.HasPrefix(content, "one\nt") {
			t.Fatalf("content = %q, want max_length capped prefix", content)
		}
		if !strings.Contains(content, "truncated") || !strings.Contains(content, "offset=2") {
			t.Fatalf("max_length truncation marker missing continuation guidance: %q", content)
		}
		if out["truncated"] != true || out["lines_shown"] != 1 || out["next_offset"] != 2 {
			t.Fatalf("metadata = truncated %v lines %v next %v, want true/1/2", out["truncated"], out["lines_shown"], out["next_offset"])
		}
	})

	t.Run("offset paging returns continuation", func(t *testing.T) {
		first := callTool(t, g, "read", map[string]interface{}{
			"path":       "notes.txt",
			"limit":      2,
			"max_length": 1024,
		})
		next, ok := first["next_offset"].(int)
		if !ok || next != 3 {
			t.Fatalf("first next_offset = %v (%T), want int 3", first["next_offset"], first["next_offset"])
		}

		second := callTool(t, g, "read", map[string]interface{}{
			"path":       "notes.txt",
			"offset":     next,
			"limit":      2,
			"max_length": 1024,
		})
		content := readToolContent(t, second)
		if !strings.HasPrefix(content, "three\nfour\n") {
			t.Fatalf("second page content = %q, want continuation from line 3", content)
		}
		if strings.Contains(content, "one\n") || strings.Contains(content, "two\n") {
			t.Fatalf("second page should not repeat previous page: %q", content)
		}
		if second["next_offset"] != 5 {
			t.Fatalf("second next_offset = %v, want 5", second["next_offset"])
		}
	})
}

func TestReadToolIssue352DefaultCapAndSmallFile(t *testing.T) {
	g, workspace := newCheckpointGogent(t)

	t.Run("small file unchanged", func(t *testing.T) {
		const content = "small\nfile\n"
		writeIssue352File(t, workspace, "small.txt", content)
		out := callTool(t, g, "read", map[string]interface{}{"path": "small.txt"})
		if got := readToolContent(t, out); got != content {
			t.Fatalf("content = %q, want unchanged %q", got, content)
		}
		if out["truncated"] != false || out["next_offset"] != 0 {
			t.Fatalf("metadata = truncated %v next %v, want false/0", out["truncated"], out["next_offset"])
		}
	})

	t.Run("default cap bounds large file", func(t *testing.T) {
		content := strings.Repeat("x\n", fileops.DefaultReadMaxLines+1)
		writeIssue352File(t, workspace, "large.txt", content)
		out := callTool(t, g, "read", map[string]interface{}{"path": "large.txt"})
		got := readToolContent(t, out)
		wantPrefix := strings.Repeat("x\n", fileops.DefaultReadMaxLines)
		if !strings.HasPrefix(got, wantPrefix) {
			t.Fatalf("default-capped content does not start with %d complete lines", fileops.DefaultReadMaxLines)
		}
		if !strings.Contains(got, "truncated") || !strings.Contains(got, "offset=2001") {
			t.Fatalf("default cap marker missing continuation guidance: %q", got[len(got)-120:])
		}
		if out["truncated"] != true || out["lines_shown"] != fileops.DefaultReadMaxLines || out["next_offset"] != fileops.DefaultReadMaxLines+1 {
			t.Fatalf("metadata = truncated %v lines %v next %v, want true/%d/%d",
				out["truncated"], out["lines_shown"], out["next_offset"], fileops.DefaultReadMaxLines, fileops.DefaultReadMaxLines+1)
		}
	})
}

func TestReadToolIssue352Errors(t *testing.T) {
	g, workspace := newCheckpointGogent(t)
	writeIssue352File(t, workspace, "exists.txt", "ok\n")

	resp, err := g.GetToolRegistry().ExecuteToolCall(&tool.ToolCall{
		Tool: "read",
		Args: map[string]interface{}{},
	}, tool.ToolContext{SessionID: "issue352"})
	if err != nil {
		t.Fatalf("missing path should be a validation response, got error: %v", err)
	}
	if resp.Success || !strings.Contains(resp.Error, "path") {
		t.Fatalf("missing path response = success %v error %q, want validation failure mentioning path", resp.Success, resp.Error)
	}

	resp, err = g.GetToolRegistry().ExecuteToolCall(&tool.ToolCall{
		Tool: "read",
		Args: map[string]interface{}{"path": "missing.txt"},
	}, tool.ToolContext{SessionID: "issue352"})
	if err == nil {
		t.Fatal("missing file should return an execution error")
	}
	if resp == nil || resp.Success || !strings.Contains(resp.Error, "failed to read file") {
		t.Fatalf("missing file response = %#v error %v, want read failure", resp, err)
	}
}
