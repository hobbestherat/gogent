package ui

import (
	"testing"

	"gogent/internal/config"

	tui "github.com/hobbestherat/turbotui"
	tv "github.com/hobbestherat/turbotui/turbotv"
)

// New coverage for the #279 (text-selection) and #291 (window) theme roles and for the
// scrolling theme editor — complements the updated #202/#243/#265/#267 suites by exercising
// the role pipeline end-to-end and proving a below-the-fold role is reachable by scrolling.

var allPalettes = []string{themeDefault, themeHighContrast, themeDark}

// TestRoles279DefaultsInvertInputFocusFill pins the #279 design: in every palette the text
// selection inverts the focused-input fill (a clear luminance gap), so it is never the same
// colour as the field it is drawn over — and its own fg/bg clear the body-text contrast tier.
func TestRoles279DefaultsInvertInputFocusFill(t *testing.T) {
	for _, name := range allPalettes {
		p := paletteByName(name)
		if p.TextSelectionBG == p.InputFocusBG {
			t.Errorf("%s: TextSelectionBG == InputFocusBG (%+v) — selection invisible on a focused input", name, p.TextSelectionBG)
		}
		if r := contrastRatio(p.TextSelectionFG, p.TextSelectionBG); r < minContrastText {
			t.Errorf("%s: text selection contrast %.2f < %.1f", name, r, minContrastText)
		}
	}
}

// TestRoles291WindowDefaultUnchanged pins #291's "appearance unchanged" requirement: the
// default window background stays ANSI 4 (matching baseTVTheme), so the look is identical —
// the role only makes it editable.
func TestRoles291WindowDefaultUnchanged(t *testing.T) {
	if got := defaultPalette().WindowBG; got != tui.ANSIColor(4) {
		t.Errorf("default WindowBG = %+v, want ANSI 4 (unchanged look)", got)
	}
	if got := defaultPalette().WindowBG; got != baseTVTheme.WindowBG {
		t.Errorf("default WindowBG %+v != baseTVTheme.WindowBG %+v", got, baseTVTheme.WindowBG)
	}
}

// TestRoles279_291Contrast pins the audit additions: the text-selection finding is present,
// pairs TextSelectionFG on TextSelectionBG at the body-text tier, and passes in every palette.
func TestRoles279_291Contrast(t *testing.T) {
	for _, name := range allPalettes {
		p := paletteByName(name)
		var f *contrastFinding
		for _, c := range paletteContrast(p, p.WindowBG) {
			if c.Role == "text-selection" {
				cc := c
				f = &cc
			}
		}
		if f == nil {
			t.Fatalf("%s: no text-selection contrast finding", name)
		}
		if f.FG != p.TextSelectionFG || f.BG != p.TextSelectionBG || f.Min != minContrastText {
			t.Errorf("%s: text-selection finding mis-paired: %+v", name, *f)
		}
		if !f.OK() {
			t.Errorf("%s: text-selection contrast %.2f < %.1f", name, f.Ratio, f.Min)
		}
	}
}

// TestRoles279_291ApplyThemeInstalls verifies ApplyTheme installs the new roles onto the
// shared tv.DefaultTheme in every palette, and that the dropdown/menu Selection* slots stay
// driven by the DropdownSelect* roles (unchanged — the #279 fix is exactly that they are now
// separate from text selection).
func TestRoles279_291ApplyThemeInstalls(t *testing.T) {
	withThemeRestore(t)
	for _, name := range allPalettes {
		th := ResolveTheme(config.ThemeConfig{Name: name}, truecolorEnv, false)
		ApplyTheme(th)
		if tv.DefaultTheme.TextSelectionFG != th.TextSelectionFG || tv.DefaultTheme.TextSelectionBG != th.TextSelectionBG {
			t.Errorf("%s: TextSelection* not installed onto tv.DefaultTheme", name)
		}
		if tv.DefaultTheme.WindowFG != th.WindowFG || tv.DefaultTheme.WindowBG != th.WindowBG {
			t.Errorf("%s: Window* not installed onto tv.DefaultTheme", name)
		}
		if tv.DefaultTheme.SelectionFG != th.DropdownSelectFG || tv.DefaultTheme.SelectionBG != th.DropdownSelectBG {
			t.Errorf("%s: dropdown/menu Selection* must stay driven by DropdownSelect*", name)
		}
	}
}

// TestRoles279_291NoColorDegrades documents the NO_COLOR behaviour: every new role degrades
// to the terminal default (NO_COLOR forbids emitting colour), which is also why a selection
// is not colour-distinguishable under NO_COLOR — a documented toolkit limitation (tui.Cell
// has no reverse attribute), not a regression.
func TestRoles279_291NoColorDegrades(t *testing.T) {
	th := ResolveTheme(config.ThemeConfig{}, noColorEnv, false)
	d := tui.DefaultColor()
	for _, c := range []struct {
		name string
		c    tui.Color
	}{
		{"TextSelectionFG", th.TextSelectionFG}, {"TextSelectionBG", th.TextSelectionBG},
		{"WindowFG", th.WindowFG}, {"WindowBG", th.WindowBG},
	} {
		if c.c != d {
			t.Errorf("under NO_COLOR %s = %+v, want terminal default", c.name, c.c)
		}
	}
}

// TestRoles279_291OverrideRoundTrip drives the editor's Save/Reopen seam: an override for
// each new key is recorded by buildThemeConfig and re-applied by editedTheme on reopen.
func TestRoles279_291OverrideRoundTrip(t *testing.T) {
	cases := map[string]struct {
		spec string
		want tui.Color
		get  func(Theme) tui.Color
	}{
		"window_bg":         {"#123456", tui.RGBColor(0x12, 0x34, 0x56), func(t Theme) tui.Color { return t.WindowBG }},
		"window_fg":         {"5", tui.ANSIColor(5), func(t Theme) tui.Color { return t.WindowFG }},
		"text_selection_fg": {"#ABCDEF", tui.RGBColor(0xAB, 0xCD, 0xEF), func(t Theme) tui.Color { return t.TextSelectionFG }},
		"text_selection_bg": {"3", tui.ANSIColor(3), func(t Theme) tui.Color { return t.TextSelectionBG }},
	}
	specs := map[string]string{}
	for _, r := range themeRoles {
		specs[r.key] = colorSpec(r.get(paletteByName(themeDefault)))
	}
	for key, c := range cases {
		specs[key] = c.spec
	}
	cfg := buildThemeConfig(themeDefault, false, false, specs)
	reopened := editedTheme(cfg)
	for key, c := range cases {
		if cfg.Overrides[key] != c.spec {
			t.Errorf("override %q not recorded by buildThemeConfig (got %q)", key, cfg.Overrides[key])
		}
		if got := c.get(reopened); got != c.want {
			t.Errorf("override %q did not round-trip through editedTheme: got %+v, want %+v", key, got, c.want)
		}
	}
}

// TestScrollRevealsBelowFold proves the scrolling editor reaches a role below the initial
// fold: code_bg (the last right-column role) is absent at scrollY=0 but becomes visible after
// scrolling — the core capacity fix that lets the editor hold the new roles without growing.
func TestScrollRevealsBelowFold(t *testing.T) {
	issue204RestoreTheme(t)
	w := newTestWorkbench(t)
	w.SetHandlers(Handlers{
		GetTheme: func() config.ThemeConfig { return config.ThemeConfig{} },
		SetTheme: func(config.ThemeConfig) {},
	})
	w.showThemeEditor()

	if themeEditorMaxScroll() == 0 {
		t.Skip("content fits without scrolling — nothing below the fold to reveal")
	}
	if containsOnScreen(screenText(w), "Code block background:") {
		t.Fatalf("precondition: code_bg should be below the initial fold, but it is visible at scrollY=0")
	}
	if !scrollEditorToReveal(w, "Code block background:") {
		t.Errorf("code_bg never became visible after scrolling — the below-the-fold role is unreachable")
	}
}
