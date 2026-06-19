package ui

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	"gogent/internal/clipboard"
)

// toTexts extracts the plain text from a slice of styled lines.
func toTexts(lines []styledLine) []string {
	out := make([]string, len(lines))
	for i, ln := range lines {
		out[i] = ln.text
	}
	return out
}

// decodeOSC52 extracts and base64-decodes the payload of the first OSC 52
// clipboard-set sequence in s, so yank tests can assert what was copied without
// touching the real clipboard.
func decodeOSC52(t *testing.T, s string) string {
	t.Helper()
	const prefix = "\x1b]52;c;"
	i := strings.Index(s, prefix)
	if i < 0 {
		t.Fatalf("no OSC 52 prefix in %q", s)
	}
	rest := s[i+len(prefix):]
	j := strings.IndexByte(rest, '\x07')
	if j < 0 {
		t.Fatalf("no BEL terminator in %q", rest)
	}
	dec, err := base64.StdEncoding.DecodeString(rest[:j])
	if err != nil {
		t.Fatalf("decode OSC 52 payload %q: %v", rest[:j], err)
	}
	return string(dec)
}

func TestLastAssistantRecordAndBody(t *testing.T) {
	m := newTranscriptModel(nil)
	// Set records directly (add would render into a nil view); lastAssistantRecord
	// and body only read the records, so this exercises them in isolation.
	m.records = []*transcriptRecord{
		{kind: kindUser, header: "You:", lines: []styledLine{{text: "hi"}}},
		{kind: kindThinking, header: "thought", lines: []styledLine{{text: "hmm"}}},
		{kind: kindAssistant, header: "Gogent:", lines: []styledLine{
			{text: "line one"}, {text: "line two"}}},
		{kind: kindUser, header: "You:", lines: []styledLine{{text: "again"}}},
	}

	rec := m.lastAssistantRecord()
	if rec == nil {
		t.Fatal("expected an assistant record, got nil")
	}
	if got := rec.body(); got != "line one\nline two" {
		t.Errorf("body = %q, want %q", got, "line one\nline two")
	}
}

func TestLastAssistantRecordNone(t *testing.T) {
	m := newTranscriptModel(nil)
	if rec := m.lastAssistantRecord(); rec != nil {
		t.Fatalf("expected nil, got %+v", rec)
	}
	if got := m.lastAssistantRecord().body(); got != "" {
		t.Errorf("nil body = %q, want empty", got)
	}
}

func TestExtractFencedCode(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   string
		want string
	}{
		{"no fence", "just prose\nno code", ""},
		{"single fence", "here:\n```go\nfmt.Println(\"hi\")\n```\ndone", "fmt.Println(\"hi\")"},
		{"info string stripped", "```\ncode\n```", "code"},
		{"multiple blocks", "```a\n1\n```\nmid\n```b\n2\n```", "1\n\n2"},
		{"code with blank lines", "```\nline1\n\nline3\n```", "line1\n\nline3"},
		{"unclosed fence tolerated", "intro\n```\npartial", "partial"},
		{"indented fence", "  ```py\n  x\n  ```", "  x"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := extractFencedCode(tc.in); got != tc.want {
				t.Errorf("extractFencedCode(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestRenderTranscriptMarkdown(t *testing.T) {
	msgs := []ChatMessage{
		{Role: "user", Content: "Hello"},
		{Role: "assistant", Content: "Here is code:\n```go\nx := 1\n```"},
		{Role: "assistant", Tool: "read", Args: `{"path":"a.go"}`},
		{Role: "tool", Tool: "read", Content: "file body"},
		{Role: "system", Content: "note"},
	}
	got := renderTranscriptMarkdown(msgs, "My Session", "2026-01-02T03:04:05Z")
	checks := []string{
		"# My Session",
		"Exported by gogent on 2026-01-02T03:04:05Z",
		"## You",
		"Hello",
		"## Gogent",
		"```go\nx := 1\n```",
		"**Tool call: read**",
		"**Result: read**",
		"> note",
	}
	for _, c := range checks {
		if !strings.Contains(got, c) {
			t.Errorf("markdown missing %q in:\n%s", c, got)
		}
	}

	// Empty title falls back; empty message list is noted.
	empty := renderTranscriptMarkdown(nil, "  ", "")
	if !strings.HasPrefix(empty, "# Session transcript") {
		t.Errorf("empty-title fallback wrong: %q", empty)
	}
	if !strings.Contains(empty, "_(no messages)_") {
		t.Errorf("empty message list not noted: %q", empty)
	}
}

func TestRenderTranscriptJSONRoundTrip(t *testing.T) {
	msgs := []ChatMessage{
		{Role: "user", Content: "Hello"},
		{Role: "assistant", Content: "Hi", Tool: "read", Args: `{"path":"a"}`},
	}
	s, err := renderTranscriptJSON(msgs, "T", "2026-01-02T03:04:05Z")
	if err != nil {
		t.Fatalf("renderTranscriptJSON: %v", err)
	}
	var out exportedTranscript
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		t.Fatalf("output is not valid JSON: %v\n%s", err, s)
	}
	if out.Title != "T" || out.Exported != "2026-01-02T03:04:05Z" {
		t.Errorf("meta = %+v", out)
	}
	if len(out.Messages) != 2 {
		t.Fatalf("messages = %d, want 2", len(out.Messages))
	}
	if out.Messages[0].Role != "user" || out.Messages[0].Content != "Hello" {
		t.Errorf("msg0 = %+v", out.Messages[0])
	}
	if out.Messages[1].Tool != "read" || out.Messages[1].Args == "" {
		t.Errorf("msg1 = %+v", out.Messages[1])
	}
}

func TestSanitizeFileSlug(t *testing.T) {
	for _, tc := range []struct {
		in, want string
	}{
		{"My Refactor", "my-refactor"},
		{"Session 1", "session-1"},
		{"  spaced  ", "spaced"},
		{"a/b\\c:d", "a-b-c-d"},
		{"---", "transcript"},
		{"", "transcript"},
		{"Uppercase_", "uppercase_"},
	} {
		if got := sanitizeFileSlug(tc.in); got != tc.want {
			t.Errorf("sanitizeFileSlug(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestWriteTranscriptExport(t *testing.T) {
	msgs := []ChatMessage{{Role: "user", Content: "hi"}, {Role: "assistant", Content: "bye"}}
	for _, tc := range []struct {
		name   string
		format string
		suffix string
	}{
		{"markdown", "md", ".md"},
		{"json", "json", ".json"},
		{"unknown defaults to json", "xml", ".json"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var captured string
			orig := transcriptExporter
			transcriptExporter = func(path, data string) error { captured = data; return nil }
			t.Cleanup(func() { transcriptExporter = orig })

			path, err := writeTranscriptExport(msgs, "My Refactor", tc.format)
			if err != nil {
				t.Fatalf("writeTranscriptExport: %v", err)
			}
			if !strings.HasSuffix(path, tc.suffix) {
				t.Errorf("path = %q, want suffix %s", path, tc.suffix)
			}
			if !strings.Contains(path, "my-refactor") {
				t.Errorf("path missing title slug: %q", path)
			}
			if strings.TrimSpace(captured) == "" {
				t.Errorf("no content written for %s", tc.format)
			}
		})
	}
}

// TestCopyLastAnswerAndCode drives the session window's yank actions through the
// real clipboard board (writing OSC 52 to a buffer) and checks both the copied
// payload and the confirmation note.
func TestCopyLastAnswerAndCode(t *testing.T) {
	w := newTestWorkbench(t)
	sw := w.openWindow("s", "S")
	var buf bytes.Buffer
	w.clipboard = clipboard.New(&buf)

	// No assistant yet: yank reports nothing to copy and writes nothing.
	sw.copyLastAnswer()
	if buf.Len() != 0 {
		t.Errorf("expected no clipboard write before an answer, got %q", decodeOSC52(t, buf.String()))
	}

	sw.addUser("what?")
	sw.addThought("pondering") // must be skipped by last-answer
	sw.addAssistant("Here you go:\n```go\nx := 42\n```\nand some prose")

	buf.Reset()
	sw.copyLastAnswer()
	if got := decodeOSC52(t, buf.String()); !strings.Contains(got, "Here you go") || !strings.Contains(got, "and some prose") {
		t.Errorf("copyLastAnswer copied %q, want the full answer", got)
	}

	buf.Reset()
	sw.copyLastCode()
	if got := decodeOSC52(t, buf.String()); got != "x := 42" {
		t.Errorf("copyLastCode copied %q, want %q", got, "x := 42")
	}

	// The final record is the confirmation note.
	last := sw.transcript.records[len(sw.transcript.records)-1]
	if last.kind != kindSystem || !strings.Contains(strings.Join(toTexts(last.lines), " "), "clipboard") {
		t.Errorf("expected a clipboard confirmation note, got kind=%d lines=%v", last.kind, last.lines)
	}
}

// TestCopyLastCodeNoFence verifies a prose-only answer reports no code block.
func TestCopyLastCodeNoFence(t *testing.T) {
	w := newTestWorkbench(t)
	sw := w.openWindow("s", "S")
	var buf bytes.Buffer
	w.clipboard = clipboard.New(&buf)

	sw.addAssistant("just words, no code at all")
	sw.copyLastCode()

	if buf.Len() != 0 {
		t.Errorf("expected no clipboard write, got %q", decodeOSC52(t, buf.String()))
	}
	last := sw.transcript.records[len(sw.transcript.records)-1]
	joined := strings.Join(toTexts(last.lines), " ")
	if !strings.Contains(joined, "no code block") {
		t.Errorf("expected 'no code block' note, got %q", joined)
	}
}

// TestExportActiveWritesThroughHandler verifies exportActive renders the handler's
// transcript and surfaces the written path (the confirm dialog content is built
// from the same path the writer returns).
func TestExportActiveWritesThroughHandler(t *testing.T) {
	w := newTestWorkbench(t)
	w.openWindow("s", "My Chat")

	var captured string
	orig := transcriptExporter
	transcriptExporter = func(path, data string) error { captured = data; return nil }
	t.Cleanup(func() { transcriptExporter = orig })

	w.handlers.GetTranscript = func(sessionID, agentID string) []ChatMessage {
		return []ChatMessage{{Role: "user", Content: "ping"}, {Role: "assistant", Content: "pong"}}
	}

	w.exportActive("md")
	if !strings.Contains(captured, "# My Chat") || !strings.Contains(captured, "ping") {
		t.Errorf("export did not render the transcript, got:\n%s", captured)
	}
}
