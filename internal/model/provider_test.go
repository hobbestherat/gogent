package model

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"gogent/internal/config"
)

func TestStringToAPIType(t *testing.T) {
	cases := map[string]APIType{
		"":       APITypeOpenAI,
		"openai": APITypeOpenAI,
		"OpenAI": APITypeOpenAI,
		"zai":    APITypeZAI,
		"z.ai":   APITypeZAI,
		" ZAI ":  APITypeZAI,
		"bogus":  APITypeOpenAI,
	}
	for in, want := range cases {
		if got := StringToAPIType(in); got != want {
			t.Errorf("StringToAPIType(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestNormalizeBaseURL(t *testing.T) {
	// OpenAI-compatible providers share the "/chat/completions" chat path and
	// differ only in their default base URL.
	const chatPath = "/chat/completions"
	const openaiDefault = "http://localhost:8080/v1"
	const zaiDefault = "https://api.z.ai/api/paas/v4"

	cases := []struct {
		name        string
		endpoint    string
		defaultBase string
		want        string
	}{
		{"openai base url", "https://api.example.com/v1", openaiDefault, "https://api.example.com/v1"},
		{"openai full url", "https://api.example.com/v1/chat/completions", openaiDefault, "https://api.example.com/v1"},
		{"openai trailing slash", "https://api.example.com/v1/", openaiDefault, "https://api.example.com/v1"},
		{"openai empty default", "", openaiDefault, "http://localhost:8080/v1"},
		{"zai empty default", "", zaiDefault, "https://api.z.ai/api/paas/v4"},
		{"zai full url collapses", "https://api.z.ai/api/paas/v4/chat/completions", zaiDefault, "https://api.z.ai/api/paas/v4"},
	}
	for _, tc := range cases {
		if got := normalizeBaseURL(tc.endpoint, tc.defaultBase, chatPath); got != tc.want {
			t.Errorf("%s: normalizeBaseURL(%q) = %q, want %q", tc.name, tc.endpoint, got, tc.want)
		}
	}
}

func TestNormalizeBaseURLPreservesQuery(t *testing.T) {
	// A full chat-completions URL carrying a query string (Azure's
	// ?api-version=) must have only the chat path stripped, with the query kept
	// so it can ride onto the derived chat/models endpoints. The old LastIndex
	// surgery left the path in place because it was not the literal tail.
	const azure = "https://r.openai.azure.com/openai/deployments/dep/chat/completions?api-version=2024-02-01"
	gotBase := normalizeBaseURL(azure, "http://localhost:8080/v1", "/chat/completions")
	wantBase := "https://r.openai.azure.com/openai/deployments/dep?api-version=2024-02-01"
	if gotBase != wantBase {
		t.Fatalf("normalizeBaseURL(azure) = %q, want %q", gotBase, wantBase)
	}
	if got := appendPath(gotBase, "/chat/completions"); got != azure {
		t.Errorf("chatURL round-trip = %q, want %q", got, azure)
	}
	wantModels := "https://r.openai.azure.com/openai/deployments/dep/models?api-version=2024-02-01"
	if got := appendPath(gotBase, "/models"); got != wantModels {
		t.Errorf("modelsURL = %q, want %q", got, wantModels)
	}
}

func TestNewModelConnectionFromConfigBaseURL(t *testing.T) {
	// An OpenAI backend given only a base URL gets /chat/completions appended.
	conn := NewModelConnection(
		&config.ProviderConnection{Endpoint: "https://api.example.com/v1"},
		&config.ModelConfig{Model: "m"},
	)
	if want := "https://api.example.com/v1/chat/completions"; conn.URL != want {
		t.Errorf("openai chat URL = %q, want %q", conn.URL, want)
	}
	if conn.APIType != APITypeOpenAI {
		t.Errorf("APIType = %q, want openai", conn.APIType)
	}

	// A Z.AI backend with no endpoint uses the provider default base URL.
	zconn := NewModelConnection(
		&config.ProviderConnection{APIType: "zai"},
		&config.ModelConfig{Model: "glm-4.6"},
	)
	if want := "https://api.z.ai/api/paas/v4/chat/completions"; zconn.URL != want {
		t.Errorf("zai chat URL = %q, want %q", zconn.URL, want)
	}
	// The derived model-listing URL is exercised end-to-end in TestZAIModelsListing.
}

func TestZAIMaxTokensClamp(t *testing.T) {
	var gotMaxTokens int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req CompletionRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.MaxTokens != nil {
			gotMaxTokens = *req.MaxTokens
		} else {
			gotMaxTokens = 0
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(CompletionResponse{Content: "ok", Role: RoleAssistant, FinishReason: "stop"})
	}))
	defer server.Close()

	// Z.AI rejects max_tokens above 131072, so an over-large config value must
	// be clamped before the request is sent.
	conn := NewModelConnection(
		&config.ProviderConnection{APIType: "zai", Endpoint: server.URL},
		&config.ModelConfig{Model: "glm-4.6", MaxTokens: 262144},
	)
	if _, err := conn.Complete([]Message{{Role: RoleUser, Content: "hi"}}); err != nil {
		t.Fatalf("Complete failed: %v", err)
	}
	if gotMaxTokens != 131072 {
		t.Errorf("zai max_tokens = %d, want clamped to 131072", gotMaxTokens)
	}

	// OpenAI provider has no known ceiling, so the value passes through.
	oconn := NewModelConnection(
		&config.ProviderConnection{Endpoint: server.URL},
		&config.ModelConfig{Model: "m", MaxTokens: 262144},
	)
	if _, err := oconn.Complete([]Message{{Role: RoleUser, Content: "hi"}}); err != nil {
		t.Fatalf("Complete failed: %v", err)
	}
	if gotMaxTokens != 262144 {
		t.Errorf("openai max_tokens = %d, want unchanged 262144", gotMaxTokens)
	}
}

func TestZAIModelsListing(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/models" {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"glm-4.6","owned_by":"z-ai"}]}`))
	}))
	defer server.Close()

	conn := NewModelConnection(
		&config.ProviderConnection{APIType: "zai", Endpoint: server.URL},
		&config.ModelConfig{Model: "glm-4.6"},
	)
	models, err := conn.ListModels()
	if err != nil {
		t.Fatalf("ListModels failed: %v", err)
	}
	if len(models) != 1 || models[0].ID != "glm-4.6" {
		t.Errorf("unexpected models: %+v", models)
	}
}

func TestOpenAIListingFallsBackToOllamaTags(t *testing.T) {
	// A bare Ollama server 404s on /v1/models (it never implements the OpenAI
	// listing route) but advertises its models at /api/tags. The generic OpenAI
	// lister must fall back to /api/tags and surface the model tags as ids.
	var modelsHit, tagsHit bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/models":
			modelsHit = true
			http.Error(w, "404 page not found", http.StatusNotFound)
		case "/api/tags":
			tagsHit = true
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"models":[` +
				`{"name":"llama3:latest","model":"llama3:latest","size":4661224676},` +
				`{"name":"mistral:7b","model":"mistral:7b","size":4109865159}]}`))
		default:
			http.Error(w, "not found", http.StatusNotFound)
		}
	}))
	defer server.Close()

	conn := NewModelConnection(
		&config.ProviderConnection{APIType: "openai", Endpoint: server.URL},
		&config.ModelConfig{Model: "llama3"},
	)
	models, err := conn.ListModels()
	if err != nil {
		t.Fatalf("ListModels failed: %v", err)
	}
	if !modelsHit || !tagsHit {
		t.Fatalf("expected both /models and /api/tags to be probed; modelsHit=%v tagsHit=%v", modelsHit, tagsHit)
	}
	if len(models) != 2 || models[0].ID != "llama3:latest" || models[1].ID != "mistral:7b" {
		t.Errorf("unexpected models from /api/tags fallback: %+v", models)
	}
}

func TestOpenAIListingFallsBackWhenModelsEmpty(t *testing.T) {
	// Some local shims answer /models with HTTP 200 but an empty list rather than a
	// 404. The "OR returns no parseable models" half of the fallback must still kick
	// in and probe /api/tags.
	var modelsHit, tagsHit bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/models":
			modelsHit = true
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"object":"list","data":[]}`))
		case "/api/tags":
			tagsHit = true
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"models":[{"name":"llama3:latest","model":"llama3:latest"}]}`))
		default:
			http.Error(w, "not found", http.StatusNotFound)
		}
	}))
	defer server.Close()

	conn := NewModelConnection(
		&config.ProviderConnection{APIType: "openai", Endpoint: server.URL},
		&config.ModelConfig{Model: "llama3"},
	)
	models, err := conn.ListModels()
	if err != nil {
		t.Fatalf("ListModels failed: %v", err)
	}
	if !modelsHit || !tagsHit {
		t.Fatalf("expected both endpoints probed; modelsHit=%v tagsHit=%v", modelsHit, tagsHit)
	}
	if len(models) != 1 || models[0].ID != "llama3:latest" {
		t.Errorf("unexpected models from empty-200 fallback: %+v", models)
	}
}

func TestOpenAIListingNoTagsFallbackWhenModelsServed(t *testing.T) {
	// When /models answers normally the lister must NOT probe /api/tags — the
	// fallback is strictly a last resort for servers that lack the OpenAI route.
	var tagsHit bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/models":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"gpt-4o","owned_by":"openai"}]}`))
		case "/api/tags":
			tagsHit = true
			http.Error(w, "should not be called", http.StatusInternalServerError)
		default:
			http.Error(w, "not found", http.StatusNotFound)
		}
	}))
	defer server.Close()

	conn := NewModelConnection(
		&config.ProviderConnection{APIType: "openai", Endpoint: server.URL},
		&config.ModelConfig{Model: "gpt-4o"},
	)
	models, err := conn.ListModels()
	if err != nil {
		t.Fatalf("ListModels failed: %v", err)
	}
	if tagsHit {
		t.Error("/api/tags probed even though /models served a list")
	}
	if len(models) != 1 || models[0].ID != "gpt-4o" {
		t.Errorf("unexpected models: %+v", models)
	}
}

func TestNonOpenAIListingSkipsTagsFallback(t *testing.T) {
	// Hosted gateways (here Z.AI) leave tagsPath empty: a /models failure must NOT
	// trigger an /api/tags probe, and the original error is surfaced.
	var tagsHit bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/tags" {
			tagsHit = true
		}
		http.Error(w, "nope", http.StatusNotFound)
	}))
	defer server.Close()

	conn := NewModelConnection(
		&config.ProviderConnection{APIType: "zai", Endpoint: server.URL},
		&config.ModelConfig{Model: "glm-4.6"},
	)
	if _, err := conn.ListModels(); err == nil {
		t.Fatal("expected ListModels to fail when /models 404s and no fallback applies")
	}
	if tagsHit {
		t.Error("zai listing probed /api/tags; fallback must be OpenAI-only")
	}
}

func TestOpenAIListingFallbackBothEndpointsFail(t *testing.T) {
	// Neither /models nor /api/tags works (not an Ollama server at all): the
	// operator must get the original /models error, not a misleading generic one.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "404 page not found", http.StatusNotFound)
	}))
	defer server.Close()

	conn := NewModelConnection(
		&config.ProviderConnection{APIType: "openai", Endpoint: server.URL},
		&config.ModelConfig{Model: "llama3"},
	)
	_, err := conn.ListModels()
	if err == nil {
		t.Fatal("expected an error when both /models and /api/tags fail")
	}
}

func TestListModelsNameKeyedShape(t *testing.T) {
	// Non-OpenAI listings key the identifier on "name" rather than "id" and wrap
	// the list under "models" (Ollama's /api/tags, Gemini's /v1beta/models). The
	// ModelInfo decoder must fall back to name so these entries are not dropped.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"models":[{"name":"llama3:latest"},{"name":"mistral"}]}`))
	}))
	defer server.Close()

	conn := NewModelConnection(
		&config.ProviderConnection{Endpoint: server.URL},
		&config.ModelConfig{Model: "llama3"},
	)
	models, err := conn.ListModels()
	if err != nil {
		t.Fatalf("ListModels failed: %v", err)
	}
	if len(models) != 2 || models[0].ID != "llama3:latest" || models[1].ID != "mistral" {
		t.Errorf("unexpected models: %+v", models)
	}
}
