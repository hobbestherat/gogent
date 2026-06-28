package ui

import (
	"testing"

	"gogent/internal/config"

	tui "github.com/hobbestherat/turbotui"
	tv "github.com/hobbestherat/turbotui/turbotv"
)

// Round 2 of the issue #548 test suite — edge cases, error paths and deeper
// defect guards that complement theme_issue548_test.go.
//
// Context: the driver widened the Save As button to W:12 in BOTH theme_editor.go
// geometry sites (creation + relayout) but did NOT update the
// themeEditorFooterButtonRect helper (theme_issue462_test.go:130, still W:11).
// These tests:
//   - guard that drift across ALL footer buttons (not just Save As), so the same
//     class of omission can't recur silently;
//   - exercise the Save As WORKFLOW through error/cancellation paths by clicking
//     the button via its rendered caption (robust to the width, and the only way
//     to reach that workflow while the #462 helper-based click is broken);
//   - cover the keyboard-activation and resize-shrink edges the round-1 tests did
//     not reach;
//   - pin the root-cause width math (StringWidth + buttonWidth) as a fast unit
//     check independent of the UI harness.

// mustFindSaveAsByCaption548 resolves the Save As button by its on-screen caption
// and fails loudly if it is not legible/clickable. It is the width-robust analogue
// of clickThemeButton("Save As…") (which is currently blocked by the stale helper).
func mustFindSaveAsByCaption548(t *testing.T, w *Workbench) *tv.VisualComponent {
	t.Helper()
	btn := findFooterButtonByCaption(t, w, "Save As…")
	if btn == nil {
		t.Fatalf(`could not target the Save As button by its rendered "Save As…" caption`)
	}
	return btn
}

// ----------------------------------------------------------------------------
// Defect guard — the footer-button helper must track reality for EVERY button.
// ----------------------------------------------------------------------------

// TestIssue548AllFooterHelpersMatchLaidOutButtons extends the round-1 helper guard
// to all five footer buttons: themeEditorFooterButtonRect must return the SAME rect
// the editor actually lays out, or clickThemeButton (which resolves by rect) and the
// #462 tests that use it cannot find the button. The "Save As…" subcase FAILS until
// the driver updates themeEditorFooterButtonRect (theme_issue462_test.go:130) from
// W:11 to W:12; the other four pass and are guarded against future drift.
func TestIssue548AllFooterHelpersMatchLaidOutButtons(t *testing.T) {
	w := openEditor548(t, 100, 30, savableHandlers548())
	row := footerButtonRowY(w)
	dialogW := dialogBounds(w).W
	cases := []struct {
		name  string
		x, wd int
	}{
		{"Reset", 2, 9},
		{"Save As…", 12, 12},
		{"Delete", 24, 10},
		{"Save", dialogW - 24, 9},
		{"Cancel", dialogW - 13, 10},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			actual := findFooterButtonByRect(w, c.x, row, c.wd)
			if actual == nil {
				t.Fatalf("actual %q button not found at the laid-out {X:%d W:%d}", c.name, c.x, c.wd)
			}
			helper := themeEditorFooterButtonRect(w, c.name)
			if helper != actual.Bounds {
				t.Errorf("themeEditorFooterButtonRect(%q) = %+v but the laid-out button = %+v — "+
					"the helper has drifted from reality; clickThemeButton(%q) and the #462 tests cannot resolve it",
					c.name, helper, actual.Bounds, c.name)
			}
		})
	}
}

// ----------------------------------------------------------------------------
// No regressions — every footer button is wide enough to avoid caption truncation.
// ----------------------------------------------------------------------------

// TestIssue548FooterWidthsAvoidCaptionTruncation pins the no-truncation invariant
// for every footer button: W >= buttonWidth(label) (= StringWidth(label) + 4 chrome).
// This is the exact inequality W:11 violated for "Save As…" (11 < 12). It does not
// depend on themeEditorFooterButtonRect, so it is not blocked by the stale helper,
// and it would re-fail immediately if any footer button were ever narrowed under
// its caption+chrome width.
func TestIssue548FooterWidthsAvoidCaptionTruncation(t *testing.T) {
	w := openEditor548(t, 100, 30, savableHandlers548())
	row := footerButtonRowY(w)
	dialogW := dialogBounds(w).W
	cases := []struct {
		label string
		x, wd int
	}{
		{"Reset", 2, 9},
		{"Save As…", 12, 12},
		{"Delete", 24, 10},
		{"Save", dialogW - 24, 9},
		{"Cancel", dialogW - 13, 10},
	}
	for _, c := range cases {
		btn := findFooterButtonByRect(w, c.x, row, c.wd)
		if btn == nil {
			t.Fatalf("%q button not found at {X:%d W:%d}", c.label, c.x, c.wd)
		}
		if need := buttonWidth(c.label); btn.Bounds.W < need {
			t.Errorf("%q button W=%d < buttonWidth %d (caption %d + chrome 4) — the caption would truncate",
				c.label, btn.Bounds.W, need, need-4)
		}
	}
}

// ----------------------------------------------------------------------------
// Usability — keyboard activation (the discoverability fix must help keyboard users).
// ----------------------------------------------------------------------------

// TestIssue548SaveAsActivatesByKeyboard is the keyboard edge case: focusing the Save
// As button and pressing Enter must activate it (Button.handleType -> press -> saveAs)
// and open the name input, exactly as a click does. A discoverability fix that only
// helps mouse users would leave the workflow hidden from keyboard navigation.
func TestIssue548SaveAsActivatesByKeyboard(t *testing.T) {
	st := &theme462Store{active: config.ThemeConfig{Name: "default"}}
	w := openEditor548(t, 100, 30, st.handlers())
	btn := findFooterButtonByRect(w, 12, footerButtonRowY(w), 12)
	if btn == nil {
		t.Fatal("Save As button not found at {X:12 W:12}")
	}
	w.desktop.SetFocus(btn)
	btn.BubbleType(tui.TypeEvent{Key: tui.KeyEnter})
	w.desktop.Redraw()

	if top := w.desktop.TopLayer(); top == nil || top.Name != "input-dialog" {
		t.Errorf("keyboard Enter on the focused Save As button did not open the name input (top=%v) — "+
			"keyboard users cannot reach the copy-theme workflow", nameOrNil(top))
	}
}

// nameOrNil returns the top layer's name for diagnostics, or "<none>".
func nameOrNil(l *tv.Layer) string {
	if l == nil {
		return "<none>"
	}
	return l.Name
}

// ----------------------------------------------------------------------------
// No regressions — the caption survives a SHRINK resize (the other relayout branch).
// ----------------------------------------------------------------------------

// TestIssue548CaptionSurvivesShrinkResize complements the round-1 grow-resize test
// with the opposite direction: open on a large terminal, shrink to the 83×24 floor,
// and assert the caption is still full and the button still W:12. relayout() runs on
// both grow and shrink; this guards the shrink branch didn't leave the button at the
// open-time width.
func TestIssue548CaptionSurvivesShrinkResize(t *testing.T) {
	w := openEditor548(t, 200, 50, savableHandlers548())
	w.app.Resize(83, 24) // shrink to the 83-wide floor
	w.desktop.Redraw()

	grid := editorGrid(w)
	if _, _, ok := findRunes(grid, "[ Save As… ]"); !ok {
		t.Errorf(`after shrinking to the floor the full "[ Save As…" ]" caption is NOT on screen — relayout re-truncated it`)
	}
	if findFooterButtonByRect(w, 12, footerButtonRowY(w), 12) == nil {
		t.Error("after shrinking to the floor the Save As button is not at {X:12 W:12}")
	}
}

// ----------------------------------------------------------------------------
// Error handling — the Save As workflow's conflict / cancellation paths.
// ----------------------------------------------------------------------------

// TestIssue548SaveAsDuplicateNameConfirmsViaLegibleButton drives the duplicate-name
// conflict path through the now-legible button (clicked by caption, not the stale
// helper): a name that already exists must open a confirm before overwriting.
// Declining leaves the saved list untouched; accepting overwrites in place, keeping
// the original casing. This is the #462 error path the stale helper currently blocks
// in TestIssue462SaveAsDuplicateConfirmsBeforeOverwrite.
func TestIssue548SaveAsDuplicateNameConfirmsViaLegibleButton(t *testing.T) {
	st := &theme462Store{
		active: config.ThemeConfig{Name: "default"},
		saved:  []config.NamedTheme{{Name: "Original", Theme: config.ThemeConfig{Name: "default", Overrides: map[string]string{"user": "#FF0000"}}}},
	}

	t.Run("decline keeps original", func(t *testing.T) {
		w := openEditor548(t, 100, 30, st.handlers())
		clickButton548(mustFindSaveAsByCaption548(t, w))
		box := inputDialogBox(t, w)
		for _, r := range "original" { // case-insensitive duplicate of "Original"
			typeDlgRune(box, r)
		}
		submitDlg(box)
		w.desktop.Redraw()

		if top := w.desktop.TopLayer(); top == nil || top.Name != "confirm-dialog" {
			t.Fatalf("a duplicate name did not open the confirm-dialog (top=%v)", nameOrNil(top))
		}
		clickTopButtonByText(t, w, confirmLabel(false)) // decline (No)
		w.desktop.Redraw()

		if len(st.setSaved) != 0 {
			t.Errorf("declining the overwrite still flushed SetSavedThemes: %+v", st.setSaved)
		}
		if len(st.saved) != 1 || st.saved[0].Name != "Original" || st.saved[0].Theme.Overrides["user"] != "#FF0000" {
			t.Errorf("declining mutated the original saved theme: %+v", st.saved)
		}
	})

	t.Run("accept overwrites in place keeping original casing", func(t *testing.T) {
		w := openEditor548(t, 100, 30, st.handlers())
		clickButton548(mustFindSaveAsByCaption548(t, w))
		box := inputDialogBox(t, w)
		for _, r := range "ORIGINAL" { // case-insensitive duplicate
			typeDlgRune(box, r)
		}
		submitDlg(box)
		w.desktop.Redraw()

		if top := w.desktop.TopLayer(); top == nil || top.Name != "confirm-dialog" {
			t.Fatalf("a duplicate name did not open the confirm-dialog (top=%v)", nameOrNil(top))
		}
		clickTopButtonByText(t, w, confirmLabel(true)) // accept (Yes)
		w.desktop.Redraw()

		if len(st.setSaved) != 1 {
			t.Fatalf("accepting the overwrite did not flush SetSavedThemes: %+v", st.setSaved)
		}
		flushed := st.setSaved[0]
		if len(flushed) != 1 || flushed[0].Name != "Original" {
			t.Errorf("overwrite did not replace in place keeping the original casing: %+v", flushed)
		}
	})
}

// TestIssue548SaveAsInputEscapeCancelsCleanly is the cancellation edge: pressing
// Escape in the Save As name input must cancel without creating a theme and return
// focus to the editor (showInputDialog -> finish("",false) -> saveAs early-returns).
func TestIssue548SaveAsInputEscapeCancelsCleanly(t *testing.T) {
	st := &theme462Store{active: config.ThemeConfig{Name: "default"}}
	w := openEditor548(t, 100, 30, st.handlers())
	clickButton548(mustFindSaveAsByCaption548(t, w))
	box := inputDialogBox(t, w)

	box.BubbleType(tui.TypeEvent{Key: tui.KeyEscape})
	w.desktop.Redraw()

	if len(st.setSaved) != 0 {
		t.Errorf("Escape in the Save As input still created a theme: %+v", st.setSaved)
	}
	if top := w.desktop.TopLayer(); top == nil || top.Name != "theme-editor" {
		t.Errorf("Escape did not close the input and return to the editor (top=%v)", nameOrNil(top))
	}
}

// ----------------------------------------------------------------------------
// Root cause — pin the caption/chrome width math that W:11 violated.
// ----------------------------------------------------------------------------

// TestIssue548CaptionWidthMathRootCause is a fast, harness-free pin on the premise
// of the fix: "…" (U+2026) is one terminal cell in turbotui's width model, so
// "Save As…" is 8 cells and the minimum non-truncating button width is 8 + 4 = 12
// (buttonWidth). W:11 < 12 is precisely the off-by-one that caused the truncation.
func TestIssue548CaptionWidthMathRootCause(t *testing.T) {
	if got := tui.StringWidth("Save As…"); got != 8 {
		t.Errorf("StringWidth(\"Save As…\") = %d, want 8 — the ellipsis must be one cell (issue #548 premise)", got)
	}
	if got := buttonWidth("Save As…"); got != 12 {
		t.Errorf("buttonWidth(\"Save As…\") = %d, want 12 (caption 8 + \"[ … ]\" chrome 4)", got)
	}
	if min := buttonWidth("Save As…"); min <= 11 {
		t.Errorf("buttonWidth(\"Save As…\") = %d — W:11 would NOT truncate, contradicting issue #548", min)
	}
}
