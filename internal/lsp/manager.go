package lsp

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	"go.lsp.dev/jsonrpc2"
	"go.lsp.dev/uri"
)

// Manager owns the per-server clients (the LSP support design §9). It routes a
// file to a ServerConfig by extension, spawns the matching server lazily on first
// use (single-flight, so two concurrent first-touches share one process), detects
// the workspace root from the config's markers, and keeps each client alive for
// the session. Servers are heavy (gopls indexes the module), so configuring five
// does not launch five.
type Manager struct {
	workspaceRoot string
	host          Host

	// byExt routes a file extension (leading dot, lower-cased) to a ServerConfig.
	byExt map[string]ServerConfig

	// LaunchGate, when set, is consulted once per server before its first spawn
	// (the lazy-launch ActionLSP gate, §9). A non-nil error skips the launch. It is
	// wired by the host so internal/lsp carries no permission dependency.
	LaunchGate func(server ServerConfig) error
	// Spawn builds the transport for a server; it defaults to launching the stdio
	// subprocess and is overridable in tests with an in-memory pipe.
	Spawn func(cfg ServerConfig) (jsonrpc2.Stream, func() error, error)

	baseCtx context.Context
	cancel  context.CancelFunc

	mu    sync.Mutex
	slots map[string]*clientSlot
}

// clientSlot deduplicates concurrent first-touches of one server: slot.mu
// serializes the spawn so concurrent first-touches share one process. A
// permission-gate denial is cached in gateErr and is sticky for the session
// (the launch gate fires once per server, §9); a transient spawn/transport
// failure is NOT cached, so a later tool call retries the launch.
//
// slot.mu serializes the spawn only; the client and gateErr fields are guarded by
// the Manager's mu, so the read loops in FileChanged/Shutdown (which iterate slots
// under m.mu) never race the writer and never block behind a long-running spawn.
type clientSlot struct {
	mu      sync.Mutex // serializes the spawn for this server
	client  *Client    // guarded by Manager.mu
	gateErr error      // guarded by Manager.mu
}

// launchGateError wraps a LaunchGate denial so ClientForFile can tell a
// permission decision (cache it, gate-once) apart from a transient spawn or
// transport failure (do not cache; allow a retry). It preserves the underlying
// error for both message and errors.Is/As.
type launchGateError struct{ err error }

func (e *launchGateError) Error() string { return e.err.Error() }
func (e *launchGateError) Unwrap() error { return e.err }

// NewManager creates a Manager for workspaceRoot serving configs, with host
// supplying the server→client callbacks. Disabled or commandless configs are
// ignored at routing time by the caller; every config passed here is routable.
func NewManager(workspaceRoot string, configs []ServerConfig, host Host) *Manager {
	ctx, cancel := context.WithCancel(context.Background())
	m := &Manager{
		workspaceRoot: workspaceRoot,
		host:          host,
		byExt:         map[string]ServerConfig{},
		baseCtx:       ctx,
		cancel:        cancel,
		slots:         map[string]*clientSlot{},
	}
	m.Spawn = spawnStdio
	// Last config wins for a contested extension; in practice extensions are
	// disjoint across servers.
	for _, cfg := range configs {
		for _, ext := range cfg.Extensions {
			m.byExt[strings.ToLower(ext)] = cfg
		}
	}
	return m
}

// configForFile returns the ServerConfig routing path's extension, if any.
func (m *Manager) configForFile(path string) (ServerConfig, bool) {
	cfg, ok := m.byExt[strings.ToLower(filepath.Ext(path))]
	return cfg, ok
}

// ClientForFile returns the running client for path's language, lazily spawning
// it on first use (§9). It returns ErrNoServer when no config matches the
// extension — a clean, non-fatal result like a declined MCP server. Concurrent
// first-touches of the same server share one spawn.
func (m *Manager) ClientForFile(ctx context.Context, path string) (*Client, error) {
	cfg, ok := m.configForFile(path)
	if !ok {
		return nil, ErrNoServer
	}
	m.mu.Lock()
	slot := m.slots[cfg.Name]
	if slot == nil {
		slot = &clientSlot{}
		m.slots[cfg.Name] = slot
	}
	m.mu.Unlock()

	// slot.mu serializes concurrent first-touches of THIS server (so they share one
	// process) without blocking other servers behind m.mu. The slot's client/gateErr
	// fields stay guarded by m.mu so FileChanged/Shutdown never race the writer.
	slot.mu.Lock()
	defer slot.mu.Unlock()

	m.mu.Lock()
	client, gateErr := slot.client, slot.gateErr
	m.mu.Unlock()
	if client != nil {
		return client, nil
	}
	if gateErr != nil {
		// A previous launch was declined by the permission gate; that decision is
		// sticky for the session (gate-once, §9).
		return nil, gateErr
	}

	client, err := m.spawnClient(cfg, path)
	m.mu.Lock()
	if err != nil {
		// Cache only a permission-gate denial. A transient spawn/transport error is
		// left uncached so a subsequent tool call can retry the launch.
		var lge *launchGateError
		if errors.As(err, &lge) {
			slot.gateErr = err
		}
		m.mu.Unlock()
		return nil, err
	}
	// A Shutdown that landed during this spawn cancels baseCtx and replaces the slot
	// table, orphaning this slot. Caching the freshly built client there would leak
	// its subprocess (nothing would ever Close it), so detect the race and close the
	// client instead of caching it. Closing happens after the lock is released.
	if m.baseCtx.Err() != nil || m.slots[cfg.Name] != slot {
		m.mu.Unlock()
		_ = client.Close()
		return nil, context.Canceled
	}
	slot.client = client
	m.mu.Unlock()
	return client, nil
}

// spawnClient gates the launch, detects the root, builds the transport, and runs
// the handshake. The client's read loop runs on the Manager's session context,
// not the caller's, so a cancelled tool call never tears down the server.
func (m *Manager) spawnClient(cfg ServerConfig, path string) (*Client, error) {
	if m.LaunchGate != nil {
		if err := m.LaunchGate(cfg); err != nil {
			// Tag the gate denial so ClientForFile caches it (gate-once) rather than
			// treating it as a retryable transient failure.
			return nil, &launchGateError{err}
		}
	}
	root := m.detectRoot(cfg, path)
	stream, closer, err := m.Spawn(cfg)
	if err != nil {
		return nil, err
	}
	return newClient(m.baseCtx, cfg, uri.File(root), m.workspaceRoot, m.host, stream, closer)
}

// detectRoot walks up from the file for the config's RootMarkers and returns the
// first directory that contains one, falling back to the workspace root (§9).
func (m *Manager) detectRoot(cfg ServerConfig, path string) string {
	dir := filepath.Dir(cleanPath(path))
	if len(cfg.RootMarkers) == 0 {
		return m.workspaceRoot
	}
	for {
		for _, marker := range cfg.RootMarkers {
			if _, err := os.Stat(filepath.Join(dir, marker)); err == nil {
				return dir
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	if m.workspaceRoot != "" {
		return m.workspaceRoot
	}
	return filepath.Dir(cleanPath(path))
}

// FileChanged fans a gogent-performed file mutation to every running client so
// each can re-sync an open document and emit watched-file notifications (§11.2,
// §11.5). It is the seam the host's fileops subscription drives.
func (m *Manager) FileChanged(path string, kind FileChangeKind) {
	m.mu.Lock()
	clients := make([]*Client, 0, len(m.slots))
	for _, slot := range m.slots {
		if slot.client != nil {
			clients = append(clients, slot.client)
		}
	}
	m.mu.Unlock()
	for _, c := range clients {
		c.FileChanged(m.baseCtx, path, kind)
	}
}

// ApplyEdit applies a Tier 3 WorkspaceEdit through the Host's write/checkpoint
// machinery (§12). It is the seam the lsp_rename/lsp_format tools call to apply a
// previewed edit; a nil Host reports the edit could not be applied.
func (m *Manager) ApplyEdit(server string, edit WorkspaceEdit) (applied bool, failureReason string, err error) {
	if m.host == nil {
		return false, "no edit host configured", nil
	}
	return m.host.ApplyEdit(server, edit)
}

// Shutdown closes every running client (clean LSP shutdown → exit → kill) and
// cancels the session context so the read loops stop. It mirrors the MCP stdio
// Close and is safe to call when none are running.
func (m *Manager) Shutdown() {
	m.mu.Lock()
	clients := make([]*Client, 0, len(m.slots))
	for _, slot := range m.slots {
		if slot.client != nil {
			clients = append(clients, slot.client)
		}
	}
	m.slots = map[string]*clientSlot{}
	m.mu.Unlock()
	for _, c := range clients {
		_ = c.Close()
	}
	m.cancel()
}

// stdioReadWriteCloser adapts a subprocess's separate stdout (read) and stdin
// (write) pipes into the single io.ReadWriteCloser jsonrpc2's stream expects.
type stdioReadWriteCloser struct {
	r io.ReadCloser
	w io.WriteCloser
}

func (s stdioReadWriteCloser) Read(p []byte) (int, error)  { return s.r.Read(p) }
func (s stdioReadWriteCloser) Write(p []byte) (int, error) { return s.w.Write(p) }
func (s stdioReadWriteCloser) Close() error {
	_ = s.w.Close()
	return s.r.Close()
}

// spawnStdio launches a server subprocess and returns an LSP stream over its
// stdio plus a closer that terminates it (mirrors the MCP stdio transport). The
// child's stderr is forwarded to gogent's so server diagnostics stay visible.
func spawnStdio(cfg ServerConfig) (jsonrpc2.Stream, func() error, error) {
	cmd := exec.Command(cfg.Command, cfg.Args...) //nolint:gosec // launches user-configured LSP server command
	cmd.Env = os.Environ()
	for k, v := range cfg.Env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	cmd.Stderr = os.Stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, nil, err
	}
	closer := func() error {
		_ = stdin.Close()
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		_ = cmd.Wait()
		return nil
	}
	stream := jsonrpc2.NewStream(stdioReadWriteCloser{r: stdout, w: stdin})
	return stream, closer, nil
}
