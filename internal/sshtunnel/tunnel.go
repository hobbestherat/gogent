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
	Host string // SSH host to dial (HostName from ~/.ssh/config if it overrode the alias)
	Port int    // SSH port; 0 → 22

	Token string // daemon bearer token (passed through to the APIClient)

	KeyPath    string // --ssh-key; "" → SSH agent + ~/.ssh/id_* defaults
	KnownHosts string // --ssh-known-hosts; "" → ~/.ssh/known_hosts
	Insecure   bool   // --ssh-insecure-skip-verify: skip host-key verification

	DaemonPort int    // ?port= override: dial the daemon over loopback TCP at this port
	DaemonSock string // ?socket= override: dial the daemon at this Unix socket path

	// ssh-config (issue #498): filled by ParseConnectURL from ~/.ssh/config as a
	// fallback for fields the URL/flags left unset. Explicit URL fields and CLI
	// flags always win; these only fill holes.
	Alias          string   // the original ssh:// host as typed (before HostName resolution), for diagnostics + the daemon-start hint
	IdentityFiles  []string // IdentityFile(s) from ~/.ssh/config, merged into the key candidates
	IdentitiesOnly bool     // IdentitiesOnly: offer only --ssh-key + IdentityFile keys (skip id_* defaults)
	ConfigFound    bool     // ~/.ssh/config actually CONTRIBUTED an honored value for cfg.Alias (for auth diagnostics)
	ConfigApplied  string   // human summary of what ssh-config applied (e.g. "User=pi HostName=192.168.1.5"); "" when nothing applied
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
// address into a Config, folding in the SSH auth/host-key flags. It then
// consults ~/.ssh/config (best-effort) for the host and fills in ONLY the fields
// the URL/flags left unset — HostName / User / Port / IdentityFile /
// IdentitiesOnly — so that `gogent --connect ssh://<alias>` honors a matching
// `Host` block the way `ssh <alias>` does (issue #498). Explicit URL fields and
// CLI flags always win; ssh-config is the fallback only. A missing user (URL +
// config) defaults to the current OS user. It validates the URL so a malformed
// --connect value fails before any network I/O.
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
		Alias:      host,
		User:       u.User.Username(),
		Token:      token,
		KeyPath:    keyPath,
		KnownHosts: knownHosts,
		Insecure:   insecure,
	}
	if p := u.Port(); p != "" {
		n, e := strconv.Atoi(p)
		if e != nil || n <= 0 {
			return Config{}, fmt.Errorf("ssh connect %q: bad ssh port %q", addr, p)
		}
		cfg.Port = n
	}
	// ssh-config fallback (issue #498): fill MISSING fields only — the URL user@
	// and :port set above, and the --ssh-* flags, take precedence. Best-effort:
	// a missing/broken config is non-fatal (rc.Found stays false). rc.Found gates
	// on a matching `Host` block: directives before the first Host line (top-level
	// globals) are intentionally NOT applied — issue #498 is scoped to `Host
	// <alias>` blocks, and applying a bare global HostName to every host would be
	// a footgun. (Globals still participate in first-value-wins WITHIN a matched
	// block, as TestReadSSHConfig_GlobalBeforeHostWins asserts.)
	// Capture whether the URL supplied user@/:port BEFORE the config block fills
	// them, so the "applied" summary can exclude a config User/Port the URL
	// overrode (the URL wins for the dial; the diagnostic must not claim config
	// applied a value it did not).
	urlHadUser := cfg.User != ""
	urlHadPort := cfg.Port != 0
	if rc, _ := ReadSSHConfig(host); rc.Found {
		if cfg.User == "" && rc.User != "" {
			cfg.User = rc.User
		}
		if cfg.Port == 0 && rc.Port > 0 {
			cfg.Port = rc.Port
		}
		if rc.HostName != "" {
			cfg.Host = rc.HostName // the real dial address; cfg.Alias keeps the typed name
		}
		cfg.IdentityFiles = rc.IdentityFiles
		cfg.IdentitiesOnly = rc.IdentitiesOnly
		// ConfigFound/ConfigApplied report what config ACTUALLY applied to the
		// dial — not merely what a Host block resolved. Two corrections matter:
		// (a) a stock /etc `Host *` that matches but sets nothing honored yields
		// "" → ConfigFound=false (no false "applied" claim); (b) a User/Port the
		// URL overrode is excluded, so the diagnostic never contradicts itself
		// ("attempted user=bob" vs "applied User=pi"). HostName/IdentityFile/
		// IdentitiesOnly have no URL override, so they report as-resolved.
		cfg.ConfigApplied = summarizeSSHConfig(rc, !urlHadUser, !urlHadPort)
		cfg.ConfigFound = cfg.ConfigApplied != ""
	}
	// OS-user fallback last, so a config User (above) wins over it.
	if cfg.User == "" {
		if cu, e := user.Current(); e == nil {
			cfg.User = cu.Username
		}
	}
	if cfg.User == "" {
		return Config{}, fmt.Errorf("ssh connect %q: no user; use ssh://user@host", addr)
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
		conn, err := client.Dial("unix", tgt.UnixSocket)
		if err != nil {
			return nil, fmt.Errorf("ssh dial unix %s: %w", tgt.UnixSocket, err)
		}
		return conn, nil
	case tgt.TCPAddr != "":
		conn, err := client.Dial("tcp", tgt.TCPAddr)
		if err != nil {
			return nil, fmt.Errorf("ssh dial tcp %s: %w", tgt.TCPAddr, err)
		}
		return conn, nil
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
		return false, fmt.Errorf("ssh tunnel restart: %w", err)
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
		if err := client.Close(); err != nil {
			return fmt.Errorf("close ssh tunnel: %w", err)
		}
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
		// Enrich an authentication failure so the user can see WHICH user, keys
		// and agent were attempted and whether ~/.ssh/config applied — the bare
		// "attempted methods [none publickey]" is otherwise inscrutable (#498).
		if isAuthError(err) {
			return nil, fmt.Errorf("ssh handshake %s: %w (%s)", addr, err, authDiagnostic(cfg))
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

// authMethods builds the SSH auth as a SINGLE "publickey" method whose signer
// callback offers the agent's signers (SSH_AUTH_SOCK) followed by the loaded
// file-key signers (an explicit --ssh-key, else the ~/.ssh/id_* defaults),
// de-duped by public-key blob. Merging matters: golang.org/x/crypto/ssh de-dupes
// candidate auth methods BY NAME, so two separate "publickey" methods (agent +
// files) would let the first one tried mask the second — an empty ssh-agent
// would shadow a valid IdentityFile key, and a bad file key would shadow a valid
// agent key (issue #502). Offering every candidate within one method makes that
// impossible: the server sees them all. At least one candidate source (a
// reachable agent OR a loadable key file) is required.
func authMethods(cfg Config) ([]ssh.AuthMethod, error) {
	// File-key signers are loaded eagerly here (as keyAuth did before): this is
	// where loadSigner's TTY passphrase prompt fires, and it lets us decide the
	// "nothing to try" gate below without prompting twice. authMethods is re-run
	// on every dialClient (including Restart redials), so this is not a one-time
	// snapshot — a redial re-loads the files just as before.
	fileSigners := fileSigners(cfg)

	// Agent conn: dialed ONCE here, captured by the callback, and queried LAZILY
	// (agent.NewClient(conn).Signers()) INSIDE the callback during the handshake —
	// so a Restart redial, which re-runs authMethods and re-dials the agent, picks
	// up keys added to the agent after the tunnel opened. The conn MUST stay open
	// for the handshake's lifetime (not be closed right after listing): the signers
	// that Signers() returns sign by calling back over this same conn, and signing
	// happens AFTER this callback returns (x/crypto lists candidate keys first,
	// then asks the chosen one to sign). It is captured by the closure, which lives
	// through ssh.NewClientConn; afterwards it is unreferenced and the fd finalizer
	// reclaims it — one agent conn per dial/redial, matching the pre-fix agentAuth.
	agentConn := dialAgent()

	// Preserve the existing gate: an unreachable agent AND zero loadable file keys
	// is the only "nothing to try" case. A reachable-but-empty agent is NOT an
	// error here (as before: agentAuth returned non-nil regardless of key count) —
	// the handshake proceeds and, if nothing is accepted, fails with the server's
	// message enriched by authDiagnostic.
	if agentConn == nil && len(fileSigners) == 0 {
		return nil, errors.New("no ssh auth available: start an ssh-agent (SSH_AUTH_SOCK) or pass --ssh-key")
	}

	return []ssh.AuthMethod{
		ssh.PublicKeysCallback(func() ([]ssh.Signer, error) {
			var signers []ssh.Signer
			// Agent first (queried lazily, per handshake), then file keys —
			// preserving the pre-fix order (agentAuth was appended before keyAuth)
			// and OpenSSH's "agent identities before on-disk keys" spirit. De-dupe
			// by marshaled public-key blob so a key present in both the agent and a
			// file is offered once; agent-first means the agent's (already-unlocked)
			// copy wins the dup.
			if agentConn != nil {
				if as, err := agent.NewClient(agentConn).Signers(); err == nil {
					signers = append(signers, as...)
				}
			}
			signers = append(signers, fileSigners...)
			return dedupeSigners(signers), nil
		}),
	}, nil
}

// isAuthError reports whether an ssh handshake error is an authentication
// failure (vs a transport/protocol error), so dialClient only enriches the
// actionable case. golang.org/x/crypto/ssh surfaces auth failures as a plain
// error whose message names the attempted methods, so we match on its text.
func isAuthError(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "unable to authenticate") ||
		strings.Contains(s, "no supported methods remain") ||
		strings.Contains(s, "ssh: handshake failed")
}

// authDiagnostic renders a one-line summary of what the auth attempt offered:
// the login user, the candidate key files (with a loaded/absent/encrypted note,
// PATHS ONLY — never key contents), whether the agent was consulted (and how
// many keys it holds), and whether/what ~/.ssh/config applied for the host. It
// is built only on the failure path, so it may do a little extra I/O.
func authDiagnostic(cfg Config) string {
	var b strings.Builder
	fmt.Fprintf(&b, "attempted user=%s", cfg.User)

	keys := keyPaths(cfg)
	if len(keys) == 0 {
		b.WriteString("; keys=[none]")
	} else {
		b.WriteString("; keys=[")
		for i, p := range keys {
			if i > 0 {
				b.WriteString(", ")
			}
			fmt.Fprintf(&b, "%s (%s)", p, classifyKey(p))
		}
		b.WriteString("]")
	}

	fmt.Fprintf(&b, "; agent=%s", agentSummary())

	alias := cfg.Alias
	if alias == "" {
		alias = cfg.Host
	}
	// Report ONLY what ssh-config actually contributed (cfg.ConfigApplied), not
	// the merged/default dial values — otherwise a stock `Host *` in
	// /etc/ssh/ssh_config makes this falsely claim config set the user/port.
	if cfg.ConfigFound {
		fmt.Fprintf(&b, "; ~/.ssh/config: applied for %q: %s", alias, cfg.ConfigApplied)
	} else {
		fmt.Fprintf(&b, "; ~/.ssh/config: no values applied for %q", alias)
	}
	return b.String()
}

// summarizeSSHConfig renders the honored directives ssh-config actually APPLIED
// to the dial (and nothing else) as a compact "User=… HostName=… …" string, for
// the auth diagnostic. userApplied/portApplied gate User/Port so a value the URL
// overrode is omitted (HostName/IdentityFile/IdentitiesOnly have no URL override
// and are reported as-resolved). It returns "" when config applied nothing — the
// signal cfg.ConfigFound is derived from.
func summarizeSSHConfig(rc ResolvedSSHConfig, userApplied, portApplied bool) string {
	var parts []string
	if userApplied && rc.User != "" {
		parts = append(parts, "User="+rc.User)
	}
	if rc.HostName != "" {
		parts = append(parts, "HostName="+rc.HostName)
	}
	if portApplied && rc.Port > 0 {
		parts = append(parts, "Port="+strconv.Itoa(rc.Port))
	}
	if rc.IdentitiesOnly {
		parts = append(parts, "IdentitiesOnly=yes")
	}
	if len(rc.IdentityFiles) > 0 {
		parts = append(parts, "IdentityFile="+strings.Join(rc.IdentityFiles, ","))
	}
	return strings.Join(parts, " ")
}

// classifyKey reports a private-key file's state for diagnostics WITHOUT leaking
// its contents and WITHOUT prompting for a passphrase: absent / loaded /
// encrypted / unreadable.
func classifyKey(p string) string {
	b, err := os.ReadFile(p) //nolint:gosec // path is the user's own key file; contents are not logged
	if err != nil {
		if os.IsNotExist(err) {
			return "absent"
		}
		return "unreadable"
	}
	if _, err := ssh.ParsePrivateKey(b); err == nil {
		return "loaded"
	} else {
		var missing *ssh.PassphraseMissingError
		if errors.As(err, &missing) {
			return "encrypted"
		}
	}
	return "unparseable"
}

// agentSummary describes the ssh-agent for diagnostics, bounded by a short
// deadline so an already-failing connect never stalls on a wedged SSH_AUTH_SOCK.
func agentSummary() string {
	sock := os.Getenv("SSH_AUTH_SOCK")
	if sock == "" {
		return "none (SSH_AUTH_SOCK unset)"
	}
	conn, err := net.DialTimeout("unix", sock, 500*time.Millisecond) //nolint:gosec // trusted local agent socket from the user environment
	if err != nil {
		return "present (unreachable)"
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(500 * time.Millisecond))
	keys, err := agent.NewClient(conn).List()
	if err != nil {
		return "present (key count unavailable)"
	}
	return fmt.Sprintf("present (%d keys)", len(keys))
}

// dialAgent dials a running ssh-agent via SSH_AUTH_SOCK and returns the open
// connection, or nil if SSH_AUTH_SOCK is unset or undialable. The caller keeps
// the conn open for the handshake's lifetime and queries it lazily (see
// authMethods), so newly-added agent keys are picked up on a Restart redial and
// the returned signers can still reach the agent to sign.
func dialAgent() net.Conn {
	sock := os.Getenv("SSH_AUTH_SOCK")
	if sock == "" {
		return nil
	}
	conn, err := net.Dial("unix", sock) //nolint:gosec // SSH_AUTH_SOCK is a trusted local agent socket path from the user environment, not attacker-controlled
	if err != nil {
		return nil
	}
	return conn
}

// fileSigners loads the signers for every loadable private key in keyPaths
// (skipping missing/unparseable/wrong-passphrase keys), in keyPaths order. It is
// the file-key half of authMethods' candidate set; loadSigner still prompts for
// a passphrase on a TTY for encrypted keys.
func fileSigners(cfg Config) []ssh.Signer {
	var signers []ssh.Signer
	for _, p := range keyPaths(cfg) {
		if s := loadSigner(p); s != nil {
			signers = append(signers, s)
		}
	}
	return signers
}

// dedupeSigners removes signers sharing a public key, preserving first-seen
// order. Keyed on the marshaled public-key blob (the canonical SSH identity), so
// a key present in both the agent and a file is offered to the server once —
// each offer otherwise costs a separate attempt against the server's MaxAuthTries.
func dedupeSigners(in []ssh.Signer) []ssh.Signer {
	if len(in) <= 1 {
		return in
	}
	seen := make(map[string]struct{}, len(in))
	out := make([]ssh.Signer, 0, len(in))
	for _, s := range in {
		k := string(s.PublicKey().Marshal())
		if _, ok := seen[k]; ok {
			continue
		}
		seen[k] = struct{}{}
		out = append(out, s)
	}
	return out
}

// keyPaths returns the private-key files to try, in order. An explicit --ssh-key
// is authoritative: it (plus any ~/.ssh/config IdentityFile) REPLACES the
// ~/.ssh/id_* defaults, preserving the flag's "default: … id_*" contract. With
// no --ssh-key, config IdentityFile(s) come first, then the id_* defaults UNLESS
// IdentitiesOnly suppresses them (issue #498). The list is de-duped (preserving
// order) so an IdentityFile that coincides with an id_* default is not offered
// twice — each key is a separate attempt against the server's MaxAuthTries.
func keyPaths(cfg Config) []string {
	if cfg.KeyPath != "" {
		return dedupPaths(append([]string{cfg.KeyPath}, cfg.IdentityFiles...))
	}
	paths := append([]string{}, cfg.IdentityFiles...)
	if !cfg.IdentitiesOnly {
		if home, err := os.UserHomeDir(); err == nil {
			sshDir := filepath.Join(home, ".ssh")
			paths = append(paths,
				filepath.Join(sshDir, "id_ed25519"),
				filepath.Join(sshDir, "id_ecdsa"),
				filepath.Join(sshDir, "id_rsa"),
			)
		}
	}
	return dedupPaths(paths)
}

// dedupPaths removes duplicate paths while preserving first-seen order.
func dedupPaths(in []string) []string {
	if len(in) <= 1 {
		return in
	}
	seen := make(map[string]struct{}, len(in))
	out := in[:0:0]
	for _, p := range in {
		if _, ok := seen[p]; ok {
			continue
		}
		seen[p] = struct{}{}
		out = append(out, p)
	}
	return out
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
	if err != nil {
		return nil, fmt.Errorf("read passphrase: %w", err)
	}
	return pass, nil
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
