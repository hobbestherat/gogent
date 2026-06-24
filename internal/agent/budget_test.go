package agent

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestAgentTokenBudget covers the per-agent token accounting and the
// at-or-over-budget predicate.
func TestAgentTokenBudget(t *testing.T) {
	ag := NewAgent("a", nil)

	// An agent with no budget is never over budget, however many tokens it spends.
	if ag.BudgetExceeded() {
		t.Fatal("zero budget should never be exceeded")
	}
	ag.AddTokensUsed(1000, 1000)
	if ag.BudgetExceeded() {
		t.Fatal("unbounded agent must stay under budget")
	}

	ag = NewAgent("b", nil)
	ag.SetTokenBudget(100)
	if total := ag.AddTokensUsed(40, 10); total != 50 {
		t.Fatalf("AddTokensUsed returned %d, want 50", total)
	}
	if ag.GetTokensUsed() != 50 {
		t.Fatalf("GetTokensUsed = %d, want 50", ag.GetTokensUsed())
	}
	if ag.BudgetExceeded() {
		t.Fatal("50/100 should be under budget")
	}
	// Reaching the budget exactly trips it (>=, not >).
	ag.AddTokensUsed(50, 0)
	if !ag.BudgetExceeded() {
		t.Fatal("100/100 should be over budget")
	}
}

// TestSubAgentOutcomeBudget verifies a budget-exceeded final is classified as a
// failed (incomplete) run, alongside the existing FAILURE/SUCCESS cases.
func TestSubAgentOutcomeBudget(t *testing.T) {
	tests := []struct {
		name  string
		final string
		want  AgentStatus
	}{
		{"success", "SUCCESS: done", StatusCompleted},
		{"failure", "FAILURE: broke", StatusFailed},
		{"budget", budgetExceededMarker + ": token budget reached (30/25 tokens)", StatusFailed},
		{"plain", "here is the answer", StatusCompleted},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := subAgentOutcome(tc.final); got != tc.want {
				t.Errorf("subAgentOutcome(%q) = %q, want %q", tc.final, got, tc.want)
			}
		})
	}
}

// TestRunLoopStopsAtTokenBudget drives a model that never stops asking for a tool
// (a runaway loop). With a token budget set, the loop must stop gracefully with a
// BUDGET_EXCEEDED final long before the step cap, bounding cost (issue #28).
func TestRunLoopStopsAtTokenBudget(t *testing.T) {
	// The server always returns a tool call, so without a budget the loop would
	// run to maxSteps. Each round-trip reports 15 tokens of usage.
	fs := &fakeServer{responses: []map[string]interface{}{
		toolCallResponse("call_1", "calc", `{"expression":"2+2"}`),
	}}
	server := httptest.NewServer(http.HandlerFunc(fs.handler))
	defer server.Close()

	us, ag := newLoopSession(t, server.URL)
	ag.SetTokenBudget(25) // two 15-token round-trips trip it

	responses, err := us.ExecuteTaskLoop(context.Background(), "root", "loop forever")
	if err != nil {
		t.Fatalf("loop returned error: %v", err)
	}

	final := responses[len(responses)-1].Content
	if !strings.HasPrefix(final, budgetExceededMarker) {
		t.Errorf("final answer should start with %s, got %q", budgetExceededMarker, final)
	}

	// It must have stopped well short of the step cap: the budget trips after
	// the second round-trip, so only a few model calls happen.
	if fs.calls > 4 {
		t.Errorf("expected the budget to bound the loop to a few calls, got %d", fs.calls)
	}
	if got := ag.GetTokensUsed(); got < 25 {
		t.Errorf("expected recorded usage to reach the budget, got %d", got)
	}
}

// TestRunLoopNoBudgetRunsToCompletion is the control: with no budget the same
// kind of loop finishes normally when the model eventually answers.
func TestRunLoopNoBudgetRunsToCompletion(t *testing.T) {
	fs := &fakeServer{responses: []map[string]interface{}{
		toolCallResponse("call_1", "calc", `{"expression":"2+2"}`),
		finalResponse("The answer is 4."),
	}}
	server := httptest.NewServer(http.HandlerFunc(fs.handler))
	defer server.Close()

	us, ag := newLoopSession(t, server.URL)
	// No budget set: prior behavior preserved.

	responses, err := us.ExecuteTaskLoop(context.Background(), "root", "what is 2+2?")
	if err != nil {
		t.Fatalf("loop returned error: %v", err)
	}
	final := responses[len(responses)-1].Content
	if strings.HasPrefix(final, budgetExceededMarker) {
		t.Fatalf("unbudgeted loop should not stop for budget, got %q", final)
	}
	if !strings.Contains(final, "4") {
		t.Errorf("expected the normal final answer, got %q", final)
	}
	_ = ag
}
