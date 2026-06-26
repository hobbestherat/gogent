package ui

import (
	"strings"

	tui "github.com/hobbestherat/turbotui"
	tv "github.com/hobbestherat/turbotui/turbotv"
)

// Sizing knobs for the confirm/message dialog (issues #299, #309). The dialog is
// sized to its message — wide enough for the longest line, tall enough for the
// wrapped body — and only grows toward the MaxW/MaxH cap when the message is long.
// A short confirmation ("Are you sure?") therefore stays compact (≈30×7) instead
// of inflating to the percentage default, while a long message grows to the cap
// and scrolls beyond it.
const (
	// messageMinWidth keeps a tiny confirmation legible on a small terminal.
	messageMinWidth = 30
	// messageMaxWidth caps how wide a long message grows before it word-wraps, so
	// even a wide message stays a readable column rather than spanning the screen.
	messageMaxWidth = 80
	// messageMaxHeight caps how tall a long message grows before the body scrolls,
	// so a wall of text does not fill the terminal (issue #309).
	messageMaxHeight = 24
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

// messageDialogSpec turns a confirm/message into a content-driven DialogSpec: at
// least messageMinWidth wide and wide enough to show the longest line in full
// (PreferredW), capped at messageMaxWidth; tall enough for the wrapped body
// (PrefH), capped at messageMaxHeight. The caps replace the old terminal-baked
// MaxH=termH-2 — which left the height path-dependent on resize (issue #309) — so
// a short message stays compact and only a long one grows toward the cap and
// scrolls. It is pure so the sizing can be tested without a live event loop; the
// body height the dialog ends up with is derived from the resolved height via
// messageBodyHeight.
func messageDialogSpec(termW, termH int, message string) tv.DialogSpec {
	prefW := tv.LongestLineWidth(message) + messagePad
	// Resolve the width first (height does not affect it) so the body-row count
	// is measured against the real dialog width.
	_, _, width, _ := tv.ResolveDialogRect(
		tv.DialogSpec{MinW: messageMinWidth, MaxW: messageMaxWidth, PreferredW: prefW}, termW, termH)
	return tv.DialogSpec{
		MinW:       messageMinWidth,
		MaxW:       messageMaxWidth,
		PreferredW: prefW,
		PrefH:      messageBodyRows(message, width) + messageChrome,
		MaxH:       messageMaxHeight,
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

// newMessageLayer builds and registers the modal dialog scaffold shared by every
// message dialog: a content-sized dialog (issues #299/#309), its wrapped,
// top-scrolled body TextView, and the modal layer with resize-reflow installed
// (NoEnterGrace, since these appear in direct response to a user action). The
// caller adds its own buttons / Escape handler / focus. It returns the dialog, the
// live layer, and the resolved content width + body height the caller needs to
// place controls. Centralising layer creation here keeps a single source of truth
// for it across showConfirm and showProgress (issue #478).
func (w *Workbench) newMessageLayer(title, message, layerName string) (*tv.Dialog, *tv.Layer, int, int) {
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

	layer := tv.NewModalLayer(layerName, dialog)
	// User-initiated dialog: it appears in direct response to a user action and so
	// cannot interrupt mid-keystroke. Opt out of the modal Enter-grace (issue #347,
	// which scopes the grace to background-triggered modals) so a deliberate Enter
	// on Yes/No/OK activates immediately.
	layer.NoEnterGrace = true
	w.desktop.AddLayer(layer)
	// The spec's PrefH is measured against the open-time width, so re-resolve
	// against the live terminal on resize rather than the stale spec dialog.Fit
	// would remember (issues #299, #309).
	installResizeReflow(w.desktop, dialog, layer, func() tv.DialogSpec {
		return messageDialogSpec(w.app.Width(), w.app.Height(), message)
	})
	return dialog, layer, width, bodyH
}

// showConfirm opens a modal confirm/message dialog. When onResult is non-nil the
// dialog asks Yes/No and reports the choice (Escape counts as No); when it is nil
// the dialog is purely informational and shows a single OK button. Unlike the
// turbotui helper it replaces, the message wraps to the dialog width and scrolls
// when it overflows, and it renders with the dialog's own high-contrast palette
// (issue #98).
func (w *Workbench) showConfirm(title, message string, onResult func(bool)) {
	dialog, layer, width, bodyH := w.newMessageLayer(title, message, "confirm-dialog")

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

	w.desktop.SetFocus(focus)
}

// showProgress opens a non-dismissable informational modal used as interim
// feedback for a background operation (the daemon handoff). Unlike showConfirm it
// has NO buttons and NO Escape handler: it blocks input while the operation runs
// and is torn down programmatically (RemoveLayer) when the result is ready, so the
// result dialog REPLACES it rather than stacking on top (issue #478). It returns
// the layer so the caller can dismiss it. Mirrors the disconnect modal's
// programmatic-only lifecycle (disconnect_modal.go). The distinct layer name keeps
// the progress modal unambiguously identifiable.
func (w *Workbench) showProgress(title, message string) *tv.Layer {
	_, layer, _, _ := w.newMessageLayer(title, message, "daemon-progress")
	// No focusable control; the modal swallows input until it is replaced. SetFocus
	// is nil-safe, and RemoveLayer restores the pre-modal focus on teardown.
	w.desktop.SetFocus(nil)
	return layer
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
