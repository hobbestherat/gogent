package ui

import (
	"fmt"
	"strings"

	"gogent/internal/gogent"
	"gogent/internal/notify"
	"gogent/internal/permission"

	tui "github.com/hobbestherat/turbotui"
	tv "github.com/hobbestherat/turbotui/turbotv"
)

// ApproveEdit implements gogent.EditApprover (issue #64). It is called from the
// agent goroutine when ReviewEdits is on and a write/edit is about to touch
// disk, so it marshals a modal diff viewer onto the UI thread and blocks until
// the user decides. It reuses the same serialize-and-survive-shutdown machinery
// as permission prompts (one modal at a time; reject if the UI goes away),
// piggy-backing on permission.Decision: Allow=accept, Always=accept-all,
// anything else=reject.
func (w *Workbench) ApproveEdit(preview gogent.EditPreview) gogent.EditDecision {
	w.notifyEditReview(preview)

	req := permission.Request{Action: permission.ActionWrite, Resource: preview.Path}
	switch w.prompt(req, func(_ permission.Request, resolve func(permission.Decision)) {
		w.desktop.Post(func() {
			showEditReviewDialog(w, preview, resolve)
		})
	}) {
	case permission.DecisionAllow:
		return gogent.EditAccept
	case permission.DecisionAlways:
		return gogent.EditAcceptAll
	default:
		return gogent.EditReject
	}
}

// notifyEditReview pings the user that a pending edit needs a decision — it
// blocks the agent exactly like a permission prompt, so it reuses the approval
// notification reason. See notifyApproval for the marshalling rationale.
func (w *Workbench) notifyEditReview(preview gogent.EditPreview) {
	if w.notify == nil {
		return
	}
	body := fmt.Sprintf("%s  (+%d -%d)", preview.Path, preview.Stat.Added, preview.Stat.Removed)
	w.desktop.Post(func() {
		if w.notify.ShouldNotify(notify.ReasonApproval, false) {
			w.notify.Notify("Review edit?", body)
		}
	})
}

// showEditReviewDialog renders a pending change's unified diff in a scrollable,
// colour-coded viewer with Accept / Accept-all / Reject controls. onResult is
// called exactly once with the chosen decision.
func showEditReviewDialog(w *Workbench, preview gogent.EditPreview, onResult func(permission.Decision)) {
	if w.desktop == nil {
		onResult(permission.DecisionDeny)
		return
	}

	width := w.app.Width() * 70 / 100
	height := (w.app.Height() - 1) * 70 / 100
	if width < 48 {
		width = max(w.app.Width()-4, 20)
	}
	if height < 12 {
		height = max(w.app.Height()-2, 8)
	}
	x, y := centeredDialog(w, width, height)

	dialog := tv.NewDialog("Review edit", x, y, width, height)
	dialog.Window.ShowClose = false

	summary := dialogLabel(
		fmt.Sprintf("%s   +%d  -%d   (a)ccept · (r)eject · accept (A)ll session · Esc rejects",
			truncate(preview.Path, max(width-48, 8)), preview.Stat.Added, preview.Stat.Removed),
		tv.Rect{X: 2, Y: 1, W: width - 4, H: 1})
	dialog.Window.AddContent(summary)

	view := tv.NewTextView("", tv.Rect{X: 2, Y: 2, W: width - 4, H: height - 6})
	view.FG = tv.DefaultTheme.DialogFG
	view.BG = tv.DefaultTheme.DialogBG
	for _, line := range strings.Split(preview.Diff, "\n") {
		view.AddColored(line, diffLineColor(line))
	}
	dialog.Window.AddContent(view)

	var layer *tv.Layer
	done := false
	finish := func(d permission.Decision) {
		if done {
			return
		}
		done = true
		w.desktop.RemoveLayer(layer)
		onResult(d)
	}

	accept := tv.NewButton("Accept", tv.Rect{X: 2, Y: height - 3, W: 10, H: 1}, func() {
		finish(permission.DecisionAllow)
	})
	acceptAll := tv.NewButton("Accept all", tv.Rect{X: 14, Y: height - 3, W: 14, H: 1}, func() {
		finish(permission.DecisionAlways)
	})
	reject := tv.NewButton("Reject", tv.Rect{X: width - 12, Y: height - 3, W: 10, H: 1}, func() {
		finish(permission.DecisionDeny)
	})
	dialog.Window.AddContent(accept)
	dialog.Window.AddContent(acceptAll)
	dialog.Window.AddContent(reject)

	// Keyboard shortcuts work regardless of which control holds focus: letters
	// bubble up from the focused diff view (which only consumes scroll keys).
	dialog.Root().OnTypeFn = func(_ *tv.VisualComponent, event tui.TypeEvent) bool {
		switch {
		case event.Key == tui.KeyEscape:
			finish(permission.DecisionDeny)
			return true
		case event.Rune == 'a':
			finish(permission.DecisionAllow)
			return true
		case event.Rune == 'A':
			finish(permission.DecisionAlways)
			return true
		case event.Rune == 'r':
			finish(permission.DecisionDeny)
			return true
		}
		return false
	}

	layer = tv.NewModalLayer("edit-review-dialog", dialog)
	w.desktop.AddLayer(layer)
	// Focus the diff so the user can scroll a long change immediately; the
	// shortcuts above and the buttons cover the actual decision.
	w.desktop.SetFocus(view)
}

// diffLineColor picks a colour for a unified-diff line from its leading marker.
func diffLineColor(line string) tui.Color {
	switch {
	case strings.HasPrefix(line, "@@"), strings.HasPrefix(line, "+++"), strings.HasPrefix(line, "---"):
		return colorInfo // hunk / file headers
	case strings.HasPrefix(line, "+"):
		return colorAgent // green additions
	case strings.HasPrefix(line, "-"):
		return colorError // red deletions
	default:
		return colorNote // context
	}
}
