package ui

import (
	"reflect"
	"testing"

	"gogent/internal/stats"
)

// These tests cover the two changes that fix the Statistics dialog:
//
//   - issue #277: lifetimeStats.fold must re-emit a stats.SessionRow for every
//     remembered-but-closed session (so the dialog keeps per-session history once
//     it consumes the folded report), keeping open rows verbatim and appending the
//     closed ones in stable id order, without mutating the input report.
//   - issue #278: filterPhantomSessions must drop the phantom backend "default"
//     session (and back its full contribution out of the grand totals) on the TUI
//     side only, so the dialog/Overall count matches the sidebar window count while
//     the backend Statistics() report — which backs GET /stats — is left untouched.
//
// They reuse the buildReport / sessSpec / conn / row / agg helpers defined in
// overall_stats_lifetime_test.go.

// closedRowOf returns the re-emitted row a fold produces for one closed session.
func sessionByID(rows []stats.SessionRow, id string) (stats.SessionRow, bool) {
	for _, r := range rows {
		if r.ID == id {
			return r, true
		}
	}
	return stats.SessionRow{}, false
}

// --- issue #277: fold re-emits closed session rows ---------------------------

// TestLifetimeFold_ReEmitsClosedSessionRow is the core #277 behaviour: a session
// that has closed is re-emitted as a SessionRow carrying its last-known tally
// (turns, tokens, tool calls, compactions, primary model, primary/fast connector),
// with the live-only ContextTokens/ContextWindow left at zero.
func TestLifetimeFold_ReEmitsClosedSessionRow(t *testing.T) {
	ls := newLifetimeStats()
	spec := sessSpec{
		id: "s1", primaryModel: "glm", turns: 7, tokensIn: 1000, tokensOut: 500,
		toolCalls: 12, compactions: 1,
		primary: conn(42, 3, 1000, 250, 500),
		fast:    conn(2, 0, 50, 0, 5),
	}
	ls.fold(buildReport(spec))      // open, recorded
	after := ls.fold(buildReport()) // close: live report is empty
	if len(after.Sessions) != 1 {
		t.Fatalf("after close: %d session rows, want 1 re-emitted closed row: %+v", len(after.Sessions), after.Sessions)
	}
	got := after.Sessions[0]
	want := stats.SessionRow{
		ID: "s1", Turns: 7, TokensIn: 1000, TokensOut: 500, ToolCalls: 12,
		Compactions: 1, PrimaryModel: "glm",
		Primary: conn(42, 3, 1000, 250, 500), Fast: conn(2, 0, 50, 0, 5),
		// ContextTokens/ContextWindow are live-only -> zero for a closed row.
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("re-emitted closed row mismatch:\n  got  = %+v\n  want = %+v", got, want)
	}
	if got.ContextTokens != 0 || got.ContextWindow != 0 {
		t.Errorf("closed row context = %d/%d, want 0/0 (context is live-only)", got.ContextTokens, got.ContextWindow)
	}
}

// TestLifetimeFold_OpenRowsVerbatimThenClosedStableOrder pins the ordering
// contract: open rows pass through verbatim in the live report's order, then the
// closed rows are appended sorted by id (so the list is deterministic regardless of
// map iteration order or the order sessions were opened/closed).
func TestLifetimeFold_OpenRowsVerbatimThenClosedStableOrder(t *testing.T) {
	ls := newLifetimeStats()
	// Open four sessions whose ids do NOT sort in insertion order.
	zSpec := sessSpec{id: "z", primaryModel: "glm", primary: conn(1, 0, 10, 0, 1)}
	mSpec := sessSpec{id: "m", primaryModel: "glm", primary: conn(2, 0, 20, 0, 2)}
	aSpec := sessSpec{id: "a", primaryModel: "glm", primary: conn(3, 0, 30, 0, 3)}
	bSpec := sessSpec{id: "b", primaryModel: "glm", primary: conn(4, 0, 40, 0, 4)}
	ls.fold(buildReport(zSpec, mSpec, aSpec, bSpec))

	// Close z, m and a; keep b open. The live report lists only b.
	after := ls.fold(buildReport(bSpec))

	var ids []string
	for _, r := range after.Sessions {
		ids = append(ids, r.ID)
	}
	// Open row first (verbatim), then the closed ids in sorted order.
	want := []string{"b", "a", "m", "z"}
	if !reflect.DeepEqual(ids, want) {
		t.Errorf("session row order = %v, want %v (open verbatim, then closed sorted by id)", ids, want)
	}

	// The open row really is the live one (verbatim), not a re-emitted copy.
	if !reflect.DeepEqual(after.Sessions[0], row(bSpec)) {
		t.Errorf("first row = %+v, want the live b row verbatim %+v", after.Sessions[0], row(bSpec))
	}
}

// TestLifetimeFold_OpenAndClosedRowsMixed verifies that when some sessions are open
// and others closed, the live (open) rows are preserved exactly and only the closed
// ones are appended — no row is dropped or duplicated.
func TestLifetimeFold_OpenAndClosedRowsMixed(t *testing.T) {
	ls := newLifetimeStats()
	s1 := sessSpec{id: "s1", primaryModel: "glm", primary: conn(10, 0, 100, 0, 10)}
	s2 := sessSpec{id: "s2", primaryModel: "glm", primary: conn(20, 0, 200, 0, 20)}
	s3 := sessSpec{id: "s3", primaryModel: "glm", primary: conn(30, 0, 300, 0, 30)}
	ls.fold(buildReport(s1, s2, s3))

	// Close s2; s1 and s3 stay open.
	after := ls.fold(buildReport(s1, s3))
	if len(after.Sessions) != 3 {
		t.Fatalf("got %d rows, want 3 (2 open + 1 closed): %+v", len(after.Sessions), after.Sessions)
	}
	if r, ok := sessionByID(after.Sessions, "s2"); !ok || r.Primary.Requests != 20 {
		t.Errorf("closed s2 row = %+v ok=%v, want re-emitted {Req:20}", r, ok)
	}
	// Live rows verbatim.
	if r, _ := sessionByID(after.Sessions, "s1"); !reflect.DeepEqual(r, row(s1)) {
		t.Errorf("open s1 row not verbatim: %+v", r)
	}
	if r, _ := sessionByID(after.Sessions, "s3"); !reflect.DeepEqual(r, row(s3)) {
		t.Errorf("open s3 row not verbatim: %+v", r)
	}
}

// TestLifetimeFold_ReEmitDoesNotMutateInputBackingArray is a sharper variant of
// TestLifetimeFold_DoesNotMutateInputReport: it gives the input's Sessions slice
// spare capacity and proves fold builds a fresh slice for the closed-row append
// rather than writing into the caller's backing array (which an append onto
// report.Sessions would silently do when cap > len).
func TestLifetimeFold_ReEmitDoesNotMutateInputBackingArray(t *testing.T) {
	ls := newLifetimeStats()
	// Remember a closed session so the re-emit path runs on the next fold.
	ls.fold(buildReport(sessSpec{id: "closed", primaryModel: "glm", primary: conn(9, 0, 90, 0, 9)}))

	// Build a live report whose Sessions slice has spare capacity.
	backing := make([]stats.SessionRow, 1, 8)
	backing[0] = row(sessSpec{id: "open", primaryModel: "glm", primary: conn(1, 0, 10, 0, 1)})
	in := buildReport()
	in.Sessions = backing[:1]
	in.Totals.Sessions = 1

	got := ls.fold(in)

	// The closed row must be present in the output...
	if _, ok := sessionByID(got.Sessions, "closed"); !ok {
		t.Fatalf("closed row missing from fold output: %+v", got.Sessions)
	}
	// ...but the caller's slice length must be unchanged and the spare capacity slot
	// must not have been written (which would corrupt an array the caller still uses).
	if len(in.Sessions) != 1 {
		t.Errorf("fold grew the caller's Sessions slice to len %d, want 1", len(in.Sessions))
	}
	full := backing[:cap(backing)]
	if full[1].ID != "" {
		t.Errorf("fold wrote a re-emitted row into the caller's spare capacity: %+v", full[1])
	}
}

// --- issue #278: filterPhantomSessions --------------------------------------

// TestFilterPhantomSessions_RemovesDefaultAndBacksOutTotals proves the phantom
// "default" row is dropped and its FULL contribution — Sessions count, turns,
// tokens, tool calls, compactions, and both connectors — is subtracted from the
// grand totals, leaving exactly what the user's real sessions contributed.
func TestFilterPhantomSessions_RemovesDefaultAndBacksOutTotals(t *testing.T) {
	def := sessSpec{
		id:    phantomDefaultSessionID, // primaryModel left empty: no per-model row
		turns: 2, tokensIn: 100, tokensOut: 50, toolCalls: 1, compactions: 1,
		primary: conn(5, 1, 100, 0, 50), fast: conn(1, 0, 10, 0, 2),
	}
	real := sessSpec{
		id: "s1", primaryModel: "glm", turns: 3, tokensIn: 200, tokensOut: 80,
		toolCalls: 2, compactions: 0, primary: conn(7, 0, 200, 0, 80),
	}

	got := filterPhantomSessions(buildReport(def, real))

	// Only the real session remains as a row.
	if len(got.Sessions) != 1 || got.Sessions[0].ID != "s1" {
		t.Fatalf("Sessions = %+v, want only [s1]", got.Sessions)
	}
	// Totals equal a report built from the real session alone (default fully removed).
	wantTotals := buildReport(real).Totals
	if !reflect.DeepEqual(got.Totals, wantTotals) {
		t.Errorf("totals after filtering default:\n  got  = %+v\n  want = %+v", got.Totals, wantTotals)
	}
	// Spell out the headline fields so a failure pinpoints the wrong subtraction.
	if got.Totals.Sessions != 1 {
		t.Errorf("Totals.Sessions = %d, want 1 (default backed out)", got.Totals.Sessions)
	}
	if got.Totals.Primary != real.primary {
		t.Errorf("Totals.Primary = %+v, want %+v (only s1)", got.Totals.Primary, real.primary)
	}
	if got.Totals.Fast != (stats.ConnectorStat{}) {
		t.Errorf("Totals.Fast = %+v, want zero (default's fast traffic backed out)", got.Totals.Fast)
	}
	if got.Totals.Turns != 3 || got.Totals.TokensIn != 200 || got.Totals.ToolCalls != 2 || got.Totals.Compactions != 0 {
		t.Errorf("session-layer totals = turns %d / in %d / tools %d / comp %d, want 3/200/2/0",
			got.Totals.Turns, got.Totals.TokensIn, got.Totals.ToolCalls, got.Totals.Compactions)
	}
}

// TestFilterPhantomSessions_NoDefaultIsNoOp: a report without a "default" row is
// returned with identical content (the common TUI case once issue #278 is fixed at
// the backend would be moot, but the TUI must still not disturb a clean report).
func TestFilterPhantomSessions_NoDefaultIsNoOp(t *testing.T) {
	in := buildReport(
		sessSpec{id: "s1", primaryModel: "glm", primary: conn(10, 0, 100, 0, 10)},
		sessSpec{id: "s2", primaryModel: "groq", primary: conn(20, 1, 200, 0, 20)},
	)
	got := filterPhantomSessions(in)
	if !reflect.DeepEqual(got, in) {
		t.Errorf("filter changed a report with no default:\n  got = %+v\n  in  = %+v", got, in)
	}
}

// TestFilterPhantomSessions_DoesNotMutateInput guards the documented invariant: the
// filter works on value copies and a fresh Sessions slice, so the caller's report
// (e.g. the one the backend may reuse) is never altered.
func TestFilterPhantomSessions_DoesNotMutateInput(t *testing.T) {
	in := buildReport(
		sessSpec{id: phantomDefaultSessionID, turns: 1, tokensIn: 100, primary: conn(5, 0, 100, 0, 50)},
		sessSpec{id: "s1", primaryModel: "glm", primary: conn(7, 0, 200, 0, 80)},
	)
	in.Tools = []stats.ToolStat{{Name: "bash", Invocations: 2}}
	before := reflect.ValueOf(in).Interface().(stats.Report)

	_ = filterPhantomSessions(in)

	if !reflect.DeepEqual(in.Totals, before.Totals) {
		t.Errorf("filter mutated input Totals:\n  before = %+v\n  after = %+v", before.Totals, in.Totals)
	}
	if !reflect.DeepEqual(in.Sessions, before.Sessions) {
		t.Errorf("filter mutated input Sessions:\n  before = %+v\n  after = %+v", before.Sessions, in.Sessions)
	}
	if len(in.Sessions) != 2 {
		t.Errorf("filter dropped a row from the input slice: len = %d, want 2", len(in.Sessions))
	}
}

// --- the interaction: filter THEN fold (the dialog's exact composition) ------

// TestDialogPipeline_FilterThenFold_DefaultNeverReEmitted is the cross-cutting test
// the two issues demand together. The dialog composes
// fold(filterPhantomSessions(report)). Because the phantom "default" is stripped
// BEFORE fold ever sees it, fold never records it, so it can never resurface as a
// re-emitted "closed" row on later refreshes — while a genuine closed session does
// survive. This is the precise interaction the combined fix hinges on.
func TestDialogPipeline_FilterThenFold_DefaultNeverReEmitted(t *testing.T) {
	ls := newLifetimeStats()
	pipeline := func(r stats.Report) stats.Report { return ls.fold(filterPhantomSessions(r)) }

	// Refresh 1: the phantom default and a real session s1 are both "open" in the
	// backend report.
	out1 := pipeline(buildReport(
		sessSpec{id: phantomDefaultSessionID, primary: conn(3, 0, 30, 0, 3)},
		sessSpec{id: "s1", primaryModel: "glm", primary: conn(10, 1, 1000, 0, 100)},
	))
	if _, ok := sessionByID(out1.Sessions, phantomDefaultSessionID); ok {
		t.Fatalf("default present in folded output while open: %+v", out1.Sessions)
	}
	if out1.Totals.Sessions != 1 {
		t.Errorf("refresh1 Totals.Sessions = %d, want 1 (default filtered, s1 open)", out1.Totals.Sessions)
	}
	if _, ok := ls.sessions[phantomDefaultSessionID]; ok {
		t.Errorf("fold recorded the phantom default in the accumulator; it must be filtered before fold")
	}

	// Refresh 2: s1 has closed; only the phantom default remains in the backend.
	out2 := pipeline(buildReport(sessSpec{id: phantomDefaultSessionID, primary: conn(4, 0, 40, 0, 4)}))

	// s1 survives as a re-emitted closed row; default is absent.
	if r, ok := sessionByID(out2.Sessions, "s1"); !ok || r.Primary.Requests != 10 || r.Primary.TokensIn != 1000 {
		t.Errorf("closed s1 = %+v ok=%v, want re-emitted {Req:10 In:1000}", r, ok)
	}
	if _, ok := sessionByID(out2.Sessions, phantomDefaultSessionID); ok {
		t.Errorf("phantom default re-emitted as a closed row: %+v", out2.Sessions)
	}
	// s1's lifetime traffic persists in the totals; default's never entered them, and
	// the open count is 0 (nothing real is open).
	if out2.Totals.Primary.Requests != 10 || out2.Totals.Primary.TokensIn != 1000 {
		t.Errorf("refresh2 totals = %+v, want s1's persisted {Req:10 In:1000}, no default", out2.Totals.Primary)
	}
	if out2.Totals.Sessions != 0 {
		t.Errorf("refresh2 Totals.Sessions = %d, want 0 (default filtered, s1 closed)", out2.Totals.Sessions)
	}

	// Stress: many more refreshes with only the phantom present must never resurface
	// "default" and must hold s1's lifetime steady (no drift, no phantom re-emit).
	for i := 0; i < 20; i++ {
		out := pipeline(buildReport(sessSpec{id: phantomDefaultSessionID, primary: conn(4, 0, 40, 0, 4)}))
		if _, ok := sessionByID(out.Sessions, phantomDefaultSessionID); ok {
			t.Fatalf("refresh #%d resurfaced the phantom default: %+v", i, out.Sessions)
		}
		if out.Totals.Primary.Requests != 10 {
			t.Fatalf("refresh #%d drifted: Requests = %d, want stable 10", i, out.Totals.Primary.Requests)
		}
	}
}

// TestFilterPhantomSessions_AlsoExcludesDefaultFromModels covers a defect found in
// round 1 and since fixed by subtractModelTraffic: the phantom "default" session is
// the shared headless HTTP/API fallback, so it CAN accrue real per-model traffic (an
// HTTP request routed to "default" while gogent also runs the TUI). If the filter
// backed it out of report.Totals/Sessions but left report.Models alone, the Models
// tab and Overview "Models (tokens)" summary would keep counting it and the per-model
// rows would no longer sum to the filtered grand total — the phantom only half
// removed, violating #278's "should not appear in the Statistics report under the
// TUI". This pins that the per-model breakdown is also corrected.
func TestFilterPhantomSessions_AlsoExcludesDefaultFromModels(t *testing.T) {
	// default and a real session both used model "glm".
	rep := buildReport(
		sessSpec{id: phantomDefaultSessionID, primaryModel: "glm", primary: conn(5, 0, 500, 0, 50)},
		sessSpec{id: "s1", primaryModel: "glm", primary: conn(10, 0, 100, 0, 10)},
	)
	got := filterPhantomSessions(rep)

	ms, ok := got.ModelByName("glm")
	if !ok {
		t.Fatalf("glm model row missing after filter: %+v", got.Models)
	}
	// After fully excluding the phantom default, the per-model connector should carry
	// only the real session's traffic and must equal the filtered grand total.
	if ms.Connector.Requests != got.Totals.Primary.Requests {
		t.Errorf("per-model glm Requests = %d but filtered grand total = %d; default's model traffic leaked into Models",
			ms.Connector.Requests, got.Totals.Primary.Requests)
	}
	if ms.Connector.Requests != 10 || ms.Connector.TokensIn != 100 {
		t.Errorf("per-model glm = %+v, want only s1's {Req:10 In:100} (default excluded)", ms.Connector)
	}
}

// TestDialogPipeline_EphemeralNeverReEmitted mirrors the default-phantom interaction
// test for the ephemeral path: because filterPhantomSessions strips a live ephemeral
// HTTP session BEFORE fold records it, the session is never remembered, so when the
// HTTP client disconnects (the row vanishes) it is never re-emitted as a "closed"
// row — unlike a genuine windowed session, which does survive.
func TestDialogPipeline_EphemeralNeverReEmitted(t *testing.T) {
	ls := newLifetimeStats()
	pipeline := func(r stats.Report) stats.Report { return ls.fold(filterPhantomSessions(r)) }

	// Refresh 1: a windowed session win-1 and a live ephemeral HTTP session http-1.
	pipeline(markEphemeral(buildReport(
		sessSpec{id: "win-1", primaryModel: "glm", primary: conn(10, 0, 1000, 0, 100)},
		sessSpec{id: "http-1", primaryModel: "glm", primary: conn(5, 0, 500, 0, 50)},
	), "http-1"))
	if _, ok := ls.sessions["http-1"]; ok {
		t.Errorf("fold recorded the ephemeral http-1; it must be filtered before fold")
	}

	// Refresh 2: win-1 closes and the ephemeral client disconnects (both gone).
	out := pipeline(buildReport())

	// win-1 survives as a re-emitted closed row...
	if _, ok := sessionByID(out.Sessions, "win-1"); !ok {
		t.Errorf("windowed win-1 should survive as a closed row: %+v", out.Sessions)
	}
	// ...but the ephemeral http-1 never resurfaces.
	if _, ok := sessionByID(out.Sessions, "http-1"); ok {
		t.Errorf("ephemeral http-1 re-emitted as a closed row: %+v", out.Sessions)
	}
	// Only win-1's traffic persisted; the ephemeral's never entered the totals.
	if out.Totals.Primary.Requests != 10 || out.Totals.Primary.TokensIn != 1000 {
		t.Errorf("totals = %+v, want only win-1's {Req:10 In:1000}", out.Totals.Primary)
	}
}

// --- issue #278 round 2: ephemeral HTTP/API sessions -------------------------

// markEphemeral flags the rows with the given ids as ephemeral, mirroring what
// Gogent.Statistics() does for NewEphemeralSession sessions. buildReport already
// folded each row's traffic into the grand Totals, so tagging here does not change
// the totals — exactly the shape the TUI receives from the backend.
func markEphemeral(rep stats.Report, ids ...string) stats.Report {
	set := make(map[string]bool, len(ids))
	for _, id := range ids {
		set[id] = true
	}
	for i := range rep.Sessions {
		if set[rep.Sessions[i].ID] {
			rep.Sessions[i].Ephemeral = true
		}
	}
	return rep
}

// TestIsPhantomSession covers the windowless-session predicate: the shared "default"
// session (matched by id, since it is created via CreateUserSession and is NOT
// flagged ephemeral) OR any backend-flagged ephemeral session. Everything else — a
// normal windowed session — is kept.
func TestIsPhantomSession(t *testing.T) {
	cases := []struct {
		name string
		row  stats.SessionRow
		want bool
	}{
		{"default by id", stats.SessionRow{ID: phantomDefaultSessionID}, true},
		{"ephemeral flag", stats.SessionRow{ID: "http-9", Ephemeral: true}, true},
		{"default also flagged", stats.SessionRow{ID: phantomDefaultSessionID, Ephemeral: true}, true},
		{"normal windowed", stats.SessionRow{ID: "s1"}, false},
		{"non-default non-ephemeral", stats.SessionRow{ID: "default-2", Ephemeral: false}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isPhantomSession(tc.row); got != tc.want {
				t.Errorf("isPhantomSession(%+v) = %v, want %v", tc.row, got, tc.want)
			}
		})
	}
}

// TestFilterPhantomSessions_DropsEphemeralSessions is the round-2 #278 case: an
// ephemeral HTTP/API session (no TUI window) must be dropped from the TUI's report
// and its full contribution backed out of the grand totals and the per-model rows,
// just like the "default" phantom — even though its id is arbitrary, not "default".
func TestFilterPhantomSessions_DropsEphemeralSessions(t *testing.T) {
	real := sessSpec{id: "s1", primaryModel: "glm", turns: 3, tokensIn: 100, tokensOut: 10, toolCalls: 2, primary: conn(10, 0, 100, 0, 10)}
	httpSess := sessSpec{id: "http-client-7", primaryModel: "glm", turns: 1, tokensIn: 500, tokensOut: 50, toolCalls: 1, primary: conn(5, 1, 500, 0, 50)}

	rep := markEphemeral(buildReport(real, httpSess), "http-client-7")
	got := filterPhantomSessions(rep)

	// Only the real windowed session remains.
	if len(got.Sessions) != 1 || got.Sessions[0].ID != "s1" {
		t.Fatalf("Sessions = %+v, want only [s1] (ephemeral http-client-7 dropped)", got.Sessions)
	}
	// Totals reduced to the real session alone.
	if got.Totals.Sessions != 1 {
		t.Errorf("Totals.Sessions = %d, want 1 (ephemeral backed out)", got.Totals.Sessions)
	}
	if got.Totals.Primary != real.primary {
		t.Errorf("Totals.Primary = %+v, want %+v (ephemeral's traffic backed out)", got.Totals.Primary, real.primary)
	}
	if got.Totals.TokensIn != 100 || got.Totals.Turns != 3 || got.Totals.ToolCalls != 2 {
		t.Errorf("session-layer totals = in %d / turns %d / tools %d, want 100/3/2",
			got.Totals.TokensIn, got.Totals.Turns, got.Totals.ToolCalls)
	}
	// Per-model glm must carry only the real session's traffic, summing to the total.
	ms, ok := got.ModelByName("glm")
	if !ok {
		t.Fatalf("glm row missing: %+v", got.Models)
	}
	if ms.Connector != real.primary || ms.Connector.Requests != got.Totals.Primary.Requests {
		t.Errorf("per-model glm = %+v, want only s1's %+v (ephemeral's model traffic leaked)", ms.Connector, real.primary)
	}
}

// TestFilterPhantomSessions_DropsDefaultAndEphemeralTogether exercises the combined
// case the issue calls out — "off-by-N with live HTTP clients": the always-present
// default plus several ephemeral sessions are all removed in one pass, leaving only
// the windowed sessions and a count that matches the sidebar.
func TestFilterPhantomSessions_DropsDefaultAndEphemeralTogether(t *testing.T) {
	rep := markEphemeral(buildReport(
		sessSpec{id: phantomDefaultSessionID, primary: conn(1, 0, 10, 0, 1)},
		sessSpec{id: "win-1", primaryModel: "glm", primary: conn(10, 0, 100, 0, 10)},
		sessSpec{id: "http-a", primaryModel: "glm", primary: conn(2, 0, 20, 0, 2)},
		sessSpec{id: "win-2", primaryModel: "glm", primary: conn(20, 0, 200, 0, 20)},
		sessSpec{id: "http-b", primaryModel: "glm", primary: conn(3, 0, 30, 0, 3)},
	), "http-a", "http-b")

	got := filterPhantomSessions(rep)

	var ids []string
	for _, s := range got.Sessions {
		ids = append(ids, s.ID)
	}
	if !reflect.DeepEqual(ids, []string{"win-1", "win-2"}) {
		t.Errorf("kept sessions = %v, want [win-1 win-2] (default + 2 ephemeral dropped)", ids)
	}
	if got.Totals.Sessions != 2 {
		t.Errorf("Totals.Sessions = %d, want 2 (matches the 2 TUI windows)", got.Totals.Sessions)
	}
	// Grand total and per-model are only the two windows: 10+20 = 30 requests.
	if got.Totals.Primary.Requests != 30 {
		t.Errorf("Totals.Primary.Requests = %d, want 30 (win-1 10 + win-2 20)", got.Totals.Primary.Requests)
	}
	if ms, _ := got.ModelByName("glm"); ms.Connector.Requests != 30 {
		t.Errorf("per-model glm Requests = %d, want 30 (phantoms backed out)", ms.Connector.Requests)
	}
}

// --- subtractModelTraffic unit tests -----------------------------------------

// TestSubtractModelTraffic_BacksOutPrimaryModel verifies a removed session's
// connector and token attribution are subtracted from its PrimaryModel row, leaving
// other models untouched and matching fold's attribution rule.
func TestSubtractModelTraffic_BacksOutPrimaryModel(t *testing.T) {
	models := []stats.ModelStat{
		{Name: "glm", TokensIn: 300, TokensOut: 60, Connector: conn(30, 1, 300, 0, 60)},
		{Name: "groq", TokensIn: 50, TokensOut: 5, Connector: conn(5, 0, 50, 0, 5)},
	}
	removed := []stats.SessionRow{
		{ID: "http-1", PrimaryModel: "glm", TokensIn: 100, TokensOut: 20, Primary: conn(10, 0, 100, 0, 20)},
	}
	got := subtractModelTraffic(models, removed)

	glm, _ := stats.Report{Models: got}.ModelByName("glm")
	if glm.Connector.Requests != 20 || glm.Connector.TokensIn != 200 || glm.TokensIn != 200 || glm.TokensOut != 40 {
		t.Errorf("glm after subtract = conn %+v tokens %d/%d, want conn{Req:20 In:200} tokens 200/40",
			glm.Connector, glm.TokensIn, glm.TokensOut)
	}
	// groq (no removed session used it) is untouched.
	groq, _ := stats.Report{Models: got}.ModelByName("groq")
	if groq.Connector.Requests != 5 || groq.TokensIn != 50 {
		t.Errorf("groq changed unexpectedly: %+v", groq)
	}
}

// TestSubtractModelTraffic_DoesNotMutateInput guards that the input slice and its
// elements are not modified (the filter relies on this to keep the backend report
// the dialog also exports from intact).
func TestSubtractModelTraffic_DoesNotMutateInput(t *testing.T) {
	models := []stats.ModelStat{{Name: "glm", TokensIn: 300, Connector: conn(30, 0, 300, 0, 60)}}
	before := append([]stats.ModelStat(nil), models...)
	removed := []stats.SessionRow{{ID: "http-1", PrimaryModel: "glm", TokensIn: 100, Primary: conn(10, 0, 100, 0, 20)}}

	_ = subtractModelTraffic(models, removed)
	if !reflect.DeepEqual(models, before) {
		t.Errorf("subtractModelTraffic mutated input:\n  before = %+v\n  after  = %+v", before, models)
	}
}

// TestSubtractModelTraffic_SkipsEmptyModelAndNoOp covers two edge cases: a removed
// session with no PrimaryModel contributes no subtraction (its traffic was never
// attributed to a model), and an empty removed set is a pass-through.
func TestSubtractModelTraffic_SkipsEmptyModelAndNoOp(t *testing.T) {
	models := []stats.ModelStat{{Name: "glm", Connector: conn(30, 0, 300, 0, 60)}}

	// Removed session with empty model: nothing to back out -> models unchanged.
	got := subtractModelTraffic(models, []stats.SessionRow{{ID: "x", PrimaryModel: "", Primary: conn(9, 0, 90, 0, 9)}})
	if !reflect.DeepEqual(got, models) {
		t.Errorf("model-less removed session changed Models: %+v", got)
	}
	// Empty removed set: pass-through.
	if got := subtractModelTraffic(models, nil); !reflect.DeepEqual(got, models) {
		t.Errorf("empty removed set changed Models: %+v", got)
	}
}

// --- issue #278 round 2: exact per-model back-out (model-switching phantom) ---

// TestSubtractModelTraffic_UsesExactPerModelSplit is the regression for the round-1
// defect: a phantom session that switched models (A then B) carries an aggregate
// Primary connector (A+B) but its traffic is split across the A and B rows in
// report.Models. Backing the WHOLE aggregate out of the final model (the old
// behaviour) would drive B negative and strand A's contribution. With SessionRow
// carrying the exact PerModel split, each model's slice must be backed out of that
// same model — leaving A at zero and B reduced by only B's slice.
func TestSubtractModelTraffic_UsesExactPerModelSplit(t *testing.T) {
	// Grand per-model rows: A used only by the phantom (10/100); B used by the
	// phantom (20/200) plus a windowed session (5/50) -> 25/250.
	models := []stats.ModelStat{
		{Name: "A", TokensIn: 100, TokensOut: 10, Connector: conn(10, 0, 100, 0, 10)},
		{Name: "B", TokensIn: 250, TokensOut: 25, Connector: conn(25, 0, 250, 0, 25)},
	}
	phantom := stats.SessionRow{
		ID: phantomDefaultSessionID, PrimaryModel: "B", // final model is B
		TokensIn: 300, TokensOut: 30,
		Primary: conn(30, 0, 300, 0, 30), // aggregate A+B
		PerModel: []stats.SessionModelStat{
			{Name: "A", TokensIn: 100, TokensOut: 10, Connector: conn(10, 0, 100, 0, 10)},
			{Name: "B", TokensIn: 200, TokensOut: 20, Connector: conn(20, 0, 200, 0, 20)},
		},
	}

	got := subtractModelTraffic(models, []stats.SessionRow{phantom})

	a, _ := stats.Report{Models: got}.ModelByName("A")
	b, _ := stats.Report{Models: got}.ModelByName("B")
	// A fully backed out (exactly its slice), NOT left stranded.
	if a.Connector != (stats.ConnectorStat{}) || a.TokensIn != 0 || a.TokensOut != 0 {
		t.Errorf("model A = conn %+v tok %d/%d, want fully zeroed (phantom's A slice backed out)", a.Connector, a.TokensIn, a.TokensOut)
	}
	// B reduced by only B's slice (25-20=5 req, 250-200=50 in), NOT driven negative.
	if b.Connector.Requests != 5 || b.Connector.TokensIn != 50 || b.TokensIn != 50 {
		t.Errorf("model B = conn %+v tok %d, want {Req:5 In:50} tok 50 (only B's slice removed)", b.Connector, b.TokensIn)
	}
	if b.Connector.Requests < 0 || b.Connector.TokensIn < 0 {
		t.Errorf("model B went negative: %+v (over-subtraction regression)", b.Connector)
	}
}

// TestSubtractModelTraffic_BacksOutNodeCounts pins the round-2 fix that the per-model
// session / sub-agent node counts are also decremented for a removed phantom, keyed
// (like Gogent.Statistics()) by the session's PrimaryModel — so the Overall panel's
// model-scoped "sessions/sub-agents using this model" no longer counts a windowless
// session.
func TestSubtractModelTraffic_BacksOutNodeCounts(t *testing.T) {
	models := []stats.ModelStat{
		{Name: "glm", Sessions: 2, SubAgents: 5, Connector: conn(30, 0, 300, 0, 60)},
	}
	removed := []stats.SessionRow{
		{ID: phantomDefaultSessionID, PrimaryModel: "glm", SubAgents: 3, Primary: conn(10, 0, 100, 0, 20)},
	}
	got := subtractModelTraffic(models, removed)
	glm, _ := stats.Report{Models: got}.ModelByName("glm")
	if glm.Sessions != 1 {
		t.Errorf("glm Sessions = %d, want 1 (2 - phantom's 1)", glm.Sessions)
	}
	if glm.SubAgents != 2 {
		t.Errorf("glm SubAgents = %d, want 2 (5 - phantom's 3)", glm.SubAgents)
	}
}

// TestSubtractModelTraffic_ClampsTokenAndNodeFloors verifies clampZero floors the
// token and node-count back-outs at zero on synthetic input where a delta exceeds
// the row's seeded value, so the breakdown never renders negative tokens or counts.
func TestSubtractModelTraffic_ClampsTokenAndNodeFloors(t *testing.T) {
	models := []stats.ModelStat{
		{Name: "glm", TokensIn: 50, TokensOut: 5, Sessions: 0, SubAgents: 1, Connector: conn(0, 0, 0, 0, 0)},
	}
	// Removed session claims more tokens / sub-agents than the row was seeded with.
	removed := []stats.SessionRow{
		{ID: "http-1", PrimaryModel: "glm", TokensIn: 100, TokensOut: 99, SubAgents: 9, Primary: conn(0, 0, 0, 0, 0)},
	}
	got := subtractModelTraffic(models, removed)
	glm, _ := stats.Report{Models: got}.ModelByName("glm")
	if glm.TokensIn != 0 || glm.TokensOut != 0 {
		t.Errorf("tokens = %d/%d, want floored to 0/0 (clampZero)", glm.TokensIn, glm.TokensOut)
	}
	if glm.Sessions != 0 || glm.SubAgents != 0 {
		t.Errorf("node counts = %d/%d, want floored to 0/0 (clampZero)", glm.Sessions, glm.SubAgents)
	}
}

// TestFilterPhantomSessions_ExactPerModelBackOutKeepsInvariant is the end-to-end
// (real backend shape) proof of the round-2 fix: a windowless phantom that switched
// models is filtered, and afterwards the per-model connector rows still sum to the
// filtered grand Totals.Primary with no negative row — the invariant the round-1
// implementation broke for model-switching phantoms.
func TestFilterPhantomSessions_ExactPerModelBackOutKeepsInvariant(t *testing.T) {
	// A windowed session on B (5/50) and a phantom default that used A (10/100) then
	// B (20/200). Grand rows and totals are the faithful sum of both, as the backend
	// produces them.
	windowed := stats.SessionRow{ID: "win-1", PrimaryModel: "B", TokensIn: 50, TokensOut: 5, Primary: conn(5, 0, 50, 0, 5),
		PerModel: []stats.SessionModelStat{{Name: "B", TokensIn: 50, TokensOut: 5, Connector: conn(5, 0, 50, 0, 5)}}}
	phantom := stats.SessionRow{ID: phantomDefaultSessionID, PrimaryModel: "B", TokensIn: 300, TokensOut: 30, Primary: conn(30, 0, 300, 0, 30),
		PerModel: []stats.SessionModelStat{
			{Name: "A", TokensIn: 100, TokensOut: 10, Connector: conn(10, 0, 100, 0, 10)},
			{Name: "B", TokensIn: 200, TokensOut: 20, Connector: conn(20, 0, 200, 0, 20)},
		}}
	rep := stats.Report{
		Totals:   stats.Totals{Sessions: 2, TokensIn: 350, TokensOut: 35, Primary: conn(35, 0, 350, 0, 35)},
		Sessions: []stats.SessionRow{windowed, phantom},
		Models: []stats.ModelStat{
			{Name: "A", TokensIn: 100, TokensOut: 10, Connector: conn(10, 0, 100, 0, 10)},
			{Name: "B", TokensIn: 250, TokensOut: 25, Connector: conn(25, 0, 250, 0, 25)},
		},
	}

	got := filterPhantomSessions(rep)

	// Only the windowed session remains, and the grand total is just its traffic.
	if got.Totals.Sessions != 1 || got.Totals.Primary != conn(5, 0, 50, 0, 5) {
		t.Fatalf("filtered totals = sessions %d primary %+v, want 1 / {Req:5 In:50}", got.Totals.Sessions, got.Totals.Primary)
	}
	// The per-model connector rows must SUM to the filtered grand total — the
	// invariant the round-1 code violated for a model-switching phantom.
	var sum stats.ConnectorStat
	for _, m := range got.Models {
		if m.Connector.Requests < 0 || m.Connector.TokensIn < 0 {
			t.Errorf("model %s went negative: %+v", m.Name, m.Connector)
		}
		sum = sum.Add(m.Connector)
	}
	if sum != got.Totals.Primary {
		t.Errorf("sum of per-model connectors %+v != filtered grand total %+v (per-model breakdown inconsistent)", sum, got.Totals.Primary)
	}
}
