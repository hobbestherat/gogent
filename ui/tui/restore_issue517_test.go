package ui

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"gogent/internal/config"
)

// Issue #517 (UI half): Restore is bounded — at most restoreEagerTranscripts
// transcripts fetched up front, the rest Deferred; deferred windows skip OnCreate
// and load their transcript exactly once on first focus; reconnect must not blank
// a deferred window. These tests pin the round-trip budget and the lazy-load /
// reconnect contract at the handler + Workbench level (no internal/server import).

// issue517Workbench is a minimal Workbench with one model, used for the
// AdoptSession / Focus / reconnect tests that drive stub handlers directly.
func issue517Workbench() *Workbench {
	return NewWorkbench([]*config.ModelConfig{
		{Name: "main", DisplayName: "Main", Model: "m1"},
	})
}

// wait517 polls an atomic counter until it reaches want (or times out), so the
// tests can observe the async ensureTranscript goroutine deterministically.
func wait517(t *testing.T, c *atomic.Int32, want int32, label string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if c.Load() >= want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s to reach %d (got %d)", label, want, c.Load())
}

// transcriptTextContains reports whether any transcript record's header or child
// line contains sub — used to assert the placeholder is present and that loaded /
// re-synced content rendered (content lands in a record's lines).
func transcriptTextContains(sw *SessionWindow, sub string) bool {
	if sw == nil || sw.transcript == nil {
		return false
	}
	for _, r := range sw.transcript.records {
		if r == nil {
			continue
		}
		if strings.Contains(r.header, sub) {
			return true
		}
		for _, ln := range r.lines {
			if strings.Contains(ln.text, sub) {
				return true
			}
		}
	}
	return false
}

func transcriptHasPlaceholder(sw *SessionWindow) bool {
	return transcriptTextContains(sw, "loads when this window is focused")
}

// --- client query contract ---------------------------------------------------

func TestListSessionsBoundedQueryContract(t *testing.T) {
	// url.Values.Encode sorts keys alphabetically, so the expected RawQuery is the
	// sorted form. limit/offset are only sent when strictly positive (<=0 ⇒ absent,
	// preserving the server's "no param ⇒ no cap / legacy" semantics).
	cases := []struct {
		name         string
		live         bool
		limit        int
		offset       int
		wantRawQuery string
	}{
		{"live and limit (Restore's call)", true, 200, 0, "limit=200&live=true"},
		{"live only", true, 0, 0, "live=true"},
		{"limit only", false, 50, 0, "limit=50"},
		{"offset only", false, 0, 10, "offset=10"},
		{"limit and offset", false, 5, 10, "limit=5&offset=10"},
		{"all absent is back-compat", false, 0, 0, ""},
		{"negative limit/offset are omitted", true, -1, -1, "live=true"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := make(chan string, 1)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/sessions" {
					select {
					case got <- r.URL.RawQuery:
					default:
					}
				}
				_ = json.NewEncoder(w).Encode([]SessionDTO{})
			}))
			defer srv.Close()

			c, err := NewAPIClient(srv.URL, "")
			if err != nil {
				t.Fatalf("NewAPIClient: %v", err)
			}
			if _, err := c.ListSessionsBounded(tc.live, tc.limit, tc.offset); err != nil {
				t.Fatalf("ListSessionsBounded: %v", err)
			}
			select {
			case raw := <-got:
				if raw != tc.wantRawQuery {
					t.Fatalf("RawQuery = %q, want %q", raw, tc.wantRawQuery)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("server never saw the /api/sessions request")
			}
		})
	}
}

func TestListSessionsBoundedDecodesSessions(t *testing.T) {
	// Smoke: the bounded call decodes session DTOs just like ListSessions.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode([]SessionDTO{
			{ID: "s1", Title: "One", Live: true, PrimaryModel: "m"},
			{ID: "s2", Title: "Two", Live: true, PrimaryModel: "m"},
		})
	}))
	defer srv.Close()

	c, _ := NewAPIClient(srv.URL, "")
	got, err := c.ListSessionsBounded(true, 10, 0)
	if err != nil {
		t.Fatalf("ListSessionsBounded: %v", err)
	}
	if len(got) != 2 || got[0].ID != "s1" || got[1].ID != "s2" {
		t.Fatalf("decoded = %+v, want s1,s2", got)
	}
}

// --- Restore bounding --------------------------------------------------------

// issue517RestoreServer serves `live` eligible sessions for GET /api/sessions and
// counts every transcript fetch, returning the counter plus the channel that
// captures the listing request's RawQuery.
func issue517RestoreServer(t *testing.T, live int) (*httptest.Server, *atomic.Int32, <-chan string) {
	t.Helper()
	var transcripts atomic.Int32
	listQuery := make(chan string, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/sessions":
			select {
			case listQuery <- r.URL.RawQuery:
			default:
			}
			out := make([]SessionDTO, live)
			for i := range out {
				out[i] = SessionDTO{
					ID: fmt.Sprintf("sess-%03d", i), Title: fmt.Sprintf("S%d", i),
					Live: true, PrimaryModel: "m",
				}
			}
			_ = json.NewEncoder(w).Encode(out)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/transcript"):
			transcripts.Add(1)
			_ = json.NewEncoder(w).Encode([]MessageDTO{{Role: "assistant", Content: "ok"}})
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &transcripts, listQuery
}

func TestRestoreEagerCapAndDeferral(t *testing.T) {
	// With N eligible live sessions, Restore fetches min(N, restoreEagerTranscripts)
	// transcripts up front and defers the rest. The wire response is bounded by the
	// restoreMaxWindows limit Restore passes.
	cases := []struct {
		name         string
		live         int
		wantFetches  int32
		wantEager    int
		wantDeferred int
	}{
		{"under cap (no deferral)", 5, 5, 5, 0},
		{"exactly at cap", restoreEagerTranscripts, int32(restoreEagerTranscripts), restoreEagerTranscripts, 0},
		{"over cap", 30, int32(restoreEagerTranscripts), restoreEagerTranscripts, 30 - restoreEagerTranscripts},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, transcripts, listQuery := issue517RestoreServer(t, tc.live)
			c, err := NewAPIClient(srv.URL, "")
			if err != nil {
				t.Fatalf("NewAPIClient: %v", err)
			}
			restored := NewRemoteClient(c, nil, nil).Handlers().Restore()

			if got := transcripts.Load(); got != tc.wantFetches {
				t.Fatalf("transcript fetches = %d, want %d", got, tc.wantFetches)
			}
			if got := len(restored); got != tc.live {
				t.Fatalf("restored %d windows, want all %d eligible", got, tc.live)
			}
			eager, deferred := 0, 0
			for _, rs := range restored {
				if rs.Deferred {
					deferred++
					if len(rs.Messages) != 0 {
						t.Fatalf("deferred session %q carries messages (should fetch on focus)", rs.ID)
					}
				} else {
					eager++
				}
			}
			if eager != tc.wantEager || deferred != tc.wantDeferred {
				t.Fatalf("eager=%d deferred=%d, want eager=%d deferred=%d", eager, deferred, tc.wantEager, tc.wantDeferred)
			}

			// Restore must pass the bound: live=true AND limit=restoreMaxWindows.
			select {
			case raw := <-listQuery:
				if !strings.Contains(raw, "live=true") {
					t.Fatalf("Restore listing query %q does not request live=true", raw)
				}
				if !strings.Contains(raw, fmt.Sprintf("limit=%d", restoreMaxWindows)) {
					t.Fatalf("Restore listing query %q does not cap to limit=%d", raw, restoreMaxWindows)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("Restore never issued GET /api/sessions")
			}
		})
	}
}

func TestRestoreExcludesDefaultAndWatcherSessions(t *testing.T) {
	// The shared "default" and "watcher:" sessions are backend-only and must be
	// excluded before any transcript fetch (unchanged exclusion, #517).
	var transcripts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/sessions":
			_ = json.NewEncoder(w).Encode([]SessionDTO{
				{ID: "default", Title: "Default", Live: true},
				{ID: "watcher:nightly", Title: "Watcher", Live: true},
				{ID: "sess-a", Title: "A", Live: true, PrimaryModel: "m"},
				{ID: "sess-b", Title: "B", Live: true, PrimaryModel: "m"},
			})
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/transcript"):
			transcripts.Add(1)
			_ = json.NewEncoder(w).Encode([]MessageDTO{{Role: "assistant", Content: "x"}})
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer srv.Close()

	c, _ := NewAPIClient(srv.URL, "")
	restored := NewRemoteClient(c, nil, nil).Handlers().Restore()

	ids := make(map[string]bool, len(restored))
	for _, rs := range restored {
		ids[rs.ID] = true
	}
	if len(restored) != 2 || !ids["sess-a"] || !ids["sess-b"] {
		t.Fatalf("restored = %+v, want only sess-a and sess-b", restored)
	}
	if ids["default"] || ids["watcher:nightly"] {
		t.Fatalf("default/watcher leaked into restore: %+v", restored)
	}
	if got := transcripts.Load(); got != 2 {
		t.Fatalf("transcript fetches = %d, want 2 (default/watcher skipped before fetch)", got)
	}
}

func TestRestoreListErrorReturnsNil(t *testing.T) {
	// A listing failure must not panic; Restore logs and returns nil so the caller
	// opens an empty workbench rather than crashing.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/sessions" {
			http.Error(w, "down", http.StatusBadGateway)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c, _ := NewAPIClient(srv.URL, "")
	if got := NewRemoteClient(c, nil, nil).Handlers().Restore(); got != nil {
		t.Fatalf("Restore on listing error = %+v, want nil", got)
	}
}

// --- AdoptSession: deferred vs eager ----------------------------------------

func TestAdoptSessionDeferredSkipsOnCreateAndRestore(t *testing.T) {
	w := issue517Workbench()
	var onCreate, getTranscript atomic.Int32
	w.handlers.OnCreate = func(id, title string) { onCreate.Add(1) }
	w.handlers.GetTranscript = func(id, agent string) []ChatMessage {
		getTranscript.Add(1)
		return []ChatMessage{{Role: "assistant", Content: "should-not-load"}}
	}

	// Eager: transcript restored from rs.Messages, OnCreate fires.
	swEager := w.AdoptSession(RestoredSession{
		ID: "e1", Title: "E", Model: "main",
		Messages: []ChatMessage{{Role: "user", Content: "hello-eager"}},
	})
	if onCreate.Load() != 1 {
		t.Fatalf("eager adopt must call OnCreate once, got %d", onCreate.Load())
	}
	if transcriptHasPlaceholder(swEager) {
		t.Fatal("eager window shows the deferred placeholder")
	}
	if !transcriptTextContains(swEager, "hello-eager") {
		t.Fatal("eager window did not restore its transcript")
	}

	// Deferred: no OnCreate, no restore, placeholder seeded, flagged.
	swDeferred := w.AdoptSession(RestoredSession{ID: "d1", Title: "D", Model: "main", Deferred: true})
	if onCreate.Load() != 1 {
		t.Fatalf("deferred adopt must NOT call OnCreate, got %d", onCreate.Load())
	}
	if getTranscript.Load() != 0 {
		t.Fatal("deferred adopt must not fetch a transcript")
	}
	if !w.deferredTranscripts["d1"] {
		t.Fatal("deferred adopt must record the deferred flag")
	}
	if !transcriptHasPlaceholder(swDeferred) {
		t.Fatal("deferred window must seed the 'loads on focus' placeholder")
	}
	if transcriptTextContains(swDeferred, "should-not-load") {
		t.Fatal("deferred window must not contain a transcript")
	}
}

// --- Focus: lazy load, exactly once -----------------------------------------

func TestFocusLazyLoadsDeferredTranscriptOnce(t *testing.T) {
	w := issue517Workbench()
	var onCreate, getTranscript atomic.Int32
	w.handlers.OnCreate = func(id, title string) { onCreate.Add(1) }
	w.handlers.GetTranscript = func(id, agent string) []ChatMessage {
		getTranscript.Add(1)
		return []ChatMessage{
			{Role: "user", Content: "hello"},
			{Role: "assistant", Content: "world"},
		}
	}

	sw := w.AdoptSession(RestoredSession{ID: "d1", Title: "D", Deferred: true})
	if onCreate.Load() != 0 || getTranscript.Load() != 0 {
		t.Fatalf("deferred adopt must not fetch, got onCreate=%d getTranscript=%d", onCreate.Load(), getTranscript.Load())
	}
	if !transcriptHasPlaceholder(sw) {
		t.Fatal("deferred window must show the placeholder before focus")
	}

	// First focus triggers exactly one async OnCreate + GetTranscript.
	w.Focus("d1")
	wait517(t, &getTranscript, 1, "first-focus GetTranscript")
	wait517(t, &onCreate, 1, "first-focus OnCreate")
	drainPostedEventually(t, w) // apply the async reload on the UI thread

	if got := getTranscript.Load(); got != 1 {
		t.Fatalf("after first focus, getTranscript = %d, want 1", got)
	}
	if w.deferredTranscripts["d1"] {
		t.Fatal("deferred flag must be cleared once the load starts")
	}
	if transcriptHasPlaceholder(sw) {
		t.Fatal("placeholder must be replaced after the transcript loads")
	}
	if !transcriptTextContains(sw, "world") {
		t.Fatal("focused deferred window did not render the fetched transcript")
	}

	// Second focus must NOT re-fetch (exactly-once).
	w.Focus("d1")
	drainPosted(t, w)
	if got := getTranscript.Load(); got != 1 {
		t.Fatalf("refocus re-fetched: getTranscript = %d, want 1", got)
	}
}

func TestFocusFailedFetchKeepsPlaceholder(t *testing.T) {
	// Same root-cause invariant as the reconnect case, on the (more common) focus
	// path: ensureTranscript clears the deferred flag BEFORE fetching (for exactly-
	// once), so a GetTranscript that returns nil (a transient failure) feeds
	// reload(nil), which wipes the placeholder and leaves a silently EMPTY window
	// that — flag already cleared — can never be retried by refocusing. A failed
	// load must not destroy what was shown.
	w := issue517Workbench()
	var loads atomic.Int32
	w.handlers.OnCreate = func(id, title string) {}
	w.handlers.GetTranscript = func(id, agent string) []ChatMessage {
		loads.Add(1)
		return nil // simulate a fetch failure
	}
	sw := w.AdoptSession(RestoredSession{ID: "d1", Title: "D", Deferred: true})
	w.Focus("d1")
	wait517(t, &loads, 1, "failed focus fetch")
	drainPostedEventually(t, w)

	if !transcriptHasPlaceholder(sw) {
		t.Fatal("a failed focus fetch blanked the deferred window's placeholder " +
			"(regression): the window is now silently empty and, the flag already cleared, cannot retry on refocus")
	}
}

func TestFocusRetriesAfterFailedFetch(t *testing.T) {
	// Companion to TestFocusFailedFetchKeepsPlaceholder: a failed fetch must not just
	// preserve the placeholder — it must RE-ARM the deferred flag so a subsequent
	// focus retries, instead of stranding the window (the exactly-once guard must
	// yield to retry-on-failure).
	w := issue517Workbench()
	var loads atomic.Int32
	w.handlers.OnCreate = func(id, title string) {}
	w.handlers.GetTranscript = func(id, agent string) []ChatMessage {
		if loads.Add(1) == 1 {
			return nil // first attempt fails
		}
		return []ChatMessage{{Role: "assistant", Content: "loaded-on-retry"}}
	}
	sw := w.AdoptSession(RestoredSession{ID: "d1", Title: "D", Deferred: true})

	w.Focus("d1") // fails (nil)
	wait517(t, &loads, 1, "first (failed) focus fetch")
	drainPostedEventually(t, w)
	if !transcriptHasPlaceholder(sw) {
		t.Fatal("placeholder should remain after a failed fetch")
	}

	w.Focus("d1") // retries and succeeds
	wait517(t, &loads, 2, "retry focus fetch")
	drainPostedEventually(t, w)
	if got := loads.Load(); got != 2 {
		t.Fatalf("fetches = %d, want 2 (one failed attempt + one retry)", got)
	}
	if transcriptHasPlaceholder(sw) {
		t.Fatal("placeholder remained after a successful retry fetch")
	}
	if !transcriptTextContains(sw, "loaded-on-retry") {
		t.Fatal("retry did not render the transcript")
	}
}

func TestFocusEmptyTranscriptClearsPlaceholder(t *testing.T) {
	// A *successful* load of a genuinely-empty transcript ([]ChatMessage{}, non-nil
	// — distinct from a nil failure, which the remote GetTranscript handler returns
	// only on error) must still clear the "loads on focus" placeholder: the
	// transcript HAS loaded, it simply has no messages. The eager path renders an
	// empty window for the same session, so the deferred path must not strand the
	// user on a perpetual placeholder (nor, with the flag re-armed, re-fetch on
	// every refocus). reload()'s len==0 no-op must not conflate empty-success with
	// nil-failure.
	w := issue517Workbench()
	var loads atomic.Int32
	w.handlers.OnCreate = func(id, title string) {}
	w.handlers.GetTranscript = func(id, agent string) []ChatMessage {
		loads.Add(1)
		return []ChatMessage{} // success, but empty
	}
	sw := w.AdoptSession(RestoredSession{ID: "d1", Title: "D", Deferred: true})
	w.Focus("d1")
	wait517(t, &loads, 1, "empty-transcript focus load")
	drainPostedEventually(t, w)

	if transcriptHasPlaceholder(sw) {
		t.Fatal("a successfully-loaded empty transcript left the 'loads on focus' " +
			"placeholder in place (empty-success conflated with nil-failure); the window reads as never-loaded and will re-fetch on every refocus")
	}
}

func TestFocusNonDeferredWindowDoesNotFetch(t *testing.T) {
	// Focusing an eagerly-restored window must be a no-op for transcript loading.
	w := issue517Workbench()
	var getTranscript atomic.Int32
	w.handlers.OnCreate = func(id, title string) {}
	w.handlers.GetTranscript = func(id, agent string) []ChatMessage {
		getTranscript.Add(1)
		return nil
	}
	w.AdoptSession(RestoredSession{
		ID: "e1", Title: "E",
		Messages: []ChatMessage{{Role: "user", Content: "hi"}},
	})
	w.Focus("e1")
	drainPosted(t, w)
	if got := getTranscript.Load(); got != 0 {
		t.Fatalf("focusing a non-deferred window fetched the transcript %d times, want 0", got)
	}
}

func TestFocusUnknownIDIsNoOp(t *testing.T) {
	// Focusing an id with no window must not panic or fetch.
	w := issue517Workbench()
	var getTranscript atomic.Int32
	w.handlers.GetTranscript = func(id, agent string) []ChatMessage {
		getTranscript.Add(1)
		return nil
	}
	w.Focus("nope-not-adopted")
	drainPosted(t, w)
	if got := getTranscript.Load(); got != 0 {
		t.Fatalf("focus on unknown id fetched %d times, want 0", got)
	}
}

// --- Reconnect: no blanking, re-sync, adopt ---------------------------------

func TestReconnectLeavesDeferredShellUnloaded(t *testing.T) {
	// An unloaded deferred shell must NOT be blanked or fetched on reconnect — it
	// stays a shell and still loads on focus. (The pre-fix reload(rs.Messages) with
	// nil messages would have cleared its placeholder.)
	w := issue517Workbench()
	var getTranscript atomic.Int32
	w.handlers.GetTranscript = func(id, agent string) []ChatMessage {
		getTranscript.Add(1)
		return []ChatMessage{{Role: "assistant", Content: "should-not-load-on-reconnect"}}
	}
	sw := w.AdoptSession(RestoredSession{ID: "d1", Title: "D", Deferred: true})
	w.handlers.Restore = func() []RestoredSession {
		return []RestoredSession{{ID: "d1", Title: "D", Deferred: true}}
	}

	w.refreshAfterReconnect()
	drainPosted(t, w)

	if got := getTranscript.Load(); got != 0 {
		t.Fatalf("reconnect fetched an unloaded shell %d times, want 0", got)
	}
	if !transcriptHasPlaceholder(sw) {
		t.Fatal("reconnect blanked the deferred shell's placeholder")
	}
	if !w.deferredTranscripts["d1"] {
		t.Fatal("reconnect cleared the deferred flag of an unloaded shell")
	}
}

func TestReconnectReSyncsLoadedDeferredWindow(t *testing.T) {
	// A deferred window the user had loaded before the drop is re-synced to the
	// daemon's current transcript on reconnect (a fresh fetch + reload), not
	// blanked.
	w := issue517Workbench()
	var seq atomic.Int32
	w.handlers.OnCreate = func(id, title string) {}
	w.handlers.GetTranscript = func(id, agent string) []ChatMessage {
		switch seq.Add(1) {
		case 1:
			return []ChatMessage{{Role: "assistant", Content: "first-load"}}
		default:
			return []ChatMessage{{Role: "assistant", Content: "re-synced"}}
		}
	}
	sw := w.AdoptSession(RestoredSession{ID: "d1", Title: "D", Deferred: true})

	// Load it once via focus, clearing the deferred flag.
	w.Focus("d1")
	wait517(t, &seq, 1, "initial focus load")
	drainPostedEventually(t, w)
	if !transcriptTextContains(sw, "first-load") {
		t.Fatal("initial focus did not load the transcript")
	}

	// Reconnect: the flag is gone, so the deferred-but-loaded branch re-syncs.
	w.handlers.Restore = func() []RestoredSession {
		return []RestoredSession{{ID: "d1", Title: "D", Deferred: true}}
	}
	w.refreshAfterReconnect() // re-fetch is synchronous on the reconnect goroutine
	wait517(t, &seq, 2, "reconnect re-sync fetch")
	drainPostedEventually(t, w)

	if transcriptTextContains(sw, "first-load") {
		t.Fatal("reconnect did not replace the stale transcript")
	}
	if !transcriptTextContains(sw, "re-synced") {
		t.Fatal("reconnect did not re-sync the loaded window with the fresh transcript")
	}
}

func TestReconnectFailedFetchKeepsContentLoaded(t *testing.T) {
	// Regression guard: if the reconnect re-sync fetch returns nothing (the handler
	// yields nil on error), a previously-loaded window must keep its content rather
	// than be blanked by reload(nil). This pins the user-visible invariant that a
	// flaky reconnect must not destroy an open conversation.
	w := issue517Workbench()
	var initial atomic.Int32
	w.handlers.OnCreate = func(id, title string) {}
	w.handlers.GetTranscript = func(id, agent string) []ChatMessage {
		initial.Add(1)
		return []ChatMessage{{Role: "assistant", Content: "first-load"}}
	}
	sw := w.AdoptSession(RestoredSession{ID: "d1", Title: "D", Deferred: true})

	// Load the deferred window once via focus, clearing the deferred flag.
	w.Focus("d1")
	wait517(t, &initial, 1, "initial focus load")
	drainPostedEventually(t, w)
	if !transcriptTextContains(sw, "first-load") {
		t.Fatal("setup: initial transcript did not load")
	}

	// Reconnect's re-sync fetch now fails (handler yields nil).
	w.handlers.GetTranscript = func(id, agent string) []ChatMessage { return nil }
	w.handlers.Restore = func() []RestoredSession {
		return []RestoredSession{{ID: "d1", Title: "D", Deferred: true}}
	}
	w.refreshAfterReconnect()
	drainPostedEventually(t, w)

	if !transcriptTextContains(sw, "first-load") {
		t.Fatal("reconnect blanked a loaded window when the re-sync fetch failed (regression)")
	}
}

func TestReconnectSyncsLoadedWindowToGenuinelyEmptyTranscript(t *testing.T) {
	// Symmetric to the nil-failure case: a *successful* re-sync that returns a
	// genuinely-empty transcript ([]ChatMessage{}, non-nil) is the daemon's
	// authoritative "this session now has no messages" state, so the loaded window
	// must sync to empty — distinct from a nil failure, which keeps the content.
	// This guards the msgs==nil (not len==0) distinction on the reconnect path too.
	w := issue517Workbench()
	var initial atomic.Int32
	w.handlers.OnCreate = func(id, title string) {}
	w.handlers.GetTranscript = func(id, agent string) []ChatMessage {
		initial.Add(1)
		return []ChatMessage{{Role: "assistant", Content: "first-load"}}
	}
	sw := w.AdoptSession(RestoredSession{ID: "d1", Title: "D", Deferred: true})
	w.Focus("d1")
	wait517(t, &initial, 1, "initial focus load")
	drainPostedEventually(t, w)
	if !transcriptTextContains(sw, "first-load") {
		t.Fatal("setup: initial transcript did not load")
	}

	// Daemon now reports an empty transcript (non-nil).
	w.handlers.GetTranscript = func(id, agent string) []ChatMessage { return []ChatMessage{} }
	w.handlers.Restore = func() []RestoredSession {
		return []RestoredSession{{ID: "d1", Title: "D", Deferred: true}}
	}
	w.refreshAfterReconnect()
	drainPostedEventually(t, w)

	if transcriptTextContains(sw, "first-load") {
		t.Fatal("reconnect did not sync the loaded window to the daemon's now-empty transcript")
	}
	if transcriptHasPlaceholder(sw) {
		t.Fatal("a loaded window re-synced to empty must not show the deferred placeholder")
	}
}

func TestReconnectAdoptsSessionLiveDuringOutage(t *testing.T) {
	// A session that became live on the daemon during the outage is adopted (a
	// deferred one as a shell that loads on focus, like first connect).
	w := issue517Workbench()
	var onCreate atomic.Int32
	w.handlers.OnCreate = func(id, title string) { onCreate.Add(1) }
	// Start with one open window so the new one is genuinely "new".
	w.AdoptSession(RestoredSession{ID: "existing", Title: "Existing", Messages: []ChatMessage{{Role: "user", Content: "x"}}})
	before := onCreate.Load()

	w.handlers.Restore = func() []RestoredSession {
		return []RestoredSession{{ID: "new-during-outage", Title: "New", Deferred: true}}
	}
	w.refreshAfterReconnect()
	drainPostedEventually(t, w)

	// The new session gets a window; as deferred it is a shell (no OnCreate yet).
	if _, ok := w.sessions["new-during-outage"]; !ok {
		t.Fatal("reconnect did not adopt a session that went live during the outage")
	}
	if got := onCreate.Load(); got != before {
		t.Fatalf("deferred new session OnCreate = %d, want %d (should load on focus)", got, before)
	}
	if !w.deferredTranscripts["new-during-outage"] {
		t.Fatal("adopted deferred new session was not flagged for lazy load")
	}
}
