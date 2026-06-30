package agent

import (
	"fmt"
	"sync"
	"testing"

	"gogent/internal/model"
)

func TestAgentCreate(t *testing.T) {
	m := newTestModelConnection()
	s := model.NewModelSession("test1", m)
	a := NewAgent("agent1", s)

	if a.ID != "agent1" {
		t.Errorf("Expected ID 'agent1', got %q", a.ID)
	}

	if a.ThoughtTrain != s {
		t.Error("Expected session to be set")
	}

	if a.State != StateIdle {
		t.Errorf("Expected state Idle, got %v", a.State)
	}
}

func TestAgentAddSubAgent(t *testing.T) {
	m := newTestModelConnection()
	s := model.NewModelSession("test2", m)
	a := NewAgent("parent", s)
	sub := NewAgent("child", s)

	a.AddSubAgent(sub)

	subAgents := a.GetSubAgents()
	if len(subAgents) != 1 {
		t.Errorf("Expected 1 sub-agent, got %d", len(subAgents))
	}

	if subAgents[0] != sub {
		t.Error("Expected sub-agent to be added")
	}

	if sub.Parent != a {
		t.Error("Expected parent to be set")
	}
}

func TestAgentRemoveSubAgent(t *testing.T) {
	m := newTestModelConnection()
	s := model.NewModelSession("test3", m)
	a := NewAgent("parent", s)
	sub1 := NewAgent("child1", s)
	sub2 := NewAgent("child2", s)

	a.AddSubAgent(sub1)
	a.AddSubAgent(sub2)

	if len(a.GetSubAgents()) != 2 {
		t.Errorf("Expected 2 sub-agents, got %d", len(a.GetSubAgents()))
	}

	a.RemoveSubAgent("child1")

	if len(a.GetSubAgents()) != 1 {
		t.Errorf("Expected 1 sub-agent after removal, got %d", len(a.GetSubAgents()))
	}
}

func TestAgentGetSubAgent(t *testing.T) {
	m := newTestModelConnection()
	s := model.NewModelSession("test4", m)
	a := NewAgent("parent", s)
	sub := NewAgent("child", s)

	a.AddSubAgent(sub)

	found := a.GetSubAgent("child")
	if found != sub {
		t.Error("Expected to find sub-agent")
	}

	notFound := a.GetSubAgent("nonexistent")
	if notFound != nil {
		t.Error("Expected not to find non-existent agent")
	}
}

func TestAgentGetRootAgent(t *testing.T) {
	m := newTestModelConnection()
	s := model.NewModelSession("test5", m)
	root := NewAgent("root", s)
	child := NewAgent("child", s)
	grandChild := NewAgent("grandChild", s)

	root.AddSubAgent(child)
	child.AddSubAgent(grandChild)

	if root.GetRootAgent() != root {
		t.Error("Root's root should be itself")
	}

	if child.GetRootAgent() != root {
		t.Error("Child's root should be root")
	}

	if grandChild.GetRootAgent() != root {
		t.Error("Grandchild's root should be root")
	}
}

func TestAgentListAllAgents(t *testing.T) {
	m := newTestModelConnection()
	s := model.NewModelSession("test6", m)
	root := NewAgent("root", s)
	child1 := NewAgent("child1", s)
	child2 := NewAgent("child2", s)
	grandChild := NewAgent("grandChild", s)

	root.AddSubAgent(child1)
	root.AddSubAgent(child2)
	child1.AddSubAgent(grandChild)

	all := root.ListAllAgents()
	if len(all) != 4 {
		t.Errorf("Expected 4 agents, got %d", len(all))
	}

	// Verify all IDs
	ids := make(map[string]bool)
	for _, a := range all {
		ids[a.ID] = true
	}

	if !ids["root"] || !ids["child1"] || !ids["child2"] || !ids["grandChild"] {
		t.Errorf("Not all agents found in list")
	}
}

func TestAgentGetAgentByID(t *testing.T) {
	m := newTestModelConnection()
	s := model.NewModelSession("test7", m)
	root := NewAgent("root", s)
	child := NewAgent("child", s)
	grandChild := NewAgent("grandChild", s)

	root.AddSubAgent(child)
	child.AddSubAgent(grandChild)

	found := root.GetAgentByID("grandChild")
	if found != grandChild {
		t.Error("Expected to find grandChild")
	}

	notFound := root.GetAgentByID("nonexistent")
	if notFound != nil {
		t.Error("Expected not to find non-existent agent")
	}
}

func TestAgentSetState(t *testing.T) {
	m := newTestModelConnection()
	s := model.NewModelSession("test8", m)
	a := NewAgent("agent", s)

	if a.GetState() != StateIdle {
		t.Errorf("Expected initial state Idle, got %v", a.GetState())
	}

	a.SetState(StateThinking)

	if a.GetState() != StateThinking {
		t.Errorf("Expected state Thinking, got %v", a.GetState())
	}
}

func TestAgentUpdateState(t *testing.T) {
	m := newTestModelConnection()
	s := model.NewModelSession("test9", m)
	a := NewAgent("agent", s)

	old := a.UpdateState(StateThinking)
	if old != StateIdle {
		t.Errorf("Expected old state Idle, got %v", old)
	}

	if a.GetState() != StateThinking {
		t.Errorf("Expected state Thinking, got %v", a.GetState())
	}
}

// TestAgentStateChangeCallback verifies the lifecycle observer wired for hooks
// (issue #47): it fires on a real transition (via both SetState and UpdateState),
// stays silent on a no-op transition, and can be cleared.
func TestAgentStateChangeCallback(t *testing.T) {
	a := NewAgent("agent", nil)

	var got [][2]AgentState
	a.SetStateChangeCallback(func(old, new AgentState) {
		got = append(got, [2]AgentState{old, new})
	})

	a.SetState(StateThinking) // idle -> thinking: fires
	a.SetState(StateThinking) // thinking -> thinking: no-op, no fire
	if old := a.UpdateState(StateIdle); old != StateThinking {
		t.Errorf("UpdateState returned old %v, want thinking", old)
	}

	want := [][2]AgentState{
		{StateIdle, StateThinking},
		{StateThinking, StateIdle},
	}
	if len(got) != len(want) {
		t.Fatalf("callback fired %d times (%v), want %d", len(got), got, len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("transition %d = %v, want %v", i, got[i], want[i])
		}
	}

	// A nil callback disables further notifications.
	a.SetStateChangeCallback(nil)
	a.SetState(StateThinking)
	if len(got) != len(want) {
		t.Errorf("callback fired after being cleared: %v", got)
	}
}

func TestAgentGetParent(t *testing.T) {
	m := newTestModelConnection()
	s := model.NewModelSession("test10", m)
	root := NewAgent("root", s)
	child := NewAgent("child", s)
	grandChild := NewAgent("grandChild", s)

	root.AddSubAgent(child)
	child.AddSubAgent(grandChild)

	cases := []struct {
		name  string
		agent *Agent
		want  *Agent
	}{
		{"root has no parent", root, nil},
		{"child parent is root", child, root},
		{"grandchild parent is child", grandChild, child},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.agent.GetParent(); got != tc.want {
				t.Errorf("GetParent() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestAgentTreeConcurrentReadWrite exercises the race described in issue #11:
// parallel AddSubAgent calls append to a parent's SubAgents slice while reader
// goroutines traverse that same slice via ListAllAgents / GetAgentByID. Every
// tree read must be lock-guarded so the concurrent append never tears the slice
// header under the readers. It also checks the post-condition invariants
// (complete child set, correct parent and root links) once all writers finish.
func TestAgentTreeConcurrentReadWrite(t *testing.T) {
	m := newTestModelConnection()
	s := model.NewModelSession("test11", m)
	root := NewAgent("root", s)

	const (
		writers           = 8
		childrenPerWriter = 64
		readers           = 4
	)
	totalChildren := writers * childrenPerWriter

	// Writers add distinct children under the root in parallel.
	var writersDone sync.WaitGroup
	writersDone.Add(writers)
	for w := 0; w < writers; w++ {
		w := w
		go func() {
			defer writersDone.Done()
			for i := 0; i < childrenPerWriter; i++ {
				root.AddSubAgent(NewAgent(fmt.Sprintf("child-%d-%d", w, i), s))
			}
		}()
	}

	// Readers hammer the tree traversals until the writers finish. Under the old
	// code these unlocked reads raced with AddSubAgent's append.
	stop := make(chan struct{})
	var readersDone sync.WaitGroup
	readersDone.Add(readers)
	for r := 0; r < readers; r++ {
		go func() {
			defer readersDone.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				_ = root.ListAllAgents()
				_ = root.GetAgentByID("root")
			}
		}()
	}

	writersDone.Wait()
	close(stop)
	readersDone.Wait()

	// Every appended child must be present exactly once.
	subs := root.GetSubAgents()
	if len(subs) != totalChildren {
		t.Fatalf("expected %d children, got %d", totalChildren, len(subs))
	}
	all := root.ListAllAgents()
	if len(all) != totalChildren+1 {
		t.Fatalf("expected %d agents in tree, got %d", totalChildren+1, len(all))
	}

	seen := make(map[string]bool, totalChildren+1)
	for _, a := range all {
		if seen[a.ID] {
			t.Errorf("agent %q surfaced more than once", a.ID)
		}
		seen[a.ID] = true
	}
	if !seen["root"] {
		t.Errorf("root missing from ListAllAgents")
	}

	// Parent and root links must resolve through the locked accessors.
	for _, sub := range subs {
		if sub.GetParent() != root {
			t.Errorf("child %q parent is not root", sub.ID)
		}
		if sub.GetRootAgent() != root {
			t.Errorf("child %q root is not root", sub.ID)
		}
	}
}
