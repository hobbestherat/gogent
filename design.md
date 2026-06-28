# Design — Provider-agnostic prompt-cache reporting & cost model (issue #544)

Branch: `pair1/provider-agnostic-cache-reporting-and-co`. Closes #544.
Scope: gogent only. Stdlib-first. No new deps. No `go.mod` bump. No turbotui change.
This is the FOUNDATIONAL backbone for #545 (Anthropic cache control), #546 (TUI display),
#547 (Gemini explicit cache). Land FIRST among the cache cluster; rebase onto origin/main
at the gate (heavy `internal/model` overlap with #542/#543, which merge before us).

---

## 1. Problem (confirmed against current code)

gogent already parses a prompt-cache *read* count from every provider but collapses it into a
single `TokenUsage.CachedTokens int`, and Anthropic's cache-**write** count
(`cache_creation_input_tokens`) is silently dropped at `adapter.go:506` (only
`CacheReadInputTokens` is copied to `CachedTokens`; the write count is folded into
`PromptTokens` and otherwise lost). The agent budget (`agent.go:339`) counts all prompt tokens
at 1× — over-counting cache-hit turns and (for Anthropic) under-pricing cache writes.

Confirmed seam map (line numbers verified this session):

| # | Seam | Location |
|---|------|----------|
| 1 | OpenAI/DeepSeek read field | `TokenUsage.CachedTokens` `connection.go:454` |
| 2 | OpenAI/DeepSeek wire normalize (2 shapes) | `TokenUsage.UnmarshalJSON` `connection.go:467-493` |
| 3 | Anthropic parse — **write discarded** | `anthropicUsage.toTokenUsage` `adapter.go:493-508` |
| 4 | Gemini parse (read only) | `geminiUsageMetadata.toTokenUsage` `adapter.go:1165-1176` |
| 5 | Connection accumulator | `ModelStats.TotalCachedTokensIn` `connection.go:570` |
| 6 | Population (3 sites) | `complete()` `:1503`, `completeStream()` `:1582`, `CompleteWithStats()` `:1133` |
| 7 | Snapshot field | `StatsSnapshot.TotalCachedTokensIn` `connector.go:139` |
| 8 | Snapshot ops | `Add :156`, `Sub :178`, `IsReset :197`, `Snapshot :211`, `Carry :233` (connector.go) |
| 9 | Neutral report field | `ConnectorStat.CachedTokensIn` `stats.go:114` |
| 10 | Convert | `FromSnapshot` `stats.go:130` |
| 11 | Headline metric | `ConnectorStat.CacheHitPercent()` `stats.go:192-197` |
| 12 | Agent connector path | `recordConnectorUsage` `user_session.go:1057-1084` (carries whole snapshot — automatic) |
| 13 | Agent budget path — **not cache-aware** | `AddTokensUsed` call site `user_session.go:1037`; method `agent.go:339` |
| 14 | Display | `overallStats.CacheHitPct` `overall_stats.go:29,454,466,520` |
| — | Capabilities | `Capabilities` struct `provider.go:148-163`; exposed via `c.caps()` `connection.go:983` |

Two domain facts that shape the design: (a) only Anthropic reports a cache-write count and charges
a premium for it; (b) only Anthropic (breakpoints) and Gemini (cached content) accept *client-side*
cache-control directives — others cache automatically. This issue is REPORTING + COST only; the
request-side control is #545/#547.

---

## 2. Design

### 2.1 Normalized cache model on `TokenUsage` (seam 1–4)

New value type in `connection.go`, next to `TokenUsage`:

```go
// CacheStats is the provider-agnostic prompt-cache split. ReadTokens and
// WriteTokens are both subsets of TokenUsage.PromptTokens. WriteTokens is
// Anthropic-only (cache_creation_input_tokens) and 0 everywhere else.
type CacheStats struct {
    ReadTokens  int `json:"cache_read_tokens,omitempty"`
    WriteTokens int `json:"cache_write_tokens,omitempty"`
}
```

`TokenUsage` carries it as a named (comparable) field, replacing the bare `CachedTokens` field:

```go
type TokenUsage struct {
    PromptTokens     int
    CompletionTokens int
    TotalTokens      int
    Cache            CacheStats // normalized read/write
    ReasoningTokens  int
}

// CachedTokens is the back-compat read alias (was a field; now computed).
func (u TokenUsage) CachedTokens() int { return u.Cache.ReadTokens }
```

`TokenUsage` stays a **comparable struct** (CacheStats is two ints), so existing struct-equality
tests (`*resp.Usage != TokenUsage{...}`) keep compiling after the literal is updated to
`Cache: CacheStats{ReadTokens: …}`.

**JSON: flat + byte-identical + back-compat.** TokenUsage already owns a custom `UnmarshalJSON`;
we add a matching `MarshalJSON` so the persisted shape stays flat and unchanged for read-only
providers. The canonical persisted/back-compat key for reads remains the legacy `cached_tokens`;
writes add a new `cache_write_tokens` key:

- `MarshalJSON`: emit `prompt_tokens`/`completion_tokens`/`total_tokens`/`reasoning_tokens` as
  today, plus `cached_tokens` (= `Cache.ReadTokens`, `omitempty`) and `cache_write_tokens`
  (= `Cache.WriteTokens`, `omitempty`). When `WriteTokens==0` the bytes are **identical to today**.
- `UnmarshalJSON` (unify here, seam 2): read the two OpenAI-compat shapes
  (`prompt_tokens_details.cached_tokens`, top-level `prompt_cache_hit_tokens`) into
  `Cache.ReadTokens`; read legacy `cached_tokens` into `Cache.ReadTokens`; read
  `cache_write_tokens` into `Cache.WriteTokens`. Precedence unchanged (nested > top-level).

> The `CacheStats` json tags (`cache_read_tokens`/`cache_write_tokens`) are used only if a
> `CacheStats` is ever marshaled standalone; inside `TokenUsage` the custom marshaler maps reads to
> the legacy `cached_tokens` key for byte-identity. This is deliberate — see Open Questions for the
> nested-`"cache":{…}` alternative.

**Unify the two parse sites.** Anthropic (`adapter.go:493-508`) and Gemini (`adapter.go:1165-1176`)
build `TokenUsage` directly — they now populate `Cache: CacheStats{ReadTokens: …, WriteTokens: …}`
instead of `CachedTokens:`. The Anthropic mapping is the lossless fix:

```go
// anthropicUsage.toTokenUsage
Cache: CacheStats{
    ReadTokens:  u.CacheReadInputTokens,
    WriteTokens: u.CacheCreationInputTokens, // was DISCARDED
},
```

(`PromptTokens` keeps summing all three input counters, unchanged.) Gemini sets
`Cache: CacheStats{ReadTokens: u.CachedContentTokenCount}` (no writes). The OpenAI/DeepSeek/Z.AI/
OpenRouter shapes are all handled by the single unified `TokenUsage.UnmarshalJSON` — adding a new
provider's read shape is one edit there; the write shape is one field in its adapter.

### 2.2 Flow the read/write split through the pipeline (seams 5–10)

Add a `…CacheWriteTokensIn` sibling next to every existing `…CachedTokensIn` (reads), so the split
is carried end-to-end. Reads keep their current field names (zero churn for read-only providers):

- `ModelStats.TotalCacheWriteTokensIn` (seam 5, `connection.go:570`)
- `StatsSnapshot.TotalCacheWriteTokensIn` (seam 7) — added to `Add`, `Sub`, `Snapshot`, `Carry`,
  and `IsReset`'s `< 0` chain (seam 8). IsReset must include the new field or a write-counter
  rewind would be missed.
- `ConnectorStat.CacheWriteTokensIn` `json:"cache_write_tokens_in,omitempty"` (seam 9) — added to
  `Add`/`Sub` and to `FromSnapshot` (seam 10).
- Population (seam 6, 3 sites): alongside `c.Stats.TotalCachedTokensIn += usage.CachedTokens()` add
  `c.Stats.TotalCacheWriteTokensIn += usage.Cache.WriteTokens`. (`usage.CachedTokens()` is now the
  method.)

`CacheHitPercent()` (seam 11) is unchanged — it stays **read-based** (cache hits ÷ input). Add a
small breakdown helper for downstream (#546):

```go
// CacheWriteShare-style helpers are read by #546; CacheHitPercent semantics unchanged.
func (c ConnectorStat) CacheReadTokensIn() int  { return c.CachedTokensIn }  // alias for clarity
```
(Minimal — we mainly need the carried `CacheWriteTokensIn` field; the TUI work is #546.)

### 2.3 Cost-weighted input for the budget (seams 12–13)

Cost weight lives in the **model** layer (it is the only layer that knows the provider). Add
per-provider multipliers to `Capabilities` (locality: each provider file declares its own cache
behavior in one place), with a documented `0 ⇒ 1.0` fallback so unconfigured/test connectors price
everything at 1× and reduce byte-identically to today:

```go
type Capabilities struct {
    // …existing…
    CacheControl         CacheControlKind // §2.4, declaration only
    CacheReadMultiplier  float64          // 0 ⇒ 1.0 (no discount)
    CacheWriteMultiplier float64          // 0 ⇒ 1.0
}
```

Per-provider values (read / write), set in `provider_*.go`:

| provider | read mult | write mult | source |
|----------|-----------|------------|--------|
| anthropic, vertex-anthropic | 0.1 | 1.25 | cache-read 0.1×, 5-min cache-write 1.25× (1h=2× deferred to #545 when TTL known) |
| openai, openrouter | 0.5 | — (1.0) | cached input ≈ 0.5× |
| deepseek | 0.1 | — (1.0) | cache-hit ≈ 0.1× |
| vertex-native (Gemini) | discount | — (1.0) | cachedContent discounted |
| zai (GLM) | discount | — (1.0) | OpenAI-compat discount |

Cost formula (model package, e.g. `connection.go`):

```go
func (c CacheStats) costWeightedInput(prompt int, readMult, writeMult float64) int {
    base := prompt - c.ReadTokens - c.WriteTokens // full-price remainder
    return int(math.Round(float64(base) +
        float64(c.ReadTokens)*orOne(readMult) +
        float64(c.WriteTokens)*orOne(writeMult)))
}
```
(`orOne(0)==1.0`; `math` is stdlib.)

Expose it to the agent layer through an **optional connector capability**, exactly mirroring the
existing `MaxTokensReporter` pattern (`connection.go:1181`, used at `user_session.go:1130`):

```go
// model package
type CacheCostReporter interface { CostWeightedInput(u TokenUsage) int }
func (c *ModelConnection) CostWeightedInput(u TokenUsage) int {
    caps := c.caps()
    return u.Cache.costWeightedInput(u.PromptTokens, caps.CacheReadMultiplier, caps.CacheWriteMultiplier)
}
```

Budget call site (`user_session.go:1037`) becomes:

```go
prompt := resp.Usage.PromptTokens
if r, ok := sess.Model.(model.CacheCostReporter); ok {
    prompt = r.CostWeightedInput(*resp.Usage)
}
agent.AddTokensUsed(prompt, resp.Usage.CompletionTokens)
```

`AddTokensUsed(agent.go:339)` is **unchanged** (still `promptTokens + completionTokens`) — we feed it
a cost-weighted prompt figure rather than threading multipliers into the agent package. Test fakes
(`fakeStatsReporter`, budget_test) don't implement `CacheCostReporter`, so they fall back to raw
`PromptTokens` — byte-identical to today, and `budget_test.go` stays green untouched.

**Policy (documented in code + PR):** the budget switches to the **cost-weighted** input. Rationale:
the raw-token ceiling under-counts Anthropic cache writes (a real premium charge) and over-counts
cache-hit turns (reads are 0.1–0.5×), so the cost-weighted figure tracks actual spend far better.
This is a behavior change only for providers with non-1.0 multipliers; for a connector with no
declared multipliers it is identical to today.

### 2.4 Cache-control capability flag (seam — Capabilities) — DECLARATION only

```go
type CacheControlKind uint8
const (
    CacheControlNone          CacheControlKind = iota // automatic caching, no client directive
    CacheControlBreakpoints                           // Anthropic cache_control breakpoints
    CacheControlCachedContent                         // Gemini explicit cachedContent
)
```

Set `CacheControl:` per provider (anthropic/vertex-anthropic → `Breakpoints`; vertex-native →
`CachedContent`; everyone else → `None`/zero). This only **declares** capability for #545/#547;
no adapter emits a directive in this PR (emission stays wire-specific in each adapter).

---

## 3. Exact files / functions touched (gogent only)

- `internal/model/connection.go` — add `CacheStats`; reshape `TokenUsage` (Cache field +
  `CachedTokens()` method); add `MarshalJSON`, extend `UnmarshalJSON`; add
  `ModelStats.TotalCacheWriteTokensIn`; update 3 population sites (`:1133/:1503/:1582`); add
  `CacheStats.costWeightedInput`, `CacheCostReporter`, `ModelConnection.CostWeightedInput`.
- `internal/model/adapter.go` — `anthropicUsage.toTokenUsage` (retain write); `geminiUsageMetadata.toTokenUsage` (Cache.ReadTokens).
- `internal/model/connector.go` — `StatsSnapshot.TotalCacheWriteTokensIn` + `Add/Sub/Snapshot/Carry/IsReset`.
- `internal/model/provider.go` — `Capabilities` (+ multipliers, `CacheControl`); `CacheControlKind` enum.
- `internal/model/provider_anthropic.go`, `provider_openai.go`, `provider_vertex.go` — set multipliers + `CacheControl`.
- `internal/stats/stats.go` — `ConnectorStat.CacheWriteTokensIn` + `Add/Sub/FromSnapshot`; (optional read alias helper). `CacheHitPercent` untouched.
- `internal/agent/user_session.go` — cost-weighted prompt at the `AddTokensUsed` call site (`:1037`).
- `internal/agent/agent.go` — **no change** (AddTokensUsed signature/behavior preserved).
- Tests (update for the field→method + write split): `internal/model/cache_test.go`,
  `anthropic_test.go`, `vertex_anthropic_test.go`, `gemini_adapter_test.go`,
  `internal/stats/stats_test.go`, `report_model_test.go`; new coverage per §5.

**turbotui: no change.** turbotui is a sibling repo; gogent's TUI (`ui/tui/overall_stats.go`) is the
only consumer of these stats and stays read-based (`CacheHitPct`) for now (#546 owns any new
display). Persisted/serialized JSON stays additive (new `cache_write_tokens*` keys, `omitempty`),
so any external reader is forward-compatible. The repo seam is respected.

---

## 4. The four design gates

**(1) GOAL MATCH.** Exactly the issue's ask: one normalized `CacheStats{Read,Write}`; parse unified
in `TokenUsage.UnmarshalJSON` (OpenAI/DeepSeek) + the two adapter builders (Anthropic/Gemini);
Anthropic write **no longer discarded**; read/write split carried through seams 5–10; budget uses
cost-weighted input; `CacheControlKind` declared on `Capabilities`. No scope creep — request-side
cache control and TUI rendering are explicitly left to #545/#546/#547 (we only declare the flag and
carry the field).

**(2) USABILITY.** Caching becomes a first-class, lossless read/write concept; the budget reflects
real cost (reads discounted, writes premium) instead of a flat 1×. The previously-silent Anthropic
write is now retained and surfaced through the pipeline. `CacheHitPercent` keeps its established
read-based meaning (no surprise to existing users); the new write data is additive and ready for the
TUI in #546. Default/test connectors with no declared multipliers behave exactly as today.

**(3) NO REGRESSIONS.** Read-only providers reduce byte-identically: JSON marshal is unchanged when
`WriteTokens==0` (legacy `cached_tokens` key preserved; `cache_write_tokens` `omitempty`); cost
weight with `0⇒1.0` fallback and no-multiplier connectors equals raw tokens. `CacheHitPercent`
semantics unchanged. `IsReset` extended to the new field to preserve the monotonic-accumulator
invariant (a write-counter rewind must still trip a reset). `recordConnectorUsage` carries the whole
snapshot, so the new field flows with zero change there. `budget_test.go` and `fakeStatsReporter`
paths untouched (no `CacheCostReporter` ⇒ raw tokens). Main risk: the `CachedTokens` field→method
conversion is a wide but mechanical rename across call sites and tests — caught at compile time;
enumerated in §3/§5.

**(4) HOLISTIC across gogent + turbotui.** Cost weight lives in the model layer (only place that
knows the provider) and is surfaced via an optional interface that mirrors `MaxTokensReporter`, so
the agent package stays provider-agnostic. The stats pipeline keeps its neutral `ConnectorStat`
seam. turbotui is untouched and the JSON contract is additive/forward-compatible. gogent-only, no new
deps, no `go.mod` bump. Lands first in the cache cluster; rebase onto current origin/main at the gate
(conflicts with #542/#543 in `internal/model` are expected and handled by merge order).

---

## 5. Verification / test plan

- `TokenUsage` round-trips `CacheStats{Read,Write}` through marshal→unmarshal; write-only and
  read-only turns; legacy `{"cached_tokens":N}` still loads into `Cache.ReadTokens` (back-compat).
- Read-only marshal is **byte-identical** to today (golden string with `WriteTokens==0`).
- Per-provider normalization: OpenAI `prompt_tokens_details.cached_tokens`→read; DeepSeek
  `prompt_cache_hit_tokens`→read; Gemini `cachedContentTokenCount`→read; Anthropic
  `cache_read_input_tokens`→read **and** `cache_creation_input_tokens`→write (the previously-lost
  field — new assertion in `anthropic_test.go` / `vertex_anthropic_test.go`).
- `CostWeightedInput`: Anthropic prices reads 0.1× + writes 1.25×; non-Anthropic `WriteTokens==0`
  and read-multiplier reduce to expected; connector with no multipliers ⇒ raw `PromptTokens`.
- `StatsSnapshot` `Add`/`Sub`/`Carry` carry `TotalCacheWriteTokensIn`; `IsReset` trips on a negative
  write delta.
- `internal/stats`: `CacheHitPercent` unchanged; add a `CacheWriteTokensIn` assertion to the report
  test and `FromSnapshot` mapping test.
- Update existing literals: `cache_test.go`, `gemini_adapter_test.go:475`, `anthropic_test.go:198/216/345`
  (`CachedTokens` field→`CachedTokens()` / `Cache.ReadTokens`).
- Gate: `gofmt`/`go build`/`go vet`/`golangci-lint` (whole-repo, 0 NEW) clean; `go test ./...` green
  (run without `-race` per Pi5 constraint) — pre-existing `TestUserSessionSendMessage` 404 is the
  only acceptable failure.

---

## 6. Open questions

1. **JSON encoding of the split.** Recommended: flat with the legacy `cached_tokens` key for reads
   (byte-identical persistence, simplest back-compat). Alternative the issue's `CacheStats` json
   tags hint at: nested `"cache":{"cache_read_tokens":…,"cache_write_tokens":…}` — cleaner data
   model but NOT byte-identical and needs custom omitempty handling for the struct. Going with flat
   unless the maintainer prefers nested.
2. **`CachedTokens` field→method.** Recommended per the issue ("computed alias"), at the cost of a
   wide mechanical rename. The lower-churn alternative is keeping `CachedTokens int` as the stored
   read field and adding a sibling `CacheWriteTokens int` (no method, no custom marshal). Going with
   the method form to match the issue; flag if the rename footprint is unwanted.
3. **Anthropic write multiplier 1.25 vs 2.0.** The response can't distinguish 5-min vs 1-h cache, so
   the foundation uses 1.25 (5-min, the default breakpoint). Exact TTL-aware pricing is deferred to
   #545 where the TTL directive is known.
4. **Budget policy.** Recommended: switch the budget to cost-weighted input (per issue). If the
   maintainer wants the raw-token ceiling preserved, the cost-weighted figure can instead be exposed
   as a separate reported metric without feeding `AddTokensUsed`.
