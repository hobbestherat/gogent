package model

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// adapter encapsulates a provider's wire format. gogent's internal request and
// response types are OpenAI-shaped (a scalar Message.Content plus OpenAI-style
// tool calls), which serves as the lingua franca; an adapter translates that
// internal shape to and from one provider's concrete protocol:
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
	buildBody(req CompletionRequest) ([]byte, error)
	parseResponse(body []byte) (*CompletionResponse, error)
	parseStream(body io.Reader, streamCh chan<- StreamResponse) (string, *TokenUsage, error)
}

// adapterFor returns the wire-format adapter for an APIType, defaulting to the
// OpenAI-compatible adapter for unknown/empty types.
func adapterFor(t APIType) adapter {
	switch t {
	case APITypeAnthropic:
		return anthropicAdapter{}
	default:
		return openAIAdapter{}
	}
}

// ---------------------------------------------------------------------------
// OpenAI-compatible adapter (OpenAI, Z.AI, Gemini compat layer, local servers)
// ---------------------------------------------------------------------------

type openAIAdapter struct{}

func (openAIAdapter) buildBody(req CompletionRequest) ([]byte, error) {
	return json.Marshal(req)
}

func (openAIAdapter) parseResponse(body []byte) (*CompletionResponse, error) {
	var resp CompletionResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, err
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
}

type anthropicTool struct {
	Name        string      `json:"name"`
	Description string      `json:"description,omitempty"`
	InputSchema interface{} `json:"input_schema"`
}

func (anthropicAdapter) buildBody(req CompletionRequest) ([]byte, error) {
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

	return json.Marshal(out)
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
		if m.Content == "" {
			return "user", nil
		}
		return "user", []anthropicContent{{Type: "text", Text: m.Content}}
	}
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
		return nil, err
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
