package ui

import (
	"strings"
	"testing"

	"gogent/internal/agent"
)

// This file tests issue #242: an interjected message (the Interject button /
// mid-turn delivery from #201) must render as the user's OWN message — a
// kindUser "You (clarification):" record with the user colour/role — instead of
// as a [System] note (kindSystem) or a collapsed model "thought" (kindThinking).
//
// The backend still delivers the framed RoleUser clarification to the model
// (that path is covered by internal/agent); these tests lock down the
// transcript RENDERING / record-type only.

// clarificationHeader is the user-visible header an interjected message is
// rendered under (issue #242). It mirrors the literal in addClarification.
const clarificationHeader = "You (clarification):"

// recordsOfKind returns every transcript record of the given kind, in order.
func recordsOfKind(sw *SessionWindow, k eventKind) []*transcriptRecord {
	var out []*transcriptRecord
	for _, r := range sw.transcript.records {
		if r.kind == k {
			out = append(out, r)
		}
	}
	return out
}

// clarificationRecords returns every "You (clarification):" user record, in order.
func clarificationRecords(sw *SessionWindow) []*transcriptRecord {
	var out []*transcriptRecord
	for _, r := range sw.transcript.records {
		if r.header == clarificationHeader {
			out = append(out, r)
		}
	}
	return out
}

// lastClarification returns the most recent "You (clarification):" record, or nil.
func lastClarification(sw *SessionWindow) *transcriptRecord {
	recs := clarificationRecords(sw)
	if len(recs) == 0 {
		return nil
	}
	return recs[len(recs)-1]
}

// TestInterjectRendersAsUserRecord is the core #242 assertion: interjecting a
// message while a turn runs appends a kindUser record — the SAME record type as a
// normally-sent "You:" message — carrying a "You (clarification):" header, the
// user colour/role, and the user's verbatim text. It must NOT be a [System] note
// or a model "thought".
func TestInterjectRendersAsUserRecord(t *testing.T) {
	w := newTestWorkbench(t)
	injected := recordInject(w)
	sw := w.openWindow("s", "S")
	sw.busy = true

	const body = "please use base 16 not 10"
	before := len(sw.transcript.records)
	sw.input.SetText(body)
	sw.interject()

	// Mid-turn delivery to the model is preserved (issue #242 fixes rendering only).
	if got := waitInject(t, injected); got != body {
		t.Fatalf("OnInject = %q, want %q (delivery must still fire)", got, body)
	}

	// Exactly one new transcript record was added.
	if got := len(sw.transcript.records); got != before+1 {
		t.Fatalf("interject added %d records, want exactly 1", got-before)
	}
	rec := lastRecord(sw)

	// It is a USER record (the same kind as a normal "You:" send)...
	if rec.kind != kindUser {
		t.Errorf("record kind = %v, want kindUser", rec.kind)
	}
	// ...not a system note or a model thought.
	if rec.kind == kindSystem {
		t.Errorf("interject rendered as a [System] note (kindSystem); issue #242 regressed")
	}
	if rec.kind == kindThinking {
		t.Errorf("interject rendered as a model thought (kindThinking); issue #242 regressed")
	}

	// Header, colour and role match a user message.
	if rec.header != clarificationHeader {
		t.Errorf("header = %q, want %q", rec.header, clarificationHeader)
	}
	if rec.role != roleUser {
		t.Errorf("role = %v, want roleUser", rec.role)
	}
	if rec.headerColor() != colorUser {
		t.Errorf("headerColor = %+v, want colorUser %+v", rec.headerColor(), colorUser)
	}
	// Body is the user's verbatim text.
	if got := rec.body(); got != body {
		t.Errorf("body = %q, want %q", got, body)
	}
}

// TestInterjectRecordIsRendered confirms the clarification record is actually
// painted into the backing view (it has a live entry), so it is not merely held
// in the model — the user sees it.
func TestInterjectRecordIsRendered(t *testing.T) {
	w := newTestWorkbench(t)
	recordInject(w)
	sw := w.openWindow("s", "S")
	sw.busy = true

	sw.input.SetText("show me")
	sw.interject()

	rec := lastClarification(sw)
	if rec == nil {
		t.Fatal("no clarification record after interject")
	}
	if rec.entry == nil {
		t.Error("clarification record has no live TextView entry; it would not be visible")
	}
}

// TestInterjectNotDuplicatedAsSystemOrThought pins down the second half of #242:
// the interjection must appear exactly once, and must NOT additionally produce a
// [System] "interjected:" echo (display point 1) or a "thought" entry (display
// point 2's UI side). We measure system/thinking record counts around the call.
func TestInterjectNotDuplicatedAsSystemOrThought(t *testing.T) {
	w := newTestWorkbench(t)
	recordInject(w)
	sw := w.openWindow("s", "S")
	sw.busy = true

	sysBefore := len(recordsOfKind(sw, kindSystem))
	thinkBefore := len(recordsOfKind(sw, kindThinking))

	sw.input.SetText("a clarification that is uniquely identifying")
	sw.interject()

	if got := len(recordsOfKind(sw, kindSystem)); got != sysBefore {
		t.Errorf("interject added a kindSystem record (%d → %d); the [System] echo must be gone",
			sysBefore, got)
	}
	if got := len(recordsOfKind(sw, kindThinking)); got != thinkBefore {
		t.Errorf("interject added a kindThinking record (%d → %d); no thought entry expected",
			thinkBefore, got)
	}
	// And no record carries the old "[System] interjected:" wording anywhere.
	for _, r := range sw.transcript.records {
		if strings.Contains(r.header, "[System]") && strings.Contains(strings.ToLower(r.header), "interject") {
			t.Errorf("stale [System] interjected header still present: %q", r.header)
		}
		for _, ln := range r.lines {
			if strings.Contains(strings.ToLower(ln.text), "interjected:") {
				t.Errorf("stale 'interjected:' system echo still present in a line: %q", ln.text)
			}
		}
	}
}

// TestInterjectUserRecordClassifiedAsUser proves the clarification shares the
// user channel for the transcript filter/search: toggling the user-type filter
// hides it (just like a normal "You:" message), while toggling system or thinking
// does not. This is the telltale asymmetry called out in the issue — before the
// fix the interjection landed in the system/thinking channels.
func TestInterjectUserRecordClassifiedAsUser(t *testing.T) {
	w := newTestWorkbench(t)
	recordInject(w)
	sw := w.openWindow("s", "S")
	sw.busy = true

	sw.input.SetText("findthis clarification")
	sw.interject()

	rec := lastClarification(sw)
	if rec == nil {
		t.Fatal("no clarification record")
	}

	// Toggling the USER filter hides the clarification (it is kindUser).
	sw.transcript.toggleKind(kindUser)
	if sw.transcript.visible(rec) {
		t.Error("clarification should be hidden when the user filter is toggled, like a normal user message")
	}
	sw.transcript.toggleKind(kindUser) // restore

	// Toggling system or thinking must NOT hide it.
	sw.transcript.toggleKind(kindSystem)
	if !sw.transcript.visible(rec) {
		t.Error("toggling the system filter hid the clarification; it is not a system record")
	}
	sw.transcript.toggleKind(kindSystem)
	sw.transcript.toggleKind(kindThinking)
	if !sw.transcript.visible(rec) {
		t.Error("toggling the thinking filter hid the clarification; it is not a thought record")
	}
	sw.transcript.toggleKind(kindThinking)

	// Search by the user's text finds it.
	sw.transcript.setQuery("findthis")
	if !sw.transcript.visible(rec) {
		t.Error("search by the interjected text did not find the clarification record")
	}
}

// TestInterjectDistinctFromNormalSend ensures the clarification header differs
// from a normal user send ("You:") so the two remain visually distinguishable,
// while BOTH are kindUser.
func TestInterjectDistinctFromNormalSend(t *testing.T) {
	w := newTestWorkbench(t)
	recordInject(w)
	sw := w.openWindow("s", "S")

	// A normal send happens while IDLE (the submit path calls addUser only on the
	// non-busy branch; while busy it would queue instead). It yields "You:".
	sw.input.SetText("normal message")
	sw.submitFn()
	normal := lastRecord(sw)
	if normal == nil || normal.header != "You:" || normal.kind != kindUser {
		t.Fatalf("normal send = %+v, want header %q kindUser", normal, "You:")
	}

	// The submit set busy; an interject now yields "You (clarification):" — a
	// different header, but the SAME kind as the normal send above.
	sw.input.SetText("a clarification")
	sw.interject()
	rec := lastRecord(sw)
	if rec.header != clarificationHeader {
		t.Errorf("interject header = %q, want %q (distinct from normal send)", rec.header, clarificationHeader)
	}
	if rec.kind != kindUser {
		t.Errorf("interject kind = %v, want kindUser (same as normal send)", rec.kind)
	}
}

// TestInterjectMultipleProducesOrderedUserRecords: several interjections append
// several kindUser clarification records, in entry order, each with its own text.
func TestInterjectMultipleProducesOrderedUserRecords(t *testing.T) {
	w := newTestWorkbench(t)
	recordInject(w)
	sw := w.openWindow("s", "S")
	sw.busy = true

	notes := []string{"first clarification", "second one", "and a third"}
	for _, n := range notes {
		sw.input.SetText(n)
		sw.interject()
	}

	recs := clarificationRecords(sw)
	if len(recs) != len(notes) {
		t.Fatalf("got %d clarification records, want %d", len(recs), len(notes))
	}
	for i, r := range recs {
		if r.kind != kindUser {
			t.Errorf("record %d kind = %v, want kindUser", i, r.kind)
		}
		if r.body() != notes[i] {
			t.Errorf("record %d body = %q, want %q", i, r.body(), notes[i])
		}
	}
}

// TestInterjectPreservesMultilineBody: a multi-line interjection keeps its
// newlines in the rendered body (the yank/search text), the same as a normal
// multi-line user message.
func TestInterjectPreservesMultilineBody(t *testing.T) {
	w := newTestWorkbench(t)
	recordInject(w)
	sw := w.openWindow("s", "S")
	sw.busy = true

	body := "line one\nline two\nline three"
	sw.input.SetText(body)
	sw.interject()

	rec := lastClarification(sw)
	if rec == nil {
		t.Fatal("no clarification record")
	}
	if got := rec.body(); got != body {
		t.Errorf("multiline body = %q, want %q", got, body)
	}
	// Each physical line is its own styled child.
	if len(rec.lines) != 3 {
		t.Errorf("got %d child lines, want 3", len(rec.lines))
	}
	for _, ln := range rec.lines {
		if ln.role != roleUser {
			t.Errorf("child line role = %v, want roleUser", ln.role)
		}
	}
}

// TestInterjectTrimsWhitespaceAndStillClarifies: surrounding whitespace is
// trimmed before the record is created (so the rendered body and the injected
// text are both the trimmed form), matching the trim already applied to the
// OnInject payload.
func TestInterjectTrimsWhitespaceAndStillClarifies(t *testing.T) {
	w := newTestWorkbench(t)
	injected := recordInject(w)
	sw := w.openWindow("s", "S")
	sw.busy = true

	sw.input.SetText("  trimmed clarification  ")
	sw.interject()

	rec := lastClarification(sw)
	if rec == nil {
		t.Fatal("no clarification record")
	}
	if rec.body() != "trimmed clarification" {
		t.Errorf("body = %q, want trimmed %q", rec.body(), "trimmed clarification")
	}
	if got := waitInject(t, injected); got != "trimmed clarification" {
		t.Errorf("OnInject = %q, want trimmed %q", got, "trimmed clarification")
	}
}

// --- Edge cases: the interject path's existing guards must still hold, and
// must NOT fabricate a clarification record when there is nothing to say. ---

// TestInterjectBlankInputAddsNoRecord: whitespace-only input is a no-op — no
// record of any kind is added, nothing is injected.
func TestInterjectBlankInputAddsNoRecord(t *testing.T) {
	w := newTestWorkbench(t)
	injected := recordInject(w)
	sw := w.openWindow("s", "S")
	sw.busy = true

	for _, blank := range []string{"", "   ", "\t\n  "} {
		before := len(sw.transcript.records)
		sw.input.SetText(blank)
		sw.interject()
		if got := len(sw.transcript.records); got != before {
			t.Errorf("blank input %q added a record (%d → %d); expected no-op", blank, before, got)
		}
		noInject(t, injected)
		if lastClarification(sw) != nil {
			t.Errorf("blank input %q produced a clarification record", blank)
		}
	}
}

// TestInterjectIdleAddsNoRecord: with no turn running, interject is a no-op — no
// record, no injection, input preserved.
func TestInterjectIdleAddsNoRecord(t *testing.T) {
	w := newTestWorkbench(t)
	injected := recordInject(w)
	sw := w.openWindow("s", "S")
	// sw.busy left false (idle).

	before := len(sw.transcript.records)
	sw.input.SetText("nothing in flight")
	sw.interject()

	if got := len(sw.transcript.records); got != before {
		t.Errorf("idle interject added a record (%d → %d); expected no-op", before, got)
	}
	noInject(t, injected)
	if got := sw.input.GetText(); strings.TrimSpace(got) != "nothing in flight" {
		t.Errorf("idle interject should preserve input, got %q", got)
	}
	if lastClarification(sw) != nil {
		t.Error("idle interject produced a clarification record")
	}
}

// TestInterjectUnavailableStillSystemNote: when there is no backend injection
// handler, interject surfaces a [System] "interject unavailable" note — genuine
// client-side feedback — NOT a user clarification, and leaves the input intact.
// This guards that the #242 change did not swallow the unavailable path.
func TestInterjectUnavailableStillSystemNote(t *testing.T) {
	w := newTestWorkbench(t)
	w.handlers.OnInject = nil
	sw := w.openWindow("s", "S")
	sw.busy = true

	before := len(sw.transcript.records)
	sw.input.SetText("try to interject")
	sw.interject()

	// Exactly one new record, and it is a SYSTEM note (not a user clarification).
	if got := len(sw.transcript.records); got != before+1 {
		t.Fatalf("expected exactly one 'unavailable' note, got %d new records", got-before)
	}
	rec := lastRecord(sw)
	if rec.kind != kindSystem {
		t.Errorf("unavailable note kind = %v, want kindSystem", rec.kind)
	}
	if !strings.Contains(strings.ToLower(rec.body()), "interject unavailable") {
		t.Errorf("unavailable note body = %q, want it to mention 'interject unavailable'", rec.body())
	}
	if lastClarification(sw) != nil {
		t.Error("unavailable path must not create a user clarification record")
	}
	if got := sw.input.GetText(); got != "try to interject" {
		t.Errorf("input should be preserved when interject is unavailable, got %q", got)
	}
}

// TestInterjectClearsInputAfterClarification: a successful interject clears the
// input box (the clarification is now in the transcript), matching the pre-#242
// behaviour.
func TestInterjectClearsInputAfterClarification(t *testing.T) {
	w := newTestWorkbench(t)
	recordInject(w)
	sw := w.openWindow("s", "S")
	sw.busy = true

	sw.input.SetText("clears me")
	sw.interject()
	if got := strings.TrimSpace(sw.input.GetText()); got != "" {
		t.Errorf("input should be cleared after interject, got %q", got)
	}
}

// TestInterjectDoesNotDuplicateViaAssistantStepEvent guards display point 2 at
// the UI boundary: even if something fed the framed clarification in as a
// SessionEventAssistantStep, the interject path itself must not have already
// doubled it. We interject, then simulate a stray AssistantStep carrying the
// note and assert the result is one clarification + one thought (i.e. the UI
// still treats AssistantStep as a thought — the FIX is that the backend no
// longer emits it, not that apply() changed).
func TestInterjectDoesNotDuplicateViaAssistantStepEvent(t *testing.T) {
	w := newTestWorkbench(t)
	recordInject(w)
	sw := w.openWindow("s", "S")
	sw.busy = true

	sw.input.SetText("my note")
	sw.interject()

	clarifs := clarificationRecords(sw)
	if len(clarifs) != 1 {
		t.Fatalf("expected exactly 1 clarification before stray event, got %d", len(clarifs))
	}

	// A SessionEventAssistantStep is STILL routed to addThought (model reasoning).
	// This is intentional — the fix removed the backend emit, not the routing.
	sw.apply(agent.SessionEvent{Type: agent.SessionEventAssistantStep, Text: "model reasoning here"})
	if got := len(recordsOfKind(sw, kindThinking)); got != 1 {
		t.Errorf("AssistantStep should still render as a thought, got %d thinking records", got)
	}
	// And the clarification is unchanged — still exactly one.
	if len(clarificationRecords(sw)) != 1 {
		t.Errorf("stray AssistantStep changed the clarification count; want 1, got %d", len(clarificationRecords(sw)))
	}
}
