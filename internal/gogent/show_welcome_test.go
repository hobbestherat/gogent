package gogent

import (
	"os"
	"path/filepath"
	"testing"

	"gogent/internal/config"
)

func showWelcomeBool(v bool) *bool { return &v }

func TestGetShowWelcomeNilAndExplicitValues(t *testing.T) {
	cases := []struct {
		name string
		cfg  *config.Config
		want bool
	}{
		{"nil config is treated as show", nil, true},
		{"nil field is treated as show", &config.Config{}, true},
		{"explicit true shows", &config.Config{ShowWelcome: showWelcomeBool(true)}, true},
		{"explicit false suppresses", &config.Config{ShowWelcome: showWelcomeBool(false)}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGogentWithWorkspace(t.TempDir(), t.TempDir())
			g.config = tc.cfg
			if got := g.GetShowWelcome(); got != tc.want {
				t.Fatalf("GetShowWelcome() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestSetShowWelcomePersistsPreference(t *testing.T) {
	home := t.TempDir()
	g := NewGogentWithWorkspace(home, t.TempDir())

	if err := g.SetShowWelcome(false); err != nil {
		t.Fatalf("SetShowWelcome(false): %v", err)
	}
	if got := g.GetShowWelcome(); got {
		t.Fatal("GetShowWelcome() after SetShowWelcome(false) = true, want false")
	}
	loaded, err := config.LoadConfig(home)
	if err != nil {
		t.Fatalf("LoadConfig after false: %v", err)
	}
	if loaded.ShowWelcome == nil || *loaded.ShowWelcome {
		t.Fatalf("persisted ShowWelcome after false = %v, want explicit false", loaded.ShowWelcome)
	}

	if err := g.SetShowWelcome(true); err != nil {
		t.Fatalf("SetShowWelcome(true): %v", err)
	}
	loaded, err = config.LoadConfig(home)
	if err != nil {
		t.Fatalf("LoadConfig after true: %v", err)
	}
	if loaded.ShowWelcome == nil || !*loaded.ShowWelcome {
		t.Fatalf("persisted ShowWelcome after true = %v, want explicit true", loaded.ShowWelcome)
	}
}

func TestSetShowWelcomeReportsSaveError(t *testing.T) {
	parent := t.TempDir()
	homeFile := filepath.Join(parent, "not-a-directory")
	if err := os.WriteFile(homeFile, []byte("x"), 0600); err != nil {
		t.Fatalf("write home file: %v", err)
	}
	g := NewGogentWithWorkspace(t.TempDir(), t.TempDir())
	g.homeDir = homeFile
	g.config = config.GetDefaultConfig()

	if err := g.SetShowWelcome(false); err == nil {
		t.Fatal("SetShowWelcome with unwritable home path returned nil, want error")
	}
	if got := g.GetShowWelcome(); got {
		t.Fatal("SetShowWelcome should still update in-memory preference before reporting persistence error")
	}
}
