package ui

import (
	"strings"
	"testing"

	tv "github.com/hobbestherat/turbotui/turbotv"
)

// newTestSession builds a SessionWindow with only its transcript model wired,
// which is all the add*/transcript controls need. The backing TextView is
// headless (no terminal required).
func newTestSession() *SessionWindow {
	return &SessionWindow{transcript: newTranscriptModel(tv.NewTextView("", tv.Rect{}))}
}

// populate fills a session with one record of each interesting kind so search
// and filter behaviour can be asserted against a known transcript.
func populate(sw *SessionWindow) {
	sw.addUser("hello world")
	sw.addThought("considering the options")
	sw.beginToolCall("Read", map[string]interface{}{"path": "main.go"})
	sw.finishToolCall("Read", "package main")
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
	sw.beginToolCall("Edit", map[string]interface{}{"file": "x.go"})
	if sw.pendingTool == nil {
		t.Fatal("expected a pending tool record after beginToolCall")
	}
	sw.finishToolCall("Edit", "ok")
	if sw.pendingTool != nil {
		t.Error("pendingTool should be cleared after finishToolCall")
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
