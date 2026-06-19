package model

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// TestCompleteWithToolsCtxCancelDuringRequest verifies that cancelling the
// context aborts an in-flight completion instead of blocking until the server
// responds (or the client timeout fires) — the core of issue #24.
func TestCompleteWithToolsCtxCancelDuringRequest(t *testing.T) {
	// The server blocks until the test signals release, simulating a hung model.
	release := make(chan struct{})
	var hits int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		select {
		case <-release:
		case <-r.Context().Done():
		}
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(CompletionResponse{Content: "late", Role: RoleAssistant})
	}))
	defer server.Close()
	defer close(release)

	c := newTestConn(server.URL)

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() {
		_, err := c.CompleteWithToolsCtx(ctx, []Message{{Role: RoleUser, Content: "hi"}}, nil)
		done <- err
	}()

	// Give the request a moment to reach the (blocking) server, then cancel.
	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected an error after cancellation, got nil")
		}
		modelErr, ok := err.(*ModelError)
		if !ok {
			t.Fatalf("expected *ModelError, got %T: %v", err, err)
		}
		if modelErr.Type != ErrorConnection {
			t.Errorf("expected ErrorConnection, got %q", modelErr.Type)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("CompleteWithToolsCtx did not return promptly after cancellation")
	}
}

// TestCompleteCancelledDoesNotRetry verifies a cancelled context is terminal:
// the connection must not burn its retry budget re-issuing a request the caller
// has already abandoned.
func TestCompleteCancelledDoesNotRetry(t *testing.T) {
	var hits int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		// Consume the request body, as any real server does. Go's HTTP server
		// only begins watching the connection for a client disconnect (and thus
		// only cancels r.Context()) once the handler has read the request body to
		// EOF; a handler that blocks without reading the body never observes the
		// client's cancellation (see net/http/server.go: requestBodyRemains /
		// backgroundRead). Draining here lets the cancelled client abort us.
		_, _ = io.Copy(io.Discard, r.Body)
		<-r.Context().Done() // never responds; cancellation is the only exit
	}))
	defer server.Close()

	c := newTestConn(server.URL)
	c.maxAttempts = 3 // would retry twice more on a transient failure

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := c.CompleteWithToolsCtx(ctx, []Message{{Role: RoleUser, Content: "hi"}}, nil)
		done <- err
	}()

	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("request did not abort promptly after cancellation")
	}

	// Exactly one attempt should have reached the server; a cancelled context
	// must not be retried.
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Errorf("expected exactly 1 server hit, got %d (cancelled request was retried)", got)
	}
}

// TestSleepCtx covers the cancellable backoff helper: it returns true when the
// full delay elapses and false the moment the context is cancelled.
func TestSleepCtx(t *testing.T) {
	if !sleepCtx(context.Background(), 0) {
		t.Error("sleepCtx with zero delay and live context should return true")
	}

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if sleepCtx(cancelled, 0) {
		t.Error("sleepCtx with zero delay and cancelled context should return false")
	}

	start := time.Now()
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()
	if sleepCtx(ctx, time.Hour) {
		t.Error("sleepCtx should return false when cancelled before the delay elapses")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("sleepCtx blocked for %v; should have aborted on cancellation", elapsed)
	}
}
