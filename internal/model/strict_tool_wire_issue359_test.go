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
	conn := NewModelConnectionFromConfig(&config.ModelConfig{Model: "gpt-4o"})
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

func TestIssue359StrictToolWireFormatAnthropicDropsUnsupportedStrictField(t *testing.T) {
	conn := NewModelConnectionFromConfig(&config.ModelConfig{APIType: "anthropic", Model: "claude-x"})
	req := conn.buildRequest([]Message{{Role: RoleUser, Content: "hi"}}, false, []ToolDef{issue359StrictToolDef()}, nil)
	raw, err := buildBodyBytes(anthropicAdapter{}, req)
	if err != nil {
		t.Fatalf("buildBody: %v", err)
	}
	if strings.Contains(string(raw), `"strict"`) {
		t.Fatalf("Anthropic wire body leaked unsupported strict field: %s", raw)
	}

	var got struct {
		Tools []struct {
			Name        string                 `json:"name"`
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
	if got := got.Tools[0].InputSchema["additionalProperties"]; got != false {
		t.Fatalf("Anthropic input_schema.additionalProperties = %v, want false", got)
	}
}

func TestIssue359StrictToolWireFormatGeminiDropsUnsupportedStrictField(t *testing.T) {
	conn := NewModelConnectionFromConfig(&config.ModelConfig{APIType: "gemini", Model: "gemini-2.5-flash"})
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
	if got := params["additionalProperties"]; got != false {
		t.Fatalf("Gemini parameters.additionalProperties = %v, want false", got)
	}
	cfg := got["toolConfig"].(map[string]interface{})["functionCallingConfig"].(map[string]interface{})
	if cfg["parallelFunctionCalls"] != false {
		t.Fatalf("Gemini parallelFunctionCalls = %v, want false for strict tool set", cfg["parallelFunctionCalls"])
	}
}
