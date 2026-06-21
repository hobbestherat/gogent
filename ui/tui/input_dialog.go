package ui

import (
	tui "github.com/hobbestherat/turbotui"
	tv "github.com/hobbestherat/turbotui/turbotv"
)

// showInputDialog opens a modal prompt with a single-line text field pre-filled
// with initial. onResult receives the edited value and true when the user
// accepts (OK or Enter), or "" and false when cancelled (Cancel, Escape, or the
// window's close button). It mirrors tv.ShowConfirmYesNo's shape so callers can
// treat it as an asynchronous prompt.
func (w *Workbench) showInputDialog(title, label, initial string, onResult func(value string, ok bool)) {
	const width = 54
	const height = 8
	x, y := centeredDialog(w, width, height)

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
	w.desktop.SetFocus(box)
}
