package agent

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"gogent/internal/config"
	"gogent/internal/model"
)

// newSlotSession builds an offline UserSession whose root agent can spawn
// sub-agents through newSubAgent without touching the network: newSubAgent only
// constructs child model sessions and wires the tree, it never makes a request.
// maxSub sets the per-parent fan-out budget under test (issue #280, change F).
func newSlotSession(t *testing.T, maxSub int) (*UserSession, *Agent) {
	t.Helper()
	conn := newTestModelConnection()
	root := NewAgent("root", model.NewModelSession("root", conn))
	us := NewUserSession("s", root)
	cfg := config.DefaultSubAgentConfig() // one-shot, no recursion
	cfg.MaxSubAgents = maxSub
	us.SetSubAgentConfig(cfg)
	return us, root
}

// --- ActiveSubAgentCount: the helper the slot check now relies on -------------

// TestActiveSubAgentCount_OnlyNonTerminalChildrenCount pins the core accounting
// contract: completed/failed children stay in the tree but no longer occupy a
// delegation slot, while running/waiting children do.
func TestActiveSubAgentCount_OnlyNonTerminalChildrenCount(t *testing.T) {
	conn := newTestModelConnection()
	root := NewAgent("root", model.NewModelSession("root", conn))

	if got := root.ActiveSubAgentCount(); got != 0 {
		t.Fatalf("ActiveSubAgentCount with no children = %d, want 0", got)
	}

	statuses := map[string]AgentStatus{
		"run":  StatusRunning,
		"wait": StatusWaiting,
		"done": StatusCompleted,
		"fail": StatusFailed,
	}
	for name, st := range statuses {
		child := NewAgent("root/"+name, model.NewModelSession(name, conn))
		child.SetStatus(st)
		root.AddSubAgent(child)
	}

	// running + waiting count; completed + failed do not.
	if got := root.ActiveSubAgentCount(); got != 2 {
		t.Errorf("ActiveSubAgentCount = %d, want 2 (running+waiting only; completed/failed excluded)", got)
	}
	// All four remain in the tree — the slot fix must not drop terminal agents.
	if got := len(root.GetSubAgents()); got != 4 {
		t.Errorf("GetSubAgents len = %d, want 4 (terminal agents stay in the tree)", got)
	}
}

// TestActiveSubAgentCount_DirectChildrenOnly guards that the count is per-parent
// (its own direct children), not a recursive tree-wide tally — otherwise a
// grandchild would wrongly consume the parent's slot budget.
func TestActiveSubAgentCount_DirectChildrenOnly(t *testing.T) {
	conn := newTestModelConnection()
	root := NewAgent("root", model.NewModelSession("root", conn))

	child := NewAgent("root/c", model.NewModelSession("c", conn))
	child.SetStatus(StatusRunning)
	root.AddSubAgent(child)

	grand := NewAgent("root/c/g", model.NewModelSession("g", conn))
	grand.SetStatus(StatusRunning)
	child.AddSubAgent(grand)

	if got := root.ActiveSubAgentCount(); got != 1 {
		t.Errorf("root.ActiveSubAgentCount = %d, want 1 (grandchild belongs to the child's budget, not root's)", got)
	}
	if got := child.ActiveSubAgentCount(); got != 1 {
		t.Errorf("child.ActiveSubAgentCount = %d, want 1", got)
	}
}

// --- newSubAgent slot enforcement --------------------------------------------

// TestNewSubAgent_ActiveSlotsCapEnforced confirms the budget still bounds
// *concurrent* fan-out: with all children left running, the (budget+1)th spawn
// is rejected with the documented "max sub-agents" error.
func TestNewSubAgent_ActiveSlotsCapEnforced(t *testing.T) {
	const budget = 2
	us, _ := newSlotSession(t, budget)

	for i := 0; i < budget; i++ {
		if _, err := us.newSubAgent("root", fmt.Sprintf("a%d", i), "task", KindTool); err != nil {
			t.Fatalf("spawn #%d within budget failed: %v", i+1, err)
		}
	}
	_, err := us.newSubAgent("root", "overflow", "task", KindTool)
	if err == nil {
		t.Fatalf("spawn beyond budget %d succeeded, want rejection", budget)
	}
	if !strings.Contains(err.Error(), "max sub-agents") {
		t.Errorf("error = %q, want it to mention %q", err.Error(), "max sub-agents")
	}
}

// TestNewSubAgent_TerminalChildrenFreeSlots is the headline acceptance test for
// issue #280 change F: once the running children reach a terminal state they
// must release their slots so a fresh spawn succeeds, yet they remain in the
// tree for the UI.
func TestNewSubAgent_TerminalChildrenFreeSlots(t *testing.T) {
	const budget = 2
	us, root := newSlotSession(t, budget)

	first := mustSpawn(t, us, "first")
	second := mustSpawn(t, us, "second")

	// Budget full while both run.
	if _, err := us.newSubAgent("root", "third", "task", KindTool); err == nil {
		t.Fatalf("third spawn succeeded while %d slots full, want rejection", budget)
	}

	// Drive both to terminal states (one each, to cover both terminal values).
	first.SetStatus(StatusCompleted)
	second.SetStatus(StatusFailed)

	if got := root.ActiveSubAgentCount(); got != 0 {
		t.Fatalf("after both children terminal, ActiveSubAgentCount = %d, want 0", got)
	}

	// The previously-blocked spawn now succeeds — this is the bug fix.
	if _, err := us.newSubAgent("root", "third", "task", KindTool); err != nil {
		t.Fatalf("spawn after children went terminal failed: %v (slot fix regressed)", err)
	}

	// Terminal agents are kept in the tree alongside the new one.
	if got := len(root.GetSubAgents()); got != 3 {
		t.Errorf("tree has %d children, want 3 (2 terminal kept + 1 new)", got)
	}
}

// TestNewSubAgent_LongSessionDoesNotExhaustBudget reproduces the exact failure
// mode the issue describes: over a long session, serially spawning and
// completing far more than `max_subagents` helpers must never run out of slots,
// because terminal children no longer count. Before the fix the (budget+1)th
// cumulative spawn errored even though nothing was still running.
func TestNewSubAgent_LongSessionDoesNotExhaustBudget(t *testing.T) {
	const budget = 4
	const cycles = 20 // well past the cumulative budget
	us, root := newSlotSession(t, budget)

	for i := 0; i < cycles; i++ {
		child, err := us.newSubAgent("root", fmt.Sprintf("c%d", i), "task", KindTool)
		if err != nil {
			t.Fatalf("cycle %d: spawn failed: %v (terminal children still consuming slots?)", i, err)
		}
		// Finish it immediately, freeing the slot for the next cycle.
		child.SetStatus(StatusCompleted)
	}

	if got := root.ActiveSubAgentCount(); got != 0 {
		t.Errorf("after %d completed cycles, active count = %d, want 0", cycles, got)
	}
	if got := len(root.GetSubAgents()); got != cycles {
		t.Errorf("tree retained %d children, want %d (terminal agents preserved)", got, cycles)
	}
}

// TestNewSubAgent_MixedActiveAndTerminal checks the boundary precisely: with a
// budget of 3 and one completed child, exactly enough slots free up to reach
// three *active* children, and the next spawn is then rejected.
func TestNewSubAgent_MixedActiveAndTerminal(t *testing.T) {
	const budget = 3
	us, root := newSlotSession(t, budget)

	a := mustSpawn(t, us, "a")
	mustSpawn(t, us, "b")
	mustSpawn(t, us, "c") // 3 active -> full

	if _, err := us.newSubAgent("root", "d", "task", KindTool); err == nil {
		t.Fatal("4th spawn succeeded with 3 active, want rejection")
	}

	a.SetStatus(StatusCompleted) // frees one slot (active 2)

	d := mustSpawn(t, us, "d") // back to 3 active
	if got := root.ActiveSubAgentCount(); got != 3 {
		t.Fatalf("active count = %d, want 3", got)
	}
	_ = d

	// Full again: another spawn must be rejected.
	if _, err := us.newSubAgent("root", "e", "task", KindTool); err == nil {
		t.Fatal("spawn succeeded again at 3 active, want rejection")
	}
}

// TestNewSubAgent_ParentNotFound is a guard around the unchanged error path so
// the slot rework did not swallow the not-found case.
func TestNewSubAgent_ParentNotFound(t *testing.T) {
	us, _ := newSlotSession(t, 4)
	_, err := us.newSubAgent("does-not-exist", "x", "task", KindTool)
	if err == nil {
		t.Fatal("newSubAgent with unknown parent succeeded, want NotFoundError")
	}
	var nf *NotFoundError
	if !errors.As(err, &nf) {
		t.Errorf("error = %v (%T), want *NotFoundError", err, err)
	}
}

// mustSpawn spawns a one-shot sub-agent and fails the test on error. The child
// is left in its default StatusRunning state (set by newSubAgent), i.e. active.
func mustSpawn(t *testing.T, us *UserSession, name string) *Agent {
	t.Helper()
	child, err := us.newSubAgent("root", name, "task", KindTool)
	if err != nil {
		t.Fatalf("spawn %q failed: %v", name, err)
	}
	if child.GetStatus() != StatusRunning {
		t.Fatalf("freshly spawned %q has status %q, want %q", name, child.GetStatus(), StatusRunning)
	}
	return child
}

// --- A. coordinatorInstructions reframing ------------------------------------

// TestCoordinatorInstructions_OneShotLatencyReframing verifies acceptance
// criterion A: the one-shot delegation block is reframed around latency, keeps
// the batch-into-one-call instruction and the SUCCESS/FAILURE contract, and
// drops the old "otherwise do the work yourself" close that biased toward inline
// work.
func TestCoordinatorInstructions_OneShotLatencyReframing(t *testing.T) {
	cfg := config.DefaultSubAgentConfig() // one-shot
	got := coordinatorInstructions(cfg)
	lower := strings.ToLower(got)

	// (a) latency / parallelism framing present.
	if !strings.Contains(lower, "latency") {
		t.Error("one-shot coordinator instructions never mention latency (acceptance A)")
	}
	if !strings.Contains(got, "CONCURRENTLY") && !strings.Contains(lower, "concurrent") {
		t.Error("instructions do not describe concurrent execution")
	}

	// (b) the batch-into-ONE-call instruction (gates the concurrent fast path).
	if !strings.Contains(got, "subtasks") {
		t.Error("instructions dropped the \"subtasks\" array guidance")
	}
	if !strings.Contains(lower, "one call") && !strings.Contains(lower, "one spawn_subagent call") {
		t.Error("instructions no longer tell the model to batch into ONE call")
	}

	// (c) SUCCESS/FAILURE contract intact.
	if !strings.Contains(got, "SUCCESS") || !strings.Contains(got, "FAILURE") {
		t.Error("instructions dropped the SUCCESS/FAILURE contract")
	}

	// (d) the inline-biasing close is softened, not the old wording.
	if strings.Contains(lower, "otherwise do the work yourself") {
		t.Error("old \"otherwise do the work yourself\" close is still present (should be reframed)")
	}

	// concrete tool-tied triggers (diagnostics/verify/grep, multiple modules).
	hasTrigger := strings.Contains(lower, "diagnostics") ||
		strings.Contains(lower, "modules") || strings.Contains(lower, "verify")
	if !hasTrigger {
		t.Error("instructions lack concrete tool-tied triggers (diagnostics/verify/modules)")
	}
}
