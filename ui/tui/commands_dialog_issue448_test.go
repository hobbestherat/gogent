package ui

import (
	"fmt"
	"testing"

	tv "github.com/hobbestherat/turbotui/turbotv"
)

// commandsDialogSizeTests isolates the acceptance/defect-hunting coverage for
// issue #448 — the Commands configuration dialog was sized with an inline
// tv.DialogSpec{MinW: 84, MinH: 26, PreferredW: 96} that set PreferredW but
// omitted MaxW/MaxH/PrefH. That pinned the width at 96 and let the height
// balloon to ~85% of the terminal. The fix extracted a content-driven
// commandsDialogSpec() (mirroring watchers/sessions/statistics). These tests
// drive the real spec and the real open dialog so a drift in either is caught.

// wiredCommandsWorkbench returns a workbench with the ListCommands + GetCustomCommand
// handlers showCommandsDialog needs, seeded with `n` commands so the open path also
// exercises loadSelected. The dialog never calls Create/Update/Delete on open, so
// those stay nil (and the editor degrades gracefully if a footer action invokes them).
func wiredCommandsWorkbench(t *testing.T, n int) *Workbench {
	t.Helper()
	w := newTestWorkbench(t)
	w.SetHandlers(Handlers{
		ListCommands: func() []CommandInfo {
			out := make([]CommandInfo, 0, n)
			for i := 0; i < n; i++ {
				out = append(out, CommandInfo{
					Name:        fmt.Sprintf("cmd-%d", i),
					Description: "desc",
				})
			}
			return out
		},
		GetCustomCommand: func(name string) (CommandDef, error) {
			return CommandDef{Name: name, Template: "do $thing"}, nil
		},
	})
	return w
}

// TestCommandsDialogSpecShape locks the shape of the content-driven spec itself.
// These invariants are what keep the dialog well-behaved: the floors are below the
// preferred sizes (so the dialog can both grow and shrink), the caps sit under the
// 80%×85% balloon (so it never sprawls), and MinW is raised to the footer's measured
// width when the footer needs more than the floor (so the action row never overlaps).
// A drift here — e.g. swapping PrefH for the differently-spelled PreferredH, dropping
// a cap, or a floor above its preferred — is the most likely way the fix regresses.
func TestCommandsDialogSpecShape(t *testing.T) {
	spec := newTestWorkbench(t).commandsDialogSpec()

	// The footprint the fix moved to: a content-driven preferred size with caps and
	// a floor, NOT the old PreferredW-only inline spec.
	if spec.PreferredW != 112 || spec.PrefH != 34 {
		t.Errorf("preferred = %dx%d, want 112x34 (content footprint)", spec.PreferredW, spec.PrefH)
	}
	if spec.MinW != 84 || spec.MinH != 26 {
		t.Errorf("floors = %dx%d, want 84x26", spec.MinW, spec.MinH)
	}
	if spec.MaxW != 140 || spec.MaxH != 40 {
		t.Errorf("caps = %dx%d, want 140x40", spec.MaxW, spec.MaxH)
	}

	// Structural sanity: floors below preferred below caps on each axis. If a floor
	// ever rises above its preferred the dialog is pinned; if a cap drops below the
	// floor the resolver's min-last ordering silently defeats the cap.
	if spec.MinW > spec.PreferredW || spec.PreferredW > spec.MaxW {
		t.Errorf("width ordering broken: Min %d / Pref %d / Max %d", spec.MinW, spec.PreferredW, spec.MaxW)
	}
	if spec.MinH > spec.PrefH || spec.PrefH > spec.MaxH {
		t.Errorf("height ordering broken: Min %d / Pref %d / Max %d", spec.MinH, spec.PrefH, spec.MaxH)
	}

	// The caps must stay under the 80%×85% balloon (160×42 on a 200×50 terminal) —
	// otherwise the dialog can still sprawl to the percentage box the issue complains of.
	if spec.MaxW >= 160 {
		t.Errorf("MaxW %d >= 160 lets the dialog balloon toward the percentage width", spec.MaxW)
	}
	if spec.MaxH >= 42 {
		t.Errorf("MaxH %d >= 42 lets the dialog balloon toward the percentage height", spec.MaxH)
	}
}

// TestCommandsDialogSpecFloorsAboveFooterNeed is the core "footer never overlaps"
// guarantee. commandsDialogSpec raises MinW to footerRowMinWidth(commandsFooterLabels,
// DefaultButtonGap) when that exceeds the 84 floor. The commands footer needs 63 cells
// (below 84, so MinW stays 84), but the invariant the code expresses is MinW >= the
// footer's true width — locked here so a future label change that widens the footer is
// automatically reflected in the floor rather than silently letting buttons clamp.
func TestCommandsDialogSpecFloorsAboveFooterNeed(t *testing.T) {
	spec := newTestWorkbench(t).commandsDialogSpec()
	need := footerRowMinWidth(commandsFooterLabels, tv.DefaultButtonGap)

	// The documented footer width (5 buttons + 4 gaps + 4 edge cells), for regression
	// notice if a label change shifts it.
	if need != 63 {
		t.Logf("note: commands footer now needs %d cells (was 63 at #448)", need)
	}
	if spec.MinW < need {
		t.Errorf("spec.MinW %d < footer need %d — the action row can clamp/overlap at the floor",
			spec.MinW, need)
	}
	// Today the floor (84) dominates the footer need (63), so MinW is exactly 84.
	if spec.MinW != 84 {
		t.Errorf("spec.MinW = %d, want 84 (floor dominates the 63-cell footer need)", spec.MinW)
	}
}

// TestCommandsDialogSpecIsTerminalIndependent pins the issue's claim that the spec is
// STATIC (no terminal-share term), which is why it can use dialog.Fit instead of
// installResizeReflow. If a future change bakes the terminal width into it (as
// browserDialogSpec does), the dialog would no longer be path-independent on resize.
func TestCommandsDialogSpecIsTerminalIndependent(t *testing.T) {
	w := newTestWorkbench(t)
	base := w.commandsDialogSpec()
	for _, dim := range []struct{ W, H int }{{80, 24}, {120, 30}, {200, 50}, {300, 80}} {
		w.app.Resize(dim.W, dim.H)
		got := w.commandsDialogSpec()
		if got != base {
			t.Errorf("spec changed with terminal at %dx%d: got %+v, want static %+v", dim.W, dim.H, got, base)
		}
	}
}

// TestCommandsDialogSize drives the real commandsDialogSpec() through the shared
// resolver at the terminals that matter for the acceptance criteria and checks the
// resolved size exactly. On a roomy terminal it is the content footprint (~112×28),
// not the old 96×42 balloon; on a 120-wide terminal the 80% width cap bites; on a tiny
// terminal both floors win even past the screen edge.
func TestCommandsDialogSize(t *testing.T) {
	spec := newTestWorkbench(t).commandsDialogSpec()
	for _, tc := range []struct {
		name             string
		screenW, screenH int
		wantW, wantH     int
	}{
		// #448 acceptance: 200×50 → 112×28 (was 96×42).
		{"roomy terminal sizes to content not the balloon", 200, 50, 112, 34},
		// #448 acceptance: 120×30 shrinks toward the 84×26 floor (height hits 26).
		{"120x30 shrinks toward the floor", 120, 30, 96, 26},
		// Width capped at the 80% default (96) below the 112 preferred; height keeps PrefH 34.
		{"120x40 width capped at 80 percent", 120, 40, 96, 34},
		// Ultrawide: PreferredW (112) is below the cap, so the dialog does NOT sprawl.
		{"ultrawide stays at content size", 300, 80, 112, 34},
		// Below-floor terminal: both Min floors win (84×26) even past the screen edge.
		{"tiny terminal floors both dimensions", 40, 16, 84, 26},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, gotW, gotH := tv.ResolveDialogRect(spec, tc.screenW, tc.screenH)
			if gotW != tc.wantW || gotH != tc.wantH {
				t.Errorf("size(%d,%d) = %dx%d, want %dx%d",
					tc.screenW, tc.screenH, gotW, gotH, tc.wantW, tc.wantH)
			}
			// Never below the declared floor.
			if gotW < spec.MinW || gotH < spec.MinH {
				t.Errorf("size %dx%d fell below the %dx%d floor", gotW, gotH, spec.MinW, spec.MinH)
			}
		})
	}

	// The crux of #448: on a roomy terminal it must NOT be the 160×42 percentage
	// balloon that motivated the issue, in BOTH dimensions.
	gotW, gotH := resolveSize(spec, 200, 50)
	if gotW >= 160 || gotH >= 42 {
		t.Errorf("200x50 resolved to %dx%d — the percentage balloon is back", gotW, gotH)
	}
	// And it must be materially wider than the old pinned-96 width and materially
	// shorter than the old ~42-row balloon.
	if gotW <= 96 {
		t.Errorf("width %d did not grow past the old pinned 96", gotW)
	}
	if gotH >= 40 {
		t.Errorf("height %d reached the MaxH cap — PrefH 34 should bound it well under the balloon", gotH)
	}
}

// resolveSize is a small helper so table rows stay readable.
func resolveSize(spec tv.DialogSpec, screenW, screenH int) (int, int) {
	_, _, w, h := tv.ResolveDialogRect(spec, screenW, screenH)
	return w, h
}

// TestCommandsDialogOpensContentDriven opens the real dialog (via showCommandsDialog)
// and asserts the resolved bounds match the spec end-to-end — the layer is the
// commands-dialog, it is centered, and on a roomy terminal it is NOT the balloon. It
// also covers the empty-list open path (clearForm) and a sub-floor terminal.
func TestCommandsDialogOpensContentDriven(t *testing.T) {
	for _, tc := range []struct {
		name             string
		screenW, screenH int
		commands         int
		wantW, wantH     int
	}{
		{"roomy terminal not the balloon", 200, 50, 3, 112, 34},
		{"roomy terminal empty list", 200, 50, 0, 112, 34},
		{"120x30 shrinks toward floor", 120, 30, 2, 96, 26},
		{"tiny terminal floors", 48, 14, 0, 84, 26},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := wiredCommandsWorkbench(t, tc.commands)
			w.app.Resize(tc.screenW, tc.screenH)
			w.showCommandsDialog()

			top := w.desktop.TopLayer()
			if top == nil || top.Name != "commands-dialog" {
				t.Fatalf("top layer = %v, want commands-dialog", top)
			}
			b := dialogBounds(w)
			if b.W != tc.wantW || b.H != tc.wantH {
				t.Fatalf("opened size on %dx%d = %dx%d, want %dx%d",
					tc.screenW, tc.screenH, b.W, b.H, tc.wantW, tc.wantH)
			}
			if tc.screenW >= 200 && (b.W >= 160 || b.H >= 42) {
				t.Fatalf("dialog opened as the 160x42 balloon: %dx%d", b.W, b.H)
			}
			// Centered when it fits; origin floored at 0 when the floor exceeds the screen.
			wantX, wantY := (tc.screenW-b.W)/2, (tc.screenH-b.H)/2
			if wantX < 0 {
				wantX = 0
			}
			if wantY < 0 {
				wantY = 0
			}
			if b.X != wantX || b.Y != wantY {
				t.Errorf("origin = (%d,%d), want (%d,%d)", b.X, b.Y, wantX, wantY)
			}
		})
	}
}

// TestCommandsDialogResizePathIndependent verifies the issue's claim that, because the
// spec is static, dialog.Fit re-resolves the open dialog on resize — no
// installResizeReflow needed. A dialog opened small and grown must match one opened
// fresh on the larger terminal (path-independence), and re-center.
func TestCommandsDialogResizePathIndependent(t *testing.T) {
	resized := wiredCommandsWorkbench(t, 2)
	resized.app.Resize(80, 24)
	resized.showCommandsDialog()
	before := dialogBounds(resized)

	resized.app.Resize(200, 50)
	got := dialogBounds(resized)

	fresh := wiredCommandsWorkbench(t, 2)
	fresh.app.Resize(200, 50)
	fresh.showCommandsDialog()
	want := dialogBounds(fresh)

	// Opened on the small terminal it floors to 84×26.
	if before.W != 84 || before.H != 26 {
		t.Fatalf("opened on 80x24 = %dx%d, want the 84x26 floor", before.W, before.H)
	}
	// After growing it must match a fresh open on 200×50 (path-independent).
	if got.W != want.W || got.H != want.H {
		t.Fatalf("after resize = %dx%d, want fresh-open %dx%d (spec must re-resolve, not pin the open-time size)",
			got.W, got.H, want.W, want.H)
	}
	if got.W != 112 || got.H != 34 {
		t.Fatalf("after resize to 200x50 = %dx%d, want 112x34", got.W, got.H)
	}
	if got.X != (200-got.W)/2 || got.Y != (50-got.H)/2 {
		t.Errorf("not re-centered after resize: origin (%d,%d)", got.X, got.Y)
	}
}

// TestCommandsDialogUnavailableShowsCompactFallback covers the error path:
// showCommandsDialog is a no-op (a friendly confirm) when command management is not
// wired (ListCommands == nil). It must NOT open the commands-dialog, and the fallback
// confirm must be compact — materially smaller than the commands footprint.
func TestCommandsDialogUnavailableShowsCompactFallback(t *testing.T) {
	w := newTestWorkbench(t)
	w.app.Resize(200, 50)
	w.showCommandsDialog() // no ListCommands handler wired

	top := w.desktop.TopLayer()
	if top == nil {
		t.Fatal("expected an unavailable confirmation dialog")
	}
	if top.Name == "commands-dialog" {
		t.Fatal("opened the commands-dialog despite ListCommands being nil — the unavailable guard is missing")
	}
	b := dialogBounds(w)
	// The fallback is a one-line confirm, so it is materially smaller than the
	// commands editor's 112×28 footprint.
	if b.W >= 112 || b.H >= 28 {
		t.Errorf("unavailable fallback size = %dx%d, want compact (smaller than the commands footprint)", b.W, b.H)
	}
}

// TestCommandsDialogFooterFitsAtMinWidth is the direct "footer buttons do not overlap"
// acceptance check, laid out at the dialog's MINIMUM width and the sizes it actually
// takes. Because the floor (84) is above the footer's measured need (63), every button
// keeps its full ButtonLabelWidth with a clean DefaultButtonGap between neighbours and
// a clear left margin — no clamping, no overlap.
func TestCommandsDialogFooterFitsAtMinWidth(t *testing.T) {
	const leftX, gap = 2, tv.DefaultButtonGap
	// Widths the dialog resolves to: the 84 floor, the 120-wide cap (96), the 112
	// preferred, and the 140 MaxW (a hypothetical wide terminal share).
	for _, width := range []int{84, 96, 112, 140} {
		t.Run(fmt.Sprintf("width_%d", width), func(t *testing.T) {
			rightX := width - 3
			rects := footerButtonRects(commandsFooterLabels, leftX, rightX, width-3, gap)
			assertFooterInvariants(t, commandsFooterLabels, rects, leftX, rightX, width-3, gap)

			// No two buttons overlap (the acceptance criterion), and every face keeps
			// its full caption width — i.e. the group genuinely fits, it is not merely
			// clamped into place.
			for i := 0; i < len(rects); i++ {
				if rects[i].W != tv.ButtonLabelWidth(commandsFooterLabels[i]) {
					t.Errorf("at width %d button %q clamped to W=%d (group does not fit at the floor)",
						width, commandsFooterLabels[i], rects[i].W)
				}
				for j := i + 1; j < len(rects); j++ {
					if rectsOverlap(rects[i], rects[j]) {
						t.Errorf("at width %d buttons %q and %q overlap: %+v / %+v",
							width, commandsFooterLabels[i], commandsFooterLabels[j], rects[i], rects[j])
					}
				}
			}
			// Clear left margin: the group fits with slack, not flush against the edge.
			if rects[0].X <= leftX {
				t.Errorf("at width %d the footer is flush against leftX (no slack): first X=%d",
					width, rects[0].X)
			}
		})
	}

	// At the floor (84) the footer need (63) must leave comfortable slack — the spec's
	// MinW>=footer invariant made concrete.
	const floorW = 84
	need := footerRowMinWidth(commandsFooterLabels, gap)
	if avail := floorW - 4; need > avail { // 4 = the two 2-cell margins footerButtonRects reserves
		t.Errorf("footer need %d exceeds usable %d at the %d floor", need, avail, floorW)
	}
}

// TestCommandsDialogInnerGeometryFitsAtFloorAndRoomy replicates the layout math in
// showCommandsDialog (detail pane width, box width, preview height, list pane height)
// at the resolved floor (84×26) and the roomy size (112×28) and asserts every region
// clears its collapse-guard with margin. This is the "detail form + parameter
// sub-editor + live preview fully visible" acceptance criterion made into a guard:
// if a floor were ever tightened below what the form needs, a region would collapse.
func TestCommandsDialogInnerGeometryFitsAtFloorAndRoomy(t *testing.T) {
	for _, tc := range []struct {
		name       string
		width      int
		height     int
		wantDetail int // detailW = width - 28 (detailX=26, right margin 2)
		wantBox    int // boxW = detailW - labelW(7)
		wantPane   int // paneH = height - 7 (listY 2 + 5)
		wantPrev   int // previewH = height - 18 (final row 14 + 4)
	}{
		// Floor: 84×26. detailW 84-28=56, boxW 56-7=49, paneH 26-7=19, previewH 26-18=8.
		{"floor 84x26", 84, 26, 56, 49, 19, 8},
		// Roomy: 112×28. detailW 112-28=84, boxW 84-7=77, paneH 28-7=21, previewH 28-18=10.
		{"roomy 112x28", 112, 28, 84, 77, 21, 10},
	} {
		t.Run(tc.name, func(t *testing.T) {
			width, height := tc.width, tc.height
			// Mirrors commands_dialog.go: detailX = listX(2)+listW(22)+2 = 26.
			const detailX, labelW = 26, 7
			detailW := width - detailX - 2
			boxW := detailW - labelW
			paneH := height - 2 /*listY*/ - 5
			row := 2 + 5 /*name..agent*/ + 1 /*subtask*/ + 1 /*params label*/ + 3 /*paramList*/ + 1 /*insert*/ + 1 /*args*/
			previewH := height - row - 4

			if detailW != tc.wantDetail {
				t.Errorf("detailW = %d, want %d", detailW, tc.wantDetail)
			}
			if boxW != tc.wantBox {
				t.Errorf("boxW = %d, want %d", boxW, tc.wantBox)
			}
			if paneH != tc.wantPane {
				t.Errorf("paneH = %d, want %d", paneH, tc.wantPane)
			}
			if previewH != tc.wantPrev {
				t.Errorf("previewH = %d, want %d", previewH, tc.wantPrev)
			}

			// Every region clears its collapse-guard with margin — the guards (detailW<30,
			// boxW<12, paneH<6, previewH<2) must never trigger at a resolved size.
			if detailW < 30 {
				t.Errorf("detailW %d below the 30 guard — the form column collapsed", detailW)
			}
			if boxW < 12 {
				t.Errorf("boxW %d below the 12 guard — the text boxes collapsed", boxW)
			}
			if paneH < 6 {
				t.Errorf("paneH %d below the 6 guard — the command list collapsed", paneH)
			}
			if previewH < 2 {
				t.Errorf("previewH %d below the 2 guard — the live preview collapsed", previewH)
			}

			// The preview must sit entirely ABOVE the hint (height-4) and button
			// (height-3) rows — no vertical collision between the form/preview and the
			// footer chrome. preview occupies [row, row+previewH-1]; hint is at height-4.
			previewBottom := row + previewH - 1
			if previewBottom >= height-4 {
				t.Errorf("preview bottom row %d reaches the hint row %d — footer collision",
					previewBottom, height-4)
			}
		})
	}
}

// TestCommandsDialogFooterRendersFullWidth is the live-layout check: opening the real
// dialog and inspecting the rendered button widgets (the DrawOutside components) shows
// the five footer buttons laid out by footerButtonRects at the resolved width — full
// caption width, right-aligned, and no two buttons overlapping anywhere in the dialog.
func TestCommandsDialogFooterRendersFullWidth(t *testing.T) {
	w := wiredCommandsWorkbench(t, 3)
	w.app.Resize(200, 50)
	w.showCommandsDialog()
	b := dialogBounds(w)

	wantFooter := footerButtonRects(commandsFooterLabels, 2, b.W-3, b.H-3, tv.DefaultButtonGap)

	// Buttons are exactly the DrawOutside components (turbotui buttons draw their
	// shadow outside their bounds; labels/text boxes do not).
	var buttonRects []tv.Rect
	for _, c := range dialogDescendants(w) {
		if c.DrawOutside {
			buttonRects = append(buttonRects, c.Bounds)
		}
	}
	if len(buttonRects) < len(wantFooter) {
		t.Fatalf("found %d button widgets, want at least the %d footer buttons",
			len(buttonRects), len(wantFooter))
	}
	// Every footer rect is rendered, at its full caption width.
	for i, want := range wantFooter {
		if !containsRect(buttonRects, want) {
			t.Errorf("footer button %d (%q) not rendered at %+v; buttons = %+v",
				i, commandsFooterLabels[i], want, buttonRects)
		}
		if want.W < tv.ButtonLabelWidth(commandsFooterLabels[i]) {
			t.Errorf("rendered footer button %q W=%d below ButtonLabelWidth",
				commandsFooterLabels[i], want.W)
		}
	}
	// No two buttons anywhere in the dialog overlap (footer, parameter row and preview
	// button are on distinct rows, so this is a global collision guard).
	for i := 0; i < len(buttonRects); i++ {
		for j := i + 1; j < len(buttonRects); j++ {
			if rectsOverlap(buttonRects[i], buttonRects[j]) {
				t.Errorf("rendered buttons overlap: %+v and %+v", buttonRects[i], buttonRects[j])
			}
		}
	}
}
