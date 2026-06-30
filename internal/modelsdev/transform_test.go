package modelsdev

import (
	"reflect"
	"testing"

	"gogent/internal/config"
	"gogent/internal/model"
)

// sampleProviderModel mirrors the openrouter/claude entry from the issue's
// models.dev example, so the field-mapping tests exercise the real data shape.
func sampleProviderModel() (string, Provider, Model) {
	providerID := "openrouter"
	p := Provider{
		ID:   "openrouter",
		Name: "OpenRouter",
		Env:  []string{"OPENROUTER_API_KEY"},
		API:  "https://openrouter.ai/api/v1",
	}
	m := Model{
		ID:               "anthropic/claude-opus-4-6",
		Name:             "Claude Opus 4.6",
		Reasoning:        true,
		Temperature:      true,
		StructuredOutput: true,
		OpenWeights:      true, // catalog-only: must NOT leak into Caps (no Caps field)
		Knowledge:        "2025-01",
		ReleaseDate:      "2025-02-01",
		Limit:            Limit{Context: 1000000, Output: 128000},
		Cost:             Cost{Input: 5, Output: 25},
		ReasoningOptions: []ReasoningOption{
			{Type: "effort", Values: []string{"low", "medium", "high"}},
		},
	}
	return providerID, p, m
}

func TestProviderAPIType(t *testing.T) {
	cases := []struct {
		name string
		p    Provider
		want string
	}{
		{"openrouter", Provider{ID: "openrouter"}, "openrouter"},
		{"zai", Provider{ID: "zai"}, "zai"},
		{"z-ai alias", Provider{ID: "z-ai"}, "zai"},
		{"z.ai alias", Provider{ID: "z.ai"}, "zai"},
		{"anthropic", Provider{ID: "anthropic"}, "anthropic"},
		{"google-vertex", Provider{ID: "google-vertex"}, "vertex"},
		{"vertex", Provider{ID: "vertex"}, "vertex"},
		{"google-vertex-anthropic", Provider{ID: "google-vertex-anthropic"}, "vertex-anthropic"},
		{"vertex-anthropic", Provider{ID: "vertex-anthropic"}, "vertex-anthropic"},
		// OpenAI-compatible gateways all collapse to the generic openai adapter.
		{"openai", Provider{ID: "openai"}, "openai"},
		{"groq", Provider{ID: "groq"}, "openai"},
		{"deepseek", Provider{ID: "deepseek"}, "openai"},
		{"together", Provider{ID: "together"}, "openai"},
		{"mistral", Provider{ID: "mistral"}, "openai"},
		{"fireworks", Provider{ID: "fireworks"}, "openai"},
		{"empty id defaults to openai", Provider{ID: ""}, "openai"},
		// vertex-native / gemini are intentionally NOT auto-selected (the design
		// notes the user can switch in review); they default to openai.
		{"gemini not auto-selected", Provider{ID: "google-gemini"}, "openai"},
		// Case-insensitivity and surrounding whitespace are tolerated.
		{"mixed case openrouter", Provider{ID: "OpenRouter"}, "openrouter"},
		{"padded anthropic", Provider{ID: " Anthropic "}, "anthropic"},
		// NPM client-library hint rescues an unrecognized id.
		{"npm anthropic fallback", Provider{ID: "weird-id", NPM: "@anthropic-ai/sdk"}, "anthropic"},
		{"npm openrouter fallback", Provider{ID: "weird-id", NPM: "openrouter-sdk"}, "openrouter"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ProviderAPIType(tc.p); got != tc.want {
				t.Errorf("ProviderAPIType(%+v) = %q, want %q", tc.p, got, tc.want)
			}
		})
	}
}

func TestToModelConfigFields(t *testing.T) {
	providerID, p, m := sampleProviderModel()
	conn := ToConnection(p)
	got := ToModelConfig(providerID, conn.Name, m)

	want := config.ModelConfig{
		Name:            "openrouter-anthropic-claude-opus-4-6",
		DisplayName:     "Claude Opus 4.6",
		Connection:      "openrouter",
		Model:           "anthropic/claude-opus-4-6",
		Temperature:     0.7,
		ReasoningEffort: "low",
		Caps: config.ModelCapabilities{
			ContextWindow:    1000000,
			MaxOutput:        128000,
			Reasoning:        true,
			CustomTemp:       true,
			StructuredOutput: true,
			Knowledge:        "2025-01",
			ReleaseDate:      "2025-02-01",
			EffortOptions:    []string{"low", "medium", "high"},
			InputCostPerM:    5, OutputCostPerM: 25,
			Source: "catalog",
			// OpenWeights is intentionally NOT here: by design it has no
			// config.ModelCapabilities field and is surfaced only as a catalog-only
			// display label (CapabilityLabels), so it never reaches the Caps snapshot.
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ToModelConfig =\n %+v\nwant\n %+v", got, want)
	}
}

func TestModelCapabilitiesFreeDerivation(t *testing.T) {
	cases := []struct {
		name string
		cost Cost
		want bool
	}{
		{"both zero is free", Cost{Input: 0, Output: 0}, true},
		{"input only not free", Cost{Input: 1, Output: 0}, false},
		{"output only not free", Cost{Input: 0, Output: 0.5}, false},
		{"both nonzero not free", Cost{Input: 3, Output: 15}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			caps := ToModelCapabilities(Model{ID: "m", Name: "M", Cost: tc.cost})
			if caps.Free() != tc.want {
				t.Errorf("Free = %v, want %v (cost %+v)", caps.Free(), tc.want, tc.cost)
			}
		})
	}
}

func TestToModelConfigNoEffortOptions(t *testing.T) {
	// A model with reasoning but no effort reasoning_options leaves the effort
	// controls empty.
	m := Model{ID: "gpt-x", Name: "GPT X", Reasoning: true}
	got := ToModelConfig("openai", "openai", m)
	if got.ReasoningEffort != "" {
		t.Errorf("ReasoningEffort = %q, want empty", got.ReasoningEffort)
	}
	if got.Caps.EffortOptions != nil {
		t.Errorf("EffortOptions = %v, want nil", got.Caps.EffortOptions)
	}
}

// TestToConnectionEndpointResolverAware is the core goal-match guarantee: a
// connection's Endpoint must be blank for adapters that embed their own
// base/version (anthropic, zai, openrouter, vertex*), and p.API for the generic
// openai adapter whose built-in base is a useless localhost default.
func TestToConnectionEndpointResolverAware(t *testing.T) {
	cases := []struct {
		name      string
		p         Provider
		wantAPI   string
		wantEndpt string
	}{
		{"anthropic", Provider{ID: "anthropic", API: "https://api.anthropic.com/v1"}, "anthropic", ""},
		{"zai", Provider{ID: "zai", API: "https://api.z.ai/api/paas/v4"}, "zai", ""},
		{"openrouter", Provider{ID: "openrouter", API: "https://openrouter.ai/api/v1"}, "openrouter", ""},
		{"vertex", Provider{ID: "google-vertex", API: "ignored"}, "vertex", ""},
		{"vertex-anthropic", Provider{ID: "google-vertex-anthropic", API: "ignored"}, "vertex-anthropic", ""},
		{"groq keeps p.API", Provider{ID: "groq", API: "https://api.groq.com/openai/v1"}, "openai", "https://api.groq.com/openai/v1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ToConnection(tc.p)
			if got.APIType != tc.wantAPI {
				t.Errorf("APIType = %q, want %q", got.APIType, tc.wantAPI)
			}
			if got.Endpoint != tc.wantEndpt {
				t.Errorf("Endpoint = %q, want %q", got.Endpoint, tc.wantEndpt)
			}
		})
	}
}

// TestAnthropicConnectionResolvesCorrectly guards the round-1 design defect:
// feeding models.dev's anthropic base ("https://api.anthropic.com/v1") would
// resolve to a double-/v1 chat URL (404). ToConnection must leave Endpoint blank so
// the adapter's hardcoded base applies, yielding a single /v1.
func TestAnthropicConnectionResolvesCorrectly(t *testing.T) {
	p := Provider{ID: "anthropic", API: "https://api.anthropic.com/v1"}
	m := Model{ID: "claude-opus-4-6", Name: "Claude Opus 4.6"}
	conn := ToConnection(p)
	cfg := ToModelConfig("anthropic", conn.Name, m)
	if conn.Endpoint != "" {
		t.Fatalf("anthropic Endpoint = %q, want blank (adapter owns the base)", conn.Endpoint)
	}
	mc := model.NewModelConnection(&conn, &cfg)
	if got, want := mc.URL, "https://api.anthropic.com/v1/messages"; got != want {
		t.Fatalf("anthropic chat URL = %q, want %q (single /v1 — a double-/v1 means the transform leaked the base)", got, want)
	}
}

// And the counterpart: a generic openai gateway keeps p.API and resolves through
// the openai chatPath.
func TestOpenAIGatewayConnectionResolvesCorrectly(t *testing.T) {
	p := Provider{ID: "groq", API: "https://api.groq.com/openai/v1"}
	m := Model{ID: "llama-3.3-70b", Name: "Llama 3.3 70B"}
	conn := ToConnection(p)
	cfg := ToModelConfig("groq", conn.Name, m)
	if conn.Endpoint != "https://api.groq.com/openai/v1" {
		t.Fatalf("groq Endpoint = %q, want p.API", conn.Endpoint)
	}
	mc := model.NewModelConnection(&conn, &cfg)
	if got, want := mc.URL, "https://api.groq.com/openai/v1/chat/completions"; got != want {
		t.Fatalf("groq chat URL = %q, want %q", got, want)
	}
}

func TestUniqueName(t *testing.T) {
	base := func() string { return sanitizeName("openrouter", "anthropic/claude-opus-4-6") }
	want := "openrouter-anthropic-claude-opus-4-6"
	if got := base(); got != want {
		t.Fatalf("sanitizeName = %q, want %q", got, want)
	}

	// nil taken set: base is returned as-is.
	if got := UniqueName("openrouter", "anthropic/claude-opus-4-6", nil); got != want {
		t.Errorf("UniqueName(nil taken) = %q, want %q", got, want)
	}
	// base taken → -2.
	if got := UniqueName("openrouter", "anthropic/claude-opus-4-6", map[string]bool{want: true}); got != want+"-2" {
		t.Errorf("UniqueName(base taken) = %q, want %q", got, want+"-2")
	}
	// base and -2 taken → -3.
	if got := UniqueName("openrouter", "anthropic/claude-opus-4-6", map[string]bool{want: true, want + "-2": true}); got != want+"-3" {
		t.Errorf("UniqueName(base+-2 taken) = %q, want %q", got, want+"-3")
	}
}

func TestSanitizeName(t *testing.T) {
	cases := []struct {
		providerID, modelID, want string
	}{
		{"openrouter", "anthropic/claude-opus-4-6", "openrouter-anthropic-claude-opus-4-6"},
		{"OpenAI", "GPT-4o", "openai-gpt-4o"}, // lower-cased
		{"p", "m//n", "p-m-n"},                // collapsed separators
		{"a.b", "c_d e", "a-b-c-d-e"},         // mixed punctuation → single dashes
		{"---", "///", ""},                    // all-separators → empty after trim
		{"z.ai", "glm-4.6", "z-ai-glm-4-6"},   // dots become dashes
	}
	for _, tc := range cases {
		t.Run(tc.providerID+"/"+tc.modelID, func(t *testing.T) {
			if got := sanitizeName(tc.providerID, tc.modelID); got != tc.want {
				t.Errorf("sanitizeName(%q,%q) = %q, want %q", tc.providerID, tc.modelID, got, tc.want)
			}
		})
	}
}

func TestHasThinkingToggle(t *testing.T) {
	if !HasThinkingToggle(Model{ReasoningOptions: []ReasoningOption{{Type: "toggle"}}}) {
		t.Error("HasThinkingToggle(toggle) = false, want true")
	}
	if HasThinkingToggle(Model{ReasoningOptions: []ReasoningOption{{Type: "effort", Values: []string{"low"}}}}) {
		t.Error("HasThinkingToggle(effort only) = true, want false")
	}
	if HasThinkingToggle(Model{}) {
		t.Error("HasThinkingToggle(none) = true, want false")
	}
	// Case-insensitive on the option type.
	if !HasThinkingToggle(Model{ReasoningOptions: []ReasoningOption{{Type: "Toggle"}}}) {
		t.Error("HasThinkingToggle(Toggle) = false, want true (case-insensitive)")
	}
}

func TestEffortOptionsCaseInsensitive(t *testing.T) {
	// models.dev uses "effort"; the parser tolerates case/whitespace variation.
	m := Model{ReasoningOptions: []ReasoningOption{{Type: " Effort ", Values: []string{"low", "high"}}}}
	got := ToModelConfig("openai", "openai", m)
	if !reflect.DeepEqual(got.Caps.EffortOptions, []string{"low", "high"}) {
		t.Errorf("EffortOptions = %v, want [low high]", got.Caps.EffortOptions)
	}
	if got.ReasoningEffort != "low" {
		t.Errorf("ReasoningEffort = %q, want low", got.ReasoningEffort)
	}
	// Only the first effort option group is consumed; a second is ignored.
	m2 := Model{ReasoningOptions: []ReasoningOption{
		{Type: "effort", Values: []string{"low", "medium"}},
		{Type: "effort", Values: []string{"should-be-ignored"}},
	}}
	got2 := ToModelConfig("openai", "openai", m2)
	if !reflect.DeepEqual(got2.Caps.EffortOptions, []string{"low", "medium"}) {
		t.Errorf("second effort group leaked: %v", got2.Caps.EffortOptions)
	}
}
