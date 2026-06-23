package gogent

import (
	"errors"
	"testing"

	"gogent/internal/config"
	"gogent/internal/watcher"
)

// TestCreateWatcherAttachedTargetNotActiveRejected proves the fix for the
// silent-data-loss gap: an attached watcher pointed at a session that is not live
// is rejected up front (it could never be persisted, so it would vanish on
// restart). Nothing is registered or stored.
func TestCreateWatcherAttachedTargetNotActiveRejected(t *testing.T) {
	g := startedWatcherGogent(t)
	// "ghost" was never created via NewSession, so it is not a live session.
	_, err := g.CreateWatcher(config.WatcherConfig{
		Name:            "orphan",
		Enabled:         true,
		Schedule:        config.ScheduleConfig{Every: "5m"},
		Task:            "t",
		ReportToSession: strptr("ghost"),
	}, "caller-not-live-either")
	if err == nil {
		t.Fatal("attached watcher targeting a non-live session must be rejected")
	}
	// It must not have been registered anywhere.
	if _, ok := findWatcher(g.ListWatchers("ghost"), "orphan"); ok {
		t.Error("a rejected attached watcher must not be registered")
	}
	if g.attachedWatchersFor("ghost") != nil {
		t.Error("a rejected attached watcher must not be recorded")
	}
}

// TestCreateWatcherAttachedLiveTargetAccepted is the positive control: the same
// create succeeds once the target session is live.
func TestCreateWatcherAttachedLiveTargetAccepted(t *testing.T) {
	g := startedWatcherGogent(t)
	const sid = "live-target"
	g.NewSession(sid)
	if _, err := g.CreateWatcher(config.WatcherConfig{
		Name:            "ok",
		Enabled:         true,
		Schedule:        config.ScheduleConfig{Every: "5m"},
		Task:            "t",
		ReportToSession: strptr(sid),
	}, "someone-else"); err != nil {
		t.Fatalf("CreateWatcher to a live target should succeed: %v", err)
	}
	if _, ok := findWatcher(g.ListWatchers(sid), "ok"); !ok {
		t.Error("attached watcher to a live target should be registered")
	}
}

// TestDeleteWatcherAmbiguousNameSurfacesAmbiguous proves the wrappers now surface
// ErrAmbiguous (not a misleading "not found") when a name matches more than one
// watcher — here a free-running and an attached watcher sharing a name.
func TestDeleteWatcherAmbiguousNameSurfacesAmbiguous(t *testing.T) {
	g := startedWatcherGogent(t)
	const sid = "amb-session"
	g.NewSession(sid)

	if _, err := g.CreateWatcher(config.WatcherConfig{
		Name: "dup", Enabled: true, Schedule: config.ScheduleConfig{Every: "1h"}, Task: "t",
	}, ""); err != nil {
		t.Fatalf("CreateWatcher free: %v", err)
	}
	if _, err := g.CreateWatcher(config.WatcherConfig{
		Name: "dup", Enabled: true, Schedule: config.ScheduleConfig{Every: "1h"}, Task: "t", ReportToSession: strptr(sid),
	}, sid); err != nil {
		t.Fatalf("CreateWatcher attached: %v", err)
	}

	err := g.DeleteWatcher("dup")
	if err == nil {
		t.Fatal("deleting by an ambiguous name should error")
	}
	if !errors.Is(err, watcher.ErrAmbiguous) {
		t.Errorf("error = %v, want it to wrap ErrAmbiguous (not ErrNotFound)", err)
	}
	if errors.Is(err, watcher.ErrNotFound) {
		t.Error("ambiguous name must NOT be reported as not-found")
	}

	// Operating by the unambiguous id still works (delete the free one).
	free, ok := findWatcher(g.ListWatchers(""), "dup")
	if !ok {
		t.Fatal("free-running dup should be listed at the empty scope")
	}
	if err := g.DeleteWatcher(free.ID); err != nil {
		t.Errorf("deleting by id should succeed even under a duplicate name: %v", err)
	}
}

// TestToggleWatcherAmbiguousNameSurfacesAmbiguous confirms the same ambiguity
// surfacing applies to the other id-or-name control wrappers (toggle here).
func TestToggleWatcherAmbiguousNameSurfacesAmbiguous(t *testing.T) {
	g := startedWatcherGogent(t)
	const sid = "amb-toggle"
	g.NewSession(sid)
	for _, report := range []*string{nil, strptr(sid)} {
		if _, err := g.CreateWatcher(config.WatcherConfig{
			Name: "twin", Enabled: true, Schedule: config.ScheduleConfig{Every: "1h"}, Task: "t", ReportToSession: report,
		}, sid); err != nil {
			t.Fatalf("CreateWatcher: %v", err)
		}
	}
	if err := g.ToggleWatcher("twin"); !errors.Is(err, watcher.ErrAmbiguous) {
		t.Errorf("ToggleWatcher(ambiguous) err = %v, want ErrAmbiguous", err)
	}
}

// TestSuppressWatcherNotifyMapping pins the on_complete -> SuppressNotify mapping
// that StartWatchers/CreateWatcher/UpdateWatcher feed into the runner Spec: nil
// Output keeps notifications on; an explicit notify=true keeps them on; only an
// explicit notify=false suppresses.
func TestSuppressWatcherNotifyMapping(t *testing.T) {
	for _, tc := range []struct {
		name string
		out  *config.WatcherOutput
		want bool
	}{
		{"nil output -> notify (suppress=false)", nil, false},
		{"notify=true -> suppress=false", &config.WatcherOutput{Notify: true}, false},
		{"notify=false -> suppress=true", &config.WatcherOutput{Notify: false}, true},
	} {
		if got := suppressWatcherNotify(tc.out); got != tc.want {
			t.Errorf("%s: suppressWatcherNotify = %v, want %v", tc.name, got, tc.want)
		}
	}
}
