package ui

import (
	"strings"
	"testing"

	tui "github.com/hobbestherat/turbotui"
	tv "github.com/hobbestherat/turbotui/turbotv"
)

// keybinding_customizer_issue461_test.go is the acceptance + defect-hunting suite for
// issue #461: the Customize Keybindings modal ballooned to ~80% of the terminal width
// (160 cols on a 200-wide terminal) because its inline tv.DialogSpec{MinW:58, MinH:16,
// MaxH:rows+9} left PreferredW/MaxW/PrefH at zero, so ResolveDialogRect defaulted to the
// 80%×85% share. A second bug clipped long chord labels: padName(…, 10) truncated
// Ctrl+Shift+V (12 cells, a shipped Window-tiling default) to "Ctrl+Shift".
//
// The fix extracted a content-driven keybindingsDialogSpec() (mirroring
// sessions/commands/watchers/statistics), widened the chord column to keybindChordColWidth
// (14), and named the footer labels so the spec can floor on their measured width. These
// tests lock both fixes and the surrounding invariants, and probe the four design
// criteria: (1) goal match — sizing + truncation fix only; (2) usability — right share,
// footer fits, chords surfaced; (3) no regressions — exact resolved sizes, resize path,
// row never clipped; (4) holistic — the spec resolves via turbotui's policy with no
// toolkit change. They mirror commands_dialog_issue448_test.go's structure and reuse its
// helpers (dialogBounds, assertFooterInvariants, rectsOverlap).

// keybindingsSpecDims are the terminals the sizing acceptance criteria range over, from
// a below-floor tiny terminal to an ultrawide one. They match the issue's measured table
// plus the tiny-floor and ultrawide edges TestCommandsDialogSize covers.
var keybindingsSpecDims = []struct {
	screenW, screenH int
	wantW, wantH     int // exact resolved size, hand-traced against resolveDimension
}{
	{40, 16, 58, 16},  // both Min floors win past the screen edge (MinW 58/MinH 16)
	{80, 24, 62, 20},  // width = PreferredW 62 (≤ 80%·80=64, floor 58 inert); height = 85%·24=20 caps PrefH 34 down
	{120, 40, 62, 34}, // width 62; height = PrefH 34 (< 85%·40=34 cap, < MaxH 40)
	{200, 50, 62, 34}, // the issue's headline: was 160×42, now 62×34
	{300, 80, 62, 34}, // ultrawide: settles at PreferredW/PrefH; MaxW/MaxH never bind
}

// TestKeybindingsDialogSpecShape locks the shape of the content-driven spec itself.
// A drift here — swapping PrefH for the differently-spelled PreferredH, dropping a cap,
// a floor above its preferred, or reverting to the old inline spec — is the most likely
// way the fix regresses. Mirrors TestCommandsDialogSpecShape.
func TestKeybindingsDialogSpecShape(t *testing.T) {
	spec := newTestWorkbench(t).keybindingsDialogSpec()

	// The content footprint the fix moved to (NOT the old PreferredW-less inline spec).
	if spec.PreferredW != 62 || spec.PrefH != 34 {
		t.Errorf("preferred = %dx%d, want 62x34 (content footprint)", spec.PreferredW, spec.PrefH)
	}
	if spec.MinW != 58 || spec.MinH != 16 {
		t.Errorf("floors = %dx%d, want 58x16", spec.MinW, spec.MinH)
	}
	if spec.MaxW != 76 || spec.MaxH != 40 {
		t.Errorf("caps = %dx%d, want 76x40", spec.MaxW, spec.MaxH)
	}

	// Structural sanity: floors below preferred below caps on each axis. If a floor ever
	// rises above its preferred the dialog is pinned; if a cap drops below the floor the
	// resolver's min-last ordering silently defeats the cap.
	if spec.MinW > spec.PreferredW || spec.PreferredW > spec.MaxW {
		t.Errorf("width ordering broken: Min %d / Pref %d / Max %d", spec.MinW, spec.PreferredW, spec.MaxW)
	}
	if spec.MinH > spec.PrefH || spec.PrefH > spec.MaxH {
		t.Errorf("height ordering broken: Min %d / Pref %d / Max %d", spec.MinH, spec.PrefH, spec.MaxH)
	}

	// Caps must stay under the 80%×85% balloon (160×42 on 200×50) so the dialog can never
	// sprawl back to the percentage box the issue complains about.
	if spec.MaxW >= 160 {
		t.Errorf("MaxW %d >= 160 lets the dialog balloon toward the percentage width", spec.MaxW)
	}
	if spec.MaxH >= 42 {
		t.Errorf("MaxH %d >= 42 lets the dialog balloon toward the percentage height", spec.MaxH)
	}

	// MinW is floored at the footer's measured width so the three footer buttons can never
	// overlap — the invariant the named keybindFooterLabels exists to express.
	need := footerRowMinWidth(keybindFooterLabels, tv.DefaultButtonGap)
	if spec.MinW < need {
		t.Errorf("spec.MinW %d < footer need %d — the footer can clamp/overlap at the floor",
			spec.MinW, need)
	}
}

// TestKeybindingsDialogSpecFloorsAboveFooterNeed is the "footer never overlaps" guarantee
// made concrete. The footer measures 41 cells (&Reset=10 + Reset &All=13 + Close=10, +2
// gaps of DefaultButtonGap, +4 edge; ButtonLabelWidth clamps each to minButtonWidth 10),
// below the 58 floor, so MinW stays 58 — but the floor must dominate the footer need so a
// future label change is picked up automatically. Mirrors TestCommandsDialogSpecFloorsAboveFooterNeed.
func TestKeybindingsDialogSpecFloorsAboveFooterNeed(t *testing.T) {
	spec := newTestWorkbench(t).keybindingsDialogSpec()
	need := footerRowMinWidth(keybindFooterLabels, tv.DefaultButtonGap)

	if need != 41 {
		t.Logf("note: keybindings footer now needs %d cells (was 41 at #461)", need)
	}
	if spec.MinW < need {
		t.Errorf("spec.MinW %d < footer need %d", spec.MinW, need)
	}
	// Today the 58 floor dominates the 41-cell footer need, so MinW is exactly 58.
	if spec.MinW != 58 {
		t.Errorf("spec.MinW = %d, want 58 (floor dominates the 41-cell footer need)", spec.MinW)
	}
}

// TestKeybindingsDialogSpecIsTerminalIndependent pins the issue's claim that the spec is
// STATIC (no terminal-share term), which is why it can use dialog.Fit instead of
// installResizeReflow. If a future change bakes the terminal width into it (as
// browserDialogSpec does), the dialog would no longer be path-independent on resize.
// Mirrors TestCommandsDialogSpecIsTerminalIndependent.
func TestKeybindingsDialogSpecIsTerminalIndependent(t *testing.T) {
	w := newTestWorkbench(t)
	base := w.keybindingsDialogSpec()
	for _, dim := range []struct{ W, H int }{{80, 24}, {120, 30}, {200, 50}, {300, 80}} {
		w.app.Resize(dim.W, dim.H)
		got := w.keybindingsDialogSpec()
		if got != base {
			t.Errorf("spec changed with terminal at %dx%d: got %+v, want static %+v", dim.W, dim.H, got, base)
		}
	}
}

// TestKeybindingsDialogSize drives the real keybindingsDialogSpec() through the shared
// resolver at the terminals that matter for the acceptance criteria and checks the
// resolved size EXACTLY. The loose `height ≤ 40` of an earlier draft would have masked a
// PrefH-vs-MaxH misread; exact sizes pin the truth. On a roomy terminal it is the content
// footprint (62×34), not the old 160×42 balloon. Mirrors TestCommandsDialogSize.
func TestKeybindingsDialogSize(t *testing.T) {
	spec := newTestWorkbench(t).keybindingsDialogSpec()
	for _, tc := range keybindingsSpecDims {
		t.Run(termName(tc.screenW, tc.screenH), func(t *testing.T) {
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

	// The crux of #461: on a roomy terminal it must NOT be the 160×42 percentage balloon
	// that motivated the issue, in BOTH dimensions.
	gotW, gotH := resolveSize(spec, 200, 50)
	if gotW >= 160 || gotH >= 42 {
		t.Errorf("200x50 resolved to %dx%d — the percentage balloon is back", gotW, gotH)
	}
	// Width settles at PreferredW (62), growing past the old pinned-58/80% only via the
	// preferred, and MaxW (76) is an inert ceiling there.
	if gotW != spec.PreferredW {
		t.Errorf("200x50 width = %d, want PreferredW %d (MaxW %d should be inert here)",
			gotW, spec.PreferredW, spec.MaxW)
	}
	// Direct analogue of commands_dialog_issue448_test.go:173 — PrefH, not MaxH, must bound
	// the height. MaxH is a downward-only ceiling that never binds while PrefH < MaxH.
	if gotH >= spec.MaxH {
		t.Errorf("200x50 height %d reached the MaxH cap — PrefH %d, not MaxH %d, should bound it",
			gotH, spec.PrefH, spec.MaxH)
	}
	if gotH != spec.PrefH {
		t.Errorf("200x50 height = %d, want PrefH %d (the settling height)", gotH, spec.PrefH)
	}
}

// TestKeybindingsDialogFooterLabels locks the footer captions in display order, and that
// they wire to the right handlers. The footer order is load-bearing: rects[0]→resetSelected,
// [1]→resetAll, [2]→closeFn in showKeybindingCustomizer, so &Reset must be first and Close
// last. A re-order here would silently swap which button does what.
func TestKeybindingsDialogFooterLabels(t *testing.T) {
	want := []string{"&Reset", "Reset &All", "Close"}
	if len(keybindFooterLabels) != len(want) {
		t.Fatalf("keybindFooterLabels has %d entries, want %d", len(keybindFooterLabels), len(want))
	}
	for i := range want {
		if keybindFooterLabels[i] != want[i] {
			t.Errorf("keybindFooterLabels[%d] = %q, want %q", i, keybindFooterLabels[i], want[i])
		}
	}
}

// TestKeybindingsDialogFooterFitsAtMinWidth is the direct "footer buttons do not overlap"
// acceptance check, laid out at the dialog's MINIMUM width and the sizes it actually takes.
// Because the 58 floor is above the footer's 41-cell need, every button keeps its full
// ButtonLabelWidth with a clean DefaultButtonGap between neighbours — no clamping, no
// overlap. Mirrors TestCommandsDialogFooterFitsAtMinWidth.
func TestKeybindingsDialogFooterFitsAtMinWidth(t *testing.T) {
	const leftX, gap = 2, tv.DefaultButtonGap
	// Widths the dialog resolves to: the 58 floor, the 62 preferred, and the 76 MaxW.
	for _, width := range []int{58, 62, 76} {
		t.Run(termName(width, 0), func(t *testing.T) {
			rightX := width - 3
			y := width - 3 // y is irrelevant to overlap; reuse a deterministic value
			rects := footerButtonRects(keybindFooterLabels, leftX, rightX, y, gap)
			assertFooterInvariants(t, keybindFooterLabels, rects, leftX, rightX, y, gap)

			// No two buttons overlap, and every face keeps its full caption width — i.e. the
			// group genuinely fits, it is not merely clamped into place.
			for i := 0; i < len(rects); i++ {
				if rects[i].W != tv.ButtonLabelWidth(keybindFooterLabels[i]) {
					t.Errorf("at width %d button %q clamped to W=%d (group does not fit)",
						width, keybindFooterLabels[i], rects[i].W)
				}
				for j := i + 1; j < len(rects); j++ {
					if rectsOverlap(rects[i], rects[j]) {
						t.Errorf("at width %d buttons %q and %q overlap: %+v / %+v",
							width, keybindFooterLabels[i], keybindFooterLabels[j], rects[i], rects[j])
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

	// At the 58 floor the 41-cell footer need must leave comfortable slack.
	const floorW = 58
	need := footerRowMinWidth(keybindFooterLabels, gap)
	if avail := floorW - 4; need > avail { // 4 = the two 2-cell margins footerButtonRects reserves
		t.Errorf("footer need %d exceeds usable %d at the %d floor", need, avail, floorW)
	}
}

// TestKeybindingsDialogOpensContentDriven opens the real dialog (via
// showKeybindingCustomizer) and asserts the resolved bounds match the spec end-to-end at
// every sizing terminal — the layer is the customizer, it is centered, and on a roomy
// terminal it is NOT the balloon. This catches a disconnect between keybindingsDialogSpec
// and the dialog that actually opens (e.g. the spec not being used, or Fit pinning a stale
// rect). Mirrors TestCommandsDialogOpensContentDriven.
func TestKeybindingsDialogOpensContentDriven(t *testing.T) {
	spec := newTestWorkbench(t).keybindingsDialogSpec()
	for _, tc := range keybindingsSpecDims {
		t.Run(termName(tc.screenW, tc.screenH), func(t *testing.T) {
			w := newTestWorkbench(t)
			w.app.Resize(tc.screenW, tc.screenH)
			w.showKeybindingCustomizer()

			top := w.desktop.TopLayer()
			if top == nil || top.Name != "keybinding-customizer" {
				t.Fatalf("top layer = %v, want keybinding-customizer", top)
			}
			b := dialogBounds(w)
			if b.W != tc.wantW || b.H != tc.wantH {
				t.Fatalf("opened size on %dx%d = %dx%d, want %dx%d",
					tc.screenW, tc.screenH, b.W, b.H, tc.wantW, tc.wantH)
			}
			// The opened dialog must match the spec resolved against the same terminal —
			// i.e. showKeybindingCustomizer actually uses keybindingsDialogSpec.
			_, _, rw, rh := tv.ResolveDialogRect(spec, tc.screenW, tc.screenH)
			if b.W != rw || b.H != rh {
				t.Errorf("opened %dx%d != spec-resolved %dx%d (dialog is not using keybindingsDialogSpec)",
					b.W, b.H, rw, rh)
			}
			// On a roomy terminal it must not be the 160×42 balloon.
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

// TestKeybindingsDialogResizePathIndependent verifies the issue's claim that, because the
// spec is static, dialog.Fit re-resolves the open dialog on resize — no
// installResizeReflow needed. A dialog opened small and grown must match one opened fresh
// on the larger terminal (path-independence), and re-center. Mirrors
// TestCommandsDialogResizePathIndependent.
func TestKeybindingsDialogResizePathIndependent(t *testing.T) {
	resized := newTestWorkbench(t)
	resized.app.Resize(80, 24)
	resized.showKeybindingCustomizer()
	before := dialogBounds(resized)

	resized.app.Resize(200, 50)
	got := dialogBounds(resized)

	fresh := newTestWorkbench(t)
	fresh.app.Resize(200, 50)
	fresh.showKeybindingCustomizer()
	want := dialogBounds(fresh)

	// Opened on the small terminal it resolves to 62×20 (PreferredW 62, 85%·24 height).
	if before.W != 62 || before.H != 20 {
		t.Fatalf("opened on 80x24 = %dx%d, want 62x20", before.W, before.H)
	}
	// After growing it must match a fresh open on 200×50 (path-independent): the spec
	// re-resolves, it does not pin the open-time rect.
	if got.W != want.W || got.H != want.H {
		t.Fatalf("after resize = %dx%d, want fresh-open %dx%d (spec must re-resolve, not pin)",
			got.W, got.H, want.W, want.H)
	}
	if got.W != 62 || got.H != 34 {
		t.Fatalf("after resize to 200x50 = %dx%d, want 62x34", got.W, got.H)
	}
	if got.X != (200-got.W)/2 || got.Y != (50-got.H)/2 {
		t.Errorf("not re-centered after resize: origin (%d,%d)", got.X, got.Y)
	}
}

// TestKeybindRowTextDoesNotTruncateLongChord is the core chord-truncation acceptance test
// (issue #461's secondary bug). The shipped Window-tiling default Ctrl+Shift+V (12 cells)
// must render in full — not clipped to "Ctrl+Shift" as the prior 10-cell column did. It
// uses the shipped actionWindowTileVertical default so it also guards the catalog itself.
func TestKeybindRowTextDoesNotTruncateLongChord(t *testing.T) {
	w := newTestWorkbench(t)
	a, ok := w.actionByID(actionWindowTileVertical)
	if !ok {
		t.Fatalf("actionWindowTileVertical missing from catalog")
	}
	// Sanity: the catalog default really is the chord the issue names, so the test is
	// exercising the reported path, not a constructed one.
	if got := w.chordFor(a.actionID); got.Ctrl != true || got.Shift != true || got.Rune != 'v' {
		t.Fatalf("actionWindowTileVertical default = %+v, want Ctrl+Shift+V", got)
	}

	row := w.keybindRowText(a)
	if !strings.Contains(row, "Ctrl+Shift+V") {
		t.Errorf("row %q does not contain the full \"Ctrl+Shift+V\" (was clipped to \"Ctrl+Shift\" by the old 10-cell column)", row)
	}
	if strings.Contains(row, "Ctrl+Shift") && !strings.Contains(row, "Ctrl+Shift+") {
		t.Errorf("row %q looks truncated at \"Ctrl+Shift\" (no trailing +key)", row)
	}
	// The full label must appear, including the key rune, for every shipped Ctrl+Shift+*
	// tiling default — the bindings the issue shows clipped out of the box.
	for _, id := range []tv.ActionID{actionWindowTileVertical, actionWindowTileHorizontal,
		actionWindowTileGrid, actionWindowCascade, actionWindowMaximizeAll} {
		act, ok := w.actionByID(id)
		if !ok {
			t.Errorf("catalog action %q missing", id)
			continue
		}
		label := chordLabel(w.chordFor(act.actionID))
		if !strings.Contains(w.keybindRowText(act), label) {
			t.Errorf("row for %q does not contain its full chord label %q", act.name, label)
		}
	}
}

// TestKeybindRowWidthFitsListAtAllWidths is the "the row is never clipped by the Tree"
// guard. keybindRowText builds fixed-width columns via padName (name 26, chord 14), so a
// row's display width is bounded; it must fit inside the list's inner width (width−4) at
// the dialog's MINIMUM resolved width, or the Tree would clip the (default)/(custom) tag.
// This locks the contract between the chord column width and the spec's MinW.
func TestKeybindRowWidthFitsListAtAllWidths(t *testing.T) {
	w := newTestWorkbench(t)
	spec := w.keybindingsDialogSpec()
	listInner := spec.MinW - 4 // list is NewTree(Rect{X:2, W:width-4}); narrowest at MinW

	maxRow := 0
	for _, a := range w.rebindable() {
		row := w.keybindRowText(a)
		rw := tui.StringWidth(row)
		if rw > maxRow {
			maxRow = rw
		}
		if rw > listInner {
			t.Errorf("row for %q is %d cells wide, exceeds list inner %d at MinW %d (tag would be Tree-clipped): %q",
				a.name, rw, listInner, spec.MinW, row)
		}
	}
	// A guard against the column width being raised without raising MinW: the widest row at
	// the default catalog must leave slack inside the floor's list area.
	if maxRow > listInner {
		t.Errorf("widest catalog row = %d cells, list inner at MinW = %d", maxRow, listInner)
	}
}

// TestKeybindEveryShippedChordFitsColumn is the systemic version of the truncation fix:
// every chord the catalog ships must fit within keybindChordColWidth cells, so NO default
// binding is ever clipped. If a future action ships with a default chord wider than the
// column (e.g. a Ctrl+Alt+Shift+PageDown), this fails and flags that the column needs
// widening — exactly the class of bug #461 reported.
func TestKeybindEveryShippedChordFitsColumn(t *testing.T) {
	w := newTestWorkbench(t)
	for _, a := range w.rebindable() {
		label := chordLabel(w.chordFor(a.actionID))
		if lw := tui.StringWidth(label); lw > keybindChordColWidth {
			t.Errorf("shipped chord %q for %q is %d cells, exceeds keybindChordColWidth %d (would be clipped)",
				label, a.name, lw, keybindChordColWidth)
		}
	}
}

// TestKeybindRowTextTagAndColumn pins keybindRowText's structure so the column change
// (10→14) cannot silently regress the alignment or the tag values the existing tests and
// the customizer rely on: the three tags, the name column, and that a short chord is
// right-padded (not truncated) so columns line up.
func TestKeybindRowTextTagAndColumn(t *testing.T) {
	w := newTestWorkbench(t)
	a, ok := w.actionByID(actionTranscriptToggleMsg)
	if !ok {
		t.Fatal("actionTranscriptToggleMsg missing")
	}

	// Default tag at the catalog default.
	if row := w.keybindRowText(a); !strings.Contains(row, "(default)") {
		t.Errorf("default row %q missing (default) tag", row)
	}
	// Custom tag after an override.
	w.applyBinding(actionTranscriptToggleMsg, tv.Chord{Rune: 'm'})
	if row := w.keybindRowText(a); !strings.Contains(row, "(custom)") || !strings.Contains(row, "m") {
		t.Errorf("custom row %q missing (custom) tag / chord m", row)
	}
	// Unbound tag after a clear.
	w.clearBinding(actionTranscriptToggleMsg)
	if row := w.keybindRowText(a); !strings.Contains(row, "(unbound)") {
		t.Errorf("unbound row %q missing (unbound) tag", row)
	}

	// A short chord (single rune) is padded to the full column width so the tag column
	// stays aligned across rows — the 14-cell column must pad, not just avoid truncating.
	w.applyBinding(actionTranscriptToggleMsg, tv.Chord{Rune: 'a'})
	row := w.keybindRowText(a)
	// name(26) sits after a 2-space indent; the chord "a" is followed by enough padding to
	// reach the tag. The full row width is fixed regardless of chord length.
	if got := tui.StringWidth(row); got > keybindingsSpecDims[2].wantW-4 { // 62-4 list inner
		t.Errorf("padded short-chord row %d cells exceeds list inner; padding regressed", got)
	}
}

// TestKeybindRowTextUserChordBeyondColumn is a documented boundary test for option (a)
// (hardcoded 14 + static PreferredW). A USER chord wider than 14 cells — none ship — is
// clipped to the column by padName (the chord is cut, but the row/tag still fit, because
// the column is fixed-width). This locks the chosen option's contract so a switch to a
// runtime-derived column (option b) is noticed, and confirms the row never overflows the
// list even for an over-long chord.
func TestKeybindRowTextUserChordBeyondColumn(t *testing.T) {
	w := newTestWorkbench(t)
	a, ok := w.actionByID(actionWindowTileVertical)
	if !ok {
		t.Fatal("actionWindowTileVertical missing")
	}
	// A 23-cell chord the user could plausibly bind (Ctrl+Alt+Shift+PageDown). applyBinding
	// is a force-set, so it needs no deliverability/scope validation — it just records the
	// override, which is all keybindRowText reads.
	long := tv.Chord{Key: tui.KeyPageDown, Ctrl: true, Alt: true, Shift: true}
	w.applyBinding(a.actionID, long)

	row := w.keybindRowText(a)
	full := chordLabel(long) // "Ctrl+Alt+Shift+PageDown"
	if strings.Contains(row, full) {
		t.Errorf("over-long user chord %q was NOT clipped to the %d-cell column (option a expects it clipped): %q",
			full, keybindChordColWidth, row)
	}
	// The chord is truncated to the column width; the key portion after the clip is gone.
	if strings.Contains(row, "PageDown") {
		t.Errorf("clipped row %q still contains the tail \"PageDown\" — column did not truncate", row)
	}
	// Crucially, the tag survives and the row still fits the list — option (a) clips the
	// chord, never the tag, and never overflows the Tree.
	if !strings.Contains(row, "(custom)") {
		t.Errorf("over-long-chord row %q lost its (custom) tag", row)
	}
	if rw := tui.StringWidth(row); rw > 62-4 {
		t.Errorf("over-long-chord row %d cells overflows list inner 58: %q", rw, row)
	}
}

// TestKeybindUnboundRowDoesNotMisalign guards the unbound sentinel "—" (U+2014). padName
// pads by RUNE count while the column/row is measured by display cells; if the sentinel
// were ever a wide/ambiguous-width glyph (turbotui reports U+2014 as width 1 today), the
// unbound row would be wider than its column budget and the (unbound) tag would drift a
// cell right of every other row. This locks width-1 so the unbound row aligns with the
// rest and still fits the list.
func TestKeybindUnboundRowDoesNotMisalign(t *testing.T) {
	w := newTestWorkbench(t)
	if rw := tui.RuneWidth('—'); rw != 1 {
		t.Fatalf("turbotui now reports the unbound sentinel — as width %d (was 1); padName's rune-based padding misaligns unbound rows", rw)
	}
	a, ok := w.actionByID(actionTranscriptToggleMsg)
	if !ok {
		t.Fatal("actionTranscriptToggleMsg missing")
	}
	w.clearBinding(actionTranscriptToggleMsg) // renders chordLabel as "—"

	row := w.keybindRowText(a)
	if !strings.Contains(row, "(unbound)") {
		t.Fatalf("unbound row %q missing (unbound) tag", row)
	}
	// The unbound row must be the same display width as a default row (the 1-wide sentinel
	// does not bump it past the fixed column), so columns line up and it fits the list.
	if rw := tui.StringWidth(row); rw > 62-4 {
		t.Errorf("unbound row %d cells exceeds list inner 58 (sentinel widened?): %q", rw, row)
	}
}

// termName builds a stable subtest label for a terminal size.
func termName(w, h int) string {
	if h == 0 {
		return "width"
	}
	switch {
	case w < 60:
		return "tiny"
	case w < 100:
		return "narrow"
	case w < 180:
		return "medium"
	default:
		return "wide"
	}
}
