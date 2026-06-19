package fileops

import (
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"
)

// setupGrepFixture builds a small workspace the Grep tests search:
//
//	<a.go>            package main + Foo/Bar funcs
//	sub/b.go          package sub + Foo func
//	sub/note.md       "# Foo" / "foo bar"
//	.git/config       "Foo" (must be skipped)
//
// It returns a FileSystem rooted at the workspace.
func setupGrepFixture(t *testing.T) *FileSystem {
	t.Helper()
	root := t.TempDir()
	fsys := NewFileSystem(root)

	write := func(rel, content string) {
		t.Helper()
		full := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", filepath.Dir(full), err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", rel, err)
		}
	}

	write("a.go", "package main\n\nfunc Foo() {}\nfunc Bar() {}\n")
	write("sub/b.go", "package sub\n\nfunc Foo() {}\n")
	write("sub/note.md", "# Foo\nfoo bar\n")
	write(".git/config", "Foo\n")

	return fsys
}

// TestGrep exercises FileSystem.Grep across output modes and the common options
// against a shared fixture.
func TestGrep(t *testing.T) {
	fsys := setupGrepFixture(t)

	cases := []struct {
		name    string
		pattern string
		opts    GrepOptions
		check   func(t *testing.T, res *GrepResult, err error)
	}{
		{
			name:    "content mode default",
			pattern: "Foo",
			opts:    GrepOptions{},
			check: func(t *testing.T, res *GrepResult, err error) {
				if err != nil {
					t.Fatalf("grep: %v", err)
				}
				if res.Mode != GrepModeContent {
					t.Errorf("mode: got %q want %q", res.Mode, GrepModeContent)
				}
				// Three matches: a.go:3, sub/b.go:3, sub/note.md:1 (.git skipped).
				want := []GrepLine{
					{Path: "a.go", Line: 3, Content: "func Foo() {}"},
					{Path: "sub/b.go", Line: 3, Content: "func Foo() {}"},
					{Path: "sub/note.md", Line: 1, Content: "# Foo"},
				}
				if !reflect.DeepEqual(res.Matches, want) {
					t.Errorf("matches:\n got %+v\nwant %+v", res.Matches, want)
				}
			},
		},
		{
			name:    "files_with_matches mode",
			pattern: "Foo",
			opts:    GrepOptions{OutputMode: GrepModeFiles},
			check: func(t *testing.T, res *GrepResult, err error) {
				if err != nil {
					t.Fatalf("grep: %v", err)
				}
				want := []string{"a.go", "sub/b.go", "sub/note.md"}
				if !reflect.DeepEqual(res.Files, want) {
					t.Errorf("files: got %v want %v", res.Files, want)
				}
				if len(res.Matches) != 0 || len(res.Counts) != 0 {
					t.Errorf("non-file payloads should be empty: %+v / %+v", res.Matches, res.Counts)
				}
			},
		},
		{
			name:    "count mode totals per file",
			pattern: "func",
			opts:    GrepOptions{OutputMode: GrepModeCount},
			check: func(t *testing.T, res *GrepResult, err error) {
				if err != nil {
					t.Fatalf("grep: %v", err)
				}
				want := []GrepFileCount{
					{Path: "a.go", Count: 2}, // Foo + Bar
					{Path: "sub/b.go", Count: 1},
				}
				if !reflect.DeepEqual(res.Counts, want) {
					t.Errorf("counts: got %+v want %+v", res.Counts, want)
				}
			},
		},
		{
			name:    "case insensitive",
			pattern: "foo",
			opts:    GrepOptions{CaseInsensitive: true},
			check: func(t *testing.T, res *GrepResult, err error) {
				if err != nil {
					t.Fatalf("grep: %v", err)
				}
				// a.go:3 Foo, sub/b.go:3 Foo, sub/note.md:1 "# Foo", :2 "foo bar".
				if len(res.Matches) != 4 {
					t.Fatalf("matches: got %d want 4 (%+v)", len(res.Matches), res.Matches)
				}
				var contents []string
				for _, m := range res.Matches {
					if m.Path == "sub/note.md" {
						contents = append(contents, m.Content)
					}
				}
				sort.Strings(contents)
				want := []string{"# Foo", "foo bar"}
				if !reflect.DeepEqual(contents, want) {
					t.Errorf("note.md lines: got %v want %v", contents, want)
				}
			},
		},
		{
			name:    "include glob filters by name",
			pattern: "Foo",
			opts:    GrepOptions{Include: "*.go"},
			check: func(t *testing.T, res *GrepResult, err error) {
				if err != nil {
					t.Fatalf("grep: %v", err)
				}
				got := paths(res)
				want := []string{"a.go", "sub/b.go"}
				if !reflect.DeepEqual(got, want) {
					t.Errorf("paths: got %v want %v (note.md must be excluded)", got, want)
				}
			},
		},
		{
			name:    "path scoped to a single file",
			pattern: "Foo",
			opts:    GrepOptions{Path: "sub/b.go"},
			check: func(t *testing.T, res *GrepResult, err error) {
				if err != nil {
					t.Fatalf("grep: %v", err)
				}
				if len(res.Matches) != 1 || res.Matches[0].Path != "sub/b.go" {
					t.Errorf("want single match in sub/b.go, got %+v", res.Matches)
				}
			},
		},
		{
			name:    "path scoped to a subdirectory",
			pattern: "Foo",
			opts:    GrepOptions{Path: "sub"},
			check: func(t *testing.T, res *GrepResult, err error) {
				if err != nil {
					t.Fatalf("grep: %v", err)
				}
				got := paths(res)
				want := []string{"sub/b.go", "sub/note.md"}
				if !reflect.DeepEqual(got, want) {
					t.Errorf("paths: got %v want %v", got, want)
				}
			},
		},
		{
			name:    "no matches returns empty result",
			pattern: "NoSuchSymbol",
			opts:    GrepOptions{},
			check: func(t *testing.T, res *GrepResult, err error) {
				if err != nil {
					t.Fatalf("grep: %v", err)
				}
				if len(res.Matches) != 0 {
					t.Errorf("want no matches, got %+v", res.Matches)
				}
				if res.Truncated {
					t.Errorf("empty result must not be truncated")
				}
			},
		},
		{
			name:    "max_results truncates content matches",
			pattern: "Foo",
			opts:    GrepOptions{MaxResults: 2},
			check: func(t *testing.T, res *GrepResult, err error) {
				if err != nil {
					t.Fatalf("grep: %v", err)
				}
				if len(res.Matches) != 2 {
					t.Errorf("want 2 matches under cap, got %d", len(res.Matches))
				}
				if !res.Truncated {
					t.Errorf("want truncated=true")
				}
			},
		},
		{
			name:    "git directory is skipped",
			pattern: "Foo",
			opts:    GrepOptions{OutputMode: GrepModeFiles},
			check: func(t *testing.T, res *GrepResult, err error) {
				if err != nil {
					t.Fatalf("grep: %v", err)
				}
				for _, p := range res.Files {
					if filepath.ToSlash(p) == ".git/config" || filepath.Dir(filepath.ToSlash(p)) == ".git" {
						t.Errorf(".git must be skipped, but got %q", p)
					}
				}
			},
		},
		{
			name:    "invalid regex errors",
			pattern: "(unclosed",
			opts:    GrepOptions{},
			check: func(t *testing.T, res *GrepResult, err error) {
				if err == nil {
					t.Fatal("want error for invalid regex, got nil")
				}
			},
		},
		{
			name:    "invalid output_mode errors",
			pattern: "Foo",
			opts:    GrepOptions{OutputMode: "bogus"},
			check: func(t *testing.T, res *GrepResult, err error) {
				if err == nil {
					t.Fatal("want error for invalid output_mode, got nil")
				}
			},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			res, err := fsys.Grep(c.pattern, c.opts)
			c.check(t, res, err)
		})
	}
}

// TestGrepEscapesWorkspace verifies a path outside the workspace is refused, so
// the permission-free tool cannot read beyond its boundary.
func TestGrepEscapesWorkspace(t *testing.T) {
	fsys := setupGrepFixture(t)

	outside := filepath.Join(filepath.Dir(fsys.basePath), "escape.txt")
	if err := os.WriteFile(outside, []byte("Foo\n"), 0o644); err != nil {
		t.Fatalf("write escape file: %v", err)
	}

	for _, path := range []string{outside, "../escape.txt"} {
		if _, err := fsys.Grep("Foo", GrepOptions{Path: path}); err == nil {
			t.Errorf("expected escape rejection for %s", path)
		}
	}
}

// TestGrepPathNotFound verifies a missing search path surfaces a clear error
// rather than a silent empty result.
func TestGrepPathNotFound(t *testing.T) {
	fsys := setupGrepFixture(t)
	if _, err := fsys.Grep("Foo", GrepOptions{Path: "does/not/exist"}); err == nil {
		t.Fatal("expected error for missing path, got nil")
	}
}

// paths collects the matched file paths from a content-mode result, in order.
func paths(res *GrepResult) []string {
	out := make([]string, 0, len(res.Matches))
	for _, m := range res.Matches {
		out = append(out, m.Path)
	}
	return out
}
