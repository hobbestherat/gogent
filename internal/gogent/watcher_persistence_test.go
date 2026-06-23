package gogent

import (
	"testing"

	"gogent/internal/config"
	"gogent/internal/model"
	"gogent/internal/watcher"
)

// freshWatcherGogent returns a started watcher-enabled Gogent rooted at home (so
// successive instances share the same on-disk store/watchers.json).
func freshWatcherGogent(t *testing.T, home string) *Gogent {
	t.Helper()
	g := NewGogent(home)
	enableWatchers(g)
	allowWatchers(g)
	g.StartWatchers()
	if g.watchers == nil {
		t.Fatal("watcher engine should be running")
	}
	return g
}

// seedAndAttach creates a live session with a one-message transcript and the named
// attached watcher, then flushes to disk.
func seedAndAttach(t *testing.T, g *Gogent, sid, watcherName string, enabled bool) string {
	t.Helper()
	us := g.NewSession(sid)
	us.RootAgent.ThoughtTrain.AppendMessages(model.Message{Role: model.RoleUser, Content: "seed"})
	info, err := g.CreateWatcher(config.WatcherConfig{
		Name:            watcherName,
		Enabled:         enabled,
		Schedule:        config.ScheduleConfig{Every: "5m"},
		Task:            "original task",
		ReportToSession: strptr(sid),
	}, sid)
	if err != nil {
		t.Fatalf("CreateWatcher: %v", err)
	}
	g.persistSession(sid)
	if g.store != nil {
		g.store.Sync()
	}
	return info.ID
}

// TestFreeRunningWatcherSurvivesRestart proves a free-running watcher created at
// runtime (persisted to watchers.json) is re-registered when a fresh Gogent boots
// and runs StartWatchers — the create→persist→restart→reload chain.
func TestFreeRunningWatcherSurvivesRestart(t *testing.T) {
	home := t.TempDir()
	g1 := freshWatcherGogent(t, home)
	if _, err := g1.CreateWatcher(config.WatcherConfig{
		Name: "nightly", Enabled: true, Schedule: config.ScheduleConfig{DailyAt: "07:00", Timezone: "UTC"}, Task: "t",
	}, ""); err != nil {
		t.Fatalf("CreateWatcher: %v", err)
	}
	g1.StopWatchers()

	g2 := freshWatcherGogent(t, home)
	t.Cleanup(g2.StopWatchers)
	info, ok := findWatcher(g2.ListWatchers(""), "nightly")
	if !ok {
		t.Fatalf("free-running watcher not reloaded after restart; got %+v", g2.ListWatchers(""))
	}
	if info.Kind != watcher.KindFree {
		t.Errorf("reloaded kind = %v, want KindFree", info.Kind)
	}
}

// TestDeletedAttachedWatcherStaysGoneAfterRestart proves DeleteWatcher rewrites the
// owning session's index so the watcher does not resurrect on restore.
func TestDeletedAttachedWatcherStaysGoneAfterRestart(t *testing.T) {
	home := t.TempDir()
	const sid = "sess-del"
	g1 := freshWatcherGogent(t, home)
	id := seedAndAttach(t, g1, sid, "doomed", true)

	if err := g1.DeleteWatcher(id); err != nil {
		t.Fatalf("DeleteWatcher: %v", err)
	}
	g1.persistSession(sid)
	if g1.store != nil {
		g1.store.Sync()
	}
	g1.StopWatchers()

	g2 := freshWatcherGogent(t, home)
	t.Cleanup(g2.StopWatchers)
	g2.RestoreSessions()
	if _, ok := findWatcher(g2.ListWatchers(sid), "doomed"); ok {
		t.Error("a deleted attached watcher must not come back after restart")
	}
	if got := g2.attachedWatchersFor(sid); got != nil {
		t.Errorf("deleted attached watcher must not be in the restored session, got %+v", got)
	}
}

// TestDisabledAttachedWatcherPersistsAcrossRestart proves the enabled flag is
// serialized with the session: a disabled attached watcher restores disabled (and
// is still registered, just not armed).
func TestDisabledAttachedWatcherPersistsAcrossRestart(t *testing.T) {
	home := t.TempDir()
	const sid = "sess-disable"
	g1 := freshWatcherGogent(t, home)
	id := seedAndAttach(t, g1, sid, "toggler", true)

	if err := g1.SetWatcherEnabled(id, false); err != nil {
		t.Fatalf("SetWatcherEnabled: %v", err)
	}
	g1.persistSession(sid)
	if g1.store != nil {
		g1.store.Sync()
	}
	g1.StopWatchers()

	g2 := freshWatcherGogent(t, home)
	t.Cleanup(g2.StopWatchers)
	g2.RestoreSessions()
	info, ok := findWatcher(g2.ListWatchers(sid), "toggler")
	if !ok {
		t.Fatalf("disabled attached watcher should still be restored; got %+v", g2.ListWatchers(sid))
	}
	if info.Enabled {
		t.Error("watcher disabled before restart must restore disabled")
	}
}

// TestUpdatedAttachedWatcherPersistsAcrossRestart proves an update to an attached
// watcher's task is serialized with the session and survives restart.
func TestUpdatedAttachedWatcherPersistsAcrossRestart(t *testing.T) {
	home := t.TempDir()
	const sid = "sess-update"
	g1 := freshWatcherGogent(t, home)
	id := seedAndAttach(t, g1, sid, "editme", true)

	if _, err := g1.UpdateWatcher(config.WatcherConfig{ID: id, Task: "updated task"}, sid); err != nil {
		t.Fatalf("UpdateWatcher: %v", err)
	}
	g1.persistSession(sid)
	if g1.store != nil {
		g1.store.Sync()
	}
	g1.StopWatchers()

	g2 := freshWatcherGogent(t, home)
	t.Cleanup(g2.StopWatchers)
	g2.RestoreSessions()
	att := g2.attachedWatchersFor(sid)
	if len(att) != 1 {
		t.Fatalf("expected one restored attached watcher, got %+v", att)
	}
	if att[0].Task != "updated task" {
		t.Errorf("restored task = %q, want \"updated task\"", att[0].Task)
	}
}

// TestContinueSessionRestoresAttachedWatcher exercises the on-demand re-open path
// (ContinueSession, which shares adoptLoaded → OnSessionRestored with the startup
// RestoreSessions). A session re-opened by file must bring its attached watcher
// back.
func TestContinueSessionRestoresAttachedWatcher(t *testing.T) {
	home := t.TempDir()
	const sid = "sess-continue"
	g1 := freshWatcherGogent(t, home)
	seedAndAttach(t, g1, sid, "reopen", true)
	g1.StopWatchers()

	g2 := freshWatcherGogent(t, home)
	t.Cleanup(g2.StopWatchers)

	// Find the persisted session file and re-open it on demand.
	metas, err := g2.store.ListSessions()
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	var file string
	for _, m := range metas {
		if m.ID == sid {
			file = m.File
		}
	}
	if file == "" {
		t.Fatalf("persisted session %q not found in %+v", sid, metas)
	}
	if _, ok := g2.ContinueSession(file); !ok {
		t.Fatal("ContinueSession failed to re-open the session")
	}
	if _, ok := findWatcher(g2.ListWatchers(sid), "reopen"); !ok {
		t.Errorf("ContinueSession must re-register the attached watcher; got %+v", g2.ListWatchers(sid))
	}
}
