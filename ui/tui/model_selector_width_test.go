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

// openModelsDialog drives the REAL showModelsDialog on a screenW×screenH terminal
// with one model whose row label has the requested display-cell width, and returns
// the opened dialog's outer rectangle. It replaces the old openModelEditor helper:
// since issue #509 the per-row unified dialog (a tv.Tree, no top model Select) is
// what is sized, so testing the live sizing path keeps the test honest against the
// dialog's actual width/height logic rather than a hand-mirrored spec.
//
// The row label is "<marker><disp> — <model> — <apitype>"; with model="m" and
// apitype "openai" the non-display tail is ~14 cells, so displayCells controls the
// row width for the fit assertion.
func openModelsDialog(t *testing.T, displayCells, screenW, screenH int) tv.Rect {
	t.Helper()
	if displayCells < 1 {
		t.Fatalf("displayCells %d too small", displayCells)
	}
	disp := strings.Repeat("x", displayCells)
	w := newTestWorkbench(t)
	w.SetHandlers(Handlers{
		GetModels: func() []config.ModelConfig {
			return []config.ModelConfig{{Name: "x", DisplayName: disp, Model: "m", APIType: "openai"}}
		},
		GetDefaultModel: func() string { return "x" },
		UpdateModel:     func(config.ModelConfig) error { return nil },
		AddModel:        func(config.ModelConfig) error { return nil },
		RemoveModel:     func(string) error { return nil },
		SetDefaultModel: func(string) error { return nil },
	})
	w.app.Resize(screenW, screenH)
	w.showModelsDialog()
	if top := w.desktop.TopLayer(); top == nil || top.Root == nil {
		t.Fatalf("models dialog did not open")
	}
	return dialogBounds(w)
}

// TestModelsDialogNotPercentageBalloon is the #309 regression guard carried over
// from the old editor sizing tests: on a roomy terminal the unified Models… dialog
// sizes to its CONTENT, NOT the 160×42 percentage balloon that motivated #309. The
// width follows the footer button row (wider than the 64 comfort floor once
// Add/Edit/Remove/Set Default/Done are all laid out) but stays under the 160 cap;
// the height is pinned to the one-row list plus chrome (paneRows+8 = 9 with the
// 2-row footer of issue #529), not the 85% default.
func TestModelsDialogNotPercentageBalloon(t *testing.T) {
	b := openModelsDialog(t, 6, 200, 50)
	if b.W >= 160 {
		t.Errorf("models dialog width = %d on 200 cols — the 160 percentage balloon is back", b.W)
	}
	if b.W < modelEditorMinWidth {
		t.Errorf("models dialog width = %d, want at least the %d floor", b.W, modelEditorMinWidth)
	}
	if b.H >= 42 {
		t.Errorf("models dialog height = %d, want content-sized (< 42), not the 85%% balloon", b.H)
	}
	if b.H != 9 {
		t.Errorf("models dialog height = %d, want 9 (one-row list + chrome + 2-row footer, issue #529)", b.H)
	}
}

// TestModelsDialogGrowsToFitWidestRow is the #108 regression guard: a model whose
// row label is wider than the comfort floor must grow the dialog so the row never
// clips, and the list area (width-4) must hold the full row label.
func TestModelsDialogGrowsToFitWidestRow(t *testing.T) {
	const dispCells = 60 // row ~ 2+60+3+1+3+6 = 75 cells > the 64 floor
	b := openModelsDialog(t, dispCells, 200, 50)
	if b.W <= modelEditorMinWidth {
		t.Fatalf("dialog width %d did not grow past the %d floor for a wide row", b.W, modelEditorMinWidth)
	}
	// The row label the dialog builds for this model.
	row := "✓ " + strings.Repeat("x", dispCells) + " — m — openai"
	rowW := tui.StringWidth(row)
	if listArea := b.W - 4; listArea < rowW {
		t.Errorf("dialog width %d -> list area %d cannot hold the %d-cell row; the row would clip", b.W, listArea, rowW)
	}
}

// TestLongestRuneLen checks the widest-string helper now measures in DISPLAY
// CELLS (tui.StringWidth) — the CJK case is the issue #299 fix: a three-glyph CJK
// option occupies six cells, not three (the old rune count under-sized the box).
// The helper is reused by showModelsDialog to size the list to its widest row.
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
