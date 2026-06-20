package fileops

import (
	"fmt"
	"strings"
)

// PatchOpType is the kind of file action declared in an apply_patch envelope.
type PatchOpType int

const (
	// PatchAdd creates a new file from the op's body.
	PatchAdd PatchOpType = iota
	// PatchUpdate rewrites parts of an existing file via context hunks.
	PatchUpdate
	// PatchDelete removes an existing file.
	PatchDelete
)

// String renders the op kind as the short verb used in messages and the
// diff-review gate ("add", "update", "delete").
func (t PatchOpType) String() string {
	switch t {
	case PatchAdd:
		return "add"
	case PatchUpdate:
		return "update"
	case PatchDelete:
		return "delete"
	default:
		return "unknown"
	}
}

// PatchOp is one file action parsed from a *** Begin Patch envelope. The hunks
// and add body are kept private; callers drive an op through ApplyTo, which turns
// a file's current content into its post-patch content.
type PatchOp struct {
	Type  PatchOpType
	Path  string
	add   string      // new-file body, for PatchAdd
	hunks []patchHunk // edits, for PatchUpdate
}

// patchHunk is one contiguous change region of an Update File. oldLines is the
// pre-change block (context + deleted lines) located in the file; newLines is the
// block that replaces it (context + added lines). Both preserve source order.
type patchHunk struct {
	oldLines []string
	newLines []string
}

// ApplyTo computes the content this op produces from a file's current content
// (before is "" for a file that does not yet exist). An Update fails when a
// hunk's context cannot be located, so a bad patch is rejected before any write.
func (op PatchOp) ApplyTo(before string) (string, error) {
	switch op.Type {
	case PatchAdd:
		return op.add, nil
	case PatchDelete:
		return "", nil
	case PatchUpdate:
		return applyHunks(before, op.hunks)
	default:
		return "", fmt.Errorf("unknown patch operation")
	}
}

// ParsePatch parses the unified "*** Begin Patch" envelope (the apply_patch
// format) into an ordered list of file operations. It performs no I/O, so the
// returned ops can be planned and previewed before anything is written. The
// envelope is:
//
//	*** Begin Patch
//	*** Add File: path
//	+new line
//	*** Update File: path
//	@@ optional context locator
//	 context line
//	-removed line
//	+added line
//	*** Delete File: path
//	*** End Patch
//
// File renames ("*** Move to:") are intentionally not supported; use a Delete +
// Add pair instead.
func ParsePatch(patch string) ([]PatchOp, error) {
	lines := strings.Split(patch, "\n")
	// Drop the empty element produced by the envelope's trailing newline so it is
	// not mistaken for a malformed (unprefixed) body line.
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}

	i := 0
	for i < len(lines) && strings.TrimSpace(lines[i]) == "" {
		i++
	}
	if i >= len(lines) || strings.TrimSpace(lines[i]) != "*** Begin Patch" {
		return nil, fmt.Errorf(`patch must start with "*** Begin Patch"`)
	}
	i++

	var ops []PatchOp
	for i < len(lines) {
		line := lines[i]
		if strings.TrimSpace(line) == "*** End Patch" {
			if len(ops) == 0 {
				return nil, fmt.Errorf("patch contains no file operations")
			}
			return ops, nil
		}

		switch {
		case strings.HasPrefix(line, "*** Add File: "):
			path := strings.TrimSpace(strings.TrimPrefix(line, "*** Add File: "))
			i++
			var body []string
			for i < len(lines) && !isPatchMarker(lines[i]) {
				l := lines[i]
				if !strings.HasPrefix(l, "+") {
					return nil, fmt.Errorf("add file %q: content lines must start with '+', got %q", path, l)
				}
				body = append(body, l[1:])
				i++
			}
			content := ""
			if len(body) > 0 {
				content = strings.Join(body, "\n") + "\n"
			}
			ops = append(ops, PatchOp{Type: PatchAdd, Path: path, add: content})

		case strings.HasPrefix(line, "*** Delete File: "):
			path := strings.TrimSpace(strings.TrimPrefix(line, "*** Delete File: "))
			i++
			ops = append(ops, PatchOp{Type: PatchDelete, Path: path})

		case strings.HasPrefix(line, "*** Update File: "):
			path := strings.TrimSpace(strings.TrimPrefix(line, "*** Update File: "))
			i++
			hunks, next, err := parseHunks(lines, i, path)
			if err != nil {
				return nil, err
			}
			i = next
			ops = append(ops, PatchOp{Type: PatchUpdate, Path: path, hunks: hunks})

		default:
			return nil, fmt.Errorf("unexpected line in patch: %q", line)
		}
	}

	return nil, fmt.Errorf(`patch missing "*** End Patch"`)
}

// isPatchMarker reports whether a line opens a new section ("*** ..."), which
// terminates the body of the preceding Add File or Update File.
func isPatchMarker(line string) bool {
	return strings.HasPrefix(line, "*** ")
}

// parseHunks reads the body of an Update File starting at lines[start], up to the
// next section marker, splitting it into hunks at "@@" boundaries. It returns the
// hunks and the index of the first line it did not consume.
func parseHunks(lines []string, start int, path string) ([]patchHunk, int, error) {
	var hunks []patchHunk
	var cur patchHunk
	open := false

	flush := func() {
		if open && (len(cur.oldLines) > 0 || len(cur.newLines) > 0) {
			hunks = append(hunks, cur)
		}
		cur = patchHunk{}
		open = false
	}

	i := start
	for i < len(lines) && !isPatchMarker(lines[i]) {
		l := lines[i]
		switch {
		case strings.HasPrefix(l, "@@"):
			flush()
			open = true
		case strings.HasPrefix(l, " "):
			cur.oldLines = append(cur.oldLines, l[1:])
			cur.newLines = append(cur.newLines, l[1:])
			open = true
		case strings.HasPrefix(l, "-"):
			cur.oldLines = append(cur.oldLines, l[1:])
			open = true
		case strings.HasPrefix(l, "+"):
			cur.newLines = append(cur.newLines, l[1:])
			open = true
		case l == "":
			// A blank line with no prefix is treated as blank context, which keeps
			// the parser robust to tools that drop the leading space on empty lines.
			cur.oldLines = append(cur.oldLines, "")
			cur.newLines = append(cur.newLines, "")
			open = true
		default:
			return nil, 0, fmt.Errorf("update file %q: unexpected hunk line %q", path, l)
		}
		i++
	}
	flush()

	if len(hunks) == 0 {
		return nil, 0, fmt.Errorf("update file %q: no hunks found", path)
	}
	return hunks, i, nil
}

// applyHunks applies each hunk to before in order, anchoring on the hunk's
// oldLines block. Search resumes after the previous match so repeated context
// still maps to successive hunks. A hunk whose block is absent — or that carries
// no context to anchor a pure insertion — is an error, leaving before unchanged.
func applyHunks(before string, hunks []patchHunk) (string, error) {
	lines := strings.Split(before, "\n")
	cursor := 0
	for hi, h := range hunks {
		if len(h.oldLines) == 0 {
			return "", fmt.Errorf("hunk %d has no context to anchor its insertion", hi+1)
		}
		idx := indexOfSubslice(lines, h.oldLines, cursor)
		if idx < 0 {
			return "", fmt.Errorf("hunk %d does not match the file: context not found", hi+1)
		}
		next := make([]string, 0, len(lines)-len(h.oldLines)+len(h.newLines))
		next = append(next, lines[:idx]...)
		next = append(next, h.newLines...)
		next = append(next, lines[idx+len(h.oldLines):]...)
		lines = next
		cursor = idx + len(h.newLines)
	}
	return strings.Join(lines, "\n"), nil
}

// indexOfSubslice returns the first index >= from at which sub occurs as a
// consecutive run within s, or -1 when it does not.
func indexOfSubslice(s, sub []string, from int) int {
	if len(sub) == 0 || len(sub) > len(s) {
		return -1
	}
	for i := from; i <= len(s)-len(sub); i++ {
		match := true
		for j := range sub {
			if s[i+j] != sub[j] {
				match = false
				break
			}
		}
		if match {
			return i
		}
	}
	return -1
}
