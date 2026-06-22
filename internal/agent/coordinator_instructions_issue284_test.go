package agent

import (
	"strings"
	"testing"

	"gogent/internal/config"
)

// Issue #284 (H4): coordinatorInstructions must produce three distinct shapes
// keyed on tool exposure — both styles (default), interactive-only, one-shot-only
// — and the "both" / interactive shapes must give a CONCRETE async recipe
// (launch_agent -> keep working -> wait_agent_event -> react), not just a tool
// list. These tests pin that prompt content so a regression that drops the recipe
// or mislabels the branch is caught.

func cfgFor(m config.SubAgentExecutionModel) config.SubAgentConfig {
	return config.SubAgentConfig{ExecutionModel: m}
}

// TestCoordinatorInstructionsBothDescribesEachStyle asserts the default ("both")
// prompt names BOTH coordination styles and teaches the choice between them.
func TestCoordinatorInstructionsBothDescribesBothStyles(t *testing.T) {
	got := coordinatorInstructions(cfgFor(config.SubAgentBothModel))

	mustContain := []string{
		"spawn_subagent",   // blocking primitive
		"launch_agent",     // async primitive
		"wait_agent_event", // async harvest step
		"agent_send",       // answering a CLARIFY
		"subtasks",         // batched blocking fan-out
	}
	for _, s := range mustContain {
		if !strings.Contains(got, s) {
			t.Errorf("both-styles instructions missing %q", s)
		}
	}

	// The whole point of #284 is teaching WHEN to use each. Require the
	// blocking-vs-async distinction to be spelled out.
	lower := strings.ToLower(got)
	if !strings.Contains(lower, "block") {
		t.Error("both-styles instructions should describe the blocking style")
	}
	if !strings.Contains(lower, "fire-and-forget") && !strings.Contains(lower, "keep working") &&
		!strings.Contains(lower, "background") {
		t.Error("both-styles instructions should describe the fire-and-forget / keep-working style")
	}
}

// TestCoordinatorInstructionsBothRecipeOrdering checks the async recipe is a
// concrete ordered sequence: launch_agent appears before wait_agent_event, which
// is the latency win the issue asks the prompt to teach.
func TestCoordinatorInstructionsBothRecipeOrdering(t *testing.T) {
	got := coordinatorInstructions(cfgFor(config.SubAgentBothModel))
	launchAt := strings.Index(got, "launch_agent")
	waitAt := strings.Index(got, "wait_agent_event")
	if launchAt < 0 || waitAt < 0 {
		t.Fatalf("recipe primitives missing (launch=%d wait=%d)", launchAt, waitAt)
	}
	if launchAt > waitAt {
		t.Error("recipe should introduce launch_agent before wait_agent_event")
	}
}

// TestCoordinatorInstructionsInteractiveOnly asserts the interactive-only prompt
// gives the async recipe and does NOT advertise the blocking spawn_subagent tool
// (which is stripped from that registry).
func TestCoordinatorInstructionsInteractiveOnly(t *testing.T) {
	got := coordinatorInstructions(cfgFor(config.SubAgentInteractiveModel))

	for _, s := range []string{"launch_agent", "wait_agent_event", "agent_send"} {
		if !strings.Contains(got, s) {
			t.Errorf("interactive-only instructions missing %q", s)
		}
	}
	if strings.Contains(got, "spawn_subagent") {
		t.Error("interactive-only instructions must not advertise spawn_subagent (it is stripped from the registry)")
	}
	// Concrete recipe ordering.
	if strings.Index(got, "launch_agent") > strings.Index(got, "wait_agent_event") {
		t.Error("interactive recipe should introduce launch_agent before wait_agent_event")
	}
}

// TestCoordinatorInstructionsOneShotOnly asserts the one-shot prompt describes
// blocking spawn_subagent and does NOT advertise the async tools that are
// stripped from that registry.
func TestCoordinatorInstructionsOneShotOnly(t *testing.T) {
	got := coordinatorInstructions(cfgFor(config.SubAgentOneShotModel))

	if !strings.Contains(got, "spawn_subagent") {
		t.Error("one-shot instructions must describe spawn_subagent")
	}
	for _, s := range []string{"launch_agent", "wait_agent_event", "agent_send", "agent_terminate"} {
		if strings.Contains(got, s) {
			t.Errorf("one-shot instructions must not advertise async tool %q (stripped from registry)", s)
		}
	}
}

// TestCoordinatorInstructionsEmptyDefaultsOneShot guards the zero-value config:
// it must select the stable one-shot branch, not the async or both branch.
func TestCoordinatorInstructionsEmptyDefaultsOneShot(t *testing.T) {
	got := coordinatorInstructions(config.SubAgentConfig{})
	if got != coordinatorInstructionsOneShot {
		t.Error("empty/zero-value config must select the one-shot instruction branch")
	}
}

// TestCoordinatorInstructionsBranchesAreDistinct ensures the three branches are
// genuinely different strings — a copy/paste bug that aliased two branches would
// silently mis-instruct a mode.
func TestCoordinatorInstructionsBranchesAreDistinct(t *testing.T) {
	both := coordinatorInstructions(cfgFor(config.SubAgentBothModel))
	inter := coordinatorInstructions(cfgFor(config.SubAgentInteractiveModel))
	one := coordinatorInstructions(cfgFor(config.SubAgentOneShotModel))
	if both == inter || both == one || inter == one {
		t.Errorf("instruction branches must be distinct: both==inter:%v both==one:%v inter==one:%v",
			both == inter, both == one, inter == one)
	}
}

// TestPlanModeDelegationMentionsAsyncUnavailable guards the plan-mode regression
// note (#281/#284 interaction): when delegation is allowed in plan mode it names
// the blocking spawn_subagent and explicitly says the async launch_agent family
// is NOT available, so the model doesn't emit calls that fail as unknown.
func TestPlanModeDelegationMentionsAsyncUnavailable(t *testing.T) {
	withDelegate := planModeSystemPromptWith("BASE", true)
	if !strings.Contains(withDelegate, "spawn_subagent") {
		t.Error("plan-mode (delegate) prompt should name the blocking spawn_subagent")
	}
	if !strings.Contains(withDelegate, "launch_agent") {
		t.Error("plan-mode (delegate) prompt should mention launch_agent is unavailable this turn")
	}
	lower := strings.ToLower(withDelegate)
	if !strings.Contains(lower, "not available") && !strings.Contains(lower, "unavailable") {
		t.Error("plan-mode (delegate) prompt should state the async family is not available this turn")
	}

	noDelegate := planModeSystemPromptWith("BASE", false)
	if !strings.Contains(strings.ToLower(noDelegate), "unavailable") {
		t.Error("plan-mode (no-delegate) prompt should state delegation is unavailable")
	}
	// When no delegation tool survived, the prompt must not advertise spawn_subagent.
	if strings.Contains(noDelegate, "spawn_subagent call") {
		t.Error("plan-mode (no-delegate) prompt should not advertise a spawn_subagent call")
	}
}
