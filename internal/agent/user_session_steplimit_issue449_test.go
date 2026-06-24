package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gogent/internal/model"
)

// This file covers issue #449: when runLoop exhausts its per-turn step cap while
// the model's final round-trip still carries unexecuted tool calls, the loop must
// (a) surface a visible STEP_LIMIT_REACHED notice instead of the orphaned content,
// (b) balance the orphaned tool_calls in the persisted transcript so no call id
// dangles unanswered, and (c) leave the session valid for a resumed follow-up turn.
// It also pins stopForStepLimit directly and guards that the notice fires ONLY on a
// genuine cap exit (not on a budget/truncation stop or a real final answer).

// contentAndToolCallResponse builds an OpenAI-style round-trip that carries BOTH a
// visible assistant content string AND a single calc tool call — the shape of the
// orphaned turn in the issue's Session-5 repro (text + unexecuted shell calls).
func contentAndToolCallResponse(id, content string) map[string]interface{} {
	return map[string]interface{}{
		"choices": []map[string]interface{}{{
			"index": 0,
			"message": map[string]interface{}{
				"role":    "assistant",
				"content": content,
				"tool_calls": []map[string]interface{}{{
					"id":   id,
					"type": "function",
					"function": map[string]interface{}{
						"name":      "calc",
						"arguments": `{"expression":"1+1"}`,
					},
				}},
			},
			"finish_reason": "tool_calls",
		}},
		"usage": map[string]interface{}{"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15},
	}
}

// multiToolCallResponse is provided by parallel_tools_test.go (same package); it
// builds a round-trip carrying several native tool_calls, used below to model a
// single orphaned turn with more than one dangling call id (Session-5 carried 2).

// assertTranscriptToolCallsBalanced fails if any assistant tool_call id in the
// persisted transcript has no matching tool result — i.e. a dangling, unanswered
// call that would make the next user turn an invalid (400-prone) request.
func assertTranscriptToolCallsBalanced(t *testing.T, transcript []model.Message) {
	t.Helper()
	issued := make(map[string]int)
	for _, m := range transcript {
		if m.Role != model.RoleAssistant {
			continue
		}
		for _, tc := range m.ToolCalls {
			if tc.ID != "" {
				issued[tc.ID]++
			}
		}
	}
	answered := make(map[string]int)
	for _, m := range transcript {
		if m.Role == model.RoleTool && m.ToolCallID != "" {
			answered[m.ToolCallID]++
		}
	}
	for id, n := range issued {
		if answered[id] < n {
			t.Errorf("tool call %q is dangling: %d issued but only %d result(s) in the transcript", id, n, answered[id])
		}
	}
}

// transcriptHasAssistantToolCall reports whether the transcript holds an assistant
// message that issued the given tool_call id (used to prove the orphaned turn WAS
// persisted, so the balance check below is meaningful rather than vacuous).
func transcriptHasAssistantToolCall(transcript []model.Message, id string) bool {
	for _, m := range transcript {
		if m.Role != model.RoleAssistant {
			continue
		}
		for _, tc := range m.ToolCalls {
			if tc.ID == id {
				return true
			}
		}
	}
	return false
}

// transcriptToolResultContent returns the content of the tool result answering the
// given call id, or "" if none exists.
func transcriptToolResultContent(transcript []model.Message, id string) string {
	for _, m := range transcript {
		if m.Role == model.RoleTool && m.ToolCallID == id {
			return m.Content
		}
	}
	return ""
}

// transcriptAssistantContentForCall returns the (persisted) content of the assistant
// message that issued the given tool_call id — used to check whether a folded notice
// or answer was persisted onto the terminal assistant message (FoldLastAssistantContent).
func transcriptAssistantContentForCall(transcript []model.Message, callID string) (string, bool) {
	for _, m := range transcript {
		if m.Role != model.RoleAssistant {
			continue
		}
		for _, tc := range m.ToolCalls {
			if tc.ID == callID {
				return m.Content, true
			}
		}
	}
	return "", false
}

// nativeStructuredOutputFinalResponse builds a round-trip carrying a single completed
// NATIVE structured_output{final:true} call (finish_reason "tool_calls") — the
// sub-agent/plan-mode final-answer tool. collectToolCalls returns it as an ordinary
// call (explicitFinal=false); only containsTerminalFinal detects it as a final.
func nativeStructuredOutputFinalResponse(id, response string) map[string]interface{} {
	args, _ := json.Marshal(map[string]interface{}{"final": true, "response": response})
	return map[string]interface{}{
		"choices": []map[string]interface{}{{
			"index": 0,
			"message": map[string]interface{}{
				"role": "assistant",
				"tool_calls": []map[string]interface{}{{
					"id":   id,
					"type": "function",
					"function": map[string]interface{}{
						"name":      "structured_output",
						"arguments": string(args),
					},
				}},
			},
			"finish_reason": "tool_calls",
		}},
		"usage": map[string]interface{}{"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15},
	}
}

// embeddedFinalResponse builds a round-trip whose visible content IS a JSON
// {"response":...,"final":true} object with no native tool_calls — the fallback
// final-answer shape some models emit as text. collectToolCalls folds its response
// into resp.Content and reports explicitFinal=true.
func embeddedFinalResponse(response string) map[string]interface{} {
	obj, _ := json.Marshal(map[string]interface{}{"final": true, "response": response})
	return map[string]interface{}{
		"choices": []map[string]interface{}{{
			"index":         0,
			"message":       map[string]interface{}{"role": "assistant", "content": string(obj)},
			"finish_reason": "stop",
		}},
		"usage": map[string]interface{}{"prompt_tokens": 20, "completion_tokens": 8, "total_tokens": 28},
	}
}

// TestStepCapExitSurfacesNoticeBalancesOrphansAndStaysResumableIssue449 is the
// headline regression for #449. Driving the loop one round-trip past a small step
// cap with the final turn carrying a tool call must: surface STEP_LIMIT_REACHED as
// the final event, balance the orphaned call in the persisted transcript (with the
// synthetic final-answer note, NOT a real execution), leave it un-executed, and
// keep the session valid for a resumed follow-up turn.
func TestStepCapExitSurfacesNoticeBalancesOrphansAndStaysResumableIssue449(t *testing.T) {
	const capN = 3
	// Every round-trip is a calc call; the server repeats the last once exhausted,
	// so the orphaned (capN+1)th round-trip carries an unexecuted tool call.
	fs := &fakeServer{responses: makeToolCallResponses(t, capN+10, "FINAL-ANSWER")}
	server := httptest.NewServer(http.HandlerFunc(fs.handler))
	defer server.Close()

	us, _ := newLoopSession(t, server.URL)
	us.SetMaxSteps(capN)

	var events []SessionEvent
	var finalText string
	var sawFinal bool
	us.SetObserver(func(ev SessionEvent) {
		events = append(events, ev)
		if ev.Type == SessionEventFinal {
			sawFinal = true
			finalText = ev.Text
		}
	})

	responses, err := us.ExecuteTaskLoop(context.Background(), "root", "go")
	if err != nil {
		t.Fatalf("loop returned error: %v", err)
	}
	// 1 initial round-trip + capN tool rounds advanced; the (capN+1)th is the
	// orphaned tool-call turn the loop never ran collectToolCalls over.
	if got, want := fs.calls, capN+1; got != want {
		t.Fatalf("model requests = %d, want %d (cap %d + 1)", got, want, capN)
	}

	// (a) The cap exit surfaces the STEP_LIMIT_REACHED notice (naming the cap) as
	// the final event, not the orphaned turn's content.
	if !sawFinal {
		t.Fatal("no SessionEventFinal emitted")
	}
	if !strings.Contains(finalText, stepLimitReachedMarker) {
		t.Errorf("final event = %q, want it to surface %q", finalText, stepLimitReachedMarker)
	}
	if !strings.Contains(finalText, fmt.Sprintf("(%d)", capN)) {
		t.Errorf("final event = %q, want it to name the cap value (%d)", finalText, capN)
	}
	// stopForStepLimit folds the notice onto the shared resp pointer, so the
	// returned slice's terminal response carries it too.
	if last := responses[len(responses)-1].Content; !strings.Contains(last, stepLimitReachedMarker) {
		t.Errorf("terminal response content = %q, want %q", last, stepLimitReachedMarker)
	}

	// (b) The orphaned tool call is balanced in the persisted transcript. The
	// orphaned id is round-trip #(capN+1) == call_<capN>.
	orphanedID := fmt.Sprintf("call_%d", capN)
	transcript := us.RootAgent.ThoughtTrain.GetTranscript()
	if !transcriptHasAssistantToolCall(transcript, orphanedID) {
		t.Fatalf("orphaned call %q was not persisted as an assistant tool_call; balance check would be vacuous", orphanedID)
	}
	assertTranscriptToolCallsBalanced(t, transcript)
	if got := transcriptToolResultContent(transcript, orphanedID); !strings.Contains(got, finalToolCallResultNote) {
		t.Errorf("orphaned call %q transcript result = %q, want the synthetic %q (balanced, not a real calc result)",
			orphanedID, got, finalToolCallResultNote)
	}

	// (c) The orphaned call was balanced, NOT executed: only the capN calls the
	// loop actually ran produced tool events; the orphaned id appears in none.
	if got, want := countEvents(events, SessionEventToolResult), capN; got != want {
		t.Errorf("tool result events = %d, want %d (only executed calls emit results; the orphan must be balanced silently)", got, want)
	}
	for _, ev := range events {
		if (ev.Type == SessionEventToolCall || ev.Type == SessionEventToolResult) && ev.CallID == orphanedID {
			t.Errorf("orphaned call %q was executed (event %+v); it must only be balanced, never run", orphanedID, ev)
		}
	}

	// (d) Resume validity: the first request of a follow-up turn carries the full
	// prior transcript; it must be protocol-valid (no unmatched assistant tool_calls).
	resumeStart := fs.calls
	if _, err := us.ExecuteTaskLoop(context.Background(), "root", "continue"); err != nil {
		t.Fatalf("resumed turn returned error: %v", err)
	}
	if len(fs.requests) <= resumeStart {
		t.Fatal("resumed turn made no model requests")
	}
	assertNoUnmatchedToolCalls(t, fs.requests[resumeStart])
}

// TestStepCapExitPreservesPartialProgressBeneathNoticeIssue449 checks that when
// the orphaned turn carried real partial text alongside its tool calls (the
// Session-5 shape: "I've found a strong candidate…" + 2 calls), stopForStepLimit
// preserves that partial output beneath the STEP_LIMIT_REACHED notice rather than
// discarding it.
func TestStepCapExitPreservesPartialProgressBeneathNoticeIssue449(t *testing.T) {
	const capN = 2
	const partial = "I've found a strong candidate. Let me verify the theory."
	// Turns 1..capN: plain tool calls; turn capN+1 (orphaned): partial text + call.
	responses := make([]map[string]interface{}, 0, capN+1)
	for i := 0; i < capN; i++ {
		responses = append(responses, toolCallResponse(fmt.Sprintf("call_%d", i), "calc", `{"expression":"1+1"}`))
	}
	responses = append(responses, contentAndToolCallResponse("orphan_partial", partial))

	fs := &fakeServer{responses: responses}
	server := httptest.NewServer(http.HandlerFunc(fs.handler))
	defer server.Close()

	us, _ := newLoopSession(t, server.URL)
	us.SetMaxSteps(capN)

	var finalText string
	us.SetObserver(func(ev SessionEvent) {
		if ev.Type == SessionEventFinal {
			finalText = ev.Text
		}
	})

	if _, err := us.ExecuteTaskLoop(context.Background(), "root", "go"); err != nil {
		t.Fatalf("loop returned error: %v", err)
	}
	if got, want := fs.calls, capN+1; got != want {
		t.Fatalf("model requests = %d, want %d", got, want)
	}
	if !strings.Contains(finalText, stepLimitReachedMarker) {
		t.Errorf("final event = %q, want the %q notice", finalText, stepLimitReachedMarker)
	}
	if !strings.Contains(finalText, partial) {
		t.Errorf("final event = %q, want it to preserve the partial progress %q beneath the notice", finalText, partial)
	}
	// The orphaned call is still balanced.
	transcript := us.RootAgent.ThoughtTrain.GetTranscript()
	assertTranscriptToolCallsBalanced(t, transcript)
	if !transcriptHasAssistantToolCall(transcript, "orphan_partial") {
		t.Error("orphaned partial call was not persisted")
	}
}

// TestStepCapExitWithFinalAnswerDoesNotShowStepLimitNoticeIssue449 guards the
// len(calls) > 0 gate: if the loop falls out at the cap but the orphaned round-trip
// is itself a real final answer (no tool calls), the loop must surface that answer
// normally — NOT fabricate a STEP_LIMIT_REACHED notice. The task genuinely finished
// at the cap boundary; there is nothing orphaned.
func TestStepCapExitWithFinalAnswerDoesNotShowStepLimitNoticeIssue449(t *testing.T) {
	const capN = 3
	// Exactly capN tool-call turns, then a final answer: round-trips #1..#capN are
	// processed (tools), round-trip #(capN+1) — the orphaned one — is the final.
	fs := &fakeServer{responses: makeToolCallResponses(t, capN, "REAL-FINAL-ANSWER")}
	server := httptest.NewServer(http.HandlerFunc(fs.handler))
	defer server.Close()

	us, _ := newLoopSession(t, server.URL)
	us.SetMaxSteps(capN)

	var finalText string
	us.SetObserver(func(ev SessionEvent) {
		if ev.Type == SessionEventFinal {
			finalText = ev.Text
		}
	})

	if _, err := us.ExecuteTaskLoop(context.Background(), "root", "go"); err != nil {
		t.Fatalf("loop returned error: %v", err)
	}
	if got, want := fs.calls, capN+1; got != want {
		t.Fatalf("model requests = %d, want %d", got, want)
	}
	if finalText != "REAL-FINAL-ANSWER" {
		t.Errorf("final event = %q, want the real final answer delivered at the cap boundary", finalText)
	}
	if strings.Contains(finalText, stepLimitReachedMarker) {
		t.Errorf("final event = %q; a real final answer must not be overwritten with the step-limit notice", finalText)
	}
}

// TestStepCapExitBalancesMultipleOrphanedToolCallsIssue449 reproduces the exact
// Session-5 failure mode: the orphaned turn carries several tool calls (there, 2
// shell calls). All of them must be balanced — none may dangle.
func TestStepCapExitBalancesMultipleOrphanedToolCallsIssue449(t *testing.T) {
	const capN = 2
	responses := make([]map[string]interface{}, 0, capN+1)
	for i := 0; i < capN; i++ {
		responses = append(responses, toolCallResponse(fmt.Sprintf("call_%d", i), "calc", `{"expression":"1+1"}`))
	}
	// Orphaned turn carries TWO calls, mirroring Session 5's 2 shell calls.
	responses = append(responses, multiToolCallResponse(
		[3]string{"orphan_a", "calc", `{"expression":"1+1"}`},
		[3]string{"orphan_b", "calc", `{"expression":"1+1"}`},
	))

	fs := &fakeServer{responses: responses}
	server := httptest.NewServer(http.HandlerFunc(fs.handler))
	defer server.Close()

	us, _ := newLoopSession(t, server.URL)
	us.SetMaxSteps(capN)

	var finalText string
	us.SetObserver(func(ev SessionEvent) {
		if ev.Type == SessionEventFinal {
			finalText = ev.Text
		}
	})

	if _, err := us.ExecuteTaskLoop(context.Background(), "root", "go"); err != nil {
		t.Fatalf("loop returned error: %v", err)
	}
	if got, want := fs.calls, capN+1; got != want {
		t.Fatalf("model requests = %d, want %d", got, want)
	}
	if !strings.Contains(finalText, stepLimitReachedMarker) {
		t.Errorf("final event = %q, want the %q notice", finalText, stepLimitReachedMarker)
	}
	// Both orphaned calls must be persisted and balanced (the headline Session-5 bug
	// was that they were left dangling with no tool results).
	transcript := us.RootAgent.ThoughtTrain.GetTranscript()
	for _, id := range []string{"orphan_a", "orphan_b"} {
		if !transcriptHasAssistantToolCall(transcript, id) {
			t.Errorf("orphaned call %q was not persisted as an assistant tool_call", id)
		}
		if got := transcriptToolResultContent(transcript, id); !strings.Contains(got, finalToolCallResultNote) {
			t.Errorf("orphaned call %q transcript result = %q, want the synthetic %q", id, got, finalToolCallResultNote)
		}
	}
	assertTranscriptToolCallsBalanced(t, transcript)
}

// TestStepCapExitFiresOnceAcrossTurnsIssue449 confirms the cap exit is per-turn:
// each ExecuteTaskLoop gets a fresh step budget, so a second turn after a capped
// turn runs its own full capN+1 round-trips rather than inheriting a spent counter.
func TestStepCapExitFiresOnceAcrossTurnsIssue449(t *testing.T) {
	const capN = 2
	fs := &fakeServer{responses: makeToolCallResponses(t, 99, "FINAL-ANSWER")}
	server := httptest.NewServer(http.HandlerFunc(fs.handler))
	defer server.Close()

	us, _ := newLoopSession(t, server.URL)
	us.SetMaxSteps(capN)

	if _, err := us.ExecuteTaskLoop(context.Background(), "root", "turn 1"); err != nil {
		t.Fatalf("turn 1 error: %v", err)
	}
	if got, want := fs.calls, capN+1; got != want {
		t.Fatalf("turn 1 requests = %d, want %d", got, want)
	}
	afterTurn1 := fs.calls

	if _, err := us.ExecuteTaskLoop(context.Background(), "root", "turn 2"); err != nil {
		t.Fatalf("turn 2 error: %v", err)
	}
	if got := fs.calls - afterTurn1; got != capN+1 {
		t.Errorf("turn 2 requests = %d, want %d (the step budget must reset per user turn)", got, capN+1)
	}
}

// --- stopForStepLimit unit tests (no loop, no server) -----------------------

func TestStopForStepLimitFoldsNoticeWithCapValueIssue449(t *testing.T) {
	resp := stopForStepLimit(&model.CompletionResponse{Content: "leftover"}, 7)
	if !strings.HasPrefix(resp.Content, stepLimitReachedMarker) {
		t.Errorf("content = %q, want it to start with %q", resp.Content, stepLimitReachedMarker)
	}
	if !strings.Contains(resp.Content, "(7)") {
		t.Errorf("content = %q, want it to name the cap value (7)", resp.Content)
	}
	if !strings.Contains(resp.Content, "interrupted") {
		t.Errorf("content = %q, want it to state the task was interrupted", resp.Content)
	}
	if !strings.Contains(resp.Content, "Type a message to continue") {
		t.Errorf("content = %q, want the resume hint", resp.Content)
	}
}

func TestStopForStepLimitPreservesPartialContentIssue449(t *testing.T) {
	resp := stopForStepLimit(&model.CompletionResponse{Content: "  work in progress  "}, 3)
	if !strings.Contains(resp.Content, stepLimitReachedMarker) {
		t.Errorf("content = %q, want the notice", resp.Content)
	}
	if !strings.Contains(resp.Content, "work in progress") {
		t.Errorf("content = %q, want the partial progress preserved beneath the notice", resp.Content)
	}
}

func TestStopForStepLimitEmptyContentOmitsPartialSectionIssue449(t *testing.T) {
	resp := stopForStepLimit(&model.CompletionResponse{Content: "   "}, 3)
	if !strings.Contains(resp.Content, stepLimitReachedMarker) {
		t.Errorf("content = %q, want the notice", resp.Content)
	}
	if strings.Contains(resp.Content, "Partial progress") {
		t.Errorf("content = %q; an empty partial must not add a Partial progress section", resp.Content)
	}
}

func TestStopForStepLimitNilRespIsSafeIssue449(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("stopForStepLimit(nil, ...) panicked: %v", r)
		}
	}()
	resp := stopForStepLimit(nil, 5)
	if resp == nil {
		t.Fatal("stopForStepLimit(nil, ...) returned nil")
	}
	if !strings.HasPrefix(resp.Content, stepLimitReachedMarker) {
		t.Errorf("content = %q, want the notice on a fresh response", resp.Content)
	}
}

// --- guards that the notice fires ONLY on a genuine cap exit ----------------

// TestBudgetStopDoesNotShowStepLimitNoticeIssue449 guards the stoppedInLoop flag on
// the budget path: a budget stop that lands on a tool-call turn must surface
// BUDGET_EXCEEDED, never STEP_LIMIT_REACHED. If the budget break forgot to set
// stoppedInLoop, the cap-exit branch would wrongly fire after it.
func TestBudgetStopDoesNotShowStepLimitNoticeIssue449(t *testing.T) {
	fs := &fakeServer{responses: makeToolCallResponses(t, 50, "FINAL-ANSWER")}
	server := httptest.NewServer(http.HandlerFunc(fs.handler))
	defer server.Close()

	us, ag := newLoopSession(t, server.URL)
	us.SetMaxSteps(100) // generous: the budget, not the cap, must stop this loop
	ag.SetTokenBudget(40)

	var finalText string
	us.SetObserver(func(ev SessionEvent) {
		if ev.Type == SessionEventFinal {
			finalText = ev.Text
		}
	})

	if _, err := us.ExecuteTaskLoop(context.Background(), "root", "go"); err != nil {
		t.Fatalf("loop returned error: %v", err)
	}
	if !strings.Contains(finalText, budgetExceededMarker) {
		t.Errorf("final event = %q, want %q (the budget stop)", finalText, budgetExceededMarker)
	}
	if strings.Contains(finalText, stepLimitReachedMarker) {
		t.Errorf("final event = %q; a budget stop must not surface the step-limit notice (stoppedInLoop must suppress the cap-exit branch)", finalText)
	}
}

// --- sub-agent coverage (runLoop is shared) ---------------------------------

// TestSubAgentStepCapExitSurfacesNoticeIssue449 confirms the shared runLoop surfaces
// the step-cap notice for a one-shot sub-agent too: a sub-agent that exhausts its
// step cap returns the STEP_LIMIT_REACHED notice as its final result (rather than a
// silent, incomplete answer), and — per the #449 sub-agent-boundary fix — is reported
// StatusFailed (it did not finish), not Completed.
func TestSubAgentStepCapExitSurfacesNoticeIssue449(t *testing.T) {
	const capN = 3
	fs := &fakeServer{responses: makeToolCallResponses(t, capN+10, "SUCCESS: done")}
	server := httptest.NewServer(http.HandlerFunc(fs.handler))
	defer server.Close()

	us, _ := newLoopSession(t, server.URL)
	us.SetMaxSteps(capN)

	var subAgentEvents []SessionEvent
	us.SetObserver(func(ev SessionEvent) {
		if ev.Type == SessionEventSubAgent {
			subAgentEvents = append(subAgentEvents, ev)
		}
	})

	final, err := us.SpawnSubAgent(context.Background(), "root", "child", "do it", true)
	if err != nil {
		t.Fatalf("SpawnSubAgent error: %v", err)
	}
	if got, want := fs.calls, capN+1; got != want {
		t.Errorf("sub-agent model requests = %d, want %d (shared cap)", got, want)
	}
	if !strings.Contains(final, stepLimitReachedMarker) {
		t.Errorf("sub-agent final = %q, want it to surface %q (the shared loop folds the cap notice)", final, stepLimitReachedMarker)
	}
	// The terminal SessionEventSubAgent carries the child's classified status; a
	// step-capped sub-agent did not finish, so it must be StatusFailed.
	if len(subAgentEvents) == 0 {
		t.Fatal("no SessionEventSubAgent events captured")
	}
	terminal := subAgentEvents[len(subAgentEvents)-1]
	if terminal.Status != StatusFailed {
		t.Errorf("step-capped sub-agent terminal status = %v, want StatusFailed (the task was interrupted, not completed)", terminal.Status)
	}
}

// TestSubAgentOutcomeClassifiesBudgetAndStepLimitAsFailedIssue449 pins the fix for
// the sub-agent-boundary counterpart of #449: a sub-agent whose result carries the
// BUDGET_EXCEEDED OR STEP_LIMIT_REACHED marker did NOT finish its task, so
// subAgentOutcome must classify both as StatusFailed — otherwise a coordinator would
// receive the cap/budget stop notice as if it were a successful answer. (Previously
// only BUDGET_EXCEEDED was failed; STEP_LIMIT_REACHED fell through to Completed.)
func TestSubAgentOutcomeClassifiesBudgetAndStepLimitAsFailedIssue449(t *testing.T) {
	cases := []struct {
		name  string
		final string
	}{
		{"budget exceeded", budgetExceededMarker + ": token budget reached (40/40)"},
		{"step limit reached", fmt.Sprintf("%s: reached the per-turn step cap (3)", stepLimitReachedMarker)},
		{"step limit reached with partial beneath", stepLimitReachedMarker + ": reached the per-turn step cap (100); the task was interrupted.\n\nPartial progress before stopping:\nfoo"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := subAgentOutcome(tc.final); got != StatusFailed {
				t.Errorf("subAgentOutcome(%q) = %v, want StatusFailed (an interrupted sub-agent must not be reported completed)", tc.final, got)
			}
		})
	}
	// A genuine SUCCESS final is still completed (the new markers must not over-fire).
	if got := subAgentOutcome("SUCCESS: the task is done"); got != StatusCompleted {
		t.Errorf("subAgentOutcome(SUCCESS) = %v, want StatusCompleted", got)
	}
}

// --- native/embedded finals landing on the cap-orphaned turn (#449 round 2) --
//
// The first review round found that a deliberate final answer emitted on the turn
// the cap orphans was overwritten by STEP_LIMIT_REACHED. The fix added a 3-way
// switch in the cap-exit branch: explicitFinal (embedded/salvaged) and
// containsTerminalFinal (native structured_output{final}) deliver the answer; only a
// genuine orphan surfaces the notice. These pin each branch.

// TestStepCapExitDeliversNativeStructuredOutputFinalIssue449: the orphaned turn is a
// completed NATIVE structured_output{final:true}. The model finished at the cap
// boundary, so its answer must be delivered — NOT clobbered by STEP_LIMIT_REACHED —
// and the never-executed final call balanced.
func TestStepCapExitDeliversNativeStructuredOutputFinalIssue449(t *testing.T) {
	const capN = 3
	const answer = "THE-REAL-FINAL-ANSWER"
	responses := make([]map[string]interface{}, 0, capN+1)
	for i := 0; i < capN; i++ {
		responses = append(responses, toolCallResponse(fmt.Sprintf("call_%d", i), "calc", `{"expression":"1+1"}`))
	}
	responses = append(responses, nativeStructuredOutputFinalResponse("final_call", answer))

	fs := &fakeServer{responses: responses}
	server := httptest.NewServer(http.HandlerFunc(fs.handler))
	defer server.Close()

	us, _ := newLoopSession(t, server.URL)
	us.SetMaxSteps(capN)

	var finalText string
	us.SetObserver(func(ev SessionEvent) {
		if ev.Type == SessionEventFinal {
			finalText = ev.Text
		}
	})

	if _, err := us.ExecuteTaskLoop(context.Background(), "root", "go"); err != nil {
		t.Fatalf("loop returned error: %v", err)
	}
	if got, want := fs.calls, capN+1; got != want {
		t.Fatalf("model requests = %d, want %d", got, want)
	}
	if finalText != answer {
		t.Errorf("final event = %q, want the native structured_output final %q delivered", finalText, answer)
	}
	if strings.Contains(finalText, stepLimitReachedMarker) {
		t.Errorf("final event = %q; a completed final answer must not be reported as a step-limit interruption", finalText)
	}
	transcript := us.RootAgent.ThoughtTrain.GetTranscript()
	assertTranscriptToolCallsBalanced(t, transcript)
	if !transcriptHasAssistantToolCall(transcript, "final_call") {
		t.Error("the native structured_output final call was not persisted")
	}
}

// TestStepCapExitDeliversEmbeddedStructuredOutputFinalIssue449: the orphaned turn is
// a JSON-text {"response":...,"final":true} (no native tool_calls). collectToolCalls
// folds it and reports explicitFinal, so the answer is delivered, not the notice.
func TestStepCapExitDeliversEmbeddedStructuredOutputFinalIssue449(t *testing.T) {
	const capN = 3
	const answer = "EMBEDDED-ANSWER"
	responses := make([]map[string]interface{}, 0, capN+1)
	for i := 0; i < capN; i++ {
		responses = append(responses, toolCallResponse(fmt.Sprintf("call_%d", i), "calc", `{"expression":"1+1"}`))
	}
	responses = append(responses, embeddedFinalResponse(answer))

	fs := &fakeServer{responses: responses}
	server := httptest.NewServer(http.HandlerFunc(fs.handler))
	defer server.Close()

	us, _ := newLoopSession(t, server.URL)
	us.SetMaxSteps(capN)

	var finalText string
	us.SetObserver(func(ev SessionEvent) {
		if ev.Type == SessionEventFinal {
			finalText = ev.Text
		}
	})

	if _, err := us.ExecuteTaskLoop(context.Background(), "root", "go"); err != nil {
		t.Fatalf("loop returned error: %v", err)
	}
	if got, want := fs.calls, capN+1; got != want {
		t.Fatalf("model requests = %d, want %d", got, want)
	}
	if finalText != answer {
		t.Errorf("final event = %q, want the embedded final %q delivered", finalText, answer)
	}
	if strings.Contains(finalText, stepLimitReachedMarker) {
		t.Errorf("final event = %q; a completed embedded final must not be reported as a step-limit interruption", finalText)
	}
}

// TestStepCapExitMixedBatchWithTerminalFinalIssue449: the orphaned turn carries a
// real tool call AND a native structured_output{final:true}. The final answer is
// delivered; BOTH calls are balanced (neither can execute on an orphaned turn), and
// the batch is NOT reported as a step-limit interruption.
func TestStepCapExitMixedBatchWithTerminalFinalIssue449(t *testing.T) {
	const capN = 2
	const answer = "MIXED-ANSWER"
	responses := make([]map[string]interface{}, 0, capN+1)
	for i := 0; i < capN; i++ {
		responses = append(responses, toolCallResponse(fmt.Sprintf("call_%d", i), "calc", `{"expression":"1+1"}`))
	}
	responses = append(responses, multiToolCallResponse(
		[3]string{"mixed_calc", "calc", `{"expression":"1+1"}`},
		[3]string{"mixed_final", "structured_output", fmt.Sprintf(`{"final":true,"response":%q}`, answer)},
	))

	fs := &fakeServer{responses: responses}
	server := httptest.NewServer(http.HandlerFunc(fs.handler))
	defer server.Close()

	us, _ := newLoopSession(t, server.URL)
	us.SetMaxSteps(capN)

	var finalText string
	var events []SessionEvent
	us.SetObserver(func(ev SessionEvent) {
		events = append(events, ev)
		if ev.Type == SessionEventFinal {
			finalText = ev.Text
		}
	})

	if _, err := us.ExecuteTaskLoop(context.Background(), "root", "go"); err != nil {
		t.Fatalf("loop returned error: %v", err)
	}
	if got, want := fs.calls, capN+1; got != want {
		t.Fatalf("model requests = %d, want %d", got, want)
	}
	if finalText != answer {
		t.Errorf("final event = %q, want the mixed-batch final %q delivered", finalText, answer)
	}
	if strings.Contains(finalText, stepLimitReachedMarker) {
		t.Errorf("final event = %q; a batch containing a terminal final must not be reported as a step-limit interruption", finalText)
	}
	// Both orphaned calls are balanced; neither was executed (only the capN earlier
	// calc turns ran, so exactly capN tool-result events).
	transcript := us.RootAgent.ThoughtTrain.GetTranscript()
	for _, id := range []string{"mixed_calc", "mixed_final"} {
		if !transcriptHasAssistantToolCall(transcript, id) {
			t.Errorf("orphaned call %q was not persisted", id)
		}
	}
	assertTranscriptToolCallsBalanced(t, transcript)
	if got, want := countEvents(events, SessionEventToolResult), capN; got != want {
		t.Errorf("tool result events = %d, want %d (the orphaned batch must be balanced, not executed)", got, want)
	}
}

// TestStepCapExitPersistsNoticeOnTerminalAssistantMessageIssue449 verifies the notice
// is folded onto the PERSISTED terminal assistant message (FoldLastAssistantContent),
// so reopening the session shows the step-cap explanation rather than the orphaned
// turn's empty/partial content. This was added in the round-2 fix (mirroring the
// truncation stop).
func TestStepCapExitPersistsNoticeOnTerminalAssistantMessageIssue449(t *testing.T) {
	const capN = 3
	fs := &fakeServer{responses: makeToolCallResponses(t, capN+10, "FINAL-ANSWER")}
	server := httptest.NewServer(http.HandlerFunc(fs.handler))
	defer server.Close()

	us, _ := newLoopSession(t, server.URL)
	us.SetMaxSteps(capN)

	if _, err := us.ExecuteTaskLoop(context.Background(), "root", "go"); err != nil {
		t.Fatalf("loop returned error: %v", err)
	}
	transcript := us.RootAgent.ThoughtTrain.GetTranscript()
	orphanedID := fmt.Sprintf("call_%d", capN)
	content, ok := transcriptAssistantContentForCall(transcript, orphanedID)
	if !ok {
		t.Fatalf("orphaned call %q was not persisted as an assistant tool_call", orphanedID)
	}
	if !strings.Contains(content, stepLimitReachedMarker) {
		t.Errorf("terminal assistant message content = %q, want the %q notice persisted via FoldLastAssistantContent",
			content, stepLimitReachedMarker)
	}
}

// TestSubAgentStepCapNativeFinalDeliveredIssue449: a one-shot sub-agent whose
// cap-orphaned turn is a native structured_output{final:true} returns its real answer
// (not the step-cap notice) and is reported StatusCompleted — the model finished at
// the cap boundary. This is the primary structured_output path (sub-agent finalize).
func TestSubAgentStepCapNativeFinalDeliveredIssue449(t *testing.T) {
	const capN = 3
	const answer = "SUB-FINAL-ANSWER"
	responses := make([]map[string]interface{}, 0, capN+1)
	for i := 0; i < capN; i++ {
		responses = append(responses, toolCallResponse(fmt.Sprintf("call_%d", i), "calc", `{"expression":"1+1"}`))
	}
	responses = append(responses, nativeStructuredOutputFinalResponse("sub_final", answer))

	fs := &fakeServer{responses: responses}
	server := httptest.NewServer(http.HandlerFunc(fs.handler))
	defer server.Close()

	us, _ := newLoopSession(t, server.URL)
	us.SetMaxSteps(capN)

	var subAgentEvents []SessionEvent
	us.SetObserver(func(ev SessionEvent) {
		if ev.Type == SessionEventSubAgent {
			subAgentEvents = append(subAgentEvents, ev)
		}
	})

	final, err := us.SpawnSubAgent(context.Background(), "root", "child", "do it", true)
	if err != nil {
		t.Fatalf("SpawnSubAgent error: %v", err)
	}
	if final != answer {
		t.Errorf("sub-agent final = %q, want the native final %q delivered (not the step-cap notice)", final, answer)
	}
	if strings.Contains(final, stepLimitReachedMarker) {
		t.Errorf("sub-agent final = %q; a completed final must not be reported as a step-limit interruption", final)
	}
	// The model finished, so the sub-agent is completed (not failed).
	if len(subAgentEvents) == 0 {
		t.Fatal("no SessionEventSubAgent events captured")
	}
	terminal := subAgentEvents[len(subAgentEvents)-1]
	if terminal.Status != StatusCompleted {
		t.Errorf("sub-agent terminal status = %v, want StatusCompleted (the model emitted a final at the cap boundary)", terminal.Status)
	}
}
