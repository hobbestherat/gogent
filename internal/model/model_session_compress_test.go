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

func TestEstimateTokensCountsContentAndToolArgs(t *testing.T) {
	msgs := []Message{
		{Role: RoleUser, Content: "12345678"}, // 8/4 = 2
		{Role: RoleAssistant, ToolCalls: []ToolCall{{Function: FunctionCall{Arguments: "abcd"}}}}, // 4/4 = 1
	}
	if got := EstimateTokens(msgs); got != 3 {
		t.Errorf("EstimateTokens = %d, want 3", got)
	}
}
