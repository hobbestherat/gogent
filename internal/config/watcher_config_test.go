package config

import (
	"encoding/json"
	"os"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"gogent/internal/watcher"
)

// boolPtr is a small helper for the *bool fields under test.
func boolPtr(b bool) *bool { return &b }

// TestScheduleConfigInterval covers the every/duration branch of
// ScheduleConfig.Schedule(): a valid positive duration parses to an
// IntervalSchedule with the exact duration.
func TestScheduleConfigInterval(t *testing.T) {
	for _, tc := range []struct {
		every string
		want  time.Duration
	}{
		{"5m", 5 * time.Minute},
		{"30s", 30 * time.Second},
		{"1h30m", 90 * time.Minute},
		{"  10m  ", 10 * time.Minute}, // surrounding whitespace is trimmed
	} {
		sched, err := ScheduleConfig{Every: tc.every}.Schedule()
		if err != nil {
			t.Fatalf("Schedule(every=%q): unexpected error: %v", tc.every, err)
		}
		iv, ok := sched.(watcher.IntervalSchedule)
		if !ok {
			t.Fatalf("Schedule(every=%q): want IntervalSchedule, got %T", tc.every, sched)
		}
		if iv.D != tc.want {
			t.Errorf("Schedule(every=%q): D=%v, want %v", tc.every, iv.D, tc.want)
		}
	}
}

// TestScheduleConfigIntervalInvalid covers the error cases of the interval
// branch: unparseable, zero and negative durations are all rejected.
func TestScheduleConfigIntervalInvalid(t *testing.T) {
	for _, every := range []string{"abc", "5", "0s", "0", "-5m", "-1ns"} {
		if _, err := (ScheduleConfig{Every: every}).Schedule(); err == nil {
			t.Errorf("Schedule(every=%q): expected error, got nil", every)
		}
	}
}

// TestScheduleConfigDailyAt covers the daily_at branch: a valid HH:MM with a
// timezone parses to a DailySchedule with the right hour/min/location, and an
// empty timezone defaults to UTC.
func TestScheduleConfigDailyAt(t *testing.T) {
	for _, tc := range []struct {
		dailyAt  string
		tz       string
		wantH    int
		wantM    int
		wantZone string
	}{
		{"07:00", "Europe/Zurich", 7, 0, "Europe/Zurich"},
		{"23:59", "", 23, 59, "UTC"},
		{"00:00", "UTC", 0, 0, "UTC"},
		{"7:5", "", 7, 5, "UTC"},      // lenient single-digit fields
		{" 09:30 ", "", 9, 30, "UTC"}, // outer whitespace trimmed
		{"13:45", "America/New_York", 13, 45, "America/New_York"},
	} {
		sched, err := ScheduleConfig{DailyAt: tc.dailyAt, Timezone: tc.tz}.Schedule()
		if err != nil {
			t.Fatalf("Schedule(daily=%q tz=%q): unexpected error: %v", tc.dailyAt, tc.tz, err)
		}
		ds, ok := sched.(watcher.DailySchedule)
		if !ok {
			t.Fatalf("Schedule(daily=%q): want DailySchedule, got %T", tc.dailyAt, sched)
		}
		if ds.Hour != tc.wantH || ds.Min != tc.wantM {
			t.Errorf("Schedule(daily=%q): got %02d:%02d, want %02d:%02d", tc.dailyAt, ds.Hour, ds.Min, tc.wantH, tc.wantM)
		}
		if ds.Loc == nil {
			t.Fatalf("Schedule(daily=%q): nil location", tc.dailyAt)
		}
		if ds.Loc.String() != tc.wantZone {
			t.Errorf("Schedule(daily=%q tz=%q): location=%q, want %q", tc.dailyAt, tc.tz, ds.Loc.String(), tc.wantZone)
		}
	}
}

// TestScheduleConfigDailyAtInvalid covers the daily_at validation errors:
// malformed clock strings and out-of-range hours/minutes.
func TestScheduleConfigDailyAtInvalid(t *testing.T) {
	for _, dailyAt := range []string{
		"24:00",    // hour out of range
		"07:60",    // minute out of range
		"-1:00",    // negative hour
		"07:-1",    // negative minute
		"7",        // no colon
		"07:00:00", // too many fields
		"ab:cd",    // non-numeric
		"07:",      // missing minute
		":30",      // missing hour
		"",         // empty (also caught by exactly-one-of, but defensive)
	} {
		if _, err := (ScheduleConfig{DailyAt: dailyAt}).Schedule(); err == nil {
			t.Errorf("Schedule(daily=%q): expected error, got nil", dailyAt)
		}
	}
}

// TestScheduleConfigBadTimezone confirms an invalid IANA timezone is rejected
// for a daily schedule.
func TestScheduleConfigBadTimezone(t *testing.T) {
	if _, err := (ScheduleConfig{DailyAt: "07:00", Timezone: "Not/AZone"}).Schedule(); err == nil {
		t.Fatal("Schedule with invalid timezone: expected error, got nil")
	}
}

// TestScheduleConfigExactlyOneOf confirms the exactly-one-of Every/DailyAt
// invariant: setting both, or neither, is an error (including when one field is
// only whitespace, which trims to empty).
func TestScheduleConfigExactlyOneOf(t *testing.T) {
	for _, tc := range []ScheduleConfig{
		{Every: "5m", DailyAt: "07:00"}, // both
		{},                              // neither
		{Timezone: "Europe/Zurich"},     // tz only, no cadence
		{Every: "   ", DailyAt: "   "},  // both whitespace -> neither
	} {
		if _, err := tc.Schedule(); err == nil {
			t.Errorf("Schedule(%+v): expected exactly-one-of error, got nil", tc)
		}
	}
}

// TestScheduleConfigTimezoneIgnoredForInterval locks in the documented behavior
// that Timezone is only consulted for daily_at: an interval schedule with an
// invalid timezone still parses, because the field is irrelevant there.
func TestScheduleConfigTimezoneIgnoredForInterval(t *testing.T) {
	sched, err := ScheduleConfig{Every: "5m", Timezone: "Not/AZone"}.Schedule()
	if err != nil {
		t.Fatalf("interval schedule should ignore timezone, got error: %v", err)
	}
	if _, ok := sched.(watcher.IntervalSchedule); !ok {
		t.Fatalf("want IntervalSchedule, got %T", sched)
	}
}

// TestWatchersConfigMaxConcurrentDefault covers the default-4 accessor across
// unset, zero, negative and explicit values.
func TestWatchersConfigMaxConcurrentDefault(t *testing.T) {
	for _, tc := range []struct {
		in   int
		want int
	}{
		{0, 4},  // unset -> default
		{-1, 4}, // negative -> default
		{-100, 4},
		{1, 1},
		{8, 8},
	} {
		if got := (WatchersConfig{MaxConcurrent: tc.in}).MaxConcurrentOrDefault(); got != tc.want {
			t.Errorf("MaxConcurrentOrDefault(%d)=%d, want %d", tc.in, got, tc.want)
		}
	}
}

// TestWatchersConfigSkipIfRunningDefault covers the default-true accessor: a nil
// pointer means true, while an explicit pointer is honored either way.
func TestWatchersConfigSkipIfRunningDefault(t *testing.T) {
	if got := (WatchersConfig{}).SkipIfRunningOrDefault(); got != true {
		t.Errorf("SkipIfRunningOrDefault(nil)=%v, want true", got)
	}
	if got := (WatchersConfig{DefaultSkipIfRunning: boolPtr(false)}).SkipIfRunningOrDefault(); got != false {
		t.Errorf("SkipIfRunningOrDefault(&false)=%v, want false", got)
	}
	if got := (WatchersConfig{DefaultSkipIfRunning: boolPtr(true)}).SkipIfRunningOrDefault(); got != true {
		t.Errorf("SkipIfRunningOrDefault(&true)=%v, want true", got)
	}
}

// TestGenerateWatcherID checks the id format ("watcher-" + 8 hex chars) and that
// successive ids differ.
func TestGenerateWatcherID(t *testing.T) {
	re := regexp.MustCompile(`^watcher-[0-9a-f]{8}$`)
	seen := make(map[string]bool)
	for i := 0; i < 100; i++ {
		id := GenerateWatcherID()
		if !re.MatchString(id) {
			t.Fatalf("GenerateWatcherID()=%q does not match %s", id, re)
		}
		if seen[id] {
			t.Fatalf("GenerateWatcherID produced a duplicate: %q", id)
		}
		seen[id] = true
	}
}

// TestWatcherStoreJSONShape verifies the on-disk JSON shape (field names) of a
// WatcherStore round-trips through encoding/json and uses the documented keys.
func TestWatcherStoreJSONShape(t *testing.T) {
	store := WatcherStore{Items: []WatcherConfig{{
		ID:       "watcher-a1b2c3d4",
		Name:     "daily-meeting-overview",
		Enabled:  true,
		Schedule: ScheduleConfig{DailyAt: "07:00", Timezone: "Europe/Zurich"},
		Task:     "Pull today's meetings and email an overview.",
		Model:    "local-lan",
		Output:   &WatcherOutput{Notify: true},
	}}}

	data, err := json.Marshal(store)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, key := range []string{
		`"items"`, `"id"`, `"name"`, `"enabled"`, `"schedule"`,
		`"daily_at"`, `"timezone"`, `"task"`, `"model"`, `"on_complete"`, `"notify"`,
	} {
		if !strings.Contains(string(data), key) {
			t.Errorf("marshalled store missing key %s in: %s", key, data)
		}
	}

	var back WatcherStore
	if err := json.Unmarshal(data, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !reflect.DeepEqual(store, back) {
		t.Errorf("round-trip mismatch:\n got %+v\nwant %+v", back, store)
	}
}

// TestWatcherConfigOmitsEmptyOptionalFields confirms a free-running watcher with
// no model/output/id does not emit those keys (so a hand-written file stays
// minimal) but still emits the required name/schedule/task.
func TestWatcherConfigOmitsEmptyOptionalFields(t *testing.T) {
	data, err := json.Marshal(WatcherConfig{Name: "n", Task: "t", Schedule: ScheduleConfig{Every: "5m"}})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	s := string(data)
	for _, absent := range []string{`"id"`, `"model"`, `"on_complete"`, `"enabled"`} {
		if strings.Contains(s, absent) {
			t.Errorf("expected %s to be omitted, got: %s", absent, s)
		}
	}
}

// TestSampleConfigHasWatchers verifies config.sample.json carries the watcher
// tuning block, the experimental.watchers gate, and the notify on_watcher toggle,
// and that it parses cleanly into a Config with the documented values.
func TestSampleConfigHasWatchers(t *testing.T) {
	data, err := os.ReadFile("../../config.sample.json")
	if err != nil {
		t.Fatalf("read sample config: %v", err)
	}
	// Key presence (unmarshalling alone cannot distinguish an absent key from a
	// false zero value).
	for _, key := range []string{`"watchers"`, `"on_watcher"`} {
		if !strings.Contains(string(data), key) {
			t.Errorf("config.sample.json is missing key %s", key)
		}
	}

	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("config.sample.json does not parse: %v", err)
	}
	if cfg.Experimental.Watchers {
		t.Error("sample experimental.watchers should ship disabled (false)")
	}
	if got := cfg.Watchers.MaxConcurrentOrDefault(); got != 4 {
		t.Errorf("sample watchers.max_concurrent resolves to %d, want 4", got)
	}
	if got := cfg.Watchers.SkipIfRunningOrDefault(); got != true {
		t.Errorf("sample watchers.default_skip_if_running resolves to %v, want true", got)
	}
	if cfg.Notify == nil || !cfg.Notify.OnWatcher {
		t.Errorf("sample notify.on_watcher should be true, got %+v", cfg.Notify)
	}
}
