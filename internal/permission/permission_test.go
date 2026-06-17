package permission

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPermissionConfigDefaultAsk(t *testing.T) {
	config := NewPermissionConfig()

	// Unknown permission should default to ask
	level := config.GetPermission(PermissionSkill)
	if level != PermissionAsk {
		t.Errorf("Expected default ask for unknown permission, got %s", level)
	}
}

func TestPermissionConfigSetAndGet(t *testing.T) {
	config := NewPermissionConfig()

	config.SetPermission(PermissionSkill, PermissionYes)
	config.SetPermission(PermissionSystemCommand, PermissionNo)

	if config.GetPermission(PermissionSkill) != PermissionYes {
		t.Error("Expected yes for skill")
	}

	if config.GetPermission(PermissionSystemCommand) != PermissionNo {
		t.Error("Expected no for system_command")
	}
}

func TestPermissionConfigEvaluate(t *testing.T) {
	config := NewPermissionConfig()

	tests := []struct {
		name     string
		global   PermissionLevel
		session  PermissionLevel
		agent    PermissionLevel
		expected PermissionLevel
	}{
		{"global yes ends", PermissionYes, PermissionAsk, PermissionAsk, PermissionYes},
		{"global no ends", PermissionNo, PermissionAsk, PermissionAsk, PermissionNo},
		{"global ask goes to session", PermissionAsk, PermissionYes, PermissionAsk, PermissionYes},
		{"global ask, session no ends", PermissionAsk, PermissionNo, PermissionAsk, PermissionNo},
		{"all ask, returns agent", PermissionAsk, PermissionAsk, PermissionYes, PermissionYes},
		{"all yes, returns global", PermissionYes, PermissionYes, PermissionYes, PermissionYes},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := config.Evaluate(tt.global, tt.session, tt.agent)
			if result != tt.expected {
				t.Errorf("Expected %s, got %s", tt.expected, result)
			}
		})
	}
}

func TestPermissionConfigSaveLoad(t *testing.T) {
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "permissions.json")

	config := NewPermissionConfig()
	config.SetPermission(PermissionSkill, PermissionYes)
	config.SetPermission(PermissionSystemCommand, PermissionNo)

	if err := config.Save(configPath); err != nil {
		t.Errorf("Failed to save: %v", err)
	}

	loaded := NewPermissionConfig()
	if err := loaded.Load(configPath); err != nil {
		t.Errorf("Failed to load: %v", err)
	}

	if loaded.GetPermission(PermissionSkill) != PermissionYes {
		t.Error("Expected yes for skill after loading")
	}

	if loaded.GetPermission(PermissionSystemCommand) != PermissionNo {
		t.Error("Expected no for system_command after loading")
	}
}

func TestPermissionConfigConcurrency(t *testing.T) {
	config := NewPermissionConfig()

	done := make(chan bool, 100)

	// Concurrent reads
	for i := 0; i < 50; i++ {
		go func() {
			config.GetPermission(PermissionSkill)
			done <- true
		}()
	}

	// Concurrent writes
	for i := 0; i < 50; i++ {
		go func(n int) {
			level := PermissionAsk
			if n%2 == 0 {
				level = PermissionYes
			}
			config.SetPermission(PermissionSkill, level)
			done <- true
		}(i)
	}

	for i := 0; i < 100; i++ {
		<-done
	}

	// Should not panic
	_ = config.GetPermission(PermissionSkill)
}

func TestPermissionConfigLoadNonExistent(t *testing.T) {
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "nonexistent.json")

	config := NewPermissionConfig()
	err := config.Load(configPath)

	if err != nil {
		t.Errorf("Expected no error for non-existent file, got: %v", err)
	}
}

func TestPermissionConfigLoadInvalid(t *testing.T) {
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "invalid.json")

	// Write invalid JSON
	if err := os.WriteFile(configPath, []byte("invalid json"), 0644); err != nil {
		t.Fatalf("Failed to write test file: %v", err)
	}

	config := NewPermissionConfig()
	err := config.Load(configPath)

	if err == nil {
		t.Error("Expected error for invalid JSON")
	}
}
