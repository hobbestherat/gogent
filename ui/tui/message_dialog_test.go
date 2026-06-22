package ui

import (
	"strings"
	"testing"

	tv "github.com/hobbestherat/turbotui/turbotv"
)

// resolveMessageDialog mirrors what showConfirm does at open time: build the
// spec, resolve it against the terminal (the default margin/percentages live in
// tv.ResolveDialogRect, the same policy w.dialogRect uses), and derive the body
// height. It lets the sizing be asserted without a live event loop.
func resolveMessageDialog(termW, termH int, message string) (width, height, bodyH int) {
	spec := messageDialogSpec(termW, termH, message)
	_, _, width, height = tv.ResolveDialogRect(spec, termW, termH)
	bodyH = messageBodyHeight(height)
	return width, height, bodyH
}

// TestMessageDialogLargeByDefault covers issue #299's central inversion: a short
// confirm is no longer pinned to 54×8 — it fills ≈80% of the width and ≈85% of
// the height of the terminal. The body height is always height-6 (borders +
// paddings + gap + button row) and never below 1.
func TestMessageDialogLargeByDefault(t *testing.T) {
	for _, tc := range []struct {
		name                    string
		termW, termH            int
		message                 string
		wantW, wantH, wantBodyH int
	}{
		// 80% of 80 = 64 wide; 85% of 25 = 21 tall; body = 21-6 = 15.
		{"short message fills terminal", 80, 25, "Are you sure you want to quit?", 64, 21, 15},
		{"empty message still large", 80, 25, "", 64, 21, 15},
		{"two short lines still large", 80, 25, "Line one.\nLine two.", 64, 21, 15},
		// On a big terminal it scales up, not stays small: 80% of 200 = 160, 85% of 50 = 42.
		{"scales with a big terminal", 200, 50, "ok?", 160, 42, 36},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w, h, bodyH := resolveMessageDialog(tc.termW, tc.termH, tc.message)
			if w != tc.wantW || h != tc.wantH || bodyH != tc.wantBodyH {
				t.Errorf("resolveMessageDialog(%d,%d,%q) = (w=%d,h=%d,body=%d), want (w=%d,h=%d,body=%d)",
					tc.termW, tc.termH, tc.message, w, h, bodyH, tc.wantW, tc.wantH, tc.wantBodyH)
			}
			if bodyH < 1 {
				t.Errorf("body height %d < 1", bodyH)
			}
		})
	}
}

// TestMessageDialogNoBodyCap is the acceptance test for deleting messageMaxBodyRows
// (=12): given vertical room, a tall message must grow the body well past 12 rows
// instead of truncating. The old cap would have pinned bodyH at 12 here.
func TestMessageDialogNoBodyCap(t *testing.T) {
	msg := strings.Repeat("x\n", 39) + "x" // 40 hard lines
	// A terminal tall enough to show all 40 lines plus chrome.
	_, height, bodyH := resolveMessageDialog(80, 60, msg)
	if bodyH < 40 {
		t.Errorf("bodyH = %d, want >= 40 (the 12-row cap must be gone)", bodyH)
	}
	if height > 60 {
		t.Errorf("height %d exceeds terminal 60", height)
	}
}

// TestMessageDialogGrowsWithContentWidth checks a long single line widens the
// dialog past the percentage default (PreferredW = longest line + pad), up to the
// terminal edge.
func TestMessageDialogGrowsWithContentWidth(t *testing.T) {
	long := strings.Repeat("a", 150) // far wider than 80% of a 120-col terminal
	width, _, _ := resolveMessageDialog(120, 40, long)
	def := 120 * 80 / 100 // 96
	if width <= def {
		t.Errorf("width = %d, want > %d (content wider than the 80%% default should widen it)", width, def)
	}
	if width > 120-4 {
		t.Errorf("width = %d, want <= terminal-2*margin (116)", width)
	}
}

// TestMessageDialogClampsToTinyTerminal checks a message on a tiny terminal stays
// on screen (never wider/taller than the margin allows) and keeps a visible body.
func TestMessageDialogClampsToTinyTerminal(t *testing.T) {
	for _, tc := range []struct{ termW, termH int }{
		{20, 8}, {30, 10}, {10, 6},
	} {
		w, h, bodyH := resolveMessageDialog(tc.termW, tc.termH, strings.Repeat("word ", 40))
		// MinW for the message dialog is 30; height has no floor, so it never
		// exceeds the terminal minus the 2-cell margins.
		if h > tc.termH {
			t.Errorf("termH=%d: height %d overflows the screen", tc.termH, h)
		}
		if bodyH < 1 {
			t.Errorf("termH=%d: bodyH %d, want >= 1", tc.termH, bodyH)
		}
		_ = w
	}
}

// TestMessageBodyRows checks the per-line wrap-row counter that drives PrefH. It
// wraps each hard line independently at width-5 (the body spans width-4 columns
// and turbotui reserves the last for the scrollbar) via tv.WrapText, and never
// reports fewer than one row.
func TestMessageBodyRows(t *testing.T) {
	for _, tc := range []struct {
		name    string
		message string
		width   int
		want    int
	}{
		{"empty is one row", "", 64, 1},
		{"short fits one row", "hello", 64, 1},
		{"three hard lines", "a\nb\nc", 64, 3},
		// width 10 -> wrapW 5; "hello world" wraps to 2 rows.
		{"wraps at width-5", "hello world", 10, 2},
		// A single line longer than wrapW hard-splits: 12 chars at wrapW 5 -> 3 rows.
		{"long line hard-splits", strings.Repeat("a", 12), 10, 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := messageBodyRows(tc.message, tc.width); got != tc.want {
				t.Errorf("messageBodyRows(%q,%d) = %d, want %d", tc.message, tc.width, got, tc.want)
			}
		})
	}
}

// TestMessageBodyHeight checks the chrome subtraction and the one-row floor.
func TestMessageBodyHeight(t *testing.T) {
	for _, tc := range []struct {
		height, want int
	}{
		{21, 15}, // 21 - 6 chrome
		{7, 1},   // 7 - 6
		{6, 1},   // floored, would be 0
		{0, 1},   // floored, would be negative
	} {
		if got := messageBodyHeight(tc.height); got != tc.want {
			t.Errorf("messageBodyHeight(%d) = %d, want %d", tc.height, got, tc.want)
		}
	}
}

// TestConfirmButtonRow checks the Yes/No pair is centred, sized to its rendered
// label width, and stays inside the dialog content margins.
func TestConfirmButtonRow(t *testing.T) {
	const width, btnY = 64, 3
	yes, no := confirmButtonRow(width, btnY)

	if yes.W != tv.ButtonLabelWidth("Yes") || no.W != tv.ButtonLabelWidth("No") {
		t.Fatalf("button widths = (%d,%d), want (%d,%d)", yes.W, no.W, tv.ButtonLabelWidth("Yes"), tv.ButtonLabelWidth("No"))
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
	const width, btnY = 64, 3
	r := centeredButton(width, btnY, "OK")
	if r.W != tv.ButtonLabelWidth("OK") || r.Y != btnY {
		t.Fatalf("centeredButton = %+v, want W=%d Y=%d", r, tv.ButtonLabelWidth("OK"), btnY)
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
