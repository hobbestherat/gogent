package ui

import (
	"context"
	"sync"
	"testing"
	"time"

	"gogent/internal/permission"
)

// newPromptWorkbench builds a minimal Workbench exercising only the prompt
// queue/shutdown machinery (no desktop/event loop needed).
func newPromptWorkbench() *Workbench {
	w := &Workbench{}
	w.shutdown, w.quit = context.WithCancel(context.Background())
	return w
}

// TestMarkApprovalCounting verifies the per-session in-flight prompt counter:
// concurrent prompts for one session accumulate, each resolution decrements, and
// a session is dropped from the set (so its badge clears) once it reaches zero.
// A bare workbench has no desktop/sidebar, so markApproval performs only its
// bookkeeping — exactly what is under test. (issue #55)
func TestMarkApprovalCounting(t *testing.T) {
	w := newPromptWorkbench()

	w.markApproval("s1", +1)
	w.markApproval("s1", +1)
	w.markApproval("s2", +1)
	if got := w.approvals["s1"]; got != 2 {
		t.Fatalf("s1 count = %d, want 2", got)
	}
	if got := w.approvals["s2"]; got != 1 {
		t.Fatalf("s2 count = %d, want 1", got)
	}

	// One of s1's two prompts resolves: it is still pending (count 1).
	w.markApproval("s1", -1)
	if got := w.approvals["s1"]; got != 1 {
		t.Fatalf("s1 count after one resolve = %d, want 1", got)
	}

	// Resolve the rest: both sessions drop out of the set entirely.
	w.markApproval("s1", -1)
	w.markApproval("s2", -1)
	if len(w.approvals) != 0 {
		t.Fatalf("expected empty approval set, got %v", w.approvals)
	}
}

// TestMarkApprovalEmptySessionClamps confirms an unknown/headless requester ("")
// never drives the per-session count negative.
func TestMarkApprovalEmptySessionClamps(t *testing.T) {
	w := newPromptWorkbench()
	w.markApproval("", -1) // resolve with nothing pending
	if n := w.approvals[""]; n != 0 {
		t.Fatalf("empty-session count = %d, want 0", n)
	}
	w.markApproval("", +1)
	w.markApproval("", -1)
	if len(w.approvals) != 0 {
		t.Fatalf("expected empty approval set, got %v", w.approvals)
	}
}

// TestRequesterLine covers the dialog header that names the requesting session
// and, when present, the sub-agent.
func TestRequesterLine(t *testing.T) {
	for _, tc := range []struct {
		session, agent, want string
	}{
		{"", "", ""},
		{"", "agent-1", ""},
		{"Session 2", "", "Requested by Session 2"},
		{"Session 2", "root", "Requested by Session 2"}, // primary agent: not repeated
		{"Session 2", "agent-7", "Requested by Session 2 · agent agent-7"},
	} {
		if got := requesterLine(tc.session, tc.agent); got != tc.want {
			t.Fatalf("requesterLine(%q,%q) = %q, want %q", tc.session, tc.agent, got, tc.want)
		}
	}
}

// TestPromptResolves verifies a normal answer is returned verbatim.
func TestPromptResolves(t *testing.T) {
	for _, tc := range []permission.Decision{
		permission.DecisionAllow,
		permission.DecisionAlways,
		permission.DecisionDeny,
		permission.DecisionAlwaysDeny,
	} {
		tc := tc
		t.Run(string(tc), func(t *testing.T) {
			w := newPromptWorkbench()
			got := w.prompt(permission.Request{Action: permission.ActionShell}, func(_ permission.Request, resolve func(permission.Decision)) {
				resolve(tc)
			})
			if got != tc {
				t.Fatalf("prompt returned %q, want %q", got, tc)
			}
		})
	}
}

// TestPromptShutdownBeforePresent verifies that a request arriving after the UI
// has gone is denied without even presenting a modal (which would post to a
// dead event loop).
func TestPromptShutdownBeforePresent(t *testing.T) {
	w := newPromptWorkbench()
	w.quit() // UI already shut down

	presented := false
	got := w.prompt(permission.Request{Action: permission.ActionShell}, func(_ permission.Request, _ func(permission.Decision)) {
		presented = true
	})
	if got != permission.DecisionDeny {
		t.Fatalf("prompt returned %q, want %q", got, permission.DecisionDeny)
	}
	if presented {
		t.Fatal("prompt presented a modal after shutdown")
	}
}

// TestPromptShutdownWhileWaiting is the core leak fix: a prompt outstanding when
// the UI quits must unblock with DecisionDeny rather than wedge the goroutine.
func TestPromptShutdownWhileWaiting(t *testing.T) {
	w := newPromptWorkbench()

	presented := make(chan struct{})
	done := make(chan permission.Decision, 1)
	go func() {
		// present never resolves, simulating a modal whose closure can no longer
		// run because the event loop has stopped.
		done <- w.prompt(permission.Request{Action: permission.ActionShell}, func(_ permission.Request, _ func(permission.Decision)) {
			close(presented)
		})
	}()

	<-presented // ensure we are blocked in the wait, then shut down
	w.quit()

	select {
	case got := <-done:
		if got != permission.DecisionDeny {
			t.Fatalf("prompt returned %q, want %q", got, permission.DecisionDeny)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("prompt did not unblock on shutdown (goroutine leak)")
	}
}

// TestPromptSerialized verifies concurrent requests are presented one at a time:
// a second request must not be presented until the first is resolved.
func TestPromptSerialized(t *testing.T) {
	w := newPromptWorkbench()

	entered := make(chan int, 2) // call index of each presented request
	resolvers := make(chan func(permission.Decision), 2)
	present := func(_ permission.Request, resolve func(permission.Decision)) {
		resolvers <- resolve
	}

	var wg sync.WaitGroup
	start := func(i int) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w.prompt(permission.Request{Action: permission.ActionShell}, func(req permission.Request, resolve func(permission.Decision)) {
				entered <- i
				present(req, resolve)
			})
		}()
	}

	start(1)
	start(2)

	// Exactly one request is presented; the other is queued behind promptMu.
	first := <-entered
	select {
	case <-entered:
		t.Fatal("second prompt presented before the first was resolved")
	case <-time.After(100 * time.Millisecond):
	}

	// Resolve the first; the queued request must now be presented.
	(<-resolvers)(permission.DecisionAllow)
	select {
	case second := <-entered:
		if second == first {
			t.Fatalf("same prompt presented twice (index %d)", second)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("queued prompt was not presented after the first resolved")
	}
	(<-resolvers)(permission.DecisionAllow)

	wg.Wait()
}
