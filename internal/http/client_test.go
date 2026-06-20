package http

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestClientPost(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			t.Errorf("Expected POST, got %s", r.Method)
		}

		if r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("Expected Content-Type application/json, got %s", r.Header.Get("Content-Type"))
		}
	}))
	defer server.Close()

	client := NewClient().SetBaseURL(server.URL)
	resp, err := client.Post("/test", nil)

	if err != nil {
		t.Errorf("Unexpected error: %v", err)
	}
	if resp != nil {
		defer resp.Body.Close()
	}
}

func TestClientTimeout(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
	}))
	defer server.Close()

	client := NewClient().SetBaseURL(server.URL).SetTimeout(100 * time.Millisecond)

	if client.GetTimeout() != 100*time.Millisecond {
		t.Errorf("Expected timeout 100ms, got %v", client.GetTimeout())
	}
}
