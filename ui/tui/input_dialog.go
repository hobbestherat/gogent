package ui

import (
	tui "github.com/hobbestherat/turbotui"
	tv "github.com/hobbestherat/turbotui/turbotv"
)

// Sizing knobs for the single-field input dialog (issues #299, #309).
const (
	// inputMinWidth keeps a tiny prompt usable on a small terminal.
	inputMinWidth = 40
	// inputMaxWidth caps the width so a long label does not stretch the box to the
	// full terminal width.
	inputMaxWidth = 80
	// inputDialogHeight pins the height to the fixed layout — a prompt row, the
	// field, a gap and the button row — so the box never grows vertically: there is
	// only ever one line of input.
	inputDialogHeight = 7
)

// inputDialogConfig holds the optional behaviour of showInputDialog, populated
// by the variadic inputDialogOption arguments.
type inputDialogConfig struct {
	// selectAll highlights the whole initial value on open so the first
	// printable keystroke replaces it (select-on-open, issue #235).
	selectAll bool
}

// inputDialogOption tweaks an showInputDialog invocation. Options keep the common
// call shape (title, label, initial, onResult) unchanged for callers that want the
// default behaviour, so adding new knobs never churns existing call sites.
type inputDialogOption func(*inputDialogConfig)

// withSelectAll makes the dialog open with the entire initial value selected, so
// the first printable keystroke replaces the whole text and an arrow / Home / End
// collapses the selection for normal in-place editing. Intended for "rename"-style
// prompts where retyping is the common case (issue #235); leave it off when the
// user is usually refining an existing value (search query, goal).
func withSelectAll() inputDialogOption {
	return func(c *inputDialogConfig) { c.selectAll = true }
}

// showInputDialog opens a modal prompt with a single-line text field pre-filled
// with initial. onResult receives the edited value and true when the user
// accepts (OK or Enter), or "" and false when cancelled (Cancel, Escape, or the
// window's close button). It mirrors tv.ShowConfirmYesNo's shape so callers can
// treat it as an asynchronous prompt. Optional behaviour (e.g. withSelectAll) is
// supplied via opts; with no options it behaves as a plain edit-in-place prompt.
func (w *Workbench) showInputDialog(title, label, initial string, onResult func(value string, ok bool), opts ...inputDialogOption) {
	var cfg inputDialogConfig
	for _, opt := range opts {
		opt(&cfg)
	}
	// A single label + one text field needs little room: PreferredW grows to show
	// the label in full (down to inputMinWidth, up to inputMaxWidth) and the height
	// is pinned to the fixed one-field layout, so a rename box is ~40×7 rather than
	// inflating to the percentage default (issues #299, #309).
	spec := tv.DialogSpec{
		MinW:       inputMinWidth,
		MaxW:       inputMaxWidth,
		MinH:       inputDialogHeight,
		MaxH:       inputDialogHeight,
		PreferredW: tui.StringWidth(label) + 4,
	}
	x, y, width, height := w.dialogRect(spec)

	dialog := tv.NewDialog(title, x, y, width, height)
	applyWindowShadow(dialog.Window) // honour the NoShadow theme setting (issue #215)
	dialog.Window.ShowClose = false

	prompt := dialogLabel(label, tv.Rect{X: 2, Y: 1, W: width - 4, H: 1})
	box := tv.NewTextBox(initial, tv.Rect{X: 2, Y: 2, W: width - 4, H: 1})

	var layer *tv.Layer
	finish := func(value string, ok bool) {
		w.desktop.RemoveLayer(layer)
		if onResult != nil {
			onResult(value, ok)
		}
	}
	// Enter inside the field accepts; Tab/arrow-nav still work via the desktop.
	box.OnSubmit = func() { finish(box.GetText(), true) }
	ok := newButton("OK", tv.Rect{X: width - 24, Y: height - 3, W: 9, H: 1}, func() {
		finish(box.GetText(), true)
	})
	cancel := newButton("Cancel", tv.Rect{X: width - 13, Y: height - 3, W: 10, H: 1}, func() {
		finish("", false)
	})

	dialog.Window.AddContent(prompt)
	dialog.Window.AddContent(box)
	dialog.Window.AddContent(ok)
	dialog.Window.AddContent(cancel)

	// Escape anywhere in the dialog cancels without applying.
	dialog.Root().OnTypeFn = func(_ *tv.VisualComponent, event tui.TypeEvent) bool {
		if event.Key == tui.KeyEscape {
			finish("", false)
			return true
		}
		return false
	}

	layer = tv.NewModalLayer("input-dialog", dialog)
	w.desktop.AddLayer(layer)
	dialog.Fit(spec) // re-resolve the rect when the terminal is resized (issue #299)
	w.desktop.SetFocus(box)
	if cfg.selectAll {
		// After focus, so the highlight is the field's final state on open.
		selectAllTextBox(box)
	}
}

// selectAllTextBox highlights the whole current contents of box so the first
// printable keystroke replaces it — the select-on-open rename affordance
// (issue #235). turbotui's TextBox has no exported selection setter yet (tracked
// upstream as turbotui#11); until it does, we drive the widget's own, already
// tested Ctrl+A select-all through its public type handler. Routing through the
// real handler keeps the behaviour in lockstep with the widget: a printable key
// runs deleteSelection() before inserting (replace), while Right / End / Home
// collapse the selection for normal in-place editing. Empty text is a no-op
// (Ctrl+A on no content leaves an empty, inert selection).
func selectAllTextBox(box *tv.TextBox) {
	if box == nil || box.Component == nil || box.Component.OnTypeFn == nil {
		return
	}
	box.Component.OnTypeFn(box.Component, tui.TypeEvent{Key: tui.KeyRune, Rune: 'a', Ctrl: true})
}
