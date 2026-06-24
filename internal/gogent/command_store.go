package gogent

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"gogent/internal/config"
)

// commandsFileName is the user-defined custom-command file written under
// ~/.gogent (issue #403). It mirrors the per-feature-file precedent
// (watchers.json) rather than living in config.json, keeping config.json free of
// potentially many command definitions and their full version history.
const commandsFileName = "commands.json"

// LoadCommands reads the persisted custom slash commands from
// ~/.gogent/commands.json. A missing file or a parse error yields an empty store
// (no commands) rather than failing — the same lenient contract as LoadWatchers,
// so a corrupted/truncated file can never block command management.
func (g *Gogent) LoadCommands() config.CommandStore {
	if g.homeDir == "" {
		return config.CommandStore{}
	}
	data, err := os.ReadFile(filepath.Join(g.homeDir, ".gogent", commandsFileName))
	if err != nil {
		return config.CommandStore{}
	}
	var store config.CommandStore
	if err := json.Unmarshal(data, &store); err != nil {
		return config.CommandStore{}
	}
	return store
}

// SaveCommands persists the custom slash commands atomically (write tmp + rename,
// mode 0600), mirroring SaveWatchers' crash-safe write so a half-written file can
// never be left behind. A nil store writes an empty command list.
func (g *Gogent) SaveCommands(store *config.CommandStore) error {
	if g.homeDir == "" {
		return nil
	}
	if store == nil {
		store = &config.CommandStore{}
	}
	dir := filepath.Join(g.homeDir, ".gogent")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("create gogent dir: %w", err)
	}
	data, err := json.MarshalIndent(store, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal commands: %w", err)
	}
	path := filepath.Join(dir, commandsFileName)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("write commands file: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("rename commands file: %w", err)
	}
	return nil
}
