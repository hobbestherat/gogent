package gogent

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"gogent/internal/config"
	"gogent/internal/notify"
	"gogent/internal/permission"
	"gogent/internal/watcher"
)

// watcherSessionPrefix names the dedicated, persistent session a free-running
// watcher fires into ("watcher:<name>"). It is an ordinary non-ephemeral session
// (it is neither "default" nor marked ephemeral), so it is persisted to
// ~/.gogent/sessions and restored by RestoreSessions like any session — its
// transcript accumulates across fires for live and past-run inspection.
const watcherSessionPrefix = "watcher:"

// StartWatchers starts the FREE-RUNNING watchers declared in
// ~/.gogent/watchers.json. It mirrors StartMCPServers: it is permission-gated
// (ActionWatcher) and feature-gated (Experimental.Watchers), so a config synced
// from elsewhere cannot silently start recurring agent work, and an existing
// config without the experimental key sees zero behaviour change.
//
// It is a no-op unless Experimental.Watchers is on. For each enabled watcher it
// gates the launch (a denied/invalid watcher is skipped with a warning so one
// bad entry never blocks the others), parses its schedule, and registers a
// free-running Runner. The manager is then started: there is no catch-up burst —
// each watcher's first fire is one interval / the next daily slot after Start.
//
// It should be called once, after the permission prompter is installed (so the
// gate can prompt rather than defaulting to deny), i.e. after StartMCPServers.
func (g *Gogent) StartWatchers() {
	g.mu.RLock()
	cfg := g.config
	g.mu.RUnlock()
	if cfg == nil || !cfg.Experimental.Watchers {
		return
	}
	// Idempotent: a manager already exists, so a second call must not orphan its
	// goroutines by overwriting the field (mirrors Manager.Start's started guard).
	g.mu.RLock()
	already := g.watchers != nil
	g.mu.RUnlock()
	if already {
		return
	}

	store := g.LoadWatchers()
	wcfg := cfg.Watchers
	mgr := watcher.NewManager(g,
		watcher.WithMaxConcurrent(wcfg.MaxConcurrentOrDefault()),
		watcher.WithSkipIfRunning(wcfg.SkipIfRunningOrDefault()),
		watcher.WithLogger(func(format string, args ...any) { g.logger().Infof(format, args...) }),
	)

	for _, wc := range store.Items {
		if !wc.Enabled || strings.TrimSpace(wc.Name) == "" {
			continue
		}
		// Gate the launch. The watcher name is the resource so an "always" grant is
		// scoped to a single watcher (same safety property as MCP servers).
		if g.permissions != nil {
			if err := g.permissions.CheckWithDetail(permission.ActionWatcher, wc.Name, watcherLaunchDetail(wc)); err != nil {
				g.logger().Warn("watcher not started", "name", wc.Name, "error", err)
				continue
			}
		}
		sched, err := wc.Schedule.Schedule()
		if err != nil {
			g.logger().Warn("watcher invalid schedule", "name", wc.Name, "error", err)
			continue
		}
		runner := watcher.NewRunner(watcher.Spec{
			ID:             wc.ID,
			Name:           wc.Name,
			Task:           wc.Task,
			Model:          wc.Model,
			Kind:           watcher.KindFree,
			Schedule:       sched,
			ScheduleDesc:   scheduleSummary(wc.Schedule),
			Enabled:        true,
			SuppressNotify: suppressWatcherNotify(wc.Output),
		})
		if err := mgr.Add(runner); err != nil {
			g.logger().Warn("watcher add failed", "name", wc.Name, "error", err)
		}
	}

	mgr.Start()

	g.mu.Lock()
	g.watchers = mgr
	// Snapshot any attached watchers already recorded by an earlier session restore
	// (RestoreSessions can run before StartWatchers — the manager was nil then, so
	// OnSessionRestored only recorded the configs). Register them now.
	pending := make(map[string][]config.WatcherConfig, len(g.attachedWatchers))
	for sid, wcs := range g.attachedWatchers {
		pending[sid] = append([]config.WatcherConfig(nil), wcs...)
	}
	g.mu.Unlock()

	for sid, wcs := range pending {
		for _, wc := range wcs {
			g.registerAttachedRunner(mgr, wc, sid)
		}
	}
}

// StopWatchers tears the watcher engine down cleanly: it stops every schedule
// loop and cancels every in-flight fire, blocking until all watcher goroutines
// have exited. It is safe to call when no watchers were started. It mirrors
// CloseMCPServers and should run before it in the shutdown sequence.
func (g *Gogent) StopWatchers() {
	g.mu.Lock()
	mgr := g.watchers
	g.watchers = nil
	g.mu.Unlock()
	if mgr != nil {
		mgr.Stop()
	}
}

// ListWatchers returns a snapshot of the watchers visible to sessionID (every
// free-running watcher plus that session's attached watchers; an empty
// sessionID returns free-running watchers only). It returns nil when no manager
// is running. It is the read accessor later phases (tools/HTTP/TUI) build on.
func (g *Gogent) ListWatchers(sessionID string) []watcher.WatcherInfo {
	g.mu.RLock()
	mgr := g.watchers
	g.mu.RUnlock()
	if mgr == nil {
		return nil
	}
	return mgr.ListWatchers(sessionID)
}

// GetWatcher resolves a single watcher by id or name across every registered
// watcher — free-running and attached, regardless of owning session — and
// returns its current snapshot (issue #329 Phase 5). It is the read accessor the
// HTTP GET /watchers/:id endpoint builds on, sharing the same resolver semantics
// as the mutating wrappers (DeleteWatcher/ToggleWatcher/…): it returns
// watcher.ErrNotFound when nothing matches and watcher.ErrAmbiguous when a name
// matches more than one (ids are unique, so id lookups never collide). Like
// ListWatchers it is a read and is not ActionWatcher-gated. It returns an error
// when the engine is not running.
func (g *Gogent) GetWatcher(idOrName string) (watcher.WatcherInfo, error) {
	g.mu.RLock()
	mgr := g.watchers
	g.mu.RUnlock()
	if mgr == nil {
		return watcher.WatcherInfo{}, fmt.Errorf("watcher engine is not running")
	}
	info, err := mgr.Resolve(idOrName)
	if err != nil {
		return watcher.WatcherInfo{}, fmt.Errorf("resolve watcher: %w", err)
	}
	return info, nil
}

// attachedWatchersFor returns a copy of the attached (session-scoped) watcher
// configs owned by sessionID, for the SessionStore to serialize into the
// session's index (issue #329 Phase 3). It returns nil when the session has no
// attached watchers. The copy keeps the caller from aliasing the manager's
// internal slice.
func (g *Gogent) attachedWatchersFor(sessionID string) []config.WatcherConfig {
	g.mu.RLock()
	defer g.mu.RUnlock()
	wcs := g.attachedWatchers[sessionID]
	if len(wcs) == 0 {
		return nil
	}
	return append([]config.WatcherConfig(nil), wcs...)
}

// OnSessionClosed tears down a session's attached watchers when the session is
// removed (issue #329 Phase 3): it drops their stored configs and stops their
// schedule loops + cancels any in-flight fire (manager.RemoveAttachedForSession).
// Free-running watchers are global and unaffected. It is wired into RemoveSession.
func (g *Gogent) OnSessionClosed(sessionID string) {
	g.mu.Lock()
	delete(g.attachedWatchers, sessionID)
	mgr := g.watchers
	g.mu.Unlock()
	if mgr != nil {
		mgr.RemoveAttachedForSession(sessionID)
	}
}

// OnSessionRestored re-registers a restored session's attached watchers so they
// resume their schedules with the session (issue #329 Phase 3). It records their
// configs (so a later save re-serializes them and a later StartWatchers can pick
// them up) and, when the manager is already running, registers a live Runner for
// each. It is wired into adoptLoaded (startup RestoreSessions + on-demand
// ContinueSession). A nil/empty watcher list is a no-op.
func (g *Gogent) OnSessionRestored(sessionID string, watchers []config.WatcherConfig) {
	if len(watchers) == 0 {
		return
	}
	g.mu.Lock()
	if g.attachedWatchers == nil {
		g.attachedWatchers = make(map[string][]config.WatcherConfig)
	}
	g.attachedWatchers[sessionID] = append([]config.WatcherConfig(nil), watchers...)
	mgr := g.watchers
	g.mu.Unlock()
	if mgr == nil {
		// The watcher engine is not running yet (restore ran before StartWatchers).
		// StartWatchers registers the recorded set once it builds the manager.
		return
	}
	for _, wc := range watchers {
		g.registerAttachedRunner(mgr, wc, sessionID)
	}
}

// registerAttachedRunner builds and registers a KindAttached Runner for wc owning
// sessionID. A parse/duplicate failure is logged and skipped so one bad attached
// watcher never blocks the rest of a session's restore. A duplicate id (already
// registered, e.g. a restore racing StartWatchers) is silently tolerated.
func (g *Gogent) registerAttachedRunner(mgr *watcher.Manager, wc config.WatcherConfig, sessionID string) {
	sched, err := wc.Schedule.Schedule()
	if err != nil {
		g.logger().Warn("attached watcher invalid schedule", "name", wc.Name, "error", err)
		return
	}
	runner := watcher.NewRunner(watcher.Spec{
		ID:           wc.ID,
		Name:         wc.Name,
		Task:         wc.Task,
		Model:        wc.Model,
		Kind:         watcher.KindAttached,
		SessionID:    sessionID,
		Schedule:     sched,
		ScheduleDesc: scheduleSummary(wc.Schedule),
		Enabled:      wc.Enabled,
	})
	if err := mgr.Add(runner); err != nil && !errors.Is(err, watcher.ErrDuplicate) {
		g.logger().Warn("attached watcher not registered", "name", wc.Name, "error", err)
	}
}

// CreateWatcher creates a watcher and decides its kind from cfg.ReportToSession
// (issue #329 Phase 3): a non-nil session id makes it ATTACHED (session-scoped,
// stored with that session, not in watchers.json); nil makes it FREE-RUNNING
// (global, persisted to ~/.gogent/watchers.json). It generates the id when empty,
// gates the launch through ActionWatcher (scoped by name), registers a Runner on
// the manager, and persists. It returns the created watcher's info.
//
// sessionID is the calling session; it is only used as a sanity fallback for an
// attached watcher whose ReportToSession is the empty string (the create_watcher
// tool already defaults ReportToSession to the calling session, so this path is
// defensive). It returns an error when the engine is not running, the schedule is
// invalid, the permission is denied, or registration fails.
func (g *Gogent) CreateWatcher(cfg config.WatcherConfig, sessionID string) (watcher.WatcherInfo, error) {
	g.mu.RLock()
	mgr := g.watchers
	g.mu.RUnlock()
	if mgr == nil {
		return watcher.WatcherInfo{}, fmt.Errorf("watcher engine is not running")
	}
	if strings.TrimSpace(cfg.Name) == "" {
		return watcher.WatcherInfo{}, fmt.Errorf("watcher name is required")
	}
	if cfg.ID == "" {
		cfg.ID = config.GenerateWatcherID()
	}
	sched, err := cfg.Schedule.Schedule()
	if err != nil {
		return watcher.WatcherInfo{}, fmt.Errorf("invalid watcher schedule: %w", err)
	}
	if g.permissions != nil {
		if err := g.permissions.CheckWithDetail(permission.ActionWatcher, cfg.Name, watcherLaunchDetail(cfg)); err != nil {
			return watcher.WatcherInfo{}, fmt.Errorf("permission check: %w", err)
		}
	}

	attached := cfg.ReportToSession != nil
	if attached {
		target := strings.TrimSpace(*cfg.ReportToSession)
		if target == "" {
			target = sessionID
		}
		if target == "" {
			return watcher.WatcherInfo{}, fmt.Errorf("attached watcher needs an owning session")
		}
		// The target must be a live session. An attached watcher is serialized with
		// its session's index, so one pointed at a session that is not in memory
		// could never be persisted (persistSession is a no-op for an unknown
		// session) and would silently vanish on restart. Reject it up front rather
		// than register a watcher that cannot survive (issue #329).
		if g.GetUserSession(target) == nil {
			return watcher.WatcherInfo{}, fmt.Errorf("attached watcher target session %q is not active", target)
		}
		// Normalise the stored config so the persisted target matches the registered
		// runner (the tool may have passed an empty-string sentinel).
		t := target
		cfg.ReportToSession = &t
		runner := watcher.NewRunner(watcher.Spec{
			ID:           cfg.ID,
			Name:         cfg.Name,
			Task:         cfg.Task,
			Model:        cfg.Model,
			Kind:         watcher.KindAttached,
			SessionID:    target,
			Schedule:     sched,
			ScheduleDesc: scheduleSummary(cfg.Schedule),
			Enabled:      cfg.Enabled,
		})
		if err := mgr.Add(runner); err != nil {
			return watcher.WatcherInfo{}, fmt.Errorf("register attached watcher: %w", err)
		}
		g.mu.Lock()
		if g.attachedWatchers == nil {
			g.attachedWatchers = make(map[string][]config.WatcherConfig)
		}
		g.attachedWatchers[target] = append(g.attachedWatchers[target], cfg)
		g.mu.Unlock()
		// Persist the owning session so the new attached watcher lands in its index.
		g.persistSession(target)
	} else {
		runner := watcher.NewRunner(watcher.Spec{
			ID:             cfg.ID,
			Name:           cfg.Name,
			Task:           cfg.Task,
			Model:          cfg.Model,
			Kind:           watcher.KindFree,
			Schedule:       sched,
			ScheduleDesc:   scheduleSummary(cfg.Schedule),
			Enabled:        cfg.Enabled,
			SuppressNotify: suppressWatcherNotify(cfg.Output),
		})
		if err := mgr.Add(runner); err != nil {
			return watcher.WatcherInfo{}, fmt.Errorf("register free-running watcher: %w", err)
		}
		if err := g.persistFreeWatcherUpsert(cfg); err != nil {
			g.warnf("failed to persist free-running watcher %q: %v", cfg.Name, err)
		}
	}

	info, _ := mgr.Get(cfg.ID)
	return info, nil
}

// UpdateWatcher applies a sparse patch to an existing watcher identified by
// patch.ID or patch.Name (issue #329 Phase 3): non-empty Task/Model and a
// non-empty Schedule replace the stored values; the kind/owning-session is left
// unchanged. It re-registers the Runner (a watcher carries an immutable schedule,
// so the runner is rebuilt under the same id) and re-persists. sessionID is the
// calling session, used only to scope the lookup error message.
func (g *Gogent) UpdateWatcher(patch config.WatcherConfig, sessionID string) (watcher.WatcherInfo, error) {
	g.mu.RLock()
	mgr := g.watchers
	g.mu.RUnlock()
	if mgr == nil {
		return watcher.WatcherInfo{}, fmt.Errorf("watcher engine is not running")
	}
	idOrName := patch.ID
	if idOrName == "" {
		idOrName = patch.Name
	}
	info, err := mgr.Resolve(idOrName)
	if err != nil {
		return watcher.WatcherInfo{}, fmt.Errorf("resolve watcher: %w", err)
	}
	if g.permissions != nil {
		if err := g.permissions.CheckWithDetail(permission.ActionWatcher, info.Name, "update watcher "+info.Name); err != nil {
			return watcher.WatcherInfo{}, fmt.Errorf("permission check: %w", err)
		}
	}

	cur, kind, owner, found := g.lookupWatcherConfig(info.ID)
	if !found {
		return watcher.WatcherInfo{}, watcher.ErrNotFound
	}
	// Apply the patch over the stored config.
	if strings.TrimSpace(patch.Name) != "" {
		cur.Name = patch.Name
	}
	if strings.TrimSpace(patch.Task) != "" {
		cur.Task = patch.Task
	}
	if strings.TrimSpace(patch.Model) != "" {
		cur.Model = patch.Model
	}
	if patch.Schedule != (config.ScheduleConfig{}) {
		cur.Schedule = patch.Schedule
	}
	sched, err := cur.Schedule.Schedule()
	if err != nil {
		return watcher.WatcherInfo{}, fmt.Errorf("invalid watcher schedule: %w", err)
	}

	// Rebuild the runner under the same id (schedule is immutable on a Runner).
	if err := mgr.Remove(info.ID); err != nil && !errors.Is(err, watcher.ErrNotFound) {
		return watcher.WatcherInfo{}, fmt.Errorf("update watcher: %w", err)
	}
	runner := watcher.NewRunner(watcher.Spec{
		ID:             cur.ID,
		Name:           cur.Name,
		Task:           cur.Task,
		Model:          cur.Model,
		Kind:           kind,
		SessionID:      owner,
		Schedule:       sched,
		ScheduleDesc:   scheduleSummary(cur.Schedule),
		Enabled:        cur.Enabled,
		SuppressNotify: suppressWatcherNotify(cur.Output),
	})
	if err := mgr.Add(runner); err != nil {
		return watcher.WatcherInfo{}, fmt.Errorf("re-register updated watcher: %w", err)
	}

	if kind == watcher.KindAttached {
		g.replaceAttachedConfig(owner, cur)
		g.persistSession(owner)
	} else if err := g.persistFreeWatcherUpsert(cur); err != nil {
		g.warnf("failed to persist updated watcher %q: %v", cur.Name, err)
	}
	out, _ := mgr.Get(cur.ID)
	return out, nil
}

// DeleteWatcher unregisters a watcher by id or name (issue #329 Phase 3). A
// free-running watcher is also dropped from watchers.json and its dedicated
// watcher:<name> session removed; an attached watcher is dropped from its owning
// session's stored set (and the session re-persisted). It is gated by
// ActionWatcher.
func (g *Gogent) DeleteWatcher(idOrName string) error {
	g.mu.RLock()
	mgr := g.watchers
	g.mu.RUnlock()
	if mgr == nil {
		return fmt.Errorf("watcher engine is not running")
	}
	info, err := mgr.Resolve(idOrName)
	if err != nil {
		return fmt.Errorf("resolve watcher: %w", err)
	}
	if g.permissions != nil {
		if err := g.permissions.CheckWithDetail(permission.ActionWatcher, info.Name, "delete watcher "+info.Name); err != nil {
			return fmt.Errorf("permission check: %w", err)
		}
	}
	if err := mgr.Remove(info.ID); err != nil {
		return fmt.Errorf("delete watcher: %w", err)
	}
	if info.Kind == watcher.KindAttached {
		g.removeAttachedConfig(info.TargetSession, info.ID)
		g.persistSession(info.TargetSession)
		return nil
	}
	// Free-running: drop from watchers.json and remove its dedicated session.
	if err := g.persistFreeWatcherRemove(info.ID); err != nil {
		g.warnf("failed to drop watcher %q from store: %v", info.Name, err)
	}
	g.RemoveSession(watcherSessionPrefix + info.Name)
	return nil
}

// ToggleWatcher flips a watcher's enabled state by id or name (issue #329 Phase
// 3): disabling stops its schedule, enabling re-arms it. The new state is
// persisted (watchers.json for free-running, the owning session's index for
// attached). It is gated by ActionWatcher.
func (g *Gogent) ToggleWatcher(idOrName string) error {
	g.mu.RLock()
	mgr := g.watchers
	g.mu.RUnlock()
	if mgr == nil {
		return fmt.Errorf("watcher engine is not running")
	}
	info, err := mgr.Resolve(idOrName)
	if err != nil {
		return fmt.Errorf("resolve watcher: %w", err)
	}
	return g.SetWatcherEnabled(info.ID, !info.Enabled)
}

// SetWatcherEnabled drives a watcher to a specific enabled state by id or name
// (issue #329 Phase 3) — the enable_watcher/disable_watcher tools' backing. It is
// idempotent (a no-op when already in the requested state), persists the new
// state, and is gated by ActionWatcher.
func (g *Gogent) SetWatcherEnabled(idOrName string, enabled bool) error {
	g.mu.RLock()
	mgr := g.watchers
	g.mu.RUnlock()
	if mgr == nil {
		return fmt.Errorf("watcher engine is not running")
	}
	info, err := mgr.Resolve(idOrName)
	if err != nil {
		return fmt.Errorf("resolve watcher: %w", err)
	}
	if g.permissions != nil {
		if err := g.permissions.CheckWithDetail(permission.ActionWatcher, info.Name, "toggle watcher "+info.Name); err != nil {
			return fmt.Errorf("permission check: %w", err)
		}
	}
	if info.Enabled != enabled {
		if err := mgr.Toggle(info.ID); err != nil {
			return fmt.Errorf("toggle watcher: %w", err)
		}
	}
	g.setWatcherConfigEnabled(info, enabled)
	return nil
}

// RunWatcherNow fires a watcher immediately, ignoring its schedule and enabled
// state (issue #329 Phase 3). It is gated by ActionWatcher.
func (g *Gogent) RunWatcherNow(idOrName string) error {
	g.mu.RLock()
	mgr := g.watchers
	g.mu.RUnlock()
	if mgr == nil {
		return fmt.Errorf("watcher engine is not running")
	}
	info, err := mgr.Resolve(idOrName)
	if err != nil {
		return fmt.Errorf("resolve watcher: %w", err)
	}
	if g.permissions != nil {
		if err := g.permissions.CheckWithDetail(permission.ActionWatcher, info.Name, "run watcher "+info.Name); err != nil {
			return fmt.Errorf("permission check: %w", err)
		}
	}
	if err := mgr.RunNow(info.ID); err != nil {
		return fmt.Errorf("run watcher: %w", err)
	}
	return nil
}

// StopWatcher cancels a watcher's in-flight fire (if any) without stopping its
// schedule (issue #329 Phase 3). It is gated by ActionWatcher.
func (g *Gogent) StopWatcher(idOrName string) error {
	g.mu.RLock()
	mgr := g.watchers
	g.mu.RUnlock()
	if mgr == nil {
		return fmt.Errorf("watcher engine is not running")
	}
	info, err := mgr.Resolve(idOrName)
	if err != nil {
		return fmt.Errorf("resolve watcher: %w", err)
	}
	if g.permissions != nil {
		if err := g.permissions.CheckWithDetail(permission.ActionWatcher, info.Name, "stop watcher "+info.Name); err != nil {
			return fmt.Errorf("permission check: %w", err)
		}
	}
	if err := mgr.StopWatcher(info.ID); err != nil {
		return fmt.Errorf("stop watcher: %w", err)
	}
	return nil
}

// --- attached/free config bookkeeping ---

// lookupWatcherConfig finds the stored config for a watcher id, reporting its
// kind and owning session (owner is "" for free-running). It searches the
// per-session attached sets first, then watchers.json.
func (g *Gogent) lookupWatcherConfig(id string) (cfg config.WatcherConfig, kind watcher.Kind, owner string, found bool) {
	g.mu.RLock()
	for sid, wcs := range g.attachedWatchers {
		for _, wc := range wcs {
			if wc.ID == id {
				g.mu.RUnlock()
				return wc, watcher.KindAttached, sid, true
			}
		}
	}
	g.mu.RUnlock()
	store := g.LoadWatchers()
	for _, wc := range store.Items {
		if wc.ID == id {
			return wc, watcher.KindFree, "", true
		}
	}
	return config.WatcherConfig{}, watcher.KindFree, "", false
}

// setWatcherConfigEnabled records a watcher's new enabled flag in its persistent
// store (the owning session's index for attached, watchers.json for free-running).
func (g *Gogent) setWatcherConfigEnabled(info watcher.WatcherInfo, enabled bool) {
	if info.Kind == watcher.KindAttached {
		g.mu.Lock()
		wcs := g.attachedWatchers[info.TargetSession]
		for i := range wcs {
			if wcs[i].ID == info.ID {
				wcs[i].Enabled = enabled
			}
		}
		g.mu.Unlock()
		g.persistSession(info.TargetSession)
		return
	}
	cur, _, _, found := g.lookupWatcherConfig(info.ID)
	if !found {
		return
	}
	cur.Enabled = enabled
	if err := g.persistFreeWatcherUpsert(cur); err != nil {
		g.warnf("failed to persist watcher %q enabled=%v: %v", info.Name, enabled, err)
	}
}

// replaceAttachedConfig swaps the stored attached config for the one with the
// matching id under owner.
func (g *Gogent) replaceAttachedConfig(owner string, cfg config.WatcherConfig) {
	g.mu.Lock()
	defer g.mu.Unlock()
	wcs := g.attachedWatchers[owner]
	for i := range wcs {
		if wcs[i].ID == cfg.ID {
			wcs[i] = cfg
			return
		}
	}
}

// removeAttachedConfig drops the attached config with id from owner's stored set.
func (g *Gogent) removeAttachedConfig(owner, id string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	wcs := g.attachedWatchers[owner]
	out := wcs[:0]
	for _, wc := range wcs {
		if wc.ID != id {
			out = append(out, wc)
		}
	}
	if len(out) == 0 {
		delete(g.attachedWatchers, owner)
		return
	}
	g.attachedWatchers[owner] = out
}

// persistFreeWatcherUpsert inserts or replaces cfg (matched by id) in
// watchers.json, so a free-running watcher created/updated at runtime survives
// restart. ReportToSession is cleared defensively — watchers.json holds
// free-running definitions only.
func (g *Gogent) persistFreeWatcherUpsert(cfg config.WatcherConfig) error {
	cfg.ReportToSession = nil
	store := g.LoadWatchers()
	replaced := false
	for i := range store.Items {
		if store.Items[i].ID == cfg.ID {
			store.Items[i] = cfg
			replaced = true
			break
		}
	}
	if !replaced {
		store.Items = append(store.Items, cfg)
	}
	return g.SaveWatchers(&store)
}

// persistFreeWatcherRemove drops the free-running watcher with id from
// watchers.json.
func (g *Gogent) persistFreeWatcherRemove(id string) error {
	store := g.LoadWatchers()
	out := store.Items[:0]
	for _, wc := range store.Items {
		if wc.ID != id {
			out = append(out, wc)
		}
	}
	store.Items = out
	return g.SaveWatchers(&store)
}

// RunWatcherFire executes exactly one fire of a free-running watcher to
// completion, satisfying watcher.WatcherHost. It ensures the watcher's dedicated
// persistent session ("watcher:<name>") exists, then runs the watcher's task
// prompt through the normal agent loop (the same runLoop machinery as a user
// turn) on the watcher's configured model — so tools, sub-agents, compaction and
// the checkpointer all apply, and the turn's events surface in the watcher's
// session window. The fire respects ctx: manager shutdown or a per-watcher stop
// cancels the in-flight turn.
//
// The task is prefixed with a watcher-origin tag so the session transcript can
// distinguish watcher-originated turns. A one-line result summary is recorded on
// the Runner (surfaced by the manager's completion notification and the watcher
// list).
func (g *Gogent) RunWatcherFire(ctx context.Context, r *watcher.Runner) error {
	// An attached watcher fires into its owning session's transcript (it was created
	// from within that live session and dies with it); a free-running watcher fires
	// into its own dedicated, persistent "watcher:<name>" session (issue #329).
	var sessionID string
	if r.Kind() == watcher.KindAttached {
		sessionID = r.SessionID()
		if sessionID == "" {
			return fmt.Errorf("attached watcher %q has no owning session", r.Name())
		}
		// The owning session is live for an attached watcher; if it has gone away
		// (closed mid-fire) there is nothing to report into. This check is
		// best-effort: a RemoveSession racing in *after* it still tears the watcher
		// down via OnSessionClosed -> RemoveAttachedForSession, which cancels this
		// fire's ctx, so the in-flight turn aborts and its output is discarded
		// rather than landing in a detached session (benign — no panic, no leak).
		if g.GetUserSession(sessionID) == nil {
			return fmt.Errorf("attached watcher %q owning session %q is not live", r.Name(), sessionID)
		}
	} else {
		sessionID = watcherSessionPrefix + r.Name()
		g.ensureWatcherSession(sessionID)
	}

	message := fmt.Sprintf("[Watcher %q fired]\n\n%s", r.Name(), r.Task())
	resp, err := g.SendMessageToSessionWithModel(ctx, sessionID, "root", message, r.Model())
	if err != nil {
		return err
	}
	if resp != nil {
		r.SetLastResult(firstLine(resp.Content))
	}
	return nil
}

// ensureWatcherSession makes a free-running watcher's dedicated, persistent
// "watcher:<name>" session live for a fire. If it is already in memory it is
// reused. Otherwise, when a transcript was persisted on a previous run it is
// adopted from disk so context accumulates across restarts — the free-running
// "memory across fires" goal. This adoption matters in two cases the blind
// NewSession path missed: headless mode, where the startup restore (a TUI-only
// callback) never runs, and the TUI startup race, where a fire could otherwise
// create an empty session before RestoreSessions adopts the saved one. Only when
// nothing is on disk is a fresh session created.
func (g *Gogent) ensureWatcherSession(sessionID string) {
	if g.GetUserSession(sessionID) != nil {
		return
	}
	if g.store != nil {
		if metas, err := g.store.ListSessions(); err == nil {
			for _, m := range metas {
				if m.ID != sessionID {
					continue
				}
				// ContinueSession loads the persisted transcript and adopts it as a
				// live session (so the next turn appends rather than starting over).
				if _, ok := g.ContinueSession(m.File); ok {
					return
				}
				break
			}
		}
	}
	// First-ever fire (nothing persisted) — start a fresh non-ephemeral session.
	g.NewSession(sessionID)
}

// Notify delivers a backend-originated notification, satisfying
// watcher.WatcherHost. reason is a stable token (the manager passes "watcher").
// When a notify sink is installed (TUI mode) delivery is delegated to it so the
// notification is posted on the UI thread and gated there; otherwise (headless)
// it is gated through the fallback notifier's per-event toggle (ShouldNotify)
// and written directly. A free-running watcher fire is never "focused", so focus
// suppression does not apply.
func (g *Gogent) Notify(reason, title, body string) {
	g.mu.RLock()
	sink := g.notifySink
	notifier := g.notifier
	g.mu.RUnlock()
	if sink != nil {
		sink(reason, title, body)
		return
	}
	if notifier == nil {
		return
	}
	if !notifier.ShouldNotify(notify.Reason(reason), false) {
		return
	}
	notifier.Notify(title, body)
}

// SetNotifySink installs the backend notification delivery callback, replacing
// the fallback os.Stdout notifier (issue #329). The TUI entry point uses it to
// route watcher-completion notifications through the workbench's UI thread. A nil
// fn restores the fallback notifier.
func (g *Gogent) SetNotifySink(fn func(reason, title, body string)) {
	g.mu.Lock()
	g.notifySink = fn
	g.mu.Unlock()
}

// suppressWatcherNotify maps a watcher's on_complete config to the manager's
// SuppressNotify flag (issue #329). A nil Output keeps the default-on
// notification ("nil = notify on"); a non-nil Output honours its Notify field, so
// on_complete.notify=false suppresses the completion notification.
func suppressWatcherNotify(out *config.WatcherOutput) bool {
	return out != nil && !out.Notify
}

// watcherLaunchDetail renders a human-readable summary of what starting a
// watcher entails, shown in the ActionWatcher permission prompt.
func watcherLaunchDetail(wc config.WatcherConfig) string {
	return fmt.Sprintf("start watcher %q (%s)", wc.Name, scheduleSummary(wc.Schedule))
}

// scheduleSummary renders a watcher schedule as a short human-readable string
// ("every 5m", "daily 07:00 Europe/Zurich") for prompts and logs. It does not
// validate — it only describes whatever fields are set.
func scheduleSummary(s config.ScheduleConfig) string {
	switch {
	case strings.TrimSpace(s.Every) != "":
		return "every " + strings.TrimSpace(s.Every)
	case strings.TrimSpace(s.DailyAt) != "":
		tz := strings.TrimSpace(s.Timezone)
		if tz == "" {
			tz = "UTC"
		}
		return "daily " + strings.TrimSpace(s.DailyAt) + " " + tz
	default:
		return "no schedule"
	}
}

// firstLine returns the first non-empty line of s, trimmed, truncated to a
// reasonable length for a one-line summary. An all-blank string yields "".
func firstLine(s string) string {
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		const max = 200
		if r := []rune(line); len(r) > max {
			return string(r[:max])
		}
		return line
	}
	return ""
}
