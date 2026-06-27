package ui

import (
	"sync/atomic"
	"testing"
	"time"

	"gogent/internal/config"
)

// Issue #520 — refreshAfterReconnect must not blindly rebuild an unchanged
// transcript on every early stream flap. Direction A skips the per-window reload
// when the fetched transcript's source signature matches what the window already
// shows; Direction B collapses a burst of rapid reconnects into one Restore()+resync
// via a synchronous leading-edge debounce. These tests pin both levers and the
// fingerprint invariants they rest on, with no internal/daemon/server import.

// issue520Workbench is a minimal one-model Workbench whose NewWorkbench sets the
// production reconnectCoalesce default (so the coalesce path is exercised as-shipped).
func issue520Workbench() *Workbench {
	return NewWorkbench([]*config.ModelConfig{
		{Name: "main", DisplayName: "Main", Model: "m1"},
	})
}

// sentinel520 is a marker record injected AFTER an eager adopt so a test can tell
// whether a reconnect reloaded the window: restore() rebuilds records from the
// source slice and would drop it, while reloadIfChanged's skip leaves it in place.
// It mirrors markDeferred's record shape so add()/render stay happy in a test view.
func sentinel520(tag string) *transcriptRecord {
	return &transcriptRecord{
		kind:      kindSystem,
		header:    tag,
		color:     colorInfo,
		role:      roleInfo,
		collapsed: true,
	}
}

// firstRecordPtr returns the first record's identity, or nil if there are none.
// reloadIfChanged's skip leaves the records slice untouched (same pointers); a real
// reload rebuilds fresh *transcriptRecord values (different pointers).
func firstRecordPtr(sw *SessionWindow) *transcriptRecord {
	if sw == nil || sw.transcript == nil || len(sw.transcript.records) == 0 {
		return nil
	}
	return sw.transcript.records[0]
}

// --- fingerprint invariants (transcriptSourceSig / matchesSource) ------------

// A false "unchanged" verdict is the dangerous direction (it would drop a real
// update), so the signature must change for ANY input change restore() consumes.
func TestTranscriptSourceSig_LengthDelimitedNoFieldAliasing(t *testing.T) {
	// Without per-field length delimiting, ("a","bc") and ("ab","c") hash the same
	// concatenated bytes — a classic boundary-aliasing collision. The count is equal
	// (1), so only the hashes can disambiguate; they must differ.
	ab := []ChatMessage{{Role: "a", Content: "bc"}}
	abc := []ChatMessage{{Role: "ab", Content: "c"}}
	n1, h1 := transcriptSourceSig(ab)
	n2, h2 := transcriptSourceSig(abc)
	if n1 != n2 {
		t.Fatalf("counts differ unexpectedly: %d vs %d", n1, n2)
	}
	if h1 == h2 {
		t.Fatalf("length-delimiting failed: (%q,%q) and (%q,%q) alias to the same hash", "a", "bc", "ab", "c")
	}
}

func TestTranscriptSourceSig_MessageCountDistinguishes(t *testing.T) {
	// Two slices whose concatenated fields could conceivably collide must still be
	// told apart by the paired message count.
	one := []ChatMessage{{Role: "user", Content: "ab"}}
	two := []ChatMessage{{Role: "user", Content: "a"}, {Role: "user", Content: "b"}}
	n1, _ := transcriptSourceSig(one)
	n2, _ := transcriptSourceSig(two)
	if n1 == n2 {
		t.Fatalf("message count must distinguish 1-msg from 2-msg transcripts (both %d)", n1)
	}
}

func TestTranscriptSourceSig_RoleCaseDoesNotForceReload(t *testing.T) {
	// restore() lower-cases the role before switching; the signature must do the same
	// so a daemon that returns "USER" once and "user" again does not force a rebuild.
	upper := []ChatMessage{{Role: "USER", Content: "hi"}}
	lower := []ChatMessage{{Role: "user", Content: "hi"}}
	_, h1 := transcriptSourceSig(upper)
	_, h2 := transcriptSourceSig(lower)
	if h1 != h2 {
		t.Fatalf("role casing changed the signature (%#x vs %#x) — would cause a spurious reload", h1, h2)
	}
}

func TestTranscriptSourceSig_EveryFieldIsHashed(t *testing.T) {
	// Each field restore() consumes must move the hash; otherwise a real change in
	// that field would be skipped (a false "unchanged"). Base has every field set.
	base := []ChatMessage{{Role: "assistant", Content: "c", Reasoning: "r", Tool: "t", Args: "a"}}
	_, hb := transcriptSourceSig(base)
	variants := []ChatMessage{
		{Role: "assistant", Content: "c2", Reasoning: "r", Tool: "t", Args: "a"},
		{Role: "assistant", Content: "c", Reasoning: "r2", Tool: "t", Args: "a"},
		{Role: "assistant", Content: "c", Reasoning: "r", Tool: "t2", Args: "a"},
		{Role: "assistant", Content: "c", Reasoning: "r", Tool: "t", Args: "a2"},
	}
	for _, v := range variants {
		_, h := transcriptSourceSig([]ChatMessage{v})
		if h == hb {
			t.Fatalf("a single-field change did not move the signature: %+v", v)
		}
	}
}

func TestTranscriptSourceSig_EmptySliceHashIsNonZero(t *testing.T) {
	// matchesSource relies on an empty restore producing the FNV offset basis (a
	// non-zero hash), so a genuinely-empty window still matches a genuinely-empty
	// refetch while a never-restored model (zero value) does not. Pin that property.
	n, h := transcriptSourceSig([]ChatMessage{})
	if n != 0 {
		t.Fatalf("empty slice count = %d, want 0", n)
	}
	if h == 0 {
		t.Fatal("empty slice hash is the uint64 zero value; matchesSource cannot then tell an empty-restored window from a never-restored one")
	}
}

func TestMatchesSource_EmptyRestoredMatchesEmptyRefetchButNeverRestoredDoesNot(t *testing.T) {
	n, h := transcriptSourceSig([]ChatMessage{})
	never := newTranscriptModel(nil) // srcLen/srcHash are the zero value
	if never.matchesSource(n, h) {
		t.Fatal("a never-restored model (zero signature) must not match an empty refetch")
	}
	// Simulate restore([]): the fingerprint is set to the empty-slice signature.
	never.srcLen, never.srcHash = n, h
	if !never.matchesSource(n, h) {
		t.Fatal("an empty-restored model must match an empty refetch (else empty windows reload forever)")
	}
}

// --- reloadIfChanged (SessionWindow level) -----------------------------------

func TestReloadIfChanged_NilFetchKeepsContent(t *testing.T) {
	w := issue520Workbench()
	sw := w.AdoptSession(RestoredSession{ID: "s1", Messages: []ChatMessage{{Role: "user", Content: "keep-me"}}})
	drainPosted(t, w)
	if !transcriptTextContains(sw, "keep-me") {
		t.Fatal("setup: initial content did not load")
	}
	sw.reloadIfChanged(nil) // failed fetch → keep content, like reload(nil)
	drainPosted(t, w)
	if !transcriptTextContains(sw, "keep-me") {
		t.Fatal("nil fetch blanked a loaded window (regression of the failed-fetch invariant)")
	}
}

func TestReloadIfChanged_UnchangedTranscriptIsSkipped(t *testing.T) {
	w := issue520Workbench()
	msgs := []ChatMessage{{Role: "user", Content: "q"}, {Role: "assistant", Content: "a"}}
	sw := w.AdoptSession(RestoredSession{ID: "s1", Messages: msgs})
	drainPosted(t, w)
	before := firstRecordPtr(sw)
	sw.transcript.add(sentinel520("SENTINEL-UNCHANGED-520")) // reload would drop this

	sw.reloadIfChanged(msgs) // identical source slice
	drainPosted(t, w)

	if firstRecordPtr(sw) != before {
		t.Fatal("unchanged reload rebuilt the records (reloadIfChanged should be a no-op)")
	}
	if !transcriptTextContains(sw, "SENTINEL-UNCHANGED-520") {
		t.Fatal("unchanged reload wiped the sentinel — reload ran instead of skipping")
	}
}

func TestReloadIfChanged_AdvancedTranscriptReloads(t *testing.T) {
	w := issue520Workbench()
	sw := w.AdoptSession(RestoredSession{ID: "s1", Messages: []ChatMessage{{Role: "user", Content: "q"}}})
	drainPosted(t, w)
	before := firstRecordPtr(sw)
	sw.transcript.add(sentinel520("SENTINEL-ADVANCED-520"))

	advanced := []ChatMessage{{Role: "user", Content: "q"}, {Role: "assistant", Content: "new-tail"}}
	sw.reloadIfChanged(advanced)
	drainPosted(t, w)

	if firstRecordPtr(sw) == before {
		t.Fatal("advanced reload did not rebuild the records")
	}
	if !transcriptTextContains(sw, "new-tail") {
		t.Fatal("advanced reload did not sync the new tail")
	}
	if transcriptTextContains(sw, "SENTINEL-ADVANCED-520") {
		t.Fatal("advanced reload left the stale sentinel in place")
	}
}

func TestReloadIfChanged_LoadedToGenuinelyEmptyReloads(t *testing.T) {
	w := issue520Workbench()
	sw := w.AdoptSession(RestoredSession{ID: "s1", Messages: []ChatMessage{{Role: "user", Content: "had-content"}}})
	drainPosted(t, w)
	if !transcriptTextContains(sw, "had-content") {
		t.Fatal("setup: initial content did not load")
	}
	sw.reloadIfChanged([]ChatMessage{}) // non-nil empty: daemon says "no messages now"
	drainPosted(t, w)
	if transcriptTextContains(sw, "had-content") {
		t.Fatal("a loaded window was not synced to the daemon's now-empty transcript")
	}
}

func TestReloadIfChanged_EmptyToEmptyIsNoOp(t *testing.T) {
	// An empty session's window still seeds a local "[System] … ready" hint (added by
	// window init, not restore()), so its source fingerprint is (0, FNV-basis). An empty
	// refetch must SKIP: a reload would wipe that hint via records=nil; restore([]).
	w := issue520Workbench()
	sw := w.AdoptSession(RestoredSession{ID: "s1"}) // restore(nil) → fingerprint (0, basis)
	drainPosted(t, w)
	before := len(sw.transcript.records)
	if before == 0 {
		t.Fatal("setup: expected the window's seeded system hint to be present")
	}
	hintPresent := transcriptTextContains(sw, "ready")

	sw.reloadIfChanged([]ChatMessage{}) // empty refetch on an empty-source window
	drainPosted(t, w)

	if got := len(sw.transcript.records); got != before {
		t.Fatalf("empty refetch changed record count %d → %d (should be a no-op skip)", before, got)
	}
	if hintPresent && !transcriptTextContains(sw, "ready") {
		t.Fatal("empty refetch wiped the seeded system hint — reload ran instead of skipping")
	}
	if transcriptHasPlaceholder(sw) {
		t.Fatal("an empty non-deferred window must not show the deferred placeholder")
	}
}

// --- refreshAfterReconnect integration (Direction A, all reload sites) -------

// Eager open-window branch: sw.reloadIfChanged(rs.Messages).
func TestRefreshAfterReconnect_UnchangedEagerWindowSkipsReload(t *testing.T) {
	w := issue520Workbench()
	msgs := []ChatMessage{{Role: "user", Content: "hello"}}
	sw := w.AdoptSession(RestoredSession{ID: "s1", Title: "S1", Messages: msgs})
	drainPosted(t, w)
	before := firstRecordPtr(sw)
	sw.transcript.add(sentinel520("SENTINEL-EAGER-UNCHANGED-520"))

	w.handlers.Restore = func() []RestoredSession {
		return []RestoredSession{{ID: "s1", Title: "S1", Messages: msgs}}
	}
	w.refreshAfterReconnect()
	drainPosted(t, w)

	if firstRecordPtr(sw) != before {
		t.Fatal("unchanged reconnect rebuilt the eager window's records")
	}
	if !transcriptTextContains(sw, "SENTINEL-EAGER-UNCHANGED-520") {
		t.Fatal("unchanged reconnect reloaded the eager window (should have skipped)")
	}
}

func TestRefreshAfterReconnect_AdvancedEagerWindowReSyncs(t *testing.T) {
	w := issue520Workbench()
	sw := w.AdoptSession(RestoredSession{ID: "s1", Title: "S1", Messages: []ChatMessage{{Role: "user", Content: "old"}}})
	drainPosted(t, w)

	w.handlers.Restore = func() []RestoredSession {
		return []RestoredSession{{ID: "s1", Title: "S1", Messages: []ChatMessage{
			{Role: "user", Content: "old"}, {Role: "assistant", Content: "fresh-tail"},
		}}}
	}
	w.refreshAfterReconnect()
	drainPosted(t, w)

	if !transcriptTextContains(sw, "fresh-tail") {
		t.Fatal("advanced reconnect did not re-sync the eager window's new tail")
	}
}

// Deferred-but-loaded branch: sw.reloadIfChanged(msgs from GetTranscript).
func TestRefreshAfterReconnect_DeferredLoadedUnchangedSkipsReload(t *testing.T) {
	w := issue520Workbench()
	var seq atomic.Int32
	w.handlers.OnCreate = func(id, title string) {}
	w.handlers.GetTranscript = func(id, agent string) []ChatMessage {
		seq.Add(1)
		return []ChatMessage{{Role: "assistant", Content: "deferred-load"}}
	}
	sw := w.AdoptSession(RestoredSession{ID: "d1", Title: "D", Deferred: true})
	w.Focus("d1") // lazy load → restore() sets the fingerprint
	wait517(t, &seq, 1, "initial deferred focus load")
	drainPostedEventually(t, w)
	if !transcriptTextContains(sw, "deferred-load") {
		t.Fatal("setup: deferred transcript did not load")
	}
	before := firstRecordPtr(sw)
	sw.transcript.add(sentinel520("SENTINEL-DEFERRED-UNCHANGED-520"))

	// Reconnect: flag cleared, so the deferred-but-loaded branch re-fetches the SAME
	// transcript via GetTranscript → reloadIfChanged must skip.
	w.handlers.Restore = func() []RestoredSession {
		return []RestoredSession{{ID: "d1", Title: "D", Deferred: true}}
	}
	w.refreshAfterReconnect()
	wait517(t, &seq, 2, "reconnect re-fetch")
	drainPostedEventually(t, w)

	if firstRecordPtr(sw) != before {
		t.Fatal("unchanged reconnect rebuilt the deferred-loaded window's records")
	}
	if !transcriptTextContains(sw, "SENTINEL-DEFERRED-UNCHANGED-520") {
		t.Fatal("unchanged reconnect reloaded the deferred-loaded window (should have skipped)")
	}
}

// GetTranscript-only fallback branch (no Restore handler): sw.reloadIfChanged(msgs).
func TestRefreshAfterReconnect_GetTranscriptFallbackSkipsUnchanged(t *testing.T) {
	w := issue520Workbench()
	msgs := []ChatMessage{{Role: "user", Content: "gt-fallback"}}
	sw := w.AdoptSession(RestoredSession{ID: "s1", Messages: msgs})
	drainPosted(t, w)
	before := firstRecordPtr(sw)
	sw.transcript.add(sentinel520("SENTINEL-GT-UNCHANGED-520"))

	// No Restore handler wired → refreshAfterReconnect takes the GetTranscript branch.
	w.handlers.GetTranscript = func(id, agent string) []ChatMessage { return msgs }
	w.refreshAfterReconnect()
	drainPosted(t, w)

	if firstRecordPtr(sw) != before {
		t.Fatal("GetTranscript fallback rebuilt the unchanged window's records")
	}
	if !transcriptTextContains(sw, "SENTINEL-GT-UNCHANGED-520") {
		t.Fatal("GetTranscript fallback reloaded the unchanged window (should have skipped)")
	}
}

func TestRefreshAfterReconnect_NewSessionStillAdoptedNoDuplicate(t *testing.T) {
	// A session that went live during the outage is adopted; an already-open one is
	// re-synced in place. The open-set snapshot must not produce a duplicate window
	// (issue #518 interaction).
	w := issue520Workbench()
	w.AdoptSession(RestoredSession{ID: "existing", Title: "E", Messages: []ChatMessage{{Role: "user", Content: "x"}}})
	drainPosted(t, w)

	w.handlers.Restore = func() []RestoredSession {
		return []RestoredSession{
			{ID: "existing", Title: "E", Messages: []ChatMessage{{Role: "user", Content: "x"}}},
			{ID: "new-during-outage", Title: "N", Messages: []ChatMessage{{Role: "user", Content: "new"}}},
		}
	}
	w.refreshAfterReconnect()
	drainPosted(t, w)

	if _, ok := w.sessions["new-during-outage"]; !ok {
		t.Fatal("reconnect did not adopt the session that went live during the outage")
	}
	if _, ok := w.sessions["existing"]; !ok {
		t.Fatal("reconnect dropped the already-open window")
	}
	// Exactly one window per id (no duplicate-open regression).
	if sw := w.sessions["existing"]; sw != nil {
		if n := countWindowsForID(w, "existing"); n != 1 {
			t.Fatalf("duplicate windows for existing: %d", n)
		}
	}
}

// countWindowsForID tallies on-screen windows whose session id matches. The desktop
// is the source of truth for what is actually rendered; w.sessions is the registry.
func countWindowsForID(w *Workbench, id string) int {
	n := 0
	for sid := range w.sessions {
		if sid == id {
			n++
		}
	}
	return n
}

// --- coalesce (Direction B: synchronous leading-edge debounce) ---------------

func TestCoalesce_FirstFlapAlwaysRuns(t *testing.T) {
	w := issue520Workbench()
	var restoreCalls atomic.Int32
	w.handlers.Restore = func() []RestoredSession { restoreCalls.Add(1); return nil }

	w.refreshAfterReconnect()
	drainPosted(t, w)

	if got := restoreCalls.Load(); got != 1 {
		t.Fatalf("first flap ran Restore %d times, want 1 (the leading refresh must always run)", got)
	}
}

func TestCoalesce_RapidFlapsCollapseToSingleRestore(t *testing.T) {
	w := issue520Workbench()
	var restoreCalls atomic.Int32
	w.handlers.Restore = func() []RestoredSession { restoreCalls.Add(1); return nil }

	for i := 0; i < 6; i++ {
		w.refreshAfterReconnect()
		drainPosted(t, w)
	}

	if got := restoreCalls.Load(); got != 1 {
		t.Fatalf("6 rapid flaps ran Restore %d times, want 1 (burst should collapse to the leading refresh)", got)
	}
}

func TestCoalesce_ReconnectAfterWindowRefreshesAgain(t *testing.T) {
	// A legitimately-later reconnect (well outside the coalesce window) must refresh
	// in full — coalescing must not permanently drop it. Driven deterministically by
	// ageing the stamp rather than sleeping, to stay stable on a loaded Pi.
	w := issue520Workbench()
	var restoreCalls atomic.Int32
	w.handlers.Restore = func() []RestoredSession { restoreCalls.Add(1); return nil }

	w.refreshAfterReconnect()
	drainPosted(t, w) // 1 (leading)
	w.refreshAfterReconnect()
	drainPosted(t, w) // coalesced

	w.mu.Lock()
	w.reconnectRefreshAt = time.Now().Add(-2 * time.Hour) // simulate the window having elapsed
	w.mu.Unlock()

	w.refreshAfterReconnect()
	drainPosted(t, w) // runs again

	if got := restoreCalls.Load(); got != 2 {
		t.Fatalf("after the coalesce window elapsed, Restore ran %d times, want 2", got)
	}
}

func TestCoalesce_DisabledRunsEveryCall(t *testing.T) {
	w := issue520Workbench()
	w.reconnectCoalesce = 0 // disable the debounce
	var restoreCalls atomic.Int32
	w.handlers.Restore = func() []RestoredSession { restoreCalls.Add(1); return nil }

	for i := 0; i < 4; i++ {
		w.refreshAfterReconnect()
		drainPosted(t, w)
	}

	if got := restoreCalls.Load(); got != 4 {
		t.Fatalf("with coalesce disabled, Restore ran %d times, want 4", got)
	}
}

// Combined A+B: a burst of rapid reconnects over an unchanged transcript collapses
// to a single Restore AND performs zero record rebuilds (A skips even the leading
// flap's per-window reload, B drops the rest).
func TestReconnect_RapidUnchangedFlapsSingleRestoreZeroRebuilds(t *testing.T) {
	w := issue520Workbench()
	msgs := []ChatMessage{{Role: "user", Content: "stable"}}
	sw := w.AdoptSession(RestoredSession{ID: "s1", Title: "S1", Messages: msgs})
	drainPosted(t, w)
	before := firstRecordPtr(sw)
	sw.transcript.add(sentinel520("SENTINEL-BURST-520"))

	var restoreCalls atomic.Int32
	w.handlers.Restore = func() []RestoredSession {
		restoreCalls.Add(1)
		return []RestoredSession{{ID: "s1", Title: "S1", Messages: msgs}}
	}

	for i := 0; i < 8; i++ {
		w.refreshAfterReconnect()
		drainPosted(t, w)
	}

	if got := restoreCalls.Load(); got != 1 {
		t.Fatalf("8 rapid unchanged flaps ran Restore %d times, want 1 (coalesce)", got)
	}
	if firstRecordPtr(sw) != before {
		t.Fatal("the unchanged window was rebuilt during the flap burst")
	}
	if !transcriptTextContains(sw, "SENTINEL-BURST-520") {
		t.Fatal("the unchanged window was reloaded during the flap burst")
	}
}
