package ui

import (
	"math"
	"strings"
	"testing"

	"gogent/internal/agent"
	"gogent/internal/config"

	tui "github.com/hobbestherat/turbotui"
	tv "github.com/hobbestherat/turbotui/turbotv"
)

// This file exercises issue #202: a WCAG contrast audit of the default theme
// palette. The immediate bug — the "queued (will send...)" system note (and every
// other addNote system note) painted in the low-contrast colorInfo (ANSI 12 bright
// blue, ~2.6:1 on the blue window) — was fixed by recolouring colorInfo to ANSI 6
// (cyan, ~4.6:1), colorNote and chromeDivider from ANSI 8 (dim grey, ~1.8:1) to
// ANSI 7 (light grey, ~5.7:1), and by adding the paletteContrast audit plus the
// WCAG helpers (colorRGB / relativeLuminance / contrastRatio).
//
// The tests are organised in five groups:
//  1. WCAG primitive helpers — resolution, luminance bounds, ratio sanity/edges.
//  2. The default-palette audit — every role clears its documented threshold.
//  3. The specific fixes — info/note/divider recoloured and readable.
//  4. The addNote path — system notes really are painted in colorInfo.
//  5. Documented edge cases / known limits, pinned as characterisation tests so a
//     future palette change is intentional (these are real defects the audit found
//     that are out of scope for #202's default-only fix, plus the marginal cases).

// initColor* capture the package-level colour globals at test-binary init time —
// the built-in "default" palette baked into theme.go before any test mutates them.
// Asserting against these (rather than the live globals, which ApplyTheme mutates)
// makes the "initial values match defaultPalette() and are not the old low-contrast
// indices" check independent of test execution order. This mirrors the
// initChrome*/initMd* captures in theme_issue200_test.go.
var (
	initColorInfo     = colorInfo
	initColorNote     = colorNote
	initChromeDivider = chromeDivider
)

// findingByRole returns the audit finding for a role, failing the test if it is
// absent — paletteContrast must always produce one finding per painted role.
func findingByRole(t *testing.T, findings []contrastFinding, role string) contrastFinding {
	t.Helper()
	for _, f := range findings {
		if f.Role == role {
			return f
		}
	}
	t.Fatalf("paletteContrast produced no finding for role %q (have %v)", role, findings)
	return contrastFinding{}
}

// approx reports whether two contrast ratios are equal to 1e-9, the precision
// WCAG ratios are compared at in these tests.
func approx(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

// ----------------------------------------------------------------------------
// Group 1: WCAG primitive helpers.
// ----------------------------------------------------------------------------

// TestIssue202ColorRGBResolution checks colorRGB resolves ANSI indices through the
// canonical 16-colour table (so the audit measures what a 16-colour terminal
// actually shows), passes RGB through untouched, and reports the terminal default
// as indeterminate. paletteContrast relies on all three.
func TestIssue202ColorRGBResolution(t *testing.T) {
	// RGB passes through verbatim.
	if r, g, b, ok := colorRGB(tui.RGBColor(0xE6, 0x9F, 0x00)); !ok || r != 0xE6 || g != 0x9F || b != 0x00 {
		t.Errorf("colorRGB(RGB) = (%d,%d,%d,%v), want (0xE6,0x9F,0x00,true)", r, g, b, ok)
	}
	// The terminal default is unknowable — the very case that makes NO_COLOR audit
	// indeterminate.
	if _, _, _, ok := colorRGB(tui.DefaultColor()); ok {
		t.Errorf("colorRGB(DefaultColor) ok = true; the terminal default is unknowable")
	}

	// Canonical 16-colour ANSI → RGB spot checks (must match the table degrade uses,
	// else the audit would measure a different colour than the terminal renders).
	for _, c := range []struct {
		idx     uint8
		r, g, b uint8
	}{
		{0, 0, 0, 0},        // black
		{4, 0, 0, 170},      // blue  — the default window background
		{7, 170, 170, 170},  // light grey
		{8, 85, 85, 85},     // dim grey — the old low-contrast note/divider
		{9, 255, 85, 85},    // bright red
		{12, 85, 85, 255},   // bright blue — the old low-contrast colorInfo
		{15, 255, 255, 255}, // bright white
	} {
		r, g, b, ok := colorRGB(tui.ANSIColor(c.idx))
		if !ok || r != c.r || g != c.g || b != c.b {
			t.Errorf("colorRGB(ANSI %d) = (%d,%d,%d,%v), want (%d,%d,%d,true)",
				c.idx, r, g, b, ok, c.r, c.g, c.b)
		}
	}

	// A 256-cube index resolves too (231 is white, 232 the darkest grey ramp step),
	// so the audit works on a 256-colour terminal's quantised colours.
	if r, g, b, ok := colorRGB(tui.ANSIColor(231)); !ok || r != 255 || g != 255 || b != 255 {
		t.Errorf("colorRGB(ANSI 231) = (%d,%d,%d,%v), want white", r, g, b, ok)
	}
	if _, _, _, ok := colorRGB(tui.ANSIColor(232)); !ok {
		t.Errorf("colorRGB(ANSI 232) ok = false; grey-ramp indices must resolve")
	}
}

// TestIssue202RelativeLuminance covers the WCAG luminance bounds and monotonicity
// along the neutral axis: black is 0, white is 1, and lighter greys are brighter
// than darker ones.
func TestIssue202RelativeLuminance(t *testing.T) {
	if l := relativeLuminance(0, 0, 0); !approx(l, 0) {
		t.Errorf("relativeLuminance(black) = %g, want 0", l)
	}
	if l := relativeLuminance(255, 255, 255); !approx(l, 1) {
		t.Errorf("relativeLuminance(white) = %g, want 1", l)
	}
	// A mid-grey is strictly between black and white.
	mid := relativeLuminance(128, 128, 128)
	if !(mid > 0 && mid < 1) {
		t.Fatalf("relativeLuminance(mid-grey) = %g, want within (0,1)", mid)
	}
	// Lighter neutral axis ⇒ higher luminance (monotonic on greys).
	if relativeLuminance(200, 200, 200) <= mid {
		t.Errorf("relativeLuminance is not monotonic on the neutral axis")
	}
	if relativeLuminance(60, 60, 60) >= mid {
		t.Errorf("relativeLuminance is not monotonic on the neutral axis")
	}
}

// TestIssue202ContrastRatioPrimitives pins the WCAG ratio invariants: the extreme
// pair is 21:1, identical colours are 1:1, the ratio is symmetric, every ratio is
// in [1,21], and any pairing involving the unknowable terminal default yields 0
// (which no threshold accepts, so an indeterminate pair can never silently pass).
func TestIssue202ContrastRatioPrimitives(t *testing.T) {
	black := tui.RGBColor(0, 0, 0)
	white := tui.RGBColor(255, 255, 255)

	if r := contrastRatio(white, black); !approx(r, 21) {
		t.Errorf("contrastRatio(white,black) = %g, want 21", r)
	}
	if r := contrastRatio(black, white); !approx(r, 21) {
		t.Errorf("contrastRatio(black,white) = %g, want 21 (symmetry)", r)
	}
	if r := contrastRatio(tui.ANSIColor(6), tui.ANSIColor(6)); !approx(r, 1) {
		t.Errorf("contrastRatio(equal colours) = %g, want 1", r)
	}

	// Symmetry for an arbitrary unequal pairing.
	cyan, blue := tui.ANSIColor(6), tui.ANSIColor(4)
	if contrastRatio(cyan, blue) != contrastRatio(blue, cyan) {
		t.Errorf("contrastRatio is not symmetric for cyan/blue")
	}

	// Every resolvable ratio lies in the closed interval [1,21].
	for _, fg := range []tui.Color{black, white, cyan, blue, tui.ANSIColor(9), tui.RGBColor(0x7A, 0xA2, 0xF7)} {
		for _, bg := range []tui.Color{black, white, blue, tui.RGBColor(0x10, 0x14, 0x50)} {
			r := contrastRatio(fg, bg)
			if r < 1 || r > 21 {
				t.Errorf("contrastRatio(%+v,%+v) = %g, out of [1,21]", fg, bg, r)
			}
		}
	}

	// The terminal default is indeterminate on either side.
	def := tui.DefaultColor()
	if r := contrastRatio(def, white); r != 0 {
		t.Errorf("contrastRatio(default,white) = %g, want 0 (default FG indeterminate)", r)
	}
	if r := contrastRatio(white, def); r != 0 {
		t.Errorf("contrastRatio(white,default) = %g, want 0 (default BG indeterminate)", r)
	}
	if r := contrastRatio(def, def); r != 0 {
		t.Errorf("contrastRatio(default,default) = %g, want 0", r)
	}
}

// ----------------------------------------------------------------------------
// Group 2: the default-palette audit (the core requirement of #202).
// ----------------------------------------------------------------------------

// TestIssue202DefaultPalettePassesAudit is the central assertion: every default
// palette role meets its required minimum contrast against the background it is
// actually rendered on. The window background for the default theme is the stock
// turbotui WindowBG (ANSI 4 blue); the audit must be run against that real value,
// not an assumed one.
func TestIssue202DefaultPalettePassesAudit(t *testing.T) {
	// Guard: the default window content surface really is ANSI 4. If the stock
	// turbotui theme ever changes, paletteContrast must be called with the new
	// value — so pin the precondition explicitly.
	if baseTVTheme.WindowBG != tui.ANSIColor(4) {
		t.Fatalf("baseTVTheme.WindowBG = %+v, want ANSI 4 (the default window background the audit assumes)",
			baseTVTheme.WindowBG)
	}

	def := defaultPalette()
	for _, bg := range []tui.Color{baseTVTheme.WindowBG, tui.ANSIColor(4)} {
		findings := paletteContrast(def, bg)
		for _, f := range findings {
			t.Logf("%-13s fg=%+v bg=%+v ratio=%.3f min=%.1f", f.Role, f.FG, f.BG, f.Ratio, f.Min)
			if !f.OK() {
				t.Errorf("default palette role %q fails its contrast minimum: ratio %.3f < min %.1f (on bg %+v)",
					f.Role, f.Ratio, f.Min, f.BG)
			}
		}
	}
}

// TestIssue202DefaultPaletteFindingsContract locks the audit's structure: it
// covers exactly the painted roles, checks each against its real (role-specific)
// background, and holds text roles to the body-text tier and non-text/bold roles
// to the large/bold tier. A regression that checked, say, the info role against
// the wrong background or tier would be caught here.
func TestIssue202DefaultPaletteFindingsContract(t *testing.T) {
	def := defaultPalette()
	findings := paletteContrast(def, baseTVTheme.WindowBG)

	// One finding per painted role, each against the background it really renders on.
	want := []struct {
		role string
		fg   tui.Color
		bg   tui.Color
		min  float64
	}{
		{"user", def.User, baseTVTheme.WindowBG, minContrastText},
		{"agent", def.Agent, baseTVTheme.WindowBG, minContrastText},
		{"note", def.Note, baseTVTheme.WindowBG, minContrastText},
		{"tool", def.Tool, baseTVTheme.WindowBG, minContrastText},
		{"result", def.Result, baseTVTheme.WindowBG, minContrastText},
		{"info", def.Info, baseTVTheme.WindowBG, minContrastText},
		{"error", def.Error, baseTVTheme.WindowBG, minContrastLarge}, // bold error headers
		{"desktop-hint", def.DesktopFG, def.DesktopBG, minContrastText},
		{"panel-body", def.PanelFG, def.PanelBG, minContrastText},
		{"panel-title", def.Title, def.PanelBG, minContrastText},
		{"divider", def.Divider, def.PanelBG, minContrastLarge}, // non-text border
		{"accent", def.Accent, def.PanelBG, minContrastLarge},   // non-text indicator
		// Dropdown roles (issue #260) carry their own backgrounds, so each is audited
		// against its actual fill (closed control, focused control, open-popup row).
		{"dropdown", def.DropdownFG, def.DropdownBG, minContrastText},
		{"dropdown-focus", def.DropdownFocusFG, def.DropdownFocusBG, minContrastText},
		{"dropdown-select", def.DropdownSelectFG, def.DropdownSelectBG, minContrastText},
		// Text-selection role (issue #279): a selected input cell is repainted to
		// TextSelectionFG on TextSelectionBG, audited against each other. Inserted right after
		// dropdown-select.
		{"text-selection", def.TextSelectionFG, def.TextSelectionBG, minContrastText},
		// Button/input roles (issue #265) carry their own fills. The resting button is the
		// stock white-on-green (ANSI 15/2) at 3.11:1, held to the non-text/large floor as a
		// documented gamut limit; the rest clear the body-text tier.
		{"button", def.ButtonFG, def.ButtonBG, minContrastLarge},
		{"button-focus", def.ButtonFocusFG, def.ButtonFocusBG, minContrastText},
		{"input", def.InputFG, def.InputBG, minContrastText},
		{"input-focus", def.InputFocusFG, def.InputFocusBG, minContrastText},
	}
	if len(findings) != len(want) {
		t.Fatalf("paletteContrast returned %d findings, want %d", len(findings), len(want))
	}
	for i, w := range want {
		f := findings[i]
		if f.Role != w.role {
			t.Errorf("finding[%d].Role = %q, want %q", i, f.Role, w.role)
		}
		if f.FG != w.fg {
			t.Errorf("finding %q FG = %+v, want %+v", w.role, f.FG, w.fg)
		}
		if f.BG != w.bg {
			t.Errorf("finding %q BG = %+v, want %+v (role must be checked on its real background)",
				w.role, f.BG, w.bg)
		}
		if f.Min != w.min {
			t.Errorf("finding %q Min = %g, want %g", w.role, f.Min, w.min)
		}
		// A painted foreground must be a concrete colour (the terminal default would
		// make the ratio indeterminate and the finding meaningless).
		if f.FG.Mode == tui.ColorDefault {
			t.Errorf("finding %q FG is the terminal default", w.role)
		}
	}
}

// TestIssue202DefaultPaletteEveryRoleClearsFloor asserts the stronger, uniform
// invariant the plan documents: minContrastLarge (3:1) is the floor *every* role
// clears — including the error role held to that tier — so no default role is
// below even the large/bold threshold.
func TestIssue202DefaultPaletteEveryRoleClearsFloor(t *testing.T) {
	findings := paletteContrast(defaultPalette(), baseTVTheme.WindowBG)
	for _, f := range findings {
		if f.Ratio < minContrastLarge {
			t.Errorf("default role %q ratio %.3f is below the universal floor %.1f",
				f.Role, f.Ratio, minContrastLarge)
		}
	}
}

// ----------------------------------------------------------------------------
// Group 3: the specific colour fixes.
// ----------------------------------------------------------------------------

// TestIssue202SystemNoteColourRecolouredFromBrightBlue is the direct test of the
// reported bug: the default colorInfo is no longer ANSI 12 (bright blue, the
// ~2.6:1 culprit), and the new value clears the body-text threshold on the blue
// window — and strictly beats the old colour's ratio there.
func TestIssue202SystemNoteColourRecolouredFromBrightBlue(t *testing.T) {
	def := defaultPalette()
	brightBlue := tui.ANSIColor(12)
	bg := baseTVTheme.WindowBG

	if def.Info == brightBlue {
		t.Fatalf("default Info is still ANSI 12 (the low-contrast culprit of #202)")
	}

	old := contrastRatio(brightBlue, bg) // the unreadable ~2.6:1 pairing
	got := contrastRatio(def.Info, bg)
	if got < minContrastText {
		t.Errorf("default Info ratio on window bg = %.3f, want >= %.1f (AA body text)", got, minContrastText)
	}
	if got <= old {
		t.Errorf("default Info ratio (%.3f) did not improve over the old ANSI 12 (%.3f)", got, old)
	}
}

// TestIssue202DefaultNoteRecolouredFromDimGrey checks the other low-contrast fix
// flagged in the issue: colorNote was ANSI 8 (dim grey, ~1.8:1 on blue) and is
// now light grey. The new value clears AA body text; the old one would have
// failed even the 3:1 floor.
func TestIssue202DefaultNoteRecolouredFromDimGrey(t *testing.T) {
	def := defaultPalette()
	dimGrey := tui.ANSIColor(8)
	bg := baseTVTheme.WindowBG

	if def.Note == dimGrey {
		t.Fatalf("default Note is still ANSI 8 (the ~1.8:1 dim-grey failure)")
	}
	if contrastRatio(def.Note, bg) < minContrastText {
		t.Errorf("default Note ratio on window bg = %.3f, want >= %.1f",
			contrastRatio(def.Note, bg), minContrastText)
	}
	if contrastRatio(dimGrey, bg) >= minContrastLarge {
		t.Errorf("sanity: dim grey on blue was expected to fail the 3:1 floor (the bug this recolours away from)")
	}
}

// TestIssue202DefaultDividerRecolouredFromDimGrey checks the faint-border fix:
// chromeDivider was ANSI 8 (the same ~1.8:1 dim grey) and is now light grey so
// sidebar borders stay visible on the blue panel.
func TestIssue202DefaultDividerRecolouredFromDimGrey(t *testing.T) {
	def := defaultPalette()
	dimGrey := tui.ANSIColor(8)

	if def.Divider == dimGrey {
		t.Fatalf("default Divider is still ANSI 8 (faint borders on blue)")
	}
	if contrastRatio(def.Divider, def.PanelBG) < minContrastLarge {
		t.Errorf("default Divider ratio on panel bg = %.3f, want >= %.1f",
			contrastRatio(def.Divider, def.PanelBG), minContrastLarge)
	}
}

// TestIssue202DefaultSemanticRolesDistinct holds the cohesion invariant: the
// seven transcript roles stay pairwise distinct so user vs agent vs tool vs
// result vs info vs error vs note remain distinguishable (the issue requires
// replacements to "keep the semantic roles distinguishable").
func TestIssue202DefaultSemanticRolesDistinct(t *testing.T) {
	def := defaultPalette()
	roles := []struct {
		name string
		c    tui.Color
	}{
		{"user", def.User}, {"agent", def.Agent}, {"note", def.Note}, {"tool", def.Tool},
		{"result", def.Result}, {"info", def.Info}, {"error", def.Error},
	}
	seen := make(map[tui.Color]string, len(roles))
	for _, r := range roles {
		if other, dup := seen[r.c]; dup {
			t.Errorf("default %s and %s share colour %+v; semantic roles must be distinguishable",
				other, r.name, r.c)
		}
		seen[r.c] = r.name
	}
}

// ----------------------------------------------------------------------------
// Group 4: the addNote path (the immediate bug surface).
// ----------------------------------------------------------------------------

// newMinimalSessionWindow builds a SessionWindow whose transcript is a real
// transcriptModel, enough to drive addNote/addUser without constructing the whole
// workbench. addNote touches only sw.transcript, so this is the faithful minimum.
func newMinimalSessionWindow() *SessionWindow {
	return &SessionWindow{transcript: newTranscriptModel(tv.NewTextView("", tv.Rect{}))}
}

// TestIssue202AddNotePaintsSystemNoteInColorInfo drives the real addNote path
// (the function the issue names as the root cause) and asserts the produced
// transcript record is a kindSystem note painted in colorInfo — header, body, and
// every folded child line alike — including for the exact "queued (will send…)"
// text from the bug report. It also confirms a user message is a different role
// (colorUser), so the note colour is specific rather than a global default.
func TestIssue202AddNotePaintsSystemNoteInColorInfo(t *testing.T) {
	withThemeRestore(t)

	// The default colorInfo must be readable on the window background; if it were
	// still the low-contrast ANSI 12 this is where the illegibility would show up.
	if contrastRatio(colorInfo, baseTVTheme.WindowBG) < minContrastLarge {
		t.Fatalf("colorInfo (%+v) is low-contrast (%.3f) on the window background before addNote",
			colorInfo, contrastRatio(colorInfo, baseTVTheme.WindowBG))
	}

	sw := newMinimalSessionWindow()

	// The exact note text from the issue's repro steps.
	const queuedNote = "queued (will send when the agent finishes): hello"
	sw.addNote(queuedNote)

	notes := sw.transcript.records
	if len(notes) != 1 {
		t.Fatalf("addNote produced %d records, want 1", len(notes))
	}
	rec := notes[len(notes)-1]
	if rec.kind != kindSystem {
		t.Errorf("addNote record kind = %v, want kindSystem", rec.kind)
	}
	if rec.color != colorInfo {
		t.Errorf("addNote record color = %+v, want colorInfo (%+v) — system notes must use colorInfo",
			rec.color, colorInfo)
	}
	if rec.header != "[System]" {
		t.Errorf("addNote header = %q, want %q", rec.header, "[System]")
	}
	if len(rec.lines) == 0 || rec.lines[0].text != queuedNote {
		t.Errorf("addNote body = %v, want the single line %q", rec.lines, queuedNote)
	}
	for i, ln := range rec.lines {
		if ln.color != colorInfo {
			t.Errorf("addNote child line %d color = %+v, want colorInfo", i, ln.color)
		}
	}

	// A multi-line note colours every folded child line, not just the header.
	sw2 := newMinimalSessionWindow()
	sw2.addNote("first line\nsecond line")
	multi := sw2.transcript.records[len(sw2.transcript.records)-1]
	if len(multi.lines) != 2 {
		t.Fatalf("multi-line note split into %d lines, want 2", len(multi.lines))
	}
	for i, ln := range multi.lines {
		if ln.color != colorInfo {
			t.Errorf("multi-line note child %d color = %+v, want colorInfo", i, ln.color)
		}
	}

	// Contrast with a user message: a user record uses colorUser, not colorInfo, so
	// the note colour is a deliberate per-role choice rather than a catch-all.
	sw3 := newMinimalSessionWindow()
	sw3.addUser("hi")
	if u := sw3.transcript.records[len(sw3.transcript.records)-1]; u.color != colorUser {
		t.Errorf("addUser record color = %+v, want colorUser (notes and users must differ)", u.color)
	}
}

// TestIssue202RecolouredInitialGlobalsMatchPalette guards against the exact
// regression pattern that produced #202 in the first place: #193 recoloured only
// the running status line while leaving colorInfo and its other uses (addNote,
// the ready banner, restored system messages) at the low-contrast value, so the
// bug recurred elsewhere. The package-level initial values of the three roles
// #202 recoloured must mirror defaultPalette() AND must not be the old
// low-contrast indices — so a future edit to the palette that forgets its mirror
// (or vice-versa) is caught, exactly the drift that let #193 recur as #202.
//
// It compares against the init-time captures (initColor*/initChromeDivider), not
// the live globals, so the result is independent of test execution order.
func TestIssue202RecolouredInitialGlobalsMatchPalette(t *testing.T) {
	def := defaultPalette()

	for _, c := range []struct {
		name    string
		initial tui.Color // captured at test-binary init, before any ApplyTheme
		palette tui.Color
		old     tui.Color // the pre-fix low-contrast value this role must not still be
	}{
		{"colorInfo", initColorInfo, def.Info, tui.ANSIColor(12)},           // bright blue → cyan
		{"colorNote", initColorNote, def.Note, tui.ANSIColor(8)},            // dim grey → light grey
		{"chromeDivider", initChromeDivider, def.Divider, tui.ANSIColor(8)}, // dim grey → light grey
	} {
		if c.initial != c.palette {
			t.Errorf("initial %s (%+v) != defaultPalette() value (%+v); a palette edit forgot its mirror (or vice-versa)",
				c.name, c.initial, c.palette)
		}
		if c.initial == c.old {
			t.Errorf("initial %s is still the old low-contrast %+v — the #193→#202 recurrence pattern", c.name, c.old)
		}
	}
}

// ----------------------------------------------------------------------------
// Group 5: documented edge cases / known limits (characterisation tests).
// ----------------------------------------------------------------------------

// TestIssue202NoColorAuditIsIndeterminate verifies the defensive design under
// NO_COLOR: every colour flattens to the terminal default, whose real value is
// unknowable, so every audit finding is indeterminate (ratio 0) and therefore not
// OK. The audit can never silently "pass" a pairing it cannot actually measure.
func TestIssue202NoColorAuditIsIndeterminate(t *testing.T) {
	none := ResolveTheme(config.ThemeConfig{}, noColorEnv, false)
	if none.Level != ColorNone {
		t.Fatalf("Level = %v, want ColorNone", none.Level)
	}
	findings := paletteContrast(none, baseTVTheme.WindowBG)
	if len(findings) == 0 {
		t.Fatalf("paletteContrast returned no findings under NO_COLOR")
	}
	for _, f := range findings {
		if f.Ratio != 0 {
			t.Errorf("NO_COLOR finding %q ratio = %g, want 0 (indeterminate)", f.Role, f.Ratio)
		}
		if f.OK() {
			t.Errorf("NO_COLOR finding %q unexpectedly OK; an indeterminate pair must not pass", f.Role)
		}
	}
}

// TestIssue202DarkPalettePassesAuditOnBlack confirms the dark (black-background)
// preset passes its own contrast audit on its black canvas — so the readability
// guarantee holds beyond the default theme, including its marginal divider.
func TestIssue202DarkPalettePassesAuditOnBlack(t *testing.T) {
	dark := darkPalette()
	findings := paletteContrast(dark, dark.PanelBG) // window bg == black for the dark canvas
	for _, f := range findings {
		t.Logf("dark %-13s ratio=%6.3f min=%.1f", f.Role, f.Ratio, f.Min)
		if !f.OK() {
			t.Errorf("dark palette role %q fails: ratio %.3f < min %.1f", f.Role, f.Ratio, f.Min)
		}
	}
}

// TestIssue202HighContrastInfoBelowAABodyText documents a REAL defect the audit
// found, pinned as a characterisation test. The high-contrast preset is the
// accessibility theme, yet its system-note colour — Okabe–Ito blue (#0072B2),
// the same role #202 recoloured in the default palette — reaches only ~4.05:1 on
// its black background, below the 4.5:1 AA body-text target that paletteContrast
// holds the info role to. (It does clear the 3:1 large/bold floor.) It is out of
// scope for #202, which fixed the default palette only, so this test pins the
// current behaviour: when someone lifts the high-contrast info colour to AA, the
// "!OK" assertion below will flip and this test will fail — signalling the fix.
func TestIssue202HighContrastInfoBelowAABodyText(t *testing.T) {
	hc := highContrastPalette()
	findings := paletteContrast(hc, hc.PanelBG) // window bg == black for the HC canvas

	info := findingByRole(t, findings, "info")
	t.Logf("high-contrast info (%+v) on black: ratio=%.3f, min=%.1f", info.FG, info.Ratio, info.Min)

	// Today the high-contrast info role is below the AA body-text target…
	if info.Ratio >= minContrastText {
		t.Errorf("high-contrast info now meets AA body text (ratio %.3f >= %.1f) — great, flip this characterisation test",
			info.Ratio, minContrastText)
	}
	// …but it still clears the large/bold floor, so it is a marginal miss, not a
	// severe one.
	if info.Ratio < minContrastLarge {
		t.Errorf("high-contrast info ratio %.3f is below even the 3:1 floor", info.Ratio)
	}

	// Every OTHER high-contrast role must pass — the info colour is the only gap.
	for _, f := range findings {
		if f.Role == "info" {
			continue
		}
		if !f.OK() {
			t.Errorf("unexpected high-contrast failure beyond info: %q ratio %.3f < min %.1f",
				f.Role, f.Ratio, f.Min)
		}
	}
}

// TestIssue202DefaultErrorMarginalOnBlue documents the one default-palette role
// that sits between the two tiers: the bright-red error colour (ANSI 9) reaches
// ~4.23:1 on the blue window — clear of the 3:1 large/bold floor (error headers
// are bold) but short of the 4.5:1 body-text target, because no redder hue exists
// in the 16-colour gamut. The audit therefore holds it to minContrastLarge. This
// pins the documented compromise so a future change is deliberate.
func TestIssue202DefaultErrorMarginalOnBlue(t *testing.T) {
	err := findingByRole(t, paletteContrast(defaultPalette(), baseTVTheme.WindowBG), "error")
	t.Logf("default error (%+v) on blue: ratio=%.3f (floor=%.1f, body=%.1f)", err.FG, err.Ratio, minContrastLarge, minContrastText)

	if !err.OK() {
		t.Errorf("default error must still clear its 3:1 tier; ratio %.3f", err.Ratio)
	}
	if err.Ratio >= minContrastText {
		t.Errorf("default error now meets AA body text (ratio %.3f) — update this characterisation test", err.Ratio)
	}
	if err.Min != minContrastLarge {
		t.Errorf("default error tier = %g, want minContrastLarge (the documented compromise)", err.Min)
	}
}

// TestIssue202DarkDividerNearFloor documents that the dark theme's divider
// (#5A5A5A on black) is the most marginal passing pairing in any built-in
// palette: ~3.05:1, barely above the 3:1 floor. It passes today; this pins how
// close it sits so a nudge to that colour is caught before it drops below.
func TestIssue202DarkDividerNearFloor(t *testing.T) {
	div := findingByRole(t, paletteContrast(darkPalette(), darkPalette().PanelBG), "divider")
	t.Logf("dark divider on black: ratio=%.3f (floor=%.1f)", div.Ratio, minContrastLarge)

	if !div.OK() {
		t.Errorf("dark divider must clear the 3:1 floor; ratio %.3f", div.Ratio)
	}
	if div.Ratio >= 3.5 {
		t.Errorf("dark divider ratio %.3f is no longer marginal (>= 3.5); update this characterisation test", div.Ratio)
	}
}

// TestIssue202OldBrightBlueLowContrastReproduced pins the exact value of the bug
// class: bright blue (ANSI 12, the pre-fix colorInfo) on the blue window is the
// ~2.6:1 pairing that made system notes unreadable. Keeping this assertion means a
// revert to ANSI 12 would be caught by a concrete, measured failure, not just the
// "not ANSI 12" check.
func TestIssue202OldBrightBlueLowContrastReproduced(t *testing.T) {
	bg := baseTVTheme.WindowBG
	r := contrastRatio(tui.ANSIColor(12), bg)
	t.Logf("old colorInfo (ANSI 12) on blue window: ratio=%.3f", r)
	if r >= minContrastLarge {
		t.Errorf("ANSI 12 on blue = %.3f, expected the sub-3:1 unreadable pairing of the original bug", r)
	}
}

// ----------------------------------------------------------------------------
// Group 6: the end-to-end repro and the unaudited error/dialog surfaces.
//
// These close the gaps a self-review of the suite turned up: the issue's literal
// repro (queue a message while busy) was only exercised at the addNote level, the
// sub-AA error-body-text pairing was documented in prose but not encoded, and the
// dialog-accent colours were never audited at all.
// ----------------------------------------------------------------------------

// recordContaining returns the first transcript record whose header or body
// mentions sub, or nil if none does. It finds the note/error an action produced so
// its painted colour can be asserted.
func recordContaining(sw *SessionWindow, sub string) *transcriptRecord {
	for _, r := range sw.transcript.records {
		if strings.Contains(r.header, sub) {
			return r
		}
		for _, l := range r.lines {
			if strings.Contains(l.text, sub) {
				return r
			}
		}
	}
	return nil
}

// TestIssue202QueuedNoteReproEndToEnd reproduces the issue's exact scenario
// through the real submit path — send a turn, then type another while busy — and
// asserts the echoed "queued (will send…)" note is painted colorInfo and reads on
// the window background. This is the end-to-end counterpart of the group-4 addNote
// test: it proves the busy→enqueue routing (with injection disabled) really reaches
// addNote carrying the recoloured system-note colour, not just that addNote would
// use it if called.
func TestIssue202QueuedNoteReproEndToEnd(t *testing.T) {
	withThemeRestore(t)

	w := newTestWorkbench(t)
	sent := recordSends(w)
	sw := w.openWindow("s", "S")

	// Step 1 of the repro: a first message dispatches and flips the window busy.
	sw.input.SetText("first")
	sw.submitFn()
	if got := waitSend(t, sent); got != "first" {
		t.Fatalf("first send = %q, want %q", got, "first")
	}
	if !sw.busy {
		t.Fatal("window should be busy after the first submit (repro preconditions)")
	}

	// Steps 2–3: a second message while busy is queued, not sent, and echoed as a
	// system note carrying the recoloured, readable system-note colour.
	sw.input.SetText("queued-msg")
	sw.submitFn()
	noSend(t, sent)

	rec := recordContaining(sw, "queued (will send")
	if rec == nil {
		t.Fatal("no 'queued (will send…)' note was echoed; the repro did not reach addNote")
	}
	if rec.color != colorInfo {
		t.Errorf("queued note colour = %+v, want colorInfo (the recoloured system-note role)", rec.color)
	}
	if r := contrastRatio(rec.color, baseTVTheme.WindowBG); r < minContrastText {
		t.Errorf("queued note colour ratio on window bg = %.3f, want >= %.1f (the #202 fix)", r, minContrastText)
	}
}

// TestIssue202ErrorBodyTextBelowAABodyTier encodes the most material gap the audit
// leaves open. colorError (ANSI 9) reaches only ~4.23:1 on the blue window, below
// the 4.5:1 AA body-text target, yet paletteContrast holds the error role to the
// 3:1 large/bold tier on the stated assumption that "error headers are bold". But
// colorError is ALSO painted on non-bold body text: this test drives the real
// budget-exceeded alert and shows its body line is colorError. So a genuine
// sub-AA body-text pairing is certified as passing. The test pins that fact; when
// error is recoloured or moved to the body-text tier, the ratio assertion flips.
func TestIssue202ErrorBodyTextBelowAABodyTier(t *testing.T) {
	withThemeRestore(t)

	w := newTestWorkbench(t)
	sw := w.openWindow("s", "S")

	// Drive the budget-exceeded alert directly: cumulative tokens over the budget,
	// latch unset. alertBudgetIfNewlyExceeded appends a kindSystem record whose
	// header AND body lines are colorError (session_window.go).
	sw.statusStats = agent.SessionStats{TokensIn: 600, TokensOut: 500}
	sw.alertBudgetIfNewlyExceeded(config.BudgetConfig{TokenBudget: 1000})

	rec := recordContaining(sw, "token budget exceeded")
	if rec == nil {
		t.Fatal("budget-exceeded note was not added by alertBudgetIfNewlyExceeded")
	}
	if rec.color != colorError {
		t.Errorf("budget note header colour = %+v, want colorError", rec.color)
	}
	if len(rec.lines) == 0 {
		t.Fatal("budget note has no body lines; the sub-AA body-text case is what this test targets")
	}
	if rec.lines[0].color != colorError {
		t.Errorf("budget note body colour = %+v, want colorError (non-bold body text)", rec.lines[0].color)
	}

	// colorError on the window background is below AA body text — the gap this test
	// exists to make visible. It still clears the 3:1 floor the audit holds it to.
	r := contrastRatio(colorError, baseTVTheme.WindowBG)
	t.Logf("colorError body text on window bg: ratio=%.3f (body target=%.1f, audit tier=%.1f)",
		r, minContrastText, minContrastLarge)
	if r >= minContrastText {
		t.Errorf("colorError now meets AA body text (%.3f); the gap closed — update this characterisation test", r)
	}
	if r < minContrastLarge {
		t.Errorf("colorError ratio %.3f is below even the 3:1 floor", r)
	}
}

// TestIssue202DialogAccentsUnaudited documents that paletteContrast does not cover
// the dialog-accent colours (colorDialogHeader / colorDialogDetail), even though
// they are palette-level vars rendered on a distinct background — the light-grey
// dialog chrome (DialogBG, ANSI 7) — from anything the audit checks, and it pins
// the real defect that gap hides.
//
// colorDialogHeader (ANSI 5 magenta) is the permission-dialog requester line
// (permission_dialog.go: r.FG = colorDialogHeader), painted on DialogBG. Magenta
// on light grey reaches only ~2.75:1 — below even the 3:1 floor and well under AA
// body text. That is precisely the low-contrast class #202 is about, and the audit
// misses it because dialog accents are absent from paletteContrast. This test pins
// the defect: it passes today and will flip (signalling a fix) when the header is
// recoloured or the accents are brought into the audit.
func TestIssue202DialogAccentsUnaudited(t *testing.T) {
	withThemeRestore(t)

	// The dialog accents are not among the audited roles.
	findings := paletteContrast(defaultPalette(), baseTVTheme.WindowBG)
	audited := make(map[string]bool, len(findings))
	for _, f := range findings {
		audited[f.Role] = true
	}
	if audited["dialog-header"] || audited["dialog-detail"] {
		t.Errorf("dialog accents are now audited by paletteContrast — update this characterisation test")
	}

	// Stock turbotui paints dialogs on ANSI 7 (light grey); confirm that assumption.
	if baseTVTheme.DialogBG != tui.ANSIColor(7) {
		t.Fatalf("baseTVTheme.DialogBG = %+v, want ANSI 7 (the light-grey dialog chrome)", baseTVTheme.DialogBG)
	}

	// colorDialogDetail (blue on light grey) reads fine.
	detail := contrastRatio(colorDialogDetail, baseTVTheme.DialogBG)
	t.Logf("colorDialogDetail (%+v) on DialogBG: ratio=%.3f", colorDialogDetail, detail)
	if detail < minContrastText {
		t.Errorf("colorDialogDetail on DialogBG = %.3f, below AA body text", detail)
	}

	// colorDialogHeader (magenta on light grey) is the defect: ~2.75:1.
	header := contrastRatio(colorDialogHeader, baseTVTheme.DialogBG)
	t.Logf("colorDialogHeader (%+v) on DialogBG: ratio=%.3f  [DEFECT: below the 3:1 floor, unaudited]",
		colorDialogHeader, header)
	if header >= minContrastLarge {
		t.Errorf("colorDialogHeader on DialogBG = %.3f; the low-contrast defect appears fixed — flip this characterisation test",
			header)
	}
}

// ----------------------------------------------------------------------------
// Group 7: contrast at the terminal's actual colour fidelity (degradation).
//
// paletteContrast is colour-level-agnostic, so the readability guarantee must hold
// at the fidelity a user's terminal really reports — not only truecolor. ResolveTheme
// degrades the RGB high-contrast and dark palettes down to the 16-colour baseline
// (the default for TERM=xterm, common over SSH and on basic terminals), and
// quantisation can land a colour on a different, lower-contrast ANSI index. The
// default palette is authored entirely in 16-colour ANSI indices, so it is
// fidelity-invariant; the RGB presets are not, and at 16 colours several of their
// roles drop below threshold.
// ----------------------------------------------------------------------------

// TestIssue202DefaultPalettePassesAtAllFidelities is the strong positive guarantee
// for the in-scope palette: the default theme is all 16-colour ANSI, so degrading
// to 256 or 16 colours leaves every role unchanged and the audit still passes at
// every fidelity a terminal can report. (The contrast ratio of an ANSI index does
// not change when it is "degraded" back to ANSI.)
func TestIssue202DefaultPalettePassesAtAllFidelities(t *testing.T) {
	for _, level := range []struct {
		name string
		env  func(string) string
	}{
		{"Color16", color16Env},
		{"Color256", color256Env},
		{"ColorTrue", truecolorEnv},
	} {
		t.Run(level.name, func(t *testing.T) {
			th := ResolveTheme(config.ThemeConfig{}, level.env, false)
			// The default window background is ANSI 4 and passes through degrade
			// unchanged, so it is the same surface at every fidelity.
			for _, f := range paletteContrast(th, baseTVTheme.WindowBG) {
				if !f.OK() {
					t.Errorf("%s: default role %q fails its minimum: ratio %.3f < min %.1f",
						level.name, f.Role, f.Ratio, f.Min)
				}
			}
		})
	}
}

// TestIssue202HighContrastToolDegradesBelowAAAtColor16 documents a real defect: the
// high-contrast (accessibility) tool colour (Okabe orange) reads at ~9.3:1 in
// truecolor but quantises to ANSI 3 on a 16-colour terminal, dropping to ~4.0:1 on
// black — below the 4.5 AA body-text target. paletteContrast on the raw palette
// never sees this; only the degraded theme reveals it. Pinned as a characterisation
// test: it passes today and flips when the HC palette is adjusted to survive
// 16-colour degradation.
func TestIssue202HighContrastToolDegradesBelowAAAtColor16(t *testing.T) {
	hc16 := ResolveTheme(config.ThemeConfig{Name: "high-contrast"}, color16Env, false)
	r := contrastRatio(hc16.Tool, hc16.PanelBG)
	t.Logf("high-contrast tool at Color16: %+v on black, ratio=%.3f (body target=%.1f)", hc16.Tool, r, minContrastText)
	if r >= minContrastText {
		t.Errorf("HC tool now meets AA at Color16 (%.3f) — flip this characterisation test", r)
	}
	if r < minContrastLarge {
		t.Errorf("HC tool ratio %.3f fell below the 3:1 floor at Color16", r)
	}
}

// TestIssue202DarkInfoDegradesBelowAAAtColor16 documents that the dark theme's info
// colour (a muted periwinkle) degrades to ANSI 12 (bright blue) at 16 colours —
// ~4.1:1 on black, below AA body text, and the same bright-blue low-contrast class
// issue #202 recoloured away in the default palette.
func TestIssue202DarkInfoDegradesBelowAAAtColor16(t *testing.T) {
	dark16 := ResolveTheme(config.ThemeConfig{Name: "dark"}, color16Env, false)
	r := contrastRatio(dark16.Info, dark16.PanelBG)
	t.Logf("dark info at Color16: %+v on black, ratio=%.3f (body target=%.1f)", dark16.Info, r, minContrastText)
	if r >= minContrastText {
		t.Errorf("dark info now meets AA at Color16 (%.3f) — flip this characterisation test", r)
	}
	if r < minContrastLarge {
		t.Errorf("dark info ratio %.3f fell below the 3:1 floor at Color16", r)
	}
}

// TestIssue202DarkDividerDegradesBelowFloorAtColor16 is the most severe degradation
// finding: the dark divider (#5A5A5A) quantises to ANSI 8 (dim grey) at 16 colours,
// ~2.8:1 on black — BELOW even the 3:1 large/bold floor it is held to. The raw RGB
// divider passes at 3.045:1 (what TestIssue202DarkDividerNearFloor pins), but that
// is not what a 16-colour terminal renders. Pinned; flips when fixed.
func TestIssue202DarkDividerDegradesBelowFloorAtColor16(t *testing.T) {
	dark16 := ResolveTheme(config.ThemeConfig{Name: "dark"}, color16Env, false)
	if dark16.Divider != tui.ANSIColor(8) {
		t.Logf("note: dark divider at Color16 = %+v (expected the dim-grey ANSI 8 quantisation)", dark16.Divider)
	}
	r := contrastRatio(dark16.Divider, dark16.PanelBG)
	t.Logf("dark divider at Color16: %+v on black, ratio=%.3f (floor=%.1f)", dark16.Divider, r, minContrastLarge)
	if r >= minContrastLarge {
		t.Errorf("dark divider now clears the floor at Color16 (%.3f) — flip this characterisation test", r)
	}
}
