package model

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"golang.org/x/oauth2"

	"gogent/internal/config"
)

// This file defines the provider abstraction: each model backend ("openai",
// "anthropic", "vertex", …) is a *provider value that COMPOSES small, reusable
// strategy objects — an endpointResolver (how to build URLs), an authScheme (how
// to authenticate), a wire adapter (request/response/stream translation), and
// optional per-operation strategies such as a modelLister. ModelConnection is a
// generic executor that drives a provider; it knows nothing backend-specific.
//
// Adding a backend is a self-contained change: write a provider_<name>.go that
// composes the strategies it needs and registers itself (see provider_openai.go,
// provider_anthropic.go, provider_vertex.go). Adding a new *operation* (token
// counting, embeddings, …) is likewise additive: define a strategy interface and
// a nil-able field on provider, then run it through the shared doJSON runner.

// ---------------------------------------------------------------------------
// api_type identity
// ---------------------------------------------------------------------------

// APIType is the config token (`api_type`) that names a provider. It is the
// stable public identifier used in configs, the model editor, and stats; the
// behaviour behind it lives in the registered *provider.
type APIType string

const (
	// APITypeOpenAI is the generic OpenAI-compatible API and the default.
	APITypeOpenAI APIType = "openai"
	// APITypeZAI is the Z.AI platform (OpenAI-compatible; different default base).
	APITypeZAI APIType = "zai"
	// APITypeAnthropic is the Anthropic Messages API (POST /v1/messages).
	APITypeAnthropic APIType = "anthropic"
	// APITypeOpenRouter is the OpenRouter gateway (OpenAI-compatible + attribution
	// headers).
	APITypeOpenRouter APIType = "openrouter"
	// APITypeVertex is Google Vertex AI's OpenAI-compatible endpoint (Gemini via
	// the /endpoints/openapi shim; ADC auth, project/location-derived base).
	APITypeVertex APIType = "vertex"
	// APITypeVertexNative is Vertex AI's native Gemini API (:generateContent /
	// :streamGenerateContent). The config alias "gemini" resolves here.
	APITypeVertexNative APIType = "vertex-native"
	// APITypeVertexAnthropic is Anthropic's Claude models on Vertex AI
	// (:rawPredict / :streamRawPredict). The config alias "claude-vertex" resolves
	// here.
	APITypeVertexAnthropic APIType = "vertex-anthropic"
)

// stringToAPITypeMap resolves config strings (and aliases) to an APIType.
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

// apiTypeDisplayOrder is the selectable api_type order for config UIs (first is
// the default). Kept explicit so it is independent of provider-registration
// (init) order.
var apiTypeDisplayOrder = []APIType{
	APITypeOpenAI,
	APITypeZAI,
	APITypeAnthropic,
	APITypeOpenRouter,
	APITypeVertex,
	APITypeVertexNative,
	APITypeVertexAnthropic,
}

// StringToAPIType resolves a config string to an APIType, defaulting to the
// generic OpenAI-compatible provider when empty or unknown.
func StringToAPIType(s string) APIType {
	if t, ok := stringToAPITypeMap[strings.ToLower(strings.TrimSpace(s))]; ok {
		return t
	}
	return APITypeOpenAI
}

// configModelName identifies a model config in user-facing errors, preferring its
// Name, then DisplayName, falling back to "<unnamed>" so a misconfiguration message
// always names something even when both are blank.
func configModelName(cfg *config.ModelConfig) string {
	if cfg == nil {
		return "<unnamed>"
	}
	if n := strings.TrimSpace(cfg.Name); n != "" {
		return n
	}
	if n := strings.TrimSpace(cfg.DisplayName); n != "" {
		return n
	}
	return "<unnamed>"
}

// APITypeIDs lists the selectable api_type values in display order (first is the
// default). Config UIs use this to populate an API-type dropdown.
func APITypeIDs() []string {
	ids := make([]string, 0, len(apiTypeDisplayOrder))
	for _, t := range apiTypeDisplayOrder {
		ids = append(ids, string(t))
	}
	return ids
}

// OpenRouter app-attribution headers (both optional, recommended on every
// request). See https://openrouter.ai/docs/api/reference/overview.
const (
	openRouterReferer = "https://github.com/hobbestherat/gogent"
	openRouterTitle   = "gogent"
)

// ---------------------------------------------------------------------------
// Core types
// ---------------------------------------------------------------------------

// Endpoints is the resolved set of request URLs for a model config. StreamURL is
// empty when streaming uses the same URL as blocking (the OpenAI-compatible
// case); it is set only where the streaming route is a distinct path (Vertex's
// :streamGenerateContent / :streamRawPredict).
type Endpoints struct {
	ChatURL   string
	StreamURL string
}

// CacheControlKind declares whether a provider accepts CLIENT-SIDE prompt-cache
// directives, and in what form. It is a capability declaration only: it advertises
// the request-side lever for the follow-up issues that wire emission (#545
// Anthropic breakpoints, #547 Gemini explicit cached content). Providers that
// cache automatically with no client lever are CacheControlNone. The actual
// directive stays wire-specific in each adapter; nothing reads this for emission
// yet.
type CacheControlKind uint8

const (
	// CacheControlNone: caching is automatic, no client directive accepted
	// (OpenAI, Z.AI, OpenRouter, and the Vertex OpenAI-compat surface).
	CacheControlNone CacheControlKind = iota
	// CacheControlBreakpoints: Anthropic cache_control breakpoints on content blocks.
	CacheControlBreakpoints
	// CacheControlCachedContent: Gemini explicit cachedContent resources.
	CacheControlCachedContent
)

// Capabilities reports how a provider wants the internal CompletionRequest shaped
// on the wire. buildRequest reads these flags to gate reasoning/thinking/format
// parameters and the output-token field, so a provider never emits a parameter
// its backend would reject.
type Capabilities struct {
	// MaxTokensLimit clamps the output-token cap (0 = no known limit).
	MaxTokensLimit int
	// ReasoningTokenParam, when "max_completion_tokens", makes reasoning models
	// send that field instead of max_tokens (OpenAI o-series / GPT-5).
	ReasoningTokenParam string
	// ReasoningRejectsTemperature drops temperature/top_p for reasoning models.
	ReasoningRejectsTemperature bool
	// SupportsReasoningEffort / SupportsThinking gate the reasoning_effort and
	// thinking request parameters.
	SupportsReasoningEffort bool
	SupportsThinking        bool
	// SupportsResponseFormat gates response_format (OpenAI structured outputs) and
	// the strict-tool parallel_tool_calls invariant.
	SupportsResponseFormat bool
	// CacheControl declares the provider's client-side cache-directive support, for
	// the request-side follow-ups (#545/#547). Declaration only — see CacheControlKind.
	CacheControl CacheControlKind
	// CacheReadMultiplier / CacheWriteMultiplier price prompt-cache READ and WRITE
	// tokens relative to a full-price input token, for the cost-weighted agent budget
	// (issue #544). 0 means 1.0 (face value), so a provider that declares neither
	// prices all input at 1x — identical to before cache cost-weighting. Reads are
	// discounted (<1); writes are an Anthropic-only premium (>1). A discount that
	// varies by model WITHIN a provider (e.g. DeepSeek riding api_type "openai")
	// is expressed per-model via ModelCaps, which overrides these.
	CacheReadMultiplier  float64
	CacheWriteMultiplier float64
}

// ---------------------------------------------------------------------------
// Strategy interfaces — one axis of provider behaviour each
// ---------------------------------------------------------------------------

// endpointResolver builds the chat (blocking) and stream URLs for a model config.
type endpointResolver interface {
	endpoints(cfg *config.ModelConfig) Endpoints
}

// authScheme builds the auth/transport for a model config. It wraps the shared
// pooled transport so keep-alive connections persist; returning nil means no
// auth is injected and the shared transport is used directly.
type authScheme interface {
	roundTripper(cfg *config.ModelConfig) http.RoundTripper
}

// modelLister enumerates the models a provider serves (the Scan button). It is an
// OPTIONAL operation: a provider that cannot list models leaves provider.lister
// nil and ListModels reports "not supported". This is the template for future
// per-operation strategies (token counting, embeddings, …): define the interface,
// add a nil-able field on provider, and run the call through ModelConnection.doJSON.
type modelLister interface {
	list(ctx context.Context, c *ModelConnection) ([]ModelInfo, error)
}

// ---------------------------------------------------------------------------
// provider — composes the strategies for one api_type
// ---------------------------------------------------------------------------

// provider is one model backend. It is a value composing the strategy objects
// above plus the wire adapter and request capabilities. The zero-value-friendly
// optional fields (lister, validate) keep simple providers terse.
type provider struct {
	apiType   APIType
	adapter   adapter
	caps      Capabilities
	endpoints endpointResolver
	auth      authScheme
	// lister enumerates models for the Scan button; nil = listing unsupported.
	lister modelLister
	// validate returns a deferred config error (e.g. Vertex missing
	// project/location); nil = always valid.
	validate func(cfg *config.ModelConfig) error
	// normalizeModelID, when non-nil, rewrites the outgoing request's model id at the
	// send seam (buildRequest), as a last-line defense in depth. It is the request-build
	// counterpart of the lister's format func: the Vertex OpenAI-compat shim uses it to
	// auto-qualify a bare "gemini-…" to "google/gemini-…" so a model that escaped
	// ValidateModelConfig can never be sent bare and 400 opaquely (issue #574). nil =
	// the model id is sent verbatim (every non-shim provider). It is applied ONLY to a
	// provider whose model travels in the request body; providers that carry the model
	// in the URL (vertex-native/anthropic) leave it nil and validate the shape instead.
	normalizeModelID func(id string) string
	// derivesBase reports that this provider synthesizes its own request base URL
	// (from the api_type alone, or from project/location) when the config leaves
	// Endpoint empty, so an empty endpoint is still a complete, routable config. The
	// generic OpenAI provider leaves this false: its localhost default is a
	// placeholder, so there an empty endpoint is NOT routable. This is the registry's
	// single source of truth for the api_types that mirror
	// modelsdev.deriveBaseAPITypes; routability validation
	// (NewModelConnectionFromConfig) reads it instead of a separate hardcoded list.
	derivesBase bool
}

func (p *provider) validateConfig(cfg *config.ModelConfig) error {
	if p.validate == nil {
		return nil
	}
	return p.validate(cfg)
}

// providerRegistry holds every registered backend keyed by api_type. Per-provider
// files populate it from init(); ModelConnection looks providers up by APIType.
var providerRegistry = map[APIType]*provider{}

// registerProvider adds a provider to the registry. Called from each
// provider_<name>.go init(). A duplicate api_type is a programming error.
func registerProvider(p *provider) {
	if _, dup := providerRegistry[p.apiType]; dup {
		panic(fmt.Sprintf("model: duplicate provider registration for api_type %q", p.apiType))
	}
	providerRegistry[p.apiType] = p
}

// providerFor returns the provider for an APIType, falling back to the OpenAI
// provider for an unknown/zero type so a hand-built connection still works.
func providerFor(t APIType) *provider {
	if p, ok := providerRegistry[t]; ok {
		return p
	}
	return providerRegistry[APITypeOpenAI]
}

// ---------------------------------------------------------------------------
// Shared operation runner
// ---------------------------------------------------------------------------

// doJSON performs an authenticated JSON request through the connection's client
// and returns the response body, classifying non-200s via analyzeError. It is the
// single seam every non-completion operation (model listing today; token
// counting, embeddings, … later) flows through: auth (the client's round-tripper)
// and error handling are applied uniformly, so an operation strategy only needs to
// build the URL + extra headers and parse the body. extraHeaders are operation
// specific (e.g. Vertex's X-Goog-User-Project); the auth header is added by the
// round-tripper.
func (c *ModelConnection) doJSON(ctx context.Context, method, rawURL string, extraHeaders http.Header) ([]byte, error) {
	// A deferred config error (e.g. an unroutable entry, issue #505) short-circuits
	// here too, so model listing / Scan fails fast with the clear misconfiguration
	// message instead of dialing the localhost placeholder, matching the completion
	// path (complete/completeStream).
	if c.configErr != nil {
		return nil, c.configErr
	}
	req, err := http.NewRequestWithContext(ctx, method, rawURL, nil)
	if err != nil {
		return nil, &ModelError{Type: ErrorConnection, Message: fmt.Sprintf("failed to create request: %v", err)}
	}
	req.Header.Set("Accept", "application/json")
	for k, vs := range extraHeaders {
		for _, v := range vs {
			req.Header.Set(k, v)
		}
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, &ModelError{Type: ErrorConnection, Message: fmt.Sprintf("request failed: %v", err)}
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, &ModelError{Type: ErrorGeneric, Message: fmt.Sprintf("failed to read response: %v", err)}
	}
	if resp.StatusCode != http.StatusOK {
		return nil, c.analyzeError(resp.StatusCode, string(body))
	}
	return body, nil
}

// doJSONBody is doJSON with a request body — the same auth/error seam for the
// write-side non-completion operations (today: native-Gemini cachedContents
// create/refresh/delete, issue #547). A nil body sends an empty payload (e.g. a
// DELETE). Content-Type is set when a body is present; auth is still injected by
// the client's round-tripper. Returns the 200 response bytes (empty for a
// no-content success).
func (c *ModelConnection) doJSONBody(ctx context.Context, method, rawURL string, extraHeaders http.Header, body []byte) ([]byte, error) {
	if c.configErr != nil {
		return nil, c.configErr
	}
	var rdr io.Reader
	if len(body) > 0 {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, rawURL, rdr)
	if err != nil {
		return nil, &ModelError{Type: ErrorConnection, Message: fmt.Sprintf("failed to create request: %v", err)}
	}
	req.Header.Set("Accept", "application/json")
	if len(body) > 0 {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, vs := range extraHeaders {
		for _, v := range vs {
			req.Header.Set(k, v)
		}
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, &ModelError{Type: ErrorConnection, Message: fmt.Sprintf("request failed: %v", err)}
	}
	defer func() { _ = resp.Body.Close() }()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, &ModelError{Type: ErrorGeneric, Message: fmt.Sprintf("failed to read response: %v", err)}
	}
	if resp.StatusCode != http.StatusOK {
		return nil, c.analyzeError(resp.StatusCode, string(respBody))
	}
	return respBody, nil
}

// ---------------------------------------------------------------------------
// Endpoint resolvers
// ---------------------------------------------------------------------------

// staticBaseEndpoints resolves "base URL + static chat path" endpoints, where the
// model name rides in the request body and streaming reuses the chat URL. The
// base is the user's endpoint (normalized) or, when empty, either a static
// default or a derived base (baseURLFunc, used by Vertex's OpenAI-compat shim).
type staticBaseEndpoints struct {
	defaultBaseURL string
	baseURLFunc    func(cfg *config.ModelConfig) string // optional; only when Endpoint is empty
	chatPath       string
}

func (e staticBaseEndpoints) endpoints(cfg *config.ModelConfig) Endpoints {
	base := normalizeBaseURL(cfg.Endpoint, e.defaultBaseURL, e.chatPath)
	if e.baseURLFunc != nil && strings.TrimSpace(cfg.Endpoint) == "" {
		base = e.baseURLFunc(cfg)
	}
	return Endpoints{ChatURL: appendPath(base, e.chatPath)}
}

// modelURLEndpoints resolves endpoints where the model name is embedded in the
// URL PATH (Vertex's native Gemini and Claude routes) and the streaming action is
// a distinct URL. The base is derived from project/location unless an explicit
// endpoint overrides it.
type modelURLEndpoints struct {
	baseURLFunc   func(cfg *config.ModelConfig) string
	chatURLFunc   func(base, model string) string
	streamURLFunc func(base, model string) string
}

func (e modelURLEndpoints) endpoints(cfg *config.ModelConfig) Endpoints {
	base := strings.TrimRight(strings.TrimSpace(cfg.Endpoint), "/")
	if base == "" {
		base = e.baseURLFunc(cfg)
	}
	return Endpoints{
		ChatURL:   e.chatURLFunc(base, cfg.Model),
		StreamURL: e.streamURLFunc(base, cfg.Model),
	}
}

// ---------------------------------------------------------------------------
// Auth schemes
// ---------------------------------------------------------------------------

// authMode selects where a key-based scheme puts the API key.
type authMode string

const (
	authBearer   authMode = "bearer"    // Authorization: Bearer <key>
	authXAPIKey  authMode = "x-api-key" // x-api-key: <key> (Anthropic)
	authAzureKey authMode = "azure"     // api-key: <key> (Azure OpenAI)
	authQuery    authMode = "query"     // ?<param>=<key> (Gemini-style)
)

// keyAuth is the API-key auth scheme: it presents cfg.APIKey per authMode and
// attaches any static extraHeaders (version pins, attribution). With no key it
// returns nil so the shared transport is used directly (a local server needing no
// auth still works).
type keyAuth struct {
	mode         authMode
	queryParam   string            // when mode == authQuery
	extraHeaders map[string]string // static headers (anthropic-version, attribution)
}

func (a keyAuth) headers(apiKey string) http.Header {
	h := http.Header{}
	for k, v := range a.extraHeaders {
		h.Set(k, v)
	}
	if apiKey == "" {
		return h
	}
	switch a.mode {
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

func (a keyAuth) query() string {
	if a.mode == authQuery {
		return a.queryParam
	}
	return ""
}

func (a keyAuth) roundTripper(cfg *config.ModelConfig) http.RoundTripper {
	// With static extra headers but no key (e.g. OpenRouter attribution on a
	// free tier), still attach them; with neither, use the shared transport.
	if cfg.APIKey == "" && len(a.extraHeaders) == 0 {
		return nil
	}
	return &APIKeyRoundTripper{
		apiKey:     cfg.APIKey,
		headers:    a.headers(cfg.APIKey),
		queryParam: a.query(),
		transport:  sharedHTTPTransport,
	}
}

// adcAuth authenticates with Google Application Default Credentials: a bearer
// token minted (and auto-refreshed) per request by ADCRoundTripper. No API key.
// The token source is resolved lazily on first use so construction never fails;
// an ADC-misconfiguration error surfaces on the first request instead.
type adcAuth struct{}

func (adcAuth) roundTripper(cfg *config.ModelConfig) http.RoundTripper {
	return &ADCRoundTripper{
		tokenSource: &lazyTokenSource{newTS: func() (oauth2.TokenSource, error) {
			return adcTokenSourceFunc(context.Background(), adcScope)
		}},
		transport: sharedHTTPTransport,
	}
}

// ---------------------------------------------------------------------------
// URL helpers (shared by resolvers and listers)
// ---------------------------------------------------------------------------

// normalizeBaseURL reduces a user-supplied endpoint to a bare base URL: empty
// falls back to defaultBaseURL, a full chat URL is trimmed back to its base, and
// trailing slashes are dropped.
func normalizeBaseURL(endpoint, defaultBaseURL, chatPath string) string {
	e := strings.TrimSpace(endpoint)
	if e == "" {
		e = defaultBaseURL
	}
	return stripChatPath(e, chatPath)
}

// stripChatPath reduces a URL to its provider base by removing a trailing chat
// path from the URL's *path component* and dropping trailing slashes, while
// preserving any query string (e.g. Azure's ?api-version=). Parsing with net/url
// keeps this correct when the endpoint carries a query.
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

// appendPath joins a provider path onto a base URL's path component, keeping any
// query string intact (base + "?q" + "/path" must become ".../path?q").
func appendPath(base, path string) string {
	u, err := url.Parse(base)
	if err != nil {
		return strings.TrimRight(base, "/") + path
	}
	u.Path = strings.TrimRight(u.Path, "/") + path
	return u.String()
}

// ---------------------------------------------------------------------------
// OpenAI listing convention (shared by OpenAI-compatible providers + Anthropic)
// ---------------------------------------------------------------------------

// openAILister lists models via the OpenAI/OpenRouter "GET <base>/models"
// convention, parsing {"data":[...]}, {"models":[...]}, or a bare array. It is
// reused by every provider whose backend exposes that endpoint (OpenAI, Z.AI,
// OpenRouter, and Anthropic, whose /v1/models returns the same {data:[...]} shape).
// The models URL is derived from the connection's already-resolved chat URL by
// stripping chatPath, so it works for hand-built connections that set only a URL
// (no Config) and preserves any carried query string (Azure's ?api-version=).
type openAILister struct {
	chatPath   string
	modelsPath string
}

func (l openAILister) list(ctx context.Context, c *ModelConnection) ([]ModelInfo, error) {
	base := stripChatPath(c.URL, l.chatPath)
	body, err := c.doJSON(ctx, http.MethodGet, appendPath(base, l.modelsPath), nil)
	if err != nil {
		return nil, err
	}
	var wrapped struct {
		Data   []ModelInfo `json:"data"`
		Models []ModelInfo `json:"models"`
	}
	if err := json.Unmarshal(body, &wrapped); err == nil {
		if len(wrapped.Data) > 0 {
			return wrapped.Data, nil
		}
		if len(wrapped.Models) > 0 {
			return wrapped.Models, nil
		}
	}
	var bare []ModelInfo
	if err := json.Unmarshal(body, &bare); err == nil && len(bare) > 0 {
		return bare, nil
	}
	return nil, &ModelError{Type: ErrorGeneric, Message: "no models found in response"}
}
