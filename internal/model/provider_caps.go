package model

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"

	"gogent/internal/config"
)

// Live capability self-description for the rich listers. Some backends return
// far more than an id from their /models endpoint — OpenRouter describes context,
// pricing (incl. cache read/write), modalities, supported params and reasoning
// efforts; Anthropic describes token limits and capability flags. Parsing those
// into ModelInfo.Caps lets discovery prefer the LIVE values (which reflect what
// THIS account/model actually does) over the models.dev catalog, while the catalog
// still fills whatever the listing omits (e.g. Anthropic carries no pricing).
//
// This is wired as an optional capsParser strategy on openAILister, keyed by
// provider: the rich providers (openrouter, anthropic) supply a parser; the
// id-only ones (generic openai, Z.AI) leave it nil so their Caps stays nil and
// the catalog fills every field, exactly as before. The parser is a pure
// body→[]ModelInfo function so it is trivially unit-testable and additive — the
// generic {data}/{models}/bare-array path in list() is untouched.

// capsParser turns a rich /models response body into model entries carrying
// self-described capabilities (ModelInfo.Caps). It returns an error on a body it
// cannot parse so list() can fall back to the generic id-only path.
type capsParser func(body []byte) ([]ModelInfo, error)

// effortList tolerantly decodes a "supported efforts" / "effort" field that a
// backend may express as a JSON array of strings, a single string, or a bare
// boolean (capability present but no enumerated options). It never fails the whole
// parse on a shape it does not recognize — it simply yields no options — so a
// future wire tweak to this one field cannot break model listing.
type effortList []string

func (e *effortList) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	if len(b) == 0 || string(b) == "null" {
		return nil
	}
	switch b[0] {
	case '[':
		var s []string
		if err := json.Unmarshal(b, &s); err != nil {
			return nil // unknown array shape: no options, don't fail the listing
		}
		*e = s
	case '"':
		var s string
		if err := json.Unmarshal(b, &s); err == nil && s != "" {
			*e = []string{s}
		}
	default:
		// boolean or number: capability flag only, no enumerated options.
	}
	return nil
}

// parsePerTokenUSD parses an OpenRouter pricing string (USD per single token,
// e.g. "0.000003") into a per-MILLION-token price. Empty/blank/unparseable values
// yield 0 (left for the catalog to fill); negative values are clamped to 0.
func parsePerTokenUSD(s string) float64 {
	if s == "" {
		return 0
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil || v <= 0 {
		return 0
	}
	return v * 1_000_000
}

func sliceContains(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// OpenRouter — GET /api/v1/models (the richest self-describer)
// ---------------------------------------------------------------------------

type openRouterModel struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	ContextLen   int    `json:"context_length"`
	Architecture struct {
		InputModalities  []string `json:"input_modalities"`
		OutputModalities []string `json:"output_modalities"`
	} `json:"architecture"`
	Pricing struct {
		Prompt          string `json:"prompt"`
		Completion      string `json:"completion"`
		InputCacheRead  string `json:"input_cache_read"`
		InputCacheWrite string `json:"input_cache_write"`
	} `json:"pricing"`
	TopProvider struct {
		MaxCompletionTokens int `json:"max_completion_tokens"`
	} `json:"top_provider"`
	SupportedParameters []string `json:"supported_parameters"`
	Reasoning           *struct {
		SupportedEfforts effortList `json:"supported_efforts"`
	} `json:"reasoning"`
}

// parseOpenRouterModels parses OpenRouter's rich /models listing into per-model
// capability snapshots (Source="live"). Pricing strings are USD-per-token and are
// scaled to per-million; vision/tool/structured/reasoning flags are read from
// modalities, supported_parameters and the reasoning block.
func parseOpenRouterModels(body []byte) ([]ModelInfo, error) {
	var wrapped struct {
		Data []openRouterModel `json:"data"`
	}
	if err := json.Unmarshal(body, &wrapped); err != nil {
		return nil, fmt.Errorf("parse openrouter models: %w", err)
	}
	if len(wrapped.Data) == 0 {
		return nil, fmt.Errorf("parse openrouter models: no entries")
	}
	out := make([]ModelInfo, 0, len(wrapped.Data))
	for _, m := range wrapped.Data {
		if m.ID == "" {
			continue
		}
		caps := config.ModelCapabilities{
			ContextWindow:    m.ContextLen,
			MaxOutput:        m.TopProvider.MaxCompletionTokens,
			InputModalities:  m.Architecture.InputModalities,
			OutputModalities: m.Architecture.OutputModalities,
			Vision:           sliceContains(m.Architecture.InputModalities, "image"),
			ToolCall:         sliceContains(m.SupportedParameters, "tools") || sliceContains(m.SupportedParameters, "tool_choice"),
			StructuredOutput: sliceContains(m.SupportedParameters, "response_format") || sliceContains(m.SupportedParameters, "structured_outputs"),
			CustomTemp:       sliceContains(m.SupportedParameters, "temperature"),
			InputCostPerM:    parsePerTokenUSD(m.Pricing.Prompt),
			OutputCostPerM:   parsePerTokenUSD(m.Pricing.Completion),
			CacheReadPerM:    parsePerTokenUSD(m.Pricing.InputCacheRead),
			CacheWritePerM:   parsePerTokenUSD(m.Pricing.InputCacheWrite),
			Source:           SourceLive,
		}
		if m.Reasoning != nil {
			caps.Reasoning = true
			caps.EffortOptions = m.Reasoning.SupportedEfforts
		}
		// A "reasoning" request parameter is the on/off lever for extended reasoning,
		// so it both marks the model as reasoning and exposes a thinking toggle.
		if sliceContains(m.SupportedParameters, "reasoning") || sliceContains(m.SupportedParameters, "reasoning_effort") {
			caps.Reasoning = true
			caps.ThinkingToggle = true
		}
		capsCopy := caps
		out = append(out, ModelInfo{ID: m.ID, Object: "model", Caps: &capsCopy})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("parse openrouter models: no usable entries")
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Anthropic — GET /v1/models (token limits + capability flags; no pricing)
// ---------------------------------------------------------------------------

type anthropicModel struct {
	ID             string `json:"id"`
	DisplayName    string `json:"display_name"`
	MaxInputTokens int    `json:"max_input_tokens"`
	MaxTokens      int    `json:"max_tokens"`
	Capabilities   *struct {
		Effort           effortList `json:"effort"`
		Thinking         bool       `json:"thinking"`
		ImageInput       bool       `json:"image_input"`
		PDFInput         bool       `json:"pdf_input"`
		StructuredOutput bool       `json:"structured_outputs"`
	} `json:"capabilities"`
}

// parseAnthropicModels parses Anthropic's /v1/models listing into capability
// snapshots (Source="live"). It carries token limits and capability flags but no
// pricing — the discovery merge fills pricing from the catalog while these live
// limits/flags win.
func parseAnthropicModels(body []byte) ([]ModelInfo, error) {
	var wrapped struct {
		Data []anthropicModel `json:"data"`
	}
	if err := json.Unmarshal(body, &wrapped); err != nil {
		return nil, fmt.Errorf("parse anthropic models: %w", err)
	}
	if len(wrapped.Data) == 0 {
		return nil, fmt.Errorf("parse anthropic models: no entries")
	}
	out := make([]ModelInfo, 0, len(wrapped.Data))
	for _, m := range wrapped.Data {
		if m.ID == "" {
			continue
		}
		caps := config.ModelCapabilities{
			ContextWindow: m.MaxInputTokens,
			MaxOutput:     m.MaxTokens,
			// Every Claude model tool-calls and accepts a custom temperature; the
			// /v1/models listing does not self-describe these, so assert them so
			// discovery shows them even without a catalog entry. Booleans OR-merge,
			// so this is safe alongside the catalog.
			ToolCall:   true,
			CustomTemp: true,
			Source:     SourceLive,
		}
		if c := m.Capabilities; c != nil {
			caps.ThinkingToggle = c.Thinking
			caps.Reasoning = c.Thinking || len(c.Effort) > 0
			caps.EffortOptions = c.Effort
			caps.Vision = c.ImageInput
			caps.StructuredOutput = c.StructuredOutput
			caps.InputModalities = anthropicInputModalities(c.ImageInput, c.PDFInput)
		}
		capsCopy := caps
		out = append(out, ModelInfo{ID: m.ID, Object: "model", Caps: &capsCopy})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("parse anthropic models: no usable entries")
	}
	return out, nil
}

// anthropicInputModalities derives the accepted input modalities from Anthropic's
// per-model capability flags (text is always accepted). Returns nil when only text
// is supported so the catalog's modality list can fill in via the merge.
func anthropicInputModalities(image, pdf bool) []string {
	if !image && !pdf {
		return nil
	}
	mods := []string{"text"}
	if image {
		mods = append(mods, "image")
	}
	if pdf {
		mods = append(mods, "pdf")
	}
	return mods
}
