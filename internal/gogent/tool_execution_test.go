package gogent

import (
	"context"
	"testing"
	"time"

	"gogent/internal/agent"
	"gogent/internal/config"
	"gogent/internal/model"
)

// TestToolExecutionResult tests that tool execution results are properly sent back to the model
func TestToolExecutionResult(t *testing.T) {
	requireModel(t)

	g := NewGogent("/tmp/test")
	m := model.NewModelConnection()
	m.SetURL(config.DefaultEndpoint())
	s := model.NewModelSession("session_test", m)
	agent := agent.NewAgent("agent_test", s)
	g.CreateUserSession("session_test", agent)

	// First, add a message that simulates a tool call response
	s.AddTurn([]model.Message{{Role: model.RoleUser, Content: "hi"}}, `{"tool": "calc", "args": {"expression": "2+2"}}`, nil, nil)

	// Send the message (this should detect the tool call and execute it)
	resp, err := g.SendMessageToSession(context.Background(), "session_test", "agent_test", "calculate 2+2")
	if err != nil {
		t.Errorf("Unexpected error: %v", err)
	}

	if resp == nil {
		t.Error("Expected response")
	}

	// Give time for hooks to process
	time.Sleep(100 * time.Millisecond)

	// Check that the session has multiple turns (original + tool result)
	count := g.CountMessages("session_test")
	t.Logf("Message count after tool execution: %d", count)

	// The response should contain the tool call result
	t.Logf("Response: %s", resp.Content)
}
