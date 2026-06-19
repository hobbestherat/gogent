package ui

import (
	"strings"
	"testing"
)

// TestMessageDialogLayout covers the confirm/message dialog sizing: a short
// message stays compact, a long one grows the body and dialog height, an
// over-long one caps the body (scrolls), and a narrow/short terminal clamps the
// width/height. The body height is always height-6 (borders + paddings + gap +
// button row) and never below 1.
func TestMessageDialogLayout(t *testing.T) {
	for _, tc := range []struct {
		name                    string
		termW, termH            int
		message                 string
		wantW, wantH, wantBodyH int
	}{
		{"short message compact", 80, 25, "Are you sure you want to quit?", 54, 7, 1},
		{"empty message one row", 80, 25, "", 54, 7, 1},
		{"two explicit lines", 80, 25, "Line one.\nLine two.", 54, 8, 2},
		{
			// Long single line wraps over the 49-col body; height tracks the body.
			"long line wraps and grows",
			80, 25, strings.Repeat("word ", 40),
			54, 6 + wrapRowCount(strings.Repeat("word ", 40), 54-5),
			wrapRowCount(strings.Repeat("word ", 40), 54-5),
		},
		{
			// 40 separate lines exceed the cap, so the body scrolls at the cap.
			"overflow caps body and scrolls",
			80, 25, strings.Repeat("x\n", 39) + "x",
			54, messageMaxBodyRows + 6, messageMaxBodyRows,
		},
		{
			// A short terminal clamps the height below the content; the body scrolls.
			"short terminal clamps height",
			80, 10, strings.Repeat("x\n", 39) + "x",
			54, 8, 2,
		},
		{"narrow terminal clamps width", 20, 25, "hi", 20, 7, 1},
		{"medium terminal narrows width", 40, 25, "hi", 38, 7, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w, h, bodyH := messageDialogLayout(tc.termW, tc.termH, tc.message)
			if w != tc.wantW || h != tc.wantH || bodyH != tc.wantBodyH {
				t.Errorf("messageDialogLayout(%d,%d,%q) = (w=%d,h=%d,body=%d), want (w=%d,h=%d,body=%d)",
					tc.termW, tc.termH, tc.message, w, h, bodyH, tc.wantW, tc.wantH, tc.wantBodyH)
			}
			if bodyH < 1 {
				t.Errorf("body height %d < 1", bodyH)
			}
		})
	}
}

// TestConfirmButtonRow checks the Yes/No pair is centred, sized to its rendered
// label width, and stays inside the dialog content margins.
func TestConfirmButtonRow(t *testing.T) {
	const width, btnY = 54, 3
	yes, no := confirmButtonRow(width, btnY)

	if yes.W != buttonLabelWidth("Yes") || no.W != buttonLabelWidth("No") {
		t.Fatalf("button widths = (%d,%d), want (%d,%d)", yes.W, no.W, buttonLabelWidth("Yes"), buttonLabelWidth("No"))
	}
	if yes.Y != btnY || no.Y != btnY {
		t.Errorf("button row Y = (%d,%d), want %d", yes.Y, no.Y, btnY)
	}
	// "No" follows "Yes" with a gap, and both sit within [2, width-3].
	if no.X <= yes.X+yes.W {
		t.Errorf("No (X=%d) overlaps Yes (X=%d,W=%d)", no.X, yes.X, yes.W)
	}
	if yes.X < 2 || no.X+no.W-1 > width-3 {
		t.Errorf("buttons escape margins: yes=%+v no=%+v width=%d", yes, no, width)
	}
	// The pair is roughly centred: equal slack on both sides (±1).
	leftSlack := yes.X - 2
	rightSlack := (width - 3) - (no.X + no.W - 1)
	if d := leftSlack - rightSlack; d < -1 || d > 1 {
		t.Errorf("not centred: leftSlack=%d rightSlack=%d", leftSlack, rightSlack)
	}
}

// TestCenteredButton checks a single button is centred and clamped.
func TestCenteredButton(t *testing.T) {
	const width, btnY = 54, 3
	r := centeredButton(width, btnY, "OK")
	if r.W != buttonLabelWidth("OK") || r.Y != btnY {
		t.Fatalf("centeredButton = %+v, want W=%d Y=%d", r, buttonLabelWidth("OK"), btnY)
	}
	leftSlack := r.X - 2
	rightSlack := (width - 3) - (r.X + r.W - 1)
	if d := leftSlack - rightSlack; d < -1 || d > 1 {
		t.Errorf("not centred: %+v (left=%d right=%d)", r, leftSlack, rightSlack)
	}
}

// TestShowConfirmOpensModal confirms both the informational (nil onResult) and
// the Yes/No variants push a single modal "confirm-dialog" layer on top.
func TestShowConfirmOpensModal(t *testing.T) {
	for _, tc := range []struct {
		name     string
		onResult func(bool)
	}{
		{"informational OK", nil},
		{"confirmation Yes/No", func(bool) {}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := newTestWorkbench(t)
			w.showConfirm("Title", "A message that the dialog should display.", tc.onResult)
			top := w.desktop.TopLayer()
			if top == nil || top.Name != "confirm-dialog" {
				t.Fatalf("top layer = %v, want confirm-dialog", top)
			}
		})
	}
}
