package model

import (
	"encoding/json"
	"testing"

	"gogent/internal/config"
)

// exampleSchema is a small closed JSON Schema reused across the structured-output
// tests.
func exampleSchema() map[string]interface{} {
	return map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"answer": map[string]interface{}{"type": "string"},
		},
		"required":             []string{"answer"},
		"additionalProperties": false,
	}
}

// JSONSchemaResponseFormat builds a strict json_schema response format.
func TestJSONSchemaResponseFormat(t *testing.T) {
	rf := JSONSchemaResponseFormat("reply", exampleSchema())
	if rf.Type != "json_schema" {
		t.Fatalf("Type = %q, want json_schema", rf.Type)
	}
	if rf.JSONSchema == nil {
		t.Fatal("JSONSchema is nil")
	}
	if rf.JSONSchema.Name != "reply" {
		t.Errorf("Name = %q, want reply", rf.JSONSchema.Name)
	}
	if !rf.JSONSchema.Strict {
		t.Error("Strict = false, want true (json_schema must be strict)")
	}
}

// A nil ResponseFormat must be omitted from the wire body entirely.
func TestCompletionRequestOmitsNilResponseFormat(t *testing.T) {
	b, err := json.Marshal(CompletionRequest{Model: "x"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var m map[string]interface{}
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := m["response_format"]; ok {
		t.Errorf("response_format should be omitted when unset, got %s", b)
	}
	if _, ok := m["parallel_tool_calls"]; ok {
		t.Errorf("parallel_tool_calls should be omitted when unset, got %s", b)
	}
}

// On an OpenAI-compatible provider, a response format is emitted on the wire in
// the OpenAI json_schema shape (with strict:true).
func TestBuildRequestResponseFormatOnWireOpenAI(t *testing.T) {
	conn := NewModelConnectionFromConfig(&config.ModelConfig{Model: "gpt-4o"})
	format := JSONSchemaResponseFormat("reply", exampleSchema())
	req := conn.buildRequest([]Message{{Role: RoleUser, Content: "hi"}}, false, nil, format)
	if req.ResponseFormat == nil {
		t.Fatal("ResponseFormat not set on request")
	}

	raw, err := buildBodyBytes(openAIAdapter{}, req)
	if err != nil {
		t.Fatalf("buildBody: %v", err)
	}
	var got struct {
		ResponseFormat struct {
			Type       string `json:"type"`
			JSONSchema struct {
				Name   string `json:"name"`
				Strict bool   `json:"strict"`
			} `json:"json_schema"`
		} `json:"response_format"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.ResponseFormat.Type != "json_schema" {
		t.Errorf("response_format.type = %q, want json_schema", got.ResponseFormat.Type)
	}
	if got.ResponseFormat.JSONSchema.Name != "reply" {
		t.Errorf("response_format.json_schema.name = %q, want reply", got.ResponseFormat.JSONSchema.Name)
	}
	if !got.ResponseFormat.JSONSchema.Strict {
		t.Error("response_format.json_schema.strict = false, want true")
	}
}

// Anthropic has no response_format field, so the format is dropped (not sent and
// rejected) — its provider spec leaves supportsResponseFormat unset.
func TestBuildRequestResponseFormatDroppedForAnthropic(t *testing.T) {
	conn := NewModelConnectionFromConfig(&config.ModelConfig{APIType: "anthropic", Model: "claude-x"})
	format := JSONSchemaResponseFormat("reply", exampleSchema())
	req := conn.buildRequest([]Message{{Role: RoleUser, Content: "hi"}}, false, nil, format)
	if req.ResponseFormat != nil {
		t.Errorf("ResponseFormat = %+v, want nil for a provider without support", req.ResponseFormat)
	}

	// And even if a ResponseFormat somehow reached the Anthropic adapter, its
	// request shape has no such field, so it never appears on the wire.
	raw, err := buildBodyBytes(anthropicAdapter{}, CompletionRequest{
		Model:          "claude-x",
		Messages:       []Message{{Role: RoleUser, Content: "hi"}},
		ResponseFormat: format,
	})
	if err != nil {
		t.Fatalf("buildBody: %v", err)
	}
	var m map[string]interface{}
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := m["response_format"]; ok {
		t.Errorf("Anthropic body must not carry response_format, got %s", raw)
	}
}

// A strict tool forces parallel_tool_calls:false on an OpenAI-compatible
// provider (OpenAI rejects parallel tool calls alongside strict tool schemas).
func TestBuildRequestStrictToolDisablesParallelToolCalls(t *testing.T) {
	conn := NewModelConnectionFromConfig(&config.ModelConfig{Model: "gpt-4o"})
	tools := []ToolDef{{
		Type: "function",
		Function: FunctionDef{
			Name:       "structured_output",
			Parameters: exampleSchema(),
			Strict:     true,
		},
	}}
	req := conn.buildRequest([]Message{{Role: RoleUser, Content: "hi"}}, false, tools, nil)
	if req.ParallelToolCalls == nil {
		t.Fatal("ParallelToolCalls not set for a strict tool set")
	}
	if *req.ParallelToolCalls {
		t.Error("ParallelToolCalls = true, want false for a strict tool set")
	}

	raw, err := buildBodyBytes(openAIAdapter{}, req)
	if err != nil {
		t.Fatalf("buildBody: %v", err)
	}
	var got struct {
		ParallelToolCalls *bool `json:"parallel_tool_calls"`
		Tools             []struct {
			Function struct {
				Strict bool `json:"strict"`
			} `json:"function"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.ParallelToolCalls == nil || *got.ParallelToolCalls {
		t.Errorf("parallel_tool_calls = %v, want false on the wire", got.ParallelToolCalls)
	}
	if len(got.Tools) != 1 || !got.Tools[0].Function.Strict {
		t.Errorf("tool strict flag = %+v, want strict:true on the wire", got.Tools)
	}
}

// A non-strict tool set leaves parallel_tool_calls unset (provider default).
func TestBuildRequestNonStrictToolKeepsParallelToolCallsUnset(t *testing.T) {
	conn := NewModelConnectionFromConfig(&config.ModelConfig{Model: "gpt-4o"})
	tools := []ToolDef{{
		Type:     "function",
		Function: FunctionDef{Name: "read", Parameters: map[string]interface{}{"type": "object"}},
	}}
	req := conn.buildRequest([]Message{{Role: RoleUser, Content: "hi"}}, false, tools, nil)
	if req.ParallelToolCalls != nil {
		t.Errorf("ParallelToolCalls = %v, want nil for a non-strict tool set", *req.ParallelToolCalls)
	}
}

// On Anthropic a strict tool does not produce a parallel_tool_calls field — that
// invariant is OpenAI-specific and the Anthropic request shape has no such field.
func TestBuildRequestStrictToolNoParallelFieldForAnthropic(t *testing.T) {
	conn := NewModelConnectionFromConfig(&config.ModelConfig{APIType: "anthropic", Model: "claude-x"})
	tools := []ToolDef{{
		Type:     "function",
		Function: FunctionDef{Name: "structured_output", Parameters: exampleSchema(), Strict: true},
	}}
	req := conn.buildRequest([]Message{{Role: RoleUser, Content: "hi"}}, false, tools, nil)
	if req.ParallelToolCalls != nil {
		t.Errorf("ParallelToolCalls = %v, want nil on Anthropic", *req.ParallelToolCalls)
	}
}

// hasStrictTool reports strictness across a tool set.
func TestHasStrictTool(t *testing.T) {
	tests := []struct {
		name  string
		tools []ToolDef
		want  bool
	}{
		{"nil", nil, false},
		{"none strict", []ToolDef{{Function: FunctionDef{Name: "a"}}}, false},
		{"one strict", []ToolDef{
			{Function: FunctionDef{Name: "a"}},
			{Function: FunctionDef{Name: "b", Strict: true}},
		}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := hasStrictTool(tt.tools); got != tt.want {
				t.Errorf("hasStrictTool = %v, want %v", got, tt.want)
			}
		})
	}
}
