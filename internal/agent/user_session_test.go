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
