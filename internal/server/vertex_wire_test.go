package server

import (
	"encoding/json"
	"strings"
	"testing"

	"gogent/internal/config"
)

// Credentials and the Vertex project/location now live on the provider connection,
// not the model, so this exercises connectionToView: it surfaces api_type, project
// and location, reports HasAPIKey, and never echoes the api_key itself.
func TestConnectionToViewIncludesVertexProjectLocationAndRedactsAPIKey(t *testing.T) {
	view := connectionToView(&config.ProviderConnection{
		Name:     "vertex-gemini",
		APIType:  "vertex",
		Endpoint: "",
		Project:  "gogent-prod",
		Location: "us-central1",
		APIKey:   "secret-that-must-not-leak",
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
		t.Fatalf("connection view leaked API key material: %s", data)
	}
	if !strings.Contains(string(data), `"project":"gogent-prod"`) {
		t.Fatalf("connection view JSON missing project: %s", data)
	}
	if !strings.Contains(string(data), `"location":"us-central1"`) {
		t.Fatalf("connection view JSON missing location: %s", data)
	}
}
