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

// PreviewWrite returns the file's current content and the content a Write would
// produce, without writing anything. The "before" is empty when the file does
// not yet exist. It backs the diff preview shown before an approved write
// (issue #64).
func (fm *FileMutation) PreviewWrite(path, content string, auth Authorization) (before, after string, err error) {
	before, err = fm.currentContent(path, auth)
	if err != nil {
		return "", "", err
	}
	return before, content, nil
}

// PreviewEdit returns the file's current content and the content an EditFile
// would produce by replacing find→replace, without writing anything.
func (fm *FileMutation) PreviewEdit(path, find, replace string, auth Authorization) (before, after string, err error) {
	before, err = fm.currentContent(path, auth)
	if err != nil {
		return "", "", err
	}
	return before, strings.ReplaceAll(before, find, replace), nil
}

// currentContent reads a file's content for diff preview, returning "" (and no
// error) when the file does not exist yet. A leading UTF-8 BOM is stripped so the
// preview is not polluted by the BOM that WriteTextPreservingBOM transparently
// re-adds on write.
func (fm *FileMutation) currentContent(path string, auth Authorization) (string, error) {
	existed, err := fm.fileSys.Exists(path)
	if err != nil {
		return "", err
	}
	if !existed {
		return "", nil
	}
	data, err := fm.fileSys.Read(path, auth)
	if err != nil {
		return "", err
	}
	return strings.TrimPrefix(string(data), "\uFEFF"), nil
}

// EditFile edits a file by replacing text. The Authorization is forwarded to the
// underlying reads/writes.
func (fm *FileMutation) EditFile(path, find, replace string, auth Authorization) error {
	current, err := fm.fileSys.Read(path, auth)
	if err != nil {
		return fmt.Errorf("failed to read file: %w", err)
	}

	updated := strings.ReplaceAll(string(current), find, replace)

	return fm.WriteFile(path, updated, auth)
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
