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

	entered := make(chan int, 2)   // call index of each presented request
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
