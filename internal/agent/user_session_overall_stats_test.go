package agent

import (
	"testing"

	"gogent/internal/model"
)

// fakeStatsReporter is a controllable stand-in for *model.ModelConnection so the
// per-turn-rebuilt connector (issue #191) can be modelled in a unit test: each
// call to recordConnectorUsage reads whatever snapshot the test has programmed,
// letting us simulate counters growing within a turn, a connector being rebuilt
// (zeroed), or a model switch with/without a Carry.
type fakeStatsReporter struct {
	snap model.StatsSnapshot
}

func (f *fakeStatsReporter) GetStats() *model.ModelStats { return nil }
func (f *fakeStatsReporter) StatsSnapshot() model.StatsSnapshot {
	return f.snap
}

// newOverallSession builds a session wired like the loop tests use, with a
// placeholder connector that is never actually sent to.
func newOverallSession(t *testing.T) *UserSession {
	t.Helper()
	us, _ := newLoopSession(t, "http://unused")
	return us
}

// TestOverallConnectorStats_StableAcrossTurns pins the core #191 bug fix: the
// grand-total connector stats must grow monotonically turn over turn and end up
// equal to the connector's final snapshot. The previous implementation read the
// live, per-turn-rebuilt connector, so the panel snapped back near zero every
// turn; the stable per-model accumulator must not.
func TestOverallConnectorStats_StableAcrossTurns(t *testing.T) {
	us := newOverallSession(t)
	us.SetPrimaryModel("glm")

	conn := &fakeStatsReporter{}
	turns := []model.StatsSnapshot{
		{RequestCount: 1, SuccessCount: 1, TotalTokensIn: 100, TotalTokensOut: 20, TotalCachedTokensIn: 0},
		{RequestCount: 2, SuccessCount: 2, TotalTokensIn: 250, TotalTokensOut: 60, TotalCachedTokensIn: 50},
		{RequestCount: 3, SuccessCount: 3, TotalTokensIn: 400, TotalTokensOut: 90, TotalCachedTokensIn: 100},
	}

	var prev model.StatsSnapshot
	for i, snap := range turns {
		conn.snap = snap
		us.recordConnectorUsage(conn)
		got := us.ConnectorStats()
		if got.RequestCount < prev.RequestCount || got.TotalTokensIn < prev.TotalTokensIn {
			t.Fatalf("turn %d: stats regressed prev=%+v got=%+v (must be monotonic)", i+1, prev, got)
		}
		prev = got
	}

	got := us.ConnectorStats()
	want := model.StatsSnapshot{RequestCount: 3, SuccessCount: 3, TotalTokensIn: 400, TotalTokensOut: 90, TotalCachedTokensIn: 100}
	if got.RequestCount != want.RequestCount || got.TotalTokensIn != want.TotalTokensIn ||
		got.TotalTokensOut != want.TotalTokensOut || got.TotalCachedTokensIn != want.TotalCachedTokensIn {
		t.Fatalf("ConnectorStats = %+v, want the final connector snapshot %+v (no reset)", got, want)
	}
	// The per-model bucket carries the same total.
	if mc := us.ModelConnectorStats("glm"); mc != got {
		t.Errorf("ModelConnectorStats(glm) = %+v, want %+v", mc, got)
	}
	// A model that was never used reports zero, not garbage.
	if mc := us.ModelConnectorStats("never-used"); mc != (model.StatsSnapshot{}) {
		t.Errorf("ModelConnectorStats(never-used) = %+v, want zero", mc)
	}
}

// TestOverallConnectorStats_NoDoubleCountOnSharedConnector pins the second #191
// acceptance item: sub-agents share the parent's connector pointer, and the old
// ConnectorStats summed every agent's live connector, so a tree of N agents
// holding the shared connector inflated the total to N×. Reading the connector
// once per round-trip (in modelRoundTrip) and folding only the per-read delta
// must attribute each unit of work exactly once: the grand total equals the
// connector's final snapshot regardless of how many agents shared it.
func TestOverallConnectorStats_NoDoubleCountOnSharedConnector(t *testing.T) {
	us := newOverallSession(t)
	us.SetPrimaryModel("glm")

	// One connector object shared by the root agent and its sub-agents (exactly
	// as newSubAgent wires it: childSess := model.NewModelSession(id, parent.Model)).
	shared := &fakeStatsReporter{}

	// Root round-trip, then two sub-agent round-trips, then root again — all
	// reading the SAME connector as it grows.
	steps := []model.StatsSnapshot{
		{RequestCount: 1, TotalTokensIn: 100, TotalTokensOut: 20},  // root
		{RequestCount: 2, TotalTokensIn: 250, TotalTokensOut: 60},  // sub-agent A
		{RequestCount: 3, TotalTokensIn: 300, TotalTokensOut: 90},  // sub-agent B
		{RequestCount: 4, TotalTokensIn: 500, TotalTokensOut: 120}, // root again
	}
	for _, snap := range steps {
		shared.snap = snap
		us.recordConnectorUsage(shared)
	}

	got := us.ConnectorStats()
	// Exactly the connector's final value — NOT 4×4/2000 (the per-agent sum the
	// old code produced for a 4-agent tree) and NOT 2× either.
	if got.RequestCount != 4 || got.TotalTokensIn != 500 || got.TotalTokensOut != 120 {
		t.Fatalf("shared-connector total = %+v, want exactly the final snapshot "+
			"{Req:4 In:500 Out:120} (no double-count across sub-agents)", got)
	}
}

// TestOverallConnectorStats_ModelSwitchWithCarry covers a model switch where the
// new connector starts from the carried baseline (the production path: Resume
// Carries the outgoing connector's totals into the incoming one). The carried
// base must produce a zero delta — it is already attributed to the old model —
// and only the genuinely new activity is attributed to the new model. The grand
// total stays monotonic across the switch with no reset and no double-count.
func TestOverallConnectorStats_ModelSwitchWithCarry(t *testing.T) {
	us := newOverallSession(t)

	// Model A does two requests / 200 tokens in.
	us.SetPrimaryModel("A")
	connA := &fakeStatsReporter{snap: model.StatsSnapshot{RequestCount: 2, TotalTokensIn: 200, TotalTokensOut: 40}}
	us.recordConnectorUsage(connA)

	// Switch to model B. The new connector is rebuilt+carried, so it begins at
	// A's last snapshot (2/200) and then grows by one request / 150 tokens.
	us.SetPrimaryModel("B")
	connB := &fakeStatsReporter{snap: model.StatsSnapshot{RequestCount: 3, TotalTokensIn: 350, TotalTokensOut: 90}}
	us.recordConnectorUsage(connB)

	// Per-model attribution: A keeps its own 2/200; B gets only the delta 1/150
	// (the carried 2/200 is NOT re-counted against B).
	if got := us.ModelConnectorStats("A"); got.RequestCount != 2 || got.TotalTokensIn != 200 {
		t.Errorf("model A bucket = %+v, want {Req:2 In:200}", got)
	}
	if got := us.ModelConnectorStats("B"); got.RequestCount != 1 || got.TotalTokensIn != 150 {
		t.Errorf("model B bucket = %+v, want only its delta {Req:1 In:150} (carried base not double-counted)", got)
	}
	// Grand total is the cumulative connector value (3/350), monotonic, no reset.
	if got := us.ConnectorStats(); got.RequestCount != 3 || got.TotalTokensIn != 350 {
		t.Errorf("grand total after switch = %+v, want cumulative {Req:3 In:350}", got)
	}
}

// TestOverallConnectorStats_ResetToZeroKeepsHistory models the documented
// "connector rebuilt/zeroed between reads" case (recordConnectorUsage says it
// treats a negative delta as a fresh baseline so the accumulator "only ever
// grows"). After the connector fully resets to zero and then records one fresh
// request, the accumulator must RETAIN the pre-reset totals and add the new
// request — i.e. {5 req / 1000 tok} + {1 req / 50 tok} = {6 req / 1050 tok}.
func TestOverallConnectorStats_ResetToZeroKeepsHistory(t *testing.T) {
	us := newOverallSession(t)
	us.SetPrimaryModel("A")

	c1 := &fakeStatsReporter{snap: model.StatsSnapshot{RequestCount: 5, TotalTokensIn: 1000, TotalTokensOut: 200}}
	us.recordConnectorUsage(c1)

	// Connector rebuilt to zero, then one small request lands on it.
	c2 := &fakeStatsReporter{snap: model.StatsSnapshot{RequestCount: 1, TotalTokensIn: 50, TotalTokensOut: 10}}
	us.recordConnectorUsage(c2)

	got := us.ConnectorStats()
	// Prior totals retained + new activity added.
	if got.RequestCount != 6 || got.TotalTokensIn != 1050 || got.TotalTokensOut != 210 {
		t.Errorf("after zero-reset+new request: %+v, want retained+new {Req:6 In:1050 Out:210}", got)
	}
}

// TestOverallConnectorStats_NarrowGuardShrinksAccumulator reveals a DEFECT in
// recordConnectorUsage's "fresh baseline" guard. The guard only fires when
// delta.RequestCount < 0. But a no-carry rebuild whose request count RECOVERS to
// the previous level (a fresh connector that has done exactly as many requests
// as the old one, but with fewer tokens) yields delta.RequestCount == 0, so the
// guard does NOT fire and the negative token delta is applied directly. The
// accumulator then SHRINKS instead of growing, contradicting the function's own
// documented contract ("the accumulator only ever grows") and under-counting
// requests (1 instead of 2).
//
// This scenario is latent under the current code because ModelSession.Resume
// always Carries, keeping the connector monotonic; it activates if Carry is ever
// removed/made conditional, or along any code path that swaps in a zeroed
// connector. The test asserts the documented monotonicity invariant regardless.
func TestOverallConnectorStats_NarrowGuardShrinksAccumulator(t *testing.T) {
	us := newOverallSession(t)
	us.SetPrimaryModel("A")

	// One big request on the old connector: 1 request, 1000 tokens.
	c1 := &fakeStatsReporter{snap: model.StatsSnapshot{RequestCount: 1, TotalTokensIn: 1000}}
	us.recordConnectorUsage(c1)
	before := us.ConnectorStats()
	if before.TotalTokensIn != 1000 || before.RequestCount != 1 {
		t.Fatalf("setup: %+v, want {Req:1 In:1000}", before)
	}

	// Connector rebuilt (no carry) and does one SMALL request: request count
	// recovers to 1 (== previous), but tokens are far lower.
	c2 := &fakeStatsReporter{snap: model.StatsSnapshot{RequestCount: 1, TotalTokensIn: 50}}
	us.recordConnectorUsage(c2)

	after := us.ConnectorStats()
	// The accumulator must be monotonic — it must never lose prior totals.
	if after.TotalTokensIn < before.TotalTokensIn {
		t.Errorf("DEFECT: accumulator shrank across a no-carry rebuild: "+
			"TotalTokensIn %d -> %d (recordConnectorUsage promises it only ever grows); "+
			"the <0 guard misses delta.RequestCount == 0 with negative token deltas",
			before.TotalTokensIn, after.TotalTokensIn)
	}
	// And both requests must be counted (true cumulative is 2 requests / 1050 tokens).
	if after.RequestCount != 2 {
		t.Errorf("DEFECT: request count = %d after a second request, want 2 "+
			"(the recovered-count rebuild was treated as zero new requests)", after.RequestCount)
	}
}

// TestOverallConnectorStats_ReadsAccumulatorNotLiveConnector proves ConnectorStats
// no longer reflects mutations to the live connector the way the old per-agent
// sum did. It reads the stable per-model accumulator, so connector activity that
// has not been folded in via a round-trip must NOT appear in the total. (This is
// what makes the panel immune to the rebuilt-and-zeroed live connector.)
func TestOverallConnectorStats_ReadsAccumulatorNotLiveConnector(t *testing.T) {
	us := newOverallSession(t)
	us.SetPrimaryModel("glm")

	conn := &fakeStatsReporter{snap: model.StatsSnapshot{RequestCount: 1, TotalTokensIn: 100}}
	us.recordConnectorUsage(conn)

	recorded := us.ConnectorStats()
	// Mutate the live connector heavily WITHOUT going through recordConnectorUsage
	// (i.e. no round-trip folded it in).
	conn.snap = model.StatsSnapshot{RequestCount: 99, TotalTokensIn: 9999, ErrorCount: 7}

	got := us.ConnectorStats()
	if got != recorded {
		t.Errorf("ConnectorStats tracked a live, unrecorded connector mutation: "+
			"got %+v after mutating live connector, want to stay at the recorded %+v "+
			"(must read the stable accumulator, not the live connector)", got, recorded)
	}
}

// TestOverallConnectorStats_PerModelAttributionWithTokens joins the connector
// accumulator with the session-layer token attribution (AddTokenUsage): after
// activity on two models, each model's PerModelStats row carries its own tokens
// and its own connector delta, and the connector rows sum to the grand total.
func TestOverallConnectorStats_PerModelAttributionWithTokens(t *testing.T) {
	us := newOverallSession(t)

	// Model "alpha": 100 tokens in + a connector burst of 1 req / 100 in.
	us.SetPrimaryModel("alpha")
	us.AddTokenUsage(100, 20)
	us.recordConnectorUsage(&fakeStatsReporter{snap: model.StatsSnapshot{RequestCount: 1, TotalTokensIn: 100, TotalTokensOut: 20}})

	// Model "beta": only token attribution, no connector activity recorded.
	us.SetPrimaryModel("beta")
	us.AddTokenUsage(50, 10)

	rows := us.PerModelStats()
	if len(rows) != 2 {
		t.Fatalf("PerModelStats = %d rows, want 2", len(rows))
	}
	// Sorted by name: alpha, beta.
	if rows[0].Name != "alpha" || rows[1].Name != "beta" {
		t.Fatalf("PerModelStats order = %q,%q, want alpha,beta (sorted)", rows[0].Name, rows[1].Name)
	}
	if rows[0].TokensIn != 100 || rows[0].TokensOut != 20 {
		t.Errorf("alpha tokens = %d/%d, want 100/20", rows[0].TokensIn, rows[0].TokensOut)
	}
	if rows[0].Connector.RequestCount != 1 || rows[0].Connector.TotalTokensIn != 100 {
		t.Errorf("alpha connector = %+v, want {Req:1 In:100}", rows[0].Connector)
	}
	if rows[1].TokensIn != 50 || rows[1].TokensOut != 10 {
		t.Errorf("beta tokens = %d/%d, want 50/10", rows[1].TokensIn, rows[1].TokensOut)
	}
	if rows[1].Connector != (model.StatsSnapshot{}) {
		t.Errorf("beta connector = %+v, want zero (no connector activity attributed)", rows[1].Connector)
	}

	// Invariant the Statistics report relies on: the per-model connector buckets
	// sum back to the grand total.
	var sum model.StatsSnapshot
	for _, r := range rows {
		sum = sum.Add(r.Connector)
	}
	if grand := us.ConnectorStats(); sum != grand {
		t.Errorf("sum of PerModelStats connectors = %+v, want grand ConnectorStats %+v", sum, grand)
	}
}

// TestOverallConnectorStats_PerModelOrderStable checks the sort is by name and
// that a model with ONLY connector activity (no tokens) still appears.
func TestOverallConnectorStats_PerModelOrderStable(t *testing.T) {
	us := newOverallSession(t)

	us.SetPrimaryModel("zeta")
	// Reset the delta baseline so the connector delta is attributed cleanly.
	us.lastConnSnap = model.StatsSnapshot{}
	us.recordConnectorUsage(&fakeStatsReporter{snap: model.StatsSnapshot{RequestCount: 2, TotalTokensIn: 40}})

	us.SetPrimaryModel("alpha")
	us.lastConnSnap = model.StatsSnapshot{}
	us.recordConnectorUsage(&fakeStatsReporter{snap: model.StatsSnapshot{RequestCount: 1, TotalTokensIn: 10}})

	rows := us.PerModelStats()
	if len(rows) != 2 {
		t.Fatalf("PerModelStats = %d rows, want 2", len(rows))
	}
	if rows[0].Name != "alpha" || rows[1].Name != "zeta" {
		t.Fatalf("PerModelStats order = %q,%q, want alpha,zeta (sorted by name)", rows[0].Name, rows[1].Name)
	}
	// zeta has connector activity but no tokens.
	var zeta *ModelUsage
	for i := range rows {
		if rows[i].Name == "zeta" {
			zeta = &rows[i]
		}
	}
	if zeta == nil || zeta.Connector.RequestCount != 2 || zeta.TokensIn != 0 {
		t.Errorf("zeta row = %+v, want connector-only {Req:2 TokensIn:0}", zeta)
	}
}

// TestOverallConnectorStats_EmptyPrimaryModelBucket documents an asymmetry between
// the connector accumulator and the token accumulator. AddTokenUsage attributes
// to perModelTokens ONLY when a primary model is set (usage before that lands
// only in the session grand totals). recordConnectorUsage, by contrast, always
// writes to perModelConn[s.primaryModel], so activity before SetPrimaryModel
// lands in the "" bucket. Both feed the grand total correctly; only the
// per-model breakdown differs for the pre-selection phase.
func TestOverallConnectorStats_EmptyPrimaryModelBucket(t *testing.T) {
	us := newOverallSession(t)
	// primaryModel is "" here.
	us.recordConnectorUsage(&fakeStatsReporter{snap: model.StatsSnapshot{RequestCount: 1, TotalTokensIn: 100}})

	// The "" bucket holds the activity.
	if got := us.perModelConn[""]; got.RequestCount != 1 || got.TotalTokensIn != 100 {
		t.Errorf("empty-key bucket = %+v, want {Req:1 In:100}", got)
	}
	// Grand total includes it (ModelConnectorStats("") sums every bucket).
	if got := us.ConnectorStats(); got.RequestCount != 1 || got.TotalTokensIn != 100 {
		t.Errorf("grand total = %+v, want {Req:1 In:100}", got)
	}
	// PerModelStats surfaces a "" row carrying the connector but no tokens
	// (because AddTokenUsage skipped perModelTokens before SetPrimaryModel).
	rows := us.PerModelStats()
	if len(rows) != 1 || rows[0].Name != "" {
		t.Fatalf("PerModelStats = %+v, want one empty-name row", rows)
	}
	if rows[0].Connector.RequestCount != 1 || rows[0].TokensIn != 0 {
		t.Errorf("empty-name row = %+v, want connector-only {Req:1 TokensIn:0}", rows[0])
	}
}

// TestRecordConnectorUsage_NilIsNoOp ensures a nil connector (e.g. a session
// before its model is wired) cannot panic or perturb the accumulator.
func TestRecordConnectorUsage_NilIsNoOp(t *testing.T) {
	us := newOverallSession(t)
	us.recordConnectorUsage(nil)
	if got := us.ConnectorStats(); got != (model.StatsSnapshot{}) {
		t.Errorf("after nil record, ConnectorStats = %+v, want zero", got)
	}
}

// TestSubAgentCount covers the helper Statistics() uses to attribute a session's
// sub-agents to its primary model: it is every agent in the tree except the root,
// recursing through nested sub-agents.
func TestSubAgentCount(t *testing.T) {
	sess := model.NewModelSession("t", model.NewModelConnection())
	root := NewAgent("root", sess)
	us := NewUserSession("s", root)

	if got := us.SubAgentCount(); got != 0 {
		t.Fatalf("SubAgentCount with no children = %d, want 0", got)
	}

	childA := NewAgent("a", sess)
	childB := NewAgent("b", sess)
	root.AddSubAgent(childA)
	root.AddSubAgent(childB)
	if got := us.SubAgentCount(); got != 2 {
		t.Errorf("SubAgentCount with two children = %d, want 2", got)
	}

	// A nested grandchild is counted too (ListAllAgents recurses).
	grand := NewAgent("g", sess)
	childA.AddSubAgent(grand)
	if got := us.SubAgentCount(); got != 3 {
		t.Errorf("SubAgentCount with a nested grandchild = %d, want 3", got)
	}
}
