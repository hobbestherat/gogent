package server

// Issue #481: a daemon agent turn is decoupled from the HTTP request context so it
// survives a client disconnect. The Send / ApprovePlan handlers dispatch the turn
// on a daemon-owned goroutine under context.Background() and return its turn id
// immediately (an acceptedView); the turn then runs to completion and its events
// — each stamped with the turn id — flow over the SSE hub.
//
// These tests exercise that contract end to end against the in-memory server with
// a controllable fake model. They cover the four design gates: turns survive
// disconnect, non-blocking return, busy-state held for the full turn, stop still
// cancels, turn-id correlation, no double final, and no regressions.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"gogent/internal/agent"
	"gogent/internal/gogent"
)

// newServerWithBackend builds a Server whose model endpoint is the given fake
// backend URL, so a test can drive a controllable model (block-on-channel, canned
// responses) through the real Gogent.
func newServerWithBackend(t *testing.T, backendURL string) *Server {
	t.Helper()
	t.Setenv("GOGENT_MODEL_URL", backendURL+"/chat/completions")
	return NewServer(gogent.NewGogent(t.TempDir()), Options{Password: "x"})
}

// createTestSession creates an ephemeral session over the API and returns its id.
func createTestSession(t *testing.T, srv *Server) string {
	t.Helper()
	rec := serveOne(t, srv, loopbackReq(http.MethodPost, "/api/sessions", strings.NewReader(`{"title":"s","persisted":false}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("create session status = %d; body=%s", rec.Code, rec.Body.String())
	}
	var v sessionView
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatalf("decode created session: %v", err)
	}
	if v.ID == "" {
		t.Fatal("created session has no id")
	}
	return v.ID
}

// postMessage sends a message to a session and returns the recorder.
func postMessage(t *testing.T, srv *Server, id, body string) *httptest.ResponseRecorder {
	t.Helper()
	return serveOne(t, srv, loopbackReq(http.MethodPost, "/api/sessions/"+id+"/messages", strings.NewReader(body)))
}

// acceptedTurnID decodes an acceptedView body and fails if no turn id was minted.
func acceptedTurnID(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var acc acceptedView
	if err := json.Unmarshal(rec.Body.Bytes(), &acc); err != nil {
		t.Fatalf("decode acceptedView: %v (body=%s)", err, rec.Body.String())
	}
	if acc.TurnID == "" {
		t.Fatalf("acceptedView has empty turn id; body=%s", rec.Body.String())
	}
	return acc.TurnID
}

// awaitEvent reads session events from a hub subscription until pred matches
// (draining as it goes) and returns the matching event. Fails on timeout.
func awaitEvent(t *testing.T, sub <-chan taggedEvent, pred func(agent.SessionEvent) bool, timeout time.Duration) agent.SessionEvent {
	t.Helper()
	deadline := time.After(timeout)
	for {
		select {
		case te := <-sub:
			if pred(te.ev) {
				return te.ev
			}
		case <-deadline:
			t.Fatalf("timed out after %v awaiting event", timeout)
		}
	}
}

// awaitTerminal awaits the next terminal (final/error/plan) event.
func awaitTerminal(t *testing.T, sub <-chan taggedEvent, timeout time.Duration) agent.SessionEvent {
	t.Helper()
	return awaitEvent(t, sub, func(ev agent.SessionEvent) bool { return isTerminal(ev) }, timeout)
}

// isSessionBusy reports whether the server's busy gate currently holds the session.
func isSessionBusy(srv *Server, id string) bool {
	srv.busyMu.Lock()
	defer srv.busyMu.Unlock()
	_, ok := srv.busy[id]
	return ok
}

// waitNotBusy polls until the session's busy gate is released (the turn goroutine
// has run its onDone).
func waitNotBusy(t *testing.T, srv *Server, id string) {
	t.Helper()
	deadline := time.After(3 * time.Second)
	tick := time.NewTicker(5 * time.Millisecond)
	defer tick.Stop()
	for {
		if !isSessionBusy(srv, id) {
			return
		}
		select {
		case <-tick.C:
		case <-deadline:
			t.Fatalf("session %s never became idle", id)
		}
	}
}

// blockingBackend is a fake model endpoint that signals when the model call
// arrives and blocks until release is closed (or the request is cancelled), then
// writes a final answer. It lets a test hold a dispatched turn in flight.
func blockingBackend(t *testing.T, arrived chan<- struct{}, release <-chan struct{}, final string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case arrived <- struct{}{}:
		default:
		}
		select {
		case <-release:
		case <-r.Context().Done():
		}
		writeServerTestFinal(w, final)
	}))
}

// --- Gate 2: non-blocking send + 202-style shape ----------------------------

// TestSendReturnsAcceptedWithTurnID verifies the Send handler is non-blocking in
// shape: it returns 200 with an acceptedView carrying a freshly-minted turn id
// (the framework cannot emit a literal 202; see design §2).
func TestSendReturnsAcceptedWithTurnID(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeServerTestFinal(w, "answer")
	}))
	defer backend.Close()
	srv := newServerWithBackend(t, backend.URL)
	id := createTestSession(t, srv)

	sub, unsub := srv.hub.subscribeSession(id)
	defer unsub()

	rec := postMessage(t, srv, id, `{"message":"hi"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("send status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	turnID := acceptedTurnID(t, rec)
	if !strings.HasPrefix(turnID, "turn_") {
		t.Fatalf("turn id = %q, want a turn_ prefix", turnID)
	}
	// Drain the dispatched turn so its goroutine exits before teardown.
	if term := awaitTerminal(t, sub, 3*time.Second); term.Type != agent.SessionEventFinal {
		t.Fatalf("terminal = %s, want final", term.Type)
	}
}

// TestSendReturnsBeforeTurnCompletes proves the handler returns immediately: with
// the model blocked, the response still arrives in well under a second. A blocking
// implementation would hang here until the turn finished.
func TestSendReturnsBeforeTurnCompletes(t *testing.T) {
	arrived := make(chan struct{}, 1)
	release := make(chan struct{})
	backend := blockingBackend(t, arrived, release, "done")
	defer backend.Close()
	srv := newServerWithBackend(t, backend.URL)
	id := createTestSession(t, srv)

	sub, unsub := srv.hub.subscribeSession(id)
	defer unsub()

	start := time.Now()
	rec := postMessage(t, srv, id, `{"message":"hi"}`)
	elapsed := time.Since(start)
	if elapsed > time.Second {
		t.Fatalf("send blocked for %v; the handler must return before the turn completes", elapsed)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("send status = %d; body=%s", rec.Code, rec.Body.String())
	}
	acceptedTurnID(t, rec)

	// The turn is genuinely in flight at the (blocked) model.
	select {
	case <-arrived:
	case <-time.After(3 * time.Second):
		t.Fatal("turn never reached the model")
	}

	// Let the turn finish so its goroutine exits cleanly before teardown.
	close(release)
	if term := awaitTerminal(t, sub, 3*time.Second); term.Type != agent.SessionEventFinal {
		t.Fatalf("terminal = %s, want final", term.Type)
	}
}

// --- Gate 1: turns survive disconnect ---------------------------------------

// TestDispatchedTurnSurvivesRequestCancel is the core #481 invariant for the Send
// path: cancelling the HTTP request's context while the turn is in flight must NOT
// abort the turn (it runs under context.Background()). If the request context were
// still threaded into the model call, cancelling it would surface an error event
// instead of the final answer.
func TestDispatchedTurnSurvivesRequestCancel(t *testing.T) {
	arrived := make(chan struct{}, 1)
	release := make(chan struct{})
	backend := blockingBackend(t, arrived, release, "done")
	defer backend.Close()
	srv := newServerWithBackend(t, backend.URL)
	id := createTestSession(t, srv)

	sub, unsub := srv.hub.subscribeSession(id)
	defer unsub()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req := loopbackReq(http.MethodPost, "/api/sessions/"+id+"/messages", strings.NewReader(`{"message":"hi"}`))
	req = req.WithContext(ctx)
	rec := serveOne(t, srv, req)
	turnID := acceptedTurnID(t, rec)

	// Wait until the turn is in flight, then simulate a client disconnect by
	// cancelling the request context.
	select {
	case <-arrived:
	case <-time.After(3 * time.Second):
		t.Fatal("turn never reached the model")
	}
	cancel()

	// The turn survives: releasing the model lets it complete with a final carrying
	// the dispatched turn id.
	close(release)
	term := awaitTerminal(t, sub, 3*time.Second)
	if term.Type != agent.SessionEventFinal {
		t.Fatalf("turn did not survive request cancel: terminal = %s (a coupled context would have produced an error)", term.Type)
	}
	if term.TurnID != turnID {
		t.Fatalf("final turn id = %q, want %q", term.TurnID, turnID)
	}
}

// --- Turn-id correlation ----------------------------------------------------

// TestDispatchedTurnEventsCarryTurnID asserts every event of a dispatched turn is
// stamped with the turn id returned to the caller, so a client can correlate SSE
// events to the originating POST.
func TestDispatchedTurnEventsCarryTurnID(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeServerTestFinal(w, "final answer")
	}))
	defer backend.Close()
	srv := newServerWithBackend(t, backend.URL)
	id := createTestSession(t, srv)

	sub, unsub := srv.hub.subscribeSession(id)
	defer unsub()

	rec := postMessage(t, srv, id, `{"message":"hi"}`)
	turnID := acceptedTurnID(t, rec)

	var seen int
	term := awaitEvent(t, sub, func(ev agent.SessionEvent) bool {
		if ev.TurnID != turnID {
			t.Errorf("event type=%s carries turn id %q, want %q", ev.Type, ev.TurnID, turnID)
		}
		seen++
		return isTerminal(ev)
	}, 3*time.Second)
	if term.Type != agent.SessionEventFinal {
		t.Fatalf("terminal = %s, want final", term.Type)
	}
	if seen < 2 {
		t.Fatalf("observed only %d turn event(s); expected at least a thinking + final", seen)
	}
}

// --- Gate 4: stop still cancels ---------------------------------------------

// TestStopCancelsDispatchedTurn verifies POST /stop cancels an async-dispatched
// turn (StopAgent -> agent.Cancel() cancels the loop's own child context, which is
// independent of the context.Background() parent the turn runs under) and that the
// busy gate is released when the stopped turn ends.
func TestStopCancelsDispatchedTurn(t *testing.T) {
	arrived := make(chan struct{}, 1)
	release := make(chan struct{})
	backend := blockingBackend(t, arrived, release, "done")
	defer backend.Close()
	defer close(release)
	srv := newServerWithBackend(t, backend.URL)
	id := createTestSession(t, srv)

	sub, unsub := srv.hub.subscribeSession(id)
	defer unsub()

	acceptedTurnID(t, postMessage(t, srv, id, `{"message":"hi"}`))
	select {
	case <-arrived:
	case <-time.After(3 * time.Second):
		t.Fatal("turn never reached the model")
	}

	stopRec := serveOne(t, srv, loopbackReq(http.MethodPost, "/api/sessions/"+id+"/stop", nil))
	if stopRec.Code != http.StatusOK && stopRec.Code != http.StatusNoContent {
		t.Fatalf("stop status = %d, want 200/204; body=%s", stopRec.Code, stopRec.Body.String())
	}

	// Stop aborts the in-flight model call -> runLoop emits a terminal error.
	term := awaitTerminal(t, sub, 3*time.Second)
	if term.Type != agent.SessionEventError {
		t.Fatalf("terminal = %s, want error after stop", term.Type)
	}
	// The busy gate is released when the stopped turn's goroutine ends.
	waitNotBusy(t, srv, id)
}

// --- Gate 5: busy-state held for the full turn ------------------------------

// TestDispatchedTurnHoldsBusyUntilCompletion verifies the busy gate is released on
// turn completion (onDone), not on handler return: a second send mid-turn gets
// 409, and only succeeds again after the in-flight turn finishes.
func TestDispatchedTurnHoldsBusyUntilCompletion(t *testing.T) {
	arrived := make(chan struct{}, 1)
	release := make(chan struct{})
	backend := blockingBackend(t, arrived, release, "done")
	defer backend.Close()
	srv := newServerWithBackend(t, backend.URL)
	id := createTestSession(t, srv)

	sub, unsub := srv.hub.subscribeSession(id)
	defer unsub()

	// First send: dispatches a turn that blocks at the model.
	if rec := postMessage(t, srv, id, `{"message":"first"}`); rec.Code != http.StatusOK {
		t.Fatalf("first send status = %d; body=%s", rec.Code, rec.Body.String())
	}
	select {
	case <-arrived:
	case <-time.After(3 * time.Second):
		t.Fatal("first turn never reached the model")
	}
	if !isSessionBusy(srv, id) {
		t.Fatal("session should be busy while the turn is in flight")
	}

	// A second send while the turn is in flight is rejected with 409.
	if rec := postMessage(t, srv, id, `{"message":"second"}`); rec.Code != http.StatusConflict {
		t.Fatalf("second send status = %d, want 409; body=%s", rec.Code, rec.Body.String())
	}

	// Let the turn finish: the gate is released on completion.
	close(release)
	if term := awaitTerminal(t, sub, 3*time.Second); term.Type != agent.SessionEventFinal {
		t.Fatalf("terminal = %s, want final", term.Type)
	}
	waitNotBusy(t, srv, id)

	// After completion a new send is accepted again.
	if rec := postMessage(t, srv, id, `{"message":"third"}`); rec.Code != http.StatusOK {
		t.Fatalf("third send status = %d, want 200 after completion; body=%s", rec.Code, rec.Body.String())
	}
	awaitTerminal(t, sub, 3*time.Second) // drain so the goroutine exits before teardown
}

// --- Defect hunt: subtask must emit exactly one final -----------------------

// TestSendSubtaskEmitsSingleFinalWithTurnID guards the §3.2 single-final invariant:
// an agent/subtask turn surfaces the one-shot sub-agent's result as exactly one
// SessionEventFinal (via the observer), stamped with the turn id. A regression that
// re-introduced a server-side hub.deliver shim alongside the core emit would double
// the final.
func TestSendSubtaskEmitsSingleFinalWithTurnID(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeServerTestFinal(w, "subtask result")
	}))
	defer backend.Close()
	srv := newServerWithBackend(t, backend.URL)
	id := createTestSession(t, srv)

	sub, unsub := srv.hub.subscribeSession(id)
	defer unsub()

	rec := postMessage(t, srv, id, `{"message":"do the thing","subtask":true}`)
	turnID := acceptedTurnID(t, rec)

	finals := 0
	var finalEv agent.SessionEvent
	term := awaitEvent(t, sub, func(ev agent.SessionEvent) bool {
		if ev.Type == agent.SessionEventFinal {
			finals++
			finalEv = ev
		}
		return isTerminal(ev)
	}, 5*time.Second)
	if term.Type != agent.SessionEventFinal {
		t.Fatalf("terminal = %s, want final", term.Type)
	}
	if finals != 1 {
		t.Fatalf("subtask emitted %d SessionEventFinal, want exactly 1 (no double final)", finals)
	}
	if finalEv.TurnID != turnID {
		t.Fatalf("subtask final turn id = %q, want %q", finalEv.TurnID, turnID)
	}
}

// --- Error handling ---------------------------------------------------------

// TestSendUnknownSessionReturns404 confirms a send to a missing session still 404s
// (validation is synchronous, before any goroutine or busy claim leaks).
func TestSendUnknownSessionReturns404(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeServerTestFinal(w, "x")
	}))
	defer backend.Close()
	srv := newServerWithBackend(t, backend.URL)
	rec := postMessage(t, srv, "does-not-exist", `{"message":"hi"}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("send to unknown session status = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
}

// TestSendEmptyMessageReturns400 confirms the empty-message guard still applies.
func TestSendEmptyMessageReturns400(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeServerTestFinal(w, "x")
	}))
	defer backend.Close()
	srv := newServerWithBackend(t, backend.URL)
	id := createTestSession(t, srv)
	rec := postMessage(t, srv, id, `{"message":""}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("empty-message send status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
}

// --- Gate 1 for the Stream path ---------------------------------------------

// TestStreamTurnSurvivesClientDisconnect is the strongest #481 proof: a streamed
// turn whose client disconnects mid-turn must keep running on the daemon. An
// independent subscriber observes the turn still reaching SessionEventFinal after
// the streaming client is gone, and the busy gate is released on completion (not
// on disconnect).
func TestStreamTurnSurvivesClientDisconnect(t *testing.T) {
	arrived := make(chan struct{}, 1)
	release := make(chan struct{})
	backend := blockingBackend(t, arrived, release, "done")
	defer backend.Close()
	srv := newServerWithBackend(t, backend.URL)
	httpSrv := httptest.NewServer(srv.Handler())
	defer httpSrv.Close()

	// Create the session over HTTP.
	createResp, err := http.Post(httpSrv.URL+"/api/sessions", "application/json", strings.NewReader(`{"title":"s","persisted":false}`))
	if err != nil {
		t.Fatalf("create session request: %v", err)
	}
	var created sessionView
	if err := json.NewDecoder(createResp.Body).Decode(&created); err != nil {
		t.Fatalf("decode created session: %v", err)
	}
	_ = createResp.Body.Close()

	// Independent in-process subscriber: it sees the turn's events even after the
	// streaming client goes away (mirrors a reconnecting /events subscriber).
	sub, unsub := srv.hub.subscribeSession(created.ID)
	defer unsub()

	// Open the streaming turn.
	streamCtx, streamCancel := context.WithCancel(context.Background())
	streamReq, err := http.NewRequestWithContext(streamCtx, http.MethodPost,
		httpSrv.URL+"/api/sessions/"+created.ID+"/messages/stream",
		strings.NewReader(`{"message":"stream me"}`))
	if err != nil {
		t.Fatalf("build stream request: %v", err)
	}
	streamReq.Header.Set("Content-Type", "application/json")
	streamResp, err := http.DefaultClient.Do(streamReq)
	if err != nil {
		t.Fatalf("stream request: %v", err)
	}
	defer streamResp.Body.Close()
	if streamResp.StatusCode != http.StatusOK {
		t.Fatalf("stream status = %d, want 200", streamResp.StatusCode)
	}

	select {
	case <-arrived:
	case <-time.After(3 * time.Second):
		t.Fatal("stream turn never reached the model")
	}
	if !isSessionBusy(srv, created.ID) {
		t.Fatal("session should be busy while the stream turn is in flight")
	}

	// Disconnect the streaming client mid-turn.
	streamCancel()
	_ = streamResp.Body.Close()

	// The turn must keep running on the daemon: releasing the model lets it
	// complete, observed by the independent subscriber.
	close(release)
	term := awaitTerminal(t, sub, 3*time.Second)
	if term.Type != agent.SessionEventFinal {
		t.Fatalf("stream turn did not survive client disconnect: terminal = %s", term.Type)
	}
	// Busy is released on turn completion, not on disconnect.
	waitNotBusy(t, srv, created.ID)
}

// TestStreamSubtaskEmitsSingleFinal guards the single-final invariant on the
// stream path for an agent/subtask turn.
func TestStreamSubtaskEmitsSingleFinal(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeServerTestFinal(w, "subtask result")
	}))
	defer backend.Close()
	srv := newServerWithBackend(t, backend.URL)
	httpSrv := httptest.NewServer(srv.Handler())
	defer httpSrv.Close()

	createResp, err := http.Post(httpSrv.URL+"/api/sessions", "application/json", strings.NewReader(`{"title":"s","persisted":false}`))
	if err != nil {
		t.Fatalf("create session request: %v", err)
	}
	var created sessionView
	if err := json.NewDecoder(createResp.Body).Decode(&created); err != nil {
		t.Fatalf("decode created session: %v", err)
	}
	_ = createResp.Body.Close()

	sub, unsub := srv.hub.subscribeSession(created.ID)
	defer unsub()

	streamReq, err := http.NewRequest(http.MethodPost,
		httpSrv.URL+"/api/sessions/"+created.ID+"/messages/stream",
		strings.NewReader(`{"message":"do thing","subtask":true}`))
	if err != nil {
		t.Fatalf("build stream request: %v", err)
	}
	streamReq.Header.Set("Content-Type", "application/json")
	streamResp, err := http.DefaultClient.Do(streamReq)
	if err != nil {
		t.Fatalf("stream request: %v", err)
	}
	defer streamResp.Body.Close()
	if streamResp.StatusCode != http.StatusOK {
		t.Fatalf("stream status = %d, want 200", streamResp.StatusCode)
	}

	finals := 0
	term := awaitEvent(t, sub, func(ev agent.SessionEvent) bool {
		if ev.Type == agent.SessionEventFinal {
			finals++
		}
		return isTerminal(ev)
	}, 5*time.Second)
	if term.Type != agent.SessionEventFinal {
		t.Fatalf("terminal = %s, want final", term.Type)
	}
	if finals != 1 {
		t.Fatalf("stream subtask emitted %d SessionEventFinal, want exactly 1", finals)
	}
}

// --- ApprovePlan: non-blocking dispatch + busy gate -------------------------

// TestApprovePlanNoPlanReturns400 verifies a synchronous validation failure (no
// pending plan) returns 400 WITHOUT leaking the busy gate: a subsequent send still
// succeeds.
func TestApprovePlanNoPlanReturns400(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeServerTestFinal(w, "x")
	}))
	defer backend.Close()
	srv := newServerWithBackend(t, backend.URL)
	id := createTestSession(t, srv)

	rec := serveOne(t, srv, loopbackReq(http.MethodPost, "/api/sessions/"+id+"/plan/approve", nil))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("approve with no plan status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	if isSessionBusy(srv, id) {
		t.Fatal("busy gate leaked after approve validation failure")
	}

	// A send afterwards must still work.
	sub, unsub := srv.hub.subscribeSession(id)
	defer unsub()
	if rec := postMessage(t, srv, id, `{"message":"hi"}`); rec.Code != http.StatusOK {
		t.Fatalf("send after failed approve status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	awaitTerminal(t, sub, 3*time.Second)
}

// TestApprovePlanDispatchesAsyncAndHoldsBusy verifies the approve path dispatches
// the plan-execution turn async (returns a turn id immediately), holds the busy
// gate for its full duration (a concurrent send gets 409 mid-execution), and
// releases it on completion. The pending plan is produced via the persistent
// plan-mode flag (the approvable-plan path), not the per-request mode:"plan"
// toggle (whose onDone restores plan mode and would clear the pending plan).
func TestApprovePlanDispatchesAsyncAndHoldsBusy(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	approveArrived := make(chan struct{}, 1)
	approveRelease := make(chan struct{})
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		n := calls
		mu.Unlock()
		if n == 1 {
			// Plan-mode turn: produces the plan awaiting approval.
			writeServerTestFinal(w, "THE PLAN: do it")
			return
		}
		// Plan-execution turn: block so we can observe the busy gate mid-execution.
		select {
		case approveArrived <- struct{}{}:
		default:
		}
		select {
		case <-approveRelease:
		case <-r.Context().Done():
		}
		writeServerTestFinal(w, "plan executed")
	}))
	defer backend.Close()
	srv := newServerWithBackend(t, backend.URL)
	id := createTestSession(t, srv)

	sub, unsub := srv.hub.subscribeSession(id)
	defer unsub()

	// Enable plan mode persistently, then send to produce a pending plan.
	rec := serveOne(t, srv, loopbackReq(http.MethodPut, "/api/sessions/"+id+"/plan-mode", strings.NewReader(`{"enabled":true}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("set plan-mode status = %d; body=%s", rec.Code, rec.Body.String())
	}
	if rec := postMessage(t, srv, id, `{"message":"plan it"}`); rec.Code != http.StatusOK {
		t.Fatalf("plan send status = %d; body=%s", rec.Code, rec.Body.String())
	}
	awaitEvent(t, sub, func(ev agent.SessionEvent) bool { return ev.Type == agent.SessionEventPlan }, 5*time.Second)
	waitNotBusy(t, srv, id)

	// Approve: non-blocking, returns a turn id.
	accRec := serveOne(t, srv, loopbackReq(http.MethodPost, "/api/sessions/"+id+"/plan/approve", nil))
	if accRec.Code != http.StatusOK {
		t.Fatalf("approve status = %d, want 200; body=%s", accRec.Code, accRec.Body.String())
	}
	turnID := acceptedTurnID(t, accRec)

	// The plan-execution turn is in flight: busy held, concurrent send rejected.
	select {
	case <-approveArrived:
	case <-time.After(3 * time.Second):
		t.Fatal("approve turn never reached the model")
	}
	if !isSessionBusy(srv, id) {
		t.Fatal("session should be busy while the approved plan executes")
	}
	if rec := postMessage(t, srv, id, `{"message":"concurrent"}`); rec.Code != http.StatusConflict {
		t.Fatalf("concurrent send during plan execution status = %d, want 409; body=%s", rec.Code, rec.Body.String())
	}

	// Release: the plan turn completes with a final carrying the approve turn id.
	close(approveRelease)
	term := awaitTerminal(t, sub, 3*time.Second)
	if term.Type != agent.SessionEventFinal {
		t.Fatalf("terminal = %s, want final", term.Type)
	}
	if term.TurnID != turnID {
		t.Fatalf("approve final turn id = %q, want %q", term.TurnID, turnID)
	}
	waitNotBusy(t, srv, id)
}
