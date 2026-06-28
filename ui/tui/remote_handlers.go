package ui

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"gogent/internal/agent"
	"gogent/internal/config"
	"gogent/internal/gogent"
	"gogent/internal/modelsdev"
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

// restoreEagerTranscripts bounds how many of the most-recent live sessions have
// their transcript (and OnCreate POST) fetched up front on (re)connect (issue #517).
// Every other restored window opens deferred — a labelled shell with no up-front
// round-trip — and lazily fetches its transcript the first time it is focused. 20 is
// roughly what a user keeps visible at once, so first connect stays a small constant
// number of round-trips regardless of how many sessions the daemon holds.
const restoreEagerTranscripts = 20

// restoreMaxWindows hard-caps how many live-session windows Restore opens, so a
// daemon holding thousands of live sessions cannot make first connect build that
// many windows or ship a thousands-row /sessions body. Older live sessions beyond
// the cap stay reachable from the Saved Sessions browser; hitting the cap is logged,
// never silently swallowed.
const restoreMaxWindows = 200

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

	// disconnected is true while the daemon connection is down (set in notifyLost,
	// cleared in notifyRestored). Since a dispatched turn now outlives the request
	// (issue #481), a POST that fails because the connection dropped no longer means
	// the turn failed — it may still be running on the daemon. The send handlers
	// consult this to suppress a spurious "turn failed" error event while
	// disconnected; the blocking disconnect modal covers the UX instead.
	disconnected atomic.Bool

	// tunnel, when set (ssh:// attach, issue #482), is re-established at the top of
	// each reconnect attempt before re-opening the SSE stream: a dropped stream is
	// often a dropped SSH session. It is nil for the unix/http/https transports, so
	// their reconnect path is unchanged. The RemoteClient does NOT own the tunnel's
	// Close — cmd/attach.go does (single owner); this is only the Restart handle.
	tunnel TunnelRestarter

	startOnce sync.Once

	// initialEvents holds the SSE stream opened synchronously by StartGated so its
	// consumption can be deferred until the first Restore() completes (issue #516):
	// opening early preserves fail-fast, but draining it into the sink before restore
	// would flood the UI thread. nil once consumeOnce has launched consume.
	initialEvents <-chan GlobalEventDTO
	// consumeOnce guards the deferred launch of consume()+monitorHealth() so the
	// begin closure StartGated returns is idempotent and safe from any goroutine.
	consumeOnce sync.Once

	// wsMu guards wsRoot/wsFetching, the cache backing GetWorkspaceRoot (issue
	// #570). The status line reads GetWorkspaceRoot live on every refresh, so the
	// daemon's (immutable) workspace root is fetched once in the background and
	// cached here rather than round-tripped per refresh.
	wsMu sync.Mutex
	// wsRoot is the cached daemon workspace root, empty until the first successful
	// GET /api/workspace lands.
	wsRoot string
	// wsFetching is true while a background fetch is in flight, so concurrent
	// status refreshes coalesce onto one request.
	wsFetching bool

	// watchMu guards the watcher cache backing the ListWatchers handler (issue
	// #572). refreshWatcherNodes reads ListWatchers on the UI thread every 1s (once
	// per open session + once for the free set), so — exactly like wsRoot — the
	// per-tick read must be cheap and must NOT block the UI thread on the SSH
	// tunnel (a synchronous GET is bounded by quickTimeout = 30s). The handler
	// returns a cached snapshot and refreshes it off-thread.
	watchMu sync.Mutex
	// watchCache holds the last-known watcher list per query key (the session id,
	// or "" for the free set). Returned by cachedWatchers and replaced by
	// fetchWatchers / the mutation refresh.
	watchCache map[string][]WatcherInfo
	// watchFetching tracks which keys have a background fetch in flight, so
	// concurrent ticks coalesce onto one request per key.
	watchFetching map[string]bool
	// watchGen is an epoch counter bumped (under watchMu) by every successful
	// watcher mutation. A background fetch snapshots it at launch and commits its
	// result only if the epoch still matches, so a stale in-flight fetch that
	// started before a mutation cannot clobber the fresh post-mutation state the
	// mutation handler wrote synchronously.
	watchGen uint64
}

// TunnelRestarter re-establishes an out-of-process transport (the ssh:// tunnel)
// after a connection drop. Restart probes the existing session first and reports
// redialed=false when it was healthy (no teardown), or redialed=true when it
// actually replaced the session — so the caller only flushes pooled connections
// when the underlying transport changed. *sshtunnel.Tunnel satisfies it.
type TunnelRestarter interface {
	Restart(ctx context.Context) (redialed bool, err error)
}

// NewRemoteClient builds a RemoteClient over the given APIClient. sink receives
// every event from the daemon's global SSE stream (normally
// Workbench.EmitSessionEvent); approver presents interactive gates (normally the
// *Workbench). Either may be nil in narrow tests, in which case the respective
// background loop is skipped.
func NewRemoteClient(client *APIClient, sink EventSink, approver Approver) *RemoteClient {
	ctx, cancel := context.WithCancel(context.Background())
	return &RemoteClient{
		client:        client,
		sink:          sink,
		approver:      approver,
		ctx:           ctx,
		cancel:        cancel,
		pollEvery:     approvalPollInterval,
		retryNow:      make(chan struct{}, 1),
		approvalKick:  make(chan struct{}, 1),
		backoff:       backoffFor,
		watchCache:    make(map[string][]WatcherInfo),
		watchFetching: make(map[string]bool),
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

// SetTunnel installs the ssh:// tunnel's Restart handle (issue #482) so reconnect
// re-establishes the SSH session before re-subscribing. It must be called before
// Start. Leaving it unset (unix/http/https) keeps the reconnect path unchanged.
func (rc *RemoteClient) SetTunnel(t TunnelRestarter) { rc.tunnel = t }

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
//
// Start launches consumption immediately. The attach path uses StartGated instead,
// to defer consumption until the first Restore() completes (issue #516); Start
// stays for the embedded/test callers that want the stream live right away.
func (rc *RemoteClient) Start(parent context.Context) error {
	begin, err := rc.StartGated(parent)
	if err != nil {
		return err
	}
	begin()
	return nil
}

// StartGated performs the same fail-fast connect as Start — the SSE stream is
// opened synchronously so an unreachable/denied daemon aborts before the TUI ever
// launches — and starts the approvals poller, but it does NOT begin draining the
// stream into the sink. Instead it returns a begin closure that launches the
// consumer (and the health monitor) when called. The attach path calls begin only
// after the workbench's initial Restore() has finished, so live daemon events
// cannot flood the UI thread while Restore is still grinding through its slow
// sequential round-trips (issue #516).
//
// The stream being open-but-undrained between StartGated and begin is safe: the
// daemon hub delivers non-terminal events non-blocking with drop-on-full, so a
// momentarily-unread subscriber never back-pressures the daemon or other clients,
// and the bounded backlog drains once begin runs with the UI thread free.
//
// begin is idempotent (guarded by consumeOnce) and safe to call from any
// goroutine; a nil sink makes it a no-op. StartGated itself is idempotent — only
// the first call (or a prior Start) has effect, and a repeat returns a no-op begin.
func (rc *RemoteClient) StartGated(parent context.Context) (begin func(), err error) {
	begin = func() {}
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

		if rc.approver != nil {
			// Surface a freshly-raised remote prompt without waiting for the next poll
			// tick: the daemon pushes an "approval" SSE nudge on alloc, which we turn
			// into an immediate /approvals re-scan via the same coalesced kick reconnect
			// uses (issue #569). The poller stays the authoritative backstop; the seen
			// dedup keeps a push + a racing poll from double-presenting.
			//
			// Register the handler BEFORE openStream starts the SSE reader: a nudge that
			// arrives the instant the stream opens would otherwise hit a nil handler and
			// be dropped (the poll would still backstop it, but this closes the window).
			rc.client.SetApprovalSignalHandler(rc.kickApprovals)
			// A presented prompt that timed out before the user answered surfaces a
			// notice so a late click on the still-open dialog is not silently ignored
			// (issue #569). Registered before openStream for the same reason.
			rc.client.SetApprovalExpiredHandler(rc.noteApprovalExpired)
		}
		if rc.sink != nil {
			events, oerr := rc.openStream()
			if oerr != nil {
				startErr = fmt.Errorf("subscribe to daemon events: %w", oerr)
				return
			}
			rc.initialEvents = events
			// Defer consume + monitorHealth: monitorHealth must not run before the
			// stream is being drained, or it could dropStream() the stashed channel.
			begin = func() {
				rc.consumeOnce.Do(func() {
					go rc.consume(rc.initialEvents)
					rc.initialEvents = nil
					if rc.healthEvery > 0 {
						go rc.monitorHealth()
					}
				})
			}
		}
		if rc.approver != nil {
			go rc.pollApprovals()
		}
	})
	return begin, startErr
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

// StreamLogsTo drives the daemon's diagnostic-log stream into sink until ctx is
// cancelled (issue #562). Unlike the session-event consumer it reconnects
// SILENTLY — no blocking "connection lost" modal, no tunnel restart, no "retry
// now": a gap in a best-effort log tail is acceptable, and the session stream's
// own disconnect modal already covers a real outage. It reuses only the shared
// backoff schedule. The Logs window starts this in a goroutine on open and
// cancels ctx on close, so a closed window holds no daemon stream.
//
// Duplicate suppression has two layers: the resume cursor (?since=) tells a
// cooperating daemon to skip catch-up history the client already has, and a
// bounded client-side dedup guarantees no record reaches the window twice even if
// a server re-primes (defense-in-depth, independent of server behaviour). A fresh
// invocation (each window open) starts an empty dedup, so reopening re-shows
// history as expected.
func (rc *RemoteClient) StreamLogsTo(ctx context.Context, sink func(LogRecordDTO)) {
	attempt := 0
	since := "" // resume cursor: the last record's wire timestamp (issue #562)
	dedup := newLogDedup(logsDedupCap)
	for {
		ch, err := rc.client.StreamLogsSince(ctx, since)
		if err == nil {
			attempt = 0
			for rec := range ch {
				since = rec.Time // advance the cursor so a reconnect skips this record
				if dedup.seenAdd(rec) {
					continue // already delivered (e.g. a server that re-primed history)
				}
				sink(rec)
			}
		} else {
			attempt++
		}
		if ctx.Err() != nil {
			return
		}
		wait := rc.backoff(attempt)
		if attempt == 0 {
			// The stream opened then ended (not an open failure): retry promptly.
			wait = rc.backoff(1)
		}
		select {
		case <-time.After(wait):
		case <-ctx.Done():
			return
		}
	}
}

// logsDedupCap bounds the client-side dedup window for the [daemon] log stream
// (issue #562). It must exceed the daemon's log ring size (cmd.logRingSize, 2000)
// so any record the daemon could re-prime on reconnect is still remembered here;
// the margin tolerates a larger daemon ring. Beyond the cap the oldest keys evict,
// but those records have also rotated out of the daemon's ring, so they cannot be
// re-primed — the invariant (remember everything still re-primable) holds.
const logsDedupCap = 4096

// logDedup is a bounded set of recently-delivered daemon-log record identities. It
// is the client-side guard that keeps a reconnect from re-delivering a record into
// the Logs window even if the server re-primes it. It is NOT safe for concurrent
// use — driven only by the single StreamLogsTo loop.
type logDedup struct {
	seen map[string]struct{}
	keys []string // ring of keys in insertion order, for O(1) bounded eviction
	idx  int
}

func newLogDedup(n int) *logDedup {
	return &logDedup{seen: make(map[string]struct{}, n), keys: make([]string, n)}
}

// seenAdd reports whether rec was already delivered. If not, it records rec
// (evicting the oldest key when the ring is full) and returns false. The identity
// is (time, level, text): two genuinely-distinct records collide only if logged in
// the same nanosecond with identical level and text, which the diag path does not
// produce.
func (d *logDedup) seenAdd(rec LogRecordDTO) bool {
	key := rec.Time + "\x00" + rec.Level + "\x00" + rec.Text
	if _, ok := d.seen[key]; ok {
		return true
	}
	if old := d.keys[d.idx]; old != "" {
		delete(d.seen, old)
	}
	d.keys[d.idx] = key
	d.seen[key] = struct{}{}
	d.idx = (d.idx + 1) % len(d.keys)
	return false
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
		// For an ssh:// attach, a dropped stream is often a dropped SSH session, so
		// re-establish the tunnel before re-subscribing (issue #482). Restart probes
		// first and is a near-no-op when the session is healthy; a genuine failure
		// feeds this same backoff. Only when it actually redialed do we flush pooled
		// channels bound to the now-replaced session.
		if rc.tunnel != nil {
			redialed, terr := rc.tunnel.Restart(rc.ctx)
			if terr != nil {
				if rc.ctx.Err() != nil {
					return nil
				}
				continue // tunnel still down: next attempt, longer backoff, modal stays up
			}
			if redialed {
				rc.client.CloseIdleConnections()
			}
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

// notifyLost / notifyRestored forward to the Reconnector when one is installed and
// track the connection state so the send handlers can tell a real dispatch failure
// from a POST that merely failed because the link dropped (issue #481).
func (rc *RemoteClient) notifyLost(attempt int) {
	rc.disconnected.Store(true)
	if rc.reconnector != nil {
		rc.reconnector.OnConnectionLost(attempt)
	}
}

func (rc *RemoteClient) notifyRestored() {
	rc.disconnected.Store(false)
	if rc.reconnector != nil {
		rc.reconnector.OnConnectionRestored()
	}
}

// emitSendErr reports a failed send/approve/command POST, EXCEPT while the
// connection is down (issue #481): a turn dispatched on the daemon survives a
// client disconnect, so a connection-level POST failure no longer implies the turn
// failed. While disconnected it is logged and the disconnect modal carries the UX;
// when connected it surfaces as an error event in the session window as before.
func (rc *RemoteClient) emitSendErr(sessionID string, err error) {
	if rc.disconnected.Load() {
		log.Printf("remote send for session %s failed while disconnected (turn may still be running on daemon): %v", sessionID, err)
		return
	}
	rc.emitErr(sessionID, err)
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
		TurnID:  e.TurnID,
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

// noteApprovalExpired surfaces an in-window notice when a presented approval timed
// out on the daemon before the user answered (issue #569). The agent already
// received the safe default and the turn moved on, but the dialog may still be open
// on the user's screen; without this the user could click "Allow" and have it
// silently do nothing. The notice is cause-accurate — the daemon emits the expired
// signal ONLY on a genuine timeout (not when another client answered) — so it can
// state plainly that the prompt timed out and the safe default was applied. A nil
// sink (narrow tests) is a no-op, mirroring emitNotice.
func (rc *RemoteClient) noteApprovalExpired(d ApprovalExpiredDTO) {
	rc.emitNotice(d.SessionID, "This approval prompt timed out before it was answered, so the tool used the safe default. If the dialog is still open, answering it now will have no effect.")
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
		wire := string(decision)
		status, err := rc.decide(ap.ID, wire)
		rc.reportDecision(ap.SessionID, "permission", req.Resource, wire, status, err)
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
		wire := editDecisionToWire(rc.approver.ReviewEdit(req))
		status, err := rc.decide(ap.ID, wire)
		rc.reportDecision(ap.SessionID, "edit_review", req.Path, wire, status, err)
	}
}

// decideRetryBackoff is the wait before each retry of a failed decision POST. The
// endpoint is idempotent (issue #560), so a blind retry can never double-apply a
// decision; it just rides out a transient network blip or 5xx. The length of the
// slice is the number of retries after the first attempt.
var decideRetryBackoff = []time.Duration{200 * time.Millisecond, 500 * time.Millisecond}

// decide POSTs a resolved decision to the daemon, retrying a transient failure a
// couple of times before giving up. It returns the daemon's status ("resolved"
// for an in-time decision, "late" for a reconciled one) and the final error after
// retries. A failure is still logged for the post-incident record — now to the
// diagnostics file, never the alternate screen (issue #560) — but the caller
// (reportDecision) is responsible for surfacing a definitive failure (or a late
// grant) to the user, so the decision is never silently lost.
func (rc *RemoteClient) decide(aid, decision string) (status string, err error) {
	for attempt := 0; ; attempt++ {
		if attempt > 0 {
			select {
			case <-rc.ctx.Done():
				return "", fmt.Errorf("remote approval %s: %w", aid, rc.ctx.Err())
			case <-time.After(decideRetryBackoff[attempt-1]):
			}
		}
		status, err = rc.client.DecideApproval(aid, decision)
		if err == nil {
			return status, nil
		}
		if attempt >= len(decideRetryBackoff) {
			break
		}
	}
	log.Printf("remote approval %s: %v", aid, err)
	return "", err
}

// reportDecision surfaces a remote approval outcome to the user in-band so it is
// never silent (issue #560): a definitive delivery failure becomes a kind-aware
// "[System]" note, and a sticky permission grant that the daemon reconciled after
// the prompt had already expired ("late") tells the user it will apply going
// forward. The common in-time success is silent.
//
// A late ONE-SHOT decision (allow/deny, approve/reject) stays silent by design
// (issue #560): "late" means the daemon had already removed the prompt — it timed
// out OR another attached window answered it first — and a one-shot carries no
// future effect, so a notice would be noise. With issue #569 the daemon no longer
// auto-denies a prompt before any client has surfaced it, so this residual late
// path is the rare post-presentation timeout, left silent to preserve #560.
func (rc *RemoteClient) reportDecision(sessionID, kind, resource, wire, status string, err error) {
	if err != nil {
		switch kind {
		case "edit_review":
			rc.emitNotice(sessionID, "Couldn't deliver your edit-review decision to the daemon; the edit was not applied — please try again.")
		default:
			rc.emitNotice(sessionID, "Couldn't deliver your approval to the daemon; the tool used the safe default. 'Always allow' did not take effect — please try again.")
		}
		return
	}
	if status == "late" && kind == "permission" && (wire == "always" || wire == "always_deny") {
		verb := "allow"
		if wire == "always_deny" {
			verb = "deny"
		}
		// "late" means the original prompt had already closed (timed out, or another
		// attached client answered it) before this decision arrived, so do not claim
		// the request used the safe default — that is only true for the timeout case.
		// State just what is certain: the sticky grant was saved and applies going
		// forward.
		rc.emitNotice(sessionID, fmt.Sprintf("Your 'always %s' for %s was saved after the original prompt closed; it will apply to future requests.", verb, resource))
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

// --- workspace root ---------------------------------------------------------

// cachedWorkspaceRoot returns the daemon's workspace root for the status-line
// path affordance (issue #570), fetching it once in the background and caching
// it. It backs the GetWorkspaceRoot handler, which the status line reads live on
// every refresh, so it must be cheap and must NOT block the UI thread: the first
// call kicks an async GET /api/workspace and returns "" (the status line simply
// omits the path, the documented nil-safe behaviour), and once the fetch lands
// every later call returns the cached root. A failed fetch is not cached, so a
// later refresh retries — covering a transient blip at attach time. The daemon
// root is immutable for the daemon's lifetime, so a single successful fetch is
// authoritative.
func (rc *RemoteClient) cachedWorkspaceRoot() string {
	rc.wsMu.Lock()
	defer rc.wsMu.Unlock()
	if rc.wsRoot != "" {
		return rc.wsRoot
	}
	if !rc.wsFetching {
		rc.wsFetching = true
		go rc.fetchWorkspaceRoot()
	}
	return ""
}

// fetchWorkspaceRoot performs the one background GET /api/workspace and caches a
// non-empty root. It deliberately acquires wsMu only AFTER the HTTP call returns,
// so the UI thread never blocks on the network while holding the lock; a failed
// or empty fetch clears the in-flight flag so a later refresh retries.
func (rc *RemoteClient) fetchWorkspaceRoot() {
	ws, err := rc.client.Workspace()
	rc.wsMu.Lock()
	defer rc.wsMu.Unlock()
	rc.wsFetching = false
	if err == nil && ws.Root != "" {
		rc.wsRoot = ws.Root
	}
}

// --- watcher cache (issue #572) ---------------------------------------------

// cachedWatchers backs the ListWatchers handler. It returns the last-known
// watcher list for the query key (sessionID, or "" for the free set) WITHOUT
// blocking the UI thread: refreshWatcherNodes calls this on the 1s status tick,
// and a synchronous GET would freeze the UI for up to quickTimeout on a stalled
// SSH tunnel. It mirrors cachedWorkspaceRoot — the first call returns nil and
// kicks an off-thread fetch, so the node appears on the next tick (watcher nodes
// already only move on the 1s tick). A snapshot copy is returned so the caller
// never aliases the cached slice.
func (rc *RemoteClient) cachedWatchers(sessionID string) []WatcherInfo {
	rc.watchMu.Lock()
	defer rc.watchMu.Unlock()
	if !rc.watchFetching[sessionID] {
		rc.watchFetching[sessionID] = true
		gen := rc.watchGen
		go rc.fetchWatchers(sessionID, gen)
	}
	cached := rc.watchCache[sessionID]
	if cached == nil {
		return nil
	}
	return append([]WatcherInfo(nil), cached...)
}

// fetchWatchers performs one background GET /api/watchers[?session_id=] and
// updates the cache. Like fetchWorkspaceRoot it acquires watchMu only AFTER the
// HTTP call returns, so the UI thread never blocks on the network while holding
// the lock, and it clears the in-flight flag via defer so a fetch can never
// freeze a key's cache. It commits its result only if the epoch still matches
// (gen == watchGen): a fetch that started before a mutation bumped the epoch read
// pre-mutation state, so it is discarded rather than clobbering the fresh state
// the mutation handler wrote synchronously. On error the last-good slice is kept
// (no flicker on a transient blip); the next tick retries.
func (rc *RemoteClient) fetchWatchers(sessionID string, gen uint64) {
	dtos, err := rc.client.ListWatchers(sessionID)
	rc.watchMu.Lock()
	defer rc.watchMu.Unlock()
	defer func() { rc.watchFetching[sessionID] = false }()
	if err != nil || gen != rc.watchGen {
		return
	}
	infos := make([]WatcherInfo, 0, len(dtos))
	for _, d := range dtos {
		infos = append(infos, watcherDTOToInfo(d))
	}
	rc.watchCache[sessionID] = infos
}

// invalidateWatchers is called by every watcher mutation handler on success. It
// bumps the epoch (dropping any in-flight background fetch that would otherwise
// land with pre-mutation data) and synchronously re-fetches every key currently
// in the cache, so the dialog's post-action loadWatcherItems re-render reads the
// fresh state. The synchronous network here is acceptable: the user just clicked
// and the mutation itself already blocked. Keys are bounded by "" + the open
// sessions, so the refresh is small.
func (rc *RemoteClient) invalidateWatchers() {
	rc.watchMu.Lock()
	rc.watchGen++
	keys := make([]string, 0, len(rc.watchCache))
	for k := range rc.watchCache {
		keys = append(keys, k)
	}
	rc.watchMu.Unlock()
	for _, k := range keys {
		dtos, err := rc.client.ListWatchers(k)
		if err != nil {
			continue
		}
		infos := make([]WatcherInfo, 0, len(dtos))
		for _, d := range dtos {
			infos = append(infos, watcherDTOToInfo(d))
		}
		rc.watchMu.Lock()
		rc.watchCache[k] = infos
		rc.watchMu.Unlock()
	}
}

// watcherDTOToInfo maps a wire WatcherDTO to the ui/tui WatcherInfo, reproducing
// the embedded mapper (cmd/main.go toWatcherInfo) from the wire shape: a
// free-running watcher reports its own watcher:<name> session as SessionID, an
// attached one reports its target; Running tracks the "running" status; the
// RFC3339 timestamps are reformatted to the embedded "2006-01-02 15:04" display
// form (kept raw if unparseable). All other fields pass through verbatim.
func watcherDTOToInfo(d WatcherDTO) WatcherInfo {
	free := d.Kind == "free"
	targetSession := ""
	sessionID := watcherSessionPrefix + d.Name
	if !free {
		targetSession = d.Target
		sessionID = d.Target
	}
	out := WatcherInfo{
		ID:            d.ID,
		Name:          d.Name,
		Free:          free,
		TargetSession: targetSession,
		SessionID:     sessionID,
		Enabled:       d.Enabled,
		Status:        d.Status,
		Running:       d.Status == "running",
		Task:          d.Task,
		Schedule:      d.Schedule,
		NextFire:      reformatWatcherTime(d.NextFire),
		LastRun:       reformatWatcherTime(d.LastRun),
		LastResult:    d.LastResult,
		LastError:     d.LastError,
	}
	return out
}

// reformatWatcherTime converts a wire RFC3339 timestamp to the embedded display
// form ("2006-01-02 15:04"). An empty string stays empty; an unparseable value is
// returned verbatim so a server format change degrades to showing the raw string
// rather than dropping the field.
func reformatWatcherTime(s string) string {
	if s == "" {
		return ""
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return s
	}
	return t.Format("2006-01-02 15:04")
}

// --- Handlers ---------------------------------------------------------------

// Handlers builds the Handlers struct that drives the daemon over HTTP/SSE. It
// populates only the fields that map to an existing /api endpoint; the attach
// wiring (cmd) fills the TUI-machine presentation handlers (theme, keybindings,
// layout, notifications). The default model is daemon-owned (issue #507) and so is
// wired HERE (over /api/settings), not by the attach wiring. Handlers explicitly
// deferred from this
// bounded slice — OnFork, OnRename (the window title still persists via the local
// layout), StreamThinking, the supervisor check, and the @-file workspace bridge
// — are intentionally left nil and degrade gracefully (the feature is simply
// unavailable while attached), as those are a later Phase-2/3 slice. The
// watcher-management API IS wired (issue #572): see the watcher handlers below.
func (rc *RemoteClient) Handlers() Handlers {
	c := rc.client
	// The models.dev catalog is public data fetched directly by the attached
	// client (cache lives on the client host); AddModel still mutates the DAEMON's
	// config via POST /models. A missing HOME degrades the cache to the cwd.
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		home = "."
	}
	mdc := modelsdev.NewClient(home)
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
					rc.emitSendErr(sessionID, err)
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
		// OnShell runs a !-prefixed command on the daemon at its workspace root
		// (issue #571), out-of-band from any agent turn. A background context is
		// used deliberately: the request's only bound is the daemon-side shell
		// timeout (not the client's 30s quickTimeout), so an attached !cmd honours
		// the same 5-minute ceiling as the embedded path.
		OnShell: func(command string) (ShellResult, error) {
			dto, err := c.Shell(context.Background(), command)
			if err != nil {
				return ShellResult{}, err
			}
			return ShellResult{
				Stdout:   dto.Stdout,
				Stderr:   dto.Stderr,
				ExitCode: dto.ExitCode,
				Timeout:  dto.Timeout,
			}, nil
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
					rc.emitSendErr(sessionID, err)
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
		//
		// Restore is bounded (issue #517): it asks the daemon for only the most-recent
		// live sessions (live=true excludes archived, limit caps the window count and
		// shrinks the wire body), then eagerly fetches the transcript for just the
		// first restoreEagerTranscripts of them. The rest are returned Deferred — the
		// TUI opens them as labelled shells and fetches each transcript once, lazily,
		// on first focus — so first connect costs one list call plus a small, constant
		// number of transcript round-trips rather than one per session.
		Restore: func() []RestoredSession {
			sessions, err := c.ListSessionsBounded(true, restoreMaxWindows, 0)
			if err != nil {
				log.Printf("remote restore: list sessions: %v", err)
				return nil
			}
			if len(sessions) == restoreMaxWindows {
				log.Printf("remote restore: restored the %d most-recent live sessions; "+
					"older ones are available from Saved Sessions", restoreMaxWindows)
			}
			out := make([]RestoredSession, 0, len(sessions))
			eager := 0
			for _, s := range sessions {
				if !s.Live || s.ID == "default" || strings.HasPrefix(s.ID, watcherSessionPrefix) {
					continue
				}
				title := s.Title
				if title == "" {
					title = s.ID
				}
				rs := RestoredSession{ID: s.ID, Title: title, Model: s.PrimaryModel}
				// The server already ordered the live set most-recent-first, so the
				// first restoreEagerTranscripts entries are the ones worth loading now;
				// the remainder defer their transcript fetch to first focus.
				if eager < restoreEagerTranscripts {
					eager++
					if msgs, err := c.GetTranscript(s.ID, "root"); err == nil {
						rs.Messages = messageDTOsToChat(msgs)
					} else {
						// A failed eager fetch must not leave a silently-empty window:
						// degrade it to a deferred shell so it shows the placeholder and
						// retries the transcript on first focus, instead of opening blank.
						rs.Deferred = true
					}
				} else {
					rs.Deferred = true
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

		// GetWorkspaceRoot reports the DAEMON's workspace root — where ! shell
		// commands and the agent's shell tool calls actually run — so the attached
		// status line shows the same working-directory path the local TUI does
		// (issue #570). It is daemon-owned (like the default model, #507), so it is
		// wired HERE over GET /api/workspace rather than by the attach layer's
		// installPresentationHandlers, whose local g would report the CLIENT cwd.
		// Cached + non-blocking: the value is fetched once in the background, so the
		// per-refresh read stays cheap and never stalls the UI on the SSH tunnel.
		GetWorkspaceRoot: rc.cachedWorkspaceRoot,

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
		// Default model is daemon-owned (issue #507): the dropdown is populated from
		// the daemon's models, so the default must be resolved against — and persisted
		// to — the daemon, not the client's config. Get rides GET /settings like budget.
		GetDefaultModel: func() string {
			s, err := c.GetSettings()
			if err != nil {
				return ""
			}
			return s.DefaultModel
		},
		// SetDefaultModel does its OWN read-modify-write (rather than the error-
		// swallowing rc.mutateSettings) because its Handlers signature returns an error
		// the model editor surfaces: an invalid name comes back from the daemon as a
		// 400, which APIClient.SetSettings turns into a Go error propagated here.
		SetDefaultModel: func(name string) error {
			cur, err := c.GetSettings()
			if err != nil {
				return fmt.Errorf("read settings: %w", err)
			}
			cur.DefaultModel = name
			return c.SetSettings(cur)
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
		AddModel:    func(m config.ModelConfig) error { return c.AddModel(m) },
		RemoveModel: func(name string) error { return c.RemoveModel(name) },
		GetModelCatalog: func(ctx context.Context, force bool) (modelsdev.Catalog, error) {
			return mdc.Catalog(ctx, force)
		},

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
					rc.emitSendErr(sessionID, err)
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

		// Watchers (issue #329 daemon API, remote wiring issue #572): the dialog,
		// the ◷ sidebar nodes and /watcher drive the daemon's /api/watchers surface,
		// mirroring the embedded handler set (cmd/embedded_handlers.go). ListWatchers
		// reads the non-blocking cache (refreshWatcherNodes calls it on the 1s UI
		// tick); every mutation invalidates that cache so the dialog re-renders fresh.
		// Enable/Disable are the two directions of SetWatcherEnabled. CreateWatcher
		// forwards cfg.ReportToSession (nil ⇒ free-running, a live session id ⇒
		// attached) — the daemon decides the kind — so the calling sessionID is unused
		// over the wire, matching the daemon's tool create path.
		ListWatchers: func(sessionID string) []WatcherInfo {
			return rc.cachedWatchers(sessionID)
		},
		CreateWatcher: func(cfg WatcherConfig, _ string) (WatcherInfo, error) {
			enabled := true
			req := WatcherCreateDTO{
				Name:            cfg.Name,
				Task:            cfg.Task,
				Model:           cfg.Model,
				Schedule:        config.ScheduleConfig{Every: cfg.Every, DailyAt: cfg.DailyAt, Timezone: cfg.Timezone},
				Enabled:         &enabled,
				ReportToSession: cfg.ReportToSession,
			}
			dto, err := c.CreateWatcher(req)
			if err != nil {
				return WatcherInfo{}, fmt.Errorf("create watcher: %w", err)
			}
			rc.invalidateWatchers()
			return watcherDTOToInfo(dto), nil
		},
		EnableWatcher: func(idOrName string) error {
			if err := c.SetWatcherEnabled(idOrName, true); err != nil {
				return err
			}
			rc.invalidateWatchers()
			return nil
		},
		DisableWatcher: func(idOrName string) error {
			if err := c.SetWatcherEnabled(idOrName, false); err != nil {
				return err
			}
			rc.invalidateWatchers()
			return nil
		},
		RunWatcher: func(idOrName string) error {
			if err := c.RunWatcher(idOrName); err != nil {
				return err
			}
			rc.invalidateWatchers()
			return nil
		},
		StopWatcher: func(idOrName string) error {
			if err := c.StopWatcher(idOrName); err != nil {
				return err
			}
			rc.invalidateWatchers()
			return nil
		},
		DeleteWatcher: func(idOrName string) error {
			if err := c.DeleteWatcher(idOrName); err != nil {
				return err
			}
			rc.invalidateWatchers()
			return nil
		},
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

// emitNotice surfaces an informational system note in the session's window
// (rendered as a "[System]" line), used to tell the user about a remote approval
// outcome that would otherwise be silent — a delivery failure or a late "always"
// grant (issue #560). A nil sink (narrow tests) is a no-op, mirroring emitErr.
func (rc *RemoteClient) emitNotice(sessionID, text string) {
	if rc.sink == nil {
		return
	}
	rc.sink(sessionID, agent.SessionEvent{Type: agent.SessionEventNotice, Text: text})
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
