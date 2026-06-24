package ui

import (
	"fmt"

	tv "github.com/hobbestherat/turbotui/turbotv"
)

// The daemon disconnect modal (issue #358 §7). When the remote event stream drops,
// the attached TUI raises a BLOCKING modal that covers the workbench and accepts no
// input except its own "Retry now" / "Quit" buttons — so nothing the user types is
// locally queued while the daemon is unreachable. The frozen last-known transcript
// stays rendered beneath it. The RemoteClient drives the reconnect backoff in the
// background and calls OnConnectionLost / OnConnectionRestored (the Reconnector
// seam); those marshal every UI mutation onto the event-loop thread via Post.

// SetReconnectControls wires the disconnect modal's host label and its "Retry now"
// action (the RemoteClient's RetryNow). It is called once during attach setup,
// before the UI loop starts, so it needs no synchronisation.
func (w *Workbench) SetReconnectControls(host string, retryNow func()) {
	w.reconnectHost = host
	w.reconnectRetry = retryNow
}

// OnConnectionLost raises (or, on a later attempt, updates) the blocking modal.
// It is called from the RemoteClient's background goroutine, so it marshals the UI
// work onto the event-loop thread.
func (w *Workbench) OnConnectionLost(attempt int) {
	w.desktop.Post(func() {
		w.showDisconnectModal()
		w.renderDisconnectBody(attempt)
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

// refreshAfterReconnect re-fetches each open live window's transcript from the
// daemon and swaps it in (jump-to-present), discarding the frozen copy shown
// during the outage. The fetches run on the calling (background) goroutine; each
// result is applied on the UI thread. The SSE stream is already re-subscribed by
// the RemoteClient, so live events resume from the present — this re-syncs the
// transcript history without replaying missed events one by one.
func (w *Workbench) refreshAfterReconnect() {
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
				sw.reload(msgs)
			}
		})
	}
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
