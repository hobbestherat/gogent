package gogent

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"gogent/internal/config"
)

// watchersFileName is the free-running watcher-definition file written next to
// the session transcripts under ~/.gogent. It mirrors the per-feature-file
// precedent (workbench_layout.json) rather than living in config.json, keeping
// config.json clean of potentially many watcher definitions (issue #329).
const watchersFileName = "watchers.json"

// LoadWatchers reads the persisted free-running watcher definitions from
// ~/.gogent/watchers.json. A missing file or a parse error yields an empty store
// (no watchers) rather than failing startup, so a corrupted/truncated file can
// never block the app — the same lenient contract as LoadLayout.
//
// Any item missing an id is assigned a fresh one (GenerateWatcherID) and the
// store is persisted back so ids are stable across runs. Persisting is
// best-effort: a write failure is logged but the in-memory ids are still
// returned so this boot uses stable ids.
func (g *Gogent) LoadWatchers() config.WatcherStore {
	if g.homeDir == "" {
		return config.WatcherStore{}
	}
	data, err := os.ReadFile(filepath.Join(g.homeDir, ".gogent", watchersFileName))
	if err != nil {
		return config.WatcherStore{}
	}
	var store config.WatcherStore
	if err := json.Unmarshal(data, &store); err != nil {
		return config.WatcherStore{}
	}

	changed := false
	for i := range store.Items {
		if store.Items[i].ID == "" {
			store.Items[i].ID = config.GenerateWatcherID()
			changed = true
		}
	}
	if changed {
		if err := g.SaveWatchers(&store); err != nil {
			g.warnf("failed to persist watcher ids: %v", err)
		}
	}
	return store
}

// SaveWatchers persists the free-running watcher definitions atomically (write
// tmp + rename), mirroring SaveLayout's crash-safe write so a half-written file
// can never be left behind. A nil store writes an empty item list.
func (g *Gogent) SaveWatchers(store *config.WatcherStore) error {
	if g.homeDir == "" {
		return nil
	}
	if store == nil {
		store = &config.WatcherStore{}
	}
	dir := filepath.Join(g.homeDir, ".gogent")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("create gogent dir: %w", err)
	}
	data, err := json.MarshalIndent(store, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal watchers: %w", err)
	}
	path := filepath.Join(dir, watchersFileName)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("write watchers file: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("rename watchers file: %w", err)
	}
	return nil
}
