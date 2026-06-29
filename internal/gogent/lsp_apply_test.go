package gogent

import (
	"path/filepath"
	"testing"

	"gogent/internal/lsp"
)

// newLSPHost builds an lspHost over a temp-workspace Gogent with an allow-all
// permission rule, so the host's ApplyEdit exercises the real fileops write and
// Checkpointer machinery.
func newLSPHost(t *testing.T) (*lspHost, string, *Gogent) {
	t.Helper()
	g, ws := newCheckpointGogent(t)
	return &lspHost{g: g, settings: map[string]map[string]any{}}, ws, g
}

// insertAt builds a single pure-insertion text edit at the start of the document
// (1-based line/column 1,1), which applyTextEdits resolves to byte offset 0.
func insertAt(text string) []lsp.TextEdit {
	return []lsp.TextEdit{{
		Range:   lsp.Range{Start: lsp.Position{Line: 1, Character: 1}, End: lsp.Position{Line: 1, Character: 1}},
		NewText: text,
	}}
}

// TestLSPApplyMultiFileEditUndo applies text edits across two files through the
// host and confirms UndoLastTurn restores both to their pre-edit state (§12, §14).
func TestLSPApplyMultiFileEditUndo(t *testing.T) {
	h, ws, g := newLSPHost(t)
	f1 := filepath.Join(ws, "a.go")
	f2 := filepath.Join(ws, "b.go")
	ckWrite(t, f1, "package a\n")
	ckWrite(t, f2, "package b\n")

	edit := lsp.WorkspaceEdit{Changes: map[string][]lsp.TextEdit{
		f1: insertAt("// edited a\n"),
		f2: insertAt("// edited b\n"),
	}}
	applied, reason, err := h.ApplyEdit("fake", edit)
	if err != nil || !applied {
		t.Fatalf("ApplyEdit = (%v, %q, %v), want applied", applied, reason, err)
	}
	if got := ckRead(t, f1); got != "// edited a\npackage a\n" {
		t.Fatalf("f1 = %q", got)
	}
	if got := ckRead(t, f2); got != "// edited b\npackage b\n" {
		t.Fatalf("f2 = %q", got)
	}

	if _, err := g.GetCheckpointer().UndoLastTurn(lspSession); err != nil {
		t.Fatalf("undo: %v", err)
	}
	if got := ckRead(t, f1); got != "package a\n" {
		t.Fatalf("f1 after undo = %q, want original", got)
	}
	if got := ckRead(t, f2); got != "package b\n" {
		t.Fatalf("f2 after undo = %q, want original", got)
	}
}

// TestLSPApplyRenameUndo applies a RenameFile resource op and confirms undo
// re-creates the source and removes the target — proving ApplyEdit snapshotted both
// ends of the rename (§12, §14).
func TestLSPApplyRenameUndo(t *testing.T) {
	h, ws, g := newLSPHost(t)
	src := filepath.Join(ws, "src.go")
	dst := filepath.Join(ws, "dst.go")
	ckWrite(t, src, "package src\n")

	edit := lsp.WorkspaceEdit{
		ResourceOps: []lsp.ResourceOp{{Kind: "rename", Path: src, NewPath: dst}},
		Ordered:     []lsp.DocumentChange{{Kind: lsp.ChangeRename, Path: src, NewPath: dst}},
	}
	applied, reason, err := h.ApplyEdit("fake", edit)
	if err != nil || !applied {
		t.Fatalf("ApplyEdit = (%v, %q, %v), want applied", applied, reason, err)
	}
	if ckExists(t, src) {
		t.Fatal("source should be gone after rename")
	}
	if got := ckRead(t, dst); got != "package src\n" {
		t.Fatalf("dst after rename = %q", got)
	}

	if _, err := g.GetCheckpointer().UndoLastTurn(lspSession); err != nil {
		t.Fatalf("undo: %v", err)
	}
	if !ckExists(t, src) {
		t.Fatal("source should be restored after undo")
	}
	if got := ckRead(t, src); got != "package src\n" {
		t.Fatalf("restored source = %q", got)
	}
	if ckExists(t, dst) {
		t.Fatal("rename target should be removed after undo")
	}
}

// TestLSPApplyDeleteUndo applies a DeleteFile resource op and confirms undo
// re-creates the deleted file with its original content (§12, §14).
func TestLSPApplyDeleteUndo(t *testing.T) {
	h, ws, g := newLSPHost(t)
	del := filepath.Join(ws, "del.go")
	ckWrite(t, del, "package del\n")

	edit := lsp.WorkspaceEdit{
		ResourceOps: []lsp.ResourceOp{{Kind: "delete", Path: del}},
		Ordered:     []lsp.DocumentChange{{Kind: lsp.ChangeDelete, Path: del}},
	}
	applied, reason, err := h.ApplyEdit("fake", edit)
	if err != nil || !applied {
		t.Fatalf("ApplyEdit = (%v, %q, %v), want applied", applied, reason, err)
	}
	if ckExists(t, del) {
		t.Fatal("file should be gone after delete")
	}

	if _, err := g.GetCheckpointer().UndoLastTurn(lspSession); err != nil {
		t.Fatalf("undo: %v", err)
	}
	if got := ckRead(t, del); got != "package del\n" {
		t.Fatalf("deleted file after undo = %q, want original", got)
	}
}

// TestLSPApplyCreateThenEdit is the create-truncation regression (review finding):
// a documentChanges edit that creates a file and THEN supplies its content must end
// with the content intact (the create must not truncate the just-written text), and
// undo must remove the created file (§12).
func TestLSPApplyCreateThenEdit(t *testing.T) {
	h, ws, g := newLSPHost(t)
	newf := filepath.Join(ws, "new.go")

	edit := lsp.WorkspaceEdit{
		ResourceOps: []lsp.ResourceOp{{Kind: "create", Path: newf}},
		Changes:     map[string][]lsp.TextEdit{newf: insertAt("package newpkg\n")},
		Ordered: []lsp.DocumentChange{
			{Kind: lsp.ChangeCreate, Path: newf},
			{Kind: lsp.ChangeText, Path: newf, Edits: insertAt("package newpkg\n")},
		},
	}
	applied, reason, err := h.ApplyEdit("fake", edit)
	if err != nil || !applied {
		t.Fatalf("ApplyEdit = (%v, %q, %v), want applied", applied, reason, err)
	}
	if got := ckRead(t, newf); got != "package newpkg\n" {
		t.Fatalf("created file content = %q, want it not truncated by the create op", got)
	}

	if _, err := g.GetCheckpointer().UndoLastTurn(lspSession); err != nil {
		t.Fatalf("undo: %v", err)
	}
	if ckExists(t, newf) {
		t.Fatal("created file should be removed after undo")
	}
}
