package config

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestModelConfigVertexProjectLocationJSONRoundTrip(t *testing.T) {
	in := ModelConfig{
		Name:        "vertex-gemini",
		DisplayName: "Vertex Gemini",
		APIType:     "vertex",
		Project:     "gogent-prod",
		Location:    "global",
		Model:       "google/gemini-2.5-flash",
		Temperature: 0.7,
		MaxTokens:   65536,
	}

	data, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if !strings.Contains(string(data), `"api_type":"vertex"`) {
		t.Fatalf("JSON %s missing api_type vertex", data)
	}
	if !strings.Contains(string(data), `"project":"gogent-prod"`) {
		t.Fatalf("JSON %s missing project", data)
	}
	if !strings.Contains(string(data), `"location":"global"`) {
		t.Fatalf("JSON %s missing location", data)
	}
	if strings.Contains(string(data), `"api_key"`) {
		t.Fatalf("JSON %s unexpectedly includes empty api_key", data)
	}

	var out ModelConfig
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if out.APIType != "vertex" || out.Project != "gogent-prod" || out.Location != "global" || out.Model != "google/gemini-2.5-flash" {
		t.Fatalf("round-trip = %+v, want vertex/project/location/model preserved", out)
	}
}

func TestDefaultConfigIncludesVertexCompatModelWithoutAPIKey(t *testing.T) {
	cfg := GetDefaultConfig()
	m := cfg.GetModelConfig("vertex-gemini")
	if m == nil {
		t.Fatal("default config missing vertex-gemini model")
	}
	if m.APIType != "vertex" {
		t.Errorf("APIType = %q, want vertex", m.APIType)
	}
	if m.APIKey != "" {
		t.Errorf("APIKey = %q, want empty because Vertex uses ADC", m.APIKey)
	}
	if m.Project != "" || m.Location != "" {
		t.Errorf("Project/Location = %q/%q, want empty placeholders for user configuration", m.Project, m.Location)
	}
	if m.Model == "" {
		t.Error("Model is empty, want a Vertex OpenAI-compatible model id")
	}
	if m.ContextWindow <= 0 || m.MaxTokens <= 0 {
		t.Errorf("ContextWindow/MaxTokens = %d/%d, want positive defaults", m.ContextWindow, m.MaxTokens)
	}
}
