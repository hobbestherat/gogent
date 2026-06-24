package ui

import (
	"testing"

	tv "github.com/hobbestherat/turbotui/turbotv"
)

func TestReviewButtonRowStaysInsideDialog(t *testing.T) {
	const btnY = 9
	for _, width := range []int{40, 120} {
		t.Run("", func(t *testing.T) {
			accept, acceptAll, reject := reviewButtonRow(width, btnY)
			rects := []tv.Rect{accept, acceptAll, reject}
			labels := reviewButtonLabels
			rightX := width - 3

			for i, r := range rects {
				if r.Y != btnY || r.H != 1 {
					t.Errorf("width=%d: rect %d (%q) = %+v, want Y=%d H=1", width, i, labels[i], r, btnY)
				}
				if r.W != tv.ButtonLabelWidth(labels[i]) {
					t.Errorf("width=%d: rect %d (%q) width = %d, want %d", width, i, labels[i], r.W, tv.ButtonLabelWidth(labels[i]))
				}
				if r.X < 2 || r.X+r.W-1 > rightX {
					t.Errorf("width=%d: rect %d (%q) %+v out of [2,%d]", width, i, labels[i], r, rightX)
				}
			}

			if accept.X+accept.W-1 >= acceptAll.X {
				t.Errorf("width=%d: Accept overlaps Accept all: %+v %+v", width, accept, acceptAll)
			}
			if acceptAll.X+acceptAll.W-1 >= reject.X {
				t.Errorf("width=%d: Accept all overlaps Reject: %+v %+v", width, acceptAll, reject)
			}
		})
	}
}

func TestReviewButtonRowUsesDefaultGap(t *testing.T) {
	accept, acceptAll, reject := reviewButtonRow(120, 9)
	if got := acceptAll.X - (accept.X + accept.W); got != tv.DefaultButtonGap {
		t.Errorf("gap between Accept and Accept all = %d, want %d", got, tv.DefaultButtonGap)
	}
	if got := reject.X - (acceptAll.X + acceptAll.W); got != tv.DefaultButtonGap {
		t.Errorf("gap between Accept all and Reject = %d, want %d", got, tv.DefaultButtonGap)
	}
}
