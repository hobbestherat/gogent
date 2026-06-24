package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
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
	// SessionEventThinkingDelta carries a chunk of the model's chain-of-thought
	// (reasoning) as it streams, for live display under the current turn (issue
	// #217). Text holds the delta; Step identifies the turn. Emitted only when the
	// streaming-thinking option is enabled and the backend streams reasoning, so it
	// is absent (a no-op) otherwise.
	SessionEventThinkingDelta SessionEventType = "thinking_delta"
	// SessionEventThinkingDone signals that the current turn's streamed thinking is
	// complete, so a UI can fold (collapse) the live thinking entry (issue #217).
	// Emitted after each streamed model round-trip when the option is enabled; a UI
	// with no live thinking entry treats it as a no-op.
	SessionEventThinkingDone SessionEventType = "thinking_done"
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
	// SessionEventUsage carries a fresh SessionStats snapshot, emitted after each
	// model round-trip so UIs can render live per-session token/turn/context usage
	// (e.g. a status bar) without polling the session.
	SessionEventUsage SessionEventType = "usage"
	// SessionEventTodo carries an updated task checklist (issue #43). The list is
	// the session's current todo state, rendered in the sidebar.
	SessionEventTodo SessionEventType = "todo"
	// SessionEventPlan carries a proposed plan produced in plan mode, awaiting the
	// user's approval before the agent executes it (issue #43).
	SessionEventPlan SessionEventType = "plan"
	// SessionEventYolo announces the session's effective yolo state (issue #356):
	// Yolo holds the current value. The backend emits it on toggle and at session
	// creation, so the status indicator is driven by the backend (never a UI-local
	// mirror) and config/CLI-activated yolo is announced too. A UI sets a
	// display-only field from it and refreshes the status line.
	SessionEventYolo SessionEventType = "yolo"
	// SessionEventBackground announces whether the session currently has async
	// (fire-and-forget) sub-agents running in the background (issue #353). Background
	// holds the current value. The backend emits it on the 0→1 and 1→0 edges of the
	// background-worker count, so a UI can show a third "working in background" state
	// (distinct from idle) while the main loop is done but spawned workers run on.
	// It is backend-owned, like SessionEventYolo: a UI sets a display-only field from
	// it rather than inferring background-ness from the sub-agent tree.
	SessionEventBackground SessionEventType = "background"
)

// SessionStats is a point-in-time, mutex-free snapshot of a session's per-session
// statistics. It is what UIs render as a compact status line (tokens, turns,
// context usage) and is carried by SessionEventUsage.
type SessionStats struct {
	// Turns is the number of completed user turns (top-level task loops).
	Turns int
	// TokensIn / TokensOut are the cumulative prompt/completion token totals for
	// the session's primary model, including tokens spent by its sub-agents.
	TokensIn  int
	TokensOut int
	// ToolCalls is the total number of tool executions in the session.
	ToolCalls int
	// ContextTokens is the root agent's current context size in tokens; the
	// figure a status bar shows as a percentage of ContextWindow and the
	// early-warning before context compaction (see issue #4).
	ContextTokens int
	// ContextWindow is the root agent's configured context budget in tokens. Zero
	// means unknown, in which case ContextTokens carries no percentage.
	ContextWindow int
}

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

	// CallID is the stable identifier pairing a SessionEventToolCall with its
	// SessionEventToolResult (issue #187). A UI uses it to flip the right tool
	// entry from "running" to a terminal state — essential for concurrent tool
	// batches, where several calls are in flight at once and their results may
	// arrive out of order. It is the model-supplied tool-call id when present
	// and a turn-unique synthetic id (tool#step.index) otherwise, so the
	// fallback JSON tool-call path and repeated tool names still pair one to
	// one. Populated only on ToolCall/ToolResult events.
	CallID string

	// Sub-agent identity/status (populated on SessionEventSubAgent) so a UI can
	// maintain a live session → sub-agent tree with per-agent status.
	AgentID string
	Name    string
	Status  AgentStatus
	Kind    SubAgentKind

	// Stats carries a fresh SessionStats snapshot on SessionEventUsage.
	Stats SessionStats

	// Todos carries the session's current task checklist on SessionEventTodo.
	Todos []TodoItem
	// Plan carries the proposed plan on SessionEventPlan (plan mode).
	Plan string
	// Yolo carries the effective yolo state on SessionEventYolo (issue #356).
	Yolo bool
	// Background carries whether any async sub-agent is running, on
	// SessionEventBackground (issue #353).
	Background bool
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
	// maxSteps caps how many model round-trips (steps/turns) runLoop may take per
	// task before it stops (issue #249). It holds the resolved value: a positive N
	// caps at N, while <= 0 means UNLIMITED — the loop is then bounded only by its
	// other stop conditions. It is shared by every runLoop on this session: the
	// root task loop and every one-shot / interactive sub-agent loop spawned from
	// it (which all run against this same receiver), so the cap applies uniformly,
	// just as the historical fixed bound did. Defaults to config.DefaultMaxSteps so
	// an unwired session keeps gogent's historical fixed bound.
	maxSteps int
	// subAgentTimeoutMs bounds how long a spawned sub-agent may run. Zero leaves
	// the agent's built-in default in place.
	subAgentTimeoutMs int64
	// subAgentLimiter bounds how many sub-agent loops run concurrently. It is
	// typically a process-wide limiter shared across every session (see
	// SetSubAgentLimiter), so a deep fan-out cannot spawn an unbounded number of
	// goroutines (issue #23). Nil means unbounded.
	subAgentLimiter *SubAgentLimiter
	// rateLimiter paces this session's model round-trips against the provider's
	// request-rate ceiling. Like subAgentLimiter it is typically a process-wide
	// limiter shared across every session, so the global request rate is governed
	// regardless of how many sessions or cluster nodes fan out at once (issue #28).
	// Nil means unthrottled.
	rateLimiter *RateLimiter

	// systemContextFn, when set, returns extra system-prompt context (project
	// AGENTS.md instructions, the available-skills index and the live todo
	// checklist) appended to every agent loop's system prompt. It takes the
	// session id so per-session state (the checklist) can be threaded in.
	// Evaluated per loop so runtime changes (skill activation, todo updates) are
	// reflected.
	systemContextFn func(sessionID string) string

	// Interactive (experimental) sub-agent bookkeeping.
	interactive map[string]*InteractiveAgent
	// background tracks async (fire-and-forget, one-shot) sub-agents launched by
	// spawn_subagent{async:true} (issue #353). An entry exists only while its worker
	// goroutine runs; runBackground removes it on exit. len(background) > 0 is what
	// HasBackgroundWork reports, driving the "working in background" session state.
	// Guarded by mu.
	background map[string]*BackgroundAgent
	// backgroundResults holds completed async sub-agent results awaiting re-injection
	// into the root loop's transcript (issue #353). Appended (FIFO, completion order)
	// by the worker goroutine and drained by the root runLoop at a turn boundary, so
	// the loop — the sole owner of the transcript — never races the workers on it.
	// Guarded by mu.
	backgroundResults []string
	agentEvents       chan AgentEvent
	// pendingTerminal holds terminal (completed/failed) events that did not fit
	// in agentEvents when it was full. Terminal events must never be dropped, or
	// a coordinator's wait_agent_event blocks forever on an event that was
	// discarded (issue #27). Guarded by mu; drained by NextAgentEvent.
	pendingTerminal []AgentEvent

	// Task tracking for multi-turn tool calling
	currentTask *tool.Task

	// todos is the session's current task checklist, maintained by the todo tool
	// and rendered in the sidebar (issue #43). Guarded by mu.
	todos []TodoItem
	// planMode, when true, runs the next root-agent turn against a write-free tool
	// set with a planning prompt, so the agent investigates and proposes a plan
	// instead of making changes (issue #43). pendingPlan holds the plan awaiting
	// the user's approval; approving it re-runs the turn with the full tool set.
	// Both are guarded by mu.
	planMode    bool
	pendingPlan string

	// injectQueuedInput enables mid-turn injection of a queued user note at the
	// next turn boundary in runLoop, instead of the UI waiting for full idle to
	// drain its queue (issue #170, phase 2). It is now enabled for every session
	// (SetInjectQueuedInput(true)) as the agent-side path behind the per-message
	// Interject button (issue #201), which replaced the removed
	// experimental.inject_queued_input flag. Guarded by mu.
	injectQueuedInput bool
	// streamThinking, when true, streams the model's chain-of-thought (reasoning)
	// tokens live into the transcript and folds them once each turn's thinking
	// completes (issue #217). It is opt-in (off by default) and only affects the
	// root agent's turns; with it off the loop uses the blocking model path exactly
	// as before. Guarded by mu.
	streamThinking bool
	// pendingNote holds a user note (a queued message or a future supervisor nudge,
	// issue #172) to splice into the running loop at the next turn boundary. It is
	// a single latest-wins slot — the same edit-in-place semantics as the UI queue
	// — written from the UI goroutine via InjectUserNote and drained by the loop
	// goroutine in runLoop, both under mu (mirroring how agent.cancel is published
	// and read under the agent mutex). Empty means nothing pending.
	pendingNote string

	// compressionCompleter, when set, runs context compression on a separate
	// (typically smaller/faster) model backend instead of the session's primary
	// model. When it also reports connector stats, its usage is tracked apart
	// from the primary model (see FastConnectorStats).
	compressionCompleter model.Completer

	// Stats
	tokenCountIn  int
	tokenCountOut int
	toolCallCount int
	// turnCount is the number of completed top-level task loops (one per user
	// turn). It backs the SessionStats.Turns figure shown in the status bar.
	turnCount int
	// compactionCount is how many context-compression passes have run in this
	// session (see compactIfNeeded). It feeds the Statistics view's compaction
	// breakdown (issue #57).
	compactionCount int
	// primaryModel is the name of the model the session currently routes its
	// primary turns through. It attributes token usage to a model (see
	// perModelTokens) for the per-model breakdown in the Statistics view. Empty
	// means unknown / not yet set, in which case tokens are counted only in the
	// session totals.
	primaryModel string
	// perModelTokens attributes prompt/completion tokens to each model the
	// session has used. Keyed by model config name.
	perModelTokens map[string]modelTokens

	// perModelConn is the STABLE, always-monotonic per-model accumulator for the
	// low-level connector metrics (requests, errors, cached tokens, timeouts,
	// latency) that only the connector tracks. It mirrors perModelTokens (keyed by
	// model config name) and is the fix for issue #191: the live *ModelConnection
	// the panel used to read is rebuilt-and-zeroed every turn, shared by sub-agents
	// (double-counted) and lost on restart. Instead, recordConnectorUsage folds the
	// per-read DELTA of the connector snapshot into this accumulator, attributed to
	// the active primaryModel, so totals never reset across turns, model switches or
	// sub-agent spawns and are never double-counted. Guarded by mu.
	perModelConn map[string]model.StatsSnapshot
	// lastConnSnap is the connector snapshot read on the previous recordConnectorUsage
	// call, the baseline the next delta is measured against. Guarded by mu.
	lastConnSnap model.StatsSnapshot
}

// modelTokens is the per-model token accumulator (prompt/completion totals).
type modelTokens struct {
	In  int
	Out int
}

// NewUserSession creates a new user session
func NewUserSession(id string, agent *Agent) *UserSession {
	return &UserSession{
		ID:             id,
		RootAgent:      agent,
		CreatedAt:      time.Now().Unix(),
		ToolCallback:   nil,
		subAgentCfg:    config.DefaultSubAgentConfig(),
		maxSteps:       config.DefaultMaxSteps,
		interactive:    make(map[string]*InteractiveAgent),
		background:     make(map[string]*BackgroundAgent),
		agentEvents:    make(chan AgentEvent, 64),
		currentTask:    nil,
		tokenCountIn:   0,
		tokenCountOut:  0,
		toolCallCount:  0,
		turnCount:      0,
		perModelTokens: make(map[string]modelTokens),
		perModelConn:   make(map[string]model.StatsSnapshot),
	}
}

// injectedNoteTemplate is the wording used when a queued user note is spliced
// into a running turn at the next turn boundary (issue #170, phase 2). It is a
// named constant (not inline prose) so the phrasing is easy to tune in one
// place; %s is the queued text. A future supervisor (issue #172) reuses the same
// injection path via InjectUserNote and gets the same framing.
const injectedNoteTemplate = "[The user added a clarification: %s]"

// Continuation-nudge tuning for the bounded in-loop preamble recovery (issue
// #307). A reasoning model sometimes emits a bare *preamble* — an announcement
// of intent ("I'll start by…", "Let me…") — as its own tool-free turn before it
// actually calls the tool it just described. runLoop's default rule (a turn with
// no tool calls is the final answer) abandons such a task mid-narration. When a
// tool-free turn looks like a preamble, the loop splices ONE cheap continuation
// note (reusing the same note-injection path as queued user notes) and gives the
// model another round-trip instead of breaking.
const (
	// maxContinuationNudges bounds how many times a single uninterrupted stretch
	// of tool-free turns may be nudged before the loop accepts the text as final.
	// Kept at 1 so a model that genuinely has nothing more to do cannot loop: once
	// the bound is hit the loop falls back to the normal final-answer break. The
	// budget is reset the moment a real tool call happens (see runLoop), so a long
	// task still earns a fresh nudge after each productive turn.
	maxContinuationNudges = 1

	// continuationNudgeNote is the user-role note spliced in to give a preamble
	// turn one more round-trip. It is phrased as a neutral either/or so a model
	// that truly is finished can still terminate on the next turn (it just states
	// it is done) rather than being pushed into busywork. Like the queued-note
	// injection it is deliver-only: it rides into the transcript for the model but
	// is never emitted as a SessionEventAssistantStep, so it stays out of the
	// user-visible chat.
	continuationNudgeNote = "[Continue: call the tools you described, or state that you are done.]"

	// truncatedToolCallNote is the user-role note spliced in when a tool-call turn
	// was cut off by max_tokens (finish_reason "length") and left a call with
	// malformed (truncated) arguments (issue #390). It asks the model to resume
	// and re-emit the interrupted call in full rather than have the partial JSON
	// fed to validateArgs as a failed call. Like the preamble nudge it is
	// deliver-only and bounded by maxContinuationNudges so a model that never
	// completes the call cannot loop.
	truncatedToolCallNote = "[Your previous tool call was cut off before its arguments finished. Re-issue that tool call in full, with complete JSON arguments.]"

	// finalToolCallResultNote is the synthetic tool result recorded for a terminal
	// tool call that ends the loop without a real result — a folded (or salvaged,
	// truncated) structured_output{final} (issue #390). It exists only to keep the
	// persisted transcript balanced (every assistant tool_calls answered one-to-one)
	// so a reused session's next user turn is a valid request; the model only ever
	// sees it if the conversation continues past the final answer.
	finalToolCallResultNote = "[Final answer recorded and delivered to the user.]"

	// maxPreambleLen caps how long a tool-free turn may be and still be treated as
	// a preamble. A genuine final answer — a summary, an explanation, a code block
	// — runs long; a preamble is a sentence or two. Anything longer is final.
	maxPreambleLen = 400
)

// preamblePrefixes are the case-insensitive opening phrases that mark a turn as
// an announcement of a NEXT action rather than a final answer (issue #307). The
// match is against the trimmed prefix, so a turn must literally START with an
// intent phrase to qualify — the decisive, deliberately narrow signal.
var preamblePrefixes = []string{
	"i'll ", "i will ", "i am going to", "i'm going to",
	"let me ", "let's ", "let us ",
	"first, ", "first i", "first of all",
	"to begin", "to start", "next, ",
	"now i'll", "now i will", "now let me", "now let's",
	"starting ", "i plan to", "i'm starting", "i am starting",
}

// completionMarkers are substrings whose presence means the model is presenting
// a RESULT (so the turn is final), not announcing a step. Their presence vetoes
// the preamble classification even when an intent phrase opens the text (issue
// #307). Matched case-insensitively against the whole text.
var completionMarkers = []string{
	"```", // a code fence — the turn is presenting code/output
	"done", "finished", "complete", "all set",
	"in summary", "to summarize", "in conclusion",
	"here is", "here are", "here's",
	"the answer is", "the result is", "the fix is",
	"no further", "nothing further",
	"successfully", "i have ", "i've ", // past-tense report of work already done
}

// SetInjectQueuedInput toggles mid-turn injection of queued user notes at the
// next turn boundary (issue #170, phase 2). Production wiring enables it for every
// session as the agent-side path behind the per-message Interject button (issue
// #201); Enter/Queue still drain on idle (phase 1) regardless.
func (s *UserSession) SetInjectQueuedInput(on bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.injectQueuedInput = on
}

// InjectQueuedInput reports whether mid-turn injection is enabled (issue #170).
func (s *UserSession) InjectQueuedInput() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.injectQueuedInput
}

// SetStreamThinking toggles live streaming of the model's chain-of-thought into
// the transcript (issue #217). Off by default; enabling it routes the root
// agent's model round-trips through the streaming backend so reasoning deltas are
// surfaced as SessionEventThinkingDelta and folded on SessionEventThinkingDone.
func (s *UserSession) SetStreamThinking(on bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.streamThinking = on
}

// StreamThinking reports whether live thinking-token streaming is enabled (issue
// #217).
func (s *UserSession) StreamThinking() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.streamThinking
}

// InjectUserNote hands the running task loop a user note to splice into the
// conversation at the next turn boundary, before the next model round-trip
// (issue #170, phase 2). It is the single, reusable entry point a UI uses to
// inject a queued message mid-turn, and that a future idle-watchdog supervisor
// (issue #172) reuses to nudge a session — neither has to touch the loop's
// internals.
//
// The note is held in a latest-wins slot (a newer note replaces an undrained
// one) and is guarded by the session mutex, so it is safe to call from any
// goroutine and at any time, including when no loop is running: an idle session
// simply holds the note until its next turn starts, where the loop drains it.
// Empty/whitespace text is ignored. The text is framed by the loop at drain
// time (see injectedNoteTemplate); callers pass the raw user text.
func (s *UserSession) InjectUserNote(text string) {
	text = strings.TrimSpace(text)
	if text == "" {
		return
	}
	s.mu.Lock()
	s.pendingNote = text
	s.mu.Unlock()
}

// takePendingNote atomically reads and clears the pending user note, returning
// "" when none is queued. The loop calls it at each turn boundary so a note is
// spliced once and not re-injected on the following turn.
func (s *UserSession) takePendingNote() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	note := s.pendingNote
	s.pendingNote = ""
	return note
}

// SetSubAgentConfig updates the sub-agent execution-model settings used when
// spawning sub-agents from this session.
func (s *UserSession) SetSubAgentConfig(cfg config.SubAgentConfig) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.subAgentCfg = cfg
}

// SetMaxSteps sets the per-task step (model round-trip) cap used by runLoop. A
// positive value caps the loop at that many steps; a value of 0 (or any
// non-positive value) means UNLIMITED — the loop runs until one of its other
// stop conditions fires (final answer, token budget, cancellation). The cap is
// shared by the root task loop and every sub-agent / interactive loop spawned on
// this session, so 0 unbounds those nested loops too. The value is read once at
// each loop's start, so a change takes effect on the next task, not mid-run. See
// issue #249 and Config.MaxStepsOrDefault.
func (s *UserSession) SetMaxSteps(n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.maxSteps = n
}

// MaxSteps returns the current per-task step cap (<= 0 means unlimited).
func (s *UserSession) MaxSteps() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.maxSteps
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
// context (project AGENTS.md instructions, the available-skills index and the
// live todo checklist). It receives the session id so per-session state can be
// threaded in, and is evaluated at the start of each agent loop.
func (s *UserSession) SetSystemContextProvider(fn func(sessionID string) string) {
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
	return fn(s.ID)
}

// SetSubAgentLimiter installs the (typically process-wide) limiter that bounds
// how many sub-agent loops this session may run concurrently. Passing nil
// restores unbounded behavior.
func (s *UserSession) SetSubAgentLimiter(l *SubAgentLimiter) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.subAgentLimiter = l
}

// SetRateLimiter installs the (typically process-wide) limiter that paces this
// session's model round-trips against the provider's request-rate ceiling.
// Passing nil restores unthrottled behavior.
func (s *UserSession) SetRateLimiter(l *RateLimiter) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rateLimiter = l
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

// EmitYolo announces the session's effective yolo state to its observer (issue
// #356), so the UI status indicator is driven by the backend rather than a
// UI-local mirror. It is a no-op when no observer is registered.
func (s *UserSession) EmitYolo(on bool) {
	s.emit(SessionEvent{Type: SessionEventYolo, Yolo: on})
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
		Content: s.buildMessageWithTools(agent.ToolRegistry, message),
	}

	// Send message to the agent's model session
	if agent.ThoughtTrain != nil {
		resp, err := agent.ThoughtTrain.Send([]model.Message{msg})
		if err != nil {
			return nil, fmt.Errorf("send message: %w", err)
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
			return nil, fmt.Errorf("send message: %w", err)
		}
		return resp, nil
	}

	return nil, nil
}

// buildMessageWithTools prepends a tool catalog to the message for the legacy
// single-shot SendMessage path. The catalog is generated from the live registry
// (reg.RenderToolDocs), not hand-maintained, so it cannot drift from the
// authoritative Tool.Description/InputSchema the live task loop sends as native
// function definitions (issue #357). reg may be nil or empty, in which case the
// catalog section is omitted.
func (s *UserSession) buildMessageWithTools(reg *tool.ToolRegistry, message string) string {
	var toolDocs string
	if reg != nil {
		toolDocs = reg.RenderToolDocs()
	}

	var b strings.Builder
	if toolDocs != "" {
		b.WriteString("You have access to the following tools:\n\n")
		b.WriteString(toolDocs)
		b.WriteString("\n\n")
	}
	b.WriteString(`IMPORTANT INSTRUCTIONS:
1. ALWAYS use the read tool to verify file existence and get actual content before making assumptions
2. Do not state that a file doesn't exist without first using the read tool
3. Tool execution happens automatically - you just need to request it
4. Tool results will be sent back to you after execution

To use a tool, output a JSON object with:
{"tool": "tool_name", "args": {"key": "value"}}

For final responses, use:
{"response": "...", "final": true}

`)
	b.WriteString(message)
	return b.String()
}

// ExecuteTaskLoop runs the multi-turn task loop with tool calling.
//
// It uses native OpenAI tool-calling when the model/server supports it (the
// reliable path for small models) and transparently falls back to parsing a
// JSON tool call out of the assistant's text when no native tool_calls are
// returned. Tool results are fed back as proper role:"tool" messages so the
// model keeps full context across turns.
func (s *UserSession) ExecuteTaskLoop(ctx context.Context, agentID string, initialMessage string) ([]*model.CompletionResponse, error) {
	s.mu.Lock()
	agent := s.RootAgent.GetAgentByID(agentID)
	s.mu.Unlock()

	if agent == nil {
		return nil, &NotFoundError{ID: agentID}
	}
	if agent.ThoughtTrain == nil {
		return nil, fmt.Errorf("agent %s has no model session", agentID)
	}

	// One user turn == one top-level task loop.
	s.mu.Lock()
	s.turnCount++
	s.mu.Unlock()

	// Plan mode (issue #43): run the root agent against a write-free tool set with
	// a planning prompt for this turn, then surface its answer as an approval-
	// gated plan. The swap is scoped to this turn and restored on exit so later
	// turns (and sub-agents, which bypass ExecuteTaskLoop) keep the full tool set.
	planMode := s.PlanMode() && agent.Kind == KindRoot
	if planMode {
		full := agent.ToolRegistry
		agent.SetToolRegistry(full.CloneForPlanMode(planKeptTools...))
		defer agent.SetToolRegistry(full)
	}

	tools := toolDefsFromRegistry(agent.ToolRegistry)
	systemPrompt := buildAgentSystemPrompt(len(tools) > 0, s.SubAgentConfig())
	if planMode {
		// Only advertise read-only delegation when spawn_subagent actually survived
		// the plan-mode filter. In one-shot mode it does (planKeptTools); in
		// interactive mode toolRegistryForMode strips spawn_subagent and the
		// non-read-only interactive coordination tools are dropped by CloneForPlanMode,
		// leaving no delegation tool — so the prompt must not promise one (issue #281).
		canDelegate := agent.ToolRegistry.Get("spawn_subagent") != nil
		systemPrompt = planModeSystemPromptWith(systemPrompt, canDelegate)
	}

	responses, err := s.runLoop(ctx, agent, agentID, initialMessage, systemPrompt)
	if err != nil {
		return responses, err
	}
	if planMode {
		s.recordPlan(responses)
	}
	return responses, nil
}

// planKeptTools are the non-read-only tools retained in plan mode alongside the
// read-only investigation tools: todo (to lay out the plan's steps),
// structured_output (to finalize the plan) and spawn_subagent (to fan out
// bounded, read-only investigation in parallel while planning, issue #281).
// Everything else side-effecting is stripped by CloneForPlanMode (issue #43).
//
// Keeping spawn_subagent does NOT let plan mode mutate the workspace: a
// sub-agent's registry is cloned from the parent's (see newSubAgent), which in
// plan mode is the already-plan-filtered, read-only registry — so a plan-mode
// child inherits read/grep/glob/list/diagnostics but not write/edit/multi_edit/
// apply_patch/shell. The fan-out stays bounded by the shared SubAgentLimiter and
// the per-parent max-sub-agents cap, exactly as outside plan mode.
var planKeptTools = []string{"todo", "structured_output", "spawn_subagent"}

// recordPlan captures the final answer of a plan-mode turn as the plan awaiting
// approval and emits SessionEventPlan so the UI can offer to approve it. An
// empty plan is ignored rather than surfaced as an approval gate (issue #43).
func (s *UserSession) recordPlan(responses []*model.CompletionResponse) {
	plan := ""
	if len(responses) > 0 {
		plan = responses[len(responses)-1].Content
	}
	if !s.setPendingPlan(plan) {
		return
	}
	s.emit(SessionEvent{Type: SessionEventPlan, Plan: strings.TrimSpace(plan)})
}

// planModeSystemPrompt layers planning instructions on top of the base agent
// prompt for a plan-mode turn that can delegate (the default one-shot config,
// where spawn_subagent survives the plan-mode filter). It tells the model the
// workspace is read-only this turn and that its answer is a plan for the user to
// approve, not something to carry out (issue #43), and permits bounded,
// read-only sub-agent delegation so the agent can fan out parallel investigation
// while it plans (issue #281). ExecuteTaskLoop calls planModeSystemPromptWith
// with the actual delegation availability; this wrapper is the canonical
// delegation-enabled prompt.
func planModeSystemPrompt(base string) string {
	return planModeSystemPromptWith(base, true)
}

// planModeSystemPromptWith builds the plan-mode prompt, appending the read-only
// delegation guidance only when canDelegate is true. The delegation paragraph
// names spawn_subagent, so it must not be shown when that tool is absent from the
// plan-mode registry — e.g. in interactive sub-agent mode, where
// toolRegistryForMode strips spawn_subagent and CloneForPlanMode drops the
// (non-read-only) interactive coordination tools, leaving no delegation tool.
// Advertising delegation there would have the model emit calls that fail as
// unknown, wasting turns (issue #281 regression guard).
func planModeSystemPromptWith(base string, canDelegate bool) string {
	prompt := base + `

## PLAN MODE (read-only)
You are in PLAN MODE. The tools that modify the workspace (write, edit,
multi_edit, apply_patch, shell) are unavailable this turn. Use the read-only
tools to investigate, then reply with a concrete, step-by-step plan the user
will approve before you execute it.`
	if canDelegate {
		prompt += `

You MAY delegate read-only investigation to sub-agents to research the codebase
in parallel while you plan. Batch the independent lookups into a SINGLE
spawn_subagent call's "subtasks" array (e.g. one sub-agent per module to
summarize its structure, or diagnostics + grep together) so they run
concurrently. The sub-agents are also read-only this turn: they investigate and
report findings only — they must NOT write, edit, or otherwise change anything.
The asynchronous launch_agent family is NOT available this turn — use the blocking
spawn_subagent only, and ignore any earlier guidance about launching async agents.`
	} else {
		// No delegation tool survived the plan-mode filter (interactive mode strips
		// spawn_subagent and CloneForPlanMode drops the non-read-only interactive
		// coordination tools). The base prompt's coordinatorInstructions still
		// advertises those tools, so explicitly neutralize that stale guidance —
		// otherwise the model emits delegation calls that fail as unknown this turn
		// (issue #281: restores the coherence the removed "tools unavailable" line
		// used to provide for interactive mode).
		prompt += `

Sub-agent delegation is unavailable this turn: do the investigation yourself with
the read-only tools, and ignore any earlier guidance about launching or spawning
sub-agents.`
	}
	prompt += `

Lay the plan's steps out with the todo tool. Do NOT attempt to carry the plan
out — present it as your final answer.`
	return prompt
}

// budgetExceededMarker prefixes an agent's final result when it stopped because
// its token budget was reached. It doubles as the signal subAgentOutcome uses to
// classify the run as failed (the task did not finish) (issue #28).
const budgetExceededMarker = "BUDGET_EXCEEDED"

// waitRateLimit blocks until the session's rate limiter grants a permit (or ctx
// is cancelled). It is a no-op when no limiter is installed.
func (s *UserSession) waitRateLimit(ctx context.Context) error {
	s.mu.RLock()
	rl := s.rateLimiter
	s.mu.RUnlock()
	return rl.Wait(ctx)
}

// modelRoundTrip performs one paced, accounted model request: it waits on the
// rate limiter, sends, and folds the reported usage into the agent's per-agent
// token total so the loop can enforce its budget. It centralizes the three call
// sites in runLoop so rate limiting and token accounting cannot drift apart.
//
// onReasoning, when non-nil, receives the model's chain-of-thought deltas as
// they stream (issue #217); nil selects the blocking model path unchanged.
func (s *UserSession) modelRoundTrip(ctx context.Context, sess *model.ModelSession, agent *Agent, messages []model.Message, tools []model.ToolDef, onReasoning model.ReasoningSink) (*model.CompletionResponse, error) {
	if err := s.waitRateLimit(ctx); err != nil {
		return nil, err
	}
	// A non-nil sink routes the request through the streaming backend so reasoning
	// deltas surface live; nil keeps the byte-identical blocking path (issue #217).
	resp, err := sess.SendWithToolsStreamCtx(ctx, messages, tools, onReasoning)
	// Fold this round-trip's connector activity into the stable per-model
	// accumulator (issue #191), reading the snapshot once as a delta. Done even on
	// error: the connector records error/timeout outcomes on its own counters, so
	// those must be captured too. sess.Model is the connector that did the work;
	// sub-agents share the root's connector pointer, so reading it here (one read
	// per round-trip) attributes usage exactly once with no double-counting.
	if sess != nil && sess.Model != nil {
		s.recordConnectorUsage(sess.Model)
	}
	if err != nil {
		return nil, fmt.Errorf("model round-trip: %w", err)
	}
	if resp != nil && resp.Usage != nil {
		agent.AddTokensUsed(resp.Usage.PromptTokens, resp.Usage.CompletionTokens)
	}
	return resp, nil
}

// recordConnectorUsage folds the delta of a connector's snapshot (since the last
// read) into the stable per-model connector accumulator, attributed to the
// session's current primary model (issue #191). It is the connector-metric sibling
// of AddTokenUsage: where that keeps tokenCountIn/Out and perModelTokens monotonic
// with a plain +=, this keeps requests/errors/cache/timeouts/latency monotonic by
// accumulating per-read deltas rather than reading the live, per-turn-rebuilt
// connector.
//
// A reset delta (any counter going backwards — see StatsSnapshot.IsReset) means the
// connector was replaced with a zeroed one between reads (a per-turn rebuild without
// a carry); the snapshot is then treated as a fresh baseline so the accumulator only
// ever grows. This makes the accumulator correct independently of the
// ModelStats.Carry mitigation, which is left in place. The snapshot is read under the
// session lock so concurrent sub-agents sharing the connector cannot interleave a
// stale read with the baseline update.
func (s *UserSession) recordConnectorUsage(conn model.StatsReporter) {
	if conn == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	// Read the snapshot under the session lock. Sub-agents share the root's
	// connector pointer and run concurrently — one-shot spawns fan out across
	// goroutines (RunSubAgentsBounded) and interactive agents run in their own
	// goroutine — so each calls this on the same *s and the same connector. Reading
	// cur outside the lock opened a TOCTOU window: a stale cur could be written back
	// to lastConnSnap, rewinding the baseline and double-counting the next delta.
	// Reading inside the lock serializes read+update so each call folds exactly its
	// share of the monotonic growth. The snapshot only briefly takes the connector's
	// own stats mutex (lock order s.mu -> Stats.Mutex; the token callback never holds
	// Stats.Mutex, so there is no inverse ordering / deadlock).
	cur := conn.StatsSnapshot()
	delta := cur.Sub(s.lastConnSnap)
	if delta.IsReset() {
		// Connector was rebuilt/zeroed between reads; treat cur as a fresh baseline
		// so the accumulator only ever grows (it never loses prior totals). IsReset
		// checks every counter, not just RequestCount, so a rebuild whose request
		// count recovers to its prior level with lower token counters is still caught.
		delta = cur
	}
	s.lastConnSnap = cur
	s.perModelConn[s.primaryModel] = s.perModelConn[s.primaryModel].Add(delta)
}

// stopForBudget folds a graceful BUDGET_EXCEEDED notice into the agent's last
// response so the loop can break with a final answer that records the stop (and,
// for sub-agents, is classified as a failed/incomplete run). Any partial progress
// the model had produced is preserved beneath the notice.
func stopForBudget(agent *Agent, resp *model.CompletionResponse) *model.CompletionResponse {
	if resp == nil {
		resp = &model.CompletionResponse{}
	}
	note := fmt.Sprintf("%s: token budget reached (%d/%d tokens); stopping early.",
		budgetExceededMarker, agent.GetTokensUsed(), agent.TokenBudget)
	if partial := strings.TrimSpace(resp.Content); partial != "" {
		note += "\n\nPartial progress before stopping:\n" + partial
	}
	resp.Content = note
	return resp
}

// runLoop is the shared multi-turn tool-calling loop used by both the top-level
// task loop and sub-agents (sub-agents pass a different system prompt).
func (s *UserSession) runLoop(ctx context.Context, agent *Agent, agentID, initialMessage, systemPrompt string) (responses []*model.CompletionResponse, err error) {
	if ctx == nil {
		ctx = context.Background()
	}
	// Scope a cancellable context to this loop and publish its cancel func on the
	// agent so StopAgent / session close can abort the in-flight model work. The
	// child context inherits the caller's, so cancelling a parent loop (or the
	// whole session) propagates into sub-agent loops spawned from here (issue #24).
	ctx, cancel := context.WithCancel(ctx)
	agent.setCancel(cancel)
	defer func() {
		cancel()
		agent.setCancel(nil)
	}()

	sess := agent.ThoughtTrain
	tools := toolDefsFromRegistry(agent.ToolRegistry)

	// Rebuild the system prompt before every model round-trip (not just once at
	// loop start) so the extra system context — the skills index, the live git
	// status and the todo checklist — reflects the latest session state. This is
	// essential after an intra-turn compaction: compaction summarizes the
	// originating todo tool calls out of the transcript, and re-injecting here is
	// what keeps the checklist in front of the model on the very next round-trip
	// within the same turn (issue #263). The base prompt is captured once so the
	// per-loop context is re-appended fresh rather than accumulated.
	baseSystemPrompt := systemPrompt
	refreshSystemPrompt := func() {
		full := baseSystemPrompt
		if sc := s.systemContext(); sc != "" {
			full += "\n\n" + sc
		}
		sess.SetSystemPrompt(full)
	}

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

	// Live thinking (issue #217): only the root agent streams its reasoning into
	// the session window, and only when the option is enabled. reasoningSink builds
	// the per-step sink that turns each streamed reasoning delta into a
	// SessionEventThinkingDelta; it returns nil when streaming is off so
	// modelRoundTrip takes the blocking path unchanged. After each round-trip the
	// loop emits SessionEventThinkingDone so the UI folds the live entry.
	streamThinking := s.StreamThinking() && agent.Kind == KindRoot
	reasoningSink := func(step int) model.ReasoningSink {
		if !streamThinking {
			return nil
		}
		return func(delta string) {
			emit(SessionEvent{Type: SessionEventThinkingDelta, Step: step, Text: delta})
		}
	}
	thinkingDone := func(step int) {
		if streamThinking {
			emit(SessionEvent{Type: SessionEventThinkingDone, Step: step})
		}
	}

	// Contain a panic anywhere in the model/tool loop (tool-call parsing, a
	// slice index, a type assertion) so it fails this one agent instead of
	// crashing the whole multi-session process. Every loop-driven goroutine —
	// the root task loop, parallel sub-agent spawns, and interactive workers —
	// funnels through here, so this is the single guard for all of them (issue
	// #8). The defer is registered after emit is set so the panic can be
	// reported to the (root) session window like any other loop error.
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("agent loop panicked: %v", r)
			emit(SessionEvent{Type: SessionEventError, Err: err})
		}
	}()

	responses = make([]*model.CompletionResponse, 0)

	emit(SessionEvent{Type: SessionEventThinking, Step: 0})

	// First request carries the user message. The root loop also splices in any
	// async sub-agent results that completed while the session was idle, so a
	// background completion lands on the very next user turn rather than waiting for
	// this turn's first tool round (issue #353).
	firstMessages := []model.Message{{Role: model.RoleUser, Content: initialMessage}}
	if agent.Kind == KindRoot {
		firstMessages = append(firstMessages, s.takeBackgroundResults()...)
	}
	s.compactIfNeeded(sess, emit)
	refreshSystemPrompt()
	resp, err := s.modelRoundTrip(ctx, sess, agent,
		firstMessages,
		tools,
		reasoningSink(0),
	)
	thinkingDone(0)
	if err != nil {
		emit(SessionEvent{Type: SessionEventError, Err: err})
		return responses, err
	}
	responses = append(responses, resp)
	s.emitUsage(emit)

	// lastAssistant retains the most recent non-empty assistant text so a final
	// turn that lands with empty content (e.g. a model that closes with
	// structured_output{final:true} carrying an empty/absent "response", or an
	// empty terminal message) can still surface an answer instead of silently
	// dropping it (#171).
	var lastAssistant string
	// Per-task step cap (issue #249). A positive value bounds the loop; maxSteps
	// <= 0 means UNLIMITED, so the loop relies entirely on its other exit
	// conditions (final answer, budget, cancellation) to terminate.
	maxSteps := s.MaxSteps()

	// advance performs one model round-trip with the same emit/compact/refresh/
	// usage bookkeeping the loop uses after a tool round, updating resp/responses
	// in place. It is shared by the normal tool-result continuation and the
	// preamble continuation nudge (issue #307) so both paths keep byte-identical
	// loop semantics — including SessionEventThinking/ThinkingDone/Usage emission
	// order. stepIdx is the step number the round-trip is announced under.
	advance := func(nextMessages []model.Message, stepIdx int) error {
		emit(SessionEvent{Type: SessionEventThinking, Step: stepIdx})
		s.compactIfNeeded(sess, emit)
		refreshSystemPrompt()
		var rtErr error
		resp, rtErr = s.modelRoundTrip(ctx, sess, agent, nextMessages, tools, reasoningSink(stepIdx))
		thinkingDone(stepIdx)
		if rtErr != nil {
			emit(SessionEvent{Type: SessionEventError, Err: rtErr})
			return rtErr
		}
		responses = append(responses, resp)
		s.emitUsage(emit)
		return nil
	}

	// continuationNudges counts how many preamble continuation nudges have been
	// spliced in the current uninterrupted stretch of tool-free turns (issue
	// #307). It is bounded by maxContinuationNudges and reset to 0 whenever a real
	// tool call happens, so the budget is per stretch — never per task — and a
	// model that genuinely has nothing to do cannot loop. A new user message
	// starts a fresh runLoop, so the counter is implicitly per user message too.
	//
	// Scope notes (the loop is shared, so these hold for every caller):
	//   - Sub-agent loops get the same recovery. runLoop backs both the root task
	//     loop and every sub-agent/interactive loop, each with its own local
	//     continuationNudges, so a narrating sub-agent is recovered and bounded
	//     identically (its events are suppressed, so the nudge is silent there).
	//   - A nudge consumes a step. The continuation round-trip runs under step+1
	//     and the loop's step counter advances on the `continue`, so a nudge counts
	//     against the MaxSteps cap exactly like any other round-trip — intended, so
	//     the per-task bound still governs total cost.
	continuationNudges := 0
	for step := 0; maxSteps <= 0 || step < maxSteps; step++ {
		// Bail out promptly if the loop was stopped or the session closed; the
		// in-flight request (if any) has already been cancelled via ctx.
		if err := ctx.Err(); err != nil {
			emit(SessionEvent{Type: SessionEventError, Err: err})
			return responses, fmt.Errorf("session loop cancelled: %w", err)
		}

		// Stop gracefully before spending another round-trip once the agent has
		// reached its token budget. The most recent response is finalized with a
		// BUDGET_EXCEEDED notice so cost is bounded without crashing the loop or
		// dropping the work done so far (issue #28).
		if agent.BudgetExceeded() {
			resp = stopForBudget(agent, resp)
			break
		}

		calls, explicitFinal := s.collectToolCalls(resp)

		// Part B (issue #390): the turn was cut off by max_tokens
		// (finish_reason "length") and left at least one tool call with truncated
		// (malformed-JSON) arguments. Executing it would feed validateArgs a call
		// the model never finished emitting and surface the validation error as the
		// tool result. Instead give the model one round-trip to resume and re-issue
		// the interrupted call(s) in full. This is the general counterpart to the
		// targeted structured_output salvage in collectToolCalls (Part A): it
		// recovers truncated args for ANY tool. A salvageable terminal
		// structured_output was already folded by collectToolCalls (explicitFinal),
		// so this only fires for unfinished, non-final calls.
		//
		// The truncated assistant turn was already appended to the transcript with
		// its tool_calls (see ModelSession.sendCtx), so the tool-call protocol
		// requires a tool result for every tool_call_id before the next assistant
		// turn — OpenAI and Anthropic both reject an assistant tool_calls message
		// that is not answered one-to-one. So the re-issue instruction is delivered
		// as a tool result per call (not a dangling user note, which would leave the
		// tool_calls unanswered and 400 on a real backend). makeToolResultMessage
		// emits no SessionEvent, so the synthetic results stay out of the visible
		// transcript like the other deliver-only splices.
		//
		// containsTerminalFinal excludes the mixed-batch case where the truncated
		// call was a structured_output that collectToolCalls already salvaged into a
		// terminal final (Part A): that turn is terminal, not unfinished, so it must
		// fold via the serial runner rather than be nudged to continue.
		//
		// It shares the per-stretch continuationNudges budget with the #307
		// preamble nudge and bails before the real-tool-call reset below, so a model
		// that keeps truncating cannot loop: once maxContinuationNudges is hit the
		// loop proceeds to execute the (malformed) call on the normal path. The
		// budget resets the moment a complete tool call lands.
		if !explicitFinal && resp.FinishReason == "length" && hasTruncatedToolCall(resp) &&
			!containsTerminalFinal(calls) && continuationNudges < maxContinuationNudges {
			continuationNudges++
			results := make([]model.Message, 0, len(resp.ToolCalls))
			for _, tc := range resp.ToolCalls {
				results = append(results, makeToolResultMessage(
					tool.ToolCall{Tool: tc.Function.Name, CallID: tc.ID}, truncatedToolCallNote))
			}
			sess.AppendToolResults(results)
			if err := advance(nil, step+1); err != nil {
				return responses, err
			}
			continue
		}

		if len(calls) == 0 {
			// No tool calls. Normally this is the assistant's final answer and the
			// loop ends. But a reasoning model sometimes emits a bare *preamble* — an
			// announcement of intent ("I'll start by…", "Let me…") — as its own turn
			// before it actually calls the tool it just described; treating that as
			// final abandons the task mid-narration (issue #307). When the turn looks
			// like a preamble and the per-stretch nudge budget is not yet spent, splice
			// ONE cheap continuation note (reusing the note-injection path) and give
			// the model another round-trip instead of breaking. The note is
			// deliver-only — like a queued user note it is not emitted as an assistant
			// step, so it stays out of the visible transcript.
			//
			// explicitFinal short-circuits this: a deliberate structured-output final
			// is the strongest terminal signal there is and must end the turn even if
			// its text happens to open like a preamble (issue #307 constraint #2).
			if !explicitFinal && s.shouldNudgeContinuation(resp, continuationNudges) {
				continuationNudges++
				// Retain the preamble text so the empty-final recovery (#171) can still
				// surface it if the post-nudge turn lands empty — the nudge path
				// continues before lastAssistant is set on the tool-call branch below,
				// so without this a preamble→empty-turn sequence would emit an empty
				// final (issue #307).
				if t := strings.TrimSpace(resp.Content); t != "" {
					lastAssistant = t
				}
				// A queued user interjection (the Interject button, issue #201) is NOT
				// drained here: this branch continues before the takePendingNote block
				// below. That is deliberate and lossless — the note is held in its
				// latest-wins slot and delivered at the next turn boundary (the tool
				// round after the model follows through, or the UI's drain-on-idle if the
				// model declares done), one round-trip later at most. Keeping the splice
				// points separate avoids stacking the synthetic nudge and a real user
				// note into the same request.
				note := []model.Message{{Role: model.RoleUser, Content: continuationNudgeNote}}
				if err := advance(note, step+1); err != nil {
					return responses, err
				}
				continue
			}
			// Not a (further) preamble nudge -> the assistant produced its final answer.
			// A salvaged terminal structured_output (explicitFinal) leaves its native
			// tool_calls unanswered in the persisted transcript; balance them so a
			// reused session's next user turn is a valid transcript (issue #390).
			s.finalizeTranscriptToolCalls(sess, resp, nil)
			break
		}
		// A real tool call this turn: reset the continuation-nudge budget so the bound
		// is per uninterrupted stretch of tool-free turns, not per task (issue #307).
		continuationNudges = 0

		// Surface any intermediate reasoning the model emitted alongside its
		// tool calls so the UI can show (foldable) thoughts.
		if thought := strings.TrimSpace(resp.Content); thought != "" {
			lastAssistant = thought
			emit(SessionEvent{Type: SessionEventAssistantStep, Step: step, Text: thought})
		}

		// Execute this turn's tool calls, gathering result messages in call order.
		// Two fast-paths run an independent batch concurrently to cut wall-clock
		// latency; everything else runs serially so side-effecting tools (write,
		// edit, shell, ...) keep their requested order.
		var toolMsgs []model.Message
		finished := false
		switch {
		case allSpawnSubAgent(calls):
			// Several one-shot sub-agent spawns. Sub-agents are independent and
			// every agent-tree read/write is mutex-guarded (children copied under
			// lock via GetSubAgents, the parent link via GetParent), so this is what
			// lets "delegate A, B and C at once" run in parallel. The fan-out is
			// bounded by the shared sub-agent limiter — a spawn that can't grab a
			// global slot runs inline as backpressure (issue #23).
			toolMsgs = s.runToolCallsConcurrent(ctx, agent, agentID, calls, step, emit, s.RunSubAgentsBounded)
		case allReadOnly(agent.ToolRegistry, calls):
			// Several independent read-only/idempotent calls (read, grep, glob,
			// list, calc, web_fetch). They don't mutate the workspace and their
			// ordering doesn't matter, so they run concurrently — bounded by a fixed
			// tool semaphore — and their results are reassembled in call order
			// before being appended (issue #50).
			toolMsgs = s.runToolCallsConcurrent(ctx, agent, agentID, calls, step, emit, runBoundedTools)
		default:
			toolMsgs, finished = s.runToolCallsSerial(ctx, agent, agentID, calls, step, emit, resp)
		}
		if finished {
			// A folded structured_output{final} ends the loop without the normal
			// tool-result append below, leaving this turn's tool_calls (the executed
			// siblings and the terminal call) unanswered in the persisted transcript.
			// Balance them so a reused session's next user turn stays valid (#390).
			s.finalizeTranscriptToolCalls(sess, resp, toolMsgs)
			break
		}

		// Feed results back and ask the model to continue.
		sess.AppendToolResults(toolMsgs)

		// Turn boundary: between this tool round and the next model call, splice in
		// any queued user note as a clarification when mid-turn injection is enabled
		// (issue #170, phase 2). This is the safe splice point — no hard cancel, no
		// lost tool results; the note rides into the same request as the tool
		// results. Only the root agent injects (sub-agents share the slot but run
		// their own focused loops), and only when the experimental flag is on; with
		// it off the UI's drain-on-idle path (phase 1) handles the queue instead.
		var nextMessages []model.Message
		if agent.Kind == KindRoot && s.InjectQueuedInput() {
			if note := s.takePendingNote(); note != "" {
				injected := fmt.Sprintf(injectedNoteTemplate, note)
				nextMessages = []model.Message{{Role: model.RoleUser, Content: injected}}
				// Deliver only — do not re-emit the note as a SessionEventAssistantStep
				// "thought". The injection is the user's clarification, not the model's
				// reasoning, so the UI already shows it as a "You (clarification):" record
				// at Interject-press; re-emitting it here would duplicate it as a model
				// thought (issue #242).
			}
		}
		// Re-inject any async sub-agent results that completed since the last round at
		// this same safe turn boundary (issue #353). The worker only queued the text;
		// the loop — the sole owner of the transcript — appends it here, so the splice
		// never races the worker goroutines. Ordered after any user note so a fresh
		// clarification still leads.
		if agent.Kind == KindRoot {
			nextMessages = append(nextMessages, s.takeBackgroundResults()...)
		}

		if err := advance(nextMessages, step+1); err != nil {
			return responses, err
		}
	}

	if resp != nil {
		finalText := strings.TrimSpace(resp.Content)
		if finalText == "" {
			// The terminal turn carried no text — recover the most recent
			// assistant content so the answer isn't dropped. The TUI renders
			// nothing for an empty final, which presented as the session jumping
			// tool->idle with the last turn missing (#171).
			finalText = lastAssistant
		}
		emit(SessionEvent{Type: SessionEventFinal, Text: finalText})
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
	s.mu.Lock()
	s.compactionCount++
	s.mu.Unlock()
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

// allReadOnly reports whether every call in a turn targets a registered
// read-only tool, so the batch can be executed concurrently without racing on
// the workspace or on tool ordering (issue #50). A single call is left to the
// serial path; an unknown tool (e.g. an MCP tool whose effects we can't
// classify) or any side-effecting tool makes the whole batch ineligible, so it
// runs serially — the conservative choice.
func allReadOnly(reg *tool.ToolRegistry, calls []tool.ToolCall) bool {
	if reg == nil || len(calls) < 2 {
		return false
	}
	for _, c := range calls {
		t := reg.Get(c.Tool)
		if t == nil || !t.ReadOnly {
			return false
		}
	}
	return true
}

// runToolCallsConcurrent executes every call concurrently and returns their
// result messages in call order (so the transcript is independent of which call
// happened to finish first). run bounds the goroutine fan-out — the shared
// sub-agent limiter for spawn batches, a fixed tool semaphore for read-only
// batches. A panic in any one call is contained and turned into an error result
// for that slot, so the batch still yields a complete, ordered message set
// (issue #8).
func (s *UserSession) runToolCallsConcurrent(ctx context.Context, agent *Agent, agentID string, calls []tool.ToolCall, step int, emit func(SessionEvent), run func([]func())) []model.Message {
	toolMsgs := make([]model.Message, len(calls))
	tasks := make([]func(), len(calls))
	for i, call := range calls {
		i, call := i, call
		// Announce every call up-front (in this goroutine, before any task runs)
		// so the UI shows the whole batch as running immediately. Each call's
		// terminal result is paired back to it by id (issue #187).
		id := toolEventID(call, step, i)
		emitToolCall(emit, call, step, id)
		tasks[i] = func() {
			toolMsgs[i] = s.runAndEmitResult(ctx, agent, agentID, call, step, id, emit)
		}
	}
	run(tasks)
	return toolMsgs
}

// toolEventID returns the stable id used to pair a call's ToolCall event with
// its ToolResult event (issue #187). It prefers the model-supplied tool-call id
// (native tool-calling) and otherwise synthesizes a turn-unique id from the tool
// name, step and the call's index in its batch — so the fallback JSON path
// (which carries no CallID) and repeated uses of the same tool in one turn still
// pair one to one rather than colliding.
func toolEventID(call tool.ToolCall, step, idx int) string {
	if call.CallID != "" {
		return call.CallID
	}
	return fmt.Sprintf("%s#%d.%d", call.Tool, step, idx)
}

// emitToolCall announces that a tool call is starting, carrying id so its later
// ToolResult event can be matched back to it (issue #187).
func emitToolCall(emit func(SessionEvent), call tool.ToolCall, step int, id string) {
	emit(SessionEvent{Type: SessionEventToolCall, Step: step, Tool: call.Tool, Args: call.Args, CallID: id})
}

// runAndEmitResult executes a single already-announced tool call and emits its
// terminal ToolResult event (carrying id), returning the message to feed back to
// the model. A panic in the tool is contained and turned into a terminal error
// result, so every call announced by emitToolCall always reaches a terminal
// state — the started tool can never be left "running" (issue #187, building on
// the loop-wide panic guard of issue #8). It is the single execute-and-report
// path shared by the serial and concurrent runners.
func (s *UserSession) runAndEmitResult(ctx context.Context, agent *Agent, agentID string, call tool.ToolCall, step int, id string, emit func(SessionEvent)) (msg model.Message) {
	defer func() {
		if r := recover(); r != nil {
			resultStr := fmt.Sprintf("error: tool %q panicked: %v", call.Tool, r)
			emit(SessionEvent{Type: SessionEventToolResult, Step: step, Tool: call.Tool, Args: call.Args, Result: resultStr, CallID: id})
			msg = makeToolResultMessage(call, resultStr)
		}
	}()
	resultStr := s.runToolCall(ctx, agent, agentID, call)
	emit(SessionEvent{Type: SessionEventToolResult, Step: step, Tool: call.Tool, Args: call.Args, Result: resultStr, CallID: id})
	return makeToolResultMessage(call, resultStr)
}

// runToolCallsSerial executes a turn's calls one at a time, in order. It is the
// path for any batch that is not safe to parallelize (writes, shell, mixed or
// unknown tools). A terminal structured_output{final:true} call folds its text
// into resp and stops the loop, reported via the returned finished flag.
func (s *UserSession) runToolCallsSerial(ctx context.Context, agent *Agent, agentID string, calls []tool.ToolCall, step int, emit func(SessionEvent), resp *model.CompletionResponse) (toolMsgs []model.Message, finished bool) {
	for idx, call := range calls {
		if call.Tool == "structured_output" {
			// Terminal tool: fold its response into the assistant content. No
			// ToolCall event is emitted for it, so there is nothing to leave
			// "running" when the loop breaks here.
			if final, _ := call.Args["final"].(bool); final {
				if text, ok := call.Args["response"].(string); ok && text != "" {
					resp.Content = text
				}
				return toolMsgs, true
			}
		}

		// Announce, then execute via the shared panic-safe path so a tool that
		// panics still emits a terminal result instead of unwinding to the
		// loop-wide recover and leaving this entry "running" (issue #187).
		id := toolEventID(call, step, idx)
		emitToolCall(emit, call, step, id)
		toolMsgs = append(toolMsgs, s.runAndEmitResult(ctx, agent, agentID, call, step, id, emit))
	}
	return toolMsgs, false
}

// collectToolCalls returns the tool calls for a response, preferring native
// tool_calls and falling back to a JSON object embedded in the text.
//
// explicitFinal reports that the turn was an EXPLICIT structured final — a
// JSON-text {"response": ..., "final": true} object — whose text has been folded
// into resp.Content (and no calls returned). The caller uses it to distinguish
// "no calls because the model deliberately ended" from "no calls because the
// model only narrated": the former must terminate immediately and must NOT be
// given a preamble continuation nudge, even when its response text happens to
// open like a preamble (issue #307 constraint #2). The native structured_output
// path needs no such signal — it returns a real call the serial runner finalizes.
func (s *UserSession) collectToolCalls(resp *model.CompletionResponse) (calls []tool.ToolCall, explicitFinal bool) {
	if resp == nil {
		return nil, false
	}

	// Native tool calls.
	if len(resp.ToolCalls) > 0 {
		calls := make([]tool.ToolCall, 0, len(resp.ToolCalls))
		for _, tc := range resp.ToolCalls {
			args := map[string]interface{}{}
			var parseErr error
			if tc.Function.Arguments != "" {
				parseErr = json.Unmarshal([]byte(tc.Function.Arguments), &args)
			}

			// Part A salvage (issue #390): a terminal structured_output call whose
			// streamed args were cut off by max_tokens (finish_reason "length")
			// arrives with incomplete JSON — it either fails to parse or parses to
			// an object missing the required "response". Left alone it falls through
			// to validateArgs and the user sees
			// `invalid args: missing required property "response"` in place of the
			// answer the sub-agent actually produced. When the partial args still
			// carry a "final": true marker, recover whatever partial "response" text
			// was emitted (an empty recovery defers to the loop's lastAssistant
			// fallback, #171) and treat the call as a best-effort terminal final.
			if tc.Function.Name == "structured_output" && resp.FinishReason == "length" {
				respText, _ := args["response"].(string)
				if parseErr != nil || respText == "" {
					if recovered, isFinal := recoverTruncatedFinal(tc.Function.Arguments); isFinal {
						// Sole call: nothing else to run this turn, so fold the
						// recovered final straight into resp.Content and end the turn
						// via explicitFinal — mirroring the text-embedded salvage below.
						if len(resp.ToolCalls) == 1 {
							if recovered != "" {
								resp.Content = recovered
							}
							return nil, true
						}
						// Mixed batch: rebuild the terminal call's args from the
						// recovery and return it alongside the earlier calls, so the
						// serial runner executes those first and then folds the final —
						// mirroring the well-formed structured_output{final} path rather
						// than short-circuiting and dropping the preceding calls.
						args["final"] = true
						if recovered != "" {
							args["response"] = recovered
						}
					}
				}
			}

			calls = append(calls, tool.ToolCall{
				Tool:   tc.Function.Name,
				Args:   args,
				CallID: tc.ID,
			})
		}
		return calls, false
	}

	// Fallback: one or more JSON objects embedded in the assistant text. Small
	// or local models without native tool-calling emit calls as JSON, often
	// prose-wrapped, fenced in ```json, pretty-printed, key-reordered, or several
	// at once — so we scan for every balanced object (issue #32) rather than
	// substring-matching a single exact shape.
	responseText := strings.TrimSpace(resp.Content)
	for _, obj := range tool.ExtractJSONObjects(responseText) {
		// A {"response": ..., "final": true} object is the structured final
		// answer: stop and surface its text instead of acting on any calls.
		var structuredOutput struct {
			Response string `json:"response"`
			Final    bool   `json:"final"`
		}
		if err := json.Unmarshal([]byte(obj), &structuredOutput); err == nil && structuredOutput.Final {
			resp.Content = structuredOutput.Response
			return nil, true
		}
		var parsed tool.ToolCall
		if err := json.Unmarshal([]byte(obj), &parsed); err == nil && parsed.Tool != "" {
			calls = append(calls, parsed)
		}
	}
	return calls, false
}

// hasTruncatedToolCall reports whether any of a turn's native tool calls were cut
// off mid-arguments by max_tokens (issue #390). It prefers the deterministic
// Truncated flag the streaming parsers set, falling back to a direct JSON-validity
// check so the signal still holds for any backend that does not set the flag. A
// no-argument call (empty Arguments) is complete, not truncated.
func hasTruncatedToolCall(resp *model.CompletionResponse) bool {
	if resp == nil {
		return false
	}
	for _, tc := range resp.ToolCalls {
		if tc.Truncated {
			return true
		}
		args := strings.TrimSpace(tc.Function.Arguments)
		if args != "" && !json.Valid([]byte(args)) {
			return true
		}
	}
	return false
}

// containsTerminalFinal reports whether a turn's calls include a terminal
// structured_output{final:true} — a deliberate (or salvaged, issue #390) final
// answer that the serial runner folds to end the loop. The truncated-tool
// continuation (Part B) uses it to leave such a turn alone: it is terminal, not an
// unfinished call to be re-issued.
func containsTerminalFinal(calls []tool.ToolCall) bool {
	for _, c := range calls {
		if c.Tool == "structured_output" {
			if final, _ := c.Args["final"].(bool); final {
				return true
			}
		}
	}
	return false
}

// finalizeTranscriptToolCalls keeps the persisted transcript valid when the loop
// ends on a turn that carried native tool calls. The terminal assistant turn was
// already appended with its tool_calls (ModelSession.sendCtx), and a folded
// structured_output{final} — whether well-formed or salvaged from truncated args
// (issue #390) — ends the loop without the normal tool-result append, leaving
// those tool_calls unanswered. OpenAI and Anthropic both reject an assistant
// tool_calls message that is not answered one-to-one on the next request, so any
// reused session would fail on its next user turn. This appends the results that
// were already produced (executed siblings) plus a synthetic result for every
// still-unanswered call id, so the transcript stays a balanced
// assistant-tool_calls -> tool-results sequence. makeToolResultMessage emits no
// SessionEvent, so the synthetic results stay out of the visible transcript.
func (s *UserSession) finalizeTranscriptToolCalls(sess *model.ModelSession, resp *model.CompletionResponse, executed []model.Message) {
	if sess == nil || resp == nil || len(resp.ToolCalls) == 0 {
		return
	}
	answered := make(map[string]bool, len(executed))
	for _, m := range executed {
		if m.ToolCallID != "" {
			answered[m.ToolCallID] = true
		}
	}
	results := append([]model.Message(nil), executed...)
	for _, tc := range resp.ToolCalls {
		if tc.ID == "" || answered[tc.ID] {
			continue
		}
		results = append(results, makeToolResultMessage(
			tool.ToolCall{Tool: tc.Function.Name, CallID: tc.ID}, finalToolCallResultNote))
		answered[tc.ID] = true
	}
	if len(results) > 0 {
		sess.AppendToolResults(results)
	}
}

// recoverTruncatedFinal performs a best-effort, dependency-free lenient parse of
// a truncated structured_output arguments string (cut off mid-JSON by
// max_tokens, issue #390). It reports whether the partial JSON still carries a
// "final": true marker and returns whatever partial "response" string value was
// emitted before the cut (empty when truncation landed before the value began,
// in which case the caller falls back to the assistant's preceding text, #171).
func recoverTruncatedFinal(raw string) (response string, isFinal bool) {
	return partialStringValue(raw, "response"), hasFinalTrue(raw)
}

// hasFinalTrue reports whether the partial args carry a `"final": true` marker,
// tolerating arbitrary whitespace between the key, colon and value. It only has
// to recognise the marker the model already emitted before truncation, so a
// plain scan (no JSON parse, which the truncated bytes would fail) suffices.
func hasFinalTrue(raw string) bool {
	idx := strings.Index(raw, `"final"`)
	if idx < 0 {
		return false
	}
	rest := strings.TrimSpace(raw[idx+len(`"final"`):])
	rest = strings.TrimPrefix(rest, ":")
	rest = strings.TrimSpace(rest)
	return strings.HasPrefix(rest, "true")
}

// partialStringValue extracts the (possibly truncated) string value of a JSON
// key from an incomplete object. It locates `"key"`, steps past the colon to the
// opening quote, then reads to the matching unescaped closing quote — or, when
// the stream was cut before that quote arrived, takes the remainder as the
// partial value. The captured span (re-closed if needed) is decoded with the
// standard JSON unescaper so escapes the model emitted survive; an undecodable
// fragment (e.g. a dangling \u escape) yields "" so the caller falls back to the
// preceding assistant text rather than surfacing mojibake (issue #390).
func partialStringValue(raw, key string) string {
	marker := `"` + key + `"`
	idx := strings.Index(raw, marker)
	if idx < 0 {
		return ""
	}
	rest := strings.TrimSpace(raw[idx+len(marker):])
	rest = strings.TrimPrefix(rest, ":")
	rest = strings.TrimSpace(rest)
	if !strings.HasPrefix(rest, `"`) {
		return ""
	}
	// Find the closing quote, skipping escaped characters. end < 0 means the
	// value was truncated before its closing quote arrived.
	end := -1
	for i := 1; i < len(rest); i++ {
		if rest[i] == '\\' {
			i++ // skip the escaped char
			continue
		}
		if rest[i] == '"' {
			end = i
			break
		}
	}
	var quoted string
	if end >= 0 {
		quoted = rest[:end+1]
	} else {
		// Truncated mid-value: drop a dangling backslash (an incomplete escape)
		// then synthesize the closing quote so the span is a parseable JSON string.
		quoted = strings.TrimRight(rest, `\`) + `"`
	}
	var out string
	if err := json.Unmarshal([]byte(quoted), &out); err != nil {
		return ""
	}
	return out
}

// shouldNudgeContinuation reports whether a tool-free turn should be given one
// more round-trip with a continuation note rather than being accepted as the
// final answer (issue #307). It is the gate in front of looksLikePreamble: it
// also enforces the per-stretch bound (alreadyNudged < maxContinuationNudges)
// and rejects a nil response, so a model that keeps narrating cannot loop —
// once the bound is hit the loop falls back to its normal final-answer break.
func (s *UserSession) shouldNudgeContinuation(resp *model.CompletionResponse, alreadyNudged int) bool {
	if resp == nil || alreadyNudged >= maxContinuationNudges {
		return false
	}
	return looksLikePreamble(resp.Content)
}

// looksLikePreamble reports whether a tool-free assistant turn reads as a bare
// *announcement of intent* — a model narrating the step it is about to take
// ("I'll start by…", "Let me…", "First, …") — rather than a genuine final
// answer (issue #307).
//
// It is deliberately NARROW: a turn must clear THREE bars to qualify, and
// anything ambiguous is treated as final (returns false). A false "continue"
// (looping on a real answer) is worse than an occasional false "stop" (the
// historical behaviour), so the test errs toward stopping:
//
//  1. SHORT. A real final answer — a summary, an explanation, a code block —
//     runs long; a preamble is a sentence or two. Longer than maxPreambleLen is
//     final.
//  2. NO completion/terminal marker (a code fence, "done", "in summary", "here
//     is", "the answer is", a past-tense "I have/I've", …). Such a marker means
//     the model is presenting a result, not announcing a next step.
//  3. OPENS with a recognised intent phrase (preamblePrefixes). This is the
//     decisive signal: without an explicit announcement of a NEXT action the
//     turn is treated as final. Matching the prefix (not just containment) means
//     the text must literally start with the announcement to count.
func looksLikePreamble(content string) bool {
	text := strings.TrimSpace(content)
	// An empty turn is not a preamble; the loop's empty-final recovery (#171)
	// handles it, and nudging a silent model risks a wasted round-trip.
	if text == "" || len(text) > maxPreambleLen {
		return false
	}
	lower := strings.ToLower(text)
	for _, marker := range completionMarkers {
		if strings.Contains(lower, marker) {
			return false
		}
	}
	for _, p := range preamblePrefixes {
		if strings.HasPrefix(lower, p) {
			return true
		}
	}
	return false
}

// runToolCall executes a single tool call and returns a textual result. ctx is
// the cancellation scope of the running loop; it is passed to the tool so a
// tool that itself runs a model loop (spawn_subagent) inherits cancellation.
func (s *UserSession) runToolCall(ctx context.Context, agent *Agent, agentID string, call tool.ToolCall) string {
	toolCtx := tool.ToolContext{
		SessionID:    s.ID,
		AgentID:      agentID,
		ToolCallID:   call.CallID,
		ToolCallback: s.ToolCallback,
		Context:      ctx,
	}
	toolResp, err := agent.ToolRegistry.ExecuteToolCall(&call, toolCtx)
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

// toolDefsFromRegistry converts the agent's tool registry into native tool defs,
// advertising only the currently enabled tools (a disabled tool is hidden from
// the model so it neither sees nor attempts to call it).
//
// Strictness is per tool: FunctionDef.Strict mirrors the tool's opt-in
// Tool.Strict flag (issue #359). Read-only tools with simple closed schemas
// (read, glob, list, calc, git, grep, verify, diagnostics) opt in, eliminating
// type-coercion errors and validate-and-retry rounds. spawn_subagent — and any
// tool whose schema uses a union type or otherwise needs parallel-tool-call
// freedom — stays non-strict: a strict tool forces parallel_tool_calls:false on
// OpenAI-compatible providers (see model.parallelToolCallsMustBeDisabled), which
// would suppress a batched-spawn turn where the model emits several
// spawn_subagent calls, or one spawn_subagent carrying a "subtasks" batch, to
// run concurrently (issue #282).
func toolDefsFromRegistry(reg *tool.ToolRegistry) []model.ToolDef {
	if reg == nil {
		return nil
	}
	tools := reg.ListEnabled()
	defs := make([]model.ToolDef, 0, len(tools))
	for _, t := range tools {
		defs = append(defs, model.ToolDef{
			Type: "function",
			Function: model.FunctionDef{
				Name:        t.Name,
				Description: t.Description,
				Parameters:  t.InputSchema,
				Strict:      t.Strict,
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
// top-level agent and (when recursion is enabled) by sub-agents. There are three
// shapes: both styles available (the default, issue #284), interactive only, and
// one-shot only.
func coordinatorInstructions(cfg config.SubAgentConfig) string {
	switch {
	case cfg.ExposesOneShotTools() && cfg.ExposesInteractiveTools():
		return coordinatorInstructionsBoth
	case cfg.ExposesInteractiveTools():
		return coordinatorInstructionsInteractive
	default:
		return coordinatorInstructionsOneShot
	}
}

// coordinatorInstructionsOneShot describes blocking spawn_subagent delegation.
const coordinatorInstructionsOneShot = `

## Delegating work (one-shot sub-agents)
Sub-agents are your tool for cutting wall-clock latency: a single spawn_subagent
call runs every entry of its "subtasks" array CONCURRENTLY and blocks only until
the slowest finishes. Make delegation your default whenever a turn has TWO OR MORE
independent lookups — reserve doing the work inline for trivial single-step
actions (one read, one grep). Typical triggers:
  - investigating several modules/files at once,
  - running diagnostics + verify + grep together to validate a change,
  - researching a topic (or auditing subsystems) while you keep editing.
ALWAYS batch the independent parts into ONE spawn_subagent call's "subtasks"
array — one call, not one call per part. Emitting separate spawns across turns
runs them one at a time with no speed-up, so never do that. For example, to probe
three modules in parallel instead of reading them one after another, in ONE call:
  {"tool":"spawn_subagent","args":{"subtasks":[
    {"name":"agent","task":"Map internal/agent: key types and how the loop runs"},
    {"name":"gogent","task":"Map internal/gogent: tool registry and spawn flow"},
    {"name":"verify","task":"Run diagnostics and the agent tests; report failures"}
  ]}}
Each sub-agent runs to completion and returns a result starting with "SUCCESS: "
or "FAILURE: ". Use a single "name"/"task" pair only for a lone subtask; always
batch two or more independent subtasks into the one call's "subtasks" array.
Add "async": true to the call when you would rather keep working than wait: it
returns a pending handle per subtask immediately (no blocking) and re-injects each
result into the conversation when that sub-agent finishes. Use it to start
background work and carry on; omit it (the default) when you need every result in
hand before continuing.`

// coordinatorInstructionsInteractive describes asynchronous fire-and-forget
// delegation with a concrete launch → continue → wait_agent_event → react recipe.
const coordinatorInstructionsInteractive = `

## Delegating work (interactive, fire-and-forget sub-agents)
Launch asynchronous workers that run while you keep working, then harvest their
results when they land:
- launch_agent {name, task} starts a worker and returns its agent_id IMMEDIATELY —
  you are NOT blocked, so keep editing/reasoning after it returns.
- agent_status {agent_id} reports running/waiting/completed/failed and any result.
- wait_agent_event blocks until SOME sub-agent finishes or asks a question, and
  returns that event (its agent_id, status, and result/question).
- agent_send {agent_id, message} answers a sub-agent's CLARIFY question. Only use
  it once the agent has asked (it must be 'waiting'); send it in response to a
  clarify event from wait_agent_event.
- agent_terminate {agent_id} stops a sub-agent you no longer need.
Concrete recipe for "research X in the background while I work on Y":
  1. launch_agent {name:"research", task:"<X>"}  -> get agent_id, DON'T wait.
  2. Continue making progress on Y (read/edit/grep) for as long as you usefully can.
  3. When you need the findings (or have nothing else to do), call wait_agent_event.
  4. React to the returned event: if it's CLARIFY, answer with agent_send and loop
     back to step 2; if it completed, fold the result into your work.
  5. Repeat wait_agent_event until every launched agent has completed.
Only call wait_agent_event while at least one agent is still running; once they
have all finished (or are only waiting on a CLARIFY) it just times out. A CLARIFY
agent stays parked and holds a slot until you answer it (agent_send) or
agent_terminate it, so don't leave one hanging.
Launch several investigations up front, keep working, and collect results as they
finish — that is the latency win over blocking.`

// coordinatorInstructionsBoth is shown when BOTH styles are available (the
// default, issue #284). It teaches the choice — block when you must wait on
// batched work, fire-and-forget when you can keep working — then gives the recipe
// for each.
const coordinatorInstructionsBoth = `

## Delegating work (two styles available — pick by whether you'll wait)
You have BOTH blocking and fire-and-forget delegation. Make delegation your default
whenever a turn has two or more independent lookups; both styles cut wall-clock
latency by running work concurrently. Choose the style by what you will do next:
- BLOCKING (spawn_subagent): use when you need ALL results before you can continue
  — batched independent work you will wait on (investigating several modules at
  once, or running diagnostics + verify + grep together to validate a change). ONE
  spawn_subagent call runs every entry of its "subtasks" array CONCURRENTLY and
  blocks only until the slowest finishes:
    {"tool":"spawn_subagent","args":{"subtasks":[
      {"name":"modules","task":"Map internal/agent and internal/gogent: key types and flow"},
      {"name":"verify","task":"Run diagnostics and the agent tests; report failures"}
    ]}}
  Always batch the independent parts into ONE call (never one spawn per part across
  turns). Each child returns "SUCCESS: " or "FAILURE: ".
  Add "async": true to that SAME call when you can keep working instead of waiting:
  it returns a pending handle per subtask IMMEDIATELY (it does NOT block), and each
  result is re-injected into the conversation when that sub-agent finishes. Reach for
  it to start background work (research, a long check) and carry on with your main
  task — the result comes back on its own, with no polling or waiting needed.
- FIRE-AND-FORGET (launch_agent family): use when you want to kick work off and KEEP
  WORKING — e.g. background research while you refactor. Recipe:
    1. launch_agent {name, task} -> returns an agent_id IMMEDIATELY; you are NOT blocked.
    2. Keep editing/reasoning on your main task.
    3. When you need the findings, call wait_agent_event (blocks until some agent
       finishes or asks a question) and react: answer a CLARIFY with agent_send,
       fold a completed result into your work, agent_terminate one you no longer need.
    4. Repeat wait_agent_event until every launched agent has completed. Only wait
       while an agent is still running — waiting with nothing outstanding (or only a
       parked CLARIFY) just times out. Answer or agent_terminate a CLARIFY agent
       promptly; it holds a slot until you do.
    agent_status {agent_id} reports a single worker's state on demand.
Rule of thumb: "I need everything now" -> spawn_subagent; "start this and let me keep
going" -> launch_agent then wait_agent_event later.`

// recursionInstructions is appended to a sub-agent's prompt when it is itself
// permitted to spawn sub-agents.
func recursionInstructions(cfg config.SubAgentConfig) string {
	return "\n\nYou are permitted to spawn your own sub-agents to break this task" +
		" down further. Do so sparingly, only for genuinely independent subtasks." +
		coordinatorInstructions(cfg)
}

// ExecuteTaskLoopWithModel runs the multi-turn task loop with a specific model config
func (s *UserSession) ExecuteTaskLoopWithModel(ctx context.Context, agentID, message string, modelConfig *config.ModelConfig) ([]*model.CompletionResponse, error) {
	// Call the regular ExecuteTaskLoop
	return s.ExecuteTaskLoop(ctx, agentID, message)
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

// subAgentOneShotPrompt is a deliberately lean prompt for one-shot sub-agents
// (issue #283): it leads with the SUCCESS/FAILURE task contract and skips broad
// persona framing, since the child is scoped to a single subtask. It still
// receives the full systemContext (repo map, AGENTS.md, git status, skills,
// todos), which runLoop appends every turn — only the persona is trimmed. The
// path-trust line (G3) tells the child to act on paths it was handed rather than
// re-grepping to rediscover them.
const subAgentOneShotPrompt = `Complete the single delegated subtask below using the available tools, then stop.
Reply with a final plain-text answer that STARTS with either:
  "SUCCESS: " followed by the result, or
  "FAILURE: " followed by the reason you could not complete it.
Trust any file paths given in the task (or in the primed context) as authoritative — read them directly; do not grep or list to rediscover paths you were already given.
Do not ask questions; make reasonable assumptions and proceed. Be concise.`

const subAgentInteractivePrompt = `You are a sub-agent working on a delegated subtask. Use the available tools to complete it.
When finished, reply with a final plain-text answer that STARTS with either:
  "SUCCESS: " followed by the result, or
  "FAILURE: " followed by the reason you could not complete it.
If — and only if — you are genuinely blocked and need the coordinator to decide something, instead reply STARTING with
  "CLARIFY: " followed by your specific question.
Trust any file paths given in the task (or in the primed context) as authoritative — read them directly; do not grep or list to rediscover paths you were already given.
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

// Bounds for the sub-agent context primer (issue #283). The primer re-uses the
// parent's already-gathered discovery so the child does not re-read/re-grep from
// scratch; it must stay small so it never becomes a transcript dump.
const (
	maxPrimerPaths    = 20
	maxPrimerSearches = 8
	maxPrimerBytes    = 1500
)

// primerPathTools are the tools whose calls reveal a workspace path the parent
// already inspected; primerSearchTools are those that reveal a search it ran.
var (
	primerPathTools   = map[string]bool{"read": true, "edit": true, "write": true, "list": true}
	primerSearchTools = map[string]bool{"grep": true, "glob": true}
)

// subAgentPrimer builds a bounded context primer from what the parent agent has
// already discovered — the file paths it read/edited/listed and the searches it
// ran — so a freshly spawned child can act on them instead of re-deriving them
// (issue #283, G1). It carries only references (paths and search descriptors),
// never file contents or the parent's reasoning, so it stays small and does not
// defeat the point of the child's fresh context. The repo map is already
// injected via systemContext, so it is not duplicated here. Returns "" when the
// parent has gathered nothing worth priming.
func subAgentPrimer(parent *Agent) string {
	if parent == nil || parent.ThoughtTrain == nil {
		return ""
	}
	var paths, searches []string
	seenPath := map[string]bool{}
	seenSearch := map[string]bool{}
	for _, msg := range parent.ThoughtTrain.GetTranscript() {
		for _, call := range msg.ToolCalls {
			name := call.Function.Name
			isPath, isSearch := primerPathTools[name], primerSearchTools[name]
			if !isPath && !isSearch {
				continue
			}
			var args map[string]interface{}
			if json.Unmarshal([]byte(call.Function.Arguments), &args) != nil {
				continue
			}
			if isPath {
				if p := strings.TrimSpace(stringField(args, "path")); p != "" && !seenPath[p] && len(paths) < maxPrimerPaths {
					seenPath[p] = true
					paths = append(paths, p)
				}
				continue
			}
			if d := describePrimerSearch(name, args); d != "" && !seenSearch[d] && len(searches) < maxPrimerSearches {
				seenSearch[d] = true
				searches = append(searches, d)
			}
		}
	}
	if len(paths) == 0 && len(searches) == 0 {
		return ""
	}

	var b strings.Builder
	b.WriteString("Context already gathered by the delegating agent (authoritative — do not re-discover):")
	if len(paths) > 0 {
		b.WriteString("\nFiles/paths already inspected:")
		for _, p := range paths {
			b.WriteString("\n- " + p)
		}
	}
	if len(searches) > 0 {
		b.WriteString("\nSearches already run:")
		for _, d := range searches {
			b.WriteString("\n- " + d)
		}
	}
	b.WriteString("\nAct on these directly; read a listed file only if you need its contents, and do not grep/list to rediscover paths you were already given.")
	return truncatePrimer(b.String(), maxPrimerBytes)
}

// describePrimerSearch renders a grep/glob call as a one-line descriptor for the
// primer, or "" if the call lacks a usable pattern.
func describePrimerSearch(name string, args map[string]interface{}) string {
	pat := strings.TrimSpace(stringField(args, "pattern"))
	if pat == "" {
		return ""
	}
	switch name {
	case "grep":
		if p := strings.TrimSpace(stringField(args, "path")); p != "" {
			return fmt.Sprintf("grep %q in %s", pat, p)
		}
		return fmt.Sprintf("grep %q", pat)
	case "glob":
		return fmt.Sprintf("glob %q", pat)
	}
	return ""
}

// stringField returns args[key] as a string, or "" if missing or not a string.
func stringField(args map[string]interface{}, key string) string {
	s, _ := args[key].(string)
	return s
}

// truncatePrimer caps the primer at max bytes, cutting on a line boundary so a
// path is never sliced in half and appending a truncation marker. For any
// realistic budget (max >= the marker length, always true for the sole caller's
// maxPrimerBytes) the result is at most max bytes; for a degenerate tiny max the
// budget floors at 0 and the marker alone is returned.
func truncatePrimer(s string, max int) string {
	if len(s) <= max {
		return s
	}
	const suffix = "\n- … (truncated)"
	budget := max - len(suffix)
	if budget < 0 {
		budget = 0
	}
	cut := s[:budget]
	if i := strings.LastIndexByte(cut, '\n'); i > 0 {
		cut = cut[:i]
	}
	return cut + suffix
}

// SeededMessage prepends the agent's context primer (SeedContext) to its task
// message, producing the first user message for a sub-agent's loop. With no
// primer it returns task unchanged (issue #283).
func (a *Agent) SeededMessage(task string) string {
	if a == nil {
		return task
	}
	// SeedContext is set once in newSubAgent before the child is published (and
	// before runInteractive's goroutine is started), then never mutated — so it
	// is read here without the agent lock, matching how its sibling construction
	// fields are accessed. Do not write SeedContext after the agent is published.
	seed := a.SeedContext
	if strings.TrimSpace(seed) == "" {
		return task
	}
	return seed + "\n\n" + task
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

	// Cap how many sub-agents a single parent may run AT ONCE. Only non-terminal
	// children count: completed/failed helpers stay in the tree (the UI shows them)
	// but free their slot, so a long session does not exhaust the budget as finished
	// sub-agents accumulate (issue #280).
	if parent.ActiveSubAgentCount() >= cfg.MaxSubAgentsOrDefault() {
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
	// Pre-seed the child with a bounded primer of what the parent already
	// discovered, so a small delegated task does not pay a re-discovery tax
	// (issue #283). Built before the child is published; SeededMessage folds it
	// into the child's first user message.
	child.SeedContext = subAgentPrimer(parent)
	child.Status = StatusRunning
	if timeoutMs > 0 {
		child.TimeoutMs = timeoutMs
	}
	// Give the child a per-agent token budget so a sub-agent (or a recursive
	// fan-out of them) cannot loop to the step cap with no token ceiling. Zero
	// leaves it unbounded, preserving prior behavior (issue #28).
	child.TokenBudget = cfg.TokenBudget

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
// starting with FAILURE: is a failure, as is one stopped at its token budget
// (BUDGET_EXCEEDED), since the task did not run to completion; anything else is
// treated as completed.
func subAgentOutcome(final string) AgentStatus {
	up := strings.TrimSpace(strings.ToUpper(final))
	if strings.HasPrefix(up, "FAILURE") || strings.HasPrefix(up, budgetExceededMarker) {
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
func (s *UserSession) SpawnSubAgent(ctx context.Context, parentAgentID, name, task string, oneShot bool) (string, error) {
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

	responses, runErr := s.runLoop(ctx, child, child.ID, child.SeededMessage(task), s.subAgentPrompt(oneShot))

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

// --- Asynchronous (fire-and-forget, one-shot) sub-agents -----------------------
//
// These back spawn_subagent{async:true} (issue #353). They reuse the interactive
// engine's primitives — a background goroutine, a session-scoped context, the
// shared SubAgentLimiter (acquire-or-reject), and the agent tree — but run the
// child as a plain ONE-SHOT loop (no CLARIFY, no inbox): the tool returns a
// pending handle immediately and the result is re-injected into the root loop's
// transcript when the worker finishes, instead of the coordinator manually driving
// launch_agent → wait_agent_event.

// BackgroundAgent tracks one asynchronous one-shot sub-agent. It is the one-shot
// analogue of InteractiveAgent: no inbox, since a background worker never pauses
// for a CLARIFY — it runs to completion and its result is re-injected.
type BackgroundAgent struct {
	ID      string
	Name    string
	agent   *Agent
	done    chan struct{}    // closed to request termination
	once    sync.Once        // guards a single close(done)
	limiter *SubAgentLimiter // the concurrency slot acquired at launch, released once when the worker exits
}

// LaunchBackgroundSubAgent starts an asynchronous one-shot sub-agent and returns
// its agent_id immediately. The worker runs concurrently on its own goroutine; its
// result is re-injected into the root loop's transcript when it completes (see
// runBackground / takeBackgroundResults), so the coordinator keeps working instead
// of blocking on the child.
//
// Like LaunchInteractiveAgent it counts against the shared SubAgentLimiter and must
// stay non-blocking, so the slot is acquired-or-the-launch-is-rejected (rather than
// running inline): the coordinator can retry once a slot frees. The slot is released
// in runBackground when the worker exits, on every terminal path.
func (s *UserSession) LaunchBackgroundSubAgent(parentAgentID, name, task string) (string, error) {
	s.mu.RLock()
	limiter := s.subAgentLimiter
	s.mu.RUnlock()
	if !limiter.tryAcquire() {
		return "", fmt.Errorf("sub-agent concurrency limit reached: wait for a running sub-agent to finish before spawning another")
	}

	child, err := s.newSubAgent(parentAgentID, name, task, KindTool)
	if err != nil {
		limiter.release() // never started the worker; give the slot back
		return "", err
	}

	ba := &BackgroundAgent{
		ID:      child.ID,
		Name:    child.DisplayName(),
		agent:   child,
		done:    make(chan struct{}),
		limiter: limiter, // release exactly this acquired slot when the worker exits
	}
	first := s.addBackground(ba)

	child.SetStatus(StatusRunning)
	s.emitSubAgent(child, "launched (background): "+task, nil)
	// Announce the 0→1 edge so the session shows "working in background" even after
	// the main loop's turn ends (issue #353).
	if first {
		s.emit(SessionEvent{Type: SessionEventBackground, Background: true})
	}

	go s.runBackground(ba, task)
	return child.ID, nil
}

// runBackground drives an asynchronous one-shot sub-agent to completion on its own
// goroutine and queues its result for re-injection into the root transcript.
func (s *UserSession) runBackground(ba *BackgroundAgent, task string) {
	// Wind the worker down on every terminal path (completion, failure, cancellation,
	// panic). Defers run LIFO, so register removeBackground FIRST and limiter.release
	// SECOND: the slot is released before removeBackground emits the 1→0
	// SessionEventBackground edge, so by the time the session reads "no background
	// work" the concurrency slot is already free for the next spawn. removeBackground
	// also drops the agent from the background set (HasBackgroundWork → false when this
	// was the last worker).
	defer s.removeBackground(ba.ID)
	// Release the exact limiter captured at acquire time so exactly one release pairs
	// with the one acquire (never a re-read of s.subAgentLimiter).
	defer ba.limiter.release()

	// This runs on its own goroutine, so a panic here would crash the whole process.
	// Contain it and finish the agent as failed instead (issue #8).
	defer func() {
		if r := recover(); r != nil {
			s.finishBackground(ba, StatusFailed, fmt.Sprintf("panic: %v", r))
		}
	}()

	// Background workers are asynchronous and outlive any single turn, so their loop
	// is scoped to the session (background) rather than a turn context. Terminating
	// the agent (done closed by Stop / TerminateInteractiveAgent-style close, or
	// agent.Cancel via StopAgent) cancels its in-flight model work too (issue #24).
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		select {
		case <-ba.done:
			cancel()
		case <-ctx.Done():
		}
	}()

	ba.agent.SetState(StateThinking)
	ba.agent.SetStatus(StatusRunning)
	responses, err := s.runLoop(ctx, ba.agent, ba.agent.ID, ba.agent.SeededMessage(task), s.subAgentPrompt(true))
	ba.agent.SetState(StateIdle)
	if err != nil {
		s.finishBackground(ba, StatusFailed, err.Error())
		return
	}
	final := ""
	if len(responses) > 0 {
		final = strings.TrimSpace(responses[len(responses)-1].Content)
	}
	s.finishBackground(ba, subAgentOutcome(final), final)
}

// finishBackground records a terminal status for an async sub-agent, notifies the
// UI observer, and queues its result for re-injection into the root loop. Unlike
// finishInteractive it does NOT push an AgentEvent onto agentEvents: a background
// result is delivered by re-injection, and the model never drives wait_agent_event
// for it, so pushing terminal events the coordinator never drains would let
// pendingTerminal grow without bound over a long session (issue #27 inverted).
func (s *UserSession) finishBackground(ba *BackgroundAgent, status AgentStatus, result string) {
	ba.agent.SetState(StateIdle)
	ba.agent.SetStatus(status)
	ba.agent.SetResult(result)
	// Deliberately do NOT touch the parent's state: an async spawn never set the root
	// to StateWaitingForSubAgent (unlike the blocking SpawnSubAgent / interactive
	// paths), because the root keeps running its own loop while the worker is in
	// flight. The root owns its own state machine; the worker only re-injects its
	// result and updates the session-level background flag (issue #353).
	s.emitSubAgent(ba.agent, "", nil)
	s.enqueueBackgroundResult(ba.Name, status, result)
}

// addBackground registers a background worker and reports whether it is the first
// one (the 0→1 edge), so the caller emits SessionEventBackground exactly on the edge.
func (s *UserSession) addBackground(ba *BackgroundAgent) (first bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	first = len(s.background) == 0
	s.background[ba.ID] = ba
	return first
}

// removeBackground unregisters a finished background worker. When it was the last
// one (the 1→0 edge) it emits SessionEventBackground{false} so the session can fall
// back to idle once the main loop is also done.
func (s *UserSession) removeBackground(id string) {
	s.mu.Lock()
	_, ok := s.background[id]
	delete(s.background, id)
	empty := ok && len(s.background) == 0
	s.mu.Unlock()
	if empty {
		s.emit(SessionEvent{Type: SessionEventBackground, Background: false})
	}
}

// HasBackgroundWork reports whether any async sub-agent is currently running. It
// backs the "working in background" session state surfaced by sessionToView and the
// TUI (issue #353).
func (s *UserSession) HasBackgroundWork() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.background) > 0
}

// backgroundResultTemplate frames a re-injected async result so the model reads it
// as a delivered background completion rather than a fresh user instruction. %q is
// the sub-agent name, %s its result text.
const backgroundResultTemplate = "[Background sub-agent %q finished]\n%s"

// enqueueBackgroundResult appends a completed async result to the re-injection
// queue. The root runLoop drains it at a turn boundary (takeBackgroundResults).
func (s *UserSession) enqueueBackgroundResult(name string, status AgentStatus, result string) {
	if strings.TrimSpace(name) == "" {
		name = "sub-agent"
	}
	body := result
	if strings.TrimSpace(body) == "" {
		body = "(no result text; status: " + string(status) + ")"
	}
	s.mu.Lock()
	s.backgroundResults = append(s.backgroundResults, fmt.Sprintf(backgroundResultTemplate, name, body))
	s.mu.Unlock()
}

// takeBackgroundResults drains the queued async results as role:user messages to
// splice into the root loop's next request (issue #353, re-injection approach A).
// They are role:user — not role:tool — because a background completion has no
// matching tool_call id in the live turn; a tool message without one would break
// strict providers. Returns nil when nothing is queued. FIFO == completion order.
func (s *UserSession) takeBackgroundResults() []model.Message {
	s.mu.Lock()
	queued := s.backgroundResults
	s.backgroundResults = nil
	s.mu.Unlock()
	if len(queued) == 0 {
		return nil
	}
	msgs := make([]model.Message, len(queued))
	for i, text := range queued {
		msgs[i] = model.Message{Role: model.RoleUser, Content: text}
	}
	return msgs
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
	ID      string
	Name    string
	agent   *Agent
	inbox   chan string   // coordinator → sub-agent messages (e.g. CLARIFY answers)
	done    chan struct{} // closed to request termination
	once    sync.Once
	limiter *SubAgentLimiter // the concurrency slot acquired at launch, released once when the worker exits
}

// LaunchInteractiveAgent starts an asynchronous sub-agent and returns its id
// immediately. The worker runs concurrently; the coordinator observes its
// progress via NextAgentEvent / InteractiveAgentStatus and can steer it with
// SendToInteractiveAgent / TerminateInteractiveAgent.
//
// Fire-and-forget agents count against the shared SubAgentLimiter just like
// one-shot batches, so the global MaxConcurrent cap bounds BOTH engines (issue
// #284 — "both" is the default, so async launches must not be able to slip the
// global ceiling). Because a launch must stay non-blocking, the slot is acquired
// or the launch is rejected (rather than running inline as RunSubAgentsBounded
// does): the coordinator can wait for a running agent to finish and retry. The
// slot is released in runInteractive when the worker goroutine exits.
func (s *UserSession) LaunchInteractiveAgent(parentAgentID, name, task string) (string, error) {
	s.mu.RLock()
	limiter := s.subAgentLimiter
	s.mu.RUnlock()
	if !limiter.tryAcquire() {
		return "", fmt.Errorf("sub-agent concurrency limit reached: wait for a running agent to finish (via wait_agent_event) before launching another")
	}

	child, err := s.newSubAgent(parentAgentID, name, task, KindInteractive)
	if err != nil {
		limiter.release() // never started the worker; give the slot back
		return "", err
	}
	parent := child.GetParent()

	ia := &InteractiveAgent{
		ID:      child.ID,
		Name:    child.DisplayName(),
		agent:   child,
		inbox:   make(chan string, 4),
		done:    make(chan struct{}),
		limiter: limiter, // release exactly this acquired slot when the worker exits
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
	// Release the global concurrency slot acquired in LaunchInteractiveAgent when
	// this worker exits, on every terminal path (completion, failure, termination,
	// panic). Release the exact limiter captured at acquire time (not a re-read of
	// s.subAgentLimiter) so the release can never hit a different limiter if one
	// were ever swapped in mid-launch. Exactly one release pairs with the one
	// acquire at launch.
	defer ia.limiter.release()

	// This runs on its own background goroutine, so a panic here would crash the
	// whole process. Contain it and finish the agent as failed instead (issue #8).
	defer func() {
		if r := recover(); r != nil {
			s.finishInteractive(ia, StatusFailed, fmt.Sprintf("panic: %v", r), AgentEventFailed)
		}
	}()
	// Interactive sub-agents are asynchronous and outlive any single turn, so
	// their loop is scoped to the session (background) rather than a turn context.
	// Terminating the agent cancels its in-flight model work too (issue #24).
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		select {
		case <-ia.done:
			cancel()
		case <-ctx.Done():
		}
	}()

	// Seed only the first turn with the parent's primer; subsequent turns carry
	// the coordinator's replies verbatim (issue #283).
	message := ia.agent.SeededMessage(task)
	for {
		select {
		case <-ia.done:
			s.finishInteractive(ia, StatusFailed, "terminated by coordinator", AgentEventFailed)
			return
		default:
		}

		ia.agent.SetState(StateThinking)
		ia.agent.SetStatus(StatusRunning)
		responses, err := s.runLoop(ctx, ia.agent, ia.agent.ID, message, s.subAgentPrompt(false))
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

// pushAgentEvent delivers a coordinator event without blocking the sub-agent
// goroutine. Non-terminal (clarify) events are best-effort and dropped when the
// buffer is full, since the coordinator can still recover them via agent_status.
// Terminal (completed/failed) events are never dropped: when the buffer is full
// they spill into pendingTerminal, which NextAgentEvent drains (issue #27).
func (s *UserSession) pushAgentEvent(ev AgentEvent) {
	select {
	case s.agentEvents <- ev:
		return
	default:
	}
	if ev.Type == AgentEventClarify {
		return
	}
	s.mu.Lock()
	s.pendingTerminal = append(s.pendingTerminal, ev)
	s.mu.Unlock()
}

// popPendingTerminal removes and returns the oldest spilled terminal event, if
// any.
func (s *UserSession) popPendingTerminal() (AgentEvent, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.pendingTerminal) == 0 {
		return AgentEvent{}, false
	}
	ev := s.pendingTerminal[0]
	s.pendingTerminal = s.pendingTerminal[1:]
	return ev, true
}

// NextAgentEvent blocks for the next interactive sub-agent event, up to timeout.
// A non-positive timeout waits INDEFINITELY: callers must only pass one when they
// know an event is still coming (a running agent will report), otherwise the call
// blocks forever with nothing to wake it. The wait_agent_event tool therefore
// always passes a finite timeout (see defaultWaitAgentEventTimeout). The boolean
// is false on timeout.
func (s *UserSession) NextAgentEvent(timeout time.Duration) (AgentEvent, bool) {
	// Drain buffered events first, then any terminal events that spilled over
	// when the buffer was full, before blocking. This guarantees a terminal
	// event is always observable and the coordinator never waits forever for one
	// that was discarded (issue #27).
	select {
	case ev := <-s.agentEvents:
		return ev, true
	default:
	}
	if ev, ok := s.popPendingTerminal(); ok {
		return ev, true
	}
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

// SendToInteractiveAgent delivers a message (an answer to a CLARIFY) to an
// interactive sub-agent that is awaiting input. The interactive loop only reads
// its inbox while paused at a CLARIFY, so a message sent to an agent that is busy
// running (and may finish without ever pausing) would be silently discarded. To
// avoid reporting a false success for a message that will never be consumed, this
// rejects sends unless the agent is currently waiting; the coordinator should
// drive sends off a CLARIFY event from wait_agent_event (by which point the agent
// is waiting).
func (s *UserSession) SendToInteractiveAgent(agentID, message string) error {
	s.mu.RLock()
	ia := s.interactive[agentID]
	s.mu.RUnlock()
	if ia == nil {
		return &NotFoundError{ID: agentID}
	}
	if status := ia.agent.GetStatus(); status != StatusWaiting {
		return fmt.Errorf("agent %s is not awaiting input (status %s); agent_send only answers a CLARIFY question", agentID, status)
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
	ba := s.background[agentID]
	root := s.RootAgent
	s.mu.RUnlock()
	if ia != nil {
		return ia.agent.GetStatus(), ia.agent.GetResult(), nil
	}
	// An async (spawn_subagent{async:true}) handle is queryable the same way while the
	// worker runs (issue #353).
	if ba != nil {
		return ba.agent.GetStatus(), ba.agent.GetResult(), nil
	}
	// A finished async worker is dropped from the background map but stays in the
	// agent tree, so resolve a completed/failed handle there: the symbolic-future
	// handle remains queryable for its terminal status and result after completion,
	// matching the MCP-task-handle semantics the design cites, rather than going
	// NotFound the instant the work lands (issue #353).
	if root != nil {
		if a := root.GetAgentByID(agentID); a != nil {
			return a.GetStatus(), a.GetResult(), nil
		}
	}
	return "", "", &NotFoundError{ID: agentID}
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

// StopAgent stops an agent, cancelling its in-flight task loop (and, since
// sub-agent loops inherit the parent's context, any sub-agents it spawned) so
// the work aborts immediately instead of running to the request timeout.
func (s *UserSession) StopAgent(agentID string) error {
	s.mu.Lock()
	agent := s.RootAgent.GetAgentByID(agentID)
	if agent == nil {
		s.mu.Unlock()
		return &NotFoundError{ID: agentID}
	}
	// Snapshot any async (background) workers within the stopped agent's subtree.
	// Unlike blocking sub-agents — whose loops inherit the parent's context and so are
	// cancelled transitively by agent.Cancel below — background workers run on a
	// session-scoped context (they outlive a single turn by design), so cancelling
	// only the target would leave them running after a user-visible /stop. Closing
	// their done channel cancels the worker even before it reaches runLoop, and
	// cancelling the worker's own agent aborts its in-flight model round-trip (issue
	// #353, cancellation criterion). GetAgentByID scopes this to the target's subtree,
	// so StopAgent("root") stops every background worker while StopAgent on a specific
	// handle stops just that one.
	var workers []*BackgroundAgent
	for _, ba := range s.background {
		if agent.GetAgentByID(ba.agent.ID) != nil {
			workers = append(workers, ba)
		}
	}
	s.mu.Unlock()

	for _, ba := range workers {
		ba.once.Do(func() { close(ba.done) })
		ba.agent.Cancel()
	}
	agent.Cancel()
	agent.SetState(StateIdle)
	return nil
}

// Stop cancels every in-flight task loop in the session. It is called when the
// session is removed/closed so detached loops do not keep mutating a session
// that is no longer reachable (issue #24).
func (s *UserSession) Stop() {
	s.mu.RLock()
	root := s.RootAgent
	// Snapshot the async workers so their done channels are closed too: closing done
	// cancels a background loop's session-scoped context (it does not inherit a turn
	// context), which a.Cancel below also does via the loop's published cancel — both
	// are idempotent, so doing both is safe and guarantees cancellation regardless of
	// whether the worker has reached runLoop yet (issue #353).
	workers := make([]*BackgroundAgent, 0, len(s.background))
	for _, ba := range s.background {
		workers = append(workers, ba)
	}
	s.mu.RUnlock()
	if root == nil {
		return
	}
	for _, ba := range workers {
		ba.once.Do(func() { close(ba.done) })
	}
	for _, a := range root.ListAllAgents() {
		a.Cancel()
	}
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

// InterruptAgent interrupts an agent, cancelling its in-flight task loop.
func (s *UserSession) InterruptAgent(agentID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	agent := s.RootAgent.GetAgentByID(agentID)
	if agent == nil {
		return &NotFoundError{ID: agentID}
	}

	agent.Cancel()
	agent.SetState(StateIdle)
	return nil
}

// CountMessages counts total messages in the session
func (s *UserSession) CountMessages() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.countMessagesLocked()
}

// countMessagesLocked counts total messages in the session. Callers must hold
// s.mu (read or write). RWMutex is not reentrant, so methods that already hold
// the lock must use this instead of CountMessages.
func (s *UserSession) countMessagesLocked() int {
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
		"total_turns":     s.countMessagesLocked(),
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

// AddTokenUsage adds token usage to the session stats, attributing it to the
// currently selected primary model when one is known (see SetPrimaryModel) for
// the per-model breakdown surfaced in the Statistics view.
func (s *UserSession) AddTokenUsage(promptTokens, completionTokens int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tokenCountIn += promptTokens
	s.tokenCountOut += completionTokens
	if s.primaryModel != "" {
		m := s.perModelTokens[s.primaryModel]
		m.In += promptTokens
		m.Out += completionTokens
		s.perModelTokens[s.primaryModel] = m
	}
}

// SetPrimaryModel records the name of the model the session routes its primary
// turns through, so subsequent token usage is attributed to it. It is set by the
// backend when a user picks a model for a turn.
func (s *UserSession) SetPrimaryModel(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.primaryModel = name
}

// PrimaryModel returns the name of the currently selected primary model, or "".
func (s *UserSession) PrimaryModel() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.primaryModel
}

// ModelTokens returns the per-model token attribution accumulated by the session
// (keyed by model config name). The order is stable (sorted by model name) so
// the Statistics view renders deterministically.
func (s *UserSession) ModelTokens() []ModelTokenStat {
	s.mu.RLock()
	defer s.mu.RUnlock()
	names := make([]string, 0, len(s.perModelTokens))
	for name := range s.perModelTokens {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]ModelTokenStat, 0, len(names))
	for _, name := range names {
		m := s.perModelTokens[name]
		out = append(out, ModelTokenStat{Name: name, TokensIn: m.In, TokensOut: m.Out})
	}
	return out
}

// ModelTokenStat is a UI/report-facing view of one model's token usage within a
// session. It mirrors stats.ModelStat but lives in the agent package so the
// session can return it without importing the stats package (which would invert
// the dependency direction).
type ModelTokenStat struct {
	Name      string
	TokensIn  int
	TokensOut int
}

// CompactionCount returns how many context-compression passes have run in this
// session.
func (s *UserSession) CompactionCount() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.compactionCount
}

// Snapshot returns a mutex-free, point-in-time view of the session's per-session
// statistics (the data a UI renders as a compact status line). The scalar
// counters are copied under the session lock; the context figures are then read
// from the root agent's model session under its own lock. The two reads are
// sequential rather than nested, which keeps this safe to call from the task
// loop right after a model round-trip (no lock held there) and avoids the
// UserSession→ModelSession vs ModelSession→UserSession ordering inversion the
// token-callback path already lives with.
func (s *UserSession) Snapshot() SessionStats {
	s.mu.RLock()
	out := SessionStats{
		Turns:     s.turnCount,
		TokensIn:  s.tokenCountIn,
		TokensOut: s.tokenCountOut,
		ToolCalls: s.toolCallCount,
	}
	root := s.RootAgent
	s.mu.RUnlock()
	if root != nil && root.ThoughtTrain != nil {
		out.ContextTokens = root.ThoughtTrain.GetTokenCount()
		out.ContextWindow = root.ThoughtTrain.GetMaxContextLength()
	}
	return out
}

// emitUsage emits a SessionEventUsage carrying a fresh stats snapshot. It is
// called from the task loop after each model round-trip so a UI's status bar
// updates on every usage report.
func (s *UserSession) emitUsage(emit func(SessionEvent)) {
	emit(SessionEvent{Type: SessionEventUsage, Stats: s.Snapshot()})
}

// ConnectorStats returns the session's grand-total low-level connector statistics
// (request counts, token totals, timing, error breakdown), summed across every
// model the session has used.
//
// It reads the STABLE per-model accumulator (perModelConn) rather than summing the
// live per-agent connectors, which fixes issue #191: the live connector is rebuilt
// and zeroed every turn, is shared by sub-agents (so summing per agent
// double-counts it), and is lost on restart. The accumulator is fed monotonic
// deltas by recordConnectorUsage, so this total never resets across turns, model
// switches or sub-agent spawns and is never double-counted.
func (s *UserSession) ConnectorStats() model.StatsSnapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.connectorStatsLocked("")
}

// ModelConnectorStats returns the stable connector accumulator for a single model
// (by config name), or the grand total across all models when name is empty.
// Callers hold no lock.
func (s *UserSession) ModelConnectorStats(name string) model.StatsSnapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.connectorStatsLocked(name)
}

// connectorStatsLocked sums the per-model connector accumulator: a single model's
// bucket when name is non-empty, otherwise every bucket. Callers must hold s.mu.
func (s *UserSession) connectorStatsLocked(name string) model.StatsSnapshot {
	if name != "" {
		return s.perModelConn[name]
	}
	var total model.StatsSnapshot
	for _, snap := range s.perModelConn {
		total = total.Add(snap)
	}
	return total
}

// ModelUsage is a UI/report-facing view of one model's full usage within a session:
// the session-layer token attribution (perModelTokens) joined with the stable
// connector accumulator (perModelConn). It lets the Statistics report present a
// per-model breakdown of every metric the Overall panel scopes (issue #191).
type ModelUsage struct {
	Name      string
	TokensIn  int
	TokensOut int
	Connector model.StatsSnapshot
}

// PerModelStats returns the per-model token + connector usage accumulated by the
// session, keyed by model config name. The order is stable (sorted by name) so the
// Statistics view renders deterministically. A model appears if it has either token
// or connector activity attributed to it.
func (s *UserSession) PerModelStats() []ModelUsage {
	s.mu.RLock()
	defer s.mu.RUnlock()
	names := make(map[string]struct{}, len(s.perModelTokens)+len(s.perModelConn))
	for name := range s.perModelTokens {
		names[name] = struct{}{}
	}
	for name := range s.perModelConn {
		names[name] = struct{}{}
	}
	ordered := make([]string, 0, len(names))
	for name := range names {
		ordered = append(ordered, name)
	}
	sort.Strings(ordered)
	out := make([]ModelUsage, 0, len(ordered))
	for _, name := range ordered {
		t := s.perModelTokens[name]
		out = append(out, ModelUsage{
			Name:      name,
			TokensIn:  t.In,
			TokensOut: t.Out,
			Connector: s.perModelConn[name],
		})
	}
	return out
}

// SubAgentCount returns how many sub-agents the session currently holds (every
// agent in the tree except the root). The Statistics report uses it to attribute a
// session's sub-agents to its primary model for the per-model Overall view (issue
// #191).
func (s *UserSession) SubAgentCount() int {
	s.mu.RLock()
	root := s.RootAgent
	s.mu.RUnlock()
	if root == nil {
		return 0
	}
	n := len(root.ListAllAgents()) - 1
	if n < 0 {
		n = 0
	}
	return n
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
