package ui

import (
	"regexp"
	"sort"
	"strings"
	"unicode"
)

// @-file mentions (issue #46) let the user attach a specific workspace file to a
// message by typing "@<path>". The completer in mention_completer.go offers the
// matching files as you type; on send, expandMentions inlines the mentioned
// files' contents so the model receives them as attached context instead of
// having to discover and read each file itself. This file holds the pure,
// desktop-free helpers behind both halves so they can be unit-tested directly.

// mentionRe matches an @-file mention: an '@' at the start of the text or after
// whitespace, followed by one or more non-space characters (the path). The
// leading boundary keeps an email address's '@' (preceded by a word character)
// from being treated as a mention, and is captured separately so it stays out of
// the path.
var mentionRe = regexp.MustCompile(`(^|\s)@(\S+)`)

// mentionTrailingPunct is the set of trailing punctuation trimmed from a captured
// mention path, so "see @main.go." or "@main.go," resolves to the bare path. A
// trailing dot is included because no real filename ends in one.
const mentionTrailingPunct = ".,;:!?)]}'\""

// parseMentions returns the unique file paths @-mentioned in text, in first-seen
// order. Trailing sentence punctuation is trimmed from each path so a mention at
// the end of a sentence still resolves.
func parseMentions(text string) []string {
	var out []string
	seen := make(map[string]bool)
	for _, m := range mentionRe.FindAllStringSubmatch(text, -1) {
		path := strings.TrimRight(m[2], mentionTrailingPunct)
		if path == "" || seen[path] {
			continue
		}
		seen[path] = true
		out = append(out, path)
	}
	return out
}

// mentionToken locates the @-file mention the cursor is currently inside, given
// the current input line (as runes) and the cursor's column (a rune index). It
// returns the rune index of the '@', the partial path typed between it and the
// cursor (empty right after '@'), and ok=true when the cursor sits in an active
// mention. A mention is active only when an '@' precedes the cursor with no
// intervening whitespace and the '@' is at line start or follows whitespace — so
// an email address's '@' never opens the completer.
func mentionToken(line []rune, cursorX int) (start int, query string, ok bool) {
	if cursorX < 0 {
		return 0, "", false
	}
	if cursorX > len(line) {
		cursorX = len(line)
	}
	for i := cursorX - 1; i >= 0; i-- {
		switch {
		case line[i] == '@':
			if i > 0 && !unicode.IsSpace(line[i-1]) {
				return 0, "", false // '@' not at a word boundary (e.g. an email)
			}
			return i, string(line[i+1 : cursorX]), true
		case unicode.IsSpace(line[i]):
			return 0, "", false // whitespace before any '@': not in a mention
		}
	}
	return 0, "", false
}

// filterPaths returns the workspace paths matching query as a fuzzy subsequence,
// best match first, capped at limit (a non-positive limit is unbounded). An empty
// query lists paths in their natural (lexical) order. Matching is over the whole
// relative path, so "uiutil" finds "ui/util.go" and a bare base name still
// matches. It reuses fuzzyScore, the command palette's matcher.
func filterPaths(paths []string, query string, limit int) []string {
	type scored struct {
		path  string
		score int
	}
	matches := make([]scored, 0, len(paths))
	for _, p := range paths {
		if s, ok := fuzzyScore(query, p); ok {
			matches = append(matches, scored{p, s})
		}
	}
	sort.SliceStable(matches, func(i, j int) bool {
		return matches[i].score < matches[j].score
	})
	if limit > 0 && len(matches) > limit {
		matches = matches[:limit]
	}
	out := make([]string, len(matches))
	for i, m := range matches {
		out[i] = m.path
	}
	return out
}

// maxMentionFileBytes caps the content attached for a single @-mention so a huge
// file cannot blow up the message; the excess is dropped with a truncation note.
const maxMentionFileBytes = 64 * 1024

// expandMentions augments a user message with the contents of the files it
// @-mentions, so the model receives them as attached context without having to
// read each file itself (issue #46). read resolves a mention path to its content
// (ok=false when it cannot be read, e.g. a typo or a directory); unresolved
// mentions are left untouched in the prose. It returns the augmented message and
// the list of paths actually attached (for a transcript note); when nothing
// resolves — including a nil reader — the message is returned unchanged with a
// nil list.
func expandMentions(text string, read func(path string) (string, bool)) (string, []string) {
	if read == nil {
		return text, nil
	}
	var attached []string
	var b strings.Builder
	for _, path := range parseMentions(text) {
		content, ok := read(path)
		if !ok {
			continue
		}
		if len(content) > maxMentionFileBytes {
			content = content[:maxMentionFileBytes] + "\n… [truncated]"
		}
		b.WriteString("\n\n===== ")
		b.WriteString(path)
		b.WriteString(" =====\n")
		b.WriteString(content)
		attached = append(attached, path)
	}
	if len(attached) == 0 {
		return text, nil
	}
	return text + "\n\nAttached files (referenced with @):" + b.String(), attached
}
