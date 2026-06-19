package gogent

import (
	"os"
	"path/filepath"
	"testing"

	"gogent/internal/fileops"
	"gogent/internal/permission"
	"gogent/internal/tool"
)

// newCheckpointGogent builds a Gogent rooted at a temp workspace with an
// allow-all permission rule, the configuration used by the checkpoint tests.
func newCheckpointGogent(t *testing.T) (*Gogent, string) {
	t.Helper()
	tempDir := t.TempDir()
	workspace := filepath.Join(tempDir, "ws")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatalf("mkdir workspace: %v", err)
	}
	g := NewGogentWithWorkspace(tempDir, workspace)
	g.GetPermissionService().AddRule(permission.Rule{
		Action:   "*",
		Resource: "*",
		Effect:   string(permission.EffectAllow),
	})
	return g, workspace
}

// runTurn drives one agent turn: bracket the tool calls with Begin/Commit so the
// snapshots accumulate exactly as SendMessageToSessionWithModel arranges them.
func runTurn(g *Gogent, sessionID string, fn func()) {
	g.GetCheckpointer().BeginTurn(sessionID)
	fn()
	g.GetCheckpointer().CommitTurn(sessionID)
}

func ckWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func ckRead(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

func ckExists(t *testing.T, path string) bool {
	t.Helper()
	_, err := os.Stat(path)
	if err == nil {
		return true
	}
	if os.IsNotExist(err) {
		return false
	}
	t.Fatalf("stat %s: %v", path, err)
	return false
}

// TestCheckpointUndoWriteTool drives the real "write" tool: a turn that
// overwrites and creates files is fully reverted by UndoLastTurn.
func TestCheckpointUndoWriteTool(t *testing.T) {
	g, workspace := newCheckpointGogent(t)
	writeTool := g.GetToolRegistry().Get("write")
	if writeTool == nil {
		t.Fatal("write tool not registered")
	}
	ctx := tool.ToolContext{SessionID: "s1", PermissionService: g.GetPermissionService()}

	existing := filepath.Join(workspace, "existing.txt")
	ckWrite(t, existing, "before")

	runTurn(g, "s1", func() {
		if _, err := writeTool.Execute(map[string]any{"path": "existing.txt", "content": "after"}, ctx); err != nil {
			t.Fatalf("write existing: %v", err)
		}
		if _, err := writeTool.Execute(map[string]any{"path": "created.txt", "content": "new"}, ctx); err != nil {
			t.Fatalf("write created: %v", err)
		}
	})

	if got := ckRead(t, existing); got != "after" {
		t.Fatalf("existing = %q, want after", got)
	}
	if !ckExists(t, filepath.Join(workspace, "created.txt")) {
		t.Fatal("created file missing after turn")
	}
	if g.CheckpointCount("s1") != 1 {
		t.Fatalf("count = %d, want 1", g.CheckpointCount("s1"))
	}

	summary, err := g.UndoLastTurn("s1")
	if err != nil {
		t.Fatalf("undo: %v", err)
	}
	if summary == "" {
		t.Fatal("undo summary empty")
	}
	if got := ckRead(t, existing); got != "before" {
		t.Fatalf("existing after undo = %q, want before", got)
	}
	if ckExists(t, filepath.Join(workspace, "created.txt")) {
		t.Fatal("created file should be removed after undo")
	}
}

// TestCheckpointUndoEditTool drives the real "edit" tool and reverts it.
func TestCheckpointUndoEditTool(t *testing.T) {
	g, workspace := newCheckpointGogent(t)
	editTool := g.GetToolRegistry().Get("edit")
	ctx := tool.ToolContext{SessionID: "s1", PermissionService: g.GetPermissionService()}

	p := filepath.Join(workspace, "f.txt")
	ckWrite(t, p, "alpha beta gamma")

	runTurn(g, "s1", func() {
		_, err := editTool.Execute(map[string]any{
			"path":    "f.txt",
			"find":    "beta",
			"replace": "BETA",
		}, ctx)
		if err != nil {
			t.Fatalf("edit: %v", err)
		}
	})
	if got := ckRead(t, p); got != "alpha BETA gamma" {
		t.Fatalf("after edit = %q", got)
	}

	if _, err := g.UndoLastTurn("s1"); err != nil {
		t.Fatalf("undo: %v", err)
	}
	if got := ckRead(t, p); got != "alpha beta gamma" {
		t.Fatalf("after undo = %q, want original", got)
	}
}

// TestCheckpointRewindTurns verifies Rewind across multiple real write-tool turns,
// including the oldest-wins merge for a shared file.
func TestCheckpointRewindTurns(t *testing.T) {
	g, workspace := newCheckpointGogent(t)
	writeTool := g.GetToolRegistry().Get("write")
	ctx := tool.ToolContext{SessionID: "s1", PermissionService: g.GetPermissionService()}

	shared := filepath.Join(workspace, "shared.txt")
	ckWrite(t, shared, "base")

	runTurn(g, "s1", func() {
		if _, err := writeTool.Execute(map[string]any{"path": "shared.txt", "content": "t1"}, ctx); err != nil {
			t.Fatalf("write shared: %v", err)
		}
	})
	runTurn(g, "s1", func() {
		if _, err := writeTool.Execute(map[string]any{"path": "shared.txt", "content": "t2"}, ctx); err != nil {
			t.Fatalf("write shared: %v", err)
		}
		if _, err := writeTool.Execute(map[string]any{"path": "only2.txt", "content": "two"}, ctx); err != nil {
			t.Fatalf("write only2: %v", err)
		}
	})

	if g.CheckpointCount("s1") != 2 {
		t.Fatalf("count = %d, want 2", g.CheckpointCount("s1"))
	}

	if _, err := g.Rewind("s1", 0); err != nil {
		t.Fatalf("rewind: %v", err)
	}
	if got := ckRead(t, shared); got != "base" {
		t.Fatalf("shared after rewind = %q, want base", got)
	}
	if ckExists(t, filepath.Join(workspace, "only2.txt")) {
		t.Fatal("only2.txt should be removed after full rewind")
	}
	if g.CheckpointCount("s1") != 0 {
		t.Fatalf("count after rewind = %d, want 0", g.CheckpointCount("s1"))
	}
}

// TestCheckpointUndoNoHistory verifies UndoLastTurn on a session with no
// checkpoints returns a non-nil error and changes nothing.
func TestCheckpointUndoNoHistory(t *testing.T) {
	g, workspace := newCheckpointGogent(t)
	p := filepath.Join(workspace, "f.txt")
	ckWrite(t, p, "keep")

	summary, err := g.UndoLastTurn("s1")
	if err == nil {
		t.Fatal("undo with no history should error")
	}
	if summary != "" {
		t.Fatalf("summary should be empty on error, got %q", summary)
	}
	if got := ckRead(t, p); got != "keep" {
		t.Fatalf("file changed by failed undo: %q", got)
	}
}

// TestSnapshotIsBestEffort ensures checkpointing never blocks a write even when
// the file system cannot resolve a path: the bad path is skipped and the good
// write still succeeds and is undoable.
func TestSnapshotHandlesMissingTurn(t *testing.T) {
	g, workspace := newCheckpointGogent(t)
	writeTool := g.GetToolRegistry().Get("write")
	ctx := tool.ToolContext{SessionID: "s1", PermissionService: g.GetPermissionService()}

	// A snapshot taken with no turn in progress is a no-op: it must not panic or
	// record state that a later undo could misuse.
	g.GetCheckpointer().Snapshot("s1", "a.txt", fileops.Authorization{})

	p := filepath.Join(workspace, "a.txt")
	ckWrite(t, p, "orig")
	runTurn(g, "s1", func() {
		if _, err := writeTool.Execute(map[string]any{"path": "a.txt", "content": "new"}, ctx); err != nil {
			t.Fatalf("write a.txt: %v", err)
		}
	})
	if _, err := g.UndoLastTurn("s1"); err != nil {
		t.Fatalf("undo: %v", err)
	}
	if got := ckRead(t, p); got != "orig" {
		t.Fatalf("after undo = %q, want orig", got)
	}
}
