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

// Reconnector is notified when the daemon connection drops and is re-established
// (issue #358 §7), so the attached TUI can present the BLOCKING disconnect modal
// and perform the jump-to-present refresh on reconnect. The *Workbench satisfies
// it. Either method may run from the RemoteClient's background goroutine, so an
// implementation marshals UI work onto the event-loop thread itself.
type Reconnector interface {
	// OnConnectionLost is called when the event stream drops and before each
	// backoff wait, with the 1-based reconnect attempt number, so the modal can
	// show "Reconnecting… (attempt N)".
	OnConnectionLost(attempt int)
	// OnConnectionRestored is called once the stream re-opens: the implementation
	// dismisses the modal and re-fetches state (a jump-to-present, not a replay).
	OnConnectionRestored()
}

// approvalPollInterval is how often the attached TUI polls GET /api/approvals for
// pending interactive gates. The current server announces approvals only via that
// list (no SSE push), so a short poll keeps remote prompts responsive without a
// new endpoint; it is cheap (a small JSON list) and bounded.
const approvalPollInterval = 750 * time.Millisecond

// watcherSessionPrefix mirrors the backend's session id for a free-running
// watcher's dedicated session ("watcher:<name>"). Those sessions are backend-only
// (no user window), so the session-restore/browse handlers filter them out of the
// session list.
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

	// reconnector, when set, is notified on disconnect/reconnect so the TUI shows
	// the blocking modal and re-fetches state (issue #358 §7). nil keeps the old
	// silent best-effort reconnect (used by narrow tests).
	reconnector Reconnector
	// retryNow collapses the current backoff wait when the user clicks "Retry now".
	// Buffered (cap 1) so a poke is never lost and RetryNow never blocks.
	retryNow chan struct{}
	// approvalKick forces an immediate /approvals re-fetch out of band of the poll
	// ticker. Reconnect signals it so pending approvals are re-fetched as part of
	// the jump-to-present (issue #358 §7) instead of waiting for the next tick.
	// Buffered (cap 1) so a kick is coalesced and never blocks.
	approvalKick chan struct{}
	// backoff returns the wait before the Nth (1-based) reconnect attempt. It is a
	// field so tests can shorten the schedule; production uses backoffFor.
	backoff func(attempt int) time.Duration

	// healthEvery, when > 0, runs a background /health ping at that interval so a
	// half-open/stalled SSE connection (which the stream read cannot detect) still
	// trips the disconnect/reconnect path (issue #358 §7). 0 disables it.
	healthEvery time.Duration
	// streamMu guards streamCancel, the cancel for the currently-open SSE stream so
	// the health monitor can drop a wedged stream and force a reconnect.
	streamMu     sync.Mutex
	streamCancel context.CancelFunc

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
		client:       client,
		sink:         sink,
		approver:     approver,
		ctx:          ctx,
		cancel:       cancel,
		pollEvery:    approvalPollInterval,
		retryNow:     make(chan struct{}, 1),
		approvalKick: make(chan struct{}, 1),
		backoff:      backoffFor,
	}
}

// SetReconnector installs the disconnect/reconnect observer (issue #358 §7),
// normally the *Workbench. It must be called before Start. With none set the
// client reconnects silently (no modal), preserving the simpler Phase-2 behaviour.
func (rc *RemoteClient) SetReconnector(r Reconnector) { rc.reconnector = r }

// SetHealthCheck enables a background /health ping at interval every (issue #358
// §7): a failed ping drops the current SSE stream so the consumer falls into the
// reconnect path even when the stream read itself is wedged on a half-open socket.
// It must be called before Start; a non-positive interval leaves it disabled.
func (rc *RemoteClient) SetHealthCheck(every time.Duration) { rc.healthEvery = every }

// RetryNow collapses the current reconnect backoff so the next attempt fires
// immediately. It backs the disconnect modal's "Retry now" button and is safe to
// call from any goroutine; a poke while not waiting is simply coalesced.
func (rc *RemoteClient) RetryNow() {
	select {
	case rc.retryNow <- struct{}{}:
	default:
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
			events, err := rc.openStream()
			if err != nil {
				startErr = fmt.Errorf("subscribe to daemon events: %w", err)
				return
			}
			go rc.consume(events)
			if rc.healthEvery > 0 {
				go rc.monitorHealth()
			}
		}
		if rc.approver != nil {
			go rc.pollApprovals()
		}
	})
	return startErr
}

// consume forwards the first (already-open) event stream into the sink, then on
// stream end drives the blocking disconnect/reconnect cycle until the context is
// cancelled. A deliberate shutdown (Close cancels the context) ends the loop
// without surfacing the modal.
func (rc *RemoteClient) consume(events <-chan GlobalEventDTO) {
	for {
		for ge := range events {
			rc.sink(ge.SessionID, eventDTOToSessionEvent(ge.Event))
		}
		// Stream ended (server closed it, or the health monitor cancelled it).
		// Release its context, then stop if we are shutting down, else reconnect.
		rc.dropStream()
		if rc.ctx.Err() != nil {
			return
		}
		next := rc.reconnect()
		if next == nil {
			return // context cancelled while reconnecting
		}
		events = next
	}
}

// openStream opens a fresh SSE stream under a child context whose cancel is stored
// so the health monitor can drop a wedged stream. The caller (consume) releases
// the context via dropStream when the stream ends.
func (rc *RemoteClient) openStream() (<-chan GlobalEventDTO, error) {
	streamCtx, cancel := context.WithCancel(rc.ctx)
	ch, err := rc.client.StreamEvents(streamCtx)
	if err != nil {
		cancel()
		return nil, err
	}
	rc.streamMu.Lock()
	rc.streamCancel = cancel
	rc.streamMu.Unlock()
	return ch, nil
}

// dropStream cancels the current SSE stream's context, ending its read. It is
// called by consume when a stream finishes (to release the context) and by the
// health monitor to force a reconnect on a stalled connection. Idempotent.
func (rc *RemoteClient) dropStream() {
	rc.streamMu.Lock()
	cancel := rc.streamCancel
	rc.streamCancel = nil
	rc.streamMu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// healthFailThreshold is how many consecutive failed /health pings force a
// reconnect. Requiring two avoids flashing the disconnect modal on a single
// transient blip while the SSE stream is actually fine, yet still catches a
// genuinely stalled connection within ~2 intervals.
const healthFailThreshold = 2

// monitorHealth pings the daemon's /health every rc.healthEvery and, after
// healthFailThreshold consecutive failures, drops the current SSE stream so
// consume falls into the reconnect path — catching a half-open/stalled connection
// the stream read alone would not (issue #358 §7). A single recovered ping resets
// the streak, so a momentary blip never surfaces the modal.
func (rc *RemoteClient) monitorHealth() {
	ticker := time.NewTicker(rc.healthEvery)
	defer ticker.Stop()
	fails := 0
	for {
		select {
		case <-rc.ctx.Done():
			return
		case <-ticker.C:
		}
		if err := rc.client.Health(); err != nil {
			if fails++; fails >= healthFailThreshold {
				rc.dropStream()
				fails = 0
			}
			continue
		}
		fails = 0
	}
}

// reconnect drives the disconnect/reconnect cycle (issue #358 §7). It tells the
// Reconnector the connection dropped — which raises the BLOCKING modal — then
// re-opens the SSE stream with exponential backoff (0.5s → 1s → 2s → 5s, capped
// ~10s) until it succeeds or the context is cancelled, collapsing the current
// wait when the user clicks "Retry now". On success it tells the Reconnector the
// connection is restored (the modal dismisses and the TUI re-fetches state) and
// returns the fresh stream; on cancellation it returns nil. Returning a brand-new
// StreamEvents — never a buffered backlog — is what makes the resume a
// jump-to-present rather than a replay.
func (rc *RemoteClient) reconnect() <-chan GlobalEventDTO {
	for attempt := 1; ; attempt++ {
		rc.notifyLost(attempt)
		select {
		case <-rc.ctx.Done():
			return nil
		case <-rc.retryNow:
		case <-time.After(rc.backoff(attempt)):
		}
		next, err := rc.openStream()
		if err == nil {
			// Jump-to-present: re-fetch full state. notifyRestored drives the
			// Workbench's /sessions + transcript refresh; kickApprovals re-fetches
			// /approvals now rather than on the next poll tick (issue #358 §7).
			rc.notifyRestored()
			rc.kickApprovals()
			return next
		}
		if rc.ctx.Err() != nil {
			return nil
		}
		// Still down: loop, showing the next attempt and backing off further.
	}
}

// notifyLost / notifyRestored forward to the Reconnector when one is installed.
func (rc *RemoteClient) notifyLost(attempt int) {
	if rc.reconnector != nil {
		rc.reconnector.OnConnectionLost(attempt)
	}
}

func (rc *RemoteClient) notifyRestored() {
	if rc.reconnector != nil {
		rc.reconnector.OnConnectionRestored()
	}
}

// backoffFor is the production reconnect schedule: 0.5s, 1s, 2s, 5s, then a 10s
// cap (issue #358 §7). attempt is 1-based.
func backoffFor(attempt int) time.Duration {
	switch {
	case attempt <= 1:
		return 500 * time.Millisecond
	case attempt == 2:
		return time.Second
	case attempt == 3:
		return 2 * time.Second
	case attempt == 4:
		return 5 * time.Second
	default:
		return 10 * time.Second
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
		case <-rc.approvalKick:
			// Forced re-fetch (reconnect jump-to-present): scan now rather than
			// waiting for the next tick.
		}
		rc.scanApprovals(seen)
	}
}

// scanApprovals fetches the daemon's pending gates once and presents any not yet
// seen, pruning resolved/timed-out ids from seen. It is the body shared by the
// poll ticker and the reconnect kick; the poller goroutine is the sole owner of
// seen, so this is only ever called from that one goroutine (no shared-state race).
func (rc *RemoteClient) scanApprovals(seen map[string]bool) {
	pending, err := rc.client.ListApprovals()
	if err != nil {
		return // transient; try again next tick/kick
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
	// Forget approvals that are gone (resolved or timed out) so a future approval
	// that happens to reuse an id is still presented.
	for id := range seen {
		if !present[id] {
			delete(seen, id)
		}
	}
}

// kickApprovals forces an immediate /approvals re-fetch by the poller. Reconnect
// calls it so pending approvals re-surface as part of the jump-to-present (issue
// #358 §7) instead of waiting for the poll ticker. Non-blocking and coalesced; a
// kick when no poller is running (no approver) is simply buffered and ignored.
func (rc *RemoteClient) kickApprovals() {
	select {
	case rc.approvalKick <- struct{}{}:
	default:
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
// layout, notifications, default model). Handlers explicitly deferred from this
// bounded slice — OnFork, OnRename (the window title still persists via the local
// layout), StreamThinking, the supervisor check, the @-file workspace bridge, and
// the watcher-management API — are intentionally left nil and degrade gracefully
// (the feature is simply unavailable while attached), as those are a later
// Phase-2/3 slice.
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
		// ListSavedSessions lists every session the daemon knows (live + saved) for
		// the Saved Sessions browser. The daemon addresses sessions by id, so the
		// id is carried as the File handle the browser later hands to
		// OpenSavedSession. A non-live persisted session is reported Archived (a
		// closed window), mirroring the embedded browser's archived marker.
		//
		// Deliberate bounded-slice limitations of this remote contract: SessionMeta
		// is degraded — File is the daemon's session id (not an on-disk path, which
		// is meaningless across the wire) and the per-session turn/message/token
		// counts are omitted, because GET /sessions carries only index metadata. An
		// archived (non-live) session is listed for parity with the embedded browser
		// but cannot be OPENED over the wire yet (OpenSavedSession returns ok=false):
		// reading a non-live session's persisted transcript needs a server endpoint
		// that is a later API-enrichment slice. Live sessions — every non-archived
		// session, which the daemon restores on startup — open normally.
		ListSavedSessions: func() []SessionMeta {
			sessions, err := c.ListSessions()
			if err != nil {
				return nil
			}
			out := make([]SessionMeta, 0, len(sessions))
			for _, s := range sessions {
				if s.ID == "default" || strings.HasPrefix(s.ID, watcherSessionPrefix) {
					continue
				}
				out = append(out, SessionMeta{
					ID:        s.ID,
					Title:     s.Title,
					CreatedAt: s.CreatedAt,
					Model:     s.PrimaryModel,
					File:      s.ID,
					Archived:  s.Persisted && !s.Live,
				})
			}
			return out
		},
		// OpenSavedSession opens one session by the id carried in File, fetching its
		// metadata + transcript over the wire. It supports sessions the daemon holds
		// live (the daemon restores all non-archived sessions on startup, so these
		// are exactly the ones worth opening when attached): for a continue the
		// session is already live so sends land, and for a read-only open the
		// workbench raises an analysis window. A non-live archived session has no
		// over-the-wire transcript yet (its persisted transcript is only readable
		// once restored live — a later API-enrichment slice), so the fetch fails and
		// this returns ok=false; the browser reports it could not be opened rather
		// than fabricating an empty live session on the daemon.
		OpenSavedSession: func(file string, continueSession bool) (RestoredSession, bool) {
			id := file
			meta, err := c.GetSession(id)
			if err != nil {
				return RestoredSession{}, false
			}
			msgs, err := c.GetTranscript(id, "root")
			if err != nil {
				return RestoredSession{}, false
			}
			title := meta.Title
			if title == "" {
				title = id
			}
			return RestoredSession{ID: id, Title: title, Messages: messageDTOsToChat(msgs), Model: meta.PrimaryModel}, true
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

		// --- custom commands (issue #403) ---
		// Mapped to the /api/commands endpoints so the editor, palette, slash
		// autocomplete and runtime dispatch all work while attached, exactly as
		// embedded. Read failures degrade to empty results; write failures surface as
		// errors the editor shows inline.
		// ReservedCommandNames uses the TUI-local mirror while attached (the built-in
		// set is static and identical to the daemon's command.ReservedNames).
		ReservedCommandNames: func() map[string]bool { return reservedBuiltinCommands },
		// OnSendCommand applies a custom command's overrides while attached, at full
		// parity with embedded: the daemon /messages endpoint carries agent/subtask, so
		// a subtask/agent invocation spawns a daemon-side sub-agent whose result streams
		// back over SSE. model rides the same call. Background context so detaching the
		// TUI does not cancel the daemon-side turn (cancellation is POST /stop).
		OnSendCommand: func(sessionID, message, modelName, agentName string, subtask bool, effort string) {
			go func() {
				if _, err := c.SendMessageWithOverrides(context.Background(), sessionID, message, modelName, agentName, subtask, effort); err != nil {
					rc.emitErr(sessionID, err)
				}
			}()
		},
		ListCommands: func() []CommandInfo {
			defs, err := c.ListCommands()
			if err != nil {
				return nil
			}
			out := make([]CommandInfo, 0, len(defs))
			for _, d := range defs {
				out = append(out, CommandInfo{Name: d.Name, Description: d.Description, Version: d.Version})
			}
			return out
		},
		GetCustomCommand: func(name string) (CommandDef, error) {
			d, err := c.GetCommand(name)
			if err != nil {
				return CommandDef{}, err
			}
			return commandDTOToDef(d), nil
		},
		CreateCommand: func(def CommandDef) error { return c.CreateCommand(commandDefToDTO(def)) },
		UpdateCommand: func(def CommandDef) error { return c.UpdateCommand(commandDefToDTO(def)) },
		DeleteCommand: func(name string) error { return c.DeleteCommand(name) },
		GetCommandHistory: func(name string) ([]CommandVersion, error) {
			vers, err := c.GetCommandHistory(name)
			if err != nil {
				return nil, err
			}
			out := make([]CommandVersion, 0, len(vers))
			for _, v := range vers {
				out = append(out, commandVersionDTOToVersion(v))
			}
			return out, nil
		},
		RestoreCommandVer: func(name string, v int) error { return c.RestoreCommandVersion(name, v) },

		// NOTE: the watcher handlers (ListWatchers/CreateWatcher/…) are deliberately
		// left nil. Watcher management over the wire is an explicitly deferred
		// Phase-3 API-gap item, out of scope for this bounded remote-client slice; an
		// attached TUI simply hides the watcher dialog/sidebar nodes until that slice
		// lands.
	}
}

// commandDTOToDef / commandDefToDTO map between the api_client wire DTO and the
// ui/tui-facing command type (issue #403). Versions are server-owned, so the
// editor never sends them back; commandDefToDTO omits them.
func commandDTOToDef(d CommandDTO) CommandDef {
	def := CommandDef{
		Name:        d.Name,
		Description: d.Description,
		Parameters:  commandParamsDTOToParams(d.Parameters),
		Template:    d.Template,
		Model:       d.Model,
		Agent:       d.Agent,
		Subtask:     d.Subtask,
		Version:     d.Version,
	}
	for _, v := range d.Versions {
		def.Versions = append(def.Versions, commandVersionDTOToVersion(v))
	}
	return def
}

func commandDefToDTO(def CommandDef) CommandDTO {
	return CommandDTO{
		Name:        def.Name,
		Description: def.Description,
		Parameters:  commandParamsToDTO(def.Parameters),
		Template:    def.Template,
		Model:       def.Model,
		Agent:       def.Agent,
		Subtask:     def.Subtask,
	}
}

func commandVersionDTOToVersion(v CommandVersionDTO) CommandVersion {
	return CommandVersion{
		Version:    v.Version,
		Template:   v.Template,
		Parameters: commandParamsDTOToParams(v.Parameters),
		Model:      v.Model,
		Agent:      v.Agent,
		Subtask:    v.Subtask,
		SavedAt:    v.SavedAt,
	}
}

func commandParamsDTOToParams(params []CommandParamDTO) []CommandParam {
	if params == nil {
		return nil
	}
	out := make([]CommandParam, len(params))
	for i, p := range params {
		out[i] = CommandParam(p) // identical fields; tags are ignored in conversion
	}
	return out
}

func commandParamsToDTO(params []CommandParam) []CommandParamDTO {
	if params == nil {
		return nil
	}
	out := make([]CommandParamDTO, len(params))
	for i, p := range params {
		out[i] = CommandParamDTO(p) // identical fields; tags are ignored in conversion
	}
	return out
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
