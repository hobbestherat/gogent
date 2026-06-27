package ui

import (
	"encoding/json"
	"fmt"
	tui "github.com/hobbestherat/turbotui"
	tv "github.com/hobbestherat/turbotui/turbotv"
	"gogent/internal/agent"
	"gogent/internal/config"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// SessionWindow is a single chat session rendered in its own window/layer.
type SessionWindow struct {
	wb         *Workbench
	id         string
	title      string
	window     *tv.Window
	layer      *tv.Layer
	history    *tv.TextView
	transcript *transcriptModel
	input      *tv.MultiLineInput
	sendButton *tv.Button
	// interjectButton, queueButton and stopButton are the running-turn input
	// controls (issue #201): while a turn is in flight they replace the single
	// idle Send button next to the prompt box. Queue mirrors the Enter default
	// (drain-on-idle), Interject splices the current input into the running turn
	// now (disabled when the input is empty), and Stop cancels the turn and clears
	// the queue. They carry zero bounds while idle, so only Send shows; the swap is
	// driven by layoutInputRow from the busy flag.
	interjectButton *tv.Button
	queueButton     *tv.Button
	stopButton      *tv.Button
	modelLabel      *tv.Label
	modelSelect     *tv.Select
	// effortLabel / effortSelect are the per-session reasoning-effort control
	// (issue #177), right-aligned on the model header row. The selector's options
	// are ["(default)"] + the selected model's EffortOptions; "(default)" means
	// "no override — use the model config's reasoning_effort". effortEnabled is
	// false for a model with no effort options (greyed out, click/keys ignored).
	// effortHidden is true on windows too narrow to show the control without
	// overlapping the model selector.
	effortLabel   *tv.Label
	effortSelect  *tv.Select
	effortEnabled bool
	effortHidden  bool
	// effortLabelEnabledFG remembers the themed label colour so the greyed-out
	// state (a model with no effort options) can be restored to it.
	effortLabelEnabledFG tui.Color
	status               *tv.Label
	// separator is the horizontal divider rule drawn on its own row directly above
	// the status line (issue #195). It supplies the top edge of the controls region
	// (status line + input row); together with the window frame's left/right/bottom
	// borders it fences that region off from the transcript above, which previously
	// ran flush into the status line. It is created only on live windows (nil on the
	// read-only analysis window, which has no status/input chrome) and re-sized to
	// the window width on every layout so it tracks resizes.
	separator *tv.Label
	// readOnly marks a static analysis window opened from the Sessions browser
	// (issue #58): it renders a saved transcript with the full search/filter/
	// fold/yank toolkit but has no input, model selector or live backend session,
	// so several can sit open side-by-side for comparison without cost. Its id is
	// an "analysis-N" synthetic (never a backend session id), it is excluded from
	// the persisted layout, and closing it tears down no backend session.
	readOnly bool
	// pendingTools tracks the transcript record of each in-flight tool call,
	// keyed by the call's stable event id (SessionEvent.CallID), so its result
	// can be appended to the same foldable entry when it returns. It is a map
	// rather than a single slot because a concurrent tool batch has several calls
	// running at once whose results may arrive in any order — keying by id is what
	// lets every result flip the right entry from "running" to done, so none is
	// left stuck "running" (issue #187). The invariant the safety net relies on:
	// every record still showing "(running...)" is reachable here until it reaches
	// a terminal state, so failPendingTools can always sweep it. An id-less call is
	// tracked under a synthetic key (see untrackedTools) to preserve that.
	pendingTools map[string]*transcriptRecord
	// untrackedTools counts id-less tool calls, used to mint a collision-free
	// synthetic key so an id-less "(running...)" entry is still swept on busy→idle
	// even though it can never be paired to a result by id (issue #187).
	untrackedTools int
	// liveThought is the in-flight streamed "thinking" record for the current turn,
	// or nil when no reasoning is streaming (issue #217). Streamed reasoning deltas
	// (SessionEventThinkingDelta) append to it expanded so the user watches it live;
	// it is relabelled "thought", collapsed and cleared when the turn's thinking
	// completes (SessionEventThinkingDone) or on the busy→idle safety net.
	// liveThoughtBuf line-buffers partial deltas so only complete lines are
	// committed to the entry (the trailing partial is flushed when the entry folds).
	// Touched only on the UI thread.
	liveThought    *transcriptRecord
	liveThoughtBuf string
	busy           bool
	// background is true while the session has async sub-agents running in the
	// background (issue #353), driven by the backend's SessionEventBackground. It is
	// the third busy state: when the main loop's turn ends (busy→false) while workers
	// are still running, the session shows "working in background..." instead of idle,
	// and the busy→idle edge logic (drainQueue/maybeSupervise/failPendingTools/
	// foldLiveThought) is deferred until BOTH the loop is done AND no workers remain.
	background bool
	// statusState is the current left-hand status text (idle/working.../thinking
	// ... (step N)); statusStats holds the latest per-session stats snapshot.
	// refreshStatus composes the two into the single bottom status line.
	statusState string
	statusStats agent.SessionStats
	// turnStart / turnStartOut anchor the live elapsed timer and output
	// throughput shown while a turn is generating. turnStart is the zero time
	// when the session is idle. budgetAlerted latches the one-time "budget
	// exceeded" transcript note so it fires once per breach (cumulative token
	// usage is monotonic, so a breach never self-clears).
	turnStart     time.Time
	turnStartOut  int
	budgetAlerted bool
	// maximized tracks whether the window is expanded to the available desktop
	// area via the title-bar maximize button (issue #105); preMaximizeBounds
	// remembers the bounds to return to on restore. maximizable mirrors
	// config.WindowConfig.Maximizable and gates the button entirely.
	maximized         bool
	preMaximizeBounds tv.Rect
	maximizable       bool
	// completer drives the @-file mention popup over the input (issue #46): typing
	// "@" offers matching workspace files for precise context attachment, expanded
	// into the sent message by expandMentions. Nil on read-only analysis windows,
	// which have no input.
	completer *mentionCompleter
	// planMode mirrors the backend plan-mode flag for the status indicator, and
	// planPending marks a plan awaiting approval (set on SessionEventPlan),
	// enabling the /act (approve) command (issue #43).
	planMode    bool
	planPending bool
	// yoloMode mirrors the backend yolo flag for the status indicator (issue
	// #356): when on, permission prompts are auto-approved (except rules.json
	// hard-deny guardrails) and the step cap is removed.
	yoloMode bool
	// pending holds a message typed while the agent was busy (issue #170). Rather
	// than dropping input mid-turn, the submit handler stows the latest text here
	// (a single editable, latest-wins slot — simplest UX, matching the backend's
	// pendingNote) and surfaces it as a "queued:" hint in the status line. When the
	// agent returns to idle it auto-fires as the next turn (drain-on-idle, phase
	// 1); when the user stops the agent it is discarded with a note rather than
	// auto-firing. With the experimental inject flag on (phase 2) the text is sent
	// to the backend for mid-turn injection instead and the slot is cleared so it
	// does not double-fire on idle. draining guards the auto-submit re-entry so a
	// queued message cannot itself be re-queued. Touched only on the UI thread.
	pending  string
	draining bool
	// pendingCmd carries the per-invocation overrides (model/agent/subtask) of a
	// custom command queued while busy (issue #403), paired with pending. Without
	// it the drain would re-send the expanded text through the normal submit path
	// and silently lose the command's model override. nil for an ordinary queued
	// message; a normal enqueue clears it so a later plain message never inherits a
	// stale override. Touched only on the UI thread.
	pendingCmd *pendingCommand
	// submitFn re-enters the input submit path (issue #170): the drain-on-idle
	// logic uses it to auto-fire a queued message as the next user turn, reusing
	// the exact same send path (mention expansion, busy/transcript handling) as a
	// hand-typed message rather than duplicating it.
	submitFn func()
	// promptHistory is the per-session, in-memory list of prompts the user actually
	// submitted this session, oldest→newest, driving the Up/Down recall in the input
	// (issue #203). Only user-typed submissions are captured (slash commands included,
	// supervisor nudges excluded); a consecutive duplicate is skipped. historyNav is
	// the recall cursor: historyNav == len(promptHistory) means "not navigating", at
	// the in-progress draft; len-1 is the newest entry and 0 the oldest. historyDraft
	// stashes whatever the user had typed when navigation began so Down past the
	// newest restores it (shell-style). All three are touched only on the UI thread.
	promptHistory []string
	historyNav    int
	historyDraft  string
	// goal is the session's supervisor objective set via /goal (issue #172) — the
	// definition of "done" the idle watchdog re-checks on each busy→idle edge.
	// Empty means no goal. nudgeCount is how many consecutive supervisor nudges
	// have fired since the last real (non-supervisor) user message; it is reset to
	// 0 when the user intervenes and bounded by the configured max-nudges budget.
	// supervisorBusy guards a single in-flight async completion check so an idle
	// edge cannot launch overlapping checks. nudgingSend marks the in-flight
	// submit as a supervisor-originated nudge so it does not reset the budget.
	// All four are touched only on the UI thread (the async check posts its result
	// back via the workbench's UI queue), so they need no extra locking — matching
	// the pending/draining fields above.
	goal           string
	nudgeCount     int
	supervisorBusy bool
	nudgingSend    bool
	// nudgeGaveUp latches the one-time "goal still unmet after N nudges" note so
	// the supervisor surfaces its give-up exactly once per budget, not on every
	// subsequent idle edge. Reset together with nudgeCount when the user intervenes.
	nudgeGaveUp bool
	// runSupervisorCheck dispatches the completion check and applies its verdict.
	// Production wiring (set in newSessionWindow) runs the check on a background
	// goroutine and posts applySupervisorVerdict back onto the UI thread so a model
	// judge never blocks the loop. Tests override it to run synchronously, since
	// the event-loop post queue is not pumped under test. Nil disables the
	// watchdog. Touched only on the UI thread.
	runSupervisorCheck func(goal string)
}

// newSessionWindow builds the window, its widgets and their layout/handlers. A
// readOnly window (opened from the Sessions browser, issue #58) omits the input,
// model selector and status line and gives the transcript the full height; a
// live window wires the full send/model/status chrome.
func newSessionWindow(wb *Workbench, id, title string, bounds tv.Rect, readOnly bool) *SessionWindow {
	sw := &SessionWindow{wb: wb, id: id, title: title, readOnly: readOnly, pendingTools: map[string]*transcriptRecord{}}
	displayTitle := title
	if readOnly {
		displayTitle = title + " (analysis)"
	}
	window := tv.NewWindow(displayTitle, bounds, tui.LineSingle)
	applyWindowShadow(window) // honour the NoShadow theme setting (issue #215)

	// Enable scalable windows using turbotv options
	window.Resizable = wb.windowConfig.Resizable
	window.Minimizable = wb.windowConfig.Minimizable
	window.MinWidth = wb.windowConfig.MinWidth
	window.MinHeight = wb.windowConfig.MinHeight

	window.OnClose = func(_ *tv.Window) { wb.CloseSession(id) }
	history := tv.NewTextView("", tv.Rect{})
	history.Wrap = true
	sw.window = window
	sw.history = history
	// Overlay the title-bar maximize/restore button when the config opts in. It
	// applies to both live and read-only windows, so it is wired before the
	// readOnly branch below.
	if wb.windowConfig.Maximizable {
		sw.maximizable = true
		sw.installMaximizeButton()
	}
	// Constrain drag/resize to the pinned sidebar area (issue #106). Installed
	// for every window (live and read-only) and as the outermost click wrapper so
	// it runs after the maximize button and the base drag/resize handler.
	sw.installSidebarClamp()
	sw.transcript = newTranscriptModel(history)
	sw.transcript.add(&transcriptRecord{
		kind:   kindSystem,
		header: "[System] " + title + " ready. Type a message and press Enter (Shift+Enter for newline).",
		color:  colorInfo,
		role:   roleInfo,
	})
	// Register the transcript-context keys (search/filter/fold/yank) as Focus-scope
	// bindings on the desktop's BindingRegistry, scoped to this window's transcript
	// (issue #269, phase 4a). The toolkit consults them at the focused-widget stage —
	// AFTER the TextView's own scroll handler declines the key — so the TextView keeps
	// its scroll keys and these only fire while this transcript holds focus. Registered
	// before the readOnly return so analysis windows (full transcripts) get them too.
	sw.registerTranscriptBindings()

	if readOnly {
		// Analysis window: transcript only, no input/model/status chrome.
		window.AddContent(history)
		window.Content.LayoutFn = func(c *tv.VisualComponent) {
			wd := c.Bounds.W
			ht := c.Bounds.H
			if wd < 4 || ht < 4 {
				return
			}
			history.Component.SetBounds(tv.Rect{X: 0, Y: 0, W: wd, H: ht})
		}
		sw.layer = tv.NewWindowLayer("layer-"+id, window)
		return sw
	}

	input := tv.NewMultiLineInput("", tv.Rect{})
	// Word-wrap the prompt box so whole words stay on one line instead of being cut
	// mid-word at the right edge (issue #270). The caret/history-edge helpers read
	// the widget's real wrap layout via CaretRowInLine, so recall stays correct.
	input.WordWrap = true
	sendButton := newButton("Send", tv.Rect{}, nil)
	// Running-turn controls (issue #201). Labels carry the Enter affordance (Queue)
	// and an error-coloured halt (Stop); their handlers are wired below, once the
	// shared submit closure exists. They start hidden (zero bounds) until busy.
	interjectButton := newButton(interjectLabel, tv.Rect{}, nil)
	queueButton := newButton(queueLabel, tv.Rect{}, nil)
	stopButton := newButton(stopLabel, tv.Rect{}, nil)
	// Error-coloured halt, kept red even when keyboard-focused (issue #201).
	stopButton.FG = colorError
	stopButton.FocusFG = colorError
	modelLabel := tv.NewLabel("Model", tv.Rect{})
	modelSelect := newSelect(wb.desktop, wb.modelNames, tv.Rect{})
	modelLabel.SetTarget(modelSelect)
	effortLabel := tv.NewLabel("Effort", tv.Rect{})
	effortSelect := newSelect(wb.desktop, []string{effortDefaultOption}, tv.Rect{})
	effortLabel.SetTarget(effortSelect)
	status := tv.NewLabel("idle", tv.Rect{})
	status.FG = colorNote
	// The divider rule above the controls region (issue #195). Its text is built per
	// layout from the window width (layoutControlsSeparator); it carries the chrome
	// divider colour so it matches the separators used elsewhere (e.g. the sidebar).
	separator := tv.NewLabel("", tv.Rect{})
	separator.FG = chromeDivider
	sw.input = input
	sw.sendButton = sendButton
	sw.interjectButton = interjectButton
	sw.queueButton = queueButton
	sw.stopButton = stopButton
	sw.modelLabel = modelLabel
	sw.modelSelect = modelSelect
	sw.effortLabel = effortLabel
	sw.effortSelect = effortSelect
	sw.effortLabelEnabledFG = effortLabel.FG
	// The effort selector only opens while enabled (a model with effort options):
	// wrap its click/key handlers so a greyed-out control is inert (issue #177).
	sw.guardEffortSelect()
	// A model change in the focused session moves the Overall panel's "model"/"api"
	// rows (issue #107) and rebuilds the per-session effort options + enabled state
	// for the newly selected model (issue #177); coalesce the Overall refresh rather
	// than paying for one per pick.
	modelSelect.OnChange = func(int) {
		sw.rebuildEffortOptions()
		wb.scheduleOverallRefresh()
	}
	// Seed the effort options from the initially selected model.
	sw.rebuildEffortOptions()
	sw.status = status
	sw.separator = separator
	sw.statusState = "idle"
	window.AddContent(history)
	window.AddContent(separator)
	window.AddContent(input)
	window.AddContent(sendButton)
	window.AddContent(interjectButton)
	window.AddContent(queueButton)
	window.AddContent(stopButton)
	// Grey the Interject button while the input is empty so its disabled state
	// (nothing to slip in) is visible, mirroring the effort control (issue #201).
	sw.guardInterjectButton()
	window.AddContent(modelLabel)
	window.AddContent(modelSelect)
	window.AddContent(effortLabel)
	window.AddContent(effortSelect)
	window.AddContent(status)
	window.Content.LayoutFn = func(c *tv.VisualComponent) {
		wd := c.Bounds.W
		ht := c.Bounds.H
		if wd < 4 || ht < 7 {
			return
		}
		// The prompt box is three rows tall. While a turn runs that height also hosts
		// the running-turn controls, which stack one-per-row in a column beside the
		// prompt (Queue / Interject / Stop, issue #234), so the three buttons line up
		// against the three prompt rows — keep inputH >= 3 if this ever changes.
		inputH := 3
		selW := headerSelectWidth(sw.wb.longestModelNameWidth(), wd)
		modelLabel.Component.SetBounds(tv.Rect{X: 0, Y: 0, W: 6, H: 1})
		modelSelect.Component.SetBounds(tv.Rect{X: 7, Y: 0, W: selW, H: 1})
		sw.layoutEffortControl(wd, 7+selW)
		// The history loses one row to the divider rule that fences off the controls
		// region below it (issue #195): the rule sits on the row between the
		// transcript's last line and the status line, so the status no longer reads as
		// a continuation of the transcript.
		history.Component.SetBounds(tv.Rect{X: 0, Y: 1, W: wd, H: ht - inputH - 3})
		sw.layoutControlsSeparator(wd, ht-inputH-2)
		status.Component.SetBounds(tv.Rect{X: 0, Y: ht - inputH - 1, W: wd, H: 1})
		// The input row shows Send while idle and the three running-turn controls
		// while busy (issue #201); layoutInputRow sizes the input box to the room
		// left beside whichever set is shown.
		sw.layoutInputRow(wd, ht, inputH)
		// Reflow the status line to the new width so its stats truncate/expand
		// with the window on resize.
		sw.refreshStatus()
	}
	// The @-file mention completer (issue #46) hangs off the input: it intercepts
	// the navigation/accept keys while its popup is open and otherwise lets the
	// input handle the key, then refreshes the popup from the new cursor position.
	sw.completer = newMentionCompleter(sw)
	baseType := input.Component.OnTypeFn
	input.Component.OnTypeFn = func(c *tv.VisualComponent, event tui.TypeEvent) bool {
		if sw.completer.handleKey(event) {
			return true
		}
		// Up/Down recall older/newer submitted prompts (issue #203), but only at the
		// edges of the buffer so interior multi-line editing still moves the caret.
		// The completer above keeps priority while its popup is open.
		if sw.handleHistoryKey(event) {
			// A recall replaces the whole buffer, so there is no in-progress mention to
			// complete; dismiss any popup rather than letting update() re-open one from
			// an @-token that happens to sit at the end of the recalled text.
			sw.completer.hide()
			return true
		}
		before := input.GetText()
		handled := false
		if baseType != nil {
			handled = baseType(c, event)
		}
		sw.completer.update()
		// Clearing the draft to empty is the "or clears their input — whichever comes
		// first" trigger for a deferred background prompt (issue #346): once the input
		// the user was typing in goes empty, present any held-back permission/review
		// modal immediately rather than wait out the typing-idle window. Guarded on the
		// non-empty→empty edge so an already-empty input (navigation, a no-op Backspace)
		// does not repeatedly poke the drain.
		if before != "" && input.GetText() == "" {
			wb.drainDeferredModalNow()
		}
		return handled
	}
	submit := func() {
		text := strings.TrimSpace(input.GetText())
		if text == "" {
			return
		}
		// Submitting is the "or submits their input — whichever comes first" trigger
		// for a deferred background prompt (issue #346): the Enter went to the input,
		// not the dialog, so any held-back permission/review modal can appear now
		// rather than wait out the typing-idle window.
		wb.drainDeferredModalNow()
		// Record the prompt for Up/Down history recall (issue #203). Only user-typed
		// submissions enter history: supervisor nudges (nudgingSend) are skipped, and
		// the drain re-entry (draining) is skipped so a message queued while busy is
		// recorded once — when the user pressed Enter — not again when it drains. The
		// raw typed text is stored (before mention expansion); slash commands count as
		// prompts and are included.
		if !sw.nudgingSend && !sw.draining {
			sw.recordHistory(text)
		}
		// Busy: don't drop the input (issue #170). Queue it as the next turn instead
		// — the drain-on-idle path re-submits it when the agent finishes, and (with
		// the experimental flag on) it is injected mid-turn. A leading-slash command
		// is still handled locally even while busy so /stop and the queue controls
		// stay responsive during a turn.
		if sw.busy {
			if sw.handleSlashCommand(text) {
				input.Clear()
				return
			}
			input.Clear()
			sw.enqueue(text)
			return
		}
		// Dismiss the mention popup if a click on Send submitted while it was open.
		sw.completer.hide()
		// A leading slash is a client-side command (/undo, /rewind) handled
		// locally rather than sent to the model (issue #41).
		if sw.handleSlashCommand(text) {
			input.Clear()
			return
		}
		input.Clear()
		// A real (non-supervisor) user message is the user intervening, which resets
		// the supervisor's nudge budget (issue #172): the next idle edge gets a fresh
		// allowance of nudges. A supervisor nudge re-enters this path with nudgingSend
		// set, so it does not reset its own budget. A drained queued message is a real
		// user message and so does reset it.
		if !sw.nudgingSend {
			sw.nudgeCount = 0
			sw.nudgeGaveUp = false
		}
		sw.addUser(text)
		sw.setBusy(true)
		sw.planPending = false // sending supersedes any plan awaiting approval
		modelName := sw.selectedModelName()
		effort := sw.selectedEffort()
		// Expand any @-file mentions into attached file content so the model
		// receives the referenced files directly (issue #46). The transcript keeps
		// the message as typed; a note records what was attached.
		message := text
		if expanded, attached := expandMentions(text, wb.handlers.ReadWorkspaceFile); len(attached) > 0 {
			message = expanded
			sw.addNote("attached " + strings.Join(attached, ", "))
		}
		if wb.handlers.OnSend != nil {
			go wb.handlers.OnSend(sw.id, message, modelName, effort)
		}
	}
	sendButton.OnPress = submit
	input.OnSubmit = submit
	sw.submitFn = submit
	// Running-turn controls (issue #201): Queue mirrors Enter (drain-on-idle),
	// Interject splices the current input into the live turn now, Stop cancels it.
	queueButton.OnPress = submit
	interjectButton.OnPress = sw.interject
	stopButton.OnPress = sw.stopTurn
	// Wire the supervisor's completion-check dispatcher (issue #172). The watchdog
	// (maybeSupervise) calls it on the busy→idle edge; it runs the check off the UI
	// thread and posts the verdict back. Tests override it to run synchronously.
	sw.runSupervisorCheck = sw.defaultSupervisorCheck
	sw.layer = tv.NewWindowLayer("layer-"+id, window)
	return sw
}

// IsMaximized reports whether the window is currently expanded to the available
// desktop area via the title-bar maximize button.
func (sw *SessionWindow) IsMaximized() bool { return sw.maximized }

// Maximize expands the window to the available desktop area (the whole desktop
// below the menu bar, minus the reserved "Sessions & Agents" sidebar), remembering
// its current bounds so an unmaximize can return to them. It is a no-op when the
// window is already maximized or when maximize is disabled.
func (sw *SessionWindow) Maximize() {
	if sw.maximized || !sw.maximizable {
		return
	}
	sw.preMaximizeBounds = sw.window.Component.Bounds
	sw.maximized = true
	sw.applyMaximizedBounds()
}

// unmaximize restores the window to the bounds it had before it was maximized.
// It is a no-op when the window is not maximized.
func (sw *SessionWindow) unmaximize() {
	if !sw.maximized {
		return
	}
	sw.maximized = false
	sw.window.Component.SetBounds(sw.preMaximizeBounds)
}

// ToggleMaximize flips the window between its pre-maximize bounds and the
// available desktop area.
func (sw *SessionWindow) ToggleMaximize() {
	if sw.maximized {
		sw.unmaximize()
		return
	}
	sw.Maximize()
}

// applyMaximizedBounds sizes the window to the current maximized area, recomputed
// from the live desktop dimensions so a maximize after a terminal resize fills the
// new area rather than a stale one. It fills the pinned window area (left of the
// sidebar), or the full desktop when the sidebar is unpinned (issue #106).
func (sw *SessionWindow) applyMaximizedBounds() {
	area := sw.wb.windowArea()
	sw.window.Component.SetBounds(maximizedWindowRect(area.W, area.H))
}

// installMaximizeButton overlays a maximize/restore button onto the window's
// title bar. The turbotv Window owns its title-bar chrome (minimize/close) and
// exposes no maximize affordance, so the button is layered by wrapping the window
// component's draw and click handlers: the draw paints the glyph after the base
// window draws (so it sits on top of the title bar), and the click claims the
// button's title-bar cells before the base handler so a click toggles maximize
// instead of starting a title-bar drag. This is the same wrap-and-fall-through
// pattern used to intercept transcript keys (see newSessionWindow).
func (sw *SessionWindow) installMaximizeButton() {
	w := sw.window
	baseDraw := w.Component.DrawFn
	w.Component.DrawFn = func(c *tv.VisualComponent, surface tv.Surface) {
		baseDraw(c, surface)
		sw.drawMaximizeButton(surface)
	}
	baseClick := w.Component.OnClickFn
	w.Component.OnClickFn = func(c *tv.VisualComponent, event tui.ClickEvent) bool {
		if sw.handleMaximizeClick(event) {
			return true
		}
		return baseClick(c, event)
	}
}

// drawMaximizeButton paints the maximize/restore glyph in the title bar, to the
// left of the minimize button. It is skipped while minimized (the collapsed
// title bar keeps only the minimize/close affordances, matching turbotv) and on
// windows too narrow for the button to clear the left border.
func (sw *SessionWindow) drawMaximizeButton(surface tv.Surface) {
	w := sw.window
	if w.IsMinimized() {
		return
	}
	abs := w.Component.AbsoluteBounds()
	r := maximizeButtonRect(abs, w.ShowClose, w.Minimizable)
	if r.X <= abs.X+1 {
		return
	}
	glyph := maximizeGlyph
	if sw.maximized {
		glyph = restoreGlyph
	}
	surface.WriteString(r.X, r.Y, glyph, tui.Cell{FG: w.TitleFG, BG: w.TitleBG, Bold: true})
}

// handleMaximizeClick claims a title-bar press that lands on the maximize button
// (toggling maximize) and returns true; any other click falls through to the base
// window handler. It ignores clicks while minimized and non-press events so they
// keep their usual behavior.
func (sw *SessionWindow) handleMaximizeClick(event tui.ClickEvent) bool {
	if !sw.maximizable {
		return false
	}
	w := sw.window
	abs := w.Component.AbsoluteBounds()
	if w.IsMinimized() || !event.Down || event.Y != abs.Y {
		return false
	}
	r := maximizeButtonRect(abs, w.ShowClose, w.Minimizable)
	if r.X <= abs.X+1 {
		return false
	}
	if event.X >= r.X && event.X <= r.Right() {
		sw.ToggleMaximize()
		return true
	}
	return false
}

// installSidebarClamp wraps the window's click handler so that once the base
// handler (and the maximize button) has moved or resized the window, its bounds
// are constrained back into the pinned window area (issue #106). It is the
// outermost click wrapper, so every drag, resize and maximize-button press is
// constrained. constrainToBounds is a no-op while the sidebar is unpinned, so free
// dragging is left untouched; it also skips minimized windows so their single-row
// title bar is not enlarged back to MinHeight.
func (sw *SessionWindow) installSidebarClamp() {
	sw.wb.installSidebarClampOn(sw.window)
}

// installSidebarClampOn wraps win's click handler so that, after the base
// handler has moved or resized the window, its bounds are constrained back into
// the pinned window area (issue #106) — the same constraint SessionWindow
// enforces. It is shared by session windows (via installSidebarClamp) and the
// sub-agent monologue popup (issue #319), both of which are bare tv.NewWindows.
// constrainWindowToBounds is a no-op while the sidebar is unpinned, so free
// dragging is left untouched.
func (w *Workbench) installSidebarClampOn(win *tv.Window) {
	base := win.Component.OnClickFn
	win.Component.OnClickFn = func(c *tv.VisualComponent, event tui.ClickEvent) bool {
		before := win.Component.Bounds
		handled := base(c, event)
		constrainWindowToBounds(w, win, before)
		return handled
	}
}

// constrainWindowToBounds is the pure-geometry core of the session-window sidebar
// clamp (issue #106), lifted to a free function so the sub-agent monologue popup can
// reuse it (issue #319). before is win's bounds before the click handler ran. It pulls win
// back inside wb.windowArea() after a click moved or resized it, telling drag and
// resize apart by what changed: a resize (width/height changed) keeps the origin and
// caps the size at the area, so the anchored edges stay put and only the dragged edge
// stops at the sidebar; a drag (only the origin changed) keeps the size and shifts the
// origin, so the window slides along the boundary instead of jumping. It is a no-op
// while the sidebar is unpinned, win is minimized, or the click changed nothing.
func constrainWindowToBounds(wb *Workbench, win *tv.Window, before tv.Rect) {
	if !wb.sidebarPinned || win.IsMinimized() {
		return
	}
	b := win.Component.Bounds
	if b == before {
		return
	}
	area := wb.windowArea()
	minW, minH := win.MinWidth, win.MinHeight
	var clamped tv.Rect
	if b.W != before.W || b.H != before.H {
		clamped = clampWindowSize(b, area, minW, minH)
	} else {
		clamped = clampWindowRect(b, area.W, area.H, minW, minH)
	}
	if clamped != b {
		win.Component.SetBounds(clamped)
	}
}

// clampToWindowArea fully clamps the window (size and origin) into the pinned
// window area. It is used when the sidebar is pinned on so any window left
// covering the sidebar is pulled back in. No-op while unpinned or minimized.
func (sw *SessionWindow) clampToWindowArea() {
	clampWindowToArea(sw.wb, sw.window)
}

// clampWindowToArea is the pure-geometry core of SessionWindow.clampToWindowArea,
// lifted to a free function so the monologue popup can be re-clamped on a sidebar
// pin-on / width change too (issue #319). It fully clamps win's size and origin into
// wb.windowArea(). No-op while the sidebar is unpinned or win is minimized.
func clampWindowToArea(wb *Workbench, win *tv.Window) {
	if !wb.sidebarPinned || win.IsMinimized() {
		return
	}
	area := wb.windowArea()
	b := win.Component.Bounds
	clamped := clampWindowRect(b, area.W, area.H, win.MinWidth, win.MinHeight)
	if clamped != b {
		win.Component.SetBounds(clamped)
	}
}

// clampWindowSize caps a rect's size so it fits inside area without moving its
// origin — the constraint a resize drag expects. The anchored top-left corner
// stays fixed and only the dragged bottom-right edge is capped at the area's
// right/bottom, so resizing up to the sidebar stops smoothly there instead of
// snapping the window left. When the origin already sits too close to an edge for
// the minimum size to fit, the minimum wins (the drag clamp handles the origin on
// the next move).
func clampWindowSize(r tv.Rect, area tv.Rect, minW, minH int) tv.Rect {
	if minW < 1 {
		minW = 1
	}
	if minH < 1 {
		minH = 1
	}
	if r.W < minW {
		r.W = minW
	}
	if r.H < minH {
		r.H = minH
	}
	if area.W > 0 && r.X >= 0 && r.X < area.W && r.X+r.W > area.W {
		r.W = area.W - r.X
	}
	if area.H > 0 && r.Y >= 0 && r.Y < area.H && r.Y+r.H > area.H {
		r.H = area.H - r.Y
	}
	if r.W < minW {
		r.W = minW
	}
	if r.H < minH {
		r.H = minH
	}
	return r
}

// selectedModelName returns the backend model identifier for the current select.
// The unique config Name is preferred so distinct endpoints sharing the same
// underlying model id can still be selected individually.
func (sw *SessionWindow) selectedModelName() string {
	cfg := sw.selectedModelConfig()
	if cfg != nil {
		if cfg.Name != "" {
			return cfg.Name
		}
		return cfg.Model
	}
	return ""
}

// selectedModelConfig resolves the chosen model from the header dropdown's
// SELECTED INDEX, not its display label (issue #389). The dropdown's option list
// is built positionally from Workbench.models (option i labels models[i]), so the
// index is the model's unambiguous identity: two configs sharing a DisplayName
// stay individually selectable, and the value sent to the backend can't collapse
// onto the first config with a matching label. Returns nil for a window with no
// model selector or an out-of-range selection.
func (sw *SessionWindow) selectedModelConfig() *config.ModelConfig {
	if sw.modelSelect == nil {
		return nil
	}
	return sw.wb.modelByIndex(sw.modelSelect.GetSelected())
}

// effortDefaultOption is the always-present first option of the effort selector
// (issue #177). It means "no override — use the model config's reasoning_effort";
// selectedEffort maps it to the empty string.
const effortDefaultOption = "(default)"

// effortLabelWidth is the width reserved for the "Effort" label to the left of the
// effort selector on the header row.
const effortLabelWidth = 7

// selectedEffort returns the per-session reasoning-effort override for the current
// selection (issue #177): the picked effort value, or "" for the "(default)"
// option (and for a disabled/empty selector), meaning "use the model config's
// reasoning_effort".
func (sw *SessionWindow) selectedEffort() string {
	if sw == nil || sw.effortSelect == nil || !sw.effortEnabled {
		return ""
	}
	v := sw.effortSelect.Value()
	if v == effortDefaultOption {
		return ""
	}
	return v
}

// rebuildEffortOptions repopulates the effort selector from the currently selected
// model's EffortOptions (issue #177): the options become ["(default)"] + those
// values, and the control is enabled only when the model offers any. It is called
// when the window is built and whenever the model selector changes, so the effort
// choices always match the active model. A selector with no model options is left
// showing just "(default)" and greyed out — its effort is then a no-op the request
// gate would drop anyway.
//
// Selection (issue #255), in priority order:
//  1. An explicit prior pick (not the "(default)" sentinel) is preserved when the
//     new model still offers it — a user's choice survives a model switch.
//  2. Otherwise — a fresh session, or a prior value that was the sentinel or is no
//     longer offered — the model's configured ReasoningEffort seeds the selection
//     when it is one of the offered options. This pins the value in effect at
//     session-create / model-switch time as the session's effort (the tradeoff:
//     such a session no longer follows later config edits to reasoning_effort).
//  3. Otherwise the selection falls back to "(default)" (index 0).
func (sw *SessionWindow) rebuildEffortOptions() {
	if sw.effortSelect == nil {
		return
	}
	prev := sw.effortSelect.Value()
	cfg := sw.selectedModelConfig()
	options := []string{effortDefaultOption}
	if cfg != nil {
		options = append(options, cfg.EffortOptions...)
	}
	sw.effortSelect.Options = options
	sw.effortEnabled = len(options) > 1
	// Grey the label alongside the selector when there is no effort to choose.
	if sw.effortLabel != nil {
		if sw.effortEnabled {
			sw.effortLabel.FG = sw.effortLabelEnabledFG
		} else {
			sw.effortLabel.FG = colorNote
		}
	}
	// (1) Preserve an explicit prior pick still offered by the new model. The
	// "(default)" sentinel is not an explicit pick, so it does not block seeding
	// the configured value below.
	sw.effortSelect.Selected = 0
	preserved := false
	if prev != "" && prev != effortDefaultOption {
		for i, opt := range options {
			if opt == prev {
				sw.effortSelect.Selected = i
				preserved = true
				break
			}
		}
	}
	// (2) No carried-over explicit pick: seed from the model's configured
	// reasoning_effort when that value is one of the offered options. (3) Falls
	// through to "(default)" (index 0) when there is no configured value or it is
	// not offered.
	if !preserved && cfg != nil && cfg.ReasoningEffort != "" {
		for i, opt := range options {
			if opt == cfg.ReasoningEffort {
				sw.effortSelect.Selected = i
				break
			}
		}
	}
}

// applyEffort selects a persisted effort value if the current model offers it
// (issue #177). An empty value (or one no longer offered, e.g. after the model's
// effort set changed) leaves the selector on "(default)". It assumes the options
// are already built for the current model (rebuildEffortOptions ran first).
func (sw *SessionWindow) applyEffort(effort string) {
	if sw.effortSelect == nil || effort == "" || !sw.effortEnabled {
		return
	}
	for i, opt := range sw.effortSelect.Options {
		if opt == effort {
			sw.effortSelect.Selected = i
			return
		}
	}
}

// guardEffortSelect wraps the effort selector's click and key handlers so a
// disabled (greyed-out) control is inert: it cannot be opened, so a model with no
// effort options offers no misleading interaction (issue #177). The wrapped
// handlers fall through to the base behaviour while the control is enabled.
func (sw *SessionWindow) guardEffortSelect() {
	c := sw.effortSelect.Component
	baseClick := c.OnClickFn
	c.OnClickFn = func(vc *tv.VisualComponent, event tui.ClickEvent) bool {
		if !sw.effortEnabled {
			return true // swallow the click; a disabled control does nothing
		}
		if baseClick != nil {
			return baseClick(vc, event)
		}
		return false
	}
	baseType := c.OnTypeFn
	c.OnTypeFn = func(vc *tv.VisualComponent, event tui.TypeEvent) bool {
		if !sw.effortEnabled {
			return false
		}
		if baseType != nil {
			return baseType(vc, event)
		}
		return false
	}
	// Grey out the value text when disabled by wrapping the draw. Both colours are
	// read live from the active dropdown roles (not captured at construction) so a
	// live theme change recolours them without a restart (#204, #260). The disabled
	// colour is dropdownDisabledFG, not the raw Note grey: #260 moved the closed
	// control onto the menu-bar background, which in the default palette equals Note,
	// so a Note-grey value would be grey-on-grey; dropdownDisabledColor keeps the dim
	// cue only where it still reads and falls back to a legible foreground otherwise.
	baseDraw := c.DrawFn
	c.DrawFn = func(vc *tv.VisualComponent, surface tv.Surface) {
		if !sw.effortEnabled {
			sw.effortSelect.FG = dropdownDisabledFG
		} else {
			sw.effortSelect.FG = dropdownFG
		}
		if baseDraw != nil {
			baseDraw(vc, surface)
		}
	}
}

// layoutEffortControl positions the right-aligned effort label + selector on the
// header row (issue #177). It pins the selector to the window's right edge and the
// label just to its left, mirroring the proposed geometry in the issue. On a
// window too narrow to show the control without overlapping the model selector
// (whose right edge is modelRight) it hides the control entirely (zero-width
// bounds) so the two never collide.
func (sw *SessionWindow) layoutEffortControl(wd, modelRight int) {
	if sw.effortSelect == nil || sw.effortLabel == nil {
		return
	}
	effW := effortSelectWidth(wd)
	selX := wd - effW - 1
	labelX := selX - effortLabelWidth
	// Hide when the label would overlap (or touch) the model selector's right edge,
	// leaving a one-cell gap between them.
	if labelX <= modelRight {
		sw.effortHidden = true
		sw.effortSelect.Component.SetBounds(tv.Rect{})
		sw.effortLabel.Component.SetBounds(tv.Rect{})
		return
	}
	sw.effortHidden = false
	sw.effortSelect.Component.SetBounds(tv.Rect{X: selX, Y: 0, W: effW, H: 1})
	sw.effortLabel.Component.SetBounds(tv.Rect{X: labelX, Y: 0, W: effortLabelWidth, H: 1})
}

// effortSelectWidth sizes the header effort dropdown (issue #177). It is a small
// fixed width sufficient for the values it shows ("(default)", "high", "max", and
// the longer OpenAI sets like "minimal"/"medium"), clamped to the room available
// in the window so it never runs off a narrow window.
func effortSelectWidth(windowWidth int) int {
	const w = 11 // fits "(default)" (9) + value pad + ▼ glyph
	if max := windowWidth - 1; w > max {
		if max < 1 {
			return 1
		}
		return max
	}
	return w
}

// Running-turn input controls (issue #201). The full labels carry their
// affordances — Enter on Queue, an error-coloured halt on Stop — and degrade to
// single-glyph forms on a window too narrow to show them beside a usable input.
const (
	interjectLabel = "Interject"
	queueLabel     = "Queue ⏎"
	stopLabel      = "■ Stop"

	interjectGlyph = "»"
	queueGlyph     = "⏎"
	stopGlyph      = "■"
)

// inputRowGap is the one-cell gap between adjacent input-row buttons (and between
// the input box and the first button); inputRowMargin is the right margin past the
// last button; minInputWidth is the prompt width the full-label button set must
// leave before the row degrades to glyph-only labels (issue #201).
const (
	inputRowGap    = 1
	inputRowMargin = 1
	minInputWidth  = 20
)

// buttonWidth is the cell width a button needs to show label inside its "[ … ]"
// frame. It matches the 8-wide idle Send button (4-cell "Send" + 4-cell frame).
func buttonWidth(label string) int { return tui.StringWidth(label) + 4 }

// uniformButtonWidth is the common cell width the three running-turn buttons share
// so they read as one set rather than three differently sized boxes (issue #214):
// the widest of the given labels' individual buttonWidths. In full-label mode that
// is Interject (the longest label); in glyph mode all three are equal, so it
// collapses to the single glyph width. turbotui's Button centres its "[ … ]"
// caption within its bounds, so widening a shorter label's box just pads it.
func uniformButtonWidth(labels ...string) int {
	w := 0
	for _, label := range labels {
		if bw := buttonWidth(label); bw > w {
			w = bw
		}
	}
	return w
}

// buttonRowY centres a 1-row input-row button against the multi-row prompt box so
// it sits on the prompt's middle line rather than floating at its top edge (issue
// #214). top is the input area's first row; inputH its height.
func buttonRowY(top, inputH int) int { return top + (inputH-1)/2 }

// runningButtonsColumnWidth is the horizontal room the vertically-stacked running-
// turn buttons claim on the input row (issue #234): the three buttons share a single
// right-aligned column, so it is one uniform button frame plus the one-cell gap to the
// prompt box on its left and the right margin past it — not three frames summed side by
// side as in the pre-#234 horizontal layout. It is the budget the glyph-degradation
// check measures the prompt against (uniform sizing, issue #214).
func runningButtonsColumnWidth(interject, queue, stop string) int {
	return uniformButtonWidth(interject, queue, stop) + inputRowGap + inputRowMargin
}

// controlsSeparatorRune is the box-drawing glyph repeated across the divider rule
// that fences the controls region (status line + input row) off from the transcript
// above it (issue #195).
const controlsSeparatorRune = "─"

// layoutControlsSeparator positions and fills the horizontal divider rule on row y,
// directly above the status line (issue #195). It spans the full inner width so
// that, together with the window frame's left/right/bottom borders, it boxes the
// controls region. The rule text is rebuilt from the current width on every layout
// so it tracks window resizes. A no-op when there is no separator (the read-only
// analysis window, which has no status/input chrome).
func (sw *SessionWindow) layoutControlsSeparator(wd, y int) {
	if sw.separator == nil {
		return
	}
	sw.separator.Component.SetBounds(tv.Rect{X: 0, Y: y, W: wd, H: 1})
	sw.separator.SetText(strings.Repeat(controlsSeparatorRune, wd))
}

// layoutInputRow positions the prompt box and its buttons on the bottom input row
// (issue #201). Idle shows the single Send button at the right with the three
// running-turn controls hidden (zero bounds). Busy hides Send and stacks the three
// running-turn controls in a single right-aligned column, one per input row, top→
// bottom Queue / Interject / Stop (issue #234) — the prompt box is inputH rows tall
// and each button is one row, so the column lines up with it — shrinking the input to
// the room left of the column. On a window too narrow to show the full labels beside a
// usable input the labels degrade to single glyphs before the input overflows.
func (sw *SessionWindow) layoutInputRow(wd, ht, inputH int) {
	top := ht - inputH
	if !sw.busy {
		// Hide via Visible, not just zero bounds: turbotv's focus traversal
		// (collectFocusable) skips !Visible/!Enabled but not zero-bounds widgets, so
		// an invisible-but-visible button would still catch a Tab and swallow Enter
		// (issue #201).
		sw.hideRunningButton(sw.interjectButton)
		sw.hideRunningButton(sw.queueButton)
		sw.hideRunningButton(sw.stopButton)
		sw.sendButton.Component.Visible = true
		// The idle Send button is one row tall; centre it on the prompt box's middle
		// line so it lines up with it instead of floating at its top edge (issue #214).
		// The busy column instead spans every input row, so it does not use this.
		rowY := buttonRowY(top, inputH)
		sw.input.Component.SetBounds(tv.Rect{X: 0, Y: top, W: wd - 10, H: inputH})
		sw.sendButton.Component.SetBounds(tv.Rect{X: wd - 9, Y: rowY, W: 8, H: 1})
		return
	}
	sw.hideRunningButton(sw.sendButton)
	sw.interjectButton.Component.Visible = true
	sw.queueButton.Component.Visible = true
	sw.stopButton.Component.Visible = true
	il, ql, sl := interjectLabel, queueLabel, stopLabel
	// Degrade to glyphs once the full-label button column would leave the prompt fewer
	// than minInputWidth cells. The column consumes runningButtonsColumnWidth — one
	// uniform button frame plus the gap to the prompt on its left and the right margin —
	// so the whole of that is the budget the prompt is measured against (issues #201,
	// #234).
	if runningButtonsColumnWidth(il, ql, sl) > wd-minInputWidth {
		il, ql, sl = interjectGlyph, queueGlyph, stopGlyph
	}
	sw.interjectButton.SetLabel(il)
	sw.queueButton.SetLabel(ql)
	sw.stopButton.SetLabel(sl)
	// One uniform width for all three so they read as a consistent column; turbotui
	// centres each button's caption within its box (issue #214).
	btnW := uniformButtonWidth(il, ql, sl)
	// Stack the buttons in a single right-aligned column — they share one X and width,
	// one button per input row, top→bottom Queue / Interject / Stop (issue #234). With
	// inputH == 3 (the prompt box height) the rows are top, top+1, top+2: exactly the
	// prompt's three rows, so the column lines up cleanly beside it. The prompt shrinks
	// to the room left of the column (its left gap included).
	btnX := wd - inputRowMargin - btnW
	inputW := btnX - inputRowGap
	if inputW < 1 {
		inputW = 1
	}
	queueY, interjectY, stopY := runningButtonStackRows(top, inputH)
	sw.input.Component.SetBounds(tv.Rect{X: 0, Y: top, W: inputW, H: inputH})
	sw.queueButton.Component.SetBounds(tv.Rect{X: btnX, Y: queueY, W: btnW, H: 1})
	sw.interjectButton.Component.SetBounds(tv.Rect{X: btnX, Y: interjectY, W: btnW, H: 1})
	sw.stopButton.Component.SetBounds(tv.Rect{X: btnX, Y: stopY, W: btnW, H: 1})
}

// runningButtonStackRows returns the Y rows of the three vertically-stacked running-
// turn buttons (issue #234), top→bottom Queue / Interject / Stop, anchored to the top
// of the input area at row top. With inputH >= 3 (the prompt box is three rows tall)
// they take three consecutive rows — top, top+1, top+2 — so the column fills the prompt
// box beside it. For a shorter input area each row is clamped to its last row
// (top+inputH-1) so a button can never spill below the input area onto the status line
// that sits directly under it — trading an out-of-bounds row for two buttons sharing
// the bottom row (a visible overlap, the least-bad option once three single-row buttons
// no longer fit). That clamp is only a safety net for a future inputH < 3, not a
// configuration any current caller produces: the sole caller passes inputH == 3 (where
// no clamping occurs), and the layout guard requires the window be tall enough for it.
func runningButtonStackRows(top, inputH int) (queue, interject, stop int) {
	last := top + inputH - 1
	clamp := func(y int) int {
		if y > last {
			return last
		}
		return y
	}
	return clamp(top), clamp(top + 1), clamp(top + 2)
}

// hideRunningButton removes an input-row button from view and, crucially, from the
// Tab-focus cycle: turbotv's collectFocusable skips !Visible widgets but keeps
// zero-bounds ones, so a merely zeroed button would still catch focus and swallow
// Enter (issue #201). Zeroing the bounds too keeps the layout assertions simple.
func (sw *SessionWindow) hideRunningButton(b *tv.Button) {
	b.Component.Visible = false
	b.Component.SetBounds(tv.Rect{})
}

// restoreInputFocusFromButtons re-homes keyboard focus to the prompt when one of
// the running-turn buttons holds it as the turn ends (issue #201). Those buttons
// are hidden on the next layout via Visible=false, but turbotv clears a stale
// d.focused only on layer add/remove/raise — never on a Visible flip — so the
// focused-but-invisible button would otherwise swallow typed keys (key dispatch
// requires the focused widget to be visible-in-tree) and the input would look dead
// until the user tabbed or clicked. Scoped to the case where a running button is
// actually focused, so it never steals focus from another window, an open dialog,
// or a control the user deliberately moved to.
func (sw *SessionWindow) restoreInputFocusFromButtons() {
	if sw.input == nil {
		return
	}
	if sw.interjectButton.Component.Focused() ||
		sw.queueButton.Component.Focused() ||
		sw.stopButton.Component.Focused() {
		sw.wb.desktop.SetFocus(sw.input)
	}
}

// guardInterjectButton recolours the Interject button per draw to track its
// enabled/disabled state — the input is empty, so there is nothing to slip into the
// running turn (issue #201). It wraps the button's draw to swap its foreground via
// interjectButtonFG, mirroring guardEffortSelect; the interject() action enforces
// the same guard, so a stray activation is inert too.
func (sw *SessionWindow) guardInterjectButton() {
	b := sw.interjectButton
	baseDraw := b.Component.DrawFn
	// The colour is read live from the active turbotui theme (not captured at
	// construction) so a live theme change recolours it without a restart (#204).
	b.Component.DrawFn = func(vc *tv.VisualComponent, surface tv.Surface) {
		b.FG = interjectButtonFG(sw.interjectEnabled())
		if baseDraw != nil {
			baseDraw(vc, surface)
		}
	}
}

// interjectButtonFG is the Interject button's foreground for the given enabled
// state, read live from the active theme so a live theme switch recolours it
// (issues #214, #204). Enabled, it matches Queue (the theme's ButtonFG) and stays
// distinct from Stop's error red. Disabled (empty input), it de-emphasises the
// label without dropping it to the illegible ~1.3:1 the old colorNote reached on the
// default theme's green button: colorNote is kept where it still clears the 3:1
// large-text floor against the button background (the dark button canvas of the
// dark/high-contrast presets) or where that background is the terminal default
// (NO_COLOR — contrast is undeterminable and colorNote is itself the default);
// otherwise it falls back to the higher-contrast of black/white, which on the green
// button is black: clearly readable yet visibly recessed from the bright-white
// enabled label. Coordinates with the #202 contrast audit (contrastRatio,
// minContrastLarge) rather than re-introducing a one-off low-contrast colour.
//
// This drives only the resting foreground (b.FG). On keyboard focus turbotui paints
// the button with the theme's ButtonFocusFG/ButtonFocusBG instead, so a focused
// Interject deliberately follows the default button focus colours — matching a
// focused Queue (the "consistent with Queue" ask) and staying legible — rather than
// pinning a focus FG the way Stop pins colorError, whose red is a semantic identity
// Interject does not share.
func interjectButtonFG(enabled bool) tui.Color {
	th := tv.ActiveTheme()
	if enabled {
		return th.ButtonFG
	}
	if c := contrastRatio(colorNote, th.ButtonBG); c == 0 || c >= minContrastLarge {
		return colorNote
	}
	return mostReadableOn(th.ButtonBG)
}

// mostReadableOn returns whichever of black/white has the greater WCAG contrast
// against bg — the most legible monochrome foreground for an arbitrary button
// background (issue #214).
func mostReadableOn(bg tui.Color) tui.Color {
	black, white := tui.ANSIColor(0), tui.ANSIColor(15)
	if contrastRatio(white, bg) >= contrastRatio(black, bg) {
		return white
	}
	return black
}

// refreshTheme re-applies the active palette to a live session window after a
// theme change, without a restart (issue #204). It mirrors what a fresh
// construction does once ApplyTheme has installed the new palette: it re-renders
// the transcript so every existing record and child line resolves its semantic
// role to the new colours, re-seeds the turbotui widget chrome (the window frame
// and content surface, the model/effort labels and selectors, the input box and
// its buttons) from the freshly installed tv theme, and restores the gogent
// accents the window sets itself — the error-red Stop button, the divider rule,
// the cached effort-label colour and the severity-coloured status line. A
// read-only analysis window has no input chrome, so only its transcript and frame
// are refreshed.
func (sw *SessionWindow) refreshTheme() {
	th := tv.ActiveTheme()

	// Window frame + content surface (turbotui seeds these once at construction).
	w := sw.window
	w.TitleFG, w.TitleBG = th.WindowTitleFG, th.WindowTitleBG
	w.BorderFG, w.BorderBG = th.WindowBorderFG, th.WindowBorderBG
	w.CloseFG, w.CloseBG = th.CloseButtonFG, th.CloseButtonBG
	w.ShadowColor = th.WindowShadow
	applyWindowShadow(w) // re-apply the NoShadow toggle live (issue #215)
	w.Content.Background = tui.Cell{Ch: ' ', FG: th.WindowFG, BG: th.WindowBG}

	// Transcript view: the TextView fills its whole area with its own BG and paints
	// glyphs/scrollbar with its FG/FocusFG, all seeded once at construction. Without
	// reseeding them the transcript area keeps its old background — a blue panel in a
	// dark frame after a default→dark switch — so reseed them from the same theme
	// slots NewTextView uses (issue #204). Done before the read-only return so an
	// analysis window's transcript is reskinned too.
	sw.history.FG, sw.history.BG, sw.history.FocusFG = th.WindowFG, th.WindowBG, th.MnemonicFG

	// Transcript: a full re-render so frozen header/line colours resolve to the new
	// palette via their roles, and rich-Markdown bodies recompute from the bumped
	// generation cache.
	sw.transcript.render()

	if sw.readOnly {
		return
	}

	// Header labels and selectors.
	reseedLabel(sw.modelLabel, th)
	reseedLabel(sw.effortLabel, th)
	reseedSelect(sw.modelSelect, th)
	reseedSelect(sw.effortSelect, th)
	// The remembered "enabled" effort-label colour is the themed window foreground;
	// refresh it so the greyed→enabled restore (rebuildEffortOptions) uses the new
	// palette, then re-apply the colour for the current enabled state.
	sw.effortLabelEnabledFG = th.WindowFG
	if sw.effortEnabled {
		sw.effortLabel.FG = sw.effortLabelEnabledFG
	} else {
		sw.effortLabel.FG = colorNote
	}

	// Input box and its row buttons.
	in := sw.input
	in.FG, in.BG, in.FocusFG, in.FocusBG = th.InputFG, th.InputBG, th.InputFocusFG, th.InputFocusBG
	reseedButton(sw.sendButton, th)
	reseedButton(sw.queueButton, th)
	reseedButton(sw.interjectButton, th)
	reseedButton(sw.stopButton, th)
	// Stop stays error-red, even when keyboard-focused (issue #201).
	sw.stopButton.FG, sw.stopButton.FocusFG = colorError, colorError

	// Status and divider labels: reseed their background/hot colours like the other
	// labels (Label.draw fills its bounds with BG), then re-apply the gogent
	// foregrounds they own — the divider rule's chrome colour and the status line's
	// severity colour (set by refreshStatus) (issue #204).
	reseedLabel(sw.status, th)
	reseedLabel(sw.separator, th)
	sw.separator.FG = chromeDivider
	sw.refreshStatus()
}

// reseedLabel re-applies a turbotui theme's foreground colours to a label whose
// colours were seeded at construction (issue #204).
func reseedLabel(l *tv.Label, th tv.Theme) {
	if l == nil {
		return
	}
	l.FG, l.BG, l.HotFG = th.WindowFG, th.WindowBG, th.MnemonicFG
}

// reseedSelect re-applies the active dropdown roles to a selector whose colours
// were seeded at construction, so a live theme switch recolours an already-built
// Select without a restart (issues #204, #260). The closed-control colours come
// from the package dropdown vars ApplyTheme installs (turbotui's Select has no
// theme slot of its own), not from th — which is retained for signature symmetry
// with reseedLabel/reseedButton and the construction-time call sites. The
// enabled/disabled foreground is driven per draw by guardEffortSelect, so only the
// background and focus colours are restored here. The open popup's highlighted row
// follows tv.DefaultTheme.Selection*, installed by ApplyTheme.
func reseedSelect(s *tv.Select, _ tv.Theme) {
	if s == nil {
		return
	}
	s.FG, s.BG, s.FocusFG, s.FocusBG = dropdownFG, dropdownBG, dropdownFocusFG, dropdownFocusBG
	applySelectShadow(s) // re-apply the NoShadow toggle live (issue #231)
}

// reseedButton re-applies a turbotui theme's button colours to a button whose
// colours were seeded at construction (issue #204).
func reseedButton(b *tv.Button, th tv.Theme) {
	if b == nil {
		return
	}
	b.FG, b.BG = th.ButtonFG, th.ButtonBG
	b.FocusFG, b.FocusBG = th.ButtonFocusFG, th.ButtonFocusBG
	b.ShadowColor = th.ButtonShadow
	applyButtonShadow(b) // re-apply the NoShadow toggle live (issue #215)
}

// enqueue stows a message typed while the agent is busy as the session's single
// pending slot (issue #170, phase 1). It is latest-wins: a new entry replaces an
// undrained one (edit-in-place), so the user can correct a queued message before
// it fires; the drain-on-idle path then sends it when the agent finishes. The
// queued text is echoed as a note and surfaced in the status line so it is visible
// and editable before it fires. This is the Enter/Queue path; mid-turn delivery is
// the separate Interject button (issue #201).
func (sw *SessionWindow) enqueue(text string) {
	replaced := sw.pending != ""
	sw.pending = text
	sw.pendingCmd = nil // a plain message must not inherit a queued command's override
	if replaced {
		sw.addNote("queued message replaced: " + text)
	} else {
		sw.addNote("queued (will send when the agent finishes): " + text)
	}
	sw.refreshStatus()
}

// enqueueCommand stows an expanded custom-command prompt and its overrides as the
// single latest-wins pending slot (issue #403). It mirrors enqueue but pairs the
// text with the command's overrides so drainQueue re-applies the model override
// instead of falling back to the session's current model. The expanded text is
// already mention-free, so the drain sends it verbatim (no re-expansion).
func (sw *SessionWindow) enqueueCommand(text string, ov pendingCommand) {
	replaced := sw.pending != ""
	sw.pending = text
	cmd := ov
	sw.pendingCmd = &cmd
	if replaced {
		sw.addNote("queued message replaced: " + text)
	} else {
		sw.addNote("queued (will send when the agent finishes): " + text)
	}
	sw.refreshStatus()
}

// interject splices the current input text into the running turn now (issue #201),
// via OnInject → UserSession.InjectUserNote, then clears the box. It is the
// button-only counterpart to Enter/Queue — a per-message action, not a global
// mode — so the model sees the text as a clarification before its next step. It is
// a no-op when the input is empty (nothing to say) or when no turn is in flight,
// matching the button's disabled state.
func (sw *SessionWindow) interject() {
	text := strings.TrimSpace(sw.input.GetText())
	if text == "" || !sw.busy {
		return
	}
	// Check the handler before clearing the box, so an unwired backend does not
	// destroy the typed text (issue #201).
	if sw.wb.handlers.OnInject == nil {
		sw.addNote("interject unavailable")
		return
	}
	sw.completer.hide()
	sw.input.Clear()
	// Echo the interjection as the user's own message — a "You (clarification):"
	// record, not a [System] note — since it is the user's input, equally with a
	// normally-sent message (issue #242). This is the one place the text is shown;
	// the backend no longer re-emits it as an assistant "thought".
	sw.addClarification(text)
	go sw.wb.handlers.OnInject(sw.id, text)
}

// interjectEnabled reports whether the Interject button is actionable: a turn is
// in flight and there is non-blank input text to slip into it (issue #201). It
// drives both the button's greyed disabled state and the interject() guard; the
// busy term keeps the predicate honest even though the button is hidden while idle.
func (sw *SessionWindow) interjectEnabled() bool {
	return sw.busy && strings.TrimSpace(sw.input.GetText()) != ""
}

// clearQueue discards any pending queued message, optionally noting why. It is
// used both by the /clearqueue command and when the user stops the agent, so a
// stop never silently auto-fires a queued message (issue #170).
func (sw *SessionWindow) clearQueue(note string) {
	if sw.pending == "" {
		return
	}
	sw.pending = ""
	sw.pendingCmd = nil
	if note != "" {
		sw.addNote(note)
	}
	sw.refreshStatus()
}

// drainQueue fires the pending queued message as the next user turn once the
// agent has returned to idle (issue #170, phase 1). It re-enters the normal
// submit path so the queued text gets the same treatment (mention expansion,
// transcript echo, busy handling) as a hand-typed message. draining guards the
// re-entry so the auto-submitted message cannot itself be re-queued.
func (sw *SessionWindow) drainQueue() {
	if sw.pending == "" || sw.busy || sw.draining || sw.submitFn == nil {
		return
	}
	text := sw.pending
	sw.pending = ""
	cmd := sw.pendingCmd
	sw.pendingCmd = nil
	sw.draining = true
	if cmd != nil {
		// A queued custom command: re-send the already-expanded text directly so its
		// model override is re-applied (the normal submit path would use the current
		// model). No mention expansion — the text is already final.
		sw.sendCommandNow(text, *cmd)
		sw.draining = false
		return
	}
	sw.input.SetText(text)
	sw.submitFn()
	sw.input.Clear()
	sw.draining = false
}

// recordHistory appends a just-submitted prompt to the per-session recall history
// and resets navigation to the newest (issue #203). A prompt identical to the most
// recent one is not duplicated. Resetting historyNav to len means the next Up
// starts from the newest entry again, and the stashed draft is cleared (the input
// is cleared on submit, so there is no in-progress text to preserve).
func (sw *SessionWindow) recordHistory(text string) {
	if n := len(sw.promptHistory); n == 0 || sw.promptHistory[n-1] != text {
		sw.promptHistory = append(sw.promptHistory, text)
	}
	sw.historyNav = len(sw.promptHistory)
	sw.historyDraft = ""
}

// handleHistoryKey applies Up/Down prompt-history recall, returning true when it
// consumed the event (issue #203). Up recalls an older prompt only when the caret
// sits on the first visual line; Down recalls a newer one only on the last visual
// line — so single-line prompts (the common case) always recall, while interior
// multi-line editing keeps moving the caret between lines. With no history, or off
// the relevant edge, it returns false so the input handles the key as usual.
func (sw *SessionWindow) handleHistoryKey(event tui.TypeEvent) bool {
	if len(sw.promptHistory) == 0 {
		return false
	}
	// Plain Up/Down only. A modifier turns the arrow into a different gesture the
	// input already owns — notably Shift+arrow extends the selection — so history
	// must not intercept it, even at a buffer edge.
	if event.Shift || event.Ctrl || event.Alt {
		return false
	}
	switch event.Key {
	case tui.KeyUp:
		if !sw.caretOnFirstVisualLine() {
			return false
		}
		return sw.historyPrev()
	case tui.KeyDown:
		if !sw.caretOnLastVisualLine() {
			return false
		}
		return sw.historyNext()
	}
	return false
}

// historyPrev recalls the previous (older) prompt, caret at end. The first Up
// stashes the in-progress draft and jumps to the newest entry; subsequent Ups step
// older. At the oldest entry it stops (no wrap) but still consumes the key. It
// returns true whenever history is in play (non-empty), so the caret does not also
// move.
func (sw *SessionWindow) historyPrev() bool {
	n := len(sw.promptHistory)
	if n == 0 {
		return false
	}
	switch {
	case sw.historyNav >= n:
		sw.historyDraft = sw.input.GetText()
		sw.historyNav = n - 1
	case sw.historyNav > 0:
		sw.historyNav--
	default:
		return true // already at the oldest entry: stop, no wrap
	}
	sw.input.SetText(sw.promptHistory[sw.historyNav])
	return true
}

// historyNext recalls the next (newer) prompt, caret at end. Stepping past the
// newest entry restores the stashed in-progress draft and leaves navigation mode.
// When not navigating there is nothing newer to recall, so it returns false and
// lets the input move the caret instead.
func (sw *SessionWindow) historyNext() bool {
	n := len(sw.promptHistory)
	if sw.historyNav >= n {
		return false // not navigating: nothing newer
	}
	if sw.historyNav < n-1 {
		sw.historyNav++
		sw.input.SetText(sw.promptHistory[sw.historyNav])
		return true
	}
	sw.historyNav = n
	sw.input.SetText(sw.historyDraft)
	return true
}

// caretOnFirstVisualLine reports whether the input caret sits on the topmost
// visual row, where Up recalls older history rather than moving up a line. A caret
// on a wrapped continuation of the first logical line is not on the first visual
// line. The visual-row geometry comes from the widget's real wrap layout
// (CaretRowInLine), so it is correct under word wrap (issue #270).
func (sw *SessionWindow) caretOnFirstVisualLine() bool {
	in := sw.input
	if in == nil || in.CursorY != 0 {
		return false
	}
	row, _ := in.CaretRowInLine()
	return row == 0
}

// caretOnLastVisualLine reports whether the input caret sits on the bottommost
// visual row, where Down recalls newer history rather than moving down a line.
func (sw *SessionWindow) caretOnLastVisualLine() bool {
	in := sw.input
	if in == nil {
		return false
	}
	if in.CursorY != len(in.Lines)-1 {
		return false
	}
	row, rows := in.CaretRowInLine()
	return row == rows-1
}

// setBusy updates the status line and busy flag, anchoring the live elapsed
// timer to the turn's start (and clearing it when the turn ends). The status
// colour is left to refreshStatus, which folds in the context/budget severity.
// On the busy→idle edge it drains any message queued during the turn as the next
// user turn (issue #170, phase 1).
func (sw *SessionWindow) setBusy(busy bool) {
	wasIdle := sw.effectiveIdle()
	sw.busy = busy
	if busy {
		sw.statusState = "working..."
		// The Send button is hidden while busy (replaced by the running-turn
		// controls, issue #201), so its label no longer needs a "..." state.
		sw.turnStart = time.Now()
		sw.turnStartOut = sw.statusStats.TokensOut
		sw.refreshStatus()
		return
	}
	// The main loop's turn ended. If async sub-agents are still running in the
	// background, the session is NOT idle (issue #353): show the background state and
	// hold off the busy→idle edge work until the workers also finish.
	if sw.background {
		sw.statusState = backgroundStatusText
		sw.refreshStatus()
		return
	}
	sw.enterIdle(!wasIdle)
}

// backgroundStatusText is the left-hand status shown while only async sub-agents are
// running (the main loop has finished its turn) — issue #353.
const backgroundStatusText = "working in background..."

// effectiveIdle reports whether the session is truly idle: no foreground turn AND no
// background sub-agents. The busy→idle edge logic must run only on the transition
// into this state (issue #353).
func (sw *SessionWindow) effectiveIdle() bool {
	return !sw.busy && !sw.background
}

// setBackground updates the background-work flag from the backend's
// SessionEventBackground (issue #353) and reconciles the status line. It is the
// counterpart to setBusy for the third state. When background work starts while the
// main loop is idle it shows "working in background..."; when it clears it either
// falls back to "working..." (a foreground turn is still running) or runs the
// busy→idle edge logic (the session is now truly idle).
func (sw *SessionWindow) setBackground(on bool) {
	wasIdle := sw.effectiveIdle()
	sw.background = on
	switch {
	case sw.busy:
		// A foreground turn is in flight; it owns the status line. Just keep the timer
		// and refresh so liveStats still ticks.
		sw.refreshStatus()
	case on:
		// Idle main loop, background work starting: show the background state and
		// anchor the elapsed timer if a turn was not already timing.
		sw.statusState = backgroundStatusText
		if sw.turnStart.IsZero() {
			sw.turnStart = time.Now()
			sw.turnStartOut = sw.statusStats.TokensOut
		}
		sw.refreshStatus()
	default:
		// Background work just finished and no foreground turn is running -> truly idle.
		sw.enterIdle(!wasIdle)
	}
}

// enterIdle settles the status line to idle and, on a real transition into idle,
// runs the busy→idle edge work — failing orphaned tools, folding any live thought,
// restoring focus off the running-turn buttons, draining a queued message and
// running the supervisor watchdog (issues #187, #217, #201, #170, #172). It is the
// single funnel for "the session just became truly idle", shared by the
// foreground-turn end (setBusy) and the background-work end (setBackground), so the
// edge work fires exactly once whichever finishes last (issue #353).
func (sw *SessionWindow) enterIdle(transition bool) {
	sw.statusState = "idle"
	sw.turnStart = time.Time{}
	sw.turnStartOut = 0
	// The turn is over: any tool entry still marked "running" never got its
	// result event (cancel, early loop exit, backend crash). Flip it to a
	// terminal state so nothing is left stuck "running" (issue #187). A clean
	// turn leaves the map empty, so this is a no-op then.
	sw.failPendingTools("interrupted")
	// Likewise fold any live streamed thinking entry left open by a cancelled or
	// crashed turn so it doesn't stay expanded "thinking…" forever (issue #217).
	sw.foldLiveThought()
	sw.refreshStatus()
	if !transition {
		return
	}
	// On the →idle edge the running-turn buttons are about to be hidden by the
	// next layout. If one of them currently holds keyboard focus, restore it to the
	// prompt first (issue #201): turbotv only re-homes a stale focus on layer
	// add/remove/raise, not on a Visible flip during layout, so a focused-but-hidden
	// button would otherwise swallow keystrokes until the user tabbed or clicked.
	sw.restoreInputFocusFromButtons()
	// Auto-submit a queued message on the →idle transition. Skipped while draining so
	// a drained message is not itself re-queued.
	if !sw.draining {
		sw.drainQueue()
		// Run the supervisor's idle watchdog on the same edge, after the queue has had
		// its chance to drain (issue #172). drainQueue re-enters the submit path
		// synchronously, so by here sw.busy reflects whether a queued message fired;
		// maybeSupervise no-ops while busy/pending so a drained real message always
		// takes precedence over a nudge.
		sw.maybeSupervise()
	}
}

// refreshStatus rebuilds the bottom status line from the current state text,
// per-session stats and the live elapsed/throughput figures, sized to the
// label's current width so it truncates gracefully on narrow windows. It also
// sets the status colour (severity over idle/working) and raises the one-time
// budget-exceeded transcript note on the threshold crossing.
func (sw *SessionWindow) refreshStatus() {
	budget := sw.wb.budgetConfig()
	live := sw.liveStats()
	sw.status.FG = statusColorFor(sw.busy, sw.background, sw.statusStats, budget)
	state := sw.statusState
	if sw.planMode {
		// Surface plan mode at the left of the status line so the read-only turn
		// is unmistakable (issue #43).
		state = "PLAN · " + state
	}
	if sw.yoloMode {
		// Surface yolo at the left of the status line so the auto-approve +
		// unlimited-steps posture is unmistakable (issue #356).
		state = "YOLO · " + state
	}
	if sw.pending != "" {
		// Show the queued message distinctly so it is visible (and known to be
		// editable/cancellable) before it fires (issue #170). Trim long entries so
		// the chip never crowds out the rest of the status line.
		state += " · queued: " + queuedPreview(sw.pending)
	}
	sw.status.SetText(formatStatusLine(state, sw.statusStats, live, budget, sw.status.Component.Bounds.W))
	sw.alertBudgetIfNewlyExceeded(budget)
}

// liveStats computes the transient generation-time figures (elapsed since the
// turn started and the output-token throughput) shown only while a turn is in
// flight. It returns a zero value when idle or before the turn has started.
func (sw *SessionWindow) liveStats() liveStats {
	// Keep the elapsed/throughput figures alive while ANY work is in flight —
	// foreground turn or background sub-agents (issue #353) — so the status line does
	// not freeze its timer when only background workers remain.
	if (!sw.busy && !sw.background) || sw.turnStart.IsZero() {
		return liveStats{}
	}
	elapsed := time.Since(sw.turnStart)
	out := liveStats{elapsed: elapsed}
	if produced := sw.statusStats.TokensOut - sw.turnStartOut; elapsed > 0 && produced > 0 {
		out.tokensPerSec = float64(produced) / elapsed.Seconds()
	}
	return out
}

// alertBudgetIfNewlyExceeded records a one-line transcript note the first time a
// session's cumulative token usage crosses its configured budget, and clears the
// latch if usage ever drops back below it (it cannot for cumulative tokens, but
// the guard keeps the logic honest if the budget is later lowered).
func (sw *SessionWindow) alertBudgetIfNewlyExceeded(budget config.BudgetConfig) {
	exceeded := budgetStatus(sw.statusStats, budget) == budgetExceeded
	switch {
	case exceeded && !sw.budgetAlerted:
		sw.budgetAlerted = true
		used := sw.statusStats.TokensIn + sw.statusStats.TokensOut
		sw.transcript.add(&transcriptRecord{
			kind:   kindSystem,
			header: "[Budget] token budget exceeded",
			color:  colorError,
			role:   roleError,
			lines: styledChildLines(
				fmt.Sprintf("Cumulative usage %d tok reached the configured budget of %d tok.", used, budget.TokenBudget),
				roleError),
		})
	case !exceeded:
		sw.budgetAlerted = false
	}
}

// apply renders a single backend session event into the transcript.
func (sw *SessionWindow) apply(ev agent.SessionEvent) {
	switch ev.Type {
	case agent.SessionEventThinking:
		sw.statusState = fmt.Sprintf("thinking... (step %d)", ev.Step)
		sw.refreshStatus()
	case agent.SessionEventThinkingDelta:
		sw.appendThinkingDelta(ev.Text)
	case agent.SessionEventThinkingDone:
		sw.foldLiveThought()
	case agent.SessionEventUsage:
		sw.statusStats = ev.Stats
		sw.refreshStatus()
	case agent.SessionEventAssistantStep:
		sw.addThought(ev.Text)
	case agent.SessionEventToolCall:
		sw.beginToolCall(ev.CallID, ev.Tool, ev.Args)
	case agent.SessionEventToolResult:
		sw.finishToolCall(ev.CallID, ev.Tool, ev.Result)
	case agent.SessionEventFinal:
		sw.addAssistant(ev.Text)
		sw.setBusy(false)
	case agent.SessionEventPlan:
		// The plan itself arrives as the assistant's final answer; this just marks
		// that a plan is awaiting approval so /act (and the menu) can execute it
		// (issue #43). A non-plan turn clears a stale pending flag.
		sw.planPending = strings.TrimSpace(ev.Plan) != ""
		if sw.planPending {
			sw.addNote("Plan ready for review — approve with /act (or Session → Approve Plan) to execute it.")
		}
	case agent.SessionEventYolo:
		// Backend-owned yolo indicator (issue #356): the display field is set here
		// (never from the local toggle) so it is correct however yolo was activated.
		sw.applyYoloMode(ev.Yolo)
	case agent.SessionEventBackground:
		// Backend-owned "working in background" indicator (issue #353): set the
		// display state from the event (never inferred locally) so the status line and
		// sidebar glyph stay correct whether the main loop is mid-turn or already idle.
		sw.setBackground(ev.Background)
	case agent.SessionEventTodo:
		// The checklist is rendered in the sidebar; nothing to add to the
		// transcript (issue #43).
	case agent.SessionEventCompaction:
		sw.addCompaction(ev.Step, ev.Text)
	case agent.SessionEventError:
		if ev.Err != nil {
			sw.addError(ev.Err.Error())
		}
		sw.setBusy(false)
	}
}

// queuedPreview renders a compact, single-line preview of a queued message for
// the status line (issue #170): newlines become spaces and an over-long entry is
// truncated with an ellipsis so the chip stays short.
func queuedPreview(text string) string {
	const max = 40
	preview := strings.ReplaceAll(strings.TrimSpace(text), "\n", " ")
	if utf8.RuneCountInString(preview) > max {
		runes := []rune(preview)
		preview = string(runes[:max]) + "…"
	}
	return preview
}

// styledChildLines splits text into foldable child lines sharing one semantic
// role. Each line records both the role and a snapshot of its current colour so a
// later theme change recolours it on re-render (issue #204).
func styledChildLines(text string, role colorRole) []styledLine {
	color := roleColor(role)
	lines := childLines(text)
	out := make([]styledLine, len(lines))
	for i, line := range lines {
		out[i] = styledLine{text: line, color: color, role: role}
	}
	return out
}

// addUser appends the user's message.
// userRecord builds the "You:" transcript record for a user message, or nil when
// the text is blank. Returning nil for blank folds the skip guard that addUser and
// restore previously applied inline into one place, so the live add path and the
// batched restore path share a single source of truth for the record's shape and
// its blank-skip rule (issue #519).
func userRecord(text string) *transcriptRecord {
	if strings.TrimSpace(text) == "" {
		return nil
	}
	return &transcriptRecord{
		kind: kindUser, header: "You:", color: colorUser, role: roleUser,
		lines: styledChildLines(text, roleUser),
	}
}

func (sw *SessionWindow) addUser(text string) {
	if r := userRecord(text); r != nil {
		sw.transcript.add(r)
	}
}

// addClarification appends an interjected message as the user's own input
// (issue #242). It is the same kindUser/colorUser/roleUser record as addUser —
// so it reads as "You", not as a [System] note or a model "thought" — but carries
// a "You (clarification):" header to mark that the text was slipped into a turn
// already in flight via Interject (issue #201) rather than sent as a fresh turn.
func (sw *SessionWindow) addClarification(text string) {
	sw.transcript.add(&transcriptRecord{
		kind: kindUser, header: "You (clarification):", color: colorUser, role: roleUser,
		lines: styledChildLines(text, roleUser),
	})
}

// addNote appends a one-line system note to the transcript, used to echo
// client-side command feedback.
func (sw *SessionWindow) addNote(text string) {
	sw.transcript.add(&transcriptRecord{
		kind:   kindSystem,
		header: "[System]",
		color:  colorInfo,
		role:   roleInfo,
		lines:  styledChildLines(text, roleInfo),
	})
}

// handleSlashCommand interprets a leading "/..." input as a client-side command.
// It returns true when the command was recognized and handled (the caller clears
// the input without sending anything to the model), and false when the input is
// not a recognized command and should be sent as a normal message.
func (sw *SessionWindow) handleSlashCommand(text string) bool {
	if !strings.HasPrefix(text, "/") {
		return false
	}
	fields := strings.Fields(text)
	switch fields[0] {
	case "/undo":
		summary, err := sw.callUndo(false, 0)
		sw.echoCommand("/undo", summary, err)
		return true
	case "/rewind":
		turns := 0 // 0 => revert every recorded turn
		if len(fields) >= 2 {
			if n, err := strconv.Atoi(fields[1]); err == nil {
				turns = n
			} else {
				sw.echoCommand("/rewind", "", fmt.Errorf("usage: /rewind [turns]"))
				return true
			}
		}
		summary, err := sw.callUndo(true, turns)
		sw.echoCommand("/rewind", summary, err)
		return true
	case "/fork":
		// Branch off a peer session seeded with this session's full history (issue
		// #349). ForkSession opens and focuses the new window itself; it is a no-op
		// for an unknown parent.
		sw.wb.ForkSession(sw.id)
		return true
	case "/plan":
		sw.togglePlanMode()
		return true
	case "/yolo":
		sw.toggleYoloMode()
		return true
	case "/act":
		sw.approvePlan()
		return true
	case "/stop":
		sw.stopTurn()
		return true
	case "/clearqueue":
		if sw.pending == "" {
			sw.addNote("no queued message to clear")
		} else {
			sw.clearQueue("queued message cleared")
		}
		return true
	case "/goal":
		sw.handleGoalCommand(strings.TrimSpace(strings.TrimPrefix(text, fields[0])))
		return true
	case "/markdown":
		sw.handleMarkdownCommand(fields[1:])
		return true
	case "/thinking":
		sw.handleThinkingCommand(fields[1:])
		return true
	case "/watcher":
		sw.handleWatcherCommand(fields[1:])
		return true
	}
	// Not a built-in: a custom command (issue #403) may match. The built-in switch
	// is consulted first and returns above, so a custom command can never shadow a
	// built-in. An unresolved name falls through to false, sending the raw text to
	// the model unchanged (the prior behaviour).
	if sw.dispatchCustomCommand(fields[0], fields[1:]) {
		return true
	}
	return false
}

// dispatchCustomCommand resolves "/name" against the custom-command registry and,
// when found, expands its template with the invocation args and sends the result
// to the agent as a normal user message (issue #403). It returns true when it
// handled the command (sent or reported an error) and false when no custom
// command matched, so the caller can fall back to sending the raw text.
//
// A missing required parameter is reported as a transcript note and the command
// is NOT sent. The command's overrides are applied per invocation: model selects
// the turn's model, and a non-empty agent or subtask routes the prompt through a
// spawned sub-agent (via the OnSendCommand seam).
func (sw *SessionWindow) dispatchCustomCommand(slashName string, args []string) bool {
	if sw.wb == nil || sw.wb.handlers.GetCustomCommand == nil {
		return false
	}
	name := strings.TrimPrefix(slashName, "/")
	// Defence in depth: never let a custom command shadow a built-in, even if one
	// was hand-written into commands.json past the backend's create-time collision
	// check. The client-side built-ins are already handled by the switch above and
	// never reach here; this additionally protects the backend/ file-tool built-ins
	// (calc/echo/help, read/write/edit), which have no client-side handler. A
	// reserved name is treated as "not a custom command" so the raw text is sent
	// unchanged, exactly as before this feature existed. The reserved set comes from
	// the backend's single source of truth (with a local fallback).
	if sw.wb.reservedBuiltins()[name] {
		return false
	}
	def, err := sw.wb.handlers.GetCustomCommand(name)
	if err != nil {
		return false // not a custom command — let the caller send the raw text
	}
	expanded, err := expandTemplate(def, args)
	if err != nil {
		sw.echoCommand(slashName, "", err)
		return true
	}
	sw.sendCommandMessage(expanded, pendingCommand{model: def.Model, agent: def.Agent, subtask: def.Subtask})
	return true
}

// pendingCommand records the per-invocation overrides of a custom command so they
// survive being queued while the agent is busy (issue #403): model selects the
// turn's model, and agent/subtask route it through a spawned sub-agent. Carrying
// them with the queued text means a command invoked mid-turn re-applies every
// override when it drains, not just the model.
type pendingCommand struct {
	model   string
	agent   string
	subtask bool
}

// sendCommandMessage sends an already-expanded custom-command prompt as a normal
// user turn with the given overrides. While a turn is running it queues the
// message AND its overrides (so the drain re-applies them) rather than dropping
// the model override; otherwise it sends immediately.
func (sw *SessionWindow) sendCommandMessage(message string, ov pendingCommand) {
	if sw.wb == nil || strings.TrimSpace(message) == "" {
		return
	}
	if sw.busy {
		sw.enqueueCommand(message, ov)
		return
	}
	sw.sendCommandNow(message, ov)
}

// sendCommandNow performs the immediate send of an expanded command prompt,
// mirroring the non-busy submit path and applying the model override (empty = the
// session's current model). It is the single send site shared by the direct and
// drain-on-idle paths so the override is applied identically in both.
func (sw *SessionWindow) sendCommandNow(message string, ov pendingCommand) {
	sw.completer.hide()
	sw.addUser(message)
	sw.setBusy(true)
	sw.planPending = false
	modelName := sw.selectedModelName()
	if strings.TrimSpace(ov.model) != "" {
		modelName = ov.model
	}
	effort := sw.selectedEffort()
	// Prefer the override-aware seam so model/agent/subtask are all applied; fall
	// back to plain OnSend (model only) when the backend hasn't wired it.
	if sw.wb.handlers.OnSendCommand != nil {
		go sw.wb.handlers.OnSendCommand(sw.id, message, modelName, ov.agent, ov.subtask, effort)
		return
	}
	if sw.wb.handlers.OnSend != nil {
		go sw.wb.handlers.OnSend(sw.id, message, modelName, effort)
	}
}

// handleWatcherCommand implements /watcher (issue #329 Phase 4): a client-side
// control surface for the session's watchers mirroring the Watchers dialog
// buttons. Sub-commands:
//
//	/watcher list                 list the watchers visible to this session
//	/watcher enable  <name|id>    re-arm a watcher's schedule
//	/watcher disable <name|id>    stop future fires (a running fire finishes)
//	/watcher run     <name|id>    fire now, ignoring schedule/enabled state
//	/watcher stop    <name|id>    cancel the in-flight fire
//
// Each control dispatches to the matching workbench handler and echoes the
// outcome; an unwired handler is reported as unavailable rather than silently
// ignored.
func (sw *SessionWindow) handleWatcherCommand(args []string) {
	if sw.wb == nil {
		return
	}
	if len(args) == 0 {
		sw.addNote("usage: /watcher list|enable|disable|run|stop [name]")
		return
	}
	sub := strings.ToLower(args[0])
	if sub == "list" {
		if sw.wb.handlers.ListWatchers == nil {
			sw.addNote("watchers are unavailable")
			return
		}
		infos := sw.wb.handlers.ListWatchers(sw.id)
		if len(infos) == 0 {
			sw.echoCommand("/watcher list", "no watchers", nil)
			return
		}
		var b strings.Builder
		for i, info := range infos {
			if i > 0 {
				b.WriteString("\n")
			}
			target := "free"
			if !info.Free {
				target = info.TargetSession
			}
			status := info.Status
			if !info.Enabled {
				status += ", disabled"
			}
			fmt.Fprintf(&b, "• %s [%s] %s — %s (%s)", info.Name, target, info.Schedule, status, info.NextFire)
		}
		sw.echoCommand("/watcher list", b.String(), nil)
		return
	}

	// The remaining sub-commands act on a named watcher.
	if len(args) < 2 {
		sw.addNote("usage: /watcher " + sub + " <name|id>")
		return
	}
	name := strings.Join(args[1:], " ")
	var (
		fn    func(string) error
		label string
	)
	switch sub {
	case "enable":
		fn, label = sw.wb.handlers.EnableWatcher, "/watcher enable"
	case "disable":
		fn, label = sw.wb.handlers.DisableWatcher, "/watcher disable"
	case "run":
		fn, label = sw.wb.handlers.RunWatcher, "/watcher run"
	case "stop":
		fn, label = sw.wb.handlers.StopWatcher, "/watcher stop"
	default:
		sw.addNote("usage: /watcher list|enable|disable|run|stop [name]")
		return
	}
	if fn == nil {
		sw.addNote("watcher control is unavailable")
		return
	}
	sw.echoCommand(label, name, fn(name))
}

// handleThinkingCommand implements /thinking (issue #217): it toggles live
// streaming of the model's chain-of-thought into the transcript, or sets it
// explicitly with on/off. The change is applied to this session's backend via the
// StreamThinking handler and takes effect on the next turn.
func (sw *SessionWindow) handleThinkingCommand(args []string) {
	if sw.wb == nil || sw.wb.handlers.StreamThinking == nil {
		sw.addNote("streaming thinking is unavailable")
		return
	}
	var set *bool
	if len(args) > 0 {
		var v bool
		switch strings.ToLower(args[0]) {
		case "on", "true", "1":
			v = true
		case "off", "false", "0":
			v = false
		default:
			sw.addNote("usage: /thinking [on|off]")
			return
		}
		set = &v
	} else {
		// No argument: flip the current state.
		cur := sw.wb.handlers.StreamThinking(sw.id, nil)
		v := !cur
		set = &v
	}
	if sw.wb.handlers.StreamThinking(sw.id, set) {
		// Flag that turning it on changes failure semantics: the streaming path
		// cannot be replayed mid-stream, so a transient model error (e.g. a 429)
		// surfaces immediately instead of being retried with backoff (issue #217).
		sw.addNote("streaming thinking on (transient model errors are not retried while on)")
	} else {
		sw.addNote("streaming thinking off")
	}
}

// handleMarkdownCommand implements /markdown (issue #184): it toggles rich
// Markdown rendering of assistant answers, or sets it explicitly with on/off. The
// transcript is re-rendered so the change applies to existing answers too. When
// the terminal cannot show colour the feature is forced off regardless, which the
// note makes clear.
func (sw *SessionWindow) handleMarkdownCommand(args []string) {
	if len(args) > 0 {
		switch strings.ToLower(args[0]) {
		case "on", "true", "1":
			richMarkdown = true
		case "off", "false", "0":
			richMarkdown = false
		default:
			sw.addNote("usage: /markdown [on|off]")
			return
		}
	} else {
		richMarkdown = !richMarkdown
	}
	sw.transcript.render()
	switch {
	case !richMarkdownColorOK:
		sw.addNote("rich markdown unavailable (no colour); showing plain text")
	case richMarkdown:
		sw.addNote("rich markdown on")
	default:
		sw.addNote("rich markdown off (plain text)")
	}
}

// handleGoalCommand implements the /goal command (issue #172): the harness-level
// supervisor's per-session objective. With no argument it shows the current goal;
// "/goal clear" removes it; otherwise the argument becomes the new goal. Setting
// or clearing the goal resets the nudge budget and persists the layout so the
// objective survives a restart. The goal is purely informational unless the
// experimental supervisor is enabled — the note says so to keep it discoverable.
func (sw *SessionWindow) handleGoalCommand(arg string) {
	switch {
	case arg == "":
		if sw.goal == "" {
			sw.addNote("no goal set — use /goal <text> to set one for the supervisor")
		} else {
			sw.addNote("goal: " + sw.goal)
		}
		return
	case strings.EqualFold(arg, "clear"):
		if sw.goal == "" {
			sw.addNote("no goal to clear")
			return
		}
		sw.goal = ""
		sw.nudgeCount = 0
		sw.nudgeGaveUp = false
		sw.addNote("goal cleared")
	default:
		sw.goal = arg
		sw.nudgeCount = 0
		sw.nudgeGaveUp = false
		if sw.supervisorEnabled() {
			sw.addNote("goal set — the supervisor will nudge this session toward it when it goes idle: " + arg)
		} else {
			sw.addNote("goal set (supervisor disabled; enable experimental.supervisor to act on it): " + arg)
		}
	}
	sw.wb.persistLayout()
}

// supervisorEnabled reports whether the harness-level supervisor (issue #172) is
// enabled, defaulting to false when the handler is not wired.
func (sw *SessionWindow) supervisorEnabled() bool {
	if sw.wb.handlers.SupervisorEnabled == nil {
		return false
	}
	return sw.wb.handlers.SupervisorEnabled()
}

// supervisorMaxNudges returns the configured consecutive-nudge budget, falling
// back to defaultSupervisorMaxNudges when unwired or non-positive (issue #172).
func (sw *SessionWindow) supervisorMaxNudges() int {
	if sw.wb.handlers.SupervisorMaxNudges == nil {
		return defaultSupervisorMaxNudges
	}
	if n := sw.wb.handlers.SupervisorMaxNudges(); n > 0 {
		return n
	}
	return defaultSupervisorMaxNudges
}

// defaultSupervisorMaxNudges is the window-side fallback nudge budget used when
// the SupervisorMaxNudges handler is unwired or returns a non-positive value
// (issue #172). It mirrors config's defaultSupervisorMaxNudges.
const defaultSupervisorMaxNudges = 3

// maybeSupervise is the idle watchdog (issue #172), run on the busy→idle edge
// after the queue has drained. It is a no-op unless the supervisor is enabled,
// a goal is set, no message is queued/draining, and a nudge check is not already
// in flight. With budget remaining it launches the completion check on a
// background goroutine (a model judge must not block the UI thread) and applies
// the verdict back on the UI thread: a met goal stops nudging, an unmet goal
// fires a nudge turn (when budget remains) or surfaces a give-up note.
func (sw *SessionWindow) maybeSupervise() {
	if !sw.supervisorEnabled() || sw.goal == "" {
		return
	}
	// Don't fight a draining queue, a still-busy turn, or another in-flight check;
	// a queued real message also takes precedence over a nudge.
	if sw.busy || sw.draining || sw.supervisorBusy || sw.pending != "" {
		return
	}
	if sw.wb.handlers.OnSupervisorCheck == nil {
		return
	}
	if sw.nudgeCount >= sw.supervisorMaxNudges() {
		// Budget exhausted: stop nudging until a real user message resets it. Surface
		// the give-up note exactly once (the latch), not on every later idle edge.
		if !sw.nudgeGaveUp {
			sw.nudgeGaveUp = true
			sw.addNote(fmt.Sprintf("supervisor: goal still unmet after %d nudges — stopping", sw.supervisorMaxNudges()))
		}
		return
	}
	if sw.runSupervisorCheck == nil {
		return
	}
	sw.supervisorBusy = true
	sw.runSupervisorCheck(sw.goal)
}

// defaultSupervisorCheck is the production completion-check dispatcher (issue
// #172): it runs OnSupervisorCheck on a background goroutine — a model judge must
// not block the UI thread — and posts the verdict back onto the event loop so
// applySupervisorVerdict mutates window state on the UI thread, like every other
// session update. It is the default sw.runSupervisorCheck; tests substitute a
// synchronous variant because the post queue is not pumped under test.
func (sw *SessionWindow) defaultSupervisorCheck(goal string) {
	go func() {
		done, err := sw.wb.handlers.OnSupervisorCheck(sw.id, goal)
		sw.wb.desktop.Post(func() {
			sw.applySupervisorVerdict(goal, done, err)
		})
	}()
}

// applySupervisorVerdict consumes the completion check's result on the UI thread
// (issue #172). It clears the in-flight guard and, when the goal is still unmet,
// nudges or gives up against the budget. It re-validates state (the user may have
// sent, cleared the goal, or changed it while the async check ran) so a stale
// verdict cannot nudge a session the user has since taken over. An errored check
// is treated as "not done" but still consumes a nudge so a persistently failing
// check cannot loop unboundedly.
func (sw *SessionWindow) applySupervisorVerdict(goal string, done bool, err error) {
	sw.supervisorBusy = false
	// The world may have moved while the check ran: the goal was cleared/changed,
	// the user sent a new turn (busy), or a message was queued. Any of these means
	// this verdict is stale — drop it.
	if sw.goal != goal || sw.busy || sw.pending != "" {
		return
	}
	if done && err == nil {
		sw.addNote("supervisor: goal satisfied")
		sw.nudgeCount = 0
		return
	}
	// maybeSupervise gates the budget before dispatching the check, so a remaining
	// allowance is the normal case here; re-guard defensively in case the budget
	// shrank (config change) between dispatch and verdict.
	if sw.nudgeCount >= sw.supervisorMaxNudges() {
		return
	}
	sw.nudgeCount++
	sw.nudgeSession(goal)
}

// nudgeSession submits a supervisor nudge as a new user turn (issue #172). It
// reuses the exact send path queued input drains through (submitFn → the input
// submit handler), so the nudge gets the same treatment as any user message and
// starts a fresh agent loop — the watchdog fires on the busy→idle edge, after the
// previous loop has already ended, so a new turn (not a mid-loop injection) is the
// correct mechanism. nudgingSend marks the send so it does not reset the budget.
func (sw *SessionWindow) nudgeSession(goal string) {
	if sw.submitFn == nil || sw.busy {
		return
	}
	text := agent.SupervisorNudge(goal)
	sw.nudgingSend = true
	sw.input.SetText(text)
	sw.submitFn()
	sw.input.Clear()
	sw.nudgingSend = false
}

// stopTurn cancels the session's in-flight turn (issue #170) and discards any
// queued message, so a manual stop never silently auto-fires the queue. It is a
// no-op (with a note) when nothing is running and nothing is queued.
func (sw *SessionWindow) stopTurn() {
	if !sw.busy && sw.pending == "" {
		sw.addNote("nothing to stop")
		return
	}
	// Discard the queue first so the busy→idle transition the cancel triggers does
	// not drain it; surface the discard distinctly from an ordinary clear.
	sw.clearQueue("queued message cleared (agent stopped)")
	if sw.busy {
		if sw.wb.handlers.OnStop != nil {
			sw.addNote("stopping…")
			go sw.wb.handlers.OnStop(sw.id)
		} else {
			sw.addNote("stop unavailable")
		}
	}
}

// togglePlanMode flips the session's plan mode (issue #43). In plan mode the
// agent researches with read-only tools and proposes a plan instead of making
// changes; the change is mirrored to the backend and reflected in the status
// line. The note explains the new state so the mode is discoverable.
func (sw *SessionWindow) togglePlanMode() {
	sw.planMode = !sw.planMode
	sw.planPending = false // either direction supersedes any stale pending plan
	if sw.wb.handlers.OnSetPlanMode != nil {
		sw.wb.handlers.OnSetPlanMode(sw.id, sw.planMode)
	}
	if sw.planMode {
		sw.addNote("Plan mode on — the agent will research and propose a plan without making changes. " +
			"Send your request, then approve the plan (/act) to execute it.")
	} else {
		sw.addNote("Plan mode off — the agent may make changes directly.")
	}
	sw.refreshStatus()
}

// toggleYoloMode requests a yolo-mode flip for the session (issue #356). It is
// request-only: it asks the backend to toggle and does NOT flip the local display
// field. The backend applies the change (auto-approve permissions + remove the
// step cap) and emits a SessionEventYolo carrying the new state, which apply()
// renders — so the indicator is backend-owned and correct regardless of how yolo
// was activated (toggle, config, or --yolo), unlike plan mode's local mirror.
func (sw *SessionWindow) toggleYoloMode() {
	if sw.wb.handlers.OnSetYoloMode == nil {
		sw.addNote("YOLO mode unavailable")
		return
	}
	sw.wb.handlers.OnSetYoloMode(sw.id, !sw.yoloMode)
}

// applyYoloMode renders a backend-announced yolo state (issue #356). It sets the
// display-only field, refreshes the status line, and explains the new state on a
// real change. The change guard keeps the silent default (yolo off) quiet at
// session creation while still announcing config/CLI-activated yolo and toggles.
func (sw *SessionWindow) applyYoloMode(on bool) {
	if sw.yoloMode == on {
		return
	}
	sw.yoloMode = on
	if on {
		sw.addNote("YOLO mode on — permission prompts auto-approve (except hard-deny guardrails) " +
			"and the step cap is removed. Cancellation, token budget, and the audit trail still apply; " +
			"set a token budget as the brake.")
	} else {
		sw.addNote("YOLO mode off — permission prompts return and the configured step cap is restored.")
	}
	sw.refreshStatus()
}

// approvePlan executes the session's pending plan (issue #43). It refuses (with a
// note) when no plan is awaiting approval; otherwise it hands the turn to the
// backend approver, which re-runs the plan with the full tool set.
func (sw *SessionWindow) approvePlan() {
	if !sw.planPending {
		sw.addNote("no plan to approve — use /plan to plan a task first")
		return
	}
	sw.startApprovedTurn()
}

// startApprovedTurn marks the window busy for an approved plan's executing turn
// and dispatches it to the backend approver (issue #43). The plan-pending flag
// clears and the turn's terminal events (final/error) clear the busy state.
func (sw *SessionWindow) startApprovedTurn() {
	sw.planPending = false
	sw.planMode = false
	sw.setBusy(true)
	if sw.wb.handlers.OnApprovePlan != nil {
		go sw.wb.handlers.OnApprovePlan(sw.id)
	} else {
		sw.addNote("plan approval unavailable")
		sw.setBusy(false)
	}
}

// callUndo invokes the backend undo/rewind handler. rewind selects /rewind (the
// last turns turns, 0 = all) over /undo (the single last turn).
func (sw *SessionWindow) callUndo(rewind bool, turns int) (string, error) {
	if rewind {
		if sw.wb.handlers.OnRewind != nil {
			return sw.wb.handlers.OnRewind(sw.id, turns)
		}
	} else if sw.wb.handlers.OnUndo != nil {
		return sw.wb.handlers.OnUndo(sw.id)
	}
	return "", fmt.Errorf("undo/rewind not available")
}

// echoCommand surfaces a command's outcome as a transcript note.
func (sw *SessionWindow) echoCommand(cmd, summary string, err error) {
	if err != nil {
		sw.addNote(fmt.Sprintf("%s failed: %v", cmd, err))
		return
	}
	if summary == "" {
		summary = "done"
	}
	sw.addNote(fmt.Sprintf("%s — %s", cmd, summary))
}

// addAssistant appends the assistant's final answer (expanded, not folded).
// assistantRecord builds the "Gogent:" answer record, or nil when the text is
// blank. The record is rich so its body renders as styled Markdown children
// (issue #519 shares this builder between the live add path and batched restore).
func assistantRecord(text string) *transcriptRecord {
	if strings.TrimSpace(text) == "" {
		return nil
	}
	return &transcriptRecord{
		kind: kindAssistant, header: "Gogent:", color: colorAgent, role: roleAgent,
		lines: styledChildLines(text, roleAgent),
		rich:  true,
	}
}

func (sw *SessionWindow) addAssistant(text string) {
	r := assistantRecord(text)
	if r == nil {
		return
	}
	// The final answer is what the user asked for, so re-anchor the transcript on it
	// (addAndReveal) even when the user had scrolled up to read earlier output
	// mid-turn. Without the re-anchor the answer is appended (it is in AllText, the
	// session file and the model) but never scrolled into view, which read as the
	// agent silently dropping its reply (issue #227). Streaming events (thoughts,
	// tool calls/results) intentionally do not re-anchor, so reading scrolled-up
	// history during a turn is undisturbed until the answer lands.
	sw.transcript.addAndReveal(r)
}

// thoughtRecord builds the collapsed-by-default "thought" record for a model's
// retained chain-of-thought, or nil when the text is blank (issue #519).
func thoughtRecord(text string) *transcriptRecord {
	if strings.TrimSpace(text) == "" {
		return nil
	}
	return &transcriptRecord{
		kind: kindThinking, header: "thought", color: colorNote, role: roleNote, collapsed: true,
		lines: styledChildLines(text, roleNote),
	}
}

// addThought appends a collapsed-by-default "thought" entry.
func (sw *SessionWindow) addThought(text string) {
	if r := thoughtRecord(text); r != nil {
		sw.transcript.add(r)
	}
}

// appendThinkingDelta streams a chunk of the model's chain-of-thought into a
// live, expanded "thinking…" entry under the current turn (issue #217). The
// entry is created lazily on the first non-empty delta so a turn that streams no
// reasoning never shows one. Deltas are token fragments, so they are line
// buffered: only complete lines (terminated by a newline) are committed to the
// entry as they arrive; the trailing partial is held until the entry folds (see
// foldLiveThought). The entry starts expanded so the user watches the thinking
// build up.
func (sw *SessionWindow) appendThinkingDelta(delta string) {
	if delta == "" {
		return
	}
	if sw.liveThought == nil {
		sw.liveThought = sw.transcript.add(&transcriptRecord{
			kind: kindThinking, header: "thinking…", color: colorNote, role: roleNote,
		})
		sw.liveThoughtBuf = ""
	}
	sw.liveThoughtBuf += delta
	for {
		i := strings.IndexByte(sw.liveThoughtBuf, '\n')
		if i < 0 {
			break
		}
		line := sw.liveThoughtBuf[:i]
		sw.liveThoughtBuf = sw.liveThoughtBuf[i+1:]
		sw.transcript.appendLine(sw.liveThought, styledLine{text: line, color: roleColor(roleNote), role: roleNote})
	}
}

// foldLiveThought finishes the live streamed thinking entry: it flushes any
// trailing partial line, relabels the header from "thinking…" to "thought", and
// collapses the entry so the completed reasoning folds away (issue #217). It is a
// no-op when no thinking is streaming, so it is safe to call on the
// thinking-done event and again on the busy→idle safety net.
func (sw *SessionWindow) foldLiveThought() {
	if sw.liveThought == nil {
		return
	}
	if sw.liveThoughtBuf != "" {
		sw.transcript.appendLine(sw.liveThought, styledLine{text: sw.liveThoughtBuf, color: roleColor(roleNote), role: roleNote})
		sw.liveThoughtBuf = ""
	}
	sw.transcript.setHeader(sw.liveThought, "thought")
	sw.transcript.setCollapsed(sw.liveThought, true)
	sw.liveThought = nil
}

// addCompaction appends a collapsed note recording a context-compression pass;
// the structured summary is folded inside.
func (sw *SessionWindow) addCompaction(estTokens int, digest string) {
	sw.transcript.add(&transcriptRecord{
		kind:      kindCompaction,
		header:    fmt.Sprintf("context compacted (~%d tokens)", estTokens),
		color:     colorNote,
		role:      roleNote,
		collapsed: true,
		lines:     styledChildLines(digest, roleNote),
	})
}

// beginToolCall creates a collapsed entry for a tool call, holding its args. The
// entry is tracked under the call's stable id so its result can flip this exact
// entry to "done" later — even when several calls run concurrently and their
// results arrive out of order (issue #187).
func (sw *SessionWindow) beginToolCall(id, name string, args map[string]interface{}) {
	lines := []styledLine{{text: "args:", color: colorTool, role: roleTool}}
	for _, line := range formatArgs(args) {
		lines = append(lines, styledLine{text: "  " + line, color: colorTool, role: roleTool})
	}
	rec := sw.transcript.add(&transcriptRecord{
		kind: kindTool, header: fmt.Sprintf("tool: %s (running...)", name),
		color: colorTool, role: roleTool, collapsed: true, lines: lines,
	})
	if id == "" {
		// No stable id (legacy/stray event): track under a unique synthetic key so
		// the busy→idle safety net can still sweep this "(running...)" entry. It
		// just cannot be paired to a result by id, so its result (if any) falls back
		// to a fresh entry in finishToolCall.
		sw.untrackedTools++
		sw.pendingTools[fmt.Sprintf("\x00untracked-%d", sw.untrackedTools)] = rec
		return
	}
	// A misbehaving model can reuse a tool-call id within a turn. The displaced
	// entry can no longer be matched to a result, so flip it terminal now instead
	// of orphaning it "(running...)" beyond the safety net's reach (issue #187).
	if old := sw.pendingTools[id]; old != nil {
		sw.transcript.setHeader(old, fmt.Sprintf("tool: %s (superseded)", toolHeaderName(old)))
	}
	sw.pendingTools[id] = rec
}

// finishToolCall appends the result to the call's pending entry, matched by its
// stable id, and flips it from "running" to "done". An unknown or empty id (a
// result with no recorded call — e.g. a legacy event) falls back to a fresh
// entry so the result is never dropped (issue #187).
func (sw *SessionWindow) finishToolCall(id, name, result string) {
	rec := sw.pendingTools[id]
	if id != "" && rec != nil {
		delete(sw.pendingTools, id)
		sw.transcript.setHeader(rec, fmt.Sprintf("tool: %s (done)", name))
	} else {
		rec = sw.transcript.add(&transcriptRecord{
			kind: kindTool, header: fmt.Sprintf("tool: %s", name), color: colorTool, role: roleTool, collapsed: true,
		})
	}
	sw.transcript.appendLine(rec, styledLine{text: "result:", color: colorResult, role: roleResult})
	for _, line := range childLines(result) {
		sw.transcript.appendLine(rec, styledLine{text: "  " + line, color: colorResult, role: roleResult})
	}
	sw.transcript.setCollapsed(rec, true)
}

// failPendingTools flips every still-running tool entry to a terminal state with
// the given suffix (e.g. "interrupted"). It is the UI safety net for issue #187:
// run on the busy→idle edge, it guarantees no tool entry is left showing
// "(running...)" if its result event never arrived — a cancelled or early-broken
// loop, a backend crash, or any path that skips a result. On a clean turn the
// map is already empty, so this is a no-op.
func (sw *SessionWindow) failPendingTools(state string) {
	for id, rec := range sw.pendingTools {
		sw.transcript.setHeader(rec, fmt.Sprintf("tool: %s (%s)", toolHeaderName(rec), state))
		delete(sw.pendingTools, id)
	}
}

// toolHeaderName recovers a tool entry's name from its "tool: NAME (running...)"
// header so failPendingTools can rebuild the header with a terminal suffix
// without threading the name separately.
func toolHeaderName(rec *transcriptRecord) string {
	name := strings.TrimPrefix(rec.header, "tool: ")
	if i := strings.LastIndex(name, " ("); i >= 0 {
		name = name[:i]
	}
	return name
}

// addError appends a red error line. It is raised on the turn-ending error event,
// so it re-anchors the transcript on the message the same way addAssistant does
// (issue #227): a user who scrolled up mid-turn must still see why the turn ended.
// An empty/whitespace message is skipped (mirroring addAssistant) so a degenerate
// error never appends a bare "error:" header or perturbs the scroll position; the
// failure is still surfaced via maybeNotify, which fires independently of this.
func (sw *SessionWindow) addError(text string) {
	if strings.TrimSpace(text) == "" {
		return
	}
	lines := make([]styledLine, 0)
	for _, line := range childLines(text) {
		lines = append(lines, styledLine{text: "  " + line, color: colorError, role: roleError})
	}
	sw.transcript.addAndReveal(&transcriptRecord{
		kind: kindError, header: "error:", color: colorError, role: roleError, lines: lines,
	})
}

// registerTranscriptBindings registers the less/vim-style transcript controls as
// Focus-scope bindings on the desktop's BindingRegistry (issue #269, phase 4a): '/'
// to search, single letters to toggle event-type filters, 'f'/'u' to fold/unfold
// all, 'y' to yank the last answer, and Esc to clear an active filter/search. Each
// binding's Target is this window's transcript view, so the toolkit fires it only
// while that transcript holds focus and only after the view itself declined the key
// (its scroll keys keep priority) — exactly the old handleTranscriptKey scope, now
// routed through the registry rather than an OnTypeFn switch. The handlers invoke the
// same actions as before.
//
// One accepted behaviour change: the old switch matched the rune case-exactly
// ('a' only), whereas the registry's Chord.Matches is case-insensitive, so a capital
// (e.g. Shift+a, which most terminals deliver as rune 'A' with no Shift bit) now fires
// the same action where it used to be inert. Case sensitivity is a property of the
// toolkit's matcher, not something gogent can override while dispatching through the
// registry; restoring case-exact matching is a turbotui concern (out of phase-4a scope).
//
// Note: the registry has no Unregister, so a closed window leaves its bindings
// behind. They are inert — their Target is no longer in any focus chain, so they
// never match — and cleanup is deferred to a later phase.
func (sw *SessionWindow) registerTranscriptBindings() {
	reg := sw.wb.desktop.ScopedBindings()
	target := sw.history.Component
	// The chord comes from chordFor (override-or-catalog-default, issue #269) so a
	// persisted rebind is applied the moment this window registers; with no override
	// each is its catalog default (the historical letter/Esc/'/'). The actionID and
	// scope must match the actions() catalog so the customizer can find, rebind
	// and reset each one.
	focus := func(id tv.ActionID, handler func() bool) {
		chord := sw.wb.chordFor(id)
		if chord == unboundChord {
			return // the user cleared this binding (issue #269)
		}
		reg.Register(tv.KeyBinding{Chord: chord, ActionID: id, Scope: tv.ScopeFocus, Target: target}, handler)
	}
	// Esc clears an active filter/search; when nothing is filtered it declines (returns
	// false) so the key keeps falling through the dispatch chain exactly as before.
	focus(actionTranscriptShowAll, func() bool {
		if sw.transcript.filtering() {
			sw.transcript.showAll()
			return true
		}
		return false
	})
	focus(actionTranscriptFind, func() bool { sw.promptFind(); return true })
	focus(actionTranscriptToggleMsg, func() bool { sw.transcript.toggleKind(kindAssistant); return true })
	focus(actionTranscriptToggleTool, func() bool { sw.transcript.toggleKind(kindTool); return true })
	focus(actionTranscriptToggleThink, func() bool { sw.transcript.toggleKind(kindThinking); return true })
	focus(actionTranscriptToggleErr, func() bool { sw.transcript.toggleKind(kindError); return true })
	focus(actionTranscriptFoldAll, func() bool { sw.transcript.setFold(true); return true })
	focus(actionTranscriptUnfoldAll, func() bool { sw.transcript.setFold(false); return true })
	focus(actionTranscriptCopyAnswer, func() bool { sw.copyLastAnswer(); return true })
	// "Copy last code block" (issue #463): rebindable but unbound by default, so focus()
	// registers nothing until the user assigns a chord — it then yanks the fenced code
	// from the last answer, paralleling the 'y' copy-answer binding above.
	focus(actionTranscriptCopyCode, func() bool { sw.copyLastCode(); return true })
}

// promptFind opens the search prompt and applies the entered query as a
// find-in-transcript filter (an empty query clears the search).
func (sw *SessionWindow) promptFind() {
	sw.wb.showInputDialog("Find in Transcript", "&Search:", sw.transcript.query, func(value string, ok bool) {
		if !ok {
			return
		}
		sw.transcript.setQuery(value)
		sw.wb.desktop.Redraw()
	})
}

// copyLastAnswer yanks the most recent assistant answer to the system clipboard
// (issue #62). It is also bound to the 'y' key while the transcript is focused
// (vim-style yank). A transcript note confirms the copy, or reports when there is
// nothing yet to copy.
func (sw *SessionWindow) copyLastAnswer() {
	text := sw.transcript.lastAssistantRecord().body()
	if strings.TrimSpace(text) == "" {
		sw.addNote("no answer to copy yet")
		return
	}
	sw.wb.copyToClipboard(text)
	sw.addNote(fmt.Sprintf("copied last answer (%d chars) to clipboard", utf8.RuneCountInString(text)))
}

// copyLastCode yanks the fenced code blocks from the most recent assistant answer
// to the system clipboard (issue #62). When the answer has no fenced code it
// reports that rather than copying the prose.
func (sw *SessionWindow) copyLastCode() {
	code := extractFencedCode(sw.transcript.lastAssistantRecord().body())
	if strings.TrimSpace(code) == "" {
		sw.addNote("no code block in last answer")
		return
	}
	sw.wb.copyToClipboard(code)
	sw.addNote(fmt.Sprintf("copied code (%d chars) to clipboard", utf8.RuneCountInString(code)))
}

// reload replaces the window's transcript with msgs, discarding the current
// records first. It backs the jump-to-present refresh after a daemon reconnect
// (issue #358 §7): the frozen last-known transcript is swapped for the daemon's
// current state in one shot, rather than replaying missed events. It must run on
// the UI thread.
//
// It clears the records, then defers entirely to restore(), which builds the new
// records and composes the view exactly once via addAll. Clearing the slice before
// restore means addAll appends onto a fresh slice, so the single render() rebuilds
// the whole view — exactly one compose, no intermediate clear-render (issue #519).
func (sw *SessionWindow) reload(msgs []ChatMessage) {
	sw.transcript.records = nil
	sw.restore(msgs)
}

// restore replays a saved transcript into the model so a re-opened session is
// searchable and filterable like a live one. It mirrors renderTranscript's
// role-to-entry mapping.
//
// It builds every record up front and appends them in one batch via addAll, which
// composes the view a single time rather than the per-record renderOne() a
// per-message add() loop would do. First-connect restore of a large session is
// then an O(1) compose instead of O(M) UI-thread appends (issue #519); the final
// view — record set, order, fold state, Markdown and scroll position — is
// unchanged. Blank-text records are dropped by their builders (returning nil) and
// skipped by addAll, so the record slice never holds a nil.
func (sw *SessionWindow) restore(msgs []ChatMessage) {
	records := make([]*transcriptRecord, 0, len(msgs))
	for _, m := range msgs {
		switch strings.ToLower(m.Role) {
		case "user":
			// userRecord returns nil for blank content, folding the old explicit
			// blank-skip guard into the builder.
			if r := userRecord(m.Content); r != nil {
				records = append(records, r)
			}
		case "assistant":
			// A restored reasoning-only or partial turn renders its retained
			// chain-of-thought as the same collapsed "thought" entry the live
			// appendThinkingDelta/foldLiveThought path produces, ahead of the answer
			// (issue #402).
			if r := thoughtRecord(m.Reasoning); r != nil {
				records = append(records, r)
			}
			if r := assistantRecord(m.Content); r != nil {
				records = append(records, r)
			}
			if m.Tool != "" {
				lines := make([]styledLine, 0)
				for _, line := range childLines(m.Args) {
					lines = append(lines, styledLine{text: "  " + line, color: colorTool, role: roleTool})
				}
				records = append(records, &transcriptRecord{
					kind: kindTool, header: fmt.Sprintf("tool: %s", m.Tool),
					color: colorTool, role: roleTool, collapsed: true, lines: lines,
				})
			}
		case "tool":
			lines := make([]styledLine, 0)
			for _, line := range childLines(m.Content) {
				lines = append(lines, styledLine{text: "  " + line, color: colorResult, role: roleResult})
			}
			records = append(records, &transcriptRecord{
				kind: kindTool, header: fmt.Sprintf("result: %s", m.Tool),
				color: colorResult, role: roleResult, collapsed: true, lines: lines,
			})
		default: // system / other
			if strings.TrimSpace(m.Content) != "" {
				records = append(records, &transcriptRecord{
					kind: kindSystem, header: "[System]", color: colorInfo, role: roleInfo, collapsed: true,
					lines: styledChildLines(m.Content, roleInfo),
				})
			}
		}
	}
	sw.transcript.addAll(records)
}

// formatArgs renders tool arguments as readable key/value lines.
func formatArgs(args map[string]interface{}) []string {
	if len(args) == 0 {
		return []string{"(none)"}
	}
	keys := make([]string, 0, len(args))
	for k := range args {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		val := args[k]
		var rendered string
		switch v := val.(type) {
		case string:
			rendered = v
		default:
			if b, err := json.Marshal(v); err == nil {
				rendered = string(b)
			} else {
				rendered = fmt.Sprintf("%v", v)
			}
		}
		for i, line := range childLines(rendered) {
			if i == 0 {
				out = append(out, fmt.Sprintf("%s: %s", k, line))
			} else {
				out = append(out, "  "+line)
			}
		}
	}
	return out
}

// statusSep joins the state and each stat segment in the status line.
const statusSep = " · "

// gaugeCells is the width of the context-usage bar in display cells (e.g.
// "▰▰▰▱▱▱").
const gaugeCells = 6

// Context-usage thresholds for the status gauge's colour, expressed as
// percentages of the configured context window. contextCriticalPct intentionally
// matches the model session's compaction high-water mark (80%): the gauge turns
// red exactly when a compaction pass is about to fire, giving a visual warning
// just before context is compressed (issues #4 / #63).
const (
	contextWarnPct     = 60 // amber: approaching the compaction threshold
	contextCriticalPct = 80 // red: at/over the compaction threshold
)

// liveStats carries the transient, generation-time figures shown only while a
// turn is in flight: the elapsed time since the turn started and the output-token
// throughput. A zero liveStats renders no live segments.
type liveStats struct {
	elapsed      time.Duration
	tokensPerSec float64
}

// budgetLevel summarises how close a session's cumulative token usage is to its
// configured budget, for colouring and alerting the status line.
type budgetLevel int

const (
	budgetOK          budgetLevel = iota
	budgetApproaching             // >= the warn fraction of the budget
	budgetExceeded                // >= the full budget
)

// formatStatusLine composes the bottom status line for a session window:
//
//	‹state› · [budget!] · ‹elapsed› · ‹N t/s› · <in>/<out> tok · <n> turns · ctx ▰▰▱▱▱▱ <pct>%
//
// The state sits on the left with the compact stats following it. Segments with
// no meaningful value yet (no turn in flight, zero tokens/turns, or an unknown
// context window) are omitted, so a fresh, idle session shows just its state. On
// narrow windows the right-most stat segments are dropped first — the state (the
// most important signal) always stays visible — and as a last resort the state
// itself is truncated to width. Width is measured in display cells; the
// separator is a single-cell middle dot.
func formatStatusLine(state string, stats agent.SessionStats, live liveStats, budget config.BudgetConfig, width int) string {
	state = strings.TrimSpace(state)
	if width <= 0 {
		return state
	}
	line := state
	if tui.StringWidth(line) > width {
		// Truncate by display width (not rune count) to match the StringWidth
		// budget above, so a wide-glyph state can't overflow the line (issue #299).
		return truncateToWidth([]rune(line), width)
	}
	for _, seg := range statusSegments(stats, live, budget) {
		add := statusSep + seg
		if tui.StringWidth(line)+tui.StringWidth(add) <= width {
			line += add
		} else {
			break
		}
	}
	return line
}

// statusSegments renders the compact stat segments in display order: a budget
// breach marker and the live elapsed/throughput first (so they survive longest
// when the line is narrowed), then the cumulative token/turn/context figures.
// Zero-value segments are omitted.
func statusSegments(stats agent.SessionStats, live liveStats, budget config.BudgetConfig) []string {
	var segs []string
	if budgetStatus(stats, budget) == budgetExceeded {
		segs = append(segs, "budget!")
	}
	if d := formatDuration(live.elapsed); d != "" {
		segs = append(segs, d)
		if tps := formatTokensPerSec(live.tokensPerSec); tps != "" {
			segs = append(segs, tps)
		}
	}
	if stats.TokensIn > 0 || stats.TokensOut > 0 {
		segs = append(segs, formatTokens(stats.TokensIn)+"/"+formatTokens(stats.TokensOut)+" tok")
	}
	if stats.Turns > 0 {
		segs = append(segs, fmt.Sprintf("%d turns", stats.Turns))
	}
	if stats.ContextWindow > 0 {
		segs = append(segs, contextSegment(stats))
	}
	return segs
}

// contextSegment renders the context-usage segment as a bar plus percentage,
// e.g. "ctx ▰▰▱▱▱▱ 38%".
func contextSegment(stats agent.SessionStats) string {
	return fmt.Sprintf("ctx %s %d%%",
		contextGauge(stats.ContextTokens, stats.ContextWindow),
		contextPercent(stats.ContextTokens, stats.ContextWindow))
}

// contextGauge renders the context-usage bar (filled "▰" then empty "▱") scaled
// to gaugeCells. Usage at or over the window fills every cell; any nonzero usage
// fills at least one cell (so a little usage is visible rather than reading as
// empty); an unknown window (<=0) or zero usage yields an all-empty bar.
func contextGauge(tokens, window int) string {
	filled := 0
	if window > 0 && tokens > 0 {
		filled = (tokens*gaugeCells + window/2) / window // rounded to nearest cell
		if filled > gaugeCells {
			filled = gaugeCells
		}
		if filled < 0 {
			filled = 0
		}
		if filled == 0 { // nonzero usage should show at least one cell
			filled = 1
		}
	}
	return strings.Repeat("▰", filled) + strings.Repeat("▱", gaugeCells-filled)
}

// statusColor picks the status line's foreground colour. Severity wins over the
// idle/working state: a budget breach or context at the compaction threshold
// turns the whole line red, an approaching budget or context turns it amber, and
// otherwise it follows the state (dim grey when idle, blue when working). The
// colour is the at-a-glance warning the issue asks for — it stays even when a
// narrow line drops the text that explains it.
func statusColor(idle bool, stats agent.SessionStats, budget config.BudgetConfig) tui.Color {
	switch budgetStatus(stats, budget) {
	case budgetExceeded:
		return colorError
	case budgetApproaching:
		return colorTool
	}
	pct := contextPercent(stats.ContextTokens, stats.ContextWindow)
	if pct >= contextCriticalPct {
		return colorError
	}
	if pct >= contextWarnPct {
		return colorTool
	}
	if idle {
		return colorNote
	}
	// Running: colorInfo was bright blue (ANSI 12), too low-contrast on the blue
	// window to read (issue #193); it is now cyan (issue #202), but colorAgent stays
	// the right choice here. colorAgent (bright green / Okabe green in high-contrast)
	// reads well on both the default blue and the high-contrast black backgrounds,
	// and signals an active turn without colliding with the amber/red severity
	// colours above.
	return colorAgent
}

// statusColorFor folds the third "working in background" state (issue #353) into
// statusColor. It defers to statusColor for the foreground/idle/severity colours,
// then — and only when the plain idle colour would otherwise show (no budget/context
// severity in play) — substitutes the background role colour so a session running
// only background sub-agents reads distinctly from both idle and a live turn. The
// colour is colorInfo, a theme role (t.Info) recoloured on a live theme switch like
// the rest of the chrome (issue #379) — never a hardcoded value.
func statusColorFor(busy, background bool, stats agent.SessionStats, budget config.BudgetConfig) tui.Color {
	c := statusColor(!busy, stats, budget)
	if background && !busy && c == colorNote {
		return colorInfo
	}
	return c
}

// budgetStatus classifies a session's cumulative token usage against the
// configured budget. A disabled budget (TokenBudget <= 0) always reports OK.
func budgetStatus(stats agent.SessionStats, budget config.BudgetConfig) budgetLevel {
	if budget.TokenBudget <= 0 {
		return budgetOK
	}
	used := stats.TokensIn + stats.TokensOut
	if used >= budget.TokenBudget {
		return budgetExceeded
	}
	if float64(used) >= budget.WarnFractionOrDefault()*float64(budget.TokenBudget) {
		return budgetApproaching
	}
	return budgetOK
}

// formatDuration renders an elapsed duration compactly for the status line:
// under a minute as "Ns", a minute or more as "MmSSs" (e.g. "1m03s"). A
// non-positive duration yields "" so callers can omit it.
func formatDuration(d time.Duration) string {
	if d <= 0 {
		return ""
	}
	secs := int(d.Seconds())
	if secs < 60 {
		return fmt.Sprintf("%ds", secs)
	}
	return fmt.Sprintf("%dm%02ds", secs/60, secs%60)
}

// formatTokensPerSec renders an output-throughput figure as "N t/s" (rounded to
// a whole token). Throughput under one token per second renders "<1 t/s"; a
// non-positive figure yields "" so callers can omit it.
func formatTokensPerSec(tps float64) string {
	if tps <= 0 {
		return ""
	}
	n := int(tps + 0.5)
	if n < 1 {
		return "<1 t/s"
	}
	return fmt.Sprintf("%d t/s", n)
}

// formatTokens renders a token count compactly with k/M suffixes (e.g. 12300 ->
// "12.3k"). Values under a thousand are shown verbatim.
func formatTokens(n int) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1_000_000)
	case n >= 1_000:
		return fmt.Sprintf("%.1fk", float64(n)/1_000)
	default:
		return fmt.Sprintf("%d", n)
	}
}

// contextPercent returns context usage as a whole percentage of the window,
// clamped to [0, 100]. A non-positive window yields 0.
func contextPercent(tokens, window int) int {
	if window <= 0 {
		return 0
	}
	if p := tokens * 100 / window; p < 100 {
		return p
	}
	return 100
}

// headerSelectWidth sizes the window-header model dropdown (issue #108). It grows
// to fit the longest model name plus two cells for the Select's value padding and
// ▼ glyph, never shrinking below a sensible minimum, and is clamped to the room
// available in the window (leaving space for the "Model" label and a small right
// margin). The caller's window-width guard guarantees windowWidth >= 4, so the
// clamp keeps the control at least one cell wide.
func headerSelectWidth(longestName, windowWidth int) int {
	const minW = 24
	w := minW
	if want := longestName + 2; want > w {
		w = want
	}
	if max := windowWidth - 9; w > max {
		w = max
	}
	if w < 1 {
		w = 1
	}
	return w
}

// maximizeGlyph / restoreGlyph are the title-bar maximize button's two states:
// [□] invites expanding the window to the available area, [▣] marks it expanded
// and invites restoring the previous bounds. They pair with the minimize
// button's [▾]/[▴] and the close button's [■] (single-cell geometric shapes, like
// the rest of the chrome).
const (
	maximizeGlyph = "[□]"
	restoreGlyph  = "[▣]"
)

// menuBarHeight is the row the desktop menu bar occupies at the top of the
// screen; a maximized window starts below it.
const menuBarHeight = 1

// maximizedWindowRect returns the bounds a session window expands to when
// maximized: the whole available width (the caller's window area — already
// reduced by a pinned sidebar, issue #106) below the menu bar. Passing the window
// area keeps a maximized window left of the sidebar when pinned and lets it cover
// the full desktop when unpinned. Dimensions are floored at 1 so the rect is never
// empty.
func maximizedWindowRect(availW, screenH int) tv.Rect {
	width := availW
	if width < 1 {
		width = 1
	}
	height := screenH - menuBarHeight
	if height < 1 {
		height = 1
	}
	return tv.Rect{X: 0, Y: menuBarHeight, W: width, H: height}
}

// maximizeButtonRect is the 3-cell maximize/restore button's hit/draw region in
// the title bar. It is placed one slot (4 cells: 3 for the glyph + 1 gap) left of
// the rightmost title-bar button, mirroring the minimize/close layout in
// tv.Window so the bar reads …[□][▾][■] when all three are shown. showClose and
// minimizable reflect which of the buttons to the right are present.
func maximizeButtonRect(abs tv.Rect, showClose, minimizable bool) tv.Rect {
	x := abs.Right() - 5 // rightmost slot (close, or maximize when alone)
	switch {
	case showClose && minimizable:
		x = abs.Right() - 13 // left of close + minimize
	case showClose:
		x = abs.Right() - 9 // left of close
	case minimizable:
		x = abs.Right() - 9 // left of minimize
	}
	return tv.Rect{X: x, Y: abs.Y, W: 3, H: 1}
}

// truncateRunes returns the first max runes of s, for the rare very-narrow
// window where even the state does not fit.
func truncateRunes(s string, max int) string {
	if max <= 0 {
		return ""
	}
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	return string([]rune(s)[:max])
}
