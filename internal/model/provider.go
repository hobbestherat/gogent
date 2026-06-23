package model

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"gogent/internal/config"
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
	// APITypeVertex is Google Vertex AI's OpenAI-compatible endpoint
	// (/endpoints/openapi/chat/completions). It speaks the standard OpenAI wire
	// format — so it reuses openAIAdapter — but differs in two ways: its base URL
	// is derived from the config's Project/Location rather than a static default
	// (see providerSpec.baseURLFunc), and it authenticates with Google Application
	// Default Credentials (a bearer token injected by ADCRoundTripper) instead of
	// an API key (see authADC).
	APITypeVertex APIType = "vertex"
	// APITypeVertexNative is Google Vertex AI's NATIVE Gemini API
	// (:generateContent / :streamGenerateContent), as opposed to the
	// OpenAI-compatible shim (APITypeVertex). It speaks Gemini's own wire format —
	// contents/parts, systemInstruction, functionDeclarations, generationConfig —
	// so it is served by a dedicated geminiAdapter rather than openAIAdapter, and
	// it unlocks native-only features: thinking/reasoning (thinkingConfig,
	// thought:true parts), structured output via responseSchema, and image parts.
	// Like APITypeVertex it derives its base URL from Project/Location and
	// authenticates with ADC (authADC); unlike it, the model name lives in the URL
	// path (see chatURLFunc/streamURLFunc), not the request body. The config alias
	// "gemini" resolves here.
	APITypeVertexNative APIType = "vertex-native"
	// APITypeVertexAnthropic is Anthropic's Claude models served through Google
	// Vertex AI (:rawPredict / :streamRawPredict). It speaks the Anthropic Messages
	// wire format — so it reuses anthropicAdapter — but differs from the direct
	// Anthropic API in three Vertex-specific ways: (1) auth is Google ADC (a bearer
	// token via authADC), not x-api-key; (2) the model name lives in the URL path
	// (publishers/anthropic/models/{MODEL}:rawPredict), not the request body, so the
	// body omits "model"; and (3) the API version travels in the body as
	// "anthropic_version":"vertex-2023-10-16" rather than the anthropic-version
	// header. Like the other Vertex types its base URL is derived from
	// Project/Location (v1, GA). The config alias "claude-vertex" resolves here.
	APITypeVertexAnthropic APIType = "vertex-anthropic"
)

// vertexAnthropicVersion is the value carried in the request body's
// anthropic_version field for Claude on Vertex AI. Unlike the direct Anthropic
// API — which pins the version via the anthropic-version HEADER (see
// anthropicVersion) — Vertex names it in the body and uses its own dated token.
const vertexAnthropicVersion = "vertex-2023-10-16"

var stringToAPITypeMap = map[string]APIType{
	"openai":           APITypeOpenAI,
	"zai":              APITypeZAI,
	"z.ai":             APITypeZAI,
	"anthropic":        APITypeAnthropic,
	"claude":           APITypeAnthropic,
	"openrouter":       APITypeOpenRouter,
	"vertex":           APITypeVertex,
	"vertex-native":    APITypeVertexNative,
	"gemini":           APITypeVertexNative,
	"vertex-anthropic": APITypeVertexAnthropic,
	"claude-vertex":    APITypeVertexAnthropic,
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
	// authADC authenticates with a Google Application Default Credentials bearer
	// token, injected per request (and auto-refreshed) by an ADCRoundTripper
	// rather than from a configured API key (Vertex AI). The constructor builds
	// the ADC round-tripper when a spec selects this mode; see
	// NewModelConnectionFromConfig.
	authADC authMode = "adc"
)

// OpenRouter app-attribution headers. OpenRouter uses these (both optional) to
// rank apps on its leaderboards and to prioritize free-tier traffic; sending
// them is recommended for every request. See
// https://openrouter.ai/docs/api/reference/overview.
const (
	openRouterReferer = "https://github.com/hobbestherat/gogent"
	openRouterTitle   = "gogent"
)

// providerSpec describes how to derive concrete endpoints for an APIType from a
// (possibly empty) user-supplied base URL.
type providerSpec struct {
	// defaultBaseURL is used when the config endpoint is left empty, so simple
	// providers only need an API key.
	defaultBaseURL string
	// baseURLFunc derives the base URL from the model config when the provider's
	// endpoint is not a single static default but depends on per-model settings —
	// Vertex AI builds its host and path from Project/Location. It is nil for
	// every provider with a static defaultBaseURL; when set, the constructor uses
	// it in place of defaultBaseURL but only when the user leaves Endpoint empty,
	// so an explicit endpoint still overrides. See NewModelConnectionFromConfig.
	baseURLFunc func(*config.ModelConfig) string
	// chatURLFunc / streamURLFunc build the concrete (non-streaming / streaming)
	// request URLs from the base URL and the model name, for providers whose
	// endpoint embeds the model in the URL path rather than the request body —
	// Vertex AI's native Gemini API uses /publishers/google/models/{MODEL}:generateContent
	// and a distinct :streamGenerateContent?alt=sse streaming route. They are nil
	// for every provider whose chat path is static (the model rides in the body);
	// when set, the constructor uses them in place of spec.chatURL(base) and to
	// populate ModelConnection.StreamURL. See NewModelConnectionFromConfig.
	chatURLFunc   func(base, model string) string
	streamURLFunc func(base, model string) string
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

	// supportsResponseFormat gates the response_format request parameter (OpenAI
	// structured outputs) and the strict-tool parallel_tool_calls invariant
	// (issue #49). It is set for the OpenAI-compatible family; Anthropic, which
	// has no response_format field, leaves it unset and relies on strict tools +
	// tool_choice forcing for structured output.
	supportsResponseFormat bool
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
		supportsResponseFormat:      true,
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
		supportsResponseFormat:  true,
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
		supportsResponseFormat: true,
	},
	APITypeVertex: {
		// Vertex AI's OpenAI-compatible endpoint. The base URL is dynamic — built
		// from the config's Project/Location by baseURLFunc — so defaultBaseURL is
		// empty. The OpenAI-compat layer is preview-only and lives under v1beta1
		// (folded into the base URL); chatPath reaches the chat-completions route.
		// Auth is ADC (no API key). modelsPath is left empty: the compat endpoint
		// does not expose a usable /models listing, so users name the model
		// explicitly (e.g. "google/gemini-2.5-flash").
		chatPath:    "/endpoints/openapi/chat/completions",
		baseURLFunc: vertexOpenAIBaseURL,
		authMode:    authADC,
		// OpenAI-compatible capability surface: response_format (structured
		// output) is supported; reasoning_effort / thinking are not exposed
		// through the compat layer (those are native-Gemini features).
		supportsResponseFormat: true,
	},
	APITypeVertexNative: {
		// Vertex AI's native Gemini API. Like the compat mode the base URL is
		// derived from Project/Location (baseURLFunc), but it uses v1 (GA) and the
		// model name is part of the URL path, not the body — so chatURLFunc and
		// streamURLFunc build the :generateContent / :streamGenerateContent?alt=sse
		// routes instead of appending a static chatPath. chatPath is set to the
		// native models route purely so ListModels reports "not supported" (a
		// non-empty chatPath with an empty modelsPath) rather than probing a bad
		// URL; native model listing is a documented limitation. Auth is ADC.
		baseURLFunc:   vertexNativeBaseURL,
		chatURLFunc:   vertexNativeChatURL,
		streamURLFunc: vertexNativeStreamURL,
		chatPath:      "/publishers/google/models",
		authMode:      authADC,
		// Native Gemini capability surface: structured output via responseSchema,
		// and thinking via thinkingConfig (thought:true parts). reasoning_effort is
		// not a Gemini concept (it uses thinkingConfig/thinkingBudget), and Gemini
		// 2.5 "thinking" models accept temperature/top_p normally, so the reasoning
		// temperature rejection stays off.
		supportsResponseFormat:      true,
		supportsThinking:            true,
		supportsReasoningEffort:     false,
		reasoningRejectsTemperature: false,
	},
	APITypeVertexAnthropic: {
		// Anthropic Claude on Vertex AI. The base URL is derived from
		// Project/Location (v1, GA — same as vertexNativeBaseURL), and like the
		// native Gemini route the model name lives in the URL path, so chatURLFunc /
		// streamURLFunc build the :rawPredict / :streamRawPredict routes
		// (publishers/anthropic/...) instead of appending a static chatPath. chatPath
		// is set to the anthropic models route purely so ListModels reports "not
		// supported" (a non-empty chatPath with an empty modelsPath) rather than
		// probing a bad URL. Auth is ADC; the anthropic-version is sent in the body
		// (see anthropicAdapter), so no extraHeaders are attached here.
		baseURLFunc:   vertexNativeBaseURL,
		chatURLFunc:   vertexAnthropicChatURL,
		streamURLFunc: vertexAnthropicStreamURL,
		chatPath:      "/publishers/anthropic/models",
		authMode:      authADC,
		// Anthropic capability surface on Vertex: extended thinking is supported and
		// emitted as adaptive thinking (see anthropicAdapter.buildBody). There is no
		// response_format field (structured output goes through strict tools +
		// tool_choice forcing), and reasoning_effort is not an Anthropic body
		// parameter, so both stay off. reasoningRejectsTemperature is left off
		// because the adapter omits sampling params for this provider unconditionally
		// (modern Claude rejects temperature/top_p), not via the reasoning gate.
		supportsThinking:        true,
		supportsReasoningEffort: false,
		supportsResponseFormat:  false,
	},
}

// vertexAIHost returns the Vertex AI API host for a region. Every location is
// reached through a regional host ("{location}-aiplatform.googleapis.com")
// EXCEPT the special "global" location, which uses the unprefixed host
// (aiplatform.googleapis.com) — though its URL path still carries
// /locations/global/.
func vertexAIHost(location string) string {
	if location == "global" {
		return "aiplatform.googleapis.com"
	}
	return location + "-aiplatform.googleapis.com"
}

// vertexOpenAIBaseURL builds the Vertex AI OpenAI-compatible base URL from a
// model config's Project and Location:
//
//	https://{LOCATION}-aiplatform.googleapis.com/v1beta1/projects/{PROJECT}/locations/{LOCATION}
//
// (host prefix dropped for "global"; see vertexAIHost). The concrete chat path
// (chatPath) is appended by chatURL. Location appears in both the host and the
// path and must match.
//
// Location is lower-cased: GCP region IDs are canonically lowercase, so this
// makes the "global" host special case (and the host/path generally) robust to a
// user typing "Global" or "US-CENTRAL1" rather than silently building a bad host
// like "Global-aiplatform.googleapis.com".
func vertexOpenAIBaseURL(c *config.ModelConfig) string {
	loc := strings.ToLower(strings.TrimSpace(c.Location))
	return fmt.Sprintf("https://%s/v1beta1/projects/%s/locations/%s",
		vertexAIHost(loc), strings.TrimSpace(c.Project), loc)
}

// vertexNativeBaseURL builds the Vertex AI NATIVE Gemini base URL from a model
// config's Project and Location:
//
//	https://{LOCATION}-aiplatform.googleapis.com/v1/projects/{PROJECT}/locations/{LOCATION}
//
// It differs from vertexOpenAIBaseURL only in the API version — v1 (GA) for the
// native API rather than v1beta1 for the OpenAI-compat shim. The model name is
// NOT part of the base; it is interpolated into the path by vertexNativeChatURL /
// vertexNativeStreamURL. Host prefix is dropped for "global" (see vertexAIHost);
// Location is lower-cased so the host/path are robust to a mixed-case region.
func vertexNativeBaseURL(c *config.ModelConfig) string {
	loc := strings.ToLower(strings.TrimSpace(c.Location))
	return fmt.Sprintf("https://%s/v1/projects/%s/locations/%s",
		vertexAIHost(loc), strings.TrimSpace(c.Project), loc)
}

// vertexNativeChatURL builds the native non-streaming request URL, embedding the
// model name in the path (Gemini's native API names the model in the URL, not the
// body):
//
//	{base}/publishers/google/models/{MODEL}:generateContent
func vertexNativeChatURL(base, model string) string {
	return fmt.Sprintf("%s/publishers/google/models/%s:generateContent",
		strings.TrimRight(strings.TrimSpace(base), "/"), strings.TrimSpace(model))
}

// vertexAnthropicChatURL builds the Claude-on-Vertex non-streaming request URL,
// embedding the model name in the path (Vertex names the Anthropic model in the
// URL, not the body):
//
//	{base}/publishers/anthropic/models/{MODEL}:rawPredict
func vertexAnthropicChatURL(base, model string) string {
	return fmt.Sprintf("%s/publishers/anthropic/models/%s:rawPredict",
		strings.TrimRight(strings.TrimSpace(base), "/"), strings.TrimSpace(model))
}

// vertexAnthropicStreamURL builds the Claude-on-Vertex streaming request URL. It
// targets the distinct :streamRawPredict action, which frames the response as
// Anthropic's native server-sent events (the same message_start / content_block_*
// / message_stop grammar the direct Messages API streams):
//
//	{base}/publishers/anthropic/models/{MODEL}:streamRawPredict
//
// Unlike the native Gemini stream route, no ?alt=sse is needed — :streamRawPredict
// already streams SSE. The request body still carries "stream":true.
func vertexAnthropicStreamURL(base, model string) string {
	return fmt.Sprintf("%s/publishers/anthropic/models/%s:streamRawPredict",
		strings.TrimRight(strings.TrimSpace(base), "/"), strings.TrimSpace(model))
}

// vertexNativeStreamURL builds the native streaming request URL. It targets the
// distinct :streamGenerateContent action and appends ?alt=sse so the response is
// framed as server-sent events (without it Vertex streams a JSON array instead):
//
//	{base}/publishers/google/models/{MODEL}:streamGenerateContent?alt=sse
func vertexNativeStreamURL(base, model string) string {
	return fmt.Sprintf("%s/publishers/google/models/%s:streamGenerateContent?alt=sse",
		strings.TrimRight(strings.TrimSpace(base), "/"), strings.TrimSpace(model))
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
		string(APITypeVertex),
		string(APITypeVertexNative),
		string(APITypeVertexAnthropic),
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
	e := strings.TrimSpace(endpoint)
	if e == "" {
		e = spec.defaultBaseURL
	}
	return stripChatPath(e, spec.chatPath)
}

// stripChatPath reduces a URL to its provider base by removing a trailing chat
// path from the URL's *path component* and dropping trailing slashes, while
// preserving any query string. Parsing with net/url (rather than raw string
// surgery on the whole URL) keeps this correct when the endpoint carries a
// query — e.g. Azure's ?api-version= — or a layout where the chat path is not
// the literal tail of the string.
func stripChatPath(raw, chatPath string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return strings.TrimRight(strings.TrimSpace(raw), "/")
	}
	u.Path = strings.TrimRight(u.Path, "/")
	if chatPath != "" && strings.HasSuffix(u.Path, chatPath) {
		u.Path = strings.TrimRight(strings.TrimSuffix(u.Path, chatPath), "/")
	}
	return u.String()
}

// chatURL and modelsURL build the concrete endpoints for a base URL, inserting
// the provider path before any query string so an Azure-style ?api-version= (or
// other carried query) survives onto the derived endpoint.
func (s providerSpec) chatURL(base string) string   { return appendPath(base, s.chatPath) }
func (s providerSpec) modelsURL(base string) string { return appendPath(base, s.modelsPath) }

// appendPath joins a provider path onto a base URL's path component, keeping any
// query string intact (base + "?q" + "/path" must become ".../path?q", not
// "...?q/path"). Falls back to plain concatenation if base is unparseable.
func appendPath(base, path string) string {
	u, err := url.Parse(base)
	if err != nil {
		return strings.TrimRight(base, "/") + path
	}
	u.Path = strings.TrimRight(u.Path, "/") + path
	return u.String()
}
