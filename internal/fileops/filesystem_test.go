package fileops

import (
	"os"
	"path/filepath"
	"testing"
)

// TestWriteAbsolutePathInsideWorkspace ensures an absolute path that points
// inside the workspace is written to that exact location, not re-rooted under
// the workspace (which previously created phantom dirs like <ws>/Users/...).
func TestWriteAbsolutePathInsideWorkspace(t *testing.T) {
	root := t.TempDir()
	fsys := NewFileSystem(root)

	abs := filepath.Join(root, "sub", "PLAN.md")
	if err := fsys.Write(abs, []byte("hello"), Authorization{}); err != nil {
		t.Fatalf("write failed: %v", err)
	}

	// The file must exist at the absolute path exactly.
	if _, err := os.Stat(abs); err != nil {
		t.Fatalf("expected file at %s: %v", abs, err)
	}

	// No phantom re-rooted copy may exist.
	phantom := filepath.Join(root, root, "sub", "PLAN.md")
	if _, err := os.Stat(phantom); err == nil {
		t.Fatalf("phantom re-rooted file was created at %s", phantom)
	}
}

func TestWriteRelativePathResolvesAgainstWorkspace(t *testing.T) {
	root := t.TempDir()
	fsys := NewFileSystem(root)

	if err := fsys.Write("notes/todo.txt", []byte("x"), Authorization{}); err != nil {
		t.Fatalf("write failed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "notes", "todo.txt")); err != nil {
		t.Fatalf("relative write not resolved against workspace: %v", err)
	}
}

func TestReadWriteRoundTripAbsolute(t *testing.T) {
	root := t.TempDir()
	fsys := NewFileSystem(root)

	abs := filepath.Join(root, "data.txt")
	if err := fsys.Write(abs, []byte("content"), Authorization{}); err != nil {
		t.Fatalf("write failed: %v", err)
	}
	got, err := fsys.Read(abs, Authorization{})
	if err != nil {
		t.Fatalf("read failed: %v", err)
	}
	if string(got) != "content" {
		t.Fatalf("got %q, want %q", got, "content")
	}
}

func TestEscapeOutsideWorkspaceRejected(t *testing.T) {
	root := t.TempDir()
	fsys := NewFileSystem(root)

	outside := filepath.Join(filepath.Dir(root), "escape.txt")
	if err := fsys.Write(outside, []byte("x"), Authorization{}); err == nil {
		t.Fatalf("expected escape rejection for %s", outside)
	}
	if err := fsys.Write("../escape.txt", []byte("x"), Authorization{}); err == nil {
		t.Fatalf("expected escape rejection for relative ..")
	}
}

// TestReadWriteExternalWithAuthorization verifies that an Authorization obtained
// for an approved external path relaxes the workspace boundary for Read/Write —
// the core of issue #13. Without the authorization the same paths are rejected.
func TestReadWriteExternalWithAuthorization(t *testing.T) {
	root := t.TempDir()
	external := t.TempDir()
	fsys := NewFileSystem(root)

	target := filepath.Join(external, "outside.txt")
	auth := Authorization{external: true}

	if err := fsys.Write(target, []byte("ext"), auth); err != nil {
		t.Fatalf("authorized external write failed: %v", err)
	}
	got, err := fsys.Read(target, auth)
	if err != nil {
		t.Fatalf("authorized external read failed: %v", err)
	}
	if string(got) != "ext" {
		t.Fatalf("got %q, want %q", got, "ext")
	}

	// Same path without an external authorization must still be rejected:
	// defense-in-depth keeps the boundary in place when no grant exists.
	if err := fsys.Write(target, []byte("nope"), Authorization{}); err == nil {
		t.Fatal("expected external path to be rejected without authorization")
	}
}
