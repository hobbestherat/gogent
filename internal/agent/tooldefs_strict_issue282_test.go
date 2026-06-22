package agent

import (
	"testing"

	"gogent/internal/tool"
)

// Issue #282, E1: the agent loop advertises every tool as NON-strict. This is
// the invariant the narrowed parallel_tool_calls trigger relies on — if any
// registry tool became strict, a batched-spawn turn would silently be forced
// serial on OpenAI-compatible providers. These tests lock that down.

// closedSchema is the kind of schema a future author might think should be
// advertised "strict" (additionalProperties:false + required). toolDefsFromRegistry
// must still emit it as non-strict.
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

func TestToolDefsFromRegistryNeverStrict(t *testing.T) {
	reg := tool.NewToolRegistry()
	reg.Register(&tool.Tool{
		Name:        "spawn_subagent",
		Description: "Delegate work to sub-agents.",
		InputSchema: closedSchema(),
	})
	reg.Register(&tool.Tool{
		Name:        "read_file",
		Description: "Read a file.",
		ReadOnly:    true,
		InputSchema: map[string]interface{}{"type": "object"},
	})
	reg.Register(&tool.Tool{
		Name:        "structured_output",
		Description: "Closed schema tool.",
		InputSchema: closedSchema(),
	})

	defs := toolDefsFromRegistry(reg)
	if len(defs) == 0 {
		t.Fatal("expected tool defs, got none")
	}
	sawSpawn := false
	for _, d := range defs {
		if d.Function.Strict {
			t.Errorf("tool %q advertised with Strict=true; the agent loop must advertise all tools non-strict", d.Function.Name)
		}
		if d.Type != "function" {
			t.Errorf("tool %q: Type = %q, want function", d.Function.Name, d.Type)
		}
		if d.Function.Name == "spawn_subagent" {
			sawSpawn = true
			if d.Function.Parameters == nil {
				t.Error("spawn_subagent: Parameters not carried through to the def")
			}
		}
	}
	if !sawSpawn {
		t.Error("spawn_subagent missing from tool defs")
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
