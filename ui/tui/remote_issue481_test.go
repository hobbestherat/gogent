package ui

// Issue #481 client-side tests: the daemon now returns an accepted turn id from
// Send/ApprovePlan instead of the final answer (which streams over SSE), and a
// POST that fails because the connection dropped must no longer surface as a
// spurious "turn failed" error — the dispatched turn may still be running on the
// daemon.

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"gogent/internal/agent"
)

// TestSendMessageDecodesAcceptedTurnID verifies the client surfaces the dispatched
// turn id from the accepted response, and no longer expects the final answer in
// the response body.
func TestSendMessageDecodesAcceptedTurnID(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"turnId":"turn_abc123"}`))
	}))
	defer srv.Close()

	client, err := NewAPIClient(srv.URL, "")
	if err != nil {
		t.Fatalf("NewAPIClient: %v", err)
	}
	msg, err := client.SendMessage(context.Background(), "s1", "hello", "m1", "medium", "")
	if err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	if msg.TurnID != "turn_abc123" {
		t.Fatalf("TurnID = %q, want turn_abc123", msg.TurnID)
	}
	if msg.Content != "" {
		t.Fatalf("Content = %q, want empty (the final answer now arrives over SSE)", msg.Content)
	}
}

// TestSendMessagePropagatesDispatchError verifies a genuine dispatch failure (HTTP
// 5xx while connected) still surfaces as an error from SendMessage — only a
// connection-level failure WHILE DISCONNECTED is suppressed.
func TestSendMessagePropagatesDispatchError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "internal error", http.StatusInternalServerError)
	}))
	defer srv.Close()

	client, err := NewAPIClient(srv.URL, "")
	if err != nil {
		t.Fatalf("NewAPIClient: %v", err)
	}
	if _, err := client.SendMessage(context.Background(), "s1", "hello", "m1", "", ""); err == nil {
		t.Fatal("SendMessage: expected an error for a failed dispatch, got nil")
	}
}

// TestEmitSendErrSuppressedWhileDisconnected verifies the disconnect-aware error
// suppression (issue #481): a failed send is logged, not surfaced as an error
// event, while the connection is down; once restored it surfaces again.
func TestEmitSendErrSuppressedWhileDisconnected(t *testing.T) {
	client, err := NewAPIClient("http://127.0.0.1:0", "")
	if err != nil {
		t.Fatalf("NewAPIClient: %v", err)
	}

	var mu sync.Mutex
	var got []agent.SessionEvent
	rc := NewRemoteClient(client, func(sessionID string, ev agent.SessionEvent) {
		mu.Lock()
		defer mu.Unlock()
		got = append(got, ev)
	}, nil)

	sendErr := errors.New("connection refused")

	// While disconnected: a failed send must NOT raise an error event (the turn may
	// still be running on the daemon; the disconnect modal carries the UX).
	rc.notifyLost(1)
	rc.emitSendErr("s1", sendErr)
	mu.Lock()
	if len(got) != 0 {
		t.Fatalf("expected no error event while disconnected, got %d: %+v", len(got), got)
	}
	mu.Unlock()

	// Once restored: the same failure surfaces in the session window as before.
	rc.notifyRestored()
	rc.emitSendErr("s1", sendErr)
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 1 || got[0].Type != agent.SessionEventError {
		t.Fatalf("expected one error event after restore, got %+v", got)
	}
}

// TestNotifyLostRestoredTracksDisconnected verifies the disconnected flag is set
// on connection loss and cleared on restore (the state the send handlers consult).
func TestNotifyLostRestoredTracksDisconnected(t *testing.T) {
	client, err := NewAPIClient("http://127.0.0.1:0", "")
	if err != nil {
		t.Fatalf("NewAPIClient: %v", err)
	}
	rc := NewRemoteClient(client, nil, nil)

	if rc.disconnected.Load() {
		t.Fatal("disconnected should start false")
	}
	rc.notifyLost(1)
	if !rc.disconnected.Load() {
		t.Fatal("disconnected should be true after notifyLost")
	}
	rc.notifyRestored()
	if rc.disconnected.Load() {
		t.Fatal("disconnected should be false after notifyRestored")
	}
}
