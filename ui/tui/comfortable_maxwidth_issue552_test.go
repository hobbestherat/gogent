package ui

import (
	"fmt"
	"testing"

	tv "github.com/hobbestherat/turbotui/turbotv"
	"gogent/internal/gogent"
)

// This file locks the issue #552 comfortable-max-width cap: a single
// comfortableMaxWidth (120) governs only INITIAL/AUTO sizing — new session
// windows at open, restored widths via applyLayout, and the three previously
// uncapped dialogs (Resources browser, Command palette, Help overlay). Every
// user-driven path (manual resize, drag, maximize, maximize-all, tiling,
// sidebar) must STILL reach full terminal width; the boundary-only clamps
// (maximizedWindowRect, clampWindowSize, clampWindowRect, tileArea, sidebar
// clamps) must never apply the comfortable cap.
//
// Several tests below are adversarial: they would fail if the cap leaked into a
// user-driven path, or if open/restore sizing drifted from min(pre-cap-math, 120).

// TestComfortableMaxWidthValue pins the chosen constant. 120 matches the
// review-viewer / agent-monologue precedent; changing it is a deliberate
// product decision, not an incidental edit.
func TestComfortableMaxWidthValue(t *testing.T) {
	if comfortableMaxWidth != 120 {
		t.Errorf("comfortableMaxWidth = %d, want 120 (issue #552; review/monologue precedent)",
			comfortableMaxWidth)
	}
}

// preCapWindowWidth reproduces openWindowAny's width math EXACTLY, minus the
// new comfortableMaxWidth cap. A test can then assert the open width equals
// min(preCapWindowWidth(...), comfortableMaxWidth) across the whole terminal
// range — proving the cap is applied as a pure ceiling on the existing math,
// not a re-derivation.
func preCapWindowWidth(appW, sidebarW int) int {
	avail := appW - sidebarW
	if avail < 50 {
		avail = appW
	}
	w := avail * 90 / 100
	if w < 50 {
		w = avail - 2
	}
	return w
}

// TestSessionWindowOpenWidthIsCappedPreCapMath is the core acceptance test for
// new-window sizing: across narrow, boundary and very-wide terminals the open
// width is exactly min(pre-cap 90% math, comfortableMaxWidth). This catches a
// mis-placed cap (e.g. applied before the <50 floor, or not at all) and a
// wrong boundary.
func TestSessionWindowOpenWidthIsCappedPreCapMath(t *testing.T) {
	for _, cols := range []int{60, 80, 100, 120, 133, 134, 135, 149, 150, 200, 400, 766, 1000} {
		w := newTestWorkbench(t)
		w.app.Resize(cols, 50)
		w.openWindow("s", "S")
		got := w.sessions["s"].window.Component.Bounds.W

		preCap := preCapWindowWidth(cols, w.sidebarWidth())
		want := preCap
		if want > comfortableMaxWidth {
			want = comfortableMaxWidth
		}
		if got != want {
			t.Errorf("cols=%d: open width = %d, want pre-cap %d capped to %d", cols, got, preCap, want)
		}
		if got > comfortableMaxWidth {
			t.Errorf("cols=%d: open width %d exceeds the comfortable cap %d", cols, got, comfortableMaxWidth)
		}
	}
}

// TestSessionWindowOpenCappedOnWideTerminal is the literal acceptance criterion:
// on a terminal well over ~400 cols a freshly opened window is no wider than 120.
func TestSessionWindowOpenCappedOnWideTerminal(t *testing.T) {
	for _, cols := range []int{400, 500, 766} {
		w := newTestWorkbench(t)
		w.app.Resize(cols, 50)
		w.openWindow("s", "S")
		if got := w.sessions["s"].window.Component.Bounds.W; got != comfortableMaxWidth {
			t.Errorf("cols=%d: new window width = %d, want exactly %d", cols, got, comfortableMaxWidth)
		}
	}
}

// TestReadOnlyAnalysisWindowAlsoCapped covers the readOnly=true branch of
// openWindowAny (OpenAnalysisSession): analysis windows balloon the same way
// and must be capped too.
func TestReadOnlyAnalysisWindowAlsoCapped(t *testing.T) {
	w := newTestWorkbench(t)
	w.app.Resize(766, 50)
	w.openWindowAny("a", "Analysis", true) // readOnly path shared with OpenAnalysisSession
	got := w.sessions["a"].window.Component.Bounds.W
	if got != comfortableMaxWidth {
		t.Errorf("read-only/analysis window width = %d, want %d", got, comfortableMaxWidth)
	}
}

// TestCascadedWindowsAllCappedAndOnScreen opens a full cascade (offset wraps at
// 6) on a 766-col terminal: every window is capped, and none is pushed off-screen
// by the cascade offset.
func TestCascadedWindowsAllCappedAndOnScreen(t *testing.T) {
	const cols = 766
	w := newTestWorkbench(t)
	w.app.Resize(cols, 50)
	for i := 0; i < 6; i++ {
		w.openWindow(fmt.Sprintf("s%d", i), "S")
	}
	for i := 0; i < 6; i++ {
		b := w.sessions[fmt.Sprintf("s%d", i)].window.Component.Bounds
		if b.W != comfortableMaxWidth {
			t.Errorf("window %d width = %d, want %d", i, b.W, comfortableMaxWidth)
		}
		if b.X < 0 || b.Y < 0 || b.X+b.W > cols {
			t.Errorf("window %d bounds %+v run off-screen on a %d-col terminal", i, b, cols)
		}
	}
}

// TestMaximizeReachesFullWidthOnWideTerminal is the CRITICAL no-regression test
// for the #552 lifecycle scoping: a window opens capped at 120, but Maximize must
// still fill the whole work area, and unmaximize must return to the capped
// bounds. This fails if the cap leaked into maximizedWindowRect / Maximize.
func TestMaximizeReachesFullWidthOnWideTerminal(t *testing.T) {
	w := newTestWorkbench(t)
	w.app.Resize(766, 50)
	w.openWindow("s", "S")
	sw := w.sessions["s"]

	if b := sw.window.Component.Bounds; b.W != comfortableMaxWidth {
		t.Fatalf("initial open width = %d, want %d before maximize", b.W, comfortableMaxWidth)
	}

	sw.Maximize()
	if !sw.IsMaximized() {
		t.Fatal("window not maximized after Maximize()")
	}
	maxed := sw.window.Component.Bounds
	area := w.windowArea()
	if maxed.W != area.W {
		t.Errorf("maximized width = %d, want full work-area width %d (cap must NOT constrain maximize)",
			maxed.W, area.W)
	}
	if maxed.W <= comfortableMaxWidth {
		t.Errorf("maximized width = %d still at/under the cap — maximize must fill the area", maxed.W)
	}

	// Unmaximize restores the exact pre-maximize (capped) bounds.
	sw.ToggleMaximize()
	if sw.IsMaximized() {
		t.Fatal("window still maximized after toggle")
	}
	if b := sw.window.Component.Bounds; b.W != comfortableMaxWidth {
		t.Errorf("post-unmaximize width = %d, want restored to %d", b.W, comfortableMaxWidth)
	}
}

// TestMaximizeAllReachesFullWidthOnWideTerminal covers the bulk "Maximize All"
// path (tiling.maximizeAll → tileArea → maximizedWindowRect): it must expand
// every window to the full area on a wide terminal, not the comfortable cap.
func TestMaximizeAllReachesFullWidthOnWideTerminal(t *testing.T) {
	w := newTestWorkbench(t)
	w.app.Resize(766, 50)
	w.openWindow("a", "A")
	w.openWindow("b", "B")

	w.maximizeAll()
	area := w.tileArea()
	for _, id := range []string{"a", "b"} {
		got := w.sessions[id].window.Component.Bounds.W
		if got != area.W {
			t.Errorf("maximizeAll: window %q width = %d, want full tile-area width %d", id, got, area.W)
		}
		if got <= comfortableMaxWidth {
			t.Errorf("maximizeAll: window %q width = %d still at/under the cap", id, got)
		}
	}
}

// TestTileAreaSpansFullWorkAreaOnWideTerminal guards the tiling arrange target:
// tileArea() (used by arrange and maximizeAll) must span the full work area on a
// wide terminal — it is boundary-derived, never comfortable-capped.
func TestTileAreaSpansFullWorkAreaOnWideTerminal(t *testing.T) {
	w := newTestWorkbench(t)
	w.app.Resize(766, 50)
	area := w.tileArea()
	if area.W <= comfortableMaxWidth {
		t.Errorf("tileArea width = %d, must span the full work area (>%d) on a wide terminal",
			area.W, comfortableMaxWidth)
	}
	if area.W != w.windowArea().W {
		t.Errorf("tileArea width = %d, want windowArea width %d", area.W, w.windowArea().W)
	}
}

// TestClampWindowSizeAllowsWidthBeyondCap guards the resize-drag path
// (constrainWindowToBounds → clampWindowSize): it is boundary-only, so a window
// resized to 400 cols on a wide terminal keeps 400. Fails if the comfortable cap
// leaked in.
func TestClampWindowSizeAllowsWidthBeyondCap(t *testing.T) {
	w := newTestWorkbench(t)
	w.app.Resize(766, 50)
	area := w.windowArea()
	got := clampWindowSize(tv.Rect{X: 0, Y: 0, W: 400, H: 20}, area, 50, 5)
	if got.W != 400 {
		t.Errorf("clampWindowSize width = %d, want 400 (boundary-only; cap must not apply)", got.W)
	}
}

// TestClampWindowRectAllowsWidthBeyondCap guards the move/drag + restore boundary
// path: clampWindowRect is boundary-only, so a 400-wide rect within the screen
// keeps 400.
func TestClampWindowRectAllowsWidthBeyondCap(t *testing.T) {
	got := clampWindowRect(tv.Rect{X: 10, Y: 5, W: 400, H: 20}, 766, 50, 50, 5)
	if got.W != 400 {
		t.Errorf("clampWindowRect width = %d, want 400 (boundary-only; cap must not apply)", got.W)
	}
}

// TestApplyLayoutCeilingClampsRestoredWidth is the acceptance test for the
// restore path (option b ceiling): a persisted width ≤120 restores verbatim; a
// wider one clamps down to 120. The ceiling is WIDTH-ONLY — position (X/Y) and
// height are preserved (subject only to clampWindowRect's on-screen boundary
// clamp). This catches a restore that re-introduces the reported ~660-col window.
func TestApplyLayoutCeilingClampsRestoredWidth(t *testing.T) {
	for _, tc := range []struct {
		name   string
		savedW int
		wantW  int
	}{
		{"well under cap unchanged", 80, 80},
		{"exactly at cap unchanged", 120, 120},
		{"one over cap clamps down", 121, 120},
		{"wide saved clamps (reported annoyance)", 660, 120},
		{"very wide saved clamps", 400, 120},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := newTestWorkbench(t)
			w.app.Resize(766, 50)
			w.openWindow("s", "S") // register the session so applyLayout finds it
			w.applyLayout(gogent.Layout{Entries: []gogent.LayoutEntry{{
				ID: "s", X: 100, Y: 5, W: tc.savedW, H: 20,
			}}})
			got := w.sessions["s"].window.Component.Bounds
			if got.W != tc.wantW {
				t.Errorf("restored width = %d, want %d (savedW=%d)", got.W, tc.wantW, tc.savedW)
			}
			// Width-only ceiling: position and height pass through unchanged
			// (100+120 and 5+20 are well inside the 766×50 work area, so the
			// boundary clamp does not move them).
			if got.H != 20 {
				t.Errorf("restored height = %d, want 20 (ceiling is width-only)", got.H)
			}
			if got.X != 100 || got.Y != 5 {
				t.Errorf("restored origin = (%d,%d), want (100,5) (ceiling is width-only)", got.X, got.Y)
			}
		})
	}
}

// TestApplyLayoutCeilingHonoursBoundaryClamp confirms the ceiling composes with
// the boundary clamp: a wide saved width on a NARROW restore terminal clamps to
// the cap AND then to the screen. (Saved 300 on a 90-col terminal → min-ish of
// 120 and the screen, but never larger than 120.)
func TestApplyLayoutCeilingHonoursBoundaryClamp(t *testing.T) {
	w := newTestWorkbench(t)
	w.app.Resize(90, 30)
	w.openWindow("s", "S")
	w.applyLayout(gogent.Layout{Entries: []gogent.LayoutEntry{{
		ID: "s", X: 0, Y: 0, W: 300, H: 20,
	}}})
	got := w.sessions["s"].window.Component.Bounds.W
	if got > comfortableMaxWidth {
		t.Errorf("restored width = %d on a 90-col terminal, must not exceed the cap %d", got, comfortableMaxWidth)
	}
}

// TestBallooningDialogsCappedOnWideTerminal is the dialog acceptance test: on a
// 766-col terminal the Resources browser, Command palette and Help overlay all
// open at exactly 120 instead of the old ~612–651.
func TestBallooningDialogsCappedOnWideTerminal(t *testing.T) {
	for _, tc := range []struct {
		name string
		open func(*Workbench)
	}{
		{"resources-browser", func(w *Workbench) { w.showResourcesDialog() }},
		{"command-palette", func(w *Workbench) { w.showCommandPalette() }},
		{"help-overlay", func(w *Workbench) { w.showHelpOverlay() }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := newTestWorkbench(t)
			w.app.Resize(766, 50)
			tc.open(w)
			b := dialogBounds(w)
			if b.W > comfortableMaxWidth {
				t.Errorf("%s width = %d, must be <= %d on a 766-col terminal", tc.name, b.W, comfortableMaxWidth)
			}
			if b.W != comfortableMaxWidth {
				t.Errorf("%s width = %d, want exactly %d (cap binds above ~150 cols)", tc.name, b.W, comfortableMaxWidth)
			}
			// Still centered on the wide terminal.
			if b.X != (766-b.W)/2 {
				t.Errorf("%s origin X = %d, want centered (%d)", tc.name, b.X, (766-b.W)/2)
			}
		})
	}
}

// TestBallooningDialogsNoOpOnNarrowTerminal confirms the cap is inert below the
// ~150-col threshold: at 100 cols the three dialogs keep their pre-#552 sizing
// (the 80% default = 80), so the fix does not shrink normal terminals.
func TestBallooningDialogsNoOpOnNarrowTerminal(t *testing.T) {
	for _, tc := range []struct {
		name string
		open func(*Workbench)
	}{
		{"resources-browser", func(w *Workbench) { w.showResourcesDialog() }},
		{"command-palette", func(w *Workbench) { w.showCommandPalette() }},
		{"help-overlay", func(w *Workbench) { w.showHelpOverlay() }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := newTestWorkbench(t)
			w.app.Resize(100, 30)
			tc.open(w)
			b := dialogBounds(w)
			if b.W != 80 { // 80% of 100; the browser's 85% PreferredW clamps to it
				t.Errorf("%s width = %d, want 80 (80%% default; cap inert at 100 cols)", tc.name, b.W)
			}
			if b.W > comfortableMaxWidth {
				t.Errorf("%s width = %d exceeds cap on a narrow terminal", tc.name, b.W)
			}
		})
	}
}

// TestBrowserDialogSpecResolvesCapped verifies browserDialogSpec's resolved
// width is min(80% of screen, comfortableMaxWidth) across the range, including
// the boundary where 80% crosses 120.
func TestBrowserDialogSpecResolvesCapped(t *testing.T) {
	w := newTestWorkbench(t)
	for _, screenW := range []int{80, 100, 120, 149, 150, 160, 200, 766} {
		w.app.Resize(screenW, 50)
		_, _, gotW, _ := tv.ResolveDialogRect(w.browserDialogSpec(), screenW, 50)
		want := screenW * 80 / 100
		if want > comfortableMaxWidth {
			want = comfortableMaxWidth
		}
		if gotW != want {
			t.Errorf("screenW=%d: browser resolved width = %d, want %d", screenW, gotW, want)
		}
		if gotW > comfortableMaxWidth {
			t.Errorf("screenW=%d: browser width %d exceeds the cap", screenW, gotW)
		}
	}
}

// TestResourcesSpecMirrorsBrowserDialogSpec guards against the test-mirror drift
// found during review: resourcesSpec must stay a faithful mirror of
// browserDialogSpec (same fields, same resolved width), so it cannot silently lag
// behind a spec change like the MaxW addition.
func TestResourcesSpecMirrorsBrowserDialogSpec(t *testing.T) {
	w := newTestWorkbench(t)
	for _, screenW := range []int{80, 120, 200, 766} {
		w.app.Resize(screenW, 50)
		real := w.browserDialogSpec()
		mirror := resourcesSpec(screenW)
		if real.MinW != mirror.MinW || real.MaxW != mirror.MaxW ||
			real.MinH != mirror.MinH || real.PreferredW != mirror.PreferredW {
			t.Errorf("screenW=%d: resourcesSpec mirror %+v diverges from browserDialogSpec %+v",
				screenW, mirror, real)
		}
		_, _, rw, _ := tv.ResolveDialogRect(real, screenW, 50)
		_, _, mw, _ := tv.ResolveDialogRect(mirror, screenW, 50)
		if rw != mw {
			t.Errorf("screenW=%d: mirror resolves to %d, real to %d — they must agree",
				screenW, mw, rw)
		}
	}
}
