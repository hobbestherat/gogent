package ui

import (
	"fmt"

	"gogent/internal/permission"

	tui "github.com/hobbestherat/turbotui"
	tv "github.com/hobbestherat/turbotui/turbotv"
)

// AskPermission implements permission.Prompter. It is called from a background
// (agent-loop) goroutine, so it marshals the modal onto the UI thread and
// blocks until the user chooses.
func (w *Workbench) AskPermission(req permission.Request) permission.Decision {
	result := make(chan permission.Decision, 1)
	w.desktop.Post(func() {
		showPermissionDialog(w.desktop, req, func(d permission.Decision) {
			result <- d
		})
	})
	return <-result
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
		return "Network access?", "The agent wants to access the network.", "Always"
	default:
		return "Permission required",
			fmt.Sprintf("The agent requests %s on %s.", req.Action, req.Resource),
			"Always"
	}
}

func showPermissionDialog(desktop *tv.Desktop, req permission.Request, onResult func(permission.Decision)) {
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

	q := tv.NewLabel(question, tv.Rect{X: 2, Y: 1, W: width - 4, H: 3})
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

func truncate(s string, max int) string {
	if max <= 1 || len(s) <= max {
		return s
	}
	if max <= 3 {
		return s[:max]
	}
	return s[:max-3] + "..."
}
