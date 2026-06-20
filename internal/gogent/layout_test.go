package gogent

import (
	"os"
	"path/filepath"
	"testing"
)

// TestLayoutSaveLoadRoundTrip verifies a layout survives a save+load cycle with
// every field (title, pin, minimized, bounds) and its entry order intact.
func TestLayoutSaveLoadRoundTrip(t *testing.T) {
	home := t.TempDir()
	g := NewGogent(home)

	in := Layout{Entries: []LayoutEntry{
		{ID: "session-1", Title: "Refactor", Pinned: true, X: 2, Y: 2, W: 80, H: 24},
		{ID: "session-2", Title: "Bug fix", Minimized: true, X: 10, Y: 5, W: 60, H: 18},
		{ID: "session-3", Title: "", X: 0, Y: 0, W: 50, H: 12},
	}}
	if err := g.SaveLayout(in); err != nil {
		t.Fatalf("SaveLayout: %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, ".gogent", layoutFileName)); err != nil {
		t.Fatalf("layout file not written: %v", err)
	}

	out := g.LoadLayout()
	if len(out.Entries) != len(in.Entries) {
		t.Fatalf("expected %d entries, got %d", len(in.Entries), len(out.Entries))
	}
	for i, want := range in.Entries {
		if got := out.Entries[i]; got != want {
			t.Errorf("entry %d = %+v, want %+v", i, got, want)
		}
	}
}

// TestLayoutLoadMissingEmpty ensures a fresh home (no layout file yet) yields an
// empty layout instead of an error, so first launch is clean.
func TestLayoutLoadMissingEmpty(t *testing.T) {
	g := NewGogent(t.TempDir())
	if out := g.LoadLayout(); len(out.Entries) != 0 {
		t.Fatalf("expected empty layout for missing file, got %d entries", len(out.Entries))
	}
}

// TestLayoutLoadMalformedEmpty ensures a corrupted/truncated layout file can
// never block startup: it degrades to an empty layout.
func TestLayoutLoadMalformedEmpty(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, ".gogent")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, layoutFileName), []byte("{not valid json"), 0o644); err != nil {
		t.Fatal(err)
	}
	g := NewGogent(home)
	if out := g.LoadLayout(); len(out.Entries) != 0 {
		t.Fatalf("expected empty layout for malformed file, got %d entries", len(out.Entries))
	}
}

// TestLayoutSaveAtomic verifies a successful save leaves no .tmp file behind
// (the tmp+rename swap must clean up).
func TestLayoutSaveAtomic(t *testing.T) {
	home := t.TempDir()
	g := NewGogent(home)
	if err := g.SaveLayout(Layout{Entries: []LayoutEntry{{ID: "x"}}}); err != nil {
		t.Fatalf("SaveLayout: %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, ".gogent", layoutFileName+".tmp")); !os.IsNotExist(err) {
		t.Fatalf("expected no leftover .tmp file, got %v", err)
	}
}

// TestLayoutSaveOverwrite verifies a second save replaces the first (not
// appended/duplicated), so the file always reflects the current desktop.
func TestLayoutSaveOverwrite(t *testing.T) {
	home := t.TempDir()
	g := NewGogent(home)
	if err := g.SaveLayout(Layout{Entries: []LayoutEntry{{ID: "a"}, {ID: "b"}}}); err != nil {
		t.Fatal(err)
	}
	if err := g.SaveLayout(Layout{Entries: []LayoutEntry{{ID: "c"}}}); err != nil {
		t.Fatal(err)
	}
	out := g.LoadLayout()
	if len(out.Entries) != 1 || out.Entries[0].ID != "c" {
		t.Fatalf("expected single entry [c] after overwrite, got %+v", out.Entries)
	}
}

// TestLayoutEntryLookup covers the Entry helper (hit and miss).
func TestLayoutEntryLookup(t *testing.T) {
	l := Layout{Entries: []LayoutEntry{{ID: "a", Title: "A"}, {ID: "b", Title: "B"}}}
	if e := l.Entry("b"); e == nil || e.Title != "B" {
		t.Fatalf("Entry(b) = %+v, want Title B", e)
	}
	if e := l.Entry("missing"); e != nil {
		t.Fatalf("Entry(missing) = %+v, want nil", e)
	}
}

// TestLayoutOverallModelRoundTrip covers the persistence of the Overall band's
// per-model selection (issue #191, acceptance #3): a saved OverallModel must
// survive a save+load cycle alongside the rest of the layout, so the choice
// survives a restart.
func TestLayoutOverallModelRoundTrip(t *testing.T) {
	home := t.TempDir()
	g := NewGogent(home)

	in := Layout{
		Entries:      []LayoutEntry{{ID: "s1", Title: "Work", X: 1, Y: 1, W: 80, H: 24}},
		SidebarWidth: 30,
		OverallModel: "glm",
	}
	if err := g.SaveLayout(in); err != nil {
		t.Fatalf("SaveLayout: %v", err)
	}
	out := g.LoadLayout()
	if out.OverallModel != "glm" {
		t.Errorf("OverallModel = %q, want glm (must round-trip)", out.OverallModel)
	}
	if out.SidebarWidth != 30 {
		t.Errorf("SidebarWidth = %d, want 30 (unrelated field unaffected)", out.SidebarWidth)
	}
}

// TestLayoutOverallModelOmittedDefaultsEmpty ensures a layout file written
// before the OverallModel field existed (issue #175 era, or any older build)
// loads cleanly as the aggregate ("all models") view rather than breaking or
// inheriting garbage. This is the back-compat guarantee the field's omitempty
// tag provides.
func TestLayoutOverallModelOmittedDefaultsEmpty(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, ".gogent")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	// A pre-#191 layout file: no overall_model key.
	old := []byte(`{"entries":[{"id":"s1","title":"Old"}],"sidebar_width":24}`)
	if err := os.WriteFile(filepath.Join(dir, layoutFileName), old, 0o644); err != nil {
		t.Fatal(err)
	}
	g := NewGogent(home)
	out := g.LoadLayout()
	if out.OverallModel != "" {
		t.Errorf("OverallModel = %q on an old-format file, want empty (aggregate)", out.OverallModel)
	}
}

// TestLayoutOverallModelEmptyRoundTrip confirms the aggregate selection (empty
// string) is stable across a round-trip, so the default does not get mutated.
func TestLayoutOverallModelEmptyRoundTrip(t *testing.T) {
	home := t.TempDir()
	g := NewGogent(home)
	if err := g.SaveLayout(Layout{OverallModel: "", Entries: []LayoutEntry{{ID: "x"}}}); err != nil {
		t.Fatalf("SaveLayout: %v", err)
	}
	out := g.LoadLayout()
	if out.OverallModel != "" {
		t.Errorf("OverallModel = %q, want empty", out.OverallModel)
	}
}
