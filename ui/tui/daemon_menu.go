package ui

import (
	"fmt"
	"strings"

	tv "github.com/hobbestherat/turbotui/turbotv"
)

// DaemonMode is the TUI's current relationship to a daemon (issue #358 §6). It
// decides which handoff actions the Daemon menu offers: Start is only meaningful
// when running embedded, Stop only when attached to the local daemon, and neither
// when attached to a remote --connect daemon (the handoff operates on the local
// daemon only).
type DaemonMode int

const (
	// DaemonModeEmbedded is the default: the core runs in this process. "Start
	// daemon" migrates it out to a freshly-spawned local daemon.
	DaemonModeEmbedded DaemonMode = iota
	// DaemonModeAttachedLocal means the TUI is a thin client of the local daemon
	// (the Unix socket). "Stop daemon" migrates the state back in-process.
	DaemonModeAttachedLocal
	// DaemonModeAttachedRemote means the TUI is attached to a remote daemon over
	// TCP/SSH (--connect). Start/Stop are disabled — they only ever drive the local
	// daemon — but "Daemon status" still inspects the connected one.
	DaemonModeAttachedRemote
)

// DaemonStatusReport is the UI-facing snapshot the "Daemon status" dialog renders
// (issue #358 §6). It is assembled by the handoff controller from the daemon
// lifecycle files plus a GET /api/daemon/status round-trip (or, in embedded mode,
// from the in-process core), so ui/tui stays free of internal/daemon and
// internal/server. Fields that do not apply to the current mode are left zero and
// simply omitted from the rendered detail.
type DaemonStatusReport struct {
	// Running is true when a daemon is live (attached, or a local daemon answers).
	// In embedded mode it is false: there is no daemon, the core is in-process.
	Running bool
	// Mode is the attachment mode the report was taken in, so the dialog can label
	// embedded vs attached-local vs attached-remote.
	Mode DaemonMode
	// Transport is a human label for how the TUI reaches the core ("embedded",
	// "unix socket", "tcp").
	Transport string
	// Address is the daemon's discovery/connect address (socket path or host:port),
	// empty in embedded mode.
	Address string
	// PID / Uptime / LiveSessions / Watchers / MCPServers are the daemon's live
	// figures; zero/empty when unavailable (e.g. a stopped local daemon).
	PID          int
	Uptime       string
	LiveSessions int
	Watchers     int
	MCPServers   []string
	// Note carries an optional human aside (e.g. why a field is blank). Optional.
	Note string
}

// daemonItems builds the Daemon submenu, mode-aware (issue #358 §6). It is only
// reached when Handlers.DaemonMode is wired (rebuildMenu guards the submenu), so a
// nil DaemonMode never lands here. Disabled items stay visible (rather than
// vanishing) so the menu's shape is stable and the user can see why an action is
// unavailable in the current mode.
func (w *Workbench) daemonItems() []*tv.MenuItem {
	mode := w.handlers.DaemonMode()
	var items []*tv.MenuItem

	switch mode {
	case DaemonModeEmbedded:
		start := tv.NewMenuItem("&Start daemon", func() { w.startDaemonFromMenu() })
		start.Enabled = w.handlers.StartDaemon != nil
		items = append(items, start)
	case DaemonModeAttachedLocal:
		stop := tv.NewMenuItem("S&top daemon", func() { w.stopDaemonFromMenu() })
		stop.Enabled = w.handlers.StopDaemon != nil
		items = append(items, stop)
	case DaemonModeAttachedRemote:
		// Start/Stop operate on the local daemon only; attached to a remote one they
		// are inapplicable. Show a single disabled marker so the menu is not empty.
		na := tv.NewMenuItem("Start/Stop (local daemon only)", nil)
		na.Enabled = false
		items = append(items, na)
	}

	if w.handlers.DaemonStatusInfo != nil {
		items = append(items,
			tv.NewSeparator(),
			tv.NewMenuItem("Daemon stat&us…", func() { w.showDaemonStatusDialog() }),
		)
	}
	return items
}

// startDaemonFromMenu runs the embedded->daemon handoff off the UI thread (it
// spawns a process and waits for it to come up), then reports the outcome and
// rebuilds the menu so it reflects the new attached-local mode. The handler itself
// swaps the Workbench Handlers (on the UI thread via Post) as part of the handoff.
func (w *Workbench) startDaemonFromMenu() {
	if w.handlers.StartDaemon == nil {
		return
	}
	w.showConfirm("Start daemon", "Migrating to the local daemon…\nIn-flight turns are cancelled; their partial output is preserved and reappears after reattach.", nil)
	go func() {
		err := w.handlers.StartDaemon()
		w.desktop.Post(func() {
			w.rebuildMenu()
			if err != nil {
				w.showConfirm("Start daemon", "Could not start the daemon:\n"+err.Error(), nil)
				return
			}
			w.showConfirm("Start daemon", "The local daemon is running and this TUI is now attached to it. Closing the terminal no longer stops your sessions or watchers.", nil)
		})
	}()
}

// stopDaemonFromMenu runs the daemon->embedded handoff off the UI thread (it asks
// the daemon to persist + exit and rebuilds the embedded core), then reports the
// outcome and rebuilds the menu so it reflects the new embedded mode.
func (w *Workbench) stopDaemonFromMenu() {
	if w.handlers.StopDaemon == nil {
		return
	}
	w.showConfirm("Stop daemon", "Migrating back to in-process (embedded) mode…\nThe daemon persists its state and shuts down; sessions and watchers continue in this process.", nil)
	go func() {
		err := w.handlers.StopDaemon()
		w.desktop.Post(func() {
			w.rebuildMenu()
			if err != nil {
				w.showConfirm("Stop daemon", "Could not stop the daemon:\n"+err.Error(), nil)
				return
			}
			w.showConfirm("Stop daemon", "The daemon has stopped and the core is running in this process again.", nil)
		})
	}()
}

// showDaemonStatusDialog fetches the daemon status off the UI thread (it may make
// an HTTP round-trip) and presents it as a read-only info dialog.
func (w *Workbench) showDaemonStatusDialog() {
	if w.handlers.DaemonStatusInfo == nil {
		return
	}
	go func() {
		report, err := w.handlers.DaemonStatusInfo()
		w.desktop.Post(func() {
			if err != nil {
				w.showConfirm("Daemon status", "Could not read daemon status:\n"+err.Error(), nil)
				return
			}
			w.showConfirm("Daemon status", formatDaemonStatus(report), nil)
		})
	}()
}

// formatDaemonStatus renders a DaemonStatusReport as the multi-line body of the
// status dialog. It is pure so the rendering can be unit-tested without a UI.
func formatDaemonStatus(r DaemonStatusReport) string {
	var b strings.Builder
	switch r.Mode {
	case DaemonModeEmbedded:
		b.WriteString("Mode: embedded (no daemon — core runs in this process)\n")
	case DaemonModeAttachedLocal:
		b.WriteString("Mode: attached to local daemon\n")
	case DaemonModeAttachedRemote:
		b.WriteString("Mode: attached to remote daemon\n")
	}
	if r.Running {
		b.WriteString("State: running\n")
	} else {
		b.WriteString("State: stopped\n")
	}
	if r.Transport != "" {
		b.WriteString("Transport: " + r.Transport + "\n")
	}
	if r.Address != "" {
		b.WriteString("Address: " + r.Address + "\n")
	}
	if r.PID > 0 {
		fmt.Fprintf(&b, "PID: %d\n", r.PID)
	}
	if r.Uptime != "" {
		b.WriteString("Uptime: " + r.Uptime + "\n")
	}
	if r.Running || r.Mode == DaemonModeEmbedded {
		fmt.Fprintf(&b, "Live sessions: %d\n", r.LiveSessions)
		fmt.Fprintf(&b, "Watchers: %d\n", r.Watchers)
		mcp := "(none)"
		if len(r.MCPServers) > 0 {
			mcp = strings.Join(r.MCPServers, ", ")
		}
		b.WriteString("MCP servers: " + mcp + "\n")
	}
	if r.Note != "" {
		b.WriteString("\n" + r.Note + "\n")
	}
	return strings.TrimRight(b.String(), "\n")
}
