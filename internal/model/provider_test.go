package model

import (
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
	openai := specFor(APITypeOpenAI)
	zai := specFor(APITypeZAI)

	cases := []struct {
		name     string
		endpoint string
		spec     providerSpec
		want     string
	}{
		{"openai base url", "https://api.example.com/v1", openai, "https://api.example.com/v1"},
		{"openai full url", "https://api.example.com/v1/chat/completions", openai, "https://api.example.com/v1"},
		{"openai trailing slash", "https://api.example.com/v1/", openai, "https://api.example.com/v1"},
		{"openai empty default", "", openai, "http://localhost:8080/v1"},
		{"zai empty default", "", zai, "https://api.z.ai/api/paas/v4"},
		{"zai full url collapses", "https://api.z.ai/api/paas/v4/chat/completions", zai, "https://api.z.ai/api/paas/v4"},
	}
	for _, tc := range cases {
		if got := normalizeBaseURL(tc.endpoint, tc.spec); got != tc.want {
			t.Errorf("%s: normalizeBaseURL(%q) = %q, want %q", tc.name, tc.endpoint, got, tc.want)
		}
	}
}

func TestNewModelConnectionFromConfigBaseURL(t *testing.T) {
	// An OpenAI backend given only a base URL gets /chat/completions appended.
	conn := NewModelConnectionFromConfig(&config.ModelConfig{
		Endpoint: "https://api.example.com/v1",
		Model:    "m",
	})
	if want := "https://api.example.com/v1/chat/completions"; conn.URL != want {
		t.Errorf("openai chat URL = %q, want %q", conn.URL, want)
	}
	if conn.APIType != APITypeOpenAI {
		t.Errorf("APIType = %q, want openai", conn.APIType)
	}

	// A Z.AI backend with no endpoint uses the provider default base URL.
	zconn := NewModelConnectionFromConfig(&config.ModelConfig{
		APIType: "zai",
		Model:   "glm-4.6",
	})
	if want := "https://api.z.ai/api/paas/v4/chat/completions"; zconn.URL != want {
		t.Errorf("zai chat URL = %q, want %q", zconn.URL, want)
	}
	if want := "https://api.z.ai/api/paas/v4/models"; zconn.modelsURL() != want {
		t.Errorf("zai models URL = %q, want %q", zconn.modelsURL(), want)
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

	conn := NewModelConnectionFromConfig(&config.ModelConfig{
		APIType:  "zai",
		Endpoint: server.URL,
		Model:    "glm-4.6",
	})
	models, err := conn.ListModels()
	if err != nil {
		t.Fatalf("ListModels failed: %v", err)
	}
	if len(models) != 1 || models[0].ID != "glm-4.6" {
		t.Errorf("unexpected models: %+v", models)
	}
}
