package agent

import (
	"strings"
	"testing"

	"gogent/internal/model"
)

// stubCompleter is a fake compression backend: it records how many times it was
// asked to summarize, returns a canned digest, and reports separable stats so we
// can verify fast-model usage is tracked apart from the primary model.
type stubCompleter struct {
	calls   int
	content string
	snap    model.StatsSnapshot
}

func (s *stubCompleter) Complete(messages []model.Message) (*model.CompletionResponse, error) {
	s.calls++
	return &model.CompletionResponse{Content: s.content, Role: model.RoleAssistant}, nil
}

func (s *stubCompleter) GetStats() *model.ModelStats        { return &model.ModelStats{} }
func (s *stubCompleter) StatsSnapshot() model.StatsSnapshot { return s.snap }

// makeCompressibleSession builds a session whose transcript has enough turns to
// split and whose token count is over the compression threshold.
func makeCompressibleSession(id string) (*UserSession, *model.ModelSession) {
	conn := newTestModelConnection()
	sess := model.NewModelSession(id, conn)
	sess.SetMaxContextLength(100)

	var msgs []model.Message
	for i := 0; i < 6; i++ {
		msgs = append(msgs, model.Message{Role: model.RoleUser, Content: "question"})
		msgs = append(msgs, model.Message{Role: model.RoleAssistant, Content: "answer"})
	}
	sess.ReplaceTranscript(msgs)
	sess.CurrentTokenCount = 999 // force NeedsCompression

	ag := NewAgent("root", sess)
	return NewUserSession(id, ag), sess
}

// TestCompactIfNeededUsesCompressionCompleter verifies compaction routes to the
// injected (fast) completer and reports its usage separately in stats.
func TestCompactIfNeededUsesCompressionCompleter(t *testing.T) {
	us, sess := makeCompressibleSession("fast")

	stub := &stubCompleter{
		content: "### Goal\n- digest",
		snap:    model.StatsSnapshot{TotalTokensIn: 42, TotalTokensOut: 7},
	}
	us.SetCompressionCompleter(stub)

	us.compactIfNeeded(sess, func(SessionEvent) {})

	if stub.calls != 1 {
		t.Fatalf("expected fast completer to be called once, got %d", stub.calls)
	}
	got := sess.GetTranscript()
	if len(got) == 0 || !strings.Contains(got[0].Content, "digest") {
		t.Fatalf("expected compacted transcript to start with the digest, got %+v", got)
	}

	stats := us.GetStats()
	if in, _ := stats["fast_tokens_in"].(int); in != 42 {
		t.Errorf("fast_tokens_in = %v, want 42", stats["fast_tokens_in"])
	}
	if out, _ := stats["fast_tokens_out"].(int); out != 7 {
		t.Errorf("fast_tokens_out = %v, want 7", stats["fast_tokens_out"])
	}
}

// TestFastConnectorStatsZeroWhenUnset verifies that with no fast model wired in,
// the fast-model stats are zero (the primary model handles compaction).
func TestFastConnectorStatsZeroWhenUnset(t *testing.T) {
	us, _ := makeCompressibleSession("primary")

	if snap := us.FastConnectorStats(); snap != (model.StatsSnapshot{}) {
		t.Errorf("expected zero fast stats when unset, got %+v", snap)
	}
	stats := us.GetStats()
	if in, _ := stats["fast_tokens_in"].(int); in != 0 {
		t.Errorf("fast_tokens_in = %v, want 0", stats["fast_tokens_in"])
	}
}
