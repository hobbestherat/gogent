package agent

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// This file tests the backend half of issue #242: at the turn boundary a queued
// user note (an Interject / mid-turn clarification) is still spliced into the
// next model request as a RoleUser message — the mid-turn DELIVERY is preserved —
// but it is no longer re-emitted as a SessionEventAssistantStep, which the UI
// rendered as a model "thought" and so duplicated the user's clarification.
//
// The fix removed only the emit line in runLoop; nextMessages (the RoleUser
// delivery to the model) is unchanged. These tests lock down both sides.

// captureStepEvents installs an observer that records every
// SessionEventAssistantStep's text. The observer may fire from the loop's own
// goroutine and from streaming sinks, so the slice is mutex-guarded.
func captureStepEvents(us *UserSession) func() []string {
	var (
		mu  sync.Mutex
		buf []string
	)
	us.SetObserver(func(ev SessionEvent) {
		if ev.Type != SessionEventAssistantStep {
			return
		}
		mu.Lock()
		buf = append(buf, ev.Text)
		mu.Unlock()
	})
	return func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), buf...)
	}
}

// requestCarriesUserContent reports whether any user-role message in the given
// captured request contains sub.
func requestCarriesUserContent(msgs []map[string]interface{}, sub string) bool {
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

// TestInjectUserNoteNotReEmittedAsAssistantStep is the core backend #242
// assertion: with mid-turn injection on, the queued note reaches the model as a
// framed RoleUser clarification in the second request, but produces NO
// SessionEventAssistantStep carrying the note (which previously duplicated it as
// a "thought").
func TestInjectUserNoteNotReEmittedAsAssistantStep(t *testing.T) {
	const note = "actually use base 16"
	fs := &fakeServer{responses: []map[string]interface{}{
		toolCallResponse("call_1", "calc", `{"expression":"2+2"}`),
		finalResponse("The answer is 4."),
	}}
	server := httptest.NewServer(http.HandlerFunc(fs.handler))
	defer server.Close()

	us, _ := newLoopSession(t, server.URL)
	us.SetInjectQueuedInput(true)
	stepTexts := captureStepEvents(us)

	// Queue the note before the loop; it is drained at the first turn boundary
	// (after the calc tool round), so it must land in the second model request.
	us.InjectUserNote(note)

	if _, err := us.ExecuteTaskLoop(context.Background(), "root", "what is 2+2?"); err != nil {
		t.Fatalf("loop returned error: %v", err)
	}

	// --- Delivery preserved: the model still receives the framed clarification. ---
	if len(fs.requests) != 2 {
		t.Fatalf("expected 2 model requests, got %d", len(fs.requests))
	}
	wantFraming := fmt.Sprintf(injectedNoteTemplate, note)
	if !requestCarriesUserContent(fs.requests[1], wantFraming) {
		t.Errorf("second request did not carry the injected clarification %q; messages: %v",
			wantFraming, fs.requests[1])
	}

	// --- Rendering fix: no SessionEventAssistantStep echoes the note. ---
	// Before #242 the splice emitted the framed note as an assistant step; that
	// is the exact emit the fix removed. Neither the user's raw text nor the
	// "[The user added a clarification:" framing may appear in any step event.
	for _, txt := range stepTexts() {
		if strings.Contains(txt, note) {
			t.Errorf("injected note %q re-emitted as a SessionEventAssistantStep thought: %q", note, txt)
		}
		if strings.Contains(txt, "added a clarification") {
			t.Errorf("injected framing re-emitted as a SessionEventAssistantStep thought: %q", txt)
		}
	}

	// The slot is single-use: nothing left to re-inject or re-emit afterwards.
	if leftover := us.takePendingNote(); leftover != "" {
		t.Errorf("pending note should be cleared after injection, got %q", leftover)
	}
}

// TestInjectUserNoteDisabledIsNeitherSplicedNorStepped confirms the experimental
// flag still gates the splice: with injection OFF (the default), the queued note
// is not handed to the model mid-turn AND emits no assistant step. (The UI's
// drain-on-idle path owns display instead.)
func TestInjectUserNoteDisabledIsNeitherSplicedNorStepped(t *testing.T) {
	const note = "should not be injected"
	fs := &fakeServer{responses: []map[string]interface{}{
		toolCallResponse("call_1", "calc", `{"expression":"2+2"}`),
		finalResponse("The answer is 4."),
	}}
	server := httptest.NewServer(http.HandlerFunc(fs.handler))
	defer server.Close()

	us, _ := newLoopSession(t, server.URL)
	// InjectQueuedInput left at its default (off).
	stepTexts := captureStepEvents(us)
	us.InjectUserNote(note)

	if _, err := us.ExecuteTaskLoop(context.Background(), "root", "what is 2+2?"); err != nil {
		t.Fatalf("loop returned error: %v", err)
	}

	// Not spliced into either model request.
	wantFraming := fmt.Sprintf(injectedNoteTemplate, note)
	for i, req := range fs.requests {
		if requestCarriesUserContent(req, wantFraming) {
			t.Errorf("disabled injection still spliced the note into request %d", i)
		}
	}
	// And no assistant step carries it.
	for _, txt := range stepTexts() {
		if strings.Contains(txt, note) || strings.Contains(txt, "added a clarification") {
			t.Errorf("disabled injection emitted a SessionEventAssistantStep for the note: %q", txt)
		}
	}
	// The note is still queued (never drained) for the drain-on-idle path.
	if got := us.takePendingNote(); got != note {
		t.Errorf("disabled injection should leave the note queued, takePendingNote = %q, want %q", got, note)
	}
}

// TestInjectUserNoteStepEventAbsentEvenWhenModelReasons guards against a
// regression where a real model "thought" (a genuine SessionEventAssistantStep
// from the model's own reasoning) could be confused with the removed note emit.
// The model emits reasoning that mentions "clarification" generically; the
// assert remains that only the model's OWN text is ever carried, never the
// user's verbatim note. (The fake server below has no reasoning field, so the
// only way the note could appear as a step is the removed emit — making this a
// strong negative test.)
func TestInjectUserNoteStepEventAbsentEvenWhenModelReasons(t *testing.T) {
	const note = "my uniquely identifying clarification xyzzy"
	fs := &fakeServer{responses: []map[string]interface{}{
		toolCallResponse("call_1", "calc", `{"expression":"1+1"}`),
		finalResponse("Done."),
	}}
	server := httptest.NewServer(http.HandlerFunc(fs.handler))
	defer server.Close()

	us, _ := newLoopSession(t, server.URL)
	us.SetInjectQueuedInput(true)
	stepTexts := captureStepEvents(us)
	us.InjectUserNote(note)

	if _, err := us.ExecuteTaskLoop(context.Background(), "root", "go"); err != nil {
		t.Fatalf("loop returned error: %v", err)
	}

	// Delivery happened…
	if len(fs.requests) < 2 {
		t.Fatalf("expected >=2 model requests, got %d", len(fs.requests))
	}
	wantFraming := fmt.Sprintf(injectedNoteTemplate, note)
	if !requestCarriesUserContent(fs.requests[1], wantFraming) {
		t.Errorf("expected the note delivered in the second request; messages: %v", fs.requests[1])
	}
	// …yet no step event leaked the user's note text.
	for _, txt := range stepTexts() {
		if strings.Contains(txt, note) {
			t.Errorf("user note text leaked into a SessionEventAssistantStep: %q", txt)
		}
	}
}
