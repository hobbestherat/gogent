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

// browserDialogSpec is the sizing intent of the Resources browser: a two-pane
// read-only browser that fills the screen up to a comfortable cap, with a 60×14
// floor so it stays usable on a small terminal. MaxW caps it at
// comfortableMaxWidth (120) so it no longer balloons toward ~650 cols on a very
// wide terminal (issue #552); below ~150 cols the cap is inert and the dialog
// still grows with the terminal. The PreferredW share is recomputed (via this
// method as the specFn) on every resize rather than baked at open time (issue
// #299); it is already functionally dead — turbotui clamps the 85% request down
// to its 80% percentage default (issue #309), and the new MaxW is the binding
// constraint above 120 — but it is kept so resolved widths track the terminal
// below the cap. The list/detail split is derived from the resolved width, so the
// panes grow with the dialog. (The Statistics dialog used to share this spec but
// now has its own content-driven statisticsDialogSpec — issue #345.)
func (w *Workbench) browserDialogSpec() tv.DialogSpec {
	return tv.DialogSpec{MinW: 60, MaxW: comfortableMaxWidth, MinH: 14, PreferredW: w.app.Width() * 85 / 100}
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

// commandsDialogSpec is the content-driven size of the Custom Commands editor
// (issues #448, #455). Like watchersDialogSpec it expresses a content footprint
// rather than a share of the terminal: a command list beside a detail form that
// holds a name/desc/template/model/agent set of fields, a parameter sub-editor and
// a live preview. PreferredW 112 widens the detail pane (detailW = width-28) so the
// form and preview are readable. PrefH 34 / MaxH 40 (raised from 28/34 in #455)
// give the multi-line template editor (now a 6-row MultiLineInput) the vertical
// room a single-line TextBox did not need, while staying under the 80%×85% balloon
// (MaxH 40 < 42). At the MinH 26 floor the preview collapses to its 2-row minimum
// (previewH = height-24) but the template still shows its full 6 rows, so the floor
// stays usable on a small terminal. MaxW 140 allows growth on a wide terminal
// without sprawling; MinW is raised to the footer's measured width if that is
// larger, so the buttons never overlap. The spec is static (no terminal-share
// term), so the dialog uses dialog.Fit on resize, like sessionsDialogSpec.
func (w *Workbench) commandsDialogSpec() tv.DialogSpec {
	spec := tv.DialogSpec{
		MinW: 84, MaxW: 140, PreferredW: 112,
		MinH: 26, MaxH: 40, PrefH: 34,
	}
	if need := footerRowMinWidth(commandsFooterLabels, tv.DefaultButtonGap); spec.MinW < need {
		spec.MinW = need
	}
	return spec
}

// keybindingsDialogSpec is the content-driven size of the Customize Keybindings
// modal (issue #461). Like sessionsDialogSpec/commandsDialogSpec it expresses a
// content footprint rather than a share of the terminal: a single-column list of
// "name  chord  (tag)" rows whose inner width is ~54 cells, so PreferredW 62 (54 +
// dialog chrome, with headroom for the 14-cell chord column and longer/translated
// action names) settles well under the 80% balloon the prior inline spec fell into
// (it left PreferredW/MaxW/PrefH at 0, so ResolveDialogRect defaulted to 80%×85% —
// 160 cols on a 200-wide terminal). MaxW 76 is an inert ceiling at the default
// catalog that only bites to stop sprawl on an ultrawide terminal. PrefH 34 settles
// the height (≈27 visible rows) and MaxH 40 caps it, replacing the old MaxH=rows+9
// which ballooned to ~85% with the ~55-row catalog. MinW is floored at the footer's
// measured width so the &Reset / Reset &All / Close buttons can never overlap
// (mirroring commandsDialogSpec). The spec is static (no terminal-share term), so
// the dialog uses dialog.Fit on resize.
func (w *Workbench) keybindingsDialogSpec() tv.DialogSpec {
	spec := tv.DialogSpec{
		MinW: 58, MaxW: 76, PreferredW: 62,
		MinH: 16, MaxH: 40, PrefH: 34,
	}
	if need := footerRowMinWidth(keybindFooterLabels, tv.DefaultButtonGap); spec.MinW < need {
		spec.MinW = need
	}
	return spec
}

// themeEditorDialogSpec is the content-driven size of the modal theme editor (issue
// #471). Like sessionsDialogSpec/keybindingsDialogSpec it expresses a content
// footprint rather than a share of the terminal. The editor draws two columns of
// swatch+label+field rows; the two column footprints plus the inter-column gutter, the
// scrollbar column and the dialog chrome come to exactly the documented 83-column floor
// (themeEditorDialogW) — the floor is that tight two-column fit, widened 80→83 with issue
// #477's 1→4 inter-column gutter. So width is PINNED there (MinW == MaxW == PreferredW):
// growing wider would only spread the surplus into the role-label cells
// (resolveThemeEditorLayout's extra/2), the spacing/association cascade tracked as a
// separate issue. The contentW sum below MUST stay equal to themeEditorDialogW — they are
// the same number expressed two ways, and a drift makes MinW > MaxW; the layout test
// asserts themeEditorDialogSpec() resolves MinW == MaxW == PreferredW == themeEditorDialogW
// to catch exactly that. Without this spec showThemeEditor fell back to the 80%×85%
// percentage default and ballooned to 160×42 on a 200×50 terminal.
//
// Height: PrefH shows every themeEditorContentRows() role with no scrolling. The chrome
// the viewport math subtracts is constant and floor-height-independent — the resolver
// computes visibleRows = height-3-contentTop, so the non-viewport rows are always
// 3 + themeEditorButtonGap + themeEditorContentTop (button row + border below, the buttonGap
// blank row above the buttons, preset/toggle rows above). MinH keeps the documented 83×22
// floor, where the existing scrolling viewport takes over on a short terminal. MaxH caps the
// height to that content fit so a tall terminal does not stretch the dialog past its rows.
//
// The spec is static (no terminal-share term), so it is path-independent: the same spec
// flows into w.dialogRect at open and into relayout() on resize, and the dialog re-centres
// via the existing relayout()/OnResize path.
func (w *Workbench) themeEditorDialogSpec() tv.DialogSpec {
	const (
		leftCol  = themeEditorSwatchW + 1 + themeEditorLeftLabelW + 1 + themeEditorFieldW // 36
		rightCol = themeEditorSwatchW + 1 + themeEditorLabelW + 1 + themeEditorFieldW     // 38
		// left border+gap (2) + leftCol + gutter (4) + rightCol + scrollbar (1) + gap+border (2).
		// Equals themeEditorDialogW (83) by construction (issue #477 widened the gutter 1→4).
		contentW = 2 + leftCol + 4 + rightCol + 1 + 2
		// Rows the viewport math subtracts. MUST mirror the resolver's visibleRows formula
		// exactly — visibleRows = height-3-themeEditorButtonGap-themeEditorContentTop — or PrefH
		// is off by the omitted term. Issue #477's sectionPad raise grew contentRows to 22 and
		// exposed a prior omission of themeEditorButtonGap here (PrefH was one row short, so the
		// "grown" editor still scrolled the last role). Floor-height-independent.
		chromeH = 3 + themeEditorButtonGap + themeEditorContentTop // 7
	)
	prefH := themeEditorContentRows() + chromeH // all roles visible without scrolling
	return tv.DialogSpec{
		MinW: themeEditorDialogW, MaxW: contentW, PreferredW: contentW,
		MinH: themeEditorDialogH, MaxH: prefH, PrefH: prefH,
	}
}

// statisticsDialogSpec is the content-driven size of the Statistics dialog (issue
// #345). Unlike browserDialogSpec (shared with Resources, which renders arbitrarily
// long SKILL.md / input-schema text and genuinely fills the screen), Statistics
// renders a fixed-column tabular report in a single wrapping TextView: its widest
// line is 97 cells (the Models footnote) and only the Overview section approaches
// 30 lines. So it grows toward — but is capped well below — the 80%/85% browser
// balloon (160×42 on 200×50) instead of always filling it.
//
// PreferredW 100 sizes the detail pane to width-4 = 96 cells (after the listX=2
// margin and the right border), which holds the 73-cell Sessions table outright and
// all but one cell of the 97-cell Models footnote — it wraps by a single cell, which
// is acceptable — instead of the old browser balloon's ~156-cell pane. It is capped
// at MaxW 110 (matching the permission dialog's 110 ceiling) so it never sprawls on
// an ultrawide terminal. PrefH 24 fits
// the tallest typical section (a fast-backend Overview is ~20-30 lines) plus the 8
// rows of chrome, capped at MaxH 36 so a heavy Overview stays under the 42-row
// balloon. The 60×14 floor keeps it usable on a small terminal. The spec is static
// (no terminal-share term), so it is path-independent and the dialog uses
// dialog.Fit on resize, like sessionsDialogSpec.
func (w *Workbench) statisticsDialogSpec() tv.DialogSpec {
	return tv.DialogSpec{
		MinW: 60, MaxW: 110, PreferredW: 100,
		MinH: 14, MaxH: 36, PrefH: 24,
	}
}

// watchersDialogSpec is the content-driven size of the Watchers dialog (issue
// #329 Phase 4). Like sessionsDialogSpec it expresses a content footprint rather
// than a share of the terminal: a watcher list beside a detail pane that shows the
// task text needs a touch more width and height than the sessions browser, so it
// grows toward — but is capped below — the browser balloon (130×32 cap) with the
// usual 60×16 floor to stay usable on a small terminal. The spec is static (no
// terminal-share term), so the dialog uses dialog.Fit on resize.
func (w *Workbench) watchersDialogSpec() tv.DialogSpec {
	return tv.DialogSpec{
		MinW: 60, MaxW: 130, PreferredW: 104,
		MinH: 16, MaxH: 32, PrefH: 24,
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
