package ui

import (
	"fmt"
	"strings"

	"gogent/internal/gogent"
	"gogent/internal/notify"

	tui "github.com/hobbestherat/turbotui"
	tv "github.com/hobbestherat/turbotui/turbotv"
)

// ReviewEdit implements gogent.EditReviewer (issue #64). It is called from a
// background (agent-loop) goroutine before a write/edit hits disk: it marshals a
// diff-preview modal onto the UI thread and blocks until the user accepts,
// rejects, or accepts all further edits this session.
//
// It reuses the permission-prompt machinery: a notification fires first (a
// blocking review is exactly the "needs attention" event a user stepping away
// wants pinged for), the requesting session is badged for the life of the
// prompt, and serializePrompt guarantees one modal at a time and unblocks the
// agent on shutdown — returning EditReject (the safe default: never apply an
// unreviewed edit) if the UI is gone.
func (w *Workbench) ReviewEdit(req gogent.EditReviewRequest) gogent.EditReviewDecision {
	w.notifyReview(req)
	w.markApproval(req.SessionID, +1)
	defer w.markApproval(req.SessionID, -1)
	return serializePrompt(w, gogent.EditReject, func(resolve func(gogent.EditReviewDecision)) {
		requester := w.requesterLabel(req.SessionID)
		w.desktop.Post(func() {
			showReviewDialog(w.desktop, req, requester, resolve)
		})
	})
}

// notifyReview fires an "approval needed" notification for a pending edit review,
// naming the requesting session so an alert for an unfocused background session
// is actionable. Mirrors notifyApproval; a review always notifies regardless of
// focus because it blocks the agent.
func (w *Workbench) notifyReview(req gogent.EditReviewRequest) {
	if w.notify == nil {
		return
	}
	body := fmt.Sprintf("%s %s", req.Op, req.Path)
	if label := w.requesterLabel(req.SessionID); label != "" {
		body = label + ": " + body
	}
	w.desktop.Post(func() {
		if w.notify.ShouldNotify(notify.ReasonApproval, false) {
			w.notify.Notify("Review edit?", body)
		}
	})
}

// showReviewDialog renders the unified diff for a proposed edit in a scrollable,
// colour-coded view and offers Accept / Accept all / Reject. Escape rejects.
func showReviewDialog(desktop *tv.Desktop, req gogent.EditReviewRequest, requester string, onResult func(gogent.EditReviewDecision)) {
	if desktop == nil {
		onResult(gogent.EditReject)
		return
	}

	width := desktop.App().Width() * 70 / 100
	height := (desktop.App().Height() - 1) * 70 / 100
	if width < 40 {
		width = desktop.App().Width() - 2
	}
	if height < 12 {
		height = desktop.App().Height() - 2
	}
	x := (desktop.App().Width() - width) / 2
	y := (desktop.App().Height() - height) / 2
	if x < 0 {
		x = 0
	}
	if y < 0 {
		y = 0
	}

	title := fmt.Sprintf("Review %s: %s", req.Op, req.Path)
	dialog := tv.NewDialog(truncate(title, width-4), x, y, width, height)
	applyWindowShadow(dialog.Window) // honour the NoShadow theme setting (issue #215)
	dialog.Window.ShowClose = false

	// Optional "Requested by …" header for background sessions (issue #55).
	headerY := 1
	if label := requesterLine(requester, req.AgentID); label != "" {
		r := tv.NewLabel(truncate(label, width-4), tv.Rect{X: 2, Y: 1, W: width - 4, H: 1})
		r.FG = colorUser
		r.BG = tv.DefaultTheme.DialogBG
		dialog.Window.AddContent(r)
		headerY = 2
	}

	diffView := tv.NewTextView("", tv.Rect{X: 1, Y: headerY, W: width - 2, H: height - headerY - 4})
	renderDiff(diffView, req.Diff)
	// A diff is read top-down, so open anchored at the first line (issue #174).
	diffView.ScrollToTop()
	dialog.Window.AddContent(diffView)

	var layer *tv.Layer
	done := false
	finish := func(d gogent.EditReviewDecision) {
		if done {
			return
		}
		done = true
		desktop.RemoveLayer(layer)
		onResult(d)
	}

	// Left-packed so the three buttons never overlap on a narrow terminal.
	btnY := height - 3
	accept := newButton("&Accept", tv.Rect{X: 2, Y: btnY, W: 10, H: 1}, func() {
		finish(gogent.EditApprove)
	})
	acceptAll := newButton("Accept a&ll", tv.Rect{X: 13, Y: btnY, W: 15, H: 1}, func() {
		finish(gogent.EditApproveAll)
	})
	reject := newButton("&Reject", tv.Rect{X: 29, Y: btnY, W: 10, H: 1}, func() {
		finish(gogent.EditReject)
	})
	dialog.Window.AddContent(accept)
	dialog.Window.AddContent(acceptAll)
	dialog.Window.AddContent(reject)

	// Escape rejects.
	dialog.Root().OnTypeFn = func(_ *tv.VisualComponent, event tui.TypeEvent) bool {
		if event.Key == tui.KeyEscape {
			finish(gogent.EditReject)
			return true
		}
		return false
	}

	layer = tv.NewModalLayer("review-dialog", dialog)
	desktop.AddLayer(layer)
	desktop.SetFocus(reject)
}

// renderDiff fills a TextView with the unified diff, colouring added lines green,
// removed lines red, hunk headers cyan and file headers grey so the change is
// scannable at a glance.
func renderDiff(view *tv.TextView, diff string) {
	if strings.TrimSpace(diff) == "" {
		view.AddColored("(no changes)", colorNote)
		return
	}
	for _, line := range strings.Split(diff, "\n") {
		view.AddColored(line, diffLineColor(line))
	}
}

// diffLineColor maps a unified-diff line to its display colour.
func diffLineColor(line string) tui.Color {
	switch {
	case strings.HasPrefix(line, "+++") || strings.HasPrefix(line, "---"):
		return colorNote // file headers
	case strings.HasPrefix(line, "@@"):
		return colorInfo // hunk headers
	case strings.HasPrefix(line, "+"):
		return colorAgent // additions (green)
	case strings.HasPrefix(line, "-"):
		return colorError // deletions (red)
	default:
		return tv.DefaultTheme.DialogFG
	}
}
