package watcher_test

import (
	"context"
	"errors"
	"runtime"
	"sync"
	"testing"
	"time"

	"gogent/internal/watcher"
)

// ---------------------------------------------------------------------------
// Test doubles
// ---------------------------------------------------------------------------

type notice struct{ reason, title, body string }

// fakeHost is a deterministic WatcherHost: it records every fire, tracks
// concurrent activity so tests can assert the no-overlap and max-concurrent
// invariants, and can be told to block each fire on a channel so a test can
// simulate a long-running fire and release it on demand. It never sleeps on a
// fixed wall-clock duration unless dwell is set explicitly.
type fakeHost struct {
	mu        sync.Mutex
	fires     int // total RunWatcherFire invocations
	active    int // fires currently inside RunWatcherFire
	maxActive int // high-water mark of active
	ctxErrs   int // fires that returned because ctx was cancelled
	notifies  []notice

	// block, when non-nil, makes every fire wait until the channel is closed
	// (or ctx is cancelled). Set before the manager starts firing.
	block chan struct{}
	// dwell, when > 0, makes every fire take this long (ctx-cancellable).
	dwell time.Duration
	// setResult, when non-empty, is reported via Runner.SetLastResult.
	setResult string
	// errFor maps a watcher ID to an error the fire should return.
	errFor map[string]error
}

func (h *fakeHost) RunWatcherFire(ctx context.Context, r *watcher.Runner) error {
	h.mu.Lock()
	h.fires++
	h.active++
	if h.active > h.maxActive {
		h.maxActive = h.active
	}
	block := h.block
	dwell := h.dwell
	setResult := h.setResult
	var err error
	if h.errFor != nil {
		err = h.errFor[r.ID()]
	}
	h.mu.Unlock()

	defer func() {
		h.mu.Lock()
		h.active--
		h.mu.Unlock()
	}()

	if setResult != "" {
		r.SetLastResult(setResult)
	}

	if block != nil {
		select {
		case <-block:
		case <-ctx.Done():
			h.mu.Lock()
			h.ctxErrs++
			h.mu.Unlock()
			return ctx.Err()
		}
	} else if dwell > 0 {
		select {
		case <-time.After(dwell):
		case <-ctx.Done():
			h.mu.Lock()
			h.ctxErrs++
			h.mu.Unlock()
			return ctx.Err()
		}
	}

	if ctx.Err() != nil {
		h.mu.Lock()
		h.ctxErrs++
		h.mu.Unlock()
		return ctx.Err()
	}
	return err
}

func (h *fakeHost) Notify(reason, title, body string) {
	h.mu.Lock()
	h.notifies = append(h.notifies, notice{reason, title, body})
	h.mu.Unlock()
}

func (h *fakeHost) fireCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.fires
}

func (h *fakeHost) activeCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.active
}

func (h *fakeHost) maxActiveCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.maxActive
}

func (h *fakeHost) ctxCancelled() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.ctxErrs
}

func (h *fakeHost) notifyList() []notice {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]notice, len(h.notifies))
	copy(out, h.notifies)
	return out
}

// constSchedule fires a fixed duration after each arming.
type constSchedule struct{ d time.Duration }

func (s constSchedule) Next(now time.Time) time.Time { return now.Add(s.d) }

// scriptSchedule returns a scripted sequence of offsets from now; once the
// script is exhausted it returns now+rest (default very far in the future, so
// the schedule loop parks instead of hot-spinning).
type scriptSchedule struct {
	mu   sync.Mutex
	offs []time.Duration
	i    int
	rest time.Duration
}

func newScript(rest time.Duration, offs ...time.Duration) *scriptSchedule {
	if rest == 0 {
		rest = time.Hour
	}
	return &scriptSchedule{offs: offs, rest: rest}
}

func (s *scriptSchedule) Next(now time.Time) time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	d := s.rest
	if s.i < len(s.offs) {
		d = s.offs[s.i]
		s.i++
	}
	return now.Add(d)
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// waitFor polls cond up to timeout. It avoids fixed wall-clock sleeps: it
// returns as soon as cond is satisfied.
func waitFor(t *testing.T, timeout time.Duration, cond func() bool) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(time.Millisecond)
	}
	return cond()
}

const waitTimeout = 3 * time.Second

func newRunner(id, name string, kind watcher.Kind, session string, sched watcher.Schedule, enabled bool) *watcher.Runner {
	return watcher.NewRunner(watcher.Spec{
		ID:        id,
		Name:      name,
		Task:      "task for " + name,
		Model:     "test-model",
		Kind:      kind,
		SessionID: session,
		Schedule:  sched,
		Enabled:   enabled,
	})
}

func mustGet(t *testing.T, m *watcher.Manager, id string) watcher.WatcherInfo {
	t.Helper()
	info, ok := m.Get(id)
	if !ok {
		t.Fatalf("Get(%q): expected to find watcher", id)
	}
	return info
}

// ---------------------------------------------------------------------------
// Schedule unit tests (pure, deterministic, no goroutines)
// ---------------------------------------------------------------------------

func TestIntervalScheduleNext(t *testing.T) {
	now := time.Date(2026, 6, 23, 10, 0, 0, 0, time.UTC)
	s := watcher.IntervalSchedule{D: 5 * time.Minute}
	got := s.Next(now)
	want := now.Add(5 * time.Minute)
	if !got.Equal(want) {
		t.Fatalf("Next = %v, want %v", got, want)
	}
}

func TestIntervalScheduleNonPositiveClamped(t *testing.T) {
	now := time.Date(2026, 6, 23, 10, 0, 0, 0, time.UTC)
	for _, d := range []time.Duration{0, -time.Hour} {
		s := watcher.IntervalSchedule{D: d}
		got := s.Next(now)
		if !got.After(now) {
			t.Fatalf("D=%v: Next=%v not strictly after now=%v", d, got, now)
		}
		if got.Sub(now) != time.Nanosecond {
			t.Fatalf("D=%v: expected clamp to 1ns, got delta %v", d, got.Sub(now))
		}
	}
}

func TestDailyScheduleLaterToday(t *testing.T) {
	loc := time.UTC
	now := time.Date(2026, 6, 23, 10, 0, 0, 0, loc)
	s := watcher.DailySchedule{Hour: 14, Min: 30, Loc: loc}
	got := s.Next(now)
	want := time.Date(2026, 6, 23, 14, 30, 0, 0, loc)
	if !got.Equal(want) {
		t.Fatalf("Next = %v, want %v (today)", got, want)
	}
}

func TestDailySchedulePastTodayRollsTomorrow(t *testing.T) {
	loc := time.UTC
	now := time.Date(2026, 6, 23, 15, 0, 0, 0, loc)
	s := watcher.DailySchedule{Hour: 14, Min: 30, Loc: loc}
	got := s.Next(now)
	want := time.Date(2026, 6, 24, 14, 30, 0, 0, loc)
	if !got.Equal(want) {
		t.Fatalf("Next = %v, want %v (tomorrow)", got, want)
	}
}

func TestDailyScheduleExactlyNowRollsTomorrow(t *testing.T) {
	// "strictly after now": when now is exactly the slot, fire is tomorrow.
	loc := time.UTC
	now := time.Date(2026, 6, 23, 14, 30, 0, 0, loc)
	s := watcher.DailySchedule{Hour: 14, Min: 30, Loc: loc}
	got := s.Next(now)
	want := time.Date(2026, 6, 24, 14, 30, 0, 0, loc)
	if !got.Equal(want) {
		t.Fatalf("Next = %v, want %v (tomorrow, strictly after now)", got, want)
	}
	if !got.After(now) {
		t.Fatalf("Next %v not strictly after now %v", got, now)
	}
}

func TestDailyScheduleNilLocUsesUTC(t *testing.T) {
	now := time.Date(2026, 6, 23, 10, 0, 0, 0, time.UTC)
	s := watcher.DailySchedule{Hour: 14, Min: 0, Loc: nil}
	got := s.Next(now)
	want := time.Date(2026, 6, 23, 14, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Fatalf("Next = %v, want %v (UTC default)", got, want)
	}
	if got.Location() != time.UTC {
		t.Fatalf("Next location = %v, want UTC", got.Location())
	}
}

func TestDailyScheduleRespectsLocation(t *testing.T) {
	// +2h fixed zone. 07:00 in that zone == 05:00 UTC. At 06:00 zone time
	// (04:00 UTC) the next 07:00-zone slot is the same day.
	zone := time.FixedZone("TST", 2*60*60)
	now := time.Date(2026, 6, 23, 6, 0, 0, 0, zone)
	s := watcher.DailySchedule{Hour: 7, Min: 0, Loc: zone}
	got := s.Next(now)
	if !got.After(now) {
		t.Fatalf("Next %v not after now %v", got, now)
	}
	inZone := got.In(zone)
	if inZone.Hour() != 7 || inZone.Minute() != 0 {
		t.Fatalf("Next in zone = %02d:%02d, want 07:00", inZone.Hour(), inZone.Minute())
	}
	// 07:00 +0200 == 05:00 UTC.
	if got.UTC().Hour() != 5 {
		t.Fatalf("Next in UTC hour = %d, want 5", got.UTC().Hour())
	}
}

func TestDailyScheduleConsecutiveDays(t *testing.T) {
	loc := time.UTC
	s := watcher.DailySchedule{Hour: 7, Min: 0, Loc: loc}
	now := time.Date(2026, 6, 23, 8, 0, 0, 0, loc)
	first := s.Next(now)    // tomorrow 07:00
	second := s.Next(first) // strictly after first -> next day 07:00
	if !second.After(first) {
		t.Fatalf("second %v not after first %v", second, first)
	}
	if d := second.Sub(first); d != 24*time.Hour {
		t.Fatalf("gap between consecutive daily fires = %v, want 24h", d)
	}
}

// ---------------------------------------------------------------------------
// Manager: firing on schedule
// ---------------------------------------------------------------------------

func TestFiresOnSchedule(t *testing.T) {
	host := &fakeHost{}
	m := watcher.NewManager(host)
	r := newRunner("w1", "alpha", watcher.KindFree, "", newScript(time.Hour, 20*time.Millisecond), true)
	if err := m.Add(r); err != nil {
		t.Fatalf("Add: %v", err)
	}
	m.Start()
	defer m.Stop()

	if !waitFor(t, waitTimeout, func() bool { return host.fireCount() == 1 }) {
		t.Fatalf("expected exactly 1 fire, got %d", host.fireCount())
	}
	// nextFire should be recorded.
	if mustGet(t, m, "w1").NextFire.IsZero() {
		t.Fatalf("NextFire not recorded")
	}
	// Eventually idle after the single fire.
	if !waitFor(t, waitTimeout, func() bool { return mustGet(t, m, "w1").Status == watcher.StatusIdle }) {
		t.Fatalf("status = %v, want idle", mustGet(t, m, "w1").Status)
	}
}

func TestNoCatchUpBurstFirstFireDelayed(t *testing.T) {
	// First fire must be one interval out, never immediately on Start.
	host := &fakeHost{}
	m := watcher.NewManager(host)
	r := newRunner("w1", "alpha", watcher.KindFree, "", constSchedule{200 * time.Millisecond}, true)
	if err := m.Add(r); err != nil {
		t.Fatalf("Add: %v", err)
	}
	m.Start()
	defer m.Stop()

	// Immediately after Start there must be no fire.
	if host.fireCount() != 0 {
		t.Fatalf("fired immediately on Start: count=%d (no catch-up burst expected)", host.fireCount())
	}
	if !waitFor(t, waitTimeout, func() bool { return host.fireCount() >= 1 }) {
		t.Fatalf("expected a fire after one interval, got %d", host.fireCount())
	}
}

// ---------------------------------------------------------------------------
// Manager: serialization (never overlap a watcher with itself)
// ---------------------------------------------------------------------------

func TestFiresOfOneWatcherAreSerial(t *testing.T) {
	host := &fakeHost{dwell: 25 * time.Millisecond}
	m := watcher.NewManager(host, watcher.WithSkipIfRunning(false))
	// Many quick arming offsets; dwell makes each fire outlast the next arm.
	r := newRunner("w1", "alpha", watcher.KindFree, "",
		newScript(time.Hour, 10*time.Millisecond, 10*time.Millisecond, 10*time.Millisecond, 10*time.Millisecond, 10*time.Millisecond), true)
	if err := m.Add(r); err != nil {
		t.Fatalf("Add: %v", err)
	}
	m.Start()
	defer m.Stop()

	// Let several arming windows elapse.
	waitFor(t, waitTimeout, func() bool { return host.fireCount() >= 2 })
	if got := host.maxActiveCount(); got > 1 {
		t.Fatalf("watcher overlapped itself: maxActive=%d, want 1", got)
	}
}

// ---------------------------------------------------------------------------
// Manager: skip-if-running
// ---------------------------------------------------------------------------

func TestSkippedWhenPriorStillRunning(t *testing.T) {
	host := &fakeHost{block: make(chan struct{})}
	m := watcher.NewManager(host) // default skipIfRunning = true
	r := newRunner("w1", "alpha", watcher.KindFree, "",
		newScript(time.Hour, 15*time.Millisecond, 15*time.Millisecond), true)
	if err := m.Add(r); err != nil {
		t.Fatalf("Add: %v", err)
	}
	m.Start()
	defer func() {
		close(host.block)
		m.Stop()
	}()

	// First fire starts and blocks.
	if !waitFor(t, waitTimeout, func() bool { return host.activeCount() == 1 }) {
		t.Fatalf("first fire never started")
	}
	// Second due fire arrives while first is blocked -> skipped.
	if !waitFor(t, waitTimeout, func() bool { return mustGet(t, m, "w1").Status == watcher.StatusSkipped }) {
		t.Fatalf("status = %v, want skipped", mustGet(t, m, "w1").Status)
	}
	// Only one real fire actually executed.
	if got := host.fireCount(); got != 1 {
		t.Fatalf("fireCount = %d while blocked, want 1 (the rest skipped)", got)
	}
}

func TestSkipDisabledDoesNotRecordSkip(t *testing.T) {
	host := &fakeHost{block: make(chan struct{})}
	m := watcher.NewManager(host, watcher.WithSkipIfRunning(false))
	r := newRunner("w1", "alpha", watcher.KindFree, "",
		newScript(time.Hour, 15*time.Millisecond, 15*time.Millisecond), true)
	if err := m.Add(r); err != nil {
		t.Fatalf("Add: %v", err)
	}
	m.Start()
	defer func() {
		close(host.block)
		m.Stop()
	}()

	if !waitFor(t, waitTimeout, func() bool { return host.activeCount() == 1 }) {
		t.Fatalf("first fire never started")
	}
	// Give the second due window time to be dropped (not recorded as skipped).
	time.Sleep(60 * time.Millisecond)
	if st := mustGet(t, m, "w1").Status; st == watcher.StatusSkipped {
		t.Fatalf("status = skipped with skipIfRunning=false, want running")
	}
	// Still no overlap.
	if host.maxActiveCount() > 1 {
		t.Fatalf("overlapped with skipIfRunning=false: maxActive=%d", host.maxActiveCount())
	}
}

// ---------------------------------------------------------------------------
// Manager: global max-concurrent bound
// ---------------------------------------------------------------------------

func TestMaxConcurrentBound(t *testing.T) {
	const limit = 2
	host := &fakeHost{block: make(chan struct{})}
	m := watcher.NewManager(host, watcher.WithMaxConcurrent(limit))
	m.Start()
	for i, name := range []string{"a", "b", "c", "d", "e"} {
		r := newRunner(name, name, watcher.KindFree, "", constSchedule{time.Hour}, true)
		if err := m.Add(r); err != nil {
			t.Fatalf("Add %d: %v", i, err)
		}
	}
	defer func() {
		close(host.block)
		m.Stop()
	}()

	// Trigger all five at once; only `limit` may execute concurrently.
	for _, name := range []string{"a", "b", "c", "d", "e"} {
		if err := m.RunNow(name); err != nil {
			t.Fatalf("RunNow %s: %v", name, err)
		}
	}

	if !waitFor(t, waitTimeout, func() bool { return host.activeCount() == limit }) {
		t.Fatalf("never reached %d concurrent fires (active=%d)", limit, host.activeCount())
	}
	// Hold a moment; it must never exceed the limit.
	time.Sleep(80 * time.Millisecond)
	if got := host.maxActiveCount(); got > limit {
		t.Fatalf("maxActive = %d, exceeds bound %d", got, limit)
	}
	if got := host.activeCount(); got != limit {
		t.Fatalf("active = %d, want pinned at %d while blocked", got, limit)
	}
}

func TestMaxConcurrentZeroIgnoredDefaultFour(t *testing.T) {
	// WithMaxConcurrent(0) is ignored -> default 4.
	const want = 4
	host := &fakeHost{block: make(chan struct{})}
	m := watcher.NewManager(host, watcher.WithMaxConcurrent(0))
	m.Start()
	names := []string{"a", "b", "c", "d", "e", "f"}
	for _, name := range names {
		if err := m.Add(newRunner(name, name, watcher.KindFree, "", constSchedule{time.Hour}, true)); err != nil {
			t.Fatalf("Add %s: %v", name, err)
		}
	}
	defer func() {
		close(host.block)
		m.Stop()
	}()
	for _, name := range names {
		if err := m.RunNow(name); err != nil {
			t.Fatalf("RunNow %s: %v", name, err)
		}
	}
	if !waitFor(t, waitTimeout, func() bool { return host.activeCount() == want }) {
		t.Fatalf("never reached default bound %d (active=%d)", want, host.activeCount())
	}
	time.Sleep(80 * time.Millisecond)
	if got := host.maxActiveCount(); got > want {
		t.Fatalf("maxActive = %d, exceeds default bound %d", got, want)
	}
}

// ---------------------------------------------------------------------------
// Manager: notifications
// ---------------------------------------------------------------------------

func TestFreeRunningFireNotifies(t *testing.T) {
	host := &fakeHost{setResult: "did the thing"}
	m := watcher.NewManager(host)
	m.Start()
	r := newRunner("w1", "emailer", watcher.KindFree, "", constSchedule{time.Hour}, true)
	if err := m.Add(r); err != nil {
		t.Fatalf("Add: %v", err)
	}
	defer m.Stop()
	if err := m.RunNow("w1"); err != nil {
		t.Fatalf("RunNow: %v", err)
	}
	if !waitFor(t, waitTimeout, func() bool { return len(host.notifyList()) == 1 }) {
		t.Fatalf("expected 1 notify, got %d", len(host.notifyList()))
	}
	n := host.notifyList()[0]
	if n.reason != "watcher" {
		t.Fatalf("notify reason = %q, want %q", n.reason, "watcher")
	}
	if n.title != "emailer" {
		t.Fatalf("notify title = %q, want watcher name", n.title)
	}
	if n.body != "did the thing" {
		t.Fatalf("notify body = %q, want last result summary", n.body)
	}
}

func TestFreeRunningNotifyDefaultBodyWhenNoResult(t *testing.T) {
	host := &fakeHost{} // no setResult
	m := watcher.NewManager(host)
	m.Start()
	r := newRunner("w1", "emailer", watcher.KindFree, "", constSchedule{time.Hour}, true)
	if err := m.Add(r); err != nil {
		t.Fatalf("Add: %v", err)
	}
	defer m.Stop()
	if err := m.RunNow("w1"); err != nil {
		t.Fatalf("RunNow: %v", err)
	}
	if !waitFor(t, waitTimeout, func() bool { return len(host.notifyList()) == 1 }) {
		t.Fatalf("expected 1 notify, got %d", len(host.notifyList()))
	}
	if body := host.notifyList()[0].body; body == "" {
		t.Fatalf("notify body empty, want a default completion message")
	}
}

func TestAttachedFireDoesNotNotify(t *testing.T) {
	host := &fakeHost{}
	m := watcher.NewManager(host)
	m.Start()
	r := newRunner("w1", "poller", watcher.KindAttached, "sessA", constSchedule{time.Hour}, true)
	if err := m.Add(r); err != nil {
		t.Fatalf("Add: %v", err)
	}
	defer m.Stop()
	if err := m.RunNow("w1"); err != nil {
		t.Fatalf("RunNow: %v", err)
	}
	if !waitFor(t, waitTimeout, func() bool { return host.fireCount() == 1 }) {
		t.Fatalf("attached watcher did not fire")
	}
	// Give any erroneous notify a chance to land.
	time.Sleep(40 * time.Millisecond)
	if got := len(host.notifyList()); got != 0 {
		t.Fatalf("attached watcher notified %d times, want 0", got)
	}
}

func TestFailedFireDoesNotNotifyAndRecordsError(t *testing.T) {
	wantErr := errors.New("boom")
	host := &fakeHost{errFor: map[string]error{"w1": wantErr}}
	m := watcher.NewManager(host)
	m.Start()
	r := newRunner("w1", "emailer", watcher.KindFree, "", constSchedule{time.Hour}, true)
	if err := m.Add(r); err != nil {
		t.Fatalf("Add: %v", err)
	}
	defer m.Stop()
	if err := m.RunNow("w1"); err != nil {
		t.Fatalf("RunNow: %v", err)
	}
	if !waitFor(t, waitTimeout, func() bool { return mustGet(t, m, "w1").Status == watcher.StatusFailed }) {
		t.Fatalf("status = %v, want failed", mustGet(t, m, "w1").Status)
	}
	info := mustGet(t, m, "w1")
	if info.LastError != wantErr.Error() {
		t.Fatalf("LastError = %q, want %q", info.LastError, wantErr.Error())
	}
	// A failed free-running fire must not emit a completion notification.
	time.Sleep(40 * time.Millisecond)
	if got := len(host.notifyList()); got != 0 {
		t.Fatalf("failed fire notified %d times, want 0", got)
	}
}

func TestSuccessAfterFailureClearsError(t *testing.T) {
	host := &fakeHost{errFor: map[string]error{"w1": errors.New("boom")}}
	m := watcher.NewManager(host)
	m.Start()
	r := newRunner("w1", "emailer", watcher.KindFree, "", constSchedule{time.Hour}, true)
	if err := m.Add(r); err != nil {
		t.Fatalf("Add: %v", err)
	}
	defer m.Stop()

	if err := m.RunNow("w1"); err != nil {
		t.Fatalf("RunNow: %v", err)
	}
	if !waitFor(t, waitTimeout, func() bool { return mustGet(t, m, "w1").Status == watcher.StatusFailed }) {
		t.Fatalf("expected failed first")
	}
	// Remove the scripted error and fire again.
	host.mu.Lock()
	host.errFor = nil
	host.mu.Unlock()
	if err := m.RunNow("w1"); err != nil {
		t.Fatalf("RunNow 2: %v", err)
	}
	if !waitFor(t, waitTimeout, func() bool {
		info := mustGet(t, m, "w1")
		return info.Status == watcher.StatusIdle && info.LastError == ""
	}) {
		info := mustGet(t, m, "w1")
		t.Fatalf("after success: status=%v lastError=%q, want idle/empty", info.Status, info.LastError)
	}
}

// ---------------------------------------------------------------------------
// Manager: Stop cancels in-flight fire and drains goroutines
// ---------------------------------------------------------------------------

func TestStopCancelsInFlightFire(t *testing.T) {
	host := &fakeHost{block: make(chan struct{})} // never released
	m := watcher.NewManager(host)
	m.Start()
	r := newRunner("w1", "alpha", watcher.KindFree, "", constSchedule{time.Hour}, true)
	if err := m.Add(r); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := m.RunNow("w1"); err != nil {
		t.Fatalf("RunNow: %v", err)
	}
	if !waitFor(t, waitTimeout, func() bool { return host.activeCount() == 1 }) {
		t.Fatalf("fire never started")
	}

	done := make(chan struct{})
	go func() {
		m.Stop() // must cancel the blocked fire via ctx and return
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(waitTimeout):
		t.Fatalf("Stop did not return: in-flight fire was not cancelled")
	}
	if host.ctxCancelled() < 1 {
		t.Fatalf("in-flight fire did not observe ctx cancellation")
	}
}

func TestStopIdempotent(t *testing.T) {
	host := &fakeHost{}
	m := watcher.NewManager(host)
	m.Start()
	m.Stop()
	// Second Stop must not panic or block.
	done := make(chan struct{})
	go func() { m.Stop(); close(done) }()
	select {
	case <-done:
	case <-time.After(waitTimeout):
		t.Fatalf("second Stop blocked")
	}
}

func TestStopDrainsGoroutines(t *testing.T) {
	host := &fakeHost{dwell: 10 * time.Millisecond}
	base := runtime.NumGoroutine()
	m := watcher.NewManager(host)
	for _, name := range []string{"a", "b", "c", "d"} {
		if err := m.Add(newRunner(name, name, watcher.KindFree, "", constSchedule{15 * time.Millisecond}, true)); err != nil {
			t.Fatalf("Add %s: %v", name, err)
		}
	}
	m.Start()
	// Let some fires happen.
	waitFor(t, waitTimeout, func() bool { return host.fireCount() >= 2 })
	m.Stop()
	// After Stop returns, all schedule loops + fires must be gone.
	if !waitFor(t, waitTimeout, func() bool { return runtime.NumGoroutine() <= base+2 }) {
		t.Fatalf("goroutines leaked after Stop: now=%d base=%d", runtime.NumGoroutine(), base)
	}
}

// ---------------------------------------------------------------------------
// Manager: Toggle
// ---------------------------------------------------------------------------

func TestToggleDisablesThenEnables(t *testing.T) {
	host := &fakeHost{}
	m := watcher.NewManager(host)
	r := newRunner("w1", "alpha", watcher.KindFree, "", constSchedule{80 * time.Millisecond}, true)
	if err := m.Add(r); err != nil {
		t.Fatalf("Add: %v", err)
	}
	m.Start()
	defer m.Stop()

	// Disable before the first (80ms-armed) fire happens.
	if err := m.Toggle("w1"); err != nil {
		t.Fatalf("Toggle disable: %v", err)
	}
	if mustGet(t, m, "w1").Enabled {
		t.Fatalf("watcher still enabled after disable")
	}
	// The pending fire must be cancelled: no fire for a while.
	time.Sleep(250 * time.Millisecond)
	if got := host.fireCount(); got != 0 {
		t.Fatalf("disabled watcher fired %d times, want 0", got)
	}

	// Re-enable: the schedule must arm again and fire.
	if err := m.Toggle("w1"); err != nil {
		t.Fatalf("Toggle enable: %v", err)
	}
	if !mustGet(t, m, "w1").Enabled {
		t.Fatalf("watcher not enabled after re-enable")
	}
	if !waitFor(t, waitTimeout, func() bool { return host.fireCount() >= 1 }) {
		t.Fatalf("re-enabled watcher never fired")
	}
}

func TestToggleNotFound(t *testing.T) {
	m := watcher.NewManager(&fakeHost{})
	m.Start()
	defer m.Stop()
	if err := m.Toggle("nope"); !errors.Is(err, watcher.ErrNotFound) {
		t.Fatalf("Toggle unknown = %v, want ErrNotFound", err)
	}
}

// ---------------------------------------------------------------------------
// Manager: RunNow
// ---------------------------------------------------------------------------

func TestRunNowFiresImmediately(t *testing.T) {
	host := &fakeHost{}
	m := watcher.NewManager(host)
	m.Start()
	// Schedule is far in the future; only RunNow can make it fire soon.
	r := newRunner("w1", "alpha", watcher.KindFree, "", constSchedule{time.Hour}, true)
	if err := m.Add(r); err != nil {
		t.Fatalf("Add: %v", err)
	}
	defer m.Stop()
	if err := m.RunNow("w1"); err != nil {
		t.Fatalf("RunNow: %v", err)
	}
	if !waitFor(t, waitTimeout, func() bool { return host.fireCount() == 1 }) {
		t.Fatalf("RunNow did not fire promptly (count=%d)", host.fireCount())
	}
}

func TestRunNowRespectsNoOverlap(t *testing.T) {
	host := &fakeHost{block: make(chan struct{})}
	m := watcher.NewManager(host) // skipIfRunning default true
	m.Start()
	r := newRunner("w1", "alpha", watcher.KindFree, "", constSchedule{time.Hour}, true)
	if err := m.Add(r); err != nil {
		t.Fatalf("Add: %v", err)
	}
	defer func() {
		close(host.block)
		m.Stop()
	}()

	if err := m.RunNow("w1"); err != nil {
		t.Fatalf("RunNow 1: %v", err)
	}
	if !waitFor(t, waitTimeout, func() bool { return host.activeCount() == 1 }) {
		t.Fatalf("first fire never started")
	}
	// Second RunNow while first is in flight must not start a 2nd fire.
	if err := m.RunNow("w1"); err != nil {
		t.Fatalf("RunNow 2: %v", err)
	}
	time.Sleep(50 * time.Millisecond)
	if got := host.fireCount(); got != 1 {
		t.Fatalf("overlapping RunNow started %d fires, want 1", got)
	}
	if st := mustGet(t, m, "w1").Status; st != watcher.StatusSkipped && st != watcher.StatusRunning {
		t.Fatalf("status = %v, want running or skipped", st)
	}
}

func TestRunNowNotFound(t *testing.T) {
	m := watcher.NewManager(&fakeHost{})
	m.Start()
	defer m.Stop()
	if err := m.RunNow("nope"); !errors.Is(err, watcher.ErrNotFound) {
		t.Fatalf("RunNow unknown = %v, want ErrNotFound", err)
	}
}

// TestStartAfterRunNowStillArmsSchedule guards the documented contract that
// Start drives the schedules of all enabled watchers. RunNow is "fire now,
// ignoring the schedule" — it must not silently disable later scheduled fires.
func TestStartAfterRunNowStillArmsSchedule(t *testing.T) {
	host := &fakeHost{}
	m := watcher.NewManager(host)
	r := newRunner("w1", "alpha", watcher.KindFree, "", constSchedule{40 * time.Millisecond}, true)
	if err := m.Add(r); err != nil {
		t.Fatalf("Add: %v", err)
	}
	// Fire once before Start.
	if err := m.RunNow("w1"); err != nil {
		t.Fatalf("RunNow: %v", err)
	}
	if !waitFor(t, waitTimeout, func() bool { return host.fireCount() >= 1 }) {
		t.Fatalf("RunNow did not fire")
	}
	// Now Start: the 40ms schedule must drive further fires.
	m.Start()
	defer m.Stop()
	if !waitFor(t, waitTimeout, func() bool { return host.fireCount() >= 3 }) {
		t.Fatalf("Start did not arm the schedule after RunNow: only %d fires", host.fireCount())
	}
}

// ---------------------------------------------------------------------------
// Manager: StopWatcher (cancel in-flight, keep schedule)
// ---------------------------------------------------------------------------

func TestStopWatcherCancelsInFlight(t *testing.T) {
	host := &fakeHost{block: make(chan struct{})}
	m := watcher.NewManager(host)
	m.Start()
	r := newRunner("w1", "alpha", watcher.KindFree, "", constSchedule{time.Hour}, true)
	if err := m.Add(r); err != nil {
		t.Fatalf("Add: %v", err)
	}
	defer func() {
		close(host.block)
		m.Stop()
	}()

	if err := m.RunNow("w1"); err != nil {
		t.Fatalf("RunNow: %v", err)
	}
	if !waitFor(t, waitTimeout, func() bool { return host.activeCount() == 1 }) {
		t.Fatalf("fire never started")
	}
	if err := m.StopWatcher("w1"); err != nil {
		t.Fatalf("StopWatcher: %v", err)
	}
	if !waitFor(t, waitTimeout, func() bool { return host.ctxCancelled() >= 1 }) {
		t.Fatalf("StopWatcher did not cancel the in-flight fire")
	}
	// Watcher remains registered (schedule not removed).
	if _, ok := m.Get("w1"); !ok {
		t.Fatalf("StopWatcher dropped the watcher; it should only cancel the fire")
	}
}

func TestStopWatcherNotFound(t *testing.T) {
	m := watcher.NewManager(&fakeHost{})
	m.Start()
	defer m.Stop()
	if err := m.StopWatcher("nope"); !errors.Is(err, watcher.ErrNotFound) {
		t.Fatalf("StopWatcher unknown = %v, want ErrNotFound", err)
	}
}

// ---------------------------------------------------------------------------
// Manager: Remove
// ---------------------------------------------------------------------------

func TestRemoveStopsAndDrops(t *testing.T) {
	host := &fakeHost{block: make(chan struct{})}
	m := watcher.NewManager(host)
	m.Start()
	r := newRunner("w1", "alpha", watcher.KindFree, "", constSchedule{time.Hour}, true)
	if err := m.Add(r); err != nil {
		t.Fatalf("Add: %v", err)
	}
	defer func() {
		close(host.block)
		m.Stop()
	}()

	if err := m.RunNow("w1"); err != nil {
		t.Fatalf("RunNow: %v", err)
	}
	if !waitFor(t, waitTimeout, func() bool { return host.activeCount() == 1 }) {
		t.Fatalf("fire never started")
	}
	if err := m.Remove("w1"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	// Dropped from the manager.
	if _, ok := m.Get("w1"); ok {
		t.Fatalf("watcher still present after Remove")
	}
	// In-flight fire cancelled.
	if !waitFor(t, waitTimeout, func() bool { return host.ctxCancelled() >= 1 }) {
		t.Fatalf("Remove did not cancel the in-flight fire")
	}
}

func TestRemoveStopsFutureFires(t *testing.T) {
	host := &fakeHost{}
	m := watcher.NewManager(host)
	r := newRunner("w1", "alpha", watcher.KindFree, "", constSchedule{60 * time.Millisecond}, true)
	if err := m.Add(r); err != nil {
		t.Fatalf("Add: %v", err)
	}
	m.Start()
	defer m.Stop()
	if err := m.Remove("w1"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	time.Sleep(200 * time.Millisecond)
	if got := host.fireCount(); got != 0 {
		t.Fatalf("removed watcher fired %d times, want 0", got)
	}
}

func TestRemoveNotFound(t *testing.T) {
	m := watcher.NewManager(&fakeHost{})
	m.Start()
	defer m.Stop()
	if err := m.Remove("nope"); !errors.Is(err, watcher.ErrNotFound) {
		t.Fatalf("Remove unknown = %v, want ErrNotFound", err)
	}
}

// ---------------------------------------------------------------------------
// Manager: ListWatchers contract
// ---------------------------------------------------------------------------

func setupListManager(t *testing.T) *watcher.Manager {
	t.Helper()
	m := watcher.NewManager(&fakeHost{})
	add := func(id, session string, kind watcher.Kind) {
		if err := m.Add(newRunner(id, id, kind, session, constSchedule{time.Hour}, false)); err != nil {
			t.Fatalf("Add %s: %v", id, err)
		}
	}
	add("free1", "", watcher.KindFree)
	add("free2", "", watcher.KindFree)
	add("attA1", "sessA", watcher.KindAttached)
	add("attA2", "sessA", watcher.KindAttached)
	add("attB1", "sessB", watcher.KindAttached)
	return m
}

func idSet(infos []watcher.WatcherInfo) map[string]bool {
	s := make(map[string]bool, len(infos))
	for _, i := range infos {
		s[i.ID] = true
	}
	return s
}

func assertIDs(t *testing.T, got map[string]bool, want ...string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("ids = %v, want exactly %v", got, want)
	}
	for _, w := range want {
		if !got[w] {
			t.Fatalf("missing id %q in %v (want %v)", w, got, want)
		}
	}
}

func TestListWatchersForSession(t *testing.T) {
	m := setupListManager(t)
	assertIDs(t, idSet(m.ListWatchers("sessA")), "free1", "free2", "attA1", "attA2")
}

func TestListWatchersOtherSessionAttachedHidden(t *testing.T) {
	m := setupListManager(t)
	got := idSet(m.ListWatchers("sessA"))
	if got["attB1"] {
		t.Fatalf("session A listing leaked session B's attached watcher")
	}
}

func TestListWatchersEmptySessionFreeOnly(t *testing.T) {
	m := setupListManager(t)
	assertIDs(t, idSet(m.ListWatchers("")), "free1", "free2")
}

func TestListWatchersUnknownSessionFreeOnly(t *testing.T) {
	m := setupListManager(t)
	assertIDs(t, idSet(m.ListWatchers("ghost")), "free1", "free2")
}

func TestListWatchersSessionBOnlyOwn(t *testing.T) {
	m := setupListManager(t)
	assertIDs(t, idSet(m.ListWatchers("sessB")), "free1", "free2", "attB1")
}

// ---------------------------------------------------------------------------
// Manager: RemoveAttachedForSession
// ---------------------------------------------------------------------------

func TestRemoveAttachedForSession(t *testing.T) {
	m := setupListManager(t)
	m.RemoveAttachedForSession("sessA")
	// Session A's attached are gone.
	if _, ok := m.Get("attA1"); ok {
		t.Fatalf("attA1 not removed")
	}
	if _, ok := m.Get("attA2"); ok {
		t.Fatalf("attA2 not removed")
	}
	// Free + session B untouched.
	for _, id := range []string{"free1", "free2", "attB1"} {
		if _, ok := m.Get(id); !ok {
			t.Fatalf("%s should survive RemoveAttachedForSession(sessA)", id)
		}
	}
}

func TestRemoveAttachedForSessionUnknownNoop(t *testing.T) {
	m := setupListManager(t)
	m.RemoveAttachedForSession("ghost")
	for _, id := range []string{"free1", "free2", "attA1", "attA2", "attB1"} {
		if _, ok := m.Get(id); !ok {
			t.Fatalf("%s wrongly removed for unknown session", id)
		}
	}
}

func TestRemoveAttachedForSessionLeavesFreeWithSameAccident(t *testing.T) {
	// A free watcher with a non-empty (but irrelevant) session id must not be
	// removed by RemoveAttachedForSession.
	m := watcher.NewManager(&fakeHost{})
	if err := m.Add(newRunner("f", "f", watcher.KindFree, "sessA", constSchedule{time.Hour}, false)); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := m.Add(newRunner("a", "a", watcher.KindAttached, "sessA", constSchedule{time.Hour}, false)); err != nil {
		t.Fatalf("Add: %v", err)
	}
	m.RemoveAttachedForSession("sessA")
	if _, ok := m.Get("f"); !ok {
		t.Fatalf("free watcher removed by RemoveAttachedForSession")
	}
	if _, ok := m.Get("a"); ok {
		t.Fatalf("attached watcher not removed")
	}
}

// ---------------------------------------------------------------------------
// Manager: Add / lifecycle errors
// ---------------------------------------------------------------------------

func TestAddDuplicateID(t *testing.T) {
	m := watcher.NewManager(&fakeHost{})
	r1 := newRunner("dup", "one", watcher.KindFree, "", constSchedule{time.Hour}, false)
	r2 := newRunner("dup", "two", watcher.KindFree, "", constSchedule{time.Hour}, false)
	if err := m.Add(r1); err != nil {
		t.Fatalf("Add r1: %v", err)
	}
	if err := m.Add(r2); !errors.Is(err, watcher.ErrDuplicate) {
		t.Fatalf("Add duplicate = %v, want ErrDuplicate", err)
	}
}

func TestAddAfterStopRejected(t *testing.T) {
	m := watcher.NewManager(&fakeHost{})
	m.Start()
	m.Stop()
	r := newRunner("w1", "alpha", watcher.KindFree, "", constSchedule{time.Hour}, true)
	if err := m.Add(r); !errors.Is(err, watcher.ErrStopped) {
		t.Fatalf("Add after Stop = %v, want ErrStopped", err)
	}
}

func TestRunNowAfterStopRejected(t *testing.T) {
	m := watcher.NewManager(&fakeHost{})
	r := newRunner("w1", "alpha", watcher.KindFree, "", constSchedule{time.Hour}, true)
	if err := m.Add(r); err != nil {
		t.Fatalf("Add: %v", err)
	}
	m.Start()
	m.Stop()
	if err := m.RunNow("w1"); !errors.Is(err, watcher.ErrStopped) {
		t.Fatalf("RunNow after Stop = %v, want ErrStopped", err)
	}
}

func TestAddEnabledWhileStartedLaunchesLoop(t *testing.T) {
	host := &fakeHost{}
	m := watcher.NewManager(host)
	m.Start()
	defer m.Stop()
	r := newRunner("w1", "alpha", watcher.KindFree, "", constSchedule{40 * time.Millisecond}, true)
	if err := m.Add(r); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if !waitFor(t, waitTimeout, func() bool { return host.fireCount() >= 1 }) {
		t.Fatalf("watcher added after Start never fired")
	}
}

func TestAddDisabledWhileStartedDoesNotFire(t *testing.T) {
	host := &fakeHost{}
	m := watcher.NewManager(host)
	m.Start()
	defer m.Stop()
	r := newRunner("w1", "alpha", watcher.KindFree, "", constSchedule{40 * time.Millisecond}, false)
	if err := m.Add(r); err != nil {
		t.Fatalf("Add: %v", err)
	}
	time.Sleep(200 * time.Millisecond)
	if got := host.fireCount(); got != 0 {
		t.Fatalf("disabled watcher fired %d times, want 0", got)
	}
	// Enabling it should make it run.
	if err := m.Toggle("w1"); err != nil {
		t.Fatalf("Toggle: %v", err)
	}
	if !waitFor(t, waitTimeout, func() bool { return host.fireCount() >= 1 }) {
		t.Fatalf("enabled watcher never fired")
	}
}

// ---------------------------------------------------------------------------
// Manager: Get / resolution by id and name
// ---------------------------------------------------------------------------

func TestGetByIDAndName(t *testing.T) {
	m := watcher.NewManager(&fakeHost{})
	r := newRunner("the-id", "the-name", watcher.KindAttached, "sessX", constSchedule{time.Hour}, false)
	if err := m.Add(r); err != nil {
		t.Fatalf("Add: %v", err)
	}
	byID, ok := m.Get("the-id")
	if !ok {
		t.Fatalf("Get by id failed")
	}
	byName, ok := m.Get("the-name")
	if !ok {
		t.Fatalf("Get by name failed")
	}
	if byID.ID != byName.ID {
		t.Fatalf("id/name resolution mismatch: %q vs %q", byID.ID, byName.ID)
	}
	if byID.Name != "the-name" || byID.Kind != watcher.KindAttached || byID.TargetSession != "sessX" {
		t.Fatalf("unexpected snapshot: %+v", byID)
	}
}

func TestGetNotFound(t *testing.T) {
	m := watcher.NewManager(&fakeHost{})
	if _, ok := m.Get("nope"); ok {
		t.Fatalf("Get on empty manager returned ok=true")
	}
}

func TestControlMethodsResolveByName(t *testing.T) {
	host := &fakeHost{}
	m := watcher.NewManager(host)
	m.Start()
	defer m.Stop()
	r := newRunner("id-1", "friendly", watcher.KindFree, "", constSchedule{time.Hour}, true)
	if err := m.Add(r); err != nil {
		t.Fatalf("Add: %v", err)
	}
	// RunNow by name.
	if err := m.RunNow("friendly"); err != nil {
		t.Fatalf("RunNow by name: %v", err)
	}
	if !waitFor(t, waitTimeout, func() bool { return host.fireCount() == 1 }) {
		t.Fatalf("RunNow by name did not fire")
	}
	// Toggle by name.
	if err := m.Toggle("friendly"); err != nil {
		t.Fatalf("Toggle by name: %v", err)
	}
	if mustGet(t, m, "id-1").Enabled {
		t.Fatalf("Toggle by name did not disable")
	}
}

// ---------------------------------------------------------------------------
// Manager: snapshot integrity
// ---------------------------------------------------------------------------

func TestWatcherInfoSnapshotFields(t *testing.T) {
	host := &fakeHost{setResult: "summary line"}
	m := watcher.NewManager(host)
	m.Start()
	defer m.Stop()
	r := newRunner("w1", "alpha", watcher.KindFree, "", constSchedule{time.Hour}, true)
	if err := m.Add(r); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := m.RunNow("w1"); err != nil {
		t.Fatalf("RunNow: %v", err)
	}
	if !waitFor(t, waitTimeout, func() bool {
		return mustGet(t, m, "w1").Status == watcher.StatusIdle && mustGet(t, m, "w1").LastResult != ""
	}) {
		t.Fatalf("fire did not complete with result")
	}
	info := mustGet(t, m, "w1")
	if info.ID != "w1" || info.Name != "alpha" {
		t.Fatalf("identity wrong: %+v", info)
	}
	if info.Kind != watcher.KindFree || info.TargetSession != "" {
		t.Fatalf("free watcher should have empty TargetSession: %+v", info)
	}
	if info.LastResult != "summary line" {
		t.Fatalf("LastResult = %q, want %q", info.LastResult, "summary line")
	}
	if info.LastRun.IsZero() {
		t.Fatalf("LastRun not recorded")
	}
}

func TestAttachedSnapshotHasTargetSession(t *testing.T) {
	m := watcher.NewManager(&fakeHost{})
	r := newRunner("w1", "alpha", watcher.KindAttached, "sessZ", constSchedule{time.Hour}, false)
	if err := m.Add(r); err != nil {
		t.Fatalf("Add: %v", err)
	}
	info := mustGet(t, m, "w1")
	if info.TargetSession != "sessZ" {
		t.Fatalf("TargetSession = %q, want sessZ", info.TargetSession)
	}
}

// ---------------------------------------------------------------------------
// Status stringer
// ---------------------------------------------------------------------------

func TestStatusString(t *testing.T) {
	cases := map[watcher.Status]string{
		watcher.StatusIdle:    "idle",
		watcher.StatusRunning: "running",
		watcher.StatusSkipped: "skipped",
		watcher.StatusFailed:  "failed",
	}
	for st, want := range cases {
		if got := st.String(); got != want {
			t.Fatalf("Status(%d).String() = %q, want %q", st, got, want)
		}
	}
}

// ---------------------------------------------------------------------------
// Concurrency smoke test (no -race on Pi5, but catches panics/deadlocks)
// ---------------------------------------------------------------------------

func TestConcurrentControlNoPanic(t *testing.T) {
	host := &fakeHost{dwell: 2 * time.Millisecond}
	m := watcher.NewManager(host)
	m.Start()
	defer m.Stop()
	for i := 0; i < 8; i++ {
		id := string(rune('a' + i))
		if err := m.Add(newRunner(id, id, watcher.KindFree, "", constSchedule{10 * time.Millisecond}, true)); err != nil {
			t.Fatalf("Add: %v", err)
		}
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		id := string(rune('a' + i))
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				_ = m.RunNow(id)
				_ = m.Toggle(id)
				m.ListWatchers("")
				_, _ = m.Get(id)
				_ = m.StopWatcher(id)
			}
		}(id)
	}
	wg.Wait()
}
