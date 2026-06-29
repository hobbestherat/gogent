package ui

import (
	"context"
	"testing"

	"gogent/internal/config"
	"gogent/internal/modelsdev"
	"gogent/internal/stats"

	tv "github.com/hobbestherat/turbotui/turbotv"
)

// Issue #585 — deeper acceptance tests for "Models… footer buttons revert to 1 row +
// a blank separator line above them". The sibling file models_dialog_issue529_test.go
// holds the #529-origin regression tests (flipped to assert H==1 + the basic blank
// guard). This file adds the stronger, pixel-level and multi-state coverage aimed at
// the four design criteria:
//   (1) goal — H==1 in ALL footer states (empty 2-button / 5-button / 6-button), and
//       a truly blank (all-space) separator row directly above the buttons;
//   (2) usability — the buttons sit flush on the last interior row (NOT the border),
//       the vertical order is list → hint → BLANK → buttons with no overlap, and the
//       layout holds across model counts;
//   (3) no regressions — nothing paints the blank row (exhaustive leaf check),
//       height stays paneRows+8, resize/tiny-terminal paths keep the invariant, and
//       the footerButtonRectsH primitive is retained (callable, delegates to H==1);
//   (4) holistic — peer dialogs (Statistics/Sessions/Watchers) all stay 1-row.
//
// Geometry note: listY/hintY/buttonY in showModelsDialog are CONTENT-relative (the
// dialog window's content area is Inset(1), so content row r is at absolute Y
// b.Y+1+r). Consequently buttonY=height-3 resolves to absolute b.Y+b.H-2 — the LAST
// interior row, flush against the bottom border at b.Y+b.H-1. Every assertion below
// derives from the actual resolved bounds, so it is robust to recentering.

// rowIsBlankInterior reports whether the row at absolute screen Y absY renders as
// all spaces across the dialog's interior columns (excluding the left/right border
// cells), AND is strictly inside the dialog (not on the top/bottom border row). This
// is the user-visible "empty line" check: no caption, no hint text, no list glyph.
func rowIsBlankInterior(t *testing.T, w *Workbench, absY int) bool {
	t.Helper()
	b := dialogBounds(w)
	if b == (tv.Rect{}) {
		t.Fatal("no dialog open")
	}
	if absY <= b.Y || absY >= b.Y+b.H-1 {
		return false // on or past a border row
	}
	grid := editorGrid(w)
	if absY < 0 || absY >= len(grid) {
		return false
	}
	row := grid[absY]
	for x := b.X + 1; x <= b.X+b.W-2; x++ {
		if x < 0 || x >= len(row) {
			continue
		}
		if row[x] != ' ' {
			return false
		}
	}
	return true
}

// modelsDialogListBounds returns the absolute bounds of the Models… dialog's list
// pane (the focused tv.Tree), or ok=false if no focused descendant is found.
func modelsDialogListBounds(w *Workbench) (tv.Rect, bool) {
	for _, c := range dialogDescendants(w) {
		if c.Focused() {
			return c.AbsoluteBounds(), true
		}
	}
	return tv.Rect{}, false
}

// openModelsDialogCatalog opens the Models… dialog with the catalog wired so the full
// six-button footer (Catalog/Empty/Edit/Remove/SetDefault/Done) is present.
func openModelsDialogCatalog(t *testing.T) *Workbench {
	t.Helper()
	w := newTestWorkbench(t)
	w.SetHandlers(Handlers{
		GetModels: func() []config.ModelConfig {
			return []config.ModelConfig{{Name: "x", DisplayName: "X", Model: "m", APIType: "openai"}}
		},
		GetDefaultModel: func() string { return "x" },
		UpdateModel:     func(config.ModelConfig) error { return nil },
		AddModel:        func(config.ModelConfig) error { return nil },
		RemoveModel:     func(string) error { return nil },
		SetDefaultModel: func(string) error { return nil },
		GetModelCatalog: func(context.Context, bool) (modelsdev.Catalog, error) {
			return modelsdev.Catalog{}, nil
		},
	})
	w.app.Resize(200, 50)
	w.showModelsDialog()
	if w.desktop.TopLayer() == nil {
		t.Fatal("models dialog did not open")
	}
	w.desktop.Redraw()
	return w
}

// modelsDialogFooterRowY returns the shared absolute Y of the footer buttons, or
// fails the test if there are none.
func modelsDialogFooterRowY(t *testing.T, w *Workbench) int {
	t.Helper()
	btns := footerButtons(w)
	if len(btns) == 0 {
		t.Fatal("no footer buttons")
	}
	return btns[0].AbsoluteBounds().Y
}

// TestModelsDialogIssue585AllStatesOneRowAndBlankSeparator runs every footer state
// (empty 2-button, 5-button no-catalog, 6-button catalog) and asserts, for each:
// every button is H==1; all buttons share one row; the row directly above the buttons
// is a truly blank all-space line; and the hint is present above that blank line.
// This is criterion (1) in full — the headline goal of #585 across every state.
func TestModelsDialogIssue585AllStatesOneRowAndBlankSeparator(t *testing.T) {
	states := []struct {
		name string
		open func(t *testing.T) *Workbench
		want int // expected button count
	}{
		{"empty_list", func(t *testing.T) *Workbench { return openModelsDialogCount(t, 0) }, 2},
		{"no_catalog", func(t *testing.T) *Workbench { return openModelsDialogCount(t, 3) }, 5},
		{"catalog", openModelsDialogCatalog, 6},
	}
	for _, st := range states {
		t.Run(st.name, func(t *testing.T) {
			w := st.open(t)
			btns := footerButtons(w)
			if len(btns) != st.want {
				t.Fatalf("%s: got %d footer buttons, want %d", st.name, len(btns), st.want)
			}
			buttonY := btns[0].AbsoluteBounds().Y
			for i, b := range btns {
				abs := b.AbsoluteBounds()
				if abs.H != 1 {
					t.Errorf("%s: button #%d H=%d, want 1", st.name, i, abs.H)
				}
				if abs.Y != buttonY {
					t.Errorf("%s: button #%d Y=%d, want shared row %d", st.name, i, abs.Y, buttonY)
				}
			}
			if !rowIsBlankInterior(t, w, buttonY-1) {
				t.Errorf("%s: row directly above buttons (absY %d) is not a blank line", st.name, buttonY-1)
			}
			if !modelsDialogHasText(t, w, "Esc close") {
				t.Errorf("%s: hint text missing above the blank line", st.name)
			}
		})
	}
}

// TestModelsDialogIssue585ButtonsFlushOnLastInteriorRowNotBorder pins the exact
// vertical placement: buttons land on the LAST interior row (absY b.Y+b.H-2), one row
// above the bottom border (b.Y+b.H-1). The border row must be drawn (non-blank), so
// the buttons are provably flush on the interior and NOT painted on the border.
// Guards against an off-by-one that would either put the buttons on the border or
// leave a stray blank row between the buttons and the border.
func TestModelsDialogIssue585ButtonsFlushOnLastInteriorRowNotBorder(t *testing.T) {
	for _, n := range []int{0, 1, 5, 12} {
		w := openModelsDialogCount(t, n)
		b := dialogBounds(w)
		buttonY := modelsDialogFooterRowY(t, w)

		if buttonY != b.Y+b.H-2 {
			t.Errorf("models=%d: button row absY=%d, want last interior row %d (flush above border)",
				n, buttonY, b.Y+b.H-2)
		}
		if buttonY == b.Y+b.H-1 {
			t.Errorf("models=%d: button row lands ON the bottom border row %d", n, b.Y+b.H-1)
		}

		// The bottom border row itself must contain box-drawing (non-space) cells —
		// i.e. it is the border, distinct from the blank separator above the buttons.
		grid := editorGrid(w)
		borderY := b.Y + b.H - 1
		if borderY < 0 || borderY >= len(grid) {
			t.Fatalf("models=%d: border row %d out of grid", n, borderY)
		}
		hasBorder := false
		for x := b.X; x <= b.X+b.W-1 && x < len(grid[borderY]); x++ {
			if grid[borderY][x] != ' ' {
				hasBorder = true
				break
			}
		}
		if !hasBorder {
			t.Errorf("models=%d: bottom border row %d is entirely blank (expected box border)", n, borderY)
		}
	}
}

// TestModelsDialogIssue585VerticalOrderingListHintBlankButton asserts the strict
// top-to-bottom order with no overlap: list bottom < hint row < blank row < button
// row < border row. The hint must carry text, the blank row must be all spaces, and
// the button row must carry button text — so the blank separator is genuinely
// BETWEEN the hint and the buttons (the requested "empty line above the buttons").
func TestModelsDialogIssue585VerticalOrderingListHintBlankButton(t *testing.T) {
	w := openModelsDialogCount(t, 4)
	b := dialogBounds(w)
	buttonY := modelsDialogFooterRowY(t, w)
	blankY := buttonY - 1
	hintY := buttonY - 2

	// Hint row carries hint text; blank row is empty; button row carries a button
	// caption — checked on the rendered grid.
	grid := editorGrid(w)
	rowHasText := func(y int) bool {
		if y < 0 || y >= len(grid) {
			return false
		}
		for x := b.X + 1; x <= b.X+b.W-2 && x < len(grid[y]); x++ {
			if grid[y][x] != ' ' {
				return true
			}
		}
		return false
	}
	if !rowHasText(hintY) {
		t.Errorf("hint row %d has no text", hintY)
	}
	if rowHasText(blankY) {
		t.Errorf("blank row %d should be empty but has text", blankY)
	}
	if !rowHasText(buttonY) {
		t.Errorf("button row %d has no text", buttonY)
	}

	// List pane bottom is strictly above the hint row (no overlap with hint/blank/
	// button). With 4 models paneH=4, the list occupies several rows well above.
	list, ok := modelsDialogListBounds(w)
	if !ok {
		t.Fatal("no focused list found")
	}
	if listBottom := list.Y + list.H - 1; listBottom >= hintY {
		t.Errorf("list bottom %d >= hint row %d (overlap)", listBottom, hintY)
	}
	// Ordering sanity.
	if !(list.Y+list.H-1 < hintY && hintY < blankY && blankY < buttonY && buttonY < b.Y+b.H-1) {
		t.Errorf("ordering broken: listBottom=%d hint=%d blank=%d button=%d border=%d",
			list.Y+list.H-1, hintY, blankY, buttonY, b.Y+b.H-1)
	}
}

// TestModelsDialogIssue585NothingPaintsBlankRow is the exhaustive guard for the blank
// separator: NO descendant component whose bounds cover the blank row may be a
// content-painting leaf (button / label / tree). The dialog's background fill
// container (UseBackground, spanning the whole interior) is the only component allowed
// to cover that row, and it paints spaces. This catches a regression where a future
// edit adds a label, moves the hint, or re-introduces a 2-row button onto the
// separator row.
func TestModelsDialogIssue585NothingPaintsBlankRow(t *testing.T) {
	for _, st := range []struct {
		name string
		open func(t *testing.T) *Workbench
	}{
		{"empty_list", func(t *testing.T) *Workbench { return openModelsDialogCount(t, 0) }},
		{"no_catalog", func(t *testing.T) *Workbench { return openModelsDialogCount(t, 5) }},
		{"catalog", openModelsDialogCatalog},
	} {
		t.Run(st.name, func(t *testing.T) {
			w := st.open(t)
			blankY := modelsDialogFooterRowY(t, w) - 1
			for _, c := range dialogDescendants(w) {
				if c.UseBackground {
					continue // the interior background fill — allowed (paints spaces)
				}
				abs := c.AbsoluteBounds()
				if abs.H == 0 || abs.W == 0 {
					continue
				}
				if abs.Y <= blankY && blankY <= abs.Y+abs.H-1 {
					t.Errorf("%s: component bounds %+v (drawOutside=%v) covers the blank row %d",
						st.name, abs, c.DrawOutside, blankY)
				}
			}
			// And the rendered row is truly empty.
			if !rowIsBlankInterior(t, w, blankY) {
				t.Errorf("%s: blank row %d is not all-space", st.name, blankY)
			}
		})
	}
}

// TestModelsDialogIssue585ListBottomAboveHintAcrossCounts verifies the list pane
// never grows into the hint/blank/button rows across the full paneRows clamp range
// (including the 12-row cap and over-cap), and the dialog height stays paneRows+8.
// This is the "no overlap with list" half of criterion (2)/(3) under varying content.
func TestModelsDialogIssue585ListBottomAboveHintAcrossCounts(t *testing.T) {
	for _, tc := range []struct {
		models   int
		paneRows int
	}{
		{0, 1}, {1, 1}, {5, 5}, {12, 12}, {20, 12},
	} {
		w := openModelsDialogCount(t, tc.models)
		b := dialogBounds(w)

		if got := b.H; got != tc.paneRows+8 {
			t.Errorf("models=%d: height=%d, want %d", tc.models, got, tc.paneRows+8)
		}
		buttonY := modelsDialogFooterRowY(t, w)
		blankY := buttonY - 1
		hintY := buttonY - 2

		list, ok := modelsDialogListBounds(w)
		if !ok {
			t.Fatalf("models=%d: no focused list", tc.models)
		}
		listBottom := list.Y + list.H - 1
		if listBottom >= hintY {
			t.Errorf("models=%d: list bottom %d >= hint row %d", tc.models, listBottom, hintY)
		}
		if !(listBottom < hintY && hintY < blankY && blankY < buttonY) {
			t.Errorf("models=%d: ordering broken listBot=%d hint=%d blank=%d btn=%d",
				tc.models, listBottom, hintY, blankY, buttonY)
		}
	}
}

// TestModelsDialogIssue585ResizeKeepsBlankAndOneRow exercises the reflow path with
// the full 6-button (catalog) footer too, checking after every resize that the
// buttons stay H==1, stay inside the interior, the blank separator stays blank, and
// the height stays pinned — across both shrinking and growing terminals.
func TestModelsDialogIssue585ResizeKeepsBlankAndOneRow(t *testing.T) {
	w := openModelsDialogCatalog(t)
	pinnedH := dialogBounds(w).H

	for _, size := range []struct{ W, H int }{{120, 40}, {250, 60}, {80, 30}, {200, 50}} {
		w.app.Resize(size.W, size.H)
		w.desktop.Redraw()
		b := dialogBounds(w)
		if b.H != pinnedH {
			t.Errorf("resize %dx%d: height=%d, want pinned %d", size.W, size.H, b.H, pinnedH)
		}
		buttonY := modelsDialogFooterRowY(t, w)
		for i, btn := range footerButtons(w) {
			abs := btn.AbsoluteBounds()
			if abs.H != 1 {
				t.Errorf("resize %dx%d: button #%d H=%d, want 1", size.W, size.H, i, abs.H)
			}
			if abs.Y != buttonY || abs.Y < b.Y+1 || abs.Y > b.Y+b.H-2 {
				t.Errorf("resize %dx%d: button #%d abs=%+v not on shared interior row", size.W, size.H, i, abs)
			}
		}
		if !rowIsBlankInterior(t, w, buttonY-1) {
			t.Errorf("resize %dx%d: blank separator above buttons is not blank", size.W, size.H)
		}
	}
}

// TestModelsDialogIssue585TinyTerminalKeepsBlankAndOneRow opens the dialog directly
// on tiny terminals (the MinH floor path, including one shorter than the pinned
// height so the dialog extends past the screen edge) and asserts no panic, buttons
// H==1 inside the dialog's own interior, height pinned to 9, and nothing painted on
// the blank separator row. When the terminal is tall enough for the blank row to be
// on-screen it is additionally pixel-verified as all-space; below that (e.g. 40x6,
// where the height-9 dialog's separator row is off-screen) only the dialog-relative
// checks apply, since a row beyond the screen cannot be pixel-read.
func TestModelsDialogIssue585TinyTerminalKeepsBlankAndOneRow(t *testing.T) {
	for _, term := range []struct{ W, H int }{{60, 12}, {48, 9}, {48, 7}, {40, 6}} {
		w := newTestWorkbench(t)
		w.SetHandlers(Handlers{
			GetModels: func() []config.ModelConfig {
				return []config.ModelConfig{{Name: "x", DisplayName: "X", Model: "m", APIType: "openai"}}
			},
			GetDefaultModel: func() string { return "x" },
			UpdateModel:     func(config.ModelConfig) error { return nil },
			AddModel:        func(config.ModelConfig) error { return nil },
			RemoveModel:     func(string) error { return nil },
			SetDefaultModel: func(string) error { return nil },
		})
		w.app.Resize(term.W, term.H)
		w.showModelsDialog() // must not panic
		w.desktop.Redraw()

		b := dialogBounds(w)
		if b.H != 9 {
			t.Errorf("term %dx%d: height=%d, want pinned 9", term.W, term.H, b.H)
		}
		buttonY := modelsDialogFooterRowY(t, w)
		for i, btn := range footerButtons(w) {
			abs := btn.AbsoluteBounds()
			if abs.H != 1 {
				t.Errorf("term %dx%d: button #%d H=%d, want 1", term.W, term.H, i, abs.H)
			}
			// Dialog-relative interior: the button lies inside the dialog's own
			// interior even when that interior extends past the tiny screen.
			if abs.Y < b.Y+1 || abs.Y > b.Y+b.H-2 {
				t.Errorf("term %dx%d: button #%d abs=%+v outside dialog interior", term.W, term.H, i, abs)
			}
		}
		// No content-painting leaf covers the separator row (dialog-relative, always
		// valid — true even when the row is off-screen).
		blankY := buttonY - 1
		for _, c := range dialogDescendants(w) {
			if c.UseBackground {
				continue
			}
			abs := c.AbsoluteBounds()
			if abs.H == 0 || abs.W == 0 {
				continue
			}
			if abs.Y <= blankY && blankY <= abs.Y+abs.H-1 {
				t.Errorf("term %dx%d: component %+v covers blank row %d", term.W, term.H, abs, blankY)
			}
		}
		// Pixel-verify the blank row only when it is actually on-screen.
		if blankY >= 0 && blankY < term.H {
			if !rowIsBlankInterior(t, w, blankY) {
				t.Errorf("term %dx%d: on-screen blank row %d is not all-space", term.W, term.H, blankY)
			}
		}
	}
}

// TestModelsDialogIssue585PeerDialogsAllOneRow is the holistic guard across peer
// dialogs: Statistics, Sessions and Watchers all use the 1-row footerButtonRects
// path, so every one of their footer buttons must render H==1. It would catch a leak
// if any peer dialog (or a shared helper) were switched to footerButtonRectsH. The
// Models… dialog rejoined this 1-row contract in #585.
func TestModelsDialogIssue585PeerDialogsAllOneRow(t *testing.T) {
	opens := []struct {
		name string
		open func(t *testing.T, w *Workbench)
		min  int // minimum expected footer button count
	}{
		{"statistics", func(t *testing.T, w *Workbench) {
			w.handlers.GetStatistics = func() stats.Report { return sampleStatsReport() }
			w.showStatisticsDialog()
		}, 3},
		{"sessions", func(t *testing.T, w *Workbench) {
			w.showSessionsDialog()
		}, 1},
		{"watchers", func(t *testing.T, w *Workbench) {
			w.showWatchersDialog()
		}, 1},
	}
	for _, d := range opens {
		t.Run(d.name, func(t *testing.T) {
			w := newTestWorkbench(t)
			w.app.Resize(200, 50)
			d.open(t, w)
			w.desktop.Redraw()
			if w.desktop.TopLayer() == nil {
				t.Fatalf("%s dialog did not open", d.name)
			}
			btns := footerButtons(w)
			if len(btns) < d.min {
				t.Fatalf("%s: got %d footer buttons, want >= %d", d.name, len(btns), d.min)
			}
			for i, b := range btns {
				if h := b.AbsoluteBounds().H; h != 1 {
					t.Errorf("%s footer button #%d H=%d, want 1 (every dialog footer is 1-row)", d.name, i, h)
				}
			}
		})
	}
}

// TestModelsDialogIssue585FooterButtonRectsHRetained confirms the #529 primitive is
// still present and correct after #585 reverted its only dialog call site: it must
// still produce rects of the requested height, and the 1-row footerButtonRects
// wrapper must delegate to it with H==1. This guards the "DO NOT remove
// footerButtonRectsH" requirement and the unit-tested-primitive invariant.
func TestModelsDialogIssue585FooterButtonRectsHRetained(t *testing.T) {
	labels := []string{"&One", "T&wo", "Three"}
	leftX, rightX, y, gap := 2, 60, 5, tv.DefaultButtonGap

	// The H variant honours an explicit height.
	tall := footerButtonRectsH(labels, leftX, rightX, y, gap, 2)
	if len(tall) != len(labels) {
		t.Fatalf("footerButtonRectsH returned %d rects, want %d", len(tall), len(labels))
	}
	for i, r := range tall {
		if r.H != 2 {
			t.Errorf("tall rect #%d H=%d, want 2 (primitive must still honour explicit height)", i, r.H)
		}
	}

	// The 1-row wrapper delegates to footerButtonRectsH with H==1.
	flat := footerButtonRects(labels, leftX, rightX, y, gap)
	if len(flat) != len(labels) {
		t.Fatalf("footerButtonRects returned %d rects, want %d", len(flat), len(labels))
	}
	for i, r := range flat {
		if r.H != 1 {
			t.Errorf("flat rect #%d H=%d, want 1 (footerButtonRects must delegate to H==1)", i, r.H)
		}
		// Horizontal layout is identical between the two variants at the same row.
		if r.X != tall[i].X || r.W != tall[i].W || r.Y != tall[i].Y {
			t.Errorf("rect #%d flat=%+v != tall=%+v (horizontal layout must match)", i, r, tall[i])
		}
	}
}

// TestModelsDialogIssue585FooterButtonRectsHDegeneratesToOne pins the h<=0 contract
// (a zero/negative height collapses to 1, mirroring turbotui's ButtonHeight), so the
// primitive can never produce an invisible button. Edge-case for criterion (3).
func TestModelsDialogIssue585FooterButtonRectsHDegeneratesToOne(t *testing.T) {
	labels := []string{"&OK", "&Cancel"}
	for _, h := range []int{0, -1, -5} {
		rects := footerButtonRectsH(labels, 2, 40, 1, tv.DefaultButtonGap, h)
		for i, r := range rects {
			if r.H != 1 {
				t.Errorf("h=%d: rect #%d H=%d, want 1 (non-positive height must collapse to 1)", h, i, r.H)
			}
		}
	}
}
