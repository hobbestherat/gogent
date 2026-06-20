// Package diff produces unified-format text diffs using only the standard
// library. It backs the diff-preview shown before a write/edit is applied
// (issue #64).
package diff

import (
	"fmt"
	"strings"
)

// contextLines is the number of unchanged lines shown around each change in a
// unified-diff hunk.
const contextLines = 3

// Unified returns a unified diff transforming old into new, labelled for path
// (rendered as "a/<path>" and "b/<path>"). It returns "" when the two inputs are
// identical. The output is standard unified-diff format: "---"/"+++" file
// headers, "@@ -l,s +l,s @@" hunk headers and " "/"-"/"+" line prefixes.
func Unified(old, new, path string) string {
	if old == new {
		return ""
	}
	ops := diffLines(splitLines(old), splitLines(new))
	hunks := hunksFromOps(ops)
	if len(hunks) == 0 {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "--- a/%s\n", path)
	fmt.Fprintf(&b, "+++ b/%s\n", path)
	for _, h := range hunks {
		b.WriteString(h)
	}
	return b.String()
}

// splitLines breaks text into lines without their trailing newline. A trailing
// newline (every well-formed text file has one) is not treated as an extra empty
// line, so "a\nb\n" and "a\nb" both yield ["a", "b"]. The "no newline at end of
// file" distinction is intentionally dropped — it does not matter for a preview.
func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	lines := strings.Split(s, "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

type opKind int

const (
	opEqual opKind = iota
	opDelete
	opInsert
)

type op struct {
	kind opKind
	text string
}

// diffLines computes a line-level diff between a and b as an ordered op sequence
// via a longest-common-subsequence table. For very large inputs (where the LCS
// table would be costly) it falls back to a coarse whole-block replacement,
// which is still a valid diff.
func diffLines(a, b []string) []op {
	const maxCells = 4_000_000
	if len(a)*len(b) > maxCells {
		ops := make([]op, 0, len(a)+len(b))
		for _, s := range a {
			ops = append(ops, op{opDelete, s})
		}
		for _, s := range b {
			ops = append(ops, op{opInsert, s})
		}
		return ops
	}

	m, n := len(a), len(b)
	// lcs[i][j] = length of the LCS of a[i:] and b[j:].
	lcs := make([][]int, m+1)
	for i := range lcs {
		lcs[i] = make([]int, n+1)
	}
	for i := m - 1; i >= 0; i-- {
		for j := n - 1; j >= 0; j-- {
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
	for i < m && j < n {
		switch {
		case a[i] == b[j]:
			ops = append(ops, op{opEqual, a[i]})
			i++
			j++
		case lcs[i+1][j] >= lcs[i][j+1]:
			ops = append(ops, op{opDelete, a[i]})
			i++
		default:
			ops = append(ops, op{opInsert, b[j]})
			j++
		}
	}
	for ; i < m; i++ {
		ops = append(ops, op{opDelete, a[i]})
	}
	for ; j < n; j++ {
		ops = append(ops, op{opInsert, b[j]})
	}
	return ops
}

// hunksFromOps groups changed ops (with up to contextLines of surrounding
// context, merging hunks that touch) into rendered unified-diff hunk strings.
func hunksFromOps(ops []op) []string {
	n := len(ops)
	// 1-based starting line of each op within the old and new files.
	oldLine := make([]int, n)
	newLine := make([]int, n)
	ol, nl := 1, 1
	for k, o := range ops {
		oldLine[k], newLine[k] = ol, nl
		switch o.kind {
		case opEqual:
			ol++
			nl++
		case opDelete:
			ol++
		case opInsert:
			nl++
		}
	}

	// Collect op-index ranges [start,end) covering each change run, padded by
	// contextLines and merged when they overlap.
	var ranges [][2]int
	for k := 0; k < n; {
		if ops[k].kind == opEqual {
			k++
			continue
		}
		start := k
		for k < n && ops[k].kind != opEqual {
			k++
		}
		end := k
		s := start - contextLines
		if s < 0 {
			s = 0
		}
		e := end + contextLines
		if e > n {
			e = n
		}
		if len(ranges) > 0 && s <= ranges[len(ranges)-1][1] {
			ranges[len(ranges)-1][1] = e
		} else {
			ranges = append(ranges, [2]int{s, e})
		}
	}

	hunks := make([]string, 0, len(ranges))
	for _, r := range ranges {
		s, e := r[0], r[1]
		var body strings.Builder
		oldCount, newCount := 0, 0
		for k := s; k < e; k++ {
			switch ops[k].kind {
			case opEqual:
				body.WriteString(" " + ops[k].text + "\n")
				oldCount++
				newCount++
			case opDelete:
				body.WriteString("-" + ops[k].text + "\n")
				oldCount++
			case opInsert:
				body.WriteString("+" + ops[k].text + "\n")
				newCount++
			}
		}
		header := fmt.Sprintf("@@ -%s +%s @@\n",
			rangeSpec(oldLine[s], oldCount), rangeSpec(newLine[s], newCount))
		hunks = append(hunks, header+body.String())
	}
	return hunks
}

// rangeSpec renders a unified-diff "start,count" range. A single-line range omits
// the count; an empty range points at the line before it (the unified-format
// convention for pure insertions/deletions).
func rangeSpec(start, count int) string {
	switch count {
	case 0:
		return fmt.Sprintf("%d,0", start-1)
	case 1:
		return fmt.Sprintf("%d", start)
	default:
		return fmt.Sprintf("%d,%d", start, count)
	}
}
