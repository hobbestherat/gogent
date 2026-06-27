package ui

import (
	"bytes"
	"log"
	"strings"
	"testing"

	"gogent/internal/config"
	"gogent/internal/gogent"
)

// Tests for the duplicate-window guard (issue #518).
//
// Workbench keeps a 1:1 map w.sessions[id] -> *SessionWindow. Before the fix,
// AdoptSession, the startup restore loop and openWindowAny all created a window
// for an id without checking whether one already existed, so a second call for an
// already-open id overwrote the map entry and orphaned the first on-screen window
// (its layer stayed on the desktop with nothing pointing at it) -> split-brain.
// openWatcherSession already had the correct guard (check under w.mu; if open,
// Focus + return); this fix mirrors it at the three unguarded sites.
//
// These tests cover the four design criteria:
//   (1) GOAL MATCH — creating a window for an already-open id is a no-op-or-raise,
//       never a second window, at AdoptSession (A), the startup loop (B) and
//       openWindowAny (C).
//   (2) USABILITY — no split-brain; an already-open session is raised (Focus), and
//       the right thing is surfaced (the openWindowAny collision logs a tripwire
//       rather than failing silently).
//   (3) NO REGRESSIONS — single-open flow unchanged; the dedup branch must NOT
//       reload (a stale saved-file snapshot from the "Continue" path must not
//       clobber a live transcript), must NOT re-attach the backend observer
//       (OnCreate once), must NOT clobber a live model selection, and must still
//       advance nextNum past a restored id.
//   (4) HOLISTIC — pure ui/tui; no turbotui change.
//
// Orphaned-layer detection: Desktop.layers / layerSnapshot() are unexported in
// package turbotv, so a gogent test cannot enumerate desktop layers directly.
// Instead we assert the invariants that imply "no second AddLayer": in
// openWindowAny the map write (w.sessions[id]=sw), the w.order append and the
// AddLayer are all in the same post-collision-check block, so an unchanged
// w.sessions[id] pointer AND an unchanged w.order (no duplicate id) together prove
// none of them ran. This mirrors the assertion style of close_session_flash_test.

// dupGuardModels is the two-model config used by tests that exercise model
// preselection: index 0 = "main", index 1 = "alt" (DisplayName "Alt Model").
func dupGuardModels() []*config.ModelConfig {
	return []*config.ModelConfig{
		{Name: "main", DisplayName: "Main", Model: "m1"},
		{Name: "alt", DisplayName: "Alt Model", Model: "m2"},
	}
}

// countOrder returns how many times id appears in w.order (1 is the healthy
// invariant; 2+ means a duplicate registration slipped through — the orphan bug).
func countOrder(w *Workbench, id string) int {
	n := 0
	for _, o := range w.order {
		if o == id {
			n++
		}
	}
	return n
}

// rsMsg builds a RestoredSession carrying a single user message, for transcript
// assertions.
func rsMsg(id, body string) RestoredSession {
	return RestoredSession{
		ID:       id,
		Title:    id,
		Messages: []ChatMessage{{Role: "user", Content: body}},
	}
}

// captureLog redirects the standard logger to a buffer and returns it with a
// restore func. log.SetOutput returns no value, so the original writer is read via
// log.Default().Writer() (Go 1.16+). The restore runs via the returned closure.
func captureLog(t *testing.T) (*bytes.Buffer, func()) {
	t.Helper()
	orig := log.Default().Writer()
	var buf bytes.Buffer
	log.SetOutput(&buf)
	return &buf, func() { log.SetOutput(orig) }
}

// ---------------------------------------------------------------------------
// A. AdoptSession dedup guard
// ---------------------------------------------------------------------------

// TestAdoptSessionFirstOpenPipelineUnchanged pins criterion (3): the normal
// single-open path is untouched — one adopt opens a window, restores the
// transcript, preselects the recorded model, fires OnCreate exactly once and puts
// the window on screen (top of the z-stack). This is the baseline the dedup tests
// diverge from.
func TestAdoptSessionFirstOpenPipelineUnchanged(t *testing.T) {
	w := NewWorkbench(dupGuardModels())
	var onCreate int
	w.SetHandlers(Handlers{OnCreate: func(string, string) { onCreate++ }})

	sw := w.AdoptSession(RestoredSession{ID: "s1", Title: "S1", Model: "alt",
		Messages: []ChatMessage{{Role: "user", Content: "hello"}}})

	if sw == nil {
		t.Fatal("first adopt returned nil window")
	}
	if got := len(w.SessionIDs()); got != 1 {
		t.Errorf("SessionIDs = %d, want 1 after first open", got)
	}
	if w.sessions["s1"] != sw {
		t.Error("w.sessions[s1] is not the returned window (map not registered)")
	}
	if countOrder(w, "s1") != 1 {
		t.Errorf("s1 appears %d times in w.order, want 1", countOrder(w, "s1"))
	}
	if onCreate != 1 {
		t.Errorf("OnCreate fired %d times on first open, want 1", onCreate)
	}
	if sw.modelSelect == nil || sw.modelSelect.Value() != "Alt Model" {
		t.Errorf("model = %q, want preselected %q", sw.modelSelect.Value(), "Alt Model")
	}
	if !strings.Contains(sw.transcript.view.AllText(), "hello") {
		t.Error("first-open transcript did not restore the message")
	}
	if w.desktop.TopLayer() != sw.layer {
		t.Error("first-open window is not the top desktop layer")
	}
}

// TestAdoptSessionDuplicateReturnsSameWindow is the core criterion-(1) test:
// adopting the same id twice yields exactly one window and the second call returns
// the SAME *SessionWindow (satisfying callers that use the result), never a second.
func TestAdoptSessionDuplicateReturnsSameWindow(t *testing.T) {
	w := NewWorkbench(dupGuardModels())

	first := w.AdoptSession(rsMsg("s1", "first"))
	second := w.AdoptSession(rsMsg("s1", "second"))

	if first == nil || second == nil {
		t.Fatal("nil window returned")
	}
	if first != second {
		t.Errorf("second adopt returned a different window (%p != %p) — orphan/split-brain", second, first)
	}
	if got := len(w.SessionIDs()); got != 1 {
		t.Errorf("SessionIDs = %d, want 1 (no second window)", got)
	}
	if len(w.sessions) != 1 {
		t.Errorf("w.sessions has %d entries, want 1 (map must not hold a phantom)", len(w.sessions))
	}
	if countOrder(w, "s1") != 1 {
		t.Errorf("s1 appears %d times in w.order, want 1 (no duplicate registration)", countOrder(w, "s1"))
	}
	if w.sessions["s1"] != first {
		t.Error("w.sessions[s1] was overwritten by the duplicate adopt")
	}
}

// TestAdoptSessionDuplicatePreservesLiveTranscript is the criterion-(3) regression
// guard: the dedup branch must NOT reload rs.Messages. AdoptSession is also reached
// from the Saved Sessions "Continue" button, whose rs.Messages is a (possibly stale)
// file read; reloading it onto a live window the user is typing into would clobber
// recent messages. The fix Focuses instead. So a second adopt with different
// messages must leave the live transcript intact and never apply the second batch.
func TestAdoptSessionDuplicatePreservesLiveTranscript(t *testing.T) {
	w := NewWorkbench(dupGuardModels())

	live := w.AdoptSession(RestoredSession{ID: "s1",
		Messages: []ChatMessage{{Role: "user", Content: "live-marker-123"}}})
	// Snapshot the record set the first-open produced (it includes the window's
	// initial "[System] … ready" record plus the restored message). The dedup
	// branch must leave this set byte-for-byte untouched.
	recordsAfterFirst := len(live.transcript.records)

	// Re-adopt with a DIFFERENT (stale-snapshot-shaped) message set.
	reloaded := w.AdoptSession(RestoredSession{ID: "s1",
		Messages: []ChatMessage{{Role: "user", Content: "stale-marker-456"}}})

	if live != reloaded {
		t.Fatal("dedup returned a different window")
	}
	all := live.transcript.view.AllText()
	if !strings.Contains(all, "live-marker-123") {
		t.Errorf("live transcript lost on duplicate adopt — reload clobbered it:\n%s", all)
	}
	if strings.Contains(all, "stale-marker-456") {
		t.Errorf("dedup applied the stale snapshot (reload happened); transcript must be untouched:\n%s", all)
	}
	if n := len(live.transcript.records); n != recordsAfterFirst {
		t.Errorf("record count = %d after dedup, want %d (dedup must not append/replace records)",
			n, recordsAfterFirst)
	}
	for i, r := range live.transcript.records {
		if r.body() == "stale-marker-456" {
			t.Errorf("record[%d] holds the stale snapshot body — dedup reloaded", i)
		}
	}
}

// TestAdoptSessionDuplicateRaisesExistingWindow is the criterion-(2) usability
// test: re-adopting an already-open session raises it to the top of the z-stack
// (mirrors openWatcherSession's already-open branch). With two windows open and s2
// on top, re-adopting s1 must bring s1 to the top so the user sees it.
func TestAdoptSessionDuplicateRaisesExistingWindow(t *testing.T) {
	w := NewWorkbench(dupGuardModels())
	w.AdoptSession(rsMsg("s1", "one"))
	w.AdoptSession(rsMsg("s2", "two"))

	if w.desktop.TopLayer() != w.sessions["s2"].layer {
		t.Fatalf("precondition failed: s2 should be on top before re-adopting s1")
	}

	// Re-adopt s1 (already open) -> dedup branch Focuses it to the top.
	w.AdoptSession(rsMsg("s1", "one-again"))

	if w.desktop.TopLayer() != w.sessions["s1"].layer {
		t.Errorf("dedup did not raise s1; top layer is not s1's window")
	}
	if got := len(w.SessionIDs()); got != 2 {
		t.Errorf("SessionIDs = %d, want 2 (raise must not create a window)", got)
	}
}

// TestAdoptSessionDuplicateDoesNotReInvokeOnCreate guards criterion (3): the dedup
// branch must skip OnCreate, which attaches the live backend observer. Re-invoking
// it would double-attach the observer. Two adopts of the same id => OnCreate once.
func TestAdoptSessionDuplicateDoesNotReInvokeOnCreate(t *testing.T) {
	w := NewWorkbench(dupGuardModels())
	var onCreate int
	w.SetHandlers(Handlers{OnCreate: func(string, string) { onCreate++ }})

	w.AdoptSession(rsMsg("s1", "a"))
	w.AdoptSession(rsMsg("s1", "b"))
	w.AdoptSession(rsMsg("s1", "c"))

	if onCreate != 1 {
		t.Errorf("OnCreate fired %d times for 3 adopts of one id, want 1 (no double observer attach)", onCreate)
	}
}

// TestAdoptSessionDuplicateDoesNotClobberModelSelection guards criterion (3): the
// dedup branch must skip model-preselect. The open window already reflects the
// user's live dropdown choice; re-seeding from rs.Model would clobber it. Adopting
// with a different Model on the dedup path must leave the selection unchanged.
func TestAdoptSessionDuplicateDoesNotClobberModelSelection(t *testing.T) {
	w := NewWorkbench(dupGuardModels())

	sw := w.AdoptSession(RestoredSession{ID: "s1", Model: "alt"})
	if sw.modelSelect.Value() != "Alt Model" {
		t.Fatalf("precondition: first open should preselect alt, got %q", sw.modelSelect.Value())
	}

	// Re-adopt requesting a different model — must not clobber the live selection.
	w.AdoptSession(RestoredSession{ID: "s1", Model: "main"})

	if sw.modelSelect.Value() != "Alt Model" {
		t.Errorf("dedup clobbered model selection: got %q, want %q (live choice must win)",
			sw.modelSelect.Value(), "Alt Model")
	}
}

// TestAdoptSessionDuplicateAdvancesNextNum guards criterion (3): the nextNum bump
// runs before the dedup check, so a restored id's counter is advanced even when
// that id is already open. Otherwise a later NewSession could mint a colliding id.
func TestAdoptSessionDuplicateAdvancesNextNum(t *testing.T) {
	w := NewWorkbench(dupGuardModels())

	w.AdoptSession(RestoredSession{ID: "session-99"})
	w.AdoptSession(RestoredSession{ID: "session-99"}) // dedup path

	w.mu.Lock()
	got := w.nextNum
	w.mu.Unlock()
	if got < 99 {
		t.Errorf("nextNum = %d, want >= 99 (restored id must advance the counter even on the dedup path)", got)
	}
}

// TestAdoptSessionDistinctIdsOpenSeparateWindows pins criterion (3): the normal
// multi-open flow is unchanged — distinct ids each get their own window.
func TestAdoptSessionDistinctIdsOpenSeparateWindows(t *testing.T) {
	w := NewWorkbench(dupGuardModels())
	a := w.AdoptSession(rsMsg("s1", "a"))
	b := w.AdoptSession(rsMsg("s2", "b"))

	if a == b {
		t.Error("distinct ids returned the same window")
	}
	if got := len(w.SessionIDs()); got != 2 {
		t.Errorf("SessionIDs = %d, want 2", got)
	}
	if countOrder(w, "s1") != 1 || countOrder(w, "s2") != 1 {
		t.Errorf("w.order counts s1=%d s2=%d, want 1 each", countOrder(w, "s1"), countOrder(w, "s2"))
	}
}

// TestAdoptSessionDuplicateEmptyMessagesNoPanic is an edge-case guard: re-adopting
// with a nil/empty message slice must not panic and must still be a no-op-or-raise.
func TestAdoptSessionDuplicateEmptyMessagesNoPanic(t *testing.T) {
	w := NewWorkbench(dupGuardModels())
	w.AdoptSession(RestoredSession{ID: "s1"})
	sw := w.AdoptSession(RestoredSession{ID: "s1"}) // dedup, nil messages
	if sw == nil {
		t.Fatal("dedup with nil messages returned nil")
	}
	if got := len(w.SessionIDs()); got != 1 {
		t.Errorf("SessionIDs = %d, want 1", got)
	}
}

// ---------------------------------------------------------------------------
// B. Startup restore loop `seen` skip
// ---------------------------------------------------------------------------

// TestStartupRestoreLoopSkipsDuplicateId reproduces the guarded loop body (Run()
// blocks headless, so the loop is exercised directly) and asserts the acceptance
// criterion: a duplicate id in the restored list yields exactly one window.
// AdoptSession (A) is itself idempotent, so B is defense-in-depth, but the loop's
// end-state invariant — one window per id — is what this pins.
func TestStartupRestoreLoopSkipsDuplicateId(t *testing.T) {
	w := NewWorkbench(dupGuardModels())

	restored := []RestoredSession{
		rsMsg("dup", "first"),
		rsMsg("dup", "second"), // same id, second occurrence
		rsMsg("other", "x"),
	}
	layout := gogent.Layout{}

	// Exact reproduction of Run()'s guarded startup loop (tui.go).
	seen := make(map[string]bool, len(restored))
	for _, rs := range orderByLayout(restored, layout) {
		if seen[rs.ID] {
			continue
		}
		seen[rs.ID] = true
		w.AdoptSession(rs)
	}

	if got := len(w.SessionIDs()); got != 2 {
		t.Errorf("SessionIDs = %d, want 2 (dup id collapsed to one window, plus other)", got)
	}
	if countOrder(w, "dup") != 1 {
		t.Errorf("dup id appears %d times in w.order, want 1", countOrder(w, "dup"))
	}
	if w.sessions["dup"] == nil {
		t.Error("dup id has no window")
	}
}

// ---------------------------------------------------------------------------
// C. openWindowAny collision tripwire
// ---------------------------------------------------------------------------

// TestOpenWindowAnyCollisionReturnsExistingAndLogs is the criterion-(1)/(2) test
// for the last line of defense: a second openWindowAny for an existing id returns
// the SAME window, does NOT overwrite w.sessions[id] (so the first window is never
// orphaned), does NOT append a duplicate w.order entry (so no second AddLayer),
// and logs the tripwire rather than failing silently.
func TestOpenWindowAnyCollisionReturnsExistingAndLogs(t *testing.T) {
	w := NewWorkbench(dupGuardModels())

	first := w.openWindowAny("sess-x", "X", false)
	orderLenAfterFirst := len(w.order)

	// Capture the standard logger around the collision call.
	buf, restore := captureLog(t)
	defer restore()

	second := w.openWindowAny("sess-x", "X", false)

	if first != second {
		t.Errorf("collision returned a different window (%p != %p) — caller contract broken", second, first)
	}
	if w.sessions["sess-x"] != first {
		t.Error("w.sessions[sess-x] was overwritten on collision — first window orphaned")
	}
	if got := len(w.order); got != orderLenAfterFirst {
		t.Errorf("w.order grew from %d to %d on collision — duplicate registration / second AddLayer", orderLenAfterFirst, got)
	}
	if countOrder(w, "sess-x") != 1 {
		t.Errorf("sess-x appears %d times in w.order, want 1", countOrder(w, "sess-x"))
	}

	logged := buf.String()
	if !strings.Contains(logged, "sess-x") || !strings.Contains(logged, "duplicate-open guard") {
		t.Errorf("collision tripwire not surfaced in log:\n%s", logged)
	}
}

// TestOpenWindowAnyFirstOpenDoesNotLogTripwire confirms the tripwire fires ONLY on
// a real collision, never on the normal first open (so the log is a trustworthy
// signal of a programming error, not noise).
func TestOpenWindowAnyFirstOpenDoesNotLogTripwire(t *testing.T) {
	w := NewWorkbench(dupGuardModels())

	buf, restore := captureLog(t)
	defer restore()

	w.openWindowAny("fresh-1", "F1", false)

	if strings.Contains(buf.String(), "duplicate-open guard") {
		t.Errorf("first open logged the collision tripwire (should be silent):\n%s", buf.String())
	}
}

// TestOpenWindowAnyCollisionReadOnlyAnalysis covers the readOnly (analysis) branch
// of C: OpenAnalysisSession opens analysis-N windows; a synthetic second open for
// the same analysis id is also guarded (same window returned, no orphan).
func TestOpenWindowAnyCollisionReadOnlyAnalysis(t *testing.T) {
	w := NewWorkbench(dupGuardModels())

	first := w.openWindowAny("analysis-1", "A", true)
	orderAfterFirst := len(w.order)

	buf, restore := captureLog(t)
	defer restore()

	second := w.openWindowAny("analysis-1", "A", true)

	if first != second {
		t.Error("readOnly collision returned a different window")
	}
	if w.sessions["analysis-1"] != first {
		t.Error("readOnly window overwritten on collision")
	}
	if got := len(w.order); got != orderAfterFirst {
		t.Errorf("w.order grew on readOnly collision: %d -> %d", orderAfterFirst, got)
	}
	if !strings.Contains(buf.String(), "duplicate-open guard") {
		t.Errorf("readOnly collision did not log the tripwire:\n%s", buf.String())
	}
}

// TestOpenWindowAnyCollisionLeavesTopWindowIntact is an orphan-proxy check: a
// collision call must not AddLayer a phantom window on top of the existing one.
// With another window raised on top of sess-x, the collision on sess-x must leave
// that top window exactly where it was (no phantom layer inserted above it).
func TestOpenWindowAnyCollisionLeavesTopWindowIntact(t *testing.T) {
	w := NewWorkbench(dupGuardModels())
	w.openWindowAny("sess-x", "X", false)
	top := w.openWindowAny("sess-top", "TOP", false)
	if w.desktop.TopLayer() != top.layer {
		t.Fatalf("precondition: sess-top should be on top")
	}

	// Collision on the buried sess-x — must not disturb the top window.
	w.openWindowAny("sess-x", "X", false)

	if w.desktop.TopLayer() != top.layer {
		t.Error("collision on a buried id disturbed the top window (phantom AddLayer?)")
	}
}

// ---------------------------------------------------------------------------
// Cross-check: the three sites agree (no split-brain across A and C together)
// ---------------------------------------------------------------------------

// TestAdoptSessionDedupNeverReachesOpenWindowAnyCollision confirms the two guards
// are consistent: because AdoptSession short-circuits before openWindowAny, a
// duplicate adopt must NOT emit the openWindowAny tripwire (A handles it first). If
// it did, that would indicate A's guard is missing and only the C backstop fired.
func TestAdoptSessionDedupNeverReachesOpenWindowAnyCollision(t *testing.T) {
	w := NewWorkbench(dupGuardModels())
	w.AdoptSession(rsMsg("s1", "a"))

	buf, restore := captureLog(t)
	defer restore()

	w.AdoptSession(rsMsg("s1", "b")) // dedup — handled by A, not C

	if strings.Contains(buf.String(), "duplicate-open guard") {
		t.Errorf("duplicate adopt reached openWindowAny's C guard (A should have short-circuited):\n%s", buf.String())
	}
}
