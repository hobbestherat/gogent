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
	base, err := filepath.Abs(filepath.Clean(workspaceRoot))
	if err != nil {
		base = filepath.Clean(workspaceRoot)
	}
	return &FileSystem{
		basePath: base,
	}
}

// resolve turns a caller-supplied path into a cleaned absolute path. Absolute
// paths are honored as-is; relative paths are resolved against the workspace
// root. This mirrors how common agent harnesses treat tool paths and avoids
// re-rooting an absolute path under the workspace (which previously produced
// phantom directories like <workspace>/Users/...).
func (fsys *FileSystem) resolve(path string) (string, error) {
	if path == "" {
		return "", fmt.Errorf("empty path")
	}
	var joined string
	if filepath.IsAbs(path) {
		joined = filepath.Clean(path)
	} else {
		joined = filepath.Join(fsys.basePath, path)
	}
	resolved, err := filepath.Abs(joined)
	if err != nil {
		return "", fmt.Errorf("failed to resolve path: %w", err)
	}
	return resolved, nil
}

// ensureWithin returns an error if resolved is outside the workspace root.
func (fsys *FileSystem) ensureWithin(resolved, original string) error {
	rel, err := filepath.Rel(fsys.basePath, resolved)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("path escapes workspace: %s", original)
	}
	return nil
}

// Abs returns the absolute path a Read/Write would act on for the given path,
// honoring the workspace root exactly as the mutating operations do (absolute
// paths are used as-is; relative paths are joined under the root). It only
// resolves — it does not enforce the workspace boundary, which remains Read's and
// Write's concern. It lets callers (e.g. the checkpointer) key files by the same
// canonical path the file operations touch.
func (fsys *FileSystem) Abs(path string) (string, error) {
	return fsys.resolve(path)
}

// Read reads a file and returns its contents. The Authorization relaxes the
// workspace boundary for paths that CheckFileAccess approved as external; pass a
// zero Authorization (or one obtained for a workspace path) to keep file reads
// confined to the workspace.
func (fsys *FileSystem) Read(path string, auth Authorization) ([]byte, error) {
	resolved, err := fsys.resolve(path)
	if err != nil {
		return nil, err
	}

	if !auth.external {
		if err := fsys.ensureWithin(resolved, path); err != nil {
			return nil, err
		}
	}

	data, err := os.ReadFile(resolved)
	if err != nil {
		return nil, fmt.Errorf("failed to read file: %w", err)
	}

	return data, nil
}

// Write writes content to a file. See Read for the Authorization semantics.
func (fsys *FileSystem) Write(path string, content []byte, auth Authorization) error {
	resolved, err := fsys.resolve(path)
	if err != nil {
		return err
	}

	if !auth.external {
		if err := fsys.ensureWithin(resolved, path); err != nil {
			return err
		}
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
	resolved, err := fsys.resolve(path)
	if err != nil {
		return nil, err
	}

	if err := fsys.ensureWithin(resolved, path); err != nil {
		return nil, err
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
	resolved, err := fsys.resolve(pattern)
	if err != nil {
		return nil, err
	}

	if err := fsys.ensureWithin(resolved, pattern); err != nil {
		return nil, err
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
	resolved, err := fsys.resolve(path)
	if err != nil {
		return false, err
	}

	_, err = os.Stat(resolved)
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
	resolved, err := fsys.resolve(path)
	if err != nil {
		return err
	}

	if err := fsys.ensureWithin(resolved, path); err != nil {
		return err
	}

	if err := os.RemoveAll(resolved); err != nil {
		return fmt.Errorf("failed to remove: %w", err)
	}

	return nil
}

// ReadFile reads a file and returns its contents as a string. See Read for the
// Authorization semantics.
func (fsys *FileSystem) ReadFile(path string, auth Authorization) (string, error) {
	data, err := fsys.Read(path, auth)
	if err != nil {
		return "", err
	}
	return string(data), nil
}
