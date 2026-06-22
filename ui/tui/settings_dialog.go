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

// showSettingsDialog opens the modal sub-agent settings dialog. The delegation
// style is a mutually-exclusive Checkbox radio group of three: Both (blocking +
// fire-and-forget, the default — issue #284), One-shot only (blocking) and
// Interactive only (async, experimental). Below it are independent toggles/fields
// for recursion, the fan-out and depth limits, and the model / tool / sub-agent
// timeouts. Values are written back only on OK. The dialog opens reflecting the
// persisted config.
func (w *Workbench) showSettingsDialog() {
	if w.handlers.GetSettings == nil || w.handlers.SetSettings == nil {
		w.showConfirm("Settings", "Sub-agent settings are unavailable.", nil)
		return
	}
	cur := w.handlers.GetSettings()
	timeouts := config.DefaultTimeoutConfig()
	if w.handlers.GetTimeouts != nil {
		timeouts = w.handlers.GetTimeouts()
	}

	// A fixed-form dialog (a static column of checkboxes and numeric fields plus an
	// OK/Cancel row; no scrolling, no growing content), so it is PINNED to its content
	// footprint rather than left to fill the 80%×85% box (issue #317). PreferredW=72
	// fits the longest label ("&Both: …", 60 cells) with breathing room on any terminal
	// wide enough to honour it (≥90 cols); MaxW caps it so it never balloons to 160. On a
	// narrow 80-col terminal the 80% cap still forces width down to 64 and the longest
	// label clips — inherent to the cap policy, as before — so MinW=64 is the floor. The
	// height is pinned at 20: the last field is at Y=15 and the button row at height-3, so
	// 20 leaves one blank gap. The spec is static (no terminal-dependent fields), so the
	// dialog.Fit re-resolve of the frame on resize stays correct.
	spec := tv.DialogSpec{MinW: 64, MaxW: 76, PreferredW: 72, MinH: 20, MaxH: 20}
	x, y, width, height := w.dialogRect(spec)

	dialog := tv.NewDialog("Sub-agent Settings", x, y, width, height)
	applyWindowShadow(dialog.Window) // honour the NoShadow theme setting (issue #215)
	dialog.Window.ShowClose = false

	styleCheck := func(cb *tv.Checkbox) *tv.Checkbox {
		cb.FG = tv.DefaultTheme.DialogFG
		cb.BG = tv.DefaultTheme.DialogBG
		return cb
	}

	modelLabel := dialogLabel("Delegation tools (blocking / fire-and-forget):", tv.Rect{X: 2, Y: 1, W: width - 4, H: 1})
	// Three mutually-exclusive styles. "Both" (the default, issue #284) exposes the
	// blocking spawn_subagent AND the async launch_agent family in one session, so
	// the agent can wait on batched work or kick off background research and keep
	// going. One-shot is blocking only; interactive is async only.
	both := styleCheck(tv.NewCheckbox("&Both: block when you'll wait, fire-and-forget when you won't", tv.Rect{X: 4, Y: 2, W: width - 8, H: 1}, nil))
	oneShot := styleCheck(tv.NewCheckbox("&One-shot only (blocking spawn_subagent)", tv.Rect{X: 4, Y: 3, W: width - 8, H: 1}, nil))
	interactive := styleCheck(tv.NewCheckbox("&Interactive only (async launch_agent, experimental)", tv.Rect{X: 4, Y: 4, W: width - 8, H: 1}, nil))
	recursive := styleCheck(tv.NewCheckbox("Allow &recursive agents", tv.Rect{X: 2, Y: 6, W: width - 4, H: 1}, nil))

	// Diff-review approval gate (issue #64), an independent toggle below the
	// timeout fields.
	reviewEdits := styleCheck(tv.NewCheckbox("Re&view edits before applying (show diff)", tv.Rect{X: 2, Y: 15, W: width - 4, H: 1}, nil))
	if w.handlers.GetReviewEdits != nil {
		reviewEdits.SetChecked(w.handlers.GetReviewEdits())
	}

	// Reflect the persisted style: exactly one of the three is checked.
	both.SetChecked(cur.ExposesOneShotTools() && cur.ExposesInteractiveTools())
	oneShot.SetChecked(cur.ExposesOneShotTools() && !cur.ExposesInteractiveTools())
	interactive.SetChecked(!cur.ExposesOneShotTools() && cur.ExposesInteractiveTools())

	// Mutual exclusion: the three style checkboxes form a radio group. Selecting
	// one clears the others; a checkbox cannot be unchecked into "none".
	styles := []*tv.Checkbox{both, oneShot, interactive}
	for _, cb := range styles {
		sel := cb
		sel.OnToggle = func(checked bool) {
			if !checked {
				sel.SetChecked(true) // keep one always selected
				return
			}
			for _, other := range styles {
				if other != sel {
					other.SetChecked(false)
				}
			}
			w.desktop.Redraw()
		}
	}

	const labelW = 22
	const boxW = 6
	maxAgents := newNumField("Max sub-agents:", cur.MaxSubAgentsOrDefault(), 2, 7, labelW, boxW)
	maxDepth := newNumField("Max recursion depth:", cur.MaxDepthOrDefault(), 2, 8, labelW, boxW)
	timeoutsLabel := dialogLabel("Timeouts (seconds):", tv.Rect{X: 2, Y: 10, W: width - 4, H: 1})
	modelTO := newNumField("Model timeout:", timeouts.ModelSecondsOrDefault(), 2, 11, labelW, boxW)
	toolTO := newNumField("Tool timeout:", timeouts.ToolSecondsOrDefault(), 2, 12, labelW, boxW)
	subTO := newNumField("Sub-agent timeout:", timeouts.SubAgentSecondsOrDefault(), 2, 13, labelW, boxW)

	dialog.Window.AddContent(modelLabel)
	dialog.Window.AddContent(both)
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
		switch {
		case interactive.IsChecked():
			cfg.ExecutionModel = config.SubAgentInteractiveModel
		case oneShot.IsChecked():
			cfg.ExecutionModel = config.SubAgentOneShotModel
		default:
			cfg.ExecutionModel = config.SubAgentBothModel
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

	ok := newButton("OK", tv.Rect{X: width - 24, Y: height - 3, W: 9, H: 1}, apply)
	cancelBtn := newButton("Cancel", tv.Rect{X: width - 13, Y: height - 3, W: 10, H: 1}, cancel)
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
	dialog.Fit(spec) // re-resolve the rect when the terminal is resized (issue #299)
	w.desktop.SetFocus(both)
}
