package server

import (
	"time"

	"gogent/internal/agent"
	"gogent/internal/notify"
)

// watcherSessionIDPrefix names the dedicated session a free-running watcher fires
// into ("watcher:<name>"). Completions in such a session are skipped by the hub's
// agent-notification emission (issue #358 §9): the watcher manager already raises
// its own "watcher" notification through the notify sink, so emitting an agent
// "complete" notification for the same fire would double up. It mirrors the
// gogent-side constant of the same value.
const watcherSessionIDPrefix = "watcher:"

// notificationForEvent maps a session event to the backend notification it should
// raise (issue #358 §9), mirroring the TUI's in-process eventNotification so the
// over-the-wire path notifies for exactly the same completion/attention events: a
// final answer, a turn error, and a sub-agent CLARIFY question. ok is false for
// every other event (and for an error event with no error). sessionID is carried
// on the result so a reconnecting client can route/focus it; now stamps the emit
// time (RFC3339 UTC).
func notificationForEvent(sessionID string, ev agent.SessionEvent, now func() time.Time) (NotificationEvent, bool) {
	var reason notify.Reason
	var title, body string
	switch ev.Type {
	case agent.SessionEventFinal:
		reason, title, body = notify.ReasonComplete, "Task complete", firstLine(ev.Text)
	case agent.SessionEventError:
		if ev.Err == nil {
			return NotificationEvent{}, false
		}
		reason, title, body = notify.ReasonError, "Task error", firstLine(ev.Err.Error())
	case agent.SessionEventSubAgent:
		// A sub-agent in the "waiting" status has asked a CLARIFY question.
		if ev.Status != agent.StatusWaiting {
			return NotificationEvent{}, false
		}
		body = firstLine(ev.Result)
		if body == "" {
			body = firstLine(ev.Text)
		}
		reason, title = notify.ReasonClarify, "Clarification needed"
	default:
		return NotificationEvent{}, false
	}
	ts := ""
	if now != nil {
		ts = now().UTC().Format(time.RFC3339)
	}
	return NotificationEvent{
		Title:     title,
		Body:      body,
		Reason:    string(reason),
		SessionID: sessionID,
		Timestamp: ts,
	}, true
}

// NotifyLocalFunc delivers a notification on the daemon's own host. It is the
// unattended fallback the notification router calls when no remote client is
// connected to receive the notification over SSE — normally
// gogent.Gogent.NotifyLocalFallback (the daemon's notify.Notifier), so a
// headless/remote daemon still notifies locally on its host.
type NotifyLocalFunc func(title, body string)

// NotifyGateFunc reports whether a backend notification of reason is enabled
// under the live notify config (the master switch plus the per-event toggle) —
// normally gogent.Gogent.ShouldNotifyReason. Both the over-the-wire and the
// local-fallback paths are gated by it, so disabling a reason in config
// suppresses the notification everywhere.
type NotifyGateFunc func(reason string) bool

// NotificationSink builds the daemon's notification router and returns it as the
// callback to install with gogent.SetNotifySink (issue #358 §9). gate and local
// are injected (normally the gogent ShouldNotifyReason / NotifyLocalFallback
// methods) so the router is unit-testable with fakes against a real hub.
//
// On each backend notification it:
//  1. gates on gate(reason) — a config-disabled reason is dropped (so
//     notify.Reason/ShouldNotify still governs whether anything is emitted);
//  2. builds a NotificationEvent stamped with the server's clock;
//  3. routes by connectedness, derived from the hub's global-stream subscriber
//     count (the same source §8 uses for approvals): with >=1 connected client it
//     broadcasts the NotificationEvent on the global SSE stream so the TUI raises
//     a desktop notification on its own machine; with none connected it calls
//     local(title, body) (the daemon's own notifier) and the event is buffered in
//     the bounded ring for replay when a client next connects.
//
// The broadcast-or-buffer decision is a single atomic step in the hub, so a
// notification racing a (dis)connect is never both broadcast and buffered, nor
// lost.
func (s *Server) NotificationSink(gate NotifyGateFunc, local NotifyLocalFunc) func(reason, title, body string) {
	return func(reason, title, body string) {
		if gate != nil && !gate(reason) {
			return
		}
		nev := NotificationEvent{
			Title:     title,
			Body:      body,
			Reason:    reason,
			Timestamp: s.now().UTC().Format(time.RFC3339),
		}
		if s.hub.deliverNotification(nev) {
			return // delivered live to >=1 connected client
		}
		if local != nil {
			local(title, body) // no client connected: notify on the daemon's host
		}
	}
}

// EnableAgentNotificationFallback wires the daemon-local fallback for agent-
// completion notifications that the hub buffers while no client is connected
// (issue #358 §9). When such a notification is buffered for replay, it is also
// delivered via local on the daemon's own host, gated by gate (the live notify
// config) so a disabled reason fires nothing. Only the daemon calls this;
// embedded mode leaves it unset so the in-process TUI's notifier surfaces
// completions and no background terminal escapes hit os.Stdout. gate/local are
// normally gogent.Gogent.ShouldNotifyReason / NotifyLocalFallback.
func (s *Server) EnableAgentNotificationFallback(gate NotifyGateFunc, local NotifyLocalFunc) {
	s.hub.mu.Lock()
	s.hub.onAgentBuffered = func(nev NotificationEvent) {
		if gate != nil && !gate(nev.Reason) {
			return
		}
		if local != nil {
			local(nev.Title, nev.Body)
		}
	}
	s.hub.mu.Unlock()
}
