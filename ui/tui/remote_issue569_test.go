package ui

// Issue #569 — client half of the fix.
//
// The remote TUI discovers pending approvals by polling GET /api/approvals every
// 750ms and presenting each newly-seen one via handleApproval (the ONLY thing that
// raises the ⏳ badge in remote mode). The daemon now pushes a best-effort
// "approval" SSE nudge on alloc(), and StartGated wires that nudge to an immediate
// /approvals re-scan (kickApprovals), so a freshly-raised prompt surfaces its
// badge+dialog without waiting for the next poll tick — and cannot be lost to a
// removal that races the poll (the daemon side, Layer A, keeps it pending until
// fetched). These tests cover the SSE routing, the StartGated wiring, the
// reconnect re-scan (kickApprovals), and the seen-dedup that keeps a push + a
// racing poll from double-presenting.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gogent/internal/agent"
	"gogent/internal/gogent"
	"gogent/internal/permission"
)

// issue569Approver counts how many times AskPermission is presented and can
// optionally block (gate) so a prompt stays pending across several scans — used to
// stress the seen-dedup. ReviewEdit is unused here but required by Approver.
type issue569Approver struct {
	mu    sync.Mutex
	calls int
	gate  chan struct{} // if non-nil, AskPermission blocks until it is closed
}

func (a *issue569Approver) AskPermission(permission.Request) permission.Decision {
	a.mu.Lock()
	a.calls++
	a.mu.Unlock()
	if a.gate != nil {
		<-a.gate
	}
	return permission.DecisionAllow
}

func (a *issue569Approver) ReviewEdit(gogent.EditReviewRequest) gogent.EditReviewDecision {
	return gogent.EditApprove
}

func (a *issue569Approver) count() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.calls
}

// --- Layer B (client): the "approval" SSE frame routing ---------------------

// TestAPIClientStreamEventsRoutesApprovalSignalToHandler confirms an "approval"
// SSE frame is handed to the approval-signal handler (not the session-event sink),
// and a following session event still flows through.
func TestAPIClientStreamEventsRoutesApprovalSignalToHandler(t *testing.T) {
	started := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/events" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		close(started)
		fmt.Fprint(w, "event: approval\n")
		fmt.Fprint(w, `data: {"id":"apr_1"}`+"\n\n")
		fmt.Fprint(w, "event: final\n")
		fmt.Fprint(w, `data: {"session_id":"sess-1","event":{"type":"final","text":"done"}}`+"\n\n")
		flusher.Flush()
		<-r.Context().Done()
	}))
	defer srv.Close()

	client, err := NewAPIClient(srv.URL, "")
	if err != nil {
		t.Fatalf("NewAPIClient: %v", err)
	}
	var nudges atomic.Int32
	client.SetApprovalSignalHandler(func() { nudges.Add(1) })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	events, err := client.StreamEvents(ctx)
	if err != nil {
		t.Fatalf("StreamEvents: %v", err)
	}
	<-started

	// The nudge must reach the handler.
	deadline := time.After(2 * time.Second)
	for nudges.Load() == 0 {
		select {
		case <-time.After(5 * time.Millisecond):
		case <-deadline:
			t.Fatal("approval SSE frame was not routed to the approval-signal handler")
		}
	}

	// The subsequent session event must still flow to the channel.
	select {
	case ev := <-events:
		if ev.SessionID != "sess-1" || ev.Event.Type != string(agent.SessionEventFinal) {
			t.Fatalf("session event = %+v, want final/done after the approval nudge", ev)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the session event after the approval frame")
	}

	// The nudge must not be delivered twice, nor misrouted onto the event channel.
	if got := nudges.Load(); got != 1 {
		t.Fatalf("approval nudge delivered %d times, want exactly 1", got)
	}
}

// TestAPIClientStreamEventsApprovalSignalNilHandlerDropsFrame confirms a frame
// with no handler installed is dropped (not panicked on) and does not corrupt the
// stream.
func TestAPIClientStreamEventsApprovalSignalNilHandlerDropsFrame(t *testing.T) {
	started := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/events" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		close(started)
		fmt.Fprint(w, "event: approval\n")
		fmt.Fprint(w, `data: {"id":"apr_1"}`+"\n\n")
		fmt.Fprint(w, "event: final\n")
		fmt.Fprint(w, `data: {"session_id":"sess-1","event":{"type":"final","text":"ok"}}`+"\n\n")
		flusher.Flush()
		<-r.Context().Done()
	}))
	defer srv.Close()

	client, err := NewAPIClient(srv.URL, "")
	if err != nil {
		t.Fatalf("NewAPIClient: %v", err)
	}
	// No SetApprovalSignalHandler: the frame must be dropped, not panic.

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	events, err := client.StreamEvents(ctx)
	if err != nil {
		t.Fatalf("StreamEvents: %v", err)
	}
	<-started
	select {
	case ev := <-events:
		if ev.Event.Type != string(agent.SessionEventFinal) {
			t.Fatalf("event = %+v, want final after a dropped approval frame", ev)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a nil approval handler dropped the frame but blocked the session event")
	}
}

// --- Layer B (client): StartGated wires the nudge to an immediate re-scan ----

// issue569ApprovalsServer is a stub daemon serving GET /api/approvals (a scripted
// pending list), POST .../decision, and a holding /api/events stream. It records
// how many times the approvals list was fetched.
// issue569Events writes scripted SSE frames on the events stream (the writer is
// the ResponseWriter so frames can be Fprinted; the flusher pushes them).
type issue569Events func(w http.ResponseWriter, f http.Flusher)

func issue569ApprovalsServer(t *testing.T, pending []ApprovalDTO, events issue569Events) (
	srv *httptest.Server, listCalls *int32, setPending func([]ApprovalDTO),
) {
	t.Helper()
	var mu sync.Mutex
	cur := append([]ApprovalDTO(nil), pending...)
	calls := int32(0)
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/approvals":
			atomic.AddInt32(&calls, 1)
			mu.Lock()
			snap := append([]ApprovalDTO(nil), cur...)
			mu.Unlock()
			_ = json.NewEncoder(w).Encode(snap)
		case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/api/approvals/") && strings.HasSuffix(r.URL.Path, "/decision"):
			mu.Lock()
			cur = nil
			mu.Unlock()
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"id":"apr-1","status":"resolved"}`))
		case r.URL.Path == "/api/events":
			w.Header().Set("Content-Type", "text/event-stream")
			flusher := w.(http.Flusher)
			if events != nil {
				events(w, flusher)
			}
			<-r.Context().Done()
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &calls, func(p []ApprovalDTO) {
		mu.Lock()
		cur = p
		mu.Unlock()
	}
}

func issue569PendingApproval() []ApprovalDTO {
	return []ApprovalDTO{{
		ID: "apr-1", Kind: "permission", SessionID: "sess", AgentID: "root",
		Permission: &PermissionDetail{Action: "network", Resource: "example.com", Detail: "https://example.com/"},
	}}
}

// TestRemoteClientSSEApprovalSignalSurfacesPromptWithoutPollTick is the
// criterion-3 centerpiece: the poll interval is so long (1h) the ticker cannot
// fire during the test, so the ONLY way the prompt surfaces is the daemon's
// "approval" SSE nudge → StartGated's handler → kickApprovals → scanApprovals. If
// that wiring is missing, the badge never appears.
//
// The nudge is sent the INSTANT the stream opens (flush headers, then immediately
// push the frame). This deliberately exercises the fixes-round-1 ordering: the
// approval-signal handler is registered BEFORE openStream starts the SSE reader, so
// an immediate nudge is not dropped on a nil handler. Before that fix this test
// would race/fail (the nudge landed while the handler was still nil, and with
// pollEvery=1h there is no poll backstop within the window).
func TestRemoteClientSSEApprovalSignalSurfacesPromptWithoutPollTick(t *testing.T) {
	withFastRetries(t)
	srv, listCalls, _ := issue569ApprovalsServer(t, issue569PendingApproval(), func(w http.ResponseWriter, f http.Flusher) {
		// Send 200 + headers, then immediately push the approval nudge — no gating,
		// to stress the handler-registered-before-reader ordering.
		f.Flush()
		fmt.Fprint(w, "event: approval\n")
		fmt.Fprint(w, `data: {"id":"apr-1"}`+"\n\n")
		f.Flush()
	})

	client, err := NewAPIClient(srv.URL, "")
	if err != nil {
		t.Fatalf("NewAPIClient: %v", err)
	}
	approver := &issue569Approver{}
	// A non-nil sink is required: StartGated only opens the SSE stream when a sink
	// is set, and the nudge arrives over that stream.
	rc := NewRemoteClient(client, func(string, agent.SessionEvent) {}, approver)
	rc.pollEvery = time.Hour // the poll tick cannot surface this prompt in time
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer rc.Close()

	if err := rc.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}

	deadline := time.After(2 * time.Second)
	for approver.count() == 0 {
		select {
		case <-time.After(5 * time.Millisecond):
		case <-deadline:
			n := atomic.LoadInt32(listCalls)
			if n == 0 {
				t.Fatal("the approval SSE nudge did not trigger a /approvals re-scan; StartGated wiring is missing")
			}
			t.Fatalf("approval signal triggered %d /approvals fetches but the prompt was never presented", n)
		}
	}
}

// TestRemoteClientKickApprovalsSurfacesPrompt exercises the reconnect re-sync
// path (criterion 1): kickApprovals forces an immediate /approvals re-scan, so a
// prompt raised during a disconnect surfaces on reconnect rather than waiting for
// the next poll. Here the poll tick is disabled (1h) and there is no SSE nudge,
// so only the kick surfaces it.
func TestRemoteClientKickApprovalsSurfacesPrompt(t *testing.T) {
	withFastRetries(t)
	srv, _, _ := issue569ApprovalsServer(t, issue569PendingApproval(), nil)

	client, err := NewAPIClient(srv.URL, "")
	if err != nil {
		t.Fatalf("NewAPIClient: %v", err)
	}
	approver := &issue569Approver{}
	rc := NewRemoteClient(client, nil, approver)
	rc.pollEvery = time.Hour // no poll tick
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer rc.Close()

	if err := rc.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	// Give the stream a moment to open, then kick (as reconnect would).
	time.Sleep(50 * time.Millisecond)
	rc.kickApprovals()

	select {
	case <-approverSignal(approver):
		// presented via the kick
	case <-time.After(2 * time.Second):
		t.Fatalf("kickApprovals did not surface the pending prompt (presented %d times)", approver.count())
	}
}

// approverSignal returns a channel that closes once the approver has been called at
// least once, so a test can select on "presented" without consuming the count.
func approverSignal(a *issue569Approver) <-chan struct{} {
	ch := make(chan struct{})
	go func() {
		for a.count() == 0 {
			time.Sleep(2 * time.Millisecond)
		}
		close(ch)
	}()
	return ch
}

// TestRemoteClientApprovalPresentedOnceDespitePushAndPoll confirms the seen-dedup:
// with the prompt held open (blocking approver) while BOTH repeated poll ticks and
// several SSE nudges fire, handleApproval runs exactly once — a push and a racing
// poll never double-present the same prompt.
func TestRemoteClientApprovalPresentedOnceDespitePushAndPoll(t *testing.T) {
	withFastRetries(t)
	srv, _, _ := issue569ApprovalsServer(t, issue569PendingApproval(), func(w http.ResponseWriter, f http.Flusher) {
		// Flush headers so the stream opens, then spam several nudges while the
		// prompt stays pending (the approver blocks).
		f.Flush()
		for i := 0; i < 6; i++ {
			fmt.Fprint(w, "event: approval\n")
			fmt.Fprint(w, `data: {"id":"apr-1"}`+"\n\n")
			f.Flush()
			time.Sleep(4 * time.Millisecond)
		}
	})

	client, err := NewAPIClient(srv.URL, "")
	if err != nil {
		t.Fatalf("NewAPIClient: %v", err)
	}
	approver := &issue569Approver{gate: make(chan struct{})} // block → prompt stays pending
	// Non-nil sink so the SSE stream opens and the nudges are actually received
	// (exercising push+poll dedup, not poll-only).
	rc := NewRemoteClient(client, func(string, agent.SessionEvent) {}, approver)
	rc.pollEvery = 15 * time.Millisecond // many poll ticks during the window
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer rc.Close()
	defer close(approver.gate) // release the blocked presentation so goroutines exit

	if err := rc.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Let several polls and all the nudges land while the prompt is pending.
	time.Sleep(140 * time.Millisecond)

	if got := approver.count(); got != 1 {
		t.Fatalf("AskPermission called %d times, want exactly 1 (seen-dedup must prevent double-presentation)", got)
	}
}

// --- Layer C (deliberately dropped): late one-shot stays silent --------------

// TestReportDecisionIssue569LateOneShotStaysSilent locks in the implemented
// behaviour that a late ONE-SHOT decision is surfaced as NO notice. The originally
// designed Layer C (add a cause-agnostic notice here) was dropped during
// implementation to preserve the #560 invariant asserted by
// TestReportDecisionLateNonStickyIsSilent (see design.md §2 "Layer C revised").
// This covers the cases that test does not: permission deny, and every edit-review
// one-shot including approve_all (which is also non-effect on the daemon's late
// path, since late persistence is permission-only).
//
// NOTE: this documents the current behaviour. The task's GOAL says "when the
// decision genuinely cannot be delivered, the user must be told"; a late one-shot
// genuinely has no effect, so this silent path is the residual gap the maintainer
// accepted as a #560 policy trade-off.
func TestReportDecisionIssue569LateOneShotStaysSilent(t *testing.T) {
	cases := []struct {
		kind string
		wire string
	}{
		{"permission", "allow"},
		{"permission", "deny"},
		{"edit_review", "approve"},
		{"edit_review", "reject"},
		{"edit_review", "approve_all"},
	}
	for _, c := range cases {
		rc, sink := newReportingRC(t)
		rc.reportDecision("s1", c.kind, "res", c.wire, "late", nil)
		if got := len(sink.notices()); got != 0 {
			t.Errorf("kind=%q wire=%q: late one-shot emitted %d notice(s), want 0 (silent per #560)",
				c.kind, c.wire, got)
		}
	}
}

// TestReportDecisionIssue569LateStickyStillNotices confirms the Layer-A fix did
// not regress the one notice path #560 keeps: a late sticky permission grant still
// tells the user it applies going forward.
func TestReportDecisionIssue569LateStickyStillNotices(t *testing.T) {
	for _, wire := range []string{"always", "always_deny"} {
		rc, sink := newReportingRC(t)
		rc.reportDecision("s1", "permission", "example.com", wire, "late", nil)
		if got := len(sink.notices()); got != 1 {
			t.Errorf("wire=%q: late sticky notice count = %d, want 1", wire, got)
		}
	}
}
