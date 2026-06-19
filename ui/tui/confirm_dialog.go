package ui

import (
	"strings"

	tui "github.com/hobbestherat/turbotui"
	tv "github.com/hobbestherat/turbotui/turbotv"
)

// Sizing knobs for the wrapping confirm dialog. Width grows with the terminal
// (clamped to [confirmMinWidth, confirmMaxWidth]); height grows with the wrapped
// message up to confirmMaxBodyRows, beyond which — and on short terminals — the
// body scrolls instead of overflowing. Same shape as the permission dialog.
const (
	confirmMinWidth    = 40
	confirmMaxWidth    = 72
	confirmMinHeight   = 7
	confirmMaxBodyRows = 12
)

// showConfirmDialog is gogent's wrapping replacement for tv.ShowConfirmYesNo. The
// library confirm paints its message with a non-wrapping Label, so a message
// longer than the dialog width is clipped — export paths and multi-line errors
// are the common victims (issue #98). This builds the same Yes/No modal but sizes
// it to the wrapped message and renders the body in a wrapping, scrollable
// TextView, so the whole message is always visible. Its signature matches the
// library helper, so call sites are a drop-in swap. Escape (and the
// default-focused "No") resolve false; "Yes" resolves true.
func showConfirmDialog(desktop *tv.Desktop, title, message string, onResult func(bool)) {
	if desktop == nil {
		return
	}
	width, height, bodyY, bodyH, btnY := confirmDialogLayout(desktop.App().Width(), desktop.App().Height(), message)
	x := (desktop.App().Width() - width) / 2
	y := (desktop.App().Height() - height) / 2
	if x < 0 {
		x = 0
	}
	if y < 0 {
		y = 0
	}

	dialog := tv.NewDialog(title, x, y, width, height)
	dialog.Window.ShowClose = false

	body := tv.NewTextView("", tv.Rect{X: 2, Y: bodyY, W: width - 4, H: bodyH})
	body.Wrap = true
	body.FG = tv.DefaultTheme.DialogFG
	body.BG = tv.DefaultTheme.DialogBG
	body.FocusFG = tv.DefaultTheme.MnemonicFG
	for _, line := range strings.Split(message, "\n") {
		body.AddColored(line, dialogTextFG(tv.DefaultTheme.DialogFG))
	}
	dialog.Window.AddContent(body)

	var layer *tv.Layer
	done := false
	finish := func(v bool) {
		if done {
			return
		}
		done = true
		desktop.RemoveLayer(layer)
		if onResult != nil {
			onResult(v)
		}
	}

	// Yes/No centred on the button row, sized to their rendered labels so the
	// pair never overlaps or escapes the dialog at any width.
	const gap = 2
	yesW := buttonLabelWidth("&Yes")
	noW := buttonLabelWidth("&No")
	leftX := 2 + (width-4-(yesW+gap+noW))/2
	if leftX < 2 {
		leftX = 2
	}
	yes := tv.NewButton("&Yes", tv.Rect{X: leftX, Y: btnY, W: yesW, H: 1}, func() {
		finish(true)
	})
	no := tv.NewButton("&No", tv.Rect{X: leftX + yesW + gap, Y: btnY, W: noW, H: 1}, func() {
		finish(false)
	})
	dialog.Window.AddContent(yes)
	dialog.Window.AddContent(no)

	// Escape anywhere cancels (counts as "No"), matching the library helper.
	dialog.Root().OnTypeFn = func(_ *tv.VisualComponent, event tui.TypeEvent) bool {
		if event.Key == tui.KeyEscape {
			finish(false)
			return true
		}
		return false
	}

	layer = tv.NewModalLayer("confirm-dialog", dialog)
	desktop.AddLayer(layer)
	desktop.SetFocus(no)
}

// confirmDialogLayout sizes the confirm dialog for the terminal and its message
// and returns the body's content-relative origin/height plus the button row Y. It
// mirrors permissionDialogLayout's shape — grow with content, clamp to the
// terminal, scroll on overflow — so a long message fits when it can and scrolls
// when it cannot. Pure, so the sizing can be tested without a live event loop.
func confirmDialogLayout(termW, termH int, message string) (width, height, bodyY, bodyH, btnY int) {
	width = termW - 2
	if width > confirmMaxWidth {
		width = confirmMaxWidth
	}
	if width < confirmMinWidth {
		width = confirmMinWidth
	}
	if width > termW {
		width = termW
	}
	if width < 1 {
		width = 1
	}

	// Effective text columns inside the body: it spans width-4 (X:2 .. width-3)
	// and turbotui reserves its last column for the scrollbar.
	wrapW := width - 5
	if wrapW < 1 {
		wrapW = 1
	}
	contentRows := 0
	for _, line := range strings.Split(message, "\n") {
		contentRows += wrapRowCount(line, wrapW)
	}
	if contentRows < 1 {
		contentRows = 1
	}

	bodyY = 1
	desiredBody := contentRows
	if desiredBody > confirmMaxBodyRows {
		desiredBody = confirmMaxBodyRows
	}

	// height = 2 borders + topPad + body + 1 gap + 1 button row + 1 bottom pad.
	height = bodyY + desiredBody + 5
	if max := termH - 2; height > max {
		height = max
	}
	if height < confirmMinHeight {
		height = confirmMinHeight
	}
	if height > termH {
		height = termH
	}

	// Derive the body height from the final (possibly clamped) dialog height.
	bodyH = height - bodyY - 5
	if bodyH < 1 {
		bodyH = 1
	}
	btnY = bodyY + bodyH + 1
	return width, height, bodyY, bodyH, btnY
}
