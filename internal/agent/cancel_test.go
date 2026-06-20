package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"gogent/internal/model"
	"gogent/internal/tool"
)

// blockingLoopSession wires a session whose model points at a server that hangs
// until released, so a task loop can be observed mid-flight and cancelled.
func blockingLoopSession(t *testing.T, release <-chan struct{}) *UserSession {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(finalResponse("done"))
	}))
	t.Cleanup(server.Close)

	conn := model.NewModelConnection()
	conn.SetURL(server.URL)
	sess := model.NewModelSession("s", conn)

	ag := NewAgent("root", sess)
	ag.SetToolRegistry(tool.NewToolRegistry())
	return NewUserSession("s1", ag)
}

// TestStopAgentCancelsInFlightLoop verifies StopAgent actually interrupts a
// running task loop (cancelling the hung model request) rather than merely
// flipping a state field — the regression issue #24 targets.
func TestStopAgentCancelsInFlightLoop(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	us := blockingLoopSession(t, release)

	done := make(chan error, 1)
	go func() {
		_, err := us.ExecuteTaskLoop(context.Background(), "root", "hi")
		done <- err
	}()

	// Let the first request reach the (blocking) server, then stop the agent.
	time.Sleep(50 * time.Millisecond)
	if err := us.StopAgent("root"); err != nil {
		t.Fatalf("StopAgent: %v", err)
	}

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected the loop to return an error after StopAgent")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("loop did not return promptly after StopAgent")
	}
}

// TestSessionStopCancelsInFlightLoop verifies a session-wide Stop (used on
// RemoveSession/close) cancels in-flight loops so they stop touching a detached
// session.
func TestSessionStopCancelsInFlightLoop(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	us := blockingLoopSession(t, release)

	done := make(chan error, 1)
	go func() {
		_, err := us.ExecuteTaskLoop(context.Background(), "root", "hi")
		done <- err
	}()

	time.Sleep(50 * time.Millisecond)
	us.Stop()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected the loop to return an error after Stop")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("loop did not return promptly after Stop")
	}
}

// TestCallerContextCancelsLoop verifies cancellation propagates from the context
// the caller passes in (e.g. an HTTP request context) down to the model request.
func TestCallerContextCancelsLoop(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	us := blockingLoopSession(t, release)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := us.ExecuteTaskLoop(ctx, "root", "hi")
		done <- err
	}()

	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected the loop to return an error after the caller cancelled")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("loop did not return promptly after caller cancellation")
	}
}
