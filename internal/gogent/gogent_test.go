package gogent

import (
	"context"
	"testing"

	"gogent/internal/agent"
	"gogent/internal/config"
	"gogent/internal/model"
)

func TestGogentCreate(t *testing.T) {
	g := NewGogent(t.TempDir())

	if g == nil {
		t.Error("Expected Gogent to be created")
	}

	if len(g.userSessions) != 0 {
		t.Errorf("Expected 0 sessions, got %d", len(g.userSessions))
	}
}

// TestNotificationsRoundTrip covers the issue #59 config plumbing: Notifications
// returns the defaults until SetNotifications records an explicit config, which
// is then returned verbatim. (Persistence to disk is best-effort and not
// asserted here; the accessor round-trip is the contract the UI relies on.)
func TestNotificationsRoundTrip(t *testing.T) {
	g := NewGogent(t.TempDir())

	if !g.Notifications().Enabled {
		t.Error("Notifications() should default to enabled")
	}

	custom := config.NotifyConfig{Enabled: true, Native: true, OnComplete: false}
	g.SetNotifications(custom)
	got := g.Notifications()
	if !got.Native || got.OnComplete {
		t.Errorf("Notifications() = %+v after SetNotifications, want %+v", got, custom)
	}
}

// TestBudgetRoundTrip covers the issue #63 budget plumbing: Budget() defaults to
// the zero (alerting-off) value until SetBudget records an explicit one, which is
// then returned verbatim. (Persistence to disk is best-effort and not asserted
// here; the accessor round-trip is the contract the UI relies on.)
func TestBudgetRoundTrip(t *testing.T) {
	g := NewGogent(t.TempDir())

	if g.Budget().TokenBudget != 0 {
		t.Error("Budget() should default to zero (alerting off)")
	}

	custom := config.BudgetConfig{TokenBudget: 50000, WarnFraction: 0.9}
	g.SetBudget(custom)
	got := g.Budget()
	if got.TokenBudget != 50000 || got.WarnFraction != 0.9 {
		t.Errorf("Budget() = %+v after SetBudget, want %+v", got, custom)
	}
}

func TestGogentCreateUserSession(t *testing.T) {
	g := NewGogent(t.TempDir())
	m := model.NewModelConnection()
	s := model.NewModelSession("session1", m)
	agent := agent.NewAgent("agent1", s)

	userSession := g.CreateUserSession("session1", agent)

	if g.GetUserSession("session1") != userSession {
		t.Error("Expected session to be created")
	}
}

func TestGogentGetUserSession(t *testing.T) {
	g := NewGogent(t.TempDir())
	m := model.NewModelConnection()
	s := model.NewModelSession("session2", m)
	agent := agent.NewAgent("agent2", s)

	g.CreateUserSession("session2", agent)

	session := g.GetUserSession("session2")
	if session == nil {
		t.Error("Expected session to exist")
	}
}

func TestGogentListSessions(t *testing.T) {
	g := NewGogent(t.TempDir())
	m := model.NewModelConnection()

	// Create first session
	s1 := model.NewModelSession("s1", m)
	agent1 := agent.NewAgent("a1", s1)
	g.CreateUserSession("session1", agent1)

	// Create second session
	s2 := model.NewModelSession("s2", m)
	agent2 := agent.NewAgent("a2", s2)
	g.CreateUserSession("session2", agent2)

	sessions := g.ListSessions()
	if len(sessions) != 2 {
		t.Errorf("Expected 2 sessions, got %d", len(sessions))
	}
}

func TestGogentSendMessage(t *testing.T) {
	requireModel(t)

	g := NewGogent(t.TempDir())
	m := model.NewModelConnection()
	m.SetURL(config.DefaultEndpoint())
	s := model.NewModelSession("session3", m)
	agent := agent.NewAgent("agent3", s)
	g.CreateUserSession("session3", agent)

	resp, err := g.SendMessageToSession(context.Background(), "session3", "agent3", "hi")
	if err != nil {
		t.Errorf("Unexpected error: %v", err)
	}
	if resp == nil {
		t.Error("Expected response")
	}
}

func TestGogentSendMessageNotFound(t *testing.T) {
	g := NewGogent(t.TempDir())

	_, err := g.SendMessageToSession(context.Background(), "nonexistent", "agent", "hi")
	if err == nil {
		t.Error("Expected error for non-existent session")
	}

	_, ok := err.(*SessionNotFoundError)
	if !ok {
		t.Errorf("Expected SessionNotFoundError, got %T", err)
	}
}

func TestGogentCountMessages(t *testing.T) {
	g := NewGogent(t.TempDir())
	m := model.NewModelConnection()
	s := model.NewModelSession("session4", m)
	agent := agent.NewAgent("agent4", s)
	_ = g.CreateUserSession("session4", agent)
	agent.ThoughtTrain.AddTurn([]model.Message{{Role: model.RoleUser, Content: "hi"}}, "Hello!", nil, nil)

	count := g.CountMessages("session4")
	if count != 1 {
		t.Errorf("Expected 1 message, got %d", count)
	}
}

func TestGogentCountMessagesNotFound(t *testing.T) {
	g := NewGogent(t.TempDir())

	count := g.CountMessages("nonexistent")
	if count != 0 {
		t.Errorf("Expected 0 messages, got %d", count)
	}
}

func TestGogentAddHook(t *testing.T) {
	g := NewGogent(t.TempDir())

	received := false
	g.AddHook("test1", func(event HookEvent) {
		if event.Type == HookTokenReceived {
			received = true
		}
	})

	g.NotifyHooks(HookEvent{Type: HookTokenReceived})

	if !received {
		t.Error("Expected hook to be called")
	}
}

func TestGogentRemoveHook(t *testing.T) {
	g := NewGogent(t.TempDir())

	hookCalled := false
	hook := func(event HookEvent) {
		hookCalled = true
	}

	g.AddHook("test2", hook)
	g.RemoveHook("test2")
	g.NotifyHooks(HookEvent{Type: HookTokenReceived})

	if hookCalled {
		t.Error("Expected hook not to be called after removal")
	}
}
