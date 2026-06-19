package tool

import (
	"fmt"
	"time"
)

// Task tracks the state of a multi-turn task with tool calling
type Task struct {
	ID          string
	SessionID   string
	AgentID     string
	MessageID   string
	ToolCallID  string
	ToolName    string
	ToolArgs    map[string]interface{}
	ToolResult  interface{}
	ToolError   error
	Status      TaskStatus
	Step        int
	MaxSteps    int
	CreatedAt   int64
	CompletedAt int64
}

type TaskStatus string

const (
	TaskPending    TaskStatus = "pending"
	TaskRunning    TaskStatus = "running"
	TaskToolCalled TaskStatus = "tool_called"
	TaskCompleted  TaskStatus = "completed"
	TaskFailed     TaskStatus = "failed"
	TaskMaxSteps   TaskStatus = "max_steps"
)

func NewTask(sessionID, agentID, messageID string) *Task {
	return &Task{
		ID:        generateTaskID(),
		SessionID: sessionID,
		AgentID:   agentID,
		MessageID: messageID,
		Status:    TaskPending,
		Step:      0,
		MaxSteps:  10,
		CreatedAt: GetTimestamp(),
	}
}

func (t *Task) StartToolCall(toolName string, args map[string]interface{}) {
	t.Status = TaskRunning
	t.Step++
	t.ToolName = toolName
	t.ToolArgs = args
	t.ToolResult = nil
	t.ToolError = nil
}

func (t *Task) CompleteToolResult(result interface{}, err error) {
	t.ToolResult = result
	t.ToolError = err
	if err != nil {
		t.Status = TaskFailed
	} else {
		t.Status = TaskToolCalled
	}
	t.CompletedAt = GetTimestamp()
}

func (t *Task) IsComplete() bool {
	return t.Status == TaskCompleted || t.Status == TaskFailed || t.Status == TaskMaxSteps
}

func (t *Task) CanContinue() bool {
	if t.Status != TaskToolCalled {
		return false
	}
	return t.Step < t.MaxSteps
}

func (t *Task) MarkCompleted() {
	t.Status = TaskCompleted
	t.CompletedAt = GetTimestamp()
}

func (t *Task) MarkMaxSteps() {
	t.Status = TaskMaxSteps
	t.CompletedAt = GetTimestamp()
}

// generateTaskID creates a unique task ID
func generateTaskID() string {
	return "task_" + GetTimestampStr()
}

// GetTimestamp returns the current time as a Unix timestamp in seconds.
func GetTimestamp() int64 {
	return time.Now().Unix()
}

// GetTimestampStr returns the current time as a compact, monotonic string used
// to derive unique task ids.
func GetTimestampStr() string {
	return fmt.Sprintf("%d", time.Now().UnixNano())
}
