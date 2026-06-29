package ui

import (
	"fmt"

	"gogent/internal/config"

	tui "github.com/hobbestherat/turbotui"
	tv "github.com/hobbestherat/turbotui/turbotv"
)

// This file implements the unified "Models…" dialog (issue #509): the single home
// for ADD / EDIT / REMOVE / SET-DEFAULT of model backends, replacing the old
// split between the manual editor and a separate "Add Model from Catalog…" menu
// entry. It is built purely from existing turbotui primitives (a Tree list +
// buttons + confirm dialogs) — no new widget, no turbotui change (Solution A).
//
// It opens and works with ZERO configured models (empty-list state: only the Add
// and Done buttons are present) and, via the Add Empty… path, fully offline. The
// Add from Catalog… path reuses the existing catalog wizard unchanged and is
// hidden when the catalog is unavailable. The edit/add field set lives in the
// shared showModelForm builder (model_editor.go).

// showModelsDialog opens the unified model-management dialog. Each row shows the
// default marker, display name, model id and api type; the toolbar offers Add
// (Catalog + Empty), Edit, Remove, Set Default and Done. The dialog is rebuilt
// from the authoritative model list after every mutation so it always reflects
// persisted state (and the live dropdowns are refreshed via refreshModelsList).
func (w *Workbench) showModelsDialog() {
	if w.handlers.GetModels == nil {
		w.showConfirm("Models", "Model management is unavailable.", nil)
		return
	}
	models := w.handlers.GetModels()
	defaultName := ""
	if w.handlers.GetDefaultModel != nil {
		defaultName = w.handlers.GetDefaultModel()
	}

	// rowLabel renders one list row: default marker + display name + model id +
	// api type. The two-space lead keeps non-default rows aligned under the "✓ ".
	rowLabel := func(m config.ModelConfig) string {
		marker := "  "
		if m.Name == defaultName {
			marker = "✓ "
		}
		disp := m.DisplayName
		if disp == "" {
			disp = m.Name
		}
		return fmt.Sprintf("%s%s — %s — %s", marker, disp, m.Model, m.APIType)
	}

	// Build the action list first: it determines both the button row and the
	// minimum dialog width. Edit/Remove/Set Default appear only when there is at
	// least one model AND the backend wires the handler, so the empty-list state
	// (and a backend missing a capability) cleanly shows only Add/Done.
	type action struct {
		label string
		fn    func()
	}
	var layer *tv.Layer
	closeFn := func() { w.desktop.RemoveLayer(layer) }
	// reopen rebuilds the dialog from scratch after a mutation so the tree, marker
	// and button set all reflect the new persisted state.
	reopen := func() {
		w.desktop.RemoveLayer(layer)
		w.showModelsDialog()
	}

	// selected resolves the model behind the current list row (false on the empty
	// placeholder or no selection), so the row-scoped actions can no-op safely.
	var list *tv.Tree
	selected := func() (config.ModelConfig, bool) {
		if n := list.Selected(); n != nil {
			if m, ok := n.Data.(config.ModelConfig); ok {
				return m, true
			}
		}
		return config.ModelConfig{}, false
	}

	addEmpty := func() {
		w.showModelForm("Add model — empty", config.ModelConfig{}, true, w.handlers.AddModel, func() {
			// Add/edit/set-default always leave a non-empty list, so the guarded
			// refresh is correct AND resilient: a transient GetModels failure (remote
			// mode returns nil on error) preserves the live dropdowns rather than
			// blanking them. Only remove uses the unconditional refreshModelsList,
			// where an empty result is the legitimate expected post-state (D1).
			w.refreshModelsAfterSave()
			reopen()
		})
	}
	addCatalog := func() {
		// Reuse the existing catalog wizard. Close this list while the wizard runs,
		// then reopen a FRESH list when the wizard finishes — on success OR cancel —
		// so the newly-added model is visible and the user can keep managing (the
		// onClose continuation; the wizard already refreshes the live dropdowns on
		// save). reopen() rebuilds from the authoritative GetModels.
		w.desktop.RemoveLayer(layer)
		w.showAddModelDialog(func() { w.showModelsDialog() })
	}
	edit := func() {
		m, ok := selected()
		if !ok {
			return
		}
		title := m.DisplayName
		if title == "" {
			title = m.Name
		}
		w.showModelForm("Edit model — "+title, m, false, w.handlers.UpdateModel, func() {
			w.refreshModelsAfterSave()
			reopen()
		})
	}
	remove := func() {
		m, ok := selected()
		if !ok {
			return
		}
		disp := m.DisplayName
		if disp == "" {
			disp = m.Name
		}
		w.showConfirm("Remove model", fmt.Sprintf("Remove %q? This cannot be undone.", disp), func(ok bool) {
			if !ok {
				return
			}
			if err := w.handlers.RemoveModel(m.Name); err != nil {
				// Blocked removals (default-while-others, in-use) come back as errors
				// from core/server; surface the reason verbatim.
				w.showConfirm("Remove model", "Could not remove model:\n"+err.Error(), nil)
				return
			}
			w.refreshModelsList()
			reopen()
		})
	}
	setDefault := func() {
		m, ok := selected()
		if !ok {
			return
		}
		if err := w.handlers.SetDefaultModel(m.Name); err != nil {
			w.showConfirm("Default model", "Could not set default:\n"+err.Error(), nil)
			return
		}
		w.refreshModelsAfterSave()
		reopen()
	}

	var acts []action
	if w.catalogReady() {
		acts = append(acts, action{"Add from &Catalog…", addCatalog})
	}
	if w.handlers.AddModel != nil {
		acts = append(acts, action{"Add &Empty…", addEmpty})
	}
	if len(models) > 0 {
		if w.handlers.UpdateModel != nil {
			acts = append(acts, action{"Ed&it…", edit})
		}
		if w.handlers.RemoveModel != nil {
			acts = append(acts, action{"&Remove", remove})
		}
		if w.handlers.SetDefaultModel != nil {
			acts = append(acts, action{"&Set Default", setDefault})
		}
	}
	acts = append(acts, action{"&Done", closeFn})
	labels := make([]string, len(acts))
	for i, a := range acts {
		labels[i] = a.label
	}

	// Size to content: wide enough for the longest row AND the button row, with a
	// comfort floor; tall enough for the list (capped) plus the hint + button rows.
	rows := make([]string, len(models))
	for i := range models {
		rows[i] = rowLabel(models[i])
	}
	width0 := longestRuneLen(rows) + 6
	if min := footerRowMinWidth(labels, tv.DefaultButtonGap); width0 < min {
		width0 = min
	}
	if width0 < modelEditorMinWidth {
		width0 = modelEditorMinWidth
	}
	paneRows := len(models)
	if paneRows < 1 {
		paneRows = 1
	}
	if paneRows > 12 {
		paneRows = 12
	}
	// The footer buttons render 1 row tall with a blank separator row above them
	// (issue #585, reverting the 2-row footer of #529). The height is unchanged at
	// paneRows + 8: the 2-row button strip lost one row and the new blank separator
	// added one back, so the 1-row footer + blank row occupy the same two rows the
	// 2-row footer used: top label + list + hint row + blank row + 1-row button row
	// + borders.
	height0 := paneRows + 8

	spec := tv.DialogSpec{MinW: width0, MinH: height0, MaxH: height0, PreferredW: width0}
	x, y, width, height := w.dialogRect(spec)

	dialog := tv.NewDialog("Models", x, y, width, height)
	applyWindowShadow(dialog.Window) // honour the NoShadow theme setting (issue #215)
	dialog.Window.ShowClose = false

	listX := 2
	listW := width - 4
	listY := 2
	// Footer layout (issue #585): the 1-row action buttons sit flush on the last
	// interior row (height-3, bottom border at height-2), with a blank separator row
	// immediately above them at height-4 and the hint at height-5. Nothing is drawn
	// on height-4 — that unpainted interior row IS the requested empty line above the
	// buttons, separating them from the hint/list. The list keeps its paneRows rows
	// (height-listY-6) and the dialog height stays paneRows+8 (see height0 above).
	hintY := height - 5
	buttonY := height - 3
	paneH := height - listY - 6
	if paneH < 1 {
		paneH = 1
	}

	list = tv.NewTree(tv.Rect{X: listX, Y: listY, W: listW, H: paneH})
	list.FG = tv.DefaultTheme.ListFG
	list.BG = tv.DefaultTheme.ListBG
	// Fall back to an inverted bar when the theme paints the selection the same as
	// the list background (issue #327), matching the other dialog lists.
	list.SelFG, list.SelBG = selectionColorsFor(
		tv.DefaultTheme.ListFG, tv.DefaultTheme.ListBG,
		tv.DefaultTheme.SelectionFG, tv.DefaultTheme.SelectionBG)
	dialog.Window.AddContent(list)

	if len(models) == 0 {
		// Empty-list state: a non-selectable placeholder (no Data) so the row-scoped
		// actions stay inert. Only Add/Done are present in the button row.
		dialog.Window.AddContent(dialogLabel("No models configured — choose Add to create one.",
			tv.Rect{X: listX + 1, Y: listY, W: listW - 1, H: 1}))
	} else {
		nodes := make([]*tv.TreeNode, 0, len(models))
		for i := range models {
			n := tv.NewTreeNode(rowLabel(models[i]))
			n.Data = models[i]
			nodes = append(nodes, n)
		}
		list.Roots = nodes
		list.OnActivate = func(*tv.TreeNode) { edit() } // Enter on a row == Edit
	}

	hint := "Add a model to begin · Esc close"
	if len(models) > 0 {
		hint = "Enter edit · Tab move · Esc close"
	}
	dialog.Window.AddContent(dialogLabel(hint, tv.Rect{X: 2, Y: hintY, W: width - 4, H: 1}))

	// 1-row footer buttons with a blank separator row above (issue #585, reverting
	// #529's 2-row footer): the Models… dialog now uses the same 1-row footerButtonRects
	// path as every other dialog, the blank separator at height-4 providing the visual
	// gap the taller buttons used to.
	footer := footerButtonRects(labels, 2, width-3, buttonY, tv.DefaultButtonGap)
	for i, a := range acts {
		dialog.Window.AddContent(newButton(a.label, footer[i], a.fn))
	}

	dialog.Root().OnTypeFn = func(_ *tv.VisualComponent, event tui.TypeEvent) bool {
		if event.Key == tui.KeyEscape {
			closeFn()
			return true
		}
		return false
	}

	layer = tv.NewModalLayer("models-dialog", dialog)
	w.desktop.AddLayer(layer)
	dialog.Fit(spec) // re-resolve the rect when the terminal is resized (issue #299)
	w.desktop.SetFocus(list)
}

// refreshModelsList re-fetches the authoritative model list and pushes it into
// the workbench UNCONDITIONALLY — including the empty case — so removing the last
// model clears the sidebar's selector and every open session window's dropdown
// (issue #509). refreshModelsAfterSave gates SetModels on a non-empty list, which
// is correct for add/edit but would leave a deleted last model stale in the live
// dropdowns; this helper is the remove-safe variant and is used for every
// mutation the Models… dialog performs.
func (w *Workbench) refreshModelsList() {
	var ptrs []*config.ModelConfig
	if w.handlers.GetModels != nil {
		refreshed := w.handlers.GetModels()
		ptrs = make([]*config.ModelConfig, len(refreshed))
		for i := range refreshed {
			ptrs[i] = &refreshed[i]
		}
	}
	w.SetModels(ptrs)
	w.rebuildMenu()
}
