package ui

import (
	"strings"
	"testing"

	"gogent/internal/agent"
)

// superWindow opens a live window wired with a synchronous supervisor check that
// returns the queued verdicts in order (issue #172). The synchronous override
// stands in for the production goroutine+Post dispatch, which the test event loop
// does not pump. checks records each (sessionID, goal) the check saw.
type superHarness struct {
	w        *Workbench
	sw       *SessionWindow
	sent     <-chan string
	verdicts []bool
	idx      int
	checks   int
	maxN     int
	enabled  bool
}

func newSuperHarness(t *testing.T, verdicts []bool) *superHarness {
	t.Helper()
	w := newTestWorkbench(t)
	h := &superHarness{w: w, verdicts: verdicts, maxN: 3, enabled: true}
	h.sent = recordSends(w)
	w.handlers.SupervisorEnabled = func() bool { return h.enabled }
	w.handlers.SupervisorMaxNudges = func() int { return h.maxN }
	w.handlers.OnSupervisorCheck = func(_, _ string) (bool, error) {
		v := false
		if h.idx < len(h.verdicts) {
			v = h.verdicts[h.idx]
		}
		h.idx++
		h.checks++
		return v, nil
	}
	h.sw = w.openWindow("s", "S")
	// Run the completion check synchronously and apply the verdict inline so the
	// test does not depend on the (unpumped) event-loop post queue.
	h.sw.runSupervisorCheck = func(goal string) {
		done, err := w.handlers.OnSupervisorCheck(h.sw.id, goal)
		h.sw.applySupervisorVerdict(goal, done, err)
	}
	return h
}

// idle drives a busy→idle edge — a turn runs then finishes with the terminal
// event the loop emits — which is the edge the watchdog fires on. Going busy
// first is required: the watchdog only evaluates on the busy→idle transition.
func (h *superHarness) idle() {
	if !h.sw.busy {
		h.sw.setBusy(true)
	}
	h.sw.apply(agent.SessionEvent{Type: agent.SessionEventFinal, Text: "done"})
}

// TestGoalCommandSetShowClear covers the /goal command surface (issue #172).
func TestGoalCommandSetShowClear(t *testing.T) {
	w := newTestWorkbench(t)
	sw := w.openWindow("s", "S")

	if !sw.handleSlashCommand("/goal ship the feature") {
		t.Fatal("/goal <text> should be handled locally")
	}
	if sw.goal != "ship the feature" {
		t.Fatalf("goal = %q, want %q", sw.goal, "ship the feature")
	}

	// Bare /goal shows the current goal.
	sw.handleSlashCommand("/goal")
	if !noteContains(sw, "ship the feature") {
		t.Error("/goal should echo the current goal")
	}

	// /goal clear removes it.
	sw.handleSlashCommand("/goal clear")
	if sw.goal != "" {
		t.Fatalf("goal after clear = %q, want empty", sw.goal)
	}
	if !noteContains(sw, "goal cleared") {
		t.Error("/goal clear should echo a cleared note")
	}
}

// TestSupervisorNudgesWhenGoalUnmet verifies the watchdog submits a supervisor
// nudge turn on idle when the completion check says the goal is not met, and that
// the nudge re-enters the normal send path (issue #172).
func TestSupervisorNudgesWhenGoalUnmet(t *testing.T) {
	h := newSuperHarness(t, []bool{false})
	h.sw.handleSlashCommand("/goal finish it")

	h.idle()
	got := waitSend(t, h.sent)
	if !agent.IsSupervisorNudge(got) {
		t.Fatalf("expected a supervisor nudge to be sent, got %q", got)
	}
	if !strings.Contains(got, "finish it") {
		t.Errorf("nudge %q should carry the goal", got)
	}
	if h.sw.nudgeCount != 1 {
		t.Errorf("nudgeCount = %d, want 1", h.sw.nudgeCount)
	}
}

// TestSupervisorQuietWhenGoalMet verifies a met goal stops the watchdog: no nudge
// is sent and the budget stays at zero (issue #172).
func TestSupervisorQuietWhenGoalMet(t *testing.T) {
	h := newSuperHarness(t, []bool{true})
	h.sw.handleSlashCommand("/goal finish it")

	h.idle()
	noSend(t, h.sent)
	if h.sw.nudgeCount != 0 {
		t.Errorf("nudgeCount = %d, want 0 (goal met)", h.sw.nudgeCount)
	}
	if !noteContains(h.sw, "goal satisfied") {
		t.Error("a met goal should surface a 'goal satisfied' note")
	}
}

// TestSupervisorDisabledNoNudge verifies the watchdog never fires when the
// supervisor flag is off, even with a goal set and an unmet verdict available
// (issue #172): default-off must not change behaviour.
func TestSupervisorDisabledNoNudge(t *testing.T) {
	h := newSuperHarness(t, []bool{false})
	h.enabled = false
	h.sw.handleSlashCommand("/goal finish it")

	h.idle()
	noSend(t, h.sent)
	if h.checks != 0 {
		t.Errorf("completion check ran %d times while disabled, want 0", h.checks)
	}
}

// TestSupervisorNoGoalNoNudge verifies no goal means no supervision.
func TestSupervisorNoGoalNoNudge(t *testing.T) {
	h := newSuperHarness(t, []bool{false})
	h.idle()
	noSend(t, h.sent)
	if h.checks != 0 {
		t.Errorf("completion check ran %d times with no goal, want 0", h.checks)
	}
}

// TestSupervisorBudgetExhaustion verifies nudges stop after MaxNudges and the
// give-up note is surfaced (issue #172). Each nudge re-enters the send path
// (setting the window busy); returning to idle is the next watchdog edge.
func TestSupervisorBudgetExhaustion(t *testing.T) {
	h := newSuperHarness(t, []bool{false, false, false, false, false})
	h.maxN = 2
	h.sw.handleSlashCommand("/goal keep going")

	// Edge 1 → nudge 1. The nudge submits a new turn (window goes busy).
	h.idle()
	if got := waitSend(t, h.sent); !agent.IsSupervisorNudge(got) {
		t.Fatalf("edge 1 should nudge, got %q", got)
	}
	if h.sw.nudgeCount != 1 {
		t.Fatalf("after edge 1 nudgeCount = %d, want 1", h.sw.nudgeCount)
	}

	// Edge 2 (the nudged turn finished) → nudge 2 (budget now full).
	h.idle()
	if got := waitSend(t, h.sent); !agent.IsSupervisorNudge(got) {
		t.Fatalf("edge 2 should nudge, got %q", got)
	}
	if h.sw.nudgeCount != 2 {
		t.Fatalf("after edge 2 nudgeCount = %d, want 2", h.sw.nudgeCount)
	}

	// Edge 3: budget exhausted → no nudge, give-up note.
	h.idle()
	noSend(t, h.sent)
	if !noteContains(h.sw, "still unmet after 2 nudges") {
		t.Error("budget exhaustion should surface a give-up note")
	}
}

// TestSupervisorUserMessageResetsBudget verifies a real user message resets the
// nudge budget so the watchdog gets a fresh allowance (issue #172).
func TestSupervisorUserMessageResetsBudget(t *testing.T) {
	h := newSuperHarness(t, []bool{false, false, false})
	h.maxN = 1
	h.sw.handleSlashCommand("/goal keep going")

	// Edge 1 → nudge (budget now full at 1).
	h.idle()
	if got := waitSend(t, h.sent); !agent.IsSupervisorNudge(got) {
		t.Fatalf("edge 1 should nudge, got %q", got)
	}
	if h.sw.nudgeCount != 1 {
		t.Fatalf("nudgeCount = %d, want 1", h.sw.nudgeCount)
	}
	// The nudged turn finishes; budget is exhausted so no further nudge.
	h.idle()
	noSend(t, h.sent)

	// A real user message resets the budget. Send it from idle.
	h.sw.input.SetText("user takes over")
	h.sw.submitFn()
	if got := waitSend(t, h.sent); got != "user takes over" {
		t.Fatalf("user send = %q", got)
	}
	if h.sw.nudgeCount != 0 {
		t.Fatalf("user message should reset nudgeCount, got %d", h.sw.nudgeCount)
	}

	// The user's turn finishes → fresh budget → a nudge fires again.
	h.idle()
	if got := waitSend(t, h.sent); !agent.IsSupervisorNudge(got) {
		t.Fatalf("after reset, idle should nudge again, got %q", got)
	}
}

// TestSupervisorGoalPersistsInLayout verifies the goal round-trips through the
// captured/applied layout (issue #172).
func TestSupervisorGoalPersistsInLayout(t *testing.T) {
	w := newTestWorkbench(t)
	sw := w.openWindow("s", "S")
	sw.handleSlashCommand("/goal persist me")

	layout := w.captureLayout()
	var found bool
	for _, e := range layout.Entries {
		if e.ID == "s" {
			found = true
			if e.Goal != "persist me" {
				t.Fatalf("layout goal = %q, want %q", e.Goal, "persist me")
			}
		}
	}
	if !found {
		t.Fatal("session entry missing from captured layout")
	}

	// Apply onto a fresh window and confirm the goal is restored.
	w2 := newTestWorkbench(t)
	sw2 := w2.openWindow("s", "S")
	w2.applyLayout(layout)
	if sw2.goal != "persist me" {
		t.Fatalf("restored goal = %q, want %q", sw2.goal, "persist me")
	}
}
