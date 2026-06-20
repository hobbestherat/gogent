package ui

import (
	"html"
	"regexp"
	"strconv"
	"strings"

	"github.com/alecthomas/chroma/v2"
	"github.com/alecthomas/chroma/v2/lexers"
	tui "github.com/hobbestherat/turbotui"
	tv "github.com/hobbestherat/turbotui/turbotv"
	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	extast "github.com/yuin/goldmark/extension/ast"
	"github.com/yuin/goldmark/text"
)

// richMarkdown is the user's intent to render assistant answers with Markdown
// formatting and syntax-highlighted code (issue #184). It defaults to on and is
// flipped by the /markdown command. The effective switch is richMarkdownEnabled,
// which also honours the terminal's colour capability (richMarkdownColorOK, set
// by ApplyTheme): rich rendering is pointless without colour, so it auto-disables
// under NO_COLOR / TERM=dumb.
var richMarkdown = true

// richMarkdownEnabled reports whether assistant answers should be rendered as
// rich Markdown. It combines the user toggle with the terminal's colour support.
func richMarkdownEnabled() bool { return richMarkdown && richMarkdownColorOK }

// markdownPalette holds the colours rich-Markdown rendering draws with. It is a
// package var (mdPalette) recomputed by ApplyTheme from the active Theme so it
// tracks the NO_COLOR and high-contrast presets like the rest of the UI. The
// initial value mirrors the default 16-colour palette so rendering is coloured
// before any theme is applied (and so tests have stable expectations).
type markdownPalette struct {
	text       tui.Color // body prose (assistant colour)
	heading    tui.Color // ATX headings (rendered bold)
	code       tui.Color // inline code and the default fenced-code foreground
	quote      tui.Color // blockquote text and the "> " marker
	listMarker tui.Color // list bullets / numbers
	rule       tui.Color // thematic breaks

	codeBG    tui.Color // fenced-code background
	hasCodeBG bool      // whether to paint codeBG (off under NO_COLOR)

	// Syntax-highlight colours, mapped from chroma token categories.
	keyword  tui.Color // keywords (rendered bold)
	str      tui.Color // string / char literals
	number   tui.Color // numeric literals
	comment  tui.Color // comments (rendered italic)
	function tui.Color // function names
	typ      tui.Color // type / class / namespace names
	builtin  tui.Color // builtins
}

// mdPalette is the active rich-Markdown palette. ApplyTheme overwrites it; the
// initial literal matches the default 16-colour ANSI palette.
var mdPalette = markdownPalette{
	text:       tui.ANSIColor(10), // agent green
	heading:    tui.ANSIColor(12), // bright blue
	code:       tui.ANSIColor(13), // magenta
	quote:      tui.ANSIColor(8),  // dim grey
	listMarker: tui.ANSIColor(11), // bright yellow
	rule:       tui.ANSIColor(8),  // dim grey
	codeBG:     tui.ANSIColor(0),  // black
	hasCodeBG:  true,
	keyword:    tui.ANSIColor(12), // blue
	str:        tui.ANSIColor(11), // yellow
	number:     tui.ANSIColor(13), // magenta
	comment:    tui.ANSIColor(8),  // grey
	function:   tui.ANSIColor(14), // cyan
	typ:        tui.ANSIColor(12), // blue
	builtin:    tui.ANSIColor(14), // cyan
}

// richMarkdownColorOK reports whether the terminal can show colour at all. It is
// set false for ColorNone (NO_COLOR / TERM=dumb) by ApplyTheme so rich Markdown
// auto-disables there. It defaults true to match the coloured default palette.
var richMarkdownColorOK = true

// mdPaletteGen is bumped whenever the rich-Markdown palette changes (a theme is
// applied). Cached per-record styled spans record the generation they were
// rendered with and are recomputed when it no longer matches, so a theme switch
// recolours existing answers.
var mdPaletteGen uint64

// applyMarkdownPalette derives the rich-Markdown palette from a resolved Theme
// and records whether colour is available. Called from ApplyTheme.
func applyMarkdownPalette(t Theme) {
	mdPaletteGen++
	mdPalette = markdownPalette{
		text:       t.Agent,
		heading:    t.Info,
		code:       t.Result,
		quote:      t.Note,
		listMarker: t.Tool,
		rule:       t.Note,
		keyword:    t.Info,
		str:        t.Tool,
		number:     t.Result,
		comment:    t.Note,
		function:   t.User,
		typ:        t.Info,
		builtin:    t.User,
	}
	if t.Level != ColorNone {
		// A subtle background sets fenced code apart. The high-contrast preset is
		// already pure black, so a black code background would vanish — rely on its
		// bright token foregrounds instead.
		mdPalette.codeBG = tui.ANSIColor(0)
		mdPalette.hasCodeBG = t.Name != themeHighContrast
	}
	richMarkdownColorOK = t.Level != ColorNone
}

// mdStyle is the styling carried while walking the Markdown tree; it is turned
// into a tv.StyledSpan for each run of text.
type mdStyle struct {
	fg        tui.Color
	hasFG     bool
	bg        tui.Color
	hasBG     bool
	bold      bool
	italic    bool
	underline bool
}

// span wraps s in a StyledSpan carrying this style.
func (s mdStyle) span(text string) tv.StyledSpan {
	return tv.StyledSpan{
		Text:      text,
		FG:        s.fg,
		HasFG:     s.hasFG,
		BG:        s.bg,
		HasBG:     s.hasBG,
		Bold:      s.bold,
		Italic:    s.italic,
		Underline: s.underline,
	}
}

// withFG returns a copy of the style using colour c as its foreground.
func (s mdStyle) withFG(c tui.Color) mdStyle {
	s.fg, s.hasFG = c, true
	return s
}

// renderMarkdown converts assistant Markdown text into per-line styled spans for
// rich transcript rendering. Each element of the result is ONE logical display
// line: a slice of styled spans, or an empty slice for a blank spacer line.
//
// The concatenation of a line's span texts is NOT guaranteed to equal the source
// line — inline markers are stripped and fenced code is re-tokenised — so callers
// must keep the original Markdown as the source of truth for copy and export.
//
// The supported subset is headings, emphasis, inline code, blockquotes, lists,
// fenced/indented code (chroma-highlighted), tables and thematic breaks. GFM
// strikethrough and task-list checkboxes are parsed but intentionally flattened
// to plain text — turbotui has no strikethrough attribute and they are outside
// this issue's subset.
func renderMarkdown(src string) [][]tv.StyledSpan {
	md := goldmark.New(goldmark.WithExtensions(extension.GFM))
	source := []byte(src)
	doc := md.Parser().Parse(text.NewReader(source))
	r := &markdownRenderer{src: source, pal: mdPalette, textColor: mdPalette.text}
	return r.containerLines(doc, true)
}

// markdownRenderer walks a parsed Markdown document into styled lines. textColor
// is the prose colour for the current subtree; it is the assistant colour at the
// top level and the quote colour inside a blockquote.
type markdownRenderer struct {
	src       []byte
	pal       markdownPalette
	textColor tui.Color
}

// base is the default body style for the current subtree's prose colour.
func (r *markdownRenderer) base() mdStyle {
	return mdStyle{fg: r.textColor, hasFG: true}
}

// containerLines renders the block children of parent into display lines. When
// spaced is true (the top level), a blank line is inserted between successive
// blocks to mirror Markdown's paragraph spacing; nested containers (list items)
// pass false for a tight layout.
func (r *markdownRenderer) containerLines(parent ast.Node, spaced bool) [][]tv.StyledSpan {
	var out [][]tv.StyledSpan
	for c := parent.FirstChild(); c != nil; c = c.NextSibling() {
		block := r.blockLines(c)
		if len(block) == 0 {
			continue
		}
		if spaced && len(out) > 0 {
			out = append(out, nil) // blank spacer between top-level blocks
		}
		out = append(out, block...)
	}
	return out
}

// blockLines renders a single block node into display lines.
func (r *markdownRenderer) blockLines(n ast.Node) [][]tv.StyledSpan {
	switch node := n.(type) {
	case *ast.Heading:
		st := r.base().withFG(r.pal.heading)
		st.bold = true
		return r.inlineLines(node, st)
	case *ast.Paragraph:
		return r.inlineLines(node, r.base())
	case *ast.TextBlock:
		return r.inlineLines(node, r.base())
	case *ast.FencedCodeBlock:
		return r.codeLines(node, string(node.Language(r.src)))
	case *ast.CodeBlock:
		return r.codeLines(node, "")
	case *ast.Blockquote:
		return r.quoteLines(node)
	case *ast.List:
		return r.listLines(node, 0)
	case *extast.Table:
		return r.tableLines(node)
	case *ast.ThematicBreak:
		return [][]tv.StyledSpan{{r.base().withFG(r.pal.rule).span(strings.Repeat("─", 24))}}
	default:
		// Unknown / HTML block: fall back to its raw source lines (preserving line
		// structure and any literal markup) rather than garbling it.
		return r.rawLines(n)
	}
}

// inlineLines renders the inline children of a block (paragraph, heading, list
// item text) into display lines, splitting on soft and hard line breaks. base is
// the starting style; emphasis, code spans and links layer onto it.
func (r *markdownRenderer) inlineLines(n ast.Node, base mdStyle) [][]tv.StyledSpan {
	var lines [][]tv.StyledSpan
	var cur []tv.StyledSpan
	flush := func() {
		lines = append(lines, cur)
		cur = nil
	}
	add := func(st mdStyle, s string) {
		if s != "" {
			cur = append(cur, st.span(s))
		}
	}

	var walk func(node ast.Node, st mdStyle)
	walk = func(node ast.Node, st mdStyle) {
		for c := node.FirstChild(); c != nil; c = c.NextSibling() {
			switch child := c.(type) {
			case *ast.Text:
				// Decode HTML entities (&amp; → &) for display only; the raw Markdown
				// is preserved separately for copy/export.
				add(st, decodeEntities(string(child.Segment.Value(r.src))))
				if child.HardLineBreak() || child.SoftLineBreak() {
					flush()
				}
			case *ast.String:
				add(st, decodeEntities(string(child.Value)))
			case *ast.CodeSpan:
				// Inline code keeps the code colour but inherits the surrounding
				// emphasis (bold/italic) so `code` inside **bold** stays bold.
				cs := st
				cs.fg, cs.hasFG = r.pal.code, true
				add(cs, r.collectText(child))
			case *ast.Emphasis:
				es := st
				if child.Level >= 2 {
					es.bold = true
				} else {
					es.italic = true
				}
				walk(child, es)
			case *ast.Link:
				ls := st
				ls.underline = true
				walk(child, ls)
			case *ast.AutoLink:
				ls := st
				ls.underline = true
				add(ls, string(child.URL(r.src)))
			default:
				// RawHTML and any other inline container: render its text plainly.
				if txt := r.collectText(c); txt != "" {
					add(st, txt)
				} else {
					walk(c, st)
				}
			}
		}
	}
	walk(n, base)
	if len(cur) > 0 || len(lines) == 0 {
		flush()
	}
	return lines
}

// quoteLines renders a blockquote: its inner blocks rendered with the quote prose
// colour, each line prefixed with a dim "> " marker. Inline code keeps its own
// colour because it sets its foreground explicitly while walking.
func (r *markdownRenderer) quoteLines(n ast.Node) [][]tv.StyledSpan {
	inner := r.withTextColor(r.pal.quote).containerLines(n, true)
	marker := r.base().withFG(r.pal.quote).span("> ")
	out := make([][]tv.StyledSpan, 0, len(inner))
	for _, line := range inner {
		out = append(out, append([]tv.StyledSpan{marker}, line...))
	}
	return out
}

// withTextColor returns a shallow copy of the renderer whose prose colour is c,
// used to recolour a blockquote's inner content.
func (r *markdownRenderer) withTextColor(c tui.Color) *markdownRenderer {
	clone := *r
	clone.textColor = c
	return &clone
}

// listLines renders a list. Each item is prefixed with a bullet ("•") or its
// ordinal ("1.") in the list-marker colour; continuation and nested lines are
// indented to align under the item text. depth drives the indent for nested
// lists.
func (r *markdownRenderer) listLines(list *ast.List, depth int) [][]tv.StyledSpan {
	marker := r.base().withFG(r.pal.listMarker)
	indent := strings.Repeat("  ", depth)
	number := list.Start
	var out [][]tv.StyledSpan
	for item := list.FirstChild(); item != nil; item = item.NextSibling() {
		var glyph string
		if list.IsOrdered() {
			glyph = strconv.Itoa(number) + ". "
			number++
		} else {
			glyph = "• "
		}
		lead := indent + glyph
		// Size the continuation indent by the marker's terminal width, not its rune
		// count, so a wide bullet glyph still aligns wrapped/continued lines.
		cont := indent + strings.Repeat(" ", tui.StringWidth(glyph))

		lines := r.itemLines(item, depth)
		for i, line := range lines {
			prefix := cont
			if i == 0 {
				prefix = lead
			}
			out = append(out, append([]tv.StyledSpan{marker.span(prefix)}, line...))
		}
	}
	return out
}

// itemLines renders one list item's blocks. A nested list is recursed with an
// increased depth; other blocks render normally (tight, no blank spacers).
func (r *markdownRenderer) itemLines(item ast.Node, depth int) [][]tv.StyledSpan {
	var out [][]tv.StyledSpan
	for c := item.FirstChild(); c != nil; c = c.NextSibling() {
		if sub, ok := c.(*ast.List); ok {
			out = append(out, r.listLines(sub, depth+1)...)
			continue
		}
		out = append(out, r.blockLines(c)...)
	}
	return out
}

// tableLines renders a GFM table: the header row (bold), a dashed rule, then the
// data rows. Cells are separated by a dim " │ " so the column structure stays
// legible; cell inline content (emphasis, code) is rendered normally. Cells are
// not column-aligned — the transcript soft-wraps — but the pipes and per-row
// layout are preserved so the table reads as a table rather than a run-on.
func (r *markdownRenderer) tableLines(tbl ast.Node) [][]tv.StyledSpan {
	sep := r.base().withFG(r.pal.rule)
	var out [][]tv.StyledSpan
	row := func(cells ast.Node, header bool) {
		var line []tv.StyledSpan
		i := 0
		for cell := cells.FirstChild(); cell != nil; cell = cell.NextSibling() {
			if i > 0 {
				line = append(line, sep.span(" │ "))
			}
			st := r.base()
			st.bold = header
			line = append(line, r.inlineSpans(cell, st)...)
			i++
		}
		out = append(out, line)
	}
	for child := tbl.FirstChild(); child != nil; child = child.NextSibling() {
		switch node := child.(type) {
		case *extast.TableHeader:
			row(node, true)
			width := 0
			for _, span := range out[len(out)-1] {
				width += tui.StringWidth(span.Text)
			}
			if width < 3 {
				width = 3
			}
			out = append(out, []tv.StyledSpan{sep.span(strings.Repeat("─", width))})
		case *extast.TableRow:
			row(node, false)
		}
	}
	return out
}

// inlineSpans renders a node's inline content into a single styled line, joining
// any soft-wrapped sub-lines with a space. Used for table cells, which must stay
// on one logical line.
func (r *markdownRenderer) inlineSpans(n ast.Node, base mdStyle) []tv.StyledSpan {
	var out []tv.StyledSpan
	for i, line := range r.inlineLines(n, base) {
		if i > 0 {
			out = append(out, base.span(" "))
		}
		out = append(out, line...)
	}
	return out
}

// codeLines renders a fenced or indented code block: its raw text is syntax
// highlighted with chroma (lang selects the lexer; unknown languages fall back to
// plaintext) and split into one styled line per source line, painted with the
// code background.
func (r *markdownRenderer) codeLines(n ast.Node, lang string) [][]tv.StyledSpan {
	var b strings.Builder
	lines := n.Lines()
	for i := 0; i < lines.Len(); i++ {
		seg := lines.At(i)
		b.Write(seg.Value(r.src))
	}
	return r.highlight(b.String(), strings.TrimSpace(lang))
}

// highlight tokenises code with chroma and maps each token to a styled span,
// splitting on newlines into display lines. The lexer is chosen by language name,
// then by content analysis, then plaintext — chroma never errors on unknown
// input.
func (r *markdownRenderer) highlight(code, lang string) [][]tv.StyledSpan {
	lexer := lexers.Get(lang)
	if lexer == nil {
		lexer = lexers.Analyse(code)
	}
	if lexer == nil {
		lexer = lexers.Fallback
	}
	iterator, err := lexer.Tokenise(nil, code)
	if err != nil {
		return r.plainCode(code)
	}

	var lines [][]tv.StyledSpan
	var cur []tv.StyledSpan
	for _, tok := range iterator.Tokens() {
		st := r.tokenStyle(tok.Type)
		segments := strings.Split(tok.Value, "\n")
		for i, seg := range segments {
			if i > 0 {
				lines = append(lines, cur)
				cur = nil
			}
			if seg != "" {
				cur = append(cur, st.span(seg))
			}
		}
	}
	lines = append(lines, cur)
	// Code text usually ends with a trailing newline, leaving an empty final line.
	if n := len(lines); n > 0 && len(lines[n-1]) == 0 {
		lines = lines[:n-1]
	}
	return r.fillBlankCodeLines(lines)
}

// fillBlankCodeLines gives interior blank lines of a code block a single
// background-painted space so the block reads as one continuous panel rather than
// being split by background-less gaps. Only applied when a code background is in
// effect.
func (r *markdownRenderer) fillBlankCodeLines(lines [][]tv.StyledSpan) [][]tv.StyledSpan {
	if !r.pal.hasCodeBG {
		return lines
	}
	blank := r.codeStyle().span(" ")
	for i := range lines {
		if len(lines[i]) == 0 {
			lines[i] = []tv.StyledSpan{blank}
		}
	}
	return lines
}

// plainCode renders code without highlighting, one line per source line, in the
// code colour and background. Used if a lexer unexpectedly errors.
func (r *markdownRenderer) plainCode(code string) [][]tv.StyledSpan {
	st := r.codeStyle().withFG(r.pal.code)
	var out [][]tv.StyledSpan
	for _, line := range strings.Split(strings.TrimRight(code, "\n"), "\n") {
		if line == "" {
			out = append(out, nil)
			continue
		}
		out = append(out, []tv.StyledSpan{st.span(line)})
	}
	return r.fillBlankCodeLines(out)
}

// codeStyle is the base style for code: code foreground on the code background.
func (r *markdownRenderer) codeStyle() mdStyle {
	st := mdStyle{fg: r.pal.code, hasFG: true}
	if r.pal.hasCodeBG {
		st.bg, st.hasBG = r.pal.codeBG, true
	}
	return st
}

// tokenStyle maps a chroma token type to a code style: a token colour (from the
// palette) on the code background, bold for keywords and italic for comments.
func (r *markdownRenderer) tokenStyle(tt chroma.TokenType) mdStyle {
	st := r.codeStyle()
	switch tt.Category() {
	case chroma.Keyword:
		st.fg, st.bold = r.pal.keyword, true
	case chroma.Comment:
		st.fg, st.italic = r.pal.comment, true
	case chroma.Literal:
		switch tt.SubCategory() {
		case chroma.LiteralNumber:
			st.fg = r.pal.number
		default:
			st.fg = r.pal.str
		}
	case chroma.Name:
		switch {
		case tt.InSubCategory(chroma.NameFunction):
			st.fg = r.pal.function
		case tt == chroma.NameBuiltin || tt == chroma.NameBuiltinPseudo:
			st.fg = r.pal.builtin
		case tt == chroma.NameClass || tt == chroma.NameNamespace:
			st.fg = r.pal.typ
		}
	}
	return st
}

// rawLines renders an unrecognised block as plain text in the body colour. It
// prefers the node's raw source lines (preserving line structure and any literal
// markup, e.g. an HTML block) and falls back to concatenated descendant text when
// the node exposes no source lines. Used for HTML blocks and any node type not
// explicitly handled.
func (r *markdownRenderer) rawLines(n ast.Node) [][]tv.StyledSpan {
	st := r.base()
	if src := n.Lines(); src != nil && src.Len() > 0 {
		var b strings.Builder
		for i := 0; i < src.Len(); i++ {
			seg := src.At(i)
			b.Write(seg.Value(r.src))
		}
		var out [][]tv.StyledSpan
		for _, line := range strings.Split(strings.TrimRight(b.String(), "\n"), "\n") {
			out = append(out, []tv.StyledSpan{st.span(line)})
		}
		return out
	}
	txt := r.collectText(n)
	if txt == "" {
		return nil
	}
	var out [][]tv.StyledSpan
	for _, line := range strings.Split(txt, "\n") {
		out = append(out, []tv.StyledSpan{st.span(line)})
	}
	return out
}

// collectText concatenates the literal text of a node and its descendants. It is
// used for inline code spans and raw blocks where styling is uniform.
func (r *markdownRenderer) collectText(n ast.Node) string {
	var b strings.Builder
	var walk func(node ast.Node)
	walk = func(node ast.Node) {
		switch t := node.(type) {
		case *ast.Text:
			b.Write(t.Segment.Value(r.src))
			if t.SoftLineBreak() || t.HardLineBreak() {
				b.WriteByte('\n')
			}
		case *ast.String:
			b.Write(t.Value)
		}
		for c := node.FirstChild(); c != nil; c = c.NextSibling() {
			walk(c)
		}
	}
	walk(n)
	return b.String()
}

// entityRef matches a well-formed HTML entity reference: a named (&amp;), decimal
// (&#39;) or hex (&#x2F;) reference that ends with a semicolon. CommonMark only
// recognises entities in this terminated form.
var entityRef = regexp.MustCompile(`&(?:#[0-9]+|#[xX][0-9a-fA-F]+|[a-zA-Z][a-zA-Z0-9]*);`)

// decodeEntities decodes HTML entity references in prose text for display, while
// matching CommonMark's rule that a reference must end with a semicolon. It only
// hands the matched "&…;" runs to html.UnescapeString, so a bare legacy entity
// such as "&copyX" or "&ampb" stays literal (Go's html.UnescapeString would
// otherwise decode those, deviating from CommonMark). Unknown references like
// "&notreal;" are left unchanged by html.UnescapeString.
func decodeEntities(s string) string {
	if !strings.Contains(s, "&") {
		return s
	}
	return entityRef.ReplaceAllStringFunc(s, html.UnescapeString)
}
