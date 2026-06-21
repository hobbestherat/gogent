package gogent

import (
	"testing"

	"gogent/internal/config"
)

// TestSetThemePersistsNoShadow is the end-to-end persistence test for issue #215.
// It exercises the exact backend handler the TUI theme-editor Save invokes
// (g.SetTheme at cmd/main.go:223) — not just config.SaveConfig in isolation — and
// confirms NoShadow survives a SetTheme → SaveConfig → relaunch round-trip. A break
// here (e.g. SetTheme not routing through SaveConfig, or the field dropped between
// g.config and the on-disk file) would silently lose the preference across restarts
// even though the config-layer round-trip passes.
func TestSetThemePersistsNoShadow(t *testing.T) {
	dir := t.TempDir()

	g := NewGogent(dir)
	// Default before any change: shadows on.
	if g.Theme().NoShadow {
		t.Fatalf("default g.Theme().NoShadow = true, want false")
	}

	// The runtime SetTheme handler: update in-memory + persist to disk.
	g.SetTheme(config.ThemeConfig{Name: "default", NoShadow: true})
	if !g.Theme().NoShadow {
		t.Fatalf("g.Theme().NoShadow = false immediately after SetTheme, want true (in-memory round-trip)")
	}

	// Simulate a relaunch: a fresh instance must load the persisted preference.
	g2 := NewGogent(dir)
	if !g2.Theme().NoShadow {
		t.Fatalf("relaunched g.Theme().NoShadow = false, want true — SetTheme did not persist NoShadow to disk")
	}

	// The reverse direction round-trips too: clearing it survives a relaunch.
	g2.SetTheme(config.ThemeConfig{Name: "default", NoShadow: false})
	g3 := NewGogent(dir)
	if g3.Theme().NoShadow {
		t.Fatalf("relaunched g.Theme().NoShadow = true after clearing, want false")
	}
}
