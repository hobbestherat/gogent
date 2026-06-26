package ui

import (
	"testing"

	tv "github.com/hobbestherat/turbotui/turbotv"
	"gogent/internal/config"
)

// Tests for issue #477: the theme-editor dialog widens the inter-column gutter from 1
// to 4 columns, raises the inter-section spacing to (a baseline of) 2 blank rows, and
// lifts the dialog width floor 80 → 83 to absorb the extra columns. These tests pin the
// four acceptance criteria — inter-column gap == 4, ≥2 blank rows between sections with
// the two second sections still aligned, themeEditorDialogW == 83, contentRows == 22 /
// floor maxScroll == 7 — plus the cross-file spec invariant, the no-collision/reveal
// guards, and the grown-editor "fits all content" contract. turbotui is untouched;
// everything is derived from gogent's own resolver/consts.

// interColumnGap is the number of blank columns between the left column's swatch and the
// right column's label — the quantity #477 widens 1 → 4. It is rightX minus everything the
// left column occupies in a row (origin + label cell + the two single-space intra-row
// gaps that flank the field/swatch). Crucially the gutter is a CONSTANT: on a wider
// terminal the surplus width flows into the two label cells (resolveThemeEditorLayout's
// extra/2), it does not widen the gutter.
func interColumnGap(l themeEditorLayout) int {
	left, right := l.columns[0], l.columns[1]
	return right.x - (left.x + left.labelW + themeEditorFieldW + themeEditorSwatchW + 2)
}

// TestIssue477_InterColumnGapIsFourAtFloor pins the headline horizontal change: at the
// 83-wide floor the two columns are separated by exactly 4 blank columns (3 more than the
// old 1), which places the right column's origin at x = 42.
func TestIssue477_InterColumnGapIsFourAtFloor(t *testing.T) {
	l := resolveThemeEditorLayout(themeEditorDialogW, themeEditorDialogH)
	if got := interColumnGap(l); got != 4 {
		t.Errorf("inter-column gap at floor = %d, want 4 (issue #477 widened the gutter 1→4)", got)
	}
	if l.columns[1].x != 42 {
		t.Errorf("right column floor x = %d, want 42 (leftX 2 + leftLabelW 20 + fieldW 7 + swatchW 7 + 2 gaps + 4 gutter)", l.columns[1].x)
	}
	if got := interColumnGap(l); got <= 1 {
		t.Errorf("inter-column gap = %d — the #477 gutter widening did not take effect", got)
	}
}

// TestIssue477_InterColumnGapStaysFourOnWiderTerminals is the "grows correctly on wider
// terminals" half of criterion 1: the gutter is a constant 4 at every width ≥ floor. The
// extra width beyond 83 is shared between the two label cells, not the gutter.
func TestIssue477_InterColumnGapStaysFourOnWiderTerminals(t *testing.T) {
	for _, width := range []int{themeEditorDialogW, 90, 100, 120, 160, 200, 240} {
		l := resolveThemeEditorLayout(width, themeEditorDialogH)
		if got := interColumnGap(l); got != 4 {
			t.Errorf("at width %d inter-column gap = %d, want 4 (the gutter is constant; surplus width goes to the label cells)", width, got)
		}
	}
	// Surplus width really does land in the label cells: at 200 wide each label cell has
	// grown past its 20/22 floor by half the surplus, while the gutter is unchanged.
	l := resolveThemeEditorLayout(200, themeEditorDialogH)
	if l.columns[0].labelW <= themeEditorLeftLabelW || l.columns[1].labelW <= themeEditorLabelW {
		t.Errorf("at width 200 label cells = %d/%d, both should exceed the %d/%d floor (surplus went somewhere else)",
			l.columns[0].labelW, l.columns[1].labelW, themeEditorLeftLabelW, themeEditorLabelW)
	}
}

// TestIssue477_SectionPadValues pins the sectionPad regime #477 installed: left column 2,
// right column 1. (The left carries one more than the right so its shorter first section
// still aligns its second section with the right's — see TestIssue477_SecondSectionsAlign.)
func TestIssue477_SectionPadValues(t *testing.T) {
	cols := themeEditorColumns()
	if cols[0].sectionPad != 2 {
		t.Errorf("left column sectionPad = %d, want 2", cols[0].sectionPad)
	}
	if cols[1].sectionPad != 1 {
		t.Errorf("right column sectionPad = %d, want 1", cols[1].sectionPad)
	}
}

// TestIssue477_TwoBlankRowsBetweenSections pins criterion 1's vertical change: every
// inter-section gap shows at least 2 blank rows. The gap is the single separator row plus
// sectionPad extra rows, so right (sectionPad 1) gets exactly 2 — the issue's "2 blank
// rows" baseline — and left (sectionPad 2) gets 3 (its third row is the cross-column
// alignment lever, not extra breathing room).
func TestIssue477_TwoBlankRowsBetweenSections(t *testing.T) {
	for i, col := range themeEditorColumns() {
		if blankRows := 1 + col.sectionPad; blankRows < 2 {
			t.Errorf("column %d inserts %d blank rows per inter-section gap, want ≥ 2", i, blankRows)
		}
	}
	if got := 1 + themeEditorColumns()[1].sectionPad; got != 2 {
		t.Errorf("right column blank rows per gap = %d, want exactly 2 (the #477 baseline)", got)
	}
}

// TestIssue477_SecondSectionsAligned pins criterion 1's alignment requirement: despite the
// unequal sectionPad, the two columns' second sections (UI chrome / Buttons and inputs)
// still start on the same logical row, and both first sections still start at row 0.
func TestIssue477_SecondSectionsAligned(t *testing.T) {
	left, right := themeEditorColumns()[0], themeEditorColumns()[1]
	leftSecondStart := 1 + len(left.groups[0].roles) + 1 + left.sectionPad // header + roles + separator + pad
	rightSecondStart := 1 + len(right.groups[0].roles) + 1 + right.sectionPad
	if leftSecondStart != rightSecondStart {
		t.Errorf("second sections misaligned: left UI chrome at logical row %d, right Buttons and inputs at %d",
			leftSecondStart, rightSecondStart)
	}
	if leftSecondStart != 11 {
		t.Errorf("second sections start at logical row %d, want 11 (8 session rows + 1 sep + 2 pad)", leftSecondStart)
	}
}

// TestIssue477_DialogWidthFloorIs83 pins criterion 3's width floor and the two consts that
// derive from it: scrollbarX = floor-3 = 80, and the height-driven visibleRows = 15.
func TestIssue477_DialogWidthFloorIs83(t *testing.T) {
	if themeEditorDialogW != 83 {
		t.Errorf("themeEditorDialogW = %d, want 83 (issue #477 raised the floor 80→83)", themeEditorDialogW)
	}
	if themeEditorScrollbarX != 80 {
		t.Errorf("themeEditorScrollbarX = %d, want 80 (themeEditorDialogW-3)", themeEditorScrollbarX)
	}
	if themeEditorVisibleRows != 15 {
		t.Errorf("themeEditorVisibleRows = %d, want 15 (height-driven, unchanged by the width floor)", themeEditorVisibleRows)
	}
}

// TestIssue477_ContentRowsAndScrollMath pins criterion 3's scroll math: the tallest column
// is now 22 rows (was 21), so the floor maxScroll is 7 (was 6) and the reveal invariant
// (contentRows - maxScroll ≤ visibleRows) still holds at 15 ≤ 15.
func TestIssue477_ContentRowsAndScrollMath(t *testing.T) {
	if got := themeEditorContentRows(); got != 22 {
		t.Errorf("themeEditorContentRows() = %d, want 22 (both columns grew to 22 with the #477 section spacing)", got)
	}
	if got := themeEditorMaxScroll(); got != 7 {
		t.Errorf("themeEditorMaxScroll() = %d, want 7 (contentRows 22 − visibleRows 15)", got)
	}
	if reveal := themeEditorContentRows() - themeEditorMaxScroll(); reveal > themeEditorVisibleRows {
		t.Errorf("reveal invariant broken: contentRows %d − maxScroll %d = %d > visibleRows %d — the last roles could never be scrolled into view",
			themeEditorContentRows(), themeEditorMaxScroll(), reveal, themeEditorVisibleRows)
	}
}

// TestIssue477_DialogSpecPinnedToFloorWidth is the cross-file invariant the design asked
// for: themeEditorDialogSpec pins the width to the content footprint, so MinW == MaxW ==
// PreferredW == themeEditorDialogW. This is the only compile/test-time link between the
// bare themeEditorDialogW const in theme_editor.go and the independently-summed contentW
// in dialog_sizing.go — a drift (one moved without the other) would make MinW > MaxW and
// is caught here.
func TestIssue477_DialogSpecPinnedToFloorWidth(t *testing.T) {
	spec := newTestWorkbench(t).themeEditorDialogSpec()
	if spec.MinW != themeEditorDialogW || spec.MaxW != themeEditorDialogW || spec.PreferredW != themeEditorDialogW {
		t.Errorf("themeEditorDialogSpec width = MinW %d / MaxW %d / PreferredW %d, all want %d — "+
			"a drift here desynchronises the const and the contentW sum (MinW>MaxW)",
			spec.MinW, spec.MaxW, spec.PreferredW, themeEditorDialogW)
	}
}

// TestIssue477_NoColumnCollisionAtFloor pins the collision invariants checkThemeEditorLayout
// guards, at the new 83-wide floor: the right column's swatch ends exactly one column before
// the scrollbar (the grown dialog uses the full width), and the left column clears the right
// by the new 4-column gutter.
func TestIssue477_NoColumnCollisionAtFloor(t *testing.T) {
	l := resolveThemeEditorLayout(themeEditorDialogW, themeEditorDialogH)
	left, right := l.columns[0], l.columns[1]
	rightSwatchEnd := right.x + right.labelW + themeEditorFieldW + themeEditorSwatchW + 2 - 1
	if rightSwatchEnd != l.scrollbarX-1 {
		t.Errorf("right column swatch ends at %d, want scrollbarX-1 = %d (flush before the scrollbar)", rightSwatchEnd, l.scrollbarX-1)
	}
	leftSwatchEnd := left.x + left.labelW + themeEditorFieldW + themeEditorSwatchW + 2 - 1
	if clearedBy := right.x - leftSwatchEnd - 1; clearedBy != 4 {
		t.Errorf("left column clears the right by %d gap columns, want 4", clearedBy)
	}
}

// TestIssue477_CheckLayoutInvariantsHold confirms the init-time guard does not panic at the
// new floor/gap/sectionPad — criterion 3's "checkThemeEditorLayout does not panic at init".
func TestIssue477_CheckLayoutInvariantsHold(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("checkThemeEditorLayout panicked: %v", r)
		}
	}()
	checkThemeEditorLayout()
}

// TestIssue477_BelowFloorResolvesToFloorWidth pins the below-floor edge case the floor raise
// turns into the standard-80-column behaviour: turbotui honours MinW last, so even on a
// 70-wide terminal the dialog resolves to its 83-wide floor (and clips its rightmost
// columns off-screen). This is the dialog's long-standing below-floor policy — #477 only
// moved the threshold from "<80" to "<83", which is why the classic 80-column terminal now
// crosses it.
func TestIssue477_BelowFloorResolvesToFloorWidth(t *testing.T) {
	spec := newTestWorkbench(t).themeEditorDialogSpec()
	_, _, w, h := tv.ResolveDialogRect(spec, 70, 24)
	if w != themeEditorDialogW {
		t.Errorf("on a 70-wide terminal dialog width = %d, want the %d floor (MinW honoured last)", w, themeEditorDialogW)
	}
	if h != themeEditorDialogH {
		t.Errorf("on a 24-row terminal dialog height = %d, want the %d floor", h, themeEditorDialogH)
	}
}

// TestIssue477_RightColumnFieldFullyVisibleOnSupportedTerminal is the positive counterpart
// to the below-floor clipping: on a supported (≥83) terminal the rightmost role's field
// renders its full 7-char hex value — it is only the below-83 screen that truncates it.
func TestIssue477_RightColumnFieldFullyVisibleOnSupportedTerminal(t *testing.T) {
	issue204RestoreTheme(t)
	w := newTestWorkbench(t)
	w.SetHandlers(Handlers{
		GetTheme: func() config.ThemeConfig { return config.ThemeConfig{} },
		SetTheme: func(config.ThemeConfig) {},
	})
	w.app.Resize(100, 30)
	w.showThemeEditor()
	w.desktop.Redraw()
	if !scrollEditorToReveal(w, "Code block background:") {
		t.Fatal("code_bg label not revealable by scrolling")
	}
	rows := editorGrid(w)
	r, c, ok := findRunes(rows, "Code block background:")
	if !ok {
		t.Fatal("code_bg label not on screen after scrolling")
	}
	// Read the field token: skip the single space after the label, then take up to fieldW
	// non-space chars.
	row := rows[r]
	p := c + len([]rune("Code block background:"))
	for p < len(row) && row[p] == ' ' {
		p++
	}
	start := p
	for p < len(row) && row[p] != ' ' && p-start < themeEditorFieldW {
		p++
	}
	token := string(row[start:p])
	if len(token) != themeEditorFieldW {
		t.Errorf("code_bg field = %q (%d chars) on a 100-wide terminal, want a full %d-char hex (only below-83 screens clip it)",
			token, len(token), themeEditorFieldW)
	}
}

// TestIssue477_GrownPrefHFitsAllContent guards the grown-editor "fits all content without
// scrolling" contract: dialog_sizing.go's themeEditorDialogSpec sets PrefH so every
// themeEditorContentRows() role is visible with no scrolling. PrefH must therefore reserve
// EVERY non-viewport row — contentTop above the viewport and (button row + bottom border +
// buttonGap) below — i.e. chromeH = 3 + themeEditorButtonGap + themeEditorContentTop (= 7),
// giving PrefH = contentRows + 7 = 29.
//
// History: chromeH originally omitted themeEditorButtonGap (= 6 → PrefH 28), leaving the
// grown viewport one row short of contentRows so the last role (code_bg) needed scrolling.
// Pre-#477 this was masked (the right column was 20 rows, ≤ the 20-row PrefH-27 viewport);
// #477's sectionPad raise grew the right column to 22 and exposed it. The fix — chromeH
// includes themeEditorButtonGap — is now in dialog_sizing.go; this test pins it so a future
// revert (dropping buttonGap from chromeH) fails immediately here, in the pre-existing
// TestThemeEditorNoScrollWhenGrown, and in TestThemeEditorResizeReflowsColumnsAndScroll.
func TestIssue477_GrownPrefHFitsAllContent(t *testing.T) {
	correctChromeH := 3 + themeEditorButtonGap + themeEditorContentTop // = 7
	wantPrefH := themeEditorContentRows() + correctChromeH             // = 29
	spec := newTestWorkbench(t).themeEditorDialogSpec()
	if spec.PrefH != wantPrefH {
		t.Errorf("PrefH = %d, want %d (contentRows %d + chrome %d INCLUDING themeEditorButtonGap) — "+
			"chromeH must include themeEditorButtonGap or the grown editor is one row short of fitting all content",
			spec.PrefH, wantPrefH, themeEditorContentRows(), correctChromeH)
	}

	// The rendered consequence: on a tall terminal the last role must be visible at scrollY=0.
	issue204RestoreTheme(t)
	w := newTestWorkbench(t)
	w.SetHandlers(Handlers{
		GetTheme: func() config.ThemeConfig { return config.ThemeConfig{} },
		SetTheme: func(config.ThemeConfig) {},
	})
	w.app.Resize(200, 50)
	w.showThemeEditor()
	w.desktop.Redraw()
	if !containsOnScreen(screenText(w), "Code block background:") {
		t.Error("on a 200×50 terminal code_bg is not visible at scrollY=0 — the grown editor should fit all roles without scrolling")
	}
}
