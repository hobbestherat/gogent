package model

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestModelConnectionDefaultURL(t *testing.T) {
	c := NewModelConnection()
	if c.URL != DefaultModelURL {
		t.Errorf("Expected default URL %q, got %q", DefaultModelURL, c.URL)
	}
}

func TestListModelsWithMockServer(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"model-a","owned_by":"acme"},{"id":"model-b"}]}`))
	}))
	defer server.Close()

	c := NewModelConnection()
	c.SetURL(server.URL + "/v1/chat/completions")

	models, err := c.ListModels()
	if err != nil {
		t.Fatalf("ListModels failed: %v", err)
	}
	if len(models) != 2 || models[0].ID != "model-a" || models[1].ID != "model-b" {
		t.Errorf("Unexpected models: %+v", models)
	}
}

func TestModelConnectionStats(t *testing.T) {
	c := NewModelConnection()

	stats := c.GetStats()
	if stats.RequestCount != 0 {
		t.Errorf("Expected RequestCount 0, got %d", stats.RequestCount)
	}
}

func TestModelConnectionSetters(t *testing.T) {
	c := NewModelConnection()

	c.SetURL("http://test:8080")
	if c.URL != "http://test:8080" {
		t.Errorf("Expected URL http://test:8080, got %q", c.URL)
	}

	c.SetTimeout(5 * time.Second)
	if c.Timeout != 5*time.Second {
		t.Errorf("Expected timeout 5s, got %v", c.Timeout)
	}
}

func TestModelConnectionWithMockServer(t *testing.T) {
	var requestCount int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount++
		w.Header().Set("Content-Type", "application/json")

		var req CompletionRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}

		if len(req.Messages) == 0 {
			t.Error("Expected messages")
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}

		w.WriteHeader(http.StatusOK)

		response := CompletionResponse{
			Content:      "Hello!",
			Role:         RoleAssistant,
			FinishReason: "stop",
			Usage: &TokenUsage{
				PromptTokens:     5,
				CompletionTokens: 2,
				TotalTokens:      7,
			},
		}

		json.NewEncoder(w).Encode(response)
	}))
	defer server.Close()

	c := NewModelConnection()
	c.SetURL(server.URL)

	resp, err := c.Complete([]Message{{Role: RoleUser, Content: "hi"}})

	if err != nil {
		t.Errorf("Unexpected error: %v", err)
	}

	if resp.Content != "Hello!" {
		t.Errorf("Expected 'Hello!', got %q", resp.Content)
	}

	if resp.Usage == nil {
		t.Error("Expected usage in response")
	}

	if resp.Usage.PromptTokens != 5 {
		t.Errorf("Expected 5 prompt tokens, got %d", resp.Usage.PromptTokens)
	}

	if requestCount != 1 {
		t.Errorf("Expected 1 request, got %d", requestCount)
	}
}

func TestModelConnectionWithEmptyURL(t *testing.T) {
	c := NewModelConnection()
	c.SetURL("")
	_, err := c.Complete([]Message{{Role: RoleUser, Content: "hi"}})

	if err == nil {
		t.Error("Expected error with empty URL, got nil")
	}

	modelErr, ok := err.(*ModelError)
	if !ok {
		t.Errorf("Expected ModelError, got %T", err)
		return
	}

	if modelErr.Type != ErrorConnection {
		t.Errorf("Expected ErrorConnection, got %v", modelErr.Type)
	}
}
