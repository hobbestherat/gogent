package permission

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
)

// PermissionAction represents an action that can be permitted
type PermissionAction string

const (
	PermissionSkill         PermissionAction = "skill"
	PermissionSystemCommand PermissionAction = "system_command"
	PermissionNetwork       PermissionAction = "network"
	PermissionFile          PermissionAction = "file"
	PermissionTool          PermissionAction = "tool"
)

// PermissionLevel represents the permission level
type PermissionLevel string

const (
	PermissionYes PermissionLevel = "yes"
	PermissionNo  PermissionLevel = "no"
	PermissionAsk PermissionLevel = "ask"
)

// PermissionConfig holds the permission configuration
type PermissionConfig struct {
	Permissions map[string]PermissionLevel `json:"permissions"`
	mu          sync.RWMutex
}

// NewPermissionConfig creates a new permission config
func NewPermissionConfig() *PermissionConfig {
	return &PermissionConfig{
		Permissions: make(map[string]PermissionLevel),
	}
}

// Load loads the permission config from a file
func (c *PermissionConfig) Load(path string) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	data, err := os.ReadFile(path)
	if err != nil {
		// File doesn't exist, return empty config
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}

	return json.Unmarshal(data, c)
}

// Save saves the permission config to a file
func (c *PermissionConfig) Save(path string) error {
	c.mu.RLock()
	defer c.mu.RUnlock()

	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}

	return os.WriteFile(path, data, 0644)
}

// GetPermission gets the permission level for an action
func (c *PermissionConfig) GetPermission(action PermissionAction) PermissionLevel {
	c.mu.RLock()
	defer c.mu.RUnlock()

	if level, ok := c.Permissions[string(action)]; ok {
		return level
	}
	return PermissionAsk // Default to ask if not set
}

// SetPermission sets the permission level for an action
func (c *PermissionConfig) SetPermission(action PermissionAction, level PermissionLevel) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.Permissions[string(action)] = level
}

// Evaluate evaluates permission with three-level hierarchy
// global -> session -> agent
func (c *PermissionConfig) Evaluate(
	globalLevel PermissionLevel,
	sessionLevel PermissionLevel,
	agentLevel PermissionLevel,
) PermissionLevel {
	// Top-down evaluation with short-circuit
	if globalLevel != PermissionAsk {
		return globalLevel
	}
	if sessionLevel != PermissionAsk {
		return sessionLevel
	}
	return agentLevel
}

// GetPermissionWithFallback gets permission with fallback to default
func (c *PermissionConfig) GetPermissionWithFallback(action PermissionAction, defaultLevel PermissionLevel) PermissionLevel {
	level := c.GetPermission(action)
	if level == PermissionAsk {
		return defaultLevel
	}
	return level
}

// LoadPermissionConfig loads the permission config from the gogent config directory
func LoadPermissionConfig(configDir string) (*PermissionConfig, error) {
	configPath := filepath.Join(configDir, "permissions.json")
	config := NewPermissionConfig()

	if err := config.Load(configPath); err != nil {
		return nil, err
	}

	return config, nil
}

// SavePermissionConfig saves the permission config to the gogent config directory
func SavePermissionConfig(configDir string, config *PermissionConfig) error {
	if err := os.MkdirAll(configDir, 0755); err != nil {
		return err
	}

	configPath := filepath.Join(configDir, "permissions.json")
	return config.Save(configPath)
}
