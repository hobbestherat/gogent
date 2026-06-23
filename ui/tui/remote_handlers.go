package ui

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"gogent/internal/agent"
	"gogent/internal/config"
	"gogent/internal/gogent"
	"gogent/internal/permission"
	"gogent/internal/stats"
)

// EventSink consumes a converted session event, normally Workbench.EmitSessionEvent.
// It is injected (rather than the RemoteClient calling the Workbench directly) so
// the SSE→event path can be driven and observed under test without a live UI.
type EventSink func(sessionID string, ev agent.SessionEvent)

// Approver presents an interactive gate and returns the user's verdict. The
// *Workbench already satisfies it (AskPermission + ReviewEdit render the exact
// same modals the embedded path uses), so the attached TUI reuses the whole
// approval UI unchanged; the RemoteClient only round-trips the decision over HTTP.
type Approver interface {
	AskPermission(permission.Request) permission.Decision
	ReviewEdit(gogent.EditReviewRequest) gogent.EditReviewDecision
}

// approvalPollInterval is how often the attached TUI polls GET /api/approvals for
// pending interactive gates. The current server announces approvals only via that
// list (no SSE push), so a short poll keeps remote prompts responsive without a
// new endpoint; it is cheap (a small JSON list) and bounded.
const approvalPollInterval = 750 * time.Millisecond

// watcherSessionPrefix mirrors the backend's session id for a free-running
// watcher's dedicated session ("watcher:<name>"). The remote WatcherInfo
// conversion resolves it exactly as cmd/main.go's embedded mapping does, so the
// sidebar's Open Session button raises the same id.
const watcherSessionPrefix = "watcher:"

// RemoteClient backs the TUI's Handlers seam with an APIClient when the TUI is
// attached to a daemon (issue #358, Phase 2). It owns three things: the
// request-mapping handlers (each Handlers field → one /api call), the global SSE
// consumer that feeds streamed events into the sink, and the approvals poller
// that drives the Workbench's permission/edit-review modals and POSTs the
// decisions back. It deliberately does not fork the TUI: the same typed
// agent.SessionEvent stream and the same modals are reused, just sourced over
// HTTP/SSE instead of in-process.
type RemoteClient struct {
	client   *APIClient
	sink     EventSink
	approver Approver

	// ctx bounds every background turn (SendMessage/ApprovePlan), the SSE
	// consumer and the approvals poller. It is created once at construction and
	// cancelled by Close (or when the ctx passed to Start is cancelled), so reads
	// of it from handler closures never race a write.
	ctx    context.Context
	cancel context.CancelFunc

	pollEvery time.Duration

	startOnce sync.Once
}

// NewRemoteClient builds a RemoteClient over the given APIClient. sink receives
// every event from the daemon's global SSE stream (normally
// Workbench.EmitSessionEvent); approver presents interactive gates (normally the
// *Workbench). Either may be nil in narrow tests, in which case the respective
// background loop is skipped.
func NewRemoteClient(client *APIClient, sink EventSink, approver Approver) *RemoteClient {
	ctx, cancel := context.WithCancel(context.Background())
	return &RemoteClient{
		client:    client,
		sink:      sink,
		approver:  approver,
		ctx:       ctx,
		cancel:    cancel,
		pollEvery: approvalPollInterval,
	}
}

// Client exposes the underlying APIClient so the attach wiring can make one-off
// calls (e.g. fetch the model list for the dropdown) without re-deriving it.
func (rc *RemoteClient) Client() *APIClient { return rc.client }

// Close stops the SSE consumer, the approvals poller and any in-flight turns by
// cancelling the client's context. It is safe to call more than once.
func (rc *RemoteClient) Close() { rc.cancel() }

// Start begins consuming the daemon's global event stream and polling for
// approvals. The initial SSE connect is synchronous so an unreachable/denied
// daemon fails fast (returned error); after that, the consumer reconnects with a
// short backoff until the context is cancelled. When parent is cancelled, the
// client's own context is cancelled too (stopping every background goroutine and
// turn). Start is idempotent — only the first call has effect.
func (rc *RemoteClient) Start(parent context.Context) error {
	var startErr error
	rc.startOnce.Do(func() {
		// Tie the externally-provided lifetime to the client's context.
		go func() {
			select {
			case <-parent.Done():
				rc.cancel()
			case <-rc.ctx.Done():
			}
		}()

		if rc.sink != nil {
			events, err := rc.client.StreamEvents(rc.ctx)
			if err != nil {
				startErr = fmt.Errorf("subscribe to daemon events: %w", err)
				return
			}
			go rc.consume(events)
		}
		if rc.approver != nil {
			go rc.pollApprovals()
		}
	})
	return startErr
}

// consume forwards the first (already-open) event stream into the sink, then
// reconnects on stream end until the context is cancelled. Reconnect is a plain
// best-effort retry (the disconnect modal + jump-to-present reconnect is a later
// slice); a transient blip simply re-subscribes and live events resume.
func (rc *RemoteClient) consume(events <-chan GlobalEventDTO) {
	for {
		for ge := range events {
			rc.sink(ge.SessionID, eventDTOToSessionEvent(ge.Event))
		}
		// Stream ended. Stop if we are shutting down; otherwise re-subscribe.
		if rc.ctx.Err() != nil {
			return
		}
		select {
		case <-rc.ctx.Done():
			return
		case <-time.After(time.Second):
		}
		next, err := rc.client.StreamEvents(rc.ctx)
		if err != nil {
			if rc.ctx.Err() != nil {
				return
			}
			continue // keep trying until the context is cancelled
		}
		events = next
	}
}

// eventDTOToSessionEvent rebuilds the typed agent.SessionEvent the TUI renders
// from the JSON view carried over SSE. It is the inverse of the server's
// eventToView, so the remote event stream drives the UI exactly as the in-process
// observer does.
func eventDTOToSessionEvent(e EventDTO) agent.SessionEvent {
	ev := agent.SessionEvent{
		Type:    agent.SessionEventType(e.Type),
		Step:    e.Step,
		Text:    e.Text,
		Tool:    e.Tool,
		Args:    e.Args,
		Result:  e.Result,
		CallID:  e.CallID,
		AgentID: e.AgentID,
		Name:    e.Name,
		Status:  agent.AgentStatus(e.Status),
		Kind:    agent.SubAgentKind(e.Kind),
		Plan:    e.Plan,
	}
	if e.Error != "" {
		ev.Err = errors.New(e.Error)
	}
	if e.Stats != nil {
		ev.Stats = agent.SessionStats{
			Turns:         e.Stats.Turns,
			TokensIn:      e.Stats.TokensIn,
			TokensOut:     e.Stats.TokensOut,
			ToolCalls:     e.Stats.ToolCalls,
			ContextTokens: e.Stats.ContextTokens,
			ContextWindow: e.Stats.ContextWindow,
		}
	}
	for _, t := range e.Todos {
		ev.Todos = append(ev.Todos, agent.TodoItem{Content: t.Content, Status: agent.TodoStatus(t.Status)})
	}
	return ev
}

// --- approvals --------------------------------------------------------------

// pollApprovals discovers pending interactive gates on the daemon and presents
// each one exactly once. The poller goroutine is the sole owner of the seen set,
// so the per-approval handler goroutines never touch shared state — keeping the
// path clean under -race. A handler blocks on the (modal) Approver and POSTs the
// decision; the approval then disappears from the daemon's list, at which point
// the poller forgets it.
func (rc *RemoteClient) pollApprovals() {
	ticker := time.NewTicker(rc.pollEvery)
	defer ticker.Stop()
	seen := make(map[string]bool)
	for {
		select {
		case <-rc.ctx.Done():
			return
		case <-ticker.C:
		}
		pending, err := rc.client.ListApprovals()
		if err != nil {
			continue // transient; try again next tick
		}
		present := make(map[string]bool, len(pending))
		for _, ap := range pending {
			present[ap.ID] = true
			if seen[ap.ID] {
				continue
			}
			seen[ap.ID] = true
			go rc.handleApproval(ap)
		}
		// Forget approvals that are gone (resolved or timed out) so a future
		// approval that happens to reuse an id is still presented.
		for id := range seen {
			if !present[id] {
				delete(seen, id)
			}
		}
	}
}

// handleApproval reconstructs the typed request from the wire view, presents it
// through the Approver (the Workbench modal), and round-trips the decision. It
// runs in its own goroutine so several concurrent gates can be queued; the
// Workbench serializes the modals itself.
func (rc *RemoteClient) handleApproval(ap ApprovalDTO) {
	switch ap.Kind {
	case "permission":
		if ap.Permission == nil {
			return
		}
		req := permission.Request{
			Action:   permission.Action(ap.Permission.Action),
			Resource: ap.Permission.Resource,
			Detail:   ap.Permission.Detail,
			Context:  permission.RequestContext{SessionID: ap.SessionID, Agent: ap.AgentID},
		}
		decision := rc.approver.AskPermission(req)
		// permission.Decision values ("allow"/"deny"/"always"/"always_deny") are
		// exactly the wire tokens the server's decision endpoint expects.
		rc.decide(ap.ID, string(decision))
	case "edit_review":
		if ap.EditReview == nil {
			return
		}
		req := gogent.EditReviewRequest{
			SessionID: ap.SessionID,
			AgentID:   ap.AgentID,
			Path:      ap.EditReview.Path,
			Op:        ap.EditReview.Op,
			Diff:      ap.EditReview.Diff,
		}
		rc.decide(ap.ID, editDecisionToWire(rc.approver.ReviewEdit(req)))
	}
}

// decide POSTs a resolved decision, logging (not surfacing) a failure: a 404/409
// means the gate was already resolved or timed out on the daemon, which is benign.
func (rc *RemoteClient) decide(aid, decision string) {
	if err := rc.client.DecideApproval(aid, decision); err != nil {
		log.Printf("remote approval %s: %v", aid, err)
	}
}

// editDecisionToWire maps an edit-review verdict to the server's wire token.
func editDecisionToWire(d gogent.EditReviewDecision) string {
	switch d {
	case gogent.EditApprove:
		return "approve"
	case gogent.EditApproveAll:
		return "approve_all"
	default:
		return "reject"
	}
}

// --- Handlers ---------------------------------------------------------------

// Handlers builds the Handlers struct that drives the daemon over HTTP/SSE. It
// populates only the fields that map to an existing /api endpoint; the attach
// wiring (cmd) fills the TUI-machine presentation handlers (theme, keybindings,
// layout, notifications, default model). Handlers with no daemon endpoint in this
// bounded slice — OnFork, OnRename (the window title still persists via the local
// layout), StreamThinking, the supervisor check, and the @-file workspace bridge
// — are intentionally left nil and degrade gracefully (the feature is simply
// unavailable while attached), as those API gaps are a later Phase-2/3 slice.
func (rc *RemoteClient) Handlers() Handlers {
	c := rc.client
	return Handlers{
		// OnCreate creates the daemon session under the TUI's window id so the two
		// stay in lock-step and the global SSE stream routes the session's events
		// to the matching window. It is synchronous (the user types next), so a
		// failure surfaces as an error in the window.
		OnCreate: func(sessionID, title string) {
			if _, err := c.CreateSession(sessionID, title, true); err != nil {
				rc.emitErr(sessionID, fmt.Errorf("create session: %w", err))
			}
		},
		// OnSend fires the turn on the daemon in the background; progress streams
		// back over the global SSE consumer into this session's window, exactly as
		// the embedded observer delivers it.
		OnSend: func(sessionID, message, modelName, effort string) {
			// The turn's lifetime belongs to the daemon: use a background context
			// (not rc.ctx) so detaching the TUI does not proactively cancel the
			// request. Cancellation comes from the session's own controls (OnStop →
			// POST /stop). Progress streams back over the global SSE consumer.
			go func() {
				if _, err := c.SendMessage(context.Background(), sessionID, message, modelName, effort); err != nil {
					rc.emitErr(sessionID, err)
				}
			}()
		},
		OnClose: func(sessionID string) {
			if err := c.DeleteSession(sessionID); err != nil {
				log.Printf("remote close session %s: %v", sessionID, err)
			}
		},
		OnStop: func(sessionID string) {
			if err := c.StopSession(sessionID); err != nil {
				log.Printf("remote stop session %s: %v", sessionID, err)
			}
		},
		OnInject: func(sessionID, message string) {
			if err := c.InjectSession(sessionID, message); err != nil {
				log.Printf("remote inject session %s: %v", sessionID, err)
			}
		},
		OnUndo:   func(sessionID string) (string, error) { return c.Undo(sessionID) },
		OnRewind: func(sessionID string, turns int) (string, error) { return c.Rewind(sessionID, turns) },
		OnSetPlanMode: func(sessionID string, on bool) {
			if err := c.SetPlanMode(sessionID, on); err != nil {
				log.Printf("remote plan-mode session %s: %v", sessionID, err)
			}
		},
		OnApprovePlan: func(sessionID string) {
			// Plan execution is a daemon-owned turn, like OnSend: background context.
			go func() {
				if err := c.ApprovePlan(context.Background(), sessionID); err != nil {
					rc.emitErr(sessionID, err)
				}
			}()
		},
		GetTranscript: func(sessionID, agentID string) []ChatMessage {
			msgs, err := c.GetTranscript(sessionID, agentID)
			if err != nil {
				return nil
			}
			return messageDTOsToChat(msgs)
		},
		// Restore reopens a window for each session the daemon currently holds live
		// (it restored them on its own startup). The shared "default" HTTP session
		// and the free-running watcher sessions are backend-only and get no window.
		Restore: func() []RestoredSession {
			sessions, err := c.ListSessions()
			if err != nil {
				log.Printf("remote restore: list sessions: %v", err)
				return nil
			}
			var out []RestoredSession
			for _, s := range sessions {
				if !s.Live || s.ID == "default" || strings.HasPrefix(s.ID, watcherSessionPrefix) {
					continue
				}
				title := s.Title
				if title == "" {
					title = s.ID
				}
				rs := RestoredSession{ID: s.ID, Title: title, Model: s.PrimaryModel}
				if msgs, err := c.GetTranscript(s.ID, "root"); err == nil {
					rs.Messages = messageDTOsToChat(msgs)
				}
				out = append(out, rs)
			}
			return out
		},

		// --- settings (one settingsView; setters read-modify-write) ---
		GetSettings: func() config.SubAgentConfig {
			s, err := c.GetSettings()
			if err != nil {
				return config.SubAgentConfig{}
			}
			return s.SubAgents
		},
		SetSettings: func(cfg config.SubAgentConfig) {
			rc.mutateSettings(func(s *SettingsDTO) { s.SubAgents = cfg })
		},
		GetTimeouts: func() config.TimeoutConfig {
			s, err := c.GetSettings()
			if err != nil {
				return config.TimeoutConfig{}
			}
			return s.Timeouts
		},
		SetTimeouts: func(t config.TimeoutConfig) {
			rc.mutateSettings(func(s *SettingsDTO) { s.Timeouts = t })
		},
		GetBudget: func() config.BudgetConfig {
			s, err := c.GetSettings()
			if err != nil {
				return config.BudgetConfig{}
			}
			return s.Budget
		},
		SetBudget: func(b config.BudgetConfig) {
			rc.mutateSettings(func(s *SettingsDTO) { s.Budget = b })
		},
		GetReviewEdits: func() bool {
			s, err := c.GetSettings()
			if err != nil {
				return false
			}
			return s.ReviewEdits
		},
		SetReviewEdits: func(enabled bool) {
			rc.mutateSettings(func(s *SettingsDTO) { s.ReviewEdits = enabled })
		},

		// --- models ---
		GetModels: func() []config.ModelConfig {
			models, err := c.ListModels()
			if err != nil {
				return nil
			}
			out := make([]config.ModelConfig, 0, len(models))
			for _, m := range models {
				out = append(out, m.ToModelConfig())
			}
			return out
		},
		UpdateModel: func(m config.ModelConfig) error { return c.UpdateModel(m) },
		ScanModels:  func(m config.ModelConfig) ([]string, error) { return c.ScanModels(m.Name) },

		// --- tools ---
		GetTools: func() []ToolInfo {
			tools, err := c.ListTools()
			if err != nil {
				return nil
			}
			out := make([]ToolInfo, 0, len(tools))
			for _, t := range tools {
				out = append(out, ToolInfo{
					Name:        t.Name,
					Description: t.Description,
					InputSchema: t.InputSchema,
					Enabled:     t.Enabled,
					Invocations: t.Invocations,
				})
			}
			return out
		},
		SetToolEnabled: func(name string, enabled bool) {
			if err := c.SetToolEnabled(name, enabled); err != nil {
				log.Printf("remote set tool %s enabled: %v", name, err)
			}
		},

		// --- skills ---
		GetSkills: func() []SkillInfo {
			skills, err := c.ListSkills()
			if err != nil {
				return nil
			}
			out := make([]SkillInfo, 0, len(skills))
			for _, s := range skills {
				out = append(out, SkillInfo{
					Name:        s.Name,
					Description: s.Description,
					Active:      s.Active,
					Success:     s.Success,
					Failure:     s.Failure,
					TotalCalls:  s.TotalCalls,
				})
			}
			return out
		},
		SetSkillActive: func(name string, active bool) {
			if err := c.SetSkillActive(name, active); err != nil {
				log.Printf("remote set skill %s active: %v", name, err)
			}
		},

		GetStatistics: func() stats.Report {
			r, err := c.GetStatistics()
			if err != nil {
				return stats.Report{}
			}
			return r
		},

		// --- watchers ---
		ListWatchers: func(sessionID string) []WatcherInfo {
			ws, err := c.ListWatchers(sessionID)
			if err != nil {
				return nil
			}
			out := make([]WatcherInfo, 0, len(ws))
			for _, w := range ws {
				out = append(out, watcherDTOToInfo(w))
			}
			return out
		},
		CreateWatcher: func(cfg WatcherConfig, sessionID string) (WatcherInfo, error) {
			enabled := true
			req := CreateWatcherRequest{
				Name:            cfg.Name,
				Task:            cfg.Task,
				Model:           cfg.Model,
				Enabled:         &enabled,
				Schedule:        config.ScheduleConfig{Every: cfg.Every, DailyAt: cfg.DailyAt, Timezone: cfg.Timezone},
				ReportToSession: cfg.ReportToSession,
			}
			w, err := c.CreateWatcher(req)
			if err != nil {
				return WatcherInfo{}, fmt.Errorf("create watcher: %w", err)
			}
			return watcherDTOToInfo(w), nil
		},
		EnableWatcher:  func(idOrName string) error { return c.SetWatcherEnabled(idOrName, true) },
		DisableWatcher: func(idOrName string) error { return c.SetWatcherEnabled(idOrName, false) },
		RunWatcher:     func(idOrName string) error { return c.RunWatcher(idOrName) },
		StopWatcher:    func(idOrName string) error { return c.StopWatcher(idOrName) },
		DeleteWatcher:  func(idOrName string) error { return c.DeleteWatcher(idOrName) },
	}
}

// emitErr surfaces a background-call failure as an error event in the session's
// window, mirroring how the embedded OnSend reports a failed turn.
func (rc *RemoteClient) emitErr(sessionID string, err error) {
	if rc.sink == nil {
		return
	}
	rc.sink(sessionID, agent.SessionEvent{Type: agent.SessionEventError, Err: err})
}

// mutateSettings applies a one-field change with a read-modify-write against
// PUT /api/settings (the server exposes a single settings block, so a setter
// must preserve the other fields).
func (rc *RemoteClient) mutateSettings(mutate func(*SettingsDTO)) {
	cur, err := rc.client.GetSettings()
	if err != nil {
		log.Printf("remote settings: read: %v", err)
		return
	}
	mutate(&cur)
	if err := rc.client.SetSettings(cur); err != nil {
		log.Printf("remote settings: write: %v", err)
	}
}

// messageDTOsToChat converts wire transcript messages to the UI's ChatMessage view.
func messageDTOsToChat(msgs []MessageDTO) []ChatMessage {
	out := make([]ChatMessage, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, ChatMessage{Role: m.Role, Content: m.Content})
	}
	return out
}

// watcherDTOToInfo maps a wire watcher view to the UI-facing WatcherInfo,
// resolving the reporting session id exactly as the embedded toWatcherInfo does:
// a free-running watcher reports into its dedicated watcher:<name> session, an
// attached one into its target session.
func watcherDTOToInfo(w WatcherDTO) WatcherInfo {
	free := w.Kind == "free"
	target := w.Target
	if target == "free" {
		target = ""
	}
	sessionID := target
	if free {
		sessionID = watcherSessionPrefix + w.Name
	}
	return WatcherInfo{
		ID:            w.ID,
		Name:          w.Name,
		Free:          free,
		TargetSession: target,
		SessionID:     sessionID,
		Enabled:       w.Enabled,
		Status:        w.Status,
		Running:       w.Status == "running",
		Task:          w.Task,
		Schedule:      w.Schedule,
		NextFire:      w.NextFire,
		LastRun:       w.LastRun,
		LastResult:    w.LastResult,
		LastError:     w.LastError,
	}
}
