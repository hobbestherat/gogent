package ui

import (
	"fmt"
	"strings"

	tui "github.com/hobbestherat/turbotui"
	tv "github.com/hobbestherat/turbotui/turbotv"
)

// eventKind classifies a transcript record so it can be indexed for the
// in-transcript search and the event-type filter. The set mirrors the live
// session events plus the system/compaction housekeeping lines.
type eventKind int

const (
	kindSystem eventKind = iota
	kindUser
	kindAssistant
	kindThinking
	kindTool // a tool call together with its result (and restored tool messages)
	kindError
	kindCompaction
)

// kindSet is a bitmask of eventKinds, used to track which event types are
// currently hidden by the filter.
type kindSet uint

func (k eventKind) bit() kindSet { return 1 << uint(k) }

// label returns the human-readable name used in the filter status line.
func (k eventKind) label() string {
	switch k {
	case kindSystem:
		return "system"
	case kindUser:
		return "user"
	case kindAssistant:
		return "messages"
	case kindThinking:
		return "thinking"
	case kindTool:
		return "tools"
	case kindError:
		return "errors"
	case kindCompaction:
		return "compaction"
	}
	return "?"
}

// styledLine is one foldable child line with its own colour.
type styledLine struct {
	text  string
	color tui.Color
}

// transcriptRecord is the model-side view of one transcript entry: a header
// line, its foldable children, and the event kind used for search and filtering.
// entry points at the live TextView entry while the record is rendered (nil when
// it is filtered out), so in-place updates (e.g. a tool result arriving) and
// fold/unfold can mutate the view without a full rebuild.
type transcriptRecord struct {
	kind      eventKind
	header    string
	color     tui.Color
	lines     []styledLine
	collapsed bool
	entry     *tv.TextEntry
	// rich marks a record whose body should be rendered as formatted Markdown
	// (issue #184) when rich rendering is enabled. lines still holds the raw text
	// so copy/export/search are unchanged; the styled rendering is derived from it
	// at render time. Set for assistant answers.
	rich bool
	// styled caches the rendered Markdown spans for a rich record so re-renders
	// (search-as-you-type, filter toggles, trims) do not re-parse and re-tokenise.
	// styledGen records the palette generation it was built with; a theme change
	// bumps mdPaletteGen and invalidates the cache.
	//
	// Cache invariant: styled is derived from lines (via body()), so any code that
	// mutates lines MUST reset styled to nil. appendLine — the only mutator today —
	// does this; a future streaming/append path must too, or it will serve stale
	// spans.
	styled    [][]tv.StyledSpan
	styledGen uint64
}

// markdownSpans returns the record's rendered Markdown lines, computing and
// caching them on first use and recomputing them after a theme change.
func (r *transcriptRecord) markdownSpans() [][]tv.StyledSpan {
	if r.styled == nil || r.styledGen != mdPaletteGen {
		r.styled = renderMarkdown(r.body())
		r.styledGen = mdPaletteGen
	}
	return r.styled
}

// matches reports whether the record's header or any child line contains query
// (case-insensitive). The query is assumed already lower-cased by the caller.
func (r *transcriptRecord) matches(query string) bool {
	if strings.Contains(strings.ToLower(r.header), query) {
		return true
	}
	for _, ln := range r.lines {
		if strings.Contains(strings.ToLower(ln.text), query) {
			return true
		}
	}
	return false
}

// body joins a record's child lines back into the text they were rendered from.
// For a user or assistant record — whose lines carry the verbatim message split
// on newlines — this reconstructs the original message text, which the yank
// actions copy to the clipboard (issue #62).
func (r *transcriptRecord) body() string {
	if r == nil {
		return ""
	}
	parts := make([]string, len(r.lines))
	for i, ln := range r.lines {
		parts[i] = ln.text
	}
	return strings.Join(parts, "\n")
}

// lastAssistantRecord returns the most recent assistant record, or nil when the
// transcript has no assistant message yet. It anchors the yank-last-answer and
// yank-last-code actions (issue #62).
func (m *transcriptModel) lastAssistantRecord() *transcriptRecord {
	for i := len(m.records) - 1; i >= 0; i-- {
		if m.records[i].kind == kindAssistant {
			return m.records[i]
		}
	}
	return nil
}

// defaultTranscriptLimit bounds the number of records a session keeps live in its
// TextView. The view otherwise grows without bound — every event, folded tool
// result and compaction digest is appended forever — so capping it bounds both
// memory and the per-frame render cost over a long session. Context compaction
// shrinks the model context but not the widget tree, so the UI needs its own cap
// (issue #22). The durable transcript lives in the session JSONL, so dropping old
// records from the in-memory view loses nothing queryable.
const defaultTranscriptLimit = 1000

// transcriptModel is the indexed source of truth for one session's transcript.
// Entries are appended as they arrive and rendered into the backing TextView;
// the model layers find-in-transcript (query) and event-type filtering (hidden)
// on top by re-rendering from the records rather than the painted cells.
type transcriptModel struct {
	view    *tv.TextView
	records []*transcriptRecord
	hidden  kindSet
	query   string // lower-cased active search query, "" when not searching
	// limit caps the number of records kept live in the TextView (0 = unbounded).
	// The oldest records are dropped once it is exceeded; the newest — including
	// any in-flight tool entry — are always kept.
	limit int
}

func newTranscriptModel(view *tv.TextView) *transcriptModel {
	return &transcriptModel{view: view, limit: defaultTranscriptLimit}
}

// filtering reports whether a search or any type filter is currently active.
func (m *transcriptModel) filtering() bool { return m.query != "" || m.hidden != 0 }

// visible reports whether a record passes the current filter and search.
func (m *transcriptModel) visible(r *transcriptRecord) bool {
	if m.hidden&r.kind.bit() != 0 {
		return false
	}
	if m.query != "" && !r.matches(m.query) {
		return false
	}
	return true
}

// add appends a record and reflects it in the view. While not filtering this is
// a cheap append (matching the live streaming path); while a filter or search is
// active the view is rebuilt so the new entry and the match count stay correct.
// Once the record count exceeds the limit the oldest batch is dropped and the
// view rebuilt, so the live TextView cannot grow without bound.
func (m *transcriptModel) add(r *transcriptRecord) *transcriptRecord {
	m.records = append(m.records, r)
	if m.limit > 0 && len(m.records) > m.limit {
		// Over the cap: drop the oldest batch and rebuild the view from what
		// remains. The TextView exposes no per-entry removal, so a rebuild is the
		// only way to reflect dropped records. The just-appended r is the newest
		// record, so render() renders it too — skip the per-record path below.
		m.trim()
		return r
	}
	if m.filtering() {
		m.render()
	} else {
		m.renderOne(r)
	}
	return r
}

// trim drops the oldest records to bring the transcript back under its limit,
// then rebuilds the view. Roughly a tenth of the limit is dropped at once so the
// full rebuild is amortised across many adds rather than firing on every add
// while streaming past the limit. Only head (oldest) records are removed, so the
// in-flight tool entry tracked by the session — always the newest record — is
// never dropped mid-call. The retained records are copied into a fresh slice so
// the dropped ones (and their folded children) are released to the GC.
func (m *transcriptModel) trim() {
	keep := m.limit - m.limit/10
	if keep < 1 {
		keep = 1
	}
	if len(m.records) > keep {
		retained := make([]*transcriptRecord, keep)
		copy(retained, m.records[len(m.records)-keep:])
		m.records = retained
	}
	m.render()
}

// renderOne appends a single record's entry (and its children) to the view,
// recording the live entry on the record.
//
// An expanded rich record (an assistant answer, when rich Markdown is enabled)
// renders its header as a plain entry and its body as top-level styled lines
// derived from the raw text — turbotui styles only top-level entries, so a styled
// body cannot be a foldable child of its header. A collapsed rich record instead
// uses the flat children path so it can actually fold (fold-all re-renders, see
// setFold); since the body is hidden when collapsed, losing the styling there is
// invisible. Every other record always uses the flat children path, which keeps
// folding and is the fallback in plain/no-colour mode.
func (m *transcriptModel) renderOne(r *transcriptRecord) {
	entry := m.view.AddColored(r.header, r.color)
	if r.rich && richMarkdownEnabled() && !r.collapsed {
		for _, spans := range r.markdownSpans() {
			m.view.AddStyled(spans)
		}
	} else {
		for _, ln := range r.lines {
			entry.AddColored(ln.text, ln.color)
		}
	}
	entry.SetCollapsed(r.collapsed)
	r.entry = entry
}

// render rebuilds the whole view from the records, honouring the active filter
// and search and prefixing a status line while either is active.
func (m *transcriptModel) render() {
	m.view.Clear()
	for _, r := range m.records {
		r.entry = nil
	}
	if note := m.filterNote(); note != "" {
		m.view.AddColored(note, colorInfo)
	}
	for _, r := range m.records {
		if m.visible(r) {
			m.renderOne(r)
		}
	}
	m.view.ScrollToBottom()
}

// appendLine grows a record's children, mirroring the change into the live entry
// when it is currently rendered. A rich record's body is not made of children of
// r.entry (it renders as styled top-level entries), so appending live would
// attach an orphan child under the header; rich records are added whole and never
// streamed, but invalidate the styled cache and re-render defensively to stay
// correct if that ever changes.
func (m *transcriptModel) appendLine(r *transcriptRecord, ln styledLine) {
	r.lines = append(r.lines, ln)
	if r.rich {
		r.styled = nil
		if r.entry != nil {
			m.render()
		}
		return
	}
	if r.entry != nil {
		r.entry.AddColored(ln.text, ln.color)
	}
}

// setHeader replaces a record's header text in the model and the live entry.
func (m *transcriptModel) setHeader(r *transcriptRecord, header string) {
	r.header = header
	if r.entry != nil {
		r.entry.SetText(header)
	}
}

// setCollapsed sets a record's fold state in the model and the live entry.
func (m *transcriptModel) setCollapsed(r *transcriptRecord, collapsed bool) {
	r.collapsed = collapsed
	if r.entry != nil {
		r.entry.SetCollapsed(collapsed)
	}
}

// setQuery sets the active search query (case-insensitively) and re-renders.
func (m *transcriptModel) setQuery(query string) {
	m.query = strings.ToLower(strings.TrimSpace(query))
	m.render()
}

// toggleKind flips whether a given event kind is hidden, then re-renders.
func (m *transcriptModel) toggleKind(k eventKind) {
	m.hidden ^= k.bit()
	m.render()
}

// showAll clears the search and every type filter, restoring the full view.
func (m *transcriptModel) showAll() {
	m.query = ""
	m.hidden = 0
	m.render()
}

// setFold collapses or expands every record (fold/unfold all). A rich record
// renders its body as styled top-level entries when expanded but as foldable
// children when collapsed (see renderOne), so toggling one needs a full re-render
// to switch forms; the cheap in-place path is kept when no rich record is
// affected (plain mode, or no assistant answers).
func (m *transcriptModel) setFold(collapsed bool) {
	needRender := false
	if richMarkdownEnabled() {
		for _, r := range m.records {
			if r.rich && r.collapsed != collapsed {
				needRender = true
				break
			}
		}
	}
	if needRender {
		for _, r := range m.records {
			r.collapsed = collapsed
		}
		m.render()
		return
	}
	for _, r := range m.records {
		m.setCollapsed(r, collapsed)
	}
}

// matchCount returns the number of currently-visible records matching the search
// (0 when not searching).
func (m *transcriptModel) matchCount() int {
	if m.query == "" {
		return 0
	}
	n := 0
	for _, r := range m.records {
		if m.visible(r) {
			n++
		}
	}
	return n
}

// hiddenNames lists the labels of the hidden kinds in a stable order.
func (m *transcriptModel) hiddenNames() string {
	var names []string
	for k := kindSystem; k <= kindCompaction; k++ {
		if m.hidden&k.bit() != 0 {
			names = append(names, k.label())
		}
	}
	return strings.Join(names, ", ")
}

// filterNote builds the dim status line shown at the top of the transcript while
// a search or filter is active, summarising what is in effect.
func (m *transcriptModel) filterNote() string {
	if !m.filtering() {
		return ""
	}
	var parts []string
	if m.query != "" {
		parts = append(parts, fmt.Sprintf("find %q: %d", m.query, m.matchCount()))
	}
	if names := m.hiddenNames(); names != "" {
		parts = append(parts, "hidden: "+names)
	}
	return "— " + strings.Join(parts, " · ") + " · Esc to clear —"
}
