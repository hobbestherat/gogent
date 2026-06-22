package gogent

import (
	"testing"

	"gogent/internal/agent"
	"gogent/internal/tool"
)

// execTodo runs the registered todo tool against session id and returns the
// structured result map (or fails the test).
func execTodo(t *testing.T, g *Gogent, id string, args map[string]interface{}) map[string]interface{} {
	t.Helper()
	resp, err := g.GetToolRegistry().ExecuteToolCall(&tool.ToolCall{
		Tool: "todo",
		Args: args,
	}, tool.ToolContext{SessionID: id, AgentID: "root"})
	if err != nil {
		t.Fatalf("todo tool errored: %v", err)
	}
	if !resp.Success {
		t.Fatalf("todo tool returned non-success: %v", resp.Error)
	}
	res, ok := resp.Result.(map[string]interface{})
	if !ok {
		t.Fatalf("todo result is not a map: %T %+v", resp.Result, resp.Result)
	}
	return res
}

// newTodoGogent builds an in-memory Gogent with one session for the todo tests.
func newTodoGogent(t *testing.T, id string) *Gogent {
	t.Helper()
	g := NewGogent("/tmp/test")
	g.store = nil // no on-disk persistence in tests
	g.NewSession(id)
	if g.GetUserSession(id) == nil {
		t.Fatalf("session %s not created", id)
	}
	return g
}

// TestTodoToolReadModeEmpty verifies that calling the todo tool with no `todos`
// argument is read mode: it must pass validation (todos was dropped from the
// schema's required list), report mode "read", and return an empty list with a
// zero summary without mutating session state (issue #263).
func TestTodoToolReadModeEmpty(t *testing.T) {
	id := "read-empty"
	g := newTodoGogent(t, id)

	res := execTodo(t, g, id, map[string]interface{}{})

	if res["mode"] != "read" {
		t.Errorf("mode = %v, want read", res["mode"])
	}
	if c, _ := res["count"].(int); c != 0 {
		t.Errorf("count = %v, want 0", res["count"])
	}
	if res["summary"] != "0 done, 0 in progress, 0 pending" {
		t.Errorf("summary = %v, want zero tally", res["summary"])
	}
	todos, ok := res["todos"].([]agent.TodoItem)
	if !ok {
		t.Fatalf("todos field is not []agent.TodoItem: %T", res["todos"])
	}
	if len(todos) != 0 {
		t.Errorf("read mode on a fresh session returned %d todos, want 0", len(todos))
	}
}

// TestTodoToolReadModeReturnsCurrentList verifies read mode returns the list a
// previous write stored, with the correct summary, and that read mode does not
// mutate the stored checklist (issue #263).
func TestTodoToolReadModeReturnsCurrentList(t *testing.T) {
	id := "read-list"
	g := newTodoGogent(t, id)
	us := g.GetUserSession(id)

	// Seed via a write.
	execTodo(t, g, id, map[string]interface{}{
		"todos": []interface{}{
			map[string]interface{}{"content": "one", "status": "completed"},
			map[string]interface{}{"content": "two", "status": "in_progress"},
			map[string]interface{}{"content": "three"},
		},
	})

	res := execTodo(t, g, id, map[string]interface{}{}) // read

	if res["mode"] != "read" {
		t.Errorf("mode = %v, want read", res["mode"])
	}
	if c, _ := res["count"].(int); c != 3 {
		t.Errorf("count = %v, want 3", res["count"])
	}
	if res["summary"] != "1 done, 1 in progress, 1 pending" {
		t.Errorf("summary = %q, want %q", res["summary"], "1 done, 1 in progress, 1 pending")
	}
	todos := res["todos"].([]agent.TodoItem)
	want := []string{"one", "two", "three"}
	for i, w := range want {
		if todos[i].Content != w {
			t.Errorf("todos[%d].Content = %q, want %q", i, todos[i].Content, w)
		}
	}

	// Read must not have changed the stored checklist.
	if len(us.Todos()) != 3 {
		t.Errorf("read mode mutated the checklist: now %d items", len(us.Todos()))
	}
}

// TestTodoToolWriteEchoesStoredList verifies write mode returns the stored list
// (not just {success, count}), with mode "write", a count, a summary and the
// normalized items — so the call's effect is unambiguous in the transcript
// (issue #263, fixes #1).
func TestTodoToolWriteEchoesStoredList(t *testing.T) {
	id := "write-echo"
	g := newTodoGogent(t, id)

	res := execTodo(t, g, id, map[string]interface{}{
		"todos": []interface{}{
			map[string]interface{}{"content": "read", "status": "completed"},
			map[string]interface{}{"content": "edit", "status": "IN_PROGRESS"}, // loose case
			map[string]interface{}{"content": "verify"},                        // default pending
		},
	})

	if res["mode"] != "write" {
		t.Errorf("mode = %v, want write", res["mode"])
	}
	if res["success"] != true {
		t.Errorf("success = %v, want true", res["success"])
	}
	if c, _ := res["count"].(int); c != 3 {
		t.Errorf("count = %v, want 3", res["count"])
	}
	if res["summary"] != "1 done, 1 in progress, 1 pending" {
		t.Errorf("summary = %q, want %q", res["summary"], "1 done, 1 in progress, 1 pending")
	}
	todos, ok := res["todos"].([]agent.TodoItem)
	if !ok {
		t.Fatalf("write result has no echoed todos slice: %T", res["todos"])
	}
	if len(todos) != 3 {
		t.Fatalf("echoed %d todos, want 3", len(todos))
	}
	// Statuses are normalized in the echoed list.
	if todos[0].Status != agent.TodoCompleted ||
		todos[1].Status != agent.TodoInProgress ||
		todos[2].Status != agent.TodoPending {
		t.Errorf("echoed statuses not normalized: %+v", todos)
	}
}

// TestTodoToolWriteThenReadConsistent verifies the echoed write list and a
// subsequent read agree, and that an empty write (clearing the list) is echoed
// back as an empty list with a zero summary.
func TestTodoToolWriteThenReadConsistent(t *testing.T) {
	id := "write-read"
	g := newTodoGogent(t, id)

	execTodo(t, g, id, map[string]interface{}{
		"todos": []interface{}{
			map[string]interface{}{"content": "a", "status": "completed"},
		},
	})
	// Clear with an empty list (write mode, empty array present).
	cleared := execTodo(t, g, id, map[string]interface{}{
		"todos": []interface{}{},
	})
	if cleared["mode"] != "write" {
		t.Errorf("empty write mode = %v, want write", cleared["mode"])
	}
	if c, _ := cleared["count"].(int); c != 0 {
		t.Errorf("cleared count = %v, want 0", cleared["count"])
	}
	if cleared["summary"] != "0 done, 0 in progress, 0 pending" {
		t.Errorf("cleared summary = %v", cleared["summary"])
	}

	read := execTodo(t, g, id, map[string]interface{}{})
	if c, _ := read["count"].(int); c != 0 {
		t.Errorf("read after clear count = %v, want 0", read["count"])
	}
}

// TestTodoToolNoteParsedAndEchoed verifies the optional `note` field is parsed,
// trimmed, stored on the TodoItem, and echoed back in the tool result (issue
// #263, part C).
func TestTodoToolNoteParsedAndEchoed(t *testing.T) {
	id := "note"
	g := newTodoGogent(t, id)
	us := g.GetUserSession(id)

	res := execTodo(t, g, id, map[string]interface{}{
		"todos": []interface{}{
			map[string]interface{}{
				"content": "Read main.go",
				"status":  "completed",
				"note":    "  found the bug on line 42  ",
			},
			map[string]interface{}{"content": "no note here"},
		},
	})

	todos := res["todos"].([]agent.TodoItem)
	if todos[0].Note != "found the bug on line 42" {
		t.Errorf("note = %q, want trimmed %q", todos[0].Note, "found the bug on line 42")
	}
	if todos[1].Note != "" {
		t.Errorf("item without a note got %q, want empty", todos[1].Note)
	}

	// Stored session state carries the same trimmed note.
	stored := us.Todos()
	if stored[0].Note != "found the bug on line 42" {
		t.Errorf("stored note = %q, want trimmed", stored[0].Note)
	}
}

// TestTodoToolNonStringNoteIgnored verifies a non-string note does not corrupt
// the item: the parser uses a comma-ok assertion, so a numeric note is dropped
// rather than panicking — but the schema declares note as a string, so the
// validator rejects it first. Either way the call must not store a bad note.
func TestTodoToolNonStringNoteIgnored(t *testing.T) {
	id := "bad-note"
	g := newTodoGogent(t, id)
	us := g.GetUserSession(id)

	resp, err := g.GetToolRegistry().ExecuteToolCall(&tool.ToolCall{
		Tool: "todo",
		Args: map[string]interface{}{
			"todos": []interface{}{
				map[string]interface{}{"content": "x", "note": 123},
			},
		},
	}, tool.ToolContext{SessionID: id, AgentID: "root"})
	if err != nil {
		// validateArgs rejected the wrong-typed note; acceptable and safe.
		return
	}
	if resp.Success {
		// If it was accepted, the note must have been dropped to empty, never 123.
		if got := us.Todos(); len(got) == 1 && got[0].Note != "" {
			t.Errorf("non-string note stored as %q, want dropped", got[0].Note)
		}
	}
}

// TestTodoToolReadModeExplicitNull verifies that an explicit JSON null for
// `todos` is treated as read mode, not a rejected call: a model that sends
// {"todos": null} meaning "just read" must get the current list back (issue
// #263). The driver's fix added `if !present || raw == nil` in the tool's
// Execute to handle this.
//
// DEFECT (round 2): this fails. validateArgs (internal/tool/validate.go) runs in
// ExecuteToolCall BEFORE Execute and rejects a null `todos` against its declared
// `type: array` ("invalid args: args.todos: expected array, got null"). The
// request therefore never reaches the `raw == nil` branch, so that branch is
// dead code for the null case. The fix is at the wrong layer — the schema must
// permit null (e.g. drop `type` on `todos`, or allow ["array","null"]) for the
// explicit-null read path to work end-to-end. The OMITTED case ({} with no todos
// key) works, because validateObject only validates properties that are present.
func TestTodoToolReadModeExplicitNull(t *testing.T) {
	id := "read-null"
	g := newTodoGogent(t, id)

	// Seed a list via a normal write.
	execTodo(t, g, id, map[string]interface{}{
		"todos": []interface{}{
			map[string]interface{}{"content": "seeded", "status": "in_progress"},
		},
	})

	// Read it back via an explicit null. This must succeed as read mode.
	resp, err := g.GetToolRegistry().ExecuteToolCall(&tool.ToolCall{
		Tool: "todo",
		Args: map[string]interface{}{"todos": nil},
	}, tool.ToolContext{SessionID: id, AgentID: "root"})
	if err != nil {
		t.Fatalf("explicit-null read errored: %v", err)
	}
	if !resp.Success {
		t.Fatalf("explicit-null todos was rejected instead of treated as read mode: %q "+
			"(validateArgs rejects null before Execute's raw==nil branch can run)", resp.Error)
	}
	res, ok := resp.Result.(map[string]interface{})
	if !ok {
		t.Fatalf("result not a map: %T", resp.Result)
	}
	if res["mode"] != "read" {
		t.Errorf("explicit null mode = %v, want read", res["mode"])
	}
	todos, ok := res["todos"].([]agent.TodoItem)
	if !ok || len(todos) != 1 || todos[0].Content != "seeded" {
		t.Errorf("explicit null read did not return the current list: %+v", res["todos"])
	}

	// The read must not have cleared the seeded list.
	if len(g.GetUserSession(id).Todos()) != 1 {
		t.Errorf("explicit null read mutated the checklist")
	}
}

// TestTodoToolNonArrayTodosRejected guards the round-2 schema change: dropping
// `"type": "array"` from the todos property (so an explicit null reaches read
// mode) must NOT open a hole where a non-array, non-null todos silently slips
// through. A string/number/object todos is present and non-nil, so it takes the
// write path and parseTodoItems must reject it with a clear error — never panic,
// never store garbage (issue #263).
func TestTodoToolNonArrayTodosRejected(t *testing.T) {
	id := "non-array"
	g := newTodoGogent(t, id)

	for _, bad := range []interface{}{
		"not an array",
		float64(42),
		map[string]interface{}{"content": "looks like one item but is an object"},
		true,
	} {
		resp, err := g.GetToolRegistry().ExecuteToolCall(&tool.ToolCall{
			Tool: "todo",
			Args: map[string]interface{}{"todos": bad},
		}, tool.ToolContext{SessionID: id, AgentID: "root"})
		if err == nil && resp.Success {
			t.Errorf("non-array todos %#v was accepted, want rejection: %+v", bad, resp)
		}
		// Whatever happened, the checklist must remain empty (nothing stored).
		if n := len(g.GetUserSession(id).Todos()); n != 0 {
			t.Errorf("non-array todos %#v mutated the checklist (now %d items)", bad, n)
		}
	}
}

// TestTodoToolReadModeUnknownSession verifies read mode still reports a clear
// error for a session that does not exist (the session lookup precedes the
// read/write branch).
func TestTodoToolReadModeUnknownSession(t *testing.T) {
	g := NewGogent("/tmp/test")
	g.store = nil
	resp, err := g.GetToolRegistry().ExecuteToolCall(&tool.ToolCall{
		Tool: "todo",
		Args: map[string]interface{}{}, // read mode, no session
	}, tool.ToolContext{SessionID: "ghost"})
	if err == nil && resp.Success {
		t.Errorf("expected failure for unknown session in read mode, got %+v", resp)
	}
}
