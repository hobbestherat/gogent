package ui

import (
	"fmt"
	"reflect"
	"testing"

	tv "github.com/hobbestherat/turbotui/turbotv"
)

// TestCleanMnemonicRunes checks the mnemonic-stripping width matches
// turbotv.parseMnemonic for plain text, hotkey markers, escaped ampersands and
// trailing ampersands.
func TestCleanMnemonicRunes(t *testing.T) {
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
		{"Trailing&", 9},      // "Trailing" (8) + literal trailing '&' = 9
		{"中文&字", 3},           // rune-aware, not byte-aware
	} {
		if got := cleanMnemonicRunes(tc.label); got != tc.want {
			t.Errorf("cleanMnemonicRunes(%q) = %d, want %d", tc.label, got, tc.want)
		}
	}
}

// TestButtonLabelWidth checks the declared width is the clean label plus the
// "[ " ... " ]" chrome, so a button renders without clipping.
func TestButtonLabelWidth(t *testing.T) {
	for _, tc := range []struct {
		label string
		want  int
	}{
		{"Close", 5 + buttonChrome},         // "[ Close ]"
		{"Export &CSV", 10 + buttonChrome},  // "[ Export CSV ]"
		{"Export &JSON", 11 + buttonChrome}, // "[ Export JSON ]"
	} {
		if got := buttonLabelWidth(tc.label); got != tc.want {
			t.Errorf("buttonLabelWidth(%q) = %d, want %d", tc.label, got, tc.want)
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
		if r.W != buttonLabelWidth(labels[i]) {
			t.Errorf("rect %d (%q) width = %d, want label width %d", i, labels[i], r.W, buttonLabelWidth(labels[i]))
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
// checks every footer invariant holds.
func TestFooterButtonRectsStatistics(t *testing.T) {
	const width = 80
	const leftX, rightX, y, gap = 2, width - 3, 21, 2
	labels := []string{"Export &CSV", "Export &JSON", "Close"}

	rects := footerButtonRects(labels, leftX, rightX, y, gap)

	want := []tv.Rect{
		{X: 36, Y: 21, W: 14, H: 1}, // "[ Export CSV ]"
		{X: 52, Y: 21, W: 15, H: 1}, // "[ Export JSON ]"
		{X: 69, Y: 21, W: 9, H: 1},  // "[ Close ]"
	}
	if !reflect.DeepEqual(rects, want) {
		t.Errorf("footerButtonRects = %+v, want %+v", rects, want)
	}
	assertFooterInvariants(t, labels, rects, leftX, rightX, y, gap)
}

// TestFooterButtonRectsAtDialogSizes verifies the statistics footer is correctly
// laid out (in-bounds, right-aligned, non-overlapping) across the range of
// widths the statistics dialog can actually take — its 60-column floor up to the
// 80-column cap. This is the "correct at small and large widths" acceptance
// criterion for issue #104.
func TestFooterButtonRectsAtDialogSizes(t *testing.T) {
	labels := []string{"Export &CSV", "Export &JSON", "Close"}
	for _, width := range []int{60, 64, 72, 80} {
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
