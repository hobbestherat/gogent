package compression

import (
	"strings"
	"testing"

	"gogent/internal/model"
)

// fakeCompleter is a stateless stand-in for a model backend.
type fakeCompleter struct {
	got   []model.Message
	reply string
	err   error
}

func (f *fakeCompleter) Complete(messages []model.Message) (*model.CompletionResponse, error) {
	f.got = messages
	if f.err != nil {
		return nil, f.err
	}
	return &model.CompletionResponse{Content: f.reply}, nil
}

func toolCallMsg() model.Message {
	return model.Message{
		Role: model.RoleAssistant,
		ToolCalls: []model.ToolCall{{
			ID:       "c1",
			Type:     "function",
			Function: model.FunctionCall{Name: "read", Arguments: `{"path":"x"}`},
		}},
	}
}

// sampleTranscript: two early turns (one with a tool call/result pair) plus two
// recent turns.
func sampleTranscript() []model.Message {
	return []model.Message{
		{Role: model.RoleUser, Content: "u1"},
		toolCallMsg(),
		{Role: model.RoleTool, ToolCallID: "c1", Content: "tool out"},
		{Role: model.RoleAssistant, Content: "a1"},
		{Role: model.RoleUser, Content: "u2"},
		{Role: model.RoleAssistant, Content: "a2"},
		{Role: model.RoleUser, Content: "u3"},
		{Role: model.RoleAssistant, Content: "a3"},
	}
}

func TestSafeSplitKeepsRecentTurnsAtUserBoundary(t *testing.T) {
	msgs := sampleTranscript()
	older, recent := SafeSplit(msgs, 1)

	if len(recent) == 0 || recent[0].Role != model.RoleUser {
		t.Fatalf("recent slice must start at a user message, got %+v", recent)
	}
	if len(older)+len(recent) != len(msgs) {
		t.Fatalf("older+recent (%d+%d) must reconstruct original %d", len(older), len(recent), len(msgs))
	}
	// keepRecentTurns=1 -> recent is the last user turn (u3, a3).
	if len(recent) != 2 || recent[0].Content != "u3" {
		t.Errorf("expected recent = [u3 a3], got %+v", recent)
	}
}

func TestSafeSplitNeverSplitsToolCallFromResults(t *testing.T) {
	msgs := sampleTranscript()
	// Any keepRecentTurns value must yield a recent slice that begins with a user
	// message, so an assistant tool_calls message is never stranded from its
	// role:"tool" results.
	for keep := 1; keep <= 5; keep++ {
		older, recent := SafeSplit(msgs, keep)
		if len(recent) > 0 && recent[0].Role != model.RoleUser {
			t.Errorf("keep=%d: recent starts with %s, want user", keep, recent[0].Role)
		}
		// A tool result must never be the first recent message (orphan result).
		if len(recent) > 0 && recent[0].Role == model.RoleTool {
			t.Errorf("keep=%d: recent begins with an orphan tool result", keep)
		}
		if len(older)+len(recent) != len(msgs) {
			t.Errorf("keep=%d: split did not reconstruct the transcript", keep)
		}
	}
}

func TestSummarizeProducesDigestFromOlderSlice(t *testing.T) {
	digest := "### Goal\n- do the thing\n### Next Steps\n- continue"
	fc := &fakeCompleter{reply: digest}
	ca := NewCompressionAgent(nil, fc)

	older, _ := SafeSplit(sampleTranscript(), 1)
	out, err := ca.Summarize(older)
	if err != nil {
		t.Fatalf("Summarize failed: %v", err)
	}
	if out != digest {
		t.Errorf("digest = %q, want %q", out, digest)
	}
	// The prompt must carry the older conversation and the structured format.
	if len(fc.got) != 1 {
		t.Fatalf("expected one prompt message, got %d", len(fc.got))
	}
	prompt := fc.got[0].Content
	for _, want := range []string{"## Conversation To Summarize", "### Goal", "[user]: u1", "[tool result]: tool out", "calls read"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt missing %q", want)
		}
	}
}

func TestSummarizeEmptyReturnsEmpty(t *testing.T) {
	fc := &fakeCompleter{reply: "should not be used"}
	ca := NewCompressionAgent(nil, fc)
	out, err := ca.Summarize(nil)
	if err != nil || out != "" {
		t.Errorf("empty older slice: got (%q, %v), want empty", out, err)
	}
	if fc.got != nil {
		t.Errorf("completer must not be called for an empty slice")
	}
}
