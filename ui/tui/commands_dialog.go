package ui

import (
	"fmt"
	"sort"
	"strings"

	tui "github.com/hobbestherat/turbotui"
	tv "github.com/hobbestherat/turbotui/turbotv"
)

// Custom slash-command editor (issue #403). A two-pane modal: the left list shows
// every stored command, the right pane is a detail form (name, description,
// template, parameter sub-editor, model/agent/subtask overrides and a live
// preview of the expanded prompt). Save validates through the backend, which
// enforces collision and shape rules and returns an error shown inline. The
// History button opens the version browser (command_history_dialog.go).

// commandsFooterLabels are the footer action captions in display order.
var commandsFooterLabels = []string{"&New", "&History", "De&lete", "&Save", "&Cancel"}

// showCommandsDialog opens the custom-command editor. It is a no-op (with a
// friendly note) when the backend has not wired command management.
func (w *Workbench) showCommandsDialog() {
	if w.handlers.ListCommands == nil {
		w.showConfirm("Commands", "Custom command management is unavailable.", nil)
		return
	}

	spec := tv.DialogSpec{MinW: 84, MinH: 26, PreferredW: 96}
	if need := footerRowMinWidth(commandsFooterLabels, tv.DefaultButtonGap); spec.MinW < need {
		spec.MinW = need
	}
	x, y, width, height := w.dialogRect(spec)

	var layer *tv.Layer
	closeFn := func() { w.desktop.RemoveLayer(layer) }
	dialog := newCloseableDialog("Custom Commands", x, y, width, height, closeFn)

	listX, listY := 2, 2
	buttonY := height - 3
	hintY := height - 4
	paneH := height - listY - 5
	if paneH < 6 {
		paneH = 6
	}
	listW := 22
	detailX := listX + listW + 2
	detailW := width - detailX - 2
	if detailW < 30 {
		detailW = 30
	}
	const labelW = 7
	boxX := detailX + labelW
	boxW := detailW - labelW
	if boxW < 12 {
		boxW = 12
	}

	dialog.Window.AddContent(dialogLabel("Commands", tv.Rect{X: listX, Y: 1, W: listW, H: 1}))
	dialog.Window.AddContent(dialogLabel("Edit", tv.Rect{X: detailX, Y: 1, W: detailW, H: 1}))

	list := tv.NewTree(tv.Rect{X: listX, Y: listY, W: listW, H: paneH})
	list.FG = tv.DefaultTheme.ListFG
	list.BG = tv.DefaultTheme.ListBG
	list.SelFG, list.SelBG = selectionColorsFor(
		tv.DefaultTheme.ListFG, tv.DefaultTheme.ListBG,
		tv.DefaultTheme.SelectionFG, tv.DefaultTheme.SelectionBG)
	dialog.Window.AddContent(list)

	// Detail-form widgets, laid out top to bottom in the right pane.
	row := listY
	mkField := func(label string) *tv.TextBox {
		dialog.Window.AddContent(dialogLabel(label, tv.Rect{X: detailX, Y: row, W: labelW, H: 1}))
		box := tv.NewTextBox("", tv.Rect{X: boxX, Y: row, W: boxW, H: 1})
		dialog.Window.AddContent(box)
		row++
		return box
	}
	nameBox := mkField("Name:")
	descBox := mkField("Desc:")
	tmplBox := mkField("Tmpl:")
	modelBox := mkField("Model:")
	agentBox := mkField("Agent:")

	subtask := tv.NewCheckbox("Sub&task (run via sub-agent)", tv.Rect{X: detailX, Y: row, W: detailW, H: 1}, nil)
	subtask.FG = tv.DefaultTheme.DialogFG
	subtask.BG = tv.DefaultTheme.DialogBG
	dialog.Window.AddContent(subtask)
	row++

	// Parameter sub-editor: a small list plus Add/Edit/Del and an Insert dropdown
	// that appends the chosen ${name} placeholder to the template.
	dialog.Window.AddContent(dialogLabel("Parameters:", tv.Rect{X: detailX, Y: row, W: detailW, H: 1}))
	row++
	paramsH := 3
	paramList := tv.NewTree(tv.Rect{X: detailX, Y: row, W: detailW, H: paramsH})
	paramList.FG = tv.DefaultTheme.ListFG
	paramList.BG = tv.DefaultTheme.ListBG
	paramList.SelFG, paramList.SelBG = selectionColorsFor(
		tv.DefaultTheme.ListFG, tv.DefaultTheme.ListBG,
		tv.DefaultTheme.SelectionFG, tv.DefaultTheme.SelectionBG)
	dialog.Window.AddContent(paramList)
	row += paramsH

	// Working parameter list (the detail form's editable state).
	var params []CommandParam
	refreshParams := func() {
		nodes := make([]*tv.TreeNode, 0, len(params))
		for i := range params {
			n := tv.NewTreeNode(formatParamRow(params[i]))
			n.Data = params[i]
			nodes = append(nodes, n)
		}
		paramList.Roots = nodes
	}

	insertSel := newSelect(w.desktop, []string{"Insert $param ▾"}, tv.Rect{X: detailX, Y: row, W: 20, H: 1})
	dialog.Window.AddContent(insertSel)
	paramBtnX := detailX + 21
	addBtn := newButton("+Add", tv.Rect{X: paramBtnX, Y: row, W: 7, H: 1}, nil)
	editBtn := newButton("Edit", tv.Rect{X: paramBtnX + 8, Y: row, W: 7, H: 1}, nil)
	delBtn := newButton("Del", tv.Rect{X: paramBtnX + 16, Y: row, W: 6, H: 1}, nil)
	dialog.Window.AddContent(addBtn)
	dialog.Window.AddContent(editBtn)
	dialog.Window.AddContent(delBtn)
	row++

	// Live preview: sample args + the expanded prompt. TextBox exposes no change
	// callback, so the template/args boxes' OnTypeFn is wrapped below (after
	// refreshPreview exists) to re-expand on every keystroke; the Preview button and
	// parameter edits refresh it too.
	dialog.Window.AddContent(dialogLabel("Args:", tv.Rect{X: detailX, Y: row, W: labelW, H: 1}))
	argsBox := tv.NewTextBox("", tv.Rect{X: boxX, Y: row, W: boxW - 10, H: 1})
	dialog.Window.AddContent(argsBox)
	previewBtn := newButton("Preview", tv.Rect{X: detailX + detailW - 9, Y: row, W: 9, H: 1}, nil)
	dialog.Window.AddContent(previewBtn)
	row++

	previewH := height - row - 4
	if previewH < 2 {
		previewH = 2
	}
	preview := tv.NewTextView("", tv.Rect{X: detailX, Y: row, W: detailW, H: previewH})
	preview.Wrap = true
	preview.FG = tv.DefaultTheme.DialogFG
	preview.BG = tv.DefaultTheme.DialogBG
	dialog.Window.AddContent(preview)

	// status reports inline validation errors and save outcomes on the hint row.
	status := dialogLabel("", tv.Rect{X: 2, Y: hintY, W: width - 4, H: 1})
	dialog.Window.AddContent(status)
	setStatus := func(msg string) { status.SetText(msg); w.desktop.Redraw() }

	// currentDef snapshots the form into a CommandDef for preview and save.
	currentDef := func() CommandDef {
		return CommandDef{
			Name:        strings.TrimSpace(nameBox.GetText()),
			Description: strings.TrimSpace(descBox.GetText()),
			Parameters:  append([]CommandParam(nil), params...),
			Template:    tmplBox.GetText(),
			Model:       strings.TrimSpace(modelBox.GetText()),
			Agent:       strings.TrimSpace(agentBox.GetText()),
			Subtask:     subtask.IsChecked(),
		}
	}
	refreshPreview := func() {
		def := currentDef()
		out, err := expandTemplate(def, strings.Fields(argsBox.GetText()))
		if err != nil {
			preview.SetText("(" + err.Error() + ")")
		} else {
			preview.SetText(out)
		}
		preview.ScrollToTop()
		w.desktop.Redraw()
	}

	// Make the preview genuinely live: wrap the template and args boxes so the
	// expansion re-runs after each keystroke the box handles. TextBox installs its
	// own OnTypeFn to edit the buffer; we call it first, then refresh, so editing is
	// preserved and the preview reflects the just-typed character.
	wrapLive := func(box *tv.TextBox) {
		base := box.Component.OnTypeFn
		box.Component.OnTypeFn = func(c *tv.VisualComponent, ev tui.TypeEvent) bool {
			handled := false
			if base != nil {
				handled = base(c, ev)
			}
			refreshPreview()
			return handled
		}
	}
	wrapLive(tmplBox)
	wrapLive(argsBox)

	// originalName is "" for a brand-new command and the loaded command's name
	// otherwise; it decides create vs update at save time.
	originalName := ""
	loadForm := func(def CommandDef) {
		originalName = def.Name
		nameBox.SetText(def.Name)
		descBox.SetText(def.Description)
		tmplBox.SetText(def.Template)
		modelBox.SetText(def.Model)
		agentBox.SetText(def.Agent)
		subtask.SetChecked(def.Subtask)
		params = append([]CommandParam(nil), def.Parameters...)
		refreshParams()
		refreshInsertOptions(insertSel, params)
		refreshPreview()
		setStatus("")
	}
	clearForm := func() {
		loadForm(CommandDef{})
		setStatus("New command — fill in name and template, then Save.")
	}

	// renderList rebuilds the command list from a fresh backend snapshot.
	var commands []CommandInfo
	renderList := func() {
		commands = w.handlers.ListCommands()
		sort.Slice(commands, func(i, j int) bool { return commands[i].Name < commands[j].Name })
		nodes := make([]*tv.TreeNode, 0, len(commands))
		for i := range commands {
			label := commands[i].Name
			if commands[i].Description != "" {
				label += " — " + commands[i].Description
			}
			n := tv.NewTreeNode(label)
			n.Data = commands[i]
			nodes = append(nodes, n)
		}
		list.Roots = nodes
		w.desktop.Redraw()
	}

	// loadSelected pulls the full command for the list selection into the form.
	loadSelected := func() {
		n := list.Selected()
		if n == nil {
			return
		}
		info, ok := n.Data.(CommandInfo)
		if !ok || w.handlers.GetCustomCommand == nil {
			return
		}
		def, err := w.handlers.GetCustomCommand(info.Name)
		if err != nil {
			setStatus("Could not load: " + err.Error())
			return
		}
		loadForm(def)
	}
	list.OnSelect = func(*tv.TreeNode) { loadSelected() }

	// Parameter editing.
	selectedParamIndex := func() int {
		n := paramList.Selected()
		if n == nil {
			return -1
		}
		if p, ok := n.Data.(CommandParam); ok {
			for i := range params {
				if params[i].Name == p.Name {
					return i
				}
			}
		}
		return -1
	}
	addBtn.OnPress = func() {
		w.showCommandParamDialog(CommandParam{}, func(p CommandParam, ok bool) {
			if !ok {
				return
			}
			params = append(params, p)
			refreshParams()
			refreshInsertOptions(insertSel, params)
			refreshPreview()
		})
	}
	editBtn.OnPress = func() {
		i := selectedParamIndex()
		if i < 0 {
			setStatus("Select a parameter to edit.")
			return
		}
		w.showCommandParamDialog(params[i], func(p CommandParam, ok bool) {
			if !ok {
				return
			}
			params[i] = p
			refreshParams()
			refreshInsertOptions(insertSel, params)
			refreshPreview()
		})
	}
	delBtn.OnPress = func() {
		i := selectedParamIndex()
		if i < 0 {
			setStatus("Select a parameter to remove.")
			return
		}
		params = append(params[:i], params[i+1:]...)
		refreshParams()
		refreshInsertOptions(insertSel, params)
		refreshPreview()
	}
	insertSel.OnChange = func(index int) {
		if index <= 0 || index > len(params) {
			return
		}
		tmplBox.SetText(tmplBox.GetText() + "${" + params[index-1].Name + "}")
		insertSel.SetSelected(0) // reset the prompt row
		refreshPreview()
	}
	previewBtn.OnPress = refreshPreview

	// Footer actions.
	newAction := func() { clearForm(); w.desktop.SetFocus(nameBox) }
	historyAction := func() {
		name := strings.TrimSpace(nameBox.GetText())
		if originalName == "" || name != originalName {
			setStatus("Save the command before browsing its history.")
			return
		}
		w.showCommandHistory(originalName, func() { renderList(); loadForm(mustGetCommand(w, originalName)) })
	}
	deleteAction := func() {
		name := strings.TrimSpace(nameBox.GetText())
		if originalName == "" {
			setStatus("Nothing to delete.")
			return
		}
		if w.handlers.DeleteCommand == nil {
			setStatus("Delete is unavailable.")
			return
		}
		if err := w.handlers.DeleteCommand(originalName); err != nil {
			setStatus("Delete failed: " + err.Error())
			return
		}
		_ = name
		clearForm()
		renderList()
		setStatus("Deleted.")
	}
	saveAction := func() {
		def := currentDef()
		// Hard-block collisions in the editor itself (issue #403), before the backend
		// save: a name that is a built-in command, or that duplicates a different
		// existing custom command, is rejected inline. The backend re-enforces both as
		// defence in depth, but blocking here gives the immediate, specific error the
		// issue asks the editor to surface.
		if w.reservedBuiltins()[def.Name] {
			setStatus(fmt.Sprintf("Cannot save: %q is a built-in command name.", def.Name))
			return
		}
		if def.Name != originalName && commandNameExists(commands, def.Name) {
			setStatus(fmt.Sprintf("Cannot save: a command named %q already exists.", def.Name))
			return
		}
		// Warn (do not block) about placeholders with no matching parameter.
		if warns := templateWarnings(def.Template, def.Parameters); len(warns) > 0 {
			setStatus("Warning: " + strings.Join(warns, "; "))
		}
		exists := commandNameExists(commands, def.Name)
		var err error
		if exists && def.Name == originalName {
			if w.handlers.UpdateCommand == nil {
				setStatus("Update is unavailable.")
				return
			}
			err = w.handlers.UpdateCommand(def)
		} else {
			if w.handlers.CreateCommand == nil {
				setStatus("Create is unavailable.")
				return
			}
			err = w.handlers.CreateCommand(def)
		}
		if err != nil {
			setStatus("Cannot save: " + err.Error())
			return
		}
		renderList()
		loadForm(mustGetCommand(w, def.Name))
		w.rebuildMenu() // refresh the palette's custom-command entries
		if status.GetText() == "" || !strings.HasPrefix(status.GetText(), "Warning") {
			setStatus("Saved.")
		}
	}

	footer := footerButtonRects(commandsFooterLabels, 2, width-3, buttonY, tv.DefaultButtonGap)
	actions := []func(){newAction, historyAction, deleteAction, saveAction, closeFn}
	for i, lbl := range commandsFooterLabels {
		dialog.Window.AddContent(newButton(lbl, footer[i], actions[i]))
	}

	dialog.Window.OnClose = func(*tv.Window) { closeFn() }
	dialog.Root().OnTypeFn = func(_ *tv.VisualComponent, event tui.TypeEvent) bool {
		if event.Key == tui.KeyEscape {
			closeFn()
			return true
		}
		return false
	}

	layer = tv.NewModalLayer("commands-dialog", dialog)
	w.desktop.AddLayer(layer)
	dialog.Fit(spec)
	renderList()
	if len(commands) > 0 {
		loadSelected()
	} else {
		clearForm()
	}
	w.desktop.SetFocus(list)
}

// refreshInsertOptions repopulates the Insert dropdown with one row per declared
// parameter (preceded by the prompt row), so inserting a placeholder is a pick.
func refreshInsertOptions(sel *tv.Select, params []CommandParam) {
	opts := make([]string, 0, len(params)+1)
	opts = append(opts, "Insert $param ▾")
	for _, p := range params {
		opts = append(opts, p.Name)
	}
	sel.SetOptions(opts)
	sel.SetSelected(0)
}

// formatParamRow renders one parameter list row: the name, its required/optional
// flag and its default (when set).
func formatParamRow(p CommandParam) string {
	kind := "optional"
	if p.Required {
		kind = "required"
	}
	row := fmt.Sprintf("%s (%s)", p.Name, kind)
	if p.Default != "" {
		row += fmt.Sprintf(" default=%q", p.Default)
	}
	return row
}

// commandNameExists reports whether name is among the listed commands.
func commandNameExists(cmds []CommandInfo, name string) bool {
	for _, c := range cmds {
		if c.Name == name {
			return true
		}
	}
	return false
}

// mustGetCommand fetches a command for re-display after a save/restore, returning
// an empty def (rather than panicking) if the lookup is unavailable or fails — the
// caller only uses it to repaint the form.
func mustGetCommand(w *Workbench, name string) CommandDef {
	if w.handlers.GetCustomCommand == nil {
		return CommandDef{Name: name}
	}
	def, err := w.handlers.GetCustomCommand(name)
	if err != nil {
		return CommandDef{Name: name}
	}
	return def
}

// showCommandParamDialog opens a small modal to add or edit one parameter
// (name, description, required, default). onResult receives the edited parameter
// and true on accept, or false on cancel.
func (w *Workbench) showCommandParamDialog(initial CommandParam, onResult func(CommandParam, bool)) {
	spec := tv.DialogSpec{MinW: 48, MaxW: 64, MinH: 9, MaxH: 9, PreferredW: 52}
	x, y, width, height := w.dialogRect(spec)

	var layer *tv.Layer
	finish := func(p CommandParam, ok bool) {
		w.desktop.RemoveLayer(layer)
		if onResult != nil {
			onResult(p, ok)
		}
	}

	dialog := tv.NewDialog("Parameter", x, y, width, height)
	applyWindowShadow(dialog.Window)
	dialog.Window.ShowClose = false

	const labelW = 9
	boxX := 2 + labelW
	boxW := width - boxX - 2
	mk := func(label, val string, rowY int) *tv.TextBox {
		dialog.Window.AddContent(dialogLabel(label, tv.Rect{X: 2, Y: rowY, W: labelW, H: 1}))
		b := tv.NewTextBox(val, tv.Rect{X: boxX, Y: rowY, W: boxW, H: 1})
		dialog.Window.AddContent(b)
		return b
	}
	nameBox := mk("Name:", initial.Name, 1)
	descBox := mk("Desc:", initial.Description, 2)
	defBox := mk("Default:", initial.Default, 3)
	required := tv.NewCheckbox("&Required", tv.Rect{X: boxX, Y: 4, W: boxW, H: 1}, nil)
	required.FG = tv.DefaultTheme.DialogFG
	required.BG = tv.DefaultTheme.DialogBG
	required.SetChecked(initial.Required)
	dialog.Window.AddContent(required)

	save := func() {
		name := strings.TrimSpace(nameBox.GetText())
		if !cmdParamNameValid(name) {
			w.showConfirm("Parameter", "Invalid name. Use a letter/underscore then letters, digits or underscores.", nil)
			return
		}
		finish(CommandParam{
			Name:        name,
			Description: strings.TrimSpace(descBox.GetText()),
			Required:    required.IsChecked(),
			Default:     defBox.GetText(),
		}, true)
	}
	dialog.Window.AddContent(newButton("Save", tv.Rect{X: width - 22, Y: height - 3, W: 9, H: 1}, save))
	dialog.Window.AddContent(newButton("Cancel", tv.Rect{X: width - 12, Y: height - 3, W: 10, H: 1}, func() { finish(CommandParam{}, false) }))

	dialog.Root().OnTypeFn = func(_ *tv.VisualComponent, event tui.TypeEvent) bool {
		if event.Key == tui.KeyEscape {
			finish(CommandParam{}, false)
			return true
		}
		return false
	}

	layer = tv.NewModalLayer("command-param-dialog", dialog)
	w.desktop.AddLayer(layer)
	dialog.Fit(spec)
	w.desktop.SetFocus(nameBox)
}

// cmdParamNameValid mirrors command.ValidParamName for the UI layer (a C-style
// identifier), keeping ui/tui free of the internal package.
func cmdParamNameValid(name string) bool {
	if name == "" {
		return false
	}
	for i, r := range name {
		isAlpha := r == '_' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z')
		isDigit := r >= '0' && r <= '9'
		if i == 0 && !isAlpha {
			return false
		}
		if i > 0 && !isAlpha && !isDigit {
			return false
		}
	}
	return true
}
