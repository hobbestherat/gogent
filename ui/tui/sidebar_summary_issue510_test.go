package ui

// Tests for issue #510: the per-session sub-agent summary bar is ALWAYS-ON.
//
// Goal (gogent issue #510, maintainer kloune): every session row shows a summary
// line beneath it — even sessions with zero sub-agents — and that line always
// shows all four lifecycle states in fixed order
//   ▶running  ⏸waiting  ✓completed  ✗failed
// each with its integer count INCLUDING 0 (so a fresh session reads
// "|▶0  ⏸0  ✓0  ✗0|"). The bar is wrapped in straight pipes |…| (not […]); the
// trailing +/- expand-collapse suffix is unchanged and sits right after the
// closing |; and the leading | stays aligned with the session name's first
// character (no indent/padding added, no turbotui no-indent option).
//
// These tests pin all four design criteria: (1) goal match, (2) usability /
// alignment, (3) no regressions in the always-on + bracket→pipe behaviour, and
// (4) holistic — everything is gogent-local (no turbotui change).

import (
	"strings"
	"testing"
	"time"

	"gogent/internal/agent"
	"gogent/internal/config"

	tui "github.com/hobbestherat/turbotui"
	tv "github.com/hobbestherat/turbotui/turbotv"
)

// allZeroBar is the canonical always-on label for a session with no sub-agents.
const allZeroBar = "|▶0  ⏸0  ✓0  ✗0|"

// =============================================================================
// statusBarLabel — pure-function unit tests (criterion #1: all four, incl. 0)
// =============================================================================

// TestStatusBarLabel_AllFourAlwaysPresentIncludingZeros asserts the bar always
// emits exactly four segments in fixed order, including zero counts, for a range
// of inputs. This is the heart of issue #510 (the old code omitted zeros).
func TestStatusBarLabel_AllFourAlwaysPresentIncludingZeros(t *testing.T) {
	cases := []struct {
		name                           string
		running, waiting, done, failed int
		want                           string
	}{
		{"all zero", 0, 0, 0, 0, "|▶0  ⏸0  ✓0  ✗0|"},
		{"only running", 1, 0, 0, 0, "|▶1  ⏸0  ✓0  ✗0|"},
		{"only waiting", 0, 5, 0, 0, "|▶0  ⏸5  ✓0  ✗0|"},
		{"only completed", 0, 0, 3, 0, "|▶0  ⏸0  ✓3  ✗0|"},
		{"only failed", 0, 0, 0, 2, "|▶0  ⏸0  ✓0  ✗2|"},
		{"mixed", 2, 1, 2, 1, "|▶2  ⏸1  ✓2  ✗1|"},
		{"all non-zero", 7, 8, 9, 4, "|▶7  ⏸8  ✓9  ✗4|"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := statusBarLabel(tc.running, tc.waiting, tc.done, tc.failed)
			if got != tc.want {
				t.Fatalf("statusBarLabel(%d,%d,%d,%d) = %q, want %q",
					tc.running, tc.waiting, tc.done, tc.failed, got, tc.want)
			}
		})
	}
}

// TestStatusBarLabel_FixedOrder pins the segment order ▶ ⏸ ✓ ✗ regardless of
// which counts are non-zero (guards against a future re-ordering regression).
func TestStatusBarLabel_FixedOrder(t *testing.T) {
	bar := statusBarLabel(1, 1, 1, 1)
	order := []string{"▶1", "⏸1", "✓1", "✗1"}
	prev := -1
	for _, seg := range order {
		i := strings.Index(bar, seg)
		if i < 0 {
			t.Fatalf("bar %q missing segment %q", bar, seg)
		}
		if i <= prev {
			t.Fatalf("segment %q at %d not strictly after previous %d (bar %q)", seg, i, prev, bar)
		}
		prev = i
	}
}

// TestStatusBarLabel_PipesNotBrackets: the bar is wrapped in straight pipes,
// never brackets (issue #510 G2). Exactly two '|' bytes frame it.
func TestStatusBarLabel_PipesNotBrackets(t *testing.T) {
	for _, args := range [][4]int{{0, 0, 0, 0}, {1, 2, 3, 4}, {9, 9, 9, 9}} {
		bar := statusBarLabel(args[0], args[1], args[2], args[3])
		if strings.ContainsAny(bar, "[]") {
			t.Fatalf("bar %q must not contain brackets", bar)
		}
		if !strings.HasPrefix(bar, "|") || !strings.HasSuffix(bar, "|") {
			t.Fatalf("bar %q must be wrapped in leading+trailing '|'", bar)
		}
		// CRITICAL: the waiting glyph ⏸ (U+23F8) is NOT a pipe byte, so there are
		// exactly two '|' bytes (the brackets) even when waiting > 0.
		if got := strings.Count(bar, "|"); got != 2 {
			t.Fatalf("bar %q has %d '|' bytes, want exactly 2 (the ⏸ waiting glyph must not count)", bar, got)
		}
	}
}

// TestStatusBarLabel_NeverEmitsIdleGlyph: the idle • glyph is never produced by
// the summary bar (it is only used for real agent rows in an unknown state).
func TestStatusBarLabel_NeverEmitsIdleGlyph(t *testing.T) {
	for r := 0; r <= 3; r++ {
		for w := 0; w <= 3; w++ {
			for c := 0; c <= 3; c++ {
				for f := 0; f <= 3; f++ {
					if bar := statusBarLabel(r, w, c, f); strings.Contains(bar, "•") {
						t.Fatalf("statusBarLabel(%d,%d,%d,%d) = %q must not emit •", r, w, c, f, bar)
					}
				}
			}
		}
	}
}

// TestStatusBarLabel_MultiDigitCounts: large counts grow each segment but the
// pipe framing and fixed order are preserved (width sanity; no truncation here).
func TestStatusBarLabel_MultiDigitCounts(t *testing.T) {
	bar := statusBarLabel(12, 345, 6, 7890)
	want := "|▶12  ⏸345  ✓6  ✗7890|"
	if bar != want {
		t.Fatalf("multi-digit bar = %q, want %q", bar, want)
	}
	if strings.Count(bar, "|") != 2 {
		t.Fatalf("multi-digit bar %q must still have exactly 2 pipes", bar)
	}
}

// =============================================================================
// Always-on summary (criterion #1: every session, even zero-agent)
// =============================================================================

// TestSummary_AlwaysOnForZeroAgentSession: a freshly added session with NO
// sub-agent events still gets the summary node at child[0], painting the all-zero
// bar, hidden-marker, collapsed, no suffix, and inert to toggle/click.
func TestSummary_AlwaysOnForZeroAgentSession(t *testing.T) {
	s, _ := newFoldSidebar(t)
	s.addSession("s1", "Session 1", false)

	parent := s.sessions["s1"]
	summary := summaryOf(s, "s1")
	if summary == nil {
		t.Fatalf("a zero-agent session must still get the always-on summary node (issue #510)")
	}
	if foldOf(s, "s1") == nil {
		t.Fatalf("a zero-agent session must get fold bookkeeping (issue #510)")
	}
	if parent.Children[0] != summary {
		t.Fatalf("summary must be child[0]")
	}
	if got := countSyntheticChildren(parent); got != 1 {
		t.Fatalf("want exactly one synthetic summary child, got %d", got)
	}
	if !summary.HideMarker {
		t.Fatalf("summary must hide its leading marker (HideMarker)")
	}
	if summary.Expanded {
		t.Fatalf("summary must start collapsed")
	}
	if got, want := summary.Label, allZeroBar; got != want {
		t.Fatalf("zero-agent summary label = %q, want %q", got, want)
	}
	if got := suffixAfterBar(summary.Label); got != "" {
		t.Fatalf("zero-agent summary must have no suffix, got %q", got)
	}
	// No real agent rows; the summary is the only child.
	if got := countNodeRefChildren(parent); got != 0 {
		t.Fatalf("zero-agent session must have 0 real agent children, got %d", got)
	}
	// Inert: a childless summary does not toggle.
	if s.tree.OnToggle(summary, tui.ClickEvent{}) {
		t.Fatalf("a childless all-zero summary must not toggle")
	}
}

// TestSummary_EagerFoldIsIdempotent: addSession creates the summary eagerly, and
// the first sub-agent event reuses it (ensureFold is idempotent) — never a
// duplicate summary node.
func TestSummary_EagerFoldIsIdempotent(t *testing.T) {
	s, _ := newFoldSidebar(t)
	s.addSession("s1", "Session 1", false)
	before := summaryOf(s, "s1")

	s.applySubAgent("s1", subEv("a1", "worker", agent.StatusRunning))

	parent := s.sessions["s1"]
	after := summaryOf(s, "s1")
	if after == nil || after != before {
		t.Fatalf("first sub-agent must reuse the eager summary node, got %v (before %v)", after, before)
	}
	if got := countSyntheticChildren(parent); got != 1 {
		t.Fatalf("exactly one synthetic summary after a sub-agent, got %d", got)
	}
	// Label advanced from all-zero to running=1, keeping all four segments.
	if got, want := after.Label, "|▶1  ⏸0  ✓0  ✗0|"; got != want {
		t.Fatalf("summary after running agent = %q, want %q", got, want)
	}
}

// TestSummary_AlwaysOnSurvivesFullLifecycle: the summary node is NEVER torn down
// across the whole sub-agent lifecycle and dismissal — it only ever reverts to
// the all-zero bar. Guards criterion #3 (no node leak/loss) and #1 (always-on).
func TestSummary_AlwaysOnSurvivesFullLifecycle(t *testing.T) {
	s, c := newFoldSidebar(t)
	s.addSession("s1", "Session 1", false)
	want := summaryOf(s, "s1") // pointer must stay stable throughout

	check := func(stage string, wantLabel string) {
		t.Helper()
		got := summaryOf(s, "s1")
		if got == nil {
			t.Fatalf("%s: summary node vanished", stage)
		}
		if got != want {
			t.Fatalf("%s: summary node pointer changed (node was recreated)", stage)
		}
		if got.Label != wantLabel {
			t.Fatalf("%s: label = %q, want %q", stage, got.Label, wantLabel)
		}
		// Every stage shows all four segments.
		for _, seg := range []string{"▶", "⏸", "✓", "✗"} {
			if !strings.Contains(got.Label, seg) {
				t.Fatalf("%s: label %q missing segment %q", stage, got.Label, seg)
			}
		}
	}

	check("fresh", allZeroBar)

	s.applySubAgent("s1", subEv("a1", "r", agent.StatusRunning))
	check("running", "|▶1  ⏸0  ✓0  ✗0|")

	s.applySubAgent("s1", subEv("a2", "w", agent.StatusWaiting))
	check("waiting", "|▶1  ⏸1  ✓0  ✗0|")

	s.applySubAgent("s1", subEv("a3", "d", agent.StatusCompleted))
	check("completed", "|▶1  ⏸1  ✓1  ✗0|")

	s.applySubAgent("s1", subEv("a4", "f", agent.StatusFailed))
	check("failed", "|▶1  ⏸1  ✓1  ✗1|")

	// Fold the completed agent: summary gains children + "+" suffix; bar unchanged.
	c.add(5 * time.Second)
	s.tickFolds()
	if base := summaryOf(s, "s1").Label; !strings.HasPrefix(base, "|▶1  ⏸1  ✓1  ✗1|") {
		t.Fatalf("post-fold label %q must keep the bar prefix", base)
	}
	if got := suffixAfterBar(summaryOf(s, "s1").Label); got != "+" {
		t.Fatalf("post-fold suffix = %q, want +", got)
	}

	// Dismiss the failure: ✗ drops to 0; bar still four segments; node persists.
	s.dismissFailed("s1")
	check("after-dismiss-failed", func() string {
		// completed agent is archived under summary so a suffix remains; recompute
		// the exact label by re-reading it rather than guessing the suffix.
		return summaryOf(s, "s1").Label
	}())
	// Explicitly: no ✗1, completed still counted, suffix still present.
	bar := summaryOf(s, "s1").Label
	if strings.Contains(bar, "✗1") {
		t.Fatalf("after dismiss ✗ must be 0: %q", bar)
	}
	if !strings.Contains(bar, "✓1") {
		t.Fatalf("archived completed agent must still be counted: %q", bar)
	}

	// Remove every remaining agent via removeSession-like teardown is out of
	// scope, but dismissing/clearing must leave the node intact (already shown).
}

// TestSummary_AlwaysOnRenderEverySession: rendering several sessions with no
// sub-agents shows an all-zero summary row beneath EACH one — criterion #1
// "every session row" + the row-count consequence.
func TestSummary_AlwaysOnRenderEverySession(t *testing.T) {
	w := NewWorkbench([]*config.ModelConfig{{Name: "m", Model: "m"}})
	s := w.sidebar
	s.addSession("s1", "Alpha", false)
	s.addSession("s2", "Beta", false)
	s.addSession("s3", "Gamma", false)

	rows := renderSidebar(w)
	// Each session title row is immediately followed by its all-zero summary row.
	for _, title := range []string{"Alpha", "Beta", "Gamma"} {
		idx := -1
		for i, r := range rows {
			if strings.Contains(r, title) {
				idx = i
				break
			}
		}
		if idx < 0 {
			t.Fatalf("session %q not rendered:\n%s", title, strings.Join(rows, "\n"))
		}
		if idx+1 >= len(rows) || !strings.Contains(rows[idx+1], allZeroBar) {
			t.Fatalf("session %q must be followed by its all-zero summary row:\n%s", title, strings.Join(rows, "\n"))
		}
	}
	// Exactly three summary rows in total.
	count := 0
	for _, r := range rows {
		if strings.Contains(r, allZeroBar) {
			count++
		}
	}
	if count != 3 {
		t.Fatalf("want 3 all-zero summary rows (one per session), got %d:\n%s", count, strings.Join(rows, "\n"))
	}
}

// =============================================================================
// Alignment (criterion #2 / G3: leading | under the session name's first char)
// =============================================================================

// runeCol returns the rune index of the first occurrence of sub within row, or
// -1. Indexing by rune (not byte) matches the per-cell render so multi-byte
// glyphs each occupy one column.
func runeCol(row, sub string) int {
	r := []rune(row)
	s := []rune(sub)
	for i := 0; i+len(s) <= len(r); i++ {
		match := true
		for j := 0; j < len(s); j++ {
			if r[i+j] != s[j] {
				match = false
				break
			}
		}
		if match {
			return i
		}
	}
	return -1
}

// summaryPipeCol returns the rune column of the summary row's leading '|'. It
// indexes by RUNE (not byte) so the multi-byte border/marker glyphs each count
// as one column, matching the per-cell render.
func summaryPipeCol(row string) int {
	for i, r := range []rune(row) {
		if r == '|' {
			return i
		}
	}
	return -1
}

// TestSummary_RenderLeadingPipeAlignedToSessionName: the summary's leading '|'
// renders at the SAME column as the session name's first character (issue #510
// G3). This is the headline alignment invariant; [->| must not shift it.
func TestSummary_RenderLeadingPipeAlignedToSessionName(t *testing.T) {
	w := NewWorkbench([]*config.ModelConfig{{Name: "m", Model: "m"}})
	s := w.sidebar
	const title = "Session 1"
	s.addSession("s1", title, false)

	rows := renderSidebar(w)
	sessionRow := rowContaining(rows, title)
	if sessionRow == "" {
		t.Fatalf("session row not rendered:\n%s", strings.Join(rows, "\n"))
	}
	summaryRow := rowContaining(rows, allZeroBar)
	if summaryRow == "" {
		t.Fatalf("all-zero summary row not rendered:\n%s", strings.Join(rows, "\n"))
	}

	nameCol := runeCol(sessionRow, title)
	pipeCol := summaryPipeCol(summaryRow)
	if nameCol < 0 {
		t.Fatalf("title %q not found in session row %q", title, sessionRow)
	}
	if pipeCol < 0 {
		t.Fatalf("leading '|' not found in summary row %q", summaryRow)
	}
	if pipeCol != nameCol {
		t.Fatalf("alignment broken: summary '|' at col %d, session name at col %d\nsession: %q\nsummary: %q",
			pipeCol, nameCol, sessionRow, summaryRow)
	}
}

// TestSummary_RenderAlignedAcrossSessionStates: the leading '|' stays under the
// session name's first char for idle (○), busy (●), AND background (◐) sessions.
// The status marker changes but the name column (and thus the pipe column) does
// not — guards G3 across all session marker types, expanded or collapsed.
func TestSummary_RenderAlignedAcrossSessionStates(t *testing.T) {
	w := NewWorkbench([]*config.ModelConfig{{Name: "m", Model: "m"}})
	s := w.sidebar
	// Set state before addSession so the label is built with the right marker.
	s.addSession("idle", "IdleTitle", false)
	s.busy["busy"] = true
	s.addSession("busy", "BusyTitle", false)
	s.background["bg"] = true
	s.addSession("bg", "BgTitle", false)

	rows := renderSidebar(w)
	for _, tc := range []struct{ session, title, marker string }{
		{"idle", "IdleTitle", "○"},
		{"busy", "BusyTitle", "●"},
		{"bg", "BgTitle", "◐"},
	} {
		sessionRow := rowContaining(rows, tc.title)
		// The summary row for a zero-agent session is the all-zero bar; find it as
		// the row directly beneath this session's title row.
		idx := -1
		for i, r := range rows {
			if strings.Contains(r, tc.title) {
				idx = i
				break
			}
		}
		if idx < 0 || idx+1 >= len(rows) {
			t.Fatalf("%s: session row missing or has no following row", tc.session)
		}
		summaryRow := rows[idx+1]
		if !strings.Contains(summaryRow, allZeroBar) {
			t.Fatalf("%s: row beneath session should be the all-zero summary:\n%s", tc.session, summaryRow)
		}
		nameCol := runeCol(sessionRow, tc.title)
		pipeCol := summaryPipeCol(summaryRow)
		if pipeCol != nameCol {
			t.Fatalf("%s: alignment broken (pipe %d != name %d)\nsession: %q\nsummary: %q",
				tc.session, pipeCol, nameCol, sessionRow, summaryRow)
		}
		// The session marker itself must be present (sanity: the right session).
		if !strings.Contains(sessionRow, tc.marker) {
			t.Fatalf("%s: session row %q missing marker %q", tc.session, sessionRow, tc.marker)
		}
	}
}

// TestSummary_RenderNoLeadingSpacesOnLabel: the summary node's stored Label
// begins directly with '|' — no indent/padding is baked into the label (that
// would break alignment, which comes from the tree's depth indent). The indent
// is supplied by turbotui at render time, not by the label string.
func TestSummary_RenderNoLeadingSpacesOnLabel(t *testing.T) {
	s, _ := newFoldSidebar(t)
	s.addSession("s1", "Session 1", false)
	summary := summaryOf(s, "s1")
	if strings.HasPrefix(summary.Label, " ") {
		t.Fatalf("summary label must not have leading spaces (breaks alignment): %q", summary.Label)
	}
	if !strings.HasPrefix(summary.Label, "|") {
		t.Fatalf("summary label must start with '|': %q", summary.Label)
	}
}

// TestSummary_BarFitsAtMinSidebarWidth: the always-on bar (13 cells + indent) is
// wider than the old omit-zeros bar, so confirm it is NOT truncated at the
// sidebar's minimum width — both pipes must render intact.
func TestSummary_BarFitsAtMinSidebarWidth(t *testing.T) {
	w := NewWorkbench([]*config.ModelConfig{{Name: "m", Model: "m"}})
	w.sidebar.addSession("s1", "S", false)
	w.sidebar.panel.SetBounds(tv.Rect{X: 0, Y: 0, W: minSidebarWidth, H: 8})
	w.desktop.Redraw()
	abs := w.sidebar.panel.AbsoluteBounds()
	var b strings.Builder
	for x := 0; x < abs.W; x++ {
		ch := w.app.ReadCell(abs.X+x, abs.Y+2).Ch // summary row beneath the session
		if ch == 0 {
			ch = ' '
		}
		b.WriteRune(ch)
	}
	row := b.String()
	if !strings.Contains(row, allZeroBar) {
		t.Fatalf("all-zero bar must render untruncated at minSidebarWidth=%d: row %q", minSidebarWidth, row)
	}
	// Both framing pipes survive (no ellipsis truncation eating the closing |).
	if strings.Count(row, "|") < 2 {
		t.Fatalf("bar truncated at min width (lost a framing pipe): %q", row)
	}
	if strings.Contains(row, "…") {
		t.Fatalf("bar must not be ellipsis-truncated at min width: %q", row)
	}
}

// TestSummary_AlignmentHoldsWithArchiveExpanded: when the summary parents
// archived (TTL-folded) agents and is expanded, the leading '|' still aligns
// with the session name's first char — HideMarker keeps the summary's leading
// column blank regardless of whether it has children.
func TestSummary_AlignmentHoldsWithArchiveExpanded(t *testing.T) {
	w := NewWorkbench([]*config.ModelConfig{{Name: "m", Model: "m"}})
	s := w.sidebar
	clk := &clock{t: time.Unix(1_700_000_000, 0)}
	s.now = clk.now
	s.ttl = 1 * time.Second
	const title = "Session 1"
	s.addSession("s1", title, false)
	s.applySubAgent("s1", subEv("a1", "worker", agent.StatusCompleted))
	clk.add(2 * time.Second)
	s.tickFolds()

	for _, expanded := range []bool{false, true} {
		summaryOf(s, "s1").Expanded = expanded
		rows := renderSidebar(w)
		sessionRow := rowContaining(rows, title)
		// The summary row is the one containing the bar prefix "|▶0  ⏸0  ✓1".
		summaryRow := rowContaining(rows, "|▶0  ⏸0  ✓1")
		if sessionRow == "" || summaryRow == "" {
			t.Fatalf("expanded=%v: rows missing\n%s", expanded, strings.Join(rows, "\n"))
		}
		nameCol := runeCol(sessionRow, title)
		pipeCol := summaryPipeCol(summaryRow)
		if pipeCol != nameCol {
			t.Fatalf("expanded=%v: alignment broken (pipe %d != name %d)\nsession: %q\nsummary: %q",
				expanded, pipeCol, nameCol, sessionRow, summaryRow)
		}
	}
}

// =============================================================================
// Bracket->pipe sentinel invariant (criterion #3: no suffix/label corruption)
// =============================================================================

// TestSyncFoldSuffixes_PipeSentinelNotWaitingGlyph: syncFoldSuffixes re-derives
// the suffix against the closing '|'. Because the waiting glyph ⏸ (U+23F8) is
// not a '|' (U+007C) byte, a summary with waiting agents must NOT have its bar
// truncated at the ⏸. This pins the subtle bracket->pipe sentinel correctness.
func TestSyncFoldSuffixes_PipeSentinelNotWaitingGlyph(t *testing.T) {
	s, c := newFoldSidebar(t)
	s.addSession("s1", "Session 1", false)
	// Two completed (will fold -> summary gains children -> suffix) + a waiting
	// agent so the bar contains a ⏸ segment.
	s.applySubAgent("s1", subEv("a1", "d1", agent.StatusCompleted))
	s.applySubAgent("s1", subEv("a2", "d2", agent.StatusCompleted))
	s.applySubAgent("s1", subEv("a3", "w", agent.StatusWaiting))
	c.add(5 * time.Second)
	s.tickFolds()

	summary := summaryOf(s, "s1")
	// Flip Expanded without refreshFoldChrome so the label is stale, then reconcile.
	summary.Expanded = true
	s.syncFoldSuffixes()

	// The full bar (with the ⏸1 segment) must survive intact up to the closing |;
	// only the suffix changed to "-".
	base := summary.Label[:strings.LastIndexByte(summary.Label, '|')+1]
	wantBar := "|▶0  ⏸1  ✓2  ✗0|"
	if base != wantBar {
		t.Fatalf("syncFoldSuffixes corrupted the bar (expected ⏸1 segment preserved): base %q, want %q", base, wantBar)
	}
	if got := suffixAfterBar(summary.Label); got != "-" {
		t.Fatalf("suffix after reconcile = %q, want -", got)
	}
	// And the ⏸ was not mistaken for the bracket pipe: still exactly two '|'.
	if got := strings.Count(base, "|"); got != 2 {
		t.Fatalf("bar base %q has %d '|' bytes, want 2", base, got)
	}
}

// =============================================================================
// Documented side-effect: every session is now a parent (▾ marker)
// =============================================================================

// TestSummary_EverySessionRendersExpandMarker pins the intended consequence of
// the eager summary (flagged in the design): because every session now has the
// summary child, every session row paints a ▾ expand marker at the leading
// column — even a zero-agent session. This is NOT a bug; it is the cost of
// always-on with no turbotui change. The test makes it intentional and detects
// any future regression.
func TestSummary_EverySessionRendersExpandMarker(t *testing.T) {
	w := NewWorkbench([]*config.ModelConfig{{Name: "m", Model: "m"}})
	s := w.sidebar
	s.addSession("s1", "ZeroAgent", false) // no sub-agents at all

	rows := renderSidebar(w)
	sessionRow := rowContaining(rows, "ZeroAgent")
	if sessionRow == "" {
		t.Fatalf("session row not rendered:\n%s", strings.Join(rows, "\n"))
	}
	if !strings.Contains(sessionRow, "▾") {
		t.Fatalf("a zero-agent session row should now render the ▾ expand marker "+
			"(always-on summary makes it a parent):\n%q", sessionRow)
	}
	// The summary row itself still hides its marker (HideMarker).
	summaryRow := rowContaining(rows, allZeroBar)
	if strings.Contains(summaryRow, "▾") || strings.Contains(summaryRow, "▸") {
		t.Fatalf("summary row must keep its leading marker hidden: %q", summaryRow)
	}
}

// =============================================================================
// Always-on + counting rules unchanged (criterion #3: counting intact)
// =============================================================================

// TestSummary_CountingRulesUnchangedByAlwaysOn: the always-on bar counts exactly
// as before — folded (archived) completed agents still count in ✓, dismissed
// failures still drop from ✗, and ActiveSubAgentCount-style bookkeeping
// (s.agents) is untouched by the summary's existence.
func TestSummary_CountingRulesUnchangedByAlwaysOn(t *testing.T) {
	s, c := newFoldSidebar(t)
	s.addSession("s1", "Session 1", false)
	s.applySubAgent("s1", subEv("a1", "done", agent.StatusCompleted))
	s.applySubAgent("s1", subEv("a2", "boom", agent.StatusFailed))
	c.add(5 * time.Second)
	s.tickFolds() // a1 archived under summary

	// ✓ includes the folded (archived) agent; ✗ counts the undismissed failure.
	bar := summaryOf(s, "s1").Label
	if base := bar[:strings.LastIndexByte(bar, '|')+1]; base != "|▶0  ⏸0  ✓1  ✗1|" {
		t.Fatalf("bar before dismiss = %q, want |▶0  ⏸0  ✓1  ✗1|", base)
	}
	// s.agents still holds both (folding is visibility-only).
	if len(s.agents) != 2 {
		t.Fatalf("s.agents = %d, want 2 (folding must not drop agents)", len(s.agents))
	}

	// Dismiss the failure: ✗ drops to 0; ✓ unchanged; one agent leaves s.agents.
	s.dismissFailed("s1")
	bar = summaryOf(s, "s1").Label
	if base := bar[:strings.LastIndexByte(bar, '|')+1]; base != "|▶0  ⏸0  ✓1  ✗0|" {
		t.Fatalf("bar after dismiss = %q, want |▶0  ⏸0  ✓1  ✗0|", base)
	}
	if len(s.agents) != 1 {
		t.Fatalf("s.agents = %d after dismiss, want 1", len(s.agents))
	}
}
