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
	// observed is set true the first time a client fetches this approval via
	// GET /approvals (b.list). Until then no attached client has had the chance to
	// surface the prompt, so the connected-but-unresponsive auto-deny must NOT be
	// charged against this un-presented time (issue #569): wait holds the connected
	// clock at zero while !observed, governing the prompt by the longer unattended
	// safety bound instead. Guarded by b.mu.
	observed bool
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
// (issue #358 §8): connectedTimeout governs while a client is connected AND has
// observed the prompt (the connected-but-unresponsive auto-deny), unattendedTimeout
// governs otherwise — each measuring only continuous time in its own state and
// resetting on the opposite transition. So a daemon whose TUI blips offline keeps
// its long watcher turns alive, on reconnect a client picks the prompt up via GET
// /approvals with a fresh grace window, and the unattended bound never alters the
// OBSERVED connected case (where connectedTimeout == 0 still means "never"). An
// un-observed connected prompt is governed by the unattended bound instead (see
// below), so connectedTimeout == 0 leaves it to that bound rather than blocking
// forever. A delivered decision always wins.
//
// Crucially the connected clock is charged only against time AFTER a client has
// fetched the prompt (issue #569): an approval raised while the TUI is briefly
// disconnected, or before the first GET /approvals poll lands, is never
// auto-denied before the attached TUI can surface its ⏳ badge + dialog. See wait
// for the exact accrual rules.
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

	// recent remembers approvals that were already removed (resolved or timed out)
	// so a decision POST that lands AFTER removal can be reconciled idempotently
	// instead of hard-404ing and losing the user's answer (issue #560). It is
	// bounded by recentCap with FIFO eviction (recentOrder); entries are tiny.
	recent      map[string]concludedApproval
	recentOrder []string
}

// recentCap bounds the recall ring (issue #560). It is far above any realistic
// number of approvals in flight at once, so a late decision is essentially always
// reconcilable; an evicted id falls back to a 404 the client retries then surfaces.
const recentCap = 64

// concludedApproval is the compact record kept for a removed approval so a late
// decision POST can still be applied (a sticky permission grant) or acknowledged
// idempotently (issue #560).
type concludedApproval struct {
	kind       string
	sessionID  string
	agentID    string
	permission *permissionDetail
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
		recent:            make(map[string]concludedApproval),
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
// by polling GET /approvals; the blocked tool goroutine then waits in wait until a
// decision arrives or a bound fires. To shorten discovery latency (issue #569) a
// best-effort SSE "approval" signal is broadcast to connected global subscribers
// so an attached client re-fetches /approvals immediately rather than waiting for
// its next poll tick. The broadcast is non-blocking and the poll remains the
// authoritative backstop, so a dropped signal never loses the prompt.
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
	// Nudge connected clients to re-scan now (outside b.mu: broadcast takes the hub
	// lock, and wait reads clientCount then isObserved sequentially, so the two
	// locks are never nested in either order).
	if b.hub != nil {
		b.hub.broadcastApprovalSignal(id)
	}
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
//   - While a client is connected AND has observed the prompt (fetched it via GET
//     /approvals), connectedTimeout governs — the connected-but-unresponsive
//     auto-deny (default 5 min). The unattended clock is held at zero, so the
//     unattended cap can never shorten or alter the connected case (in particular
//     connectedTimeout == 0 still means "never").
//   - Otherwise — no client connected, OR a client is connected but has not yet
//     fetched the prompt — unattendedTimeout governs (default 1h) and the
//     connected clock is held at zero. This keeps the prompt alive across a TUI
//     blip, AND keeps an approval raised before the first poll from being
//     auto-denied before any client has had the chance to surface it (issue #569);
//     a reconnecting/first-polling client then gets a fresh connected grace window.
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
			if b.hub != nil && b.hub.clientCount() > 0 && b.isObserved(id) {
				// Connected AND a client has fetched the prompt: the
				// connected-but-unresponsive auto-deny governs. Accrue the connected
				// clock, reset the unattended one.
				connectedFor += delta
				unattendedFor = 0
				if b.connectedTimeout > 0 && connectedFor >= b.connectedTimeout {
					return b.expireDeny(id, sessionID, def)
				}
			} else {
				// Either no client is connected, or a client is connected but has not
				// yet observed the prompt (so no human has had the chance to answer):
				// the longer unattended safety bound governs and the connected clock is
				// held at zero (issue #569). Accrue the unattended clock, reset the
				// connected one.
				unattendedFor += delta
				connectedFor = 0
				if b.unattendedTimeout > 0 && unattendedFor >= b.unattendedTimeout {
					return b.expireDeny(id, sessionID, def)
				}
			}
		}
	}
}

// expireDeny is the timeout return path of wait: it applies the safe default but
// first, for a PRESENTED (observed) prompt, broadcasts a best-effort "approval
// expired" signal so connected clients can tell the user the prompt timed out
// before it was answered — so a late click on a still-open dialog is not silently
// ignored (issue #569). It fires only for an observed prompt: an un-presented one
// showed no dialog, so there is nothing to retract. Unlike the late-decision
// reconcile path (which cannot tell a timeout from another client answering), this
// fires ONLY on a genuine timeout, so the surfaced notice is cause-accurate. The
// broadcast reaches connected global subscribers only; with none connected it is a
// no-op (and there is no human to inform anyway).
func (b *approvalBridge) expireDeny(id, sessionID string, def decision) decision {
	if b.hub != nil && b.isObserved(id) {
		b.hub.broadcastApprovalExpired(id, sessionID)
	}
	return def
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

// isObserved reports whether a client has fetched the pending approval at least
// once via GET /approvals (issue #569). A removed/unknown id is reported false.
// wait reads it each tick to decide whether the connected-but-unresponsive
// auto-deny clock may accrue.
func (b *approvalBridge) isObserved(id string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	ap := b.pending[id]
	return ap != nil && ap.observed
}

func (b *approvalBridge) remove(id string) {
	b.mu.Lock()
	if ap, ok := b.pending[id]; ok {
		b.rememberLocked(id, concludedApproval{
			kind:       ap.kind,
			sessionID:  ap.sessionID,
			agentID:    ap.agentID,
			permission: ap.permission,
		})
		delete(b.pending, id)
	}
	b.mu.Unlock()
}

// rememberLocked records a concluded approval in the bounded recall ring, evicting
// the oldest entry when full. Caller holds b.mu.
func (b *approvalBridge) rememberLocked(id string, c concludedApproval) {
	if _, exists := b.recent[id]; !exists {
		b.recentOrder = append(b.recentOrder, id)
		for len(b.recentOrder) > recentCap {
			oldest := b.recentOrder[0]
			b.recentOrder = b.recentOrder[1:]
			delete(b.recent, oldest)
		}
	}
	b.recent[id] = c
}

// recall returns the record of a removed approval, if it is still in the ring. A
// decision POST that arrives after the pending approval was removed uses it to
// reconcile idempotently rather than hard-404ing (issue #560).
func (b *approvalBridge) recall(id string) (concludedApproval, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	c, ok := b.recent[id]
	return c, ok
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

// list returns a snapshot of all pending approvals as views. Fetching the list is
// what marks each returned approval "observed" (issue #569): a successful GET
// /approvals is precisely the moment an attached client has the prompt in hand and
// can surface its badge/dialog, so from here the connected-but-unresponsive
// auto-deny clock is allowed to run (see wait). The only production caller is the
// GET /approvals handler; the test helpers that call it simulate that same client
// poll.
func (b *approvalBridge) list() []approvalView {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]approvalView, 0, len(b.pending))
	for _, ap := range b.pending {
		ap.observed = true
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
