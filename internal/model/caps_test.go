package model

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"gogent/internal/config"
)

// These tests pin the per-(provider,model) capability layer added for issue #543
// (see caps.go / model_overrides.go and design.md). They cover the four design
// gates: GOAL MATCH (the Opus 4.8 fix + the data-not-branch seam), USABILITY
// (current-gen Claude works out of the box; quirks are data rows), NO REGRESSIONS
// (empty resolution is byte-identical to today; old Claude and non-Claude models
// unaffected), and HOLISTIC (gogent-only wire concern). They are deliberately
// written to FAIL if the fix is reverted, if the override table loses a row, or
// if the adapter re-acquires a sampling branch.

// ---------------------------------------------------------------------------
// resolveModelCaps — tiered resolution (DoD unit test)
// ---------------------------------------------------------------------------

func TestResolveModelCapsTiering(t *testing.T) {
	currentGenClaude := []string{
		"claude-opus-4-8", "claude-opus-4-5", "claude-opus-4-1",
		"claude-sonnet-4-5", "claude-haiku-4-5",
	}

	tests := []struct {
		name        string
		apiType     APIType
		model       string
		wantRejects bool // ModelCaps.RejectsSampling
		wantNonZero bool // expect a non-empty ModelCaps overall
		note        string
	}{
		// --- Model-only tier: applies across EVERY provider (issue #543 design) ---
		{
			name:    "direct anthropic opus-4-8 (the bug)",
			apiType: APITypeAnthropic, model: "claude-opus-4-8",
			wantRejects: true, wantNonZero: true,
			note: "the named fix; model-only row fires on the direct path",
		},
		{
			name:    "openrouter bare opus-4-8 inherits model-only row",
			apiType: APITypeOpenRouter, model: "claude-opus-4-8",
			wantRejects: true, wantNonZero: true,
			note: "model-only tier has cross-provider blast radius (documented)",
		},
		{
			name:    "zai bare opus-4-8 inherits model-only row",
			apiType: APITypeZAI, model: "claude-opus-4-8",
			wantRejects: true, wantNonZero: true,
		},
		{
			name:    "openrouter prefixed id NOT matched (exact model id)",
			apiType: APITypeOpenRouter, model: "anthropic/claude-opus-4-8",
			wantRejects: false, wantNonZero: false,
			note: "OpenRouter's prefixed form is not covered by step-1 exact rows",
		},

		// --- Provider-wildcard tier: any model on vertex-anthropic drops sampling ---
		{
			name:    "vertex opus-4-8 (provider-wildcard, agrees with model-only)",
			apiType: APITypeVertexAnthropic, model: "claude-opus-4-8",
			wantRejects: true, wantNonZero: true,
		},
		{
			name:    "vertex arbitrary current-gen claude",
			apiType: APITypeVertexAnthropic, model: "claude-sonnet-4-5",
			wantRejects: true, wantNonZero: true,
		},
		{
			name:    "vertex OLD claude still drops via provider-wildcard",
			apiType: APITypeVertexAnthropic, model: "claude-3-5-sonnet",
			wantRejects: true, wantNonZero: true,
			note: "reproduces the former blanket `if a.vertex` drop for every Vertex Claude",
		},
		{
			name:    "vertex arbitrary non-claude model still drops via wildcard",
			apiType: APITypeVertexAnthropic, model: "anything-at-all",
			wantRejects: true, wantNonZero: true,
		},

		// --- Empty (inherit-everything) tier: the byte-identity invariant ---
		{
			name:    "direct anthropic OLD claude -> empty (keeps temperature)",
			apiType: APITypeAnthropic, model: "claude-3-5-sonnet",
			wantRejects: false, wantNonZero: false,
			note: "old Claude accepts temperature; no row => byte-identical to today",
		},
		{
			name:    "openai gpt-4o -> empty",
			apiType: APITypeOpenAI, model: "gpt-4o",
			wantRejects: false, wantNonZero: false,
		},
		{
			name:    "zai glm-5.2 -> empty",
			apiType: APITypeZAI, model: "glm-5.2",
			wantRejects: false, wantNonZero: false,
		},
		{
			name:    "empty model -> empty",
			apiType: APITypeAnthropic, model: "",
			wantRejects: false, wantNonZero: false,
		},
		{
			name:    "unknown api_type with NON-overridden model -> empty",
			apiType: APIType("no-such-provider"), model: "gpt-4o",
			wantRejects: false, wantNonZero: false,
		},
		{
			name:    "unknown api_type with overridden model still matches (model-only is global)",
			apiType: APIType("no-such-provider"), model: "claude-opus-4-8",
			wantRejects: true, wantNonZero: true,
			note: "the model-only tier (empty provider) matches ANY apiType, recognized or not",
		},
		{
			name:    "unknown provider that happens to be 'anthropic'-adjacent still empty for non-current",
			apiType: APITypeVertex, model: "gemini-2.5-pro",
			wantRejects: false, wantNonZero: false,
		},

		// --- Snapshot normalization: pinned id inherits its family row ---
		{
			name:    "pinned snapshot opus-4-5@20251101 on direct -> rejects (base normalized)",
			apiType: APITypeAnthropic, model: "claude-opus-4-5@20251101",
			wantRejects: true, wantNonZero: true,
		},
		{
			name:    "pinned snapshot opus-4-5@20251101 on vertex -> rejects",
			apiType: APITypeVertexAnthropic, model: "claude-opus-4-5@20251101",
			wantRejects: true, wantNonZero: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := resolveModelCaps(tt.apiType, tt.model)
			if got.RejectsSampling != tt.wantRejects {
				t.Errorf("resolveModelCaps(%q,%q).RejectsSampling = %v, want %v",
					tt.apiType, tt.model, got.RejectsSampling, tt.wantRejects)
			}
			nonZero := got != ModelCaps{}
			if nonZero != tt.wantNonZero {
				t.Errorf("resolveModelCaps(%q,%q) = %+v, want zero=%v",
					tt.apiType, tt.model, got, !tt.wantNonZero)
			}
		})
	}

	// Guard: every current-gen Claude id named in the override table resolves to
	// RejectsSampling on the DIRECT path (the bug path). If a row is removed this
	// fails loudly.
	for _, m := range currentGenClaude {
		if got := resolveModelCaps(APITypeAnthropic, m); !got.RejectsSampling {
			t.Errorf("resolveModelCaps(anthropic,%q) = %+v, want RejectsSampling true (override row missing?)", m, got)
		}
	}
}

// TestResolveModelCapsCatalogTierNotWired documents the step-1 boundary: the
// models.dev catalog default tier is NOT active yet, so any model absent from the
// override table resolves to EMPTY regardless of what a catalog might say. This
// prevents a future catalog import from silently changing behavior, and pins the
// "override is authoritative; catalog is a separate additive step" design.
func TestResolveModelCapsCatalogTierNotWired(t *testing.T) {
	for _, tt := range []struct {
		apiType APIType
		model   string
	}{
		{APITypeAnthropic, "claude-3-5-sonnet"}, // old claude — catalog may carry temperature:true, but no override => empty
		{APITypeOpenAI, "gpt-4o"},
		{APITypeZAI, "glm-4.6"},
		{APITypeAnthropic, "some-brand-new-claude-5"}, // not yet curated
	} {
		if got := resolveModelCaps(tt.apiType, tt.model); got != (ModelCaps{}) {
			t.Errorf("resolveModelCaps(%q,%q) = %+v, want empty ModelCaps (catalog tier not wired in step 1)",
				tt.apiType, tt.model, got)
		}
	}
}

// ---------------------------------------------------------------------------
// baseModelID — snapshot normalization edge cases
// ---------------------------------------------------------------------------

func TestBaseModelID(t *testing.T) {
	tests := []struct{ in, want string }{
		{"claude-opus-4-8", "claude-opus-4-8"},
		{"claude-opus-4-5@20251101", "claude-opus-4-5"},
		{"  claude-opus-4-8  ", "claude-opus-4-8"},            // surrounding whitespace trimmed
		{"claude-opus-4-5@20251101@extra", "claude-opus-4-5"}, // first '@' wins
		{"", ""},
		{"   ", ""},
		{"@", ""},
		{"no-at-sign", "no-at-sign"},
	}
	for _, tt := range tests {
		if got := baseModelID(tt.in); got != tt.want {
			t.Errorf("baseModelID(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

// ---------------------------------------------------------------------------
// buildRequest — the DoD regression test (the fix), at the resolved-request level
// ---------------------------------------------------------------------------

// TestBuildRequestDropsSamplingForCurrentGenClaudeDirect is the core regression
// test: a direct-Anthropic config for a current-gen Claude with temperature/top_p
// set must resolve to a request carrying NEITHER. This is the mirror of the
// vertex_anthropic_test.go sampling assertion, lifted to the resolved-request
// level where the decision now lives (DoD). It also guards against OVER-dropping:
// max_tokens and the model id must still be set.
func TestBuildRequestDropsSamplingForCurrentGenClaudeDirect(t *testing.T) {
	currentGen := []string{
		"claude-opus-4-8", "claude-opus-4-5", "claude-opus-4-1",
		"claude-sonnet-4-5", "claude-haiku-4-5",
	}
	for _, model := range currentGen {
		t.Run(model, func(t *testing.T) {
			req := buildRequestFor(&config.ModelConfig{
				APIType:     "anthropic",
				Model:       model,
				Temperature: 0.7,
				TopP:        0.9,
				MaxTokens:   1234,
			})
			if req.Temperature != nil {
				t.Errorf("Temperature = %v, want nil (current-gen Claude rejects sampling on direct Anthropic)", *req.Temperature)
			}
			if req.TopP != nil {
				t.Errorf("TopP = %v, want nil", *req.TopP)
			}
			// Guard against over-dropping: the output cap and model id are unrelated
			// to sampling and must still be present.
			if req.MaxTokens == nil || *req.MaxTokens != 1234 {
				t.Errorf("MaxTokens = %v, want 1234 (must not be collateral damage)", req.MaxTokens)
			}
			if req.MaxCompletionTokens != nil {
				t.Errorf("MaxCompletionTokens = %v, want nil (direct Anthropic is not max_completion_tokens)", req.MaxCompletionTokens)
			}
			if req.Model != model {
				t.Errorf("Model = %q, want %q", req.Model, model)
			}
		})
	}
}

// TestBuildRequestDirectAndVertexIdenticalForCurrentGenClaude pins the DoD
// invariant "Vertex-Anthropic and direct-Anthropic behave IDENTICALLY for the same
// [current-gen] model": both paths resolve to no sampling params via the same data.
func TestBuildRequestDirectAndVertexIdenticalForCurrentGenClaude(t *testing.T) {
	cases := []struct {
		name  string
		api   string
		extra func(c *config.ModelConfig)
	}{
		{"direct", "anthropic", func(c *config.ModelConfig) {}},
		{"vertex", "vertex-anthropic", func(c *config.ModelConfig) {
			c.Project, c.Location = "p", "us-central1"
		}},
	}
	for _, model := range []string{"claude-opus-4-8", "claude-sonnet-4-5"} {
		for _, tc := range cases {
			t.Run(tc.name+"_"+model, func(t *testing.T) {
				cfg := &config.ModelConfig{
					APIType: tc.api, Model: model,
					Temperature: 0.7, TopP: 0.9, MaxTokens: 500,
				}
				tc.extra(cfg)
				req := buildRequestFor(cfg)
				if req.Temperature != nil {
					t.Errorf("%s/%s: Temperature = %v, want nil", tc.name, model, *req.Temperature)
				}
				if req.TopP != nil {
					t.Errorf("%s/%s: TopP = %v, want nil", tc.name, model, *req.TopP)
				}
			})
		}
	}
}

// TestBuildRequestKeepsSamplingForUnaffectedModels is the NO-REGRESSION guard: with
// no matching override, resolution is empty and the sampling gate must be
// byte-identical to before this layer existed. Critically this includes OLD Claude
// on the direct path (which still accepts temperature) and the OpenAI reasoning
// path (which drops via the pre-existing ReasoningRejectsTemperature flag, NOT via
// an override). These cases had ZERO coverage before this change.
func TestBuildRequestKeepsSamplingForUnaffectedModels(t *testing.T) {
	t.Run("gpt-4o keeps temperature and top_p", func(t *testing.T) {
		req := buildRequestFor(&config.ModelConfig{Model: "gpt-4o", Temperature: 0.7, TopP: 0.9, MaxTokens: 4096})
		if req.Temperature == nil || *req.Temperature != 0.7 {
			t.Errorf("Temperature = %v, want 0.7", req.Temperature)
		}
		if req.TopP == nil || *req.TopP != 0.9 {
			t.Errorf("TopP = %v, want 0.9", req.TopP)
		}
	})

	t.Run("old claude on DIRECT anthropic keeps temperature (no override row)", func(t *testing.T) {
		req := buildRequestFor(&config.ModelConfig{APIType: "anthropic", Model: "claude-3-5-sonnet", Temperature: 0.5, MaxTokens: 1024})
		if req.Temperature == nil || *req.Temperature != 0.5 {
			t.Errorf("Temperature = %v, want 0.5 (old Claude accepts temperature; must be byte-identical to today)", req.Temperature)
		}
	})

	t.Run("deliberate temperature 0 survives on a model that accepts it", func(t *testing.T) {
		req := buildRequestFor(&config.ModelConfig{Model: "gpt-4o", Temperature: 0, MaxTokens: 4096})
		if req.Temperature == nil || *req.Temperature != 0 {
			t.Errorf("Temperature = %v, want 0 (pointer must preserve a deliberate zero)", req.Temperature)
		}
	})

	t.Run("openai reasoning o3 drops temperature via the pre-existing path (unchanged)", func(t *testing.T) {
		req := buildRequestFor(&config.ModelConfig{Model: "o3", Temperature: 0.7, ReasoningEffort: "high", MaxTokens: 8000})
		if req.Temperature != nil {
			t.Errorf("Temperature = %v, want nil (OpenAI reasoning rejects temperature; path must be unchanged)", *req.Temperature)
		}
		if req.MaxCompletionTokens == nil {
			t.Error("MaxCompletionTokens = nil, want set (reasoning encoding must be unchanged)")
		}
	})

	t.Run("zai reasoning keeps temperature (glm reasoning does not reject it)", func(t *testing.T) {
		req := buildRequestFor(&config.ModelConfig{APIType: "zai", Model: "glm-5.2", Temperature: 0.6, ReasoningEffort: "max", MaxTokens: 4096})
		if req.Temperature == nil || *req.Temperature != 0.6 {
			t.Errorf("Temperature = %v, want 0.6", req.Temperature)
		}
	})
}

// TestBuildRequestCurrentGenClaudeWithReasoningStillDropsSampling ensures the fix
// holds even when the user ALSO opts into reasoning (reasoning_effort/thinking): the
// (provider,model) override wins, so temperature is dropped regardless. This guards
// the case the task brief mis-stated ("IsReasoningModel true") — it confirms the
// override, not the reasoning flag, is what fixes the bug.
func TestBuildRequestCurrentGenClaudeWithReasoningStillDropsSampling(t *testing.T) {
	req := buildRequestFor(&config.ModelConfig{
		APIType: "anthropic", Model: "claude-opus-4-8",
		Temperature: 0.7, ReasoningEffort: "high", MaxTokens: 4096,
	})
	if req.Temperature != nil {
		t.Errorf("Temperature = %v, want nil (override drops sampling even with reasoning opted in)", *req.Temperature)
	}
	// Direct Anthropic has no reasoning_effort/thinking capability flags, so neither
	// is emitted; only the override effect (sampling drop) should show.
	if req.ReasoningEffort != "" {
		t.Errorf("ReasoningEffort = %q, want empty (direct Anthropic does not emit it)", req.ReasoningEffort)
	}
}

// ---------------------------------------------------------------------------
// Full-path reproduction: the actual bug, end-to-end over a real HTTP exchange
// ---------------------------------------------------------------------------

// TestDirectAnthropicCurrentGenClaudeEndToEndNoSamplingOnWire is the strongest
// guard: spin up a direct-Anthropic server, send a claude-opus-4-8 request with
// temperature/top_p set, and assert the WIRE BODY the server receives carries
// neither field. Without the fix this body contained temperature and would 400
// against real Anthropic. Mirrors TestVertexAnthropicEndToEndADCAndWire for the
// direct path.
func TestDirectAnthropicCurrentGenClaudeEndToEndNoSamplingOnWire(t *testing.T) {
	var rawBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rawBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":3,"output_tokens":1}}`))
	}))
	defer server.Close()

	conn := NewModelConnectionFromConfig(&config.ModelConfig{
		APIType:     "anthropic",
		Endpoint:    server.URL,
		Model:       "claude-opus-4-8",
		APIKey:      "k",
		Temperature: 0.7, // the field whose mere presence used to 400
		TopP:        0.9,
		MaxTokens:   321,
	})
	if _, err := conn.Complete([]Message{{Role: RoleUser, Content: "hi"}}); err != nil {
		t.Fatalf("Complete: %v", err)
	}

	body := decodeAnthropicBody(t, rawBody)
	if _, ok := body["temperature"]; ok {
		t.Errorf("wire body has temperature; current-gen Claude over direct Anthropic must omit it: %s", rawBody)
	}
	if _, ok := body["top_p"]; ok {
		t.Errorf("wire body has top_p; current-gen Claude over direct Anthropic must omit it: %s", rawBody)
	}
	// Don't over-drop: model + max_tokens are unrelated and must still be present.
	if body["model"] != "claude-opus-4-8" {
		t.Errorf("wire model = %v, want claude-opus-4-8", body["model"])
	}
	if got, ok := body["max_tokens"].(float64); !ok || got != 321 {
		t.Errorf("wire max_tokens = %v, want 321", body["max_tokens"])
	}
}

// TestPinnedSnapshotDropsSamplingButPreservesWireModel checks the snapshot-id
// normalization does not leak into the wire: a pinned id "claude-opus-4-5@20251101"
// drops sampling (base matches the family row) BUT the wire `model` field keeps the
// pinned id verbatim (the dated snapshot must reach the provider unchanged).
func TestPinnedSnapshotDropsSamplingButPreservesWireModel(t *testing.T) {
	const pinned = "claude-opus-4-5@20251101"

	// Request-level: sampling dropped because base normalizes to the family row.
	req := buildRequestFor(&config.ModelConfig{
		APIType: "anthropic", Model: pinned, Temperature: 0.7, TopP: 0.9, MaxTokens: 100,
	})
	if req.Temperature != nil || req.TopP != nil {
		t.Errorf("pinned snapshot sampling not dropped: Temperature=%v TopP=%v", req.Temperature, req.TopP)
	}

	// Wire-level: the pinned id is preserved verbatim on the body.
	var rawBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rawBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer server.Close()

	conn := NewModelConnectionFromConfig(&config.ModelConfig{
		APIType: "anthropic", Endpoint: server.URL, Model: pinned, APIKey: "k", Temperature: 0.7, MaxTokens: 50,
	})
	if _, err := conn.Complete([]Message{{Role: RoleUser, Content: "hi"}}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	body := decodeAnthropicBody(t, rawBody)
	if body["model"] != pinned {
		t.Errorf("wire model = %v, want pinned id %q verbatim (normalization must be lookup-only)", body["model"], pinned)
	}
	if _, ok := body["temperature"]; ok {
		t.Errorf("wire body has temperature; pinned snapshot must still drop sampling")
	}
}

// ---------------------------------------------------------------------------
// adapter contract: buildBody is now a pure forwarder on BOTH paths
// ---------------------------------------------------------------------------

// TestAnthropicAdapterBuildBodyForwardsSamplingOnBothPaths pins the new adapter
// contract introduced by issue #543: buildBody forwards whatever sampling pointers
// it is handed on the direct AND the Vertex path, and omits them when nil. The
// "should this model accept sampling?" decision no longer lives in the adapter; it
// lives in buildRequest via resolveModelCaps. This test fails if anyone re-introduces
// a `if a.vertex { /* drop */ }` sampling branch.
func TestAnthropicAdapterBuildBodyForwardsSamplingOnBothPaths(t *testing.T) {
	temp := float32(0.7)

	for _, vertex := range []bool{false, true} {
		label := "direct"
		if vertex {
			label = "vertex"
		}
		t.Run(label+"_forwards_non_nil", func(t *testing.T) {
			req := CompletionRequest{
				Model: "claude-opus-4-8", MaxTokens: intp(50),
				Temperature: &temp, TopP: &temp,
				Messages: []Message{{Role: RoleUser, Content: "hi"}},
			}
			body, _ := buildBodyBytes(anthropicAdapter{vertex: vertex}, req)
			m := decodeAnthropicBody(t, body)
			if m["temperature"] == nil {
				t.Errorf("body missing temperature; adapter must forward what it is handed: %s", body)
			}
			if m["top_p"] == nil {
				t.Errorf("body missing top_p; adapter must forward what it is handed: %s", body)
			}
		})
		t.Run(label+"_omits_nil", func(t *testing.T) {
			// nil pointers (the resolved request for a sampling-rejecting model) are
			// dropped by omitempty — this is how the buildRequest decision reaches the
			// wire without an adapter branch.
			req := CompletionRequest{
				Model: "claude-opus-4-8", MaxTokens: intp(50), // Temperature/TopP intentionally nil
				Messages: []Message{{Role: RoleUser, Content: "hi"}},
			}
			body, _ := buildBodyBytes(anthropicAdapter{vertex: vertex}, req)
			m := decodeAnthropicBody(t, body)
			if _, ok := m["temperature"]; ok {
				t.Errorf("body has temperature; nil pointer must be omitted: %s", body)
			}
			if _, ok := m["top_p"]; ok {
				t.Errorf("body has top_p; nil pointer must be omitted: %s", body)
			}
		})
	}
}

// TestAnthropicAdapterVertexBodyShapeUnchangedElsewhere guards that removing the
// Vertex sampling branch did not disturb the other Vertex body-shape invariants
// (model omitted, anthropic_version in body, cache breakpoints). These remain the
// adapter's responsibility.
func TestAnthropicAdapterVertexBodyShapeUnchangedElsewhere(t *testing.T) {
	temp := float32(0.7)
	var buf bytes.Buffer
	if err := (anthropicAdapter{vertex: true}).buildBody(CompletionRequest{
		Model: "claude-opus-4-8", MaxTokens: intp(200), Temperature: &temp,
		Messages: []Message{
			{Role: RoleSystem, Content: "sys"},
			{Role: RoleUser, Content: "hi"},
		},
	}, &buf); err != nil {
		t.Fatalf("buildBody: %v", err)
	}
	body := decodeAnthropicBody(t, buf.Bytes())
	if _, ok := body["model"]; ok {
		t.Error("vertex body has model; Vertex carries the model in the URL path, not the body")
	}
	if body["anthropic_version"] != vertexAnthropicVersion {
		t.Errorf("anthropic_version = %v, want %q", body["anthropic_version"], vertexAnthropicVersion)
	}
	// Cache breakpoint still rides the system block.
	sys, ok := body["system"].([]any)
	if !ok || len(sys) != 1 {
		t.Fatalf("system = %v, want one-element block array", body["system"])
	}
	if cc, ok := sys[0].(map[string]any)["cache_control"].(map[string]any); !ok || cc["type"] != "ephemeral" {
		t.Errorf("system cache_control = %v, want ephemeral", sys[0].(map[string]any)["cache_control"])
	}
}
