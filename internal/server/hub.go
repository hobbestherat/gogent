package server

import (
	"strings"
	"sync"
	"time"

	"gogent/internal/agent"
)

// taggedEvent is an agent SessionEvent annotated with its originating session id.
// Per-session subscribers drop the tag (they know the session); global
// subscribers need it to route the event.
type taggedEvent struct {
	sessionID string
	ev        agent.SessionEvent
	// notif, when non-nil, marks this frame as a backend notification (issue #358
	// §9) rather than a session event. It rides the same global subscriber
	// channels (no separate channel) and is emitted as an SSE event named
	// "notification"; per-session subscribers never receive it.
	notif *NotificationEvent
	// approvalSignal marks this frame as a best-effort "approval pending" nudge
	// (issue #569): it carries no event payload, only approvalID for diagnostics,
	// and tells a connected client to re-fetch GET /approvals now rather than wait
	// for its next poll tick. Like notif it rides the global channels and is
	// emitted under the SSE event name "approval"; per-session subscribers never
	// receive it.
	//
	// approvalExpired marks this frame as a "presented approval timed out" signal
	// (issue #569): a prompt the user had been shown reached its auto-deny bound
	// before being answered, so the safe default was applied. It carries approvalID
	// and sessionID (reusing the sessionID field) so a connected client can tell the
	// user, in the right window, that a late answer no longer applies. Emitted under
	// the SSE event name "approval_expired"; global subscribers only.
	//
	// A frame is exactly one of: session event, notif, approvalSignal, or
	// approvalExpired.
	approvalSignal  bool
	approvalExpired bool
	approvalID      string
}

// notificationRingSize bounds the missed-notification ring (issue #358 Open
// Decision #5): while no client is connected the daemon buffers at most this many
// recent notifications, dropping the oldest past the bound; a reconnecting client
// drains them. Kept small — it is a catch-up convenience, not a durable log.
const notificationRingSize = 50

// hub fans live agent.SessionEvents out to SSE subscribers. Each session has its
// own set of subscribers (for /sessions/:id/events), and there is one global set
// (for /events, which carries the session id on every event). The server wires a
// session's SetObserver to hub.sessionObserver(id) when it creates or loads a
// session, so the same typed event stream the TUI consumes is delivered to API
// clients unchanged.
type hub struct {
	mu sync.Mutex

	// per-session subscribers: session id -> set of channels.
	session map[string]map[chan taggedEvent]struct{}
	// global subscribers receive every event (tagged with its session id).
	global map[chan taggedEvent]struct{}
	// ring buffers notifications that fired while no client was connected, bounded
	// to notificationRingSize (oldest dropped). A reconnecting global subscriber
	// drains it (issue #358 §9). Guarded by mu like the subscriber maps.
	ring []NotificationEvent
	// now stamps notification timestamps. Defaults to time.Now (newHub) and is
	// replaced with the server's injectable clock in NewServer.
	now func() time.Time
	// agentGate / agentLocal configure the agent-completion notification path
	// (issue #358 §9), the mirror of NotificationSink's (gate, local) for the
	// watcher path: agentGate decides whether a reason is notified at all (gating
	// the SSE frame, the replay buffer AND the local fallback together), and
	// agentLocal is the daemon-local fallback used when no client is connected.
	// Both are wired only by the daemon (Server.EnableAgentNotificationFallback);
	// embedded mode leaves them nil so the in-process TUI's own notifier surfaces
	// completions and no background terminal escapes are written to os.Stdout.
	// Guarded by mu.
	agentGate  NotifyGateFunc
	agentLocal NotifyLocalFunc
}

func newHub() *hub {
	return &hub{
		session: make(map[string]map[chan taggedEvent]struct{}),
		global:  make(map[chan taggedEvent]struct{}),
		now:     time.Now,
	}
}

// sessionObserver returns a SessionObserver that fans this session's events to
// its session subscribers and to the global subscribers. It is what the server
// installs via UserSession.SetObserver so a running turn's events reach the API.
func (h *hub) sessionObserver(sessionID string) agent.SessionObserver {
	return func(ev agent.SessionEvent) {
		h.deliver(sessionID, ev)
	}
}

// deliver sends ev to the session's subscribers and the global ones. Non-terminal
// events are non-blocking (a slow/full subscriber is dropped rather than stalling
// the agent loop — the stream is "best-effort live", not lossless). Terminal
// events (final/error/plan) get a short blocking send so a momentarily-full
// buffer doesn't drop them; if still full after the grace period they are dropped
// rather than stalling the agent loop on a dead consumer.
func (h *hub) deliver(sessionID string, ev agent.SessionEvent) {
	te := taggedEvent{sessionID: sessionID, ev: ev}
	h.mu.Lock()
	subs := h.cloneSubs(h.session[sessionID])
	glob := h.cloneSubs(h.global)
	h.mu.Unlock()

	terminal := isTerminal(ev)
	send := func(ch chan taggedEvent) {
		if terminal {
			// Terminal events get a brief blocking attempt: a live consumer
			// drains quickly; a dead one never unsubscribed yet is bounded by
			// the timeout so we never wedge the agent loop.
			select {
			case ch <- te:
			case <-time.After(250 * time.Millisecond):
			}
			return
		}
		select {
		case ch <- te:
		default:
		}
	}
	for _, ch := range subs {
		send(ch)
	}
	for _, ch := range glob {
		send(ch)
	}

	// Backend notification (issue #358 §9): a completion/attention event (final,
	// error, sub-agent CLARIFY) also raises a NotificationEvent on the global stream
	// so a connected TUI surfaces it on its own machine and a reconnecting one
	// replays what it missed. Watcher-session completions are excluded: the watcher
	// manager raises its own "watcher" notification for those, so emitting here too
	// would double up.
	if !strings.HasPrefix(sessionID, watcherSessionIDPrefix) {
		if nev, ok := notificationForEvent(sessionID, ev, h.now); ok {
			h.emitAgentNotification(nev)
		}
	}
}

// emitAgentNotification routes an agent-completion notification through the same
// gate → broadcast-or-buffer → local-fallback pipeline the watcher path uses
// (NotificationSink), so the same notify.Reason config governs both (issue #358
// §9). agentGate (when set) decides whether the reason notifies at all: a disabled
// reason emits nothing — no SSE frame, no replay buffer, no local fallback. An
// allowed notification is delivered live to connected clients, or buffered for
// replay and delivered to the daemon-local fallback when none is connected.
func (h *hub) emitAgentNotification(nev NotificationEvent) {
	h.mu.Lock()
	gate := h.agentGate
	local := h.agentLocal
	h.mu.Unlock()
	if gate != nil && !gate(nev.Reason) {
		return
	}
	if h.deliverNotification(nev) {
		return // delivered live to >=1 connected client
	}
	if local != nil {
		local(nev.Title, nev.Body) // no client connected: notify on the daemon's host
	}
}

// deliverNotification routes a backend notification onto the global SSE stream
// when it reaches at least one connected client, or buffers it in the bounded
// missed-notification ring otherwise (issue #358 §9). The send and the
// buffer-decision happen together under the hub lock, so a notification racing a
// (dis)connect is atomically either broadcast or buffered — never both, never
// lost. It returns true when the notification was delivered live to >=1
// subscriber, false when it was buffered for replay (so the caller knows to use
// the local fallback). Sends are non-blocking, but unlike a streaming event a
// notification is not silently dropped on a full buffer: if NO subscriber accepts
// it (none connected, or every subscriber's buffer is full) it falls through to
// the ring, so a stalled connected client never loses one with no fallback.
func (h *hub) deliverNotification(nev NotificationEvent) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := nev
	te := taggedEvent{notif: &n}
	delivered := false
	for ch := range h.global {
		select {
		case ch <- te:
			delivered = true
		default:
		}
	}
	if delivered {
		return true
	}
	h.ring = append(h.ring, nev)
	if len(h.ring) > notificationRingSize {
		h.ring = h.ring[len(h.ring)-notificationRingSize:]
	}
	return false
}

// broadcastApprovalSignal wakes every connected global subscriber to re-fetch GET
// /approvals immediately rather than waiting for its 750ms poll tick (issue #569).
// It is a best-effort nudge: non-blocking, drop-on-full, and NOT ring-buffered — a
// reconnecting client re-scans approvals on its own, and /approvals is the
// authoritative source, so a dropped signal only forfeits the latency it was
// shaving, never the prompt. The frame carries the approval id for diagnostics;
// the client ignores the body and just re-scans. Per-session subscribers are not
// signalled (the approval list is a global resource).
func (h *hub) broadcastApprovalSignal(id string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	te := taggedEvent{approvalSignal: true, approvalID: id}
	for ch := range h.global {
		select {
		case ch <- te:
		default:
		}
	}
}

// broadcastApprovalExpired tells every connected global subscriber that a
// previously-presented approval reached its auto-deny timeout before it was
// answered (issue #569), so each can surface a "this prompt timed out; the safe
// default was applied" notice in the prompt's session window — closing the silent
// gap where a user's late click on a still-open dialog had no effect and they were
// never told. Best-effort like broadcastApprovalSignal: non-blocking, drop-on-full,
// NOT ring-buffered (it is only meaningful to a client that saw the live prompt;
// a reconnecting client re-syncs the live /approvals list instead). id+sessionID
// ride the frame so the client routes the notice to the right window.
func (h *hub) broadcastApprovalExpired(id, sessionID string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	te := taggedEvent{approvalExpired: true, approvalID: id, sessionID: sessionID}
	for ch := range h.global {
		select {
		case ch <- te:
		default:
		}
	}
}

// cloneSubs returns a snapshot of a subscriber map's channels under the lock so
// delivery can happen without holding it.
func (h *hub) cloneSubs(m map[chan taggedEvent]struct{}) []chan taggedEvent {
	out := make([]chan taggedEvent, 0, len(m))
	for ch := range m {
		out = append(out, ch)
	}
	return out
}

// clientCount returns the number of currently-connected SSE subscribers across
// the global (/events) stream and every per-session (/sessions/:id/events)
// stream. Each open stream is one connected client that could answer interactive
// prompts. The approval bridge reads this to decide its wait bound: with
// subscribers present a stalled prompt auto-denies after the normal
// ApprovalTimeout (connected-but-unresponsive); with zero it waits up to the
// longer UnattendedApprovalTimeout so a transiently-disconnected daemon does not
// kill long watcher turns (issue #358 §8).
func (h *hub) clientCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := len(h.global)
	for _, subs := range h.session {
		n += len(subs)
	}
	return n
}

// subscribeSession adds a buffered channel as a per-session subscriber and
// returns it along with an unsubscribe func. The caller drains it from its SSE
// producer loop and calls unsubscribe when the client disconnects.
func (h *hub) subscribeSession(sessionID string) (<-chan taggedEvent, func()) {
	ch := make(chan taggedEvent, 64)
	h.mu.Lock()
	if h.session[sessionID] == nil {
		h.session[sessionID] = make(map[chan taggedEvent]struct{})
	}
	h.session[sessionID][ch] = struct{}{}
	h.mu.Unlock()
	return ch, func() { h.unsubscribe(sessionID, ch) }
}

// subscribeGlobal adds a buffered channel as a global subscriber. Before the
// channel goes live it is preloaded with the missed-notification ring (the
// notifications buffered while no client was connected) and the ring is cleared —
// the reconnect replay (issue #358 §9). Draining and subscribing happen under one
// lock so a notification arriving mid-(re)subscribe is never both replayed and
// re-buffered. The replayed frames sit ahead of any live event in the buffer, so
// the client sees its backlog first.
func (h *hub) subscribeGlobal() (<-chan taggedEvent, func()) {
	ch := make(chan taggedEvent, 128)
	h.mu.Lock()
	for i := range h.ring {
		n := h.ring[i]
		select {
		case ch <- taggedEvent{notif: &n}:
		default:
		}
	}
	h.ring = nil
	h.global[ch] = struct{}{}
	h.mu.Unlock()
	return ch, func() { h.unsubscribe("", ch) }
}

func (h *hub) unsubscribe(sessionID string, ch chan taggedEvent) {
	h.mu.Lock()
	if sessionID == "" {
		delete(h.global, ch)
	} else if m := h.session[sessionID]; m != nil {
		delete(m, ch)
		if len(m) == 0 {
			delete(h.session, sessionID)
		}
	}
	h.mu.Unlock()
}

// isTerminal reports whether ev is one a client must not miss (final answer,
// error, plan, subagent completion). These bypass the drop-on-full path.
func isTerminal(ev agent.SessionEvent) bool {
	switch ev.Type {
	case agent.SessionEventFinal, agent.SessionEventError, agent.SessionEventPlan:
		return true
	}
	return false
}
