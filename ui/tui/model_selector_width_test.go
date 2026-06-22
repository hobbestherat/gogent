package ui

import (
	"strings"
	"testing"

	"gogent/internal/config"

	tui "github.com/hobbestherat/turbotui"
	tv "github.com/hobbestherat/turbotui/turbotv"
)

// TestHeaderSelectWidth checks the window-header model dropdown grows to fit the
// longest model name (plus the Select's two cells of chrome), keeps a minimum,
// and is clamped to the room left in the window (issue #108).
func TestHeaderSelectWidth(t *testing.T) {
	for _, tc := range []struct {
		name        string
		longest     int
		windowWidth int
		want        int
	}{
		// Short names: stays at the 24-cell minimum (matches the old behaviour).
		{"short name keeps minimum", 5, 100, 24},
		{"empty list keeps minimum", 0, 100, 24},
		// "Local LAN (env: GOGENT_MODEL_URL)" is 33 runes; +2 for value pad and ▼
		// glyph = 35, which fits a wide window without truncation.
		{"long name grows to fit", 33, 100, 35},
		// Window too narrow to fit the desired width: clamp to windowWidth-9.
		{"clamped to window width", 33, 30, 21},
		// Even a tiny window keeps the control at least one cell wide.
		{"never below one cell", 33, 8, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := headerSelectWidth(tc.longest, tc.windowWidth); got != tc.want {
				t.Errorf("headerSelectWidth(%d, %d) = %d, want %d",
					tc.longest, tc.windowWidth, got, tc.want)
			}
		})
	}
}

// TestHeaderSelectWidthFitsLongestName verifies that at a normal window width the
// selector is wide enough that the Select's text area (W-2) holds the full name,
// measured in display cells (tui.StringWidth), so a name like
// "Local LAN (env: GOGENT_MODEL_URL)" is not truncated.
func TestHeaderSelectWidthFitsLongestName(t *testing.T) {
	const name = "Local LAN (env: GOGENT_MODEL_URL)"
	longest := tui.StringWidth(name)
	selW := headerSelectWidth(longest, 120)
	if textArea := selW - 2; textArea < longest {
		t.Errorf("text area %d cannot hold %d-cell name %q (selW=%d)",
			textArea, longest, name, selW)
	}
}

// openModelEditor drives the REAL showModelEditor on a screenW×screenH terminal
// with a single model whose option label has the requested display-cell width, and
// returns the opened dialog's outer rectangle. Testing the live code path (rather
// than a mirror of its DialogSpec) is deliberate: round 1 used a static
// modelEditorSpec mirror that silently drifted from the source after the clip fix
// landed, turning a fixed bug into a false failure. Driving showModelEditor keeps
// the test honest against whatever sizing logic the dialog actually uses.
//
// The option label is nameLabel's "DisplayName (Name)"; with Name="x" (1 cell) the
// label width is len(DisplayName)+4, so labelCells controls the widest option.
func openModelEditor(t *testing.T, labelCells, screenW, screenH int) tv.Rect {
	t.Helper()
	if labelCells < 5 {
		t.Fatalf("labelCells %d too small to build a label", labelCells)
	}
	dn := strings.Repeat("x", labelCells-4) // "<dn> (x)" == labelCells cells
	w := newTestWorkbench(t)
	w.SetHandlers(Handlers{
		GetModels:   func() []config.ModelConfig { return []config.ModelConfig{{Name: "x", DisplayName: dn, Model: "m"}} },
		UpdateModel: func(config.ModelConfig) error { return nil },
	})
	w.app.Resize(screenW, screenH)
	w.showModelEditor()
	if top := w.desktop.TopLayer(); top == nil || top.Root == nil {
		t.Fatalf("model editor did not open")
	}
	return dialogBounds(w)
}

// modelEditorLabelCells reports the display width of the option label
// openModelEditor builds for labelCells (== labelCells by construction); it exists
// so the fit assertion measures the real label rather than assuming.
func modelEditorLabelCells(labelCells int) int {
	return tui.StringWidth(strings.Repeat("x", labelCells-4) + " (x)")
}

// TestModelEditorWidthFloor checks the 64-column comfort floor holds on a terminal
// whose 80% default would otherwise be narrower, for a short option.
func TestModelEditorWidthFloor(t *testing.T) {
	// screen 70 -> 80% = 56 < 64; a short option keeps the 64 floor.
	if b := openModelEditor(t, 8, 70, 40); b.W != modelEditorMinWidth {
		t.Errorf("model editor width on a 70-col terminal = %d, want %d (floor)", b.W, modelEditorMinWidth)
	}
}

// TestModelEditorSizedToContent checks the model editor sizes to content after the
// #309 turbotui cap-not-floor change: with a short option it is the 64-wide comfort
// floor (not the old 80% default), and — the round-1 BUG 3 fix — its height is the
// fixed 18-row footprint, not the inflated 85% vertical default.
func TestModelEditorSizedToContent(t *testing.T) {
	b := openModelEditor(t, 8, 200, 50)
	if b.W != modelEditorMinWidth {
		t.Errorf("model editor width on a 200-col terminal = %d, want %d (content floor, not 160)", b.W, modelEditorMinWidth)
	}
	if b.H != modelEditorHeight {
		t.Errorf("model editor height on a 50-row terminal = %d, want %d (pinned, not the 85%% default of 42)", b.H, modelEditorHeight)
	}
}

// TestModelEditorWidthFitsOption is the round-1 BUG 1 regression guard: the boxed
// Select must show the widest option in full whenever the terminal has room for it.
// model_editor.go now lifts MinW to the content width (bounded by the usable
// screen) so a content-driven PreferredW above the 80% cap is no longer clamped
// below what the option needs. The {70,100} case is the original repro: a 70-cell
// option needs a 93-col dialog, which fits in the 96 usable cols.
func TestModelEditorWidthFitsOption(t *testing.T) {
	for _, tc := range []struct {
		name               string
		labelCells, screen int
	}{
		{"short option, roomy screen", 10, 200},
		{"long option exceeds 80% cap but fits screen", 70, 100},
		{"very long option, very roomy screen", 120, 250},
	} {
		t.Run(tc.name, func(t *testing.T) {
			labelW := modelEditorLabelCells(tc.labelCells)
			needed := labelW + 2 + modelEditorBoxX + 3
			usable := tc.screen - 2*tv.DefaultDialogMargin
			if needed > usable {
				t.Skipf("screen %d too small to ever hold a %d-cell option (needed %d > usable %d)",
					tc.screen, labelW, needed, usable)
			}
			b := openModelEditor(t, tc.labelCells, tc.screen, 50)
			boxW := b.W - modelEditorBoxX - 3 // mirrors showModelEditor's boxW
			if boxW-2 < labelW {
				t.Errorf("labelW=%d screen=%d: dialog width %d -> boxW %d cannot hold the option (text area %d); "+
					"long model name would clip", labelW, tc.screen, b.W, boxW, boxW-2)
			}
		})
	}
}

// TestLongestRuneLen checks the widest-string helper now measures in DISPLAY
// CELLS (tui.StringWidth) — the CJK case is the issue #299 fix: a three-glyph CJK
// option occupies six cells, not three (the old rune count under-sized the box).
func TestLongestRuneLen(t *testing.T) {
	for _, tc := range []struct {
		name string
		ss   []string
		want int
	}{
		{"empty", nil, 0},
		{"single", []string{"abc"}, 3},
		{"widest wins", []string{"a", "abcd", "ab"}, 4},
		{"cell-aware not rune-count", []string{"中文字"}, 6},
		{"cjk widest beats ascii", []string{"abcd", "中文"}, 4}, // ascii 4 vs cjk 4 cells
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := longestRuneLen(tc.ss); got != tc.want {
				t.Errorf("longestRuneLen(%v) = %d, want %d", tc.ss, got, tc.want)
			}
		})
	}
}
