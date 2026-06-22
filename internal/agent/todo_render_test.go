package agent

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestTodoSummaryCounts verifies TodoSummary tallies items in the documented
// done / in-progress / pending order and shape "N done, N in progress, N
// pending" (issue #263). The example in the issue is "2 done, 1 in progress, 0
// pending".
func TestTodoSummaryCounts(t *testing.T) {
	cases := []struct {
		name  string
		items []TodoItem
		want  string
	}{
		{"empty", nil, "0 done, 0 in progress, 0 pending"},
		{"empty slice", []TodoItem{}, "0 done, 0 in progress, 0 pending"},
		{
			// The exact example string from the issue: "2 done, 1 in progress,
			// 0 pending".
			"issue example",
			[]TodoItem{
				{Content: "a", Status: TodoCompleted},
				{Content: "b", Status: TodoCompleted},
				{Content: "c", Status: TodoInProgress},
			},
			"2 done, 1 in progress, 0 pending",
		},
		{
			"unknown status counts as pending",
			[]TodoItem{
				{Content: "a", Status: TodoStatus("garbage")},
				{Content: "b", Status: ""},
			},
			"0 done, 0 in progress, 2 pending",
		},
		{
			"all in progress",
			[]TodoItem{
				{Content: "a", Status: TodoInProgress},
				{Content: "b", Status: TodoInProgress},
			},
			"0 done, 2 in progress, 0 pending",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := TodoSummary(tc.items); got != tc.want {
				t.Errorf("TodoSummary = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestRenderTodosEmpty verifies an empty/nil checklist renders to "" so the
// caller (buildSystemContext) can omit the section entirely (issue #263).
func TestRenderTodosEmpty(t *testing.T) {
	if got := RenderTodos(nil); got != "" {
		t.Errorf("RenderTodos(nil) = %q, want empty", got)
	}
	if got := RenderTodos([]TodoItem{}); got != "" {
		t.Errorf("RenderTodos([]) = %q, want empty", got)
	}
}

// TestRenderTodosBlockShape exercises the full system-prompt block: the header,
// the per-item glyph rows mirroring the sidebar (☐ ◐ ✔), the optional note in
// parentheses, and the trailing summary line (issue #263).
func TestRenderTodosBlockShape(t *testing.T) {
	items := []TodoItem{
		{Content: "Read main.go", Status: TodoCompleted, Note: "found the bug on line 42"},
		{Content: "Fix the bug", Status: TodoInProgress},
		{Content: "Run tests", Status: TodoPending},
	}
	got := RenderTodos(items)

	// Header present.
	if !strings.Contains(got, "## Task checklist") {
		t.Errorf("missing checklist header:\n%s", got)
	}
	// Each status glyph appears with its content.
	for _, want := range []string{
		"✔ Read main.go",
		"◐ Fix the bug",
		"☐ Run tests",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing row %q in:\n%s", want, got)
		}
	}
	// The note is rendered in parentheses, attached to its item only.
	if !strings.Contains(got, "✔ Read main.go (found the bug on line 42)") {
		t.Errorf("note not rendered on its row:\n%s", got)
	}
	// The trailing summary reflects the live tally.
	if !strings.Contains(got, "(1 done, 1 in progress, 1 pending)") {
		t.Errorf("missing/incorrect summary line:\n%s", got)
	}
	// A note-less item must NOT gain stray parentheses.
	if strings.Contains(got, "Run tests (") {
		t.Errorf("note-less item gained parentheses:\n%s", got)
	}
}

// TestRenderTodosBlankContentPlaceholder verifies a blank/whitespace content
// renders the "(empty)" placeholder rather than an empty row, and notes are
// trimmed (issue #263). This path is reachable via SetTodos directly even though
// the tool's parseTodoItems rejects blank content.
func TestRenderTodosBlankContentPlaceholder(t *testing.T) {
	got := RenderTodos([]TodoItem{
		{Content: "   ", Status: TodoPending, Note: "  trimmed note  "},
		{Content: "", Status: TodoCompleted},
	})
	if !strings.Contains(got, "☐ (empty)") {
		t.Errorf("blank content should show (empty) placeholder:\n%s", got)
	}
	// Note is trimmed before rendering.
	if !strings.Contains(got, "(empty) (trimmed note)") {
		t.Errorf("note should be trimmed:\n%s", got)
	}
	if strings.Contains(got, "  trimmed note  ") {
		t.Errorf("untrimmed note leaked into render:\n%s", got)
	}
}

// TestRenderTodosNoteWhitespaceOnlyOmitted verifies a whitespace-only note adds
// no parentheses (it trims to empty), matching the sidebar's todoLabel behavior.
func TestRenderTodosNoteWhitespaceOnlyOmitted(t *testing.T) {
	got := RenderTodos([]TodoItem{
		{Content: "task", Status: TodoPending, Note: "   "},
	})
	if strings.Contains(got, "task (") {
		t.Errorf("whitespace-only note should not produce parentheses:\n%s", got)
	}
}

// TestTodoItemNoteOmitEmptyTag verifies the Note field uses `omitempty`, so a
// checklist serialized without notes does not emit a null/empty "note" key. The
// todo tool round-trips items through JSON to the model, so the tag matters
// (issue #263).
func TestTodoItemNoteOmitEmptyTag(t *testing.T) {
	withoutNote, err := json.Marshal(TodoItem{Content: "x", Status: TodoPending})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(withoutNote), "note") {
		t.Errorf("empty note should be omitted, got %s", withoutNote)
	}

	withNote, err := json.Marshal(TodoItem{Content: "x", Status: TodoPending, Note: "detail"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(withNote), `"note":"detail"`) {
		t.Errorf("non-empty note should serialize, got %s", withNote)
	}
}
