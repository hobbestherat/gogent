package model

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"gogent/internal/config"
)

// TestProviderSpecAuthHeaders covers the per-spec auth policy: each authMode
// places the key in the right place, static extraHeaders ride along, and an
// empty key contributes no auth header (only the extras).
func TestProviderSpecAuthHeaders(t *testing.T) {
	cases := []struct {
		name   string
		spec   providerSpec
		key    string
		want   map[string]string // header -> value ("" means must be absent)
		absent []string
	}{
		{
			name: "bearer",
			spec: providerSpec{authMode: authBearer},
			key:  "k1",
			want: map[string]string{"Authorization": "Bearer k1"},
		},
		{
			name: "zero value defaults to bearer",
			spec: providerSpec{},
			key:  "k2",
			want: map[string]string{"Authorization": "Bearer k2"},
		},
		{
			name:   "x-api-key with version pin",
			spec:   providerSpec{authMode: authXAPIKey, extraHeaders: map[string]string{"anthropic-version": anthropicVersion}},
			key:    "k3",
			want:   map[string]string{"x-api-key": "k3", "anthropic-version": anthropicVersion},
			absent: []string{"Authorization"},
		},
		{
			name:   "azure api-key",
			spec:   providerSpec{authMode: authAzureKey},
			key:    "k4",
			want:   map[string]string{"api-key": "k4"},
			absent: []string{"Authorization"},
		},
		{
			name:   "query auth contributes no header",
			spec:   providerSpec{authMode: authQuery, authQueryParam: "key"},
			key:    "k5",
			absent: []string{"Authorization", "x-api-key", "api-key", "key"},
		},
		{
			name: "bearer with attribution headers",
			spec: providerSpec{authMode: authBearer, extraHeaders: map[string]string{"HTTP-Referer": openRouterReferer, "X-Title": openRouterTitle}},
			key:  "k6",
			want: map[string]string{"Authorization": "Bearer k6", "HTTP-Referer": openRouterReferer, "X-Title": openRouterTitle},
		},
		{
			name:   "empty key yields only extras",
			spec:   providerSpec{authMode: authBearer, extraHeaders: map[string]string{"X-Title": openRouterTitle}},
			key:    "",
			want:   map[string]string{"X-Title": openRouterTitle},
			absent: []string{"Authorization"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := tc.spec.authHeaders(tc.key)
			for k, v := range tc.want {
				if got := h.Get(k); got != v {
					t.Errorf("header %q = %q, want %q", k, got, v)
				}
			}
			for _, k := range tc.absent {
				if got := h.Get(k); got != "" {
					t.Errorf("header %q = %q, want absent", k, got)
				}
			}
		})
	}
}

func TestStringToAPITypeOpenRouter(t *testing.T) {
	for _, in := range []string{"openrouter", "OpenRouter", " OPENROUTER "} {
		if got := StringToAPIType(in); got != APITypeOpenRouter {
			t.Errorf("StringToAPIType(%q) = %q, want openrouter", in, got)
		}
	}
	var found bool
	for _, id := range APITypeIDs() {
		if id == string(APITypeOpenRouter) {
			found = true
		}
	}
	if !found {
		t.Errorf("APITypeIDs() = %v, missing openrouter", APITypeIDs())
	}
}

// TestOpenRouterEndpoints checks that the openrouter api_type supplies the base
// URL automatically and keeps the OpenAI-compatible wire adapter.
func TestOpenRouterEndpoints(t *testing.T) {
	conn := NewModelConnectionFromConfig(&config.ModelConfig{
		APIType: "openrouter",
		Model:   "google/gemma-3-27b-it:free",
	})
	if want := "https://openrouter.ai/api/v1/chat/completions"; conn.URL != want {
		t.Errorf("chat URL = %q, want %q", conn.URL, want)
	}
	if want := "https://openrouter.ai/api/v1/models"; conn.modelsURL() != want {
		t.Errorf("models URL = %q, want %q", conn.modelsURL(), want)
	}
	if _, ok := conn.wireAdapter().(openAIAdapter); !ok {
		t.Errorf("adapter = %T, want openAIAdapter", conn.wireAdapter())
	}
}

// TestOpenRouterAttribution drives the full blocking path through a fake server
// and asserts the bearer key plus the recommended HTTP-Referer / X-Title
// attribution headers are sent.
func TestOpenRouterAttribution(t *testing.T) {
	var gotAuth, gotReferer, gotTitle string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotReferer = r.Header.Get("HTTP-Referer")
		gotTitle = r.Header.Get("X-Title")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}]}`))
	}))
	defer server.Close()

	conn := NewModelConnectionFromConfig(&config.ModelConfig{
		APIType:  "openrouter",
		Endpoint: server.URL,
		Model:    "google/gemma-3-27b-it:free",
		APIKey:   "or-secret",
	})
	if _, err := conn.Complete([]Message{{Role: RoleUser, Content: "hi"}}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if gotAuth != "Bearer or-secret" {
		t.Errorf("Authorization = %q, want %q", gotAuth, "Bearer or-secret")
	}
	if gotReferer != openRouterReferer {
		t.Errorf("HTTP-Referer = %q, want %q", gotReferer, openRouterReferer)
	}
	if gotTitle != openRouterTitle {
		t.Errorf("X-Title = %q, want %q", gotTitle, openRouterTitle)
	}
}

// TestAPIKeyRoundTripperQueryAuth verifies that query-parameter auth places the
// key in the URL (preserving any existing query), and does not also emit a
// bearer header.
func TestAPIKeyRoundTripperQueryAuth(t *testing.T) {
	var gotKey, gotAlt, gotAuth string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey = r.URL.Query().Get("key")
		gotAlt = r.URL.Query().Get("alt")
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	spec := providerSpec{authMode: authQuery, authQueryParam: "key"}
	client := &http.Client{Transport: &APIKeyRoundTripper{
		apiKey:     "g-secret",
		headers:    spec.authHeaders("g-secret"),
		queryParam: spec.authQuery(),
	}}
	req, _ := http.NewRequest("GET", server.URL+"/v1beta/models?alt=sse", nil)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	resp.Body.Close()

	if gotKey != "g-secret" {
		t.Errorf("query key = %q, want g-secret", gotKey)
	}
	if gotAlt != "sse" {
		t.Errorf("pre-existing query param lost: alt = %q, want sse", gotAlt)
	}
	if gotAuth != "" {
		t.Errorf("query auth must not also send Authorization, got %q", gotAuth)
	}
}

// TestAPIKeyRoundTripperAzureAuth verifies Azure's api-key header scheme through
// the round-tripper, without a bearer fallback.
func TestAPIKeyRoundTripperAzureAuth(t *testing.T) {
	var gotKey, gotAuth string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey = r.Header.Get("api-key")
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	spec := providerSpec{authMode: authAzureKey}
	client := &http.Client{Transport: &APIKeyRoundTripper{
		apiKey:     "az-secret",
		headers:    spec.authHeaders("az-secret"),
		queryParam: spec.authQuery(),
	}}
	req, _ := http.NewRequest("GET", server.URL, nil)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	resp.Body.Close()

	if gotKey != "az-secret" {
		t.Errorf("api-key = %q, want az-secret", gotKey)
	}
	if gotAuth != "" {
		t.Errorf("azure auth must not send Authorization, got %q", gotAuth)
	}
}
