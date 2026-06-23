package tool

import (
	"errors"
	"testing"
	"time"

	"gogent/internal/config"
	"gogent/internal/watcher"
)

// fakeWatcherController is a recording stand-in for the gogent backend so the
// watcher tool closures can be tested in isolation (argument marshalling,
// defaulting, output shape, error propagation).
type fakeWatcherController struct {
	createCfg     config.WatcherConfig
	createSession string
	createInfo    watcher.WatcherInfo
	createErr     error

	updatePatch   config.WatcherConfig
	updateSession string
	updateInfo    watcher.WatcherInfo
	updateErr     error

	deleted   string
	deleteErr error

	enabledID  string
	enabledVal bool
	enableErr  error

	listSession string
	listInfos   []watcher.WatcherInfo
}

func (f *fakeWatcherController) CreateWatcher(cfg config.WatcherConfig, sessionID string) (watcher.WatcherInfo, error) {
	f.createCfg = cfg
	f.createSession = sessionID
	if f.createErr != nil {
		return watcher.WatcherInfo{}, f.createErr
	}
	info := f.createInfo
	if info.Name == "" {
		info.Name = cfg.Name
	}
	if info.ID == "" {
		info.ID = "id-" + cfg.Name
	}
	return info, nil
}

func (f *fakeWatcherController) UpdateWatcher(patch config.WatcherConfig, sessionID string) (watcher.WatcherInfo, error) {
	f.updatePatch = patch
	f.updateSession = sessionID
	return f.updateInfo, f.updateErr
}

func (f *fakeWatcherController) DeleteWatcher(idOrName string) error {
	f.deleted = idOrName
	return f.deleteErr
}

func (f *fakeWatcherController) SetWatcherEnabled(idOrName string, enabled bool) error {
	f.enabledID = idOrName
	f.enabledVal = enabled
	return f.enableErr
}

func (f *fakeWatcherController) ListWatchers(sessionID string) []watcher.WatcherInfo {
	f.listSession = sessionID
	return f.listInfos
}

// registerInto builds a registry with the watcher tools registered over ctrl and
// returns it.
func registerInto(ctrl WatcherController) *ToolRegistry {
	tr := NewToolRegistry()
	tr.RegisterWatcherTools(ctrl)
	return tr
}

// --- registration & ReadOnly flags -----------------------------------------

func TestRegisterWatcherToolsRegistersAllSix(t *testing.T) {
	tr := registerInto(&fakeWatcherController{})
	want := map[string]bool{ // name -> expected ReadOnly
		"create_watcher":  false,
		"list_watchers":   true,
		"update_watcher":  false,
		"enable_watcher":  false,
		"disable_watcher": false,
		"delete_watcher":  false,
	}
	for name, ro := range want {
		tl := tr.Get(name)
		if tl == nil {
			t.Errorf("tool %q not registered", name)
			continue
		}
		if tl.ReadOnly != ro {
			t.Errorf("tool %q ReadOnly = %v, want %v", name, tl.ReadOnly, ro)
		}
	}
}

// --- reportToSession defaulting --------------------------------------------

func TestReportToSession(t *testing.T) {
	const caller = "sess-current"
	for _, tc := range []struct {
		name string
		args map[string]interface{}
		want *string // nil = free-running
	}{
		{"absent defaults to caller", map[string]interface{}{}, strp(caller)},
		{"explicit null -> free", map[string]interface{}{"report_to_session": nil}, nil},
		{"empty string -> free", map[string]interface{}{"report_to_session": ""}, nil},
		{"whitespace -> free", map[string]interface{}{"report_to_session": "   "}, nil},
		{"independent -> free", map[string]interface{}{"report_to_session": "independent"}, nil},
		{"INDEPENDENT case-insensitive -> free", map[string]interface{}{"report_to_session": "INDEPENDENT"}, nil},
		{"explicit other session -> attached", map[string]interface{}{"report_to_session": "sess-other"}, strp("sess-other")},
		{"non-string -> free", map[string]interface{}{"report_to_session": 42}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := reportToSession(tc.args, caller)
			switch {
			case tc.want == nil && got != nil:
				t.Errorf("got %q, want nil (free-running)", *got)
			case tc.want != nil && got == nil:
				t.Errorf("got nil, want %q", *tc.want)
			case tc.want != nil && got != nil && *got != *tc.want:
				t.Errorf("got %q, want %q", *got, *tc.want)
			}
		})
	}
}

// TestReportToSessionAbsentWithNoCaller covers the corner where the key is absent
// AND there is no calling session: there is nothing to attach to, so it must
// resolve to free-running (nil).
func TestReportToSessionAbsentWithNoCaller(t *testing.T) {
	if got := reportToSession(map[string]interface{}{}, ""); got != nil {
		t.Errorf("absent key + empty caller should be free-running, got %q", *got)
	}
}

func strp(s string) *string { return &s }

// --- scheduleFromArg --------------------------------------------------------

func TestScheduleFromArg(t *testing.T) {
	got, err := scheduleFromArg(map[string]interface{}{
		"every": " 5m ", "daily_at": "", "timezone": " Europe/Zurich ",
	})
	if err != nil {
		t.Fatalf("scheduleFromArg: %v", err)
	}
	if got.Every != "5m" || got.Timezone != "Europe/Zurich" {
		t.Errorf("scheduleFromArg trimmed = %+v, want every=5m timezone=Europe/Zurich", got)
	}

	if _, err := scheduleFromArg("not-an-object"); err == nil {
		t.Error("scheduleFromArg must reject a non-object argument")
	}
	if _, err := scheduleFromArg(nil); err == nil {
		t.Error("scheduleFromArg must reject a nil argument")
	}
}

// --- create_watcher closure -------------------------------------------------

func TestCreateWatcherToolDefaultsAndArgs(t *testing.T) {
	ctrl := &fakeWatcherController{createInfo: watcher.WatcherInfo{ID: "w1", Name: "n", Kind: watcher.KindAttached, TargetSession: "sess-x"}}
	tr := registerInto(ctrl)

	res, err := tr.Get("create_watcher").Execute(map[string]interface{}{
		"name":     "n",
		"schedule": map[string]interface{}{"every": "5m"},
		"task":     "do it",
	}, ToolContext{SessionID: "sess-x"})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	// Defaulted to the calling session.
	if ctrl.createCfg.ReportToSession == nil || *ctrl.createCfg.ReportToSession != "sess-x" {
		t.Errorf("ReportToSession = %v, want sess-x (defaulted)", ctrl.createCfg.ReportToSession)
	}
	if ctrl.createSession != "sess-x" {
		t.Errorf("sessionID passed = %q, want sess-x", ctrl.createSession)
	}
	// enabled defaults to true.
	if !ctrl.createCfg.Enabled {
		t.Error("enabled should default to true")
	}
	if ctrl.createCfg.Task != "do it" || ctrl.createCfg.Schedule.Every != "5m" {
		t.Errorf("cfg not marshalled: %+v", ctrl.createCfg)
	}
	m := res.(map[string]interface{})
	if m["target"] != "sess-x" {
		t.Errorf("result target = %v, want sess-x", m["target"])
	}
}

func TestCreateWatcherToolEnabledFalseHonored(t *testing.T) {
	ctrl := &fakeWatcherController{}
	tr := registerInto(ctrl)
	_, err := tr.Get("create_watcher").Execute(map[string]interface{}{
		"name":     "n",
		"schedule": map[string]interface{}{"every": "5m"},
		"task":     "t",
		"enabled":  false,
	}, ToolContext{SessionID: "s"})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if ctrl.createCfg.Enabled {
		t.Error("enabled=false must be honored, not overridden by the default")
	}
}

func TestCreateWatcherToolValidatesRequiredArgs(t *testing.T) {
	ctrl := &fakeWatcherController{}
	tr := registerInto(ctrl)
	tl := tr.Get("create_watcher")

	// Missing name.
	if _, err := tl.Execute(map[string]interface{}{
		"schedule": map[string]interface{}{"every": "5m"}, "task": "t",
	}, ToolContext{SessionID: "s"}); err == nil {
		t.Error("missing name should error")
	}
	// Missing task.
	if _, err := tl.Execute(map[string]interface{}{
		"name": "n", "schedule": map[string]interface{}{"every": "5m"},
	}, ToolContext{SessionID: "s"}); err == nil {
		t.Error("missing task should error")
	}
	// Blank (whitespace) name.
	if _, err := tl.Execute(map[string]interface{}{
		"name": "   ", "schedule": map[string]interface{}{"every": "5m"}, "task": "t",
	}, ToolContext{SessionID: "s"}); err == nil {
		t.Error("blank name should error")
	}
	// The controller must never have been called when validation fails.
	if ctrl.createCfg.Name != "" {
		t.Error("controller should not be invoked on invalid args")
	}
}

func TestCreateWatcherToolPropagatesControllerError(t *testing.T) {
	ctrl := &fakeWatcherController{createErr: errors.New("boom")}
	tr := registerInto(ctrl)
	_, err := tr.Get("create_watcher").Execute(map[string]interface{}{
		"name": "n", "schedule": map[string]interface{}{"every": "5m"}, "task": "t",
	}, ToolContext{SessionID: "s"})
	if err == nil {
		t.Fatal("controller error should surface")
	}
}

// --- list_watchers closure --------------------------------------------------

func TestListWatchersToolOutputShape(t *testing.T) {
	now := time.Date(2026, 6, 23, 7, 0, 0, 0, time.UTC)
	ctrl := &fakeWatcherController{listInfos: []watcher.WatcherInfo{
		{ID: "f1", Name: "free", Kind: watcher.KindFree, Enabled: true, Status: watcher.StatusIdle, NextFire: now},
		{ID: "a1", Name: "att", Kind: watcher.KindAttached, TargetSession: "sess-z", Enabled: false, Status: watcher.StatusRunning, LastRun: now, LastResult: "ok", LastError: "prev failure"},
	}}
	tr := registerInto(ctrl)

	res, err := tr.Get("list_watchers").Execute(map[string]interface{}{}, ToolContext{SessionID: "sess-z"})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if ctrl.listSession != "sess-z" {
		t.Errorf("ListWatchers called with session %q, want sess-z", ctrl.listSession)
	}
	m := res.(map[string]interface{})
	if m["count"] != 2 {
		t.Errorf("count = %v, want 2", m["count"])
	}
	list := m["watchers"].([]map[string]interface{})
	if len(list) != 2 {
		t.Fatalf("got %d entries, want 2", len(list))
	}

	free := list[0]
	if free["target"] != "free" {
		t.Errorf("free-running target = %v, want \"free\"", free["target"])
	}
	if free["next_fire"] != now.Format(time.RFC3339) {
		t.Errorf("next_fire = %v, want %v", free["next_fire"], now.Format(time.RFC3339))
	}
	if _, present := free["last_run"]; present {
		t.Error("zero last_run should be omitted")
	}
	if _, present := free["last_error"]; present {
		t.Error("empty last_error should be omitted")
	}

	att := list[1]
	if att["target"] != "sess-z" {
		t.Errorf("attached target = %v, want sess-z", att["target"])
	}
	if att["status"] != "running" {
		t.Errorf("status = %v, want running", att["status"])
	}
	if att["last_result"] != "ok" || att["last_error"] != "prev failure" {
		t.Errorf("last_result/last_error not surfaced: %+v", att)
	}
	if _, present := att["next_fire"]; present {
		t.Error("zero next_fire should be omitted")
	}
}

func TestListWatchersToolEmpty(t *testing.T) {
	tr := registerInto(&fakeWatcherController{})
	res, err := tr.Get("list_watchers").Execute(map[string]interface{}{}, ToolContext{SessionID: "s"})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	m := res.(map[string]interface{})
	if m["count"] != 0 {
		t.Errorf("count = %v, want 0", m["count"])
	}
	if list := m["watchers"].([]map[string]interface{}); len(list) != 0 {
		t.Errorf("watchers = %v, want empty", list)
	}
}

// --- update_watcher closure -------------------------------------------------

func TestUpdateWatcherToolPatch(t *testing.T) {
	ctrl := &fakeWatcherController{updateInfo: watcher.WatcherInfo{ID: "w1", Name: "renamed", Kind: watcher.KindFree}}
	tr := registerInto(ctrl)

	_, err := tr.Get("update_watcher").Execute(map[string]interface{}{
		"watcher":  "old-name",
		"name":     "renamed",
		"task":     "new task",
		"schedule": map[string]interface{}{"daily_at": "08:00", "timezone": "UTC"},
	}, ToolContext{SessionID: "s"})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if ctrl.updatePatch.ID != "old-name" {
		t.Errorf("patch.ID (lookup key) = %q, want old-name", ctrl.updatePatch.ID)
	}
	if ctrl.updatePatch.Name != "renamed" || ctrl.updatePatch.Task != "new task" {
		t.Errorf("patch fields not marshalled: %+v", ctrl.updatePatch)
	}
	if ctrl.updatePatch.Schedule.DailyAt != "08:00" {
		t.Errorf("patch schedule = %+v, want daily_at 08:00", ctrl.updatePatch.Schedule)
	}
}

func TestUpdateWatcherToolOmittedScheduleStaysZero(t *testing.T) {
	ctrl := &fakeWatcherController{}
	tr := registerInto(ctrl)
	if _, err := tr.Get("update_watcher").Execute(map[string]interface{}{
		"watcher": "w", "task": "only task",
	}, ToolContext{SessionID: "s"}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if ctrl.updatePatch.Schedule != (config.ScheduleConfig{}) {
		t.Errorf("omitted schedule should stay zero, got %+v", ctrl.updatePatch.Schedule)
	}
}

func TestUpdateWatcherToolRequiresWatcher(t *testing.T) {
	tr := registerInto(&fakeWatcherController{})
	if _, err := tr.Get("update_watcher").Execute(map[string]interface{}{
		"task": "t",
	}, ToolContext{SessionID: "s"}); err == nil {
		t.Error("update_watcher must require the watcher (name or id) argument")
	}
}

// --- enable / disable closures ----------------------------------------------

func TestEnableDisableWatcherTools(t *testing.T) {
	ctrl := &fakeWatcherController{}
	tr := registerInto(ctrl)

	if _, err := tr.Get("enable_watcher").Execute(map[string]interface{}{"watcher": "w"}, ToolContext{SessionID: "s"}); err != nil {
		t.Fatalf("enable Execute: %v", err)
	}
	if ctrl.enabledID != "w" || ctrl.enabledVal != true {
		t.Errorf("enable_watcher called with (%q,%v), want (w,true)", ctrl.enabledID, ctrl.enabledVal)
	}

	if _, err := tr.Get("disable_watcher").Execute(map[string]interface{}{"watcher": "w"}, ToolContext{SessionID: "s"}); err != nil {
		t.Fatalf("disable Execute: %v", err)
	}
	if ctrl.enabledID != "w" || ctrl.enabledVal != false {
		t.Errorf("disable_watcher called with (%q,%v), want (w,false)", ctrl.enabledID, ctrl.enabledVal)
	}
}

func TestEnableWatcherToolRequiresArg(t *testing.T) {
	tr := registerInto(&fakeWatcherController{})
	if _, err := tr.Get("enable_watcher").Execute(map[string]interface{}{}, ToolContext{SessionID: "s"}); err == nil {
		t.Error("enable_watcher must require the watcher argument")
	}
}

func TestEnableWatcherToolPropagatesError(t *testing.T) {
	ctrl := &fakeWatcherController{enableErr: errors.New("nope")}
	tr := registerInto(ctrl)
	if _, err := tr.Get("disable_watcher").Execute(map[string]interface{}{"watcher": "w"}, ToolContext{SessionID: "s"}); err == nil {
		t.Error("controller error should surface from disable_watcher")
	}
}

// --- delete_watcher closure -------------------------------------------------

func TestDeleteWatcherTool(t *testing.T) {
	ctrl := &fakeWatcherController{}
	tr := registerInto(ctrl)
	res, err := tr.Get("delete_watcher").Execute(map[string]interface{}{"watcher": "gone"}, ToolContext{SessionID: "s"})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if ctrl.deleted != "gone" {
		t.Errorf("DeleteWatcher called with %q, want gone", ctrl.deleted)
	}
	if m := res.(map[string]interface{}); m["success"] != true || m["watcher"] != "gone" {
		t.Errorf("delete result = %+v, want success/watcher", m)
	}
}

func TestDeleteWatcherToolRequiresArg(t *testing.T) {
	tr := registerInto(&fakeWatcherController{})
	if _, err := tr.Get("delete_watcher").Execute(map[string]interface{}{}, ToolContext{SessionID: "s"}); err == nil {
		t.Error("delete_watcher must require the watcher argument")
	}
}

func TestDeleteWatcherToolPropagatesError(t *testing.T) {
	ctrl := &fakeWatcherController{deleteErr: errors.New("missing")}
	tr := registerInto(ctrl)
	if _, err := tr.Get("delete_watcher").Execute(map[string]interface{}{"watcher": "x"}, ToolContext{SessionID: "s"}); err == nil {
		t.Error("controller error should surface from delete_watcher")
	}
}
