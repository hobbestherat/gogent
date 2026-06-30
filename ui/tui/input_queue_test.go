package ui

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"gogent/internal/agent"
)

// recordSends installs an OnSend handler that forwards every dispatched message
// over a buffered channel, so a test can observe what the (goroutine-dispatched)
// submit path actually sent to the backend. The channel is buffered generously
// so a send never blocks the UI thread under test.
func recordSends(w *Workbench) <-chan string {
	sent := make(chan string, 8)
	w.handlers.OnSend = func(_, message, _, _, _ string) { sent <- message }
	return sent
}

// waitSend reads one dispatched message or fails the test if none arrives. The
// submit path dispatches OnSend on a goroutine, so a small timeout decouples the
// assertion from scheduling.
func waitSend(t *testing.T, sent <-chan string) string {
	t.Helper()
	select {
	case m := <-sent:
		return m
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for a dispatched message")
		return ""
	}
}

// noSend asserts that nothing is dispatched within a short window.
func noSend(t *testing.T, sent <-chan string) {
	t.Helper()
	select {
	case m := <-sent:
		t.Fatalf("unexpected dispatched message %q", m)
	case <-time.After(150 * time.Millisecond):
	}
}

// noteContains reports whether any transcript record's text mentions sub.
func noteContains(sw *SessionWindow, sub string) bool {
	for _, r := range sw.transcript.records {
		if strings.Contains(r.header, sub) {
			return true
		}
		for _, l := range r.lines {
			if strings.Contains(l.text, sub) {
				return true
			}
		}
	}
	return false
}

// TestQueueWhileBusyDoesNotDrop verifies that typing while the agent is busy
// queues the text (issue #170, phase 1) instead of dropping it: the first turn
// dispatches immediately, a message typed mid-turn is held in the pending slot,
// exposed in the status line and as a note, and nothing is sent until idle.
func TestQueueWhileBusyDoesNotDrop(t *testing.T) {
	w := newTestWorkbench(t)
	sent := recordSends(w)
	sw := w.openWindow("s", "S")

	// First message: not busy, so it dispatches and marks the window busy.
	sw.input.SetText("first")
	sw.submitFn()
	if got := waitSend(t, sent); got != "first" {
		t.Fatalf("first send = %q, want %q", got, "first")
	}
	if !sw.busy {
		t.Fatal("window should be busy after the first submit")
	}

	// Second message arrives while busy: it must be queued, not dropped or sent.
	sw.input.SetText("second")
	sw.submitFn()
	noSend(t, sent)
	if sw.pending != "second" {
		t.Fatalf("pending = %q, want %q (queued, not dropped)", sw.pending, "second")
	}
	if !strings.Contains(sw.status.Text, "queued") {
		t.Errorf("status line %q should expose the queued message", sw.status.Text)
	}
	if !noteContains(sw, "second") {
		t.Error("queued message should be echoed as a transcript note")
	}
}

// TestQueueDrainsOnIdle verifies the busy→idle transition auto-submits the
// queued message as the next user turn (issue #170, phase 1).
func TestQueueDrainsOnIdle(t *testing.T) {
	w := newTestWorkbench(t)
	sent := recordSends(w)
	sw := w.openWindow("s", "S")

	sw.input.SetText("first")
	sw.submitFn()
	if got := waitSend(t, sent); got != "first" {
		t.Fatalf("first send = %q", got)
	}
	sw.input.SetText("queued one")
	sw.submitFn()
	if sw.pending == "" {
		t.Fatal("expected a queued message before draining")
	}

	// Returning to idle (the terminal event the loop emits) drains the queue.
	sw.apply(agent.SessionEvent{Type: agent.SessionEventFinal, Text: "done"})
	if got := waitSend(t, sent); got != "queued one" {
		t.Fatalf("drained send = %q, want %q", got, "queued one")
	}
	if sw.pending != "" {
		t.Errorf("pending should be empty after draining, got %q", sw.pending)
	}
	if !sw.busy {
		t.Error("draining the queue should mark the window busy for the new turn")
	}
}

// TestStopClearsQueue verifies that stopping the agent discards the queue with a
// visible note rather than auto-firing it on the resulting idle transition
// (issue #170, phase 1).
func TestStopClearsQueue(t *testing.T) {
	w := newTestWorkbench(t)
	sent := recordSends(w)
	stopped := make(chan string, 1)
	w.handlers.OnStop = func(id string) { stopped <- id }
	sw := w.openWindow("s", "S")

	sw.input.SetText("first")
	sw.submitFn()
	if got := waitSend(t, sent); got != "first" {
		t.Fatalf("first send = %q", got)
	}
	sw.input.SetText("queued")
	sw.submitFn()
	if sw.pending == "" {
		t.Fatal("expected a queued message before stopping")
	}

	// /stop discards the queue and cancels the turn.
	sw.handleSlashCommand("/stop")
	if sw.pending != "" {
		t.Errorf("stop should discard the queue, pending = %q", sw.pending)
	}
	if !noteContains(sw, "cleared") {
		t.Error("stop should note that the queued message was cleared")
	}
	select {
	case id := <-stopped:
		if id != "s" {
			t.Errorf("OnStop got id %q, want s", id)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("OnStop was not invoked")
	}

	// The backend's terminal (error/cancel) event then arrives; the now-empty
	// queue must not auto-fire.
	sw.apply(agent.SessionEvent{Type: agent.SessionEventError, Err: fmt.Errorf("cancelled")})
	noSend(t, sent)
}
