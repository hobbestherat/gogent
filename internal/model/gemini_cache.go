package model

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Native-Gemini explicit context caching (issue #547).
//
// Vertex's native Gemini API exposes a persistent, TTL-managed shared-prefix
// cache as a CachedContent resource: a separate cachedContents.create call
// supplies the stable prefix (systemInstruction + tools + a leading slice of
// contents) and returns a resource name; subsequent generateContent /
// streamGenerateContent requests reference it via the top-level cachedContent
// field (which shadows those contents server-side) instead of re-sending them.
// Unlike Gemini's implicit/ephemeral prefix caching (automatic, short-window,
// already reported via usageMetadata.cachedContentTokenCount), this persists
// across the >5-minute gaps where implicit caching expires — at the price of
// billable storage, so it is OPT-IN (config CacheTTL) and HEURISTIC-GATED (a
// minimum prefix size).
//
// The lifecycle (create / reference / refresh / recreate / delete) lives here,
// in the connection layer, because it is network I/O with a context; the wire
// adapter only consumes the resolved reference (CompletionRequest.GeminiCachedContent,
// set by ensureGeminiCache below). State is per-connection and, because a session
// issues turns serially, contention on its mutex is nil — the lock merely keeps a
// stray concurrent caller safe.

// geminiMinCacheTokens is the estimated prefix-token floor below which explicit
// caching is NOT engaged: small prefixes are left to Gemini's free implicit
// caching, where a billable CachedContent would not pay off. It is set above
// Vertex's hard minimum (~1024–4096 by model) but WELL BELOW a typical context
// window so the feature actually triggers for genuinely large prefixes — at the
// old 32768 floor it equalled config.defaultContextWindow, so compaction fired
// before the prefix ever reached it and the opt-in was a silent no-op. The
// estimate is a cheap bytes/4 approximation (no extra token-count round-trip).
const geminiMinCacheTokens = 8192

// geminiCacheState is the per-connection explicit-cache lifecycle state. The zero
// value is "no resource, never tried" and is safe for every non-vertex-native
// connection (which never reaches ensureGeminiCache).
type geminiCacheState struct {
	mu         sync.Mutex
	name       string    // "projects/…/cachedContents/{id}" ("" = none live)
	prefixHash string    // hex sha256 of the cached (systemInstruction+tools+prefix)
	prefixLen  int       // # merged contents the resource holds (→ GeminiCachedPrefixContents)
	expiresAt  time.Time // local estimate of TTL expiry, for the refresh window
	disabled   bool      // a create failed → fall back to implicit caching for the session
}

// geminiCachePrefix is the canonical shape of the cached prefix, marshaled once
// for BOTH the size estimate and the invalidation hash so the two never diverge.
type geminiCachePrefix struct {
	SystemInstruction *geminiContent  `json:"systemInstruction,omitempty"`
	Tools             []geminiTool    `json:"tools,omitempty"`
	Contents          []geminiContent `json:"contents,omitempty"`
}

// geminiCachedContentsCreate is the cachedContents.create request body. Model is
// the publisher-qualified resource path ("projects/…/publishers/google/models/…").
type geminiCachedContentsCreate struct {
	Model             string          `json:"model"`
	SystemInstruction *geminiContent  `json:"systemInstruction,omitempty"`
	Tools             []geminiTool    `json:"tools,omitempty"`
	Contents          []geminiContent `json:"contents,omitempty"`
	TTL               string          `json:"ttl,omitempty"`
}

// geminiCachedContentsResource is the subset of the cachedContents resource the
// lifecycle needs back from create.
type geminiCachedContentsResource struct {
	Name       string `json:"name"`
	ExpireTime string `json:"expireTime"`
}

// ensureGeminiCache resolves the native-Gemini explicit context cache for one
// request, annotating reqBody with a CachedContent reference when one is active.
// It is a NO-OP for every non-vertex-native provider, when explicit caching is
// disabled (config CacheTTL unset/off) or has been disabled by a prior failure,
// and when the stable prefix is below the size gate — in all those cases reqBody
// is left untouched and the adapter marshals exactly as today. A cache failure
// never fails the turn: the request simply falls back to sending the full prefix.
func (c *ModelConnection) ensureGeminiCache(ctx context.Context, reqBody *CompletionRequest) {
	// Gate: only the provider that advertises the CachedContent capability, and
	// only when the user opted in with a positive CacheTTL (issue #545 knob).
	if c.caps().CacheControl != CacheControlCachedContent {
		return
	}
	ttlStr := c.Config.GeminiCacheTTL()
	if ttlStr == "" {
		return
	}
	ttl, err := time.ParseDuration(ttlStr)
	if err != nil || ttl <= 0 {
		return
	}

	st := &c.geminiCache
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.disabled {
		return
	}

	// Build the same (systemInstruction, contents, tools) the adapter will, so the
	// resource and the request agree byte-for-byte on what is shadowed.
	sys, contents, tools, _ := geminiBuildContents(*reqBody)

	// Reuse: a live resource still covering an unchanged prefix. The earlier
	// transcript is immutable, so contents[:prefixLen] is byte-identical across
	// turns and only the post-snapshot tail grows.
	if st.name != "" {
		matches := st.prefixLen <= len(contents)
		if matches {
			raw, herr := geminiPrefixBytes(sys, tools, contents[:st.prefixLen])
			matches = herr == nil && geminiHashHex(raw) == st.prefixHash
		}
		if matches {
			// The prefix is unchanged. Refresh the TTL when at/near expiry so a
			// long-running session persists past the configured window — the whole
			// point of explicit caching. CRUCIALLY, when the resource is already PAST
			// its (estimated) expiry and the refresh fails, the resource is gone
			// server-side: drop it and fall through to recreate rather than reference
			// a dead resource — otherwise the request 400s and, because the frozen
			// prefix hash never changes, every later turn 400s too, wedging the
			// session across exactly the >1h gap the feature exists to bridge. A
			// failed refresh while the resource is still comfortably live is non-fatal
			// (it stays referenced; the next turn retries the refresh).
			live := true
			if st.expiresAt.IsZero() || time.Now().After(st.expiresAt.Add(-ttl/4)) {
				if exp, ok := c.geminiCacheRefresh(ctx, st.name, ttlStr, ttl); ok {
					st.expiresAt = exp
				} else if st.expiresAt.IsZero() || time.Now().After(st.expiresAt) {
					live = false // expired and unrevivable → recreate below
				}
			}
			if live {
				reqBody.GeminiCachedContent = st.name
				reqBody.GeminiCachedPrefixContents = st.prefixLen
				return
			}
		}
		// Stale (tools/system edited, or compaction rewrote the transcript) or
		// expired-and-unrevivable: delete the old resource best-effort and fall
		// through to recreate on the current prefix.
		c.geminiCacheDelete(ctx, st.name)
		st.name, st.prefixHash, st.prefixLen, st.expiresAt = "", "", 0, time.Time{}
	}

	// Create: cache the largest stable prefix — everything up to and including the
	// last model(assistant) turn. Ending on a model turn means the post-snapshot
	// tail starts on a user turn, so the seam (cached.contents ++ request.contents)
	// stays role-alternating. This subsumes the provider-agnostic Volatile-tail
	// rule: the volatile per-turn context is always a TRAILING user turn (see
	// model_session.go), so "up to the last model turn" necessarily excludes it —
	// and additionally guarantees the alternating seam that Gemini needs, which the
	// Volatile flag alone would not.
	b := geminiStablePrefixBoundary(contents)
	if b == 0 {
		return // no completed assistant turn yet → nothing stable to cache
	}
	raw, err := geminiPrefixBytes(sys, tools, contents[:b])
	if err != nil {
		return
	}
	if len(raw)/4 < geminiMinCacheTokens {
		return // prefix too small — implicit caching suffices; skip the storage cost
	}
	name, exp, ok := c.geminiCacheCreate(ctx, sys, tools, contents[:b], ttlStr, ttl)
	if !ok {
		// A cancelled context is transient — the turn is aborting anyway, so never
		// let it disable caching for the session. Any other failure (unsupported
		// region, quota, persistent error) disables explicit caching for the rest of
		// this connection's life rather than retrying a doomed create every turn; the
		// session degrades cleanly to implicit caching.
		if ctx.Err() == nil {
			st.disabled = true
		}
		return
	}
	st.name, st.prefixLen, st.prefixHash, st.expiresAt = name, b, geminiHashHex(raw), exp
	reqBody.GeminiCachedContent = name
	reqBody.GeminiCachedPrefixContents = b
}

// dropGeminiCacheRefAfterError clears the explicit-cache state after a request
// that REFERENCED a resource failed with a PERMANENT client error. This is the
// reactive safety net for a referenced cache the server rejects for a reason the
// proactive expiry check cannot see — eviction before the estimated TTL, a region
// that dropped the resource, a stale reference — so the next turn recreates
// instead of re-referencing a dead resource and 400ing forever. It is gated to a
// permanent 4xx: a 5xx or a RETRYABLE 4xx (408/409/429) is transient — the caller
// retries it — so invalidating the reference there would churn an unnecessary
// recreate during a rate-limit/conflict, the opposite of ideal. The gate tracks
// the retry policy exactly (isRetryableStatus), not a raw status range. A needless
// drop on a genuine permanent 4xx merely recreates next turn, so the heuristic is
// safe and self-healing.
func (c *ModelConnection) dropGeminiCacheRefAfterError(referenced bool, status int) {
	if !referenced || status < 400 || status >= 500 || isRetryableStatus(status) {
		return
	}
	st := &c.geminiCache
	st.mu.Lock()
	st.name, st.prefixHash, st.prefixLen, st.expiresAt = "", "", 0, time.Time{}
	st.mu.Unlock()
}

// geminiStablePrefixBoundary returns the count of leading contents that form the
// cacheable prefix: everything up to and including the last model(assistant)
// turn. Returns 0 when there is no model turn yet (so nothing stable to cache).
func geminiStablePrefixBoundary(contents []geminiContent) int {
	for i := len(contents) - 1; i >= 0; i-- {
		if contents[i].Role == "model" {
			return i + 1
		}
	}
	return 0
}

// geminiPrefixBytes marshals the cached-prefix triple to its canonical JSON, used
// for both the size estimate and the invalidation hash.
func geminiPrefixBytes(sys *geminiContent, tools []geminiTool, prefix []geminiContent) ([]byte, error) {
	b, err := json.Marshal(geminiCachePrefix{SystemInstruction: sys, Tools: tools, Contents: prefix})
	if err != nil {
		return nil, fmt.Errorf("marshal gemini cache prefix: %w", err)
	}
	return b, nil
}

// geminiHashHex is the hex sha256 of b — the cached prefix's invalidation key.
func geminiHashHex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// geminiCacheCreate POSTs a cachedContents resource for the given prefix and TTL,
// returning its resource name and a local expiry estimate. ok is false on any
// error (caller degrades to the full-request path).
func (c *ModelConnection) geminiCacheCreate(ctx context.Context, sys *geminiContent, tools []geminiTool, prefix []geminiContent, ttlStr string, ttl time.Duration) (name string, exp time.Time, ok bool) {
	collURL, modelPath, derived := geminiCacheEndpoint(c)
	if !derived {
		return "", time.Time{}, false
	}
	body, err := json.Marshal(geminiCachedContentsCreate{
		Model:             modelPath,
		SystemInstruction: sys,
		Tools:             tools,
		Contents:          prefix,
		TTL:               ttlStr,
	})
	if err != nil {
		return "", time.Time{}, false
	}
	respBytes, err := c.doJSONBody(ctx, http.MethodPost, collURL, nil, body)
	if err != nil {
		return "", time.Time{}, false
	}
	var res geminiCachedContentsResource
	if err := json.Unmarshal(respBytes, &res); err != nil || strings.TrimSpace(res.Name) == "" {
		return "", time.Time{}, false
	}
	return res.Name, geminiCacheExpiry(res.ExpireTime, ttl), true
}

// geminiCacheExpiry resolves the resource's expiry: the server's authoritative
// expireTime when present and parseable (Vertex may cap the requested TTL, so the
// local now+ttl estimate would drift and refresh too late), falling back to
// now+ttl otherwise.
func geminiCacheExpiry(expireTime string, ttl time.Duration) time.Time {
	if t, err := time.Parse(time.RFC3339, strings.TrimSpace(expireTime)); err == nil {
		return t
	}
	return time.Now().Add(ttl)
}

// geminiCacheRefresh PATCHes the resource's TTL (updateMask=ttl) to extend its
// lifetime, returning a fresh local expiry estimate. Best-effort: ok is false on
// any error and the caller keeps the existing (soon-to-expire) estimate.
func (c *ModelConnection) geminiCacheRefresh(ctx context.Context, name, ttlStr string, ttl time.Duration) (time.Time, bool) {
	resURL, ok := geminiCacheResourceURL(c, name)
	if !ok {
		return time.Time{}, false
	}
	body, err := json.Marshal(map[string]string{"ttl": ttlStr})
	if err != nil {
		return time.Time{}, false
	}
	respBytes, err := c.doJSONBody(ctx, http.MethodPatch, resURL+"?updateMask=ttl", nil, body)
	if err != nil {
		return time.Time{}, false
	}
	var res geminiCachedContentsResource
	_ = json.Unmarshal(respBytes, &res) // best-effort: fall back to now+ttl on a bodyless/odd response
	return geminiCacheExpiry(res.ExpireTime, ttl), true
}

// geminiCacheDelete DELETEs a stale resource. Best-effort — errors are ignored
// (the resource also self-expires at its TTL).
func (c *ModelConnection) geminiCacheDelete(ctx context.Context, name string) {
	if resURL, ok := geminiCacheResourceURL(c, name); ok {
		_, _ = c.doJSONBody(ctx, http.MethodDelete, resURL, nil, nil)
	}
}

// geminiCacheEndpoint derives the cachedContents collection URL and the
// publisher-qualified model resource path from the connection's chat URL
// (".../v1/projects/{p}/locations/{l}/publishers/google/models/{model}:generateContent"),
// so it is correct for any region and for an explicit endpoint override. ok is
// false if the URL is not the expected native-Gemini shape.
func geminiCacheEndpoint(c *ModelConnection) (collectionURL, modelPath string, ok bool) {
	u := strings.TrimSpace(c.URL)
	pubIdx := strings.Index(u, "/publishers/")
	projIdx := strings.Index(u, "/projects/")
	if pubIdx < 0 || projIdx < 0 {
		return "", "", false
	}
	base := u[:pubIdx] // https://host/v1/projects/{p}/locations/{l}
	modelFull := strings.TrimSuffix(u, ":generateContent")
	modelPath = strings.TrimPrefix(modelFull[projIdx:], "/") // projects/{p}/locations/{l}/publishers/google/models/{model}
	return base + "/cachedContents", modelPath, true
}

// geminiCacheResourceURL builds the full URL of an individual cachedContents
// resource from the connection's chat URL root and the resource name
// ("projects/…/cachedContents/{id}").
func geminiCacheResourceURL(c *ModelConnection, name string) (string, bool) {
	u := strings.TrimSpace(c.URL)
	projIdx := strings.Index(u, "/projects/")
	if projIdx < 0 {
		return "", false
	}
	root := u[:projIdx] // https://host/v1
	return root + "/" + strings.TrimPrefix(strings.TrimSpace(name), "/"), true
}
