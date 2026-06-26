package sshtunnel

// Tests for the native ssh:// transport (issue #482). The pure helpers
// (ParseConnectURL, parseAddr, hostPortOf) are unit-tested directly; the
// networked pieces (New/Discover/DialContext/Restart/Close, host-key
// verification, bounded dial) are exercised against an in-process
// golang.org/x/crypto/ssh server that:
//   - answers `cat ~/.gogent/daemon.addr` and `printf %s "$HOME"` exec channels
//     (so Discover's discovery + fallback paths run for real),
//   - forwards direct-streamlocal@openssh.com channels to a Unix-socket daemon,
//   - forwards direct-tcpip channels to a loopback TCP daemon,
// so the *real* client code path (Tunnel.DialContext -> ssh.Client.Dial) is
// driven end-to-end, not stubbed.

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

// prevent accidental interference from the developer's real ssh-agent / keys:
// every integration test offers only the generated test key.
func disableRealSSHEnv(t *testing.T) {
	t.Helper()
	t.Setenv("SSH_AUTH_SOCK", "")
}

// --------------------------------------------------------------------------------
// Pure unit tests
// --------------------------------------------------------------------------------

func TestParseConnectURL(t *testing.T) {
	currentUser := "" // the default ParseConnectURL falls back to
	if cu, err := user.Current(); err == nil {
		currentUser = cu.Username
	}

	tests := []struct {
		name      string
		addr      string
		wantErr   bool
		wantHost  string
		wantUser  string
		wantPort  int
		wantDPort int
		wantDSock string
	}{
		{name: "user host", addr: "ssh://alice@machineB", wantHost: "machineB", wantUser: "alice", wantPort: 0},
		{name: "host only defaults user", addr: "ssh://machineB", wantHost: "machineB", wantUser: currentUser, wantPort: 0},
		{name: "user host sshport", addr: "ssh://bob@host:2222", wantHost: "host", wantUser: "bob", wantPort: 2222},
		{name: "ipv4 host port", addr: "ssh://10.0.0.5:2200", wantHost: "10.0.0.5", wantPort: 2200, wantUser: currentUser},
		{name: "daemon port query", addr: "ssh://h?port=8080", wantHost: "h", wantDPort: 8080, wantUser: currentUser},
		{name: "daemon socket query", addr: "ssh://h?socket=/custom/sock", wantHost: "h", wantDSock: "/custom/sock", wantUser: currentUser},
		{name: "port+sshport+user", addr: "ssh://u@h:2222?port=9000", wantHost: "h", wantUser: "u", wantPort: 2222, wantDPort: 9000},
		{name: "empty host", addr: "ssh://", wantErr: true},
		{name: "user empty host", addr: "ssh://alice@", wantErr: true},
		{name: "port only no host", addr: "ssh://:22", wantErr: true},
		{name: "bad ssh port alpha", addr: "ssh://h:abc", wantErr: true},
		{name: "bad ssh port zero", addr: "ssh://h:0", wantErr: true},
		{name: "bad ssh port negative", addr: "ssh://h:-1", wantErr: true},
		{name: "bad daemon port", addr: "ssh://h?port=abc", wantErr: true},
		{name: "bad daemon port zero", addr: "ssh://h?port=0", wantErr: true},
		{name: "non-ssh scheme", addr: "http://h", wantErr: true},
		{name: "unix scheme", addr: "unix:///x", wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := ParseConnectURL(tc.addr, "tok", "/key", "/kh", false)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("ParseConnectURL(%q) want error, got nil %+v", tc.addr, cfg)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseConnectURL(%q) unexpected error: %v", tc.addr, err)
			}
			if cfg.Host != tc.wantHost {
				t.Errorf("host = %q, want %q", cfg.Host, tc.wantHost)
			}
			if tc.wantUser != "" && cfg.User != tc.wantUser {
				t.Errorf("user = %q, want %q", cfg.User, tc.wantUser)
			}
			if cfg.Port != tc.wantPort {
				t.Errorf("port = %d, want %d", cfg.Port, tc.wantPort)
			}
			if cfg.DaemonPort != tc.wantDPort {
				t.Errorf("daemon port = %d, want %d", cfg.DaemonPort, tc.wantDPort)
			}
			if cfg.DaemonSock != tc.wantDSock {
				t.Errorf("daemon sock = %q, want %q", cfg.DaemonSock, tc.wantDSock)
			}
			if cfg.Token != "tok" || cfg.KeyPath != "/key" || cfg.KnownHosts != "/kh" {
				t.Errorf("auth fields not folded through: %+v", cfg)
			}
		})
	}
}

func TestParseAddr(t *testing.T) {
	tests := []struct {
		name     string
		in       string
		wantErr  bool
		wantUnix string
		wantTCP  string
	}{
		{name: "unix primary", in: "unix:///home/u/.gogent/daemon.sock", wantUnix: "/home/u/.gogent/daemon.sock"},
		{name: "unix primary trailing newline/spaces", in: "  unix:///x/sock\n", wantUnix: "/x/sock"},
		{name: "windows http primary only", in: "http://127.0.0.1:8080", wantTCP: "127.0.0.1:8080"},
		{name: "unix primary plus tcp preferred", in: "unix:///a/sock http://127.0.0.1:8080", wantTCP: "127.0.0.1:8080"},
		{name: "unix primary plus tcp preferred order independent of http pos", in: "unix:///a/sock http://127.0.0.1:9", wantTCP: "127.0.0.1:9"},
		{name: "https tcp token", in: "unix:///a/sock https://h:443", wantTCP: "h:443"},
		{name: "empty", in: "", wantErr: true},
		{name: "only whitespace", in: "   \n\t ", wantErr: true},
		{name: "unrecognised token", in: "foobarbaz", wantErr: true},
		{name: "unix token no path", in: "unix://", wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tgt, err := parseAddr(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("parseAddr(%q) want error, got %+v", tc.in, tgt)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseAddr(%q) unexpected error: %v", tc.in, err)
			}
			if tc.wantUnix != "" && tgt.UnixSocket != tc.wantUnix {
				t.Errorf("unix = %q, want %q", tgt.UnixSocket, tc.wantUnix)
			}
			if tc.wantTCP != "" && tgt.TCPAddr != tc.wantTCP {
				t.Errorf("tcp = %q, want %q", tgt.TCPAddr, tc.wantTCP)
			}
			// exactly one set
			if (tgt.UnixSocket == "") == (tgt.TCPAddr == "") {
				t.Errorf("exactly one transport must be set, got %+v", tgt)
			}
		})
	}
}

// TestParseAddrTCPNoPath documents a parsing edge: an http token with no port
// yields a port-less TCPAddr (the real daemon always writes host:port, so this
// only matters for a hand-edited daemon.addr). It is not silently rejected.
func TestParseAddrTCPNoPort(t *testing.T) {
	tgt, err := parseAddr("http://127.0.0.1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if tgt.TCPAddr != "127.0.0.1" {
		t.Fatalf("TCPAddr = %q, want 127.0.0.1 (port-less is accepted, not normalised)", tgt.TCPAddr)
	}
}

// --------------------------------------------------------------------------------
// In-process SSH server + test daemon harness
// --------------------------------------------------------------------------------

// sshTestEnv is a live SSH server plus a real (unix + tcp) HTTP daemon it can
// forward to. addrContent is what `cat ~/.gogent/daemon.addr` returns; an empty
// value makes that exec fail (exit 1) to exercise the absent-file fallback.
type sshTestEnv struct {
	sshAddr    string // "127.0.0.1:PORT" of the SSH server
	clientKey  string // path to a PEM private key the server accepts
	hostPub    ssh.PublicKey
	hostLine   string // "[host]:port" form for known_hosts
	daemonUnix string // unix socket path of the test daemon
	daemonTCP  string // "127.0.0.1:PORT" of the test daemon TCP listener

	addrContent atomic.Value // string returned by `cat .../daemon.addr` ("" => exec fails)
	home        string       // returned by `printf %s "$HOME"`
	accepts     atomic.Int64 // number of SSH TCP conns accepted (for Restart assertions)

	close func()
}

func newSSHTestEnv(t *testing.T) *sshTestEnv {
	t.Helper()
	disableRealSSHEnv(t)

	// --- keys: a host key (server signs) and a client key (client auths) ---
	hostPriv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("host key: %v", err)
	}
	hostSigner, err := ssh.NewSignerFromKey(hostPriv)
	if err != nil {
		t.Fatalf("host signer: %v", err)
	}
	hostPub := hostSigner.PublicKey()

	clientPriv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("client key: %v", err)
	}
	clientPubKey, err := ssh.NewPublicKey(&clientPriv.PublicKey)
	if err != nil {
		t.Fatalf("client pub: %v", err)
	}
	clientPubBytes := clientPubKey.Marshal()
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(clientPriv)})
	keyFile := filepath.Join(t.TempDir(), "id_rsa")
	if err := os.WriteFile(keyFile, keyPEM, 0o600); err != nil {
		t.Fatalf("write client key: %v", err)
	}

	env := &sshTestEnv{clientKey: keyFile, hostPub: hostPub, home: "/home/test"}

	// --- SSH server listener ---
	sshLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("ssh listen: %v", err)
	}
	env.sshAddr = sshLn.Addr().String()
	host, port, _ := net.SplitHostPort(env.sshAddr)
	env.hostLine = knownhosts.Line([]string{knownhosts.Normalize(net.JoinHostPort(host, port))}, hostPub)

	serverCfg := &ssh.ServerConfig{
		MaxAuthTries: 6,
		PublicKeyCallback: func(_ ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			if bytes.Equal(key.Marshal(), clientPubBytes) {
				return nil, nil
			}
			return nil, errors.New("unknown public key")
		},
	}
	serverCfg.AddHostKey(hostSigner)

	serveDone := make(chan struct{})
	go func() {
		defer close(serveDone)
		for {
			c, err := sshLn.Accept()
			if err != nil {
				return
			}
			env.accepts.Add(1)
			go env.handleServerConn(c, serverCfg)
		}
	}()

	// --- test daemon: one HTTP handler on a unix socket + a loopback TCP port ---
	mux := http.NewServeMux()
	mux.HandleFunc("/api/health", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/api/sessions", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("[]"))
	})

	unixLn, err := net.Listen("unix", filepath.Join(t.TempDir(), "daemon.sock"))
	if err != nil {
		t.Fatalf("unix listen: %v", err)
	}
	env.daemonUnix = unixLn.Addr().String()
	unixSrv := &http.Server{Handler: mux, ReadHeaderTimeout: time.Second}

	tcpLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("tcp listen: %v", err)
	}
	env.daemonTCP = tcpLn.Addr().String()
	tcpSrv := &http.Server{Handler: mux, ReadHeaderTimeout: time.Second}

	srvDone := make(chan struct{})
	go func() { _ = unixSrv.Serve(unixLn) }()
	go func() { _ = tcpSrv.Serve(tcpLn) }()
	go func() { defer close(srvDone); <-srvDone }()

	// Default: a socket-only daemon (the headline "no --tcp" case).
	env.setAddr("unix://" + env.daemonUnix)

	env.close = func() {
		_ = sshLn.Close()
		_ = unixSrv.Close()
		_ = tcpSrv.Close()
		<-serveDone
	}
	t.Cleanup(env.close)
	return env
}

func (e *sshTestEnv) setAddr(s string) { e.addrContent.Store(s) }

func (e *sshTestEnv) handleServerConn(c net.Conn, cfg *ssh.ServerConfig) {
	defer c.Close()
	sconn, chans, globs, err := ssh.NewServerConn(c, cfg)
	if err != nil {
		return
	}
	defer sconn.Close()
	go ssh.DiscardRequests(globs) // replies to keepalive@openssh.com (probes liveness)

	for newCh := range chans {
		switch newCh.ChannelType() {
		case "session":
			e.handleSession(newCh)
		case "direct-tcpip":
			e.forward(newCh, "tcp")
		case "direct-streamlocal@openssh.com":
			e.forward(newCh, "unix")
		default:
			_ = newCh.Reject(ssh.UnknownChannelType, "unsupported channel type")
		}
	}
}

func (e *sshTestEnv) handleSession(newCh ssh.NewChannel) {
	ch, reqs, err := newCh.Accept()
	if err != nil {
		return
	}
	defer ch.Close()
	for req := range reqs {
		if req.Type != "exec" {
			if req.WantReply {
				_ = req.Reply(false, nil)
			}
			continue
		}
		var ex struct{ Command string }
		_ = ssh.Unmarshal(req.Payload, &ex)
		_ = req.Reply(true, nil)

		out, code := e.execOutput(ex.Command)
		if out != "" {
			_, _ = ch.Write([]byte(out))
		}
		_, _ = ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Code uint32 }{Code: code}))
		return
	}
}

// execOutput simulates the two commands the tunnel issues over SSH.
func (e *sshTestEnv) execOutput(cmd string) (string, uint32) {
	switch {
	case strings.Contains(cmd, "daemon.addr"):
		if v, ok := e.addrContent.Load().(string); ok && v != "" {
			return v, 0
		}
		return "", 1 // file not found → drives the default-socket fallback
	case strings.Contains(cmd, "$HOME") || strings.Contains(cmd, "printf"):
		return e.home, 0
	default:
		return "", 1
	}
}

// forward accepts a direct-tcpip / direct-streamlocal channel and pipes it to a
// fresh connection to the real test daemon, exercising the client's
// ssh.Client.Dial path for real.
func (e *sshTestEnv) forward(newCh ssh.NewChannel, network string) {
	var dest string
	switch network {
	case "tcp":
		var msg struct {
			Addr       string
			Port       uint32
			OriginAddr string
			OriginPort uint32
		}
		if err := ssh.Unmarshal(newCh.ExtraData(), &msg); err != nil {
			_ = newCh.Reject(ssh.ConnectionFailed, "bad payload")
			return
		}
		dest = net.JoinHostPort(msg.Addr, strconv.Itoa(int(msg.Port)))
	case "unix":
		var msg struct {
			SocketPath string
			Reserved0  string
			Reserved1  uint32
		}
		if err := ssh.Unmarshal(newCh.ExtraData(), &msg); err != nil {
			_ = newCh.Reject(ssh.ConnectionFailed, "bad payload")
			return
		}
		dest = msg.SocketPath
	}
	upstream, err := net.Dial(network, dest)
	if err != nil {
		_ = newCh.Reject(ssh.ConnectionFailed, err.Error())
		return
	}
	ch, reqs, err := newCh.Accept()
	if err != nil {
		_ = upstream.Close()
		return
	}
	go ssh.DiscardRequests(reqs)
	pipe(ch, upstream)
}

// pipe shuttles bytes both ways and closes both ends when either side ends.
func pipe(a io.ReadWriteCloser, b io.ReadWriteCloser) {
	done := make(chan struct{}, 2)
	closeBoth := func() {
		_ = a.Close()
		_ = b.Close()
	}
	go func() { _, _ = io.Copy(b, a); closeBoth(); done <- struct{}{} }()
	go func() { _, _ = io.Copy(a, b); closeBoth(); done <- struct{}{} }()
	<-done
}

// insecureCfg is the standard client Config for happy-path tests (host-key
// verification off, the generated test key). Config.Host is the host ONLY —
// sshAddr() re-adds the port via JoinHostPort, matching how ParseConnectURL
// splits u.Hostname()/u.Port().
func (e *sshTestEnv) insecureCfg() Config {
	host, port, _ := net.SplitHostPort(e.sshAddr)
	p, _ := strconv.Atoi(port)
	return Config{Host: host, Port: p, User: "test", KeyPath: e.clientKey, Insecure: true}
}

// dialEnvNew opens a tunnel against the env and fails the test on any error.
func (e *sshTestEnv) mustNew(t *testing.T) *Tunnel {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	tun, err := New(ctx, e.insecureCfg())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return tun
}

// --------------------------------------------------------------------------------
// End-to-end transport tests (criterion #1: socket-only AND --tcp reachable)
// --------------------------------------------------------------------------------

// TestEndToEnd_UnixSocketDaemon is the headline case: a daemon started with plain
// `gogent daemon start` (socket-only, no --tcp) is reached over a
// direct-streamlocal channel and an HTTP request lands. Proves "--tcp NOT required".
func TestEndToEnd_UnixSocketDaemon(t *testing.T) {
	env := newSSHTestEnv(t) // default addrContent is the unix socket
	tun := env.mustNew(t)
	defer tun.Close()

	tgt, err := tun.Discover()
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if tgt.UnixSocket != env.daemonUnix {
		t.Fatalf("resolved unix = %q, want %q", tgt.UnixSocket, env.daemonUnix)
	}
	if tgt.TCPAddr != "" {
		t.Fatalf("socket-only daemon must resolve to a Unix target, got TCP %q", tgt.TCPAddr)
	}

	if err := healthOver(tun); err != nil {
		t.Fatalf("GET /api/health over the streamlocal tunnel failed: %v", err)
	}
}

// TestEndToEnd_TCPDaemonPrefersTCPToken: a --tcp daemon's daemon.addr carries a
// 2nd http:// token which must be PREFERRED for the TCP dial (direct-tcpip).
func TestEndToEnd_TCPDaemonPrefersTCPToken(t *testing.T) {
	env := newSSHTestEnv(t)
	env.setAddr(fmt.Sprintf("unix://%s http://%s", env.daemonUnix, env.daemonTCP))
	tun := env.mustNew(t)
	defer tun.Close()

	tgt, err := tun.Discover()
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if tgt.TCPAddr != env.daemonTCP {
		t.Fatalf("resolved tcp = %q, want --tcp token %q", tgt.TCPAddr, env.daemonTCP)
	}
	if tgt.UnixSocket != "" {
		t.Fatalf("expected TCP target, also got unix %q", tgt.UnixSocket)
	}
	if err := healthOver(tun); err != nil {
		t.Fatalf("GET /api/health over the direct-tcpip tunnel failed: %v", err)
	}
}

// TestEndToEnd_AbsentAddrFallsBackToDefaultSocket: a missing daemon.addr (daemon
// not running) resolves to $HOME/.gogent/daemon.sock, matching local readAddr —
// not an outright error (the subsequent Health() surfaces the actionable failure).
func TestEndToEnd_AbsentAddrFallsBackToDefaultSocket(t *testing.T) {
	env := newSSHTestEnv(t)
	env.setAddr("") // `cat` now exits 1
	env.home = "/home/charlie"
	tun := env.mustNew(t)
	defer tun.Close()

	tgt, err := tun.Discover()
	if err != nil {
		t.Fatalf("Discover on absent daemon.addr should fall back, got error: %v", err)
	}
	want := "/home/charlie/.gogent/daemon.sock"
	if tgt.UnixSocket != want {
		t.Fatalf("fallback unix = %q, want %q", tgt.UnixSocket, want)
	}
	// The fallback socket is not actually listening, so a dial must fail cleanly
	// (this is the path that becomes the "no daemon found" error in runAttached).
	conn, derr := tun.DialContext(context.Background(), "", "")
	if derr == nil {
		_ = conn.Close()
		t.Fatalf("dial of the non-listening fallback socket should fail, got %v", conn)
	}
}

// TestDiscover_QueryOverrides: ?socket= and ?port= win over discovery.
func TestDiscover_QueryOverrides(t *testing.T) {
	env := newSSHTestEnv(t)
	env.setAddr(fmt.Sprintf("unix://%s http://%s", env.daemonUnix, env.daemonTCP))

	t.Run("socket override", func(t *testing.T) {
		cfg := env.insecureCfg()
		cfg.DaemonSock = env.daemonUnix // explicit override
		tun := env.mustNewCfg(t, cfg)
		defer tun.Close()
		tgt, err := tun.Discover()
		if err != nil {
			t.Fatalf("Discover: %v", err)
		}
		if tgt.UnixSocket != env.daemonUnix || tgt.TCPAddr != "" {
			t.Fatalf("?socket= override should force a Unix target, got %+v", tgt)
		}
	})
	t.Run("port override forces loopback tcp", func(t *testing.T) {
		// ?port= always dials 127.0.0.1 (documented behaviour); the test daemon's
		// TCP listener is loopback, so this still connects.
		cfg := env.insecureCfg()
		port, _ := strconv.Atoi(strings.Split(env.daemonTCP, ":")[1])
		cfg.DaemonPort = port
		tun := env.mustNewCfg(t, cfg)
		defer tun.Close()
		tgt, err := tun.Discover()
		if err != nil {
			t.Fatalf("Discover: %v", err)
		}
		if !strings.HasPrefix(tgt.TCPAddr, "127.0.0.1:") {
			t.Fatalf("?port= should resolve to a loopback TCP target, got %q", tgt.TCPAddr)
		}
	})
}

// healthOver issues a single GET /api/health through the tunnel's DialContext
// (exactly how the APIClient drives it), asserting a 200.
func healthOver(tun *Tunnel) error {
	tr := &http.Transport{
		DialContext:       tun.DialContext,
		DisableKeepAlives: false,
	}
	defer tr.CloseIdleConnections()
	c := &http.Client{Transport: tr, Timeout: 5 * time.Second}
	req, err := http.NewRequest(http.MethodGet, "http://ssh/api/health", nil)
	if err != nil {
		return err
	}
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("health status = %d, want 200", resp.StatusCode)
	}
	return nil
}

// --------------------------------------------------------------------------------
// Restart semantics (criterion #2/#4 reconnect, round-1/round-2 fixes)
// --------------------------------------------------------------------------------

// TestRestart_ProbeSkipsRedial: a healthy session is reused — Restart returns
// redialed=false and the server accepts NO new TCP connection.
func TestRestart_ProbeSkipsRedial(t *testing.T) {
	env := newSSHTestEnv(t)
	tun := env.mustNew(t)
	defer tun.Close()
	if _, err := tun.Discover(); err != nil {
		t.Fatalf("Discover: %v", err)
	}
	before := env.accepts.Load()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	redialed, err := tun.Restart(ctx)
	cancel()
	if err != nil {
		t.Fatalf("Restart on live session: %v", err)
	}
	if redialed {
		t.Fatalf("Restart on a live session should report redialed=false, got true")
	}
	if got := env.accepts.Load(); got != before {
		t.Fatalf("live-session Restart opened a new connection: accepts %d -> %d (no redial expected)", before, got)
	}
}

// TestRestart_RedialsDeadSession: after the underlying session is killed, Restart
// must probe-fail, redial (a new accepted connection) and report redialed=true.
func TestRestart_RedialsDeadSession(t *testing.T) {
	env := newSSHTestEnv(t)
	tun := env.mustNew(t)
	if _, err := tun.Discover(); err != nil {
		t.Fatalf("Discover: %v", err)
	}
	before := env.accepts.Load()

	// Kill the live client from the outside (simulates a dropped session),
	// leaving the Tunnel's pointer in place so Restart must recover it.
	if tun.client == nil {
		t.Fatal("tunnel has no client after New")
	}
	_ = tun.client.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	redialed, err := tun.Restart(ctx)
	cancel()
	if err != nil {
		t.Fatalf("Restart after dead session: %v", err)
	}
	if !redialed {
		t.Fatalf("Restart on a dead session should report redialed=true, got false")
	}
	if got := env.accepts.Load(); got <= before {
		t.Fatalf("dead-session Restart should open a new connection: accepts %d -> %d", before, got)
	}
	// The recovered tunnel must actually work.
	if err := healthOver(tun); err != nil {
		t.Fatalf("health over recovered tunnel failed: %v", err)
	}
}

// TestRestart_HonorsCancelledContext: a cancelled ctx short-circuits Restart.
func TestRestart_HonoursCancelledContext(t *testing.T) {
	env := newSSHTestEnv(t)
	tun := env.mustNew(t)
	defer tun.Close()
	if _, err := tun.Discover(); err != nil {
		t.Fatalf("Discover: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled

	start := time.Now()
	redialed, err := tun.Restart(ctx)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatalf("Restart with a cancelled ctx should error, got redialed=%v nil", redialed)
	}
	if elapsed > time.Second {
		t.Fatalf("Restart should return promptly on a cancelled ctx, took %v", elapsed)
	}
}

// --------------------------------------------------------------------------------
// Bounded dial / fail-fast (criterion #2 usability, round-1 fix)
// --------------------------------------------------------------------------------

// TestNew_HandshakeHonoursContext: against a server that accepts the TCP
// connection but never speaks SSH, a short ctx must abort the connect well
// before the OS/DialTimeout ceiling — proving the connect is bounded+cancelable.
func TestNew_HandshakeHonoursContext(t *testing.T) {
	disableRealSSHEnv(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	silent := make(chan struct{})
	t.Cleanup(func() { _ = ln.Close(); close(silent) })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			// Accept then stay mute: TCP up, SSH handshake hangs.
			go func(c net.Conn) {
				<-silent
				_ = c.Close()
			}(c)
		}
	}()

	keyFile := writeRSAKeyFile(t)
	host, port, _ := net.SplitHostPort(ln.Addr().String())
	p, _ := strconv.Atoi(port)
	cfg := Config{Host: host, Port: p, User: "x", KeyPath: keyFile, Insecure: true}

	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err = New(ctx, cfg)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("New against a silent server should fail, got nil")
	}
	if elapsed > 2*time.Second {
		t.Fatalf("bounded dial should fail fast (~ctx), took %v", elapsed)
	}
}

// TestNew_UnreachableHostFails: a refused port fails fast (not the OS timeout).
func TestNew_UnreachableHostFails(t *testing.T) {
	disableRealSSHEnv(t)
	// Bind then immediately close a loopback port so dialing it is refused at once.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	host, port, _ := net.SplitHostPort(addr)
	p, _ := strconv.Atoi(port)
	cfg := Config{Host: host, Port: p, User: "x", KeyPath: writeRSAKeyFile(t), Insecure: true}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	start := time.Now()
	_, err = New(ctx, cfg)
	if err == nil {
		t.Fatal("New to a refused port should fail")
	}
	if time.Since(start) > 3*time.Second {
		t.Fatalf("refused port should fail immediately, took %v", time.Since(start))
	}
}

// TestNew_AuthFailure: a key the server does not accept is a handshake failure,
// surfaced as a Go error (criterion #2: auth failure is actionable).
func TestNew_AuthFailure(t *testing.T) {
	env := newSSHTestEnv(t)
	// A key the test server does NOT accept.
	wrongKey := writeRSAKeyFile(t)
	cfg := env.insecureCfg()
	cfg.KeyPath = wrongKey
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := New(ctx, cfg)
	if err == nil {
		t.Fatal("New with an unaccepted key should fail with an auth/handshake error")
	}
}

// --------------------------------------------------------------------------------
// Host-key verification (criterion #2, design §2.1)
// --------------------------------------------------------------------------------

func TestHostKey_StrictRejectsUnknownHost(t *testing.T) {
	env := newSSHTestEnv(t)
	// Strict mode with an EMPTY known_hosts -> the host is unknown -> hard fail.
	kh := filepath.Join(t.TempDir(), "known_hosts")
	if err := os.WriteFile(kh, nil, 0o600); err != nil {
		t.Fatalf("write known_hosts: %v", err)
	}
	cfg := env.insecureCfg()
	cfg.Insecure = false
	cfg.KnownHosts = kh
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := New(ctx, cfg)
	if err == nil {
		t.Fatal("strict host-key check on an unknown host must fail")
	}
	if !strings.Contains(err.Error(), "unknown host") {
		t.Fatalf("unknown-host error should mention the ssh-keyscan remedy, got: %v", err)
	}
}

func TestHostKey_StrictAcceptsKnownHost(t *testing.T) {
	env := newSSHTestEnv(t)
	kh := filepath.Join(t.TempDir(), "known_hosts")
	if err := os.WriteFile(kh, []byte(env.hostLine+"\n"), 0o600); err != nil {
		t.Fatalf("write known_hosts: %v", err)
	}
	cfg := env.insecureCfg()
	cfg.Insecure = false
	cfg.KnownHosts = kh
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	tun, err := New(ctx, cfg)
	if err != nil {
		t.Fatalf("strict host-key check on a known host should succeed: %v", err)
	}
	defer tun.Close()
}

func TestHostKey_InsecureBypass(t *testing.T) {
	env := newSSHTestEnv(t)
	// Insecure bypass accepts any host key (no known_hosts needed).
	cfg := env.insecureCfg() // Insecure: true
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	tun, err := New(ctx, cfg)
	if err != nil {
		t.Fatalf("insecure bypass should connect: %v", err)
	}
	defer tun.Close()
}

// --------------------------------------------------------------------------------
// Close / concurrency
// --------------------------------------------------------------------------------

func TestClose_IdempotentAndReleases(t *testing.T) {
	env := newSSHTestEnv(t)
	tun := env.mustNew(t)
	if err := tun.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := tun.Close(); err != nil {
		t.Fatalf("second Close should be a benign no-op, got: %v", err)
	}
	// DialContext after Close must report not-connected, not panic.
	if _, err := tun.DialContext(context.Background(), "", ""); err == nil {
		t.Fatal("DialContext after Close should fail (not connected)")
	}
}

// TestDialContext_ConcurrentSafety hammers DialContext from many goroutines to
// exercise the mu-guarded client/target snapshot under concurrency.
func TestDialContext_ConcurrentSafety(t *testing.T) {
	env := newSSHTestEnv(t)
	tun := env.mustNew(t)
	defer tun.Close()
	if _, err := tun.Discover(); err != nil {
		t.Fatalf("Discover: %v", err)
	}

	const n = 16
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		go func() {
			errs <- func() error { return healthOver(tun) }()
		}()
	}
	for i := 0; i < n; i++ {
		if err := <-errs; err != nil {
			t.Errorf("concurrent health %d: %v", i, err)
		}
	}
}

// --------------------------------------------------------------------------------
// helpers
// --------------------------------------------------------------------------------

func (e *sshTestEnv) mustNewCfg(t *testing.T, cfg Config) *Tunnel {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	tun, err := New(ctx, cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return tun
}

func writeRSAKeyFile(t *testing.T) string {
	t.Helper()
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("rsa key: %v", err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(priv)})
	p := filepath.Join(t.TempDir(), "id_rsa")
	if err := os.WriteFile(p, pemBytes, 0o600); err != nil {
		t.Fatalf("write key: %v", err)
	}
	return p
}
