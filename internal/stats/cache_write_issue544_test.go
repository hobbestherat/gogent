package stats

import (
	"encoding/json"
	"strings"
	"testing"

	"gogent/internal/model"
)

// This file guards the neutral-report seams for issue #544: FromSnapshot maps the
// write counter, ConnectorStat Add/Sub carry it, the CSV/text renderer surfaces it
// (not silently dropped), CacheHitPercent stays read-based (writes excluded), and
// the JSON key is omitempty so non-Anthropic providers serialize unchanged.

// TestFromSnapshotMapsCacheWrite covers seam 10: the neutral ConnectorStat carries
// the write counter from the model snapshot, alongside the unchanged read counter.
func TestFromSnapshotMapsCacheWrite(t *testing.T) {
	snap := model.StatsSnapshot{
		RequestCount: 3, TotalTokensIn: 1000, TotalCachedTokensIn: 800, TotalCacheWriteTokensIn: 120,
	}
	got := FromSnapshot(snap)
	if got.CacheWriteTokensIn != 120 {
		t.Errorf("CacheWriteTokensIn = %d, want 120", got.CacheWriteTokensIn)
	}
	if got.CachedTokensIn != 800 {
		t.Errorf("CachedTokensIn = %d, want 800 (reads unaffected)", got.CachedTokensIn)
	}
}

// TestConnectorStatAddSubCarryWrite covers seam 9 ops: Add/Sub carry the write field.
func TestConnectorStatAddSubCarryWrite(t *testing.T) {
	a := ConnectorStat{TokensIn: 100, CachedTokensIn: 50, CacheWriteTokensIn: 10}
	b := ConnectorStat{TokensIn: 30, CachedTokensIn: 20, CacheWriteTokensIn: 5}
	if sum := a.Add(b); sum.CacheWriteTokensIn != 15 {
		t.Errorf("Add CacheWriteTokensIn = %d, want 15", sum.CacheWriteTokensIn)
	}
	if diff := a.Sub(b); diff.CacheWriteTokensIn != 5 {
		t.Errorf("Sub CacheWriteTokensIn = %d, want 5", diff.CacheWriteTokensIn)
	}
}

// TestCacheHitPercentExcludesWrites is the headline usability guard (gate 2):
// CacheHitPercent is READ-based (cache reads ÷ input) and must NOT fold cache writes
// into the numerator. Folding writes in would silently change the established metric.
func TestCacheHitPercentExcludesWrites(t *testing.T) {
	c := ConnectorStat{TokensIn: 1000, CachedTokensIn: 800, CacheWriteTokensIn: 100}
	// 800 reads / 1000 input = 80%. If writes were wrongly added: 900/1000 = 90%.
	if got := c.CacheHitPercent(); got != 80 {
		t.Errorf("CacheHitPercent = %d, want 80 (reads-only; writes must not count)", got)
	}
}

// TestReportCSVEmitsCacheWrite covers seam 11b: writeConnectorCSV must emit a
// cache_write_tokens_in row so the write count is NOT silently dropped at the one
// human-readable surface (issue #544 goal #2). The previously-silent field now shows.
func TestReportCSVEmitsCacheWrite(t *testing.T) {
	rep := Report{Models: []ModelStat{{
		Name:      "glm",
		TokensIn:  250,
		Connector: ConnectorStat{Requests: 9, TokensIn: 250, CachedTokensIn: 50, CacheWriteTokensIn: 7},
	}}}
	out, err := rep.CSV()
	if err != nil {
		t.Fatalf("CSV: %v", err)
	}
	if !strings.Contains(out, "model:glm:connector,,cached_tokens_in,50") {
		t.Errorf("CSV missing cached_tokens_in row\n--- CSV ---\n%s", out)
	}
	if !strings.Contains(out, "model:glm:connector,,cache_write_tokens_in,7") {
		t.Errorf("CSV missing cache_write_tokens_in row (write would be silently dropped)\n--- CSV ---\n%s", out)
	}
}

// TestConnectorStatJSONOmitEmptyWrite pins the JSON contract: cache_write_tokens_in
// is omitempty, so a non-Anthropic provider (write==0) serializes exactly as before
// #544 (no write key), while a write turn includes it. This preserves byte-identity
// for external JSON readers and persisted reports.
func TestConnectorStatJSONOmitEmptyWrite(t *testing.T) {
	zero := ConnectorStat{Requests: 1, TokensIn: 100, CachedTokensIn: 40} // CacheWriteTokensIn: 0
	b, err := json.Marshal(zero)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(b), "cache_write_tokens_in") {
		t.Errorf("zero-write ConnectorStat JSON included cache_write_tokens_in; want omitempty: %s", b)
	}
	withWrite := ConnectorStat{Requests: 1, TokensIn: 100, CachedTokensIn: 40, CacheWriteTokensIn: 9}
	b2, err := json.Marshal(withWrite)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(b2), `"cache_write_tokens_in":9`) {
		t.Errorf("write ConnectorStat JSON missing cache_write_tokens_in:9: %s", b2)
	}
}
