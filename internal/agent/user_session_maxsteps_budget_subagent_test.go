package agent

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gogent/internal/config"
)

// This file closes the two coverage gaps the first review round flagged for the
// issue #249 change:
//   1. The "token budget" stop condition under maxSteps=0 (unlimited) — the
//      primary financial backstop for a runaway "yolo" loop, previously untested.
//   2. The documented behaviour that the configured cap is shared by EVERY loop
//      on the session — including one-shot sub-agent loops — since they all run
//      through UserSession.runLoop on the same receiver.

// TestUnlimitedLoopStillStopsOnTokenBudget verifies that with the step cap
// removed (maxSteps 0), the loop is still bounded by the agent's token budget:
// a model that keeps requesting tools stops at BudgetExceeded() instead of
// looping forever. Without this, "yolo" mode would have no cost backstop.
func TestUnlimitedLoopStillStopsOnTokenBudget(t *testing.T) {
	// A single tool-call response, served forever (fakeServer repeats the last
	// entry once exhausted). Each round-trip reports 15 tokens (prompt 10 +
	// completion 5), so a budget of 40 trips after 3 round-trips (15, 30, 45).
	fs := &fakeServer{responses: makeToolCallResponses(t, 50, "FINAL-ANSWER")}
	server := httptest.NewServer(http.HandlerFunc(fs.handler))
	defer server.Close()

	us, ag := newLoopSession(t, server.URL)
	us.SetMaxSteps(0)     // UNLIMITED — no step cap
	ag.SetTokenBudget(40) // the only brake on this runaway loop

	responses, err := us.ExecuteTaskLoop(context.Background(), "root", "go")
	if err != nil {
		t.Fatalf("budget stop should be graceful (no error), got: %v", err)
	}
	// ceil(40 / 15) == 3 round-trips before the budget check at the top of the
	// next iteration trips. Allow a small envelope to stay robust to token
	// accounting tweaks, but it MUST be tiny and far below the default cap.
	if got := fs.calls; got < 2 || got > 5 {
		t.Errorf("unlimited+budget made %d requests, want a small bounded count (≈3) "+
			"stopped by the budget, not the step cap", got)
	}
	if got := fs.calls; got > 26 {
		t.Errorf("unlimited+budget ran %d requests — the budget backstop failed to stop the loop", got)
	}
	// stopForBudget rewrites the final response to carry the BUDGET_EXCEEDED
	// marker; that is the direct proof the budget branch fired (not the cap,
	// which is off, and not a final answer, which the server never sends).
	last := responses[len(responses)-1].Content
	if !strings.Contains(last, budgetExceededMarker) {
		t.Errorf("expected %q in the final response, got %q", budgetExceededMarker, last)
	}
}

// TestBudgetStillStopsUnderConfiguredCap confirms budget and the step cap are
// independent stop conditions: with a SMALL step cap that would otherwise allow
// many iterations, a tighter budget fires first. (Guards against a refactor that
// gates the budget check behind the step condition.)
func TestBudgetStillStopsUnderConfiguredCap(t *testing.T) {
	fs := &fakeServer{responses: makeToolCallResponses(t, 50, "FINAL-ANSWER")}
	server := httptest.NewServer(http.HandlerFunc(fs.handler))
	defer server.Close()

	us, ag := newLoopSession(t, server.URL)
	us.SetMaxSteps(50)    // would permit 51 round-trips on its own
	ag.SetTokenBudget(40) // trips at ~3 round-trips — well under the cap

	responses, err := us.ExecuteTaskLoop(context.Background(), "root", "go")
	if err != nil {
		t.Fatalf("budget stop should be graceful (no error), got: %v", err)
	}
	if got := fs.calls; got > 5 {
		t.Errorf("budget should stop the loop (~3 requests) well before the 50-step cap; got %d", got)
	}
	last := responses[len(responses)-1].Content
	if !strings.Contains(last, budgetExceededMarker) {
		t.Errorf("expected %q in the final response, got %q", budgetExceededMarker, last)
	}
}

// TestSubAgentLoopHonoursConfiguredCap pins the documented contract that the
// step cap is shared by every loop on the session: a one-shot sub-agent spawned
// via SpawnSubAgent runs its own runLoop on the same receiver, so it must cap at
// the configured N just like the root task loop.
func TestSubAgentLoopHonoursConfiguredCap(t *testing.T) {
	fs := &fakeServer{responses: makeToolCallResponses(t, 40, "SUCCESS: done")}
	server := httptest.NewServer(http.HandlerFunc(fs.handler))
	defer server.Close()

	us, ag := newLoopSession(t, server.URL)
	_ = ag
	us.SetMaxSteps(3)

	final, err := us.SpawnSubAgent(context.Background(), "root", "child", "do it", true)
	if err != nil {
		t.Fatalf("SpawnSubAgent error: %v", err)
	}
	// The sub-agent's loop is capped at 3 tool rounds -> 1 initial + 3 == 4
	// requests to the model. This proves the shared cap governs the sub-agent
	// loop, not just the root task loop.
	if got, want := fs.calls, 4; got != want {
		t.Errorf("sub-agent model requests = %d, want %d (cap of 3 via shared maxSteps)", got, want)
	}
	if !strings.Contains(final, "SUCCESS") {
		// Capped before the final answer is expected — the child never reached
		// "SUCCESS: done" because its loop was bounded at 3 steps. Just assert
		// it returned without error; the request count above is the real check.
		t.Logf("sub-agent final (capped before completion): %q", final)
	}
}

// TestSubAgentLoopHonoursUnlimited confirms the other half of the shared-cap
// contract: maxSteps 0 also unbounds sub-agent loops, so a delegated subtask that
// keeps calling tools runs past the historical 25-step bound instead of being
// artificially interrupted.
func TestSubAgentLoopHonoursUnlimited(t *testing.T) {
	const toolRounds = 30
	fs := &fakeServer{responses: makeToolCallResponses(t, toolRounds, "SUCCESS: done")}
	server := httptest.NewServer(http.HandlerFunc(fs.handler))
	defer server.Close()

	us, _ := newLoopSession(t, server.URL)
	us.SetMaxSteps(0) // unlimited — must also apply to the sub-agent loop

	if _, err := us.SpawnSubAgent(context.Background(), "root", "child", "do it", true); err != nil {
		t.Fatalf("SpawnSubAgent error: %v", err)
	}
	// toolRounds tool calls + the initial + reaching the final => toolRounds+1
	// requests, which must exceed the default 26-request cap.
	if got, want := fs.calls, toolRounds+1; got != want {
		t.Errorf("sub-agent model requests = %d, want %d (unlimited via shared maxSteps)", got, want)
	}
	if fs.calls <= 26 {
		t.Errorf("unlimited sub-agent only made %d requests; it must run past the default cap", fs.calls)
	}
}

// TestSubAgentLoopUsesHistoricalDefaultWhenUnwired guards the behaviour-preservation
// invariant for sub-agents: a session that never calls SetMaxSteps keeps the
// historical 25-step bound on its sub-agent loops too (mirroring pre-#249).
func TestSubAgentLoopUsesHistoricalDefaultWhenUnwired(t *testing.T) {
	fs := &fakeServer{responses: makeToolCallResponses(t, 120, "SUCCESS: done")}
	server := httptest.NewServer(http.HandlerFunc(fs.handler))
	defer server.Close()

	us, _ := newLoopSession(t, server.URL) // maxSteps left at the default (25)

	if _, err := us.SpawnSubAgent(context.Background(), "root", "child", "do it", true); err != nil {
		t.Fatalf("SpawnSubAgent error: %v", err)
	}
	if got, want := fs.calls, config.DefaultMaxSteps+1; got != want {
		t.Errorf("unwired sub-agent model requests = %d, want %d (historical default %d + 1)",
			got, want, config.DefaultMaxSteps)
	}
}
