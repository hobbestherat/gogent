package model

import (
	"context"
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
	// compressSuppressed is the hysteresis flag for context compaction: once a
	// compaction runs it stays set until CurrentTokenCount recedes below the
	// low-water mark, preventing a summarization round-trip every turn. Its zero
	// value (false) means "armed", so freshly created sessions compact normally.
	compressSuppressed bool

	// SystemPrompt is prepended (as a system message) to every request.
	SystemPrompt string
	// Transcript is the canonical running conversation: user/assistant/tool
	// messages in order. Unlike History it is what actually gets re-sent to the
	// model, so the model always sees its own prior outputs and tool results.
	Transcript []Message
	// transcriptEpoch increments each time the transcript is replaced wholesale
	// (via ReplaceTranscript or ApplyCompressedTranscript) and is left untouched
	// by append-only growth. Persistence compares it against the epoch observed
	// at the last save to detect that a previously recorded "messages already on
	// disk" frontier no longer lines up with the in-memory transcript, so it can
	// rebuild the file instead of appending stale deltas (issue #21).
	transcriptEpoch uint64
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

// TranscriptLen returns the number of messages in the canonical transcript
// without copying it. It is the cheap read side used by persistence bookkeeping
// (issue #21) where only the length, not the contents, is needed.
func (s *ModelSession) TranscriptLen() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.Transcript)
}

// TranscriptEpoch returns a value that changes whenever the transcript is
// replaced wholesale (compaction or restore-seeding) and stays stable across
// append-only growth. See the transcriptEpoch field doc for why persistence
// tracks it (issue #21).
func (s *ModelSession) TranscriptEpoch() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.transcriptEpoch
}

// AppendMessages adds messages to the transcript without sending a request.
func (s *ModelSession) AppendMessages(messages ...Message) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Transcript = append(s.Transcript, messages...)
}

// FoldLastAssistantContent overwrites the Content of the most recent assistant
// message in the transcript (and the matching History response), so the agent
// loop can fold a terminal notice onto a turn it is finalizing. sendCtx appends
// every assistant response to the transcript before the loop decides whether to
// accept, retry or fold it, so a turn truncated by max_tokens lands as an empty
// assistant message; when the bounded retries are exhausted the loop folds an
// actionable notice here rather than leaving that empty message as the persisted
// terminal record (issue #402, B5). It is a no-op when the transcript holds no
// assistant message.
func (s *ModelSession) FoldLastAssistantContent(content string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := len(s.Transcript) - 1; i >= 0; i-- {
		if s.Transcript[i].Role != RoleAssistant {
			continue
		}
		s.Transcript[i].Content = content
		if n := len(s.History); n > 0 {
			s.History[n-1].Response = content
		}
		return
	}
}

// ReplaceTranscript replaces the conversation transcript (used by compression).
func (s *ModelSession) ReplaceTranscript(messages []Message) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Transcript = append([]Message(nil), messages...)
	s.transcriptEpoch++
}

// SetMaxContextLength sets the context window (input token budget) for this
// session. When the window actually changes — e.g. switching to a model with a
// different context size — any compaction hysteresis is cleared so the new
// window is evaluated fresh; re-setting the same value (the common per-turn
// case) leaves hysteresis intact.
func (s *ModelSession) SetMaxContextLength(length int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.MaxContextLength != length {
		s.compressSuppressed = false
	}
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

	// A turn's Usage.TotalTokens is the size of the entire context at that turn
	// (the whole conversation is re-sent every request), so it is the current
	// context size — it supersedes, rather than adds to, the running count.
	// Adding would double-count the growing prefix and inflate the count
	// quadratically (the premature-compaction bug). Mirror SendWithToolsCtx,
	// which sets CurrentTokenCount = resp.Usage.TotalTokens for the same reason.
	if usage != nil && usage.TotalTokens > 0 {
		s.CurrentTokenCount = usage.TotalTokens
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

// GetMaxContextLength returns the configured context window (input token budget)
// the conversation is calibrated against. It is the read side of
// SetMaxContextLength, exposed so UIs can report context usage without reaching
// into the session's fields.
func (s *ModelSession) GetMaxContextLength() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.MaxContextLength
}

// Compression water marks, expressed as fractions of the context window.
//
// compaction fires once the running context reaches the high-water mark. After a
// compaction it stays suppressed until the context recedes below the low-water
// mark. The band between the two is the hysteresis that keeps compaction from
// re-arming every turn (a synchronous summarization round-trip each turn) when
// the post-compaction estimate or real usage still sits near the trigger.
const (
	compressionHighWater = 0.8 // trigger compaction
	compressionLowWater  = 0.5 // re-arm / post-compaction target
)

// NeedsCompression reports whether the running context has grown past the
// compression high-water mark (80% of the configured context window).
//
// Hysteresis: a successful compaction suppresses further compression until the
// context drops below the low-water mark (50%). A normal session therefore
// compacts occasionally (at 80%, settling near 50%), not every turn. If a
// compaction cannot get the context below 50% — the verbatim recent turns alone
// are large — compression stays suppressed rather than re-firing futilely each
// turn; the remedy is a larger context window or fewer kept-recent turns.
func (s *ModelSession) NeedsCompression() bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.MaxContextLength <= 0 {
		return false
	}
	high := int(float64(s.MaxContextLength) * compressionHighWater)
	low := int(float64(s.MaxContextLength) * compressionLowWater)

	// Re-arm once the context has receded below the low-water mark.
	if s.compressSuppressed && s.CurrentTokenCount <= low {
		s.compressSuppressed = false
	}
	if s.compressSuppressed {
		return false
	}
	return s.CurrentTokenCount >= high
}

// EstimateTokens is a rough char/4 heuristic used to size a transcript between a
// compression pass and the next real (usage-reporting) send.
func EstimateTokens(messages []Message) int {
	total := 0
	for _, m := range messages {
		total += len(m.Content) / 4
		for _, tc := range m.ToolCalls {
			total += len(tc.Function.Arguments) / 4
		}
	}
	return total
}

// ApplyCompressedTranscript replaces the transcript with its compressed form and
// updates the token estimate, emitting EventCompression with the real
// before-count (captured before mutation) and the post-compression estimate. The
// system prompt is owned separately and is intentionally never part of the
// transcript, so compression cannot affect it.
func (s *ModelSession) ApplyCompressedTranscript(newTranscript []Message) {
	s.mu.Lock()
	before := s.CurrentTokenCount
	s.Transcript = append([]Message(nil), newTranscript...)
	s.transcriptEpoch++
	after := EstimateTokens(newTranscript)
	if s.SystemPrompt != "" {
		after += len(s.SystemPrompt) / 4
	}
	s.CurrentTokenCount = after
	// A compaction just ran: engage hysteresis so NeedsCompression holds off
	// until the (real, usage-corrected) count recedes below the low-water mark.
	s.compressSuppressed = true
	callbacks := append([]func(event CallbackEvent){}, s.Callbacks...)
	s.mu.Unlock()

	for _, cb := range callbacks {
		cb(CallbackEvent{
			Type:          EventCompression,
			Compression:   &CompressionInfo{Before: before, After: after},
			CurrentTokens: after,
		})
	}
}

// Resume resumes the session on a new model backend. When the backend actually
// changes, the running token count is recomputed from the recorded per-turn
// usage so it no longer reflects the previous model's (possibly differently
// sized) context window.
//
// The count is the LAST recorded turn's usage total, NOT the sum of every
// turn's total. A turn's Usage.TotalTokens is the size of the entire context at
// that turn (system prompt + full transcript + the new content), because the
// whole conversation is re-sent on every request (see SendWithToolsCtx, which
// sets CurrentTokenCount = Usage.TotalTokens). Summing the per-turn totals
// therefore counts the same growing prefix over and over and inflates the count
// roughly quadratically — which made compaction's 80% high-water mark fire far
// too early (on a 1M-window model at ~10% real usage). Taking the last turn's
// total mirrors exactly what SendWithToolsCtx records and keeps the count
// honest across the per-send connection rebuild gogent performs.
func (s *ModelSession) Resume(newModel Connector) {
	s.mu.Lock()
	defer s.mu.Unlock()

	prev := s.Model
	s.Model = newModel

	// Recompute only when the model backend changes. The comparison must be
	// against the value captured before the assignment above; comparing against
	// s.Model after the assignment is trivially false (the original bug).
	if newModel != prev {
		// Carry the outgoing backend's accumulated usage counters into the fresh
		// connector. The new connection starts with zeroed stats, so without this
		// the session's connector-level token / request / error totals would
		// appear to reset to zero on every model switch — and gogent rebuilds the
		// connection on each send, so they reset on essentially every turn (issue
		// #146). The per-turn token count is recomputed from History below.
		if prev != nil && newModel != nil {
			if from, ok := prev.(StatsReporter); ok {
				if to, ok := newModel.(StatsReporter); ok {
					if dst := to.GetStats(); dst != nil {
						dst.Carry(from.StatsSnapshot())
					}
				}
			}
		}
		// The running context size is the latest turn's usage total — it already
		// covers the whole conversation, so summing every turn's total would
		// double-count the growing prefix (see the method doc). Walk back-to-front
		// for the most recent real measurement; falls back to 0 when no turn has
		// reported usage yet.
		s.CurrentTokenCount = lastUsageTotal(s.History)
	}
}

// lastUsageTotal returns the Usage.TotalTokens of the most recent history turn
// that has one, or 0 when none does. It is the honest "current context size"
// after a model/backend swap: a turn's total already counts the entire context
// at that point, so the newest one supersedes its predecessors rather than
// adding to them.
func lastUsageTotal(history []Turn) int {
	for i := len(history) - 1; i >= 0; i-- {
		if u := history[i].Usage; u != nil && u.TotalTokens > 0 {
			return u.TotalTokens
		}
	}
	return 0
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
	return s.SendWithToolsCtx(context.Background(), messages, tools)
}

// SendWithToolsCtx is SendWithTools bound to a context, so the underlying model
// request is abandoned the moment ctx is cancelled (issue #24). The transcript
// and history are still updated for the messages we attempted to send.
func (s *ModelSession) SendWithToolsCtx(ctx context.Context, messages []Message, tools []ToolDef) (*CompletionResponse, error) {
	return s.sendCtx(ctx, messages, tools, nil)
}

// SendWithToolsStreamCtx is SendWithToolsCtx that additionally streams the
// model's chain-of-thought (reasoning) deltas to onReasoning as they arrive, so
// a caller can render live thinking and fold it when the turn completes (issue
// #217). It uses the streaming backend only when the connector implements
// StreamingToolCompleter AND onReasoning is non-nil; otherwise it falls back to
// the identical blocking path, so a backend that cannot stream — or a caller
// that does not want live thinking — is wholly unaffected. The assembled
// response and all transcript/history/token bookkeeping are identical to the
// blocking path either way.
func (s *ModelSession) SendWithToolsStreamCtx(ctx context.Context, messages []Message, tools []ToolDef, onReasoning ReasoningSink) (*CompletionResponse, error) {
	return s.sendCtx(ctx, messages, tools, onReasoning)
}

// sendCtx is the shared core of the blocking and streaming send paths. When
// onReasoning is non-nil and the backend is a StreamingToolCompleter it streams
// (forwarding reasoning deltas); otherwise it issues the blocking
// CompleteWithToolsCtx. Transcript growth, history recording and token
// accounting are the same regardless of which backend call is used.
func (s *ModelSession) sendCtx(ctx context.Context, messages []Message, tools []ToolDef, onReasoning ReasoningSink) (*CompletionResponse, error) {
	s.mu.Lock()
	// Append the new messages to the transcript.
	s.Transcript = append(s.Transcript, messages...)

	// Build the full request: system prompt (if any) + entire transcript.
	//
	// The system prompt is intentionally kept out of the transcript (so
	// compaction cannot drop or rewrite it), so it has to be prepended here.
	// That means a fresh slice copy of the transcript each turn — O(K) over a
	// K-message session. It is a shallow struct-header copy (the message
	// strings/slices are not duplicated), so the per-turn cost is small relative
	// to marshaling the body. Eliminating it entirely would mean prepending the
	// system message at marshal time (a per-adapter "2-slice writer" or a
	// System field threaded down to the adapter) rather than producing one
	// combined []Message; that is a larger change tracked as a follow-up to
	// issue #20. The expensive part — re-marshaling the body — is already
	// addressed by marshaling once per send into a pooled buffer (connection.go).
	fullMessages := make([]Message, 0, len(s.Transcript)+1)
	if s.SystemPrompt != "" {
		fullMessages = append(fullMessages, Message{Role: RoleSystem, Content: s.SystemPrompt})
	}
	fullMessages = append(fullMessages, s.Transcript...)

	// Record a history turn for stats/back-compat.
	s.History = append(s.History, Turn{Request: messages})
	s.mu.Unlock()

	// Stream (surfacing reasoning) only when a sink is wanted and the backend can
	// do it; otherwise the blocking call, which is byte-for-byte the prior path.
	var resp *CompletionResponse
	var err error
	if sc, ok := s.Model.(StreamingToolCompleter); ok && onReasoning != nil {
		resp, err = sc.CompleteWithToolsStreamCtx(ctx, fullMessages, tools, onReasoning)
	} else {
		resp, err = s.Model.CompleteWithToolsCtx(ctx, fullMessages, tools)
	}
	if err != nil {
		s.mu.Lock()
		s.History[len(s.History)-1].Error = &ModelError{Message: err.Error()}
		s.mu.Unlock()
		return nil, fmt.Errorf("complete with tools: %w", err)
	}

	s.mu.Lock()
	// Append the assistant turn to the transcript (content + any tool calls, plus
	// any extended-thinking block to replay on the next turn — required for tool
	// use with thinking enabled on Anthropic/Vertex; inert for other providers).
	s.Transcript = append(s.Transcript, Message{
		Role:              RoleAssistant,
		Content:           resp.Content,
		ToolCalls:         resp.ToolCalls,
		Thinking:          resp.Thinking,
		ThinkingSignature: resp.ThinkingSignature,
		// Retain the turn's chain-of-thought so a reasoning-only turn (empty Content)
		// is not an unrecoverable empty message, and so it survives a session reopen
		// (Reasoning is serialized; see Message.MarshalJSON). Issue #402.
		Reasoning: resp.Reasoning,
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
