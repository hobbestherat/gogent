// Package sshtunnel provides a native ssh:// transport for the attached
// ("thin client") TUI (issue #482, Tier 2 remote attach). It opens an
// in-process SSH session with golang.org/x/crypto/ssh, auto-resolves the
// remote daemon's transport by reading ~/.gogent/daemon.addr over the session,
// and exposes a DialContext that opens an SSH channel to that daemon per HTTP
// request — exactly mirroring the unix:// transport in ui/tui/api_client.go,
// with no local listener.
//
// The daemon is reachable with ZERO custom channel code: a Unix-socket daemon
// (the default, started with plain `gogent daemon start`, no --tcp) is dialed
// over a direct-streamlocal@openssh.com channel; a --tcp daemon over
// direct-tcpip. Both yield a net.Conn backed by an ssh.Channel that net/http
// uses identically. Discovery is file-based and needs no port guessing.
//
// A Tunnel owns one *ssh.Client and is safe for concurrent Dial. Restart
// re-establishes a dropped session (probing first so a healthy session is never
// torn down), feeding the existing RemoteClient.reconnect backoff. Close tears
// the session down on exit; the remote daemon keeps running.
package sshtunnel

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/user"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
	"golang.org/x/crypto/ssh/knownhosts"
	"golang.org/x/term"
)

const (
	// DialTimeout bounds the TCP connect AND the SSH handshake so an unreachable
	// or firewalled host fails fast (~10s) instead of blocking on the OS TCP
	// timeout (~75–130s). It is exported so the attach layer can derive its own
	// context deadline for the synchronous initial connect.
	DialTimeout = 10 * time.Second

	// probeTimeout bounds the liveness probe in Restart. It is deliberately much
	// shorter than DialTimeout: a half-open session (peer gone, no FIN) must trip
	// into a redial quickly so "Retry now" stays responsive.
	probeTimeout = 2 * time.Second

	defaultSSHPort = 22
)

// Config describes an ssh:// attachment target. Auth is key/agent-based; the
// Token is the daemon's bearer token (carried by the APIClient over the tunnel,
// not used for SSH).
type Config struct {
	User string // SSH login user; defaults to the current OS user when absent
	Host string // SSH host
	Port int    // SSH port; 0 → 22

	Token string // daemon bearer token (passed through to the APIClient)

	KeyPath    string // --ssh-key; "" → SSH agent + ~/.ssh/id_* defaults
	KnownHosts string // --ssh-known-hosts; "" → ~/.ssh/known_hosts
	Insecure   bool   // --ssh-insecure-skip-verify: skip host-key verification

	DaemonPort int    // ?port= override: dial the daemon over loopback TCP at this port
	DaemonSock string // ?socket= override: dial the daemon at this Unix socket path
}

func (c Config) sshPort() int {
	if c.Port > 0 {
		return c.Port
	}
	return defaultSSHPort
}

func (c Config) sshAddr() string {
	return net.JoinHostPort(c.Host, strconv.Itoa(c.sshPort()))
}

// ResolvedTarget is the remote daemon transport discovered over SSH. Exactly one
// field is set: a Unix socket (default daemon) or a host:port (a --tcp daemon).
type ResolvedTarget struct {
	UnixSocket string // absolute remote socket path → direct-streamlocal channel
	TCPAddr    string // "host:port" → direct-tcpip channel
}

// Tunnel owns one SSH client session and the resolved daemon target. It is safe
// for concurrent DialContext; Restart may replace the underlying client, guarded
// by mu.
type Tunnel struct {
	cfg Config

	mu     sync.Mutex
	client *ssh.Client
	target ResolvedTarget
}

// ParseConnectURL parses an ssh://[user@]host[:sshport][?port=&socket=] connect
// address into a Config, folding in the SSH auth/host-key flags. A missing user
// defaults to the current OS user. It validates the URL so a malformed --connect
// value fails before any network I/O.
func ParseConnectURL(addr, token, keyPath, knownHosts string, insecure bool) (Config, error) {
	u, err := url.Parse(addr)
	if err != nil {
		return Config{}, fmt.Errorf("parse connect address %q: %w", addr, err)
	}
	if u.Scheme != "ssh" {
		return Config{}, fmt.Errorf("not an ssh connect address: %q", addr)
	}
	host := u.Hostname()
	if host == "" {
		return Config{}, fmt.Errorf("ssh connect address %q has no host", addr)
	}
	cfg := Config{
		Host:       host,
		User:       u.User.Username(),
		Token:      token,
		KeyPath:    keyPath,
		KnownHosts: knownHosts,
		Insecure:   insecure,
	}
	if cfg.User == "" {
		if cu, e := user.Current(); e == nil {
			cfg.User = cu.Username
		}
	}
	if cfg.User == "" {
		return Config{}, fmt.Errorf("ssh connect %q: no user; use ssh://user@host", addr)
	}
	if p := u.Port(); p != "" {
		n, e := strconv.Atoi(p)
		if e != nil || n <= 0 {
			return Config{}, fmt.Errorf("ssh connect %q: bad ssh port %q", addr, p)
		}
		cfg.Port = n
	}
	q := u.Query()
	if v := q.Get("port"); v != "" {
		n, e := strconv.Atoi(v)
		if e != nil || n <= 0 {
			return Config{}, fmt.Errorf("ssh connect %q: bad daemon ?port=%q", addr, v)
		}
		cfg.DaemonPort = n
	}
	if v := q.Get("socket"); v != "" {
		cfg.DaemonSock = v
	}
	return cfg, nil
}

// New opens the SSH session (TCP dial + handshake + auth + host-key verify),
// bounded by ctx and DialTimeout. A failure here is the user's "SSH reachable +
// authed?" answer and surfaces as a clear error before any UI is built. Call
// Discover next to resolve the daemon transport.
func New(ctx context.Context, cfg Config) (*Tunnel, error) {
	client, err := dialClient(ctx, cfg)
	if err != nil {
		return nil, err
	}
	return &Tunnel{cfg: cfg, client: client}, nil
}

// Discover resolves the remote daemon transport and stores it for DialContext.
// It honors ?socket=/?port= overrides, else reads ~/.gogent/daemon.addr over the
// session (preferring the optional --tcp http:// token), else falls back to the
// default ~/.gogent/daemon.sock — the same fallback the local readAddr uses.
func (t *Tunnel) Discover() (ResolvedTarget, error) {
	t.mu.Lock()
	client := t.client
	t.mu.Unlock()
	if client == nil {
		return ResolvedTarget{}, errors.New("ssh tunnel not connected")
	}
	tgt, err := discoverTarget(client, t.cfg)
	if err != nil {
		return ResolvedTarget{}, err
	}
	t.mu.Lock()
	t.target = tgt
	t.mu.Unlock()
	return tgt, nil
}

// DialContext opens an SSH channel to the resolved daemon transport. The network
// and address arguments are ignored (as the unix:// dialer ignores them): the
// channel target is the discovered socket or host:port. The returned net.Conn is
// backed by an ssh.Channel, so net/http drives it identically to a TCP conn.
func (t *Tunnel) DialContext(_ context.Context, _, _ string) (net.Conn, error) {
	t.mu.Lock()
	client, tgt := t.client, t.target
	t.mu.Unlock()
	if client == nil {
		return nil, errors.New("ssh tunnel not connected")
	}
	switch {
	case tgt.UnixSocket != "":
		return client.Dial("unix", tgt.UnixSocket)
	case tgt.TCPAddr != "":
		return client.Dial("tcp", tgt.TCPAddr)
	default:
		return nil, errors.New("ssh tunnel has no resolved daemon target (call Discover)")
	}
}

// Restart re-establishes the SSH session after a drop, for RemoteClient.reconnect.
// It probes the existing session first: a healthy session is reused untouched
// (redialed=false), so the common "the SSE stream dropped, not the tunnel" case
// costs nothing. Only a genuinely dead session is redialed + re-auth'd +
// re-Discovered (redialed=true), with the slow network dial done WITHOUT holding
// mu so concurrent DialContext/poll calls are not blocked for ~10s. ctx cancels
// the probe and the redial promptly (shutdown / "Retry now").
func (t *Tunnel) Restart(ctx context.Context) (redialed bool, err error) {
	t.mu.Lock()
	client := t.client
	t.mu.Unlock()

	if client != nil && probe(ctx, client) {
		return false, nil // session is alive — reuse it, no teardown
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if client != nil {
		// The session is dead: close it and drop the pointer so a failed redial
		// below leaves t.client nil (a clean "not connected") rather than a stale
		// closed handle. The publish at the end installs the fresh client.
		_ = client.Close()
		t.mu.Lock()
		if t.client == client {
			t.client = nil
		}
		t.mu.Unlock()
	}

	// Slow path: dial + discover OUTSIDE the lock, then publish under it.
	newClient, derr := dialClient(ctx, t.cfg)
	if derr != nil {
		return false, derr
	}
	tgt, terr := discoverTarget(newClient, t.cfg)
	if terr != nil {
		_ = newClient.Close()
		return false, terr
	}
	t.mu.Lock()
	t.client = newClient
	t.target = tgt
	t.mu.Unlock()
	return true, nil
}

// Close tears down the SSH session. The remote daemon keeps running. Safe to call
// more than once.
func (t *Tunnel) Close() error {
	t.mu.Lock()
	client := t.client
	t.client = nil
	t.mu.Unlock()
	if client != nil {
		return client.Close()
	}
	return nil
}

// probe sends an SSH global "keepalive" request: any reply (even REQUEST_FAILURE
// for the unrecognised request type) returns err==nil and proves the session is
// alive; a dead session errors. Bounded by probeTimeout and ctx so a wedged
// half-open session does not stall the caller.
func probe(ctx context.Context, client *ssh.Client) bool {
	done := make(chan bool, 1)
	go func() {
		_, _, err := client.SendRequest("keepalive@openssh.com", true, nil)
		done <- err == nil
	}()
	select {
	case alive := <-done:
		return alive
	case <-time.After(probeTimeout):
		return false
	case <-ctx.Done():
		return false
	}
}

// dialClient performs the TCP dial + SSH handshake + auth, bounded by ctx and
// DialTimeout. ctx cancellation aborts an in-flight handshake by closing the conn.
func dialClient(ctx context.Context, cfg Config) (*ssh.Client, error) {
	auths, err := authMethods(cfg)
	if err != nil {
		return nil, err
	}
	hostKey, err := hostKeyCallback(cfg)
	if err != nil {
		return nil, err
	}
	clientCfg := &ssh.ClientConfig{
		User:            cfg.User,
		Auth:            auths,
		HostKeyCallback: hostKey,
		Timeout:         DialTimeout,
	}

	addr := cfg.sshAddr()
	d := net.Dialer{Timeout: DialTimeout}
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", addr, err)
	}

	// Abort a slow handshake on ctx cancel (Config.Timeout only caps the
	// handshake's own deadline; ctx covers shutdown / "Retry now"). The nested
	// select guarantees we never close the conn once the handshake has completed —
	// otherwise a ctx cancel racing handshake completion would tear down the live
	// client's connection.
	hsDone := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			select {
			case <-hsDone: // handshake already finished — leave the conn alone
			default:
				_ = conn.Close()
			}
		case <-hsDone:
		}
	}()

	c, chans, reqs, err := ssh.NewClientConn(conn, addr, clientCfg)
	close(hsDone)
	if err != nil {
		_ = conn.Close()
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, fmt.Errorf("ssh connect %s: %w", addr, ctxErr)
		}
		return nil, fmt.Errorf("ssh handshake %s: %w", addr, err)
	}
	// Belt-and-suspenders for the close(hsDone) race: if ctx was cancelled in the
	// window where the watcher could still have closed conn, do not hand back a
	// client whose transport may already be torn down — fail cleanly instead.
	if ctxErr := ctx.Err(); ctxErr != nil {
		_ = c.Close()
		return nil, fmt.Errorf("ssh connect %s: %w", addr, ctxErr)
	}
	return ssh.NewClient(c, chans, reqs), nil
}

// discoverTarget resolves the daemon transport over an established client.
func discoverTarget(client *ssh.Client, cfg Config) (ResolvedTarget, error) {
	// Explicit overrides win and need no remote read.
	if cfg.DaemonSock != "" {
		return ResolvedTarget{UnixSocket: cfg.DaemonSock}, nil
	}
	if cfg.DaemonPort > 0 {
		return ResolvedTarget{TCPAddr: net.JoinHostPort("127.0.0.1", strconv.Itoa(cfg.DaemonPort))}, nil
	}

	out, err := runCommand(client, "cat ~/.gogent/daemon.addr")
	out = strings.TrimSpace(out)
	if err != nil || out == "" {
		// File absent → daemon likely not running. Fall back to the default
		// socket (matching local readAddr) so the subsequent Health() check
		// produces the actionable "no daemon" error rather than a vague one.
		home, herr := remoteHome(client)
		if herr != nil {
			return ResolvedTarget{}, fmt.Errorf("read ~/.gogent/daemon.addr and resolve $HOME: %w", herr)
		}
		return ResolvedTarget{UnixSocket: path.Join(home, ".gogent", "daemon.sock")}, nil
	}
	tgt, perr := parseAddr(out)
	if perr != nil {
		return ResolvedTarget{}, perr
	}
	return tgt, nil
}

// parseAddr parses the daemon.addr contents: a space-separated list whose first
// token is the primary transport (unix:///path or http://host:port) and whose
// optional second token (only with --tcp) is an http://host:port preferred for
// TCP dialing.
func parseAddr(s string) (ResolvedTarget, error) {
	tokens := strings.Fields(s)
	if len(tokens) == 0 {
		return ResolvedTarget{}, errors.New("empty daemon.addr")
	}
	// Prefer an explicit http:// TCP token (the --tcp listener) if present.
	for _, tok := range tokens[1:] {
		if strings.HasPrefix(tok, "http://") || strings.HasPrefix(tok, "https://") {
			hostport, err := hostPortOf(tok)
			if err != nil {
				return ResolvedTarget{}, err
			}
			return ResolvedTarget{TCPAddr: hostport}, nil
		}
	}
	first := tokens[0]
	switch {
	case strings.HasPrefix(first, "unix://"):
		sock := strings.TrimPrefix(first, "unix://")
		if sock == "" {
			return ResolvedTarget{}, fmt.Errorf("daemon.addr unix token %q has no path", first)
		}
		return ResolvedTarget{UnixSocket: sock}, nil
	case strings.HasPrefix(first, "http://"), strings.HasPrefix(first, "https://"):
		hostport, err := hostPortOf(first)
		if err != nil {
			return ResolvedTarget{}, err
		}
		return ResolvedTarget{TCPAddr: hostport}, nil
	default:
		return ResolvedTarget{}, fmt.Errorf("unrecognised daemon.addr transport %q", first)
	}
}

func hostPortOf(rawURL string) (string, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", fmt.Errorf("parse daemon transport %q: %w", rawURL, err)
	}
	if u.Host == "" {
		return "", fmt.Errorf("daemon transport %q has no host", rawURL)
	}
	return u.Host, nil
}

// runCommand runs a command over a one-shot SSH session and returns its stdout.
func runCommand(client *ssh.Client, cmd string) (string, error) {
	sess, err := client.NewSession()
	if err != nil {
		return "", fmt.Errorf("open ssh session: %w", err)
	}
	defer func() { _ = sess.Close() }()
	out, err := sess.Output(cmd)
	return string(out), err
}

// remoteHome resolves the remote login user's home directory (for the default
// socket fallback). gogent always roots its state at $HOME/.gogent.
func remoteHome(client *ssh.Client) (string, error) {
	out, err := runCommand(client, `printf %s "$HOME"`)
	if err != nil {
		return "", err
	}
	home := strings.TrimSpace(out)
	if home == "" {
		return "", errors.New("remote $HOME is empty")
	}
	return home, nil
}

// --- auth ------------------------------------------------------------------

// authMethods builds the SSH auth method list: the agent (SSH_AUTH_SOCK) first,
// then key files (an explicit --ssh-key, else the ~/.ssh/id_* defaults). At least
// one usable method is required.
func authMethods(cfg Config) ([]ssh.AuthMethod, error) {
	var methods []ssh.AuthMethod
	if m := agentAuth(); m != nil {
		methods = append(methods, m)
	}
	if m := keyAuth(cfg); m != nil {
		methods = append(methods, m)
	}
	if len(methods) == 0 {
		return nil, errors.New("no ssh auth available: start an ssh-agent (SSH_AUTH_SOCK) or pass --ssh-key")
	}
	return methods, nil
}

// agentAuth returns an auth method backed by a running ssh-agent, or nil if none
// is reachable.
func agentAuth() ssh.AuthMethod {
	sock := os.Getenv("SSH_AUTH_SOCK")
	if sock == "" {
		return nil
	}
	conn, err := net.Dial("unix", sock)
	if err != nil {
		return nil
	}
	// conn stays open for the tunnel's lifetime; the agent is consulted lazily
	// during each handshake (including Restart redials).
	return ssh.PublicKeysCallback(agent.NewClient(conn).Signers)
}

// keyAuth returns a public-key auth method over the loadable private keys, or nil
// if none load.
func keyAuth(cfg Config) ssh.AuthMethod {
	var signers []ssh.Signer
	for _, p := range keyPaths(cfg) {
		if s := loadSigner(p); s != nil {
			signers = append(signers, s)
		}
	}
	if len(signers) == 0 {
		return nil
	}
	return ssh.PublicKeys(signers...)
}

// keyPaths returns the private-key files to try: an explicit --ssh-key, else the
// conventional ~/.ssh defaults.
func keyPaths(cfg Config) []string {
	if cfg.KeyPath != "" {
		return []string{cfg.KeyPath}
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	sshDir := filepath.Join(home, ".ssh")
	return []string{
		filepath.Join(sshDir, "id_ed25519"),
		filepath.Join(sshDir, "id_ecdsa"),
		filepath.Join(sshDir, "id_rsa"),
	}
}

// loadSigner reads and parses a private key, prompting on a TTY for a passphrase
// when the key is encrypted. A missing/unparseable/wrong-passphrase key yields nil
// (it is simply skipped as an auth candidate).
func loadSigner(p string) ssh.Signer {
	b, err := os.ReadFile(p) //nolint:gosec // path is the user's own key file
	if err != nil {
		return nil
	}
	signer, err := ssh.ParsePrivateKey(b)
	if err == nil {
		return signer
	}
	var missing *ssh.PassphraseMissingError
	if errors.As(err, &missing) {
		pass, perr := promptPassphrase(p)
		if perr != nil || len(pass) == 0 {
			return nil
		}
		if s, e := ssh.ParsePrivateKeyWithPassphrase(b, pass); e == nil {
			return s
		}
	}
	return nil
}

// promptPassphrase reads a key passphrase from the controlling terminal (no echo).
// It fails when stdin is not a terminal, so non-interactive runs simply skip an
// encrypted key rather than blocking.
func promptPassphrase(keyPath string) ([]byte, error) {
	fd := int(os.Stdin.Fd())
	if !term.IsTerminal(fd) {
		return nil, errors.New("passphrase required but stdin is not a terminal")
	}
	fmt.Fprintf(os.Stderr, "Enter passphrase for %s: ", keyPath)
	pass, err := term.ReadPassword(fd)
	fmt.Fprintln(os.Stderr)
	return pass, err
}

// --- host-key verification -------------------------------------------------

// hostKeyCallback returns the host-key verifier: strict ~/.ssh/known_hosts by
// default (mismatch/unknown host → hard fail), or an explicit insecure bypass.
func hostKeyCallback(cfg Config) (ssh.HostKeyCallback, error) {
	if cfg.Insecure {
		return ssh.InsecureIgnoreHostKey(), nil //nolint:gosec // opt-in via --ssh-insecure-skip-verify
	}
	khPath := cfg.KnownHosts
	if khPath == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("resolve ~/.ssh/known_hosts: %w", err)
		}
		khPath = filepath.Join(home, ".ssh", "known_hosts")
	}
	cb, err := knownhosts.New(khPath)
	if err != nil {
		return nil, fmt.Errorf("load known_hosts %s: %w (add the host with ssh-keyscan, or pass --ssh-insecure-skip-verify)", khPath, err)
	}
	return func(hostname string, remote net.Addr, key ssh.PublicKey) error {
		if err := cb(hostname, remote, key); err != nil {
			var ke *knownhosts.KeyError
			if errors.As(err, &ke) && len(ke.Want) == 0 {
				return fmt.Errorf("unknown host %s: add it with `ssh-keyscan %s >> %s`, or pass --ssh-insecure-skip-verify",
					hostname, cfg.Host, khPath)
			}
			return fmt.Errorf("host key verification failed for %s: %w (pass --ssh-insecure-skip-verify only if you trust this host)", hostname, err)
		}
		return nil
	}, nil
}
