package model

import (
	"context"
	"encoding/json"
	"fmt"
)

// This file defines the clean, reusable interface surface for "something that
// can talk to a model". It is intentionally split into small capability
// interfaces so the connector can later be extracted into its own standalone
// module: a downstream project that only needs blocking completions can depend
// on Completer alone, while gogent uses the full Connector (tools + streaming +
// stats) to drive multi-turn tool calling and live progress.
// Completer is the minimal capability a model backend must provide: turn a list
// of chat messages into a single completion. This is the interface most
// external callers need.
type Completer interface {
	Complete(messages []Message) (*CompletionResponse, error)
}

// ToolCompleter additionally supports advertising native (OpenAI-style) tools so
// the model can emit structured tool calls.
type ToolCompleter interface {
	CompleteWithTools(messages []Message, tools []ToolDef) (*CompletionResponse, error)
	// CompleteWithToolsCtx is CompleteWithTools bound to a context so an in-flight
	// completion can be cancelled — the agent loop uses it to make "Stop"/session
	// close abort the request instead of running to the timeout (issue #24).
	CompleteWithToolsCtx(ctx context.Context, messages []Message, tools []ToolDef) (*CompletionResponse, error)
}

// Streamer supports incremental/streaming completions. gogent uses this to
// surface live progress as tokens arrive.
type Streamer interface {
	CompleteStream(messages []Message) (<-chan StreamResponse, <-chan error)
}

// ReasoningSink receives the model's chain-of-thought (reasoning) text as it
// streams, one delta per call (issue #217). It is invoked from the streaming
// read goroutine, potentially many times per turn, so implementations must be
// cheap and non-blocking — typically just forwarding the delta onto a UI event
// channel.
type ReasoningSink func(delta string)

// StreamingToolCompleter is an optional capability: a streaming tool-calling
// completion that surfaces the model's reasoning/thinking deltas live via a
// ReasoningSink while still returning the fully assembled response (content,
// tool calls, usage) exactly like CompleteWithToolsCtx (issue #217). It is kept
// out of the core Connector because not every backend can stream tool calls or
// reasoning; callers type-assert to it and fall back to the blocking
// CompleteWithToolsCtx when it is absent or when no live thinking is wanted.
type StreamingToolCompleter interface {
	CompleteWithToolsStreamCtx(ctx context.Context, messages []Message, tools []ToolDef, onReasoning ReasoningSink) (*CompletionResponse, error)
}

// StructuredCompleter additionally supports constraining the model's output to a
// response format — typically a strict JSON schema (see JSONSchemaResponseFormat)
// — so programmatic consumers get deterministically schema-valid output instead
// of best-effort, prompt-extracted JSON (issue #49). It is kept out of the core
// Connector because not every backend enforces it; callers should type-assert to
// StructuredCompleter and fall back to a prompted/tool-based approach when absent.
type StructuredCompleter interface {
	CompleteStructuredCtx(ctx context.Context, messages []Message, tools []ToolDef, format *ResponseFormat) (*CompletionResponse, error)
}

// StatsReporter exposes accumulated usage/latency counters collected by the
// connector (token counts, request counts, error breakdown, ...).
type StatsReporter interface {
	GetStats() *ModelStats
	// StatsSnapshot returns a mutex-free, copyable view of the counters, which
	// is the safe way to read/aggregate stats from outside the connector.
	StatsSnapshot() StatsSnapshot
}

// Connector is the full model-backend surface used inside gogent. It bundles the
// capability interfaces above. ModelSession depends on this interface rather
// than the concrete *ModelConnection, which keeps the model package easy to
// lift out into its own GitHub module (and makes the backend swappable/mockable
// in tests).
type Connector interface {
	Completer
	ToolCompleter
	Streamer
	StatsReporter
}

// ModelInfo describes one model advertised by a backend's listing endpoint.
type ModelInfo struct {
	ID      string `json:"id"`
	Object  string `json:"object,omitempty"`
	OwnedBy string `json:"owned_by,omitempty"`
}

// UnmarshalJSON reads a listing entry, falling back to a "name" field when the
// OpenAI-style "id" is absent. Non-OpenAI listings key the identifier on name
// (Ollama's /api/tags, Gemini's /v1beta/models), so this lets ListModels handle
// those shapes without a per-provider response parser.
func (m *ModelInfo) UnmarshalJSON(data []byte) error {
	type alias ModelInfo // strips methods to avoid infinite recursion
	var raw struct {
		alias
		Name string `json:"name"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return fmt.Errorf("unmarshal model info: %w", err)
	}
	*m = ModelInfo(raw.alias)
	if m.ID == "" {
		m.ID = raw.Name
	}
	return nil
}

// ModelLister is an optional capability: a backend that can report which models
// it serves (the OpenAI/OpenRouter "GET /v1/models" convention). It is kept out
// of the core Connector because not every backend supports it; callers should
// type-assert to ModelLister and fall back to the configured model when absent.
type ModelLister interface {
	ListModels() ([]ModelInfo, error)
}

// Compile-time assertions that the concrete HTTP connection satisfies the full
// Connector contract and the optional model-listing capability.
var (
	_ Connector              = (*ModelConnection)(nil)
	_ ModelLister            = (*ModelConnection)(nil)
	_ StructuredCompleter    = (*ModelConnection)(nil)
	_ StreamingToolCompleter = (*ModelConnection)(nil)
)

// StatsSnapshot is a mutex-free, copyable view of a connector's accumulated
// counters. It mirrors ModelStats without the embedded sync.Mutex so it can be
// safely returned by value, logged, serialized, or summed.
type StatsSnapshot struct {
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
	GenericErrorCount          int
}

// Add returns the element-wise sum of two snapshots, which is how per-agent or
// per-session connector stats are aggregated into a grand total.
func (s StatsSnapshot) Add(other StatsSnapshot) StatsSnapshot {
	return StatsSnapshot{
		RequestCount:               s.RequestCount + other.RequestCount,
		SuccessCount:               s.SuccessCount + other.SuccessCount,
		ErrorCount:                 s.ErrorCount + other.ErrorCount,
		TotalTokensIn:              s.TotalTokensIn + other.TotalTokensIn,
		TotalCachedTokensIn:        s.TotalCachedTokensIn + other.TotalCachedTokensIn,
		TotalCacheWriteTokensIn:    s.TotalCacheWriteTokensIn + other.TotalCacheWriteTokensIn,
		TotalTokensOut:             s.TotalTokensOut + other.TotalTokensOut,
		TotalTimeMs:                s.TotalTimeMs + other.TotalTimeMs,
		TimeoutCount:               s.TimeoutCount + other.TimeoutCount,
		ContextWindowOverflowCount: s.ContextWindowOverflowCount + other.ContextWindowOverflowCount,
		RefusalCount:               s.RefusalCount + other.RefusalCount,
		GenericErrorCount:          s.GenericErrorCount + other.GenericErrorCount,
	}
}

// Sub returns the element-wise difference s-other. It is how a stable per-model
// accumulator folds in the *delta* of a connector snapshot since it was last read
// (see UserSession.recordConnectorUsage): the connector counters grow within a
// turn, so subtracting the previously-read snapshot yields just the new activity
// to attribute to the active model. The result can be negative if the connector
// was rebuilt/zeroed between reads; callers that require monotonicity guard for it.
func (s StatsSnapshot) Sub(other StatsSnapshot) StatsSnapshot {
	return StatsSnapshot{
		RequestCount:               s.RequestCount - other.RequestCount,
		SuccessCount:               s.SuccessCount - other.SuccessCount,
		ErrorCount:                 s.ErrorCount - other.ErrorCount,
		TotalTokensIn:              s.TotalTokensIn - other.TotalTokensIn,
		TotalCachedTokensIn:        s.TotalCachedTokensIn - other.TotalCachedTokensIn,
		TotalCacheWriteTokensIn:    s.TotalCacheWriteTokensIn - other.TotalCacheWriteTokensIn,
		TotalTokensOut:             s.TotalTokensOut - other.TotalTokensOut,
		TotalTimeMs:                s.TotalTimeMs - other.TotalTimeMs,
		TimeoutCount:               s.TimeoutCount - other.TimeoutCount,
		ContextWindowOverflowCount: s.ContextWindowOverflowCount - other.ContextWindowOverflowCount,
		RefusalCount:               s.RefusalCount - other.RefusalCount,
		GenericErrorCount:          s.GenericErrorCount - other.GenericErrorCount,
	}
}

// IsReset reports whether s — a delta produced by Sub — indicates the underlying
// connector was rebuilt/zeroed between the two reads: a monotonic counter going
// backwards. A live connector's counters only ever grow, so ANY negative component
// means the previous baseline no longer applies and the current snapshot should be
// treated as a fresh start. Checking every field (not just RequestCount) is what
// keeps the per-model accumulator monotonic even when a rebuild's request count
// happens to recover to its prior level while token counters drop.
func (s StatsSnapshot) IsReset() bool {
	return s.RequestCount < 0 || s.SuccessCount < 0 || s.ErrorCount < 0 ||
		s.TotalTokensIn < 0 || s.TotalCachedTokensIn < 0 || s.TotalCacheWriteTokensIn < 0 ||
		s.TotalTokensOut < 0 ||
		s.TotalTimeMs < 0 || s.TimeoutCount < 0 || s.ContextWindowOverflowCount < 0 ||
		s.RefusalCount < 0 || s.GenericErrorCount < 0
}

// Snapshot returns a mutex-free copy of the current counters.
func (s *ModelStats) Snapshot() StatsSnapshot {
	s.Mutex.Lock()
	defer s.Mutex.Unlock()
	return StatsSnapshot{
		RequestCount:               s.RequestCount,
		SuccessCount:               s.SuccessCount,
		ErrorCount:                 s.ErrorCount,
		TotalTokensIn:              s.TotalTokensIn,
		TotalCachedTokensIn:        s.TotalCachedTokensIn,
		TotalCacheWriteTokensIn:    s.TotalCacheWriteTokensIn,
		TotalTokensOut:             s.TotalTokensOut,
		TotalTimeMs:                s.TotalTimeMs,
		TimeoutCount:               s.TimeoutCount,
		ContextWindowOverflowCount: s.ContextWindowOverflowCount,
		RefusalCount:               s.RefusalCount,
		GenericErrorCount:          s.GenericErrorCount,
	}
}

// Carry folds a previously-accumulated snapshot into these counters. It is used
// when a session's model backend is swapped (ModelSession.Resume): the incoming
// connector starts with zeroed stats, so the outgoing connector's totals are
// carried over to keep per-session usage cumulative across model switches
// instead of appearing to reset to zero (issue #146).
func (s *ModelStats) Carry(prev StatsSnapshot) {
	s.Mutex.Lock()
	defer s.Mutex.Unlock()
	s.RequestCount += prev.RequestCount
	s.SuccessCount += prev.SuccessCount
	s.ErrorCount += prev.ErrorCount
	s.TotalTokensIn += prev.TotalTokensIn
	s.TotalCachedTokensIn += prev.TotalCachedTokensIn
	s.TotalCacheWriteTokensIn += prev.TotalCacheWriteTokensIn
	s.TotalTokensOut += prev.TotalTokensOut
	s.TotalTimeMs += prev.TotalTimeMs
	s.TimeoutCount += prev.TimeoutCount
	s.ContextWindowOverflowCount += prev.ContextWindowOverflowCount
	s.RefusalCount += prev.RefusalCount
	s.GenericErrorCount += prev.GenericErrorCount
}
