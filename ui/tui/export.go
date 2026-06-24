package ui

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// fenceMarker is the Markdown fenced-code delimiter a yank/export recognises.
const fenceMarker = "```"

// extractFencedCode returns the contents of the Markdown fenced code blocks
// (```) found in s, joined with a blank line between blocks. It strips the fence
// markers and any info string (e.g. ```go); text outside fences is ignored, so
// yank-last-code copies just the code the assistant produced. Returns "" when
// there is no fenced block. An unclosed fence yields what was accumulated.
func extractFencedCode(s string) string {
	var blocks []string
	var b strings.Builder
	inFence := false
	for _, line := range strings.Split(s, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), fenceMarker) {
			if inFence {
				blocks = append(blocks, b.String())
				b.Reset()
				inFence = false
			} else {
				inFence = true
			}
			continue
		}
		if inFence {
			if b.Len() > 0 {
				b.WriteByte('\n')
			}
			b.WriteString(line)
		}
	}
	if inFence && b.Len() > 0 { // tolerate an unclosed fence
		blocks = append(blocks, b.String())
	}
	return strings.Join(blocks, "\n\n")
}

// exportedMessage is one message in a structured (JSON) transcript export.
type exportedMessage struct {
	Role      string `json:"role"`
	Content   string `json:"content,omitempty"`
	Tool      string `json:"tool,omitempty"`
	Args      string `json:"args,omitempty"`
	Reasoning string `json:"reasoning,omitempty"`
}

// exportedTranscript is the structured shape of a JSON session export.
type exportedTranscript struct {
	Title    string            `json:"title"`
	Exported string            `json:"exported_at"`
	Messages []exportedMessage `json:"messages"`
}

// renderTranscriptMarkdown renders chat messages as a readable Markdown
// transcript: a title header, then each turn. User and assistant text is emitted
// verbatim (so an assistant's fenced code blocks survive intact); tool calls and
// results are wrapped in fenced blocks so they stay readable and round-trip.
// exportedAt (RFC3339) is noted under the title. Pure so it can be unit tested.
func renderTranscriptMarkdown(msgs []ChatMessage, title, exportedAt string) string {
	if strings.TrimSpace(title) == "" {
		title = "Session transcript"
	}
	var b strings.Builder
	b.WriteString("# ")
	b.WriteString(title)
	b.WriteString("\n\n")
	if exportedAt != "" {
		b.WriteString("_Exported by gogent on ")
		b.WriteString(exportedAt)
		b.WriteString("._\n\n")
	}
	if len(msgs) == 0 {
		b.WriteString("_(no messages)_\n")
		return b.String()
	}
	for _, m := range msgs {
		switch strings.ToLower(m.Role) {
		case "user":
			b.WriteString("## You\n\n")
			b.WriteString(m.Content)
			b.WriteString("\n\n")
		case "assistant":
			b.WriteString("## Gogent\n\n")
			if strings.TrimSpace(m.Content) != "" {
				b.WriteString(m.Content)
				b.WriteString("\n\n")
			}
			if m.Tool != "" {
				b.WriteString("**Tool call: ")
				b.WriteString(m.Tool)
				b.WriteString("**\n\n```json\n")
				b.WriteString(m.Args)
				b.WriteString("\n```\n\n")
			}
		case "tool":
			b.WriteString("**Result: ")
			b.WriteString(m.Tool)
			b.WriteString("**\n\n```\n")
			b.WriteString(m.Content)
			b.WriteString("\n```\n\n")
		default: // system / other
			if strings.TrimSpace(m.Content) != "" {
				b.WriteString("> ")
				b.WriteString(strings.ReplaceAll(m.Content, "\n", "\n> "))
				b.WriteString("\n\n")
			}
		}
	}
	return b.String()
}

// renderTranscriptJSON renders chat messages as a structured JSON transcript
// (title, export timestamp and the message list). Pure so it can be unit tested.
func renderTranscriptJSON(msgs []ChatMessage, title, exportedAt string) (string, error) {
	out := exportedTranscript{
		Title:    title,
		Exported: exportedAt,
		Messages: make([]exportedMessage, 0, len(msgs)),
	}
	for _, m := range msgs {
		out.Messages = append(out.Messages, exportedMessage(m))
	}
	b, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return "", fmt.Errorf("marshal transcript json: %w", err)
	}
	return string(b), nil
}

// transcriptExporter decouples the export path from the filesystem so it can be
// unit tested with an in-memory writer. It defaults to writing a real file.
var transcriptExporter = func(path, data string) error {
	return os.WriteFile(path, []byte(data), 0o600)
}

// writeTranscriptExport renders the transcript in the given format ("md" or
// "json") and writes it to a timestamped file under ~/.gogent/, returning the
// path. title supplies the document title and a slug in the filename. It mirrors
// the Statistics export path so the two exports feel consistent.
func writeTranscriptExport(msgs []ChatMessage, title, format string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home dir: %w", err)
	}
	dir := filepath.Join(home, ".gogent")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return "", fmt.Errorf("create export dir: %w", err)
	}

	now := time.Now()
	exportedAt := now.Format(time.RFC3339)
	var data string
	switch format {
	case "md":
		data = renderTranscriptMarkdown(msgs, title, exportedAt)
	default: // "json"
		format = "json"
		s, err := renderTranscriptJSON(msgs, title, exportedAt)
		if err != nil {
			return "", err
		}
		data = s
	}

	name := fmt.Sprintf("gogent-session-%s-%s.%s",
		sanitizeFileSlug(title), now.Format("20060102-150405"), format)
	path := filepath.Join(dir, name)
	if err := transcriptExporter(path, data); err != nil {
		return "", err
	}
	return path, nil
}

// sanitizeFileSlug turns a session title into a safe, filename-friendly slug.
// Empty or all-symbol titles fall back to "transcript".
func sanitizeFileSlug(title string) string {
	if title == "" {
		return "transcript"
	}
	var b strings.Builder
	for _, r := range strings.TrimSpace(strings.ToLower(title)) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-' || r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	if s := strings.Trim(b.String(), "-"); s != "" {
		return s
	}
	return "transcript"
}
