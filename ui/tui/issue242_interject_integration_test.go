package ui

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gogent/internal/agent"
	"gogent/internal/model"
	"gogent/internal/tool"
)

// Round-2 integration tests for issue #242. Round 1 locked the two halves
// separately (backend: no SessionEventAssistantStep carries the note; UI:
// interject → kindUser "You (clarification):" record). These tests exercise the
// full loop end-to-end — a real backend UserSession whose observer feeds live
// events into SessionWindow.apply() — to prove the user-visible scenario from
// the issue in a single flow: the interjection renders exactly once as the
// user's message and never as a model "thought", while still being delivered
// mid-turn to the model.
//
// They also characterize a pre-existing, out-of-#242-scope quirk (the backend's
// latest-wins pendingNote slot) that the new user-message rendering makes more
// surprising: N interjections before a turn boundary render as N "You
// (clarification):" records but only the last reaches the model.

// fakeAgentServer is a minimal OpenAI-style test server: it serves canned
// responses in sequence and records each request's "messages" array so a test
// can assert what the model received. It mirrors internal/agent's fakeServer so
// the real model layer parses the responses correctly, but lives here so the UI
// test can drive a real *agent.UserSession without an import reversal.
type fakeAgentServer struct {
	responses []map[string]interface{}
	requests  [][]map[string]interface{}
	calls     int
}

func (f *fakeAgentServer) handler(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	var req struct {
		Messages []map[string]interface{} `json:"messages"`
	}
	_ = json.Unmarshal(body, &req)
	f.requests = append(f.requests, req.Messages)

	idx := f.calls
	if idx >= len(f.responses) {
		idx = len(f.responses) - 1
	}
	f.calls++
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(f.responses[idx])
}

func agentToolCallResponse(id, name, args string) map[string]interface{} {
	return map[string]interface{}{
		"choices": []map[string]interface{}{{
			"index": 0,
			"message": map[string]interface{}{
				"role": "assistant",
				"tool_calls": []map[string]interface{}{{
					"id":   id,
					"type": "function",
					"function": map[string]interface{}{
						"name":      name,
						"arguments": args,
					},
				}},
			},
			"finish_reason": "tool_calls",
		}},
		"usage": map[string]interface{}{"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15},
	}
}

func agentFinalResponse(content string) map[string]interface{} {
	return map[string]interface{}{
		"choices": []map[string]interface{}{{
			"index":         0,
			"message":       map[string]interface{}{"role": "assistant", "content": content},
			"finish_reason": "stop",
		}},
		"usage": map[string]interface{}{"prompt_tokens": 20, "completion_tokens": 8, "total_tokens": 28},
	}
}

// newInjectableSession builds a real root UserSession pointed at url with the
// calc tool registered, mirroring internal/agent.newLoopSession using only
// exported API so the UI test can drive a genuine loop.
func newInjectableSession(t *testing.T, url string) *agent.UserSession {
	t.Helper()
	conn := model.NewModelConnection()
	conn.SetURL(url)
	sess := model.NewModelSession("test", conn)
	reg := tool.NewToolRegistry()
	reg.RegisterCalcTool()
	ag := agent.NewAgent("root", sess)
	ag.SetToolRegistry(reg)
	return agent.NewUserSession("s1", ag)
}

// requestHasUserContent reports whether any user-role message in a captured
// request contains sub.
func requestHasUserContent(msgs []map[string]interface{}, sub string) bool {
	for _, m := range msgs {
		if role, _ := m["role"].(string); role != "user" {
			continue
		}
		if content, _ := m["content"].(string); strings.Contains(content, sub) {
			return true
		}
	}
	return false
}

// TestInterjectEndToEndRendersOnceAndDelivers is the gold-standard #242 test:
// it drives a real backend turn (tool call → final), wires the Interject path
// to InjectUserNote, and feeds every backend event into the live
// SessionWindow. It then asserts the three contract points together —
//  1. the interjection renders exactly once, as a kindUser "You
//     (clarification):" record (never a [System] note, never a thought);
//  2. no model "thought" record carries the user's note (display point 2 —
//     the removed SessionEventAssistantStep emit);
//  3. the model still received the framed clarification mid-turn (delivery
//     preserved).
func TestInterjectEndToEndRendersOnceAndDelivers(t *testing.T) {
	const note = "please switch to base 16"
	fs := &fakeAgentServer{responses: []map[string]interface{}{
		agentToolCallResponse("call_1", "calc", `{"expression":"2+2"}`),
		agentFinalResponse("The answer is 4."),
	}}
	server := httptest.NewServer(http.HandlerFunc(fs.handler))
	defer server.Close()

	us := newInjectableSession(t, server.URL)
	us.SetInjectQueuedInput(true)

	w := newTestWorkbench(t)
	sw := w.openWindow("s", "S")
	sw.busy = true

	// Wire the real backend into the Interject path. interject() dispatches
	// OnInject on a goroutine, so the handler signals once InjectUserNote has
	// landed — this establishes happens-before with the loop's takePendingNote.
	injected := make(chan string, 4)
	w.handlers.OnInject = func(_, message string) {
		us.InjectUserNote(message)
		injected <- message
	}
	// The backend's live event stream drives the UI transcript, exactly as in
	// the real app.
	us.SetObserver(func(ev agent.SessionEvent) { sw.apply(ev) })

	// Interject mid-turn: adds the clarification record and primes pendingNote.
	sw.input.SetText(note)
	sw.interject()
	<-injected // wait for the note to reach the backend slot

	if _, err := us.ExecuteTaskLoop(context.Background(), "root", "what is 2+2?"); err != nil {
		t.Fatalf("loop returned error: %v", err)
	}

	// (1) Exactly one clarification record, and it is a USER record.
	clarifs := clarificationRecords(sw)
	if len(clarifs) != 1 {
		t.Fatalf("expected exactly 1 clarification record end-to-end, got %d", len(clarifs))
	}
	if clarifs[0].kind != kindUser {
		t.Errorf("clarification kind = %v, want kindUser", clarifs[0].kind)
	}
	if clarifs[0].body() != note {
		t.Errorf("clarification body = %q, want %q", clarifs[0].body(), note)
	}

	// (2) No model "thought" record anywhere carries the user's note. Before
	// #242 the turn-boundary splice emitted it as a SessionEventAssistantStep,
	// which apply() rendered as a thought — that duplicate must be gone.
	for _, r := range sw.transcript.records {
		if r.kind != kindThinking {
			continue
		}
		if strings.Contains(r.header, note) {
			t.Errorf("a thought header carries the note: %q", r.header)
		}
		for _, ln := range r.lines {
			if strings.Contains(ln.text, note) {
				t.Errorf("a thought line carries the note: %q", ln.text)
			}
		}
	}

	// (3) Delivery preserved: the model's second request carries the framed
	// clarification (the first request is the initial prompt + tool call).
	if len(fs.requests) != 2 {
		t.Fatalf("expected 2 model requests, got %d", len(fs.requests))
	}
	if !requestHasUserContent(fs.requests[1], note) {
		t.Errorf("second request did not carry the note %q; messages: %v", note, fs.requests[1])
	}
}

// TestInterjectMultipleRendersAllButBackendDeliversOnlyLast is a
// characterization test for a PRE-EXISTING quirk that is OUT OF SCOPE for #242
// (which is render-only) but which the new "You (clarification):" rendering
// makes more surprising: the backend's pendingNote is a latest-wins slot
// (user_session.go: "a newer note replaces an undrained one"), so N interjections
// before a single turn boundary render as N distinct user messages but only the
// LAST is delivered to the model. This test pins the current behaviour so any
// future change to the slot semantics is a deliberate, visible decision — and
// flags the display/delivery asymmetry as a candidate follow-up.
func TestInterjectMultipleRendersAllButBackendDeliversOnlyLast(t *testing.T) {
	first, second := "first clarification", "second clarification"
	fs := &fakeAgentServer{responses: []map[string]interface{}{
		agentToolCallResponse("call_1", "calc", `{"expression":"1+1"}`),
		agentFinalResponse("Done."),
	}}
	server := httptest.NewServer(http.HandlerFunc(fs.handler))
	defer server.Close()

	us := newInjectableSession(t, server.URL)
	us.SetInjectQueuedInput(true)

	w := newTestWorkbench(t)
	sw := w.openWindow("s", "S")
	sw.busy = true

	// OnInject dispatches on a goroutine, so the handler signals once each
	// InjectUserNote has landed — waiting for both keeps the latest-wins
	// ordering deterministic relative to the loop's takePendingNote.
	injectedDone := make(chan string, 4)
	w.handlers.OnInject = func(_, message string) {
		us.InjectUserNote(message)
		injectedDone <- message
	}
	us.SetObserver(func(ev agent.SessionEvent) { sw.apply(ev) })

	// Two interjections before the turn boundary drains the slot. We wait for
	// each OnInject to land before issuing the next so the latest-wins ordering
	// is deterministic — note that in real rapid usage the two goroutine-
	// dispatched OnInject calls RACE on the slot mutex, so which one wins is
	// itself non-deterministic; that deeper quirk is out of #242 scope.
	sw.input.SetText(first)
	sw.interject()
	<-injectedDone // first note landed → slot = first
	sw.input.SetText(second)
	sw.interject()
	<-injectedDone // second note overwrote it → slot = second

	if _, err := us.ExecuteTaskLoop(context.Background(), "root", "go"); err != nil {
		t.Fatalf("loop returned error: %v", err)
	}

	// Display side: BOTH interjections render as user messages.
	clarifs := clarificationRecords(sw)
	if len(clarifs) != 2 {
		t.Fatalf("expected 2 clarification records, got %d", len(clarifs))
	}
	if clarifs[0].body() != first || clarifs[1].body() != second {
		t.Errorf("clarification bodies = %q, %q; want %q, %q",
			clarifs[0].body(), clarifs[1].body(), first, second)
	}

	// Delivery side (the asymmetry): only the LAST note reaches the model.
	if len(fs.requests) < 2 {
		t.Fatalf("expected >=2 model requests, got %d", len(fs.requests))
	}
	if !requestHasUserContent(fs.requests[1], second) {
		t.Errorf("last interjection %q was not delivered; messages: %v", second, fs.requests[1])
	}
	if requestHasUserContent(fs.requests[1], first) {
		t.Errorf("first interjection %q was delivered too — slot is no longer latest-wins "+
			"(if this is an intentional fix to the display/delivery asymmetry, update this test)", first)
	}
}
