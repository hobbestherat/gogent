package ui

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Issue #570 — RemoteClient.Handlers wires GetWorkspaceRoot to cachedWorkspaceRoot,
// which fetches the daemon's (immutable) workspace root ONCE in the background and
// caches it. The status line reads GetWorkspaceRoot live on every refresh, so the
// cache must: (a) never block the UI thread on the network, (b) round-trip
// /api/workspace at most once and serve later refreshes from cache, (c) retry after a
// transient failure (a failed fetch is not cached), and (d) stay race-clean under
// concurrent refreshes (CI runs -race). These tests pin each property.

// pollFor waits up to ~3s for cond to read true. The cache is populated
// asynchronously (a background GET), so cache tests poll rather than assume the first
// synchronous call already has the value.
func pollFor(t *testing.T, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for: %s", msg)
}

// wsServer returns an httptest daemon that writes the given root JSON to every
// /api/workspace request and counts requests via *count.
func wsServer(t *testing.T, root string, count *int32) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(count, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"root":"` + root + `"}`))
	}))
}

func newRemoteGet(t *testing.T, srv *httptest.Server) func() string {
	t.Helper()
	client, err := NewAPIClient(srv.URL, "tok")
	if err != nil {
		t.Fatalf("NewAPIClient: %v", err)
	}
	return NewRemoteClient(client, nil, nil).Handlers().GetWorkspaceRoot
}

// TestRemoteHandlerGetWorkspaceRootWired is the package-level wiring pin (the #551
// pin, flipped): Handlers() must wire GetWorkspaceRoot so the attached status line can
// show the daemon root. Was intentionally nil pre-#570.
func TestRemoteHandlerGetWorkspaceRootWired(t *testing.T) {
	var c int32
	srv := wsServer(t, "/d/ws", &c)
	defer srv.Close()

	if got := newRemoteGet(t, srv); got == nil {
		t.Fatal("attached Handlers must wire GetWorkspaceRoot (issue #570); was nil")
	}
}

// TestCachedWorkspaceRootEmptyUntilFetchThenCaches: the first call returns "" and
// kicks a background GET (it must NOT block the UI thread on the network); once the
// fetch lands the daemon root is cached and every later call returns it.
func TestCachedWorkspaceRootEmptyUntilFetchThenCaches(t *testing.T) {
	var c int32
	srv := wsServer(t, "/d/ws", &c)
	defer srv.Close()

	get := newRemoteGet(t, srv)

	// First call is non-blocking: it kicks the fetch and returns "" synchronously
	// before the HTTP round-trip can complete.
	if got := get(); got != "" {
		t.Errorf("first GetWorkspaceRoot = %q, want empty (the fetch is async/non-blocking)", got)
	}
	pollFor(t, func() bool { return get() == "/d/ws" }, "cached daemon workspace root")
	// Stable thereafter (immutable root, served from cache).
	if got := get(); got != "/d/ws" {
		t.Errorf("GetWorkspaceRoot = %q after cache, want /d/ws", got)
	}
}

// TestCachedWorkspaceRootCoalescesToOneRequest: GetWorkspaceRoot is read on every
// status refresh, so the cache must round-trip /api/workspace EXACTLY once and serve
// every later refresh from cache — otherwise each refresh would stall the UI on the
// SSH tunnel. A short server delay widens the in-flight window so concurrent callers
// are observed coalescing onto the single request rather than each issuing its own.
func TestCachedWorkspaceRootCoalescesToOneRequest(t *testing.T) {
	var requests int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&requests, 1)
		// Hold the in-flight request briefly so callers arriving while it is in
		// flight are observed coalescing (wsFetching dedup), not spawning their own.
		time.Sleep(25 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"root":"/d/ws"}`))
	}))
	defer srv.Close()

	get := newRemoteGet(t, srv)

	// Kick the fetch; it is now in flight (sleeping in the server).
	get()
	pollFor(t, func() bool { return atomic.LoadInt32(&requests) >= 1 }, "background fetch to reach the server")

	// Every caller arriving while the fetch is in flight must coalesce onto it: no
	// per-caller HTTP. (Late callers that land after the fetch completes simply hit
	// the cache; either way exactly one request is issued.)
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = get()
		}()
	}
	wg.Wait()

	// Release/observe completion and assert the cache serves every later read.
	pollFor(t, func() bool { return get() == "/d/ws" }, "cached root after the in-flight fetch completes")
	for i := 0; i < 20; i++ {
		if got := get(); got != "/d/ws" {
			t.Errorf("post-cache GetWorkspaceRoot = %q, want /d/ws (served from cache)", got)
		}
	}
	if got := atomic.LoadInt32(&requests); got != 1 {
		t.Errorf("issued %d /api/workspace requests, want exactly 1 (cache coalescing failed)", got)
	}
}

// TestCachedWorkspaceRootRetriesAfterFailure: a failed fetch is NOT cached, so a later
// refresh re-fetches and succeeds once the daemon recovers (a transient blip at attach
// time). The first /api/workspace fails; once the server answers 200 the next settled
// refresh caches the root.
func TestCachedWorkspaceRootRetriesAfterFailure(t *testing.T) {
	var attempts int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if atomic.AddInt32(&attempts, 1) == 1 {
			http.Error(w, "boom", http.StatusBadGateway)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"root":"/d/ws"}`))
	}))
	defer srv.Close()

	get := newRemoteGet(t, srv)

	// The repeated status-refresh read (pollFor calls get() each tick) must eventually
	// retry past the first 502 and cache the daemon root.
	pollFor(t, func() bool { return get() == "/d/ws" }, "retry to succeed after the first 502")
	if got := atomic.LoadInt32(&attempts); got < 2 {
		t.Errorf("attempts = %d, want >= 2 (a failed fetch must not be cached → a later refresh retries)", got)
	}
	// Now cached: further calls do not re-fetch.
	for i := 0; i < 10; i++ {
		if got := get(); got != "/d/ws" {
			t.Errorf("GetWorkspaceRoot = %q after cache, want /d/ws", got)
		}
	}
}

// TestCachedWorkspaceRootConcurrentCallersRaceClean: many status-refresh reads from
// concurrent UI goroutines must not race the background fetch's cache write. Run under
// -race (CI: go test ./... -race -count=1) to guard the wsMu over wsRoot/wsFetching.
// After the storm the cache must hold the authoritative root.
func TestCachedWorkspaceRootConcurrentCallersRaceClean(t *testing.T) {
	var c int32
	srv := wsServer(t, "/d/ws", &c)
	defer srv.Close()

	get := newRemoteGet(t, srv)

	var wg sync.WaitGroup
	for i := 0; i < 200; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				_ = get() // exercises wsMu concurrently with the fetch goroutine
			}
		}()
	}
	wg.Wait()

	// The fetch is async, so under load it may still be in flight the instant the
	// storm ends; poll for it to land. (The storm is what stresses wsMu under -race;
	// the eventual value is asserted, not the instant-after-Wait snapshot.)
	pollFor(t, func() bool { return get() == "/d/ws" }, "cached root after the concurrent storm")
}

// TestCachedWorkspaceRootEmptyDaemonRootNotCached: if the daemon reports an EMPTY root
// (a misconfigured daemon — GetWorkspaceRoot is normally non-empty), the cache treats
// it like a failed fetch and refuses to pin an empty string; the status line simply
// stays blank rather than showing a bogus empty path. Documents issue #570's nil-safe
// behaviour at the cache boundary.
func TestCachedWorkspaceRootEmptyDaemonRootNotCached(t *testing.T) {
	var requests int32
	srv := wsServer(t, "", &requests)
	defer srv.Close()

	get := newRemoteGet(t, srv)

	// A few refreshes with settle time between them: the empty root is never cached,
	// so each settled refresh retries and the read stays "" (nil-safe).
	for i := 0; i < 4; i++ {
		if got := get(); got != "" {
			t.Errorf("GetWorkspaceRoot = %q for an empty daemon root, want empty (not cached)", got)
		}
		time.Sleep(15 * time.Millisecond) // let any in-flight fetch settle
	}
	if got := atomic.LoadInt32(&requests); got < 2 {
		t.Errorf("requests = %d for an empty root, want >= 2 (an empty root is not cached → retries)", got)
	}
}
