package agent

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Issue #281 regression guard: when no delegation tool survives the plan-mode
// filter (e.g. interactive sub-agent mode strips spawn_subagent and
// CloneForPlanMode drops the non-read-only interactive coordination tools), the
// plan-mode prompt must NOT promise delegation. Instead it must neutralize the
// base prompt's stale coordinator guidance so the model does not emit
// spawn/launch calls that fail as unknown this turn.
//
// These cover the canDelegate=false branch the driver added after the first
// review; the rest of the suite (plan_mode_subagent_issue281_test.go) covers the
// canDelegate=true path.

// hasSub reports whether s mentions the delegation promise (the batched
// "subtasks" instruction is the operative thing that requires a real tool).
func plan281PromisesDelegation(prompt string) bool {
	lower := strings.ToLower(prompt)
	return strings.Contains(lower, "subtasks") ||
		strings.Contains(lower, "you may delegate")
}

// TestPlanModeSystemPromptWithoutDelegation asserts the no-delegation branch of
// planModeSystemPromptWith: it keeps the PLAN MODE / read-only framing and the
// base prompt, drops the spawn_subagent promise, and explicitly tells the model
// delegation is unavailable so the base prompt's coordinator block is overridden.
func TestPlanModeSystemPromptWithoutDelegation(t *testing.T) {
	const base = "BASE-SENTINEL"
	got := planModeSystemPromptWith(base, false)
	lower := strings.ToLower(got)

	if !strings.Contains(got, base) {
		t.Error("no-delegation prompt dropped the base prompt")
	}
	if !strings.Contains(got, "PLAN MODE") {
		t.Error("no-delegation prompt no longer announces PLAN MODE")
	}
	if plan281PromisesDelegation(got) {
		t.Errorf("no-delegation prompt still promises spawn_subagent delegation: %q", got)
	}
	// It must neutralize the base prompt's stale launch/spawn guidance.
	if !strings.Contains(lower, "unavailable") {
		t.Error("no-delegation prompt does not state delegation is unavailable")
	}
	if !strings.Contains(lower, "ignore") && !strings.Contains(lower, "yourself") {
		t.Error("no-delegation prompt does not override earlier delegation guidance")
	}
	// Still asks for the plan via todo and a final answer.
	if !strings.Contains(lower, "todo") {
		t.Error("no-delegation prompt dropped the todo planning instruction")
	}
}

// TestPlanModeSystemPromptWithDelegationContrast pins that the two branches
// genuinely differ: the canDelegate=true branch promises delegation, the false
// branch does not. Guards against a refactor that collapses both to one text.
func TestPlanModeSystemPromptWithDelegationContrast(t *testing.T) {
	with := planModeSystemPromptWith("B", true)
	without := planModeSystemPromptWith("B", false)
	if !plan281PromisesDelegation(with) {
		t.Error("canDelegate=true branch should promise delegation")
	}
	if plan281PromisesDelegation(without) {
		t.Error("canDelegate=false branch must not promise delegation")
	}
	if with == without {
		t.Error("both branches produced identical prompts; the canDelegate gate is a no-op")
	}
}

// TestPlanModeTurnWithoutSpawnToolOmitsDelegationPromise is the end-to-end
// regression guard for the canDelegate wiring in ExecuteTaskLoop: a plan-mode
// turn whose registry has NO spawn_subagent (newPlanSession registers only
// read+write — the interactive-mode shape) must send a system prompt that does
// NOT promise delegation. Before the fix the prompt unconditionally told the
// model to call spawn_subagent, which would fail as unknown.
func TestPlanModeTurnWithoutSpawnToolOmitsDelegationPromise(t *testing.T) {
	fs := &planFakeServer{responses: []map[string]interface{}{finalOnly("a plan")}}
	server := httptest.NewServer(http.HandlerFunc(fs.handler))
	defer server.Close()

	us, ag := newPlanSession(t, server.URL) // registry: read (ro) + write — no spawn_subagent
	if ag.ToolRegistry.Get("spawn_subagent") != nil {
		t.Fatal("test precondition: newPlanSession should not register spawn_subagent")
	}
	us.SetPlanMode(true)

	if _, err := us.ExecuteTaskLoop(context.Background(), "root", "plan it"); err != nil {
		t.Fatalf("ExecuteTaskLoop: %v", err)
	}

	fs.mu.Lock()
	defer fs.mu.Unlock()
	if len(fs.messages) == 0 {
		t.Fatal("no requests recorded")
	}
	var systemText string
	for _, m := range fs.messages[0] {
		if role, _ := m["role"].(string); role == "system" {
			systemText, _ = m["content"].(string)
		}
	}
	// Isolate the PLAN MODE section — that is the part ExecuteTaskLoop's
	// canDelegate gate controls. The base coordinator block (config-driven, not
	// touched by issue #281) may itself mention spawn_subagent in one-shot config,
	// so asserting on the whole prompt would test the wrong thing.
	idx := strings.Index(systemText, "## PLAN MODE")
	if idx < 0 {
		t.Fatalf("system prompt missing PLAN MODE section: %q", systemText)
	}
	planSection := systemText[idx:]
	if plan281PromisesDelegation(planSection) {
		t.Errorf("plan-mode section without spawn_subagent still promised delegation: %q", planSection)
	}
	if !strings.Contains(strings.ToLower(planSection), "unavailable") {
		t.Errorf("plan-mode section without a delegation tool did not neutralize delegation guidance: %q", planSection)
	}
	// And it must explicitly override the base prompt's stale coordinator block.
	if !strings.Contains(strings.ToLower(planSection), "ignore") {
		t.Errorf("plan-mode section does not tell the model to ignore earlier delegation guidance: %q", planSection)
	}
}
