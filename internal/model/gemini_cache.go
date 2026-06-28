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
// caching is NOT engaged. Vertex's hard minimum is far smaller (~1024–4096 by
// model), but a billable CachedContent only pays off for a genuinely large,
// reused prefix, so the gate is deliberately conservative. The estimate is a
// cheap bytes/4 approximation (no extra token-count round-trip).
const geminiMinCacheTokens = 32768

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
		if st.prefixLen <= len(contents) {
			if raw, herr := geminiPrefixBytes(sys, tools, contents[:st.prefixLen]); herr == nil && geminiHashHex(raw) == st.prefixHash {
				reqBody.GeminiCachedContent = st.name
				reqBody.GeminiCachedPrefixContents = st.prefixLen
				// Refresh the TTL when within the last quarter of its lifetime, so a
				// long-running session keeps the resource alive past the configured
				// window. Best-effort: a failed refresh still references the cache.
				if time.Now().After(st.expiresAt.Add(-ttl / 4)) {
					if exp, ok := c.geminiCacheRefresh(ctx, st.name, ttlStr, ttl); ok {
						st.expiresAt = exp
					}
				}
				return
			}
		}
		// The cached prefix changed (tools/system edited, or compaction rewrote the
		// transcript): the frozen resource is stale. Delete it best-effort and fall
		// through to recreate.
		c.geminiCacheDelete(ctx, st.name)
		st.name, st.prefixHash, st.prefixLen = "", "", 0
	}

	// Create: cache the largest stable prefix — everything up to and including the
	// last model(assistant) turn, so the post-snapshot tail starts on a user turn
	// and the seam (cached.contents ++ request.contents) stays role-alternating.
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
		// Fail-safe: a create failure (unsupported region, quota, transient error)
		// disables explicit caching for the rest of this connection's life rather
		// than retrying every turn; the session degrades cleanly to implicit caching.
		st.disabled = true
		return
	}
	st.name, st.prefixLen, st.prefixHash, st.expiresAt = name, b, geminiHashHex(raw), exp
	reqBody.GeminiCachedContent = name
	reqBody.GeminiCachedPrefixContents = b
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
	return res.Name, time.Now().Add(ttl), true
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
	if _, err := c.doJSONBody(ctx, http.MethodPatch, resURL+"?updateMask=ttl", nil, body); err != nil {
		return time.Time{}, false
	}
	return time.Now().Add(ttl), true
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
