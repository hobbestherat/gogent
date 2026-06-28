package ui

import (
	"fmt"
	"testing"
)

// Round 4 of the issue #548 suite — formal "all sizes" sweep and a focused-state
// exact-face assertion, complementing the per-size tests in rounds 1–3.
//
// The driver's omission (themeEditorFooterButtonRect still W:11 while the button is
// W:12) remains, so the helper-drift guards in the earlier files still fail. These
// tests do not depend on that helper.

// TestIssue548NoTruncationAcrossTerminalWidthSweep is the formal "all sizes"
// acceptance check (criterion 1): across a sweep of terminal widths — sub-floor,
// exact-floor, and well above floor — the truncated caption "Save A…" must NEVER
// appear on screen and the Save As button must always be laid out at W:12. The
// dialog width is pinned at 83, so the sweep varies centering/clipping, not the
// button; this proves no terminal width re-triggers the original W:11 truncation.
func TestIssue548NoTruncationAcrossTerminalWidthSweep(t *testing.T) {
	for _, tw := range []int{60, 70, 80, 83, 90, 100, 120, 160, 200} {
		tw := tw
		t.Run(fmt.Sprintf("w%d", tw), func(t *testing.T) {
			w := openEditor548(t, tw, 24, savableHandlers548())
			grid := editorGrid(w)
			if _, _, ok := findRunes(grid, "Save A…"); ok {
				t.Errorf("width %d: the truncated caption \"Save A…\" is on screen — the original W:11 truncation re-manifests at this terminal width", tw)
			}
			btn := findFooterButtonByRect(w, 12, footerButtonRowY(w), 12)
			if btn == nil {
				t.Fatalf("width %d: Save As button not laid out at {X:12 W:12}", tw)
			}
			if btn.Bounds.W != 12 {
				t.Errorf("width %d: Save As button W = %d, want 12 (width-invariant)", tw, btn.Bounds.W)
			}
		})
	}
}

// TestIssue548FocusedFaceIsExactChevronCaption is the cell-level counterpart of
// TestIssue548SaveAsFaceIsExactBracketedCaption for the FOCUSED state: turbotui
// swaps "[ … ]" for "►…◄" chevrons (single-cell each), giving avail = W-2 = 10, so
// the 8-cell caption centres with one cell of slack each side. The focused face must
// spell EXACTLY "► Save As… ◄" — pinning that focus does not truncate or mis-centre
// the caption (a guard against a future chrome change breaking the focused render).
func TestIssue548FocusedFaceIsExactChevronCaption(t *testing.T) {
	w := openEditor548(t, 100, 30, savableHandlers548())
	btn := findFooterButtonByRect(w, 12, footerButtonRowY(w), 12)
	if btn == nil {
		t.Fatal("Save As button not found at {X:12 W:12}")
	}
	w.desktop.SetFocus(btn)
	w.desktop.Redraw()

	got := buttonCaption(t, w, btn)
	const want = "► Save As… ◄"
	if got != want {
		t.Errorf("focused Save As face = %q, want exactly %q — the focused chevron caption is not the full, centred, untruncated render", got, want)
	}
}
