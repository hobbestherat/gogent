package model

import (
	"net"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"gogent/internal/config"
)

// TestSharedHTTPTransportTuned asserts the process-wide transport that backs
// every model client carries the pool tuning from issue #19, and that it did not
// discard the sensible defaults inherited from http.DefaultTransport (notably
// proxy-from-environment, so a user behind a corporate proxy / HTTPS_PROXY still
// works).
func TestSharedHTTPTransportTuned(t *testing.T) {
	if got := sharedHTTPTransport.MaxIdleConns; got != 100 {
		t.Errorf("MaxIdleConns = %d, want 100", got)
	}
	if got := sharedHTTPTransport.MaxIdleConnsPerHost; got != 32 {
		t.Errorf("MaxIdleConnsPerHost = %d, want 32 (default of 2 throttles fan-out)", got)
	}
	if got := sharedHTTPTransport.IdleConnTimeout; got != 90*time.Second {
		t.Errorf("IdleConnTimeout = %v, want 90s", got)
	}
	if !sharedHTTPTransport.ForceAttemptHTTP2 {
		t.Error("ForceAttemptHTTP2 = false, want true")
	}
	if sharedHTTPTransport.Proxy == nil {
		t.Error("Proxy = nil; cloning http.DefaultTransport should keep proxy-from-environment")
	}
}

// baseTransport unwraps the auth round-tripper (when present) to expose the
// pooled *http.Transport a connection's client actually rides on.
func baseTransport(c *ModelConnection) http.RoundTripper {
	if rt, ok := c.client.Transport.(*APIKeyRoundTripper); ok {
		return rt.transport
	}
	return c.client.Transport
}

// TestNewModelConnectionUsesSharedTransport verifies the default connection
// shares the process-wide pooled transport rather than carrying a nil Transport
// (which would fall back to http.DefaultTransport's MaxIdleConnsPerHost of 2).
func TestNewModelConnectionUsesSharedTransport(t *testing.T) {
	c := newPlaceholderConnection()
	if baseTransport(c) != sharedHTTPTransport {
		t.Errorf("NewModelConnection transport = %p, want shared transport %p", baseTransport(c), sharedHTTPTransport)
	}
}

// TestNewModelConnectionFromConfigUsesSharedTransport verifies that, with or
// without an API key, the connection always rides the same pooled transport:
// the key only adds a cheap auth round-tripper wrapping it, never a fresh pool.
func TestNewModelConnectionFromConfigUsesSharedTransport(t *testing.T) {
	t.Run("without_key", func(t *testing.T) {
		c := NewModelConnection(&config.ProviderConnection{APIType: "openai"}, &config.ModelConfig{Model: "gpt-x"})
		if _, wrapped := c.client.Transport.(*APIKeyRoundTripper); wrapped {
			t.Fatal("no API key should not wrap the transport in an auth round-tripper")
		}
		if baseTransport(c) != sharedHTTPTransport {
			t.Errorf("transport = %p, want shared transport %p", baseTransport(c), sharedHTTPTransport)
		}
	})

	t.Run("with_key", func(t *testing.T) {
		c := NewModelConnection(&config.ProviderConnection{APIType: "openai", APIKey: "k"}, &config.ModelConfig{Model: "gpt-x"})
		rt, ok := c.client.Transport.(*APIKeyRoundTripper)
		if !ok {
			t.Fatalf("transport = %T, want *APIKeyRoundTripper", c.client.Transport)
		}
		if rt.transport != sharedHTTPTransport {
			t.Errorf("auth round-tripper transport = %p, want shared transport %p", rt.transport, sharedHTTPTransport)
		}
	})
}

// countingListener tallies every accepted TCP connection so a test can tell
// keep-alive reuse (no new Accept) from a fresh dial (new Accept).
type countingListener struct {
	net.Listener
	count *atomic.Int64
}

func (l *countingListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err == nil {
		l.count.Add(1)
	}
	return c, err
}

// TestModelConnectionReusesKeepAliveAcrossClients is the functional proof of the
// issue #19 fix: production rebuilds a fresh *http.Client on every user turn
// (internal/gogent buildConnection -> NewModelConnectionFromConfig). Before the
// fix each client carried its own (default) transport, so keep-alive conns could
// never be reused across turns and every turn redialed. Now every per-turn
// client shares one pooled transport, so N sequential requests — each from a
// separately built connection — reuse a single TCP connection instead of opening
// N. A connection count greater than one would mean the pool is not shared.
func TestModelConnectionReusesKeepAliveAcrossClients(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	})

	rawLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	var connCount atomic.Int64
	srv := &http.Server{Handler: handler}
	go func() { _ = srv.Serve(&countingListener{Listener: rawLn, count: &connCount}) }()
	defer srv.Close()
	url := "http://" + rawLn.Addr().String() + "/v1/chat/completions"

	const turns = 5
	for i := 0; i < turns; i++ {
		// A fresh client per turn — exactly the per-message rebuild in
		// production — all sharing one pooled transport.
		c := newPlaceholderConnection()
		c.SetURL(url)
		c.SetTimeout(10 * time.Second)
		resp, err := c.Complete([]Message{{Role: RoleUser, Content: "hi"}})
		if err != nil {
			t.Fatalf("turn %d: Complete: %v", i, err)
		}
		if resp.Content != "ok" {
			t.Fatalf("turn %d: content = %q, want %q", i, resp.Content, "ok")
		}
	}

	if got := connCount.Load(); got != 1 {
		t.Errorf("expected 1 TCP connection reused across %d per-turn clients, got %d (keep-alive pool not shared)", turns, got)
	}
}
