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

// browserDialogSpec is the shared sizing intent of the two-pane read-only browsers
// that still fill the screen (Resources, Statistics): large by default at ≈85% of
// the terminal width with a 60×14 floor so each stays usable on a small terminal.
// PreferredW is a share of the *current* terminal, so it is recomputed (via this
// method as the specFn) on every resize rather than baked at open time (issue
// #299). The list/detail split is derived from the resolved width, so the panes
// grow with the dialog.
func (w *Workbench) browserDialogSpec() tv.DialogSpec {
	return tv.DialogSpec{MinW: 60, MinH: 14, PreferredW: w.app.Width() * 85 / 100}
}

// sessionsDialogSpec is the content-driven size of the Saved Sessions browser
// (issues #322, #338). It expresses a content footprint rather than a share of
// the terminal, but the footprint must match what the list actually draws: a full
// formatSessionRow is ~50 cols (title sessionRowTitleWidth=26 + space + date 16 +
// " Nt Nm" counts; ~62 with the "(archived)" suffix), and the list pane is
// width/2-2, so the dialog needs PreferredW ≈ 2*50+4 ≈ 104 for a row to fit
// without the Tree's "…" truncation (#338 — the prior 90 left the list at 43 cols,
// always clipped). MaxW 160 / MaxH 40 let it use a wide terminal; PrefH 26 shows
// ~17 rows instead of ~11. The content-driven Preferred + caps keep it off the old
// 80%×85% balloon (#322). The 60×14 floor keeps it usable on a small terminal. The
// spec is static (path-independent), so the dialog uses dialog.Fit on resize.
func (w *Workbench) sessionsDialogSpec() tv.DialogSpec {
	return tv.DialogSpec{
		MinW: 60, MaxW: 160, PreferredW: 104,
		MinH: 14, MaxH: 40, PrefH: 26,
	}
}

// installResizeReflow makes an open dialog re-resolve against the CURRENT terminal
// on every resize by recomputing its spec from scratch via specFn, then applying
// the new centered bounds. Dialogs whose spec encodes the terminal dimensions —
// the confirm/message and permission content sizes, and browserDialogSpec's
// percentage width — must use this instead of dialog.Fit, because Fit remembers the
// open-time spec and would pin the dialog to the terminal it was opened on (a
// confirm opened on 80×24 then grown to 200×50 would stay capped at the stale
// MaxH=22 rather than growing to ~85% of the new screen). Static-spec dialogs —
// those whose spec is pure Min/Max floors or a fixed content size (e.g.
// sessionsDialogSpec) — stay on dialog.Fit, which is already path-independent. This
// mirrors the agent-monologue window's own resize hook (issue #299).
func installResizeReflow(desktop *tv.Desktop, dialog *tv.Dialog, layer *tv.Layer, specFn func() tv.DialogSpec) {
	layer.OnResize = func(tv.Rect) {
		app := desktop.App()
		x, y, w, h := tv.ResolveDialogRect(specFn(), app.Width(), app.Height())
		dialog.Window.Component.SetBounds(tv.Rect{X: x, Y: y, W: w, H: h})
	}
}
