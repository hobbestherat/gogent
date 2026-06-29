package agent

import (
	"testing"

	"gogent/internal/tool"
)

// Issue #282 / #359: strictness is now opt-in per tool. Eligible read-only
// tools may advertise strict schemas, but spawn_subagent must stay non-strict so
// batched sub-agent fan-out is not suppressed by OpenAI's strict-tool
// parallel_tool_calls invariant.

// closedSchema is the shape required by strict tool-use.
func closedSchema() map[string]interface{} {
	return map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"task": map[string]interface{}{"type": "string"},
		},
		"required":             []string{"task"},
		"additionalProperties": false,
	}
}

func TestToolDefsFromRegistryMirrorsToolStrictFlag(t *testing.T) {
	reg := tool.NewToolRegistry()
	reg.Register(&tool.Tool{
		Name:        "spawn_subagent",
		Description: "Delegate work to sub-agents.",
		InputSchema: closedSchema(),
	})
	strictIssue359Tools := []string{"read", "glob", "list", "calc", "git", "grep"}
	for _, name := range strictIssue359Tools {
		reg.Register(&tool.Tool{
			Name:        name,
			Description: name + " strict tool.",
			ReadOnly:    true,
			Strict:      true,
			InputSchema: closedSchema(),
		})
	}
	reg.Register(&tool.Tool{
		Name:        "legacy_read_only",
		Description: "Read something without opting into strict mode.",
		ReadOnly:    true,
		InputSchema: map[string]interface{}{"type": "object"},
	})
	reg.Register(&tool.Tool{
		Name:        "structured_output",
		Description: "Closed schema tool.",
		Strict:      true,
		InputSchema: closedSchema(),
	})

	defs := toolDefsFromRegistry(reg)
	if len(defs) == 0 {
		t.Fatal("expected tool defs, got none")
	}
	got := make(map[string]bool, len(defs))
	for _, d := range defs {
		if d.Type != "function" {
			t.Errorf("tool %q: Type = %q, want function", d.Function.Name, d.Type)
		}
		got[d.Function.Name] = d.Function.Strict
		if d.Function.Name == "spawn_subagent" {
			if d.Function.Parameters == nil {
				t.Error("spawn_subagent: Parameters not carried through to the def")
			}
		}
	}

	want := map[string]bool{
		"spawn_subagent":    false,
		"legacy_read_only":  false,
		"structured_output": true,
	}
	for _, name := range strictIssue359Tools {
		want[name] = true
	}
	for name, strict := range want {
		if gotStrict, ok := got[name]; !ok {
			t.Errorf("%s missing from tool defs", name)
		} else if gotStrict != strict {
			t.Errorf("%s Strict = %v, want %v", name, gotStrict, strict)
		}
	}
}

func TestToolDefsFromRegistryNil(t *testing.T) {
	if defs := toolDefsFromRegistry(nil); defs != nil {
		t.Errorf("toolDefsFromRegistry(nil) = %v, want nil", defs)
	}
}

// allSpawnSubAgent gates the concurrent fast path: only a turn of >=2 calls that
// are ALL spawn_subagent qualifies.
func TestAllSpawnSubAgent(t *testing.T) {
	spawn := tool.ToolCall{Tool: "spawn_subagent"}
	other := tool.ToolCall{Tool: "read_file"}
	cases := []struct {
		name  string
		calls []tool.ToolCall
		want  bool
	}{
		{"nil", nil, false},
		{"empty", []tool.ToolCall{}, false},
		{"single spawn", []tool.ToolCall{spawn}, false},
		{"two spawns", []tool.ToolCall{spawn, spawn}, true},
		{"three spawns", []tool.ToolCall{spawn, spawn, spawn}, true},
		{"spawn plus other", []tool.ToolCall{spawn, other}, false},
		{"other plus spawn", []tool.ToolCall{other, spawn}, false},
		{"two others", []tool.ToolCall{other, other}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := allSpawnSubAgent(tc.calls); got != tc.want {
				t.Errorf("allSpawnSubAgent(%v) = %v, want %v", tc.calls, got, tc.want)
			}
		})
	}
}

func TestStrictReadOnlyToolsStillQualifyForParallelFastPath(t *testing.T) {
	reg := tool.NewToolRegistry()
	reg.Register(&tool.Tool{
		Name:        "read",
		Description: "Read a file.",
		ReadOnly:    true,
		Strict:      true,
		InputSchema: closedSchema(),
		Execute: func(map[string]interface{}, tool.ToolContext) (interface{}, error) {
			return "", nil
		},
	})
	reg.Register(&tool.Tool{
		Name:        "grep",
		Description: "Search files.",
		ReadOnly:    true,
		Strict:      true,
		InputSchema: closedSchema(),
		Execute: func(map[string]interface{}, tool.ToolContext) (interface{}, error) {
			return "", nil
		},
	})
	reg.Register(&tool.Tool{
		Name:        "spawn_subagent",
		Description: "Delegate work.",
		InputSchema: closedSchema(),
		Execute: func(map[string]interface{}, tool.ToolContext) (interface{}, error) {
			return "", nil
		},
	})

	if !allReadOnly(reg, []tool.ToolCall{{Tool: "read"}, {Tool: "grep"}}) {
		t.Fatal("strict read-only tools should remain eligible for the parallel read-only fast path")
	}
	if allReadOnly(reg, []tool.ToolCall{{Tool: "read"}, {Tool: "spawn_subagent"}}) {
		t.Fatal("non-read-only spawn_subagent should not become eligible just because other tools are strict")
	}
}
