package ui

import (
	"strings"
	"testing"

	"gogent/internal/config"
)

func newForkWorkbench(t *testing.T) *Workbench {
	t.Helper()
	return NewWorkbench([]*config.ModelConfig{
		{Name: "base", DisplayName: "Base", Model: "base", EffortOptions: []string{"low"}},
		{Name: "alt", DisplayName: "Alt", Model: "alt", EffortOptions: []string{"high", "max"}},
	})
}

func TestIssue349SlashForkOpensFocusesAndDispatchesFork(t *testing.T) {
	w := newForkWorkbench(t)
	parent := w.openWindow("parent", "Parent")
	parent.modelSelect.SetSelected(1)
	parent.rebuildEffortOptions()
	parent.effortSelect.SetSelected(1) // "high"

	var transcriptSessionID string
	var transcriptAgentID string
	w.handlers.GetTranscript = func(sessionID, agentID string) []ChatMessage {
		transcriptSessionID = sessionID
		transcriptAgentID = agentID
		return []ChatMessage{
			{Role: "user", Content: "original question"},
			{Role: "assistant", Content: "original answer"},
			{Role: "tool", Tool: "read_file", Content: "result body"},
		}
	}
	var forkParentID string
	var forkNewID string
	var forkTitle string
	w.handlers.OnFork = func(parentSessionID, newSessionID, title string) {
		forkParentID = parentSessionID
		forkNewID = newSessionID
		forkTitle = title
	}

	if !parent.handleSlashCommand("/fork") {
		t.Fatal("/fork was not handled as a slash command")
	}

	if transcriptSessionID != "parent" || transcriptAgentID != "root" {
		t.Fatalf("GetTranscript called with (%q, %q), want (parent, root)", transcriptSessionID, transcriptAgentID)
	}
	if forkParentID != "parent" || forkNewID != "session-1" || forkTitle != "Session 1" {
		t.Fatalf("OnFork called with (%q, %q, %q), want (parent, session-1, Session 1)", forkParentID, forkNewID, forkTitle)
	}
	if order := w.orderIDs(); !equalOrder(order, []string{"parent", "session-1"}) {
		t.Fatalf("session order = %v, want [parent session-1]", order)
	}
	if got := w.ActiveID(); got != "session-1" {
		t.Fatalf("ActiveID after /fork = %q, want session-1", got)
	}

	fork := w.sessions["session-1"]
	if fork == nil {
		t.Fatal("fork window was not opened")
	}
	if !fork.input.Component.Focused() {
		t.Fatal("fork input is not focused")
	}
	if got := fork.selectedModelName(); got != "alt" {
		t.Fatalf("fork selected model = %q, want alt", got)
	}
	if got := fork.selectedEffort(); got != "high" {
		t.Fatalf("fork selected effort = %q, want high", got)
	}
	for _, want := range []string{"original question", "original answer", "result body"} {
		if !noteContains(fork, want) {
			t.Fatalf("fork transcript missing %q; records=%#v", want, fork.transcript.records)
		}
	}
	if noteContains(parent, "original question") {
		t.Fatal("parent window transcript should not be modified by /fork restore")
	}
}

func TestIssue349ForkSessionUnknownParentIsNoOp(t *testing.T) {
	w := newForkWorkbench(t)
	called := false
	w.handlers.OnFork = func(parentSessionID, newSessionID, title string) { called = true }

	if got := w.ForkSession("missing"); got != nil {
		t.Fatalf("ForkSession for missing parent returned %#v, want nil", got)
	}
	if called {
		t.Fatal("OnFork should not be called for an unknown parent")
	}
	if order := w.orderIDs(); len(order) != 0 {
		t.Fatalf("unknown-parent fork opened sessions: %v", order)
	}
}

func TestIssue349ForkSessionWithoutBackendHandlerIsNoOp(t *testing.T) {
	w := newForkWorkbench(t)
	parent := w.openWindow("parent", "Parent")
	w.handlers.OnFork = nil
	w.handlers.GetTranscript = func(sessionID, agentID string) []ChatMessage {
		return []ChatMessage{{Role: "user", Content: "should not be copied without backend fork"}}
	}

	if got := w.ForkSession("parent"); got != nil {
		t.Fatalf("ForkSession without OnFork returned %#v, want nil", got)
	}
	if order := w.orderIDs(); !equalOrder(order, []string{"parent"}) {
		t.Fatalf("ForkSession without OnFork changed session order to %v, want [parent]", order)
	}
	if got := w.ActiveID(); got != "parent" {
		t.Fatalf("ForkSession without OnFork active session = %q, want parent", got)
	}
	if !parent.input.Component.Focused() {
		t.Fatal("ForkSession without OnFork should leave parent focused")
	}
	if noteContains(parent, "should not be copied") {
		t.Fatal("ForkSession without OnFork should not restore copied history anywhere")
	}
}

func TestIssue349ForkCommandPaletteDiscoverableAndDispatches(t *testing.T) {
	w := newForkWorkbench(t)
	w.openWindow("parent", "Parent")

	cmd := findCommandByKeys(w.commands(), "/fork")
	if cmd == nil {
		t.Fatal("commands() missing /fork entry")
	}
	if cmd.category != "Session" || strings.TrimSpace(cmd.name) == "" || cmd.run == nil {
		t.Fatalf("/fork command malformed: %+v", *cmd)
	}
	if text := helpText(w.commands()); !strings.Contains(text, "/fork") {
		t.Fatalf("helpText missing /fork\n%s", text)
	}

	var forkParentID string
	var forkNewID string
	w.handlers.OnFork = func(parentSessionID, newSessionID, title string) {
		forkParentID = parentSessionID
		forkNewID = newSessionID
	}
	cmd.run()
	if forkParentID != "parent" || forkNewID != "session-1" {
		t.Fatalf("/fork palette action called OnFork with (%q, %q), want (parent, session-1)", forkParentID, forkNewID)
	}
	if got := w.ActiveID(); got != "session-1" {
		t.Fatalf("ActiveID after palette /fork = %q, want session-1", got)
	}
}
