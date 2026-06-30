package ui

import (
	"fmt"
	"strings"

	"gogent/internal/config"
	"gogent/internal/model"

	tui "github.com/hobbestherat/turbotui"
	tv "github.com/hobbestherat/turbotui/turbotv"
)

// This file implements the "Connections…" dialog: the home for ADD / EDIT /
// REMOVE of provider connections — the credentialed backends (api_type, endpoint,
// api_key / project+location) that models reference by name. It mirrors the
// Models… dialog (a Tree list + buttons + confirm dialogs) and the shared editor
// form pattern, built from existing turbotui primitives only.

// connectionsReady reports whether connection management is wired.
func (w *Workbench) connectionsReady() bool {
	return w.handlers.GetConnections != nil
}

// showConnectionsDialog opens the connection-management dialog. Each row shows the
// connection name, api_type, and a short endpoint/credential hint; the toolbar
// offers Add, Edit, Remove and Done. It is rebuilt after every mutation.
func (w *Workbench) showConnectionsDialog() {
	if !w.connectionsReady() {
		w.showConfirm("Connections", "Connection management is unavailable.", nil)
		return
	}
	conns := w.handlers.GetConnections()

	rowLabel := func(c config.ProviderConnection) string {
		at := c.APIType
		if at == "" {
			at = "openai"
		}
		detail := strings.TrimSpace(c.Endpoint)
		if detail == "" {
			if strings.HasPrefix(at, "vertex") {
				detail = strings.TrimSpace(c.Project + "/" + c.Location)
			} else {
				detail = "(provider default)"
			}
		}
		return fmt.Sprintf("%s — %s — %s", c.Name, at, detail)
	}

	type action struct {
		label string
		fn    func()
	}
	var layer *tv.Layer
	closeFn := func() { w.desktop.RemoveLayer(layer) }
	reopen := func() {
		w.desktop.RemoveLayer(layer)
		w.showConnectionsDialog()
	}

	var list *tv.Tree
	selected := func() (config.ProviderConnection, bool) {
		if n := list.Selected(); n != nil {
			if c, ok := n.Data.(config.ProviderConnection); ok {
				return c, true
			}
		}
		return config.ProviderConnection{}, false
	}

	add := func() {
		if w.handlers.AddConnection == nil {
			return
		}
		w.showConnectionForm("Add connection", config.ProviderConnection{APIType: "openai"}, true,
			w.handlers.AddConnection, reopen)
	}
	edit := func() {
		c, ok := selected()
		if !ok || w.handlers.UpdateConnection == nil {
			return
		}
		w.showConnectionForm("Edit connection — "+c.Name, c, false, w.handlers.UpdateConnection, reopen)
	}
	remove := func() {
		c, ok := selected()
		if !ok || w.handlers.RemoveConnection == nil {
			return
		}
		w.showConfirm("Remove connection",
			fmt.Sprintf("Remove connection %q? Models that use it will stop working.", c.Name),
			func(ok bool) {
				if !ok {
					return
				}
				if err := w.handlers.RemoveConnection(c.Name); err != nil {
					w.showConfirm("Remove connection", "Could not remove:\n"+err.Error(), nil)
					return
				}
				reopen()
			})
	}

	var acts []action
	if w.handlers.AddConnection != nil {
		acts = append(acts, action{"&Add…", add})
	}
	if len(conns) > 0 {
		if w.handlers.UpdateConnection != nil {
			acts = append(acts, action{"Ed&it…", edit})
		}
		if w.handlers.RemoveConnection != nil {
			acts = append(acts, action{"&Remove", remove})
		}
	}
	acts = append(acts, action{"&Done", closeFn})
	labels := make([]string, len(acts))
	for i, a := range acts {
		labels[i] = a.label
	}

	rows := make([]string, len(conns))
	for i := range conns {
		rows[i] = rowLabel(conns[i])
	}
	width0 := longestRuneLen(rows) + 6
	if min := footerRowMinWidth(labels, tv.DefaultButtonGap); width0 < min {
		width0 = min
	}
	if width0 < modelEditorMinWidth {
		width0 = modelEditorMinWidth
	}
	paneRows := len(conns)
	if paneRows < 1 {
		paneRows = 1
	}
	if paneRows > 12 {
		paneRows = 12
	}
	height0 := paneRows + 8

	spec := tv.DialogSpec{MinW: width0, MinH: height0, MaxH: height0, PreferredW: width0}
	x, y, width, height := w.dialogRect(spec)

	dialog := tv.NewDialog("Connections", x, y, width, height)
	applyWindowShadow(dialog.Window)
	dialog.Window.ShowClose = false

	listX := 2
	listW := width - 4
	listY := 2
	hintY := height - 5
	buttonY := height - 3
	paneH := height - listY - 6
	if paneH < 1 {
		paneH = 1
	}

	list = tv.NewTree(tv.Rect{X: listX, Y: listY, W: listW, H: paneH})
	list.FG = tv.DefaultTheme.ListFG
	list.BG = tv.DefaultTheme.ListBG
	list.SelFG, list.SelBG = selectionColorsFor(
		tv.DefaultTheme.ListFG, tv.DefaultTheme.ListBG,
		tv.DefaultTheme.SelectionFG, tv.DefaultTheme.SelectionBG)
	dialog.Window.AddContent(list)

	if len(conns) == 0 {
		dialog.Window.AddContent(dialogLabel("No connections — choose Add to create one.",
			tv.Rect{X: listX + 1, Y: listY, W: listW - 1, H: 1}))
	} else {
		nodes := make([]*tv.TreeNode, 0, len(conns))
		for i := range conns {
			n := tv.NewTreeNode(rowLabel(conns[i]))
			n.Data = conns[i]
			nodes = append(nodes, n)
		}
		list.Roots = nodes
		list.OnActivate = func(*tv.TreeNode) { edit() }
	}

	hint := "Add a connection to begin · Esc close"
	if len(conns) > 0 {
		hint = "Enter edit · Tab move · Esc close"
	}
	dialog.Window.AddContent(dialogLabel(hint, tv.Rect{X: 2, Y: hintY, W: width - 4, H: 1}))

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

	layer = tv.NewModalLayer("connections-dialog", dialog)
	w.desktop.AddLayer(layer)
	dialog.Fit(spec)
	w.desktop.SetFocus(list)
}

// showConnectionForm is the shared add/edit form for a provider connection. The
// api_key box is left blank on edit (the daemon redacts it); a blank key on save
// preserves the stored secret. Project/Location are shown for vertex* and ignored
// elsewhere; DiscoveryEndpoint is an optional listing-host override.
func (w *Workbench) showConnectionForm(title string, initial config.ProviderConnection, nameEditable bool, onSave func(config.ProviderConnection) error, onSaved func()) {
	const labelW = modelEditorLabelW
	const boxX = modelEditorBoxX
	const formHeight = 14

	spec := tv.DialogSpec{MinW: 70, MinH: formHeight, MaxH: formHeight, PreferredW: 84}
	x, y, width, height := w.dialogRect(spec)
	boxW := width - boxX - 3

	dialog := tv.NewDialog(title, x, y, width, height)
	applyWindowShadow(dialog.Window)
	dialog.Window.ShowClose = false

	field := func(text string, row int) *tv.TextBox {
		dialog.Window.AddContent(dialogLabel(text, tv.Rect{X: 2, Y: row, W: labelW, H: 1}))
		box := tv.NewTextBox("", tv.Rect{X: boxX, Y: row, W: boxW, H: 1})
		dialog.Window.AddContent(box)
		return box
	}

	// Name: editable on add, read-only on edit (the reference key).
	dialog.Window.AddContent(dialogLabel("Name:", tv.Rect{X: 2, Y: 1, W: labelW, H: 1}))
	var nameBox *tv.TextBox
	if nameEditable {
		nameBox = tv.NewTextBox("", tv.Rect{X: boxX, Y: 1, W: boxW, H: 1})
		nameBox.SetText(initial.Name)
		dialog.Window.AddContent(nameBox)
	} else {
		dialog.Window.AddContent(dialogLabel(initial.Name, tv.Rect{X: boxX, Y: 1, W: boxW, H: 1}))
	}

	dialog.Window.AddContent(dialogLabel("API type:", tv.Rect{X: 2, Y: 2, W: labelW, H: 1}))
	apiTypeOpts := model.APITypeIDs()
	apiType := newSelect(w.desktop, apiTypeOpts, tv.Rect{X: boxX, Y: 2, W: boxW, H: 1})
	apiType.SetSelected(indexOrZero(apiTypeOpts, initial.APIType))
	dialog.Window.AddContent(apiType)

	endpoint := field("Endpoint:", 3)
	endpoint.SetText(initial.Endpoint)
	discovery := field("Discovery URL:", 4)
	discovery.SetText(initial.DiscoveryEndpoint)
	apiKey := field("API key:", 5)
	apiKey.SetText(initial.APIKey)
	project := field("Project:", 6)
	project.SetText(initial.Project)
	location := field("Location:", 7)
	location.SetText(initial.Location)

	dialog.Window.AddContent(dialogLabel(
		"Endpoint/Discovery URL: blank uses the provider default. Project/Location: Vertex only.",
		tv.Rect{X: 2, Y: 9, W: width - 4, H: 1}))

	var layer *tv.Layer
	cancel := func() { w.desktop.RemoveLayer(layer) }
	save := func() {
		pc := initial
		if nameEditable {
			pc.Name = strings.TrimSpace(nameBox.GetText())
		}
		pc.APIType = apiType.Value()
		pc.Endpoint = strings.TrimSpace(endpoint.GetText())
		pc.DiscoveryEndpoint = strings.TrimSpace(discovery.GetText())
		pc.APIKey = apiKey.GetText() // blank preserves the stored key (server/core)
		pc.Project = strings.TrimSpace(project.GetText())
		pc.Location = strings.TrimSpace(location.GetText())

		if pc.Name == "" {
			w.showConfirm("Connection", "A unique connection name is required.", nil)
			return
		}
		if err := onSave(pc); err != nil {
			w.showConfirm("Connection", "Could not save connection:\n"+err.Error(), nil)
			return
		}
		w.desktop.RemoveLayer(layer)
		if onSaved != nil {
			onSaved()
		}
	}

	dialog.Window.AddContent(newButton("Save", tv.Rect{X: width - 24, Y: height - 3, W: 9, H: 1}, save))
	dialog.Window.AddContent(newButton("Cancel", tv.Rect{X: width - 13, Y: height - 3, W: 10, H: 1}, cancel))

	dialog.Root().OnTypeFn = func(_ *tv.VisualComponent, event tui.TypeEvent) bool {
		if event.Key == tui.KeyEscape {
			cancel()
			return true
		}
		return false
	}

	layer = tv.NewModalLayer("connection-form", dialog)
	w.desktop.AddLayer(layer)
	dialog.Fit(spec)
	if nameEditable {
		w.desktop.SetFocus(nameBox)
	} else {
		w.desktop.SetFocus(apiType)
	}
}
