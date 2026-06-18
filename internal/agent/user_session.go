package agent

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"gogent/internal/compression"
	"gogent/internal/config"
	"gogent/internal/model"
	"gogent/internal/tool"
)

// SessionEventType identifies the kind of live update emitted during a task loop.
type SessionEventType string

const (
	// SessionEventThinking signals the agent has started a model turn.
	SessionEventThinking SessionEventType = "thinking"
	// SessionEventAssistantStep carries intermediate reasoning text the model
	// produced alongside (or instead of) a tool call.
	SessionEventAssistantStep SessionEventType = "assistant_step"
	// SessionEventToolCall is emitted just before a tool is executed.
	SessionEventToolCall SessionEventType = "tool_call"
	// SessionEventToolResult carries the textual result of a tool execution.
	SessionEventToolResult SessionEventType = "tool_result"
	// SessionEventFinal carries the assistant's final answer.
	SessionEventFinal SessionEventType = "final"
	// SessionEventError reports a failure during the loop.
	SessionEventError SessionEventType = "error"
	// SessionEventSubAgent reports a sub-agent lifecycle change (spawned/finished).
	SessionEventSubAgent SessionEventType = "subagent"
	// SessionEventCompaction reports that the context was compressed; Text holds
	// the structured summary and Step carries the post-compaction token estimate.
	SessionEventCompaction SessionEventType = "compaction"
)

// SessionEvent is a single observable update from a running task loop. UIs use
// these to render live, foldable detail (thoughts, tool calls, results).
type SessionEvent struct {
	Type   SessionEventType
	Step   int
	Text   string
	Tool   string
	Args   map[string]interface{}
	Result string
	Err    error

	// Sub-agent identity/status (populated on SessionEventSubAgent) so a UI can
	// maintain a live session → sub-agent tree with per-agent status.
	AgentID string
	Name    string
	Status  AgentStatus
	Kind    SubAgentKind
}

// SessionObserver receives SessionEvents as a task loop progresses. It is always
// invoked from the goroutine running the loop, so observers that touch a UI must
// marshal back onto their own event thread.
type SessionObserver func(SessionEvent)

// UserSession represents a user-facing session
type UserSession struct {
	ID           string
	RootAgent    *Agent
	CreatedAt    int64
	ToolCallback func(toolName string, args map[string]interface{}) error
	observer     SessionObserver
	mu           sync.RWMutex

	// Sub-agent execution-model settings (one-shot vs interactive, recursion).
	subAgentCfg config.SubAgentConfig
	// subAgentTimeoutMs bounds how long a spawned sub-agent may run. Zero leaves
	// the agent's built-in default in place.
	subAgentTimeoutMs int64

	// systemContextFn, when set, returns extra system-prompt context (project
	// AGENTS.md instructions and the available-skills index) appended to every
	// agent loop's system prompt. Evaluated per loop so runtime changes (skill
	// activation) are reflected.
	systemContextFn func() string

	// Interactive (experimental) sub-agent bookkeeping.
	interactive map[string]*InteractiveAgent
	agentEvents chan AgentEvent

	// Task tracking for multi-turn tool calling
	currentTask *tool.Task

	// compressionCompleter, when set, runs context compression on a separate
	// (typically smaller/faster) model backend instead of the session's primary
	// model. When it also reports connector stats, its usage is tracked apart
	// from the primary model (see FastConnectorStats).
	compressionCompleter model.Completer

	// Stats
	tokenCountIn  int
	tokenCountOut int
	toolCallCount int
}

// NewUserSession creates a new user session
func NewUserSession(id string, agent *Agent) *UserSession {
	return &UserSession{
		ID:            id,
		RootAgent:     agent,
		CreatedAt:     time.Now().Unix(),
		ToolCallback:  nil,
		subAgentCfg:   config.DefaultSubAgentConfig(),
		interactive:   make(map[string]*InteractiveAgent),
		agentEvents:   make(chan AgentEvent, 64),
		currentTask:   nil,
		tokenCountIn:  0,
		tokenCountOut: 0,
		toolCallCount: 0,
	}
}

// SetSubAgentConfig updates the sub-agent execution-model settings used when
// spawning sub-agents from this session.
func (s *UserSession) SetSubAgentConfig(cfg config.SubAgentConfig) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.subAgentCfg = cfg
}

// SubAgentConfig returns the current sub-agent execution-model settings.
func (s *UserSession) SubAgentConfig() config.SubAgentConfig {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.subAgentCfg
}

// SetCompressionCompleter routes context-compression summaries to a dedicated
// completer (typically a small/fast model). When unset, compaction uses the
// session's own primary model, preserving prior behavior.
func (s *UserSession) SetCompressionCompleter(c model.Completer) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.compressionCompleter = c
}

// UsesFastCompression reports whether context compression runs on a dedicated
// (typically smaller/faster) model backend rather than the session's primary
// model. UIs can use this to indicate that a fast model is active.
func (s *UserSession) UsesFastCompression() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.compressionCompleter != nil
}

// SetSystemContextProvider registers a function returning extra system-prompt
// context (project AGENTS.md instructions and the available-skills index). It is
// evaluated at the start of each agent loop.
func (s *UserSession) SetSystemContextProvider(fn func() string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.systemContextFn = fn
}

// systemContext returns the current extra system context, or "" if none.
func (s *UserSession) systemContext() string {
	s.mu.RLock()
	fn := s.systemContextFn
	s.mu.RUnlock()
	if fn == nil {
		return ""
	}
	return fn()
}

// SetSubAgentTimeout sets the timeout applied to newly spawned sub-agents. A
// non-positive duration leaves each agent's built-in default in place.
func (s *UserSession) SetSubAgentTimeout(d time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if d <= 0 {
		s.subAgentTimeoutMs = 0
		return
	}
	s.subAgentTimeoutMs = d.Milliseconds()
}

// SetToolCallback sets the callback for tool calls
func (s *UserSession) SetToolCallback(cb func(toolName string, args map[string]interface{}) error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ToolCallback = cb
}

// SetObserver registers a live event observer for this session's task loops.
func (s *UserSession) SetObserver(observer SessionObserver) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.observer = observer
}

// emit dispatches a session event to the registered observer (if any).
func (s *UserSession) emit(event SessionEvent) {
	s.mu.RLock()
	observer := s.observer
	s.mu.RUnlock()
	if observer != nil {
		observer(event)
	}
}

// Init initializes the session
func (s *UserSession) Init() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.CreatedAt == 0 {
		s.CreatedAt = 0 // Set to current time
	}
}

// ListAgents returns all agents in the session
func (s *UserSession) ListAgents() []*Agent {
	if s.RootAgent == nil {
		return []*Agent{}
	}
	return s.RootAgent.ListAllAgents()
}

// GetAgent finds an agent by ID
func (s *UserSession) GetAgent(id string) *Agent {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.RootAgent == nil {
		return nil
	}
	return s.RootAgent.GetAgentByID(id)
}

// AddAgent adds an agent to the session under a parent
func (s *UserSession) AddAgent(parentID string, agent *Agent) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.RootAgent == nil {
		s.RootAgent = agent
		return nil
	}

	parent := s.RootAgent.GetAgentByID(parentID)
	if parent == nil {
		return &NotFoundError{ID: parentID}
	}

	parent.AddSubAgent(agent)
	return nil
}

// SendMessage sends a message to an agent and returns the model response
func (s *UserSession) SendMessage(agentID, message string) (*model.CompletionResponse, error) {
	s.mu.Lock()
	agent := s.RootAgent.GetAgentByID(agentID)
	s.mu.Unlock()

	if agent == nil {
		return nil, &NotFoundError{ID: agentID}
	}

	// Create message with tool definitions in system prompt
	msg := model.Message{
		Role:    model.RoleUser,
		Content: s.buildMessageWithTools(message),
	}

	// Send message to the agent's model session
	if agent.ThoughtTrain != nil {
		resp, err := agent.ThoughtTrain.Send([]model.Message{msg})
		if err != nil {
			return nil, err
		}
		return resp, nil
	}

	return nil, nil
}

// SendMessageNoTools sends a message without prepending tool definitions (for tool results)
func (s *UserSession) SendMessageNoTools(agentID, message string) (*model.CompletionResponse, error) {
	s.mu.Lock()
	agent := s.RootAgent.GetAgentByID(agentID)
	s.mu.Unlock()

	if agent == nil {
		return nil, &NotFoundError{ID: agentID}
	}

	// Send message directly without tool definitions
	msg := model.Message{
		Role:    model.RoleUser,
		Content: message,
	}

	// Send message to the agent's model session
	if agent.ThoughtTrain != nil {
		resp, err := agent.ThoughtTrain.Send([]model.Message{msg})
		if err != nil {
			return nil, err
		}
		return resp, nil
	}

	return nil, nil
}

// buildMessageWithTools adds tool definitions to the message
func (s *UserSession) buildMessageWithTools(message string) string {
	toolDefs := `You have access to the following tools:

read:
  description: Read a file from the workspace. ALWAYS use this tool to verify file existence and content before making assumptions about files. This tool returns the actual file content.

write:
  description: Write content to a file. Use this to create or overwrite files.
  input: {"type": "object", "properties": {"path": {"type": "string"}, "content": {"type": "string"}}}

edit:
  description: Edit a file by replacing exact text. Use this for precise edits.
  input: {"type": "object", "properties": {"path": {"type": "string"}, "find": {"type": "string"}, "replace": {"type": "string"}}}

calc:
  	description: Calculate mathematical expressions like 5+5 or 10*20/5
  	input: {"type": "object", "properties": {"expression": {"type": "string"}}}

  	shell:
  	description: Execute shell commands like curl, wget, ls, grep, etc.
  	input: {"type": "object", "properties": {"command": {"type": "string"}}}
  	example: {"tool": "shell", "args": {"command": "curl -s https://unsorted.ch/account/api/info"}}
  	returns: {"command": "...", "stdout": "...", "stderr": "...", "exit_code": 0, "timeout": false, "error": null}

  	structured_output:
  	description: Use this tool to return your final response
  	input: {"type": "object", "properties": {"response": {"type": "string"}, "final": {"type": "boolean"}}}

  	IMPORTANT INSTRUCTIONS:
1. ALWAYS use the read tool to verify file existence and get actual content before making assumptions
2. Do not state that a file doesn't exist without first using the read tool
3. Tool execution happens automatically - you just need to request it
4. Tool results will be sent back to you after execution

To use a tool, output a JSON object with:
{"tool": "tool_name", "args": {"key": "value"}}

For final responses, use:
{"response": "...", "final": true}
`

	return toolDefs + "\n\n" + message
}

// ExecuteTaskLoop runs the multi-turn task loop with tool calling.
//
// It uses native OpenAI tool-calling when the model/server supports it (the
// reliable path for small models) and transparently falls back to parsing a
// JSON tool call out of the assistant's text when no native tool_calls are
// returned. Tool results are fed back as proper role:"tool" messages so the
// model keeps full context across turns.
func (s *UserSession) ExecuteTaskLoop(agentID string, initialMessage string) ([]*model.CompletionResponse, error) {
	s.mu.Lock()
	agent := s.RootAgent.GetAgentByID(agentID)
	s.mu.Unlock()

	if agent == nil {
		return nil, &NotFoundError{ID: agentID}
	}
	if agent.ThoughtTrain == nil {
		return nil, fmt.Errorf("agent %s has no model session", agentID)
	}

	tools := toolDefsFromRegistry(agent.ToolRegistry)
	return s.runLoop(agent, agentID, initialMessage, buildAgentSystemPrompt(len(tools) > 0, s.SubAgentConfig()))
}

// runLoop is the shared multi-turn tool-calling loop used by both the top-level
// task loop and sub-agents (sub-agents pass a different system prompt).
func (s *UserSession) runLoop(agent *Agent, agentID, initialMessage, systemPrompt string) ([]*model.CompletionResponse, error) {
	sess := agent.ThoughtTrain
	tools := toolDefsFromRegistry(agent.ToolRegistry)
	if ctx := s.systemContext(); ctx != "" {
		systemPrompt += "\n\n" + ctx
	}
	sess.SetSystemPrompt(systemPrompt)

	// Only the top-level (root) agent streams its thinking/tool events into the
	// session window. Sub-agent loops stay silent here so their internal tool
	// calls don't clutter the main chat — their progress is surfaced via the
	// sidebar (SessionEventSubAgent lifecycle events) and their full detail is
	// available on demand in the sub-agent monologue popup (which reads the
	// agent's transcript directly).
	emit := s.emit
	if agent.Kind != KindRoot {
		emit = func(SessionEvent) {}
	}

	responses := make([]*model.CompletionResponse, 0)

	emit(SessionEvent{Type: SessionEventThinking, Step: 0})

	// First request carries the user message.
	s.compactIfNeeded(sess, emit)
	resp, err := sess.SendWithTools(
		[]model.Message{{Role: model.RoleUser, Content: initialMessage}},
		tools,
	)
	if err != nil {
		emit(SessionEvent{Type: SessionEventError, Err: err})
		return responses, err
	}
	responses = append(responses, resp)

	const maxSteps = 25
	for step := 0; step < maxSteps; step++ {
		calls := s.collectToolCalls(resp)
		if len(calls) == 0 {
			// No tool calls -> the assistant produced its final answer.
			break
		}

		// Surface any intermediate reasoning the model emitted alongside its
		// tool calls so the UI can show (foldable) thoughts.
		if thought := strings.TrimSpace(resp.Content); thought != "" {
			emit(SessionEvent{Type: SessionEventAssistantStep, Step: step, Text: thought})
		}

		// Execute each requested tool and gather result messages.
		toolMsgs := make([]model.Message, len(calls))

		// Parallel fast-path: when a single turn asks for several sub-agent
		// spawns, run them concurrently. Sub-agents are independent and every
		// agent-tree read and write is mutex-guarded (children are copied under
		// lock via GetSubAgents, the parent link via GetParent), so this is
		// safe and is what lets "delegate A, B and C at once" run in parallel.
		if allSpawnSubAgent(calls) {
			var wg sync.WaitGroup
			for i, call := range calls {
				i, call := i, call
				emit(SessionEvent{Type: SessionEventToolCall, Step: step, Tool: call.Tool, Args: call.Args})
				wg.Add(1)
				go func() {
					defer wg.Done()
					resultStr := s.runToolCall(agent, agentID, call)
					emit(SessionEvent{Type: SessionEventToolResult, Step: step, Tool: call.Tool, Args: call.Args, Result: resultStr})
					toolMsgs[i] = makeToolResultMessage(call, resultStr)
				}()
			}
			wg.Wait()
			sess.AppendToolResults(toolMsgs)
			emit(SessionEvent{Type: SessionEventThinking, Step: step + 1})
			s.compactIfNeeded(sess, emit)
			resp, err = sess.SendWithTools(nil, tools)
			if err != nil {
				emit(SessionEvent{Type: SessionEventError, Err: err})
				return responses, err
			}
			responses = append(responses, resp)
			continue
		}

		toolMsgs = toolMsgs[:0]
		finished := false
		for _, call := range calls {
			if call.Tool == "structured_output" {
				// Terminal tool: fold its response into the assistant content.
				if final, _ := call.Args["final"].(bool); final {
					if text, ok := call.Args["response"].(string); ok && text != "" {
						resp.Content = text
					}
					finished = true
					break
				}
			}

			emit(SessionEvent{Type: SessionEventToolCall, Step: step, Tool: call.Tool, Args: call.Args})
			resultStr := s.runToolCall(agent, agentID, call)
			emit(SessionEvent{Type: SessionEventToolResult, Step: step, Tool: call.Tool, Args: call.Args, Result: resultStr})
			toolMsgs = append(toolMsgs, makeToolResultMessage(call, resultStr))
		}
		if finished {
			break
		}

		// Feed results back and ask the model to continue.
		sess.AppendToolResults(toolMsgs)
		emit(SessionEvent{Type: SessionEventThinking, Step: step + 1})
		s.compactIfNeeded(sess, emit)
		resp, err = sess.SendWithTools(nil, tools)
		if err != nil {
			emit(SessionEvent{Type: SessionEventError, Err: err})
			return responses, err
		}
		responses = append(responses, resp)
	}

	if resp != nil {
		emit(SessionEvent{Type: SessionEventFinal, Text: strings.TrimSpace(resp.Content)})
	}

	return responses, nil
}

// compactIfNeeded compresses the session transcript in place when it has grown
// past the model's compression threshold. It summarizes the older part of the
// conversation (preserving the most recent turns verbatim and never splitting a
// tool-call from its results) and splices the digest back into the transcript.
// Summarization uses a stateless completion on the configured compression
// backend (the fast model when set, else the session's own backend), so it never
// pollutes the live transcript. On any failure it leaves the transcript
// untouched rather than risk losing context.
func (s *UserSession) compactIfNeeded(sess *model.ModelSession, emit func(SessionEvent)) {
	if sess == nil || sess.Model == nil || !sess.NeedsCompression() {
		return
	}

	transcript := sess.GetTranscript()
	older, recent := compression.SafeSplit(transcript, compression.DefaultKeepRecentTurns)
	if len(older) == 0 {
		return // boundary keeps everything recent; nothing to compress yet
	}

	// Summarize on the configured fast model when one was wired in for the
	// compression role, otherwise fall back to the session's own backend.
	completer := model.Completer(sess.Model)
	s.mu.RLock()
	if s.compressionCompleter != nil {
		completer = s.compressionCompleter
	}
	s.mu.RUnlock()

	agent := compression.NewCompressionAgent(nil, completer)
	digest, err := agent.Summarize(older)
	if err != nil || strings.TrimSpace(digest) == "" {
		return
	}

	digestMsg := model.Message{
		Role:    model.RoleUser,
		Content: "[Earlier conversation summarized to save context]\n\n" + digest,
	}
	newTranscript := append([]model.Message{digestMsg}, recent...)
	sess.ApplyCompressedTranscript(newTranscript)
	emit(SessionEvent{Type: SessionEventCompaction, Step: sess.GetTokenCount(), Text: digest})
}

// allSpawnSubAgent reports whether a turn's tool calls are all one-shot
// sub-agent spawns, in which case they can be executed concurrently.
func allSpawnSubAgent(calls []tool.ToolCall) bool {
	if len(calls) < 2 {
		return false
	}
	for _, c := range calls {
		if c.Tool != "spawn_subagent" {
			return false
		}
	}
	return true
}

// collectToolCalls returns the tool calls for a response, preferring native
// tool_calls and falling back to a JSON object embedded in the text.
func (s *UserSession) collectToolCalls(resp *model.CompletionResponse) []tool.ToolCall {
	if resp == nil {
		return nil
	}

	// Native tool calls.
	if len(resp.ToolCalls) > 0 {
		calls := make([]tool.ToolCall, 0, len(resp.ToolCalls))
		for _, tc := range resp.ToolCalls {
			args := map[string]interface{}{}
			if tc.Function.Arguments != "" {
				_ = json.Unmarshal([]byte(tc.Function.Arguments), &args)
			}
			calls = append(calls, tool.ToolCall{
				Tool:   tc.Function.Name,
				Args:   args,
				CallID: tc.ID,
			})
		}
		return calls
	}

	// Fallback: JSON object in the text.
	responseText := strings.TrimSpace(resp.Content)

	// A {"response": ..., "final": true} object means we're done.
	var structuredOutput struct {
		Response string `json:"response"`
		Final    bool   `json:"final"`
	}
	if jsonStr := extractToolCallJSON(responseText); jsonStr != "" {
		if err := json.Unmarshal([]byte(jsonStr), &structuredOutput); err == nil && structuredOutput.Final {
			resp.Content = structuredOutput.Response
			return nil
		}
		var parsed struct {
			Tool string                 `json:"tool"`
			Args map[string]interface{} `json:"args"`
		}
		if err := json.Unmarshal([]byte(jsonStr), &parsed); err == nil && parsed.Tool != "" {
			return []tool.ToolCall{{Tool: parsed.Tool, Args: parsed.Args}}
		}
	}
	return nil
}

// runToolCall executes a single tool call and returns a textual result.
func (s *UserSession) runToolCall(agent *Agent, agentID string, call tool.ToolCall) string {
	ctx := tool.ToolContext{
		SessionID:    s.ID,
		AgentID:      agentID,
		ToolCallID:   call.CallID,
		ToolCallback: s.ToolCallback,
	}
	toolResp, err := agent.ToolRegistry.ExecuteToolCall(&call, ctx)
	switch {
	case err != nil:
		return fmt.Sprintf("error: %v", err)
	case toolResp == nil:
		return "error: tool returned no response"
	case !toolResp.Success:
		return fmt.Sprintf("error: %s", toolResp.Error)
	default:
		return fmt.Sprintf("%v", toolResp.Result)
	}
}

// makeToolResultMessage builds the message carrying a tool result back to the
// model. Native calls use role:"tool" with the originating call id; fallback
// calls (no id) use a plain user message the dumb model can still read.
func makeToolResultMessage(call tool.ToolCall, result string) model.Message {
	if call.CallID != "" {
		return model.Message{
			Role:       model.RoleTool,
			ToolCallID: call.CallID,
			Name:       call.Tool,
			Content:    result,
		}
	}
	return model.Message{
		Role:    model.RoleUser,
		Content: fmt.Sprintf("TOOL_RESULT[%s]: %s", call.Tool, result),
	}
}

// toolDefsFromRegistry converts the agent's tool registry into native tool defs.
func toolDefsFromRegistry(reg *tool.ToolRegistry) []model.ToolDef {
	if reg == nil {
		return nil
	}
	tools := reg.List()
	defs := make([]model.ToolDef, 0, len(tools))
	for _, t := range tools {
		defs = append(defs, model.ToolDef{
			Type: "function",
			Function: model.FunctionDef{
				Name:        t.Name,
				Description: t.Description,
				Parameters:  t.InputSchema,
			},
		})
	}
	return defs
}

// buildAgentSystemPrompt returns the system prompt for the top-level agent loop.
// It appends delegation guidance describing which sub-agent execution model(s)
// the user has enabled (see cfg), so the model knows how it may spawn helpers.
func buildAgentSystemPrompt(hasTools bool, cfg config.SubAgentConfig) string {
	var base string
	if hasTools {
		base = `You are Gogent, a focused coding assistant operating in a workspace.

Use the provided tools to inspect and modify files and run commands. Guidelines:
- Prefer calling a tool over guessing. Read files before editing them.
- Call one or more tools, observe the results, then continue until the task is done.
- When the task is complete, reply with a short plain-text answer (do NOT call a tool).
- Keep answers concise.`
	} else {
		base = `You are Gogent, a focused coding assistant. Answer concisely.`
	}
	return base + coordinatorInstructions(cfg)
}

// coordinatorInstructions describes how the agent may delegate work to
// sub-agents, tailored to the enabled execution model. It is shared by the
// top-level agent and (when recursion is enabled) by sub-agents.
func coordinatorInstructions(cfg config.SubAgentConfig) string {
	if cfg.IsOneShot() {
		return `

## Delegating work (one-shot sub-agents)
When a task has several INDEPENDENT parts, delegate them so they run in parallel.
Prefer ONE spawn_subagent call carrying a "subtasks" array — every entry runs
concurrently and the call blocks until all of them finish. For example:
  {"tool":"spawn_subagent","args":{"subtasks":[
    {"name":"docs","task":"Summarise README.md"},
    {"name":"tests","task":"List the failing tests"},
    {"name":"deps","task":"Audit go.mod for outdated modules"}
  ]}}
Each sub-agent runs to completion and returns a result starting with "SUCCESS: "
or "FAILURE: ". Use a single "name"/"task" pair only for a lone subtask. Do not
issue the spawns one at a time across turns — batch independent work together.
Delegate only when it is clearly worthwhile; otherwise do the work yourself.`
	}
	return `

## Delegating work (interactive sub-agents, experimental)
You may launch asynchronous sub-agents that run concurrently:
- launch_agent {name, task} starts a worker and returns its agent_id immediately.
- agent_status {agent_id} reports running/waiting/completed/failed and any result.
- wait_agent_event blocks until a sub-agent finishes or asks a question.
- agent_send {agent_id, message} answers a sub-agent's CLARIFY question or gives
  it more direction.
- agent_terminate {agent_id} stops a sub-agent you no longer need.
After launching workers, call wait_agent_event and react to each event until all
sub-agents have completed.`
}

// recursionInstructions is appended to a sub-agent's prompt when it is itself
// permitted to spawn sub-agents.
func recursionInstructions(cfg config.SubAgentConfig) string {
	return "\n\nYou are permitted to spawn your own sub-agents to break this task" +
		" down further. Do so sparingly, only for genuinely independent subtasks." +
		coordinatorInstructions(cfg)
}

// ExecuteTaskLoopWithModel runs the multi-turn task loop with a specific model config
func (s *UserSession) ExecuteTaskLoopWithModel(agentID, message string, modelConfig *config.ModelConfig) ([]*model.CompletionResponse, error) {
	// Call the regular ExecuteTaskLoop
	return s.ExecuteTaskLoop(agentID, message)
}

// subAgentToolNames lists the tools that let an agent spawn or coordinate
// sub-agents. They are stripped from a child's registry when recursive
// sub-agents are not allowed, so a sub-agent cannot delegate further.
var subAgentToolNames = []string{
	"spawn_subagent",
	"launch_agent",
	"agent_status",
	"agent_send",
	"agent_terminate",
	"wait_agent_event",
}

const subAgentOneShotPrompt = `You are a focused sub-agent working on a single delegated subtask. Use the available tools to complete it.
When finished, reply with a final plain-text answer that STARTS with either:
  "SUCCESS: " followed by the result, or
  "FAILURE: " followed by the reason you could not complete it.
Do not ask questions; make reasonable assumptions and proceed. Keep it concise.`

const subAgentInteractivePrompt = `You are a sub-agent working on a delegated subtask. Use the available tools to complete it.
When finished, reply with a final plain-text answer that STARTS with either:
  "SUCCESS: " followed by the result, or
  "FAILURE: " followed by the reason you could not complete it.
If — and only if — you are genuinely blocked and need the coordinator to decide something, instead reply STARTING with
  "CLARIFY: " followed by your specific question.
Keep it concise.`

// subAgentPrompt builds the system prompt for a sub-agent, layering recursion
// guidance on top of the base one-shot/interactive prompt when the sub-agent is
// itself allowed to delegate.
func (s *UserSession) subAgentPrompt(oneShot bool) string {
	cfg := s.SubAgentConfig()
	prompt := subAgentOneShotPrompt
	if !oneShot {
		prompt = subAgentInteractivePrompt
	}
	if cfg.AllowRecursive {
		prompt += recursionInstructions(cfg)
	}
	return prompt
}

// newSubAgent validates depth, builds a child agent under parentAgentID, wires
// its model session / tool registry (recursion-restricted when needed), attaches
// it to the tree, and returns it ready to run.
func (s *UserSession) newSubAgent(parentAgentID, name, task string, kind SubAgentKind) (*Agent, error) {
	s.mu.RLock()
	parent := s.RootAgent.GetAgentByID(parentAgentID)
	cfg := s.subAgentCfg
	timeoutMs := s.subAgentTimeoutMs
	s.mu.RUnlock()
	if parent == nil {
		return nil, &NotFoundError{ID: parentAgentID}
	}
	if parent.ThoughtTrain == nil {
		return nil, fmt.Errorf("parent agent %s has no model session", parentAgentID)
	}

	depth := 0
	for p := parent; p != nil; p = p.GetParent() {
		depth++
	}
	if depth > cfg.MaxDepthOrDefault() {
		return nil, fmt.Errorf("max sub-agent depth (%d) reached", cfg.MaxDepthOrDefault())
	}

	// Cap how many sub-agents a single parent may spawn.
	if len(parent.GetSubAgents()) >= cfg.MaxSubAgentsOrDefault() {
		return nil, fmt.Errorf("max sub-agents (%d) reached for %s", cfg.MaxSubAgentsOrDefault(), parentAgentID)
	}

	if strings.TrimSpace(name) == "" {
		name = fmt.Sprintf("sub-%d", len(parent.GetSubAgents())+1)
	}
	childID := parentAgentID + "/" + name

	// Share the parent's model backend (stateless HTTP connection) but give the
	// child its own transcript, and route its token usage into session stats.
	childSess := model.NewModelSession(childID, parent.ThoughtTrain.Model)
	childSess.AddTokenCallback(func(promptTokens, completionTokens int) {
		s.AddTokenUsage(promptTokens, completionTokens)
	})
	child := NewAgent(childID, childSess)
	child.Name = name
	child.Kind = kind
	child.Task = task
	child.Status = StatusRunning
	if timeoutMs > 0 {
		child.TimeoutMs = timeoutMs
	}

	// Recursion control: when recursive sub-agents are disabled, hand the child a
	// registry that omits the spawn/coordinate tools so it cannot delegate.
	if cfg.AllowRecursive {
		child.SetToolRegistry(parent.ToolRegistry)
	} else {
		child.SetToolRegistry(parent.ToolRegistry.CloneWithout(subAgentToolNames...))
	}

	parent.AddSubAgent(child)
	return child, nil
}

// emitSubAgent emits a sub-agent lifecycle event carrying the child's identity
// and current status, so a UI can keep a live session → sub-agent tree.
func (s *UserSession) emitSubAgent(child *Agent, text string, err error) {
	s.emit(SessionEvent{
		Type:    SessionEventSubAgent,
		AgentID: child.ID,
		Name:    child.DisplayName(),
		Tool:    child.DisplayName(),
		Kind:    child.Kind,
		Status:  child.GetStatus(),
		Text:    text,
		Result:  child.GetResult(),
		Err:     err,
	})
}

// subAgentOutcome maps a sub-agent's final text to a terminal status. A reply
// starting with FAILURE: is a failure; anything else is treated as completed.
func subAgentOutcome(final string) AgentStatus {
	if strings.HasPrefix(strings.TrimSpace(strings.ToUpper(final)), "FAILURE") {
		return StatusFailed
	}
	return StatusCompleted
}

// SpawnSubAgent creates a one-shot child agent under parentAgentID, runs it to
// completion on the given task, and returns its final answer. The sub-agent is
// instructed to finish with SUCCESS:/FAILURE:. The child agent is kept in the
// agent tree (with a terminal status) so UIs can display it.
//
// The oneShot parameter selects the sub-agent's base prompt; the surrounding
// execution is always blocking here. Asynchronous, conversational workers use
// LaunchInteractiveAgent instead.
func (s *UserSession) SpawnSubAgent(parentAgentID, name, task string, oneShot bool) (string, error) {
	kind := KindTool
	if !oneShot {
		kind = KindInteractive
	}
	child, err := s.newSubAgent(parentAgentID, name, task, kind)
	if err != nil {
		return "", err
	}
	parent := child.GetParent()

	parent.SetState(StateWaitingForSubAgent)
	child.SetState(StateThinking)
	child.SetStatus(StatusRunning)
	s.emitSubAgent(child, "spawned: "+task, nil)

	responses, runErr := s.runLoop(child, child.ID, task, s.subAgentPrompt(oneShot))

	child.SetState(StateIdle)
	parent.SetState(StateThinking)
	if runErr != nil {
		child.SetStatus(StatusFailed)
		child.SetResult(runErr.Error())
		s.emitSubAgent(child, "", runErr)
		return "", runErr
	}
	final := ""
	if len(responses) > 0 {
		final = strings.TrimSpace(responses[len(responses)-1].Content)
	}
	child.SetResult(final)
	child.SetStatus(subAgentOutcome(final))
	s.emitSubAgent(child, "", nil)
	return final, nil
}

// --- Interactive (experimental) sub-agents -------------------------------------

// AgentEventType classifies a coordinator-facing interactive sub-agent event.
type AgentEventType string

const (
	// AgentEventClarify means a sub-agent is blocked and asked a question.
	AgentEventClarify AgentEventType = "clarify"
	// AgentEventCompleted means a sub-agent finished successfully.
	AgentEventCompleted AgentEventType = "completed"
	// AgentEventFailed means a sub-agent failed or was terminated.
	AgentEventFailed AgentEventType = "failed"
)

// AgentEvent is delivered to the coordinator (the main agent) when an
// interactive sub-agent finishes or needs clarification.
type AgentEvent struct {
	AgentID string
	Name    string
	Type    AgentEventType
	Text    string
}

// InteractiveAgent tracks one asynchronous, conversational sub-agent.
type InteractiveAgent struct {
	ID    string
	Name  string
	agent *Agent
	inbox chan string   // coordinator → sub-agent messages (e.g. CLARIFY answers)
	done  chan struct{} // closed to request termination
	once  sync.Once
}

// LaunchInteractiveAgent starts an asynchronous sub-agent and returns its id
// immediately. The worker runs concurrently; the coordinator observes its
// progress via NextAgentEvent / InteractiveAgentStatus and can steer it with
// SendToInteractiveAgent / TerminateInteractiveAgent.
func (s *UserSession) LaunchInteractiveAgent(parentAgentID, name, task string) (string, error) {
	child, err := s.newSubAgent(parentAgentID, name, task, KindInteractive)
	if err != nil {
		return "", err
	}
	parent := child.GetParent()

	ia := &InteractiveAgent{
		ID:    child.ID,
		Name:  child.DisplayName(),
		agent: child,
		inbox: make(chan string, 4),
		done:  make(chan struct{}),
	}
	s.mu.Lock()
	s.interactive[child.ID] = ia
	s.mu.Unlock()

	parent.SetState(StateWaitingForSubAgent)
	child.SetStatus(StatusRunning)
	s.emitSubAgent(child, "launched: "+task, nil)

	go s.runInteractive(ia, task)
	return child.ID, nil
}

// runInteractive drives an interactive sub-agent across one or more rounds,
// pausing for coordinator input whenever the model replies CLARIFY:.
func (s *UserSession) runInteractive(ia *InteractiveAgent, task string) {
	message := task
	for {
		select {
		case <-ia.done:
			s.finishInteractive(ia, StatusFailed, "terminated by coordinator", AgentEventFailed)
			return
		default:
		}

		ia.agent.SetState(StateThinking)
		ia.agent.SetStatus(StatusRunning)
		responses, err := s.runLoop(ia.agent, ia.agent.ID, message, s.subAgentPrompt(false))
		ia.agent.SetState(StateIdle)
		if err != nil {
			s.finishInteractive(ia, StatusFailed, err.Error(), AgentEventFailed)
			return
		}

		final := ""
		if len(responses) > 0 {
			final = strings.TrimSpace(responses[len(responses)-1].Content)
		}

		if strings.HasPrefix(strings.ToUpper(strings.TrimSpace(final)), "CLARIFY") {
			ia.agent.SetStatus(StatusWaiting)
			ia.agent.SetResult(final)
			s.emitSubAgent(ia.agent, "", nil)
			s.pushAgentEvent(AgentEvent{AgentID: ia.ID, Name: ia.Name, Type: AgentEventClarify, Text: final})

			// Block until the coordinator answers or the agent is terminated.
			select {
			case reply, ok := <-ia.inbox:
				if !ok {
					s.finishInteractive(ia, StatusFailed, "terminated by coordinator", AgentEventFailed)
					return
				}
				message = reply
				continue
			case <-ia.done:
				s.finishInteractive(ia, StatusFailed, "terminated by coordinator", AgentEventFailed)
				return
			}
		}

		status := subAgentOutcome(final)
		evType := AgentEventCompleted
		if status == StatusFailed {
			evType = AgentEventFailed
		}
		s.finishInteractive(ia, status, final, evType)
		return
	}
}

// finishInteractive records a terminal status for an interactive sub-agent and
// notifies both the UI observer and the coordinator event stream.
func (s *UserSession) finishInteractive(ia *InteractiveAgent, status AgentStatus, result string, evType AgentEventType) {
	ia.agent.SetState(StateIdle)
	ia.agent.SetStatus(status)
	ia.agent.SetResult(result)
	if parent := ia.agent.GetParent(); parent != nil {
		parent.SetState(StateThinking)
	}
	s.emitSubAgent(ia.agent, "", nil)
	s.pushAgentEvent(AgentEvent{AgentID: ia.ID, Name: ia.Name, Type: evType, Text: result})
}

// pushAgentEvent delivers a coordinator event, dropping it if the buffer is full
// rather than blocking the sub-agent goroutine.
func (s *UserSession) pushAgentEvent(ev AgentEvent) {
	select {
	case s.agentEvents <- ev:
	default:
	}
}

// NextAgentEvent blocks for the next interactive sub-agent event, up to timeout.
// A non-positive timeout waits indefinitely. The boolean is false on timeout.
func (s *UserSession) NextAgentEvent(timeout time.Duration) (AgentEvent, bool) {
	if timeout <= 0 {
		ev := <-s.agentEvents
		return ev, true
	}
	select {
	case ev := <-s.agentEvents:
		return ev, true
	case <-time.After(timeout):
		return AgentEvent{}, false
	}
}

// SendToInteractiveAgent delivers a message (e.g. an answer to a CLARIFY) to a
// running interactive sub-agent.
func (s *UserSession) SendToInteractiveAgent(agentID, message string) error {
	s.mu.RLock()
	ia := s.interactive[agentID]
	s.mu.RUnlock()
	if ia == nil {
		return &NotFoundError{ID: agentID}
	}
	select {
	case ia.inbox <- message:
		return nil
	case <-ia.done:
		return fmt.Errorf("agent %s has terminated", agentID)
	}
}

// TerminateInteractiveAgent stops a running interactive sub-agent.
func (s *UserSession) TerminateInteractiveAgent(agentID string) error {
	s.mu.RLock()
	ia := s.interactive[agentID]
	s.mu.RUnlock()
	if ia == nil {
		return &NotFoundError{ID: agentID}
	}
	ia.once.Do(func() { close(ia.done) })
	return nil
}

// InteractiveAgentStatus returns the current status and last result of an
// interactive sub-agent.
func (s *UserSession) InteractiveAgentStatus(agentID string) (AgentStatus, string, error) {
	s.mu.RLock()
	ia := s.interactive[agentID]
	s.mu.RUnlock()
	if ia == nil {
		return "", "", &NotFoundError{ID: agentID}
	}
	return ia.agent.GetStatus(), ia.agent.GetResult(), nil
}

// ListInteractiveAgents returns the ids of all interactive sub-agents launched
// in this session.
func (s *UserSession) ListInteractiveAgents() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	ids := make([]string, 0, len(s.interactive))
	for id := range s.interactive {
		ids = append(ids, id)
	}
	return ids
}

// StopAgent stops an agent
func (s *UserSession) StopAgent(agentID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	agent := s.RootAgent.GetAgentByID(agentID)
	if agent == nil {
		return &NotFoundError{ID: agentID}
	}

	agent.SetState(StateIdle)
	return nil
}

// ResumeAgent resumes an agent
func (s *UserSession) ResumeAgent(agentID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	agent := s.RootAgent.GetAgentByID(agentID)
	if agent == nil {
		return &NotFoundError{ID: agentID}
	}

	agent.SetState(StateThinking)
	return nil
}

// InterruptAgent interrupts an agent
func (s *UserSession) InterruptAgent(agentID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	agent := s.RootAgent.GetAgentByID(agentID)
	if agent == nil {
		return &NotFoundError{ID: agentID}
	}

	agent.SetState(StateIdle)
	return nil
}

// CountMessages counts total messages in the session
func (s *UserSession) CountMessages() int {
	s.mu.RLock()
	defer s.mu.RUnlock()

	count := 0
	agents := s.RootAgent.ListAllAgents()
	for _, agent := range agents {
		count += len(agent.ThoughtTrain.GetHistory())
	}
	return count
}

// GetStats returns session statistics including token counts
func (s *UserSession) GetStats() map[string]interface{} {
	s.mu.RLock()
	defer s.mu.RUnlock()

	agents := s.RootAgent.ListAllAgents()
	fast := s.fastConnectorStatsLocked()
	return map[string]interface{}{
		"id":              s.ID,
		"agent_count":     len(agents),
		"total_turns":     s.CountMessages(),
		"tokens_in":       s.tokenCountIn,
		"tokens_out":      s.tokenCountOut,
		"tool_calls":      s.toolCallCount,
		"fast_tokens_in":  fast.TotalTokensIn,
		"fast_tokens_out": fast.TotalTokensOut,
	}
}

// FastConnectorStats returns the low-level connector statistics for this
// session's auxiliary/fast model backend (e.g. the compression completer), or a
// zero snapshot when no fast model is configured. This lets callers report
// fast-model usage and cost separately from the primary model.
func (s *UserSession) FastConnectorStats() model.StatsSnapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.fastConnectorStatsLocked()
}

// fastConnectorStatsLocked reads the fast-model connector stats. Callers must
// hold s.mu (read or write).
func (s *UserSession) fastConnectorStatsLocked() model.StatsSnapshot {
	if r, ok := s.compressionCompleter.(model.StatsReporter); ok {
		return r.StatsSnapshot()
	}
	return model.StatsSnapshot{}
}

// AddTokenUsage adds token usage to the session stats
func (s *UserSession) AddTokenUsage(promptTokens, completionTokens int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tokenCountIn += promptTokens
	s.tokenCountOut += completionTokens
}

// ConnectorStats aggregates the low-level model-connector statistics across all
// of this session's agents. These come straight from the HTTP layer (request
// counts, token totals, timing) and complement the higher-level session stats.
func (s *UserSession) ConnectorStats() model.StatsSnapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var total model.StatsSnapshot
	if s.RootAgent == nil {
		return total
	}
	for _, a := range s.RootAgent.ListAllAgents() {
		if a.ThoughtTrain != nil && a.ThoughtTrain.Model != nil {
			total = total.Add(a.ThoughtTrain.Model.StatsSnapshot())
		}
	}
	return total
}

// IncrementToolCall increments the tool call count
func (s *UserSession) IncrementToolCall() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.toolCallCount++
}

// NotFoundError represents an error when an agent is not found
type NotFoundError struct {
	ID string
}

func (e *NotFoundError) Error() string {
	return "agent not found: " + e.ID
}

// extractToolCallJSON extracts a JSON tool call from a response that may contain other text
func extractToolCallJSON(response string) string {
	// Find the start of a JSON object with "tool" or structured_output
	start := strings.Index(response, `{"tool"`)
	if start == -1 {
		// Try with spaces or other prefixes
		start = strings.Index(response, `{"tool":`)
		if start == -1 {
			// Try for structured_output: {"response": "...", "final": true}
			start = strings.Index(response, `{"response":`)
			if start == -1 {
				return ""
			}
		}
	}

	// Find the matching closing brace
	depth := 0
	for i := start; i < len(response); i++ {
		switch response[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return response[start : i+1]
			}
		}
	}

	return ""
}
