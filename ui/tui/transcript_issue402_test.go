package ui

import (
	"strings"
	"testing"

	tv "github.com/hobbestherat/turbotui/turbotv"
)

func TestRestoreReasoningOnlyAssistantRendersThoughtIssue402(t *testing.T) {
	sw := newTestSession()
	sw.restore([]ChatMessage{
		{Role: "user", Content: "finish"},
		{Role: "assistant", Reasoning: "retained reasoning"},
	})

	if len(sw.transcript.records) != 2 {
		t.Fatalf("records = %d, want user plus thought: %+v", len(sw.transcript.records), sw.transcript.records)
	}
	rec := sw.transcript.records[1]
	if rec.kind != kindThinking {
		t.Fatalf("restored reasoning record kind = %v, want kindThinking", rec.kind)
	}
	if rec.header != "thought" {
		t.Fatalf("restored reasoning header = %q, want thought", rec.header)
	}
	if !rec.collapsed {
		t.Fatal("restored reasoning entry should be collapsed like folded live thinking")
	}
	if rec.body() != "retained reasoning" {
		t.Fatalf("restored reasoning body = %q, want retained reasoning", rec.body())
	}
	if got := sw.transcript.view.AllText(); !strings.Contains(got, "thought") {
		t.Fatalf("rendered transcript missing thought header:\n%s", got)
	}
}

func TestRenderTranscriptReasoningOnlyAssistantRendersThoughtIssue402(t *testing.T) {
	history := tv.NewTextView("", tv.Rect{})
	renderTranscript(history, []ChatMessage{
		{Role: "assistant", Reasoning: "persisted thought"},
	})

	got := history.AllText()
	if !strings.Contains(got, "thought") || !strings.Contains(got, "persisted thought") {
		t.Fatalf("rendered history missing restored thought:\n%s", got)
	}
	if strings.Contains(got, "Gogent:") {
		t.Fatalf("reasoning-only assistant rendered an empty visible answer:\n%s", got)
	}
}
