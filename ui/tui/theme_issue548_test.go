package ui

import (
	"strings"
	"testing"

	"gogent/internal/config"

	tui "github.com/hobbestherat/turbotui"
	tv "github.com/hobbestherat/turbotui/turbotv"
)

// Issue #548 — the Themes editor "Save As…" footer button was declared W:11, one
// column too narrow for its 8-column caption "Save As…" ("…" U+2026 is one cell).
// turbotui's Button.draw wraps the caption in "[ … ]" chrome (4 cells) and
// ellipsises anything that does not fit, so W:11 left only 7 caption cells and the
// button rendered "[ Save A… ]" — both a cosmetic truncation and a discoverability
// bug, since "Save As…" is the ONLY entry point to the non-destructive copy-theme
// workflow (#462).
//
// The fix widens the button to W:12 (= caption 8 + chrome 4) in the two geometry
// sites in theme_editor.go (creation + relayout) and — per the design — in the
// themeEditorFooterButtonRect test helper.
//
// These tests pin the fix directly against the REAL laid-out button and the REAL
// rendered screen, NOT through themeEditorFooterButtonRect (which the #462 tests
// use to resolve the button by rect). Resolving by rect or by the on-screen
// caption keeps these tests honest about what a user actually sees and keeps them
// independent of that helper, so a width change can't be "fixed" in only two of the
// three sites without one of these failing.
//
// NOTE: TestIssue548FooterHelperMatchesLaidOutButton intentionally FAILS until the
// driver updates the test helper (theme_issue462_test.go:130) from W:11 to W:12.
// It documents that omission; the three #462 Save As tests fail for the same root
// cause (they resolve the button through that stale helper).

// openEditor548 opens the theme editor on a terminal resized to termW×termH with
// the given handlers, redrawing once so the first frame is painted. It mirrors
// openThemeEditor462Raw but lets each test pick the terminal size (the dialog
// floors at 83×22, so terminals ≥83 wide show the full footer including the
// right-anchored Save/Cancel buttons).
func openEditor548(t *testing.T, termW, termH int, h Handlers) *Workbench {
	t.Helper()
	issue204RestoreTheme(t)
	w := newTestWorkbench(t)
	w.app.Resize(termW, termH)
	w.SetHandlers(h)
	w.showThemeEditor()
	w.desktop.Redraw()
	return w
}

// savableHandlers548 returns a handler set with all four theme handlers wired
// (GetTheme/SetTheme/GetSavedThemes/SetSavedThemes), so the editor offers the Save
// As/Delete actions (savable == true). It does not record calls — use it for layout
// and rendering tests that only need the button to exist.
func savableHandlers548() Handlers {
	var saved []config.NamedTheme
	return Handlers{
		GetTheme:       func() config.ThemeConfig { return config.ThemeConfig{Name: "default"} },
		SetTheme:       func(config.ThemeConfig) {},
		GetSavedThemes: func() []config.NamedTheme { return cloneNamedThemes(saved) },
		SetSavedThemes: func(t []config.NamedTheme) { saved = cloneNamedThemes(t) },
	}
}

// footerButtonRowY is the window-relative Y of the action-button row.
func footerButtonRowY(w *Workbench) int { return dialogBounds(w).H - 3 }

// findFooterButtonByRect returns the footer button whose window-relative Bounds
// equal {X:x, Y:y, W:wd, H:1}, or nil. Buttons are DrawOutside components with an
// OnClickFn. Resolving by the exact rect (rather than by caption) lets a test
// assert a specific button's width without the "Save" ⊂ "Save As…" caption
// ambiguity, and without depending on themeEditorFooterButtonRect.
func findFooterButtonByRect(w *Workbench, x, y, wd int) *tv.VisualComponent {
	want := tv.Rect{X: x, Y: y, W: wd, H: 1}
	for _, c := range dialogDescendants(w) {
		if c.DrawOutside && c.OnClickFn != nil && c.Bounds == want {
			return c
		}
	}
	return nil
}

// findFooterButtonByCaption returns the DrawOutside footer button whose absolute
// bounds contain the first on-screen occurrence of caption, or nil. It is the
// caption-driven counterpart of findFooterButtonByRect, robust to the exact width,
// and is how a user actually targets the button (by its visible label).
func findFooterButtonByCaption(t *testing.T, w *Workbench, caption string) *tv.VisualComponent {
	t.Helper()
	row, col, ok := findRunes(editorGrid(w), caption)
	if !ok {
		return nil
	}
	for _, c := range dialogDescendants(w) {
		if !c.DrawOutside || c.OnClickFn == nil {
			continue
		}
		abs := c.AbsoluteBounds()
		if col >= abs.X && col < abs.X+abs.W && row >= abs.Y && row < abs.Y+abs.H {
			return c
		}
	}
	return nil
}

// themeFooterButtons returns every action button on the theme editor's footer row
// (window-relative Y == height-3, H == 1, DrawOutside with an OnClickFn).
func themeFooterButtons(w *Workbench) []*tv.VisualComponent {
	y := footerButtonRowY(w)
	var out []*tv.VisualComponent
	for _, c := range dialogDescendants(w) {
		if c.DrawOutside && c.OnClickFn != nil && c.Bounds.Y == y && c.Bounds.H == 1 {
			out = append(out, c)
		}
	}
	return out
}

// buttonCaption reads the rendered text across a button's face on its centred
// caption row (the single row for an H:1 button). It is the per-button view of what
// a user sees, including the "[ … ]" / "►…◄" chrome, so a test can assert the full
// caption survived inside it.
func buttonCaption(t *testing.T, w *Workbench, btn *tv.VisualComponent) string {
	t.Helper()
	grid := editorGrid(w)
	abs := btn.AbsoluteBounds()
	y := abs.Y + abs.H/2
	if y < 0 || y >= len(grid) {
		return ""
	}
	row := grid[y]
	var b strings.Builder
	for x := abs.X; x < abs.X+abs.W; x++ {
		if x >= 0 && x < len(row) {
			b.WriteRune(row[x])
		}
	}
	return b.String()
}

// clickButton548 drives a full press (mouse-down then mouse-up at the button
// centre) of an already-resolved button component — the exact path a real click
// takes, so the button's OnPress closure runs.
func clickButton548(btn *tv.VisualComponent) {
	abs := btn.AbsoluteBounds()
	cx, cy := abs.X+abs.W/2, abs.Y+abs.H/2
	btn.OnClickFn(btn, tui.ClickEvent{X: cx, Y: cy, Down: true})
	btn.OnClickFn(btn, tui.ClickEvent{X: cx, Y: cy, Down: false})
}

// rectsHorizontallyOverlap reports whether two same-row rects share any column.
// Flush-adjacent rects (a's right edge == b's left edge) do NOT overlap.
func rectsHorizontallyOverlap(a, b tv.Rect) bool {
	return a.X < b.X+b.W && b.X < a.X+a.W
}

// ----------------------------------------------------------------------------
// Goal match — the caption renders fully and is not truncated.
// ----------------------------------------------------------------------------

// TestIssue548SaveAsCaptionRendersFully is the headline regression: at the 83×22
// dialog floor the Save As button renders the full "[ Save As… ]" — not the
// truncated "[ Save A… ]" the W:11 bug produced. Both the bracketed literal
// (acceptance criterion's exact string) and the bare caption must be on screen, and
// the truncated form must be absent.
func TestIssue548SaveAsCaptionRendersFully(t *testing.T) {
	w := openEditor548(t, 100, 30, savableHandlers548())
	grid := editorGrid(w)

	if _, _, ok := findRunes(grid, "[ Save As… ]"); !ok {
		t.Errorf(`the full "[ Save As…" ]" caption is NOT on screen — the button is still truncated`)
	}
	if _, _, ok := findRunes(grid, "Save As…"); !ok {
		t.Errorf(`the "Save As…" caption is NOT on screen`)
	}
	// The truncated form "Save A…" (ellipsis directly after "A", no "s") appears ONLY
	// when the caption was clipped; it must be absent now.
	if _, _, ok := findRunes(grid, "Save A…"); ok {
		t.Errorf(`the truncated "Save A…" is on screen — the caption is still being clipped to 7 cells`)
	}
}

// TestIssue548SaveAsButtonWidthIs12 asserts the actually-laid-out Save As button is
// W:12 at X:12, resolved by rect (independent of themeEditorFooterButtonRect). This
// is the direct guard on the fix's value: W:11 here is the bug, W:12 is the fix.
func TestIssue548SaveAsButtonWidthIs12(t *testing.T) {
	w := openEditor548(t, 100, 30, savableHandlers548())
	btn := findFooterButtonByRect(w, 12, footerButtonRowY(w), 12)
	if btn == nil {
		t.Fatalf(`Save As button not found at the expected {X:12 W:12} rect — ` +
			`the fix did not widen it to W:12 (or it moved off X:12)`)
	}
	if btn.Bounds.W != 12 {
		t.Errorf("Save As button Bounds.W = %d, want 12 (caption 8 + chrome 4)", btn.Bounds.W)
	}
	if btn.Bounds.X != 12 {
		t.Errorf("Save As button Bounds.X = %d, want 12", btn.Bounds.X)
	}
}

// ----------------------------------------------------------------------------
// No regressions — no footer-button overlap; relayout keeps it correct.
// ----------------------------------------------------------------------------

// TestIssue548NoFooterButtonOverlap guards the layout-collision check in the design:
// after widening Save As to W:12 (cols 12–23) the footer buttons must not overlap.
// Save As and Delete become flush-adjacent (Save As ends at col 23, Delete starts at
// 24), which is allowed; any actual column overlap is a regression.
func TestIssue548NoFooterButtonOverlap(t *testing.T) {
	w := openEditor548(t, 100, 30, savableHandlers548())
	btns := themeFooterButtons(w)
	if got := len(btns); got != 5 {
		t.Fatalf("expected 5 footer buttons (Reset/Save As/Delete/Save/Cancel), got %d", got)
	}
	for i := 0; i < len(btns); i++ {
		for j := i + 1; j < len(btns); j++ {
			if rectsHorizontallyOverlap(btns[i].Bounds, btns[j].Bounds) {
				t.Errorf("footer buttons overlap: %+v and %+v", btns[i].Bounds, btns[j].Bounds)
			}
		}
	}
	// Specifically: Save As (X:12,W:12 → ends at col 24) must be flush with, not
	// under, Delete (X:24).
	saveAs := findFooterButtonByRect(w, 12, footerButtonRowY(w), 12)
	deleteBtn := findFooterButtonByRect(w, 24, footerButtonRowY(w), 10)
	if saveAs == nil || deleteBtn == nil {
		t.Fatalf("Save As or Delete button not found at its expected rect")
	}
	if saveAs.Bounds.X+saveAs.Bounds.W != deleteBtn.Bounds.X {
		t.Errorf("Save As right edge %d != Delete left edge %d — expected flush adjacency",
			saveAs.Bounds.X+saveAs.Bounds.W, deleteBtn.Bounds.X)
	}
	if rectsHorizontallyOverlap(saveAs.Bounds, deleteBtn.Bounds) {
		t.Errorf("Save As %+v overlaps Delete %+v", saveAs.Bounds, deleteBtn.Bounds)
	}
}

// TestIssue548CaptionStaysFullAfterResize guards the SECOND geometry site (the
// relayout resize handler): opening small then enlarging must keep the caption full
// and the button W:12. If the create site were W:12 but the relayout site left at
// W:11, a resize would shrink the button back and re-truncate it (#317 invariant:
// resized dialog == freshly-opened dialog).
func TestIssue548CaptionStaysFullAfterResize(t *testing.T) {
	w := openEditor548(t, 100, 24, savableHandlers548())

	// Enlarge the terminal; the editor's layer.OnResize -> relayout() re-applies the
	// button bounds from the live geometry.
	w.app.Resize(160, 50)
	w.desktop.Redraw()

	grid := editorGrid(w)
	if _, _, ok := findRunes(grid, "[ Save As… ]"); !ok {
		t.Errorf(`after resize the full "[ Save As…" ]" caption is NOT on screen — relayout re-truncated it (relayout site is not W:12)`)
	}
	if _, _, ok := findRunes(grid, "Save A…"); ok {
		t.Errorf(`after resize the truncated "Save A…" is on screen — relayout shrank the button back to W:11`)
	}
	btn := findFooterButtonByRect(w, 12, footerButtonRowY(w), 12)
	if btn == nil {
		t.Fatalf(`after resize the Save As button is not at {X:12 W:12} — relayout did not re-apply W:12`)
	}
}

// TestIssue548CaptionFullOnGrownDialog opens the editor directly on a large terminal
// (the dialog width is pinned at 83, but its height grows) and asserts the caption is
// still full and the button still W:12 — the fix holds on larger dialogs, not just
// the floor.
func TestIssue548CaptionFullOnGrownDialog(t *testing.T) {
	w := openEditor548(t, 200, 50, savableHandlers548())
	grid := editorGrid(w)
	if _, _, ok := findRunes(grid, "[ Save As… ]"); !ok {
		t.Errorf(`on a large terminal the full "[ Save As…" ]" caption is NOT on screen`)
	}
	if findFooterButtonByRect(w, 12, footerButtonRowY(w), 12) == nil {
		t.Errorf(`on a large terminal the Save As button is not at {X:12 W:12}`)
	}
}

// TestIssue548FocusedCaptionAlsoFull is the focus-state edge case. turbotui swaps the
// "[ … ]" brackets for single-cell "►…◄" chevrons when a button is focused (avail =
// W-2, more forgiving than the unfocused W-4), so the caption fits at W:12 either
// way — but a regression to W:11 plus any future chrome change should still leave the
// focused caption legible, so pin it.
func TestIssue548FocusedCaptionAlsoFull(t *testing.T) {
	w := openEditor548(t, 100, 30, savableHandlers548())
	btn := findFooterButtonByRect(w, 12, footerButtonRowY(w), 12)
	if btn == nil {
		t.Fatalf("Save As button not found at {X:12 W:12}")
	}
	w.desktop.SetFocus(btn) // *VisualComponent satisfies Widget via Root()
	w.desktop.Redraw()

	grid := editorGrid(w)
	if _, _, ok := findRunes(grid, "Save As…"); !ok {
		t.Errorf(`the focused Save As caption is not on screen`)
	}
	if _, _, ok := findRunes(grid, "Save A…"); ok {
		t.Errorf(`the focused Save As caption is truncated to "Save A…"`)
	}
}

// TestIssue548OtherFooterButtonsNotTruncated is the collateral check (criterion 3):
// widening Save As must not truncate any other footer button. Each button is resolved
// by its known foot rect and its rendered caption read directly, so the "Save" ⊂
// "Save As…" caption ambiguity cannot mask a real truncation.
func TestIssue548OtherFooterButtonsNotTruncated(t *testing.T) {
	w := openEditor548(t, 100, 30, savableHandlers548())
	dialogW := dialogBounds(w).W
	row := footerButtonRowY(w)
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
		btn := findFooterButtonByRect(w, c.x, row, c.wd)
		if btn == nil {
			t.Errorf("footer button %q not found at {X:%d W:%d}", c.name, c.x, c.wd)
			continue
		}
		caption := buttonCaption(t, w, btn)
		if !strings.Contains(caption, c.name) {
			t.Errorf("footer button %q caption = %q — does not contain its full label (truncated by the width change?)", c.name, caption)
		}
	}
}

// ----------------------------------------------------------------------------
// Usability / function — the legible button drives the #462 copy-theme workflow.
// ----------------------------------------------------------------------------

// TestIssue548SaveAsWorkflowReachableThroughLegibleButton proves the discoverability
// fix end-to-end: the now-legible "Save As…" button is clickable by its rendered
// caption, opens the name-input dialog, and a fresh name creates a new ★ saved theme
// copied from the current selection (the #462 non-destructive path). This is the
// workflow the truncation was hiding.
func TestIssue548SaveAsWorkflowReachableThroughLegibleButton(t *testing.T) {
	st := &theme462Store{active: config.ThemeConfig{Name: "default"}}
	w := openEditor548(t, 100, 30, st.handlers())

	// Click the Save As button by its on-screen caption (what a user sees).
	btn := findFooterButtonByCaption(t, w, "Save As…")
	if btn == nil {
		t.Fatalf(`could not target the Save As button by its rendered "Save As…" caption — the button is not legible/clickable`)
	}
	clickButton548(btn)

	// The name-input dialog must open on top (discoverability).
	box := inputDialogBox(t, w)
	for _, r := range "My Copy" {
		typeDlgRune(box, r)
	}
	submitDlg(box)
	w.desktop.Redraw()

	// A fresh name creates exactly one new saved theme.
	if len(st.setSaved) != 1 {
		t.Fatalf("expected one SetSavedThemes call creating the copy, got %d: %+v", len(st.setSaved), st.setSaved)
	}
	flushed := st.setSaved[0]
	if len(flushed) != 1 || flushed[0].Name != "My Copy" {
		t.Errorf("Save As did not create a single named copy: %+v", flushed)
	}
	// The copy becomes the live active theme via the SavedName back-link.
	if len(st.setTheme) != 1 || st.setTheme[0].SavedName != "My Copy" {
		t.Errorf("Save As did not apply the copy live with the SavedName back-link: %+v", st.setTheme)
	}
}

// ----------------------------------------------------------------------------
// No regressions — graceful degradation without saved-themes handlers.
// ----------------------------------------------------------------------------

// TestIssue548SaveAsHiddenWithoutSavedHandlers guards the savable gate: without
// GetSavedThemes/SetSavedThemes the editor is the built-ins-only editor — no Save As
// button (and so no caption, full or truncated) — while the core buttons remain.
func TestIssue548SaveAsHiddenWithoutSavedHandlers(t *testing.T) {
	w := openEditor548(t, 100, 30, Handlers{
		GetTheme: func() config.ThemeConfig { return config.ThemeConfig{Name: "default"} },
		SetTheme: func(config.ThemeConfig) {},
		// GetSavedThemes / SetSavedThemes intentionally nil -> savable == false.
	})
	row := footerButtonRowY(w)

	if findFooterButtonByRect(w, 12, row, 12) != nil {
		t.Error("a Save As button is present without GetSavedThemes/SetSavedThemes — it should be hidden")
	}
	grid := editorGrid(w)
	if _, _, ok := findRunes(grid, "Save As…"); ok {
		t.Error(`the full "Save As…" caption is rendered without saved-themes handlers — the button should be absent`)
	}
	if _, _, ok := findRunes(grid, "Save A…"); ok {
		t.Error(`the truncated "Save A…" caption is rendered without saved-themes handlers — the button should be absent`)
	}

	// Sanity: the core footer buttons are still laid out.
	dialogW := dialogBounds(w).W
	for _, c := range []struct{ x, wd int }{{2, 9}, {dialogW - 24, 9}, {dialogW - 13, 10}} {
		if findFooterButtonByRect(w, c.x, row, c.wd) == nil {
			t.Errorf("core footer button at {X:%d W:%d} is missing", c.x, c.wd)
		}
	}
}

// ----------------------------------------------------------------------------
// The defect guard — the footer-button helper must track the real laid-out button.
// ----------------------------------------------------------------------------

// TestIssue548FooterHelperMatchesLaidOutButton pins that themeEditorFooterButtonRect
// returns the SAME rect the editor actually lays out for the Save As button.
// clickThemeButton and the #462 Save As tests resolve the button through that helper,
// so a drift between the helper and reality breaks them. This test FAILS until the
// driver updates themeEditorFooterButtonRect (theme_issue462_test.go:130) from W:11
// to W:12 to match the widened button — the omission that currently breaks
// TestIssue462SaveAsCreatesNamedCopy and TestIssue462SaveAsDuplicateConfirmsBeforeOverwrite.
func TestIssue548FooterHelperMatchesLaidOutButton(t *testing.T) {
	w := openEditor548(t, 100, 30, savableHandlers548())
	btn := findFooterButtonByRect(w, 12, footerButtonRowY(w), 12)
	if btn == nil {
		t.Fatal("Save As button not found at {X:12 W:12} — the fix did not widen it to W:12")
	}
	helperRect := themeEditorFooterButtonRect(w, "Save As…")
	if helperRect != btn.Bounds {
		t.Errorf("themeEditorFooterButtonRect(\"Save As…\") = %+v but the actual laid-out button = %+v — "+
			"the helper is stale (still W:11). Update ui/tui/theme_issue462_test.go:130 to W:12; "+
			"otherwise clickThemeButton(\"Save As…\") and the #462 Save As tests cannot resolve the button.",
			helperRect, btn.Bounds)
	}
}
