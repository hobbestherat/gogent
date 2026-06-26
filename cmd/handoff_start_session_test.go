package main

import (
	"bytes"
	"context"
	"encoding/json"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gogent/internal/gogent"
	"gogent/internal/server"
	tuipkg "gogent/ui/tui"
)

// Regression coverage for issue #476: the embedded->daemon ("Start daemon")
// handoff must leave EVERY open window with a live backend session on the daemon,
// so the first send into a fresh, never-messaged window runs the turn instead of
// 404ing. These tests drive createDaemonWindowSessions — the remote-side mirror of
// Stop()'s bindWindowSession loop — against a real /api daemon built the same way
// the handoff lands on one.
//
// Design criteria under test:
//   (1) goal — fresh window's session exists on the daemon (no 404), turn runs.
//   (2) usability/regression — already-conversed (restored) windows survive; a
//       per-window create failure degrades instead of aborting the handoff.
//   (3) no regressions — idempotent creates, empty-workbench no-op, no panics.
//   (4) holistic — the "every open window has a live backend session" invariant
//       (symmetric with Stop/bindWindowSession); backend-only "default"/"watcher:"
//       ids are filtered; the public SessionTitle accessor is exercised.

// newHandoffTestServer stands up a fresh daemon backend the way an
// embedded->daemon handoff arrives at one: a live /api server (loopback, so the
// credential-less APIClient is human-scoped) over a fresh core. The returned core
// IS the daemon's in-memory state, so tests assert directly via GetUserSession as
// well as through the HTTP client.
func newHandoffTestServer(t *testing.T) (*gogent.Gogent, *httptest.Server, *tuipkg.APIClient) {
	t.Helper()
	g := gogent.NewGogent(t.TempDir())
	srv := server.NewServer(g, server.Options{})
	httpSrv := httptest.NewServer(srv.Handler())
	t.Cleanup(httpSrv.Close)
	client, err := tuipkg.NewAPIClient(httpSrv.URL, "")
	if err != nil {
		t.Fatalf("build api client: %v", err)
	}
	return g, httpSrv, client
}

// newHandoffWorkbench builds a headless workbench with no backend handlers wired,
// exactly the orphaned-window state: NewSession opens a window and fires OnCreate
// ONLY when a handler is set, so these windows have no backend session behind them
// — the precise scenario the daemon cannot reconstruct from disk.
func newHandoffWorkbench(t *testing.T) *tuipkg.Workbench {
	t.Helper()
	return tuipkg.NewWorkbench(nil)
}

// stubChatCompletions is a minimal OpenAI-style backend that always replies with a
// final assistant message (no tool calls), so a blocking send resolves in one
// round-trip. Set GOGENT_MODEL_URL to srv.URL+"/chat/completions" BEFORE building
// the core so its default connection points here.
func stubChatCompletions(t *testing.T, reply string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{
				"index":         0,
				"message":       map[string]any{"role": "assistant", "content": reply},
				"finish_reason": "stop",
			}},
			"usage": map[string]any{"prompt_tokens": 5, "completion_tokens": 3, "total_tokens": 8},
		})
	}))
	t.Cleanup(srv.Close)
	return srv
}

// awaitAssistantAnswer polls the session transcript until an assistant message
// containing needle appears — i.e. the async-dispatched turn completed (issue
// #481: Send no longer blocks until the turn finishes, so a test that needs the
// turn's result must wait for it explicitly). Returns whether it arrived.
func awaitAssistantAnswer(t *testing.T, client *tuipkg.APIClient, sessionID, needle string, timeout time.Duration) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		msgs, err := client.GetTranscript(sessionID, "root")
		if err == nil {
			for _, m := range msgs {
				if m.Role == "assistant" && strings.Contains(m.Content, needle) {
					return true
				}
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}

// TestStartHandoffCreatesFreshWindowSessionOnDaemon is the core regression: a
// fresh, never-messaged window (no backend session, never persisted) must gain a
// live session on the daemon once the handoff's per-window create runs, so its
// next send resolves instead of 404ing. (criteria 1, 4)
func TestStartHandoffCreatesFreshWindowSessionOnDaemon(t *testing.T) {
	g, _, client := newHandoffTestServer(t)
	wb := newHandoffWorkbench(t)
	sw := wb.NewSession() // session-1, "Session 1", no backend (no OnCreate wired)
	if sw == nil {
		t.Fatal("NewSession returned nil window")
	}

	// Precondition: the daemon has no record of the fresh window (the bug state).
	if us := g.GetUserSession("session-1"); us != nil {
		t.Fatalf("precondition: daemon already has session-1 before handoff")
	}

	createDaemonWindowSessions(client, wb)

	// Direct backend proof: the session now lives on the daemon core.
	if us := g.GetUserSession("session-1"); us == nil {
		t.Fatalf("after handoff: daemon has no live session for session-1 (would 404 on send)")
	}
	// HTTP proof: GET /sessions/:id resolves to it (the 404 path is now unreachable
	// — Send's GetUserSession nil-check at internal/server/messages.go:22 cannot fire).
	dto, err := client.GetSession("session-1")
	if err != nil {
		t.Fatalf("GetSession(session-1) after handoff: %v", err)
	}
	if dto.ID != "session-1" {
		t.Errorf("GetSession id = %q, want session-1 (window id and daemon id must stay in lock-step)", dto.ID)
	}
	if !dto.Live {
		t.Errorf("GetSession live = false, want true (the freshly-created session is live)")
	}
	// NOTE: dto.Title / dto.Persisted are NOT asserted here. The daemon's Get view
	// derives them from the on-disk index (titleFor/isEphemeral both scan
	// ListSessions), and a freshly-created session is only written to disk at the
	// end of its first completed turn (persistSession, gogent.go:2604) — the very
	// root cause of #476. So a zero-turn session legitimately reports title=""/
	// persisted=false via Get even though Create stored the title on the live
	// session and made it durable. Title preservation is asserted end-to-end (via
	// a completed turn) in TestStartHandoffPreservesWindowTitleAcrossHandoff.
}

// TestStartHandoffFreshWindowSendRunsTurnNo404 is the literal acceptance check: the
// message "404: session not found" reproduction, inverted. With the per-window
// create done, a real turn runs on the daemon against a stubbed model and returns a
// final answer — not a 404. (criterion 1)
func TestStartHandoffFreshWindowSendRunsTurnNo404(t *testing.T) {
	backend := stubChatCompletions(t, "hello from the daemon")
	t.Setenv("GOGENT_MODEL_URL", backend.URL+"/chat/completions")
	g := gogent.NewGogent(t.TempDir())
	srv := server.NewServer(g, server.Options{})
	httpSrv := httptest.NewServer(srv.Handler())
	t.Cleanup(httpSrv.Close)
	client, err := tuipkg.NewAPIClient(httpSrv.URL, "")
	if err != nil {
		t.Fatalf("build api client: %v", err)
	}

	wb := newHandoffWorkbench(t)
	wb.NewSession() // fresh, never-messaged window — the orphaned case
	createDaemonWindowSessions(client, wb)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	dto, err := client.SendMessage(ctx, "session-1", "hello", "", "")
	if err != nil {
		// The bug manifests as exactly this error string; fail loudly if it returns.
		if strings.Contains(err.Error(), "404") || strings.Contains(err.Error(), "session not found") {
			t.Fatalf("fresh window send 404'd after handoff (the bug): %v", err)
		}
		t.Fatalf("fresh window send returned an unexpected error after handoff: %v", err)
	}
	// Issue #481: Send is non-blocking and returns the dispatched turn id — the
	// answer no longer rides in the response body (it arrives over SSE). Confirm
	// the turn actually ran on the daemon by waiting for the assistant answer.
	if dto.TurnID == "" {
		t.Fatal("fresh window send returned no turn id (dispatch did not start)")
	}
	if !awaitAssistantAnswer(t, client, "session-1", "hello from the daemon", 10*time.Second) {
		t.Fatal("fresh window send: the turn did not run on the daemon (no assistant answer in transcript)")
	}
}

// TestStartHandoffPreservesWindowTitleAcrossHandoff asserts the window title is
// preserved on the daemon after the handoff. The helper passes wb.SessionTitle(id)
// into CreateSession, whose handler stores it on the live session via SetSessionTitle;
// it becomes observable through Get (whose title is index-derived) once the first
// turn persists it. So this completes one turn against a stubbed model and then
// reads the title back — a faithful end-to-end check of the public-accessor path.
// (criterion 1 / acceptance; criterion 4 — accessor)
func TestStartHandoffPreservesWindowTitleAcrossHandoff(t *testing.T) {
	backend := stubChatCompletions(t, "ok")
	t.Setenv("GOGENT_MODEL_URL", backend.URL+"/chat/completions")
	g := gogent.NewGogent(t.TempDir())
	srv := server.NewServer(g, server.Options{})
	httpSrv := httptest.NewServer(srv.Handler())
	t.Cleanup(httpSrv.Close)
	client, err := tuipkg.NewAPIClient(httpSrv.URL, "")
	if err != nil {
		t.Fatalf("build api client: %v", err)
	}

	wb := newHandoffWorkbench(t)
	// A window adopted from a prior conversation can carry an arbitrary id+title;
	// this is the path that reproduces a restored window with a custom name.
	const wantTitle = "Quarterly plan"
	wb.AdoptSession(tuipkg.RestoredSession{ID: "session-7", Title: wantTitle})
	if got := wb.SessionTitle("session-7"); got != wantTitle {
		t.Fatalf("precondition: SessionTitle(session-7) = %q, want %q", got, wantTitle)
	}

	createDaemonWindowSessions(client, wb)

	// Complete one turn so persistSession writes the title the helper carried onto
	// the daemon's disk index (the only way Get can observe it for a fresh session).
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if _, err := client.SendMessage(ctx, "session-7", "hi", "", ""); err != nil {
		t.Fatalf("seed turn to persist title: %v", err)
	}
	// Issue #481: the seed turn runs asynchronously, so wait for it to complete
	// (and call persistSession, which writes the carried title to the index) before
	// reading the title back.
	if !awaitAssistantAnswer(t, client, "session-7", "ok", 10*time.Second) {
		t.Fatal("seed turn to persist the title did not complete on the daemon")
	}

	dto, err := client.GetSession("session-7")
	if err != nil {
		t.Fatalf("GetSession(session-7): %v", err)
	}
	if dto.Title != wantTitle {
		t.Errorf("daemon session title = %q, want %q (custom window title not preserved across handoff)", dto.Title, wantTitle)
	}
}

// TestSessionTitleAccessor pins the public accessor added for the handoff (the
// single-source-of-truth delegate of sessionTitle). It must return the window
// title for an open id and "" for an unknown one. (criterion 4 — API gap)
func TestSessionTitleAccessor(t *testing.T) {
	wb := newHandoffWorkbench(t)
	wb.NewSession() // session-1
	wb.NewSession() // session-2

	if got := wb.SessionTitle("session-1"); got != "Session 1" {
		t.Errorf("SessionTitle(session-1) = %q, want %q", got, "Session 1")
	}
	if got := wb.SessionTitle("session-2"); got != "Session 2" {
		t.Errorf("SessionTitle(session-2) = %q, want %q", got, "Session 2")
	}
	if got := wb.SessionTitle("does-not-exist"); got != "" {
		t.Errorf("SessionTitle(unknown) = %q, want empty", got)
	}
}

// TestStartHandoffIdempotentOverRestoredSession is the regression guard: a window
// whose session ALREADY existed on the daemon (restored from disk / already
// conversed) must still work after the handoff — the idempotent create neither
// duplicates nor breaks it. Running the create twice must also be safe. (criterion 2)
func TestStartHandoffIdempotentOverRestoredSession(t *testing.T) {
	g, _, client := newHandoffTestServer(t)
	wb := newHandoffWorkbench(t)
	wb.NewSession() // session-1 (fresh)
	wb.NewSession() // session-2 (will simulate a restored, already-conversed session)

	// Simulate session-2 having been restored from disk by the daemon: a live
	// backend session already exists under that id before the handoff create runs.
	pre := g.NewSession("session-2")
	if pre == nil {
		t.Fatal("precondition: NewSession(session-2) returned nil")
	}

	createDaemonWindowSessions(client, wb)

	// The restored session is still present and addressable (not broken).
	if us := g.GetUserSession("session-2"); us == nil {
		t.Fatalf("restored session-2 missing after idempotent create")
	}
	if _, err := client.GetSession("session-2"); err != nil {
		t.Fatalf("GetSession(session-2) after idempotent create: %v", err)
	}
	// And the fresh window was created alongside it.
	if g.GetUserSession("session-1") == nil {
		t.Fatalf("fresh session-1 missing alongside the restored one")
	}

	// Idempotency: a second pass over the same windows must be harmless — no error,
	// no duplication (session ids are unique keys in the core's session map).
	createDaemonWindowSessions(client, wb)
	seen := map[string]int{}
	for _, id := range g.SessionIDs() {
		seen[id]++
	}
	for id, n := range seen {
		if n != 1 {
			t.Errorf("session %s counted %d times after double create; idempotency broken", id, n)
		}
	}
	if _, err := client.GetSession("session-2"); err != nil {
		t.Fatalf("GetSession(session-2) after second create: %v", err)
	}
}

// TestStartHandoffCreatesEveryOpenWindowSession asserts the holistic invariant the
// fix exists to uphold — symmetric with Stop()/bindWindowSession: after the
// embedded->daemon handoff, EVERY open (user-facing) window has a live backend
// session on the daemon. (criterion 4)
func TestStartHandoffCreatesEveryOpenWindowSession(t *testing.T) {
	g, _, client := newHandoffTestServer(t)
	wb := newHandoffWorkbench(t)
	const n = 3
	for i := 0; i < n; i++ {
		wb.NewSession()
	}

	createDaemonWindowSessions(client, wb)

	// Invariant: each window id the helper considered now resolves on the daemon.
	for _, id := range wb.SessionIDs() {
		if id == "default" || strings.HasPrefix(id, "watcher:") {
			continue // backend-only ids are deliberately excluded (see filter test)
		}
		if us := g.GetUserSession(id); us == nil {
			t.Errorf("open window %q has no live backend session after handoff (invariant broken)", id)
		}
	}
	// No stray backend-only sessions were created as a side effect.
	if g.GetUserSession("default") != nil {
		t.Errorf("a 'default' session was created by the handoff; backend-only ids must be excluded")
	}
}

// TestStartHandoffSkipsBackendOnlyDefaultAndWatcherIds directly exercises the
// backend-only filter by injecting live windows under reserved ids via AdoptSession
// (which opens a non-read-only window with a caller-chosen id). The helper must
// skip "default" and any "watcher:"-prefixed id even though SessionIDs() lists them,
// while still creating the genuine user window. (criterion 1 — filter)
func TestStartHandoffSkipsBackendOnlyDefaultAndWatcherIds(t *testing.T) {
	g, _, client := newHandoffTestServer(t)
	wb := newHandoffWorkbench(t)
	wb.NewSession() // session-1 — a genuine user window
	// Reserved-id live windows: these would be created as user sessions by a naive
	// loop; the filter must exclude them.
	wb.AdoptSession(tuipkg.RestoredSession{ID: "default", Title: "sneaky default"})
	wb.AdoptSession(tuipkg.RestoredSession{ID: "watcher:daily", Title: "sneaky watcher"})

	// Confirm the injected reserved ids are actually in the window set the helper
	// iterates — otherwise this would not be exercising the filter at all.
	ids := wb.SessionIDs()
	mustContain := func(want string) {
		for _, id := range ids {
			if id == want {
				return
			}
		}
		t.Fatalf("precondition: SessionIDs() = %v does not include %q", ids, want)
	}
	mustContain("default")
	mustContain("watcher:daily")

	createDaemonWindowSessions(client, wb)

	// The genuine user window was created.
	if g.GetUserSession("session-1") == nil {
		t.Errorf("genuine window session-1 was NOT created on the daemon")
	}
	// The backend-only reserved ids were skipped — no daemon session under either.
	if us := g.GetUserSession("default"); us != nil {
		t.Errorf("backend-only 'default' was created on the daemon; the filter must exclude it")
	}
	if us := g.GetUserSession("watcher:daily"); us != nil {
		t.Errorf("backend-only 'watcher:daily' was created on the daemon; the filter must exclude it")
	}
}

// TestStartHandoffEmptyWorkbenchIsNoop guards the degenerate case: with no open
// windows the helper must be a harmless no-op (no panic, no requests). (criterion 3)
func TestStartHandoffEmptyWorkbenchIsNoop(t *testing.T) {
	g, _, client := newHandoffTestServer(t)
	wb := newHandoffWorkbench(t) // no windows

	createDaemonWindowSessions(client, wb) // must not panic

	for _, id := range g.SessionIDs() {
		t.Errorf("empty workbench handoff created an unexpected daemon session %q", id)
	}
}

// TestStartHandoffCreateFailureLoggedAndContinues pins the graceful-degradation
// contract: if a per-window create fails (e.g. the daemon became unreachable), the
// failure is logged and the helper continues with the remaining windows rather than
// aborting the whole handoff for one window. (criterion 2 — usability)
func TestStartHandoffCreateFailureLoggedAndContinues(t *testing.T) {
	g, httpSrv, client := newHandoffTestServer(t)
	wb := newHandoffWorkbench(t)
	wb.NewSession() // session-1
	wb.NewSession() // session-2

	// Take the daemon down before the create loop: every CreateSession now fails.
	httpSrv.Close()

	var buf bytes.Buffer
	orig := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(orig)

	createDaemonWindowSessions(client, wb) // must not panic despite all creates failing

	out := buf.String()
	// Each failed window is logged (matching the existing remote OnClose/OnStop
	// log-and-continue pattern), not silently swallowed.
	if got := strings.Count(out, "handoff: create session"); got < 2 {
		t.Errorf("expected at least 2 per-window create-failure log lines (one per window); got %d in %q", got, out)
	}
	// No partial session leaked onto the (now-closed) daemon core.
	for _, id := range g.SessionIDs() {
		t.Errorf("a create-failure path left an unexpected daemon session %q", id)
	}
}
