package agent

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gogent/internal/model"
)

// TestTodosComplete covers the rule-based short-circuit classification (issue
// #172): empty list is not decisive, an all-completed list is done, any
// incomplete item is not done.
func TestTodosComplete(t *testing.T) {
	cases := []struct {
		name        string
		todos       []TodoItem
		wantAllDone bool
		wantHas     bool
	}{
		{"empty", nil, false, false},
		{"all complete", []TodoItem{
			{Content: "a", Status: TodoCompleted},
			{Content: "b", Status: TodoCompleted},
		}, true, true},
		{"one pending", []TodoItem{
			{Content: "a", Status: TodoCompleted},
			{Content: "b", Status: TodoPending},
		}, false, true},
		{"in progress", []TodoItem{
			{Content: "a", Status: TodoInProgress},
		}, false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			allDone, has := TodosComplete(tc.todos)
			if allDone != tc.wantAllDone || has != tc.wantHas {
				t.Fatalf("TodosComplete=%v,%v want %v,%v", allDone, has, tc.wantAllDone, tc.wantHas)
			}
		})
	}
}

// TestCheckGoalComplete drives the completion-check decision tree (issue #172):
// the todo short-circuit beats the judge; the judge decides only when todos are
// not decisive; an empty goal is trivially done.
func TestCheckGoalComplete(t *testing.T) {
	yes := func(string, []model.Message) (bool, error) { return true, nil }
	no := func(string, []model.Message) (bool, error) { return false, nil }
	boom := func(string, []model.Message) (bool, error) { return false, errors.New("boom") }

	t.Run("empty goal is done without judging", func(t *testing.T) {
		called := false
		judge := func(string, []model.Message) (bool, error) { called = true; return false, nil }
		done, err := CheckGoalComplete("   ", nil, judge, nil)
		if !done || err != nil || called {
			t.Fatalf("empty goal: done=%v err=%v judged=%v", done, err, called)
		}
	})

	t.Run("all todos complete short-circuits done", func(t *testing.T) {
		called := false
		judge := func(string, []model.Message) (bool, error) { called = true; return false, nil }
		done, err := CheckGoalComplete("g", []TodoItem{{Content: "x", Status: TodoCompleted}}, judge, nil)
		if !done || err != nil || called {
			t.Fatalf("complete todos: done=%v err=%v judged=%v", done, err, called)
		}
	})

	t.Run("incomplete todos short-circuit not-done without judging", func(t *testing.T) {
		called := false
		judge := func(string, []model.Message) (bool, error) { called = true; return true, nil }
		done, err := CheckGoalComplete("g", []TodoItem{{Content: "x", Status: TodoPending}}, judge, nil)
		if done || err != nil || called {
			t.Fatalf("incomplete todos: done=%v err=%v judged=%v", done, err, called)
		}
	})

	t.Run("no todos defers to judge yes", func(t *testing.T) {
		done, err := CheckGoalComplete("g", nil, yes, nil)
		if !done || err != nil {
			t.Fatalf("judge yes: done=%v err=%v", done, err)
		}
	})

	t.Run("no todos defers to judge no", func(t *testing.T) {
		done, err := CheckGoalComplete("g", nil, no, nil)
		if done || err != nil {
			t.Fatalf("judge no: done=%v err=%v", done, err)
		}
	})

	t.Run("judge error propagates as not done", func(t *testing.T) {
		done, err := CheckGoalComplete("g", nil, boom, nil)
		if done || err == nil {
			t.Fatalf("judge error: done=%v err=%v", done, err)
		}
	})

	t.Run("nil judge with no decisive todos is not done", func(t *testing.T) {
		done, err := CheckGoalComplete("g", nil, nil, nil)
		if done || err != nil {
			t.Fatalf("nil judge: done=%v err=%v", done, err)
		}
	})
}

// TestParseYesNo verifies the lightweight yes/no parser tolerates surrounding
// punctuation and prose while defaulting to "not done".
func TestParseYesNo(t *testing.T) {
	yes := []string{"yes", "Yes.", "  YES", "**yes**", "yes, it is complete", "true", "Done"}
	no := []string{"no", "No.", "not yet", "almost", "", "the goal is not met"}
	for _, s := range yes {
		if !parseYesNo(s) {
			t.Errorf("parseYesNo(%q) = false, want true", s)
		}
	}
	for _, s := range no {
		if parseYesNo(s) {
			t.Errorf("parseYesNo(%q) = true, want false", s)
		}
	}
}

// TestGoalSatisfiedModelJudge exercises the production wiring end-to-end through
// a fakeServer: with no todos, GoalSatisfied issues a single completion and maps
// its yes/no answer to the verdict (issue #172). It also confirms the todo
// short-circuit avoids the model call entirely.
func TestGoalSatisfiedModelJudge(t *testing.T) {
	t.Run("model says yes", func(t *testing.T) {
		fs := &fakeServer{responses: []map[string]interface{}{finalResponse("yes")}}
		server := httptest.NewServer(http.HandlerFunc(fs.handler))
		defer server.Close()
		us, _ := newLoopSession(t, server.URL)

		done, err := us.GoalSatisfied("ship the feature")
		if err != nil || !done {
			t.Fatalf("done=%v err=%v", done, err)
		}
		if fs.calls != 1 {
			t.Fatalf("expected exactly 1 model call, got %d", fs.calls)
		}
	})

	t.Run("model says no", func(t *testing.T) {
		fs := &fakeServer{responses: []map[string]interface{}{finalResponse("no, still TODO")}}
		server := httptest.NewServer(http.HandlerFunc(fs.handler))
		defer server.Close()
		us, _ := newLoopSession(t, server.URL)

		done, err := us.GoalSatisfied("ship the feature")
		if err != nil || done {
			t.Fatalf("done=%v err=%v", done, err)
		}
	})

	t.Run("todo short-circuit skips the model", func(t *testing.T) {
		fs := &fakeServer{responses: []map[string]interface{}{finalResponse("no")}}
		server := httptest.NewServer(http.HandlerFunc(fs.handler))
		defer server.Close()
		us, _ := newLoopSession(t, server.URL)
		us.SetTodos([]TodoItem{{Content: "done it", Status: TodoCompleted}})

		done, err := us.GoalSatisfied("anything")
		if err != nil || !done {
			t.Fatalf("done=%v err=%v", done, err)
		}
		if fs.calls != 0 {
			t.Fatalf("expected 0 model calls (todo short-circuit), got %d", fs.calls)
		}
	})
}

// TestSupervisorNudgeHelpers checks the nudge template formatting and detection.
func TestSupervisorNudgeHelpers(t *testing.T) {
	n := SupervisorNudge("finish the docs")
	if !strings.Contains(n, "finish the docs") || !IsSupervisorNudge(n) {
		t.Fatalf("nudge %q not well-formed / not detected", n)
	}
	if IsSupervisorNudge("a normal user message") {
		t.Fatal("normal message misdetected as supervisor nudge")
	}
}
