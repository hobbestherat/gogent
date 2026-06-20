package web

import (
	"html"
	"regexp"
	"strings"
	"unicode"
)

// HTMLToMarkdown reduces an HTML document to readable Markdown. It is a
// best-effort, dependency-free extractor (the standard library ships no HTML
// parser): it drops non-content regions (script/style/head/…), maps common
// structural and inline tags to their Markdown equivalents, strips the rest, and
// unescapes HTML entities. It returns the document <title> (when present) and the
// Markdown body.
func HTMLToMarkdown(raw string) (title, body string) {
	title = extractTitle(raw)
	return title, finalize(render(stripRegions(raw)))
}

var (
	reTitle   = regexp.MustCompile(`(?is)<title[^>]*>(.*?)</title>`)
	reComment = regexp.MustCompile(`(?s)<!--.*?-->`)
	reHref    = regexp.MustCompile(`(?is)\bhref\s*=\s*("([^"]*)"|'([^']*)'|([^\s>]+))`)

	// Non-content regions removed wholesale before rendering.
	regionTags = []string{"script", "style", "head", "title", "noscript", "svg", "iframe", "template", "object", "canvas"}
	regionREs  = compileRegionREs()

	// Tags that introduce a paragraph break (blank line).
	blockTags = map[string]bool{
		"p": true, "div": true, "section": true, "article": true, "header": true,
		"footer": true, "main": true, "aside": true, "figure": true, "figcaption": true,
		"ul": true, "ol": true, "dl": true, "table": true, "blockquote": true,
		"form": true, "fieldset": true, "address": true, "hr": true,
	}
	// Tags that introduce a single line break.
	lineTags = map[string]bool{"tr": true, "dt": true, "dd": true, "caption": true}
)

func compileRegionREs() []*regexp.Regexp {
	res := make([]*regexp.Regexp, len(regionTags))
	for i, t := range regionTags {
		res[i] = regexp.MustCompile(`(?is)<` + t + `\b[^>]*>.*?</` + t + `>`)
	}
	return res
}

func extractTitle(raw string) string {
	m := reTitle.FindStringSubmatch(raw)
	if m == nil {
		return ""
	}
	return strings.TrimSpace(collapseSpaces(html.UnescapeString(m[1])))
}

func stripRegions(s string) string {
	for _, re := range regionREs {
		s = re.ReplaceAllString(s, " ")
	}
	// Comments carry no text and introduce no whitespace, so drop them outright.
	return reComment.ReplaceAllString(s, "")
}

// out is an append-only Markdown buffer that tracks its trailing byte so spacing
// and line-break helpers run in O(1).
type out struct{ buf []byte }

func (o *out) last() byte {
	if len(o.buf) == 0 {
		return 0
	}
	return o.buf[len(o.buf)-1]
}

func (o *out) writeString(s string) { o.buf = append(o.buf, s...) }
func (o *out) writeByte(c byte)     { o.buf = append(o.buf, c) }
func (o *out) writeRune(r rune)     { o.buf = append(o.buf, string(r)...) }

// space writes a single separating space unless we are at the start of a line or
// already on whitespace.
func (o *out) space() {
	switch o.last() {
	case 0, ' ', '\n':
		return
	}
	o.writeByte(' ')
}

// ensureNL guarantees the buffer ends with a newline (no-op when empty).
func (o *out) ensureNL() {
	if len(o.buf) == 0 || o.last() == '\n' {
		return
	}
	o.writeByte('\n')
}

// ensureBlank guarantees the buffer ends with a blank line (paragraph break).
func (o *out) ensureBlank() {
	if len(o.buf) == 0 {
		return
	}
	n := 0
	for i := len(o.buf) - 1; i >= 0 && o.buf[i] == '\n'; i-- {
		n++
	}
	for ; n < 2; n++ {
		o.writeByte('\n')
	}
}

// render walks the (region-stripped) HTML and emits Markdown.
func render(s string) string {
	o := &out{}
	var linkHref string
	inPre := false

	i := 0
	for i < len(s) {
		if s[i] == '<' {
			j := strings.IndexByte(s[i:], '>')
			if j < 0 {
				break // unterminated tag: drop the trailing fragment
			}
			handleTag(o, s[i+1:i+j], &linkHref, &inPre)
			i += j + 1
			continue
		}
		j := strings.IndexByte(s[i:], '<')
		if j < 0 {
			writeText(o, s[i:], inPre)
			break
		}
		writeText(o, s[i:i+j], inPre)
		i += j
	}
	return string(o.buf)
}

func handleTag(o *out, tag string, linkHref *string, inPre *bool) {
	closing := strings.HasPrefix(tag, "/")
	if closing {
		tag = tag[1:]
	}
	name := tagName(tag)
	switch {
	case name == "br":
		o.ensureNL()
	case name == "pre":
		if closing {
			o.ensureNL()
			o.writeString("```")
			o.ensureBlank()
			*inPre = false
		} else {
			o.ensureBlank()
			o.writeString("```\n")
			*inPre = true
		}
	case len(name) == 2 && name[0] == 'h' && name[1] >= '1' && name[1] <= '6':
		if closing {
			o.ensureBlank()
		} else {
			o.ensureBlank()
			o.writeString(strings.Repeat("#", int(name[1]-'0')))
			o.writeByte(' ')
		}
	case name == "li":
		if !closing {
			o.ensureNL()
			o.writeString("- ")
		}
	case name == "strong" || name == "b":
		o.writeString("**")
	case name == "em" || name == "i":
		o.writeString("*")
	case name == "code":
		if !*inPre {
			o.writeString("`")
		}
	case name == "a":
		if closing {
			if *linkHref != "" {
				o.writeString("](")
				o.writeString(*linkHref)
				o.writeString(")")
				*linkHref = ""
			}
		} else if href := hrefOf(tag); href != "" && !strings.HasPrefix(strings.ToLower(href), "javascript:") {
			*linkHref = href
			o.writeString("[")
		}
	case name == "td" || name == "th":
		o.space()
	case blockTags[name]:
		o.ensureBlank()
	case lineTags[name]:
		o.ensureNL()
	}
}

func writeText(o *out, text string, inPre bool) {
	text = html.UnescapeString(text)
	if inPre {
		o.writeString(text)
		return
	}
	pending := false
	for _, r := range text {
		if unicode.IsSpace(r) {
			pending = true
			continue
		}
		if pending {
			o.space()
			pending = false
		}
		o.writeRune(r)
	}
	if pending {
		o.space()
	}
}

func tagName(tag string) string {
	tag = strings.TrimSpace(tag)
	if i := strings.IndexAny(tag, " \t\r\n/"); i >= 0 {
		tag = tag[:i]
	}
	return strings.ToLower(tag)
}

func hrefOf(tag string) string {
	m := reHref.FindStringSubmatch(tag)
	if m == nil {
		return ""
	}
	for _, g := range m[2:] {
		if g != "" {
			return strings.TrimSpace(html.UnescapeString(g))
		}
	}
	return ""
}

// finalize trims trailing whitespace per line and collapses runs of blank lines.
func finalize(s string) string {
	lines := strings.Split(s, "\n")
	for i := range lines {
		lines[i] = strings.TrimRight(lines[i], " \t")
	}
	s = strings.Join(lines, "\n")
	for strings.Contains(s, "\n\n\n") {
		s = strings.ReplaceAll(s, "\n\n\n", "\n\n")
	}
	return strings.TrimSpace(s)
}

// collapseSpaces replaces every run of whitespace with a single space.
func collapseSpaces(s string) string {
	return strings.Join(strings.FieldsFunc(s, unicode.IsSpace), " ")
}
