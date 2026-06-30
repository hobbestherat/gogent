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
// for recursion and the fan-out and depth limits. The model / tool / sub-agent
// timeouts moved to their own discoverable Timeouts dialog (see showTimeoutsDialog,
// issue #590); they are no longer buried here. Values are written back only on OK.
// The dialog opens reflecting the persisted config.
func (w *Workbench) showSettingsDialog() {
	if w.handlers.GetSettings == nil || w.handlers.SetSettings == nil {
		w.showConfirm("Settings", "Sub-agent settings are unavailable.", nil)
		return
	}
	cur := w.handlers.GetSettings()

	// A fixed-form dialog (a static column of checkboxes and numeric fields plus an
	// OK/Cancel row; no scrolling, no growing content), so it is PINNED to its content
	// footprint rather than left to fill the 80%×85% box (issue #317). PreferredW=72
	// fits the longest label ("&Both: …", 60 cells) with breathing room on any terminal
	// wide enough to honour it (≥90 cols); MaxW caps it so it never balloons to 160. On a
	// narrow 80-col terminal the 80% cap still forces width down to 64 and the longest
	// label clips — inherent to the cap policy, as before — so MinW=64 is the floor. The
	// height is pinned at 16: with the timeout fields relocated (issue #590) the last
	// toggles are the review-edits gate at Y=10 and the startup-welcome toggle at Y=11,
	// with the button row at height-3 (Y=13). The spec is static (no terminal-dependent
	// fields), so the dialog.Fit re-resolve of the frame on resize stays correct.
	spec := tv.DialogSpec{MinW: 64, MaxW: 76, PreferredW: 72, MinH: 16, MaxH: 16}
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
	// sub-agent limits.
	reviewEdits := styleCheck(tv.NewCheckbox("Re&view edits before applying (show diff)", tv.Rect{X: 2, Y: 10, W: width - 4, H: 1}, nil))
	if w.handlers.GetReviewEdits != nil {
		reviewEdits.SetChecked(w.handlers.GetReviewEdits())
	}

	// Startup welcome dialog preference (issues #339/#341/#342), an independent
	// toggle alongside the review-edits gate. It is wired to the same
	// GetShowWelcome / SetShowWelcome handlers the welcome dialog itself uses, so
	// the startup preference can be toggled from settings too — not only from the
	// welcome dialog's own "Don't show this on startup again" checkbox (issue #339
	// acceptance). Checked means the dialog is shown on startup.
	showWelcome := styleCheck(tv.NewCheckbox("Show &welcome dialog on startup", tv.Rect{X: 2, Y: 11, W: width - 4, H: 1}, nil))
	if w.handlers.GetShowWelcome != nil {
		showWelcome.SetChecked(w.handlers.GetShowWelcome())
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

	dialog.Window.AddContent(modelLabel)
	dialog.Window.AddContent(both)
	dialog.Window.AddContent(oneShot)
	dialog.Window.AddContent(interactive)
	dialog.Window.AddContent(recursive)
	dialog.Window.AddContent(reviewEdits)
	dialog.Window.AddContent(showWelcome)
	for _, f := range []*numField{maxAgents, maxDepth} {
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

		if w.handlers.SetShowWelcome != nil {
			w.handlers.SetShowWelcome(showWelcome.IsChecked())
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

// showTimeoutsDialog opens the modal Timeouts dialog (issue #590): the dedicated,
// discoverable home for the model / tool / sub-agent timeouts that used to be
// buried inside the Sub-agent Settings dialog. It is a small fixed-form dialog of
// three labelled numeric fields (seconds) plus a hint line and an OK/Cancel row,
// seeded from the persisted TimeoutConfig and written back only on OK via the same
// SetTimeouts handler the old in-Sub-agents form used — so the on-disk config and
// its keys are unchanged, only the UI location moved. Gated on GetTimeouts /
// SetTimeouts; the menu/palette entries are gated the same way so this is never
// reached unwired, but the guard keeps the contract explicit.
func (w *Workbench) showTimeoutsDialog() {
	if w.handlers.GetTimeouts == nil || w.handlers.SetTimeouts == nil {
		w.showConfirm("Timeouts", "Timeout settings are unavailable.", nil)
		return
	}
	timeouts := w.handlers.GetTimeouts()

	// Fixed-form: a hint line, three numeric fields and an OK/Cancel row, pinned to
	// its content footprint like the Sub-agent dialog (issue #317). PreferredW=60
	// comfortably fits the longest label ("Sub-agent timeout:" + box) and the hint;
	// MaxW caps it, MinW floors it on a narrow terminal. Height is pinned at 11:
	// hint at Y=1, the three fields at Y=3/4/5, button row at height-3 (Y=8). Static
	// spec, so dialog.Fit stays correct on resize.
	spec := tv.DialogSpec{MinW: 48, MaxW: 64, PreferredW: 60, MinH: 11, MaxH: 11}
	x, y, width, height := w.dialogRect(spec)

	dialog := tv.NewDialog("Timeouts", x, y, width, height)
	applyWindowShadow(dialog.Window) // honour the NoShadow theme setting (issue #215)
	dialog.Window.ShowClose = false

	hint := dialogLabel("Seconds. 0 = use the built-in default (300s).", tv.Rect{X: 2, Y: 1, W: width - 4, H: 1})

	const labelW = 20
	const boxW = 7
	modelTO := newNumField("Model timeout:", timeouts.ModelSecondsOrDefault(), 2, 3, labelW, boxW)
	toolTO := newNumField("Tool timeout:", timeouts.ToolSecondsOrDefault(), 2, 4, labelW, boxW)
	subTO := newNumField("Sub-agent timeout:", timeouts.SubAgentSecondsOrDefault(), 2, 5, labelW, boxW)

	dialog.Window.AddContent(hint)
	for _, f := range []*numField{modelTO, toolTO, subTO} {
		dialog.Window.AddContent(f.label)
		dialog.Window.AddContent(f.box)
	}

	var layer *tv.Layer
	apply := func() {
		// Start from the persisted config so any future fields not edited here are
		// preserved, and fall back to the current effective value on blank/garbage
		// input (atoiOr) so a stray keystroke can't wipe a timeout.
		t := timeouts
		t.ModelSeconds = atoiOr(modelTO.box.GetText(), timeouts.ModelSecondsOrDefault())
		t.ToolSeconds = atoiOr(toolTO.box.GetText(), timeouts.ToolSecondsOrDefault())
		t.SubAgentSeconds = atoiOr(subTO.box.GetText(), timeouts.SubAgentSecondsOrDefault())
		w.handlers.SetTimeouts(t)
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

	layer = tv.NewModalLayer("timeouts-dialog", dialog)
	w.desktop.AddLayer(layer)
	dialog.Fit(spec) // re-resolve the rect when the terminal is resized (issue #299)
	w.desktop.SetFocus(modelTO.box)
}
