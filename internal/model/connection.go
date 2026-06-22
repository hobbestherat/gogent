package model

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"gogent/internal/config"
)

type Role string

const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleFunction  Role = "function"
	RoleTool      Role = "tool"
)

// FunctionCall is the function payload of a native tool call.
type FunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"` // JSON-encoded object as a string (OpenAI convention)
}

// ToolCall is a native OpenAI-style tool call emitted by the assistant.
type ToolCall struct {
	ID       string       `json:"id,omitempty"`
	Type     string       `json:"type,omitempty"` // always "function"
	Function FunctionCall `json:"function"`
}

type Message struct {
	Role    Role   `json:"role"`
	Content string `json:"content"`
	// Images carries any image attachments on the message (multimodal input).
	// It is kept separate from the scalar Content text so the rest of gogent can
	// keep reading Content as "the text"; the two are fused into an OpenAI-style
	// content-parts array on the wire (see MarshalJSON) and split back apart on
	// the way in (see UnmarshalJSON). Empty for the overwhelmingly common
	// text-only message, in which case the wire form is byte-identical to before.
	Images     []ImageURL `json:"-"`
	Name       string     `json:"name,omitempty"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
}

// ImageURL is a single image attachment, matching OpenAI's image_url content
// part. URL is either a remote http(s) URL or an inline RFC 2397 data URL
// ("data:image/png;base64,...") for a pasted/dropped image (see DataURL). Detail
// is an optional provider rendering hint ("low" | "high" | "auto").
type ImageURL struct {
	URL    string `json:"url"`
	Detail string `json:"detail,omitempty"`
}

// contentPart is the wire shape of one element of an OpenAI multimodal content
// array: a text part (Text set) or an image_url part (ImageURL set).
type contentPart struct {
	Type     string    `json:"type"`
	Text     string    `json:"text,omitempty"`
	ImageURL *ImageURL `json:"image_url,omitempty"`
}

// MarshalJSON encodes the message for the OpenAI-compatible wire format. With no
// images attached it emits the plain object whose content is a scalar string —
// byte-for-byte what every text-only message has always sent. When images are
// present, content instead becomes an array of parts: a leading text part (when
// Content is non-empty) followed by one image_url part per image, which is how
// OpenAI expresses vision input and the shape the Anthropic adapter translates.
func (m Message) MarshalJSON() ([]byte, error) {
	wire := struct {
		Role       Role        `json:"role"`
		Content    interface{} `json:"content"`
		Name       string      `json:"name,omitempty"`
		ToolCalls  []ToolCall  `json:"tool_calls,omitempty"`
		ToolCallID string      `json:"tool_call_id,omitempty"`
	}{Role: m.Role, Name: m.Name, ToolCalls: m.ToolCalls, ToolCallID: m.ToolCallID}

	if len(m.Images) == 0 {
		wire.Content = m.Content
	} else {
		parts := make([]contentPart, 0, len(m.Images)+1)
		if m.Content != "" {
			parts = append(parts, contentPart{Type: "text", Text: m.Content})
		}
		for i := range m.Images {
			img := m.Images[i]
			parts = append(parts, contentPart{Type: "image_url", ImageURL: &img})
		}
		wire.Content = parts
	}
	b, err := json.Marshal(wire)
	if err != nil {
		return nil, fmt.Errorf("marshal message: %w", err)
	}
	return b, nil
}

// UnmarshalJSON decodes a message whose content may be either a scalar string
// (the common text-only case, and how transcripts have always been persisted) or
// an OpenAI multimodal parts array. Text parts are concatenated into Content and
// image_url parts collected into Images, so callers keep reading Content as "the
// text" regardless of which shape arrived.
func (m *Message) UnmarshalJSON(data []byte) error {
	var raw struct {
		Role       Role            `json:"role"`
		Content    json.RawMessage `json:"content"`
		Name       string          `json:"name,omitempty"`
		ToolCalls  []ToolCall      `json:"tool_calls,omitempty"`
		ToolCallID string          `json:"tool_call_id,omitempty"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return fmt.Errorf("unmarshal message: %w", err)
	}
	*m = Message{Role: raw.Role, Name: raw.Name, ToolCalls: raw.ToolCalls, ToolCallID: raw.ToolCallID}

	trimmed := bytes.TrimSpace(raw.Content)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return nil
	}
	if trimmed[0] != '[' {
		// Scalar string content (or a malformed scalar, surfaced as an error).
		if err := json.Unmarshal(trimmed, &m.Content); err != nil {
			return fmt.Errorf("unmarshal content: %w", err)
		}
		return nil
	}
	var parts []contentPart
	if err := json.Unmarshal(trimmed, &parts); err != nil {
		return fmt.Errorf("unmarshal content parts: %w", err)
	}
	var text strings.Builder
	for _, p := range parts {
		if p.Type == "image_url" {
			if p.ImageURL != nil {
				m.Images = append(m.Images, *p.ImageURL)
			}
			continue
		}
		text.WriteString(p.Text) // "text" parts, and unknown parts degrade to their text
	}
	m.Content = text.String()
	return nil
}

// DataURL builds an RFC 2397 base64 data: URL from a MIME type and raw image
// bytes — the canonical way to embed a pasted or dropped image inline in a
// message without a separate upload.
func DataURL(mimeType string, data []byte) string {
	return "data:" + mimeType + ";base64," + base64.StdEncoding.EncodeToString(data)
}

// UserImageMessage builds a user turn carrying optional text plus one or more
// images. Each image reference is an http(s) URL or a data: URL (see DataURL).
func UserImageMessage(text string, imageURLs ...string) Message {
	m := Message{Role: RoleUser, Content: text}
	for _, u := range imageURLs {
		m.Images = append(m.Images, ImageURL{URL: u})
	}
	return m
}

// FunctionDef describes a callable function exposed to the model.
type FunctionDef struct {
	Name        string      `json:"name"`
	Description string      `json:"description"`
	Parameters  interface{} `json:"parameters"`
	// Strict marks the parameter schema as strictly enforced (OpenAI structured
	// outputs / constrained decoding): the model's arguments are guaranteed to
	// validate against Parameters rather than merely prompted to. It requires a
	// closed schema (additionalProperties:false, every property listed in
	// required) and, on OpenAI, that parallel tool calls be disabled — see
	// buildRequest, which sets parallel_tool_calls:false whenever any advertised
	// tool is strict. Emitted only where the provider spec supports it.
	Strict bool `json:"strict,omitempty"`
}

// ResponseFormat is the OpenAI-style response_format request parameter, which
// constrains the model's free-text output. "json_object" asks for syntactically
// valid JSON; "json_schema" with a strict schema additionally guarantees the
// output validates against that schema — true structured output rather than a
// best-effort prompt. It is emitted only for providers whose spec advertises
// supportsResponseFormat (OpenAI-compatible backends); providers without the
// field (Anthropic) obtain schema-valid output through strict tool definitions
// plus tool_choice forcing instead, so the format is dropped for them.
type ResponseFormat struct {
	Type       string          `json:"type"` // "text" | "json_object" | "json_schema"
	JSONSchema *JSONSchemaSpec `json:"json_schema,omitempty"`
}

// JSONSchemaSpec is the json_schema payload of a json_schema response format: a
// named JSON Schema document the output must conform to. Strict turns on the
// provider's constrained-decoding guarantee and requires a closed schema
// (additionalProperties:false with every property required).
type JSONSchemaSpec struct {
	Name   string      `json:"name"`
	Schema interface{} `json:"schema"`
	Strict bool        `json:"strict,omitempty"`
}

// JSONSchemaResponseFormat builds a strict json_schema response format from a
// schema name and a JSON Schema document — the canonical way to request
// deterministically schema-valid structured output.
func JSONSchemaResponseFormat(name string, schema interface{}) *ResponseFormat {
	return &ResponseFormat{
		Type:       "json_schema",
		JSONSchema: &JSONSchemaSpec{Name: name, Schema: schema, Strict: true},
	}
}

// ToolDef is a native OpenAI-style tool definition sent in the request.
type ToolDef struct {
	Type     string      `json:"type"` // always "function"
	Function FunctionDef `json:"function"`
}

type CompletionRequest struct {
	Messages      []Message      `json:"messages"`
	Stream        bool           `json:"stream"`
	StreamOptions *StreamOptions `json:"stream_options,omitempty"`
	N             int            `json:"n,omitempty"`
	// Numeric sampling/limit params are pointers so a deliberate zero (e.g.
	// temperature 0) is expressible and distinguishable from "unset" — a plain
	// float32/int with omitempty silently drops a valid 0.
	Temperature *float32 `json:"temperature,omitempty"`
	TopP        *float32 `json:"top_p,omitempty"`
	MaxTokens   *int     `json:"max_tokens,omitempty"`
	// MaxCompletionTokens is the output cap for OpenAI reasoning models
	// (o-series, GPT-5), which reject the legacy max_tokens. Exactly one of
	// MaxTokens / MaxCompletionTokens is set per request (see buildRequest).
	MaxCompletionTokens *int `json:"max_completion_tokens,omitempty"`
	// ReasoningEffort and Thinking are reasoning-model controls, emitted only
	// for providers that understand them (see providerSpec capabilities).
	ReasoningEffort string         `json:"reasoning_effort,omitempty"`
	Thinking        *ThinkingParam `json:"thinking,omitempty"`
	Model           string         `json:"model,omitempty"`
	Tools           []ToolDef      `json:"tools,omitempty"`
	ToolChoice      *ToolChoice    `json:"tool_choice,omitempty"`
	// ResponseFormat constrains the model's text output to a schema (OpenAI
	// structured outputs). Set via the structured-completion entry points; gated
	// by the provider spec in buildRequest.
	ResponseFormat *ResponseFormat `json:"response_format,omitempty"`
	// ParallelToolCalls is a pointer so a deliberate false is expressible and
	// distinguishable from "unset" (which lets the provider default apply). It is
	// forced to false when any advertised tool is strict, because OpenAI rejects
	// parallel tool calls alongside strict tool schemas.
	ParallelToolCalls *bool `json:"parallel_tool_calls,omitempty"`
}

// ToolChoiceMode is the provider-independent tool-selection policy. It abstracts
// over the per-provider wire encodings: OpenAI takes a string or a function
// object, Anthropic an object with a "type" discriminator. See ToolChoice.
type ToolChoiceMode int

const (
	// ToolChoiceAuto lets the model decide whether to call a tool (the default
	// whenever tools are offered).
	ToolChoiceAuto ToolChoiceMode = iota
	// ToolChoiceNone forbids tool calls for this turn.
	ToolChoiceNone
	// ToolChoiceRequired forces the model to call some tool.
	ToolChoiceRequired
	// ToolChoiceTool forces the model to call the specific tool named in Name
	// (e.g. always structured_output).
	ToolChoiceTool
)

// ToolChoice is a typed, provider-independent tool_choice. The OpenAI wire form
// is produced by MarshalJSON (so the OpenAI-compatible adapter, which marshals
// the request struct directly, needs no special-casing); other adapters read the
// fields and emit their own encoding (see anthropicToolChoice).
type ToolChoice struct {
	Mode ToolChoiceMode
	// Name is the forced tool's name; used only when Mode is ToolChoiceTool.
	Name string
}

// ForceTool returns a ToolChoice that compels the model to call a named tool.
func ForceTool(name string) *ToolChoice { return &ToolChoice{Mode: ToolChoiceTool, Name: name} }

// MarshalJSON encodes the choice in OpenAI's tool_choice format: the bare strings
// "auto"/"none"/"required", or a {"type":"function","function":{"name":...}}
// object to force a specific tool.
func (tc ToolChoice) MarshalJSON() ([]byte, error) {
	switch tc.Mode {
	case ToolChoiceNone:
		return []byte(`"none"`), nil
	case ToolChoiceRequired:
		return []byte(`"required"`), nil
	case ToolChoiceTool:
		b, err := json.Marshal(map[string]interface{}{
			"type":     "function",
			"function": map[string]string{"name": tc.Name},
		})
		if err != nil {
			return nil, fmt.Errorf("marshal tool choice: %w", err)
		}
		return b, nil
	default:
		return []byte(`"auto"`), nil
	}
}

// ThinkingParam is the Z.AI/Anthropic-style chain-of-thought toggle, sent as
// thinking:{"type":"enabled"|"disabled"}.
type ThinkingParam struct {
	Type string `json:"type"`
}

// StreamOptions mirrors OpenAI's stream_options. include_usage asks the backend
// to emit a final SSE chunk carrying token usage (otherwise streamed responses
// report no usage at all).
type StreamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

type CompletionResponse struct {
	Content      string      `json:"content"`
	Role         Role        `json:"role"`
	FinishReason string      `json:"finish_reason"`
	Usage        *TokenUsage `json:"usage,omitempty"`
	Choices      []Choice    `json:"choices,omitempty"`
	ToolCalls    []ToolCall  `json:"-"`
}

type Choice struct {
	Message      Message `json:"message"`
	FinishReason string  `json:"finish_reason"`
	Index        int     `json:"index"`
}

type TokenUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
	// CachedTokens is the count of prompt (input) tokens that the provider served
	// from its prompt cache rather than reprocessing. It is a subset of
	// PromptTokens, billed at a steep discount, so it measures how much of the
	// stable prefix (tools + system prompt + history) was reused this turn.
	//
	// OpenAI-compatible backends (incl. Z.AI) report it nested under
	// usage.prompt_tokens_details.cached_tokens; DeepSeek-style backends report it
	// top-level as prompt_cache_hit_tokens. UnmarshalJSON reads either form. The
	// own tag keeps it round-tripping through gogent's persistence.
	CachedTokens int `json:"cached_tokens,omitempty"`
	// ReasoningTokens is the count of output tokens a reasoning model spent on
	// internal chain-of-thought. It is a subset of CompletionTokens (already
	// billed within it), reported under
	// usage.completion_tokens_details.reasoning_tokens. UnmarshalJSON lifts it
	// out; the own tag keeps it round-tripping through gogent's persistence.
	ReasoningTokens int `json:"reasoning_tokens,omitempty"`
}

// UnmarshalJSON parses provider token usage, normalizing the cached-prompt-token
// count from the two shapes OpenAI-compatible backends use (a nested
// prompt_tokens_details.cached_tokens, or a top-level prompt_cache_hit_tokens)
// into CachedTokens.
func (u *TokenUsage) UnmarshalJSON(data []byte) error {
	type alias TokenUsage // strips methods to avoid infinite recursion
	var raw struct {
		alias
		PromptTokensDetails *struct {
			CachedTokens int `json:"cached_tokens"`
		} `json:"prompt_tokens_details"`
		PromptCacheHitTokens    int `json:"prompt_cache_hit_tokens"`
		CompletionTokensDetails *struct {
			ReasoningTokens int `json:"reasoning_tokens"`
		} `json:"completion_tokens_details"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return fmt.Errorf("unmarshal token usage: %w", err)
	}
	*u = TokenUsage(raw.alias)
	switch {
	case raw.PromptTokensDetails != nil && raw.PromptTokensDetails.CachedTokens > 0:
		u.CachedTokens = raw.PromptTokensDetails.CachedTokens
	case raw.PromptCacheHitTokens > 0:
		u.CachedTokens = raw.PromptCacheHitTokens
	}
	if raw.CompletionTokensDetails != nil && raw.CompletionTokensDetails.ReasoningTokens > 0 {
		u.ReasoningTokens = raw.CompletionTokensDetails.ReasoningTokens
	}
	return nil
}

// StreamResponse is one event delivered on the streaming channel. Content/Role
// carry an incremental text delta as it arrives; Reasoning carries an
// incremental chain-of-thought (thinking) delta, emitted separately from the
// visible answer so a UI can render the model's reasoning live and fold it once
// the turn completes (issue #217). The terminal event (Done) is emitted once at
// end-of-stream and carries the finish reason, the fully assembled tool calls
// and the final token usage.
type StreamResponse struct {
	Content string `json:"content,omitempty"`
	// Reasoning is an incremental chain-of-thought delta. It is the streamed
	// counterpart of the visible Content: providers that expose reasoning emit it
	// in a side channel (OpenAI-compatible: reasoning_content / reasoning;
	// Anthropic: thinking_delta), and it is surfaced here so callers can show live
	// thinking. Empty for providers (or turns) that stream no reasoning.
	Reasoning    string      `json:"reasoning,omitempty"`
	Role         Role        `json:"role,omitempty"`
	ToolCalls    []ToolCall  `json:"tool_calls,omitempty"`
	FinishReason *string     `json:"finish_reason,omitempty"`
	Usage        *TokenUsage `json:"usage,omitempty"`
	Done         bool        `json:"done,omitempty"`
}

// streamChunk is the wire shape of a single OpenAI SSE "data:" payload. Streamed
// completions deliver content and tool calls under choices[].delta (not the
// blocking choices[].message), so this is parsed separately from
// CompletionResponse.
type streamChunk struct {
	Choices []streamChoice `json:"choices"`
	Usage   *TokenUsage    `json:"usage"`
}

type streamChoice struct {
	Delta        streamDelta `json:"delta"`
	FinishReason *string     `json:"finish_reason"`
	Index        int         `json:"index"`
}

type streamDelta struct {
	Role    Role   `json:"role"`
	Content string `json:"content"`
	// ReasoningContent / Reasoning carry the streamed chain-of-thought delta.
	// OpenAI-compatible reasoning backends disagree on the field name — Z.AI/GLM
	// and DeepSeek use reasoning_content, OpenRouter uses reasoning — so both are
	// read and whichever is populated becomes the StreamResponse.Reasoning delta
	// (issue #217). Backends that stream no reasoning leave both empty.
	ReasoningContent string           `json:"reasoning_content"`
	Reasoning        string           `json:"reasoning"`
	ToolCalls        []streamToolCall `json:"tool_calls"`
}

// streamToolCall is a tool-call fragment within a delta. The model streams a
// tool call across many chunks: the first carries id/name, the rest append
// argument text, all correlated by Index.
type streamToolCall struct {
	Index    int    `json:"index"`
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type ModelStats struct {
	Mutex                      sync.Mutex
	RequestCount               int
	SuccessCount               int
	ErrorCount                 int
	TotalTokensIn              int
	TotalCachedTokensIn        int
	TotalTokensOut             int
	TotalTimeMs                int64
	TimeoutCount               int
	ContextWindowOverflowCount int
	RefusalCount               int
	RateLimitCount             int
	GenericErrorCount          int
}

type ModelErrorType string

const (
	ErrorNone               ModelErrorType = ""
	ErrorContextOverflow    ModelErrorType = "context_overflow"
	ErrorRefusal            ModelErrorType = "refusal"
	ErrorGeneric            ModelErrorType = "generic"
	ErrorTimeout            ModelErrorType = "timeout"
	ErrorConnection         ModelErrorType = "connection"
	ErrorRateLimit          ModelErrorType = "rate_limit"
	ErrorContextLengthLimit ModelErrorType = "context_length_limit"
)

type ModelError struct {
	Type           ModelErrorType
	Message        string
	HTTPStatusCode int
	RawResponse    string
}

func (e *ModelError) Error() string {
	return fmt.Sprintf("%s: %s", e.Type, e.Message)
}

type ModelConnection struct {
	URL       string
	ModelName string
	APIType   APIType
	Config    *config.ModelConfig
	Stats     *ModelStats
	Timeout   time.Duration
	spec      providerSpec
	adapter   adapter
	client    *http.Client

	// Retry policy for transient completion failures. Defaults are set by the
	// constructors; tests may override them to keep backoff deterministic/fast.
	maxAttempts    int           // total request attempts (including the first)
	retryBaseDelay time.Duration // base for exponential backoff with full jitter
	retryMaxDelay  time.Duration // cap on any single backoff (also caps Retry-After)
}

// Default retry policy: a handful of attempts with exponential backoff capped at
// a few tens of seconds. These mirror the AWS "exponential backoff with full
// jitter" guidance and only ever fire for transient status classes.
const (
	defaultMaxAttempts    = 3
	defaultRetryBaseDelay = 500 * time.Millisecond
	defaultRetryMaxDelay  = 30 * time.Second
)

// reqBodyPool reuses the bytes.Buffer that backs the marshaled request body.
// A transcript grows turn over turn, so without pooling each send would
// allocate (and then GC) a fresh, ever-larger JSON buffer; the pool lets one
// buffer expand once and be reused across sends. sync.Pool is GC-aware, so an
// idle buffer is reclaimed after a couple of cycles rather than held forever.
var reqBodyPool = sync.Pool{
	New: func() any { return new(bytes.Buffer) },
}

func acquireReqBodyBuf() *bytes.Buffer  { return reqBodyPool.Get().(*bytes.Buffer) }
func releaseReqBodyBuf(b *bytes.Buffer) { reqBodyPool.Put(b) }

// sharedHTTPTransport is the single, tuned *http.Transport that backs every
// ModelConnection's *http.Client. An *http.Client is cheap — just a per-config
// wrapper (URL, model, key, timeout) — and safe to rebuild each turn, but the
// keep-alive connection pool it rides on is expensive to rebuild: a fresh
// transport discards every pooled TCP/TLS conn, forcing a full handshake next
// turn. Sharing one transport lets that pool persist across turns and across
// sub-agent fan-out, which all hit one host through one transport.
//
// It is cloned from http.DefaultTransport — preserving proxy-from-environment,
// dialer and TLS-handshake defaults — with only the idle-conn knobs raised:
// http.DefaultTransport leaves MaxIdleConnsPerHost at its default of 2, which
// throttles parallel requests to a single host and forces an open-then-close on
// every fan-out round beyond two in flight (issue #19). A *http.Transport is
// safe for concurrent use by many goroutines, so it is designed to be shared.
var sharedHTTPTransport = newSharedTransport()

func newSharedTransport() *http.Transport {
	base, ok := http.DefaultTransport.(*http.Transport)
	if !ok || base == nil {
		base = new(http.Transport)
	} else {
		base = base.Clone()
	}
	base.MaxIdleConns = 100
	base.MaxIdleConnsPerHost = 32
	base.IdleConnTimeout = 90 * time.Second
	base.ForceAttemptHTTP2 = true
	return base
}

// newClient builds an *http.Client that runs all model traffic over the shared,
// pooled transport. timeout is the per-request deadline; the connection pool
// itself is shared across every client so keep-alive conns persist. rt, when
// non-nil (e.g. an APIKeyRoundTripper), wraps the shared transport and becomes
// the client's Transport; otherwise the client uses the shared transport
// directly (issue #19).
func newClient(timeout time.Duration, rt http.RoundTripper) *http.Client {
	if rt == nil {
		rt = sharedHTTPTransport
	}
	return &http.Client{Transport: rt, Timeout: timeout}
}

// DefaultModelURL is the connector's neutral fallback endpoint: a local
// OpenAI-compatible server on the conventional port. This is intentionally
// generic so the connector stays reusable as a standalone library. Applications
// with environment-specific defaults (env vars, LAN hosts, ...) should resolve
// the URL themselves and pass it in via NewModelConnectionFromConfig or SetURL.
const DefaultModelURL = "http://localhost:8080/v1/chat/completions"

// NewModelConnection creates a new model connection pointed at DefaultModelURL.
func NewModelConnection() *ModelConnection {
	return &ModelConnection{
		URL:            DefaultModelURL,
		APIType:        APITypeOpenAI,
		spec:           specFor(APITypeOpenAI),
		adapter:        adapterFor(APITypeOpenAI),
		Stats:          &ModelStats{},
		Timeout:        5 * time.Minute,
		client:         newClient(5*time.Minute, nil),
		maxAttempts:    defaultMaxAttempts,
		retryBaseDelay: defaultRetryBaseDelay,
		retryMaxDelay:  defaultRetryMaxDelay,
	}
}

// NewModelConnectionFromConfig creates a model connection from config. The
// configured APIType selects the provider conventions; the endpoint may be a
// full chat-completions URL or just a base URL (or empty, to use the provider
// default), which is normalized into the concrete endpoints automatically.
func NewModelConnectionFromConfig(modelConfig *config.ModelConfig) *ModelConnection {
	apiType := StringToAPIType(modelConfig.APIType)
	spec := specFor(apiType)
	base := normalizeBaseURL(modelConfig.Endpoint, spec)

	conn := &ModelConnection{
		URL:            spec.chatURL(base),
		ModelName:      modelConfig.Model,
		APIType:        apiType,
		Config:         modelConfig,
		Stats:          &ModelStats{},
		Timeout:        5 * time.Minute,
		spec:           spec,
		adapter:        adapterFor(apiType),
		maxAttempts:    defaultMaxAttempts,
		retryBaseDelay: defaultRetryBaseDelay,
		retryMaxDelay:  defaultRetryMaxDelay,
	}

	// Attach the provider's auth when a key is present. The exact scheme is
	// spec-driven (OpenAI/OpenRouter bearer, Anthropic x-api-key + version, Azure
	// api-key, or a Gemini-style query parameter); see providerSpec.authHeaders.
	// The round-tripper wraps the shared pooled transport so keep-alive conns
	// persist regardless of auth; without a key the client uses that shared
	// transport directly (issue #19).
	var rt http.RoundTripper
	if modelConfig.APIKey != "" {
		rt = &APIKeyRoundTripper{
			apiKey:     modelConfig.APIKey,
			headers:    spec.authHeaders(modelConfig.APIKey),
			queryParam: spec.authQuery(),
			transport:  sharedHTTPTransport,
		}
	}
	conn.client = newClient(30*time.Second, rt)

	return conn
}

// APIKeyRoundTripper injects a provider's auth into every request. headers holds
// the spec-resolved set (e.g. Authorization: Bearer … for OpenAI, x-api-key +
// anthropic-version for Anthropic, or attribution headers for OpenRouter) and,
// when queryParam is set, the key is instead/also placed in that URL query
// parameter (Gemini's ?key=). When headers is empty and queryParam is unset it
// falls back to the OpenAI bearer scheme using apiKey, so a bare
// APIKeyRoundTripper{apiKey: …} keeps working.
type APIKeyRoundTripper struct {
	apiKey     string
	headers    http.Header
	queryParam string
	transport  http.RoundTripper
}

func (rt *APIKeyRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	if rt.transport == nil {
		rt.transport = http.DefaultTransport
	}
	if len(rt.headers) > 0 {
		for k, vals := range rt.headers {
			for _, v := range vals {
				req.Header.Set(k, v)
			}
		}
	} else if rt.apiKey != "" && rt.queryParam == "" {
		req.Header.Set("Authorization", "Bearer "+rt.apiKey)
	}
	if rt.queryParam != "" && rt.apiKey != "" {
		q := req.URL.Query()
		q.Set(rt.queryParam, rt.apiKey)
		req.URL.RawQuery = q.Encode()
	}
	resp, err := rt.transport.RoundTrip(req)
	if err != nil {
		return nil, fmt.Errorf("round trip: %w", err)
	}
	return resp, nil
}

// wireAdapter returns the connection's wire-format adapter, defaulting to the
// OpenAI-compatible adapter so a zero-value or hand-built connection still works.
func (c *ModelConnection) wireAdapter() adapter {
	if c.adapter != nil {
		return c.adapter
	}
	return openAIAdapter{}
}

func (c *ModelConnection) SetURL(url string) *ModelConnection {
	c.URL = url
	return c
}

func (c *ModelConnection) SetTimeout(timeout time.Duration) *ModelConnection {
	c.Timeout = timeout
	c.client.Timeout = timeout
	return c
}

func (c *ModelConnection) Complete(messages []Message) (*CompletionResponse, error) {
	return c.complete(context.Background(), messages, false, nil, nil)
}

// CompleteWithTools sends a completion request advertising the given native tools.
func (c *ModelConnection) CompleteWithTools(messages []Message, tools []ToolDef) (*CompletionResponse, error) {
	return c.complete(context.Background(), messages, false, tools, nil)
}

// CompleteStructuredCtx issues a blocking completion whose output is constrained
// to a response format (typically a strict JSON schema, see
// JSONSchemaResponseFormat) — the reliable way to obtain schema-valid output for
// programmatic consumers (issue #49). tools may be nil. The format is honored
// only on providers whose spec advertises response_format support; on others it
// is silently dropped (callers that need a hard guarantee there should force a
// strict tool via ToolChoice instead). Like CompleteWithToolsCtx the request is
// abandoned the moment ctx is cancelled.
func (c *ModelConnection) CompleteStructuredCtx(ctx context.Context, messages []Message, tools []ToolDef, format *ResponseFormat) (*CompletionResponse, error) {
	return c.complete(ctx, messages, false, tools, format)
}

// CompleteWithToolsCtx is CompleteWithTools bound to a context: the completion —
// including its HTTP request and any retry backoff — is abandoned the moment ctx
// is cancelled, so a stopped or closed session does not run to the request
// timeout leaking the goroutine and connection (issue #24).
func (c *ModelConnection) CompleteWithToolsCtx(ctx context.Context, messages []Message, tools []ToolDef) (*CompletionResponse, error) {
	return c.complete(ctx, messages, false, tools, nil)
}

func (c *ModelConnection) CompleteStream(messages []Message) (<-chan StreamResponse, <-chan error) {
	streamCh := make(chan StreamResponse, 100)
	errCh := make(chan error, 1)

	go func() {
		defer close(streamCh)
		defer close(errCh)
		_, err := c.completeStream(context.Background(), messages, nil, streamCh)
		if err != nil {
			errCh <- err
		}
	}()

	return streamCh, errCh
}

// CompleteWithToolsStreamCtx issues a streaming tool-calling completion that
// behaves like CompleteWithToolsCtx — same request, and the same fully assembled
// *CompletionResponse (content, native tool calls, token usage) — but
// additionally forwards the model's chain-of-thought (reasoning) deltas to
// onReasoning as they arrive, so a caller can render live thinking and fold it
// when the turn completes (issue #217).
//
// onReasoning may be nil, in which case reasoning deltas are discarded and this
// is a plain streamed completion. A backend (or a turn) that streams no
// reasoning simply never invokes onReasoning, so the method degrades to an
// ordinary streamed completion with no thinking shown. Like the blocking path it
// is abandoned the moment ctx is cancelled.
//
// Note: unlike the blocking complete() path this does not retry transient
// failures — a streamed response cannot be safely replayed mid-stream — so it is
// used only on the opt-in streaming-thinking path; the default loop keeps the
// retrying blocking path.
func (c *ModelConnection) CompleteWithToolsStreamCtx(ctx context.Context, messages []Message, tools []ToolDef, onReasoning ReasoningSink) (*CompletionResponse, error) {
	streamCh := make(chan StreamResponse, 100)
	errCh := make(chan error, 1)

	go func() {
		// Mirror the loop-wide panic guard (issue #8): completeStream runs on this
		// separate goroutine, OUTSIDE runLoop's recover, so a panic in stream
		// parsing would otherwise crash the whole multi-session process instead of
		// failing this one request. Contain it and surface it as an ordinary error.
		// Both channels are closed the same way as the sibling CompleteStream so a
		// future second reader cannot hang.
		defer close(errCh)
		defer close(streamCh)
		defer func() {
			if r := recover(); r != nil {
				errCh <- &ModelError{Type: ErrorGeneric, Message: fmt.Sprintf("stream panicked: %v", r)}
			}
		}()
		if _, err := c.completeStream(ctx, messages, tools, streamCh); err != nil {
			errCh <- err
		}
	}()

	var content strings.Builder
	resp := &CompletionResponse{Role: RoleAssistant}
	for ev := range streamCh {
		if ev.Reasoning != "" && onReasoning != nil {
			onReasoning(ev.Reasoning)
		}
		if ev.Content != "" {
			content.WriteString(ev.Content)
		}
		if ev.Done {
			// The terminal event carries the authoritative assembled tool calls,
			// finish reason and usage (see parseOpenAIStream / anthropic parseStream).
			resp.ToolCalls = ev.ToolCalls
			if ev.FinishReason != nil {
				resp.FinishReason = *ev.FinishReason
			}
			resp.Usage = ev.Usage
		}
	}
	if err := <-errCh; err != nil {
		return nil, err
	}
	resp.Content = content.String()
	return resp, nil
}

func (c *ModelConnection) CompleteWithStats(messages []Message) (*CompletionResponse, *TokenUsage, error) {
	resp, err := c.complete(context.Background(), messages, false, nil, nil)
	if err != nil {
		return nil, nil, err
	}
	var usage *TokenUsage
	if resp.Usage != nil {
		usage = resp.Usage
		c.Stats.Mutex.Lock()
		c.Stats.TotalTokensIn += usage.PromptTokens
		c.Stats.TotalCachedTokensIn += usage.CachedTokens
		c.Stats.TotalTokensOut += usage.CompletionTokens
		c.Stats.Mutex.Unlock()
	}
	return resp, usage, nil
}

// buildRequest assembles a CompletionRequest with the connection's configured
// model, token limit and temperature applied. It is shared by the blocking and
// streaming paths so both send identical parameters; the only difference is that
// streaming additionally requests a final usage chunk via stream_options.
func (c *ModelConnection) buildRequest(messages []Message, stream bool, tools []ToolDef, format *ResponseFormat) CompletionRequest {
	maxTokens := 4096
	var temperature, topP float32
	var reasoningEffort string
	var thinking *bool
	if c.Config != nil {
		if c.Config.MaxTokens > 0 {
			maxTokens = c.Config.MaxTokens
		}
		temperature = c.Config.Temperature
		topP = c.Config.TopP
		reasoningEffort = c.Config.ReasoningEffort
		thinking = c.Config.Thinking
	}
	// Clamp to the provider's max_tokens ceiling; some backends (e.g. Z.AI) 400
	// on out-of-range values instead of capping them.
	if c.spec.maxTokensLimit > 0 && maxTokens > c.spec.maxTokensLimit {
		maxTokens = c.spec.maxTokensLimit
	}

	reqBody := CompletionRequest{
		Messages: messages,
		Stream:   stream,
		Tools:    tools,
	}

	reasoning := c.Config.IsReasoningModel()

	// Output-token cap: reasoning models on some providers (OpenAI o-series /
	// GPT-5) reject max_tokens and require max_completion_tokens.
	mt := maxTokens
	if reasoning && c.spec.reasoningTokenParam == "max_completion_tokens" {
		reqBody.MaxCompletionTokens = &mt
	} else {
		reqBody.MaxTokens = &mt
	}

	// Sampling params. Omit them for reasoning models on providers that reject a
	// custom temperature (OpenAI reasoning tiers); otherwise send temperature
	// (pointer, so a deliberate 0 survives) and top_p when configured.
	if !reasoning || !c.spec.reasoningRejectsTemperature {
		t := temperature
		reqBody.Temperature = &t
		if topP > 0 {
			p := topP
			reqBody.TopP = &p
		}
	}

	// Reasoning controls, emitted only where the provider understands them.
	if reasoningEffort != "" && c.spec.supportsReasoningEffort {
		reqBody.ReasoningEffort = reasoningEffort
	}
	if thinking != nil && c.spec.supportsThinking {
		state := "disabled"
		if *thinking {
			state = "enabled"
		}
		reqBody.Thinking = &ThinkingParam{Type: state}
	}

	if len(tools) > 0 {
		reqBody.ToolChoice = &ToolChoice{Mode: ToolChoiceAuto}
	}

	// Structured output (issue #49): emit response_format only where the provider
	// understands it (OpenAI-compatible backends). Providers without the field
	// (Anthropic) get schema-valid output through strict tools + tool_choice
	// forcing, so the format is dropped here rather than sent and rejected.
	if format != nil && c.spec.supportsResponseFormat {
		reqBody.ResponseFormat = format
	}
	// OpenAI structured outputs require parallel tool calls to be disabled
	// whenever any advertised tool uses a strict schema; honor that invariant so
	// a strict tool set is not rejected. The trigger is deliberately narrow — it
	// keys on actual tool strictness, never on the mere presence of a tool — so a
	// non-strict tool batch (e.g. several spawn_subagent calls, or read-only
	// calls) is left at the provider default and can still be emitted in parallel.
	// See parallelToolCallsMustBeDisabled for the audit behind that scoping.
	if parallelToolCallsMustBeDisabled(c.spec, tools) {
		off := false
		reqBody.ParallelToolCalls = &off
	}

	if stream {
		reqBody.StreamOptions = &StreamOptions{IncludeUsage: true}
	}
	if c.ModelName != "" {
		reqBody.Model = c.ModelName
	}
	return reqBody
}

// hasStrictTool reports whether any advertised tool carries a strict schema.
func hasStrictTool(tools []ToolDef) bool {
	for _, t := range tools {
		if t.Function.Strict {
			return true
		}
	}
	return false
}

// parallelToolCallsMustBeDisabled reports whether this request must pin
// parallel_tool_calls:false. The OpenAI structured-outputs invariant is the only
// reason to do so: when an advertised tool uses a strict schema, OpenAI (and the
// OpenAI-compatible family that advertises supportsResponseFormat) rejects the
// request unless parallel tool calls are disabled.
//
// It is intentionally the *minimal* trigger required by that invariant — strict
// tool present, on a provider that enforces it — and nothing more. In particular,
// gogent's agent loop advertises every tool as non-strict (toolDefsFromRegistry
// never sets FunctionDef.Strict), and spawn_subagent is non-strict, so this
// returns false for ordinary tool sets and a batched-spawn turn is never forced
// serial by this rule. Providers without the invariant (e.g. Anthropic, which has
// no response_format field) leave supportsResponseFormat unset and are never
// affected, so their behaviour is unchanged.
func parallelToolCallsMustBeDisabled(spec providerSpec, tools []ToolDef) bool {
	return spec.supportsResponseFormat && hasStrictTool(tools)
}

func (c *ModelConnection) complete(ctx context.Context, messages []Message, stream bool, tools []ToolDef, format *ResponseFormat) (*CompletionResponse, error) {
	reqBody := c.buildRequest(messages, stream, tools, format)

	// Marshal the request body ONCE, before the retry loop. Only the socket send
	// needs retrying, so re-marshaling the (potentially large) transcript on every
	// attempt would needlessly multiply the marshal cost (issue #20). The body is
	// marshaled into a pooled buffer that is reused across sends rather than
	// re-allocated each turn; the bytes stay live for the whole loop and the
	// buffer is returned to the pool when complete returns.
	bodyBuf := acquireReqBodyBuf()
	defer releaseReqBodyBuf(bodyBuf)
	if err := c.wireAdapter().buildBody(reqBody, bodyBuf); err != nil {
		return nil, &ModelError{
			Type:    ErrorGeneric,
			Message: fmt.Sprintf("failed to marshal request: %v", err),
		}
	}
	jsonData := bodyBuf.Bytes()

	attempts := c.maxAttempts
	if attempts < 1 {
		attempts = 1
	}

	var resp *http.Response
	var bodyBytes []byte

	startTime := time.Now()

	for attempt := 0; attempt < attempts; attempt++ {
		req, err := http.NewRequestWithContext(ctx, "POST", c.URL, bytes.NewReader(jsonData))
		if err != nil {
			return nil, &ModelError{
				Type:    ErrorConnection,
				Message: fmt.Sprintf("failed to create request: %v", err),
			}
		}

		req.Header.Set("Content-Type", "application/json")

		resp, err = c.client.Do(req)
		if err != nil {
			// A cancelled/expired context is terminal: surface it without
			// retrying so a stopped or closed session aborts immediately.
			if ctx.Err() != nil {
				return nil, ctxError(ctx)
			}
			// Network/timeout errors are transient: retry with backoff.
			if attempt < attempts-1 {
				if !sleepCtx(ctx, c.backoff(attempt, 0)) {
					return nil, ctxError(ctx)
				}
				continue
			}
			return nil, &ModelError{
				Type:    ErrorConnection,
				Message: fmt.Sprintf("failed to connect to model: %v", err),
			}
		}

		bodyBytes, err = io.ReadAll(resp.Body)
		retryAfter, _ := parseRetryAfter(resp.Header.Get("Retry-After"), startTime)
		_ = resp.Body.Close()
		if err != nil {
			return nil, &ModelError{
				Type:    ErrorGeneric,
				Message: fmt.Sprintf("failed to read response: %v", err),
			}
		}

		if resp.StatusCode == http.StatusOK {
			break
		}

		// Fail fast on permanent errors (most 4xx); retry only transient
		// classes (408/409/429/5xx), honoring Retry-After when present.
		if !isRetryableStatus(resp.StatusCode) || attempt == attempts-1 {
			return nil, c.analyzeError(resp.StatusCode, string(bodyBytes))
		}

		if !sleepCtx(ctx, c.backoff(attempt, retryAfter)) {
			return nil, ctxError(ctx)
		}
	}

	c.Stats.Mutex.Lock()
	c.Stats.RequestCount++
	c.Stats.TotalTimeMs += time.Since(startTime).Milliseconds()
	c.Stats.Mutex.Unlock()

	fullResp, err := c.wireAdapter().parseResponse(bodyBytes)
	if err != nil {
		return nil, &ModelError{
			Type:    ErrorGeneric,
			Message: fmt.Sprintf("failed to parse response: %v", err),
		}
	}

	c.Stats.Mutex.Lock()
	c.Stats.SuccessCount++
	if fullResp.Usage != nil {
		c.Stats.TotalTokensIn += fullResp.Usage.PromptTokens
		c.Stats.TotalCachedTokensIn += fullResp.Usage.CachedTokens
		c.Stats.TotalTokensOut += fullResp.Usage.CompletionTokens
	}
	c.Stats.Mutex.Unlock()

	return fullResp, nil
}

// completeStream issues a streaming completion and forwards incremental deltas
// on streamCh, returning the fully assembled content. It reuses c.client so the
// APIKeyRoundTripper (auth header) and configured timeout apply exactly as on
// the blocking path, and asks for include_usage so token stats are populated.
func (c *ModelConnection) completeStream(ctx context.Context, messages []Message, tools []ToolDef, streamCh chan<- StreamResponse) (string, error) {
	reqBody := c.buildRequest(messages, true, tools, nil)

	// Marshal into a pooled buffer (issue #20): the bytes stay live through the
	// single request send and the buffer is returned to the pool on return.
	bodyBuf := acquireReqBodyBuf()
	defer releaseReqBodyBuf(bodyBuf)
	if err := c.wireAdapter().buildBody(reqBody, bodyBuf); err != nil {
		return "", &ModelError{
			Type:    ErrorGeneric,
			Message: fmt.Sprintf("failed to marshal request: %v", err),
		}
	}
	jsonData := bodyBuf.Bytes()

	req, err := http.NewRequestWithContext(ctx, "POST", c.URL, bytes.NewReader(jsonData))
	if err != nil {
		return "", &ModelError{
			Type:    ErrorConnection,
			Message: fmt.Sprintf("failed to create request: %v", err),
		}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")

	resp, err := c.client.Do(req)
	if err != nil {
		return "", &ModelError{
			Type:    ErrorConnection,
			Message: fmt.Sprintf("failed to connect to model: %v", err),
		}
	}
	defer func() { _ = resp.Body.Close() }()

	c.Stats.Mutex.Lock()
	c.Stats.RequestCount++
	c.Stats.Mutex.Unlock()
	startTime := time.Now()

	// A non-200 response is a JSON error body, not an SSE stream.
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		c.Stats.Mutex.Lock()
		c.Stats.TotalTimeMs += time.Since(startTime).Milliseconds()
		c.Stats.Mutex.Unlock()
		return "", c.analyzeError(resp.StatusCode, string(body))
	}

	full, usage, err := c.wireAdapter().parseStream(resp.Body, streamCh)

	c.Stats.Mutex.Lock()
	c.Stats.TotalTimeMs += time.Since(startTime).Milliseconds()
	if err == nil {
		c.Stats.SuccessCount++
		if usage != nil {
			c.Stats.TotalTokensIn += usage.PromptTokens
			c.Stats.TotalCachedTokensIn += usage.CachedTokens
			c.Stats.TotalTokensOut += usage.CompletionTokens
		}
	}
	c.Stats.Mutex.Unlock()

	return full, err
}

// parseOpenAIStream parses an OpenAI server-sent-event stream, forwarding each
// content delta on streamCh and accumulating tool-call fragments (correlated by
// index) and the trailing usage chunk. It drains to "[DONE]"/EOF so the final
// usage event is not dropped, and emits one terminal StreamResponse carrying the
// finish reason, assembled tool calls and usage. A bufio.Reader (not Scanner) is
// used so arbitrarily long SSE lines never hit the 64 KB token cap.
func parseOpenAIStream(body io.Reader, streamCh chan<- StreamResponse) (string, *TokenUsage, error) {
	reader := bufio.NewReaderSize(body, 64*1024)

	// Tool calls stream as fragments across many chunks; accumulate by index.
	type accTool struct {
		id, typ, name string
		args          strings.Builder
	}
	toolsByIndex := map[int]*accTool{}
	var order []int

	var content strings.Builder
	var usage *TokenUsage
	var finishReason *string

	for {
		line, readErr := reader.ReadString('\n')
		if data, ok := strings.CutPrefix(strings.TrimSpace(line), "data:"); ok {
			data = strings.TrimSpace(data)
			if data == "[DONE]" {
				break
			}
			var chunk streamChunk
			if err := json.Unmarshal([]byte(data), &chunk); err == nil {
				if chunk.Usage != nil {
					usage = chunk.Usage
				}
				for _, ch := range chunk.Choices {
					// Surface a reasoning (thinking) delta separately from the visible
					// answer so callers can render live chain-of-thought (issue #217).
					// reasoning_content (Z.AI/GLM, DeepSeek) and reasoning (OpenRouter)
					// are alternative names for the same channel; prefer whichever is set.
					if r := ch.Delta.ReasoningContent; r != "" {
						streamCh <- StreamResponse{Reasoning: r, Role: ch.Delta.Role}
					} else if r := ch.Delta.Reasoning; r != "" {
						streamCh <- StreamResponse{Reasoning: r, Role: ch.Delta.Role}
					}
					if ch.Delta.Content != "" {
						content.WriteString(ch.Delta.Content)
						streamCh <- StreamResponse{Content: ch.Delta.Content, Role: ch.Delta.Role}
					}
					for _, tc := range ch.Delta.ToolCalls {
						acc := toolsByIndex[tc.Index]
						if acc == nil {
							acc = &accTool{}
							toolsByIndex[tc.Index] = acc
							order = append(order, tc.Index)
						}
						if tc.ID != "" {
							acc.id = tc.ID
						}
						if tc.Type != "" {
							acc.typ = tc.Type
						}
						if tc.Function.Name != "" {
							acc.name = tc.Function.Name
						}
						acc.args.WriteString(tc.Function.Arguments)
					}
					if ch.FinishReason != nil && *ch.FinishReason != "" {
						reason := *ch.FinishReason
						finishReason = &reason
					}
				}
			}
		}
		if readErr != nil {
			if readErr == io.EOF {
				break
			}
			return content.String(), usage, &ModelError{
				Type:    ErrorGeneric,
				Message: fmt.Sprintf("error reading stream: %v", readErr),
			}
		}
	}

	// Assemble accumulated tool calls in first-seen order.
	var toolCalls []ToolCall
	for _, idx := range order {
		acc := toolsByIndex[idx]
		id := acc.id
		if id == "" {
			// vLLM omits tool_calls.id when streaming; synthesize a stable id so
			// downstream tool-result correlation still works.
			id = fmt.Sprintf("call_%d", idx)
		}
		typ := acc.typ
		if typ == "" {
			typ = "function"
		}
		toolCalls = append(toolCalls, ToolCall{
			ID:       id,
			Type:     typ,
			Function: FunctionCall{Name: acc.name, Arguments: acc.args.String()},
		})
	}

	// One authoritative end-of-stream event.
	streamCh <- StreamResponse{
		ToolCalls:    toolCalls,
		FinishReason: finishReason,
		Usage:        usage,
		Done:         true,
	}

	return content.String(), usage, nil
}

func (c *ModelConnection) analyzeError(statusCode int, response string) *ModelError {
	c.Stats.Mutex.Lock()
	c.Stats.ErrorCount++
	c.Stats.Mutex.Unlock()

	lowerResponse := strings.ToLower(response)

	switch statusCode {
	case 400:
		if strings.Contains(lowerResponse, "context") || strings.Contains(lowerResponse, "length") {
			c.Stats.Mutex.Lock()
			c.Stats.ContextWindowOverflowCount++
			c.Stats.Mutex.Unlock()
			return &ModelError{
				Type:           ErrorContextOverflow,
				HTTPStatusCode: statusCode,
				Message:        "context window overflow",
				RawResponse:    response,
			}
		}
	case 403:
		if strings.Contains(lowerResponse, "refusal") || strings.Contains(lowerResponse, "content") {
			c.Stats.Mutex.Lock()
			c.Stats.RefusalCount++
			c.Stats.Mutex.Unlock()
			return &ModelError{
				Type:           ErrorRefusal,
				HTTPStatusCode: statusCode,
				Message:        "content policy refusal",
				RawResponse:    response,
			}
		}
	case 429:
		c.Stats.Mutex.Lock()
		c.Stats.RateLimitCount++
		c.Stats.Mutex.Unlock()
		return &ModelError{
			Type:           ErrorRateLimit,
			HTTPStatusCode: statusCode,
			Message:        "rate limit exceeded",
			RawResponse:    response,
		}
	case 504:
		c.Stats.Mutex.Lock()
		c.Stats.TimeoutCount++
		c.Stats.Mutex.Unlock()
		return &ModelError{
			Type:           ErrorTimeout,
			HTTPStatusCode: statusCode,
			Message:        "gateway timeout",
			RawResponse:    response,
		}
	}

	c.Stats.Mutex.Lock()
	c.Stats.GenericErrorCount++
	c.Stats.Mutex.Unlock()

	return &ModelError{
		Type:           ErrorGeneric,
		HTTPStatusCode: statusCode,
		Message:        fmt.Sprintf("unexpected error: status %d", statusCode),
		RawResponse:    response,
	}
}

// isRetryableStatus reports whether an HTTP status denotes a transient failure
// worth retrying. Permanent client errors (400/401/403/404/422, ...) are not
// retried so config/schema mistakes fail fast instead of burning attempts.
func isRetryableStatus(code int) bool {
	switch code {
	case http.StatusRequestTimeout, // 408
		http.StatusConflict,        // 409
		http.StatusTooManyRequests: // 429
		return true
	}
	return code >= 500 && code <= 599
}

// parseRetryAfter interprets a Retry-After header, which may be either a number
// of seconds or an HTTP-date (RFC 7231). It returns the delay relative to now
// and whether a valid value was parsed. Negative/past values clamp to zero.
func parseRetryAfter(header string, now time.Time) (time.Duration, bool) {
	header = strings.TrimSpace(header)
	if header == "" {
		return 0, false
	}
	if secs, err := strconv.Atoi(header); err == nil {
		if secs < 0 {
			return 0, false
		}
		return time.Duration(secs) * time.Second, true
	}
	if t, err := http.ParseTime(header); err == nil {
		if d := t.Sub(now); d > 0 {
			return d, true
		}
		return 0, true
	}
	return 0, false
}

// backoff computes how long to wait before the next attempt. A server-provided
// Retry-After (capped by retryMaxDelay) takes precedence; otherwise it uses
// exponential backoff with full jitter: a uniform random delay in [0, base*2^n],
// capped at retryMaxDelay (AWS "exponential backoff and jitter").
func (c *ModelConnection) backoff(attempt int, retryAfter time.Duration) time.Duration {
	maxDelay := c.retryMaxDelay
	if retryAfter > 0 {
		if maxDelay > 0 && retryAfter > maxDelay {
			return maxDelay
		}
		return retryAfter
	}
	base := c.retryBaseDelay
	if base <= 0 {
		return 0
	}
	d := base << attempt
	if d <= 0 || (maxDelay > 0 && d > maxDelay) { // d <= 0 guards shift overflow
		d = maxDelay
	}
	if d <= 0 {
		return 0
	}
	return time.Duration(rand.Int64N(int64(d) + 1)) //nolint:gosec // jitter only, not security-sensitive
}

// sleepCtx waits for d, or until ctx is cancelled, whichever comes first. It
// returns true if the full delay elapsed and false if the context was cancelled,
// so retry backoff is promptly abortable instead of blocking for the whole delay.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}

// ctxError wraps a cancelled/expired context as a connection-class ModelError so
// callers see a uniform error type when work is aborted (issue #24).
func ctxError(ctx context.Context) *ModelError {
	return &ModelError{
		Type:    ErrorConnection,
		Message: fmt.Sprintf("request cancelled: %v", ctx.Err()),
	}
}

func (c *ModelConnection) GetStats() *ModelStats {
	c.Stats.Mutex.Lock()
	defer c.Stats.Mutex.Unlock()
	return c.Stats
}

// modelsURL derives the provider's model-listing endpoint from the configured
// chat-completions URL (e.g. ".../chat/completions" -> ".../models"), honoring
// the provider-specific path layout. It strips the chat path from the URL's
// path component and re-derives via the spec, so a carried query string (Azure's
// ?api-version=) and non-/v1 layouts are handled the same way as construction.
func (c *ModelConnection) modelsURL() string {
	spec := c.spec
	if spec.chatPath == "" {
		spec = specFor(APITypeOpenAI)
	}
	return spec.modelsURL(stripChatPath(c.URL, spec.chatPath))
}

// ListModels asks the backend which models it serves, using the OpenAI/OpenRouter
// "GET /v1/models" convention. It is an optional capability: local servers that
// do not implement the endpoint simply return an error, which callers can treat
// as "unknown / use configured model".
func (c *ModelConnection) ListModels() ([]ModelInfo, error) {
	req, err := http.NewRequest("GET", c.modelsURL(), nil)
	if err != nil {
		return nil, &ModelError{Type: ErrorConnection, Message: fmt.Sprintf("failed to create models request: %v", err)}
	}
	req.Header.Set("Accept", "application/json")

	resp, err := c.client.Do(req)
	if err != nil {
		return nil, &ModelError{Type: ErrorConnection, Message: fmt.Sprintf("failed to list models: %v", err)}
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, &ModelError{Type: ErrorGeneric, Message: fmt.Sprintf("failed to read models response: %v", err)}
	}
	if resp.StatusCode != http.StatusOK {
		return nil, c.analyzeError(resp.StatusCode, string(body))
	}

	// Most providers (OpenAI, OpenRouter, llama.cpp) wrap the list in {"data":[...]}.
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

	// Fallback: some servers return a bare JSON array.
	var bare []ModelInfo
	if err := json.Unmarshal(body, &bare); err == nil && len(bare) > 0 {
		return bare, nil
	}

	return nil, &ModelError{Type: ErrorGeneric, Message: "no models found in response"}
}

// StatsSnapshot returns a mutex-free copy of this connection's statistics.
func (c *ModelConnection) StatsSnapshot() StatsSnapshot {
	return c.Stats.Snapshot()
}
