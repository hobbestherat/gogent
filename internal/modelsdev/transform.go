package modelsdev

import (
	"fmt"
	"strconv"
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

// ToConnection builds a draft config.ProviderConnection for a models.dev provider.
// It is pure (no network); the API key (or Vertex project/location) is left blank
// for the user. Endpoint is resolver-aware: blank for deriveBaseAPITypes (adapter
// default base), p.API otherwise (the generic "openai" adapter's built-in base is a
// useless localhost default, so a real gateway must carry p.API). Name is the
// provider id; callers resolve collisions / reuse before saving.
func ToConnection(p Provider) config.ProviderConnection {
	apiType := ProviderAPIType(p)
	pc := config.ProviderConnection{
		Name:    strings.ToLower(strings.TrimSpace(p.ID)),
		APIType: apiType,
	}
	if !deriveBaseAPITypes[apiType] {
		pc.Endpoint = p.API
	}
	return pc
}

// ToModelCapabilities maps a models.dev model to a config.ModelCapabilities
// snapshot (the discovery merge's catalog tier). Source is set to "catalog".
func ToModelCapabilities(m Model) config.ModelCapabilities {
	caps := config.ModelCapabilities{
		ContextWindow:    m.Limit.Context,
		MaxOutput:        m.Limit.Output,
		Reasoning:        ReasoningCapable(m),
		ThinkingToggle:   HasThinkingToggle(m),
		EffortOptions:    effortOptions(m),
		Vision:           m.Attachment,
		ToolCall:         m.ToolCall,
		StructuredOutput: m.StructuredOutput,
		CustomTemp:       m.Temperature,
		InputModalities:  append([]string(nil), m.Modalities.Input...),
		OutputModalities: append([]string(nil), m.Modalities.Output...),
		InputCostPerM:    m.Cost.Input,
		OutputCostPerM:   m.Cost.Output,
		CacheReadPerM:    m.Cost.CacheRead,
		CacheWritePerM:   m.Cost.CacheWrite,
		Knowledge:        m.Knowledge,
		ReleaseDate:      m.ReleaseDate,
		Source:           "catalog",
	}
	return caps
}

// ToModelConfig builds a draft config.ModelConfig from a models.dev model, pointing
// at the given connection name and carrying a capability snapshot. Name is the
// sanitized base — call UniqueName to resolve collisions before saving. Thinking is
// left nil (provider default); the default ReasoningEffort is the first effort
// option when the model exposes one.
func ToModelConfig(providerID, connName string, m Model) config.ModelConfig {
	cfg := config.ModelConfig{
		Name:        sanitizeName(providerID, m.ID),
		DisplayName: m.Name,
		Connection:  connName,
		Model:       m.ID,
		Temperature: 0.7,
		Caps:        ToModelCapabilities(m),
	}
	if effort := effortOptions(m); len(effort) > 0 {
		cfg.ReasoningEffort = effort[0]
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

// ReasoningCapable reports whether the model can reason in any form, honoring
// Model.Reasoning directly so a reasoning:true model with only a toggle (or no
// reasoning option at all) is still flagged — the picker badge infers this only
// from an effort option (issue #542 note 2). The review form shows it as a
// capability indicator.
func ReasoningCapable(m Model) bool {
	return m.Reasoning || len(effortOptions(m)) > 0 || HasThinkingToggle(m)
}

// CapabilityLabels returns the display-only capability indicators models.dev
// supplies for a model, in a stable order, omitting those it lacks. These are
// surfaced as a "Capabilities:" row in the review form; none is persisted to
// ModelConfig. Model.Temperature reports only WHETHER a custom temperature is
// accepted (a capability), not a value.
func CapabilityLabels(m Model) []string {
	var labels []string
	if ReasoningCapable(m) {
		labels = append(labels, "reasoning")
	}
	if m.ToolCall {
		labels = append(labels, "tool calling")
	}
	if m.Attachment {
		labels = append(labels, "vision")
	}
	if m.Temperature {
		labels = append(labels, "custom temperature")
	}
	return labels
}

// CostSummary renders a model's per-million-token pricing for the review form's
// "Cost:" indicator: "Free" when both sides are zero, otherwise
// "$<in> in / $<out> out per M".
func CostSummary(m Model) string {
	if m.Cost.Input == 0 && m.Cost.Output == 0 {
		return "Free"
	}
	return fmt.Sprintf("$%s in / $%s out per M", trimCost(m.Cost.Input), trimCost(m.Cost.Output))
}

// trimCost formats a USD-per-million figure without trailing zeros (5 -> "5",
// 0.15 -> "0.15", 2.5 -> "2.5") so the cost row stays compact.
func trimCost(v float64) string {
	return strconv.FormatFloat(v, 'f', -1, 64)
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
