package server

import (
	"testing"
	"time"

	"gogent/internal/agent"
)

// TestHubFansOutToSessionSubscribers confirms a session observer's events reach
// that session's subscribers and the global subscribers, tagged with the id.
func TestHubFansOutToSessionSubscribers(t *testing.T) {
	h := newHub()
	obs := h.sessionObserver("s1")

	sub, unsub := h.subscribeSession("s1")
	defer unsub()
	gsub, gunsub := h.subscribeGlobal()
	defer gunsub()

	obs(agent.SessionEvent{Type: agent.SessionEventThinking, Step: 0})

	select {
	case te := <-sub:
		if te.sessionID != "s1" {
			t.Fatalf("session subscriber sessionID = %q, want s1", te.sessionID)
		}
		if te.ev.Type != agent.SessionEventThinking {
			t.Fatalf("event type = %v, want thinking", te.ev.Type)
		}
	case <-time.After(time.Second):
		t.Fatal("session subscriber did not receive the event")
	}

	select {
	case te := <-gsub:
		if te.sessionID != "s1" {
			t.Fatalf("global subscriber sessionID = %q, want s1", te.sessionID)
		}
	case <-time.After(time.Second):
		t.Fatal("global subscriber did not receive the event")
	}
}

// TestHubUnsubscribeStopsDelivery confirms an unsubscribed channel gets nothing.
func TestHubUnsubscribeStopsDelivery(t *testing.T) {
	h := newHub()
	obs := h.sessionObserver("s1")

	sub, unsub := h.subscribeSession("s1")
	unsub()

	obs(agent.SessionEvent{Type: agent.SessionEventFinal, Text: "done"})

	// The event is non-blocking; after unsub it should not arrive.
	select {
	case <-sub:
		t.Fatal("unsubscribed channel should not receive events")
	case <-time.After(20 * time.Millisecond):
	}
}

// TestHubOtherSessionDoesNotReceive confirms events for one session don't leak
// to another session's subscribers.
func TestHubOtherSessionDoesNotReceive(t *testing.T) {
	h := newHub()
	obsS1 := h.sessionObserver("s1")
	sub2, unsub2 := h.subscribeSession("s2")
	defer unsub2()

	obsS1(agent.SessionEvent{Type: agent.SessionEventThinking})

	select {
	case <-sub2:
		t.Fatal("s2 subscriber received an s1 event")
	case <-time.After(20 * time.Millisecond):
	}
}

// TestHubTerminalEventDeliveredOnFullBuffer confirms a terminal event (final)
// is delivered even when the buffer is full, because the terminal send path
// briefly blocks until a consumer drains room. The consumer drains concurrently.
func TestHubTerminalEventDeliveredOnFullBuffer(t *testing.T) {
	h := newHub()
	obs := h.sessionObserver("s1")
	sub, unsub := h.subscribeSession("s1")
	defer unsub()

	// Fill the buffered channel with non-terminal events (capacity 64).
	for i := 0; i < 64; i++ {
		obs(agent.SessionEvent{Type: agent.SessionEventThinking, Step: i})
	}

	// Emit the terminal event off the test goroutine; it briefly blocks until a
	// consumer drains room.
	done := make(chan struct{})
	go func() {
		obs(agent.SessionEvent{Type: agent.SessionEventFinal, Text: "done"})
		close(done)
	}()

	// Drain until we see the final event.
	sawFinal := false
	deadline := time.After(2 * time.Second)
	for !sawFinal {
		select {
		case te := <-sub:
			if te.ev.Type == agent.SessionEventFinal {
				sawFinal = true
			}
		case <-deadline:
			t.Fatal("terminal event was dropped despite the blocking-delivery path")
		}
	}

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("terminal send never completed")
	}
}

// TestIsTerminal confirms the terminal classification used by the drop policy.
func TestIsTerminal(t *testing.T) {
	cases := []struct {
		t    agent.SessionEventType
		want bool
	}{
		{agent.SessionEventFinal, true},
		{agent.SessionEventError, true},
		{agent.SessionEventPlan, true},
		{agent.SessionEventThinking, false},
		{agent.SessionEventToolCall, false},
		{agent.SessionEventUsage, false},
	}
	for _, c := range cases {
		if got := isTerminal(agent.SessionEvent{Type: c.t}); got != c.want {
			t.Errorf("isTerminal(%v) = %v, want %v", c.t, got, c.want)
		}
	}
}
