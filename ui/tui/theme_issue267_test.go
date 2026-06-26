package ui

import (
	"sort"
	"strings"
	"testing"

	"gogent/internal/config"

	tv "github.com/hobbestherat/turbotui/turbotv"
)

// This file exercises issue #267 — regrouping the theme editor's colour selectors
// into labelled sections (Session output, UI chrome, Controls, Buttons and inputs,
// Code) rendered as one header-plus-roles section per group, instead of the old
// flat positional two-column halving.
//
// The suite hunts for the defects such a regrouping can introduce:
//   - a role dropped, duplicated, or moved into the wrong group while flattening;
//   - the flattened themeRoles drifting out of step with the on-screen placement
//     order (which silently misindexes fields[i]/swatches[i]);
//   - a section header that wraps, clips, mis-sizes its rule, or fails to render;
//   - the grown layout clipping the last section or colliding with the buttons;
//   - the regrouping not actually grouping (related roles still scattered across
//     columns, the very problem the issue reports).
// All assertions are correctness properties (presence, grouping, no clipping), so
// they do not couple to the exact private column offsets where avoidable.

// ----------------------------------------------------------------------------
// Group structure — the regrouping itself.
// ----------------------------------------------------------------------------

// issue267WantGroups is the section layout the issue specifies, extended with the
// "Buttons and inputs" group the #265 resting roles live under. Order matters: the
// editor stacks groups[:2] in the left column and groups[2:] in the right, and the
// flattened themeRoles is the concatenation in this order.
var issue267WantGroups = []struct {
	title string
	keys  []string
}{
	{"Session output", []string{"user", "agent", "note", "tool", "result", "info", "error"}},
	{"UI chrome", []string{"desktop_fg", "desktop_bg", "panel_fg", "panel_bg", "window_fg", "window_bg", "list_bg", "title", "divider", "accent"}},
	{"Controls", []string{"menu_bar_fg", "menu_bar_bg", "dropdown_fg", "dropdown_bg", "dropdown_focus_fg", "dropdown_focus_bg", "dropdown_select_fg", "dropdown_select_bg"}},
	{"Buttons and inputs", []string{"button_fg", "button_bg", "input_fg", "input_bg", "text_selection_fg", "text_selection_bg"}},
	{"Code", []string{"code_bg"}},
}

// TestIssue267GroupTitlesAndOrder pins the section titles and their order. A reorder
// would break the documented left/right column split (groups[:2] | groups[2:]) and the
// flatten order that fields/swatches are keyed on.
func TestIssue267GroupTitlesAndOrder(t *testing.T) {
	if len(themeGroups) != len(issue267WantGroups) {
		t.Fatalf("len(themeGroups) = %d, want %d", len(themeGroups), len(issue267WantGroups))
	}
	for i, want := range issue267WantGroups {
		if themeGroups[i].title != want.title {
			t.Errorf("themeGroups[%d].title = %q, want %q", i, themeGroups[i].title, want.title)
		}
	}
}

// TestIssue267GroupMembership pins exactly which roles live in each group, in order.
// This is the core of the regrouping: a role landing in the wrong section, or a
// section gaining/losing a role, fails here.
func TestIssue267GroupMembership(t *testing.T) {
	byTitle := make(map[string][]string)
	for _, g := range themeGroups {
		keys := make([]string, len(g.roles))
		for i, r := range g.roles {
			keys[i] = r.key
		}
		byTitle[g.title] = keys
	}
	for _, want := range issue267WantGroups {
		got, ok := byTitle[want.title]
		if !ok {
			t.Errorf("missing group %q", want.title)
			continue
		}
		if strings.Join(got, ",") != strings.Join(want.keys, ",") {
			t.Errorf("group %q roles = %v, want %v", want.title, got, want.keys)
		}
	}
}

// TestIssue267GroupCounts pins each group's size and the total. The two-column layout
// math is sized from these; a wrong count would overflow a column past the buttons
// or leave the dialog mis-sized. (UI chrome gained list_bg in #327.)
func TestIssue267GroupCounts(t *testing.T) {
	wantCounts := []int{7, 10, 8, 6, 1}
	total := 0
	for i, g := range themeGroups {
		if len(g.roles) != wantCounts[i] {
			t.Errorf("group %q has %d roles, want %d", g.title, len(g.roles), wantCounts[i])
		}
		total += len(g.roles)
	}
	if total != 32 {
		t.Errorf("groups hold %d roles total, want 32", total)
	}
}

// TestIssue267FlattenEqualsThemeRoles is the central consistency check: themeRoles
// must be exactly the concatenation of the groups' roles, in section order. Every
// piece of save/round-trip machinery iterates themeRoles by index while the editor
// draws by group; if the two diverge, fields[i] is seeded for one role but drawn under
// another's label — a silent mis-wire. Identity is checked on key, label, AND the
// accessor (via a distinctive per-field probe), since two roles can share a key but
// read different Theme fields.
func TestIssue267FlattenEqualsThemeRoles(t *testing.T) {
	flat := flattenThemeGroups()
	if len(flat) != len(themeRoles) {
		t.Fatalf("flattenThemeGroups() len = %d, themeRoles len = %d", len(flat), len(themeRoles))
	}
	for i := range themeRoles {
		if flat[i].key != themeRoles[i].key {
			t.Errorf("index %d: flatten key %q != themeRoles key %q", i, flat[i].key, themeRoles[i].key)
		}
		if flat[i].label != themeRoles[i].label {
			t.Errorf("index %d (%s): flatten label %q != themeRoles label %q", i, flat[i].key, flat[i].label, themeRoles[i].label)
		}
		// Accessors must read the same field: applying a marker override the role
		// understands must move both accessors identically.
		marked := paletteByName(themeDefault)
		applyOverrides(&marked, map[string]string{themeRoles[i].key: "#12EFA0"})
		if flat[i].get(marked) != themeRoles[i].get(marked) {
			t.Errorf("index %d (%s): flatten and themeRoles accessors diverge: %+v vs %+v",
				i, themeRoles[i].key, flat[i].get(marked), themeRoles[i].get(marked))
		}
	}
}

// TestIssue267EveryRoleInExactlyOneGroup is the explicit non-drop / non-duplicate
// guard the task calls out: every themeRoles key must appear in exactly one group, and
// every group role must be a themeRoles key. A regroup that dropped or double-listed a
// role fails here.
func TestIssue267EveryRoleInExactlyOneGroup(t *testing.T) {
	count := make(map[string]int)
	for _, g := range themeGroups {
		for _, r := range g.roles {
			count[r.key]++
		}
	}
	for _, r := range themeRoles {
		switch count[r.key] {
		case 0:
			t.Errorf("themeRoles key %q is in NO group — it would never be drawn", r.key)
		case 1:
			// good
		default:
			t.Errorf("themeRoles key %q appears in %d groups — it would be drawn twice and double-saved", r.key, count[r.key])
		}
	}
	// And nothing in a group is absent from themeRoles.
	inRoles := make(map[string]bool, len(themeRoles))
	for _, r := range themeRoles {
		inRoles[r.key] = true
	}
	for key := range count {
		if !inRoles[key] {
			t.Errorf("group role %q is missing from themeRoles — its field/swatch index does not exist", key)
		}
	}
}

// TestIssue267EveryGroupKeyHonouredByApplyOverrides ensures every grouped (and thus
// drawn-and-editable) role is actually wired into applyOverrides. A role shown in a
// section whose key applyOverrides ignores would accept an edit that silently never
// round-trips.
func TestIssue267EveryGroupKeyHonouredByApplyOverrides(t *testing.T) {
	marker, ok := parseColor("#12EFA0")
	if !ok {
		t.Fatalf("setup: parseColor failed")
	}
	for _, g := range themeGroups {
		for _, role := range g.roles {
			got := paletteByName(themeDefault)
			applyOverrides(&got, map[string]string{role.key: "#12EFA0"})
			if role.get(got) != marker {
				t.Errorf("group %q role %q: applyOverrides did not set it — an editable role whose override is dropped",
					g.title, role.key)
			}
		}
	}
}

// TestIssue267FocusRolesNotGrouped guards the documented exclusion: the #265 focus
// pairs are deliberately kept out of the editor (layout ceiling) and carried instead.
// If one slipped into a group it would be drawn as a sixth section row and push the
// grid past the menu-bar height ceiling. (Their preservation is covered by the #265
// carryUnexposedOverrides suite.)
func TestIssue267FocusRolesNotGrouped(t *testing.T) {
	focus := []string{"button_focus_fg", "button_focus_bg", "input_focus_fg", "input_focus_bg"}
	grouped := make(map[string]bool)
	for _, g := range themeGroups {
		for _, r := range g.roles {
			grouped[r.key] = true
		}
	}
	for _, key := range focus {
		if grouped[key] {
			t.Errorf("focus role %q is in a group — it must stay out of the editor (carried by carryUnexposedOverrides)", key)
		}
	}
}

// ----------------------------------------------------------------------------
// themeSectionHeader — the section-divider label.
// ----------------------------------------------------------------------------

// runeWidth counts display cells (runes) in s, the unit the 1-row Label is laid out in.
func runeWidth(s string) int { return len([]rune(s)) }

// TestIssue267SectionHeaderFillsExactWidth pins the header's contract: when there is
// room for a rule, the text is the title, one space, then a "─" rule that fills the
// cell to *exactly* r.W runes — no more (which would wrap the 1-row Label and drop the
// rule) and no less (a short rule reads as a ragged divider).
func TestIssue267SectionHeaderFillsExactWidth(t *testing.T) {
	cases := []struct {
		title string
		w     int
	}{
		{"Session output", 36},
		{"UI chrome", 36},
		{"Controls", 38},
		{"Buttons and inputs", 38},
		{"Code", 38},
		{"X", 10},
	}
	for _, c := range cases {
		t.Run(c.title, func(t *testing.T) {
			lbl := themeSectionHeader(c.title, tv.Rect{X: 0, Y: 0, W: c.w, H: 1})
			got := lbl.GetText()
			if w := runeWidth(got); w != c.w {
				t.Errorf("themeSectionHeader(%q, W=%d).text width = %d (%q), want exactly %d — a wider text wraps the rule away, a narrower one is a ragged divider",
					c.title, c.w, w, got, c.w)
			}
			if !strings.HasPrefix(got, c.title+" ") {
				t.Errorf("header %q does not start with the title and a space: %q", c.title, got)
			}
			rule := strings.TrimPrefix(got, c.title+" ")
			if rule != strings.Repeat("─", runeWidth(rule)) {
				t.Errorf("header fill is not a solid ─ rule: %q", rule)
			}
			if len([]rune(rule)) == 0 {
				t.Errorf("header %q (W=%d) has no rule — it does not read as a section divider", c.title, c.w)
			}
		})
	}
}

// TestIssue267SectionHeaderNoColon distinguishes a header from a role label: the role
// labels all end in ":", and the header must not, or the eye cannot tell the divider
// from a value row.
func TestIssue267SectionHeaderNoColon(t *testing.T) {
	for _, g := range themeGroups {
		lbl := themeSectionHeader(g.title, tv.Rect{X: 0, Y: 0, W: 38, H: 1})
		if strings.HasSuffix(strings.TrimRight(lbl.GetText(), "─ "), ":") {
			t.Errorf("section header for %q ends with ':' like a role label: %q", g.title, lbl.GetText())
		}
	}
}

// TestIssue267SectionHeaderNarrowWidthNoPanic covers the degenerate widths: when the
// cell cannot hold "title + space + ≥1 rule rune" the header must fall back to the bare
// title (never a negative strings.Repeat, never a wrapped over-width string).
func TestIssue267SectionHeaderNarrowWidthNoPanic(t *testing.T) {
	title := "Controls"
	for _, w := range []int{0, 1, len(title) - 1, len(title), len(title) + 1} {
		lbl := themeSectionHeader(title, tv.Rect{X: 0, Y: 0, W: w, H: 1})
		got := lbl.GetText()
		// With no room for a rule (w <= len(title)+1) the bare title is returned.
		if w <= len([]rune(title))+1 {
			if got != title {
				t.Errorf("W=%d: header = %q, want bare title %q (no rule fits)", w, got, title)
			}
		}
		if strings.Contains(got, "─") && runeWidth(got) > w && w > 0 {
			t.Errorf("W=%d: header %q is wider than its cell and would wrap", w, got)
		}
	}
}

// ----------------------------------------------------------------------------
// Rendered layout — black-box, drives the real editor.
// ----------------------------------------------------------------------------

// issue267Render opens the real theme editor over a default config and returns the
// rendered screen as rows of runes (one rune per cell, so a rune index equals a screen
// column even on rows carrying wide glyphs like ▉ and ─).
func issue267Render(t *testing.T) (*Workbench, [][]rune) {
	t.Helper()
	issue204RestoreTheme(t)
	w := newTestWorkbench(t)
	w.SetHandlers(Handlers{
		GetTheme: func() config.ThemeConfig { return config.ThemeConfig{} },
		SetTheme: func(config.ThemeConfig) {},
	})
	// Since issue #477 the editor floors at 83 wide, so render on an 83-column terminal:
	// on the 80-column default the 83-wide dialog clips its rightmost columns and the last
	// role's (code_bg) field value truncates off-screen. At exactly 83 wide the dialog
	// still sits at x=0 (no centring offset), so frame-at-col-0 assertions are unchanged;
	// the editor internals are identical (the dialog resolves to the same 83×22 floor).
	w.app.Resize(83, 25)
	w.showThemeEditor()
	w.desktop.Redraw()
	rows := make([][]rune, w.app.Height())
	for y := 0; y < w.app.Height(); y++ {
		row := make([]rune, w.app.Width())
		for x := 0; x < w.app.Width(); x++ {
			ch := w.app.ReadCell(x, y).Ch
			if ch == 0 {
				ch = ' '
			}
			row[x] = ch
		}
		rows[y] = row
	}
	return w, rows
}

// findRunes locates needle in the rune grid, scanning top-to-bottom then
// left-to-right, returning the (row, col) of its first rune. col is a screen column.
func findRunes(rows [][]rune, needle string) (row, col int, ok bool) {
	n := []rune(needle)
	for y, r := range rows {
		for x := 0; x+len(n) <= len(r); x++ {
			match := true
			for k := range n {
				if r[x+k] != n[k] {
					match = false
					break
				}
			}
			if match {
				return y, x, true
			}
		}
	}
	return 0, 0, false
}

// TestIssue267AllSectionHeadersRender is the headline feature check: every section
// title renders on screen as a header (title followed by its ─ rule). A forgotten or
// clipped header is the regression this guards. The "+ ─" suffix ensures we match the
// header row, not an incidental substring (e.g. "Code" inside "Code block background").
func TestIssue267AllSectionHeadersRender(t *testing.T) {
	w, _ := issue267Render(t)
	needles := make([]string, 0, len(themeGroups))
	for _, g := range themeGroups {
		needles = append(needles, g.title+" ─")
	}
	pos := editorScrollFind(w, needles)
	for _, g := range themeGroups {
		if !pos[g.title+" ─"].found {
			t.Errorf("section header %q (title + ─ rule) is not on screen at any scroll offset — the grouped layout is missing its heading", g.title)
		}
	}
}

// issue267labelColumn classifies a screen column as the left (col < 40) or right
// section column. The dialog spans the full 80-wide buffer with the left column near
// col 3 and the right near col 41, so 40 is a safe divider that does not couple to the
// exact private offsets.
func issue267labelColumn(col int) string {
	if col < 40 {
		return "left"
	}
	return "right"
}

// TestIssue267RolesGroupedContiguouslyInOneColumn is the direct test of the reported
// defect ("session colours split across both columns and interleaved with chrome"):
// each group's roles must sit in a SINGLE column on CONSECUTIVE rows, directly under
// that group's header. If a group's roles were scattered across columns or interleaved
// with another group's, this fails.
func TestIssue267RolesGroupedContiguouslyInOneColumn(t *testing.T) {
	w, _ := issue267Render(t)
	// Collect every header and role label across all scroll offsets, with each one's column
	// and column-local logical row, so the grouping holds end-to-end even though no single
	// frame shows the whole column.
	var needles []string
	for _, g := range themeGroups {
		needles = append(needles, g.title+" ─")
		for _, role := range g.roles {
			needles = append(needles, issue243WantLabels[role.key]+":")
		}
	}
	pos := editorScrollFind(w, needles)

	for _, g := range themeGroups {
		hp := pos[g.title+" ─"]
		if !hp.found {
			t.Errorf("group %q: header not found at any offset", g.title)
			continue
		}
		var roleRows []int
		for _, role := range g.roles {
			rp := pos[issue243WantLabels[role.key]+":"]
			if !rp.found {
				t.Errorf("group %q: role label %q not on screen at any offset", g.title, role.key)
				continue
			}
			if rp.isRight != hp.isRight {
				t.Errorf("group %q role %q renders in a different column from its header — the group is split across columns", g.title, role.key)
			}
			roleRows = append(roleRows, rp.logical)
		}
		if len(roleRows) != len(g.roles) {
			continue
		}
		// Logical rows must be the header row + 1, +2, … contiguous with no gap.
		sort.Ints(roleRows)
		if roleRows[0] != hp.logical+1 {
			t.Errorf("group %q: first role is on logical row %d but the header is on row %d — they are not adjacent", g.title, roleRows[0], hp.logical)
		}
		for i := 1; i < len(roleRows); i++ {
			if roleRows[i] != roleRows[i-1]+1 {
				t.Errorf("group %q: role rows are not contiguous (%v) — another section's row is interleaved", g.title, roleRows)
				break
			}
		}
	}
}

// TestIssue267OnScreenOrderMatchesThemeRoles reads every role label by (column, row)
// and reconstructs the placement order the editor walked (left column top-down, then
// right column top-down). That order MUST equal themeRoles, because fields[i]/
// swatches[i] are seeded and saved by the flat index i while drawn at the i-th walked
// slot. A divergence means a field is seeded for one role but shown/saved under
// another — the silent mis-wire the grouping must not introduce.
func TestIssue267OnScreenOrderMatchesThemeRoles(t *testing.T) {
	w, _ := issue267Render(t)
	// Reconstruct each role's (column, logical row) across all scroll offsets, then order
	// the labels the way the editor walks them (left column top-down, then right column
	// top-down) and assert that equals themeRoles. The scroll viewport never shows the whole
	// column at once, so the order is recovered from logical rows, not a single frame.
	type pos struct {
		key     string
		isRight bool
		logical int
	}
	needles := make([]string, 0, len(themeRoles))
	for _, role := range themeRoles {
		needles = append(needles, issue243WantLabels[role.key]+":")
	}
	found := editorScrollFind(w, needles)
	var got []pos
	for _, role := range themeRoles {
		p := found[issue243WantLabels[role.key]+":"]
		if !p.found {
			t.Fatalf("role %q label not on screen at any scroll offset", role.key)
		}
		got = append(got, pos{role.key, p.isRight, p.logical})
	}
	sort.SliceStable(got, func(i, j int) bool {
		if got[i].isRight != got[j].isRight {
			return !got[i].isRight // left column first
		}
		return got[i].logical < got[j].logical
	})
	for i, role := range themeRoles {
		if got[i].key != role.key {
			t.Errorf("on-screen placement slot %d is role %q, but themeRoles[%d] is %q — fields/swatches are indexed by themeRoles order, so this slot shows/saves the wrong colour",
				i, got[i].key, i, role.key)
		}
	}
}

// TestIssue267FieldsSeededInPlacementOrder verifies, per role, that the spec value the
// editor seeds beside that role's label equals the role's own colour in the seeded
// theme. This catches a placement/flatten mis-index that TestIssue267OnScreenOrder
// might miss if labels and fields were shifted together: here the *value* must match
// the *label's* role, end to end through the real loadFields path.
func TestIssue267FieldsSeededInPlacementOrder(t *testing.T) {
	w, _ := issue267Render(t)
	seeded := editedTheme(config.ThemeConfig{})
	for _, role := range themeRoles {
		label := issue243WantLabels[role.key] + ":"
		// The role may sit below the initial fold; scroll it into view, then read the field
		// token from the frame on which it is visible.
		if !scrollEditorToReveal(w, label) {
			t.Fatalf("role %q label not on screen at any scroll offset", role.key)
		}
		rows := editorGrid(w)
		r, c, ok := findRunes(rows, label)
		if !ok {
			t.Fatalf("role %q label vanished after scrolling it into view", role.key)
		}
		// The spec field is the first token after the label cell, fieldW columns wide.
		// Cap the read at fieldW: since issue #462 the swatch leads the row so the field
		// sits at the far right and the rightmost role's field abuts the scrollbar column
		// with no trailing space — a greedy whitespace read would run past the value into
		// the scrollbar glyphs.
		p := c + len([]rune(label))
		row := rows[r]
		for p < len(row) && row[p] == ' ' {
			p++
		}
		start := p
		for p < len(row) && row[p] != ' ' && p-start < themeEditorFieldW {
			p++
		}
		token := string(row[start:p])
		want := colorSpec(role.get(seeded))
		if token != want {
			t.Errorf("role %q: field beside its label shows %q, want %q (the role's own seeded colour) — the field is mis-indexed against its label",
				role.key, token, want)
		}
	}
}

// TestIssue267LastSectionClearsButtons guards the grown-dialog height math: the last
// right-column role (code_bg) must render strictly above the action buttons, and Reset/
// Save/Cancel must all be visible. The right column exactly fills its 16 rows, so a
// single mis-count would push code_bg onto the button row and clip one or the other.
func TestIssue267LastSectionClearsButtons(t *testing.T) {
	w, _ := issue267Render(t)

	// Buttons are fixed and visible before any scrolling.
	for _, btn := range []string{"Reset", "Save", "Cancel"} {
		if _, _, ok := findRunes(editorGrid(w), btn); !ok {
			t.Errorf("%s button is not visible — the layout pushed it off the dialog", btn)
		}
	}

	// Scroll code_bg into view, then assert it lands above the Save button on that frame.
	if !scrollEditorToReveal(w, "Code block background:") {
		t.Fatalf("code_bg never became visible after scrolling — the last section is unreachable")
	}
	rows := editorGrid(w)
	codeRow, _, ok := findRunes(rows, "Code block background:")
	if !ok {
		t.Fatalf("code_bg vanished after scrolling it into view")
	}
	saveRow, _, ok := findRunes(rows, "Save")
	if !ok {
		t.Fatalf("Save button not on screen")
	}
	if codeRow >= saveRow {
		t.Errorf("code_bg is on row %d but the Save button is on row %d — the scrolled section collides with or sits below the buttons", codeRow, saveRow)
	}
}

// TestIssue267DialogFitsTwentyFourRowTerminal checks the height ceiling the code
// documents: centred at height 22 the dialog must clear the always-on-top menu bar
// (row 0) and fit within a 24-row terminal — top frame at row ≥1, bottom frame present,
// every section between them. The headless app is ≥24 rows, so this asserts the dialog's
// own frame fits in a 24-row budget rather than resizing the buffer.
func TestIssue267DialogFitsTwentyFourRowTerminal(t *testing.T) {
	w, rows := issue267Render(t)

	topRow, bottomRow := -1, -1
	for y, r := range rows {
		if len(r) > 0 && r[0] == '╔' {
			topRow = y
		}
		if len(r) > 0 && r[0] == '╚' {
			bottomRow = y
		}
	}
	if topRow < 0 || bottomRow < 0 {
		t.Fatalf("could not locate the dialog frame (top=%d bottom=%d)", topRow, bottomRow)
	}
	if topRow < 1 {
		t.Errorf("dialog top frame is on row %d — it overwrites the always-on-top menu bar at row 0", topRow)
	}
	// The dialog stays at its fixed 22-row height regardless of scrolling — the content
	// scrolls inside it, the frame does not grow — so it must fit a 24-row terminal.
	if h := bottomRow - topRow + 1; h > 24 {
		t.Errorf("dialog frame spans %d rows (top=%d bottom=%d) — taller than a 24-row terminal", h, topRow, bottomRow)
	}
	// Every section header must, when scrolled into view, sit strictly inside the (fixed)
	// dialog frame — the scroll viewport lives between the frames, so no header escapes it.
	for _, g := range themeGroups {
		if !scrollEditorToReveal(w, g.title+" ─") {
			t.Errorf("section %q header never became visible inside the frame", g.title)
			continue
		}
		hr, _, ok := findRunes(editorGrid(w), g.title+" ─")
		if !ok {
			t.Errorf("section %q header vanished after scrolling it into view", g.title)
			continue
		}
		if hr <= topRow || hr >= bottomRow {
			t.Errorf("section %q header on row %d is outside the dialog frame (%d..%d)", g.title, hr, topRow, bottomRow)
		}
	}
}

// TestIssue267HeadersRenderUnderNoColor confirms the section headers stay legible with
// colours disabled: they are dialog-coloured labels, so NO_COLOR must not blank them.
// A header sourced from a now-neutralised role colour (a plausible mistake) would
// vanish here.
func TestIssue267HeadersRenderUnderNoColor(t *testing.T) {
	issue204RestoreTheme(t)
	w := newTestWorkbench(t)
	w.SetHandlers(Handlers{
		GetTheme: func() config.ThemeConfig { return config.ThemeConfig{NoColor: true} },
		SetTheme: func(config.ThemeConfig) {},
	})
	w.showThemeEditor()
	// Headers below the initial fold (e.g. "Code") only render after scrolling, so aggregate
	// the rendered rows across every scroll offset before asserting each header is present.
	screen := editorScrollAggregate(t, w)
	for _, g := range themeGroups {
		if !containsOnScreen(screen, g.title) {
			t.Errorf("section header %q vanished under NO_COLOR", g.title)
		}
	}
}

// ----------------------------------------------------------------------------
// Editor open/guard paths.
// ----------------------------------------------------------------------------

// TestIssue267EditorUnavailableWithoutHandlers covers the guard at the top of
// showThemeEditor: with no GetTheme/SetTheme wired it must surface the "unavailable"
// message and never panic walking the (un-built) grouped widgets.
func TestIssue267EditorUnavailableWithoutHandlers(t *testing.T) {
	issue204RestoreTheme(t)
	w := newTestWorkbench(t)
	// No handlers set.
	w.showThemeEditor()
	screen := screenText(w)
	if !containsOnScreen(screen, "unavailable") {
		t.Errorf("expected the 'Theme editing is unavailable.' notice; screen lacked it")
	}
	// And no section header should have been drawn.
	if containsOnScreen(screen, "Session output") {
		t.Errorf("a section header rendered despite missing handlers")
	}
}
