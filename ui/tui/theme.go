package ui

import (
	"math"
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
	colorNote   = tui.ANSIColor(7)  // light grey (thoughts/idle/disabled); see #202
	colorTool   = tui.ANSIColor(11) // bright yellow (tool calls)
	colorResult = tui.ANSIColor(13) // magenta (tool results)
	colorInfo   = tui.ANSIColor(6)  // cyan (system notes/banners); see #202
	colorError  = tui.ANSIColor(9)  // bright red
)

// Chrome colours: the desktop background, sidebar panel and titles. Kept as
// variables for the same reason as the semantic colours above so NO_COLOR and
// the high-contrast preset can recolour the whole UI, not just the transcript.
var (
	chromeDesktopFG = tui.ANSIColor(7)  // hint text on the desktop
	chromeDesktopBG = tui.ANSIColor(4)  // desktop background (blue)
	chromePanelFG   = tui.ANSIColor(7)  // sidebar body text
	chromePanelBG   = tui.ANSIColor(4)  // sidebar background (matches the desktop chrome)
	chromeTitle     = tui.ANSIColor(15) // panel titles (bright white)
	chromeDivider   = tui.ANSIColor(7)  // separators / borders (light grey; see #202)
	chromeAccent    = tui.ANSIColor(11) // indicators / badges
)

// Dropdown (tv.Select) closed-control colours (issue #260). turbotui's Select has
// no theme slot of its own, so gogent carries the resolved closed-control colours
// here — ApplyTheme installs them from the active Theme's Dropdown* roles, and
// newSelect/reseedSelect seed every Select's FG/BG/FocusFG/FocusBG from them so a
// live theme switch recolours closed dropdowns without a restart. The initial
// values are the default palette (DropdownBG == MenuBarBG, a grey bar background),
// so dropdowns are coloured before a theme is applied. The open popup's highlighted
// row is driven separately, via tv.DefaultTheme.Selection* (see ApplyTheme).
var (
	dropdownFG         = tui.ANSIColor(0) // closed control text (black on the grey bar bg)
	dropdownBG         = tui.ANSIColor(7) // closed control background (== MenuBarBG)
	dropdownFocusFG    = tui.ANSIColor(0) // focused closed control text
	dropdownFocusBG    = tui.ANSIColor(6) // focused closed control background (cyan highlight)
	dropdownDisabledFG = tui.ANSIColor(0) // greyed value of a disabled dropdown, legible on dropdownBG
)

// dropdownDisabledColor picks the foreground for a disabled dropdown's value — the
// greyed "(default)" of an effort selector whose model has no effort options
// (issue #177), painted by guardEffortSelect. Before #260 the closed control sat on
// the black InputBG, so the dim Note grey read as a legible "greyed out"; #260 moved
// it onto DropdownBG (the menu-bar background), which in the default palette is the
// same grey as Note — grey-on-grey, invisible. So the dim Note grey is used only
// while it still clears the non-text/inactive contrast floor on the closed-control
// background (the dark backgrounds of the black-canvas presets); on the light default
// control it falls back to DropdownFG. The "disabled" cue is in any case also carried
// by the greyed effort label, which sits on the window background where Note reads.
//
// The result is legible by construction, so it needs no separate paletteContrast
// finding: the Note branch is taken only when it clears minContrastLarge here, and the
// DropdownFG fallback is the foreground paletteContrast already audits as the
// "dropdown" pair (DropdownFG on DropdownBG, minContrastText), so a palette change that
// broke the disabled value would already trip that finding. Inactive controls are in
// any case exempt from the AA body-text minimum (WCAG 1.4.3).
func dropdownDisabledColor(t Theme) tui.Color {
	if contrastRatio(t.Note, t.DropdownBG) >= minContrastLarge {
		return t.Note
	}
	return t.DropdownFG
}

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

// shadowsEnabled reports whether gogent's surfaces draw drop shadows. It tracks
// the active theme's NoShadow setting (issue #215): ApplyTheme sets it from the
// resolved Theme, and every surface gogent builds — session/monologue windows,
// dialogs, the menu bar and buttons — seeds its turbotui Shadow flag from it via
// the applyWindowShadow/applyButtonShadow/applyMenuBarShadow helpers and the
// newButton wrapper. It defaults to true so shadows are on until a theme is
// applied (matching the pre-#215 look and the existing tests).
var shadowsEnabled = true

// applyWindowShadow seeds a window's (or dialog window's) drop shadow from the
// active NoShadow preference. Call it after tv.NewWindow/tv.NewDialog and from
// the live theme-apply path (SessionWindow.refreshTheme) so a toggle re-applies
// without a restart (issue #215).
func applyWindowShadow(w *tv.Window) {
	if w != nil {
		w.Shadow = shadowsEnabled
	}
}

// applyButtonShadow seeds a button's drop shadow from the active NoShadow
// preference. newButton applies it at construction and reseedButton on the live
// theme-apply path (issue #215).
func applyButtonShadow(b *tv.Button) {
	if b != nil {
		b.Shadow = shadowsEnabled
	}
}

// applyMenuBarShadow seeds the desktop menu bar's drop shadow from the active
// NoShadow preference. The menu bar is rebuilt by RefreshTheme, so this also
// covers the live theme-apply path (issue #215).
func applyMenuBarShadow(m *tv.MenuBar) {
	if m != nil {
		m.Shadow = shadowsEnabled
	}
}

// applySelectShadow seeds a Select's dropdown-popup drop shadow from the active
// NoShadow preference. newSelect applies it at construction and reseedSelect on
// the live theme-apply path, so the #215 toggle reaches Select popups too — they
// were previously the one chrome element that always cast a shadow (issue #231).
func applySelectShadow(s *tv.Select) {
	if s != nil {
		s.Shadow = shadowsEnabled
	}
}

// newButton constructs a turbotui button and seeds its drop shadow from the
// active NoShadow preference (issue #215). gogent builds every button through
// this wrapper so the shadow toggle reaches dialog and session buttons alike
// without touching each construction site.
func newButton(label string, bounds tv.Rect, onPress func()) *tv.Button {
	b := tv.NewButton(label, bounds, onPress)
	applyButtonShadow(b)
	return b
}

// newSelect constructs a turbotui Select, seeds its closed-control colours from the
// active dropdown roles (issue #260) and its dropdown-popup drop shadow from the
// active NoShadow preference (issue #231). gogent builds every selector through
// this wrapper so the dropdown palette and the shadow toggle reach every combo box
// — session selectors, dialog selects and the sidebar — without touching each
// construction site, mirroring newButton. turbotui's NewSelect seeds the closed
// control from the Input* slots; reseedSelect re-applies the same dropdown roles on
// the live theme-apply path so a theme switch recolours an already-built Select.
func newSelect(desktop *tv.Desktop, options []string, bounds tv.Rect) *tv.Select {
	s := tv.NewSelect(desktop, options, bounds)
	s.FG, s.BG, s.FocusFG, s.FocusBG = dropdownFG, dropdownBG, dropdownFocusFG, dropdownFocusBG
	applySelectShadow(s)
	return s
}

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
	// NoShadow suppresses every drop shadow when true (issue #215). It is carried
	// here (not just in config) so ApplyTheme can install it onto shadowsEnabled
	// alongside the colours, keeping the live theme-apply path the single source.
	NoShadow bool

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

	// Menu bar colours (issue #243). The always-on-top menu bar is a surface the
	// user can see but, before #243, could not recolour: the default preset
	// inherited turbotui's stock grey bar and the black-canvas presets derived the
	// bar from PanelFG/PanelBG inside blackCanvasTVTheme. They are now first-class
	// roles so the bar is overridable like any other colour; ApplyTheme installs
	// them onto tv.DefaultTheme's MenuBar* slots.
	MenuBarFG tui.Color
	MenuBarBG tui.Color

	// Dropdown (tv.Select) colours (issue #260). The combo boxes — the per-session
	// Model/Effort selectors and the selects in dialogs and the sidebar — had no
	// dedicated roles before #260: the closed control borrowed the Input* colours
	// (a dark ANSI-0 box) and the open popup the Dialog*/Selection* chrome, so a
	// closed dropdown read as a quiet input rather than prominent chrome on par with
	// the menu bar. They are now first-class roles, defaulting DropdownBG to MenuBarBG
	// so a closed dropdown carries the bar's background.
	//
	// DropdownFG/DropdownBG colour the closed control and DropdownFocusFG/
	// DropdownFocusBG the focused closed control; both are carried in package vars by
	// ApplyTheme and seeded onto every Select by newSelect/reseedSelect (turbotui's
	// Select has no theme slot of its own). DropdownSelectFG/DropdownSelectBG colour
	// the highlighted row in the open popup; ApplyTheme installs them onto
	// tv.DefaultTheme's Selection* slots, which drawPopup reads at draw time.
	DropdownFG       tui.Color
	DropdownBG       tui.Color
	DropdownFocusFG  tui.Color
	DropdownFocusBG  tui.Color
	DropdownSelectFG tui.Color
	DropdownSelectBG tui.Color

	// CodeBG is the background painted behind fenced/indented code blocks in the
	// rich-Markdown transcript (issue #184). It is a theme role, not a hardcoded
	// black, so code blocks read as part of the active theme rather than a black
	// island (issue #200). Whether it is painted at all is decided per palette in
	// applyMarkdownPalette (the pure-black high-contrast preset suppresses it).
	CodeBG tui.Color
}

// Built-in palette names accepted in config (case-insensitive). The
// high-contrast aliases map to the same Okabe–Ito-based colourblind-safe preset;
// the dark aliases map to the plain black-background "dark" preset (issue #200).
const (
	themeDefault      = "default"
	themeHighContrast = "high-contrast"
	themeDark         = "dark"
)

// canonicalThemeName maps the configured palette name (and its accepted
// aliases) to a built-in palette, defaulting to the default palette.
func canonicalThemeName(name string) string {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "high-contrast", "high_contrast", "highcontrast", "contrast", "colorblind", "colourblind":
		return themeHighContrast
	case "dark", "midnight", "black":
		return themeDark
	default:
		return themeDefault
	}
}

// paletteByName returns the raw (un-degraded) built-in palette for a name. The
// default palette uses 16-colour ANSI indices; the high-contrast and dark
// palettes use truecolor RGB (degraded later) for their black backgrounds.
func paletteByName(name string) Theme {
	switch canonicalThemeName(name) {
	case themeHighContrast:
		return highContrastPalette()
	case themeDark:
		return darkPalette()
	default:
		return defaultPalette()
	}
}

// defaultPalette is the original hardcoded palette, now expressed as a Theme.
// Every foreground is contrast-checked against the background it is actually drawn
// on — the blue (ANSI 4) window/panel/desktop chrome — by paletteContrast, and the
// roles below were chosen so each clears the documented minimum (issue #202). The
// dim-grey note (ANSI 8) and bright-blue info (ANSI 12) of earlier revisions were
// ~1.8:1 and ~2.6:1 on blue and have been recoloured to light grey and cyan.
func defaultPalette() Theme {
	return Theme{
		Name:  themeDefault,
		User:  tui.ANSIColor(14),
		Agent: tui.ANSIColor(10),
		// Light grey, not the old dim grey (ANSI 8): the dim/secondary/disabled role
		// must still read on the blue window (1.78:1 → 5.7:1).
		Note:   tui.ANSIColor(7),
		Tool:   tui.ANSIColor(11),
		Result: tui.ANSIColor(13),
		// Cyan, not the old bright blue (ANSI 12): the nearest readable cool hue to
		// the original blue, distinct from the grey note and the bright-cyan user
		// (2.61:1 → 4.64:1 on the blue window). System notes/banners use it.
		Info:      tui.ANSIColor(6),
		Error:     tui.ANSIColor(9),
		DesktopFG: tui.ANSIColor(7),
		DesktopBG: tui.ANSIColor(4),
		PanelFG:   tui.ANSIColor(7),
		// The sidebar shares the blue desktop chrome rather than being a black
		// island (issue #200); the divider glyph and title still delineate it.
		PanelBG: tui.ANSIColor(4),
		Title:   tui.ANSIColor(15),
		// Light grey, not the old dim grey (ANSI 8): faint borders on blue (1.78:1)
		// are bumped to a visible 5.7:1 (issue #202).
		Divider: tui.ANSIColor(7),
		Accent:  tui.ANSIColor(11),
		// Stock turbotui menu bar: black text on a light-grey bar (the values
		// baseTVTheme carries), expressed as roles so the default preset's bar is
		// editable too rather than silently inheriting the library default.
		MenuBarFG: tui.ANSIColor(0),
		MenuBarBG: tui.ANSIColor(7),
		// Dropdowns (issue #260): the closed control carries the menu bar's grey
		// background (DropdownBG == MenuBarBG) with black text, so it reads as
		// prominent chrome rather than the old dark ANSI-0 input box. The focused
		// control and the open popup's highlighted row use cyan (ANSI 6) — the
		// palette's existing focus colour (InputFocusBG/ButtonFocusBG) — so they stand
		// out from the grey closed control, stay distinct from the ANSI-4 blue window,
		// and match the rest of the theme's focus treatment.
		DropdownFG:       tui.ANSIColor(0),
		DropdownBG:       tui.ANSIColor(7),
		DropdownFocusFG:  tui.ANSIColor(0),
		DropdownFocusBG:  tui.ANSIColor(6),
		DropdownSelectFG: tui.ANSIColor(0),
		DropdownSelectBG: tui.ANSIColor(6),
		// Fenced-code panel: a dark navy inset — a subtle shade of the desktop blue
		// (issue #200) so code reads as a distinct themed panel rather than the old
		// black-on-blue island. It is the one RGB role in this otherwise 16-colour
		// palette: on truecolor/256 it is a dim blue distinct from the ANSI-4 window
		// background, and on a 16-colour terminal it degrades to black (ANSI 0) —
		// still distinct from the blue window and clear of the grey comment foreground
		// (ANSI 8), so every token stays legible.
		CodeBG: tui.RGBColor(0x10, 0x14, 0x50),
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
		// White-on-black menu bar, matching the black canvas (it equals PanelFG /
		// PanelBG so the bar reads as part of the surrounding chrome).
		MenuBarFG: white,
		MenuBarBG: black,
		// Dropdowns (issue #260): the closed control matches the menu bar (white on
		// black, == MenuBar*/PanelFG/PanelBG) so it reads as part of the black canvas,
		// and the focused control and open highlighted row use the bright yellow accent
		// for an unmistakable high-contrast highlight (black on yellow). The accent
		// equals the black-canvas SelectionBG, so the popup-highlight install is a no-op.
		DropdownFG:       white,
		DropdownBG:       black,
		DropdownFocusFG:  black,
		DropdownFocusBG:  okabeYellow,
		DropdownSelectFG: black,
		DropdownSelectBG: okabeYellow,
		// Unused: applyMarkdownPalette suppresses the code background for this
		// pure-black preset (a black panel on black would vanish), but the role is
		// set for completeness so the Theme is fully populated.
		CodeBG: black,
	}
}

// darkPalette is a plain black-background dark theme (issue #200): an easy-on-the
// -eye look with a cohesive, muted palette — cool blues and greens for prose and
// the semantic roles, a warm amber accent, and soft whites for chrome. Unlike the
// high-contrast preset it prioritises aesthetic cohesion over accessibility-max,
// so the colours are desaturated rather than maximally bright. Expressed as
// truecolor RGB and degraded to the terminal's fidelity by ResolveTheme.
func darkPalette() Theme {
	black := tui.RGBColor(0x00, 0x00, 0x00)
	softWhite := tui.RGBColor(0xD6, 0xD6, 0xD6)
	title := tui.RGBColor(0xEC, 0xEC, 0xEC)
	dimGrey := tui.RGBColor(0x80, 0x80, 0x80)
	divider := tui.RGBColor(0x5A, 0x5A, 0x5A)
	amber := tui.RGBColor(0xE0, 0xAF, 0x68) // accent tone, shared by Tool/Accent
	return Theme{
		Name:      themeDark,
		User:      tui.RGBColor(0x7D, 0xCF, 0xE6), // soft cyan
		Agent:     tui.RGBColor(0x9E, 0xCE, 0x6A), // muted green
		Note:      dimGrey,                        // thoughts
		Tool:      tui.RGBColor(0xE0, 0xAF, 0x68), // warm amber
		Result:    tui.RGBColor(0xC6, 0x8F, 0xD6), // soft mauve
		Info:      tui.RGBColor(0x7A, 0xA2, 0xF7), // muted periwinkle
		Error:     tui.RGBColor(0xE0, 0x6C, 0x75), // muted red
		DesktopFG: softWhite,
		DesktopBG: black,
		PanelFG:   softWhite,
		PanelBG:   black,
		Title:     title,
		Divider:   divider,
		Accent:    tui.RGBColor(0xE0, 0xAF, 0x68), // amber, matching the tool tone
		// Soft-white-on-dark-grey menu bar: a #262626 panel lifts the bar off the
		// pure-black canvas while staying cohesive with it.
		MenuBarFG: softWhite,
		MenuBarBG: tui.RGBColor(0x26, 0x26, 0x26),
		// Dropdowns (issue #260): the closed control matches the menu bar background
		// (soft white on the #262626 bar) so it sits cohesively on the dark canvas; the
		// focused control and open highlighted row use the amber accent (black on amber)
		// for a warm, legible highlight. DropdownBG tracks MenuBarBG, which is #262626
		// here (baked by the dark preset), not pure black.
		DropdownFG:       softWhite,
		DropdownBG:       tui.RGBColor(0x26, 0x26, 0x26), // == MenuBarBG
		DropdownFocusFG:  black,
		DropdownFocusBG:  amber,
		DropdownSelectFG: black,
		DropdownSelectBG: amber,
		// Code blocks render directly on the pure-black canvas (no distinct panel).
		CodeBG: black,
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

	t.NoShadow = cfg.NoShadow
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
	t.MenuBarFG = degrade(t.MenuBarFG, level)
	t.MenuBarBG = degrade(t.MenuBarBG, level)
	t.DropdownFG = degrade(t.DropdownFG, level)
	t.DropdownBG = degrade(t.DropdownBG, level)
	t.DropdownFocusFG = degrade(t.DropdownFocusFG, level)
	t.DropdownFocusBG = degrade(t.DropdownFocusBG, level)
	t.DropdownSelectFG = degrade(t.DropdownSelectFG, level)
	t.DropdownSelectBG = degrade(t.DropdownSelectBG, level)
	t.CodeBG = degrade(t.CodeBG, level)
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
		case "menu_bar_fg":
			t.MenuBarFG = c
		case "menu_bar_bg":
			t.MenuBarBG = c
		case "dropdown_fg":
			t.DropdownFG = c
		case "dropdown_bg":
			t.DropdownBG = c
		case "dropdown_focus_fg":
			t.DropdownFocusFG = c
		case "dropdown_focus_bg":
			t.DropdownFocusBG = c
		case "dropdown_select_fg":
			t.DropdownSelectFG = c
		case "dropdown_select_bg":
			t.DropdownSelectBG = c
		case "code_bg":
			t.CodeBG = c
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

// --- Contrast audit (issue #202) ---------------------------------------------
//
// Every transcript/chrome foreground is drawn on a concrete background (the blue
// ANSI-4 window/panel/desktop chrome in the default palette). The helpers below
// compute the WCAG 2.x contrast ratio of each pairing so a palette change cannot
// silently reintroduce a low-contrast pair like the bright-blue-on-blue system
// note this issue fixed.

const (
	// minContrastText is the WCAG 2.x AA contrast target for normal-weight body
	// text (4.5:1). It is the bar transcript message and note text are held to.
	minContrastText = 4.5
	// minContrastLarge is the WCAG 2.x AA threshold for large or bold text and for
	// non-text UI components such as borders and indicators (3:1). It is the floor
	// every default-palette role must clear. It also bounds the one transcript role
	// the 16-colour gamut cannot lift to minContrastText: the error red is ANSI 9,
	// the reddest hue available, and reaches only 4.23:1 on the blue window — a
	// purer or darker red scores worse, and a lighter salmon degrades straight back
	// to ANSI 9 on a 16-colour terminal. That 4.23:1 falls short of the body-text
	// target (error is painted on non-bold body lines, not only bold headers), but
	// clears this floor comfortably, so it is accepted as the documented gamut limit
	// rather than abandoning the red hue the role depends on.
	minContrastLarge = 3.0
)

// colorRGB resolves a colour to 8-bit RGB for luminance maths, returning ok=false
// for the terminal default (whose real value the program cannot know). ANSI
// indices are mapped through the same canonical table degrade uses, so the audit
// measures the colours a 16-/256-colour terminal actually shows.
func colorRGB(c tui.Color) (r, g, b uint8, ok bool) {
	switch c.Mode {
	case tui.ColorRGB:
		return uint8((c.Value >> 16) & 0xFF), uint8((c.Value >> 8) & 0xFF), uint8(c.Value & 0xFF), true
	case tui.ColorANSI:
		r, g, b = xterm256ToRGB(uint8(c.Value & 0xFF)) //nolint:gosec // ANSI index already in 0-255
		return r, g, b, true
	default:
		return 0, 0, 0, false
	}
}

// relativeLuminance returns the WCAG relative luminance (0..1) of an sRGB colour
// (https://www.w3.org/TR/WCAG21/#dfn-relative-luminance).
func relativeLuminance(r, g, b uint8) float64 {
	lin := func(v uint8) float64 {
		s := float64(v) / 255
		if s <= 0.03928 {
			return s / 12.92
		}
		return math.Pow((s+0.055)/1.055, 2.4)
	}
	return 0.2126*lin(r) + 0.7152*lin(g) + 0.0722*lin(b)
}

// contrastRatio returns the WCAG 2.x contrast ratio (1.0..21.0) between a
// foreground and background colour (https://www.w3.org/TR/WCAG21/#dfn-contrast-ratio).
// A pairing that involves the terminal default — whose true colour is unknowable —
// returns 0, which no threshold accepts, so the audit never silently passes an
// undeterminable pair.
func contrastRatio(fg, bg tui.Color) float64 {
	fr, fgc, fb, ok1 := colorRGB(fg)
	br, bgc, bb, ok2 := colorRGB(bg)
	if !ok1 || !ok2 {
		return 0
	}
	l1 := relativeLuminance(fr, fgc, fb)
	l2 := relativeLuminance(br, bgc, bb)
	if l1 < l2 {
		l1, l2 = l2, l1
	}
	return (l1 + 0.05) / (l2 + 0.05)
}

// contrastFinding is one (role → background) pairing produced by paletteContrast:
// the foreground the UI paints for that role, the background it is drawn on, the
// measured WCAG ratio and the minimum that pairing must meet.
type contrastFinding struct {
	Role  string
	FG    tui.Color
	BG    tui.Color
	Ratio float64
	Min   float64
}

// OK reports whether the pairing meets its required minimum contrast.
func (f contrastFinding) OK() bool { return f.Ratio >= f.Min }

// paletteContrast audits a resolved theme for legibility (issue #202). It returns
// one finding per foreground the UI actually paints, each checked against the
// background it is really rendered on: the transcript and status-line semantic
// colours against windowBG, the sidebar body/title/divider/indicator chrome
// against the theme's own panel background, and the desktop hint against the
// desktop background. Pass the live window/content background for windowBG — for
// the default theme that is tv.DefaultTheme.WindowBG (ANSI 4, blue); the
// black-canvas presets render the transcript on their PanelBG, so pass that.
//
// Audit a theme already degraded to the terminal's ColorLevel (the ResolveTheme
// output), not a raw palette: the default palette is authored entirely in 16-colour
// ANSI indices and so is fidelity-invariant, but an RGB preset can quantise to a
// different, lower-contrast index at 16 or 256 colours, so auditing its raw
// truecolor form over-reports the contrast a real terminal renders.
//
// Body-text roles are held to minContrastText; the gamut-limited error role and
// the non-text border and indicator roles to minContrastLarge. Asserting every
// finding's OK() guarantees no role has regressed below its threshold.
func paletteContrast(t Theme, windowBG tui.Color) []contrastFinding {
	finding := func(role string, fg, bg tui.Color, min float64) contrastFinding {
		return contrastFinding{Role: role, FG: fg, BG: bg, Ratio: contrastRatio(fg, bg), Min: min}
	}
	return []contrastFinding{
		finding("user", t.User, windowBG, minContrastText),
		finding("agent", t.Agent, windowBG, minContrastText),
		finding("note", t.Note, windowBG, minContrastText),
		finding("tool", t.Tool, windowBG, minContrastText),
		finding("result", t.Result, windowBG, minContrastText),
		finding("info", t.Info, windowBG, minContrastText),
		// The error red is the one transcript role the 16-colour gamut pins below
		// minContrastText: ANSI 9 is the reddest hue available and reaches 4.23:1 on
		// the blue window, and nothing redder does better (a purer/darker red is
		// worse; a lighter salmon degrades back to ANSI 9 on a 16-colour terminal).
		// It is painted on non-bold body lines too (the budget note, error records),
		// so this is a genuine sub-AA-body pairing — but keeping the red hue is worth
		// more than the last 0.27 of ratio, and it stays well clear of the 3:1
		// non-text/large floor. Held to minContrastLarge as the documented gamut
		// limit, not silently certified at the body tier it cannot reach.
		finding("error", t.Error, windowBG, minContrastLarge),
		finding("desktop-hint", t.DesktopFG, t.DesktopBG, minContrastText),
		finding("panel-body", t.PanelFG, t.PanelBG, minContrastText),
		finding("panel-title", t.Title, t.PanelBG, minContrastText),
		// Borders and indicators are non-text UI components (3:1 tier).
		finding("divider", t.Divider, t.PanelBG, minContrastLarge),
		finding("accent", t.Accent, t.PanelBG, minContrastLarge),
		// Dropdown roles (issue #260) carry their own backgrounds, so each is audited
		// against its actual fill rather than windowBG: the closed control's text on
		// the menu-bar-background fill, the focused control's text on its highlight,
		// and the open popup's highlighted-row text on the select highlight. They paint
		// label text, so all three are held to the body-text tier — a palette change
		// can't silently reintroduce a low-contrast dropdown pair (#202).
		finding("dropdown", t.DropdownFG, t.DropdownBG, minContrastText),
		finding("dropdown-focus", t.DropdownFocusFG, t.DropdownFocusBG, minContrastText),
		finding("dropdown-select", t.DropdownSelectFG, t.DropdownSelectBG, minContrastText),
	}
}

// ApplyTheme installs t as the active theme: it sets the package-level colour
// variables every TUI component reads, and reconfigures turbotui's shared
// window/dialog chrome (tv.DefaultTheme) to match — neutralised for NO_COLOR and
// switched to the high-contrast chrome for that preset. It must be called before
// the workbench (and its desktop) are constructed.
func ApplyTheme(t Theme) {
	// Drop-shadow preference (issue #215): installed alongside the colours so the
	// runtime theme-apply path is the single place the whole UI re-reads. Surfaces
	// built afterwards (and re-skinned by RefreshTheme) seed their Shadow flag from
	// this via the applyWindowShadow/applyButtonShadow/applyMenuBarShadow helpers.
	shadowsEnabled = !t.NoShadow

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
	case t.Name == themeHighContrast || t.Name == themeDark:
		// The high-contrast and dark presets both have a black canvas, so they use a
		// black dialog chrome where the palette accents (which would wash out on the
		// stock light grey) read well.
		tv.DefaultTheme = blackCanvasTVTheme(t)
		colorDialogHeader, colorDialogDetail = t.User, t.Tool
	default:
		// Stock light-grey dialog: use the dark, high-contrast accents.
		tv.DefaultTheme = baseTVTheme
		colorDialogHeader, colorDialogDetail = tui.ANSIColor(5), tui.ANSIColor(4)
	}

	// Menu bar colours are first-class theme roles (issue #243), so install them
	// over whichever chrome the switch selected — including the stock light-grey
	// chrome of the default preset, whose bar would otherwise stay on turbotui's
	// library default and ignore an override. MenuHotBG follows the bar background
	// so the hot-key cell stays flush with the bar. Under NO_COLOR both colours
	// have degraded to the terminal default, leaving the neutral bar untouched.
	tv.DefaultTheme.MenuBarFG = t.MenuBarFG
	tv.DefaultTheme.MenuBarBG = t.MenuBarBG
	tv.DefaultTheme.MenuHotBG = t.MenuBarBG

	// Dropdown roles (issue #260). The closed-control colours have no slot in
	// turbotui's Theme, so they are carried in package vars that newSelect and
	// reseedSelect seed every Select from; install them here so freshly built and
	// live-reseeded dropdowns follow the active palette. The open popup's highlighted
	// row reads tv.DefaultTheme.Selection* at draw time (turbotui widget_select.go
	// drawPopup), so install DropdownSelect* there — the popup body keeps the dialog
	// chrome, but its highlight follows the dropdown palette. For the black-canvas
	// presets DropdownSelectBG equals the accent the chrome already uses for Selection,
	// so this is a no-op there; under NO_COLOR both have degraded to the terminal
	// default, leaving the neutral selection untouched.
	dropdownFG, dropdownBG = t.DropdownFG, t.DropdownBG
	dropdownFocusFG, dropdownFocusBG = t.DropdownFocusFG, t.DropdownFocusBG
	dropdownDisabledFG = dropdownDisabledColor(t)
	tv.DefaultTheme.SelectionFG = t.DropdownSelectFG
	tv.DefaultTheme.SelectionBG = t.DropdownSelectBG

	// Keep turbotui's active chrome theme in lockstep with the dialog chrome above.
	// turbotui widgets (windows, the menu bar, labels, selects, buttons, inputs)
	// seed their colours from tv.ActiveTheme() at construction, and the desktop and
	// menu resolve it at draw time — but only tv.SetTheme updates it; assigning
	// tv.DefaultTheme alone (which only gogent's own dialogs read) leaves the rest
	// on the stock palette. Installing it here means freshly built widgets, the
	// rebuilt menu bar and the re-seeded open session windows (RefreshTheme) all
	// draw in the active palette, so a live theme switch matches a restart (#204).
	tv.SetTheme(tv.DefaultTheme)
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

// blackCanvasTVTheme derives the window/dialog chrome for a black-background
// preset (high-contrast or dark) from t: a black canvas with the palette's
// foreground text and its accent colour marking focus, titles and selection.
func blackCanvasTVTheme(t Theme) tv.Theme {
	black, white, accent, divider, err := t.PanelBG, t.PanelFG, t.Accent, t.Divider, t.Error
	return tv.Theme{
		DesktopBG: black, DesktopFG: white,
		WindowBG: black, WindowFG: white, WindowBorderFG: white, WindowBorderBG: black,
		WindowTitleFG: accent, WindowTitleBG: black, WindowShadow: divider,
		ButtonBG: black, ButtonFG: white, ButtonFocusBG: accent, ButtonFocusFG: black, ButtonShadow: divider,
		DialogBG: black, DialogFG: white, DialogBorderFG: white, DialogBorderBG: black,
		InputBG: black, InputFG: white, InputFocusBG: accent, InputFocusFG: black,
		CloseButtonBG: err, CloseButtonFG: white,
		MnemonicFG: accent, DialogMnemonicFG: accent, SelectionBG: accent, SelectionFG: black,
		// Menu chrome: the menubar and dropdowns are drawn over the black canvas, so
		// they need explicit colours too — otherwise they fall back to the terminal
		// default and read as low-contrast/invisible (issue #200). The bar colours come
		// from the (overridable) MenuBar* roles (issue #243); for a pristine
		// black-canvas preset they equal PanelFG/PanelBG. The accent marks the hot key
		// and the selected row.
		MenuBarFG: t.MenuBarFG, MenuBarBG: t.MenuBarBG,
		MenuHotFG: accent, MenuHotBG: t.MenuBarBG,
		MenuSelectFG: black, MenuSelectBG: accent, MenuShadow: divider,
	}
}
