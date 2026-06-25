package ui

import (
	"testing"

	"gogent/internal/config"

	tui "github.com/hobbestherat/turbotui"
	tv "github.com/hobbestherat/turbotui/turbotv"
)

// Supplementary TESTER coverage for #279/#291 + the scrolling editor. These probe the
// failure modes the driver's own suite does not pin down: an override reaching the
// *installed* tv.DefaultTheme for the stock default preset (the #291 core bug), the
// black-canvas window matching its panel, the wheel scroll *direction*, full role
// reachability across the scroll range, the NO_COLOR install path, and focus following a
// scroll. They only add tests — no implementation is touched.

// ----------------------------------------------------------------------------
// Component-tree helpers (the driver's scroll helpers don't expose the viewport).
// ----------------------------------------------------------------------------

func testerWalk(c *tv.VisualComponent, fn func(*tv.VisualComponent)) {
	if c == nil {
		return
	}
	fn(c)
	for _, ch := range c.Children() {
		testerWalk(ch, fn)
	}
}

// testerViewport returns the scrolling content container — the only component whose
// OnScrollFn is wired (set only when the content overflows the viewport).
func testerViewport(w *Workbench) *tv.VisualComponent {
	top := w.desktop.TopLayer()
	if top == nil || top.Root == nil {
		return nil
	}
	var vp *tv.VisualComponent
	testerWalk(top.Root, func(c *tv.VisualComponent) {
		if c.OnScrollFn != nil {
			vp = c
		}
	})
	return vp
}

// ----------------------------------------------------------------------------
// #291 — window background install / black-canvas parity.
// ----------------------------------------------------------------------------

// TestWindowOverrideReachesDefaultPresetInstall is the direct regression for #291's
// reported bug ("window background not changeable"). The default preset sets
// tv.DefaultTheme = baseTVTheme and then installs individual role fields; a window_bg
// override must land on tv.DefaultTheme.WindowBG (the slot the windows/transcript draw
// from), not be lost on the library default.
func TestWindowOverrideReachesDefaultPresetInstall(t *testing.T) {
	withThemeRestore(t)
	cfg := config.ThemeConfig{Overrides: map[string]string{"window_bg": "2", "window_fg": "11"}}
	ApplyTheme(ResolveTheme(cfg, truecolorEnv, false))
	if tv.DefaultTheme.WindowBG != tui.ANSIColor(2) {
		t.Errorf("window_bg override did not reach tv.DefaultTheme.WindowBG: got %+v, want ANSI 2 — the window is still not changeable (#291)", tv.DefaultTheme.WindowBG)
	}
	if tv.DefaultTheme.WindowFG != tui.ANSIColor(11) {
		t.Errorf("window_fg override did not reach tv.DefaultTheme.WindowFG: got %+v, want ANSI 11", tv.DefaultTheme.WindowFG)
	}
}

// TestTextSelectionOverrideReachesDefaultPresetInstall is the #279 analogue: a
// text_selection_bg override must reach the installed slot the input widgets read.
func TestTextSelectionOverrideReachesDefaultPresetInstall(t *testing.T) {
	withThemeRestore(t)
	cfg := config.ThemeConfig{Overrides: map[string]string{"text_selection_bg": "9"}}
	ApplyTheme(ResolveTheme(cfg, truecolorEnv, false))
	if tv.DefaultTheme.TextSelectionBG != tui.ANSIColor(9) {
		t.Errorf("text_selection_bg override did not reach tv.DefaultTheme.TextSelectionBG: got %+v, want ANSI 9", tv.DefaultTheme.TextSelectionBG)
	}
}

// TestWindowBlackCanvasMatchesPanel pins #291's "appearance unchanged" promise for the
// black-canvas presets: the window must equal the panel colours so those presets look
// identical while the window becomes overridable.
func TestWindowBlackCanvasMatchesPanel(t *testing.T) {
	for _, name := range []string{themeHighContrast, themeDark} {
		p := paletteByName(name)
		if p.WindowBG != p.PanelBG {
			t.Errorf("%s: WindowBG %+v != PanelBG %+v — black-canvas look would change", name, p.WindowBG, p.PanelBG)
		}
		if p.WindowFG != p.PanelFG {
			t.Errorf("%s: WindowFG %+v != PanelFG %+v", name, p.WindowFG, p.PanelFG)
		}
	}
}

// ----------------------------------------------------------------------------
// NO_COLOR install path (neutral builder sources the roles).
// ----------------------------------------------------------------------------

// TestNoColorInstallNeutralisesNewRoles verifies the install path under NO_COLOR:
// ApplyTheme builds neutralTVTheme (which must source Window*/TextSelection* from the
// degraded roles), so tv.DefaultTheme's new slots are the terminal default — and the
// selection background equals the focused-input fill, the documented reason a selection
// is not colour-distinguishable under NO_COLOR.
func TestNoColorInstallNeutralisesNewRoles(t *testing.T) {
	withThemeRestore(t)
	d := tui.DefaultColor()
	ApplyTheme(ResolveTheme(config.ThemeConfig{}, noColorEnv, false))
	for _, c := range []struct {
		name string
		c    tui.Color
	}{
		{"WindowBG", tv.DefaultTheme.WindowBG}, {"WindowFG", tv.DefaultTheme.WindowFG},
		{"TextSelectionBG", tv.DefaultTheme.TextSelectionBG}, {"TextSelectionFG", tv.DefaultTheme.TextSelectionFG},
	} {
		if c.c != d {
			t.Errorf("under NO_COLOR tv.DefaultTheme.%s = %+v, want terminal default", c.name, c.c)
		}
	}
	if tv.DefaultTheme.TextSelectionBG != tv.DefaultTheme.InputFocusBG {
		t.Errorf("under NO_COLOR TextSelectionBG (%+v) should equal InputFocusBG (%+v) — both terminal default",
			tv.DefaultTheme.TextSelectionBG, tv.DefaultTheme.InputFocusBG)
	}
}

// ----------------------------------------------------------------------------
// Scroll math + full reachability.
// ----------------------------------------------------------------------------

// TestScrollClampAndReachability pins the scroll bounds and proves every logical content
// row is visible at some offset in [0, maxScroll] — a too-small maxScroll (an off-by-one)
// would leave the last roles editable in principle but unreachable.
func TestScrollClampAndReachability(t *testing.T) {
	max := themeEditorMaxScroll()
	if max <= 0 {
		t.Fatalf("expected the new roles to overflow the viewport (maxScroll=%d)", max)
	}
	if got := clampThemeScroll(-7); got != 0 {
		t.Errorf("clampThemeScroll(-7) = %d, want 0", got)
	}
	if got := clampThemeScroll(max + 7); got != max {
		t.Errorf("clampThemeScroll(max+7) = %d, want %d", got, max)
	}
	if got := clampThemeScroll(1); got != 1 {
		t.Errorf("clampThemeScroll(1) = %d, want 1", got)
	}
	content, vis := themeEditorContentRows(), themeEditorVisibleRows
	for row := 0; row < content; row++ {
		reachable := false
		for y := 0; y <= max; y++ {
			if row >= y && row < y+vis {
				reachable = true
				break
			}
		}
		if !reachable {
			t.Errorf("logical content row %d is unreachable for any scrollY in [0,%d] (viewport %d)", row, max, vis)
		}
	}
}

// TestEveryRoleReachableByScrolling walks the editor through every scroll offset and
// asserts every role label appears at some offset — the end-to-end completeness check
// (the driver's suite only proves code_bg is reachable).
func TestEveryRoleReachableByScrolling(t *testing.T) {
	w, _ := issue267Render(t)
	needles := make([]string, 0, len(themeRoles))
	for _, r := range themeRoles {
		needles = append(needles, issue243WantLabels[r.key]+":")
	}
	pos := editorScrollFind(w, needles)
	for _, r := range themeRoles {
		if !pos[issue243WantLabels[r.key]+":"].found {
			t.Errorf("role %q (label %q) never visible at any scroll offset — unreachable/uneditable", r.key, issue243WantLabels[r.key])
		}
	}
}

// ----------------------------------------------------------------------------
// Mouse-wheel scrolling direction + focus-follows-scroll.
// ----------------------------------------------------------------------------

// TestWheelScrollsNaturalDirection drives the real viewport.OnScrollFn. turbotui delivers
// Delta = -1 for a downward wheel notch, and its scrollables advance with `offset -= Delta`;
// the editor must do the same so wheel-down reveals lower content. A sign flip would clamp
// wheel-down at the top and never reveal the bottom — caught here.
func TestWheelScrollsNaturalDirection(t *testing.T) {
	if themeEditorMaxScroll() == 0 {
		t.Skip("content fits — wheel scrolling not engaged")
	}
	w, _ := issue267Render(t)
	vp := testerViewport(w)
	if vp == nil || vp.OnScrollFn == nil {
		t.Fatalf("scrolling viewport (OnScrollFn) not found")
	}
	if containsOnScreen(screenText(w), "Code block background:") {
		t.Fatalf("precondition: code_bg should be below the initial fold")
	}
	// Wheel down (Delta = -1) repeatedly; clamps at the bottom.
	for i := 0; i < themeEditorContentRows(); i++ {
		vp.OnScrollFn(vp, tui.ScrollEvent{Delta: -1})
	}
	if !containsOnScreen(screenText(w), "Code block background:") {
		t.Errorf("after wheel-down, bottom role 'Code block background:' still hidden — wheel direction is inverted")
	}
	if containsOnScreen(screenText(w), "User messages:") {
		t.Errorf("after wheel-down to the bottom, top role 'User messages:' still visible — content did not scroll")
	}
	// Wheel up (Delta = +1) returns to the top.
	for i := 0; i < themeEditorContentRows(); i++ {
		vp.OnScrollFn(vp, tui.ScrollEvent{Delta: 1})
	}
	if !containsOnScreen(screenText(w), "User messages:") {
		t.Errorf("after wheel-up, top role 'User messages:' not visible — wheel-up did not return to the top")
	}
	if containsOnScreen(screenText(w), "Code block background:") {
		t.Errorf("after wheel-up to the top, bottom role 'Code block background:' still visible")
	}
}

// TestFocusFollowsScrollOffHiddenField covers keepFocusVisible: a focused field scrolled
// out of the viewport stops receiving keys (the desktop only delivers to a visible focused
// widget), which would strand keyboard scrolling. After a scroll hides the focused field,
// focus must move off it. Without the guard the hidden field stays focused.
func TestFocusFollowsScrollOffHiddenField(t *testing.T) {
	if themeEditorMaxScroll() == 0 {
		t.Skip("content fits — fields never scroll out of view")
	}
	w, _ := issue267Render(t)
	vp := testerViewport(w)
	if vp == nil || vp.OnScrollFn == nil {
		t.Fatalf("scrolling viewport not found")
	}
	// The first role's spec field (top of the left column, logical row 1) is on screen at
	// scrollY=0 and is the first to scroll off when paging down. After issue #462 the row
	// order is swatch → label → field, so the field sits at swatch+gap+label+gap from the
	// column origin (previously it was label+gap).
	left := themeEditorColumns()[0]
	fieldX := left.x + themeEditorSwatchW + 1 + left.labelW + 1
	var topField *tv.VisualComponent
	for _, ch := range vp.Children() {
		b := ch.Bounds
		if b.X == fieldX && b.Y == 1 && b.W == themeEditorFieldW {
			topField = ch
			break
		}
	}
	if topField == nil {
		t.Fatalf("could not locate the top-of-column spec field at x=%d,y=1", fieldX)
	}
	w.desktop.SetFocus(topField)
	if !topField.Focused() {
		t.Fatalf("setup: top field did not take focus")
	}
	for i := 0; i < themeEditorContentRows(); i++ {
		vp.OnScrollFn(vp, tui.ScrollEvent{Delta: -1})
	}
	if topField.Visible {
		t.Fatalf("precondition: top field still visible at max scroll — it should have scrolled out")
	}
	if topField.Focused() {
		t.Errorf("focus stranded on the hidden top field after scrolling — keepFocusVisible did not move it")
	}
}
