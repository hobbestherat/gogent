package shell

import (
	"path/filepath"
	"sort"
	"strings"
)

// ExternalRoots best-effort scans a shell command for filesystem paths that
// escape the workspace and returns the distinct external root directories they
// touch (cleaned absolute paths). It is a guardrail, not a sandbox: shell is
// Turing-complete and a determined command can still reach outside the
// workspace (variables, eval, subshells). It exists to catch the common,
// accidental cases such as `rm /etc/...` or `cat ../../secrets`.
//
// workspaceRoot must be a cleaned absolute path.
func ExternalRoots(command, workspaceRoot string) []string {
	seen := make(map[string]struct{})
	for _, tok := range pathLikeTokens(command) {
		abs := resolveToken(tok, workspaceRoot)
		if abs == "" {
			continue
		}
		if pathUnder(abs, workspaceRoot) {
			continue
		}
		root := externalRoot(abs)
		seen[root] = struct{}{}
	}

	roots := make([]string, 0, len(seen))
	for r := range seen {
		roots = append(roots, r)
	}
	sort.Strings(roots)
	return roots
}

// pathLikeTokens splits a command into whitespace-separated tokens and keeps
// only those that look like filesystem paths.
func pathLikeTokens(command string) []string {
	fields := strings.FieldsFunc(command, func(r rune) bool {
		switch r {
		case ' ', '\t', '\n', '\r', '=', ';', '|', '&', '(', ')', '<', '>', '"', '\'', '`', ',':
			return true
		}
		return false
	})

	var out []string
	for _, f := range fields {
		f = strings.TrimSpace(f)
		if looksLikePath(f) {
			out = append(out, f)
		}
	}
	return out
}

func looksLikePath(tok string) bool {
	if tok == "" {
		return false
	}
	// Skip option flags like -rf, --force.
	if strings.HasPrefix(tok, "-") {
		return false
	}
	if strings.HasPrefix(tok, "/") || strings.HasPrefix(tok, "~") {
		return true
	}
	if strings.HasPrefix(tok, "./") || strings.HasPrefix(tok, "../") {
		return true
	}
	// A bare token containing a separator (e.g. sub/dir) but not a URL/flag.
	if strings.Contains(tok, "/") && !strings.Contains(tok, "://") {
		return true
	}
	return false
}

// resolveToken turns a path-like token into a cleaned absolute path relative to
// the workspace. Tokens with shell metacharacters we cannot resolve return "".
func resolveToken(tok, workspaceRoot string) string {
	if strings.ContainsAny(tok, "*?$") {
		// Globs and variable expansions are out of scope for the scanner.
		return ""
	}
	if strings.HasPrefix(tok, "~") {
		// Home-relative; treat as external by anchoring at root so it is flagged.
		tok = strings.TrimPrefix(tok, "~")
		tok = strings.TrimPrefix(tok, "/")
		return filepath.Clean("/" + tok)
	}
	if filepath.IsAbs(tok) {
		return filepath.Clean(tok)
	}
	return filepath.Clean(filepath.Join(workspaceRoot, tok))
}

// externalRoot reduces an absolute path to the folder the user is asked about:
// the directory containing the touched path (e.g. /etc/passwd -> /etc). One
// grant then covers the whole folder.
func externalRoot(abs string) string {
	abs = filepath.Clean(abs)
	dir := filepath.Dir(abs)
	if dir == "." {
		return string(filepath.Separator)
	}
	return dir
}

func pathUnder(child, parent string) bool {
	if parent == "" || child == "" {
		return false
	}
	if child == parent {
		return true
	}
	rel, err := filepath.Rel(parent, child)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
