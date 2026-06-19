package agent

import (
	"context"
	"math"
	"sync"
	"time"
)

// RateLimiter paces model requests against a provider's requests-per-minute
// ceiling using a token bucket. Permits refill continuously at rate-per-second;
// a burst of up to capacity permits is allowed before callers must wait. This is
// the throttle that keeps a wide sub-agent fan-out (or several cluster nodes
// firing at once) from stampeding a provider into 429s (issue #28).
//
// Wait blocks the caller until a permit is available — the "backoff" — so callers
// naturally serialize down to the configured rate instead of erroring. It is a
// process-wide governor: one limiter is shared across every session so the global
// request rate, not any single session's, is what the provider sees.
//
// A nil *RateLimiter, or one built with a non-positive rate, imposes no limit:
// Wait returns immediately. This keeps sessions created without a limiter (e.g.
// in tests) behaving exactly as before.
type RateLimiter struct {
	mu       sync.Mutex
	capacity float64 // max permits the bucket holds (burst)
	tokens   float64 // permits currently available
	rate     float64 // permits refilled per second
	last     time.Time
	// now is the clock, injectable so tests can drive refill deterministically.
	now func() time.Time
}

// NewRateLimiter returns a limiter allowing perMinute requests per minute, with a
// burst allowance of burst permits (defaulting to perMinute when non-positive). A
// non-positive perMinute yields an unbounded limiter.
func NewRateLimiter(perMinute, burst int) *RateLimiter {
	if perMinute <= 0 {
		return &RateLimiter{}
	}
	if burst <= 0 {
		burst = perMinute
	}
	return &RateLimiter{
		capacity: float64(burst),
		tokens:   float64(burst),
		rate:     float64(perMinute) / 60.0,
		last:     time.Now(),
		now:      time.Now,
	}
}

// take consumes one permit using the current clock, refilling the bucket for the
// elapsed time first. It returns true when a permit was granted, or false plus
// the duration to wait before one becomes available. It is the testable core of
// Wait (tests drive it through an injected clock).
func (l *RateLimiter) take() (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.now()
	if elapsed := now.Sub(l.last).Seconds(); elapsed > 0 {
		l.tokens = math.Min(l.capacity, l.tokens+elapsed*l.rate)
		l.last = now
	}
	if l.tokens >= 1 {
		l.tokens--
		return true, 0
	}
	deficit := 1 - l.tokens
	return false, time.Duration(deficit / l.rate * float64(time.Second))
}

// Wait blocks until a permit is available or ctx is cancelled, returning ctx's
// error in the latter case. An unbounded limiter (nil receiver or zero rate)
// returns immediately.
func (l *RateLimiter) Wait(ctx context.Context) error {
	if l == nil || l.rate == 0 {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	for {
		ok, wait := l.take()
		if ok {
			return nil
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}
