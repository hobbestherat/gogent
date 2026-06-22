package agent

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// systemMsgContains reports whether any system-role message in a captured
// request carries sub. The system prompt is prepended to every request as a
// {role:"system"} message (model_session.go), so this is where an injected
// checklist would land.
func systemMsgContains(req []map[string]interface{}, sub string) bool {
	for _, m := range req {
		if m["role"] != "system" {
			continue
		}
		if c, _ := m["content"].(string); strings.Contains(c, sub) {
			return true
		}
	}
	return false
}

// TestSystemContextChecklistInjectedAtTurnStart is the positive control for
// issue #263 part A: a checklist that already exists when a turn begins must be
// rendered into the system prompt of that turn's first model request. This pins
// that the SystemContextProvider is actually wired into runLoop (not just unit-
// testable in isolation).
func TestSystemContextChecklistInjectedAtTurnStart(t *testing.T) {
	fs := &fakeServer{responses: []map[string]interface{}{finalResponse("done")}}
	server := httptest.NewServer(http.HandlerFunc(fs.handler))
	defer server.Close()

	us, _ := newLoopSession(t, server.URL)
	us.SetSystemContextProvider(func(string) string { return RenderTodos(us.Todos()) })
	us.SetTodos([]TodoItem{{Content: "PREEXISTING-TASK", Status: TodoInProgress}})

	if _, err := us.ExecuteTaskLoop(context.Background(), "root", "go"); err != nil {
		t.Fatalf("loop error: %v", err)
	}
	if len(fs.requests) == 0 {
		t.Fatal("no request captured")
	}
	if !systemMsgContains(fs.requests[0], "PREEXISTING-TASK") {
		t.Errorf("a checklist present at turn start was not injected into the system prompt;\nreq0 = %v", fs.requests[0])
	}
}

// TestSystemContextChecklistRefreshedWithinTurn is the regression test for the
// core acceptance criterion of issue #263: "The checklist is injected into the
// system prompt every turn and survives compaction."
//
// The reported failure mode is intra-turn: an agent lays out its checklist via
// the todo tool partway through one long autonomous run (one runLoop, many model
// round-trips), then a compaction fires and the originating todo tool calls are
// summarized out of the transcript. For the model to still see the checklist,
// the system prompt — the one place compaction does not touch — must reflect the
// checklist on EVERY round-trip, not just a snapshot taken before the first one.
//
// This test simulates the model creating the checklist during the first tool
// round (the handler sets the session's todos right after serving request 0),
// then asserts the checklist appears in the system prompt of the next request in
// the SAME turn. If the provider is only evaluated once at runLoop start, the
// second request's system prompt is stale and the checklist is absent — exactly
// the bug.
func TestSystemContextChecklistRefreshedWithinTurn(t *testing.T) {
	fs := &fakeServer{responses: []map[string]interface{}{
		toolCallResponse("c1", "calc", `{"expression":"1+1"}`),
		finalResponse("done"),
	}}

	var us *UserSession
	var mu sync.Mutex
	todoWritten := false

	// Wrap fs.handler: after serving the first request, simulate the model
	// having written a checklist mid-turn by setting the session's todos.
	h := func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		first := !todoWritten
		mu.Unlock()

		fs.handler(w, r) // captures the request, then serves the canned response

		if first {
			us.SetTodos([]TodoItem{{Content: "MIDTURN-CHECKLIST-MARKER", Status: TodoInProgress}})
			mu.Lock()
			todoWritten = true
			mu.Unlock()
		}
	}
	server := httptest.NewServer(http.HandlerFunc(h))
	defer server.Close()

	us, _ = newLoopSession(t, server.URL)
	us.SetSystemContextProvider(func(string) string { return RenderTodos(us.Todos()) })

	if _, err := us.ExecuteTaskLoop(context.Background(), "root", "go"); err != nil {
		t.Fatalf("loop error: %v", err)
	}
	if len(fs.requests) < 2 {
		t.Fatalf("expected at least 2 model requests, got %d", len(fs.requests))
	}

	// The mid-turn checklist must be visible in the system prompt of the request
	// that follows its creation (and of every later request in the turn).
	if !systemMsgContains(fs.requests[1], "MIDTURN-CHECKLIST-MARKER") {
		t.Errorf("checklist created mid-turn did NOT reach the next request's system prompt " +
			"in the same turn — it is injected only once at turn start, so it cannot survive " +
			"an intra-turn compaction (issue #263 acceptance criterion A);\nreq1 system messages absent the marker")
	}
}
