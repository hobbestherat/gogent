package ui

import (
	"reflect"
	"testing"

	"gogent/internal/config"

	tui "github.com/hobbestherat/turbotui"
	tv "github.com/hobbestherat/turbotui/turbotv"
)

// This file exercises issue #200: the two theme-ignoring regions (the fenced-code
// background and the sidebar/Overall panel background) now derive from the active
// theme instead of a hardcoded black, and a new built-in "dark" (black-background)
// theme was added. The tests cover the normal behaviour, edge cases (degradation
// ladders, NO_COLOR, aliases), error handling (bad overrides) and a few
// invariants that would catch a regression where a palette change forgets its
// mirror in the package-level initial values or the markdown palette.

// initChrome* / initMd* capture the package-level colour globals at test-binary
// init time — the built-in "default" palette baked into theme.go / markdown.go,
// before any test mutates them. Asserting against these (rather than the live
// globals) makes the "initial values match defaultPalette()" check independent of
// test execution order.
var (
	initChromePanelBG   = chromePanelBG
	initChromeDesktopBG = chromeDesktopBG
	initMdCodeBG        = mdPalette.codeBG
	initMdHasCodeBG     = mdPalette.hasCodeBG
)

// Pre-built env accessors for the three colour fidelities (plus NO_COLOR), reused
// across the ResolveTheme / ApplyTheme tests.
var (
	truecolorEnv = envOf(map[string]string{"TERM": "xterm", "COLORTERM": "truecolor"})
	color256Env  = envOf(map[string]string{"TERM": "xterm-256color"})
	color16Env   = envOf(map[string]string{"TERM": "xterm"})
	noColorEnv   = envOf(map[string]string{"NO_COLOR": "1", "TERM": "xterm"})
)

// savedThemeGlobals captures every package global ApplyTheme mutates so a test can
// restore the exact prior state. snapshotColors/restoreColors (defined in
// theme_test.go) cover the nine semantic + dialog-accent colours; the rest are
// the chrome colours, the markdown palette and the shared turbotui theme.
type savedThemeGlobals struct {
	sem                  [9]tui.Color
	desktopFG, desktopBG tui.Color
	panelFG, panelBG     tui.Color
	title, divider       tui.Color
	accent               tui.Color
	pal                  markdownPalette
	palGen               uint64
	richOK               bool
	tvTheme              tv.Theme
}

func snapshotThemeGlobals() savedThemeGlobals {
	return savedThemeGlobals{
		sem:       snapshotColors(),
		desktopFG: chromeDesktopFG,
		desktopBG: chromeDesktopBG,
		panelFG:   chromePanelFG,
		panelBG:   chromePanelBG,
		title:     chromeTitle,
		divider:   chromeDivider,
		accent:    chromeAccent,
		pal:       mdPalette,
		palGen:    mdPaletteGen,
		richOK:    richMarkdownColorOK,
		tvTheme:   tv.DefaultTheme,
	}
}

func restoreThemeGlobals(g savedThemeGlobals) {
	restoreColors(g.sem)
	chromeDesktopFG, chromeDesktopBG = g.desktopFG, g.desktopBG
	chromePanelFG, chromePanelBG = g.panelFG, g.panelBG
	chromeTitle, chromeDivider, chromeAccent = g.title, g.divider, g.accent
	mdPalette, mdPaletteGen, richMarkdownColorOK = g.pal, g.palGen, g.richOK
	tv.DefaultTheme = g.tvTheme
}

// withThemeRestore snapshots the theme globals for the test and restores them on
// cleanup, keeping each ApplyTheme/applyMarkdownPalette test hermetic.
func withThemeRestore(t *testing.T) {
	t.Helper()
	saved := snapshotThemeGlobals()
	t.Cleanup(func() { restoreThemeGlobals(saved) })
}

// ----------------------------------------------------------------------------
// Part 1a: the default palette's panel/code backgrounds are no longer ANSI 0.
// ----------------------------------------------------------------------------

// TestIssue200DefaultPaletteNoHardcodedBlack checks the core of part 1: under the
// default (blue) theme neither the sidebar panel nor the fenced-code background is
// the hardcoded black (ANSI 0) that produced a black-on-blue island, and the
// sidebar is reconciled with the desktop chrome.
//
// It deliberately does NOT assert CodeBG equals the desktop/chrome colour: the
// fenced-code background renders on top of the window content surface (whose
// background is WindowBG, see turbotv/window.go), so a CodeBG equal to that
// surface yields no visual separation. That gap is tracked separately in
// TestIssue200DefaultCodeBGBlendsWithWindowBackground.
func TestIssue200DefaultPaletteNoHardcodedBlack(t *testing.T) {
	def := defaultPalette()
	black := tui.ANSIColor(0)

	if def.PanelBG == black {
		t.Errorf("default PanelBG is still hardcoded ANSI 0 (black island)")
	}
	if def.CodeBG == black {
		t.Errorf("default CodeBG is still hardcoded ANSI 0 (black-on-blue)")
	}
	if def.CodeBG.Mode == tui.ColorDefault {
		t.Errorf("default CodeBG is unset (default mode)")
	}
	// The sidebar is reconciled with the desktop chrome: same blue background
	// (issue #200 part 1 — the Overall/TODO panel must belong to the desktop).
	if def.PanelBG != def.DesktopBG {
		t.Errorf("default PanelBG (%+v) != DesktopBG (%+v); sidebar should share the desktop chrome",
			def.PanelBG, def.DesktopBG)
	}
	// Pin the sidebar reconciliation value so a silent swap is caught.
	if def.PanelBG != tui.ANSIColor(4) {
		t.Errorf("default PanelBG = %+v, want ANSI 4 (desktop blue)", def.PanelBG)
	}
}

// TestIssue200DefaultCodeBGBlendsWithWindowBackground documents a real defect in
// the chosen default CodeBG value. The fenced-code background renders on the
// session window's content surface, which turbotv paints with WindowBG
// (turbotv/window.go: Content.UseBackground=true, Background=WindowBG). For the
// default theme WindowBG is ANSI 4 (stock turbotui DefaultTheme), and the driver
// set CodeBG to ANSI 4 as well — so a fenced-code block paints the very same
// blue as the surface behind the surrounding prose. The code panel therefore has
// NO visual separation under the default theme, contradicting both the issue
// ("a subtle shade relative to the panel/desktop bg") and the markdown.go
// comment ("A subtle background sets fenced code apart").
//
// This test pins the current behaviour (it passes today). When CodeBG is changed
// to a distinct shade, flip the assertion below to require the difference.
// TestIssue200DefaultCodeBGDistinctFromWindowBackground verifies the default
// theme's fenced-code background is a DISTINCT shade (a dark navy), not the
// ANSI-4 window/content surface it renders on. The session window fills its
// content area with WindowBG (turbotv/window.go: Content.UseBackground=true), so
// a CodeBG equal to WindowBG would make fenced code invisible against prose. On
// truecolor/256 the navy is distinct; on 16-colour it degrades to black, which is
// still distinct from the blue window (covered in TestIssue200CodeBGDegrade).
func TestIssue200DefaultCodeBGDistinctFromWindowBackground(t *testing.T) {
	def := defaultPalette()
	windowBG := baseTVTheme.WindowBG // ANSI 4 — what ApplyTheme restores for the default theme

	if def.CodeBG == windowBG {
		t.Fatalf("default CodeBG (%+v) equals WindowBG (%+v); fenced code would have no separation",
			def.CodeBG, windowBG)
	}
	// Resolved default CodeBG must stay distinct from the window surface at the
	// colour levels a "default"-theme terminal realistically uses.
	if c := ResolveTheme(config.ThemeConfig{}, truecolorEnv, false).CodeBG; c == windowBG {
		t.Errorf("truecolor default CodeBG (%+v) == WindowBG; fenced code would be invisible", c)
	}
	if c := ResolveTheme(config.ThemeConfig{}, color256Env, false).CodeBG; c == windowBG {
		t.Errorf("256 default CodeBG (%+v) == WindowBG; fenced code would be invisible", c)
	}
}

// TestIssue200InitGlobalsMatchDefaultPalette locks in the second half of the fix:
// the pre-ApplyTheme initial values of the package globals must mirror the new
// default palette. The initial chromePanelBG used to be ANSI 0; if a future
// palette edit forgets to update its mirror (or vice versa) this catches it.
func TestIssue200InitGlobalsMatchDefaultPalette(t *testing.T) {
	def := defaultPalette()

	if initChromePanelBG != def.PanelBG {
		t.Errorf("initial chromePanelBG (%+v) != defaultPalette().PanelBG (%+v)", initChromePanelBG, def.PanelBG)
	}
	if initChromePanelBG != initChromeDesktopBG {
		t.Errorf("initial chromePanelBG (%+v) != chromeDesktopBG (%+v); the sidebar must share the desktop chrome",
			initChromePanelBG, initChromeDesktopBG)
	}
	if initChromePanelBG == tui.ANSIColor(0) {
		t.Errorf("initial chromePanelBG is still ANSI 0 (the original black-island bug)")
	}
	if initMdCodeBG != def.CodeBG {
		t.Errorf("initial mdPalette.codeBG (%+v) != defaultPalette().CodeBG (%+v)", initMdCodeBG, def.CodeBG)
	}
	if initMdCodeBG == tui.ANSIColor(0) {
		t.Errorf("initial mdPalette.codeBG is still ANSI 0 (the original black-on-blue bug)")
	}
	if !initMdHasCodeBG {
		t.Errorf("initial mdPalette.hasCodeBG = false; the default palette should paint a code background")
	}
}

// ----------------------------------------------------------------------------
// Part 1b: the code/panel backgrounds are theme-derived — they vary per palette.
// ----------------------------------------------------------------------------

// TestIssue200CodeBGDiffersPerTheme asserts the fenced-code background is a theme
// role, not a constant: the three built-in palettes each carry a distinct CodeBG,
// and none is the legacy hardcoded ANSI 0.
func TestIssue200CodeBGDiffersPerTheme(t *testing.T) {
	palettes := []struct {
		name string
		t    Theme
	}{
		{"default", defaultPalette()},
		{"high-contrast", highContrastPalette()},
		{"dark", darkPalette()},
	}
	black := tui.ANSIColor(0)
	seen := make(map[tui.Color][]string, len(palettes))
	for _, p := range palettes {
		if p.t.CodeBG == black {
			t.Errorf("%s palette CodeBG is the legacy hardcoded ANSI 0", p.name)
		}
		if p.t.CodeBG.Mode == tui.ColorDefault {
			t.Errorf("%s palette CodeBG is unset (default mode)", p.name)
		}
		seen[p.t.CodeBG] = append(seen[p.t.CodeBG], p.name)
	}
	// Every palette must own a distinct CodeBG — that is what "theme-derived" means.
	for c, owners := range seen {
		if len(owners) > 1 {
			t.Errorf("CodeBG %+v shared by palettes %v; each palette should derive its own", c, owners)
		}
	}
}

// TestIssue200PanelBGThemeDerived asserts the panel background follows the theme:
// the default (blue) palette differs from the two black-canvas presets. (The
// high-contrast and dark presets intentionally share a pure-black panel bg, so
// panel bg is not required to be pairwise-unique — only theme-dependent.)
func TestIssue200PanelBGThemeDerived(t *testing.T) {
	def, hc, dark := defaultPalette(), highContrastPalette(), darkPalette()
	blackRGB := tui.RGBColor(0, 0, 0)

	if def.PanelBG == tui.ANSIColor(0) {
		t.Errorf("default PanelBG is hardcoded ANSI 0")
	}
	if def.PanelBG == hc.PanelBG || def.PanelBG == dark.PanelBG {
		t.Errorf("default PanelBG (%+v) collides with a black-canvas preset; it must be the blue desktop chrome", def.PanelBG)
	}
	// The two black-canvas presets really are black.
	if hc.PanelBG != blackRGB || dark.PanelBG != blackRGB {
		t.Errorf("black-canvas PanelBG: hc=%+v dark=%+v, want RGB black", hc.PanelBG, dark.PanelBG)
	}
}

// ----------------------------------------------------------------------------
// Part 1c: applyMarkdownPalette reads the theme's CodeBG (per palette) and
// suppresses the background only for the pure-black high-contrast preset.
// ----------------------------------------------------------------------------

// TestIssue200ApplyMarkdownPaletteUsesCodeBG drives applyMarkdownPalette directly
// with each resolved palette and checks mdPalette.codeBG tracks t.CodeBG, while
// hasCodeBG is on for default/dark and off for high-contrast (and under NO_COLOR).
func TestIssue200ApplyMarkdownPaletteUsesCodeBG(t *testing.T) {
	withThemeRestore(t)

	cases := []struct {
		name        string
		theme       Theme
		wantHasBG   bool
		wantRichOK  bool
		checkCodeBG bool // assert mdPalette.codeBG == theme.CodeBG
	}{
		{
			name: "default", wantHasBG: true, wantRichOK: true, checkCodeBG: true,
			theme: ResolveTheme(config.ThemeConfig{}, color16Env, false),
		},
		{
			name: "dark", wantHasBG: true, wantRichOK: true, checkCodeBG: true,
			theme: ResolveTheme(config.ThemeConfig{Name: "dark"}, truecolorEnv, false),
		},
		{
			name: "high-contrast", wantHasBG: false, wantRichOK: true, checkCodeBG: false,
			theme: ResolveTheme(config.ThemeConfig{Name: "high-contrast"}, truecolorEnv, false),
		},
		{
			name: "no-color", wantHasBG: false, wantRichOK: false, checkCodeBG: false,
			theme: ResolveTheme(config.ThemeConfig{}, noColorEnv, false),
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			applyMarkdownPalette(c.theme)
			if mdPalette.hasCodeBG != c.wantHasBG {
				t.Errorf("hasCodeBG = %v, want %v", mdPalette.hasCodeBG, c.wantHasBG)
			}
			if richMarkdownColorOK != c.wantRichOK {
				t.Errorf("richMarkdownColorOK = %v, want %v", richMarkdownColorOK, c.wantRichOK)
			}
			if c.checkCodeBG && mdPalette.codeBG != c.theme.CodeBG {
				t.Errorf("mdPalette.codeBG (%+v) != theme.CodeBG (%+v); the code background must come from the theme",
					mdPalette.codeBG, c.theme.CodeBG)
			}
		})
	}
}

// TestIssue200ApplyThemeMarkdownAndChrome is the integration counterpart: after
// ApplyTheme the live globals reflect the resolved palette — the sidebar is no
// longer a black island under the default theme, and dark keeps a visible code
// background while high-contrast suppresses it.
func TestIssue200ApplyThemeMarkdownAndChrome(t *testing.T) {
	withThemeRestore(t)

	// Default (blue): panel shares the desktop chrome and is not black; the code
	// background is the themed blue, not ANSI 0.
	def := ResolveTheme(config.ThemeConfig{}, color16Env, false)
	ApplyTheme(def)
	if chromePanelBG == tui.ANSIColor(0) {
		t.Errorf("default: chromePanelBG is ANSI 0 (black island regressed)")
	}
	if chromePanelBG != chromeDesktopBG {
		t.Errorf("default: chromePanelBG (%+v) != chromeDesktopBG (%+v)", chromePanelBG, chromeDesktopBG)
	}
	if mdPalette.codeBG != def.CodeBG {
		t.Errorf("default: mdPalette.codeBG (%+v) != CodeBG (%+v)", mdPalette.codeBG, def.CodeBG)
	}
	if !mdPalette.hasCodeBG {
		t.Errorf("default: mdPalette.hasCodeBG = false, want true")
	}

	// Dark: pure-black panel but a distinct (dark-grey) code background.
	dark := ResolveTheme(config.ThemeConfig{Name: "dark"}, truecolorEnv, false)
	ApplyTheme(dark)
	if mdPalette.codeBG != dark.CodeBG {
		t.Errorf("dark: mdPalette.codeBG (%+v) != CodeBG (%+v)", mdPalette.codeBG, dark.CodeBG)
	}
	if !mdPalette.hasCodeBG {
		t.Errorf("dark: mdPalette.hasCodeBG = false; dark should paint a code background (unlike high-contrast)")
	}
	if mdPalette.codeBG == dark.DesktopBG {
		t.Errorf("dark: code background (%+v) equals the black desktop; code blocks would be invisible", mdPalette.codeBG)
	}

	// High-contrast: pure-black UI suppresses the code background.
	hc := ResolveTheme(config.ThemeConfig{Name: "high-contrast"}, truecolorEnv, false)
	ApplyTheme(hc)
	if mdPalette.hasCodeBG {
		t.Errorf("high-contrast: mdPalette.hasCodeBG = true; the pure-black preset must suppress the code background")
	}
}

// ----------------------------------------------------------------------------
// Part 2: the new "dark" theme is complete, cohesive, and selectable.
// ----------------------------------------------------------------------------

// TestIssue200DarkPaletteComplete verifies darkPalette() returns a fully
// populated theme on a black background with every role explicitly coloured.
func TestIssue200DarkPaletteComplete(t *testing.T) {
	d := darkPalette()
	if d.Name != themeDark {
		t.Errorf("darkPalette Name = %q, want %q", d.Name, themeDark)
	}
	black := tui.RGBColor(0, 0, 0)
	if d.DesktopBG != black {
		t.Errorf("dark DesktopBG = %+v, want RGB black", d.DesktopBG)
	}
	if d.PanelBG != black {
		t.Errorf("dark PanelBG = %+v, want RGB black", d.PanelBG)
	}
	// Every colour role is explicitly set (the dark theme is authored in RGB, so
	// any role still at the zero/default value is a missing entry).
	roles := []struct {
		label string
		c     tui.Color
	}{
		{"User", d.User}, {"Agent", d.Agent}, {"Note", d.Note}, {"Tool", d.Tool},
		{"Result", d.Result}, {"Info", d.Info}, {"Error", d.Error},
		{"DesktopFG", d.DesktopFG}, {"DesktopBG", d.DesktopBG},
		{"PanelFG", d.PanelFG}, {"PanelBG", d.PanelBG},
		{"Title", d.Title}, {"Divider", d.Divider}, {"Accent", d.Accent},
		{"CodeBG", d.CodeBG},
	}
	for _, r := range roles {
		if r.c.Mode != tui.ColorRGB {
			t.Errorf("dark %s = %+v, want an explicit RGB colour", r.label, r.c)
		}
	}
	// The code background must lift code off the black background (issue goal:
	// "a dark-grey CodeBG so code stands apart on black").
	if d.CodeBG == black {
		t.Errorf("dark CodeBG is black; code blocks would vanish on the black background")
	}
}

// TestIssue200DarkPaletteDistinctSemanticColours is a cohesion/legibility sanity
// check: the seven semantic transcript roles are pairwise distinct (so user vs
// agent vs tool etc. remain distinguishable) and none is the black background.
func TestIssue200DarkPaletteDistinctSemanticColours(t *testing.T) {
	d := darkPalette()
	black := tui.RGBColor(0, 0, 0)
	sem := []struct {
		label string
		c     tui.Color
	}{
		{"User", d.User}, {"Agent", d.Agent}, {"Note", d.Note}, {"Tool", d.Tool},
		{"Result", d.Result}, {"Info", d.Info}, {"Error", d.Error},
	}
	seen := make(map[tui.Color]string, len(sem))
	for _, r := range sem {
		if r.c == black {
			t.Errorf("dark %s is the black background (illegible)", r.label)
		}
		if other, dup := seen[r.c]; dup {
			t.Errorf("dark %s and %s share colour %+v; semantic roles should be distinguishable", other, r.label, r.c)
		}
		seen[r.c] = r.label
	}
}

// TestIssue200CanonicalThemeNameDark covers the dark aliases (case/space
// insensitive) and guards the existing default/high-contrast canonicalisation.
func TestIssue200CanonicalThemeNameDark(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"dark", themeDark},
		{"Dark", themeDark},
		{"DARK", themeDark},
		{"midnight", themeDark},
		{"  midnight  ", themeDark},
		{"black", themeDark},
		{"Black", themeDark},
		// Existing behaviour must be unchanged.
		{"high-contrast", themeHighContrast},
		{"colorblind", themeHighContrast},
		{"", themeDefault},
		{"unknown-theme", themeDefault},
		{"default", themeDefault},
	}
	for _, c := range cases {
		if got := canonicalThemeName(c.in); got != c.want {
			t.Errorf("canonicalThemeName(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestIssue200PaletteByNameDark checks paletteByName resolves the dark aliases to
// the exact darkPalette (DeepEqual), case-insensitively.
func TestIssue200PaletteByNameDark(t *testing.T) {
	want := darkPalette()
	for _, name := range []string{"dark", "midnight", "black", "DARK", "  Dark "} {
		got := paletteByName(name)
		if got.Name != themeDark {
			t.Errorf("paletteByName(%q).Name = %q, want %q", name, got.Name, themeDark)
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("paletteByName(%q) = %+v, want %+v", name, got, want)
		}
	}
	// An unknown name still falls back to the default palette, not dark.
	if paletteByName("nope").Name != themeDefault {
		t.Errorf("paletteByName(\"nope\") resolved to a non-default palette")
	}
}

// TestIssue200ResolveThemeDarkDegrade mirrors the high-contrast degrade test: the
// dark RGB palette is preserved on truecolor, quantised to a 256-index on
// 256-colour terminals and to a 0..15 ANSI index on 16-colour terminals.
func TestIssue200ResolveThemeDarkDegrade(t *testing.T) {
	cfg := config.ThemeConfig{Name: "midnight"} // alias for dark

	tc := ResolveTheme(cfg, truecolorEnv, false)
	if tc.Name != themeDark {
		t.Fatalf("Name = %q, want %q", tc.Name, themeDark)
	}
	if tc.User.Mode != tui.ColorRGB || tc.User != darkPalette().User {
		t.Errorf("truecolor User = %+v, want the raw dark RGB %+v", tc.User, darkPalette().User)
	}

	c256 := ResolveTheme(cfg, color256Env, false)
	if c256.User.Mode != tui.ColorANSI || c256.User.Value < 16 {
		t.Errorf("256 User = %+v, want a 256-colour ANSI index (>=16)", c256.User)
	}

	c16 := ResolveTheme(cfg, color16Env, false)
	if c16.User.Mode != tui.ColorANSI || c16.User.Value > 15 {
		t.Errorf("16 User = %+v, want a 0..15 ANSI index", c16.User)
	}
}

// TestIssue200ApplyThemeDarkBlackCanvas checks ApplyTheme installs the black-canvas
// dialog/window chrome for the dark theme (shared with high-contrast) and seeds
// the dialog accents from the palette.
func TestIssue200ApplyThemeDarkBlackCanvas(t *testing.T) {
	withThemeRestore(t)

	dark := ResolveTheme(config.ThemeConfig{Name: "dark"}, truecolorEnv, false)
	ApplyTheme(dark)

	black := tui.RGBColor(0, 0, 0)
	if tv.DefaultTheme.DialogBG != black {
		t.Errorf("dark DialogBG = %+v, want RGB black (black-canvas chrome)", tv.DefaultTheme.DialogBG)
	}
	if tv.DefaultTheme.DesktopBG != black {
		t.Errorf("dark DesktopBG = %+v, want RGB black", tv.DefaultTheme.DesktopBG)
	}
	if tv.DefaultTheme.WindowBG != black {
		t.Errorf("dark WindowBG = %+v, want RGB black", tv.DefaultTheme.WindowBG)
	}
	// The chrome accents follow the palette's user/tool colours (as high-contrast does).
	if colorDialogHeader != dark.User || colorDialogDetail != dark.Tool {
		t.Errorf("dark dialog accents = (header=%+v detail=%+v), want (User=%+v Tool=%+v)",
			colorDialogHeader, colorDialogDetail, dark.User, dark.Tool)
	}
	// The panel chrome is installed from the theme too.
	if chromePanelBG != dark.PanelBG {
		t.Errorf("dark chromePanelBG = %+v, want %+v", chromePanelBG, dark.PanelBG)
	}
}

// TestIssue200DarkThemeMenuAndMnemonicChrome locks in the black-canvas chrome
// being fully populated for the dark theme: the menubar, dropdown menus and
// dialog mnemonics must carry explicit colours, otherwise they fall back to the
// terminal default and render low-contrast/invisible on the black canvas. (This
// regressed for high-contrast historically because blackCanvasTVTheme left the
// Menu*/DialogMnemonicFG fields at their zero value; the dark theme must not.)
func TestIssue200DarkThemeMenuAndMnemonicChrome(t *testing.T) {
	withThemeRestore(t)

	dark := ResolveTheme(config.ThemeConfig{Name: "dark"}, truecolorEnv, false)
	ApplyTheme(dark)

	// Every menu/mnemonic slot must be explicitly set (not the zero ColorDefault).
	fields := []struct {
		label string
		c     tui.Color
	}{
		{"DialogMnemonicFG", tv.DefaultTheme.DialogMnemonicFG},
		{"MenuBarFG", tv.DefaultTheme.MenuBarFG},
		{"MenuBarBG", tv.DefaultTheme.MenuBarBG},
		{"MenuHotFG", tv.DefaultTheme.MenuHotFG},
		{"MenuHotBG", tv.DefaultTheme.MenuHotBG},
		{"MenuSelectFG", tv.DefaultTheme.MenuSelectFG},
		{"MenuSelectBG", tv.DefaultTheme.MenuSelectBG},
		{"MenuShadow", tv.DefaultTheme.MenuShadow},
	}
	for _, f := range fields {
		if f.c.Mode == tui.ColorDefault {
			t.Errorf("dark %s is unset (ColorDefault); menu/mnemonic chrome would be invisible on the black canvas", f.label)
		}
	}
	// Spot-check the intended mapping: white-on-black menubar with the accent
	// marking the hot key and the selected row.
	if tv.DefaultTheme.MenuBarFG != dark.PanelFG || tv.DefaultTheme.MenuBarBG != dark.PanelBG {
		t.Errorf("dark MenuBar = (FG:%+v BG:%+v), want white-on-black (PanelFG/PanelBG)",
			tv.DefaultTheme.MenuBarFG, tv.DefaultTheme.MenuBarBG)
	}
	if tv.DefaultTheme.MenuSelectBG != dark.Accent {
		t.Errorf("dark MenuSelectBG = %+v, want Accent %+v", tv.DefaultTheme.MenuSelectBG, dark.Accent)
	}
}

// TestIssue200PresetIndexDark checks the theme-editor dropdown maps the dark
// aliases to their index, and that the default/high-contrast indices are stable.
func TestIssue200PresetIndexDark(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want int
	}{
		{"dark", "dark", 2},
		{"midnight", "midnight", 2},
		{"black", "black", 2},
		{"Dark caps", "Dark", 2},
		{"default", "default", 0},
		{"empty", "", 0},
		{"unknown", "nope", 0},
		{"high-contrast", "high-contrast", 1},
		{"colorblind alias", "colorblind", 1},
	}
	for _, c := range cases {
		if got := presetIndex(c.in); got != c.want {
			t.Errorf("presetIndex(%q) = %d, want %d", c.in, got, c.want)
		}
	}
	// The dark preset must actually exist in the dropdown list.
	found := false
	for _, p := range themePresets {
		if p.name == themeDark {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("themePresets has no entry for the dark theme (%q)", themeDark)
	}
}

// TestIssue200ThemePresetsAndRoles wiring checks: the preset list is unique and
// the editor exposes the new code_bg role so an override round-trips.
func TestIssue200ThemePresetsAndRoles(t *testing.T) {
	seen := make(map[string]bool, len(themePresets))
	for _, p := range themePresets {
		if seen[p.name] {
			t.Errorf("duplicate themePreset name %q", p.name)
		}
		seen[p.name] = true
	}
	if !seen[themeDefault] || !seen[themeHighContrast] || !seen[themeDark] {
		t.Errorf("themePresets missing one of default/high-contrast/dark; have %v", seen)
	}

	hasCodeBG := false
	for _, r := range themeRoles {
		if r.key == "code_bg" {
			hasCodeBG = true
			if r.get(darkPalette()) != darkPalette().CodeBG {
				t.Errorf("code_bg role getter does not return Theme.CodeBG")
			}
		}
	}
	if !hasCodeBG {
		t.Errorf("themeRoles has no code_bg entry; a code_bg override could not be edited")
	}
}

// ----------------------------------------------------------------------------
// code_bg override + editor round-trip (error handling included).
// ----------------------------------------------------------------------------

// TestIssue200CodeBGOverride checks the code_bg override is honoured by
// applyOverrides/ResolveTheme, and that bad values are ignored.
func TestIssue200CodeBGOverride(t *testing.T) {
	// A valid hex override wins, surviving un-degraded on truecolor.
	cfg := config.ThemeConfig{Overrides: map[string]string{"code_bg": "#102030"}}
	got := ResolveTheme(cfg, truecolorEnv, false)
	if got.CodeBG != tui.RGBColor(0x10, 0x20, 0x30) {
		t.Errorf("code_bg override: CodeBG = %+v, want #102030", got.CodeBG)
	}

	// An ANSI-index override.
	cfg = config.ThemeConfig{Overrides: map[string]string{"code_bg": "60"}}
	got = ResolveTheme(cfg, truecolorEnv, false)
	if got.CodeBG != tui.ANSIColor(60) {
		t.Errorf("code_bg override: CodeBG = %+v, want ANSI 60", got.CodeBG)
	}

	// "default" maps to the terminal default colour.
	cfg = config.ThemeConfig{Overrides: map[string]string{"code_bg": "default"}}
	got = ResolveTheme(cfg, truecolorEnv, false)
	if got.CodeBG != tui.DefaultColor() {
		t.Errorf("code_bg default: CodeBG = %+v, want default", got.CodeBG)
	}

	// An unparseable value is ignored — the palette's own CodeBG is kept.
	cfg = config.ThemeConfig{Name: "dark", Overrides: map[string]string{"code_bg": "not-a-colour"}}
	got = ResolveTheme(cfg, truecolorEnv, false)
	if got.CodeBG != darkPalette().CodeBG {
		t.Errorf("invalid code_bg override should be ignored; CodeBG = %+v, want dark palette %+v",
			got.CodeBG, darkPalette().CodeBG)
	}
}

// TestIssue200CodeBGEditorRoundTrip checks the code_bg role survives the editor's
// save→reopen cycle for the dark preset, and that a pristine dark preset has no
// overrides (so it is stored by name alone).
func TestIssue200CodeBGEditorRoundTrip(t *testing.T) {
	darkSpecs := specsFor(paletteByName(themeDark))

	t.Run("pristine dark has no overrides", func(t *testing.T) {
		got := buildThemeConfig("dark", false, false, darkSpecs)
		want := config.ThemeConfig{Name: themeDark}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("pristine dark: got %+v, want %+v", got, want)
		}
	})

	t.Run("changed code_bg becomes an override", func(t *testing.T) {
		specs := cloneSpecs(darkSpecs)
		specs["code_bg"] = "#101010"
		got := buildThemeConfig("dark", false, false, specs)
		if got.Overrides["code_bg"] != "#101010" {
			t.Fatalf("expected code_bg override, got %+v", got.Overrides)
		}
	})

	t.Run("round-trip is stable", func(t *testing.T) {
		specs := cloneSpecs(darkSpecs)
		specs["code_bg"] = "#101010"
		specs["user"] = "#FFFFFF"
		cfg := buildThemeConfig("dark", false, false, specs)
		// Reopen: seed fields from the edited theme and rebuild — must match.
		reopened := buildThemeConfig(cfg.Name, cfg.NoColor, false, specsFor(editedTheme(cfg)))
		if !reflect.DeepEqual(reopened, cfg) {
			t.Fatalf("round-trip mismatch:\n got %+v\nwant %+v", reopened, cfg)
		}
	})
}

// ----------------------------------------------------------------------------
// CodeBG degradation + NO_COLOR (the threading added for the new role).
// ----------------------------------------------------------------------------

// TestIssue200CodeBGDegrade verifies the CodeBG role is degraded like every other
// colour: RGB on truecolor, an ANSI index on 256/16, and the terminal default
// under NO_COLOR. The default palette's ANSI CodeBG passes through unchanged.
func TestIssue200CodeBGDegrade(t *testing.T) {
	darkRaw := darkPalette().CodeBG       // RGB
	defaultRaw := defaultPalette().CodeBG // RGB navy — the one RGB role in the default palette

	// Dark palette.
	if c := ResolveTheme(config.ThemeConfig{Name: "dark"}, truecolorEnv, false).CodeBG; c != darkRaw {
		t.Errorf("truecolor dark CodeBG = %+v, want %+v", c, darkRaw)
	}
	if c := ResolveTheme(config.ThemeConfig{Name: "dark"}, color256Env, false).CodeBG; c.Mode != tui.ColorANSI || c.Value < 16 {
		t.Errorf("256 dark CodeBG = %+v, want an ANSI index >=16", c)
	}
	if c := ResolveTheme(config.ThemeConfig{Name: "dark"}, color16Env, false).CodeBG; c.Mode != tui.ColorANSI {
		t.Errorf("16 dark CodeBG = %+v, want ANSI mode", c)
	}

	// Default palette — CodeBG is now an RGB role, so it degrades like dark's RGB
	// colours rather than passing through as ANSI 4.
	if c := ResolveTheme(config.ThemeConfig{}, truecolorEnv, false).CodeBG; c != defaultRaw {
		t.Errorf("truecolor default CodeBG = %+v, want %+v", c, defaultRaw)
	}
	if c := ResolveTheme(config.ThemeConfig{}, color256Env, false).CodeBG; c.Mode != tui.ColorANSI || c.Value < 16 {
		t.Errorf("256 default CodeBG = %+v, want an ANSI index >=16", c)
	}
	// At 16 colours the navy (0x101450) quantises to ANSI 0 (black). That is still
	// distinct from the ANSI-4 window background so the code panel stays visible,
	// but it means 16-colour terminals render code blocks black-on-blue — the look
	// issue #200 objected to, just no longer hardcoded to it.
	c16 := ResolveTheme(config.ThemeConfig{}, color16Env, false).CodeBG
	if c16 != tui.ANSIColor(0) {
		t.Errorf("16 default CodeBG = %+v, want ANSI 0 (navy degrades to black at 16 colours)", c16)
	}
	if c16 == baseTVTheme.WindowBG {
		t.Errorf("16 default CodeBG collides with the window background; code panel would be invisible")
	}
}

// TestIssue200ResolveThemeNoColorFlattensCodeBG fills a gap in the pre-existing
// TestResolveThemeNoColor (which predates the CodeBG role): under NO_COLOR the
// code background must flatten to the terminal default for every palette.
func TestIssue200ResolveThemeNoColorFlattensCodeBG(t *testing.T) {
	def := tui.DefaultColor()
	for _, name := range []string{"", "high-contrast", "dark"} {
		got := ResolveTheme(config.ThemeConfig{Name: name}, noColorEnv, false)
		if got.CodeBG != def {
			t.Errorf("NO_COLOR theme %q: CodeBG = %+v, want default", name, got.CodeBG)
		}
	}
}

// TestIssue200DarkCodeBGColor16CollidesWithBackground documents a real degradation
// edge: the dark theme's dark-grey CodeBG (0x262626) quantises to ANSI 0 (black)
// on a 16-colour terminal — the same colour as its black panel background. So at
// 16 colours the "code panel" is no longer visually distinct, the exact
// invisibility the high-contrast preset avoids by suppressing the code background
// (applyMarkdownPalette sets hasCodeBG=false only for high-contrast, not for dark).
// At 256 colours and truecolor the code background stays distinct. This test
// pins the current behaviour; if the driver makes dark suppress or recolour its
// code background at low fidelity, the Color16 assertion below should flip.
func TestIssue200DarkCodeBGColor16CollidesWithBackground(t *testing.T) {
	// Truecolor and 256: the code background is distinct from the black panel —
	// code blocks read as a separate region, as intended.
	tc := ResolveTheme(config.ThemeConfig{Name: "dark"}, truecolorEnv, false)
	if tc.CodeBG == tc.PanelBG {
		t.Errorf("truecolor: dark CodeBG (%+v) == PanelBG; code must stand apart on black", tc.CodeBG)
	}
	c256 := ResolveTheme(config.ThemeConfig{Name: "dark"}, color256Env, false)
	if c256.CodeBG == c256.PanelBG {
		t.Errorf("256: dark CodeBG (%+v) == PanelBG; code must stand apart on black", c256.CodeBG)
	}

	// 16-colour: 0x262626 maps to ANSI 0, colliding with the black background.
	c16 := ResolveTheme(config.ThemeConfig{Name: "dark"}, color16Env, false)
	if c16.CodeBG != tui.ANSIColor(0) {
		t.Fatalf("16: dark CodeBG = %+v, want ANSI 0 (quantised from 0x262626)", c16.CodeBG)
	}
	t.Logf("color-level note: at 16 colours the dark CodeBG (%+v) equals its black background; "+
		"code blocks are invisible there though their token foregrounds still render. "+
		"High-contrast avoids this by suppressing hasCodeBG; dark does not.", c16.CodeBG)
}
