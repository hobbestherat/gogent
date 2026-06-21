package ui

// Render-integration tests for issue #245.
//
// The sibling file issue245_styled_foldable_child_test.go tests the model-side
// behaviour through AllText() and in-package state. That is enough for the three
// workaround removals, but it CANNOT see the actual on-screen rendering: turbotui
// keeps the entry-tree fields (children/spans/foldable/collapsed) and the whole
// layout/draw path unexported, and AllText() recurses children unconditionally of
// collapse — so "body is a foldable, indented child" is invisible there.
//
// These tests close that gap by driving the REAL Draw path the production frame
// uses: a headless Workbench (newTestWorkbench) rendered via desktop.Redraw, with
// the on-screen cells read back through w.app.ReadCell — the same harness the issue
// #227 and #233 render tests use. The rendered cells DO reveal the fold marker,
// the indent, and whether folding hides the body. Each of these fails under the OLD
// m.view.AddStyled topology (verified by mutation: the marker vanishes and the body
// renders flush-left), so they guard the central change of issue #245 directly.

import (
	"strings"
	"testing"
)

// issue245Rows renders a workbench frame and returns the on-screen rows (one per
// terminal line), NUL cells turned to spaces. Equivalent to screenText but returns
// the slice so callers can scan individual rows.
func issue245Rows(w *Workbench) []string {
	return strings.Split(screenText(w), "\n")
}

// issue245RowContaining returns the index of the first row containing sub, or -1.
func issue245RowContaining(rows []string, sub string) int {
	for i, r := range rows {
		if strings.Contains(r, sub) {
			return i
		}
	}
	return -1
}

// issue245ColOf returns the rune column at which sub begins within row, or -1. It
// works in runes (the fold markers ▾/▸ are multi-byte) so callers can read the
// cell back via w.app.ReadCell(col, rowIdx).
func issue245ColOf(row string, sub string) int {
	rs := []rune(row)
	rsub := []rune(sub)
	for i := 0; i+len(rsub) <= len(rs); i++ {
		match := true
		for j := 0; j < len(rsub); j++ {
			if rs[i+j] != rsub[j] {
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

// issue245ExpandedGogent is the marker+header pattern an EXPANDED foldable Gogent:
// header paints on screen: the ▾ glyph, a space, then "Gogent:". Under the old
// top-level topology the header has no children and so no marker, rendering as a
// bare "Gogent:" flush against the border — this substring is absent.
const (
	issue245ExpandedGogent  = "▾ Gogent:"
	issue245CollapsedGogent = "▸ Gogent:"
	issue245ExpandedYou     = "▾ You:"
)

// newIssue245RenderWorkbench builds a headless Workbench with one focused session,
// notifications silenced, a distinct user question, and a rich assistant answer
// whose body carries a unique sentinel and an inline-code span. Rich rendering is
// forced on with a known palette so the markers, indent and styling are deterministic.
func newIssue245RenderWorkbench(t *testing.T) (*Workbench, *SessionWindow) {
	t.Helper()
	withTestPalette(t)
	withRichState(t, true)
	w := newTestWorkbench(t)
	silenceNotifications(w)
	sw := w.openWindow("s1", "Session 1")
	sw.addUser("what is the answer")
	sw.addAssistant("# Heading\n\nUNIQUEBODYSENTINEL and `code`")
	return w, sw
}

// ---------------------------------------------------------------------------
// Acceptance #1 (core): the Gogent: header renders with a fold marker, exactly
// like the You: header — i.e. the body is now foldable children of the header.
// ---------------------------------------------------------------------------

// TestIssue245RenderAssistantHeaderHasFoldMarker asserts the expanded Gogent: row
// carries the ▾ marker. Under the old flush-left topology the header had no
// children and no marker, so this fails there.
func TestIssue245RenderAssistantHeaderHasFoldMarker(t *testing.T) {
	w, _ := newIssue245RenderWorkbench(t)
	rows := issue245Rows(w)

	gRow := issue245RowContaining(rows, "Gogent:")
	if gRow < 0 {
		t.Fatalf("no Gogent: row on screen\n%s", strings.Join(rows, "\n"))
	}
	if !strings.Contains(rows[gRow], issue245ExpandedGogent) {
		t.Errorf("expanded Gogent: header has no ▾ fold marker (body not foldable children):\nrow %d: %q", gRow, rows[gRow])
	}
	// Parity: the You: header — always a foldable parent — carries the same marker.
	if uRow := issue245RowContaining(rows, "You:"); uRow >= 0 && !strings.Contains(rows[uRow], issue245ExpandedYou) {
		t.Errorf("You: header unexpectedly lacks ▾ marker\nrow %d: %q", uRow, rows[uRow])
	}
}

// ---------------------------------------------------------------------------
// Acceptance #2: the body is INDENTED under the Gogent: header, like the You:
// body is. Under the old topology the body sat at top-level (flush-left, same
// column as the header), one indent level shallower than a real child.
// ---------------------------------------------------------------------------

// TestIssue245RenderAssistantBodyIndentedLikeUser asserts the assistant body text
// starts at the same screen column as the user body text (both are depth-1
// children). Under the old top-level topology the assistant body was a depth-0
// entry and sat two columns to the left.
func TestIssue245RenderAssistantBodyIndentedLikeUser(t *testing.T) {
	w, _ := newIssue245RenderWorkbench(t)
	rows := issue245Rows(w)

	gRow := issue245RowContaining(rows, "UNIQUEBODYSENTINEL")
	uRow := issue245RowContaining(rows, "what is the answer")
	if gRow < 0 || uRow < 0 {
		t.Fatalf("assistant body or user body not on screen\n%s", strings.Join(rows, "\n"))
	}
	gCol := issue245ColOf(rows[gRow], "UNIQUEBODYSENTINEL")
	uCol := issue245ColOf(rows[uRow], "what is the answer")
	if gCol < 0 || uCol < 0 {
		t.Fatalf("could not locate body text columns (assistant=%d user=%d)", gCol, uCol)
	}
	if gCol != uCol {
		t.Errorf("assistant body not indented like user body: assistant col %d, user col %d (expected equal, depth-1 children)\nassistant: %q\nuser:      %q",
			gCol, uCol, rows[gRow], rows[uRow])
	}
	// And it must NOT be flush against the panel border (the old top-level form):
	// a depth-1 child is two columns in from the border.
	if borderCol := issue245ColOf(rows[gRow], "│"); borderCol >= 0 && gCol == borderCol+1 {
		t.Errorf("assistant body is flush-left against the border (old top-level topology):\n%q", rows[gRow])
	}
}

// ---------------------------------------------------------------------------
// Acceptance #1 (folding): folding the assistant HIDES its body and flips the
// marker to ▸; unfolding restores the body. Under the old topology the header had
// no children, so folding hid nothing and no ▸ appeared.
// ---------------------------------------------------------------------------

// TestIssue245RenderFoldHidesBodyAndFlipsMarker folds the assistant record in place
// (the setCollapsed path the simplified setFold uses) and asserts the body leaves
// the screen, the marker becomes ▸, and unfolding brings both back.
func TestIssue245RenderFoldHidesBodyAndFlipsMarker(t *testing.T) {
	w, sw := newIssue245RenderWorkbench(t)

	rec := sw.transcript.lastAssistantRecord()
	if rec == nil {
		t.Fatal("no assistant record")
	}

	// Precondition: expanded, body visible, ▾ marker.
	rows := issue245Rows(w)
	if issue245RowContaining(rows, "UNIQUEBODYSENTINEL") < 0 {
		t.Fatalf("precondition: assistant body not visible when expanded\n%s", strings.Join(rows, "\n"))
	}

	// Fold just the assistant (the in-place path; a full setFold would fold every
	// record and muddy the marker check).
	sw.transcript.setCollapsed(rec, true)
	rows = issue245Rows(w)
	gRow := issue245RowContaining(rows, "Gogent:")
	if gRow < 0 {
		t.Fatalf("Gogent: header vanished after fold (should remain)\n%s", strings.Join(rows, "\n"))
	}
	if !strings.Contains(rows[gRow], issue245CollapsedGogent) {
		t.Errorf("folded Gogent: header did not flip to ▸ marker:\nrow %d: %q", gRow, rows[gRow])
	}
	if issue245RowContaining(rows, "UNIQUEBODYSENTINEL") >= 0 {
		t.Errorf("folding the assistant did NOT hide its body (still on screen):\n%s", strings.Join(rows, "\n"))
	}
	if issue245RowContaining(rows, "Heading") >= 0 {
		t.Errorf("folding the assistant did NOT hide the heading body row:\n%s", strings.Join(rows, "\n"))
	}

	// Unfold: body returns and marker flips back to ▾.
	sw.transcript.setCollapsed(rec, false)
	rows = issue245Rows(w)
	if issue245RowContaining(rows, "UNIQUEBODYSENTINEL") < 0 {
		t.Errorf("unfolding did NOT restore the assistant body:\n%s", strings.Join(rows, "\n"))
	}
	if gRow = issue245RowContaining(rows, "Gogent:"); gRow >= 0 && !strings.Contains(rows[gRow], issue245ExpandedGogent) {
		t.Errorf("unfolded Gogent: header did not flip back to ▾ marker:\nrow %d: %q", gRow, rows[gRow])
	}
}

// TestIssue245RenderFoldAllHidesRichAssistant folds via the setFold(all) path — the
// one the driver simplified — and confirms the rich assistant's body is hidden too,
// proving the simplified in-place fold works for rich records through the Draw path.
func TestIssue245RenderFoldAllHidesRichAssistant(t *testing.T) {
	w, sw := newIssue245RenderWorkbench(t)

	sw.transcript.setFold(true)
	rows := issue245Rows(w)
	if issue245RowContaining(rows, "UNIQUEBODYSENTINEL") >= 0 {
		t.Errorf("fold-all did not hide the rich assistant body:\n%s", strings.Join(rows, "\n"))
	}
	if gRow := issue245RowContaining(rows, "Gogent:"); gRow >= 0 && !strings.Contains(rows[gRow], issue245CollapsedGogent) {
		t.Errorf("fold-all did not mark the Gogent: header collapsed:\nrow %d: %q", gRow, rows[gRow])
	}
	// Headers themselves survive fold-all (only children hide).
	for _, header := range []string{"Gogent:", "You:"} {
		if issue245RowContaining(rows, header) < 0 {
			t.Errorf("fold-all hid the %q header itself (only children should hide):\n%s", header, strings.Join(rows, "\n"))
		}
	}
}

// ---------------------------------------------------------------------------
// Acceptance #3 (rich styling survives): the styled children render with
// per-span attributes — a Markdown heading paints bold, inline code paints in the
// code colour — so expanding shows the rich formatting.
// ---------------------------------------------------------------------------

// TestIssue245RenderRichStylingPreserved reads back the rendered cells of the
// heading and the inline code and asserts the heading is bold and the code uses
// the palette's code colour. (These attributes live on the styled spans, so they
// only render if the body is emitted via entry.AddStyled children.)
func TestIssue245RenderRichStylingPreserved(t *testing.T) {
	withTestPalette(t)
	w := newTestWorkbench(t)
	silenceNotifications(w)
	sw := w.openWindow("s1", "Session 1")
	sw.addAssistant("# Heading\n\nrun `code` now")

	rows := issue245Rows(w)
	hRow := issue245RowContaining(rows, "Heading")
	if hRow < 0 {
		t.Fatalf("heading body row not on screen\n%s", strings.Join(rows, "\n"))
	}
	// The heading's first rune must be bold.
	hCol := issue245ColOf(rows[hRow], "Heading")
	if hCol < 0 {
		t.Fatalf("could not locate Heading column")
	}
	hCell := w.app.ReadCell(hCol, hRow)
	if !hCell.Bold {
		t.Errorf("Markdown heading not rendered bold at (%d,%d): %+v", hCol, hRow, hCell)
	}

	// The inline-code word must carry the palette's code colour.
	codeRow := issue245RowContaining(rows, "code")
	if codeRow < 0 {
		t.Fatalf("inline-code body row not on screen\n%s", strings.Join(rows, "\n"))
	}
	cCol := issue245ColOf(rows[codeRow], "code")
	if cCol < 0 {
		t.Fatalf("could not locate code column")
	}
	cCell := w.app.ReadCell(cCol, codeRow)
	wantCode := mdPalette.code
	if cCell.FG != wantCode {
		t.Errorf("inline code not rendered in the code colour at (%d,%d): got FG=%+v, want %+v",
			cCol, codeRow, cCell.FG, wantCode)
	}
}

// ---------------------------------------------------------------------------
// Acceptance #4 (copy/export unchanged through the real view): AllText of the
// rendered session still contains the verbatim body text after the topology switch,
// so copy/export/yank are unaffected.
// ---------------------------------------------------------------------------

// TestIssue245RenderSessionCopyTextUnchanged confirms the live session view's
// AllText (what copy/export read) still carries the assistant body verbatim, and
// that folding does not alter it (AllText recurses children regardless of collapse).
func TestIssue245RenderSessionCopyTextUnchanged(t *testing.T) {
	w, sw := newIssue245RenderWorkbench(t)
	_ = w

	all := sw.transcript.view.AllText()
	for _, want := range []string{"Gogent:", "UNIQUEBODYSENTINEL", "Heading", "code"} {
		if !strings.Contains(all, want) {
			t.Errorf("AllText lost %q after topology switch:\n%s", want, all)
		}
	}

	// Folding must not change the copyable text.
	rec := sw.transcript.lastAssistantRecord()
	sw.transcript.setCollapsed(rec, true)
	folded := sw.transcript.view.AllText()
	if folded != all {
		t.Errorf("folding changed AllText (copy/export would depend on fold state)\nexpanded:\n%s\nfolded:\n%s", all, folded)
	}
}

// ---------------------------------------------------------------------------
// Marker robustness: assert the fold marker glyph-agnostically. The Draw path
// paints the marker cell BOLD (widget_textview.go draw: Bold: true), two cells
// before the header text. Asserting "a bold, non-space glyph sits at col-2" avoids
// coupling the test to turbotui's specific marker rune (▾/▸/▼/▶…).
// ---------------------------------------------------------------------------

// TestIssue245RenderMarkerCellIsBoldGlyph reads the cell two columns before the
// "Gogent:" text and asserts it is a bold, non-space marker — present under the new
// child topology, absent (a plain border/space cell) under the old top-level one.
func TestIssue245RenderMarkerCellIsBoldGlyph(t *testing.T) {
	w, _ := newIssue245RenderWorkbench(t)
	rows := issue245Rows(w)

	gRow := issue245RowContaining(rows, "Gogent:")
	if gRow < 0 {
		t.Fatalf("no Gogent: row on screen\n%s", strings.Join(rows, "\n"))
	}
	gCol := issue245ColOf(rows[gRow], "Gogent:")
	if gCol < 2 {
		t.Fatalf("Gogent: text at col %d leaves no room for a marker before it: %q", gCol, rows[gRow])
	}
	mCell := w.app.ReadCell(gCol-2, gRow)
	if !mCell.Bold || mCell.Ch == ' ' || mCell.Ch == 0 {
		t.Errorf("no bold fold-marker glyph at (%d,%d) (2 before Gogent:): %+v\nrow: %q",
			gCol-2, gRow, mCell, rows[gRow])
	}
}

// ---------------------------------------------------------------------------
// Scenario: a folded rich assistant stays folded while a later answer renders
// expanded. Folding is per-record state, so the new answer is unaffected and the
// old one keeps its ▸ marker and hidden body. (Previously untested.)
// ---------------------------------------------------------------------------

// TestIssue245RenderFoldedAssistantStaysFoldedAcrossNewAnswer folds the first
// assistant, adds a second, and asserts the first stays folded (marker ▸, body
// hidden) while the second renders expanded (marker ▾, body visible).
func TestIssue245RenderFoldedAssistantStaysFoldedAcrossNewAnswer(t *testing.T) {
	withTestPalette(t)
	withRichState(t, true)
	w := newTestWorkbench(t)
	silenceNotifications(w)
	sw := w.openWindow("s1", "Session 1")
	sw.addAssistant("# FirstHeading\n\nFIRSTBODY")

	first := sw.transcript.lastAssistantRecord()
	sw.transcript.setCollapsed(first, true) // fold the first answer

	sw.addAssistant("# SecondHeading\n\nSECONDBODY") // a later, expanded answer

	rows := issue245Rows(w)
	all := strings.Join(rows, "\n")

	// First answer stays folded: its body is hidden and its header shows ▸.
	if strings.Contains(all, "FIRSTBODY") {
		t.Errorf("folded first assistant body leaked onto screen:\n%s", all)
	}
	if !strings.Contains(all, issue245CollapsedGogent) {
		t.Errorf("folded first assistant header missing ▸ marker:\n%s", all)
	}
	// Second answer renders expanded: its body is visible and its header shows ▾.
	if !strings.Contains(all, "SECONDBODY") {
		t.Errorf("new (expanded) assistant body not on screen:\n%s", all)
	}
	if !strings.Contains(all, issue245ExpandedGogent) {
		t.Errorf("new assistant header missing ▾ marker:\n%s", all)
	}
}

// ---------------------------------------------------------------------------
// Scenario: a rich record added while a SEARCH is active goes through the full
// m.render() rebuild path (transcriptModel.add → filtering → render), exercising
// the guard-removed renderOne during a filtered re-render. It must still emerge as
// styled, foldable children matching the query. (Previously untested.)
// ---------------------------------------------------------------------------

// TestIssue245RenderRichRecordAddedWhileSearching activates a search, then adds an
// assistant whose body matches, and asserts it renders through the full-rebuild
// path with the fold marker and indented body.
func TestIssue245RenderRichRecordAddedWhileSearching(t *testing.T) {
	withTestPalette(t)
	withRichState(t, true)
	w := newTestWorkbench(t)
	silenceNotifications(w)
	sw := w.openWindow("s1", "Session 1")
	sw.addUser("an unrelated question") // does not match the query below

	// Activate a search that only the upcoming answer matches.
	sw.transcript.setQuery("MATCHME")
	// Sanity: with only the non-matching user record, nothing renders but the note.
	preRows := issue245Rows(w)
	if issue245RowContaining(preRows, "an unrelated question") >= 0 {
		t.Fatalf("precondition: non-matching record should be hidden by the search\n%s", strings.Join(preRows, "\n"))
	}

	// Add a rich assistant that matches the query → add() takes the full render() path.
	sw.addAssistant("# Heading\n\nMATCHME body")

	rows := issue245Rows(w)
	all := strings.Join(rows, "\n")
	gRow := issue245RowContaining(rows, "Gogent:")
	if gRow < 0 {
		t.Fatalf("matching rich assistant not rendered through the search-rebuild path:\n%s", all)
	}
	if !strings.Contains(rows[gRow], issue245ExpandedGogent) {
		t.Errorf("rich record from search-rebuild path lacks ▾ marker:\nrow %d: %q", gRow, rows[gRow])
	}
	if issue245RowContaining(rows, "MATCHME") < 0 {
		t.Errorf("rich record body not visible after search-rebuild render:\n%s", all)
	}
	// The non-matching user record stays hidden.
	if issue245RowContaining(rows, "an unrelated question") >= 0 {
		t.Errorf("non-matching record leaked through the active search:\n%s", all)
	}
}
