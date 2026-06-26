package ui

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"gogent/internal/config"
)

// Issue #486: APIClient.AddModel POSTs a flat config.ModelConfig to /api/models
// to create a new entry on the daemon. These assert the wire shape (method,
// path, body, auth) and that a 409 conflict surfaces as an error (c.do fails on
// any non-2xx).

func TestAPIClientAddModelRequest(t *testing.T) {
	var gotMethod, gotPath, gotAuth string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath, gotAuth = r.Method, r.URL.Path, r.Header.Get("Authorization")
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"name": "or-claude"})
	}))
	defer srv.Close()

	client, err := NewAPIClient(srv.URL, "tok")
	if err != nil {
		t.Fatalf("NewAPIClient: %v", err)
	}
	cfg := config.ModelConfig{
		Name:          "openrouter-claude-opus-4-6",
		DisplayName:   "Claude Opus 4.6",
		APIType:       "openrouter",
		Model:         "anthropic/claude-opus-4-6",
		Endpoint:      "", // openrouter derives its base
		APIKey:        "or-key",
		Temperature:   0.7,
		MaxTokens:     128000,
		ContextWindow: 1000000,
	}
	if err := client.AddModel(cfg); err != nil {
		t.Fatalf("AddModel: %v", err)
	}

	if gotMethod != http.MethodPost {
		t.Errorf("method = %q, want POST", gotMethod)
	}
	if gotPath != "/api/models" {
		t.Errorf("path = %q, want /api/models", gotPath)
	}
	if gotAuth != "Bearer tok" {
		t.Errorf("auth = %q, want Bearer tok", gotAuth)
	}
	// The body is the flat ModelConfig (updateModelRequest embeds it anonymously).
	if gotBody["name"] != "openrouter-claude-opus-4-6" {
		t.Errorf("body name = %v, want openrouter-claude-opus-4-6", gotBody["name"])
	}
	if gotBody["model"] != "anthropic/claude-opus-4-6" {
		t.Errorf("body model = %v, want anthropic/claude-opus-4-6", gotBody["model"])
	}
	if gotBody["api_key"] != "or-key" {
		t.Errorf("body api_key = %v, want or-key (the credential travels with the create)", gotBody["api_key"])
	}
	if gotBody["api_type"] != "openrouter" {
		t.Errorf("body api_type = %v, want openrouter", gotBody["api_type"])
	}
}

func TestAPIClientAddModelConflictSurfacesError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "model already exists", http.StatusConflict)
	}))
	defer srv.Close()

	client, err := NewAPIClient(srv.URL, "tok")
	if err != nil {
		t.Fatalf("NewAPIClient: %v", err)
	}
	if err := client.AddModel(config.ModelConfig{Name: "dup"}); err == nil {
		t.Fatal("AddModel on 409 = nil, want error (c.do must surface non-2xx)")
	}
}

func TestAPIClientAddModelServerErrorSurfacesError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	client, err := NewAPIClient(srv.URL, "tok")
	if err != nil {
		t.Fatalf("NewAPIClient: %v", err)
	}
	if err := client.AddModel(config.ModelConfig{Name: "x"}); err == nil {
		t.Fatal("AddModel on 500 = nil, want error")
	}
}
