package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"gogent/internal/config"
)

// makeToolCallResponses builds n calc tool-call responses followed by one final
// answer. A loop that never caps consumes the n tool rounds and terminates at
// the final (n+1 requests); a capped loop stops before reaching the final.
func makeToolCallResponses(t *testing.T, n int, final string) []map[string]interface{} {
	t.Helper()
	out := make([]map[string]interface{}, 0, n+1)
	for i := 0; i < n; i++ {
		out = append(out, toolCallResponse(
			fmt.Sprintf("call_%d", i), "calc", `{"expression":"1+1"}`))
	}
	out = append(out, finalResponse(final))
	return out
}

// TestNewUserSessionDefaultsMaxStepsToHistoricalBound verifies an unwired session
// inherits the built-in step cap (issue #249). #449 raised that default from the
// historical 25 to 100, so an unwired session now keeps the 100-step backstop.
func TestNewUserSessionDefaultsMaxStepsToHistoricalBound(t *testing.T) {
	us, _ := newLoopSession(t, "http://unused.test")
	if got := us.MaxSteps(); got != config.DefaultMaxSteps {
		t.Errorf("new session MaxSteps = %d, want default %d", got, config.DefaultMaxSteps)
	}
	if got := us.MaxSteps(); got != 100 {
		t.Errorf("built-in cap must be 100 (raised from 25 in #449), got %d", got)
	}
}

// TestSetMaxStepsRoundTripsThroughAccessors exercises the mutex-guarded
// setter/getter across positive, zero (unlimited), and negative (also
// unlimited) values.
func TestSetMaxStepsRoundTripsThroughAccessors(t *testing.T) {
	us, _ := newLoopSession(t, "http://unused.test")
	for _, n := range []int{0, 1, 2, 7, 25, 1000, -1, -1000} {
		us.SetMaxSteps(n)
		if got := us.MaxSteps(); got != n {
			t.Errorf("SetMaxSteps(%d) -> MaxSteps() = %d", n, got)
		}
	}
}

// TestDefaultMaxStepsCapsLoopAtHistoricalBound is the cap-fires test: with the
// default cap (100) untouched, a model that keeps requesting tools is interrupted
// after 100 tool rounds. The canned responses carry more than 100 tool-call turns
// so the loop hits the cap before reaching the final answer, and — per #449 — the
// exit surfaces a visible STEP_LIMIT_REACHED notice rather than the orphaned turn.
func TestDefaultMaxStepsCapsLoopAtHistoricalBound(t *testing.T) {
	// More tool-call turns than the default cap so the cap fires on a tool-call
	// turn (the orphaned round-trip), not on a final answer.
	fs := &fakeServer{responses: makeToolCallResponses(t, config.DefaultMaxSteps+20, "FINAL-ANSWER")}
	server := httptest.NewServer(http.HandlerFunc(fs.handler))
	defer server.Close()

	us, _ := newLoopSession(t, server.URL) // default maxSteps == DefaultMaxSteps (100)

	responses, err := us.ExecuteTaskLoop(context.Background(), "root", "go")
	if err != nil {
		t.Fatalf("loop returned error: %v", err)
	}
	// One initial round-trip + DefaultMaxSteps capped tool rounds == DefaultMaxSteps+1 requests.
	want := config.DefaultMaxSteps + 1
	if got := fs.calls; got != want {
		t.Errorf("model requests = %d, want %d (default %d-step cap)", got, want, config.DefaultMaxSteps)
	}
	if len(responses) != want {
		t.Errorf("responses = %d, want %d", len(responses), want)
	}
	last := responses[len(responses)-1].Content
	// The cap fired before the model could emit its final answer.
	if strings.Contains(last, "FINAL-ANSWER") {
		t.Errorf("loop reached the final answer despite the default cap; last content = %q", last)
	}
	// #449: a cap exit whose final round-trip carried unexecuted tool calls must
	// surface a visible STEP_LIMIT_REACHED notice (naming the cap) instead of the
	// orphaned content, so the stop is explainable rather than silent.
	if !strings.Contains(last, stepLimitReachedMarker) {
		t.Errorf("cap-exit final content = %q, want it to surface %q", last, stepLimitReachedMarker)
	}
	if !strings.Contains(last, fmt.Sprintf("(%d)", config.DefaultMaxSteps)) {
		t.Errorf("cap-exit final content = %q, want it to name the cap value (%d)", last, config.DefaultMaxSteps)
	}
}

// TestConfiguredMaxStepsCapsLoop verifies a configured positive N bounds the
// loop at N tool rounds (N+1 total model requests), for N at and around the
// boundary, including N == 1 (a single tool round permitted).
func TestConfiguredMaxStepsCapsLoop(t *testing.T) {
	cases := []int{1, 2, 3, 5, 25}
	for _, n := range cases {
		t.Run(fmt.Sprintf("cap=%d", n), func(t *testing.T) {
			fs := &fakeServer{responses: makeToolCallResponses(t, 60, "FINAL-ANSWER")}
			server := httptest.NewServer(http.HandlerFunc(fs.handler))
			defer server.Close()

			us, _ := newLoopSession(t, server.URL)
			us.SetMaxSteps(n)

			if _, err := us.ExecuteTaskLoop(context.Background(), "root", "go"); err != nil {
				t.Fatalf("loop returned error: %v", err)
			}
			// Initial round-trip + N capped tool rounds == N+1 requests.
			if got, want := fs.calls, n+1; got != want {
				t.Errorf("cap=%d: model requests = %d, want %d", n, got, want)
			}
		})
	}
}

// TestMaxStepsZeroIsUnlimitedRunsPastDefaultBound is the headline behaviour test
// for issue #249: max_steps 0 ("yolo") removes the artificial 25-step cap, so a
// model that keeps calling tools runs well past the old bound and completes
// naturally at its final answer instead of being interrupted.
func TestMaxStepsZeroIsUnlimitedRunsPastDefaultBound(t *testing.T) {
	const toolRounds = 40 // well past the historical 25-step cap
	fs := &fakeServer{responses: makeToolCallResponses(t, toolRounds, "FINAL-ANSWER")}
	server := httptest.NewServer(http.HandlerFunc(fs.handler))
	defer server.Close()

	us, _ := newLoopSession(t, server.URL)
	us.SetMaxSteps(0) // unlimited

	responses, err := us.ExecuteTaskLoop(context.Background(), "root", "go")
	if err != nil {
		t.Fatalf("loop returned error: %v", err)
	}
	// Unbounded: the loop consumes all tool rounds + the initial + reaches the
	// final => toolRounds + 1 requests, and that must exceed the default cap.
	if got, want := fs.calls, toolRounds+1; got != want {
		t.Errorf("model requests = %d, want %d (unlimited should run to completion)", got, want)
	}
	const defaultCapRequests = 26 // 1 initial + 25
	if fs.calls <= defaultCapRequests {
		t.Errorf("unlimited loop only made %d requests; it must run PAST the %d-request default cap",
			fs.calls, defaultCapRequests)
	}
	last := responses[len(responses)-1].Content
	if !strings.Contains(last, "FINAL-ANSWER") {
		t.Errorf("unlimited loop should reach the final answer; last content = %q", last)
	}
}

// TestMaxStepsNegativeIsAlsoUnlimited confirms the documented contract that any
// non-positive value (not just 0) means unlimited: runLoop's `maxSteps <= 0`
// branch must treat a negative cap as unbounded too.
func TestMaxStepsNegativeIsAlsoUnlimited(t *testing.T) {
	const toolRounds = 30
	fs := &fakeServer{responses: makeToolCallResponses(t, toolRounds, "FINAL-ANSWER")}
	server := httptest.NewServer(http.HandlerFunc(fs.handler))
	defer server.Close()

	us, _ := newLoopSession(t, server.URL)
	us.SetMaxSteps(-5) // non-positive => unlimited per the accessor/loop contract

	responses, err := us.ExecuteTaskLoop(context.Background(), "root", "go")
	if err != nil {
		t.Fatalf("loop returned error: %v", err)
	}
	if got, want := fs.calls, toolRounds+1; got != want {
		t.Errorf("model requests = %d, want %d (negative cap must be unlimited)", got, want)
	}
	if fs.calls <= 26 {
		t.Errorf("negative-cap loop only made %d requests; it must run past the default cap", fs.calls)
	}
	last := responses[len(responses)-1].Content
	if !strings.Contains(last, "FINAL-ANSWER") {
		t.Errorf("negative-cap loop should reach the final answer; last content = %q", last)
	}
}

// TestUnlimitedLoopStillTerminatesOnFinalAnswer guards the other unchanged exit
// conditions: under maxSteps 0, a normal "one tool call then final answer"
// conversation must still terminate via the no-tool-calls exit — the unlimited
// condition must not turn the loop into an infinite spin.
func TestUnlimitedLoopStillTerminatesOnFinalAnswer(t *testing.T) {
	fs := &fakeServer{responses: []map[string]interface{}{
		toolCallResponse("c1", "calc", `{"expression":"1+1"}`),
		finalResponse("FINAL-ANSWER"),
	}}
	server := httptest.NewServer(http.HandlerFunc(fs.handler))
	defer server.Close()

	us, _ := newLoopSession(t, server.URL)
	us.SetMaxSteps(0)

	responses, err := us.ExecuteTaskLoop(context.Background(), "root", "go")
	if err != nil {
		t.Fatalf("loop returned error: %v", err)
	}
	if got, want := fs.calls, 2; got != want {
		t.Errorf("model requests = %d, want %d (no-tool-calls exit must still fire under unlimited)", got, want)
	}
	if last := responses[len(responses)-1].Content; !strings.Contains(last, "FINAL-ANSWER") {
		t.Errorf("expected the final answer, got %q", last)
	}
}

// TestUnlimitedLoopStillHonoursContextCancellation is the safety test for the
// "yolo" mode: with the step cap removed, a runaway loop's only brakes are its
// other stop conditions. Cancellation (StopAgent / session close / caller ctx)
// must still terminate it — otherwise unlimited mode hangs the session forever.
func TestUnlimitedLoopStillHonoursContextCancellation(t *testing.T) {
	release := make(chan struct{})
	// defer close(release) unblocks any handler parked in its select. It MUST
	// run before the server shuts down, so the server close is registered as a
	// t.Cleanup (which runs after defers) — a plain defer server.Close() would
	// run first (LIFO) and deadlock on the parked connection.
	defer close(release)

	var mu sync.Mutex
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		idx := calls
		calls++
		mu.Unlock()

		if idx == 0 {
			// First round-trip returns a tool call so the loop enters its body
			// under maxSteps 0 and starts a second round-trip (which blocks).
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(
				toolCallResponse("c1", "calc", `{"expression":"1+1"}`))
			return
		}
		// Second+ round-trip: hang until released or the request's context is
		// cancelled (the caller cancelling propagates here).
		select {
		case <-release:
		case <-r.Context().Done():
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(finalResponse("done"))
	}))
	t.Cleanup(server.Close) // runs after the close(release) defer, avoiding a deadlock

	us, _ := newLoopSession(t, server.URL)
	us.SetMaxSteps(0) // unlimited — without cancellation this loop never stops

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		_, err := us.ExecuteTaskLoop(ctx, "root", "go")
		done <- err
	}()

	// Let the loop finish round-trip 1, run the calc tool, and block on
	// round-trip 2 — i.e. be genuinely iterating under the unlimited cap.
	time.Sleep(80 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("unlimited loop returned nil after cancellation; want a cancellation error")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("unlimited loop did not return within 2s of cancellation — it ignored ctx and would spin forever")
	}
}

// TestMaxStepsResolvedAtLoopStartNotMidRun documents that the cap is read once
// at loop entry (so a reconfigure takes effect on the NEXT task, not mid-loop).
// This keeps the behaviour predictable and avoids a mid-run cap change
// stranding the loop between two policies.
func TestMaxStepsResolvedAtLoopStartNotMidRun(t *testing.T) {
	fs := &fakeServer{responses: makeToolCallResponses(t, 40, "FINAL-ANSWER")}
	server := httptest.NewServer(http.HandlerFunc(fs.handler))
	defer server.Close()

	us, _ := newLoopSession(t, server.URL)
	us.SetMaxSteps(2)

	// Reconfigure to unlimited WHILE idle, before the loop reads the cap.
	us.SetMaxSteps(0)
	if _, err := us.ExecuteTaskLoop(context.Background(), "root", "go"); err != nil {
		t.Fatalf("loop returned error: %v", err)
	}
	// maxSteps was 0 at loop start => unbounded => reaches the final (41 reqs),
	// proving the value read at entry (0) governed the run, not a stale 2.
	if got := fs.calls; got != 41 {
		t.Errorf("model requests = %d, want 41 (cap read as 0 at loop start)", got)
	}
}
