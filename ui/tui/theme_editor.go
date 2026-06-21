package ui

import (
	"fmt"
	"strconv"

	"gogent/internal/config"

	tui "github.com/hobbestherat/turbotui"
	tv "github.com/hobbestherat/turbotui/turbotv"
)

// themeRole describes one editable colour role: the config-override key (matching
// the names applyOverrides understands), its UI label, and an accessor for the
// role's colour in a resolved Theme.
type themeRole struct {
	key   string
	label string
	get   func(Theme) tui.Color
}

// themeRoles is the ordered list of roles the editor exposes: the seven semantic
// transcript colours followed by the chrome colours. The keys must match the
// names parsed in applyOverrides so a saved override round-trips on next launch.
var themeRoles = []themeRole{
	{"user", "User", func(t Theme) tui.Color { return t.User }},
	{"agent", "Agent", func(t Theme) tui.Color { return t.Agent }},
	{"note", "Note", func(t Theme) tui.Color { return t.Note }},
	{"tool", "Tool", func(t Theme) tui.Color { return t.Tool }},
	{"result", "Result", func(t Theme) tui.Color { return t.Result }},
	{"info", "Info", func(t Theme) tui.Color { return t.Info }},
	{"error", "Error", func(t Theme) tui.Color { return t.Error }},
	{"desktop_fg", "Desktop FG", func(t Theme) tui.Color { return t.DesktopFG }},
	{"desktop_bg", "Desktop BG", func(t Theme) tui.Color { return t.DesktopBG }},
	{"panel_fg", "Panel FG", func(t Theme) tui.Color { return t.PanelFG }},
	{"panel_bg", "Panel BG", func(t Theme) tui.Color { return t.PanelBG }},
	{"title", "Title", func(t Theme) tui.Color { return t.Title }},
	{"divider", "Divider", func(t Theme) tui.Color { return t.Divider }},
	{"accent", "Accent", func(t Theme) tui.Color { return t.Accent }},
	{"code_bg", "Code BG", func(t Theme) tui.Color { return t.CodeBG }},
}

// themePresets are the selectable built-in palettes shown in the editor's preset
// dropdown, paired with their canonical config name.
var themePresets = []struct {
	label string
	name  string
}{
	{"Default", themeDefault},
	{"High-contrast (Okabe–Ito)", themeHighContrast},
	{"Dark (black background)", themeDark},
}

// colorSpec renders a colour as an editable spec string, the inverse of
// parseColor: "default" for the terminal default, "#RRGGBB" for an RGB colour
// and the decimal index for an ANSI colour.
func colorSpec(c tui.Color) string {
	switch c.Mode {
	case tui.ColorRGB:
		return fmt.Sprintf("#%06X", c.Value&0xFFFFFF)
	case tui.ColorANSI:
		return strconv.Itoa(int(c.Value))
	default:
		return "default"
	}
}

// editedTheme builds the (un-degraded) Theme the editor currently shows: the
// selected preset palette with the config overrides applied on top. It is what
// the field spec strings are seeded from when the dialog opens or the preset
// changes.
func editedTheme(cfg config.ThemeConfig) Theme {
	t := paletteByName(cfg.Name)
	applyOverrides(&t, cfg.Overrides)
	return t
}

// buildThemeConfig assembles a ThemeConfig from the editor's state: the chosen
// preset, the NO_COLOR and NO_SHADOW toggles, and the per-role spec strings. It
// records an override only for roles whose spec parses to a colour different from
// the preset's built-in value, so a pristine preset (or a field left at its
// default) is saved without redundant overrides. Unparseable specs are ignored,
// leaving the preset's colour for that role.
func buildThemeConfig(preset string, noColor, noShadow bool, specs map[string]string) config.ThemeConfig {
	cfg := config.ThemeConfig{Name: canonicalThemeName(preset), NoColor: noColor, NoShadow: noShadow}
	base := paletteByName(cfg.Name)
	overrides := map[string]string{}
	for _, role := range themeRoles {
		spec, ok := specs[role.key]
		if !ok {
			continue
		}
		c, ok := parseColor(spec)
		if !ok || c == role.get(base) {
			continue
		}
		overrides[role.key] = spec
	}
	if len(overrides) > 0 {
		cfg.Overrides = overrides
	}
	return cfg
}

// presetIndex returns the themePresets index for a canonical palette name,
// defaulting to 0 (the default palette).
func presetIndex(name string) int {
	canon := canonicalThemeName(name)
	for i, p := range themePresets {
		if p.name == canon {
			return i
		}
	}
	return 0
}

// showThemeEditor opens the modal theme editor (issue #103). A preset dropdown
// picks a built-in palette; each colour role has a spec field (ANSI index,
// #RRGGBB hex, or "default") and a live swatch that recolours when the field is
// committed (Enter), the preset changes, or colour is toggled off. Save persists
// the palette as the preferred theme and re-applies it to the live UI; Reset
// restores the default palette.
func (w *Workbench) showThemeEditor() {
	if w.handlers.GetTheme == nil || w.handlers.SetTheme == nil {
		w.showConfirm("Theme", "Theme editing is unavailable.", nil)
		return
	}
	cur := w.handlers.GetTheme()

	const width = 72
	const height = 15
	x, y := centeredDialog(w, width, height)

	dialog := tv.NewDialog("Theme", x, y, width, height)
	applyWindowShadow(dialog.Window) // honour the NoShadow theme setting (issue #215)
	dialog.Window.ShowClose = false

	const sample = "▉▉ Aa"

	// refresh is wired into the widgets below; it is assigned after they exist.
	var refresh func()

	dialog.Window.AddContent(dialogLabel("Preset:", tv.Rect{X: 2, Y: 1, W: 8, H: 1}))
	presetLabels := make([]string, len(themePresets))
	for i, p := range themePresets {
		presetLabels[i] = p.label
	}
	preset := tv.NewSelect(w.desktop, presetLabels, tv.Rect{X: 10, Y: 1, W: 30, H: 1})
	dialog.Window.AddContent(preset)

	noColor := tv.NewCheckbox("Disable &colours", tv.Rect{X: 44, Y: 1, W: width - 48, H: 1}, func(bool) { refresh() })
	noColor.FG = tv.DefaultTheme.DialogFG
	noColor.BG = tv.DefaultTheme.DialogBG
	dialog.Window.AddContent(noColor)

	// Disable-shadows toggle (issue #215), stacked under the colour toggle. It does
	// not affect the colour swatches, so it needs no refresh on change; its state is
	// read at Save and persisted as ThemeConfig.NoShadow.
	noShadow := tv.NewCheckbox("Disable &shadows", tv.Rect{X: 44, Y: 2, W: width - 48, H: 1}, nil)
	noShadow.FG = tv.DefaultTheme.DialogFG
	noShadow.BG = tv.DefaultTheme.DialogBG
	dialog.Window.AddContent(noShadow)

	// One row per role across two columns; fields[i] edits themeRoles[i] and
	// swatches[i] previews it. The left column holds the seven semantic colours,
	// the right column the seven chrome colours.
	fields := make([]*tv.TextBox, len(themeRoles))
	swatches := make([]*tv.Label, len(themeRoles))
	half := (len(themeRoles) + 1) / 2
	for i, role := range themeRoles {
		col, row := 0, 3+i
		if i >= half {
			col, row = 1, 3+i-half
		}
		lx := 2 + col*34
		dialog.Window.AddContent(dialogLabel(role.label+":", tv.Rect{X: lx, Y: row, W: 11, H: 1}))
		box := tv.NewTextBox("", tv.Rect{X: lx + 11, Y: row, W: 10, H: 1})
		box.OnSubmit = func() { refresh() }
		dialog.Window.AddContent(box)
		fields[i] = box
		sw := tv.NewLabel(sample, tv.Rect{X: lx + 22, Y: row, W: 9, H: 1})
		sw.BG = tv.DefaultTheme.DialogBG
		dialog.Window.AddContent(sw)
		swatches[i] = sw
	}

	dialog.Window.AddContent(dialogLabel(
		"Spec: ANSI 0–255, #RRGGBB, or 'default'. Enter updates the swatch.",
		tv.Rect{X: 2, Y: height - 4, W: width - 4, H: 1}))

	// loadFields seeds every spec field from a Theme.
	loadFields := func(t Theme) {
		for i, role := range themeRoles {
			fields[i].SetText(colorSpec(role.get(t)))
		}
	}

	refresh = func() {
		off := noColor.IsChecked()
		for i := range themeRoles {
			sw := swatches[i]
			c, ok := parseColor(fields[i].GetText())
			switch {
			case off:
				sw.SetText(sample)
				sw.FG = tv.DefaultTheme.DialogFG
			case !ok:
				sw.SetText("invalid")
				sw.FG = tv.DefaultTheme.DialogFG
			default:
				sw.SetText(sample)
				sw.FG = c
			}
		}
		w.desktop.Redraw()
	}

	preset.OnChange = func(index int) {
		loadFields(paletteByName(themePresets[index].name))
		refresh()
	}

	// Seed from the current configuration: the selected preset palette with the
	// saved overrides applied so the fields show the user's real colours.
	preset.SetSelected(presetIndex(cur.Name))
	noColor.SetChecked(cur.NoColor)
	noShadow.SetChecked(cur.NoShadow)
	loadFields(editedTheme(cur))
	refresh()

	var layer *tv.Layer
	save := func() {
		specs := make(map[string]string, len(themeRoles))
		for i, role := range themeRoles {
			specs[role.key] = fields[i].GetText()
		}
		idx := preset.GetSelected()
		if idx < 0 || idx >= len(themePresets) {
			idx = 0
		}
		cfg := buildThemeConfig(themePresets[idx].name, noColor.IsChecked(), noShadow.IsChecked(), specs)
		w.handlers.SetTheme(cfg) // persists + re-applies the live palette
		w.desktop.RemoveLayer(layer)
		w.rebuildMenu()
	}
	reset := func() {
		preset.SetSelected(0)
		noColor.SetChecked(false)
		noShadow.SetChecked(false)
		loadFields(paletteByName(themeDefault))
		refresh()
	}
	cancel := func() { w.desktop.RemoveLayer(layer) }

	dialog.Window.AddContent(newButton("Reset", tv.Rect{X: 2, Y: height - 3, W: 9, H: 1}, reset))
	dialog.Window.AddContent(newButton("Save", tv.Rect{X: width - 24, Y: height - 3, W: 9, H: 1}, save))
	dialog.Window.AddContent(newButton("Cancel", tv.Rect{X: width - 13, Y: height - 3, W: 10, H: 1}, cancel))

	dialog.Root().OnTypeFn = func(_ *tv.VisualComponent, event tui.TypeEvent) bool {
		if event.Key == tui.KeyEscape {
			cancel()
			return true
		}
		return false
	}

	layer = tv.NewModalLayer("theme-editor", dialog)
	w.desktop.AddLayer(layer)
	w.desktop.SetFocus(preset)
}
