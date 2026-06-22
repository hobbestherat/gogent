package ui

import (
	"fmt"
	"reflect"
	"testing"

	tui "github.com/hobbestherat/turbotui"
	tv "github.com/hobbestherat/turbotui/turbotv"
)

// TestParseMnemonicCleanWidth checks the contract gogent now depends on after
// issue #299: button measurement strips the '&' mnemonic marker and measures the
// remaining label in DISPLAY CELLS (tui.StringWidth), not rune count. The CJK case
// is the regression that motivated the migration — the old gogent cleanMnemonicRunes
// returned a rune count (3 for "中文&字"), under-counting the four wide cells the
// label actually occupies.
func TestParseMnemonicCleanWidth(t *testing.T) {
	cleanWidth := func(label string) int {
		clean, _ := tv.ParseMnemonic(label)
		return tui.StringWidth(clean)
	}
	for _, tc := range []struct {
		label string
		want  int
	}{
		{"", 0},
		{"Close", 5},
		{"Export &CSV", 10},  // '&' marks the 'C' hotkey and is dropped
		{"Export &JSON", 11}, // '&' marks the 'J' hotkey and is dropped
		{"&OK", 2},
		{"Save && &Quit", 11}, // "&&" -> literal '&', '&' marks 'Q' -> "Save & Quit"
		{"Trailing&", 9},      // a lone trailing '&' has no following rune, kept literally
		{"中文&字", 6},           // display-cell aware: three CJK glyphs at 2 cells each
	} {
		if got := cleanWidth(tc.label); got != tc.want {
			t.Errorf("clean display width of %q = %d, want %d", tc.label, got, tc.want)
		}
	}
}

// TestButtonLabelWidth checks the declared width is the clean label's display
// width plus the "[ " ... " ]" chrome, floored at turbotui's minButtonWidth (10)
// so short captions like "Close"/"Deny" never render as a cramped "[…]".
func TestButtonLabelWidth(t *testing.T) {
	for _, tc := range []struct {
		label string
		want  int
	}{
		{"Close", 10},        // 5 + 4 chrome = 9, floored up to 10
		{"Deny", 10},         // 4 + 4 = 8, floored up to 10
		{"OK", 10},           // 2 + 4 = 6, floored up to 10
		{"Export &CSV", 14},  // "[ Export CSV ]"  = 10 + 4
		{"Export &JSON", 15}, // "[ Export JSON ]" = 11 + 4
		{"Allow once", 14},   // 10 + 4, above the floor
	} {
		if got := tv.ButtonLabelWidth(tc.label); got != tc.want {
			t.Errorf("tv.ButtonLabelWidth(%q) = %d, want %d", tc.label, got, tc.want)
		}
	}
}

// assertFooterInvariants checks the properties the footer must always satisfy:
// every rect is on row y with height 1, sized to its label, inside [leftX,
// rightX], the last button is flush with rightX, and consecutive buttons are
// separated by exactly gap columns. It is the acceptance check for issue #104
// (clean, aligned, non-overlapping, in-bounds button row).
func assertFooterInvariants(t *testing.T, labels []string, rects []tv.Rect, leftX, rightX, y, gap int) {
	t.Helper()
	if len(rects) != len(labels) {
		t.Fatalf("got %d rects, want %d", len(rects), len(labels))
	}
	for i, r := range rects {
		if r.Y != y || r.H != 1 {
			t.Errorf("rect %d (%q) = %+v, want Y=%d H=1", i, labels[i], r, y)
		}
		if r.W != tv.ButtonLabelWidth(labels[i]) {
			t.Errorf("rect %d (%q) width = %d, want label width %d", i, labels[i], r.W, tv.ButtonLabelWidth(labels[i]))
		}
		if r.X < leftX {
			t.Errorf("rect %d (%q) X=%d before leftX=%d", i, labels[i], r.X, leftX)
		}
		if right := r.X + r.W - 1; right > rightX {
			t.Errorf("rect %d (%q) right=%d past rightX=%d", i, labels[i], right, rightX)
		}
	}
	if len(rects) > 0 {
		last := rects[len(rects)-1]
		if right := last.X + last.W - 1; right != rightX {
			t.Errorf("last button right edge = %d, want rightX=%d (group must be right-aligned)", right, rightX)
		}
	}
	for i := 0; i < len(rects)-1; i++ {
		got := rects[i+1].X - rects[i].X - rects[i].W
		if got != gap {
			t.Errorf("gap between %q and %q = %d, want %d", labels[i], labels[i+1], got, gap)
		}
	}
}

// TestFooterButtonRectsStatistics locks the exact layout of the statistics
// footer (Export CSV / Export JSON / Close) at the dialog's default width and
// checks every footer invariant holds. The "Close" button is 10 wide now (the
// minButtonWidth floor in tv.ButtonLabelWidth), up from the old gogent 9.
func TestFooterButtonRectsStatistics(t *testing.T) {
	const width = 80
	const leftX, rightX, y, gap = 2, width - 3, 21, 2
	labels := []string{"Export &CSV", "Export &JSON", "Close"}

	rects := footerButtonRects(labels, leftX, rightX, y, gap)

	want := []tv.Rect{
		{X: 35, Y: 21, W: 14, H: 1}, // "[ Export CSV ]"
		{X: 51, Y: 21, W: 15, H: 1}, // "[ Export JSON ]"
		{X: 68, Y: 21, W: 10, H: 1}, // "[ Close ]" floored to 10
	}
	if !reflect.DeepEqual(rects, want) {
		t.Errorf("footerButtonRects = %+v, want %+v", rects, want)
	}
	assertFooterInvariants(t, labels, rects, leftX, rightX, y, gap)
}

// TestFooterButtonRectsAtDialogSizes verifies the statistics footer is correctly
// laid out (in-bounds, right-aligned, non-overlapping) across the range of
// widths the statistics dialog can actually take — its 60-column floor up to a
// roomy 120-column terminal. This is the "correct at small and large widths"
// acceptance criterion for issue #104.
func TestFooterButtonRectsAtDialogSizes(t *testing.T) {
	labels := []string{"Export &CSV", "Export &JSON", "Close"}
	for _, width := range []int{60, 64, 72, 80, 120} {
		t.Run(fmt.Sprintf("width_%d", width), func(t *testing.T) {
			const leftX, y, gap = 2, 21, 2
			rightX := width - 3
			rects := footerButtonRects(labels, leftX, rightX, y, gap)
			assertFooterInvariants(t, labels, rects, leftX, rightX, y, gap)
		})
	}
}

// TestFooterButtonRectsClampsOnNarrowDialog checks the safety net: when the
// dialog is far too narrow to hold the group, no rect ever has a negative/zero
// width or escapes [leftX, rightX]. (Buttons may overlap in this degenerate
// case; the statistics dialog floors its width above the group's needs, so this
// never triggers in practice — it just must not produce garbled rectangles.)
func TestFooterButtonRectsClampsOnNarrowDialog(t *testing.T) {
	labels := []string{"Export &CSV", "Export &JSON", "Close"}
	const leftX, rightX, y, gap = 2, 27, 21, 2 // 26 usable columns for a 42-wide group
	rects := footerButtonRects(labels, leftX, rightX, y, gap)
	for i, r := range rects {
		if r.W <= 0 {
			t.Errorf("rect %d (%q) has non-positive width %d", i, labels[i], r.W)
		}
		if r.X < leftX {
			t.Errorf("rect %d (%q) X=%d before leftX=%d", i, labels[i], r.X, leftX)
		}
		if right := r.X + r.W - 1; right > rightX {
			t.Errorf("rect %d (%q) right=%d past rightX=%d", i, labels[i], right, rightX)
		}
	}
}

// TestFooterButtonRectsSingleButton checks a one-button footer right-aligns to
// rightX.
func TestFooterButtonRectsSingleButton(t *testing.T) {
	const leftX, rightX, y, gap = 2, 77, 21, 2
	rects := footerButtonRects([]string{"Close"}, leftX, rightX, y, gap)
	assertFooterInvariants(t, []string{"Close"}, rects, leftX, rightX, y, gap)
}

// TestFooterButtonRectsEmpty checks an empty label list yields no rects.
func TestFooterButtonRectsEmpty(t *testing.T) {
	if rects := footerButtonRects(nil, 2, 77, 21, 2); len(rects) != 0 {
		t.Errorf("footerButtonRects(nil) = %+v, want empty", rects)
	}
}

// TestClampDialogRect covers the in-bounds guard directly: a rect that starts
// before leftX is pushed in, one that runs past rightX is trimmed, and a rect
// trimmed below zero width is floored at 0 (never negative).
func TestClampDialogRect(t *testing.T) {
	const leftX, rightX = 2, 20
	for _, tc := range []struct {
		name string
		in   tv.Rect
		want tv.Rect
	}{
		{"already inside", tv.Rect{X: 5, Y: 1, W: 4, H: 1}, tv.Rect{X: 5, Y: 1, W: 4, H: 1}},
		{"left of margin pushed in", tv.Rect{X: 0, Y: 1, W: 4, H: 1}, tv.Rect{X: 2, Y: 1, W: 4, H: 1}},
		{"runs past right trimmed", tv.Rect{X: 18, Y: 1, W: 10, H: 1}, tv.Rect{X: 18, Y: 1, W: 3, H: 1}},
		{"fully past right floored to zero width", tv.Rect{X: 25, Y: 1, W: 5, H: 1}, tv.Rect{X: 25, Y: 1, W: 0, H: 1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := clampDialogRect(tc.in, leftX, rightX); got != tc.want {
				t.Errorf("clampDialogRect(%+v) = %+v, want %+v", tc.in, got, tc.want)
			}
			got := clampDialogRect(tc.in, leftX, rightX)
			if got.W < 0 {
				t.Errorf("clampDialogRect produced negative width: %+v", got)
			}
		})
	}
}
