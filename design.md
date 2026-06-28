# Design — Gemini explicit context caching (CachedContent management) · issue #547

Branch: `pair1/gemini-explicit-context-caching-cachedco` · gogent-only · stdlib-first · no new deps · no go.mod bump · no turbotui change.

Closes #547. Depends on #544 (CacheStats{Read,Write} + the `CacheControlCachedContent` capability flag) and #545 (`ModelConfig.CacheTTL`) — both already merged into `main` (verified: `provider.go:159` `CacheControlCachedContent`, `provider_vertex.go:58` declares it for vertex-native, `config.go:76` `CacheTTL`, `connection.go:461` `CacheStats`). This issue *implements* the capability that #544 only declared.

## 0. What exists today vs. the gap

- vertex-native (`geminiAdapter`, `adapter.go:855`) sends `systemInstruction` + `tools` + `contents[]` on every `:generateContent` / `:streamGenerateContent` request. It never creates or references a `CachedContent` resource.
- It already *parses* implicit-cache hits: `usageMetadata.cachedContentTokenCount → Cache.ReadTokens` (`adapter.go:1258`), which #544 routes into `CacheStats.ReadTokens`. So the **read-reporting half is done**; Gemini reports no write count, so `WriteTokens` stays 0 (gate-1 item 4 already satisfied — nothing to change there).
- The gap is the two missing steps: (a) `POST …/cachedContents` to create the resource; (b) reference it via the top-level `cachedContent` field while omitting the contents it shadows.

The `cachedContentTokenCount` read path is the **acceptance signal**: when explicit caching is active and working, that field comes back `> 0` and prompt tokens drop — already surfaced through #544's CacheStats with no further plumbing.

## 1. Goal match (gate 1)

Implement the full `CachedContent` lifecycle for vertex-native, opt-in and heuristic-gated:

1. **Create** a `CachedContent` from the stable prefix (`systemInstruction` + `tools` + a stable leading slice of `contents`) on the first eligible turn.
2. **Reference** it on every subsequent request via top-level `cachedContent: "projects/…/cachedContents/{id}"`, omitting the `systemInstruction`, `tools`, and prefix `contents` it shadows — sending only the post-snapshot tail (new user turn, tool results, volatile tail).
3. **Refresh** the TTL on reuse (best-effort `PATCH …?updateMask=ttl`) so a long-running session keeps the resource alive across >1h gaps.
4. **Recreate** when the cached prefix is invalidated (tools or systemInstruction changed → prefix hash mismatch), deleting the stale resource first.
5. **Delete** (best-effort) the resource when it is replaced or the session closes.

Reads continue to surface via #544 `CacheStats.ReadTokens` from `cachedContentTokenCount`; `WriteTokens` stays 0. No scope creep: no change to Anthropic/OpenAI paths, no new TUI surface beyond what #544 already shows.

### Opt-in & heuristic gate (gate 1 item 3, gate 2 "no surprise storage cost")

Explicit Gemini caching is **OFF by default** and engages only when BOTH hold:

- **Opt-in via the shared `ModelConfig.CacheTTL` knob (#545).** Today `CacheTTL` is Anthropic-semantics ("" ⇒ 5m default ON). For Gemini the empty value must mean **off** (a billable storage resource must never be created without the user asking). New helper `config.ModelConfig.GeminiCacheTTL() string` (sibling to `AnthropicCacheTTL`, same file): returns a normalized Gemini `ttl` string (e.g. `"3600s"`) only when `CacheTTL` is an explicit positive duration (`"1h"`, `"5m"`, `"30m"`, `"<N>s"`); returns `""` (disabled) for empty/`"off"`/`"none"`/unrecognized. This keeps one user-facing field doing the analogous job on both providers while making Gemini opt-in.
- **Heuristic token-threshold gate.** Only create a resource when the cacheable prefix is genuinely large and stable. Gate on an estimated prefix-token count `≥ geminiMinCacheTokens` (const, default conservative — see Open Questions; Vertex's documented explicit-cache minimum is ~4096 for older Gemini, ~1024 for 2.5; we default higher, e.g. 32768, so the storage cost clearly pays off). Estimate cheaply from prefix byte length (≈ chars/4) — no extra round-trip. Below threshold: behave exactly as today (implicit caching only).

Both gates also short-circuit when the provider does not advertise `CacheControlCachedContent` (so only vertex-native ever reaches this code), mirroring how `connection.go:1479` forces Anthropic TTL `"off"` without the `CacheControlBreakpoints` cap.

## 2. Design — where the seam lands

The marshaling adapter (`buildBody`) is pure and network-free; the create/refresh calls are network I/O with a `ctx`. So the lifecycle runs in the **connection layer** (which has `ctx`, `c.client` with ADC auth, `c.Config`, `c.ModelName`), and the adapter only consumes the resolved cache reference passed on the request struct — exactly the pattern `CacheTTL` already uses (`connection.go:348`, json:"-", consumed only by the Anthropic adapter).

### 2a. Request plumbing — `CompletionRequest` (connection.go) + `geminiRequest` (adapter.go)

Add two `json:"-"` fields to `CompletionRequest` (next to `CacheTTL`, ~`connection.go:348`):

```go
// GeminiCachedContent, when set, is the full resource name of a Gemini
// CachedContent ("projects/…/locations/…/cachedContents/{id}") to reference
// in place of the shadowed prefix. Consumed ONLY by the Gemini adapter; empty
// on every other path (json:"-" keeps it off the OpenAI wire). Issue #547.
GeminiCachedContent string `json:"-"`
// GeminiCachedPrefixContents is the number of leading merged Gemini contents
// the referenced CachedContent already holds; the adapter emits contents[N:]
// and omits systemInstruction/tools. Meaningful only when GeminiCachedContent
// is set.
GeminiCachedPrefixContents int `json:"-"`
```

Add one wire field to `geminiRequest` (`adapter.go:858`):

```go
CachedContent string `json:"cachedContent,omitempty"`
```

`omitempty` guarantees **byte-identical output when inactive** (gate 3).

### 2b. Adapter change — `geminiAdapter.buildBody` (adapter.go:955)

The contents/system/tools build is refactored into a small shared helper so the lifecycle manager and `buildBody` agree byte-for-byte on the prefix:

```go
// geminiBuildContents maps req.Messages to (systemInstruction, contents) with
// the existing hoist + same-role merge logic (today's loop at adapter.go:962-981),
// and req.Tools to functionDeclarations (today's adapter.go:984-1002). Pure;
// no network. Returns the SAME values today's inline code produces.
func geminiBuildContents(req CompletionRequest) (sys *geminiContent, contents []geminiContent, tools []geminiTool, toolCfg *geminiToolConfig)
```

`buildBody` then:

```go
sys, contents, tools, toolCfg := geminiBuildContents(req)
out := geminiRequest{}
if req.GeminiCachedContent != "" {
    out.CachedContent = req.GeminiCachedContent
    // Shadowed by the resource: omit systemInstruction, tools, and the cached
    // prefix contents. Send only the post-snapshot tail.
    n := req.GeminiCachedPrefixContents
    if n > len(contents) { n = len(contents) } // defensive
    out.Contents = contents[n:]
    // tools/toolConfig/systemInstruction intentionally omitted — they live in
    // the cached resource. (Vertex rejects re-declaring tools alongside a cache.)
} else {
    out.SystemInstruction, out.Contents, out.Tools, out.ToolConfig = sys, contents, tools, toolCfg
}
// generationConfig is built exactly as today (NOT cached — sampling/thinking
// stay per-request) and applies on both branches.
```

When `GeminiCachedContent == ""` the else-branch reproduces today's assignments exactly ⇒ inactive path unchanged.

### 2c. Lifecycle manager — new file `internal/model/gemini_cache.go`

Per-connection state (the `ModelConnection` is stable across a session's turns — it is `s.Model`):

```go
type geminiCacheState struct {
    mu         sync.Mutex
    name       string // "projects/…/cachedContents/{id}" ("" = none live)
    prefixHash string // hash(systemInstruction + tools + cached prefix contents)
    prefixLen  int    // # merged contents the resource holds (→ GeminiCachedPrefixContents)
    expiresAt  time.Time
}
```

Add `geminiCache geminiCacheState` to `ModelConnection` (zero-value safe; only touched on the vertex-native path).

New method, called from `complete` and `completeStream` **after `buildRequest`, before `buildBody`**:

```go
func (c *ModelConnection) ensureGeminiCache(ctx context.Context, reqBody *CompletionRequest)
```

Logic:

1. Gate: return immediately unless `c.provider.caps.CacheControl == CacheControlCachedContent` **and** `ttl := c.Config.GeminiCacheTTL()` is non-empty.
2. Build `(sys, contents, tools)` via `geminiBuildContents(*reqBody)`. Compute the **cacheable prefix boundary** `B` using the existing Volatile-tail rule: the prefix is all non-volatile contents up to and **ending on a `model`(assistant) turn** — never the volatile tail, never a dangling trailing user turn (so the seam `cachedContent.contents ++ request.contents` never produces two adjacent user turns; see §4 risk). Reuses the same `Message.Volatile` signal the Anthropic breakpoint walk uses (`adapter.go:373`).
3. Estimate prefix tokens (prefix bytes / 4). If `< geminiMinCacheTokens`, leave `reqBody` untouched (no resource, no reference) and return.
4. `hash := sha256(sys ‖ tools ‖ contents[:B])`. Under `mu`:
   - **Reuse:** `name != "" && hash == prefixHash` → set `reqBody.GeminiCachedContent = name`, `reqBody.GeminiCachedPrefixContents = prefixLen`; if `expiresAt` within a refresh window, fire a best-effort `refresh` (PATCH ttl). Return.
   - **Invalidated:** `name != "" && hash != prefixHash` (tools/system changed) → best-effort `delete(name)`, fall through to create.
   - **Create:** `POST …/cachedContents` with `{model, systemInstruction, contents[:B], tools, ttl}`. On success store `name/hash/prefixLen=B/expiresAt`; set the two `reqBody` fields. **On any error, log-and-continue with the resource NOT referenced** — caching is a pure optimization; a cache failure must never fail the turn (fall back to sending the full request as today).

Network helpers (also in `gemini_cache.go`), reusing `c.client` (ADC auth applies automatically via the round-tripper):

- `geminiCachedContentsURL(c)` = `vertexNativeBaseURL(c.Config)` + `/cachedContents`. Resource `model` field = `projects/{p}/locations/{l}/publishers/google/models/{ModelName}`.
- A small body-capable POST/PATCH/DELETE helper. `doJSON` (`provider.go:291`) is GET/no-body/200-only; rather than widen its contract I add `doJSONBody(ctx, method, url, headers, body []byte)` next to it (same auth seam, accepts a request body, tolerates 200). `delete`/`refresh` are best-effort (errors logged, ignored).

### 2d. Call-site wiring (connection.go)

In `complete` (before `adapter.buildBody` at `connection.go:1531`) and `completeStream` (before `connection.go:1663`):

```go
c.ensureGeminiCache(ctx, &reqBody)
```

One line each, after `buildRequest`. No-op for every non-vertex-native provider and for vertex-native when the gates are unmet.

## 3. Usability (gate 2)

- **User drives it via one familiar field.** `CacheTTL` on the model config (already in the Models… dialog from #545) turns Gemini explicit caching on with a chosen lifetime (`"1h"` for long sessions); empty/`off` keeps today's behavior. No new dialog, no new config surface — consistent with the Anthropic knob.
- **Right thing surfaced, not silent.** Cache reads already show in the existing token/cache stats via #544's `CacheStats.ReadTokens` (`cachedContentTokenCount`) — the user sees prompt tokens drop and cached-read tokens rise exactly as for other providers. A one-line debug log on create/recreate/refresh records lifecycle events without noise.
- **No surprise storage cost.** Double-gated (explicit TTL opt-in **and** a large-prefix threshold). Small or volatile prefixes are never cached. A failed create degrades silently to the full-request path — never a hard error mid-conversation.
- **Behaves as expected across gaps.** TTL refresh-on-use keeps a genuinely large prefix warm across >1h idle gaps where implicit caching would have expired — the stated goal.

## 4. No regressions (gate 3)

- **Inactive path byte-identical.** New `geminiRequest.CachedContent` is `omitempty`; when `GeminiCachedContent == ""`, `buildBody` runs the unchanged else-branch. `geminiBuildContents` is a pure extract-method refactor of the current inline code — covered by re-running existing `gemini_adapter_test.go` (representative-request, volatile-tail-merge, tool-choice, function-response tests) unchanged.
- **`ensureGeminiCache` is a no-op everywhere it must be:** non-vertex-native (capability gate), TTL empty, below threshold, or any network error. The OpenAI/Anthropic/DeepSeek paths never touch it.
- **Implicit-cache reporting unchanged.** `cachedContentTokenCount` parsing (`adapter.go:1258`) is untouched; it serves both implicit and explicit reads identically.
- **Seam-correctness risk (the main one):** `cachedContent.contents ++ request.contents` must form a valid alternating-role conversation. Mitigation: boundary `B` is chosen to end on a `model` turn, so the first non-cached content is a `user` turn — no adjacent same-role turns across the seam. The volatile tail is always post-`B` (it is never cached), matching today's invariant.
- **Concurrency:** state guarded by `geminiCacheState.mu`; a session issues turns serially, so contention is nil, but the mutex keeps a stray concurrent caller safe.
- **Build/vet/gofmt/lint:** new file uses only stdlib (`crypto/sha256`, `net/http`, `time`, `encoding/json`) + existing internal helpers; no new deps, no go.mod bump. golangci-lint: 0 new (errors on best-effort delete/refresh explicitly `_`-ignored with a comment, matching repo style).
- **Tests gate:** `go test ./...` green except the pre-existing `TestUserSessionSendMessage` 404 (accepted). New unit tests (see §6) added to `gemini_adapter_test.go` / a new `gemini_cache_test.go`.

## 5. Holistic / cross-repo (gate 4)

- **Right place:** wire-format concern lives entirely in `internal/model` (adapter = marshaling, connection = network lifecycle). This mirrors the Anthropic cache_control design exactly (capability declared on the provider, directive carried as a `json:"-"` request field, consumed by one adapter). Reuses #544's capability flag, #545's `CacheTTL` field, and the existing `Message.Volatile` prefix logic — no parallel mechanism invented.
- **turbotui untouched.** Explicit caching is a backend optimization; its only user-visible effect (cache-read tokens) already flows through #544's `CacheStats`, which turbotui already renders. No new field crosses the repo seam, no turbotui change, no go.mod bump in either repo. Confirmed against `$HOME/work/turbotui` (read-only): cache stats are consumed there as the existing read/write counters; this change only makes those counters non-zero on the Gemini path. The seam is respected — gogent owns the provider wire, turbotui owns presentation.

## 6. Files & verification

**Touched (gogent only):**
- `internal/config/config.go` — add `GeminiCacheTTL()` helper (Gemini-semantics: empty ⇒ off).
- `internal/model/connection.go` — two `json:"-"` fields on `CompletionRequest`; `geminiCache` field on `ModelConnection`; `doJSONBody` helper; one `ensureGeminiCache` call in `complete` and in `completeStream`.
- `internal/model/adapter.go` — `CachedContent` field on `geminiRequest`; extract `geminiBuildContents`; cache-reference branch in `buildBody`.
- `internal/model/gemini_cache.go` *(new)* — `geminiCacheState`, `ensureGeminiCache`, create/refresh/delete + URL/body helpers.
- `internal/model/gemini_cache_test.go` *(new)* + additions to `gemini_adapter_test.go`.

**Unit tests:**
1. `buildBody` with `GeminiCachedContent` set + `GeminiCachedPrefixContents=N` ⇒ emits `cachedContent`, omits `systemInstruction`/`tools`, sends only `contents[N:]`.
2. `buildBody` inactive ⇒ output **byte-identical** to a golden of today (no `cachedContent` key, full contents/system/tools).
3. Lifecycle (httptest server stubbing `POST /cachedContents`): first eligible turn **creates**; same prefix next turn **reuses** (no second POST); changed tools/system **recreates** (delete + POST, new name); below-threshold prefix ⇒ **no POST, no reference**.
4. Create-failure ⇒ turn still sends the full request (no `cachedContent`), no error surfaced.

**Empirical (real Gemini on Vertex, manual):** large stable prefix cached ⇒ subsequent requests report `cachedContentTokenCount > 0` with a meaningful prompt-token reduction.

**Gate:** rebase onto current `origin/main` (already at HEAD here); `gofmt`/`build`/`vet`/`golangci-lint` clean; `go test ./...` green (only the accepted `TestUserSessionSendMessage` 404). PR body: "Closes #547".

## 7. Open questions

1. **Threshold value (`geminiMinCacheTokens`).** Default proposal: 32768 tokens (well above Vertex's hard minimum, so storage clearly pays off). Lower (e.g. 4096) caches more aggressively. Make it a tunable const, or also expose via config? Proposed: const for v1, revisit if needed. **Recommendation: const 32768.**
2. **Refresh strategy.** Options: (a) set a generous TTL once and only recreate on expiry (simplest); (b) `PATCH` ttl on each reuse when within a refresh window (keeps long sessions warm, one extra call near expiry). **Recommendation: (b)** with a window = 25% of TTL — matches the "persist across >1h gaps" goal at negligible cost.
3. **Cache-extension as the transcript grows.** A frozen snapshot only shadows the prefix as of creation; later turns send an ever-growing post-snapshot tail. Should the manager periodically **recreate** to re-absorb the grown tail (more shadowing, another create cost)? **Recommendation: defer** — recreate only on invalidation for v1; note "growing-tail re-cache" as a follow-up. Implicit caching still covers the recent tail.
4. **Resource `model` path form.** Vertex wants the publisher-qualified `projects/{p}/locations/{l}/publishers/google/models/{model}`; confirm against a live Vertex `cachedContents.create` (the empirical step) — adjust the formatter if the bare/short form is required for a given region.
5. **Session-close delete.** Best-effort delete on replacement is in scope; a delete on session teardown needs a hook in the session lifecycle (not just the connection). **Recommendation: rely on TTL expiry for teardown in v1** (resources self-expire); add an explicit teardown delete only if storage cost proves material.
