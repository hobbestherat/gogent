package config

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestProviderConnectionVertexProjectLocationJSONRoundTrip(t *testing.T) {
	in := ProviderConnection{
		Name:     "vertex",
		APIType:  "vertex",
		Project:  "gogent-prod",
		Location: "global",
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

	var out ProviderConnection
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if out.APIType != "vertex" || out.Project != "gogent-prod" || out.Location != "global" {
		t.Fatalf("round-trip = %+v, want vertex/project/location preserved", out)
	}
}

func TestDefaultConfigIncludesVertexCompatModelWithoutAPIKey(t *testing.T) {
	cfg := GetDefaultConfig()
	m := cfg.GetModelConfig("vertex-gemini")
	if m == nil {
		t.Fatal("default config missing vertex-gemini model")
	}
	conn := cfg.ConnectionForModel(m)
	if conn == nil {
		t.Fatalf("vertex-gemini references unknown connection %q", m.Connection)
	}
	if conn.APIType != "vertex" {
		t.Errorf("connection APIType = %q, want vertex", conn.APIType)
	}
	if conn.APIKey != "" {
		t.Errorf("APIKey = %q, want empty because Vertex uses ADC", conn.APIKey)
	}
	if conn.Project != "" || conn.Location != "" {
		t.Errorf("Project/Location = %q/%q, want empty placeholders for user configuration", conn.Project, conn.Location)
	}
	if m.Model == "" {
		t.Error("Model is empty, want a Vertex OpenAI-compatible model id")
	}
	if m.Caps.ContextWindow <= 0 || m.MaxTokens <= 0 {
		t.Errorf("ContextWindow/MaxTokens = %d/%d, want positive defaults", m.Caps.ContextWindow, m.MaxTokens)
	}
}
