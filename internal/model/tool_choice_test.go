package model

import (
	"encoding/json"
	"testing"

	"gogent/internal/config"
)

// The OpenAI wire form of tool_choice comes from ToolChoice.MarshalJSON, since
// the OpenAI-compatible adapter marshals the request struct directly.
func TestToolChoiceMarshalOpenAI(t *testing.T) {
	tests := []struct {
		name   string
		choice ToolChoice
		want   string
	}{
		{"auto", ToolChoice{Mode: ToolChoiceAuto}, `"auto"`},
		{"none", ToolChoice{Mode: ToolChoiceNone}, `"none"`},
		{"required", ToolChoice{Mode: ToolChoiceRequired}, `"required"`},
		{"tool", ToolChoice{Mode: ToolChoiceTool, Name: "structured_output"},
			`{"function":{"name":"structured_output"},"type":"function"}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b, err := json.Marshal(tt.choice)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if string(b) != tt.want {
				t.Errorf("Marshal(%+v) = %s, want %s", tt.choice, b, tt.want)
			}
		})
	}
}

// A nil ToolChoice on the request must be omitted entirely (no tools => no
// tool_choice key).
func TestCompletionRequestOmitsNilToolChoice(t *testing.T) {
	b, err := json.Marshal(CompletionRequest{Model: "x"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var m map[string]interface{}
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := m["tool_choice"]; ok {
		t.Errorf("tool_choice should be omitted when unset, got %s", b)
	}
}

func TestForceTool(t *testing.T) {
	tc := ForceTool("structured_output")
	if tc.Mode != ToolChoiceTool || tc.Name != "structured_output" {
		t.Errorf("ForceTool = %+v", tc)
	}
}

// End to end: when tools are offered, buildRequest sets ToolChoice to auto and
// the OpenAI adapter serializes it as the bare string "auto".
func TestBuildRequestToolChoiceAutoOnWire(t *testing.T) {
	conn := NewModelConnectionFromConfig(&config.ModelConfig{Model: "gpt-4o"})
	tools := []ToolDef{{
		Type:     "function",
		Function: FunctionDef{Name: "read", Parameters: map[string]interface{}{"type": "object"}},
	}}
	req := conn.buildRequest([]Message{{Role: RoleUser, Content: "hi"}}, false, tools)
	if req.ToolChoice == nil || req.ToolChoice.Mode != ToolChoiceAuto {
		t.Fatalf("ToolChoice = %+v, want auto", req.ToolChoice)
	}
	raw, err := buildBodyBytes(openAIAdapter{}, req)
	if err != nil {
		t.Fatalf("buildBody: %v", err)
	}
	var m map[string]interface{}
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if m["tool_choice"] != "auto" {
		t.Errorf("tool_choice = %v, want \"auto\"", m["tool_choice"])
	}
}

// No tools => buildRequest leaves ToolChoice unset, and it is omitted on the wire.
func TestBuildRequestNoToolChoiceWithoutTools(t *testing.T) {
	conn := NewModelConnectionFromConfig(&config.ModelConfig{Model: "gpt-4o"})
	req := conn.buildRequest([]Message{{Role: RoleUser, Content: "hi"}}, false, nil)
	if req.ToolChoice != nil {
		t.Errorf("ToolChoice = %+v, want nil without tools", req.ToolChoice)
	}
}

// The Anthropic adapter encodes tool_choice as an object with a "type"
// discriminator, and only when a tool set is present.
func TestAnthropicToolChoiceEncoding(t *testing.T) {
	tools := []ToolDef{{
		Type:     "function",
		Function: FunctionDef{Name: "get_weather", Parameters: map[string]interface{}{"type": "object"}},
	}}

	tests := []struct {
		name   string
		choice *ToolChoice
		want   map[string]interface{}
	}{
		{"auto", &ToolChoice{Mode: ToolChoiceAuto}, map[string]interface{}{"type": "auto"}},
		{"none", &ToolChoice{Mode: ToolChoiceNone}, map[string]interface{}{"type": "none"}},
		{"required maps to any", &ToolChoice{Mode: ToolChoiceRequired}, map[string]interface{}{"type": "any"}},
		{"force tool", ForceTool("get_weather"), map[string]interface{}{"type": "tool", "name": "get_weather"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			raw, err := buildBodyBytes(anthropicAdapter{}, CompletionRequest{
				Model:      "claude-x",
				Messages:   []Message{{Role: RoleUser, Content: "hi"}},
				Tools:      tools,
				ToolChoice: tt.choice,
			})
			if err != nil {
				t.Fatalf("buildBody: %v", err)
			}
			var got struct {
				ToolChoice map[string]interface{} `json:"tool_choice"`
			}
			if err := json.Unmarshal(raw, &got); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if len(got.ToolChoice) != len(tt.want) {
				t.Fatalf("tool_choice = %v, want %v", got.ToolChoice, tt.want)
			}
			for k, v := range tt.want {
				if got.ToolChoice[k] != v {
					t.Errorf("tool_choice[%q] = %v, want %v", k, got.ToolChoice[k], v)
				}
			}
		})
	}
}

// Without tools, no tool_choice is emitted even if one is set (Anthropic rejects
// a tool_choice with no tools).
func TestAnthropicToolChoiceOmittedWithoutTools(t *testing.T) {
	raw, err := buildBodyBytes(anthropicAdapter{}, CompletionRequest{
		Model:      "claude-x",
		Messages:   []Message{{Role: RoleUser, Content: "hi"}},
		ToolChoice: &ToolChoice{Mode: ToolChoiceAuto},
	})
	if err != nil {
		t.Fatalf("buildBody: %v", err)
	}
	var m map[string]interface{}
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := m["tool_choice"]; ok {
		t.Errorf("tool_choice should be omitted without tools, got %s", raw)
	}
}
