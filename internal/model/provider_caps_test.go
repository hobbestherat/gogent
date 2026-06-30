package model

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"gogent/internal/config"
)

// openRouterModelsBody is a trimmed but realistic OpenRouter GET /api/v1/models
// response: one reasoning/vision model with full pricing (incl. cache read/write)
// and one plain text model.
const openRouterModelsBody = `{
  "data": [
    {
      "id": "anthropic/claude-sonnet-4",
      "name": "Anthropic: Claude Sonnet 4",
      "context_length": 200000,
      "architecture": {"input_modalities": ["text","image"], "output_modalities": ["text"]},
      "pricing": {"prompt": "0.000003", "completion": "0.000015", "input_cache_read": "0.0000003", "input_cache_write": "0.00000375"},
      "top_provider": {"max_completion_tokens": 64000},
      "supported_parameters": ["tools","temperature","response_format","reasoning"],
      "reasoning": {"supported_efforts": ["low","medium","high"]}
    },
    {
      "id": "openai/gpt-4o-mini",
      "name": "OpenAI: GPT-4o-mini",
      "context_length": 128000,
      "architecture": {"input_modalities": ["text"], "output_modalities": ["text"]},
      "pricing": {"prompt": "0.00000015", "completion": "0.0000006"},
      "top_provider": {"max_completion_tokens": 16384},
      "supported_parameters": ["tools","temperature"]
    }
  ]
}`

func TestParseOpenRouterModels(t *testing.T) {
	models, err := parseOpenRouterModels([]byte(openRouterModelsBody))
	if err != nil {
		t.Fatalf("parseOpenRouterModels: %v", err)
	}
	if len(models) != 2 {
		t.Fatalf("got %d models, want 2", len(models))
	}

	m := models[0]
	if m.ID != "anthropic/claude-sonnet-4" {
		t.Errorf("id = %q", m.ID)
	}
	if m.Caps == nil {
		t.Fatal("Caps is nil, want populated")
	}
	c := m.Caps
	if c.ContextWindow != 200000 {
		t.Errorf("ContextWindow = %d, want 200000", c.ContextWindow)
	}
	if c.MaxOutput != 64000 {
		t.Errorf("MaxOutput = %d, want 64000", c.MaxOutput)
	}
	// Per-token strings scaled to per-million.
	if c.InputCostPerM != 3.0 {
		t.Errorf("InputCostPerM = %v, want 3.0", c.InputCostPerM)
	}
	if c.OutputCostPerM != 15.0 {
		t.Errorf("OutputCostPerM = %v, want 15.0", c.OutputCostPerM)
	}
	if c.CacheReadPerM != 0.3 {
		t.Errorf("CacheReadPerM = %v, want 0.3", c.CacheReadPerM)
	}
	if c.CacheWritePerM != 3.75 {
		t.Errorf("CacheWritePerM = %v, want 3.75", c.CacheWritePerM)
	}
	if !c.Vision {
		t.Error("Vision = false, want true (image modality)")
	}
	if !c.ToolCall {
		t.Error("ToolCall = false, want true (tools param)")
	}
	if !c.StructuredOutput {
		t.Error("StructuredOutput = false, want true (response_format)")
	}
	if !c.CustomTemp {
		t.Error("CustomTemp = false, want true (temperature param)")
	}
	if !c.Reasoning {
		t.Error("Reasoning = false, want true")
	}
	if !c.ThinkingToggle {
		t.Error("ThinkingToggle = false, want true (reasoning param supported)")
	}
	if len(c.OutputModalities) != 1 || c.OutputModalities[0] != "text" {
		t.Errorf("OutputModalities = %v, want [text]", c.OutputModalities)
	}
	if len(c.EffortOptions) != 3 || c.EffortOptions[0] != "low" {
		t.Errorf("EffortOptions = %v, want [low medium high]", c.EffortOptions)
	}
	if len(c.InputModalities) != 2 || c.InputModalities[1] != "image" {
		t.Errorf("InputModalities = %v", c.InputModalities)
	}
	if c.Source != SourceLive {
		t.Errorf("Source = %q, want live", c.Source)
	}

	// Plain text model: no vision, no reasoning, no cache pricing.
	c2 := models[1].Caps
	if c2 == nil {
		t.Fatal("second model Caps nil")
	}
	if c2.Vision || c2.Reasoning || len(c2.EffortOptions) != 0 {
		t.Errorf("gpt-4o-mini caps over-asserted: %+v", c2)
	}
	if c2.CacheReadPerM != 0 || c2.CacheWritePerM != 0 {
		t.Errorf("gpt-4o-mini cache pricing should be 0: %+v", c2)
	}
	if c2.InputCostPerM != 0.15 {
		t.Errorf("gpt-4o-mini InputCostPerM = %v, want 0.15", c2.InputCostPerM)
	}
}

// TestEffortListUnmarshal exercises the polymorphic effort decode directly:
// arrays and single strings yield options; bool/number/null/unknown-shape yield no
// options and never error (a wire tweak to this one field must not break listing).
func TestEffortListUnmarshal(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{`["low","high"]`, []string{"low", "high"}},
		{`"medium"`, []string{"medium"}},
		{`true`, nil},
		{`false`, nil},
		{`null`, nil},
		{`42`, nil},
		{`{"x":1}`, nil},
		{`""`, nil},
	}
	for _, tc := range cases {
		var e effortList
		if err := e.UnmarshalJSON([]byte(tc.in)); err != nil {
			t.Errorf("UnmarshalJSON(%s) errored: %v", tc.in, err)
			continue
		}
		if len(e) != len(tc.want) {
			t.Errorf("UnmarshalJSON(%s) = %v, want %v", tc.in, e, tc.want)
			continue
		}
		for i := range tc.want {
			if e[i] != tc.want[i] {
				t.Errorf("UnmarshalJSON(%s) = %v, want %v", tc.in, e, tc.want)
				break
			}
		}
	}
}

// TestParsePerTokenUSD covers the per-token→per-million scaling plus the
// blank/zero/negative/unparseable guards (all → 0, left for the catalog to fill).
func TestParsePerTokenUSD(t *testing.T) {
	cases := []struct {
		in   string
		want float64
	}{
		{"", 0},
		{"0", 0},
		{"-1", 0}, // clamp negative
		{"abc", 0},
		{"0.000003", 3.0},
		{"0.00000015", 0.15},
	}
	for _, tc := range cases {
		if got := parsePerTokenUSD(tc.in); got != tc.want {
			t.Errorf("parsePerTokenUSD(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

// TestParseOpenRouterAlternateParams covers the OR-branches of capability
// detection: the alternate supported_parameters spellings (tool_choice,
// structured_outputs, reasoning_effort) and a reasoning block whose efforts are
// absent (Reasoning true, no enumerated options).
func TestParseOpenRouterAlternateParams(t *testing.T) {
	body := `{"data":[{
		"id":"vendor/alt-model",
		"context_length":64000,
		"architecture":{"input_modalities":["text"],"output_modalities":["text"]},
		"pricing":{"prompt":"0.000001","completion":"0.000002"},
		"top_provider":{"max_completion_tokens":8192},
		"supported_parameters":["tool_choice","structured_outputs","reasoning_effort"]
	}]}`
	models, err := parseOpenRouterModels([]byte(body))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	c := models[0].Caps
	if c == nil {
		t.Fatal("Caps nil")
	}
	if !c.ToolCall {
		t.Error("ToolCall = false, want true (tool_choice param)")
	}
	if !c.StructuredOutput {
		t.Error("StructuredOutput = false, want true (structured_outputs param)")
	}
	if !c.Reasoning || !c.ThinkingToggle {
		t.Errorf("reasoning_effort param should set Reasoning+ThinkingToggle: %+v", c)
	}
	if len(c.EffortOptions) != 0 {
		t.Errorf("no reasoning block → no effort options, got %v", c.EffortOptions)
	}
}

// TestAnthropicPDFOnlyModalities covers the pdf-only modality branch.
func TestAnthropicPDFOnlyModalities(t *testing.T) {
	body := `{"data":[{"id":"claude-pdf","max_input_tokens":100000,"max_tokens":4096,"capabilities":{"pdf_input":true,"image_input":false}}]}`
	models, err := parseAnthropicModels([]byte(body))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	c := models[0].Caps
	if len(c.InputModalities) != 2 || c.InputModalities[0] != "text" || c.InputModalities[1] != "pdf" {
		t.Errorf("InputModalities = %v, want [text pdf]", c.InputModalities)
	}
	if c.Vision {
		t.Error("Vision = true, want false (no image_input)")
	}
}

// anthropicModelsBody is a realistic Anthropic GET /v1/models response: token
// limits + a capabilities block. No pricing (the catalog fills it).
const anthropicModelsBody = `{
  "data": [
    {
      "id": "claude-opus-4-20250514",
      "display_name": "Claude Opus 4",
      "max_input_tokens": 200000,
      "max_tokens": 32000,
      "capabilities": {"effort": ["low","medium","high"], "thinking": true, "image_input": true, "pdf_input": true, "structured_outputs": true}
    }
  ]
}`

func TestParseAnthropicModels(t *testing.T) {
	models, err := parseAnthropicModels([]byte(anthropicModelsBody))
	if err != nil {
		t.Fatalf("parseAnthropicModels: %v", err)
	}
	if len(models) != 1 {
		t.Fatalf("got %d models, want 1", len(models))
	}
	c := models[0].Caps
	if c == nil {
		t.Fatal("Caps nil")
	}
	if c.ContextWindow != 200000 {
		t.Errorf("ContextWindow = %d, want 200000", c.ContextWindow)
	}
	if c.MaxOutput != 32000 {
		t.Errorf("MaxOutput = %d, want 32000", c.MaxOutput)
	}
	if !c.ThinkingToggle {
		t.Error("ThinkingToggle = false, want true")
	}
	if !c.Reasoning {
		t.Error("Reasoning = false, want true")
	}
	if !c.Vision {
		t.Error("Vision = false, want true (image_input)")
	}
	if !c.StructuredOutput {
		t.Error("StructuredOutput = false, want true")
	}
	if !c.ToolCall {
		t.Error("ToolCall = false, want true (every Claude model tool-calls)")
	}
	if !c.CustomTemp {
		t.Error("CustomTemp = false, want true (Claude accepts a custom temperature)")
	}
	if len(c.EffortOptions) != 3 {
		t.Errorf("EffortOptions = %v, want 3", c.EffortOptions)
	}
	if len(c.InputModalities) != 3 { // text, image, pdf
		t.Errorf("InputModalities = %v, want text/image/pdf", c.InputModalities)
	}
	// Anthropic listing has no pricing — left for the catalog.
	if c.InputCostPerM != 0 || c.OutputCostPerM != 0 {
		t.Errorf("Anthropic pricing should be 0, got %+v", c)
	}
	if c.Source != SourceLive {
		t.Errorf("Source = %q, want live", c.Source)
	}
}

// TestParseAnthropicEffortTolerant: the effort field may arrive as a bool (or be
// absent); that must not break parsing — it just yields no enumerated options.
func TestParseAnthropicEffortTolerant(t *testing.T) {
	body := `{"data":[{"id":"claude-x","max_input_tokens":100000,"max_tokens":8192,"capabilities":{"effort":true,"thinking":true,"image_input":false}}]}`
	models, err := parseAnthropicModels([]byte(body))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	c := models[0].Caps
	if c == nil || !c.ThinkingToggle {
		t.Fatalf("caps = %+v, want thinking toggle", c)
	}
	if len(c.EffortOptions) != 0 {
		t.Errorf("effort bool should yield no options, got %v", c.EffortOptions)
	}
	if !c.Reasoning { // thinking implies reasoning even without efforts
		t.Error("Reasoning should be true from thinking flag")
	}
}

// TestOpenRouterListEndToEnd drives the full openAILister path for the openrouter
// provider: an httptest server returns the rich body and ListModels yields caps.
func TestOpenRouterListEndToEnd(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/models" {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(openRouterModelsBody))
	}))
	defer server.Close()

	conn := NewModelConnection(
		&config.ProviderConnection{APIType: "openrouter", Endpoint: server.URL},
		&config.ModelConfig{Model: "anthropic/claude-sonnet-4"},
	)
	models, err := conn.ListModels()
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	if len(models) != 2 || models[0].Caps == nil || models[0].Caps.ContextWindow != 200000 {
		t.Fatalf("rich caps not parsed via lister: %+v", models)
	}
}

// TestAnthropicListEndToEnd drives the full lister path for the anthropic provider.
func TestAnthropicListEndToEnd(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(anthropicModelsBody))
	}))
	defer server.Close()

	conn := NewModelConnection(
		&config.ProviderConnection{APIType: "anthropic", Endpoint: server.URL},
		&config.ModelConfig{Model: "claude-opus-4-20250514"},
	)
	models, err := conn.ListModels()
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	if len(models) != 1 || models[0].Caps == nil || !models[0].Caps.ThinkingToggle {
		t.Fatalf("anthropic caps not parsed via lister: %+v", models)
	}
}

// TestGenericListerLeavesCapsNil confirms the id-only providers (generic OpenAI,
// Z.AI) do not populate Caps — discovery fills those from the catalog.
func TestGenericListerLeavesCapsNil(t *testing.T) {
	for _, apiType := range []string{"openai", "zai"} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"some-model","owned_by":"x"}]}`))
		}))
		conn := NewModelConnection(
			&config.ProviderConnection{APIType: apiType, Endpoint: server.URL},
			&config.ModelConfig{Model: "some-model"},
		)
		models, err := conn.ListModels()
		server.Close()
		if err != nil {
			t.Fatalf("[%s] ListModels: %v", apiType, err)
		}
		if len(models) != 1 || models[0].Caps != nil {
			t.Errorf("[%s] expected Caps nil for id-only lister, got %+v", apiType, models[0].Caps)
		}
	}
}

// TestRichParseFallsBackToGeneric: a body the rich parser cannot use (no "data"
// array) must fall through to the generic {models:[...]} path rather than error.
func TestRichParseFallsBackToGeneric(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"models":[{"id":"router-only-model"}]}`))
	}))
	defer server.Close()

	conn := NewModelConnection(
		&config.ProviderConnection{APIType: "openrouter", Endpoint: server.URL},
		&config.ModelConfig{Model: "router-only-model"},
	)
	models, err := conn.ListModels()
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	if len(models) != 1 || models[0].ID != "router-only-model" {
		t.Fatalf("generic fallback failed: %+v", models)
	}
	if models[0].Caps != nil {
		t.Errorf("generic fallback should leave Caps nil, got %+v", models[0].Caps)
	}
}

// TestDiscoveryPrefersLiveCapsOverCatalog wires parsed live caps through
// MergeDiscovery against a catalog with conflicting numbers and confirms the live
// values win while the catalog fills what the live listing omitted (pricing for
// Anthropic).
func TestDiscoveryPrefersLiveCapsOverCatalog(t *testing.T) {
	live, err := parseAnthropicModels([]byte(anthropicModelsBody))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	cat := stubLookup{
		exact: map[string]config.ModelCapabilities{
			"claude-opus-4-20250514": {
				ContextWindow:  1000000, // should LOSE to live 200000
				MaxOutput:      8192,    // should LOSE to live 32000
				InputCostPerM:  15,      // should FILL (live has none)
				OutputCostPerM: 75,
				Source:         SourceCatalog,
			},
		},
	}
	got := MergeDiscovery(APITypeAnthropic, live, cat)
	if len(got) != 1 {
		t.Fatalf("got %d, want 1", len(got))
	}
	d := got[0]
	if d.Caps.ContextWindow != 200000 || d.Caps.MaxOutput != 32000 {
		t.Errorf("live limits should win: %+v", d.Caps)
	}
	if d.Caps.InputCostPerM != 15 || d.Caps.OutputCostPerM != 75 {
		t.Errorf("catalog pricing should fill: %+v", d.Caps)
	}
	if !d.Caps.ThinkingToggle || !d.Caps.Vision {
		t.Errorf("live flags lost: %+v", d.Caps)
	}
	if d.Caps.Source != SourceMerged {
		t.Errorf("Source = %q, want merged", d.Caps.Source)
	}
	if !d.Available || !d.InCatalog {
		t.Errorf("availability flags wrong: %+v", d)
	}
}
