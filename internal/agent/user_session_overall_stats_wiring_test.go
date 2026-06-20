package agent

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"gogent/internal/model"
)

// These tests exercise the REAL wiring the unit tests in
// user_session_overall_stats_test.go bypass: they drive the model loop through a
// fake HTTP backend so recordConnectorUsage is reached via modelRoundTrip (the
// single choke point the root loop AND every sub-agent share), and they drive a
// real ModelSession.Resume/Carry. They pin the load-bearing assumptions behind
// issue #191's "no double-count across sub-agents" and "monotonic across model
// switch" claims — namely that a sub-agent shares the parent's connector pointer
// and that the carried baseline produces a zero delta.

// TestOverallConnectorStats_RealRoundTripFeedsAccumulator proves the accumulator
// is actually fed by a real model round-trip (via modelRoundTrip), not just by a
// direct call to recordConnectorUsage: after one root turn against a fake
// backend, ConnectorStats must reflect the connector's real counters exactly.
func TestOverallConnectorStats_RealRoundTripFeedsAccumulator(t *testing.T) {
	fs := &fakeServer{responses: []map[string]interface{}{finalResponse("done")}}
	srv := httptest.NewServer(http.HandlerFunc(fs.handler))
	defer srv.Close()

	us, ag := newLoopSession(t, srv.URL)
	us.SetPrimaryModel("glm")

	if _, err := us.ExecuteTaskLoop(context.Background(), "root", "hi"); err != nil {
		t.Fatalf("ExecuteTaskLoop: %v", err)
	}

	got := us.ConnectorStats()
	if got.RequestCount != 1 {
		t.Fatalf("ConnectorStats.RequestCount = %d after one real round-trip, want 1 (modelRoundTrip must feed the accumulator)", got.RequestCount)
	}
	// finalResponse reports prompt_tokens=20; that must land in the accumulator.
	if got.TotalTokensIn != 20 {
		t.Errorf("TotalTokensIn = %d, want 20 (the fake response's prompt_tokens)", got.TotalTokensIn)
	}
	// The accumulator must equal the live connector's snapshot (one model, no
	// resets) — i.e. the panel reads the same numbers the connector holds.
	live := ag.ThoughtTrain.Model.StatsSnapshot()
	if got.RequestCount != live.RequestCount || got.TotalTokensIn != live.TotalTokensIn {
		t.Errorf("accumulator %+v diverges from live connector %+v (should match for a single model)", got, live)
	}
}

// TestOverallConnectorStats_RealSubAgentSharedConnectorNoDoubleCount is the
// end-to-end version of the no-double-count test. A root agent plus two one-shot
// sub-agents all send through the SAME shared connector (the wiring
// newSubAgent/SpawnSubAgent set up). The accumulator must total exactly the
// connector's final value — NOT N× it (the old per-agent ConnectorStats summed
// the shared connector once per agent in the tree).
func TestOverallConnectorStats_RealSubAgentSharedConnectorNoDoubleCount(t *testing.T) {
	// Plenty of canned final answers; each round-trip consumes one.
	resps := make([]map[string]interface{}, 0, 8)
	for i := 0; i < 8; i++ {
		resps = append(resps, finalResponse("ok"))
	}
	fs := &fakeServer{responses: resps}
	srv := httptest.NewServer(http.HandlerFunc(fs.handler))
	defer srv.Close()

	us, ag := newLoopSession(t, srv.URL)
	us.SetPrimaryModel("glm")
	rootConn := ag.ThoughtTrain.Model

	ctx := context.Background()
	if _, err := us.ExecuteTaskLoop(ctx, "root", "hi"); err != nil {
		t.Fatalf("root loop: %v", err)
	}
	if _, err := us.SpawnSubAgent(ctx, "root", "sub1", "do thing", true); err != nil {
		t.Fatalf("SpawnSubAgent sub1: %v", err)
	}
	if _, err := us.SpawnSubAgent(ctx, "root", "sub2", "do thing", true); err != nil {
		t.Fatalf("SpawnSubAgent sub2: %v", err)
	}

	// Load-bearing assumption: the sub-agents really do share the root's
	// connector pointer. If they didn't, this whole accounting scheme breaks.
	agents := ag.ListAllAgents()
	if len(agents) != 3 {
		t.Fatalf("agent tree has %d agents, want 3 (root + 2 sub-agents)", len(agents))
	}
	for i, a := range agents {
		if a.ThoughtTrain == nil || a.ThoughtTrain.Model != rootConn {
			t.Errorf("agent %d (%s) does not share the root connector — the no-double-count scheme depends on this", i, a.ID)
		}
	}

	live := rootConn.StatsSnapshot()
	got := us.ConnectorStats()
	// Each of the 3 agents made one request through the shared connector, so the
	// connector's RequestCount is 3. The accumulator must be exactly 3 — the old
	// code would have summed 3 (agents) × 3 (shared count) = 9.
	if got.RequestCount != live.RequestCount {
		t.Errorf("ConnectorStats.RequestCount = %d, want exactly the shared connector's %d "+
			"(old per-agent sum would give %d for a 3-agent tree) — double-count via real modelRoundTrip",
			got.RequestCount, live.RequestCount, live.RequestCount*len(agents))
	}
	if got.TotalTokensIn != live.TotalTokensIn {
		t.Errorf("ConnectorStats.TotalTokensIn = %d, want shared connector's %d", got.TotalTokensIn, live.TotalTokensIn)
	}
}

// TestOverallConnectorStats_RealResumeCarryNotDoubleCounted drives a REAL
// ModelSession.Resume (which Carries the outgoing connector's totals into the
// incoming one) rather than hand-faking the carried baseline. The carried base
// must produce a zero delta (it is already attributed), and only subsequent
// growth is added — the cumulative-across-switches guarantee.
func TestOverallConnectorStats_RealResumeCarryNotDoubleCounted(t *testing.T) {
	us := newOverallSession(t)
	us.SetPrimaryModel("A")

	c1 := model.NewModelConnection()
	c1.Stats.RequestCount = 2
	c1.Stats.TotalTokensIn = 200
	c1.Stats.TotalTokensOut = 40
	sess := model.NewModelSession("t", c1)
	us.recordConnectorUsage(c1) // baseline: perModelConn[A] = {2,200}

	// The per-turn rebuild path: a fresh connector swapped in via Resume, which
	// Carries c1's snapshot into c2.
	c2 := model.NewModelConnection()
	sess.Resume(c2)
	// c2 now holds the carried {2,200}; reading it must yield a ZERO delta.
	us.recordConnectorUsage(c2)
	if got := us.ModelConnectorStats("A"); got.RequestCount != 2 || got.TotalTokensIn != 200 {
		t.Errorf("after carried Resume: A = %+v, want {Req:2 In:200} (carried base must not be re-counted)", got)
	}

	// Subsequent activity on c2 is attributed as a delta on top.
	c2.Stats.RequestCount = 3
	c2.Stats.TotalTokensIn = 350
	us.recordConnectorUsage(c2)
	got := us.ConnectorStats()
	if got.RequestCount != 3 || got.TotalTokensIn != 350 {
		t.Errorf("after carry + new work: %+v, want cumulative {Req:3 In:350}", got)
	}
}

// TestOverallConnectorStats_ConcurrentNoDoubleCount is the regression guard for
// the TOCTOU fix in recordConnectorUsage: sub-agents share the root's connector
// and run concurrently (one-shot fan-out via RunSubAgentsBounded, interactive
// agents in their own goroutine), so recordConnectorUsage is called from many
// goroutines on the same *s and the same connector. Reading the snapshot UNDER
// s.mu (the fix) serializes each read+update so the folded deltas sum exactly to
// the connector's final value. Reading it outside the lock let a stale snapshot
// rewind lastConnSnap and double-count the next delta.
//
// The assertion is deterministic on the fixed code: because every simulated
// request increments the shared connector before its own recordConnectorUsage
// call, the last call (in lock-acquisition order) observes every increment, so
// Σ(delta) == N exactly — never more. (go test -race is out of scope per the
// task; this tests the logic outcome, not the memory race.)
func TestOverallConnectorStats_ConcurrentNoDoubleCount(t *testing.T) {
	us := newOverallSession(t)
	us.SetPrimaryModel("glm")
	conn := model.NewModelConnection()

	const N = 80
	var wg sync.WaitGroup
	wg.Add(N)
	start := make(chan struct{})
	for i := 0; i < N; i++ {
		go func() {
			defer wg.Done()
			<-start // maximize overlap so interleavings are likely
			// One request's effect on the shared connector, then fold via the
			// real choke point exactly as modelRoundTrip does.
			conn.Stats.Mutex.Lock()
			conn.Stats.RequestCount++
			conn.Stats.TotalTokensIn += 10
			conn.Stats.TotalTokensOut += 2
			conn.Stats.Mutex.Unlock()
			us.recordConnectorUsage(conn)
		}()
	}
	close(start) // release all goroutines at once
	wg.Wait()

	got := us.ConnectorStats()
	if got.RequestCount != N {
		t.Errorf("concurrent RequestCount = %d, want exactly %d — double-count under concurrency "+
			"(recordConnectorUsage must read the snapshot under s.mu so lastConnSnap cannot rewind)", got.RequestCount, N)
	}
	if got.TotalTokensIn != 10*N || got.TotalTokensOut != 2*N {
		t.Errorf("concurrent tokens = in %d out %d, want in %d out %d", got.TotalTokensIn, got.TotalTokensOut, 10*N, 2*N)
	}
	// The accumulator must equal the shared connector's final snapshot exactly.
	if live := conn.StatsSnapshot(); got != live {
		t.Errorf("accumulator %+v != live connector %+v under concurrency (must match exactly)", got, live)
	}
}
