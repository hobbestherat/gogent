// Package diff produces a unified, line-based diff between two text blobs using
// only the standard library. It backs the edit-review preview (issue #64): the
// write/edit tools render the before/after as a unified diff so the user can
// approve or reject a change before it touches disk.
package diff

import (
	"fmt"
	"strings"
)

// Stat summarizes the magnitude of a change: the number of added and removed
// lines. A change with Added==0 && Removed==0 is a no-op.
type Stat struct {
	Added   int
	Removed int
}

// IsEmpty reports whether the diff changes nothing.
func (s Stat) IsEmpty() bool { return s.Added == 0 && s.Removed == 0 }

// context is the number of unchanged lines kept around each change, matching the
// conventional `diff -u` default.
const context = 3

// kind classifies a single diff op.
type kind int

const (
	equal kind = iota
	del
	ins
)

// Unified computes the unified diff between oldText and newText and returns the
// rendered text (with `--- path`/`+++ path` and `@@` hunk headers, suitable for
// display or for `patch`) together with a Stat. When the texts are identical the
// returned string is empty and Stat.IsEmpty() is true. Each line carries the
// conventional leading marker (" ", "+", "-", "@@", "---", "+++"), so a renderer
// can colour it by its first character.
func Unified(path, oldText, newText string) (string, Stat) {
	a := splitLines(oldText)
	b := splitLines(newText)
	ops := diffOps(a, b)

	var stat Stat
	for _, o := range ops {
		switch o.kind {
		case ins:
			stat.Added++
		case del:
			stat.Removed++
		}
	}
	if stat.IsEmpty() {
		return "", stat
	}

	// Number every op against its source/target line so hunk headers can be
	// emitted, then keep only lines within `context` of a change.
	type numbered struct {
		kind  kind
		text  string
		aLine int
		bLine int
	}
	ai, bi := 0, 0
	all := make([]numbered, 0, len(ops))
	for _, o := range ops {
		n := numbered{kind: o.kind, text: o.text}
		switch o.kind {
		case equal:
			ai++
			bi++
			n.aLine, n.bLine = ai, bi
		case del:
			ai++
			n.aLine = ai
		case ins:
			bi++
			n.bLine = bi
		}
		all = append(all, n)
	}

	keep := make([]bool, len(all))
	for i, n := range all {
		if n.kind == equal {
			continue
		}
		lo := max(0, i-context)
		hi := min(len(all)-1, i+context)
		for j := lo; j <= hi; j++ {
			keep[j] = true
		}
	}

	var sb strings.Builder
	sb.WriteString("--- " + path + "\n")
	sb.WriteString("+++ " + path)

	for s := 0; s < len(all); {
		if !keep[s] {
			s++
			continue
		}
		e := s
		for e+1 < len(all) && keep[e+1] {
			e++
		}

		var aStart, aCount, bStart, bCount int
		for k := s; k <= e; k++ {
			n := all[k]
			if n.kind != ins { // counts toward the old file
				if aCount == 0 {
					aStart = n.aLine
				}
				aCount++
			}
			if n.kind != del { // counts toward the new file
				if bCount == 0 {
					bStart = n.bLine
				}
				bCount++
			}
		}
		// A pure-insertion or pure-deletion hunk has no anchor line on one side;
		// reference the line just before the hunk (0 at the start of the file).
		if aCount == 0 {
			if s > 0 {
				aStart = all[s-1].aLine
			}
		}
		if bCount == 0 {
			if s > 0 {
				bStart = all[s-1].bLine
			}
		}

		fmt.Fprintf(&sb, "\n@@ -%s +%s @@", rangeStr(aStart, aCount), rangeStr(bStart, bCount))
		for k := s; k <= e; k++ {
			n := all[k]
			switch n.kind {
			case equal:
				sb.WriteString("\n " + n.text)
			case del:
				sb.WriteString("\n-" + n.text)
			case ins:
				sb.WriteString("\n+" + n.text)
			}
		}
		s = e + 1
	}

	return sb.String(), stat
}

// rangeStr renders a unified-diff range. Counts of exactly one drop the ",n"
// suffix, matching GNU diff.
func rangeStr(start, count int) string {
	if count == 1 {
		return fmt.Sprintf("%d", start)
	}
	return fmt.Sprintf("%d,%d", start, count)
}

// op is a single edit-script entry produced by diffOps.
type op struct {
	kind kind
	text string
}

// diffOps returns the line-level edit script turning a into b, computed from a
// longest-common-subsequence table. It is O(len(a)*len(b)) in time and memory,
// which is fine for the source-file-sized inputs the edit-review path handles.
func diffOps(a, b []string) []op {
	n, m := len(a), len(b)
	// lcs[i][j] = length of the LCS of a[i:] and b[j:].
	lcs := make([][]int, n+1)
	for i := range lcs {
		lcs[i] = make([]int, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if a[i] == b[j] {
				lcs[i][j] = lcs[i+1][j+1] + 1
			} else if lcs[i+1][j] >= lcs[i][j+1] {
				lcs[i][j] = lcs[i+1][j]
			} else {
				lcs[i][j] = lcs[i][j+1]
			}
		}
	}

	var ops []op
	i, j := 0, 0
	for i < n && j < m {
		switch {
		case a[i] == b[j]:
			ops = append(ops, op{equal, a[i]})
			i++
			j++
		case lcs[i+1][j] >= lcs[i][j+1]:
			ops = append(ops, op{del, a[i]})
			i++
		default:
			ops = append(ops, op{ins, b[j]})
			j++
		}
	}
	for ; i < n; i++ {
		ops = append(ops, op{del, a[i]})
	}
	for ; j < m; j++ {
		ops = append(ops, op{ins, b[j]})
	}
	return ops
}

// splitLines splits text into lines without the trailing newline of each line. A
// trailing newline on the whole blob is not treated as an extra empty line, so
// "a\nb\n" and "a\nb" both yield ["a","b"]; the empty string yields no lines.
func splitLines(text string) []string {
	if text == "" {
		return nil
	}
	return strings.Split(strings.TrimSuffix(text, "\n"), "\n")
}
