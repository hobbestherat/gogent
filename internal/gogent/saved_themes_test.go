package gogent

import (
	"reflect"
	"testing"

	"gogent/internal/config"
)

// Issue #462 — Gogent.SavedThemes / SetSavedThemes persistence. These tests cover
// the empty default, the deep-copy-on-read contract (callers can mutate the
// returned slice/maps without aliasing the live config), the set/get round-trip,
// and survival across a config.json reload.

// TestSavedThemesEmptyByDefault: a fresh Gogent has no saved themes (nil), so a
// config.json predating the feature yields nothing.
func TestSavedThemesEmptyByDefault(t *testing.T) {
	g := NewGogent(t.TempDir())
	if got := g.SavedThemes(); got != nil {
		t.Errorf("new gogent saved themes = %+v, want nil", got)
	}
}

// TestSavedThemesReturnsDeepCopy verifies the defensive-copy guarantee: mutating
// the slice/maps returned by SavedThemes — or appending to it — must not change
// the live config. Without the deep copy the editor's working copy would alias
// the live state and edits would leak before SetSavedThemes.
func TestSavedThemesReturnsDeepCopy(t *testing.T) {
	g := NewGogent(t.TempDir())
	g.SetSavedThemes([]config.NamedTheme{
		{Name: "A", Theme: config.ThemeConfig{Name: "default", Overrides: map[string]string{"user": "#FF0000"}}},
	})

	got := g.SavedThemes()
	// Mutate the returned copy in every way a caller might.
	got[0].Theme.Overrides["user"] = "#00FF00"
	got[0].Theme.Overrides["injected"] = "#000000"
	got[0].Name = "MUTATED"
	_ = append(got, config.NamedTheme{Name: "extra"})

	again := g.SavedThemes()
	if len(again) != 1 {
		t.Fatalf("appending to the returned slice changed the live count: got %d", len(again))
	}
	if again[0].Name != "A" {
		t.Errorf("Name field aliased the live config: got %q, want A", again[0].Name)
	}
	if again[0].Theme.Overrides["user"] != "#FF0000" {
		t.Errorf("override map aliased the live config: user = %q, want #FF0000", again[0].Theme.Overrides["user"])
	}
	if _, ok := again[0].Theme.Overrides["injected"]; ok {
		t.Errorf("an injected override leaked into the live config: %+v", again[0].Theme.Overrides)
	}
}

// TestSavedThemesSetGetRoundTrip: what SetSavedThemes stores, SavedThemes returns.
func TestSavedThemesSetGetRoundTrip(t *testing.T) {
	g := NewGogent(t.TempDir())
	in := []config.NamedTheme{
		{Name: "X", Theme: config.ThemeConfig{Name: "dark", Overrides: map[string]string{"agent": "#123456"}}},
		{Name: "Y", Theme: config.ThemeConfig{Name: "default", NoShadow: true}},
	}
	g.SetSavedThemes(in)
	got := g.SavedThemes()
	if !reflect.DeepEqual(got, in) {
		t.Fatalf("round-trip mismatch:\n got %+v\nwant %+v", got, in)
	}
}

// TestSavedThemesClearOnEmpty: setting an empty slice clears the list (a Delete
// that removes the last entry persists an empty list, not a stale one).
func TestSavedThemesClearOnEmpty(t *testing.T) {
	g := NewGogent(t.TempDir())
	g.SetSavedThemes([]config.NamedTheme{{Name: "A", Theme: config.ThemeConfig{Name: "default"}}})
	g.SetSavedThemes(nil)
	if got := g.SavedThemes(); got != nil {
		t.Errorf("after SetSavedThemes(nil), got %+v, want nil", got)
	}
}

// TestSavedThemesPersistAcrossReload confirms the saved themes are written to
// config.json and reloaded by a fresh Gogent from the same home — the
// "persist across restarts" acceptance criterion.
func TestSavedThemesPersistAcrossReload(t *testing.T) {
	home := t.TempDir()
	g1 := NewGogent(home)
	in := []config.NamedTheme{
		{Name: "Persist", Theme: config.ThemeConfig{Name: "default", NoShadow: true, Overrides: map[string]string{"user": "#A1B2C3"}}},
	}
	g1.SetSavedThemes(in)

	g2 := NewGogent(home) // re-read config.json from the same home
	got := g2.SavedThemes()
	if len(got) != 1 {
		t.Fatalf("saved themes did not survive reload: got %+v", got)
	}
	if got[0].Name != "Persist" || got[0].Theme.Name != "default" || !got[0].Theme.NoShadow {
		t.Errorf("reloaded saved theme lost fields: %+v", got[0])
	}
	if got[0].Theme.Overrides["user"] != "#A1B2C3" {
		t.Errorf("reloaded saved theme lost overrides: %+v", got[0].Theme.Overrides)
	}
}

// TestSavedThemesIndependentOfActiveTheme confirms the saved list and the active
// theme are stored separately: setting one does not perturb the other (the active
// Theme and SavedThemes are distinct Config fields).
func TestSavedThemesIndependentOfActiveTheme(t *testing.T) {
	g := NewGogent(t.TempDir())
	g.SetTheme(config.ThemeConfig{Name: "dark", SavedName: "X", Overrides: map[string]string{"user": "#FF0000"}})
	g.SetSavedThemes([]config.NamedTheme{{Name: "X", Theme: config.ThemeConfig{Name: "dark"}}})

	if got := g.Theme(); got.Name != "dark" || got.SavedName != "X" {
		t.Errorf("active theme perturbed by SetSavedThemes: %+v", got)
	}
	if len(g.SavedThemes()) != 1 || g.SavedThemes()[0].Name != "X" {
		t.Errorf("saved themes perturbed by SetTheme: %+v", g.SavedThemes())
	}
}
