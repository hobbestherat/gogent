package fileops

import (
	"os"
	"path/filepath"
	"testing"
)

// mutate simulates one agent write/edit within a turn: snapshot the path's
// pre-mutation state, then overwrite it. It mirrors the order the Gogent write
// and edit tool handlers use (snapshot after approval, before the mutation).
func mutate(c *Checkpointer, fsys *FileSystem, sessionID, path, content string) {
	c.Snapshot(sessionID, path, Authorization{})
	if err := fsys.Write(path, []byte(content), Authorization{}); err != nil {
		panic(err)
	}
}

func readTmp(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

func existsTmp(t *testing.T, path string) bool {
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

// TestUndoRestoresModifiedFile verifies a turn that overwrites an existing file
// is reverted to its pre-turn content.
func TestUndoRestoresModifiedFile(t *testing.T) {
	root := t.TempDir()
	fsys := NewFileSystem(root)
	c := NewCheckpointer(fsys)

	p := filepath.Join(root, "a.txt")
	if err := os.WriteFile(p, []byte("original"), 0o644); err != nil {
		t.Fatal(err)
	}

	c.BeginTurn("s")
	mutate(c, fsys, "s", "a.txt", "changed")
	c.CommitTurn("s")

	if got := readTmp(t, p); got != "changed" {
		t.Fatalf("after turn, got %q, want %q", got, "changed")
	}

	n, err := c.UndoLastTurn("s")
	if err != nil {
		t.Fatalf("undo: %v", err)
	}
	if n != 1 {
		t.Fatalf("restored %d files, want 1", n)
	}
	if got := readTmp(t, p); got != "original" {
		t.Fatalf("after undo, got %q, want %q", got, "original")
	}
	if c.Count("s") != 0 {
		t.Fatalf("count after undo = %d, want 0", c.Count("s"))
	}
}

// TestUndoRemovesCreatedFile verifies a turn that creates a brand-new file
// deletes it on undo (rather than leaving it empty).
func TestUndoRemovesCreatedFile(t *testing.T) {
	root := t.TempDir()
	fsys := NewFileSystem(root)
	c := NewCheckpointer(fsys)

	p := filepath.Join(root, "new.txt")
	c.BeginTurn("s")
	mutate(c, fsys, "s", "new.txt", "fresh")
	c.CommitTurn("s")

	if !existsTmp(t, p) {
		t.Fatal("file should exist after turn")
	}

	if _, err := c.UndoLastTurn("s"); err != nil {
		t.Fatalf("undo: %v", err)
	}
	if existsTmp(t, p) {
		t.Fatal("created file should be removed after undo")
	}
}

// TestFirstMutationWins covers the core invariant: within a turn, a file edited
// repeatedly is snapshotted only once (at its pre-turn state), so undo restores
// the original — not an intermediate version — regardless of the path spelling
// used by each edit.
func TestFirstMutationWins(t *testing.T) {
	root := t.TempDir()
	fsys := NewFileSystem(root)
	c := NewCheckpointer(fsys)

	p := filepath.Join(root, "f.txt")
	if err := os.WriteFile(p, []byte("v0"), 0o644); err != nil {
		t.Fatal(err)
	}

	c.BeginTurn("s")
	// Two path spellings of the same file, plus a re-edit: only the first
	// snapshot (v0) must stick.
	mutate(c, fsys, "s", "f.txt", "v1")
	mutate(c, fsys, "s", filepath.Join(root, "f.txt"), "v2")
	mutate(c, fsys, "s", "./f.txt", "v3")
	c.CommitTurn("s")

	if _, err := c.UndoLastTurn("s"); err != nil {
		t.Fatalf("undo: %v", err)
	}
	if got := readTmp(t, p); got != "v0" {
		t.Fatalf("after undo got %q, want original %q", got, "v0")
	}
}

// TestRewindMultipleTurns verifies rewind across several turns. A shared file
// edited in every turn must restore to its state before the earliest reverted
// turn (oldest snapshot wins); files touched only in one turn restore to that
// turn's pre-state.
func TestRewindMultipleTurns(t *testing.T) {
	root := t.TempDir()
	fsys := NewFileSystem(root)
	c := NewCheckpointer(fsys)

	shared := filepath.Join(root, "shared.txt")
	only2 := filepath.Join(root, "only2.txt")
	only3 := filepath.Join(root, "only3.txt")
	if err := os.WriteFile(shared, []byte("base"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Turn 1: shared = base -> t1
	c.BeginTurn("s")
	mutate(c, fsys, "s", "shared.txt", "t1")
	c.CommitTurn("s")
	// Turn 2: shared = t1 -> t2, create only2
	c.BeginTurn("s")
	mutate(c, fsys, "s", "shared.txt", "t2")
	mutate(c, fsys, "s", "only2.txt", "made-in-2")
	c.CommitTurn("s")
	// Turn 3: shared = t2 -> t3, create only3
	c.BeginTurn("s")
	mutate(c, fsys, "s", "shared.txt", "t3")
	mutate(c, fsys, "s", "only3.txt", "made-in-3")
	c.CommitTurn("s")

	if c.Count("s") != 3 {
		t.Fatalf("count = %d, want 3", c.Count("s"))
	}

	// Rewind all three: shared -> base, only2 removed, only3 removed.
	files, reverted, err := c.Rewind("s", 0)
	if err != nil {
		t.Fatalf("rewind: %v", err)
	}
	if reverted != 3 {
		t.Fatalf("reverted %d turns, want 3", reverted)
	}
	if files != 3 {
		t.Fatalf("restored %d files, want 3 (shared, only2, only3)", files)
	}
	if got := readTmp(t, shared); got != "base" {
		t.Fatalf("shared after rewind = %q, want %q", got, "base")
	}
	if existsTmp(t, only2) || existsTmp(t, only3) {
		t.Fatal("turn-2 and turn-3 created files should be gone after full rewind")
	}
	if c.Count("s") != 0 {
		t.Fatalf("count after full rewind = %d, want 0", c.Count("s"))
	}
}

// TestUndoThenRewind checks that undo and rewind compose: undo the last turn,
// then rewind an earlier one.
func TestUndoThenRewind(t *testing.T) {
	root := t.TempDir()
	fsys := NewFileSystem(root)
	c := NewCheckpointer(fsys)

	a := filepath.Join(root, "a.txt")
	b := filepath.Join(root, "b.txt")
	if err := os.WriteFile(a, []byte("a0"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(b, []byte("b0"), 0o644); err != nil {
		t.Fatal(err)
	}

	c.BeginTurn("s")
	mutate(c, fsys, "s", "a.txt", "a1")
	c.CommitTurn("s")
	c.BeginTurn("s")
	mutate(c, fsys, "s", "b.txt", "b1")
	c.CommitTurn("s")

	if _, err := c.UndoLastTurn("s"); err != nil { // undo turn 2 -> b0
		t.Fatal(err)
	}
	if got := readTmp(t, b); got != "b0" {
		t.Fatalf("b = %q, want b0", got)
	}
	if _, _, err := c.Rewind("s", 1); err != nil { // rewind turn 1 -> a0
		t.Fatal(err)
	}
	if got := readTmp(t, a); got != "a0" {
		t.Fatalf("a = %q, want a0", got)
	}
}

// TestNoCheckpointErrors is table-driven over the empty-history cases.
func TestNoCheckpointErrors(t *testing.T) {
	root := t.TempDir()
	c := NewCheckpointer(NewFileSystem(root))

	for _, tc := range []struct {
		name string
		call func() error
	}{
		{"undo", func() error { _, err := c.UndoLastTurn("s"); return err }},
		{"rewind", func() error { _, _, err := c.Rewind("s", 1); return err }},
		{"rewind-all", func() error { _, _, err := c.Rewind("s", 0); return err }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.call(); err != ErrNoCheckpoint {
				t.Fatalf("got err = %v, want ErrNoCheckpoint", err)
			}
		})
	}
}

// TestEmptyTurnDropped verifies a turn with no mutations leaves no undo history.
func TestEmptyTurnDropped(t *testing.T) {
	root := t.TempDir()
	c := NewCheckpointer(NewFileSystem(root))

	c.BeginTurn("s")
	c.CommitTurn("s") // no snapshots taken

	if c.Count("s") != 0 {
		t.Fatalf("count = %d, want 0", c.Count("s"))
	}
	if _, err := c.UndoLastTurn("s"); err != ErrNoCheckpoint {
		t.Fatalf("undo on empty history: got %v, want ErrNoCheckpoint", err)
	}
}

// TestAbortTurnDropsActive verifies an aborted turn's snapshots are discarded.
func TestAbortTurnDropsActive(t *testing.T) {
	root := t.TempDir()
	fsys := NewFileSystem(root)
	c := NewCheckpointer(fsys)

	p := filepath.Join(root, "a.txt")
	if err := os.WriteFile(p, []byte("orig"), 0o644); err != nil {
		t.Fatal(err)
	}
	c.BeginTurn("s")
	c.Snapshot("s", "a.txt", Authorization{})
	c.AbortTurn("s")

	if c.Count("s") != 0 {
		t.Fatalf("count after abort = %d, want 0", c.Count("s"))
	}
}

// TestModePreserved verifies an executable file restored by undo keeps its
// executable bit.
func TestModePreserved(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("mode bits unreliable when running as root")
	}
	root := t.TempDir()
	fsys := NewFileSystem(root)
	c := NewCheckpointer(fsys)

	p := filepath.Join(root, "run.sh")
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	c.BeginTurn("s")
	mutate(c, fsys, "s", "run.sh", "overwritten")
	c.CommitTurn("s")

	if _, err := c.UndoLastTurn("s"); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&0o111 == 0 {
		t.Fatalf("exec bit lost: mode %v", info.Mode())
	}
	if got := readTmp(t, p); got != "#!/bin/sh\n" {
		t.Fatalf("content = %q, want original", got)
	}
}

// TestSessionsAreIndependent verifies two sessions keep separate undo history.
func TestSessionsAreIndependent(t *testing.T) {
	root := t.TempDir()
	fsys := NewFileSystem(root)
	c := NewCheckpointer(fsys)

	p := filepath.Join(root, "a.txt")
	if err := os.WriteFile(p, []byte("orig"), 0o644); err != nil {
		t.Fatal(err)
	}

	c.BeginTurn("s1")
	mutate(c, fsys, "s1", "a.txt", "by-s1")
	c.CommitTurn("s1")

	// s2 has no history; its undo must error and must not touch s1's file.
	if _, err := c.UndoLastTurn("s2"); err != ErrNoCheckpoint {
		t.Fatalf("s2 undo: got %v, want ErrNoCheckpoint", err)
	}
	if got := readTmp(t, p); got != "by-s1" {
		t.Fatalf("s1 file changed by s2 undo: %q", got)
	}

	// s1 undo works.
	if _, err := c.UndoLastTurn("s1"); err != nil {
		t.Fatal(err)
	}
	if got := readTmp(t, p); got != "orig" {
		t.Fatalf("s1 undo: %q, want orig", got)
	}
}
