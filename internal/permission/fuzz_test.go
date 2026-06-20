package permission

import (
	"path/filepath"
	"strings"
	"testing"
)

// FuzzPathUnder exercises the path-containment predicate that gates external,
// read and write access. The security-critical invariant is soundness: if
// pathUnder reports a child as nested under a parent, the cleaned child must not
// escape the parent via "..". A false positive here would let a granted root
// silently cover paths outside it.
func FuzzPathUnder(f *testing.F) {
	seeds := [][2]string{
		{"/etc", "/etc/hosts"},
		{"/etc", "/etc"},
		{"/etc", "/etcd/x"},
		{"/a/b", "/a/b/../c"},
		{"/a/b", "/a"},
		{"", "/a"},
		{"/a", ""},
		{"/", "/anything"},
		{"relative", "relative/child"},
	}
	for _, s := range seeds {
		f.Add(s[0], s[1])
	}

	f.Fuzz(func(t *testing.T, parent, child string) {
		under := pathUnder(child, parent)

		// Reflexivity: a non-empty path is always under itself.
		if parent != "" && !pathUnder(parent, parent) {
			t.Fatalf("pathUnder not reflexive for %q", parent)
		}

		// Soundness: a reported containment must be backed by a relative path
		// that does not climb above the parent.
		if under && parent != "" && child != "" && child != parent {
			rel, err := filepath.Rel(parent, child)
			if err != nil {
				t.Fatalf("pathUnder(%q,%q)=true but filepath.Rel failed: %v", child, parent, err)
			}
			if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
				t.Fatalf("pathUnder(%q,%q)=true but rel %q escapes parent", child, parent, rel)
			}
		}
	})
}

// FuzzWildcardMatch checks the resource matcher used by allow/deny rules. The
// contract is narrow: "*" matches everything, a trailing "*" is a prefix match,
// and any other pattern requires exact equality. Fuzzing guards against panics
// and against the prefix/exact branches drifting apart.
func FuzzWildcardMatch(f *testing.F) {
	seeds := [][2]string{
		{"foo.txt", "*"},
		{"secret.txt", "secret*"},
		{"foo", "foo"},
		{"foo", "bar"},
		{"", ""},
		{"abc", ""},
		{"a*b", "a*"},
	}
	for _, s := range seeds {
		f.Add(s[0], s[1])
	}

	f.Fuzz(func(t *testing.T, value, pattern string) {
		got := wildcardMatch(value, pattern)

		// "*" matches unconditionally.
		if !wildcardMatch(value, "*") {
			t.Fatalf("wildcardMatch(%q, \"*\") = false, want true", value)
		}

		var want bool
		switch {
		case pattern == "*":
			want = true
		case strings.HasSuffix(pattern, "*"):
			want = strings.HasPrefix(value, strings.TrimSuffix(pattern, "*"))
		default:
			want = value == pattern
		}
		if got != want {
			t.Fatalf("wildcardMatch(%q, %q) = %v, want %v", value, pattern, got, want)
		}
	})
}
