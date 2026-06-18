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

// transcriptModel is the indexed source of truth for one session's transcript.
// Entries are appended as they arrive and rendered into the backing TextView;
// the model layers find-in-transcript (query) and event-type filtering (hidden)
// on top by re-rendering from the records rather than the painted cells.
type transcriptModel struct {
	view    *tv.TextView
	records []*transcriptRecord
	hidden  kindSet
	query   string // lower-cased active search query, "" when not searching
}

func newTranscriptModel(view *tv.TextView) *transcriptModel {
	return &transcriptModel{view: view}
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
func (m *transcriptModel) add(r *transcriptRecord) *transcriptRecord {
	m.records = append(m.records, r)
	if m.filtering() {
		m.render()
	} else {
		m.renderOne(r)
	}
	return r
}

// renderOne appends a single record's entry (and its children) to the view,
// recording the live entry on the record.
func (m *transcriptModel) renderOne(r *transcriptRecord) {
	entry := m.view.AddColored(r.header, r.color)
	for _, ln := range r.lines {
		entry.AddColored(ln.text, ln.color)
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
// when it is currently rendered.
func (m *transcriptModel) appendLine(r *transcriptRecord, ln styledLine) {
	r.lines = append(r.lines, ln)
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

// setFold collapses or expands every record (fold/unfold all).
func (m *transcriptModel) setFold(collapsed bool) {
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
