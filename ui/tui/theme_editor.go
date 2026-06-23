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

// themeGroup is a labelled section of related roles in the editor (issue #267).
// Grouping the roles under named headings — Session output, UI chrome, Controls,
// Buttons and inputs, Code — lets a user find "all transcript colours" or "all
// dropdown colours" at a glance instead of scanning one flat two-column wall. The
// groups only decide where each role is drawn on screen; the colour/override
// machinery still works off the flattened themeRoles below, so every key
// round-trips exactly as before.
type themeGroup struct {
	title string
	roles []themeRole
}

// themeGroups is the editor's section layout: each group carries its heading and the
// roles drawn beneath it (issue #267). The keys must match the names parsed in
// applyOverrides so a saved override round-trips on next launch. The labels are
// screen-anchored descriptions (issue #243) — what the colour actually paints on
// screen — rather than the struct field names, so a user can tell each role apart at
// a glance.
//
// The button/input roles (issue #265) live under "Buttons and inputs" and expose the
// resting pair for each — button_fg/button_bg and input_fg/input_bg, the most-seen
// state and the core ask of #265, with full fg+bg control so a recoloured background
// stays legible — followed by the #279 text-selection pair. The focus pairs
// (button_focus_*/input_focus_*) are deliberately kept out of the editor to keep it to
// the most-used roles, but they remain first-class, config-overridable roles
// (applyOverrides/ResolveTheme/ApplyTheme), are carried across a Save by
// carryUnexposedOverrides, and are audited by paletteContrast like every other role.
// The section content now scrolls (see showThemeEditor), so the editor can hold more
// roles than fit at once without growing past the 24-row terminal ceiling.
var themeGroups = []themeGroup{
	{"Session output", []themeRole{
		{"user", "User messages", func(t Theme) tui.Color { return t.User }},
		{"agent", "Agent replies", func(t Theme) tui.Color { return t.Agent }},
		{"note", "Thoughts / idle", func(t Theme) tui.Color { return t.Note }},
		{"tool", "Tool calls", func(t Theme) tui.Color { return t.Tool }},
		{"result", "Tool results", func(t Theme) tui.Color { return t.Result }},
		{"info", "System notes", func(t Theme) tui.Color { return t.Info }},
		{"error", "Errors", func(t Theme) tui.Color { return t.Error }},
	}},
	{"UI chrome", []themeRole{
		{"desktop_fg", "Desktop hint text", func(t Theme) tui.Color { return t.DesktopFG }},
		{"desktop_bg", "Desktop background", func(t Theme) tui.Color { return t.DesktopBG }},
		{"panel_fg", "Sidebar text", func(t Theme) tui.Color { return t.PanelFG }},
		{"panel_bg", "Sidebar background", func(t Theme) tui.Color { return t.PanelBG }},
		// Window background/text (issue #291): the surface behind the transcript, now a
		// first-class, editable role rather than turbotui's fixed ANSI-4 blue.
		{"window_fg", "Window text", func(t Theme) tui.Color { return t.WindowFG }},
		{"window_bg", "Window background", func(t Theme) tui.Color { return t.WindowBG }},
		// Dialog-list background (issue #327): the inset behind the filterable lists.
		// Only the background is exposed (matching code_bg); list_fg stays a first-class
		// config-overridable role carried across a Save by carryUnexposedOverrides.
		{"list_bg", "List background", func(t Theme) tui.Color { return t.ListBG }},
		{"title", "Panel titles", func(t Theme) tui.Color { return t.Title }},
		{"divider", "Borders / dividers", func(t Theme) tui.Color { return t.Divider }},
		{"accent", "Indicators / badges", func(t Theme) tui.Color { return t.Accent }},
	}},
	{"Controls", []themeRole{
		{"menu_bar_fg", "Menu bar text", func(t Theme) tui.Color { return t.MenuBarFG }},
		{"menu_bar_bg", "Menu bar background", func(t Theme) tui.Color { return t.MenuBarBG }},
		{"dropdown_fg", "Dropdown text", func(t Theme) tui.Color { return t.DropdownFG }},
		{"dropdown_bg", "Dropdown background", func(t Theme) tui.Color { return t.DropdownBG }},
		{"dropdown_focus_fg", "Focused dropdown fg", func(t Theme) tui.Color { return t.DropdownFocusFG }},
		{"dropdown_focus_bg", "Focused dropdown bg", func(t Theme) tui.Color { return t.DropdownFocusBG }},
		{"dropdown_select_fg", "Open row text", func(t Theme) tui.Color { return t.DropdownSelectFG }},
		{"dropdown_select_bg", "Open row highlight", func(t Theme) tui.Color { return t.DropdownSelectBG }},
	}},
	{"Buttons and inputs", []themeRole{
		{"button_fg", "Button text", func(t Theme) tui.Color { return t.ButtonFG }},
		{"button_bg", "Button background", func(t Theme) tui.Color { return t.ButtonBG }},
		{"input_fg", "Input text", func(t Theme) tui.Color { return t.InputFG }},
		{"input_bg", "Input background", func(t Theme) tui.Color { return t.InputBG }},
		// Text selection inside inputs (issue #279): the colours a selected run is painted
		// in, distinct from the dropdown/menu Selection* roles.
		{"text_selection_fg", "Selected text fg", func(t Theme) tui.Color { return t.TextSelectionFG }},
		{"text_selection_bg", "Selected text bg", func(t Theme) tui.Color { return t.TextSelectionBG }},
	}},
	{"Code", []themeRole{
		{"code_bg", "Code block background", func(t Theme) tui.Color { return t.CodeBG }},
	}},
}

// themeRoles is the flattened view of themeGroups in section order: the seven
// transcript colours, the chrome colours, the controls, the button/input resting
// pairs and the code-block background. applyOverrides, buildThemeConfig,
// carryUnexposedOverrides, editedTheme and the round-trip/contrast tests all iterate
// this flat list, so regrouping the editor into sections leaves them untouched —
// fields[i] and swatches[i] stay keyed by this single role index; the groups only
// change where each row is drawn. The flatten order equals the historical flat order,
// so nothing that iterates themeRoles by index shifts.
var themeRoles = flattenThemeGroups()

// flattenThemeGroups concatenates every group's roles into the ordered themeRoles
// view, preserving section order.
func flattenThemeGroups() []themeRole {
	var roles []themeRole
	for _, g := range themeGroups {
		roles = append(roles, g.roles...)
	}
	return roles
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

// pickerCommitSentinel is a colour with an invalid ColorMode (turbotui defines
// only ColorDefault/ColorANSI/ColorRGB == 0/1/2), so it equals no colour the
// ColorPicker can ever commit. The theme editor parks it in a picker after it
// opens so turbotui's commit — which fires OnChange only when the committed colour
// differs from the picker's current Color — reports a change on every explicit
// commit, including a re-pick of the seeded colour or a non-canonical spec. It is
// never drawn or committed, only used as an always-different baseline (issue #366).
var pickerCommitSentinel = tui.Color{Mode: 0xFF}

// themeEditorLabelW is the MINIMUM width of the right column's role-label cell — the
// column that carries the longest labels. It must hold the longest descriptive label
// (issue #243) plus its trailing ":" on a single row: the labels live in 1-row Labels,
// so a cell narrower than the text makes the wrapped Label drop the overflow and clip
// the label on screen (e.g. "Code block background" → "Code block"). The longest label,
// "Code block background:", is 22 columns, so the cell is at least 22 wide. On a dialog
// wider than the 80-column floor the cell grows past this (issue #317); it never shrinks
// below it.
const themeEditorLabelW = 22

// themeEditorLeftLabelW is the MINIMUM width of the left column's role-label cell. The
// left column's labels top out at "Indicators / badges:" (20), so the cell floors at 20
// and, like the right cell, widens on a larger dialog (issue #317).
const themeEditorLeftLabelW = 20

// themeEditorContentTop is the first row the scrolling role/section viewport occupies
// (issue #267). The roles start here with no blank separator — the spec-format hint moved
// up beside the toggles on row 2 — so the viewport gets the full run of rows between the
// toggles and the action buttons.
const themeEditorContentTop = 3

// themeEditorVisibleRows is the FLOOR height of the scroll viewport: the rows between
// themeEditorContentTop and the Save/Reset/Cancel button row (themeEditorDialogH-3) at
// the 80×22 minimum. The live viewport height is resolveThemeEditorLayout(...).visibleRows
// (which grows with a larger dialog, issue #317); this const remains the documented floor
// value the layout guard and the editor's scroll-math tests rest on.
const themeEditorVisibleRows = themeEditorDialogH - 3 - themeEditorContentTop

// themeEditorScrollbarX is the FLOOR content-relative column of the fixed vertical
// scrollbar: the last usable content column at the 80-wide minimum, just inside the right
// border. The live column is resolveThemeEditorLayout(...).scrollbarX = width-3 (issue
// #317); this const is the documented floor value the layout guard and tests rest on.
const themeEditorScrollbarX = themeEditorDialogW - 3

// Geometry FLOOR of the modal theme editor (issue #267). Width floors at 80 so the
// dialog fits a standard 80-column terminal, and height at 22 — the 24-row terminal is
// the hard ceiling. Since issue #317 the dialog is no longer PINNED to this footprint:
// the shared resolver is given these as Min floors only, so the editor grows toward the
// 80%×85% cap on a larger terminal while still collapsing to 80×22 on a small one, and is
// centred (and re-centred on resize) like every other dialog (issue #299). The renderer
// derives all of its geometry from the resolved live bounds (resolveThemeEditorLayout),
// not these constants; they remain the documented minimum and the size at which
// checkThemeEditorLayout asserts the scrolling model is self-consistent. themeEditorFieldW
// and themeEditorSwatchW are the spec-field ("#RRGGBB"/"default") and live-swatch
// ("invalid") cell widths that follow each role label.
const (
	themeEditorDialogW = 80
	themeEditorDialogH = 22
	themeEditorFieldW  = 7
	themeEditorSwatchW = 7
)

// themeEditorColumn places a contiguous run of groups in one on-screen column of the
// editor: its x origin, the width of its label cell, and the groups stacked in it (each
// drawn as a header row then one role per row).
type themeEditorColumn struct {
	x, labelW int
	groups    []themeGroup
}

// themeEditorLayout is the editor's geometry resolved from the live dialog bounds (issue
// #317), replacing the former compile-time constants as the renderer's source of truth.
// The dialog floors at themeEditorDialogW×themeEditorDialogH and grows toward the shared
// 80%×85% cap; every position the renderer and the scroll math need is derived from the
// resolved width/height so the two columns, the scrollbar and the viewport fill the
// larger dialog while still collapsing to the documented 80×22 minimum. The two-column
// scrolling model is unchanged — only the geometry source moved from constants to these
// resolved bounds.
type themeEditorLayout struct {
	width, height int                 // resolved dialog bounds
	contentTop    int                 // first viewport row
	visibleRows   int                 // scroll viewport height
	scrollbarX    int                 // content-relative scrollbar column (last usable column)
	columns       []themeEditorColumn // the two section columns, spread to the width
}

// resolveThemeEditorLayout derives the editor's geometry from the resolved dialog bounds
// (issue #317). The scrollbar sits one column inside the right border (width-3); the
// viewport spans the rows between themeEditorContentTop and the action-button row
// (height-3). The two section columns split the content area — the left column stacks
// Session output and UI chrome, the right column stacks Controls, Buttons and inputs and
// Code (issue #267) — and the extra width beyond the 80-column floor is shared evenly
// between the two label cells, so the columns, fields and swatches use the space on a
// larger dialog while collapsing to the original {x:2,labelW:20} / {x:39,labelW:22}
// placement at the floor. By construction the right column's swatch always ends exactly at
// the column before the scrollbar, and the left column clears the right by one gap column,
// so checkThemeEditorLayout's collision/label-fit invariants hold at every width ≥ 80.
func resolveThemeEditorLayout(width, height int) themeEditorLayout {
	extra := width - themeEditorDialogW
	if extra < 0 {
		extra = 0
	}
	extraL := extra / 2
	extraR := extra - extraL

	const leftX = 2
	leftLabelW := themeEditorLeftLabelW + extraL
	// One gap column between the left column's swatch and the right column's label.
	rightX := leftX + leftLabelW + themeEditorFieldW + themeEditorSwatchW + 2 + 1
	rightLabelW := themeEditorLabelW + extraR

	return themeEditorLayout{
		width:       width,
		height:      height,
		contentTop:  themeEditorContentTop,
		visibleRows: height - 3 - themeEditorContentTop,
		scrollbarX:  width - 3,
		columns: []themeEditorColumn{
			{leftX, leftLabelW, themeGroups[:2]},   // left: Session output, UI chrome
			{rightX, rightLabelW, themeGroups[2:]}, // right: Controls, Buttons and inputs, Code
		},
	}
}

// maxScroll is the largest valid scroll offset for this layout's viewport: 0 when the
// content fits, otherwise the number of rows by which the tallest column overflows it. On
// a tall (grown) dialog the content fits and this is 0 — no scrolling; at the 80×22 floor
// the #279/#291 roles overflow and it scrolls.
func (l themeEditorLayout) maxScroll() int {
	if m := themeEditorContentRows() - l.visibleRows; m > 0 {
		return m
	}
	return 0
}

// clampScroll clamps a scroll offset to [0, l.maxScroll()].
func (l themeEditorLayout) clampScroll(y int) int {
	if y < 0 {
		return 0
	}
	if max := l.maxScroll(); y > max {
		return max
	}
	return y
}

// themeEditorColumns is the two-column section placement at the 80×22 floor — the value
// the init-time layout guard (checkThemeEditorLayout) and the editor's layout tests read.
// It is the floor case of resolveThemeEditorLayout, so the split point can never drift
// from what the renderer draws on an 80-column terminal.
func themeEditorColumns() []themeEditorColumn {
	return resolveThemeEditorLayout(themeEditorDialogW, themeEditorDialogH).columns
}

// themeEditorColumnRows is the number of on-screen rows a column occupies before
// scrolling: one header row per group plus one row per role.
func themeEditorColumnRows(col themeEditorColumn) int {
	rows := 0
	for _, g := range col.groups {
		rows += 1 + len(g.roles)
	}
	return rows
}

// themeEditorContentRows is the height of the tallest column — the full scrollable
// content height the viewport pages through. It depends only on the group split, not the
// resolved width, so it is geometry-independent.
func themeEditorContentRows() int {
	max := 0
	for _, col := range themeEditorColumns() {
		if r := themeEditorColumnRows(col); r > max {
			max = r
		}
	}
	return max
}

// themeEditorMaxScroll is the largest valid scroll offset at the 80×22 floor — the value
// the layout guard and the editor's scroll-math tests read. The renderer uses the live
// layout's maxScroll method instead (issue #317).
func themeEditorMaxScroll() int {
	return resolveThemeEditorLayout(themeEditorDialogW, themeEditorDialogH).maxScroll()
}

// clampThemeScroll clamps a scroll offset to [0, themeEditorMaxScroll()] at the floor.
func clampThemeScroll(y int) int {
	return resolveThemeEditorLayout(themeEditorDialogW, themeEditorDialogH).clampScroll(y)
}

// checkThemeEditorLayout asserts the scrolling editor's layout invariants hold for the
// current themeGroups, themeEditorColumns and geometry consts, panicking at package init
// (and thus in every test run and at program start) if a future edit breaks one. The
// renderer relies on all of these silently. Unlike the pre-scroll guard, adding a role no
// longer has to fit under the height ceiling — the viewport scrolls — so this asserts the
// scrolling model is self-consistent instead: the viewport is non-empty and clears the
// buttons, the columns do not collide with each other or the scrollbar, every label fits
// its cell, every role is placed exactly once, and the scroll range can bring the tallest
// column's last row into view.
func checkThemeEditorLayout() {
	const buttonRow = themeEditorDialogH - 3 // Reset/Save/Cancel live on this row

	// The scroll viewport must be at least one row tall and sit strictly above the buttons.
	if themeEditorVisibleRows < 1 {
		panic(fmt.Sprintf("theme editor: visible viewport is %d rows — nothing would show", themeEditorVisibleRows))
	}
	if last := themeEditorContentTop + themeEditorVisibleRows - 1; last >= buttonRow {
		panic(fmt.Sprintf("theme editor: viewport last row %d collides with the buttons at row %d", last, buttonRow))
	}

	cols := themeEditorColumns()
	placed := 0
	for ci, col := range cols {
		// Columns must not collide: this column's widest extent (its swatch end) must fall
		// before the next column's x; the last column must stop short of the scrollbar column.
		swatchEnd := col.x + col.labelW + themeEditorFieldW + themeEditorSwatchW + 2 - 1
		limit := themeEditorScrollbarX - 1
		if ci+1 < len(cols) {
			limit = cols[ci+1].x - 1
		}
		if swatchEnd > limit {
			panic(fmt.Sprintf("theme editor: column %d swatch ends at col %d, past its limit %d — move the column or narrow the label cell", ci, swatchEnd, limit))
		}
		for _, g := range col.groups {
			for _, role := range g.roles {
				if n := len([]rune(role.label)) + 1; n > col.labelW {
					panic(fmt.Sprintf("theme editor: label %q + \":\" is %d cols but column %d cell is %d wide — it would clip on screen", role.label, n, ci, col.labelW))
				}
				placed++
			}
		}
	}
	if placed != len(themeRoles) {
		panic(fmt.Sprintf("theme editor: columns place %d roles but themeRoles has %d — a group is unplaced or double-placed", placed, len(themeRoles)))
	}

	// The scrollbar column must sit inside the right border.
	if themeEditorScrollbarX >= themeEditorDialogW-2 {
		panic(fmt.Sprintf("theme editor: scrollbar column %d overlaps the right border", themeEditorScrollbarX))
	}
	// Scroll-bounds consistency: at the maximum offset the tallest column's last row must
	// land within the viewport, so every role is reachable by scrolling.
	if reveal := themeEditorContentRows() - themeEditorMaxScroll(); reveal > themeEditorVisibleRows {
		panic(fmt.Sprintf("theme editor: max scroll leaves %d rows for a %d-row viewport — the last roles can never be revealed", reveal, themeEditorVisibleRows))
	}
	// Menu-bar ceiling: centred on a 24-row terminal the top frame must clear row 0.
	if topY := (24 - themeEditorDialogH) / 2; topY < 1 {
		panic(fmt.Sprintf("theme editor: height %d centres the top frame onto the menu bar on a 24-row terminal", themeEditorDialogH))
	}
}

func init() { checkThemeEditorLayout() }

// themeSectionHeader builds a section heading for the grouped editor (issue #267): the
// title followed by a horizontal rule that fills the rest of the column, so the
// section reads as a divider above its roles. It is a dialog-coloured label like every
// other dialog text, so it stays legible under every preset and the NO_COLOR toggle,
// and is distinguished from the role rows by its own row, the fill rule, and the
// absence of a trailing ":". The text is sized to exactly r.W columns so the 1-row
// label never wraps the rule away.
func themeSectionHeader(title string, r tv.Rect) *tv.Label {
	text := title
	if pad := r.W - len([]rune(title)) - 1; pad > 0 {
		text = title + " " + strings.Repeat("─", pad)
	}
	return dialogLabel(text, r)
}

// drawThemeEditorScrollbar paints the editor's 1-column vertical scrollbar over track:
// a full-height │ line with ▲/▼ caps and a single █ thumb whose position reflects offset
// within [0, total-visible]. It mirrors turbotui's shared (but unexported) text-view/tree
// scrollbar so the editor's scroll affordance looks and behaves like the rest of the UI.
// With nothing to scroll (total <= visible) only the track and caps are drawn.
func drawThemeEditorScrollbar(surface tv.Surface, track tv.Rect, total, visible, offset int, fg, bg tui.Color) {
	if track.H < 1 {
		return
	}
	x := track.X
	for row := 0; row < track.H; row++ {
		surface.SetCell(x, track.Y+row, tui.Cell{Ch: '│', FG: fg, BG: bg})
	}
	surface.SetCell(x, track.Y, tui.Cell{Ch: '▲', FG: fg, BG: bg})
	surface.SetCell(x, track.Bottom(), tui.Cell{Ch: '▼', FG: fg, BG: bg})
	span := total - visible
	inner := track.H - 2 // rows between the two arrow caps
	if span <= 0 || inner <= 0 {
		return
	}
	if offset < 0 {
		offset = 0
	}
	if offset > span {
		offset = span
	}
	thumb := offset * (inner - 1) / span
	if thumb < 0 {
		thumb = 0
	}
	if thumb > inner-1 {
		thumb = inner - 1
	}
	surface.SetCell(x, track.Y+1+thumb, tui.Cell{Ch: '█', FG: fg, BG: bg})
}

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
// (applyOverrides understands them) but are deliberately kept out of themeRoles to keep the
// editor to the most-used roles. Keys the editor DOES expose are left to the rebuilt set — the
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

	// The grouped layout (issue #267) draws the roles as five labelled sections across two
	// columns (see resolveThemeEditorLayout): the left column stacks Session output and UI
	// chrome, the right column stacks Controls, Buttons and inputs and Code. Each section costs
	// one header row plus its roles. With the #279/#291 roles added the columns are taller than
	// fit between contentTop and the buttons at the 80×22 floor, so the section content lives
	// inside a scrolling viewport (built below): the preset row and the Save/Reset/Cancel
	// buttons stay fixed while the grouped rows scroll. On a larger terminal the dialog grows
	// (issue #317), the viewport gains rows and the content stops needing to scroll.
	// checkThemeEditorLayout (run at init) asserts the scroll model is self-consistent at the
	// floor, so this renderer can trust the resolved geometry.

	// Floored, not pinned (issue #317): the shared resolver is given an 80×22 Min floor only,
	// so the editor floors at 80×22 on an 80×24 terminal and grows toward the 80%×85% cap on a
	// larger one, centred (and re-centred on resize) like every other dialog (issue #299). The
	// spec is a pure floor, so dialog.Fit is path-independent; the renderer reads the resolved
	// width/height (not the compile-time constants) so the columns, scrollbar and viewport fill
	// whatever the dialog resolved to.
	spec := tv.DialogSpec{MinW: themeEditorDialogW, MinH: themeEditorDialogH}
	x, y, width, height := w.dialogRect(spec)
	layout := resolveThemeEditorLayout(width, height)

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

	// Spec-format hint, lifted up beside the toggles (issue #267) so the grouped role
	// sections below can start at row 3 and still clear the menu-bar height ceiling. It
	// sits in the free left half of row 2, left of the Disable-shadows checkbox at x=44.
	dialog.Window.AddContent(dialogLabel(
		"Spec: ANSI 0–255, #RRGGBB, or 'default'.",
		tv.Rect{X: 2, Y: 2, W: 40, H: 1}))

	// One row per role across two columns of labelled sections (issue #267); fields[i]
	// edits themeRoles[i] and pickers[i] previews it and opens the colour picker for it
	// (issue #366). The placement loop below walks themeGroups in flatten order, so
	// fields/pickers stay keyed by the single flat role index — the grouping only
	// changes where each row is drawn.
	fields := make([]*tv.TextBox, len(themeRoles))
	pickers := make([]*tv.ColorPicker, len(themeRoles))

	// Two columns of labelled sections inside a scrolling viewport (issues #267/#279/#291).
	// The viewport is a clipping container spanning the rows between the toggles and the
	// buttons; its children are positioned by their column-local logical row (a header row then
	// one role per row) and reflow() shifts and shows/hides them as scrollY changes, so rows
	// outside the window are neither drawn, clicked nor focus-navigated. The columns and the
	// scrollbar column come from the resolved layout (issue #317): the left column clears the
	// right by one gap column and the right column's swatch ends just before layout.scrollbarX,
	// which holds the fixed scrollbar. fields[i]/swatches[i] stay keyed by the single flat
	// themeRoles index, so the scrolling only changes where each row is drawn — Save still
	// reads every field regardless of scroll position. checkThemeEditorLayout (init) guards
	// these bounds at the floor, and the layout grows monotonically, so they hold above it.
	const fieldW, swatchW = themeEditorFieldW, themeEditorSwatchW

	// viewport clips the scrolling section content to rows layout.contentTop ..
	// layout.contentTop+layout.visibleRows-1, stopping short of the scrollbar column.
	viewport := tv.NewComponent(tv.Rect{X: 0, Y: layout.contentTop, W: layout.scrollbarX, H: layout.visibleRows})
	dialog.Window.AddContent(viewport)

	// rowKind tags which cell of a role row a component is, so its horizontal placement can be
	// re-derived from the resolved column geometry both at build time and on resize (issue #317).
	type rowKind int
	const (
		rowHeader rowKind = iota
		rowLabel
		rowField
		rowSwatch
	)
	// cellRect is the x-origin and width of a row's cell within its column — the single place
	// horizontal placement is computed, called when building the rows and again by relayout()
	// when the dialog is resized.
	cellRect := func(col themeEditorColumn, kind rowKind) (x, wdt int) {
		switch kind {
		case rowHeader:
			return col.x, col.labelW + fieldW + swatchW + 2
		case rowField:
			return col.x + col.labelW + 1, fieldW
		case rowSwatch:
			return col.x + col.labelW + fieldW + 2, swatchW
		default: // rowLabel
			return col.x, col.labelW
		}
	}

	// scrollRow couples a header/label/field/swatch component to its column-local logical row
	// and the column/cell it belongs to, so reflow() can reposition and show/hide it as the
	// viewport scrolls and relayout() can re-place it horizontally when the dialog is resized.
	type scrollRow struct {
		comp    *tv.VisualComponent
		logical int
		colIdx  int
		kind    rowKind
	}
	var rows []scrollRow
	addRow := func(comp *tv.VisualComponent, logical, colIdx int, kind rowKind) {
		viewport.AddChild(comp)
		rows = append(rows, scrollRow{comp, logical, colIdx, kind})
	}

	i := 0
	for colIdx, col := range layout.columns {
		logical := 0 // column-local row; viewport-relative Y is logical-scrollY (set by reflow)
		for _, g := range col.groups {
			hx, hw := cellRect(col, rowHeader)
			addRow(themeSectionHeader(g.title, tv.Rect{X: hx, Y: logical, W: hw, H: 1}).Root(), logical, colIdx, rowHeader)
			logical++
			for _, role := range g.roles {
				idx := i
				lx, lw := cellRect(col, rowLabel)
				addRow(dialogLabel(role.label+":", tv.Rect{X: lx, Y: logical, W: lw, H: 1}).Root(), logical, colIdx, rowLabel)
				fx, fw := cellRect(col, rowField)
				box := tv.NewTextBox("", tv.Rect{X: fx, Y: logical, W: fw, H: 1})
				box.OnSubmit = func() { refresh() }
				addRow(box.Root(), logical, colIdx, rowField)
				fields[idx] = box
				sx, sWid := cellRect(col, rowSwatch)
				// The swatch is an interactive colour picker (issue #366): it renders the
				// live preview AND, on click or Enter/Space, opens turbotui's ColorPicker
				// popup. The text field stays the canonical spec input; the picker is an
				// accelerator that writes a spec back into it, so the save/override path is
				// untouched.
				pk := newColorPicker(w.desktop, tv.Rect{X: sx, Y: logical, W: sWid, H: 1})
				// Render the swatch from the field's current value on every render (issue
				// #243) so it tracks the field as the user types or moves focus away, with a
				// focus-background cue so keyboard activation is visible. This replaces the
				// picker's own closed-swatch draw, keeping gogent's "▉▉ Aa"/"invalid"/neutral
				// look and the noColor toggle behaviour.
				pk.Component.DrawFn = func(c *tv.VisualComponent, surface tv.Surface) {
					text, fg := swatchStyle(fields[idx].GetText(), noColor.IsChecked())
					bg := tv.DefaultTheme.DialogBG
					if c.Focused() {
						bg = tv.DefaultTheme.SelectionBG
					}
					abs := c.AbsoluteBounds()
					surface.Fill(abs, tui.Cell{Ch: ' ', FG: fg, BG: bg})
					surface.WriteStringClipped(abs.X, abs.Y, abs.W, text, tui.Cell{Ch: ' ', FG: fg, BG: bg})
				}
				// Seed the picker from the field's current colour just before it opens (so its
				// cursor lands on that colour), gate on the gogent "Disable colours" toggle
				// (when colours are off there is nothing to pick), and — once open — park an
				// impossible sentinel colour in the picker. turbotui's ColorPicker.commit fires
				// OnChange only when the committed colour differs from the picker's current
				// Color; seeding Color to the field colour would therefore swallow a commit of
				// that same colour (Enter on the highlighted cell, or re-picking a non-canonical
				// spec like "003"/"none"). open() has already read the seed to position the
				// cursor by the time armPicker runs, and the closed swatch renders from the field
				// (not picker.Color), so a colour no real pick can equal (an invalid ColorMode)
				// makes every explicit commit report a change while staying invisible. Cancelling
				// (Escape/outside click) never commits, so the field is left untouched.
				armPicker := func() {
					if c, ok := parseColor(fields[idx].GetText()); ok {
						pk.SetColor(c)
					} else {
						pk.SetColor(tui.DefaultColor())
					}
				}
				openType, openClick := pk.Component.OnTypeFn, pk.Component.OnClickFn
				pk.Component.OnTypeFn = func(c *tv.VisualComponent, ev tui.TypeEvent) bool {
					if noColor.IsChecked() {
						return false // colours off: leave the key to focus navigation
					}
					armPicker()
					handled := openType(c, ev)
					if pk.IsOpen() {
						pk.SetColor(pickerCommitSentinel)
					}
					return handled
				}
				pk.Component.OnClickFn = func(c *tv.VisualComponent, ev tui.ClickEvent) bool {
					if noColor.IsChecked() {
						return true // colours off: swallow the click, nothing to pick
					}
					armPicker()
					handled := openClick(c, ev)
					if pk.IsOpen() {
						pk.SetColor(pickerCommitSentinel)
					}
					return handled
				}
				// A committed colour is written back as the canonical spec for this role and
				// the editor refreshed, so the swatch updates and Save reads it from the field
				// exactly as a hand-typed spec — the existing applyOverrides/ResolveTheme/
				// SetTheme round-trip is unchanged. Escape in the popup cancels without firing
				// OnChange, leaving the field untouched.
				pk.OnChange = func(c tui.Color) {
					fields[idx].SetText(colorSpec(c))
					refresh()
				}
				addRow(pk.Root(), logical, colIdx, rowSwatch)
				pickers[idx] = pk
				i++
				logical++
			}
		}
	}

	// Scrolling state. scrollY is the first visible logical row; reflow() repositions every
	// row to viewport-relative Y = logical-scrollY and hides those outside the window so they
	// are not drawn, hit-tested or focus-navigated. scrollTo() clamps, reflows and redraws.
	scrollY := 0
	reflow := func() {
		for _, r := range rows {
			b := r.comp.Bounds
			b.Y = r.logical - scrollY
			r.comp.SetBounds(b)
			r.comp.Visible = r.logical >= scrollY && r.logical < scrollY+layout.visibleRows
		}
	}
	// keepFocusVisible moves focus off a field that the latest scroll has hidden. A hidden
	// focused widget stops receiving keys (the desktop's visibleInTree guard), which would
	// otherwise strand keyboard scrolling until the user clicked or wheel-scrolled; moving
	// focus to a still-visible field keeps Up/Down/PageUp/PageDown bubbling to this dialog.
	keepFocusVisible := func() {
		// Both the spec field and its colour picker are focusable (issue #366), so either
		// can be the focused row widget the scroll just hid; check both, then park focus on
		// the first still-visible field so keyboard scrolling keeps bubbling to the dialog.
		focusedHidden := false
		for i := range fields {
			if fields[i].Root().Focused() && !fields[i].Root().Visible {
				focusedHidden = true
				break
			}
			if pickers[i].Root().Focused() && !pickers[i].Root().Visible {
				focusedHidden = true
				break
			}
		}
		if !focusedHidden {
			return
		}
		for _, f := range fields {
			if f.Root().Visible {
				w.desktop.SetFocus(f)
				return
			}
		}
	}
	scrollTo := func(y int) {
		n := layout.clampScroll(y)
		if n == scrollY {
			return
		}
		scrollY = n
		reflow()
		keepFocusVisible()
		w.desktop.Redraw()
	}

	// The fixed vertical scrollbar and the wheel handler are always wired so they track the
	// live viewport across a resize (issue #317): the bar's DrawFn paints nothing when the
	// content fits the (grown) viewport, and scrollTo clamps to maxScroll (a no-op at 0). The
	// DrawFn reads the live scrollY each frame, so the thumb tracks the current offset.
	bar := tv.NewComponent(tv.Rect{X: layout.scrollbarX, Y: layout.contentTop, W: 1, H: layout.visibleRows})
	bar.DrawFn = func(c *tv.VisualComponent, surface tv.Surface) {
		if layout.maxScroll() == 0 {
			return // content fits the viewport — no scrollbar to draw
		}
		drawThemeEditorScrollbar(surface, c.AbsoluteBounds(),
			themeEditorContentRows(), layout.visibleRows, scrollY,
			tv.DefaultTheme.DialogFG, tv.DefaultTheme.DialogBG)
	}
	dialog.Window.AddContent(bar)
	// Mouse wheel over the section content scrolls it. Delta is +1 for wheel-up and -1
	// for wheel-down, so subtract it (scrollY -= Delta) to scroll the content the natural
	// way — the same convention turbotui's own text view, tree and select use.
	viewport.OnScrollFn = func(_ *tv.VisualComponent, event tui.ScrollEvent) bool {
		scrollTo(scrollY - event.Delta)
		return true
	}

	// loadFields seeds every spec field from a Theme.
	loadFields := func(t Theme) {
		for i, role := range themeRoles {
			fields[i].SetText(colorSpec(role.get(t)))
		}
	}

	refresh = func() {
		// Each swatch recomputes from its field on every render (issue #243/#366), so a
		// refresh only needs to redraw — the picker DrawFns pick up the new field values.
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
	reflow() // position rows and hide those below the initial fold before the first paint
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

	resetBtn := newButton("Reset", tv.Rect{X: 2, Y: height - 3, W: 9, H: 1}, reset)
	saveBtn := newButton("Save", tv.Rect{X: width - 24, Y: height - 3, W: 9, H: 1}, save)
	cancelBtn := newButton("Cancel", tv.Rect{X: width - 13, Y: height - 3, W: 10, H: 1}, cancel)
	dialog.Window.AddContent(resetBtn)
	dialog.Window.AddContent(saveBtn)
	dialog.Window.AddContent(cancelBtn)

	dialog.Root().OnTypeFn = func(_ *tv.VisualComponent, event tui.TypeEvent) bool {
		if event.Key == tui.KeyEscape {
			cancel()
			return true
		}
		// Scroll the section viewport with Up/Down and PageUp/PageDown, but only when there
		// is something to scroll — otherwise leave the arrows to the desktop's focus
		// navigation so the editor behaves exactly as before when the content fits. These
		// keys reach here by bubbling up from a focused field, which ignores them.
		if layout.maxScroll() == 0 {
			return false
		}
		switch event.Key {
		case tui.KeyUp:
			scrollTo(scrollY - 1)
		case tui.KeyDown:
			scrollTo(scrollY + 1)
		case tui.KeyPageUp:
			scrollTo(scrollY - layout.visibleRows)
		case tui.KeyPageDown:
			scrollTo(scrollY + layout.visibleRows)
		default:
			return false
		}
		return true
	}

	// relayout re-derives the whole interior from the live dialog bounds when the terminal is
	// resized (issue #317): it re-resolves and re-centres the outer frame, recomputes the
	// layout, and re-places the viewport, every scrolling row, the scrollbar and the action
	// buttons. Without it dialog.Fit would re-resolve only the outer frame and leave the
	// controls clustered in the open-time box; with it a dialog opened small then enlarged
	// matches one opened fresh at the new size. It reads the live bounds rather than the
	// open-time ones, honouring "the renderer must read the live window bounds, not the
	// constants".
	relayout := func() {
		nx, ny, nw, nh := w.dialogRect(spec)
		dialog.Window.Component.SetBounds(tv.Rect{X: nx, Y: ny, W: nw, H: nh})
		layout = resolveThemeEditorLayout(nw, nh)
		viewport.SetBounds(tv.Rect{X: 0, Y: layout.contentTop, W: layout.scrollbarX, H: layout.visibleRows})
		for _, r := range rows {
			x, wdt := cellRect(layout.columns[r.colIdx], r.kind)
			b := r.comp.Bounds
			b.X, b.W = x, wdt
			r.comp.SetBounds(b) // Y is set by reflow() below
		}
		bar.SetBounds(tv.Rect{X: layout.scrollbarX, Y: layout.contentTop, W: 1, H: layout.visibleRows})
		resetBtn.Root().SetBounds(tv.Rect{X: 2, Y: nh - 3, W: 9, H: 1})
		saveBtn.Root().SetBounds(tv.Rect{X: nw - 24, Y: nh - 3, W: 9, H: 1})
		cancelBtn.Root().SetBounds(tv.Rect{X: nw - 13, Y: nh - 3, W: 10, H: 1})
		scrollY = layout.clampScroll(scrollY)
		reflow()
		keepFocusVisible()
	}

	layer = tv.NewModalLayer("theme-editor", dialog)
	w.desktop.AddLayer(layer)
	// Re-resolve and re-flow the dialog (frame + interior) on terminal resize (issues #299/#317).
	layer.OnResize = func(tv.Rect) {
		relayout()
		w.desktop.Redraw()
	}
	w.desktop.SetFocus(preset)
}
