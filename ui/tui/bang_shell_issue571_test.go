package ui

// Issue #571 — `!`-prefixed shell commands. These tests pin the four design
// criteria for the new out-of-band shell affordance:
//
//   (1) GOAL MATCH — a leading "!" routes to OnShell and runs the command; it is
//       never sent to the model (OnSend is not invoked) and never starts a turn.
//   (2) USABILITY — output is surfaced inline as a distinct system record; bare
//       "!" is a help no-op; the remote APIClient.Shell honours the caller's
//       context (NOT the 30s quickTimeout), matching the embedded 5-min ceiling.
//   (3) NO REGRESSIONS — "/cmd" and plain messages still take the model path;
//       shell records are display-only (kind kindSystem, never kindUser); the
//       OnShell dispatch is async so a slow command never blocks the UI thread.
//   (4) HOLISTIC — ui/tui stays exec-free (it only calls the OnShell handler);
//       ShellResult is a local DTO mirroring internal/shell.ExecuteResult.
//
// They reuse the in-package helpers recordSends/noSend/noteContains
// (input_queue_test.go) and drainPosted/drainPostedEventually
// (focus_deferral_issue346_348_test.go), which marshal the background OnShell
// goroutine's result back onto the (test) UI thread exactly as production does
// via Workbench.Post.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// bangLastRecord returns the most-recently-added transcript record, or nil.
func bangLastRecord(sw *SessionWindow) *transcriptRecord {
	rs := sw.transcript.records
	if len(rs) == 0 {
		return nil
	}
	return rs[len(rs)-1]
}

// lastRecordText joins the text of the last record's lines.
func lastRecordText(sw *SessionWindow) string {
	r := bangLastRecord(sw)
	if r == nil {
		return ""
	}
	return strings.Join(toTexts(r.lines), "\n")
}

// waitForOnShell wires an OnShell that records the command it received and
// signals arrival. Returns the command channel and a func to install/replace it.
func installRecordingOnShell(w *Workbench) (<-chan string, func(ShellResult, error)) {
	got := make(chan string, 4)
	w.handlers.OnShell = func(cmd string) (ShellResult, error) {
		select {
		case got <- cmd:
		default:
		}
		return ShellResult{Stdout: "ok"}, nil
	}
	return got, func(res ShellResult, err error) {
		// Replace the handler so the NEXT call returns the scripted result.
		w.handlers.OnShell = func(cmd string) (ShellResult, error) { return res, err }
	}
}

// recvCmd reads one OnShell command or fails after a short timeout.
func recvCmd(t *testing.T, got <-chan string) string {
	t.Helper()
	select {
	case c := <-got:
		return c
	case <-time.After(2 * time.Second):
		t.Fatal("OnShell was not called for a !-prefixed command")
		return ""
	}
}

// --- criterion (1) GOAL MATCH: routing + no model involvement ----------------

// TestBangCommandDispatchesToOnShell: typing "!ls -la" runs OnShell("ls -la")
// (leading "!" stripped, remainder preserved verbatim) and renders a transcript
// record — the core of the feature.
func TestBangCommandDispatchesToOnShell(t *testing.T) {
	w := newTestWorkbench(t)
	got, _ := installRecordingOnShell(w)
	sw := w.openWindow("s", "S")

	sw.input.SetText("!ls -la")
	sw.submitFn()

	if cmd := recvCmd(t, got); cmd != "ls -la" {
		t.Fatalf("OnShell command = %q, want %q", cmd, "ls -la")
	}
	drainPostedEventually(t, w) // execute wb.Post(addShellResult) on the UI thread

	r := bangLastRecord(sw)
	if r == nil {
		t.Fatal("expected a shell transcript record after !cmd")
	}
	if r.kind != kindSystem {
		t.Errorf("record kind = %v, want kindSystem (display-only)", r.kind)
	}
	if !strings.Contains(lastRecordText(sw), "ls -la") {
		t.Errorf("record does not echo the command: %q", lastRecordText(sw))
	}
	if !strings.Contains(lastRecordText(sw), "ok") {
		t.Errorf("record does not contain the command output: %q", lastRecordText(sw))
	}
}

// TestBangCommandNeverSentToModel: the defining invariant — OnSend is NOT
// invoked, no turn is started (busy stays false), for a !cmd.
func TestBangCommandNeverSentToModel(t *testing.T) {
	w := newTestWorkbench(t)
	sent := recordSends(w)
	got, _ := installRecordingOnShell(w)
	sw := w.openWindow("s", "S")

	sw.input.SetText("!echo hi")
	sw.submitFn()
	_ = recvCmd(t, got)
	drainPosted(t, w)

	noSend(t, sent) // criterion (1): nothing reached the model
	if sw.busy {
		t.Errorf("busy = true after !cmd; a shell command must not start a turn")
	}
}

// TestHandleBangCommandRoutingTable: only a leading "!" is a bang command;
// slash commands and plain text fall through (return false).
func TestHandleBangCommandRoutingTable(t *testing.T) {
	w := newTestWorkbench(t)
	_, _ = installRecordingOnShell(w)
	sw := w.openWindow("s", "S")

	for _, tc := range []struct {
		name string
		text string
		want bool
	}{
		{"bang", "!ls", true},
		{"bang with args", "!git status", true},
		{"bang then slash path still bang", "!/bin/ls", true},
		{"slash", "/undo", false},
		{"plain", "hello model", false},
		{"empty", "", false},
		{"exclaim mid-text is not a prefix", "wait!", false},
		{"hash is not bang", "#comment", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := sw.handleBangCommand(tc.text); got != tc.want {
				t.Errorf("handleBangCommand(%q) = %v, want %v", tc.text, got, tc.want)
			}
		})
	}
}

// TestPlainAndSlashPathsUnchanged: inserting the bang check must not disturb the
// existing input paths — a plain message still reaches the model (OnSend), and a
// slash command is still handled locally (not sent). Regression guard for the
// additive early-return placement (after recordHistory, before the busy block).
func TestPlainAndSlashPathsUnchanged(t *testing.T) {
	w := newTestWorkbench(t)
	sent := recordSends(w)
	_, _ = installRecordingOnShell(w)
	sw := w.openWindow("s", "S")

	// Plain message still goes to the model.
	sw.input.SetText("hello model")
	sw.submitFn()
	if got := waitSend(t, sent); got != "hello model" {
		t.Fatalf("plain message send = %q, want %q (the model path must be unchanged)", got, "hello model")
	}
	// Slash command is handled locally and never sent: /clearqueue produces a
	// "queued" note (either "no queued message to clear" or "queued message
	// cleared") and never reaches OnSend — proving the slash path is intact.
	sw.input.SetText("/clearqueue")
	sw.submitFn()
	noSend(t, sent)
	if !noteContains(sw, "queued") {
		t.Errorf("/clearqueue did not run locally (no 'queued' note); the slash path is broken")
	}
}

// TestBangCommandMultipleInFlight: because !cmd works while busy, several can run
// at once; each result lands as its own record, marshalled through Workbench.Post
// so they never race the transcript (criterion 3 concurrency). Each !cmd runs on
// its own goroutine and posts independently, so we poll-drain until every result
// has landed rather than assuming a single drain catches them all.
func TestBangCommandMultipleInFlight(t *testing.T) {
	w := newTestWorkbench(t)
	w.handlers.OnShell = func(cmd string) (ShellResult, error) {
		return ShellResult{Stdout: "out:" + cmd}, nil
	}
	sw := w.openWindow("s", "S")

	for _, c := range []string{"a", "b", "c", "d"} {
		sw.input.SetText("!" + c)
		sw.submitFn()
	}

	// Poll-drain the UI post queue until all four outputs have landed (or timeout).
	want := []string{"out:a", "out:b", "out:c", "out:d"}
	deadline := time.Now().Add(2 * time.Second)
	for {
		drainPosted(t, w)
		all := ""
		for _, r := range sw.transcript.records {
			all += strings.Join(toTexts(r.lines), "\n") + "\n"
		}
		ready := true
		for _, s := range want {
			if !strings.Contains(all, s) {
				ready = false
				break
			}
		}
		if ready {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for all in-flight !cmd results; transcript so far:\n%s", all)
		}
		time.Sleep(time.Millisecond)
	}

	// Every command's output must be present exactly once.
	all := ""
	for _, r := range sw.transcript.records {
		all += strings.Join(toTexts(r.lines), "\n") + "\n"
	}
	for _, c := range []string{"a", "b", "c", "d"} {
		if n := strings.Count(all, "out:"+c); n != 1 {
			t.Errorf("output for !%s appears %d times, want 1 (dropped or duplicated)", c, n)
		}
	}
}

// --- criterion (2) USABILITY + (3) NO REGRESSIONS: edges ---------------------

// TestBangCommandBareIsNoOp: a bare "!" (and "!" + whitespace) prints a usage
// note and executes nothing.
func TestBangCommandBareIsNoOp(t *testing.T) {
	for _, text := range []string{"!", "!   ", "!\t"} {
		t.Run("input="+text, func(t *testing.T) {
			w := newTestWorkbench(t)
			called := int32(0)
			w.handlers.OnShell = func(string) (ShellResult, error) {
				atomic.AddInt32(&called, 1)
				return ShellResult{}, nil
			}
			sw := w.openWindow("s", "S")

			if !sw.handleBangCommand(text) {
				t.Fatalf("handleBangCommand(%q) = false, want true (bare ! is handled)", text)
			}
			if atomic.LoadInt32(&called) != 0 {
				t.Fatalf("OnShell called %d times for a bare !; want 0", atomic.LoadInt32(&called))
			}
			if !noteContains(sw, "usage:") {
				t.Errorf("expected a usage note for bare !; last record = %q", lastRecordText(sw))
			}
		})
	}
}

// TestBangCommandUnavailableWithoutHandler: when OnShell is unwired (read-only
// analysis window) the feature is reported unavailable, not crashed.
func TestBangCommandUnavailableWithoutHandler(t *testing.T) {
	w := newTestWorkbench(t) // OnShell left nil
	sw := w.openWindow("s", "S")

	if !sw.handleBangCommand("!ls") {
		t.Fatal("handleBangCommand(!ls) = false, want true")
	}
	if !noteContains(sw, "unavailable") {
		t.Errorf("expected an 'unavailable' note; last record = %q", lastRecordText(sw))
	}
}

// TestBangCommandWorksWhileBusy: a !cmd while a turn is running is executed
// immediately — it is NOT queued in the pending slot (criterion: no turn state).
func TestBangCommandWorksWhileBusy(t *testing.T) {
	w := newTestWorkbench(t)
	got, _ := installRecordingOnShell(w)
	sw := w.openWindow("s", "S")
	sw.setBusy(true)

	sw.input.SetText("!ls")
	sw.submitFn()
	_ = recvCmd(t, got)
	drainPosted(t, w)

	if sw.pending != "" {
		t.Errorf("pending = %q after !cmd while busy; a !cmd must not be queued", sw.pending)
	}
}

// TestBangCommandDispatchIsAsync: submit returns without waiting for OnShell,
// so a slow command never freezes the UI thread (criterion 2).
func TestBangCommandDispatchIsAsync(t *testing.T) {
	w := newTestWorkbench(t)
	release := make(chan struct{})
	w.handlers.OnShell = func(string) (ShellResult, error) {
		<-release
		return ShellResult{Stdout: "slow"}, nil
	}
	sw := w.openWindow("s", "S")

	sw.input.SetText("!slow")
	done := make(chan struct{})
	go func() {
		sw.submitFn()
		close(done)
	}()
	select {
	case <-done:
		// good: submitFn returned without waiting for the blocking OnShell
	case <-time.After(2 * time.Second):
		t.Fatal("submitFn blocked on a slow OnShell — the dispatch must be on a background goroutine")
	}
	close(release)
	drainPostedEventually(t, w)
}

// TestBangCommandRecordedInHistory: like slash commands, a !cmd is recallable
// via Up/Down prompt history.
func TestBangCommandRecordedInHistory(t *testing.T) {
	w := newTestWorkbench(t)
	_, _ = installRecordingOnShell(w)
	sw := w.openWindow("s", "S")

	sw.input.SetText("!git status")
	sw.submitFn()
	drainPosted(t, w)

	if n := len(sw.promptHistory); n != 1 || sw.promptHistory[0] != "!git status" {
		t.Errorf("promptHistory = %v, want [!git status]", sw.promptHistory)
	}
}

// TestBangCommandStripsOnlyLeadingBang: leading whitespace after "!" is trimmed
// (so "!  ls" == "!ls"), but the rest of the command is preserved verbatim.
func TestBangCommandStripsOnlyLeadingBang(t *testing.T) {
	w := newTestWorkbench(t)
	got := make(chan string, 1)
	w.handlers.OnShell = func(cmd string) (ShellResult, error) {
		got <- cmd
		return ShellResult{}, nil
	}
	sw := w.openWindow("s", "S")

	sw.input.SetText("!   echo    spaced")
	sw.submitFn()
	if c := recvCmd(t, got); c != "echo    spaced" {
		t.Errorf("OnShell(%q): only the leading ! and surrounding whitespace may be stripped", c)
	}
	drainPosted(t, w)
}

// --- criterion (3) NO REGRESSIONS: addShellResult rendering invariants --------

// runAddShellResult calls addShellResult on a fresh window and returns its last
// record (or fails if addShellResult produced none).
func addShellResultRecord(t *testing.T, command string, res ShellResult, err error) *transcriptRecord {
	t.Helper()
	w := newTestWorkbench(t)
	sw := w.openWindow("s", "S")
	sw.addShellResult(command, res, err)
	r := bangLastRecord(sw)
	if r == nil {
		t.Fatal("addShellResult produced no record")
	}
	return r
}

// TestAddShellResultSuccessNoStatus: exit 0 with output renders a [Shell] system
// record carrying the output, with no "[exit ...]"/"[timed out]" annotation.
func TestAddShellResultSuccessNoStatus(t *testing.T) {
	r := addShellResultRecord(t, "ls", ShellResult{Stdout: "file1\nfile2\n", ExitCode: 0}, nil)
	if r.kind != kindSystem {
		t.Errorf("kind = %v, want kindSystem", r.kind)
	}
	if r.header != "[Shell]" {
		t.Errorf("header = %q, want [Shell]", r.header)
	}
	text := strings.Join(toTexts(r.lines), "\n")
	for _, want := range []string{"! ls", "file1", "file2"} {
		if !strings.Contains(text, want) {
			t.Errorf("record text missing %q: %q", want, text)
		}
	}
	if strings.Contains(text, "exit") || strings.Contains(text, "timed out") {
		t.Errorf("exit-0 success should not carry a status annotation: %q", text)
	}
}

// TestAddShellResultNonZeroExit: a non-zero exit is surfaced as "[exit N]".
func TestAddShellResultNonZeroExit(t *testing.T) {
	r := addShellResultRecord(t, "false", ShellResult{ExitCode: 2, Stderr: "boom"}, nil)
	text := strings.Join(toTexts(r.lines), "\n")
	if !strings.Contains(text, "[exit 2]") {
		t.Errorf("non-zero exit should annotate [exit 2]: %q", text)
	}
	if !strings.Contains(text, "boom") {
		t.Errorf("stderr should be rendered: %q", text)
	}
}

// TestAddShellResultTimeout: a timeout is surfaced as "[timed out]".
func TestAddShellResultTimeout(t *testing.T) {
	r := addShellResultRecord(t, "sleep 999", ShellResult{Timeout: true}, nil)
	text := strings.Join(toTexts(r.lines), "\n")
	if !strings.Contains(text, "[timed out]") {
		t.Errorf("timeout should annotate [timed out]: %q", text)
	}
}

// TestAddShellResultEmptyOutput: exit 0 with no output shows "[no output]" so
// the user always gets feedback (nothing is silent).
func TestAddShellResultEmptyOutput(t *testing.T) {
	r := addShellResultRecord(t, "true", ShellResult{ExitCode: 0}, nil)
	text := strings.Join(toTexts(r.lines), "\n")
	if !strings.Contains(text, "[no output]") {
		t.Errorf("empty exit-0 result should annotate [no output]: %q", text)
	}
}

// TestAddShellResultErrorIsNote: an execution/transport error is reported as a
// failure note and does NOT append a shell record (it cannot have output).
func TestAddShellResultErrorIsNote(t *testing.T) {
	w := newTestWorkbench(t)
	sw := w.openWindow("s", "S")
	sw.addShellResult("boom-cmd", ShellResult{}, errBangTest)
	if !noteContains(sw, "failed") {
		t.Errorf("expected a 'failed' note for an error; last = %q", lastRecordText(sw))
	}
	if r := bangLastRecord(sw); r != nil && r.header == "[Shell]" {
		t.Errorf("an error must not append a [Shell] output record; got %q", r.header)
	}
}

// sentinel error for the error-path test (avoids importing fmt just for errors.New).
var errBangTest = bangTestErr{}

type bangTestErr struct{}

func (bangTestErr) Error() string { return "transport failed" }

// TestShellRecordsAreNotUserKind: every addShellResult record is kindSystem, never
// kindUser — i.e. the shell invocation is never mistaken for a user/model message.
func TestShellRecordsAreNotUserKind(t *testing.T) {
	for _, res := range []ShellResult{
		{Stdout: "x", ExitCode: 0},
		{ExitCode: 1},
		{Timeout: true},
	} {
		r := addShellResultRecord(t, "c", res, nil)
		if r.kind == kindUser {
			t.Errorf("shell record kind = kindUser; must never enter the model conversation: %+v", res)
		}
	}
}

// --- criterion (2)/(4): APIClient.Shell — wire shape + context bound -----------

// TestAPIClientShellRoundTrip pins the daemon RPC: POST, /api/shell path, bearer
// token, JSON body {"command":...}, and symmetric decode into ShellResultDTO.
func TestAPIClientShellRoundTrip(t *testing.T) {
	var gotMethod, gotPath, gotAuth, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.EscapedPath()
		gotAuth = r.Header.Get("Authorization")
		buf := make([]byte, 4096)
		n, _ := r.Body.Read(buf)
		gotBody = string(buf[:n])
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"stdout":"hi\n","exit_code":0}`))
	}))
	defer srv.Close()

	client, err := NewAPIClient(srv.URL, "tok")
	if err != nil {
		t.Fatalf("NewAPIClient: %v", err)
	}
	out, err := client.Shell(context.Background(), "echo hi")
	if err != nil {
		t.Fatalf("Shell: %v", err)
	}
	if gotMethod != http.MethodPost {
		t.Errorf("method = %q, want POST", gotMethod)
	}
	if gotPath != "/api/shell" {
		t.Errorf("path = %q, want /api/shell", gotPath)
	}
	if gotAuth != "Bearer tok" {
		t.Errorf("auth = %q, want Bearer tok", gotAuth)
	}
	if !strings.Contains(gotBody, `"command":"echo hi"`) {
		t.Errorf("body = %q, want JSON {\"command\":\"echo hi\"}", gotBody)
	}
	if out.Stdout != "hi\n" {
		t.Errorf("Stdout = %q, want %q", out.Stdout, "hi\n")
	}
	if out.ExitCode != 0 {
		t.Errorf("ExitCode = %d, want 0", out.ExitCode)
	}
}

// TestAPIClientShellDecodesTimeoutAndExit: a daemon timeout / non-zero exit
// decode distinctly (Timeout + ExitCode), preserving the structured result.
func TestAPIClientShellDecodesTimeoutAndExit(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"stdout":"partial","stderr":"","exit_code":124,"timeout":true}`))
	}))
	defer srv.Close()

	client, err := NewAPIClient(srv.URL, "tok")
	if err != nil {
		t.Fatalf("NewAPIClient: %v", err)
	}
	out, err := client.Shell(context.Background(), "slow")
	if err != nil {
		t.Fatalf("Shell: %v", err)
	}
	if !out.Timeout {
		t.Errorf("Timeout = false, want true")
	}
	if out.ExitCode != 124 {
		t.Errorf("ExitCode = %d, want 124", out.ExitCode)
	}
	if out.Stdout != "partial" {
		t.Errorf("Stdout = %q, want partial", out.Stdout)
	}
}

// TestAPIClientShellRespectsCallerContext is the regression guard for the
// design's key fix: Shell must NOT use c.do() (which hard-caps every call at
// quickTimeout = 30s via an internal context.Background()). It must honour the
// caller's ctx — like SendMessage — so a remote !cmd can run up to the daemon's
// 5-minute shell timeout. A pre-cancelled ctx therefore must abort the call
// immediately; if Shell were routed through do(), the caller ctx would be ignored
// and the request would complete against the stub (err == nil), failing this.
func TestAPIClientShellRespectsCallerContext(t *testing.T) {
	hits := int32(0)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"stdout":"x"}`))
	}))
	defer srv.Close()

	client, err := NewAPIClient(srv.URL, "tok")
	if err != nil {
		t.Fatalf("NewAPIClient: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled before the call

	start := time.Now()
	_, err = client.Shell(ctx, "echo hi")
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("Shell with a cancelled ctx returned nil error; it is ignoring the caller ctx " +
			"(likely reverted to c.do()/quickTimeout) — remote !cmd would be wrongly capped at 30s")
	}
	if elapsed > time.Second {
		t.Errorf("Shell took %v on a cancelled ctx; it should abort immediately", elapsed)
	}
	// The transport may or may not have reached the server before cancellation;
	// either is acceptable. The contract under test is the fast ctx-respecting abort.
}

// TestAPIClientShellNon2xxIsError: a daemon error (e.g. 400/500) surfaces as a
// Go error carrying the status, not a silent zero-value result.
func TestAPIClientShellNon2xxIsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("boom"))
	}))
	defer srv.Close()

	client, err := NewAPIClient(srv.URL, "tok")
	if err != nil {
		t.Fatalf("NewAPIClient: %v", err)
	}
	if _, err := client.Shell(context.Background(), "x"); err == nil {
		t.Fatal("Shell on a 500 returned nil error; a non-2xx must surface as an error")
	}
}

// TestRemoteClientOnShellMapsDTO: the attached TUI's OnShell handler maps the
// daemon's ShellResultDTO into the UI ShellResult and threads errors through.
func TestRemoteClientOnShellMapsDTO(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"stdout":"hi\n","stderr":"","exit_code":0}`))
	}))
	defer srv.Close()

	client, err := NewAPIClient(srv.URL, "tok")
	if err != nil {
		t.Fatalf("NewAPIClient: %v", err)
	}
	w := newTestWorkbench(t)
	rc := NewRemoteClient(client, w.EmitSessionEvent, w)
	onShell := rc.Handlers().OnShell
	if onShell == nil {
		t.Fatal("RemoteClient.Handlers().OnShell is nil; the attached TUI must wire OnShell")
	}
	res, err := onShell("echo hi")
	if err != nil {
		t.Fatalf("OnShell: %v", err)
	}
	if res.Stdout != "hi\n" {
		t.Errorf("ShellResult.Stdout = %q, want %q", res.Stdout, "hi\n")
	}
}

// TestRemoteClientOnShellSurfacesTransportError: a daemon transport failure is
// returned as the OnShell error (not swallowed into a zero-value ShellResult).
func TestRemoteClientOnShellSurfacesTransportError(t *testing.T) {
	// A server that returns 503 so Shell() yields a non-2xx error.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	client, err := NewAPIClient(srv.URL, "tok")
	if err != nil {
		t.Fatalf("NewAPIClient: %v", err)
	}
	w := newTestWorkbench(t)
	rc := NewRemoteClient(client, w.EmitSessionEvent, w)
	if _, err := rc.Handlers().OnShell("echo hi"); err == nil {
		t.Fatal("OnShell returned nil for a daemon transport failure; the error must surface")
	}
}
