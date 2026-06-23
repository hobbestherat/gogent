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

// sessionView is the wire representation of a live or saved session.
type sessionView struct {
	ID           string      `json:"id"`
	Title        string      `json:"title,omitempty"`
	CreatedAt    string      `json:"created_at,omitempty"`
	State        string      `json:"state"`
	PrimaryModel string      `json:"primary_model,omitempty"`
	Persisted    bool        `json:"persisted"`
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

// createSessionRequest is the body of POST /sessions.
type createSessionRequest struct {
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
	Message string `json:"message"`
	Model   string `json:"model,omitempty"`
	Effort  string `json:"effort,omitempty"`
	Mode    string `json:"mode,omitempty"` // "normal" (default) | "plan"
}

// messageView is the response from the blocking message endpoint.
type messageView struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// transcriptQuery binds the ?agent= query parameter.
type transcriptQuery struct {
	Agent string `json:"agent"`
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
}

type todoItemView struct {
	Content string `json:"content"`
	Status  string `json:"status"`
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
	SubAgents   config.SubAgentConfig `json:"sub_agents"`
	Timeouts    config.TimeoutConfig  `json:"timeouts"`
	Budget      config.BudgetConfig   `json:"budget"`
	ReviewEdits bool                  `json:"review_edits"`
}

type reviewEditsView struct {
	Enabled bool `json:"enabled"`
}

// --- Models -----------------------------------------------------------------

// modelView is a redacted model config (api_key never echoed in GET responses).
type modelView struct {
	Name            string   `json:"name"`
	DisplayName     string   `json:"display_name"`
	APIType         string   `json:"api_type,omitempty"`
	Endpoint        string   `json:"endpoint"`
	Project         string   `json:"project,omitempty"`
	Location        string   `json:"location,omitempty"`
	Model           string   `json:"model"`
	HasAPIKey       bool     `json:"has_api_key"`
	Temperature     float32  `json:"temperature"`
	TopP            float32  `json:"top_p,omitempty"`
	MaxTokens       int      `json:"max_tokens"`
	ContextWindow   int      `json:"context_window,omitempty"`
	ReasoningEffort string   `json:"reasoning_effort,omitempty"`
	EffortOptions   []string `json:"effort_options,omitempty"`
	Thinking        *bool    `json:"thinking,omitempty"`
	Free            bool     `json:"free"`
}

// updateModelRequest is the body of PUT /models/:name. APIKey is optional: an
// empty string preserves the existing key so a GET→edit→PUT round-trip doesn't
// wipe it.
type updateModelRequest struct {
	config.ModelConfig
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

type gitInfo struct {
	Branch string `json:"branch,omitempty"`
	Dirty  bool   `json:"dirty"`
}

// --- Conversion helpers -----------------------------------------------------

func modelToView(m *config.ModelConfig) modelView {
	if m == nil {
		return modelView{}
	}
	return modelView{
		Name:            m.Name,
		DisplayName:     m.DisplayName,
		APIType:         m.APIType,
		Endpoint:        m.Endpoint,
		Project:         m.Project,
		Location:        m.Location,
		Model:           m.Model,
		HasAPIKey:       m.APIKey != "",
		Temperature:     m.Temperature,
		TopP:            m.TopP,
		MaxTokens:       m.MaxTokens,
		ContextWindow:   m.ContextWindow,
		ReasoningEffort: m.ReasoningEffort,
		EffortOptions:   m.EffortOptions,
		Thinking:        m.Thinking,
		Free:            m.Free,
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
