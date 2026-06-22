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
