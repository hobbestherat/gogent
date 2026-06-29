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
	// OnMutation, when set, is invoked after a successful write with the file's
	// path and whether it was newly created. It is the single seam the LSP Manager
	// subscribes to so a language server's view tracks gogent's own writes (the LSP
	// support design §11.2). Every edit/multi-edit/patch write funnels through
	// Write, so one hook covers them all. nil disables it; it must not block or
	// panic (it runs on the write path).
	OnMutation func(path string, created bool)
	// OnRemove mirrors OnMutation for deletions, so a watched file gogent deletes
	// emits a didChangeWatchedFiles(deleted). nil disables it.
	OnRemove func(path string)
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

	// Notify any subscriber (the LSP Manager) after the write succeeds, so the
	// server's view tracks gogent's own edits (§11.2). Best-effort; never blocks
	// or fails the write.
	if fm.OnMutation != nil {
		fm.OnMutation(path, !existed)
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
	if err := fm.fileSys.Remove(path); err != nil {
		return err
	}
	if fm.OnRemove != nil {
		fm.OnRemove(path)
	}
	return nil
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
// would produce by replacing find→replace, without writing anything. It honours
// the same uniqueness rule as EditFile, so an ambiguous edit is rejected at
// preview time rather than being silently applied.
func (fm *FileMutation) PreviewEdit(path, find, replace string, replaceAll bool, auth Authorization) (before, after string, err error) {
	before, err = fm.currentContent(path, auth)
	if err != nil {
		return "", "", err
	}
	after, err = editContent(before, find, replace, replaceAll)
	if err != nil {
		return "", "", err
	}
	return before, after, nil
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

// EditOp is a single find→replace within a multi-edit. ReplaceAll relaxes the
// uniqueness rule for that one edit (see editContent).
type EditOp struct {
	Find       string
	Replace    string
	ReplaceAll bool
}

// PreviewMultiEdit returns the file's current content and the content a
// MultiEditFile would produce by applying edits in order, without writing
// anything. Each edit honours editContent's uniqueness rule against the running
// result of the prior edits, so an ambiguous or missing match is rejected at
// preview time rather than being half-applied.
func (fm *FileMutation) PreviewMultiEdit(path string, edits []EditOp, auth Authorization) (before, after string, err error) {
	before, err = fm.currentContent(path, auth)
	if err != nil {
		return "", "", err
	}
	after, err = multiEditContent(before, edits)
	if err != nil {
		return "", "", err
	}
	return before, after, nil
}

// MultiEditFile applies a batch of find→replace edits to a file in order. The
// batch is all-or-nothing: if any edit is ambiguous or its find text is absent,
// nothing is written and the file is left untouched. The Authorization is
// forwarded to the underlying reads/writes.
func (fm *FileMutation) MultiEditFile(path string, edits []EditOp, auth Authorization) error {
	current, err := fm.fileSys.Read(path, auth)
	if err != nil {
		return fmt.Errorf("failed to read file: %w", err)
	}

	updated, err := multiEditContent(string(current), edits)
	if err != nil {
		return err
	}

	return fm.WriteFile(path, updated, auth)
}

// multiEditContent folds edits over content in order, each applied to the result
// of the previous one. It fails (returning the original unchanged) the moment any
// edit cannot be applied, so a caller never persists a partially-applied batch.
func multiEditContent(content string, edits []EditOp) (string, error) {
	if len(edits) == 0 {
		return "", fmt.Errorf("no edits provided")
	}
	for i, e := range edits {
		updated, err := editContent(content, e.Find, e.Replace, e.ReplaceAll)
		if err != nil {
			return "", fmt.Errorf("edit %d: %w", i+1, err)
		}
		content = updated
	}
	return content, nil
}

// EditFile edits a file by replacing text. Unless replaceAll is set the find
// text must occur exactly once (see editContent). The Authorization is forwarded
// to the underlying reads/writes.
func (fm *FileMutation) EditFile(path, find, replace string, replaceAll bool, auth Authorization) error {
	current, err := fm.fileSys.Read(path, auth)
	if err != nil {
		return fmt.Errorf("failed to read file: %w", err)
	}

	updated, err := editContent(string(current), find, replace, replaceAll)
	if err != nil {
		return err
	}

	return fm.WriteFile(path, updated, auth)
}

// editContent computes the result of replacing find with replace in content.
// Unless replaceAll is set it requires find to occur exactly once, so an
// ambiguous edit fails loudly instead of silently rewriting every match
// (issue #18). It returns an error when find is empty, absent, or — in unique
// mode — present more than once.
func editContent(content, find, replace string, replaceAll bool) (string, error) {
	if find == "" {
		return "", fmt.Errorf("find text must not be empty")
	}

	count := strings.Count(content, find)
	switch {
	case count == 0:
		return "", fmt.Errorf("no changes made: find text not found in file")
	case count > 1 && !replaceAll:
		return "", fmt.Errorf("find text is not unique: found %d occurrences; provide more surrounding context to identify a single match, or set replace_all to replace every occurrence", count)
	}

	return strings.ReplaceAll(content, find, replace), nil
}
