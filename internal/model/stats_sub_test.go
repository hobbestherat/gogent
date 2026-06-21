package model

import "testing"

// TestStatsSnapshotSub covers the element-wise subtraction added for issue #191:
// recordConnectorUsage folds the DELTA of a connector snapshot (cur minus the
// last-read snapshot) into the stable per-model accumulator, so Sub must subtract
// every field exactly and must be able to go negative (a rebuilt/zeroed connector
// produces negative deltas the caller guards for).
func TestStatsSnapshotSub(t *testing.T) {
	a := StatsSnapshot{
		RequestCount:               10,
		SuccessCount:               8,
		ErrorCount:                 2,
		TotalTokensIn:              1000,
		TotalCachedTokensIn:        200,
		TotalTokensOut:             400,
		TotalTimeMs:                5000,
		TimeoutCount:               1,
		ContextWindowOverflowCount: 3,
		RefusalCount:               0,
		GenericErrorCount:          2,
	}
	b := StatsSnapshot{
		RequestCount:               4,
		SuccessCount:               3,
		ErrorCount:                 1,
		TotalTokensIn:              250,
		TotalCachedTokensIn:        50,
		TotalTokensOut:             100,
		TotalTimeMs:                1200,
		TimeoutCount:               1,
		ContextWindowOverflowCount: 1,
		RefusalCount:               0,
		GenericErrorCount:          1,
	}
	want := StatsSnapshot{
		RequestCount:               6,
		SuccessCount:               5,
		ErrorCount:                 1,
		TotalTokensIn:              750,
		TotalCachedTokensIn:        150,
		TotalTokensOut:             300,
		TotalTimeMs:                3800,
		TimeoutCount:               0,
		ContextWindowOverflowCount: 2,
		RefusalCount:               0,
		GenericErrorCount:          1,
	}
	if got := a.Sub(b); got != want {
		t.Errorf("a.Sub(b) = %+v, want %+v", got, want)
	}
}

// TestStatsSnapshotSubCanGoNegative confirms Sub does NOT clamp at zero: a
// rebuilt/zeroed connector yields cur < last-read, so the delta is negative on
// the fields that shrank. recordConnectorUsage relies on detecting this (via
// delta.RequestCount < 0) to restart the baseline.
func TestStatsSnapshotSubCanGoNegative(t *testing.T) {
	cur := StatsSnapshot{RequestCount: 0, TotalTokensIn: 0}
	prev := StatsSnapshot{RequestCount: 5, TotalTokensIn: 1000}
	got := cur.Sub(prev)
	if got.RequestCount != -5 || got.TotalTokensIn != -1000 {
		t.Errorf("Sub of a reset connector = %+v, want negative {-5 -1000} (no clamping)", got)
	}
}

// TestStatsSnapshotSubAddRoundTrip checks the inverse relationship with Add:
// (a.Sub(b)).Add(b) == a. The accumulator adds deltas back together to rebuild a
// total, so Sub and Add must round-trip cleanly across every field, including
// through negative intermediate deltas.
func TestStatsSnapshotSubAddRoundTrip(t *testing.T) {
	a := StatsSnapshot{RequestCount: 7, TotalTokensIn: 700, TotalTimeMs: 900, ErrorCount: 2}
	b := StatsSnapshot{RequestCount: 12, TotalTokensIn: 1200, TotalTimeMs: 1500, ErrorCount: 5}
	// b > a on purpose: the intermediate Sub goes negative, then Add restores.
	roundTrip := a.Sub(b).Add(b)
	if roundTrip != a {
		t.Errorf("(a.Sub(b)).Add(b) = %+v, want a %+v", roundTrip, a)
	}
}

// TestStatsSnapshotSubZeroIdentity confirms subtracting a zero snapshot is a no-op
// (the first recordConnectorUsage call has a zero lastConnSnap, so delta == cur).
func TestStatsSnapshotSubZeroIdentity(t *testing.T) {
	a := StatsSnapshot{RequestCount: 3, TotalTokensIn: 300, TotalTokensOut: 90, TotalCachedTokensIn: 30}
	if got := a.Sub(StatsSnapshot{}); got != a {
		t.Errorf("a.Sub(zero) = %+v, want a %+v", got, a)
	}
}
