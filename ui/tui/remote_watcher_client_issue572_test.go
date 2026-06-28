package ui

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// Issue #572: remote watcher-management client wiring. These exercise the
// Handlers() wiring (each mutation routes to the right daemon verb), the
// WatcherDTO→WatcherInfo mapper (parity with cmd/main.go toWatcherInfo), and the
// non-blocking watcher cache — especially the epoch guard that prevents a stale
// in-flight fetch from clobbering fresh post-mutation state, plus a
// refreshWatcherNodes integration proving the remote-backed ListWatchers builds
// ◷ sidebar nodes (free roots + attached children).

// --- handler routing --------------------------------------------------------

// TestRemoteWatcherHandlersRouteToDaemon asserts each wired handler hits the
// correct method/path/body on the daemon. The cache is left cold so each
// handler's invalidateWatchers() is a no-op and the recorded request is exactly
// the mutation verb (not a stray refresh GET).
func TestRemoteWatcherHandlersRouteToDaemon(t *testing.T) {
	type rec struct {
		Method string
		Path   string
		Body   map[string]any
	}
	var mu sync.Mutex
	var last rec
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body) // nil body → EOF, body stays nil
		mu.Lock()
		last = rec{r.Method, r.URL.Path, body}
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet {
			_, _ = w.Write([]byte("[]"))
			return
		}
		if r.Method == http.MethodPost && r.URL.Path == "/api/watchers" {
			_ = json.NewEncoder(w).Encode(WatcherDTO{ID: "w1", Name: "new", Kind: "free", Target: "free"})
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c, err := NewAPIClient(srv.URL, "tok")
	if err != nil {
		t.Fatalf("NewAPIClient: %v", err)
	}
	h := NewRemoteClient(c, nil, nil).Handlers()

	mk := func() rec {
		mu.Lock()
		defer mu.Unlock()
		return last
	}

	if err := h.EnableWatcher("emailer"); err != nil {
		t.Fatalf("EnableWatcher: %v", err)
	}
	if r := mk(); r.Method != http.MethodPut || r.Path != "/api/watchers/emailer/enabled" || r.Body["enabled"] != true {
		t.Errorf("enable = %+v, want PUT /api/watchers/emailer/enabled {enabled:true}", r)
	}

	if err := h.DisableWatcher("emailer"); err != nil {
		t.Fatalf("DisableWatcher: %v", err)
	}
	if r := mk(); r.Method != http.MethodPut || r.Path != "/api/watchers/emailer/enabled" || r.Body["enabled"] != false {
		t.Errorf("disable = %+v, want PUT .../enabled {enabled:false}", r)
	}

	if err := h.RunWatcher("emailer"); err != nil {
		t.Fatalf("RunWatcher: %v", err)
	}
	if r := mk(); r.Method != http.MethodPost || r.Path != "/api/watchers/emailer/run" {
		t.Errorf("run = %+v, want POST /api/watchers/emailer/run", r)
	}

	if err := h.StopWatcher("emailer"); err != nil {
		t.Fatalf("StopWatcher: %v", err)
	}
	if r := mk(); r.Method != http.MethodPost || r.Path != "/api/watchers/emailer/stop" {
		t.Errorf("stop = %+v, want POST /api/watchers/emailer/stop", r)
	}

	if err := h.DeleteWatcher("emailer"); err != nil {
		t.Fatalf("DeleteWatcher: %v", err)
	}
	if r := mk(); r.Method != http.MethodDelete || r.Path != "/api/watchers/emailer" {
		t.Errorf("delete = %+v, want DELETE /api/watchers/emailer", r)
	}

	// Create forwards ReportToSession and an explicit Enabled:true; the daemon
	// decides kind from report_to_session (nil ⇒ free).
	target := "sess-attach"
	info, err := h.CreateWatcher(WatcherConfig{
		Name: "new", Task: "do thing", Model: "claude", Every: "5m", ReportToSession: &target,
	}, "calling-session-ignored")
	if err != nil {
		t.Fatalf("CreateWatcher: %v", err)
	}
	r := mk()
	if r.Method != http.MethodPost || r.Path != "/api/watchers" {
		t.Errorf("create = %+v, want POST /api/watchers", r)
	}
	if r.Body["name"] != "new" || r.Body["model"] != "claude" || r.Body["enabled"] != true {
		t.Errorf("create body = %+v, want name/model/enabled:true", r.Body)
	}
	if r.Body["report_to_session"] != "sess-attach" {
		t.Errorf("create report_to_session = %v, want forwarded from cfg", r.Body["report_to_session"])
	}
	if sched, ok := r.Body["schedule"].(map[string]any); !ok || sched["every"] != "5m" {
		t.Errorf("create schedule = %+v, want every=5m", r.Body["schedule"])
	}
	// CreateWatcher is also a mutation: it must return a mapped WatcherInfo and
	// must NOT panic on a cold cache (invalidateWatchers over an empty key set).
	if info.ID != "w1" || !info.Free {
		t.Errorf("CreateWatcher returned %+v, want w1 free", info)
	}
}

// TestRemoteWatcherCreateWatcherErrorWrapping mirrors embedded's
// "create watcher: %w" wrap so the dialog surfaces a consistent message.
func TestRemoteWatcherCreateWatcherErrorWrapping(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "bad schedule", http.StatusBadRequest)
	}))
	defer srv.Close()
	c, _ := NewAPIClient(srv.URL, "")
	h := NewRemoteClient(c, nil, nil).Handlers()
	_, err := h.CreateWatcher(WatcherConfig{Name: "x", Every: "5m"}, "")
	if err == nil {
		t.Fatal("CreateWatcher on 400 = nil, want error")
	}
	// Don't over-assert the exact text, but the wrap should be non-empty.
	if err.Error() == "" {
		t.Errorf("CreateWatcher error text empty")
	}
}

// --- mapper -----------------------------------------------------------------

func TestWatcherDTOToInfo(t *testing.T) {
	free := watcherDTOToInfo(WatcherDTO{
		ID: "f1", Name: "emailer", Kind: "free", Target: "free", Task: "send",
		Schedule: "every 5m", Enabled: true, Status: "idle",
		NextFire: "2026-06-28T15:04:00Z", LastResult: "ok", LastError: "",
	})
	if !free.Free {
		t.Errorf("free.Free = false, want true (kind=free)")
	}
	if free.TargetSession != "" {
		t.Errorf("free.TargetSession = %q, want empty", free.TargetSession)
	}
	if free.SessionID != "watcher:emailer" {
		t.Errorf("free.SessionID = %q, want watcher:emailer", free.SessionID)
	}
	if free.Running {
		t.Errorf("free.Running = true, want false (status=idle)")
	}
	if free.Task != "send" || free.Schedule != "every 5m" || free.LastResult != "ok" || !free.Enabled {
		t.Errorf("free pass-through fields wrong: %+v", free)
	}
	if free.NextFire != "2026-06-28 15:04" {
		t.Errorf("free.NextFire = %q, want 2026-06-28 15:04", free.NextFire)
	}

	att := watcherDTOToInfo(WatcherDTO{
		ID: "a1", Name: "gh", Kind: "attached", Target: "sess-9", Status: "running",
	})
	if att.Free {
		t.Errorf("attached.Free = true, want false")
	}
	if att.TargetSession != "sess-9" {
		t.Errorf("attached.TargetSession = %q, want sess-9", att.TargetSession)
	}
	if att.SessionID != "sess-9" {
		t.Errorf("attached.SessionID = %q, want sess-9 (the target)", att.SessionID)
	}
	if !att.Running {
		t.Errorf("attached.Running = false, want true (status=running)")
	}
}

func TestWatcherDTOToInfoRunningFromStatus(t *testing.T) {
	for _, s := range []string{"idle", "skipped", "failed"} {
		if got := watcherDTOToInfo(WatcherDTO{Kind: "free", Status: s}); got.Running {
			t.Errorf("status %q: Running = true, want false", s)
		}
	}
	if got := watcherDTOToInfo(WatcherDTO{Kind: "free", Status: "running"}); !got.Running {
		t.Errorf("status running: Running = false, want true")
	}
}

func TestReformatWatcherTime(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", ""},
		{"2026-06-28T15:04:05Z", "2026-06-28 15:04"},
		{"2026-06-28T15:04:05.123Z", "2026-06-28 15:04"}, // fractional seconds allowed
		{"not-a-time", "not-a-time"},                     // unparseable → raw, not dropped
		{"2026-13-40T99:99:99Z", "2026-13-40T99:99:99Z"},
	}
	for _, tc := range cases {
		if got := reformatWatcherTime(tc.in); got != tc.want {
			t.Errorf("reformatWatcherTime(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// --- watcher cache ----------------------------------------------------------

// peekCache reads a cache key under the lock (no off-thread spawn), so a test can
// observe the cache deterministically without invoking the spawning handler.
func peekCache(rc *RemoteClient, key string) []WatcherInfo {
	rc.watchMu.Lock()
	defer rc.watchMu.Unlock()
	return rc.watchCache[key]
}

// waitWatcherCache polls until the cache key holds want entries (or times out),
// letting an off-thread fetch land. Returns false on timeout.
func waitWatcherCache(rc *RemoteClient, key string, want int) bool {
	for i := 0; i < 500; i++ {
		rc.watchMu.Lock()
		n := len(rc.watchCache[key])
		rc.watchMu.Unlock()
		if n == want {
			return true
		}
		time.Sleep(2 * time.Millisecond)
	}
	return false
}

func currentGen(rc *RemoteClient) uint64 {
	rc.watchMu.Lock()
	defer rc.watchMu.Unlock()
	return rc.watchGen
}

// TestWatcherCacheFirstCallNilThenPopulates: the handler is non-blocking — the
// first call returns nil and kicks an off-thread fetch; subsequent calls return
// the cached snapshot. This is what keeps refreshWatcherNodes off the network on
// the UI thread.
func TestWatcherCacheFirstCallNilThenPopulates(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode([]WatcherDTO{{ID: "w", Name: "w", Kind: "free"}})
	}))
	defer srv.Close()
	c, _ := NewAPIClient(srv.URL, "")
	rc := NewRemoteClient(c, nil, nil)
	h := rc.Handlers()

	if got := h.ListWatchers(""); got != nil {
		t.Fatalf("first call must return nil (non-blocking), got %+v", got)
	}
	if !waitWatcherCache(rc, "", 1) {
		t.Fatal("off-thread fetch did not populate the cache")
	}
	got := h.ListWatchers("")
	if len(got) != 1 || got[0].ID != "w" {
		t.Fatalf("after populate = %+v, want one watcher w", got)
	}
}

// TestWatcherCacheReturnsDefensiveCopy: the handler returns a copy of the cached
// slice so a caller cannot mutate the cache's backing storage.
func TestWatcherCacheReturnsDefensiveCopy(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode([]WatcherDTO{{ID: "w", Name: "w", Kind: "free"}})
	}))
	defer srv.Close()
	c, _ := NewAPIClient(srv.URL, "")
	rc := NewRemoteClient(c, nil, nil)
	h := rc.Handlers()

	h.ListWatchers("")
	if !waitWatcherCache(rc, "", 1) {
		t.Fatal("fetch did not land")
	}
	got := h.ListWatchers("")
	got[0].ID = "MUTATED"
	grown := append(got, WatcherInfo{ID: "extra"}) // grow the returned slice

	cached := peekCache(rc, "")
	if len(cached) != 1 {
		t.Errorf("cache length changed to %d after caller mutated/grew the returned slice", len(cached))
	}
	if cached[0].ID == "MUTATED" {
		t.Error("caller's element mutation escaped into the cache (no defensive copy)")
	}
	if len(grown) != 2 {
		t.Errorf("local append on the returned copy should reach len 2, got %d", len(grown))
	}
}

// TestWatcherCacheKeepsLastGoodOnError: a failed fetch keeps the last-good slice
// (no flicker to empty on a transient blip); the next tick retries.
func TestWatcherCacheKeepsLastGoodOnError(t *testing.T) {
	var mu sync.Mutex
	fail := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		f := fail
		mu.Unlock()
		if f {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode([]WatcherDTO{{ID: "w", Name: "w", Kind: "free"}})
	}))
	defer srv.Close()
	c, _ := NewAPIClient(srv.URL, "")
	rc := NewRemoteClient(c, nil, nil)

	// Seed the cache with a successful fetch at gen 0.
	rc.fetchWatchers("", 0)
	if len(peekCache(rc, "")) != 1 {
		t.Fatal("seed fetch did not populate cache")
	}
	// A failing fetch (same epoch) must not wipe the last-good slice.
	mu.Lock()
	fail = true
	mu.Unlock()
	rc.fetchWatchers("", currentGen(rc))
	if got := peekCache(rc, ""); len(got) != 1 || got[0].ID != "w" {
		t.Fatalf("last-good slice wiped on error: %+v", got)
	}
}

// TestWatcherCacheEpochGuardsStaleFetch is the core race-fix test (design §3). A
// background fetch that started BEFORE a mutation (old epoch) reads pre-mutation
// state and must be DISCARDED on commit so it cannot clobber the fresh
// post-mutation state the mutation handler wrote synchronously. It also proves a
// current-epoch fetch still commits (positive control).
func TestWatcherCacheEpochGuardsStaleFetch(t *testing.T) {
	var mu sync.Mutex
	enabled := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet {
			mu.Lock()
			e := enabled
			mu.Unlock()
			_ = json.NewEncoder(w).Encode([]WatcherDTO{{ID: "w", Name: "w", Kind: "free", Enabled: e, Status: "idle"}})
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	c, _ := NewAPIClient(srv.URL, "")
	rc := NewRemoteClient(c, nil, nil)

	// gen 0: populate the cache with enabled=false.
	rc.fetchWatchers("", 0)
	if got := peekCache(rc, ""); len(got) != 1 || got[0].Enabled {
		t.Fatalf("initial fetch want disabled, got %+v", got)
	}

	// Mutation bumps the epoch to 1 and synchronously refreshes to enabled=true.
	mu.Lock()
	enabled = true
	mu.Unlock()
	rc.invalidateWatchers()
	if got := peekCache(rc, ""); !got[0].Enabled {
		t.Fatalf("mutation refresh want enabled, got %+v", got)
	}

	// Stale fetch at the OLD epoch (0) reads enabled=false; it must be dropped so
	// the fresh enabled=true state survives (the cache must still read enabled).
	mu.Lock()
	enabled = false
	mu.Unlock()
	rc.fetchWatchers("", 0)
	if got := peekCache(rc, ""); !got[0].Enabled {
		t.Fatalf("stale gen-0 fetch clobbered the fresh enabled state — epoch guard failed: %+v", got)
	}

	// Positive control: a fetch at the CURRENT epoch commits normally.
	mu.Lock()
	enabled = false
	mu.Unlock()
	rc.fetchWatchers("", currentGen(rc))
	if got := peekCache(rc, ""); got[0].Enabled {
		t.Fatalf("current-gen fetch should have committed disabled, got %+v", got)
	}
}

// TestRemoteWatcherHandlerMutationRefreshesCache: after a mutation handler
// succeeds, the next ListWatchers read is fresh IMMEDIATELY (synchronous
// epoch-guarded refresh) — the dialog's post-action re-render must not wait for
// the 1s tick. This is the dialog-freshness property from design §3.
func TestRemoteWatcherHandlerMutationRefreshesCache(t *testing.T) {
	var mu sync.Mutex
	enabled := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet {
			mu.Lock()
			e := enabled
			mu.Unlock()
			_ = json.NewEncoder(w).Encode([]WatcherDTO{{ID: "w", Name: "w", Kind: "free", Enabled: e}})
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	c, _ := NewAPIClient(srv.URL, "")
	rc := NewRemoteClient(c, nil, nil)
	h := rc.Handlers()

	// Warm the free-set cache with the disabled state.
	h.ListWatchers("")
	if !waitWatcherCache(rc, "", 1) {
		t.Fatal("initial fetch did not land")
	}
	if peekCache(rc, "")[0].Enabled {
		t.Fatal("want disabled before mutation")
	}

	// Flip the daemon state and run the Enable mutation handler.
	mu.Lock()
	enabled = true
	mu.Unlock()
	if err := h.EnableWatcher("w"); err != nil {
		t.Fatalf("EnableWatcher: %v", err)
	}

	// The very next read reflects the mutation — no tick wait required.
	got := h.ListWatchers("")
	if len(got) != 1 || !got[0].Enabled {
		t.Fatalf("want enabled immediately after mutation, got %+v", got)
	}
}

// --- refreshWatcherNodes integration (acceptance #1/#6) ---------------------

// TestRefreshWatcherNodesWithRemoteHandlers proves the live path end-to-end:
// the remote-backed (cached) ListWatchers feeds refreshWatcherNodes, which builds
// free-running watchers as top-level ◷ roots and attached watchers as children of
// their target session — the exact sidebar shape acceptance #1 requires.
func TestRefreshWatcherNodesWithRemoteHandlers(t *testing.T) {
	free := WatcherDTO{ID: "free1", Name: "emailer", Kind: "free", Target: "free"}
	attached := WatcherDTO{ID: "att1", Name: "gh", Kind: "attached", Target: "sess1"}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Query().Get("session_id") {
		case "sess1":
			_ = json.NewEncoder(w).Encode([]WatcherDTO{free, attached})
		default:
			_ = json.NewEncoder(w).Encode([]WatcherDTO{free}) // free-running only
		}
	}))
	defer srv.Close()

	c, _ := NewAPIClient(srv.URL, "")
	rc := NewRemoteClient(c, nil, nil)
	h := rc.Handlers()

	// Pre-warm both query keys so refreshWatcherNodes' reads hit the cache (a cold
	// cache returns nil and builds no nodes on the first tick).
	h.ListWatchers("")
	if !waitWatcherCache(rc, "", 1) {
		t.Fatal("free-set fetch did not land")
	}
	h.ListWatchers("sess1")
	if !waitWatcherCache(rc, "sess1", 2) {
		t.Fatal("session fetch did not land")
	}

	w := &Workbench{}
	w.handlers = h
	w.sidebar = newSidebar(w)
	// setWatchers attaches an attached watcher under its target session's node, so
	// sess1 must exist as a sidebar session (not just appear in w.order).
	w.sidebar.addSession("sess1", "Session 1", false)
	w.mu.Lock()
	w.order = []string{"sess1"}
	w.mu.Unlock()

	if !w.refreshWatcherNodes() {
		t.Fatal("refreshWatcherNodes reported no change, but watchers should have been added")
	}

	if n := w.sidebar.watchers["free1"]; n == nil {
		t.Fatal("free-running watcher free1 was not added as a sidebar node")
	} else if w.sidebar.watcherParents["free1"] != "" || !isTreeRoot(w.sidebar, n) {
		t.Errorf("free1 should be a top-level root, parent=%q", w.sidebar.watcherParents["free1"])
	}
	if n := w.sidebar.watchers["att1"]; n == nil {
		t.Fatal("attached watcher att1 was not added as a sidebar node")
	} else if w.sidebar.watcherParents["att1"] != "sess1" {
		t.Errorf("att1 should be a child of sess1, parent=%q", w.sidebar.watcherParents["att1"])
	}
}
