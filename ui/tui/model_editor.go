package ui

import (
	"fmt"
	"strconv"
	"strings"

	"gogent/internal/config"

	tui "github.com/hobbestherat/turbotui"
	tv "github.com/hobbestherat/turbotui/turbotv"
)

// modelEditor layout constants: the field labels occupy labelW cells from X=2,
// and the boxed fields (and the model Select) start at boxX. boxW is derived as
// width - boxX - 3 so the fields and the trailing Scan button fit the dialog.
const (
	modelEditorLabelW = 16
	modelEditorBoxX   = 2 + modelEditorLabelW
	// modelEditorMinWidth is the comfort floor when the widest option is short.
	modelEditorMinWidth = 64
	// modelEditorHeight is the model form's fixed footprint: the field rows (Name
	// down to Location) plus the button row at height-3. The layout never grows
	// vertically, so the height is pinned here rather than inflating to the 85%
	// vertical default (issue #309). The Vertex AI rows (Project, Location) are
	// always shown, so the form is 20 rows tall.
	modelEditorHeight = 20
	// modelFormScanW is the width of the model-id Scan button (edit mode only).
	modelFormScanW = 8
)

// longestRuneLen returns the display width (in cells) of the widest string in ss,
// measured with tui.StringWidth so CJK/emoji model names are sized by the cells
// they occupy rather than their rune count.
func longestRuneLen(ss []string) int {
	max := 0
	for _, s := range ss {
		if l := tui.StringWidth(s); l > max {
			max = l
		}
	}
	return max
}

// showModelForm opens the shared add/edit model form — the single field-set
// builder used by BOTH the Models… dialog's Edit (nameEditable=false, onSave =
// UpdateModel) and its Add Empty… path (nameEditable=true, onSave = AddModel) —
// so the field list lives in one place. initial pre-fills every field; on Save
// the assembled config is validated and handed to onSave, and when onSave
// succeeds the form closes and onSaved runs (the caller refreshes the list and
// the live dropdowns).
//
// Scan (model-id discovery) is offered ONLY in edit mode: it needs a model that
// already exists on the backend (the remote /models/:name/scan route is keyed by
// a SAVED name), so a not-yet-created draft can't be scanned in remote mode. To
// keep embedded and remote identical we hide Scan whenever the name is editable
// (i.e. an Add). The API key is left optional (an empty key on Edit preserves the
// daemon's stored key, mirroring the prior manual editor).
func (w *Workbench) showModelForm(title string, initial config.ModelConfig, nameEditable bool, onSave func(config.ModelConfig) error, onSaved func()) {
	const labelW = modelEditorLabelW
	const boxX = modelEditorBoxX

	spec := tv.DialogSpec{MinW: 70, MinH: modelEditorHeight, MaxH: modelEditorHeight, PreferredW: 84}
	x, y, width, height := w.dialogRect(spec)
	boxW := width - boxX - 3

	dialog := tv.NewDialog(title, x, y, width, height)
	applyWindowShadow(dialog.Window) // honour the NoShadow theme setting (issue #215)
	dialog.Window.ShowClose = false

	// field builds a labelled text field, registers it on the dialog, and returns
	// its box for later get/set.
	field := func(text string, row int) *tv.TextBox {
		dialog.Window.AddContent(dialogLabel(text, tv.Rect{X: 2, Y: row, W: labelW, H: 1}))
		box := tv.NewTextBox("", tv.Rect{X: boxX, Y: row, W: boxW, H: 1})
		dialog.Window.AddContent(box)
		return box
	}

	// Name: editable on Add (it is the new entry's key, kept unique), read-only on
	// Edit (UpdateModel matches on it; a rename would orphan the old entry, so a
	// rename is intentionally done via remove + add).
	dialog.Window.AddContent(dialogLabel("Name:", tv.Rect{X: 2, Y: 1, W: labelW, H: 1}))
	var nameBox *tv.TextBox
	if nameEditable {
		nameBox = tv.NewTextBox("", tv.Rect{X: boxX, Y: 1, W: boxW, H: 1})
		nameBox.SetText(initial.Name)
		dialog.Window.AddContent(nameBox)
	} else {
		dialog.Window.AddContent(dialogLabel(initial.Name, tv.Rect{X: boxX, Y: 1, W: boxW, H: 1}))
	}

	display := field("Display name:", 2)
	display.SetText(initial.DisplayName)

	// Connection: the credentialed provider connection this model talks through
	// (credentials/api_type/endpoint live there now, edited in the Connections…
	// dialog). A text field for now; stage 5b turns this into a picker.
	connection := field("Connection:", 3)
	connection.SetText(initial.Connection)

	// Model id: a text field, with a Scan button (edit mode only) that discovers the
	// connection's models and, on success, swaps the text field for a dropdown.
	dialog.Window.AddContent(dialogLabel("Model id:", tv.Rect{X: 2, Y: 5, W: labelW, H: 1}))
	scanEnabled := !nameEditable && w.handlers.ScanModels != nil
	modelBoxW := boxW
	if scanEnabled {
		modelBoxW = boxW - modelFormScanW - 1
	}
	modelRect := tv.Rect{X: boxX, Y: 5, W: modelBoxW, H: 1}
	modelID := tv.NewTextBox("", modelRect)
	modelID.SetText(initial.Model)
	dialog.Window.AddContent(modelID)
	modelSelect := newSelect(w.desktop, nil, modelRect)
	modelSelect.Root().Visible = false
	dialog.Window.AddContent(modelSelect)
	currentModelID := func() string {
		if modelSelect.Root().Visible {
			return modelSelect.Value()
		}
		return modelID.GetText()
	}

	temp := field("Temperature:", 7)
	temp.SetText(strconv.FormatFloat(float64(initial.Temperature), 'g', -1, 32))
	maxTokens := field("Max tokens:", 8)
	maxTokens.SetText(strconv.Itoa(initial.MaxTokens))
	reasoningEffort := field("Reasoning:", 9)
	reasoningEffort.SetText(initial.ReasoningEffort)

	// Thinking is tri-state: "default" leaves the toggle unset (provider default),
	// "on"/"off" force it. Only providers that understand it actually send it (see
	// buildRequest), so it is safe to show for every model.
	dialog.Window.AddContent(dialogLabel("Thinking:", tv.Rect{X: 2, Y: 10, W: labelW, H: 1}))
	thinking := newSelect(w.desktop, []string{"default", "on", "off"}, tv.Rect{X: boxX, Y: 10, W: boxW, H: 1})
	thinking.SetSelected(thinkingIndex(initial.Thinking))
	dialog.Window.AddContent(thinking)

	// Scan (edit mode only): discover the connection's models off the UI thread so a
	// slow backend can't freeze the dialog.
	if scanEnabled {
		scanModels := func() {
			draft := initial
			draft.Connection = strings.TrimSpace(connection.GetText())
			draft.Model = currentModelID()
			go func() {
				ids, err := w.handlers.ScanModels(draft)
				w.desktop.Post(func() {
					if err != nil {
						w.showConfirm("Scan", "Failed to list models:\n"+err.Error(), nil)
						return
					}
					modelSelect.Options = ids
					modelSelect.SetSelected(indexOrZero(ids, currentModelID()))
					modelID.Root().Visible = false
					modelSelect.Root().Visible = true
					w.desktop.SetFocus(modelSelect)
				})
			}()
		}
		dialog.Window.AddContent(newButton("Scan",
			tv.Rect{X: boxX + modelBoxW + 1, Y: 5, W: modelFormScanW, H: 1}, scanModels))
	}

	var layer *tv.Layer
	cancel := func() { w.desktop.RemoveLayer(layer) }
	save := func() {
		cfg := initial
		if nameEditable {
			cfg.Name = strings.TrimSpace(nameBox.GetText())
		}
		cfg.DisplayName = display.GetText()
		cfg.Connection = strings.TrimSpace(connection.GetText())
		cfg.Model = strings.TrimSpace(currentModelID())
		if v, err := strconv.ParseFloat(temp.GetText(), 32); err == nil {
			cfg.Temperature = float32(v)
		}
		cfg.MaxTokens = atoiOr(maxTokens.GetText(), cfg.MaxTokens)
		cfg.ReasoningEffort = strings.TrimSpace(reasoningEffort.GetText())
		cfg.Thinking = thinkingValue(thinking.Value())
		// cfg.Caps is carried through from initial (set by discovery / catalog).

		if cfg.Name == "" {
			w.showConfirm("Model", "A unique model name is required.", nil)
			return
		}
		if cfg.Model == "" {
			w.showConfirm("Model", "A model id is required.", nil)
			return
		}
		// On Add, reject a name that already exists up front (AddModel re-checks and
		// is the final authority); Edit keeps its stable name so it never collides.
		if nameEditable && w.takenModelNames()[cfg.Name] {
			w.showConfirm("Model", fmt.Sprintf("A model named %q already exists.", cfg.Name), nil)
			return
		}
		if err := onSave(cfg); err != nil {
			w.showConfirm("Model", "Could not save model:\n"+err.Error(), nil)
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

	layer = tv.NewModalLayer("model-form", dialog)
	w.desktop.AddLayer(layer)
	dialog.Fit(spec) // re-resolve the rect when the terminal is resized (issue #299)
	if nameEditable {
		w.desktop.SetFocus(nameBox)
	} else {
		w.desktop.SetFocus(display)
	}
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
