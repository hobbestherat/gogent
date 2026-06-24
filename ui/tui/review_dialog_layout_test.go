package ui

import (
	"testing"

	tv "github.com/hobbestherat/turbotui/turbotv"
)

func TestReviewButtonRowStaysInsideDialog(t *testing.T) {
	const btnY = 9
	for _, width := range []int{40, 41, 44, 50, 120} {
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

func TestReviewButtonRowGapAtFloorAndWideWidth(t *testing.T) {
	for _, tc := range []struct {
		name  string
		width int
	}{
		{"min width shrinks gap to fit full labels", 40},
		{"wide width uses default gap", 120},
	} {
		t.Run(tc.name, func(t *testing.T) {
			total := 0
			for _, label := range reviewButtonLabels {
				total += tv.ButtonLabelWidth(label)
			}
			wantGap := tv.DefaultButtonGap
			if slack := (tc.width - 4) - total; slack < wantGap*(len(reviewButtonLabels)-1) {
				wantGap = slack / (len(reviewButtonLabels) - 1)
				if wantGap < 0 {
					wantGap = 0
				}
			}
			accept, acceptAll, reject := reviewButtonRow(tc.width, 9)
			if got := acceptAll.X - (accept.X + accept.W); got != wantGap {
				t.Errorf("gap between Accept and Accept all = %d, want %d", got, wantGap)
			}
			if got := reject.X - (acceptAll.X + acceptAll.W); got != wantGap {
				t.Errorf("gap between Accept all and Reject = %d, want %d", got, wantGap)
			}
		})
	}
}

func TestReviewButtonRowBelowFloorClampsInsteadOfEscaping(t *testing.T) {
	const width = 37
	_, _, reject := reviewButtonRow(width, 9)
	rightX := width - 3
	if reject.X < 2 || reject.X+reject.W-1 > rightX {
		t.Fatalf("below floor reject rect %+v escaped [2,%d]", reject, rightX)
	}
	if reject.W >= tv.ButtonLabelWidth("&Reject") {
		t.Fatalf("below floor reject width = %d, want clipped below full width %d", reject.W, tv.ButtonLabelWidth("&Reject"))
	}
}
