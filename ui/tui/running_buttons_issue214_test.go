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
//     own label (uniformButtonWidth / runningButtonsColumnWidth).
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

// TestIssue214RunningButtonsWidthUniformFormula pins runningButtonsColumnWidth as
// ONE copy of the uniform width plus the prompt gap and the right margin — the
// horizontal room the vertically-stacked column claims (issue #234) — for both
// label forms and for an arbitrary label set where the widest is not the first.
func TestIssue214RunningButtonsWidthUniformFormula(t *testing.T) {
	check := func(interject, queue, stop string) {
		t.Helper()
		got := runningButtonsColumnWidth(interject, queue, stop)
		want := uniformButtonWidth(interject, queue, stop) + inputRowGap + inputRowMargin
		if got != want {
			t.Errorf("runningButtonsColumnWidth(%q,%q,%q) = %d, want %d", interject, queue, stop, got, want)
		}
	}
	check(interjectLabel, queueLabel, stopLabel) // 15
	check(interjectGlyph, queueGlyph, stopGlyph) // 7
	// Widest label is the third one — the uniform width still drives the column.
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
// mode too (narrow window): all three share the glyph width. The vertical column
// (#234) narrowed the footprint, so the glyph flip moved from wd=58 to wd=35; a
// wd=30 window is below that flip and renders glyphs.
func TestIssue214ButtonsUniformWidthGlyphs(t *testing.T) {
	w := newTestWorkbench(t)
	sw := w.openWindow("s", "S")
	sw.busy = true
	layout(sw, 30, 24, 3) // below the wd=35 flip → glyphs

	if sw.interjectButton.Label != interjectGlyph {
		t.Fatalf("expected glyph mode at wd=30, got interject label %q", sw.interjectButton.Label)
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

	for _, wd := range []int{30, 34, 35, 40, 50, 60, 70, 80, 100, 120} {
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

// TestIssue214ButtonsAlignedWithPromptBox is the core alignment assertion, updated
// for the #234 vertical stack: while busy the three running buttons line up with
// the prompt box by spanning its three rows — Queue on the prompt's top row,
// Interject on the middle row, Stop on the bottom row — each 1 row tall and every
// one inside the input area. (Pre-#234 they shared the prompt's centre row; #234
// spreads them across all three rows so they stack beside the prompt.)
func TestIssue214ButtonsAlignedWithPromptBox(t *testing.T) {
	w := newTestWorkbench(t)
	sw := w.openWindow("s", "S")
	sw.busy = true
	layout(sw, 80, 24, 3)

	in := boundsOfInput(sw)
	queue := boundsOf(sw.queueButton)
	interject := boundsOf(sw.interjectButton)
	stop := boundsOf(sw.stopButton)
	// Each button sits on a distinct prompt row (top / middle / bottom).
	if queue.Y != in.Y {
		t.Errorf("Queue Y=%d, want the prompt top row %d", queue.Y, in.Y)
	}
	if interject.Y != in.Y+(in.H-1)/2 {
		t.Errorf("Interject Y=%d, want the prompt centre row %d", interject.Y, in.Y+(in.H-1)/2)
	}
	if stop.Y != in.Y+in.H-1 {
		t.Errorf("Stop Y=%d, want the prompt bottom row %d", stop.Y, in.Y+in.H-1)
	}
	// Every button stays within the prompt box's rows and is 1 row tall.
	for name, r := range map[string]tv.Rect{"queue": queue, "interject": interject, "stop": stop} {
		if r.H != 1 {
			t.Errorf("%s button H=%d, want 1 (a single-row button)", name, r.H)
		}
		if r.Y < in.Y || r.Y >= in.Y+in.H {
			t.Errorf("%s button Y=%d falls outside the input rows [%d,%d)", name, r.Y, in.Y, in.Y+in.H)
		}
	}
}

// TestIssue214ButtonsCentredNotFloatingAtTop is the direct regression guard for the
// alignment fix, updated for #234: the column must NOT clump at the input area's
// top row (the pre-#214 "floats at the top" bug). Instead it spans the whole input
// height — covering the top, middle AND bottom rows — so the stack fills the prompt
// box rather than sitting at one edge.
func TestIssue214ButtonsCentredNotFloatingAtTop(t *testing.T) {
	w := newTestWorkbench(t)
	sw := w.openWindow("s", "S")
	sw.busy = true
	const wd, ht, inputH = 80, 24, 3
	layout(sw, wd, ht, inputH)

	top := ht - inputH
	queue := boundsOf(sw.queueButton)
	stop := boundsOf(sw.stopButton)
	// The column spans from the input top row to the input bottom row — it does
	// not float at just the top edge.
	if queue.Y != top {
		t.Errorf("Queue Y=%d, want the input top row %d (column should start at the top)", queue.Y, top)
	}
	if stop.Y != top+inputH-1 {
		t.Errorf("Stop Y=%d, want the input bottom row %d (column should reach the bottom, not float at the top)",
			stop.Y, top+inputH-1)
	}
}

// TestIssue214IdleSendAlignedWithPromptBox asserts the idle Send button (which
// occupies the same visual slot as the running buttons) is centred on the prompt
// box and 1 row tall, so the slot's vertical placement is consistent. The idle
// Send placement is unchanged by #234 (only the busy column stacks).
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

// TestIssue214NoControlLeavesInputAreaOnBusyToggle is the alignment invariant the
// issue calls out, updated for #234: the idle Send button and the busy running
// buttons all stay within the prompt box's rows across a busy toggle, so toggling
// never pushes a control outside the input area. (Pre-#234 the invariant was
// "same single row"; #234 spreads the busy column across the prompt's rows, so the
// invariant is now "within the input area" rather than "same row".)
func TestIssue214NoControlLeavesInputAreaOnBusyToggle(t *testing.T) {
	w := newTestWorkbench(t)
	sw := w.openWindow("s", "S")
	const wd, ht, inputH = 80, 24, 3

	// Idle: Send sits within the input area.
	layout(sw, wd, ht, inputH)
	in := boundsOfInput(sw)
	send := boundsOf(sw.sendButton)
	if send.Y < in.Y || send.Y >= in.Y+in.H {
		t.Fatalf("idle Send Y=%d outside the input area [%d,%d)", send.Y, in.Y, in.Y+in.H)
	}

	// Busy: every column button stays within the same input area's rows.
	sw.busy = true
	layout(sw, wd, ht, inputH)
	for name, b := range map[string]*tv.Button{"queue": sw.queueButton, "interject": sw.interjectButton, "stop": sw.stopButton} {
		r := boundsOf(b)
		if r.Y < in.Y || r.Y >= in.Y+in.H {
			t.Errorf("busy %s Y=%d jumped outside the input area [%d,%d) on toggle", name, r.Y, in.Y, in.Y+in.H)
		}
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

// TestIssue214InterjectBothStatesReadableAcrossThemes is the core colour fix
// (issue #214): the Interject foreground must be legible in BOTH states — its
// WCAG contrast against the button background clears the 3:1 large-text floor for
// the enabled colour (ButtonFG, the driver's choice) and the disabled colour
// alike — on every theme with a determinable background. Under NO_COLOR the
// background is the unknowable terminal default, so the disabled path takes the
// documented undeterminable branch and returns colorNote (which is itself the
// default); enabled is ButtonFG, likewise the default.
func TestIssue214InterjectBothStatesReadableAcrossThemes(t *testing.T) {
	for _, c := range issue214ThemeCases() {
		t.Run(c.name, func(t *testing.T) {
			withThemeRestore(t)
			ApplyTheme(ResolveTheme(c.cfg, envOf(c.env), false))
			bg := tv.ActiveTheme().ButtonBG
			if contrastRatio(bg, bg) == 0 && bg.Mode == tui.ColorDefault {
				// Undeterminable (NO_COLOR): the disabled branch must return
				// colorNote; enabled is ButtonFG. Both are the terminal default
				// here, so no contrast can be measured — just pin the branch.
				if interjectButtonFG(false) != colorNote {
					t.Errorf("%s: undeterminable bg, disabled should be colorNote, got %+v", c.name, interjectButtonFG(false))
				}
				if interjectButtonFG(true) != tv.ActiveTheme().ButtonFG {
					t.Errorf("%s: enabled should be ButtonFG", c.name)
				}
				return
			}
			for _, state := range []struct {
				name string
				fg   tui.Color
			}{
				{"enabled", interjectButtonFG(true)},
				{"disabled", interjectButtonFG(false)},
			} {
				ratio := contrastRatio(state.fg, bg)
				if ratio < minContrastLarge {
					t.Errorf("%s: %s Interject contrast %.3f < minContrastLarge %.1f (fg %+v on bg %+v) — illegible",
						c.name, state.name, ratio, minContrastLarge, state.fg, bg)
				}
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
// the #234 vertical column (wd=35, where the prompt gets exactly minInputWidth) and
// that the flip width is runningButtonsColumnWidth(full) + minInputWidth. The
// column claims one frame + gap + margin (15), not three frames, so the flip moved
// down from the pre-#234 wd=63 to wd=35.
func TestIssue214DegradeFlipAtUniformFootprint(t *testing.T) {
	w := newTestWorkbench(t)
	sw := w.openWindow("s", "S")
	sw.busy = true

	// One below the flip: glyphs.
	layout(sw, 34, 24, 3)
	if sw.interjectButton.Label != interjectGlyph {
		t.Errorf("wd=34 should use glyphs, got %q", sw.interjectButton.Label)
	}
	// At the flip: full labels and the prompt gets exactly minInputWidth.
	layout(sw, 35, 24, 3)
	if sw.interjectButton.Label != interjectLabel {
		t.Errorf("wd=35 should use full labels, got %q", sw.interjectButton.Label)
	}
	if got := boundsOfInput(sw).W; got != minInputWidth {
		t.Errorf("flip-point input width = %d, want exactly minInputWidth %d", got, minInputWidth)
	}
	// The flip width is the column footprint + minInputWidth (the gap and margin
	// are already inside runningButtonsColumnWidth).
	wantFlip := runningButtonsColumnWidth(interjectLabel, queueLabel, stopLabel) + minInputWidth
	if wantFlip != 35 {
		t.Errorf("expected flip at 35 (column %d + minInput %d), got formula %d",
			runningButtonsColumnWidth(interjectLabel, queueLabel, stopLabel), minInputWidth, wantFlip)
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

// TestIssue214LayoutAtMinimumWindowWidth exercises a representative narrow window:
// the buttons stay uniform, on-screen, non-overlapping (checked in 2D, since the
// stacked column shares an X), form a vertical column, and the prompt keeps at
// least one cell. Note the #234 column is narrow enough that wd=40 is now in
// full-label mode (the glyph flip is at wd=35).
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
	// Vertical column order (Queue→Interject→Stop, distinct increasing Y).
	if q.Y >= i.Y || i.Y >= s.Y {
		t.Errorf("wd=%d: not in vertical order (q.Y=%d i.Y=%d s.Y=%d)", wd, q.Y, i.Y, s.Y)
	}
	for name, r := range map[string]tv.Rect{"interject": i, "queue": q, "stop": s} {
		if r.X < 0 {
			t.Errorf("wd=%d: %s off-screen left at X=%d", wd, name, r.X)
		}
	}
	// 2D overlap check: the stacked buttons share an X, so an X-only check would
	// wrongly flag them.
	for _, pair := range [][2]tv.Rect{{in, i}, {in, q}, {in, s}, {i, q}, {q, s}, {i, s}} {
		if rectsOverlap2D(pair[0], pair[1]) {
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
