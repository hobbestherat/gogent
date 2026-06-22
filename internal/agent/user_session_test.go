package agent

import (
	"sync"
	"testing"
	"time"

	"gogent/internal/model"
)

func TestUserSessionCreate(t *testing.T) {
	m := model.NewModelConnection()
	s := model.NewModelSession("test1", m)
	agent := NewAgent("agent1", s)
	userSession := NewUserSession("session1", agent)

	if userSession.ID != "session1" {
		t.Errorf("Expected ID 'session1', got %q", userSession.ID)
	}

	if userSession.RootAgent != agent {
		t.Error("Expected root agent to be set")
	}
}

func TestUserSessionListAgents(t *testing.T) {
	m := model.NewModelConnection()
	s := model.NewModelSession("test2", m)
	root := NewAgent("root", s)
	child := NewAgent("child", s)
	root.AddSubAgent(child)

	userSession := NewUserSession("session2", root)

	agents := userSession.ListAgents()
	if len(agents) != 2 {
		t.Errorf("Expected 2 agents, got %d", len(agents))
	}
}

func TestUserSessionGetAgent(t *testing.T) {
	m := model.NewModelConnection()
	s := model.NewModelSession("test3", m)
	root := NewAgent("root", s)
	child := NewAgent("child", s)
	root.AddSubAgent(child)

	userSession := NewUserSession("session3", root)

	found := userSession.GetAgent("child")
	if found != child {
		t.Error("Expected to find child agent")
	}

	notFound := userSession.GetAgent("nonexistent")
	if notFound != nil {
		t.Error("Expected not to find non-existent agent")
	}
}

func TestUserSessionAddAgent(t *testing.T) {
	m := model.NewModelConnection()
	s := model.NewModelSession("test4", m)
	root := NewAgent("root", s)
	userSession := NewUserSession("session4", root)

	newAgent := NewAgent("new", s)
	err := userSession.AddAgent("root", newAgent)
	if err != nil {
		t.Errorf("Unexpected error: %v", err)
	}

	found := userSession.GetAgent("new")
	if found != newAgent {
		t.Error("Expected to find new agent")
	}
}

func TestUserSessionSendMessage(t *testing.T) {
	requireModel(t)

	m := model.NewModelConnection()
	s := model.NewModelSession("test5", m)
	agent := NewAgent("agent", s)
	userSession := NewUserSession("session5", agent)

	_, err := userSession.SendMessage("agent", "hi")
	if err != nil {
		t.Errorf("Unexpected error: %v", err)
	}
}

func TestUserSessionStopAgent(t *testing.T) {
	m := model.NewModelConnection()
	s := model.NewModelSession("test6", m)
	agent := NewAgent("agent", s)
	userSession := NewUserSession("session6", agent)

	agent.SetState(StateThinking)
	if agent.GetState() != StateThinking {
		t.Errorf("Expected state Thinking, got %v", agent.GetState())
	}

	err := userSession.StopAgent("agent")
	if err != nil {
		t.Errorf("Unexpected error: %v", err)
	}

	if agent.GetState() != StateIdle {
		t.Errorf("Expected state Idle after stop, got %v", agent.GetState())
	}
}

func TestUserSessionResumeAgent(t *testing.T) {
	m := model.NewModelConnection()
	s := model.NewModelSession("test7", m)
	agent := NewAgent("agent", s)
	userSession := NewUserSession("session7", agent)

	agent.SetState(StateIdle)
	if agent.GetState() != StateIdle {
		t.Errorf("Expected state Idle, got %v", agent.GetState())
	}

	err := userSession.ResumeAgent("agent")
	if err != nil {
		t.Errorf("Unexpected error: %v", err)
	}

	if agent.GetState() != StateThinking {
		t.Errorf("Expected state Thinking after resume, got %v", agent.GetState())
	}
}

func TestUserSessionInterruptAgent(t *testing.T) {
	m := model.NewModelConnection()
	s := model.NewModelSession("test8", m)
	agent := NewAgent("agent", s)
	userSession := NewUserSession("session8", agent)

	agent.SetState(StateThinking)
	if agent.GetState() != StateThinking {
		t.Errorf("Expected state Thinking, got %v", agent.GetState())
	}

	err := userSession.InterruptAgent("agent")
	if err != nil {
		t.Errorf("Unexpected error: %v", err)
	}

	if agent.GetState() != StateIdle {
		t.Errorf("Expected state Idle after interrupt, got %v", agent.GetState())
	}
}

func TestUserSessionCountMessages(t *testing.T) {
	m := model.NewModelConnection()
	s := model.NewModelSession("test9", m)
	agent := NewAgent("agent", s)
	userSession := NewUserSession("session9", agent)

	agent.ThoughtTrain.AddTurn([]model.Message{{Role: model.RoleUser, Content: "hi"}}, "Hello!", nil, nil)

	count := userSession.CountMessages()
	if count != 1 {
		t.Errorf("Expected 1 message, got %d", count)
	}
}

func TestUserSessionGetStatsTotalTurns(t *testing.T) {
	m := model.NewModelConnection()
	s := model.NewModelSession("test-stats", m)
	agent := NewAgent("agent", s)
	userSession := NewUserSession("session-stats", agent)

	agent.ThoughtTrain.AddTurn([]model.Message{{Role: model.RoleUser, Content: "hi"}}, "Hello!", nil, nil)
	agent.ThoughtTrain.AddTurn([]model.Message{{Role: model.RoleUser, Content: "bye"}}, "Goodbye!", nil, nil)

	stats := userSession.GetStats()
	if got := stats["total_turns"]; got != 2 {
		t.Errorf("Expected total_turns 2, got %v", got)
	}
	if got := stats["total_turns"]; got != userSession.CountMessages() {
		t.Errorf("GetStats total_turns %v disagrees with CountMessages %d", got, userSession.CountMessages())
	}
}

// TestUserSessionGetStatsNoDeadlock exercises GetStats concurrently with
// writers that take the write lock. RWMutex is not reentrant, so the previous
// GetStats -> CountMessages re-entry would deadlock against an interleaving
// writer; this test hangs (and is killed by the test timeout) on the old code.
func TestUserSessionGetStatsNoDeadlock(t *testing.T) {
	m := model.NewModelConnection()
	s := model.NewModelSession("test-deadlock", m)
	agent := NewAgent("agent", s)
	userSession := NewUserSession("session-deadlock", agent)
	agent.ThoughtTrain.AddTurn([]model.Message{{Role: model.RoleUser, Content: "hi"}}, "Hello!", nil, nil)

	const iterations = 2000
	var wg sync.WaitGroup
	wg.Add(3)

	go func() {
		defer wg.Done()
		for i := 0; i < iterations; i++ {
			userSession.GetStats()
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < iterations; i++ {
			userSession.AddTokenUsage(1, 1)
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < iterations; i++ {
			userSession.SetObserver(nil)
		}
	}()

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("GetStats deadlocked under concurrent writers")
	}
}

func TestUserSessionNotFound(t *testing.T) {
	m := model.NewModelConnection()
	s := model.NewModelSession("test10", m)
	agent := NewAgent("agent", s)
	userSession := NewUserSession("session10", agent)

	_, err := userSession.SendMessage("nonexistent", "hi")
	if err == nil {
		t.Error("Expected error for non-existent agent")
	}

	_, ok := err.(*NotFoundError)
	if !ok {
		t.Errorf("Expected NotFoundError, got %T", err)
	}
}

// newEventTestSession builds a minimal session for exercising the
// coordinator event channel directly.
func newEventTestSession(id string) *UserSession {
	m := model.NewModelConnection()
	ms := model.NewModelSession(id, m)
	return NewUserSession(id, NewAgent("root", ms))
}

func TestNextAgentEventDeliversBufferedEvent(t *testing.T) {
	s := newEventTestSession("evt-buffered")

	s.pushAgentEvent(AgentEvent{AgentID: "a1", Type: AgentEventCompleted, Text: "done"})

	ev, ok := s.NextAgentEvent(time.Second)
	if !ok {
		t.Fatal("expected an event, got timeout")
	}
	if ev.AgentID != "a1" || ev.Type != AgentEventCompleted {
		t.Errorf("unexpected event: %+v", ev)
	}
}

func TestNextAgentEventTimesOut(t *testing.T) {
	s := newEventTestSession("evt-timeout")

	if _, ok := s.NextAgentEvent(10 * time.Millisecond); ok {
		t.Error("expected timeout when no events are queued")
	}
}

// TestNextAgentEventTerminalNotDroppedWhenFull is the regression test for
// issue #27: a terminal event pushed while the buffer is full must still be
// observable, so a coordinator waiting for completion never blocks forever.
func TestNextAgentEventTerminalNotDroppedWhenFull(t *testing.T) {
	s := newEventTestSession("evt-full")

	// Saturate the buffer with clarify events that nobody is reading.
	for i := 0; i < cap(s.agentEvents)+8; i++ {
		s.pushAgentEvent(AgentEvent{AgentID: "noise", Type: AgentEventClarify})
	}

	// The terminal completion event overflows the buffer but must survive.
	s.pushAgentEvent(AgentEvent{AgentID: "worker", Type: AgentEventCompleted, Text: "SUCCESS"})

	// Drain until we observe the terminal event. A non-positive timeout would
	// block forever if it had been dropped, so bound each wait.
	var got *AgentEvent
	for i := 0; i < cap(s.agentEvents)+16; i++ {
		ev, ok := s.NextAgentEvent(time.Second)
		if !ok {
			break
		}
		if ev.Type == AgentEventCompleted && ev.AgentID == "worker" {
			got = &ev
			break
		}
	}
	if got == nil {
		t.Fatal("terminal completion event was dropped when the buffer was full")
	}
	if got.Text != "SUCCESS" {
		t.Errorf("unexpected terminal event text: %q", got.Text)
	}
}

// TestNextAgentEventDrainsBufferBeforeOverflow verifies buffered events are
// returned ahead of spilled terminal events, and both are eventually observed.
func TestNextAgentEventDrainsBufferBeforeOverflow(t *testing.T) {
	s := newEventTestSession("evt-order")

	n := cap(s.agentEvents)
	for i := 0; i < n; i++ {
		s.pushAgentEvent(AgentEvent{AgentID: "buffered", Type: AgentEventCompleted})
	}
	// This one cannot fit and spills into pendingTerminal.
	s.pushAgentEvent(AgentEvent{AgentID: "spilled", Type: AgentEventFailed})

	buffered, spilled := 0, 0
	for i := 0; i < n+1; i++ {
		ev, ok := s.NextAgentEvent(time.Second)
		if !ok {
			t.Fatalf("timed out after %d events", buffered+spilled)
		}
		switch ev.AgentID {
		case "buffered":
			if spilled > 0 {
				t.Error("spilled event returned before buffer was fully drained")
			}
			buffered++
		case "spilled":
			spilled++
		}
	}
	if buffered != n || spilled != 1 {
		t.Errorf("expected %d buffered and 1 spilled, got %d and %d", n, buffered, spilled)
	}
}

// TestCollectToolCallsFallback covers the JSON-text tool-call fallback for
// models without native tool_calls (issue #32): formatting variations that the
// old substring matcher dropped must now resolve to calls, a structured
// final-answer object must end the turn, and several calls in one reply must all
// be collected.
func TestCollectToolCallsFallback(t *testing.T) {
	s := &UserSession{}

	tests := []struct {
		name        string
		content     string
		wantTools   []string
		wantContent string // when set, expect zero calls and resp.Content rewritten
	}{
		{
			name:      "pretty printed single call",
			content:   "{\n  \"tool\": \"read\",\n  \"args\": {\"path\": \"a\"}\n}",
			wantTools: []string{"read"},
		},
		{
			name:      "reordered keys with prose",
			content:   `Let me look: {"args":{"path":"a"},"tool":"read"}`,
			wantTools: []string{"read"},
		},
		{
			name:      "fenced multiple calls",
			content:   "```json\n{\"tool\":\"read\",\"args\":{}}\n{\"tool\":\"write\",\"args\":{}}\n```",
			wantTools: []string{"read", "write"},
		},
		{
			name:        "structured final answer",
			content:     `Done. {"response":"all set","final":true}`,
			wantContent: "all set",
		},
		{
			name:      "thinking without action yields no calls",
			content:   "I should probably read the file, but here is no JSON.",
			wantTools: nil,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			resp := &model.CompletionResponse{Content: tc.content}
			calls, _ := s.collectToolCalls(resp)
			if tc.wantContent != "" {
				if len(calls) != 0 {
					t.Fatalf("expected no calls for final answer, got %+v", calls)
				}
				if resp.Content != tc.wantContent {
					t.Errorf("resp.Content = %q, want %q", resp.Content, tc.wantContent)
				}
				return
			}
			if len(calls) != len(tc.wantTools) {
				t.Fatalf("got %d calls, want %d: %+v", len(calls), len(tc.wantTools), calls)
			}
			for i, want := range tc.wantTools {
				if calls[i].Tool != want {
					t.Errorf("call %d tool = %q, want %q", i, calls[i].Tool, want)
				}
			}
		})
	}
}
