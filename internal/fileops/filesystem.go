package fileops

import (
	"bufio"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
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

// maxWorkspaceFiles caps WorkspaceFiles when the caller passes no limit, so an
// enormous tree cannot flood the @-mention completer that consumes the listing.
const maxWorkspaceFiles = 5000

// WorkspaceFiles returns every regular file in the workspace as a path relative
// to the workspace root, in lexical (WalkDir) order, skipping the .git directory
// (mirroring Grep). It backs the TUI's @-mention file completer (issue #46): a
// read-only, workspace-confined listing the UI can offer so the user can attach
// a file to a message without the model having to discover it. limit caps the
// number of paths returned (a non-positive limit falls back to
// maxWorkspaceFiles); the returned bool reports whether the cap truncated the
// listing.
func (fsys *FileSystem) WorkspaceFiles(limit int) (files []string, truncated bool) {
	if limit <= 0 {
		limit = maxWorkspaceFiles
	}
	_ = filepath.WalkDir(fsys.basePath, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // skip entries we cannot stat
		}
		if d.IsDir() {
			if d.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		rel, relErr := filepath.Rel(fsys.basePath, path)
		if relErr != nil {
			return nil
		}
		files = append(files, rel)
		if len(files) >= limit {
			truncated = true
			return filepath.SkipAll
		}
		return nil
	})
	return files, truncated
}

// Output modes for Grep. They select the shape of a GrepResult.
const (
	GrepModeContent = "content"            // every matched line, with its file:line
	GrepModeFiles   = "files_with_matches" // only the paths that contain a match
	GrepModeCount   = "count"              // one match total per file
)

// grepDefaultMaxResults bounds a Grep search when the caller sets no cap, so a
// broad pattern cannot flood the model with thousands of lines.
const grepDefaultMaxResults = 200

// grepMaxLinePreview truncates an individual matched line in content mode so one
// huge or minified line cannot dominate the result.
const grepMaxLinePreview = 1000

// GrepLine is a single matched line, for the content output mode.
type GrepLine struct {
	Path    string
	Line    int
	Content string
}

// GrepFileCount is a per-file match tally, for the count output mode.
type GrepFileCount struct {
	Path  string
	Count int
}

// GrepOptions controls a Grep search.
type GrepOptions struct {
	// Path scopes the search to a file or directory, relative to the workspace
	// root (or absolute). Empty means the whole workspace.
	Path string
	// OutputMode selects the result shape; empty defaults to content.
	OutputMode string
	// CaseInsensitive makes the pattern match regardless of letter case.
	CaseInsensitive bool
	// Include restricts the search to files whose base name matches this glob
	// (e.g. "*.go"). Empty means no name filter.
	Include string
	// MaxResults caps the number of matches (content) or matching files
	// (files_with_matches/count) returned. Zero falls back to a default; a
	// negative value disables the cap.
	MaxResults int
}

// GrepResult holds the outcome of a Grep search. Exactly one of Matches, Files or
// Counts is populated, matching the chosen output mode; Truncated reports that
// MaxResults cut the result short.
type GrepResult struct {
	Mode      string
	Pattern   string
	Matches   []GrepLine
	Files     []string
	Counts    []GrepFileCount
	Truncated bool
}

// Grep searches file contents under the workspace for a regular expression. It
// is the primitive behind the model-callable "grep" tool: it walks the
// (workspace-confined) Path, reads each file line by line, and reports matches
// according to OutputMode. It is read-only and never leaves the workspace, so it
// needs no permission gate — unlike routing the same search through the shell, it
// prompts nothing and returns structured file:line results the caller can feed
// back to Read. The .git directory and files that cannot be read (binary, no
// permission, lines beyond the scan cap) are skipped. Path may name a single file
// or a directory; empty means the workspace root. Results are in lexical path
// order (filepath.WalkDir order) for deterministic output.
func (fsys *FileSystem) Grep(pattern string, opts GrepOptions) (*GrepResult, error) {
	mode := opts.OutputMode
	switch mode {
	case "", GrepModeContent:
		mode = GrepModeContent
	case GrepModeFiles, GrepModeCount:
	default:
		return nil, fmt.Errorf("invalid output_mode %q (want %s, %s or %s)",
			mode, GrepModeContent, GrepModeFiles, GrepModeCount)
	}

	expr := pattern
	if opts.CaseInsensitive {
		expr = "(?i)" + expr
	}
	re, err := regexp.Compile(expr)
	if err != nil {
		return nil, fmt.Errorf("invalid pattern: %w", err)
	}

	root := opts.Path
	if root == "" {
		root = "."
	}
	resolved, err := fsys.resolve(root)
	if err != nil {
		return nil, err
	}
	if err := fsys.ensureWithin(resolved, root); err != nil {
		return nil, err
	}
	if _, err := os.Stat(resolved); err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("path not found: %s", opts.Path)
		}
		return nil, fmt.Errorf("failed to stat path: %w", err)
	}

	max := opts.MaxResults
	capped := max > 0
	if max == 0 {
		max = grepDefaultMaxResults
	}
	collectLines := mode == GrepModeContent
	stopAfterFirst := mode == GrepModeFiles

	res := &GrepResult{Mode: mode, Pattern: pattern}
	stop := false
	walkErr := filepath.WalkDir(resolved, func(path string, d fs.DirEntry, err error) error {
		if stop {
			return filepath.SkipDir
		}
		if err != nil {
			return nil // skip entries we cannot stat
		}
		if d.IsDir() {
			if d.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		if opts.Include != "" {
			if matched, _ := filepath.Match(opts.Include, d.Name()); !matched {
				return nil
			}
		}
		lines, n, ok := grepFile(path, re, collectLines, stopAfterFirst)
		if !ok {
			return nil
		}
		rel, relErr := filepath.Rel(fsys.basePath, path)
		if relErr != nil {
			rel = path
		}
		switch mode {
		case GrepModeContent:
			for _, ml := range lines {
				res.Matches = append(res.Matches, GrepLine{Path: rel, Line: ml.Line, Content: ml.Content})
				if capped && len(res.Matches) >= max {
					res.Truncated = true
					stop = true
					return nil
				}
			}
		case GrepModeFiles:
			res.Files = append(res.Files, rel)
			if capped && len(res.Files) >= max {
				res.Truncated = true
				stop = true
				return nil
			}
		case GrepModeCount:
			res.Counts = append(res.Counts, GrepFileCount{Path: rel, Count: n})
			if capped && len(res.Counts) >= max {
				res.Truncated = true
				stop = true
				return nil
			}
		}
		return nil
	})
	if walkErr != nil {
		return nil, fmt.Errorf("failed to search: %w", walkErr)
	}
	return res, nil
}

// grepFile scans one file for regex matches. It returns the matched lines (only
// when collectLines is true), the total number of matches, and whether the file
// had at least one match. stopAfterFirst short-circuits after the first match, as
// used by the files_with_matches mode where the count is irrelevant. Files that
// cannot be opened or scanned (missing, binary, no permission, oversized lines)
// report no match rather than aborting the whole search.
func grepFile(path string, re *regexp.Regexp, collectLines, stopAfterFirst bool) (lines []GrepLine, n int, ok bool) {
	f, err := os.Open(path)
	if err != nil {
		return nil, 0, false
	}
	defer func() { _ = f.Close() }()

	scanner := bufio.NewScanner(f)
	// Raise the per-line token cap so long (but legitimate) lines are still
	// searched; a line beyond the cap ends the scan and the file is skipped.
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)

	lineNo := 0
	for scanner.Scan() {
		lineNo++
		if !re.MatchString(scanner.Text()) {
			continue
		}
		n++
		if collectLines {
			content := scanner.Text()
			if len(content) > grepMaxLinePreview {
				content = content[:grepMaxLinePreview] + " …[truncated]"
			}
			lines = append(lines, GrepLine{Line: lineNo, Content: content})
		}
		if stopAfterFirst {
			break
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, 0, false
	}
	return lines, n, n > 0
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
