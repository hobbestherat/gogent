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
		w.monologWindow = nil
	}

	title := name
	if title == "" {
		title = agentID
	}
	// Large by default (≈80%×85% of the terminal) with a 40×10 floor; the history
	// view fills the window, so it grows with the terminal (issue #299). A 120-column
	// MaxW keeps the transcript readable on an ultrawide terminal (issue #317) while it
	// still grows tall (no height cap; a transcript wants the vertical space).
	spec := tv.DialogSpec{MinW: 40, MaxW: 120, MinH: 10}
	// Center the popup on the pinned window area (left of the sidebar), not the full
	// screen, so it opens clear of the "Sessions & Agents" panel — mirroring how
	// session windows are sized against the window area (issue #319). windowArea() is
	// the full screen when the sidebar is unpinned, so the unpinned case is unchanged.
	area := w.windowArea()
	x, y, width, height := tv.ResolveDialogRect(spec, area.W, area.H)
	x += area.X
	y += area.Y

	window := tv.NewWindow("monologue: "+title, tv.Rect{X: x, Y: y, W: width, H: height}, tui.LineSingle)
	applyWindowShadow(window) // honour the NoShadow theme setting (issue #215)
	// Constrain drag/resize to the pinned sidebar area like a session window does
	// (issue #319); installed before AddLayer so the very first interaction is
	// clamped. A no-op while the sidebar is unpinned.
	w.installSidebarClampOn(window)
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
		// Re-resolve against the (possibly resized) window area so the popup stays
		// ≈80%×85% and clear of the pinned sidebar (issue #319).
		a := w.windowArea()
		nx, ny, nw, nh := tv.ResolveDialogRect(spec, a.W, a.H)
		window.Component.SetBounds(tv.Rect{X: nx + a.X, Y: ny + a.Y, W: nw, H: nh})
	}
	window.OnClose = func(_ *tv.Window) {
		w.desktop.RemoveLayer(layer)
		if w.monolog == layer {
			w.monolog = nil
			w.monologWindow = nil
		}
	}
	w.monolog = layer
	w.monologWindow = window
	w.desktop.AddLayer(layer)
	w.desktop.SetFocus(history)
	w.desktop.Redraw()
}
