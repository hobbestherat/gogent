package model

import "testing"

// TestModelSessionResumeRecomputesOnModelSwitch covers the fix for issue #3:
// the token-count recompute inside Resume must actually run when the model
// backend changes. Previously the guard compared newModel against s.Model
// *after* assigning it, so the branch was unreachable dead code and the count
// stayed stale across a model switch.
//
// The recompute takes the LATEST turn's usage total, not the sum of every
// turn's total: a turn's TotalTokens is the size of the entire context at that
// turn (the whole conversation is re-sent on every request), so the newest one
// supersedes its predecessors. Summing double-counts the growing prefix and was
// the root cause of premature context compaction (the session compacted at
// ~10% real usage on large-window models).
func TestModelSessionResumeRecomputesOnModelSwitch(t *testing.T) {
	first := newPlaceholderConnection()
	s := NewModelSession("t", first)

	// Record two turns with known usage. The second turn's total already covers
	// the whole conversation, so Resume recomputes to it — not the sum.
	s.AddTurn([]Message{{Role: RoleUser, Content: "hi"}}, "hello",
		&TokenUsage{TotalTokens: 100}, nil)
	s.AddTurn([]Message{{Role: RoleUser, Content: "again"}}, "hi again",
		&TokenUsage{TotalTokens: 150}, nil)
	const wantRecount = 150 // latest turn's total, NOT 100 + 150

	// Poison the running count so a no-op Resume cannot mask a broken recompute.
	s.CurrentTokenCount = 9999

	t.Run("same model leaves token count untouched", func(t *testing.T) {
		s.Resume(first)
		if got := s.GetCurrentTokenCount(); got != 9999 {
			t.Errorf("after resuming on the same model: count = %d, want unchanged 9999", got)
		}
	})

	t.Run("different model recomputes from history", func(t *testing.T) {
		s.Resume(newPlaceholderConnection())
		if got := s.GetCurrentTokenCount(); got != wantRecount {
			t.Errorf("after resuming on a new model: count = %d, want recomputed %d", got, wantRecount)
		}
	})
}

// TestModelSessionResumeWithNoUsage resets to zero when switching to a backend
// with no recorded usage, ensuring the recompute degrades cleanly rather than
// leaving the stale count in place.
func TestModelSessionResumeWithNoUsage(t *testing.T) {
	s := NewModelSession("t", newPlaceholderConnection())
	s.CurrentTokenCount = 4321 // stale value carried over from the old model

	s.Resume(newPlaceholderConnection())

	if got := s.GetCurrentTokenCount(); got != 0 {
		t.Errorf("after resuming on a new model with no recorded usage: count = %d, want 0", got)
	}
}

// TestModelSessionResumeCarriesConnectorStats covers issue #146: the overall
// stats panel reads cumulative connector counters, but gogent rebuilds the
// connection (with zeroed stats) on every send and swaps it in via Resume. The
// outgoing connector's accumulated counters must be carried into the incoming
// one so the totals stay cumulative across switches instead of resetting.
func TestModelSessionResumeCarriesConnectorStats(t *testing.T) {
	first := newPlaceholderConnection()
	first.Stats.RequestCount = 3
	first.Stats.SuccessCount = 3
	first.Stats.ErrorCount = 1
	first.Stats.TotalTokensIn = 1000
	first.Stats.TotalCachedTokensIn = 200
	first.Stats.TotalTokensOut = 400
	first.Stats.TotalTimeMs = 500

	s := NewModelSession("t", first)

	t.Run("switching backend carries accumulated counters", func(t *testing.T) {
		second := newPlaceholderConnection()
		s.Resume(second)

		got := second.StatsSnapshot()
		want := StatsSnapshot{
			RequestCount:        3,
			SuccessCount:        3,
			ErrorCount:          1,
			TotalTokensIn:       1000,
			TotalCachedTokensIn: 200,
			TotalTokensOut:      400,
			TotalTimeMs:         500,
		}
		if got != want {
			t.Errorf("carried stats = %+v, want %+v", got, want)
		}
	})

	t.Run("further usage accumulates on top of carried totals", func(t *testing.T) {
		// The current backend already holds the carried totals; record more usage
		// on it, then switch again and confirm the running total keeps growing.
		cur := s.Model.(*ModelConnection)
		cur.Stats.TotalTokensIn += 50
		cur.Stats.RequestCount++

		third := newPlaceholderConnection()
		s.Resume(third)

		if got := third.StatsSnapshot().TotalTokensIn; got != 1050 {
			t.Errorf("tokens-in after second switch = %d, want 1050", got)
		}
		if got := third.StatsSnapshot().RequestCount; got != 4 {
			t.Errorf("requests after second switch = %d, want 4", got)
		}
	})

	t.Run("resuming the same backend does not double-count", func(t *testing.T) {
		before := s.Model.(*ModelConnection).StatsSnapshot()
		s.Resume(s.Model)
		if after := s.Model.(*ModelConnection).StatsSnapshot(); after != before {
			t.Errorf("same-backend resume changed stats: %+v -> %+v", before, after)
		}
	})
}

// TestResumeDoesNotDoubleCountContextAcrossTurns is the regression test for the
// premature-compaction bug. gogent rebuilds the model connection on every send
// and swaps it in via Resume, so Resume recomputes the running context size from
// history on essentially every turn. Each turn's Usage.TotalTokens is the size
// of the ENTIRE context at that turn (the whole conversation is re-sent each
// request), so the honest running size is the latest turn's total — not the sum
// of every turn's total. Summing counts the growing prefix over and over and
// inflates the count ~quadratically, which made compaction's 80% high-water
// mark fire at ~10% real usage on large-window models.
//
// This models a 20-turn GLM-5.2 session (1M window) growing ~5K tokens/turn:
// the real context reaches ~100K (10%), but the old sum-based recompute would
// report ~1M (100%) by the end. After the fix Resume reports the real ~100K.
func TestResumeDoesNotDoubleCountContextAcrossTurns(t *testing.T) {
	s := NewModelSession("t", newPlaceholderConnection())
	s.SetMaxContextLength(1_000_000) // GLM-5.2 1M window

	// Each turn's total grows by ~5K (the new content) on top of the prior
	// whole-conversation size, exactly as a real provider reports.
	const growth = 5000
	const turns = 20
	for i := 1; i <= turns; i++ {
		total := growth * i // 5K, 10K, ..., 100K
		s.History = append(s.History, Turn{
			Usage: &TokenUsage{TotalTokens: total},
		})
	}
	wantReal := growth * turns // 100K — the latest turn's total, 10% of the window

	s.Resume(newPlaceholderConnection()) // gogent does this every send
	got := s.GetCurrentTokenCount()

	if got != wantReal {
		t.Errorf("Resume recomputed context = %d, want %d (latest turn total); "+
			"summing would give %d and fire compaction at ~%d%% of the 1M window",
			got, wantReal, growth*turns*(turns+1)/2, 100*growth*turns*(turns+1)/2/1_000_000)
	}
	// And the session must NOT think it needs compaction at 10% real usage.
	if s.NeedsCompression() {
		t.Errorf("NeedsCompression = true at %d/%d tokens (%.0f%%); "+
			"compaction must not fire before the 80%% high-water mark",
			got, s.GetMaxContextLength(), 100*float64(got)/float64(s.GetMaxContextLength()))
	}
}

// TestAddTurnDoesNotAccumulateContext pins the same invariant on the
// AddTurn recording path: a turn's total is the whole-context size, so the
// running count must be set, not added to.
func TestAddTurnDoesNotAccumulateContext(t *testing.T) {
	s := NewModelSession("t", newPlaceholderConnection())
	s.AddTurn([]Message{{Role: RoleUser, Content: "hi"}}, "hello",
		&TokenUsage{TotalTokens: 1000}, nil)
	s.AddTurn([]Message{{Role: RoleUser, Content: "again"}}, "hi",
		&TokenUsage{TotalTokens: 1500}, nil)

	if got, want := s.GetCurrentTokenCount(), 1500; got != want {
		t.Errorf("after two AddTurns: CurrentTokenCount = %d, want %d (latest total, not the sum 2500)", got, want)
	}
}

// TestModelStatsCarry pins the element-wise fold used to preserve connector
// stats across a model switch (issue #146).
func TestModelStatsCarry(t *testing.T) {
	dst := &ModelStats{RequestCount: 2, TotalTokensIn: 100, ErrorCount: 1}
	dst.Carry(StatsSnapshot{
		RequestCount:               5,
		SuccessCount:               4,
		ErrorCount:                 2,
		TotalTokensIn:              900,
		TotalCachedTokensIn:        50,
		TotalTokensOut:             300,
		TotalTimeMs:                700,
		TimeoutCount:               1,
		ContextWindowOverflowCount: 1,
		RefusalCount:               1,
		GenericErrorCount:          1,
	})

	got := dst.Snapshot()
	want := StatsSnapshot{
		RequestCount:               7,
		SuccessCount:               4,
		ErrorCount:                 3,
		TotalTokensIn:              1000,
		TotalCachedTokensIn:        50,
		TotalTokensOut:             300,
		TotalTimeMs:                700,
		TimeoutCount:               1,
		ContextWindowOverflowCount: 1,
		RefusalCount:               1,
		GenericErrorCount:          1,
	}
	if got != want {
		t.Errorf("after Carry: %+v, want %+v", got, want)
	}
}
