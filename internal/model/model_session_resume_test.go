package model

import "testing"

// TestModelSessionResumeRecomputesOnModelSwitch covers the fix for issue #3:
// the token-count recompute inside Resume must actually run when the model
// backend changes. Previously the guard compared newModel against s.Model
// *after* assigning it, so the branch was unreachable dead code and the count
// stayed stale across a model switch.
func TestModelSessionResumeRecomputesOnModelSwitch(t *testing.T) {
	first := NewModelConnection()
	s := NewModelSession("t", first)

	// Record two turns with known usage. Resume recomputes the running count as
	// the sum of these per-turn totals.
	s.AddTurn([]Message{{Role: RoleUser, Content: "hi"}}, "hello",
		&TokenUsage{TotalTokens: 100}, nil)
	s.AddTurn([]Message{{Role: RoleUser, Content: "again"}}, "hi again",
		&TokenUsage{TotalTokens: 150}, nil)
	const wantRecount = 250

	// Poison the running count so a no-op Resume cannot mask a broken recompute.
	s.CurrentTokenCount = 9999

	t.Run("same model leaves token count untouched", func(t *testing.T) {
		s.Resume(first)
		if got := s.GetCurrentTokenCount(); got != 9999 {
			t.Errorf("after resuming on the same model: count = %d, want unchanged 9999", got)
		}
	})

	t.Run("different model recomputes from history", func(t *testing.T) {
		s.Resume(NewModelConnection())
		if got := s.GetCurrentTokenCount(); got != wantRecount {
			t.Errorf("after resuming on a new model: count = %d, want recomputed %d", got, wantRecount)
		}
	})
}

// TestModelSessionResumeWithNoUsage resets to zero when switching to a backend
// with no recorded usage, ensuring the recompute degrades cleanly rather than
// leaving the stale count in place.
func TestModelSessionResumeWithNoUsage(t *testing.T) {
	s := NewModelSession("t", NewModelConnection())
	s.CurrentTokenCount = 4321 // stale value carried over from the old model

	s.Resume(NewModelConnection())

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
	first := NewModelConnection()
	first.Stats.RequestCount = 3
	first.Stats.SuccessCount = 3
	first.Stats.ErrorCount = 1
	first.Stats.TotalTokensIn = 1000
	first.Stats.TotalCachedTokensIn = 200
	first.Stats.TotalTokensOut = 400
	first.Stats.TotalTimeMs = 500

	s := NewModelSession("t", first)

	t.Run("switching backend carries accumulated counters", func(t *testing.T) {
		second := NewModelConnection()
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

		third := NewModelConnection()
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
