package model

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/oauth2"

	"gogent/internal/config"
)

type staticTokenSource struct {
	token string
	calls atomic.Int64
}

func (s *staticTokenSource) Token() (*oauth2.Token, error) {
	s.calls.Add(1)
	return &oauth2.Token{AccessToken: s.token, TokenType: "Bearer", Expiry: time.Now().Add(time.Hour)}, nil
}

type errorTokenSource struct {
	err error
}

func (s errorTokenSource) Token() (*oauth2.Token, error) {
	return nil, s.err
}

type captureRoundTripper struct {
	req *http.Request
}

func (rt *captureRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	rt.req = req
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader("ok")),
		Header:     make(http.Header),
		Request:    req,
	}, nil
}

func withFakeADCTokenSource(t *testing.T, fn func(context.Context, ...string) (oauth2.TokenSource, error)) {
	t.Helper()
	orig := adcTokenSourceFunc
	adcTokenSourceFunc = fn
	t.Cleanup(func() { adcTokenSourceFunc = orig })
}

func TestVertexAPITypeAndSpec(t *testing.T) {
	for _, in := range []string{"vertex", "Vertex", " VERTEX "} {
		if got := StringToAPIType(in); got != APITypeVertex {
			t.Errorf("StringToAPIType(%q) = %q, want vertex", in, got)
		}
	}

	found := false
	for _, id := range APITypeIDs() {
		if id == string(APITypeVertex) {
			found = true
		}
	}
	if !found {
		t.Fatalf("APITypeIDs() = %v, missing vertex", APITypeIDs())
	}

	spec := specFor(APITypeVertex)
	if spec.authMode != authADC {
		t.Errorf("authMode = %q, want adc", spec.authMode)
	}
	if !spec.supportsResponseFormat {
		t.Error("supportsResponseFormat = false, want true for OpenAI-compatible Vertex endpoint")
	}
	if spec.supportsReasoningEffort {
		t.Error("supportsReasoningEffort = true, want false for Vertex OpenAI-compatible endpoint")
	}
	if spec.supportsThinking {
		t.Error("supportsThinking = true, want false for Vertex OpenAI-compatible endpoint")
	}
	if spec.modelsPath != "" {
		t.Errorf("modelsPath = %q, want empty because Vertex compat model listing is unsupported", spec.modelsPath)
	}
	if _, ok := adapterFor(APITypeVertex).(openAIAdapter); !ok {
		t.Errorf("adapterFor(vertex) = %T, want openAIAdapter", adapterFor(APITypeVertex))
	}
}

func TestVertexChatURLFromProjectLocation(t *testing.T) {
	cases := []struct {
		name     string
		project  string
		location string
		want     string
	}{
		{
			name:     "regional",
			project:  "gogent-prod",
			location: "us-central1",
			want:     "https://us-central1-aiplatform.googleapis.com/v1beta1/projects/gogent-prod/locations/us-central1/endpoints/openapi/chat/completions",
		},
		{
			name:     "global drops host prefix only",
			project:  "gogent-prod",
			location: "global",
			want:     "https://aiplatform.googleapis.com/v1beta1/projects/gogent-prod/locations/global/endpoints/openapi/chat/completions",
		},
		{
			name:     "trims whitespace",
			project:  "  my-project  ",
			location: " europe-west4 ",
			want:     "https://europe-west4-aiplatform.googleapis.com/v1beta1/projects/my-project/locations/europe-west4/endpoints/openapi/chat/completions",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			conn := NewModelConnectionFromConfig(&config.ModelConfig{
				APIType:  "vertex",
				Project:  tc.project,
				Location: tc.location,
				Model:    "google/gemini-2.5-flash",
			})
			if conn.URL != tc.want {
				t.Fatalf("URL = %q, want %q", conn.URL, tc.want)
			}
		})
	}
}

func TestVertexExplicitEndpointOverridesProjectLocation(t *testing.T) {
	conn := NewModelConnectionFromConfig(&config.ModelConfig{
		APIType:  "vertex",
		Endpoint: "https://proxy.example.test/root",
		Project:  "ignored-project",
		Location: "ignored-location",
		Model:    "google/gemini-2.5-flash",
	})
	if want := "https://proxy.example.test/root/endpoints/openapi/chat/completions"; conn.URL != want {
		t.Fatalf("URL = %q, want %q", conn.URL, want)
	}
}

func TestADCRoundTripperAddsBearerTokenOnClone(t *testing.T) {
	ts := &staticTokenSource{token: "adc-token"}
	base := &captureRoundTripper{}
	rt := &ADCRoundTripper{tokenSource: ts, transport: base}

	req, err := http.NewRequest("GET", "https://vertex.example.test/v1", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer caller-token")

	resp, err := rt.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	_ = resp.Body.Close()

	if ts.calls.Load() != 1 {
		t.Errorf("Token calls = %d, want 1", ts.calls.Load())
	}
	if got := base.req.Header.Get("Authorization"); got != "Bearer adc-token" {
		t.Errorf("sent Authorization = %q, want %q", got, "Bearer adc-token")
	}
	if got := req.Header.Get("Authorization"); got != "Bearer caller-token" {
		t.Errorf("original request Authorization mutated to %q", got)
	}
	if base.req == req {
		t.Error("RoundTrip forwarded original request; want cloned request")
	}
}

func TestADCRoundTripperTokenErrorIsActionable(t *testing.T) {
	rt := &ADCRoundTripper{tokenSource: errorTokenSource{err: errors.New("no credentials")}, transport: &captureRoundTripper{}}
	req, err := http.NewRequest("GET", "https://vertex.example.test/v1", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := rt.RoundTrip(req)
	if resp != nil {
		_ = resp.Body.Close()
	}
	if err == nil {
		t.Fatal("RoundTrip error = nil, want ADC setup guidance")
	}
	msg := err.Error()
	for _, want := range []string{"vertex ADC credentials not found", "gcloud auth application-default login", "GOOGLE_APPLICATION_CREDENTIALS", "no credentials"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q does not contain %q", msg, want)
		}
	}
}

func TestVertexConnectionUsesADCSharedTransport(t *testing.T) {
	conn := NewModelConnectionFromConfig(&config.ModelConfig{
		APIType:  "vertex",
		Project:  "p",
		Location: "us-central1",
		Model:    "google/gemini-2.5-flash",
		APIKey:   "must-not-use-api-key",
	})
	rt, ok := conn.client.Transport.(*ADCRoundTripper)
	if !ok {
		t.Fatalf("Transport = %T, want *ADCRoundTripper", conn.client.Transport)
	}
	if rt.transport != sharedHTTPTransport {
		t.Errorf("ADC transport = %p, want shared transport %p", rt.transport, sharedHTTPTransport)
	}
	if _, ok := conn.client.Transport.(*APIKeyRoundTripper); ok {
		t.Fatal("vertex must not use APIKeyRoundTripper")
	}
}

func TestVertexConnectionCompleteUsesOpenAIWireAndADC(t *testing.T) {
	tokenSource := &staticTokenSource{token: "vertex-access-token"}
	var scopesSeen []string
	withFakeADCTokenSource(t, func(ctx context.Context, scopes ...string) (oauth2.TokenSource, error) {
		scopesSeen = append([]string(nil), scopes...)
		return tokenSource, nil
	})

	var gotAuth, gotPath string
	var got CompletionRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"hello from vertex"},"finish_reason":"stop"}],"usage":{"prompt_tokens":2,"completion_tokens":3,"total_tokens":5}}`))
	}))
	defer server.Close()

	conn := NewModelConnectionFromConfig(&config.ModelConfig{
		APIType:     "vertex",
		Endpoint:    server.URL,
		Project:     "ignored-when-endpoint-set",
		Location:    "global",
		Model:       "google/gemini-2.5-flash",
		APIKey:      "must-not-be-sent",
		Temperature: 0.25,
		MaxTokens:   123,
	})
	resp, err := conn.Complete([]Message{{Role: RoleUser, Content: "hi"}})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}

	if gotPath != "/endpoints/openapi/chat/completions" {
		t.Errorf("path = %q, want Vertex OpenAI-compatible chat path", gotPath)
	}
	if gotAuth != "Bearer vertex-access-token" {
		t.Errorf("Authorization = %q, want ADC bearer token", gotAuth)
	}
	if len(scopesSeen) != 1 || scopesSeen[0] != adcScope {
		t.Errorf("ADC scopes = %v, want [%q]", scopesSeen, adcScope)
	}
	if tokenSource.calls.Load() != 1 {
		t.Errorf("Token calls = %d, want 1", tokenSource.calls.Load())
	}
	if got.Model != "google/gemini-2.5-flash" {
		t.Errorf("body model = %q, want configured model", got.Model)
	}
	if len(got.Messages) != 1 || got.Messages[0].Role != RoleUser || got.Messages[0].Content != "hi" {
		t.Errorf("messages = %+v, want one user message", got.Messages)
	}
	if got.Temperature == nil || *got.Temperature != 0.25 {
		t.Errorf("temperature = %v, want 0.25", got.Temperature)
	}
	if got.MaxTokens == nil || *got.MaxTokens != 123 {
		t.Errorf("max_tokens = %v, want 123", got.MaxTokens)
	}
	if resp.Content != "hello from vertex" || resp.Role != RoleAssistant || resp.FinishReason != "stop" {
		t.Errorf("response = %+v, want flattened OpenAI-compatible response", resp)
	}
}

func TestVertexStructuredOutputRequestKeepsOpenAIResponseFormat(t *testing.T) {
	withFakeADCTokenSource(t, func(ctx context.Context, scopes ...string) (oauth2.TokenSource, error) {
		return &staticTokenSource{token: "structured-token"}, nil
	})

	var got CompletionRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"{\"ok\":true}"},"finish_reason":"stop"}]}`))
	}))
	defer server.Close()

	conn := NewModelConnectionFromConfig(&config.ModelConfig{
		APIType:  "vertex",
		Endpoint: server.URL,
		Model:    "google/gemini-2.5-flash",
	})
	format := JSONSchemaResponseFormat("result", map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"properties": map[string]any{
			"ok": map[string]any{"type": "boolean"},
		},
		"required": []string{"ok"},
	})
	if _, err := conn.CompleteStructuredCtx(context.Background(), []Message{{Role: RoleUser, Content: "json"}}, nil, format); err != nil {
		t.Fatalf("CompleteStructuredCtx: %v", err)
	}
	if got.ResponseFormat == nil {
		t.Fatal("response_format missing; Vertex compat should keep OpenAI structured-output wire field")
	}
	if got.ResponseFormat.Type != "json_schema" || got.ResponseFormat.JSONSchema == nil || got.ResponseFormat.JSONSchema.Name != "result" {
		t.Errorf("response_format = %+v, want json_schema named result", got.ResponseFormat)
	}
}

func TestVertexADCFailureSurfacesOnFirstRequest(t *testing.T) {
	withFakeADCTokenSource(t, func(ctx context.Context, scopes ...string) (oauth2.TokenSource, error) {
		return nil, errors.New("adc unavailable")
	})

	conn := NewModelConnectionFromConfig(&config.ModelConfig{
		APIType:  "vertex",
		Endpoint: "http://127.0.0.1:1",
		Model:    "google/gemini-2.5-flash",
	})
	conn.maxAttempts = 1

	_, err := conn.Complete([]Message{{Role: RoleUser, Content: "hi"}})
	if err == nil {
		t.Fatal("Complete error = nil, want ADC error")
	}
	msg := err.Error()
	for _, want := range []string{"failed to connect to model", "vertex ADC credentials not found", "gcloud auth application-default login", "GOOGLE_APPLICATION_CREDENTIALS", "adc unavailable"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q does not contain %q", msg, want)
		}
	}
}
