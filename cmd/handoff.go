package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"gogent/internal/agent"
	"gogent/internal/daemon"
	"gogent/internal/gogent"
	"gogent/internal/server"
	tuipkg "gogent/ui/tui"
)

// daemonHealthEvery is how often an attached TUI pings the daemon's /health while
// the SSE stream looks idle, so a half-open or stalled connection that the stream
// read cannot detect still trips the disconnect modal (issue #358 §7). The handoff
// and attach paths set it; tests leave it at zero (off).
const daemonHealthEvery = 10 * time.Second

// daemonController owns the symmetric embedded <-> daemon handoff driven from the
// TUI's Daemon menu (issue #358 §6). It tracks the TUI's current attachment mode
// and the live plumbing for that mode (an embedded *gogent.Gogent, or an attached
// APIClient + RemoteClient), and swaps the Workbench Handlers between the
// in-process closures and the remote (HTTP/SSE) implementation as the user starts
// or stops the local daemon.
//
// The handoff is always: persist -> spawn/restore the target -> switch Handlers ->
// shut the source down, built entirely on the existing on-disk persistence (which
// is already restart-safe). A turn in flight at handoff time is cancelled; its
// partial output is already in the persisted transcript and reappears after
// reattach, and the user re-sends to continue — there is no live migration of an
// in-flight model stream (the issue's stated, deliberate limitation).
type daemonController struct {
	wb      *tuipkg.Workbench
	homeDir string
	noColor bool
	paths   daemon.Paths

	// embeddedHandlers rebuilds the exact in-process Handlers for a given core. It
	// is the same closure main() installs at startup, so a daemon->embedded handoff
	// restores byte-for-byte identical embedded behaviour.
	embeddedHandlers func(*gogent.Gogent) tuipkg.Handlers

	mu        sync.Mutex
	mode      tuipkg.DaemonMode
	connect   string               // connect address when attached, else ""
	g         *gogent.Gogent       // embedded/presentation core (never nil)
	apiServer *server.Server       // embedded in-process API surface (embedded mode)
	http      embeddedHTTP         // embedded TCP HTTP server handle + rebuild params
	client    *tuipkg.APIClient    // daemon API client (attached modes)
	rc        *tuipkg.RemoteClient // daemon remote client (attached modes)
}

// embeddedHTTP bundles the in-process TCP HTTP API server handle and the params
// needed to rebuild it, so the handoff can shut the source listener down when
// migrating embedded->daemon and bring a fresh one up when migrating back (issue
// #358 §6). srv is nil when the bind failed or when the process started attached
// (it has no embedded server until a Stop handoff creates one).
type embeddedHTTP struct {
	srv      *http.Server
	host     string
	port     int
	password string
}

// newEmbeddedController builds the controller for a process that started embedded
// (the default `gogent` with no live daemon). g/apiServer are the in-process core
// and server; embeddedHandlers rebuilds the in-process Handlers for a core.
func newEmbeddedController(wb *tuipkg.Workbench, homeDir string, noColor bool, g *gogent.Gogent, apiServer *server.Server, httpInfo embeddedHTTP, embeddedHandlers func(*gogent.Gogent) tuipkg.Handlers) *daemonController {
	return &daemonController{
		wb:               wb,
		homeDir:          homeDir,
		noColor:          noColor,
		paths:            daemon.PathsFor(daemonDir(homeDir)),
		embeddedHandlers: embeddedHandlers,
		mode:             tuipkg.DaemonModeEmbedded,
		g:                g,
		apiServer:        apiServer,
		http:             httpInfo,
	}
}

// newAttachedController builds the controller for a process that started already
// attached to a daemon (cmd/attach.go). g is the presentation-only core (theme,
// layout, keybindings) the attach path builds; client/rc drive the daemon. local
// selects attached-local (the Unix socket, "Stop daemon" applies) vs attached-
// remote (a --connect address, where Start/Stop are inapplicable).
func newAttachedController(wb *tuipkg.Workbench, homeDir string, noColor bool, g *gogent.Gogent, client *tuipkg.APIClient, rc *tuipkg.RemoteClient, addr string, local bool, httpInfo embeddedHTTP) *daemonController {
	mode := tuipkg.DaemonModeAttachedRemote
	if local {
		mode = tuipkg.DaemonModeAttachedLocal
	}
	return &daemonController{
		wb:      wb,
		homeDir: homeDir,
		noColor: noColor,
		paths:   daemon.PathsFor(daemonDir(homeDir)),
		// A process that started attached still needs the embedded Handlers builder
		// so "Stop daemon" can migrate back in-process; it is the same package-level
		// source main() uses, so the rebuilt embedded behaviour is identical.
		embeddedHandlers: func(core *gogent.Gogent) tuipkg.Handlers {
			return embeddedHandlersFor(core, wb, noColor)
		},
		mode:    mode,
		connect: addr,
		g:       g,
		client:  client,
		rc:      rc,
		// No embedded server runs yet (the attach path started none); httpInfo
		// carries the bind params so a Stop handoff can bring one up for the new core.
		http: httpInfo,
	}
}

// installMenuHandlers wires the controller's four daemon-menu callbacks onto a
// Handlers set. The menu itself decides which are offered per mode (DaemonMode),
// so all four are always wired; an inapplicable one is simply never invoked.
func (dc *daemonController) installMenuHandlers(h *tuipkg.Handlers) {
	h.DaemonMode = dc.Mode
	h.StartDaemon = dc.Start
	h.StopDaemon = dc.Stop
	h.DaemonStatusInfo = dc.Status
	h.ConnectionLabel = dc.Label
}

// Mode reports the current attachment mode.
func (dc *daemonController) Mode() tuipkg.DaemonMode {
	dc.mu.Lock()
	defer dc.mu.Unlock()
	return dc.mode
}

// Label returns the terse remote target for the menu-bar connection-status indicator
// (issue #500). It is cheap and synchronous (a lock-protected field read, no daemon
// round-trip), so the TUI can call it on every menu rebuild/resize. It is empty for
// embedded and attached-local mode (the indicator derives those from the mode alone);
// for attached-remote it renders the connect address terse via remoteTargetLabel.
func (dc *daemonController) Label() string {
	dc.mu.Lock()
	mode := dc.mode
	connect := dc.connect
	dc.mu.Unlock()
	if mode != tuipkg.DaemonModeAttachedRemote {
		return ""
	}
	return remoteTargetLabel(connect)
}

// remoteTargetLabel renders a --connect address as the terse target shown in the
// status indicator. An ssh:// URL becomes "ssh:user@host" (the SSH port is dropped for
// terseness); an http(s):// TCP address becomes its "host:port". The scheme-less /
// unix forms (never reached in attached-remote mode) fall back to the raw address.
func remoteTargetLabel(addr string) string {
	u, err := url.Parse(addr)
	if err != nil || u.Host == "" {
		return addr
	}
	if u.Scheme == "ssh" {
		host := u.Hostname() // drops the SSH port
		if u.User != nil {
			if name := u.User.Username(); name != "" {
				host = name + "@" + host
			}
		}
		return "ssh:" + host
	}
	return u.Host
}

// Start performs the embedded->daemon handoff. It is invoked on a background
// goroutine by the menu. Only valid in embedded mode.
func (dc *daemonController) Start() error {
	dc.mu.Lock()
	if dc.mode != tuipkg.DaemonModeEmbedded {
		dc.mu.Unlock()
		return errors.New("a daemon is already attached")
	}
	g := dc.g
	dc.mu.Unlock()

	// The handoff follows §6 strictly — persist -> spawn/restore target -> switch
	// Handlers -> shut down source — so the source's live services keep running
	// until the target is confirmed working. Every failure before the switch
	// returns with the embedded core fully intact (nothing of the source has been
	// torn down yet), so a failed Start never strands the user in a half-shut-down
	// embedded process.

	// 1. Persist: cancel in-flight turns and flush the store so the daemon restores
	//    the current state (a cancelled turn's partial transcript is already saved).
	//    This must precede the spawn so the daemon restores from current state.
	cancelInflightTurns(g)
	g.SyncStore()

	// 2. Spawn/restore the target: spawn the daemon detached (unless one is already
	//    up) and wait until it binds its socket and serves /health. The source is
	//    still fully live here, so a spawn/readiness failure rolls back cleanly.
	spawned := false
	if st := daemon.Query(dc.paths); !st.Running {
		if _, err := daemon.Spawn(dc.paths, []string{"daemon", "start", "--foreground"}); err != nil {
			return fmt.Errorf("spawn daemon: %w", err)
		}
		spawned = true
	}
	if !waitRunning(dc.paths, 15*time.Second) {
		dc.rollbackSpawn(spawned)
		return fmt.Errorf("daemon did not become ready; see %s", dc.paths.Log)
	}

	// 3. Build the API + remote client and confirm BOTH /health and the live event
	//    stream work before switching Handlers, so a broken target rolls back the
	//    spawn and leaves the source untouched.
	addr := "unix://" + dc.paths.Sock
	client, err := tuipkg.NewAPIClient(addr, "")
	if err != nil {
		dc.rollbackSpawn(spawned)
		return fmt.Errorf("build api client: %w", err)
	}
	if err := client.Health(); err != nil {
		dc.rollbackSpawn(spawned)
		return fmt.Errorf("daemon not reachable after start: %w", err)
	}
	// Recreate every open window's session on the daemon before switching Handlers,
	// so a fresh, never-messaged window — which was never persisted, so the daemon's
	// RestoreSessions cannot rebuild it — has a live backend session and its next
	// send resolves instead of 404ing (issue #476). This is the remote-side mirror of
	// the Stop() path's bindWindowSession loop, making both handoff directions
	// guarantee every open window has a live backend session. The daemon is healthy
	// (sessions are creatable) and the source core is still fully live here, so this
	// runs before the Handlers switch and well before the source shutdown below.
	createDaemonWindowSessions(client, dc.wb)
	rc, err := dc.switchToRemote(g, client, addr)
	if err != nil {
		dc.rollbackSpawn(spawned)
		return err
	}

	// 4. Shut the source down now that the target owns the work: stop its watchers
	//    and MCP, and close the in-process HTTP API server so the stale source core
	//    is no longer addressable (no split-brain — the daemon owns the state and
	//    serves the TUI over the socket from here on). §6's "shut down source".
	g.StopWatchers()
	g.CloseMCPServers()
	dc.stopEmbeddedHTTP()

	// 5. Commit the new attached-local state.
	dc.mu.Lock()
	dc.mode = tuipkg.DaemonModeAttachedLocal
	dc.client = client
	dc.rc = rc
	dc.connect = addr
	dc.mu.Unlock()
	return nil
}

// switchToRemote builds a RemoteClient over client, confirms its live event stream
// opens (rc.Start is synchronous on the initial subscribe), and only then swaps the
// Workbench Handlers to the remote implementation on the UI thread. Returning an
// error before the swap — when the stream cannot be opened — leaves the Handlers
// untouched, so the caller can roll back. addr labels the reconnect modal's host.
func (dc *daemonController) switchToRemote(g *gogent.Gogent, client *tuipkg.APIClient, addr string) (*tuipkg.RemoteClient, error) {
	rc := tuipkg.NewRemoteClient(client, dc.wb.EmitSessionEvent, dc.wb)
	rc.SetReconnector(dc.wb)
	rc.SetHealthCheck(daemonHealthEvery)
	if err := rc.Start(context.Background()); err != nil {
		rc.Close()
		return nil, fmt.Errorf("start remote event stream: %w", err)
	}
	handlers := rc.Handlers()
	// Verbatim --connect argument for the daemon-aware quit dialog's re-attach line
	// (issue #503); set on both initial switchToRemote and the reconnect path.
	handlers.ReconnectAddress = func() string { return addr }
	installPresentationHandlers(&handlers, g, dc.wb, dc.noColor)
	dc.installMenuHandlers(&handlers)
	dc.applyOnUI(func() {
		dc.wb.SetHandlers(handlers)
		dc.wb.SetReconnectControls(hostLabel(addr), rc.RetryNow)
		dc.wb.RefreshMenu()
	})
	return rc, nil
}

// rollbackSpawn stops a daemon this Start spawned, used when a later step fails
// before the Handlers switch. It never stops a pre-existing daemon (spawned is
// false then), so attaching to an already-running daemon that later fails does not
// kill someone else's instance.
func (dc *daemonController) rollbackSpawn(spawned bool) {
	if spawned {
		_ = daemon.Stop(dc.paths, 5*time.Second, true)
	}
}

// stopEmbeddedHTTP shuts the in-process TCP HTTP API server down so the stale
// source core is no longer reachable after an embedded->daemon handoff (issue #358
// §6). Idempotent: a nil handle (bind failed, or already stopped) is a no-op.
func (dc *daemonController) stopEmbeddedHTTP() {
	if dc.http.srv != nil {
		_ = dc.http.srv.Close()
		dc.http.srv = nil
		dc.apiServer = nil
	}
}

// startEmbeddedHTTP brings the in-process TCP HTTP API server up for core g on a
// daemon->embedded handoff, restoring embedded mode's "always expose the API"
// behaviour. It builds a fresh API surface bound to g and serves quietly (no
// stdout banner, so it never corrupts the TUI screen). A bind failure is logged
// and degrades gracefully — the embedded TUI still works without the HTTP API.
func (dc *daemonController) startEmbeddedHTTP(g *gogent.Gogent) {
	apiServer := server.NewServer(g, server.Options{
		Password:                  dc.http.password,
		Token:                     os.Getenv("GOGENT_HTTP_TOKEN"),
		ApprovalTimeout:           5 * time.Minute,
		UnattendedApprovalTimeout: g.GetConfig().UnattendedApprovalTimeoutOrDefault(),
	})
	srv, err := serveHTTPAPI(dc.http.host, dc.http.port, g, apiServer, dc.http.password)
	if err != nil {
		log.Printf("daemon handoff: embedded HTTP server not restarted: %v", err)
		return
	}
	dc.apiServer = apiServer
	dc.http.srv = srv
}

// Stop performs the daemon->embedded handoff. It is invoked on a background
// goroutine by the menu. Only valid when attached to the LOCAL daemon.
func (dc *daemonController) Stop() error {
	dc.mu.Lock()
	if dc.mode != tuipkg.DaemonModeAttachedLocal {
		dc.mu.Unlock()
		return errors.New("Stop daemon applies only when attached to the local daemon")
	}
	rc := dc.rc
	client := dc.client
	addr := dc.connect
	dc.mu.Unlock()

	// 1. Detach the remote client first so the daemon's graceful /exit — which drops
	//    the SSE stream — does not trip the disconnect modal mid-handoff.
	if rc != nil {
		rc.Close()
	}
	// 2. Ask the local daemon to persist and shut down gracefully (it flushes the
	//    store on its way out, so the disk is current for the embedded restore). If
	//    it will not stop, the daemon is still live and owns the state, so re-attach
	//    a fresh remote client and stay attached rather than strand the user with a
	//    closed client and no embedded core (symmetric rollback).
	if err := daemon.Stop(dc.paths, 15*time.Second, false); err != nil && !errors.Is(err, daemon.ErrNotRunning) {
		if newRC, rerr := dc.switchToRemote(dc.g, client, addr); rerr == nil {
			dc.mu.Lock()
			dc.rc = newRC
			dc.mu.Unlock()
		}
		return fmt.Errorf("stop daemon: %w", err)
	}

	// 3. Build a fresh embedded core and restore sessions from disk.
	g := buildDaemonCore(dc.homeDir, daemonStartOpts{})
	// 4. Rewire each open window's backend observer to the new core so live sessions
	//    keep streaming into their windows exactly as the embedded OnCreate does.
	for _, id := range dc.wb.SessionIDs() {
		bindWindowSession(g, dc.wb, id)
	}
	// 5. Restart MCP + watchers in-process (mirrors the embedded startup order) and
	//    bring the in-process HTTP API server back up for the new core, so embedded
	//    mode again "always" exposes the API (the symmetric inverse of the source
	//    shutdown in Start).
	g.GetPermissionService().SetPrompter(dc.wb)
	g.SetReviewer(dc.wb)
	g.StartMCPServers()
	g.StartWatchers()
	dc.startEmbeddedHTTP(g)

	// 6. Switch the Handlers back to the in-process implementation on the UI thread,
	//    re-seeding the live notifier/budget/keybindings as the embedded startup does.
	handlers := dc.embeddedHandlers(g)
	dc.installMenuHandlers(&handlers)
	dc.applyOnUI(func() {
		dc.wb.SetHandlers(handlers)
		dc.wb.SetReconnectControls("", nil)
		dc.wb.SetNotifyConfig(g.Notifications())
		g.SetNotifySink(dc.wb.NotifyFromBackend)
		dc.wb.SetBudgetConfig(g.Budget())
		dc.wb.LoadKeybindings(g.Keybindings())
		dc.wb.RefreshMenu()
	})

	// 7. Commit the new embedded state.
	dc.mu.Lock()
	dc.mode = tuipkg.DaemonModeEmbedded
	dc.g = g
	dc.rc = nil
	dc.client = nil
	dc.connect = ""
	dc.mu.Unlock()
	return nil
}

// Status assembles the snapshot the "Daemon status" dialog renders. In embedded
// mode it reports the in-process core directly; when attached it does a single
// GET /api/daemon/status round-trip and tags it with the local lifecycle address.
func (dc *daemonController) Status() (tuipkg.DaemonStatusReport, error) {
	dc.mu.Lock()
	mode := dc.mode
	client := dc.client
	connect := dc.connect
	g := dc.g
	dc.mu.Unlock()

	if mode == tuipkg.DaemonModeEmbedded {
		return tuipkg.DaemonStatusReport{
			Mode:         tuipkg.DaemonModeEmbedded,
			Running:      false,
			Transport:    "embedded (in-process)",
			LiveSessions: liveUserSessionCount(g),
			Watchers:     len(g.ListWatchers("")),
			MCPServers:   g.MCPServerNames(),
			Note:         "No daemon: the core runs in this process. Use \"Start daemon\" to migrate it out.",
		}, nil
	}

	report := tuipkg.DaemonStatusReport{Mode: mode}
	if mode == tuipkg.DaemonModeAttachedLocal {
		report.Transport = "unix socket"
		report.Address = dc.paths.Sock
	} else {
		report.Transport = "tcp"
		report.Address = connect
	}
	if client == nil {
		return report, errors.New("not attached")
	}
	st, err := client.DaemonStatus()
	if err != nil {
		return report, fmt.Errorf("daemon status: %w", err)
	}
	report.Running = true
	report.PID = st.PID
	report.Uptime = formatUptime(st.UptimeSeconds)
	report.LiveSessions = st.LiveSessions
	report.Watchers = st.Watchers
	report.MCPServers = st.MCPServers
	return report, nil
}

// applyOnUI runs fn on the Workbench's UI thread and blocks until it completes, so
// a handoff's Handlers swap is fully applied before the caller reports success.
func (dc *daemonController) applyOnUI(fn func()) {
	done := make(chan struct{})
	dc.wb.Post(func() {
		fn()
		close(done)
	})
	<-done
}

// cancelInflightTurns stops the root agent of every live session, so a turn in
// flight at handoff time is cancelled (its partial transcript is already persisted
// per turn). It is the "persist" step's cancellation half.
func cancelInflightTurns(g *gogent.Gogent) {
	for _, id := range g.SessionIDs() {
		if s := g.GetUserSession(id); s != nil {
			_ = s.StopAgent("root")
		}
	}
}

// bindWindowSession ensures a live backend session exists for an open window id on
// the embedded core and wires its observer to the window, mirroring the embedded
// OnCreate. RestoreSessions has already rebuilt persisted sessions; this adopts
// each open window (creating a session for one not on disk yet) and announces its
// yolo state once the observer is installed.
func bindWindowSession(g *gogent.Gogent, wb *tuipkg.Workbench, id string) {
	s := g.GetUserSession(id)
	if s == nil {
		s = g.NewSession(id)
	}
	s.SetObserver(func(ev agent.SessionEvent) { wb.EmitSessionEvent(id, ev) })
	g.EmitYoloState(id)
}

// createDaemonWindowSessions creates each open window's session on the freshly
// started daemon, so an embedded->daemon handoff leaves every window with a live
// backend session (issue #476). It is the remote equivalent of the Stop() path's
// bindWindowSession: a fresh, never-messaged window was never persisted, so the
// daemon's RestoreSessions cannot rebuild it and the window's next OnSend would
// 404. The backend-only "default" and "watcher:"-prefixed sessions are excluded —
// they are not user windows — using the same filter as liveUserSessionCount. The
// server's createSession is idempotent, so a window already restored from disk is
// re-created harmlessly (no duplicate, no error). The window title is carried
// across so the daemon session keeps the user's name, mirroring OnCreate on the
// auto-attach path. A per-window create failure is logged and degrades to the
// pre-fix behaviour for that one window (it would 404 on send), so a single failure
// never aborts the whole handoff.
func createDaemonWindowSessions(client *tuipkg.APIClient, wb *tuipkg.Workbench) {
	for _, id := range wb.SessionIDs() {
		if id == "default" || strings.HasPrefix(id, "watcher:") {
			continue
		}
		title := wb.SessionTitle(id)
		if _, err := client.CreateSession(id, title, true); err != nil {
			log.Printf("handoff: create session %s on daemon: %v", id, err)
		}
	}
}

// liveUserSessionCount counts the user-facing live sessions on g — the shared
// "default" HTTP session and the backend-only "watcher:" sessions are excluded so
// the figure matches the daemon-status endpoint's live_sessions.
func liveUserSessionCount(g *gogent.Gogent) int {
	n := 0
	for _, id := range g.SessionIDs() {
		if id == "default" || strings.HasPrefix(id, "watcher:") {
			continue
		}
		n++
	}
	return n
}

// hostLabel derives a human host label for the disconnect modal from a connect
// address: the local socket reads as "the local daemon", a TCP endpoint as its
// host:port.
func hostLabel(addr string) string {
	u, err := url.Parse(addr)
	if err != nil || u.Scheme == "unix" || u.Scheme == "" {
		return "the local daemon"
	}
	if u.Host != "" {
		return u.Host
	}
	return addr
}

// formatUptime renders a whole-second uptime as a compact "1h2m3s" style string.
func formatUptime(seconds int64) string {
	if seconds <= 0 {
		return "0s"
	}
	return (time.Duration(seconds) * time.Second).String()
}

// daemonDir resolves the daemon lifecycle directory under a home dir (~/.gogent),
// matching daemon.DefaultDir without re-resolving the home.
func daemonDir(homeDir string) string {
	return filepath.Join(homeDir, ".gogent")
}
