package ui

import (
	"strings"

	tui "github.com/hobbestherat/turbotui"
	tv "github.com/hobbestherat/turbotui/turbotv"
)

// Sizing knobs for the confirm/message dialog. The width is a fixed preferred
// value clamped to the terminal; the height grows with the wrapped message up
// to messageMaxBodyRows, beyond which (and on short terminals) the body scrolls
// rather than overflowing — the original turbotui helper used a fixed 2-row,
// non-wrapping label, so long messages were silently clipped (issue #98).
const (
	messageWidth       = 54
	messageMinWidth    = 30
	messageMaxBodyRows = 12
)

// messageDialogLayout sizes a confirm/message dialog for the terminal and the
// wrapped message, returning the dialog width/height and the body view height.
// Width is the preferred width clamped to the terminal; height grows with the
// wrapped body up to messageMaxBodyRows, then the body scrolls. It is pure so
// the grow-with-content / clamp-to-terminal / scroll-on-overflow behaviour can
// be tested without a live event loop.
func messageDialogLayout(termW, termH int, message string) (width, height, bodyH int) {
	width = messageWidth
	if width > termW-2 {
		width = termW - 2
	}
	if width < messageMinWidth {
		width = messageMinWidth
	}
	if width > termW {
		width = termW
	}
	if width < 1 {
		width = 1
	}

	// The body spans width-4 columns (X:2 … width-3) and turbotui reserves the
	// last column for the scrollbar, so text wraps at width-5.
	wrapW := width - 5
	if wrapW < 1 {
		wrapW = 1
	}
	rows := 0
	for _, line := range strings.Split(message, "\n") {
		rows += wrapRowCount(line, wrapW)
	}
	if rows < 1 {
		rows = 1
	}
	bodyH = rows
	if bodyH > messageMaxBodyRows {
		bodyH = messageMaxBodyRows
	}

	// height = 2 borders + top pad + body + 1 gap + 1 button row + 1 bottom pad.
	height = bodyH + 6
	if maxH := termH - 2; height > maxH {
		height = maxH
	}
	if height < 1 {
		height = 1
	}
	if height > termH {
		height = termH
	}

	// Derive the body height from the final (possibly clamped) dialog height.
	bodyH = height - 6
	if bodyH < 1 {
		bodyH = 1
	}
	return width, height, bodyH
}

// showConfirm opens a modal confirm/message dialog. When onResult is non-nil the
// dialog asks Yes/No and reports the choice (Escape counts as No); when it is nil
// the dialog is purely informational and shows a single OK button. Unlike the
// turbotui helper it replaces, the message wraps to the dialog width and scrolls
// when it overflows, and it renders with the dialog's own high-contrast palette
// (issue #98).
func (w *Workbench) showConfirm(title, message string, onResult func(bool)) {
	width, height, bodyH := messageDialogLayout(w.app.Width(), w.app.Height(), message)
	x, y := centeredDialog(w, width, height)

	dialog := tv.NewDialog(title, x, y, width, height)
	applyWindowShadow(dialog.Window) // honour the NoShadow theme setting (issue #215)
	dialog.Window.ShowClose = false

	body := tv.NewTextView("", tv.Rect{X: 2, Y: 1, W: width - 4, H: bodyH})
	body.Wrap = true
	body.FG = tv.DefaultTheme.DialogFG
	body.BG = tv.DefaultTheme.DialogBG
	body.FocusFG = tv.DefaultTheme.MnemonicFG
	for _, line := range strings.Split(message, "\n") {
		body.AddLine(line)
	}
	// An informational message reads top-down, so open at the first line (issue #174).
	body.ScrollToTop()
	dialog.Window.AddContent(body)

	var layer *tv.Layer
	dismiss := func(value bool) {
		w.desktop.RemoveLayer(layer)
		if onResult != nil {
			onResult(value)
		}
	}

	btnY := bodyH + 2
	var focus *tv.Button
	if onResult == nil {
		// Informational message: a single OK button (Escape also dismisses).
		ok := newButton("OK", centeredButton(width, btnY, "OK"), func() { dismiss(false) })
		dialog.Window.AddContent(ok)
		focus = ok
	} else {
		yesRect, noRect := confirmButtonRow(width, btnY)
		yes := newButton("Yes", yesRect, func() { dismiss(true) })
		no := newButton("No", noRect, func() { dismiss(false) })
		dialog.Window.AddContent(yes)
		dialog.Window.AddContent(no)
		focus = no // default to the safe choice
	}

	// Escape dismisses the dialog (counts as "No" for a confirmation).
	dialog.Root().OnTypeFn = func(_ *tv.VisualComponent, event tui.TypeEvent) bool {
		if event.Key == tui.KeyEscape {
			dismiss(false)
			return true
		}
		return false
	}

	layer = tv.NewModalLayer("confirm-dialog", dialog)
	w.desktop.AddLayer(layer)
	w.desktop.SetFocus(focus)
}

// confirmButtonRow centres a "Yes"/"No" button pair on row btnY across a dialog
// of the given width, sizing each to its rendered label width. The pair is
// clamped to the content margins so it never escapes a narrow dialog.
func confirmButtonRow(width, btnY int) (yes, no tv.Rect) {
	const gap = 4
	yesW := buttonLabelWidth("Yes")
	noW := buttonLabelWidth("No")
	total := yesW + gap + noW
	startX := (width - total) / 2
	if startX < 2 {
		startX = 2
	}
	leftX, rightX := 2, width-3
	yes = clampDialogRect(tv.Rect{X: startX, Y: btnY, W: yesW, H: 1}, leftX, rightX)
	no = clampDialogRect(tv.Rect{X: startX + yesW + gap, Y: btnY, W: noW, H: 1}, leftX, rightX)
	return yes, no
}

// centeredButton centres a single button labelled by label on row btnY.
func centeredButton(width, btnY int, label string) tv.Rect {
	w := buttonLabelWidth(label)
	x := (width - w) / 2
	if x < 2 {
		x = 2
	}
	return clampDialogRect(tv.Rect{X: x, Y: btnY, W: w, H: 1}, 2, width-3)
}
