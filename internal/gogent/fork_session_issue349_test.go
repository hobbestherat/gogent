package gogent

import (
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"gogent/internal/agent"
	"gogent/internal/model"
)

func TestIssue349ForkSessionCopiesFullTranscriptAndTodos(t *testing.T) {
	g := NewGogent(t.TempDir())
	parent := g.NewSession("parent")
	parent.SetPrimaryModel("parent-model")
	parent.SetTodos([]agent.TodoItem{
		{Content: "keep context", Status: agent.TodoInProgress, Note: "branch should inherit this"},
		{Content: "ship fork", Status: agent.TodoPending},
	})

	wantTranscript := []model.Message{
		{Role: model.RoleUser, Content: "first user message", Images: []model.ImageURL{{URL: "data:image/png;base64,aaa", Detail: "high"}}},
		{Role: model.RoleAssistant, Content: "assistant answer", ToolCalls: []model.ToolCall{{
			ID: "call-1", Type: "function", Function: model.FunctionCall{Name: "read_file", Arguments: `{"path":"README.md"}`},
		}}},
		{Role: model.RoleTool, ToolCallID: "call-1", Name: "read_file", Content: "file contents"},
		{Role: model.RoleUser, Content: "follow-up"},
	}
	parent.RootAgent.ThoughtTrain.ReplaceTranscript(wantTranscript)

	forked, err := g.ForkSession("parent", "fork")
	if err != nil {
		t.Fatalf("ForkSession: %v", err)
	}
	if forked == nil {
		t.Fatal("ForkSession returned nil session")
	}
	if got := g.GetUserSession("fork"); got != forked {
		t.Fatal("forked session was not registered under the new id")
	}
	if forked == parent {
		t.Fatal("ForkSession returned the parent session")
	}
	if got := forked.PrimaryModel(); got != "parent-model" {
		t.Fatalf("fork PrimaryModel = %q, want parent-model", got)
	}
	if got := forked.RootAgent.State; got != agent.StateIdle {
		t.Fatalf("fork root state = %v, want idle", got)
	}
	if got := forked.RootAgent.ThoughtTrain.GetTranscript(); !reflect.DeepEqual(got, wantTranscript) {
		t.Fatalf("fork transcript mismatch\n got: %#v\nwant: %#v", got, wantTranscript)
	}
	if got := forked.Todos(); !reflect.DeepEqual(got, parent.Todos()) {
		t.Fatalf("fork todos = %#v, want %#v", got, parent.Todos())
	}
}

func TestIssue349ForkSessionTranscriptDivergesIndependently(t *testing.T) {
	g := NewGogent(t.TempDir())
	parent := g.NewSession("parent")
	parent.RootAgent.ThoughtTrain.ReplaceTranscript([]model.Message{
		{Role: model.RoleUser, Content: "with image", Images: []model.ImageURL{{URL: "data:image/png;base64,parent", Detail: "high"}}},
		{Role: model.RoleAssistant, Content: "with tool", ToolCalls: []model.ToolCall{{
			ID: "call-parent", Type: "function", Function: model.FunctionCall{Name: "tool", Arguments: `{"from":"parent"}`},
		}}},
	})
	parent.SetTodos([]agent.TodoItem{{Content: "parent todo", Status: agent.TodoPending, Note: "original"}})

	forked, err := g.ForkSession("parent", "fork")
	if err != nil {
		t.Fatalf("ForkSession: %v", err)
	}

	forked.RootAgent.ThoughtTrain.AppendMessages(model.Message{Role: model.RoleUser, Content: "fork-only follow-up"})
	forked.SetTodos([]agent.TodoItem{{Content: "fork todo", Status: agent.TodoCompleted, Note: "changed"}})

	parentTranscript := parent.RootAgent.ThoughtTrain.GetTranscript()
	if len(parentTranscript) != 2 {
		t.Fatalf("parent transcript length after fork append = %d, want 2", len(parentTranscript))
	}
	if got := parentTranscript[len(parentTranscript)-1].Content; got != "with tool" {
		t.Fatalf("parent last message after fork append = %q, want with tool", got)
	}
	if got := parent.Todos(); len(got) != 1 || got[0].Content != "parent todo" || got[0].Status != agent.TodoPending {
		t.Fatalf("parent todos changed after mutating fork: %#v", got)
	}

	forked.RootAgent.ThoughtTrain.Transcript[0].Images[0].URL = "data:image/png;base64,fork"
	forked.RootAgent.ThoughtTrain.Transcript[1].ToolCalls[0].ID = "call-fork"
	forked.RootAgent.ThoughtTrain.Transcript[1].ToolCalls[0].Function.Arguments = `{"from":"fork"}`

	parentTranscript = parent.RootAgent.ThoughtTrain.GetTranscript()
	if got := parentTranscript[0].Images[0].URL; got != "data:image/png;base64,parent" {
		t.Fatalf("parent image URL changed through fork mutation: %q", got)
	}
	if got := parentTranscript[1].ToolCalls[0].ID; got != "call-parent" {
		t.Fatalf("parent tool call ID changed through fork mutation: %q", got)
	}
	if got := parentTranscript[1].ToolCalls[0].Function.Arguments; got != `{"from":"parent"}` {
		t.Fatalf("parent tool call args changed through fork mutation: %q", got)
	}
}

func TestIssue349ForkSessionErrors(t *testing.T) {
	g := NewGogent(t.TempDir())

	if got, err := g.ForkSession("missing", "fork"); err == nil || got != nil || !strings.Contains(err.Error(), "parent session") {
		t.Fatalf("ForkSession missing parent = (%v, %v), want nil parent-session error", got, err)
	}

	g.NewSession("parent")
	g.NewSession("already")
	if got, err := g.ForkSession("parent", "already"); err == nil || got != nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("ForkSession duplicate id = (%v, %v), want nil duplicate error", got, err)
	}
}

func TestIssue349ConcurrentForkSessionSameIDCreatesAtMostOneFork(t *testing.T) {
	g := NewGogent(t.TempDir())
	parent := g.NewSession("parent")
	parent.RootAgent.ThoughtTrain.ReplaceTranscript([]model.Message{
		{Role: model.RoleUser, Content: "shared starting point"},
	})

	const workers = 32
	start := make(chan struct{})
	var wg sync.WaitGroup
	var successes atomic.Int32
	var failures atomic.Int32
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			forked, err := g.ForkSession("parent", "fork")
			if err == nil {
				if forked == nil {
					t.Errorf("ForkSession returned nil without error")
					return
				}
				successes.Add(1)
				return
			}
			if !strings.Contains(err.Error(), "already exists") {
				t.Errorf("ForkSession concurrent error = %v, want already-exists error", err)
			}
			failures.Add(1)
		}()
	}
	close(start)
	wg.Wait()

	if successes.Load() != 1 {
		t.Fatalf("concurrent ForkSession successes = %d, want exactly 1", successes.Load())
	}
	if failures.Load() != workers-1 {
		t.Fatalf("concurrent ForkSession failures = %d, want %d", failures.Load(), workers-1)
	}

	forked := g.GetUserSession("fork")
	if forked == nil {
		t.Fatal("winning fork was not registered")
	}
	if got := forked.RootAgent.ThoughtTrain.GetTranscript(); !reflect.DeepEqual(got, parent.RootAgent.ThoughtTrain.GetTranscript()) {
		t.Fatalf("winning fork transcript = %#v, want parent transcript", got)
	}
}
