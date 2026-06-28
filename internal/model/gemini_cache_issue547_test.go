package model

// Tests for native-Gemini explicit context caching (issue #547): the
// CachedContent lifecycle (internal/model/gemini_cache.go), the wire-format
// shadowing in geminiAdapter.buildBody, the GeminiCacheTTL config helper, and the
// call-site wiring in complete()/completeStream().
//
// These tests are written to exercise the four design gates:
//   - gate 1 (goal match): create/reference/refresh/delete lifecycle; opt-in +
//     heuristic-gated; reads surface via CacheStats.
//   - gate 2 (usability): large persistent prefixes cached; small/volatile
//     prefixes untouched; no surprise storage cost (off-by-default + threshold).
//   - gate 3 (no regressions): inactive path emits no cachedContent and shadows
//     nothing; implicit-cache reporting unchanged.
//   - gate 4 (holistic): gogent-only, reuses #544 capability + #545 CacheTTL.
//
// The headline scenario under test (ReuseAcrossGrownTranscript) is the one the
// design-review flagged: a cached prefix must be REUSED — not recreated every
// turn — as the transcript grows, because the frozen prefix slice is stable.

import (
	"context"
	"encoding/json"
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
// Small shared builders
// ---------------------------------------------------------------------------

// largeCacheMessages returns a transcript whose cached prefix (user + assistant
// turn) marshals well above geminiMinCacheTokens (32768 tokens ≈ 131072 bytes),
// so the size gate engages. A fresh slice is returned each call so callers can
// safely append to grow the transcript.
func largeCacheMessages() []Message {
	return []Message{
		{Role: RoleUser, Content: strings.Repeat("x", 220000)},
		{Role: RoleAssistant, Content: "ack"},
	}
}

// smallCacheMessages is the same shape but far below the size gate.
func smallCacheMessages() []Message {
	return []Message{
		{Role: RoleUser, Content: "hello"},
		{Role: RoleAssistant, Content: "hi"},
	}
}

func withSystem(msgs []Message, sys string) []Message {
	out := make([]Message, 0, len(msgs)+1)
	out = append(out, Message{Role: RoleSystem, Content: sys})
	out = append(out, msgs...)
	return out
}

func geminiObjectParams() map[string]interface{} {
	return map[string]interface{}{"type": "object", "properties": map[string]interface{}{}}
}

func ptrFloat32(v float32) *float32 { return &v }

// oneTool builds a single non-strict function tool with the given name.
func oneTool(name string) []ToolDef {
	return []ToolDef{{
		Type: "function",
		Function: FunctionDef{
			Name:        name,
			Description: name + " tool",
			Parameters:  geminiObjectParams(),
		},
	}}
}

// ---------------------------------------------------------------------------
// Fake cachedContents / generateContent server
// ---------------------------------------------------------------------------

// geminiCacheServer is an httptest server that implements the subset of the
// Vertex surface the cache lifecycle touches: POST /cachedContents (create),
// PATCH /cachedContents/{id}?updateMask=ttl (refresh), DELETE
// /cachedContents/{id} (delete), and the generateContent / streamGenerateContent
// completion routes (for the end-to-end wiring tests). createStatus lets a test
// force a create failure.
type geminiCacheServer struct {
	*httptest.Server

	mu                   sync.Mutex
	creates, refreshes   int
	deletes              int
	createStatus         int
	nextID               int
	lastCreateBody       []byte
	lastCreatePath       string
	lastRefreshPath      string
	lastRefreshBody      []byte
	lastDeletePath       string
	genBody              []byte
	streamBody           []byte
	cachedReadTokenCount int // reported back in usageMetadata, for the wiring test
}

func newGeminiCacheServer(t *testing.T, createStatus int) *geminiCacheServer {
	t.Helper()
	s := &geminiCacheServer{createStatus: createStatus, cachedReadTokenCount: 999}
	if s.createStatus == 0 {
		s.createStatus = http.StatusOK
	}
	s.Server = httptest.NewServer(http.HandlerFunc(s.handle))
	t.Cleanup(s.Close)
	return s
}

func (s *geminiCacheServer) handle(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "read body: "+err.Error(), http.StatusInternalServerError)
		return
	}
	path := r.URL.Path
	switch {
	case strings.HasSuffix(path, "/cachedContents") && r.Method == http.MethodPost:
		s.mu.Lock()
		s.creates++
		s.lastCreateBody = body
		s.lastCreatePath = path
		id := s.nextID
		s.nextID++
		st := s.createStatus
		s.mu.Unlock()
		if st != http.StatusOK {
			http.Error(w, `{"error":"create rejected"}`, st)
			return
		}
		name := fmt.Sprintf("projects/p/locations/us-central1/cachedContents/id-%d", id)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"name":"` + name + `","expireTime":"2035-01-01T00:00:00Z","model":"projects/p/locations/us-central1/publishers/google/models/gemini-2.5-flash"}`))
	case strings.Contains(path, "/cachedContents/") && r.Method == http.MethodPatch:
		s.mu.Lock()
		s.refreshes++
		s.lastRefreshPath = r.URL.String()
		s.lastRefreshBody = body
		s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"name":"` + strings.TrimPrefix(path, "/v1/") + `","expireTime":"2035-01-01T00:00:00Z"}`))
	case strings.Contains(path, "/cachedContents/") && r.Method == http.MethodDelete:
		s.mu.Lock()
		s.deletes++
		s.lastDeletePath = path
		s.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	case strings.HasSuffix(path, ":generateContent"):
		s.mu.Lock()
		s.genBody = body
		cached := s.cachedReadTokenCount
		s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"candidates":[{"content":{"parts":[{"text":"ok"}],"role":"model"},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":1,"totalTokenCount":11,"cachedContentTokenCount":%d}}`, cached)
	case strings.HasSuffix(path, ":streamGenerateContent"):
		s.mu.Lock()
		s.streamBody = body
		cached := s.cachedReadTokenCount
		s.mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprintf(w, "data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"ok\"}],\"role\":\"model\"},\"finishReason\":\"STOP\"}],\"usageMetadata\":{\"promptTokenCount\":10,\"candidatesTokenCount\":1,\"totalTokenCount\":11,\"cachedContentTokenCount\":%d}}\n\n", cached)
	default:
		http.Error(w, "unexpected request "+r.URL.String(), http.StatusInternalServerError)
	}
}

func (s *geminiCacheServer) snapshot() (creates, refreshes, deletes int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.creates, s.refreshes, s.deletes
}

func (s *geminiCacheServer) lastCreateBodyCopy() []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]byte, len(s.lastCreateBody))
	copy(out, s.lastCreateBody)
	return out
}

// setupGeminiCacheTest wires a vertex-native connection against the fake server,
// faking ADC so the connection's ADC round-tripper can talk to the plain-HTTP
// test server. The connection's URL is forced into the native
// /v1/projects/{p}/locations/{l}/publishers/google/models/{m}:generateContent
// shape so geminiCacheEndpoint can derive both the collection URL and the
// publisher-qualified model path.
func setupGeminiCacheTest(t *testing.T, cacheTTL string, createStatus int) (*ModelConnection, *geminiCacheServer) {
	t.Helper()
	withFakeADCTokenSource(t, func(ctx context.Context, _ ...string) (oauth2.TokenSource, error) {
		return &staticTokenSource{token: "test-token"}, nil
	})
	srv := newGeminiCacheServer(t, createStatus)
	conn := NewModelConnectionFromConfig(&config.ModelConfig{
		APIType:   "vertex-native",
		Endpoint:  srv.URL,
		Project:   "p",
		Location:  "us-central1",
		Model:     "gemini-2.5-flash",
		CacheTTL:  cacheTTL,
		MaxTokens: 64,
	})
	conn.URL = srv.URL + "/v1/projects/p/locations/us-central1/publishers/google/models/gemini-2.5-flash:generateContent"
	return conn, srv
}

// ---------------------------------------------------------------------------
// Gate-focus helpers: assert a request body carries (or omits) a JSON key
// ---------------------------------------------------------------------------

func bodyKeys(t *testing.T, raw []byte) map[string]interface{} {
	t.Helper()
	var m map[string]interface{}
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unmarshal body %q: %v", string(raw), err)
	}
	return m
}

func assertKeyPresent(t *testing.T, m map[string]interface{}, key string) {
	t.Helper()
	if _, ok := m[key]; !ok {
		t.Errorf("expected %q to be present in body; have keys %v", key, keysOf(m))
	}
}

func assertKeyAbsent(t *testing.T, m map[string]interface{}, key string) {
	t.Helper()
	if _, ok := m[key]; ok {
		t.Errorf("expected %q to be ABSENT from body (it must be shadowed by the cache)", key)
	}
}

func keysOf(m map[string]interface{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// ===========================================================================
// 1. Config: GeminiCacheTTL resolution (gate 1, gate 2 opt-in/off-by-default)
// ===========================================================================

func TestGeminiCacheTTLResolution(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		// Off-by-default and explicit disables → "" (no resource created).
		{"", ""},
		{"off", ""},
		{"none", ""},
		{"disabled", ""},
		// Positive durations → Gemini ttl string "<seconds>s".
		{"1h", "3600s"},
		{"5m", "300s"},
		{"30m", "1800s"},
		{"90s", "90s"},
		{"2h", "7200s"},
		{"1.5h", "5400s"},
		{"45m30s", "2730s"},
		// Unparseable / non-positive → disabled (never create a billable resource).
		{"garbage", ""},
		{"-1h", ""},
		{"0s", ""},
		{"0", ""}, // ParseDuration("0") fails → disabled
		// Tolerant: whitespace trimmed, case-insensitive.
		{"  1h  ", "3600s"},
		{"1H", "3600s"},
		{"5M", "300s"},
	}
	for _, tc := range cases {
		got := (&config.ModelConfig{CacheTTL: tc.in}).GeminiCacheTTL()
		if got != tc.want {
			t.Errorf("GeminiCacheTTL(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}

	// nil receiver must not panic and must read as disabled.
	var nilCfg *config.ModelConfig
	if got := nilCfg.GeminiCacheTTL(); got != "" {
		t.Errorf("nil GeminiCacheTTL() = %q, want \"\"", got)
	}
}

// TestGeminiCacheTTLIsOptInWhileAnthropicIsDefaultOn documents the deliberate
// dual semantics of the shared CacheTTL field (a gate-2 usability property): for
// Anthropic an empty CacheTTL keeps caching ON (5m default), whereas for Gemini
// an empty CacheTTL means OFF — a billable CachedContent is never created unless
// the user asked.
func TestGeminiCacheTTLIsOptInWhileAnthropicIsDefaultOn(t *testing.T) {
	cfg := &config.ModelConfig{CacheTTL: ""}

	// Gemini: empty ⇒ off (no ttl string).
	if g := cfg.GeminiCacheTTL(); g != "" {
		t.Errorf("Gemini empty CacheTTL = %q, want \"\" (off)", g)
	}
	// Anthropic: empty ⇒ default-on sentinel "" (which the adapter treats as the
	// 5m ephemeral cache, i.e. ON). Both return "" here, but for opposite reasons;
	// the distinguishing test is that a set value enables both.
	cfg.CacheTTL = "1h"
	if g, a := cfg.GeminiCacheTTL(), cfg.AnthropicCacheTTL(); g != "3600s" || a != "1h" {
		t.Errorf("CacheTTL=1h: gemini=%q anthropic=%q; want 3600s / 1h", g, a)
	}
}

// ===========================================================================
// 2. Adapter wire format (gate 1: reference + omit shadowed; gate 3: inactive
//    byte-identical — no cachedContent key)
// ===========================================================================

func TestGeminiBuildBodyInactiveEmitsNoCachedContent(t *testing.T) {
	// Inactive path (no GeminiCachedContent): the request must marshal with no
	// "cachedContent" key at all, and system/tools/contents present as today.
	req := CompletionRequest{
		Messages: []Message{
			{Role: RoleSystem, Content: "S"},
			{Role: RoleUser, Content: "U1"},
			{Role: RoleAssistant, Content: "A1"},
			{Role: RoleUser, Content: "U2"},
		},
		Tools:       oneTool("t"),
		ToolChoice:  &ToolChoice{Mode: ToolChoiceAuto},
		Temperature: ptrFloat32(0.5),
	}
	raw, err := buildBodyBytes(geminiAdapter{}, req)
	if err != nil {
		t.Fatalf("buildBody: %v", err)
	}
	m := bodyKeys(t, raw)
	assertKeyAbsent(t, m, "cachedContent")
	assertKeyPresent(t, m, "systemInstruction")
	assertKeyPresent(t, m, "tools")
	if cs, _ := m["contents"].([]interface{}); len(cs) != 3 {
		t.Errorf("inactive contents len = %d, want 3 (full transcript)", len(cs))
	}
}

func TestGeminiBuildBodyActiveReferencesCacheAndOmitsShadowedPrefix(t *testing.T) {
	// merged contents = [user(U1), model(A1), user(U2)]. Reference a cache that
	// shadows the first 2 (systemInstruction + tools + U1 + A1); the request must
	// emit only the post-snapshot tail [user(U2)] and MUST NOT re-send system,
	// tools, or toolConfig (Vertex rejects re-declaring tools alongside a cache).
	const resName = "projects/p/locations/us-central1/cachedContents/abc"
	req := CompletionRequest{
		Messages: []Message{
			{Role: RoleSystem, Content: "S"},
			{Role: RoleUser, Content: "U1"},
			{Role: RoleAssistant, Content: "A1"},
			{Role: RoleUser, Content: "U2"},
		},
		Tools:       oneTool("t"),
		ToolChoice:  &ToolChoice{Mode: ToolChoiceAuto},
		Temperature: ptrFloat32(0.5),
	}
	req.GeminiCachedContent = resName
	req.GeminiCachedPrefixContents = 2

	raw, err := buildBodyBytes(geminiAdapter{}, req)
	if err != nil {
		t.Fatalf("buildBody: %v", err)
	}
	m := bodyKeys(t, raw)
	if got := m["cachedContent"]; got != resName {
		t.Errorf("cachedContent = %v, want %q", got, resName)
	}
	// Everything shadowed by the resource is omitted.
	assertKeyAbsent(t, m, "systemInstruction")
	assertKeyAbsent(t, m, "tools")
	assertKeyAbsent(t, m, "toolConfig") // documented narrowing: tool_choice is dropped on the cached branch
	// Only the tail is sent.
	cs, ok := m["contents"].([]interface{})
	if !ok || len(cs) != 1 {
		t.Fatalf("contents = %v, want a 1-element tail (only U2)", m["contents"])
	}
	first := cs[0].(map[string]interface{})
	if first["role"] != "user" {
		t.Errorf("tail role = %v, want user (seam must stay role-alternating)", first["role"])
	}
	// generationConfig is per-request and NOT cached — it must still be emitted so
	// sampling/limits apply on the cached branch.
	assertKeyPresent(t, m, "generationConfig")
}

func TestGeminiBuildBodyActiveClampsPrefixContentsIndex(t *testing.T) {
	// Defensive slicing: GeminiCachedPrefixContents out of range / negative must
	// never panic and must clamp, not slice past the available contents.
	mk := func(n int) CompletionRequest {
		req := CompletionRequest{
			Messages: []Message{
				{Role: RoleUser, Content: "U1"},
				{Role: RoleAssistant, Content: "A1"},
				{Role: RoleUser, Content: "U2"},
			},
		}
		req.GeminiCachedContent = "projects/p/locations/l/cachedContents/x"
		req.GeminiCachedPrefixContents = n
		return req
	}

	t.Run("overlarge_clamps_to_all_shadowed", func(t *testing.T) {
		raw, err := buildBodyBytes(geminiAdapter{}, mk(99))
		if err != nil {
			t.Fatalf("buildBody: %v", err)
		}
		m := bodyKeys(t, raw)
		if cs, _ := m["contents"].([]interface{}); len(cs) != 0 {
			t.Errorf("overlarge N: contents len = %d, want 0 (everything shadowed)", len(cs))
		}
	})
	t.Run("negative_clamps_to_zero", func(t *testing.T) {
		raw, err := buildBodyBytes(geminiAdapter{}, mk(-3))
		if err != nil {
			t.Fatalf("buildBody: %v", err)
		}
		m := bodyKeys(t, raw)
		// system/tools still omitted because a cache IS referenced; all contents sent.
		assertKeyAbsent(t, m, "systemInstruction")
		assertKeyAbsent(t, m, "tools")
		if cs, _ := m["contents"].([]interface{}); len(cs) != 3 {
			t.Errorf("negative N: contents len = %d, want 3", len(cs))
		}
	})
}

// ===========================================================================
// 3. Stable-prefix boundary computation (gate 1: cacheable prefix shape)
// ===========================================================================

func TestGeminiStablePrefixBoundary(t *testing.T) {
	cases := []struct {
		name     string
		contents []geminiContent
		want     int
	}{
		{"empty", nil, 0},
		{"user_only_no_model", []geminiContent{{Role: "user"}}, 0},
		{"user_then_model", []geminiContent{{Role: "user"}, {Role: "model"}}, 2},
		{"user_model_user", []geminiContent{{Role: "user"}, {Role: "model"}, {Role: "user"}}, 2},
		{"user_model_user_model", []geminiContent{{Role: "user"}, {Role: "model"}, {Role: "user"}, {Role: "model"}}, 4},
		{"model_first", []geminiContent{{Role: "model"}, {Role: "user"}}, 1},
		{"function_role_is_not_a_cache_endpoint", []geminiContent{{Role: "user"}, {Role: "function"}, {Role: "model"}}, 3},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := geminiStablePrefixBoundary(tc.contents); got != tc.want {
				t.Errorf("geminiStablePrefixBoundary = %d, want %d", got, tc.want)
			}
		})
	}
}

// ===========================================================================
// 4. URL derivation (gate 1: correct cachedContents collection + resource URLs)
// ===========================================================================

func TestGeminiCacheEndpointDerivation(t *testing.T) {
	conn := NewModelConnection()
	const chatURL = "https://us-central1-aiplatform.googleapis.com/v1/projects/myproj/locations/us-central1/publishers/google/models/gemini-2.5-flash:generateContent"
	conn.URL = chatURL

	t.Run("collection_and_model_path", func(t *testing.T) {
		coll, modelPath, ok := geminiCacheEndpoint(conn)
		if !ok {
			t.Fatalf("geminiCacheEndpoint ok = false, want true for native URL")
		}
		wantColl := "https://us-central1-aiplatform.googleapis.com/v1/projects/myproj/locations/us-central1/cachedContents"
		if coll != wantColl {
			t.Errorf("collection URL = %q, want %q", coll, wantColl)
		}
		wantModel := "projects/myproj/locations/us-central1/publishers/google/models/gemini-2.5-flash"
		if modelPath != wantModel {
			t.Errorf("model path = %q, want %q", modelPath, wantModel)
		}
	})

	t.Run("resource_url_for_refresh_and_delete", func(t *testing.T) {
		resURL, ok := geminiCacheResourceURL(conn, "projects/myproj/locations/us-central1/cachedContents/abc")
		if !ok {
			t.Fatalf("geminiCacheResourceURL ok = false, want true")
		}
		want := "https://us-central1-aiplatform.googleapis.com/v1/projects/myproj/locations/us-central1/cachedContents/abc"
		if resURL != want {
			t.Errorf("resource URL = %q, want %q", resURL, want)
		}
	})

	t.Run("rejects_url_without_publishers_segment", func(t *testing.T) {
		c := NewModelConnection()
		c.URL = "https://host/v1/projects/p/locations/l/foo:generateContent"
		if _, _, ok := geminiCacheEndpoint(c); ok {
			t.Errorf("expected ok=false when /publishers/ is absent")
		}
	})

	t.Run("rejects_url_without_projects_segment", func(t *testing.T) {
		c := NewModelConnection()
		c.URL = "https://host/publishers/google/models/gemini-2.5-flash:generateContent"
		if _, _, ok := geminiCacheEndpoint(c); ok {
			t.Errorf("expected ok=false when /projects/ is absent (e.g. bare Endpoint override)")
		}
	})
}

// ===========================================================================
// 5. Lifecycle (the core of gate 1 & gate 2): create / reuse / recreate / delete
//    / refresh, plus all the no-op gates.
// ===========================================================================

func TestGeminiCacheLifecycle(t *testing.T) {
	ctx := context.Background()

	t.Run("below_threshold_creates_nothing", func(t *testing.T) {
		conn, srv := setupGeminiCacheTest(t, "1h", http.StatusOK)
		req := CompletionRequest{Messages: smallCacheMessages()}
		conn.ensureGeminiCache(ctx, &req)
		if creates, _, _ := srv.snapshot(); creates != 0 {
			t.Errorf("below-threshold prefix created %d resources, want 0", creates)
		}
		if req.GeminiCachedContent != "" {
			t.Errorf("below-threshold prefix set GeminiCachedContent=%q, want empty", req.GeminiCachedContent)
		}
	})

	t.Run("no_completed_model_turn_creates_nothing", func(t *testing.T) {
		// A huge prefix with NO assistant turn: nothing stable to cache yet (the
		// post-snapshot tail must start on a user turn, so we never cache a prefix
		// that does not end on a model turn).
		conn, srv := setupGeminiCacheTest(t, "1h", http.StatusOK)
		req := CompletionRequest{Messages: []Message{{Role: RoleUser, Content: strings.Repeat("y", 220000)}}}
		conn.ensureGeminiCache(ctx, &req)
		if creates, _, _ := srv.snapshot(); creates != 0 {
			t.Errorf("no-model-turn prefix created %d resources, want 0", creates)
		}
		if req.GeminiCachedContent != "" {
			t.Errorf("no-model-turn prefix set GeminiCachedContent=%q, want empty", req.GeminiCachedContent)
		}
	})

	t.Run("creates_on_first_eligible_turn", func(t *testing.T) {
		conn, srv := setupGeminiCacheTest(t, "1h", http.StatusOK)
		req := CompletionRequest{Messages: largeCacheMessages()}
		conn.ensureGeminiCache(ctx, &req)
		if creates, _, _ := srv.snapshot(); creates != 1 {
			t.Fatalf("creates = %d, want 1", creates)
		}
		if req.GeminiCachedContent == "" {
			t.Fatalf("GeminiCachedContent empty after eligible create")
		}
		if req.GeminiCachedPrefixContents != 2 {
			t.Errorf("GeminiCachedPrefixContents = %d, want 2 (user+model)", req.GeminiCachedPrefixContents)
		}
		// The create body must carry the publisher-qualified model path, the ttl,
		// and the cached prefix — and must NOT carry a generationConfig.
		var cb map[string]interface{}
		if err := json.Unmarshal(srv.lastCreateBodyCopy(), &cb); err != nil {
			t.Fatalf("unmarshal create body: %v", err)
		}
		if cb["model"] != "projects/p/locations/us-central1/publishers/google/models/gemini-2.5-flash" {
			t.Errorf("create body model = %v, want publisher-qualified path", cb["model"])
		}
		if cb["ttl"] != "3600s" {
			t.Errorf("create body ttl = %v, want 3600s", cb["ttl"])
		}
		assertKeyPresent(t, cb, "contents")
		assertKeyAbsent(t, cb, "generationConfig") // sampling is per-request, never cached
		assertKeyAbsent(t, cb, "cachedContent")    // the create body is the resource, not a reference
	})

	t.Run("reuse_on_same_prefix_no_second_create", func(t *testing.T) {
		conn, srv := setupGeminiCacheTest(t, "1h", http.StatusOK)
		req1 := CompletionRequest{Messages: largeCacheMessages()}
		conn.ensureGeminiCache(ctx, &req1)
		name1 := req1.GeminiCachedContent
		if name1 == "" {
			t.Fatalf("first call did not create a cache")
		}
		req2 := CompletionRequest{Messages: largeCacheMessages()}
		conn.ensureGeminiCache(ctx, &req2)
		if creates, _, _ := srv.snapshot(); creates != 1 {
			t.Errorf("creates = %d, want 1 (reuse must not re-create)", creates)
		}
		if req2.GeminiCachedContent != name1 {
			t.Errorf("reuse: GeminiCachedContent = %q, want same %q", req2.GeminiCachedContent, name1)
		}
	})

	// THE headline scenario: the cached prefix must be REUSED, not recreated, as
	// the transcript grows — because the frozen prefix slice is stable across
	// turns (earlier transcript is immutable). This is the behavior the design
	// review flagged as the central risk.
	t.Run("reuse_across_grown_transcript", func(t *testing.T) {
		conn, srv := setupGeminiCacheTest(t, "1h", http.StatusOK)

		// Turn 1: cache the [user, assistant] prefix.
		req1 := CompletionRequest{Messages: largeCacheMessages()}
		conn.ensureGeminiCache(ctx, &req1)
		name1 := req1.GeminiCachedContent
		if creates, _, _ := srv.snapshot(); creates != 1 {
			t.Fatalf("turn1 creates = %d, want 1", creates)
		}

		// Turn 2: same prefix, plus a new user turn appended AFTER the cached
		// boundary. The frozen contents[:prefixLen] is byte-identical → reuse.
		grown1 := append(largeCacheMessages(), Message{Role: RoleUser, Content: "second question"})
		req2 := CompletionRequest{Messages: grown1}
		conn.ensureGeminiCache(ctx, &req2)
		if creates, _, _ := srv.snapshot(); creates != 1 {
			t.Errorf("turn2 creates = %d, want 1 (growth must reuse, not recreate)", creates)
		}
		if req2.GeminiCachedContent != name1 {
			t.Errorf("turn2 referenced %q, want same %q", req2.GeminiCachedContent, name1)
		}
		if req2.GeminiCachedPrefixContents != 2 {
			t.Errorf("turn2 prefix len = %d, want 2 (frozen)", req2.GeminiCachedPrefixContents)
		}

		// Turn 3: grown further, alternating user/assistant as a real transcript
		// does (a tool result always sits between two assistant turns, so the new
		// assistant turn is a SEPARATE model content after a user turn — it never
		// merges back into the cached model turn). The frozen slice is still
		// byte-identical → reuse, no churn.
		grown2 := append(grown1, Message{Role: RoleAssistant, Content: "a2"})
		req3 := CompletionRequest{Messages: grown2}
		conn.ensureGeminiCache(ctx, &req3)
		if creates, _, del := srv.snapshot(); creates != 1 || del != 0 {
			t.Errorf("turn3 creates=%d deletes=%d, want 1/0 (no churn across growth)", creates, del)
		}
		if req3.GeminiCachedContent != name1 {
			t.Errorf("turn3 referenced %q, want same %q", req3.GeminiCachedContent, name1)
		}
		// Turn 4: another user turn on top. Still reuse.
		grown3 := append(grown2, Message{Role: RoleUser, Content: "third question"})
		req4 := CompletionRequest{Messages: grown3}
		conn.ensureGeminiCache(ctx, &req4)
		if creates, _, del := srv.snapshot(); creates != 1 || del != 0 {
			t.Errorf("turn4 creates=%d deletes=%d, want 1/0", creates, del)
		}
		if req4.GeminiCachedContent != name1 {
			t.Errorf("turn4 referenced %q, want same %q", req4.GeminiCachedContent, name1)
		}
	})

	t.Run("invalidate_and_recreate_on_tools_change", func(t *testing.T) {
		conn, srv := setupGeminiCacheTest(t, "1h", http.StatusOK)
		req1 := CompletionRequest{Messages: largeCacheMessages(), Tools: oneTool("alpha")}
		conn.ensureGeminiCache(ctx, &req1)
		name1 := req1.GeminiCachedContent

		// Different tool set → the tools portion of the prefix hash changes → the
		// frozen resource is stale → delete + recreate.
		req2 := CompletionRequest{Messages: largeCacheMessages(), Tools: oneTool("beta")}
		conn.ensureGeminiCache(ctx, &req2)
		creates, _, deletes := srv.snapshot()
		if creates != 2 {
			t.Errorf("creates = %d, want 2 (recreate after tools change)", creates)
		}
		if deletes != 1 {
			t.Errorf("deletes = %d, want 1 (stale resource deleted)", deletes)
		}
		if req2.GeminiCachedContent == "" || req2.GeminiCachedContent == name1 {
			t.Errorf("recreate referenced %q, want a NEW non-empty name", req2.GeminiCachedContent)
		}
	})

	t.Run("invalidate_and_recreate_on_system_change", func(t *testing.T) {
		conn, srv := setupGeminiCacheTest(t, "1h", http.StatusOK)
		req1 := CompletionRequest{Messages: withSystem(largeCacheMessages(), "system A")}
		conn.ensureGeminiCache(ctx, &req1)
		name1 := req1.GeminiCachedContent

		req2 := CompletionRequest{Messages: withSystem(largeCacheMessages(), "system B")}
		conn.ensureGeminiCache(ctx, &req2)
		creates, _, deletes := srv.snapshot()
		if creates != 2 || deletes != 1 {
			t.Errorf("system change: creates=%d deletes=%d, want 2/1", creates, deletes)
		}
		if req2.GeminiCachedContent == "" || req2.GeminiCachedContent == name1 {
			t.Errorf("recreate referenced %q, want a NEW non-empty name", req2.GeminiCachedContent)
		}
	})

	t.Run("create_failure_disables_for_connection_life", func(t *testing.T) {
		// A create failure must NOT fail the turn, and must not retry every turn:
		// it disables explicit caching for the rest of the connection's life.
		conn, srv := setupGeminiCacheTest(t, "1h", http.StatusInternalServerError)
		req1 := CompletionRequest{Messages: largeCacheMessages()}
		conn.ensureGeminiCache(ctx, &req1)
		if req1.GeminiCachedContent != "" {
			t.Errorf("failed create still set GeminiCachedContent=%q (turn must fall back to full prefix)", req1.GeminiCachedContent)
		}
		if creates, _, _ := srv.snapshot(); creates != 1 {
			t.Fatalf("failed attempt counted creates = %d, want 1", creates)
		}
		// Second turn: disabled → no further create attempt at all.
		req2 := CompletionRequest{Messages: largeCacheMessages()}
		conn.ensureGeminiCache(ctx, &req2)
		if creates, _, _ := srv.snapshot(); creates != 1 {
			t.Errorf("after disable, creates = %d, want 1 (no retry after failure)", creates)
		}
		if req2.GeminiCachedContent != "" {
			t.Errorf("disabled connection set GeminiCachedContent=%q, want empty", req2.GeminiCachedContent)
		}
	})

	t.Run("refresh_patches_ttl_near_expiry", func(t *testing.T) {
		conn, srv := setupGeminiCacheTest(t, "1h", http.StatusOK)
		req1 := CompletionRequest{Messages: largeCacheMessages()}
		conn.ensureGeminiCache(ctx, &req1)
		name1 := req1.GeminiCachedContent
		if _, refreshes, _ := srv.snapshot(); refreshes != 0 {
			t.Fatalf("initial refreshes = %d, want 0", refreshes)
		}

		// Force the resource into the last 25% of its lifetime so the reuse path
		// fires a best-effort TTL refresh.
		conn.geminiCache.mu.Lock()
		conn.geminiCache.expiresAt = time.Now().Add(time.Minute)
		conn.geminiCache.mu.Unlock()

		req2 := CompletionRequest{Messages: largeCacheMessages()}
		conn.ensureGeminiCache(ctx, &req2)
		creates, refreshes, deletes := srv.snapshot()
		if creates != 1 || deletes != 0 {
			t.Errorf("refresh path: creates=%d deletes=%d, want 1/0 (refresh must not recreate)", creates, deletes)
		}
		if refreshes != 1 {
			t.Errorf("refreshes = %d, want 1 (TTL PATCH near expiry)", refreshes)
		}
		if req2.GeminiCachedContent != name1 {
			t.Errorf("after refresh, referenced %q, want same %q", req2.GeminiCachedContent, name1)
		}
		srv.mu.Lock()
		path := srv.lastRefreshPath
		body := string(srv.lastRefreshBody)
		srv.mu.Unlock()
		if !strings.Contains(path, "updateMask=ttl") {
			t.Errorf("refresh path = %q, want updateMask=ttl", path)
		}
		if !strings.Contains(body, `"ttl":"3600s"`) {
			t.Errorf("refresh body = %q, want ttl:3600s", body)
		}
	})

	t.Run("compaction_shrinking_transcript_recreates", func(t *testing.T) {
		// If the transcript shrinks BELOW the cached prefix length (e.g. after
		// compaction), the frozen slice can no longer match → invalidate + recreate
		// on whatever stable prefix now exists.
		conn, srv := setupGeminiCacheTest(t, "1h", http.StatusOK)
		req1 := CompletionRequest{Messages: largeCacheMessages()}
		conn.ensureGeminiCache(ctx, &req1)
		if creates, _, _ := srv.snapshot(); creates != 1 {
			t.Fatalf("setup creates = %d, want 1", creates)
		}
		// Shrink to a fresh large prefix of length 1 turn (still a model turn last).
		shrunk := []Message{
			{Role: RoleUser, Content: strings.Repeat("z", 220000)},
			{Role: RoleAssistant, Content: "compact"},
		}
		req2 := CompletionRequest{Messages: shrunk}
		conn.ensureGeminiCache(ctx, &req2)
		creates, _, deletes := srv.snapshot()
		if creates != 2 || deletes != 1 {
			t.Errorf("after shrink: creates=%d deletes=%d, want 2/1 (stale frozen slice recreated)", creates, deletes)
		}
	})
}

// ===========================================================================
// 6. No-op gates (gate 2: never surprise-create a billable resource)
// ===========================================================================

func TestGeminiCacheNoopGates(t *testing.T) {
	ctx := context.Background()

	t.Run("non_vertex_native_provider_is_noop", func(t *testing.T) {
		withFakeADCTokenSource(t, func(ctx context.Context, _ ...string) (oauth2.TokenSource, error) {
			return &staticTokenSource{token: "tok"}, nil
		})
		srv := newGeminiCacheServer(t, http.StatusOK)
		conn := NewModelConnectionFromConfig(&config.ModelConfig{
			APIType: "openai", Endpoint: srv.URL, Model: "gpt-4o", CacheTTL: "1h",
		})
		conn.URL = srv.URL + "/v1/projects/p/locations/us-central1/publishers/google/models/gemini-2.5-flash:generateContent"
		req := CompletionRequest{Messages: largeCacheMessages()}
		conn.ensureGeminiCache(ctx, &req)
		if creates, _, _ := srv.snapshot(); creates != 0 {
			t.Errorf("openai provider created %d caches, want 0 (capability gate)", creates)
		}
		if req.GeminiCachedContent != "" {
			t.Errorf("openai provider set GeminiCachedContent=%q, want empty", req.GeminiCachedContent)
		}
	})

	t.Run("empty_ttl_is_noop_even_on_vertex_native", func(t *testing.T) {
		conn, srv := setupGeminiCacheTest(t, "", http.StatusOK)
		req := CompletionRequest{Messages: largeCacheMessages()}
		conn.ensureGeminiCache(ctx, &req)
		if creates, _, _ := srv.snapshot(); creates != 0 {
			t.Errorf("empty TTL created %d caches, want 0 (opt-in)", creates)
		}
	})

	t.Run("off_ttl_is_noop", func(t *testing.T) {
		conn, srv := setupGeminiCacheTest(t, "off", http.StatusOK)
		req := CompletionRequest{Messages: largeCacheMessages()}
		conn.ensureGeminiCache(ctx, &req)
		if creates, _, _ := srv.snapshot(); creates != 0 {
			t.Errorf("off TTL created %d caches, want 0", creates)
		}
	})

	t.Run("none_ttl_is_noop", func(t *testing.T) {
		conn, srv := setupGeminiCacheTest(t, "none", http.StatusOK)
		req := CompletionRequest{Messages: largeCacheMessages()}
		conn.ensureGeminiCache(ctx, &req)
		if creates, _, _ := srv.snapshot(); creates != 0 {
			t.Errorf("none TTL created %d caches, want 0", creates)
		}
	})
}

// ===========================================================================
// 7. End-to-end wiring through complete() and completeStream() (gate 1 +
//    gate 4): the cache is resolved before marshaling, referenced on the wire,
//    and cache reads surface through #544's CacheStats.
// ===========================================================================

func TestGeminiCacheWiredIntoCompletePath(t *testing.T) {
	withFakeADCTokenSource(t, func(ctx context.Context, _ ...string) (oauth2.TokenSource, error) {
		return &staticTokenSource{token: "tok"}, nil
	})
	srv := newGeminiCacheServer(t, http.StatusOK)
	conn := NewModelConnectionFromConfig(&config.ModelConfig{
		APIType: "vertex-native", Endpoint: srv.URL, Project: "p", Location: "us-central1",
		Model: "gemini-2.5-flash", CacheTTL: "1h", MaxTokens: 64,
	})
	conn.URL = srv.URL + "/v1/projects/p/locations/us-central1/publishers/google/models/gemini-2.5-flash:generateContent"

	msgs := []Message{
		{Role: RoleSystem, Content: "SYS"},
		{Role: RoleUser, Content: strings.Repeat("x", 220000)},
		{Role: RoleAssistant, Content: "a1"},
		{Role: RoleUser, Content: "next"},
	}
	resp, err := conn.CompleteWithToolsCtx(context.Background(), msgs, oneTool("t"))
	if err != nil {
		t.Fatalf("CompleteWithToolsCtx: %v", err)
	}

	// The lifecycle engaged exactly once before the completion.
	if creates, refreshes, deletes := srv.snapshot(); creates != 1 || refreshes != 0 || deletes != 0 {
		t.Errorf("lifecycle creates=%d refreshes=%d deletes=%d, want 1/0/0", creates, refreshes, deletes)
	}

	// The generateContent request referenced the cache and omitted the shadowed
	// system + tools.
	var gen map[string]interface{}
	if err := json.Unmarshal(srv.genBody, &gen); err != nil {
		t.Fatalf("unmarshal generateContent body: %v", err)
	}
	assertKeyPresent(t, gen, "cachedContent")
	assertKeyAbsent(t, gen, "systemInstruction") // shadowed by the resource
	assertKeyAbsent(t, gen, "tools")             // shadowed by the resource

	// Cache reads surface through #544's CacheStats (gate 1 item 4, gate 2).
	if resp.Usage == nil {
		t.Fatalf("resp.Usage is nil; expected cache read stats")
	}
	if got := resp.Usage.Cache.ReadTokens; got != 999 {
		t.Errorf("Cache.ReadTokens = %d, want 999 (cachedContentTokenCount)", got)
	}
	if got := resp.Usage.Cache.WriteTokens; got != 0 {
		t.Errorf("Cache.WriteTokens = %d, want 0 (Gemini reports no write count)", got)
	}
	if got := resp.Usage.CachedTokens(); got != 999 {
		t.Errorf("CachedTokens() = %d, want 999 (legacy alias of Cache.ReadTokens)", got)
	}
}

func TestGeminiCacheWiredIntoStreamPath(t *testing.T) {
	withFakeADCTokenSource(t, func(ctx context.Context, _ ...string) (oauth2.TokenSource, error) {
		return &staticTokenSource{token: "tok"}, nil
	})
	srv := newGeminiCacheServer(t, http.StatusOK)
	conn := NewModelConnectionFromConfig(&config.ModelConfig{
		APIType: "vertex-native", Endpoint: srv.URL, Project: "p", Location: "us-central1",
		Model: "gemini-2.5-flash", CacheTTL: "1h", MaxTokens: 64,
	})
	conn.URL = srv.URL + "/v1/projects/p/locations/us-central1/publishers/google/models/gemini-2.5-flash:generateContent"

	msgs := []Message{
		{Role: RoleUser, Content: strings.Repeat("x", 220000)},
		{Role: RoleAssistant, Content: "a1"},
		{Role: RoleUser, Content: "next"},
	}
	resp, err := conn.CompleteWithToolsStreamCtx(context.Background(), msgs, oneTool("t"), nil)
	if err != nil {
		t.Fatalf("CompleteWithToolsStreamCtx: %v", err)
	}
	if resp.Content != "ok" {
		t.Errorf("stream content = %q, want ok", resp.Content)
	}
	if creates, _, _ := srv.snapshot(); creates != 1 {
		t.Errorf("stream path creates = %d, want 1", creates)
	}
	var gen map[string]interface{}
	if err := json.Unmarshal(srv.streamBody, &gen); err != nil {
		t.Fatalf("unmarshal stream body: %v", err)
	}
	assertKeyPresent(t, gen, "cachedContent")
	assertKeyAbsent(t, gen, "tools")
}

// ===========================================================================
// 8. Implicit-cache reporting is unchanged when explicit caching is inactive
//    (gate 3): cachedContentTokenCount still flows to Cache.ReadTokens on the
//    full-request path (no CacheTTL configured).
// ===========================================================================

func TestGeminiImplicitCacheReportingIntactWhenExplicitInactive(t *testing.T) {
	withFakeADCTokenSource(t, func(ctx context.Context, _ ...string) (oauth2.TokenSource, error) {
		return &staticTokenSource{token: "tok"}, nil
	})
	srv := newGeminiCacheServer(t, http.StatusOK)
	conn := NewModelConnectionFromConfig(&config.ModelConfig{
		APIType: "vertex-native", Endpoint: srv.URL, Project: "p", Location: "us-central1",
		Model: "gemini-2.5-flash", MaxTokens: 64, // no CacheTTL → explicit caching OFF
	})
	conn.URL = srv.URL + "/v1/projects/p/locations/us-central1/publishers/google/models/gemini-2.5-flash:generateContent"

	// Small prefix: no explicit cache; implicit hits still reported.
	resp, err := conn.CompleteWithToolsCtx(context.Background(), smallCacheMessages(), nil)
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if creates, _, _ := srv.snapshot(); creates != 0 {
		t.Errorf("inactive explicit path created %d caches, want 0", creates)
	}
	var gen map[string]interface{}
	if err := json.Unmarshal(srv.genBody, &gen); err != nil {
		t.Fatalf("unmarshal gen body: %v", err)
	}
	assertKeyAbsent(t, gen, "cachedContent") // explicit cache inactive → no reference on the wire
	if resp.Usage == nil || resp.Usage.Cache.ReadTokens != 999 {
		t.Errorf("implicit cachedContentTokenCount not surfaced: %+v", resp.Usage)
	}
}
