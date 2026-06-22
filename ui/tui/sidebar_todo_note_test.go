package ui

import (
	"strings"
	"testing"

	tv "github.com/hobbestherat/turbotui/turbotv"
	"gogent/internal/agent"
)

// TestTodoLabelWithNote covers the note rendering added in issue #263: a todo
// row is "<glyph> <content> (<note>)", the note is trimmed, and a blank/absent
// note adds no parentheses. The issue's worked example is
// "☐ Read main.go (found the bug on line 42)".
func TestTodoLabelWithNote(t *testing.T) {
	for _, tc := range []struct {
		name string
		item agent.TodoItem
		want string
	}{
		{
			"note rendered in parens",
			agent.TodoItem{Content: "Read main.go", Status: agent.TodoPending, Note: "found the bug on line 42"},
			"☐ Read main.go (found the bug on line 42)",
		},
		{
			"completed with note",
			agent.TodoItem{Content: "Done", Status: agent.TodoCompleted, Note: "shipped"},
			"✔ Done (shipped)",
		},
		{
			"note trimmed",
			agent.TodoItem{Content: "task", Status: agent.TodoInProgress, Note: "  spaced  "},
			"◐ task (spaced)",
		},
		{
			"empty note: no parens",
			agent.TodoItem{Content: "task", Status: agent.TodoPending, Note: ""},
			"☐ task",
		},
		{
			"whitespace-only note: no parens",
			agent.TodoItem{Content: "task", Status: agent.TodoPending, Note: "   "},
			"☐ task",
		},
		{
			"blank content with note keeps placeholder",
			agent.TodoItem{Content: "  ", Status: agent.TodoPending, Note: "n"},
			"☐ (empty) (n)",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := todoLabel(tc.item); got != tc.want {
				t.Errorf("todoLabel = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestSidebarDrawsTodoNote drives the real draw path and asserts a checklist
// item's note is rendered in the middle TODO region alongside its content
// (issue #263, part C: "shown in the sidebar").
func TestSidebarDrawsTodoNote(t *testing.T) {
	w := newTestWorkbench(t)
	s := w.sidebar
	s.addSession("s1", "Session 1", false)
	s.applyTodo("s1", []agent.TodoItem{
		{Content: "Investigate", Status: agent.TodoInProgress, Note: "noteworthy"},
	})
	s.focusSession("s1")

	rows := renderSidebarRows(t, w, tv.Rect{X: 0, Y: 0, W: 40, H: 24})
	todosIdx := rowWith(rows, "TODOs")
	if todosIdx < 0 {
		t.Fatalf("TODOs header missing; rows:\n%s", joinRows(rows))
	}
	overallIdx := rowWith(rows, "Overall")
	if overallIdx < 0 {
		overallIdx = len(rows)
	}
	if !anyRowInRangeContains(rows, todosIdx, overallIdx, "noteworthy") {
		t.Errorf("todo note not rendered in the middle region; rows:\n%s", joinRows(rows))
	}
}

// TestSidebarTodoNoteRowClipped verifies a long note is clipped to the content
// width like any other row content, so it cannot run past the divider/edge.
func TestSidebarTodoNoteRowClipped(t *testing.T) {
	contentW := 32 - 3
	long := agent.TodoItem{
		Content: "short",
		Status:  agent.TodoPending,
		Note:    strings.Repeat("N", contentW+20),
	}
	rendered := truncateRunes(todoLabel(long), contentW)
	if rc := len([]rune(rendered)); rc > contentW {
		t.Errorf("rendered note row = %d runes, want <= %d (%q)", rc, contentW, rendered)
	}
}
