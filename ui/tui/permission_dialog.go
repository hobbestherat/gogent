package ui

import (
	"fmt"

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
	// One modal at a time: later requests queue here rather than stacking.
	w.promptMu.Lock()
	defer w.promptMu.Unlock()

	// Don't post to a dead event loop if we're already shutting down.
	select {
	case <-w.shutdown.Done():
		return permission.DecisionDeny
	default:
	}

	result := make(chan permission.Decision, 1)
	present(req, func(d permission.Decision) {
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
		// The UI loop stopped before the user answered; deny rather than leak.
		return permission.DecisionDeny
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

	const width = 64
	const height = 13
	x := (desktop.App().Width() - width) / 2
	y := (desktop.App().Height() - height) / 2
	if x < 0 {
		x = 0
	}
	if y < 0 {
		y = 0
	}

	dialog := tv.NewDialog(title, x, y, width, height)
	dialog.Window.ShowClose = false

	// Name the requesting session/agent so a prompt raised by an unfocused
	// background session is unambiguous (issue #55). It takes the top content row;
	// the question moves down one row to make space.
	questionY := 1
	if label := requesterLine(requester, req.Context.Agent); label != "" {
		r := tv.NewLabel(truncate(label, width-4), tv.Rect{X: 2, Y: 1, W: width - 4, H: 1})
		r.FG = tui.ANSIColor(14)
		r.BG = tv.DefaultTheme.DialogBG
		dialog.Window.AddContent(r)
		questionY = 2
	}

	q := tv.NewLabel(question, tv.Rect{X: 2, Y: questionY, W: width - 4, H: 3})
	q.FG = tv.DefaultTheme.DialogFG
	q.BG = tv.DefaultTheme.DialogBG
	dialog.Window.AddContent(q)

	if req.Detail != "" {
		detail := tv.NewLabel("$ "+truncate(req.Detail, width-6), tv.Rect{X: 2, Y: 5, W: width - 4, H: 1})
		detail.FG = tui.ANSIColor(11)
		detail.BG = tv.DefaultTheme.DialogBG
		dialog.Window.AddContent(detail)
	}

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

	allowOnce := tv.NewButton("Allow once", tv.Rect{X: 2, Y: 8, W: 14, H: 1}, func() {
		finish(permission.DecisionAllow)
	})
	always := tv.NewButton(truncate(alwaysLabel, 24), tv.Rect{X: 18, Y: 8, W: 28, H: 1}, func() {
		finish(permission.DecisionAlways)
	})
	deny := tv.NewButton("Deny", tv.Rect{X: 48, Y: 8, W: 10, H: 1}, func() {
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
