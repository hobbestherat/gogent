package ui

import (
	"fmt"
	"time"

	tv "github.com/hobbestherat/turbotui/turbotv"
)

// The daemon disconnect modal (issue #358 §7). When the remote event stream drops,
// the attached TUI raises a BLOCKING modal that covers the workbench and accepts no
// input except its own "Retry now" / "Quit" buttons — so nothing the user types is
// locally queued while the daemon is unreachable. The frozen last-known transcript
// stays rendered beneath it. The RemoteClient drives the reconnect backoff in the
// background and calls OnConnectionLost / OnConnectionRestored (the Reconnector
// seam); those marshal every UI mutation onto the event-loop thread via Post.

// reconnectCoalesceWindow is the production leading-edge debounce window for the
// reconnect jump-to-present refresh (issue #520). It sits below the reconnect
// backoff floor (500ms) and is roughly one approval-poll interval, so it collapses
// the sub-second flap storm of an early stream flap while leaving any genuinely
// later reconnect (well outside this window) to refresh in full.
const reconnectCoalesceWindow = 750 * time.Millisecond

// SetReconnectControls wires the disconnect modal's host label and its "Retry now"
// action (the RemoteClient's RetryNow). It is called once during attach setup,
// before the UI loop starts, so it needs no synchronisation.
func (w *Workbench) SetReconnectControls(host string, retryNow func()) {
	w.reconnectHost = host
	w.reconnectRetry = retryNow
}

// SetAfterRestore installs a callback Run invokes once, after its initial Restore()
// loop has populated the workbench. The attach path uses it to start the deferred
// SSE consumer only after restore, so live daemon events cannot flood the UI thread
// while Restore is still running (issue #516). It is called once during attach
// setup, before the UI loop starts, so it needs no synchronisation; a nil callback
// (the embedded path) leaves Run unchanged.
func (w *Workbench) SetAfterRestore(fn func()) { w.afterRestore = fn }

// OnConnectionLost raises (or, on a later attempt, updates) the blocking modal.
// It is called from the RemoteClient's background goroutine, so it marshals the UI
// work onto the event-loop thread.
func (w *Workbench) OnConnectionLost(attempt int) {
	w.desktop.Post(func() {
		w.showDisconnectModal()
		w.renderDisconnectBody(attempt)
		// Reflect the drop in the menu-bar status indicator (issue #500). The first
		// notification (attempt 1) is the fresh drop ("○ disconnected"); a later attempt
		// is an active backoff retry ("○ reconnecting…"). This is purely presentational —
		// the modal above still owns the attempt count and Retry/Quit.
		if attempt > 1 {
			w.connPhase = connReconnecting
		} else {
			w.connPhase = connDisconnected
		}
		w.refreshConnectionStatus()
		w.desktop.RequestRedraw()
	})
}

// OnConnectionRestored dismisses the modal and performs the jump-to-present
// refresh. The modal teardown is marshalled onto the UI thread; the transcript
// re-fetch runs on this (background) goroutine so its HTTP calls never stall the
// event loop, applying each result back on the UI thread.
func (w *Workbench) OnConnectionRestored() {
	w.desktop.Post(func() {
		w.dismissDisconnectModal()
		// Back to the healthy attached state in the menu-bar status indicator (issue #500).
		w.connPhase = connHealthy
		w.refreshConnectionStatus()
		w.desktop.RequestRedraw()
	})
	w.refreshAfterReconnect()
}

// showDisconnectModal builds and shows the blocking modal if it is not already up.
// It must run on the UI thread.
func (w *Workbench) showDisconnectModal() {
	if w.disconnectLayer != nil {
		return // already shown; renderDisconnectBody updates the attempt count
	}
	spec := tv.DialogSpec{MinW: 44, MaxW: 72, PreferredW: 64, PrefH: 11, MaxH: 14}
	x, y, width, height := w.dialogRect(spec)

	dialog := tv.NewDialog("Connection lost", x, y, width, height)
	applyWindowShadow(dialog.Window) // honour the NoShadow theme setting (issue #215)
	dialog.Window.ShowClose = false

	bodyH := height - 5
	if bodyH < 1 {
		bodyH = 1
	}
	body := tv.NewTextView("", tv.Rect{X: 2, Y: 1, W: width - 4, H: bodyH})
	body.Wrap = true
	body.FG = tv.DefaultTheme.DialogFG
	body.BG = tv.DefaultTheme.DialogBG
	dialog.Window.AddContent(body)
	w.disconnectBody = body

	btnY := height - 3
	retryRect, quitRect := disconnectButtonRow(width, btnY)
	retry := newButton("Retry now", retryRect, func() {
		if w.reconnectRetry != nil {
			w.reconnectRetry()
		}
	})
	// Quit exits this TUI client only; the daemon keeps running on its end.
	quit := newButton("Quit", quitRect, func() { w.QuitFunc()() })
	dialog.Window.AddContent(retry)
	dialog.Window.AddContent(quit)

	layer := tv.NewModalLayer("daemon-disconnect", dialog)
	// Deliberately no Escape handler: the modal blocks until the connection comes
	// back or the user explicitly retries/quits — there is no dismiss-on-Escape.
	layer.NoEnterGrace = true
	w.disconnectLayer = layer
	w.desktop.AddLayer(layer)
	w.desktop.SetFocus(retry)
}

// renderDisconnectBody (re)writes the modal's body for the given 1-based reconnect
// attempt. Host-aware so a remote/--connect attachment names the host. UI thread.
func (w *Workbench) renderDisconnectBody(attempt int) {
	if w.disconnectBody == nil {
		return
	}
	target := "the daemon"
	if w.reconnectHost != "" {
		target = "the daemon at " + w.reconnectHost
	}
	w.disconnectBody.Clear()
	w.disconnectBody.AddLine(fmt.Sprintf("Connection to %s lost.", target))
	w.disconnectBody.AddLine("")
	w.disconnectBody.AddLine(fmt.Sprintf("Reconnecting… (attempt %d)", attempt))
	w.disconnectBody.AddLine("")
	w.disconnectBody.AddLine("The daemon keeps running — your sessions and watchers are safe.")
	w.disconnectBody.AddLine("Use \"Retry now\" to retry immediately, or \"Quit\" to exit this client (the daemon stays up).")
}

// dismissDisconnectModal removes the blocking modal if present. UI thread.
func (w *Workbench) dismissDisconnectModal() {
	if w.disconnectLayer == nil {
		return
	}
	w.desktop.RemoveLayer(w.disconnectLayer)
	w.disconnectLayer = nil
	w.disconnectBody = nil
}

// refreshAfterReconnect performs the §7 jump-to-present: it re-fetches the
// daemon's full live state (GET /sessions + each transcript, via the Restore
// handler) and re-syncs the UI to it, rather than replaying the events missed
// during the outage. An already-open window has its transcript swapped for the
// daemon's current copy; a session that became live on the daemon during the
// outage is reopened in a new window. Pending approvals re-surface on their own:
// the approvals poller resumes once the connection is back. The fetch runs on the
// calling (background) goroutine; every UI mutation is applied on the UI thread.
//
// Restore (GET /sessions + transcripts) is the precise §7 contract, so it is
// preferred; GetTranscript is a fallback that at least re-syncs open windows when
// no Restore handler is wired.
//
// Two §520 refinements keep an early stream flap from rebuilding the same
// transcript over and over:
//   - Per window, reloadIfChanged skips the clear+rebuild when the fetched
//     transcript is identical to the one the window already shows.
//   - Across flaps, a leading-edge coalesce window collapses a burst of rapid
//     reconnects into a single Restore()+resync (see coalesceReconnectRefresh).
//
// This stays a jump-to-present, not a replay: a change that lands during a
// coalesced sub-second outage is reconciled by the next reconnect or the resumed
// live stream, never replayed from a cursor (Direction C is out of scope; see the
// no-replay note on reconnect() in remote_handlers.go).
func (w *Workbench) refreshAfterReconnect() {
	if w.coalesceReconnectRefresh() {
		return
	}
	if w.handlers.Restore != nil {
		open := make(map[string]bool)
		for _, id := range w.SessionIDs() {
			open[id] = true
		}
		for _, rs := range w.handlers.Restore() {
			rs := rs
			if open[rs.ID] {
				// A deferred restore (issue #517) carries no transcript, so reloading
				// rs.Messages would BLANK an open window. Handle the two deferred cases:
				// an unloaded shell is left untouched (it still loads on focus); a
				// window the user had already loaded before the drop is re-synced with a
				// fresh transcript fetch, on this background goroutine.
				if rs.Deferred {
					w.mu.Lock()
					stillShell := w.deferredTranscripts[rs.ID]
					w.mu.Unlock()
					if stillShell {
						continue
					}
					if w.handlers.GetTranscript == nil {
						continue
					}
					msgs := w.handlers.GetTranscript(rs.ID, "root")
					w.desktop.Post(func() {
						w.mu.Lock()
						sw := w.sessions[rs.ID]
						w.mu.Unlock()
						if sw != nil && !sw.readOnly {
							sw.reloadIfChanged(msgs)
						}
					})
					continue
				}
				w.desktop.Post(func() {
					w.mu.Lock()
					sw := w.sessions[rs.ID]
					w.mu.Unlock()
					if sw != nil && !sw.readOnly {
						sw.reloadIfChanged(rs.Messages)
					}
				})
				continue
			}
			// Became live on the daemon during the outage: reopen its window (a
			// deferred one adopts as a shell and loads on focus, like first connect).
			w.desktop.Post(func() { w.AdoptSession(rs) })
		}
		return
	}
	if w.handlers.GetTranscript == nil {
		return
	}
	for _, id := range w.SessionIDs() {
		id := id
		msgs := w.handlers.GetTranscript(id, "root")
		w.desktop.Post(func() {
			w.mu.Lock()
			sw := w.sessions[id]
			w.mu.Unlock()
			if sw != nil && !sw.readOnly {
				sw.reloadIfChanged(msgs)
			}
		})
	}
}

// coalesceReconnectRefresh implements the leading-edge debounce that dedupes a
// burst of rapid early reconnects (issue #520). It runs synchronously on the
// reconnect goroutine (refreshAfterReconnect's caller), so it never races the SSE
// consumer and cannot reorder a reload behind a freshly-streamed live event.
//
// It returns true when this invocation should be skipped: a refresh ran within the
// last reconnectCoalesce window, so re-running the full Restore()+resync would only
// rebuild the same just-fetched state. The FIRST flap in a burst runs normally and
// stamps reconnectRefreshAt; a reconnect AFTER the window (stale stamp) refreshes
// fully again, so a legitimately-later change is never permanently coalesced. A
// non-positive reconnectCoalesce disables the debounce (every call refreshes),
// which is the mode narrow tests run in.
func (w *Workbench) coalesceReconnectRefresh() bool {
	if w.reconnectCoalesce <= 0 {
		return false
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.reconnectRefreshAt.IsZero() && time.Since(w.reconnectRefreshAt) < w.reconnectCoalesce {
		return true
	}
	w.reconnectRefreshAt = time.Now()
	return false
}

// disconnectButtonRow centres the "Retry now" / "Quit" pair on row btnY, sizing
// each button to its rendered label and clamping to the content margins.
func disconnectButtonRow(width, btnY int) (retry, quit tv.Rect) {
	const gap = 4
	rw := tv.ButtonLabelWidth("Retry now")
	qw := tv.ButtonLabelWidth("Quit")
	total := rw + gap + qw
	startX := (width - total) / 2
	if startX < 2 {
		startX = 2
	}
	retry = clampDialogRect(tv.Rect{X: startX, Y: btnY, W: rw, H: 1}, 2, width-3)
	quit = clampDialogRect(tv.Rect{X: startX + rw + gap, Y: btnY, W: qw, H: 1}, 2, width-3)
	return retry, quit
}

// compile-time assertion: *Workbench satisfies the RemoteClient's Reconnector seam.
var _ Reconnector = (*Workbench)(nil)
