package ui

import (
	"strings"
	"testing"

	tui "github.com/hobbestherat/turbotui"
	tv "github.com/hobbestherat/turbotui/turbotv"

	"gogent/internal/config"
)

// These tests cover issue #184's rich-Markdown renderer (renderMarkdown) and its
// wiring into the transcript (renderOne, the plain-mode gate, and the raw-text
// round-trip that copy/export depend on).
//
// The renderer reads the package-level mdPalette and is gated by the package
// globals richMarkdown / richMarkdownColorOK, which other tests (notably
// TestApplyTheme) mutate. Every test below therefore snapshots and restores
// those globals so it is hermetic regardless of test order.

// testPalette returns a rich-Markdown palette whose colours are all distinct,
// so a span carrying the wrong colour is caught unambiguously (e.g. a keyword
// painted with the code colour instead of the keyword colour).
func testPalette() markdownPalette {
	return markdownPalette{
		text:       tui.ANSIColor(10),
		heading:    tui.ANSIColor(9),
		code:       tui.ANSIColor(13),
		quote:      tui.ANSIColor(8),
		listMarker: tui.ANSIColor(11),
		rule:       tui.ANSIColor(7),
		codeBG:     tui.ANSIColor(0),
		hasCodeBG:  true,
		keyword:    tui.ANSIColor(12),
		str:        tui.ANSIColor(3),
		number:     tui.ANSIColor(5),
		comment:    tui.ANSIColor(6),
		function:   tui.ANSIColor(14),
		typ:        tui.ANSIColor(4),
		builtin:    tui.ANSIColor(2),
	}
}

// withTestPalette installs testPalette() for the duration of the test and
// restores the previous mdPalette afterwards.
func withTestPalette(t *testing.T) {
	t.Helper()
	saved := mdPalette
	mdPalette = testPalette()
	t.Cleanup(func() { mdPalette = saved })
}

// withRichState forces the effective rich-Markdown switch on or off for the
// duration of the test (both the user toggle and the colour-capability flag) and
// restores the previous values afterwards.
func withRichState(t *testing.T, on bool) {
	t.Helper()
	savedR, savedOK := richMarkdown, richMarkdownColorOK
	richMarkdown, richMarkdownColorOK = on, on
	t.Cleanup(func() { richMarkdown, richMarkdownColorOK = savedR, savedOK })
}

// mdLineText concatenates the text of every span on one display line.
func mdLineText(spans []tv.StyledSpan) string {
	var b strings.Builder
	for _, s := range spans {
		b.WriteString(s.Text)
	}
	return b.String()
}

// mdAllText joins every display line (blank spacer lines included) with "\n".
func mdAllText(lines [][]tv.StyledSpan) string {
	parts := make([]string, len(lines))
	for i, ln := range lines {
		parts[i] = mdLineText(ln)
	}
	return strings.Join(parts, "\n")
}

// mdFindSpan returns the first span whose Text contains substr, anywhere in the
// rendered lines. Returns ok=false when no such span exists.
func mdFindSpan(lines [][]tv.StyledSpan, substr string) (tv.StyledSpan, bool) {
	for _, ln := range lines {
		for _, s := range ln {
			if strings.Contains(s.Text, substr) {
				return s, true
			}
		}
	}
	return tv.StyledSpan{}, false
}

// mdFindLine returns the first display line whose concatenated text contains
// substr.
func mdFindLine(lines [][]tv.StyledSpan, substr string) ([]tv.StyledSpan, bool) {
	for _, ln := range lines {
		if strings.Contains(mdLineText(ln), substr) {
			return ln, true
		}
	}
	return nil, false
}

// spanIs returns true when got matches the foreground colour c (and has a
// foreground set at all).
func spanIs(got tv.StyledSpan, c tui.Color) bool {
	return got.HasFG && got.FG == c
}

// ----------------------------------------------------------------------------
// Required feature coverage: spans for heading, bold, inline code, a fenced go
// code block (tokenised), and a list. Asserts both the attribute and the colour.
// ----------------------------------------------------------------------------

func TestRenderMarkdownRequiredSample(t *testing.T) {
	withTestPalette(t)
	pal := mdPalette
	goBlock := "package main\n\n// a comment\nfunc addNum() int {\n\treturn 42\n}\n"
	src := "# Title\n\nSome **bold** and `inline` text.\n\n```go\n" + goBlock + "```\n\n- one\n- two\n"
	lines := renderMarkdown(src)

	// Heading: bold + heading colour.
	if h, ok := mdFindSpan(lines, "Title"); !ok {
		t.Errorf("heading span not found")
	} else if !h.Bold || !spanIs(h, pal.heading) {
		t.Errorf("heading = {Bold:%v FG:%v hasFG:%v}, want bold + heading colour", h.Bold, h.FG, h.HasFG)
	}

	// Bold run: bold + body colour (asterisks stripped).
	if b, ok := mdFindSpan(lines, "bold"); !ok {
		t.Errorf("bold span not found")
	} else if !b.Bold || !spanIs(b, pal.text) {
		t.Errorf("bold = {Bold:%v FG:%v}, want bold + text colour", b.Bold, b.FG)
	}
	if all := mdAllText(lines); strings.Contains(all, "**") {
		t.Errorf("bold markers should be stripped, still present: %q", all)
	}

	// Inline code: code colour, and NO background (only fenced code gets a bg).
	if c, ok := mdFindSpan(lines, "inline"); !ok {
		t.Errorf("inline code span not found")
	} else if !spanIs(c, pal.code) || c.HasBG {
		t.Errorf("inline code = {FG:%v hasBG:%v}, want code colour and no background", c.FG, c.HasBG)
	}

	// Fenced go code is tokenised: keywords are bold with the keyword colour and
	// carry the code background.
	for _, kw := range []string{"package", "func", "return"} {
		if k, ok := mdFindSpan(lines, kw); !ok {
			t.Errorf("keyword span %q not found", kw)
		} else if !k.Bold || !spanIs(k, pal.keyword) || !k.HasBG {
			t.Errorf("keyword %q = {Bold:%v FG:%v hasBG:%v}, want bold+keyword colour+bg", kw, k.Bold, k.FG, k.HasBG)
		}
	}

	// Comment is italic with the comment colour.
	if c, ok := mdFindSpan(lines, "comment"); !ok {
		t.Errorf("comment span not found")
	} else if !c.Italic || !spanIs(c, pal.comment) {
		t.Errorf("comment = {Italic:%v FG:%v}, want italic + comment colour", c.Italic, c.FG)
	}

	// Function name is the function colour (not bold).
	if f, ok := mdFindSpan(lines, "addNum"); !ok {
		t.Errorf("function-name span not found")
	} else if f.Bold || !spanIs(f, pal.function) {
		t.Errorf("function name = {Bold:%v FG:%v}, want function colour, not bold", f.Bold, f.FG)
	}

	// Numeric literal is the number colour (not bold).
	if n, ok := mdFindSpan(lines, "42"); !ok {
		t.Errorf("number span not found")
	} else if n.Bold || !spanIs(n, pal.number) {
		t.Errorf("number = {Bold:%v FG:%v}, want number colour, not bold", n.Bold, n.FG)
	}

	// List items render with a bullet marker span in the list colour followed by
	// the item text in the body colour.
	if m, ok := mdFindSpan(lines, "•"); !ok {
		t.Errorf("bullet marker span not found")
	} else if !spanIs(m, pal.listMarker) {
		t.Errorf("bullet marker FG=%v, want list colour", m.FG)
	}
	if one, ok := mdFindSpan(lines, "one"); !ok {
		t.Errorf("list item 'one' span not found")
	} else if !spanIs(one, pal.text) {
		t.Errorf("list item 'one' FG=%v, want text colour", one.FG)
	}
}

func TestRenderMarkdownHeading(t *testing.T) {
	withTestPalette(t)
	for _, lvl := range []int{1, 2, 3, 4, 5, 6} {
		t.Run("level", func(t *testing.T) {
			src := strings.Repeat("#", lvl) + " Heading" + strings.Repeat("a", lvl)
			lines := renderMarkdown(src)
			h, ok := mdFindSpan(lines, "Heading")
			if !ok {
				t.Fatalf("heading span not found for level %d", lvl)
			}
			if !h.Bold {
				t.Errorf("level %d heading not bold", lvl)
			}
		})
	}
}

func TestRenderMarkdownBoldAndItalic(t *testing.T) {
	withTestPalette(t)
	lines := renderMarkdown("**bold** and *italic*")
	b, ok := mdFindSpan(lines, "bold")
	if !ok || !b.Bold {
		t.Errorf("bold not rendered bold: %+v", b)
	}
	i, ok := mdFindSpan(lines, "italic")
	if !ok || !i.Italic {
		t.Errorf("italic not rendered italic: %+v", i)
	}
	if i.Bold {
		t.Errorf("italic should not also be bold")
	}
}

func TestRenderMarkdownInlineCode(t *testing.T) {
	withTestPalette(t)
	pal := mdPalette
	lines := renderMarkdown("see `code here` now")
	c, ok := mdFindSpan(lines, "code here")
	if !ok {
		t.Fatalf("inline code span not found")
	}
	if !spanIs(c, pal.code) {
		t.Errorf("inline code FG=%v, want code colour %v", c.FG, pal.code)
	}
	if c.HasBG {
		t.Errorf("inline code should have no background, got BG=%v", c.BG)
	}
	// Backticks must be stripped from the rendered output.
	if all := mdAllText(lines); strings.Contains(all, "`") {
		t.Errorf("inline code backticks not stripped: %q", all)
	}
}

func TestRenderMarkdownFencedGoKeywords(t *testing.T) {
	withTestPalette(t)
	pal := mdPalette
	lines := renderMarkdown("```go\npackage main\nfunc f() {}\nif for return\n```\n")
	for _, kw := range []string{"package", "func", "if", "for", "return"} {
		k, ok := mdFindSpan(lines, kw)
		if !ok {
			t.Errorf("keyword %q not tokenised", kw)
			continue
		}
		if !k.Bold {
			t.Errorf("keyword %q not bold", kw)
		}
		if !spanIs(k, pal.keyword) {
			t.Errorf("keyword %q FG=%v, want keyword colour", kw, k.FG)
		}
		if !k.HasBG {
			t.Errorf("fenced keyword %q should carry the code background", kw)
		}
	}
}

func TestRenderMarkdownFencedCodeBackground(t *testing.T) {
	withTestPalette(t)
	pal := mdPalette
	lines := renderMarkdown("```go\nx := 1\n```\n")
	x, ok := mdFindSpan(lines, "x")
	if !ok {
		t.Fatal("code span not found")
	}
	// Every token in a fenced block carries the code background.
	if !x.HasBG {
		t.Errorf("fenced code span should have background, hasBG=%v", x.HasBG)
	}
	if pal.hasCodeBG && x.BG != pal.codeBG {
		t.Errorf("fenced code BG=%v, want %v", x.BG, pal.codeBG)
	}
}

func TestRenderMarkdownBulletList(t *testing.T) {
	withTestPalette(t)
	lines := renderMarkdown("- alpha\n- beta\n- gamma")
	for _, want := range []string{"alpha", "beta", "gamma"} {
		ln, ok := mdFindLine(lines, want)
		if !ok {
			t.Errorf("list line %q missing", want)
			continue
		}
		if txt := mdLineText(ln); !strings.HasPrefix(txt, "• ") {
			t.Errorf("list line %q should start with bullet, got %q", want, txt)
		}
	}
}

func TestRenderMarkdownOrderedList(t *testing.T) {
	withTestPalette(t)
	lines := renderMarkdown("1. first\n2. second\n3. third")
	for i, want := range []string{"first", "second", "third"} {
		ln, ok := mdFindLine(lines, want)
		if !ok {
			t.Errorf("ordered list line %q missing", want)
			continue
		}
		prefix := string(rune('1'+i)) + ". "
		if txt := mdLineText(ln); !strings.HasPrefix(txt, prefix) {
			t.Errorf("ordered list line %q should start with %q, got %q", want, prefix, txt)
		}
	}
}

func TestRenderMarkdownNestedListIndent(t *testing.T) {
	withTestPalette(t)
	lines := renderMarkdown("- top\n  - nested\n- bottom")
	// The nested item must be indented relative to the top item so the nesting
	// is visible.
	top, ok := mdFindLine(lines, "top")
	if !ok {
		t.Fatal("top list line missing")
	}
	nested, ok := mdFindLine(lines, "nested")
	if !ok {
		t.Fatal("nested list line missing")
	}
	topText, nestedText := mdLineText(top), mdLineText(nested)
	if !strings.HasPrefix(nestedText, "  ") {
		t.Errorf("nested list item should be indented, got %q", nestedText)
	}
	if indentLen(nestedText) <= indentLen(topText) {
		t.Errorf("nested item (%q) should be more indented than top (%q)", nestedText, topText)
	}
}

// indentLen returns the count of leading spaces.
func indentLen(s string) int {
	n := 0
	for _, r := range s {
		if r == ' ' {
			n++
			continue
		}
		break
	}
	return n
}

func TestRenderMarkdownBlockquote(t *testing.T) {
	withTestPalette(t)
	pal := mdPalette
	lines := renderMarkdown("> quoted text")
	ln, ok := mdFindLine(lines, "quoted")
	if !ok {
		t.Fatal("blockquote line missing")
	}
	if txt := mdLineText(ln); !strings.HasPrefix(txt, "> ") {
		t.Errorf("blockquote should be prefixed with \"> \", got %q", txt)
	}
	// The marker is drawn in the quote colour.
	if m, ok := mdFindSpan(lines, "> "); !ok || !spanIs(m, pal.quote) {
		t.Errorf("blockquote marker should use quote colour, got %+v", m)
	}
	// The quoted prose is recoloured to the quote colour.
	if q, ok := mdFindSpan(lines, "quoted"); !ok || !spanIs(q, pal.quote) {
		t.Errorf("blockquote prose should use quote colour, got %+v", q)
	}
}

func TestRenderMarkdownThematicBreak(t *testing.T) {
	withTestPalette(t)
	pal := mdPalette
	lines := renderMarkdown("a\n\n---\n\nb")
	var found bool
	for _, ln := range lines {
		txt := mdLineText(ln)
		if strings.Trim(txt, "─") == "" && txt != "" {
			found = true
			if len(ln) != 1 || !spanIs(ln[0], pal.rule) {
				t.Errorf("thematic break should be a single rule-coloured span, got %+v", ln)
			}
		}
	}
	if !found {
		t.Errorf("thematic break (rule line) not rendered; lines=%v", lines)
	}
}

func TestRenderMarkdownBlankSpacersBetweenBlocks(t *testing.T) {
	withTestPalette(t)
	// Two paragraphs separated by a blank line produce a blank spacer line
	// (an empty span slice) between them.
	lines := renderMarkdown("first paragraph\n\nsecond paragraph")
	var hasBlank bool
	for _, ln := range lines {
		if len(ln) == 0 {
			hasBlank = true
		}
	}
	if !hasBlank {
		t.Errorf("expected a blank spacer line between blocks; got %d lines", len(lines))
	}
}

func TestRenderMarkdownSoftBreakSplitsLines(t *testing.T) {
	withTestPalette(t)
	// A soft line break (newline within a paragraph) splits the paragraph into
	// two display lines.
	lines := renderMarkdown("line alpha\nline beta")
	if len(lines) < 2 {
		t.Fatalf("soft break should split into >=2 lines, got %d", len(lines))
	}
	all := mdAllText(lines)
	if !strings.Contains(all, "line alpha") || !strings.Contains(all, "line beta") {
		t.Errorf("both halves of the paragraph should survive: %q", all)
	}
}

func TestRenderMarkdownHardBreak(t *testing.T) {
	withTestPalette(t)
	// A hard line break (two trailing spaces) also splits the line.
	lines := renderMarkdown("one  \ntwo")
	if len(lines) < 2 {
		t.Fatalf("hard break should split into >=2 lines, got %d", len(lines))
	}
	if !strings.Contains(mdLineText(lines[0]), "one") {
		t.Errorf("first line should hold 'one': %q", mdLineText(lines[0]))
	}
}

func TestRenderMarkdownLinkUnderline(t *testing.T) {
	withTestPalette(t)
	lines := renderMarkdown("see [the docs](https://example.com) now")
	l, ok := mdFindSpan(lines, "the docs")
	if !ok {
		t.Fatal("link text span not found")
	}
	if !l.Underline {
		t.Errorf("link text should be underlined")
	}
}

func TestRenderMarkdownAutoLinkUnderline(t *testing.T) {
	withTestPalette(t)
	lines := renderMarkdown("visit <https://example.com> now")
	l, ok := mdFindSpan(lines, "https://example.com")
	if !ok {
		t.Fatal("autolink span not found")
	}
	if !l.Underline {
		t.Errorf("autolink URL should be underlined")
	}
}

// ----------------------------------------------------------------------------
// Edge cases and error handling: the renderer must never panic and must degrade
// gracefully for empty / malformed / unsupported input.
// ----------------------------------------------------------------------------

func TestRenderMarkdownEmpty(t *testing.T) {
	withTestPalette(t)
	lines := renderMarkdown("")
	if len(lines) != 0 {
		t.Errorf("empty input should render no lines, got %d: %v", len(lines), lines)
	}
}

func TestRenderMarkdownWhitespaceOnly(t *testing.T) {
	withTestPalette(t)
	for _, src := range []string{"\n", "\n\n\n", "   ", "\t\t"} {
		lines := renderMarkdown(src)
		// Whitespace-only input must not panic and must produce no visible
		// (non-blank) lines.
		for _, ln := range lines {
			if txt := strings.TrimSpace(mdLineText(ln)); txt != "" {
				t.Errorf("whitespace-only input %q produced visible line %q", src, txt)
			}
		}
	}
}

func TestRenderMarkdownUnclosedFence(t *testing.T) {
	withTestPalette(t)
	// An unclosed fenced block must not panic; its content is still rendered as
	// code.
	lines := renderMarkdown("```go\npackage main")
	all := mdAllText(lines)
	if !strings.Contains(all, "package") {
		t.Errorf("unclosed fence content lost: %q", all)
	}
}

func TestRenderMarkdownEmptyFenceNoLanguage(t *testing.T) {
	withTestPalette(t)
	pal := mdPalette
	lines := renderMarkdown("```\nhello\nworld\n```\n")
	all := mdAllText(lines)
	if !strings.Contains(all, "hello") || !strings.Contains(all, "world") {
		t.Errorf("plain code block content lost: %q", all)
	}
	// With no language the Analyse/fallback path still paints the code colour.
	if h, ok := mdFindSpan(lines, "hello"); !ok || !spanIs(h, pal.code) {
		t.Errorf("plain code should use code colour, got %+v", h)
	}
}

func TestRenderMarkdownUnknownLanguageFallback(t *testing.T) {
	withTestPalette(t)
	// A nonsense language name must fall back without error and render the full
	// code text (chroma never errors on unknown languages).
	src := "```totally-not-a-language-xyz\nalpha + beta = 42\n```\n"
	lines := renderMarkdown(src)
	all := mdAllText(lines)
	for _, want := range []string{"alpha", "beta", "42"} {
		if !strings.Contains(all, want) {
			t.Errorf("unknown-language code lost %q: %q", want, all)
		}
	}
}

func TestRenderMarkdownCodeNoCRLeak(t *testing.T) {
	withTestPalette(t)
	// CRLF line endings inside a fenced block must not leak a bare carriage
	// return into any span's text (it would corrupt the rendered line).
	lines := renderMarkdown("```go\nfoo()\r\nbar()\r\n```\n")
	for i, ln := range lines {
		for _, s := range ln {
			if strings.ContainsRune(s.Text, '\r') {
				t.Errorf("line %d span %q contains a carriage return", i, s.Text)
			}
		}
	}
}

// TestRenderMarkdownNeverPanics sweeps a table of pathological inputs to ensure
// the renderer is robust (no nil-derefs, no index-out-of-range, no chroma
// explosion). It only asserts "did not panic and returned something".
func TestRenderMarkdownNeverPanics(t *testing.T) {
	withTestPalette(t)
	cases := map[string]string{
		"empty":            "",
		"only fence":       "```",
		"lang only fence":  "```go",
		"unclosed":         "```\ncode",
		"deeply nested bq": strings.Repeat("> ", 20) + "deep",
		"deeply nested list": func() string {
			var b strings.Builder
			for i := 0; i < 20; i++ {
				b.WriteString(strings.Repeat("  ", i))
				b.WriteString("- item\n")
			}
			return b.String()
		}(),
		"empty code block":  "```\n```",
		"many breaks":       "a\na\na\na\na",
		"html block":        "<div>\n  <p>x</p>\n</div>\n",
		"raw entities":      "&amp;&lt;&gt;&#39;&quot;",
		"emoji":             "hello 👍 world 🚀",
		"long line":         strings.Repeat("word ", 500),
		"tabs in text":      "\t\ttabbed",
		"table":             "| a | b |\n|---|---|\n| 1 | 2 |\n",
		"setext heading":    "Title\n=====\nbody",
		"nested everything": "# H\n\n> quote\n>\n> - list\n>   - nested\n>\n> ```go\n> code\n> ```\n",
	}
	for name, src := range cases {
		t.Run(name, func(t *testing.T) {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("renderMarkdown panicked on %q: %v", name, r)
				}
			}()
			_ = renderMarkdown(src)
		})
	}
}

// ----------------------------------------------------------------------------
// Raw-text round-trip: copy / export / yank must return the verbatim Markdown,
// not the styled rendering. The styled spans are display-only.
// ----------------------------------------------------------------------------

func TestBodyRoundTripsRawMarkdown(t *testing.T) {
	withRichState(t, true)
	src := "# Title\n\nSome **bold** and `inline`.\n\n```go\npackage main\nfunc f() {}\n```\n\n- a\n- b\n"
	sw := newTestSession()
	sw.addAssistant(src)
	rec := sw.transcript.lastAssistantRecord()
	if rec == nil {
		t.Fatal("no assistant record")
	}
	if got := rec.body(); got != src {
		t.Errorf("body() did not round-trip the raw Markdown:\nwant %q\n got %q", src, got)
	}
}

func TestCopyLastCodeFromRawBody(t *testing.T) {
	withRichState(t, true)
	src := "Here is code:\n\n```go\npackage main\n\nfunc main() {}\n```\n\nAnd prose.\n"
	sw := newTestSession()
	sw.addAssistant(src)
	code := extractFencedCode(sw.transcript.lastAssistantRecord().body())
	want := "package main\n\nfunc main() {}"
	if code != want {
		t.Errorf("extractFencedCode(body()) = %q, want %q (raw code, not styled)", code, want)
	}
}

func TestExportPreservesRawMarkdown(t *testing.T) {
	withRichState(t, true)
	src := "Text with `code` and **bold**.\n\n```go\nfunc f() {}\n```\n"
	out := renderTranscriptMarkdown([]ChatMessage{{Role: "assistant", Content: src}}, "Session", "")
	// The export emits the assistant content verbatim, so the raw markers and
	// fence survive intact (untouched by rich rendering).
	for _, want := range []string{"**bold**", "`code`", "```go", "func f() {}", "```"} {
		if !strings.Contains(out, want) {
			t.Errorf("export lost raw markdown %q:\n%s", want, out)
		}
	}
}

func TestEmptyAssistantBodyIsCopyable(t *testing.T) {
	withRichState(t, true)
	sw := newTestSession()
	sw.addAssistant("")
	if got := sw.transcript.lastAssistantRecord().body(); got != "" {
		t.Errorf("empty assistant body should be empty, got %q", got)
	}
}

// ----------------------------------------------------------------------------
// Plain-mode gate and wiring: richMarkdownEnabled() and renderOne's two paths.
// ----------------------------------------------------------------------------

func TestRichMarkdownGate(t *testing.T) {
	savedR, savedOK := richMarkdown, richMarkdownColorOK
	t.Cleanup(func() { richMarkdown, richMarkdownColorOK = savedR, savedOK })
	for _, tc := range []struct {
		name    string
		rich    bool
		colorOK bool
		enabled bool
	}{
		{"on with colour", true, true, true},
		{"off with colour", false, true, false},
		{"on without colour (NO_COLOR)", true, false, false},
		{"off without colour", false, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			richMarkdown, richMarkdownColorOK = tc.rich, tc.colorOK
			if got := richMarkdownEnabled(); got != tc.enabled {
				t.Errorf("richMarkdownEnabled() = %v, want %v", got, tc.enabled)
			}
		})
	}
}

func TestRenderOneRichPathStripsMarkers(t *testing.T) {
	withRichState(t, true)
	sw := newTestSession()
	sw.addAssistant("Hello **world** with `code`.\n\n```go\nfunc main() {}\n```\n")
	all := sw.transcript.view.AllText()
	// Rich rendering strips inline markers and fence markers from the display.
	for _, gone := range []string{"**world**", "`code`", "```go"} {
		if strings.Contains(all, gone) {
			t.Errorf("rich view should not contain raw marker %q:\n%s", gone, all)
		}
	}
	// But the rendered content is still visible.
	for _, want := range []string{"world", "code", "func main"} {
		if !strings.Contains(all, want) {
			t.Errorf("rich view lost rendered content %q:\n%s", want, all)
		}
	}
}

func TestRenderOnePlainFallbackShowsRaw(t *testing.T) {
	withRichState(t, false) // plain mode: no colour / user-disabled
	sw := newTestSession()
	sw.addAssistant("Hello **world** with `code`.\n\n```go\nfunc main() {}\n```\n")
	all := sw.transcript.view.AllText()
	// Plain mode renders the raw text flat, so the markers survive verbatim.
	for _, want := range []string{"**world**", "`code`", "```go", "func main() {}"} {
		if !strings.Contains(all, want) {
			t.Errorf("plain view should contain raw %q:\n%s", want, all)
		}
	}
}

func TestApplyThemeNoColorDisablesRich(t *testing.T) {
	// ApplyTheme under NO_COLOR must flip richMarkdownColorOK off so the feature
	// auto-disables. Save/restore every global ApplyTheme touches.
	savedColors := snapshotColors()
	savedTV := tv.DefaultTheme
	savedPal := mdPalette
	savedR, savedOK := richMarkdown, richMarkdownColorOK
	savedDH, savedDD := colorDialogHeader, colorDialogDetail
	t.Cleanup(func() {
		restoreColors(savedColors)
		tv.DefaultTheme = savedTV
		mdPalette = savedPal
		richMarkdown, richMarkdownColorOK = savedR, savedOK
		colorDialogHeader, colorDialogDetail = savedDH, savedDD
	})
	richMarkdown = true // user wants it on...
	ApplyTheme(ResolveTheme(config.ThemeConfig{}, envOf(map[string]string{"NO_COLOR": "1"}), false))
	if richMarkdownColorOK {
		t.Errorf("richMarkdownColorOK should be false under NO_COLOR")
	}
	if richMarkdownEnabled() {
		t.Errorf("rich rendering should be disabled under NO_COLOR even when the user toggle is on")
	}
}

func TestHandleMarkdownCommandToggle(t *testing.T) {
	savedR := richMarkdown
	t.Cleanup(func() { richMarkdown = savedR })
	sw := newTestSession()
	richMarkdown = true
	sw.handleMarkdownCommand([]string{"off"})
	if richMarkdown {
		t.Errorf("/markdown off should disable richMarkdown")
	}
	sw.handleMarkdownCommand([]string{"on"})
	if !richMarkdown {
		t.Errorf("/markdown on should enable richMarkdown")
	}
	// A bare /markdown toggles the current state (on -> off here).
	sw.handleMarkdownCommand(nil)
	if richMarkdown {
		t.Errorf("/markdown with no arg should toggle richMarkdown off (was on)")
	}
	sw.handleMarkdownCommand(nil)
	if !richMarkdown {
		t.Errorf("/markdown with no arg should toggle richMarkdown back on")
	}
}

// ----------------------------------------------------------------------------
// DEFECTS FOUND. The two tests below assert CORRECT behaviour that the current
// implementation does NOT yet provide; they are expected to fail and document
// the defects so they can be fixed. See the accompanying report.
// ----------------------------------------------------------------------------

// DEFECT: HTML entities are emitted verbatim instead of being decoded. A user
// sees "Tom &amp; Jerry" rather than "Tom & Jerry". CommonMark requires entity
// references in text to be decoded for display; the raw source is already
// preserved separately for copy/export via body(), so the rendered spans should
// decode them.
func TestRenderMarkdownDecodesHTMLEntities(t *testing.T) {
	withTestPalette(t)
	lines := renderMarkdown("Tom &amp; Jerry uses &lt;tags&gt; and &#39;quotes&#39;")
	all := mdAllText(lines)
	for _, bad := range []string{"&amp;", "&lt;", "&gt;", "&#39;"} {
		if strings.Contains(all, bad) {
			t.Errorf("HTML entity %q should be decoded for display, but appears verbatim in %q", bad, all)
		}
	}
	if !strings.Contains(all, "&") || !strings.Contains(all, "<") || !strings.Contains(all, ">") {
		t.Errorf("decoded entity text missing from %q", all)
	}
}

// DEFECT: a GFM table collapses to a single garbled token. The input
//
//	| a | b |
//	|---|---|
//	| 1 | 2 |
//
// renders as the single line "ab12" — the pipes and row/cell structure are
// discarded, and the cells run together with no separator. Tables are extremely
// common in model output, so this produces unreadable transcripts. (Tables are
// not in the renderer's supported subset; the acceptable fallback would be the
// raw source lines or one cell per line — not "ab12".)
func TestRenderMarkdownTableReadable(t *testing.T) {
	withTestPalette(t)
	lines := renderMarkdown("| a | b |\n|---|---|\n| 1 | 2 |\n")
	joined := mdAllText(lines)
	if joined == "ab12" {
		t.Errorf("GFM table collapsed to a single garbled token %q — cell structure and separators lost", joined)
	}
}
