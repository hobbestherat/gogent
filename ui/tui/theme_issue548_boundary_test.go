package ui

import (
	"testing"
)

// Round 3 of the issue #548 suite — two sharp, non-redundant boundary checks
// complementing theme_issue548_test.go and theme_issue548_edges_test.go.
//
// The driver's omission (themeEditorFooterButtonRect still W:11 while the button is
// W:12) remains, so the helper-drift guards in the earlier files still fail. These
// two tests do NOT depend on that helper and pin the fix at the cell level and under
// sub-floor clipping.

// TestIssue548SaveAsFaceIsExactBracketedCaption is the sharpest no-truncation proof:
// it reads the Save As button's face cells and asserts they spell EXACTLY
// "[ Save As… ]" — 12 cells, every one correct, no trailing/missing cell. Unlike a
// substring search, an exact-equality check fails if the caption were truncated
// ("[ Save A… ]"), padded, or shifted; it pins the precise rendered face the fix
// produces at W:12 = caption 8 + chrome 4.
func TestIssue548SaveAsFaceIsExactBracketedCaption(t *testing.T) {
	w := openEditor548(t, 100, 30, savableHandlers548())
	btn := findFooterButtonByRect(w, 12, footerButtonRowY(w), 12)
	if btn == nil {
		t.Fatal("Save As button not found at {X:12 W:12}")
	}
	got := buttonCaption(t, w, btn)
	const want = "[ Save As… ]"
	if got != want {
		t.Errorf("Save As button face = %q, want exactly %q — the caption is not the full, untruncated, exact-width render",
			got, want)
	}
}

// TestIssue548SaveAsSurvivesWidthClippingAndExtremeSizes covers the clipping
// boundary in two regimes:
//   - "sub-floor width": a terminal NARROWER than the 83-col dialog (70×22). The
//     dialog's right edge (Cancel/scrollbar/border) clips, but the left-anchored
//     Save As must stay legible and W:12 — the fix's benefit survives the
//     right-edge clipping that any sub-83-col terminal produces.
//   - "below floor in both dims": a terminal smaller than the 83×22 dialog in BOTH
//     dimensions (60×20). Here the whole footer row is off-screen (pre-existing —
//     the dialog is simply larger than the terminal; the design acknowledges
//     sub-floor clipping), but the editor must open without panicking and the
//     button's geometry must still be W:12 (window-relative Bounds are
//     clip-independent), so the fix is applied even where it cannot be seen.
func TestIssue548SaveAsSurvivesWidthClippingAndExtremeSizes(t *testing.T) {
	t.Run("legible when only the right edge clips", func(t *testing.T) {
		w := openEditor548(t, 70, 22, savableHandlers548())
		grid := editorGrid(w)
		if _, _, ok := findRunes(grid, "Save As…"); !ok {
			t.Errorf(`"Save As…" is not legible at 70×22 — right-edge clipping ate the left-anchored button`)
		}
		if _, _, ok := findRunes(grid, "Save A…"); ok {
			t.Errorf(`the truncated "Save A…" is on screen at 70×22 — the caption is being clipped to 7 cells`)
		}
		if findFooterButtonByRect(w, 12, footerButtonRowY(w), 12) == nil {
			t.Error("the Save As button is not laid out at {X:12 W:12} at 70×22")
		}
	})

	t.Run("no panic and W:12 geometry below the floor in both dims", func(t *testing.T) {
		w := openEditor548(t, 60, 20, savableHandlers548()) // reaching here = no panic on open
		btn := findFooterButtonByRect(w, 12, footerButtonRowY(w), 12)
		if btn == nil {
			t.Fatal("the Save As button is not laid out at {X:12 W:12} on a 60×20 terminal")
		}
		if btn.Bounds.W != 12 {
			t.Errorf("Save As button W = %d at 60×20, want 12 (geometry is clip-independent)", btn.Bounds.W)
		}
		// The footer is legitimately off-screen here (dialog 83×22 > terminal 60×20),
		// so the caption need not be visible — but the truncated form must not be the
		// one rendered when any of it is visible.
	})
}
