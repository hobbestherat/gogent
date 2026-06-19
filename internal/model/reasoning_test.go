package model

import (
	"encoding/json"
	"testing"

	"gogent/internal/config"
)

func boolPtr(b bool) *bool { return &b }

// buildRequestFor constructs a connection from cfg (endpoint left empty so the
// provider spec/capabilities are derived from api_type) and returns the request
// buildRequest would send.
func buildRequestFor(cfg *config.ModelConfig) CompletionRequest {
	conn := NewModelConnectionFromConfig(cfg)
	return conn.buildRequest([]Message{{Role: RoleUser, Content: "hi"}}, false, nil)
}

func TestBuildRequestReasoningParams(t *testing.T) {
	tests := []struct {
		name string
		cfg  *config.ModelConfig
		// expectations
		wantMaxTokens       *int
		wantMaxCompletion   *int
		wantTempSet         bool
		wantTemp            float32
		wantTopPSet         bool
		wantTopP            float32
		wantReasoningEffort string
		wantThinking        string // "" => omitted
	}{
		{
			name:          "openai non-reasoning sends max_tokens and temperature",
			cfg:           &config.ModelConfig{Model: "gpt-4o", MaxTokens: 4096, Temperature: 0.7},
			wantMaxTokens: intPtr(4096),
			wantTempSet:   true,
			wantTemp:      0.7,
		},
		{
			name:          "temperature zero is expressible (pointer, not dropped)",
			cfg:           &config.ModelConfig{Model: "gpt-4o", MaxTokens: 4096, Temperature: 0},
			wantMaxTokens: intPtr(4096),
			wantTempSet:   true,
			wantTemp:      0,
		},
		{
			name:          "top_p emitted when configured",
			cfg:           &config.ModelConfig{Model: "gpt-4o", MaxTokens: 4096, Temperature: 0.7, TopP: 0.9},
			wantMaxTokens: intPtr(4096),
			wantTempSet:   true,
			wantTemp:    0.7,
			wantTopPSet: true,
			wantTopP:    0.9,
		},
		{
			name:                "openai reasoning effort switches to max_completion_tokens, drops temperature",
			cfg:                 &config.ModelConfig{Model: "o3", MaxTokens: 8000, Temperature: 0.7, ReasoningEffort: "high"},
			wantMaxCompletion:   intPtr(8000),
			wantReasoningEffort: "high",
		},
		{
			name:              "openai thinking toggle marks reasoning but thinking param is unsupported",
			cfg:               &config.ModelConfig{Model: "gpt-5", MaxTokens: 8000, Temperature: 0.7, Thinking: boolPtr(true)},
			wantMaxCompletion: intPtr(8000),
			// OpenAI has no thinking param; it must be omitted, and temperature dropped.
			wantThinking: "",
		},
		{
			name:                "zai reasoning keeps max_tokens and temperature, emits thinking + effort",
			cfg:                 &config.ModelConfig{APIType: "zai", Model: "glm-5.2", MaxTokens: 4096, Temperature: 0.6, ReasoningEffort: "max", Thinking: boolPtr(true)},
			wantMaxTokens:       intPtr(4096),
			wantTempSet:         true,
			wantTemp:            0.6,
			wantReasoningEffort: "max",
			wantThinking:        "enabled",
		},
		{
			name:          "zai thinking disabled emits disabled toggle",
			cfg:           &config.ModelConfig{APIType: "zai", Model: "glm-4.6", MaxTokens: 4096, Temperature: 0.6, Thinking: boolPtr(false)},
			wantMaxTokens: intPtr(4096),
			wantTempSet:   true,
			wantTemp:      0.6,
			wantThinking:  "disabled",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := buildRequestFor(tt.cfg)

			if !eqIntPtr(req.MaxTokens, tt.wantMaxTokens) {
				t.Errorf("MaxTokens = %v, want %v", derefInt(req.MaxTokens), derefInt(tt.wantMaxTokens))
			}
			if !eqIntPtr(req.MaxCompletionTokens, tt.wantMaxCompletion) {
				t.Errorf("MaxCompletionTokens = %v, want %v", derefInt(req.MaxCompletionTokens), derefInt(tt.wantMaxCompletion))
			}
			if tt.wantTempSet {
				if req.Temperature == nil {
					t.Errorf("Temperature = nil, want %v", tt.wantTemp)
				} else if *req.Temperature != tt.wantTemp {
					t.Errorf("Temperature = %v, want %v", *req.Temperature, tt.wantTemp)
				}
			} else if req.Temperature != nil {
				t.Errorf("Temperature = %v, want omitted", *req.Temperature)
			}
			if tt.wantTopPSet {
				if req.TopP == nil || *req.TopP != tt.wantTopP {
					t.Errorf("TopP = %v, want %v", req.TopP, tt.wantTopP)
				}
			} else if req.TopP != nil {
				t.Errorf("TopP = %v, want omitted", *req.TopP)
			}
			if req.ReasoningEffort != tt.wantReasoningEffort {
				t.Errorf("ReasoningEffort = %q, want %q", req.ReasoningEffort, tt.wantReasoningEffort)
			}
			gotThinking := ""
			if req.Thinking != nil {
				gotThinking = req.Thinking.Type
			}
			if gotThinking != tt.wantThinking {
				t.Errorf("Thinking = %q, want %q", gotThinking, tt.wantThinking)
			}
		})
	}
}

// TestBuildRequestReasoningJSONShape verifies the on-the-wire JSON omits the
// fields the issue calls out (no max_tokens/temperature for OpenAI reasoning)
// and includes the reasoning controls.
func TestBuildRequestReasoningJSONShape(t *testing.T) {
	req := buildRequestFor(&config.ModelConfig{
		Model: "o3", MaxTokens: 8000, Temperature: 0.7, ReasoningEffort: "medium",
	})
	data, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := m["max_tokens"]; ok {
		t.Errorf("max_tokens present for OpenAI reasoning model: %s", data)
	}
	if _, ok := m["temperature"]; ok {
		t.Errorf("temperature present for OpenAI reasoning model: %s", data)
	}
	if _, ok := m["max_completion_tokens"]; !ok {
		t.Errorf("max_completion_tokens missing: %s", data)
	}
	if _, ok := m["reasoning_effort"]; !ok {
		t.Errorf("reasoning_effort missing: %s", data)
	}
}

func TestTokenUsageReasoningTokens(t *testing.T) {
	const body = `{
		"prompt_tokens": 100,
		"completion_tokens": 80,
		"total_tokens": 180,
		"completion_tokens_details": {"reasoning_tokens": 50}
	}`
	var u TokenUsage
	if err := json.Unmarshal([]byte(body), &u); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if u.ReasoningTokens != 50 {
		t.Errorf("ReasoningTokens = %d, want 50", u.ReasoningTokens)
	}
	if u.CompletionTokens != 80 {
		t.Errorf("CompletionTokens = %d, want 80", u.CompletionTokens)
	}
}

func intPtr(i int) *int { return &i }

func eqIntPtr(a, b *int) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

func derefInt(p *int) interface{} {
	if p == nil {
		return nil
	}
	return *p
}
