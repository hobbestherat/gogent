package ui

import (
	"testing"

	tui "github.com/hobbestherat/turbotui"
	tv "github.com/hobbestherat/turbotui/turbotv"
	"gogent/internal/config"
)

// This file exercises issue #214 — the polish of the #201 running-turn buttons
// (Interject / Queue / Stop). Three sub-fixes, each covered in its own group:
//
//  1. UNIFORM SIZE — the three buttons share one width (the widest label's) in
//     both full-label and degraded-glyph modes, instead of each sizing to its
//     own label (uniformButtonWidth / runningButtonsWidth).
//  2. ALIGNMENT — the 1-row buttons are centred on the prompt box's middle line
//     instead of floating at the top edge of the 3-row input area (buttonRowY),
//     and the idle Send button shares that row so nothing jumps on busy toggle.
//  3. INTERJECT COLOUR — the Interject foreground is readable and theme-aware in
//     BOTH the enabled and the disabled (empty-input) state, distinct from Stop's
//     error red and consistent with Queue (interjectButtonFG / mostReadableOn),
//     routed through the per-draw guardInterjectButton hook.
//
// The pre-#214 running_buttons_test.go encoded the old ragged widths (footprint
// 37, per-label button widths, glyph flip at wd=58); those three assertions were
// updated in place to the uniform spec (42 / uniform 13 / flip at wd=63). The
// tests below add the new #214 coverage and the stricter properties.

// ----------------------------------------------------------------------------
// Group 1 — uniform-width helpers.
// ----------------------------------------------------------------------------

// TestIssue214UniformButtonWidth pins uniformButtonWidth: it is the widest of the
// given labels' individual buttonWidths, so the three running buttons share one
// size. Full labels collapse to Interject (13); glyphs are already equal (5).
func TestIssue214UniformButtonWidth(t *testing.T) {
	full := uniformButtonWidth(interjectLabel, queueLabel, stopLabel)
	if full != 13 {
		t.Errorf("uniformButtonWidth(full) = %d, want 13 (Interject is the widest)", full)
	}
	glyph := uniformButtonWidth(interjectGlyph, queueGlyph, stopGlyph)
	if glyph != 5 {
		t.Errorf("uniformButtonWidth(glyph) = %d, want 5 (all glyphs equal)", glyph)
	}
	// It must equal the max of the individual buttonWidths (the definition).
	wantMax := buttonWidth(interjectLabel)
	if buttonWidth(queueLabel) > wantMax {
		wantMax = buttonWidth(queueLabel)
	}
	if buttonWidth(stopLabel) > wantMax {
		wantMax = buttonWidth(stopLabel)
	}
	if full != wantMax {
		t.Errorf("uniformButtonWidth = %d, want max of individual widths %d", full, wantMax)
	}
}

// TestIssue214UniformButtonWidthEdges covers the helper's degenerate inputs so the
// max-fold is robust: no labels → 0, all-empty → the empty label's frame width,
// a single label → its own width, and a mix where a short label does not shrink
// the common width.
func TestIssue214UniformButtonWidthEdges(t *testing.T) {
	if got := uniformButtonWidth(); got != 0 {
		t.Errorf("uniformButtonWidth() = %d, want 0 for no labels", got)
	}
	if got := uniformButtonWidth("", "", ""); got != buttonWidth("") {
		t.Errorf("uniformButtonWidth of empty labels = %d, want buttonWidth(\"\") = %d", got, buttonWidth(""))
	}
	if got := uniformButtonWidth(interjectLabel); got != buttonWidth(interjectLabel) {
		t.Errorf("uniformButtonWidth(single) = %d, want %d", got, buttonWidth(interjectLabel))
	}
	// A short label among long ones must not shrink the common width.
	if got := uniformButtonWidth(interjectLabel, "x", "y"); got != buttonWidth(interjectLabel) {
		t.Errorf("uniformButtonWidth(long,short,short) = %d, want %d", got, buttonWidth(interjectLabel))
	}
}

// TestIssue214RunningButtonsWidthUniformFormula pins runningButtonsWidth as three
// copies of the uniform width plus the two inter-button gaps and the right
// margin — the consequence of uniform sizing — for both label forms and for an
// arbitrary label set where the widest is not the first.
func TestIssue214RunningButtonsWidthUniformFormula(t *testing.T) {
	check := func(interject, queue, stop string) {
		t.Helper()
		got := runningButtonsWidth(interject, queue, stop)
		want := 3*uniformButtonWidth(interject, queue, stop) + 2*inputRowGap + inputRowMargin
		if got != want {
			t.Errorf("runningButtonsWidth(%q,%q,%q) = %d, want %d", interject, queue, stop, got, want)
		}
	}
	check(interjectLabel, queueLabel, stopLabel) // 42
	check(interjectGlyph, queueGlyph, stopGlyph) // 18
	// Widest label is the third one — the uniform width still drives all three slots.
	check("a", "bb", "cccccccccc")
}

// ----------------------------------------------------------------------------
// Group 2 — uniform width in the laid-out row (full + glyph modes).
// ----------------------------------------------------------------------------

// runningButtonRects returns the three running-turn buttons' current bounds.
func runningButtonRects(sw *SessionWindow) (i, q, s tv.Rect) {
	return boundsOf(sw.interjectButton), boundsOf(sw.queueButton), boundsOf(sw.stopButton)
}

// TestIssue214ButtonsUniformWidthFullLabels asserts the three running buttons are
// the same width on a wide window (full labels), and that the shared width is the
// uniform full width — not any button's own (shorter) label width.
func TestIssue214ButtonsUniformWidthFullLabels(t *testing.T) {
	w := newTestWorkbench(t)
	sw := w.openWindow("s", "S")
	sw.busy = true
	layout(sw, 80, 24, 3)

	i, q, s := runningButtonRects(sw)
	want := uniformButtonWidth(interjectLabel, queueLabel, stopLabel)
	if i.W != want || q.W != want || s.W != want {
		t.Errorf("full-label widths not uniform: i.W=%d q.W=%d s.W=%d, want all %d", i.W, q.W, s.W, want)
	}
	// Guard against a revert to per-label sizing: Queue (11) and Stop (10) must
	// have been widened to the shared 13, not left at their own widths.
	if q.W == buttonWidth(queueLabel) {
		t.Errorf("Queue kept its per-label width %d instead of the uniform %d", q.W, want)
	}
	if s.W == buttonWidth(stopLabel) {
		t.Errorf("Stop kept its per-label width %d instead of the uniform %d", s.W, want)
	}
}

// TestIssue214ButtonsUniformWidthGlyphs asserts uniformity holds in degraded-glyph
// mode too (narrow window): all three share the glyph width.
func TestIssue214ButtonsUniformWidthGlyphs(t *testing.T) {
	w := newTestWorkbench(t)
	sw := w.openWindow("s", "S")
	sw.busy = true
	layout(sw, 40, 24, 3) // narrow → glyphs

	if sw.interjectButton.Label != interjectGlyph {
		t.Fatalf("expected glyph mode at wd=40, got interject label %q", sw.interjectButton.Label)
	}
	i, q, s := runningButtonRects(sw)
	want := uniformButtonWidth(interjectGlyph, queueGlyph, stopGlyph)
	if i.W != want || q.W != want || s.W != want {
		t.Errorf("glyph widths not uniform: i.W=%d q.W=%d s.W=%d, want all %d", i.W, q.W, s.W, want)
	}
}

// TestIssue214ButtonsUniformWidthAcrossWidths is the property check: across a
// range of window widths (spanning both glyph and full-label modes, including the
// flip point), the three buttons are always the same width, and that width is
// always uniformButtonWidth of whichever labels that width selected.
func TestIssue214ButtonsUniformWidthAcrossWidths(t *testing.T) {
	w := newTestWorkbench(t)
	sw := w.openWindow("s", "S")
	sw.busy = true

	for _, wd := range []int{40, 44, 50, 56, 60, 62, 63, 64, 70, 80, 100, 120} {
		layout(sw, wd, 24, 3)
		i, q, s := runningButtonRects(sw)
		if i.W != q.W || q.W != s.W {
			t.Errorf("wd=%d: button widths differ (i=%d q=%d s=%d)", wd, i.W, q.W, s.W)
		}
		want := uniformButtonWidth(sw.interjectButton.Label, sw.queueButton.Label, sw.stopButton.Label)
		if i.W != want {
			t.Errorf("wd=%d: width %d != uniformButtonWidth(current labels) %d", wd, i.W, want)
		}
	}
}

// ----------------------------------------------------------------------------
// Group 3 — vertical alignment with the prompt box.
// ----------------------------------------------------------------------------

// TestIssue214ButtonRowY pins buttonRowY: it centres a 1-row button on the input
// area's middle line (top + (inputH-1)/2), the fix for the buttons floating at
// the top edge of the 3-row input area.
func TestIssue214ButtonRowY(t *testing.T) {
	for _, tc := range []struct {
		top, inputH, want int
	}{
		{21, 3, 22}, // the real layout: 3-row input, centre row is top+1
		{0, 3, 1},
		{0, 1, 0}, // single-row input centres on itself
		{0, 5, 2}, // odd height → exact middle row
		{10, 4, 11},
	} {
		if got := buttonRowY(tc.top, tc.inputH); got != tc.want {
			t.Errorf("buttonRowY(%d,%d) = %d, want %d", tc.top, tc.inputH, got, tc.want)
		}
	}
}

// TestIssue214ButtonsAlignedWithPromptBox is the core alignment assertion (issue
// #214): while busy, all three running buttons sit on the prompt box's vertical
// centre row — not the top row — are 1 row tall, and share one Y so they line up
// with each other and with the input.
func TestIssue214ButtonsAlignedWithPromptBox(t *testing.T) {
	w := newTestWorkbench(t)
	sw := w.openWindow("s", "S")
	sw.busy = true
	layout(sw, 80, 24, 3)

	in := boundsOfInput(sw)
	centerY := in.Y + (in.H-1)/2
	if centerY == in.Y {
		t.Fatalf("test setup: expected a multi-row input (H=%d) so centre != top", in.H)
	}
	for name, r := range map[string]tv.Rect{"interject": boundsOf(sw.interjectButton), "queue": boundsOf(sw.queueButton), "stop": boundsOf(sw.stopButton)} {
		if r.Y != centerY {
			t.Errorf("%s button Y=%d, want prompt-centre Y=%d (not the input's top row %d)", name, r.Y, centerY, in.Y)
		}
		if r.H != 1 {
			t.Errorf("%s button H=%d, want 1 (a single-row button)", name, r.H)
		}
	}
}

// TestIssue214ButtonsCentredNotFloatingAtTop is the direct regression guard for
// the alignment fix: the button row must NOT sit on the input area's top row
// (the pre-#214 "floats at the top" bug). With inputH=3 the centre is one row
// below the top.
func TestIssue214ButtonsCentredNotFloatingAtTop(t *testing.T) {
	w := newTestWorkbench(t)
	sw := w.openWindow("s", "S")
	sw.busy = true
	const wd, ht, inputH = 80, 24, 3
	layout(sw, wd, ht, inputH)

	top := ht - inputH
	stop := boundsOf(sw.stopButton)
	if stop.Y == top {
		t.Errorf("Stop floats at the input top row Y=%d; want the centre row %d (issue #214)", stop.Y, buttonRowY(top, inputH))
	}
	if stop.Y != buttonRowY(top, inputH) {
		t.Errorf("Stop Y=%d, want centred row %d", stop.Y, buttonRowY(top, inputH))
	}
}

// TestIssue214IdleSendAlignedWithPromptBox asserts the idle Send button (which
// occupies the same visual slot as the running buttons) is also centred on the
// prompt box and 1 row tall, so the slot's vertical placement is consistent.
func TestIssue214IdleSendAlignedWithPromptBox(t *testing.T) {
	w := newTestWorkbench(t)
	sw := w.openWindow("s", "S")
	layout(sw, 80, 24, 3) // idle

	in := boundsOfInput(sw)
	send := boundsOf(sw.sendButton)
	centerY := in.Y + (in.H-1)/2
	if send.Y != centerY {
		t.Errorf("idle Send Y=%d, want prompt-centre Y=%d", send.Y, centerY)
	}
	if send.H != 1 {
		t.Errorf("idle Send H=%d, want 1", send.H)
	}
}

// TestIssue214NoVerticalJumpOnBusyToggle is the alignment invariant the issue
// calls out: the idle Send and the busy running buttons sit on the same row, so
// toggling busy does not make the controls jump vertically.
func TestIssue214NoVerticalJumpOnBusyToggle(t *testing.T) {
	w := newTestWorkbench(t)
	sw := w.openWindow("s", "S")
	const wd, ht, inputH = 80, 24, 3

	layout(sw, wd, ht, inputH)
	idleY := boundsOf(sw.sendButton).Y

	sw.busy = true
	layout(sw, wd, ht, inputH)
	busyY := boundsOf(sw.stopButton).Y

	if idleY != busyY {
		t.Errorf("vertical jump on busy toggle: idle Send Y=%d, busy Stop Y=%d (want equal)", idleY, busyY)
	}
}

// ----------------------------------------------------------------------------
// Group 4 — Interject colour: readable + theme-aware in both states.
// ----------------------------------------------------------------------------

// issue214ThemeCases is the matrix of themes the Interject colour must read well
// on: the stock-turbotui default green button, the dark and high-contrast black
// presets, and the NO_COLOR neutral chrome. Each is applied hermetically under a
// withThemeRestore snapshot.
func issue214ThemeCases() []struct {
	name string
	cfg  config.ThemeConfig
	env  map[string]string
} {
	return []struct {
		name string
		cfg  config.ThemeConfig
		env  map[string]string
	}{
		{"default-truecolor", config.ThemeConfig{}, map[string]string{"TERM": "xterm", "COLORTERM": "truecolor"}},
		{"default-16color", config.ThemeConfig{}, map[string]string{"TERM": "xterm"}},
		{"dark", config.ThemeConfig{Name: "dark"}, map[string]string{"TERM": "xterm", "COLORTERM": "truecolor"}},
		{"high-contrast", config.ThemeConfig{Name: "high-contrast"}, map[string]string{"TERM": "xterm", "COLORTERM": "truecolor"}},
		{"NO_COLOR", config.ThemeConfig{}, map[string]string{"NO_COLOR": "1", "TERM": "xterm"}},
	}
}

// TestIssue214InterjectEnabledMatchesQueueDistinctFromStop verifies the enabled
// Interject foreground matches Queue (the theme's ButtonFG) and stays distinct
// from Stop's error red, on every theme.
func TestIssue214InterjectEnabledMatchesQueueDistinctFromStop(t *testing.T) {
	for _, c := range issue214ThemeCases() {
		t.Run(c.name, func(t *testing.T) {
			withThemeRestore(t)
			ApplyTheme(ResolveTheme(c.cfg, envOf(c.env), false))
			buttonFG := tv.ActiveTheme().ButtonFG
			if got := interjectButtonFG(true); got != buttonFG {
				t.Errorf("enabled Interject = %+v, want Queue's ButtonFG %+v", got, buttonFG)
			}
			// Under NO_COLOR every colour is the terminal default, so the
			// distinctness check is meaningless there.
			if c.name == "NO_COLOR" {
				return
			}
			if got := interjectButtonFG(true); got == colorError {
				t.Errorf("enabled Interject %+v collides with Stop's error colour", got)
			}
		})
	}
}

// TestIssue214InterjectDisabledReadableAcrossThemes is the core colour fix (issue
// #214): the disabled (empty-input) Interject foreground must be legible — its
// WCAG contrast against the button background clears the 3:1 large-text floor —
// on every theme with a determinable background. Under NO_COLOR the background is
// the unknowable terminal default, so the function takes the documented
// undeterminable branch and returns colorNote (which is itself the default).
func TestIssue214InterjectDisabledReadableAcrossThemes(t *testing.T) {
	for _, c := range issue214ThemeCases() {
		t.Run(c.name, func(t *testing.T) {
			withThemeRestore(t)
			ApplyTheme(ResolveTheme(c.cfg, envOf(c.env), false))
			bg := tv.ActiveTheme().ButtonBG
			disabled := interjectButtonFG(false)
			ratio := contrastRatio(disabled, bg)
			if ratio == 0 {
				// Undeterminable (NO_COLOR): the documented branch returns colorNote.
				if disabled != colorNote {
					t.Errorf("undeterminable bg: disabled = %+v, want colorNote (the NO_COLOR branch)", disabled)
				}
				return
			}
			if ratio < minContrastLarge {
				t.Errorf("%s: disabled Interject contrast %.3f < minContrastLarge %.1f (fg %+v on bg %+v) — illegible",
					c.name, ratio, minContrastLarge, disabled, bg)
			}
		})
	}
}

// TestIssue214InterjectDisabledDefaultThemeFallback pins the default-theme path
// specifically: colorNote (light grey) is only ~1.3:1 on the stock green button —
// the bug — so the function must NOT return colorNote; it must fall back to
// mostReadableOn(ButtonBG), which is the higher-contrast of black/white and clears
// the floor.
func TestIssue214InterjectDisabledDefaultThemeFallback(t *testing.T) {
	withThemeRestore(t)
	ApplyTheme(ResolveTheme(config.ThemeConfig{}, truecolorEnv, false))
	bg := tv.ActiveTheme().ButtonBG

	noteRatio := contrastRatio(colorNote, bg)
	if noteRatio >= minContrastLarge {
		t.Fatalf("precondition changed: colorNote now clears the floor on the default button (%.3f:1); re-evaluate this test", noteRatio)
	}
	disabled := interjectButtonFG(false)
	if disabled == colorNote {
		t.Errorf("disabled Interject is colorNote (%.3f:1 on the default button) — the illegible pre-#214 colour", noteRatio)
	}
	if disabled != mostReadableOn(bg) {
		t.Errorf("disabled Interject = %+v, want the contrast fallback mostReadableOn(bg) = %+v", disabled, mostReadableOn(bg))
	}
	if r := contrastRatio(disabled, bg); r < minContrastLarge {
		t.Errorf("disabled Interject fallback contrast %.3f < %.1f", r, minContrastLarge)
	}
}

// TestIssue214InterjectDisabledDistinctFromStop asserts the disabled Interject
// foreground does not collide with Stop's error red (so the two remain visually
// distinct) on every theme with a determinable background.
func TestIssue214InterjectDisabledDistinctFromStop(t *testing.T) {
	for _, c := range issue214ThemeCases() {
		t.Run(c.name, func(t *testing.T) {
			withThemeRestore(t)
			ApplyTheme(ResolveTheme(c.cfg, envOf(c.env), false))
			if contrastRatio(colorError, tv.ActiveTheme().ButtonBG) == 0 {
				return // NO_COLOR: everything is the terminal default
			}
			if interjectButtonFG(false) == colorError {
				t.Errorf("disabled Interject collides with Stop's error colour")
			}
		})
	}
}

// TestIssue214MostReadableOn pins mostReadableOn: it returns whichever of
// black/white has the greater WCAG contrast against the background, defaulting to
// white on a tie and on an undeterminable (terminal-default) background.
func TestIssue214MostReadableOn(t *testing.T) {
	white, black := tui.ANSIColor(15), tui.ANSIColor(0)
	// Black text is most readable on a bright (white) background.
	if got := mostReadableOn(white); got != black {
		t.Errorf("mostReadableOn(white) = %+v, want black", got)
	}
	// White text is most readable on a black background.
	if got := mostReadableOn(black); got != white {
		t.Errorf("mostReadableOn(black) = %+v, want white", got)
	}
	// Undeterminable background (terminal default): both ratios are 0, so the >=
	// tie picks white.
	if got := mostReadableOn(tui.DefaultColor()); got != white {
		t.Errorf("mostReadableOn(default) = %+v, want white (tie-break)", got)
	}
	// For an arbitrary background the result is always one of black/white and is
	// at least as contrasty as the other.
	for _, bg := range []tui.Color{tui.ANSIColor(2), tui.ANSIColor(4), tui.ANSIColor(11)} {
		got := mostReadableOn(bg)
		if got != black && got != white {
			t.Errorf("mostReadableOn(%+v) = %+v, want black or white", bg, got)
		}
		other := black
		if got == black {
			other = white
		}
		if contrastRatio(got, bg) < contrastRatio(other, bg) {
			t.Errorf("mostReadableOn(%+v) = %+v (%.3f) is less readable than %+v (%.3f)",
				bg, got, contrastRatio(got, bg), other, contrastRatio(other, bg))
		}
	}
}

// TestIssue214GuardInterjectHookAppliesStateColour drives the real per-draw hook
// (guardInterjectButton) via a desktop redraw and asserts it paints the Interject
// button with interjectButtonFG of its live enabled/disabled state — and that
// toggling the input text live-recalculates the colour on the next redraw. This
// catches wiring defects the pure-function tests cannot (a hook that ignores the
// state, never calls interjectButtonFG, or reads a stale theme).
func TestIssue214GuardInterjectHookAppliesStateColour(t *testing.T) {
	withThemeRestore(t)
	ApplyTheme(ResolveTheme(config.ThemeConfig{}, truecolorEnv, false))
	w := newTestWorkbench(t)
	sw := w.openWindow("s", "S")
	sw.busy = true
	layout(sw, 80, 24, 3)

	assertFG := func(want tui.Color, state string) {
		t.Helper()
		w.desktop.Redraw()
		if got := sw.interjectButton.FG; got != want {
			t.Errorf("after redraw (%s): hook set Interject FG %+v, want %+v", state, got, want)
		}
	}

	// Empty input → disabled.
	sw.input.SetText("")
	assertFG(interjectButtonFG(false), "disabled")

	// Non-blank input → enabled.
	sw.input.SetText("a clarification")
	assertFG(interjectButtonFG(true), "enabled")

	// Clearing the input live-flips back to disabled (the hook re-reads state).
	sw.input.SetText("")
	assertFG(interjectButtonFG(false), "back-to-disabled")
}

// TestIssue214GuardInterjectHookRecoloursOnThemeSwitch verifies the hook reads
// the theme live: after switching themes (and re-seeding the window so its
// background tracks the new palette, as the production RefreshTheme path does),
// a redraw repaints Interject with the new theme's disabled colour, which differs
// from the old one.
func TestIssue214GuardInterjectHookRecoloursOnThemeSwitch(t *testing.T) {
	withThemeRestore(t)
	w := newTestWorkbench(t)
	sw := w.openWindow("s", "S")
	sw.busy = true
	layout(sw, 80, 24, 3)
	sw.input.SetText("") // disabled

	applyAndRedraw := func(cfg config.ThemeConfig) (fg, want tui.Color) {
		t.Helper()
		ApplyTheme(ResolveTheme(cfg, truecolorEnv, false))
		sw.refreshTheme() // re-seed the window chrome to the new palette (#204 path)
		w.desktop.Redraw()
		// Evaluate the expected colour under the theme just applied (before the
		// next iteration swaps it out).
		return sw.interjectButton.FG, interjectButtonFG(false)
	}

	defFG, defWant := applyAndRedraw(config.ThemeConfig{})               // default: disabled fallback (black)
	darkFG, darkWant := applyAndRedraw(config.ThemeConfig{Name: "dark"}) // dark: colorNote (grey, clears the floor)
	if defFG != defWant {
		t.Errorf("default-theme disabled FG %+v != interjectButtonFG(false) %+v", defFG, defWant)
	}
	if darkFG != darkWant {
		t.Errorf("dark-theme disabled FG %+v != interjectButtonFG(false) %+v", darkFG, darkWant)
	}
	if defFG == darkFG {
		t.Errorf("theme switch did not recolour Interject: default and dark both %+v", defFG)
	}
	// On the dark theme colorNote clears the floor, so the disabled colour stays
	// legible against the freshly re-seeded dark button background.
	if r := contrastRatio(darkFG, sw.interjectButton.BG); r < minContrastLarge {
		t.Errorf("dark-theme disabled Interject contrast %.3f < %.1f after the switch", r, minContrastLarge)
	}
}

// ----------------------------------------------------------------------------
// Group 5 — degrade threshold under uniform sizing.
// ----------------------------------------------------------------------------

// TestIssue214DegradeFlipAtUniformFootprint pins the exact glyph/full flip under
// uniform sizing (wd=63, where the prompt gets exactly minInputWidth) and that the
// flip width is runningButtonsWidth(full) + minInputWidth + the prompt-button gap.
func TestIssue214DegradeFlipAtUniformFootprint(t *testing.T) {
	w := newTestWorkbench(t)
	sw := w.openWindow("s", "S")
	sw.busy = true

	// One below the flip: glyphs.
	layout(sw, 62, 24, 3)
	if sw.interjectButton.Label != interjectGlyph {
		t.Errorf("wd=62 should use glyphs, got %q", sw.interjectButton.Label)
	}
	// At the flip: full labels and the prompt gets exactly minInputWidth.
	layout(sw, 63, 24, 3)
	if sw.interjectButton.Label != interjectLabel {
		t.Errorf("wd=63 should use full labels, got %q", sw.interjectButton.Label)
	}
	if got := boundsOfInput(sw).W; got != minInputWidth {
		t.Errorf("flip-point input width = %d, want exactly minInputWidth %d", got, minInputWidth)
	}
	// The flip width is the full footprint + minInputWidth + the prompt-button gap.
	wantFlip := runningButtonsWidth(interjectLabel, queueLabel, stopLabel) + minInputWidth + inputRowGap
	if wantFlip != 63 {
		t.Errorf("expected flip at 63 (footprint %d + minInput %d + gap %d), got formula %d",
			runningButtonsWidth(interjectLabel, queueLabel, stopLabel), minInputWidth, inputRowGap, wantFlip)
	}
	// And the prompt only grows from the flip upward.
	layout(sw, 80, 24, 3)
	if got := boundsOfInput(sw).W; got <= minInputWidth {
		t.Errorf("wd=80 input width = %d, want > minInputWidth %d", got, minInputWidth)
	}
}

// ----------------------------------------------------------------------------
// Group 6 — edge cases / robustness.
// ----------------------------------------------------------------------------

// TestIssue214LayoutAtMinimumWindowWidth exercises the narrowest realistic window
// (40, the window minimum): the buttons stay uniform, on-screen, non-overlapping,
// and the prompt keeps at least one cell.
func TestIssue214LayoutAtMinimumWindowWidth(t *testing.T) {
	w := newTestWorkbench(t)
	sw := w.openWindow("s", "S")
	sw.busy = true
	const wd = 40
	layout(sw, wd, 24, 3)

	in := boundsOfInput(sw)
	i, q, s := runningButtonRects(sw)
	if in.W < 1 {
		t.Errorf("wd=%d: prompt width %d must be >= 1", wd, in.W)
	}
	if i.W != q.W || q.W != s.W {
		t.Errorf("wd=%d: buttons not uniform (i=%d q=%d s=%d)", wd, i.W, q.W, s.W)
	}
	for name, r := range map[string]tv.Rect{"interject": i, "queue": q, "stop": s} {
		if r.X < 0 {
			t.Errorf("wd=%d: %s off-screen left at X=%d", wd, name, r.X)
		}
	}
	for _, pair := range [][2]tv.Rect{{in, i}, {i, q}, {q, s}} {
		if rectsMeet(pair[0], pair[1]) {
			t.Errorf("wd=%d: input-row widgets overlap: %+v and %+v", wd, pair[0], pair[1])
		}
	}
	if got := s.X + s.W; got != wd-inputRowMargin {
		t.Errorf("wd=%d: Stop right edge %d, want %d (flush with the margin)", wd, got, wd-inputRowMargin)
	}
}

// ----------------------------------------------------------------------------
// Group 7 — characterization: an accepted gamut quirk the fix surfaces.
// ----------------------------------------------------------------------------

// TestIssue214DefaultDisabledNotDimmerThanEnabled_Characterization documents a
// real quirk the uniform/colour fix exposes (pinned, not asserted-as-desired): on
// the stock-turbotui default GREEN button the enabled label is white at only
// ~3.1:1 — the gamut-limited floor turbotui ships — while the disabled fallback
// (black) reaches ~6.75:1, so the "disabled" state is paradoxically the MORE
// legible of the two. There is no legible-but-dimmer 16-colour alternative to
// white on green (grey drops to ~1.3:1, the original bug), so this is an accepted
// trade-off, pinned here so a future palette change or turbotui bump that alters
// it is a conscious decision — exactly the way theme_issue202_test.go pins its
// out-of-scope gamut limits.
func TestIssue214DefaultDisabledNotDimmerThanEnabled_Characterization(t *testing.T) {
	withThemeRestore(t)
	ApplyTheme(ResolveTheme(config.ThemeConfig{}, truecolorEnv, false))
	bg := tv.ActiveTheme().ButtonBG
	enabled := interjectButtonFG(true)
	disabled := interjectButtonFG(false)
	enR, disR := contrastRatio(enabled, bg), contrastRatio(disabled, bg)

	// The actual #214 acceptance: both states clear the large-text floor.
	if enR < minContrastLarge {
		t.Errorf("enabled Interject contrast %.3f < %.1f", enR, minContrastLarge)
	}
	if disR < minContrastLarge {
		t.Errorf("disabled Interject contrast %.3f < %.1f", disR, minContrastLarge)
	}
	// Pin the concrete colours and ratios so a change is loud.
	if enabled != tui.ANSIColor(15) {
		t.Errorf("default enabled Interject = %+v, want stock ButtonFG ANSI 15 (white)", enabled)
	}
	if disabled != tui.ANSIColor(0) {
		t.Errorf("default disabled Interject = %+v, want fallback ANSI 0 (black)", disabled)
	}
	t.Logf("characterization: default-theme enabled=%.3f:1 (white, gamut-limited), disabled=%.3f:1 (black fallback) — disabled is MORE legible than enabled; accepted 16-colour gamut trade-off (see issue #214 / #202)", enR, disR)
}
