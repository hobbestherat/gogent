package model

// Round-2 tests for native-Gemini explicit context caching (issue #547): these
// verify the "fixes round 1" changes that addressed the round-1 trace findings —
// the >1h-gap expiry recreate, the reactive 4xx drop, ctx-cancel no longer
// stickily disabling, server expireTime parsing, the lowered 8192 threshold, and
// the conditional toolConfig on the cached branch — and probe their edge cases.
//
// A self-contained fake server (cacheSrv) is used so these tests can flip the
// refresh / generate status codes per scenario without touching the round-1
// suite. Pure helpers (largeCacheMessages, oneTool, bodyKeys, …) are reused from
// gemini_cache_issue547_test.go (same package).

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"gogent/internal/config"

	"golang.org/x/oauth2"
)

// ---------------------------------------------------------------------------
// Self-contained fake server with per-status knobs for refresh/generate
// ---------------------------------------------------------------------------

type cacheSrv struct {
	*httptest.Server

	mu                                     sync.Mutex
	creates, refreshes, deletes            int
	createStatus, refreshStatus, genStatus int
	nextID                                 int
	cachedRead                             int
	createExpire, refreshExpire            string
	genBody, streamBody                    []byte
}

func newCacheSrv(t *testing.T) *cacheSrv {
	t.Helper()
	s := &cacheSrv{
		createStatus:  http.StatusOK,
		refreshStatus: http.StatusOK,
		genStatus:     http.StatusOK,
		cachedRead:    999,
		createExpire:  "2035-01-01T00:00:00Z",
		refreshExpire: "2036-06-01T00:00:00Z",
	}
	s.Server = httptest.NewServer(http.HandlerFunc(s.handle))
	t.Cleanup(s.Close)
	return s
}

func (s *cacheSrv) handle(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "read: "+err.Error(), http.StatusInternalServerError)
		return
	}
	path := r.URL.Path
	switch {
	case strings.HasSuffix(path, "/cachedContents") && r.Method == http.MethodPost:
		s.mu.Lock()
		s.creates++
		id := s.nextID
		s.nextID++
		st := s.createStatus
		exp := s.createExpire
		s.mu.Unlock()
		if st != http.StatusOK {
			http.Error(w, `{"error":"create rejected"}`, st)
			return
		}
		name := fmt.Sprintf("projects/p/locations/us-central1/cachedContents/id-%d", id)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"name":"` + name + `","expireTime":"` + exp + `","model":"projects/p/locations/us-central1/publishers/google/models/gemini-2.5-flash"}`))
	case strings.Contains(path, "/cachedContents/") && r.Method == http.MethodPatch:
		s.mu.Lock()
		s.refreshes++
		st := s.refreshStatus
		exp := s.refreshExpire
		s.mu.Unlock()
		if st != http.StatusOK {
			http.Error(w, `{"error":"refresh rejected"}`, st)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"name":"` + strings.TrimPrefix(path, "/v1/") + `","expireTime":"` + exp + `"}`))
	case strings.Contains(path, "/cachedContents/") && r.Method == http.MethodDelete:
		s.mu.Lock()
		s.deletes++
		s.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	case strings.HasSuffix(path, ":generateContent"):
		s.mu.Lock()
		s.genBody = body
		st := s.genStatus
		cached := s.cachedRead
		s.mu.Unlock()
		if st != http.StatusOK {
			http.Error(w, `{"error":"generate rejected"}`, st)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(fmt.Sprintf(`{"candidates":[{"content":{"parts":[{"text":"ok"}],"role":"model"},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":1,"totalTokenCount":11,"cachedContentTokenCount":%d}}`, cached)))
	case strings.HasSuffix(path, ":streamGenerateContent"):
		s.mu.Lock()
		s.streamBody = body
		st := s.genStatus
		cached := s.cachedRead
		s.mu.Unlock()
		if st != http.StatusOK {
			http.Error(w, `{"error":"stream rejected"}`, st)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(fmt.Sprintf("data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"ok\"}],\"role\":\"model\"},\"finishReason\":\"STOP\"}],\"usageMetadata\":{\"promptTokenCount\":10,\"candidatesTokenCount\":1,\"totalTokenCount\":11,\"cachedContentTokenCount\":%d}}\n\n", cached)))
	default:
		http.Error(w, "unexpected "+r.URL.String(), http.StatusInternalServerError)
	}
}

func (s *cacheSrv) snapshot() (creates, refreshes, deletes int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.creates, s.refreshes, s.deletes
}

// setupCacheConn builds a vertex-native connection pointed at srv, faking ADC,
// forcing the native URL shape, and pinning a single attempt with negligible
// backoff so error-path tests are fast and deterministic.
func setupCacheConn(t *testing.T, srv *cacheSrv, cacheTTL string) *ModelConnection {
	t.Helper()
	withFakeADCTokenSource(t, func(ctx context.Context, _ ...string) (oauth2.TokenSource, error) {
		return &staticTokenSource{token: "tok"}, nil
	})
	conn := NewModelConnectionFromConfig(&config.ModelConfig{
		APIType: "vertex-native", Endpoint: srv.URL, Project: "p", Location: "us-central1",
		Model: "gemini-2.5-flash", CacheTTL: cacheTTL, MaxTokens: 64,
	})
	conn.URL = srv.URL + "/v1/projects/p/locations/us-central1/publishers/google/models/gemini-2.5-flash:generateContent"
	conn.StreamURL = srv.URL + "/v1/projects/p/locations/us-central1/publishers/google/models/gemini-2.5-flash:streamGenerateContent"
	conn.maxAttempts = 1
	conn.retryBaseDelay = time.Millisecond
	conn.retryMaxDelay = time.Millisecond
	return conn
}

// ===========================================================================
// Fix 1: an expired-and-unrevivable resource is RECREATED, not referenced
// (the >1h-gap wedge fix). The frozen prefix hash never changes, so without this
// the session would 400 forever across exactly the gap the feature bridges.
// ===========================================================================

func TestGeminiCacheRecreatesWhenExpiredResourceIsUnrevivable(t *testing.T) {
	srv := newCacheSrv(t)
	srv.refreshStatus = http.StatusNotFound // refresh 404s → resource gone
	conn := setupCacheConn(t, srv, "1h")

	ctx := context.Background()

	// Turn 1: create the resource. Its local expiry is the server's expireTime
	// (2035), so force it into the past to simulate the post-TTL state.
	req1 := CompletionRequest{Messages: largeCacheMessages()}
	conn.ensureGeminiCache(ctx, &req1)
	if req1.GeminiCachedContent == "" {
		t.Fatalf("turn1 did not create a cache")
	}
	name1 := req1.GeminiCachedContent

	conn.geminiCache.mu.Lock()
	conn.geminiCache.expiresAt = time.Now().Add(-time.Minute) // past expiry
	conn.geminiCache.mu.Unlock()

	// Turn 2 (after a >TTL gap): the resource is expired; refresh 404s. The manager
	// must drop the dead reference, delete it, and CREATE a fresh one — NOT re-send
	// the dead name (which would 400 the turn and wedge the session).
	req2 := CompletionRequest{Messages: largeCacheMessages()}
	conn.ensureGeminiCache(ctx, &req2)

	creates, refreshes, deletes := srv.snapshot()
	if creates != 2 {
		t.Errorf("creates = %d, want 2 (recreate after expired-refresh failure)", creates)
	}
	if refreshes != 1 {
		t.Errorf("refreshes = %d, want 1 (one attempted revive of the expired resource)", refreshes)
	}
	if deletes != 1 {
		t.Errorf("deletes = %d, want 1 (dead resource deleted before recreate)", deletes)
	}
	if req2.GeminiCachedContent == "" {
		t.Fatalf("turn2 did not reference the recreated cache")
	}
	if req2.GeminiCachedContent == name1 {
		t.Errorf("turn2 referenced the DEAD name %q; want a freshly-created name", name1)
	}
	if req2.GeminiCachedPrefixContents != 2 {
		t.Errorf("turn2 prefix len = %d, want 2", req2.GeminiCachedPrefixContents)
	}
}

// A transient refresh failure while the resource is still comfortably WITHIN its
// TTL must NOT drop the reference (the resource is presumed live; the next turn
// retries the refresh). Only past-expiry-and-unrevivable triggers recreate.
func TestGeminiCacheKeepsReferenceOnTransientRefreshFailureBeforeExpiry(t *testing.T) {
	srv := newCacheSrv(t)
	srv.refreshStatus = http.StatusNotFound // refresh fails
	conn := setupCacheConn(t, srv, "1h")
	ctx := context.Background()

	req1 := CompletionRequest{Messages: largeCacheMessages()}
	conn.ensureGeminiCache(ctx, &req1)
	name1 := req1.GeminiCachedContent

	// Near (but BEFORE) expiry: inside the refresh window, but not yet past TTL.
	conn.geminiCache.mu.Lock()
	conn.geminiCache.expiresAt = time.Now().Add(2 * time.Minute) // within last 25% of 1h, but > now
	conn.geminiCache.mu.Unlock()

	req2 := CompletionRequest{Messages: largeCacheMessages()}
	conn.ensureGeminiCache(ctx, &req2)
	creates, refreshes, deletes := srv.snapshot()
	if creates != 1 || deletes != 0 {
		t.Errorf("transient pre-expiry refresh failure: creates=%d deletes=%d, want 1/0 (keep live reference)", creates, deletes)
	}
	if refreshes != 1 {
		t.Errorf("refreshes = %d, want 1", refreshes)
	}
	if req2.GeminiCachedContent != name1 {
		t.Errorf("referenced %q, want kept %q", req2.GeminiCachedContent, name1)
	}
}

// ===========================================================================
// Fix 2: reactive 4xx drop clears a referenced cache the server rejects, and the
// session self-heals (next turn recreates). End-to-end through complete().
// ===========================================================================

func TestGeminiCacheReactiveDropOn4xxClearsAndSelfHeals(t *testing.T) {
	srv := newCacheSrv(t)
	conn := setupCacheConn(t, srv, "1h")
	ctx := context.Background()

	// Turn 1: cache created, completion succeeds.
	if _, err := conn.CompleteWithToolsCtx(ctx, largeCacheMessages(), oneTool("t")); err != nil {
		t.Fatalf("turn1 Complete: %v", err)
	}
	conn.geminiCache.mu.Lock()
	name1 := conn.geminiCache.name
	conn.geminiCache.mu.Unlock()
	if name1 == "" {
		t.Fatalf("turn1 did not create a cache")
	}

	// Turn 2: the server rejects the referenced cache (400). complete() must clear
	// the reference so the next turn recreates instead of 400ing forever.
	srv.mu.Lock()
	srv.genStatus = http.StatusBadRequest
	srv.mu.Unlock()
	resp2, err := conn.CompleteWithToolsCtx(ctx, largeCacheMessages(), oneTool("t"))
	if err == nil {
		t.Fatalf("turn2: expected 400 error, got resp=%+v", resp2)
	}
	conn.geminiCache.mu.Lock()
	nameAfterDrop := conn.geminiCache.name
	conn.geminiCache.mu.Unlock()
	if nameAfterDrop != "" {
		t.Errorf("after 4xx on a referenced cache, name = %q, want empty (reactive drop)", nameAfterDrop)
	}

	// Turn 3: self-heal — recreate and succeed.
	srv.mu.Lock()
	srv.genStatus = http.StatusOK
	srv.mu.Unlock()
	if _, err := conn.CompleteWithToolsCtx(ctx, largeCacheMessages(), oneTool("t")); err != nil {
		t.Fatalf("turn3 self-heal Complete: %v", err)
	}
	creates, _, _ := srv.snapshot()
	if creates != 2 {
		t.Errorf("creates = %d, want 2 (id-0 turn1, id-1 after reactive drop)", creates)
	}
	conn.geminiCache.mu.Lock()
	name3 := conn.geminiCache.name
	conn.geminiCache.mu.Unlock()
	if name3 == "" || name3 == name1 {
		t.Errorf("turn3 name = %q, want a NEW non-empty name (recreated)", name3)
	}
}

// The reactive drop also fires on the streaming path.
func TestGeminiCacheReactiveDropOnStream4xx(t *testing.T) {
	srv := newCacheSrv(t)
	conn := setupCacheConn(t, srv, "1h")
	ctx := context.Background()

	// Establish a live cache via a successful stream turn.
	if _, err := conn.CompleteWithToolsStreamCtx(ctx, largeCacheMessages(), oneTool("t"), nil); err != nil {
		t.Fatalf("stream turn1: %v", err)
	}
	conn.geminiCache.mu.Lock()
	before := conn.geminiCache.name
	conn.geminiCache.mu.Unlock()
	if before == "" {
		t.Fatalf("no live cache reference after stream turn1")
	}

	// Now the server rejects the referenced cache (400). The streaming path must
	// clear the reference (completeStream calls dropGeminiCacheRefAfterError on a
	// non-200 before returning).
	srv.mu.Lock()
	srv.genStatus = http.StatusBadRequest
	srv.mu.Unlock()
	if _, err := conn.CompleteWithToolsStreamCtx(ctx, largeCacheMessages(), oneTool("t"), nil); err == nil {
		t.Fatalf("expected stream 4xx error")
	}
	conn.geminiCache.mu.Lock()
	after := conn.geminiCache.name
	conn.geminiCache.mu.Unlock()
	if after != "" {
		t.Errorf("stream 4xx did not clear the cache reference: name=%q, want empty", after)
	}
}

// dropGeminiCacheRefAfterError guard: a 5xx is transient and must NOT clear; a
// non-referenced request is a no-op.
func TestGeminiCacheReactiveDropGuard(t *testing.T) {
	conn := NewModelConnection()

	set := func() {
		conn.geminiCache.mu.Lock()
		conn.geminiCache.name = "projects/p/locations/l/cachedContents/live"
		conn.geminiCache.prefixHash = "h"
		conn.geminiCache.prefixLen = 2
		conn.geminiCache.mu.Unlock()
	}
	clear := func() bool {
		conn.geminiCache.mu.Lock()
		defer conn.geminiCache.mu.Unlock()
		return conn.geminiCache.name == ""
	}

	set()
	conn.dropGeminiCacheRefAfterError(false, http.StatusBadRequest) // not referenced
	if clear() {
		t.Error("unreferenced 400 cleared the cache; want no-op")
	}
	set()
	conn.dropGeminiCacheRefAfterError(true, http.StatusBadGateway) // 502 → 5xx
	if clear() {
		t.Error("5xx cleared the cache; want no-op (transient, retried)")
	}
	set()
	conn.dropGeminiCacheRefAfterError(true, http.StatusNotFound) // 404 → cache gone
	if !clear() {
		t.Error("404 did not clear the cache; want cleared (reference rejected)")
	}
	set()
	conn.dropGeminiCacheRefAfterError(true, http.StatusBadRequest) // 400
	if !clear() {
		t.Error("400 did not clear the cache; want cleared")
	}
}

// DEFECT (documented): the reactive-drop guard is `status >= 400 && status < 500`,
// which also matches the RETRYABLE 4xx codes 408/409/429. After those exhaust
// retries (immediately, with no retry, on the streaming path), a transient
// rate-limit/conflict drops the cache reference and forces an unnecessary
// recreate next turn — adding load during rate-limiting, the opposite of ideal,
// and contradicting the method's own docstring ("transient 5xx/429 ... need no
// invalidation"). The guard should also exclude 408/409/429 (e.g. gate on
// `!isRetryableStatus(status)`). This test pins the CURRENT behavior so the suite
// stays green; flip the expectation when the guard is narrowed.
func TestGeminiCacheReactiveDropGuardOverbroadOnRetryable4xx(t *testing.T) {
	conn := NewModelConnection()
	conn.geminiCache.mu.Lock()
	conn.geminiCache.name = "projects/p/locations/l/cachedContents/live"
	conn.geminiCache.mu.Unlock()

	for _, code := range []int{http.StatusTooManyRequests, http.StatusRequestTimeout, http.StatusConflict} {
		conn.geminiCache.mu.Lock()
		conn.geminiCache.name = "projects/p/locations/l/cachedContents/live"
		conn.geminiCache.mu.Unlock()
		conn.dropGeminiCacheRefAfterError(true, code)
		conn.geminiCache.mu.Lock()
		gone := conn.geminiCache.name == ""
		conn.geminiCache.mu.Unlock()
		t.Logf("retryable %d: cache reference dropped = %v (docstring says it should NOT invalidate)", code, gone)
		if !gone {
			t.Errorf("status %d: expected CURRENT (buggy) behavior to drop the reference; if this now passes, the guard was fixed — update this test", code)
		}
	}
}

// ===========================================================================
// Fix 3: a cancelled context during create must NOT stickily disable caching.
// ===========================================================================

func TestGeminiCacheCtxCancelDoesNotDisable(t *testing.T) {
	srv := newCacheSrv(t)
	conn := setupCacheConn(t, srv, "1h")

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled: the create's HTTP call aborts before sending

	req := CompletionRequest{Messages: largeCacheMessages()}
	conn.ensureGeminiCache(ctx, &req)

	conn.geminiCache.mu.Lock()
	disabled := conn.geminiCache.disabled
	conn.geminiCache.mu.Unlock()
	if disabled {
		t.Fatalf("a cancelled context disabled explicit caching for the session; want transient (retry next turn)")
	}
	if req.GeminiCachedContent != "" {
		t.Errorf("cancelled create set GeminiCachedContent=%q, want empty", req.GeminiCachedContent)
	}
	if creates, _, _ := srv.snapshot(); creates != 0 {
		t.Errorf("cancelled create reached the server (creates=%d); want 0", creates)
	}

	// A subsequent NON-cancelled turn must still be able to create.
	req2 := CompletionRequest{Messages: largeCacheMessages()}
	conn.ensureGeminiCache(context.Background(), &req2)
	if creates, _, _ := srv.snapshot(); creates != 1 {
		t.Errorf("after ctx-cancel, creates = %d, want 1 (cancellation must not permanently disable)", creates)
	}
	if req2.GeminiCachedContent == "" {
		t.Errorf("post-cancel turn did not create a cache (disabled stuck?)")
	}
}

// A GENUINE create failure (server error, ctx healthy) still disables for the
// connection's life — the fix only exempts cancellation.
func TestGeminiCacheGenuineCreateFailureStillDisables(t *testing.T) {
	srv := newCacheSrv(t)
	srv.createStatus = http.StatusInternalServerError
	conn := setupCacheConn(t, srv, "1h")

	req := CompletionRequest{Messages: largeCacheMessages()}
	conn.ensureGeminiCache(context.Background(), &req)
	conn.geminiCache.mu.Lock()
	disabled := conn.geminiCache.disabled
	conn.geminiCache.mu.Unlock()
	if !disabled {
		t.Errorf("genuine 500 create did not disable; want disabled (avoid retrying a doomed create every turn)")
	}
	if req.GeminiCachedContent != "" {
		t.Errorf("failed create set GeminiCachedContent=%q, want empty (fall back to full prefix)", req.GeminiCachedContent)
	}
}

// ===========================================================================
// Fix 4: the server's authoritative expireTime is honored (Vertex may cap TTL).
// ===========================================================================

func TestGeminiCacheExpiryHelper(t *testing.T) {
	// Authoritative server time is used verbatim.
	got := geminiCacheExpiry("2035-01-01T00:00:00Z", time.Hour)
	want := time.Date(2035, 1, 1, 0, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Errorf("geminiCacheExpiry(2035...) = %v, want %v", got, want)
	}
	// Empty / unparseable → now+ttl fallback.
	fallback := geminiCacheExpiry("", time.Hour)
	now := time.Now()
	if fallback.Before(now) || fallback.After(now.Add(2*time.Hour)) {
		t.Errorf("geminiCacheExpiry('') = %v, want ~now+1h", fallback)
	}
	fallback2 := geminiCacheExpiry("not-rfc3339", time.Hour)
	if fallback2.Before(now) || fallback2.After(now.Add(2*time.Hour)) {
		t.Errorf("geminiCacheExpiry('not-rfc3339') = %v, want ~now+1h", fallback2)
	}
}

func TestGeminiCacheCreateAndRefreshUseServerExpireTime(t *testing.T) {
	srv := newCacheSrv(t)
	conn := setupCacheConn(t, srv, "1h")
	ctx := context.Background()

	// Create returns the server's expireTime (2035-01-01); the local estimate must
	// reflect THAT, not now+1h, so a Vertex TTL cap doesn't drift the refresh.
	req := CompletionRequest{Messages: largeCacheMessages()}
	conn.ensureGeminiCache(ctx, &req)
	conn.geminiCache.mu.Lock()
	exp := conn.geminiCache.expiresAt
	conn.geminiCache.mu.Unlock()
	want := time.Date(2035, 1, 1, 0, 0, 0, 0, time.UTC)
	if !exp.Equal(want) {
		t.Errorf("after create, expiresAt = %v, want server expireTime %v", exp, want)
	}

	// A successful refresh updates to the refresh response's expireTime (2036-06-01).
	conn.geminiCache.mu.Lock()
	conn.geminiCache.expiresAt = time.Now().Add(-time.Minute) // force the refresh window
	conn.geminiCache.mu.Unlock()
	req2 := CompletionRequest{Messages: largeCacheMessages()}
	conn.ensureGeminiCache(ctx, &req2)
	conn.geminiCache.mu.Lock()
	exp2 := conn.geminiCache.expiresAt
	conn.geminiCache.mu.Unlock()
	want2 := time.Date(2036, 6, 1, 0, 0, 0, 0, time.UTC)
	if !exp2.Equal(want2) {
		t.Errorf("after refresh, expiresAt = %v, want server expireTime %v", exp2, want2)
	}
	if _, refreshes, _ := srv.snapshot(); refreshes != 1 {
		t.Errorf("refreshes = %d, want 1", refreshes)
	}
}

// ===========================================================================
// Fix 5: the threshold floor was lowered 32768 → 8192 so the feature engages for
// genuinely large prefixes instead of being a silent no-op at the default 32k
// context window (where compaction fires first).
// ===========================================================================

func TestGeminiCacheThresholdLoweredToEngageForLargePrefixes(t *testing.T) {
	if geminiMinCacheTokens != 8192 {
		t.Fatalf("geminiMinCacheTokens = %d, want 8192 (lowered from 32768 which equalled defaultContextWindow)", geminiMinCacheTokens)
	}
	srv := newCacheSrv(t)
	conn := setupCacheConn(t, srv, "1h")
	ctx := context.Background()

	// ~50k-char prefix: marshals to ~50k bytes → ~12.5k estimated tokens, ABOVE the
	// new 8192 floor (and well BELOW the old 32768 floor that needed ~128k bytes).
	// Engages now; would have been a silent no-op before the fix.
	big := []Message{
		{Role: RoleUser, Content: strings.Repeat("x", 50000)},
		{Role: RoleAssistant, Content: "ack"},
	}
	conn.ensureGeminiCache(ctx, &CompletionRequest{Messages: big})
	if creates, _, _ := srv.snapshot(); creates != 1 {
		t.Errorf("50k-char prefix: creates = %d, want 1 (engages at the lowered threshold)", creates)
	}

	// ~10k-char prefix (~2.5k tokens) is still below the floor → no cache.
	small := []Message{
		{Role: RoleUser, Content: strings.Repeat("y", 10000)},
		{Role: RoleAssistant, Content: "ack"},
	}
	conn2 := setupCacheConn(t, newCacheSrv(t), "1h")
	conn2.ensureGeminiCache(ctx, &CompletionRequest{Messages: small})
	// (conn2 points at its own server; just assert no reference was set.)
	if conn2.geminiCache.name != "" {
		t.Errorf("10k-char prefix engaged caching; want below the 8192-token floor")
	}
}

// ===========================================================================
// Fix (adapter): a non-default tool_choice (ANY/NONE/specific) is preserved on
// the cached branch so caching cannot silently drop a forcing directive. AUTO
// (Gemini's default) is still omitted to keep the referencing request minimal.
// ===========================================================================

func TestGeminiBuildBodyCachedPreservesNonAutoToolChoice(t *testing.T) {
	mk := func(mode ToolChoiceMode) []byte {
		req := CompletionRequest{
			Messages: []Message{
				{Role: RoleUser, Content: "U1"},
				{Role: RoleAssistant, Content: "A1"},
				{Role: RoleUser, Content: "U2"},
			},
			Tools:      oneTool("t"),
			ToolChoice: &ToolChoice{Mode: mode},
		}
		req.GeminiCachedContent = "projects/p/locations/l/cachedContents/x"
		req.GeminiCachedPrefixContents = 2
		raw, err := buildBodyBytes(geminiAdapter{}, req)
		if err != nil {
			t.Fatalf("buildBody: %v", err)
		}
		return raw
	}

	t.Run("required_emits_toolConfig", func(t *testing.T) {
		m := bodyKeys(t, mk(ToolChoiceRequired))
		assertKeyPresent(t, m, "cachedContent")
		assertKeyAbsent(t, m, "tools") // tools stay shadowed by the resource
		assertKeyPresent(t, m, "toolConfig")
	})
	t.Run("auto_omits_toolConfig", func(t *testing.T) {
		m := bodyKeys(t, mk(ToolChoiceAuto))
		assertKeyPresent(t, m, "cachedContent")
		assertKeyAbsent(t, m, "toolConfig")
	})
}
