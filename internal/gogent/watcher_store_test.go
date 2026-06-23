package gogent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"gogent/internal/config"
)

// watchersPath returns the on-disk watchers.json path for a gogent homeDir.
func watchersPath(home string) string {
	return filepath.Join(home, ".gogent", watchersFileName)
}

// writeRawWatchers writes raw bytes to the watchers.json file, creating the
// .gogent dir. Used to exercise the lenient loader with hand-written/corrupt
// content.
func writeRawWatchers(t *testing.T, home string, data []byte) {
	t.Helper()
	dir := filepath.Join(home, ".gogent")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(watchersPath(home), data, 0o600); err != nil {
		t.Fatalf("write watchers file: %v", err)
	}
}

// TestSaveLoadWatchersRoundTrip confirms a store with fully-populated items
// survives a save/load cycle byte-for-byte (no id generation needed since ids
// are present).
func TestSaveLoadWatchersRoundTrip(t *testing.T) {
	home := t.TempDir()
	g := NewGogent(home)

	want := config.WatcherStore{Items: []config.WatcherConfig{
		{
			ID:       "watcher-aaaaaaaa",
			Name:     "alpha",
			Enabled:  true,
			Schedule: config.ScheduleConfig{Every: "5m"},
			Task:     "do alpha",
			Model:    "local-lan",
			Output:   &config.WatcherOutput{Notify: true},
		},
		{
			ID:       "watcher-bbbbbbbb",
			Name:     "beta",
			Enabled:  false,
			Schedule: config.ScheduleConfig{DailyAt: "07:00", Timezone: "Europe/Zurich"},
			Task:     "do beta",
		},
	}}

	if err := g.SaveWatchers(&want); err != nil {
		t.Fatalf("SaveWatchers: %v", err)
	}
	got := g.LoadWatchers()
	if !reflect.DeepEqual(got, want) {
		t.Errorf("round-trip mismatch:\n got %+v\nwant %+v", got, want)
	}
}

// TestLoadWatchersMissingFileEmpty confirms a missing file loads as an empty
// store (lenient), never an error that would block startup.
func TestLoadWatchersMissingFileEmpty(t *testing.T) {
	g := NewGogent(t.TempDir())
	got := g.LoadWatchers()
	if len(got.Items) != 0 {
		t.Errorf("missing file should load empty, got %d items", len(got.Items))
	}
}

// TestLoadWatchersCorruptEmpty confirms malformed / truncated / non-object
// content loads as an empty store rather than failing.
func TestLoadWatchersCorruptEmpty(t *testing.T) {
	for _, raw := range []string{
		"",                       // empty file
		"{",                      // truncated object
		"not json at all",        // garbage
		`{"items": "not-array"}`, // wrong type for items
		"[1,2,3]",                // top-level array, not the store object
	} {
		home := t.TempDir()
		g := NewGogent(home)
		writeRawWatchers(t, home, []byte(raw))
		got := g.LoadWatchers()
		if len(got.Items) != 0 {
			t.Errorf("corrupt content %q should load empty, got %d items", raw, len(got.Items))
		}
	}
}

// TestLoadWatchersEmptyObjects confirms valid-but-empty JSON shapes load as an
// empty store with no error.
func TestLoadWatchersEmptyObjects(t *testing.T) {
	for _, raw := range []string{`{}`, `null`, `{"items": []}`, `{"items": null}`} {
		home := t.TempDir()
		g := NewGogent(home)
		writeRawWatchers(t, home, []byte(raw))
		if got := g.LoadWatchers(); len(got.Items) != 0 {
			t.Errorf("content %q should load empty, got %d items", raw, len(got.Items))
		}
	}
}

// TestLoadWatchersGeneratesAndPersistsIDs confirms that items missing an id get
// one generated on load AND that the generated id is persisted back to disk so
// it is stable across runs.
func TestLoadWatchersGeneratesAndPersistsIDs(t *testing.T) {
	home := t.TempDir()
	g := NewGogent(home)
	writeRawWatchers(t, home, []byte(`{"items":[
		{"name":"a","schedule":{"every":"5m"},"task":"ta"},
		{"name":"b","schedule":{"daily_at":"07:00"},"task":"tb"}
	]}`))

	got := g.LoadWatchers()
	if len(got.Items) != 2 {
		t.Fatalf("expected 2 items, got %d", len(got.Items))
	}
	idRe := regexp.MustCompile(`^watcher-[0-9a-f]{8}$`)
	for i, it := range got.Items {
		if !idRe.MatchString(it.ID) {
			t.Errorf("item %d: id %q not generated/valid", i, it.ID)
		}
	}

	// The ids must be written back to disk: re-read the raw file and confirm both
	// generated ids are present.
	raw, err := os.ReadFile(watchersPath(home))
	if err != nil {
		t.Fatalf("re-read file: %v", err)
	}
	for _, it := range got.Items {
		if !strings.Contains(string(raw), it.ID) {
			t.Errorf("generated id %q was not persisted to disk: %s", it.ID, raw)
		}
	}
}

// TestLoadWatchersStableIDsAcrossLoads confirms generated ids do not change on a
// subsequent load (they were persisted, not regenerated).
func TestLoadWatchersStableIDsAcrossLoads(t *testing.T) {
	home := t.TempDir()
	g := NewGogent(home)
	writeRawWatchers(t, home, []byte(`{"items":[{"name":"a","schedule":{"every":"5m"},"task":"ta"}]}`))

	first := g.LoadWatchers()
	second := g.LoadWatchers()
	if len(first.Items) != 1 || len(second.Items) != 1 {
		t.Fatalf("expected 1 item each load, got %d and %d", len(first.Items), len(second.Items))
	}
	if first.Items[0].ID != second.Items[0].ID {
		t.Errorf("id changed across loads: %q -> %q", first.Items[0].ID, second.Items[0].ID)
	}
}

// TestLoadWatchersPreservesExistingIDs confirms a hand-written id is not
// regenerated, while a sibling without an id still gets one.
func TestLoadWatchersPreservesExistingIDs(t *testing.T) {
	home := t.TempDir()
	g := NewGogent(home)
	writeRawWatchers(t, home, []byte(`{"items":[
		{"id":"watcher-keepme0","name":"a","schedule":{"every":"5m"},"task":"ta"},
		{"name":"b","schedule":{"every":"5m"},"task":"tb"}
	]}`))

	got := g.LoadWatchers()
	if len(got.Items) != 2 {
		t.Fatalf("expected 2 items, got %d", len(got.Items))
	}
	if got.Items[0].ID != "watcher-keepme0" {
		t.Errorf("explicit id was changed: %q", got.Items[0].ID)
	}
	if got.Items[1].ID == "" {
		t.Error("sibling without id should have been generated one")
	}
}

// TestSaveWatchersNilWritesEmpty confirms a nil store is written as an empty,
// valid items list (and loads back empty).
func TestSaveWatchersNilWritesEmpty(t *testing.T) {
	home := t.TempDir()
	g := NewGogent(home)
	if err := g.SaveWatchers(nil); err != nil {
		t.Fatalf("SaveWatchers(nil): %v", err)
	}
	data, err := os.ReadFile(watchersPath(home))
	if err != nil {
		t.Fatalf("read after nil save: %v", err)
	}
	var store config.WatcherStore
	if err := json.Unmarshal(data, &store); err != nil {
		t.Fatalf("nil save produced invalid JSON: %v (%s)", err, data)
	}
	if len(store.Items) != 0 {
		t.Errorf("nil save should yield empty items, got %d", len(store.Items))
	}
}

// TestSaveWatchersAtomicNoTempLeft confirms the atomic temp+rename write leaves
// no .tmp file behind on success.
func TestSaveWatchersAtomicNoTempLeft(t *testing.T) {
	home := t.TempDir()
	g := NewGogent(home)
	if err := g.SaveWatchers(&config.WatcherStore{Items: []config.WatcherConfig{
		{ID: "watcher-cccccccc", Name: "c", Schedule: config.ScheduleConfig{Every: "5m"}, Task: "tc"},
	}}); err != nil {
		t.Fatalf("SaveWatchers: %v", err)
	}
	if _, err := os.Stat(watchersPath(home) + ".tmp"); !os.IsNotExist(err) {
		t.Errorf("expected no leftover .tmp file, stat err=%v", err)
	}
}

// TestSaveWatchersOverwrites confirms a second save replaces the prior content
// rather than appending or merging.
func TestSaveWatchersOverwrites(t *testing.T) {
	home := t.TempDir()
	g := NewGogent(home)
	if err := g.SaveWatchers(&config.WatcherStore{Items: []config.WatcherConfig{
		{ID: "watcher-dddddddd", Name: "d", Schedule: config.ScheduleConfig{Every: "5m"}, Task: "td"},
	}}); err != nil {
		t.Fatalf("SaveWatchers 1: %v", err)
	}
	if err := g.SaveWatchers(&config.WatcherStore{Items: []config.WatcherConfig{
		{ID: "watcher-eeeeeeee", Name: "e", Schedule: config.ScheduleConfig{Every: "10m"}, Task: "te"},
	}}); err != nil {
		t.Fatalf("SaveWatchers 2: %v", err)
	}
	got := g.LoadWatchers()
	if len(got.Items) != 1 || got.Items[0].ID != "watcher-eeeeeeee" {
		t.Fatalf("expected only the second item to remain, got %+v", got.Items)
	}
}
