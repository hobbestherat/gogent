package gogent

import (
	"context"
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
			ID:       wc.ID,
			Name:     wc.Name,
			Task:     wc.Task,
			Model:    wc.Model,
			Kind:     watcher.KindFree,
			Schedule: sched,
			Enabled:  true,
		})
		if err := mgr.Add(runner); err != nil {
			g.logger().Warn("watcher add failed", "name", wc.Name, "error", err)
		}
	}

	mgr.Start()

	g.mu.Lock()
	g.watchers = mgr
	g.mu.Unlock()
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
	sessionID := watcherSessionPrefix + r.Name()
	g.ensureWatcherSession(sessionID)

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
