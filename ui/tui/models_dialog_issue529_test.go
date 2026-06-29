package ui

import (
	"context"
	"fmt"
	"testing"

	"gogent/internal/config"
	"gogent/internal/modelsdev"
	"gogent/internal/stats"

	tv "github.com/hobbestherat/turbotui/turbotv"
)

// Issue #585 (reverting gogent #529): the Models… footer buttons are back to a
// 1-row-tall strip with a blank separator row directly above them. These drive the
// REAL showModelsDialog and inspect the constructed button components' absolute
// bounds (the same bounds turbotui's Button.draw fills every row of), the resolved
// dialog height, the footer geometry, the empty-list state, the blank separator and
// the resize/reflow path — mapping onto the four design criteria:
//   (1) goal — every footer button renders H==1 in all states (empty/5/6-button);
//       a blank row sits directly above the button row; dialog height stays
//       paneRows+8 (the 1-row footer + blank separator occupy the same two rows the
//       old 2-row footer used);
//   (2) usability — the blank line visually separates the single-row action strip
//       from the list/hint above, matching the 1-row-footer dialogs; empty-list +
//       resize paths stay correct;
//   (3) no regressions — the footer never clashes with the border or overlaps the
//       list, and other dialogs keep 1-row footers;
//   (4) the change is gogent-only geometry: footerButtonRects (H==1) replaces
//       footerButtonRectsH(...,2); turbotui renders whatever Rect.H it is handed.
//
// ui/tui stays free of internal/daemon|server imports (Handlers stubs only), as the
// dialog's tests require.

// footerButtons returns the top dialog's action buttons: the DrawOutside,
// click-handling leaf components (turbotui Buttons). The Models… dialog's list and
// hint are not DrawOutside and it has no title close button, so these are exactly
// the footer action buttons. Order matches the order they were added to the dialog.
func footerButtons(w *Workbench) []*tv.VisualComponent {
	var out []*tv.VisualComponent
	for _, c := range dialogDescendants(w) {
		if c.DrawOutside && c.OnClickFn != nil {
			out = append(out, c)
		}
	}
	return out
}

// openModelsDialogCount drives the real showModelsDialog on a 200×50 terminal with
// n short model rows and all CRUD handlers wired (no catalog), then redraws so the
// component bounds are current. It returns the workbench for inspection. n=0 yields
// the empty-list state (only Add Empty + Done).
func openModelsDialogCount(t *testing.T, n int) *Workbench {
	t.Helper()
	models := make([]config.ModelConfig, n)
	for i := range models {
		models[i] = config.ModelConfig{
			Name: fmt.Sprintf("m%d", i), DisplayName: fmt.Sprintf("M%d", i),
			Model: "x", APIType: "openai",
		}
	}
	def := ""
	if n > 0 {
		def = models[0].Name
	}
	w := newTestWorkbench(t)
	w.SetHandlers(Handlers{
		GetModels:       func() []config.ModelConfig { return models },
		GetDefaultModel: func() string { return def },
		UpdateModel:     func(config.ModelConfig) error { return nil },
		AddModel:        func(config.ModelConfig) error { return nil },
		RemoveModel:     func(string) error { return nil },
		SetDefaultModel: func(string) error { return nil },
	})
	w.app.Resize(200, 50)
	w.showModelsDialog()
	if w.desktop.TopLayer() == nil {
		t.Fatalf("models dialog did not open for n=%d", n)
	}
	w.desktop.Redraw()
	return w
}

// TestModelsDialogFooterButtonsAreOneRowTall is the issue #585 headline acceptance
// test: the Models… footer action buttons render exactly 1 row tall via turbotui's
// Button.draw (reverting #529's 2-row footer). Without a catalog wired the footer is
// Add Empty/Edit/Remove/Set Default/Done (5 buttons); each constructed button's
// bounds must carry H==1.
func TestModelsDialogFooterButtonsAreOneRowTall(t *testing.T) {
	w := openModelsDialogCount(t, 3)
	btns := footerButtons(w)
	if len(btns) != 5 {
		t.Fatalf("got %d footer buttons, want 5 (Empty/Edit/Remove/SetDefault/Done without catalog)", len(btns))
	}
	for i, b := range btns {
		if h := b.AbsoluteBounds().H; h != 1 {
			t.Errorf("footer button #%d bounds = %+v, want H==1 (1-row footer, issue #585)", i, b.AbsoluteBounds())
		}
	}
}

// TestModelsDialogAllSixFooterButtonsAreOneRowTall wires the catalog too, so the full
// six-button footer the issue names (Add from Catalog…/Add Empty…/Edit…/Remove/Set
// Default/Done) is present — and every one of the six is 1 row tall.
func TestModelsDialogAllSixFooterButtonsAreOneRowTall(t *testing.T) {
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
	w.desktop.Redraw()

	btns := footerButtons(w)
	if len(btns) != 6 {
		t.Fatalf("got %d footer buttons, want 6 (Catalog/Empty/Edit/Remove/SetDefault/Done)", len(btns))
	}
	for i, b := range btns {
		if h := b.AbsoluteBounds().H; h != 1 {
			t.Errorf("footer button #%d H = %d, want 1", i, h)
		}
	}
}

// TestModelsDialogHeightIsPaneRowsPlusEight pins the dialog height: height0 ==
// paneRows + 8, where paneRows is the model count clamped to [1, 12]. The spec pins
// height (MinH==MaxH==height0), so on a roomy terminal the dialog tracks the list
// size exactly — never the 85% balloon. The height is UNCHANGED by #585: the 1-row
// footer loses one row versus #529's 2-row footer, but the new blank separator adds
// one row back, so the two cancel and the total stays paneRows+8.
func TestModelsDialogHeightIsPaneRowsPlusEight(t *testing.T) {
	for _, tc := range []struct {
		models   int
		paneRows int // clamped paneRows the dialog uses
	}{
		{0, 1}, // empty-list clamps paneRows up to 1
		{1, 1},
		{3, 3},
		{12, 12}, // at the cap
		{20, 12}, // over the cap -> still 12
	} {
		w := openModelsDialogCount(t, tc.models)
		got := dialogBounds(w).H
		want := tc.paneRows + 8
		if got != want {
			t.Errorf("models=%d: dialog height = %d, want %d (paneRows=%d + 8)", tc.models, got, want, tc.paneRows)
		}
	}
}

// TestModelsDialogFooterGeometryNoBorderClashNoOverlap is the structural guard for
// the 1-row footer's geometry: every footer button's single row must lie fully INSIDE
// the dialog interior — so the button (and its drop shadow) never lands on the top or
// bottom border row — all buttons share one footer row, and the list pane ends above
// the footer (no list/button overlap). This is the no-clash / no-cramping requirement
// behind criterion (2)/(3), checked on the actual resolved bounds rather than a
// hand-traced table.
func TestModelsDialogFooterGeometryNoBorderClashNoOverlap(t *testing.T) {
	w := openModelsDialogCount(t, 5)
	b := dialogBounds(w)
	topInterior := b.Y + 1
	lastInterior := b.Y + b.H - 2 // row before the bottom border

	btns := footerButtons(w)
	if len(btns) == 0 {
		t.Fatal("no footer buttons found")
	}
	buttonY := btns[0].AbsoluteBounds().Y
	for i, btn := range btns {
		abs := btn.AbsoluteBounds()
		if abs.H != 1 {
			t.Errorf("button #%d H=%d, want 1", i, abs.H)
		}
		if abs.Y != buttonY {
			t.Errorf("button #%d Y=%d differs from footer row %d (footer must share one row)", i, abs.Y, buttonY)
		}
		if abs.Y < topInterior || abs.Y+abs.H-1 > lastInterior {
			t.Errorf("button #%d rows %d..%d fall outside interior %d..%d (border clash)",
				i, abs.Y, abs.Y+abs.H-1, topInterior, lastInterior)
		}
	}
	// The button row is flush on the LAST interior row (height-3 in code / absY
	// b.Y+b.H-2), directly above the bottom border — never on the border itself.
	if buttonY != lastInterior {
		t.Errorf("button row Y=%d, want last interior row %d (buttons must sit flush on the last interior row)",
			buttonY, lastInterior)
	}
	// The list is focused (showModelsDialog calls SetFocus(list)); its bottom must
	// stay above the footer's row so the list never overlaps the buttons — and above
	// the blank separator / hint rows beneath it.
	for _, c := range dialogDescendants(w) {
		if !c.Focused() {
			continue
		}
		listAbs := c.AbsoluteBounds()
		if listBottom := listAbs.Y + listAbs.H - 1; listBottom >= buttonY {
			t.Errorf("list bottom row %d >= footer row %d (list/button overlap)", listBottom, buttonY)
		}
		break
	}
}

// TestModelsDialogEmptyListOneRowButtons locks the empty-list state: with zero
// models only Add Empty… + Done are present (Edit/Remove/Set Default absent), yet
// both render 1 row tall, the placeholder still shows, the dialog sizes to
// paneRows(1)+8 = 9, and the blank separator above the buttons is present.
func TestModelsDialogEmptyListOneRowButtons(t *testing.T) {
	w := openModelsDialogCount(t, 0)

	btns := footerButtons(w)
	if len(btns) != 2 {
		t.Fatalf("empty-list state: got %d footer buttons, want 2 (Add Empty + Done)", len(btns))
	}
	for i, b := range btns {
		if h := b.AbsoluteBounds().H; h != 1 {
			t.Errorf("empty-list footer button #%d H=%d, want 1 (1-row footer applies to the empty state too)", i, h)
		}
	}
	if got := dialogBounds(w).H; got != 9 {
		t.Errorf("empty-list dialog height = %d, want 9 (paneRows 1 + 8)", got)
	}
	if !modelsDialogHasText(t, w, "No models configured") {
		t.Error("empty-list placeholder missing")
	}
	for _, absent := range []string{"Edit", "Remove", "Set Default", "Catalog"} {
		if modelsDialogHasText(t, w, absent) {
			t.Errorf("empty-list state should not surface a %q affordance", absent)
		}
	}
	blankRowAbsY := btns[0].AbsoluteBounds().Y - 1
	if !rowIsBlankInterior(t, w, blankRowAbsY) {
		t.Errorf("empty-list blank separator row (absY %d) is not blank", blankRowAbsY)
	}
}

// TestModelsDialogOneRowButtonsSurviveResize verifies the reflow path (dialog.Fit's
// OnResize hook) keeps the 1-row footer correct across a terminal resize: the buttons
// stay H==1, stay inside the dialog interior, the height stays content-pinned at
// paneRows+8, the blank separator stays blank, and the dialog re-centers on the final
// terminal.
func TestModelsDialogOneRowButtonsSurviveResize(t *testing.T) {
	w := openModelsDialogCount(t, 4)
	pinnedH := dialogBounds(w).H

	for _, size := range []struct{ W, H int }{{120, 40}, {250, 60}, {200, 50}} {
		w.app.Resize(size.W, size.H)
		w.desktop.Redraw()
		b := dialogBounds(w)
		if b.H != pinnedH {
			t.Errorf("after resize to %dx%d: height = %d, want pinned %d (content height must not drift)",
				size.W, size.H, b.H, pinnedH)
		}
		btns := footerButtons(w)
		buttonY := btns[0].AbsoluteBounds().Y
		for i, btn := range btns {
			abs := btn.AbsoluteBounds()
			if abs.H != 1 {
				t.Errorf("after resize to %dx%d: button #%d H=%d, want 1", size.W, size.H, i, abs.H)
			}
			if abs.Y < b.Y+1 || abs.Y+abs.H-1 > b.Y+b.H-2 {
				t.Errorf("after resize to %dx%d: button #%d rows %d..%d outside interior %d..%d",
					size.W, size.H, i, abs.Y, abs.Y+abs.H-1, b.Y+1, b.Y+b.H-2)
			}
		}
		if !rowIsBlankInterior(t, w, buttonY-1) {
			t.Errorf("after resize to %dx%d: blank separator row %d is not blank", size.W, size.H, buttonY-1)
		}
	}

	// Reflow re-centered the dialog on the final 200×50 terminal.
	b := dialogBounds(w)
	if wantX, wantY := (200-b.W)/2, (50-b.H)/2; b.X != wantX || b.Y != wantY {
		t.Errorf("dialog not re-centered after resize: origin (%d,%d), want (%d,%d)", b.X, b.Y, wantX, wantY)
	}
}

// TestPeerDialogsKeepOneRowFooter is the holistic regression guard: every dialog
// footer uses the 1-row footerButtonRects path (the Models… dialog reverted to it in
// #585), so a peer dialog that uses the default path (here the Statistics dialog)
// must render every footer button exactly 1 row tall. It catches a leak if any peer
// dialog were switched to footerButtonRectsH. (The unit-level guard is
// TestFooterButtonRectsDefaultIsOneRowTall.)
func TestPeerDialogsKeepOneRowFooter(t *testing.T) {
	w := newTestWorkbench(t)
	w.handlers.GetStatistics = func() stats.Report { return sampleStatsReport() }
	w.app.Resize(200, 50)
	w.showStatisticsDialog()
	w.desktop.Redraw()
	if w.desktop.TopLayer() == nil {
		t.Fatal("statistics dialog did not open")
	}
	btns := footerButtons(w)
	if len(btns) < 3 {
		t.Fatalf("got %d footer buttons in the statistics dialog, want at least 3 (Export CSV/JSON/Close)", len(btns))
	}
	for i, b := range btns {
		if h := b.AbsoluteBounds().H; h != 1 {
			t.Errorf("statistics footer button #%d H=%d, want 1 (every dialog footer is 1-row)", i, h)
		}
	}
}

// TestModelsDialogOneRowFooterOnTinyTerminal opens the dialog AT a small terminal
// (the resolver's MinH-clamp path), including one shorter than height0 so the floor
// wins past the screen edge. The 1-row footer must still lay out without panic, the
// buttons must stay H==1 and within the dialog's own interior, the list must not
// overlap them, and the blank separator must remain — proving the geometry formulas
// hold at the floor, not just on a roomy terminal.
func TestModelsDialogOneRowFooterOnTinyTerminal(t *testing.T) {
	for _, term := range []struct{ W, H int }{{60, 12}, {48, 9}, {48, 7}} {
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
		w.showModelsDialog() // must not panic at the floor
		w.desktop.Redraw()

		b := dialogBounds(w)
		// Height is pinned at paneRows(1)+8 = 9 by MinH==MaxH; the floor wins even
		// past the screen edge, so height must be 9 regardless of the tiny terminal.
		if b.H != 9 {
			t.Errorf("term %dx%d: dialog height = %d, want pinned 9 (MinH floor)", term.W, term.H, b.H)
		}
		btns := footerButtons(w)
		if len(btns) == 0 {
			t.Errorf("term %dx%d: no footer buttons", term.W, term.H)
			continue
		}
		buttonY := btns[0].AbsoluteBounds().Y
		for i, btn := range btns {
			abs := btn.AbsoluteBounds()
			if abs.H != 1 {
				t.Errorf("term %dx%d: button #%d H=%d, want 1", term.W, term.H, i, abs.H)
			}
			if abs.Y < b.Y+1 || abs.Y+abs.H-1 > b.Y+b.H-2 {
				t.Errorf("term %dx%d: button #%d rows %d..%d outside interior %d..%d",
					term.W, term.H, i, abs.Y, abs.Y+abs.H-1, b.Y+1, b.Y+b.H-2)
			}
		}
		if !rowIsBlankInterior(t, w, buttonY-1) {
			t.Errorf("term %dx%d: blank separator row %d is not blank", term.W, term.H, buttonY-1)
		}
	}
}

// TestModelsDialogBlankSeparatorRowAboveButtons is the #585 acceptance criterion: the
// row directly above the button row is BLANK — no footer button, no hint label, and
// no list content sits on it — and it is strictly inside the dialog interior. It
// guards the requested "empty line above the buttons" against regressions (e.g. a
// future edit re-introducing a 2-row footer, moving the hint down, or painting a
// label on that row). buttonY and height are derived from the actual resolved bounds
// so the test is robust to recentering.
func TestModelsDialogBlankSeparatorRowAboveButtons(t *testing.T) {
	w := openModelsDialogCount(t, 3)
	b := dialogBounds(w)

	btns := footerButtons(w)
	if len(btns) == 0 {
		t.Fatal("no footer buttons")
	}
	buttonY := btns[0].AbsoluteBounds().Y
	blankY := buttonY - 1 // the row immediately above the button strip

	// The blank row must be strictly inside the dialog interior (not on a border).
	if blankY <= b.Y || blankY >= b.Y+b.H-1 {
		t.Fatalf("blank row absY %d outside dialog interior (%d..%d)", blankY, b.Y+1, b.Y+b.H-2)
	}

	// (a) No footer button occupies the blank row — every button is on buttonY.
	for i, btn := range btns {
		if abs := btn.AbsoluteBounds(); abs.Y <= blankY && blankY <= abs.Y+abs.H-1 {
			t.Errorf("button #%d bounds %+v covers the blank row %d", i, abs, blankY)
		}
	}

	// (b) No list content reaches the blank row — the focused list pane's bottom
	// must end strictly above it.
	for _, c := range dialogDescendants(w) {
		if !c.Focused() {
			continue
		}
		listAbs := c.AbsoluteBounds()
		if listBottom := listAbs.Y + listAbs.H - 1; listBottom >= blankY {
			t.Errorf("list bottom row %d reaches/overlaps blank row %d", listBottom, blankY)
		}
		break
	}

	// (c) The blank row renders as dialog-interior background: every interior cell on
	// that row is a space (no caption, no hint text, no list glyph). This is the
	// user-visible "empty line above the buttons".
	grid := editorGrid(w)
	for x := b.X + 1; x <= b.X+b.W-2; x++ {
		if y := blankY; y >= 0 && y < len(grid) && x >= 0 && x < len(grid[y]) {
			if ch := grid[y][x]; ch != ' ' {
				t.Errorf("blank row %d col %d = %q, want space (row must be empty)", blankY, x, string(ch))
			}
		}
	}

	// (d) The hint sits two rows above the buttons (above the blank), so the blank is
	// genuinely BETWEEN the hint and the button strip.
	if !modelsDialogHasText(t, w, "Esc close") {
		t.Fatal("hint text not found in dialog")
	}
	if buttonY-2 < b.Y+1 {
		t.Errorf("expected a hint row two above the buttons; buttonY=%d", buttonY)
	}
}
