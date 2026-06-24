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
		// Defer presentation while the user is mid-keystroke so the dialog cannot
		// hijack their Enter; the badge/notification already fired (issue #346).
		w.desktop.Post(func() {
			w.presentBackgroundModal(func() {
				showReviewDialog(w.desktop, req, requester, resolve)
			})
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

	// Large by default (≈80%×85% of the terminal) with a 40×12 floor; the diff view
	// fills the dialog, so it grows with the terminal (issue #299). A 120-column MaxW
	// keeps a diff readable on an ultrawide terminal (issue #317) — matching the
	// permission dialog's cap — while it still grows tall (no height cap; a diff wants
	// the vertical space).
	spec := tv.DialogSpec{MinW: 40, MaxW: 120, MinH: 12}
	x, y, width, height := tv.ResolveDialogRect(spec, desktop.App().Width(), desktop.App().Height())

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

	// Size each button to its rendered label width and clamp the row to the content
	// margins so the group never overlaps or escapes the border on a narrow terminal
	// and re-flows as the dialog grows (issue #447).
	btnY := height - 3
	acceptRect, acceptAllRect, rejectRect := reviewButtonRow(width, btnY)
	accept := newButton("&Accept", acceptRect, func() {
		finish(gogent.EditApprove)
	})
	acceptAll := newButton("Accept a&ll", acceptAllRect, func() {
		finish(gogent.EditApproveAll)
	})
	reject := newButton("&Reject", rejectRect, func() {
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
	dialog.Fit(spec) // re-resolve the rect when the terminal is resized (issue #299)
	desktop.SetFocus(reject)
}

// reviewButtonLabels are the review dialog's action buttons in display order
// (left to right). They are a package var so a test can reference the same labels
// reviewButtonRow sizes against without re-declaring them.
var reviewButtonLabels = []string{"&Accept", "Accept a&ll", "&Reject"}

// reviewButtonRow lays out the three review buttons on content row btnY across a
// dialog of the given width. Each is sized to its full rendered label width
// (tv.ButtonLabelWidth) and the group is left-packed from the content left margin.
// Buttons are separated by tv.DefaultButtonGap on a roomy dialog; when the dialog
// is too narrow to hold the group at that gap, the gap shrinks (toward 0) just
// enough to keep every button at full width inside [2, width-3] — so all three
// stay fully visible and in-bounds down to the MinW (40) floor and the row re-flows
// as the dialog grows. clampDialogRect remains the safety net: only below the floor,
// where even a zero gap cannot fit the group, is the trailing button clipped — its
// width collapses (to 0 in the degenerate case) so it is never drawn past the
// border. Mirrors permissionButtonRow and footerButtonRects (#447).
func reviewButtonRow(width, btnY int) (accept, acceptAll, reject tv.Rect) {
	leftX, rightX := 2, width-3
	n := len(reviewButtonLabels)
	widths := make([]int, n)
	total := 0
	for i, label := range reviewButtonLabels {
		widths[i] = tv.ButtonLabelWidth(label)
		total += widths[i]
	}

	// Shrink the inter-button gap from DefaultButtonGap only as far as needed to
	// keep every button at full width within [leftX, rightX]; on a roomy dialog the
	// gap is exactly DefaultButtonGap.
	gap := tv.DefaultButtonGap
	if n > 1 {
		if slack := (rightX - leftX + 1) - total; slack < gap*(n-1) {
			if gap = slack / (n - 1); gap < 0 {
				gap = 0
			}
		}
	}

	rects := make([]tv.Rect, n)
	x := leftX
	for i := range widths {
		rects[i] = clampDialogRect(tv.Rect{X: x, Y: btnY, W: widths[i], H: 1}, leftX, rightX)
		x += widths[i] + gap
	}
	return rects[0], rects[1], rects[2]
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
