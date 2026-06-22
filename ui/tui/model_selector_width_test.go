package ui

import (
	"testing"

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

// modelEditorSpec rebuilds the DialogSpec showModelEditor uses (issues #108,
// #299): large by default (≈80% wide) with a 64×18 floor, and PreferredW wide
// enough to hold the widest option in the boxed Select (longest + 2 cells of
// Select chrome + boxX + 3) so a long model name never clips.
func modelEditorSpec(longest int) tv.DialogSpec {
	return tv.DialogSpec{MinW: 64, MinH: 18, PreferredW: longest + 2 + modelEditorBoxX + 3}
}

// TestModelEditorWidthFloor checks the 64-column floor holds on a terminal whose
// 80% default would otherwise be narrower.
func TestModelEditorWidthFloor(t *testing.T) {
	// screen 70 -> 80% = 56 < 64; short options keep the 64 floor.
	_, _, w, _ := tv.ResolveDialogRect(modelEditorSpec(10), 70, 40)
	if w != 64 {
		t.Errorf("model editor width on a 70-col terminal = %d, want 64 (floor)", w)
	}
}

// TestModelEditorLargeByDefault checks the editor is large by default — ≈80% of a
// roomy terminal — rather than pinned to its 64-column floor (issue #299).
func TestModelEditorLargeByDefault(t *testing.T) {
	_, _, w, h := tv.ResolveDialogRect(modelEditorSpec(10), 200, 50)
	if w != 160 {
		t.Errorf("model editor width on a 200-col terminal = %d, want 160 (80%%)", w)
	}
	if h != 42 { // 85% of 50
		t.Errorf("model editor height on a 50-row terminal = %d, want 42 (85%%)", h)
	}
}

// TestModelEditorWidthFitsOption verifies the derived boxW holds the widest
// option plus the Select's two cells of chrome across the screen sizes where the
// dialog is not forced below its preferred width.
func TestModelEditorWidthFitsOption(t *testing.T) {
	for _, tc := range []struct {
		longest, screen int
	}{
		{10, 200},  // short option, roomy screen
		{70, 100},  // long option drives the width
		{120, 250}, // very long option, very roomy screen
	} {
		_, _, width, _ := tv.ResolveDialogRect(modelEditorSpec(tc.longest), tc.screen, 50)
		boxW := width - modelEditorBoxX - 3
		if boxW-2 < tc.longest {
			t.Errorf("longest=%d screen=%d: boxW %d cannot hold the option (text area %d)",
				tc.longest, tc.screen, boxW, boxW-2)
		}
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
