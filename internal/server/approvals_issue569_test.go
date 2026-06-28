package server

// Issue #569 — server half of the fix.
//
// The connected-but-unresponsive auto-deny used to start accruing the instant a
// prompt was allocated, before any client had fetched it via GET /approvals. A
// prompt raised during a brief TUI disconnect, or before the first 750ms poll
// landed, could therefore be auto-denied and removed before handleApproval ever
// ran → no badge, no dialog, a silent DecisionDeny.
//
// Layer A: wait() now charges the connected clock ONLY once a client has fetched
// the prompt (list() marks it "observed"); until then the longer unattended
// safety bound governs, so the prompt cannot vanish before it is surfaced.
// Layer B: alloc() broadcasts a best-effort "approval" SSE nudge to connected
// global subscribers so a client re-fetches /approvals immediately.
//
// These tests cover the daemon side of both layers plus the existing path that
// must keep working (an OBSERVED connected prompt still auto-denies on schedule).

import (
	"encoding/json"
	"testing"
	"time"

	"gogent/internal/agent"
	"gogent/internal/gogent"
	"gogent/internal/permission"
)

// --- Layer A: the connected clock is not charged against un-presented time ----

// TestApprovalIssue569ConnectedUnobservedNotDeniedBeforeFetch is the headline
// acceptance test (criteria 1 & 2): a connected client whose poll has NOT yet
// observed the prompt must NOT lose it to the 5-min connected auto-deny. The
// prompt stays pending well past connectedTimeout; only once a client fetches it
// (list) does the connected clock start and auto-deny on schedule.
func TestApprovalIssue569ConnectedUnobservedNotDeniedBeforeFetch(t *testing.T) {
	h := newHub()
	_, unsub := h.subscribeGlobal() // a client IS connected (clientCount > 0)
	defer unsub()
	if got := h.clientCount(); got != 1 {
		t.Fatalf("clientCount = %d, want 1", got)
	}
	// connectedTimeout short, unattendedTimeout much longer (the safety bound that
	// must govern while the prompt is un-presented).
	bridge := newApprovalBridge(h, 50*time.Millisecond, 5*time.Second, time.Now)

	// alloc directly (not via AskPermission) so we hold the id and can inspect
	// pendingness WITHOUT observing: get() does not mark observed, only list() does.
	id := bridge.alloc("permission", "s1", "root",
		&permissionDetail{Action: "shell", Resource: "rm -rf /x", Detail: "x"}, nil)
	done := make(chan decision, 1)
	go func() { done <- bridge.wait(id, "s1", decision{perm: permission.DecisionDeny}) }()

	// Wait well past the 50ms connectedTimeout WITHOUT ever fetching the prompt.
	time.Sleep(200 * time.Millisecond)
	if got := bridge.get(id); got == nil {
		t.Fatal("un-presented connected approval was removed before any client fetched it")
	}
	select {
	case d := <-done:
		t.Fatalf("un-presented connected approval was auto-denied with %v; want still pending", d.perm)
	default:
	}

	// Now a client fetches (observes) the prompt: the connected clock starts.
	bridge.list()

	// It must now auto-deny within roughly the connected window.
	select {
	case d := <-done:
		if d.perm != permission.DecisionDeny {
			t.Fatalf("post-observation auto-deny = %v, want deny (the safe default)", d.perm)
		}
	case <-time.After(time.Second):
		t.Fatal("observed connected approval did not auto-deny within the connected window")
	}
	if got := bridge.get(id); got != nil {
		t.Fatalf("approval %q was not removed after the connected auto-deny", id)
	}
}

// TestApprovalIssue569ConnectedUnobservedAppliesToEditReview confirms Layer A is
// not permission-specific: an edit-review gate raised before the first fetch is
// likewise not auto-denied before presentation.
func TestApprovalIssue569ConnectedUnobservedAppliesToEditReview(t *testing.T) {
	h := newHub()
	_, unsub := h.subscribeGlobal()
	defer unsub()
	bridge := newApprovalBridge(h, 40*time.Millisecond, 5*time.Second, time.Now)

	id := bridge.alloc("edit_review", "s1", "root", nil,
		&editReviewDetail{Path: "a.go", Op: "write", Diff: "-old\n+new"})
	done := make(chan decision, 1)
	go func() { done <- bridge.wait(id, "s1", decision{edit: gogent.EditReject}) }()

	time.Sleep(160 * time.Millisecond) // >> 40ms connectedTimeout, never fetched
	if got := bridge.get(id); got == nil {
		t.Fatal("un-presented edit-review approval was removed before any client fetched it")
	}
	select {
	case d := <-done:
		t.Fatalf("un-presented edit-review approval auto-rejected with %v; want still pending", d.edit)
	default:
	}
	// Clean up so the goroutine does not linger.
	bridge.resolve(id, decision{edit: gogent.EditApprove})
}

// TestApprovalIssue569ObservedConnectedClientDeniesAtConnectedTimeout is the
// regression guard for the unchanged half of Layer A: once a client HAS fetched
// the prompt, the normal connected-but-unresponsive auto-deny applies on the same
// schedule as before (issue #358).
func TestApprovalIssue569ObservedConnectedClientDeniesAtConnectedTimeout(t *testing.T) {
	h := newHub()
	_, unsub := h.subscribeGlobal()
	defer unsub()
	bridge := newApprovalBridge(h, 50*time.Millisecond, 5*time.Second, time.Now)

	id := bridge.alloc("permission", "s1", "root",
		&permissionDetail{Action: "shell", Resource: "r"}, nil)
	done := make(chan decision, 1)
	go func() { done <- bridge.wait(id, "s1", decision{perm: permission.DecisionDeny}) }()

	bridge.list() // observe immediately

	select {
	case d := <-done:
		if d.perm != permission.DecisionDeny {
			t.Fatalf("observed connected auto-deny = %v, want deny", d.perm)
		}
	case <-time.After(time.Second):
		t.Fatal("observed connected approval did not auto-deny within the connected window")
	}
}

// TestApprovalIssue569UnobservedChargedToUnattendedSafetyBound confirms the
// un-presented time is not simply ignored: while un-observed it accrues against
// the unattended bound, so a prompt no client ever fetches still denies
// eventually (the safety net is intact). connectedTimeout=0 means the connected
// clock never denies, isolating the unattended path.
func TestApprovalIssue569UnobservedChargedToUnattendedSafetyBound(t *testing.T) {
	h := newHub()
	_, unsub := h.subscribeGlobal() // connected, but never fetches
	defer unsub()
	bridge := newApprovalBridge(h, 0, 80*time.Millisecond, time.Now)

	id := bridge.alloc("permission", "s1", "root",
		&permissionDetail{Action: "shell", Resource: "r"}, nil)
	done := make(chan decision, 1)
	go func() { done <- bridge.wait(id, "s1", decision{perm: permission.DecisionDeny}) }()

	select {
	case d := <-done:
		if d.perm != permission.DecisionDeny {
			t.Fatalf("un-observed safety deny = %v, want deny", d.perm)
		}
	case <-time.After(time.Second):
		t.Fatal("un-observed approval was never denied by the unattended safety bound")
	}
}

// TestApprovalIssue569ObservedConnectedTimeoutZeroNeverDenies locks in the
// documented split (approvals.go): connectedTimeout == 0 means "never" for an
// OBSERVED connected prompt (a human fetched it and could answer) — the connected
// clock must not deny it, and because it is observed the unattended clock is held
// at zero while connected. The un-observed connected case is the one governed by
// the unattended bound (TestApprovalIssue569UnobservedChargedToUnattendedSafetyBound).
func TestApprovalIssue569ObservedConnectedTimeoutZeroNeverDenies(t *testing.T) {
	h := newHub()
	_, unsub := h.subscribeGlobal() // connected
	defer unsub()
	// connectedTimeout=0 (never) with a short unattended bound, so a spurious deny
	// would show up quickly.
	bridge := newApprovalBridge(h, 0, 60*time.Millisecond, time.Now)

	id := bridge.alloc("permission", "s1", "root", &permissionDetail{Action: "shell", Resource: "r"}, nil)
	done := make(chan decision, 1)
	go func() { done <- bridge.wait(id, "s1", decision{perm: permission.DecisionDeny}) }()

	bridge.list() // observe → connectedTimeout=0 must now mean "never deny"

	// Well past the 60ms unattended bound: an OBSERVED connected prompt must NOT be
	// denied by either clock.
	time.Sleep(160 * time.Millisecond)
	select {
	case d := <-done:
		t.Fatalf("observed connected prompt with connectedTimeout=0 was denied with %v; want to block until a decision", d.perm)
	default:
	}
	if bridge.get(id) == nil {
		t.Fatal("observed connected approval was removed despite connectedTimeout=0 (never)")
	}
	bridge.resolve(id, decision{perm: permission.DecisionAllow})
}

// TestApprovalIssue569IsObservedHelper exercises the helper wait() reads each tick.
func TestApprovalIssue569IsObservedHelper(t *testing.T) {
	h := newHub()
	bridge := newApprovalBridge(h, time.Minute, time.Minute, time.Now)
	id := bridge.alloc("permission", "s1", "root", &permissionDetail{Action: "shell"}, nil)

	if bridge.isObserved(id) {
		t.Fatal("freshly allocated approval reported observed before any fetch")
	}
	if bridge.isObserved("apr_does_not_exist") {
		t.Fatal("unknown id reported observed")
	}
	bridge.list()
	if !bridge.isObserved(id) {
		t.Fatal("approval not observed after list()")
	}
	// A second list does not flip it back off — observation is sticky.
	bridge.list()
	if !bridge.isObserved(id) {
		t.Fatal("observation should be sticky across repeated list() calls")
	}
}

// TestApprovalIssue569ListSideEffectDoesNotBreakUnattendedPath guards the
// documented side effect: list() marks observed even when no client is connected
// (the #358 unattended tests call list() to discover). Because the connected
// branch requires BOTH connected AND observed, marking observed while unattended
// must not switch the prompt onto the (shorter) connected clock.
func TestApprovalIssue569ListSideEffectDoesNotBreakUnattendedPath(t *testing.T) {
	h := newHub()
	bridge := newApprovalBridge(h, 20*time.Millisecond, 400*time.Millisecond, time.Now)
	// No subscriber: clientCount == 0.

	id := bridge.alloc("permission", "s1", "root", &permissionDetail{Action: "shell"}, nil)
	done := make(chan decision, 1)
	go func() { done <- bridge.wait(id, "s1", decision{perm: permission.DecisionDeny}) }()

	bridge.list() // observed=true, but still not connected

	// Must NOT deny at the 20ms connected bound (connected is false). It must stay
	// pending until the 400ms unattended bound.
	time.Sleep(80 * time.Millisecond)
	select {
	case d := <-done:
		t.Fatalf("observed-but-unconnected approval denied at %v with connected-bound timing; want unattended", d.perm)
	default:
	}
	bridge.resolve(id, decision{perm: permission.DecisionAllow})
}

// --- Layer B (daemon): the approval SSE nudge -------------------------------

// recvApprovalSignalIssue569 reads one approval-signal frame from a subscriber,
// failing if it is not an approval signal or does not arrive in time.
func recvApprovalSignalIssue569(t *testing.T, sub <-chan taggedEvent) taggedEvent {
	t.Helper()
	select {
	case te := <-sub:
		if !te.approvalSignal {
			t.Fatalf("received non-approval-signal frame: %+v", te)
		}
		return te
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for approval signal frame")
	}
	return taggedEvent{}
}

// TestApprovalIssue569AllocBroadcastsApprovalSignalToGlobalSubscriber confirms
// alloc() pushes a best-effort "approval" nudge carrying the approval id to every
// connected global subscriber (Layer B).
func TestApprovalIssue569AllocBroadcastsApprovalSignalToGlobalSubscriber(t *testing.T) {
	h := newHub()
	sub, unsub := h.subscribeGlobal()
	defer unsub()

	id := h_broadcastingBridgeAlloc(t, h, "apr_xyz") // alloc via a real bridge so alloc() runs

	te := recvApprovalSignalIssue569(t, sub)
	if te.approvalID != id {
		t.Fatalf("signal approvalID = %q, want %q", te.approvalID, id)
	}
	// It must not also carry a notification or session event (mutually exclusive).
	if te.notif != nil || te.ev.Type != "" {
		t.Fatalf("approval signal frame carries extra payload: %+v", te)
	}
}

// TestApprovalIssue569ApprovalSignalGlobalOnlyNotSessionSubscribers confirms the
// nudge rides the global stream only, never per-session subscribers (the approval
// list is a global resource), mirroring notifications.
func TestApprovalIssue569ApprovalSignalGlobalOnlyNotSessionSubscribers(t *testing.T) {
	h := newHub()
	sessSub, sessUnsub := h.subscribeSession("s1")
	defer sessUnsub()

	h_broadcastingBridgeAlloc(t, h, "apr_global_only")

	select {
	case te := <-sessSub:
		t.Fatalf("per-session subscriber received an approval signal: %+v", te)
	case <-time.After(40 * time.Millisecond):
	}
}

// TestApprovalIssue569BroadcastApprovalSignalNonBlockingDropOnFull confirms the
// nudge never blocks the agent/alloc path: with a subscriber buffer completely
// full, broadcastApprovalSignal returns promptly (drop-on-full).
func TestApprovalIssue569BroadcastApprovalSignalNonBlockingDropOnFull(t *testing.T) {
	h := newHub()
	sub, unsub := h.subscribeGlobal()
	defer unsub()
	// Fill the global subscriber's buffered channel (cap 128) with approval frames.
	for i := 0; i < cap(sub); i++ {
		h.broadcastApprovalSignal("fill")
	}

	done := make(chan struct{})
	go func() {
		h.broadcastApprovalSignal("dropped") // buffer full: must drop, not block
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("broadcastApprovalSignal blocked on a full subscriber buffer (must be drop-on-full)")
	}
}

// TestApprovalIssue569BroadcastApprovalSignalNoSubscribersNoPanic confirms a
// broadcast with no subscribers is a harmless no-op.
func TestApprovalIssue569BroadcastApprovalSignalNoSubscribersNoPanic(t *testing.T) {
	h := newHub()
	h.broadcastApprovalSignal("apr_none") // no global subscribers registered
}

// TestApprovalIssue569SignalNotReplayedToLateSubscriber confirms the approval nudge
// is NOT ring-buffered (unlike notifications): a client that subscribes AFTER
// alloc() does not receive the past nudge. Correctness does not depend on it — the
// reconnect kick and the authoritative /approvals poll backstop discovery — so the
// signal stays unbuffered to avoid a stale wake after the approval resolved.
func TestApprovalIssue569SignalNotReplayedToLateSubscriber(t *testing.T) {
	h := newHub()
	h_broadcastingBridgeAlloc(t, h, "apr_before") // nudge fires before any subscriber
	sub, unsub := h.subscribeGlobal()             // subscribe after alloc
	defer unsub()
	select {
	case te := <-sub:
		if te.approvalSignal {
			t.Fatalf("late subscriber received an un-buffered approval signal: %+v", te)
		}
		// A buffered notification (none here) would be fine; only an approval signal is wrong.
	case <-time.After(40 * time.Millisecond):
	}
}

// TestApprovalIssue569AllocWithNilHubDoesNotPanic confirms the alloc nudge is
// nil-hub guarded (some narrow tests construct a bridge with no hub).
func TestApprovalIssue569AllocWithNilHubDoesNotPanic(t *testing.T) {
	bridge := newApprovalBridge(nil, time.Minute, time.Minute, time.Now)
	// Reaching here without panicking is the assertion.
	_ = bridge.alloc("permission", "s1", "root", &permissionDetail{Action: "shell"}, nil)
}

// TestApprovalIssue569GlobalSSESerializesApprovalFrame confirms globalSSE emits
// the approval-signal frame under the dedicated "approval" event name with the id
// payload, distinct from both session events and notifications.
func TestApprovalIssue569GlobalSSESerializesApprovalFrame(t *testing.T) {
	ev := globalSSE(taggedEvent{approvalSignal: true, approvalID: "apr_42"})
	if ev.Name != approvalEventName {
		t.Fatalf("SSE event name = %q, want %q", ev.Name, approvalEventName)
	}
	var v approvalSignalView
	if err := json.Unmarshal([]byte(ev.Data), &v); err != nil {
		t.Fatalf("approval frame data is not valid JSON (%q): %v", ev.Data, err)
	}
	if v.ID != "apr_42" {
		t.Fatalf("approval frame id = %q, want apr_42", v.ID)
	}
}

// TestApprovalIssue569ApprovalEventNameDistinctFromSessionEvents guards the
// design claim that "approval" collides with no agent SessionEventType, so a
// client can route a frame by its event name alone.
func TestApprovalIssue569ApprovalEventNameDistinctFromSessionEvents(t *testing.T) {
	sessionEventNames := []string{
		string(agent.SessionEventThinking),
		string(agent.SessionEventAssistantStep),
		string(agent.SessionEventToolCall),
		string(agent.SessionEventToolResult),
		string(agent.SessionEventFinal),
		string(agent.SessionEventError),
		string(agent.SessionEventNotice),
		string(agent.SessionEventSubAgent),
		string(agent.SessionEventCompaction),
		string(agent.SessionEventUsage),
		string(agent.SessionEventTodo),
		string(agent.SessionEventPlan),
		string(agent.SessionEventYolo),
		string(agent.SessionEventBackground),
	}
	for _, n := range sessionEventNames {
		if n == approvalEventName {
			t.Fatalf("approval event name %q collides with SessionEventType %q — a client could not route it by name", approvalEventName, n)
		}
	}
}

// h_broadcastingBridgeAlloc builds a real bridge over h and allocs a permission
// approval, returning its id. Using the bridge (rather than calling
// broadcastApprovalSignal directly) exercises the alloc() nudge wiring.
func h_broadcastingBridgeAlloc(t *testing.T, h *hub, _ string) string {
	t.Helper()
	bridge := newApprovalBridge(h, time.Minute, time.Minute, time.Now)
	return bridge.alloc("permission", "s1", "root", &permissionDetail{Action: "shell", Resource: "r"}, nil)
}

// --- fixes-round-2: the "approval_expired" timeout signal (Layer C, server) ---
//
// When a PRESENTED (observed) prompt reaches its auto-deny timeout, wait() calls
// expireDeny, which broadcasts an "approval_expired" signal so a connected client
// can tell the user the prompt timed out and the safe default was applied —
// closing the silent-late-click gap (issue #569). It fires ONLY for an observed
// prompt (an un-presented one showed no dialog) and ONLY on a genuine timeout, so
// the surfaced notice is cause-accurate (unlike the reportDecision "late" path,
// which cannot tell a timeout from another client answering).

// recvApprovalExpiredIssue569 drains frames from a subscriber until it sees an
// approval-expired one (skipping any approval-signal frames), failing on timeout.
func recvApprovalExpiredIssue569(t *testing.T, sub <-chan taggedEvent) taggedEvent {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		select {
		case te := <-sub:
			if te.approvalExpired {
				return te
			}
			// skip approval-signal / session-event frames
		case <-deadline:
			t.Fatal("timed out waiting for an approval_expired frame")
			return taggedEvent{}
		}
	}
}

// TestApprovalIssue569ObservedConnectedTimeoutBroadcastsExpiredAndDenies is the
// headline: a prompt a client has fetched (observed) that reaches the connected
// auto-deny must (a) return the safe deny default AND (b) push an approval_expired
// signal carrying its id+session so the client can tell the user.
func TestApprovalIssue569ObservedConnectedTimeoutBroadcastsExpiredAndDenies(t *testing.T) {
	h := newHub()
	sub, unsub := h.subscribeGlobal()
	defer unsub()
	bridge := newApprovalBridge(h, 50*time.Millisecond, 5*time.Second, time.Now)

	id := bridge.alloc("permission", "sess-1", "root",
		&permissionDetail{Action: "shell", Resource: "r"}, nil)
	done := make(chan decision, 1)
	go func() { done <- bridge.wait(id, "sess-1", decision{perm: permission.DecisionDeny}) }()

	bridge.list() // observe → the connected clock now governs

	select {
	case d := <-done:
		if d.perm != permission.DecisionDeny {
			t.Fatalf("observed connected timeout decision = %v, want deny (safe default)", d.perm)
		}
	case <-time.After(time.Second):
		t.Fatal("observed connected approval did not auto-deny within the connected window")
	}

	te := recvApprovalExpiredIssue569(t, sub)
	if te.approvalID != id {
		t.Fatalf("expired signal approvalID = %q, want %q", te.approvalID, id)
	}
	if te.sessionID != "sess-1" {
		t.Fatalf("expired signal sessionID = %q, want sess-1", te.sessionID)
	}
}

// TestApprovalIssue569UnobservedTimeoutDoesNotBroadcastExpired confirms an
// UN-presented prompt that times out does NOT emit an approval_expired signal — no
// human ever saw a dialog, so there is nothing to retract and nobody to tell.
func TestApprovalIssue569UnobservedTimeoutDoesNotBroadcastExpired(t *testing.T) {
	h := newHub()
	sub, unsub := h.subscribeGlobal()
	defer unsub()
	// connectedTimeout=0 (never) + short unattended bound; a subscriber is connected
	// but never fetches, so the prompt is un-observed and the unattended clock denies.
	bridge := newApprovalBridge(h, 0, 50*time.Millisecond, time.Now)

	id := bridge.alloc("permission", "sess-1", "root", &permissionDetail{Action: "shell"}, nil)
	done := make(chan decision, 1)
	go func() { done <- bridge.wait(id, "sess-1", decision{perm: permission.DecisionDeny}) }()

	select {
	case <-done: // denied at the unattended bound
	case <-time.After(time.Second):
		t.Fatal("un-observed approval did not deny at the unattended bound")
	}

	// Drain any buffered frames: only the alloc approval-signal should be present,
	// never an approval_expired frame.
	time.Sleep(30 * time.Millisecond) // let a straggler arrive, if any
	for {
		select {
		case te := <-sub:
			if te.approvalExpired {
				t.Fatalf("un-presented timeout emitted an approval_expired signal: %+v", te)
			}
		default:
			return // drained, no expired frame — correct
		}
	}
}

// TestApprovalIssue569BroadcastApprovalExpiredGlobalOnlyNotSession confirms the
// expired signal rides the global stream only, never per-session subscribers.
func TestApprovalIssue569BroadcastApprovalExpiredGlobalOnlyNotSession(t *testing.T) {
	h := newHub()
	sessSub, sessUnsub := h.subscribeSession("s1")
	defer sessUnsub()

	h.broadcastApprovalExpired("apr_1", "s1")

	select {
	case te := <-sessSub:
		t.Fatalf("per-session subscriber received an approval_expired signal: %+v", te)
	case <-time.After(40 * time.Millisecond):
	}
}

// TestApprovalIssue569BroadcastApprovalExpiredNonBlockingDropOnFull confirms the
// expired fan-out never blocks the wait()/expireDeny path: with a subscriber buffer
// completely full it returns promptly (drop-on-full).
func TestApprovalIssue569BroadcastApprovalExpiredNonBlockingDropOnFull(t *testing.T) {
	h := newHub()
	sub, unsub := h.subscribeGlobal()
	defer unsub()
	for i := 0; i < cap(sub); i++ {
		h.broadcastApprovalExpired("fill", "s")
	}
	done := make(chan struct{})
	go func() {
		h.broadcastApprovalExpired("dropped", "s") // buffer full: must drop, not block
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("broadcastApprovalExpired blocked on a full subscriber buffer (must be drop-on-full)")
	}
}

// TestApprovalIssue569BroadcastApprovalExpiredNotReplayedToLateSubscriber confirms
// the expired signal is NOT ring-buffered: a client subscribing AFTER the timeout
// does not receive a stale replay (it re-syncs the live /approvals list instead).
func TestApprovalIssue569BroadcastApprovalExpiredNotReplayedToLateSubscriber(t *testing.T) {
	h := newHub()
	h.broadcastApprovalExpired("apr_before", "s1") // fires before any subscriber
	sub, unsub := h.subscribeGlobal()              // subscribe after
	defer unsub()
	select {
	case te := <-sub:
		if te.approvalExpired {
			t.Fatalf("late subscriber received an un-buffered approval_expired signal: %+v", te)
		}
	case <-time.After(40 * time.Millisecond):
	}
}

// TestApprovalIssue569BroadcastApprovalExpiredNilHubNoPanic confirms expireDeny's
// nil-hub guard: a bridge with no hub does not panic on timeout.
func TestApprovalIssue569BroadcastApprovalExpiredNilHubNoPanic(t *testing.T) {
	bridge := newApprovalBridge(nil, 0, 30*time.Millisecond, time.Now)
	id := bridge.alloc("permission", "s1", "root", &permissionDetail{Action: "shell"}, nil)
	// list() observes it (so expireDeny would broadcast on a non-nil hub); with a nil
	// hub it must simply deny without panicking.
	bridge.list()
	done := make(chan decision, 1)
	go func() { done <- bridge.wait(id, "s1", decision{perm: permission.DecisionDeny}) }()
	select {
	case d := <-done:
		if d.perm != permission.DecisionDeny {
			t.Fatalf("nil-hub timeout decision = %v, want deny", d.perm)
		}
	case <-time.After(time.Second):
		t.Fatal("nil-hub observed approval did not deny at the unattended bound")
	}
}

// TestApprovalIssue569GlobalSSESerializesApprovalExpiredFrame confirms globalSSE
// emits the expired frame under the dedicated "approval_expired" name with the
// id+session payload.
func TestApprovalIssue569GlobalSSESerializesApprovalExpiredFrame(t *testing.T) {
	ev := globalSSE(taggedEvent{approvalExpired: true, approvalID: "apr_7", sessionID: "sess-7"})
	if ev.Name != approvalExpiredEventName {
		t.Fatalf("SSE event name = %q, want %q", ev.Name, approvalExpiredEventName)
	}
	var v approvalExpiredView
	if err := json.Unmarshal([]byte(ev.Data), &v); err != nil {
		t.Fatalf("expired frame data is not valid JSON (%q): %v", ev.Data, err)
	}
	if v.ID != "apr_7" || v.SessionID != "sess-7" {
		t.Fatalf("expired frame = %+v, want {apr_7 sess-7}", v)
	}
}

// TestApprovalIssue569ApprovalExpiredEventNameDistinct guards that "approval_expired"
// collides with no agent SessionEventType nor the other global frame names.
func TestApprovalIssue569ApprovalExpiredEventNameDistinct(t *testing.T) {
	others := []string{
		string(agent.SessionEventFinal), string(agent.SessionEventError),
		string(agent.SessionEventNotice), string(agent.SessionEventSubAgent),
		string(agent.SessionEventPlan), string(agent.SessionEventUsage),
		approvalEventName, notificationEventName,
	}
	for _, n := range others {
		if n == approvalExpiredEventName {
			t.Fatalf("approval_expired name collides with %q — a client could not route it by name", n)
		}
	}
}
