package agent

import (
	"context"
	"testing"
	"time"
)

// TestRateLimiterBurstThenThrottle drives the token bucket with a fake clock: a
// burst is allowed up front, further requests are denied until permits refill.
func TestRateLimiterBurstThenThrottle(t *testing.T) {
	rl := NewRateLimiter(60, 2) // 1 permit/sec, burst of 2
	base := time.Unix(0, 0)
	cur := base
	rl.now = func() time.Time { return cur }
	rl.last = base

	if ok, _ := rl.take(); !ok {
		t.Fatal("first burst permit should be granted")
	}
	if ok, _ := rl.take(); !ok {
		t.Fatal("second burst permit should be granted")
	}

	// Burst exhausted: the next permit is denied and reports the wait until one
	// refills (1 permit/sec => ~1s).
	ok, wait := rl.take()
	if ok {
		t.Fatal("third permit should be denied once the burst is spent")
	}
	if wait <= 0 || wait > time.Second {
		t.Fatalf("expected a sub-second wait, got %v", wait)
	}

	// Advance one second: exactly one permit refills.
	cur = base.Add(time.Second)
	if ok, _ := rl.take(); !ok {
		t.Fatal("a permit should be granted after a second of refill")
	}
	if ok, _ := rl.take(); ok {
		t.Fatal("only one permit should refill per second")
	}
}

// TestRateLimiterUnbounded verifies a nil or zero-rate limiter never blocks.
func TestRateLimiterUnbounded(t *testing.T) {
	var nilRL *RateLimiter
	if err := nilRL.Wait(context.Background()); err != nil {
		t.Fatalf("nil limiter should not block: %v", err)
	}
	if err := NewRateLimiter(0, 0).Wait(context.Background()); err != nil {
		t.Fatalf("zero-rate limiter should not block: %v", err)
	}
}

// TestRateLimiterWaitCancels verifies Wait honors context cancellation while
// backing off for a permit instead of blocking indefinitely.
func TestRateLimiterWaitCancels(t *testing.T) {
	rl := NewRateLimiter(60, 1) // 1 permit/sec, burst of 1

	// Spend the single burst permit.
	if err := rl.Wait(context.Background()); err != nil {
		t.Fatalf("first Wait should succeed immediately: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := rl.Wait(ctx); err == nil {
		t.Fatal("Wait should return the context error when cancelled mid-backoff")
	}
}
