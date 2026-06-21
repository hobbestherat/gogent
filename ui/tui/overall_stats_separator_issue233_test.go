package ui

import (
	"strings"
	"testing"

	tui "github.com/hobbestherat/turbotui"
	tv "github.com/hobbestherat/turbotui/turbotv"
	"gogent/internal/config"
)

// This file tests issue #233: a thin, theme-aware horizontal separator drawn
// ABOVE the Overall stats band (bottom of the right-hand sidebar) so the stats
// region reads as distinct from the content above it.
//
// The fix relocated the band's existing '─' rule from *below* the model selector
// to the band's TOP row (above the selector), and re-expressed overallBandHeight
// so the separator is now a named, reserved row while the numeric band height
// stays 12. These tests pin all four acceptance angles:
//
//  1. the constant decomposition (separator is its own row; band height unchanged);
//  2. the LayoutFn wiring (the selector now sits one row below the separator);
//  3. the rendered rule (a full-width '─' run on the band's top row, in the theme
//     divider colour, above the title and metrics, single-line, not overlapping
//     the stats text, tracking resizes);
//  4. the edge cases (short sidebar drops the band → no separator, no panic).
//
// The rendered tests drive the real workbench headlessly (desktop.Redraw) and scan
// the app's cell buffer via w.app.ReadCell — the same approach the issue #227
// final-render tests use. They never construct a tv.Surface directly (its fields are
// unexported), so the band is exercised through the same Draw path a real frame uses.

// ----------------------------------------------------------------------------
// Cell-scan helpers.
// ----------------------------------------------------------------------------

// issue233PanelRect returns the sidebar panel's absolute screen rect. The panel is
// a layer root (Parent == nil), so AbsoluteBounds == Bounds.
func issue233PanelRect(w *Workbench) tv.Rect { return w.sidebar.panel.Bounds }

// issue233BandTopY returns the absolute screen Y of the Overall band's top row —
// the separator row issue #233 added — derived from the sidebar's live panel rect
// and the band height LayoutFn resolved for the current size.
func issue233BandTopY(w *Workbench) int {
	p := issue233PanelRect(w)
	return p.Y + p.H - w.sidebar.overallBandH
}

// issue233Segment reads n cells starting at (x0, y) and returns them as a string,
// rendering NUL/0 cells as spaces. It is the headless cell-scan primitive for the
// rendered-separator assertions.
func issue233Segment(w *Workbench, y, x0, n int) string {
	var b strings.Builder
	for dx := 0; dx < n; dx++ {
		ch := w.app.ReadCell(x0+dx, y).Ch
		if ch == 0 {
			ch = ' '
		}
		b.WriteRune(ch)
	}
	return b.String()
}

// issue233IsRuleRow reports whether row y holds a full-width run of the horizontal
// rule glyph across the panel's inner width (the shape both the Overall and the
// TODOs separators produce). It reads exactly the columns drawOverall's rule loop
// writes (cols X+1 .. X+W-2).
func issue233IsRuleRow(w *Workbench, y int) bool {
	p := issue233PanelRect(w)
	if p.W < 3 {
		return false
	}
	return issue233Segment(w, y, p.X+1, p.W-2) == strings.Repeat("─", p.W-2)
}

// issue233RuleRows returns the absolute Y of every full-width rule row inside the
// sidebar panel, top to bottom.
func issue233RuleRows(w *Workbench) []int {
	p := issue233PanelRect(w)
	var rows []int
	for y := p.Y; y < p.Y+p.H; y++ {
		if issue233IsRuleRow(w, y) {
			rows = append(rows, y)
		}
	}
	return rows
}

// issue233RedrawFail renders the workbench and fails the test if it panics, so an
// edge-case size that breaks the render path fails loudly instead of crashing the
// whole test binary.
func issue233RedrawFail(t *testing.T, w *Workbench) {
	t.Helper()
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("desktop.Redraw panicked: %v", r)
		}
	}()
	w.desktop.Redraw()
}

// ----------------------------------------------------------------------------
// Group 1: constant / decomposition invariants (pure, no rendering).
// ----------------------------------------------------------------------------

// TestIssue233_SeparatorConstants pins the data layout the renderer and LayoutFn
// depend on: the separator is exactly one row, the band height is still 12 (so the
// reserved space and the existing height-formula tests are unchanged), and the
// height formula now names the separator as a distinct top row. If a future change
// folds the separator into another row or lets the band grow, the rendered layout
// (title at bandTop+2, 9 metrics filling the rest) would silently desync.
func TestIssue233_SeparatorConstants(t *testing.T) {
	if overallSeparatorLines != 1 {
		t.Errorf("overallSeparatorLines = %d, want 1 (issue #233 is a single thin line)",
			overallSeparatorLines)
	}
	// The reserved band height must not change — only the separator's row relocated
	// from below the selector to the top. A value change would shift every height
	// boundary the sidebar's drop logic depends on.
	if overallBandHeight != 12 {
		t.Errorf("overallBandHeight = %d, want 12 (unchanged by the separator move)",
			overallBandHeight)
	}
	// The formula now accounts for the separator as its own top row.
	if got := overallSeparatorLines + overallSelectorLines + overallMetricLines + 1; got != overallBandHeight {
		t.Errorf("separator(%d)+selector(%d)+metrics(%d)+1 = %d, want overallBandHeight %d",
			overallSeparatorLines, overallSelectorLines, overallMetricLines, got, overallBandHeight)
	}
	// Removing the separator must leave exactly selector + title + metrics (1+1+9=11),
	// i.e. the separator is a genuinely reserved row, not folded into a neighbour.
	if overallBandHeight-overallSeparatorLines != overallSelectorLines+overallMetricLines+1 {
		t.Errorf("band minus separator = %d, want selector+metrics+1 = %d (separator must be its own row)",
			overallBandHeight-overallSeparatorLines, overallSelectorLines+overallMetricLines+1)
	}
}

// ----------------------------------------------------------------------------
// Group 2: LayoutFn wiring (selector sits below the band-top separator).
// ----------------------------------------------------------------------------

// TestIssue233_SelectorSitsBelowBandTopSeparator is the core layout assertion: with
// the band shown, the model selector is positioned one row below the band's top row
// (where the separator now lives), never on it. Before #233 the selector occupied
// the band top and the rule sat below it; this pins the swap so the two never
// collide and the separator is always the topmost thing in the band.
func TestIssue233_SelectorSitsBelowBandTopSeparator(t *testing.T) {
	w := newTestWorkbench(t)
	if w.sidebar.overallSelect == nil {
		t.Fatal("precondition: test workbench has no model selector")
	}
	for _, h := range []int{overallBandHeight + 20, overallBandHeight + 5, 30, 24, 50} {
		w.sidebar.panel.SetBounds(tv.Rect{X: 48, Y: 1, W: defaultSidebarWidth, H: h})
		if w.sidebar.overallBandH == 0 {
			continue // band dropped at this height: selector hidden, nothing to check
		}
		root := w.sidebar.overallSelect.Root()
		sel := root.Bounds
		bandTopRel := h - w.sidebar.overallBandH
		if !root.Visible {
			t.Errorf("h=%d: selector hidden while band is shown", h)
		}
		if sel.Y != bandTopRel+overallSeparatorLines {
			t.Errorf("h=%d: selector panel-relative Y=%d, want band top+separator=%d",
				h, sel.Y, bandTopRel+overallSeparatorLines)
		}
		if sel.Y <= bandTopRel {
			t.Errorf("h=%d: selector Y=%d must be strictly below the separator row %d (no overlap)",
				h, sel.Y, bandTopRel)
		}
	}
}

// TestIssue233_SelectorHiddenWhenBandDropped confirms the selector (and thus the
// separator region) disappears on a short sidebar: LayoutFn hides the selector and
// zeroes the band height together, so a too-short terminal never shows a dangling
// separator with no stats beneath it.
func TestIssue233_SelectorHiddenWhenBandDropped(t *testing.T) {
	w := newTestWorkbench(t)
	if w.sidebar.overallSelect == nil {
		t.Fatal("precondition: test workbench has no model selector")
	}
	// Just too short to keep the band (tree would fall below its minimum).
	w.sidebar.panel.SetBounds(tv.Rect{X: 48, Y: 1, W: defaultSidebarWidth, H: overallBandHeight + 4})
	if w.sidebar.overallBandH != 0 {
		t.Fatalf("overallBandH = %d, want 0 (band should drop at H=%d)",
			w.sidebar.overallBandH, overallBandHeight+4)
	}
	if w.sidebar.overallSelect.Root().Visible {
		t.Error("selector should be hidden when the band is dropped")
	}
}

// ----------------------------------------------------------------------------
// Group 3: rendered separator (real Draw path via desktop.Redraw).
// ----------------------------------------------------------------------------

// TestIssue233_SeparatorRenderedAtBandTop is the headline render assertion: the
// band's top row is a full-width run of the horizontal-rule glyph, sitting exactly
// where LayoutFn reserves it (panel-relative h-bandH). It must fail without the #233
// fix, which drew the rule below the selector instead of at the band top.
func TestIssue233_SeparatorRenderedAtBandTop(t *testing.T) {
	w := newTestWorkbench(t)
	issue233RedrawFail(t, w)
	if w.sidebar.overallBandH == 0 {
		t.Fatal("precondition: Overall band not shown at default size (overallBandH=0)")
	}
	p := issue233PanelRect(w)
	bandTop := issue233BandTopY(w)

	seg := issue233Segment(w, bandTop, p.X+1, p.W-2)
	want := strings.Repeat("─", p.W-2)
	if seg != want {
		t.Errorf("band top row (y=%d) = %q, want a full-width rule %q", bandTop, seg, want)
	}
	// And it is the only full-width rule row when no TODO region is shown.
	if rows := issue233RuleRows(w); len(rows) != 1 || rows[0] != bandTop {
		t.Errorf("rule rows = %v, want exactly [bandTop=%d] (the Overall separator)", rows, bandTop)
	}
}

// TestIssue233_SeparatorAboveTitleAndMetrics pins the vertical order the band
// contract requires: separator → selector row → "Overall" title → 9 metric rows.
// The separator must be strictly above the title and the first metric, with the
// selector row between them — never under or over the stats text.
func TestIssue233_SeparatorAboveTitleAndMetrics(t *testing.T) {
	w := newTestWorkbench(t)
	issue233RedrawFail(t, w)
	p := issue233PanelRect(w)
	bandTop := issue233BandTopY(w)
	titleY := bandTop + overallSeparatorLines + overallSelectorLines

	if got := issue233Segment(w, titleY, p.X+2, 7); got != "Overall" {
		t.Errorf("title row y=%d = %q, want \"Overall\" (separator must sit above the title)", titleY, got)
	}
	// First metric row sits directly below the title.
	if got := issue233Segment(w, titleY+1, p.X+2, 8); !strings.HasPrefix(got, "sessions") {
		t.Errorf("first metric row y=%d = %q, want the \"sessions\" label", titleY+1, got)
	}
	if titleY <= bandTop {
		t.Errorf("title y=%d must be strictly below the separator row %d", titleY, bandTop)
	}
}

// TestIssue233_SeparatorUsesThemeDividerColour verifies the rule is theme-aware:
// its colour tracks the theme's Divider role at draw time, across EVERY ApplyTheme
// branch (default, dark, high-contrast and NO_COLOR), not just the stock palette.
// ApplyTheme reseeds the package-global chromeDivider from t.Divider, and drawOverall
// reads it at draw time — so the rendered cell must equal both the live chromeDivider
// and the theme's Divider for each preset. The closing loop then drives two distinct
// custom dividers to prove the colour is read live rather than baked in.
func TestIssue233_SeparatorUsesThemeDividerColour(t *testing.T) {
	withThemeRestore(t)
	w := newTestWorkbench(t)
	p := issue233PanelRect(w)
	bandTop := issue233BandTopY(w)

	// Every real ApplyTheme branch must leave the separator on the theme's Divider.
	// (A slice, not a map, so failures are reported in a fixed order.)
	presets := []struct {
		name string
		th   Theme
	}{
		{"default", issue204Default()},
		{"dark", issue204Dark()},
		{"high-contrast", issue204HighContrast()},
		{"no-color", ResolveTheme(config.ThemeConfig{}, noColorEnv, false)},
	}
	for _, tc := range presets {
		if tc.th.Level == ColorNone && tc.th.Divider != tui.DefaultColor() {
			t.Fatalf("%s: precondition — Divider should degrade to DefaultColor under NO_COLOR, got %v", tc.name, tc.th.Divider)
		}
		ApplyTheme(tc.th)
		issue233RedrawFail(t, w)
		got := w.app.ReadCell(p.X+2, bandTop).FG
		if got != chromeDivider {
			t.Errorf("%s theme: separator FG=%v, want live chromeDivider=%v (must read the theme at draw time)",
				tc.name, got, chromeDivider)
		}
		if got != tc.th.Divider {
			t.Errorf("%s theme: separator FG=%v, want theme Divider=%v", tc.name, got, tc.th.Divider)
		}
	}

	// Dynamic proof the colour is not hardcoded: two distinct dividers on the same
	// theme both take effect on a re-render.
	base := issue204Default()
	for _, div := range []tui.Color{tui.ANSIColor(11), tui.ANSIColor(9)} {
		base.Divider = div
		ApplyTheme(base)
		issue233RedrawFail(t, w)
		if got := w.app.ReadCell(p.X+2, bandTop).FG; got != div {
			t.Errorf("custom divider %v: separator FG=%v, want %v (must track Divider, not a constant)", div, got, div)
		}
	}
}

// TestIssue233_SeparatorSpanRespectsPanelEdges guards the rule's horizontal extent:
// it starts one column in (leaving the panel's left '│' divider intact), spans to
// one column short of the right edge, and never overwrites the divider or bleeds
// past the panel. A regression to x:=0 or x<abs.W would clobber the divider.
func TestIssue233_SeparatorSpanRespectsPanelEdges(t *testing.T) {
	w := newTestWorkbench(t)
	issue233RedrawFail(t, w)
	p := issue233PanelRect(w)
	bandTop := issue233BandTopY(w)

	if got := w.app.ReadCell(p.X, bandTop).Ch; got != '│' {
		t.Errorf("panel left divider at band top = %q, want '│' (rule must not overwrite it)", got)
	}
	if got := w.app.ReadCell(p.X+1, bandTop).Ch; got != '─' {
		t.Errorf("first rule cell (X+1) = %q, want '─'", got)
	}
	if got := w.app.ReadCell(p.X+p.W-2, bandTop).Ch; got != '─' {
		t.Errorf("last rule cell (X+W-2) = %q, want '─'", got)
	}
	if got := w.app.ReadCell(p.X+p.W-1, bandTop).Ch; got == '─' {
		t.Errorf("right-margin cell (X+W-1) = '─'; the rule must not reach the panel's right edge")
	}
}

// TestIssue233_SeparatorIsSingleThinLine asserts exactly one rule row lives in the
// band (the top separator) and that the selector and title rows beneath it are not
// rules — the issue asks for "a thin separator line", not a boxed panel.
func TestIssue233_SeparatorIsSingleThinLine(t *testing.T) {
	w := newTestWorkbench(t)
	issue233RedrawFail(t, w)
	bandTop := issue233BandTopY(w)

	if !issue233IsRuleRow(w, bandTop) {
		t.Error("band top row is not a separator rule")
	}
	// The selector row and the title row below the separator are not rule rows.
	if issue233IsRuleRow(w, bandTop+overallSeparatorLines) {
		t.Error("the selector row is also a rule — separator must be a single line")
	}
	if issue233IsRuleRow(w, bandTop+overallSeparatorLines+overallSelectorLines) {
		t.Error("the title row is a rule — separator must be a single line")
	}
}

// TestIssue233_StatsContentNotClipped confirms the separator does not clip the
// stats content: the title and all 9 metric labels render in full, and the last
// metric lands exactly on the band's bottom row (no row is lost to the separator).
func TestIssue233_StatsContentNotClipped(t *testing.T) {
	w := newTestWorkbench(t)
	issue233RedrawFail(t, w)
	p := issue233PanelRect(w)
	bandTop := issue233BandTopY(w)
	titleY := bandTop + overallSeparatorLines + overallSelectorLines

	if got := issue233Segment(w, titleY, p.X+2, 7); got != "Overall" {
		t.Errorf("title = %q, want \"Overall\"", got)
	}
	wantLabels := []string{"sessions", "sub-agents", "tokens in", "tokens out",
		"requests", "errors", "cache hit", "model", "api"}
	for i, label := range wantLabels {
		seg := issue233Segment(w, titleY+1+i, p.X+2, len(label))
		if seg != label {
			t.Errorf("metric %d (%q) at row %d = %q, want the full label (not clipped)",
				i, label, titleY+1+i, seg)
		}
	}
	// The last metric sits on the band's bottom row: the separator took the top row
	// but did not eat into the metrics' space.
	lastMetricRow := titleY + overallMetricLines
	bandBottom := bandTop + w.sidebar.overallBandH - 1
	if lastMetricRow != bandBottom {
		t.Errorf("last metric row = %d, want band bottom %d (a metric row must not be clipped by the separator)",
			lastMetricRow, bandBottom)
	}
}

// TestIssue233_SeparatorDoesNotOverlapStatsContent is the converse of the clipping
// test: the separator row carries only the rule glyph (never stats text), and the
// title/metric rows carry no rule glyph — so the line divides the regions without
// colliding with either.
func TestIssue233_SeparatorDoesNotOverlapStatsContent(t *testing.T) {
	w := newTestWorkbench(t)
	issue233RedrawFail(t, w)
	p := issue233PanelRect(w)
	bandTop := issue233BandTopY(w)
	titleY := bandTop + overallSeparatorLines + overallSelectorLines

	sepRow := issue233Segment(w, bandTop, p.X, p.W)
	for _, frag := range []string{"Overall", "sessions", "tokens", "requests", "errors", "cache", "model"} {
		if strings.Contains(sepRow, frag) {
			t.Errorf("separator row leaks stats text %q: %q", frag, sepRow)
		}
	}
	// Title + metric rows (below the selector) carry no rule glyph.
	for y := titleY; y < bandTop+w.sidebar.overallBandH; y++ {
		if row := issue233Segment(w, y, p.X, p.W); strings.ContainsRune(row, '─') {
			t.Errorf("stats row y=%d contains a separator glyph: %q", y, row)
		}
	}
}

// TestIssue233_SeparatorTracksSidebarWidth verifies the rule follows the sidebar's
// live width (issue: "verify it looks correct across resizing"): after each resize
// the rule spans the new inner width and stays on the band's top row.
func TestIssue233_SeparatorTracksSidebarWidth(t *testing.T) {
	w := newTestWorkbench(t)
	for _, wd := range []int{20, 24, defaultSidebarWidth, 40} {
		w.setSidebarWidth(wd)
		issue233RedrawFail(t, w)
		if w.sidebar.overallBandH == 0 {
			t.Errorf("width=%d: band dropped unexpectedly", wd)
			continue
		}
		p := issue233PanelRect(w)
		bandTop := issue233BandTopY(w)
		seg := issue233Segment(w, bandTop, p.X+1, p.W-2)
		if want := strings.Repeat("─", p.W-2); seg != want {
			t.Errorf("width=%d: separator (panel W=%d) = %q, want %q", wd, p.W, seg, want)
		}
	}
}

// TestIssue233_OverallSeparatorDistinctFromTodosSeparator is the integration check
// that the Overall separator stays correct when the per-session TODO region is also
// shown: the Overall rule renders as a clean full-width line on the band top (the
// TODOs rule, by contrast, carries the "TODOs" title over it), the two sit on
// distinct rows, and the title/metrics below the Overall rule stay intact. Guards
// against the two rules colliding or the Overall rule being displaced when a middle
// region is inserted above it.
func TestIssue233_OverallSeparatorDistinctFromTodosSeparator(t *testing.T) {
	w := newTestWorkbench(t)
	// Set up the TODO region as pure sidebar state (a focused session with a
	// checklist). A real openWindow is deliberately avoided: at the default 80×25
	// app the area left of the sidebar is <50 cols, so openWindow falls back to a
	// near-full-width window that paints over the sidebar and would mask the rules.
	w.sidebar.addSession("s1", "Session 1", false)
	w.sidebar.applyTodo("s1", threeTodos())
	w.sidebar.focusSession("s1")
	issue233RedrawFail(t, w)

	if w.sidebar.todosBandH == 0 {
		t.Fatal("precondition: TODO region not shown, so this integration check is vacuous")
	}
	p := issue233PanelRect(w)
	bandTop := issue233BandTopY(w)

	// The Overall separator is a clean full-width rule on the band top.
	if !issue233IsRuleRow(w, bandTop) {
		t.Errorf("Overall separator at band top y=%d is not a clean full-width rule", bandTop)
	}
	// The TODOs separator sits on its own row above the Overall band and carries the
	// "TODOs" title over its rule (so it is NOT a clean run — the trait that tells
	// the two apart). It must be a distinct, higher row.
	todosSepY := bandTop - w.sidebar.todosBandH
	todosRow := issue233Segment(w, todosSepY, p.X, p.W)
	if !strings.Contains(todosRow, "TODOs") {
		t.Errorf("TODOs separator row y=%d = %q, want it to carry the \"TODOs\" title", todosSepY, todosRow)
	}
	if !strings.ContainsRune(todosRow, '─') {
		t.Errorf("TODOs separator row y=%d has no rule glyph: %q", todosSepY, todosRow)
	}
	if todosSepY >= bandTop {
		t.Errorf("TODOs separator y=%d must be above (not on/after) the Overall separator y=%d",
			todosSepY, bandTop)
	}
	// Title still renders directly below the Overall separator + selector row.
	titleY := bandTop + overallSeparatorLines + overallSelectorLines
	if got := issue233Segment(w, titleY, p.X+2, 7); got != "Overall" {
		t.Errorf("title = %q, want \"Overall\" with the TODO region present", got)
	}
}

// ----------------------------------------------------------------------------
// Group 4: edge cases / error handling.
// ----------------------------------------------------------------------------

// TestIssue233_NoSeparatorOrPanicWhenBandDropped covers the short-sidebar edge: when
// LayoutFn drops the band (sidebar too short), rendering must not panic and must not
// leave a separator rule with no stats beneath it.
func TestIssue233_NoSeparatorOrPanicWhenBandDropped(t *testing.T) {
	w := newTestWorkbench(t)
	// Height where the band drops so the tree keeps its minimum.
	w.sidebar.panel.SetBounds(tv.Rect{X: 48, Y: 1, W: defaultSidebarWidth, H: overallBandHeight + 4})
	if w.sidebar.overallBandH != 0 {
		t.Fatalf("overallBandH = %d, want 0 at H=%d", w.sidebar.overallBandH, overallBandHeight+4)
	}
	issue233RedrawFail(t, w) // must not panic
	if rows := issue233RuleRows(w); len(rows) != 0 {
		t.Errorf("found %d rule row(s) %v with the band dropped; the separator must be suppressed", len(rows), rows)
	}
}

// TestIssue233_DrawOverallNoPanicAcrossHeights sweeps a range of sidebar heights and
// asserts a redraw never panics and that the separator appears exactly when the band
// is reserved and is absent otherwise — a broad regression guard for the render path
// at degenerate sizes (issue: "verify it looks correct across resizing").
//
// Heights are capped to the headless app buffer height (the panel is pinned at Y=1,
// so its bottom row is h; the 25-row test buffer only holds rows 0..24). That still
// spans the band-dropped (h≤16) and band-shown (h≥17) regimes.
func TestIssue233_DrawOverallNoPanicAcrossHeights(t *testing.T) {
	w := newTestWorkbench(t)
	maxH := w.app.Height() - 1 // keep the panel inside the cell buffer
	for h := 2; h <= maxH; h++ {
		w.sidebar.panel.SetBounds(tv.Rect{X: 48, Y: 1, W: defaultSidebarWidth, H: h})
		issue233RedrawFail(t, w) // never panics at any height

		bandShown := w.sidebar.overallBandH > 0
		rules := issue233RuleRows(w)
		switch {
		case bandShown && len(rules) == 0:
			t.Errorf("h=%d: band shown but no separator rule rendered", h)
		case !bandShown && len(rules) > 0:
			t.Errorf("h=%d: band dropped but %d rule row(s) rendered %v", h, len(rules), rules)
		case bandShown:
			bandTop := issue233BandTopY(w)
			if rules[len(rules)-1] != bandTop {
				t.Errorf("h=%d: separator y=%d, want band top %d", h, rules[len(rules)-1], bandTop)
			}
		}
	}
}
