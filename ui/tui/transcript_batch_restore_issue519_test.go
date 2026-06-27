package ui

import (
	"fmt"
	"strings"
	"testing"

	tv "github.com/hobbestherat/turbotui/turbotv"
)

// Tests for the batched first-connect transcript restore (issue #519).
//
// restore() now builds every record up front and composes the view exactly once
// via transcriptModel.addAll, instead of calling add() (→ renderOne) per message;
// reload() shares the same helper. These tests cover the four design criteria:
// goal (batched single compose), usability (identical final view), no regressions
// (filter/limit/trim/order/blank handling, reload==restore), and holistic (pure
// ui/tui, no turbotui change).
//
// Note on the "single render()" property: it is a *performance* characteristic
// with no black-box-observable final-state difference from the old per-record
// path — same records, order, fold state, AllText and scroll position. render()
// also Clear()s before rebuilding, so even a redundant render leaves no duplicate
// trace to count. The renderCount instrumentation the design named was not added
// to transcriptModel, and render's scroll/follow state is unexported in package
// tv, so a literal "render was called once" assertion is impossible from package
// ui without modifying an implementation file. These tests instead lock the
// observable invariants the single-render change must preserve (complete,
// consistent, duplicate-free compose; correct records/order/fold; correct trim and
// filtering; no nil records). A literal render-count test requires the driver to
// add the counter hook in transcriptModel.render.

// recLine renders a record's identity to a comparable string. recordSummary uses
// the exact same format, so expected lines can be built with explicit field
// values (kind via the named constant — never a hardcoded int) and compared.
func recLine(kind eventKind, header, body string, collapsed, rich bool) string {
	return fmt.Sprintf("kind=%d|hdr=%q|body=%q|collapsed=%v|rich=%v",
		kind, header, body, collapsed, rich)
}

func recordSummary(r *transcriptRecord) string {
	return recLine(r.kind, r.header, r.body(), r.collapsed, r.rich)
}

func recordSummaries(rs []*transcriptRecord) []string {
	out := make([]string, len(rs))
	for i, r := range rs {
		out[i] = recordSummary(r)
	}
	return out
}

// userMessages builds n user ChatMessages whose content uniquely identifies each,
// for trim/order assertions.
func userMessages(n int) []ChatMessage {
	msgs := make([]ChatMessage, n)
	for i := 0; i < n; i++ {
		msgs[i] = ChatMessage{Role: "user", Content: fmt.Sprintf("msg-%04d", i)}
	}
	return msgs
}

// ---------------------------------------------------------------------------
// addAll helper (transcript_model.go)
// ---------------------------------------------------------------------------

// TestAddAllAppendsAllAndRendersOnce verifies the new batching helper appends
// every record and leaves the whole batch rendered in a single compose.
func TestAddAllAppendsAllAndRendersOnce(t *testing.T) {
	m := newTranscriptModel(tv.NewTextView("", tv.Rect{}))
	m.addAll([]*transcriptRecord{
		userRecord("hello"),
		thoughtRecord("thinking"),
		assistantRecord("answer"),
	})
	if len(m.records) != 3 {
		t.Fatalf("len(records) = %d, want 3", len(m.records))
	}
	for i, r := range m.records {
		if r == nil {
			t.Fatalf("records[%d] is nil", i)
		}
		if r.entry == nil {
			t.Errorf("records[%d] (%q) not rendered (entry nil) — compose did not run", i, r.header)
		}
	}
	all := m.view.AllText()
	for _, want := range []string{"You:", "hello", "thought", "thinking", "Gogent:", "answer"} {
		if !strings.Contains(all, want) {
			t.Errorf("addAll view missing %q\n%s", want, all)
		}
	}
}

// TestAddAllSkipsNilEntries exercises the fail-safe nil backstop: even if a nil
// reaches addAll, it must never land in m.records (render dereferences every
// record).
func TestAddAllSkipsNilEntries(t *testing.T) {
	m := newTranscriptModel(tv.NewTextView("", tv.Rect{}))
	m.addAll([]*transcriptRecord{userRecord("a"), nil, thoughtRecord("b"), nil, nil})
	if len(m.records) != 2 {
		t.Fatalf("len(records) = %d, want 2 (nils skipped)", len(m.records))
	}
	for i, r := range m.records {
		if r == nil {
			t.Fatalf("records[%d] is nil — backstop failed", i)
		}
	}
}

func TestAddAllEmptyDoesNotPanic(t *testing.T) {
	m := newTranscriptModel(tv.NewTextView("", tv.Rect{}))
	m.addAll(nil)
	m.addAll([]*transcriptRecord{})
	if len(m.records) != 0 {
		t.Fatalf("len(records) = %d, want 0", len(m.records))
	}
}

// TestAddAllHonoursLimitZero confirms a zero limit disables trim entirely.
func TestAddAllHonoursLimitZero(t *testing.T) {
	m := newTranscriptModel(tv.NewTextView("", tv.Rect{}))
	m.limit = 0
	recs := make([]*transcriptRecord, 50)
	for i := range recs {
		recs[i] = userRecord(fmt.Sprintf("m-%d", i))
	}
	m.addAll(recs)
	if len(m.records) != 50 {
		t.Errorf("limit 0 should disable trim; len = %d, want 50", len(m.records))
	}
}

// TestAddAllTrimsWhenOverLimit locks the limit/trim contract: a single end-of-
// batch trim keeps limit-limit/10 newest (NOT the incremental path's keep..limit
// hover) — the deliberate retained-count shift documented in the design.
func TestAddAllTrimsWhenOverLimit(t *testing.T) {
	m := newTranscriptModel(tv.NewTextView("", tv.Rect{}))
	m.limit = 10
	const n = 25
	recs := make([]*transcriptRecord, n)
	for i := range recs {
		recs[i] = userRecord(fmt.Sprintf("msg-%04d", i))
	}
	m.addAll(recs)
	keep := m.limit - m.limit/10 // 9
	if len(m.records) != keep {
		t.Fatalf("len(records) = %d, want keep %d", len(m.records), keep)
	}
	all := m.view.AllText()
	if !strings.Contains(all, "msg-0024") {
		t.Errorf("newest dropped after trim\n%s", all)
	}
	if strings.Contains(all, "msg-0000") {
		t.Errorf("oldest retained after trim\n%s", all)
	}
}

// TestAddAllAppendsOntoExistingRecords confirms addAll works as a general helper
// (appends onto a non-empty model), not only on a fresh one.
func TestAddAllAppendsOntoExistingRecords(t *testing.T) {
	m := newTranscriptModel(tv.NewTextView("", tv.Rect{}))
	m.limit = 1000
	m.add(userRecord("pre-1"))
	m.add(userRecord("pre-2"))
	m.addAll([]*transcriptRecord{userRecord("batch-1"), assistantRecord("batch-2")})
	if len(m.records) != 4 {
		t.Fatalf("len = %d, want 4", len(m.records))
	}
	all := m.view.AllText()
	for _, w := range []string{"pre-1", "pre-2", "batch-1", "Gogent:", "batch-2"} {
		if !strings.Contains(all, w) {
			t.Errorf("missing %q\n%s", w, all)
		}
	}
}

// ---------------------------------------------------------------------------
// record builders (session_window.go)
// ---------------------------------------------------------------------------

func TestBuildersReturnNilForBlank(t *testing.T) {
	for _, in := range []string{"", "   ", "\t\n"} {
		if r := userRecord(in); r != nil {
			t.Errorf("userRecord(%q) = %v, want nil", in, r)
		}
		if r := thoughtRecord(in); r != nil {
			t.Errorf("thoughtRecord(%q) = %v, want nil", in, r)
		}
		if r := assistantRecord(in); r != nil {
			t.Errorf("assistantRecord(%q) = %v, want nil", in, r)
		}
	}
}

func TestBuildersShapeForNonBlank(t *testing.T) {
	if u := userRecord("hi"); u == nil || u.kind != kindUser || u.header != "You:" || u.collapsed || u.rich {
		t.Errorf("userRecord shape wrong: %+v", u)
	}
	if th := thoughtRecord("hmm"); th == nil || th.kind != kindThinking || th.header != "thought" || !th.collapsed || th.rich {
		t.Errorf("thoughtRecord shape wrong: %+v", th)
	}
	if a := assistantRecord("yo"); a == nil || a.kind != kindAssistant || a.header != "Gogent:" || !a.rich || a.collapsed {
		t.Errorf("assistantRecord shape wrong: %+v", a)
	}
}

// TestLiveAddPathsDropBlankText guards the new no-op blank guard on addUser
// (which had no guard before) and the preserved guards on addThought/addAssistant.
func TestLiveAddPathsDropBlankText(t *testing.T) {
	sw := newTestSession()
	sw.addUser("")
	sw.addUser("   ")
	sw.addAssistant("")
	sw.addThought("")
	if len(sw.transcript.records) != 0 {
		t.Fatalf("blank live adds produced %d records, want 0", len(sw.transcript.records))
	}
	sw.addUser("hi")
	sw.addAssistant("hello")
	sw.addThought("hmm")
	if len(sw.transcript.records) != 3 {
		t.Errorf("non-blank live adds: %d records, want 3", len(sw.transcript.records))
	}
}

// ---------------------------------------------------------------------------
// restore(): correctness, order, edge cases
// ---------------------------------------------------------------------------

// TestRestoreBuildsMixedRecordsInOrder is the core criterion-(1)/(2) test: a
// mixed restore produces exactly the right records, in order, with the right
// kind/header/fold/rich — i.e. the same final view, batched.
func TestRestoreBuildsMixedRecordsInOrder(t *testing.T) {
	sw := newTestSession()
	sw.restore([]ChatMessage{
		{Role: "user", Content: "u1"},
		{Role: "assistant", Reasoning: "why", Content: "a1", Tool: "Grep", Args: "pattern: x"},
		{Role: "tool", Tool: "Grep", Content: "hit"},
		{Role: "system", Content: "sys"},
	})
	got := recordSummaries(sw.transcript.records)
	want := []string{
		recLine(kindUser, "You:", "u1", false, false),
		recLine(kindThinking, "thought", "why", true, false),
		recLine(kindAssistant, "Gogent:", "a1", false, true),
		recLine(kindTool, "tool: Grep", "  pattern: x", true, false),
		recLine(kindTool, "result: Grep", "  hit", true, false),
		recLine(kindSystem, "[System]", "sys", true, false),
	}
	if len(got) != len(want) {
		t.Fatalf("restore produced %d records, want %d:\n%s", len(got), len(want), strings.Join(got, "\n"))
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("records[%d]:\n got %s\nwant %s", i, got[i], want[i])
		}
	}
}

// TestRestoreRendersAllRecordsInOneCompose locks the observable consequence of
// the single compose: every record has a live entry, and each header appears
// exactly once (no duplicate/leftover compose artifacts).
func TestRestoreRendersAllRecordsInOneCompose(t *testing.T) {
	sw := newTestSession()
	sw.restore([]ChatMessage{
		{Role: "user", Content: "u1"},
		{Role: "assistant", Reasoning: "r1", Content: "a1"},
		{Role: "system", Content: "s1"},
	})
	m := sw.transcript
	for i, r := range m.records {
		if r.entry == nil {
			t.Errorf("record %d (%q) has no live entry after restore", i, r.header)
		}
	}
	all := m.view.AllText()
	for _, hdr := range []string{"You:", "thought", "Gogent:", "[System]"} {
		if c := strings.Count(all, hdr); c != 1 {
			t.Errorf("header %q appears %d times, want 1 (duplicate compose?)\n%s", hdr, c, all)
		}
	}
}

// TestRestoreLargeBatchOrderAndCount is a normal-case scale test: an interleaved
// user/assistant restore (under the limit, so no trim) preserves count and order.
func TestRestoreLargeBatchOrderAndCount(t *testing.T) {
	const pairs = 500
	msgs := make([]ChatMessage, 0, 2*pairs)
	for i := 0; i < pairs; i++ {
		msgs = append(msgs,
			ChatMessage{Role: "user", Content: fmt.Sprintf("u-%d", i)},
			ChatMessage{Role: "assistant", Content: fmt.Sprintf("a-%d", i)})
	}
	sw := newTestSession()
	sw.restore(msgs)
	if len(sw.transcript.records) != 2*pairs {
		t.Fatalf("len = %d, want %d", len(sw.transcript.records), 2*pairs)
	}
	for i := 0; i < pairs; i++ {
		if sw.transcript.records[2*i].kind != kindUser || sw.transcript.records[2*i+1].kind != kindAssistant {
			t.Errorf("pair %d order broken", i)
			break
		}
	}
}

func TestRestoreDropsAllBlankRecords(t *testing.T) {
	sw := newTestSession()
	sw.restore([]ChatMessage{
		{Role: "user", Content: "   "},
		{Role: "assistant", Content: "", Reasoning: ""},
		{Role: "system", Content: "\t"},
	})
	if len(sw.transcript.records) != 0 {
		t.Fatalf("blank restore produced %d records, want 0", len(sw.transcript.records))
	}
}

// TestRestoreReasoningOnlyAssistant (issue #402) must still render a single
// collapsed thought when content is blank but reasoning is present.
func TestRestoreReasoningOnlyAssistant(t *testing.T) {
	sw := newTestSession()
	sw.restore([]ChatMessage{{Role: "assistant", Reasoning: "only thought"}})
	if len(sw.transcript.records) != 1 {
		t.Fatalf("records = %d, want 1", len(sw.transcript.records))
	}
	r := sw.transcript.records[0]
	if r.kind != kindThinking || !r.collapsed || r.body() != "only thought" {
		t.Errorf("reasoning-only record wrong: %+v", r)
	}
}

func TestRestoreContentOnlyAssistant(t *testing.T) {
	sw := newTestSession()
	sw.restore([]ChatMessage{{Role: "assistant", Content: "just answer"}})
	if len(sw.transcript.records) != 1 || sw.transcript.records[0].kind != kindAssistant {
		t.Fatalf("want one assistant record, got %+v", sw.transcript.records)
	}
}

func TestRestoreRoleCaseInsensitive(t *testing.T) {
	sw := newTestSession()
	sw.restore([]ChatMessage{
		{Role: "USER", Content: "u"},
		{Role: "Assistant", Content: "a"},
	})
	if len(sw.transcript.records) != 2 {
		t.Fatalf("records = %d, want 2", len(sw.transcript.records))
	}
	if sw.transcript.records[0].kind != kindUser || sw.transcript.records[1].kind != kindAssistant {
		t.Errorf("case-insensitive roles misrouted")
	}
}

func TestRestoreUnknownRoleBecomesSystem(t *testing.T) {
	sw := newTestSession()
	sw.restore([]ChatMessage{{Role: "weird", Content: "x"}})
	if len(sw.transcript.records) != 1 || sw.transcript.records[0].kind != kindSystem {
		t.Fatalf("unknown role should map to one system record, got %+v", sw.transcript.records)
	}
}

// TestRestoreToolCallBlankArgsNoPanic guards childLines("") returning [""] so a
// tool call with empty args builds a record without panicking.
func TestRestoreToolCallBlankArgsNoPanic(t *testing.T) {
	sw := newTestSession()
	sw.restore([]ChatMessage{{Role: "assistant", Content: "a", Tool: "Read", Args: ""}})
	if len(sw.transcript.records) != 2 {
		t.Fatalf("records = %d, want 2 (answer + tool call)", len(sw.transcript.records))
	}
	if sw.transcript.records[1].header != "tool: Read" {
		t.Errorf("tool header = %q", sw.transcript.records[1].header)
	}
}

func TestRestoreNilAndEmptyMessages(t *testing.T) {
	sw := newTestSession()
	sw.restore(nil)
	sw.restore([]ChatMessage{})
	if len(sw.transcript.records) != 0 {
		t.Errorf("nil/empty restore should add no records, got %d", len(sw.transcript.records))
	}
}

// TestRestoreOverDefaultLimitTrimsToKeep is the >1000 case: restoring 1500
// records trims once to keep = limit-limit/10 (900), retaining the newest tail.
func TestRestoreOverDefaultLimitTrimsToKeep(t *testing.T) {
	sw := newTestSession()
	sw.restore(userMessages(1500))
	keep := defaultTranscriptLimit - defaultTranscriptLimit/10 // 900
	if len(sw.transcript.records) != keep {
		t.Fatalf("len(records) = %d, want keep %d", len(sw.transcript.records), keep)
	}
	all := sw.transcript.view.AllText()
	if !strings.Contains(all, "msg-1499") {
		t.Errorf("newest record dropped after trim")
	}
	if strings.Contains(all, "msg-0000") {
		t.Errorf("oldest record retained after trim")
	}
	// Retained tail = last 900 of 1500 → indices 600..1499.
	if got := sw.transcript.records[0].body(); got != "msg-0600" {
		t.Errorf("first retained record = %q, want msg-0600", got)
	}
}

// ---------------------------------------------------------------------------
// filtering during restore
// ---------------------------------------------------------------------------

// TestRestoreUnderActiveFilter: with a query set before restore, addAll's single
// render() must produce the correct filtered view and match count in one pass.
func TestRestoreUnderActiveFilter(t *testing.T) {
	sw := newTestSession()
	sw.transcript.setQuery("grep")
	sw.restore([]ChatMessage{
		{Role: "user", Content: "hello"},
		{Role: "assistant", Content: "grep results here"},
		{Role: "user", Content: "world"},
	})
	m := sw.transcript
	if m.matchCount() != 1 {
		t.Fatalf("matchCount = %d, want 1", m.matchCount())
	}
	all := m.view.AllText()
	if !strings.Contains(all, "grep results here") {
		t.Errorf("filtered view missing the match\n%s", all)
	}
	for _, leak := range []string{"hello", "world"} {
		if strings.Contains(all, leak) {
			t.Errorf("non-match %q leaked into filtered view\n%s", leak, all)
		}
	}
	if !strings.Contains(all, `find "grep": 1`) {
		t.Errorf("filter note missing/wrong\n%s", all)
	}
}

// TestRestoreThenFilterStillWorks confirms post-restore search/filter behave
// identically to a live transcript (the restored model is fully indexed).
func TestRestoreThenFilterStillWorks(t *testing.T) {
	sw := newTestSession()
	sw.restore([]ChatMessage{
		{Role: "user", Content: "find me"},
		{Role: "assistant", Content: "ignore"},
	})
	sw.transcript.setQuery("find")
	if sw.transcript.matchCount() != 1 {
		t.Errorf("post-restore matchCount = %d, want 1", sw.transcript.matchCount())
	}
	if !strings.Contains(sw.transcript.view.AllText(), "find me") {
		t.Errorf("post-restore filter lost the match")
	}
}

// ---------------------------------------------------------------------------
// reload(): shares addAll, single render, clears old records, preserves filter
// ---------------------------------------------------------------------------

// TestReloadMatchesRestore is the criterion test restore()==reload(): the same
// messages through reload (after prior state) and restore (fresh) yield an
// identical record set in order.
func TestReloadMatchesRestore(t *testing.T) {
	msgs := []ChatMessage{
		{Role: "user", Content: "u1"},
		{Role: "assistant", Reasoning: "r", Content: "a1", Tool: "T", Args: "x: 1"},
		{Role: "tool", Tool: "T", Content: "res"},
		{Role: "system", Content: "s"},
	}
	fresh := newTestSession()
	fresh.restore(msgs)

	reloaded := newTestSession()
	reloaded.addUser("preexisting") // prior state reload must discard
	reloaded.reload(msgs)

	got := recordSummaries(reloaded.transcript.records)
	want := recordSummaries(fresh.transcript.records)
	if len(got) != len(want) {
		t.Fatalf("reload %d records != restore %d", len(got), len(want))
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("records[%d]:\n reload %s\n restore %s", i, got[i], want[i])
		}
	}
}

func TestReloadDiscardsOldRecords(t *testing.T) {
	sw := newTestSession()
	sw.restore([]ChatMessage{{Role: "user", Content: "old"}})
	sw.reload([]ChatMessage{{Role: "user", Content: "new"}})
	if len(sw.transcript.records) != 1 {
		t.Fatalf("len = %d, want 1", len(sw.transcript.records))
	}
	all := sw.transcript.view.AllText()
	if strings.Contains(all, "old") {
		t.Errorf("old record survived reload\n%s", all)
	}
	if !strings.Contains(all, "new") {
		t.Errorf("new record missing after reload\n%s", all)
	}
}

// TestReloadLeavesAllRecordsRendered confirms reload (which used to clear-render
// + per-record + final render) now ends in a single consistent render.
func TestReloadLeavesAllRecordsRendered(t *testing.T) {
	sw := newTestSession()
	sw.reload([]ChatMessage{
		{Role: "user", Content: "u"},
		{Role: "assistant", Content: "a"},
	})
	for i, r := range sw.transcript.records {
		if r.entry == nil {
			t.Errorf("record %d (%q) not rendered after reload", i, r.header)
		}
	}
}

// TestReloadPreservesActiveFilter: a search active before a reconnect reload
// must remain active and applied after reload (reload must not reset query/hidden).
func TestReloadPreservesActiveFilter(t *testing.T) {
	sw := newTestSession()
	sw.restore([]ChatMessage{
		{Role: "user", Content: "alpha"},
		{Role: "user", Content: "beta"},
	})
	sw.transcript.setQuery("alpha")
	if !sw.transcript.filtering() {
		t.Fatal("filter should be active before reload")
	}
	sw.reload([]ChatMessage{
		{Role: "user", Content: "alpha-two"},
		{Role: "user", Content: "beta-two"},
	})
	m := sw.transcript
	if !m.filtering() {
		t.Fatal("filter should remain active after reload")
	}
	if m.matchCount() != 1 {
		t.Errorf("matchCount = %d, want 1 (alpha-two)", m.matchCount())
	}
	all := m.view.AllText()
	if !strings.Contains(all, "alpha-two") || strings.Contains(all, "beta-two") {
		t.Errorf("reload under filter produced wrong view\n%s", all)
	}
}

// ---------------------------------------------------------------------------
// identical final view: batch restore vs per-record live path
// ---------------------------------------------------------------------------

// TestRestoreBatchMatchesPerRecordLiveView is the criterion-(2) lock for the
// shared-builder record types (user/thought/assistant): rebuilding the same
// messages through restore (batch) and through the live per-record add path
// (the old behaviour) must yield a byte-identical view.
func TestRestoreBatchMatchesPerRecordLiveView(t *testing.T) {
	msgs := []ChatMessage{
		{Role: "user", Content: "u1"},
		{Role: "assistant", Reasoning: "thinking it over", Content: "a1"},
		{Role: "user", Content: "u2"},
	}
	batch := newTestSession()
	batch.restore(msgs)

	live := newTestSession()
	for _, m := range msgs {
		switch strings.ToLower(m.Role) {
		case "user":
			live.addUser(m.Content)
		case "assistant":
			live.addThought(m.Reasoning)
			live.addAssistant(m.Content)
		}
	}

	batchText := batch.transcript.view.AllText()
	liveText := live.transcript.view.AllText()
	if batchText != liveText {
		t.Errorf("batch and live views differ\nbatch:\n%s\nlive:\n%s", batchText, liveText)
	}
	batchSum := recordSummaries(batch.transcript.records)
	liveSum := recordSummaries(live.transcript.records)
	if len(batchSum) != len(liveSum) {
		t.Fatalf("record counts differ: batch %d vs live %d", len(batchSum), len(liveSum))
	}
	for i := range batchSum {
		if batchSum[i] != liveSum[i] {
			t.Errorf("record %d differs:\n batch %s\n live  %s", i, batchSum[i], liveSum[i])
		}
	}
}
