package tool

import (
	"sort"
	"testing"
)

// TestCloneForPlanMode verifies the plan-mode registry keeps read-only tools and
// the named extras while stripping side-effecting tools (issue #43), and that it
// shares counters with the source (so plan-mode calls still reach Statistics).
func TestCloneForPlanMode(t *testing.T) {
	tests := []struct {
		name     string
		keep     []string
		ReadOnly map[string]bool // tool name -> ReadOnly flag
		want     []string        // tool names retained by the plan-mode clone (sorted)
	}{
		{
			name: "keeps_readonly_and_extras_strips_side_effects",
			keep: []string{"todo", "structured_output"},
			ReadOnly: map[string]bool{
				"read":              true,
				"grep":              true,
				"calc":              true,
				"write":             false,
				"edit":              false,
				"shell":             false,
				"todo":              false, // kept by name, not by ReadOnly
				"structured_output": false, // kept by name
				"spawn":             false,
			},
			want: []string{"calc", "grep", "read", "structured_output", "todo"},
		},
		{
			name: "no_extras_keeps_only_readonly",
			keep: nil,
			ReadOnly: map[string]bool{
				"read":  true,
				"write": false,
			},
			want: []string{"read"},
		},
		{
			name: "all_readonly_strips_nothing",
			keep: []string{},
			ReadOnly: map[string]bool{
				"read": true,
				"calc": true,
			},
			want: []string{"calc", "read"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			parent := NewToolRegistry()
			for name, ro := range tt.ReadOnly {
				parent.Register(&Tool{
					Name: name, Description: "d", ReadOnly: ro, InputSchema: nil,
					Execute: func(map[string]interface{}, ToolContext) (interface{}, error) { return nil, nil },
				})
			}
			clone := parent.CloneForPlanMode(tt.keep...)
			got := clone.List()
			gotNames := make([]string, 0, len(got))
			for _, g := range got {
				gotNames = append(gotNames, g.Name)
			}
			sort.Strings(gotNames)
			if len(gotNames) != len(tt.want) {
				t.Fatalf("plan clone tools = %v, want %v", gotNames, tt.want)
			}
			for i, w := range tt.want {
				if gotNames[i] != w {
					t.Errorf("plan clone tools = %v, want %v", gotNames, tt.want)
					break
				}
			}
			// Counters stay shared: a call on the clone reaches the parent.
			if readIdx := indexOf(gotNames, "read"); readIdx >= 0 {
				clone.ExecuteToolCall(&ToolCall{Tool: "read", Args: map[string]interface{}{}}, ToolContext{})
				if parent.Invocations("read") != 1 {
					t.Errorf("parent Invocations(read) = %d, want 1 (counters should be shared)", parent.Invocations("read"))
				}
			}
		})
	}
}

func indexOf(s []string, v string) int {
	for i, x := range s {
		if x == v {
			return i
		}
	}
	return -1
}
