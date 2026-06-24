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
//
// The applicable bound tracks the live connected-client count continuously
// (issue #358 §8): connectedTimeout governs while a client is connected (the
// connected-but-unresponsive auto-deny), unattendedTimeout governs while none is
// — each measuring only continuous time in its own state and resetting on the
// opposite transition. So a daemon whose TUI blips offline keeps its long watcher
// turns alive, on reconnect a client picks the prompt up via GET /approvals with
// a fresh grace window, and the unattended bound never alters the connected case
// (connectedTimeout == 0 still means "never"). A delivered decision always wins.
// See wait for the exact accrual rules.
type approvalBridge struct {
	hub *hub
	// connectedTimeout bounds the wait when a human client IS connected (it could
	// answer but is unresponsive); 0 means never (block forever).
	connectedTimeout time.Duration
	// unattendedTimeout bounds the wait when NO client is connected; 0 means
	// never (block forever). It is normally much longer than connectedTimeout.
	unattendedTimeout time.Duration
	now               func() time.Time

	mu      sync.Mutex
	pending map[string]*pendingApproval
	nextSeq int64
}

func newApprovalBridge(h *hub, connectedTimeout, unattendedTimeout time.Duration, now func() time.Time) *approvalBridge {
	if now == nil {
		now = time.Now
	}
	// Sanity floor: the unattended bound should grant at least as much grace as
	// the connected one (it is meant to be the *longer* safety bound — default 1h
	// vs 5 min). Normalize a nonsensical config where unattended < connected so an
	// unattended prompt never dies sooner than a connected one would (issue #358
	// §8). A non-positive bound means "wait forever" for that state and is left
	// as-is. (The connected case is independent of this regardless: wait() holds
	// the unattended clock at zero while a client is connected.)
	if connectedTimeout > 0 && unattendedTimeout > 0 && unattendedTimeout < connectedTimeout {
		unattendedTimeout = connectedTimeout
	}
	return &approvalBridge{
		hub:               h,
		connectedTimeout:  connectedTimeout,
		unattendedTimeout: unattendedTimeout,
		now:               now,
		pending:           make(map[string]*pendingApproval),
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

// alloc registers a new pending approval and returns its id. Clients discover it
// by polling GET /approvals (there is no SSE push for approvals); the blocked
// tool goroutine then waits in wait until a decision arrives or a bound fires.
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

// wait blocks until a decision arrives or a timeout fires, applying the given
// default on timeout/abort. It always removes the pending entry before returning.
//
// The bound that applies is the *effective* timeout for the prompt's current
// connection state (issue #358 §8), evaluated continuously — not snapshotted once
// — so a transient disconnect mid-prompt is handled correctly. Each bound tracks
// only the continuous wall-time spent in its own state and RESETS on the opposite
// transition:
//
//   - While a client is connected, connectedTimeout governs — the
//     connected-but-unresponsive auto-deny (default 5 min). The unattended clock
//     is held at zero, so the unattended cap can never shorten or alter the
//     connected case (in particular connectedTimeout == 0 still means "never").
//   - While no client is connected, unattendedTimeout governs (default 1h), so a
//     daemon whose TUI blips offline keeps the prompt alive and a reconnecting
//     client gets a fresh connected grace window to answer it.
//
// A non-positive bound disables auto-deny for that state ("0 = wait forever");
// with both disabled the prompt blocks until a decision arrives.
func (b *approvalBridge) wait(id, sessionID string, def decision) decision {
	ap := b.get(id)
	if ap == nil {
		return def
	}
	defer b.remove(id)

	// Fast path: no bounds means block forever on a decision (no polling needed).
	if b.connectedTimeout <= 0 && b.unattendedTimeout <= 0 {
		return <-ap.decided
	}

	lastTick := time.Now()
	var connectedFor, unattendedFor time.Duration // continuous time in each state

	ticker := time.NewTicker(b.pollInterval())
	defer ticker.Stop()

	for {
		select {
		case d := <-ap.decided:
			return d
		case now := <-ticker.C:
			delta := now.Sub(lastTick)
			lastTick = now
			if b.hub != nil && b.hub.clientCount() > 0 {
				// Connected: accrue the connected clock, reset the unattended one.
				connectedFor += delta
				unattendedFor = 0
				if b.connectedTimeout > 0 && connectedFor >= b.connectedTimeout {
					return def
				}
			} else {
				// Unattended: accrue the unattended clock, reset the connected one.
				unattendedFor += delta
				connectedFor = 0
				if b.unattendedTimeout > 0 && unattendedFor >= b.unattendedTimeout {
					return def
				}
			}
		}
	}
}

// pollInterval is how often wait() re-checks the connected-client count and the
// elapsed bounds. It is a quarter of the smaller active bound (so the bound is
// honored responsively) but capped at 1s so a long unattended wait does not
// busy-spin, and floored at 1ms so a tiny test bound stays well-behaved.
func (b *approvalBridge) pollInterval() time.Duration {
	const maxPoll = time.Second
	const minPoll = time.Millisecond
	cand := maxPoll
	if b.connectedTimeout > 0 && b.connectedTimeout/4 < cand {
		cand = b.connectedTimeout / 4
	}
	if b.unattendedTimeout > 0 && b.unattendedTimeout/4 < cand {
		cand = b.unattendedTimeout / 4
	}
	if cand < minPoll {
		cand = minPoll
	}
	return cand
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
