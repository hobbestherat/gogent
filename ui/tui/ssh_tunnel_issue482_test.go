package ui

// Integration tests for the ssh:// transport wiring (issue #482): the NewAPIClient
// scheme switch (case "ssh", WithDialContext, default-error string), the token
// pass-through over the tunnel, CloseIdleConnections delegation, and — most
// importantly — RemoteClient.reconnect calling the injected tunnel's Restart
// before re-opening the SSE stream, including the Restart-error-continues-backoff
// guard and the redialed=true/false branches. These are package-internal so they
// can build APIClient directly, inject a fake TunnelRestarter, and exercise the
// unexported reconnect() path.

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gogent/internal/agent"
)

// --------------------------------------------------------------------------------
// NewAPIClient case "ssh"
// --------------------------------------------------------------------------------

// TestNewAPIClient_SSHRequiresInjectedDialer: ssh:// with no injected tunnel is a
// hard error (it never happens in the real path, but must not silently build a
// client with no transport).
func TestNewAPIClient_SSHRequiresInjectedDialer(t *testing.T) {
	_, err := NewAPIClient("ssh://user@host", "")
	if err == nil {
		t.Fatal("ssh:// without an injected tunnel must error")
	}
	if !strings.Contains(err.Error(), "ssh") || !strings.Contains(err.Error(), "tunnel") {
		t.Fatalf("error should mention ssh/tunnel, got: %v", err)
	}
}

// TestNewAPIClient_DefaultErrorMentionsSSH: the unknown-scheme error now lists
// ssh:// (regression guard for the help/error surface).
func TestNewAPIClient_DefaultErrorMentionsSSH(t *testing.T) {
	_, err := NewAPIClient("ftp://host", "")
	if err == nil {
		t.Fatal("ftp:// must be rejected")
	}
	if !strings.Contains(err.Error(), "ssh://") {
		t.Fatalf("unsupported-scheme error should advertise ssh://, got: %v", err)
	}
}

// TestNewAPIClient_VariadicOptsDoNotBreakOtherSchemes: the signature change to
// variadic must leave unix/http/https call sites working even when opts are
// passed (they are ignored for those schemes).
func TestNewAPIClient_VariadicOptsDoNotBreakOtherSchemes(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	// Pass a stray WithDialContext that MUST be ignored for an http:// client.
	noop := WithDialContext("http://bogus", func(context.Context, string, string) (net.Conn, error) {
		return nil, errors.New("should not be used for http://")
	})
	c, err := NewAPIClient(srv.URL, "", noop)
	if err != nil {
		t.Fatalf("NewAPIClient(http) with stray opts: %v", err)
	}
	if err := c.Health(); err != nil {
		t.Fatalf("Health over http:// should ignore the ssh-only opt: %v", err)
	}
}

// TestNewAPIClient_SSHUsesInjectedDialerAndCarriesToken: the ssh:// case wires the
// injected DialContext into the transport and base "http://ssh", drives a real
// HTTP round-trip through it, and carries the bearer token over the tunnel.
func TestNewAPIClient_SSHUsesInjectedDialerAndCarriesToken(t *testing.T) {
	var gotAuth, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	host, port, _ := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	// The injected dialer is the seam the SSH tunnel provides: it ignores the
	// placeholder addr and reaches the real daemon transport.
	dial := func(ctx context.Context, _, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "tcp", net.JoinHostPort(host, port))
	}
	c, err := NewAPIClient("ssh://user@machineB", "tok-over-tunnel", WithDialContext("http://ssh", dial))
	if err != nil {
		t.Fatalf("NewAPIClient ssh: %v", err)
	}
	if c.base != "http://ssh" {
		t.Fatalf("base = %q, want http://ssh (placeholder, like unix://)", c.base)
	}
	if err := c.Health(); err != nil {
		t.Fatalf("Health over the injected ssh dialer: %v", err)
	}
	if gotPath != "/api/health" {
		t.Errorf("request path = %q, want /api/health", gotPath)
	}
	if gotAuth != "Bearer tok-over-tunnel" {
		t.Errorf("Authorization over tunnel = %q, want the bearer token carried through", gotAuth)
	}
}

// --------------------------------------------------------------------------------
// CloseIdleConnections delegation (the post-redial pool flush)
// --------------------------------------------------------------------------------

type closeCountTransport struct {
	closes int
}

func (c *closeCountTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("not used")
}

func (c *closeCountTransport) CloseIdleConnections() { c.closes++ }

// TestAPIClient_CloseIdleConnectionsDelegates: http.Client.CloseIdleConnections
// dispatches to any Transport implementing CloseIdleConnections, so reconnect's
// post-redial flush actually reaches the transport pool.
func TestAPIClient_CloseIdleConnectionsDelegates(t *testing.T) {
	tr := &closeCountTransport{}
	c := &APIClient{http: &http.Client{Transport: tr}, base: "http://ssh"}
	c.CloseIdleConnections()
	c.CloseIdleConnections()
	if tr.closes != 2 {
		t.Fatalf("CloseIdleConnections delegated %d times, want 2", tr.closes)
	}
}

// --------------------------------------------------------------------------------
// reconnect -> tunnel.Restart (criterion #4 reconnect; round-2 conditional flush)
// --------------------------------------------------------------------------------

// scriptedTunnel is a fake TunnelRestarter that returns a scripted sequence of
// (redialed, err) results, recording how many times Restart was called. It lets a
// test drive reconnect's tunnel branch without a real SSH session.
type scriptedTunnel struct {
	mu      sync.Mutex
	calls   int
	results []tunnelResult
}

type tunnelResult struct {
	redialed bool
	err      error
}

func (s *scriptedTunnel) Restart(context.Context) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	if len(s.results) == 0 {
		return false, nil // default: a healthy, probe-skip session
	}
	r := s.results[0]
	s.results = s.results[1:]
	return r.redialed, r.err
}

func (s *scriptedTunnel) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

// reconnectSSEServer is a minimal /api/events SSE source whose stream channels a
// test can close to force a reconnect, mirroring the daemon_lifecycle harness.
type reconnectSSEServer struct {
	srv     *httptest.Server
	mu      sync.Mutex
	streams []chan GlobalEventDTO
	opens   int
}

func newReconnectSSEServer(t *testing.T) *reconnectSSEServer {
	t.Helper()
	s := &reconnectSSEServer{}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/events" {
			http.NotFound(w, r)
			return
		}
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "no flush", 500)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher.Flush()
		ch := make(chan GlobalEventDTO, 4)
		s.mu.Lock()
		s.streams = append(s.streams, ch)
		s.opens++
		s.mu.Unlock()
		for {
			select {
			case ge, open := <-ch:
				if !open {
					return
				}
				fmt.Fprintf(w, "data: {\"session_id\":\"%s\"}\n\n", ge.SessionID)
				flusher.Flush()
			case <-r.Context().Done():
				return
			}
		}
	}))
	t.Cleanup(s.srv.Close)
	return s
}

func (s *reconnectSSEServer) closeFirst(t *testing.T) {
	t.Helper()
	s.mu.Lock()
	if len(s.streams) == 0 {
		s.mu.Unlock()
		t.Fatal("no stream to close")
	}
	first := s.streams[0]
	s.streams = s.streams[1:]
	s.mu.Unlock()
	close(first)
}

func (s *reconnectSSEServer) openCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.opens
}

// testReconnector records disconnect/reconnect signals.
type testReconnector struct {
	lost     atomic.Int32
	restored atomic.Int32
}

func (r *testReconnector) OnConnectionLost(int)  { r.lost.Add(1) }
func (r *testReconnector) OnConnectionRestored() { r.restored.Add(1) }
func (r *testReconnector) restoredCount() int32  { return r.restored.Load() }

func waitFor(t *testing.T, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for: %s", msg)
}

// newReconnectClient builds a RemoteClient over the SSE server with a fast backoff,
// ready to observe a reconnect cycle.
func newReconnectClient(t *testing.T) (*RemoteClient, *reconnectSSEServer, *testReconnector) {
	t.Helper()
	srv := newReconnectSSEServer(t)
	client, err := NewAPIClient(srv.srv.URL, "")
	if err != nil {
		t.Fatalf("NewAPIClient: %v", err)
	}
	rc := NewRemoteClient(client, func(string, agent.SessionEvent) {}, nil)
	rec := &testReconnector{}
	rc.SetReconnector(rec)
	rc.backoff = func(int) time.Duration { return time.Millisecond }
	return rc, srv, rec
}

// TestReconnect_InvokesTunnelRestart: a dropped stream must call the tunnel's
// Restart before re-opening the SSE stream, then restore.
func TestReconnect_InvokesTunnelRestart(t *testing.T) {
	rc, srv, rec := newReconnectClient(t)
	tun := &scriptedTunnel{results: []tunnelResult{{redialed: true}}} // a redial happened
	rc.SetTunnel(tun)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := rc.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer rc.Close()
	waitFor(t, func() bool { return srv.openCount() == 1 }, "initial stream")

	srv.closeFirst(t) // force a drop -> reconnect

	waitFor(t, func() bool { return tun.callCount() >= 1 }, "tunnel.Restart on reconnect")
	waitFor(t, func() bool { return srv.openCount() == 2 }, "replacement stream after redial")
	waitFor(t, func() bool { return rec.restoredCount() == 1 }, "restored signal")
	if got := tun.callCount(); got != 1 {
		t.Errorf("Restart call count = %d, want 1 (probe-skip should not re-call)", got)
	}
}

// TestReconnect_ProbeSkipDoesNotFlushBreaksNothing: Restart(redialed=false) (a
// healthy, probe-skipped session) must still let reconnect proceed normally.
func TestReconnect_ProbeSkipProceeds(t *testing.T) {
	rc, srv, rec := newReconnectClient(t)
	tun := &scriptedTunnel{} // default: redialed=false, nil
	rc.SetTunnel(tun)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := rc.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer rc.Close()
	waitFor(t, func() bool { return srv.openCount() == 1 }, "initial stream")

	srv.closeFirst(t)
	waitFor(t, func() bool { return tun.callCount() >= 1 }, "tunnel.Restart")
	waitFor(t, func() bool { return rec.restoredCount() == 1 }, "restored despite no redial")
	waitFor(t, func() bool { return srv.openCount() == 2 }, "replacement stream")
}

// TestReconnect_TunnelRestartErrorContinuesBackoff: if Restart reports the tunnel
// still down, reconnect must NOT give up or deadlock — it continues the backoff
// loop and restores once a subsequent Restart/openStream succeeds.
func TestReconnect_TunnelRestartErrorContinuesBackoff(t *testing.T) {
	rc, srv, rec := newReconnectClient(t)
	tun := &scriptedTunnel{results: []tunnelResult{
		{false, errors.New("ssh still down")}, // first attempt: tunnel down
	}}
	rc.SetTunnel(tun)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := rc.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer rc.Close()
	waitFor(t, func() bool { return srv.openCount() == 1 }, "initial stream")

	start := time.Now()
	srv.closeFirst(t)
	// First Restart errors -> continue -> second Restart (default false,nil) ->
	// openStream succeeds -> restored. The whole cycle must complete promptly.
	waitFor(t, func() bool { return rec.restoredCount() == 1 }, "restored after tunnel recovered")
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("reconnect after a Restart error took too long (%v); backoff may have stalled", elapsed)
	}
	waitFor(t, func() bool { return srv.openCount() == 2 }, "replacement stream")
	if got := tun.callCount(); got < 2 {
		t.Errorf("Restart call count = %d, want >=2 (error must not abort the loop)", got)
	}
}

// TestReconnect_NoTunnelIsUnchanged: with no tunnel set (unix/http/https), the
// reconnect path behaves exactly as before — a drop restores without touching any
// tunnel. Regression guard for criterion #3.
func TestReconnect_NoTunnelIsUnchanged(t *testing.T) {
	rc, srv, rec := newReconnectClient(t)
	// rc.tunnel intentionally left nil.

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := rc.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer rc.Close()
	waitFor(t, func() bool { return srv.openCount() == 1 }, "initial stream")

	srv.closeFirst(t)
	waitFor(t, func() bool { return rec.restoredCount() == 1 }, "restored with no tunnel")
	waitFor(t, func() bool { return srv.openCount() == 2 }, "replacement stream")
	if rc.tunnel != nil {
		t.Fatal("tunnel field must stay nil for non-ssh transports")
	}
}
