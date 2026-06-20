package fileops

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestMultiEditContent covers the all-or-nothing sequential semantics of a
// multi_edit batch (issue #45): edits apply in order against the running result,
// each find must be unique unless replace_all is set, and a single bad edit
// aborts the whole batch.
func TestMultiEditContent(t *testing.T) {
	tests := []struct {
		name    string
		content string
		edits   []EditOp
		want    string
		wantErr string
	}{
		{
			name:    "two sequential edits",
			content: "alpha beta gamma",
			edits:   []EditOp{{Find: "alpha", Replace: "one"}, {Find: "gamma", Replace: "three"}},
			want:    "one beta three",
		},
		{
			name:    "later edit sees earlier result",
			content: "a",
			edits:   []EditOp{{Find: "a", Replace: "b"}, {Find: "b", Replace: "c"}},
			want:    "c",
		},
		{
			name:    "replace_all per edit",
			content: "x x y",
			edits:   []EditOp{{Find: "x", Replace: "z", ReplaceAll: true}, {Find: "y", Replace: "w"}},
			want:    "z z w",
		},
		{
			name:    "ambiguous edit aborts whole batch",
			content: "a a b",
			edits:   []EditOp{{Find: "b", Replace: "c"}, {Find: "a", Replace: "d"}},
			wantErr: "edit 2: find text is not unique",
		},
		{
			name:    "missing find aborts batch",
			content: "hello",
			edits:   []EditOp{{Find: "nope", Replace: "x"}},
			wantErr: "edit 1: no changes made",
		},
		{
			name:    "empty batch rejected",
			content: "hello",
			edits:   nil,
			wantErr: "no edits provided",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := multiEditContent(tt.content, tt.edits)
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

// TestMultiEditFileAtomic ensures a failed batch leaves the file on disk
// untouched, while a fully-applying batch is written.
func TestMultiEditFileAtomic(t *testing.T) {
	root := t.TempDir()
	fm := NewFileMutation(NewFileSystem(root), NewLocationMutation(root))

	path := filepath.Join(root, "m.txt")
	original := "one two three\n"
	if err := os.WriteFile(path, []byte(original), 0644); err != nil {
		t.Fatalf("seed write failed: %v", err)
	}

	// Second edit is non-unique → whole batch must fail, file unchanged.
	bad := []EditOp{{Find: "one", Replace: "1"}, {Find: " ", Replace: "_"}}
	if err := fm.MultiEditFile(path, bad, Authorization{}); err == nil {
		t.Fatalf("expected error for ambiguous edit in batch")
	}
	if got, _ := os.ReadFile(path); string(got) != original {
		t.Fatalf("file mutated by a failed batch: %q", string(got))
	}

	good := []EditOp{{Find: "one", Replace: "1"}, {Find: "three", Replace: "3"}}
	if err := fm.MultiEditFile(path, good, Authorization{}); err != nil {
		t.Fatalf("good batch failed: %v", err)
	}
	if got, _ := os.ReadFile(path); string(got) != "1 two 3\n" {
		t.Fatalf("expected %q, got %q", "1 two 3\n", string(got))
	}
}

// TestParseAndApplyPatch covers the *** Begin Patch envelope: add, update and
// delete sections, context-anchored hunks, and multi-hunk updates.
func TestParseAndApplyPatch(t *testing.T) {
	tests := []struct {
		name    string
		before  string // current file content (ignored for add)
		patch   string
		wantOp  PatchOpType
		want    string // expected after-content (for add/update)
		wantErr string
	}{
		{
			name:   "add file",
			patch:  "*** Begin Patch\n*** Add File: new.txt\n+hello\n+world\n*** End Patch",
			wantOp: PatchAdd,
			want:   "hello\nworld\n",
		},
		{
			name:   "delete file",
			before: "gone\n",
			patch:  "*** Begin Patch\n*** Delete File: old.txt\n*** End Patch",
			wantOp: PatchDelete,
			want:   "",
		},
		{
			name:   "update single hunk",
			before: "line1\nline2\nline3\n",
			patch:  "*** Begin Patch\n*** Update File: f.txt\n@@\n line1\n-line2\n+LINE2\n line3\n*** End Patch",
			wantOp: PatchUpdate,
			want:   "line1\nLINE2\nline3\n",
		},
		{
			name:   "update pure insertion via context",
			before: "a\nb\n",
			patch:  "*** Begin Patch\n*** Update File: f.txt\n@@\n a\n+inserted\n b\n*** End Patch",
			wantOp: PatchUpdate,
			want:   "a\ninserted\nb\n",
		},
		{
			name:   "update two hunks",
			before: "1\n2\n3\n4\n5\n",
			patch:  "*** Begin Patch\n*** Update File: f.txt\n@@\n-1\n+ONE\n@@\n-5\n+FIVE\n*** End Patch",
			wantOp: PatchUpdate,
			want:   "ONE\n2\n3\n4\nFIVE\n",
		},
		{
			name:    "hunk context not found",
			before:  "a\nb\n",
			patch:   "*** Begin Patch\n*** Update File: f.txt\n@@\n-zzz\n+yyy\n*** End Patch",
			wantErr: "does not match",
		},
		{
			name:    "missing begin",
			patch:   "*** Update File: f.txt\n@@\n-a\n+b\n*** End Patch",
			wantErr: "must start with",
		},
		{
			name:    "missing end",
			patch:   "*** Begin Patch\n*** Add File: x\n+y\n",
			wantErr: "missing",
		},
		{
			name:    "empty patch",
			patch:   "*** Begin Patch\n*** End Patch",
			wantErr: "no file operations",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ops, err := ParsePatch(tt.patch)
			if tt.wantErr != "" {
				if err != nil {
					if !strings.Contains(err.Error(), tt.wantErr) {
						t.Fatalf("expected error containing %q, got %q", tt.wantErr, err.Error())
					}
					return
				}
				// Parse succeeded; the error must surface when applying.
				_, aerr := ops[0].ApplyTo(tt.before)
				if aerr == nil || !strings.Contains(aerr.Error(), tt.wantErr) {
					t.Fatalf("expected apply error containing %q, got %v", tt.wantErr, aerr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected parse error: %v", err)
			}
			if len(ops) != 1 {
				t.Fatalf("expected 1 op, got %d", len(ops))
			}
			if ops[0].Type != tt.wantOp {
				t.Fatalf("expected op %v, got %v", tt.wantOp, ops[0].Type)
			}
			got, err := ops[0].ApplyTo(tt.before)
			if err != nil {
				t.Fatalf("unexpected apply error: %v", err)
			}
			if got != tt.want {
				t.Fatalf("expected %q, got %q", tt.want, got)
			}
		})
	}
}

// TestParsePatchMultiFile checks an envelope that touches several files in one
// shot, preserving order and op kinds.
func TestParsePatchMultiFile(t *testing.T) {
	patch := strings.Join([]string{
		"*** Begin Patch",
		"*** Add File: a.txt",
		"+new",
		"*** Update File: b.txt",
		"@@",
		"-old",
		"+fresh",
		"*** Delete File: c.txt",
		"*** End Patch",
	}, "\n")

	ops, err := ParsePatch(patch)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(ops) != 3 {
		t.Fatalf("expected 3 ops, got %d", len(ops))
	}
	want := []struct {
		typ  PatchOpType
		path string
	}{
		{PatchAdd, "a.txt"},
		{PatchUpdate, "b.txt"},
		{PatchDelete, "c.txt"},
	}
	for i, w := range want {
		if ops[i].Type != w.typ || ops[i].Path != w.path {
			t.Fatalf("op %d: expected (%v,%q), got (%v,%q)", i, w.typ, w.path, ops[i].Type, ops[i].Path)
		}
	}
}
