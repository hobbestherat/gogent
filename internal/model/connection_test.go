package model

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestModelConnectionDefaultURL(t *testing.T) {
	c := newPlaceholderConnection()
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

	c := newPlaceholderConnection()
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
	c := newPlaceholderConnection()

	stats := c.GetStats()
	if stats.RequestCount != 0 {
		t.Errorf("Expected RequestCount 0, got %d", stats.RequestCount)
	}
}

func TestModelConnectionSetters(t *testing.T) {
	c := newPlaceholderConnection()

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

	c := newPlaceholderConnection()
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

// newTestConn returns a connection pointed at url with backoff disabled so the
// retry logic can be exercised without sleeping.
func newTestConn(url string) *ModelConnection {
	c := newPlaceholderConnection()
	c.SetURL(url)
	c.retryBaseDelay = 0
	c.retryMaxDelay = 0
	return c
}

func TestCompleteRetryByStatus(t *testing.T) {
	okResponse := func(w http.ResponseWriter) {
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(CompletionResponse{Content: "ok", Role: RoleAssistant})
	}

	tests := []struct {
		name         string
		status       int
		wantAttempts int
		wantErrType  ModelErrorType
	}{
		{"bad_request_fails_fast", http.StatusBadRequest, 1, ErrorGeneric},
		{"unauthorized_fails_fast", http.StatusUnauthorized, 1, ErrorGeneric},
		{"forbidden_fails_fast", http.StatusForbidden, 1, ErrorGeneric},
		{"unprocessable_fails_fast", http.StatusUnprocessableEntity, 1, ErrorGeneric},
		{"too_many_requests_retries", http.StatusTooManyRequests, 3, ErrorRateLimit},
		{"request_timeout_retries", http.StatusRequestTimeout, 3, ErrorGeneric},
		{"conflict_retries", http.StatusConflict, 3, ErrorGeneric},
		{"server_error_retries", http.StatusInternalServerError, 3, ErrorGeneric},
		{"bad_gateway_retries", http.StatusBadGateway, 3, ErrorGeneric},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var attempts int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				atomic.AddInt32(&attempts, 1)
				http.Error(w, "boom", tc.status)
			}))
			defer server.Close()

			c := newTestConn(server.URL)
			_, err := c.Complete([]Message{{Role: RoleUser, Content: "hi"}})
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			if got := int(atomic.LoadInt32(&attempts)); got != tc.wantAttempts {
				t.Errorf("status %d: expected %d attempts, got %d", tc.status, tc.wantAttempts, got)
			}
			modelErr, ok := err.(*ModelError)
			if !ok {
				t.Fatalf("expected *ModelError, got %T", err)
			}
			if modelErr.Type != tc.wantErrType {
				t.Errorf("status %d: expected error type %q, got %q", tc.status, tc.wantErrType, modelErr.Type)
			}
		})
	}

	t.Run("recovers_after_transient_error", func(t *testing.T) {
		var attempts int32
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if atomic.AddInt32(&attempts, 1) == 1 {
				http.Error(w, "try again", http.StatusServiceUnavailable)
				return
			}
			okResponse(w)
		}))
		defer server.Close()

		c := newTestConn(server.URL)
		resp, err := c.Complete([]Message{{Role: RoleUser, Content: "hi"}})
		if err != nil {
			t.Fatalf("expected success after retry, got %v", err)
		}
		if resp.Content != "ok" {
			t.Errorf("expected content %q, got %q", "ok", resp.Content)
		}
		if got := int(atomic.LoadInt32(&attempts)); got != 2 {
			t.Errorf("expected 2 attempts, got %d", got)
		}
	})
}

// TestCompleteRequestBodyConsistentAcrossRetries verifies the two halves of the
// issue #20 fix in the blocking path: the request body is marshaled ONCE before
// the retry loop (so it is identical on every attempt), and it lives in a pooled
// buffer that must stay valid for the whole loop — not be released or overwritten
// mid-retry. The server fails twice then succeeds; every attempt must carry the
// exact same body bytes.
func TestCompleteRequestBodyConsistentAcrossRetries(t *testing.T) {
	var mu sync.Mutex
	var bodies [][]byte
	var attempts int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, append([]byte(nil), body...))
		mu.Unlock()
		if atomic.AddInt32(&attempts, 1) < 3 {
			http.Error(w, "try again", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(CompletionResponse{Content: "ok", Role: RoleAssistant})
	}))
	defer server.Close()

	c := newTestConn(server.URL)
	if _, err := c.Complete([]Message{{Role: RoleUser, Content: "hi"}}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if len(bodies) != 3 {
		t.Fatalf("got %d attempts, want 3", len(bodies))
	}
	for i := 1; i < len(bodies); i++ {
		if !bytes.Equal(bodies[i], bodies[0]) {
			t.Errorf("request body differs on retry %d: pooled marshal buffer must stay live for the whole retry loop", i)
		}
	}
}

func TestIsRetryableStatus(t *testing.T) {
	retryable := []int{408, 409, 429, 500, 502, 503, 504, 599}
	permanent := []int{200, 400, 401, 403, 404, 422, 451}
	for _, code := range retryable {
		if !isRetryableStatus(code) {
			t.Errorf("status %d should be retryable", code)
		}
	}
	for _, code := range permanent {
		if isRetryableStatus(code) {
			t.Errorf("status %d should not be retryable", code)
		}
	}
}

func TestParseRetryAfter(t *testing.T) {
	now := time.Date(2026, 6, 18, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name   string
		header string
		wantOK bool
		want   time.Duration
	}{
		{"empty", "", false, 0},
		{"seconds", "12", true, 12 * time.Second},
		{"zero_seconds", "0", true, 0},
		{"negative_seconds", "-5", false, 0},
		{"garbage", "soon", false, 0},
		{"http_date_future", now.Add(30 * time.Second).UTC().Format(http.TimeFormat), true, 30 * time.Second},
		{"http_date_past", now.Add(-30 * time.Second).UTC().Format(http.TimeFormat), true, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := parseRetryAfter(tc.header, now)
			if ok != tc.wantOK {
				t.Errorf("ok = %v, want %v", ok, tc.wantOK)
			}
			if got != tc.want {
				t.Errorf("delay = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestBackoffHonorsRetryAfter(t *testing.T) {
	c := newPlaceholderConnection()
	c.retryBaseDelay = time.Second
	c.retryMaxDelay = 30 * time.Second

	if got := c.backoff(0, 5*time.Second); got != 5*time.Second {
		t.Errorf("expected Retry-After of 5s, got %v", got)
	}
	// Retry-After is capped by retryMaxDelay.
	if got := c.backoff(0, time.Hour); got != 30*time.Second {
		t.Errorf("expected Retry-After capped at 30s, got %v", got)
	}
	// Full jitter stays within [0, base*2^attempt], capped at retryMaxDelay.
	for attempt := 0; attempt < 8; attempt++ {
		got := c.backoff(attempt, 0)
		if got < 0 || got > c.retryMaxDelay {
			t.Errorf("attempt %d: backoff %v out of range [0, %v]", attempt, got, c.retryMaxDelay)
		}
	}
}

func TestModelConnectionWithEmptyURL(t *testing.T) {
	c := newPlaceholderConnection()
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

// TestAnalyzeErrorCounters verifies analyzeError classifies each status and bumps
// the matching ModelStats counter — including the 429 rate-limit case, whose
// counter was previously left at zero by a no-op Lock/Unlock pair (issue #51).
func TestAnalyzeErrorCounters(t *testing.T) {
	tests := []struct {
		name     string
		status   int
		response string
		wantType ModelErrorType
		counter  func(*ModelStats) int
	}{
		{"rate_limit", 429, "slow down", ErrorRateLimit, func(s *ModelStats) int { return s.RateLimitCount }},
		{"context_overflow", 400, "context length exceeded", ErrorContextOverflow, func(s *ModelStats) int { return s.ContextWindowOverflowCount }},
		{"refusal", 403, "content refusal", ErrorRefusal, func(s *ModelStats) int { return s.RefusalCount }},
		{"timeout", 504, "gateway", ErrorTimeout, func(s *ModelStats) int { return s.TimeoutCount }},
		{"generic", 500, "boom", ErrorGeneric, func(s *ModelStats) int { return s.GenericErrorCount }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := newPlaceholderConnection()
			me := c.analyzeError(tt.status, tt.response)
			if me.Type != tt.wantType {
				t.Errorf("type = %q, want %q", me.Type, tt.wantType)
			}
			stats := c.GetStats()
			if got := tt.counter(stats); got != 1 {
				t.Errorf("specific counter = %d, want 1", got)
			}
			// Every error path also bumps the overall error count exactly once.
			if stats.ErrorCount != 1 {
				t.Errorf("ErrorCount = %d, want 1", stats.ErrorCount)
			}
		})
	}
}
