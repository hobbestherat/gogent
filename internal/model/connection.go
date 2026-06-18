package model

import (
	"bufio"
	"bytes"
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
	Messages      []Message `json:"messages"`
	Stream        bool      `json:"stream"`
	N             int       `json:"n,omitempty"`
	Temperature   float32   `json:"temperature,omitempty"`
	TopP          float32   `json:"top_p,omitempty"`
	MaxTokens     int       `json:"max_tokens,omitempty"`
	ContextLength int       `json:"context_length,omitempty"`
	Model         string    `json:"model,omitempty"`
	Tools         []ToolDef `json:"tools,omitempty"`
	ToolChoice    string    `json:"tool_choice,omitempty"`
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
}

type StreamResponse struct {
	Content      string      `json:"content"`
	Role         Role        `json:"role"`
	FinishReason *string     `json:"finish_reason,omitempty"`
	Usage        *TokenUsage `json:"usage,omitempty"`
	Done         bool        `json:"done"`
	Choices      []Choice    `json:"choices,omitempty"`
}

type ModelStats struct {
	Mutex                      sync.Mutex
	RequestCount               int
	SuccessCount               int
	ErrorCount                 int
	TotalTokensIn              int
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
		client:         &http.Client{Timeout: 30 * time.Second},
		maxAttempts:    defaultMaxAttempts,
		retryBaseDelay: defaultRetryBaseDelay,
		retryMaxDelay:  defaultRetryMaxDelay,
	}

	// Add API key header if present
	if modelConfig.APIKey != "" {
		conn.client.Transport = &APIKeyRoundTripper{
			apiKey:    modelConfig.APIKey,
			transport: conn.client.Transport,
		}
	}

	return conn
}

// APIKeyRoundTripper adds API key header to requests
type APIKeyRoundTripper struct {
	apiKey    string
	transport http.RoundTripper
}

func (rt *APIKeyRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	if rt.transport == nil {
		rt.transport = http.DefaultTransport
	}
	req.Header.Set("Authorization", "Bearer "+rt.apiKey)
	return rt.transport.RoundTrip(req)
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
	return c.complete(messages, false, nil)
}

// CompleteWithTools sends a completion request advertising the given native tools.
func (c *ModelConnection) CompleteWithTools(messages []Message, tools []ToolDef) (*CompletionResponse, error) {
	return c.complete(messages, false, tools)
}

func (c *ModelConnection) CompleteStream(messages []Message) (<-chan StreamResponse, <-chan error) {
	streamCh := make(chan StreamResponse, 100)
	errCh := make(chan error, 1)

	go func() {
		defer close(streamCh)
		defer close(errCh)
		_, err := c.completeStream(messages, streamCh)
		if err != nil {
			errCh <- err
		}
	}()

	return streamCh, errCh
}

func (c *ModelConnection) CompleteWithStats(messages []Message) (*CompletionResponse, *TokenUsage, error) {
	resp, err := c.complete(messages, false, nil)
	if err != nil {
		return nil, nil, err
	}
	var usage *TokenUsage
	if resp.Usage != nil {
		usage = resp.Usage
		c.Stats.Mutex.Lock()
		c.Stats.TotalTokensIn += usage.PromptTokens
		c.Stats.TotalTokensOut += usage.CompletionTokens
		c.Stats.Mutex.Unlock()
	}
	return resp, usage, nil
}

func (c *ModelConnection) complete(messages []Message, stream bool, tools []ToolDef) (*CompletionResponse, error) {
	maxTokens := 4096
	contextLength := 0 // 0 => omitted; let the server use the model's full window
	var temperature float32
	if c.Config != nil {
		if c.Config.MaxTokens > 0 {
			maxTokens = c.Config.MaxTokens
		}
		temperature = c.Config.Temperature
	}
	// Clamp to the provider's max_tokens ceiling; some backends (e.g. Z.AI) 400
	// on out-of-range values instead of capping them.
	if c.spec.maxTokensLimit > 0 && maxTokens > c.spec.maxTokensLimit {
		maxTokens = c.spec.maxTokensLimit
	}
	reqBody := CompletionRequest{
		Messages:      messages,
		Stream:        stream,
		MaxTokens:     maxTokens,
		ContextLength: contextLength,
		Temperature:   temperature,
		Tools:         tools,
	}
	if len(tools) > 0 {
		reqBody.ToolChoice = "auto"
	}

	// Add model name if specified
	if c.ModelName != "" {
		reqBody.Model = c.ModelName
	}

	jsonData, err := json.Marshal(reqBody)
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
		req, err := http.NewRequest("POST", c.URL, bytes.NewReader(jsonData))
		if err != nil {
			return nil, &ModelError{
				Type:    ErrorConnection,
				Message: fmt.Sprintf("failed to create request: %v", err),
			}
		}

		req.Header.Set("Content-Type", "application/json")

		resp, err = c.client.Do(req)
		if err != nil {
			// Network/timeout errors are transient: retry with backoff.
			if attempt < attempts-1 {
				time.Sleep(c.backoff(attempt, 0))
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

		time.Sleep(c.backoff(attempt, retryAfter))
	}

	c.Stats.Mutex.Lock()
	c.Stats.RequestCount++
	c.Stats.TotalTimeMs += time.Since(startTime).Milliseconds()
	c.Stats.Mutex.Unlock()

	var fullResp CompletionResponse
	if err := json.Unmarshal(bodyBytes, &fullResp); err != nil {
		return nil, &ModelError{
			Type:    ErrorGeneric,
			Message: fmt.Sprintf("failed to parse response: %v", err),
		}
	}

	if len(fullResp.Choices) > 0 {
		fullResp.Content = fullResp.Choices[0].Message.Content
		fullResp.Role = fullResp.Choices[0].Message.Role
		fullResp.FinishReason = fullResp.Choices[0].FinishReason
		fullResp.ToolCalls = fullResp.Choices[0].Message.ToolCalls
	}

	c.Stats.Mutex.Lock()
	c.Stats.SuccessCount++
	if fullResp.Usage != nil {
		c.Stats.TotalTokensIn += fullResp.Usage.PromptTokens
		c.Stats.TotalTokensOut += fullResp.Usage.CompletionTokens
	}
	c.Stats.Mutex.Unlock()

	return &fullResp, nil
}

func (c *ModelConnection) completeStream(messages []Message, streamCh chan<- StreamResponse) (string, error) {
	reqBody := CompletionRequest{
		Messages: messages,
		Stream:   true,
	}

	// Add model name if specified
	if c.ModelName != "" {
		reqBody.Model = c.ModelName
	}

	jsonData, err := json.Marshal(reqBody)
	if err != nil {
		return "", &ModelError{
			Type:    ErrorGeneric,
			Message: fmt.Sprintf("failed to marshal request: %v", err),
		}
	}

	req, err := http.NewRequest("POST", c.URL, bytes.NewReader(jsonData))
	if err != nil {
		return "", &ModelError{
			Type:    ErrorConnection,
			Message: fmt.Sprintf("failed to create request: %v", err),
		}
	}

	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: c.Timeout}
	resp, err := client.Do(req)
	if err != nil {
		return "", &ModelError{
			Type:    ErrorConnection,
			Message: fmt.Sprintf("failed to connect to model: %v", err),
		}
	}
	defer resp.Body.Close()

	c.Stats.Mutex.Lock()
	c.Stats.RequestCount++
	startTime := time.Now()
	c.Stats.Mutex.Unlock()

	scanner := bufio.NewScanner(resp.Body)
	var fullResponse string
	var usage *TokenUsage
	var finishReason *string

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}

		if !strings.HasPrefix(line, "data: ") {
			continue
		}

		jsonData := strings.TrimPrefix(line, "data: ")
		if jsonData == "[DONE]" {
			break
		}

		var streamResp StreamResponse
		if err := json.Unmarshal([]byte(jsonData), &streamResp); err != nil {
			continue
		}

		if streamResp.Content != "" {
			fullResponse += streamResp.Content
			streamCh <- streamResp
		}

		if streamResp.Usage != nil {
			usage = streamResp.Usage
		}

		if streamResp.FinishReason != nil {
			reason := *streamResp.FinishReason
			finishReason = &reason
		}

		if streamResp.Done || (finishReason != nil && *finishReason != "") {
			break
		}
	}

	if err := scanner.Err(); err != nil {
		return "", &ModelError{
			Type:    ErrorGeneric,
			Message: fmt.Sprintf("error reading stream: %v", err),
		}
	}

	c.Stats.Mutex.Lock()
	c.Stats.TotalTimeMs += time.Since(startTime).Milliseconds()
	if resp.StatusCode == http.StatusOK {
		c.Stats.SuccessCount++
		if usage != nil {
			c.Stats.TotalTokensIn += usage.PromptTokens
			c.Stats.TotalTokensOut += usage.CompletionTokens
		}
	}
	c.Stats.Mutex.Unlock()

	if resp.StatusCode != http.StatusOK {
		return "", c.analyzeError(resp.StatusCode, fullResponse)
	}

	return fullResponse, nil
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

func (c *ModelConnection) GetStats() *ModelStats {
	c.Stats.Mutex.Lock()
	defer c.Stats.Mutex.Unlock()
	return c.Stats
}

// modelsURL derives the provider's model-listing endpoint from the configured
// chat-completions URL (e.g. ".../chat/completions" -> ".../models"), honoring
// the provider-specific path layout.
func (c *ModelConnection) modelsURL() string {
	spec := c.spec
	if spec.chatPath == "" {
		spec = specFor(APITypeOpenAI)
	}
	u := strings.TrimRight(c.URL, "/")
	if i := strings.LastIndex(u, spec.chatPath); i >= 0 {
		return u[:i] + spec.modelsPath
	}
	return u + spec.modelsPath
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
