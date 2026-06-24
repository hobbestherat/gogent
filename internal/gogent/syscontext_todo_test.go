package gogent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gogent/internal/agent"
	"gogent/internal/vcs"
)

// TestBuildSystemContextInjectsTodos verifies the live checklist is rendered
// into the per-session system context, keyed by the session id threaded into
// buildSystemContext (issue #263, part A). This is the residency that survives
// compaction: the system prompt is rebuilt each loop from live session state,
// not from the compaction-able transcript.
func TestBuildSystemContextInjectsTodos(t *testing.T) {
	id := "ctx-inject"
	g := newTodoGogent(t, id)
	us := g.GetUserSession(id)

	us.SetTodos([]agent.TodoItem{
		{Content: "Read main.go", Status: agent.TodoCompleted, Note: "bug on line 42"},
		{Content: "Fix it", Status: agent.TodoInProgress},
		{Content: "Add a test", Status: agent.TodoPending},
	})

	_, ctx := g.buildSystemContext(id)

	if !strings.Contains(ctx, "## Task checklist") {
		t.Fatalf("checklist not injected into system context:\n%s", ctx)
	}
	for _, want := range []string{
		"✔ Read main.go (bug on line 42)",
		"◐ Fix it",
		"☐ Add a test",
		"(1 done, 1 in progress, 1 pending)",
	} {
		if !strings.Contains(ctx, want) {
			t.Errorf("system context missing %q:\n%s", want, ctx)
		}
	}
}

// TestBuildSystemContextNoTodosOmitsSection verifies the checklist section is
// absent when the session has no todos, so an idle session's prompt is not
// padded with an empty block (issue #263).
func TestBuildSystemContextNoTodosOmitsSection(t *testing.T) {
	id := "ctx-empty"
	g := newTodoGogent(t, id)

	_, ctx := g.buildSystemContext(id)
	if strings.Contains(ctx, "Task checklist") {
		t.Errorf("checklist header present for an empty checklist:\n%s", ctx)
	}
}

// TestBuildSystemContextReflectsUpdates verifies the injected checklist tracks
// status changes between loops: flipping an item to completed must be visible in
// the next-built context. This is the "re-evaluated each loop" guarantee that
// makes the sidebar and the model's view converge (issue #263).
func TestBuildSystemContextReflectsUpdates(t *testing.T) {
	id := "ctx-update"
	g := newTodoGogent(t, id)
	us := g.GetUserSession(id)

	us.SetTodos([]agent.TodoItem{{Content: "do work", Status: agent.TodoInProgress}})
	_, before := g.buildSystemContext(id)
	if !strings.Contains(before, "◐ do work") {
		t.Fatalf("expected in-progress glyph before update:\n%s", before)
	}

	// The model marks it done on the next turn.
	us.SetTodos([]agent.TodoItem{{Content: "do work", Status: agent.TodoCompleted}})
	_, after := g.buildSystemContext(id)
	if !strings.Contains(after, "✔ do work") {
		t.Errorf("completed status not reflected in re-built context:\n%s", after)
	}
	if strings.Contains(after, "◐ do work") {
		t.Errorf("stale in-progress glyph survived the update:\n%s", after)
	}
	if !strings.Contains(after, "(1 done, 0 in progress, 0 pending)") {
		t.Errorf("summary not updated after status flip:\n%s", after)
	}
}

// TestBuildSystemContextSessionScoped verifies the checklist is per-session: the
// id threaded into buildSystemContext selects whose todos are injected, so one
// session's list never leaks into another's prompt (issue #263, the session-id
// threading requirement).
func TestBuildSystemContextSessionScoped(t *testing.T) {
	g := NewGogent("/tmp/test")
	g.store = nil
	g.NewSession("a")
	g.NewSession("b")

	g.GetUserSession("a").SetTodos([]agent.TodoItem{{Content: "alpha-task", Status: agent.TodoPending}})
	g.GetUserSession("b").SetTodos([]agent.TodoItem{{Content: "beta-task", Status: agent.TodoPending}})

	_, ctxA := g.buildSystemContext("a")
	if !strings.Contains(ctxA, "alpha-task") {
		t.Errorf("session a context missing its own task:\n%s", ctxA)
	}
	if strings.Contains(ctxA, "beta-task") {
		t.Errorf("session b's task leaked into session a's context:\n%s", ctxA)
	}

	_, ctxB := g.buildSystemContext("b")
	if !strings.Contains(ctxB, "beta-task") || strings.Contains(ctxB, "alpha-task") {
		t.Errorf("session b context wrong:\n%s", ctxB)
	}

	// An unknown / empty session id injects no checklist (and must not panic).
	if _, got := g.buildSystemContext("does-not-exist"); strings.Contains(got, "Task checklist") {
		t.Errorf("unknown session id injected a checklist:\n%s", got)
	}
}

// TestBuildSystemContextTodoWiredThroughTool verifies the end-to-end path the
// model actually drives: a write via the todo tool lands in the session and is
// then injected into the system context built for that session (issue #263).
// This couples part B (the tool) and part A (the injection).
func TestBuildSystemContextTodoWiredThroughTool(t *testing.T) {
	id := "ctx-tool"
	g := newTodoGogent(t, id)

	execTodo(t, g, id, map[string]interface{}{
		"todos": []interface{}{
			map[string]interface{}{"content": "ship it", "status": "in_progress", "note": "almost"},
		},
	})

	_, ctx := g.buildSystemContext(id)
	if !strings.Contains(ctx, "◐ ship it (almost)") {
		t.Errorf("tool-written todo (with note) not injected into system context:\n%s", ctx)
	}
}

func TestBuildSystemContextSplitsStableAndVolatileBucketsIssue404(t *testing.T) {
	if !vcs.Available() {
		t.Skip("git not installed")
	}
	repo := t.TempDir()
	for _, args := range [][]string{
		{"init"},
		{"config", "user.email", "test@example.com"},
		{"config", "user.name", "Test"},
		{"config", "commit.gpgsign", "false"},
	} {
		res, err := vcs.Run(repo, vcs.DefaultTimeout, args...)
		if err != nil {
			t.Fatalf("git %v: %v", args, err)
		}
		if !res.OK() {
			t.Fatalf("git %v failed: exit=%d stderr=%s", args, res.ExitCode, res.Stderr)
		}
	}
	if err := os.WriteFile(filepath.Join(repo, "changed.txt"), []byte("dirty\n"), 0644); err != nil {
		t.Fatal(err)
	}

	const id = "ctx-split"
	g := NewGogentWithWorkspace(t.TempDir(), repo)
	g.store = nil
	g.gitRepo = true
	g.agentsContext = "## Project instructions (AGENTS.md)\nstable agent rules"
	g.repoMap = "## Repository map\nstable repo map"
	g.NewSession(id)
	g.GetUserSession(id).SetTodos([]agent.TodoItem{{Content: "volatile todo", Status: agent.TodoPending}})

	stable, volatile := g.buildSystemContext(id)
	for _, want := range []string{"stable agent rules", "stable repo map"} {
		if !strings.Contains(stable, want) {
			t.Errorf("stable context missing %q:\n%s", want, stable)
		}
	}
	for _, forbidden := range []string{"## Git status", "changed.txt", "## Task checklist", "volatile todo"} {
		if strings.Contains(stable, forbidden) {
			t.Errorf("stable context contains volatile marker %q:\n%s", forbidden, stable)
		}
	}
	for _, want := range []string{"## Git status", "changed.txt", "## Task checklist", "volatile todo"} {
		if !strings.Contains(volatile, want) {
			t.Errorf("volatile context missing %q:\n%s", want, volatile)
		}
	}
	for _, forbidden := range []string{"stable agent rules", "stable repo map"} {
		if strings.Contains(volatile, forbidden) {
			t.Errorf("volatile context contains stable marker %q:\n%s", forbidden, volatile)
		}
	}
}
