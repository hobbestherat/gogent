package watcher

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// minFireDelay is the floor the manager applies to every armed delay. A
// Schedule may legitimately return an instant only nanoseconds in the future
// (for example IntervalSchedule clamps a non-positive interval to 1ns); without
// a floor a misconfigured schedule would spin its goroutine, re-arming a
// zero-length timer thousands of times a second. The floor keeps a degenerate
// schedule cheap while staying well below any sane real cadence.
const minFireDelay = time.Millisecond

// Kind distinguishes the two watcher lifecycles.
type Kind int

const (
	// KindFree is a free-running watcher: a process-global resource that runs
	// into its own dedicated session and delivers its result out of band
	// (notification on completion). Not bound to any user session.
	KindFree Kind = iota
	// KindAttached is a session-scoped watcher: it reports into an owning user
	// session and dies with it.
	KindAttached
)

// Status is the last-known state of a Runner.
type Status int

const (
	// StatusIdle means no fire is in flight and the last fire (if any)
	// completed successfully or was intentionally cancelled.
	StatusIdle Status = iota
	// StatusRunning means a fire is currently executing.
	StatusRunning
	// StatusSkipped means the most recent due fire was skipped because a
	// previous fire was still running.
	StatusSkipped
	// StatusFailed means the most recent fire returned an error (a cancelled
	// fire is not a failure — see runFire).
	StatusFailed
)

// String renders a Status as the lowercase token used across the UI and HTTP
// surfaces ("idle", "running", "skipped", "failed").
func (s Status) String() string {
	switch s {
	case StatusRunning:
		return "running"
	case StatusSkipped:
		return "skipped"
	case StatusFailed:
		return "failed"
	default:
		return "idle"
	}
}

// Errors returned by Manager control methods.
var (
	// ErrNotFound is returned when no watcher matches the supplied id or name.
	ErrNotFound = errors.New("watcher: not found")
	// ErrAmbiguous is returned when a name matches more than one watcher.
	// Resolution by id is always unambiguous; resolution by name errors rather
	// than guess.
	ErrAmbiguous = errors.New("watcher: ambiguous name")
	// ErrDuplicate is returned by Add when a watcher with the same id already
	// exists.
	ErrDuplicate = errors.New("watcher: duplicate id")
	// ErrNilSchedule is returned by Add when the watcher has no Schedule.
	ErrNilSchedule = errors.New("watcher: nil schedule")
	// ErrStopped is returned by control methods after the Manager has been
	// stopped.
	ErrStopped = errors.New("watcher: manager stopped")
)

// WatcherHost is the minimal surface the Manager needs from the host (gogent
// implements it in a later phase). Keeping it small lets the scheduling engine
// be unit-tested with a fake host and keeps this package free of dependencies on
// the rest of the system.
type WatcherHost interface {
	// RunWatcherFire executes exactly one fire of the watcher's task to
	// completion, blocking until the fire finishes. It must respect ctx: when
	// ctx is cancelled (manager shutdown or a per-watcher stop) the fire must
	// abort promptly and return ctx.Err() (or another error). The Runner is
	// passed so the host can read its configuration (ID/Name/Task/Model/Kind/
	// SessionID) and report a one-line result summary via Runner.SetLastResult.
	// A panic in RunWatcherFire is recovered by the manager and recorded as a
	// failed fire, so one misbehaving host cannot crash the process.
	RunWatcherFire(ctx context.Context, r *Runner) error
	// Notify delivers a completion notification for a free-running watcher.
	// reason is a stable token ("watcher"); title and body are human-facing.
	Notify(reason, title, body string)
}

// Spec is the immutable configuration used to construct a Runner via NewRunner.
type Spec struct {
	ID        string
	Name      string
	Task      string
	Model     string
	Kind      Kind
	SessionID string // owning/target session for KindAttached; "" for KindFree
	Schedule  Schedule
	Enabled   bool
	// SuppressNotify opts a free-running watcher out of the completion
	// notification the manager otherwise emits on every successful free-running
	// fire. The zero value (false) keeps the default-on behaviour, so a watcher
	// configured on_complete.notify=false sets this to true. It has no effect on
	// attached watchers, which never notify.
	SuppressNotify bool
}

// Runner owns a single watcher: its immutable configuration plus the mutable
// runtime state guarded by mu. The host receives a *Runner in RunWatcherFire and
// reads its configuration through the exported accessors; all mutable state is
// reached only through Manager methods (or Runner.SetLastResult).
type Runner struct {
	// Immutable after construction.
	id             string
	name           string
	task           string
	model          string
	kind           Kind
	sessionID      string
	schedule       Schedule
	suppressNotify bool

	mu         sync.Mutex
	enabled    bool
	status     Status
	running    bool
	removed    bool // dropped from the manager; suppresses a late completion notify
	lastRun    time.Time
	lastResult string
	lastError  string
	nextFire   time.Time

	done       chan struct{}      // closed to stop the active schedule loop
	cancelFire context.CancelFunc // cancels the in-flight fire, if any
}

// NewRunner builds a Runner from a Spec. Schedule should be non-nil; a Runner
// with a nil Schedule is rejected by Manager.Add with ErrNilSchedule.
func NewRunner(spec Spec) *Runner {
	return &Runner{
		id:             spec.ID,
		name:           spec.Name,
		task:           spec.Task,
		model:          spec.Model,
		kind:           spec.Kind,
		sessionID:      spec.SessionID,
		schedule:       spec.Schedule,
		suppressNotify: spec.SuppressNotify,
		enabled:        spec.Enabled,
		status:         StatusIdle,
	}
}

// ID returns the stable unique identifier.
func (r *Runner) ID() string { return r.id }

// Name returns the human-friendly display label.
func (r *Runner) Name() string { return r.name }

// Task returns the configured task prompt.
func (r *Runner) Task() string { return r.task }

// Model returns the configured model name (empty = host default).
func (r *Runner) Model() string { return r.model }

// Kind reports whether the watcher is free-running or attached.
func (r *Runner) Kind() Kind { return r.kind }

// SessionID returns the owning/target session id (empty for free-running).
func (r *Runner) SessionID() string { return r.sessionID }

// SetLastResult records a one-line summary of the most recent fire's outcome.
// The host may call it from within RunWatcherFire.
func (r *Runner) SetLastResult(summary string) {
	r.mu.Lock()
	r.lastResult = summary
	r.mu.Unlock()
}

// WatcherInfo is a snapshot of a Runner's state for listing/reporting. It is a
// value type, safe to hand to other layers without further locking.
type WatcherInfo struct {
	ID            string
	Name          string
	Kind          Kind
	TargetSession string // session id for attached watchers; "" for free-running
	Enabled       bool
	Status        Status
	LastRun       time.Time
	LastResult    string
	LastError     string
	NextFire      time.Time
}

func (r *Runner) snapshot() WatcherInfo {
	r.mu.Lock()
	defer r.mu.Unlock()
	target := ""
	if r.kind == KindAttached {
		target = r.sessionID
	}
	return WatcherInfo{
		ID:            r.id,
		Name:          r.name,
		Kind:          r.kind,
		TargetSession: target,
		Enabled:       r.enabled,
		Status:        r.status,
		LastRun:       r.lastRun,
		LastResult:    r.lastResult,
		LastError:     r.lastError,
		NextFire:      r.nextFire,
	}
}

// Manager owns a set of Runners and drives their schedules. It is safe for
// concurrent use. Lock order is always m.mu before any Runner.mu.
type Manager struct {
	host          WatcherHost
	maxConcurrent int
	skipIfRunning bool
	logf          func(format string, args ...any)

	sem chan struct{} // global concurrency cap across all watcher fires

	mu      sync.Mutex
	runners map[string]*Runner
	started bool
	stopped bool
	ctx     context.Context
	cancel  context.CancelFunc
	wg      sync.WaitGroup // schedule loops + in-flight fires
}

// Option configures a Manager.
type Option func(*Manager)

// WithMaxConcurrent bounds the total number of watcher fires that may run
// concurrently across all watchers. Values < 1 are ignored (default 4).
func WithMaxConcurrent(n int) Option {
	return func(m *Manager) {
		if n >= 1 {
			m.maxConcurrent = n
		}
	}
}

// WithSkipIfRunning sets whether a due fire is recorded as StatusSkipped when
// the watcher's previous fire is still in flight. The default is true.
// Regardless of this setting the Manager never overlaps a watcher with itself —
// a due fire that arrives while a previous one runs is always dropped; this flag
// only governs whether the drop is surfaced as StatusSkipped.
func WithSkipIfRunning(skip bool) Option {
	return func(m *Manager) { m.skipIfRunning = skip }
}

// WithLogger installs an optional log callback used for fire
// start/skip/complete/failed/panic events. nil disables logging (the default).
func WithLogger(fn func(format string, args ...any)) Option {
	return func(m *Manager) { m.logf = fn }
}

// NewManager creates a Manager bound to host. The Manager is created stopped;
// call Start to begin driving schedules.
func NewManager(host WatcherHost, opts ...Option) *Manager {
	m := &Manager{
		host:          host,
		maxConcurrent: 4,
		skipIfRunning: true,
		runners:       make(map[string]*Runner),
	}
	for _, opt := range opts {
		opt(m)
	}
	m.sem = make(chan struct{}, m.maxConcurrent)
	return m
}

func (m *Manager) log(format string, args ...any) {
	if m.logf != nil {
		m.logf(format, args...)
	}
}

// ensureCtxLocked lazily creates the manager context. It is idempotent and
// independent of the started flag, so a RunNow before Start can fire without
// marking the manager started (which would otherwise short-circuit Start and
// leave the schedules un-armed). Caller must hold m.mu.
func (m *Manager) ensureCtxLocked() {
	if m.ctx == nil {
		m.ctx, m.cancel = context.WithCancel(context.Background())
	}
}

// Add registers a watcher. If the Manager has already started and the watcher is
// enabled, its schedule loop is launched immediately. Returns ErrDuplicate if a
// watcher with the same id already exists, ErrNilSchedule if it has no schedule,
// or ErrStopped if the Manager has been stopped.
func (m *Manager) Add(r *Runner) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.stopped {
		return ErrStopped
	}
	if r.schedule == nil {
		return ErrNilSchedule
	}
	if _, ok := m.runners[r.id]; ok {
		return ErrDuplicate
	}
	m.runners[r.id] = r
	if m.started && r.enabled {
		m.launchLoopLocked(r)
	}
	return nil
}

// Start begins driving schedules: every enabled watcher gets a long-lived
// goroutine that fires it on its cadence. Start is idempotent. There is no
// catch-up burst — the first fire of each watcher is one interval / the next
// daily slot after Start, never immediately.
func (m *Manager) Start() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.started || m.stopped {
		return
	}
	m.ensureCtxLocked()
	m.started = true
	for _, r := range m.runners {
		r.mu.Lock()
		enabled := r.enabled
		hasLoop := r.done != nil
		r.mu.Unlock()
		if enabled && !hasLoop {
			m.launchLoopLocked(r)
		}
	}
}

// Stop tears everything down: it stops every schedule loop, cancels every
// in-flight fire (via context), and blocks until all goroutines have exited. The
// Manager cannot be restarted after Stop.
func (m *Manager) Stop() {
	m.mu.Lock()
	if m.stopped {
		m.mu.Unlock()
		return
	}
	m.stopped = true
	if m.cancel != nil {
		m.cancel()
	}
	m.mu.Unlock()
	m.wg.Wait()
}

// launchLoopLocked starts a fresh schedule loop for r, unless one is already
// running. Caller must hold m.mu.
func (m *Manager) launchLoopLocked(r *Runner) {
	r.mu.Lock()
	if r.done != nil {
		r.mu.Unlock()
		return
	}
	done := make(chan struct{})
	r.done = done
	r.enabled = true
	r.mu.Unlock()
	m.wg.Add(1)
	go m.scheduleLoop(r, done)
}

// scheduleLoop arms a timer for r's next fire, waits, fires, and re-arms. It
// exits when its done channel is closed (per-watcher disable/remove) or the
// manager context is cancelled (Stop). A panic in the schedule (e.g. a custom
// Schedule.Next) is recovered so it cannot crash the process.
func (m *Manager) scheduleLoop(r *Runner, done chan struct{}) {
	defer m.wg.Done()
	defer func() {
		if rec := recover(); rec != nil {
			r.mu.Lock()
			r.status = StatusFailed
			r.lastError = fmt.Sprintf("watcher schedule panicked: %v", rec)
			r.mu.Unlock()
			m.log("watcher schedule panic: name=%s recovered=%v", r.name, rec)
		}
	}()
	if r.schedule == nil {
		r.mu.Lock()
		r.status = StatusFailed
		r.lastError = "watcher has no schedule"
		r.mu.Unlock()
		return
	}
	for {
		now := time.Now()
		next := r.schedule.Next(now)
		r.mu.Lock()
		r.nextFire = next
		r.mu.Unlock()

		// Delay relative to the now we handed Next, floored to avoid spinning
		// on a non-advancing schedule.
		d := next.Sub(now)
		if d < minFireDelay {
			d = minFireDelay
		}
		timer := time.NewTimer(d)
		select {
		case <-m.ctx.Done():
			timer.Stop()
			return
		case <-done:
			timer.Stop()
			return
		case <-timer.C:
		}
		m.startFire(r)
	}
}

// startFire begins a fire for r unless one is already running (watchers never
// overlap themselves). A due fire that arrives while a previous fire is still in
// flight is dropped; when skipIfRunning is set it is recorded as StatusSkipped.
func (m *Manager) startFire(r *Runner) {
	r.mu.Lock()
	if r.running {
		if m.skipIfRunning {
			r.status = StatusSkipped
		}
		name := r.name
		r.mu.Unlock()
		m.log("watcher skip: previous fire still running name=%s", name)
		return
	}
	r.running = true
	name := r.name
	r.mu.Unlock()

	m.log("watcher fire: name=%s", name)
	m.wg.Add(1)
	go m.runFire(r)
}

// runFire acquires a global concurrency slot, runs one fire to completion, and
// records the outcome. It always clears the running flag on exit and recovers
// any panic from the host as a failed fire.
func (m *Manager) runFire(r *Runner) {
	defer m.wg.Done()
	defer func() {
		rec := recover()
		r.mu.Lock()
		r.running = false
		r.cancelFire = nil
		if rec != nil {
			r.status = StatusFailed
			r.lastError = fmt.Sprintf("watcher fire panicked: %v", rec)
		}
		r.mu.Unlock()
		if rec != nil {
			m.log("watcher panic: name=%s recovered=%v", r.name, rec)
		}
	}()

	// Acquire a global slot, bounding total concurrent fires across all
	// watchers. Abort if the manager is shutting down while we wait.
	select {
	case m.sem <- struct{}{}:
	case <-m.ctx.Done():
		return
	}
	defer func() { <-m.sem }()

	// The manager may have been cancelled while we waited for a slot.
	if m.ctx.Err() != nil {
		return
	}

	ctx, cancel := context.WithCancel(m.ctx)
	r.mu.Lock()
	r.cancelFire = cancel
	r.lastRun = time.Now()
	r.status = StatusRunning
	name := r.name
	kind := r.kind
	r.mu.Unlock()

	err := m.host.RunWatcherFire(ctx, r)
	// A cancelled fire is an intentional stop (manager shutdown, StopWatcher,
	// Remove), not a failure — detect it before our own cancel() below.
	cancelled := ctx.Err() != nil
	cancel()

	r.mu.Lock()
	switch {
	case cancelled:
		r.status = StatusIdle
	case err != nil:
		r.status = StatusFailed
		r.lastError = err.Error()
	default:
		r.status = StatusIdle
		r.lastError = ""
	}
	summary := r.lastResult
	removed := r.removed
	suppressNotify := r.suppressNotify
	r.mu.Unlock()

	switch {
	case cancelled:
		m.log("watcher cancelled: name=%s", name)
	case err != nil:
		m.log("watcher failed: name=%s error=%v", name, err)
	default:
		m.log("watcher complete: name=%s", name)
	}

	// Free-running watchers announce successful completion through the host
	// notifier; attached watchers surface their work through the owning session
	// instead. A cancelled, failed, already-removed, or notify-suppressed
	// (on_complete.notify=false) watcher never notifies.
	if !cancelled && err == nil && kind == KindFree && !removed && !suppressNotify {
		body := summary
		if body == "" {
			body = "Watcher \"" + name + "\" completed."
		}
		m.host.Notify("watcher", name, body)
	}
}

// resolveLocked looks up a watcher by exact id first (always unambiguous), then
// by name. A name matching more than one watcher returns ErrAmbiguous; no match
// returns ErrNotFound. Caller must hold m.mu.
func (m *Manager) resolveLocked(idOrName string) (*Runner, error) {
	if r, ok := m.runners[idOrName]; ok {
		return r, nil
	}
	var found *Runner
	count := 0
	for _, r := range m.runners {
		if r.name == idOrName {
			found = r
			count++
		}
	}
	switch count {
	case 0:
		return nil, ErrNotFound
	case 1:
		return found, nil
	default:
		return nil, ErrAmbiguous
	}
}

// Toggle flips a watcher's enabled state. Disabling stops its schedule loop (a
// running fire finishes naturally); enabling re-arms it. Identified by id or
// name.
func (m *Manager) Toggle(idOrName string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, err := m.resolveLocked(idOrName)
	if err != nil {
		return err
	}
	r.mu.Lock()
	enabled := r.enabled
	r.mu.Unlock()
	if enabled {
		m.disableLocked(r)
		return nil
	}
	if m.stopped {
		return ErrStopped
	}
	if m.started {
		m.launchLoopLocked(r) // also sets enabled = true
		return nil
	}
	r.mu.Lock()
	r.enabled = true
	r.mu.Unlock()
	return nil
}

// disableLocked stops r's schedule loop and marks it disabled. A running fire is
// left to finish. Caller must hold m.mu.
func (m *Manager) disableLocked(r *Runner) {
	r.mu.Lock()
	r.enabled = false
	if r.done != nil {
		close(r.done)
		r.done = nil
	}
	r.mu.Unlock()
}

// RunNow fires a watcher immediately, ignoring its schedule and its enabled
// state (a disabled watcher can still be triggered manually). It still respects
// the no-overlap rule: if a fire is already in flight the request is a no-op
// (recorded as skipped when skipIfRunning). Identified by id or name.
func (m *Manager) RunNow(idOrName string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.stopped {
		return ErrStopped
	}
	r, err := m.resolveLocked(idOrName)
	if err != nil {
		return err
	}
	m.ensureCtxLocked()
	// startFire's wg.Add happens under m.mu, serialised with Stop's stopped
	// flag, so it can never race a zero-counter Wait.
	m.startFire(r)
	return nil
}

// StopWatcher cancels a watcher's in-flight fire (if any) without stopping its
// schedule — the next fire will still be armed. Identified by id or name.
func (m *Manager) StopWatcher(idOrName string) error {
	m.mu.Lock()
	r, err := m.resolveLocked(idOrName)
	m.mu.Unlock()
	if err != nil {
		return err
	}
	r.mu.Lock()
	cancel := r.cancelFire
	r.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	return nil
}

// Remove stops a watcher's schedule loop, cancels any in-flight fire, and drops
// it from the Manager. The in-flight fire (if any) is cancelled and reaped
// asynchronously; Stop still waits for it, and it will not emit a late
// completion notification. Identified by id or name.
func (m *Manager) Remove(idOrName string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, err := m.resolveLocked(idOrName)
	if err != nil {
		return err
	}
	m.teardownLocked(r)
	delete(m.runners, r.id)
	return nil
}

// RemoveAttachedForSession removes every attached watcher owned by sessionID
// (stopping its loop and cancelling any in-flight fire). Free-running watchers
// are unaffected. Called when a session closes.
func (m *Manager) RemoveAttachedForSession(sessionID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, r := range m.runners {
		if r.kind != KindAttached || r.sessionID != sessionID {
			continue
		}
		m.teardownLocked(r)
		delete(m.runners, id)
	}
}

// teardownLocked stops r's loop, cancels its in-flight fire, and marks it
// removed so a fire completing concurrently does not notify. Caller must hold
// m.mu.
func (m *Manager) teardownLocked(r *Runner) {
	r.mu.Lock()
	r.removed = true
	r.enabled = false
	if r.done != nil {
		close(r.done)
		r.done = nil
	}
	cancel := r.cancelFire
	r.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// ListWatchers returns a snapshot of the watchers visible to sessionID: every
// free-running watcher plus the attached watchers owned by sessionID. An empty
// sessionID ("") returns free-running watchers only. Other sessions' attached
// watchers are never returned. The result order is unspecified.
func (m *Manager) ListWatchers(sessionID string) []WatcherInfo {
	m.mu.Lock()
	runners := make([]*Runner, 0, len(m.runners))
	for _, r := range m.runners {
		switch r.kind {
		case KindFree:
			runners = append(runners, r)
		case KindAttached:
			if sessionID != "" && r.sessionID == sessionID {
				runners = append(runners, r)
			}
		}
	}
	m.mu.Unlock()

	out := make([]WatcherInfo, 0, len(runners))
	for _, r := range runners {
		out = append(out, r.snapshot())
	}
	return out
}

// Get returns a snapshot of a single watcher by id or name. It returns ok=false
// when the watcher is not found or a name is ambiguous. Callers that need to tell
// those two cases apart (to report "ambiguous; use the id" rather than "not
// found") should use Resolve instead.
func (m *Manager) Get(idOrName string) (WatcherInfo, bool) {
	info, err := m.Resolve(idOrName)
	if err != nil {
		return WatcherInfo{}, false
	}
	return info, true
}

// Resolve looks up a single watcher by id or name and returns its snapshot, or
// the resolution error: ErrNotFound when nothing matches, or ErrAmbiguous when a
// name matches more than one watcher (resolution by id is always unambiguous).
// It is the error-surfacing counterpart of Get, so control surfaces can tell the
// agent "ambiguous; use the id" instead of misreporting a duplicate name as "not
// found".
func (m *Manager) Resolve(idOrName string) (WatcherInfo, error) {
	m.mu.Lock()
	r, err := m.resolveLocked(idOrName)
	m.mu.Unlock()
	if err != nil {
		return WatcherInfo{}, err
	}
	return r.snapshot(), nil
}
