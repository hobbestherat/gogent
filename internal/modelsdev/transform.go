package modelsdev

import (
	"fmt"
	"strings"

	"gogent/internal/config"
)

// deriveBaseAPITypes are the gogent api_types whose adapter already knows (or
// version-embeds) its base URL, so the catalog transform must leave Endpoint
// BLANK and let the adapter fill it. Feeding models.dev's p.API for these would
// double-count the version segment — most importantly for "anthropic", whose
// chatPath is "/v1/messages" (base "https://api.anthropic.com"): a p.API of
// ".../v1" would resolve to ".../v1/v1/messages" (404). zai/openrouter/vertex*
// likewise synthesize their base from the api_type alone (their seeded default
// config entries all leave Endpoint empty).
var deriveBaseAPITypes = map[string]bool{
	"anthropic":        true,
	"zai":              true,
	"openrouter":       true,
	"vertex":           true,
	"vertex-native":    true,
	"vertex-anthropic": true,
}

// ProviderAPIType maps a models.dev provider to gogent's api_type token. Most
// OpenAI-compatible gateways (Groq, Together, DeepSeek, Mistral, Fireworks, …)
// resolve to "openai"; the gateways gogent models specially get their own type.
// Unknown providers default to "openai", matching model.StringToAPIType.
func ProviderAPIType(p Provider) string {
	switch strings.ToLower(strings.TrimSpace(p.ID)) {
	case "openrouter":
		return "openrouter"
	case "zai", "z-ai", "z.ai":
		return "zai"
	case "anthropic":
		return "anthropic"
	case "google-vertex", "vertex":
		return "vertex"
	case "google-vertex-anthropic", "vertex-anthropic":
		return "vertex-anthropic"
	}
	// Fall back to a client-library hint before defaulting.
	switch npm := strings.ToLower(strings.TrimSpace(p.NPM)); {
	case strings.Contains(npm, "anthropic"):
		return "anthropic"
	case strings.Contains(npm, "openrouter"):
		return "openrouter"
	}
	return "openai"
}

// ToModelConfig builds a draft config.ModelConfig from a models.dev provider and
// model. It is pure (no network) and fills every field models.dev can supply; the
// API key (or Vertex project/location) is left blank for the user, and Name is the
// sanitized base — call UniqueName to resolve collisions before saving.
//
// Endpoint is resolver-aware: blank for deriveBaseAPITypes (adapter default base),
// p.API otherwise (the generic "openai" adapter's built-in base is a useless
// localhost default, so a real gateway must carry p.API). Thinking is deliberately
// left nil in v1 (provider default): forcing it would flip IsReasoningModel and
// change the request encoding. The review form leaves the toggle user-editable.
func ToModelConfig(providerID string, p Provider, m Model) config.ModelConfig {
	apiType := ProviderAPIType(p)
	cfg := config.ModelConfig{
		Name:          sanitizeName(providerID, m.ID),
		DisplayName:   m.Name,
		APIType:       apiType,
		Model:         m.ID,
		Temperature:   0.7,
		MaxTokens:     m.Limit.Output,
		ContextWindow: m.Limit.Context,
		Free:          m.Cost.Input == 0 && m.Cost.Output == 0,
	}
	if !deriveBaseAPITypes[apiType] {
		cfg.Endpoint = p.API
	}
	if effort := effortOptions(m); len(effort) > 0 {
		cfg.ReasoningEffort = effort[0]
		cfg.EffortOptions = effort
	}
	return cfg
}

// effortOptions returns the reasoning_options[type=effort] values (the first is
// the default), or nil when the model exposes no effort control.
func effortOptions(m Model) []string {
	for _, ro := range m.ReasoningOptions {
		if strings.EqualFold(strings.TrimSpace(ro.Type), "effort") && len(ro.Values) > 0 {
			return append([]string(nil), ro.Values...)
		}
	}
	return nil
}

// HasThinkingToggle reports whether the model advertises a type=toggle reasoning
// option, so the review form can highlight that the Thinking selector is
// meaningful for this model (the value itself stays user-driven).
func HasThinkingToggle(m Model) bool {
	for _, ro := range m.ReasoningOptions {
		if strings.EqualFold(strings.TrimSpace(ro.Type), "toggle") {
			return true
		}
	}
	return false
}

// UniqueName returns a config-unique model name for a provider+model, suffixing
// -2/-3/… when the sanitized base collides with an entry in taken (the set of
// existing model names). taken may be nil.
func UniqueName(providerID, modelID string, taken map[string]bool) string {
	base := sanitizeName(providerID, modelID)
	if !taken[base] {
		return base
	}
	for i := 2; ; i++ {
		cand := fmt.Sprintf("%s-%d", base, i)
		if !taken[cand] {
			return cand
		}
	}
}

// sanitizeName joins provider id and model id into a stable, lower-case,
// dash-separated config name (e.g. "openrouter-anthropic-claude-opus-4-6"). Any
// run of non-alphanumeric characters collapses to a single dash.
func sanitizeName(providerID, modelID string) string {
	raw := providerID + "-" + modelID
	var b strings.Builder
	lastDash := false
	for _, r := range strings.ToLower(raw) {
		switch {
		case (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'):
			b.WriteRune(r)
			lastDash = false
		default:
			if !lastDash {
				b.WriteByte('-')
				lastDash = true
			}
		}
	}
	return strings.Trim(b.String(), "-")
}
