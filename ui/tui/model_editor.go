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
	// down to the optional per-model Model timeout) plus the button row at
	// height-3. The layout never grows vertically, so the height is pinned here
	// rather than inflating to the 85% vertical default (issue #309). The Vertex AI
	// rows (Project, Location) and the model-timeout row (issue #590) are always
	// shown, so the form is 20 rows tall.
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
	// dialog). A dropdown of configured connections, falling back to a text field
	// when none are wired/known.
	connNames := w.connectionNames()
	dialog.Window.AddContent(dialogLabel("Connection:", tv.Rect{X: 2, Y: 3, W: labelW, H: 1}))
	var connectionSel *tv.Select
	var connectionBox *tv.TextBox
	if len(connNames) > 0 {
		connectionSel = newSelect(w.desktop, connNames, tv.Rect{X: boxX, Y: 3, W: boxW, H: 1})
		connectionSel.SetSelected(indexOrZero(connNames, initial.Connection))
		dialog.Window.AddContent(connectionSel)
	} else {
		connectionBox = tv.NewTextBox("", tv.Rect{X: boxX, Y: 3, W: boxW, H: 1})
		connectionBox.SetText(initial.Connection)
		dialog.Window.AddContent(connectionBox)
	}
	currentConnection := func() string {
		if connectionSel != nil {
			return connectionSel.Value()
		}
		return strings.TrimSpace(connectionBox.GetText())
	}

	// Model id: a text field, with a Discover button that merges the connection's
	// live listing with the catalog and, on success, swaps the text field for a
	// dropdown of advertised models (✓ available / ⚠ catalog-only); picking one
	// also auto-fills its capability snapshot. Discovery works on both add and edit
	// since it is keyed by the connection, not a saved model name.
	dialog.Window.AddContent(dialogLabel("Model id:", tv.Rect{X: 2, Y: 5, W: labelW, H: 1}))
	scanEnabled := w.handlers.DiscoverModels != nil
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
	// capsByID carries the discovered capability snapshot for each model id, applied
	// to the saved config when the user picks a discovered model. The "⚠ " prefix on
	// catalog-only rows is stripped here.
	capsByID := map[string]config.ModelCapabilities{}
	stripFlag := func(s string) string { return strings.TrimPrefix(strings.TrimPrefix(s, "⚠ "), "✓ ") }
	currentModelID := func() string {
		if modelSelect.Root().Visible {
			return stripFlag(modelSelect.Value())
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

	// Capabilities: edited via a sub-form for catalog-less / local models (or to tweak
	// a discovered snapshot). manualCaps holds the override once the user opens the
	// form; otherwise discovery / the carried-through initial caps win (see save).
	var manualCaps config.ModelCapabilities
	manualCapsEdited := false
	bestCaps := func() config.ModelCapabilities {
		if c, ok := capsByID[currentModelID()]; ok {
			return c
		}
		return initial.Caps
	}
	dialog.Window.AddContent(dialogLabel("Capabilities:", tv.Rect{X: 2, Y: 11, W: labelW, H: 1}))
	dialog.Window.AddContent(newButton("Edit…", tv.Rect{X: boxX, Y: 11, W: 9, H: 1}, func() {
		seed := manualCaps
		if !manualCapsEdited {
			seed = bestCaps()
		}
		w.showCapsForm(seed, func(c config.ModelCapabilities) {
			manualCaps = c
			manualCapsEdited = true
		})
	}))

	// Model timeout (issue #590): an optional per-model override of the global
	// model-request timeout, for slow local models. Blank/0 means "use the global
	// timeout" — shown blank (not "0") so it reads as unset.
	modelTimeout := field("Model timeout:", 13)
	if initial.ModelTimeoutSeconds > 0 {
		modelTimeout.SetText(strconv.Itoa(initial.ModelTimeoutSeconds))
	}

	// Discover: merge the selected connection's live listing with the catalog off the
	// UI thread so a slow backend can't freeze the dialog. On success swap the model
	// text box for a dropdown of advertised models, flagged ✓ available / ⚠
	// catalog-only, and remember each model's caps for auto-fill on save.
	if scanEnabled {
		discover := func() {
			connName := currentConnection()
			if connName == "" {
				w.showConfirm("Discover", "Choose a connection first.", nil)
				return
			}
			go func() {
				ds, err := w.handlers.DiscoverModels(connName)
				w.desktop.Post(func() {
					if err != nil {
						w.showConfirm("Discover", "Failed to discover models:\n"+err.Error(), nil)
						return
					}
					opts := make([]string, 0, len(ds))
					capsByID = map[string]config.ModelCapabilities{}
					for _, d := range ds {
						label := "✓ " + d.ID
						if !d.Available {
							label = "⚠ " + d.ID
						}
						opts = append(opts, label)
						capsByID[d.ID] = d.Caps
					}
					if len(opts) == 0 {
						w.showConfirm("Discover", "The connection returned no models.", nil)
						return
					}
					modelSelect.Options = opts
					modelSelect.SetSelected(0)
					modelID.Root().Visible = false
					modelSelect.Root().Visible = true
					w.desktop.SetFocus(modelSelect)
				})
			}()
		}
		dialog.Window.AddContent(newButton("Scan",
			tv.Rect{X: boxX + modelBoxW + 1, Y: 5, W: modelFormScanW, H: 1}, discover))
	}

	var layer *tv.Layer
	cancel := func() { w.desktop.RemoveLayer(layer) }
	save := func() {
		cfg := initial
		if nameEditable {
			cfg.Name = strings.TrimSpace(nameBox.GetText())
		}
		cfg.DisplayName = display.GetText()
		cfg.Connection = currentConnection()
		cfg.Model = strings.TrimSpace(currentModelID())
		// Capability precedence: a manual edit wins; else a discovered snapshot for the
		// chosen model; else the caps carried through from initial.
		switch {
		case manualCapsEdited:
			cfg.Caps = manualCaps
		default:
			if caps, ok := capsByID[cfg.Model]; ok {
				cfg.Caps = caps
			}
		}
		if v, err := strconv.ParseFloat(temp.GetText(), 32); err == nil {
			cfg.Temperature = float32(v)
		}
		cfg.MaxTokens = atoiOr(maxTokens.GetText(), cfg.MaxTokens)
		cfg.ReasoningEffort = strings.TrimSpace(reasoningEffort.GetText())
		cfg.Thinking = thinkingValue(thinking.Value())
		// Blank clears the model-timeout override (0 = use global); a valid
		// non-negative value sets it; garbage leaves the prior value untouched.
		if txt := strings.TrimSpace(modelTimeout.GetText()); txt == "" {
			cfg.ModelTimeoutSeconds = 0
		} else if v, err := strconv.Atoi(txt); err == nil && v >= 0 {
			cfg.ModelTimeoutSeconds = v
		}

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
