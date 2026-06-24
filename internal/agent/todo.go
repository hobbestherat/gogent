package agent

import (
	"fmt"
	"strings"
)

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
// it at a glance. Note is an optional free-form annotation (a finding, rationale
// or detail) the model can attach to a task, turning the checklist into a
// working artifact rather than just a label (issue #263).
type TodoItem struct {
	Content string     `json:"content"`
	Status  TodoStatus `json:"status"`
	Note    string     `json:"note,omitempty"`
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

// todoGlyph maps a status to a compact glyph for the system-prompt checklist
// block. It mirrors the sidebar's glyphs (☐ pending, ◐ in-progress, ✔ completed)
// so the model and the human see the same shape (issue #315).
func todoGlyph(status TodoStatus) string {
	switch status {
	case TodoInProgress:
		return "◐"
	case TodoCompleted:
		return "✔"
	default:
		return "☐"
	}
}

// TodoSummary returns a one-line tally of the checklist in done/in-progress/
// pending order, e.g. "2 done, 1 in progress, 0 pending". It is used both in the
// todo tool result and the rendered system-prompt block (issue #263).
func TodoSummary(items []TodoItem) string {
	var done, inProgress, pending int
	for _, it := range items {
		switch it.Status {
		case TodoCompleted:
			done++
		case TodoInProgress:
			inProgress++
		default:
			pending++
		}
	}
	return fmt.Sprintf("%d done, %d in progress, %d pending", done, inProgress, pending)
}

// RenderTodos produces a compact markdown block describing the live checklist,
// suitable for re-injection into the model's context every loop (issue #263).
// Each row is a status glyph + content (+ note in parentheses); a final line
// carries the counts. It returns "" for an empty list so callers can omit the
// section entirely. The caller injects it as the trailing volatile per-request
// message — after the transcript, out of the cacheable prefix (issue #404) — and
// because it is rebuilt every loop and kept out of the compaction-able transcript,
// the checklist stays in front of the model even after a context compaction.
func RenderTodos(items []TodoItem) string {
	if len(items) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("## Task checklist\n")
	b.WriteString("The live checklist for this session (the same list shown in the sidebar). It survives context compaction, so treat it as the source of truth for outstanding work. Call the `todo` tool to flip an item's status as you make progress, and to read the current list back.\n")
	for _, it := range items {
		content := strings.TrimSpace(it.Content)
		if content == "" {
			content = "(empty)"
		}
		fmt.Fprintf(&b, "- %s %s", todoGlyph(it.Status), content)
		if note := strings.TrimSpace(it.Note); note != "" {
			fmt.Fprintf(&b, " (%s)", note)
		}
		b.WriteString("\n")
	}
	fmt.Fprintf(&b, "(%s)", TodoSummary(items))
	return b.String()
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
