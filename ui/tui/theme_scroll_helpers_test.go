package ui

import (
	"strings"
	"testing"

	tui "github.com/hobbestherat/turbotui"
)

// editorScrollAggregate returns the union of screenText across every scroll offset of the
// theme editor (assumed to be the top modal layer). With the #279/#291 roles the grouped
// content exceeds the viewport and scrolls (the issue #267 render model is now a scrolling
// viewport), so a header/label/role that is only visible at some offset still appears in the
// aggregate. It drives the real scroll path (Down bubbles to the dialog's scroll handler)
// from offset 0 down to themeEditorMaxScroll(), then restores the scroll to the top.
func editorScrollAggregate(t *testing.T, w *Workbench) string {
	t.Helper()
	var b strings.Builder
	b.WriteString(screenText(w))
	top := w.desktop.TopLayer()
	if top == nil {
		return b.String()
	}
	max := themeEditorMaxScroll()
	for k := 0; k < max; k++ {
		top.Root.BubbleType(tui.TypeEvent{Key: tui.KeyDown})
		b.WriteString(screenText(w))
	}
	for k := 0; k < max; k++ {
		top.Root.BubbleType(tui.TypeEvent{Key: tui.KeyUp})
	}
	return b.String()
}

// scrollEditorToReveal scrolls the open theme editor until needle is on screen, returning
// true if it became visible at some offset (and leaving it scrolled there). It is the
// single-target counterpart to editorScrollAggregate for tests that want to prove a
// below-the-fold role is reachable by scrolling, not merely present in an aggregate.
func scrollEditorToReveal(w *Workbench, needle string) bool {
	top := w.desktop.TopLayer()
	// Reset to the top first so the search covers every offset regardless of where a prior
	// reveal left the scroll (callers iterate roles in both columns, jumping top↔bottom).
	if top != nil {
		for k := 0; k < themeEditorMaxScroll(); k++ {
			top.Root.BubbleType(tui.TypeEvent{Key: tui.KeyUp})
		}
	}
	if containsOnScreen(screenText(w), needle) {
		return true
	}
	if top == nil {
		return false
	}
	for k := 0; k < themeEditorMaxScroll(); k++ {
		top.Root.BubbleType(tui.TypeEvent{Key: tui.KeyDown})
		if containsOnScreen(screenText(w), needle) {
			return true
		}
	}
	return false
}

// editorGrid renders the current screen of w as a rune grid (one rune per cell), the same
// representation issue267Render produces but for the editor's current scroll offset.
func editorGrid(w *Workbench) [][]rune {
	w.desktop.Redraw()
	rows := make([][]rune, w.app.Height())
	for y := 0; y < w.app.Height(); y++ {
		row := make([]rune, w.app.Width())
		for x := 0; x < w.app.Width(); x++ {
			ch := w.app.ReadCell(x, y).Ch
			if ch == 0 {
				ch = ' '
			}
			row[x] = ch
		}
		rows[y] = row
	}
	return rows
}

// editorRowPos is a needle's reconstructed placement in the scrolling editor: which column
// it is in and its column-local logical row (header rows included), recovered across scroll
// offsets so positional assertions survive the viewport. found is false if it never appeared.
type editorRowPos struct {
	isRight bool
	logical int
	found   bool
}

// editorScrollFind scrolls the open editor through every offset (0..maxScroll) and locates
// each needle, returning its column and logical row. Logical row is computed as
// (screenRow - h0) + offset, where h0 is the screen row of the first header ("Session
// output ─") at offset 0 — both columns share that content top, so it is the stable
// logical-row-0 reference for either column. The editor is restored to the top before
// returning. Needles are matched as substrings of a single rendered row (findRunes), so pass
// "<label>:" for roles and "<title> ─" for section headers.
func editorScrollFind(w *Workbench, needles []string) map[string]editorRowPos {
	res := make(map[string]editorRowPos, len(needles))
	for _, n := range needles {
		res[n] = editorRowPos{}
	}
	h0, _, ok := findRunes(editorGrid(w), "Session output ─")
	if !ok {
		return res
	}
	top := w.desktop.TopLayer()
	max := themeEditorMaxScroll()
	for off := 0; off <= max; off++ {
		grid := editorGrid(w)
		for _, n := range needles {
			if res[n].found {
				continue
			}
			if r, c, ok := findRunes(grid, n); ok {
				res[n] = editorRowPos{isRight: issue267labelColumn(c) == "right", logical: (r - h0) + off, found: true}
			}
		}
		if off < max && top != nil {
			top.Root.BubbleType(tui.TypeEvent{Key: tui.KeyDown})
		}
	}
	if top != nil {
		for k := 0; k < max; k++ {
			top.Root.BubbleType(tui.TypeEvent{Key: tui.KeyUp})
		}
	}
	return res
}
