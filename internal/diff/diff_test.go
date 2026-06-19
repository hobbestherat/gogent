package diff

import (
	"strings"
	"testing"
)

func TestUnified(t *testing.T) {
	for _, tc := range []struct {
		name     string
		old, new string
		want     string
	}{
		{
			name: "identical returns empty",
			old:  "a\nb\nc\n",
			new:  "a\nb\nc\n",
			want: "",
		},
		{
			name: "single line replace",
			old:  "hello world\n",
			new:  "hello universe\n",
			want: "--- a/f.txt\n+++ b/f.txt\n@@ -1 +1 @@\n-hello world\n+hello universe\n",
		},
		{
			name: "insert into empty file",
			old:  "",
			new:  "first\n",
			want: "--- a/f.txt\n+++ b/f.txt\n@@ -0,0 +1 @@\n+first\n",
		},
		{
			name: "delete whole file",
			old:  "only\n",
			new:  "",
			want: "--- a/f.txt\n+++ b/f.txt\n@@ -1 +0,0 @@\n-only\n",
		},
		{
			name: "change keeps surrounding context",
			old:  "1\n2\n3\n4\n5\n6\n7\n",
			new:  "1\n2\n3\nX\n5\n6\n7\n",
			want: "--- a/f.txt\n+++ b/f.txt\n@@ -1,7 +1,7 @@\n 1\n 2\n 3\n-4\n+X\n 5\n 6\n 7\n",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := Unified(tc.old, tc.new, "f.txt")
			if got != tc.want {
				t.Errorf("Unified mismatch\n got: %q\nwant: %q", got, tc.want)
			}
		})
	}
}

// TestUnifiedDistantChangesSplitIntoHunks verifies two far-apart changes produce
// two separate hunks rather than one spanning the untouched middle.
func TestUnifiedDistantChangesSplitIntoHunks(t *testing.T) {
	var oldB, newB strings.Builder
	for i := 1; i <= 20; i++ {
		line := string(rune('a'+(i%26))) + "\n"
		oldB.WriteString(line)
		switch i {
		case 1:
			newB.WriteString("FIRST\n")
		case 20:
			newB.WriteString("LAST\n")
		default:
			newB.WriteString(line)
		}
	}
	got := Unified(oldB.String(), newB.String(), "f.txt")
	if n := strings.Count(got, "@@ -"); n != 2 {
		t.Fatalf("expected 2 hunks, got %d\n%s", n, got)
	}
	if !strings.Contains(got, "+FIRST") || !strings.Contains(got, "+LAST") {
		t.Errorf("both changes should appear in the diff:\n%s", got)
	}
}

// TestUnifiedNoTrailingNewline confirms files without a trailing newline diff the
// same as their newline-terminated counterparts (the distinction is dropped).
func TestUnifiedNoTrailingNewline(t *testing.T) {
	got := Unified("a\nb", "a\nc", "f.txt")
	want := "--- a/f.txt\n+++ b/f.txt\n@@ -1,2 +1,2 @@\n a\n-b\n+c\n"
	if got != want {
		t.Errorf("Unified mismatch\n got: %q\nwant: %q", got, want)
	}
}
