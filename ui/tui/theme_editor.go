package ui

import (
	"fmt"
	"strconv"
	"strings"

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
// The labels are screen-anchored descriptions (issue #243) — what the colour
// actually paints on screen — rather than the struct field names, so a user can
// tell each role apart at a glance.
//
// The button/input roles (issue #265) expose the resting pair for each — button_fg/
// button_bg and input_fg/input_bg, the most-seen state and the core ask of #265, with
// full fg+bg control so a recoloured background stays legible. The focus pairs
// (button_focus_*/input_focus_*) are deliberately omitted from the editor — they would
// add a fourth row per column, and a height tall enough to hold them centres into the
// always-on-top menu bar on the 24-row terminal this dialog targets — but they remain
// first-class, config-overridable roles (applyOverrides/ResolveTheme/ApplyTheme) and
// are audited by paletteContrast like every other role.
var themeRoles = []themeRole{
	{"user", "User messages", func(t Theme) tui.Color { return t.User }},
	{"agent", "Agent replies", func(t Theme) tui.Color { return t.Agent }},
	{"note", "Thoughts / idle", func(t Theme) tui.Color { return t.Note }},
	{"tool", "Tool calls", func(t Theme) tui.Color { return t.Tool }},
	{"result", "Tool results", func(t Theme) tui.Color { return t.Result }},
	{"info", "System notes", func(t Theme) tui.Color { return t.Info }},
	{"error", "Errors", func(t Theme) tui.Color { return t.Error }},
	{"desktop_fg", "Desktop hint text", func(t Theme) tui.Color { return t.DesktopFG }},
	{"desktop_bg", "Desktop background", func(t Theme) tui.Color { return t.DesktopBG }},
	{"panel_fg", "Sidebar text", func(t Theme) tui.Color { return t.PanelFG }},
	{"panel_bg", "Sidebar background", func(t Theme) tui.Color { return t.PanelBG }},
	{"title", "Panel titles", func(t Theme) tui.Color { return t.Title }},
	{"divider", "Borders / dividers", func(t Theme) tui.Color { return t.Divider }},
	{"accent", "Indicators / badges", func(t Theme) tui.Color { return t.Accent }},
	{"menu_bar_fg", "Menu bar text", func(t Theme) tui.Color { return t.MenuBarFG }},
	{"menu_bar_bg", "Menu bar background", func(t Theme) tui.Color { return t.MenuBarBG }},
	{"dropdown_fg", "Dropdown text", func(t Theme) tui.Color { return t.DropdownFG }},
	{"dropdown_bg", "Dropdown background", func(t Theme) tui.Color { return t.DropdownBG }},
	{"dropdown_focus_fg", "Focused dropdown fg", func(t Theme) tui.Color { return t.DropdownFocusFG }},
	{"dropdown_focus_bg", "Focused dropdown bg", func(t Theme) tui.Color { return t.DropdownFocusBG }},
	{"dropdown_select_fg", "Open row text", func(t Theme) tui.Color { return t.DropdownSelectFG }},
	{"dropdown_select_bg", "Open row highlight", func(t Theme) tui.Color { return t.DropdownSelectBG }},
	{"button_fg", "Button text", func(t Theme) tui.Color { return t.ButtonFG }},
	{"button_bg", "Button background", func(t Theme) tui.Color { return t.ButtonBG }},
	{"input_fg", "Input text", func(t Theme) tui.Color { return t.InputFG }},
	{"input_bg", "Input background", func(t Theme) tui.Color { return t.InputBG }},
	{"code_bg", "Code block background", func(t Theme) tui.Color { return t.CodeBG }},
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

// swatchSample is the glyph string a colour swatch paints: two filled blocks and
// a letter pair, so both a fill and text rendering of the colour are visible.
const swatchSample = "▉▉ Aa"

// themeEditorLabelW is the width of the widest role label cell in the editor (the
// right column, which carries the longest labels). It must hold the longest
// descriptive label (issue #243) plus its trailing ":" on a single row: the labels
// live in 1-row Labels, so a cell narrower than the text makes the wrapped Label
// drop the overflow and clip the label on screen (e.g. "Code block background" →
// "Code block"). The longest label, "Code block background:", is 22 columns, so the
// cell is 22 wide; keep it in step with the widest themeRoles label.
const themeEditorLabelW = 22

// swatchStyle computes a swatch's display from a spec field's current text and
// the disable-colours toggle. It is the single source the live swatch is driven
// from (issue #243): the editor recomputes it on every render from the field's
// current value, so the swatch always tracks the field rather than a colour
// cached when the dialog opened. Colours off → the neutral sample in the dialog
// foreground; an unparseable spec → "invalid"; otherwise the sample in the
// parsed colour.
func swatchStyle(spec string, noColor bool) (text string, fg tui.Color) {
	switch {
	case noColor:
		return swatchSample, tv.DefaultTheme.DialogFG
	default:
		c, ok := parseColor(spec)
		if !ok {
			return "invalid", tv.DefaultTheme.DialogFG
		}
		return swatchSample, c
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

// carryUnexposedOverrides preserves any prior override whose key the editor does not
// expose as a field. buildThemeConfig rebuilds Overrides from themeRoles alone, and
// Gogent.SetTheme replaces the persisted theme config wholesale (no merge), so without
// this a Save — even one made for an unrelated reason — would silently drop a hand-set
// override that has no editor row. Issue #265's focus pairs (button_focus_fg/bg,
// input_focus_fg/bg) are the first such keys: they are first-class config roles
// (applyOverrides understands them) but are deliberately kept out of themeRoles for the
// editor's layout ceiling. Keys the editor DOES expose are left to the rebuilt set — the
// field is their source of truth — and an exposed key is matched after the same
// normalisation applyOverrides uses, so a differently-cased duplicate is not carried
// alongside the field-derived value.
func carryUnexposedOverrides(cfg config.ThemeConfig, prior map[string]string) config.ThemeConfig {
	exposed := make(map[string]bool, len(themeRoles))
	for _, role := range themeRoles {
		exposed[role.key] = true
	}
	for k, v := range prior {
		if exposed[strings.ToLower(strings.TrimSpace(k))] {
			continue
		}
		if cfg.Overrides == nil {
			cfg.Overrides = map[string]string{}
		}
		cfg.Overrides[k] = v
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
// #RRGGBB hex, or "default") and a live swatch. The swatch tracks the field as
// the user types or moves away — it is recomputed from the field's current value
// on every render via swatchStyle (issue #243), not cached when the dialog opens
// — and also follows the preset and the disable-colours toggle. Save persists the
// palette as the preferred theme and re-applies it to the live UI; Reset restores
// the default palette.
func (w *Workbench) showThemeEditor() {
	if w.handlers.GetTheme == nil || w.handlers.SetTheme == nil {
		w.showConfirm("Theme", "Theme editing is unavailable.", nil)
		return
	}
	cur := w.handlers.GetTheme()

	// Roomier than the original 72×15 (issue #243): a wider label column for the
	// descriptive role names and a blank separator row under the header so the two
	// columns of rows are not cramped. The width stays at 80 so the dialog fits a
	// standard 80-column terminal (centeredDialog only clamps the origin, it does
	// not scale an oversized dialog). The height grows with the role count: each
	// column holds half the roles starting at Y=4, and the "Spec:" hint sits at
	// height-4, so height must clear that last role row — the dropdown roles (#260)
	// added three rows per column, lifting it from 18 to 21, and the button/input
	// resting roles (#265) added one more per column, lifting it to 22. The 24-row
	// terminal is the hard ceiling: this dialog is centred (centeredDialog), so on a
	// 24-row terminal it sits at y=(24-height)/2, which must stay ≥1 to clear the
	// always-on-top menu bar on row 0 — i.e. height ≤ 22. That ceiling is why #265's
	// four focus roles are kept out of the editor (see themeRoles); a taller dialog
	// would centre its top frame onto the menu bar.
	const width = 80
	const height = 22
	x, y := centeredDialog(w, width, height)

	dialog := tv.NewDialog("Theme", x, y, width, height)
	applyWindowShadow(dialog.Window) // honour the NoShadow theme setting (issue #215)
	dialog.Window.ShowClose = false

	// refresh is wired into the widgets below; it is assigned after they exist.
	var refresh func()

	dialog.Window.AddContent(dialogLabel("Preset:", tv.Rect{X: 2, Y: 1, W: 8, H: 1}))
	presetLabels := make([]string, len(themePresets))
	for i, p := range themePresets {
		presetLabels[i] = p.label
	}
	preset := newSelect(w.desktop, presetLabels, tv.Rect{X: 10, Y: 1, W: 30, H: 1})
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
	// swatches[i] previews it. The roles split evenly: the first half fill the left
	// column (the semantic transcript colours and the chrome roles), the remainder the
	// right column (the menu-bar, dropdown, button/input and code-block roles).
	fields := make([]*tv.TextBox, len(themeRoles))
	swatches := make([]*tv.Label, len(themeRoles))

	// updateSwatch recomputes swatches[i] from fields[i]'s current text via
	// swatchStyle (issue #243). It is the single place a swatch is derived, called
	// both from refresh and from each swatch's per-render DrawFn, so the preview
	// always reflects the field's current value rather than one cached at open.
	updateSwatch := func(i int) {
		text, fg := swatchStyle(fields[i].GetText(), noColor.IsChecked())
		swatches[i].SetText(text)
		swatches[i].FG = fg
	}

	// Two columns inside the width-80 dialog. turbotui insets window content by the
	// border (one column each side), so the usable content area is 78 columns wide
	// (relative cols 0..77) — a child running to relative col 78 lands on the right
	// border and is clipped. Each column holds a label, a 7-wide spec field ("#RRGGBB"
	// / "default") and a 7-wide swatch ("invalid"); the right column's swatch must end
	// no later than relative col 77. The right column carries the longest labels so it
	// gets the full themeEditorLabelW cell; the left column's labels top out at 19
	// columns ("Desktop background:"), so a narrower cell there buys the gap the right
	// column needs to keep its swatch (and the "invalid" marker) fully on screen.
	// The left cell is 20 (not 19): the button/input roles (#265) grew each column, so
	// the 19-column label "Indicators / badges" now falls in the left column and needs a
	// 20-wide cell to hold its trailing ":". The left swatch then ends at
	// x = 2+20+7+2+7-1 = 37, still clear of the right column at x=40.
	const fieldW, swatchW = 7, 7
	columns := [...]struct{ x, labelW int }{
		{2, 20},                 // left column
		{40, themeEditorLabelW}, // right column (longest labels)
	}
	half := (len(themeRoles) + 1) / 2
	for i, role := range themeRoles {
		col, row := 0, 4+i
		if i >= half {
			col, row = 1, 4+i-half
		}
		lx, labelW := columns[col].x, columns[col].labelW
		dialog.Window.AddContent(dialogLabel(role.label+":", tv.Rect{X: lx, Y: row, W: labelW, H: 1}))
		box := tv.NewTextBox("", tv.Rect{X: lx + labelW + 1, Y: row, W: fieldW, H: 1})
		box.OnSubmit = func() { refresh() }
		dialog.Window.AddContent(box)
		fields[i] = box
		sw := tv.NewLabel(swatchSample, tv.Rect{X: lx + labelW + fieldW + 2, Y: row, W: swatchW, H: 1})
		sw.BG = tv.DefaultTheme.DialogBG
		// Drive the swatch from the field's current value on every render (issue
		// #243) so it tracks the field as the user types or moves focus away, not
		// only on Enter. baseDraw is the Label's own renderer, run after the recolour.
		idx := i
		baseDraw := sw.Component.DrawFn
		sw.Component.DrawFn = func(c *tv.VisualComponent, surface tv.Surface) {
			updateSwatch(idx)
			baseDraw(c, surface)
		}
		dialog.Window.AddContent(sw)
		swatches[i] = sw
	}

	dialog.Window.AddContent(dialogLabel(
		"Spec: ANSI 0–255, #RRGGBB, or 'default'. Swatch tracks the field live.",
		tv.Rect{X: 2, Y: height - 4, W: width - 4, H: 1}))

	// loadFields seeds every spec field from a Theme.
	loadFields := func(t Theme) {
		for i, role := range themeRoles {
			fields[i].SetText(colorSpec(role.get(t)))
		}
	}

	refresh = func() {
		for i := range themeRoles {
			updateSwatch(i)
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
		// Don't let a Save erase overrides the editor has no field for (the #265 focus
		// pairs): SetTheme replaces the config wholesale, so carry the unexposed keys.
		cfg = carryUnexposedOverrides(cfg, cur.Overrides)
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
