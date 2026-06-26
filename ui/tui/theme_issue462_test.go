package ui

import (
	"strings"
	"sync"
	"testing"

	"gogent/internal/config"

	tui "github.com/hobbestherat/turbotui"
	tv "github.com/hobbestherat/turbotui/turbotv"
)

// Issue #462 — theme editor: clearer colour/line association (Goal 1) and saved
// custom themes with a Save As / Delete / edit-in-place flow (Goal 2).
//
// The tests are organised in three tiers:
//  1. pure logic (findSavedThemeByName, separator row-counting, carryUnexposed
//     override carry-over) — no UI, maximally robust;
//  2. rendering (swatch leads the label, section separators, scroll reach, the
//     dropdown lists saved themes, graceful degradation without handlers);
//  3. full UI flows driven through the real widgets (Save As, duplicate-name
//     confirm, Delete active vs. non-active, Save routing, edit+save of a saved
//     entry, carryUnexposedOverrides through a saved Save, the SavedName back-link
//     reselect on open).
//
// They aim to surface real defects against the four design criteria, especially
// the cross-reopen identity gap, silent overwrite, wrong-target delete reset, and
// post-mutation dropdown/field consistency.

// ----------------------------------------------------------------------------
// Test harness: a recording handler set + editor open/button-driving helpers.
// ----------------------------------------------------------------------------

// theme462Store is a recording stand-in for the theme persistence handlers. It
// mirrors Gogent's contract (GetTheme/SetTheme/GetSavedThemes/SetSavedThemes)
// faithfully enough to exercise the editor: GetSavedThemes returns a deep copy,
// and SetSavedThemes snapshots its argument so the editor's later in-place
// mutations of its working slice can't retroactively rewrite history.
type theme462Store struct {
	mu       sync.Mutex
	active   config.ThemeConfig
	saved    []config.NamedTheme
	setTheme []config.ThemeConfig
	setSaved [][]config.NamedTheme
}

func (s *theme462Store) handlers() Handlers {
	return Handlers{
		GetTheme: func() config.ThemeConfig {
			s.mu.Lock()
			defer s.mu.Unlock()
			return s.active
		},
		SetTheme: func(t config.ThemeConfig) {
			s.mu.Lock()
			defer s.mu.Unlock()
			s.active = t
			s.setTheme = append(s.setTheme, t)
		},
		GetSavedThemes: func() []config.NamedTheme {
			s.mu.Lock()
			defer s.mu.Unlock()
			return cloneNamedThemes(s.saved)
		},
		SetSavedThemes: func(t []config.NamedTheme) {
			s.mu.Lock()
			defer s.mu.Unlock()
			s.saved = cloneNamedThemes(t)
			s.setSaved = append(s.setSaved, cloneNamedThemes(t))
		},
	}
}

// cloneNamedThemes deep-copies a saved-theme slice (slice + per-entry override
// maps) so callers/recordings cannot alias one another.
func cloneNamedThemes(in []config.NamedTheme) []config.NamedTheme {
	if len(in) == 0 {
		return nil
	}
	out := make([]config.NamedTheme, len(in))
	for i, nt := range in {
		cp := nt
		if nt.Theme.Overrides != nil {
			cp.Theme.Overrides = make(map[string]string, len(nt.Theme.Overrides))
			for k, v := range nt.Theme.Overrides {
				cp.Theme.Overrides[k] = v
			}
		}
		out[i] = cp
	}
	return out
}

// openThemeEditor462 opens the real editor over a recording handler set and
// returns the workbench and the store. The active theme and saved list are seeded
// from the store before opening.
func openThemeEditor462(t *testing.T, st *theme462Store) *Workbench {
	t.Helper()
	issue204RestoreTheme(t)
	w := newTestWorkbench(t)
	w.SetHandlers(st.handlers())
	w.showThemeEditor()
	w.desktop.Redraw()
	return w
}

// openThemeEditor462Raw opens the editor with an explicit Handlers set (for tests
// that need handler configurations the store does not cover, e.g. nil handlers).
func openThemeEditor462Raw(t *testing.T, h Handlers) *Workbench {
	t.Helper()
	issue204RestoreTheme(t)
	w := newTestWorkbench(t)
	w.SetHandlers(h)
	w.showThemeEditor()
	w.desktop.Redraw()
	return w
}

// themeEditorFooterButtonRect is the window-relative rect showThemeEditor lays out
// for each footer button. Computing it from the live dialog bounds (not a hardcoded
// 80×22) keeps the tests honest if the floor ever moves.
func themeEditorFooterButtonRect(w *Workbench, label string) tv.Rect {
	b := dialogBounds(w)
	y := b.H - 3
	switch label {
	case "Reset":
		return tv.Rect{X: 2, Y: y, W: 9, H: 1}
	case "Save As…":
		return tv.Rect{X: 12, Y: y, W: 11, H: 1}
	case "Delete":
		return tv.Rect{X: 24, Y: y, W: 10, H: 1}
	case "Save":
		return tv.Rect{X: b.W - 24, Y: y, W: 9, H: 1}
	case "Cancel":
		return tv.Rect{X: b.W - 13, Y: y, W: 10, H: 1}
	}
	return tv.Rect{}
}

// clickThemeButton drives a full press (down then up at the button centre) of the
// theme-editor footer button whose laid-out Bounds match the label's rect — the
// exact path a real mouse click takes, so the OnPress closure runs. It matches by
// rect (not label) because "Save" is a substring of "Save As…" and would otherwise
// resolve to the wrong button.
func clickThemeButton(t *testing.T, w *Workbench, label string) {
	t.Helper()
	want := themeEditorFooterButtonRect(w, label)
	for _, c := range dialogDescendants(w) {
		if c.Bounds != want || !c.DrawOutside || c.OnClickFn == nil {
			continue
		}
		abs := c.AbsoluteBounds()
		cx, cy := abs.X+abs.W/2, abs.Y+abs.H/2
		c.OnClickFn(c, tui.ClickEvent{X: cx, Y: cy, Down: true})
		c.OnClickFn(c, tui.ClickEvent{X: cx, Y: cy, Down: false})
		return
	}
	t.Fatalf("theme editor %q button (rect %+v) not found in the top layer", label, want)
}

// clickTopButtonByText clicks the top dialog's button whose rendered label matches,
// by locating the label on screen and pressing the DrawOutside button whose
// absolute bounds contain that cell. Used for distinctive labels (Yes/No/OK) where
// no two buttons share a prefix.
func clickTopButtonByText(t *testing.T, w *Workbench, label string) {
	t.Helper()
	row, col, ok := findRunes(editorGrid(w), label)
	if !ok {
		t.Fatalf("button label %q not found on screen", label)
	}
	for _, c := range dialogDescendants(w) {
		if !c.DrawOutside || c.OnClickFn == nil {
			continue
		}
		abs := c.AbsoluteBounds()
		if col < abs.X || col >= abs.X+abs.W || row < abs.Y || row >= abs.Y+abs.H {
			continue
		}
		cx, cy := abs.X+abs.W/2, abs.Y+abs.H/2
		c.OnClickFn(c, tui.ClickEvent{X: cx, Y: cy, Down: true})
		c.OnClickFn(c, tui.ClickEvent{X: cx, Y: cy, Down: false})
		return
	}
	t.Fatalf("no button bounds contain label %q at (%d,%d)", label, col, row)
}

// findPresetSelect returns the preset dropdown component (the focusable Select at
// the editor's preset rect), so tests can open its popup.
func findPresetSelect(w *Workbench) *tv.VisualComponent {
	want := tv.Rect{X: 10, Y: 1, W: 30, H: 1}
	for _, c := range dialogDescendants(w) {
		if c.Bounds == want && c.Focusable {
			return c
		}
	}
	return nil
}

// pickPresetOption opens the preset dropdown and commits the option whose rendered
// label matches, by clicking its row in the popup (popupClick maps a click on an
// option row to its index). This is how a user selects a saved theme or a different
// built-in.
func pickPresetOption(t *testing.T, w *Workbench, optionLabel string) {
	t.Helper()
	sel := findPresetSelect(w)
	if sel == nil {
		t.Fatalf("preset dropdown not found")
	}
	sel.OnTypeFn(sel, tui.TypeEvent{Key: tui.KeyEnter}) // open the popup
	w.desktop.Redraw()
	row, col, ok := findRunes(editorGrid(w), optionLabel)
	if !ok {
		t.Fatalf("option %q not rendered in the open dropdown popup", optionLabel)
	}
	top := w.desktop.TopLayer()
	if top == nil {
		t.Fatalf("dropdown popup did not open")
	}
	// Click the option row to commit it (down then up, as a real click).
	top.Root.OnClickFn(top.Root, tui.ClickEvent{X: col, Y: row, Down: true})
	top.Root.OnClickFn(top.Root, tui.ClickEvent{X: col, Y: row, Down: false})
	w.desktop.Redraw()
}

// setRoleField replaces the spec field beside the given role label with spec. It
// finds the editable field (CopyFn != nil) on the label's screen row immediately
// to the right of the label, then selects-all (Ctrl+A) and types the new spec.
func setRoleField(t *testing.T, w *Workbench, label, spec string) {
	t.Helper()
	grid := editorGrid(w)
	row, col, ok := findRunes(grid, label)
	if !ok {
		t.Fatalf("role label %q not visible — scroll it into view first", label)
	}
	labelEnd := col + len([]rune(label))
	var best *tv.VisualComponent
	bestDX := 1 << 30
	for _, c := range dialogDescendants(w) {
		if c.CopyFn == nil {
			continue
		}
		abs := c.AbsoluteBounds()
		if abs.Y != row || abs.X <= labelEnd {
			continue
		}
		if dx := abs.X - labelEnd; dx < bestDX {
			bestDX = dx
			best = c
		}
	}
	if best == nil {
		t.Fatalf("no editable field found right of %q on row %d", label, row)
	}
	best.BubbleType(tui.TypeEvent{Key: tui.KeyRune, Rune: 'a', Ctrl: true}) // select all
	for _, r := range spec {
		best.BubbleType(tui.TypeEvent{Key: tui.KeyRune, Rune: r})
	}
	w.desktop.Redraw()
}

// fieldSpecAfterLabel reads the spec token rendered immediately after a role label
// (the first whitespace-delimited token past the label), mirroring how
// TestIssue267FieldsSeededInPlacementOrder reads a field value.
func fieldSpecAfterLabel(t *testing.T, w *Workbench, label string) string {
	t.Helper()
	grid := editorGrid(w)
	r, c, ok := findRunes(grid, label)
	if !ok {
		t.Fatalf("label %q not visible", label)
	}
	p := c + len([]rune(label))
	row := grid[r]
	for p < len(row) && row[p] == ' ' {
		p++
	}
	start := p
	for p < len(row) && row[p] != ' ' {
		p++
	}
	return string(row[start:p])
}

// presetShows reports whether the closed preset dropdown currently displays needle
// (its selected value), i.e. the entry is the one selected on screen.
func presetShows(w *Workbench, needle string) bool {
	return strings.Contains(screenText(w), needle)
}

// saveAsViaUI clicks Save As…, types name into the input dialog, and submits. If
// the name already exists the editor opens a confirm; confirmOverwrite says whether
// to accept (Yes) or decline (No) it.
func saveAsViaUI(t *testing.T, w *Workbench, name string, confirmOverwrite bool) {
	t.Helper()
	clickThemeButton(t, w, "Save As…")
	box := inputDialogBox(t, w) // asserts an input-dialog layer is on top
	for _, r := range name {
		typeDlgRune(box, r)
	}
	submitDlg(box) // Enter → onResult; for a duplicate name this opens a confirm
	w.desktop.Redraw()
	// A confirm layer only appears on a duplicate name.
	if top := w.desktop.TopLayer(); top != nil && top.Name == "confirm-dialog" {
		clickTopButtonByText(t, w, confirmLabel(confirmOverwrite))
		w.desktop.Redraw()
	}
}

// confirmLabel is the button text to click for a yes/no decision on the confirm.
func confirmLabel(yes bool) string {
	if yes {
		return "Yes"
	}
	return "No"
}

// deleteViaUI clicks Delete and answers the confirm with yes/no. No-op safety (the
// button may be absent or the confirm may not appear for a built-in) is handled.
func deleteViaUI(t *testing.T, w *Workbench, confirm bool) {
	t.Helper()
	clickThemeButton(t, w, "Delete")
	w.desktop.Redraw()
	if top := w.desktop.TopLayer(); top != nil && top.Name == "confirm-dialog" {
		clickTopButtonByText(t, w, confirmLabel(confirm))
		w.desktop.Redraw()
	}
}

// ----------------------------------------------------------------------------
// Tier 1 — pure logic.
// ----------------------------------------------------------------------------

// TestFindSavedThemeByName covers the duplicate/identity lookup Save As and the
// back-link both rely on: case-insensitive, whitespace-tolerant, -1 on miss.
func TestFindSavedThemeByName(t *testing.T) {
	themes := []config.NamedTheme{
		{Name: "Mine", Theme: config.ThemeConfig{Name: "default"}},
		{Name: "Café Dark", Theme: config.ThemeConfig{Name: "dark"}},
	}
	cases := []struct {
		name string
		want int
	}{
		{"Mine", 0},
		{"mine", 0},      // case-insensitive
		{"  mine  ", 0},  // trimmed
		{"MINE", 0},      // all-caps
		{"Café Dark", 1}, // unicode
		{"café dark", 1}, // unicode case-insensitive
		{"Other", -1},    // miss
		{"", -1},         // empty never matches a real name
		{"Min", -1},      // prefix is not a match
	}
	for _, c := range cases {
		if got := findSavedThemeByName(themes, c.name); got != c.want {
			t.Errorf("findSavedThemeByName(%q) = %d, want %d", c.name, got, c.want)
		}
	}
	if got := findSavedThemeByName(nil, "Mine"); got != -1 {
		t.Errorf("findSavedThemeByName(nil,…) = %d, want -1", got)
	}
}

// TestThemeEditorColumnRowsCountsSectionSeparators verifies the separator count
// matches what the build loop inserts: one blank row between each pair of adjacent
// sections in a column, none for a single-section column. A mismatch would desync
// maxScroll/scrollbar (criterion 3 watchlist #1).
func TestThemeEditorColumnRowsCountsSectionSeparators(t *testing.T) {
	cols := themeEditorColumns()
	if len(cols) != 2 {
		t.Fatalf("expected 2 columns, got %d", len(cols))
	}
	// Manual count: header + roles per group, plus (1 separator + col.sectionPad) rows
	// per inter-section gap. The left column carries sectionPad=1 so its second section
	// (UI chrome) drops onto the same logical row as the right column's second section
	// (Buttons and inputs); the right column keeps sectionPad=0.
	wantRows := func(col themeEditorColumn) int {
		rows := 0
		for _, g := range col.groups {
			rows += 1 + len(g.roles)
		}
		if len(col.groups) > 1 {
			rows += (len(col.groups) - 1) * (1 + col.sectionPad)
		}
		return rows
	}
	left, right := cols[0], cols[1]
	if left.sectionPad != 1 {
		t.Errorf("left column sectionPad = %d, want 1 (aligns UI chrome with Buttons and inputs)", left.sectionPad)
	}
	if right.sectionPad != 0 {
		t.Errorf("right column sectionPad = %d, want 0", right.sectionPad)
	}
	if got, want := themeEditorColumnRows(left), wantRows(left); got != want {
		t.Errorf("left column rows = %d, want %d (separators/pad miscounted)", got, want)
	}
	if got, want := themeEditorColumnRows(right), wantRows(right); got != want {
		t.Errorf("right column rows = %d, want %d (separators/pad miscounted)", got, want)
	}

	// A single-group column must add NO separator (the guard against off-by-one).
	single := themeEditorColumn{groups: []themeGroup{{title: "Only", roles: []themeRole{{}, {}}}}}
	if got := themeEditorColumnRows(single); got != 3 { // 1 header + 2 roles
		t.Errorf("single-group column rows = %d, want 3 (no separator expected)", got)
	}

	// The full content height is the tallest column; with the #462 separators it is
	// strictly taller than the no-separator count, so the viewport scrolls a little
	// more (explicitly allowed by the issue).
	noSep := 0
	for _, col := range cols {
		r := 0
		for _, g := range col.groups {
			r += 1 + len(g.roles)
		}
		if r > noSep {
			noSep = r
		}
	}
	if themeEditorContentRows() <= noSep {
		t.Errorf("content rows = %d, expected > %d once separators are counted", themeEditorContentRows(), noSep)
	}

	// The left column's first section (Session output: 1 header + 7 roles = 8 rows) is
	// one row shorter than the right's (Controls: 1 header + 8 roles = 9 rows). Without
	// padding the left's second section (UI chrome) would land one row above the right's
	// (Buttons and inputs). sectionPad=1 on the left column widens its single gap to two
	// rows so the two second-section headers share a logical row, while both first-section
	// headers still share logical row 0.
	left, right = cols[0], cols[1]
	leftSecondStart := 1 + len(left.groups[0].roles) + 1 + left.sectionPad // header+roles+sep+pad
	rightSecondStart := 1 + len(right.groups[0].roles) + 1 + right.sectionPad
	if leftSecondStart != rightSecondStart {
		t.Errorf("second sections misaligned: left UI chrome at logical %d, right Buttons and inputs at %d",
			leftSecondStart, rightSecondStart)
	}
}

// TestIssue471SecondSectionsAlign is the rendered counterpart of the row-count
// alignment check: the UI chrome and Buttons and inputs section headers land on the
// SAME screen row (and thus the same logical row), so the two columns' second
// sections read as a aligned pair rather than staggered by one row.
func TestIssue471SecondSectionsAlign(t *testing.T) {
	w := openThemeEditor462Raw(t, Handlers{
		GetTheme: func() config.ThemeConfig { return config.ThemeConfig{} },
		SetTheme: func(config.ThemeConfig) {},
	})
	pos := editorScrollFind(w, []string{"UI chrome ─", "Buttons and inputs ─"})
	uiChrome := pos["UI chrome ─"]
	buttons := pos["Buttons and inputs ─"]
	if !uiChrome.found || !buttons.found {
		t.Fatal("could not locate the UI chrome or Buttons and inputs section header")
	}
	if uiChrome.logical != buttons.logical {
		t.Errorf("second sections not aligned: UI chrome at logical %d, Buttons and inputs at %d",
			uiChrome.logical, buttons.logical)
	}
	// Sanity: the first sections are still aligned at logical row 0.
	first := editorScrollFind(w, []string{"Session output ─", "Controls ─"})
	if first["Session output ─"].found && first["Controls ─"].found {
		if first["Session output ─"].logical != first["Controls ─"].logical {
			t.Errorf("first sections drifted: Session output at %d, Controls at %d",
				first["Session output ─"].logical, first["Controls ─"].logical)
		}
	}
}

// TestCarryUnexposedOverridesSavedSeed confirms the focus-pair overrides (unexposed
// editor roles like button_focus_fg) survive a Save when the carry source is a
// SAVED theme's own overrides — the per-theme carry that stops the active theme's
// unexposed keys bleeding onto a saved one (criterion 3 watchlist #2).
func TestCarryUnexposedOverridesSavedSeed(t *testing.T) {
	// A saved theme carries an exposed override (user) and an unexposed one
	// (button_focus_fg, which has no editor field).
	savedOverrides := map[string]string{
		"user":            "#FF0000",
		"button_focus_fg": "#123456",
	}
	// The editor rebuilds overrides from the exposed fields alone (user changed).
	cfg := buildThemeConfig("default", false, false, map[string]string{"user": "#00FF00"})
	got := carryUnexposedOverrides(cfg, savedOverrides)
	if got.Overrides["user"] != "#00FF00" {
		t.Errorf("exposed override should come from the field: user = %q", got.Overrides["user"])
	}
	if got.Overrides["button_focus_fg"] != "#123456" {
		t.Errorf("unexposed focus-pair override was dropped: %+v", got.Overrides)
	}
	if len(got.Overrides) != 2 {
		t.Errorf("expected exactly the exposed + carried keys, got %+v", got.Overrides)
	}
}

// ----------------------------------------------------------------------------
// Tier 2 — rendering.
// ----------------------------------------------------------------------------

// TestIssue462SwatchLeadsLabel is Goal 1 criterion 1a: the swatch is the LEFT-most
// cell of a role row (left of its label), no longer detached at the far right.
func TestIssue462SwatchLeadsLabel(t *testing.T) {
	w := openThemeEditor462Raw(t, Handlers{
		GetTheme: func() config.ThemeConfig { return config.ThemeConfig{} },
		SetTheme: func(config.ThemeConfig) {},
	})
	grid := editorGrid(w)
	swRow, swCol, ok := findRunes(grid, "▉")
	if !ok {
		t.Fatal("no colour swatch (▉) rendered")
	}
	lblRow, lblCol, ok := findRunes(grid, "User messages:")
	if !ok {
		t.Fatal("User messages label not rendered")
	}
	if swRow != lblRow {
		t.Fatalf("swatch row %d != label row %d — they are not on the same role line", swRow, lblRow)
	}
	if swCol >= lblCol {
		t.Errorf("swatch at col %d is not LEFT of the label at col %d — colour no longer leads the line", swCol, lblCol)
	}
	// And the editable spec field is still the first token AFTER the label, so the
	// existing field-after-label placement property still holds post-reorder.
	if spec := fieldSpecAfterLabel(t, w, "User messages:"); spec == "" {
		t.Error("no spec field token found after the label")
	}
}

// TestIssue462SectionSeparatorBetweenGroups is Goal 1 criterion 1b: a blank
// separator row sits between adjacent sections, so groups read with breathing
// room. Asserted as a gap in logical rows between one section's last role and the
// next section's header.
func TestIssue462SectionSeparatorBetweenGroups(t *testing.T) {
	w := openThemeEditor462Raw(t, Handlers{
		GetTheme: func() config.ThemeConfig { return config.ThemeConfig{} },
		SetTheme: func(config.ThemeConfig) {},
	})
	pos := editorScrollFind(w, []string{"Errors:", "UI chrome"})
	lastRole := pos["Errors:"]  // last role of "Session output"
	nextHdr := pos["UI chrome"] // header of the next section
	if !lastRole.found || !nextHdr.found {
		t.Fatal("could not locate the Session output last role or the UI chrome header")
	}
	// Without a separator the next header would be exactly lastRole+1. With the
	// separator it is at least lastRole+2 (the blank row sits between them).
	if nextHdr.logical < lastRole.logical+2 {
		t.Errorf("no separator between sections: UI chrome header at logical %d, last Session-output role at %d (want a gap)",
			nextHdr.logical, lastRole.logical)
	}
}

// TestIssue462RolesStillReachableByScrolling confirms the added separator rows did
// not desync the scroll math: the last role of the tallest (left) column is still
// reachable by scrolling (criterion: scrolling still works).
func TestIssue462RolesStillReachableByScrolling(t *testing.T) {
	w := openThemeEditor462Raw(t, Handlers{
		GetTheme: func() config.ThemeConfig { return config.ThemeConfig{} },
		SetTheme: func(config.ThemeConfig) {},
	})
	// "Indicators / badges" is the accent role, last in the left (taller) column.
	if !scrollEditorToReveal(w, "Indicators / badges:") {
		t.Error("the left column's last role is unreachable by scrolling — separator rows desynced maxScroll")
	}
}

// TestIssue462SavedThemesListedInDropdown verifies saved themes appear in the
// preset dropdown (Goal 2): opening the popup lists the built-ins AND each saved
// theme under its "★" label.
func TestIssue462SavedThemesListedInDropdown(t *testing.T) {
	st := &theme462Store{saved: []config.NamedTheme{
		{Name: "Alpha", Theme: config.ThemeConfig{Name: "default"}},
		{Name: "Beta", Theme: config.ThemeConfig{Name: "dark"}},
	}}
	w := openThemeEditor462(t, st)

	sel := findPresetSelect(w)
	if sel == nil {
		t.Fatal("preset dropdown not found")
	}
	sel.OnTypeFn(sel, tui.TypeEvent{Key: tui.KeyEnter}) // open the popup
	w.desktop.Redraw()
	screen := screenText(w)
	for _, want := range []string{"Default", "Dark (black background)", "★ Alpha", "★ Beta"} {
		if !strings.Contains(screen, want) {
			t.Errorf("dropdown popup missing option %q", want)
		}
	}
}

// TestIssue462NoSavedHandlersHidesActions is the graceful-degradation guarantee
// (criterion 3): with GetSavedThemes/SetSavedThemes unset, the editor offers no
// Save As / Delete and behaves as the built-ins-only editor.
func TestIssue462NoSavedHandlersHidesActions(t *testing.T) {
	w := openThemeEditor462Raw(t, Handlers{
		GetTheme: func() config.ThemeConfig { return config.ThemeConfig{} },
		SetTheme: func(config.ThemeConfig) {},
		// GetSavedThemes / SetSavedThemes intentionally nil.
	})
	for _, label := range []string{"Save As…", "Delete"} {
		rect := themeEditorFooterButtonRect(w, label)
		for _, c := range dialogDescendants(w) {
			if c.Bounds == rect && c.DrawOutside && c.OnClickFn != nil {
				t.Errorf("%q button is present without GetSavedThemes/SetSavedThemes — should be hidden", label)
			}
		}
	}
	// Sanity: Reset/Save/Cancel are still present.
	for _, label := range []string{"Reset", "Save", "Cancel"} {
		rect := themeEditorFooterButtonRect(w, label)
		found := false
		for _, c := range dialogDescendants(w) {
			if c.Bounds == rect && c.DrawOutside && c.OnClickFn != nil {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("%q button missing (core buttons should remain)", label)
		}
	}
}

// ----------------------------------------------------------------------------
// Tier 3 — full UI flows.
// ----------------------------------------------------------------------------

// TestIssue462SaveAsCreatesNamedCopy is the core Goal 2 path: Save As with a fresh
// name creates an independent named copy, makes it the active theme with the
// SavedName back-link, and re-selects it in the dropdown. The source built-in is
// untouched.
func TestIssue462SaveAsCreatesNamedCopy(t *testing.T) {
	st := &theme462Store{active: config.ThemeConfig{Name: "default"}}
	w := openThemeEditor462(t, st)

	saveAsViaUI(t, w, "My Copy", false)

	if len(st.setSaved) != 1 {
		t.Fatalf("expected one SetSavedThemes call, got %d", len(st.setSaved))
	}
	flushed := st.setSaved[0]
	if len(flushed) != 1 || flushed[0].Name != "My Copy" {
		t.Fatalf("Save As did not create a single named copy: %+v", flushed)
	}
	// The stored config carries no SavedName (only the active theme does).
	if flushed[0].Theme.SavedName != "" {
		t.Errorf("saved entry stored a SavedName %q — entries must not self-reference", flushed[0].Theme.SavedName)
	}
	// The active theme now points at the copy via the back-link.
	if len(st.setTheme) != 1 || st.setTheme[0].SavedName != "My Copy" {
		t.Errorf("active theme not set with SavedName back-link: %+v", st.setTheme)
	}
	// The dropdown re-selected the new ★ entry.
	if !presetShows(w, "★ My Copy") {
		t.Error("preset did not re-select the new ★ entry after Save As")
	}
}

// TestIssue462SaveAsDuplicateConfirmsBeforeOverwrite guards criterion 2: a
// duplicate name does NOT silently overwrite. Declining leaves the original
// untouched; accepting overwrites it (keeping the original casing).
func TestIssue462SaveAsDuplicateConfirmsBeforeOverwrite(t *testing.T) {
	st := &theme462Store{
		active: config.ThemeConfig{Name: "default"},
		saved:  []config.NamedTheme{{Name: "Original", Theme: config.ThemeConfig{Name: "default", Overrides: map[string]string{"user": "#FF0000"}}}},
	}

	t.Run("decline keeps original", func(t *testing.T) {
		w := openThemeEditor462(t, st)
		saveAsViaUI(t, w, "original", false) // case-insensitive dup → confirm → No
		// Declining must not flush anything: the saved list is unchanged and no
		// SetSavedThemes call was made (guards against a silent overwrite).
		if len(st.setSaved) != 0 {
			t.Errorf("declined overwrite still flushed SetSavedThemes: %+v", st.setSaved)
		}
		if len(st.saved) != 1 || st.saved[0].Name != "Original" {
			t.Errorf("declined overwrite mutated the saved list: %+v", st.saved)
		}
		if st.saved[0].Theme.Overrides["user"] != "#FF0000" {
			t.Errorf("declined overwrite changed the original's overrides: %+v", st.saved[0].Theme.Overrides)
		}
	})

	t.Run("accept overwrites in place keeping original casing", func(t *testing.T) {
		w := openThemeEditor462(t, st)
		saveAsViaUI(t, w, "ORIGINAL", true) // dup → confirm → Yes
		// Accepting MUST flush an overwrite (otherwise the confirm was not driven).
		if len(st.setSaved) != 1 {
			t.Fatalf("accept did not flush an overwrite (confirm not driven?): setSaved=%+v", st.setSaved)
		}
		flushed := st.setSaved[0]
		if len(flushed) != 1 {
			t.Fatalf("overwrite should replace in place, got %d entries: %+v", len(flushed), flushed)
		}
		if flushed[0].Name != "Original" {
			t.Errorf("overwrite changed the stored casing: got %q, want Original", flushed[0].Name)
		}
	})
}

// TestIssue462SaveOnSavedEntryUpdatesEntryOnly verifies Save routing (Goal 2):
// with a saved theme selected, Save writes back to that saved entry (and the
// active theme), never to the built-in palettes.
func TestIssue462SaveOnSavedEntryUpdatesEntryOnly(t *testing.T) {
	st := &theme462Store{
		active: config.ThemeConfig{Name: "default", SavedName: "Mine", Overrides: map[string]string{"user": "#FF0000"}},
		saved:  []config.NamedTheme{{Name: "Mine", Theme: config.ThemeConfig{Name: "default", Overrides: map[string]string{"user": "#FF0000"}}}},
	}
	w := openThemeEditor462(t, st)
	// selectActive(cur) on open selected the ★Mine entry.
	if !presetShows(w, "★ Mine") {
		t.Fatal("precondition: editor did not open with the saved ★Mine entry selected")
	}

	clickThemeButton(t, w, "Save")

	// The last SetSavedThemes flushed the saved list with the Mine entry updated.
	if len(st.setSaved) == 0 {
		t.Fatal("Save on a saved entry did not flush SetSavedThemes")
	}
	last := st.setSaved[len(st.setSaved)-1]
	if len(last) != 1 || last[0].Name != "Mine" {
		t.Errorf("Save did not write back to the Mine entry: %+v", last)
	}
	// Built-ins are never mutated: the saved list only ever contains user themes,
	// and every flushed entry's parent is a real built-in name.
	for _, nt := range last {
		switch nt.Theme.Name {
		case "default", "high-contrast", "dark":
		default:
			t.Errorf("saved entry has non-built-in parent %q — built-in palette was mutated", nt.Theme.Name)
		}
	}
	// The active theme still carries the SavedName back-link after the save.
	if len(st.setTheme) == 0 || st.setTheme[len(st.setTheme)-1].SavedName != "Mine" {
		t.Errorf("Save on a saved entry did not keep the SavedName back-link: %+v", st.setTheme)
	}
}

// TestIssue462EditAndSaveSavedEntryUpdatesContent drives an actual field edit then
// Save, asserting the saved entry's stored override reflects the edit (criterion:
// editing + saving while a saved theme is selected updates that saved entry).
func TestIssue462EditAndSaveSavedEntryUpdatesContent(t *testing.T) {
	st := &theme462Store{
		active: config.ThemeConfig{Name: "default", SavedName: "Mine", Overrides: map[string]string{"user": "#FF0000"}},
		saved:  []config.NamedTheme{{Name: "Mine", Theme: config.ThemeConfig{Name: "default", Overrides: map[string]string{"user": "#FF0000"}}}},
	}
	w := openThemeEditor462(t, st)

	// Edit the User messages field from #FF0000 to #00FF00.
	setRoleField(t, w, "User messages:", "#00FF00")
	if got := fieldSpecAfterLabel(t, w, "User messages:"); got != "#00FF00" {
		t.Fatalf("field edit did not take: got %q", got)
	}
	clickThemeButton(t, w, "Save")

	last := st.setSaved[len(st.setSaved)-1]
	if last[0].Theme.Overrides["user"] != "#00FF00" {
		t.Errorf("Save did not persist the edited override: user = %q, want #00FF00 (entry = %+v)",
			last[0].Theme.Overrides["user"], last[0].Theme.Overrides)
	}
}

// TestIssue462CarryUnexposedAcrossSavedSave is the UI counterpart of the pure
// carry test: an unexposed override (button_focus_fg) on a saved theme survives an
// edit+Save of an EXPOSED field — carryUnexposedOverrides must source from the
// saved entry's overrides, not drop them.
func TestIssue462CarryUnexposedAcrossSavedSave(t *testing.T) {
	st := &theme462Store{
		active: config.ThemeConfig{
			Name:      "default",
			SavedName: "Mine",
			Overrides: map[string]string{"user": "#FF0000", "button_focus_fg": "#123456"},
		},
		saved: []config.NamedTheme{{
			Name:  "Mine",
			Theme: config.ThemeConfig{Name: "default", Overrides: map[string]string{"user": "#FF0000", "button_focus_fg": "#123456"}},
		}},
	}
	w := openThemeEditor462(t, st)

	setRoleField(t, w, "User messages:", "#00AA00") // edit an exposed role
	clickThemeButton(t, w, "Save")

	last := st.setSaved[len(st.setSaved)-1]
	ov := last[0].Theme.Overrides
	if ov["user"] != "#00AA00" {
		t.Errorf("exposed edit lost: user = %q, want #00AA00", ov["user"])
	}
	if ov["button_focus_fg"] != "#123456" {
		t.Errorf("unexposed override dropped across the saved Save: button_focus_fg = %q, want #123456 (overrides = %+v)",
			ov["button_focus_fg"], ov)
	}
}

// TestIssue462DeleteNonActiveKeepsActive is the round-3 regression: deleting a
// saved theme that is merely SELECTED (not the live active theme) must not reset
// the active theme. Detection keys on the live SavedName, never the dropdown
// highlight.
func TestIssue462DeleteNonActiveKeepsActive(t *testing.T) {
	st := &theme462Store{
		active: config.ThemeConfig{Name: "default", SavedName: "A", Overrides: map[string]string{"user": "#FF0000"}},
		saved: []config.NamedTheme{
			{Name: "A", Theme: config.ThemeConfig{Name: "default", Overrides: map[string]string{"user": "#FF0000"}}},
			{Name: "B", Theme: config.ThemeConfig{Name: "default", Overrides: map[string]string{"agent": "#00FF00"}}},
		},
	}
	w := openThemeEditor462(t, st)
	if !presetShows(w, "★ A") {
		t.Fatal("precondition: editor did not open with active ★A selected")
	}
	// Browse the dropdown to ★B (selection only — active is still ★A).
	pickPresetOption(t, w, "★ B")
	if !presetShows(w, "★ B") {
		t.Fatal("precondition: did not browse to ★B in the dropdown")
	}

	deleteViaUI(t, w, true) // confirm the delete of ★B

	// ★B is gone; ★A (the active theme) is untouched — no reset to a built-in.
	flushed := st.setSaved[len(st.setSaved)-1]
	if len(flushed) != 1 || flushed[0].Name != "A" {
		t.Errorf("delete of non-active ★B removed the wrong entry or left ★B: %+v", flushed)
	}
	// The active theme must NOT have been reset (no SetTheme to a pristine parent).
	for _, c := range st.setTheme {
		if c.SavedName == "" && c.Name != "" && len(c.Overrides) == 0 {
			t.Errorf("delete of non-active ★B reset the active theme to a pristine built-in: %+v", c)
		}
	}
	// And the editor reconciled back to the still-active ★A.
	if !presetShows(w, "★ A") {
		t.Error("after deleting ★B the editor did not re-select the active ★A")
	}
}

// TestIssue462DeleteActiveResetsToParent covers the other Delete branch: deleting
// the LIVE active saved theme re-points the active theme to the pristine parent
// built-in (SavedName cleared, no overrides) and re-selects that built-in.
func TestIssue462DeleteActiveResetsToParent(t *testing.T) {
	st := &theme462Store{
		active: config.ThemeConfig{Name: "default", SavedName: "A", Overrides: map[string]string{"user": "#FF0000"}},
		saved:  []config.NamedTheme{{Name: "A", Theme: config.ThemeConfig{Name: "default", Overrides: map[string]string{"user": "#FF0000"}}}},
	}
	w := openThemeEditor462(t, st)

	deleteViaUI(t, w, true)

	// The saved list is now empty.
	if len(st.saved) != 0 {
		t.Errorf("active saved theme was not removed: %+v", st.saved)
	}
	// The active theme was reset to the pristine parent (no SavedName, no overrides).
	last := st.setTheme[len(st.setTheme)-1]
	if last.SavedName != "" {
		t.Errorf("active theme still has SavedName %q after deleting it — dangling back-link", last.SavedName)
	}
	if len(last.Overrides) != 0 {
		t.Errorf("active theme was not reset to the pristine parent (kept overrides): %+v", last.Overrides)
	}
	// The editor re-selected the parent built-in (no ★ shown).
	if presetShows(w, "★") {
		t.Error("after deleting the active saved theme a ★ entry is still selected")
	}
}

// TestIssue462DeleteBuiltInIsNoOp confirms built-ins are non-deletable: Delete
// with a built-in selected does not open a confirm and does not flush anything.
func TestIssue462DeleteBuiltInIsNoOp(t *testing.T) {
	st := &theme462Store{
		active: config.ThemeConfig{Name: "default"},
		saved:  []config.NamedTheme{{Name: "A", Theme: config.ThemeConfig{Name: "default"}}},
	}
	w := openThemeEditor462(t, st)
	// Pick a built-in (High-contrast) so a built-in is selected.
	pickPresetOption(t, w, "High-contrast (Okabe–Ito)")

	deleteViaUI(t, w, true)

	// No confirm opened, no SetSavedThemes flushed, saved list intact.
	if top := w.desktop.TopLayer(); top != nil && top.Name == "confirm" {
		t.Error("Delete on a built-in opened a confirm — built-ins must be non-deletable")
	}
	if len(st.setSaved) != 0 {
		t.Errorf("Delete on a built-in flushed SetSavedThemes: %+v", st.setSaved)
	}
	if len(st.saved) != 1 || st.saved[0].Name != "A" {
		t.Errorf("Delete on a built-in mutated the saved list: %+v", st.saved)
	}
}

// TestIssue462BackLinkReselectsSavedOnOpen is the cross-reopen fix (criterion 2):
// when the active theme carries a SavedName, opening the editor re-selects the
// matching ★ entry (so a later Save writes back to it, not the parent built-in).
func TestIssue462BackLinkReselectsSavedOnOpen(t *testing.T) {
	st := &theme462Store{
		active: config.ThemeConfig{Name: "default", SavedName: "Mine", Overrides: map[string]string{"user": "#FF0000"}},
		saved:  []config.NamedTheme{{Name: "Mine", Theme: config.ThemeConfig{Name: "default", Overrides: map[string]string{"user": "#FF0000"}}}},
	}
	w := openThemeEditor462(t, st)
	if !presetShows(w, "★ Mine") {
		t.Fatal("opening with SavedName set did not re-select the ★ entry (the back-link / selectActive on open is broken)")
	}
	// And the fields are seeded from the saved theme's colours, not the pristine parent.
	if got := fieldSpecAfterLabel(t, w, "User messages:"); got != "#FF0000" {
		t.Errorf("fields not seeded from the saved theme: user = %q, want #FF0000", got)
	}
}

// TestIssue462DanglingSavedNameFallsBackToBuiltIn: if SavedName points at a saved
// theme that no longer exists (deleted/renamed out-of-band), opening the editor
// degrades gracefully to the parent built-in rather than stranding.
func TestIssue462DanglingSavedNameFallsBackToBuiltIn(t *testing.T) {
	st := &theme462Store{
		active: config.ThemeConfig{Name: "default", SavedName: "Ghost"}, // no saved theme named "Ghost"
		saved:  []config.NamedTheme{{Name: "Real", Theme: config.ThemeConfig{Name: "default"}}},
	}
	w := openThemeEditor462(t, st)
	// No ★Ghost exists, so the editor must fall back to the Default built-in.
	if presetShows(w, "★") {
		t.Error("a dangling SavedName selected a ★ entry instead of falling back to a built-in")
	}
	if !presetShows(w, "Default") {
		t.Error("a dangling SavedName did not fall back to the Default built-in")
	}
}

// TestIssue462CancelDoesNotPersist: Cancel closes the editor without flushing the
// active theme or the saved list (error-handling / no-side-effect on discard).
func TestIssue462CancelDoesNotPersist(t *testing.T) {
	st := &theme462Store{active: config.ThemeConfig{Name: "default"}}
	w := openThemeEditor462(t, st)
	// Make some unsaved edits first.
	setRoleField(t, w, "User messages:", "#00FF00")

	clickThemeButton(t, w, "Cancel")

	if len(st.setTheme) != 0 {
		t.Errorf("Cancel persisted the active theme: %+v", st.setTheme)
	}
	if len(st.setSaved) != 0 {
		t.Errorf("Cancel persisted the saved list: %+v", st.setSaved)
	}
	if top := w.desktop.TopLayer(); top != nil && (top.Name == "theme-editor") {
		t.Error("Cancel did not close the editor")
	}
}
