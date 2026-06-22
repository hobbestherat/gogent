package ui

import (
	"strings"
	"testing"

	tui "github.com/hobbestherat/turbotui"
	tv "github.com/hobbestherat/turbotui/turbotv"
)

// These tests cover issue #313: rich-Markdown tables must column-align. tableLines
// runs a measure pass (max display width per column) then a render pass that pads
// each cell to its column width, honouring GFM alignment (:--- / ---: / :---:),
// keeping padding/separator spans neutral and preserving per-cell inline styling.
//
// The invariants they assert against the rendered styled spans:
//   - every row and the rule have identical display width;
//   - the │ separators (and the ┼ on the rule) land at the SAME screen column on
//     every row regardless of cell content length;
//   - GFM Left/None pad on the right, Right pads on the left, Center splits;
//   - padding is emitted as neutral (unstyled) spans; separators carry only the
//     rule colour; cell-text spans keep their inline styling intact.

// tableRows returns the rendered lines of the first (only) table in src, with the
// renderer's test palette installed for the duration of the test.
func tableRows(t *testing.T, src string) [][]tv.StyledSpan {
	t.Helper()
	withTestPalette(t)
	return renderMarkdown(src)
}

// sepColumns returns the display column (cumulative StringWidth from the start of
// the line) of every │ or ┼ glyph in s. Used to prove separators line up across
// rows even when cell content has different widths.
func sepColumns(s string) []int {
	var cols []int
	w := 0
	for _, r := range s {
		if r == '│' || r == '┼' {
			cols = append(cols, w)
		}
		w += tui.StringWidth(string(r))
	}
	return cols
}

// isNeutralSpace reports whether sp is a padding span: blank text with no styling
// at all (no foreground, not bold/italic/underline). Padding must be neutral so it
// neither tints the grid nor disturbs the cell-text span.
func isNeutralSpace(sp tv.StyledSpan) bool {
	return sp.Text != "" && strings.Trim(sp.Text, " ") == "" &&
		!sp.HasFG && !sp.HasBG && !sp.Bold && !sp.Italic && !sp.Underline
}

// splitCells breaks one rendered table row into its cells, dropping the " │ "
// separator spans. The padding and content spans of each cell are preserved.
func splitCells(row []tv.StyledSpan) [][]tv.StyledSpan {
	var cells [][]tv.StyledSpan
	var cur []tv.StyledSpan
	for _, sp := range row {
		if sp.Text == " │ " {
			cells = append(cells, cur)
			cur = nil
			continue
		}
		cur = append(cur, sp)
	}
	return append(cells, cur)
}

// cellPadding inspects one cell's spans and returns the leading neutral pad width,
// the trailing neutral pad width, and the concatenated content text in between.
func cellPadding(cell []tv.StyledSpan) (left, right int, content string) {
	i := 0
	for i < len(cell) && isNeutralSpace(cell[i]) {
		left += tui.StringWidth(cell[i].Text)
		i++
	}
	j := len(cell)
	for j > i && isNeutralSpace(cell[j-1]) {
		right += tui.StringWidth(cell[j-1].Text)
		j--
	}
	var b strings.Builder
	for _, sp := range cell[i:j] {
		b.WriteString(sp.Text)
	}
	return left, right, b.String()
}

// ruleLine returns the table's rule line (the one made only of ─/┼) and its index.
func ruleLine(t *testing.T, lines [][]tv.StyledSpan) ([]tv.StyledSpan, int) {
	t.Helper()
	for i, ln := range lines {
		txt := mdLineText(ln)
		if txt != "" && strings.Contains(txt, "─") && strings.Trim(txt, "─┼") == "" {
			return ln, i
		}
	}
	t.Fatalf("no rule line (only ─/┼) found in %q", mdAllText(lines))
	return nil, -1
}

// The core issue-#313 guarantee: a short-cell row and a long-cell row place their
// │ separators at the SAME screen columns, the rule's ┼ sit under those │, and
// every row (rule included) has the same total display width.
func TestRenderMarkdownTableColumnsAlign(t *testing.T) {
	lines := tableRows(t, "| Name | Count | Description |\n"+
		"|------|-------|-------------|\n"+
		"| alpha | 1 | short |\n"+
		"| betagammalong | 99999 | a longer description here |\n")

	// Collect every rendered line that belongs to the table (header, rule, rows).
	var texts []string
	for _, ln := range lines {
		if txt := mdLineText(ln); strings.ContainsAny(txt, "│┼") {
			texts = append(texts, txt)
		}
	}
	if len(texts) < 4 {
		t.Fatalf("expected 4 table lines (header+rule+2 rows), got %d: %v", len(texts), texts)
	}

	// All separator/boundary columns identical across rows.
	want := sepColumns(texts[0])
	if len(want) != 2 {
		t.Fatalf("header should have 2 separators, got %d in %q", len(want), texts[0])
	}
	for _, txt := range texts {
		if got := sepColumns(txt); !eqInts(got, want) {
			t.Errorf("separator columns drift:\n want %v (from %q)\n  got %v (from %q)", want, texts[0], got, txt)
		}
	}

	// All rows share one display width.
	wantW := tui.StringWidth(texts[0])
	for _, txt := range texts {
		if got := tui.StringWidth(txt); got != wantW {
			t.Errorf("row width %d != header width %d for %q", got, wantW, txt)
		}
	}
}

// Header rule spans the full table width (matches every row) and is built only
// from ─ fill with one ┼ per interior column boundary, each ┼ under a header │.
func TestRenderMarkdownTableRuleSpansFullWidth(t *testing.T) {
	lines := tableRows(t, "| Name | Count | Description |\n"+
		"|------|-------|-------------|\n"+
		"| betagammalong | 99999 | a longer description here |\n")

	header := lines[0]
	rule, _ := ruleLine(t, lines)
	headerTxt, ruleTxt := mdLineText(header), mdLineText(rule)

	if tui.StringWidth(ruleTxt) != tui.StringWidth(headerTxt) {
		t.Errorf("rule width %d != header width %d\n header %q\n rule   %q",
			tui.StringWidth(ruleTxt), tui.StringWidth(headerTxt), headerTxt, ruleTxt)
	}
	// One ┼ per interior boundary (3 columns -> 2 boundaries).
	if n := strings.Count(ruleTxt, "┼"); n != 2 {
		t.Errorf("rule should have 2 ┼ boundaries, got %d in %q", n, ruleTxt)
	}
	// ┼ sit under the header's │.
	if got, want := sepColumns(ruleTxt), sepColumns(headerTxt); !eqInts(got, want) {
		t.Errorf("┼ columns %v do not match │ columns %v", got, want)
	}
	// The rule is a single span in the rule colour (neutral content, just coloured).
	if len(rule) != 1 {
		t.Errorf("rule should be one span, got %d: %v", len(rule), rule)
	} else if !spanIs(rule[0], mdPalette.rule) {
		t.Errorf("rule span should use rule colour, got %+v", rule[0])
	}
}

// GFM right-alignment (---:) pads on the LEFT: a shorter cell gets leading neutral
// pad and no trailing pad, so its content is flush against the right separator.
func TestRenderMarkdownTableRightAlign(t *testing.T) {
	lines := tableRows(t, "| L | R |\n|:---|---:|\n| a | 1 |\n| bb | 9999 |\n")
	// Data row "a | 1": column R (width 4) holds "1" right-aligned.
	row, ok := mdFindLine(lines, "1")
	if !ok {
		t.Fatal("data row with '1' not found")
	}
	cells := splitCells(row)
	if len(cells) != 2 {
		t.Fatalf("expected 2 cells, got %d", len(cells))
	}
	left, right, content := cellPadding(cells[1])
	if content != "1" {
		t.Fatalf("right cell content = %q, want %q", content, "1")
	}
	if left <= 0 {
		t.Errorf("right-aligned cell should have leading pad, got left=%d", left)
	}
	if right != 0 {
		t.Errorf("right-aligned cell should have NO trailing pad, got right=%d", right)
	}
	// The left column ":---" is left-aligned: "a" pads on the right instead.
	lLeft, lRight, lContent := cellPadding(cells[0])
	if lContent != "a" || lLeft != 0 || lRight <= 0 {
		t.Errorf("left-aligned cell = (left=%d right=%d content=%q), want left=0 right>0 content=\"a\"", lLeft, lRight, lContent)
	}
}

// GFM center-alignment (:---:) splits the pad: equal on even gaps, with the extra
// space on the right for odd gaps (left = gap/2, right = gap - left).
func TestRenderMarkdownTableCenterAlign(t *testing.T) {
	// Column width 5 ("wxyzv"); "ab" -> gap 3 -> left 1, right 2 (odd gap).
	lines := tableRows(t, "| C |\n|:---:|\n| ab |\n| wxyzv |\n")
	row, ok := mdFindLine(lines, "ab")
	if !ok {
		t.Fatal("row 'ab' not found")
	}
	cells := splitCells(row)
	left, right, content := cellPadding(cells[0])
	if content != "ab" {
		t.Fatalf("centre cell content = %q, want %q", content, "ab")
	}
	if left != 1 || right != 2 {
		t.Errorf("centre pad for odd gap = (left=%d right=%d), want (1,2)", left, right)
	}
}

// Left and None (no colon markers) both pad on the right.
func TestRenderMarkdownTableLeftNonePadRight(t *testing.T) {
	lines := tableRows(t, "| N |\n|---|\n| x |\n| yyyy |\n")
	row, ok := mdFindLine(lines, "x")
	if !ok {
		t.Fatal("row 'x' not found")
	}
	left, right, content := cellPadding(splitCells(row)[0])
	if content != "x" {
		t.Fatalf("cell content = %q, want %q", content, "x")
	}
	if left != 0 || right != 3 {
		t.Errorf("None-aligned pad = (left=%d right=%d), want (0,3)", left, right)
	}
}

// Padding spans must be neutral: every span that is whitespace-only and is not the
// " │ " separator must carry no colour and no attributes, so the grid background
// stays untinted and the cell-text span is never merged with its pad.
func TestRenderMarkdownTablePaddingNeutral(t *testing.T) {
	lines := tableRows(t, "| H | Wide |\n|---|---|\n| a | b |\n| longcell | c |\n")
	for _, ln := range lines {
		for _, sp := range ln {
			if sp.Text == " │ " {
				if !spanIs(sp, mdPalette.rule) {
					t.Errorf("separator span should use rule colour, got %+v", sp)
				}
				continue
			}
			if strings.Trim(sp.Text, " ") == "" && sp.Text != "" {
				if !isNeutralSpace(sp) {
					t.Errorf("padding span %q should be neutral (no colour/attrs), got %+v", sp.Text, sp)
				}
			}
		}
	}
}

// Wide (double-width) glyphs are measured by display width, so a CJK cell aligns
// the grid by columns, not rune count.
func TestRenderMarkdownTableWideChars(t *testing.T) {
	lines := tableRows(t, "| 中文 | b |\n|---|---|\n| z | 长长长 |\n")
	var texts []string
	for _, ln := range lines {
		if txt := mdLineText(ln); strings.ContainsAny(txt, "│┼") {
			texts = append(texts, txt)
		}
	}
	if len(texts) < 3 {
		t.Fatalf("expected header+rule+row, got %d: %v", len(texts), texts)
	}
	want := sepColumns(texts[0])
	for _, txt := range texts {
		if got := sepColumns(txt); !eqInts(got, want) {
			t.Errorf("wide-char separators drift: want %v got %v (%q)", want, got, txt)
		}
		if w := tui.StringWidth(txt); w != tui.StringWidth(texts[0]) {
			t.Errorf("wide-char row width %d != %d (%q)", w, tui.StringWidth(texts[0]), txt)
		}
	}
	// "z" sits in a column whose width is set by "中文" (display width 4): it gets 3
	// trailing pad columns (left-aligned default), not 2 (which a rune-count
	// measure would wrongly produce).
	row, _ := mdFindLine(lines, "z")
	left, right, content := cellPadding(splitCells(row)[0])
	if content != "z" || left != 0 || right != 3 {
		t.Errorf("wide-column pad for \"z\" = (left=%d right=%d content=%q), want (0,3,\"z\")", left, right, content)
	}
}

// A single-column table has no │ separators and a rule of bare ─ with no ┼.
func TestRenderMarkdownTableSingleColumn(t *testing.T) {
	lines := tableRows(t, "| only |\n|---|\n| a |\n| longer |\n")
	if _, ok := mdFindSpan(lines, "│"); ok {
		t.Errorf("single-column table should have no │ separator")
	}
	rule, _ := ruleLine(t, lines)
	ruleTxt := mdLineText(rule)
	if strings.Contains(ruleTxt, "┼") {
		t.Errorf("single-column rule should have no ┼, got %q", ruleTxt)
	}
	// Rule width equals the (padded) header width.
	if tui.StringWidth(ruleTxt) != tui.StringWidth(mdLineText(lines[0])) {
		t.Errorf("single-column rule width %d != header width %d", tui.StringWidth(ruleTxt), tui.StringWidth(mdLineText(lines[0])))
	}
}

// A column whose widest cell is a DATA row (not the header) sizes to that data
// width; the header pads to match and the grid still aligns.
func TestRenderMarkdownTableDataWiderThanHeader(t *testing.T) {
	lines := tableRows(t, "| h | x |\n|---|---|\n| superlongvalue | y |\n")
	header, rule := lines[0], func() string { r, _ := ruleLine(t, lines); return mdLineText(r) }()
	row, _ := mdFindLine(lines, "superlongvalue")
	if w := tui.StringWidth(mdLineText(header)); w != tui.StringWidth(mdLineText(row)) {
		t.Errorf("header width %d != data row width %d (header must pad to data)", w, tui.StringWidth(mdLineText(row)))
	}
	if tui.StringWidth(rule) != tui.StringWidth(mdLineText(row)) {
		t.Errorf("rule width %d != data row width %d", tui.StringWidth(rule), tui.StringWidth(mdLineText(row)))
	}
	// The header "h" cell is padded out to the data width (14).
	left, right, content := cellPadding(splitCells(header)[0])
	if content != "h" || left != 0 || right != len("superlongvalue")-1 {
		t.Errorf("header cell pad = (left=%d right=%d content=%q), want right=%d", left, right, content, len("superlongvalue")-1)
	}
}

// Empty cells must not break alignment or panic: a blank cell is pure padding and
// the separators stay put.
func TestRenderMarkdownTableEmptyCells(t *testing.T) {
	lines := tableRows(t, "| a | b | c |\n|---|---|---|\n|  | y |  |\n| xxxx |  | z |\n")
	var texts []string
	for _, ln := range lines {
		if txt := mdLineText(ln); strings.ContainsAny(txt, "│┼") {
			texts = append(texts, txt)
		}
	}
	want := sepColumns(texts[0])
	for _, txt := range texts {
		if got := sepColumns(txt); !eqInts(got, want) {
			t.Errorf("empty-cell row separators drift: want %v got %v (%q)", want, got, txt)
		}
	}
}

// Cell inline styling survives padding: with separate neutral pad spans, the
// styled content span stays EXACT (Text == the cell content) so code/emphasis
// colour and attributes are preserved even when the cell is padded.
func TestRenderMarkdownTableCellStylingPreservedWithPadding(t *testing.T) {
	lines := tableRows(t, "| **bold** | `code` |\n|---|---|\n| plaincellwide | *it* |\n")
	pal := mdPalette // tableRows installs the test palette before rendering

	// Header "bold" is a wide column (padded), yet the styled span is exact + bold.
	header := lines[0]
	if b, ok := spanExact(header, "bold"); !ok || !b.Bold || !spanIs(b, pal.text) {
		t.Errorf("padded header bold span should stay exact, bold, text-coloured: %+v ok=%v", b, ok)
	}
	// Header "code" stays bold (inherited) + code-coloured even with padding.
	if c, ok := spanExact(header, "code"); !ok || !c.Bold || !spanIs(c, pal.code) {
		t.Errorf("padded header code span should stay exact, bold, code-coloured: %+v ok=%v", c, ok)
	}
	// Data row "it" is italic + text-coloured and exact despite the cell being padded.
	row, _ := mdFindLine(lines, "it")
	if i, ok := spanExact(row, "it"); !ok || !i.Italic {
		t.Errorf("padded data italic span should stay exact + italic: %+v ok=%v", i, ok)
	}
	// And the wide plain cell content span is exact (its pad is separate).
	if p, ok := spanExact(row, "plaincellwide"); !ok || p.Bold {
		t.Errorf("plain data span should be exact and not bold: %+v ok=%v", p, ok)
	}
}

// eqInts reports whether two int slices are element-wise equal.
func eqInts(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
