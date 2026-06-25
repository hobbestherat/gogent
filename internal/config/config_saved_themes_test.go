package config

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// Issue #462 — saved/named custom themes (NamedTheme, Config.SavedThemes) and the
// active↔saved back-link (ThemeConfig.SavedName). These tests pin the config data
// model and its JSON contract: the new fields round-trip, an older config.json
// that predates them loads as the zero value (no behaviour change), the omitempty
// tags keep a pristine config clean, and SavedName is metadata only.

// TestSavedThemesConfigRoundTrip confirms the full saved-themes model survives a
// JSON marshal/unmarshal cycle intact — the parent pointer, overrides and the
// active theme's SavedName back-link.
func TestSavedThemesConfigRoundTrip(t *testing.T) {
	in := &Config{
		Theme: ThemeConfig{
			Name:      "dark",
			SavedName: "Mine", // active theme is the saved theme "Mine"
			Overrides: map[string]string{"user": "#FF0000"},
		},
		SavedThemes: []NamedTheme{
			{Name: "Mine", Theme: ThemeConfig{Name: "dark", Overrides: map[string]string{"user": "#FF0000", "agent": "#00FF00"}}},
			{Name: "Bright", Theme: ThemeConfig{Name: "default", NoColor: false, NoShadow: true}},
		},
	}
	data, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out Config
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out.Theme.SavedName != "Mine" {
		t.Errorf("active SavedName lost in round-trip: got %q", out.Theme.SavedName)
	}
	if len(out.SavedThemes) != 2 {
		t.Fatalf("saved themes lost in round-trip: got %d", len(out.SavedThemes))
	}
	want := in.SavedThemes
	for i, nt := range want {
		got := out.SavedThemes[i]
		if got.Name != nt.Name {
			t.Errorf("saved[%d].Name = %q, want %q", i, got.Name, nt.Name)
		}
		if got.Theme.Name != nt.Theme.Name {
			t.Errorf("saved[%d].Theme.Name = %q, want %q (parent built-in)", i, got.Theme.Name, nt.Theme.Name)
		}
		if !reflect.DeepEqual(got.Theme.Overrides, nt.Theme.Overrides) {
			t.Errorf("saved[%d].Theme.Overrides = %+v, want %+v", i, got.Theme.Overrides, nt.Theme.Overrides)
		}
		// A stored saved theme must NOT carry a SavedName — the entry's own Name is
		// its identity, so SavedName is empty on the stored config (criterion: the
		// back-link lives only on the active Config.Theme).
		if got.Theme.SavedName != "" {
			t.Errorf("saved[%d].Theme.SavedName = %q, want empty (saved entries must not self-reference)", i, got.Theme.SavedName)
		}
	}
}

// TestSavedThemesLegacyConfigLoads is the backward-compat guarantee (criterion 3):
// an older config.json with neither "saved_themes" nor "saved_name" loads with the
// zero values, so the editor behaves exactly as before.
func TestSavedThemesLegacyConfigLoads(t *testing.T) {
	const legacy = `{"default_model": "x", "theme": {"name": "dark"}}`
	var cfg Config
	if err := json.Unmarshal([]byte(legacy), &cfg); err != nil {
		t.Fatalf("legacy unmarshal: %v", err)
	}
	if len(cfg.SavedThemes) != 0 {
		t.Errorf("legacy config should have no saved themes, got %+v", cfg.SavedThemes)
	}
	if cfg.Theme.SavedName != "" {
		t.Errorf("legacy active theme should have empty SavedName, got %q", cfg.Theme.SavedName)
	}
	// The pre-existing theme name must still load unchanged.
	if cfg.Theme.Name != "dark" {
		t.Errorf("legacy theme name lost: got %q, want dark", cfg.Theme.Name)
	}
}

// TestSavedThemesOmitEmpty confirms the omitempty tags keep a pristine install
// clean: no "saved_themes" or "saved_name" keys are emitted when there are no
// saved themes and the active theme is a plain built-in. This is what keeps the
// on-disk format stable for users who never use the feature.
func TestSavedThemesOmitEmpty(t *testing.T) {
	pristine := &Config{Theme: ThemeConfig{Name: "default"}} // no SavedName, no SavedThemes
	data, err := json.Marshal(pristine)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	s := string(data)
	if strings.Contains(s, "saved_themes") {
		t.Errorf("pristine config emits saved_themes: %s", s)
	}
	if strings.Contains(s, "saved_name") {
		t.Errorf("pristine config emits saved_name: %s", s)
	}

	// And conversely a populated config DOES emit both keys (guards against an
	// accidental `json:"-"` or a renamed tag silently dropping the data).
	populated := &Config{
		Theme:       ThemeConfig{Name: "default", SavedName: "X"},
		SavedThemes: []NamedTheme{{Name: "X", Theme: ThemeConfig{Name: "default"}}},
	}
	data2, err := json.Marshal(populated)
	if err != nil {
		t.Fatalf("marshal populated: %v", err)
	}
	s2 := string(data2)
	if !strings.Contains(s2, `"saved_themes"`) {
		t.Errorf("populated config missing saved_themes key: %s", s2)
	}
	if !strings.Contains(s2, `"saved_name"`) {
		t.Errorf("active theme missing saved_name key: %s", s2)
	}
}

// TestNamedThemeStoresParentOverrides documents the stored shape: a NamedTheme is
// a display name plus a ThemeConfig whose Name is the parent built-in and whose
// Overrides carry the customisations — exactly what paletteByName/editedTheme
// consume to re-resolve the theme.
func TestNamedThemeStoresParentOverrides(t *testing.T) {
	nt := NamedTheme{
		Name:  "My Dark",
		Theme: ThemeConfig{Name: "dark", Overrides: map[string]string{"user": "#10FF10"}},
	}
	data, err := json.Marshal(nt)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out NamedTheme
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out.Name != "My Dark" {
		t.Errorf("Name = %q, want My Dark", out.Name)
	}
	if out.Theme.Name != "dark" {
		t.Errorf("Theme.Name = %q, want dark (parent)", out.Theme.Name)
	}
	if out.Theme.Overrides["user"] != "#10FF10" {
		t.Errorf("override lost: got %+v", out.Theme.Overrides)
	}
}
