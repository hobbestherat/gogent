package fileops

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// FileInfo represents information about a file or directory
type FileInfo struct {
	Path  string
	Name  string
	IsDir bool
	Size  int64
	Mode  fs.FileMode
}

// FileSystem abstracts file system operations
type FileSystem struct {
	basePath string
}

// NewFileSystem creates a new file system service
func NewFileSystem(workspaceRoot string) *FileSystem {
	return &FileSystem{
		basePath: filepath.Clean(workspaceRoot),
	}
}

// Read reads a file and returns its contents
func (fsys *FileSystem) Read(path string) ([]byte, error) {
	resolved, err := filepath.Abs(filepath.Join(fsys.basePath, path))
	if err != nil {
		return nil, fmt.Errorf("failed to resolve path: %w", err)
	}

	rel, err := filepath.Rel(fsys.basePath, resolved)
	if err != nil || strings.HasPrefix(rel, "..") {
		return nil, fmt.Errorf("path escapes workspace: %s", path)
	}

	data, err := os.ReadFile(resolved)
	if err != nil {
		return nil, fmt.Errorf("failed to read file: %w", err)
	}

	return data, nil
}

// Write writes content to a file
func (fsys *FileSystem) Write(path string, content []byte) error {
	resolved, err := filepath.Abs(filepath.Join(fsys.basePath, path))
	if err != nil {
		return fmt.Errorf("failed to resolve path: %w", err)
	}

	rel, err := filepath.Rel(fsys.basePath, resolved)
	if err != nil || strings.HasPrefix(rel, "..") {
		return fmt.Errorf("path escapes workspace: %s", path)
	}

	dir := filepath.Dir(resolved)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("failed to create directory: %w", err)
	}

	if err := os.WriteFile(resolved, content, 0644); err != nil {
		return fmt.Errorf("failed to write file: %w", err)
	}

	return nil
}

// List lists files and directories in a path
func (fsys *FileSystem) List(path string) ([]FileInfo, error) {
	resolved, err := filepath.Abs(filepath.Join(fsys.basePath, path))
	if err != nil {
		return nil, fmt.Errorf("failed to resolve path: %w", err)
	}

	rel, err := filepath.Rel(fsys.basePath, resolved)
	if err != nil || strings.HasPrefix(rel, "..") {
		return nil, fmt.Errorf("path escapes workspace: %s", path)
	}

	entries, err := os.ReadDir(resolved)
	if err != nil {
		return nil, fmt.Errorf("failed to list directory: %w", err)
	}

	var infos []FileInfo
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil {
			continue
		}

		infos = append(infos, FileInfo{
			Path:  filepath.Join(path, entry.Name()),
			Name:  entry.Name(),
			IsDir: entry.IsDir(),
			Size:  info.Size(),
			Mode:  info.Mode(),
		})
	}

	return infos, nil
}

// Glob matches files using a glob pattern
func (fsys *FileSystem) Glob(pattern string) ([]string, error) {
	resolved, err := filepath.Abs(filepath.Join(fsys.basePath, pattern))
	if err != nil {
		return nil, fmt.Errorf("failed to resolve pattern: %w", err)
	}

	rel, err := filepath.Rel(fsys.basePath, resolved)
	if err != nil || strings.HasPrefix(rel, "..") {
		return nil, fmt.Errorf("pattern escapes workspace: %s", pattern)
	}

	matches, err := filepath.Glob(resolved)
	if err != nil {
		return nil, fmt.Errorf("failed to glob: %w", err)
	}

	var relativeMatches []string
	for _, match := range matches {
		rel, err := filepath.Rel(fsys.basePath, match)
		if err != nil {
			continue
		}
		relativeMatches = append(relativeMatches, rel)
	}

	return relativeMatches, nil
}

// Exists checks if a file or directory exists
func (fsys *FileSystem) Exists(path string) (bool, error) {
	_, err := os.Stat(filepath.Join(fsys.basePath, path))
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, fmt.Errorf("failed to stat file: %w", err)
	}
	return true, nil
}

// Remove removes a file or directory
func (fsys *FileSystem) Remove(path string) error {
	resolved, err := filepath.Abs(filepath.Join(fsys.basePath, path))
	if err != nil {
		return fmt.Errorf("failed to resolve path: %w", err)
	}

	rel, err := filepath.Rel(fsys.basePath, resolved)
	if err != nil || strings.HasPrefix(rel, "..") {
		return fmt.Errorf("path escapes workspace: %s", path)
	}

	if err := os.RemoveAll(resolved); err != nil {
		return fmt.Errorf("failed to remove: %w", err)
	}

	return nil
}

// ReadFile reads a file and returns its contents as a string
func (fsys *FileSystem) ReadFile(path string) (string, error) {
	data, err := fsys.Read(path)
	if err != nil {
		return "", err
	}
	return string(data), nil
}
