package ui

import (
	"fmt"
	"strings"
	"testing"

	tv "github.com/hobbestherat/turbotui/turbotv"
)

// newTestSession builds a SessionWindow with only its transcript model wired,
// which is all the add*/transcript controls need. The backing TextView is
// headless (no terminal required).
func newTestSession() *SessionWindow {
	return &SessionWindow{
		transcript:   newTranscriptModel(tv.NewTextView("", tv.Rect{})),
		pendingTools: map[string]*transcriptRecord{},
	}
}

// populate fills a session with one record of each interesting kind so search
// and filter behaviour can be asserted against a known transcript.
func populate(sw *SessionWindow) {
	sw.addUser("hello world")
	sw.addThought("considering the options")
	sw.beginToolCall("call_read", "Read", map[string]interface{}{"path": "main.go"})
	sw.finishToolCall("call_read", "Read", "package main")
	sw.addAssistant("done reading the file")
	sw.addError("disk on fire")
}

func TestRecordMatches(t *testing.T) {
	rec := &transcriptRecord{
		header: "tool: Read (done)",
		lines: []styledLine{
			{text: "  path: main.go"},
			{text: "  result:"},
		},
	}
	for _, tc := range []struct {
		query string
		want  bool
	}{
		{"read", true},       // header (matches assumes a lower-cased query)
		{"main.go", true},    // child line
		{"missing", false},   // absent
		{"tool: read", true}, // spans header text
	} {
		if got := rec.matches(strings.ToLower(tc.query)); got != tc.want {
			t.Errorf("matches(%q) = %v, want %v", tc.query, got, tc.want)
		}
	}
}

func TestTranscriptSearch(t *testing.T) {
	sw := newTestSession()
	populate(sw)
	m := sw.transcript

	m.setQuery("read")
	all := m.view.AllText()
	if m.matchCount() != 2 {
		t.Fatalf("matchCount = %d, want 2 (tool + assistant), got view:\n%s", m.matchCount(), all)
	}
	for _, want := range []string{"tool: Read (done)", "done reading the file", `find "read": 2`} {
		if !strings.Contains(all, want) {
			t.Errorf("filtered view missing %q\n%s", want, all)
		}
	}
	for _, absent := range []string{"hello world", "considering the options", "disk on fire"} {
		if strings.Contains(all, absent) {
			t.Errorf("filtered view should not contain %q\n%s", absent, all)
		}
	}

	// Searching matches child lines (tool args) too.
	m.setQuery("main.go")
	if m.matchCount() != 1 {
		t.Errorf("matchCount for 'main.go' = %d, want 1", m.matchCount())
	}

	// Clearing restores the full transcript.
	m.showAll()
	full := m.view.AllText()
	for _, want := range []string{"hello world", "disk on fire", "considering the options"} {
		if !strings.Contains(full, want) {
			t.Errorf("after showAll, view missing %q\n%s", want, full)
		}
	}
	if strings.Contains(full, "Esc to clear") {
		t.Errorf("status note should be gone after showAll\n%s", full)
	}
}

func TestTranscriptFilterByKind(t *testing.T) {
	sw := newTestSession()
	populate(sw)
	m := sw.transcript

	m.toggleKind(kindThinking)
	if got := m.view.AllText(); strings.Contains(got, "considering the options") {
		t.Errorf("thinking entry should be hidden\n%s", got)
	}
	if !strings.Contains(m.view.AllText(), "hidden: thinking") {
		t.Errorf("status note should report hidden: thinking\n%s", m.view.AllText())
	}

	m.toggleKind(kindTool)
	if got := m.view.AllText(); strings.Contains(got, "tool: Read") {
		t.Errorf("tool entry should be hidden\n%s", got)
	}
	// Other kinds still present.
	for _, want := range []string{"hello world", "done reading the file", "disk on fire"} {
		if !strings.Contains(m.view.AllText(), want) {
			t.Errorf("unfiltered kind missing %q\n%s", want, m.view.AllText())
		}
	}

	// Toggling the same kinds back off restores them.
	m.toggleKind(kindThinking)
	m.toggleKind(kindTool)
	if m.filtering() {
		t.Errorf("expected no active filter after toggling all kinds back")
	}
	for _, want := range []string{"considering the options", "tool: Read"} {
		if !strings.Contains(m.view.AllText(), want) {
			t.Errorf("restored kind missing %q\n%s", want, m.view.AllText())
		}
	}
}

func TestTranscriptFoldAll(t *testing.T) {
	sw := newTestSession()
	populate(sw)
	m := sw.transcript

	m.setFold(true)
	for _, r := range m.records {
		if !r.collapsed {
			t.Errorf("record %q not collapsed after fold-all", r.header)
		}
	}

	m.setFold(false)
	for _, r := range m.records {
		if r.collapsed {
			t.Errorf("record %q still collapsed after unfold-all", r.header)
		}
	}
}

// TestToolCallMerge verifies a tool call and its result fold into one entry with
// a "(done)" header and the result body, and that the pending pointer clears.
func TestToolCallMerge(t *testing.T) {
	sw := newTestSession()
	sw.beginToolCall("call_edit", "Edit", map[string]interface{}{"file": "x.go"})
	if sw.pendingTools["call_edit"] == nil {
		t.Fatal("expected a pending tool record after beginToolCall")
	}
	sw.finishToolCall("call_edit", "Edit", "ok")
	if sw.pendingTools["call_edit"] != nil {
		t.Error("pending tool entry should be cleared after finishToolCall")
	}
	got := sw.transcript.view.AllText()
	for _, want := range []string{"tool: Edit (done)", "file: x.go", "result:", "ok"} {
		if !strings.Contains(got, want) {
			t.Errorf("merged tool entry missing %q\n%s", want, got)
		}
	}
	// Exactly one tool record exists (call+result merged, not two).
	tools := 0
	for _, r := range sw.transcript.records {
		if r.kind == kindTool {
			tools++
		}
	}
	if tools != 1 {
		t.Errorf("expected 1 merged tool record, got %d", tools)
	}
}

// TestRestoreIndexesTranscript checks that a restored transcript is searchable,
// i.e. its messages land in the indexed model.
func TestRestoreIndexesTranscript(t *testing.T) {
	sw := newTestSession()
	sw.restore([]ChatMessage{
		{Role: "user", Content: "fix the bug"},
		{Role: "assistant", Content: "looking into it", Tool: "Grep", Args: "pattern: TODO"},
		{Role: "tool", Tool: "Grep", Content: "found 3 hits"},
		{Role: "system", Content: "session resumed"},
	})
	m := sw.transcript
	if len(m.records) != 5 { // user, assistant, tool-call, tool-result, system
		t.Fatalf("restore produced %d records, want 5", len(m.records))
	}
	m.setQuery("grep")
	if m.matchCount() != 2 {
		t.Errorf("search over restored transcript: matchCount = %d, want 2", m.matchCount())
	}
}

// addRecords appends n user messages whose text uniquely identifies each (e.g.
// "msg-0007"), returning the texts in insertion order so callers can assert
// which survived a cap-driven trim.
func addRecords(sw *SessionWindow, n int) []string {
	texts := make([]string, n)
	for i := 0; i < n; i++ {
		texts[i] = fmt.Sprintf("msg-%04d", i)
		sw.addUser(texts[i])
	}
	return texts
}

// TestTranscriptCapBoundsRecords verifies the live record slice and rendered view
// never exceed the configured limit: the newest entry is always retained and the
// oldest is dropped once the limit is crossed. With no trimming the transcript is
// untouched.
func TestTranscriptCapBoundsRecords(t *testing.T) {
	for _, tc := range []struct {
		name  string
		limit int
		adds  int
	}{
		{"small cap many adds", 10, 25},
		{"cap exceeded once", 8, 9},
		{"large add stream", 50, 500},
		{"no trim under cap", 100, 50},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sw := newTestSession()
			sw.transcript.limit = tc.limit
			texts := addRecords(sw, tc.adds)
			m := sw.transcript

			if len(m.records) > tc.limit {
				t.Errorf("len(records) = %d, want <= limit %d", len(m.records), tc.limit)
			}

			all := m.view.AllText()
			newest := texts[tc.adds-1]
			if !strings.Contains(all, newest) {
				t.Errorf("newest record %q dropped from view\n%s", newest, all)
			}

			oldest := texts[0]
			trimmed := tc.adds > tc.limit
			switch {
			case trimmed && strings.Contains(all, oldest):
				t.Errorf("oldest record %q should have been trimmed\n%s", oldest, all)
			case !trimmed && !strings.Contains(all, oldest):
				t.Errorf("oldest record %q should still be present\n%s", oldest, all)
			}
		})
	}
}

// TestTranscriptCapZeroUnbounded confirms a zero limit disables trimming entirely
// (used only by tests that opt out of the cap).
func TestTranscriptCapZeroUnbounded(t *testing.T) {
	sw := newTestSession()
	sw.transcript.limit = 0
	addRecords(sw, 50)
	if got := len(sw.transcript.records); got != 50 {
		t.Errorf("with limit 0, len(records) = %d, want 50 (no trimming)", got)
	}
}

// TestTranscriptCapKeepsInFlightTool confirms that even after heavy trimming the
// newest record — an in-flight tool call — is never dropped, so its result still
// folds into the same entry when it arrives.
func TestTranscriptCapKeepsInFlightTool(t *testing.T) {
	sw := newTestSession()
	sw.transcript.limit = 5
	addRecords(sw, 20) // fill well past the cap
	sw.beginToolCall("call_read", "Read", map[string]interface{}{"path": "x.go"})
	sw.finishToolCall("call_read", "Read", "the result body")
	got := sw.transcript.view.AllText()
	for _, want := range []string{"tool: Read (done)", "result:", "the result body"} {
		if !strings.Contains(got, want) {
			t.Errorf("in-flight tool entry missing %q after cap trims\n%s", want, got)
		}
	}
}
