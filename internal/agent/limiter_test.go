package agent

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestSubAgentLimiterAcquireRelease verifies the counting-semaphore mechanics:
// a bounded limiter grants exactly its size in slots, then refuses until one is
// released.
func TestSubAgentLimiterAcquireRelease(t *testing.T) {
	l := NewSubAgentLimiter(2)

	if !l.tryAcquire() || !l.tryAcquire() {
		t.Fatal("expected the first two acquisitions to succeed")
	}
	if l.InFlight() != 2 {
		t.Fatalf("expected 2 slots in flight, got %d", l.InFlight())
	}
	if l.tryAcquire() {
		t.Fatal("expected the third acquisition to fail at the limit")
	}

	l.release()
	if l.InFlight() != 1 {
		t.Fatalf("expected 1 slot in flight after release, got %d", l.InFlight())
	}
	if !l.tryAcquire() {
		t.Fatal("expected acquisition to succeed after a slot was released")
	}
}

// TestSubAgentLimiterUnbounded verifies that a nil limiter and a non-positive
// size impose no limit, preserving pre-issue-23 behavior for sessions created
// without a shared limiter.
func TestSubAgentLimiterUnbounded(t *testing.T) {
	var nilLimiter *SubAgentLimiter
	for i := 0; i < 100; i++ {
		if !nilLimiter.tryAcquire() {
			t.Fatal("nil limiter must always grant a slot")
		}
	}
	nilLimiter.release() // must not panic
	if nilLimiter.InFlight() != 0 {
		t.Fatalf("nil limiter should report 0 in flight, got %d", nilLimiter.InFlight())
	}

	zero := NewSubAgentLimiter(0)
	for i := 0; i < 100; i++ {
		if !zero.tryAcquire() {
			t.Fatal("zero-size limiter must be unbounded")
		}
	}
}

// TestRunSubAgentsBoundedCapsConcurrency runs more tasks than the limiter allows
// and asserts that the peak number of simultaneously-running tasks never exceeds
// the limit plus one (the inline task the caller runs itself when no slot is
// free), while every task still runs exactly once. This is the core invariant of
// issue #23: bounded goroutine fan-out with inline backpressure.
func TestRunSubAgentsBoundedCapsConcurrency(t *testing.T) {
	const limit = 2
	const n = 8

	s := NewUserSession("s", NewAgent("root", nil))
	s.SetSubAgentLimiter(NewSubAgentLimiter(limit))

	var current, peak, completed int64
	var peakMu sync.Mutex

	tasks := make([]func(), n)
	for i := 0; i < n; i++ {
		tasks[i] = func() {
			cur := atomic.AddInt64(&current, 1)
			peakMu.Lock()
			if cur > peak {
				peak = cur
			}
			peakMu.Unlock()
			time.Sleep(20 * time.Millisecond) // let concurrent tasks overlap
			atomic.AddInt64(&current, -1)
			atomic.AddInt64(&completed, 1)
		}
	}

	s.RunSubAgentsBounded(tasks)

	if completed != n {
		t.Fatalf("expected all %d tasks to complete, got %d", n, completed)
	}
	if peak > limit+1 {
		t.Fatalf("peak concurrency %d exceeded the bound of limit+1 (%d)", peak, limit+1)
	}
	if peak < 2 {
		t.Fatalf("expected real parallelism (peak >= 2), got %d", peak)
	}
	// All slots must have been returned.
	if inflight := s.subAgentLimiter.InFlight(); inflight != 0 {
		t.Fatalf("expected 0 slots in flight after the batch, got %d", inflight)
	}
}

// TestRunSubAgentsBoundedUnbounded verifies that without a limiter every task is
// still executed (the goroutine-per-task path), so existing callers that never
// install a limiter keep working.
func TestRunSubAgentsBoundedUnbounded(t *testing.T) {
	s := NewUserSession("s", NewAgent("root", nil))

	var completed int64
	tasks := make([]func(), 16)
	for i := 0; i < len(tasks); i++ {
		tasks[i] = func() { atomic.AddInt64(&completed, 1) }
	}

	s.RunSubAgentsBounded(tasks)
	if completed != 16 {
		t.Fatalf("expected 16 tasks to complete, got %d", completed)
	}
}
