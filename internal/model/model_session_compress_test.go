package model

import "testing"

func TestApplyCompressedTranscriptPreservesSystemPrompt(t *testing.T) {
	s := NewModelSession("t", NewModelConnection())
	s.SetSystemPrompt("SYSTEM PROMPT")
	s.ReplaceTranscript([]Message{
		{Role: RoleUser, Content: "a long original first message"},
		{Role: RoleAssistant, Content: "a long original answer"},
		{Role: RoleUser, Content: "second"},
		{Role: RoleAssistant, Content: "second answer"},
	})
	// Simulate a real (API-reported) token count before compression.
	s.CurrentTokenCount = 5000

	var before, after int
	var fired bool
	s.AddCallback(func(e CallbackEvent) {
		if e.Type == EventCompression && e.Compression != nil {
			fired = true
			before = e.Compression.Before
			after = e.Compression.After
		}
	})

	newTranscript := []Message{
		{Role: RoleUser, Content: "[summary] short digest"},
		{Role: RoleUser, Content: "second"},
		{Role: RoleAssistant, Content: "second answer"},
	}
	s.ApplyCompressedTranscript(newTranscript)

	if s.SystemPrompt != "SYSTEM PROMPT" {
		t.Errorf("system prompt changed to %q", s.SystemPrompt)
	}
	got := s.GetTranscript()
	if len(got) != len(newTranscript) || got[0].Content != "[summary] short digest" {
		t.Errorf("transcript not replaced: %+v", got)
	}
	if !fired {
		t.Fatal("EventCompression was not emitted")
	}
	if before != 5000 {
		t.Errorf("Before = %d, want the pre-compression count 5000", before)
	}
	if after <= 0 || after >= before {
		t.Errorf("After = %d, want a smaller positive estimate than Before=%d", after, before)
	}
	if s.GetTokenCount() != after {
		t.Errorf("CurrentTokenCount = %d, want %d", s.GetTokenCount(), after)
	}
}

func TestNeedsCompressionThreshold(t *testing.T) {
	s := NewModelSession("t", NewModelConnection())
	s.SetMaxContextLength(1000)

	s.CurrentTokenCount = 799
	if s.NeedsCompression() {
		t.Error("should not need compression below 80%")
	}
	s.CurrentTokenCount = 800
	if !s.NeedsCompression() {
		t.Error("should need compression at 80%")
	}
}

// TestNeedsCompressionHysteresis verifies the water-mark hysteresis that keeps a
// compaction from re-firing every turn (the "synchronous summarization
// round-trip each turn" thrash from issue #4). Compaction fires at the 80%
// high-water mark, then stays suppressed until the context recedes below the 50%
// low-water mark.
func TestNeedsCompressionHysteresis(t *testing.T) {
	s := NewModelSession("t", NewModelConnection())
	s.SetMaxContextLength(1000) // high-water 800, low-water 500

	// Armed by default: fires at the high-water mark.
	s.CurrentTokenCount = 800
	if !s.NeedsCompression() {
		t.Fatal("armed session should need compression at the 80% high-water mark")
	}

	// A compaction suppresses further compression...
	s.ApplyCompressedTranscript([]Message{{Role: RoleUser, Content: "digest"}})
	// ...even when the count is back above the high-water mark (no thrash), and
	// also while it sits in the hysteresis band between the two marks.
	for _, count := range []int{700, 950} {
		s.CurrentTokenCount = count
		if s.NeedsCompression() {
			t.Fatalf("suppressed session re-fired at %d tokens (thrash)", count)
		}
	}

	// Re-arm only once the count drops to/below the low-water mark.
	s.CurrentTokenCount = 500
	if s.NeedsCompression() {
		t.Error("re-arming at the low-water mark should not yet fire (below high)")
	}
	// After re-arming, the high-water mark fires again.
	s.CurrentTokenCount = 800
	if !s.NeedsCompression() {
		t.Error("re-armed session should fire again at the 80% high-water mark")
	}
}

// TestNeedsCompressionDisabledWithoutWindow verifies that with no context window
// configured, compaction never fires (the safeguard is off rather than firing at
// a nonsense threshold).
func TestNeedsCompressionDisabledWithoutWindow(t *testing.T) {
	s := NewModelSession("t", NewModelConnection())
	s.SetMaxContextLength(0)
	s.CurrentTokenCount = 1_000_000
	if s.NeedsCompression() {
		t.Error("should not compress with no context window configured")
	}
}

// TestSetMaxContextLengthResetsHysteresisOnChange verifies that switching to a
// different context window clears the suppression flag (so a freshly enlarged
// window is evaluated against the new budget), while re-setting the same window
// leaves an active suppression intact.
func TestSetMaxContextLengthResetsHysteresisOnChange(t *testing.T) {
	s := NewModelSession("t", NewModelConnection())
	s.SetMaxContextLength(1000) // high 800, low 500
	s.CurrentTokenCount = 800
	if !s.NeedsCompression() {
		t.Fatal("expected to fire at high-water mark")
	}
	s.ApplyCompressedTranscript([]Message{{Role: RoleUser, Content: "digest"}})

	// Same window re-applied: suppression survives (the common per-turn case).
	s.SetMaxContextLength(1000)
	s.CurrentTokenCount = 950
	if s.NeedsCompression() {
		t.Error("re-setting the same window should not clear hysteresis")
	}

	// Different window: hysteresis resets and the high-water mark fires again.
	s.SetMaxContextLength(10000) // high 8000
	s.CurrentTokenCount = 8000
	if !s.NeedsCompression() {
		t.Error("switching windows should reset hysteresis and re-arm")
	}
}

func TestEstimateTokensCountsContentAndToolArgs(t *testing.T) {
	msgs := []Message{
		{Role: RoleUser, Content: "12345678"},                                                     // 8/4 = 2
		{Role: RoleAssistant, ToolCalls: []ToolCall{{Function: FunctionCall{Arguments: "abcd"}}}}, // 4/4 = 1
	}
	if got := EstimateTokens(msgs); got != 3 {
		t.Errorf("EstimateTokens = %d, want 3", got)
	}
}
