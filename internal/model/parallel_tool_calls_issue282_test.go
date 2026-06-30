package model

import (
	"encoding/json"
	"testing"

	"gogent/internal/config"
)

// Issue #282, E1: a non-strict spawn_subagent (and a non-strict tool batch in
// general) must NOT cause a blanket parallel_tool_calls:false, while the OpenAI
// structured-outputs invariant (strict tool present on a response-format
// provider) is preserved. These tests complement response_format_test.go.

// spawnSubagentDef mimics the wire shape that toolDefsFromRegistry produces for
// the real spawn_subagent tool: a rich object schema but Strict left false.
func spawnSubagentDef() ToolDef {
	return ToolDef{
		Type: "function",
		Function: FunctionDef{
			Name:        "spawn_subagent",
			Description: "Delegate work to sub-agents.",
			Parameters: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"name":     map[string]interface{}{"type": "string"},
					"task":     map[string]interface{}{"type": "string"},
					"subtasks": map[string]interface{}{"type": "array"},
				},
			},
			// Strict intentionally omitted (false) — this is the invariant E1 relies on.
		},
	}
}

func readOnlyDef(name string) ToolDef {
	return ToolDef{
		Type: "function",
		Function: FunctionDef{
			Name:       name,
			Parameters: map[string]interface{}{"type": "object"},
		},
	}
}

// parallelToolCallsMustBeDisabled is the minimal trigger for the structured
// outputs invariant: strict tool present AND provider enforces response_format.
func TestParallelToolCallsMustBeDisabledMatrix(t *testing.T) {
	strict := ToolDef{Function: FunctionDef{Name: "structured_output", Strict: true}}
	cases := []struct {
		name        string
		respFmt     bool
		tools       []ToolDef
		wantDisable bool
	}{
		{"respfmt+strict -> disable", true, []ToolDef{strict}, true},
		{"respfmt+strict alongside spawn -> disable", true, []ToolDef{spawnSubagentDef(), strict}, true},
		{"respfmt+nonstrict spawn batch -> keep", true, []ToolDef{spawnSubagentDef(), readOnlyDef("read")}, false},
		{"respfmt+no tools -> keep", true, nil, false},
		{"respfmt+empty tools -> keep", true, []ToolDef{}, false},
		{"no respfmt+strict -> keep (anthropic-like)", false, []ToolDef{strict}, false},
		{"no respfmt+nonstrict -> keep", false, []ToolDef{spawnSubagentDef()}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			caps := Capabilities{SupportsResponseFormat: tc.respFmt}
			if got := parallelToolCallsMustBeDisabled(caps, tc.tools); got != tc.wantDisable {
				t.Errorf("parallelToolCallsMustBeDisabled = %v, want %v", got, tc.wantDisable)
			}
		})
	}
}

// Wire-level check (issue qualitative eval #2): a request advertising a
// non-strict spawn_subagent plus read-only tools must leave parallel_tool_calls
// unset (provider default), so batched spawns can still be emitted in parallel.
func TestBuildRequestNonStrictSpawnBatchKeepsParallelUnset(t *testing.T) {
	conn := NewModelConnection(
		&config.ProviderConnection{},
		&config.ModelConfig{Model: "gpt-4o"},
	)
	tools := []ToolDef{
		spawnSubagentDef(),
		readOnlyDef("read_file"),
		readOnlyDef("list_files"),
	}
	req := conn.buildRequest([]Message{{Role: RoleUser, Content: "hi"}}, false, tools, nil)
	if req.ParallelToolCalls != nil {
		t.Fatalf("ParallelToolCalls = %v, want nil for a non-strict spawn batch", *req.ParallelToolCalls)
	}

	// Confirm it never reaches the wire either.
	raw, err := buildBodyBytes(openAIAdapter{}, req)
	if err != nil {
		t.Fatalf("buildBody: %v", err)
	}
	var got struct {
		ParallelToolCalls *bool `json:"parallel_tool_calls"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.ParallelToolCalls != nil {
		t.Errorf("parallel_tool_calls = %v on the wire, want omitted", *got.ParallelToolCalls)
	}
}

// The structured-output invariant must survive the narrowing: even when a
// non-strict spawn_subagent is advertised alongside a strict tool, the strict
// tool still forces parallel_tool_calls:false on an OpenAI-compatible provider.
func TestBuildRequestStrictInvariantPreservedAlongsideSpawn(t *testing.T) {
	conn := NewModelConnection(
		&config.ProviderConnection{},
		&config.ModelConfig{Model: "gpt-4o"},
	)
	tools := []ToolDef{
		spawnSubagentDef(),
		{Type: "function", Function: FunctionDef{Name: "structured_output", Parameters: exampleSchema(), Strict: true}},
	}
	req := conn.buildRequest([]Message{{Role: RoleUser, Content: "hi"}}, false, tools, nil)
	if req.ParallelToolCalls == nil {
		t.Fatal("ParallelToolCalls unset; a strict tool in the batch must force it false")
	}
	if *req.ParallelToolCalls {
		t.Error("ParallelToolCalls = true, want false when a strict tool is present")
	}

	raw, err := buildBodyBytes(openAIAdapter{}, req)
	if err != nil {
		t.Fatalf("buildBody: %v", err)
	}
	var got struct {
		ParallelToolCalls *bool `json:"parallel_tool_calls"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.ParallelToolCalls == nil || *got.ParallelToolCalls {
		t.Errorf("parallel_tool_calls = %v on the wire, want false", got.ParallelToolCalls)
	}
}

// On a provider without response_format support (Anthropic), advertising a
// non-strict spawn batch never produces a parallel_tool_calls field — the
// narrowing leaves that provider's behaviour unchanged.
func TestBuildRequestSpawnBatchNoParallelFieldForAnthropic(t *testing.T) {
	conn := NewModelConnection(
		&config.ProviderConnection{APIType: "anthropic"},
		&config.ModelConfig{Model: "claude-x"},
	)
	tools := []ToolDef{spawnSubagentDef(), readOnlyDef("read_file")}
	req := conn.buildRequest([]Message{{Role: RoleUser, Content: "hi"}}, false, tools, nil)
	if req.ParallelToolCalls != nil {
		t.Errorf("ParallelToolCalls = %v, want nil on Anthropic", *req.ParallelToolCalls)
	}
}
