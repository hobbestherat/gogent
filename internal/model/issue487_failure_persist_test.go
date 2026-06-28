package model

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

// fakeIssue487Conn is a configurable Connector for issue #487 persistence tests.
// It can return a classified *ModelError, a successful response with Usage, or
// (resp, err) together to exercise the forward-compatible error-path usage guard.
// It deliberately does NOT implement StreamingToolCompleter so sendCtx takes the
// blocking CompleteWithToolsCtx path — the path real blocking connectors use.
type fakeIssue487Conn struct {
	resp  *CompletionResponse
	err   error
	calls int
}

func (c *fakeIssue487Conn) Complete(messages []Message) (*CompletionResponse, error) {
	return c.CompleteWithTools(messages, nil)
}

func (c *fakeIssue487Conn) CompleteWithTools(messages []Message, tools []ToolDef) (*CompletionResponse, error) {
	return c.CompleteWithToolsCtx(context.Background(), messages, tools)
}

func (c *fakeIssue487Conn) CompleteWithToolsCtx(ctx context.Context, messages []Message, tools []ToolDef) (*CompletionResponse, error) {
	c.calls++
	// When both resp and err are set, return them together so we can exercise the
	// error-path usage-capture guard (a future-connector shape).
	if c.err != nil && c.resp != nil {
		return c.resp, c.err
	}
	if c.err != nil {
		return nil, c.err
	}
	if c.resp != nil {
		return c.resp, nil
	}
	return &CompletionResponse{Role: RoleAssistant, Content: "ok"}, nil
}

func (c *fakeIssue487Conn) CompleteStream(messages []Message) (<-chan StreamResponse, <-chan error) {
	ch := make(chan StreamResponse)
	errCh := make(chan error, 1)
	close(ch)
	close(errCh)
	return ch, errCh
}

func (c *fakeIssue487Conn) GetStats() *ModelStats        { return &ModelStats{} }
func (c *fakeIssue487Conn) StatsSnapshot() StatsSnapshot { return StatsSnapshot{} }

// TestSendCtxErrorPreservesTypedModelError_Issue487 is the core of gap #1: the
// classified *ModelError (Type/HTTPStatusCode/RawResponse) must be preserved on
// the error path instead of flattened to a bare &ModelError{Message:...}. Without
// the fix the persisted record would lose its classification.
func TestSendCtxErrorPreservesTypedModelError_Issue487(t *testing.T) {
	conn := &fakeIssue487Conn{err: &ModelError{
		Type:           ErrorContextOverflow,
		Message:        "context window overflow",
		HTTPStatusCode: 400,
		RawResponse:    `{"error":{"message":"too long"}}`,
	}}
	s := NewModelSession("s", conn)

	if _, err := s.SendWithToolsCtx(context.Background(), []Message{{Role: RoleUser, Content: "hi"}}, nil); err == nil {
		t.Fatal("expected SendWithToolsCtx to return the model error, got nil")
	}
	h := s.GetHistory()
	if len(h) != 1 {
		t.Fatalf("History len = %d, want 1", len(h))
	}
	me := h[0].Error
	if me == nil {
		t.Fatal("History[0].Error is nil — the typed error was not recorded on the error path")
	}
	if me.Type != ErrorContextOverflow {
		t.Errorf("Error.Type = %q, want %q (must preserve classification, not flatten)", me.Type, ErrorContextOverflow)
	}
	if me.HTTPStatusCode != 400 {
		t.Errorf("Error.HTTPStatusCode = %d, want 400", me.HTTPStatusCode)
	}
	if me.RawResponse != `{"error":{"message":"too long"}}` {
		t.Errorf("Error.RawResponse = %q, want the raw body preserved", me.RawResponse)
	}
	if me.Message != "context window overflow" {
		t.Errorf("Error.Message = %q, want original message", me.Message)
	}
}

// TestSendCtxErrorPreservesWrappedModelError_Issue487 confirms errors.As recovers
// the *ModelError through arbitrary wrapping (a connector may wrap its classified
// error before returning it).
func TestSendCtxErrorPreservesWrappedModelError_Issue487(t *testing.T) {
	inner := &ModelError{Type: ErrorRateLimit, Message: "slow down", HTTPStatusCode: 429, RawResponse: "rl"}
	conn := &fakeIssue487Conn{err: fmt.Errorf("transport: %w", inner)}
	s := NewModelSession("s", conn)

	if _, err := s.SendWithToolsCtx(context.Background(), []Message{{Role: RoleUser, Content: "hi"}}, nil); err == nil {
		t.Fatal("expected error")
	}
	me := s.GetHistory()[0].Error
	if me == nil || me.Type != ErrorRateLimit || me.HTTPStatusCode != 429 {
		t.Fatalf("typed error not recovered through wrap: %+v", me)
	}
}

// TestSendCtxErrorNonModelErrorFallsBackToGeneric_Issue487 confirms a non-*ModelError
// failure (e.g. a config error surfacing as a plain error) is recorded as ErrorGeneric
// rather than dropping the turn's failure record entirely.
func TestSendCtxErrorNonModelErrorFallsBackToGeneric_Issue487(t *testing.T) {
	conn := &fakeIssue487Conn{err: errors.New("something broke")}
	s := NewModelSession("s", conn)

	if _, err := s.SendWithToolsCtx(context.Background(), []Message{{Role: RoleUser, Content: "hi"}}, nil); err == nil {
		t.Fatal("expected error")
	}
	me := s.GetHistory()[0].Error
	if me == nil {
		t.Fatal("Error nil for a non-ModelError failure")
	}
	if me.Type != ErrorGeneric {
		t.Errorf("Type = %q, want %q for an unclassified failure", me.Type, ErrorGeneric)
	}
	if me.Message != "something broke" {
		t.Errorf("Message = %q, want the original text", me.Message)
	}
}

// TestSendCtxStampsTimestampOnBothPaths_Issue487 confirms the per-turn Timestamp
// (source of the persisted "at" field) is stamped on both the success and error
// paths, so a failed turn's record carries a real time and a compaction rewrite
// preserves the original time.
func TestSendCtxStampsTimestampOnBothPaths_Issue487(t *testing.T) {
	before := time.Now()

	// Error path.
	se := NewModelSession("s", &fakeIssue487Conn{err: &ModelError{Type: ErrorTimeout, Message: "timed out"}})
	if _, err := se.SendWithToolsCtx(context.Background(), []Message{{Role: RoleUser, Content: "hi"}}, nil); err == nil {
		t.Fatal("expected error")
	}
	if ts := se.GetHistory()[0].Timestamp; ts.IsZero() || ts.Before(before) {
		t.Errorf("error-path Timestamp = %v (zero or before send), want a fresh stamp", ts)
	}

	// Success path.
	ss := NewModelSession("s", &fakeIssue487Conn{resp: &CompletionResponse{Role: RoleAssistant, Content: "ok", Usage: &TokenUsage{PromptTokens: 1, TotalTokens: 1}}})
	if _, err := ss.SendWithToolsCtx(context.Background(), []Message{{Role: RoleUser, Content: "hi"}}, nil); err != nil {
		t.Fatalf("success send: %v", err)
	}
	if ts := ss.GetHistory()[0].Timestamp; ts.IsZero() || ts.Before(before) {
		t.Errorf("success-path Timestamp = %v, want a fresh stamp", ts)
	}
}

// TestSendCtxErrorCapturesUsageWhenProviderReturnsIt_Issue487 exercises the
// forward-compatible guard: if a connector ever returns usage alongside an error,
// it is captured on the error-path turn. No real connector does this today (both
// return resp==nil on error), so this is the only place the guard is exercised.
func TestSendCtxErrorCapturesUsageWhenProviderReturnsIt_Issue487(t *testing.T) {
	usage := &TokenUsage{PromptTokens: 4242, CompletionTokens: 0, TotalTokens: 4242, Cache: CacheStats{ReadTokens: 10}}
	conn := &fakeIssue487Conn{
		resp: &CompletionResponse{Role: RoleAssistant, Usage: usage},
		err:  &ModelError{Type: ErrorConnection, Message: "reset"},
	}
	s := NewModelSession("s", conn)

	if _, err := s.SendWithToolsCtx(context.Background(), []Message{{Role: RoleUser, Content: "hi"}}, nil); err == nil {
		t.Fatal("expected error")
	}
	turn := s.GetHistory()[0]
	if turn.Error == nil || turn.Error.Type != ErrorConnection {
		t.Fatalf("typed error not recorded: %+v", turn.Error)
	}
	if turn.Usage == nil {
		t.Fatal("Usage not captured on the error path even though the provider returned it")
	}
	if turn.Usage.PromptTokens != 4242 || turn.Usage.CachedTokens() != 10 {
		t.Errorf("captured usage = %+v, want the provider-reported prompt/cached tokens", turn.Usage)
	}
}

// TestGetHistoryFromBounds_Issue487 confirms the delta getter is panic-free at the
// boundaries and returns only History[off:]. encodeTurnMeta relies on it to copy
// just the meta delta; an out-of-range offset must yield an empty slice, not panic.
func TestGetHistoryFromBounds_Issue487(t *testing.T) {
	s := NewModelSession("s", &fakeIssue487Conn{})
	// Seed three turns directly via AddTurn (Request-only; fine for this test).
	s.AddTurn([]Message{{Role: RoleUser, Content: "a"}}, "ra", nil, nil)
	s.AddTurn([]Message{{Role: RoleUser, Content: "b"}}, "rb", nil, nil)
	s.AddTurn([]Message{{Role: RoleUser, Content: "c"}}, "rc", nil, nil)

	cases := []struct {
		name string
		off  int
		want int // expected length of returned slice
	}{
		{"zero", 0, 3},
		{"middle", 1, 2},
		{"last", 2, 1},
		{"at-len", 3, 0},
		{"past-len", 5, 0},
		{"negative", -1, 3},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := s.GetHistoryFrom(tc.off)
			if len(got) != tc.want {
				t.Fatalf("GetHistoryFrom(%d) len = %d, want %d", tc.off, len(got), tc.want)
			}
			// Confirm the copy is detached (mutating it does not affect History).
			if len(got) > 0 {
				got[0].Response = "MUTATED"
				again := s.GetHistoryFrom(0)
				if again[0].Response == "MUTATED" {
					t.Error("GetHistoryFrom returned a view into live History, not a copy")
				}
			}
		})
	}
}

// TestRestoreHistoryMetaSetsTokenCountAndIndicator_Issue487 confirms a restored
// History seeds CurrentTokenCount from the latest usage (the "context-size
// accounting lost" fix) and carries the failure indicator (last turn's Error).
func TestRestoreHistoryMetaSetsTokenCountAndIndicator_Issue487(t *testing.T) {
	s := NewModelSession("s", &fakeIssue487Conn{})
	if got := s.GetCurrentTokenCount(); got != 0 {
		t.Fatalf("fresh CurrentTokenCount = %d, want 0", got)
	}

	restored := []Turn{
		{Usage: &TokenUsage{PromptTokens: 100, CompletionTokens: 10, TotalTokens: 110}},
		{Usage: &TokenUsage{PromptTokens: 200, CompletionTokens: 20, TotalTokens: 220}},
		{Error: &ModelError{Type: ErrorRefusal, Message: "refused", HTTPStatusCode: 403}}, // last turn failed
	}
	s.RestoreHistoryMeta(restored)

	if got, want := s.GetCurrentTokenCount(), 220; got != want {
		t.Errorf("CurrentTokenCount after restore = %d, want %d (latest turn's total)", got, want)
	}
	h := s.GetHistory()
	if len(h) != 3 {
		t.Fatalf("restored History len = %d, want 3", len(h))
	}
	if h[2].Error == nil || h[2].Error.Type != ErrorRefusal {
		t.Errorf("restored last turn Error = %+v, want the refusal indicator", h[2].Error)
	}
}

// TestRestoreHistoryMetaEmptyIsNoOp_Issue487 confirms restoring an empty turn slice
// (a session with no on-disk usage/error records, e.g. an old shard) leaves
// CurrentTokenCount at 0 — the backward-compatible no-op.
func TestRestoreHistoryMetaEmptyIsNoOp_Issue487(t *testing.T) {
	s := NewModelSession("s", &fakeIssue487Conn{})
	s.RestoreHistoryMeta(nil)
	if got := s.GetCurrentTokenCount(); got != 0 {
		t.Errorf("CurrentTokenCount after empty restore = %d, want 0", got)
	}
	if got := s.HistoryLen(); got != 0 {
		t.Errorf("HistoryLen after empty restore = %d, want 0", got)
	}
}

// TestHistoryLenMatchesGetHistory_Issue487 confirms the cheap length read agrees
// with the copying reader across growth, so the meta frontier bookkeeping is sound.
func TestHistoryLenMatchesGetHistory_Issue487(t *testing.T) {
	s := NewModelSession("s", &fakeIssue487Conn{})
	for i := 0; i < 5; i++ {
		s.AddTurn([]Message{{Role: RoleUser, Content: "x"}}, "r", nil, nil)
		if s.HistoryLen() != len(s.GetHistory()) {
			t.Fatalf("after %d turns: HistoryLen=%d != len(GetHistory)=%d", i+1, s.HistoryLen(), len(s.GetHistory()))
		}
	}
}
