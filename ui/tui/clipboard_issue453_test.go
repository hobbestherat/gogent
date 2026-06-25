package ui

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"strings"
	"sync"
	"testing"

	tui "github.com/hobbestherat/turbotui"
	tv "github.com/hobbestherat/turbotui/turbotv"
)

// These tests pin issue #453's fix: the yank path (the 'y' key, "Copy Last
// Answer", "Copy Last Code Block") must reach the clipboard through turbotui's
// App.CopyToClipboard — the single writer that serializes its OSC 52 escape under
// the same lock (App.writeMu) that guards Apply() frame flushes — instead of the
// deleted internal/clipboard.Board, which wrote to os.Stdout under a separate lock
// and could interleave byte-for-byte with rendering, corrupting the escape stream.

// newBufferWorkbench builds a Workbench whose turbotui App writes OSC 52 (and only
// OSC 52 — ClipboardOSC52Only, so no native clipboard utility is shelled out) into
// buf. This lets yank tests observe exactly what reached the "clipboard" through
// the real production path (copyToClipboard -> w.app.CopyToClipboard) without a real
// terminal or host clipboard. It reassigns the app/desktop after NewWorkbench
// (mirroring the established sidebar_issue379_test.go pattern) and must run before
// openWindow, so the window is built against the buffer-backed app.
func newBufferWorkbench(t *testing.T, buf *bytes.Buffer) *Workbench {
	t.Helper()
	w := newTestWorkbench(t)
	w.app = tui.NewWithSize(80, 25, buf)
	w.app.SetClipboardBackend(tui.ClipboardOSC52Only)
	w.desktop = tv.NewDesktop(w.app)
	return w
}

// previewForLog trims a string for safe inclusion in a test failure message.
func previewForLog(s string) string {
	if len(s) > 60 {
		return s[:60] + "..."
	}
	return s
}

// extractAllOSC52 decodes every complete OSC 52 clipboard-set sequence
// (ESC ] 52 ; c ; <base64> BEL) in s and returns the payloads. It fatals on a
// sequence that is missing its BEL terminator or whose payload is not valid base64 —
// either is a signature of the escape stream being corrupted by interleaved writes,
// which is exactly the issue #453 failure mode.
func extractAllOSC52(t *testing.T, s string) []string {
	t.Helper()
	const prefix = "\x1b]52;c;"
	var out []string
	for {
		i := strings.Index(s, prefix)
		if i < 0 {
			break
		}
		s = s[i+len(prefix):]
		j := strings.IndexByte(s, '\x07')
		if j < 0 {
			t.Fatalf("OSC 52 sequence missing BEL terminator (stream corruption): %q", previewForLog(s))
		}
		dec, err := base64.StdEncoding.DecodeString(s[:j])
		if err != nil {
			t.Fatalf("OSC 52 payload is not valid base64 (stream corruption): %v payload=%q", err, previewForLog(s[:j]))
		}
		out = append(out, string(dec))
		s = s[j+1:]
	}
	return out
}

// --- Criterion (1): GOAL MATCH — the yank path routes through App.CopyToClipboard
// and internal/clipboard.Board is gone. ---

// TestCopyToClipboardRoutesThroughApp is the central #453 assertion: the yank path
// reaches the clipboard through turbotui's App.CopyToClipboard (observed via a
// buffer-backed App), not through the deleted internal/clipboard.Board. If the swap
// had not happened, the buffer would stay empty (Board is removed) and this fails.
func TestCopyToClipboardRoutesThroughApp(t *testing.T) {
	var buf bytes.Buffer
	w := newBufferWorkbench(t, &buf)

	w.copyToClipboard("hello-clipboard")

	got := decodeOSC52(t, buf.String())
	if got != "hello-clipboard" {
		t.Fatalf("copyToClipboard routed %q to the clipboard, want %q", got, "hello-clipboard")
	}
}

// TestBothYankActionsReachApp confirms both yank entry points (answer and code) flow
// through the same App.CopyToClipboard writer by observing distinct payloads in the
// buffer for each.
func TestBothYankActionsReachApp(t *testing.T) {
	var buf bytes.Buffer
	w := newBufferWorkbench(t, &buf)
	sw := w.openWindow("s", "S")

	sw.addAssistant("the answer is:\n```go\nfmt.Println(\"hi\")\n```")

	buf.Reset()
	sw.copyLastAnswer()
	if got := decodeOSC52(t, buf.String()); !strings.Contains(got, "the answer is") {
		t.Errorf("copyLastAnswer copied %q, want the answer text", got)
	}

	buf.Reset()
	sw.copyLastCode()
	if got := decodeOSC52(t, buf.String()); got != `fmt.Println("hi")` {
		t.Errorf("copyLastCode copied %q, want the fenced code", got)
	}
}

// --- Criterion (2): USABILITY — correct user-facing behavior and surfacing. ---

// TestCopyLastAnswerNoAnswerYet verifies a window with no assistant answer surfaces
// "no answer to copy yet" and writes nothing to the clipboard (rather than silently
// copying stale/empty content).
func TestCopyLastAnswerNoAnswerYet(t *testing.T) {
	var buf bytes.Buffer
	w := newBufferWorkbench(t, &buf)
	sw := w.openWindow("s", "S")

	sw.addUser("only a user message") // no assistant answer yet
	sw.copyLastAnswer()

	if buf.Len() != 0 {
		t.Errorf("expected no clipboard write with no answer, got %q", buf.String())
	}
	last := sw.transcript.records[len(sw.transcript.records)-1]
	if joined := strings.Join(toTexts(last.lines), " "); !strings.Contains(joined, "no answer to copy yet") {
		t.Errorf("expected a 'no answer to copy yet' note, got %q", joined)
	}
}

// TestCopyLastCodeNoAssistantRecord verifies copyLastCode on a transcript with no
// assistant record reports "no code block" instead of panicking (the nil-receiver
// path through lastAssistantRecord().body()).
func TestCopyLastCodeNoAssistantRecord(t *testing.T) {
	var buf bytes.Buffer
	w := newBufferWorkbench(t, &buf)
	sw := w.openWindow("s", "S")

	sw.copyLastCode() // no records at all

	if buf.Len() != 0 {
		t.Errorf("expected no clipboard write, got %q", buf.String())
	}
	last := sw.transcript.records[len(sw.transcript.records)-1]
	if joined := strings.Join(toTexts(last.lines), " "); !strings.Contains(joined, "no code block") {
		t.Errorf("expected a 'no code block' note, got %q", joined)
	}
}

// TestCopyLastCodeMultipleFencedBlocks verifies copyLastCode copies every fenced
// block in the last answer (extractFencedCode joins blocks with a blank line).
func TestCopyLastCodeMultipleFencedBlocks(t *testing.T) {
	var buf bytes.Buffer
	w := newBufferWorkbench(t, &buf)
	sw := w.openWindow("s", "S")

	sw.addAssistant("intro\n```go\nfoo()\n```\nmid\n```py\nbar()\n```")
	buf.Reset()
	sw.copyLastCode()

	got := decodeOSC52(t, buf.String())
	if !strings.Contains(got, "foo()") || !strings.Contains(got, "bar()") {
		t.Errorf("expected both fenced blocks copied, got %q", got)
	}
}

// TestCopyLastAnswerConfirmationNoteRuneCount checks the confirmation note reports a
// rune count (utf8.RuneCountInString), not a byte count, so multibyte answers show
// an accurate length. "héllo🙂" is 6 runes (10 bytes).
func TestCopyLastAnswerConfirmationNoteRuneCount(t *testing.T) {
	var buf bytes.Buffer
	w := newBufferWorkbench(t, &buf)
	sw := w.openWindow("s", "S")

	const answer = "héllo🙂" // 6 runes, 10 bytes
	sw.addAssistant(answer)
	buf.Reset()
	sw.copyLastAnswer()

	if got := decodeOSC52(t, buf.String()); got != answer {
		t.Fatalf("copied %q, want %q", got, answer)
	}
	last := sw.transcript.records[len(sw.transcript.records)-1]
	joined := strings.Join(toTexts(last.lines), " ")
	// 6 is the rune count; a byte-count regression would print a different number.
	if !strings.Contains(joined, "(6 chars)") {
		t.Errorf("expected rune-count note '(6 chars)', got %q", joined)
	}
}

// TestCopyLastAnswerSkipsThoughts verifies copyLastAnswer copies the assistant answer
// and skips interleaved thinking records (lastAssistantRecord anchors on kindAssistant).
func TestCopyLastAnswerSkipsThoughts(t *testing.T) {
	var buf bytes.Buffer
	w := newBufferWorkbench(t, &buf)
	sw := w.openWindow("s", "S")

	sw.addAssistant("first answer")
	sw.addThought("a private thought that must NOT be copied")
	sw.addUser("again")
	sw.addAssistant("the real answer")

	buf.Reset()
	sw.copyLastAnswer()
	got := decodeOSC52(t, buf.String())
	if got != "the real answer" {
		t.Errorf("copyLastAnswer copied %q, want only the last assistant answer (thoughts skipped)", got)
	}
}

// TestCopyToClipboardOverSSHStillUsesOSC52 verifies the user-facing SSH guarantee:
// when gogent appears to run over SSH, the yank path still sets the LOCAL clipboard
// via OSC 52. turbotui skips the native fallback over SSH (it would target the
// remote host), so setting SSH_* here also keeps the test from shelling out. We use
// the production default backend (OSC52AndNative) to exercise the real policy.
func TestCopyToClipboardOverSSHStillUsesOSC52(t *testing.T) {
	var buf bytes.Buffer
	w := newTestWorkbench(t)
	w.app = tui.NewWithSize(80, 25, &buf) // default backend: ClipboardOSC52AndNative
	w.desktop = tv.NewDesktop(w.app)

	t.Setenv("SSH_CONNECTION", "1.2.3.4 22 5.6.7.8 22")
	w.copyToClipboard("over-ssh-payload")

	got := decodeOSC52(t, buf.String())
	if got != "over-ssh-payload" {
		t.Fatalf("over SSH, expected OSC 52 to set the local clipboard to %q, got %q", "over-ssh-payload", got)
	}
}

// --- Criterion (3): NO REGRESSIONS — nil-safety, edge cases, size-cap behavior. ---

// TestCopyToClipboardNilAppIsNoOp preserves the old "no-op when no board configured"
// contract: with no App wired up, copyToClipboard must not panic and must write nothing.
func TestCopyToClipboardNilAppIsNoOp(t *testing.T) {
	w := newTestWorkbench(t)
	w.app = nil

	// Must not panic and must write nothing.
	w.copyToClipboard("anything")
}

// TestCopyToClipboardOversizeSkipsOSC52 characterizes the size cap the design flagged
// as a narrow regression: turbotui skips the OSC 52 write once the base64 exceeds 1 MiB
// (~768 KiB of raw text). With ClipboardOSC52Only there is no native fallback, so an
// oversize copy writes nothing — the SSH + very-large-answer edge. This pins the
// behavior so a future change to the cap is deliberate.
func TestCopyToClipboardOversizeSkipsOSC52(t *testing.T) {
	var buf bytes.Buffer
	w := newBufferWorkbench(t, &buf)

	// 1 MiB of raw text encodes to ~1.33 MiB of base64 — over the 1 MiB cap.
	big := strings.Repeat("x", 1024*1024)
	w.copyToClipboard(big)
	if buf.Len() != 0 {
		t.Errorf("expected OSC 52 to be skipped for an over-cap payload, but wrote %d bytes", buf.Len())
	}

	// A comfortably sub-cap payload still copies intact.
	buf.Reset()
	ok := strings.Repeat("y", 100*1024)
	w.copyToClipboard(ok)
	if got := decodeOSC52(t, buf.String()); got != ok {
		t.Errorf("expected the sub-cap payload to copy intact, got %d bytes decoded", len(got))
	}
}

// --- Criterion (3)/(4): the root-cause fix — OSC 52 writes serialize with frame
// flushes under App.writeMu, so no byte-level interleaving corruption. ---

// TestCopyToClipboardSerializesAgainstApplyFrames verifies the root-cause fix: the
// yank path's OSC 52 write and turbotui's Apply() frame flush now share App.writeMu,
// so concurrent copies and frame flushes cannot interleave byte-for-byte and corrupt
// the escape stream. We fire many copyToClipboard calls from separate goroutines
// while a single goroutine drives full repaints (Apply) into the same buffer, then
// assert every OSC 52 sequence survived intact. CopyToClipboard touches only a.out
// (under writeMu), so the concurrent copies do not race Apply's cell buffers — only
// the shared output is involved, which writeMu serializes.
func TestCopyToClipboardSerializesAgainstApplyFrames(t *testing.T) {
	var buf bytes.Buffer
	w := newBufferWorkbench(t, &buf)

	const copies = 256
	payloads := make([]string, copies)
	for i := range payloads {
		payloads[i] = fmt.Sprintf("yank-%d-%d", i, i*1000003+7)
	}

	var wg sync.WaitGroup
	wg.Add(copies)
	for i := 0; i < copies; i++ {
		i := i
		go func() {
			defer wg.Done()
			w.copyToClipboard(payloads[i])
		}()
	}
	// One goroutine drives frame flushes; Apply is not safe to call from multiple
	// goroutines at once (its cell-buffer diff runs outside writeMu), so keep it to a
	// single flusher and let the copies overlap it.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for k := 0; k < copies; k++ {
			w.app.Invalidate() // force a full repaint so Apply writes real frame bytes
			_ = w.app.Apply()
		}
	}()
	wg.Wait()

	got := extractAllOSC52(t, buf.String())
	if len(got) != copies {
		t.Fatalf("expected %d intact OSC 52 sequences, got %d — the stream was corrupted by interleaved Apply frames", copies, len(got))
	}
	want := make(map[string]bool, copies)
	for _, p := range payloads {
		want[p] = true
	}
	for _, g := range got {
		if !want[g] {
			t.Errorf("decoded a corrupted/unexpected OSC 52 payload %q", g)
		}
	}
}
