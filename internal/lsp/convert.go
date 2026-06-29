package lsp

import (
	"strings"
	"unicode/utf16"

	"go.lsp.dev/protocol"
	"go.lsp.dev/uri"
)

// Positions live in two coordinate systems (the LSP support design §11.3):
//
//   - the tool boundary speaks 1-based line and 1-based character, where a
//     character is a rune offset (what the model naturally counts), and
//   - the wire speaks 0-based line and 0-based character, where a character is a
//     UTF-16 code-unit offset (what LSP mandates; the library does not expose
//     positionEncodings, so utf-16 is the only wire encoding, §6).
//
// The edge layer owns the rune↔UTF-16 conversion. A column on a line is the
// number of UTF-16 code units before it, so a non-ASCII or astral character
// counts as 1 or 2 units, not as one rune or some number of bytes. These helpers
// take the relevant line's text so the conversion is exact.

// lineText returns the 0-based line'th line of text, without its terminator, or
// "" when the index is out of range. It is the input both conversions need.
func lineOf(text string, line int) string {
	if line < 0 {
		return ""
	}
	// Splitting on "\n" and trimming a trailing "\r" handles both LF and CRLF; LSP
	// positions are line-end agnostic, so the terminator never counts as a column.
	lines := strings.Split(text, "\n")
	if line >= len(lines) {
		return ""
	}
	return strings.TrimSuffix(lines[line], "\r")
}

// runeColToUTF16 converts a 1-based rune column on line to the 0-based UTF-16
// code-unit offset the wire expects. A column past the line's end clamps to the
// line's UTF-16 length, which is the spec's guidance for an out-of-range column.
func runeColToUTF16(line string, col1 int) uint32 {
	if col1 <= 1 {
		return 0
	}
	target := col1 - 1 // runes before the column
	units := 0
	seen := 0
	for _, r := range line {
		if seen >= target {
			break
		}
		units += utf16.RuneLen(r)
		seen++
	}
	return uint32(units) //nolint:gosec // column counts are far within uint32
}

// utf16ColToRune converts a 0-based UTF-16 code-unit offset on line to the
// 1-based rune column used at the tool edge. An offset past the line's end clamps
// to one past the last rune.
func utf16ColToRune(line string, units uint32) int {
	if units == 0 {
		return 1
	}
	consumed := 0
	runes := 0
	for _, r := range line {
		if consumed >= int(units) {
			break
		}
		consumed += utf16.RuneLen(r)
		runes++
	}
	return runes + 1
}

// lineProvider yields the text of a 0-based line for a path, so incoming wire
// positions can be converted to rune columns even for files the Client has not
// opened. The Client backs it with open-document content (preferred) and a
// disk-read fallback.
type lineProvider func(path string, line int) string

// toWirePosition converts a tool-edge Position for path into a wire Position,
// using getLine to fetch the line's text for the column conversion.
func toWirePosition(getLine lineProvider, path string, p Position) protocol.Position {
	line := p.Line - 1
	if line < 0 {
		line = 0
	}
	return protocol.Position{
		Line:      uint32(line), //nolint:gosec // line numbers are within uint32
		Character: runeColToUTF16(getLine(path, line), p.Character),
	}
}

// toWireRange converts a tool-edge Range for path into a wire Range.
func toWireRange(getLine lineProvider, path string, r Range) protocol.Range {
	return protocol.Range{
		Start: toWirePosition(getLine, path, r.Start),
		End:   toWirePosition(getLine, path, r.End),
	}
}

// fromWirePosition converts a wire Position in path into a tool-edge Position.
func fromWirePosition(getLine lineProvider, path string, p protocol.Position) Position {
	line := int(p.Line)
	return Position{
		Line:      line + 1,
		Character: utf16ColToRune(getLine(path, line), p.Character),
	}
}

// fromWireRange converts a wire Range in path into a tool-edge Range.
func fromWireRange(getLine lineProvider, path string, r protocol.Range) Range {
	return Range{
		Start: fromWirePosition(getLine, path, r.Start),
		End:   fromWirePosition(getLine, path, r.End),
	}
}

// fromWireLocation converts a wire Location into a tool-edge Location.
func fromWireLocation(getLine lineProvider, l protocol.Location) Location {
	path := uriToPath(l.URI)
	return Location{Path: path, Range: fromWireRange(getLine, path, l.Range)}
}

// uriToPath converts a file URI to a filesystem path, returning the raw string
// for a non-file URI so nothing is silently dropped.
func uriToPath(u uri.URI) string {
	if u == "" {
		return ""
	}
	if u.IsFile() {
		return u.Path()
	}
	return string(u)
}

// pathToURI converts a filesystem path to a file URI.
func pathToURI(path string) uri.URI { return uri.File(path) }
