package model

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// adapter encapsulates a provider's wire format. gogent's internal request and
// response types are OpenAI-shaped (a scalar Message.Content for text, optional
// Message.Images for multimodal input, plus OpenAI-style tool calls), which
// serves as the lingua franca; an adapter translates that internal shape to and
// from one provider's concrete protocol:
//
//   - buildBody marshals an internal CompletionRequest into the provider's
//     request JSON,
//   - parseResponse maps a blocking response back into a CompletionResponse,
//   - parseStream maps a streaming (SSE) response onto the internal delta channel.
//
// Authentication is deliberately NOT an adapter concern: it lives on the
// provider's auth scheme (see keyAuth / adcAuth) because providers that share one
// wire adapter still authenticate differently (OpenAI bearer vs. Azure api-key
// vs. Gemini query-param vs. Vertex ADC), and some add static headers (OpenRouter
// attribution).
//
// OpenAI-compatible providers (incl. Z.AI, OpenRouter, Gemini's OpenAI-compat
// layer and local servers) share openAIAdapter; genuinely different protocols
// get their own. The adapter is selected from the APIType (see adapterFor),
// which is the seam to extend for a new provider family.
type adapter interface {
	// buildBody marshals an internal CompletionRequest into the provider's
	// request JSON, writing into buf. The caller owns buf (and may pool it,
	// see acquireReqBodyBuf); buildBody resets buf first so a pooled buffer is
	// safe to hand in directly. Returning the bytes via the caller-owned buffer
	// — rather than allocating a fresh one per call — lets a large, growing
	// transcript be marshaled into a single reused buffer across turns instead
	// of being re-allocated and GC'd on every send (issue #20).
	buildBody(req CompletionRequest, buf *bytes.Buffer) error
	parseResponse(body []byte) (*CompletionResponse, error)
	parseStream(body io.Reader, streamCh chan<- StreamResponse) (string, *TokenUsage, error)
}

// adapterFor returns the wire-format adapter registered for an APIType, defaulting
// to the OpenAI-compatible adapter for unknown/empty types. The adapter is owned
// by the provider (see provider.adapter); this is a thin accessor over the
// registry used by callers/tests that only need the wire format.
func adapterFor(t APIType) adapter {
	return providerFor(t).adapter
}

// encodeJSON serializes v into buf as compact JSON, reusing buf's existing
// capacity. It is byte-identical to json.Marshal(v) for gogent's request types
// (the encoder uses the same HTML escaping) while letting the caller own — and
// pool — the destination buffer. json.Encoder.Encode appends a trailing newline;
// it is trimmed so the wire body stays exactly what json.Marshal would have
// produced.
func encodeJSON(buf *bytes.Buffer, v any) error {
	buf.Reset()
	if err := json.NewEncoder(buf).Encode(v); err != nil {
		return fmt.Errorf("encode json: %w", err)
	}
	if b := buf.Bytes(); len(b) > 0 && b[len(b)-1] == '\n' {
		buf.Truncate(buf.Len() - 1)
	}
	return nil
}

// ---------------------------------------------------------------------------
// OpenAI-compatible adapter (OpenAI, Z.AI, Gemini compat layer, local servers)
// ---------------------------------------------------------------------------

type openAIAdapter struct{}

func (openAIAdapter) buildBody(req CompletionRequest, buf *bytes.Buffer) error {
	return encodeJSON(buf, req)
}

func (openAIAdapter) parseResponse(body []byte) (*CompletionResponse, error) {
	var resp CompletionResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("unmarshal response: %w", err)
	}
	// The blocking OpenAI response nests the message under choices[0]; flatten the
	// first choice onto the top-level fields gogent reads.
	if len(resp.Choices) > 0 {
		resp.Content = resp.Choices[0].Message.Content
		resp.Role = resp.Choices[0].Message.Role
		resp.FinishReason = resp.Choices[0].FinishReason
		resp.ToolCalls = resp.Choices[0].Message.ToolCalls
		// Retain the turn's chain-of-thought side channel: Message.UnmarshalJSON has
		// already folded reasoning_content (GLM/DeepSeek) and reasoning (OpenRouter)
		// into Reasoning, so a reasoning-only blocking turn is recoverable (issue #402).
		resp.Reasoning = resp.Choices[0].Message.Reasoning
		// Synthesize a stable id for any tool call the backend returned without one,
		// mirroring parseOpenAIStream: some OpenAI-compatible backends (e.g. vLLM)
		// omit tool_calls.id, and every downstream consumer correlates a tool result
		// to its call by id — so an empty id would leave the assistant tool_calls
		// unanswerable and the transcript invalid (issue #390).
		for i := range resp.ToolCalls {
			if resp.ToolCalls[i].ID == "" {
				resp.ToolCalls[i].ID = fmt.Sprintf("call_%d", i)
			}
		}
	}
	return &resp, nil
}

func (openAIAdapter) parseStream(body io.Reader, streamCh chan<- StreamResponse) (string, *TokenUsage, error) {
	return parseOpenAIStream(body, streamCh)
}

// ---------------------------------------------------------------------------
// Anthropic Messages adapter
// ---------------------------------------------------------------------------

// anthropicVersion is the Messages API version pinned via the anthropic-version
// header (required on every request); the Anthropic provider attaches it as a
// static auth header (see provider_anthropic.go, keyAuth.extraHeaders).
const anthropicVersion = "2023-06-01"

// anthropicAdapter speaks the Anthropic Messages wire format. The same adapter
// serves both the direct Anthropic API and Claude on Google Vertex AI; the
// vertex flag selects the Vertex body shape — the model name is omitted (it
// rides in the URL path), the API version is sent as the anthropic_version body
// field instead of a header, and extended thinking is emitted as adaptive
// thinking. Sampling params (temperature/top_p) are forwarded identically on both
// paths; whether a given model accepts them is decided upstream in buildRequest
// via the (provider,model) capability layer (resolveModelCaps), not here — so
// current-gen Claude, which rejects them, simply arrives with nil pointers that
// omitempty drops (issue #543). Prompt-cache breakpoints (cache_control on the
// system block + the end of the cacheable prefix) are emitted on BOTH paths
// (issue #404). See buildBody.
type anthropicAdapter struct{ vertex bool }

// anthropicThinking is the Messages-API thinking control. For Claude on Vertex,
// gogent emits adaptive thinking ({"type":"adaptive"}), which lets the model
// decide when and how much to reason and is the only mode current Claude models
// accept (the older {"type":"enabled","budget_tokens":N} form is rejected). The
// display field opts the streamed reasoning back in as a summary so gogent's
// live thinking view (issue #217) keeps working.
type anthropicThinking struct {
	Type    string `json:"type"`
	Display string `json:"display,omitempty"`
}

// anthropicCacheControl marks a content block as a prompt-cache breakpoint. Both
// the direct Anthropic Messages API and Vertex AI support manual (cache_control)
// prompt caching; gogent places a 5-minute ephemeral breakpoint on the system
// prompt and at the end of the cacheable prefix so a growing agent transcript is
// largely served from cache across turns (issue #404).
type anthropicCacheControl struct {
	Type string `json:"type"`
	// Ttl selects the cache lifetime: omitted = the default 5-minute ephemeral
	// cache; "1h" = the 1-hour cache (a 2× write premium). Anthropic accepts ttl
	// only as "5m" or "1h"; gogent emits it solely for "1h" so the default request
	// stays byte-identical to before the TTL knob existed (issue #545).
	Ttl string `json:"ttl,omitempty"`
}

// Anthropic prompt-cache breakpoint policy (issue #545).
const (
	// anthropicCacheLookback is Anthropic's cache-read lookback window in content
	// blocks: a breakpoint finds a prior cache write only when that write lies
	// within this many blocks BEFORE the breakpoint. It is the constraint
	// cacheBreakpointSpacing is sized against.
	anthropicCacheLookback = 20
	// cacheBreakpointSpacing is the minimum block-distance between successive
	// transcript breakpoints. Held a margin under anthropicCacheLookback so the
	// chain of breakpoints always keeps a recent cache write inside the next
	// request's lookback window even on a turn that appended many content blocks (a
	// turn with M parallel tool calls adds ~2M+1 blocks). Small turns whose whole
	// stable prefix is shorter than this emit no extra breakpoint, preserving the
	// prior single-transcript-breakpoint behavior byte-for-byte.
	cacheBreakpointSpacing = anthropicCacheLookback - 4 // 16
	// maxAnthropicBreakpoints is the hard API ceiling on cache_control breakpoints
	// per request (system + transcript). Anthropic 400s on more than four.
	maxAnthropicBreakpoints = 4
)

// anthropicCacheCtl builds a cache_control breakpoint for the given resolved TTL
// directive ("" / "5m" → default 5-minute ephemeral; "1h" → 1-hour ephemeral).
// The "off" case is handled by the caller (which omits the breakpoint entirely),
// so it is treated as the default here defensively.
func anthropicCacheCtl(ttl string) *anthropicCacheControl {
	cc := &anthropicCacheControl{Type: "ephemeral"}
	if ttl == "1h" {
		cc.Ttl = "1h"
	}
	return cc
}

// anthropicSystemBlock is one system-prompt text block. Both the direct Anthropic
// and Vertex bodies send the system prompt as a block array (rather than a bare
// string) so a cache_control breakpoint can ride on it — Anthropic accepts
// cache_control only on a content block, and the direct Messages API accepts the
// block-array system form (issue #404).
type anthropicSystemBlock struct {
	Type         string                 `json:"type"`
	Text         string                 `json:"text"`
	CacheControl *anthropicCacheControl `json:"cache_control,omitempty"`
}

// anthropicRequest is the POST /v1/messages body. Unlike chat-completions it
// hoists the system prompt to the top level, requires max_tokens, and carries
// content-block arrays per message.
type anthropicRequest struct {
	// Model is omitted on Vertex (the model name lives in the URL path); on the
	// direct API it is always set, so omitempty is byte-identical there.
	Model string `json:"model,omitempty"`
	// AnthropicVersion carries the Messages-API version in the body. It is set
	// only on Vertex (value "vertex-2023-10-16"); the direct API pins the version
	// via the anthropic-version header instead, leaving this empty.
	AnthropicVersion string `json:"anthropic_version,omitempty"`
	MaxTokens        int    `json:"max_tokens"`
	// System carries the system prompt. It is a []anthropicSystemBlock on both the
	// direct API and Vertex so a cache_control breakpoint can ride on it (issue
	// #404); interface{} is retained for the omitempty nil case (no system message)
	// and back-compat with any caller that still assigns a scalar string.
	System      interface{}        `json:"system,omitempty"`
	Messages    []anthropicMessage `json:"messages"`
	Tools       []anthropicTool    `json:"tools,omitempty"`
	ToolChoice  interface{}        `json:"tool_choice,omitempty"`
	Temperature *float32           `json:"temperature,omitempty"`
	TopP        *float32           `json:"top_p,omitempty"`
	Thinking    *anthropicThinking `json:"thinking,omitempty"`
	Stream      bool               `json:"stream,omitempty"`
}

type anthropicMessage struct {
	Role    string             `json:"role"`
	Content []anthropicContent `json:"content"`
}

// anthropicContent is one content block. Distinct block types reuse the struct;
// omitempty keeps each block to only the fields its type defines.
type anthropicContent struct {
	Type string `json:"type"`
	// text
	Text string `json:"text,omitempty"`
	// thinking (replayed assistant reasoning; signature must be unmodified)
	Thinking  string `json:"thinking,omitempty"`
	Signature string `json:"signature,omitempty"`
	// tool_use
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`
	// tool_result
	ToolUseID string `json:"tool_use_id,omitempty"`
	Content   string `json:"content,omitempty"`
	// image
	Source *anthropicImageSource `json:"source,omitempty"`
	// prompt-cache breakpoint (placed on the last block of the cacheable prefix)
	CacheControl *anthropicCacheControl `json:"cache_control,omitempty"`
}

// anthropicImageSource is the source of an Anthropic image block: an inline
// base64 payload ("type":"base64" with media_type + data) or a remote
// ("type":"url") reference.
type anthropicImageSource struct {
	Type      string `json:"type"`
	MediaType string `json:"media_type,omitempty"`
	Data      string `json:"data,omitempty"`
	URL       string `json:"url,omitempty"`
}

type anthropicTool struct {
	Name        string      `json:"name"`
	Description string      `json:"description,omitempty"`
	InputSchema interface{} `json:"input_schema"`
	// Strict opts the tool into Anthropic's strict tool use (structured outputs):
	// the model's arguments are guaranteed to validate against input_schema rather
	// than merely prompted to. Anthropic accepts it on the tool definition the same
	// way OpenAI does (supported on the modern models gogent targets), so a tool
	// marked strict gets the guarantee on Anthropic too, not only on the
	// OpenAI-compatible wire (issue #359). Omitted when false so non-strict tools
	// and older callers are byte-identical to before.
	Strict bool `json:"strict,omitempty"`
}

func (a anthropicAdapter) buildBody(req CompletionRequest, buf *bytes.Buffer) error {
	out := anthropicRequest{
		Stream: req.Stream,
		// Sampling params are forwarded verbatim on BOTH the direct and Vertex
		// paths. Whether a model accepts them is decided UPSTREAM in buildRequest
		// via the (provider,model) capability layer (resolveModelCaps): current-gen
		// Claude rejects temperature/top_p, so those pointers arrive nil here and
		// omitempty drops them from the wire body. The adapter no longer makes that
		// decision — it is data (model_overrides.go), not a per-instance branch
		// (issue #543).
		Temperature: req.Temperature,
		TopP:        req.TopP,
	}

	if a.vertex {
		// Vertex shape: the model name rides in the URL path (so it is omitted from
		// the body) and the API version travels in the body. Extended thinking, when
		// enabled, is emitted as adaptive thinking.
		out.AnthropicVersion = vertexAnthropicVersion
		if req.Thinking != nil && req.Thinking.Type == "enabled" {
			out.Thinking = &anthropicThinking{Type: "adaptive", Display: "summarized"}
		}
	} else {
		out.Model = req.Model
	}

	// max_tokens is mandatory for Anthropic. buildRequest always sets one of the
	// token caps (MaxTokens for non-reasoning encodings); fall back defensively.
	switch {
	case req.MaxTokens != nil:
		out.MaxTokens = *req.MaxTokens
	case req.MaxCompletionTokens != nil:
		out.MaxTokens = *req.MaxCompletionTokens
	}
	if out.MaxTokens <= 0 {
		out.MaxTokens = 4096
	}

	// Translate the OpenAI-shaped transcript: system messages collapse into the
	// top-level system string; everything else maps to content blocks, merging
	// consecutive same-role messages (Anthropic wants one turn per role, and
	// requires tool results to ride in a user turn).
	//
	// boundaries records the end of each NON-volatile message — every candidate
	// prompt-cache breakpoint position — together with the running count of
	// stable-prefix content blocks up to and including it. The trailing volatile
	// message (live git status + todos, issue #404) carries fast-changing content
	// that must sit AFTER every breakpoint, or it would invalidate the cached
	// transcript each turn, so it never contributes a boundary. The transcript
	// breakpoints are then chosen from these positions so the cache-read lookback
	// always finds a recent write (issue #545; see the placement loop below).
	var systemParts []string
	type cacheBoundary struct{ msg, block, prefixBlocks int }
	var boundaries []cacheBoundary
	prefixBlocks := 0
	for _, m := range req.Messages {
		if m.Role == RoleSystem {
			if m.Content != "" {
				systemParts = append(systemParts, m.Content)
			}
			continue
		}
		role, blocks := anthropicBlocks(m)
		// On Vertex, replay the assistant turn's extended-thinking block (text +
		// signature) ahead of its text/tool_use blocks. When thinking is enabled
		// and the turn made tool calls, Anthropic requires the original thinking
		// block — unmodified, signature included, and preceding the tool_use — to be
		// present on the resent turn, or it rejects the follow-up request. The
		// blocks are captured from the response and round-tripped via the transcript
		// (see ModelSession and CompletionResponse.Thinking).
		if a.vertex && role == "assistant" && (m.Thinking != "" || m.ThinkingSignature != "") {
			blocks = append([]anthropicContent{{
				Type:      "thinking",
				Thinking:  m.Thinking,
				Signature: m.ThinkingSignature,
			}}, blocks...)
		}
		if len(blocks) == 0 {
			continue
		}
		if n := len(out.Messages); n > 0 && out.Messages[n-1].Role == role {
			out.Messages[n-1].Content = append(out.Messages[n-1].Content, blocks...)
		} else {
			out.Messages = append(out.Messages, anthropicMessage{Role: role, Content: blocks})
		}
		// Record the end of this message as a breakpoint candidate, with the running
		// stable-prefix block count, but only when it is NOT the volatile tail. A
		// volatile RoleUser message merges into the prior user turn (e.g. after a
		// tool result), yet the cacheable prefix ends at the last block contributed
		// by a non-volatile message — never on the volatile tail.
		if !m.Volatile {
			prefixBlocks += len(blocks)
			msg := len(out.Messages) - 1
			boundaries = append(boundaries, cacheBoundary{
				msg:          msg,
				block:        len(out.Messages[msg].Content) - 1,
				prefixBlocks: prefixBlocks,
			})
		}
	}

	// cacheOn gates all prompt-cache breakpoint emission. It is on by default so a
	// directly-constructed request (and the prior callers) keep caching; only an
	// explicit "off" directive — set by buildRequest when the model disables
	// caching or the provider lacks the CacheControlBreakpoints capability — turns
	// it off (issue #545).
	cacheOn := req.CacheTTL != "off"

	// System prompt. It is sent as a text-block array (carrying a cache_control
	// breakpoint when caching is on) on both the direct Anthropic API and Vertex —
	// Anthropic accepts cache_control only on a system content block, not on the
	// scalar string form, and the direct Messages API accepts the block-array
	// system shape the same as Vertex (issue #404). Assigning the field only when
	// non-empty keeps an empty system omitted rather than marshaling "system":[].
	systemHasBreakpoint := false
	if system := strings.Join(systemParts, "\n\n"); system != "" {
		block := anthropicSystemBlock{Type: "text", Text: system}
		if cacheOn {
			block.CacheControl = anthropicCacheCtl(req.CacheTTL)
			systemHasBreakpoint = true
		}
		out.System = []anthropicSystemBlock{block}
	}

	// Prompt-cache breakpoints across the stable prefix. On a multi-turn agent loop
	// these let the prior transcript be served from cache on the next request.
	// Walk the non-volatile message boundaries backward from the END of the
	// cacheable prefix: always break there (preserving the prior single-breakpoint
	// placement) and add an earlier breakpoint each time the block-distance since
	// the last one reaches cacheBreakpointSpacing. Because the spacing is under
	// Anthropic's ~20-block read lookback, a recent cache write stays inside the
	// next request's lookback window even when one turn appended many content
	// blocks (a turn with M parallel tool calls adds ~2M+1 blocks) — the gap that
	// silently forced a full-prefix rewrite before (issue #545). The total never
	// exceeds maxAnthropicBreakpoints including the system breakpoint, and the
	// per-block CacheControl is shared (a single immutable value). Emitted for both
	// the direct Anthropic API and Vertex; skipped entirely when caching is off.
	if cacheOn && len(boundaries) > 0 {
		budget := maxAnthropicBreakpoints
		if systemHasBreakpoint {
			budget-- // the system block already holds one of the four breakpoints
		}
		cc := anthropicCacheCtl(req.CacheTTL)
		placed, lastCum := 0, 0
		for i := len(boundaries) - 1; i >= 0 && placed < budget; i-- {
			b := boundaries[i]
			if placed == 0 || lastCum-b.prefixBlocks >= cacheBreakpointSpacing {
				out.Messages[b.msg].Content[b.block].CacheControl = cc
				lastCum = b.prefixBlocks
				placed++
			}
		}
	}

	for _, t := range req.Tools {
		schema := t.Function.Parameters
		if schema == nil {
			// Anthropic requires an object schema even for a no-argument tool.
			schema = map[string]interface{}{"type": "object", "properties": map[string]interface{}{}}
		}
		// Normalize the tool schema for Anthropic strict validation: a property that
		// combines a nullable union "type" with an "enum" is rejected (issue #567).
		// Deep-copies so the caller's shared Parameters map is never mutated.
		schema = anthropicSchema(schema)
		out.Tools = append(out.Tools, anthropicTool{
			Name:        t.Function.Name,
			Description: t.Function.Description,
			InputSchema: schema,
			Strict:      t.Function.Strict,
		})
	}

	// tool_choice is only valid alongside a tool set, and gogent only sets it
	// when offering tools; guard anyway so a stray choice can't produce a
	// request Anthropic would reject.
	if req.ToolChoice != nil && len(out.Tools) > 0 {
		out.ToolChoice = anthropicToolChoice(*req.ToolChoice)
	}

	return encodeJSON(buf, out)
}

// anthropicToolChoice maps the provider-independent ToolChoice onto Anthropic's
// object encoding: {"type":"auto"|"any"|"none"} or {"type":"tool","name":...}.
// "required" (force some tool) becomes Anthropic's "any".
func anthropicToolChoice(tc ToolChoice) map[string]interface{} {
	switch tc.Mode {
	case ToolChoiceNone:
		return map[string]interface{}{"type": "none"}
	case ToolChoiceRequired:
		return map[string]interface{}{"type": "any"}
	case ToolChoiceTool:
		return map[string]interface{}{"type": "tool", "name": tc.Name}
	default:
		return map[string]interface{}{"type": "auto"}
	}
}

// anthropicBlocks maps one internal message to its Anthropic role and content
// blocks. Assistant tool calls become tool_use blocks; tool/function results
// become a tool_result block in a user turn.
func anthropicBlocks(m Message) (string, []anthropicContent) {
	switch m.Role {
	case RoleAssistant:
		var blocks []anthropicContent
		if m.Content != "" {
			blocks = append(blocks, anthropicContent{Type: "text", Text: m.Content})
		}
		for _, tc := range m.ToolCalls {
			args := tc.Function.Arguments
			if strings.TrimSpace(args) == "" || !json.Valid([]byte(args)) {
				// tool_use.input must be a JSON object; fall back to empty.
				args = "{}"
			}
			blocks = append(blocks, anthropicContent{
				Type:  "tool_use",
				ID:    tc.ID,
				Name:  tc.Function.Name,
				Input: json.RawMessage(args),
			})
		}
		return "assistant", blocks
	case RoleTool, RoleFunction:
		return "user", []anthropicContent{{
			Type:      "tool_result",
			ToolUseID: m.ToolCallID,
			Content:   m.Content,
		}}
	default: // user
		var blocks []anthropicContent
		if m.Content != "" {
			blocks = append(blocks, anthropicContent{Type: "text", Text: m.Content})
		}
		for _, img := range m.Images {
			if b, ok := anthropicImageBlock(img); ok {
				blocks = append(blocks, b)
			}
		}
		return "user", blocks
	}
}

// anthropicImageBlock converts an OpenAI-style image reference into an Anthropic
// image content block: a data: URL becomes an inline base64 source, any other URL
// becomes a url source. Returns false for an empty/unusable reference.
func anthropicImageBlock(img ImageURL) (anthropicContent, bool) {
	url := strings.TrimSpace(img.URL)
	if url == "" {
		return anthropicContent{}, false
	}
	if mediaType, data, ok := parseDataURL(url); ok {
		return anthropicContent{Type: "image", Source: &anthropicImageSource{
			Type:      "base64",
			MediaType: mediaType,
			Data:      data,
		}}, true
	}
	return anthropicContent{Type: "image", Source: &anthropicImageSource{Type: "url", URL: url}}, true
}

// parseDataURL splits an RFC 2397 base64 data URL ("data:<media-type>;base64,<data>")
// into its media type and base64 payload. It returns ok=false for any non-data or
// non-base64 URL (e.g. a remote http URL), which the caller sends as a url source.
func parseDataURL(url string) (mediaType, data string, ok bool) {
	rest, ok := strings.CutPrefix(url, "data:")
	if !ok {
		return "", "", false
	}
	meta, payload, ok := strings.Cut(rest, ",")
	if !ok {
		return "", "", false
	}
	mediaType, isB64 := strings.CutSuffix(meta, ";base64")
	if !isB64 {
		return "", "", false
	}
	return mediaType, payload, true
}

// anthropicUsage is the usage block shared by the blocking response and the
// streaming message_start/message_delta events. input_tokens excludes cached
// prompt tokens, which Anthropic reports separately, so the full prompt total is
// the sum of all three input counters.
type anthropicUsage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
}

func (u anthropicUsage) toTokenUsage(outputTokens int) *TokenUsage {
	prompt := u.InputTokens + u.CacheReadInputTokens + u.CacheCreationInputTokens
	out := outputTokens
	if out == 0 {
		out = u.OutputTokens
	}
	if prompt == 0 && out == 0 {
		return nil
	}
	return &TokenUsage{
		PromptTokens:     prompt,
		CompletionTokens: out,
		TotalTokens:      prompt + out,
		// Anthropic reports BOTH cache tiers: cache_read_input_tokens (served from
		// cache, discounted) and cache_creation_input_tokens (written to cache, billed
		// at a premium). Both are retained — the write count was previously discarded
		// (issue #544). Anthropic is the only provider with a write count; 0 elsewhere.
		Cache: CacheStats{
			ReadTokens:  u.CacheReadInputTokens,
			WriteTokens: u.CacheCreationInputTokens,
		},
	}
}

type anthropicResponse struct {
	Content []struct {
		Type      string          `json:"type"`
		Text      string          `json:"text"`
		Thinking  string          `json:"thinking"`
		Signature string          `json:"signature"`
		ID        string          `json:"id"`
		Name      string          `json:"name"`
		Input     json.RawMessage `json:"input"`
	} `json:"content"`
	StopReason string         `json:"stop_reason"`
	Usage      anthropicUsage `json:"usage"`
}

func (anthropicAdapter) parseResponse(body []byte) (*CompletionResponse, error) {
	var ar anthropicResponse
	if err := json.Unmarshal(body, &ar); err != nil {
		return nil, fmt.Errorf("unmarshal response: %w", err)
	}
	resp := &CompletionResponse{Role: RoleAssistant}
	var text, reasoning strings.Builder
	for _, b := range ar.Content {
		switch b.Type {
		case "text":
			text.WriteString(b.Text)
		case "thinking":
			// Capture the extended-thinking block so it can be replayed unmodified
			// on the next turn (required for tool use with thinking enabled). The
			// first thinking block of the turn is the one that precedes any tool_use.
			if resp.Thinking == "" && resp.ThinkingSignature == "" {
				resp.Thinking = b.Thinking
				resp.ThinkingSignature = b.Signature
			}
			// Also retain the human-readable thinking-summary text in Reasoning so a
			// thinking-only turn is recoverable, mirroring the OpenAI-compatible
			// reasoning side channel (issue #402). All thinking blocks contribute.
			reasoning.WriteString(b.Thinking)
		case "tool_use":
			args := string(b.Input)
			if strings.TrimSpace(args) == "" {
				args = "{}"
			}
			resp.ToolCalls = append(resp.ToolCalls, ToolCall{
				ID:       b.ID,
				Type:     "function",
				Function: FunctionCall{Name: b.Name, Arguments: args},
			})
		}
	}
	resp.Content = text.String()
	resp.Reasoning = reasoning.String()
	resp.FinishReason = anthropicStopReason(ar.StopReason)
	resp.Usage = ar.Usage.toTokenUsage(0)
	return resp, nil
}

// anthropicStopReason maps an Anthropic stop_reason onto the OpenAI-style
// finish_reason gogent's callers already understand.
func anthropicStopReason(reason string) string {
	switch reason {
	case "end_turn", "stop_sequence":
		return "stop"
	case "tool_use":
		return "tool_calls"
	case "max_tokens":
		return "length"
	default:
		return reason
	}
}

// anthropicStreamEvent is one decoded SSE "data:" payload from the Messages
// streaming API. A single message arrives as message_start → (content_block_start
// → content_block_delta* → content_block_stop)* → message_delta → message_stop;
// the event type drives which fields are populated.
type anthropicStreamEvent struct {
	Type    string `json:"type"`
	Index   int    `json:"index"`
	Message *struct {
		Usage *anthropicUsage `json:"usage"`
	} `json:"message"`
	ContentBlock *struct {
		Type string `json:"type"`
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"content_block"`
	Delta *struct {
		Type        string `json:"type"`
		Text        string `json:"text"`
		Thinking    string `json:"thinking"`
		Signature   string `json:"signature"`
		PartialJSON string `json:"partial_json"`
		StopReason  string `json:"stop_reason"`
	} `json:"delta"`
	Usage *anthropicUsage `json:"usage"`
}

func (anthropicAdapter) parseStream(body io.Reader, streamCh chan<- StreamResponse) (string, *TokenUsage, error) {
	reader := bufio.NewReaderSize(body, 64*1024)

	// tool_use arguments stream as input_json_delta fragments under a content
	// block index; accumulate them per block in first-seen order.
	type accTool struct {
		id, name string
		args     strings.Builder
	}
	toolsByBlock := map[int]*accTool{}
	var order []int

	var content strings.Builder
	var promptUsage anthropicUsage
	outputTokens := 0
	var finishReason *string

	// Capture the first extended-thinking block (text + signature) so it can be
	// replayed unmodified on the next turn — required for tool use with thinking
	// enabled (see buildBody). thinkingIdx pins the first thinking content block;
	// only its deltas are accumulated, so a later thinking block can't corrupt the
	// captured signature/text pairing. Live reasoning is still surfaced for every
	// thinking block regardless (issue #217).
	var thinkingBuf strings.Builder
	var thinkingSig string
	thinkingIdx := -1

	for {
		line, readErr := reader.ReadString('\n')
		if data, ok := strings.CutPrefix(strings.TrimSpace(line), "data:"); ok {
			data = strings.TrimSpace(data)
			if data != "" {
				var ev anthropicStreamEvent
				if err := json.Unmarshal([]byte(data), &ev); err == nil {
					switch ev.Type {
					case "message_start":
						if ev.Message != nil && ev.Message.Usage != nil {
							promptUsage = *ev.Message.Usage
						}
					case "content_block_start":
						if ev.ContentBlock != nil {
							switch ev.ContentBlock.Type {
							case "tool_use":
								toolsByBlock[ev.Index] = &accTool{id: ev.ContentBlock.ID, name: ev.ContentBlock.Name}
								order = append(order, ev.Index)
							case "thinking":
								if thinkingIdx == -1 {
									thinkingIdx = ev.Index
								}
							}
						}
					case "content_block_delta":
						if ev.Delta != nil {
							switch ev.Delta.Type {
							case "text_delta":
								if ev.Delta.Text != "" {
									content.WriteString(ev.Delta.Text)
									streamCh <- StreamResponse{Content: ev.Delta.Text, Role: RoleAssistant}
								}
							case "thinking_delta":
								// Extended-thinking reasoning delta — surfaced as live
								// thinking, kept out of the visible answer (issue #217),
								// and accumulated for the first block so it can be
								// replayed next turn.
								if ev.Delta.Thinking != "" {
									streamCh <- StreamResponse{Reasoning: ev.Delta.Thinking, Role: RoleAssistant}
									if ev.Index == thinkingIdx {
										thinkingBuf.WriteString(ev.Delta.Thinking)
									}
								}
							case "signature_delta":
								// The thinking block's signature; must be preserved
								// verbatim for replay. Captured for the first thinking
								// block only.
								if ev.Index == thinkingIdx {
									thinkingSig += ev.Delta.Signature
								}
							case "input_json_delta":
								if acc := toolsByBlock[ev.Index]; acc != nil {
									acc.args.WriteString(ev.Delta.PartialJSON)
								}
							}
						}
					case "message_delta":
						if ev.Delta != nil && ev.Delta.StopReason != "" {
							r := anthropicStopReason(ev.Delta.StopReason)
							finishReason = &r
						}
						if ev.Usage != nil {
							outputTokens = ev.Usage.OutputTokens
						}
					}
				}
			}
		}
		if readErr != nil {
			if readErr == io.EOF {
				break
			}
			return content.String(), promptUsage.toTokenUsage(outputTokens), &ModelError{
				Type:    ErrorGeneric,
				Message: fmt.Sprintf("error reading stream: %v", readErr),
			}
		}
	}

	// A turn cut off by max_tokens (stop_reason "max_tokens" → "length") may have
	// left a tool_use block's input_json_delta fragments incomplete; flag any call
	// whose assembled args no longer parse so the agent layer can salvage or
	// complete it deterministically (issue #390). The empty→"{}" default below is
	// applied first, so a call that streamed no input is treated as complete, not
	// truncated.
	truncatedTurn := finishReason != nil && *finishReason == "length"
	var toolCalls []ToolCall
	for _, idx := range order {
		acc := toolsByBlock[idx]
		args := acc.args.String()
		if strings.TrimSpace(args) == "" {
			args = "{}"
		}
		toolCalls = append(toolCalls, ToolCall{
			ID:        acc.id,
			Type:      "function",
			Function:  FunctionCall{Name: acc.name, Arguments: args},
			Truncated: truncatedTurn && argsTruncated(args),
		})
	}

	usage := promptUsage.toTokenUsage(outputTokens)
	streamCh <- StreamResponse{
		ToolCalls:         toolCalls,
		FinishReason:      finishReason,
		Usage:             usage,
		Thinking:          thinkingBuf.String(),
		ThinkingSignature: thinkingSig,
		Done:              true,
	}
	return content.String(), usage, nil
}

// ---------------------------------------------------------------------------
// Vertex AI native Gemini adapter
// ---------------------------------------------------------------------------

// geminiAdapter translates gogent's OpenAI-shaped request/response to and from
// Google's NATIVE Gemini wire format (Vertex AI :generateContent /
// :streamGenerateContent), as opposed to the OpenAI-compatible shim served by
// openAIAdapter. The native protocol differs substantially:
//
//   - messages become contents[], each a {role, parts[]} block; the system
//     prompt is hoisted to a top-level systemInstruction;
//   - roles are user|model|function rather than user|assistant|tool;
//   - parts are polymorphic (text, inlineData/fileData images, functionCall,
//     functionResponse, and reasoning text flagged thought:true);
//   - tools are tools[].functionDeclarations[] and tool_choice is
//     toolConfig.functionCallingConfig.mode (AUTO|ANY|NONE);
//   - sampling/limits/structured-output/thinking live under generationConfig
//     (camelCase: maxOutputTokens, topP, responseMimeType, responseSchema,
//     thinkingConfig) and JSON-Schema type names are UPPERCASE (OBJECT, STRING…);
//   - the streaming SSE has no [DONE] sentinel — it ends at EOF, with the
//     finishReason and usageMetadata carried only in the final chunk.
//
// The model name is NOT in the request body for native Gemini — it lives in the
// URL path (see modelURLEndpoints in provider_vertex.go) — so buildBody ignores
// req.Model.
type geminiAdapter struct{}

// geminiRequest is the native :generateContent / :streamGenerateContent body.
type geminiRequest struct {
	Contents          []geminiContent   `json:"contents,omitempty"`
	SystemInstruction *geminiContent    `json:"systemInstruction,omitempty"`
	Tools             []geminiTool      `json:"tools,omitempty"`
	ToolConfig        *geminiToolConfig `json:"toolConfig,omitempty"`
	GenerationConfig  *geminiGenConfig  `json:"generationConfig,omitempty"`
	// CachedContent references an explicit Gemini CachedContent resource
	// ("projects/…/locations/…/cachedContents/{id}") whose systemInstruction +
	// tools + leading contents this request reuses instead of re-sending. When
	// set (issue #547), buildBody OMITS the shadowed systemInstruction/tools and
	// the cached prefix contents, emitting only the post-snapshot tail. omitempty
	// keeps it — and the whole request — byte-identical to today when inactive.
	CachedContent string `json:"cachedContent,omitempty"`
}

// geminiContent is one turn: a role (user|model|function) and a polymorphic parts
// array. Role is omitted for the systemInstruction content (which has none).
type geminiContent struct {
	Role  string       `json:"role,omitempty"`
	Parts []geminiPart `json:"parts"`
}

// geminiPart is one content part. Exactly one discriminator field is set per part;
// omitempty keeps each part to just the fields its kind uses.
type geminiPart struct {
	Text             string                  `json:"text,omitempty"`
	InlineData       *geminiInlineData       `json:"inlineData,omitempty"`
	FileData         *geminiFileData         `json:"fileData,omitempty"`
	FunctionCall     *geminiFunctionCall     `json:"functionCall,omitempty"`
	FunctionResponse *geminiFunctionResponse `json:"functionResponse,omitempty"`
	// ThoughtSignature re-emits the opaque signature Vertex returned on a Gemini
	// 3.x functionCall part. It MUST accompany the functionCall when that call is
	// replayed in conversation history, else Vertex rejects the turn with HTTP 400
	// ("Function call is missing a thought_signature"). omitempty keeps it off
	// every part that has no signature (issue #573).
	ThoughtSignature string `json:"thoughtSignature,omitempty"`
}

// geminiInlineData is an inline (base64) image/media part.
type geminiInlineData struct {
	MimeType string `json:"mimeType"`
	Data     string `json:"data"`
}

// geminiFileData is a by-reference media part (gs:// or http(s) URI). MimeType is
// optional — Gemini infers it for many types when omitted.
type geminiFileData struct {
	MimeType string `json:"mimeType,omitempty"`
	FileURI  string `json:"fileUri"`
}

// geminiFunctionCall is a model→tool call. Args is the JSON-object arguments
// (Gemini wants an object, not the OpenAI arguments-as-string).
type geminiFunctionCall struct {
	Name string          `json:"name"`
	Args json.RawMessage `json:"args,omitempty"`
	ID   string          `json:"id,omitempty"`
}

// geminiFunctionResponse is a tool result, carried in a role:"user" content.
// Response MUST be a JSON object (scalars are wrapped as {"result": …}); ID
// echoes the originating functionCall id (required for Gemini 3 round-trips).
type geminiFunctionResponse struct {
	Name     string          `json:"name"`
	Response json.RawMessage `json:"response"`
	ID       string          `json:"id,omitempty"`
}

type geminiTool struct {
	FunctionDeclarations []geminiFunctionDeclaration `json:"functionDeclarations,omitempty"`
}

type geminiFunctionDeclaration struct {
	Name        string      `json:"name"`
	Description string      `json:"description,omitempty"`
	Parameters  interface{} `json:"parameters,omitempty"`
}

type geminiToolConfig struct {
	FunctionCallingConfig *geminiFunctionCallingConfig `json:"functionCallingConfig,omitempty"`
}

type geminiFunctionCallingConfig struct {
	Mode                 string   `json:"mode,omitempty"`
	AllowedFunctionNames []string `json:"allowedFunctionNames,omitempty"`
	// NOTE: no parallelFunctionCalls field. The Vertex AI native Gemini endpoint
	// (the only backend this adapter targets) rejects an unknown
	// "parallelFunctionCalls" in functionCallingConfig with a 400 — it is a Gemini
	// Developer API field, not a Vertex one. Gemini allows parallel function calls
	// by default and Vertex exposes no toggle here, so gogent simply does not emit it.
}

type geminiGenConfig struct {
	Temperature      *float32              `json:"temperature,omitempty"`
	TopP             *float32              `json:"topP,omitempty"`
	MaxOutputTokens  int                   `json:"maxOutputTokens,omitempty"`
	ResponseMimeType string                `json:"responseMimeType,omitempty"`
	ResponseSchema   interface{}           `json:"responseSchema,omitempty"`
	ThinkingConfig   *geminiThinkingConfig `json:"thinkingConfig,omitempty"`
}

// geminiThinkingConfig controls reasoning. ThinkingBudget is a pointer so a
// deliberate 0 (disable, on Flash) is expressible and distinguishable from unset;
// -1 means dynamic/auto. IncludeThoughts:true surfaces thought summaries as
// text parts flagged thought:true.
type geminiThinkingConfig struct {
	IncludeThoughts bool `json:"includeThoughts,omitempty"`
	ThinkingBudget  *int `json:"thinkingBudget,omitempty"`
}

// geminiBuildContents maps a request's messages and tools to the native Gemini
// pieces — the top-level systemInstruction, the merged contents[] turns, the
// tools[].functionDeclarations, and the toolConfig — without touching the wire
// (no marshaling, no network). It is the SINGLE source of truth for that mapping
// so buildBody and the explicit-cache lifecycle manager (ensureGeminiCache, issue
// #547) agree byte-for-byte on what a CachedContent resource shadows. Returns
// (nil, …) for any piece the request does not produce, exactly as the prior
// inline code left those fields unset.
func geminiBuildContents(req CompletionRequest) (sys *geminiContent, contents []geminiContent, tools []geminiTool, toolCfg *geminiToolConfig) {
	// Hoist system messages to the top-level systemInstruction; map the rest to
	// contents, merging consecutive same-role turns (Gemini wants one turn per
	// role, and tool results must ride in a user turn).
	var systemParts []string
	for _, m := range req.Messages {
		if m.Role == RoleSystem {
			if m.Content != "" {
				systemParts = append(systemParts, m.Content)
			}
			continue
		}
		role, parts := geminiParts(m)
		if len(parts) == 0 {
			continue
		}
		if n := len(contents); n > 0 && contents[n-1].Role == role {
			contents[n-1].Parts = append(contents[n-1].Parts, parts...)
		} else {
			contents = append(contents, geminiContent{Role: role, Parts: parts})
		}
	}
	if len(systemParts) > 0 {
		sys = &geminiContent{Parts: []geminiPart{{Text: strings.Join(systemParts, "\n\n")}}}
	}

	// Tools → functionDeclarations (JSON-Schema type names upper-cased).
	if len(req.Tools) > 0 {
		decls := make([]geminiFunctionDeclaration, 0, len(req.Tools))
		for _, t := range req.Tools {
			params := geminiSchema(t.Function.Parameters)
			if params == nil {
				// Gemini requires an object schema even for a no-argument tool.
				params = map[string]interface{}{"type": "OBJECT", "properties": map[string]interface{}{}}
			}
			decls = append(decls, geminiFunctionDeclaration{
				Name:        t.Function.Name,
				Description: t.Function.Description,
				Parameters:  params,
			})
		}
		tools = []geminiTool{{FunctionDeclarations: decls}}
		if req.ToolChoice != nil {
			toolCfg = geminiToolConfigFor(*req.ToolChoice)
		}
	}
	return sys, contents, tools, toolCfg
}

func (geminiAdapter) buildBody(req CompletionRequest, buf *bytes.Buffer) error {
	out := geminiRequest{}

	sys, contents, tools, toolCfg := geminiBuildContents(req)
	if req.GeminiCachedContent != "" {
		// An explicit CachedContent resource holds the systemInstruction, tools,
		// and the leading GeminiCachedPrefixContents turns — Vertex prepends them
		// server-side, so re-sending them (and re-declaring tools) is redundant and
		// rejected. Reference the resource and emit only the post-snapshot tail.
		out.CachedContent = req.GeminiCachedContent
		n := req.GeminiCachedPrefixContents
		if n < 0 {
			n = 0
		}
		if n > len(contents) {
			n = len(contents) // defensive: never slice past the available contents
		}
		out.Contents = contents[n:]
		// toolConfig (tool_choice) is a per-request directive, NOT part of the cached
		// resource, so a non-default forcing (ANY/NONE/specific) must still ride the
		// request or caching would silently drop it. AUTO is Gemini's default when
		// toolConfig is omitted, so leave it off on the cached branch (keeping the
		// referencing request minimal). The agent loop only ever sends AUTO.
		if req.ToolChoice != nil && req.ToolChoice.Mode != ToolChoiceAuto {
			out.ToolConfig = toolCfg
		}
	} else {
		// Inactive path — identical to the prior inline mapping, so a non-cached
		// request marshals byte-for-byte as before.
		out.SystemInstruction = sys
		out.Contents = contents
		out.Tools = tools
		out.ToolConfig = toolCfg
	}

	// generationConfig: sampling, output cap, structured output, thinking.
	gc := &geminiGenConfig{}
	hasGen := false
	if req.Temperature != nil {
		gc.Temperature = req.Temperature
		hasGen = true
	}
	if req.TopP != nil {
		gc.TopP = req.TopP
		hasGen = true
	}
	switch {
	case req.MaxTokens != nil && *req.MaxTokens > 0:
		gc.MaxOutputTokens = *req.MaxTokens
		hasGen = true
	case req.MaxCompletionTokens != nil && *req.MaxCompletionTokens > 0:
		gc.MaxOutputTokens = *req.MaxCompletionTokens
		hasGen = true
	}
	if req.ResponseFormat != nil {
		switch req.ResponseFormat.Type {
		case "json_object", "json_schema":
			gc.ResponseMimeType = "application/json"
			hasGen = true
			if req.ResponseFormat.JSONSchema != nil && req.ResponseFormat.JSONSchema.Schema != nil {
				gc.ResponseSchema = geminiSchema(req.ResponseFormat.JSONSchema.Schema)
			}
		}
	}
	if req.Thinking != nil {
		tc := &geminiThinkingConfig{}
		if req.Thinking.Type == "enabled" {
			tc.IncludeThoughts = true
			budget := -1 // dynamic/auto
			tc.ThinkingBudget = &budget
		} else {
			budget := 0 // disable (Flash; 2.5 Pro cannot fully disable — documented limitation)
			tc.ThinkingBudget = &budget
		}
		gc.ThinkingConfig = tc
		hasGen = true
	}
	if hasGen {
		out.GenerationConfig = gc
	}

	return encodeJSON(buf, out)
}

// geminiParts maps one internal message to its Gemini role and parts. Assistant
// tool calls become functionCall parts in a model turn; tool/function results
// become a functionResponse part in a user turn (Gemini carries tool results in
// the user role).
func geminiParts(m Message) (string, []geminiPart) {
	switch m.Role {
	case RoleAssistant:
		var parts []geminiPart
		if m.Content != "" {
			parts = append(parts, geminiPart{Text: m.Content})
		}
		for _, tc := range m.ToolCalls {
			// Gemini requires functionCall.args to be a JSON object; forward it only
			// when it parses as one, so a malformed/non-object arguments string from
			// persisted history is dropped rather than emitted as invalid wire format.
			var args json.RawMessage
			if a := strings.TrimSpace(tc.Function.Arguments); a != "" {
				var obj map[string]interface{}
				if json.Unmarshal([]byte(a), &obj) == nil {
					args = json.RawMessage(a)
				}
			}
			parts = append(parts, geminiPart{
				FunctionCall: &geminiFunctionCall{
					Name: tc.Function.Name,
					Args: args,
					ID:   tc.ID,
				},
				// Re-emit the Gemini 3.x thoughtSignature so the replayed call is
				// accepted; empty (and thus omitted) for non-Gemini-origin calls (#573).
				ThoughtSignature: tc.ThoughtSignature,
			})
		}
		return "model", parts
	case RoleTool, RoleFunction:
		return "user", []geminiPart{{FunctionResponse: &geminiFunctionResponse{
			Name:     m.Name,
			Response: geminiToolResponseObject(m.Content),
			ID:       m.ToolCallID,
		}}}
	default: // user
		var parts []geminiPart
		if m.Content != "" {
			parts = append(parts, geminiPart{Text: m.Content})
		}
		for _, img := range m.Images {
			if p, ok := geminiImagePart(img); ok {
				parts = append(parts, p)
			}
		}
		return "user", parts
	}
}

// geminiToolResponseObject wraps a tool result for a functionResponse, whose
// response field MUST be a JSON object. A result that is already a JSON object is
// used as-is; any other JSON value (array/number/bool/string/null) is wrapped as
// {"result": <value>}; a non-JSON string is wrapped as {"result": "<string>"}.
func geminiToolResponseObject(content string) json.RawMessage {
	trimmed := strings.TrimSpace(content)
	if trimmed != "" && json.Valid([]byte(trimmed)) {
		var v interface{}
		if json.Unmarshal([]byte(trimmed), &v) == nil {
			if _, isObj := v.(map[string]interface{}); isObj {
				return json.RawMessage(trimmed)
			}
			if wrapped, err := json.Marshal(map[string]interface{}{"result": v}); err == nil {
				return json.RawMessage(wrapped)
			}
		}
	}
	if wrapped, err := json.Marshal(map[string]string{"result": content}); err == nil {
		return json.RawMessage(wrapped)
	}
	return json.RawMessage(`{}`)
}

// geminiImagePart converts an OpenAI-style image reference into a Gemini part: a
// data: URL becomes an inlineData (base64) part, any other URI (gs://, http(s))
// becomes a fileData reference. Returns false for an empty/unusable reference.
func geminiImagePart(img ImageURL) (geminiPart, bool) {
	u := strings.TrimSpace(img.URL)
	if u == "" {
		return geminiPart{}, false
	}
	if mediaType, data, ok := parseDataURL(u); ok {
		return geminiPart{InlineData: &geminiInlineData{MimeType: mediaType, Data: data}}, true
	}
	return geminiPart{FileData: &geminiFileData{FileURI: u}}, true
}

// geminiToolConfigFor maps the provider-independent ToolChoice onto Gemini's
// functionCallingConfig: AUTO (model decides), ANY (must call some tool — the
// "required" analogue), NONE (no calls), or ANY restricted to a single
// allowedFunctionNames entry for a forced specific tool.
func geminiToolConfigFor(tc ToolChoice) *geminiToolConfig {
	cfg := &geminiFunctionCallingConfig{}
	switch tc.Mode {
	case ToolChoiceNone:
		cfg.Mode = "NONE"
	case ToolChoiceRequired:
		cfg.Mode = "ANY"
	case ToolChoiceTool:
		cfg.Mode = "ANY"
		cfg.AllowedFunctionNames = []string{tc.Name}
	default:
		cfg.Mode = "AUTO"
	}
	// No parallelFunctionCalls: Vertex rejects the field and defaults to parallel
	// calls anyway (see geminiFunctionCallingConfig).
	return &geminiToolConfig{FunctionCallingConfig: cfg}
}

// geminiSchema normalizes a JSON-Schema document into the OpenAPI subset Gemini's
// Schema proto accepts, by deep-copying it (via a JSON round-trip, so it accepts a
// map, a struct or a json.RawMessage — and the caller's shared Parameters map is
// never mutated) and running sanitizeGeminiSchema over the copy. That recursively:
// strips every keyword Gemini rejects (additionalProperties, $ref/$defs, oneOf/
// allOf/not, const, patternProperties, multipleOf, …); collapses union "type"
// arrays to a single UPPERCASE scalar (+ nullable for a dropped "null"); infers a
// "type" on any typeless node (Gemini requires one); converts const→enum,
// oneOf→anyOf, allOf→merge; and prunes "required" to declared properties. Returns
// nil for nil input and falls back to the original value if it is not
// JSON-encodable.
func geminiSchema(v interface{}) interface{} {
	if v == nil {
		return nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return v
	}
	var generic interface{}
	if err := json.Unmarshal(b, &generic); err != nil {
		return v
	}
	return sanitizeGeminiSchema(generic)
}

// geminiSchemaAllowed is the set of JSON-Schema keywords Gemini's Schema proto (an
// OpenAPI 3.0 subset) actually accepts on a node. Any key NOT in this set is an
// unknown proto field and makes Vertex reject the whole request with HTTP 400
// ("Invalid JSON payload received. Unknown name …"), so the sanitizer strips it.
// This is the single source of truth for the allow-list; everything else is
// dropped (additionalProperties, $ref/$defs, oneOf/allOf/not, const,
// patternProperties, multipleOf, exclusiveMinimum/Maximum, …).
var geminiSchemaAllowed = map[string]bool{
	"type": true, "format": true, "title": true, "description": true,
	"nullable": true, "enum": true, "items": true, "properties": true,
	"required": true, "propertyOrdering": true,
	"minItems": true, "maxItems": true,
	"minProperties": true, "maxProperties": true,
	"minLength": true, "maxLength": true, "pattern": true,
	"minimum": true, "maximum": true,
	"anyOf": true, "default": true, "example": true,
}

// sanitizeGeminiSchema deep-rewrites a decoded JSON-Schema value into a document
// Gemini's Schema proto accepts, recursing through "properties", "items" and
// "anyOf" so the rules apply at every depth. The decoded input is owned by this
// call (geminiSchema round-trips through JSON first), so in-place mutation is safe
// and the caller's shared Parameters map is never touched. Per node it:
//
//   - Converts combinators Gemini lacks: "const" X → enum [X]; "oneOf" → "anyOf";
//     "allOf" → a shallow merge of its subschemas (best-effort). "not" is dropped.
//   - Strips every key not in geminiSchemaAllowed (additionalProperties, $ref,
//     $defs, definitions, patternProperties, multipleOf, exclusive*, …) — each
//     would otherwise 400 as an unknown proto field.
//   - Normalizes "type": collapses a union array to a single scalar (dropping
//     "null" and setting "nullable":true when present, #573), and upper-cases the
//     surviving scalar to Gemini's enum form (string→STRING, object→OBJECT, …). A
//     degenerate ["null"]-only type is removed (no representable scalar).
//   - INFERS a "type" when none is declared (properties→OBJECT, items→ARRAY,
//     enum→STRING, anyOf→leave typeless, else STRING). Gemini requires a type on
//     every node ("schema didn't specify the schema type field"); gogent tools
//     such as todo deliberately omit it on a property to let other providers
//     accept array-or-null (#263), so inferring keeps both paths working.
//   - Prunes "required" to names that actually exist in "properties" (a stray
//     required entry is rejected).
func sanitizeGeminiSchema(v interface{}) interface{} {
	switch t := v.(type) {
	case map[string]interface{}:
		canonicalizeGeminiCombinators(t)
		// Strip unknown keys first so they neither reach the wire nor confuse the
		// type-inference/required-pruning that follows.
		for k := range t {
			if !geminiSchemaAllowed[k] {
				delete(t, k)
			}
		}
		normalizeGeminiType(t)
		// Recurse into the structural children that hold nested schemas.
		if props, ok := t["properties"].(map[string]interface{}); ok {
			for name, sub := range props {
				props[name] = sanitizeGeminiSchema(sub)
			}
		}
		if items, ok := t["items"]; ok {
			t["items"] = sanitizeGeminiSchema(items)
		}
		if anyOf, ok := t["anyOf"].([]interface{}); ok {
			for i := range anyOf {
				anyOf[i] = sanitizeGeminiSchema(anyOf[i])
			}
		}
		pruneGeminiRequired(t)
		return t
	case []interface{}:
		// A bare array (e.g. a tuple "items") — sanitize each element defensively.
		for i := range t {
			t[i] = sanitizeGeminiSchema(t[i])
		}
		return t
	default:
		return v
	}
}

// canonicalizeGeminiCombinators rewrites the JSON-Schema constructs Gemini does
// not support into ones it does, in place: "const" X becomes an enum of one
// ([X]); "oneOf" becomes "anyOf" (the only combinator Gemini accepts), unless an
// "anyOf" already exists, in which case oneOf is dropped; "allOf" is shallow-
// merged into the node (later subschemas win on key collisions) so its
// constraints survive without the unsupported keyword; "not" is dropped (no
// equivalent). Called before the allow-list strip so the products (enum/anyOf)
// survive and the originals (const/oneOf/allOf/not) are then removed.
func canonicalizeGeminiCombinators(node map[string]interface{}) {
	if c, ok := node["const"]; ok {
		if _, hasEnum := node["enum"]; !hasEnum {
			node["enum"] = []interface{}{c}
		}
	}
	if oneOf, ok := node["oneOf"].([]interface{}); ok {
		if _, hasAnyOf := node["anyOf"]; !hasAnyOf {
			node["anyOf"] = oneOf
		}
	}
	if allOf, ok := node["allOf"].([]interface{}); ok {
		for _, sub := range allOf {
			if m, ok := sub.(map[string]interface{}); ok {
				for k, val := range m {
					if _, exists := node[k]; !exists || k == "properties" {
						mergeGeminiAllOfKey(node, k, val)
					}
				}
			}
		}
	}
}

// mergeGeminiAllOfKey folds one key/value from an allOf subschema into the parent
// node. "properties" and "required" accumulate (union); any other key is set only
// when the parent lacks it (the caller already guards non-properties keys).
func mergeGeminiAllOfKey(node map[string]interface{}, k string, val interface{}) {
	switch k {
	case "properties":
		dst, _ := node["properties"].(map[string]interface{})
		if dst == nil {
			dst = map[string]interface{}{}
			node["properties"] = dst
		}
		if src, ok := val.(map[string]interface{}); ok {
			for name, sub := range src {
				dst[name] = sub
			}
		}
	case "required":
		existing, _ := node["required"].([]interface{})
		if add, ok := val.([]interface{}); ok {
			node["required"] = append(existing, add...)
		}
	default:
		node[k] = val
	}
}

// normalizeGeminiType collapses a node's "type" to the single scalar Gemini's
// proto enum requires and upper-cases it, then infers a type when none is present.
// A union array is reduced via dropNullFromType (shared with the Anthropic caller,
// #567): "null" is dropped and "nullable":true set; the first surviving non-null
// member becomes the scalar. A ["null"]-only type is removed outright. When no
// "type" exists it is inferred from sibling keywords so every node carries one.
func normalizeGeminiType(node map[string]interface{}) {
	switch tv := node["type"].(type) {
	case string:
		if up := strings.ToUpper(tv); up != "NULL" {
			node["type"] = up
		} else {
			delete(node, "type")
		}
	case []interface{}:
		rewritten, droppedNull := dropNullFromType(tv)
		if scalar, ok := geminiScalarType(rewritten); ok {
			node["type"] = strings.ToUpper(scalar)
			if droppedNull {
				node["nullable"] = true
			}
		} else {
			// Degenerate ["null"]-only: no representable scalar — drop the type so
			// it can be re-inferred below (and never reaches the wire as an array).
			delete(node, "type")
		}
	}
	if _, has := node["type"]; has {
		return
	}
	// Infer a missing type from structural siblings (most specific first).
	switch {
	case hasGeminiKey(node, "properties", "minProperties", "maxProperties"):
		node["type"] = "OBJECT"
	case hasGeminiKey(node, "items", "minItems", "maxItems"):
		node["type"] = "ARRAY"
	case hasGeminiKey(node, "enum"):
		node["type"] = "STRING"
	case hasGeminiKey(node, "anyOf"):
		// A union node carries its type in each branch; leave the parent typeless.
	default:
		node["type"] = "STRING"
	}
}

// pruneGeminiRequired drops any "required" entry that does not name a declared
// property, which Vertex rejects, and removes an empty/!array required.
func pruneGeminiRequired(node map[string]interface{}) {
	req, ok := node["required"].([]interface{})
	if !ok {
		if _, present := node["required"]; present {
			delete(node, "required")
		}
		return
	}
	props, _ := node["properties"].(map[string]interface{})
	kept := req[:0]
	for _, r := range req {
		if name, ok := r.(string); ok {
			if _, exists := props[name]; exists {
				kept = append(kept, name)
			}
		}
	}
	if len(kept) == 0 {
		delete(node, "required")
		return
	}
	node["required"] = kept
}

// geminiScalarType reduces the non-null remainder of a "type" union to the single
// scalar Gemini can carry in its (scalar enum) Schema.type proto field. It accepts
// either a scalar string (already collapsed by dropNullFromType when one non-null
// member remained) or an array of remaining members, returning the first non-null
// string member. Returns ("", false) when no non-null string scalar is available
// (e.g. an empty or degenerate ["null"]-only remainder), so the caller leaves the
// value as-is rather than upper-casing "null" into a bogus "NULL" type.
func geminiScalarType(rewritten interface{}) (string, bool) {
	switch r := rewritten.(type) {
	case string:
		if r == "null" {
			return "", false
		}
		return r, true
	case []interface{}:
		for _, m := range r {
			if s, ok := m.(string); ok && s != "null" {
				return s, true
			}
		}
	}
	return "", false
}

// hasGeminiKey reports whether node contains any of the given keys.
func hasGeminiKey(node map[string]interface{}, keys ...string) bool {
	for _, k := range keys {
		if _, ok := node[k]; ok {
			return true
		}
	}
	return false
}

// anthropicSchema normalizes a JSON-Schema document for Anthropic strict tool
// validation by deep-copying it (via a JSON round-trip, so it accepts a map, a
// struct or a json.RawMessage) and reconciling every property that combines a
// nullable union "type" with an "enum". Anthropic's strict validator cannot
// match a scalar enum member against a union type, so such a property is
// rejected with HTTP 400 before the model runs (issue #567). The deep copy means
// the caller's shared Parameters map is never mutated (concurrent-reuse safe).
// Returns nil for nil input and falls back to the original value if it is not
// JSON-encodable. Applied only on the Anthropic/Vertex-Anthropic path — the
// OpenAI/Z.AI/OpenRouter and Gemini adapters are unaffected.
func anthropicSchema(v interface{}) interface{} {
	if v == nil {
		return nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return v
	}
	var generic interface{}
	if err := json.Unmarshal(b, &generic); err != nil {
		return v
	}
	return normalizeAnthropicSchemaTypes(generic)
}

// normalizeAnthropicSchemaTypes walks a decoded JSON-Schema value and, for every
// node that declares BOTH an "enum" and a union "type" array containing "null"
// (e.g. {"type":["string","null"],"enum":[...]}), drops the "null" member from
// the type so the surviving shape is a verified-valid Anthropic strict schema
// (plain "string" + enum). The "enum" and "required" siblings are left untouched
// — nullability for these fields is expressed by the model omitting them / the
// handler defaulting, not by a wire-level null the strict validator rejects. When
// exactly one non-null member remains the type collapses to that scalar string;
// with several it stays an array; a degenerate ["null"]-only type is left as-is.
// Recurses through every map value and array element, so the rule applies
// wherever it appears — nested properties, items, $defs, and anyOf/allOf/oneOf
// combinators — covering MCP and future strict tools too.
func normalizeAnthropicSchemaTypes(v interface{}) interface{} {
	switch t := v.(type) {
	case map[string]interface{}:
		if _, hasEnum := t["enum"]; hasEnum {
			if union, ok := t["type"].([]interface{}); ok {
				if rewritten, changed := dropNullFromType(union); changed {
					t["type"] = rewritten
				}
			}
		}
		for k, val := range t {
			t[k] = normalizeAnthropicSchemaTypes(val)
		}
		return t
	case []interface{}:
		for i := range t {
			t[i] = normalizeAnthropicSchemaTypes(t[i])
		}
		return t
	default:
		return v
	}
}

// dropNullFromType removes every "null" member from a union "type" array. It
// returns the rewritten value (a scalar string when exactly one non-null member
// remains, otherwise the trimmed array) and whether anything changed. If no
// non-null member remains, the original is returned unchanged (changed=false) so
// a degenerate ["null"]-only enum field is left for the caller to keep as-is.
func dropNullFromType(union []interface{}) (interface{}, bool) {
	kept := make([]interface{}, 0, len(union))
	sawNull := false
	for _, m := range union {
		if s, ok := m.(string); ok && s == "null" {
			sawNull = true
			continue
		}
		kept = append(kept, m)
	}
	if !sawNull || len(kept) == 0 {
		return union, false
	}
	if len(kept) == 1 {
		return kept[0], true
	}
	return kept, true
}

// geminiResponse is the native :generateContent response, and also the shape of
// each :streamGenerateContent SSE chunk (a partial GenerateContentResponse).
type geminiResponse struct {
	Candidates    []geminiCandidate    `json:"candidates"`
	UsageMetadata *geminiUsageMetadata `json:"usageMetadata"`
}

type geminiCandidate struct {
	Content struct {
		Parts []geminiRespPart `json:"parts"`
		Role  string           `json:"role"`
	} `json:"content"`
	FinishReason string `json:"finishReason"`
}

// geminiRespPart is one response/stream content part: visible text, a reasoning
// summary (Thought true), or a complete functionCall.
type geminiRespPart struct {
	Text         string                  `json:"text"`
	Thought      bool                    `json:"thought"`
	FunctionCall *geminiRespFunctionCall `json:"functionCall"`
	// ThoughtSignature is the opaque, base64 signature Vertex attaches to a Gemini
	// 3.x part (sibling to functionCall). It is captured for functionCall parts so
	// the agent loop can echo it back on later turns (issue #573).
	ThoughtSignature string `json:"thoughtSignature"`
}

type geminiRespFunctionCall struct {
	Name string          `json:"name"`
	Args json.RawMessage `json:"args"`
	ID   string          `json:"id"`
}

type geminiUsageMetadata struct {
	PromptTokenCount        int `json:"promptTokenCount"`
	CandidatesTokenCount    int `json:"candidatesTokenCount"`
	TotalTokenCount         int `json:"totalTokenCount"`
	ThoughtsTokenCount      int `json:"thoughtsTokenCount"`
	CachedContentTokenCount int `json:"cachedContentTokenCount"`
}

// toTokenUsage maps Gemini usage onto gogent's TokenUsage. candidatesTokenCount
// excludes thinking, which is reported separately as thoughtsTokenCount and
// surfaced as ReasoningTokens; cachedContentTokenCount becomes Cache.ReadTokens.
func (u *geminiUsageMetadata) toTokenUsage() *TokenUsage {
	if u == nil {
		return nil
	}
	return &TokenUsage{
		PromptTokens:     u.PromptTokenCount,
		CompletionTokens: u.CandidatesTokenCount,
		TotalTokens:      u.TotalTokenCount,
		ReasoningTokens:  u.ThoughtsTokenCount,
		// Gemini reports cache reads only (cachedContentTokenCount); no write count.
		Cache: CacheStats{ReadTokens: u.CachedContentTokenCount},
	}
}

func (geminiAdapter) parseResponse(body []byte) (*CompletionResponse, error) {
	var gr geminiResponse
	if err := json.Unmarshal(body, &gr); err != nil {
		return nil, fmt.Errorf("unmarshal response: %w", err)
	}
	// A well-formed :generateContent response always carries at least one
	// candidate; none means a malformed (or prompt-blocked) provider response, so
	// surface it as an error rather than a silent blank completion (eval-safety),
	// mirroring how parseStream rejects a corrupt SSE chunk.
	if len(gr.Candidates) == 0 {
		return nil, fmt.Errorf("gemini response has no candidates")
	}
	resp := &CompletionResponse{Role: RoleAssistant}
	var text, reasoning strings.Builder
	cand := gr.Candidates[0]
	for _, p := range cand.Content.Parts {
		switch {
		case p.FunctionCall != nil:
			tc, err := geminiToolCall(p.FunctionCall, p.ThoughtSignature)
			if err != nil {
				return nil, err
			}
			resp.ToolCalls = append(resp.ToolCalls, tc)
		case p.Thought:
			// Reasoning summary — not part of the visible answer; its token cost
			// is reported via usageMetadata.thoughtsTokenCount (ReasoningTokens).
			// Retain its text so a thinking-only turn is recoverable (issue #402).
			reasoning.WriteString(p.Text)
		default:
			text.WriteString(p.Text)
		}
	}
	resp.Content = text.String()
	resp.Reasoning = reasoning.String()
	resp.FinishReason = geminiFinishReason(cand.FinishReason, len(resp.ToolCalls) > 0)
	resp.Usage = gr.UsageMetadata.toTokenUsage()
	return resp, nil
}

// geminiToolCall converts a Gemini functionCall part into a gogent ToolCall,
// carrying the JSON-object args through as the OpenAI arguments-as-string form.
// Empty args default to "{}" (a no-argument call); a non-empty args that is not a
// JSON object is malformed provider output and is rejected as an error rather
// than propagated into a gogent tool call (eval-safety). thoughtSig is the
// part-level thoughtSignature (sibling to the functionCall in the wire); it is
// carried on the ToolCall so the agent loop re-emits it on later turns (#573).
func geminiToolCall(fc *geminiRespFunctionCall, thoughtSig string) (ToolCall, error) {
	args := strings.TrimSpace(string(fc.Args))
	if args == "" {
		args = "{}"
	} else {
		var obj map[string]interface{}
		if err := json.Unmarshal([]byte(args), &obj); err != nil {
			return ToolCall{}, fmt.Errorf("gemini functionCall args is not a JSON object: %s", args)
		}
	}
	return ToolCall{
		ID:               fc.ID,
		Type:             "function",
		Function:         FunctionCall{Name: fc.Name, Arguments: args},
		ThoughtSignature: thoughtSig,
	}, nil
}

// geminiFinishReason maps a Gemini finishReason onto the OpenAI-style
// finish_reason gogent's callers understand. A turn carrying tool calls always
// reports "tool_calls" (Gemini reports STOP even for function calls). Safety/
// policy stops collapse to "content_filter".
func geminiFinishReason(reason string, hasToolCalls bool) string {
	if hasToolCalls {
		return "tool_calls"
	}
	switch reason {
	case "":
		return ""
	case "STOP":
		return "stop"
	case "MAX_TOKENS":
		return "length"
	case "MALFORMED_FUNCTION_CALL":
		return "tool_calls"
	case "SAFETY", "RECITATION", "BLOCKLIST", "PROHIBITED_CONTENT", "SPII", "IMAGE_SAFETY":
		return "content_filter"
	default:
		return "stop"
	}
}

func (geminiAdapter) parseStream(body io.Reader, streamCh chan<- StreamResponse) (string, *TokenUsage, error) {
	reader := bufio.NewReaderSize(body, 64*1024)

	var content strings.Builder
	var usage *TokenUsage
	var finishReason *string
	var toolCalls []ToolCall

	for {
		line, readErr := reader.ReadString('\n')
		if data, ok := strings.CutPrefix(strings.TrimSpace(line), "data:"); ok {
			data = strings.TrimSpace(data)
			if data != "" {
				var chunk geminiResponse
				// A data: frame is a complete JSON GenerateContentResponse; a frame
				// that fails to parse is a malformed/corrupt response, so surface it
				// as an error rather than silently dropping it (eval-safety: never
				// turn broken provider output into a successful empty stream).
				if err := json.Unmarshal([]byte(data), &chunk); err != nil {
					return content.String(), usage, &ModelError{
						Type:    ErrorGeneric,
						Message: fmt.Sprintf("malformed Gemini SSE chunk: %v", err),
					}
				}
				if chunk.UsageMetadata != nil {
					usage = chunk.UsageMetadata.toTokenUsage()
				}
				if len(chunk.Candidates) > 0 {
					cand := chunk.Candidates[0]
					for _, p := range cand.Content.Parts {
						switch {
						case p.FunctionCall != nil:
							// functionCall arrives complete in one chunk; accumulate
							// and emit only in the terminal Done event (as the
							// OpenAI/Anthropic adapters do).
							tc, err := geminiToolCall(p.FunctionCall, p.ThoughtSignature)
							if err != nil {
								return content.String(), usage, &ModelError{
									Type:    ErrorGeneric,
									Message: err.Error(),
								}
							}
							toolCalls = append(toolCalls, tc)
						case p.Thought:
							if p.Text != "" {
								streamCh <- StreamResponse{Reasoning: p.Text, Role: RoleAssistant}
							}
						default:
							if p.Text != "" {
								content.WriteString(p.Text)
								streamCh <- StreamResponse{Content: p.Text, Role: RoleAssistant}
							}
						}
					}
					if cand.FinishReason != "" {
						r := geminiFinishReason(cand.FinishReason, len(toolCalls) > 0)
						finishReason = &r
					}
				}
			}
		}
		if readErr != nil {
			// Gemini SSE has no [DONE] sentinel — the stream simply ends at EOF.
			if readErr == io.EOF {
				break
			}
			return content.String(), usage, &ModelError{
				Type:    ErrorGeneric,
				Message: fmt.Sprintf("error reading stream: %v", readErr),
			}
		}
	}

	streamCh <- StreamResponse{
		ToolCalls:    toolCalls,
		FinishReason: finishReason,
		Usage:        usage,
		Done:         true,
	}
	return content.String(), usage, nil
}
