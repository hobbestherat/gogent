package gogent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gogent/internal/tool"
)

// TestMultiEditTool drives the registered "multi_edit" tool end-to-end: several
// edits land in one call, in order.
func TestMultiEditTool(t *testing.T) {
	g, workspace := newCheckpointGogent(t)
	mt := g.GetToolRegistry().Get("multi_edit")
	if mt == nil {
		t.Fatal("multi_edit tool not registered")
	}
	ctx := tool.ToolContext{SessionID: "s1", PermissionService: g.GetPermissionService()}

	p := filepath.Join(workspace, "f.txt")
	ckWrite(t, p, "one two three\n")

	_, err := mt.Execute(map[string]any{
		"path": "f.txt",
		"edits": []interface{}{
			map[string]interface{}{"find": "one", "replace": "1"},
			map[string]interface{}{"find": "three", "replace": "3"},
		},
	}, ctx)
	if err != nil {
		t.Fatalf("multi_edit: %v", err)
	}
	if got := ckRead(t, p); got != "1 two 3\n" {
		t.Fatalf("after multi_edit = %q", got)
	}
}

// TestMultiEditToolAtomicFailure ensures a batch with one bad edit writes
// nothing.
func TestMultiEditToolAtomicFailure(t *testing.T) {
	g, workspace := newCheckpointGogent(t)
	mt := g.GetToolRegistry().Get("multi_edit")
	ctx := tool.ToolContext{SessionID: "s1", PermissionService: g.GetPermissionService()}

	p := filepath.Join(workspace, "f.txt")
	ckWrite(t, p, "a a b\n")

	_, err := mt.Execute(map[string]any{
		"path": "f.txt",
		"edits": []interface{}{
			map[string]interface{}{"find": "b", "replace": "B"},
			map[string]interface{}{"find": "a", "replace": "X"}, // non-unique → abort
		},
	}, ctx)
	if err == nil {
		t.Fatal("expected error for ambiguous edit")
	}
	if got := ckRead(t, p); got != "a a b\n" {
		t.Fatalf("file mutated by failed batch: %q", got)
	}
}

// TestApplyPatchTool drives the registered "apply_patch" tool across an add, an
// update and a delete in a single envelope.
func TestApplyPatchTool(t *testing.T) {
	g, workspace := newCheckpointGogent(t)
	pt := g.GetToolRegistry().Get("apply_patch")
	if pt == nil {
		t.Fatal("apply_patch tool not registered")
	}
	ctx := tool.ToolContext{SessionID: "s1", PermissionService: g.GetPermissionService()}

	ckWrite(t, filepath.Join(workspace, "upd.txt"), "keep\nold\ntail\n")
	ckWrite(t, filepath.Join(workspace, "del.txt"), "remove me\n")

	patch := "*** Begin Patch\n" +
		"*** Add File: new.txt\n+created\n+line2\n" +
		"*** Update File: upd.txt\n@@\n keep\n-old\n+new\n tail\n" +
		"*** Delete File: del.txt\n" +
		"*** End Patch"

	if _, err := pt.Execute(map[string]any{"patch": patch}, ctx); err != nil {
		t.Fatalf("apply_patch: %v", err)
	}

	if got := ckRead(t, filepath.Join(workspace, "new.txt")); got != "created\nline2\n" {
		t.Fatalf("added file = %q", got)
	}
	if got := ckRead(t, filepath.Join(workspace, "upd.txt")); got != "keep\nnew\ntail\n" {
		t.Fatalf("updated file = %q", got)
	}
	if _, err := os.Stat(filepath.Join(workspace, "del.txt")); !os.IsNotExist(err) {
		t.Fatalf("deleted file still present (err=%v)", err)
	}
}

// TestApplyPatchToolAtomicOnBadHunk ensures a patch whose hunk context does not
// match writes nothing at all — even the valid add earlier in the envelope.
func TestApplyPatchToolAtomicOnBadHunk(t *testing.T) {
	g, workspace := newCheckpointGogent(t)
	pt := g.GetToolRegistry().Get("apply_patch")
	ctx := tool.ToolContext{SessionID: "s1", PermissionService: g.GetPermissionService()}

	ckWrite(t, filepath.Join(workspace, "u.txt"), "real content\n")

	patch := "*** Begin Patch\n" +
		"*** Add File: should_not_exist.txt\n+nope\n" +
		"*** Update File: u.txt\n@@\n-absent line\n+x\n" +
		"*** End Patch"

	if _, err := pt.Execute(map[string]any{"patch": patch}, ctx); err == nil {
		t.Fatal("expected error for non-matching hunk")
	}
	if _, err := os.Stat(filepath.Join(workspace, "should_not_exist.txt")); !os.IsNotExist(err) {
		t.Fatalf("add was applied despite a later failing hunk (err=%v)", err)
	}
	if got := ckRead(t, filepath.Join(workspace, "u.txt")); got != "real content\n" {
		t.Fatalf("update target mutated: %q", got)
	}
}

// TestApplyPatchToolRejectsDuplicatePath guards against a patch touching the same
// file twice, which the planner cannot order safely.
func TestApplyPatchToolRejectsDuplicatePath(t *testing.T) {
	g, workspace := newCheckpointGogent(t)
	pt := g.GetToolRegistry().Get("apply_patch")
	ctx := tool.ToolContext{SessionID: "s1", PermissionService: g.GetPermissionService()}

	ckWrite(t, filepath.Join(workspace, "dup.txt"), "x\n")

	patch := "*** Begin Patch\n" +
		"*** Update File: dup.txt\n@@\n-x\n+y\n" +
		"*** Delete File: dup.txt\n" +
		"*** End Patch"

	if _, err := pt.Execute(map[string]any{"patch": patch}, ctx); err == nil {
		t.Fatal("expected error for duplicate path in patch")
	}
}

func TestPatchToolsDeclareInputExamples(t *testing.T) {
	g, _ := newCheckpointGogent(t)
	reg := g.GetToolRegistry()

	tests := []struct {
		name      string
		wantKeys  []string
		docNeedle string
	}{
		{
			name:      "multi_edit",
			wantKeys:  []string{"path", "edits"},
			docNeedle: `"edits":[{"find":"port := 8080","replace":"port := 9090"},{"find":"\"localhost\"","replace":"\"0.0.0.0\"","replace_all":true}],"path":"internal/server/server.go"`,
		},
		{
			name:      "apply_patch",
			wantKeys:  []string{"patch"},
			docNeedle: `"patch":"*** Begin Patch\n*** Update File: internal/config/config.go`,
		},
	}

	docs := reg.RenderToolDocs()
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tl := reg.Get(tt.name)
			if tl == nil {
				t.Fatalf("%s tool not registered", tt.name)
			}
			if len(tl.InputExamples) == 0 {
				t.Fatalf("%s InputExamples is empty", tt.name)
			}
			for _, key := range tt.wantKeys {
				if _, ok := tl.InputExamples[0][key]; !ok {
					t.Fatalf("%s first InputExample missing key %q: %+v", tt.name, key, tl.InputExamples[0])
				}
			}
			section := renderToolDocSection(t, docs, tt.name)
			if !strings.Contains(section, "Examples:\n") {
				t.Fatalf("%s docs missing Examples header:\n%s", tt.name, section)
			}
			if !strings.Contains(section, tt.docNeedle) {
				t.Fatalf("%s docs missing compact JSON input example %q:\n%s", tt.name, tt.docNeedle, section)
			}
		})
	}
}

func renderToolDocSection(t *testing.T, docs, name string) string {
	t.Helper()
	start := strings.Index(docs, "### "+name)
	if start == -1 {
		t.Fatalf("docs missing %s section:\n%s", name, docs)
	}
	rest := docs[start+len("### "+name):]
	if next := strings.Index(rest, "\n### "); next != -1 {
		return docs[start : start+len("### "+name)+next]
	}
	return docs[start:]
}
