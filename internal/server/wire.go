// Package server implements gogent's comprehensive HTTP+SSE API on top of
// hobbestherat/webapi. It exposes the full agent capability surface — sessions,
// messages (blocking + streamed), events, approvals, settings, models, tools,
// skills — so a browser or another gogent can drive it over the network.
//
// See docs/API.md for the full design and endpoint reference.
package server

import (
	"gogent/internal/agent"
	"gogent/internal/config"
	"gogent/internal/gogent"
	"gogent/internal/model"
)

// --- Sessions ---------------------------------------------------------------

// sessionView is the wire representation of a live or saved session. Live
// reports whether the session is currently in the daemon's memory (a running or
// restored UserSession) as opposed to merely a saved index entry: an attached
// TUI reopens windows for the live sessions and lists the rest, so it needs to
// tell the two apart from a single GET /sessions.
type sessionView struct {
	ID           string      `json:"id"`
	Title        string      `json:"title,omitempty"`
	CreatedAt    string      `json:"created_at,omitempty"`
	State        string      `json:"state"`
	PrimaryModel string      `json:"primary_model,omitempty"`
	Persisted    bool        `json:"persisted"`
	Live         bool        `json:"live"`
	Agents       []agentView `json:"agents,omitempty"`
}

// agentView is one node in a session's agent tree.
type agentView struct {
	ID     string `json:"id"`
	Name   string `json:"name,omitempty"`
	Status string `json:"status"`
	Kind   string `json:"kind"`
	State  string `json:"state,omitempty"`
}

// createSessionRequest is the body of POST /sessions. ID is optional: when set
// (and not already live) the session is created under that exact id rather than
// a server-generated one, so an attached TUI can keep its window id and the
// daemon session id in lock-step (the event stream is routed by that id). An
// empty ID preserves the original server-assigns-the-id behaviour.
type createSessionRequest struct {
	ID        string `json:"id,omitempty"`
	Title     string `json:"title"`
	Persisted bool   `json:"persisted"`
	Model     string `json:"model"`
}

// sessionStatsView mirrors agent.SessionStats.
type sessionStatsView struct {
	Turns         int `json:"turns"`
	TokensIn      int `json:"tokens_in"`
	TokensOut     int `json:"tokens_out"`
	ToolCalls     int `json:"tool_calls"`
	ContextTokens int `json:"context_tokens"`
	ContextWindow int `json:"context_window"`
}

// --- Messages ---------------------------------------------------------------

// sendMessageRequest is the body of POST /sessions/:id/messages[?stream=true].
type sendMessageRequest struct {
	Message  string `json:"message"`
	Model    string `json:"model,omitempty"`
	Effort   string `json:"effort,omitempty"`
	Thinking string `json:"thinking,omitempty"` // "on" | "off" | "" (model default)
	Mode     string `json:"mode,omitempty"`     // "normal" (default) | "plan"
	// Agent/Subtask carry a custom command's per-invocation overrides (issue #403):
	// a non-empty Agent or Subtask=true routes the prompt through a daemon-side
	// one-shot sub-agent (Agent names it) whose result is surfaced as the turn's
	// final answer, so an attached TUI applies the overrides exactly as embedded.
	Agent   string `json:"agent,omitempty"`
	Subtask bool   `json:"subtask,omitempty"`
}

// messageView is the response from the blocking message endpoint.
type messageView struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// acceptedView is the response from the non-blocking send/approve endpoints
// (issue #481): the id of the turn the daemon dispatched. The turn runs to
// completion independently of the request connection; its progress and final
// answer arrive over the SSE hub, not in this response. (The framework cannot
// emit a literal 202 for a JSON body, so this is returned with 200 — the
// behavioural contract is the non-blocking return plus the turn id, see design
// §2.)
//
// Field name note: this POST/approve response uses camelCase "turnId" while SSE
// events (eventView) use snake_case "turn_id". They are distinct messages decoded
// into distinct client structs, so the difference is cosmetic; the client-side
// contract tests pin "turnId" here, so normalizing both to turn_id is a
// coordinated follow-up that would also update those tests.
type acceptedView struct {
	TurnID string `json:"turnId"`
}

// transcriptQuery binds the ?agent= query parameter.
type transcriptQuery struct {
	Agent string `json:"agent"`
}

// listSessionsQuery binds the optional ?live=&limit=&offset= params on
// GET /sessions (issue #517). All are absent-by-default; an absent param preserves
// the pre-#517 full, ID-sorted listing, so any other caller is unaffected.
//
// Live is a string, not a bool, deliberately: the webapi query binder only binds
// string and int/int64 struct fields, so a bool would be silently dropped. It is
// parsed truthy for "true"/"1" — restricting the listing to live sessions, which
// excludes archived/closed ones (they are persisted-but-not-live).
type listSessionsQuery struct {
	Live   string `json:"live"`   // "true"/"1" => live sessions only (archived excluded)
	Limit  int    `json:"limit"`  // <=0 => no cap
	Offset int    `json:"offset"` // <0 => 0; >len => empty slice
}

// --- Session control --------------------------------------------------------

type injectRequest struct {
	Message string `json:"message"`
}

type rewindRequest struct {
	Turns int `json:"turns"`
}

// --- Plan mode --------------------------------------------------------------

type planModeRequest struct {
	Enabled bool `json:"enabled"`
}

type planView struct {
	Plan string `json:"plan"`
}

// --- Events -----------------------------------------------------------------

// --- Watchers ---------------------------------------------------------------

// watcherView is the wire representation of a watcher (issue #329 Phase 5). It
// is the typed mirror of tool.watcherInfoMap: target is the owning session id
// for attached watchers or "free" for free-running ones; zero timestamps are
// omitted.
type watcherView struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Kind       string `json:"kind"`   // "free" | "attached"
	Target     string `json:"target"` // session id, or "free" for free-running
	Task       string `json:"task,omitempty"`
	Schedule   string `json:"schedule,omitempty"`
	Enabled    bool   `json:"enabled"`
	Status     string `json:"status"`
	NextFire   string `json:"next_fire,omitempty"`
	LastRun    string `json:"last_run,omitempty"`
	LastResult string `json:"last_result,omitempty"`
	LastError  string `json:"last_error,omitempty"`
}

// createWatcherRequest is the body of POST /watchers. Schedule reuses the config
// type (exactly one of every / daily_at). Enabled defaults to true when omitted.
// ReportToSession decides the watcher's kind: nil/omitted = free-running
// (global); a non-nil session id = attached to that (live) session.
type createWatcherRequest struct {
	Name            string                `json:"name"`
	Task            string                `json:"task"`
	Schedule        config.ScheduleConfig `json:"schedule"`
	Model           string                `json:"model,omitempty"`
	Enabled         *bool                 `json:"enabled,omitempty"`
	ReportToSession *string               `json:"report_to_session,omitempty"`
	Output          *config.WatcherOutput `json:"on_complete,omitempty"`
}

// watcherListQuery binds the ?session_id= query parameter of GET /watchers. An
// empty session id lists free-running watchers only; a session id lists every
// free-running watcher plus that session's own attached watchers (the scoping
// the gogent ListWatchers wrapper enforces).
type watcherListQuery struct {
	SessionID string `json:"session_id"`
}

// updateWatcherRequest is the body of PUT/PATCH /watchers/:id — a sparse patch.
// Only non-empty fields are applied; the watcher's kind/owning session is never
// changed.
type updateWatcherRequest struct {
	Name     string                `json:"name,omitempty"`
	Task     string                `json:"task,omitempty"`
	Schedule config.ScheduleConfig `json:"schedule,omitempty"`
	Model    string                `json:"model,omitempty"`
}

// --- Custom commands (issue #403) -------------------------------------------

// commandParamView is the wire form of config.CommandParam.
type commandParamView struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Required    bool   `json:"required,omitempty"`
	Default     string `json:"default,omitempty"`
}

// commandVersionView is the wire form of one immutable history snapshot.
type commandVersionView struct {
	Version    int                `json:"version"`
	Template   string             `json:"template"`
	Parameters []commandParamView `json:"parameters,omitempty"`
	Model      string             `json:"model,omitempty"`
	Agent      string             `json:"agent,omitempty"`
	Subtask    bool               `json:"subtask,omitempty"`
	SavedAt    string             `json:"saved_at"`
}

// commandView is the wire representation of a custom command (issue #403): the
// latest content plus the append-only version history.
type commandView struct {
	Name        string               `json:"name"`
	Description string               `json:"description,omitempty"`
	Parameters  []commandParamView   `json:"parameters,omitempty"`
	Template    string               `json:"template"`
	Model       string               `json:"model,omitempty"`
	Agent       string               `json:"agent,omitempty"`
	Subtask     bool                 `json:"subtask,omitempty"`
	Version     int                  `json:"version"`
	Versions    []commandVersionView `json:"versions,omitempty"`
	// Warnings carries the save-time template warnings (placeholders with no
	// matching parameter, which expand literally) on a create/update response, so a
	// non-TUI API client gets the same feedback the editor shows at save time. It is
	// empty on list/get and omitted from JSON when there are none.
	Warnings []string `json:"warnings,omitempty"`
}

// commandBody is the request body of POST /commands (create) and PUT
// /commands/:name (update). Versions are server-owned (append-only) and ignored
// if sent.
type commandBody struct {
	Name        string             `json:"name"`
	Description string             `json:"description,omitempty"`
	Parameters  []commandParamView `json:"parameters,omitempty"`
	Template    string             `json:"template"`
	Model       string             `json:"model,omitempty"`
	Agent       string             `json:"agent,omitempty"`
	Subtask     bool               `json:"subtask,omitempty"`
}

// restoreCommandBody is the request body of POST /commands/:name/restore.
type restoreCommandBody struct {
	Version int `json:"version"`
}

// --- Events -----------------------------------------------------------------

// eventView is the SSE wire representation of an agent.SessionEvent. The event
// type is carried as the SSE event: field; the data: is this struct as JSON.
type eventView struct {
	Type   string            `json:"type"`
	Step   int               `json:"step,omitempty"`
	Text   string            `json:"text,omitempty"`
	Tool   string            `json:"tool,omitempty"`
	Args   map[string]any    `json:"args,omitempty"`
	Result string            `json:"result,omitempty"`
	CallID string            `json:"call_id,omitempty"`
	Error  string            `json:"error,omitempty"`
	Stats  *sessionStatsView `json:"stats,omitempty"`
	// Sub-agent fields (populated on "subagent" events).
	AgentID string `json:"agent_id,omitempty"`
	Name    string `json:"name,omitempty"`
	Status  string `json:"status,omitempty"`
	Kind    string `json:"kind,omitempty"`
	// Todo/plan fields.
	Todos []todoItemView `json:"todos,omitempty"`
	Plan  string         `json:"plan,omitempty"`
	// SessionID is set on the global event stream so a client can route events.
	SessionID string `json:"session_id,omitempty"`
	// TurnID correlates the event with the async-dispatched turn that produced it
	// (issue #481). Empty for legacy/embedded turns and non-turn events.
	TurnID string `json:"turn_id,omitempty"`
}

type todoItemView struct {
	Content string `json:"content"`
	Status  string `json:"status"`
}

// --- Notifications ----------------------------------------------------------

// NotificationEvent is a backend-originated notification (a watcher/agent
// completion) delivered to a connected client over the global SSE stream
// (/api/events) as an SSE event named "notification" (issue #358 §9). A
// connected TUI raises an OS desktop notification on ITS machine from it; when
// no client is connected the daemon falls back to its local notify.Notifier and
// buffers the event in a bounded ring for replay on reconnect.
//
// Reason is the notify.Reason token ("watcher", "complete", …). SessionID is the
// originating session when known and empty for free-running watcher completions
// (which have no owning user session). Timestamp is RFC3339 (UTC), stamped by
// the daemon when the notification fires.
type NotificationEvent struct {
	Title     string `json:"title"`
	Body      string `json:"body"`
	Reason    string `json:"reason"`
	SessionID string `json:"session_id,omitempty"`
	Timestamp string `json:"timestamp,omitempty"`
}

// --- Approvals --------------------------------------------------------------

// approvalView is a pending interactive gate (permission prompt or edit review).
type approvalView struct {
	ID         string            `json:"id"`
	Kind       string            `json:"kind"` // "permission" | "edit_review"
	SessionID  string            `json:"session_id"`
	AgentID    string            `json:"agent_id,omitempty"`
	Permission *permissionDetail `json:"permission,omitempty"`
	EditReview *editReviewDetail `json:"edit_review,omitempty"`
	CreatedAt  string            `json:"created_at"`
}

type permissionDetail struct {
	Action   string `json:"action"`
	Resource string `json:"resource"`
	Detail   string `json:"detail,omitempty"`
}

type editReviewDetail struct {
	Path string `json:"path"`
	Op   string `json:"op"`
	Diff string `json:"diff"`
}

// approvalDecisionRequest is the body of POST /approvals/:aid/decision.
type approvalDecisionRequest struct {
	Decision string `json:"decision"`
}

// --- Settings ---------------------------------------------------------------

type settingsView struct {
	SubAgents config.SubAgentConfig `json:"sub_agents"`
	Timeouts  config.TimeoutConfig  `json:"timeouts"`
	Budget    config.BudgetConfig   `json:"budget"`
	// DefaultModel is the daemon's default-model name for new sessions (issue #507).
	// It is daemon-owned: an attached TUI reads and writes it here over HTTP rather
	// than from the client machine's config, exactly as budget is. Empty in a PUT
	// leaves the daemon's default unchanged (an older client that omits the field
	// never clears it).
	DefaultModel string `json:"default_model"`
	ReviewEdits  bool   `json:"review_edits"`
}

type reviewEditsView struct {
	Enabled bool `json:"enabled"`
}

// --- Models -----------------------------------------------------------------

// capsView mirrors config.ModelCapabilities on the wire (display + selectors).
type capsView struct {
	ContextWindow    int      `json:"context_window,omitempty"`
	MaxOutput        int      `json:"max_output,omitempty"`
	Reasoning        bool     `json:"reasoning,omitempty"`
	ThinkingToggle   bool     `json:"thinking_toggle,omitempty"`
	EffortOptions    []string `json:"effort_options,omitempty"`
	Vision           bool     `json:"vision,omitempty"`
	ToolCall         bool     `json:"tool_call,omitempty"`
	StructuredOutput bool     `json:"structured_output,omitempty"`
	CustomTemp       bool     `json:"custom_temperature,omitempty"`
	InputModalities  []string `json:"input_modalities,omitempty"`
	OutputModalities []string `json:"output_modalities,omitempty"`
	InputCostPerM    float64  `json:"input_cost_per_m,omitempty"`
	OutputCostPerM   float64  `json:"output_cost_per_m,omitempty"`
	CacheReadPerM    float64  `json:"cache_read_per_m,omitempty"`
	CacheWritePerM   float64  `json:"cache_write_per_m,omitempty"`
	Knowledge        string   `json:"knowledge,omitempty"`
	ReleaseDate      string   `json:"release_date,omitempty"`
	Source           string   `json:"source,omitempty"`
}

// modelView is a model config on the wire (references a connection by name; caps
// nested; no credentials — those live on the connection).
type modelView struct {
	Name                string   `json:"name"`
	DisplayName         string   `json:"display_name"`
	Connection          string   `json:"connection"`
	Model               string   `json:"model"`
	Caps                capsView `json:"caps"`
	Free                bool     `json:"free"`
	Temperature         float32  `json:"temperature,omitempty"`
	TopP                float32  `json:"top_p,omitempty"`
	MaxTokens           int      `json:"max_tokens,omitempty"`
	ModelTimeoutSeconds int      `json:"model_timeout_seconds,omitempty"`
	ReasoningEffort     string   `json:"reasoning_effort,omitempty"`
	Thinking            *bool    `json:"thinking,omitempty"`
	CacheTTL            string   `json:"cache_ttl,omitempty"`
}

// connectionView is a redacted provider connection (api_key never echoed in GET
// responses; HasAPIKey reports whether one is set).
type connectionView struct {
	Name              string `json:"name"`
	APIType           string `json:"api_type,omitempty"`
	Endpoint          string `json:"endpoint,omitempty"`
	DiscoveryEndpoint string `json:"discovery_endpoint,omitempty"`
	Project           string `json:"project,omitempty"`
	Location          string `json:"location,omitempty"`
	HasAPIKey         bool   `json:"has_api_key"`
}

// updateModelRequest is the body of PUT /models/:name.
type updateModelRequest struct {
	config.ModelConfig
}

// updateConnectionRequest is the body of POST/PUT /connections. APIKey is optional
// on update: an empty string preserves the existing key so a GET→edit→PUT
// round-trip doesn't wipe it.
type updateConnectionRequest struct {
	config.ProviderConnection
}

// discoveredModelView is one merged discovery result (live + catalog).
type discoveredModelView struct {
	ID          string   `json:"id"`
	DisplayName string   `json:"display_name,omitempty"`
	Available   bool     `json:"available"`
	InCatalog   bool     `json:"in_catalog"`
	Caps        capsView `json:"caps"`
}

// --- Tools ------------------------------------------------------------------

type toolView struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	InputSchema string `json:"input_schema,omitempty"`
	Enabled     bool   `json:"enabled"`
	Invocations int    `json:"invocations"`
	ReadOnly    bool   `json:"read_only"`
}

type setEnabledRequest struct {
	Enabled bool `json:"enabled"`
}

// --- Skills -----------------------------------------------------------------

type skillView struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Active      bool   `json:"active"`
	Success     int    `json:"success"`
	Failure     int    `json:"failure"`
	TotalCalls  int    `json:"total_calls"`
}

// --- System -----------------------------------------------------------------

type healthView struct {
	Status string `json:"status"`
}

type workspaceView struct {
	Root string   `json:"root"`
	Git  *gitInfo `json:"git,omitempty"`
}

// daemonStatusView is the wire representation of GET /api/daemon/status: the
// one-call summary the TUI's "Daemon status" menu renders (issue #358 §6). It is
// composed from process state plus the live core so an attached client gets pid,
// uptime, the live session/watcher counts and the connected MCP servers without
// stitching together /health + /sessions + /watchers. LiveSessions counts the
// user-facing live sessions only — the shared "default" HTTP session and the
// backend-only "watcher:" sessions are excluded so the figure matches what the
// user sees in their windows. Watchers counts the free-running watchers (the
// ones that keep firing while the terminal is away, which is what the daemon is
// for); attached watchers belong to their session.
type daemonStatusView struct {
	PID           int      `json:"pid"`
	StartedAt     string   `json:"started_at"`
	UptimeSeconds int64    `json:"uptime_seconds"`
	LiveSessions  int      `json:"live_sessions"`
	Watchers      int      `json:"watchers"`
	MCPServers    []string `json:"mcp_servers"`
}

type gitInfo struct {
	Branch string `json:"branch,omitempty"`
	Dirty  bool   `json:"dirty"`
}

// shellRequest is the body of POST /api/shell (issue #571): a single !-prefixed
// shell command to run out-of-band at the daemon workspace root, outside any
// agent turn.
type shellRequest struct {
	Command string `json:"command"`
}

// shellView is the wire result of POST /api/shell, mirroring
// internal/shell.ExecuteResult: stdout/stderr, the command exit code, whether it
// timed out, and an Error set only when the command could not be launched (a
// non-zero exit is NOT an error — it is carried in ExitCode).
type shellView struct {
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
	ExitCode int    `json:"exit_code,omitempty"`
	Timeout  bool   `json:"timeout,omitempty"`
	Error    string `json:"error,omitempty"`
}

// --- Conversion helpers -----------------------------------------------------

func capsToView(c config.ModelCapabilities) capsView {
	return capsView{
		ContextWindow:    c.ContextWindow,
		MaxOutput:        c.MaxOutput,
		Reasoning:        c.Reasoning,
		ThinkingToggle:   c.ThinkingToggle,
		EffortOptions:    c.EffortOptions,
		Vision:           c.Vision,
		ToolCall:         c.ToolCall,
		StructuredOutput: c.StructuredOutput,
		CustomTemp:       c.CustomTemp,
		InputModalities:  c.InputModalities,
		OutputModalities: c.OutputModalities,
		InputCostPerM:    c.InputCostPerM,
		OutputCostPerM:   c.OutputCostPerM,
		CacheReadPerM:    c.CacheReadPerM,
		CacheWritePerM:   c.CacheWritePerM,
		Knowledge:        c.Knowledge,
		ReleaseDate:      c.ReleaseDate,
		Source:           c.Source,
	}
}

func modelToView(m *config.ModelConfig) modelView {
	if m == nil {
		return modelView{}
	}
	return modelView{
		Name:                m.Name,
		DisplayName:         m.DisplayName,
		Connection:          m.Connection,
		Model:               m.Model,
		Caps:                capsToView(m.Caps),
		Free:                m.Caps.Free(),
		Temperature:         m.Temperature,
		TopP:                m.TopP,
		MaxTokens:           m.MaxTokens,
		ModelTimeoutSeconds: m.ModelTimeoutSeconds,
		ReasoningEffort:     m.ReasoningEffort,
		Thinking:            m.Thinking,
		CacheTTL:            m.CacheTTL,
	}
}

func connectionToView(c *config.ProviderConnection) connectionView {
	if c == nil {
		return connectionView{}
	}
	return connectionView{
		Name:              c.Name,
		APIType:           c.APIType,
		Endpoint:          c.Endpoint,
		DiscoveryEndpoint: c.DiscoveryEndpoint,
		Project:           c.Project,
		Location:          c.Location,
		HasAPIKey:         c.APIKey != "",
	}
}

func discoveredToView(d model.DiscoveredModel) discoveredModelView {
	return discoveredModelView{
		ID:          d.ID,
		DisplayName: d.DisplayName,
		Available:   d.Available,
		InCatalog:   d.InCatalog,
		Caps:        capsToView(d.Caps),
	}
}

func sessionToView(g *gogent.Gogent, id string, us *agent.UserSession, title string) sessionView {
	v := sessionView{
		ID:           id,
		Title:        title,
		State:        "idle",
		PrimaryModel: us.PrimaryModel(),
	}
	if us.RootAgent != nil {
		for _, a := range us.RootAgent.ListAllAgents() {
			v.Agents = append(v.Agents, agentView{
				ID:     a.ID,
				Name:   a.DisplayName(),
				Status: string(a.GetStatus()),
				Kind:   string(a.Kind),
				State:  string(a.GetState()),
			})
		}
		// Derive a human-friendly state from the root agent.
		switch us.RootAgent.GetState() {
		case agent.StateThinking:
			v.State = "thinking"
		case agent.StateWaitingForSubAgent, agent.StateWaitingForShell, agent.StateWaitingForTool:
			v.State = "waiting"
		}
	}
	// While async sub-agents run in the background the session must not read as idle,
	// even after the main loop's turn has ended (issue #353). Surface a third
	// "background" state when the root loop is otherwise idle but background work is
	// still in flight. A root that is actively thinking/waiting already reads as busy,
	// so only the idle case is overridden.
	if v.State == "idle" && us.HasBackgroundWork() {
		v.State = "background"
	}
	return v
}

func snapshotToView(s agent.SessionStats) sessionStatsView {
	return sessionStatsView{
		Turns:         s.Turns,
		TokensIn:      s.TokensIn,
		TokensOut:     s.TokensOut,
		ToolCalls:     s.ToolCalls,
		ContextTokens: s.ContextTokens,
		ContextWindow: s.ContextWindow,
	}
}

func eventToView(ev agent.SessionEvent, sessionID string) eventView {
	v := eventView{
		Type:      string(ev.Type),
		Step:      ev.Step,
		Text:      ev.Text,
		Tool:      ev.Tool,
		Args:      ev.Args,
		Result:    ev.Result,
		CallID:    ev.CallID,
		AgentID:   ev.AgentID,
		Name:      ev.Name,
		Status:    string(ev.Status),
		Kind:      string(ev.Kind),
		Plan:      ev.Plan,
		SessionID: sessionID,
		TurnID:    ev.TurnID,
	}
	if ev.Err != nil {
		v.Error = ev.Err.Error()
	}
	if ev.Type == agent.SessionEventUsage {
		s := snapshotToView(ev.Stats)
		v.Stats = &s
	}
	if len(ev.Todos) > 0 {
		v.Todos = make([]todoItemView, len(ev.Todos))
		for i, t := range ev.Todos {
			v.Todos[i] = todoItemView{Content: t.Content, Status: string(t.Status)}
		}
	}
	return v
}

func messagesToViews(msgs []model.Message) []messageView {
	out := make([]messageView, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, messageView{Role: string(m.Role), Content: m.Content})
	}
	return out
}

// (The /stats endpoint returns stats.Report directly — it is already
// JSON-serializable, so no separate view type is needed.)
