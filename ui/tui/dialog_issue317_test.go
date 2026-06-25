package ui

import (
	"testing"

	"gogent/internal/config"
	"gogent/internal/gogent"

	tv "github.com/hobbestherat/turbotui/turbotv"
)

// Issue #317: set a content-driven preferred size for every dialog so a dialog is
// only as big as it needs to be (the floor-only "balloon" dialogs resolved to the
// full 80%×85% box). This suite drives the REAL show* functions (so it catches drift
// between a dialog's documented spec and what it actually hands the resolver) and
// unit-tests the theme editor's new live-bounds geometry.
//
// Behaviour under test, per the issue:
//   - Sub-agent Settings & Notifications: PINNED to their fixed-form content footprint
//     (never 160×42 on a roomy terminal, never ballooning on an ultrawide one).
//   - Theme editor: FLOORED at 80×22 (was pinned), growing toward the cap on a larger
//     terminal with its renderer geometry derived from the resolved live bounds.
//   - Review (diff) & Monologue (transcript): floor-only viewers, now width-capped at
//     120 so they grow tall but not ultrawide.

const issue317DefW, issue317DefH = 160, 42 // the 80%×85% percentage default ("the balloon")

// ----------------------------------------------------------------------------
// Handler fixtures.
// ----------------------------------------------------------------------------

// settingsHandlers wires the minimum handlers showSettingsDialog needs to build the
// real form (rather than the "unavailable" fallback).
func settingsHandlers() Handlers {
	return Handlers{
		GetSettings:    func() config.SubAgentConfig { return config.DefaultSubAgentConfig() },
		SetSettings:    func(config.SubAgentConfig) {},
		GetTimeouts:    func() config.TimeoutConfig { return config.DefaultTimeoutConfig() },
		SetTimeouts:    func(config.TimeoutConfig) {},
		GetReviewEdits: func() bool { return false },
		SetReviewEdits: func(bool) {},
	}
}

func notificationsHandlers() Handlers {
	return Handlers{
		GetNotifyConfig: func() config.NotifyConfig { return config.NotifyConfig{} },
		SetNotifyConfig: func(config.NotifyConfig) {},
	}
}

// ----------------------------------------------------------------------------
// 1 & 2. Sub-agent Settings and Notifications — pinned to content, never the balloon.
// ----------------------------------------------------------------------------

// TestSettingsDialogPinnedToContent opens the real Sub-agent Settings dialog and
// asserts it resolves to its fixed-form footprint (spec MinW64/MaxW76/PreferredW72,
// MinH20/MaxH20) rather than the 160×42 box: 72×20 on a roomy terminal, 64×20 floored
// on an 80-column one, and capped — never wider than 72 — even on an ultrawide one.
func TestSettingsDialogPinnedToContent(t *testing.T) {
	t.Run("roomy terminal sizes to content, not the 160x42 balloon", func(t *testing.T) {
		w := newTestWorkbench(t)
		w.SetHandlers(settingsHandlers())
		w.app.Resize(200, 50)
		w.showSettingsDialog()
		b := dialogBounds(w)
		if b.W == issue317DefW && b.H == issue317DefH {
			t.Fatalf("settings is still the regressed %dx%d balloon", issue317DefW, issue317DefH)
		}
		if b.W != 72 || b.H != 20 {
			t.Errorf("settings size = %dx%d, want pinned 72x20", b.W, b.H)
		}
		if b.X != (200-b.W)/2 || b.Y != (50-b.H)/2 {
			t.Errorf("settings origin = (%d,%d), want centered", b.X, b.Y)
		}
	})

	t.Run("never balloons even on an ultrawide terminal", func(t *testing.T) {
		w := newTestWorkbench(t)
		w.SetHandlers(settingsHandlers())
		w.app.Resize(300, 80)
		w.showSettingsDialog()
		b := dialogBounds(w)
		if b.W != 72 || b.H != 20 {
			t.Errorf("settings on 300x80 = %dx%d, want capped 72x20 (MaxW/MaxH must hold)", b.W, b.H)
		}
	})

	t.Run("floors on a small 80-column terminal", func(t *testing.T) {
		w := newTestWorkbench(t)
		w.SetHandlers(settingsHandlers())
		w.app.Resize(80, 24)
		w.showSettingsDialog()
		b := dialogBounds(w)
		if b.W != 64 || b.H != 20 {
			t.Errorf("settings on 80x24 = %dx%d, want 64x20 (MinW floor, MaxH cap)", b.W, b.H)
		}
	})
}

// TestNotificationsDialogPinnedToContent is the Notifications counterpart: the real
// dialog resolves to 54×18 on a roomy terminal (spec PreferredW54/MaxW58, MinH18/MaxH18),
// never the 160×42 box, and stays 54×18 on an ultrawide one.
func TestNotificationsDialogPinnedToContent(t *testing.T) {
	t.Run("roomy terminal sizes to content, not the 160x42 balloon", func(t *testing.T) {
		w := newTestWorkbench(t)
		w.SetHandlers(notificationsHandlers())
		w.app.Resize(200, 50)
		w.showNotificationsDialog()
		b := dialogBounds(w)
		if b.W == issue317DefW && b.H == issue317DefH {
			t.Fatalf("notifications is still the regressed %dx%d balloon", issue317DefW, issue317DefH)
		}
		if b.W != 54 || b.H != 18 {
			t.Errorf("notifications size = %dx%d, want pinned 54x18", b.W, b.H)
		}
		if b.X != (200-b.W)/2 || b.Y != (50-b.H)/2 {
			t.Errorf("notifications origin = (%d,%d), want centered", b.X, b.Y)
		}
	})

	t.Run("never balloons even on an ultrawide terminal", func(t *testing.T) {
		w := newTestWorkbench(t)
		w.SetHandlers(notificationsHandlers())
		w.app.Resize(300, 80)
		w.showNotificationsDialog()
		b := dialogBounds(w)
		if b.W != 54 || b.H != 18 {
			t.Errorf("notifications on 300x80 = %dx%d, want capped 54x18", b.W, b.H)
		}
	})

	t.Run("floors on a tiny terminal", func(t *testing.T) {
		w := newTestWorkbench(t)
		w.SetHandlers(notificationsHandlers())
		w.app.Resize(40, 12)
		w.showNotificationsDialog()
		b := dialogBounds(w)
		if b.W < 50 || b.H < 18 {
			t.Errorf("notifications on 40x12 = %dx%d, want at least the 50x18 floor", b.W, b.H)
		}
	})
}

// TestSettingsLongestLabelFitsOnRoomyTerminal is the content-fit rationale behind the
// 72-wide preferred size: the longest row (the "&Both: …" checkbox, 60 display cells,
// drawn at x=4 in a width-8 cell) must fit without clipping when the dialog is at its
// preferred 72 columns. A narrower PreferredW would clip the label on every terminal.
func TestSettingsLongestLabelFitsOnRoomyTerminal(t *testing.T) {
	w := newTestWorkbench(t)
	w.SetHandlers(settingsHandlers())
	w.app.Resize(200, 50)
	w.showSettingsDialog()
	b := dialogBounds(w)
	// The checkbox label cell is width-8 wide (x=4 origin, 4-col border/checkbox chrome
	// on the right). The "Both" label is 60 cells plus the "[ ] " checkbox glyph (4).
	const longestLabelCells = 60 + 4
	if avail := b.W - 8; avail < longestLabelCells {
		t.Errorf("settings width %d leaves %d cells for the longest checkbox row, need %d — the label clips",
			b.W, avail, longestLabelCells)
	}
}

// ----------------------------------------------------------------------------
// 4. Review (diff) and Monologue (transcript) — width-capped viewers.
// ----------------------------------------------------------------------------

// TestReviewDialogWidthCapped drives the real showReviewDialog and asserts the issue
// #317 cap: it grows tall (fills 85% of the height) but its width is capped at 120 so a
// diff stays readable on an ultrawide terminal instead of spanning 160+ columns.
func TestReviewDialogWidthCapped(t *testing.T) {
	open := func(termW, termH int) tv.Rect {
		w := newTestWorkbench(t)
		w.app.Resize(termW, termH)
		showReviewDialog(w.desktop, gogent.EditReviewRequest{Op: "edit", Path: "a.go", Diff: "@@\n+x\n-y"}, "", func(gogent.EditReviewDecision) {})
		return dialogBounds(w)
	}

	t.Run("capped at 120 wide, full height on a roomy terminal", func(t *testing.T) {
		b := open(200, 50)
		if b.W != 120 {
			t.Errorf("review width = %d on 200x50, want capped 120", b.W)
		}
		if b.H != 42 { // 85% of 50 — no height cap, a diff wants vertical space
			t.Errorf("review height = %d on 200x50, want 42 (grows tall, uncapped)", b.H)
		}
	})

	t.Run("stays 120 wide but grows taller on an ultrawide terminal", func(t *testing.T) {
		b := open(300, 80)
		if b.W != 120 {
			t.Errorf("review width = %d on 300x80, want still 120 (width cap holds)", b.W)
		}
		if b.H != 68 { // 85% of 80 — height keeps growing
			t.Errorf("review height = %d on 300x80, want 68 (height uncapped)", b.H)
		}
	})

	t.Run("floors on a tiny terminal", func(t *testing.T) {
		b := open(30, 8)
		if b.W < 40 || b.H < 12 {
			t.Errorf("review on 30x8 = %dx%d, want at least the 40x12 floor", b.W, b.H)
		}
	})
}

// TestMonologDialogWidthCapped is the transcript-viewer counterpart: showAgentMonolog
// grows tall but caps its width at 120 on a wide terminal.
func TestMonologDialogWidthCapped(t *testing.T) {
	open := func(termW, termH int) tv.Rect {
		w := newTestWorkbench(t)
		w.SetHandlers(Handlers{
			GetTranscript: func(string, string) []ChatMessage { return nil },
		})
		w.app.Resize(termW, termH)
		w.showAgentMonolog("s1", "a1", "agent")
		return dialogBounds(w)
	}

	t.Run("capped at 120 wide, full height on a roomy terminal", func(t *testing.T) {
		b := open(200, 50)
		if b.W != 120 {
			t.Errorf("monologue width = %d on 200x50, want capped 120", b.W)
		}
		if b.H != 42 {
			t.Errorf("monologue height = %d on 200x50, want 42 (grows tall, uncapped)", b.H)
		}
	})

	t.Run("stays 120 wide on an ultrawide terminal", func(t *testing.T) {
		b := open(300, 80)
		if b.W != 120 {
			t.Errorf("monologue width = %d on 300x80, want still 120", b.W)
		}
	})

	t.Run("floors on a tiny terminal", func(t *testing.T) {
		b := open(30, 8)
		if b.W < 40 || b.H < 10 {
			t.Errorf("monologue on 30x8 = %dx%d, want at least the 40x10 floor", b.W, b.H)
		}
	})
}

// ----------------------------------------------------------------------------
// 3. Theme editor — live-bounds geometry (resolveThemeEditorLayout).
// ----------------------------------------------------------------------------

// checkThemeLayoutInvariants asserts the layout self-consistency checkThemeEditorLayout
// guards at the floor, but for an ARBITRARY resolved size: the viewport is non-empty and
// clears the buttons, the two columns do not collide with each other or the scrollbar,
// the right column's swatch ends exactly at the column before the scrollbar, and every
// role label fits its column cell. These must hold at every width ≥ 80 / height ≥ 22, or
// the grown editor clips a label or overruns the scrollbar.
func checkThemeLayoutInvariants(t *testing.T, l themeEditorLayout) {
	t.Helper()
	buttonRow := l.height - 3
	if l.visibleRows < 1 {
		t.Errorf("visibleRows = %d at %dx%d — nothing would show", l.visibleRows, l.width, l.height)
	}
	if last := l.contentTop + l.visibleRows - 1; last >= buttonRow {
		t.Errorf("viewport last row %d collides with the buttons at row %d (%dx%d)", last, buttonRow, l.width, l.height)
	}
	if l.scrollbarX >= l.width-2 {
		t.Errorf("scrollbar column %d overlaps the right border at width %d", l.scrollbarX, l.width)
	}
	cols := l.columns
	placed := 0
	for ci, col := range cols {
		swatchEnd := col.x + col.labelW + themeEditorFieldW + themeEditorSwatchW + 2 - 1
		limit := l.scrollbarX - 1
		if ci+1 < len(cols) {
			limit = cols[ci+1].x - 1
		}
		if swatchEnd > limit {
			t.Errorf("at %dx%d column %d swatch ends at %d, past its limit %d", l.width, l.height, ci, swatchEnd, limit)
		}
		for _, g := range col.groups {
			for _, role := range g.roles {
				if n := len([]rune(role.label)) + 1; n > col.labelW {
					t.Errorf("at %dx%d label %q (%d cols) overflows column %d cell (%d wide)", l.width, l.height, role.label, n, ci, col.labelW)
				}
				placed++
			}
		}
	}
	if placed != len(themeRoles) {
		t.Errorf("at %dx%d columns place %d roles but themeRoles has %d", l.width, l.height, placed, len(themeRoles))
	}
}

// TestResolveThemeEditorLayoutFloor pins the floor case: at the 80×22 minimum the
// resolved layout must reproduce the documented compile-time geometry exactly (the
// original {x:2,labelW:20} / {x:39,labelW:22} columns, contentTop 3, visibleRows 16,
// scrollbar at 77), so the floor render is byte-for-byte what the pre-#317 constants
// drew. themeEditorColumns() is defined as this floor case, so they must agree.
func TestResolveThemeEditorLayoutFloor(t *testing.T) {
	l := resolveThemeEditorLayout(themeEditorDialogW, themeEditorDialogH)
	if l.contentTop != themeEditorContentTop {
		t.Errorf("floor contentTop = %d, want %d", l.contentTop, themeEditorContentTop)
	}
	if l.visibleRows != themeEditorVisibleRows {
		t.Errorf("floor visibleRows = %d, want %d (the documented floor const)", l.visibleRows, themeEditorVisibleRows)
	}
	if l.scrollbarX != themeEditorScrollbarX {
		t.Errorf("floor scrollbarX = %d, want %d (the documented floor const)", l.scrollbarX, themeEditorScrollbarX)
	}
	if len(l.columns) != 2 {
		t.Fatalf("floor columns = %d, want 2", len(l.columns))
	}
	if l.columns[0].x != 2 || l.columns[0].labelW != 20 {
		t.Errorf("floor left column = {x:%d,labelW:%d}, want {2,20}", l.columns[0].x, l.columns[0].labelW)
	}
	if l.columns[1].x != 39 || l.columns[1].labelW != 22 {
		t.Errorf("floor right column = {x:%d,labelW:%d}, want {39,22}", l.columns[1].x, l.columns[1].labelW)
	}
	// themeEditorColumns() is documented as the floor case; its x/labelW origins must
	// match (themeEditorColumn embeds a slice, so compare the comparable fields).
	got := themeEditorColumns()
	if len(got) != 2 {
		t.Fatalf("themeEditorColumns() len = %d, want 2", len(got))
	}
	for i := range got {
		if got[i].x != l.columns[i].x || got[i].labelW != l.columns[i].labelW {
			t.Errorf("themeEditorColumns()[%d] = {x:%d,labelW:%d}, want {x:%d,labelW:%d}",
				i, got[i].x, got[i].labelW, l.columns[i].x, l.columns[i].labelW)
		}
	}
	checkThemeLayoutInvariants(t, l)
}

// TestResolveThemeEditorLayoutGrows sweeps a range of resolved sizes the dialog can take
// (from the floor up to a large terminal) and asserts the geometry stays self-consistent
// at every one: the scrollbar tracks width-3, the viewport tracks height-3-contentTop,
// the right column's swatch ends exactly one column before the scrollbar, and no label
// clips. This is the core correctness property of deriving geometry from live bounds.
func TestResolveThemeEditorLayoutGrows(t *testing.T) {
	for _, dim := range []struct{ W, H int }{
		{80, 22}, {81, 23}, {96, 34}, {120, 40}, {160, 42}, {200, 50}, {240, 68},
	} {
		t.Run("", func(t *testing.T) {
			l := resolveThemeEditorLayout(dim.W, dim.H)
			if l.width != dim.W || l.height != dim.H {
				t.Errorf("layout bounds = %dx%d, want %dx%d", l.width, l.height, dim.W, dim.H)
			}
			if l.scrollbarX != dim.W-3 {
				t.Errorf("at %dx%d scrollbarX = %d, want width-3 = %d", dim.W, dim.H, l.scrollbarX, dim.W-3)
			}
			if l.visibleRows != dim.H-3-themeEditorContentTop {
				t.Errorf("at %dx%d visibleRows = %d, want height-3-contentTop = %d", dim.W, dim.H, l.visibleRows, dim.H-3-themeEditorContentTop)
			}
			// The right column's swatch must end exactly at the column before the scrollbar,
			// so the grown dialog uses the full width with the scrollbar flush right.
			right := l.columns[len(l.columns)-1]
			swatchEnd := right.x + right.labelW + themeEditorFieldW + themeEditorSwatchW + 2 - 1
			if swatchEnd != l.scrollbarX-1 {
				t.Errorf("at %dx%d right swatch ends at %d, want one column before the scrollbar (%d)", dim.W, dim.H, swatchEnd, l.scrollbarX-1)
			}
			checkThemeLayoutInvariants(t, l)
		})
	}
}

// TestResolveThemeEditorLayoutMonotonic checks the layout grows monotonically with the
// resolved bounds: a wider dialog never shrinks a label cell or moves the scrollbar left,
// and a taller dialog never shrinks the viewport. A non-monotonic step would mean some
// width reflows content backwards.
func TestResolveThemeEditorLayoutMonotonic(t *testing.T) {
	prev := resolveThemeEditorLayout(80, 22)
	for w := 81; w <= 240; w++ {
		cur := resolveThemeEditorLayout(w, 22)
		if cur.scrollbarX < prev.scrollbarX {
			t.Fatalf("scrollbarX shrank from %d (w=%d) to %d (w=%d)", prev.scrollbarX, w-1, cur.scrollbarX, w)
		}
		if cur.columns[0].labelW < prev.columns[0].labelW || cur.columns[1].labelW < prev.columns[1].labelW {
			t.Fatalf("a label cell shrank when width grew to %d: %+v -> %+v", w, prev.columns, cur.columns)
		}
		prev = cur
	}
	ph := resolveThemeEditorLayout(80, 22)
	for h := 23; h <= 68; h++ {
		cur := resolveThemeEditorLayout(80, h)
		if cur.visibleRows < ph.visibleRows {
			t.Fatalf("visibleRows shrank from %d (h=%d) to %d (h=%d)", ph.visibleRows, h-1, cur.visibleRows, h)
		}
		ph = cur
	}
}

// TestThemeEditorContentRowsGeometryIndependent confirms the scrollable content height
// depends only on the group split, not the resolved width: it is the same at the floor
// and at a wide dialog (the columns spread horizontally, they do not restack), so the
// scroll math keys off a stable content height.
func TestThemeEditorContentRowsGeometryIndependent(t *testing.T) {
	rows := themeEditorContentRows()
	// Left column: Session output (7) + UI chrome (10, +list_bg #327) = 17 roles + 2 headers
	// = 19, plus 1 section separator (issue #462) = 20.
	// Right column: Controls (8) + Buttons and inputs (6) + Code (1) = 15 + 3 headers = 18,
	// plus 2 section separators (issue #462) = 20. Tallest = 20.
	if rows != 20 {
		t.Errorf("themeEditorContentRows() = %d, want 20 (tallest column, incl. #462 separators)", rows)
	}
}

// ----------------------------------------------------------------------------
// Theme editor — scroll behaviour preserved (floor scrolls, grown fits).
// ----------------------------------------------------------------------------

// TestThemeEditorScrollsAtFloorFitsWhenGrown is the headline scroll property: at the
// 80×22 floor the content overflows the 16-row viewport (the #279/#291 roles), so it
// scrolls; on a tall grown dialog the viewport gains rows until the content fits and
// scrolling is disabled. maxScroll is the single value the renderer keys all of that on.
func TestThemeEditorScrollsAtFloorFitsWhenGrown(t *testing.T) {
	floor := resolveThemeEditorLayout(themeEditorDialogW, themeEditorDialogH)
	if floor.maxScroll() == 0 {
		t.Errorf("at the 80x22 floor maxScroll = 0 — the overflowing roles would be unreachable")
	}
	if got, want := floor.maxScroll(), themeEditorContentRows()-floor.visibleRows; got != want {
		t.Errorf("floor maxScroll = %d, want contentRows-visibleRows = %d", got, want)
	}
	// The package-level helpers report the floor value (the renderer uses the live layout).
	if themeEditorMaxScroll() != floor.maxScroll() {
		t.Errorf("themeEditorMaxScroll() = %d, want the floor layout's %d", themeEditorMaxScroll(), floor.maxScroll())
	}

	grown := resolveThemeEditorLayout(160, 42)
	if grown.maxScroll() != 0 {
		t.Errorf("on a grown 160x42 dialog maxScroll = %d, want 0 (the content fits, no scrolling)", grown.maxScroll())
	}
	if grown.visibleRows < themeEditorContentRows() {
		t.Errorf("grown visibleRows %d < contentRows %d — it should fit without scrolling", grown.visibleRows, themeEditorContentRows())
	}
}

// TestThemeEditorClampScroll covers the scroll-offset clamp at both the floor and a
// grown layout: a negative offset clamps to 0, an over-large one clamps to maxScroll,
// and an in-range one is returned unchanged. On a grown layout (maxScroll 0) every
// offset clamps to 0.
func TestThemeEditorClampScroll(t *testing.T) {
	floor := resolveThemeEditorLayout(themeEditorDialogW, themeEditorDialogH)
	max := floor.maxScroll()
	if got := floor.clampScroll(-5); got != 0 {
		t.Errorf("clampScroll(-5) = %d, want 0", got)
	}
	if got := floor.clampScroll(max + 100); got != max {
		t.Errorf("clampScroll(over) = %d, want %d", got, max)
	}
	if max > 0 {
		if got := floor.clampScroll(1); got != 1 {
			t.Errorf("clampScroll(1) = %d, want 1 (in range)", got)
		}
	}
	// clampThemeScroll is the package helper keyed off the floor; it must agree.
	if got := clampThemeScroll(max + 100); got != max {
		t.Errorf("clampThemeScroll(over) = %d, want %d", got, max)
	}
	if got := clampThemeScroll(-1); got != 0 {
		t.Errorf("clampThemeScroll(-1) = %d, want 0", got)
	}

	grown := resolveThemeEditorLayout(160, 42)
	for _, y := range []int{-3, 0, 1, 5, 100} {
		if got := grown.clampScroll(y); got != 0 {
			t.Errorf("grown clampScroll(%d) = %d, want 0 (nothing to scroll)", y, got)
		}
	}
}

// ----------------------------------------------------------------------------
// Theme editor — rendered behaviour (real editor over a default config).
// ----------------------------------------------------------------------------

// themeEditorFrameRows returns the top and bottom screen rows of the theme editor's
// dialog frame (the ╔ … ╚ borders) in the current render.
func themeEditorFrameRows(rows [][]rune) (top, bottom int) {
	top, bottom = -1, -1
	for y, r := range rows {
		if len(r) > 0 && r[0] == '╔' {
			top = y
		}
		if len(r) > 0 && r[0] == '╚' {
			bottom = y
		}
	}
	return top, bottom
}

// TestThemeEditorOpensFlooredAndGrows opens the REAL editor at several terminal sizes
// and asserts its outer bounds match the floored-and-grows policy end-to-end (catching
// any drift in showThemeEditor's spec): floors at 80×22 on an 80×24 terminal and grows
// toward the cap on larger ones. This is the open-the-dialog companion to the
// resolver-level TestThemeEditorFlooredAndGrows.
func TestThemeEditorOpensFlooredAndGrows(t *testing.T) {
	for _, tc := range []struct {
		termW, termH int
		wantW, wantH int
	}{
		{80, 24, 80, 22},
		{200, 50, 160, 42},
		{120, 40, 96, 34},
	} {
		issue204RestoreTheme(t)
		w := newTestWorkbench(t)
		w.SetHandlers(Handlers{
			GetTheme: func() config.ThemeConfig { return config.ThemeConfig{} },
			SetTheme: func(config.ThemeConfig) {},
		})
		w.app.Resize(tc.termW, tc.termH)
		w.showThemeEditor()
		b := dialogBounds(w)
		if b.W != tc.wantW || b.H != tc.wantH {
			t.Errorf("theme editor on %dx%d = %dx%d, want %dx%d", tc.termW, tc.termH, b.W, b.H, tc.wantW, tc.wantH)
		}
	}
}

// TestThemeEditorClearsMenuBarAtFloor pins the height-ceiling invariant the issue insists
// must still hold: centred at the 80×22 floor on a 24-row terminal the dialog's top frame
// must sit on row ≥ 1, clearing the always-on-top menu bar at row 0.
func TestThemeEditorClearsMenuBarAtFloor(t *testing.T) {
	issue204RestoreTheme(t)
	w := newTestWorkbench(t)
	w.SetHandlers(Handlers{
		GetTheme: func() config.ThemeConfig { return config.ThemeConfig{} },
		SetTheme: func(config.ThemeConfig) {},
	})
	w.app.Resize(80, 24)
	w.showThemeEditor()
	top, bottom := themeEditorFrameRows(editorGrid(w))
	if top < 0 || bottom < 0 {
		t.Fatalf("could not locate the theme editor frame (top=%d bottom=%d)", top, bottom)
	}
	if top < 1 {
		t.Errorf("theme editor top frame on row %d — it overwrites the menu bar at row 0", top)
	}
	if h := bottom - top + 1; h > 24 {
		t.Errorf("theme editor frame spans %d rows — taller than a 24-row terminal at the floor", h)
	}
}

// TestThemeEditorColumnsSpreadOnWideTerminal proves the renderer actually uses the extra
// width: the right section column ("Controls") renders further right on a 200-column
// terminal than at the 80-column floor. If the renderer still keyed off the compile-time
// constants the column would sit at the same screen position regardless of terminal width.
func TestThemeEditorColumnsSpreadOnWideTerminal(t *testing.T) {
	colOf := func(termW, termH int) int {
		issue204RestoreTheme(t)
		w := newTestWorkbench(t)
		w.SetHandlers(Handlers{
			GetTheme: func() config.ThemeConfig { return config.ThemeConfig{} },
			SetTheme: func(config.ThemeConfig) {},
		})
		w.app.Resize(termW, termH)
		w.showThemeEditor()
		_, c, ok := findRunes(editorGrid(w), "Controls ─")
		if !ok {
			t.Fatalf("Controls header not found on a %dx%d terminal", termW, termH)
		}
		return c
	}
	narrow := colOf(80, 24)
	wide := colOf(200, 50)
	if wide <= narrow {
		t.Errorf("Controls header column = %d on 80-wide, %d on 200-wide — the right column did not spread; the renderer is still keyed off the constants",
			narrow, wide)
	}
}

// TestThemeEditorNoScrollWhenGrown confirms the rendered grown editor needs no scrolling:
// on a 200×50 terminal the last below-the-fold role at the floor (code_bg) is already
// visible at scroll offset 0, because the taller viewport fits the whole content.
func TestThemeEditorNoScrollWhenGrown(t *testing.T) {
	issue204RestoreTheme(t)
	w := newTestWorkbench(t)
	w.SetHandlers(Handlers{
		GetTheme: func() config.ThemeConfig { return config.ThemeConfig{} },
		SetTheme: func(config.ThemeConfig) {},
	})
	w.app.Resize(200, 50)
	w.showThemeEditor()
	if !containsOnScreen(screenText(w), "Code block background:") {
		t.Errorf("code_bg is not visible at scrollY=0 on a 200x50 terminal — the grown editor should fit all roles without scrolling")
	}
}

// ----------------------------------------------------------------------------
// Theme editor — resize-while-open (path independence of the rendered content).
// ----------------------------------------------------------------------------

// TestThemeEditorOuterBoundsReResolveOnResize is the floor-spec resize property that DOES
// hold: opening the editor on a small terminal then enlarging it re-resolves the OUTER
// dialog rectangle (via dialog.Fit) to match a fresh open at the new size — the dialog
// frame grows and re-centres rather than staying pinned to the open-time terminal.
func TestThemeEditorOuterBoundsReResolveOnResize(t *testing.T) {
	openThemeEditor := func(w *Workbench) {
		w.SetHandlers(Handlers{
			GetTheme: func() config.ThemeConfig { return config.ThemeConfig{} },
			SetTheme: func(config.ThemeConfig) {},
		})
		w.showThemeEditor()
	}

	issue204RestoreTheme(t)
	resized := newTestWorkbench(t)
	resized.app.Resize(80, 24)
	openThemeEditor(resized)
	resized.app.Resize(200, 50)
	got := dialogBounds(resized)

	fresh := newTestWorkbench(t)
	fresh.app.Resize(200, 50)
	openThemeEditor(fresh)
	want := dialogBounds(fresh)

	if got != want {
		t.Errorf("theme editor outer bounds after resize = %+v, want %+v (a fresh open on the same terminal)", got, want)
	}
}

// TestThemeEditorReflowsContentOnResize asserts that resizing the terminal while the
// editor is open reflows its INTERNAL geometry too — the issue's substantive requirement
// that "the renderer must read the live window bounds each frame, not the constants".
// After opening on 80×24 and enlarging to 200×50, the rendered content (here the Save
// button's screen column) must match a fresh open at 200×50; otherwise the dialog frame
// grows to fill the terminal but its controls stay clustered in the open-time 80×22 box.
//
// This regressed in the first implementation (the layout was computed once at open and
// dialog.Fit re-resolved only the OUTER frame). The fix installs a layer.OnResize that
// re-derives the whole interior (viewport, every row, the scrollbar and the buttons) from
// the live bounds via relayout(); this test guards that fix.
func TestThemeEditorReflowsContentOnResize(t *testing.T) {
	openThemeEditor := func(w *Workbench) {
		w.SetHandlers(Handlers{
			GetTheme: func() config.ThemeConfig { return config.ThemeConfig{} },
			SetTheme: func(config.ThemeConfig) {},
		})
		w.showThemeEditor()
	}
	saveCol := func(w *Workbench) int {
		_, c, ok := findRunes(editorGrid(w), "Save")
		if !ok {
			t.Fatalf("Save button not found on screen")
		}
		return c
	}

	issue204RestoreTheme(t)
	resized := newTestWorkbench(t)
	resized.app.Resize(80, 24)
	openThemeEditor(resized)
	resized.app.Resize(200, 50)
	gotCol := saveCol(resized)

	fresh := newTestWorkbench(t)
	fresh.app.Resize(200, 50)
	openThemeEditor(fresh)
	wantCol := saveCol(fresh)

	if gotCol != wantCol {
		t.Errorf("after resize the Save button is at column %d, but a fresh open at 200x50 puts it at %d — "+
			"the editor's internal content did not reflow to the enlarged dialog (the renderer reads the open-time "+
			"bounds, not the live ones)", gotCol, wantCol)
	}
}

// TestThemeEditorResizeReflowsColumnsAndScroll hardens the resize-reflow fix beyond the
// single Save-button check: relayout() must re-place the scrolling ROWS (not only the
// fixed buttons), and shrinking the dialog back to the floor must re-enable scrolling so a
// below-the-fold role is reachable again. These exercise relayout()'s row-repositioning
// and scroll-clamp paths, which a button-only check would miss.
func TestThemeEditorResizeReflowsColumnsAndScroll(t *testing.T) {
	openThemeEditor := func(w *Workbench) {
		w.SetHandlers(Handlers{
			GetTheme: func() config.ThemeConfig { return config.ThemeConfig{} },
			SetTheme: func(config.ThemeConfig) {},
		})
		w.showThemeEditor()
	}
	controlsCol := func(w *Workbench) int {
		_, c, ok := findRunes(editorGrid(w), "Controls ─")
		if !ok {
			t.Fatalf("Controls header not found on screen")
		}
		return c
	}

	t.Run("the right column reflows on enlarge, not just the buttons", func(t *testing.T) {
		issue204RestoreTheme(t)
		resized := newTestWorkbench(t)
		resized.app.Resize(80, 24)
		openThemeEditor(resized)
		resized.app.Resize(200, 50)
		got := controlsCol(resized)

		fresh := newTestWorkbench(t)
		fresh.app.Resize(200, 50)
		openThemeEditor(fresh)
		want := controlsCol(fresh)

		if got != want {
			t.Errorf("after enlarge the Controls column is at %d, but a fresh open at 200x50 puts it at %d — "+
				"the scrolling rows did not spread to the wider dialog", got, want)
		}
	})

	t.Run("shrinking back to the floor re-enables scrolling", func(t *testing.T) {
		issue204RestoreTheme(t)
		w := newTestWorkbench(t)
		w.app.Resize(200, 50)
		openThemeEditor(w)
		// On the grown dialog the whole content fits — code_bg is visible without scrolling.
		if !containsOnScreen(screenText(w), "Code block background:") {
			t.Fatalf("precondition: code_bg should be visible on a grown 200x50 editor")
		}
		// Shrink to the floor: the viewport loses rows, the content overflows again, and
		// code_bg falls below the fold.
		w.app.Resize(80, 24)
		if containsOnScreen(screenText(w), "Code block background:") {
			t.Fatalf("precondition: code_bg should be below the fold after shrinking to 80x24")
		}
		// Scrolling must now reach it — relayout() re-enabled the scroll bounds.
		if !scrollEditorToReveal(w, "Code block background:") {
			t.Errorf("after shrinking to the floor, code_bg is unreachable by scrolling — relayout() did not re-enable the viewport scroll")
		}
	})
}
