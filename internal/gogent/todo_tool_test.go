package gogent

import (
	"testing"

	"gogent/internal/tool"
)

// TestTodoToolUpdatesSessionTodos verifies the registered todo tool parses its
// todos argument into the session's checklist (normalizing statuses) and that
// the session observes the update (issue #43).
func TestTodoToolUpdatesSessionTodos(t *testing.T) {
	g := NewGogent("/tmp/test")
	g.store = nil // avoid on-disk persistence during the test

	id := "todo-session"
	g.NewSession(id)
	us := g.GetUserSession(id)
	if us == nil {
		t.Fatalf("session %s not created", id)
	}

	resp, err := g.GetToolRegistry().ExecuteToolCall(&tool.ToolCall{
		Tool: "todo",
		Args: map[string]interface{}{
			"todos": []interface{}{
				map[string]interface{}{"content": "read main.go", "status": "completed"},
				map[string]interface{}{"content": "edit main.go", "status": "in_progress"},
				map[string]interface{}{"content": "run tests"},
			},
		},
	}, tool.ToolContext{SessionID: id, AgentID: "root"})
	if err != nil {
		t.Fatalf("todo tool failed: %v", err)
	}
	if !resp.Success {
		t.Fatalf("todo tool returned non-success: %v", resp.Error)
	}

	todos := us.Todos()
	if len(todos) != 3 {
		t.Fatalf("expected 3 todos, got %d: %+v", len(todos), todos)
	}
	want := []string{"read main.go", "edit main.go", "run tests"}
	for i, w := range want {
		if todos[i].Content != w {
			t.Errorf("todos[%d].Content = %q, want %q", i, todos[i].Content, w)
		}
	}
	if todos[0].Status != "completed" || todos[1].Status != "in_progress" || todos[2].Status != "pending" {
		t.Errorf("statuses not normalized: %+v", todos)
	}
}

// TestTodoToolRejectsBadArgs verifies the todo tool validates its argument shape
// rather than silently storing a malformed checklist (issue #43).
func TestTodoToolRejectsBadArgs(t *testing.T) {
	g := NewGogent("/tmp/test")
	g.store = nil
	g.NewSession("s")

	tests := []struct {
		name string
		args map[string]interface{}
	}{
		{"missing todos key", map[string]interface{}{}},
		{"todos not array", map[string]interface{}{"todos": "nope"}},
		{"empty content", map[string]interface{}{"todos": []interface{}{
			map[string]interface{}{"content": "  "},
		}}},
		{"item not object", map[string]interface{}{"todos": []interface{}{"nope"}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp, err := g.GetToolRegistry().ExecuteToolCall(&tool.ToolCall{
				Tool: "todo", Args: tt.args,
			}, tool.ToolContext{SessionID: "s", AgentID: "root"})
			if err == nil && resp.Success {
				t.Errorf("expected failure for %s, got success: %+v", tt.name, resp)
			}
		})
	}
}

// TestTodoToolUnknownSession verifies the tool reports a clear error when the
// session does not exist (e.g. an MCP/headless caller with a stale id).
func TestTodoToolUnknownSession(t *testing.T) {
	g := NewGogent("/tmp/test")
	g.store = nil
	resp, err := g.GetToolRegistry().ExecuteToolCall(&tool.ToolCall{
		Tool: "todo",
		Args: map[string]interface{}{"todos": []interface{}{
			map[string]interface{}{"content": "x"},
		}},
	}, tool.ToolContext{SessionID: "nope"})
	if err == nil && resp.Success {
		t.Errorf("expected failure for unknown session, got %+v", resp)
	}
}
