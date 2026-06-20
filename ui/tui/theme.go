package ui

import (
	"strconv"
	"strings"

	"gogent/internal/config"

	tui "github.com/hobbestherat/turbotui"
	tv "github.com/hobbestherat/turbotui/turbotv"
)

// Colours used across the UI. They are package-level variables (not constants)
// so a resolved theme can be installed once at startup via ApplyTheme; every
// component reads these names rather than hardcoding ANSI indices. The initial
// values are the built-in "default" palette so the UI is coloured even before a
// theme is applied (and so the existing tests have stable expectations).
var (
	colorUser   = tui.ANSIColor(14) // bright cyan
	colorAgent  = tui.ANSIColor(10) // bright green
	colorNote   = tui.ANSIColor(8)  // dim grey (thoughts)
	colorTool   = tui.ANSIColor(11) // bright yellow (tool calls)
	colorResult = tui.ANSIColor(13) // magenta (tool results)
	colorInfo   = tui.ANSIColor(12) // bright blue
	colorError  = tui.ANSIColor(9)  // bright red
)

// Chrome colours: the desktop background, sidebar panel and titles. Kept as
// variables for the same reason as the semantic colours above so NO_COLOR and
// the high-contrast preset can recolour the whole UI, not just the transcript.
var (
	chromeDesktopFG = tui.ANSIColor(7)  // hint text on the desktop
	chromeDesktopBG = tui.ANSIColor(4)  // desktop background (blue)
	chromePanelFG   = tui.ANSIColor(7)  // sidebar body text
	chromePanelBG   = tui.ANSIColor(0)  // sidebar background
	chromeTitle     = tui.ANSIColor(15) // panel titles (bright white)
	chromeDivider   = tui.ANSIColor(8)  // separators / borders
	chromeAccent    = tui.ANSIColor(11) // indicators / badges
)

// Dialog body accent colours. Modal dialogs render on turbotui's light-grey
// default chrome, where the bright transcript colours (yellow tool calls, cyan
// user text) wash out badly — the very low-contrast "yellow on light-grey" of
// issue #98. Coloured dialog lines therefore use darker, higher-contrast
// variants. ApplyTheme overrides these for the neutral (NO_COLOR) and
// high-contrast presets, whose dialogs are dark and want bright accents.
var (
	colorDialogHeader = tui.ANSIColor(5) // requester/header line (magenta on light grey)
	colorDialogDetail = tui.ANSIColor(4) // shell command / details (blue on light grey)
)

// baseTVTheme captures turbotui's stock window/dialog palette at init so a
// coloured theme can restore it after a NO_COLOR or high-contrast run has
// replaced it (ApplyTheme mutates the shared tv.DefaultTheme).
var baseTVTheme = tv.DefaultTheme

// ColorLevel describes the colour fidelity of the output terminal. A resolved
// Theme is degraded to its level so a truecolor palette still renders sensibly
// on a 256- or 16-colour terminal, and not at all when colour is disabled.
type ColorLevel int

const (
	// ColorNone disables colour entirely (terminal defaults only). It is the
	// level chosen for NO_COLOR, --no-color and non-colour terminals (TERM=dumb).
	ColorNone ColorLevel = iota
	// Color16 is the baseline 16-colour ANSI palette.
	Color16
	// Color256 is the 256-colour (8-bit) palette.
	Color256
	// ColorTrue is 24-bit truecolor (COLORTERM=truecolor|24bit).
	ColorTrue
)

// Theme is the full set of colours the TUI draws with. Built-in palettes are
// returned by paletteByName; ResolveTheme degrades a palette to the terminal's
// ColorLevel and applies any config overrides, and ApplyTheme installs it.
type Theme struct {
	// Name is the built-in palette the theme derives from ("default" or
	// "high-contrast"); it selects the matching window/dialog chrome in ApplyTheme.
	Name string
	// Level is the colour fidelity the palette has been degraded to.
	Level ColorLevel

	// Semantic transcript colours.
	User   tui.Color
	Agent  tui.Color
	Note   tui.Color
	Tool   tui.Color
	Result tui.Color
	Info   tui.Color
	Error  tui.Color

	// Chrome colours (desktop, sidebar, titles).
	DesktopFG tui.Color
	DesktopBG tui.Color
	PanelFG   tui.Color
	PanelBG   tui.Color
	Title     tui.Color
	Divider   tui.Color
	Accent    tui.Color
}

// Built-in palette names accepted in config (case-insensitive). The
// high-contrast aliases map to the same Okabe–Ito-based colourblind-safe preset.
const (
	themeDefault      = "default"
	themeHighContrast = "high-contrast"
)

// canonicalThemeName maps the configured palette name (and its accepted
// aliases) to a built-in palette, defaulting to the default palette.
func canonicalThemeName(name string) string {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "high-contrast", "high_contrast", "highcontrast", "contrast", "colorblind", "colourblind":
		return themeHighContrast
	default:
		return themeDefault
	}
}

// paletteByName returns the raw (un-degraded) built-in palette for a name. The
// default palette uses 16-colour ANSI indices; the high-contrast palette uses
// the Okabe–Ito colourblind-safe colours as truecolor RGB, degraded later.
func paletteByName(name string) Theme {
	switch canonicalThemeName(name) {
	case themeHighContrast:
		return highContrastPalette()
	default:
		return defaultPalette()
	}
}

// defaultPalette is the original hardcoded palette, now expressed as a Theme.
func defaultPalette() Theme {
	return Theme{
		Name:      themeDefault,
		User:      tui.ANSIColor(14),
		Agent:     tui.ANSIColor(10),
		Note:      tui.ANSIColor(8),
		Tool:      tui.ANSIColor(11),
		Result:    tui.ANSIColor(13),
		Info:      tui.ANSIColor(12),
		Error:     tui.ANSIColor(9),
		DesktopFG: tui.ANSIColor(7),
		DesktopBG: tui.ANSIColor(4),
		PanelFG:   tui.ANSIColor(7),
		PanelBG:   tui.ANSIColor(0),
		Title:     tui.ANSIColor(15),
		Divider:   tui.ANSIColor(8),
		Accent:    tui.ANSIColor(11),
	}
}

// Okabe–Ito colourblind-safe colours (https://jfly.uni-koeln.de/color/). Chosen
// to stay distinguishable under the common forms of colour-vision deficiency.
var (
	okabeOrange    = tui.RGBColor(0xE6, 0x9F, 0x00)
	okabeSkyBlue   = tui.RGBColor(0x56, 0xB4, 0xE9)
	okabeGreen     = tui.RGBColor(0x00, 0x9E, 0x73)
	okabeYellow    = tui.RGBColor(0xF0, 0xE4, 0x42)
	okabeBlue      = tui.RGBColor(0x00, 0x72, 0xB2)
	okabeVermilion = tui.RGBColor(0xD5, 0x5E, 0x00)
	okabePurple    = tui.RGBColor(0xCC, 0x79, 0xA7)
)

// highContrastPalette is a high-contrast, colourblind-safe preset: a pure black
// background with bright Okabe–Ito accents, so every semantic colour stays
// distinct and legible for low-vision and colour-deficient users.
func highContrastPalette() Theme {
	white := tui.RGBColor(0xFF, 0xFF, 0xFF)
	black := tui.RGBColor(0x00, 0x00, 0x00)
	grey := tui.RGBColor(0xBB, 0xBB, 0xBB)
	return Theme{
		Name:      themeHighContrast,
		User:      okabeSkyBlue,
		Agent:     okabeGreen,
		Note:      grey,
		Tool:      okabeOrange,
		Result:    okabePurple,
		Info:      okabeBlue,
		Error:     okabeVermilion,
		DesktopFG: white,
		DesktopBG: black,
		PanelFG:   white,
		PanelBG:   black,
		Title:     okabeYellow,
		Divider:   grey,
		Accent:    okabeYellow,
	}
}

// ResolveTheme builds the active Theme from the theme config and the
// environment. It selects a built-in palette, applies any config overrides,
// then degrades every colour to the terminal's detected ColorLevel — forcing
// ColorNone when NO_COLOR, the --no-color flag, or cfg.NoColor is set. env is
// the environment accessor (os.Getenv in production, a stub in tests).
func ResolveTheme(cfg config.ThemeConfig, env func(string) string, noColorFlag bool) Theme {
	level := detectColorLevel(env)
	if noColorFlag || cfg.NoColor {
		level = ColorNone
	}

	t := paletteByName(cfg.Name)
	applyOverrides(&t, cfg.Overrides)

	t.Level = level
	t.User = degrade(t.User, level)
	t.Agent = degrade(t.Agent, level)
	t.Note = degrade(t.Note, level)
	t.Tool = degrade(t.Tool, level)
	t.Result = degrade(t.Result, level)
	t.Info = degrade(t.Info, level)
	t.Error = degrade(t.Error, level)
	t.DesktopFG = degrade(t.DesktopFG, level)
	t.DesktopBG = degrade(t.DesktopBG, level)
	t.PanelFG = degrade(t.PanelFG, level)
	t.PanelBG = degrade(t.PanelBG, level)
	t.Title = degrade(t.Title, level)
	t.Divider = degrade(t.Divider, level)
	t.Accent = degrade(t.Accent, level)
	return t
}

// detectColorLevel infers the terminal's colour fidelity from the environment,
// honouring the NO_COLOR convention (https://no-color.org/): any non-empty
// NO_COLOR disables colour. A missing or "dumb" TERM is treated as no colour;
// COLORTERM=truecolor|24bit reports truecolor; a "256"-suffixed TERM reports
// 256 colours; everything else falls back to the 16-colour baseline.
func detectColorLevel(env func(string) string) ColorLevel {
	if env("NO_COLOR") != "" {
		return ColorNone
	}
	term := strings.ToLower(env("TERM"))
	if term == "" || term == "dumb" {
		return ColorNone
	}
	switch strings.ToLower(env("COLORTERM")) {
	case "truecolor", "24bit":
		return ColorTrue
	}
	if strings.Contains(term, "256") {
		return Color256
	}
	return Color16
}

// applyOverrides applies "name → colour" config overrides on top of a palette.
// Recognised names are the semantic and chrome colour fields; an unparsable
// value or unknown name is ignored so a typo degrades gracefully rather than
// breaking startup.
func applyOverrides(t *Theme, overrides map[string]string) {
	for name, spec := range overrides {
		c, ok := parseColor(spec)
		if !ok {
			continue
		}
		switch strings.ToLower(strings.TrimSpace(name)) {
		case "user":
			t.User = c
		case "agent":
			t.Agent = c
		case "note":
			t.Note = c
		case "tool":
			t.Tool = c
		case "result":
			t.Result = c
		case "info":
			t.Info = c
		case "error":
			t.Error = c
		case "desktop_fg":
			t.DesktopFG = c
		case "desktop_bg":
			t.DesktopBG = c
		case "panel_fg":
			t.PanelFG = c
		case "panel_bg":
			t.PanelBG = c
		case "title":
			t.Title = c
		case "divider":
			t.Divider = c
		case "accent":
			t.Accent = c
		}
	}
}

// parseColor parses a colour override. It accepts "#RRGGBB"/"RRGGBB" hex, a
// decimal ANSI index ("0".."255"), and "default"/"none" for the terminal
// default. It returns ok=false for anything it cannot parse.
func parseColor(spec string) (tui.Color, bool) {
	s := strings.ToLower(strings.TrimSpace(spec))
	switch s {
	case "":
		return tui.Color{}, false
	case "default", "none":
		return tui.DefaultColor(), true
	}
	hex := strings.TrimPrefix(s, "#")
	if len(hex) == 6 {
		if v, err := strconv.ParseUint(hex, 16, 32); err == nil {
			return tui.RGBColor(uint8((v>>16)&0xFF), uint8((v>>8)&0xFF), uint8(v&0xFF)), true
		}
	}
	if n, err := strconv.Atoi(s); err == nil && n >= 0 && n <= 255 {
		return tui.ANSIColor(uint8(n)), true
	}
	return tui.Color{}, false
}

// degrade reduces a colour to the fidelity the terminal supports: no colour
// becomes the terminal default, and RGB (and 256-index) colours are quantised
// down to the 256- or 16-colour palette as needed. ANSI colours already within
// the target palette pass through unchanged.
func degrade(c tui.Color, level ColorLevel) tui.Color {
	switch level {
	case ColorNone:
		return tui.DefaultColor()
	case ColorTrue:
		return c
	}
	switch c.Mode {
	case tui.ColorRGB:
		r, g, b := uint8((c.Value>>16)&0xFF), uint8((c.Value>>8)&0xFF), uint8(c.Value&0xFF)
		if level == Color256 {
			return tui.ANSIColor(rgbTo256(r, g, b))
		}
		return tui.ANSIColor(rgbTo16(r, g, b))
	case tui.ColorANSI:
		if level == Color16 && c.Value >= 16 {
			r, g, b := xterm256ToRGB(uint8(c.Value & 0xFF)) //nolint:gosec // ANSI index already in 0-255
			return tui.ANSIColor(rgbTo16(r, g, b))
		}
		return c
	default:
		return c
	}
}

// rgbTo256 maps a 24-bit colour to the nearest xterm 256-colour index, using
// the 6×6×6 colour cube (16..231) and the grayscale ramp (232..255) for greys.
func rgbTo256(r, g, b uint8) uint8 {
	if r == g && g == b {
		if r < 8 {
			return 16
		}
		if r > 248 {
			return 231
		}
		return uint8(232 + (int(r)-8)/10) //nolint:gosec // result is within 232-255
	}
	return uint8(16 + 36*cubeIndex(r) + 6*cubeIndex(g) + cubeIndex(b)) //nolint:gosec // colour-cube result is within 16-231
}

// cubeIndex maps an 8-bit channel to its 0..5 slot in the xterm colour cube.
func cubeIndex(v uint8) int {
	if v < 48 {
		return 0
	}
	if v < 115 {
		return 1
	}
	return (int(v) - 35) / 40
}

// ansi16RGB lists the canonical RGB values of the 16 ANSI colours, used to find
// the nearest 16-colour match when degrading to a 16-colour terminal.
var ansi16RGB = [16][3]uint8{
	{0, 0, 0}, {170, 0, 0}, {0, 170, 0}, {170, 85, 0},
	{0, 0, 170}, {170, 0, 170}, {0, 170, 170}, {170, 170, 170},
	{85, 85, 85}, {255, 85, 85}, {85, 255, 85}, {255, 255, 85},
	{85, 85, 255}, {255, 85, 255}, {85, 255, 255}, {255, 255, 255},
}

// rgbTo16 returns the ANSI 0..15 index whose canonical colour is closest (by
// squared Euclidean distance) to the given RGB colour.
func rgbTo16(r, g, b uint8) uint8 {
	best, bestDist := 0, 1<<31
	for i, c := range ansi16RGB {
		dr, dg, db := int(r)-int(c[0]), int(g)-int(c[1]), int(b)-int(c[2])
		dist := dr*dr + dg*dg + db*db
		if dist < bestDist {
			best, bestDist = i, dist
		}
	}
	return uint8(best)
}

// xterm256ToRGB returns the RGB value of an xterm 256-colour index, the inverse
// of rgbTo256, so a 256-index colour can be re-quantised down to 16 colours.
func xterm256ToRGB(idx uint8) (r, g, b uint8) {
	switch {
	case idx < 16:
		c := ansi16RGB[idx]
		return c[0], c[1], c[2]
	case idx >= 232:
		v := uint8(8 + (int(idx)-232)*10) //nolint:gosec // grayscale ramp result is within 8-238
		return v, v, v
	default:
		i := int(idx) - 16
		return cubeChannel(i / 36), cubeChannel((i % 36) / 6), cubeChannel(i % 6)
	}
}

// cubeChannel maps a 0..5 colour-cube slot back to its 8-bit channel value.
func cubeChannel(n int) uint8 {
	if n == 0 {
		return 0
	}
	return uint8(55 + n*40) //nolint:gosec // cube channel result is within 95-255
}

// ApplyTheme installs t as the active theme: it sets the package-level colour
// variables every TUI component reads, and reconfigures turbotui's shared
// window/dialog chrome (tv.DefaultTheme) to match — neutralised for NO_COLOR and
// switched to the high-contrast chrome for that preset. It must be called before
// the workbench (and its desktop) are constructed.
func ApplyTheme(t Theme) {
	colorUser, colorAgent, colorNote = t.User, t.Agent, t.Note
	colorTool, colorResult, colorInfo, colorError = t.Tool, t.Result, t.Info, t.Error

	// Rich-Markdown rendering tracks the active palette and the terminal's colour
	// capability (issue #184); under NO_COLOR this also auto-disables it.
	applyMarkdownPalette(t)

	chromeDesktopFG, chromeDesktopBG = t.DesktopFG, t.DesktopBG
	chromePanelFG, chromePanelBG = t.PanelFG, t.PanelBG
	chromeTitle, chromeDivider, chromeAccent = t.Title, t.Divider, t.Accent

	switch {
	case t.Level == ColorNone:
		// Neutral chrome: dialog text is the terminal default, so coloured dialog
		// lines must be too (no ANSI colour is emitted).
		tv.DefaultTheme = neutralTVTheme()
		colorDialogHeader, colorDialogDetail = tui.DefaultColor(), tui.DefaultColor()
	case t.Name == themeHighContrast:
		// High-contrast chrome is a black dialog, so the bright palette accents
		// (which would wash out on light grey) read well here.
		tv.DefaultTheme = highContrastTVTheme(t)
		colorDialogHeader, colorDialogDetail = t.User, t.Tool
	default:
		// Stock light-grey dialog: use the dark, high-contrast accents.
		tv.DefaultTheme = baseTVTheme
		colorDialogHeader, colorDialogDetail = tui.ANSIColor(5), tui.ANSIColor(4)
	}
}

// neutralTVTheme is the window/dialog chrome under NO_COLOR: every slot uses the
// terminal default so no ANSI colour is emitted for the surrounding widgets.
func neutralTVTheme() tv.Theme {
	d := tui.DefaultColor()
	return tv.Theme{
		DesktopBG: d, DesktopFG: d,
		WindowBG: d, WindowFG: d, WindowBorderFG: d, WindowBorderBG: d,
		WindowTitleFG: d, WindowTitleBG: d, WindowShadow: d,
		ButtonBG: d, ButtonFG: d, ButtonFocusBG: d, ButtonFocusFG: d, ButtonShadow: d,
		DialogBG: d, DialogFG: d, DialogBorderFG: d, DialogBorderBG: d,
		InputBG: d, InputFG: d, InputFocusBG: d, InputFocusFG: d,
		CloseButtonBG: d, CloseButtonFG: d,
		MnemonicFG: d, SelectionBG: d, SelectionFG: d,
	}
}

// highContrastTVTheme derives the window/dialog chrome for the high-contrast
// preset from t: a black canvas with white text and the palette's accent colour
// marking focus, titles and selection.
func highContrastTVTheme(t Theme) tv.Theme {
	black, white, accent, divider, err := t.PanelBG, t.PanelFG, t.Accent, t.Divider, t.Error
	return tv.Theme{
		DesktopBG: black, DesktopFG: white,
		WindowBG: black, WindowFG: white, WindowBorderFG: white, WindowBorderBG: black,
		WindowTitleFG: accent, WindowTitleBG: black, WindowShadow: divider,
		ButtonBG: black, ButtonFG: white, ButtonFocusBG: accent, ButtonFocusFG: black, ButtonShadow: divider,
		DialogBG: black, DialogFG: white, DialogBorderFG: white, DialogBorderBG: black,
		InputBG: black, InputFG: white, InputFocusBG: accent, InputFocusFG: black,
		CloseButtonBG: err, CloseButtonFG: white,
		MnemonicFG: accent, SelectionBG: accent, SelectionFG: black,
	}
}
