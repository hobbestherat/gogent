package ui

import (
	"strconv"
	"strings"

	"gogent/internal/config"

	tui "github.com/hobbestherat/turbotui"
	tv "github.com/hobbestherat/turbotui/turbotv"
)

// showCapsForm edits a ModelCapabilities snapshot by hand — the manual fallback for
// catalog-less / local models (and for tweaking a discovered snapshot). It covers
// the fields that drive behaviour and the selectors: context window, output cap,
// reasoning/thinking/vision/tool-call flags, and the effort options. Pricing and
// modalities are left to discovery (the catalog) and are preserved unchanged.
func (w *Workbench) showCapsForm(initial config.ModelCapabilities, onSave func(config.ModelCapabilities)) {
	const labelW = 18
	const boxX = 2 + labelW
	const formHeight = 13

	spec := tv.DialogSpec{MinW: 64, MinH: formHeight, MaxH: formHeight, PreferredW: 72}
	x, y, width, height := w.dialogRect(spec)
	boxW := width - boxX - 3

	dialog := tv.NewDialog("Capabilities", x, y, width, height)
	applyWindowShadow(dialog.Window)
	dialog.Window.ShowClose = false

	field := func(text string, row int) *tv.TextBox {
		dialog.Window.AddContent(dialogLabel(text, tv.Rect{X: 2, Y: row, W: labelW, H: 1}))
		box := tv.NewTextBox("", tv.Rect{X: boxX, Y: row, W: boxW, H: 1})
		dialog.Window.AddContent(box)
		return box
	}
	yesNo := func(text string, row int, on bool) *tv.Select {
		dialog.Window.AddContent(dialogLabel(text, tv.Rect{X: 2, Y: row, W: labelW, H: 1}))
		sel := newSelect(w.desktop, []string{"no", "yes"}, tv.Rect{X: boxX, Y: row, W: 8, H: 1})
		if on {
			sel.SetSelected(1)
		}
		dialog.Window.AddContent(sel)
		return sel
	}

	contextWin := field("Context window:", 1)
	contextWin.SetText(strconv.Itoa(initial.ContextWindow))
	maxOut := field("Max output:", 2)
	maxOut.SetText(strconv.Itoa(initial.MaxOutput))
	effort := field("Effort options:", 3)
	effort.SetText(strings.Join(initial.EffortOptions, ", "))
	reasoning := yesNo("Reasoning:", 4, initial.Reasoning)
	thinkingToggle := yesNo("Thinking toggle:", 5, initial.ThinkingToggle)
	vision := yesNo("Vision:", 6, initial.Vision)
	toolCall := yesNo("Tool calling:", 7, initial.ToolCall)

	dialog.Window.AddContent(dialogLabel("Effort options: comma-separated (e.g. low, medium, high).",
		tv.Rect{X: 2, Y: 9, W: width - 4, H: 1}))

	var layer *tv.Layer
	cancel := func() { w.desktop.RemoveLayer(layer) }
	save := func() {
		caps := initial // preserve pricing/modalities/knowledge not edited here
		caps.ContextWindow = atoiOr(contextWin.GetText(), 0)
		caps.MaxOutput = atoiOr(maxOut.GetText(), 0)
		caps.EffortOptions = splitCSV(effort.GetText())
		caps.Reasoning = reasoning.Value() == "yes"
		caps.ThinkingToggle = thinkingToggle.Value() == "yes"
		caps.Vision = vision.Value() == "yes"
		caps.ToolCall = toolCall.Value() == "yes"
		caps.Source = "manual"
		w.desktop.RemoveLayer(layer)
		if onSave != nil {
			onSave(caps)
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

	layer = tv.NewModalLayer("caps-form", dialog)
	w.desktop.AddLayer(layer)
	dialog.Fit(spec)
	w.desktop.SetFocus(contextWin)
}

// splitCSV splits a comma-separated list into trimmed, non-empty values.
func splitCSV(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		if v := strings.TrimSpace(part); v != "" {
			out = append(out, v)
		}
	}
	return out
}
