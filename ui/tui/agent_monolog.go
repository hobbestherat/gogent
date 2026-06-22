package ui

import (
	tui "github.com/hobbestherat/turbotui"
	tv "github.com/hobbestherat/turbotui/turbotv"
)

// showAgentMonolog opens a popup window showing a sub-agent's internal monologue
// (its full message transcript), reusing the same foldable TextView used for the
// main chat history. Only one monologue popup exists at a time; opening another
// replaces it. The window is draggable and closable like any session window.
func (w *Workbench) showAgentMonolog(sessionID, agentID, name string) {
	if w.handlers.GetTranscript == nil {
		return
	}
	msgs := w.handlers.GetTranscript(sessionID, agentID)

	// Replace any existing monologue popup.
	if w.monolog != nil {
		w.desktop.RemoveLayer(w.monolog)
		w.monolog = nil
	}

	title := name
	if title == "" {
		title = agentID
	}
	// Large by default (≈80%×85% of the terminal) with a 40×10 floor; the history
	// view fills the window, so it grows with the terminal (issue #299).
	spec := tv.DialogSpec{MinW: 40, MinH: 10}
	x, y, width, height := w.dialogRect(spec)

	window := tv.NewWindow("monologue: "+title, tv.Rect{X: x, Y: y, W: width, H: height}, tui.LineSingle)
	applyWindowShadow(window) // honour the NoShadow theme setting (issue #215)
	history := tv.NewTextView("", tv.Rect{})
	history.Wrap = true
	if len(msgs) == 0 {
		history.AddColored("(no transcript yet)", colorNote)
	} else {
		renderTranscript(history, msgs)
	}
	window.AddContent(history)
	window.Content.LayoutFn = func(c *tv.VisualComponent) {
		history.Component.SetBounds(tv.Rect{X: 0, Y: 0, W: c.Bounds.W, H: c.Bounds.H})
	}

	layer := tv.NewWindowLayer("monolog", window)
	// Re-resolve the popup against the new terminal size on resize so it stays
	// ≈80%×85% instead of a fixed box; the content LayoutFn refills the history
	// view to the new bounds (issue #299).
	layer.OnResize = func(tv.Rect) {
		nx, ny, nw, nh := w.dialogRect(spec)
		window.Component.SetBounds(tv.Rect{X: nx, Y: ny, W: nw, H: nh})
	}
	window.OnClose = func(_ *tv.Window) {
		w.desktop.RemoveLayer(layer)
		if w.monolog == layer {
			w.monolog = nil
		}
	}
	w.monolog = layer
	w.desktop.AddLayer(layer)
	w.desktop.SetFocus(history)
	w.desktop.Redraw()
}
