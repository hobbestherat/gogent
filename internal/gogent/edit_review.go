package gogent

import (
	"errors"
	"fmt"

	"gogent/internal/diff"
)

// ErrEditRejected is returned by the write/edit tools when the user reviews a
// pending change (issue #64) and rejects it. It is surfaced to the model as a
// tool error so it can adjust course instead of retrying blindly.
var ErrEditRejected = errors.New("edit rejected by user")

// EditDecision is the outcome of an edit review.
type EditDecision int

const (
	// EditReject discards the pending change.
	EditReject EditDecision = iota
	// EditAccept applies this one change.
	EditAccept
	// EditAcceptAll applies this change and auto-approves the rest of the
	// session (no further diffs are shown until the process restarts).
	EditAcceptAll
)

// EditPreview describes a pending file change handed to an EditApprover.
type EditPreview struct {
	Path string    // workspace-relative path being written
	Diff string    // unified diff of the change (see internal/diff)
	Stat diff.Stat // added/removed line counts
}

// EditApprover shows a pending edit's diff to the user and reports their
// decision. It is implemented by the UI (the TUI workbench) and called from the
// agent goroutine, so implementations must marshal to their UI thread and block
// until the user answers. A nil approver disables review (the edit applies).
type EditApprover interface {
	ApproveEdit(EditPreview) EditDecision
}

// SetEditApprover installs the interactive edit-review approver. Without one,
// review is skipped even when ReviewEdits is enabled (e.g. headless runs have no
// human to consult), so writes are never silently lost.
func (g *Gogent) SetEditApprover(a EditApprover) {
	g.mu.Lock()
	g.editApprover = a
	g.mu.Unlock()
}

// ReviewEdits reports whether write/edit changes are gated behind a diff review.
func (g *Gogent) ReviewEdits() bool {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.config != nil && g.config.ReviewEdits
}

// SetReviewEdits enables or disables edit review and persists the choice.
// Turning review off also clears the "accept all this session" latch so
// re-enabling it starts prompting again.
func (g *Gogent) SetReviewEdits(enabled bool) {
	g.mu.Lock()
	if g.config != nil {
		g.config.ReviewEdits = enabled
	}
	if !enabled {
		g.acceptAllEdits = false
	}
	g.mu.Unlock()
	if err := g.SaveConfig(); err != nil {
		fmt.Printf("Warning: Failed to persist config: %v\n", err)
	}
}

// reviewEdit gates a pending change behind the user's approval when review is
// enabled. It returns nil when the change may be applied (review disabled, no
// approver, a no-op change, the user accepted, or "accept all" is latched) and
// ErrEditRejected when the user rejects it.
func (g *Gogent) reviewEdit(path, before, after string) error {
	g.mu.RLock()
	enabled := g.config != nil && g.config.ReviewEdits
	approver := g.editApprover
	acceptAll := g.acceptAllEdits
	g.mu.RUnlock()

	if !enabled || approver == nil || acceptAll {
		return nil
	}

	unified, stat := diff.Unified(path, before, after)
	if stat.IsEmpty() {
		// Nothing actually changes; don't bother the user.
		return nil
	}

	switch approver.ApproveEdit(EditPreview{Path: path, Diff: unified, Stat: stat}) {
	case EditAccept:
		return nil
	case EditAcceptAll:
		g.mu.Lock()
		g.acceptAllEdits = true
		g.mu.Unlock()
		return nil
	default:
		return ErrEditRejected
	}
}
