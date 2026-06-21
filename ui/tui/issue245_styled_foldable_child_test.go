package ui

// Tests for issue #245: render the assistant "Gogent:" answer as indented, foldable
// styled children of its header (entry.AddStyled) instead of flush-left top-level
// entries (m.view.AddStyled), and the three workaround removals that change enables
// in transcript_model.go (the !r.collapsed guard in renderOne, the needRender
// special case in setFold, and the appendLine rationale).
//
// What is and isn't observable from this package.
//
// turbotui keeps every entry-tree field (children, spans, foldable, collapsed) and
// the whole layout/draw path (computeRows/layoutRows/metrics, the Surface that draw
// paints to) unexported. The only exported handle on the tree a gogent test gets is
// TextView.AllText() (which recurses children UNCONDITIONALLY of collapse) and
// TextEntry.GetText(). So the literal fold marker / indent / collapsed-children-hide
// rendering — the heart of the turbotui-side change — cannot be asserted from here;
// that is covered by turbotui's own TestAddStyledFoldableMarkerAndIndent suite.
//
// What these tests DO lock in is everything the gogent-side change is responsible
// for, observed through the exported surface plus the in-package model state:
//
//   - The enabling primitive: (*TextEntry).AddStyled builds a styled child whose
//     plain text is spansText(spans), and switching the rich body from top-level
//     view.AddStyled to header.AddStyled leaves AllText byte-identical (the
//     copy/export/yank guarantee the issue calls out).
//   - renderOne still emits the rich body (markers stripped, content present) and
//     does so as a single child-emission path for rich records.
//   - The !r.collapsed guard is gone: a rich record that is COLLAPSED at render
//     time still goes through the markdown path, so AllText shows the RENDERED
//     (marker-stripped) body — not the raw r.lines the old else-branch emitted.
//   - setFold's needRender full re-render is gone: toggling a rich record is an
//     in-place collapsed flip, so each record's live entry pointer is STABLE across
//     fold/unfold (a full render() would have replaced it).
//   - The plain-mode gate, appendLine cache invalidation, ordering, and the
//     edge/error paths (empty bodies, filtered-out records, palette bumps) all hold.

import (
	"strings"
	"testing"

	tui "github.com/hobbestherat/turbotui"
	tv "github.com/hobbestherat/turbotui/turbotv"
)

// withTestPaletteGen saves/restores mdPaletteGen so a test that bumps it (to force
// markdownSpans to recompute) is hermetic.
func withTestPaletteGen(t *testing.T) {
	t.Helper()
	saved := mdPaletteGen
	t.Cleanup(func() { mdPaletteGen = saved })
}

// spansTextOf mirrors turbotui's package-private spansText: the plain-text form of a
// styled line, which is what a styled entry's .text (and thus AllText) carries.
func spansTextOf(spans []tv.StyledSpan) string {
	var b strings.Builder
	for _, s := range spans {
		b.WriteString(s.Text)
	}
	return b.String()
}

// assistantBodyFromAllText returns the portion of AllText that follows the assistant
// header, i.e. the rendered body lines of the (single) assistant record. It assumes
// the view holds exactly one assistant record and no filter status line.
func assistantBodyFromAllText(all string) string {
	idx := strings.Index(all, "Gogent:")
	if idx < 0 {
		return ""
	}
	return strings.TrimPrefix(all[idx:], "Gogent:")
}

// ---------------------------------------------------------------------------
// The enabling primitive: (*TextEntry).AddStyled and the copy/export guarantee.
// ---------------------------------------------------------------------------

// TestIssue245EntryAddStyledChildText verifies the new constructor builds a child
// whose plain text is the concatenation of the span texts — the property AllText and
// copy rely on — and that it is returned for further chaining like AddColored.
func TestIssue245EntryAddStyledChildText(t *testing.T) {
	view := tv.NewTextView("", tv.Rect{})
	header := view.AddColored("Gogent:", tui.ANSIColor(10))

	spans := []tv.StyledSpan{
		{Text: "bold ", Bold: true, HasFG: true, FG: tui.ANSIColor(9)},
		{Text: "code", HasFG: true, FG: tui.ANSIColor(13)},
	}
	child := header.AddStyled(spans)
	if child == nil {
		t.Fatal("AddStyled returned nil child")
	}
	if got := child.GetText(); got != spansTextOf(spans) {
		t.Errorf("child text = %q, want %q (spansText)", got, spansTextOf(spans))
	}
	if got := view.AllText(); !strings.Contains(got, "Gogent:") || !strings.Contains(got, "bold code") {
		t.Errorf("AllText missing header/body after AddStyled:\n%s", got)
	}
}

// TestIssue245FoldTopologyIsCopyIdentical is the central regression guard for the
// copy/export/yank guarantee: emitting the rich body as styled CHILDREN of the header
// (the new renderOne path) must produce byte-identical AllText to emitting it as
// top-level view.AddStyled entries (the old path). If switching topologies ever
// changed the copied text, this catches it.
func TestIssue245FoldTopologyIsCopyIdentical(t *testing.T) {
	// A representative rendered body: a couple of styled lines.
	bodyLines := [][]tv.StyledSpan{
		{{Text: "Hello "}, {Text: "world", Bold: true, HasFG: true, FG: tui.ANSIColor(9)}},
		{{Text: "inline ", HasFG: true, FG: tui.ANSIColor(7)}, {Text: "code", HasFG: true, FG: tui.ANSIColor(13)}},
	}

	// Old topology: header + body as separate top-level entries.
	old := tv.NewTextView("", tv.Rect{})
	old.AddColored("Gogent:", tui.ANSIColor(10))
	for _, spans := range bodyLines {
		old.AddStyled(spans)
	}

	// New topology: header with styled children.
	newView := tv.NewTextView("", tv.Rect{})
	hdr := newView.AddColored("Gogent:", tui.ANSIColor(10))
	for _, spans := range bodyLines {
		hdr.AddStyled(spans)
	}

	if gotOld, gotNew := old.AllText(), newView.AllText(); gotOld != gotNew {
		t.Errorf("topology change altered AllText (copy/export would regress)\nold:\n%s\nnew:\n%s", gotOld, gotNew)
	}
}

// ---------------------------------------------------------------------------
// renderOne: the rich body is emitted (content present, markers stripped) and the
// assistant portion of AllText equals the rendered Markdown lines exactly.
// ---------------------------------------------------------------------------

// TestIssue245RichAssistantAllTextMatchesRenderedMarkdown asserts that for a range
// of Markdown inputs the assistant body slice in AllText is EXACTLY the rendered
// Markdown (mdAllText(markdownSpans())), i.e. the child path carries the same text
// the old top-level path did. Covers headings, bold, inline code, fenced code,
// lists, HTML entities, unicode, and a long wrapping line.
func TestIssue245RichAssistantAllTextMatchesRenderedMarkdown(t *testing.T) {
	withTestPalette(t)
	withRichState(t, true)
	withTestPaletteGen(t)

	for _, src := range []string{
		"# Title\n\nSome **bold** and `inline`.\n",
		"Code:\n\n```go\npackage main\nfunc f() {}\n```\n",
		"- a\n- b\n- c\n",
		"Entities &amp; &lt;tags&gt; and unicode 日本語 emoji 🎉.\n",
		"Wide glyphs: 中国 字 wide.\n",
		"_line one is a genuinely long line that will certainly need to soft wrap across several terminal columns when rendered at the default width_\n",
	} {
		sw := newTestSession()
		sw.addAssistant(src)
		rec := sw.transcript.lastAssistantRecord()
		if rec == nil {
			t.Fatalf("addAssistant(%q) produced no record", src)
		}
		all := sw.transcript.view.AllText()
		wantBody := mdAllText(renderMarkdown(rec.body()))
		// Header is present and the body follows it.
		if !strings.HasPrefix(all, "Gogent:") {
			t.Errorf("AllText does not start with header:\n%s", all)
		}
		gotBody := assistantBodyFromAllText(all)
		// AllText is "Gogent:\n" + the rendered lines joined by "\n" (appendAllText
		// writes the header text then "\n" before recursing into children, and a
		// final TrimRight removes the trailing newline).
		wantJoined := "Gogent:\n" + wantBody
		if all != wantJoined {
			t.Errorf("AllText body != rendered Markdown for %q\nwant:\n%s\n got:\n%s", src, wantJoined, all)
		}
		// Raw Markdown markers must be stripped from the display text.
		for _, marker := range []string{"**", "```", "`inline`"} {
			if strings.Contains(gotBody, marker) && strings.Contains(src, marker) {
				// Only fail when the source actually contained the marker (entities have none).
				t.Errorf("rendered body still contains raw marker %q for %q:\n%s", marker, src, gotBody)
			}
		}
	}
}

// TestIssue245RichAssistantContentPresent is a coarser guard: even without knowing
// the exact rendering, the meaningful content words must appear in AllText.
func TestIssue245RichAssistantContentPresent(t *testing.T) {
	withRichState(t, true)
	sw := newTestSession()
	sw.addAssistant("Hello **world** with `code`.\n\n```go\nfunc main() {}\n```\n")
	all := sw.transcript.view.AllText()
	for _, want := range []string{"Gogent:", "world", "code", "func main() {}"} {
		if !strings.Contains(all, want) {
			t.Errorf("AllText missing rendered content %q:\n%s", want, all)
		}
	}
}

// ---------------------------------------------------------------------------
// The !r.collapsed guard removal: a rich record that is COLLAPSED at render time
// still goes through the markdown path.
//
// This is observable through AllText. OLD renderOne gated the rich branch on
// `!r.collapsed`, so a collapsed rich record fell through to the else-branch and
// emitted the RAW r.lines ("**bold**"). NEW renderOne has no guard, so a collapsed
// rich record emits the RENDERED markdown ("bold"). The body is hidden when
// collapsed either way (AllText ignores collapse), but the copy/export TEXT now
// matches the expanded rendering — consistent, where the old code diverged.
// ---------------------------------------------------------------------------

// TestIssue245CollapsedRichRendersMarkdownNotRaw locks in the guard removal: a rich
// record rendered while collapsed shows marker-stripped text in AllText, identical
// to its expanded form.
func TestIssue245CollapsedRichRendersMarkdownNotRaw(t *testing.T) {
	withTestPalette(t)
	withRichState(t, true)
	withTestPaletteGen(t)

	mk := func(collapsed bool) *transcriptModel {
		sw := newTestSession()
		rec := &transcriptRecord{
			kind:   kindAssistant,
			header: "Gogent:",
			color:  colorAgent,
			role:   roleAgent,
			lines:  styledChildLines("Some **bold** and `code` here", roleAgent),
			rich:   true,
		}
		rec.collapsed = collapsed
		sw.transcript.add(rec)
		return sw.transcript
	}

	expanded := mk(false).view.AllText()
	collapsed := mk(true).view.AllText()

	// The collapsed record must NOT carry the raw Markdown markers — it went through
	// the markdown path, not the raw-lines else-branch.
	for _, raw := range []string{"**bold**", "`code`"} {
		if strings.Contains(collapsed, raw) {
			t.Errorf("collapsed rich record emitted raw marker %q (guard not removed / else-branch taken):\n%s", raw, collapsed)
		}
	}
	// And it must still contain the rendered words.
	for _, want := range []string{"bold", "code"} {
		if !strings.Contains(collapsed, want) {
			t.Errorf("collapsed rich record lost rendered content %q:\n%s", want, collapsed)
		}
	}
	// Collapsed and expanded must produce identical body text (the consistency fix).
	if collapsed != expanded {
		t.Errorf("collapsed vs expanded AllText differ (copy/export would depend on fold state)\nexpanded:\n%s\ncollapsed:\n%s", expanded, collapsed)
	}
}

// ---------------------------------------------------------------------------
// setFold: the needRender full-re-render special case is gone. Toggling a rich
// record is an in-place collapsed flip, so each record's live entry pointer is
// STABLE across fold/unfold. A full render() (the old needRender path) would have
// nilled and re-created every entry, changing the pointer.
// ---------------------------------------------------------------------------

// TestIssue245SetFoldRichKeepsEntryStable asserts that fold/unfold-all leaves every
// record's r.entry pointer unchanged — only possible because setFold no longer
// rebuilds the view for rich records.
func TestIssue245SetFoldRichKeepsEntryStable(t *testing.T) {
	withRichState(t, true)
	sw := newTestSession()
	populate(sw) // includes a rich assistant answer
	m := sw.transcript

	// Sanity: there is at least one rich record affected by the old needRender path.
	richCount := 0
	for _, r := range m.records {
		if r.entry != nil {
			richCount++
		}
	}
	if richCount == 0 {
		t.Fatal("expected rendered records to anchor entry pointers")
	}

	before := make([]*tv.TextEntry, len(m.records))
	for i, r := range m.records {
		before[i] = r.entry
	}

	m.setFold(true)
	for i, r := range m.records {
		if !r.collapsed {
			t.Errorf("record %q not collapsed after fold-all", r.header)
		}
		if r.entry == nil || r.entry != before[i] {
			t.Errorf("record %q entry pointer changed across setFold(true) (in-place flip expected, full re-render happened)", r.header)
		}
	}
	// AllText still contains the bodies (AllText recurses children regardless of collapse).
	all := m.view.AllText()
	for _, want := range []string{"done reading the file", "hello world"} {
		if !strings.Contains(all, want) {
			t.Errorf("AllText lost body after fold-all:\n%s", all)
		}
	}

	m.setFold(false)
	for i, r := range m.records {
		if r.collapsed {
			t.Errorf("record %q still collapsed after unfold-all", r.header)
		}
		if r.entry == nil || r.entry != before[i] {
			t.Errorf("record %q entry pointer changed across setFold(false)", r.header)
		}
	}
}

// TestIssue245SetCollapsedIndividualRichRich verifies the per-record fold primitive
// on a rich record round-trips and keeps the entry stable.
func TestIssue245SetCollapsedIndividualRich(t *testing.T) {
	withRichState(t, true)
	sw := newTestSession()
	sw.addAssistant("# H\n\nbody\n")
	rec := sw.transcript.lastAssistantRecord()
	if rec == nil {
		t.Fatal("no assistant record")
	}
	entry := rec.entry
	if entry == nil {
		t.Fatal("rich record has no live entry after add")
	}
	sw.transcript.setCollapsed(rec, true)
	if !rec.collapsed {
		t.Error("setCollapsed(true) did not set rec.collapsed")
	}
	if rec.entry != entry {
		t.Error("setCollapsed changed the entry pointer (should be in-place)")
	}
	sw.transcript.setCollapsed(rec, false)
	if rec.collapsed {
		t.Error("setCollapsed(false) did not clear rec.collapsed")
	}
}

// ---------------------------------------------------------------------------
// Error handling: setFold / setCollapsed must not panic when some records are
// filtered out (r.entry == nil), and must still set r.collapsed on every record so
// the state is correct when the filter is later cleared and the view rebuilt.
// ---------------------------------------------------------------------------

// TestIssue245SetFoldWithFilteredRecordsNoPanic hides some records via the type
// filter (so their r.entry is nil after re-render) then folds/unfolds all.
func TestIssue245SetFoldWithFilteredRecordsNoPanic(t *testing.T) {
	withRichState(t, true)
	sw := newTestSession()
	populate(sw)
	m := sw.transcript

	// Hide thinking + tools; render() rebuilds so those records have r.entry == nil.
	m.toggleKind(kindThinking)
	m.toggleKind(kindTool)

	// Confirm at least one record is unrendered (entry == nil).
	nilEntries := 0
	for _, r := range m.records {
		if r.entry == nil {
			nilEntries++
		}
	}
	if nilEntries == 0 {
		t.Fatal("expected at least one filtered-out record with nil entry")
	}

	// Folding must set r.collapsed on ALL records (including the nil-entry ones)
	// without panicking.
	m.setFold(true)
	for _, r := range m.records {
		if !r.collapsed {
			t.Errorf("filtered-out fold did not collapse record %q", r.header)
		}
	}
	m.setFold(false)
	for _, r := range m.records {
		if r.collapsed {
			t.Errorf("record %q still collapsed after unfold-all", r.header)
		}
	}

	// Clearing the filter re-renders and must honour the (now cleared) fold state.
	m.showAll()
	for _, r := range m.records {
		if r.collapsed {
			t.Errorf("record %q collapsed after showAll (should be expanded)", r.header)
		}
		if r.entry == nil {
			t.Errorf("record %q has nil entry after showAll", r.header)
		}
	}
}

// ---------------------------------------------------------------------------
// appendLine on a rich record: the styled cache is invalidated and the record is
// re-rendered so the grown body shows. (Logic unchanged; the comment rationale was
// updated. This guards against a regression in that path.)
// ---------------------------------------------------------------------------

// TestIssue245AppendLineRichInvalidatesCacheAndRerenders adds a line to an already
// rendered rich record and asserts the cache is dropped and the new text appears.
func TestIssue245AppendLineRichInvalidatesCacheAndRerenders(t *testing.T) {
	withTestPalette(t)
	withRichState(t, true)
	withTestPaletteGen(t)

	sw := newTestSession()
	sw.addAssistant("first line")
	m := sw.transcript
	rec := m.lastAssistantRecord()
	if rec == nil {
		t.Fatal("no assistant record")
	}
	// Force the cache to be populated with the OLD body, then prove appendLine
	// invalidates it: the next markdownSpans() must reflect the GROWN body, not the
	// stale pre-append spans. (appendLine sets r.styled = nil and the re-render it
	// triggers repopulates the cache over body(), so we observe the invalidation
	// through the recomputed content rather than a transient nil.)
	before := mdAllText(rec.markdownSpans())
	if strings.Contains(before, "appended second line") {
		t.Fatalf("precondition: cached spans already contain the appended line")
	}

	m.appendLine(rec, styledLine{text: "appended second line", color: roleColor(roleAgent), role: roleAgent})

	// The styled cache was invalidated and recomputed over the grown body.
	after := mdAllText(rec.markdownSpans())
	if !strings.Contains(after, "appended second line") {
		t.Errorf("appendLine did not invalidate the styled cache: markdownSpans() still serves the pre-append body\nbefore:\n%s\nafter:\n%s", before, after)
	}
	if got := m.view.AllText(); !strings.Contains(got, "appended second line") {
		t.Errorf("appended line missing from re-rendered view:\n%s", got)
	}
	// The grown body now re-parses to include the new line.
	if body := rec.body(); !strings.Contains(body, "appended second line") {
		t.Errorf("body() does not contain appended line: %q", body)
	}
}

// TestIssue245AppendLineRichUnrenderedNoPanic appends to a rich record that is not
// currently rendered (entry == nil): it must invalidate the cache without rendering
// and without panicking.
func TestIssue245AppendLineRichUnrenderedNoPanic(t *testing.T) {
	withRichState(t, true)
	sw := newTestSession()
	sw.addAssistant("base")
	m := sw.transcript
	rec := m.lastAssistantRecord()
	// Drop the live entry to simulate a record that is currently filtered out.
	rec.entry = nil
	rec.markdownSpans()

	m.appendLine(rec, styledLine{text: "late line", color: roleColor(roleAgent), role: roleAgent})
	if rec.styled != nil {
		t.Error("styled cache should be invalidated even when entry is nil")
	}
}

// ---------------------------------------------------------------------------
// Plain-mode gate: with rich Markdown disabled, renderOne takes the else-branch and
// emits the RAW text as flat colored children (markers visible).
// ---------------------------------------------------------------------------

// TestIssue245PlainModeEmitsRawFlatChildren confirms the else-branch still fires for
// an assistant record when rich Markdown is off.
func TestIssue245PlainModeEmitsRawFlatChildren(t *testing.T) {
	withRichState(t, false)
	sw := newTestSession()
	sw.addAssistant("Hello **world** with `code`.\n\n```go\nfunc main() {}\n```\n")
	all := sw.transcript.view.AllText()
	for _, raw := range []string{"**world**", "`code`", "```go", "func main() {}"} {
		if !strings.Contains(all, raw) {
			t.Errorf("plain-mode view should contain raw %q:\n%s", raw, all)
		}
	}
}

// ---------------------------------------------------------------------------
// Edge cases: empty / whitespace bodies, and interleaved records preserving order.
// ---------------------------------------------------------------------------

// TestIssue245EmptyAssistantBodySkipped: addAssistant skips empty/whitespace-only
// text, so no record is created.
func TestIssue245EmptyAssistantBodySkipped(t *testing.T) {
	withRichState(t, true)
	for _, in := range []string{"", "   ", "\n\n\t  \n"} {
		sw := newTestSession()
		sw.addAssistant(in)
		if rec := sw.transcript.lastAssistantRecord(); rec != nil {
			t.Errorf("addAssistant(%q) should not create a record, got %v", in, rec)
		}
	}
}

// TestIssue245ManuallyBuiltEmptyRichRendersHeader renders a rich record whose body
// is empty/whitespace directly via renderOne and asserts no panic and the header is
// present. markdownSpans over an empty body must be safe.
func TestIssue245ManuallyBuiltEmptyRichRendersHeader(t *testing.T) {
	withRichState(t, true)
	for _, body := range []string{"", "   ", "\n\n"} {
		sw := newTestSession()
		rec := &transcriptRecord{
			kind:   kindAssistant,
			header: "Gogent:",
			color:  colorAgent,
			role:   roleAgent,
			lines:  styledChildLines(body, roleAgent),
			rich:   true,
		}
		sw.transcript.add(rec)
		if rec.entry == nil {
			t.Errorf("rich record with body %q has nil entry after add", body)
		}
		if all := sw.transcript.view.AllText(); !strings.HasPrefix(all, "Gogent:") {
			t.Errorf("header missing for body %q:\n%s", body, all)
		}
	}
}

// TestIssue245ManuallyBuiltEmptyRichCollapsedNoPanic repeats the above with the
// record collapsed at add time (the guard-removal path with an empty body).
func TestIssue245ManuallyBuiltEmptyRichCollapsedNoPanic(t *testing.T) {
	withRichState(t, true)
	sw := newTestSession()
	rec := &transcriptRecord{
		kind:      kindAssistant,
		header:    "Gogent:",
		color:     colorAgent,
		role:      roleAgent,
		lines:     styledChildLines("", roleAgent),
		rich:      true,
		collapsed: true,
	}
	sw.transcript.add(rec)
	if rec.entry == nil {
		t.Error("collapsed empty rich record has nil entry")
	}
}

// TestIssue245InterleavedRecordsPreserveOrder builds a transcript with several rich
// assistant answers interleaved with user/tool records and asserts the bodies land
// under the right headers in AllText.
func TestIssue245InterleavedRecordsPreserveOrder(t *testing.T) {
	withRichState(t, true)
	sw := newTestSession()
	sw.addUser("question one")
	sw.addAssistant("# Answer one\n\nbody one")
	sw.beginToolCall("call_read", "Read", map[string]interface{}{"path": "a.go"})
	sw.finishToolCall("call_read", "Read", "result one")
	sw.addAssistant("Answer **two**")
	sw.addUser("question two")
	all := sw.transcript.view.AllText()

	// Each header must be immediately followed by its own body and precede the next.
	idx := func(s string) int { return strings.Index(all, s) }
	order := []string{"You:", "Answer one", "tool: Read", "result one", "Answer two", "question two"}
	for i := 0; i+1 < len(order); i++ {
		if idx(order[i]) >= idx(order[i+1]) {
			t.Errorf("order not preserved: %q (at %d) should precede %q (at %d)\n%s",
				order[i], idx(order[i]), order[i+1], idx(order[i+1]), all)
		}
	}
	// The second assistant's marker is stripped (rich), the body word present.
	if strings.Contains(all, "Answer **two**") {
		t.Errorf("second rich answer leaked raw marker:\n%s", all)
	}
	if !strings.Contains(all, "Answer two") {
		t.Errorf("second rich answer lost rendered body:\n%s", all)
	}
}

// ---------------------------------------------------------------------------
// markdownSpans cache + re-render after a palette generation bump: the cached spans
// are recomputed and the re-rendered view still matches the fresh Markdown.
// ---------------------------------------------------------------------------

// TestIssue245MarkdownCacheRecomputesAfterPaletteBump bumps mdPaletteGen after a
// rich record is rendered and asserts markdownSpans recomputes (styledGen tracks
// the new generation) and a re-render stays consistent.
func TestIssue245MarkdownCacheRecomputesAfterPaletteBump(t *testing.T) {
	withTestPalette(t)
	withRichState(t, true)
	withTestPaletteGen(t)

	sw := newTestSession()
	sw.addAssistant("# H\n\nbody text\n")
	rec := sw.transcript.lastAssistantRecord()
	rec.markdownSpans()
	gen0 := rec.styledGen

	mdPaletteGen++ // simulate a theme change invalidating the cache
	rec.markdownSpans()
	if rec.styledGen == gen0 {
		t.Errorf("styledGen did not advance after mdPaletteGen bump (cache not recomputed)")
	}
	if rec.styledGen != mdPaletteGen {
		t.Errorf("styledGen=%d, want mdPaletteGen=%d", rec.styledGen, mdPaletteGen)
	}
	// Re-render the whole view (as a theme change would) and confirm correctness.
	sw.transcript.render()
	all := sw.transcript.view.AllText()
	for _, want := range []string{"Gogent:", "H", "body text"} {
		if !strings.Contains(all, want) {
			t.Errorf("re-rendered view missing %q after palette bump:\n%s", want, all)
		}
	}
}
