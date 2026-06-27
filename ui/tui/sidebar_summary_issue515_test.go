package ui

// Tests for issue #515: widen the sub-agent summary bar to 2-space entries and
// replace the waiting glyph ‖ (U+2016 DOUBLE VERTICAL LINE) with ⏸
// (U+23F8 DOUBLE VERTICAL BAR / pause).
//
// These tests pin the NEW behavior introduced by #515 and adversarially probe
// all four design criteria. They are deliberately SELF-CONTAINED: they define
// their own expected-bar constants (issue515AllZeroBar) rather than reusing the
// #510 `allZeroBar` literal, so they validate the implementation independently of
// whether the older #510 test literals were updated in lockstep.
//
// Coverage map (design criteria):
//   (1) Goal match     — TestIssue515_StatusIcon*, TestIssue515_Bar*,
//                        TestIssue515_AgentRowUsesPauseGlyph, ...Order
//   (2) Usability      — TestIssue515_*MinSidebarWidth, *LeadingPipeAligned*,
//                        *SuffixAfterClosingPipe*
//   (3) No regressions — TestIssue515_PipeSentinel*, *SyncFoldSuffixes*,
//                        *NoEmojiVariationSelector*, *OtherGlyphsUnchanged,
//                        *WidthDeltaIsThree*
//   (4) Holistic       — TestIssue515_PauseGlyphIsWidthOneInTurbotui,
//                        *WidthDeltaIsThree* (cross-repo RuneWidth contract)

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"gogent/internal/agent"
	"gogent/internal/config"

	tui "github.com/hobbestherat/turbotui"
	tv "github.com/hobbestherat/turbotui/turbotv"
)

// pauseGlyph is the U+23F8 DOUBLE VERTICAL BAR (pause) introduced by #515.
const pauseGlyph = "⏸"

// issue515AllZeroBar is the post-#515 always-on label for a zero-agent session:
// pause glyph + 2-space inter-entry gaps (was "|▶0 ‖0 ✓0 ✗0|" under #510).
const issue515AllZeroBar = "|▶0  ⏸0  ✓0  ✗0|"

// oldAllZeroBar is the pre-#515 form, kept only to assert the +3-cell width delta
// and that the glyph swap is width-neutral.
const oldAllZeroBar = "|▶0 ‖0 ✓0 ✗0|"

// wantBar builds the canonical post-#515 bar for the given counts. It encodes the
// SPEC (pause glyph, exactly two spaces between entries) so a buggy implementation
// (1-space, 3-space, tab, wrong glyph) is caught.
func wantBar(running, waiting, completed, failed int) string {
	return fmt.Sprintf("|▶%d  ⏸%d  ✓%d  ✗%d|", running, waiting, completed, failed)
}

// =============================================================================
// Criterion (1): GOAL MATCH — glyph swap + 2-space spacing, no scope creep
// =============================================================================

// TestIssue515_StatusIconWaitingIsPauseGlyph: statusIcon(StatusWaiting) must
// return exactly the pause glyph ⏸ (U+23F8), the single source feeding both the
// summary bar and individual agent rows.
func TestIssue515_StatusIconWaitingIsPauseGlyph(t *testing.T) {
	got := statusIcon(agent.StatusWaiting)
	if got != pauseGlyph {
		t.Fatalf("statusIcon(StatusWaiting) = %q, want %q", got, pauseGlyph)
	}
}

// TestIssue515_StatusIconOtherGlyphsUnchanged: #515 swaps ONLY the waiting glyph.
// The other three lifecycle glyphs (▶ ✓ ✗) and the idle fallback (•) must be
// untouched (criterion 4: the other glyphs are out of scope).
func TestIssue515_StatusIconOtherGlyphsUnchanged(t *testing.T) {
	cases := []struct {
		status agent.AgentStatus
		want   string
	}{
		{agent.StatusRunning, "▶"},
		{agent.StatusCompleted, "✓"},
		{agent.StatusFailed, "✗"},
	}
	for _, tc := range cases {
		if got := statusIcon(tc.status); got != tc.want {
			t.Errorf("statusIcon(%v) = %q, want %q", tc.status, got, tc.want)
		}
	}
	// Idle / unknown fallback.
	if got := statusIcon(agent.StatusIdle); got != "•" {
		t.Errorf("statusIcon(StatusIdle) = %q, want •", got)
	}
}

// TestIssue515_StatusBarLabelExactForm pins the exact rendered bar for a matrix of
// counts: pause glyph, exactly TWO spaces between entries, pipe-wrapped. Hardcoded
// literals (not derived) so a format typo in either glyph or spacing is caught.
func TestIssue515_StatusBarLabelExactForm(t *testing.T) {
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
		{"mixed single-digit", 2, 1, 2, 1, "|▶2  ⏸1  ✓2  ✗1|"},
		{"all non-zero", 7, 8, 9, 4, "|▶7  ⏸8  ✓9  ✗4|"},
		{"multi-digit", 12, 345, 6, 7890, "|▶12  ⏸345  ✓6  ✗7890|"},
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

// TestIssue515_BarUsesPauseGlyphNotDoubleVerticalLine: the emitted bar must
// contain ⏸ and must NOT contain the retired ‖ (U+2016). Covers every state being
// non-zero so the waiting slot is exercised.
func TestIssue515_BarUsesPauseGlyphNotDoubleVerticalLine(t *testing.T) {
	for _, tc := range []struct{ r, w, c, f int }{
		{0, 0, 0, 0}, {0, 1, 0, 0}, {2, 1, 2, 1}, {0, 99, 0, 0},
	} {
		bar := statusBarLabel(tc.r, tc.w, tc.c, tc.f)
		if !strings.Contains(bar, pauseGlyph) {
			t.Errorf("bar %q must contain the pause glyph %q", bar, pauseGlyph)
		}
		if strings.Contains(bar, "‖") {
			t.Errorf("bar %q must NOT contain the retired ‖ (U+2016)", bar)
		}
	}
}

// TestIssue515_BarExactlyTwoSpaceGaps: the inter-entry separator is exactly TWO
// spaces — not one (the #510 form), not three, not a tab. This is the headline
// spacing change of the issue. Asserted structurally so it is independent of the
// exact counts.
func TestIssue515_BarExactlyTwoSpaceGaps(t *testing.T) {
	bar := statusBarLabel(1, 1, 1, 1) // |▶1  ⏸1  ✓1  ✗1|
	// Three gaps, each exactly two spaces between the count digit and next glyph.
	wantGaps := []string{"1  ⏸", "1  ✓", "1  ✗"}
	for _, g := range wantGaps {
		if !strings.Contains(bar, g) {
			t.Errorf("bar %q missing 2-space gap %q", bar, g)
		}
	}
	// Negative regressions: the old 1-space form and a 3-space form must be absent.
	bad := []string{"1 ⏸", "1 ✓", "1 ✗", "1   ⏸", "1   ✓", "1   ✗"}
	for _, b := range bad {
		if strings.Contains(bar, b) {
			t.Errorf("bar %q must NOT contain %q (wrong gap width)", bar, b)
		}
	}
	// No tabs anywhere.
	if strings.Contains(bar, "\t") {
		t.Errorf("bar %q must not contain a tab", bar)
	}
}

// TestIssue515_BarFixedOrder: the four segments appear in fixed order
// ▶ ⏸ ✓ ✗ regardless of which counts are non-zero (guards a re-ordering bug).
func TestIssue515_BarFixedOrder(t *testing.T) {
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

// TestIssue515_AgentRowUsesPauseGlyph: statusIcon drives individual sub-agent rows
// too (via agentLabel), so a waiting agent's row leads with ⏸, not ‖ — the
// "consistent everywhere" claim of the issue.
func TestIssue515_AgentRowUsesPauseGlyph(t *testing.T) {
	for _, kind := range []agent.SubAgentKind{agent.KindInteractive, agent.KindTool} {
		label := agentLabel("worker", agent.StatusWaiting, kind)
		if !strings.HasPrefix(label, pauseGlyph) {
			t.Errorf("agentLabel(waiting, kind=%v) = %q, must lead with %q", kind, label, pauseGlyph)
		}
		if strings.Contains(label, "‖") {
			t.Errorf("agentLabel(waiting, kind=%v) = %q, must not contain retired ‖", kind, label)
		}
	}
}

// =============================================================================
// Criterion (2): USABILITY — single fixed-width line, alignment, suffix
// =============================================================================

// TestIssue515_BarRendersUntruncatedAtMinSidebarWidth: the widened bar is 16 cells
// (was 13). At the sidebar minimum width it must still render fully — both framing
// pipes present, no ellipsis truncation. Resolves the design's width-budget risk.
func TestIssue515_BarRendersUntruncatedAtMinSidebarWidth(t *testing.T) {
	w := NewWorkbench([]*config.ModelConfig{{Name: "m", Model: "m"}})
	w.sidebar.addSession("s1", "S", false)
	w.sidebar.panel.SetBounds(tv.Rect{X: 0, Y: 0, W: minSidebarWidth, H: 8})
	w.desktop.Redraw()
	abs := w.sidebar.panel.AbsoluteBounds()

	// Reconstruct each rendered row from its cells.
	var summaryRow string
	for y := 0; y < abs.H; y++ {
		var b strings.Builder
		for x := 0; x < abs.W; x++ {
			ch := w.app.ReadCell(abs.X+x, abs.Y+y).Ch
			if ch == 0 {
				ch = ' '
			}
			b.WriteRune(ch)
		}
		if strings.Contains(b.String(), pauseGlyph) {
			summaryRow = b.String()
			break
		}
	}
	if summaryRow == "" {
		t.Fatalf("no summary bar rendered at minSidebarWidth=%d", minSidebarWidth)
	}
	if !strings.Contains(summaryRow, issue515AllZeroBar) {
		t.Fatalf("all-zero bar must render untruncated at minSidebarWidth=%d: row %q",
			minSidebarWidth, summaryRow)
	}
	if got := strings.Count(summaryRow, "|"); got < 2 {
		t.Fatalf("bar truncated at min width (lost a framing pipe; %d '|'): %q", got, summaryRow)
	}
	if strings.Contains(summaryRow, "…") {
		t.Fatalf("bar must not be ellipsis-truncated at min width: %q", summaryRow)
	}
}

// TestIssue515_LeadingPipeAlignedToSessionName: the leading '|' of the summary bar
// renders at the SAME column as the session name's first character (inherited
// invariant from #510; #515 must not regress it despite the +3-cell width).
func TestIssue515_LeadingPipeAlignedToSessionName(t *testing.T) {
	w := NewWorkbench([]*config.ModelConfig{{Name: "m", Model: "m"}})
	const title = "Session 1"
	w.sidebar.addSession("s1", title, false)

	rows := renderSidebar(w)
	sessionRow := rowContaining(rows, title)
	summaryRow := rowContaining(rows, issue515AllZeroBar)
	if sessionRow == "" || summaryRow == "" {
		t.Fatalf("session/summary row missing:\n%s", strings.Join(rows, "\n"))
	}
	nameCol := runeCol(sessionRow, title)
	pipeCol := summaryPipeCol(summaryRow)
	if pipeCol != nameCol {
		t.Fatalf("alignment broken: summary '|' at col %d, session name at col %d\nsession: %q\nsummary: %q",
			pipeCol, nameCol, sessionRow, summaryRow)
	}
}

// TestIssue515_SuffixAfterClosingPipe: the trailing +/- fold suffix still sits
// IMMEDIATELY after the closing '|'. The extra inter-entry spacing must not move
// it. Pin both the all-zero (no suffix) and the folded ("+") cases.
func TestIssue515_SuffixAfterClosingPipe(t *testing.T) {
	s, c := newFoldSidebar(t)
	s.addSession("s1", "Session 1", false)

	// Fresh: no children -> no suffix; label is exactly the bar.
	if got, want := summaryOf(s, "s1").Label, issue515AllZeroBar; got != want {
		t.Fatalf("fresh summary label = %q, want %q (no suffix)", got, want)
	}

	// Fold a completed agent so the summary gains children -> "+" suffix. The
	// folded agent still COUNTS in the bar (✓1); only the suffix changes.
	s.applySubAgent("s1", subEv("a1", "done", agent.StatusCompleted))
	c.add(5 * time.Second)
	s.tickFolds()

	label := summaryOf(s, "s1").Label
	bar := label[:strings.LastIndexByte(label, '|')+1]
	if bar != wantBar(0, 0, 1, 0) { // |▶0  ⏸0  ✓1  ✗0|
		t.Fatalf("bar portion = %q, want %q", bar, wantBar(0, 0, 1, 0))
	}
	if got := suffixAfterBar(label); got != "+" {
		t.Fatalf("suffix after fold = %q, want + (label %q)", got, label)
	}
}

// =============================================================================
// Criterion (3): NO REGRESSIONS — pipe sentinel, width, glyph integrity
// =============================================================================

// TestIssue515_PipeSentinelTwoPipesWithWaitingAgents: ⏸ (bytes E2 8F B8) contains
// no '|' (0x7C) byte, so a bar with waiting agents still has EXACTLY two '|'
// bytes (the framing pipes). This is the load-bearing invariant for
// syncFoldSuffixes' LastIndexByte(base,'|').
func TestIssue515_PipeSentinelTwoPipesWithWaitingAgents(t *testing.T) {
	// Byte-level: the glyph itself carries no pipe byte.
	if bytes := []byte(pauseGlyph); strings.ContainsRune(string(bytes), '|') {
		t.Fatalf("pause glyph bytes % X must not contain a '|' byte", bytes)
	}
	// Structural: bars with waiting agents have exactly two pipes, for many inputs.
	for _, args := range [][4]int{
		{0, 0, 0, 0}, {0, 1, 0, 0}, {0, 9, 0, 0}, {2, 1, 2, 1}, {0, 345, 0, 0},
	} {
		bar := statusBarLabel(args[0], args[1], args[2], args[3])
		if got := strings.Count(bar, "|"); got != 2 {
			t.Errorf("bar %q has %d '|' bytes, want exactly 2", bar, got)
		}
	}
}

// TestIssue515_SyncFoldSuffixesFindsClosingPipeWithPauseGlyph: with a waiting
// agent (bar contains a ⏸ segment), syncFoldSuffixes must re-derive the suffix
// against the closing '|' — NOT truncate the bar at the ⏸. End-to-end sentinel
// guard mirroring the #510 test, but asserting the post-#515 glyph/spacing.
func TestIssue515_SyncFoldSuffixesFindsClosingPipeWithPauseGlyph(t *testing.T) {
	s, c := newFoldSidebar(t)
	s.addSession("s1", "Session 1", false)
	// Two completed (fold -> summary gains children -> suffix) + a waiting agent
	// so the bar carries a ⏸ segment.
	s.applySubAgent("s1", subEv("a1", "d1", agent.StatusCompleted))
	s.applySubAgent("s1", subEv("a2", "d2", agent.StatusCompleted))
	s.applySubAgent("s1", subEv("a3", "w", agent.StatusWaiting))
	c.add(5 * time.Second)
	s.tickFolds()

	summary := summaryOf(s, "s1")
	// Flip Expanded without refreshFoldChrome so the label is stale, then reconcile.
	summary.Expanded = true
	s.syncFoldSuffixes()

	base := summary.Label[:strings.LastIndexByte(summary.Label, '|')+1]
	wantBar := wantBar(0, 1, 2, 0) // |▶0  ⏸1  ✓2  ✗0|
	if base != wantBar {
		t.Fatalf("syncFoldSuffixes corrupted the bar (⏸1 segment must be preserved): base %q, want %q",
			base, wantBar)
	}
	if got := suffixAfterBar(summary.Label); got != "-" {
		t.Fatalf("suffix after reconcile = %q, want -", got)
	}
	if got := strings.Count(base, "|"); got != 2 {
		t.Fatalf("bar base %q has %d '|' bytes, want 2", base, got)
	}
}

// TestIssue515_NoEmojiVariationSelector: the waiting glyph must be PLAIN ⏸ (text
// presentation, width-1) with NO variation selector appended. Appending U+FE0F
// (emoji VS) would request emoji presentation and render width-2 in some
// terminals, breaking alignment — the explicit WARNING in the issue. Pin the exact
// 3-byte UTF-8 encoding E2 8F B8.
func TestIssue515_NoEmojiVariationSelector(t *testing.T) {
	got := statusIcon(agent.StatusWaiting)
	if len(got) != 3 {
		t.Fatalf("statusIcon(StatusWaiting) = %d bytes % X, want exactly 3 (plain ⏸, no VS)",
			len(got), []byte(got))
	}
	wantBytes := []byte{0xE2, 0x8F, 0xB8}
	if !bytesEqual([]byte(got), wantBytes) {
		t.Fatalf("statusIcon(StatusWaiting) bytes = % X, want % X (plain ⏸ U+23F8)",
			[]byte(got), wantBytes)
	}
	// Defense in depth: neither variation selector may appear.
	for _, vs := range []rune{0xFE0E, 0xFE0F} {
		if i := strings.IndexRune(got, vs); i >= 0 {
			t.Errorf("statusIcon(StatusWaiting) %q contains variation selector U+%04X at %d", got, vs, i)
		}
	}
	// And the emitted bar must likewise carry no variation selector.
	if bar := statusBarLabel(0, 1, 0, 0); strings.ContainsRune(bar, 0xFE0F) || strings.ContainsRune(bar, 0xFE0E) {
		t.Errorf("bar %q must not contain a variation selector", bar)
	}
}

// TestIssue515_WidthDeltaIsThreeCells: the only width change is +3 cells from the
// three extra inter-entry spaces; the glyph swap is width-neutral (⏸ == ‖ == 1).
// Pin both the absolute width and the delta via turbotui's StringWidth (the same
// measurer the renderer uses).
func TestIssue515_WidthDeltaIsThreeCells(t *testing.T) {
	newW := tui.StringWidth(issue515AllZeroBar)
	oldW := tui.StringWidth(oldAllZeroBar)
	if newW != 16 {
		t.Errorf("StringWidth(new all-zero bar) = %d, want 16", newW)
	}
	if oldW != 13 {
		t.Errorf("StringWidth(old all-zero bar) = %d, want 13", oldW)
	}
	if newW-oldW != 3 {
		t.Errorf("width delta = %d, want exactly 3 (the three extra spaces)", newW-oldW)
	}
}

// TestIssue515_BarIsSingleFixedWidthLine: every bar renders to the same column
// width for given digit-counts (uniform spacing), so columns stay aligned across
// sessions — the "single fixed-width line" usability property. Width must equal
// the rune count (all glyphs are width-1).
func TestIssue515_BarIsSingleFixedWidthLine(t *testing.T) {
	for _, tc := range []struct{ r, w, c, f int }{
		{0, 0, 0, 0}, {1, 1, 1, 1}, {12, 345, 6, 7890},
	} {
		bar := statusBarLabel(tc.r, tc.w, tc.c, tc.f)
		runeCount := utf8RuneCount(bar)
		if tui.StringWidth(bar) != runeCount {
			t.Errorf("bar %q: StringWidth %d != rune count %d (a glyph is not width-1)",
				bar, tui.StringWidth(bar), runeCount)
		}
	}
}

// =============================================================================
// Criterion (4): HOLISTIC — cross-repo seam (gogent emits strings, turbotui
// measures via RuneWidth). The swap is safe only because ⏸ measures width-1.
// =============================================================================

// TestIssue515_PauseGlyphIsWidthOneInTurbotui: the entire design rests on
// RuneWidth('⏸') == RuneWidth('‖') == 1, so the fixed-width bar stays aligned with
// NO turbotui change. If a future turbotui width-table edit made ⏸ width-2, the
// sidebar columns would silently shift — this guard catches that cross-repo
// regression at test time.
func TestIssue515_PauseGlyphIsWidthOneInTurbotui(t *testing.T) {
	if got := tui.RuneWidth('⏸'); got != 1 {
		t.Errorf("RuneWidth('⏸') = %d, want 1 (bar alignment depends on this)", got)
	}
	if got := tui.RuneWidth('‖'); got != 1 {
		t.Errorf("RuneWidth('‖') = %d, want 1 (width-neutral swap baseline)", got)
	}
	// All five lifecycle glyphs the bar/rows use must be width-1.
	for _, r := range []rune{'▶', '⏸', '✓', '✗', '•'} {
		if w := tui.RuneWidth(r); w != 1 {
			t.Errorf("RuneWidth(%q) = %d, want 1", r, w)
		}
	}
}

// =============================================================================
// Integration: the always-on bar across the full sub-agent lifecycle (criterion 1
// + 3 together). Mirrors the #510 lifecycle test but pins the post-#515 glyph and
// spacing at every stage.
// =============================================================================

// TestIssue515_BarAcrossFullLifecycle: as running/waiting/completed/failed
// sub-agents arrive, the always-on summary bar updates to the exact post-#515 form
// at each stage, and the summary node is never recreated.
func TestIssue515_BarAcrossFullLifecycle(t *testing.T) {
	s, c := newFoldSidebar(t)
	s.addSession("s1", "Session 1", false)
	want := summaryOf(s, "s1")

	check := func(stage string, wantLabel string) {
		t.Helper()
		got := summaryOf(s, "s1")
		if got != want {
			t.Fatalf("%s: summary node pointer changed (node recreated)", stage)
		}
		if got.Label != wantLabel {
			t.Fatalf("%s: label = %q, want %q", stage, got.Label, wantLabel)
		}
	}

	check("fresh", issue515AllZeroBar)
	s.applySubAgent("s1", subEv("a1", "r", agent.StatusRunning))
	check("running", wantBar(1, 0, 0, 0))
	s.applySubAgent("s1", subEv("a2", "w", agent.StatusWaiting))
	check("waiting", wantBar(1, 1, 0, 0))
	s.applySubAgent("s1", subEv("a3", "d", agent.StatusCompleted))
	check("completed", wantBar(1, 1, 1, 0))
	s.applySubAgent("s1", subEv("a4", "f", agent.StatusFailed))
	check("failed", wantBar(1, 1, 1, 1))

	// Fold the completed agent: bar prefix unchanged, "+" suffix appended.
	c.add(5 * time.Second)
	s.tickFolds()
	if base := summaryOf(s, "s1").Label; !strings.HasPrefix(base, wantBar(1, 1, 1, 1)) {
		t.Fatalf("post-fold label %q must keep the bar prefix %q", base, wantBar(1, 1, 1, 1))
	}
}

// =============================================================================
// tiny local helpers (kept unexported, test-file scoped)
// =============================================================================

func bytesEqual(a, b []byte) bool {
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

func utf8RuneCount(s string) int { return len([]rune(s)) }
