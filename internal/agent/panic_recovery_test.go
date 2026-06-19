package agent

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gogent/internal/model"
	"gogent/internal/tool"
)

// newPanicLoopSession builds a session whose registry exposes a single tool that
// panics, so the task loop drives the panic-recovery path end to end.
func newPanicLoopSession(t *testing.T, url string) (*UserSession, *Agent) {
	t.Helper()
	conn := model.NewModelConnection()
	conn.SetURL(url)
	sess := model.NewModelSession("test", conn)

	reg := tool.NewToolRegistry()
	reg.Register(&tool.Tool{
		Name:        "boom",
		Description: "panics on use",
		InputSchema: map[string]interface{}{"type": "object"},
		Execute: func(map[string]interface{}, tool.ToolContext) (interface{}, error) {
			panic("tool exploded")
		},
	})

	ag := NewAgent("root", sess)
	ag.SetToolRegistry(reg)
	us := NewUserSession("s1", ag)
	return us, ag
}

// TestTaskLoopSurvivesToolPanic verifies that when the model asks for a tool that
// panics, the loop does not crash the process: the panic is recovered, fed back
// to the model as a tool-result error, and the loop continues to a final answer
// (issue #8).
func TestTaskLoopSurvivesToolPanic(t *testing.T) {
	fs := &fakeServer{responses: []map[string]interface{}{
		toolCallResponse("call_1", "boom", `{}`),
		finalResponse("Recovered and moved on."),
	}}
	server := httptest.NewServer(http.HandlerFunc(fs.handler))
	defer server.Close()

	us, _ := newPanicLoopSession(t, server.URL)

	responses, err := us.ExecuteTaskLoop(context.Background(), "root", "use the boom tool")
	if err != nil {
		t.Fatalf("loop should not error out on a contained tool panic: %v", err)
	}
	if len(responses) == 0 {
		t.Fatal("expected at least one model response")
	}
	if got := responses[len(responses)-1].Content; !strings.Contains(got, "Recovered") {
		t.Errorf("expected the loop to reach the final answer, got %q", got)
	}

	// The tool result sent back to the model must describe the contained panic,
	// proving recovery happened at the tool boundary rather than crashing.
	if len(fs.requests) < 2 {
		t.Fatalf("expected a second request carrying the tool result, got %d", len(fs.requests))
	}
	var sawPanicResult bool
	for _, m := range fs.requests[1] {
		if c, _ := m["content"].(string); strings.Contains(c, "panicked") {
			sawPanicResult = true
		}
	}
	if !sawPanicResult {
		t.Error("the tool result fed back to the model should report the panic")
	}
}
