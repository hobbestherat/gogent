package agent

import "strings"

// TodoStatus is the lifecycle state of one checklist item (issue #43).
type TodoStatus string

const (
	// TodoPending is an item not yet started.
	TodoPending TodoStatus = "pending"
	// TodoInProgress is the item currently being worked on.
	TodoInProgress TodoStatus = "in_progress"
	// TodoCompleted is a finished item.
	TodoCompleted TodoStatus = "completed"
)

// TodoItem is one entry in a session's task checklist. Content is the
// user-facing description; Status tracks its progress so the sidebar can render
// it at a glance.
type TodoItem struct {
	Content string     `json:"content"`
	Status  TodoStatus `json:"status"`
}

// NormalizeTodoStatus coerces an arbitrary status value into a valid TodoStatus,
// defaulting to TodoPending. It lets the todo tool accept loose model input
// (missing, unknown or differently-cased statuses) without rejecting the call.
func NormalizeTodoStatus(s string) TodoStatus {
	switch TodoStatus(strings.ToLower(strings.TrimSpace(s))) {
	case TodoInProgress:
		return TodoInProgress
	case TodoCompleted:
		return TodoCompleted
	default:
		return TodoPending
	}
}

// SetTodos replaces the session's task checklist with items and emits a
// SessionEventTodo carrying the new list so the sidebar re-renders (issue #43).
// It stores a defensive copy so later caller mutation cannot corrupt the
// session state.
func (s *UserSession) SetTodos(items []TodoItem) {
	cp := make([]TodoItem, len(items))
	copy(cp, items)
	s.mu.Lock()
	s.todos = cp
	s.mu.Unlock()
	s.emit(SessionEvent{Type: SessionEventTodo, Todos: cp})
}

// Todos returns a defensive copy of the session's current task checklist.
func (s *UserSession) Todos() []TodoItem {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]TodoItem, len(s.todos))
	copy(out, s.todos)
	return out
}

// SetPlanMode toggles plan mode for the session's next root-agent turn (issue
// #43). Leaving plan mode clears any plan awaiting approval, since it is no
// longer actionable.
func (s *UserSession) SetPlanMode(on bool) {
	s.mu.Lock()
	s.planMode = on
	if !on {
		s.pendingPlan = ""
	}
	s.mu.Unlock()
}

// PlanMode reports whether the session's next root-agent turn runs in plan mode.
func (s *UserSession) PlanMode() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.planMode
}

// PendingPlan returns the plan awaiting the user's approval (empty when none).
func (s *UserSession) PendingPlan() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.pendingPlan
}

// ClearPendingPlan drops the plan awaiting approval without changing plan mode.
func (s *UserSession) ClearPendingPlan() {
	s.mu.Lock()
	s.pendingPlan = ""
	s.mu.Unlock()
}

// setPendingPlan records a plan produced in plan mode and returns whether one
// was actually set (a non-empty plan). The caller gates the SessionEventPlan
// emission on a true result so an empty plan turn is not surfaced as a gate.
func (s *UserSession) setPendingPlan(plan string) bool {
	plan = strings.TrimSpace(plan)
	if plan == "" {
		return false
	}
	s.mu.Lock()
	s.pendingPlan = plan
	s.mu.Unlock()
	return true
}
