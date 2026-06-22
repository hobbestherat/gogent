package agent

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"

	"gogent/internal/tool"
)

// Issue #281: bounded, read-only sub-agent delegation in plan mode.
//
// These tests are the TESTER's independent coverage of the four acceptance
// criteria:
//  1. spawn_subagent survives the plan-mode tool filter (root registry).
//  2. a sub-agent spawned during plan mode inherits ONLY read-only tools
//     (no write/edit/multi_edit/apply_patch/shell).
//  3. planModeSystemPrompt permits read-only delegation and no longer claims
//     the sub-agent tools are unavailable.
//  4. a plan-mode turn can actually invoke spawn_subagent.
//
// They live in package agent so they can reach the unexported planKeptTools,
// planModeSystemPrompt and newSubAgent directly.

// plan281ReadOnlyTools are the read-only investigation tools a plan-mode child
// must retain.
var plan281ReadOnlyTools = []string{"read", "grep", "glob", "list", "diagnostics"}

// plan281WriteTools are the side-effecting tools a plan-mode root (and its
// children) must NOT have.
var plan281WriteTools = []string{"write", "edit", "multi_edit", "apply_patch", "shell"}

func plan281Noop(map[string]interface{}, tool.ToolContext) (interface{}, error) { return "ok", nil }

// plan281RichRegistry builds a registry mirroring a real root: read-only
// investigation tools, side-effecting tools, the kept planning extras (todo,
// structured_output) and the spawn_subagent coordination tool.
func plan281RichRegistry() *tool.ToolRegistry {
	reg := tool.NewToolRegistry()
	for _, n := range plan281ReadOnlyTools {
		reg.Register(&tool.Tool{Name: n, ReadOnly: true, Description: "d", InputSchema: nil, Execute: plan281Noop})
	}
	for _, n := range plan281WriteTools {
		reg.Register(&tool.Tool{Name: n, ReadOnly: false, Description: "d", InputSchema: nil, Execute: plan281Noop})
	}
	// Kept planning extras and the coordination tool are side-effecting (ReadOnly
	// false) — they survive plan mode only because they are named in planKeptTools.
	for _, n := range []string{"todo", "structured_output", "spawn_subagent"} {
		reg.Register(&tool.Tool{Name: n, ReadOnly: false, Description: "d", InputSchema: nil, Execute: plan281Noop})
	}
	return reg
}

func plan281Has(reg *tool.ToolRegistry, name string) bool { return reg.Get(name) != nil }

// TestPlanKeptToolsIncludesSpawnSubagent pins criterion 1 at the source: the
// curated keep-list retains spawn_subagent alongside the planning extras, so the
// plan-mode filter never strips it.
func TestPlanKeptToolsIncludesSpawnSubagent(t *testing.T) {
	want := map[string]bool{"spawn_subagent": false, "todo": false, "structured_output": false}
	for _, n := range planKeptTools {
		if _, ok := want[n]; ok {
			want[n] = true
		}
	}
	for n, found := range want {
		if !found {
			t.Errorf("planKeptTools = %v, missing %q", planKeptTools, n)
		}
	}
}

// TestPlanModeRootRegistryKeepsSpawnSubagent pins criterion 1 end-to-end at the
// registry layer: applying the plan-mode filter with the real planKeptTools to a
// realistic root registry keeps spawn_subagent and every read-only tool, but
// strips all side-effecting tools.
func TestPlanModeRootRegistryKeepsSpawnSubagent(t *testing.T) {
	plan := plan281RichRegistry().CloneForPlanMode(planKeptTools...)

	if !plan281Has(plan, "spawn_subagent") {
		t.Error("plan-mode root registry stripped spawn_subagent (criterion 1: it must survive)")
	}
	for _, n := range plan281ReadOnlyTools {
		if !plan281Has(plan, n) {
			t.Errorf("plan-mode root registry stripped read-only tool %q", n)
		}
	}
	for _, n := range []string{"todo", "structured_output"} {
		if !plan281Has(plan, n) {
			t.Errorf("plan-mode root registry stripped kept extra %q", n)
		}
	}
	for _, n := range plan281WriteTools {
		if plan281Has(plan, n) {
			t.Errorf("plan-mode root registry retained side-effecting tool %q (must be stripped)", n)
		}
	}
}

// plan281SessionWithRegistry wires a UserSession whose root agent owns the given
// registry and a model session (needed by newSubAgent's depth/ThoughtTrain
// checks). No server is contacted — newSubAgent only builds the child.
func plan281SessionWithRegistry(t *testing.T, reg *tool.ToolRegistry) (*UserSession, *Agent) {
	t.Helper()
	// Reuse newPlanSession's wiring (it gives the root a real model session) then
	// swap in the richer registry.
	us, ag := newPlanSession(t, "http://127.0.0.1:0")
	ag.SetToolRegistry(reg)
	return us, ag
}

// TestPlanModeChildRegistryIsReadOnly is criterion 2 (the explicitly required
// test): a sub-agent spawned while the root is in plan mode receives the
// read-only tool set and CANNOT write/edit/multi_edit/apply_patch/shell.
//
// It reproduces the real swap ExecuteTaskLoop performs (root registry ->
// CloneForPlanMode(planKeptTools...)) before delegating, then asserts on the
// child registry newSubAgent builds.
func TestPlanModeChildRegistryIsReadOnly(t *testing.T) {
	us, ag := plan281SessionWithRegistry(t, plan281RichRegistry())

	// Mimic the plan-mode swap done per-turn in ExecuteTaskLoop.
	ag.SetToolRegistry(ag.ToolRegistry.CloneForPlanMode(planKeptTools...))

	child, err := us.newSubAgent("root", "investigate", "summarize package X", KindTool)
	if err != nil {
		t.Fatalf("newSubAgent during plan mode: %v", err)
	}
	if child.ToolRegistry == nil {
		t.Fatal("plan-mode child has no tool registry")
	}

	for _, n := range plan281ReadOnlyTools {
		if !plan281Has(child.ToolRegistry, n) {
			t.Errorf("plan-mode child is missing read-only tool %q (it should investigate)", n)
		}
	}
	for _, n := range plan281WriteTools {
		if plan281Has(child.ToolRegistry, n) {
			t.Errorf("plan-mode child can use side-effecting tool %q — plan mode must stay read-only", n)
		}
	}
}

// TestNonPlanModeChildRegistryHasWrites is the contrast control: outside plan
// mode a delegated child still inherits the side-effecting tools. This proves
// the read-only restriction in the test above comes specifically from the
// plan-mode swap and is not an unconditional regression in newSubAgent.
func TestNonPlanModeChildRegistryHasWrites(t *testing.T) {
	us, _ := plan281SessionWithRegistry(t, plan281RichRegistry())

	child, err := us.newSubAgent("root", "do-work", "apply a change", KindTool)
	if err != nil {
		t.Fatalf("newSubAgent outside plan mode: %v", err)
	}
	for _, n := range plan281WriteTools {
		if !plan281Has(child.ToolRegistry, n) {
			t.Errorf("non-plan child is missing side-effecting tool %q (only plan mode should strip it)", n)
		}
	}
}

// TestPlanModeSystemPromptPermitsReadOnlyDelegation is criterion 3. The prompt
// must (a) still layer on the base, (b) still declare PLAN MODE / read-only,
// (c) no longer claim sub-agent tools are unavailable, (d) permit read-only
// delegation, and (e) stress the sub-agents are themselves read-only.
func TestPlanModeSystemPromptPermitsReadOnlyDelegation(t *testing.T) {
	const base = "BASE-AGENT-PROMPT-SENTINEL"
	got := planModeSystemPrompt(base)
	lower := strings.ToLower(got)

	if !strings.Contains(got, base) {
		t.Error("planModeSystemPrompt dropped the base prompt it should layer onto")
	}
	if !strings.Contains(got, "PLAN MODE") {
		t.Error("planModeSystemPrompt no longer announces PLAN MODE")
	}

	// (c) Regression guard: the old prompt claimed "the sub-agent tools are
	// unavailable this turn". That claim must be gone.
	for _, banned := range []string{
		"sub-agent tools are unavailable",
		"subagent tools are unavailable",
		"sub-agent tool is unavailable",
	} {
		if strings.Contains(lower, banned) {
			t.Errorf("planModeSystemPrompt still claims sub-agent tools unavailable: contains %q", banned)
		}
	}
	// spawn_subagent must not be listed among the unavailable/modifying tools.
	if strings.Contains(lower, "spawn_subagent") && strings.Contains(lower, "unavailable") {
		// Allowed only if "unavailable" does not refer to spawn_subagent. Be strict:
		// the implementation lists the unavailable tools as a parenthical of write
		// tools — spawn_subagent must not appear in that list.
		idx := strings.Index(lower, "unavailable")
		window := lower[maxInt(0, idx-120):idx]
		if strings.Contains(window, "spawn_subagent") {
			t.Error("planModeSystemPrompt lists spawn_subagent as unavailable")
		}
	}

	// (d) It must actively permit delegation.
	if !strings.Contains(lower, "delegate") && !strings.Contains(lower, "spawn_subagent") && !strings.Contains(lower, "sub-agent") {
		t.Error("planModeSystemPrompt does not permit/mention sub-agent delegation")
	}
	// Guidance to batch into a single concurrent call (the bounded fast path).
	if !strings.Contains(lower, "subtasks") {
		t.Error("planModeSystemPrompt should point at the batched subtasks array for parallel investigation")
	}

	// (e) The sub-agents must be told they are read-only too.
	if !strings.Contains(lower, "read-only") {
		t.Error("planModeSystemPrompt does not state delegation stays read-only")
	}
	if !strings.Contains(lower, "must not") && !strings.Contains(lower, "not write") {
		t.Error("planModeSystemPrompt does not forbid sub-agents from making changes")
	}
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// plan281SpawnCall is a model response that emits a single spawn_subagent tool
// call, used to prove the tool is invocable during a plan-mode turn.
func plan281SpawnCall() map[string]interface{} {
	return map[string]interface{}{
		"choices": []map[string]interface{}{{
			"index": 0,
			"message": map[string]interface{}{
				"role": "assistant",
				"tool_calls": []map[string]interface{}{{
					"id":   "call_spawn",
					"type": "function",
					"function": map[string]interface{}{
						"name":      "spawn_subagent",
						"arguments": `{"name":"inv","task":"summarize pkg"}`,
					},
				}},
			},
			"finish_reason": "tool_calls",
		}},
		"usage": map[string]interface{}{"prompt_tokens": 5, "completion_tokens": 3, "total_tokens": 8},
	}
}

// TestPlanModeTurnInvokesSpawnSubagent is criterion 4: a plan-mode turn both
// advertises spawn_subagent to the model AND executes it when the model calls
// it, while the side-effecting write tool stays stripped.
func TestPlanModeTurnInvokesSpawnSubagent(t *testing.T) {
	fs := &planFakeServer{responses: []map[string]interface{}{
		plan281SpawnCall(),
		finalOnly("the plan"),
	}}
	server := httptest.NewServer(http.HandlerFunc(fs.handler))
	defer server.Close()

	us, ag := newPlanSession(t, server.URL)

	// Register a recording spawn_subagent stub (side-effecting, kept only via
	// planKeptTools) so we can observe it actually running this turn.
	var mu sync.Mutex
	spawnCalls := 0
	reg := ag.ToolRegistry
	reg.Register(&tool.Tool{
		Name: "spawn_subagent", ReadOnly: false, Description: "d", InputSchema: nil,
		Execute: func(map[string]interface{}, tool.ToolContext) (interface{}, error) {
			mu.Lock()
			spawnCalls++
			mu.Unlock()
			return "SUCCESS: investigated", nil
		},
	})

	us.SetPlanMode(true)
	if _, err := us.ExecuteTaskLoop(context.Background(), "root", "plan a change"); err != nil {
		t.Fatalf("ExecuteTaskLoop: %v", err)
	}

	// Advertised in the first request?
	fs.mu.Lock()
	advertised := toolNamesFromRequest(t, fs.tools[0])
	fs.mu.Unlock()
	sort.Strings(advertised)
	hasAdv := func(n string) bool {
		for _, a := range advertised {
			if a == n {
				return true
			}
		}
		return false
	}
	if !hasAdv("spawn_subagent") {
		t.Errorf("plan-mode turn did not advertise spawn_subagent; tools = %v", advertised)
	}
	if hasAdv("write") {
		t.Errorf("plan-mode turn advertised the write tool; tools = %v (must stay stripped)", advertised)
	}

	// Actually invoked?
	mu.Lock()
	got := spawnCalls
	mu.Unlock()
	if got != 1 {
		t.Errorf("spawn_subagent executed %d times in plan mode, want 1 (it must be invocable)", got)
	}
}
