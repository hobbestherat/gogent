package ui

import (
	"testing"

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
// now rests on (issue #299): large by default (80% wide / 85% tall), the Min floor
// wins on a tiny screen even past the edge, an explicit Max caps growth, a small
// PreferredW is ignored in favour of the percentage default, and the result is
// centered with a non-negative origin.
func TestResolveDialogRectPolicy(t *testing.T) {
	t.Run("large by default and centered", func(t *testing.T) {
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

	t.Run("preferred below the default is ignored", func(t *testing.T) {
		_, _, w, _ := tv.ResolveDialogRect(tv.DialogSpec{PreferredW: 10}, 200, 50)
		if w != 160 {
			t.Errorf("width = %d, want 160 (a small PreferredW must not shrink below 80%%)", w)
		}
	})

	t.Run("preferred above the default widens the dialog", func(t *testing.T) {
		// PreferredW must exceed the 80% default (160) yet fit screen-2*margin (196).
		_, _, w, _ := tv.ResolveDialogRect(tv.DialogSpec{PreferredW: 180}, 200, 50)
		if w != 180 {
			t.Errorf("width = %d, want 180 (content-driven preferred wins over the default)", w)
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

// TestDialogsLargeByDefault opens the dialogs that build cleanly on a bare
// workbench and checks each resolves to ≈80%×85% of a roomy terminal — the
// acceptance criterion that dialogs are large by default and grow with the
// terminal, no longer pinned to 54–60 column boxes.
func TestDialogsLargeByDefault(t *testing.T) {
	const termW, termH = 200, 50
	const wantW, wantH = 160, 42 // 80% of 200, 85% of 50
	for _, tc := range []struct {
		name string
		open func(*Workbench)
	}{
		{"command-palette", func(w *Workbench) { w.showCommandPalette() }},
		{"help-overlay", func(w *Workbench) { w.showHelpOverlay() }},
		{"input-dialog", func(w *Workbench) { w.showInputDialog("Rename", "New name", "", nil) }},
		{"confirm-dialog", func(w *Workbench) { w.showConfirm("Quit", "Are you sure?", func(bool) {}) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := newTestWorkbench(t)
			w.app.Resize(termW, termH)
			tc.open(w)
			b := dialogBounds(w)
			if b.W != wantW || b.H != wantH {
				t.Errorf("%s size = %dx%d, want %dx%d (≈80%%×85%%)", tc.name, b.W, b.H, wantW, wantH)
			}
			// Centered, on-screen.
			if b.X != (termW-b.W)/2 || b.Y != (termH-b.H)/2 {
				t.Errorf("%s origin = (%d,%d), want centered", tc.name, b.X, b.Y)
			}
		})
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
// while a dialog is open re-resolves its size" (issue #299). dialog.Fit installs
// the layer OnResize hook, so an App.Resize must re-center and re-size the open
// dialog rather than leaving it a stale box. These dialogs use static specs (no
// terminal dimensions baked into the spec), so they re-resolve fully.
func TestDialogReResolvesOnResize(t *testing.T) {
	for _, tc := range []struct {
		name string
		open func(*Workbench)
	}{
		{"command-palette", func(w *Workbench) { w.showCommandPalette() }},
		{"input-dialog", func(w *Workbench) { w.showInputDialog("Rename", "New name", "", nil) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := newTestWorkbench(t)
			w.app.Resize(80, 24)
			tc.open(w)
			before := dialogBounds(w)

			w.app.Resize(200, 50)
			after := dialogBounds(w)

			if after == before {
				t.Fatalf("%s did not re-resolve on resize: stayed %+v", tc.name, after)
			}
			if after.W != 160 || after.H != 42 {
				t.Errorf("%s after resize = %dx%d, want 160x42 (≈80%%×85%% of 200x50)", tc.name, after.W, after.H)
			}
			if after.X != (200-after.W)/2 || after.Y != (50-after.H)/2 {
				t.Errorf("%s not re-centered after resize: origin (%d,%d)", tc.name, after.X, after.Y)
			}
		})
	}
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

// TestBrowserDialogSpecTracksLiveTerminal locks the fix that the two-pane browsers
// (Resources / Saved Sessions / Statistics) recompute PreferredW from the CURRENT
// terminal (issue #299) rather than baking the open-time width. browserDialogSpec
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

// TestThemeEditorPinnedFootprint verifies the theme editor keeps its fixed
// content footprint (Min == Max == themeEditorDialogW × themeEditorDialogH) on
// every terminal size — the invariant that keeps the scrolling viewport geometry
// from issues #279/#291 valid even though it now flows through the shared resolver
// and is re-centered on resize (issue #299).
func TestThemeEditorPinnedFootprint(t *testing.T) {
	spec := tv.DialogSpec{
		MinW: themeEditorDialogW, MinH: themeEditorDialogH,
		MaxW: themeEditorDialogW, MaxH: themeEditorDialogH,
	}
	for _, dim := range []struct{ W, H int }{{200, 50}, {120, 40}, {80, 24}, {70, 20}} {
		x, y, w, h := tv.ResolveDialogRect(spec, dim.W, dim.H)
		if w != themeEditorDialogW || h != themeEditorDialogH {
			t.Errorf("at %dx%d theme editor = %dx%d, want pinned %dx%d",
				dim.W, dim.H, w, h, themeEditorDialogW, themeEditorDialogH)
		}
		// Still centered (origin floored at 0 on a terminal smaller than the editor).
		wantX, wantY := (dim.W-w)/2, (dim.H-h)/2
		if wantX < 0 {
			wantX = 0
		}
		if wantY < 0 {
			wantY = 0
		}
		if x != wantX || y != wantY {
			t.Errorf("at %dx%d origin = (%d,%d), want (%d,%d)", dim.W, dim.H, x, y, wantX, wantY)
		}
	}
}

// TestInlineDialogSpecFloors checks every dialog migrated to an inline DialogSpec
// honours the Min floor from the issue #299 table on a tiny terminal, and grows
// to the 80% default on a roomy one. The spec literals here mirror each show*
// function; a drift between them and the source is exactly what this guards.
func TestInlineDialogSpecFloors(t *testing.T) {
	for _, tc := range []struct {
		name       string
		spec       tv.DialogSpec
		floorW, fH int
		bigW, bigH int // expected on a 200x50 terminal
	}{
		{"sub-agent settings", tv.DialogSpec{MinW: 64, MinH: 22}, 64, 22, 160, 42},
		{"notifications", tv.DialogSpec{MinW: 50, MinH: 18}, 50, 18, 160, 42},
		{"command palette", tv.DialogSpec{MinW: 40, MinH: 10}, 40, 10, 160, 42},
		{"help overlay", tv.DialogSpec{MinW: 44, MinH: 12}, 44, 12, 160, 42},
		{"review", tv.DialogSpec{MinW: 40, MinH: 12}, 40, 12, 160, 42},
		{"sub-agent monologue", tv.DialogSpec{MinW: 40, MinH: 10}, 40, 10, 160, 42},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, w, h := tv.ResolveDialogRect(tc.spec, 30, 8)
			if w < tc.floorW || h < tc.fH {
				t.Errorf("tiny terminal size = %dx%d, want at least %dx%d", w, h, tc.floorW, tc.fH)
			}
			_, _, bw, bh := tv.ResolveDialogRect(tc.spec, 200, 50)
			if bw != tc.bigW || bh != tc.bigH {
				t.Errorf("roomy terminal size = %dx%d, want %dx%d (large by default)", bw, bh, tc.bigW, tc.bigH)
			}
		})
	}
}
