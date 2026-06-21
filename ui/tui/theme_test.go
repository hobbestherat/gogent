package ui

import (
	"testing"

	"gogent/internal/config"

	tui "github.com/hobbestherat/turbotui"
	tv "github.com/hobbestherat/turbotui/turbotv"
)

// envOf returns an env accessor backed by a map, for deterministic colour-level
// detection in tests.
func envOf(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

// TestDetectColorLevel covers the NO_COLOR convention and the TERM/COLORTERM
// fidelity ladder.
func TestDetectColorLevel(t *testing.T) {
	cases := []struct {
		name string
		env  map[string]string
		want ColorLevel
	}{
		{"no_color set disables", map[string]string{"NO_COLOR": "1", "TERM": "xterm-256color", "COLORTERM": "truecolor"}, ColorNone},
		{"no_color empty ignored", map[string]string{"NO_COLOR": "", "TERM": "xterm"}, Color16},
		{"missing term", map[string]string{}, ColorNone},
		{"dumb term", map[string]string{"TERM": "dumb"}, ColorNone},
		{"truecolor", map[string]string{"TERM": "xterm", "COLORTERM": "truecolor"}, ColorTrue},
		{"24bit", map[string]string{"TERM": "xterm", "COLORTERM": "24bit"}, ColorTrue},
		{"256 term", map[string]string{"TERM": "screen-256color"}, Color256},
		{"plain 16", map[string]string{"TERM": "xterm"}, Color16},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := detectColorLevel(envOf(c.env)); got != c.want {
				t.Errorf("detectColorLevel(%v) = %v, want %v", c.env, got, c.want)
			}
		})
	}
}

// TestParseColor covers hex, ANSI-index, default and rejected colour specs.
func TestParseColor(t *testing.T) {
	cases := []struct {
		spec   string
		want   tui.Color
		wantOK bool
	}{
		{"#E69F00", tui.RGBColor(0xE6, 0x9F, 0x00), true},
		{"e69f00", tui.RGBColor(0xE6, 0x9F, 0x00), true},
		{"  #FFFFFF ", tui.RGBColor(0xFF, 0xFF, 0xFF), true},
		{"12", tui.ANSIColor(12), true},
		{"255", tui.ANSIColor(255), true},
		{"default", tui.DefaultColor(), true},
		{"none", tui.DefaultColor(), true},
		{"", tui.Color{}, false},
		{"256", tui.Color{}, false},    // out of ANSI range
		{"#12345", tui.Color{}, false}, // wrong hex length
		{"orange", tui.Color{}, false}, // named colours unsupported
	}
	for _, c := range cases {
		t.Run(c.spec, func(t *testing.T) {
			got, ok := parseColor(c.spec)
			if ok != c.wantOK {
				t.Fatalf("parseColor(%q) ok = %v, want %v", c.spec, ok, c.wantOK)
			}
			if ok && got != c.want {
				t.Errorf("parseColor(%q) = %+v, want %+v", c.spec, got, c.want)
			}
		})
	}
}

// TestResolveThemeNoColor verifies NO_COLOR / --no-color / cfg.NoColor all flatten
// every colour to the terminal default.
func TestResolveThemeNoColor(t *testing.T) {
	def := tui.DefaultColor()
	check := func(t *testing.T, theme Theme) {
		t.Helper()
		if theme.Level != ColorNone {
			t.Errorf("Level = %v, want ColorNone", theme.Level)
		}
		for name, c := range map[string]tui.Color{
			"User": theme.User, "Agent": theme.Agent, "Note": theme.Note,
			"Tool": theme.Tool, "Result": theme.Result, "Info": theme.Info,
			"Error": theme.Error, "DesktopBG": theme.DesktopBG, "PanelBG": theme.PanelBG,
			"Title": theme.Title, "Divider": theme.Divider, "Accent": theme.Accent,
		} {
			if c != def {
				t.Errorf("%s = %+v, want DefaultColor", name, c)
			}
		}
	}
	colourEnv := envOf(map[string]string{"TERM": "xterm-256color", "COLORTERM": "truecolor"})

	t.Run("flag", func(t *testing.T) {
		check(t, ResolveTheme(config.ThemeConfig{}, colourEnv, true))
	})
	t.Run("cfg", func(t *testing.T) {
		check(t, ResolveTheme(config.ThemeConfig{NoColor: true}, colourEnv, false))
	})
	t.Run("env", func(t *testing.T) {
		check(t, ResolveTheme(config.ThemeConfig{}, envOf(map[string]string{"NO_COLOR": "1", "TERM": "xterm"}), false))
	})
}

// TestResolveThemeDefaultPalette confirms the default palette keeps its original
// ANSI indices on a 16-colour terminal.
func TestResolveThemeDefaultPalette(t *testing.T) {
	got := ResolveTheme(config.ThemeConfig{}, envOf(map[string]string{"TERM": "xterm"}), false)
	if got.Name != themeDefault {
		t.Errorf("Name = %q, want %q", got.Name, themeDefault)
	}
	want := defaultPalette()
	if got.User != want.User || got.Agent != want.Agent || got.Note != want.Note ||
		got.Tool != want.Tool || got.Result != want.Result || got.Info != want.Info ||
		got.Error != want.Error {
		t.Errorf("default palette colours changed under Color16:\n got %+v\nwant %+v", got, want)
	}
}

// TestResolveThemeHighContrastDegrade checks the high-contrast (RGB) palette is
// kept as RGB on truecolor and quantised to ANSI on lesser terminals.
func TestResolveThemeHighContrastDegrade(t *testing.T) {
	cfg := config.ThemeConfig{Name: "colorblind"} // alias for high-contrast

	tc := ResolveTheme(cfg, envOf(map[string]string{"TERM": "xterm", "COLORTERM": "truecolor"}), false)
	if tc.Name != themeHighContrast {
		t.Fatalf("Name = %q, want %q", tc.Name, themeHighContrast)
	}
	if tc.User.Mode != tui.ColorRGB {
		t.Errorf("truecolor User mode = %v, want RGB", tc.User.Mode)
	}
	if tc.User != okabeSkyBlue {
		t.Errorf("truecolor User = %+v, want %+v", tc.User, okabeSkyBlue)
	}

	c256 := ResolveTheme(cfg, envOf(map[string]string{"TERM": "xterm-256color"}), false)
	if c256.User.Mode != tui.ColorANSI || c256.User.Value < 16 {
		t.Errorf("256 User = %+v, want a 256-colour ANSI index", c256.User)
	}

	c16 := ResolveTheme(cfg, envOf(map[string]string{"TERM": "xterm"}), false)
	if c16.User.Mode != tui.ColorANSI || c16.User.Value > 15 {
		t.Errorf("16 User = %+v, want a 0..15 ANSI index", c16.User)
	}
}

// TestApplyOverrides checks per-role overrides win over the palette and that bad
// entries are ignored.
func TestApplyOverrides(t *testing.T) {
	cfg := config.ThemeConfig{
		Overrides: map[string]string{
			"user":     "#123456",
			"error":    "1",
			"note":     "default",
			"panel_bg": "#000000",
			"bogus":    "#FFFFFF",  // unknown role ignored
			"agent":    "nonsense", // unparsable value ignored
		},
	}
	// Truecolor so RGB overrides survive un-degraded.
	got := ResolveTheme(cfg, envOf(map[string]string{"TERM": "xterm", "COLORTERM": "truecolor"}), false)
	if got.User != tui.RGBColor(0x12, 0x34, 0x56) {
		t.Errorf("User = %+v, want #123456", got.User)
	}
	if got.Error != tui.ANSIColor(1) {
		t.Errorf("Error = %+v, want ANSI 1", got.Error)
	}
	if got.Note != tui.DefaultColor() {
		t.Errorf("Note = %+v, want default", got.Note)
	}
	if got.PanelBG != tui.RGBColor(0, 0, 0) {
		t.Errorf("PanelBG = %+v, want black", got.PanelBG)
	}
	// agent had an unparsable override, so it keeps the default-palette value.
	if got.Agent != defaultPalette().Agent {
		t.Errorf("Agent = %+v, want palette default %+v", got.Agent, defaultPalette().Agent)
	}
}

// TestRGBQuantisation spot-checks the RGB→256 and RGB→16 mappings on colours
// with known nearest matches.
func TestRGBQuantisation(t *testing.T) {
	if got := rgbTo256(0, 0, 0); got != 16 {
		t.Errorf("rgbTo256(black) = %d, want 16", got)
	}
	if got := rgbTo256(255, 255, 255); got != 231 {
		t.Errorf("rgbTo256(white) = %d, want 231", got)
	}
	if got := rgbTo256(0xBB, 0xBB, 0xBB); got != 249 {
		t.Errorf("rgbTo256(grey) = %d, want 249", got)
	}
	if got := rgbTo16(0, 0, 0); got != 0 {
		t.Errorf("rgbTo16(black) = %d, want 0", got)
	}
	if got := rgbTo16(255, 255, 255); got != 15 {
		t.Errorf("rgbTo16(white) = %d, want 15", got)
	}
	if got := rgbTo16(250, 250, 90); got != 11 {
		t.Errorf("rgbTo16(yellow) = %d, want 11", got)
	}
	// xterm256ToRGB inverts the cube/greyscale mapping closely enough that a
	// round-trip through rgbTo16 lands on a sensible 16-colour bucket.
	if r, g, b := xterm256ToRGB(231); r != 255 || g != 255 || b != 255 {
		t.Errorf("xterm256ToRGB(231) = %d,%d,%d, want 255,255,255", r, g, b)
	}
}

// TestApplyTheme verifies the resolved colours are installed into the package
// variables and that the chrome theme is swapped per palette. Globals are saved
// and restored so the test does not bleed into others.
func TestApplyTheme(t *testing.T) {
	saved := snapshotColors()
	savedTV := tv.DefaultTheme
	t.Cleanup(func() {
		restoreColors(saved)
		tv.DefaultTheme = savedTV
	})

	// No-colour: every semantic var becomes the terminal default and the chrome
	// theme is neutralised.
	ApplyTheme(ResolveTheme(config.ThemeConfig{}, envOf(map[string]string{"NO_COLOR": "1"}), false))
	if colorUser != tui.DefaultColor() || colorError != tui.DefaultColor() {
		t.Errorf("no-color: semantic colours not neutralised (user=%+v error=%+v)", colorUser, colorError)
	}
	if tv.DefaultTheme.DialogBG != tui.DefaultColor() {
		t.Errorf("no-color: tv.DefaultTheme.DialogBG = %+v, want default", tv.DefaultTheme.DialogBG)
	}
	if colorDialogHeader != tui.DefaultColor() || colorDialogDetail != tui.DefaultColor() {
		t.Errorf("no-color: dialog accents not neutralised (header=%+v detail=%+v)", colorDialogHeader, colorDialogDetail)
	}

	// High-contrast on truecolor: User picks up the Okabe sky-blue and the chrome
	// dialog goes black.
	ApplyTheme(ResolveTheme(config.ThemeConfig{Name: "high-contrast"},
		envOf(map[string]string{"TERM": "xterm", "COLORTERM": "truecolor"}), false))
	if colorUser != okabeSkyBlue {
		t.Errorf("high-contrast: colorUser = %+v, want %+v", colorUser, okabeSkyBlue)
	}
	if tv.DefaultTheme.DialogBG != tui.RGBColor(0, 0, 0) {
		t.Errorf("high-contrast: DialogBG = %+v, want black", tv.DefaultTheme.DialogBG)
	}
	// On the black high-contrast dialog the bright palette accents read well.
	if colorDialogHeader != okabeSkyBlue || colorDialogDetail != okabeOrange {
		t.Errorf("high-contrast: dialog accents = (header=%+v detail=%+v), want sky-blue/orange", colorDialogHeader, colorDialogDetail)
	}

	// Default palette restores turbotui's stock chrome — except the dropdown popup
	// highlight, which ApplyTheme now installs from the DropdownSelect roles onto
	// tv.DefaultTheme.Selection* (issue #260). The default palette's cyan highlight
	// lifts the previously bold-only popup highlight, so it intentionally diverges
	// from the stock grey Selection.
	resolved := ResolveTheme(config.ThemeConfig{}, envOf(map[string]string{"TERM": "xterm"}), false)
	ApplyTheme(resolved)
	wantDefault := baseTVTheme
	wantDefault.SelectionFG = resolved.DropdownSelectFG
	wantDefault.SelectionBG = resolved.DropdownSelectBG
	if tv.DefaultTheme != wantDefault {
		t.Errorf("default: tv.DefaultTheme = %+v, want the stock palette with the dropdown selection applied", tv.DefaultTheme)
	}
	// The stock light-grey dialog uses dark, high-contrast accents.
	if colorDialogHeader != tui.ANSIColor(5) || colorDialogDetail != tui.ANSIColor(4) {
		t.Errorf("default: dialog accents = (header=%+v detail=%+v), want ANSI 5/4", colorDialogHeader, colorDialogDetail)
	}
}

func snapshotColors() [9]tui.Color {
	return [9]tui.Color{colorUser, colorAgent, colorNote, colorTool, colorResult, colorInfo, colorError,
		colorDialogHeader, colorDialogDetail}
}

func restoreColors(c [9]tui.Color) {
	colorUser, colorAgent, colorNote, colorTool, colorResult, colorInfo, colorError =
		c[0], c[1], c[2], c[3], c[4], c[5], c[6]
	colorDialogHeader, colorDialogDetail = c[7], c[8]
}
