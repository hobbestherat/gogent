package ui

import (
	"fmt"
	"strconv"

	tui "github.com/hobbestherat/turbotui"
	tv "github.com/hobbestherat/turbotui/turbotv"
)

// showModelEditor opens a modal editor for the configured model backends. A
// Select switches between models; the text fields below edit the selected
// model's endpoint, model id, API key, temperature and max tokens. Edits are
// kept in a local copy as the selection changes and are persisted (per model)
// only when the user presses Save.
func (w *Workbench) showModelEditor() {
	if w.handlers.GetModels == nil || w.handlers.UpdateModel == nil {
		tv.ShowConfirmYesNo(w.desktop, "Models", "Model editing is unavailable.", nil)
		return
	}
	models := w.handlers.GetModels()
	if len(models) == 0 {
		tv.ShowConfirmYesNo(w.desktop, "Models", "No models are configured.", nil)
		return
	}

	const width = 64
	const height = 17
	const labelW = 16
	const boxX = 2 + labelW
	boxW := width - boxX - 3
	x, y := centeredDialog(w, width, height)

	dialog := tv.NewDialog("Model Settings", x, y, width, height)
	dialog.Window.ShowClose = false

	names := make([]string, len(models))
	for i, m := range models {
		label := m.DisplayName
		if label == "" {
			label = m.Name
		}
		names[i] = fmt.Sprintf("%s (%s)", label, m.Name)
	}

	// field builds a labelled text field, registers it on the dialog, and
	// returns its box for later get/set.
	field := func(text string, row int) *tv.TextBox {
		dialog.Window.AddContent(dialogLabel(text, tv.Rect{X: 2, Y: row, W: labelW, H: 1}))
		box := tv.NewTextBox("", tv.Rect{X: boxX, Y: row, W: boxW, H: 1})
		dialog.Window.AddContent(box)
		return box
	}

	dialog.Window.AddContent(dialogLabel("Model:", tv.Rect{X: 2, Y: 1, W: labelW, H: 1}))
	sel := tv.NewSelect(w.desktop, names, tv.Rect{X: boxX, Y: 1, W: boxW, H: 1})
	dialog.Window.AddContent(sel)

	display := field("Display name:", 3)
	endpoint := field("Endpoint:", 4)
	modelID := field("Model id:", 5)
	apiKey := field("API key:", 6)
	temp := field("Temperature:", 7)
	maxTokens := field("Max tokens:", 8)

	cur := 0
	load := func(i int) {
		m := models[i]
		display.SetText(m.DisplayName)
		endpoint.SetText(m.Endpoint)
		modelID.SetText(m.Model)
		apiKey.SetText(m.APIKey)
		temp.SetText(strconv.FormatFloat(float64(m.Temperature), 'g', -1, 32))
		maxTokens.SetText(strconv.Itoa(m.MaxTokens))
	}
	store := func(i int) {
		models[i].DisplayName = display.GetText()
		models[i].Endpoint = endpoint.GetText()
		models[i].Model = modelID.GetText()
		models[i].APIKey = apiKey.GetText()
		if v, err := strconv.ParseFloat(temp.GetText(), 32); err == nil {
			models[i].Temperature = float32(v)
		}
		models[i].MaxTokens = atoiOr(maxTokens.GetText(), models[i].MaxTokens)
	}
	load(cur)
	sel.OnChange = func(index int) {
		if index == cur {
			return
		}
		store(cur)
		cur = index
		load(cur)
		w.desktop.Redraw()
	}

	var layer *tv.Layer
	save := func() {
		store(cur)
		var failed string
		for _, m := range models {
			if err := w.handlers.UpdateModel(m); err != nil {
				failed = err.Error()
			}
		}
		w.desktop.RemoveLayer(layer)
		w.rebuildMenu()
		if failed != "" {
			tv.ShowConfirmYesNo(w.desktop, "Models", "Some models failed to save:\n"+failed, nil)
		}
	}
	cancel := func() { w.desktop.RemoveLayer(layer) }

	dialog.Window.AddContent(tv.NewButton("Save", tv.Rect{X: width - 24, Y: height - 3, W: 9, H: 1}, save))
	dialog.Window.AddContent(tv.NewButton("Cancel", tv.Rect{X: width - 13, Y: height - 3, W: 10, H: 1}, cancel))

	dialog.Root().OnTypeFn = func(_ *tv.VisualComponent, event tui.TypeEvent) bool {
		if event.Key == tui.KeyEscape {
			cancel()
			return true
		}
		return false
	}

	layer = tv.NewModalLayer("model-editor", dialog)
	w.desktop.AddLayer(layer)
	w.desktop.SetFocus(sel)
}
