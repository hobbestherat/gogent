package agent

import "sync"

// SubAgentLimiter bounds how many sub-agent loops may run concurrently across a
// process. The structural limits (per-parent MaxSubAgents, recursion MaxDepth)
// compose multiplicatively — up to MaxSubAgents^MaxDepth agents — so without a
// global cap a deep fan-out spawns an unbounded number of blocking goroutines
// that thunder against the backend's small connection pool (issue #23).
//
// It is a counting semaphore backed by a buffered channel. Callers use the
// try-acquire-or-run-inline pattern (see UserSession.RunSubAgentsBounded): work
// that cannot grab a slot runs inline in the caller's own goroutine instead of
// queueing. That gives bounded goroutine fan-out with natural backpressure and
// is deadlock-free even under recursive spawning (a parent holding a slot while
// its children contend for slots can always make progress inline).
//
// A nil *SubAgentLimiter, or one constructed with a non-positive size, imposes
// no limit: tryAcquire always succeeds. This keeps sessions created without a
// shared limiter (e.g. in tests) behaving exactly as before.
type SubAgentLimiter struct {
	slots chan struct{}
}

// NewSubAgentLimiter returns a limiter allowing at most n concurrent sub-agents.
// A non-positive n yields an unbounded limiter.
func NewSubAgentLimiter(n int) *SubAgentLimiter {
	if n <= 0 {
		return &SubAgentLimiter{}
	}
	return &SubAgentLimiter{slots: make(chan struct{}, n)}
}

// tryAcquire grabs a slot without blocking, reporting whether one was free. An
// unbounded limiter (nil receiver or nil channel) always succeeds.
func (l *SubAgentLimiter) tryAcquire() bool {
	if l == nil || l.slots == nil {
		return true
	}
	select {
	case l.slots <- struct{}{}:
		return true
	default:
		return false
	}
}

// release returns a previously acquired slot. It is a no-op for an unbounded
// limiter or when no slot is held.
func (l *SubAgentLimiter) release() {
	if l == nil || l.slots == nil {
		return
	}
	select {
	case <-l.slots:
	default:
	}
}

// InFlight reports how many slots are currently held. It is primarily an
// observability/testing hook; it returns 0 for an unbounded limiter.
func (l *SubAgentLimiter) InFlight() int {
	if l == nil || l.slots == nil {
		return 0
	}
	return len(l.slots)
}

// maxParallelToolCalls bounds how many independent tool calls from a single turn
// run concurrently (issue #50). It caps the goroutine fan-out — and the pressure
// on the workspace and network — when a model requests a large read-only batch
// (e.g. "read a.go, b.go and c.go"); excess calls wait for a slot.
const maxParallelToolCalls = 8

// runBoundedTools runs each task concurrently with at most maxParallelToolCalls
// active at once, blocking until every task has finished. It is the tool-call
// analogue of RunSubAgentsBounded, but bounded by a fixed local semaphore rather
// than the shared sub-agent limiter: tool calls are cheap and never recurse into
// this runner, so a plain bounded worker pool suffices. Tasks are responsible for
// their own panic recovery (see runToolCallsConcurrent).
func runBoundedTools(tasks []func()) {
	sem := make(chan struct{}, maxParallelToolCalls)
	var wg sync.WaitGroup
	for _, task := range tasks {
		task := task
		sem <- struct{}{}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			task()
		}()
	}
	wg.Wait()
}

// RunSubAgentsBounded runs each task concurrently, but with at most the shared
// limiter's worth of goroutines active at once; tasks that cannot get a slot run
// inline in the caller's goroutine as backpressure (issue #23). It blocks until
// every task has completed. Tasks are responsible for their own panic recovery —
// a panic in a task otherwise propagates out of its goroutine and crashes the
// process.
func (s *UserSession) RunSubAgentsBounded(tasks []func()) {
	s.mu.RLock()
	limiter := s.subAgentLimiter
	s.mu.RUnlock()

	var wg sync.WaitGroup
	for _, task := range tasks {
		task := task
		if limiter.tryAcquire() {
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer limiter.release()
				task()
			}()
			continue
		}
		// At the global limit: run this task inline rather than spawning another
		// goroutine. Worst case the whole batch runs sequentially, which still
		// makes progress and never deadlocks under recursion.
		task()
	}
	wg.Wait()
}
