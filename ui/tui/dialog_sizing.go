package ui

import (
	tv "github.com/hobbestherat/turbotui/turbotv"
)

// dialogRect resolves a centered dialog rectangle for spec against the current
// terminal size. It is gogent's single dialog-sizing entry point (issue #299):
// it forwards the live screen dimensions to turbotui's ResolveDialogRect, which
// owns the policy — dialogs default LARGE (≈80% wide / 85% tall), grow with the
// terminal and shrink only when the spec's content size or an explicit cap
// demands it. The house style (margin 2, the 80/85% defaults) is turbotui's
// default, so the wrapper only supplies screenW/screenH.
//
// It replaced the per-dialog sizing helpers (centeredDialog, messageDialogLayout,
// permissionDialogLayout, resourcesDialogSize, sessionsDialogSize,
// statisticsDialogSize, paletteSize, helpSize) and the inline centering math, so
// every dialog shares one terminal-aware policy instead of its own magic numbers.
func (w *Workbench) dialogRect(spec tv.DialogSpec) (x, y, wid, h int) {
	return tv.ResolveDialogRect(spec, w.app.Width(), w.app.Height())
}

// installResizeReflow makes an open dialog re-resolve against the CURRENT terminal
// on every resize by recomputing its spec from scratch via specFn, then applying
// the new centered bounds. Dialogs whose spec encodes the terminal dimensions —
// the confirm/message and permission content sizes, and the browsers' percentage
// widths — must use this instead of dialog.Fit, because Fit remembers the
// open-time spec and would pin the dialog to the terminal it was opened on (a
// confirm opened on 80×24 then grown to 200×50 would stay capped at the stale
// MaxH=22 rather than growing to ~85% of the new screen). Static-spec dialogs —
// those whose spec is pure Min/Max floors — stay on dialog.Fit, which is already
// path-independent. This mirrors the agent-monologue window's own resize hook
// (issue #299).
// browserDialogSpec is the shared sizing intent of the three two-pane read-only
// browsers (Resources, Saved Sessions, Statistics): large by default at ≈85% of
// the terminal width with a 60×14 floor so each stays usable on a small terminal.
// PreferredW is a share of the *current* terminal, so it is recomputed (via this
// method as the specFn) on every resize rather than baked at open time (issue
// #299). The list/detail split is derived from the resolved width, so the panes
// grow with the dialog.
func (w *Workbench) browserDialogSpec() tv.DialogSpec {
	return tv.DialogSpec{MinW: 60, MinH: 14, PreferredW: w.app.Width() * 85 / 100}
}

func installResizeReflow(desktop *tv.Desktop, dialog *tv.Dialog, layer *tv.Layer, specFn func() tv.DialogSpec) {
	layer.OnResize = func(tv.Rect) {
		app := desktop.App()
		x, y, w, h := tv.ResolveDialogRect(specFn(), app.Width(), app.Height())
		dialog.Window.Component.SetBounds(tv.Rect{X: x, Y: y, W: w, H: h})
	}
}
