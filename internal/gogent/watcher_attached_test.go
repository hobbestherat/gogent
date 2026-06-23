package gogent

import (
	"sort"
	"testing"

	"gogent/internal/config"
	"gogent/internal/model"
	"gogent/internal/permission"
	"gogent/internal/tool"
	"gogent/internal/watcher"
)

// --- small helpers (the allow/enable helpers live in watcher_wiring_test.go) ---

// strptr returns a pointer to s; used to build a non-nil ReportToSession (attached
// watcher) in tests.
func strptr(s string) *string { return &s }

// startedWatcherGogent returns a Gogent with the experimental feature on, the
// ActionWatcher permission granted, and the watcher engine started (an empty
// manager). It is the common fixture for the attached-watcher lifecycle tests.
func startedWatcherGogent(t *testing.T) *Gogent {
	t.Helper()
	g := NewGogent(t.TempDir())
	enableWatchers(g)
	allowWatchers(g)
	g.StartWatchers()
	t.Cleanup(g.StopWatchers)
	if g.watchers == nil {
		t.Fatal("watcher engine should be running")
	}
	return g
}

// findWatcher returns the WatcherInfo with the given name from a snapshot, or
// ok=false if absent.
func findWatcher(infos []watcher.WatcherInfo, name string) (watcher.WatcherInfo, bool) {
	for _, info := range infos {
		if info.Name == name {
			return info, true
		}
	}
	return watcher.WatcherInfo{}, false
}

// storeHasWatcher reports whether ~/.gogent/watchers.json (free-running
// definitions) currently contains a watcher with the given name.
func storeHasWatcher(g *Gogent, name string) bool {
	for _, wc := range g.LoadWatchers().Items {
		if wc.Name == name {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// CreateWatcher kind decision
// ---------------------------------------------------------------------------

// TestCreateWatcherFreeRunningPersistedToStore covers the ReportToSession==nil
// branch: the watcher is KindFree, persisted to watchers.json (with no
// report_to_session), and visible to every session (including the empty scope).
func TestCreateWatcherFreeRunningPersistedToStore(t *testing.T) {
	g := startedWatcherGogent(t)

	info, err := g.CreateWatcher(config.WatcherConfig{
		Name:     "free-poll",
		Enabled:  true,
		Schedule: config.ScheduleConfig{Every: "1h"},
		Task:     "poll",
		// ReportToSession nil -> free-running.
	}, "")
	if err != nil {
		t.Fatalf("CreateWatcher (free): %v", err)
	}
	if info.Kind != watcher.KindFree {
		t.Errorf("kind = %v, want KindFree", info.Kind)
	}
	if info.ID == "" {
		t.Error("CreateWatcher must generate an id")
	}

	// Persisted to watchers.json.
	items := g.LoadWatchers().Items
	var stored *config.WatcherConfig
	for i := range items {
		if items[i].Name == "free-poll" {
			stored = &items[i]
		}
	}
	if stored == nil {
		t.Fatalf("free-running watcher must be persisted to watchers.json; got %+v", items)
	}
	if stored.ReportToSession != nil {
		t.Errorf("watchers.json must never carry report_to_session, got %q", *stored.ReportToSession)
	}
	if stored.ID != info.ID {
		t.Errorf("persisted id %q != created id %q", stored.ID, info.ID)
	}

	// Visible to the empty (global) scope; not recorded as an attached watcher.
	if _, ok := findWatcher(g.ListWatchers(""), "free-poll"); !ok {
		t.Error("free-running watcher should be visible at the empty scope")
	}
	if got := g.attachedWatchersFor("anything"); got != nil {
		t.Errorf("free-running watcher must not be recorded as attached, got %+v", got)
	}
}

// TestCreateWatcherAttachedStoredWithSession covers the ReportToSession!=nil
// branch: the watcher is KindAttached, owned by its session, NOT in watchers.json,
// and visible only to its owning session.
func TestCreateWatcherAttachedStoredWithSession(t *testing.T) {
	g := startedWatcherGogent(t)
	const sid = "sess-attached"
	g.NewSession(sid)

	info, err := g.CreateWatcher(config.WatcherConfig{
		Name:            "issue-poller",
		Enabled:         true,
		Schedule:        config.ScheduleConfig{Every: "5m"},
		Task:            "triage issues",
		ReportToSession: strptr(sid),
	}, sid)
	if err != nil {
		t.Fatalf("CreateWatcher (attached): %v", err)
	}
	if info.Kind != watcher.KindAttached {
		t.Errorf("kind = %v, want KindAttached", info.Kind)
	}
	if info.TargetSession != sid {
		t.Errorf("target session = %q, want %q", info.TargetSession, sid)
	}

	// NOT in watchers.json.
	if storeHasWatcher(g, "issue-poller") {
		t.Error("attached watcher must NOT be written to watchers.json")
	}
	// Recorded as an attached watcher of its session.
	att := g.attachedWatchersFor(sid)
	if len(att) != 1 || att[0].Name != "issue-poller" {
		t.Fatalf("attachedWatchersFor(%q) = %+v, want the one attached watcher", sid, att)
	}
	if att[0].ReportToSession == nil || *att[0].ReportToSession != sid {
		t.Errorf("stored attached config must keep report_to_session = %q, got %+v", sid, att[0].ReportToSession)
	}

	// Visibility scoping.
	if _, ok := findWatcher(g.ListWatchers(sid), "issue-poller"); !ok {
		t.Error("owning session must see its attached watcher")
	}
	if _, ok := findWatcher(g.ListWatchers(""), "issue-poller"); ok {
		t.Error("empty scope must NOT see an attached watcher")
	}
	if _, ok := findWatcher(g.ListWatchers("some-other-session"), "issue-poller"); ok {
		t.Error("another session must NOT see this session's attached watcher")
	}
}

// TestCreateWatcherAttachedEmptyReportFallsBackToCaller covers the defensive
// branch where ReportToSession is a non-nil empty string: the watcher attaches to
// the calling sessionID instead.
func TestCreateWatcherAttachedEmptyReportFallsBackToCaller(t *testing.T) {
	g := startedWatcherGogent(t)
	const sid = "sess-caller"
	g.NewSession(sid)

	info, err := g.CreateWatcher(config.WatcherConfig{
		Name:            "fallback",
		Enabled:         true,
		Schedule:        config.ScheduleConfig{Every: "30s"},
		Task:            "t",
		ReportToSession: strptr("   "), // non-nil but blank
	}, sid)
	if err != nil {
		t.Fatalf("CreateWatcher: %v", err)
	}
	if info.Kind != watcher.KindAttached || info.TargetSession != sid {
		t.Errorf("blank report_to_session should attach to the caller %q, got kind=%v target=%q", sid, info.Kind, info.TargetSession)
	}
}

// TestCreateWatcherAttachedNoOwningSessionErrors covers the error path: an
// attached watcher with neither an explicit target nor a calling session has
// nowhere to report.
func TestCreateWatcherAttachedNoOwningSessionErrors(t *testing.T) {
	g := startedWatcherGogent(t)
	_, err := g.CreateWatcher(config.WatcherConfig{
		Name:            "orphan",
		Enabled:         true,
		Schedule:        config.ScheduleConfig{Every: "1m"},
		Task:            "t",
		ReportToSession: strptr(""),
	}, "")
	if err == nil {
		t.Fatal("attached watcher with no owning session should error")
	}
}

// TestCreateWatcherEngineNotRunningErrors confirms CreateWatcher refuses to act
// before StartWatchers built the manager.
func TestCreateWatcherEngineNotRunningErrors(t *testing.T) {
	g := NewGogent(t.TempDir())
	enableWatchers(g)
	allowWatchers(g)
	// Deliberately NOT calling StartWatchers.
	if _, err := g.CreateWatcher(config.WatcherConfig{
		Name: "x", Enabled: true, Schedule: config.ScheduleConfig{Every: "1m"}, Task: "t",
	}, ""); err == nil {
		t.Fatal("CreateWatcher should fail when the engine is not running")
	}
}

// TestCreateWatcherInvalidScheduleErrors confirms a malformed schedule is rejected
// (and nothing is persisted).
func TestCreateWatcherInvalidScheduleErrors(t *testing.T) {
	g := startedWatcherGogent(t)
	if _, err := g.CreateWatcher(config.WatcherConfig{
		Name: "bad", Enabled: true, Schedule: config.ScheduleConfig{Every: "5m", DailyAt: "07:00"}, Task: "t",
	}, ""); err == nil {
		t.Fatal("CreateWatcher should reject a schedule with both every and daily_at")
	}
	if storeHasWatcher(g, "bad") {
		t.Error("an invalid watcher must not be persisted")
	}
}

// TestCreateWatcherDeniedByPermission confirms an explicit deny rule blocks
// creation (and nothing is persisted).
func TestCreateWatcherDeniedByPermission(t *testing.T) {
	g := NewGogent(t.TempDir())
	enableWatchers(g)
	g.GetPermissionService().AddRule(permission.Rule{
		Action: string(permission.ActionWatcher), Resource: "*", Effect: string(permission.EffectDeny),
	})
	g.StartWatchers()
	t.Cleanup(g.StopWatchers)

	if _, err := g.CreateWatcher(config.WatcherConfig{
		Name: "denied", Enabled: true, Schedule: config.ScheduleConfig{Every: "1m"}, Task: "t",
	}, ""); err == nil {
		t.Fatal("CreateWatcher should be denied without permission")
	}
	if storeHasWatcher(g, "denied") {
		t.Error("a denied watcher must not be persisted")
	}
}

// ---------------------------------------------------------------------------
// OnSessionClosed
// ---------------------------------------------------------------------------

// TestOnSessionClosedRemovesOnlyThatSessionsAttached creates attached watchers in
// two sessions plus one free-running watcher, then closes one session. Only that
// session's attached watcher is removed; the other session's and the free-running
// one survive.
func TestOnSessionClosedRemovesOnlyThatSessionsAttached(t *testing.T) {
	g := startedWatcherGogent(t)
	const sa, sb = "sess-A", "sess-B"
	g.NewSession(sa)
	g.NewSession(sb)

	mustCreate := func(name, owner string, report *string) {
		t.Helper()
		if _, err := g.CreateWatcher(config.WatcherConfig{
			Name: name, Enabled: true, Schedule: config.ScheduleConfig{Every: "1h"}, Task: "t", ReportToSession: report,
		}, owner); err != nil {
			t.Fatalf("CreateWatcher %q: %v", name, err)
		}
	}
	mustCreate("watch-A", sa, strptr(sa))
	mustCreate("watch-B", sb, strptr(sb))
	mustCreate("watch-free", "", nil)

	g.OnSessionClosed(sa)

	if _, ok := findWatcher(g.ListWatchers(sa), "watch-A"); ok {
		t.Error("closed session's attached watcher should be gone")
	}
	if g.attachedWatchersFor(sa) != nil {
		t.Error("closed session's attached config record should be dropped")
	}
	if _, ok := findWatcher(g.ListWatchers(sb), "watch-B"); !ok {
		t.Error("other session's attached watcher must survive")
	}
	if _, ok := findWatcher(g.ListWatchers(""), "watch-free"); !ok {
		t.Error("free-running watcher must survive a session close")
	}
}

// TestRemoveSessionTearsDownAttachedWatchers exercises the real wiring:
// RemoveSession (the session-close path) must invoke OnSessionClosed so the
// session's attached watcher is torn down.
func TestRemoveSessionTearsDownAttachedWatchers(t *testing.T) {
	g := startedWatcherGogent(t)
	const sid = "sess-remove"
	g.NewSession(sid)
	if _, err := g.CreateWatcher(config.WatcherConfig{
		Name: "doomed", Enabled: true, Schedule: config.ScheduleConfig{Every: "1h"}, Task: "t", ReportToSession: strptr(sid),
	}, sid); err != nil {
		t.Fatalf("CreateWatcher: %v", err)
	}

	g.RemoveSession(sid)

	if _, ok := findWatcher(g.ListWatchers(sid), "doomed"); ok {
		t.Error("RemoveSession must tear down the session's attached watchers")
	}
	if g.attachedWatchersFor(sid) != nil {
		t.Error("RemoveSession must drop the session's attached config records")
	}
}

// ---------------------------------------------------------------------------
// Round-trip: attached watcher serialized with the session, restored + re-registered
// ---------------------------------------------------------------------------

// roundTripAttached persists a session carrying one attached watcher, then returns
// a fresh Gogent on the same home with the feature on + permission granted (the
// caller chooses the StartWatchers / RestoreSessions order).
func roundTripAttached(t *testing.T, home, sid string) *Gogent {
	t.Helper()
	g := NewGogent(home)
	enableWatchers(g)
	allowWatchers(g)
	g.StartWatchers()

	us := g.NewSession(sid)
	if us == nil {
		t.Fatal("NewSession returned nil")
	}
	// Seed a transcript so the session has content to persist alongside the watcher.
	us.RootAgent.ThoughtTrain.AppendMessages(model.Message{Role: model.RoleUser, Content: "hello"})

	if _, err := g.CreateWatcher(config.WatcherConfig{
		Name:            "persisted-poller",
		Enabled:         true,
		Schedule:        config.ScheduleConfig{Every: "5m"},
		Task:            "remembered task",
		ReportToSession: strptr(sid),
	}, sid); err != nil {
		t.Fatalf("CreateWatcher: %v", err)
	}
	// CreateWatcher persists the owning session; force the flush to disk.
	g.persistSession(sid)
	if g.store != nil {
		g.store.Sync()
	}
	g.StopWatchers()

	return NewGogent(home)
}

// TestAttachedWatcherRoundTripStartThenRestore restores with the engine already
// running: OnSessionRestored registers the runner directly.
func TestAttachedWatcherRoundTripStartThenRestore(t *testing.T) {
	home := t.TempDir()
	const sid = "sess-roundtrip-1"
	g2 := roundTripAttached(t, home, sid)
	enableWatchers(g2)
	allowWatchers(g2)
	g2.StartWatchers()
	t.Cleanup(g2.StopWatchers)
	g2.RestoreSessions()

	assertRestoredAttached(t, g2, sid)
}

// TestAttachedWatcherRoundTripRestoreThenStart restores BEFORE the engine starts:
// OnSessionRestored only records the configs (manager is nil), and StartWatchers
// must then pick up the pending attached watchers.
func TestAttachedWatcherRoundTripRestoreThenStart(t *testing.T) {
	home := t.TempDir()
	const sid = "sess-roundtrip-2"
	g2 := roundTripAttached(t, home, sid)
	enableWatchers(g2)
	allowWatchers(g2)
	g2.RestoreSessions() // engine not running yet
	if _, ok := findWatcher(g2.ListWatchers(sid), "persisted-poller"); ok {
		t.Fatal("watcher must not appear before StartWatchers builds the manager")
	}
	g2.StartWatchers()
	t.Cleanup(g2.StopWatchers)

	assertRestoredAttached(t, g2, sid)
}

// assertRestoredAttached checks the round-tripped watcher came back as an attached
// watcher of sid with its fields intact, and did not leak into watchers.json.
func assertRestoredAttached(t *testing.T, g *Gogent, sid string) {
	t.Helper()
	info, ok := findWatcher(g.ListWatchers(sid), "persisted-poller")
	if !ok {
		t.Fatalf("attached watcher was not re-registered for session %q; got %+v", sid, g.ListWatchers(sid))
	}
	if info.Kind != watcher.KindAttached {
		t.Errorf("restored watcher kind = %v, want KindAttached", info.Kind)
	}
	if info.TargetSession != sid {
		t.Errorf("restored target = %q, want %q", info.TargetSession, sid)
	}
	if !info.Enabled {
		t.Error("restored watcher should be enabled")
	}
	// Must remain private to the session and absent from the free-running store.
	if _, ok := findWatcher(g.ListWatchers(""), "persisted-poller"); ok {
		t.Error("restored attached watcher must not be visible at the empty scope")
	}
	if storeHasWatcher(g, "persisted-poller") {
		t.Error("restored attached watcher must not leak into watchers.json")
	}
}

// ---------------------------------------------------------------------------
// ListWatchers scoping (union of free-running + the caller's own attached)
// ---------------------------------------------------------------------------

// TestListWatchersUnionScoping builds a mixed set (two free-running + one attached
// per session) and asserts each session sees exactly the free-running watchers
// plus its own attached one.
func TestListWatchersUnionScoping(t *testing.T) {
	g := startedWatcherGogent(t)
	const sa, sb = "scope-A", "scope-B"
	g.NewSession(sa)
	g.NewSession(sb)

	mk := func(name, owner string, report *string) {
		t.Helper()
		if _, err := g.CreateWatcher(config.WatcherConfig{
			Name: name, Enabled: true, Schedule: config.ScheduleConfig{Every: "1h"}, Task: "t", ReportToSession: report,
		}, owner); err != nil {
			t.Fatalf("CreateWatcher %q: %v", name, err)
		}
	}
	mk("free-1", "", nil)
	mk("free-2", "", nil)
	mk("att-A", sa, strptr(sa))
	mk("att-B", sb, strptr(sb))

	names := func(infos []watcher.WatcherInfo) []string {
		out := make([]string, 0, len(infos))
		for _, i := range infos {
			out = append(out, i.Name)
		}
		sort.Strings(out)
		return out
	}

	gotA := names(g.ListWatchers(sa))
	wantA := []string{"att-A", "free-1", "free-2"}
	if !equalStrings(gotA, wantA) {
		t.Errorf("ListWatchers(%q) = %v, want %v", sa, gotA, wantA)
	}
	gotB := names(g.ListWatchers(sb))
	wantB := []string{"att-B", "free-1", "free-2"}
	if !equalStrings(gotB, wantB) {
		t.Errorf("ListWatchers(%q) = %v, want %v", sb, gotB, wantB)
	}
	gotEmpty := names(g.ListWatchers(""))
	wantEmpty := []string{"free-1", "free-2"}
	if !equalStrings(gotEmpty, wantEmpty) {
		t.Errorf("ListWatchers(\"\") = %v, want %v (free-running only)", gotEmpty, wantEmpty)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// ---------------------------------------------------------------------------
// Tool registration gating (Experimental.Watchers)
// ---------------------------------------------------------------------------

// watcherToolNames are the six agent-facing watcher tools registered by
// RegisterWatcherTools.
var watcherToolNames = []string{
	"create_watcher", "list_watchers", "update_watcher",
	"enable_watcher", "disable_watcher", "delete_watcher",
}

// TestWatcherToolsAbsentWhenExperimentalOff confirms the default (feature off)
// registers none of the watcher tools.
func TestWatcherToolsAbsentWhenExperimentalOff(t *testing.T) {
	g := NewGogent(t.TempDir())
	for _, name := range watcherToolNames {
		if g.toolRegistry.Get(name) != nil {
			t.Errorf("tool %q must NOT be registered when Experimental.Watchers is off", name)
		}
	}
}

// TestWatcherToolsRegisteredWhenExperimentalOn writes a config with the feature on
// so initializeToolRegistry registers all six watcher tools at construction.
func TestWatcherToolsRegisteredWhenExperimentalOn(t *testing.T) {
	home := t.TempDir()
	cfg := config.GetDefaultConfig()
	cfg.Experimental.Watchers = true
	if err := config.SaveConfig(home, cfg); err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}

	g := NewGogent(home)
	for _, name := range watcherToolNames {
		tl := g.toolRegistry.Get(name)
		if tl == nil {
			t.Errorf("tool %q must be registered when Experimental.Watchers is on", name)
			continue
		}
		// list_watchers is the only read-only watcher tool; the rest mutate.
		wantReadOnly := name == "list_watchers"
		if tl.ReadOnly != wantReadOnly {
			t.Errorf("tool %q ReadOnly = %v, want %v", name, tl.ReadOnly, wantReadOnly)
		}
	}
}

// TestWatcherToolsPropagateToExistingSessionOnEnable confirms enabling the feature
// at runtime (enableWatcherTools) propagates the tools into an already-running
// session's registry via refreshSessionRegistries.
func TestWatcherToolsPropagateToExistingSessionOnEnable(t *testing.T) {
	g := NewGogent(t.TempDir())
	const sid = "live-session"
	us := g.NewSession(sid)
	if us == nil || us.RootAgent == nil {
		t.Fatal("failed to create session")
	}
	// Before enabling, the live session must not see the watcher tools.
	if us.RootAgent.ToolRegistry != nil && us.RootAgent.ToolRegistry.Get("create_watcher") != nil {
		t.Fatal("session should not have create_watcher before the feature is enabled")
	}

	enableWatchers(g)
	g.enableWatcherTools() // idempotent runtime enable
	g.enableWatcherTools() // second call must not panic or double-fault

	if us.RootAgent.ToolRegistry == nil || us.RootAgent.ToolRegistry.Get("create_watcher") == nil {
		t.Error("enabling watchers at runtime must propagate create_watcher into the live session")
	}
}

// ---------------------------------------------------------------------------
// create_watcher tool default report_to_session (via the real Gogent controller)
// ---------------------------------------------------------------------------

// TestCreateWatcherToolDefaultsToCallingSession drives the registered
// create_watcher tool with no report_to_session argument and asserts the resulting
// watcher is attached to ctx.SessionID.
func TestCreateWatcherToolDefaultsToCallingSession(t *testing.T) {
	home := t.TempDir()
	cfg := config.GetDefaultConfig()
	cfg.Experimental.Watchers = true
	if err := config.SaveConfig(home, cfg); err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}
	g := NewGogent(home)
	allowWatchers(g)
	g.StartWatchers()
	t.Cleanup(g.StopWatchers)

	const sid = "conv-session"
	g.NewSession(sid)

	tl := g.toolRegistry.Get("create_watcher")
	if tl == nil {
		t.Fatal("create_watcher tool not registered")
	}
	res, err := tl.Execute(map[string]interface{}{
		"name":     "conv-watcher",
		"schedule": map[string]interface{}{"every": "5m"},
		"task":     "watch the repo",
		// no report_to_session -> default to the calling session
	}, tool.ToolContext{SessionID: sid})
	if err != nil {
		t.Fatalf("create_watcher Execute: %v", err)
	}
	m, ok := res.(map[string]interface{})
	if !ok {
		t.Fatalf("create_watcher returned %T, want map", res)
	}
	if m["target"] != sid {
		t.Errorf("default target = %v, want %q (the calling session)", m["target"], sid)
	}

	// It must be an attached watcher private to the session and not in watchers.json.
	if _, ok := findWatcher(g.ListWatchers(sid), "conv-watcher"); !ok {
		t.Error("default create_watcher should attach to the calling session")
	}
	if _, ok := findWatcher(g.ListWatchers(""), "conv-watcher"); ok {
		t.Error("a default (attached) watcher must not be globally visible")
	}
	if storeHasWatcher(g, "conv-watcher") {
		t.Error("a default (attached) watcher must not be persisted to watchers.json")
	}
}

// TestCreateWatcherToolExplicitNullIsFreeRunning drives create_watcher with an
// explicit null report_to_session and asserts a free-running watcher results.
func TestCreateWatcherToolExplicitNullIsFreeRunning(t *testing.T) {
	home := t.TempDir()
	cfg := config.GetDefaultConfig()
	cfg.Experimental.Watchers = true
	if err := config.SaveConfig(home, cfg); err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}
	g := NewGogent(home)
	allowWatchers(g)
	g.StartWatchers()
	t.Cleanup(g.StopWatchers)

	const sid = "conv-session-2"
	g.NewSession(sid)

	tl := g.toolRegistry.Get("create_watcher")
	res, err := tl.Execute(map[string]interface{}{
		"name":              "global-watcher",
		"schedule":          map[string]interface{}{"every": "1h"},
		"task":              "run globally",
		"report_to_session": nil, // explicit null -> free-running
	}, tool.ToolContext{SessionID: sid})
	if err != nil {
		t.Fatalf("create_watcher Execute: %v", err)
	}
	m := res.(map[string]interface{})
	if m["target"] != "free" {
		t.Errorf("explicit null target = %v, want \"free\"", m["target"])
	}
	if !storeHasWatcher(g, "global-watcher") {
		t.Error("a free-running watcher must be persisted to watchers.json")
	}
	if _, ok := findWatcher(g.ListWatchers(""), "global-watcher"); !ok {
		t.Error("a free-running watcher must be globally visible")
	}
}

// TestListWatchersToolScoping drives the registered list_watchers tool and asserts
// it returns the free-running watchers plus only the calling session's attached
// ones.
func TestListWatchersToolScoping(t *testing.T) {
	home := t.TempDir()
	cfg := config.GetDefaultConfig()
	cfg.Experimental.Watchers = true
	if err := config.SaveConfig(home, cfg); err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}
	g := NewGogent(home)
	allowWatchers(g)
	g.StartWatchers()
	t.Cleanup(g.StopWatchers)

	const sa, sb = "list-A", "list-B"
	g.NewSession(sa)
	g.NewSession(sb)
	mk := func(name, owner string, report *string) {
		t.Helper()
		if _, err := g.CreateWatcher(config.WatcherConfig{
			Name: name, Enabled: true, Schedule: config.ScheduleConfig{Every: "1h"}, Task: "t", ReportToSession: report,
		}, owner); err != nil {
			t.Fatalf("CreateWatcher %q: %v", name, err)
		}
	}
	mk("lfree", "", nil)
	mk("latt-A", sa, strptr(sa))
	mk("latt-B", sb, strptr(sb))

	tl := g.toolRegistry.Get("list_watchers")
	res, err := tl.Execute(map[string]interface{}{}, tool.ToolContext{SessionID: sa})
	if err != nil {
		t.Fatalf("list_watchers Execute: %v", err)
	}
	m := res.(map[string]interface{})
	list, _ := m["watchers"].([]map[string]interface{})
	got := map[string]bool{}
	for _, w := range list {
		got[w["name"].(string)] = true
	}
	if !got["lfree"] {
		t.Error("list_watchers must include free-running watchers")
	}
	if !got["latt-A"] {
		t.Error("list_watchers must include the calling session's attached watcher")
	}
	if got["latt-B"] {
		t.Error("list_watchers must NOT include another session's attached watcher")
	}
	if n, _ := m["count"].(int); n != len(list) {
		t.Errorf("count = %d, want %d (len of list)", n, len(list))
	}
}
