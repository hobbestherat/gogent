package agent

import (
	"sync"
	"testing"

	"gogent/internal/model"
)

// newTestSession builds a minimal user session + root agent for unit tests that
// do not need a reachable model (the connection is never dialed here).
func newTestSession(id string) *UserSession {
	sess := model.NewModelSession("test", model.NewModelConnection())
	ag := NewAgent("root", sess)
	return NewUserSession(id, ag)
}

// TestSetTodosStoresEmitsAndCopies verifies SetTodos records the checklist,
// emits a SessionEventTodo carrying the items, and that Todos() returns a copy a
// caller cannot mutate (issue #43).
func TestSetTodosStoresEmitsAndCopies(t *testing.T) {
	us := newTestSession("s1")

	var mu sync.Mutex
	var got []SessionEvent
	us.SetObserver(func(ev SessionEvent) {
		mu.Lock()
		defer mu.Unlock()
		got = append(got, ev)
	})

	items := []TodoItem{
		{Content: "read file", Status: TodoCompleted},
		{Content: "edit file", Status: TodoInProgress},
		{Content: "verify", Status: TodoPending},
	}
	us.SetTodos(items)

	// One todo event carrying the items in order.
	mu.Lock()
	defer mu.Unlock()
	count := 0
	for _, ev := range got {
		if ev.Type == SessionEventTodo {
			count++
			if len(ev.Todos) != 3 || ev.Todos[0].Content != "read file" ||
				ev.Todos[1].Status != TodoInProgress || ev.Todos[2].Status != TodoPending {
				t.Errorf("todo event = %+v, want the three items in order", ev.Todos)
			}
		}
	}
	if count != 1 {
		t.Errorf("expected 1 todo event, got %d", count)
	}

	// Todos() returns the stored items.
	stored := us.Todos()
	if len(stored) != 3 || stored[0].Content != "read file" {
		t.Errorf("Todos() = %+v, want the three stored items", stored)
	}

	// Mutating the returned slice must not affect session state.
	stored[0].Content = "tampered"
	if got := us.Todos(); got[0].Content != "read file" {
		t.Errorf("Todos() returned a shared slice: mutating it changed session state to %q", got[0].Content)
	}
}

// TestSetTodosEmptyClears verifies an empty list clears the checklist and still
// emits an event (so the sidebar can drop its nodes).
func TestSetTodosEmptyClears(t *testing.T) {
	us := newTestSession("s1")
	us.SetTodos([]TodoItem{{Content: "one", Status: TodoPending}})
	if len(us.Todos()) != 1 {
		t.Fatal("expected one item after setting")
	}

	var mu sync.Mutex
	var sawEmpty bool
	us.SetObserver(func(ev SessionEvent) {
		mu.Lock()
		defer mu.Unlock()
		if ev.Type == SessionEventTodo && len(ev.Todos) == 0 {
			sawEmpty = true
		}
	})
	us.SetTodos(nil)

	if len(us.Todos()) != 0 {
		t.Errorf("expected checklist cleared, got %+v", us.Todos())
	}
	mu.Lock()
	defer mu.Unlock()
	if !sawEmpty {
		t.Error("expected a SessionEventTodo for the cleared list")
	}
}

// TestNormalizeTodoStatus covers the loose-status coercion the todo tool relies
// on so model input never rejects a checklist.
func TestNormalizeTodoStatus(t *testing.T) {
	cases := map[string]TodoStatus{
		"pending":       TodoPending,
		"completed":     TodoCompleted,
		"in_progress":   TodoInProgress,
		"IN_PROGRESS":   TodoInProgress,
		" in_progress ": TodoInProgress,
		"":              TodoPending,
		"nonsense":      TodoPending,
	}
	for in, want := range cases {
		if got := NormalizeTodoStatus(in); got != want {
			t.Errorf("NormalizeTodoStatus(%q) = %q, want %q", in, got, want)
		}
	}
}
