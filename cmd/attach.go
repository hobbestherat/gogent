package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"gogent/internal/config"
	"gogent/internal/gogent"
	"gogent/internal/sshtunnel"
	tuipkg "gogent/ui/tui"

	tui "github.com/hobbestherat/turbotui"
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
// keybindings, layout, notifications, welcome): it starts no server, sessions,
// watchers or MCP, so it never duplicates the daemon's work. The default model is
// daemon-owned (issue #507) and comes over HTTP, not from this local core; the local
// skills/, rules.json and workspace are loaded but unused in remote mode. See the
// ownership block on installPresentationHandlers for the full client-vs-daemon split.
//
// The embedded (non-attached) path in main is left entirely unchanged; this is a
// separate entry that returns when the TUI loop exits, after which the caller
// (main) returns without running the embedded startup.
func runAttached(homeDir, addr, token string, noColorFlag bool) error {
	// The cancelable context and signal handler are established FIRST so a slow or
	// hung initial connect — especially an ssh:// dial — is both bounded and
	// Ctrl+C-interruptible (issue #482). There is exactly ONE sigChan consumer: it
	// cancels ctx AND forwards into httpShutdownCh (the same funnel the TUI-loop
	// goroutine uses), so a single Ctrl+C always unblocks the final wait below and
	// prints the detach line, on every scheme.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	var sigSeen atomic.Value
	go func() {
		sig := <-sigChan
		sigSeen.Store(sig)
		cancel()
		select {
		case httpShutdownCh <- struct{}{}:
		default:
		}
	}()

	// ssh:// (issue #482): build the in-process SSH tunnel BEFORE the API client so
	// a connect/auth/host-key failure surfaces as a clear error instead of an empty
	// TUI. The tunnel's DialContext is injected so the APIClient mirrors the unix://
	// transport (an SSH channel per request; no local listener). The tunnel is
	// closed on every return path; reconnect re-establishes it via rc.SetTunnel.
	var apiOpts []tuipkg.APIClientOption
	var tunnel *sshtunnel.Tunnel
	var sshTarget string // "user@host" — the exact target the tunnel authenticated as
	if strings.HasPrefix(addr, "ssh://") {
		cfg, perr := sshtunnel.ParseConnectURL(addr, token, *sshKey, *sshKnownHosts, *sshInsecure)
		if perr != nil {
			return fmt.Errorf("bad --connect %q: %w", addr, perr)
		}
		// Use the alias the user typed (not the ~/.ssh/config-resolved HostName)
		// so the "ssh <host> gogent daemon start" hint below matches what `ssh`
		// itself accepts on this machine (issue #498).
		hintHost := cfg.Alias
		if hintHost == "" {
			hintHost = cfg.Host
		}
		sshTarget = cfg.User + "@" + hintHost
		connectCtx, connectCancel := context.WithTimeout(ctx, sshtunnel.DialTimeout)
		t, nerr := sshtunnel.New(connectCtx, cfg)
		connectCancel()
		if nerr != nil {
			return fmt.Errorf("ssh connect %s: %w", cfg.Host, nerr)
		}
		if _, derr := t.Discover(); derr != nil {
			_ = t.Close()
			return fmt.Errorf("resolve daemon at %s: %w", cfg.Host, derr)
		}
		tunnel = t
		apiOpts = append(apiOpts, tuipkg.WithDialContext("http://ssh", tunnel.DialContext))
	}
	defer func() {
		if tunnel != nil {
			_ = tunnel.Close()
		}
	}()

	client, err := tuipkg.NewAPIClient(addr, token, apiOpts...)
	if err != nil {
		return fmt.Errorf("build api client: %w", err)
	}
	// Confirm a live daemon before standing up the UI, so a stale socket or a
	// wrong --connect fails with a clear message instead of an empty TUI.
	if err := client.Health(); err != nil {
		if tunnel != nil {
			// The daemon runs on the REMOTE host, so point the user there, not at
			// the local machine. sshTarget is the exact user@host that just authed.
			return fmt.Errorf("no daemon found at %s — start it on the remote host with `ssh %s gogent daemon start` (%w)", addr, sshTarget, err)
		}
		return fmt.Errorf("daemon not reachable: %w", err)
	}

	// Presentation-config source only (no sessions/server/watchers/MCP started).
	g := gogent.NewGogent(homeDir)

	// Install colour detection once (terminfo-aware — issue #549), then resolve and
	// install the theme at that level before the workbench is built, exactly as the
	// embedded path does. Detection runs on the LOCAL terminal here (the TUI renders
	// in this attach process, not the remote daemon), so a local `--connect
	// ssh://host` is coloured at the local terminal's fidelity. env == nil resolves
	// at the level just installed, keeping the palette degrade and renderer in sync.
	tui.SetColorLevel(tui.DetectColorLevel())
	tuipkg.ApplyTheme(tuipkg.ResolveTheme(g.GetConfig().Theme, nil, noColorFlag))

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
	// Give reconnect the SSH tunnel's Restart handle so a dropped stream
	// re-establishes the tunnel before re-subscribing (issue #482). nil for the
	// other transports leaves their reconnect path unchanged.
	if tunnel != nil {
		rc.SetTunnel(tunnel)
	}
	wb.SetReconnectControls(hostLabel(addr), rc.RetryNow)
	handlers := rc.Handlers()
	// Verbatim --connect argument for the daemon-aware quit dialog's re-attach line
	// (issue #503): unlike hostLabel (a display label) this is a real connect string.
	handlers.ReconnectAddress = func() string { return addr }
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

	// Establish the remote event stream + approvals polling BEFORE entering the
	// TUI loop, so a daemon that went away between the health check and the
	// subscribe fails cleanly without ever launching (and tearing down) UI state.
	// StartGated opens the stream synchronously (preserving that fail-fast) but
	// defers DRAINING it into the UI until the first Restore() completes: it returns
	// a begin closure we hand to the workbench so the live consumer starts only after
	// restore has populated the windows, never flooding the UI thread mid-restore
	// (issue #516). ctx is the one created at the top, so a SIGINT during startup
	// cancels it too.
	begin, err := rc.StartGated(ctx)
	if err != nil {
		rc.Close()
		return fmt.Errorf("start remote event stream: %w", err)
	}
	wb.SetAfterRestore(begin)

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

	// Block until shutdown. BOTH an OS interrupt (forwarded by the signal goroutine
	// above) and a normal TUI quit arrive via httpShutdownCh, so a single Ctrl+C
	// exits cleanly on every scheme. The daemon keeps running — detaching never
	// stops it. The deferred tunnel.Close() then tears the SSH session down.
	<-httpShutdownCh
	if sig := sigSeen.Load(); sig != nil {
		fmt.Printf("\nReceived signal %v, detaching...\n", sig)
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
// machine (its colour theme, keybindings, window layout, desktop notifications and
// onboarding dialog), not the daemon's agent state, so they stay local exactly as the
// issue's layout/notifications guidance recommends. The remote (daemon-backed) fields
// set by RemoteClient.Handlers are left untouched.
//
// Ownership in attached mode (issue #507):
//
//   - CLIENT-owned (this machine's ~/.gogent/config.json, set here): theme + saved
//     themes, keybindings, window layout, welcome/onboarding, and notifications. The
//     notifications block governs THIS terminal's live notifier (its bell/sound/
//     enabled) — the notifier hardware is local, so the client legitimately owns it.
//     NewGogent also loads skills/, rules.json and the workspace/AGENTS.md/repo map,
//     but those are IGNORED in remote mode (their handlers come from the daemon via
//     RemoteClient); the local core is built purely as this presentation-config source.
//   - DAEMON-owned (over HTTP via RemoteClient.Handlers): sessions/messages/models/
//     tools/skills/stats/watchers, the DEFAULT MODEL, budget, timeouts, sub-agents and
//     review-edits. The daemon's own /api/settings/notifications block governs ONLY
//     daemon-side notification fallback (watcher/agent completions when no client is
//     attached); the attached client deliberately neither reads nor writes it, so it
//     never pretends to control daemon-side notifications.
//
// Default model is therefore NOT wired here anymore (it was the split-brain bug): it
// is daemon-owned and comes from RemoteClient.Handlers, so "Set as default" while
// attached updates the DAEMON and the selector resolves the daemon's default against
// the daemon's model list.
func installPresentationHandlers(h *tuipkg.Handlers, g *gogent.Gogent, wb *tuipkg.Workbench, noColorFlag bool) {
	h.GetTheme = func() config.ThemeConfig { return g.Theme() }
	h.SetTheme = func(t config.ThemeConfig) {
		g.SetTheme(t)
		// Re-detect on a live theme switch so a changed terminal/env reflects
		// consistently (issue #549), then resolve at that level (env == nil).
		tui.SetColorLevel(tui.DetectColorLevel())
		tuipkg.ApplyTheme(tuipkg.ResolveTheme(t, nil, noColorFlag))
		wb.RefreshTheme()
	}
	// Named custom themes (issue #462): persisted alongside the active theme; the
	// editor applies a saved theme live via SetTheme, so this only stores the list.
	h.GetSavedThemes = func() []config.NamedTheme { return g.SavedThemes() }
	h.SetSavedThemes = func(themes []config.NamedTheme) { g.SetSavedThemes(themes) }
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
	// Notifications are CLIENT-owned (issue #507): the live notifier fires on THIS
	// machine, so its config legitimately stays local. The daemon's
	// /api/settings/notifications block (daemon-side fallback only) is intentionally
	// not touched from here. GetDefaultModel/SetDefaultModel are deliberately NOT set
	// here — the default model is daemon-owned and wired by RemoteClient.Handlers.
	h.GetNotifyConfig = func() config.NotifyConfig { return g.Notifications() }
	h.SetNotifyConfig = func(n config.NotifyConfig) {
		g.SetNotifications(n)
		wb.SetNotifyConfig(n)
	}
}
