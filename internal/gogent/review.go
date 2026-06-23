package gogent

import (
	"fmt"

	"gogent/internal/diff"
	"gogent/internal/tool"
)

// EditReviewDecision is the user's verdict on a proposed file mutation surfaced
// by the diff-review flow (issue #64).
type EditReviewDecision int

const (
	// EditReject discards the proposed write/edit; nothing is written.
	EditReject EditReviewDecision = iota
	// EditApprove applies this one proposed write/edit.
	EditApprove
	// EditApproveAll applies this write/edit and every later one in the session
	// without prompting again.
	EditApproveAll
)

// EditReviewRequest describes a proposed file mutation awaiting approval. Diff is
// a unified diff of the file's content before and after the operation.
type EditReviewRequest struct {
	SessionID string
	AgentID   string
	Path      string // workspace-relative (or external) path being written
	Op        string // "write" or "edit"
	Diff      string // unified diff (before → after)
}

// EditReviewer renders a proposed edit's diff and returns the user's verdict. It
// is implemented by the UI and invoked from the agent goroutine, so it must
// block until the user decides.
type EditReviewer interface {
	ReviewEdit(EditReviewRequest) EditReviewDecision
}

// SetReviewer installs the interactive edit reviewer. With no reviewer the
// diff-review gate is inert and writes proceed unchanged: the underlying write is
// already permission-authorized, so review is only an interactive confirmation
// (a headless run has nobody to ask).
func (g *Gogent) SetReviewer(r EditReviewer) {
	g.mu.Lock()
	g.reviewer = r
	g.mu.Unlock()
}

// ReviewEdits reports whether write/edit operations are gated behind the
// interactive diff-review approval (issue #64).
func (g *Gogent) ReviewEdits() bool {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.config != nil && g.config.ReviewEdits
}

// SetReviewEdits enables or disables the diff-review approval gate and persists
// the setting. Disabling it clears any per-session "approve all" grants so
// re-enabling starts from a clean slate.
func (g *Gogent) SetReviewEdits(enabled bool) {
	g.mu.Lock()
	if g.config != nil {
		g.config.ReviewEdits = enabled
	}
	if !enabled {
		g.reviewApprovedAll = make(map[string]bool)
	}
	g.mu.Unlock()
	if err := g.SaveConfig(); err != nil {
		g.warnf("Failed to persist config: %v", err)
	}
}

// reviewActive reports whether a proposed mutation in this session must be routed
// through the diff-review gate: review is enabled, a reviewer is installed, and
// the session has not chosen "approve all". When false, callers skip the preview
// read entirely and write exactly as before.
//
// Yolo mode (issue #356) skips the gate entirely: yolo's contract is "no human in
// the loop," so the edit-review gate — a human-in-the-loop layer on top of
// permissions — is bypassed alongside permission prompts. The rules.json hard-deny
// guardrails (issue #355) still apply, since they gate at the permission layer the
// write must clear first.
func (g *Gogent) reviewActive(sessionID string) bool {
	if g.permissions.EffectiveYolo(sessionID) {
		return false
	}
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.config != nil && g.config.ReviewEdits && g.reviewer != nil && !g.reviewApprovedAll[sessionID]
}

// reviewEdit asks the user to approve a proposed mutation, given the file's
// content before and after. It returns nil to proceed with the write or an error
// when the user rejected it. A no-op change (before == after) passes without
// prompting; "approve all" is remembered for the session so later edits skip the
// gate. Callers gate this behind reviewActive to avoid the preview cost when the
// feature is off.
func (g *Gogent) reviewEdit(ctx tool.ToolContext, op, path, before, after string) error {
	if before == after {
		return nil
	}
	g.mu.RLock()
	reviewer := g.reviewer
	g.mu.RUnlock()
	if reviewer == nil {
		return nil
	}

	decision := reviewer.ReviewEdit(EditReviewRequest{
		SessionID: ctx.SessionID,
		AgentID:   ctx.AgentID,
		Path:      path,
		Op:        op,
		Diff:      diff.Unified(before, after, path),
	})
	switch decision {
	case EditApproveAll:
		g.mu.Lock()
		if g.reviewApprovedAll == nil {
			g.reviewApprovedAll = make(map[string]bool)
		}
		g.reviewApprovedAll[ctx.SessionID] = true
		g.mu.Unlock()
		return nil
	case EditApprove:
		return nil
	default:
		return fmt.Errorf("%s rejected by user: %s not applied", op, path)
	}
}
