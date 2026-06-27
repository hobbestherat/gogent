package ui

import (
	"fmt"
	"strings"

	tui "github.com/hobbestherat/turbotui"
	tv "github.com/hobbestherat/turbotui/turbotv"
)

// The daemon-aware quit dialog (issue #503). The quit confirmation varies its
// title, body, buttons and default focus on the TUI's attachment mode and — when
// attached — the live daemon status, so the user knows what quitting will actually
// do: close this client while the daemon survives (attached), or stop everything
// (embedded). It never changes quit semantics: w.quit() remains the only teardown,
// and the daemon is started/stopped only via an explicit button, never implicitly.
//
// confirmQuit routes every confirmable quit gesture here when daemon wiring is
// present; with no wiring (DaemonMode nil) it keeps today's generic Yes/No box.

// quitButtonKind identifies what a quit-dialog button does, so the layout (which
// labels to show) stays separate from the wiring (what each press performs).
type quitButtonKind int

const (
	// quitClient closes this TUI client only — w.quit(). Labelled "Quit client"
	// when attached (the daemon survives) and "Quit (stops all)" when embedded.
	quitClient quitButtonKind = iota
	// quitStopAndQuit stops the LOCAL daemon (the daemon->embedded handoff) and then
	// quits. Offered only in attached-local mode (Stop drives the local daemon only).
	quitStopAndQuit
	// quitStartAndQuit starts the local daemon (the embedded->daemon handoff) so the
	// work survives, and then quits. Offered only in embedded mode.
	quitStartAndQuit
	// quitCancel dismisses the dialog and does nothing (also the Escape action).
	quitCancel
)

// quitButton is one button in the dialog: its rendered label (already escaped for
// turbotui's mnemonic parser — a literal "&" is written "&&") and the action kind.
type quitButton struct {
	Label string
	Kind  quitButtonKind
}

// quitDialogModel is the pure, UI-free description of the quit dialog for a given
// mode + status snapshot: the title, the body text, the ordered buttons, and the
// index of the button that gets default focus (always the safe Cancel). Keeping it
// pure (built by buildQuitModel) lets every render state be unit-tested without a
// live event loop, mirroring formatDaemonStatus / daemonIndicatorText.
type quitDialogModel struct {
	Title      string
	Body       string
	Buttons    []quitButton
	DefaultIdx int
}

// buildQuitModel decides the quit dialog's title/body/buttons/default-focus from
// the attachment mode and (when attached and haveReport) the live status snapshot.
// host is the human display label (reconnectHost) used in "The daemon at {host}…";
// addr is the verbatim --connect argument (ReconnectAddress(), "" when unavailable)
// used in the re-attach line — the two are distinct so they are never conflated and
// both are directly assertable. canStop/canStart gate the handoff buttons (false =
// the corresponding Handlers func is nil, so the button is omitted). It is pure.
func buildQuitModel(mode DaemonMode, report DaemonStatusReport, haveReport bool, host, addr string, canStop, canStart bool) quitDialogModel {
	switch mode {
	case DaemonModeAttachedLocal:
		m := quitDialogModel{
			Title: "Quit Gogent (daemon stays running)",
			Body:  attachedLocalBody(report, haveReport),
		}
		m.Buttons = append(m.Buttons, quitButton{"Quit client", quitClient})
		if canStop {
			m.Buttons = append(m.Buttons, quitButton{"Stop daemon && quit", quitStopAndQuit})
		}
		m.Buttons = append(m.Buttons, quitButton{"Cancel", quitCancel})
		m.DefaultIdx = len(m.Buttons) - 1 // Cancel — the safe default
		return m

	case DaemonModeAttachedRemote:
		m := quitDialogModel{
			Title: "Quit Gogent (daemon stays running)",
			Body:  attachedRemoteBody(report, haveReport, host, addr),
		}
		// No "Stop daemon & quit": Stop only ever drives the LOCAL daemon.
		m.Buttons = append(m.Buttons,
			quitButton{"Quit client", quitClient},
			quitButton{"Cancel", quitCancel},
		)
		m.DefaultIdx = len(m.Buttons) - 1
		return m

	case DaemonModeEmbedded:
		m := quitDialogModel{
			Title: "Quit Gogent — stops all sessions",
			Body:  embeddedBody(),
		}
		m.Buttons = append(m.Buttons, quitButton{"Quit (stops all)", quitClient})
		if canStart {
			m.Buttons = append(m.Buttons, quitButton{"Start daemon && quit", quitStartAndQuit})
		}
		m.Buttons = append(m.Buttons, quitButton{"Cancel", quitCancel})
		m.DefaultIdx = len(m.Buttons) - 1
		return m

	default:
		// Defensive: nil DaemonMode is handled in confirmQuit (today's Yes/No box) and
		// never reaches here, but degrade to the generic confirmation just in case.
		return quitDialogModel{
			Title:      "Quit Gogent",
			Body:       "Are you sure you want to quit?",
			Buttons:    []quitButton{{"Quit", quitClient}, {"Cancel", quitCancel}},
			DefaultIdx: 1,
		}
	}
}

// attachedLocalBody renders the body for attached-local: enriched with the live
// counts when the status snapshot is in, otherwise the un-enriched fallback. Both
// tell the user the client closes but the local daemon keeps running, and how to
// re-attach.
func attachedLocalBody(report DaemonStatusReport, haveReport bool) string {
	if !haveReport {
		return "Quitting closes this TUI client only.\n" +
			"The local daemon keeps running — your sessions, watchers and\n" +
			"MCP servers continue in the background.\n" +
			"\n" +
			"Re-attach later with:  gogent"
	}
	var b strings.Builder
	b.WriteString("Quitting closes this TUI client only.\n")
	b.WriteString("The local daemon keeps running:\n")
	b.WriteString("\n")
	b.WriteString(quitCountBullets(report))
	b.WriteString("\n")
	b.WriteString("Re-attach later with:  gogent")
	return b.String()
}

// attachedRemoteBody renders the body for attached-remote. It names the host when
// known and shows the exact "--connect {addr}" re-attach command when an address is
// available (addr != ""), omitting the re-attach line otherwise.
func attachedRemoteBody(report DaemonStatusReport, haveReport bool, host, addr string) string {
	daemonPhrase := "The daemon"
	if host != "" {
		daemonPhrase = "The daemon at " + host
	}
	var b strings.Builder
	b.WriteString("Quitting closes this TUI client only.\n")
	if haveReport {
		b.WriteString(daemonPhrase + " keeps running:\n")
		b.WriteString("\n")
		b.WriteString(quitCountBullets(report))
	} else {
		b.WriteString(daemonPhrase + " keeps running — your sessions and watchers\n")
		b.WriteString("continue in the background.\n")
	}
	if addr != "" {
		b.WriteString("\n")
		b.WriteString("Re-attach later with:  gogent --connect " + addr)
	}
	return strings.TrimRight(b.String(), "\n")
}

// embeddedBody renders the body for embedded mode: quitting is destructive (it
// stops everything in this process), with the advice to start the daemon first to
// keep work running.
func embeddedBody() string {
	return "You are running embedded (no daemon).\n" +
		"Quitting stops ALL sessions and watchers in this process;\n" +
		"in-flight turns are cancelled.\n" +
		"\n" +
		"To keep your work running after you leave, start the\n" +
		"daemon first."
}

// quitCountBullets renders the three live-count bullets, pluralising each.
func quitCountBullets(report DaemonStatusReport) string {
	return "  • " + quitCountLine(report.LiveSessions, "live session") + "\n" +
		"  • " + quitCountLine(report.Watchers, "watcher") + "\n" +
		"  • " + quitCountLine(len(report.MCPServers), "MCP server") + "\n"
}

// quitCountLine renders "1 <noun>" or "N <noun>s" — the small pluralisation
// formatDaemonStatus lacks (issue #503). The nouns used here ("live session",
// "watcher", "MCP server") all pluralise by a trailing "s".
func quitCountLine(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

// quitSizingBody returns the enriched body *shape* (placeholder zero counts) for an
// attached mode, used only to size the dialog at open so a later in-place enrich
// never clips the re-attach line off the bottom (issue #503): the enriched body is
// taller than the fallback, so we size to the enriched shape and render the shorter
// fallback into it, keeping the layout frozen while guaranteeing the enriched body
// fits. Placeholder vs real counts differ by at most a couple of digits, so the
// line count is identical and the longest-line width is unchanged — the sizing is
// exact. Only called for the attached modes that enrich.
func quitSizingBody(mode DaemonMode, host, addr string) string {
	switch mode {
	case DaemonModeAttachedLocal:
		return attachedLocalBody(DaemonStatusReport{}, true)
	case DaemonModeAttachedRemote:
		return attachedRemoteBody(DaemonStatusReport{}, true, host, addr)
	default:
		return embeddedBody()
	}
}

// reconnectAddress is the nil-safe accessor for Handlers.ReconnectAddress, so call
// sites and the pure model stay free of nil checks. It returns "" when unwired
// (embedded/attached-local, or a build with no address available).
func (w *Workbench) reconnectAddress() string {
	if w.handlers.ReconnectAddress != nil {
		return w.handlers.ReconnectAddress()
	}
	return ""
}

// rewriteBody replaces a message dialog's body text in place: clear, re-add each
// wrapped line, and scroll back to the top (an informational body reads top-down).
// Used to render the fallback into the enriched-sized box and to swap in the
// enriched body when the status snapshot arrives.
func rewriteBody(body *tv.TextView, message string) {
	if body == nil {
		return
	}
	body.Clear()
	for _, line := range strings.Split(message, "\n") {
		body.AddLine(line)
	}
	body.ScrollToTop()
}

// showQuitDialog opens the daemon-aware quit confirmation. It opens IMMEDIATELY
// with the mode-based fallback body and, for attached modes with a status handler,
// enriches the body in place from a background DaemonStatusInfo() fetch — never
// blocking the quit on a daemon round-trip. The dialog is sized for the enriched
// shape so the enrich cannot clip the re-attach line (see quitSizingBody).
func (w *Workbench) showQuitDialog() {
	mode := w.handlers.DaemonMode()
	host := w.reconnectHost
	addr := w.reconnectAddress()
	canStop := w.handlers.StopDaemon != nil
	canStart := w.handlers.StartDaemon != nil

	model := buildQuitModel(mode, DaemonStatusReport{}, false, host, addr, canStop, canStart)

	// Enrichment runs only for attached modes with a status handler. When it will
	// run, size to the enriched shape so the taller enriched body fits without
	// scrolling; otherwise size to the body actually shown (no cosmetic gap).
	willEnrich := (mode == DaemonModeAttachedLocal || mode == DaemonModeAttachedRemote) &&
		w.handlers.DaemonStatusInfo != nil
	sizing := model.Body
	if willEnrich {
		sizing = quitSizingBody(mode, host, addr)
	}

	dialog, layer, body, width, bodyH := w.newMessageLayer(model.Title, sizing, "quit-dialog")
	rewriteBody(body, model.Body) // render the fallback into the (possibly enriched-sized) box
	w.quitDialogLayer = layer

	dismiss := func() {
		if w.quitDialogLayer != nil {
			w.desktop.RemoveLayer(w.quitDialogLayer)
			w.quitDialogLayer = nil
		}
	}
	quitNow := func() {
		if w.quit != nil {
			w.quit()
		}
	}
	action := func(kind quitButtonKind) func() {
		switch kind {
		case quitStopAndQuit:
			return func() {
				dismiss()
				w.runStopDaemon(func(err error) {
					if err != nil {
						w.showConfirm("Stop daemon", "Could not stop the daemon:\n"+err.Error(), nil)
						return // stay alive on failure
					}
					quitNow()
				})
			}
		case quitStartAndQuit:
			return func() {
				dismiss()
				w.runStartDaemon(func(err error) {
					if err != nil {
						w.showConfirm("Start daemon", "Could not start the daemon:\n"+err.Error(), nil)
						return // stay alive on failure
					}
					quitNow()
				})
			}
		case quitCancel:
			return dismiss
		default: // quitClient
			return func() { dismiss(); quitNow() }
		}
	}

	// Narrow-terminal degradation: if three buttons will not fit side by side, drop
	// the middle handoff button (Stop/Start). It stays reachable from the Daemon
	// menu, so nothing is lost. Two-button rows (attached-remote) always keep both.
	btns := model.Buttons
	labels := quitLabels(btns)
	if len(btns) == 3 && !quitButtonRowFits(width, labels...) {
		btns = []quitButton{btns[0], btns[2]} // keep Quit + Cancel, drop the middle
		labels = quitLabels(btns)
	}

	btnY := bodyH + 2
	rects := quitButtonRow(width, btnY, labels...)
	var focus *tv.Button
	for i, qb := range btns {
		btn := newButton(qb.Label, rects[i], action(qb.Kind))
		dialog.Window.AddContent(btn)
		if qb.Kind == quitCancel {
			focus = btn // default focus on the safe choice
		}
	}

	// Escape dismisses (counts as Cancel).
	dialog.Root().OnTypeFn = func(_ *tv.VisualComponent, event tui.TypeEvent) bool {
		if event.Key == tui.KeyEscape {
			dismiss()
			return true
		}
		return false
	}

	if focus != nil {
		w.desktop.SetFocus(focus)
	}

	if !willEnrich {
		return
	}
	// Enrich the body in place from a background status fetch (mirrors
	// showDaemonStatusDialog). Never blocks the quit; on slow/failed fetch the
	// fallback text stays. Applies only if this same dialog is still up.
	go func() {
		report, err := w.handlers.DaemonStatusInfo()
		w.desktop.Post(func() {
			if err != nil || w.quitDialogLayer != layer {
				return
			}
			enriched := buildQuitModel(mode, report, true, host, addr, canStop, canStart)
			rewriteBody(body, enriched.Body)
			w.desktop.RequestRedraw()
		})
	}()
}

// quitLabels extracts the ordered labels from a button slice.
func quitLabels(btns []quitButton) []string {
	labels := make([]string, len(btns))
	for i, qb := range btns {
		labels[i] = qb.Label
	}
	return labels
}
