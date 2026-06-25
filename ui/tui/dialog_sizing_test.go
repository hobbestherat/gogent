package ui

import (
	"testing"

	"gogent/internal/stats"

	tv "github.com/hobbestherat/turbotui/turbotv"
)

// dialogBounds returns the outer rectangle of the top-most open dialog window, so
// a test can assert the size an opened dialog actually resolved to.
func dialogBounds(w *Workbench) tv.Rect {
	top := w.desktop.TopLayer()
	if top == nil || top.Root == nil {
		return tv.Rect{}
	}
	return top.Root.Bounds
}

// TestResolveDialogRectPolicy locks the turbotui sizing policy every gogent dialog
// now rests on after #309: the 80%/85% percentage is a CAP the content grows
// toward, not a floor it is forced up to. A zero preferred fills the percentage
// default; a content-driven PreferredW BELOW the default is honoured (the dialog
// sizes to its content); a PreferredW ABOVE the default is clamped down to it; the
// Min floor still wins on a tiny screen even past the edge; an explicit Max caps
// growth; and the result is centered with a non-negative origin.
func TestResolveDialogRectPolicy(t *testing.T) {
	t.Run("zero preferred fills the percentage default", func(t *testing.T) {
		x, y, w, h := tv.ResolveDialogRect(tv.DialogSpec{}, 200, 50)
		if w != 160 || h != 42 { // 80% of 200, 85% of 50
			t.Errorf("size = %dx%d, want 160x42", w, h)
		}
		if x != (200-w)/2 || y != (50-h)/2 {
			t.Errorf("origin = (%d,%d), want centered (%d,%d)", x, y, (200-w)/2, (50-h)/2)
		}
	})

	t.Run("min floor wins on a tiny terminal", func(t *testing.T) {
		_, _, w, h := tv.ResolveDialogRect(tv.DialogSpec{MinW: 60, MinH: 14}, 40, 10)
		if w < 60 || h < 14 {
			t.Errorf("size = %dx%d, want at least 60x14 (floor honoured past the edge)", w, h)
		}
	})

	t.Run("origin floored at zero when dialog exceeds screen", func(t *testing.T) {
		x, y, _, _ := tv.ResolveDialogRect(tv.DialogSpec{MinW: 60, MinH: 14}, 40, 10)
		if x < 0 || y < 0 {
			t.Errorf("origin = (%d,%d), want both >= 0", x, y)
		}
	})

	t.Run("explicit max caps growth below the default", func(t *testing.T) {
		_, _, w, h := tv.ResolveDialogRect(tv.DialogSpec{MaxW: 50, MaxH: 20}, 200, 50)
		if w != 50 || h != 20 {
			t.Errorf("size = %dx%d, want capped 50x20", w, h)
		}
	})

	t.Run("preferred below the default is honoured (cap not floor)", func(t *testing.T) {
		// The #309 inversion: a content-driven PreferredW under the 80% default (160)
		// now sizes the dialog to its content instead of inflating to the default.
		_, _, w, _ := tv.ResolveDialogRect(tv.DialogSpec{PreferredW: 100}, 200, 50)
		if w != 100 {
			t.Errorf("width = %d, want 100 (a small PreferredW now sizes to content)", w)
		}
	})

	t.Run("preferred above the default is capped at it", func(t *testing.T) {
		// The percentage is now an upper bound: a PreferredW above the 80% default
		// (160) is clamped down to it even though the screen (196 usable) could hold
		// it. This is the same clamp that bites the model editor and the browsers.
		_, _, w, _ := tv.ResolveDialogRect(tv.DialogSpec{PreferredW: 180}, 200, 50)
		if w != 160 {
			t.Errorf("width = %d, want 160 (the percentage default is a cap on PreferredW)", w)
		}
	})

	t.Run("a Max above the percentage cap does not lift the cap", func(t *testing.T) {
		// Even with a generous MaxW, the percentage default still clamps a large
		// PreferredW: the cap is applied before Max, so Max can only tighten, never
		// loosen, the percentage ceiling. (Only MinW, applied last, can raise width
		// back above the percentage.)
		_, _, w, _ := tv.ResolveDialogRect(tv.DialogSpec{PreferredW: 180, MaxW: 190}, 200, 50)
		if w != 160 {
			t.Errorf("width = %d, want 160 (MaxW above the percentage cap cannot lift it)", w)
		}
	})
}

// TestDialogRectWrapper checks the gogent wrapper forwards the live terminal size
// to turbotui's resolver — at the initial size and after a resize — so it is a
// pure pass-through to the one shared policy with no second sizing layer (#299).
func TestDialogRectWrapper(t *testing.T) {
	w := newTestWorkbench(t)
	spec := tv.DialogSpec{MinW: 50, MinH: 12}
	for _, dim := range []struct{ W, H int }{{80, 25}, {200, 50}, {44, 14}} {
		w.app.Resize(dim.W, dim.H)
		gotX, gotY, gotW, gotH := w.dialogRect(spec)
		wantX, wantY, wantW, wantH := tv.ResolveDialogRect(spec, dim.W, dim.H)
		if gotX != wantX || gotY != wantY || gotW != wantW || gotH != wantH {
			t.Errorf("dialogRect at %dx%d = (%d,%d,%d,%d), want (%d,%d,%d,%d)",
				dim.W, dim.H, gotX, gotY, gotW, gotH, wantX, wantY, wantW, wantH)
		}
	}
}

// TestDialogsSizedToContent is the #309 acceptance test (it replaces the old
// TestDialogsLargeByDefault, which demanded every dialog be exactly 160×42 and so
// codified the very regression #309 fixes). On a roomy 200×50 terminal:
//
//   - small-content dialogs (a one-field input, a one-line confirm) resolve to
//     their CONTENT size — materially smaller than the 160×42 percentage default
//     in BOTH dimensions — never the full percentage box;
//   - list-driven dialogs (command palette, help overlay) stay wide at the
//     percentage default but have their HEIGHT capped to their item count, so they
//     do not fill 42 rows when short.
//
// Every dialog stays centered and on-screen.
func TestDialogsSizedToContent(t *testing.T) {
	const termW, termH = 200, 50
	const defW, defH = 160, 42 // the 80%×85% percentage default

	t.Run("small dialogs size to content, not the percentage box", func(t *testing.T) {
		for _, tc := range []struct {
			name string
			open func(*Workbench)
		}{
			{"input-dialog", func(w *Workbench) { w.showInputDialog("Rename", "New name", "", nil) }},
			{"confirm-dialog", func(w *Workbench) { w.showConfirm("Quit", "Are you sure?", func(bool) {}) }},
		} {
			t.Run(tc.name, func(t *testing.T) {
				w := newTestWorkbench(t)
				w.app.Resize(termW, termH)
				tc.open(w)
				b := dialogBounds(w)
				// Materially smaller than the percentage default on BOTH axes — the
				// crux of the bug report ("a one-line confirm renders as 160×42").
				if b.W >= defW || b.H >= defH {
					t.Errorf("%s size = %dx%d, want materially smaller than the %dx%d default", tc.name, b.W, b.H, defW, defH)
				}
				if b.W > defW/2 || b.H > defH/2 {
					t.Errorf("%s size = %dx%d, want well under half the %dx%d default for one-line content", tc.name, b.W, b.H, defW, defH)
				}
				if b.X != (termW-b.W)/2 || b.Y != (termH-b.H)/2 {
					t.Errorf("%s origin = (%d,%d), want centered", tc.name, b.X, b.Y)
				}
			})
		}
	})

	t.Run("list-driven dialogs stay wide but cap height to content", func(t *testing.T) {
		for _, tc := range []struct {
			name string
			open func(*Workbench)
		}{
			{"command-palette", func(w *Workbench) { w.showCommandPalette() }},
			{"help-overlay", func(w *Workbench) { w.showHelpOverlay() }},
		} {
			t.Run(tc.name, func(t *testing.T) {
				w := newTestWorkbench(t)
				w.app.Resize(termW, termH)
				tc.open(w)
				b := dialogBounds(w)
				if b.W != defW {
					t.Errorf("%s width = %d, want the %d percentage default (list-driven)", tc.name, b.W, defW)
				}
				// Height never exceeds the default, and is capped to the item count
				// (+chrome) so a short list does not fill 42 rows.
				if b.H > defH {
					t.Errorf("%s height = %d, want <= %d", tc.name, b.H, defH)
				}
				if b.X != (termW-b.W)/2 || b.Y != (termH-b.H)/2 {
					t.Errorf("%s origin = (%d,%d), want centered", tc.name, b.X, b.Y)
				}
			})
		}
	})

	t.Run("statistics sizes to its fixed report content", func(t *testing.T) {
		w := newTestWorkbench(t)
		w.handlers.GetStatistics = func() stats.Report { return sampleStatsReport() }
		w.app.Resize(termW, termH)
		w.showStatisticsDialog()
		b := dialogBounds(w)
		if b.W != 100 || b.H != 24 {
			t.Fatalf("statistics size = %dx%d, want content footprint 100x24", b.W, b.H)
		}
		if b.W == defW && b.H == defH {
			t.Fatalf("statistics is still the regressed %dx%d browser balloon", defW, defH)
		}
		if b.X != (termW-b.W)/2 || b.Y != (termH-b.H)/2 {
			t.Errorf("statistics origin = (%d,%d), want centered", b.X, b.Y)
		}
	})
}

// TestConfirmDialogNotHugeForOneLine is the explicit guard the bug report asked
// for: a one-line confirmation is NOT the 160×42 box that motivated #309. It is
// materially smaller in BOTH dimensions on a roomy terminal.
func TestConfirmDialogNotHugeForOneLine(t *testing.T) {
	const termW, termH = 200, 50
	w := newTestWorkbench(t)
	w.app.Resize(termW, termH)
	w.showConfirm("Quit", "Are you sure?", func(bool) {})
	b := dialogBounds(w)
	if b.W == 160 && b.H == 42 {
		t.Fatalf("one-line confirm is still the regressed 160x42 box")
	}
	// A 13-char question needs ~30 cols and ~7 rows; assert it is comfortably small.
	if b.W > 60 {
		t.Errorf("one-line confirm width = %d, want compact (<= 60)", b.W)
	}
	if b.H > 12 {
		t.Errorf("one-line confirm height = %d, want compact (<= 12)", b.H)
	}
}

// TestDialogsClampToTinyTerminal opens the same dialogs on a tiny terminal and
// checks they honour their Min floors (never collapsing) while keeping the origin
// on-screen.
func TestDialogsClampToTinyTerminal(t *testing.T) {
	const termW, termH = 48, 14
	for _, tc := range []struct {
		name             string
		open             func(*Workbench)
		minWidth, minHgt int
	}{
		{"command-palette", func(w *Workbench) { w.showCommandPalette() }, 40, 10},
		{"help-overlay", func(w *Workbench) { w.showHelpOverlay() }, 44, 12},
		{"input-dialog", func(w *Workbench) { w.showInputDialog("Rename", "New name", "", nil) }, 40, 7},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := newTestWorkbench(t)
			w.app.Resize(termW, termH)
			tc.open(w)
			b := dialogBounds(w)
			if b.W < tc.minWidth || b.H < tc.minHgt {
				t.Errorf("%s size = %dx%d, want at least %dx%d (floor)", tc.name, b.W, b.H, tc.minWidth, tc.minHgt)
			}
			if b.X < 0 || b.Y < 0 {
				t.Errorf("%s origin = (%d,%d), want both >= 0", tc.name, b.X, b.Y)
			}
		})
	}
}

// TestDialogReResolvesOnResize is the acceptance test for "resizing the terminal
// while a dialog is open re-resolves its size" (issue #299). dialog.Fit / the
// reflow hook must re-center the open dialog on resize.
//
// The command palette is percentage-driven, so growing the terminal grows it (to
// the 80% width default). The input dialog is content-fixed (one field), so after
// #309 it keeps its content size across the resize but must still re-center — the
// new distinction: small dialogs no longer balloon to the percentage box.
func TestDialogReResolvesOnResize(t *testing.T) {
	t.Run("percentage-driven palette grows with the terminal", func(t *testing.T) {
		w := newTestWorkbench(t)
		w.app.Resize(80, 24)
		w.showCommandPalette()
		before := dialogBounds(w)

		w.app.Resize(200, 50)
		after := dialogBounds(w)

		if after.W <= before.W {
			t.Errorf("palette width did not grow on resize: before=%d after=%d", before.W, after.W)
		}
		if after.W != 160 {
			t.Errorf("palette width after resize = %d, want 160 (80%% of 200)", after.W)
		}
		if after.X != (200-after.W)/2 || after.Y != (50-after.H)/2 {
			t.Errorf("palette not re-centered after resize: origin (%d,%d)", after.X, after.Y)
		}
	})

	t.Run("content-fixed input re-centers without ballooning", func(t *testing.T) {
		w := newTestWorkbench(t)
		w.app.Resize(80, 24)
		w.showInputDialog("Rename", "New name", "", nil)
		before := dialogBounds(w)

		w.app.Resize(200, 50)
		after := dialogBounds(w)

		// Same content size (a single field) on both terminals — not inflated to 160×42.
		if after.W != before.W || after.H != before.H {
			t.Errorf("input size changed on resize: before=%dx%d after=%dx%d (content should stay fixed)",
				before.W, before.H, after.W, after.H)
		}
		if after.W >= 160 || after.H >= 42 {
			t.Errorf("input ballooned to %dx%d on resize, want content size", after.W, after.H)
		}
		// But it must re-center on the larger terminal.
		if after.X != (200-after.W)/2 || after.Y != (50-after.H)/2 {
			t.Errorf("input not re-centered after resize: origin (%d,%d)", after.X, after.Y)
		}
		if after.X == before.X && after.Y == before.Y {
			t.Errorf("input origin unchanged after resize: stayed (%d,%d)", after.X, after.Y)
		}
	})
}

// TestConfirmDialogResizePathIndependent pins a DEFECT in the confirm/message
// dialog's resize re-resolution (issue #299 acceptance: "resizing the terminal
// while a dialog is open re-resolves the dialog size", and dialogs must "grow with
// terminal size").
//
// messageDialogSpec bakes the OPEN-TIME terminal height into the spec it hands to
// dialog.Fit: MaxH = termH-2 (and PrefH is measured against the open-time width).
// dialog.Fit remembers that spec for the resize hook, so when the terminal grows,
// reflow() re-resolves against the *stale* MaxH. The result: width grows to ~80%
// of the new terminal, but the HEIGHT stays capped at the original terminal's
// height instead of growing to ~85% of the new one.
//
// The crisp symptom: the same confirm on the same 200×50 terminal is a different
// size depending on how it got there — 160×42 when opened fresh (see
// TestDialogsLargeByDefault), but only 160×22 when opened on 80×24 and resized up.
// Opening should be path-independent. This test fails until the message/permission
// dialogs re-derive the terminal-dependent spec fields on resize (or drop the
// terminal-derived MaxH in favour of letting the resolver cap to the screen).
func TestConfirmDialogResizePathIndependent(t *testing.T) {
	resized := newTestWorkbench(t)
	resized.app.Resize(80, 24)
	resized.showConfirm("Quit", "Sure?", func(bool) {})
	resized.app.Resize(200, 50)
	got := dialogBounds(resized)

	fresh := newTestWorkbench(t)
	fresh.app.Resize(200, 50)
	fresh.showConfirm("Quit", "Sure?", func(bool) {})
	want := dialogBounds(fresh)

	if got.W != want.W {
		t.Errorf("width after resize = %d, want %d (fresh open on the same terminal)", got.W, want.W)
	}
	if got.H != want.H {
		t.Errorf("height after resize = %d, want %d: a confirm resized into 200×50 must match one opened fresh there; "+
			"the open-time MaxH=termH-2 is baked into the remembered spec and caps vertical growth (DEFECT)", got.H, want.H)
	}
}

// TestDialogShrinksOnResize checks the re-resolution also shrinks an open dialog
// when the terminal gets smaller, not just grows it.
func TestDialogShrinksOnResize(t *testing.T) {
	w := newTestWorkbench(t)
	w.app.Resize(200, 50)
	w.showCommandPalette()
	big := dialogBounds(w)

	w.app.Resize(80, 24)
	small := dialogBounds(w)

	if small.W >= big.W || small.H >= big.H {
		t.Errorf("dialog did not shrink: big=%+v small=%+v", big, small)
	}
	if small.W != 64 || small.H != 20 { // 80% of 80, 85% of 24 (= 20)
		t.Errorf("shrunk size = %dx%d, want 64x20", small.W, small.H)
	}
}

// TestBrowserDialogSpecTracksLiveTerminal locks the fix that the Resources browser
// recomputes PreferredW from the CURRENT terminal (issue #299) rather than baking
// the open-time width. browserDialogSpec
// is the specFn handed to installResizeReflow, so its PreferredW must follow the
// live w.app width; otherwise a browser opened small then enlarged would grow only
// to the 80% default instead of its intended 85%.
func TestBrowserDialogSpecTracksLiveTerminal(t *testing.T) {
	w := newTestWorkbench(t)
	for _, dim := range []struct{ W, H int }{{80, 24}, {200, 50}, {120, 40}} {
		w.app.Resize(dim.W, dim.H)
		spec := w.browserDialogSpec()
		if spec.PreferredW != dim.W*85/100 {
			t.Errorf("at width %d: browserDialogSpec PreferredW = %d, want %d (live 85%%)",
				dim.W, spec.PreferredW, dim.W*85/100)
		}
		if spec.MinW != 60 || spec.MinH != 14 {
			t.Errorf("browserDialogSpec floors = %dx%d, want 60x14", spec.MinW, spec.MinH)
		}
	}
}

// TestBrowserPreferredWidthClamped documents a #309 finding: the Resources browser
// still declares PreferredW = 85% of the terminal, but turbotui's percentage is now
// an 80% CAP, so ResolveDialogRect clamps the 85% request down to 80%. The 85%
// intent is therefore dead — the browser renders at 80% wide. This remains
// intentional for Resources, which genuinely benefits from the large browser
// footprint.
func TestBrowserPreferredWidthClamped(t *testing.T) {
	w := newTestWorkbench(t)
	for _, screenW := range []int{120, 160, 200} {
		w.app.Resize(screenW, 50)
		spec := w.browserDialogSpec()
		if spec.PreferredW != screenW*85/100 {
			t.Fatalf("browserDialogSpec PreferredW = %d, want %d (the 85%% intent)", spec.PreferredW, screenW*85/100)
		}
		_, _, gotW, _ := tv.ResolveDialogRect(spec, screenW, 50)
		want := screenW * 80 / 100 // the 80% cap, below the 85% the spec asks for
		if gotW != want {
			t.Errorf("screenW=%d: resolved width = %d, want %d (85%% PreferredW clamped to the 80%% cap)", screenW, gotW, want)
		}
		if gotW >= spec.PreferredW {
			t.Errorf("screenW=%d: resolved width %d met the 85%% PreferredW %d — the clamp finding no longer holds",
				screenW, gotW, spec.PreferredW)
		}
	}
}

// TestThemeEditorFlooredAndGrows replaces the old TestThemeEditorPinnedFootprint:
// after issue #317 the theme editor is no longer pinned (Min == Max). Its spec is a
// pure 80×22 FLOOR (tv.DialogSpec{MinW: 80, MinH: 22}), so it collapses to 80×22 on a
// small terminal — keeping the menu-bar clearance and the #279/#291 scrolling viewport
// valid — and grows toward the shared 80%×85% cap on a larger one. This pins the new
// invariant: floor at the bottom, grow toward the cap above it, centred throughout.
func TestThemeEditorFlooredAndGrows(t *testing.T) {
	// The real spec showThemeEditor hands the resolver. A drift here (e.g. a stray
	// MaxW/MaxH reintroducing the pin) is caught by TestThemeEditorOpensFlooredAndGrows,
	// which opens the editor itself; this guards the resolver policy directly.
	spec := tv.DialogSpec{MinW: themeEditorDialogW, MinH: themeEditorDialogH}

	for _, tc := range []struct {
		name  string
		termW int
		termH int
		wantW int
		wantH int
	}{
		{"floors on an 80x24 terminal", 80, 24, 80, 22},
		{"floors on a sub-floor terminal", 70, 20, 80, 22},
		{"floors on a tiny terminal", 30, 8, 80, 22},
		{"grows toward the cap on 200x50", 200, 50, 160, 42}, // 80% of 200, 85% of 50
		{"grows on a mid terminal", 120, 40, 96, 34},         // 80% of 120, 85% of 40
		{"grows toward the cap on an ultrawide", 300, 80, 240, 68},
	} {
		t.Run(tc.name, func(t *testing.T) {
			x, y, w, h := tv.ResolveDialogRect(spec, tc.termW, tc.termH)
			if w != tc.wantW || h != tc.wantH {
				t.Errorf("at %dx%d theme editor = %dx%d, want %dx%d", tc.termW, tc.termH, w, h, tc.wantW, tc.wantH)
			}
			// Never below the documented 80×22 floor, never pinned (it must exceed the
			// floor once the terminal is roomy enough).
			if w < themeEditorDialogW || h < themeEditorDialogH {
				t.Errorf("at %dx%d size %dx%d fell below the %dx%d floor", tc.termW, tc.termH, w, h, themeEditorDialogW, themeEditorDialogH)
			}
			// Centred, with the origin floored at 0 on a terminal smaller than the editor.
			wantX, wantY := (tc.termW-w)/2, (tc.termH-h)/2
			if wantX < 0 {
				wantX = 0
			}
			if wantY < 0 {
				wantY = 0
			}
			if x != wantX || y != wantY {
				t.Errorf("at %dx%d origin = (%d,%d), want (%d,%d)", tc.termW, tc.termH, x, y, wantX, wantY)
			}
		})
	}

	// It must actually GROW, not stay pinned: a roomy terminal is strictly larger than
	// the floor in both axes.
	_, _, bigW, bigH := tv.ResolveDialogRect(spec, 200, 50)
	if bigW <= themeEditorDialogW || bigH <= themeEditorDialogH {
		t.Errorf("on 200x50 the editor resolved to %dx%d — it stayed pinned at the %dx%d floor instead of growing",
			bigW, bigH, themeEditorDialogW, themeEditorDialogH)
	}
}

// TestInlineDialogSpecFloors pins each inline DialogSpec's resolved size on a tiny
// terminal (the Min floor) and on a roomy 200×50 one. The spec literals here mirror
// each show* function; a drift between them and the source is exactly what this guards
// (and the open-the-dialog tests in dialog_issue317_test.go catch drift end-to-end).
//
// After issue #317 NONE of these four is a pure Min-floor spec any more: the two
// fixed-form dialogs (Sub-agent Settings, Notifications) are PINNED to a content
// footprint (PreferredW + MaxW + MaxH == MinH) so they never balloon to 160×42, and
// the two viewers (Review, Monologue) carry a 120-column MaxW so they grow tall but not
// ultrawide. Each row's bigW/bigH is the documented resolved size on 200×50.
//
// The command palette and help overlay are deliberately omitted: after #309 their
// real specs carry a content-keyed MaxH (item count + chrome), so they no longer
// reach the full 42-row default on a roomy terminal — see TestDialogsSizedToContent
// (list-driven, height-capped) for their current behaviour.
func TestInlineDialogSpecFloors(t *testing.T) {
	for _, tc := range []struct {
		name       string
		spec       tv.DialogSpec
		floorW, fH int
		bigW, bigH int // expected on a 200x50 terminal
	}{
		{"sub-agent settings", tv.DialogSpec{MinW: 64, MaxW: 76, PreferredW: 72, MinH: 20, MaxH: 20}, 64, 20, 72, 20},
		{"notifications", tv.DialogSpec{MinW: 50, MaxW: 58, PreferredW: 54, MinH: 18, MaxH: 18}, 50, 18, 54, 18},
		{"review", tv.DialogSpec{MinW: 40, MaxW: 120, MinH: 12}, 40, 12, 120, 42},
		{"sub-agent monologue", tv.DialogSpec{MinW: 40, MaxW: 120, MinH: 10}, 40, 10, 120, 42},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, w, h := tv.ResolveDialogRect(tc.spec, 30, 8)
			if w < tc.floorW || h < tc.fH {
				t.Errorf("tiny terminal size = %dx%d, want at least %dx%d", w, h, tc.floorW, tc.fH)
			}
			_, _, bw, bh := tv.ResolveDialogRect(tc.spec, 200, 50)
			if bw != tc.bigW || bh != tc.bigH {
				t.Errorf("roomy terminal size = %dx%d, want %dx%d", bw, bh, tc.bigW, tc.bigH)
			}
			// The two pinned forms must NOT reach the 160×42 percentage box that motivated
			// the issue; the two capped viewers must NOT span an ultrawide terminal.
			if bw >= 160 {
				t.Errorf("%s width = %d on 200x50 — it balloons toward the 160-wide percentage box (issue #317)", tc.name, bw)
			}
		})
	}
}

// TestCommandsDialogSpecSizing is the sizing assertion for the Custom Commands editor
// (issue #448), mirroring the watchers/sessions/statistics sizing tests. showCommandsDialog
// formerly sized with an inline tv.DialogSpec{MinW:84, MinH:26, PreferredW:96} — PreferredW
// with no MaxW/MaxH/PrefH — so the width pinned at 96 and the height ballooned to ~85% of
// the terminal (~42 rows on a 50-row screen). The dedicated commandsDialogSpec() is now
// content-driven; this drives the REAL method through the shared resolver and pins the
// resolved footprint across the terminals the acceptance criteria name, plus the
// footer-min-width invariant (MinW is raised to the footer's measured width so the action
// buttons never overlap). The deeper open-dialog / footer-render / geometry / resize checks
// live in commands_dialog_issue448_test.go; this is the canonical sizing-spec guard.
func TestCommandsDialogSpecSizing(t *testing.T) {
	w := newTestWorkbench(t)
	spec := w.commandsDialogSpec()

	// Content footprint, not the old PreferredW-only inline spec.
	if spec.PreferredW != 112 || spec.PrefH != 34 {
		t.Errorf("preferred = %dx%d, want 112x34", spec.PreferredW, spec.PrefH)
	}
	// The footer never forces a clamp/overlap: MinW covers the action row's real width.
	if need := footerRowMinWidth(commandsFooterLabels, tv.DefaultButtonGap); spec.MinW < need {
		t.Errorf("MinW %d < footer need %d — footer buttons could overlap at the floor", spec.MinW, need)
	}

	for _, tc := range []struct {
		name             string
		screenW, screenH int
		wantW, wantH     int
	}{
		// #448 acceptance: 200×50 → 112×28 (was the 96×42 balloon).
		{"roomy terminal is content size not the balloon", 200, 50, 112, 34},
		// #448 acceptance: 120×30 shrinks toward the 84×26 floor (height hits 26).
		{"120x30 shrinks toward the floor", 120, 30, 96, 26},
		// Width capped at the 80% default (96) below the 112 preferred; height keeps PrefH 34.
		{"120x40 width capped at 80 percent", 120, 40, 96, 34},
		// Ultrawide: PreferredW (112) is below the cap, so it does not sprawl.
		{"ultrawide stays at content size", 300, 80, 112, 34},
		// Tiny terminal: both floors win even past the screen edge.
		{"tiny terminal floors both dimensions", 40, 16, 84, 26},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, gotW, gotH := tv.ResolveDialogRect(spec, tc.screenW, tc.screenH)
			if gotW != tc.wantW || gotH != tc.wantH {
				t.Errorf("size(%d,%d) = %dx%d, want %dx%d",
					tc.screenW, tc.screenH, gotW, gotH, tc.wantW, tc.wantH)
			}
			if gotW < spec.MinW || gotH < spec.MinH {
				t.Errorf("size %dx%d fell below the %dx%d floor", gotW, gotH, spec.MinW, spec.MinH)
			}
		})
	}

	// The crux of #448: on a roomy terminal it must NOT be the 160×42 percentage balloon.
	_, _, bw, bh := tv.ResolveDialogRect(spec, 200, 50)
	if bw >= 160 || bh >= 42 {
		t.Errorf("200x50 resolved to %dx%d — the percentage balloon is back", bw, bh)
	}
}
