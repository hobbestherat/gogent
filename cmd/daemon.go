package main

import (
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"gogent/internal/agent"
	"gogent/internal/command"
	"gogent/internal/daemon"
	"gogent/internal/diag"
	"gogent/internal/gogent"
	"gogent/internal/model"
	"gogent/internal/server"
)

// daemonUsage is printed for an unknown or missing daemon subcommand.
const daemonUsage = `usage: gogent daemon <command>

commands:
  start     start the daemon detached (use --foreground to run in this terminal)
  stop      stop a running daemon gracefully (--force to SIGKILL after --timeout)
  status    report whether the daemon is running, with its pid and address
  restart   stop then start the daemon
`

// daemonStartOpts carries the daemon's runtime configuration from the start/
// restart flag sets through to the foreground runner.
type daemonStartOpts struct {
	foreground bool
	tcp        bool
	httpHost   string
	httpPort   int
	password   string
	yolo       bool
}

// runDaemon dispatches `gogent daemon <subcommand>` and returns the process
// exit code. It owns its own flag parsing per subcommand so the global flags in
// main (which describe the embedded invocation) stay untouched.
func runDaemon(args []string) int {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, daemonUsage)
		return 2
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "start":
		return daemonStart(rest)
	case "stop":
		return daemonStop(rest)
	case "status":
		return daemonStatus(rest)
	case "restart":
		return daemonRestart(rest)
	default:
		fmt.Fprintf(os.Stderr, "gogent daemon: unknown command %q\n\n%s", sub, daemonUsage)
		return 2
	}
}

// daemonPaths resolves the lifecycle-file set under ~/.gogent. A failure to find
// the home directory is fatal for every subcommand, so it is reported once here.
func daemonPaths() (daemon.Paths, error) {
	dir, err := daemon.DefaultDir()
	if err != nil {
		return daemon.Paths{}, fmt.Errorf("resolve daemon dir: %w", err)
	}
	return daemon.PathsFor(dir), nil
}

// daemonStart handles `gogent daemon start`. Without --foreground it spawns a
// detached child (which re-enters this function with --foreground) and returns;
// with --foreground it runs the daemon in this process until shutdown.
func daemonStart(args []string) int {
	fs := flag.NewFlagSet("daemon start", flag.ContinueOnError)
	opts := daemonStartOpts{}
	fs.BoolVar(&opts.foreground, "foreground", false, "run the daemon in this process instead of detaching (debugging, systemd, launchd)")
	fs.BoolVar(&opts.tcp, "tcp", false, "also expose the HTTP API over TCP (in addition to the default Unix socket) for curl/remote clients")
	fs.StringVar(&opts.httpHost, "http-host", "127.0.0.1", "TCP host for the HTTP API (with --tcp)")
	fs.IntVar(&opts.httpPort, "http-port", 8080, "TCP port for the HTTP API (with --tcp)")
	fs.StringVar(&opts.password, "http-password", "", "password for HTTP API login (env GOGENT_HTTP_PASSWORD); authorizes a non-loopback bind")
	fs.BoolVar(&opts.yolo, "yolo", false, "yolo mode: auto-approve prompts except rules.json hard-deny guardrails")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	p, err := daemonPaths()
	if err != nil {
		fmt.Fprintf(os.Stderr, "daemon start: %v\n", err)
		return 1
	}

	if opts.foreground {
		// We are the daemon. Run core + server until a shutdown signal.
		if err := runDaemonForeground(p, opts); err != nil {
			log.Printf("daemon: %v", err)
			return 1
		}
		return 0
	}

	// Pre-flight: avoid spawning when a daemon is already live (the authoritative
	// single-instance guard is the flock the foreground child takes in
	// daemon.Listen, which also reclaims any crash residue safely under the lock —
	// so we deliberately do not clean stale files here, where a concurrent start
	// could race and unlink a peer's freshly-bound socket).
	if st := daemon.Query(p); st.Running {
		fmt.Printf("daemon already running (pid %d) at %s\n", st.PID, st.Addr)
		return 0
	}

	// Spawn a detached copy of ourselves running the foreground path.
	pid, err := daemon.Spawn(p, append([]string{"daemon", "start", "--foreground"}, args...))
	if err != nil {
		fmt.Fprintf(os.Stderr, "daemon start: %v\n", err)
		return 1
	}

	// Wait for the child to come up (bind its socket and start serving /health)
	// so we can confirm liveness — and surface an early crash, e.g. a --tcp bind
	// failure — before returning success. The window covers RestoreSessions, so
	// give it generous headroom.
	if !waitRunning(p, 10*time.Second) {
		fmt.Fprintf(os.Stderr, "daemon start: spawned pid %d but it did not become ready; see %s\n", pid, p.Log)
		return 1
	}
	st := daemon.Query(p)
	fmt.Printf("daemon started (pid %d) at %s\n", st.PID, st.Addr)
	return 0
}

// daemonStop handles `gogent daemon stop`.
func daemonStop(args []string) int {
	fs := flag.NewFlagSet("daemon stop", flag.ContinueOnError)
	var timeout time.Duration
	var force bool
	fs.DurationVar(&timeout, "timeout", 10*time.Second, "grace period to wait for a clean exit before giving up (or SIGKILL with --force)")
	fs.BoolVar(&force, "force", false, "SIGKILL the daemon if it does not exit within --timeout")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	p, err := daemonPaths()
	if err != nil {
		fmt.Fprintf(os.Stderr, "daemon stop: %v\n", err)
		return 1
	}

	switch err := daemon.Stop(p, timeout, force); err {
	case nil:
		fmt.Println("daemon stopped")
		return 0
	case daemon.ErrNotRunning:
		fmt.Println("daemon not running")
		return 0
	default:
		fmt.Fprintf(os.Stderr, "daemon stop: %v\n", err)
		return 1
	}
}

// daemonStatus handles `gogent daemon status`.
func daemonStatus(args []string) int {
	fs := flag.NewFlagSet("daemon status", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return 2
	}

	p, err := daemonPaths()
	if err != nil {
		fmt.Fprintf(os.Stderr, "daemon status: %v\n", err)
		return 1
	}

	st := daemon.Query(p)
	switch {
	case st.Running:
		fmt.Printf("running (pid %d) at %s\n", st.PID, st.Addr)
		return 0
	case st.Stale:
		fmt.Printf("not running (stale pidfile/socket present under %s)\n", p.Dir)
		return 1
	default:
		fmt.Println("not running")
		return 1
	}
}

// daemonRestart handles `gogent daemon restart`: a stop (forced after a grace
// period) followed by a fresh start. Start flags are forwarded to the new
// instance; the stop phase always forces so restart is reliable.
func daemonRestart(args []string) int {
	p, err := daemonPaths()
	if err != nil {
		fmt.Fprintf(os.Stderr, "daemon restart: %v\n", err)
		return 1
	}

	if err := daemon.Stop(p, 10*time.Second, true); err != nil && err != daemon.ErrNotRunning {
		fmt.Fprintf(os.Stderr, "daemon restart: stop: %v\n", err)
		return 1
	}
	return daemonStart(args)
}

// waitRunning polls until a live daemon answers on the socket or the deadline
// elapses, so `daemon start` can confirm the detached child actually came up.
func waitRunning(p daemon.Paths, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		if daemon.Query(p).Running {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// runDaemonForeground is the daemon proper: it builds the core *gogent.Gogent and
// the existing HTTP/SSE server, serves them over the Unix socket (and the TCP
// API for curl/headless clients), and blocks until SIGTERM/SIGINT or an
// authorized /exit, then shuts down gracefully. It reuses the headless path
// exactly — it never starts the in-process TUI.
func runDaemonForeground(p daemon.Paths, opts daemonStartOpts) error {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("resolve home directory: %w", err)
	}

	// Acquire the single-instance lock and bind the listeners BEFORE building the
	// core. daemon.Listen takes the exclusive flock, so a losing concurrent start
	// fails right here — without running buildDaemonCore/RestoreSessions or
	// touching any session state before ownership is established (issue #358).
	ln, err := daemon.Listen(p.Sock)
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}

	// When --tcp is requested, bind the TCP listener now (synchronously) so a
	// failure — port in use, or a non-loopback host without auth — fails the
	// whole start instead of being swallowed in a goroutine while we report
	// success on the Unix socket alone. Bind before Acquire so a failure leaves
	// no pidfile/addr behind (closing ln releases the lock and unlinks the
	// socket).
	var tcpLn net.Listener
	if opts.tcp {
		tcpLn, err = tcpListener(opts.httpHost, opts.httpPort, resolveHTTPPassword(opts.password))
		if err != nil {
			_ = ln.Close()
			return fmt.Errorf("tcp transport: %w", err)
		}
	}

	// Record the discovery address. The Unix socket is always the primary local
	// transport; when --tcp is bound, append the TCP endpoint so daemon.addr (and
	// `daemon status`) reflect the HTTP/curl endpoint too, not just the socket.
	addr := "unix://" + p.Sock
	if tcpLn != nil {
		addr += " " + fmt.Sprintf("http://%s:%d", opts.httpHost, opts.httpPort)
	}
	if err := daemon.Acquire(p, addr); err != nil {
		_ = ln.Close()
		if tcpLn != nil {
			_ = tcpLn.Close()
		}
		return fmt.Errorf("acquire lifecycle: %w", err)
	}
	fmt.Printf("daemon listening on %s (pid %d)\n", addr, os.Getpid())

	g := buildDaemonCore(homeDir, opts)

	// The daemon is headless, so the API bridge is the only prompter/reviewer: a
	// remote client (or curl) answers interactive prompts over /approvals.
	apiServer := server.NewServer(g, server.Options{
		Password:                  resolveHTTPPassword(opts.password),
		Token:                     os.Getenv("GOGENT_HTTP_TOKEN"),
		ApprovalTimeout:           5 * time.Minute,
		UnattendedApprovalTimeout: g.GetConfig().UnattendedApprovalTimeoutOrDefault(),
	})
	apiServer.InstallApprovalGates()
	// Route backend notifications (watcher completions) over the wire: when a
	// client is subscribed to the global SSE stream they are delivered as a
	// notification event the connected TUI surfaces on its own machine; when none
	// is connected they fall back to the daemon's local notifier and are buffered
	// for replay on reconnect (issue #358 §9).
	g.SetNotifySink(apiServer.NotificationSink(g.ShouldNotifyReason, g.NotifyLocalFallback))
	root := buildRootHandler(g, apiServer)

	unixSrv := &http.Server{
		Handler:           root,
		ReadHeaderTimeout: httpReadHeaderTimeout,
		ReadTimeout:       httpReadTimeout,
		IdleTimeout:       httpIdleTimeout,
	}
	go func() {
		if err := unixSrv.Serve(ln); err != nil && err != http.ErrServerClosed {
			log.Printf("daemon unix server error: %v", err)
		}
	}()

	var tcpSrv *http.Server
	if tcpLn != nil {
		tcpSrv = &http.Server{
			Handler:           root,
			ReadHeaderTimeout: httpReadHeaderTimeout,
			ReadTimeout:       httpReadTimeout,
			IdleTimeout:       httpIdleTimeout,
		}
		fmt.Printf("daemon TCP API on http://%s:%d\n", opts.httpHost, opts.httpPort)
		go func() {
			if err := tcpSrv.Serve(tcpLn); err != nil && err != http.ErrServerClosed {
				log.Printf("daemon tcp server error: %v", err)
			}
		}()
	}

	// Connect MCP servers and start free-running watchers, mirroring the headless
	// startup order so the permission prompter is installed first.
	g.StartMCPServers()
	g.StartWatchers()

	// Block until a shutdown signal or an authorized /exit.
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	select {
	case sig := <-sigChan:
		fmt.Printf("daemon: received signal %v, shutting down...\n", sig)
	case <-httpShutdownCh:
		fmt.Println("daemon: shutdown requested via /exit")
	}

	// Graceful shutdown: stop watchers (which may use MCP) before releasing MCP,
	// flush dirty session writes to disk, then close the listeners and reclaim
	// the lifecycle files (issue #358 shutdown sequence).
	g.StopWatchers()
	g.CloseMCPServers()
	g.SyncStore()
	_ = unixSrv.Close()
	if tcpSrv != nil {
		_ = tcpSrv.Close()
	}
	if err := daemon.Release(p); err != nil {
		log.Printf("daemon: release lifecycle: %v", err)
	}
	return nil
}

// buildDaemonCore builds the headless *gogent.Gogent the daemon owns: the same
// core the embedded path builds, minus the TUI. It mirrors main's essential
// setup — loggers, audit trail, the default session and its root agent, the
// command registry and file tools — so the /api surface is fully functional.
func buildDaemonCore(homeDir string, opts daemonStartOpts) *gogent.Gogent {
	g := gogent.NewGogent(homeDir)
	if opts.yolo {
		g.SetGlobalYolo(true)
	}

	// Diagnostics and the security audit trail are file-backed (the daemon has no
	// terminal). A failed open is non-fatal.
	if lg, err := diag.NewFile(filepath.Join(homeDir, ".gogent", "gogent.log")); err == nil {
		g.SetLogger(lg)
	} else {
		log.Printf("open diagnostic log: %v", err)
	}
	if au, err := diag.NewAuditFile(filepath.Join(homeDir, ".gogent", "audit.log")); err == nil {
		g.SetAudit(au)
	} else {
		log.Printf("open audit log: %v", err)
	}

	// The default (HTTP) session, pointed at the configured default endpoint.
	m := model.NewModelConnection()
	if def := g.GetConfig().GetModelConfig(g.GetConfig().DefaultModel); def != nil && def.Endpoint != "" {
		m.SetURL(def.Endpoint)
	}
	rootAgent := agent.NewAgent("root", model.NewModelSession("main", m))
	rootAgent.SetState(agent.StateIdle)
	_ = g.CreateUserSession("default", rootAgent)

	// Restore persisted sessions so the daemon comes up with the user's live
	// state (the whole point: sessions survive a terminal disconnect).
	g.RestoreSessions()

	// Command registry + file tools, as in the embedded path.
	cmdRegistry := command.NewCommandRegistry()
	cmdRegistry.RegisterBuiltInCommands()
	g.RegisterFileTools(cmdRegistry)

	return g
}
