package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// This file tests issue #215's config half: ThemeConfig.NoShadow (json
// "no_shadow,omitempty", default false = shadows on). It covers the JSON contract
// (the tag, omitempty, the default), the real on-disk Save/Load round-trip, error
// handling for a corrupt file, and that NoShadow is orthogonal to NoColor.

// TestNoShadowDefaultsFalse asserts the zero value keeps shadows on (NoShadow ==
// false), matching the issue's "default unchanged" acceptance criterion, and that
// the application default config does not flip it on.
func TestNoShadowDefaultsFalse(t *testing.T) {
	var tc ThemeConfig
	if tc.NoShadow {
		t.Errorf("zero ThemeConfig.NoShadow = true, want false (shadows default on)")
	}
	if GetDefaultConfig().Theme.NoShadow {
		t.Errorf("GetDefaultConfig().Theme.NoShadow = true, want false")
	}
}

// TestNoShadowJSONTag pins the field's json contract: the tag is "no_shadow" with
// omitempty, so true serialises to "no_shadow":true (compact) and false omits the
// key entirely. A wrong/missing tag would break both persistence and the "default
// unchanged" guarantee (a false would be written and re-read noisily).
func TestNoShadowJSONTag(t *testing.T) {
	out, err := json.Marshal(ThemeConfig{NoShadow: true})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(out), `"no_shadow":true`) {
		t.Errorf("marshal NoShadow=true = %s, want it to contain \"no_shadow\":true", out)
	}

	out, err = json.Marshal(ThemeConfig{NoShadow: false})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(out), "no_shadow") {
		t.Errorf("marshal NoShadow=false = %s, want no_shadow omitted by omitempty", out)
	}
}

// TestNoShadowUnmarshal covers reading the key back: explicit true, explicit false,
// and absent (default false). The absent case is the backwards-compat guarantee for
// an older config.json predating the field.
func TestNoShadowUnmarshal(t *testing.T) {
	cases := []struct {
		name string
		json string
		want bool
	}{
		{"explicit true", `{"no_shadow":true}`, true},
		{"explicit false", `{"no_shadow":false}`, false},
		{"absent is false", `{}`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var c ThemeConfig
			if err := json.Unmarshal([]byte(tc.json), &c); err != nil {
				t.Fatalf("unmarshal %s: %v", tc.json, err)
			}
			if c.NoShadow != tc.want {
				t.Errorf("NoShadow = %v, want %v", c.NoShadow, tc.want)
			}
		})
	}
}

// TestNoShadowFullConfigUnmarshal ensures no_shadow is read inside a complete
// Config's theme block (the shape config.json actually has on disk), alongside and
// independent of no_color.
func TestNoShadowFullConfigUnmarshal(t *testing.T) {
	const data = `{
		"default_model": "x",
		"models": [],
		"theme": {
			"name": "high-contrast",
			"no_shadow": true,
			"no_color": true
		}
	}`
	var cfg Config
	if err := json.Unmarshal([]byte(data), &cfg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !cfg.Theme.NoShadow {
		t.Errorf("Theme.NoShadow = false, want true")
	}
	if !cfg.Theme.NoColor {
		t.Errorf("Theme.NoColor = false, want true (must remain independent of NoShadow)")
	}
}

// TestNoShadowSaveLoadRoundTrip exercises the real persistence path end to end: a
// config with NoShadow=true is written to disk and read back preserving the flag,
// and the on-disk file actually carries the serialised key. The false direction is
// checked too so the field truly round-trips both ways (not just "stays true").
func TestNoShadowSaveLoadRoundTrip(t *testing.T) {
	t.Run("true persists and reloads", func(t *testing.T) {
		dir := t.TempDir()
		in := &Config{DefaultModel: "x", Theme: ThemeConfig{Name: "default", NoShadow: true}}
		if err := SaveConfig(dir, in); err != nil {
			t.Fatalf("SaveConfig: %v", err)
		}
		// The persisted file must carry the serialised key (MarshalIndent uses ": ").
		data, err := os.ReadFile(filepath.Join(dir, ".gogent", "config.json"))
		if err != nil {
			t.Fatalf("read config file: %v", err)
		}
		if !strings.Contains(string(data), `"no_shadow": true`) {
			t.Errorf("config file missing \"no_shadow\": true:\n%s", data)
		}
		out, err := LoadConfig(dir)
		if err != nil {
			t.Fatalf("LoadConfig: %v", err)
		}
		if !out.Theme.NoShadow {
			t.Errorf("reloaded Theme.NoShadow = false, want true")
		}
	})

	t.Run("false round-trips too", func(t *testing.T) {
		dir := t.TempDir()
		in := &Config{DefaultModel: "x", Theme: ThemeConfig{Name: "default", NoShadow: false}}
		if err := SaveConfig(dir, in); err != nil {
			t.Fatalf("SaveConfig: %v", err)
		}
		out, err := LoadConfig(dir)
		if err != nil {
			t.Fatalf("LoadConfig: %v", err)
		}
		if out.Theme.NoShadow {
			t.Errorf("reloaded Theme.NoShadow = true, want false")
		}
	})
}

// TestNoShadowIndependentOfNoColor verifies the two toggles are orthogonal: a
// marshalled config carrying both unmarshals both, and neither serialisation drops
// the other.
func TestNoShadowIndependentOfNoColor(t *testing.T) {
	in := ThemeConfig{NoColor: true, NoShadow: true}
	data, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out ThemeConfig
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !out.NoColor || !out.NoShadow {
		t.Errorf("round-trip lost a flag: NoColor=%v NoShadow=%v", out.NoColor, out.NoShadow)
	}
}

// TestLoadConfigMissingReturnsDefaultNoShadow checks the no-file path returns the
// default config (NoShadow false) with no error — the behaviour for a fresh
// install / first run, which must keep shadows on.
func TestLoadConfigMissingReturnsDefaultNoShadow(t *testing.T) {
	dir := t.TempDir()
	cfg, err := LoadConfig(dir)
	if err != nil {
		t.Fatalf("LoadConfig on missing file returned err %v, want nil", err)
	}
	if cfg == nil {
		t.Fatal("LoadConfig returned nil config")
	}
	if cfg.Theme.NoShadow {
		t.Errorf("default Theme.NoShadow = true, want false")
	}
}

// TestLoadConfigMalformedReturnsError checks a corrupt config file surfaces a parse
// error rather than silently dropping the theme (the error-handling contract).
func TestLoadConfigMalformedReturnsError(t *testing.T) {
	dir := t.TempDir()
	configDir := filepath.Join(dir, ".gogent")
	if err := os.MkdirAll(configDir, 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(configDir, "config.json"), []byte("{not valid json"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := LoadConfig(dir); err == nil {
		t.Fatal("LoadConfig on malformed JSON returned nil err, want a parse error")
	}
}
