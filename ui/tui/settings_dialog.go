package ui

import (
	"strconv"
	"strings"

	"gogent/internal/config"

	tui "github.com/hobbestherat/turbotui"
	tv "github.com/hobbestherat/turbotui/turbotv"
)

// atoiOr parses a base-10 int from a textbox, falling back to def on any error
// or non-positive value, so a blank/garbage field can't wipe a limit/timeout.
func atoiOr(s string, def int) int {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil || n <= 0 {
		return def
	}
	return n
}

// centeredDialog returns the top-left corner that centers a width×height dialog.
func centeredDialog(w *Workbench, width, height int) (int, int) {
	x := (w.app.Width() - width) / 2
	y := (w.app.Height() - height) / 2
	if x < 0 {
		x = 0
	}
	if y < 0 {
		y = 0
	}
	return x, y
}

// dialogLabel builds a label coloured for a dialog background.
func dialogLabel(text string, r tv.Rect) *tv.Label {
	l := tv.NewLabel(text, r)
	l.FG = tv.DefaultTheme.DialogFG
	l.BG = tv.DefaultTheme.DialogBG
	return l
}

// numField is a labelled single-line numeric TextBox used in the settings form.
type numField struct {
	label *tv.Label
	box   *tv.TextBox
}

// newNumField creates a labelled numeric field and positions both parts: the
// label occupies labelW columns at (x,y); the box follows it (boxW wide).
func newNumField(text string, value, x, y, labelW, boxW int) *numField {
	f := &numField{
		label: dialogLabel(text, tv.Rect{X: x, Y: y, W: labelW, H: 1}),
		box:   tv.NewTextBox(strconv.Itoa(value), tv.Rect{X: x + labelW, Y: y, W: boxW, H: 1}),
	}
	return f
}

// showSettingsDialog opens the modal sub-agent settings dialog. The execution
// model is a mutually-exclusive Checkbox pair (one-shot vs interactive); below
// it are independent toggles/fields for recursion, the fan-out and depth limits,
// and the model / tool / sub-agent timeouts. Values are written back only on OK.
//
// One-shot agents stay the default until the interactive model is proven stable,
// which is why the dialog opens reflecting the persisted config.
func (w *Workbench) showSettingsDialog() {
	if w.handlers.GetSettings == nil || w.handlers.SetSettings == nil {
		showConfirmDialog(w.desktop, "Settings", "Sub-agent settings are unavailable.", nil)
		return
	}
	cur := w.handlers.GetSettings()
	timeouts := config.DefaultTimeoutConfig()
	if w.handlers.GetTimeouts != nil {
		timeouts = w.handlers.GetTimeouts()
	}

	const width = 56
	const height = 20
	x, y := centeredDialog(w, width, height)

	dialog := tv.NewDialog("Sub-agent Settings", x, y, width, height)
	dialog.Window.ShowClose = false

	styleCheck := func(cb *tv.Checkbox) *tv.Checkbox {
		cb.FG = tv.DefaultTheme.DialogFG
		cb.BG = tv.DefaultTheme.DialogBG
		return cb
	}

	modelLabel := dialogLabel("Execution model:", tv.Rect{X: 2, Y: 1, W: width - 4, H: 1})
	oneShot := styleCheck(tv.NewCheckbox("&One-shot agents", tv.Rect{X: 4, Y: 2, W: width - 8, H: 1}, nil))
	interactive := styleCheck(tv.NewCheckbox("&Interactive agents (experimental)", tv.Rect{X: 4, Y: 3, W: width - 8, H: 1}, nil))
	recursive := styleCheck(tv.NewCheckbox("Allow &recursive agents", tv.Rect{X: 2, Y: 5, W: width - 4, H: 1}, nil))

	// Diff-review approval gate (issue #64), an independent toggle below the
	// timeout fields.
	reviewEdits := styleCheck(tv.NewCheckbox("Re&view edits before applying (show diff)", tv.Rect{X: 2, Y: 14, W: width - 4, H: 1}, nil))
	if w.handlers.GetReviewEdits != nil {
		reviewEdits.SetChecked(w.handlers.GetReviewEdits())
	}

	oneShot.SetChecked(cur.IsOneShot())
	interactive.SetChecked(!cur.IsOneShot())
	recursive.SetChecked(cur.AllowRecursive)

	// Mutual exclusion: the execution-model checkboxes form a radio pair.
	oneShot.OnToggle = func(checked bool) {
		interactive.SetChecked(!checked)
		w.desktop.Redraw()
	}
	interactive.OnToggle = func(checked bool) {
		oneShot.SetChecked(!checked)
		w.desktop.Redraw()
	}

	const labelW = 22
	const boxW = 6
	maxAgents := newNumField("Max sub-agents:", cur.MaxSubAgentsOrDefault(), 2, 6, labelW, boxW)
	maxDepth := newNumField("Max recursion depth:", cur.MaxDepthOrDefault(), 2, 7, labelW, boxW)
	timeoutsLabel := dialogLabel("Timeouts (seconds):", tv.Rect{X: 2, Y: 9, W: width - 4, H: 1})
	modelTO := newNumField("Model timeout:", timeouts.ModelSecondsOrDefault(), 2, 10, labelW, boxW)
	toolTO := newNumField("Tool timeout:", timeouts.ToolSecondsOrDefault(), 2, 11, labelW, boxW)
	subTO := newNumField("Sub-agent timeout:", timeouts.SubAgentSecondsOrDefault(), 2, 12, labelW, boxW)

	dialog.Window.AddContent(modelLabel)
	dialog.Window.AddContent(oneShot)
	dialog.Window.AddContent(interactive)
	dialog.Window.AddContent(recursive)
	dialog.Window.AddContent(reviewEdits)
	dialog.Window.AddContent(timeoutsLabel)
	for _, f := range []*numField{maxAgents, maxDepth, modelTO, toolTO, subTO} {
		dialog.Window.AddContent(f.label)
		dialog.Window.AddContent(f.box)
	}

	var layer *tv.Layer
	apply := func() {
		cfg := w.handlers.GetSettings()
		if interactive.IsChecked() {
			cfg.ExecutionModel = config.SubAgentInteractiveModel
		} else {
			cfg.ExecutionModel = config.SubAgentOneShotModel
		}
		cfg.AllowRecursive = recursive.IsChecked()
		cfg.MaxSubAgents = atoiOr(maxAgents.box.GetText(), cur.MaxSubAgentsOrDefault())
		cfg.MaxDepth = atoiOr(maxDepth.box.GetText(), cur.MaxDepthOrDefault())
		w.handlers.SetSettings(cfg)

		if w.handlers.SetReviewEdits != nil {
			w.handlers.SetReviewEdits(reviewEdits.IsChecked())
		}

		if w.handlers.SetTimeouts != nil {
			t := timeouts
			t.ModelSeconds = atoiOr(modelTO.box.GetText(), timeouts.ModelSecondsOrDefault())
			t.ToolSeconds = atoiOr(toolTO.box.GetText(), timeouts.ToolSecondsOrDefault())
			t.SubAgentSeconds = atoiOr(subTO.box.GetText(), timeouts.SubAgentSecondsOrDefault())
			w.handlers.SetTimeouts(t)
		}

		w.desktop.RemoveLayer(layer)
		w.rebuildMenu()
	}
	cancel := func() { w.desktop.RemoveLayer(layer) }

	ok := tv.NewButton("OK", tv.Rect{X: width - 24, Y: height - 3, W: 9, H: 1}, apply)
	cancelBtn := tv.NewButton("Cancel", tv.Rect{X: width - 13, Y: height - 3, W: 10, H: 1}, cancel)
	dialog.Window.AddContent(ok)
	dialog.Window.AddContent(cancelBtn)

	// Escape anywhere in the dialog cancels without applying.
	dialog.Root().OnTypeFn = func(_ *tv.VisualComponent, event tui.TypeEvent) bool {
		if event.Key == tui.KeyEscape {
			cancel()
			return true
		}
		return false
	}

	layer = tv.NewModalLayer("settings-dialog", dialog)
	w.desktop.AddLayer(layer)
	w.desktop.SetFocus(oneShot)
}
