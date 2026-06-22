package ui

import (
	"reflect"
	"strings"
	"testing"

	"gogent/internal/config"
	"gogent/internal/stats"
)

// These tests cover the process-lifetime accumulator that fixes issue #232: the
// Overall panel must keep token / request / error / cache-hit totals when a
// session closes, instead of recomputing them from the set of currently-open
// sessions. The Statistics report sums only the open sessions, so lifetimeStats
// remembers each session's last-known cumulative tally and folds it back into the
// grand totals / per-model breakdown once the session drops out of the live
// report.
//
// buildReport (below) mirrors how the backend assembles a Report: the grand
// Totals and the per-model Models are the faithful sum of the (open) session
// rows. Lifetime tests feed a sequence of such reports through fold and check
// that removing a session from the specs (i.e. closing it) never shrinks the
// accumulated totals.

// sessSpec is a compact description of one open session: its per-session row plus
// its contribution to the grand Totals and, via PrimaryModel, the per-model
// Connector breakdown.
type sessSpec struct {
	id           string
	primaryModel string
	turns        int
	tokensIn     int
	tokensOut    int
	toolCalls    int
	compactions  int
	primary      stats.ConnectorStat
	fast         stats.ConnectorStat
}

// row turns a sessSpec into the SessionRow a Statistics report carries.
func row(s sessSpec) stats.SessionRow {
	return stats.SessionRow{
		ID:           s.id,
		PrimaryModel: s.primaryModel,
		Turns:        s.turns,
		TokensIn:     s.tokensIn,
		TokensOut:    s.tokensOut,
		ToolCalls:    s.toolCalls,
		Compactions:  s.compactions,
		Primary:      s.primary,
		Fast:         s.fast,
	}
}

// buildReport assembles a Statistics report whose grand Totals (Sessions, Turns,
// token counters, ToolCalls, Compactions, Primary and Fast connectors) and whose
// per-model Models are the exact sum of the given open sessions — what the
// backend produces each refresh. Models are emitted in first-seen order.
func buildReport(specs ...sessSpec) stats.Report {
	var t stats.Totals
	t.Sessions = len(specs)
	rows := make([]stats.SessionRow, 0, len(specs))
	byModel := make(map[string]stats.ConnectorStat)
	var order []string
	for _, s := range specs {
		rows = append(rows, row(s))
		t.Turns += s.turns
		t.TokensIn += s.tokensIn
		t.TokensOut += s.tokensOut
		t.ToolCalls += s.toolCalls
		t.Compactions += s.compactions
		t.Primary = t.Primary.Add(s.primary)
		t.Fast = t.Fast.Add(s.fast)
		if s.primaryModel != "" {
			if _, ok := byModel[s.primaryModel]; !ok {
				order = append(order, s.primaryModel)
			}
			byModel[s.primaryModel] = byModel[s.primaryModel].Add(s.primary)
		}
	}
	models := make([]stats.ModelStat, 0, len(order))
	for _, name := range order {
		models = append(models, stats.ModelStat{Name: name, Connector: byModel[name]})
	}
	return stats.Report{Totals: t, Sessions: rows, Models: models}
}

// agg renders a folded report through the Overall panel's aggregate ("all
// models") view, returning just the headline traffic fields. Sessions/SubAgents
// node counts are passed as 0 here because the lifetime tests focus on the
// report-derived traffic figures, which is what #232 is about.
func agg(report stats.Report) overallStats {
	return buildOverallStats(report, 0, 0, nil, "")
}

// conn is a terse connector constructor to keep the specs readable.
func conn(req, errs, tin, cache, tout int) stats.ConnectorStat {
	return stats.ConnectorStat{
		Requests: req, Errors: errs, TokensIn: tin, CachedTokensIn: cache, TokensOut: tout,
	}
}

// --- core #232 scenarios -----------------------------------------------------

// TestLifetimeFold_AccruesWhileOpen is the baseline: while a session is open, the
// folded report matches the live report (fold is a no-op until something closes),
// and growth between folds is reflected immediately.
func TestLifetimeFold_AccruesWhileOpen(t *testing.T) {
	ls := newLifetimeStats()

	// First fold records the session; totals pass through unchanged.
	open := buildReport(sessSpec{id: "s1", primaryModel: "glm", primary: conn(10, 1, 1000, 100, 200)})
	r1 := ls.fold(open)
	if got := agg(r1); got.Requests != 10 || got.TokensIn != 1000 || got.Errors != 1 {
		t.Fatalf("open accrual = %+v, want {Req:10 In:1000 Err:1}", got)
	}

	// The session burns more tokens; the live total grows on the next fold.
	grew := buildReport(sessSpec{id: "s1", primaryModel: "glm", primary: conn(25, 2, 2500, 300, 500)})
	r2 := ls.fold(grew)
	if got := agg(r2); got.Requests != 25 || got.TokensIn != 2500 || got.Errors != 2 {
		t.Fatalf("grown accrual = %+v, want {Req:25 In:2500 Err:2}", got)
	}
}

// TestLifetimeFold_CloseKeepsTotals is THE regression for #232: a session that is
// present on one fold and absent on the next (closed) must NOT decrease the
// displayed totals. The folded total after close equals the total while it was
// open.
func TestLifetimeFold_CloseKeepsTotals(t *testing.T) {
	ls := newLifetimeStats()
	spec := sessSpec{id: "s1", primaryModel: "glm",
		primary: conn(42, 3, 1000, 250, 500), turns: 7, toolCalls: 12, compactions: 1}

	// Open the session and record its tally.
	ls.fold(buildReport(spec))

	// Close it: the session row disappears from the live report.
	after := ls.fold(buildReport()) // empty report

	got := agg(after)
	want := agg(buildReport(spec))
	if got.Requests != want.Requests || got.TokensIn != want.TokensIn ||
		got.TokensOut != want.TokensOut || got.Errors != want.Errors ||
		got.CacheHitPct != want.CacheHitPct {
		t.Fatalf("close dropped totals:\n  got  = %+v\n  want = %+v", got, want)
	}
	// Explicit zero-drop assertions so a failure points at the exact field.
	if got.Requests != 42 {
		t.Errorf("Requests = %d, want 42 (must persist after close)", got.Requests)
	}
	if got.TokensIn != 1000 {
		t.Errorf("TokensIn = %d, want 1000 (must persist after close)", got.TokensIn)
	}
	if got.TokensOut != 500 {
		t.Errorf("TokensOut = %d, want 500 (must persist after close)", got.TokensOut)
	}
	if got.Errors != 3 {
		t.Errorf("Errors = %d, want 3 (must persist after close)", got.Errors)
	}
	if got.CacheHitPct != 25 { // 250/1000
		t.Errorf("CacheHitPct = %d, want 25 (must persist after close)", got.CacheHitPct)
	}
}

// TestLifetimeFold_NewSessionAddsOnTop: after a session closes and its tally is
// remembered, opening a fresh session adds its traffic on top of the accumulated
// total rather than resetting it.
func TestLifetimeFold_NewSessionAddsOnTop(t *testing.T) {
	ls := newLifetimeStats()

	ls.fold(buildReport(sessSpec{id: "s1", primaryModel: "glm", primary: conn(10, 1, 1000, 0, 100)}))
	ls.fold(buildReport()) // s1 closes; its 1000 in / 100 out / 10 req persist

	// A new session opens and burns its own tokens.
	after := ls.fold(buildReport(sessSpec{id: "s2", primaryModel: "glm", primary: conn(5, 0, 500, 0, 50)}))
	got := agg(after)
	if got.Requests != 15 {
		t.Errorf("Requests = %d, want 15 (10 persisted + 5 new)", got.Requests)
	}
	if got.TokensIn != 1500 {
		t.Errorf("TokensIn = %d, want 1500 (1000 persisted + 500 new)", got.TokensIn)
	}
	if got.TokensOut != 150 {
		t.Errorf("TokensOut = %d, want 150 (100 persisted + 50 new)", got.TokensOut)
	}
	if got.Errors != 1 {
		t.Errorf("Errors = %d, want 1 (persisted from closed session)", got.Errors)
	}
}

// TestLifetimeFold_AllFieldsPersist checks every lifetime-sensitive aggregate —
// not just the four the panel shows — survives a close, so auxiliary rows (Fast
// backend, turns, tool calls, compactions) and every connector sub-counter are
// preserved too.
func TestLifetimeFold_AllFieldsPersist(t *testing.T) {
	ls := newLifetimeStats()
	spec := sessSpec{
		id: "s1", primaryModel: "glm", turns: 9, toolCalls: 4, compactions: 2,
		primary: stats.ConnectorStat{
			Requests: 11, Success: 9, Errors: 2, TokensIn: 1234, CachedTokensIn: 234,
			TokensOut: 567, TotalTimeMs: 8888, Timeouts: 1, ContextOverflows: 1,
			Refusals: 1, GenericErrors: 0,
		},
		fast: stats.ConnectorStat{
			Requests: 3, Success: 3, Errors: 0, TokensIn: 99, CachedTokensIn: 0,
			TokensOut: 12, TotalTimeMs: 100,
		},
	}
	want := buildReport(spec).Totals
	ls.fold(buildReport(spec))

	// Close the session.
	got := ls.fold(buildReport()).Totals

	// Traffic aggregates must match the open totals exactly...
	if got.Primary != want.Primary {
		t.Errorf("Primary connector changed on close:\n  got  = %+v\n  want = %+v", got.Primary, want.Primary)
	}
	if got.Fast != want.Fast {
		t.Errorf("Fast connector changed on close:\n  got  = %+v\n  want = %+v", got.Fast, want.Fast)
	}
	for _, tc := range []struct {
		name string
		g, w int
	}{
		{"Turns", got.Turns, want.Turns},
		{"TokensIn", got.TokensIn, want.TokensIn},
		{"TokensOut", got.TokensOut, want.TokensOut},
		{"ToolCalls", got.ToolCalls, want.ToolCalls},
		{"Compactions", got.Compactions, want.Compactions},
	} {
		if tc.g != tc.w {
			t.Errorf("%s = %d, want %d (must persist after close)", tc.name, tc.g, tc.w)
		}
	}
	// ...except Sessions, which is a live node count and is meant to drop on close.
	if got.Sessions != 0 {
		t.Errorf("Sessions = %d, want 0 (node count must reflect the closed set, not the lifetime)", got.Sessions)
	}
}

// TestLifetimeFold_MultipleSessionsPartialClose: with several sessions open,
// closing one keeps its contribution while the others keep updating live — the
// total is always the sum of every session ever seen.
func TestLifetimeFold_MultipleSessionsPartialClose(t *testing.T) {
	ls := newLifetimeStats()
	s1 := sessSpec{id: "s1", primaryModel: "glm", primary: conn(10, 0, 1000, 0, 100)}
	s2 := sessSpec{id: "s2", primaryModel: "glm", primary: conn(20, 1, 2000, 0, 200)}
	s3 := sessSpec{id: "s3", primaryModel: "glm", primary: conn(30, 2, 3000, 0, 300)}

	// All three open.
	ls.fold(buildReport(s1, s2, s3))
	// Close the middle one; s1 and s3 keep updating live (their numbers grow).
	grew1, grew3 := s1, s3
	grew1.primary = conn(15, 0, 1500, 0, 150)
	grew3.primary = conn(45, 3, 4500, 0, 450)
	got := agg(ls.fold(buildReport(grew1, grew3)))

	// Lifetime = s2's closed tally + the live s1/s3 values.
	if got.Requests != 20+15+45 {
		t.Errorf("Requests = %d, want 80 (s2 closed 20 + live s1 15 + s3 45)", got.Requests)
	}
	if got.TokensIn != 2000+1500+4500 {
		t.Errorf("TokensIn = %d, want 8000", got.TokensIn)
	}
	if got.Errors != 1+0+3 {
		t.Errorf("Errors = %d, want 4", got.Errors)
	}

	// Now close the remaining two: the lifetime total must still hold everything.
	got = agg(ls.fold(buildReport()))
	if got.Requests != 20+15+45 {
		t.Errorf("Requests after all closed = %d, want 80", got.Requests)
	}
	if got.TokensIn != 2000+1500+4500 {
		t.Errorf("TokensIn after all closed = %d, want 8000", got.TokensIn)
	}
}

// TestLifetimeFold_NoDoubleCountOnRepeatedFolds guards against the classic
// accumulator bug: folding the same open session report repeatedly must not
// inflate the totals. Each fold overwrites (not adds to) the remembered tally,
// and open sessions are never added back.
func TestLifetimeFold_NoDoubleCountOnRepeatedFolds(t *testing.T) {
	ls := newLifetimeStats()
	rep := buildReport(sessSpec{id: "s1", primaryModel: "glm", primary: conn(10, 1, 1000, 0, 100)})

	for i := 0; i < 5; i++ {
		got := agg(ls.fold(rep))
		if got.Requests != 10 || got.TokensIn != 1000 {
			t.Fatalf("fold #%d: Requests=%d TokensIn=%d, want 10/1000 (repeated fold must not inflate)",
				i, got.Requests, got.TokensIn)
		}
	}

	// And after closing once, re-folding the empty report repeatedly stays flat.
	empty := buildReport()
	for i := 0; i < 5; i++ {
		got := agg(ls.fold(empty))
		if got.Requests != 10 || got.TokensIn != 1000 {
			t.Fatalf("post-close fold #%d: Requests=%d TokensIn=%d, want 10/1000 (flat)", i, got.Requests, got.TokensIn)
		}
	}
}

// TestLifetimeFold_LiveUpdateOverwritesLastKnown pins that the remembered tally
// is the MOST RECENT snapshot, not the first. A session that grows then closes
// must persist its final (grown) value — guarding against a "store first, never
// update" regression that would under-count on close.
func TestLifetimeFold_LiveUpdateOverwritesLastKnown(t *testing.T) {
	ls := newLifetimeStats()
	ls.fold(buildReport(sessSpec{id: "s1", primaryModel: "glm", primary: conn(10, 0, 1000, 0, 100)}))
	ls.fold(buildReport(sessSpec{id: "s1", primaryModel: "glm", primary: conn(40, 0, 4000, 0, 400)}))
	ls.fold(buildReport(sessSpec{id: "s1", primaryModel: "glm", primary: conn(70, 0, 7000, 0, 700)}))

	got := agg(ls.fold(buildReport())) // close
	if got.Requests != 70 || got.TokensIn != 7000 || got.TokensOut != 700 {
		t.Fatalf("close persisted a stale tally = %+v, want the latest {Req:70 In:7000 Out:700}", got)
	}
}

// TestLifetimeFold_AggregateCacheHitAcrossSessions verifies the cache-hit
// percentage is recomputed from the summed cached/total input tokens across an
// open + closed mix, not remembered per-session and averaged.
func TestLifetimeFold_AggregateCacheHitAcrossSessions(t *testing.T) {
	ls := newLifetimeStats()
	// s1: 1000 in, 500 cached (50%). s2: 1000 in, 0 cached (0%).
	ls.fold(buildReport(sessSpec{id: "s1", primaryModel: "glm", primary: conn(1, 0, 1000, 500, 0)}))
	ls.fold(buildReport()) // s1 closes

	got := agg(ls.fold(buildReport(sessSpec{id: "s2", primaryModel: "glm", primary: conn(1, 0, 1000, 0, 0)})))
	// Lifetime: 2000 in, 500 cached -> 25%.
	if got.CacheHitPct != 25 {
		t.Errorf("CacheHitPct = %d, want 25 (500/2000 across open s2 + closed s1)", got.CacheHitPct)
	}
}

// --- per-model (selected-model) persistence ----------------------------------

// TestLifetimeFold_SelectedModelPersistsOnClose drives the panel through the
// selected-model view (issue #191): closing a session that used the selected
// model must keep that model's tokens / requests / errors / cache-hit in the
// scoped panel, not drop them.
func TestLifetimeFold_SelectedModelPersistsOnClose(t *testing.T) {
	ls := newLifetimeStats()
	spec := sessSpec{id: "s1", primaryModel: "glm", primary: conn(70, 4, 700, 70, 140)}
	ls.fold(buildReport(spec))
	after := ls.fold(buildReport()) // close

	want := buildOverallStats(buildReport(spec), 0, 0, nil, "glm")
	got := buildOverallStats(after, 0, 0, nil, "glm")
	if got.Requests != want.Requests || got.TokensIn != want.TokensIn ||
		got.TokensOut != want.TokensOut || got.Errors != want.Errors ||
		got.CacheHitPct != want.CacheHitPct {
		t.Fatalf("selected-model metrics dropped on close:\n  got  = %+v\n  want = %+v", got, want)
	}
	if got.Requests != 70 || got.TokensIn != 700 || got.Errors != 4 || got.CacheHitPct != 10 {
		t.Errorf("selected-model = %+v, want {Req:70 In:700 Err:4 Cache:10}", got)
	}
}

// TestLifetimeFold_PerModelMixedOpenAndClosed: a model with two sessions — one
// open, one closed — shows the SUM of both in its per-model connector (the open
// one live, the closed one folded back).
func TestLifetimeFold_PerModelMixedOpenAndClosed(t *testing.T) {
	ls := newLifetimeStats()
	ls.fold(buildReport(
		sessSpec{id: "s1", primaryModel: "glm", primary: conn(10, 1, 100, 0, 10)},
		sessSpec{id: "s2", primaryModel: "glm", primary: conn(20, 2, 200, 0, 20)},
	))
	// Close s1; s2 keeps updating.
	after := ls.fold(buildReport(sessSpec{id: "s2", primaryModel: "glm", primary: conn(25, 2, 250, 0, 25)}))

	ms, ok := after.ModelByName("glm")
	if !ok {
		t.Fatalf("ModelByName(glm) not found after mixed close")
	}
	// Closed s1 (10/100/10) + live s2 (25/250/25).
	if ms.Connector.Requests != 35 {
		t.Errorf("per-model Requests = %d, want 35 (closed 10 + live 25)", ms.Connector.Requests)
	}
	if ms.Connector.TokensIn != 350 {
		t.Errorf("per-model TokensIn = %d, want 350 (closed 100 + live 250)", ms.Connector.TokensIn)
	}
	if ms.Connector.Errors != 3 {
		t.Errorf("per-model Errors = %d, want 3 (closed 1 + live 2)", ms.Connector.Errors)
	}
}

// TestLifetimeFold_PerModelAllClosedSurfacesRow: when every session of a model
// has closed, the model row must still be present (with its accumulated
// connector) so the model selector keeps showing its lifetime traffic instead of
// the model vanishing from the breakdown.
func TestLifetimeFold_PerModelAllClosedSurfacesRow(t *testing.T) {
	ls := newLifetimeStats()
	ls.fold(buildReport(sessSpec{id: "s1", primaryModel: "glm", primary: conn(10, 1, 1000, 100, 200)}))
	after := ls.fold(buildReport()) // all closed -> live report has no models

	ms, ok := after.ModelByName("glm")
	if !ok {
		t.Fatalf("glm model row vanished after all its sessions closed; want it preserved with lifetime traffic")
	}
	if ms.Connector.Requests != 10 || ms.Connector.TokensIn != 1000 || ms.Connector.Errors != 1 {
		t.Errorf("closed-only glm row = %+v, want lifetime {Req:10 In:1000 Err:1}", ms.Connector)
	}
}

// TestLifetimeFold_PerModelNodeCountsStayLive documents that per-model
// Sessions/SubAgents are "what's on screen" counts and ARE meant to drop when a
// session closes — only the traffic figures (connector) persist. This pins the
// asymmetry the issue calls out as correct.
func TestLifetimeFold_PerModelNodeCountsStayLive(t *testing.T) {
	ls := newLifetimeStats()
	// glm has one open session (1) and one sub-agent.
	rep := buildReport(sessSpec{id: "s1", primaryModel: "glm", primary: conn(5, 0, 500, 0, 50)})
	rep.Models[0].Sessions = 1
	rep.Models[0].SubAgents = 1
	ls.fold(rep)

	after := ls.fold(buildReport()) // close
	ms, ok := after.ModelByName("glm")
	if !ok {
		t.Fatalf("glm row should still be present (lifetime traffic)")
	}
	if ms.Sessions != 0 || ms.SubAgents != 0 {
		t.Errorf("per-model node counts = %d/%d, want 0/0 (node counts must drop on close; only traffic persists)",
			ms.Sessions, ms.SubAgents)
	}
	if ms.Connector.Requests != 5 || ms.Connector.TokensIn != 500 {
		t.Errorf("per-model traffic did not persist = %+v, want {Req:5 In:500}", ms.Connector)
	}
}

// --- edge cases & robustness -------------------------------------------------

// TestLifetimeFold_EmptyReportIsNoOp: folding a report with no sessions (and no
// remembered sessions) returns it essentially unchanged — the panel's first
// frame and the "no sessions yet" state stay at zero.
func TestLifetimeFold_EmptyReportIsNoOp(t *testing.T) {
	ls := newLifetimeStats()
	empty := buildReport()
	got := ls.fold(empty)
	if got.Totals != (stats.Totals{}) {
		t.Errorf("empty fold produced non-zero totals = %+v", got.Totals)
	}
	if len(got.Sessions) != 0 || len(got.Models) != 0 {
		t.Errorf("empty fold produced rows = %+v", got)
	}
}

// TestLifetimeFold_SessionWithNoModelPersistsInAggregate: a session that never
// selected a model (empty PrimaryModel) still contributes its traffic to the
// aggregate grand total after it closes; it simply is not attributed to any
// per-model row (matching its live behaviour).
func TestLifetimeFold_SessionWithNoModelPersistsInAggregate(t *testing.T) {
	ls := newLifetimeStats()
	ls.fold(buildReport(sessSpec{id: "s1", primary: conn(8, 2, 300, 0, 30)}))
	after := ls.fold(buildReport()) // close

	got := agg(after)
	if got.Requests != 8 || got.TokensIn != 300 || got.Errors != 2 {
		t.Errorf("model-less session did not persist in aggregate = %+v, want {Req:8 In:300 Err:2}", got)
	}
	if _, ok := after.ModelByName("anything"); ok {
		t.Errorf("model-less session should not create a per-model row")
	}
	if len(after.Models) != 0 {
		t.Errorf("model-less session produced %d model rows, want 0", len(after.Models))
	}
}

// TestLifetimeFold_ReopenSameIDResumesLive: a session id that closes then
// reappears (a reopen) is treated as open again — it updates live and, when it
// closes a second time, persists its latest tally. This guards against the
// remembered tally getting "stuck" at the first close.
func TestLifetimeFold_ReopenSameIDResumesLive(t *testing.T) {
	ls := newLifetimeStats()
	ls.fold(buildReport(sessSpec{id: "s1", primaryModel: "glm", primary: conn(10, 0, 1000, 0, 100)}))
	ls.fold(buildReport()) // close
	if got := agg(ls.fold(buildReport())); got.TokensIn != 1000 {
		t.Fatalf("prerequisite: close should persist 1000, got %d", got.TokensIn)
	}

	// Reopen s1 with fresh counters starting from zero.
	ls.fold(buildReport(sessSpec{id: "s1", primaryModel: "glm", primary: conn(3, 0, 60, 0, 6)}))
	// While open it shows its own live value only.
	if got := agg(ls.fold(buildReport(sessSpec{id: "s1", primaryModel: "glm", primary: conn(3, 0, 60, 0, 6)}))); got.TokensIn != 60 {
		t.Errorf("reopened live TokensIn = %d, want 60", got.TokensIn)
	}
	// Close again: persists the reopened tally (60), not the stale first one (1000).
	got := agg(ls.fold(buildReport()))
	if got.TokensIn != 60 {
		t.Errorf("after second close TokensIn = %d, want 60 (latest reopen tally), not stale 1000", got.TokensIn)
	}
}

// TestLifetimeFold_DoesNotMutateInputReport verifies fold leaves the caller's
// report untouched: it must not mutate the grand totals, the per-session rows,
// the per-model breakdown, or the tools/skills the Statistics view renders from
// the raw report.
func TestLifetimeFold_DoesNotMutateInputReport(t *testing.T) {
	ls := newLifetimeStats()
	// Seed a remembered session first so the close-back path actually runs.
	ls.fold(buildReport(sessSpec{id: "s1", primaryModel: "glm", primary: conn(10, 1, 1000, 0, 100)}))

	in := buildReport(sessSpec{id: "s2", primaryModel: "glm", primary: conn(5, 0, 500, 0, 50)})
	in.Tools = []stats.ToolStat{{Name: "bash", Invocations: 3}}
	in.Skills = []stats.SkillStat{{Name: "deep-research", Success: 1}}
	before := reflect.ValueOf(in).Interface().(stats.Report)

	_ = ls.fold(in)

	if !reflect.DeepEqual(in.Totals, before.Totals) {
		t.Errorf("fold mutated input report.Totals:\n  before = %+v\n  after  = %+v", before.Totals, in.Totals)
	}
	if !reflect.DeepEqual(in.Sessions, before.Sessions) {
		t.Errorf("fold mutated input report.Sessions")
	}
	if !reflect.DeepEqual(in.Models, before.Models) {
		t.Errorf("fold mutated input report.Models")
	}
	if !reflect.DeepEqual(in.Tools, before.Tools) || !reflect.DeepEqual(in.Skills, before.Skills) {
		t.Errorf("fold mutated input report.Tools/Skills")
	}
}

// TestLifetimeFold_PassesThroughLiveRowsAndRegistries checks that fold only
// augments the lifetime-sensitive aggregates: the per-session rows, tool and
// skill breakdowns pass through verbatim (the Statistics view consumes the raw
// report and must keep its per-active-session semantics).
func TestLifetimeFold_PassesThroughLiveRowsAndRegistries(t *testing.T) {
	ls := newLifetimeStats()
	in := buildReport(sessSpec{id: "s1", primaryModel: "glm", primary: conn(1, 0, 10, 0, 1)})
	in.Tools = []stats.ToolStat{{Name: "bash", Invocations: 2, Success: 2}}
	in.Skills = []stats.SkillStat{{Name: "deep-research", Success: 1, Failure: 1, TotalCalls: 2}}

	got := ls.fold(in)
	if !reflect.DeepEqual(got.Sessions, in.Sessions) {
		t.Errorf("Sessions rows changed:\n  got = %+v\n  in  = %+v", got.Sessions, in.Sessions)
	}
	if !reflect.DeepEqual(got.Tools, in.Tools) {
		t.Errorf("Tools changed:\n  got = %+v\n  in  = %+v", got.Tools, in.Tools)
	}
	if !reflect.DeepEqual(got.Skills, in.Skills) {
		t.Errorf("Skills changed:\n  got = %+v\n  in  = %+v", got.Skills, in.Skills)
	}
}

// TestLifetimeFold_TrustsReportTotalsForOpenSessions documents the deliberate
// design choice noted in fold's doc comment: open sessions are taken from the
// report's own grand totals (not recomputed from the per-session rows), so fold
// is robust to reports whose totals are not a strict sum of their rows. The flip
// side — which this test pins — is that a session is only "added back" once it
// disappears from the live report.
func TestLifetimeFold_TrustsReportTotalsForOpenSessions(t *testing.T) {
	ls := newLifetimeStats()
	// A deliberately inconsistent report: a session row with traffic, but the
	// grand total reports none of it (simulating a backend hiccup).
	weird := buildReport()
	weird.Sessions = append(weird.Sessions, row(sessSpec{id: "s1", primaryModel: "glm", primary: conn(99, 0, 9900, 0, 0)}))

	got := agg(ls.fold(weird))
	// While open, fold trusts report.Totals (0), so the panel shows 0 — it does
	// not recompute from the row.
	if got.Requests != 0 || got.TokensIn != 0 {
		t.Errorf("open session should use report.Totals (0), got %+v", got)
	}
	// Once that session is gone, its remembered tally (99/9900) is folded back in.
	got = agg(ls.fold(buildReport()))
	if got.Requests != 99 || got.TokensIn != 9900 {
		t.Errorf("after close, remembered tally should be folded back: got %+v, want {Req:99 In:9900}", got)
	}
}

// --- mergeModelLifetime unit tests -------------------------------------------

// TestMergeModelLifetime_NoExtraIsPassthrough: with nothing to add back, the
// models slice is returned unchanged in content (a no-op merge).
func TestMergeModelLifetime_NoExtraIsPassthrough(t *testing.T) {
	in := []stats.ModelStat{
		{Name: "glm", Sessions: 1, Connector: conn(10, 0, 100, 0, 10)},
		{Name: "groq", Connector: conn(5, 0, 50, 0, 5)},
	}
	got := mergeModelLifetime(in, nil)
	if !reflect.DeepEqual(got, in) {
		t.Errorf("no-extra merge changed the slice:\n  got = %+v\n  in  = %+v", got, in)
	}
}

// TestMergeModelLifetime_AddsToExistingModel: extra connector for a model that is
// still in the live breakdown is added onto that model's Connector.
func TestMergeModelLifetime_AddsToExistingModel(t *testing.T) {
	in := []stats.ModelStat{{Name: "glm", Sessions: 1, Connector: conn(10, 0, 100, 0, 10)}}
	extra := map[string]stats.ConnectorStat{"glm": conn(20, 2, 200, 0, 20)}

	got := mergeModelLifetime(in, extra)
	if len(got) != 1 {
		t.Fatalf("got %d rows, want 1", len(got))
	}
	if got[0].Connector.Requests != 30 || got[0].Connector.TokensIn != 300 || got[0].Connector.Errors != 2 {
		t.Errorf("merged Connector = %+v, want {Req:30 In:300 Err:2}", got[0].Connector)
	}
	// The live node counts on the existing row are left untouched.
	if got[0].Sessions != 1 {
		t.Errorf("Sessions = %d, want 1 (live node counts must be preserved on merge)", got[0].Sessions)
	}
}

// TestMergeModelLifetime_AppendsClosedOnlyModel: a model present only in extra
// (all its sessions closed) is appended as a new row carrying its lifetime
// connector.
func TestMergeModelLifetime_AppendsClosedOnlyModel(t *testing.T) {
	in := []stats.ModelStat{{Name: "glm", Sessions: 1, Connector: conn(10, 0, 100, 0, 10)}}
	extra := map[string]stats.ConnectorStat{"groq": conn(7, 1, 70, 0, 7)}

	got := mergeModelLifetime(in, extra)
	if len(got) != 2 {
		t.Fatalf("got %d rows, want 2 (existing + appended closed-only)", len(got))
	}
	// Existing row first, appended row second.
	if got[0].Name != "glm" || got[0].Connector.Requests != 10 {
		t.Errorf("existing row = %+v, want glm/10", got[0])
	}
	if got[1].Name != "groq" || got[1].Connector.Requests != 7 || got[1].Connector.TokensIn != 70 {
		t.Errorf("appended row = %+v, want groq {Req:7 In:70}", got[1])
	}
	if got[1].Sessions != 0 || got[1].SubAgents != 0 {
		t.Errorf("appended closed-only row should carry zero node counts, got Sessions=%d SubAgents=%d",
			got[1].Sessions, got[1].SubAgents)
	}
}

// TestMergeModelLifetime_DoesNotMutateInput verifies the input slice and its
// elements are not modified — fold relies on this to avoid corrupting the raw
// report the Statistics view renders from.
func TestMergeModelLifetime_DoesNotMutateInput(t *testing.T) {
	in := []stats.ModelStat{{Name: "glm", Connector: conn(10, 0, 100, 0, 10)}}
	before := append([]stats.ModelStat(nil), in...)
	extra := map[string]stats.ConnectorStat{"glm": conn(5, 0, 50, 0, 5)}

	_ = mergeModelLifetime(in, extra)
	if !reflect.DeepEqual(in, before) {
		t.Errorf("merge mutated input models:\n  before = %+v\n  after  = %+v", before, in)
	}
}

// --- Workbench integration ---------------------------------------------------

// TestWorkbench_OverallPersistsOnClose drives the full refresh path
// (GetStatistics -> fold -> sidebar.overall) and proves the acceptance criterion
// end-to-end: with one session open the panel shows its traffic; after that
// session closes, the panel keeps showing the same totals instead of dropping to
// zero.
func TestWorkbench_OverallPersistsOnClose(t *testing.T) {
	w := newTestWorkbench(t)
	var current stats.Report
	w.handlers.GetStatistics = func() stats.Report { return current }

	// s1 open and burning tokens.
	current = buildReport(sessSpec{id: "s1", primaryModel: "test",
		primary: conn(42, 3, 1000, 250, 500)})
	w.refreshOverall()
	if got := w.sidebar.overall; got.Requests != 42 || got.TokensIn != 1000 || got.Errors != 3 {
		t.Fatalf("open overall = %+v, want {Req:42 In:1000 Err:3}", got)
	}

	// s1 closes: the report no longer lists it.
	current = buildReport()
	w.refreshOverall()
	got := w.sidebar.overall
	if got.Requests != 42 {
		t.Errorf("Requests = %d after close, want 42 (must persist)", got.Requests)
	}
	if got.TokensIn != 1000 {
		t.Errorf("TokensIn = %d after close, want 1000 (must persist)", got.TokensIn)
	}
	if got.TokensOut != 500 {
		t.Errorf("TokensOut = %d after close, want 500 (must persist)", got.TokensOut)
	}
	if got.Errors != 3 {
		t.Errorf("Errors = %d after close, want 3 (must persist)", got.Errors)
	}
	if got.CacheHitPct != 25 {
		t.Errorf("CacheHitPct = %d after close, want 25 (must persist)", got.CacheHitPct)
	}
}

// TestWorkbench_OverallAccruesMultipleSessions is the full lifecycle through the
// workbench: open two sessions (totals = s1+s2), close one (totals unchanged),
// then close the other (totals still hold), then open a third (adds on top).
func TestWorkbench_OverallAccruesMultipleSessions(t *testing.T) {
	w := newTestWorkbench(t)
	var current stats.Report
	w.handlers.GetStatistics = func() stats.Report { return current }

	// Open s1 and s2.
	current = buildReport(
		sessSpec{id: "s1", primaryModel: "test", primary: conn(10, 1, 1000, 0, 100)},
		sessSpec{id: "s2", primaryModel: "test", primary: conn(20, 2, 2000, 0, 200)},
	)
	w.refreshOverall()
	if got := w.sidebar.overall; got.Requests != 30 || got.TokensIn != 3000 {
		t.Fatalf("two open: overall = %+v, want {Req:30 In:3000}", got)
	}

	// Close s1; s2 remains.
	current = buildReport(sessSpec{id: "s2", primaryModel: "test", primary: conn(20, 2, 2000, 0, 200)})
	w.refreshOverall()
	if got := w.sidebar.overall; got.Requests != 30 || got.TokensIn != 3000 {
		t.Errorf("after s1 close: overall = %+v, want {Req:30 In:3000} (s1 persisted)", got)
	}

	// Close s2 too; nothing is open.
	current = buildReport()
	w.refreshOverall()
	if got := w.sidebar.overall; got.Requests != 30 || got.TokensIn != 3000 {
		t.Errorf("after all closed: overall = %+v, want {Req:30 In:3000} (both persisted)", got)
	}

	// Open s3; it adds on top of the accumulated lifetime.
	current = buildReport(sessSpec{id: "s3", primaryModel: "test", primary: conn(5, 0, 500, 0, 50)})
	w.refreshOverall()
	if got := w.sidebar.overall; got.Requests != 35 || got.TokensIn != 3500 {
		t.Errorf("after s3 open: overall = %+v, want {Req:35 In:3500} (lifetime 30 + s3 5)", got)
	}
}

// TestWorkbench_OverallPersistsAcrossManyRefreshes is a stress variant: a burst
// of refreshes (as the coalesced ticker / event paths fire) while the session is
// closed must keep the lifetime total stable — no drift from repeated folds.
func TestWorkbench_OverallPersistsAcrossManyRefreshes(t *testing.T) {
	w := newTestWorkbench(t)
	w.handlers.GetStatistics = func() stats.Report {
		return buildReport(sessSpec{id: "s1", primaryModel: "test", primary: conn(10, 0, 1000, 0, 100)})
	}
	w.refreshOverall()                                                      // record s1
	w.handlers.GetStatistics = func() stats.Report { return buildReport() } // s1 closed
	for i := 0; i < 25; i++ {
		w.refreshOverall()
		if got := w.sidebar.overall; got.Requests != 10 || got.TokensIn != 1000 {
			t.Fatalf("refresh #%d: overall = %+v, want stable {Req:10 In:1000}", i, got)
		}
	}
}

// --- round 2: gaps surfaced in review ----------------------------------------

// TestStatisticsDialog_UsesFoldedFilteredReport pins the new contract (issues #277
// + #278) and REPLACES the old TestStatisticsView_UsesRawReportNotFolded, which
// pinned the now-removed "dialog consumes the raw report" behaviour.
//
// showStatisticsDialog now builds its report as
//
//	w.overallLifetime.fold(filterPhantomSessions(w.handlers.GetStatistics()))
//
// (statistics_dialog.go), so the dialog must: (a) fold through the process-lifetime
// accumulator like the Overall panel — closing a session keeps its traffic and its
// per-session row (#277); and (b) filter the phantom backend "default" session —
// which has no TUI window — BEFORE the fold, so its count matches the sidebar and it
// is never remembered as a closed row (#278).
//
// The dialog renders into a TextView the test cannot read back, so the dialog is
// bound from two angles: its observable side effect on the shared lifetime
// accumulator (fold records the open set; the filter keeps "default" out of it), and
// the report rebuilt from that exact accumulator expression, rendered through the
// same renderStatistics the dialog uses.
func TestStatisticsDialog_UsesFoldedFilteredReport(t *testing.T) {
	w := newTestWorkbench(t)

	closed := sessSpec{id: "s1", primaryModel: "test", primary: conn(100, 1, 1111, 0, 0)}
	open := sessSpec{id: "s2", primaryModel: "test", primary: conn(23, 0, 222, 0, 0)}

	// s1 opens then closes; s2 stays open. The Overall panel (folded) keeps both.
	w.handlers.GetStatistics = func() stats.Report { return buildReport(closed) }
	w.refreshOverall()
	if got := w.sidebar.overall; got.Requests != 100 || got.TokensIn != 1111 {
		t.Fatalf("Overall after s1 open = %+v, want {Req:100 In:1111}", got)
	}

	// The live backend report now lists the open s2 plus the always-present phantom
	// "default" session (which has its own HTTP traffic but no TUI window). s1 closed.
	var calls int
	w.handlers.GetStatistics = func() stats.Report {
		calls++
		return buildReport(open, sessSpec{id: phantomDefaultSessionID, primary: conn(7, 2, 70, 0, 7)})
	}
	w.showStatisticsDialog()

	// The dialog fetches exactly one report (the single GetStatistics call wrapped by
	// filterPhantomSessions), and opens a non-blocking, panic-free modal layer.
	if calls != 1 {
		t.Fatalf("showStatisticsDialog called GetStatistics %d times, want exactly 1", calls)
	}
	if top := w.desktop.TopLayer(); top == nil || top.Name != "statistics-dialog" {
		t.Fatalf("top layer = %v, want statistics-dialog", top)
	}

	// Folding is observable: opening the dialog records the live (open) sessions into
	// the shared lifetime accumulator. The phantom "default" must NOT be recorded —
	// filterPhantomSessions strips it BEFORE fold, so it can never be re-emitted as a
	// closed row on a later refresh. (Had the dialog used the raw report, neither s2
	// nor anything else would have been recorded by opening it.)
	if _, ok := w.overallLifetime.sessions["s2"]; !ok {
		t.Error("dialog did not fold: open session s2 was not recorded in the lifetime accumulator")
	}
	if _, ok := w.overallLifetime.sessions[phantomDefaultSessionID]; ok {
		t.Error("dialog recorded the phantom default; filterPhantomSessions must strip it before fold")
	}

	// Rebuild the exact report the dialog rendered and assert both fixes in its
	// rendered sections.
	report := w.overallLifetime.fold(filterPhantomSessions(w.handlers.GetStatistics()))

	// #278: no phantom default row, and the Overview session count equals the open
	// window count (1: s2) — NOT the backend count, which would include "default".
	for _, s := range report.Sessions {
		if s.ID == phantomDefaultSessionID {
			t.Errorf("phantom default leaked into the dialog's Sessions: %+v", report.Sessions)
		}
	}
	if report.Totals.Sessions != 1 {
		t.Errorf("Overview Sessions = %d, want 1 (open s2; default filtered, closed s1 not counted)", report.Totals.Sessions)
	}

	// #277: the closed s1 survives as a per-session row with its last-known tally, and
	// its traffic persists in the grand totals.
	var s1row *stats.SessionRow
	for i := range report.Sessions {
		if report.Sessions[i].ID == "s1" {
			s1row = &report.Sessions[i]
		}
	}
	if s1row == nil {
		t.Fatalf("closed session s1 missing from the dialog's Sessions: %+v", report.Sessions)
	}
	if s1row.Primary.Requests != 100 || s1row.Primary.TokensIn != 1111 {
		t.Errorf("closed s1 row = %+v, want {Req:100 In:1111}", s1row.Primary)
	}
	// Lifetime grand totals = closed s1 (100/1111) + open s2 (23/222); the phantom
	// default (7/70) is excluded.
	if report.Totals.Primary.Requests != 123 || report.Totals.Primary.TokensIn != 1333 {
		t.Errorf("dialog grand totals = %+v, want {Req:123 In:1333} (s1+s2, default excluded)", report.Totals.Primary)
	}

	// Text-level proof through the dialog's own renderStatistics: the Overview shows
	// the lifetime request total (123), and the Sessions section lists the closed s1
	// but no "default" row.
	overview := renderStatistics(statsOverview, report)
	if !strings.Contains(overview, "123") {
		t.Errorf("Overview missing lifetime request total 123:\n%s", overview)
	}
	sessionsView := renderStatistics(statsSessions, report)
	if !strings.Contains(sessionsView, "s1") {
		t.Errorf("Sessions view missing the closed session s1:\n%s", sessionsView)
	}
	if strings.Contains(sessionsView, phantomDefaultSessionID) {
		t.Errorf("Sessions view shows the phantom default row:\n%s", sessionsView)
	}
}

// TestLifetimeFold_ModelSwitchingKeepsAggregate covers the in-session model-switch
// edge case: a session that changes PrimaryModel mid-life must not lose tokens
// from the lifetime aggregate, and must not be double-counted across models. Its
// full connector is attributed to its FINAL model (matching the live backend,
// which keys per-model attribution on the session's current model).
func TestLifetimeFold_ModelSwitchingKeepsAggregate(t *testing.T) {
	ls := newLifetimeStats()
	// s1 starts on modelA.
	ls.fold(buildReport(sessSpec{id: "s1", primaryModel: "A", primary: conn(10, 0, 1000, 0, 100)}))
	// s1 switches to modelB and burns more tokens (connector is cumulative).
	ls.fold(buildReport(sessSpec{id: "s1", primaryModel: "B", primary: conn(25, 1, 2500, 0, 250)}))
	// s1 closes.
	got := ls.fold(buildReport())

	// Aggregate is model-agnostic: the full cumulative connector survives.
	if got.Totals.Primary.Requests != 25 || got.Totals.Primary.TokensIn != 2500 {
		t.Errorf("aggregate after model switch = %+v, want {Req:25 In:2500} (no loss)", got.Totals.Primary)
	}
	// Per-model: everything attributes to the final model B; A gets nothing.
	b, okB := got.ModelByName("B")
	if !okB {
		t.Fatal("final model B row missing after switch+close")
	}
	if b.Connector.Requests != 25 || b.Connector.TokensIn != 2500 {
		t.Errorf("model B connector = %+v, want the full {Req:25 In:2500}", b.Connector)
	}
	if _, okA := got.ModelByName("A"); okA {
		t.Error("model A should not appear (session's earlier A usage is reattributed to its final model B, matching live)")
	}
	// No token is lost or double-counted across the switch: the sum of every
	// per-model connector equals the aggregate primary connector.
	var sum stats.ConnectorStat
	for _, m := range got.Models {
		sum = sum.Add(m.Connector)
	}
	if sum != got.Totals.Primary {
		t.Errorf("sum of per-model connectors %+v != aggregate %+v (leakage/double-count across model switch)", sum, got.Totals.Primary)
	}
}

// TestWorkbench_SelectedClosedOnlyModelShowsLifetime is the end-to-end version of
// PerModelAllClosedSurfacesRow: through the real Workbench, selecting a model
// whose sessions have ALL closed still renders that model's lifetime traffic in
// the Overall panel (the model does not vanish from the selector, and scoping to
// it shows the accumulated connector, not zeros).
func TestWorkbench_SelectedClosedOnlyModelShowsLifetime(t *testing.T) {
	w := newTestWorkbench(t)
	w.SetModels([]*config.ModelConfig{
		{Name: "glm", DisplayName: "GLM"},
		{Name: "groq", DisplayName: "Groq"},
	})

	// One session on glm, then it closes.
	w.handlers.GetStatistics = func() stats.Report {
		return buildReport(sessSpec{id: "s1", primaryModel: "glm", primary: conn(10, 1, 1000, 100, 200)})
	}
	w.refreshOverall()
	w.handlers.GetStatistics = func() stats.Report { return buildReport() } // glm fully closed
	w.refreshOverall()

	// Select the now-closed-only model and refresh through the real path.
	w.sidebar.setSelectedOverallModel("glm")
	w.refreshOverall()

	got := w.sidebar.overall
	if got.Requests != 10 || got.TokensIn != 1000 || got.TokensOut != 200 || got.Errors != 1 {
		t.Errorf("selected closed-only glm = %+v, want lifetime {Req:10 In:1000 Out:200 Err:1}", got)
	}
	if got.CacheHitPct != 10 { // 100/1000
		t.Errorf("selected closed-only glm CacheHitPct = %d, want 10", got.CacheHitPct)
	}
	// The model/api rows still describe the selected model's backend even with no
	// open session (resolved from config, not from the live session map).
	if got.Model != "GLM" {
		t.Errorf("Model = %q, want GLM (resolved from config for the closed-only selection)", got.Model)
	}
}

// TestLifetimeFold_CacheHitTruncatesNotRounds pins that the recomputed cache-hit
// percentage truncates (int() conversion) rather than rounds, and that it is
// recomputed from the SUMMED cached/total tokens across sessions — not averaged
// per-session.
func TestLifetimeFold_CacheHitTruncatesNotRounds(t *testing.T) {
	for _, tc := range []struct {
		name       string
		cached, in int
		want       int
	}{
		{"third truncates down", 1, 3, 33},
		{"two thirds truncates down", 2, 3, 66},
		{"sixth truncates down", 1, 6, 16},
		{"exact half", 1, 2, 50},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ls := newLifetimeStats()
			ls.fold(buildReport(sessSpec{id: "s1", primaryModel: "glm",
				primary: stats.ConnectorStat{Requests: 1, TokensIn: tc.in, CachedTokensIn: tc.cached}}))
			got := agg(ls.fold(buildReport())) // close
			if got.CacheHitPct != tc.want {
				t.Errorf("cached=%d in=%d: CacheHitPct = %d, want %d (truncated, not rounded)", tc.cached, tc.in, got.CacheHitPct, tc.want)
			}
		})
	}

	// Recompute-from-sum, not average-of-per-session: s1 closed (2/3 = 66%) and
	// s2 open (1/3 = 33%) => lifetime 3/6 = 50%, NOT the per-session average
	// (66+33)/2 = 49.
	ls := newLifetimeStats()
	ls.fold(buildReport(sessSpec{id: "s1", primaryModel: "glm",
		primary: stats.ConnectorStat{Requests: 1, TokensIn: 3, CachedTokensIn: 2}}))
	got := agg(ls.fold(buildReport(sessSpec{id: "s2", primaryModel: "glm",
		primary: stats.ConnectorStat{Requests: 1, TokensIn: 3, CachedTokensIn: 1}})))
	if got.CacheHitPct != 50 {
		t.Errorf("mixed cache-hit = %d, want 50 (3/6 recomputed from summed tokens, not averaged to 49)", got.CacheHitPct)
	}
}

// TestMergeModelLifetime_SumEqualsAggregateWhenAllModeled is a structural
// invariant: when every remembered session has a non-empty primary model, the
// sum of the per-model connectors fed back by mergeModelLifetime must equal the
// aggregate primary connector those sessions contributed — no traffic silently
// dropped or counted twice. (A session with an empty model intentionally bypasses
// the per-model map, covered by SessionWithNoModelPersistsInAggregate.)
func TestMergeModelLifetime_SumEqualsAggregateWhenAllModeled(t *testing.T) {
	extra := map[string]stats.ConnectorStat{
		"glm":  conn(10, 1, 1000, 0, 100),
		"groq": conn(20, 2, 2000, 0, 200),
	}
	var aggregate stats.ConnectorStat
	for _, c := range extra {
		aggregate = aggregate.Add(c)
	}
	out := mergeModelLifetime(nil, extra)
	if len(out) != 2 {
		t.Fatalf("got %d model rows, want 2", len(out))
	}
	var sum stats.ConnectorStat
	for _, m := range out {
		sum = sum.Add(m.Connector)
	}
	if sum != aggregate {
		t.Errorf("sum of merged connectors %+v != aggregate %+v (traffic lost or double-counted)", sum, aggregate)
	}
}
