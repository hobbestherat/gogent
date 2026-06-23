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
// providerSpec (see providerSpec.authHeaders) because providers that share one
// wire adapter still authenticate differently (OpenAI bearer vs. Azure api-key
// vs. Gemini query-param), and some add static headers (OpenRouter attribution).
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

// adapterFor returns the wire-format adapter for an APIType, defaulting to the
// OpenAI-compatible adapter for unknown/empty types.
func adapterFor(t APIType) adapter {
	switch t {
	case APITypeAnthropic:
		return anthropicAdapter{}
	case APITypeVertex:
		// Vertex AI's OpenAI-compatible endpoint speaks the standard OpenAI wire
		// format, so it reuses the shared adapter; only its providerSpec (ADC auth
		// and a dynamic, project/location-derived base URL) differs.
		return openAIAdapter{}
	case APITypeVertexNative:
		// Vertex AI's native Gemini API speaks its own contents/parts wire format,
		// so it gets a dedicated adapter (see geminiAdapter).
		return geminiAdapter{}
	default:
		return openAIAdapter{}
	}
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
// header (required on every request); the Anthropic providerSpec attaches it as
// an extra header (see providerSpecs).
const anthropicVersion = "2023-06-01"

type anthropicAdapter struct{}

// anthropicRequest is the POST /v1/messages body. Unlike chat-completions it
// hoists the system prompt to the top level, requires max_tokens, and carries
// content-block arrays per message.
type anthropicRequest struct {
	Model       string             `json:"model"`
	MaxTokens   int                `json:"max_tokens"`
	System      string             `json:"system,omitempty"`
	Messages    []anthropicMessage `json:"messages"`
	Tools       []anthropicTool    `json:"tools,omitempty"`
	ToolChoice  interface{}        `json:"tool_choice,omitempty"`
	Temperature *float32           `json:"temperature,omitempty"`
	TopP        *float32           `json:"top_p,omitempty"`
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
	// tool_use
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`
	// tool_result
	ToolUseID string `json:"tool_use_id,omitempty"`
	Content   string `json:"content,omitempty"`
	// image
	Source *anthropicImageSource `json:"source,omitempty"`
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
}

func (anthropicAdapter) buildBody(req CompletionRequest, buf *bytes.Buffer) error {
	out := anthropicRequest{
		Model:       req.Model,
		Stream:      req.Stream,
		Temperature: req.Temperature,
		TopP:        req.TopP,
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
	var systemParts []string
	for _, m := range req.Messages {
		if m.Role == RoleSystem {
			if m.Content != "" {
				systemParts = append(systemParts, m.Content)
			}
			continue
		}
		role, blocks := anthropicBlocks(m)
		if len(blocks) == 0 {
			continue
		}
		if n := len(out.Messages); n > 0 && out.Messages[n-1].Role == role {
			out.Messages[n-1].Content = append(out.Messages[n-1].Content, blocks...)
		} else {
			out.Messages = append(out.Messages, anthropicMessage{Role: role, Content: blocks})
		}
	}
	out.System = strings.Join(systemParts, "\n\n")

	for _, t := range req.Tools {
		schema := t.Function.Parameters
		if schema == nil {
			// Anthropic requires an object schema even for a no-argument tool.
			schema = map[string]interface{}{"type": "object", "properties": map[string]interface{}{}}
		}
		out.Tools = append(out.Tools, anthropicTool{
			Name:        t.Function.Name,
			Description: t.Function.Description,
			InputSchema: schema,
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
		CachedTokens:     u.CacheReadInputTokens,
	}
}

type anthropicResponse struct {
	Content []struct {
		Type  string          `json:"type"`
		Text  string          `json:"text"`
		ID    string          `json:"id"`
		Name  string          `json:"name"`
		Input json.RawMessage `json:"input"`
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
	var text strings.Builder
	for _, b := range ar.Content {
		switch b.Type {
		case "text":
			text.WriteString(b.Text)
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
						if ev.ContentBlock != nil && ev.ContentBlock.Type == "tool_use" {
							toolsByBlock[ev.Index] = &accTool{id: ev.ContentBlock.ID, name: ev.ContentBlock.Name}
							order = append(order, ev.Index)
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
								// thinking, kept out of the visible answer (issue #217).
								if ev.Delta.Thinking != "" {
									streamCh <- StreamResponse{Reasoning: ev.Delta.Thinking, Role: RoleAssistant}
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

	var toolCalls []ToolCall
	for _, idx := range order {
		acc := toolsByBlock[idx]
		args := acc.args.String()
		if strings.TrimSpace(args) == "" {
			args = "{}"
		}
		toolCalls = append(toolCalls, ToolCall{
			ID:       acc.id,
			Type:     "function",
			Function: FunctionCall{Name: acc.name, Arguments: args},
		})
	}

	usage := promptUsage.toTokenUsage(outputTokens)
	streamCh <- StreamResponse{
		ToolCalls:    toolCalls,
		FinishReason: finishReason,
		Usage:        usage,
		Done:         true,
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
// URL path (see providerSpec.chatURLFunc) — so buildBody ignores req.Model.
type geminiAdapter struct{}

// geminiRequest is the native :generateContent / :streamGenerateContent body.
type geminiRequest struct {
	Contents          []geminiContent   `json:"contents,omitempty"`
	SystemInstruction *geminiContent    `json:"systemInstruction,omitempty"`
	Tools             []geminiTool      `json:"tools,omitempty"`
	ToolConfig        *geminiToolConfig `json:"toolConfig,omitempty"`
	GenerationConfig  *geminiGenConfig  `json:"generationConfig,omitempty"`
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

func (geminiAdapter) buildBody(req CompletionRequest, buf *bytes.Buffer) error {
	out := geminiRequest{}

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
		if n := len(out.Contents); n > 0 && out.Contents[n-1].Role == role {
			out.Contents[n-1].Parts = append(out.Contents[n-1].Parts, parts...)
		} else {
			out.Contents = append(out.Contents, geminiContent{Role: role, Parts: parts})
		}
	}
	if len(systemParts) > 0 {
		out.SystemInstruction = &geminiContent{Parts: []geminiPart{{Text: strings.Join(systemParts, "\n\n")}}}
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
		out.Tools = []geminiTool{{FunctionDeclarations: decls}}
		if req.ToolChoice != nil {
			out.ToolConfig = geminiToolConfigFor(*req.ToolChoice)
		}
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
			var args json.RawMessage
			if a := strings.TrimSpace(tc.Function.Arguments); a != "" && json.Valid([]byte(a)) {
				args = json.RawMessage(a)
			}
			parts = append(parts, geminiPart{FunctionCall: &geminiFunctionCall{
				Name: tc.Function.Name,
				Args: args,
				ID:   tc.ID,
			}})
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
	return &geminiToolConfig{FunctionCallingConfig: cfg}
}

// geminiSchema normalizes a JSON-Schema document for Gemini by deep-copying it
// (via a JSON round-trip, so it accepts a map, a struct or a json.RawMessage) and
// upper-casing every "type" enum value (string→STRING, object→OBJECT, …), which
// is Gemini's canonical Schema form. Returns nil for nil input and falls back to
// the original value if it is not JSON-encodable.
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
	return uppercaseSchemaTypes(generic)
}

// uppercaseSchemaTypes walks a decoded JSON-Schema value and upper-cases the
// value of every "type" field that is a string, recursing through objects and
// arrays. A "type" whose value is not a plain string (e.g. a ["string","null"]
// union) is left as-is — Gemini expresses nullability via the nullable field.
func uppercaseSchemaTypes(v interface{}) interface{} {
	switch t := v.(type) {
	case map[string]interface{}:
		for k, val := range t {
			if k == "type" {
				if s, ok := val.(string); ok {
					t[k] = strings.ToUpper(s)
					continue
				}
			}
			t[k] = uppercaseSchemaTypes(val)
		}
		return t
	case []interface{}:
		for i := range t {
			t[i] = uppercaseSchemaTypes(t[i])
		}
		return t
	default:
		return v
	}
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
// surfaced as ReasoningTokens; cachedContentTokenCount becomes CachedTokens.
func (u *geminiUsageMetadata) toTokenUsage() *TokenUsage {
	if u == nil {
		return nil
	}
	return &TokenUsage{
		PromptTokens:     u.PromptTokenCount,
		CompletionTokens: u.CandidatesTokenCount,
		TotalTokens:      u.TotalTokenCount,
		ReasoningTokens:  u.ThoughtsTokenCount,
		CachedTokens:     u.CachedContentTokenCount,
	}
}

func (geminiAdapter) parseResponse(body []byte) (*CompletionResponse, error) {
	var gr geminiResponse
	if err := json.Unmarshal(body, &gr); err != nil {
		return nil, fmt.Errorf("unmarshal response: %w", err)
	}
	resp := &CompletionResponse{Role: RoleAssistant}
	var text strings.Builder
	rawFinish := ""
	if len(gr.Candidates) > 0 {
		cand := gr.Candidates[0]
		rawFinish = cand.FinishReason
		for _, p := range cand.Content.Parts {
			switch {
			case p.FunctionCall != nil:
				resp.ToolCalls = append(resp.ToolCalls, geminiToolCall(p.FunctionCall))
			case p.Thought:
				// Reasoning summary — not part of the visible answer; its token cost
				// is reported via usageMetadata.thoughtsTokenCount (ReasoningTokens).
			default:
				text.WriteString(p.Text)
			}
		}
	}
	resp.Content = text.String()
	resp.FinishReason = geminiFinishReason(rawFinish, len(resp.ToolCalls) > 0)
	resp.Usage = gr.UsageMetadata.toTokenUsage()
	return resp, nil
}

// geminiToolCall converts a Gemini functionCall part into a gogent ToolCall,
// marshaling the JSON-object args back to the OpenAI arguments-as-string form and
// defaulting empty args to "{}".
func geminiToolCall(fc *geminiRespFunctionCall) ToolCall {
	args := string(fc.Args)
	if strings.TrimSpace(args) == "" {
		args = "{}"
	}
	return ToolCall{
		ID:       fc.ID,
		Type:     "function",
		Function: FunctionCall{Name: fc.Name, Arguments: args},
	}
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
				if err := json.Unmarshal([]byte(data), &chunk); err == nil {
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
								toolCalls = append(toolCalls, geminiToolCall(p.FunctionCall))
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
