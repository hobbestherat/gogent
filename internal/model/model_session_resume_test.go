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
