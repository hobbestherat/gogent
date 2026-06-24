package model

import (
	"context"
	"errors"
	"testing"
)

type captureConnector struct {
	requests [][]Message
	resp     *CompletionResponse
	err      error
}

func (c *captureConnector) Complete(messages []Message) (*CompletionResponse, error) {
	return c.CompleteWithTools(messages, nil)
}

func (c *captureConnector) CompleteWithTools(messages []Message, tools []ToolDef) (*CompletionResponse, error) {
	return c.CompleteWithToolsCtx(context.Background(), messages, tools)
}

func (c *captureConnector) CompleteWithToolsCtx(ctx context.Context, messages []Message, tools []ToolDef) (*CompletionResponse, error) {
	cp := append([]Message(nil), messages...)
	c.requests = append(c.requests, cp)
	if c.err != nil {
		return nil, c.err
	}
	if c.resp != nil {
		return c.resp, nil
	}
	return &CompletionResponse{Role: RoleAssistant, Content: "ok"}, nil
}

func (c *captureConnector) CompleteStream(messages []Message) (<-chan StreamResponse, <-chan error) {
	ch := make(chan StreamResponse)
	errCh := make(chan error, 1)
	close(ch)
	close(errCh)
	return ch, errCh
}

func (c *captureConnector) GetStats() *ModelStats { return &ModelStats{} }

func (c *captureConnector) StatsSnapshot() StatsSnapshot { return StatsSnapshot{} }

func TestModelSessionRemoveCallback(t *testing.T) {
	m := NewModelConnection()
	s := NewModelSession("test1", m)

	s.AddCallback(func(event CallbackEvent) {})

	// RemoveCallback is simplified for now - removes all callbacks
	s.RemoveCallback(func(event CallbackEvent) {})

	if len(s.Callbacks) != 0 {
		t.Errorf("Expected 0 callbacks after removal, got %d", len(s.Callbacks))
	}
}

func TestModelSessionVolatileContextAppendedAfterTranscriptAndNotPersisted(t *testing.T) {
	conn := &captureConnector{}
	s := NewModelSession("volatile", conn)
	s.SetSystemPrompt("stable-system")
	s.SetVolatileContext("## Git status\nM file.go")

	resp, err := s.Send([]Message{{Role: RoleUser, Content: "first user"}})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if resp.Content != "ok" {
		t.Fatalf("response content = %q, want ok", resp.Content)
	}
	if len(conn.requests) != 1 {
		t.Fatalf("requests = %d, want 1", len(conn.requests))
	}

	got := conn.requests[0]
	if len(got) != 3 {
		t.Fatalf("wire messages = %d, want system + transcript + volatile tail: %+v", len(got), got)
	}
	if got[0].Role != RoleSystem || got[0].Content != "stable-system" {
		t.Errorf("message 0 = %+v, want stable system", got[0])
	}
	if got[1].Role != RoleUser || got[1].Content != "first user" {
		t.Errorf("message 1 = %+v, want transcript user message", got[1])
	}
	if got[2].Role != RoleUser || got[2].Content != "## Git status\nM file.go" || !got[2].Volatile {
		t.Errorf("message 2 = %+v, want volatile trailing user message", got[2])
	}

	transcript := s.GetTranscript()
	if len(transcript) != 2 {
		t.Fatalf("transcript len = %d, want user + assistant only: %+v", len(transcript), transcript)
	}
	for _, m := range transcript {
		if m.Content == "## Git status\nM file.go" || m.Volatile {
			t.Fatalf("volatile context persisted into transcript: %+v", transcript)
		}
	}
}

func TestModelSessionVolatileContextNotPersistedOnModelError(t *testing.T) {
	conn := &captureConnector{err: errors.New("backend down")}
	s := NewModelSession("volatile-error", conn)
	s.SetSystemPrompt("stable-system")
	s.SetVolatileContext("## Task checklist\n- pending")

	if _, err := s.Send([]Message{{Role: RoleUser, Content: "work"}}); err == nil {
		t.Fatal("Send error = nil, want backend error")
	}
	if len(conn.requests) != 1 || len(conn.requests[0]) != 3 {
		t.Fatalf("captured request = %+v, want system + transcript + volatile tail", conn.requests)
	}
	if tail := conn.requests[0][2]; tail.Content != "## Task checklist\n- pending" || !tail.Volatile {
		t.Fatalf("volatile tail not sent on failing request: %+v", tail)
	}

	transcript := s.GetTranscript()
	if len(transcript) != 1 {
		t.Fatalf("transcript len = %d, want attempted user message only: %+v", len(transcript), transcript)
	}
	if transcript[0].Content == "## Task checklist\n- pending" || transcript[0].Volatile {
		t.Fatalf("volatile context persisted after error: %+v", transcript)
	}
}
