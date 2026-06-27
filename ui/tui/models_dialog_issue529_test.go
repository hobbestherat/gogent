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

// Issue #529 (gogent half) — acceptance tests for the 2-row-tall Models… footer.
// These drive the REAL showModelsDialog and inspect the constructed button
// components' absolute bounds (the same bounds turbotui's Button.draw fills every
// row of), the resolved dialog height, the footer geometry, the empty-list state,
// and the resize/reflow path — mapping onto the four design criteria:
//   (1) goal — six footer buttons render H==2; dialog grows to paneRows+8;
//   (2) usability — empty-list + resize paths stay correct with the taller footer;
//   (3) no regressions — the footer never clashes with the border or overlaps the
//       list, and other dialogs keep 1-row footers;
//   (4) the change consumes turbotui's tall Button.draw via the Rect.H seam only.
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

// TestModelsDialogFooterButtonsAreTwoRowsTall is the issue #529 headline acceptance
// test: the Models… footer action buttons render 2 rows tall via turbotui's tall
// Button.draw. Without a catalog wired the footer is Add Empty/Edit/Remove/Set
// Default/Done (5 buttons); each constructed button's bounds must carry H==2.
func TestModelsDialogFooterButtonsAreTwoRowsTall(t *testing.T) {
	w := openModelsDialogCount(t, 3)
	btns := footerButtons(w)
	if len(btns) != 5 {
		t.Fatalf("got %d footer buttons, want 5 (Empty/Edit/Remove/SetDefault/Done without catalog)", len(btns))
	}
	for i, b := range btns {
		if h := b.AbsoluteBounds().H; h != 2 {
			t.Errorf("footer button #%d bounds = %+v, want H==2 (2-row-tall footer, issue #529)", i, b.AbsoluteBounds())
		}
	}
}

// TestModelsDialogAllSixFooterButtonsTwoRowsTall wires the catalog too, so the full
// six-button footer the issue names (Add from Catalog…/Add Empty…/Edit…/Remove/Set
// Default/Done) is present — and every one of the six is 2 rows tall.
func TestModelsDialogAllSixFooterButtonsTwoRowsTall(t *testing.T) {
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
		if h := b.AbsoluteBounds().H; h != 2 {
			t.Errorf("footer button #%d H = %d, want 2", i, h)
		}
	}
}

// TestModelsDialogHeightIsPaneRowsPlusEight pins the height growth from issue #529:
// height0 == paneRows + 8 (was +7), where paneRows is the model count clamped to
// [1, 12]. The spec pins height (MinH==MaxH==height0), so on a roomy terminal the
// dialog tracks the list size exactly — never the 85% balloon — and grows by one
// row versus the old 1-row footer.
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
// the taller footer's geometry: every footer button's two rows must lie fully INSIDE
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
		if abs.H != 2 {
			t.Errorf("button #%d H=%d, want 2", i, abs.H)
		}
		if abs.Y != buttonY {
			t.Errorf("button #%d Y=%d differs from footer row %d (footer must share one row)", i, abs.Y, buttonY)
		}
		if abs.Y < topInterior || abs.Y+abs.H-1 > lastInterior {
			t.Errorf("button #%d rows %d..%d fall outside interior %d..%d (border clash)",
				i, abs.Y, abs.Y+abs.H-1, topInterior, lastInterior)
		}
	}
	// The list is focused (showModelsDialog calls SetFocus(list)); its bottom must
	// stay above the footer's top row so the list never overlaps the buttons.
	for _, c := range dialogDescendants(w) {
		if !c.Focused() {
			continue
		}
		listAbs := c.AbsoluteBounds()
		if listBottom := listAbs.Y + listAbs.H - 1; listBottom >= buttonY {
			t.Errorf("list bottom row %d >= footer top row %d (list/button overlap)", listBottom, buttonY)
		}
		break
	}
}

// TestModelsDialogEmptyListTwoRowButtons locks the empty-list state at the taller
// height: with zero models only Add Empty… + Done are present (Edit/Remove/Set
// Default absent), yet both still render 2 rows tall, the placeholder still shows,
// and the dialog sizes to paneRows(1)+8 = 9.
func TestModelsDialogEmptyListTwoRowButtons(t *testing.T) {
	w := openModelsDialogCount(t, 0)

	btns := footerButtons(w)
	if len(btns) != 2 {
		t.Fatalf("empty-list state: got %d footer buttons, want 2 (Add Empty + Done)", len(btns))
	}
	for i, b := range btns {
		if h := b.AbsoluteBounds().H; h != 2 {
			t.Errorf("empty-list footer button #%d H=%d, want 2 (taller footer applies to the empty state too)", i, h)
		}
	}
	if got := dialogBounds(w).H; got != 9 {
		t.Errorf("empty-list dialog height = %d, want 9 (paneRows 1 + 8)", got)
	}
	if !modelsDialogHasText(t, w, "No models configured") {
		t.Error("empty-list placeholder missing at the taller height")
	}
	for _, absent := range []string{"Edit", "Remove", "Set Default", "Catalog"} {
		if modelsDialogHasText(t, w, absent) {
			t.Errorf("empty-list state should not surface a %q affordance", absent)
		}
	}
}

// TestModelsDialogTwoRowButtonsSurviveResize verifies the reflow path (dialog.Fit's
// OnResize hook) keeps the 2-row footer correct across a terminal resize: the buttons
// stay H==2, stay inside the dialog interior, the height stays content-pinned at
// paneRows+8, and the dialog re-centers on the final terminal.
func TestModelsDialogTwoRowButtonsSurviveResize(t *testing.T) {
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
		for i, btn := range footerButtons(w) {
			abs := btn.AbsoluteBounds()
			if abs.H != 2 {
				t.Errorf("after resize to %dx%d: button #%d H=%d, want 2", size.W, size.H, i, abs.H)
			}
			if abs.Y < b.Y+1 || abs.Y+abs.H-1 > b.Y+b.H-2 {
				t.Errorf("after resize to %dx%d: button #%d rows %d..%d outside interior %d..%d",
					size.W, size.H, i, abs.Y, abs.Y+abs.H-1, b.Y+1, b.Y+b.H-2)
			}
		}
	}

	// Reflow re-centered the dialog on the final 200×50 terminal.
	b := dialogBounds(w)
	if wantX, wantY := (200-b.W)/2, (50-b.H)/2; b.X != wantX || b.Y != wantY {
		t.Errorf("dialog not re-centered after resize: origin (%d,%d), want (%d,%d)", b.X, b.Y, wantX, wantY)
	}
}

// TestPeerDialogsKeepOneRowFooter is the holistic regression guard: only the
// Models… dialog opted into the tall footer, so a peer dialog that uses the default
// footerButtonRects path (here the Statistics dialog) must still render every footer
// button exactly 1 row tall. It catches a leak if any peer dialog were switched to
// footerButtonRectsH. (The unit-level guard is TestFooterButtonRectsDefaultIsOneRowTall.)
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
			t.Errorf("statistics footer button #%d H=%d, want 1 (only the Models… dialog opted into 2-row buttons)", i, h)
		}
	}
}

// TestModelsDialogTwoRowFooterOnTinyTerminal opens the dialog AT a small terminal
// (the resolver's MinH-clamp path), including one shorter than height0 so the floor
// wins past the screen edge. The taller footer must still lay out without panic, the
// buttons must stay H==2 and within the dialog's own interior, and the list must not
// overlap them — proving the geometry formulas hold at the floor, not just on a roomy
// terminal.
func TestModelsDialogTwoRowFooterOnTinyTerminal(t *testing.T) {
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
		for i, btn := range btns {
			abs := btn.AbsoluteBounds()
			if abs.H != 2 {
				t.Errorf("term %dx%d: button #%d H=%d, want 2", term.W, term.H, i, abs.H)
			}
			if abs.Y < b.Y+1 || abs.Y+abs.H-1 > b.Y+b.H-2 {
				t.Errorf("term %dx%d: button #%d rows %d..%d outside interior %d..%d",
					term.W, term.H, i, abs.Y, abs.Y+abs.H-1, b.Y+1, b.Y+b.H-2)
			}
		}
	}
}
