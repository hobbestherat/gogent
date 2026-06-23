package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func showWelcomeConfigBool(v bool) *bool { return &v }

// TestShowWelcomeDefaultConfig documents the first-run behavior: freshly written
// configs expose show_welcome:true, while older configs that omit the key still
// leave a nil pointer for the Gogent accessor to treat as "show".
func TestShowWelcomeDefaultConfig(t *testing.T) {
	cfg := GetDefaultConfig()
	if cfg.ShowWelcome == nil {
		t.Fatal("GetDefaultConfig left ShowWelcome nil; fresh configs should document the startup preference")
	}
	if !*cfg.ShowWelcome {
		t.Fatal("GetDefaultConfig ShowWelcome = false, want true")
	}

	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal default config: %v", err)
	}
	if !strings.Contains(string(raw), `"show_welcome":true`) {
		t.Fatalf("default config JSON missing show_welcome:true; got %s", raw)
	}
}

func TestShowWelcomeJSONPointerSemantics(t *testing.T) {
	cases := []struct {
		name    string
		value   *bool
		wantKey string
	}{
		{"absent", nil, ""},
		{"explicit false", showWelcomeConfigBool(false), `"show_welcome":false`},
		{"explicit true", showWelcomeConfigBool(true), `"show_welcome":true`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := json.Marshal(&Config{ShowWelcome: tc.value})
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if tc.wantKey == "" {
				if strings.Contains(string(raw), "show_welcome") {
					t.Fatalf("nil ShowWelcome must be omitted; JSON was %s", raw)
				}
			} else if !strings.Contains(string(raw), tc.wantKey) {
				t.Fatalf("serialized JSON %s missing %s", raw, tc.wantKey)
			}

			var loaded Config
			if err := json.Unmarshal(raw, &loaded); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if tc.value == nil {
				if loaded.ShowWelcome != nil {
					t.Fatalf("omitted show_welcome loaded as non-nil: %v", *loaded.ShowWelcome)
				}
				return
			}
			if loaded.ShowWelcome == nil || *loaded.ShowWelcome != *tc.value {
				t.Fatalf("round-tripped ShowWelcome = %v, want %v", loaded.ShowWelcome, *tc.value)
			}
		})
	}
}

func TestLoadConfigShowWelcomeCompatibilityAndPersistence(t *testing.T) {
	t.Run("missing file uses default true", func(t *testing.T) {
		cfg, err := LoadConfig(t.TempDir())
		if err != nil {
			t.Fatalf("LoadConfig: %v", err)
		}
		if cfg.ShowWelcome == nil || !*cfg.ShowWelcome {
			t.Fatalf("missing config ShowWelcome = %v, want explicit true default", cfg.ShowWelcome)
		}
	})

	t.Run("older config without key leaves nil", func(t *testing.T) {
		home := t.TempDir()
		dir := filepath.Join(home, ".gogent")
		if err := os.MkdirAll(dir, 0750); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(`{"default_model":"test"}`), 0600); err != nil {
			t.Fatalf("write config: %v", err)
		}
		cfg, err := LoadConfig(home)
		if err != nil {
			t.Fatalf("LoadConfig: %v", err)
		}
		if cfg.ShowWelcome != nil {
			t.Fatalf("older config without show_welcome loaded ShowWelcome=%v, want nil", *cfg.ShowWelcome)
		}
	})

	t.Run("explicit false survives save load", func(t *testing.T) {
		home := t.TempDir()
		cfg := GetDefaultConfig()
		cfg.ShowWelcome = showWelcomeConfigBool(false)
		if err := SaveConfig(home, cfg); err != nil {
			t.Fatalf("SaveConfig: %v", err)
		}
		loaded, err := LoadConfig(home)
		if err != nil {
			t.Fatalf("LoadConfig: %v", err)
		}
		if loaded.ShowWelcome == nil || *loaded.ShowWelcome {
			t.Fatalf("loaded ShowWelcome = %v, want explicit false", loaded.ShowWelcome)
		}
	})
}
