package model

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"math/rand/v2"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"

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
	// Truncated marks a tool call whose streamed Arguments were cut off mid-JSON
	// because the response hit max_tokens (finish_reason "length"), so the
	// assembled Arguments is non-empty but not valid JSON. It is set by the
	// streaming parsers (parseOpenAIStream, Anthropic parseStream) and lets the
	// agent layer distinguish "model emitted malformed JSON" from "the stream was
	// cut off" — driving the truncated-final salvage and the continuation
	// round-trip deterministically rather than re-sniffing the bytes (issue #390).
	// Kept off the wire (json:"-"): it is an internal assembly signal.
	Truncated bool `json:"-"`
}

// argsTruncated reports whether an assembled tool-call Arguments string looks
// cut off mid-JSON: non-empty (a no-argument call legitimately streams nothing)
// but not parseable as JSON. Callers gate this on finish_reason "length" so a
// model that simply emitted malformed JSON without hitting the token cap is not
// misread as truncated (issue #390).
func argsTruncated(args string) bool {
	if strings.TrimSpace(args) == "" {
		return false
	}
	return !json.Valid([]byte(args))
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
	// Thinking / ThinkingSignature carry an assistant turn's extended-thinking
	// block so it can be replayed on the next request. They are kept off the
	// OpenAI wire (json:"-") and are read only by the Anthropic-on-Vertex adapter,
	// which re-emits them as a thinking content block ahead of the turn's tool_use
	// blocks. Empty for every other provider and for the common text-only turn.
	Thinking          string `json:"-"`
	ThinkingSignature string `json:"-"`
	// Reasoning carries the assistant turn's chain-of-thought side channel
	// (reasoning_content / reasoning / thinking-summary) so a reasoning-only turn
	// (empty Content) is recoverable and the thinking survives a session reopen.
	// It is NOT the visible answer.
	//
	// Contract (Message is both the persistence shape and the provider wire shape,
	// so the two uses diverge here): MarshalJSON serializes Reasoning as "reasoning"
	// for PERSISTENCE — that is what round-trips it through the transcript so a
	// restored reasoning-only turn is not empty. It is deliberately NOT sent back to
	// the provider: the send path (buildRequest -> stripReasoning) clears it from
	// every outbound request, because replaying reasoning is the out-of-scope A6
	// follow-up and a stray "reasoning" input field can be rejected by strict
	// OpenAI-compatible APIs. Any new code that builds a provider request from
	// transcript messages MUST go through buildRequest (the single send choke point)
	// so this strip is applied. Empty for non-reasoning providers/turns; a
	// backward-compatible addition (old transcripts decode with an empty Reasoning).
	// See CompletionResponse.Reasoning (issue #402).
	Reasoning string `json:"reasoning,omitempty"`
	// Volatile marks a per-request trailing message that carries live, fast-changing
	// context (the git status + todo checklist) appended AFTER the transcript so the
	// stable [system + transcript] prefix stays cacheable across turns (issue #404).
	// It is an internal, send-time-only flag: it is never persisted to the transcript
	// and never serialized (json:"-"), and is read only by the Anthropic adapter so
	// the prompt-cache breakpoint lands at the end of the cacheable prefix (the last
	// non-volatile message) rather than on this tail. Empty/false for every persisted
	// message and every other provider.
	Volatile bool `json:"-"`
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
		// Reasoning is serialized (omitempty) so a reasoning-only assistant turn
		// survives a transcript round-trip; see Message.Reasoning (issue #402).
		Reasoning string `json:"reasoning,omitempty"`
	}{Role: m.Role, Name: m.Name, ToolCalls: m.ToolCalls, ToolCallID: m.ToolCallID, Reasoning: m.Reasoning}

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
		// Reasoning is the persisted/OpenRouter field name; ReasoningContent is the
		// Z.AI/GLM and DeepSeek name for the same chain-of-thought side channel. Both
		// are read so the blocking response message (parseResponse flattens
		// Choices[0].Message) and a reopened transcript both populate Reasoning
		// (issue #402).
		Reasoning        string `json:"reasoning,omitempty"`
		ReasoningContent string `json:"reasoning_content,omitempty"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return fmt.Errorf("unmarshal message: %w", err)
	}
	reasoning := raw.Reasoning
	if reasoning == "" {
		reasoning = raw.ReasoningContent
	}
	*m = Message{Role: raw.Role, Name: raw.Name, ToolCalls: raw.ToolCalls, ToolCallID: raw.ToolCallID, Reasoning: reasoning}

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
	// validate against Parameters rather than merely prompted to. This requires the
	// schema to be in the strict subset — a closed object (additionalProperties:false)
	// that lists EVERY property in "required", with optional properties expressed as
	// nullable (type union including "null") rather than omitted. A strict tool must
	// author its schema that way (see the read/grep/git/list tools and
	// validateArgs, which treats a nullable property as optional). On OpenAI a strict
	// tool also forces parallel tool calls off — see buildRequest, which sets
	// parallel_tool_calls:false whenever any advertised tool is strict. Both the
	// OpenAI-compatible wire and the Anthropic wire serialize the flag (Anthropic
	// supports per-tool strict tool use on the modern models gogent targets — see
	// anthropicTool.Strict). Gemini's functionDeclarations have no per-tool strict
	// field (structured output there is request-level via responseSchema), so the
	// Gemini adapter drops the flag but still receives the closed schema.
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
	// for providers that understand them (see provider Capabilities).
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
	// CacheTTL is the resolved Anthropic prompt-cache directive: "" (the default
	// 5-minute ephemeral cache), "1h" (the 1-hour cache), or "off" (suppress all
	// cache_control). buildRequest sets it from the model config's
	// AnthropicCacheTTL, forced to "off" when the provider does not advertise the
	// CacheControlBreakpoints capability (issue #545). It is consumed ONLY by the
	// Anthropic adapter; json:"-" keeps it off the OpenAI-compatible wire, which
	// marshals the whole request (see openAIAdapter.buildBody).
	CacheTTL string `json:"-"`
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
	// Thinking / ThinkingSignature carry the assistant turn's extended-thinking
	// block (summarized text + opaque signature) when the backend returns one
	// (Anthropic on Vertex with thinking enabled). They are round-tripped into the
	// transcript so the block can be replayed unmodified on the next turn, which
	// Anthropic requires for tool use with thinking enabled. Empty otherwise.
	Thinking          string `json:"-"`
	ThinkingSignature string `json:"-"`
	// Reasoning carries the assistant turn's accumulated chain-of-thought text from
	// a reasoning model's side channel (reasoning_content / reasoning /
	// thinking-summary). It is NOT part of the visible answer; it is retained so a
	// reasoning-only turn (empty Content) is recoverable — the agent loop reads it
	// to tell a partial/reasoning-only truncation apart from a budget exhausted on
	// unretained thinking (issue #402) — and so it can be persisted onto the
	// transcript Message (see ModelSession.sendCtx). Kept off the wire here
	// (json:"-"): it is assembled from the response, not sent back. Empty for
	// non-reasoning providers/turns.
	Reasoning string `json:"-"`
}

type Choice struct {
	Message      Message `json:"message"`
	FinishReason string  `json:"finish_reason"`
	Index        int     `json:"index"`
}

// CacheStats is the provider-agnostic prompt-cache split for one turn. ReadTokens
// and WriteTokens are both subsets of TokenUsage.PromptTokens. ReadTokens is the
// portion of the prompt served from the provider's cache at a steep discount;
// WriteTokens is the portion the provider charged a premium to WRITE into its
// cache. Only Anthropic reports (and bills) a write count — WriteTokens is 0 for
// every other provider, so the model reduces to a single read counter there.
//
// The inner json tags describe a standalone CacheStats; inside TokenUsage the
// custom (Un)MarshalJSON maps ReadTokens to the legacy "cached_tokens" key (for
// byte-identical persistence and back-compat) and WriteTokens to
// "cache_write_tokens".
type CacheStats struct {
	ReadTokens  int `json:"cache_read_tokens,omitempty"`
	WriteTokens int `json:"cache_write_tokens,omitempty"`
}

type TokenUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
	// Cache is the normalized prompt-cache read/write split (see CacheStats). It is
	// populated provider-agnostically: OpenAI-compatible backends (incl. Z.AI) via
	// the nested usage.prompt_tokens_details.cached_tokens, DeepSeek via the
	// top-level prompt_cache_hit_tokens, Gemini via cachedContentTokenCount, and
	// Anthropic via cache_read_input_tokens / cache_creation_input_tokens. It is
	// (un)marshaled flat — see MarshalJSON / UnmarshalJSON — not as a nested object.
	Cache CacheStats `json:"-"`
	// ReasoningTokens is the count of output tokens a reasoning model spent on
	// internal chain-of-thought. It is a subset of CompletionTokens (already
	// billed within it), reported under
	// usage.completion_tokens_details.reasoning_tokens. UnmarshalJSON lifts it
	// out; the own tag keeps it round-tripping through gogent's persistence.
	ReasoningTokens int `json:"reasoning_tokens,omitempty"`
}

// CachedTokens reports the prompt-cache READ token count (a subset of
// PromptTokens). It is the back-compat alias for the former CachedTokens field,
// now computed from Cache.ReadTokens.
func (u TokenUsage) CachedTokens() int { return u.Cache.ReadTokens }

// MarshalJSON serializes TokenUsage flat. The five pre-cache-write keys keep their
// historical reflection order and positions — prompt_tokens, completion_tokens,
// total_tokens, cached_tokens (=Cache.ReadTokens), reasoning_tokens — so a turn
// with no cache writes is byte-identical to gogent's prior persistence. The new
// cache_write_tokens (=Cache.WriteTokens) is omitempty and appended LAST, so it
// appears only on Anthropic write turns and never shifts an existing key.
func (u TokenUsage) MarshalJSON() ([]byte, error) {
	b, err := json.Marshal(struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
		TotalTokens      int `json:"total_tokens"`
		CachedTokens     int `json:"cached_tokens,omitempty"`
		ReasoningTokens  int `json:"reasoning_tokens,omitempty"`
		CacheWriteTokens int `json:"cache_write_tokens,omitempty"`
	}{
		PromptTokens:     u.PromptTokens,
		CompletionTokens: u.CompletionTokens,
		TotalTokens:      u.TotalTokens,
		CachedTokens:     u.Cache.ReadTokens,
		ReasoningTokens:  u.ReasoningTokens,
		CacheWriteTokens: u.Cache.WriteTokens,
	})
	if err != nil {
		return nil, fmt.Errorf("marshal token usage: %w", err)
	}
	return b, nil
}

// UnmarshalJSON parses provider token usage, normalizing the prompt-cache split
// from every shape under one roof. Cache READS resolve most-authoritative first:
// the nested OpenAI/Z.AI/OpenRouter usage.prompt_tokens_details.cached_tokens, then
// the top-level DeepSeek prompt_cache_hit_tokens, then the legacy top-level
// cached_tokens key gogent itself persists (kept so stored turns still load after
// the field became Cache.ReadTokens). Cache WRITES come only from gogent's own
// cache_write_tokens tag; providers that report a write count do so through their
// adapters (anthropicUsage.toTokenUsage), not this path.
func (u *TokenUsage) UnmarshalJSON(data []byte) error {
	type alias TokenUsage // strips methods to avoid infinite recursion
	var raw struct {
		alias
		PromptTokensDetails *struct {
			CachedTokens int `json:"cached_tokens"`
		} `json:"prompt_tokens_details"`
		PromptCacheHitTokens    int `json:"prompt_cache_hit_tokens"` // DeepSeek wire
		LegacyCachedTokens      int `json:"cached_tokens"`           // gogent-persisted reads
		CacheWriteTokens        int `json:"cache_write_tokens"`      // gogent-persisted writes
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
		u.Cache.ReadTokens = raw.PromptTokensDetails.CachedTokens
	case raw.PromptCacheHitTokens > 0:
		u.Cache.ReadTokens = raw.PromptCacheHitTokens
	case raw.LegacyCachedTokens > 0:
		u.Cache.ReadTokens = raw.LegacyCachedTokens
	}
	if raw.CacheWriteTokens > 0 {
		u.Cache.WriteTokens = raw.CacheWriteTokens
	}
	if raw.CompletionTokensDetails != nil && raw.CompletionTokensDetails.ReasoningTokens > 0 {
		u.ReasoningTokens = raw.CompletionTokensDetails.ReasoningTokens
	}
	return nil
}

// orOne returns m, or 1.0 when m is 0 — the "no special pricing" default so an
// unset cache multiplier prices tokens at face value.
func orOne(m float64) float64 {
	if m == 0 {
		return 1
	}
	return m
}

// costWeightedInput prices a turn's prompt tokens by cache tier: the full-price
// remainder (prompt minus reads and writes) plus reads at readMult plus writes at
// writeMult, rounded to the nearest whole token. ReadTokens and WriteTokens are
// subsets of prompt for every well-formed provider response, so the remainder is
// normally non-negative. A 0 multiplier means 1.0 (face value), so an unpriced
// provider reduces to raw prompt tokens. The result is floored at 0 so a malformed
// response that over-reports cached tokens (Read+Write > prompt) can never charge a
// negative cost and rewind the agent budget.
func (c CacheStats) costWeightedInput(prompt int, readMult, writeMult float64) int {
	base := prompt - c.ReadTokens - c.WriteTokens
	weighted := math.Round(float64(base) +
		float64(c.ReadTokens)*orOne(readMult) +
		float64(c.WriteTokens)*orOne(writeMult))
	return int(math.Max(0, weighted))
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
	// Thinking / ThinkingSignature are carried on the terminal (Done) event: the
	// fully accumulated extended-thinking block (text + signature) for the turn,
	// for round-tripping on the next request. Empty for turns/providers that
	// stream no thinking. See CompletionResponse.Thinking.
	Thinking          string `json:"thinking,omitempty"`
	ThinkingSignature string `json:"thinking_signature,omitempty"`
	Done              bool   `json:"done,omitempty"`
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
	TotalCacheWriteTokensIn    int
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
	// ErrorEmptyResponse is returned when the backend replies 200 OK with an empty
	// or whitespace-only body (blocking) or a stream that yields no content,
	// reasoning, tool calls, usage, or finish reason at all (streaming). It is a
	// known-transient failure mode of OpenAI-compatible gateways (OpenRouter, Z.AI,
	// vLLM, LiteLLM, …) that answer 200 then close early / send a zero-length body;
	// the blocking path retries it up to maxAttempts before surfacing it (issue #485).
	ErrorEmptyResponse ModelErrorType = "empty_response"
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
	URL string
	// StreamURL is the endpoint for streaming completions when it differs from the
	// blocking URL. It is empty for every OpenAI-compatible provider (which streams
	// from the same chat URL with stream:true) and set only where the streaming
	// route is a distinct path — Vertex AI's native Gemini API streams from
	// :streamGenerateContent?alt=sse rather than :generateContent. completeStream
	// POSTs here when non-empty, falling back to URL otherwise. See
	// NewModelConnectionFromConfig and modelURLEndpoints (provider_vertex.go).
	StreamURL string
	ModelName string
	APIType   APIType
	Config    *config.ModelConfig
	Stats     *ModelStats
	Timeout   time.Duration
	// provider carries all backend-specific behaviour (endpoints, auth, wire
	// adapter, capabilities, optional operations like model listing). It is set by
	// the constructors from the provider registry; methods delegate to it.
	provider *provider
	client   *http.Client

	// configErr, when non-nil, is a construction-time configuration error that
	// could not be returned (NewModelConnectionFromConfig has no error result).
	// It is surfaced — clearly, before any network I/O — on the first completion
	// call. Used for misconfigurations a request would otherwise turn into an
	// opaque DNS/HTTP failure, e.g. a Vertex model missing Project/Location.
	configErr error

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
		provider:       providerFor(APITypeOpenAI),
		Stats:          &ModelStats{},
		Timeout:        5 * time.Minute,
		client:         newClient(5*time.Minute, nil),
		maxAttempts:    defaultMaxAttempts,
		retryBaseDelay: defaultRetryBaseDelay,
		retryMaxDelay:  defaultRetryMaxDelay,
	}
}

// NewUnroutableConnection returns a connection that carries a deferred configErr and
// therefore fails every completion/scan call with a clear, actionable message instead
// of silently dialing the DefaultModelURL placeholder. gogent uses it when no routable
// model is configured (e.g. every configured entry was swept as unroutable at load),
// preserving the #505/#511 "no silent localhost 404" guarantee even in that case: a
// bare NewModelConnection() has configErr == nil and would target localhost, so it must
// not be used as the no-model fallback.
func NewUnroutableConnection(message string) *ModelConnection {
	conn := NewModelConnection()
	conn.configErr = &ModelError{Type: ErrorGeneric, Message: message}
	return conn
}

// NewModelConnectionFromConfig creates a model connection from config. The
// configured APIType selects the provider conventions; the endpoint may be a
// full chat-completions URL or just a base URL (or empty, to use the provider
// default), which is normalized into the concrete endpoints automatically.
func NewModelConnectionFromConfig(modelConfig *config.ModelConfig) *ModelConnection {
	p := providerFor(StringToAPIType(modelConfig.APIType))
	eps := p.endpoints.endpoints(modelConfig)

	conn := &ModelConnection{
		URL:            eps.ChatURL,
		StreamURL:      eps.StreamURL,
		ModelName:      modelConfig.Model,
		APIType:        p.apiType,
		Config:         modelConfig,
		Stats:          &ModelStats{},
		Timeout:        5 * time.Minute,
		provider:       p,
		maxAttempts:    defaultMaxAttempts,
		retryBaseDelay: defaultRetryBaseDelay,
		retryMaxDelay:  defaultRetryMaxDelay,
	}

	// Deferred config validation, surfaced clearly on the first completion/scan call
	// instead of here (the constructor has no error result) and instead of as an
	// opaque HTTP failure. Two layers: a generic routability check (an unroutable
	// entry — e.g. empty api_type AND endpoint — would otherwise silently target the
	// localhost placeholder and 404), then the provider's own check (e.g. a Vertex
	// model missing project/location). The routability check runs first so the
	// clearest, most actionable message wins.
	if err := validateRoutableConfig(modelConfig); err != nil {
		conn.configErr = err
	} else if err := p.validateConfig(modelConfig); err != nil {
		conn.configErr = err
	}

	// The provider's auth scheme builds the round-tripper (bearer / x-api-key /
	// Azure api-key / Gemini query param / Google ADC), wrapping the shared pooled
	// transport so keep-alive conns persist; a nil round-tripper means no auth is
	// injected and the shared transport is used directly (issue #19).
	conn.client = newClient(30*time.Second, p.auth.roundTripper(modelConfig))

	return conn
}

// validateRoutableConfig rejects model configs that cannot be routed, returning a
// deferred *ModelError that names the model and the missing field(s). It is the fix
// for the silent localhost:8080 fallback (issue #505): without it an entry that has
// neither an explicit endpoint nor a base-URL-deriving api_type is sent to the
// generic OpenAI provider's placeholder default and fails with an opaque 404.
//
// It rejects in two cases:
//
//  1. Routability: the endpoint is empty AND the api_type does not supply a base URL.
//     "Supplies a base URL" means a base-URL-deriving provider (provider.derivesBase:
//     zai/openrouter/anthropic/vertex*) OR the explicit "openai" api_type, whose
//     localhost default is a documented, intentional local-server target. So only an
//     empty or unrecognized api_type (both of which silently fall back to localhost)
//     is rejected. The unrecognized case echoes the raw api_type so a typo is visible.
//  2. Hosted-gateway empty model: the model is empty on a known hosted gateway
//     (openrouter/zai) where an empty model name is almost certainly a mistake. This
//     is deliberately narrow — api_type "openai" with an explicit endpoint may
//     legitimately omit the model (some local servers auto-select it), and Vertex is
//     left to its own validation.
//
// Returns nil for every valid config, including base-URL-deriving providers and local
// "openai" servers with an explicit endpoint, so existing configs are unaffected.
// ValidateModelConfig is the exported wrapper over validateRoutableConfig so callers
// outside this package (gogent's save/load/use paths, issue #532) can reject an
// unroutable config at the source — at save time, at load time, and when resolving a
// default — instead of only lazily at connection-build time. It returns the same
// model-named, field-naming *ModelError (or nil for a valid config), so there is a
// single routability rule with no duplicated logic.
func ValidateModelConfig(cfg *config.ModelConfig) error {
	return validateRoutableConfig(cfg)
}

func validateRoutableConfig(cfg *config.ModelConfig) error {
	if cfg == nil {
		return nil
	}
	rawType := strings.ToLower(strings.TrimSpace(cfg.APIType))
	resolved := StringToAPIType(cfg.APIType)

	// 1. Routability: empty endpoint with no way to determine the base URL.
	if strings.TrimSpace(cfg.Endpoint) == "" &&
		!providerFor(resolved).derivesBase && rawType != "openai" {
		if rawType == "" {
			return &ModelError{
				Type: ErrorGeneric,
				Message: fmt.Sprintf(
					"model %q is misconfigured: api_type and endpoint are both empty (cannot determine where to send requests)",
					configModelName(cfg)),
			}
		}
		return &ModelError{
			Type: ErrorGeneric,
			Message: fmt.Sprintf(
				"model %q is misconfigured: endpoint is empty and api_type %q is unrecognized (set an explicit endpoint or use a known api_type)",
				configModelName(cfg), cfg.APIType),
		}
	}

	// 2. Hosted gateway with no model name.
	if strings.TrimSpace(cfg.Model) == "" &&
		(resolved == APITypeOpenRouter || resolved == APITypeZAI) {
		return &ModelError{
			Type: ErrorGeneric,
			Message: fmt.Sprintf(
				"model %q is misconfigured: model is empty (api_type %q requires a model name)",
				configModelName(cfg), cfg.APIType),
		}
	}

	return nil
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

// adcScope is the OAuth2 scope Vertex AI access tokens are minted for. The
// calling principal needs roles/aiplatform.user on the project.
const adcScope = "https://www.googleapis.com/auth/cloud-platform"

// adcTokenSourceFunc resolves the Application Default Credentials token source.
// It is a package-level seam (defaulting to google.DefaultTokenSource) so tests
// can inject a fake source returning a static token without real GCP
// credentials. The returned oauth2.TokenSource auto-refreshes the (~1h-lived)
// access token, so a raw token is never cached. ADC search order is
// GOOGLE_APPLICATION_CREDENTIALS, then `gcloud auth application-default login`
// user creds, then the GCE/GKE/Cloud Run metadata server.
var adcTokenSourceFunc = func(ctx context.Context, scopes ...string) (oauth2.TokenSource, error) {
	return google.DefaultTokenSource(ctx, scopes...)
}

// lazyTokenSource defers ADC credential resolution to the first Token() call.
// NewModelConnectionFromConfig cannot return an error, so credentials must not
// be resolved eagerly at construction; instead the first request triggers
// resolution and any failure (no ADC configured) surfaces there — once, since
// the result is memoized. It is safe for concurrent use.
type lazyTokenSource struct {
	newTS func() (oauth2.TokenSource, error)
	once  sync.Once
	ts    oauth2.TokenSource
	err   error
}

func (l *lazyTokenSource) Token() (*oauth2.Token, error) {
	l.once.Do(func() { l.ts, l.err = l.newTS() })
	if l.err != nil {
		return nil, l.err
	}
	tok, err := l.ts.Token()
	if err != nil {
		return nil, fmt.Errorf("adc token: %w", err)
	}
	return tok, nil
}

// ADCRoundTripper authenticates Vertex AI requests with a Google Application
// Default Credentials bearer token instead of an API key. It is the ADC analogue
// of APIKeyRoundTripper: it wraps the SAME shared pooled transport so keep-alive
// connections persist across turns and sub-agent fan-out (issue #19), and on
// each request it fetches a fresh access token from tokenSource (which
// auto-refreshes when the ~1h token expires) and sets Authorization: Bearer
// <token> on a clone of the request. tokenSource is injectable so tests can use
// a static fake without real credentials.
type ADCRoundTripper struct {
	tokenSource oauth2.TokenSource
	transport   http.RoundTripper
}

func (rt *ADCRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	tok, err := rt.tokenSource.Token()
	if err != nil {
		return nil, fmt.Errorf("vertex ADC credentials not found — run `gcloud auth application-default login` or set GOOGLE_APPLICATION_CREDENTIALS: %w", err)
	}
	base := rt.transport
	if base == nil {
		base = http.DefaultTransport
	}
	// Clone before mutating headers so the caller's request (which may be retried
	// or shared) is left untouched, mirroring the standard RoundTripper contract.
	clone := req.Clone(req.Context())
	clone.Header.Set("Authorization", "Bearer "+tok.AccessToken)
	resp, err := base.RoundTrip(clone)
	if err != nil {
		return nil, fmt.Errorf("round trip: %w", err)
	}
	return resp, nil
}

// wireAdapter returns the connection's wire-format adapter, defaulting to the
// OpenAI-compatible adapter so a zero-value or hand-built connection still works.
func (c *ModelConnection) wireAdapter() adapter {
	if c.provider != nil && c.provider.adapter != nil {
		return c.provider.adapter
	}
	return openAIAdapter{}
}

// caps returns the connection's provider capabilities, defaulting to the empty
// set for a zero-value/hand-built connection without a provider.
func (c *ModelConnection) caps() Capabilities {
	if c.provider != nil {
		return c.provider.caps
	}
	return Capabilities{}
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

	var content, reasoning strings.Builder
	resp := &CompletionResponse{Role: RoleAssistant}
	for ev := range streamCh {
		if ev.Reasoning != "" {
			// Accumulate the reasoning so the assembled response retains it (issue
			// #402) in ADDITION to forwarding the delta for live rendering. Without
			// this a reasoning-only turn would collapse to an unrecoverable empty
			// string once the fire-and-forget sink returned.
			reasoning.WriteString(ev.Reasoning)
			if onReasoning != nil {
				onReasoning(ev.Reasoning)
			}
		}
		if ev.Content != "" {
			content.WriteString(ev.Content)
		}
		if ev.Done {
			// The terminal event carries the authoritative assembled tool calls,
			// finish reason, usage and any extended-thinking block to round-trip
			// (see parseOpenAIStream / anthropic parseStream).
			resp.ToolCalls = ev.ToolCalls
			if ev.FinishReason != nil {
				resp.FinishReason = *ev.FinishReason
			}
			resp.Usage = ev.Usage
			resp.Thinking = ev.Thinking
			resp.ThinkingSignature = ev.ThinkingSignature
		}
	}
	if err := <-errCh; err != nil {
		return nil, err
	}
	resp.Content = content.String()
	resp.Reasoning = reasoning.String()
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
		c.Stats.TotalCachedTokensIn += usage.CachedTokens()
		c.Stats.TotalCacheWriteTokensIn += usage.Cache.WriteTokens
		c.Stats.TotalTokensOut += usage.CompletionTokens
		c.Stats.Mutex.Unlock()
	}
	return resp, usage, nil
}

// DefaultMaxTokens is the fallback per-request output-token cap used when a
// connection has no configured MaxTokens. Exported so the agent layer can base a
// raised truncation-retry budget on the same default the request path uses when
// the connector reports no configured cap (issue #402).
const DefaultMaxTokens = 4096

// maxTokensOverrideKey is the context key carrying a per-request output-token cap
// that supersedes the connection's configured MaxTokens for one request. The
// unexported key type avoids collisions with other packages' context values.
type maxTokensOverrideKey struct{}

// WithMaxTokensOverride returns a context that makes the next model request issued
// under it use maxTokens as its output-token cap instead of the connection's
// configured MaxTokens. The agent loop uses it to retry a turn that exhausted its
// budget on reasoning with no visible output (finish_reason "length") under a
// raised cap (issue #402). A non-positive maxTokens is ignored (the context is
// returned unchanged). The override is still clamped to the provider's
// MaxTokensLimit in buildRequest, so it can never exceed what the backend accepts.
func WithMaxTokensOverride(ctx context.Context, maxTokens int) context.Context {
	if ctx == nil || maxTokens <= 0 {
		return ctx
	}
	return context.WithValue(ctx, maxTokensOverrideKey{}, maxTokens)
}

// MaxTokensOverrideFrom returns the per-request output-token override carried by
// ctx (see WithMaxTokensOverride) and whether one was set. ok is false (and n 0)
// when no override is present.
func MaxTokensOverrideFrom(ctx context.Context) (n int, ok bool) {
	if ctx == nil {
		return 0, false
	}
	v, ok := ctx.Value(maxTokensOverrideKey{}).(int)
	return v, ok
}

// MaxTokensReporter is an optional connector capability: it reports the connector's
// configured per-request output cap and the provider's hard ceiling, so the agent
// loop can compute a raised retry budget for a turn truncated by max_tokens
// (issue #402). A connector that does not implement it (e.g. a test fake) leaves
// the loop to fall back to DefaultMaxTokens with no known ceiling.
type MaxTokensReporter interface {
	// MaxTokensConfig returns the configured per-request output cap (0 when unset,
	// i.e. the DefaultMaxTokens default applies) and the provider's hard ceiling
	// (0 when there is no known limit).
	MaxTokensConfig() (configured, limit int)
}

// MaxTokensConfig reports the connection's configured output cap and the provider
// ceiling so callers can compute a raised retry budget (issue #402). It satisfies
// MaxTokensReporter.
func (c *ModelConnection) MaxTokensConfig() (configured, limit int) {
	if c.Config != nil {
		configured = c.Config.MaxTokens
	}
	return configured, c.caps().MaxTokensLimit
}

// CacheCostReporter is an optional connector capability: it reports a turn's
// cost-weighted input token count, pricing prompt-cache reads (discounted) and
// writes (Anthropic premium) at the provider's multipliers instead of counting
// every prompt token at face value. The agent budget consults it so recorded
// spend reflects the real cost of cached tokens (issue #544). A connector that
// does not implement it leaves the budget to use raw PromptTokens.
type CacheCostReporter interface {
	CostWeightedInput(u TokenUsage) int
}

// CostWeightedInput prices u's prompt tokens by cache tier using this connection's
// per-provider cache multipliers (Capabilities), overridden by any per-(provider,
// model) ModelCaps entry — the same two-axis resolution buildRequest uses for wire
// quirks. The override path is what lets DeepSeek (which rides api_type "openai"
// and so shares OpenAI's Capabilities) carry its own deeper cache discount. With
// no provider and no override every multiplier defaults to 1.0, so the result
// equals u.PromptTokens — identical budget accounting to before cache
// cost-weighting existed. It satisfies CacheCostReporter.
func (c *ModelConnection) CostWeightedInput(u TokenUsage) int {
	caps := c.caps()
	readMult, writeMult := caps.CacheReadMultiplier, caps.CacheWriteMultiplier
	if mc := resolveModelCaps(c.APIType, c.ModelName); mc.CacheReadMultiplier != nil || mc.CacheWriteMultiplier != nil {
		if mc.CacheReadMultiplier != nil {
			readMult = *mc.CacheReadMultiplier
		}
		if mc.CacheWriteMultiplier != nil {
			writeMult = *mc.CacheWriteMultiplier
		}
	}
	return u.Cache.costWeightedInput(u.PromptTokens, readMult, writeMult)
}

// stripReasoning returns messages with the retained chain-of-thought (Reasoning)
// cleared from every message, so it survives in the persisted transcript
// (Message.MarshalJSON) yet is never sent back to the provider on a later request.
// Replaying reasoning is the explicitly out-of-scope A6 follow-up (issue #402), and
// an unknown "reasoning" field on an input message can be rejected by strict
// OpenAI-compatible chat APIs. It copies lazily: when no message carries reasoning
// (the overwhelmingly common case) the original slice is returned with no
// allocation, so the hot path is unaffected.
func stripReasoning(messages []Message) []Message {
	first := -1
	for i := range messages {
		if messages[i].Reasoning != "" {
			first = i
			break
		}
	}
	if first < 0 {
		return messages
	}
	out := make([]Message, len(messages))
	copy(out, messages)
	for i := first; i < len(out); i++ {
		out[i].Reasoning = ""
	}
	return out
}

// buildRequest assembles a CompletionRequest with the connection's configured
// model, token limit and temperature applied. It is shared by the blocking and
// streaming paths so both send identical parameters; the only difference is that
// streaming additionally requests a final usage chunk via stream_options.
//
// maxTokensOverride is an optional per-request output-token cap (the first
// positive value wins): when set it replaces the configured/default MaxTokens
// before the MaxTokensLimit clamp, which is how a truncation retry raises the
// budget (issue #402). It is variadic so existing call sites pass no override and
// keep their exact behavior.
func (c *ModelConnection) buildRequest(messages []Message, stream bool, tools []ToolDef, format *ResponseFormat, maxTokensOverride ...int) CompletionRequest {
	caps := c.caps()
	maxTokens := DefaultMaxTokens
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
	// A per-request override (truncation retry, issue #402) supersedes the
	// configured/default cap before the ceiling clamp below, so a raised budget is
	// still bounded by what the backend accepts.
	for _, ov := range maxTokensOverride {
		if ov > 0 {
			maxTokens = ov
			break
		}
	}
	// Clamp to the provider's max_tokens ceiling; some backends (e.g. Z.AI) 400
	// on out-of-range values instead of capping them.
	if caps.MaxTokensLimit > 0 && maxTokens > caps.MaxTokensLimit {
		maxTokens = caps.MaxTokensLimit
	}

	reqBody := CompletionRequest{
		// Strip the retained reasoning side channel from the outbound request:
		// Reasoning is serialized for persistence (Message.MarshalJSON) but must not
		// be replayed to the provider — replay is the explicitly out-of-scope A6
		// follow-up, and some OpenAI-compatible chat APIs reject an unknown
		// "reasoning" field on input messages (issue #402).
		Messages: stripReasoning(messages),
		Stream:   stream,
		Tools:    tools,
	}

	reasoning := c.Config.IsReasoningModel()

	// Output-token cap: reasoning models on some providers (OpenAI o-series /
	// GPT-5) reject max_tokens and require max_completion_tokens.
	mt := maxTokens
	if reasoning && caps.ReasoningTokenParam == "max_completion_tokens" {
		reqBody.MaxCompletionTokens = &mt
	} else {
		reqBody.MaxTokens = &mt
	}

	// Sampling params. Drop temperature/top_p when EITHER the (provider,model)
	// capability layer says this model rejects them outright — current-gen Claude
	// 400s on the mere presence of temperature, independent of reasoning (issue
	// #543) — OR this is a reasoning model on a provider that rejects a custom
	// temperature (OpenAI reasoning tiers). Otherwise send temperature (pointer,
	// so a deliberate 0 survives) and top_p when configured. With no override the
	// resolved ModelCaps is empty, so this reduces to the prior reasoning-only gate
	// and is byte-identical for every other model.
	modelCaps := resolveModelCaps(c.APIType, c.ModelName)
	dropSampling := modelCaps.RejectsSampling || (reasoning && caps.ReasoningRejectsTemperature)
	if !dropSampling {
		t := temperature
		reqBody.Temperature = &t
		if topP > 0 {
			p := topP
			reqBody.TopP = &p
		}
	}

	// Reasoning controls, emitted only where the provider understands them.
	if reasoningEffort != "" && caps.SupportsReasoningEffort {
		reqBody.ReasoningEffort = reasoningEffort
	}
	if thinking != nil && caps.SupportsThinking {
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
	if format != nil && caps.SupportsResponseFormat {
		reqBody.ResponseFormat = format
	}
	// OpenAI structured outputs require parallel tool calls to be disabled
	// whenever any advertised tool uses a strict schema; honor that invariant so
	// a strict tool set is not rejected. The trigger keys on actual tool
	// strictness, not on the mere presence of a tool, so a non-strict tool batch
	// (e.g. several spawn_subagent calls, or read-only calls) is left at the
	// provider default and stays eligible for parallel emission. See
	// parallelToolCallsMustBeDisabled for the scoping and the issue #282 audit.
	if parallelToolCallsMustBeDisabled(caps, tools) {
		off := false
		reqBody.ParallelToolCalls = &off
	}

	if stream {
		reqBody.StreamOptions = &StreamOptions{IncludeUsage: true}
	}
	if c.ModelName != "" {
		reqBody.Model = c.ModelName
	}

	// Resolve the Anthropic prompt-cache directive (issue #545). The model config
	// chooses the breakpoint TTL (5m default / 1h) or disables caching ("off"); a
	// provider that does not advertise client-side cache_control breakpoints can
	// never emit them, so force "off" there. The Anthropic adapter is the only
	// consumer (the field is json:"-"); on every other adapter this is inert.
	reqBody.CacheTTL = c.Config.AnthropicCacheTTL()
	if caps.CacheControl != CacheControlBreakpoints {
		reqBody.CacheTTL = "off"
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
// The trigger is exactly that invariant and nothing broader — a strict tool, on a
// provider that enforces it. The issue #282 audit confirmed this is already the
// right scope rather than something to narrow: gogent's agent loop advertises
// every tool as non-strict (toolDefsFromRegistry never sets FunctionDef.Strict),
// and spawn_subagent is non-strict, so for ordinary tool sets this returns false
// and a batched-spawn turn is never forced serial by this rule. Naming the
// predicate locks that scope down (see the model tests) so a future strict tool
// can never silently disable parallel spawns. Providers without the invariant
// (e.g. Anthropic, no response_format field) leave supportsResponseFormat unset
// and are never affected.
func parallelToolCallsMustBeDisabled(caps Capabilities, tools []ToolDef) bool {
	return caps.SupportsResponseFormat && hasStrictTool(tools)
}

func (c *ModelConnection) complete(ctx context.Context, messages []Message, stream bool, tools []ToolDef, format *ResponseFormat) (*CompletionResponse, error) {
	if c.configErr != nil {
		return nil, c.configErr
	}
	ov, _ := MaxTokensOverrideFrom(ctx)
	reqBody := c.buildRequest(messages, stream, tools, format, ov)

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
			if len(bytes.TrimSpace(bodyBytes)) == 0 {
				// Empty/whitespace-only 200 from an OpenAI-compatible gateway: a transient
				// transport hiccup (early close / zero-length body), NOT a real completion.
				// Retry with backoff while attempts remain rather than break-and-parse,
				// which would otherwise unmarshal "" into `unexpected end of JSON input`
				// and abort the turn on the first attempt (issue #485).
				if attempt < attempts-1 {
					if !sleepCtx(ctx, c.backoff(attempt, retryAfter)) {
						return nil, ctxError(ctx)
					}
					continue
				}
				return nil, &ModelError{
					Type:    ErrorEmptyResponse,
					Message: fmt.Sprintf("model returned an empty response (HTTP 200, 0 bytes) after %d attempt(s)", attempts),
				}
			}
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
		c.Stats.TotalCachedTokensIn += fullResp.Usage.CachedTokens()
		c.Stats.TotalCacheWriteTokensIn += fullResp.Usage.Cache.WriteTokens
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
	if c.configErr != nil {
		return "", c.configErr
	}
	ov, _ := MaxTokensOverrideFrom(ctx)
	reqBody := c.buildRequest(messages, true, tools, nil, ov)

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

	// Stream from StreamURL when the provider's streaming route differs from the
	// blocking one (Vertex native: :streamGenerateContent?alt=sse); otherwise the
	// OpenAI-compatible path streams from the same chat URL with stream:true.
	streamURL := c.URL
	if c.StreamURL != "" {
		streamURL = c.StreamURL
	}
	req, err := http.NewRequestWithContext(ctx, "POST", streamURL, bytes.NewReader(jsonData))
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
			c.Stats.TotalCachedTokensIn += usage.CachedTokens()
			c.Stats.TotalCacheWriteTokensIn += usage.Cache.WriteTokens
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
	var reasoning strings.Builder
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
						reasoning.WriteString(r)
						streamCh <- StreamResponse{Reasoning: r, Role: ch.Delta.Role}
					} else if r := ch.Delta.Reasoning; r != "" {
						reasoning.WriteString(r)
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
	//
	// When the turn was cut off by max_tokens (finish_reason "length"), a call
	// whose accumulated Arguments is non-empty but no longer parses as JSON was
	// truncated mid-stream; flag it so the agent layer can salvage or complete it
	// deterministically rather than feed the partial JSON to validateArgs (#390).
	truncatedTurn := finishReason != nil && *finishReason == "length"
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
		args := acc.args.String()
		toolCalls = append(toolCalls, ToolCall{
			ID:        id,
			Type:      typ,
			Function:  FunctionCall{Name: acc.name, Arguments: args},
			Truncated: truncatedTurn && argsTruncated(args),
		})
	}

	// An OpenAI-compatible gateway can answer 200 then send a zero-length /
	// immediately-closed stream; parseOpenAIStream would otherwise return
	// ("", nil, nil) — a silently empty assistant turn. Treat a stream that
	// produced LITERALLY NOTHING — no content, no reasoning, no tool calls, no
	// finish reason, no usage — as an empty-response failure (issue #485). The
	// reasoning term is essential: a reasoning-model stream that streamed thinking
	// and was then cut before any finish/usage chunk is NOT empty (see
	// TestCompleteWithToolsStreamCtxPartialStreamDeliversDeltas); omitting it would
	// turn a previously-usable reasoning-only partial turn into a spurious error.
	if content.Len() == 0 && reasoning.Len() == 0 && len(toolCalls) == 0 &&
		finishReason == nil && usage == nil {
		return "", nil, &ModelError{
			Type:    ErrorEmptyResponse,
			Message: "model returned an empty response (streaming: no content, reasoning, tool calls, usage, or finish reason)",
		}
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

	// reason is the bounded, provider-agnostic rejection reason lifted from the
	// response body (error.message, see extractProviderMessage). It is spliced
	// into every branch's Message via withReason so ModelError.Error() surfaces
	// the actual cause — naming the offending field — instead of an opaque status
	// (issue #555). Empty/unparsable bodies yield "" and withReason leaves the
	// prior status-only message untouched. RawResponse still keeps the full body.
	reason := boundedReason(extractProviderMessage(response))

	switch statusCode {
	case 400:
		if strings.Contains(lowerResponse, "context") || strings.Contains(lowerResponse, "length") {
			c.Stats.Mutex.Lock()
			c.Stats.ContextWindowOverflowCount++
			c.Stats.Mutex.Unlock()
			return &ModelError{
				Type:           ErrorContextOverflow,
				HTTPStatusCode: statusCode,
				Message:        withReason("context window overflow", reason),
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
				Message:        withReason("content policy refusal", reason),
				RawResponse:    response,
			}
		}
	case 404:
		// A genuine wrong-endpoint/route 404 (the misconfiguration that escapes
		// validateRoutableConfig — e.g. a wrong but non-empty endpoint or model path).
		// Stays non-retryable (isRetryableStatus omits 404) and still counts as a
		// generic error; this only makes the message more descriptive than the catch-all.
		c.Stats.Mutex.Lock()
		c.Stats.GenericErrorCount++
		c.Stats.Mutex.Unlock()
		return &ModelError{
			Type:           ErrorGeneric,
			HTTPStatusCode: statusCode,
			Message:        withReason("not found (status 404): the endpoint or model path is wrong — check api_type/endpoint/model", reason),
			RawResponse:    response,
		}
	case 429:
		c.Stats.Mutex.Lock()
		c.Stats.RateLimitCount++
		c.Stats.Mutex.Unlock()
		return &ModelError{
			Type:           ErrorRateLimit,
			HTTPStatusCode: statusCode,
			Message:        withReason("rate limit exceeded", reason),
			RawResponse:    response,
		}
	case 504:
		c.Stats.Mutex.Lock()
		c.Stats.TimeoutCount++
		c.Stats.Mutex.Unlock()
		return &ModelError{
			Type:           ErrorTimeout,
			HTTPStatusCode: statusCode,
			Message:        withReason("gateway timeout", reason),
			RawResponse:    response,
		}
	}

	c.Stats.Mutex.Lock()
	c.Stats.GenericErrorCount++
	c.Stats.Mutex.Unlock()

	return &ModelError{
		Type:           ErrorGeneric,
		HTTPStatusCode: statusCode,
		Message:        withReason(fmt.Sprintf("unexpected error: status %d", statusCode), reason),
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

// ListModels asks the backend which models it serves (the Scan button). It is an
// optional capability: the provider supplies a modelLister strategy (the OpenAI
// "GET <base>/models" convention for OpenAI-compatible backends and Anthropic, the
// Vertex Model Garden catalog for Vertex). A provider with no lister reports "not
// supported"; callers treat that as "unknown / set the model id manually".
func (c *ModelConnection) ListModels() ([]ModelInfo, error) {
	if c.provider == nil || c.provider.lister == nil {
		return nil, &ModelError{Type: ErrorGeneric, Message: "model listing is not supported for this provider; set the model id manually"}
	}
	return c.provider.lister.list(context.Background(), c)
}

// StatsSnapshot returns a mutex-free copy of this connection's statistics.
func (c *ModelConnection) StatsSnapshot() StatsSnapshot {
	return c.Stats.Snapshot()
}
