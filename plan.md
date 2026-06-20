# Issue #184 — Rich Markdown rendering in the transcript

## Goal
Render the assistant's final answers with Markdown formatting (bold, italic,
inline code, headings, blockquotes, lists) and syntax-highlighted fenced code
blocks, while keeping copy / export / yank returning the **raw** Markdown text
unchanged. Falls back to the existing flat rendering in plain mode / no-colour.

## Dependencies (approved for this issue only)
- `github.com/hobbestherat/turbotui@main` — new per-span API: `TextView.AddStyled`,
  `tv.StyledSpan`, `tui.Cell.Italic`.
- `github.com/yuin/goldmark@v1.8.2` — CommonMark + GFM parser (used directly).
- `github.com/alecthomas/chroma/v2@v2.27.0` — syntax highlighter (used directly:
  `lexers.Get`/`Analyse`/`Fallback`, `lexer.Tokenise`, `chroma.Token`).
- `github.com/dlclark/regexp2/v2` — chroma's single indirect dependency.

## Design

### New file `ui/tui/markdown.go`
Pure, self-contained renderer. Public entry point:

```go
// renderMarkdown converts assistant Markdown into per-line styled spans.
// Each [][]tv.StyledSpan element is ONE logical display line; an empty element
// is a blank spacer line. The concatenated span text of a line is NOT
// guaranteed to equal the source line (markers are stripped, code is
// re-tokenised), so callers must keep the raw text as the copy/export source.
func renderMarkdown(src string) [][]tv.StyledSpan
```

- Parse with `goldmark.New(goldmark.WithExtensions(extension.GFM))`, walk the AST
  manually (block → inline) accumulating lines.
- Block mapping: Heading → bold + heading colour; Paragraph/TextBlock → inline;
  Blockquote → `"> "` prefix + quote colour; List → bullet `•` / `N.` glyph +
  indent (nested lists indent further); FencedCodeBlock/CodeBlock → chroma; rule
  → a dim line of `─`.
- Inline mapping: Strong (`ast.Emphasis` level 2) → Bold; Emph (level 1) → Italic
  (`tui.Cell.Italic`); CodeSpan → code colour; Link/AutoLink → underline; soft/
  hard line breaks split lines.
- Fenced code: gather the block's raw lines, `lexers.Get(lang)` →
  `Analyse(code)` → `Fallback`; `Tokenise`; map `Token.Type` → (colour, bold/
  italic) via a small category table (keyword/comment/string/number/function/
  type/builtin); split token values on `\n` into lines; apply the code background.
  Chroma never errors on unknown languages (falls back to plaintext).

### Theme integration (`ui/tui/theme.go`)
- `markdownPalette` struct + package var `mdPalette` (initialised to the default
  16-colour palette) recomputed in `ApplyTheme` from the active `Theme`, so it
  tracks NO_COLOR / high-contrast.
- `richMarkdownColorOK` package bool — false under `ColorNone` (NO_COLOR /
  `TERM=dumb`), set in `ApplyTheme`.

### Plain-mode toggle (`ui/tui/markdown.go`)
- `richMarkdown` package bool, default `true` (user intent).
- `richMarkdownEnabled() = richMarkdown && richMarkdownColorOK` — the effective
  gate. Auto-disabled when colour is off.
- `/markdown [on|off]` slash command toggles it and re-renders the transcript.

### Wiring (`ui/tui/transcript_model.go`, `ui/tui/session_window.go`)
- `transcriptRecord` gains `rich bool`. `addAssistant` sets `rich: true` and keeps
  `lines: styledChildLines(text, colorAgent)` (the RAW logical model).
- `renderOne`: when `r.rich && richMarkdownEnabled()`, render the header as a
  plain entry and append the Markdown lines as top-level `view.AddStyled` entries
  (computed from `r.body()` — the raw text). Otherwise the existing flat
  children path. (Styled lines cannot be foldable children — turbotui only styles
  top-level entries — so rich assistant bodies are not collapsible; an acceptable
  trade-off, plain mode keeps folding.)
- `body()` / `lines` stay raw, so `copyLastAnswer` / `copyLastCode` /
  `transcript_model.body()` / `export.go` are unchanged and verbatim.

## Tests (written by GLM partner — NOT here)
Target `renderMarkdown` span attributes/colours, the `go` code-block keyword
colour, raw-text round-trip for copy/export, and plain-mode flat fallback.
