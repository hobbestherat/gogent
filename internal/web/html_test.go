package web

import (
	"strings"
	"testing"
)

func TestExtractTitle(t *testing.T) {
	tests := []struct {
		name string
		html string
		want string
	}{
		{"simple", "<html><head><title>Hello</title></head></html>", "Hello"},
		{"entities", "<title>A &amp; B</title>", "A & B"},
		{"whitespace", "<title>\n  spaced   out\n</title>", "spaced out"},
		{"attrs", `<title lang="en">Doc</title>`, "Doc"},
		{"none", "<html><body>no title</body></html>", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := extractTitle(tt.html); got != tt.want {
				t.Errorf("extractTitle() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestHTMLToMarkdown(t *testing.T) {
	tests := []struct {
		name      string
		html      string
		wantTitle string
		want      string
	}{
		{
			name:      "headings and paragraphs",
			html:      "<title>T</title><body><h1>Title</h1><p>First para.</p><p>Second para.</p></body>",
			wantTitle: "T",
			want:      "# Title\n\nFirst para.\n\nSecond para.",
		},
		{
			name: "drops script and style",
			html: "<body><style>.x{color:red}</style><p>Keep</p><script>alert('x')</script></body>",
			want: "Keep",
		},
		{
			name: "link extraction",
			html: `<p>See <a href="https://go.dev">the docs</a> now.</p>`,
			want: "See [the docs](https://go.dev) now.",
		},
		{
			name: "emphasis and code",
			html: "<p>Use <strong>bold</strong> and <em>italic</em> and <code>x()</code>.</p>",
			want: "Use **bold** and *italic* and `x()`.",
		},
		{
			name: "unordered list",
			html: "<ul><li>one</li><li>two</li><li>three</li></ul>",
			want: "- one\n- two\n- three",
		},
		{
			name: "entities unescaped",
			html: "<p>a &lt; b &amp;&amp; c &gt; d</p>",
			want: "a < b && c > d",
		},
		{
			name: "collapses whitespace",
			html: "<p>lots   of\n\n   space</p>",
			want: "lots of space",
		},
		{
			name: "preformatted preserved",
			html: "<pre>line1\n  indented</pre>",
			want: "```\nline1\n  indented\n```",
		},
		{
			name: "javascript href dropped",
			html: `<p>click <a href="javascript:evil()">here</a></p>`,
			want: "click here",
		},
		{
			name: "comments removed",
			html: "<p>before<!-- secret -->after</p>",
			want: "beforeafter",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			title, md := HTMLToMarkdown(tt.html)
			if tt.wantTitle != "" && title != tt.wantTitle {
				t.Errorf("title = %q, want %q", title, tt.wantTitle)
			}
			if md != tt.want {
				t.Errorf("markdown =\n%q\nwant\n%q", md, tt.want)
			}
		})
	}
}

func TestHTMLToMarkdownNoTrailingNoise(t *testing.T) {
	// A messy document should still collapse to clean, trimmed Markdown.
	html := "<html><head><title>Doc</title><style>x</style></head>" +
		"<body>\n\n  <h2>Section</h2>\n\n  <p>Para one.</p>\n\n\n  <p>Para two.</p>  \n</body></html>"
	_, md := HTMLToMarkdown(html)
	if strings.Contains(md, "\n\n\n") {
		t.Errorf("expected no triple newlines, got %q", md)
	}
	if strings.HasPrefix(md, "\n") || strings.HasSuffix(md, "\n") || strings.HasSuffix(md, " ") {
		t.Errorf("expected trimmed output, got %q", md)
	}
	if !strings.HasPrefix(md, "## Section") {
		t.Errorf("expected heading first, got %q", md)
	}
}
