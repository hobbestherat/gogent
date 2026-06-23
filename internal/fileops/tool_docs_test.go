package fileops

import (
	"strings"
	"testing"
)

func TestFileOperationToolDescriptionsAreActionable(t *testing.T) {
	root := t.TempDir()
	fs := NewFileSystem(root)
	loc := NewLocationMutation(root)
	mut := NewFileMutation(fs, loc)

	tools := []FileTool{
		NewReadTool(fs, loc, nil),
		NewWriteTool(mut, loc, nil),
		NewEditTool(mut, loc, nil),
	}

	for _, tl := range tools {
		t.Run(tl.Name(), func(t *testing.T) {
			desc := strings.TrimSpace(tl.Description())
			if sentenceCount(desc) < 3 {
				t.Fatalf("description is too terse: got %d sentence(s): %q", sentenceCount(desc), desc)
			}

			lower := strings.ToLower(desc)
			if !containsAny(lower, "use ", "read ", "write ", "edit ") {
				t.Errorf("description should explain when to use the tool: %q", desc)
			}
			if !containsAny(lower, "prefer ", "do not", "don't", "not ", "never ", "instead", "only ", "fails ") {
				t.Errorf("description should differentiate alternatives or state limitations: %q", desc)
			}
			if !strings.Contains(lower, "relative") || !strings.Contains(lower, "absolute") {
				t.Errorf("description should document relative and absolute path handling: %q", desc)
			}
		})
	}
}

func sentenceCount(s string) int {
	count := 0
	for _, r := range s {
		switch r {
		case '.', '!', '?':
			count++
		}
	}
	return count
}

func containsAny(s string, needles ...string) bool {
	for _, needle := range needles {
		if strings.Contains(s, needle) {
			return true
		}
	}
	return false
}
