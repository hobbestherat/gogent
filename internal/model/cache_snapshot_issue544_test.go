package model

import "testing"

// This file guards the snapshot-ops seams (issue #544 §2.2): Add/Sub/Snapshot/Carry
// all carry the new TotalCacheWriteTokensIn counter, and IsReset trips when ONLY the
// write counter rewinds (a connector rebuild a request-only check would miss).

// TestStatsSnapshotCarriesCacheWrite covers every snapshot op for the write counter
// and the IsReset invariant that depends on it.
func TestStatsSnapshotCarriesCacheWrite(t *testing.T) {
	t.Run("Add sums the write counter", func(t *testing.T) {
		got := StatsSnapshot{RequestCount: 1, TotalCachedTokensIn: 10, TotalCacheWriteTokensIn: 5}.
			Add(StatsSnapshot{RequestCount: 2, TotalCachedTokensIn: 20, TotalCacheWriteTokensIn: 7})
		if got.TotalCacheWriteTokensIn != 12 {
			t.Errorf("Add TotalCacheWriteTokensIn = %d, want 12", got.TotalCacheWriteTokensIn)
		}
		if got.TotalCachedTokensIn != 30 {
			t.Errorf("Add TotalCachedTokensIn = %d, want 30 (reads unaffected)", got.TotalCachedTokensIn)
		}
	})
	t.Run("Sub differences the write counter", func(t *testing.T) {
		got := StatsSnapshot{RequestCount: 5, TotalCachedTokensIn: 100, TotalCacheWriteTokensIn: 30}.
			Sub(StatsSnapshot{RequestCount: 2, TotalCachedTokensIn: 40, TotalCacheWriteTokensIn: 10})
		if got.TotalCacheWriteTokensIn != 20 {
			t.Errorf("Sub TotalCacheWriteTokensIn = %d, want 20", got.TotalCacheWriteTokensIn)
		}
	})
	t.Run("IsReset trips on a negative WRITE delta alone", func(t *testing.T) {
		// Every other counter is non-negative; only the write counter rewinds. A
		// request-only IsReset check would miss this and double-count after a rebuild.
		delta := StatsSnapshot{
			RequestCount: 1, TotalTokensIn: 10, TotalCachedTokensIn: 5,
			TotalCacheWriteTokensIn: -1,
		}
		if !delta.IsReset() {
			t.Error("IsReset = false for a write-only rewind, want true (must check every counter)")
		}
	})
	t.Run("IsReset stays false when all counters grow", func(t *testing.T) {
		delta := StatsSnapshot{RequestCount: 1, TotalTokensIn: 10, TotalCachedTokensIn: 5, TotalCacheWriteTokensIn: 3}
		if delta.IsReset() {
			t.Error("IsReset = true for an all-positive delta, want false")
		}
	})
	t.Run("Snapshot copies the write counter", func(t *testing.T) {
		ms := &ModelStats{RequestCount: 1, TotalCacheWriteTokensIn: 9, TotalCachedTokensIn: 4}
		if got := ms.Snapshot(); got.TotalCacheWriteTokensIn != 9 || got.TotalCachedTokensIn != 4 {
			t.Errorf("Snapshot = %+v, want write=9 read=4", got)
		}
	})
	t.Run("Carry folds the write counter", func(t *testing.T) {
		ms := &ModelStats{TotalCacheWriteTokensIn: 5, TotalCachedTokensIn: 3}
		ms.Carry(StatsSnapshot{TotalCacheWriteTokensIn: 7, TotalCachedTokensIn: 9})
		if ms.TotalCacheWriteTokensIn != 12 {
			t.Errorf("Carry TotalCacheWriteTokensIn = %d, want 12", ms.TotalCacheWriteTokensIn)
		}
		if ms.TotalCachedTokensIn != 12 {
			t.Errorf("Carry TotalCachedTokensIn = %d, want 12", ms.TotalCachedTokensIn)
		}
	})
}
