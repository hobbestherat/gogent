package tool

import (
	"strings"
	"testing"
)

// TestEnableDisableAndExecution covers the enable/disable lifecycle and the
// invocation accounting: tools start enabled, disabling hides a tool from
// ListEnabled (but not List) and refuses execution without counting it, and
// re-enabling restores execution and records the invocation.
func TestEnableDisableAndExecution(t *testing.T) {
	reg := NewToolRegistry()
	reg.RegisterCalcTool()

	if !reg.IsEnabled("calc") {
		t.Fatal("tools must be enabled by default")
	}
	if got := len(reg.ListEnabled()); got != 1 {
		t.Fatalf("expected 1 enabled tool, got %d", got)
	}
	if got := len(reg.List()); got != 1 {
		t.Fatalf("List should report all tools, got %d", got)
	}

	// Disabling hides the tool from the model's view but keeps it in List.
	reg.SetEnabled("calc", false)
	if reg.IsEnabled("calc") {
		t.Fatal("calc should be disabled after SetEnabled(false)")
	}
	if got := len(reg.ListEnabled()); got != 0 {
		t.Fatalf("disabled tool should be hidden from ListEnabled, got %d", got)
	}
	if got := len(reg.List()); got != 1 {
		t.Fatalf("List should still include disabled tools, got %d", got)
	}

	// A disabled tool is refused at execution time and does not count as a use.
	resp, err := reg.ExecuteToolCall(&ToolCall{
		Tool: "calc",
		Args: map[string]interface{}{"expression": "1+1"},
	}, ToolContext{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Success {
		t.Fatal("disabled tool must not execute")
	}
	if !strings.Contains(resp.Error, "disabled") {
		t.Fatalf("expected a disabled error, got %q", resp.Error)
	}
	if got := reg.Invocations("calc"); got != 0 {
		t.Fatalf("disabled refusal should not increment invocations, got %d", got)
	}
	if !reg.LastUsed("calc").IsZero() {
		t.Fatal("LastUsed should stay zero when execution was refused")
	}

	// Re-enabling restores execution and records exactly one invocation.
	reg.SetEnabled("calc", true)
	resp, err = reg.ExecuteToolCall(&ToolCall{
		Tool: "calc",
		Args: map[string]interface{}{"expression": "2+2"},
	}, ToolContext{})
	if err != nil || !resp.Success {
		t.Fatalf("enabled tool should execute, err=%v resp=%+v", err, resp)
	}
	if got := reg.Invocations("calc"); got != 1 {
		t.Fatalf("expected 1 invocation, got %d", got)
	}
	if reg.LastUsed("calc").IsZero() {
		t.Fatal("LastUsed should be set after an invocation")
	}

	// A second call accumulates.
	reg.ExecuteToolCall(&ToolCall{
		Tool: "calc",
		Args: map[string]interface{}{"expression": "3+3"},
	}, ToolContext{})
	if got := reg.Invocations("calc"); got != 2 {
		t.Fatalf("expected 2 invocations, got %d", got)
	}
}

// TestInvocationNotCountedOnInvalidArgs ensures a call that fails validation
// (missing required arg) is rejected before any invocation is recorded.
func TestInvocationNotCountedOnInvalidArgs(t *testing.T) {
	reg := NewToolRegistry()
	reg.RegisterCalcTool()
	resp, err := reg.ExecuteToolCall(&ToolCall{
		Tool: "calc",
		Args: map[string]interface{}{}, // missing required "expression"
	}, ToolContext{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Success {
		t.Fatal("expected validation failure")
	}
	if got := reg.Invocations("calc"); got != 0 {
		t.Fatalf("invalid-args call should not count, got %d", got)
	}
}

// TestExecuteUnknownTool asserts unknown tools are reported without a Go error.
func TestExecuteUnknownTool(t *testing.T) {
	reg := NewToolRegistry()
	resp, err := reg.ExecuteToolCall(&ToolCall{Tool: "nope"}, ToolContext{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Success {
		t.Fatal("unknown tool must not succeed")
	}
}

// TestSchemaJSON covers the nil case and that object keys are sorted and
// indented, so the Resources browser shows stable, readable schemas.
func TestSchemaJSON(t *testing.T) {
	t.Run("nil is empty", func(t *testing.T) {
		if got := SchemaJSON(nil); got != "" {
			t.Fatalf("expected empty string, got %q", got)
		}
	})
	t.Run("sorted and indented", func(t *testing.T) {
		schema := map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"b": map[string]interface{}{"type": "string"},
				"a": map[string]interface{}{"type": "number"},
			},
		}
		got := SchemaJSON(schema)
		if !strings.Contains(got, "\"a\"") || !strings.Contains(got, "\"b\"") {
			t.Fatalf("expected both property keys, got %q", got)
		}
		if strings.Index(got, "\"a\"") > strings.Index(got, "\"b\"") {
			t.Fatalf("expected keys sorted alphabetically, got %q", got)
		}
		if !strings.Contains(got, "\n") {
			t.Fatalf("expected indented multiline JSON, got %q", got)
		}
	})
}

// TestListEnabledExcludesDisabled is a focused check that a registry with a mix
// of enabled and disabled tools advertises only the enabled ones.
func TestListEnabledExcludesDisabled(t *testing.T) {
	reg := NewToolRegistry()
	reg.RegisterCalcTool()
	reg.Register(&Tool{Name: "dummy", Description: "d", InputSchema: nil,
		Execute: func(map[string]interface{}, ToolContext) (interface{}, error) { return nil, nil }})
	reg.SetEnabled("dummy", false)

	names := make(map[string]bool, 2)
	for _, tl := range reg.ListEnabled() {
		names[tl.Name] = true
	}
	if !names["calc"] {
		t.Error("calc should be enabled")
	}
	if names["dummy"] {
		t.Error("dummy should be excluded from ListEnabled")
	}
}

// TestExtractJSONObjects exercises the tolerant JSON-object scanner that backs
// the JSON-text tool-call fallback (issue #32): it must find balanced objects
// regardless of surrounding prose, Markdown fences, whitespace, or how many
// objects appear, and must not be fooled by braces inside string literals.
func TestExtractJSONObjects(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want []string
	}{
		{"empty", "", nil},
		{"no json", "just some prose without braces", nil},
		{"bare object", `{"tool":"read"}`, []string{`{"tool":"read"}`}},
		{
			"prose wrapped",
			`Sure, I'll do that: {"tool":"read","args":{"path":"a"}} now.`,
			[]string{`{"tool":"read","args":{"path":"a"}}`},
		},
		{
			"fenced json block",
			"Here you go:\n```json\n{\"tool\":\"read\"}\n```\n",
			[]string{`{"tool":"read"}`},
		},
		{
			"pretty printed",
			"{\n  \"tool\": \"read\",\n  \"args\": {\n    \"path\": \"a\"\n  }\n}",
			[]string{"{\n  \"tool\": \"read\",\n  \"args\": {\n    \"path\": \"a\"\n  }\n}"},
		},
		{
			"braces inside string value",
			`{"tool":"write","args":{"content":"a } b { c"}}`,
			[]string{`{"tool":"write","args":{"content":"a } b { c"}}`},
		},
		{
			"escaped quote inside string",
			`{"tool":"write","args":{"content":"say \"hi\" }"}}`,
			[]string{`{"tool":"write","args":{"content":"say \"hi\" }"}}`},
		},
		{
			"multiple objects",
			`{"tool":"a"} and then {"tool":"b"}`,
			[]string{`{"tool":"a"}`, `{"tool":"b"}`},
		},
		{"unbalanced is skipped", `{"tool":"a"`, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := ExtractJSONObjects(tc.in)
			if len(got) != len(tc.want) {
				t.Fatalf("got %d objects %q, want %d %q", len(got), got, len(tc.want), tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("object %d = %q, want %q", i, got[i], tc.want[i])
				}
			}
		})
	}
}

// TestParseToolCalls verifies the fallback decodes embedded JSON into tool calls
// across the formatting variations small/local models produce, including the
// pretty-printed, space-before-colon, key-reordered, and fenced shapes that the
// old substring matcher silently dropped, plus multiple calls in one reply.
func TestParseToolCalls(t *testing.T) {
	tests := []struct {
		name      string
		in        string
		wantTools []string
	}{
		{"none", "I'm thinking but not acting.", nil},
		{"pretty printed", "{\n \"tool\": \"read\",\n \"args\": {\"path\":\"a\"}\n}", []string{"read"}},
		{"space before colon", `{"tool" : "read", "args" : {"path":"a"}}`, []string{"read"}},
		{"reordered keys", `{"args":{"path":"a"},"tool":"read"}`, []string{"read"}},
		{"fenced", "```json\n{\"tool\":\"grep\",\"args\":{\"pattern\":\"x\"}}\n```", []string{"grep"}},
		{"multiple calls", `{"tool":"read","args":{}} {"tool":"write","args":{}}`, []string{"read", "write"}},
		{"non-tool object ignored", `{"foo":"bar"} {"tool":"read"}`, []string{"read"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			calls := ParseToolCalls(tc.in)
			if len(calls) != len(tc.wantTools) {
				t.Fatalf("got %d calls, want %d: %+v", len(calls), len(tc.wantTools), calls)
			}
			for i, want := range tc.wantTools {
				if calls[i].Tool != want {
					t.Errorf("call %d tool = %q, want %q", i, calls[i].Tool, want)
				}
			}
		})
	}
}

// TestParseToolCallReturnsFirst confirms the single-call wrapper surfaces the
// first call and errors only when nothing parseable is present.
func TestParseToolCallReturnsFirst(t *testing.T) {
	reg := NewToolRegistry()
	tc, err := reg.ParseToolCall(`prefix {"tool":"read","args":{"path":"a"}} suffix`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if tc.Tool != "read" {
		t.Errorf("tool = %q, want read", tc.Tool)
	}
	if _, err := reg.ParseToolCall("no json here"); err == nil {
		t.Error("expected error for response without a tool call")
	}
}
