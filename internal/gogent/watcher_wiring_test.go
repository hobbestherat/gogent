package gogent

import (
	"bytes"
	"sort"
	"strings"
	"testing"

	"gogent/internal/config"
	"gogent/internal/model"
	"gogent/internal/notify"
	"gogent/internal/permission"
	"gogent/internal/watcher"
)

// allowWatchers grants the ActionWatcher permission so StartWatchers can launch
// (no interactive prompter is installed in tests, so "ask" would otherwise deny).
func allowWatchers(g *Gogent) {
	g.GetPermissionService().AddRule(permission.Rule{
		Action: string(permission.ActionWatcher), Resource: "*", Effect: string(permission.EffectAllow),
	})
}

// enableWatchers flips the experimental gate on the live config.
func enableWatchers(g *Gogent) {
	g.config.Experimental.Watchers = true
}

// freeWatcherNames returns the sorted names of the free-running watchers the
// manager currently knows about.
func freeWatcherNames(g *Gogent) []string {
	infos := g.ListWatchers("")
	names := make([]string, 0, len(infos))
	for _, info := range infos {
		names = append(names, info.Name)
	}
	sort.Strings(names)
	return names
}

// TestStartWatchersNoopWhenExperimentalOff confirms that with the experimental
// gate off, StartWatchers builds no manager at all — even with a populated
// watchers.json and the permission granted.
func TestStartWatchersNoopWhenExperimentalOff(t *testing.T) {
	g := NewGogent(t.TempDir())
	allowWatchers(g)
	if err := g.SaveWatchers(&config.WatcherStore{Items: []config.WatcherConfig{
		{ID: "watcher-11111111", Name: "alpha", Enabled: true, Schedule: config.ScheduleConfig{Every: "1h"}, Task: "t"},
	}}); err != nil {
		t.Fatalf("SaveWatchers: %v", err)
	}
	// Experimental.Watchers deliberately left false.
	g.StartWatchers()
	t.Cleanup(g.StopWatchers)

	if g.watchers != nil {
		t.Error("StartWatchers must be a no-op (nil manager) when Experimental.Watchers is off")
	}
	if got := g.ListWatchers(""); got != nil {
		t.Errorf("ListWatchers should be nil with no manager, got %+v", got)
	}
}

// TestStartWatchersDefaultDenyNoRule confirms that with the feature enabled but
// NO permission rule, the default-deny posture (no prompter -> deny) keeps every
// watcher from being registered, while the manager itself is still built.
func TestStartWatchersDefaultDenyNoRule(t *testing.T) {
	g := NewGogent(t.TempDir())
	enableWatchers(g)
	if err := g.SaveWatchers(&config.WatcherStore{Items: []config.WatcherConfig{
		{ID: "watcher-22222222", Name: "alpha", Enabled: true, Schedule: config.ScheduleConfig{Every: "1h"}, Task: "t"},
	}}); err != nil {
		t.Fatalf("SaveWatchers: %v", err)
	}
	g.StartWatchers()
	t.Cleanup(g.StopWatchers)

	if g.watchers == nil {
		t.Fatal("manager should be built when the feature is enabled")
	}
	if names := freeWatcherNames(g); len(names) != 0 {
		t.Errorf("default-deny should register no watchers, got %v", names)
	}
}

// TestStartWatchersDeniedByRule confirms an explicit deny rule keeps watchers
// unregistered (mirrors the MCP gating test).
func TestStartWatchersDeniedByRule(t *testing.T) {
	g := NewGogent(t.TempDir())
	enableWatchers(g)
	g.GetPermissionService().AddRule(permission.Rule{
		Action: string(permission.ActionWatcher), Resource: "*", Effect: string(permission.EffectDeny),
	})
	if err := g.SaveWatchers(&config.WatcherStore{Items: []config.WatcherConfig{
		{ID: "watcher-33333333", Name: "alpha", Enabled: true, Schedule: config.ScheduleConfig{Every: "1h"}, Task: "t"},
	}}); err != nil {
		t.Fatalf("SaveWatchers: %v", err)
	}
	g.StartWatchers()
	t.Cleanup(g.StopWatchers)

	if names := freeWatcherNames(g); len(names) != 0 {
		t.Errorf("denied watcher must not be registered, got %v", names)
	}
}

// TestStartWatchersRegistersEnabledFreeRunning is the positive path: with the
// feature on and permission granted, exactly the enabled, well-formed,
// non-empty-named free-running watchers are registered as KindFree runners.
// Disabled, empty-named, and invalid-schedule entries are each skipped without
// blocking the others.
func TestStartWatchersRegistersEnabledFreeRunning(t *testing.T) {
	g := NewGogent(t.TempDir())
	enableWatchers(g)
	allowWatchers(g)
	if err := g.SaveWatchers(&config.WatcherStore{Items: []config.WatcherConfig{
		{ID: "watcher-aaaa0001", Name: "alpha", Enabled: true, Schedule: config.ScheduleConfig{Every: "1h"}, Task: "ta"},
		{ID: "watcher-aaaa0002", Name: "beta", Enabled: true, Schedule: config.ScheduleConfig{DailyAt: "07:00", Timezone: "Europe/Zurich"}, Task: "tb"},
		{ID: "watcher-aaaa0003", Name: "gamma-disabled", Enabled: false, Schedule: config.ScheduleConfig{Every: "1h"}, Task: "tc"},
		{ID: "watcher-aaaa0004", Name: "  ", Enabled: true, Schedule: config.ScheduleConfig{Every: "1h"}, Task: "td"},                            // empty name
		{ID: "watcher-aaaa0005", Name: "epsilon-bad", Enabled: true, Schedule: config.ScheduleConfig{Every: "5m", DailyAt: "07:00"}, Task: "te"}, // both -> invalid
	}}); err != nil {
		t.Fatalf("SaveWatchers: %v", err)
	}
	g.StartWatchers()
	t.Cleanup(g.StopWatchers)

	got := freeWatcherNames(g)
	want := []string{"alpha", "beta"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("registered watchers = %v, want %v", got, want)
	}

	// Every registered watcher is free-running.
	for _, info := range g.ListWatchers("") {
		if info.Kind != watcher.KindFree {
			t.Errorf("watcher %q has kind %v, want KindFree", info.Name, info.Kind)
		}
	}
}

// TestStartWatchersInvalidScheduleSkippedOthersSurvive isolates the "one bad
// entry never blocks the rest" property with a single malformed schedule.
func TestStartWatchersInvalidScheduleSkippedOthersSurvive(t *testing.T) {
	g := NewGogent(t.TempDir())
	enableWatchers(g)
	allowWatchers(g)
	if err := g.SaveWatchers(&config.WatcherStore{Items: []config.WatcherConfig{
		{ID: "watcher-bad00001", Name: "bad", Enabled: true, Schedule: config.ScheduleConfig{Every: "not-a-duration"}, Task: "t"},
		{ID: "watcher-good0001", Name: "good", Enabled: true, Schedule: config.ScheduleConfig{Every: "1h"}, Task: "t"},
	}}); err != nil {
		t.Fatalf("SaveWatchers: %v", err)
	}
	g.StartWatchers()
	t.Cleanup(g.StopWatchers)

	if got := freeWatcherNames(g); strings.Join(got, ",") != "good" {
		t.Errorf("expected only the well-formed watcher to register, got %v", got)
	}
}

// TestStopWatchersIdempotentAndSafe confirms StopWatchers is safe when nothing
// was started and clears the manager after a start.
func TestStopWatchersIdempotentAndSafe(t *testing.T) {
	g := NewGogent(t.TempDir())
	// Safe with no manager.
	g.StopWatchers()
	g.StopWatchers()

	enableWatchers(g)
	allowWatchers(g)
	if err := g.SaveWatchers(&config.WatcherStore{Items: []config.WatcherConfig{
		{ID: "watcher-stop0001", Name: "s", Enabled: true, Schedule: config.ScheduleConfig{Every: "1h"}, Task: "t"},
	}}); err != nil {
		t.Fatalf("SaveWatchers: %v", err)
	}
	g.StartWatchers()
	if g.watchers == nil {
		t.Fatal("manager should be set after StartWatchers")
	}
	g.StopWatchers()
	if g.watchers != nil {
		t.Error("StopWatchers should clear the manager")
	}
	// A second stop after teardown must not panic.
	g.StopWatchers()
}

// TestStartWatchersEmptyStoreBuildsEmptyManager confirms enabling the feature
// with no watcher definitions builds a manager with zero runners (not nil), so
// later phases (tools/HTTP) have a live manager to add to.
func TestStartWatchersEmptyStoreBuildsEmptyManager(t *testing.T) {
	g := NewGogent(t.TempDir())
	enableWatchers(g)
	allowWatchers(g)
	g.StartWatchers()
	t.Cleanup(g.StopWatchers)

	if g.watchers == nil {
		t.Fatal("manager should be built even with an empty store")
	}
	if got := g.ListWatchers(""); len(got) != 0 {
		t.Errorf("empty store should yield zero watchers, got %+v", got)
	}
}

// TestWatcherSessionIsNonEphemeralAndRestored proves the key visibility property
// for free-running watchers: a "watcher:<name>" session is an ordinary,
// non-ephemeral session — it is NOT special-cased as ephemeral/default, so it is
// persisted and picked up by RestoreSessions on the next boot. (We avoid a live
// model fire, exercising the session lifecycle directly.)
func TestWatcherSessionIsNonEphemeralAndRestored(t *testing.T) {
	home := t.TempDir()
	g := NewGogent(home)

	const sid = watcherSessionPrefix + "daily-meeting" // "watcher:daily-meeting"
	us := g.NewSession(sid)
	if us == nil {
		t.Fatal("NewSession returned nil")
	}

	// The watcher: prefix must not mark the session ephemeral.
	g.mu.RLock()
	eph := g.ephemeral[sid]
	g.mu.RUnlock()
	if eph {
		t.Fatalf("watcher session %q must not be ephemeral", sid)
	}

	// Seed a transcript so there is something to persist, then persist + flush.
	us.RootAgent.ThoughtTrain.AppendMessages(model.Message{Role: model.RoleUser, Content: "first fire"})
	g.persistSession(sid)
	if g.store != nil {
		g.store.Sync()
	}

	// A fresh gogent on the same home restores the watcher session.
	g2 := NewGogent(home)
	restored := g2.RestoreSessions()
	found := false
	for _, ls := range restored {
		if ls.ID == sid {
			found = true
			break
		}
	}
	if !found {
		ids := make([]string, 0, len(restored))
		for _, ls := range restored {
			ids = append(ids, ls.ID)
		}
		t.Fatalf("watcher session %q was not restored; restored=%v", sid, ids)
	}
}

// TestNotifyWrapperGating exercises Gogent.Notify (the watcher.WatcherHost notify
// seam): it fires through the notifier only when ShouldNotify allows, and is a
// safe no-op when no notifier is installed.
func TestNotifyWrapperGating(t *testing.T) {
	base := config.DefaultNotifyConfig()
	base.Native = false // never shell out in tests

	on := base // Enabled + OnWatcher + Bell + Desktop all true

	masterOff := base
	masterOff.Enabled = false

	watcherOff := base
	watcherOff.OnWatcher = false

	for _, tc := range []struct {
		name      string
		cfg       config.NotifyConfig
		reason    string
		wantWrite bool
	}{
		{"watcher on writes", on, string(notify.ReasonWatcher), true},
		{"master off suppresses", masterOff, string(notify.ReasonWatcher), false},
		{"on_watcher off suppresses", watcherOff, string(notify.ReasonWatcher), false},
		{"unknown reason suppressed", on, "bogus-reason", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			g := NewGogent(t.TempDir())
			g.notifier = notify.New(tc.cfg, &buf)
			g.Notify(tc.reason, "title", "body")
			if got := buf.Len() > 0; got != tc.wantWrite {
				t.Errorf("Notify wrote=%v (%q), want %v", got, buf.String(), tc.wantWrite)
			}
			if tc.wantWrite && !strings.Contains(buf.String(), "\x07") {
				t.Errorf("expected a bell in the notification output, got %q", buf.String())
			}
		})
	}
}

// TestNotifyWrapperNilNotifierSafe confirms Notify does not panic when the
// backend notifier is absent.
func TestNotifyWrapperNilNotifierSafe(t *testing.T) {
	g := NewGogent(t.TempDir())
	g.notifier = nil
	g.Notify(string(notify.ReasonWatcher), "title", "body") // must not panic
}

// TestScheduleSummary covers the human-readable schedule rendering used in the
// ActionWatcher permission prompt and logs.
func TestScheduleSummary(t *testing.T) {
	for _, tc := range []struct {
		in   config.ScheduleConfig
		want string
	}{
		{config.ScheduleConfig{Every: "5m"}, "every 5m"},
		{config.ScheduleConfig{DailyAt: "07:00", Timezone: "Europe/Zurich"}, "daily 07:00 Europe/Zurich"},
		{config.ScheduleConfig{DailyAt: "07:00"}, "daily 07:00 UTC"},
		{config.ScheduleConfig{}, "no schedule"},
	} {
		if got := scheduleSummary(tc.in); got != tc.want {
			t.Errorf("scheduleSummary(%+v)=%q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestWatcherLaunchDetail confirms the permission-prompt detail names the watcher
// and summarizes its schedule.
func TestWatcherLaunchDetail(t *testing.T) {
	got := watcherLaunchDetail(config.WatcherConfig{Name: "daily", Schedule: config.ScheduleConfig{Every: "5m"}})
	if !strings.Contains(got, "daily") || !strings.Contains(got, "every 5m") {
		t.Errorf("watcherLaunchDetail = %q, want it to mention the name and schedule", got)
	}
}

// TestFirstLine covers the one-line result-summary helper used to record a fire's
// outcome: it returns the first non-empty trimmed line and truncates long lines.
func TestFirstLine(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want string
	}{
		{"", ""},
		{"   \n\t\n", ""},
		{"hello", "hello"},
		{"  hello  ", "hello"},
		{"\n\n  second line is first non-empty\nthird", "second line is first non-empty"},
	} {
		if got := firstLine(tc.in); got != tc.want {
			t.Errorf("firstLine(%q)=%q, want %q", tc.in, got, tc.want)
		}
	}

	// Long lines are truncated to 200 bytes.
	long := strings.Repeat("x", 500)
	if got := firstLine(long); len(got) != 200 {
		t.Errorf("firstLine of a 500-char line returned %d chars, want 200", len(got))
	}
}
