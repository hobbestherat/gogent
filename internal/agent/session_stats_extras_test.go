package agent

import (
	"testing"
)

// TestPerModelTokenAttribution verifies that token usage is attributed to the
// currently selected primary model (set via SetPrimaryModel), that switching
// models accumulates into separate per-model buckets, and that usage before any
// model is set is counted only in the session totals.
func TestPerModelTokenAttribution(t *testing.T) {
	us, _ := newLoopSession(t, "http://unused")

	// Before a model is selected, usage is not attributed to any model.
	us.AddTokenUsage(10, 2)
	if m := us.ModelTokens(); len(m) != 0 {
		t.Errorf("ModelTokens before SetPrimaryModel = %v, want empty", m)
	}

	// Attribute the next batch to "opus".
	us.SetPrimaryModel("opus")
	us.AddTokenUsage(100, 20)
	us.AddTokenUsage(50, 10)

	// Switch to a second model and attribute more usage to it.
	us.SetPrimaryModel("haiku")
	us.AddTokenUsage(30, 5)

	got := us.ModelTokens()
	if len(got) != 2 {
		t.Fatalf("ModelTokens = %v, want 2 models", got)
	}
	// ModelTokens returns rows sorted by model name.
	if got[0].Name != "haiku" || got[0].TokensIn != 30 || got[0].TokensOut != 5 {
		t.Errorf("haiku row = %+v, want haiku 30/5", got[0])
	}
	if got[1].Name != "opus" || got[1].TokensIn != 150 || got[1].TokensOut != 30 {
		t.Errorf("opus row = %+v, want opus 150/30", got[1])
	}

	// Session totals include ALL usage (pre- and post-attribution).
	snap := us.Snapshot()
	if snap.TokensIn != 190 || snap.TokensOut != 37 {
		t.Errorf("Snapshot tokens = %d/%d, want 190/37", snap.TokensIn, snap.TokensOut)
	}
}

// TestPrimaryModelAccessors covers the round-trip of SetPrimaryModel/PrimaryModel.
func TestPrimaryModelAccessors(t *testing.T) {
	us, _ := newLoopSession(t, "http://unused")
	if name := us.PrimaryModel(); name != "" {
		t.Errorf("PrimaryModel = %q before set, want empty", name)
	}
	us.SetPrimaryModel("opus")
	if name := us.PrimaryModel(); name != "opus" {
		t.Errorf("PrimaryModel = %q, want opus", name)
	}
}

// TestCompactionCount verifies the compaction counter increments when
// compactIfNeeded successfully summarizes, using the existing compression test
// harness.
func TestCompactionCount(t *testing.T) {
	us, sess := makeCompressibleSession("compact")

	stub := &stubCompleter{content: "### digest"}
	us.SetCompressionCompleter(stub)

	if got := us.CompactionCount(); got != 0 {
		t.Fatalf("CompactionCount before compaction = %d, want 0", got)
	}

	us.compactIfNeeded(sess, func(SessionEvent) {})

	if got := us.CompactionCount(); got != 1 {
		t.Errorf("CompactionCount after one compaction = %d, want 1", got)
	}
}

// TestCompactionCountNoIncrementWhenSummarizeFails verifies the counter does not
// advance when the compression completer fails (compactIfNeeded must leave the
// transcript and counters untouched on failure).
func TestCompactionCountNoIncrementWhenSummarizeFails(t *testing.T) {
	us, sess := makeCompressibleSession("compact-fail")

	// A completer that returns empty content: compactIfNeeded treats an empty
	// digest as a failure and returns without applying anything.
	stub := &stubCompleter{content: ""}
	us.SetCompressionCompleter(stub)

	us.compactIfNeeded(sess, func(SessionEvent) {})

	if got := us.CompactionCount(); got != 0 {
		t.Errorf("CompactionCount after failed summarization = %d, want 0", got)
	}
}
