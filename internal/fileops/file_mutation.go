package fileops

import (
	"fmt"
	"strings"
)

// Target represents a file target for mutation
type Target struct {
	Path       string
	Resource   string
	IsExternal bool
}

// WriteResult represents the result of a write operation
type WriteResult struct {
	Operation string
	Path      string
	Resource  string
	Existed   bool
}

// StaleContentError is returned when content has changed
type StaleContentError struct {
	Path string
}

func (e *StaleContentError) Error() string {
	return "file content has changed: " + e.Path
}

// FileMutation provides safe file mutation operations
type FileMutation struct {
	fileSys  *FileSystem
	location *LocationMutation
}

// NewFileMutation creates a new file mutation service
func NewFileMutation(fileSys *FileSystem, location *LocationMutation) *FileMutation {
	return &FileMutation{
		fileSys:  fileSys,
		location: location,
	}
}

// Write writes content to a file (unconditional)
func (fm *FileMutation) Write(path string, content []byte) (*WriteResult, error) {
	existed, err := fm.fileSys.Exists(path)
	if err != nil {
		return nil, fmt.Errorf("failed to check if file exists: %w", err)
	}

	if err := fm.fileSys.Write(path, content); err != nil {
		return nil, fmt.Errorf("failed to write file: %w", err)
	}

	resource, err := fm.location.GetResource(path)
	if err != nil {
		return nil, err
	}

	return &WriteResult{
		Operation: "write",
		Path:      path,
		Resource:  resource,
		Existed:   existed,
	}, nil
}

// WriteIfUnchanged writes content only if the file hasn't changed
func (fm *FileMutation) WriteIfUnchanged(path string, expected []byte, content []byte) (*WriteResult, error) {
	current, err := fm.fileSys.Read(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read file: %w", err)
	}

	if string(current) != string(expected) {
		return nil, &StaleContentError{Path: path}
	}

	return fm.Write(path, content)
}

// Remove removes a file
func (fm *FileMutation) Remove(path string) error {
	return fm.fileSys.Remove(path)
}

// WriteTextPreservingBOM writes text while preserving UTF-8 BOM
func (fm *FileMutation) WriteTextPreservingBOM(path string, content string) (*WriteResult, error) {
	hasBOM := false
	existed, err := fm.fileSys.Exists(path)
	if err == nil && existed {
		current, _ := fm.fileSys.Read(path)
		if len(current) >= 3 && current[0] == 0xEF && current[1] == 0xBB && current[2] == 0xBF {
			hasBOM = true
		}
	}

	if hasBOM {
		content = "\uFEFF" + content
	}

	return fm.Write(path, []byte(content))
}

// WriteFile writes content to a file
func (fm *FileMutation) WriteFile(path string, content string) error {
	_, err := fm.WriteTextPreservingBOM(path, content)
	return err
}

// EditFile edits a file by replacing text
func (fm *FileMutation) EditFile(path, find, replace string) error {
	current, err := fm.fileSys.Read(path)
	if err != nil {
		return fmt.Errorf("failed to read file: %w", err)
	}

	updated := strings.ReplaceAll(string(current), find, replace)

	return fm.WriteFile(path, updated)
}

// replaceString replaces all occurrences of old with new
func replaceString(s, old, new string) string {
	if old == "" {
		return s
	}
	return replaceAll(s, old, new)
}

func replaceAll(s, old, new string) string {
	if old == "" {
		return s
	}

	var result string
	lastIndex := 0

	for i := 0; i <= len(s)-len(old); i++ {
		if s[i:i+len(old)] == old {
			result += s[lastIndex:i] + new
			lastIndex = i + len(old)
			i += len(old) - 1
		}
	}

	result += s[lastIndex:]
	return result
}
