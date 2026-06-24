package server

import "time"

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
