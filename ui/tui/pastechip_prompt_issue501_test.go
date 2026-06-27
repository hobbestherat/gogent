package ui

import (
	"strings"
	"testing"

	tv "github.com/hobbestherat/turbotui/turbotv"
)

// Prompt-box interaction coverage for issue #501. turbotui owns the chip *mechanics*
// (atomic caret/backspace/selection/wrap, the sentinel-rune + side-store model,
// GetText() expansion); gogent owns the prompt-box flows that run over the widget.
// These tests prove those flows hold with a chip in the buffer by driving the REAL
// paste path (Component.HandlePaste → turbotui's handlePaste → sanitizePaste → chip)
// rather than a stand-in, and by exercising the same pure helpers the prompt box
// uses (mentionToken, parseMentions) plus the GetText()-based invariants behind
// submit, slash detection, history recall and the typing-idle drain.

// chipCount returns the total number of paste-chip sentinel runes across every
// line of the input, and the single sentinel rune if there is exactly one.
func chipCount(in *tv.MultiLineInput) (n int, sentinel rune) {
	for _, line := range in.Lines {
		for _, r := range line {
			if tv.IsPasteChipRune(r) {
				n++
				sentinel = r
			}
		}
	}
	return n, sentinel
}

// TestMultiLinePasteBecomesChipAndGetTextVerbatim is the goal-match core: a
// multi-line paste collapses to exactly one atomic chip while GetText() returns the
// verbatim original (newlines intact). Drives the real paste path.
func TestMultiLinePasteBecomesChipAndGetTextVerbatim(t *testing.T) {
	in := tv.NewMultiLineInput("", tv.Rect{})
	const want = "alpha\nbeta\ngamma"
	if !in.Component.HandlePaste(want) {
		t.Fatalf("HandlePaste returned false (paste not consumed)")
	}
	n, _ := chipCount(in)
	if n != 1 {
		t.Fatalf("multi-line paste produced %d chip runes, want exactly 1 (atomic chip)", n)
	}
	if got := in.GetText(); got != want {
		t.Fatalf("GetText() after chip paste = %q, want verbatim %q", got, want)
	}
}

// TestSingleLinePasteStaysLiteral pins the "single-line paste unchanged" half of the
// acceptance criteria: a newline-free paste is inserted literally rune-by-rune, with
// NO chip (turbotui only chips a paste containing a newline).
func TestSingleLinePasteStaysLiteral(t *testing.T) {
	in := tv.NewMultiLineInput("", tv.Rect{})
	const want = "just one line, no newline"
	if !in.Component.HandlePaste(want) {
		t.Fatalf("HandlePaste returned false")
	}
	if n, _ := chipCount(in); n != 0 {
		t.Fatalf("single-line paste produced %d chip runes, want 0 (literal insert)", n)
	}
	if got := in.GetText(); got != want {
		t.Fatalf("GetText() = %q, want literal %q", got, want)
	}
}

// TestPasteChipCRLFNormalized documents the "verbatim modulo sanitisation" caveat:
// turbotui's sanitizePaste drops '\r' (CRLF→LF), so a Windows-style paste round-trips
// as '\n'. This is turbotui's behaviour and is identical for the chip and the literal
// single-line path, so it is not a gogent regression — but pin it so a future change
// to either side is caught.
func TestPasteChipCRLFNormalized(t *testing.T) {
	in := tv.NewMultiLineInput("", tv.Rect{})
	in.Component.HandlePaste("line1\r\nline2")
	if got := in.GetText(); got != "line1\nline2" {
		t.Fatalf("CRLF paste GetText() = %q, want \"line1\\nline2\" (\\r stripped by sanitizePaste)", got)
	}
}

// TestMultipleChipsExpandIndependently guards the side-store model: two separate
// multi-line pastes each collapse to their own chip, and GetText() expands BOTH to
// their own verbatim text (the store does not collapse/leak between chips).
func TestMultipleChipsExpandIndependently(t *testing.T) {
	in := tv.NewMultiLineInput("", tv.Rect{})
	in.Component.HandlePaste("a\nb")
	in.Component.HandlePaste("c\nd")
	if n, _ := chipCount(in); n != 2 {
		t.Fatalf("two pastes produced %d chip runes, want 2", n)
	}
	got := in.GetText()
	if !strings.Contains(got, "a\nb") || !strings.Contains(got, "c\nd") {
		t.Fatalf("GetText() = %q did not expand both chips independently (want both \"a\\nb\" and \"c\\nd\")", got)
	}
}

// TestLoneChipIsNotAMention is the common-case regression guard for the @-file
// completer: a buffer that is just a chip is not parsed as an active @-mention, so
// the completer stays inactive. mentionToken (the live trigger) reads the RAW line,
// and a lone sentinel is neither '@' nor whitespace, so no '@' precedes the cursor.
func TestLoneChipIsNotAMention(t *testing.T) {
	in := tv.NewMultiLineInput("", tv.Rect{})
	in.Component.HandlePaste("a\nb")
	n, sentinel := chipCount(in)
	if n != 1 {
		t.Fatalf("setup: expected 1 chip, got %d", n)
	}
	line := []rune(in.Lines[in.CursorY])
	if len(line) != 1 || line[0] != sentinel {
		t.Fatalf("setup: chip line = %v, want a single sentinel rune", line)
	}
	if _, _, ok := mentionToken(line, in.CursorX); ok {
		t.Fatalf("a lone chip was parsed as an active @-mention (mentionToken ok=true)")
	}
}

// TestChipInsideMentionFoldsSentinel pins the one genuinely new, cosmetic behaviour
// #501 introduces in the prompt box (documented in design §3.1): a paste-chip
// landing INSIDE an active @-mention is not a boundary — mentionToken special-cases
// only '@' (opens) and whitespace (closes); every other rune, the sentinel included,
// is folded into the partial query. So "@fi⧉le" yields a query CONTAINING the
// sentinel, which matches no real path → the popup shows empty. This is popup-only
// (no crash, no corruption; accept() preserves the sentinel outside the replaced
// span and GetText still expands) and self-correcting. Asserting it here keeps the
// cosmetic-only nature pinned rather than leaving the edge unasserted.
func TestChipInsideMentionFoldsSentinel(t *testing.T) {
	src := tv.NewMultiLineInput("", tv.Rect{})
	src.Component.HandlePaste("x\ny")
	if _, sentinel := chipCount(src); !tv.IsPasteChipRune(sentinel) {
		t.Fatalf("setup: could not obtain a real chip sentinel rune")
	} else {
		// Build "@fi" + chip + "le" with the cursor at the end.
		line := []rune("@fi" + string(sentinel) + "le")
		start, query, ok := mentionToken(line, len(line))
		if !ok {
			t.Fatalf("mentionToken should still be inside a mention with a chip mid-token, got ok=false")
		}
		if start != 0 {
			t.Fatalf("mention start = %d, want 0 (the '@')", start)
		}
		if !strings.ContainsRune(query, sentinel) {
			t.Fatalf("query %q should contain the chip sentinel (chip is not a mention boundary)", query)
		}
	}
}

// TestChipAtStartIsNotSlashCommand covers slash detection both ways the prompt box
// reads it: (a) handleSlashCommand sees the EXPANDED GetText(), whose first rune is
// the pasted content's first rune — not '/' for ordinary text; and (b) the popup
// completer's slashMatches checks the RAW line's first rune, which is the sentinel —
// also not '/'. A buffer that is just a chip is therefore never a slash command.
func TestChipAtStartIsNotSlashCommand(t *testing.T) {
	in := tv.NewMultiLineInput("", tv.Rect{})
	in.Component.HandlePaste("not a command\nsecond line")

	// (a) submit-path slash detection on the expanded text.
	expanded := strings.TrimSpace(in.GetText())
	if strings.HasPrefix(expanded, "/") {
		t.Fatalf("expanded chip text %q must not be detected as a slash command", expanded)
	}
	// (b) popup-path slash detection on the raw first rune.
	first := []rune(in.Lines[0])[0]
	if first == '/' {
		t.Fatalf("raw first rune is '/', want the chip sentinel")
	}
	if !tv.IsPasteChipRune(first) {
		t.Fatalf("raw first rune is not a chip sentinel (%q)", first)
	}

	// Sanity: ordinary slash detection still works (no regression to the baseline).
	probe := tv.NewMultiLineInput("", tv.Rect{})
	probe.SetText("/stop")
	if got := strings.TrimSpace(probe.GetText()); !strings.HasPrefix(got, "/") {
		t.Fatalf("baseline slash detection broken: %q not detected as slash command", got)
	}
}

// TestParseMentionsOnExpandedGetText verifies mentions inside a pasted block still
// extract on submit: the submit path runs parseMentions over the EXPANDED GetText(),
// so an @-mention embedded in a multi-line paste resolves just as typed prose would.
func TestParseMentionsOnExpandedGetText(t *testing.T) {
	in := tv.NewMultiLineInput("", tv.Rect{})
	in.Component.HandlePaste("see @main.go and @internal/agent/agent.go\nfor details")
	mentions := parseMentions(in.GetText())
	want := []string{"main.go", "internal/agent/agent.go"}
	if len(mentions) != 2 || mentions[0] != want[0] || mentions[1] != want[1] {
		t.Fatalf("parseMentions(expanded GetText) = %v, want %v", mentions, want)
	}
}

// TestHistoryRecallRoundTripsContentFaithfully pins the history-recall path:
// prompts are stored via GetText() (expanded, newlines intact) and restored via
// SetText() — which splits on '\n' into editable lines and deliberately does NOT
// re-chip (turbotui keeps hand-typed multi-line recall editable). So a recalled
// multi-line prompt round-trips content-faithfully but is shown as editable lines,
// never corrupted. (gogent never calls SetTextChip — recall stays on SetText.)
func TestHistoryRecallRoundTripsContentFaithfully(t *testing.T) {
	in := tv.NewMultiLineInput("", tv.Rect{})
	const orig = "line one\nline two\nline three"
	in.Component.HandlePaste(orig) // store reads GetText() → expanded original
	stored := in.GetText()
	if stored != orig {
		t.Fatalf("stored (GetText) = %q, want %q", stored, orig)
	}

	// Recall restores via SetText (the path session_window.go:1477/1492/1496 uses).
	in.SetText(stored)
	if n, _ := chipCount(in); n != 0 {
		t.Fatalf("SetText recreated %d chips — recall must use literal SetText, not SetTextChip", n)
	}
	if got := in.GetText(); got != orig {
		t.Fatalf("recalled GetText() = %q, want round-tripped %q (no corruption)", got, orig)
	}
}

// TestPasteChipDrainEdgeInvariant verifies the inputs behind the typing-idle /
// deferred-modal drain trigger (session_window.go:383,395: the non-empty→empty edge
// `before != "" && input.GetText() == ""`). With a chip in the buffer GetText() is
// non-empty (the chip's expanded text), and — because a chip is exactly ONE rune, so
// a single backspace removes it atomically (turbotui's contract) — deleting it takes
// GetText() to empty, firing the edge exactly once. (Driving the backspace keystroke
// itself is turbotui's contract, covered by its TestPasteChip* suite; here we pin the
// gogent-relevant GetText() before/after values and the one-rune atomicity premise.)
func TestPasteChipDrainEdgeInvariant(t *testing.T) {
	in := tv.NewMultiLineInput("", tv.Rect{})
	in.Component.HandlePaste("a\nb")

	before := in.GetText()
	if before == "" {
		t.Fatalf("chip buffer GetText() is empty — drain 'before' would never be non-empty")
	}
	n, _ := chipCount(in)
	if n != 1 {
		t.Fatalf("expected exactly 1 chip rune (one backspace removes it), got %d", n)
	}
	// Simulate the post-delete state the drain observes: buffer emptied.
	in.Clear()
	if after := in.GetText(); after != "" {
		t.Fatalf("after emptying, GetText() = %q, want \"\" (drain edge expects empty)", after)
	}
	// Edge would fire: before != "" && after == "".
}
