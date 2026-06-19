package ui

import (
	"strings"
	"testing"
)

// TestConfirmDialogLayout covers the grow-with-content / clamp-to-terminal /
// scroll-on-overflow sizing of the wrapping confirm dialog (issue #98). A short
// message yields a compact dialog; a long message grows the body up to the cap and
// scrolls on a short terminal instead of overflowing.
func TestConfirmDialogLayout(t *testing.T) {
	t.Run("short message is compact and fits", func(t *testing.T) {
		msg := "Are you sure?"
		width, height, bodyY, bodyH, btnY := confirmDialogLayout(120, 40, msg)
		if width < confirmMinWidth || width > confirmMaxWidth {
			t.Errorf("width = %d, want in [%d,%d]", width, confirmMinWidth, confirmMaxWidth)
		}
		if height != bodyY+bodyH+5 {
			t.Errorf("height = %d, want bodyY(%d)+bodyH(%d)+5 = %d", height, bodyY, bodyH, bodyY+bodyH+5)
		}
		if btnY != bodyY+bodyH+1 {
			t.Errorf("btnY = %d, want %d", btnY, bodyY+bodyH+1)
		}
		if height < confirmMinHeight {
			t.Errorf("height %d below floor %d", height, confirmMinHeight)
		}
		// Short content fits without scrolling.
		if bodyH < wrapRowCount(msg, width-5) {
			t.Errorf("bodyH = %d < content: short message should not scroll", bodyH)
		}
	})

	t.Run("long message grows height to fit on a roomy terminal", func(t *testing.T) {
		// Many short words wrap across several rows at the dialog width.
		msg := strings.Repeat("the quick brown fox jumps over the lazy dog. ", 6)
		width, _, _, bodyH, _ := confirmDialogLayout(120, 60, msg)
		contentRows := 0
		for _, line := range strings.Split(msg, "\n") {
			contentRows += wrapRowCount(line, width-5)
		}
		if contentRows > confirmMaxBodyRows {
			contentRows = confirmMaxBodyRows // capped
		}
		if bodyH != contentRows {
			t.Errorf("bodyH = %d, want full (capped) content %d", bodyH, contentRows)
		}
	})

	t.Run("enormous message caps at max body rows", func(t *testing.T) {
		msg := strings.Repeat("x", 5000)
		_, _, _, bodyH, _ := confirmDialogLayout(120, 80, msg)
		if bodyH > confirmMaxBodyRows {
			t.Errorf("bodyH = %d, want <= cap %d", bodyH, confirmMaxBodyRows)
		}
	})

	t.Run("short terminal scrolls and stays on screen", func(t *testing.T) {
		msg := strings.Repeat("line\n", 30)
		_, height, _, bodyH, _ := confirmDialogLayout(60, 10, msg)
		if height > 10 {
			t.Fatalf("height %d overflows a 10-row terminal", height)
		}
		if bodyH < 1 {
			t.Fatalf("bodyH = %d: no visible body", bodyH)
		}
	})

	t.Run("multiline message counts each line", func(t *testing.T) {
		msg := "Wrote CSV to:\n/home/user/.gogent/gogent-stats-20260101-120000.csv"
		width, _, _, bodyH, _ := confirmDialogLayout(120, 40, msg)
		contentRows := 0
		for _, line := range strings.Split(msg, "\n") {
			contentRows += wrapRowCount(line, width-5)
		}
		if bodyH < contentRows {
			t.Errorf("bodyH = %d < content %d: multiline message should fit", bodyH, contentRows)
		}
	})

	t.Run("tiny terminal never exceeds screen", func(t *testing.T) {
		width, height, _, bodyH, _ := confirmDialogLayout(30, 8, "ok")
		if width > 30 {
			t.Errorf("width %d exceeds terminal 30", width)
		}
		if height > 8 {
			t.Errorf("height %d exceeds terminal 8", height)
		}
		if bodyH < 1 {
			t.Errorf("bodyH = %d, want >= 1", bodyH)
		}
	})
}
