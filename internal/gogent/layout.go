package gogent

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// LayoutEntry is one session's persisted window chrome: the sidebar title, pin
// state, and the on-screen position/size of its window. The title is a UI
// concern stored here (decoupled from the session id) so a renamed session
// survives a restart even though its transcript lives in a separate SessionStore
// file. Bounds/minimized capture the desktop arrangement the user left behind.
type LayoutEntry struct {
	ID        string `json:"id"`
	Title     string `json:"title,omitempty"`
	Pinned    bool   `json:"pinned,omitempty"`
	Minimized bool   `json:"minimized,omitempty"`
	X         int    `json:"x"`
	Y         int    `json:"y"`
	W         int    `json:"w"`
	H         int    `json:"h"`
}

// Layout is the persisted workbench layout. The Entries slice order is the
// sidebar order; a pinned entry carries its flag (the UI floats it to the top
// on pin). Entries whose session no longer exists are ignored on load.
type Layout struct {
	Entries []LayoutEntry `json:"entries"`
}

// Entry returns the layout entry for id, or nil if none is recorded. It lets
// callers look up a single session's chrome without scanning the slice.
func (l Layout) Entry(id string) *LayoutEntry {
	for i := range l.Entries {
		if l.Entries[i].ID == id {
			return &l.Entries[i]
		}
	}
	return nil
}

// layoutFileName is the workbench-layout file written next to the session
// transcripts under ~/.gogent.
const layoutFileName = "workbench_layout.json"

// LoadLayout reads the persisted workbench layout. A missing file or parse
// error yields an empty layout (the desktop simply starts fresh) rather than
// failing startup, so a corrupted/truncated layout can never block the app.
func (g *Gogent) LoadLayout() Layout {
	if g.homeDir == "" {
		return Layout{}
	}
	data, err := os.ReadFile(filepath.Join(g.homeDir, ".gogent", layoutFileName))
	if err != nil {
		return Layout{}
	}
	var layout Layout
	if err := json.Unmarshal(data, &layout); err != nil {
		return Layout{}
	}
	return layout
}

// SaveLayout persists the workbench layout atomically (write tmp + rename),
// mirroring SessionStore.Save's crash-safe write so a half-written layout file
// can never be left behind. It is best-effort: callers log the error but never
// block the UI on it.
func (g *Gogent) SaveLayout(layout Layout) error {
	if g.homeDir == "" {
		return nil
	}
	dir := filepath.Join(g.homeDir, ".gogent")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create gogent dir: %w", err)
	}
	data, err := json.MarshalIndent(layout, "", "  ")
	if err != nil {
		return err
	}
	path := filepath.Join(dir, layoutFileName)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
