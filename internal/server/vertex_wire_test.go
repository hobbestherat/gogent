package server

import (
	"encoding/json"
	"strings"
	"testing"

	"gogent/internal/config"
)

func TestModelToViewIncludesVertexProjectLocationAndRedactsAPIKey(t *testing.T) {
	view := modelToView(&config.ModelConfig{
		Name:        "vertex-gemini",
		DisplayName: "Vertex Gemini",
		APIType:     "vertex",
		Endpoint:    "",
		Project:     "gogent-prod",
		Location:    "us-central1",
		Model:       "google/gemini-2.5-flash",
		APIKey:      "secret-that-must-not-leak",
		MaxTokens:   4096,
	})

	if view.APIType != "vertex" {
		t.Errorf("APIType = %q, want vertex", view.APIType)
	}
	if view.Project != "gogent-prod" {
		t.Errorf("Project = %q, want gogent-prod", view.Project)
	}
	if view.Location != "us-central1" {
		t.Errorf("Location = %q, want us-central1", view.Location)
	}
	if !view.HasAPIKey {
		t.Error("HasAPIKey = false, want true when source config has an API key")
	}

	data, err := json.Marshal(view)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if strings.Contains(string(data), "secret-that-must-not-leak") || strings.Contains(string(data), `"api_key"`) {
		t.Fatalf("model view leaked API key material: %s", data)
	}
	if !strings.Contains(string(data), `"project":"gogent-prod"`) {
		t.Fatalf("model view JSON missing project: %s", data)
	}
	if !strings.Contains(string(data), `"location":"us-central1"`) {
		t.Fatalf("model view JSON missing location: %s", data)
	}
}
