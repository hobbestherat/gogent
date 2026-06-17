package model

import (
	"fmt"
	"sync"
)

// Turn represents a single exchange between user and model
type Turn struct {
	Request  []Message
	Response string
	Usage    *TokenUsage
	Error    *ModelError
}

// ModelSession is an internal session tied to a ModelConnection
type ModelSession struct {
	ID                string
	History           []Turn
	Model             Connector
	CurrentTokenCount int
	MaxContextLength  int
	Callbacks         []func(event CallbackEvent)
	TokenCallbacks    []TokenCallback
	mu                sync.Mutex

	// SystemPrompt is prepended (as a system message) to every request.
	SystemPrompt string
	// Transcript is the canonical running conversation: user/assistant/tool
	// messages in order. Unlike History it is what actually gets re-sent to the
	// model, so the model always sees its own prior outputs and tool results.
	Transcript []Message
}

// CallbackEvent types
type CallbackEvent struct {
	Type          CallbackEventType
	Token         string
	Response      string
	Usage         *TokenUsage
	Error         *ModelError
	Compression   *CompressionInfo
	CurrentTokens int
}

// TokenCallback is a callback that receives token usage information
type TokenCallback func(promptTokens, completionTokens int)

type CallbackEventType string

const (
	EventTokenReceived    CallbackEventType = "token_received"
	EventResponseComplete CallbackEventType = "response_complete"
	EventError            CallbackEventType = "error"
	EventCompression      CallbackEventType = "compression"
)

type CompressionInfo struct {
	Before int
	After  int
}

// CompressionResult represents the result of compression
type CompressionResult struct {
	Content    string
	TokenCount int
	Method     string
}

// NewModelSession creates a new model session
func NewModelSession(id string, model Connector) *ModelSession {
	return &ModelSession{
		ID:                id,
		History:           []Turn{},
		Model:             model,
		CurrentTokenCount: 0,
		MaxContextLength:  4096, // Default context length
		Callbacks:         []func(event CallbackEvent){},
		Transcript:        []Message{},
	}
}

// SetSystemPrompt sets the system prompt prepended to every request.
func (s *ModelSession) SetSystemPrompt(prompt string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.SystemPrompt = prompt
}

// GetTranscript returns a copy of the canonical conversation transcript.
func (s *ModelSession) GetTranscript() []Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Message, len(s.Transcript))
	copy(out, s.Transcript)
	return out
}

// AppendMessages adds messages to the transcript without sending a request.
func (s *ModelSession) AppendMessages(messages ...Message) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Transcript = append(s.Transcript, messages...)
}

// ReplaceTranscript replaces the conversation transcript (used by compression).
func (s *ModelSession) ReplaceTranscript(messages []Message) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Transcript = append([]Message(nil), messages...)
}

// SetMaxContextLength sets the maximum context length for this session
func (s *ModelSession) SetMaxContextLength(length int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.MaxContextLength = length
}

// AddTurn adds a turn to the session history
func (s *ModelSession) AddTurn(request []Message, response string, usage *TokenUsage, err *ModelError) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.History = append(s.History, Turn{
		Request:  request,
		Response: response,
		Usage:    usage,
		Error:    err,
	})

	// Update token count
	if usage != nil {
		s.CurrentTokenCount += usage.TotalTokens
	}
}

// GetHistory returns a copy of the history
func (s *ModelSession) GetHistory() []Turn {
	s.mu.Lock()
	defer s.mu.Unlock()

	history := make([]Turn, len(s.History))
	copy(history, s.History)
	return history
}

// GetCurrentTokenCount returns the current token count
func (s *ModelSession) GetCurrentTokenCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.CurrentTokenCount
}

// GetTokenCount returns current token count
func (s *ModelSession) GetTokenCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.CurrentTokenCount
}

// NeedsCompression checks if compression is needed
func (s *ModelSession) NeedsCompression() bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Trigger compression when at 80% of limit
	threshold := int(float64(s.MaxContextLength) * 0.8)
	return s.CurrentTokenCount >= threshold
}

// Compress compresses the context using smart compression
func (s *ModelSession) Compress() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if len(s.History) <= 1 {
		return nil // Keep at least one turn
	}

	// Build messages from history
	messages := s.buildMessagesFromHistory()

	// Get compressed result
	compressed, err := s.performCompression(messages)
	if err != nil {
		// Fall back to simple compression if smart compression fails
		return s.simpleCompress()
	}

	// Recalculate token count
	newCount := compressed.TokenCount
	s.CurrentTokenCount = newCount

	// Notify callbacks
	for _, cb := range s.Callbacks {
		cb(CallbackEvent{
			Type:          EventCompression,
			Compression:   &CompressionInfo{Before: s.CurrentTokenCount, After: newCount},
			CurrentTokens: newCount,
		})
	}

	return nil
}

// performCompression performs smart compression
func (s *ModelSession) performCompression(messages []Message) (*CompressionResult, error) {
	// For now, fall back to simple compression
	// In production, would integrate with compression package
	return s.simpleCompressResult(messages)
}

// simpleCompressResult does simple compression and returns result
func (s *ModelSession) simpleCompressResult(messages []Message) (*CompressionResult, error) {
	// Keep last message
	if len(messages) <= 1 {
		return &CompressionResult{
			Content:    s.buildConversationSummary(messages),
			TokenCount: s.estimateTokenCount(messages),
			Method:     "simple_last_only",
		}, nil
	}

	lastMsg := messages[len(messages)-1]
	return &CompressionResult{
		Content:    fmt.Sprintf("Latest:\n%s", lastMsg.Content),
		TokenCount: len(lastMsg.Content) / 4,
		Method:     "simple_latest_only",
	}, nil
}

// simpleCompress does simple compression (old behavior)
func (s *ModelSession) simpleCompress() error {
	if len(s.History) <= 1 {
		return nil
	}

	lastTurn := s.History[len(s.History)-1]
	s.History = []Turn{s.History[0], lastTurn}

	newCount := 0
	for _, turn := range s.History {
		if turn.Usage != nil {
			newCount += turn.Usage.TotalTokens
		}
	}
	s.CurrentTokenCount = newCount

	return nil
}

// buildMessagesFromHistory builds messages from history turns
func (s *ModelSession) buildMessagesFromHistory() []Message {
	messages := []Message{}
	for _, turn := range s.History {
		for _, msg := range turn.Request {
			messages = append(messages, msg)
		}
		if turn.Response != "" {
			messages = append(messages, Message{
				Role:    RoleAssistant,
				Content: turn.Response,
			})
		}
	}
	return messages
}

// buildConversationSummary builds a summary of messages
func (s *ModelSession) buildConversationSummary(messages []Message) string {
	var summary string
	for _, msg := range messages {
		if msg.Role == RoleUser {
			summary += fmt.Sprintf("[User]: %s\n", msg.Content)
		} else if msg.Role == RoleAssistant {
			summary += fmt.Sprintf("[Assistant]: %s\n", msg.Content)
		}
	}
	return summary
}

// estimateTokenCount estimates token count
func (s *ModelSession) estimateTokenCount(messages []Message) int {
	total := 0
	for _, msg := range messages {
		total += len(msg.Content) / 4
	}
	return total
}

// Resume resumes the session on a new model backend
func (s *ModelSession) Resume(newModel Connector) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.Model = newModel

	// Reset token count if different model
	if newModel != s.Model {
		newCount := 0
		for _, turn := range s.History {
			if turn.Usage != nil {
				newCount += turn.Usage.TotalTokens
			}
		}
		s.CurrentTokenCount = newCount
	}
}

// AddCallback adds a callback hook
func (s *ModelSession) AddCallback(cb func(event CallbackEvent)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Callbacks = append(s.Callbacks, cb)
}

// RemoveCallback removes a callback hook
func (s *ModelSession) RemoveCallback(cb func(event CallbackEvent)) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.Callbacks = nil // Simplified: remove all for now
}

// AddTokenCallback adds a token usage callback
func (s *ModelSession) AddTokenCallback(cb TokenCallback) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.TokenCallbacks = append(s.TokenCallbacks, cb)
}

// NotifyTokenReceived notifies callbacks of a received token
func (s *ModelSession) NotifyTokenReceived(token string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.CurrentTokenCount += len(token) // Approximate token count
	for _, cb := range s.Callbacks {
		cb(CallbackEvent{
			Type:          EventTokenReceived,
			Token:         token,
			CurrentTokens: s.CurrentTokenCount,
		})
	}
}

// NotifyResponseComplete notifies callbacks of a complete response
func (s *ModelSession) NotifyResponseComplete(response string, usage *TokenUsage) {
	s.mu.Lock()
	defer s.mu.Unlock()

	for _, cb := range s.Callbacks {
		cb(CallbackEvent{
			Type:          EventResponseComplete,
			Response:      response,
			Usage:         usage,
			CurrentTokens: s.CurrentTokenCount,
		})
	}
}

// Send sends a message to the model and returns the response.
// It maintains a canonical transcript so the model always sees the full prior
// conversation (including its own assistant turns and tool results).
func (s *ModelSession) Send(messages []Message) (*CompletionResponse, error) {
	return s.SendWithTools(messages, nil)
}

// SendWithTools is like Send but advertises native tools to the model.
func (s *ModelSession) SendWithTools(messages []Message, tools []ToolDef) (*CompletionResponse, error) {
	s.mu.Lock()
	// Append the new messages to the transcript.
	s.Transcript = append(s.Transcript, messages...)

	// Build the full request: system prompt (if any) + entire transcript.
	fullMessages := make([]Message, 0, len(s.Transcript)+1)
	if s.SystemPrompt != "" {
		fullMessages = append(fullMessages, Message{Role: RoleSystem, Content: s.SystemPrompt})
	}
	fullMessages = append(fullMessages, s.Transcript...)

	// Record a history turn for stats/back-compat.
	s.History = append(s.History, Turn{Request: messages})
	s.mu.Unlock()

	resp, err := s.Model.CompleteWithTools(fullMessages, tools)
	if err != nil {
		s.mu.Lock()
		s.History[len(s.History)-1].Error = &ModelError{Message: err.Error()}
		s.mu.Unlock()
		return nil, err
	}

	s.mu.Lock()
	// Append the assistant turn to the transcript (content + any tool calls).
	s.Transcript = append(s.Transcript, Message{
		Role:      RoleAssistant,
		Content:   resp.Content,
		ToolCalls: resp.ToolCalls,
	})
	s.History[len(s.History)-1].Response = resp.Content
	s.History[len(s.History)-1].Usage = resp.Usage

	// Real token accounting: the latest usage reflects the whole context size.
	if resp.Usage != nil && resp.Usage.TotalTokens > 0 {
		s.CurrentTokenCount = resp.Usage.TotalTokens
		for _, cb := range s.TokenCallbacks {
			cb(resp.Usage.PromptTokens, resp.Usage.CompletionTokens)
		}
	}
	s.mu.Unlock()

	return resp, nil
}

// AppendToolResults appends tool-result messages to the transcript so the next
// Send carries them to the model in OpenAI's role:"tool" format.
func (s *ModelSession) AppendToolResults(results []Message) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Transcript = append(s.Transcript, results...)
}

// NotifyError notifies callbacks of an error

// NotifyError notifies callbacks of an error
func (s *ModelSession) NotifyError(err *ModelError) {
	s.mu.Lock()
	defer s.mu.Unlock()

	for _, cb := range s.Callbacks {
		cb(CallbackEvent{
			Type:  EventError,
			Error: err,
			Usage: nil,
			Token: "",
		})
	}
}
