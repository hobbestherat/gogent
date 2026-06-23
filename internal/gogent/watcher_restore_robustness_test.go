package gogent

import (
	"testing"

	"gogent/internal/config"
	"gogent/internal/watcher"
)

// TestOnSessionRestoredSkipsInvalidScheduleWatcher proves a corrupt attached
// watcher config (an impossible schedule) in a restored session index is logged
// and skipped without blocking the well-formed watchers in the same session.
func TestOnSessionRestoredSkipsInvalidScheduleWatcher(t *testing.T) {
	g := startedWatcherGogent(t)
	const sid = "sess-corrupt"
	g.NewSession(sid)

	g.OnSessionRestored(sid, []config.WatcherConfig{
		{ID: "watcher-ok000001", Name: "ok", Enabled: true, Schedule: config.ScheduleConfig{Every: "5m"}, Task: "t", ReportToSession: strptr(sid)},
		{ID: "watcher-bad00001", Name: "bad", Enabled: true, Schedule: config.ScheduleConfig{Every: "5m", DailyAt: "07:00"}, Task: "t", ReportToSession: strptr(sid)}, // both -> invalid
	})

	if _, ok := findWatcher(g.ListWatchers(sid), "ok"); !ok {
		t.Error("the well-formed watcher must be registered despite a corrupt sibling")
	}
	if _, ok := findWatcher(g.ListWatchers(sid), "bad"); ok {
		t.Error("a watcher with an invalid schedule must be skipped, not registered")
	}
}

// TestOnSessionRestoredIdempotent proves restoring the same session's watchers
// twice does not double-register (the second pass tolerates the duplicate id).
func TestOnSessionRestoredIdempotent(t *testing.T) {
	g := startedWatcherGogent(t)
	const sid = "sess-twice"
	g.NewSession(sid)

	wcs := []config.WatcherConfig{
		{ID: "watcher-dup00001", Name: "once", Enabled: true, Schedule: config.ScheduleConfig{Every: "5m"}, Task: "t", ReportToSession: strptr(sid)},
	}
	g.OnSessionRestored(sid, wcs)
	g.OnSessionRestored(sid, wcs)

	count := 0
	for _, info := range g.ListWatchers(sid) {
		if info.Name == "once" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("watcher registered %d times, want exactly 1", count)
	}
}

// TestOnSessionRestoredEmptyIsNoop confirms a nil/empty watcher list neither
// panics nor records anything.
func TestOnSessionRestoredEmptyIsNoop(t *testing.T) {
	g := startedWatcherGogent(t)
	const sid = "sess-empty"
	g.NewSession(sid)

	g.OnSessionRestored(sid, nil)
	g.OnSessionRestored(sid, []config.WatcherConfig{})

	if got := g.attachedWatchersFor(sid); got != nil {
		t.Errorf("empty restore must record nothing, got %+v", got)
	}
	if got := g.ListWatchers(sid); len(got) != 0 {
		t.Errorf("empty restore must register nothing, got %+v", got)
	}
}

// TestOnSessionRestoredBeforeStartDefersRegistration proves that restoring before
// the engine is built records the configs and that a later StartWatchers picks
// them up (the pending-registration path in StartWatchers).
func TestOnSessionRestoredBeforeStartDefersRegistration(t *testing.T) {
	g := NewGogent(t.TempDir())
	enableWatchers(g)
	allowWatchers(g)
	const sid = "sess-pending"

	// Engine not started yet (manager nil).
	g.OnSessionRestored(sid, []config.WatcherConfig{
		{ID: "watcher-pend0001", Name: "pending", Enabled: true, Schedule: config.ScheduleConfig{Every: "5m"}, Task: "t", ReportToSession: strptr(sid)},
	})
	if g.watchers != nil {
		t.Fatal("manager must still be nil before StartWatchers")
	}
	// The config was recorded for a later save/register.
	if got := g.attachedWatchersFor(sid); len(got) != 1 {
		t.Fatalf("restore-before-start should record the config, got %+v", got)
	}

	g.StartWatchers()
	t.Cleanup(g.StopWatchers)
	if _, ok := findWatcher(g.ListWatchers(sid), "pending"); !ok {
		t.Error("StartWatchers must register watchers recorded before it ran")
	}
}

// TestCreateWatcherDuplicateExplicitIDErrors confirms supplying an already-used id
// is rejected rather than silently shadowing the existing watcher.
func TestCreateWatcherDuplicateExplicitIDErrors(t *testing.T) {
	g := startedWatcherGogent(t)
	base := config.WatcherConfig{
		ID: "watcher-fixedid1", Name: "first", Enabled: true,
		Schedule: config.ScheduleConfig{Every: "5m"}, Task: "t",
	}
	if _, err := g.CreateWatcher(base, ""); err != nil {
		t.Fatalf("first CreateWatcher: %v", err)
	}
	dup := base
	dup.Name = "second"
	if _, err := g.CreateWatcher(dup, ""); err == nil {
		t.Error("creating a watcher with a duplicate explicit id should error")
	}
}

// TestDeleteFreeRunningWatcherRemovesFromStore confirms deleting a free-running
// watcher drops it from watchers.json (and does not disturb a co-existing one).
func TestDeleteFreeRunningWatcherRemovesFromStore(t *testing.T) {
	g := startedWatcherGogent(t)
	keepInfo, err := g.CreateWatcher(config.WatcherConfig{
		Name: "keep", Enabled: true, Schedule: config.ScheduleConfig{Every: "1h"}, Task: "t",
	}, "")
	if err != nil {
		t.Fatalf("CreateWatcher keep: %v", err)
	}
	dropInfo, err := g.CreateWatcher(config.WatcherConfig{
		Name: "drop", Enabled: true, Schedule: config.ScheduleConfig{Every: "1h"}, Task: "t",
	}, "")
	if err != nil {
		t.Fatalf("CreateWatcher drop: %v", err)
	}

	if err := g.DeleteWatcher(dropInfo.ID); err != nil {
		t.Fatalf("DeleteWatcher: %v", err)
	}
	if storeHasWatcher(g, "drop") {
		t.Error("deleted free-running watcher must be removed from watchers.json")
	}
	if !storeHasWatcher(g, "keep") {
		t.Error("deleting one free-running watcher must not drop the others")
	}
	if _, ok := findWatcher(g.ListWatchers(""), "drop"); ok {
		t.Error("deleted watcher must no longer be listed")
	}
	if _, ok := findWatcher(g.ListWatchers(""), "keep"); !ok {
		t.Error("surviving watcher must still be listed")
	}
	_ = keepInfo
}

// TestDeleteWatcherUnknownErrors confirms deleting a non-existent watcher reports
// not-found rather than succeeding silently.
func TestDeleteWatcherUnknownErrors(t *testing.T) {
	g := startedWatcherGogent(t)
	if err := g.DeleteWatcher("no-such-watcher"); err == nil {
		t.Error("deleting an unknown watcher should error")
	}
}

// TestToggleWatcherFlipsEnabled confirms ToggleWatcher inverts the enabled state
// (both directions) for a free-running watcher.
func TestToggleWatcherFlipsEnabled(t *testing.T) {
	g := startedWatcherGogent(t)
	info, err := g.CreateWatcher(config.WatcherConfig{
		Name: "flip", Enabled: true, Schedule: config.ScheduleConfig{Every: "1h"}, Task: "t",
	}, "")
	if err != nil {
		t.Fatalf("CreateWatcher: %v", err)
	}

	if err := g.ToggleWatcher(info.ID); err != nil {
		t.Fatalf("ToggleWatcher: %v", err)
	}
	if cur, _ := findWatcher(g.ListWatchers(""), "flip"); cur.Enabled {
		t.Error("toggle should have disabled the watcher")
	}
	if err := g.ToggleWatcher(info.ID); err != nil {
		t.Fatalf("ToggleWatcher back: %v", err)
	}
	if cur, _ := findWatcher(g.ListWatchers(""), "flip"); !cur.Enabled {
		t.Error("second toggle should have re-enabled the watcher")
	}

	// The toggled state must round-trip into watchers.json.
	for _, wc := range g.LoadWatchers().Items {
		if wc.Name == "flip" && !wc.Enabled {
			t.Error("re-enabled watcher should persist enabled=true to the store")
		}
	}
}

// TestControlMethodsRequireEngine confirms the watcher control methods all fail
// cleanly (no panic) when the engine was never started.
func TestControlMethodsRequireEngine(t *testing.T) {
	g := NewGogent(t.TempDir())
	enableWatchers(g)
	allowWatchers(g)
	// Engine deliberately not started.

	if err := g.DeleteWatcher("x"); err == nil {
		t.Error("DeleteWatcher should error with no engine")
	}
	if err := g.ToggleWatcher("x"); err == nil {
		t.Error("ToggleWatcher should error with no engine")
	}
	if err := g.SetWatcherEnabled("x", true); err == nil {
		t.Error("SetWatcherEnabled should error with no engine")
	}
	if err := g.RunWatcherNow("x"); err == nil {
		t.Error("RunWatcherNow should error with no engine")
	}
	if err := g.StopWatcher("x"); err == nil {
		t.Error("StopWatcher should error with no engine")
	}
	if _, err := g.UpdateWatcher(config.WatcherConfig{ID: "x"}, ""); err == nil {
		t.Error("UpdateWatcher should error with no engine")
	}
	// And OnSessionClosed on a dead engine is a safe no-op.
	g.OnSessionClosed("x")
	_ = watcher.KindFree // keep the import meaningful across builds
}
