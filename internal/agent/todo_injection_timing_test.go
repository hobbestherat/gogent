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
// request carries sub.
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

func trailingUserMsgContains(req []map[string]interface{}, sub string) bool {
	if len(req) == 0 {
		return false
	}
	last := req[len(req)-1]
	if last["role"] != "user" {
		return false
	}
	c, _ := last["content"].(string)
	return strings.Contains(c, sub)
}

// TestVolatileChecklistInjectedAtTurnStart is the positive control for issue
// #263/#404: a checklist that already exists when a turn begins must be rendered
// into that turn's first model request, but as the trailing volatile user
// message, not in the cacheable system prompt.
func TestVolatileChecklistInjectedAtTurnStartIssue404(t *testing.T) {
	fs := &fakeServer{responses: []map[string]interface{}{finalResponse("done")}}
	server := httptest.NewServer(http.HandlerFunc(fs.handler))
	defer server.Close()

	us, _ := newLoopSession(t, server.URL)
	us.SetSystemContextProvider(func(string) (string, string) { return "", RenderTodos(us.Todos()) })
	us.SetTodos([]TodoItem{{Content: "PREEXISTING-TASK", Status: TodoInProgress}})

	if _, err := us.ExecuteTaskLoop(context.Background(), "root", "go"); err != nil {
		t.Fatalf("loop error: %v", err)
	}
	if len(fs.requests) == 0 {
		t.Fatal("no request captured")
	}
	if systemMsgContains(fs.requests[0], "PREEXISTING-TASK") {
		t.Errorf("volatile checklist leaked into the system prompt;\nreq0 = %v", fs.requests[0])
	}
	if !trailingUserMsgContains(fs.requests[0], "PREEXISTING-TASK") {
		t.Errorf("a checklist present at turn start was not injected as the trailing volatile message;\nreq0 = %v", fs.requests[0])
	}
}

func TestRunLoopSplitsStableSystemAndTrailingVolatileContextIssue404(t *testing.T) {
	fs := &fakeServer{responses: []map[string]interface{}{finalResponse("done")}}
	server := httptest.NewServer(http.HandlerFunc(fs.handler))
	defer server.Close()

	us, _ := newLoopSession(t, server.URL)
	us.SetSystemContextProvider(func(string) (string, string) {
		return "STABLE-CONTEXT-MARKER", "VOLATILE-CONTEXT-MARKER"
	})

	if _, err := us.ExecuteTaskLoop(context.Background(), "root", "go"); err != nil {
		t.Fatalf("loop error: %v", err)
	}
	if len(fs.requests) == 0 {
		t.Fatal("no request captured")
	}
	req := fs.requests[0]
	if !systemMsgContains(req, "STABLE-CONTEXT-MARKER") {
		t.Fatalf("stable context missing from system prompt: %v", req)
	}
	if systemMsgContains(req, "VOLATILE-CONTEXT-MARKER") {
		t.Fatalf("volatile context leaked into system prompt: %v", req)
	}
	if !trailingUserMsgContains(req, "VOLATILE-CONTEXT-MARKER") {
		t.Fatalf("volatile context missing from trailing user message: %v", req)
	}
	if len(req) < 3 || req[len(req)-2]["content"] != "go" {
		t.Fatalf("volatile context was not appended after the user transcript message: %v", req)
	}
}

// TestVolatileChecklistRefreshedWithinTurn is the regression test for the core
// acceptance criterion of issue #263/#404: the checklist is re-evaluated every
// turn and survives compaction, while staying outside the cacheable prefix.
//
// The reported failure mode is intra-turn: an agent lays out its checklist via
// the todo tool partway through one long autonomous run (one runLoop, many model
// round-trips), then a compaction fires and the originating todo tool calls are
// summarized out of the transcript. For the model to still see the checklist,
// the separately-owned per-request context — a place compaction does not touch —
// must reflect the checklist on EVERY round-trip, not just a snapshot taken
// before the first one.
//
// This test simulates the model creating the checklist during the first tool
// round (the handler sets the session's todos right after serving request 0),
// then asserts the checklist appears in the trailing volatile message of the
// next request in the SAME turn. If the provider is only evaluated once at
// runLoop start, the second request is stale and the checklist is absent.
func TestVolatileChecklistRefreshedWithinTurnIssue404(t *testing.T) {
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
	us.SetSystemContextProvider(func(string) (string, string) { return "", RenderTodos(us.Todos()) })

	if _, err := us.ExecuteTaskLoop(context.Background(), "root", "go"); err != nil {
		t.Fatalf("loop error: %v", err)
	}
	if len(fs.requests) < 2 {
		t.Fatalf("expected at least 2 model requests, got %d", len(fs.requests))
	}

	// The mid-turn checklist must be visible in the trailing volatile message of
	// the request that follows its creation (and of every later request in the
	// turn), but must not leak into the cacheable system prompt.
	if systemMsgContains(fs.requests[1], "MIDTURN-CHECKLIST-MARKER") {
		t.Errorf("checklist created mid-turn leaked into the system prompt;\nreq1 = %v", fs.requests[1])
	}
	if !trailingUserMsgContains(fs.requests[1], "MIDTURN-CHECKLIST-MARKER") {
		t.Errorf("checklist created mid-turn did NOT reach the next request's trailing volatile message "+
			"in the same turn; req1 = %v", fs.requests[1])
	}
}
