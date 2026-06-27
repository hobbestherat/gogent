package ui

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
)

// Issue #516 — on `gogent --connect` the global SSE stream is opened and consumed
// BEFORE the initial Restore() runs, so consume() floods the desktop.Post queue while
// Restore grinds through slow sequential round-trips, starving the UI thread.
//
// The fix splits RemoteClient.Start into StartGated, which opens the stream
// synchronously (fail-fast preserved) but returns a `begin` closure that launches the
// consumer only once the workbench signals its initial Restore() is done (Run's
// afterRestore hook). These tests pin the connect-order contract and hunt for
// regressions across all four design criteria, with no internal/daemon/server import.

// --- shared helpers ----------------------------------------------------------

// issue516SSEHub is a minimal global-SSE (/api/events) test server. Each subscriber
// gets its own buffered channel the test pushes GlobalEventDTOs onto; the server
// serializes them as SSE frames exactly like the daemon. It also counts subscriptions
// so a test can assert fail-fast, gating and reconnect stream counts.
type issue516SSEHub struct {
	mu      sync.Mutex
	streams []chan GlobalEventDTO
	gets    int32
}

func newIssue516SSEServer(t *testing.T, status int) (*httptest.Server, *issue516SSEHub) {
	t.Helper()
	h := &issue516SSEHub{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/events" {
			http.NotFound(w, r)
			return
		}
		if status != http.StatusOK {
			http.Error(w, "denied", status)
			return
		}
		atomic.AddInt32(&h.gets, 1)
		ch := make(chan GlobalEventDTO, 8)
		h.mu.Lock()
		h.streams = append(h.streams, ch)
		h.mu.Unlock()

		w.Header().Set("Content-Type", "text/event-stream")
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Error("response writer is not a flusher")
			return
		}
		flusher.Flush()
		for {
			select {
			case ge, ok := <-ch:
				if !ok {
					return
				}
				b, _ := json.Marshal(ge)
				fmt.Fprintf(w, "data: %s\n\n", b)
				flusher.Flush()
			case <-r.Context().Done():
				return
			}
		}
	}))
	t.Cleanup(srv.Close)
	return srv, h
}

func (h *issue516SSEHub) stream(i int) chan GlobalEventDTO {
	h.mu.Lock()
	defer h.mu.Unlock()
	if i < 0 || i >= len(h.streams) {
		return nil
	}
	return h.streams[i]
}

func (h *issue516SSEHub) streamCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.streams)
}

func (h *issue516SSEHub) closeStream(i int) {
	h.mu.Lock()
	if i >= 0 && i < len(h.streams) && h.streams[i] != nil {
		close(h.streams[i])
	}
	h.mu.Unlock()
}

// recordingSink captures every delivered event behind a mutex so a test can assert
// ordering, counts and gating without a live UI. It is the EventSink the RemoteClient
// pumps daemon events into (normally Workbench.EmitSessionEvent).
type recordingSink struct {
	mu  sync.Mutex
	evs []issue516Delivered
}

type issue516Delivered struct {
	sessionID string
	typ       agent.SessionEventType
	text      string
}

func (s *recordingSink) onEvent(sessionID string, ev agent.SessionEvent) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.evs = append(s.evs, issue516Delivered{sessionID, ev.Type, ev.Text})
}

func (s *recordingSink) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.evs)
}

func (s *recordingSink) snapshot() []issue516Delivered {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]issue516Delivered, len(s.evs))
	copy(out, s.evs)
	return out
}

// wait516 polls ok until it returns true or the deadline elapses, keeping the tests
// stable on a loaded Pi instead of sleeping a fixed guess. Mirrors waitForIssue358.
func wait516(t *testing.T, ok func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if ok() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// assertNoDeliveryFor asserts the sink receives nothing for a short window — the
// gating property: while begin() is withheld, an open-but-undrained stream must NOT
// pump events into the UI. Fails the instant a single event leaks.
func assertNoDeliveryFor(t *testing.T, s *recordingSink, window time.Duration) {
	t.Helper()
	deadline := time.Now().Add(window)
	for time.Now().Before(deadline) {
		if got := s.count(); got != 0 {
			t.Fatalf("event delivered to the UI before begin() ran (gating broken): %d delivered", got)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// countRecordsContaining counts how many transcript records carry sub in their header
// or any child line. Used to detect a buffered live event being applied ON TOP OF a
// restored transcript record (a double-apply): the same text would appear in two
// distinct records.
func countRecordsContaining(sw *SessionWindow, sub string) int {
	if sw == nil || sw.transcript == nil {
		return 0
	}
	n := 0
	for _, r := range sw.transcript.records {
		if r == nil {
			continue
		}
		if strings.Contains(r.header, sub) {
			n++
			continue
		}
		for _, ln := range r.lines {
			if strings.Contains(ln.text, sub) {
				n++
				break
			}
		}
	}
	return n
}

// --- (1) GOAL MATCH: delivery is gated until Restore completes ----------------

// TestStartGated_StreamOpenedButNoDeliveryUntilBegin is the core acceptance pin: the
// stream IS opened synchronously by StartGated (fail-fast + so the daemon starts
// buffering into the subscriber channel), but NOT a single event reaches the sink
// until begin() runs. Then, once begin() runs, the buffered event is delivered.
func TestStartGated_StreamOpenedButNoDeliveryUntilBegin(t *testing.T) {
	srv, h := newIssue516SSEServer(t, http.StatusOK)
	client, err := NewAPIClient(srv.URL, "")
	if err != nil {
		t.Fatalf("NewAPIClient: %v", err)
	}
	sink := &recordingSink{}
	rc := NewRemoteClient(client, sink.onEvent, nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	begin, err := rc.StartGated(ctx)
	if err != nil {
		t.Fatalf("StartGated: %v", err)
	}
	defer rc.Close()

	// The stream was opened during StartGated (fail-fast connect happened), proving the
	// socket is live — the gating is on DRAINING, not on opening.
	wait516(t, func() bool { return h.streamCount() == 1 }, "initial SSE subscription")

	// The daemon emits an event for an actively-running session; it lands in the
	// subscriber buffer because consume() has not started.
	ev := GlobalEventDTO{SessionID: "s1", Event: EventDTO{Type: string(agent.SessionEventThinking), Text: "during-restore"}}
	select {
	case h.stream(0) <- ev:
	default:
		t.Fatal("could not push event onto the server stream")
	}

	// Restore is still "running" (begin withheld): the event must NOT reach the UI.
	assertNoDeliveryFor(t, sink, 200*time.Millisecond)

	// Restore completes -> Run calls afterRestore -> begin(). Now the event delivers.
	begin()
	wait516(t, func() bool { return sink.count() == 1 }, "first delivery after begin")

	got := sink.snapshot()
	if len(got) != 1 || got[0].sessionID != "s1" || got[0].text != "during-restore" {
		t.Fatalf("delivered = %+v, want exactly the one gated s1 event", got)
	}
}

// TestStartGated_BeginIsIdempotent pins the consumeOnce guard: calling begin() more
// than once must launch exactly one consumer, so a buffered event is delivered once,
// not once-per-begin-call. (Run could in principle re-enter afterRestore; begin must
// stay safe.)
func TestStartGated_BeginIsIdempotent(t *testing.T) {
	srv, h := newIssue516SSEServer(t, http.StatusOK)
	client, _ := NewAPIClient(srv.URL, "")
	sink := &recordingSink{}
	rc := NewRemoteClient(client, sink.onEvent, nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	begin, err := rc.StartGated(ctx)
	if err != nil {
		t.Fatalf("StartGated: %v", err)
	}
	defer rc.Close()
	wait516(t, func() bool { return h.streamCount() == 1 }, "initial SSE subscription")

	h.stream(0) <- GlobalEventDTO{SessionID: "s1", Event: EventDTO{Type: string(agent.SessionEventFinal), Text: "once"}}

	// begin called three times must not triplicate the consumer.
	begin()
	begin()
	begin()
	wait516(t, func() bool { return sink.count() >= 1 }, "delivery after begin")

	if got := sink.count(); got != 1 {
		t.Fatalf("begin idempotency: delivered %d times, want 1 (consumeOnce must guard the launch)", got)
	}
}

// --- Start vs StartGated (startOnce) interaction -----------------------------

// TestStartGated_IsIdempotent_SecondCallReturnsNoOpBegin pins startOnce: a second
// StartGated must not open a second stream or produce a second live begin.
func TestStartGated_IsIdempotent_SecondCallReturnsNoOpBegin(t *testing.T) {
	srv, h := newIssue516SSEServer(t, http.StatusOK)
	client, _ := NewAPIClient(srv.URL, "")
	sink := &recordingSink{}
	rc := NewRemoteClient(client, sink.onEvent, nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	begin1, err := rc.StartGated(ctx)
	if err != nil {
		t.Fatalf("first StartGated: %v", err)
	}
	defer rc.Close()
	wait516(t, func() bool { return h.streamCount() == 1 }, "first SSE subscription")

	// Second call: startOnce already consumed — must be a no-op (no error, no new stream).
	begin2, err2 := rc.StartGated(ctx)
	if err2 != nil {
		t.Fatalf("second StartGated returned error %v; want a no-op", err2)
	}

	begin1() // the real consumer
	h.stream(0) <- GlobalEventDTO{SessionID: "s1", Event: EventDTO{Type: string(agent.SessionEventFinal), Text: "x"}}
	begin2() // no-op
	begin2() // no-op
	wait516(t, func() bool { return sink.count() == 1 }, "single delivery")

	if got := sink.count(); got != 1 {
		t.Fatalf("delivered %d, want 1 (second StartGated must not launch another consumer)", got)
	}
	if got := h.streamCount(); got != 1 {
		t.Fatalf("opened %d SSE streams, want 1 (StartGated must be idempotent)", got)
	}
}

// TestStartGated_AfterStartIsNoOp pins the Start/StartGated handshake: Start launches
// the consumer eagerly, so a later StartGated must be a no-op (no second stream, no
// second consumer). Guards the attach path against a future caller mixing the two.
func TestStartGated_AfterStartIsNoOp(t *testing.T) {
	srv, h := newIssue516SSEServer(t, http.StatusOK)
	client, _ := NewAPIClient(srv.URL, "")
	sink := &recordingSink{}
	rc := NewRemoteClient(client, sink.onEvent, nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := rc.Start(ctx); err != nil { // eager: consume already running
		t.Fatalf("Start: %v", err)
	}
	defer rc.Close()
	wait516(t, func() bool { return h.streamCount() == 1 }, "Start SSE subscription")

	h.stream(0) <- GlobalEventDTO{SessionID: "s1", Event: EventDTO{Type: string(agent.SessionEventFinal), Text: "first"}}
	wait516(t, func() bool { return sink.count() == 1 }, "Start eager delivery")

	// StartGated now must be a no-op (startOnce already done by Start).
	begin, err := rc.StartGated(ctx)
	if err != nil {
		t.Fatalf("StartGated after Start returned error %v; want no-op", err)
	}
	begin()

	h.stream(0) <- GlobalEventDTO{SessionID: "s1", Event: EventDTO{Type: string(agent.SessionEventFinal), Text: "second"}}
	wait516(t, func() bool { return sink.count() == 2 }, "second delivery")

	if got := h.streamCount(); got != 1 {
		t.Fatalf("opened %d streams after Start+StartGated, want 1", got)
	}
}

// TestStart_PreservesEagerConsume is the regression guard for the embedded/test path:
// Start (unchanged) must still launch the consumer immediately, delivering events
// with no begin() step. Existing Start-based suites depend on this.
func TestStart_PreservesEagerConsume(t *testing.T) {
	srv, h := newIssue516SSEServer(t, http.StatusOK)
	client, _ := NewAPIClient(srv.URL, "")
	sink := &recordingSink{}
	rc := NewRemoteClient(client, sink.onEvent, nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := rc.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer rc.Close()
	wait516(t, func() bool { return h.streamCount() == 1 }, "Start SSE subscription")

	h.stream(0) <- GlobalEventDTO{SessionID: "s1", Event: EventDTO{Type: string(agent.SessionEventFinal), Text: "eager"}}
	// No begin() call: Start must have started consuming already.
	wait516(t, func() bool { return sink.count() == 1 }, "eager delivery without begin")

	if got := sink.snapshot()[0]; got.text != "eager" {
		t.Fatalf("delivered %q, want eager", got.text)
	}
}

// --- (1) fail-fast: an unreachable/denied daemon aborts before the TUI launches --

// TestStartGated_FailFastReturnsErrorSynchronously pins the fail-fast contract: if the
// initial subscribe fails, StartGated returns the error synchronously (so attach never
// launches the TUI) and `begin` is a safe no-op.
func TestStartGated_FailFastReturnsErrorSynchronously(t *testing.T) {
	srv, _ := newIssue516SSEServer(t, http.StatusServiceUnavailable) // /events -> 503
	client, _ := NewAPIClient(srv.URL, "")
	sink := &recordingSink{}
	rc := NewRemoteClient(client, sink.onEvent, nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	begin, err := rc.StartGated(ctx)
	if err == nil {
		t.Fatal("StartGated with a denied /events returned nil; want a fail-fast error so the TUI never launches")
	}
	defer rc.Close()

	// begin must be a safe no-op (consume/monitorHealth never started).
	begin()
	assertNoDeliveryFor(t, sink, 100*time.Millisecond)
}

// --- (3) NO REGRESSIONS: reconnect re-subscribes + jumps to present ------------

// TestStartGated_ReconnectAfterBeginReSubscribesAndJumpsToPresent is the critical
// reconnect pin on the gated path: after begin(), a dropped stream must still drive
// the blocking disconnect/reconnect cycle — re-subscribing (a second stream) and
// jumping to present (OnConnectionRestored + the new stream's events deliver, never a
// replay of the old stream's backlog).
func TestStartGated_ReconnectAfterBeginReSubscribesAndJumpsToPresent(t *testing.T) {
	srv, h := newIssue516SSEServer(t, http.StatusOK)
	client, _ := NewAPIClient(srv.URL, "")
	sink := &recordingSink{}
	rc := NewRemoteClient(client, sink.onEvent, nil)
	rc.SetReconnector(&issue358Reconnector{})
	rc.backoff = func(int) time.Duration { return time.Millisecond }

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	begin, err := rc.StartGated(ctx)
	if err != nil {
		t.Fatalf("StartGated: %v", err)
	}
	defer rc.Close()
	wait516(t, func() bool { return h.streamCount() == 1 }, "initial SSE subscription")

	// Live consumption starts; the first stream carries one event.
	begin()
	h.stream(0) <- GlobalEventDTO{SessionID: "s1", Event: EventDTO{Type: string(agent.SessionEventThinking), Text: "before"}}
	wait516(t, func() bool { return sink.count() == 1 }, "pre-drop delivery")

	// Drop the first stream: consume must fall into reconnect.
	h.closeStream(0)
	wait516(t, func() bool { return h.streamCount() == 2 }, "replacement SSE subscription")
	wait516(t, func() bool {
		return sink.snapshot()[0].text == "before" // ensure prior state stable
	}, "steady state")

	// The replacement stream is a FRESH stream (jump-to-present): its events deliver,
	// and nothing from the dead first stream is replayed.
	h.stream(1) <- GlobalEventDTO{SessionID: "s1", Event: EventDTO{Type: string(agent.SessionEventFinal), Text: "present"}}
	wait516(t, func() bool { return sink.count() == 2 }, "post-reconnect delivery")

	got := sink.snapshot()
	if len(got) != 2 || got[0].text != "before" || got[1].text != "present" {
		t.Fatalf("events = %+v, want [before, present] (fresh stream, no replay)", got)
	}
}

// --- edge cases --------------------------------------------------------------

// TestStartGated_NilSink_BeginIsNoOpAndOpensNoStream pins the nil-sink branch: a
// RemoteClient built without a sink (narrow tests) must not open a stream at all, and
// begin must be a safe no-op rather than nil-derefing the consumer.
func TestStartGated_NilSink_BeginIsNoOpAndOpensNoStream(t *testing.T) {
	srv, h := newIssue516SSEServer(t, http.StatusOK)
	client, _ := NewAPIClient(srv.URL, "")
	rc := NewRemoteClient(client, nil, nil) // no sink, no approver

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	begin, err := rc.StartGated(ctx)
	if err != nil {
		t.Fatalf("StartGated nil-sink: %v", err)
	}
	defer rc.Close()

	// No sink => no stream opened (the connect block is guarded by rc.sink != nil).
	if got := h.streamCount(); got != 0 {
		t.Fatalf("nil-sink opened %d streams, want 0", got)
	}
	begin() // must not panic
	begin()
}

// TestStartGated_BeginAfterCloseIsSafe pins the Ctrl+C-during-startup edge: if the
// client is Closed before begin runs (or begin races shutdown), calling begin must be
// safe and must not spin a reconnect cycle against a cancelled context.
func TestStartGated_BeginAfterCloseIsSafe(t *testing.T) {
	srv, h := newIssue516SSEServer(t, http.StatusOK)
	client, _ := NewAPIClient(srv.URL, "")
	rc := NewRemoteClient(client, func(string, agent.SessionEvent) {}, nil)
	rec := &issue358Reconnector{}
	rc.SetReconnector(rec)
	rc.backoff = func(int) time.Duration { return 50 * time.Millisecond }

	ctx, cancel := context.WithCancel(context.Background())
	begin, err := rc.StartGated(ctx)
	if err != nil {
		t.Fatalf("StartGated: %v", err)
	}
	wait516(t, func() bool { return h.streamCount() == 1 }, "initial SSE subscription")

	// A buffered event + shutdown before consumption starts.
	h.stream(0) <- GlobalEventDTO{SessionID: "s1", Event: EventDTO{Type: string(agent.SessionEventFinal), Text: "late"}}
	cancel() // == Close()'s effect on rc.ctx
	rc.Close()

	begin() // must not panic
	// Give any (incorrect) reconnect loop a chance to fire, then assert it did not.
	time.Sleep(120 * time.Millisecond)
	if got := rec.lostAttempts(); got != 0 {
		t.Fatalf("begin-after-close triggered %d reconnect attempts; want 0 (context already cancelled)", got)
	}
}

// --- (4) HOLISTIC: the Run/afterRestore wiring lives in gogent, embedded-safe ---

// TestSetAfterRestore_NilByDefaultAndStored pins the embedded-path safety and the
// setter contract: a fresh workbench has no afterRestore hook (the embedded path
// consumes eagerly and is unaffected), and SetAfterRestore stores the callback.
// (Run itself can't be driven headlessly — tui.New wires real stdin/stdout, so Run
// would enter raw mode and block — so the Run→afterRestore ordering is pinned at the
// StartGated/begin seam above and by build + reasoning, as the design notes.)
func TestSetAfterRestore_NilByDefaultAndStored(t *testing.T) {
	w := newTestWorkbench(t)
	if w.afterRestore != nil {
		t.Fatal("a fresh workbench has afterRestore set; the embedded path (no attach) must be unaffected")
	}

	var fired atomic.Bool
	w.SetAfterRestore(func() { fired.Store(true) })
	if w.afterRestore == nil {
		t.Fatal("SetAfterRestore did not store the callback")
	}
	w.afterRestore()
	if !fired.Load() {
		t.Fatal("stored afterRestore callback did not fire")
	}
}

// --- (3) the double-apply regression hunt ------------------------------------
//
// The fix opens the stream at T0 (fail-fast) but defers draining it until Restore
// finishes at T_end. The daemon's global stream is live-only (no replay), so the
// subscriber buffer accumulates exactly the events that occurred in [T0, T_end].
// Restore fetches each session's transcript during that window: a turn that COMPLETED
// in [T0, T_get] is therefore present in BOTH the fetched transcript AND the buffered
// live stream. Pre-fix, consume ran during Restore and such an event for a
// not-yet-adopted window was DROPPED (deliverSessionEvent: sw==nil -> undelivered);
// the transcript snapshot then superseded it cleanly. Post-fix, every buffered event
// is drained AFTER its window exists, so it is applied ON TOP OF the snapshot.
// SessionWindow.apply has no dedup (addAssistant -> assistantRecord always appends),
// so the answer is rendered twice. These two tests pin the invariant the fix must
// not break: a live event whose content is already in the restored transcript must
// not be duplicated.

// TestConnectOrder_LiveFinalOverlappingRestoreTranscriptIsNotDuplicated is the
// deterministic, seam-level pin: delivering a SessionEventFinal (what consume does
// post-begin) onto a window whose restored transcript already contains that answer
// must yield exactly one record, not two.
func TestConnectOrder_LiveFinalOverlappingRestoreTranscriptIsNotDuplicated(t *testing.T) {
	w := newTestWorkbench(t)
	silenceNotifications(w)

	// Restore adopted s1 with a transcript that already contains the completed answer
	// (the turn finished during the restore window and is in the /sessions snapshot).
	sw := w.AdoptSession(RestoredSession{
		ID:       "s1",
		Title:    "S1",
		Messages: []ChatMessage{{Role: "assistant", Content: "DUP-SEAM-7X"}},
	})
	drainPosted(t, w)
	if n := countRecordsContaining(sw, "DUP-SEAM-7X"); n != 1 {
		t.Fatalf("setup: restored transcript has %d records, want 1", n)
	}

	// The deferred consumer drains the buffered live Final for the same turn onto the
	// now-open window (exactly what begin() triggers post-restore).
	w.deliverSessionEvent("s1", agent.SessionEvent{Type: agent.SessionEventFinal, Text: "DUP-SEAM-7X"})

	if n := countRecordsContaining(sw, "DUP-SEAM-7X"); n != 1 {
		t.Fatalf("issue #516 double-apply: a buffered live Final whose answer is already in the "+
			"restored transcript produced %d records, want 1 (apply() must not duplicate the snapshot)", n)
	}
}

// TestStartGated_DrainsBacklogOntoRestoredWindowWithoutDuplicating is the full
// integration pin through the real seams: StartGated (stream open + buffered) ->
// Restore adopts the session with a transcript that already contains the answer ->
// begin() drains the backlog -> EmitSessionEvent -> deliverSessionEvent. The restored
// answer must appear exactly once. It also demonstrates the ordering the issue asks
// for: restore populates the window BEFORE the live event is delivered.
func TestStartGated_DrainsBacklogOntoRestoredWindowWithoutDuplicating(t *testing.T) {
	srv, h := newIssue516SSEServer(t, http.StatusOK)
	client, _ := NewAPIClient(srv.URL, "")

	w := newTestWorkbench(t)
	silenceNotifications(w)
	w.handlers.Restore = func() []RestoredSession {
		// The transcript snapshot already contains a turn that completed during the
		// (stream-open .. transcript-fetch) window.
		return []RestoredSession{{ID: "s1", Title: "S1",
			Messages: []ChatMessage{{Role: "assistant", Content: "DUP-INT-7X"}}}}
	}
	emit := w.EmitSessionEvent

	// Sink both records delivery (ordering) and forwards into the real workbench so the
	// transcript is actually mutated (what production's wb.EmitSessionEvent does).
	var delivered atomic.Int32
	rc := NewRemoteClient(client, func(sessionID string, ev agent.SessionEvent) {
		delivered.Add(1)
		emit(sessionID, ev)
	}, nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	begin, err := rc.StartGated(ctx)
	if err != nil {
		t.Fatalf("StartGated: %v", err)
	}
	defer rc.Close()
	wait516(t, func() bool { return h.streamCount() == 1 }, "initial SSE subscription")

	// A turn completes during the restore window: its Final is buffered in the
	// subscriber channel (consume has not started).
	h.stream(0) <- GlobalEventDTO{SessionID: "s1", Event: EventDTO{Type: string(agent.SessionEventFinal), Text: "DUP-INT-7X"}}

	// Restore runs (the slow part): adopt each session with its fetched transcript.
	// This mirrors Run's restore block, which completes BEFORE afterRestore fires.
	for _, rs := range w.handlers.Restore() {
		w.AdoptSession(rs)
	}
	drainPosted(t, w)
	sw := w.sessions["s1"]
	if sw == nil {
		t.Fatal("restore did not adopt s1")
	}
	if n := countRecordsContaining(sw, "DUP-INT-7X"); n != 1 {
		t.Fatalf("restore baseline: %d records, want 1 (the snapshot answer)", n)
	}

	// Restore done -> begin drains the backlog onto the now-populated window.
	begin()
	wait516(t, func() bool { return delivered.Load() == 1 }, "backlog drained to sink")
	drainPosted(t, w) // apply the posted EmitSessionEvent on the UI thread

	if n := countRecordsContaining(sw, "DUP-INT-7X"); n != 1 {
		t.Fatalf("issue #516 double-apply (integration): draining the gated backlog onto a "+
			"restored window produced %d records, want 1 (the live Final duplicated the snapshot)", n)
	}
}

// --- driver fixes-round-1: the Final-only dedup guard -------------------------
//
// The driver addressed the double-apply not by reopening a fresh stream at begin()
// (which would have no backlog to duplicate) but by adding a tail-match dedup in
// SessionWindow.apply: a SessionEventFinal whose text equals the transcript's last
// assistant answer (with no newer user turn) is dropped. The two tests below probe
// that guard: (a) it is Final-SPECIFIC, so other append events that overlap the
// restored transcript still double-apply; (b) it correctly does NOT over-drop a
// legitimate identical answer in a brand-new turn.

// TestConnectOrder_DedupCoversFinalButNotToolCall restores a window whose transcript
// already contains a completed turn that used a tool (both the tool call and the final
// answer are in the snapshot), then drains the buffered live ToolCall + Final for that
// same turn (what consume does post-begin). The Final is deduped (1 answer record), but
// the ToolCall is NOT — apply()'s guard covers SessionEventFinal only, so tool calls
// (and, by the same gap, thoughts/errors/compactions) that overlap the restored
// transcript still double-apply. The daemon transcript format does carry tool messages
// (see reconnect_skip_unchanged_issue520_test.go / export_test.go), so this is
// reachable on connect for any tool-using turn that finished during restore.
func TestConnectOrder_DedupCoversFinalButNotToolCall(t *testing.T) {
	w := newTestWorkbench(t)
	silenceNotifications(w)
	sw := w.AdoptSession(RestoredSession{ID: "s1", Messages: []ChatMessage{
		{Role: "assistant", Content: "DUP-ANS-7X", Tool: "ZZTOOLDUP516", Args: `{"p":"q"}`},
	}})
	drainPosted(t, w)
	if got := countRecordsContaining(sw, "DUP-ANS-7X"); got != 1 {
		t.Fatalf("baseline: %d answer records, want 1", got)
	}
	if got := countRecordsContaining(sw, "ZZTOOLDUP516"); got != 1 {
		t.Fatalf("baseline: %d tool records, want 1", got)
	}

	// The deferred consumer drains the buffered live ToolCall + Final for the same turn.
	w.deliverSessionEvent("s1", agent.SessionEvent{Type: agent.SessionEventToolCall, CallID: "c1", Tool: "ZZTOOLDUP516", Args: map[string]interface{}{"p": "q"}})
	w.deliverSessionEvent("s1", agent.SessionEvent{Type: agent.SessionEventFinal, Text: "DUP-ANS-7X"})

	// The Final IS deduped by the fix: still exactly one answer record.
	if got := countRecordsContaining(sw, "DUP-ANS-7X"); got != 1 {
		t.Fatalf("Final was not deduped: %d answer records, want 1", got)
	}
	// But the ToolCall is NOT deduped -> it duplicates the restored tool block. This is
	// the remaining gap: apply()'s guard is Final-only.
	if got := countRecordsContaining(sw, "ZZTOOLDUP516"); got != 1 {
		t.Fatalf("issue #516 (remaining gap): a buffered live ToolCall overlapping the restored "+
			"transcript duplicated the tool block: %d tool records, want 1 (the dedup guards only "+
			"SessionEventFinal; ToolCall/Thought/Error/Compaction are not covered)", got)
	}
}

// TestConnectOrder_DedupKeepsIdenticalAnswerAcrossSeparateTurns pins the dedup's
// correctness property so the heuristic cannot over-drop: two legitimately-identical
// answers in two separate turns must both be kept, because the second turn's user
// record intervenes between them. (Guards against the guard becoming too aggressive.)
func TestConnectOrder_DedupKeepsIdenticalAnswerAcrossSeparateTurns(t *testing.T) {
	w := newTestWorkbench(t)
	silenceNotifications(w)
	sw := w.AdoptSession(RestoredSession{ID: "s1", Messages: []ChatMessage{
		{Role: "user", Content: "q"},
		{Role: "assistant", Content: "SAME-ANS-7X"},
	}})
	drainPosted(t, w)
	if n := countAssistantRecords(sw); n != 1 {
		t.Fatalf("baseline: %d assistant records, want 1", n)
	}

	// A new turn: its user message is recorded first, then the model replies with the
	// SAME text as the previous turn.
	sw.addUser("q-again")
	w.deliverSessionEvent("s1", agent.SessionEvent{Type: agent.SessionEventFinal, Text: "SAME-ANS-7X"})

	if n := countAssistantRecords(sw); n != 2 {
		t.Fatalf("dedup over-dropped a legitimate identical answer in a separate turn: "+
			"%d assistant records, want 2 (a user record must protect a genuine new reply)", n)
	}
}

// --- driver fixes-round-2: restored-flag dedup --------------------------------
//
// Round 2 made the dedup match ONLY snapshot-built (restored) records, so live-vs-
// live events are never suppressed. Final/ToolCall/AssistantStep are now guarded.
// The two tests below (a) pin that live-vs-live robustness and (b) probe the one
// content-bearing append path still unguarded: SessionEventThinkingDelta.

// countRecordsOfKind tallies transcript records of a given kind. Used to detect a
// restored thought being duplicated by a drained ThinkingDelta (both kindThinking).
func countRecordsOfKind(sw *SessionWindow, k eventKind) int {
	if sw == nil || sw.transcript == nil {
		return 0
	}
	n := 0
	for _, r := range sw.transcript.records {
		if r != nil && r.kind == k {
			n++
		}
	}
	return n
}

// TestConnectOrder_DedupKeepsRepeatedLiveToolCallsInOneTurn pins the restored-flag
// invariant the whole round-2 fix rests on: a drained live event is suppressed only
// when it duplicates a RESTORED record, never another live one. Two legitimate calls
// to the same tool inside one live turn must both render.
func TestConnectOrder_DedupKeepsRepeatedLiveToolCallsInOneTurn(t *testing.T) {
	w := newTestWorkbench(t)
	silenceNotifications(w)
	sw := w.AdoptSession(RestoredSession{ID: "s1", Messages: []ChatMessage{
		{Role: "user", Content: "q"},
	}})
	drainPosted(t, w)

	// A brand-new live turn calls the same tool twice (distinct call ids).
	sw.addUser("do it")
	w.deliverSessionEvent("s1", agent.SessionEvent{Type: agent.SessionEventToolCall, CallID: "c1", Tool: "ZZLIVETOOL516", Args: map[string]interface{}{"a": "1"}})
	w.deliverSessionEvent("s1", agent.SessionEvent{Type: agent.SessionEventToolCall, CallID: "c2", Tool: "ZZLIVETOOL516", Args: map[string]interface{}{"a": "2"}})
	drainPosted(t, w)

	if n := countRecordsContaining(sw, "ZZLIVETOOL516"); n != 2 {
		t.Fatalf("repeated live tool calls in one turn rendered %d tool records, want 2 "+
			"(live-vs-live must never be deduped; the guard matches only restored records)", n)
	}
}

// TestConnectOrder_DedupDoesNotCoverThinkingDelta exposes the remaining unguarded
// append path. A restored assistant message carries its reasoning as a kindThinking
// "thought" record (thoughtRecord); a buffered SessionEventThinkingDelta for the same
// reasoning (drained post-begin) calls appendThinkingDelta, which is NOT guarded by
// restoredDuplicate — so it builds a SECOND kindThinking record. apply() guards
// Final/ToolCall/AssistantStep but not ThinkingDelta (nor Compaction/Error).
func TestConnectOrder_DedupDoesNotCoverThinkingDelta(t *testing.T) {
	w := newTestWorkbench(t)
	silenceNotifications(w)
	sw := w.AdoptSession(RestoredSession{ID: "s1", Messages: []ChatMessage{
		{Role: "assistant", Reasoning: "REASON-7X", Content: "ans"},
	}})
	drainPosted(t, w)
	if n := countRecordsOfKind(sw, kindThinking); n != 1 {
		t.Fatalf("baseline: %d thinking records, want 1 (restored from Reasoning)", n)
	}

	// The deferred consumer drains a leftover ThinkingDelta for the same reasoning.
	w.deliverSessionEvent("s1", agent.SessionEvent{Type: agent.SessionEventThinkingDelta, Text: "REASON-7X\n"})
	drainPosted(t, w)

	if n := countRecordsOfKind(sw, kindThinking); n != 1 {
		t.Fatalf("issue #516 (remaining gap): a buffered ThinkingDelta overlapping the restored "+
			"reasoning duplicated the thinking block: %d kindThinking records, want 1 "+
			"(apply() dedup covers Final/ToolCall/AssistantStep but not SessionEventThinkingDelta)", n)
	}
}
