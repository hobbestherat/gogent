package server

import (
	"context"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/hobbestherat/webapi"
	"gogent/internal/gogent"
	"gogent/internal/permission"
)

// pendingApproval is one interactive gate awaiting a remote client's decision.
// It is created when the agent loop calls a blocking Prompter/Reviewer (running
// in a tool goroutine) and resolved by POST /approvals/:aid/decision.
type pendingApproval struct {
	id         string
	kind       string // "permission" | "edit_review"
	sessionID  string
	agentID    string
	permission *permissionDetail
	editReview *editReviewDetail
	createdAt  time.Time
	decided    chan decision // closed/sent to unblock the waiting tool goroutine
}

// decision carries the user's answer. For permissions it is a permission.Decision
// (allow/deny/always/always_deny); for edit reviews an EditReviewDecision.
type decision struct {
	perm permission.Decision
	edit gogent.EditReviewDecision
}

// approvalBridge adapts gogent's two blocking interactive gates —
// permission.Prompter and gogent.EditReviewer — to the async API model: a prompt
// registers a pending approval, emits it on the event hub and an /approvals
// endpoint, then blocks until a client POSTs a decision (or the timeout fires,
// in which case it denies — the safe default that matches headless behavior).
type approvalBridge struct {
	hub     *hub
	timeout time.Duration // max wait before denying; 0 means never (block forever)
	now     func() time.Time

	mu      sync.Mutex
	pending map[string]*pendingApproval
	nextSeq int64
}

func newApprovalBridge(h *hub, timeout time.Duration, now func() time.Time) *approvalBridge {
	if now == nil {
		now = time.Now
	}
	return &approvalBridge{
		hub:     h,
		timeout: timeout,
		now:     now,
		pending: make(map[string]*pendingApproval),
	}
}

// Install wires the bridge as both the permission prompter and the edit
// reviewer on the gogent instance, so every interactive gate from a running turn
// surfaces to the API. It is called explicitly (not from NewServer) so the entry
// point can decide: headless mode installs it so a remote client answers prompts
// via the API; TUI mode leaves the workbench as the prompter instead.
func (s *Server) InstallApprovalGates() {
	s.g.GetPermissionService().SetPrompter(s.approvals)
	s.g.SetReviewer(s.approvals)
}

// --- permission.Prompter ----------------------------------------------------

func (b *approvalBridge) AskPermission(req permission.Request) permission.Decision {
	id := b.alloc("permission", req.Context.SessionID, req.Context.Agent, &permissionDetail{
		Action:   string(req.Action),
		Resource: req.Resource,
		Detail:   req.Detail,
	}, nil)

	d := b.wait(id, req.Context.SessionID, decision{perm: permission.DecisionDeny})
	return d.perm
}

// --- gogent.EditReviewer ----------------------------------------------------

func (b *approvalBridge) ReviewEdit(req gogent.EditReviewRequest) gogent.EditReviewDecision {
	id := b.alloc("edit_review", req.SessionID, req.AgentID, nil, &editReviewDetail{
		Path: req.Path,
		Op:   req.Op,
		Diff: req.Diff,
	})

	d := b.wait(id, req.SessionID, decision{edit: gogent.EditReject})
	return d.edit
}

// --- internals --------------------------------------------------------------

// alloc registers a new pending approval, announces it on the hub and returns
// its id. The announcement is delivered as a synthetic SessionEvent whose Args
// field carries the serialized approvalView.
func (b *approvalBridge) alloc(kind, sessionID, agentID string, perm *permissionDetail, edit *editReviewDetail) string {
	b.mu.Lock()
	b.nextSeq++
	id := "apr_" + strconv.FormatInt(b.nextSeq, 36)
	ap := &pendingApproval{
		id:         id,
		kind:       kind,
		sessionID:  sessionID,
		agentID:    agentID,
		permission: perm,
		editReview: edit,
		createdAt:  b.now(),
		decided:    make(chan decision, 1),
	}
	b.pending[id] = ap
	b.mu.Unlock()
	return id
}

// wait blocks until a decision arrives or the timeout fires, applying the given
// default on timeout/abort. It always removes the pending entry before returning.
func (b *approvalBridge) wait(id, sessionID string, def decision) decision {
	ap := b.get(id)
	if ap == nil {
		return def
	}
	defer b.remove(id)

	var timerCh <-chan time.Time
	if b.timeout > 0 {
		t := time.NewTimer(b.timeout)
		defer t.Stop()
		timerCh = t.C
	}

	select {
	case d := <-ap.decided:
		return d
	case <-timerCh:
		// Timed out with no connected client: deny (safe default).
		return def
	}
}

func (b *approvalBridge) get(id string) *pendingApproval {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.pending[id]
}

func (b *approvalBridge) remove(id string) {
	b.mu.Lock()
	delete(b.pending, id)
	b.mu.Unlock()
}

// resolve delivers a decision to the waiting tool goroutine for the given id.
// It returns false if there is no pending approval with that id (already
// resolved, timed out, or unknown).
func (b *approvalBridge) resolve(id string, d decision) bool {
	ap := b.get(id)
	if ap == nil {
		return false
	}
	select {
	case ap.decided <- d:
		return true
	default:
		return false // already resolved
	}
}

// list returns a snapshot of all pending approvals as views.
func (b *approvalBridge) list() []approvalView {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]approvalView, 0, len(b.pending))
	for _, ap := range b.pending {
		out = append(out, ap.toView())
	}
	return out
}

func (ap *pendingApproval) toView() approvalView {
	return approvalView{
		ID:         ap.id,
		Kind:       ap.kind,
		SessionID:  ap.sessionID,
		AgentID:    ap.agentID,
		Permission: ap.permission,
		EditReview: ap.editReview,
		CreatedAt:  ap.createdAt.UTC().Format(time.RFC3339),
	}
}

// parsePermDecision maps a wire decision string for a permission prompt to the
// permission.Decision it carries. Unknown strings default to deny.
func parsePermDecision(s string) permission.Decision {
	switch s {
	case "allow":
		return permission.DecisionAllow
	case "always":
		return permission.DecisionAlways
	case "always_deny":
		return permission.DecisionAlwaysDeny
	case "deny", "":
		return permission.DecisionDeny
	}
	return permission.DecisionDeny
}

// parseEditDecision maps a wire decision string for an edit review to the
// gogent.EditReviewDecision it carries. Unknown strings default to reject.
func parseEditDecision(s string) gogent.EditReviewDecision {
	switch s {
	case "approve":
		return gogent.EditApprove
	case "approve_all":
		return gogent.EditApproveAll
	case "reject", "":
		return gogent.EditReject
	}
	return gogent.EditReject
}

// scopePermissionChecker adapts (*Server).requireHuman into webapi's
// PermissionChecker interface, so endpoints can declare Permissions to gate to
// the human scope without each handler re-checking.
type scopePermissionChecker struct {
	provider *composingProvider
}

func (c scopePermissionChecker) HasPermission(_ context.Context, userID int64, perm string) (bool, error) {
	return scopeRank(c.provider.scopeOf(userID)) >= scopeRank(scopeFor(perm)), nil
}

// scopeFor maps a permission key to the minimum scope that satisfies it.
func scopeFor(perm string) authScope {
	if perm == permHuman {
		return scopeHuman
	}
	return scopePeer
}

const permHuman = "human"

// requireHuman is a handler-level guard for endpoints that must not be reachable
// by a peer-scoped token (settings, models, shutdown, listing all sessions). It
// returns nil when the caller is human-scoped, else an HTTPError.
func requireHuman(r *http.Request, provider *composingProvider) *webapi.HTTPError {
	user, ok := webapi.GetUser(r.Context())
	if !ok || user == nil {
		return webapi.NewHTTPError(http.StatusUnauthorized, "authentication required")
	}
	if scopeRank(provider.scopeOf(user.ID)) < scopeRank(scopeHuman) {
		return webapi.NewHTTPError(http.StatusForbidden, "human scope required")
	}
	return nil
}
