package ui

import (
	"strings"

	tui "github.com/hobbestherat/turbotui"
	tv "github.com/hobbestherat/turbotui/turbotv"
)

// Sizing knobs for the confirm/message dialog (issue #299). The dialog is large
// by default (≈80%×85% of the terminal) and only grows past that when the message
// itself is wider/taller, up to the terminal edge — the cramping 12-row body cap
// is gone, so a long message is bounded only by the screen and scrolls beyond it.
const (
	// messageMinWidth keeps a tiny confirmation legible on a small terminal.
	messageMinWidth = 30
	// messagePad is the chrome around the body text: 2 borders + 2 content
	// margins, so PreferredW = longest line + messagePad shows the widest line in
	// full before the body word-wraps.
	messagePad = 4
	// messageChrome is the non-body vertical cost: 2 borders + top pad + 1 gap +
	// 1 button row + 1 bottom pad. bodyH = height - messageChrome.
	messageChrome = 6
)

// messageBodyRows reports how many display rows the message occupies when wrapped
// inside a dialog of the given outer width, matching what the body TextView
// renders: each hard line wraps independently at width-5 (the body spans width-4
// columns and turbotui reserves the last for the scrollbar). It delegates to
// turbotui's WrapText so the prediction can never drift from the real render.
func messageBodyRows(message string, width int) int {
	wrapW := width - 5
	if wrapW < 1 {
		wrapW = 1
	}
	rows := 0
	for _, line := range strings.Split(message, "\n") {
		rows += len(tv.WrapText(line, wrapW))
	}
	if rows < 1 {
		rows = 1
	}
	return rows
}

// messageDialogSpec turns a confirm/message into a terminal-aware DialogSpec: at
// least messageMinWidth wide and wide enough to show the longest line in full
// (PreferredW), large by default and grown vertically with the wrapped body
// (PrefH) up to the terminal edge (MaxH, replacing the old 12-row cap). It is
// pure so the sizing can be tested without a live event loop. The body height the
// dialog ends up with is derived from the resolved height via messageBodyHeight.
func messageDialogSpec(termW, termH int, message string) tv.DialogSpec {
	prefW := tv.LongestLineWidth(message) + messagePad
	// Resolve the width first (height does not affect it) so the body-row count
	// is measured against the real dialog width.
	_, _, width, _ := tv.ResolveDialogRect(tv.DialogSpec{MinW: messageMinWidth, PreferredW: prefW}, termW, termH)
	return tv.DialogSpec{
		MinW:       messageMinWidth,
		PreferredW: prefW,
		PrefH:      messageBodyRows(message, width) + messageChrome,
		MaxH:       termH - 2,
	}
}

// messageBodyHeight is the body TextView's row count for a resolved dialog
// height: the height minus the fixed chrome, floored at one visible row.
func messageBodyHeight(height int) int {
	bodyH := height - messageChrome
	if bodyH < 1 {
		bodyH = 1
	}
	return bodyH
}

// showConfirm opens a modal confirm/message dialog. When onResult is non-nil the
// dialog asks Yes/No and reports the choice (Escape counts as No); when it is nil
// the dialog is purely informational and shows a single OK button. Unlike the
// turbotui helper it replaces, the message wraps to the dialog width and scrolls
// when it overflows, and it renders with the dialog's own high-contrast palette
// (issue #98).
func (w *Workbench) showConfirm(title, message string, onResult func(bool)) {
	spec := messageDialogSpec(w.app.Width(), w.app.Height(), message)
	x, y, width, height := w.dialogRect(spec)
	bodyH := messageBodyHeight(height)

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
	// The spec encodes the open-time terminal (content-driven width/height), so
	// re-resolve against the live terminal on resize rather than the stale spec
	// dialog.Fit would remember (issue #299).
	installResizeReflow(w.desktop, dialog, layer, func() tv.DialogSpec {
		return messageDialogSpec(w.app.Width(), w.app.Height(), message)
	})
	w.desktop.SetFocus(focus)
}

// confirmButtonRow centres a "Yes"/"No" button pair on row btnY across a dialog
// of the given width, sizing each to its rendered label width. The pair is
// clamped to the content margins so it never escapes a narrow dialog.
func confirmButtonRow(width, btnY int) (yes, no tv.Rect) {
	const gap = 4
	yesW := tv.ButtonLabelWidth("Yes")
	noW := tv.ButtonLabelWidth("No")
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
	w := tv.ButtonLabelWidth(label)
	x := (width - w) / 2
	if x < 2 {
		x = 2
	}
	return clampDialogRect(tv.Rect{X: x, Y: btnY, W: w, H: 1}, 2, width-3)
}
