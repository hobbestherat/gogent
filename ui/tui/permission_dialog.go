package ui

import (
	"fmt"
	"strings"

	"gogent/internal/notify"
	"gogent/internal/permission"

	tui "github.com/hobbestherat/turbotui"
	tv "github.com/hobbestherat/turbotui/turbotv"
)

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
		w.desktop.Post(func() {
			showPermissionDialog(w.desktop, req, requester, resolve)
		})
	})
}

// markApproval adjusts the in-flight permission-prompt count for a session and
// refreshes the requesting session's sidebar badge plus the global header
// indicator (issue #55). delta is +1 when a prompt is raised and -1 when it
// resolves. It is called from the agent goroutine, so the sidebar mutation is
// marshalled onto the UI thread. A session id of "" (headless/unknown requester)
// still moves the global counter but badges no node.
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
	total := 0
	for _, c := range w.approvals {
		total += c
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
	w.desktop.Post(func() {
		if sessionID != "" {
			w.sidebar.setApproval(sessionID, title, pinned, pending)
		}
		w.sidebar.setGlobalApprovals(total)
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
// for a request.
func permissionPrompt(req permission.Request) (title, question, alwaysLabel string) {
	switch req.Action {
	case permission.ActionShell:
		return "Run shell command?",
			"The agent wants to run shell commands in this session.",
			"Always (this session's workspace)"
	case permission.ActionExternal:
		return "Access outside workspace?",
			fmt.Sprintf("The agent wants to access a location outside the workspace:\n%s", req.Resource),
			fmt.Sprintf("Always allow %s", req.Resource)
	case permission.ActionSubagent:
		return "Spawn sub-agent?", "The agent wants to spawn a sub-agent.", "Always"
	case permission.ActionNetwork:
		if req.Resource != "" {
			return "Network access?",
				fmt.Sprintf("The agent wants to fetch from the network:\n%s", req.Resource),
				fmt.Sprintf("Always allow %s", req.Resource)
		}
		return "Network access?", "The agent wants to access the network.", "Always"
	case permission.ActionDiagnostics:
		return "Run diagnostics?",
			"The agent wants to run the project's compiler/linter to check for errors.",
			"Always (this session)"
	default:
		return "Permission required",
			fmt.Sprintf("The agent requests %s on %s.", req.Action, req.Resource),
			"Always"
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
	width, height, bodyY, bodyH, btnY := permissionDialogLayout(app.Width(), app.Height(), requesterHdr != "", bodyLines)

	x := (app.Width() - width) / 2
	y := (app.Height() - height) / 2
	if x < 0 {
		x = 0
	}
	if y < 0 {
		y = 0
	}
	dialog := tv.NewDialog(title, x, y, width, height)
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

	// "Allow once" is left-anchored, "Deny" right-anchored, and "Always …" fills
	// the space between (elided when the resource is long), so the row stays clean
	// and in-bounds at any dialog width.
	allowRect, alwaysRect, denyRect, alwaysText := permissionButtonRow(width, btnY, alwaysLabel)
	allowOnce := tv.NewButton("Allow once", allowRect, func() {
		finish(permission.DecisionAllow)
	})
	always := tv.NewButton(alwaysText, alwaysRect, func() {
		finish(permission.DecisionAlways)
	})
	deny := tv.NewButton("Deny", denyRect, func() {
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
	desktop.SetFocus(deny)
}

// Sizing knobs for the permission dialog. Width grows with the terminal (clamped
// to [permissionMinWidth, permissionMaxWidth]) so long commands and paths get
// room; height grows with the wrapped content up to permissionMaxBodyRows, beyond
// which (and on short terminals) the body scrolls instead of overflowing.
const (
	permissionMinWidth    = 52
	permissionMaxWidth    = 92
	permissionMinHeight   = 8
	permissionMaxBodyRows = 16
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

// permissionDialogLayout sizes the dialog for the terminal and its content and
// returns the body's content-relative origin/height plus the button row Y. It is
// pure so the sizing (grow-with-content, clamp-to-terminal, scroll-on-overflow)
// can be tested without a live event loop (issue #122).
func permissionDialogLayout(termW, termH int, hasRequester bool, bodyLines []permissionBodyLine) (width, height, bodyY, bodyH, btnY int) {
	width = termW - 2
	if width > permissionMaxWidth {
		width = permissionMaxWidth
	}
	if width < permissionMinWidth {
		width = permissionMinWidth
	}
	if width > termW {
		width = termW
	}
	if width < 1 {
		width = 1
	}

	// Effective text columns inside the body: it spans width-4 columns (X:2 …
	// width-3) and turbotui reserves its last column for the scrollbar.
	wrapW := width - 5
	if wrapW < 1 {
		wrapW = 1
	}
	contentRows := 0
	for _, line := range bodyLines {
		contentRows += wrapRowCount(line.text, wrapW)
	}
	if contentRows < 1 {
		contentRows = 1
	}

	bodyY = 1 // leave content row 0 as top padding
	if hasRequester {
		bodyY = 2
	}

	desiredBody := contentRows
	if desiredBody > permissionMaxBodyRows {
		desiredBody = permissionMaxBodyRows
	}

	// height = 2 borders + topPad + requester? + body + 1 gap + 1 button row + 1 bottom pad.
	height = bodyY + desiredBody + 5
	if max := termH - 2; height > max {
		height = max
	}
	if height < permissionMinHeight {
		height = permissionMinHeight
	}
	if height > termH {
		height = termH
	}

	// Derive the body height from the final (possibly clamped) dialog height.
	bodyH = height - bodyY - 5
	if bodyH < 1 {
		bodyH = 1
	}
	btnY = bodyY + bodyH + 1
	return width, height, bodyY, bodyH, btnY
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
	allowOnce = tv.Rect{X: leftX, Y: btnY, W: buttonLabelWidth("Allow once"), H: 1}

	denyW := buttonLabelWidth("Deny")
	deny = tv.Rect{X: rightX - denyW + 1, Y: btnY, W: denyW, H: 1}

	slotStart := allowOnce.X + allowOnce.W + gap
	slotEnd := deny.X - gap - 1
	avail := slotEnd - slotStart + 1
	if avail < 1 {
		avail = 1
	}
	alwaysText = fitButtonLabel(alwaysLabel, avail)
	alwaysW := buttonLabelWidth(alwaysText)
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
// clean rune count plus the "[ " / " ]" chrome — fits in maxCols. The full
// resource stays visible in the scrollable body, so this only shortens the button
// caption. Mirrors truncate()'s "..." convention but is chrome-aware.
func fitButtonLabel(label string, maxCols int) string {
	maxClean := maxCols - buttonChrome
	if maxClean <= 0 || cleanMnemonicRunes(label) <= maxClean {
		return label
	}
	runes := []rune(label)
	if maxClean <= 3 {
		return string(runes[:maxClean])
	}
	return string(runes[:maxClean-3]) + "..."
}

// wrapRowCount reports how many display rows text occupies when word-wrapped at
// width columns, matching turbotui's TextView Wrap layout (greedy word fill that
// hard-splits words longer than the width). It mirrors turbotv.wrapText so the
// dialog's computed height matches what the widget actually renders; empty text
// occupies a single row.
func wrapRowCount(text string, width int) int {
	if width < 1 {
		width = 1
	}
	if text == "" {
		return 1
	}
	rows := 0
	cur := 0 // rune length of the in-progress row; 0 means the row is empty
	for _, word := range strings.Fields(text) {
		wlen := len([]rune(word))
		if cur != 0 && cur+1+wlen <= width {
			cur += 1 + wlen
			continue
		}
		if wlen <= width {
			rows++
			cur = wlen
			continue
		}
		// Over-long word: full width rows plus a final partial (or full) row.
		full := wlen / width
		rem := wlen % width
		if rem == 0 {
			rows += full
			cur = width
		} else {
			rows += full + 1
			cur = rem
		}
	}
	if rows == 0 {
		return 1
	}
	return rows
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

func truncate(s string, max int) string {
	if max <= 1 || len(s) <= max {
		return s
	}
	if max <= 3 {
		return s[:max]
	}
	return s[:max-3] + "..."
}
