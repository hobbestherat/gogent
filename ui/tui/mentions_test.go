package ui

import (
	"reflect"
	"strings"
	"testing"
)

// TestParseMentions covers mention extraction: leading boundary, whitespace
// boundary, trailing-punctuation trimming, de-duplication, and that an email
// address's '@' (no boundary before it) is not treated as a mention.
func TestParseMentions(t *testing.T) {
	for _, tc := range []struct {
		name string
		text string
		want []string
	}{
		{"none", "just some prose", nil},
		{"leading", "@main.go please review", []string{"main.go"}},
		{"mid sentence", "look at @internal/agent/agent.go now", []string{"internal/agent/agent.go"}},
		{"trailing punctuation", "see @main.go, then @README.md.", []string{"main.go", "README.md"}},
		{"dedup", "@a.go and again @a.go", []string{"a.go"}},
		{"multiple", "@a.go @b.go", []string{"a.go", "b.go"}},
		{"email is not a mention", "ping yves@jacoby.xyz about it", nil},
		{"bare at", "@ nothing", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := parseMentions(tc.text)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("parseMentions(%q) = %v, want %v", tc.text, got, tc.want)
			}
		})
	}
}

// TestMentionToken covers the live completer trigger: an active token right after
// '@', a partial path, the word-boundary requirement (an email never triggers),
// and a whitespace break ending the token.
func TestMentionToken(t *testing.T) {
	for _, tc := range []struct {
		name      string
		line      string
		cursor    int
		wantStart int
		wantQuery string
		wantOK    bool
	}{
		{"just after at", "@", 1, 0, "", true},
		{"partial path", "see @inter", 10, 4, "inter", true},
		{"at start with path", "@main.go", 5, 0, "main", true},
		{"whitespace breaks token", "@a b", 4, 0, "", false},
		{"email not a mention", "yves@host", 9, 0, "", false},
		{"no at on line", "hello", 5, 0, "", false},
		{"cursor before at", "x @ab", 1, 0, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			start, query, ok := mentionToken([]rune(tc.line), tc.cursor)
			if ok != tc.wantOK || query != tc.wantQuery || (ok && start != tc.wantStart) {
				t.Fatalf("mentionToken(%q, %d) = (%d, %q, %v), want (%d, %q, %v)",
					tc.line, tc.cursor, start, query, ok, tc.wantStart, tc.wantQuery, tc.wantOK)
			}
		})
	}
}

// TestFilterPaths checks fuzzy filtering ranks the closer subsequence first, an
// empty query keeps lexical order, a non-match is dropped, and the limit caps the
// result.
func TestFilterPaths(t *testing.T) {
	paths := []string{"ui/util.go", "internal/agent/agent.go", "main.go", "README.md"}

	all := filterPaths(paths, "", 0)
	if !reflect.DeepEqual(all, paths) {
		t.Fatalf("empty query should preserve order: got %v", all)
	}

	got := filterPaths(paths, "agent", 0)
	if len(got) != 1 || got[0] != "internal/agent/agent.go" {
		t.Fatalf("query \"agent\" = %v, want [internal/agent/agent.go]", got)
	}

	if got := filterPaths(paths, "zzz", 0); len(got) != 0 {
		t.Fatalf("non-matching query should return nothing, got %v", got)
	}

	if got := filterPaths(paths, "", 2); len(got) != 2 {
		t.Fatalf("limit should cap to 2, got %d (%v)", len(got), got)
	}

	// "uiutil" is a subsequence of "ui/util.go" but not of the others.
	if got := filterPaths(paths, "uiutil", 0); len(got) != 1 || got[0] != "ui/util.go" {
		t.Fatalf("query \"uiutil\" = %v, want [ui/util.go]", got)
	}
}

// TestExpandMentions covers expansion into attached content, leaving unresolved
// mentions in place, returning the message unchanged when nothing resolves, the
// nil-reader path, and truncation of an oversized file.
func TestExpandMentions(t *testing.T) {
	files := map[string]string{
		"main.go":   "package main",
		"README.md": "# gogent",
	}
	read := func(path string) (string, bool) {
		c, ok := files[path]
		return c, ok
	}

	t.Run("attaches resolved files", func(t *testing.T) {
		msg, attached := expandMentions("review @main.go and @README.md", read)
		if !reflect.DeepEqual(attached, []string{"main.go", "README.md"}) {
			t.Fatalf("attached = %v", attached)
		}
		if !strings.Contains(msg, "review @main.go and @README.md") {
			t.Fatalf("original prose missing from expansion: %q", msg)
		}
		for _, want := range []string{"===== main.go =====", "package main", "===== README.md =====", "# gogent"} {
			if !strings.Contains(msg, want) {
				t.Fatalf("expansion missing %q in %q", want, msg)
			}
		}
	})

	t.Run("unresolved mention left as-is", func(t *testing.T) {
		msg, attached := expandMentions("see @nope.go", read)
		if attached != nil {
			t.Fatalf("attached = %v, want nil", attached)
		}
		if msg != "see @nope.go" {
			t.Fatalf("message should be unchanged, got %q", msg)
		}
	})

	t.Run("no mentions returns input unchanged", func(t *testing.T) {
		msg, attached := expandMentions("plain message", read)
		if msg != "plain message" || attached != nil {
			t.Fatalf("got (%q, %v)", msg, attached)
		}
	})

	t.Run("nil reader is a no-op", func(t *testing.T) {
		msg, attached := expandMentions("@main.go", nil)
		if msg != "@main.go" || attached != nil {
			t.Fatalf("got (%q, %v)", msg, attached)
		}
	})

	t.Run("oversized file is truncated", func(t *testing.T) {
		big := strings.Repeat("a", maxMentionFileBytes+100)
		bigRead := func(path string) (string, bool) {
			if path == "big.txt" {
				return big, true
			}
			return "", false
		}
		msg, attached := expandMentions("@big.txt", bigRead)
		if !reflect.DeepEqual(attached, []string{"big.txt"}) {
			t.Fatalf("attached = %v", attached)
		}
		if !strings.Contains(msg, "[truncated]") {
			t.Fatalf("expected truncation marker in %q", msg[:80])
		}
		if strings.Count(msg, "a") >= len(big) {
			t.Fatal("expected the attached content to be shortened")
		}
	})
}
