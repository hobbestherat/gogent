package stats

import (
	"testing"
)

// This file guards the report-level surface issue #546 owns: ConnectorStat.CacheWritePercent,
// the write-share companion to CacheHitPercent that the Overall panel and the Statistics
// dialog render. The field, Add/Sub, FromSnapshot and the CSV column already exist from #544
// (see cache_write_issue544_test.go); #546 adds only this method and the display, so these
// tests pin the method's arithmetic, its zero-input guard, its truncation (not rounding)
// behaviour and its clean degrade to 0% for providers that never report writes.

// TestCacheWritePercentArithmetic pins the headline computation: cache-write share of input
// tokens, as a whole-number percentage. 100 writes / 1000 input = 10%.
func TestCacheWritePercentArithmetic(t *testing.T) {
	c := ConnectorStat{TokensIn: 1000, CacheWriteTokensIn: 100}
	if got := c.CacheWritePercent(); got != 10 {
		t.Errorf("CacheWritePercent = %d, want 10 (100/1000)", got)
	}
}

// TestCacheWritePercentTruncatesNotRounds pins that the method truncates toward zero like
// CacheHitPercent (int(float ...) ), it does NOT round. 12 writes / 1000 input = 1.2% → 1,
// never 1-by-rounding-to-2 nor 0. A switch to rounding would silently bump the displayed
// write share and is the kind of change this test exists to catch.
func TestCacheWritePercentTruncatesNotRounds(t *testing.T) {
	cases := []struct {
		write, tokens, want int
		note                string
	}{
		{12, 1000, 1, "1.2% truncates to 1, not rounds to 1 (and never 2)"},
		{19, 1000, 1, "1.9% truncates to 1, not rounds to 2"},
		{5, 1000, 0, "0.5% truncates to 0"},
		{12400, 100000, 12, "12.4% truncates to 12 (realistic Anthropic magnitudes)"},
	}
	for _, tc := range cases {
		c := ConnectorStat{TokensIn: tc.tokens, CacheWriteTokensIn: tc.write}
		if got := c.CacheWritePercent(); got != tc.want {
			t.Errorf("CacheWritePercent(write=%d, tokens=%d) = %d, want %d (%s)",
				tc.write, tc.tokens, got, tc.want, tc.note)
		}
	}
}

// TestCacheWritePercentZeroInputGuard pins the divide-by-zero guard: a connector that has
// processed no input tokens reports 0% writes, never panics or divides by zero. This is the
// empty-report / first-frame path the Overall panel hits before any traffic.
func TestCacheWritePercentZeroInputGuard(t *testing.T) {
	cases := []struct {
		name string
		c    ConnectorStat
	}{
		{"zero connector", ConnectorStat{}},
		{"writes recorded but no input yet (should not happen, guarded anyway)",
			ConnectorStat{TokensIn: 0, CacheWriteTokensIn: 50}},
		{"negative input (defensive)", ConnectorStat{TokensIn: -5, CacheWriteTokensIn: 50}},
	}
	for _, tc := range cases {
		if got := tc.c.CacheWritePercent(); got != 0 {
			t.Errorf("%s: CacheWritePercent = %d, want 0 (TokensIn<=0 guard)", tc.name, got)
		}
	}
}

// TestCacheWritePercentFullShare pins the 100% boundary: every input token written to cache.
func TestCacheWritePercentFullShare(t *testing.T) {
	c := ConnectorStat{TokensIn: 1000, CacheWriteTokensIn: 1000}
	if got := c.CacheWritePercent(); got != 100 {
		t.Errorf("CacheWritePercent = %d, want 100", got)
	}
}

// TestCacheWritePercentDegradesForNonWriteProviders is the criterion-3 degrade gate: every
// provider that never reports a cache write (OpenAI/DeepSeek/Gemini/Z.AI/OpenRouter) carries
// CacheWriteTokensIn==0, so the write share must read exactly 0% — the row renders "0% 0".
func TestCacheWritePercentDegradesForNonWriteProviders(t *testing.T) {
	// Real input, real cache reads, but zero writes (the OpenAI-style profile).
	c := ConnectorStat{TokensIn: 1000, CachedTokensIn: 700, CacheWriteTokensIn: 0}
	if got := c.CacheWritePercent(); got != 0 {
		t.Errorf("CacheWritePercent = %d, want 0 (non-write provider degrades cleanly)", got)
	}
	// Reads still report normally — the read metric is unaffected by the write method.
	if got := c.CacheHitPercent(); got != 70 {
		t.Errorf("CacheHitPercent = %d, want 70 (reads unaffected by write degrade)", got)
	}
}

// TestCacheWritePercentSameDenominatorAsHit pins the holistic invariant: CacheWritePercent
// and CacheHitPercent share the SAME denominator (TokensIn), so for well-formed provider
// data — where cache reads and cache writes are disjoint subsets of input tokens — the two
// whole-number shares never sum above 100%. A denominator drift (e.g. write% over cached
// tokens) would break this and mislead the user about spend.
func TestCacheWritePercentSameDenominatorAsHit(t *testing.T) {
	// 700 reads + 200 writes of 1000 input = 70% read + 20% write, sum 90% (10% fresh).
	c := ConnectorStat{TokensIn: 1000, CachedTokensIn: 700, CacheWriteTokensIn: 200}
	rd := c.CacheHitPercent()
	wr := c.CacheWritePercent()
	if rd != 70 || wr != 20 {
		t.Fatalf("read%%=%d write%%=%d, want 70/20", rd, wr)
	}
	if rd+wr > 100 {
		t.Errorf("read%%(%d)+write%%(%d)=%d > 100 — denominators disagree", rd, wr, rd+wr)
	}
}
