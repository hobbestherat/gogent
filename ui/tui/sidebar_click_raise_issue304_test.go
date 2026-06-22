package ui

import (
	"testing"

	"gogent/internal/config"
)

// Issue #304: clicking a background session window raises it entirely inside
// turbotui (Desktop.raiseLayer), without routing through Workbench.Focus. Before
// the OnActiveLayerChange hook, the sidebar highlight only moved on
// Workbench-initiated activations, so a toolkit-driven raise left the highlight
// stranded on the previously-active session until the busy/event tickers caught
// up. NewWorkbench now registers desktop.OnActiveLayerChange so every activation
// path — including the one the toolkit drives on its own — re-syncs the derived
// active-session state through refreshOverall -> focusSession(ActiveID()).
//
// RaiseLayer here stands in for the click-to-raise path: both call the desktop's
// notifyActiveLayerChange after updating the top of the z-stack, which fires the
// registered hook. The test asserts the sidebar highlight follows that raise
// without any Workbench.Focus call.
func TestSidebarHighlightFollowsToolkitRaise(t *testing.T) {
	w := NewWorkbench([]*config.ModelConfig{{Name: "m", Model: "m"}})
	w.openWindow("a", "Alpha")
	w.openWindow("b", "Bravo")

	// Bravo was added last, so it is on top and holds the highlight.
	if got := w.sidebar.tree.Selected(); got != w.sidebar.sessions["b"] {
		t.Fatalf("after opening Bravo, highlight = %v, want Bravo node", got)
	}

	// Raise Alpha straight through the desktop (the toolkit click-to-raise path),
	// NOT through Workbench.Focus. The OnActiveLayerChange hook must move the
	// highlight to Alpha.
	w.desktop.RaiseLayer(w.sessions["a"].layer)
	if got := w.sidebar.tree.Selected(); got != w.sidebar.sessions["a"] {
		t.Fatalf("after toolkit-raising Alpha, highlight = %v, want Alpha node", got)
	}
	if w.sidebar.focused != "a" {
		t.Fatalf("sidebar.focused = %q, want %q after raise", w.sidebar.focused, "a")
	}
	if got := w.ActiveID(); got != "a" {
		t.Fatalf("ActiveID() = %q, want %q after raise", got, "a")
	}

	// Raising the already-top layer is a no-op for the top of stack, so the
	// highlight stays put (the desktop dedupes and does not re-fire the hook).
	w.desktop.RaiseLayer(w.sessions["a"].layer)
	if got := w.sidebar.tree.Selected(); got != w.sidebar.sessions["a"] {
		t.Fatalf("re-raising Alpha changed highlight to %v, want Alpha node", got)
	}

	// Raise Bravo back; the highlight follows again.
	w.desktop.RaiseLayer(w.sessions["b"].layer)
	if got := w.sidebar.tree.Selected(); got != w.sidebar.sessions["b"] {
		t.Fatalf("after raising Bravo, highlight = %v, want Bravo node", got)
	}
}
