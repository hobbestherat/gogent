package gogent

// Tests for the issue #481 async-dispatch panic containment (recoverTurn) added to
// the Dispatch* goroutines. runLoop has its own panic recovery, but it is armed
// only once the loop is running; recoverTurn guards the synchronous turn
// entrypoint before that (model-config selection, buildConnection, ThoughtTrain
// setup, checkpoints), mirroring the embedded path's per-session containment
// (issue #8) so a panicking turn surfaces as a session error instead of crashing
// the daemon.

import (
	"strings"
	"testing"

	"gogent/internal/agent"
	"gogent/internal/model"
)

// TestRecoverTurnContainsPanicEmitsErrorAndRunsPriorDefers is the core guarantee of
// the panic-containment fix: a panic inside a recoverTurn-deferred scope
//  1. is contained — it does not escape and crash the process (if it did, the
//     assertions below are unreachable and the test binary dies),
//  2. is surfaced to the session observer as a SessionEventError stamped with the
//     originating turn id, and
//  3. does not skip a defer registered BEFORE recoverTurn — i.e. the dispatch
//     goroutine's onDone (which releases the busy gate) still runs, so a panicking
//     turn cannot leak the busy gate.
func TestRecoverTurnContainsPanicEmitsErrorAndRunsPriorDefers(t *testing.T) {
	g := NewGogent(t.TempDir())
	sess := model.NewModelSession("main", g.defaultConnection())
	root := agent.NewAgent("root", sess)
	us := g.CreateUserSession("sess-panic", root)

	var got []agent.SessionEvent
	us.SetObserver(func(ev agent.SessionEvent) { got = append(got, ev) })

	const turnID = "turn_panic_test"
	released := false

	// Reproduce the dispatch goroutine's defer ordering exactly: onDone is deferred
	// first (runs last), recoverTurn second (runs first, contains the panic).
	func() {
		defer func() { released = true }() // mimics onDone → release()
		defer recoverTurn(us, turnID)
		panic("synthetic pre-runLoop boom")
	}()

	if len(got) != 1 {
		t.Fatalf("expected exactly one error event from recoverTurn, got %d: %+v", len(got), got)
	}
	ev := got[0]
	if ev.Type != agent.SessionEventError {
		t.Fatalf("event type = %s, want error", ev.Type)
	}
	if ev.TurnID != turnID {
		t.Fatalf("error event turn id = %q, want %q", ev.TurnID, turnID)
	}
	if ev.Err == nil || !strings.Contains(ev.Err.Error(), "turn panicked") {
		t.Fatalf("error = %v, want it to mention 'turn panicked'", ev.Err)
	}
	if !released {
		t.Fatal("onDone (busy release) did not run after the recovered panic — the busy gate would leak")
	}
}

// TestRecoverTurnNoPanicIsNoOp confirms recoverTurn is inert when the turn
// completed normally (no panic to recover), so it does not emit a spurious error.
func TestRecoverTurnNoPanicIsNoOp(t *testing.T) {
	g := NewGogent(t.TempDir())
	sess := model.NewModelSession("main", g.defaultConnection())
	root := agent.NewAgent("root", sess)
	us := g.CreateUserSession("sess-nopanic", root)

	var got []agent.SessionEvent
	us.SetObserver(func(ev agent.SessionEvent) { got = append(got, ev) })

	func() {
		defer recoverTurn(us, "turn_ok")
		// Normal completion — no panic.
	}()

	if len(got) != 0 {
		t.Fatalf("recoverTurn emitted %d event(s) on a panic-free exit, want 0: %+v", len(got), got)
	}
}
