package model

import (
	"encoding/json"
	"strings"
	"testing"

	"gogent/internal/config"
)

func issue359StrictToolDef() ToolDef {
	return ToolDef{
		Type: "function",
		Function: FunctionDef{
			Name:        "read",
			Description: "Read a file.",
			Parameters: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"path": map[string]interface{}{"type": "string"},
				},
				"required":             []string{"path"},
				"additionalProperties": false,
			},
			Strict: true,
		},
	}
}

func TestIssue359StrictToolWireFormatOpenAI(t *testing.T) {
	conn := NewModelConnection(
		&config.ProviderConnection{},
		&config.ModelConfig{Model: "gpt-4o"},
	)
	req := conn.buildRequest([]Message{{Role: RoleUser, Content: "hi"}}, false, []ToolDef{issue359StrictToolDef()}, nil)
	raw, err := buildBodyBytes(openAIAdapter{}, req)
	if err != nil {
		t.Fatalf("buildBody: %v", err)
	}

	var got struct {
		ParallelToolCalls *bool `json:"parallel_tool_calls"`
		Tools             []struct {
			Function struct {
				Name       string                 `json:"name"`
				Strict     bool                   `json:"strict"`
				Parameters map[string]interface{} `json:"parameters"`
			} `json:"function"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, raw)
	}
	if got.ParallelToolCalls == nil || *got.ParallelToolCalls {
		t.Fatalf("parallel_tool_calls = %v, want false for strict OpenAI tool", got.ParallelToolCalls)
	}
	if len(got.Tools) != 1 {
		t.Fatalf("tools len = %d, want 1", len(got.Tools))
	}
	fn := got.Tools[0].Function
	if fn.Name != "read" {
		t.Errorf("tool name = %q, want read", fn.Name)
	}
	if !fn.Strict {
		t.Fatal("OpenAI function.strict = false, want true")
	}
	if got := fn.Parameters["additionalProperties"]; got != false {
		t.Fatalf("OpenAI parameters.additionalProperties = %v, want false", got)
	}
}

func TestIssue359StrictToolWireFormatAnthropicEmitsStrictField(t *testing.T) {
	// Anthropic's Messages API supports strict tool use via a top-level
	// "strict": true property on the tool definition, alongside name/description/
	// input_schema (https://docs.anthropic.com/en/docs/agents-and-tools/tool-use/
	// strict-tool-use). So a strict tool MUST carry strict:true on the Anthropic
	// wire — it is not dropped (unlike Gemini, which has no per-tool strict field).
	conn := NewModelConnection(
		&config.ProviderConnection{APIType: "anthropic"},
		&config.ModelConfig{Model: "claude-x"},
	)
	req := conn.buildRequest([]Message{{Role: RoleUser, Content: "hi"}}, false, []ToolDef{issue359StrictToolDef()}, nil)
	raw, err := buildBodyBytes(anthropicAdapter{}, req)
	if err != nil {
		t.Fatalf("buildBody: %v", err)
	}

	var got struct {
		Tools []struct {
			Name        string                 `json:"name"`
			Strict      bool                   `json:"strict"`
			InputSchema map[string]interface{} `json:"input_schema"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, raw)
	}
	if len(got.Tools) != 1 {
		t.Fatalf("tools len = %d, want 1", len(got.Tools))
	}
	if got.Tools[0].Name != "read" {
		t.Errorf("tool name = %q, want read", got.Tools[0].Name)
	}
	if !got.Tools[0].Strict {
		t.Fatalf("Anthropic tool.strict = false, want true (strict tool use is supported): %s", raw)
	}
	if got := got.Tools[0].InputSchema["additionalProperties"]; got != false {
		t.Fatalf("Anthropic input_schema.additionalProperties = %v, want false", got)
	}
}

func TestIssue359NonStrictToolOmitsStrictFieldAnthropic(t *testing.T) {
	// A non-strict tool (the default, e.g. spawn_subagent) must omit strict on the
	// Anthropic wire so it is not advertised as strict — strict:omitempty.
	def := issue359StrictToolDef()
	def.Function.Strict = false
	conn := NewModelConnection(
		&config.ProviderConnection{APIType: "anthropic"},
		&config.ModelConfig{Model: "claude-x"},
	)
	req := conn.buildRequest([]Message{{Role: RoleUser, Content: "hi"}}, false, []ToolDef{def}, nil)
	raw, err := buildBodyBytes(anthropicAdapter{}, req)
	if err != nil {
		t.Fatalf("buildBody: %v", err)
	}
	if strings.Contains(string(raw), `"strict"`) {
		t.Fatalf("non-strict Anthropic tool should omit strict field: %s", raw)
	}
}

func TestIssue359StrictToolWireFormatGeminiDropsUnsupportedStrictField(t *testing.T) {
	conn := NewModelConnection(
		&config.ProviderConnection{APIType: "gemini"},
		&config.ModelConfig{Model: "gemini-2.5-flash"},
	)
	req := conn.buildRequest([]Message{{Role: RoleUser, Content: "hi"}}, false, []ToolDef{issue359StrictToolDef()}, nil)
	raw, err := buildBodyBytes(geminiAdapter{}, req)
	if err != nil {
		t.Fatalf("buildBody: %v", err)
	}
	if strings.Contains(string(raw), `"strict"`) {
		t.Fatalf("Gemini wire body leaked unsupported strict field: %s", raw)
	}

	var got map[string]interface{}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, raw)
	}
	tools := got["tools"].([]interface{})
	decls := tools[0].(map[string]interface{})["functionDeclarations"].([]interface{})
	params := decls[0].(map[string]interface{})["parameters"].(map[string]interface{})
	// additionalProperties is NOT a field on Gemini's Schema proto — sending it
	// 400s as an unknown name ("Invalid JSON payload received. Unknown name
	// \"additionalProperties\""). The Gemini schema sanitizer strips it, so the
	// wire must NOT carry it (unlike the OpenAI/Anthropic strict paths, which
	// require it). This corrects the prior assertion, which leaked the field.
	if _, ok := params["additionalProperties"]; ok {
		t.Fatalf("Gemini parameters leaked additionalProperties (Vertex 400s on it): %v", params["additionalProperties"])
	}
	// Vertex AI's functionCallingConfig has no parallelFunctionCalls field (it 400s
	// on it), so a strict tool set's parallel-disable override is simply not
	// surfaced here — the field must be absent regardless.
	cfg := got["toolConfig"].(map[string]interface{})["functionCallingConfig"].(map[string]interface{})
	if _, ok := cfg["parallelFunctionCalls"]; ok {
		t.Fatalf("Gemini parallelFunctionCalls present (%v); Vertex rejects this field", cfg["parallelFunctionCalls"])
	}
}
