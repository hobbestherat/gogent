package model

import (
	"net/http"
	"strings"
)

// APIType identifies which provider/wire conventions a backend speaks. It
// selects two things: a providerSpec (base-URL layout, default endpoint and
// request capabilities) and a wire-format adapter (request/response/stream
// translation; see adapterFor). OpenAI-compatible providers share one adapter
// and differ only in their providerSpec; a genuinely different protocol
// (Anthropic Messages) gets its own APIType, providerSpec and adapter. That is
// the seam to extend when adding a new provider family.
type APIType string

const (
	// APITypeOpenAI is the generic OpenAI-compatible API: the caller supplies a
	// base URL (or a full chat-completions URL) and we talk plain OpenAI.
	APITypeOpenAI APIType = "openai"
	// APITypeZAI is the Z.AI platform (https://docs.z.ai). It is OpenAI
	// compatible; only the default base URL differs, so the user can leave the
	// endpoint empty and just provide an API key.
	APITypeZAI APIType = "zai"
	// APITypeAnthropic is the Anthropic Messages API (POST /v1/messages). It is
	// not OpenAI-compatible — it uses x-api-key + anthropic-version auth, a
	// top-level system prompt, content-block message arrays, input_schema tools
	// and tool_use/tool_result blocks — so it is served by a dedicated adapter.
	APITypeAnthropic APIType = "anthropic"
	// APITypeOpenRouter is the OpenRouter gateway (https://openrouter.ai). It is
	// OpenAI-compatible (bearer auth, same wire format), differing only in its
	// default base URL and the recommended HTTP-Referer / X-Title attribution
	// headers it sends for app ranking and free-tier prioritization.
	APITypeOpenRouter APIType = "openrouter"
)

var stringToAPITypeMap = map[string]APIType{
	"openai":     APITypeOpenAI,
	"zai":        APITypeZAI,
	"z.ai":       APITypeZAI,
	"anthropic":  APITypeAnthropic,
	"claude":     APITypeAnthropic,
	"openrouter": APITypeOpenRouter,
}

// StringToAPIType resolves a config string to an APIType, defaulting to the
// generic OpenAI-compatible provider when empty or unknown.
func StringToAPIType(s string) APIType {
	if t, ok := stringToAPITypeMap[strings.ToLower(strings.TrimSpace(s))]; ok {
		return t
	}
	return APITypeOpenAI
}

// authMode selects how an API key is presented to a provider. OpenAI-compatible
// backends frequently differ only here (and in extraHeaders), so the auth policy
// lives on the providerSpec rather than the wire adapter: providers that share
// one adapter (OpenAI, Z.AI, OpenRouter, ...) can still authenticate differently.
type authMode string

const (
	// authBearer sends Authorization: Bearer <key> (OpenAI, Z.AI, OpenRouter and
	// Gemini's OpenAI-compat layer). It is also the zero-value default.
	authBearer authMode = "bearer"
	// authXAPIKey sends x-api-key: <key> (Anthropic Messages API).
	authXAPIKey authMode = "x-api-key"
	// authAzureKey sends api-key: <key> (Azure OpenAI).
	authAzureKey authMode = "azure"
	// authQuery carries the key in a URL query parameter named authQueryParam
	// rather than a header (Gemini's native ?key=).
	authQuery authMode = "query"
)

// OpenRouter app-attribution headers. OpenRouter uses these (both optional) to
// rank apps on its leaderboards and to prioritize free-tier traffic; sending
// them is recommended for every request. See
// https://openrouter.ai/docs/api/reference/overview.
const (
	openRouterReferer = "https://github.com/gogent/gogent"
	openRouterTitle   = "gogent"
)

// providerSpec describes how to derive concrete endpoints for an APIType from a
// (possibly empty) user-supplied base URL.
type providerSpec struct {
	// defaultBaseURL is used when the config endpoint is left empty, so simple
	// providers only need an API key.
	defaultBaseURL string
	// chatPath and modelsPath are appended to the base URL to reach the
	// chat-completions and model-listing endpoints.
	chatPath   string
	modelsPath string
	// maxTokensLimit is the largest value the provider accepts for the
	// max_tokens (output) parameter; requests above it are clamped. 0 means no
	// known limit (don't clamp), which suits local servers.
	maxTokensLimit int

	// --- authentication policy (issue #30) ---

	// authMode selects where the API key goes: an Authorization bearer token
	// (default), an x-api-key header, an Azure api-key header, or a URL query
	// parameter (authQueryParam).
	authMode authMode
	// authQueryParam is the URL query parameter that carries the key when
	// authMode is authQuery (e.g. "key" for Gemini's native API).
	authQueryParam string
	// extraHeaders are static headers attached to every authenticated request,
	// independent of the key itself: a version pin (Anthropic's
	// anthropic-version) or attribution (OpenRouter's HTTP-Referer / X-Title).
	extraHeaders map[string]string

	// --- reasoning-model request capabilities (issue #31) ---

	// reasoningTokenParam, when non-empty, is the JSON field used to cap output
	// tokens for reasoning models instead of the default max_tokens. OpenAI's
	// o-series and GPT-5 reject max_tokens and require max_completion_tokens;
	// Z.AI keeps max_tokens, so it leaves this empty.
	reasoningTokenParam string
	// reasoningRejectsTemperature drops temperature/top_p from reasoning
	// requests entirely. OpenAI reasoning models accept only the default
	// temperature (o-series require exactly 1, GPT-5 rejects the field
	// outright), so the safe encoding is to omit it.
	reasoningRejectsTemperature bool
	// supportsReasoningEffort / supportsThinking gate the reasoning_effort and
	// thinking request parameters so they are emitted only where understood.
	supportsReasoningEffort bool
	supportsThinking        bool
}

var providerSpecs = map[APIType]providerSpec{
	APITypeOpenAI: {
		// Neutral local default; apps with their own default resolve it upstream
		// (see config.DefaultEndpoint) and pass a full endpoint in.
		defaultBaseURL: "http://localhost:8080/v1",
		chatPath:       "/chat/completions",
		modelsPath:     "/models",
		authMode:       authBearer,
		// OpenAI reasoning models (o-series, GPT-5) require max_completion_tokens
		// and reject a custom temperature; they accept reasoning_effort but have
		// no `thinking` toggle.
		reasoningTokenParam:         "max_completion_tokens",
		reasoningRejectsTemperature: true,
		supportsReasoningEffort:     true,
	},
	APITypeZAI: {
		defaultBaseURL: "https://api.z.ai/api/paas/v4",
		chatPath:       "/chat/completions",
		modelsPath:     "/models",
		authMode:       authBearer,
		// Z.AI rejects max_tokens outside [1, 131072] with a 400.
		maxTokensLimit: 131072,
		// GLM reasoning keeps max_tokens and accepts a temperature; it exposes
		// both an explicit thinking toggle and reasoning_effort (GLM-5.2).
		supportsReasoningEffort: true,
		supportsThinking:        true,
	},
	APITypeAnthropic: {
		defaultBaseURL: "https://api.anthropic.com",
		chatPath:       "/v1/messages",
		modelsPath:     "/v1/models",
		// Anthropic authenticates with x-api-key and requires the version pin on
		// every request.
		authMode:     authXAPIKey,
		extraHeaders: map[string]string{"anthropic-version": anthropicVersion},
		// max_tokens is required by the Messages API and capped at the model's
		// output limit; 0 here leaves the (always-set) request value untouched.
		// Extended thinking and reasoning_effort are not wired through the
		// Anthropic adapter yet, so leave their capability flags unset (the
		// internal thinking/effort params would otherwise be emitted in the
		// OpenAI shape, which Anthropic rejects). See follow-up below.
	},
	APITypeOpenRouter: {
		defaultBaseURL: "https://openrouter.ai/api/v1",
		chatPath:       "/chat/completions",
		modelsPath:     "/models",
		authMode:       authBearer,
		extraHeaders: map[string]string{
			"HTTP-Referer": openRouterReferer,
			"X-Title":      openRouterTitle,
		},
	},
}

// specFor returns the providerSpec for an APIType, falling back to OpenAI.
func specFor(t APIType) providerSpec {
	if s, ok := providerSpecs[t]; ok {
		return s
	}
	return providerSpecs[APITypeOpenAI]
}

// APITypeIDs lists the selectable api_type values in display order (first is the
// default). Config UIs use this to populate an API-type dropdown.
func APITypeIDs() []string {
	return []string{
		string(APITypeOpenAI),
		string(APITypeZAI),
		string(APITypeAnthropic),
		string(APITypeOpenRouter),
	}
}

// authHeaders returns the request headers that authenticate apiKey to this
// provider, merged with any static extraHeaders (version pins, attribution). For
// query-parameter auth the key rides in the URL (see authQueryParam), so only
// the extra headers are returned. An empty key yields just the extra headers.
func (s providerSpec) authHeaders(apiKey string) http.Header {
	h := http.Header{}
	for k, v := range s.extraHeaders {
		h.Set(k, v)
	}
	if apiKey == "" {
		return h
	}
	switch s.authMode {
	case authXAPIKey:
		h.Set("x-api-key", apiKey)
	case authAzureKey:
		h.Set("api-key", apiKey)
	case authQuery:
		// carried in the URL query string, not a header
	default: // authBearer and the zero value
		h.Set("Authorization", "Bearer "+apiKey)
	}
	return h
}

// authQuery returns the URL query parameter name carrying the API key, or ""
// when this provider authenticates via headers instead.
func (s providerSpec) authQuery() string {
	if s.authMode == authQuery {
		return s.authQueryParam
	}
	return ""
}

// normalizeBaseURL reduces whatever the user put in the config endpoint to a
// bare base URL: an empty value falls back to the provider default, a full
// chat-completions URL is trimmed back to its base, and trailing slashes are
// dropped. This is what lets a user supply just a base URL and have the rest
// filled in automatically.
func normalizeBaseURL(endpoint string, spec providerSpec) string {
	e := strings.TrimRight(strings.TrimSpace(endpoint), "/")
	if e == "" {
		e = strings.TrimRight(spec.defaultBaseURL, "/")
	}
	if i := strings.LastIndex(e, spec.chatPath); i >= 0 && i == len(e)-len(spec.chatPath) {
		e = strings.TrimRight(e[:i], "/")
	}
	return e
}

// chatURL and modelsURL build the concrete endpoints for a base URL.
func (s providerSpec) chatURL(base string) string   { return base + s.chatPath }
func (s providerSpec) modelsURL(base string) string { return base + s.modelsPath }
