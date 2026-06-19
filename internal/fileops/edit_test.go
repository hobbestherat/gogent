package fileops

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestEditContentUniqueness covers the find→replace rule introduced for
// issue #18: a non-unique match is rejected in the default (unique) mode but
// honoured when replace_all is set.
func TestEditContentUniqueness(t *testing.T) {
	tests := []struct {
		name       string
		content    string
		find       string
		replace    string
		replaceAll bool
		want       string
		wantErr    string
	}{
		{
			name:    "unique match replaced",
			content: "Hello, World!",
			find:    "World",
			replace: "Universe",
			want:    "Hello, Universe!",
		},
		{
			name:    "find not found",
			content: "Hello, World!",
			find:    "Mars",
			replace: "Venus",
			wantErr: "not found",
		},
		{
			name:    "empty find rejected",
			content: "Hello",
			find:    "",
			replace: "x",
			wantErr: "must not be empty",
		},
		{
			name:    "ambiguous match rejected in unique mode",
			content: "a a a",
			find:    "a",
			replace: "b",
			wantErr: "not unique",
		},
		{
			name:       "ambiguous match replaced with replace_all",
			content:    "a a a",
			find:       "a",
			replace:    "b",
			replaceAll: true,
			want:       "b b b",
		},
		{
			name:    "single match among longer context",
			content: "func foo() {}\nfunc bar() {}",
			find:    "foo",
			replace: "baz",
			want:    "func baz() {}\nfunc bar() {}",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := editContent(tt.content, tt.find, tt.replace, tt.replaceAll)
			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("expected error containing %q, got nil (result %q)", tt.wantErr, got)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("expected error containing %q, got %q", tt.wantErr, err.Error())
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Fatalf("expected %q, got %q", tt.want, got)
			}
		})
	}
}

// TestEditFileNonUniqueLeavesFileUntouched ensures an ambiguous edit fails
// without mutating the file on disk, and that replace_all rewrites every match.
func TestEditFileNonUniqueLeavesFileUntouched(t *testing.T) {
	root := t.TempDir()
	fm := NewFileMutation(NewFileSystem(root), NewLocationMutation(root))

	path := filepath.Join(root, "dup.txt")
	original := "x = 1\ny = 1\n"
	if err := os.WriteFile(path, []byte(original), 0644); err != nil {
		t.Fatalf("seed write failed: %v", err)
	}

	// Default mode: non-unique "1" must be rejected and the file unchanged.
	if err := fm.EditFile(path, "1", "2", false, Authorization{}); err == nil {
		t.Fatalf("expected error for non-unique find")
	}
	got, _ := os.ReadFile(path)
	if string(got) != original {
		t.Fatalf("file was mutated by a failed edit: %q", string(got))
	}

	// replace_all mode: every occurrence is rewritten.
	if err := fm.EditFile(path, "1", "2", true, Authorization{}); err != nil {
		t.Fatalf("replace_all edit failed: %v", err)
	}
	got, _ = os.ReadFile(path)
	if want := "x = 2\ny = 2\n"; string(got) != want {
		t.Fatalf("expected %q, got %q", want, string(got))
	}
}

// TestPreviewEditRejectsAmbiguous ensures the diff-review preview enforces the
// same uniqueness rule as the write path.
func TestPreviewEditRejectsAmbiguous(t *testing.T) {
	root := t.TempDir()
	fm := NewFileMutation(NewFileSystem(root), NewLocationMutation(root))

	path := filepath.Join(root, "p.txt")
	if err := os.WriteFile(path, []byte("a a"), 0644); err != nil {
		t.Fatalf("seed write failed: %v", err)
	}

	if _, _, err := fm.PreviewEdit(path, "a", "b", false, Authorization{}); err == nil {
		t.Fatalf("expected preview to reject non-unique find")
	}

	before, after, err := fm.PreviewEdit(path, "a", "b", true, Authorization{})
	if err != nil {
		t.Fatalf("replace_all preview failed: %v", err)
	}
	if before != "a a" || after != "b b" {
		t.Fatalf("unexpected preview: before=%q after=%q", before, after)
	}
}
