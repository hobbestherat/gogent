package watcher

import (
	"context"
	"errors"
	"sync"
	"time"
)

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
	// completed successfully.
	StatusIdle Status = iota
	// StatusRunning means a fire is currently executing.
	StatusRunning
	// StatusSkipped means the most recent due fire was skipped because a
	// previous fire was still running.
	StatusSkipped
	// StatusFailed means the most recent fire returned an error.
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
	// ErrDuplicate is returned by Add when a watcher with the same id already
	// exists.
	ErrDuplicate = errors.New("watcher: duplicate id")
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
}

// Runner owns a single watcher: its immutable configuration plus the mutable
// runtime state guarded by mu. The host receives a *Runner in RunWatcherFire and
// reads its configuration through the exported accessors; all mutable state is
// reached only through Manager methods (or Runner.SetLastResult).
type Runner struct {
	// Immutable after construction.
	id        string
	name      string
	task      string
	model     string
	kind      Kind
	sessionID string
	schedule  Schedule

	mu         sync.Mutex
	enabled    bool
	status     Status
	running    bool
	lastRun    time.Time
	lastResult string
	lastError  string
	nextFire   time.Time

	done       chan struct{}      // closed to stop the active schedule loop
	cancelFire context.CancelFunc // cancels the in-flight fire, if any
}

// NewRunner builds a Runner from a Spec. Schedule must be non-nil.
func NewRunner(spec Spec) *Runner {
	return &Runner{
		id:        spec.ID,
		name:      spec.Name,
		task:      spec.Task,
		model:     spec.Model,
		kind:      spec.Kind,
		sessionID: spec.SessionID,
		schedule:  spec.Schedule,
		enabled:   spec.Enabled,
		status:    StatusIdle,
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

// WithSkipIfRunning sets whether a due fire is skipped (recorded as
// StatusSkipped) when the watcher's previous fire is still in flight. The
// default is true. Regardless of this setting the Manager never overlaps a
// watcher with itself.
func WithSkipIfRunning(skip bool) Option {
	return func(m *Manager) { m.skipIfRunning = skip }
}

// WithLogger installs an optional structured-ish log callback used for fire
// start/skip/complete/failed events. nil disables logging (the default).
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

// Add registers a watcher. If the Manager has already started and the watcher is
// enabled, its schedule loop is launched immediately. Returns ErrDuplicate if a
// watcher with the same id already exists, or ErrStopped if the Manager has been
// stopped.
func (m *Manager) Add(r *Runner) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.stopped {
		return ErrStopped
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
	m.ctx, m.cancel = context.WithCancel(context.Background())
	m.started = true
	for _, r := range m.runners {
		if r.enabled {
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

// launchLoopLocked starts a fresh schedule loop for r. Caller must hold m.mu and
// have verified the Manager is started and not stopped.
func (m *Manager) launchLoopLocked(r *Runner) {
	done := make(chan struct{})
	r.mu.Lock()
	r.done = done
	r.enabled = true
	r.mu.Unlock()
	m.wg.Add(1)
	go m.scheduleLoop(r, done)
}

// scheduleLoop arms a timer for r's next fire, waits, fires, and re-arms. It
// exits when its done channel is closed (per-watcher disable/remove) or the
// manager context is cancelled (Stop).
func (m *Manager) scheduleLoop(r *Runner, done chan struct{}) {
	defer m.wg.Done()
	for {
		now := time.Now()
		next := r.schedule.Next(now)
		r.mu.Lock()
		r.nextFire = next
		r.mu.Unlock()

		d := time.Until(next)
		if d < 0 {
			d = 0
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
	r.status = StatusRunning
	name := r.name
	r.mu.Unlock()

	m.log("watcher fire: name=%s", name)
	m.wg.Add(1)
	go m.runFire(r)
}

// runFire acquires a global concurrency slot, runs one fire to completion, and
// records the outcome. It always clears the running flag on exit.
func (m *Manager) runFire(r *Runner) {
	defer m.wg.Done()
	defer func() {
		r.mu.Lock()
		r.running = false
		r.cancelFire = nil
		r.mu.Unlock()
	}()

	// Acquire a global slot, but abort if the manager is shutting down while we
	// wait. This bounds total concurrent fires across all watchers.
	select {
	case m.sem <- struct{}{}:
	case <-m.ctx.Done():
		r.mu.Lock()
		r.status = StatusIdle
		r.mu.Unlock()
		return
	}
	defer func() { <-m.sem }()

	// The manager may have been cancelled while we waited for a slot.
	if m.ctx.Err() != nil {
		r.mu.Lock()
		r.status = StatusIdle
		r.mu.Unlock()
		return
	}

	ctx, cancel := context.WithCancel(m.ctx)
	r.mu.Lock()
	r.cancelFire = cancel
	r.lastRun = time.Now()
	r.mu.Unlock()

	err := m.host.RunWatcherFire(ctx, r)
	cancel()

	r.mu.Lock()
	if err != nil {
		r.status = StatusFailed
		r.lastError = err.Error()
	} else {
		r.status = StatusIdle
		r.lastError = ""
	}
	kind := r.kind
	name := r.name
	summary := r.lastResult
	r.mu.Unlock()

	if err != nil {
		m.log("watcher failed: name=%s error=%v", name, err)
		return
	}
	m.log("watcher complete: name=%s", name)

	// Free-running watchers announce completion through the host notifier;
	// attached watchers surface their work through the owning session instead.
	if kind == KindFree {
		body := summary
		if body == "" {
			body = "Watcher \"" + name + "\" completed."
		}
		m.host.Notify("watcher", name, body)
	}
}

// resolve looks up a watcher by id first, then by name. Caller must hold m.mu.
func (m *Manager) resolveLocked(idOrName string) (*Runner, bool) {
	if r, ok := m.runners[idOrName]; ok {
		return r, true
	}
	for _, r := range m.runners {
		if r.name == idOrName {
			return r, true
		}
	}
	return nil, false
}

// Toggle flips a watcher's enabled state. Disabling stops its schedule loop (a
// running fire finishes naturally); enabling re-arms it. Identified by id or
// name.
func (m *Manager) Toggle(idOrName string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.resolveLocked(idOrName)
	if !ok {
		return ErrNotFound
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
	r.mu.Lock()
	r.enabled = true
	r.mu.Unlock()
	if m.started {
		m.launchLoopLocked(r)
	}
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

// RunNow fires a watcher immediately, ignoring its schedule. It still respects
// the no-overlap rule: if a fire is already in flight the request is a no-op
// (recorded as skipped when skipIfRunning). Identified by id or name.
func (m *Manager) RunNow(idOrName string) error {
	m.mu.Lock()
	if m.stopped {
		m.mu.Unlock()
		return ErrStopped
	}
	r, ok := m.resolveLocked(idOrName)
	if !ok {
		m.mu.Unlock()
		return ErrNotFound
	}
	if !m.started {
		m.ctx, m.cancel = context.WithCancel(context.Background())
		m.started = true
	}
	// startFire's wg.Add happens under m.mu, serialised with Stop's stopped
	// flag, so it can never race a zero-counter Wait.
	m.startFire(r)
	m.mu.Unlock()
	return nil
}

// StopWatcher cancels a watcher's in-flight fire (if any) without stopping its
// schedule — the next fire will still be armed. Identified by id or name.
func (m *Manager) StopWatcher(idOrName string) error {
	m.mu.Lock()
	r, ok := m.resolveLocked(idOrName)
	m.mu.Unlock()
	if !ok {
		return ErrNotFound
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
// asynchronously; Stop still waits for it. Identified by id or name.
func (m *Manager) Remove(idOrName string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.resolveLocked(idOrName)
	if !ok {
		return ErrNotFound
	}
	r.mu.Lock()
	if r.done != nil {
		close(r.done)
		r.done = nil
	}
	cancel := r.cancelFire
	r.enabled = false
	r.mu.Unlock()
	if cancel != nil {
		cancel()
	}
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
		r.mu.Lock()
		if r.done != nil {
			close(r.done)
			r.done = nil
		}
		cancel := r.cancelFire
		r.enabled = false
		r.mu.Unlock()
		if cancel != nil {
			cancel()
		}
		delete(m.runners, id)
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

// Get returns a snapshot of a single watcher by id or name.
func (m *Manager) Get(idOrName string) (WatcherInfo, bool) {
	m.mu.Lock()
	r, ok := m.resolveLocked(idOrName)
	m.mu.Unlock()
	if !ok {
		return WatcherInfo{}, false
	}
	return r.snapshot(), true
}
