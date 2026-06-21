package ui

import (
	"fmt"
	"strconv"
	"strings"

	"gogent/internal/model"

	tui "github.com/hobbestherat/turbotui"
	tv "github.com/hobbestherat/turbotui/turbotv"
)

// modelEditor layout constants: the field labels occupy labelW cells from X=2,
// and the boxed fields (and the model Select) start at boxX. boxW is derived as
// width - boxX - 3 so the fields and the trailing Scan button fit the dialog.
const (
	modelEditorLabelW = 16
	modelEditorBoxX   = 2 + modelEditorLabelW
)

// modelEditorWidth picks the model-editor dialog width so the widest Select
// option (longestOption cells) fits its box without clipping, clamped to the
// available screen width (issue #108). The Select needs two extra cells for its
// value padding and ▼ glyph, and boxW = width - modelEditorBoxX - 3, so the
// minimum non-clipping width is longestOption + 2 + modelEditorBoxX + 3. A
// baseline minimum keeps the dialog from collapsing for short names.
func modelEditorWidth(longestOption, screen int) int {
	const minWidth = 64
	width := longestOption + 2 + modelEditorBoxX + 3
	if width < minWidth {
		width = minWidth
	}
	if screen > 0 && width > screen {
		width = screen
	}
	return width
}

// longestRuneLen returns the display width (in cells) of the widest string in ss.
func longestRuneLen(ss []string) int {
	max := 0
	for _, s := range ss {
		if l := runeLen(s); l > max {
			max = l
		}
	}
	return max
}

// showModelEditor opens a modal editor for the configured model backends. A
// Select switches between models; the text fields below edit the selected
// model's endpoint, model id, API key, temperature and max tokens. Edits are
// kept in a local copy as the selection changes and are persisted (per model)
// only when the user presses Save.
func (w *Workbench) showModelEditor() {
	if w.handlers.GetModels == nil || w.handlers.UpdateModel == nil {
		w.showConfirm("Models", "Model editing is unavailable.", nil)
		return
	}
	models := w.handlers.GetModels()
	if len(models) == 0 {
		w.showConfirm("Models", "No models are configured.", nil)
		return
	}

	const height = 18
	const labelW = modelEditorLabelW
	const boxX = modelEditorBoxX

	names := make([]string, len(models))
	for i, m := range models {
		label := m.DisplayName
		if label == "" {
			label = m.Name
		}
		names[i] = fmt.Sprintf("%s (%s)", label, m.Name)
	}

	// Size the dialog so the widest option label fits the boxed fields without
	// clipping, clamped to the screen (issue #108).
	width := modelEditorWidth(longestRuneLen(names), w.app.Width())
	boxW := width - boxX - 3
	x, y := centeredDialog(w, width, height)

	dialog := tv.NewDialog("Model Settings", x, y, width, height)
	applyWindowShadow(dialog.Window) // honour the NoShadow theme setting (issue #215)
	dialog.Window.ShowClose = false

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

	dialog.Window.AddContent(dialogLabel("API type:", tv.Rect{X: 2, Y: 4, W: labelW, H: 1}))
	apiTypeOpts := model.APITypeIDs()
	apiType := tv.NewSelect(w.desktop, apiTypeOpts, tv.Rect{X: boxX, Y: 4, W: boxW, H: 1})
	dialog.Window.AddContent(apiType)

	endpoint := field("Endpoint:", 5)

	// Model id: a text field with a "Scan" button that queries the backend and,
	// on success, swaps the text field for a dropdown of the advertised models.
	dialog.Window.AddContent(dialogLabel("Model id:", tv.Rect{X: 2, Y: 6, W: labelW, H: 1}))
	const scanW = 8
	modelBoxW := boxW - scanW - 1
	modelRect := tv.Rect{X: boxX, Y: 6, W: modelBoxW, H: 1}
	modelID := tv.NewTextBox("", modelRect)
	dialog.Window.AddContent(modelID)
	modelSelect := tv.NewSelect(w.desktop, nil, modelRect)
	modelSelect.Root().Visible = false
	dialog.Window.AddContent(modelSelect)
	var scanModels func()
	scanBtn := newButton("Scan", tv.Rect{X: boxX + modelBoxW + 1, Y: 6, W: scanW, H: 1}, func() {
		if scanModels != nil {
			scanModels()
		}
	})
	dialog.Window.AddContent(scanBtn)

	apiKey := field("API key:", 7)
	temp := field("Temperature:", 8)
	maxTokens := field("Max tokens:", 9)
	reasoningEffort := field("Reasoning:", 10)

	// Thinking is tri-state: "default" leaves the toggle unset (provider
	// default), "on"/"off" force it. Only providers that understand it actually
	// send it (see buildRequest), so it is safe to show for every model.
	dialog.Window.AddContent(dialogLabel("Thinking:", tv.Rect{X: 2, Y: 11, W: labelW, H: 1}))
	thinkingOpts := []string{"default", "on", "off"}
	thinking := tv.NewSelect(w.desktop, thinkingOpts, tv.Rect{X: boxX, Y: 11, W: boxW, H: 1})
	dialog.Window.AddContent(thinking)

	// currentModelID reads the model id from whichever model widget is active.
	currentModelID := func() string {
		if modelSelect.Root().Visible {
			return modelSelect.Value()
		}
		return modelID.GetText()
	}

	cur := 0
	load := func(i int) {
		m := models[i]
		display.SetText(m.DisplayName)
		apiType.SetSelected(indexOrZero(apiTypeOpts, m.APIType))
		endpoint.SetText(m.Endpoint)
		// Reset to free-text mode; a scanned list belongs to one backend only.
		modelID.SetText(m.Model)
		modelSelect.Root().Visible = false
		modelID.Root().Visible = true
		apiKey.SetText(m.APIKey)
		temp.SetText(strconv.FormatFloat(float64(m.Temperature), 'g', -1, 32))
		maxTokens.SetText(strconv.Itoa(m.MaxTokens))
		reasoningEffort.SetText(m.ReasoningEffort)
		thinking.SetSelected(thinkingIndex(m.Thinking))
	}
	store := func(i int) {
		models[i].DisplayName = display.GetText()
		models[i].APIType = apiType.Value()
		models[i].Endpoint = endpoint.GetText()
		models[i].Model = currentModelID()
		models[i].APIKey = apiKey.GetText()
		if v, err := strconv.ParseFloat(temp.GetText(), 32); err == nil {
			models[i].Temperature = float32(v)
		}
		models[i].MaxTokens = atoiOr(maxTokens.GetText(), models[i].MaxTokens)
		models[i].ReasoningEffort = strings.TrimSpace(reasoningEffort.GetText())
		models[i].Thinking = thinkingValue(thinking.Value())
	}
	scanModels = func() {
		if w.handlers.ScanModels == nil {
			w.showConfirm("Scan", "Model scanning is unavailable.", nil)
			return
		}
		store(cur)
		target := cur
		draft := models[cur]
		// Query off the UI thread so a slow backend can't freeze the dialog.
		go func() {
			ids, err := w.handlers.ScanModels(draft)
			w.desktop.Post(func() {
				if err != nil {
					w.showConfirm("Scan", "Failed to list models:\n"+err.Error(), nil)
					return
				}
				if target != cur {
					return
				}
				modelSelect.Options = ids
				modelSelect.SetSelected(indexOrZero(ids, models[cur].Model))
				modelID.Root().Visible = false
				modelSelect.Root().Visible = true
				w.desktop.SetFocus(modelSelect)
			})
		}()
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
			w.showConfirm("Models", "Some models failed to save:\n"+failed, nil)
		}
	}
	cancel := func() { w.desktop.RemoveLayer(layer) }

	dialog.Window.AddContent(newButton("Save", tv.Rect{X: width - 24, Y: height - 3, W: 9, H: 1}, save))
	dialog.Window.AddContent(newButton("Cancel", tv.Rect{X: width - 13, Y: height - 3, W: 10, H: 1}, cancel))

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

// thinkingIndex maps a ModelConfig.Thinking pointer to its index in the
// "default"/"on"/"off" select (nil => default).
func thinkingIndex(t *bool) int {
	switch {
	case t == nil:
		return 0
	case *t:
		return 1
	default:
		return 2
	}
}

// thinkingValue maps the "default"/"on"/"off" select value back to a
// ModelConfig.Thinking pointer (default => nil, leaving the param unset).
func thinkingValue(v string) *bool {
	switch v {
	case "on":
		on := true
		return &on
	case "off":
		off := false
		return &off
	default:
		return nil
	}
}

// indexOrZero returns the index of value in opts, or 0 (the default option) when
// it is absent or empty.
func indexOrZero(opts []string, value string) int {
	for i, o := range opts {
		if o == value {
			return i
		}
	}
	return 0
}
