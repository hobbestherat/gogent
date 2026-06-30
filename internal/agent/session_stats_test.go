package agent

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"gogent/internal/model"
)

// TestSessionSnapshot verifies Snapshot reads the session counters and the root
// agent's context figures into a mutex-free copy.
func TestSessionSnapshot(t *testing.T) {
	m := newTestModelConnection()
	sess := model.NewModelSession("snap", m)
	ag := NewAgent("root", sess)
	us := NewUserSession("s", ag)

	us.tokenCountIn = 100
	us.tokenCountOut = 50
	us.toolCallCount = 3
	us.turnCount = 2
	// Seed the model session's context via its recorded usage and window so the
	// snapshot reports a meaningful context percentage.
	sess.AddTurn(nil, "", &model.TokenUsage{TotalTokens: 800}, nil)
	sess.SetMaxContextLength(1000)

	got := us.Snapshot()
	want := SessionStats{Turns: 2, TokensIn: 100, TokensOut: 50, ToolCalls: 3, ContextTokens: 800, ContextWindow: 1000}
	if got != want {
		t.Errorf("Snapshot() = %+v, want %+v", got, want)
	}
}

// TestSessionSnapshotUnknownContextWindow verifies that when the model session's
// context window is unknown (0), Snapshot reports a zeroed ContextWindow — the
// signal the status bar uses to omit the ctx% segment.
func TestSessionSnapshotUnknownContextWindow(t *testing.T) {
	us, ag := newLoopSession(t, "http://unused") // no requests made
	ag.ThoughtTrain.SetMaxContextLength(0)
	us.tokenCountIn = 10

	got := us.Snapshot()
	if got.ContextWindow != 0 {
		t.Errorf("ContextWindow = %d, want 0 (unknown)", got.ContextWindow)
	}
	if got.TokensIn != 10 {
		t.Errorf("TokensIn = %d, want 10", got.TokensIn)
	}
}

// TestUsageEventEmittedThroughLoop verifies the task loop emits a SessionEventUsage
// carrying a fresh stats snapshot after each model round-trip, and that the
// snapshot reflects cumulative token usage, context size and the turn count.
func TestUsageEventEmittedThroughLoop(t *testing.T) {
	fs := &fakeServer{responses: []map[string]interface{}{
		toolCallResponse("call_1", "calc", `{"expression":"2+2"}`),
		finalResponse("The answer is 4."),
	}}
	server := httptest.NewServer(http.HandlerFunc(fs.handler))
	defer server.Close()

	us, ag := newLoopSession(t, server.URL)
	// Mirror production wiring so prompt/completion tokens accumulate on the
	// session (see gogent.CreateUserSession).
	ag.ThoughtTrain.AddTokenCallback(func(prompt, completion int) {
		us.AddTokenUsage(prompt, completion)
	})

	var (
		mu     sync.Mutex
		events []SessionEvent
	)
	us.SetObserver(func(ev SessionEvent) {
		mu.Lock()
		defer mu.Unlock()
		events = append(events, ev)
	})

	if _, err := us.ExecuteTaskLoop(context.Background(), "root", "what is 2+2?"); err != nil {
		t.Fatalf("loop returned error: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()

	var usage []SessionStats
	for _, ev := range events {
		if ev.Type == SessionEventUsage {
			usage = append(usage, ev.Stats)
		}
	}
	if len(usage) < 2 {
		t.Fatalf("expected at least one usage event per model round-trip (>=2), got %d (events: %v)", len(usage), eventTypes(events))
	}

	// The last usage snapshot reflects both round-trips.
	last := usage[len(usage)-1]
	if last.Turns != 1 {
		t.Errorf("usage snapshot Turns = %d, want 1 (one user turn)", last.Turns)
	}
	if last.TokensIn != 30 || last.TokensOut != 13 {
		t.Errorf("usage snapshot tokens = %d/%d, want 30/13 (10+20 in / 5+8 out)", last.TokensIn, last.TokensOut)
	}
	if last.ContextTokens != 28 {
		t.Errorf("usage snapshot ContextTokens = %d, want 28 (last usage total)", last.ContextTokens)
	}
	if last.ContextWindow <= 0 {
		t.Errorf("usage snapshot ContextWindow = %d, want > 0", last.ContextWindow)
	}
}

// eventTypes returns the sequence of event types for a readable failure message.
func eventTypes(events []SessionEvent) []string {
	out := make([]string, len(events))
	for i, ev := range events {
		out[i] = string(ev.Type)
	}
	return out
}
