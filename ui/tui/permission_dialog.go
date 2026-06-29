package ui

import (
	"fmt"
	"strings"
	"time"

	"gogent/internal/notify"
	"gogent/internal/permission"

	tui "github.com/hobbestherat/turbotui"
	tv "github.com/hobbestherat/turbotui/turbotv"
)

// typingIdleThreshold is how long the focused session input must be free of
// keystrokes before a deferred background-triggered modal (permission/review) is
// presented (issue #346). It is the `within` passed to Desktop.RecentlyTyped:
// while the user typed more recently than this, presentation is held back so a
// freshly-focused dialog button cannot intercept a keystroke (e.g. an Enter the
// user meant to submit a message with).
const typingIdleThreshold = 600 * time.Millisecond

// deferredRecheckInterval is how often a held-back modal re-checks whether the
// user has gone idle. It is shorter than typingIdleThreshold so the modal appears
// promptly once typing stops (or the user submits — Enter does not count as
// typing, so RecentlyTyped decays through it) rather than only on the next full
// idle window.
const deferredRecheckInterval = 150 * time.Millisecond

// presentBackgroundModal shows a background-triggered modal immediately when the
// user is idle, or defers it until they stop typing (issue #346). show performs
// the actual AddLayer+SetFocus (e.g. showPermissionDialog/showReviewDialog). It
// must run on the desktop event loop (callers post it). Only the *visual*
// presentation is deferred — the agent goroutine still blocks on serializePrompt,
// and the sidebar badge (markApproval) plus the notification already fired, so a
// deferred prompt is still signalled and nothing is dropped.
func (w *Workbench) presentBackgroundModal(show func()) {
	w.deferredModal = show
	w.maybeShowDeferredModal()
}

// maybeShowDeferredModal presents the pending deferred modal if the user has
// stopped typing, otherwise arms a short re-check timer and tries again. It must
// run on the event loop (the timer re-enters it via Desktop.Post). With no
// desktop (tests) or no recent typing the modal shows at once; promptMu ensures
// only one presentation is ever pending.
func (w *Workbench) maybeShowDeferredModal() {
	if w.deferredModal == nil {
		return
	}
	if w.desktop != nil && w.desktop.RecentlyTyped(typingIdleThreshold) {
		w.armDeferredRecheck()
		return
	}
	w.drainDeferredModalNow()
}

// drainDeferredModalNow presents any pending background-triggered modal at once,
// bypassing the typing-idle check. It is the "or submits/clears their input —
// whichever comes first" half of the issue #346 acceptance: when the user submits
// or clears the session input, the keystroke that triggered it went to the input
// (the dialog was deferred), so the modal can appear immediately rather than wait
// out the idle window. Must run on the event loop.
//
// It declines to raise a dialog onto a desktop that is tearing down: by then
// serializePrompt has already unblocked the agent goroutine with the safe default,
// so the modal would be orphaned (and the deferred timer may have fired after
// shutdown). Clearing the state first keeps a dead closure from lingering.
func (w *Workbench) drainDeferredModalNow() {
	show := w.deferredModal
	w.deferredModal = nil
	w.stopDeferredTimer()
	if show == nil {
		return
	}
	select {
	case <-w.shutdown.Done():
		return
	default:
	}
	show()
}

// armDeferredRecheck schedules maybeShowDeferredModal to run again on the event
// loop after deferredRecheckInterval, replacing any pending timer. The timer
// fires on its own goroutine, so it funnels back through Desktop.Post.
func (w *Workbench) armDeferredRecheck() {
	w.stopDeferredTimer()
	if w.desktop == nil {
		return
	}
	w.deferredTimer = time.AfterFunc(deferredRecheckInterval, func() {
		w.desktop.Post(w.maybeShowDeferredModal)
	})
}

// stopDeferredTimer cancels and clears the pending re-check timer, if any.
func (w *Workbench) stopDeferredTimer() {
	if w.deferredTimer != nil {
		w.deferredTimer.Stop()
		w.deferredTimer = nil
	}
}

// AskPermission implements permission.Prompter. It is called from a background
// (agent-loop) goroutine, so it marshals the modal onto the UI thread and
// blocks until the user chooses.
//
// Two hazards are guarded here. First, the resolving closure only runs while
// the UI loop is alive, so a prompt still open when the user quits would block
// forever; AskPermission therefore also selects on the shutdown context and
// returns DecisionDeny when the UI is gone, unblocking the agent goroutine.
// Second, parallel tool calls would each post a modal and steal focus, so
// prompts are serialized and presented one at a time.
//
// A notification is emitted first (issue #59): an approval prompt blocks the
// agent, so it is exactly the "needs attention" event a user stepping away
// wants to be pinged for. An approval always fires regardless of which session
// is focused (focused=false) — a blocking prompt warrants attention even on the
// foreground session. The request now carries the requesting session (issue
// #55), used to badge that session's sidebar node and name it in the dialog.
func (w *Workbench) AskPermission(req permission.Request) permission.Decision {
	w.notifyApproval(req)
	// Badge the requesting session (and the global indicator) for the whole life
	// of the prompt — including the time it spends queued behind another modal —
	// so a background session's pending approval is never silently missed.
	w.markApproval(req.Context.SessionID, +1)
	defer w.markApproval(req.Context.SessionID, -1)
	return w.prompt(req, func(req permission.Request, resolve func(permission.Decision)) {
		requester := w.requesterLabel(req.Context.SessionID)
		// Defer presentation while the user is mid-keystroke so the dialog cannot
		// hijack their Enter; the badge/notification already fired (issue #346).
		w.desktop.Post(func() {
			w.presentBackgroundModal(func() {
				showPermissionDialog(w.desktop, req, requester, resolve)
			})
		})
	})
}

// markApproval adjusts the in-flight permission-prompt count for a session and
// refreshes the requesting session's sidebar ⏳ badge (issue #55). delta is +1 when
// a prompt is raised and -1 when it resolves. It is called from the agent
// goroutine, so the sidebar mutation is marshalled onto the UI thread. A session id
// of "" (headless/unknown requester) badges no node — and, since the phantom global
// header counter was removed (issue #230), no longer leaves an attention count with
// no matching row. The per-session count (w.approvals) is still tracked so a session
// with several in-flight prompts keeps its badge until the last one resolves.
func (w *Workbench) markApproval(sessionID string, delta int) {
	w.mu.Lock()
	if w.approvals == nil {
		w.approvals = make(map[string]int)
	}
	n := w.approvals[sessionID] + delta
	if n <= 0 {
		delete(w.approvals, sessionID)
		n = 0
	} else {
		w.approvals[sessionID] = n
	}
	title := ""
	pinned := w.pinned[sessionID]
	if sw := w.sessions[sessionID]; sw != nil {
		title = sw.title
	}
	w.mu.Unlock()

	if w.sidebar == nil || w.desktop == nil {
		return
	}
	pending := n > 0
	// A headless/unknown requester (sessionID == "") badges no node and, with the
	// global header counter gone (issue #230), updates nothing in the sidebar.
	if sessionID == "" {
		return
	}
	w.desktop.Post(func() {
		w.sidebar.setApproval(sessionID, title, pinned, pending)
		w.desktop.Redraw()
	})
}

// requesterLabel resolves a human label for the session that raised a prompt, so
// the dialog can name it ("Session 2") rather than leave the user guessing which
// background session is asking. Empty when the requester is unknown.
func (w *Workbench) requesterLabel(sessionID string) string {
	if sessionID == "" {
		return ""
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if sw := w.sessions[sessionID]; sw != nil {
		return sw.title
	}
	return sessionID
}

// notifyApproval fires an "approval needed" notification when the config allows
// it. It is called from the agent goroutine, so the emit is marshalled onto the
// UI thread: that keeps the bell/OSC write serialized with frame rendering
// (avoids interleaving with the TUI's own stdout writes) and matches how session
// events are notified. An approval always fires regardless of focus
// (focused=false) — a blocking prompt warrants attention even on the foreground
// session.
func (w *Workbench) notifyApproval(req permission.Request) {
	if w.notify == nil {
		return
	}
	title, _, _ := permissionPrompt(req)
	body := req.Detail
	if body == "" {
		_, body, _ = permissionPrompt(req)
	}
	// Name the requesting session so an alert for an unfocused background session
	// is actionable (issue #55).
	if label := w.requesterLabel(req.Context.SessionID); label != "" {
		body = label + ": " + body
	}
	w.desktop.Post(func() {
		if w.notify.ShouldNotify(notify.ReasonApproval, false) {
			w.notify.Notify(title, body)
		}
	})
}

// prompt serializes a single permission request and waits for either the UI to
// resolve it or the workbench to shut down. present is responsible for showing
// the request to the user (on the UI thread) and calling resolve exactly once
// with the chosen decision. It is a seam so the queue/shutdown logic can be
// tested without a live event loop.
func (w *Workbench) prompt(req permission.Request, present func(permission.Request, func(permission.Decision))) permission.Decision {
	return serializePrompt(w, permission.DecisionDeny, func(resolve func(permission.Decision)) {
		present(req, resolve)
	})
}

// serializePrompt is the shared core behind every blocking modal (permission
// prompts and the edit-review dialog): it presents one modal at a time
// (promptMu), refuses to post to a dead event loop, and unblocks the calling
// agent goroutine with onShutdown if the UI quits before the user answers.
// present must call resolve exactly once on the UI thread; a stray second call
// is dropped.
func serializePrompt[T any](w *Workbench, onShutdown T, present func(resolve func(T))) T {
	// One modal at a time: later requests queue here rather than stacking.
	w.promptMu.Lock()
	defer w.promptMu.Unlock()

	// Don't post to a dead event loop if we're already shutting down.
	select {
	case <-w.shutdown.Done():
		return onShutdown
	default:
	}

	result := make(chan T, 1)
	present(func(d T) {
		// Buffered + non-blocking so a stray second call can't block the UI.
		select {
		case result <- d:
		default:
		}
	})

	select {
	case d := <-result:
		return d
	case <-w.shutdown.Done():
		// The UI loop stopped before the user answered; fall back to the safe
		// default rather than leak the goroutine.
		return onShutdown
	}
}

// permissionPrompt renders the human-readable question and the "always" label
// for a request. The middle-button caption is the fixed, concise "Always allow"
// for every action: the specific resource/path/scope is already shown in full in
// the dialog body (the question embeds it for external & network actions and the
// shell command is shown verbatim), so a longer label would only duplicate that
// text and force permissionButtonRow to elide it (issue #447).
func permissionPrompt(req permission.Request) (title, question, alwaysLabel string) {
	const alwaysAllow = "Always allow"
	switch req.Action {
	case permission.ActionShell:
		return "Run shell command?",
			"The agent wants to run shell commands in this session.",
			alwaysAllow
	case permission.ActionExternal:
		return "Access outside workspace?",
			fmt.Sprintf("The agent wants to access a location outside the workspace:\n%s", req.Resource),
			alwaysAllow
	case permission.ActionSubagent:
		return "Spawn sub-agent?", "The agent wants to spawn a sub-agent.", alwaysAllow
	case permission.ActionNetwork:
		if req.Resource != "" {
			return "Network access?",
				fmt.Sprintf("The agent wants to fetch from the network:\n%s", req.Resource),
				alwaysAllow
		}
		return "Network access?", "The agent wants to access the network.", alwaysAllow
	case permission.ActionLSP:
		return "Launch language server?",
			fmt.Sprintf("The agent wants to launch the %q language server for semantic diagnostics and navigation.", req.Resource),
			alwaysAllow
	case permission.ActionLSPCommand:
		return "Run language-server command?",
			fmt.Sprintf("The agent wants the language server to run a command (%s). Its side effects are NOT checkpointable/undoable.", req.Resource),
			alwaysAllow
	default:
		return "Permission required",
			fmt.Sprintf("The agent requests %s on %s.", req.Action, req.Resource),
			alwaysAllow
	}
}

func showPermissionDialog(desktop *tv.Desktop, req permission.Request, requester string, onResult func(permission.Decision)) {
	if desktop == nil {
		onResult(permission.DecisionDeny)
		return
	}

	title, question, alwaysLabel := permissionPrompt(req)
	bodyLines := permissionDialogBody(req, question)

	app := desktop.App()
	requesterHdr := requesterLine(requester, req.Context.Agent)
	spec := permissionDialogSpec(app.Width(), app.Height(), requesterHdr != "", bodyLines)
	x, y, width, height := tv.ResolveDialogRect(spec, app.Width(), app.Height())
	bodyY, bodyH, btnY := permissionContentLayout(height, requesterHdr != "")

	dialog := tv.NewDialog(title, x, y, width, height)
	applyWindowShadow(dialog.Window) // honour the NoShadow theme setting (issue #215)
	dialog.Window.ShowClose = false

	// Name the requesting session/agent so a prompt raised by an unfocused
	// background session is unambiguous (issue #55). It stays pinned above the
	// (possibly scrolling) body.
	if requesterHdr != "" {
		r := tv.NewLabel(truncate(requesterHdr, width-4), tv.Rect{X: 2, Y: 1, W: width - 4, H: 1})
		r.FG = colorDialogHeader
		r.BG = tv.DefaultTheme.DialogBG
		dialog.Window.AddContent(r)
	}

	// The decision-relevant text — the question (which carries the resource/path
	// for external and network actions) and the shell command — goes in a wrapping,
	// scrollable view so nothing is hidden behind "…". The dialog is sized to show
	// it in full when it fits the terminal and scrolls when it does not (issue
	// #122). Scroll with the mouse wheel or Tab to the view and use the arrows.
	body := tv.NewTextView("", tv.Rect{X: 2, Y: bodyY, W: width - 4, H: bodyH})
	body.Wrap = true
	body.FG = tv.DefaultTheme.DialogFG
	body.BG = tv.DefaultTheme.DialogBG
	body.FocusFG = tv.DefaultTheme.MnemonicFG
	for _, line := range bodyLines {
		body.AddColored(line.text, line.color)
	}
	// The approval text is read from the top (the question precedes the command),
	// so open anchored at the first line (issue #174).
	body.ScrollToTop()
	dialog.Window.AddContent(body)

	var layer *tv.Layer
	done := false
	finish := func(d permission.Decision) {
		if done {
			return
		}
		done = true
		desktop.RemoveLayer(layer)
		onResult(d)
	}

	// "Allow once" is left-anchored, "Deny" right-anchored, and the fixed "Always
	// allow" caption fills the space between — comfortably un-elided at every width
	// down to the permissionMinWidth floor, with permissionButtonRow/fitButtonLabel
	// remaining the safety net that elides only on a sub-floor terminal (issue #447).
	allowRect, alwaysRect, denyRect, alwaysText := permissionButtonRow(width, btnY, alwaysLabel)
	allowOnce := newButton("Allow once", allowRect, func() {
		finish(permission.DecisionAllow)
	})
	always := newButton(alwaysText, alwaysRect, func() {
		finish(permission.DecisionAlways)
	})
	deny := newButton("Deny", denyRect, func() {
		finish(permission.DecisionDeny)
	})
	dialog.Window.AddContent(allowOnce)
	dialog.Window.AddContent(always)
	dialog.Window.AddContent(deny)

	// Escape denies.
	dialog.Root().OnTypeFn = func(_ *tv.VisualComponent, event tui.TypeEvent) bool {
		if event.Key == tui.KeyEscape {
			finish(permission.DecisionDeny)
			return true
		}
		return false
	}

	layer = tv.NewModalLayer("permission-dialog", dialog)
	desktop.AddLayer(layer)
	// The spec's PrefH is measured against the open-time width, so re-resolve
	// against the live terminal on resize rather than the stale spec dialog.Fit
	// would remember (issues #299, #309).
	installResizeReflow(desktop, dialog, layer, func() tv.DialogSpec {
		return permissionDialogSpec(app.Width(), app.Height(), requesterHdr != "", bodyLines)
	})
	desktop.SetFocus(deny)
}

// Sizing knobs for the permission dialog (issues #299, #309). Width is driven by
// the content — wide enough to show the longest command/path line — capped at
// permissionMaxWidth so a 3-line prompt is not near-full-screen (the old
// full-width PreferredW=MaxW=termW-2 is gone), with the cramping 92-column cap
// also gone. Height grows with the wrapped content up to permissionMaxHeight and
// then scrolls, replacing the terminal-baked MaxH=termH-2 (which made the height
// path-dependent on resize). The floors keep a prompt legible on a tiny terminal.
const (
	permissionMinWidth  = 52
	permissionMaxWidth  = 110
	permissionMinHeight = 8
	permissionMaxHeight = 24
	// permissionPad is the horizontal chrome around the body: 2 borders + 2 content
	// margins + 1 reserved scrollbar column, so PreferredW = longest body line +
	// permissionPad shows the widest line in full before it word-wraps.
	permissionPad = 5
)

// permissionBodyLine is one coloured line of the dialog's scrollable body. The
// question and the command may exceed the dialog width, so they are wrapped
// rather than truncated (issue #122).
type permissionBodyLine struct {
	text  string
	color tui.Color
}

// permissionDialogBody composes the wrapping body: the prompt question (which for
// external and network actions embeds the resource/path) followed by the shell
// command from req.Detail, prefixed with "$ " like a prompt. Each logical line is
// a separate entry so turbotui wraps it rather than collapsing embedded newlines,
// and nothing is elided — the full resource and command are always present.
func permissionDialogBody(req permission.Request, question string) []permissionBodyLine {
	lines := make([]permissionBodyLine, 0, 2)
	for _, line := range strings.Split(question, "\n") {
		lines = append(lines, permissionBodyLine{text: line, color: tv.DefaultTheme.DialogFG})
	}
	if req.Detail != "" {
		for i, line := range strings.Split(req.Detail, "\n") {
			if i == 0 {
				line = "$ " + line
			}
			lines = append(lines, permissionBodyLine{text: line, color: colorDialogDetail})
		}
	}
	return lines
}

// permissionBodyOffsetY is the body's content-relative top row: row 0 is top
// padding, and a requester header (when present) takes row 1, pushing the body to
// row 2.
func permissionBodyOffsetY(hasRequester bool) int {
	if hasRequester {
		return 2
	}
	return 1
}

// permissionContentVChrome is the vertical cost below the body: 1 gap + 1 button
// row + 1 bottom pad + 2 borders. height = bodyY + bodyH + permissionContentVChrome.
const permissionContentVChrome = 5

// permissionBodyRows reports how many display rows the wrapping body occupies in a
// dialog of the given outer width — each logical line wrapped at width-5 (the body
// spans width-4 columns and turbotui reserves the last for the scrollbar). It
// delegates to turbotui's WrapText so the prediction matches the real render.
func permissionBodyRows(bodyLines []permissionBodyLine, width int) int {
	wrapW := width - 5
	if wrapW < 1 {
		wrapW = 1
	}
	rows := 0
	for _, line := range bodyLines {
		rows += len(tv.WrapText(line.text, wrapW))
	}
	if rows < 1 {
		rows = 1
	}
	return rows
}

// permissionContentWidth is the dialog width that shows the longest body line in
// full: the widest logical line plus the horizontal chrome. The resolver caps it
// at permissionMaxWidth, so a long command grows the dialog only up to the cap and
// then wraps/scrolls.
func permissionContentWidth(bodyLines []permissionBodyLine) int {
	longest := 0
	for _, line := range bodyLines {
		if w := tui.StringWidth(line.text); w > longest {
			longest = w
		}
	}
	return longest + permissionPad
}

// permissionDialogSpec turns a permission request into a content-driven DialogSpec:
// wide enough to show the longest command/path line (PreferredW) up to
// permissionMaxWidth, and tall enough for the wrapped content (PrefH) up to
// permissionMaxHeight so nothing is hidden behind "…". The caps replace the old
// full-width PreferredW/MaxW=termW-2 and terminal-baked MaxH=termH-2 (issue #309),
// so a short prompt stays compact and a long one grows toward the cap and scrolls.
// It is pure so the sizing can be tested without a live event loop (issues #122,
// #299); the body origin/height and button row come from permissionContentLayout
// applied to the resolved height.
func permissionDialogSpec(termW, termH int, hasRequester bool, bodyLines []permissionBodyLine) tv.DialogSpec {
	prefW := permissionContentWidth(bodyLines)
	// Resolve the width first (height does not affect it) so the body-row count is
	// measured against the real dialog width.
	_, _, width, _ := tv.ResolveDialogRect(
		tv.DialogSpec{MinW: permissionMinWidth, MaxW: permissionMaxWidth, PreferredW: prefW}, termW, termH)
	bodyY := permissionBodyOffsetY(hasRequester)
	return tv.DialogSpec{
		MinW:       permissionMinWidth,
		MinH:       permissionMinHeight,
		MaxW:       permissionMaxWidth,
		PreferredW: prefW,
		PrefH:      bodyY + permissionBodyRows(bodyLines, width) + permissionContentVChrome,
		MaxH:       permissionMaxHeight,
	}
}

// permissionContentLayout derives the body's content-relative origin/height and
// the button row Y from a resolved dialog height. Splitting it from the spec keeps
// the open-time placement in lockstep with the size that was resolved.
func permissionContentLayout(height int, hasRequester bool) (bodyY, bodyH, btnY int) {
	bodyY = permissionBodyOffsetY(hasRequester)
	bodyH = height - bodyY - permissionContentVChrome
	if bodyH < 1 {
		bodyH = 1
	}
	btnY = bodyY + bodyH + 1
	return bodyY, bodyH, btnY
}

// permissionButtonRow lays out the three action buttons on content row btnY across
// a dialog of the given width. "Allow once" is left-anchored at the content left
// margin, "Deny" is right-anchored to the content right edge, and "Always …" takes
// the space between them — its label elided with "..." when the resource is long —
// so the row never overlaps or escapes the dialog. The returned alwaysLabel is the
// (possibly elided) text to render.
func permissionButtonRow(width, btnY int, alwaysLabel string) (allowOnce, always, deny tv.Rect, alwaysText string) {
	const gap = 2
	leftX := 2
	rightX := width - 3
	allowOnce = tv.Rect{X: leftX, Y: btnY, W: tv.ButtonLabelWidth("Allow once"), H: 1}

	denyW := tv.ButtonLabelWidth("Deny")
	deny = tv.Rect{X: rightX - denyW + 1, Y: btnY, W: denyW, H: 1}

	slotStart := allowOnce.X + allowOnce.W + gap
	slotEnd := deny.X - gap - 1
	avail := slotEnd - slotStart + 1
	if avail < 1 {
		avail = 1
	}
	alwaysText = fitButtonLabel(alwaysLabel, avail)
	alwaysW := tv.ButtonLabelWidth(alwaysText)
	if alwaysW < 1 {
		alwaysW = 1
	}
	if alwaysW > avail {
		alwaysW = avail
	}
	always = tv.Rect{X: slotStart, Y: btnY, W: alwaysW, H: 1}
	return allowOnce, always, deny, alwaysText
}

// fitButtonLabel elides label (appending "...") so its rendered button width — the
// clean label's display width plus the "[ " / " ]" chrome — fits in maxCols. The
// full resource stays visible in the scrollable body, so this only shortens the
// button caption. Width is measured with tui.StringWidth so a CJK/emoji resource
// is neither over- nor under-elided. Mirrors truncate()'s "..." convention but is
// chrome- and display-width-aware.
func fitButtonLabel(label string, maxCols int) string {
	maxClean := maxCols - buttonChrome
	clean, _ := tv.ParseMnemonic(label)
	if maxClean <= 0 || tui.StringWidth(clean) <= maxClean {
		return label
	}
	runes := []rune(clean)
	if maxClean <= 3 {
		return truncateToWidth(runes, maxClean)
	}
	return truncateToWidth(runes, maxClean-3) + "..."
}

// truncateToWidth returns the longest prefix of runes whose display width
// (tui.StringWidth) does not exceed max columns, so a multi-cell rune is dropped
// whole rather than split across the budget.
func truncateToWidth(runes []rune, max int) string {
	out := make([]rune, 0, len(runes))
	width := 0
	for _, r := range runes {
		rw := tui.StringWidth(string(r))
		if width+rw > max {
			break
		}
		out = append(out, r)
		width += rw
	}
	return string(out)
}

// requesterLine renders the "Requested by …" header for the permission dialog.
// It names the session and, when the request came from a sub-agent, that agent
// too. The session's primary agent ("root") is implied by the session itself, so
// its id is not repeated. Empty session yields an empty line (header omitted).
func requesterLine(session, agentID string) string {
	if session == "" {
		return ""
	}
	if agentID != "" && agentID != "root" {
		return fmt.Sprintf("Requested by %s · agent %s", session, agentID)
	}
	return "Requested by " + session
}

// truncate shortens s to fit max display columns, appending "..." when it must
// cut. Width is measured with tui.StringWidth and the cut lands on a rune
// boundary (via truncateToWidth), so a multi-cell CJK/emoji glyph in a dialog
// title or "Requested by …" header is never split mid-character into broken UTF-8
// — the display-width consistency the whole sizing migration is built on (#299).
func truncate(s string, max int) string {
	if max <= 1 || tui.StringWidth(s) <= max {
		return s
	}
	runes := []rune(s)
	if max <= 3 {
		return truncateToWidth(runes, max)
	}
	return truncateToWidth(runes, max-3) + "..."
}
