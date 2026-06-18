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

// Write writes content to a file (unconditional). The Authorization is forwarded
// to the underlying file system so an approved external path can be written.
func (fm *FileMutation) Write(path string, content []byte, auth Authorization) (*WriteResult, error) {
	existed, err := fm.fileSys.Exists(path)
	if err != nil {
		return nil, fmt.Errorf("failed to check if file exists: %w", err)
	}

	if err := fm.fileSys.Write(path, content, auth); err != nil {
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

// WriteIfUnchanged writes content only if the file hasn't changed. The
// Authorization is forwarded to the underlying reads/writes.
func (fm *FileMutation) WriteIfUnchanged(path string, expected []byte, content []byte, auth Authorization) (*WriteResult, error) {
	current, err := fm.fileSys.Read(path, auth)
	if err != nil {
		return nil, fmt.Errorf("failed to read file: %w", err)
	}

	if string(current) != string(expected) {
		return nil, &StaleContentError{Path: path}
	}

	return fm.Write(path, content, auth)
}

// Remove removes a file
func (fm *FileMutation) Remove(path string) error {
	return fm.fileSys.Remove(path)
}

// WriteTextPreservingBOM writes text while preserving UTF-8 BOM. The
// Authorization is forwarded to the underlying reads/writes.
func (fm *FileMutation) WriteTextPreservingBOM(path string, content string, auth Authorization) (*WriteResult, error) {
	hasBOM := false
	existed, err := fm.fileSys.Exists(path)
	if err == nil && existed {
		current, _ := fm.fileSys.Read(path, auth)
		if len(current) >= 3 && current[0] == 0xEF && current[1] == 0xBB && current[2] == 0xBF {
			hasBOM = true
		}
	}

	if hasBOM {
		content = "\uFEFF" + content
	}

	return fm.Write(path, []byte(content), auth)
}

// WriteFile writes content to a file. The Authorization is forwarded to the
// underlying writes.
func (fm *FileMutation) WriteFile(path string, content string, auth Authorization) error {
	_, err := fm.WriteTextPreservingBOM(path, content, auth)
	return err
}

// EditFile edits a file by replacing text. The Authorization is forwarded to the
// underlying reads/writes.
func (fm *FileMutation) EditFile(path, find, replace string, auth Authorization) error {
	_, updated, err := fm.PreviewEdit(path, find, replace, auth)
	if err != nil {
		return err
	}
	return fm.WriteFile(path, updated, auth)
}

// PreviewEdit computes the before/after content of an edit without writing it,
// so callers can show a diff and gate the commit behind approval (issue #64).
// The Authorization is forwarded to the underlying read.
func (fm *FileMutation) PreviewEdit(path, find, replace string, auth Authorization) (before, after string, err error) {
	current, err := fm.fileSys.Read(path, auth)
	if err != nil {
		return "", "", fmt.Errorf("failed to read file: %w", err)
	}
	before = string(current)
	return before, strings.ReplaceAll(before, find, replace), nil
}

// PreviewWrite returns the current content of path (empty when the file does not
// yet exist) so callers can diff it against the content they are about to write.
// The Authorization is forwarded to the underlying read.
func (fm *FileMutation) PreviewWrite(path string, auth Authorization) (before string, err error) {
	existed, err := fm.fileSys.Exists(path)
	if err != nil {
		return "", fmt.Errorf("failed to check if file exists: %w", err)
	}
	if !existed {
		return "", nil
	}
	current, err := fm.fileSys.Read(path, auth)
	if err != nil {
		return "", fmt.Errorf("failed to read file: %w", err)
	}
	return string(current), nil
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
