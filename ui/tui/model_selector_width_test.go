package ui

import "testing"

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
// so a name like "Local LAN (env: GOGENT_MODEL_URL)" is not truncated.
func TestHeaderSelectWidthFitsLongestName(t *testing.T) {
	const name = "Local LAN (env: GOGENT_MODEL_URL)"
	longest := runeLen(name)
	selW := headerSelectWidth(longest, 120)
	if textArea := selW - 2; textArea < longest {
		t.Errorf("text area %d cannot hold %d-rune name %q (selW=%d)",
			textArea, longest, name, selW)
	}
}

// TestModelEditorWidth checks the dialog width fits the widest option, keeps a
// baseline minimum, and never exceeds the screen (issue #108).
func TestModelEditorWidth(t *testing.T) {
	for _, tc := range []struct {
		name    string
		longest int
		screen  int
		want    int
	}{
		// Short options: stays at the 64-cell baseline.
		{"short options keep minimum", 10, 200, 64},
		// A long "DisplayName (config-name)" option grows the dialog:
		// longest + 2 + boxX(18) + 3.
		{"long option grows dialog", 60, 200, 60 + 2 + modelEditorBoxX + 3},
		// Clamped to the screen when the desired width would overflow.
		{"clamped to screen", 120, 80, 80},
		// A zero/unknown screen width does not clamp.
		{"unknown screen does not clamp", 60, 0, 60 + 2 + modelEditorBoxX + 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := modelEditorWidth(tc.longest, tc.screen); got != tc.want {
				t.Errorf("modelEditorWidth(%d, %d) = %d, want %d",
					tc.longest, tc.screen, got, tc.want)
			}
		})
	}
}

// TestModelEditorWidthFitsOption verifies the derived boxW holds the widest
// option plus the Select's two cells of chrome at a normal screen width.
func TestModelEditorWidthFitsOption(t *testing.T) {
	const option = "Local LAN (env: GOGENT_MODEL_URL) (lan)"
	longest := runeLen(option)
	width := modelEditorWidth(longest, 200)
	boxW := width - modelEditorBoxX - 3
	if boxW-2 < longest {
		t.Errorf("boxW %d cannot hold %d-rune option (text area %d)", boxW, longest, boxW-2)
	}
}

// TestLongestRuneLen checks the rune-aware widest-string helper.
func TestLongestRuneLen(t *testing.T) {
	for _, tc := range []struct {
		name string
		ss   []string
		want int
	}{
		{"empty", nil, 0},
		{"single", []string{"abc"}, 3},
		{"widest wins", []string{"a", "abcd", "ab"}, 4},
		{"rune-aware not byte-aware", []string{"中文字"}, 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := longestRuneLen(tc.ss); got != tc.want {
				t.Errorf("longestRuneLen(%v) = %d, want %d", tc.ss, got, tc.want)
			}
		})
	}
}
