package agent

import (
	"sync"

	"gogent/internal/model"
	"gogent/internal/tool"
)

// AgentState represents the current state of an agent
type AgentState string

const (
	StateThinking           AgentState = "thinking"
	StateWaitingForSubAgent AgentState = "waiting_for_subagent"
	StateWaitingForShell    AgentState = "waiting_for_shell"
	StateWaitingForTool     AgentState = "waiting_for_tool"
	StateIdle               AgentState = "idle"
)

// AgentStatus is the high-level lifecycle status of a (sub-)agent, as surfaced
// in the session/sub-agent sidebar tree. Unlike AgentState (which tracks what an
// active agent is momentarily blocked on), AgentStatus persists a terminal
// outcome (completed/failed) so the UI can show how a sub-agent finished.
type AgentStatus string

const (
	StatusIdle      AgentStatus = "idle"
	StatusRunning   AgentStatus = "running"
	StatusWaiting   AgentStatus = "waiting"
	StatusCompleted AgentStatus = "completed"
	StatusFailed    AgentStatus = "failed"
)

// SubAgentKind records how a sub-agent was spawned, which drives the extra
// system instructions it receives (tool-style one-shot vs interactive worker).
type SubAgentKind string

const (
	// KindRoot is the top-level user-facing agent (not a sub-agent).
	KindRoot SubAgentKind = "root"
	// KindTool is a one-shot sub-agent invoked like a tool call.
	KindTool SubAgentKind = "tool"
	// KindInteractive is an asynchronous, conversational sub-agent.
	KindInteractive SubAgentKind = "interactive"
)

// Agent represents an agent in the system
type Agent struct {
	ID           string
	Name         string
	ThoughtTrain *model.ModelSession
	SubAgents    []*Agent
	Parent       *Agent
	State        AgentState
	Status       AgentStatus
	Kind         SubAgentKind
	Task         string
	Result       string
	TimeoutMs    int64
	ToolRegistry *tool.ToolRegistry
	mu           sync.Mutex
}

// NewAgent creates a new agent
func NewAgent(id string, modelSession *model.ModelSession) *Agent {
	return &Agent{
		ID:           id,
		Name:         id,
		ThoughtTrain: modelSession,
		SubAgents:    []*Agent{},
		State:        StateIdle,
		Status:       StatusIdle,
		Kind:         KindRoot,
		TimeoutMs:    30000, // Default 30 second timeout
		ToolRegistry: tool.NewToolRegistry(),
	}
}

// AddSubAgent adds a sub-agent to this agent
func (a *Agent) AddSubAgent(subAgent *Agent) {
	a.mu.Lock()
	defer a.mu.Unlock()
	subAgent.setParent(a)
	a.SubAgents = append(a.SubAgents, subAgent)
}

// setParent links this agent to its parent. It is called only from AddSubAgent
// (which holds the parent's mutex) and takes the child's own mutex so the write
// is published under the same lock that GetParent reads through.
func (a *Agent) setParent(parent *Agent) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.Parent = parent
}

// GetParent returns the parent agent, or nil for the root. The read is
// mutex-guarded so it is safe to call while another goroutine is linking a
// freshly-spawned child via AddSubAgent.
func (a *Agent) GetParent() *Agent {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.Parent
}

// RemoveSubAgent removes a sub-agent
func (a *Agent) RemoveSubAgent(id string) {
	a.mu.Lock()
	defer a.mu.Unlock()

	newSubAgents := []*Agent{}
	for _, sub := range a.SubAgents {
		if sub.ID != id {
			newSubAgents = append(newSubAgents, sub)
		}
	}
	a.SubAgents = newSubAgents
}

// GetSubAgent finds a sub-agent by ID
func (a *Agent) GetSubAgent(id string) *Agent {
	a.mu.Lock()
	defer a.mu.Unlock()

	for _, sub := range a.SubAgents {
		if sub.ID == id {
			return sub
		}
	}
	return nil
}

// GetRootAgent returns the root agent of the tree
func (a *Agent) GetRootAgent() *Agent {
	current := a
	for {
		parent := current.GetParent()
		if parent == nil {
			return current
		}
		current = parent
	}
}

// GetSubAgents returns a copy of sub-agents
func (a *Agent) GetSubAgents() []*Agent {
	a.mu.Lock()
	defer a.mu.Unlock()

	subAgents := make([]*Agent, len(a.SubAgents))
	copy(subAgents, a.SubAgents)
	return subAgents
}

// ListAllAgents returns all agents in the tree recursively. Each level reads its
// children through the lock-guarded GetSubAgents, so it is safe to call while
// other goroutines add sub-agents to the tree.
func (a *Agent) ListAllAgents() []*Agent {
	result := []*Agent{a}
	for _, sub := range a.GetSubAgents() {
		result = append(result, sub.ListAllAgents()...)
	}
	return result
}

// GetAgentByID finds an agent by ID in the tree. The traversal reads children
// through GetSubAgents, so it is safe to call while other goroutines add
// sub-agents to the tree.
func (a *Agent) GetAgentByID(id string) *Agent {
	if a.ID == id {
		return a
	}
	for _, sub := range a.GetSubAgents() {
		if found := sub.GetAgentByID(id); found != nil {
			return found
		}
	}
	return nil
}

// SetState sets the agent state
func (a *Agent) SetState(state AgentState) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.State = state
}

// GetState returns the agent state
func (a *Agent) GetState() AgentState {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.State
}

// UpdateState updates state and returns old state
func (a *Agent) UpdateState(newState AgentState) AgentState {
	a.mu.Lock()
	defer a.mu.Unlock()
	oldState := a.State
	a.State = newState
	return oldState
}

// SetToolRegistry sets the tool registry for this agent
func (a *Agent) SetToolRegistry(registry *tool.ToolRegistry) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.ToolRegistry = registry
}

// SetStatus sets the high-level lifecycle status of the agent.
func (a *Agent) SetStatus(status AgentStatus) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.Status = status
}

// GetStatus returns the high-level lifecycle status of the agent.
func (a *Agent) GetStatus() AgentStatus {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.Status
}

// SetResult records the sub-agent's final result text (success or failure).
func (a *Agent) SetResult(result string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.Result = result
}

// GetResult returns the sub-agent's recorded result text.
func (a *Agent) GetResult() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.Result
}

// DisplayName returns the friendly name for the agent, falling back to its ID.
func (a *Agent) DisplayName() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.Name != "" {
		return a.Name
	}
	return a.ID
}
