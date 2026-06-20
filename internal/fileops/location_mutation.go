package fileops

import (
	"fmt"
	"path/filepath"
	"strings"
)

// LocationMutation handles path resolution and boundary enforcement
type LocationMutation struct {
	workspaceRoot string
}

// NewLocationMutation creates a new LocationMutation
func NewLocationMutation(workspaceRoot string) *LocationMutation {
	base, err := filepath.Abs(filepath.Clean(workspaceRoot))
	if err != nil {
		base = filepath.Clean(workspaceRoot)
	}
	return &LocationMutation{
		workspaceRoot: base,
	}
}

// ResolvePath resolves a path relative to workspace or returns absolute path
func (lm *LocationMutation) ResolvePath(path string) (string, error) {
	if filepath.IsAbs(path) {
		return filepath.Clean(path), nil
	}
	return filepath.Clean(filepath.Join(lm.workspaceRoot, path)), nil
}

// IsExternal checks if a path is outside the workspace
func (lm *LocationMutation) IsExternal(path string) (bool, error) {
	resolved, err := lm.ResolvePath(path)
	if err != nil {
		return false, err
	}

	rel, err := filepath.Rel(lm.workspaceRoot, resolved)
	if err != nil {
		return false, fmt.Errorf("compute relative path: %w", err)
	}

	// If path starts with .., it's external
	return rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)), nil
}

// GetResource returns the permission resource (relative path from workspace)
func (lm *LocationMutation) GetResource(path string) (string, error) {
	resolved, err := lm.ResolvePath(path)
	if err != nil {
		return "", err
	}

	rel, err := filepath.Rel(lm.workspaceRoot, resolved)
	if err != nil {
		return "", fmt.Errorf("compute relative path: %w", err)
	}

	if rel == "." {
		return ".", nil
	}

	// Convert to forward slashes for consistency
	return strings.ReplaceAll(rel, "\\", "/"), nil
}

// ExternalDirectoryPermission creates a permission request for external directory access
func (lm *LocationMutation) ExternalDirectoryPermission(externalPath string) (action string, resource string) {
	dir := filepath.Dir(externalPath)
	return "external_directory", strings.ReplaceAll(dir, "\\", "/")
}
