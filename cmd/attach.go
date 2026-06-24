package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"gogent/internal/config"
	"gogent/internal/gogent"
	tuipkg "gogent/ui/tui"
)

// resolveMode decides whether the default `gogent` invocation attaches the TUI
// to a daemon or runs embedded (issue #358, Phase 2). It is pure and takes an
// injected probe so the decision is unit-testable without a real daemon:
//
//   - --embedded               → embedded (escape hatch), never attach.
//   - --connect <addr>         → attach to that explicit address.
//   - otherwise, probe() true  → attach to the local socket (localSockAddr).
//   - otherwise                → embedded (current behaviour; daemon opt-in).
//
// It returns whether to attach and, when attaching, the address to connect to.
func resolveMode(embedded bool, connect, localSockAddr string, probe func() bool) (attach bool, addr string) {
	if embedded {
		return false, ""
	}
	if connect != "" {
		return true, connect
	}
	if probe != nil && probe() {
		return true, localSockAddr
	}
	return false, ""
}

// resolveConnectToken resolves the bearer token for a TCP --connect: the --token
// flag wins, then GOGENT_HTTP_TOKEN. Empty is fine for the local Unix socket
// (the daemon treats a socket caller as local).
func resolveConnectToken(flagValue string) string {
	if flagValue != "" {
		return flagValue
	}
	return os.Getenv("GOGENT_HTTP_TOKEN")
}

// runAttached runs the TUI as a thin client of a daemon at addr (issue #358,
// Phase 2). The data plane — sessions, messages, stops, injects, undo/rewind,
// plan mode, settings, models, tools, skills, statistics, watchers and the live
// event stream — is driven over HTTP/SSE via the APIClient + RemoteClient. A
// local *gogent.Gogent is built only as a presentation-config source (theme,
// keybindings, layout, notifications, default model, welcome): it starts no
// server, sessions, watchers or MCP, so it never duplicates the daemon's work.
//
// The embedded (non-attached) path in main is left entirely unchanged; this is a
// separate entry that returns when the TUI loop exits, after which the caller
// (main) returns without running the embedded startup.
func runAttached(homeDir, addr, token string, noColorFlag bool) error {
	client, err := tuipkg.NewAPIClient(addr, token)
	if err != nil {
		return fmt.Errorf("build api client: %w", err)
	}
	// Confirm a live daemon before standing up the UI, so a stale socket or a
	// wrong --connect fails with a clear message instead of an empty TUI.
	if err := client.Health(); err != nil {
		return fmt.Errorf("daemon not reachable: %w", err)
	}

	// Presentation-config source only (no sessions/server/watchers/MCP started).
	g := gogent.NewGogent(homeDir)

	// Resolve and install the colour theme before the workbench is built, exactly
	// as the embedded path does.
	tuipkg.ApplyTheme(tuipkg.ResolveTheme(g.GetConfig().Theme, os.Getenv, noColorFlag))

	// Populate the model dropdown from the daemon's configured models (the daemon
	// owns the agent models); fall back to the local config if that read fails.
	models := remoteModelConfigs(client)
	if len(models) == 0 {
		models = g.GetConfig().ModelConfigs
	}

	wb := tuipkg.NewWorkbench(models)
	fmt.Printf("Attached to daemon at %s. Press Ctrl+C to detach.\n", addr)

	// The RemoteClient feeds the daemon's global SSE stream into the workbench and
	// drives its permission/edit-review modals for remote approvals.
	rc := tuipkg.NewRemoteClient(client, wb.EmitSessionEvent, wb)
	// Surface daemon-side notifications (watcher AND agent completions) on THIS
	// machine: a "notification" SSE frame raises the TUI's desktop notifier here —
	// the point of over-the-wire notifications for a remote daemon (issue #358 §9).
	// The notification carries the originating session id so a completion for the
	// focused window is focus-suppressed like the in-process path.
	client.SetNotificationHandler(func(n tuipkg.NotificationDTO) {
		wb.NotifyFromWire(n.Reason, n.Title, n.Body, n.SessionID)
	})
	// Completions now arrive over the wire as notification frames, so suppress the
	// TUI's own per-session-event notifications to avoid double-notifying (§9).
	wb.SetEventNotificationsSuppressed(true)
	// Install the disconnect/reconnect observer + controls (issue #358 §7): a
	// dropped stream raises the blocking modal and reconnects with backoff; "Retry
	// now" pokes the client's backoff.
	rc.SetReconnector(wb)
	rc.SetHealthCheck(daemonHealthEvery)
	wb.SetReconnectControls(hostLabel(addr), rc.RetryNow)
	handlers := rc.Handlers()
	installPresentationHandlers(&handlers, g, wb, noColorFlag)
	// Daemon menu (issue #358 §6): a controller tracks the attachment mode so the
	// menu offers "Stop daemon" (local) or just "Daemon status" (remote --connect).
	local := strings.HasPrefix(addr, "unix://")
	// Carry the embedded HTTP bind params so a "Stop daemon" handoff can bring the
	// in-process API server up for the rebuilt embedded core (issue #358 §6). No
	// server runs yet in attach mode, so the handle is nil.
	httpInfo := embeddedHTTP{host: *httpHost, port: *httpPort, password: resolveHTTPPassword(*httpPassword)}
	dc := newAttachedController(wb, homeDir, noColorFlag, g, client, rc, addr, local, httpInfo)
	dc.installMenuHandlers(&handlers)
	wb.SetHandlers(handlers)

	// Seed the live notifier, budget gauge and keybindings, mirroring the embedded
	// startup so the first notification/alert/keystroke respects the user's config.
	wb.SetNotifyConfig(g.Notifications())
	if s, err := client.GetSettings(); err == nil {
		wb.SetBudgetConfig(s.Budget)
	}
	wb.LoadKeybindings(g.Keybindings())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Establish the remote event stream + approvals polling BEFORE entering the
	// TUI loop, so a daemon that went away between the health check and the
	// subscribe fails cleanly without ever launching (and tearing down) UI state.
	// The consumer's posts are queued on the workbench desktop and delivered once
	// the loop below starts.
	if err := rc.Start(ctx); err != nil {
		rc.Close()
		return fmt.Errorf("start remote event stream: %w", err)
	}

	// Run the TUI loop.
	go func() {
		if err := wb.Run(); err != nil {
			log.Printf("TUI error: %v", err)
		}
		// When the UI loop exits (user quit), trigger the shutdown path below.
		select {
		case httpShutdownCh <- struct{}{}:
		default:
		}
	}()

	// Block until an OS interrupt or the TUI loop exits, then detach cleanly. The
	// daemon keeps running on its end — detaching never stops it.
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	select {
	case sig := <-sigChan:
		fmt.Printf("\nReceived signal %v, detaching...\n", sig)
	case <-httpShutdownCh:
	}

	rc.Close()
	wb.QuitFunc()()
	time.Sleep(100 * time.Millisecond)
	return nil
}

// remoteModelConfigs fetches the daemon's models for the TUI dropdown, returning
// them as the pointer slice NewWorkbench expects. An error yields nil so the
// caller falls back to the local config.
func remoteModelConfigs(client *tuipkg.APIClient) []*config.ModelConfig {
	dtos, err := client.ListModels()
	if err != nil {
		log.Printf("attach: list daemon models: %v", err)
		return nil
	}
	out := make([]*config.ModelConfig, 0, len(dtos))
	for _, d := range dtos {
		mc := d.ToModelConfig()
		out = append(out, &mc)
	}
	return out
}

// installPresentationHandlers fills the TUI-machine presentation handlers on a
// remote Handlers set from the local *gogent.Gogent. These concern the TUI's own
// machine (its colour theme, keybindings, window layout, desktop notifications,
// onboarding dialog and default-model dropdown selection), not the daemon's agent
// state, so they stay local exactly as the issue's layout/notifications guidance
// recommends. The remote (daemon-backed) fields set by RemoteClient.Handlers are
// left untouched.
func installPresentationHandlers(h *tuipkg.Handlers, g *gogent.Gogent, wb *tuipkg.Workbench, noColorFlag bool) {
	h.GetTheme = func() config.ThemeConfig { return g.Theme() }
	h.SetTheme = func(t config.ThemeConfig) {
		g.SetTheme(t)
		tuipkg.ApplyTheme(tuipkg.ResolveTheme(t, os.Getenv, noColorFlag))
		wb.RefreshTheme()
	}
	h.GetKeybindings = func() config.KeybindingsConfig { return g.Keybindings() }
	h.SetKeybindings = func(k config.KeybindingsConfig) { g.SetKeybindings(k) }
	h.LoadLayout = func() gogent.Layout { return g.LoadLayout() }
	h.SaveLayout = func(layout gogent.Layout) {
		if err := g.SaveLayout(layout); err != nil {
			log.Printf("save layout: %v", err)
		}
	}
	h.GetShowWelcome = func() bool { return g.GetShowWelcome() }
	h.SetShowWelcome = func(show bool) { _ = g.SetShowWelcome(show) }
	h.GetNotifyConfig = func() config.NotifyConfig { return g.Notifications() }
	h.SetNotifyConfig = func(n config.NotifyConfig) {
		g.SetNotifications(n)
		wb.SetNotifyConfig(n)
	}
	h.GetDefaultModel = func() string { return g.DefaultModelName() }
	h.SetDefaultModel = func(name string) error { return g.SetDefaultModel(name) }
}
