package model

import (
	"bufio"
	"bytes"
	"context"
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
	Role       Role       `json:"role"`
	Content    string     `json:"content"`
	Name       string     `json:"name,omitempty"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
}

// FunctionDef describes a callable function exposed to the model.
type FunctionDef struct {
	Name        string      `json:"name"`
	Description string      `json:"description"`
	Parameters  interface{} `json:"parameters"`
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
	ToolChoice      string         `json:"tool_choice,omitempty"`
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
		PromptCacheHitTokens     int `json:"prompt_cache_hit_tokens"`
		CompletionTokensDetails *struct {
			ReasoningTokens int `json:"reasoning_tokens"`
		} `json:"completion_tokens_details"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
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
// carry an incremental text delta as it arrives; the terminal event (Done) is
// emitted once at end-of-stream and carries the finish reason, the fully
// assembled tool calls and the final token usage.
type StreamResponse struct {
	Content      string      `json:"content,omitempty"`
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
	Role      Role             `json:"role"`
	Content   string           `json:"content"`
	ToolCalls []streamToolCall `json:"tool_calls"`
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
		client:         &http.Client{Timeout: 5 * time.Minute},
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
		client:         &http.Client{Timeout: 30 * time.Second},
		maxAttempts:    defaultMaxAttempts,
		retryBaseDelay: defaultRetryBaseDelay,
		retryMaxDelay:  defaultRetryMaxDelay,
	}

	// Attach the provider's auth when a key is present. The exact scheme is
	// spec-driven (OpenAI/OpenRouter bearer, Anthropic x-api-key + version, Azure
	// api-key, or a Gemini-style query parameter); see providerSpec.authHeaders.
	if modelConfig.APIKey != "" {
		conn.client.Transport = &APIKeyRoundTripper{
			apiKey:     modelConfig.APIKey,
			headers:    spec.authHeaders(modelConfig.APIKey),
			queryParam: spec.authQuery(),
			transport:  conn.client.Transport,
		}
	}

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
	return rt.transport.RoundTrip(req)
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
	return c.complete(context.Background(), messages, false, nil)
}

// CompleteWithTools sends a completion request advertising the given native tools.
func (c *ModelConnection) CompleteWithTools(messages []Message, tools []ToolDef) (*CompletionResponse, error) {
	return c.complete(context.Background(), messages, false, tools)
}

// CompleteWithToolsCtx is CompleteWithTools bound to a context: the completion —
// including its HTTP request and any retry backoff — is abandoned the moment ctx
// is cancelled, so a stopped or closed session does not run to the request
// timeout leaking the goroutine and connection (issue #24).
func (c *ModelConnection) CompleteWithToolsCtx(ctx context.Context, messages []Message, tools []ToolDef) (*CompletionResponse, error) {
	return c.complete(ctx, messages, false, tools)
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

func (c *ModelConnection) CompleteWithStats(messages []Message) (*CompletionResponse, *TokenUsage, error) {
	resp, err := c.complete(context.Background(), messages, false, nil)
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
func (c *ModelConnection) buildRequest(messages []Message, stream bool, tools []ToolDef) CompletionRequest {
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
	if !(reasoning && c.spec.reasoningRejectsTemperature) {
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
		reqBody.ToolChoice = "auto"
	}
	if stream {
		reqBody.StreamOptions = &StreamOptions{IncludeUsage: true}
	}
	if c.ModelName != "" {
		reqBody.Model = c.ModelName
	}
	return reqBody
}

func (c *ModelConnection) complete(ctx context.Context, messages []Message, stream bool, tools []ToolDef) (*CompletionResponse, error) {
	reqBody := c.buildRequest(messages, stream, tools)

	jsonData, err := c.wireAdapter().buildBody(reqBody)
	if err != nil {
		return nil, &ModelError{
			Type:    ErrorGeneric,
			Message: fmt.Sprintf("failed to marshal request: %v", err),
		}
	}

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
		resp.Body.Close()
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
	reqBody := c.buildRequest(messages, true, tools)

	jsonData, err := c.wireAdapter().buildBody(reqBody)
	if err != nil {
		return "", &ModelError{
			Type:    ErrorGeneric,
			Message: fmt.Sprintf("failed to marshal request: %v", err),
		}
	}

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
	defer resp.Body.Close()

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
	return time.Duration(rand.Int64N(int64(d) + 1))
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
	defer resp.Body.Close()

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
