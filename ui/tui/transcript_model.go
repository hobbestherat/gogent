package ui

import (
	"encoding/binary"
	"fmt"
	"hash/fnv"
	"io"
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

// colorRole names a semantic palette slot a transcript record or child line is
// drawn from. Records and lines remember a role (not just a frozen tui.Color) so
// a live theme change can recolour the existing transcript when it is re-rendered:
// effectiveColor/headerColor resolve the role against the *current* package
// palette at render time (issue #204). roleNone means "no semantic role — use the
// stored color verbatim", for the few lines that carry an explicit, non-palette
// colour (e.g. permission-dialog body text).
type colorRole uint8

const (
	roleNone colorRole = iota
	roleUser
	roleAgent
	roleNote
	roleTool
	roleResult
	roleInfo
	roleError
)

// roleColor resolves a colorRole to the live package palette variable for that
// role. It is the single point that maps the stable semantic roles onto the
// colours ApplyTheme installs, so a re-render after a theme change paints every
// record in the new palette (issue #204).
func roleColor(role colorRole) tui.Color {
	switch role {
	case roleUser:
		return colorUser
	case roleAgent:
		return colorAgent
	case roleNote:
		return colorNote
	case roleTool:
		return colorTool
	case roleResult:
		return colorResult
	case roleInfo:
		return colorInfo
	case roleError:
		return colorError
	}
	return tui.Color{}
}

// styledLine is one foldable child line with its own colour. role is the semantic
// palette slot the line draws from (issue #204); color is the snapshot taken when
// the line was built and is used only when role is roleNone.
type styledLine struct {
	text  string
	color tui.Color
	role  colorRole
}

// effectiveColor is the colour the line is painted in: the live colour for its
// semantic role, or the stored snapshot when it carries no role. Resolving by
// role at render time is what lets a live theme change recolour existing lines
// (issue #204).
func (ln styledLine) effectiveColor() tui.Color {
	if ln.role != roleNone {
		return roleColor(ln.role)
	}
	return ln.color
}

// transcriptRecord is the model-side view of one transcript entry: a header
// line, its foldable children, and the event kind used for search and filtering.
// entry points at the live TextView entry while the record is rendered (nil when
// it is filtered out), so in-place updates (e.g. a tool result arriving) and
// fold/unfold can mutate the view without a full rebuild.
type transcriptRecord struct {
	kind   eventKind
	header string
	// color is the header colour snapshot taken when the record was built; role is
	// its semantic palette slot (issue #204). headerColor prefers role so a live
	// theme change recolours the existing header on re-render, falling back to color
	// for a record with no role.
	color     tui.Color
	role      colorRole
	lines     []styledLine
	collapsed bool
	// restored marks a record built from the session snapshot by restore() rather
	// than from a live event. The connect-path dedup (issue #516) matches only these
	// when deciding whether a drained backlog event would duplicate the snapshot, so
	// two live events are never deduped against each other (a turn may legitimately
	// repeat a tool call or an identical answer).
	restored bool
	entry    *tv.TextEntry
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

// headerColor is the colour the record's header is painted in: the live colour
// for its semantic role, or the stored snapshot when it carries no role (issue
// #204).
func (r *transcriptRecord) headerColor() tui.Color {
	if r.role != roleNone {
		return roleColor(r.role)
	}
	return r.color
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

// restoredDuplicate reports whether a RESTORED record (one built from the session
// snapshot by restore(), never a live event) of kind k for which match returns true
// sits at the transcript tail with no newer user record since. It is the connect-path
// dedup primitive (issue #516): the global SSE stream is opened (fail-fast) before
// the initial Restore() completes and is drained only afterwards, so a turn that
// finished in the window between the stream opening and the transcript snapshot is
// present in BOTH the restored snapshot AND the buffered live stream; re-applying the
// drained backlog must not duplicate what the snapshot already rendered.
//
// Two guards keep it from ever over-dropping a legitimate live event:
//   - it matches only records flagged restored, so two live events are never deduped
//     against each other (a turn may legitimately repeat a tool call or an identical
//     answer — the earlier one is a live record and so never matches);
//   - it stops at the first user record, so a genuinely new turn's events are always
//     kept even when textually identical to an earlier turn's.
//
// Intervening live/other records are transparent. Callers pass a kind-specific match
// (answer body, tool name, thought body); answer/thought comparisons rebuild the
// text the way the record builders store it (childLines split, joined on newlines)
// so they match body() exactly.
func (m *transcriptModel) restoredDuplicate(k eventKind, match func(*transcriptRecord) bool) bool {
	for i := len(m.records) - 1; i >= 0; i-- {
		r := m.records[i]
		if r == nil {
			continue
		}
		if r.kind == kindUser {
			return false
		}
		if r.restored && r.kind == k && match(r) {
			return true
		}
	}
	return false
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
	// srcLen/srcHash fingerprint the ChatMessage slice this model was last built
	// from via restore() (issue #520). They let the reconnect jump-to-present skip
	// a redundant clear+rebuild when the daemon returns a transcript identical to
	// the one the window already shows (the common case on an early stream flap).
	// They track the SOURCE slice, not the records, so trim() dropping old records
	// never invalidates them. Both are zero on a model that was never restored
	// (e.g. a deferred shell that only ever showed its placeholder).
	srcLen  int
	srcHash uint64
}

func newTranscriptModel(view *tv.TextView) *transcriptModel {
	return &transcriptModel{view: view, limit: defaultTranscriptLimit}
}

// transcriptSourceSig fingerprints a ChatMessage slice as (count, FNV-64a hash)
// so the reconnect refresh can cheaply tell whether a fetched transcript is
// identical to the one a window already shows (issue #520). It hashes every field
// restore() consumes — lower-cased Role (mirroring restore()'s strings.ToLower so
// a casing change never forces a spurious reload), Content, Reasoning, Tool and
// Args — each length-delimited so no value can alias across a field boundary. The
// field set is a complete superset of restore()'s inputs, so two slices with the
// same signature build byte-identical records: a false "unchanged" verdict is
// structurally impossible, and the only risk (a hash collision) is closed by the
// length-delimiting plus the paired count check.
func transcriptSourceSig(msgs []ChatMessage) (int, uint64) {
	h := fnv.New64a()
	var buf [8]byte
	// fnv's Write never returns an error; the returns are discarded explicitly.
	write := func(s string) {
		binary.LittleEndian.PutUint64(buf[:], uint64(len(s)))
		_, _ = h.Write(buf[:])
		_, _ = io.WriteString(h, s)
	}
	for _, m := range msgs {
		write(strings.ToLower(m.Role))
		write(m.Content)
		write(m.Reasoning)
		write(m.Tool)
		write(m.Args)
	}
	return len(msgs), h.Sum64()
}

// matchesSource reports whether the model's current transcript was built from a
// ChatMessage slice with the given (count, hash) signature (issue #520). It is a
// plain equality: every window that reaches the reconnect reload path went through
// restore() at least once, and an empty restore sets srcHash to the FNV offset
// basis (never the uint64 zero value), so a genuinely-empty window still matches a
// genuinely-empty refetch while a never-restored model (srcHash == 0) does not.
func (m *transcriptModel) matchesSource(n int, h uint64) bool {
	return m.srcLen == n && m.srcHash == h
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

// addAll appends every record in one batch and rebuilds the view a single time,
// instead of the per-record incremental append add() does. Restoring a saved
// transcript is M appends of already-built records with no streaming in between,
// so one render() yields the same final view at an O(1) compose rather than the
// O(M) renderOne() calls add() would make (issue #519). It honours the same
// limit/trim contract as add(): over the cap, the oldest batch is dropped via
// trim() and the single render() shows the retained tail.
//
// nil entries are skipped: the restore builders return nil for blank text, and
// render()/visible() dereference every record, so a nil must never reach
// m.records. The append sites already guard, but skipping here as well makes the
// "no nil in records" invariant fail-safe rather than convention.
func (m *transcriptModel) addAll(records []*transcriptRecord) {
	for _, r := range records {
		if r != nil {
			m.records = append(m.records, r)
		}
	}
	if m.limit > 0 && len(m.records) > m.limit {
		// Over the cap: trim() drops the oldest batch and renders once.
		m.trim()
		return
	}
	// Single compose for the whole batch; render() honours the active filter and
	// search via visible(), so a restore under an active filter still produces the
	// correct filtered view and match count in one pass.
	m.render()
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
// A rich record (an assistant answer, when rich Markdown is enabled) renders its
// body as styled children of the header via entry.AddStyled — turbotui styles
// entries uniformly at every nesting depth, so a styled body is a foldable,
// indented child of its header just like a plain one. Folding the header hides
// those children and unfolding restores their styling; the styling survives a
// fold for free. Every other record renders its body as flat colored children,
// which is also the fallback in plain/no-colour mode.
func (m *transcriptModel) renderOne(r *transcriptRecord) {
	entry := m.view.AddColored(r.header, r.headerColor())
	if r.rich && richMarkdownEnabled() {
		for _, spans := range r.markdownSpans() {
			entry.AddStyled(spans) // foldable, indented child of the header
		}
	} else {
		for _, ln := range r.lines {
			entry.AddColored(ln.text, ln.effectiveColor())
		}
	}
	entry.SetCollapsed(r.collapsed)
	r.entry = entry
}

// scrollToBottom re-enables the backing view's follow so the next appended record
// is pinned into view. It is the incremental counterpart to render()'s trailing
// ScrollToBottom: renderOne() respects the current scroll position (so streaming
// does not yank a user who scrolled up), so a caller adding a record the user is
// waiting to see — the turn's final answer — calls this BEFORE the add. The
// TextView re-pins to the bottom on the append's content change only while
// following, so enabling follow first is what makes the new record visible even
// when the user had scrolled up to read earlier output (issue #227). Prefer
// addAndReveal, which bundles the correct order; call this directly only when the
// add cannot go through it.
func (m *transcriptModel) scrollToBottom() { m.view.ScrollToBottom() }

// addAndReveal appends a record with the view re-anchored on it, so it is shown
// even when the user had scrolled up to read earlier output. It enforces the
// otherwise-implicit ordering contract — scrollToBottom MUST precede the add, since
// the view re-pins to the bottom on the append's content change only while
// following — by bundling the two. Records the user is waiting to see (the turn's
// final answer, a turn-ending error) go through here rather than re-anchoring by
// hand, so a future caller cannot reintroduce issue #227 by appending without first
// re-anchoring.
func (m *transcriptModel) addAndReveal(r *transcriptRecord) *transcriptRecord {
	m.scrollToBottom()
	return m.add(r)
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
// when it is currently rendered. A rich record's body renders as styled children
// derived from the raw text (via markdownSpans), so a new line cannot just be
// appended as a plain child — the styled cache must be invalidated and the record
// re-rendered so the Markdown is re-parsed over the grown body. No rich record is
// ever appendLine'd today — only thinking and tool records stream — so this branch
// is currently unreachable defensive code; it keeps the live entry correct if a
// streaming rich path is ever added.
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
		r.entry.AddColored(ln.text, ln.effectiveColor())
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

// setFold collapses or expands every record (fold/unfold all). Every record —
// rich or plain — renders its body as foldable children of the header (see
// renderOne), so folding is a plain collapsed flip handled in place by
// setCollapsed; no full re-render is needed.
//
// Folding is purely in place: it does not re-anchor the view, so a user who has
// scrolled up keeps their position. The old rich path went through render() and
// so snapped to the bottom, but only when a rich record's state actually changed —
// an incidental side effect of that re-render, never a designed behaviour, and
// inconsistent with plain mode (which already preserved position). Preserving
// scroll uniformly is the intended, consistent behaviour.
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
