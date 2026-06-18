package diff

import (
	"strings"
	"testing"
)

func TestUnifiedStat(t *testing.T) {
	tests := []struct {
		name             string
		old, new         string
		wantAdd, wantDel int
		wantEmpty        bool
	}{
		{name: "identical", old: "a\nb\nc\n", new: "a\nb\nc\n", wantEmpty: true},
		{name: "single replace", old: "a\nb\nc\n", new: "a\nB\nc\n", wantAdd: 1, wantDel: 1},
		{name: "pure insertion at end", old: "a\nb\n", new: "a\nb\nc\n", wantAdd: 1},
		{name: "pure deletion", old: "a\nb\nc\n", new: "a\nc\n", wantDel: 1},
		{name: "new file", old: "", new: "x\ny\n", wantAdd: 2},
		{name: "emptied file", old: "x\ny\n", new: "", wantDel: 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out, stat := Unified("f.txt", tt.old, tt.new)
			if stat.IsEmpty() != tt.wantEmpty {
				t.Fatalf("IsEmpty()=%v, want %v (stat=%+v)", stat.IsEmpty(), tt.wantEmpty, stat)
			}
			if tt.wantEmpty {
				if out != "" {
					t.Fatalf("expected empty diff, got %q", out)
				}
				return
			}
			if stat.Added != tt.wantAdd || stat.Removed != tt.wantDel {
				t.Fatalf("stat=%+v, want add=%d del=%d", stat, tt.wantAdd, tt.wantDel)
			}
			if !strings.HasPrefix(out, "--- f.txt\n+++ f.txt\n@@") {
				t.Fatalf("missing unified header:\n%s", out)
			}
		})
	}
}

func TestUnifiedHunkHeaderAndBody(t *testing.T) {
	old := "line1\nline2\nline3\n"
	new := "line1\nCHANGED\nline3\n"
	out, _ := Unified("a.go", old, new)

	want := strings.Join([]string{
		"--- a.go",
		"+++ a.go",
		"@@ -1,3 +1,3 @@",
		" line1",
		"-line2",
		"+CHANGED",
		" line3",
	}, "\n")
	if out != want {
		t.Fatalf("diff mismatch:\n got:\n%s\n\nwant:\n%s", out, want)
	}
}

// TestUnifiedContextWindow ensures distant changes split into separate hunks
// rather than dragging unrelated context between them.
func TestUnifiedContextWindow(t *testing.T) {
	var oldB, newB strings.Builder
	for i := 1; i <= 20; i++ {
		oldB.WriteString("l")
		oldB.WriteByte(byte('0' + i%10))
		oldB.WriteByte('\n')
		newB.WriteString("l")
		newB.WriteByte(byte('0' + i%10))
		newB.WriteByte('\n')
	}
	old := "head\n" + oldB.String() + "tail\n"
	// Change only the very first and very last content lines.
	new := "HEAD\n" + newB.String() + "TAIL\n"

	out, stat := Unified("big.txt", old, new)
	if stat.Added != 2 || stat.Removed != 2 {
		t.Fatalf("stat=%+v, want add=2 del=2", stat)
	}
	hunks := strings.Count(out, "@@ -")
	if hunks != 2 {
		t.Fatalf("expected 2 separate hunks, got %d:\n%s", hunks, out)
	}
}

// TestUnifiedLineMarkers checks that each rendered line carries the conventional
// unified-diff leading marker so a renderer can colour it by its first byte.
func TestUnifiedLineMarkers(t *testing.T) {
	out, stat := Unified("x", "a\nb\n", "a\nc\n")
	if stat.IsEmpty() {
		t.Fatal("expected a non-empty diff")
	}
	var add, del, ctx, hdr int
	for _, line := range strings.Split(out, "\n") {
		switch {
		case strings.HasPrefix(line, "@@"), strings.HasPrefix(line, "---"), strings.HasPrefix(line, "+++"):
			hdr++
		case strings.HasPrefix(line, "+"):
			add++
		case strings.HasPrefix(line, "-"):
			del++
		default:
			ctx++
		}
	}
	if add != 1 || del != 1 {
		t.Fatalf("add=%d del=%d, want 1/1", add, del)
	}
	if ctx != 1 {
		t.Fatalf("ctx=%d, want 1 surrounding context line", ctx)
	}
	if hdr != 3 { // two file headers + one hunk header
		t.Fatalf("hdr=%d, want 3", hdr)
	}
}
