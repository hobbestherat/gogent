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
// so they are omitted from the result and fail required-validation. focus is the
// item's primary input component, used to land focus on the field when
// required-validation flags it as missing.
type questionField struct {
	item     agent.QuestionItem
	tabIndex int
	focus    *tv.VisualComponent
	answer   func() (value interface{}, answered bool)
}

// showQuestionDialog renders a QuestionRequest as a modal tabbed form: one tab per
// topic, each item rendered by the widget its type selects (text → TextBox,
// textarea → MultiLineInput, choice → RadioGroup of Checkbox, multiselect →
// MultiSelect group of Checkbox). Submit collects the answers keyed by item id
// (after required-validation, which blocks submit with an inline error and jumps
// to the offending tab); Escape, Cancel, and a closed UI resolve as Cancelled.
//
// Each topic's items live inside a scrolling viewport (buildTopicPanel) so a long
// form scrolls instead of clipping its overflow (issue #459). The action buttons sit
// on the last interior row like every other modal — a row lower and they would clip
// onto the bottom border and never render. The dialog reflows on terminal resize.
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

	// Optional one-line context, then an always-visible indicator row.
	var summaryLabel *tv.Label
	row := 1
	if req.Summary != "" {
		summaryLabel = dialogLabel(truncate(req.Summary, width-4), tv.Rect{X: 2, Y: row, W: width - 4, H: 1})
		summaryLabel.FG = colorDialogHeader
		dialog.Window.AddContent(summaryLabel)
		row++
	}

	// The indicator row names the active topic — so a tab strip that drops overflow
	// labels (turbotui's Tabs has no horizontal label scroll) never hides which topic
	// the user is on; Prev/Next and Alt+Left/Right already cycle every topic regardless
	// of the strip — and carries the key hints the dialog needs now that the buttons
	// render: Ctrl+Enter submits from anywhere, and PgUp/PgDn (or the mouse wheel)
	// scroll an overflowing topic. Those are advertised because they are the only scroll
	// keys that also work while a textarea (which keeps Up/Down for the caret) is
	// focused.
	indicatorY := row
	indicator := dialogLabel("", tv.Rect{X: 2, Y: indicatorY, W: width - 4, H: 1})
	indicator.FG = colorDialogDetail
	dialog.Window.AddContent(indicator)
	tabsY := row + 1

	// Bottom chrome (content-relative): turbotui insets the window content by one, so
	// the bottom border is window row height-1 and the last visible interior row is
	// height-3. The buttons sit there (matching review/permission/message/disconnect
	// modals); a row lower (height-2) would land them on the border and clip them away
	// — the primary #459 defect. The inline error row sits just above the buttons.
	errorY := height - 4
	btnY := height - 3
	tabsH := errorY - tabsY
	if tabsH < 3 {
		tabsH = 3
	}

	tabs := tv.NewTabs(desktop, tv.Rect{X: 1, Y: tabsY, W: width - 2, H: tabsH})
	var fields []questionField
	var firstFocus *tv.VisualComponent
	var panels []topicPanel
	for ti, topic := range req.Topics {
		panel := buildTopicPanel(desktop, topic, ti, width-2, tabsH-1)
		tabs.AddTab(elideTabTitle(topicTabTitle(topic, ti)), panel.widget)
		panels = append(panels, panel)
		fields = append(fields, panel.fields...)
		if firstFocus == nil {
			firstFocus = panel.firstFocus
		}
	}
	dialog.Window.AddContent(tabs)

	updateIndicator := func() {
		hints := "Ctrl+Enter submit · PgUp/PgDn or wheel scroll · Tab moves field"
		text := hints
		if n := len(req.Topics); n > 1 {
			active := tabs.Active()
			text = fmt.Sprintf("Topic %d/%d: %s  ·  %s",
				active+1, n, topicTabTitle(req.Topics[active], active), hints)
		}
		indicator.SetText(truncate(text, width-4))
	}
	updateIndicator()

	// Inline required-field error, hidden (empty) until a submit attempt fails.
	errLabel := tv.NewLabel("", tv.Rect{X: 2, Y: errorY, W: width - 4, H: 1})
	errLabel.FG = colorError
	errLabel.BG = tv.DefaultTheme.DialogBG
	dialog.Window.AddContent(errLabel)

	// On a tab switch, clear a stale "X is required" (so it does not linger after the
	// user moves past it) and refresh the topic indicator. The validation path below
	// sets the error *after* switching tabs, so the auto-switch onto the offending tab
	// never wipes the message it just set.
	tabs.OnTabChange = func(int) {
		errLabel.SetText("")
		updateIndicator()
	}

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
				// Switch first (its OnTabChange clears any prior error), then scroll the
				// offending field into view and focus it, and set the message — so it
				// survives the auto-switch and the user is placed on a visible, focused
				// field even when it was below the topic's scroll fold.
				tabs.SetActive(f.tabIndex)
				if f.focus != nil {
					panels[f.tabIndex].ensureVisible(f.focus)
					desktop.SetFocus(f.focus)
				}
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
	// Submit (right-anchored). Prev/Next wrap around the ends, matching Alt+Left/Right
	// (tv.Tabs switches with wraparound), so neither is ever a dead-end at the
	// first/last tab — and they reach every topic even when the strip drops its label.
	cancelBtn := newButton("&Cancel", tv.Rect{X: 2, Y: btnY, W: tv.ButtonLabelWidth("Cancel"), H: 1}, cancel)
	dialog.Window.AddContent(cancelBtn)
	var prevBtn, nextBtn *tv.Button
	if n := len(req.Topics); n > 1 {
		prevBtn = newButton("&Prev", tv.Rect{X: 0, Y: btnY, W: tv.ButtonLabelWidth("Prev"), H: 1}, func() {
			tabs.SetActive((tabs.Active() - 1 + n) % n)
			desktop.Redraw()
		})
		dialog.Window.AddContent(prevBtn)
		nextBtn = newButton("&Next", tv.Rect{X: 0, Y: btnY, W: tv.ButtonLabelWidth("Next"), H: 1}, func() {
			tabs.SetActive((tabs.Active() + 1) % n)
			desktop.Redraw()
		})
		dialog.Window.AddContent(nextBtn)
	}
	submitBtn := newButton("&Submit", tv.Rect{X: 0, Y: btnY, W: tv.ButtonLabelWidth("Submit"), H: 1}, submit)
	dialog.Window.AddContent(submitBtn)

	// placeButtons left-packs Cancel/Prev/Next from the content margin and right-anchors
	// Submit, each clamped to [2, w-3] so a narrow dialog degrades to clipping rather
	// than overlapping or crossing the border. Run at open time and re-run on resize so
	// the row re-flows with the dialog width.
	placeButtons := func(w, by int) {
		leftX, rightX := 2, w-3
		bx := leftX
		place := func(b *tv.Button, lw int) {
			b.Root().SetBounds(clampDialogRect(tv.Rect{X: bx, Y: by, W: lw, H: 1}, leftX, rightX))
			bx = b.Root().Bounds.X + b.Root().Bounds.W + tv.DefaultButtonGap
		}
		place(cancelBtn, tv.ButtonLabelWidth("Cancel"))
		if prevBtn != nil {
			place(prevBtn, tv.ButtonLabelWidth("Prev"))
			place(nextBtn, tv.ButtonLabelWidth("Next"))
		}
		submitW := tv.ButtonLabelWidth("Submit")
		submitBtn.Root().SetBounds(clampDialogRect(tv.Rect{X: w - 3 - submitW + 1, Y: by, W: submitW, H: 1}, leftX, rightX))
	}
	placeButtons(width, btnY)

	// Escape cancels; Ctrl+Enter submits from anywhere in the form; PgUp/PgDn and (when
	// the focused field declines them) Up/Down scroll the active topic when it
	// overflows. The scroll keys reach here by bubbling up from the focused field;
	// they are only consumed when there is something to scroll, otherwise they fall
	// through to the desktop's focus navigation so a short form behaves as before.
	dialog.Root().OnTypeFn = func(_ *tv.VisualComponent, event tui.TypeEvent) bool {
		switch {
		case event.Key == tui.KeyEscape:
			cancel()
			return true
		case event.Key == tui.KeyEnter && event.Ctrl:
			submit()
			return true
		}
		if len(panels) == 0 {
			return false
		}
		p := panels[tabs.Active()]
		if !p.canScroll() {
			return false
		}
		switch event.Key {
		case tui.KeyUp:
			p.scrollBy(-1)
		case tui.KeyDown:
			p.scrollBy(1)
		case tui.KeyPageUp:
			p.pageBy(-1)
		case tui.KeyPageDown:
			p.pageBy(1)
		default:
			return false
		}
		return true
	}

	layer = tv.NewModalLayer("question-dialog", dialog)
	desktop.AddLayer(layer)

	// Reflow on terminal resize (issues #299/#459): re-resolve and re-centre the frame,
	// recompute the bottom chrome and tab height, and resize the Tabs widget — which
	// cascades (Tabs.layoutContent → panel.LayoutFn) so each topic viewport re-derives
	// its visible height, field width and scrollbar and re-clamps its scroll offset on
	// its own. Only focus needs help: a shrink can hide the focused field, so nudge it
	// back into view on the active tab. The spec is pure Min/Max floors, so re-resolving
	// each time matches a fresh open at the new size.
	relayout := func() {
		nx, ny, nw, nh := tv.ResolveDialogRect(spec, desktop.App().Width(), desktop.App().Height())
		dialog.Window.Component.SetBounds(tv.Rect{X: nx, Y: ny, W: nw, H: nh})
		if summaryLabel != nil {
			summaryLabel.Root().SetBounds(tv.Rect{X: 2, Y: 1, W: nw - 4, H: 1})
		}
		indicator.Root().SetBounds(tv.Rect{X: 2, Y: indicatorY, W: nw - 4, H: 1})
		nErrorY, nBtnY := nh-4, nh-3
		nTabsH := nErrorY - tabsY
		if nTabsH < 3 {
			nTabsH = 3
		}
		tabs.Root().SetBounds(tv.Rect{X: 1, Y: tabsY, W: nw - 2, H: nTabsH})
		errLabel.Root().SetBounds(tv.Rect{X: 2, Y: nErrorY, W: nw - 4, H: 1})
		placeButtons(nw, nBtnY)
		if len(panels) > 0 {
			panels[tabs.Active()].keepFocusVisible()
		}
		desktop.Redraw()
	}
	layer.OnResize = func(tv.Rect) { relayout() }

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

// questionTabTitleMax caps a tab-strip label so more topics fit the strip before
// turbotui's Tabs drops overflow labels (it has no horizontal label scroll). The full
// title is still shown in the indicator row and every topic stays reachable via
// Prev/Next and Alt+Left/Right, so this only trades a long on-strip label for more
// visible tabs.
const questionTabTitleMax = 16

// elideTabTitle truncates an over-long tab label with an ellipsis so the strip holds
// more topics. The untruncated title remains available in the indicator row.
func elideTabTitle(title string) string {
	return truncate(title, questionTabTitleMax)
}

// topicPanel is one topic's scrolling viewport plus the handles the dialog drives it
// with. widget is the tv.Component handed to Tabs.AddTab; fields/firstFocus feed the
// dialog's validation and initial focus. scrollBy (±rows) and pageBy (±visible pages)
// are the keyboard-scroll entry points, gated by canScroll (false when the content
// fits, so the dialog leaves the keys to focus navigation). ensureVisible scrolls a
// focusable into the window before it is focused (required-field validation,
// Enter-advance); keepFocusVisible re-homes focus off a field a resize just hid.
type topicPanel struct {
	widget           tv.Widget
	fields           []questionField
	firstFocus       *tv.VisualComponent
	scrollBy         func(rows int)
	pageBy           func(dir int)
	canScroll        func() bool
	ensureVisible    func(c *tv.VisualComponent)
	keepFocusVisible func()
}

// buildTopicPanel lays one topic's items into a scrolling viewport (the content of a
// single tab) and returns it as a topicPanel. Items stack top to bottom at fixed
// *logical* rows — a label line, the item's widget(s), an optional placeholder hint
// and help line, then a spacer. The panel hosts a hand-rolled scroll viewport (the
// pattern proven in theme_editor.go for interactive children, since turbotui's Tabs
// fills its content with no scroll and a read-only TextView cannot host input
// widgets): a scrollY offset shifts every child to viewport-relative Y = logical -
// scrollY and toggles its Visible flag, so off-window fields are neither drawn,
// hit-tested nor focus-navigated, and a 1-column scrollbar marks overflow.
//
// tabIndex is the owning tab so required-validation can jump back to it. width is the
// tab content width and visibleRows its height; both are re-derived live from the
// panel's own bounds in LayoutFn, so the viewport self-heals when Tabs resizes it on a
// terminal resize. desktop advances/keeps focus.
func buildTopicPanel(desktop *tv.Desktop, topic agent.QuestionTopic, tabIndex, width, visibleRows int) topicPanel {
	const margin = 2
	panel := tv.NewComponent(tv.Rect{X: 0, Y: 0, W: width, H: visibleRows})

	itemW := width - 2*margin
	if itemW < 1 {
		itemW = 1
	}
	barX := width - 1

	// scrollRow couples a child to its logical row (its build-time Y) and row span, so
	// reflow can reposition it as the viewport scrolls and re-width it on resize.
	type scrollRow struct {
		comp    *tv.VisualComponent
		logical int
		h       int
	}
	var rows []scrollRow
	addRow := func(c *tv.VisualComponent, logical, h int) {
		panel.AddChild(c)
		rows = append(rows, scrollRow{comp: c, logical: logical, h: h})
	}

	var fields []questionField
	// focusables is the panel's focusable widgets in tab order; it backs the
	// Enter-advances-focus wiring and keepFocusVisible. focusRows maps each focusable to
	// its logical top row and span so ensureVisible can scroll the minimum amount to
	// bring a field (e.g. a required one flagged at submit) into the window.
	var focusables []*tv.VisualComponent
	focusRows := map[*tv.VisualComponent]struct{ y, h int }{}
	addFocusable := func(c *tv.VisualComponent, logical, h int) {
		focusables = append(focusables, c)
		focusRows[c] = struct{ y, h int }{logical, h}
	}
	var textBoxes []struct {
		box *tv.TextBox
		idx int
	}

	y := 0
	for _, item := range topic.Items {
		label := dialogLabel(item.Label, tv.Rect{X: margin, Y: y, W: itemW, H: 1})
		addRow(label.Root(), y, 1)
		y++

		var answer func() (interface{}, bool)
		var itemFocus *tv.VisualComponent
		switch item.Type {
		case agent.QuestionMultiSelect:
			ms := tv.NewMultiSelect()
			for _, opt := range item.Options {
				cb := styleQuestionCheck(tv.NewCheckbox(opt, tv.Rect{X: margin, Y: y, W: itemW, H: 1}, nil))
				addRow(cb.Root(), y, 1)
				ms.Add(cb)
				addFocusable(cb.Root(), y, 1)
				if itemFocus == nil {
					itemFocus = cb.Root()
				}
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
				addRow(cb.Root(), y, 1)
				rg.Add(cb)
				addFocusable(cb.Root(), y, 1)
				if itemFocus == nil {
					itemFocus = cb.Root()
				}
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
			addRow(ml.Root(), y, 3)
			addFocusable(ml.Root(), y, 3)
			itemFocus = ml.Root()
			y += 3
			answer = func() (interface{}, bool) {
				if strings.TrimSpace(ml.GetText()) == "" {
					return nil, false
				}
				return ml.GetText(), true
			}

		default: // QuestionText (and any unforeseen type degrades to a text field)
			tb := tv.NewTextBox("", tv.Rect{X: margin, Y: y, W: itemW, H: 1})
			addRow(tb.Root(), y, 1)
			textBoxes = append(textBoxes, struct {
				box *tv.TextBox
				idx int
			}{box: tb, idx: len(focusables)})
			addFocusable(tb.Root(), y, 1)
			itemFocus = tb.Root()
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
		// the field rather than dropped (issue #406). The extra row no longer costs
		// usability now that the panel scrolls.
		if item.Placeholder != "" && (item.Type == agent.QuestionText || item.Type == agent.QuestionTextarea) {
			hint := dialogLabel("e.g. "+item.Placeholder, tv.Rect{X: margin, Y: y, W: itemW, H: 1})
			hint.FG = colorDialogDetail
			addRow(hint.Root(), y, 1)
			y++
		}
		if item.Help != "" {
			help := dialogLabel(item.Help, tv.Rect{X: margin, Y: y, W: itemW, H: 1})
			help.FG = colorDialogDetail
			addRow(help.Root(), y, 1)
			y++
		}
		y++ // blank spacer between items

		fields = append(fields, questionField{item: item, tabIndex: tabIndex, focus: itemFocus, answer: answer})
	}
	contentH := y

	// --- scroll machinery (mirrors theme_editor.go's viewport) ---

	maxScroll := func() int {
		if m := contentH - visibleRows; m > 0 {
			return m
		}
		return 0
	}
	clampScroll := func(v int) int {
		if v < 0 {
			return 0
		}
		if m := maxScroll(); v > m {
			return m
		}
		return v
	}
	scrollY := 0
	// reflow repositions every row to viewport-relative Y = logical - scrollY, sets its
	// width from the live itemW (so a horizontal resize re-widens fields), and shows
	// only rows that intersect the window — a partially-scrolled 3-row textarea stays
	// visible/focusable (turbotui clips its off-window rows).
	reflow := func() {
		for _, r := range rows {
			r.comp.SetBounds(tv.Rect{X: margin, Y: r.logical - scrollY, W: itemW, H: r.h})
			r.comp.Visible = r.logical+r.h > scrollY && r.logical < scrollY+visibleRows
		}
	}
	// keepFocusVisible re-homes focus off a field the latest scroll/resize hid: a hidden
	// focused widget stops receiving keys (the desktop's visibleInTree guard), which
	// would strand keyboard scrolling until a click. Moving focus to a still-visible
	// field keeps the scroll keys bubbling to the dialog.
	keepFocusVisible := func() {
		hidden := false
		for _, c := range focusables {
			if c.Focused() && !c.Visible {
				hidden = true
				break
			}
		}
		if !hidden {
			return
		}
		for _, c := range focusables {
			if c.Visible {
				desktop.SetFocus(c)
				return
			}
		}
	}
	scrollTo := func(v int) {
		n := clampScroll(v)
		if n == scrollY {
			return
		}
		scrollY = n
		reflow()
		keepFocusVisible()
		desktop.Redraw()
	}
	// ensureVisible scrolls the minimum amount to bring a focusable's [y, y+h) span into
	// the window, used before focusing a field that may sit below the fold.
	ensureVisible := func(c *tv.VisualComponent) {
		fr, ok := focusRows[c]
		if !ok {
			return
		}
		switch {
		case fr.y < scrollY:
			scrollTo(fr.y)
		case fr.y+fr.h > scrollY+visibleRows:
			scrollTo(fr.y + fr.h - visibleRows)
		}
	}

	// The 1-column scrollbar lives in the panel's last column (items end one column
	// short of it). It draws nothing when the content fits, and reads the live scrollY
	// each frame so the thumb tracks the offset.
	bar := tv.NewComponent(tv.Rect{X: barX, Y: 0, W: 1, H: visibleRows})
	bar.DrawFn = func(c *tv.VisualComponent, surface tv.Surface) {
		if maxScroll() == 0 {
			return
		}
		drawDialogVScrollbar(surface, c.AbsoluteBounds(), contentH, visibleRows, scrollY,
			tv.DefaultTheme.DialogFG, tv.DefaultTheme.DialogBG)
	}
	panel.AddChild(bar)

	// LayoutFn is the single place live geometry is re-derived. Tabs.layoutContent sizes
	// each tab's content via SetBounds, which fires this; SetBounds also runs it on
	// resize and Draw runs it each frame, so the viewport self-heals on both resize and
	// redraw. The lastW/lastH guard makes a steady-state redraw a no-op (scroll changes
	// go through scrollTo, which reflows directly). A zero/negative bound keeps the
	// build-time seeds so nothing is hidden before the first real layout.
	lastW, lastH := -1, -1
	panel.LayoutFn = func(c *tv.VisualComponent) {
		b := c.Bounds
		if b.W == lastW && b.H == lastH {
			return
		}
		lastW, lastH = b.W, b.H
		if b.H > 0 {
			visibleRows = b.H
		}
		if b.W > 0 {
			itemW = b.W - 2*margin
			if itemW < 1 {
				itemW = 1
			}
			barX = b.W - 1
		}
		bar.SetBounds(tv.Rect{X: barX, Y: 0, W: 1, H: visibleRows})
		scrollY = clampScroll(scrollY)
		reflow()
	}

	// Mouse wheel scrolls the topic (Delta +1 up / -1 down, subtracted to scroll the
	// natural way) — it works regardless of which field holds focus.
	panel.OnScrollFn = func(_ *tv.VisualComponent, event tui.ScrollEvent) bool {
		if maxScroll() == 0 {
			return false
		}
		scrollTo(scrollY - event.Delta)
		return true
	}

	// Enter in a single-line field advances focus to the next focusable (wrapping),
	// scrolling it into view first so the new focus is never below the fold. A textarea
	// keeps Enter for newlines (its OnSubmit is left nil). Wired after the loop so the
	// captured index resolves against the panel's complete focus order.
	for _, tb := range textBoxes {
		tb := tb
		tb.box.OnSubmit = func() {
			if desktop == nil || len(focusables) == 0 {
				return
			}
			next := focusables[(tb.idx+1)%len(focusables)]
			ensureVisible(next)
			desktop.SetFocus(next)
			desktop.Redraw()
		}
	}

	var firstFocus *tv.VisualComponent
	if len(focusables) > 0 {
		firstFocus = focusables[0]
	}

	return topicPanel{
		widget:           panel,
		fields:           fields,
		firstFocus:       firstFocus,
		scrollBy:         func(n int) { scrollTo(scrollY + n) },
		pageBy:           func(dir int) { scrollTo(scrollY + dir*visibleRows) },
		canScroll:        func() bool { return maxScroll() > 0 },
		ensureVisible:    ensureVisible,
		keepFocusVisible: keepFocusVisible,
	}
}

// styleQuestionCheck colours a checkbox for the dialog background, matching the
// settings dialog's local styleCheck.
func styleQuestionCheck(cb *tv.Checkbox) *tv.Checkbox {
	cb.FG = tv.DefaultTheme.DialogFG
	cb.BG = tv.DefaultTheme.DialogBG
	return cb
}
