package model

// Adversarial test suite for issue #574: the vertex OpenAI-compat shim silently
// accepts a bare model name (gemini-3.5-flash) and sends it verbatim, so the
// request reaches Vertex and fails with an opaque "Malformed publisher model"
// 400. The fix has three layers, each exercised here against all four design
// criteria:
//
//  1. validateRoutableConfig rejects a mis-shaped model id at save/load
//     (shim requires "google/<model>"; native requires bare).
//  2. The shim provider's normalizeModelID auto-qualifies a bare id at the send
//     seam (buildRequest) — the last-line defense for a connection that bypassed
//     validation (e.g. hand-built / library use).
//  3. extractProviderMessage handles the array form [{"error":{…}}] Vertex
//     returns for this 400, and analyzeError surfaces the reason as ErrorGeneric.
//
// These tests live in package `model` so they can call the private buildRequest
// / extractProviderMessage / analyzeError helpers and inspect configErr / Stats.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"golang.org/x/oauth2"

	"gogent/internal/config"
)

// vertexPublisher400Message is the realistic Vertex reason for the bare-name 400,
// in the exact wording the issue reports (backticks and all).
const vertexPublisher400Message = "Malformed publisher model (`model`: 'gemini-3.5-flash') for the 'openapi' request endpoint ID; expected '<publisher>/<model>'."

// vertexPublisher400Body is that reason wrapped in the JSON ARRAY shape Vertex
// uses for this error (a single-element array of error objects).
const vertexPublisher400Body = `[{"error":{"code":400,"message":"` + vertexPublisher400Message + `","status":"INVALID_ARGUMENT"}}]`

// ---------------------------------------------------------------------------
// Criterion 1 — validate at save & load (validateRoutableConfig / ValidateModelConfig)
// ---------------------------------------------------------------------------

func TestValidateModelConfig_VertexShimPublisherPrefix(t *testing.T) {
	tests := []struct {
		name        string
		model       string
		wantReject  bool
		wantSubs    []string // substrings the rejection message must contain
		wantNotSubs []string // substrings it must NOT contain
	}{
		{
			name:       "bare name rejected with actionable message",
			model:      "gemini-3.5-flash",
			wantReject: true,
			wantSubs: []string{
				`api_type "vertex"`,
				"publisher-qualified",
				`"google/gemini-3.5-flash"`,
				`got "gemini-3.5-flash"`,
				`"vertex-native"`,
				`(alias "gemini")`,
			},
		},
		{
			// The "/" presence is all the rule checks; any publisher-qualified id
			// passes (the issue is specifically a *bare* name). This documents that
			// scope — a wrong-but-qualified publisher is not this rule's concern.
			name:  "wrong publisher but qualified is accepted (out of scope)",
			model: "anthropic/claude-opus-4-8", wantReject: false,
		},
		{
			// Empty model is deliberately left to other rules (the m != "" guard);
			// vertex derives its base, so an empty model on the shim is accepted at
			// validation time today (mirrors the anthropic empty-model scoping).
			name:  "empty model not rejected by this rule",
			model: "", wantReject: false,
		},
		{
			// Whitespace must be trimmed before the slash check, so a padded bare
			// name is still caught and the echoed "got" value is the trimmed form.
			name:       "whitespace-padded bare name rejected (trimmed)",
			model:      "   gemini-3.5-flash\t",
			wantReject: true,
			wantSubs:   []string{`got "gemini-3.5-flash"`},
		},
		{
			name: "qualified accepted", model: "google/gemini-3.5-flash", wantReject: false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			pc := &config.ProviderConnection{
				Name: "v", APIType: "vertex",
				Project: "my-proj", Location: "us-central1",
			}
			cfg := &config.ModelConfig{Name: "v", Model: tc.model}
			err := ValidateModelConfig(pc, cfg)
			if tc.wantReject {
				me := requireMisconfig(t, err)
				if me.Type != ErrorGeneric {
					t.Errorf("Type = %v, want ErrorGeneric", me.Type)
				}
				if me.HTTPStatusCode != 0 {
					t.Errorf("HTTPStatusCode = %d, want 0 (config error is not an HTTP status)", me.HTTPStatusCode)
				}
				for _, sub := range tc.wantSubs {
					if !strings.Contains(me.Message, sub) {
						t.Errorf("message missing %q; got: %q", sub, me.Message)
					}
				}
				for _, sub := range tc.wantNotSubs {
					if strings.Contains(me.Message, sub) {
						t.Errorf("message must not contain %q; got: %q", sub, me.Message)
					}
				}
				return
			}
			if err != nil {
				t.Errorf("expected config accepted, got: %v", err)
			}
		})
	}
}

// TestValidateModelConfig_VertexNativeBareRequired documents the chosen native
// rule (criterion 4): a qualified "google/…" model on the native route is
// rejected (it would build a broken publishers/google/models/google/… URL); a
// bare name is accepted. The "gemini" alias resolves to native and the error
// echoes the user's raw api_type token, not the canonical name.
func TestValidateModelConfig_VertexNativeBareRequired(t *testing.T) {
	tests := []struct {
		name       string
		apiType    string
		model      string
		wantReject bool
		wantSubs   []string
	}{
		{name: "qualified on native rejected", apiType: "vertex-native", model: "google/gemini-3.5-flash",
			wantReject: true, wantSubs: []string{"bare model id", `got "google/gemini-3.5-flash"`, `Use api_type "vertex"`}},
		{name: "bare on native accepted", apiType: "vertex-native", model: "gemini-3.5-flash", wantReject: false},
		{name: "bare on gemini alias accepted", apiType: "gemini", model: "gemini-3.5-flash", wantReject: false},
		{
			// The error uses cfg.APIType verbatim, so the alias surfaces as
			// api_type "gemini" (not the canonical "vertex-native").
			name:    "qualified on gemini alias rejected and echoes raw api_type",
			apiType: "gemini", model: "google/gemini-3.5-flash",
			wantReject: true, wantSubs: []string{`api_type "gemini"`, "bare model id"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			pc := &config.ProviderConnection{
				Name: "n", APIType: tc.apiType,
				Project: "my-proj", Location: "us-central1",
			}
			cfg := &config.ModelConfig{Name: "n", Model: tc.model}
			err := ValidateModelConfig(pc, cfg)
			if tc.wantReject {
				me := requireMisconfig(t, err)
				if me.Type != ErrorGeneric {
					t.Errorf("Type = %v, want ErrorGeneric", me.Type)
				}
				for _, sub := range tc.wantSubs {
					if !strings.Contains(me.Message, sub) {
						t.Errorf("message missing %q; got: %q", sub, me.Message)
					}
				}
				return
			}
			if err != nil {
				t.Errorf("expected config accepted, got: %v", err)
			}
		})
	}
}

// TestValidateModelConfig_PublisherRulesScopedToVertexFamily guards criterion 3
// (no regressions): the two new rules fire ONLY for the vertex shim / native
// types. vertex-anthropic (and its "claude-vertex" alias) and every other
// provider are untouched regardless of model shape.
func TestValidateModelConfig_PublisherRulesScopedToVertexFamily(t *testing.T) {
	tests := []struct {
		name    string
		apiType string
		model   string
	}{
		{"vertex-anthropic bare model", "vertex-anthropic", "claude-opus-4-8"},
		{"vertex-anthropic qualified model", "vertex-anthropic", "anthropic/claude-opus-4-8"},
		{"claude-vertex alias bare", "claude-vertex", "claude-opus-4-8"},
		{"openai bare", "openai", "gpt-4o"},
		{"openai qualified (still accepted)", "openai", "openai/gpt-4o"},
		{"zai bare", "zai", "glm-4.6"},
		{"openrouter qualified", "openrouter", "google/gemma-3-27b-it:free"},
		{"anthropic bare", "anthropic", "claude-opus-4-8"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			pc := &config.ProviderConnection{Name: "x", APIType: tc.apiType}
			cfg := &config.ModelConfig{Name: "x", Model: tc.model}
			// Some of these need an endpoint to be routable; supply one so a
			// routability rejection is not mistaken for a publisher-prefix hit.
			if tc.apiType == "openai" {
				pc.Endpoint = "http://127.0.0.1:8080/v1"
			}
			err := ValidateModelConfig(pc, cfg)
			if err != nil {
				t.Errorf("publisher rules must not fire for api_type %q; got: %v", tc.apiType, err)
			}
		})
	}
}

// TestValidateModelConfig_VertexShimPublisherErrorShadowsProjectLocation locks
// the documented error precedence (design §Fix 1): for a bare shim config that
// is ALSO missing project/location, the publisher-prefix error wins over
// "project and location are required". validateRoutableConfig runs before the
// provider's vertexValidate, and it is the only validator at save/load.
func TestValidateModelConfig_VertexShimPublisherErrorShadowsProjectLocation(t *testing.T) {
	pc := &config.ProviderConnection{Name: "v", APIType: "vertex"} // no project/location
	cfg := &config.ModelConfig{Name: "v", Model: "gemini-3.5-flash"}
	me := requireMisconfig(t, configErrOf(t, pc, cfg))
	if !strings.Contains(me.Message, "publisher-qualified") {
		t.Errorf("publisher-prefix error must win for a bare shim config; got: %q", me.Message)
	}
	if strings.Contains(me.Message, "project and location are required") {
		t.Errorf("project/location message must be shadowed by the publisher error; got: %q", me.Message)
	}
}

// ---------------------------------------------------------------------------
// Criterion 2 — normalize at request-build (ensureGooglePublisher + buildRequest)
// ---------------------------------------------------------------------------

func TestEnsureGooglePublisher(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"bare qualified", "gemini-3.5-flash", "google/gemini-3.5-flash"},
		{"already qualified unchanged", "google/gemini-3.5-flash", "google/gemini-3.5-flash"},
		{"other publisher unchanged", "anthropic/claude-opus-4-8", "anthropic/claude-opus-4-8"},
		{"empty stays empty", "", ""},
		// A full resource name already contains "/", so it is left alone — the
		// helper only adds a missing publisher; it never rewrites a present one.
		{"full resource path unchanged", "publishers/google/models/gemini-2.5-flash", "publishers/google/models/gemini-2.5-flash"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := ensureGooglePublisher(tc.in); got != tc.want {
				t.Errorf("ensureGooglePublisher(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestVertexShimProviderHasNormalizer wires the registry expectation: the shim
// provider carries ensureGooglePublisher; native and anthropic do not (their
// model travels in the URL, so a body normalizer would be a dead no-op).
func TestVertexShimProviderHasNormalizer(t *testing.T) {
	if got := providerFor(APITypeVertex).normalizeModelID; got == nil {
		t.Error("shim provider must have a normalizeModelID hook (ensureGooglePublisher)")
	}
	if providerFor(APITypeVertexNative).normalizeModelID != nil {
		t.Error("native provider must NOT have a body normalizer (model is URL-baked)")
	}
	if providerFor(APITypeVertexAnthropic).normalizeModelID != nil {
		t.Error("vertex-anthropic provider must NOT have a body normalizer (model is URL-baked)")
	}
}

// TestVertexShim_BuildRequestAutoQualifiesBareModel exercises the send seam
// directly: buildRequest (called by both the blocking and streaming paths) must
// route the model id through the provider's normalizer. A bare id on the shim is
// qualified; a qualified id is left alone; native and other providers pass the
// id through verbatim. configErr does not gate buildRequest, so this works even
// when validation has flagged the config.
func TestVertexShim_BuildRequestAutoQualifiesBareModel(t *testing.T) {
	mk := func(apiType, model string) *ModelConnection {
		return NewModelConnection(
			&config.ProviderConnection{
				Name: "v", APIType: apiType,
				Project: "my-proj", Location: "us-central1",
			},
			&config.ModelConfig{Name: "v", Model: model},
		)
	}
	t.Run("shim bare qualified at build", func(t *testing.T) {
		conn := mk("vertex", "gemini-3.5-flash")
		req := conn.buildRequest([]Message{{Role: RoleUser, Content: "hi"}}, false, nil, nil)
		if req.Model != "google/gemini-3.5-flash" {
			t.Errorf("Model = %q, want auto-qualified %q", req.Model, "google/gemini-3.5-flash")
		}
	})
	t.Run("shim qualified unchanged at build", func(t *testing.T) {
		conn := mk("vertex", "google/gemini-3.5-flash")
		req := conn.buildRequest([]Message{{Role: RoleUser, Content: "hi"}}, false, nil, nil)
		if req.Model != "google/gemini-3.5-flash" {
			t.Errorf("Model = %q, want unchanged (no double prefix)", req.Model)
		}
	})
	t.Run("native bare NOT rewritten at build (model is in the URL)", func(t *testing.T) {
		conn := mk("vertex-native", "gemini-3.5-flash")
		req := conn.buildRequest([]Message{{Role: RoleUser, Content: "hi"}}, false, nil, nil)
		if req.Model != "gemini-3.5-flash" {
			t.Errorf("native Model = %q, want verbatim bare (native carries the model in the URL, not the body)", req.Model)
		}
	})
	t.Run("openai bare NOT rewritten at build", func(t *testing.T) {
		conn := NewModelConnection(
			&config.ProviderConnection{Name: "o", APIType: "openai", Endpoint: "http://127.0.0.1:8080/v1"},
			&config.ModelConfig{Name: "o", Model: "gpt-4o"},
		)
		req := conn.buildRequest([]Message{{Role: RoleUser, Content: "hi"}}, false, nil, nil)
		if req.Model != "gpt-4o" {
			t.Errorf("openai Model = %q, want verbatim (nil normalizer)", req.Model)
		}
	})
}

// TestVertexShim_NormalizationFiresOnSend is the end-to-end defense-in-depth
// test: a shim connection whose ModelName was set to a bare id AFTER
// construction (simulating a hand-built / library connection that bypassed
// ValidateModelConfig) must still send "google/<model>" on the wire, never bare.
// This is the exact scenario the gate ("auto-prefixed at request-build") names.
func TestVertexShim_NormalizationFiresOnSend(t *testing.T) {
	withFakeADCTokenSource(t, func(ctx context.Context, scopes ...string) (oauth2.TokenSource, error) {
		return &staticTokenSource{token: "vertex-access-token"}, nil
	})

	var got CompletionRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	defer server.Close()

	// Build with a QUALIFIED model so configErr is nil and the completion
	// proceeds; then override ModelName to a bare id, as a caller bypassing
	// validation would. Without the normalizer this would reach Vertex bare.
	conn := NewModelConnection(
		&config.ProviderConnection{APIType: "vertex", Endpoint: server.URL},
		&config.ModelConfig{Model: "google/gemini-3.5-flash"},
	)
	if conn.configErr != nil {
		t.Fatalf("precondition: qualified config must be routable, got configErr: %v", conn.configErr)
	}
	conn.ModelName = "gemini-3.5-flash"

	if _, err := conn.Complete([]Message{{Role: RoleUser, Content: "hi"}}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if got.Model != "google/gemini-3.5-flash" {
		t.Errorf("wire model = %q, a bare name escaped normalization and would 400 opaquely; want %q",
			got.Model, "google/gemini-3.5-flash")
	}
}

// TestVertexShim_QualifiedModelSentUnchangedOnSend is the regression partner:
// an already-qualified model must not be double-prefixed on the wire.
func TestVertexShim_QualifiedModelSentUnchangedOnSend(t *testing.T) {
	withFakeADCTokenSource(t, func(ctx context.Context, scopes ...string) (oauth2.TokenSource, error) {
		return &staticTokenSource{token: "vertex-access-token"}, nil
	})
	var got CompletionRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	defer server.Close()

	conn := NewModelConnection(
		&config.ProviderConnection{APIType: "vertex", Endpoint: server.URL},
		&config.ModelConfig{Model: "google/gemini-3.5-flash"},
	)
	if _, err := conn.Complete([]Message{{Role: RoleUser, Content: "hi"}}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if got.Model != "google/gemini-3.5-flash" {
		t.Errorf("wire model = %q, want unchanged (no double google/google/ prefix)", got.Model)
	}
}

// ---------------------------------------------------------------------------
// Criterion 3 — clearer error mapping (extractProviderMessage array form +
// analyzeError classification)
// ---------------------------------------------------------------------------

func TestExtractProviderMessage_ArrayForm(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{
			name: "vertex publisher 400 single-element array",
			body: vertexPublisher400Body,
			want: vertexPublisher400Message,
		},
		{
			// A first element that carries a message is surfaced directly (the
			// realistic Vertex body is single-element with a message).
			name: "multi-element first message surfaces",
			body: `[{"error":{"message":"first"}},{"error":{"message":"second"}}]`,
			want: "first",
		},
		{
			// Nested arrays recurse until an element yields a message.
			name: "nested array recurses",
			body: `[[{"error":{"message":"deep"}}]]`,
			want: "deep",
		},
		{
			// Whitespace around the array is trimmed before the '[' check.
			name: "whitespace-padded array",
			body: "  \t " + `[{"error":{"message":"padded"}}]` + "  ",
			want: "padded",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := extractProviderMessage(tc.body); got != tc.want {
				t.Errorf("extractProviderMessage = %q\nwant %q", got, tc.want)
			}
		})
	}
}

// TestExtractProviderMessage_ArrayExtractsFirstElement documents the array
// branch's actual behavior: it extracts the FIRST element and returns whatever
// that yields. When the first element has an error.message the clean reason
// surfaces (the #574 case). When the first element is message-less, the
// per-element recursion falls to ITS raw fallback, so the search does NOT advance
// to a later message-bearing element — the raw first element is returned instead.
//
// This is a minor robustness gap versus the design's "first element that yields a
// message" intent, but it is harmless for #574: Vertex's publisher-model 400 is a
// single-element array whose sole element carries the message (see
// TestExtractProviderMessage_ArrayForm / vertex_publisher_400_single-element_array).
// Recorded here so a future hardening of the loop would flip this assertion.
func TestExtractProviderMessage_ArrayExtractsFirstElement(t *testing.T) {
	t.Run("first element message-less returns raw first element, not a later message", func(t *testing.T) {
		body := `[{"error":{"code":400}},{"error":{"message":"never reached"}}]`
		got := extractProviderMessage(body)
		// Documents current behavior: the message-less first element's raw text
		// wins; the second element's message is NOT searched.
		if !strings.Contains(got, `"code":400`) {
			t.Errorf("expected the raw first element as fallback, got %q", got)
		}
		if strings.Contains(got, "never reached") {
			t.Errorf("the array search must not advance past a non-empty first element; got %q", got)
		}
	})
	t.Run("non-object first token also short-circuits (null)", func(t *testing.T) {
		if got := extractProviderMessage(`[null,{"error":{"message":"x"}}]`); got != "null" {
			t.Errorf("first element (null) short-circuits; got %q, want %q", got, "null")
		}
	})
}

// TestExtractProviderMessage_DegenerateArraysFallThrough documents the fallback
// ladder for arrays that carry no usable reason: the extractor does not invent a
// message, it returns the (bounded-by-the-caller) raw text so analyzeError can
// still surface something rather than an empty reason.
func TestExtractProviderMessage_DegenerateArraysFallThrough(t *testing.T) {
	t.Run("empty array returns raw body", func(t *testing.T) {
		if got := extractProviderMessage(`[]`); got != `[]` {
			t.Errorf("empty array = %q, want raw %q (no message to extract)", got, `[]`)
		}
	})
	t.Run("element without error.message returns raw element", func(t *testing.T) {
		got := extractProviderMessage(`[{"foo":"bar"}]`)
		if !strings.Contains(got, "foo") {
			t.Errorf("degenerate element = %q, want the raw element text as fallback", got)
		}
	})
}

// TestExtractProviderMessage_ObjectFormUnchangedRegression guards that the new
// array branch did not disturb the existing object-form extraction.
func TestExtractProviderMessage_ObjectFormUnchangedRegression(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{"openai object form", `{"error":{"message":"obj reason","type":"invalid_request_error"}}`, "obj reason"},
		{"empty body returns empty", "", ""},
		{"bare string error", `{"error":"bare string reason"}`, "bare string reason"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := extractProviderMessage(tc.body); got != tc.want {
				t.Errorf("extractProviderMessage = %q\nwant %q", got, tc.want)
			}
		})
	}
}

// TestAnalyzeError_VertexPublisherArray400 is the headline criterion-3 test: the
// array-form 400 surfaces the clean Vertex reason prominently, classifies as
// ErrorGeneric (NOT context_overflow — the body contains neither "context" nor
// "length"), stays non-retryable at 400, and preserves the full RawResponse.
func TestAnalyzeError_VertexPublisherArray400(t *testing.T) {
	conn := newPlaceholderConnection() // has Stats initialized
	me := conn.analyzeError(400, vertexPublisher400Body)

	if me.Type != ErrorGeneric {
		t.Errorf("Type = %v, want ErrorGeneric (must NOT be misclassified as context_overflow)", me.Type)
	}
	if me.HTTPStatusCode != 400 {
		t.Errorf("HTTPStatusCode = %d, want 400", me.HTTPStatusCode)
	}
	if !strings.Contains(me.Message, "Malformed publisher model") {
		t.Errorf("Message must surface the provider reason; got: %q", me.Message)
	}
	if !strings.Contains(me.Message, "status 400") {
		t.Errorf("Message must carry the status; got: %q", me.Message)
	}
	if me.RawResponse != vertexPublisher400Body {
		t.Errorf("RawResponse must preserve the full body; got: %q", me.RawResponse)
	}
	if isRetryableStatus(400) {
		t.Error("400 must remain non-retryable")
	}
	if conn.Stats.ContextWindowOverflowCount != 0 {
		t.Errorf("a publisher 400 must not bump ContextWindowOverflowCount; got %d", conn.Stats.ContextWindowOverflowCount)
	}
	if conn.Stats.GenericErrorCount == 0 {
		t.Error("a publisher 400 must bump GenericErrorCount")
	}
}

// TestAnalyzeError_400ContextHeuristicUnchanged guards that the pre-existing
// 400 context/length substring heuristic still fires for the object form (a
// regression the array branch could in principle introduce), and that it also
// applies to an array body that genuinely contains "context".
func TestAnalyzeError_400ContextHeuristicUnchanged(t *testing.T) {
	t.Run("object 400 with context -> context_overflow", func(t *testing.T) {
		conn := newPlaceholderConnection()
		me := conn.analyzeError(400, `{"error":{"message":"This model's maximum context length is 8192 tokens."}}`)
		if me.Type != ErrorContextOverflow {
			t.Errorf("Type = %v, want ErrorContextOverflow", me.Type)
		}
	})
	t.Run("array 400 with context -> context_overflow (heuristic scans raw body)", func(t *testing.T) {
		conn := newPlaceholderConnection()
		me := conn.analyzeError(400, `[{"error":{"message":"maximum context length exceeded"}}]`)
		if me.Type != ErrorContextOverflow {
			t.Errorf("Type = %v, want ErrorContextOverflow (heuristic must scan the raw array body)", me.Type)
		}
	})
}

// ---------------------------------------------------------------------------
// Criterion 4 — defaults & sweep: the production default and sample still
// validate; nothing legitimate regressed.
// ---------------------------------------------------------------------------

// TestValidateModelConfig_DefaultVertexSampleQualifies guards the shipped
// default + sample config (api_type "vertex", model "google/gemini-2.5-flash")
// against the new rule — it must continue to validate.
func TestValidateModelConfig_DefaultVertexSampleQualifies(t *testing.T) {
	pc := &config.ProviderConnection{
		Name: "vertex", APIType: "vertex",
		Project: "your-gcp-project", Location: "us-central1",
	}
	cfg := &config.ModelConfig{Name: "gemini-vertex", Connection: "vertex", Model: "google/gemini-2.5-flash"}
	if err := ValidateModelConfig(pc, cfg); err != nil {
		t.Errorf("the shipped default/sample vertex config must still validate; got: %v", err)
	}
}
