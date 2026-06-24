package ui

import (
	"fmt"
	"strings"

	"gogent/internal/agent"
	"gogent/internal/notify"

	tui "github.com/hobbestherat/turbotui"
	tv "github.com/hobbestherat/turbotui/turbotv"
)

// AskQuestions implements agent.QuestionAsker (issue #406), the bridge behind the
// model-facing `ask_user` tool. It is called from a background (agent-loop)
// goroutine, so it marshals a tabbed question modal onto the UI thread and blocks
// until the user submits or cancels.
//
// It reuses the exact permission/review machinery: a notification fires first (a
// blocking question is precisely the "needs attention" event a user stepping away
// wants pinged for), the requesting session is badged for the life of the prompt,
// and serializePrompt presents one modal at a time and unblocks the agent on
// shutdown — returning Cancelled (the safe default) if the UI is gone. The visual
// presentation is deferred via presentBackgroundModal while the user is
// mid-keystroke, but the badge and notification already fired so the prompt is
// never lost (issue #346). The error return is always nil; a cancellation is
// conveyed in QuestionResponse.Cancelled, which the tool turns into a tool error.
func (w *Workbench) AskQuestions(req agent.QuestionRequest) (agent.QuestionResponse, error) {
	w.notifyQuestions(req)
	w.markApproval(req.SessionID, +1)
	defer w.markApproval(req.SessionID, -1)
	resp := serializePrompt(w, agent.QuestionResponse{Cancelled: true}, func(resolve func(agent.QuestionResponse)) {
		// Defer presentation while the user is mid-keystroke so the dialog cannot
		// hijack their Enter; the badge/notification already fired (issue #346).
		w.desktop.Post(func() {
			w.presentBackgroundModal(func() {
				showQuestionDialog(w.desktop, req, resolve)
			})
		})
	})
	return resp, nil
}

// notifyQuestions fires an "attention needed" notification for a pending question,
// naming the requesting session so an alert for an unfocused background session is
// actionable. Mirrors notifyReview/notifyApproval; a question always notifies
// regardless of focus because it blocks the agent.
func (w *Workbench) notifyQuestions(req agent.QuestionRequest) {
	if w.notify == nil {
		return
	}
	title := "Agent has questions"
	if req.Title != "" {
		title = req.Title
	}
	body := req.Summary
	if body == "" {
		body = "The agent is asking for your input."
	}
	if label := w.requesterLabel(req.SessionID); label != "" {
		body = label + ": " + body
	}
	w.desktop.Post(func() {
		if w.notify.ShouldNotify(notify.ReasonApproval, false) {
			w.notify.Notify(title, body)
		}
	})
}

// Sizing knobs for the question dialog. It is large by default (≈80%×85% of the
// terminal) so a multi-topic form has room, capped so it neither shrinks below a
// usable floor nor balloons on an ultrawide terminal — matching the review dialog.
const (
	questionMinWidth  = 50
	questionMaxWidth  = 110
	questionMinHeight = 14
)

// questionField binds one rendered form item to its answer extractor. answer
// returns the item's current value and whether it counts as answered: a blank
// text/textarea, an unselected choice, or an empty multiselect are all unanswered,
// so they are omitted from the result and fail required-validation.
type questionField struct {
	item     agent.QuestionItem
	tabIndex int
	answer   func() (value interface{}, answered bool)
}

// showQuestionDialog renders a QuestionRequest as a modal tabbed form: one tab per
// topic, each item rendered by the widget its type selects (text → TextBox,
// textarea → MultiLineInput, choice → RadioGroup of Checkbox, multiselect →
// MultiSelect group of Checkbox). Submit collects the answers keyed by item id
// (after required-validation, which blocks submit with an inline error and jumps
// to the offending tab); Escape, Cancel, and a closed UI resolve as Cancelled.
func showQuestionDialog(desktop *tv.Desktop, req agent.QuestionRequest, onResult func(agent.QuestionResponse)) {
	if desktop == nil {
		onResult(agent.QuestionResponse{Cancelled: true})
		return
	}

	spec := tv.DialogSpec{MinW: questionMinWidth, MaxW: questionMaxWidth, MinH: questionMinHeight}
	x, y, width, height := tv.ResolveDialogRect(spec, desktop.App().Width(), desktop.App().Height())

	title := req.Title
	if title == "" {
		title = "Questions"
	}
	dialog := tv.NewDialog(truncate(title, width-4), x, y, width, height)
	applyWindowShadow(dialog.Window) // honour the NoShadow theme setting (issue #215)
	dialog.Window.ShowClose = false

	// Optional one-line context above the tabs.
	tabsY := 1
	if req.Summary != "" {
		summary := dialogLabel(truncate(req.Summary, width-4), tv.Rect{X: 2, Y: 1, W: width - 4, H: 1})
		summary.FG = colorDialogHeader
		dialog.Window.AddContent(summary)
		tabsY = 2
	}

	// Bottom chrome (content-relative): inline error row, then the button row.
	errorY := height - 3
	btnY := height - 2
	tabsH := errorY - tabsY
	if tabsH < 3 {
		tabsH = 3
	}

	tabs := tv.NewTabs(desktop, tv.Rect{X: 1, Y: tabsY, W: width - 2, H: tabsH})
	var fields []questionField
	var firstFocus *tv.VisualComponent
	for ti, topic := range req.Topics {
		panel, tFields, first := buildTopicPanel(topic, ti, width-2, tabsH-1)
		tabs.AddTab(topicTabTitle(topic, ti), panel)
		fields = append(fields, tFields...)
		if firstFocus == nil {
			firstFocus = first
		}
	}
	dialog.Window.AddContent(tabs)

	// Inline required-field error, hidden (empty) until a submit attempt fails.
	errLabel := tv.NewLabel("", tv.Rect{X: 2, Y: errorY, W: width - 4, H: 1})
	errLabel.FG = colorError
	errLabel.BG = tv.DefaultTheme.DialogBG
	dialog.Window.AddContent(errLabel)

	// Clear a stale "X is required" once the user navigates to another tab, so the
	// error does not linger after they move past it. The validation path below sets
	// the error *after* switching tabs, so the auto-switch onto the offending tab
	// never wipes the message it just set.
	tabs.OnTabChange = func(int) { errLabel.SetText("") }

	var layer *tv.Layer
	done := false
	finish := func(resp agent.QuestionResponse) {
		if done {
			return
		}
		done = true
		desktop.RemoveLayer(layer)
		onResult(resp)
	}

	submit := func() {
		// Required-validation: stop at the first unanswered required item, surface an
		// inline error, and switch to its tab so the user lands on the field to fix.
		for _, f := range fields {
			if !f.item.Required {
				continue
			}
			if _, ok := f.answer(); !ok {
				// Switch first (its OnTabChange clears any prior error), then set the
				// message so it survives the auto-switch onto the offending tab.
				tabs.SetActive(f.tabIndex)
				errLabel.SetText(fmt.Sprintf("✗ %s is required", f.item.Label))
				desktop.Redraw()
				return
			}
		}
		answers := make(map[string]interface{}, len(fields))
		for _, f := range fields {
			if v, ok := f.answer(); ok {
				answers[f.item.ID] = v
			}
		}
		finish(agent.QuestionResponse{Answers: answers})
	}
	cancel := func() { finish(agent.QuestionResponse{Cancelled: true}) }

	// Buttons: Cancel (left), optional Prev/Next tab nav (only with >1 topic), and
	// Submit (right-anchored). Left-packed so they never overlap on a narrow dialog.
	bx := 2
	cancelBtn := newButton("&Cancel", tv.Rect{X: bx, Y: btnY, W: tv.ButtonLabelWidth("Cancel"), H: 1}, cancel)
	dialog.Window.AddContent(cancelBtn)
	bx += cancelBtn.Root().Bounds.W + 2
	if n := len(req.Topics); n > 1 {
		// Prev/Next wrap around the ends, matching the Alt+Left/Alt+Right keyboard
		// nav (tv.Tabs switches with wraparound), so neither button is ever a
		// dead-end no-op at the first/last tab.
		prevBtn := newButton("&Prev", tv.Rect{X: bx, Y: btnY, W: tv.ButtonLabelWidth("Prev"), H: 1}, func() {
			tabs.SetActive((tabs.Active() - 1 + n) % n)
			desktop.Redraw()
		})
		dialog.Window.AddContent(prevBtn)
		bx += prevBtn.Root().Bounds.W + 2
		nextBtn := newButton("&Next", tv.Rect{X: bx, Y: btnY, W: tv.ButtonLabelWidth("Next"), H: 1}, func() {
			tabs.SetActive((tabs.Active() + 1) % n)
			desktop.Redraw()
		})
		dialog.Window.AddContent(nextBtn)
	}
	submitW := tv.ButtonLabelWidth("Submit")
	submitBtn := newButton("&Submit", tv.Rect{X: width - 3 - submitW + 1, Y: btnY, W: submitW, H: 1}, submit)
	dialog.Window.AddContent(submitBtn)

	// Escape cancels; Ctrl+Enter submits from anywhere in the form.
	dialog.Root().OnTypeFn = func(_ *tv.VisualComponent, event tui.TypeEvent) bool {
		switch {
		case event.Key == tui.KeyEscape:
			cancel()
			return true
		case event.Key == tui.KeyEnter && event.Ctrl:
			submit()
			return true
		}
		return false
	}

	layer = tv.NewModalLayer("question-dialog", dialog)
	desktop.AddLayer(layer)
	// Land focus on the first field so the user can start typing/selecting at once;
	// fall back to Submit when the form has no focusable items.
	if firstFocus != nil {
		desktop.SetFocus(firstFocus)
	} else {
		desktop.SetFocus(submitBtn)
	}
}

// topicTabTitle is the strip label for a topic — its title, or a generated
// "Topic N" when the model left it blank.
func topicTabTitle(topic agent.QuestionTopic, index int) string {
	if strings.TrimSpace(topic.Title) != "" {
		return topic.Title
	}
	return fmt.Sprintf("Topic %d", index+1)
}

// buildTopicPanel lays one topic's items into a fixed-position panel widget (the
// content of a single tab) and returns it together with the answer bindings and
// the first focusable widget in the panel. Items stack top to bottom: a label
// line, the item's widget(s), then an optional dim help line. tabIndex is the
// owning tab so required-validation can jump back to it. width/height are the tab
// content area; children are placed at fixed offsets relative to the panel origin.
func buildTopicPanel(topic agent.QuestionTopic, tabIndex, width, _ int) (tv.Widget, []questionField, *tv.VisualComponent) {
	panel := tv.NewComponent(tv.Rect{X: 0, Y: 0, W: width, H: 1})
	const margin = 2
	itemW := width - 2*margin
	if itemW < 1 {
		itemW = 1
	}

	var fields []questionField
	var firstFocus *tv.VisualComponent
	noteFocus := func(c *tv.VisualComponent) {
		if firstFocus == nil {
			firstFocus = c
		}
	}

	y := 0
	for _, item := range topic.Items {
		label := dialogLabel(item.Label, tv.Rect{X: margin, Y: y, W: itemW, H: 1})
		panel.AddChild(label)
		y++

		var answer func() (interface{}, bool)
		switch item.Type {
		case agent.QuestionMultiSelect:
			ms := tv.NewMultiSelect()
			for _, opt := range item.Options {
				cb := styleQuestionCheck(tv.NewCheckbox(opt, tv.Rect{X: margin, Y: y, W: itemW, H: 1}, nil))
				panel.AddChild(cb)
				ms.Add(cb)
				noteFocus(cb.Root())
				y++
			}
			answer = func() (interface{}, bool) {
				vals := ms.SelectedValues()
				if len(vals) == 0 {
					return nil, false
				}
				return vals, true
			}

		case agent.QuestionChoice:
			rg := tv.NewRadioGroup()
			for _, opt := range item.Options {
				cb := styleQuestionCheck(tv.NewCheckbox(opt, tv.Rect{X: margin, Y: y, W: itemW, H: 1}, nil))
				panel.AddChild(cb)
				rg.Add(cb)
				noteFocus(cb.Root())
				y++
			}
			answer = func() (interface{}, bool) {
				if rg.Selected() < 0 {
					return nil, false
				}
				return rg.Value(), true
			}

		case agent.QuestionTextarea:
			ml := tv.NewMultiLineInput("", tv.Rect{X: margin, Y: y, W: itemW, H: 3})
			panel.AddChild(ml)
			noteFocus(ml.Root())
			y += 3
			answer = func() (interface{}, bool) {
				if strings.TrimSpace(ml.GetText()) == "" {
					return nil, false
				}
				return ml.GetText(), true
			}

		default: // QuestionText (and any unforeseen type degrades to a text field)
			tb := tv.NewTextBox("", tv.Rect{X: margin, Y: y, W: itemW, H: 1})
			panel.AddChild(tb)
			noteFocus(tb.Root())
			y++
			answer = func() (interface{}, bool) {
				if strings.TrimSpace(tb.GetText()) == "" {
					return nil, false
				}
				return tb.GetText(), true
			}
		}

		// turbotui's TextBox/MultiLineInput have no ghost-placeholder API, so a
		// text/textarea placeholder is surfaced as a dim "e.g. …" hint line beneath
		// the field rather than dropped (issue #406).
		if item.Placeholder != "" && (item.Type == agent.QuestionText || item.Type == agent.QuestionTextarea) {
			hint := dialogLabel("e.g. "+item.Placeholder, tv.Rect{X: margin, Y: y, W: itemW, H: 1})
			hint.FG = colorDialogDetail
			panel.AddChild(hint)
			y++
		}
		if item.Help != "" {
			help := dialogLabel(item.Help, tv.Rect{X: margin, Y: y, W: itemW, H: 1})
			help.FG = colorDialogDetail
			panel.AddChild(help)
			y++
		}
		y++ // blank spacer between items

		fields = append(fields, questionField{item: item, tabIndex: tabIndex, answer: answer})
	}

	panel.Bounds.H = y
	return panel, fields, firstFocus
}

// styleQuestionCheck colours a checkbox for the dialog background, matching the
// settings dialog's local styleCheck.
func styleQuestionCheck(cb *tv.Checkbox) *tv.Checkbox {
	cb.FG = tv.DefaultTheme.DialogFG
	cb.BG = tv.DefaultTheme.DialogBG
	return cb
}
