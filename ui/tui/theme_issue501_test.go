package ui

import (
	"testing"

	"gogent/internal/config"

	tui "github.com/hobbestherat/turbotui"
	tv "github.com/hobbestherat/turbotui/turbotv"
)

// Theme-wiring coverage for issue #501 (the paste-chip). turbotui's
// MultiLineInput/TextBox read activeTheme.PasteChipFG/PasteChipBG at draw time
// to paint the collapsed "[pasted N lines]" chip; gogent's only production
// change is to expose those two roles through the full gogent theme pipeline
// (struct field → per-palette value → ResolveTheme degrade → applyOverrides →
// ApplyTheme install). These tests pin each link of that pipeline and mirror the
// #279 text-selection suite (the closest precedent role), then add a 16-colour
// legibility probe that the design only reasoned about.

// TestPasteChipDefaultsDistinctAndReadable pins the goal/usability gate for every
// palette: the chip background is a *distinct* muted accent — not the focused-input
// fill (InputFocusBG) and not the selection box (TextSelectionBG), so a chip reads
// as a chip and a *selected* chip stays legible — and its fg/bg clear the WCAG AA
// body-text tier at full fidelity. Mirrors TestRoles279DefaultsInvertInputFocusFill.
func TestPasteChipDefaultsDistinctAndReadable(t *testing.T) {
	for _, name := range allPalettes {
		p := paletteByName(name)
		if p.PasteChipBG == p.InputFocusBG {
			t.Errorf("%s: PasteChipBG == InputFocusBG (%+v) — chip would vanish on the focused input fill", name, p.PasteChipBG)
		}
		if p.PasteChipBG == p.TextSelectionBG {
			t.Errorf("%s: PasteChipBG == TextSelectionBG (%+v) — a selected chip could not be told from a selection", name, p.PasteChipBG)
		}
		if p.PasteChipFG == p.PasteChipBG {
			t.Errorf("%s: PasteChipFG == PasteChipBG (%+v) — chip text invisible", name, p.PasteChipBG)
		}
		if r := contrastRatio(p.PasteChipFG, p.PasteChipBG); r < minContrastText {
			t.Errorf("%s: paste-chip contrast %.2f < %.1f (fg=%+v bg=%+v)", name, r, minContrastText, p.PasteChipFG, p.PasteChipBG)
		}
	}
}

// TestPasteChipApplyThemeInstalls verifies ApplyTheme propagates the roles onto the
// shared tv.DefaultTheme in every palette — the one seam turbotui's widgets read.
// Mirrors TestRoles279_291ApplyThemeInstalls.
func TestPasteChipApplyThemeInstalls(t *testing.T) {
	withThemeRestore(t)
	for _, name := range allPalettes {
		th := ResolveTheme(config.ThemeConfig{Name: name}, truecolorEnv, false)
		ApplyTheme(th)
		if tv.DefaultTheme.PasteChipFG != th.PasteChipFG || tv.DefaultTheme.PasteChipBG != th.PasteChipBG {
			t.Errorf("%s: PasteChip* not installed onto tv.DefaultTheme (got fg=%+v bg=%+v)", name,
				tv.DefaultTheme.PasteChipFG, tv.DefaultTheme.PasteChipBG)
		}
		// The active theme (what widgets actually read at draw time) must move in lockstep.
		if got := tv.ActiveTheme(); got.PasteChipFG != th.PasteChipFG || got.PasteChipBG != th.PasteChipBG {
			t.Errorf("%s: tv.ActiveTheme() PasteChip* not propagated by SetTheme (got fg=%+v bg=%+v)", name,
				got.PasteChipFG, got.PasteChipBG)
		}
	}
}

// TestPasteChipNoColorDegrades pins the NO_COLOR behaviour: like every other role,
// both chip colours degrade to the terminal default (NO_COLOR forbids emitting
// colour). Mirrors TestRoles279_291NoColorDegrades.
func TestPasteChipNoColorDegrades(t *testing.T) {
	th := ResolveTheme(config.ThemeConfig{}, noColorEnv, false)
	d := tui.DefaultColor()
	if th.PasteChipFG != d {
		t.Errorf("under NO_COLOR PasteChipFG = %+v, want terminal default", th.PasteChipFG)
	}
	if th.PasteChipBG != d {
		t.Errorf("under NO_COLOR PasteChipBG = %+v, want terminal default", th.PasteChipBG)
	}
}

// TestPasteChipContrastSurvives16ColorDegrade is the adversarial probe: the design
// claims the two truecolor chip picks (Okabe purple in high-contrast, mauve in dark)
// both quantise toward light grey at Color16 so a black foreground stays legible
// (~9:1). Verify that for real — at a 16-colour terminal the resolved chip must
// STILL clear the body-text contrast tier, or the chip is illegible on the most
// common terminal class. (At Color16 neither role is the terminal default, so
// contrastRatio returns a real number rather than the indeterminate 0.)
func TestPasteChipContrastSurvives16ColorDegrade(t *testing.T) {
	for _, name := range allPalettes {
		th := ResolveTheme(config.ThemeConfig{Name: name}, color16Env, false)
		if th.PasteChipFG == tui.DefaultColor() || th.PasteChipBG == tui.DefaultColor() {
			t.Errorf("%s @Color16: PasteChip* degraded to terminal default unexpectedly", name)
		}
		if r := contrastRatio(th.PasteChipFG, th.PasteChipBG); r < minContrastText {
			t.Errorf("%s @Color16: paste-chip contrast %.2f < %.1f after degrade (fg=%+v bg=%+v) — illegible on a 16-colour terminal",
				name, r, minContrastText, th.PasteChipFG, th.PasteChipBG)
		}
	}
}

// TestPasteChipConfigOverrideReachesWidget drives the real config-override path:
// a hand-written config [theme] overrides = {paste_chip_fg=…, paste_chip_bg=…} is
// parsed into config.ThemeConfig.Overrides, applied by applyOverrides inside
// ResolveTheme, and then propagated onto tv.DefaultTheme by ApplyTheme — the full
// chain a user configures. (The roles are intentionally NOT in the scrolling
// editor's themeRoles — like button_focus_*/input_focus_* — so buildThemeConfig
// does not record them; the editor Save seam is therefore the wrong seam to test,
// and the config-file → applyOverrides path here is the one the acceptance criteria
// mean by "overridable".)
func TestPasteChipConfigOverrideReachesWidget(t *testing.T) {
	withThemeRestore(t)
	wantFG := tui.RGBColor(0x11, 0x22, 0x33)
	wantBG := tui.ANSIColor(4)
	th := ResolveTheme(config.ThemeConfig{
		Name: themeDefault,
		Overrides: map[string]string{
			"paste_chip_fg": "#112233",
			"paste_chip_bg": "4",
		},
	}, truecolorEnv, false)

	if th.PasteChipFG != wantFG {
		t.Errorf("applyOverrides: PasteChipFG = %+v, want %+v (override not applied)", th.PasteChipFG, wantFG)
	}
	if th.PasteChipBG != wantBG {
		t.Errorf("applyOverrides: PasteChipBG = %+v, want %+v (override not applied)", th.PasteChipBG, wantBG)
	}
	ApplyTheme(th)
	if tv.DefaultTheme.PasteChipFG != wantFG || tv.DefaultTheme.PasteChipBG != wantBG {
		t.Errorf("ApplyTheme did not propagate overridden chip colours onto tv.DefaultTheme (got fg=%+v bg=%+v)",
			tv.DefaultTheme.PasteChipFG, tv.DefaultTheme.PasteChipBG)
	}
}

// TestPasteChipResolveThemeDegradeWired guards the degrade link: at Color256 the
// truecolor chip picks must have been quantised away from 24-bit raw (Mode !=
// ColorTrue), proving the ResolveTheme degrade lines were placed — without them an
// RGB chip would be emitted raw to a 256-/16-colour terminal. The default palette
// is 16-colour ANSI natively, so only the RGB-bearing presets are asserted.
func TestPasteChipResolveThemeDegradeWired(t *testing.T) {
	for _, name := range []string{themeHighContrast, themeDark} {
		raw := paletteByName(name).PasteChipBG
		if raw.Mode == tui.ColorANSI {
			continue // only the RGB picks are interesting here
		}
		got := ResolveTheme(config.ThemeConfig{Name: name}, envOf(map[string]string{"TERM": "xterm-256color"}), false).PasteChipBG
		if got == raw {
			t.Errorf("%s @Color256: PasteChipBG was not degraded (still raw %+v) — ResolveTheme degrade missing", name, raw)
		}
	}
}
