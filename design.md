# Design — Provider-agnostic prompt-cache reporting & cost model (issue #544)

Branch: `pair1/provider-agnostic-cache-reporting-and-co`. Closes #544.
Scope: gogent only. Stdlib-first. No new deps. No `go.mod` bump. No turbotui change.
This is the FOUNDATIONAL backbone for #545 (Anthropic cache control), #546 (TUI display),
#547 (Gemini explicit cache). Land FIRST among the cache cluster; rebase onto origin/main
at the gate (heavy `internal/model` overlap with #542/#543, which merge before us).

> Hard dependency on #543: this design **reuses the per-model capability layer**
> (`ModelCaps` / `resolveModelCaps`, `caps.go` + `model_overrides.go`) that #543 introduces.
> #544 runs after #543 merges, so that layer exists when we land.

---

## 0. Round-2 revision log (resolves every round-1 concern — verify against the file, not the diff)

> Round-1 review keyed off an empty `git diff HEAD`; the round-2 content was committed, so the diff
> looks empty while the file *is* revised. Each concern below names the exact section/line that
> resolves it so re-review can confirm against the current file.

| # | Round-1 concern (gate) | Resolution in this file |
|---|---|---|
| 1 | DeepSeek=0.1 vs OpenAI=0.5 unimplementable from one shared `Capabilities` (gate 1) | §2.3 table row **deepseek** + §1 "Provider topology": DeepSeek discount moved to **per-(provider,model) `ModelCaps` exact rows** (`{APITypeOpenAI,"deepseek-chat",0.10}`), beating OpenAI's 0.50 provider default. The two are no longer claimed distinct on `Capabilities`. |
| 2 | `discount` is not a number for Gemini/Z.AI (gate 1) | §2.3 table: Gemini **0.25**, Z.AI **0.20 (PROVISIONAL, Open Q3)**, OpenRouter **1.0 documented-inaccurate**. No `discount` placeholders remain. |
| 3 | Writes silent in CSV; `writeConnectorCSV` never extended (gate 2) | §2.2 adds `set("cache_write_tokens_in", int64(c.CacheWriteTokensIn))` after `stats.go:347`; §3 lists the renderer; §5 report assertion now satisfiable. |
| 4 | Legacy `cached_tokens` unmarshal broken by field→method (alias loses the field) (gate 3) | §2.1 `raw` struct adds explicit `LegacyCachedTokens int json:"cached_tokens"` + `CacheWriteTokens` fields and a specified **three-way read precedence** (nested > `prompt_cache_hit_tokens` > legacy). |
| 5 | MarshalJSON byte-order unspecified (gate 3) | §2.1 pins key order — five existing keys keep today's reflection positions, `cache_write_tokens(omitempty)` appended **last** so the write==0 output is byte-identical; golden-string test in §5. |
| 6 | Rename enumeration incomplete (gate 3) | §3 + §5 now list **both** `issue487_failure_persist_test.go` files (`internal/model` `:165/:182`, `internal/gogent` `:61/:228/:381`). |
| 7 | Budget-fallback rationale mechanistically wrong (gate 3) | §2.3 "the real mechanism": `*ModelConnection` **does** satisfy `CacheCostReporter` (assertion succeeds); raw fallback is empty `caps()`→1.0 + nil `ModelCaps` overrides + no cache tokens, not interface non-implementation. |

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
| 11b | CSV/text renderer | `writeConnectorCSV` `stats.go:332-356` (emits `cached_tokens_in` `:347`) |
| 12 | Agent connector path | `recordConnectorUsage` `user_session.go:1057-1084` (carries whole snapshot — automatic) |
| 13 | Agent budget path — **not cache-aware** | `AddTokensUsed` call site `user_session.go:1037`; method `agent.go:339` |
| 14 | Display | `overallStats.CacheHitPct` `overall_stats.go:29,454,466,520` |
| — | Per-provider caps | `Capabilities` struct `provider.go:148-163`; exposed via `c.caps()` `connection.go:983` |
| — | Per-(provider,model) caps | `ModelCaps` / `resolveModelCaps` `caps.go:24,45`; rows in `model_overrides.go:28` (from #543) |

Two domain facts that shape the design: (a) only Anthropic reports a cache-write count and charges
a premium for it; (b) only Anthropic (breakpoints) and Gemini (cached content) accept *client-side*
cache-control directives. This issue is REPORTING + COST only; request-side control is #545/#547.

**Provider topology (verified — grep returns no DeepSeek provider).** `internal/model` registers
these `api_type`s: `openai`, `zai`, `openrouter` (`provider_openai.go:12/30/47`), `anthropic`
(`provider_anthropic.go`), `vertex-native` + `vertex-anthropic` (`provider_vertex.go`). **DeepSeek
is not a provider** — it is reached as a base-URL config on `api_type: openai`, so it shares
OpenAI's per-provider `Capabilities`. Distinguishing DeepSeek's deeper cache discount from OpenAI's
therefore *cannot* be done on `Capabilities` (one literal per api_type); it requires the
per-(provider,model) layer (§2.3).

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

**JSON: flat, byte-identical, back-compat — with the subtleties spelled out.** The field→method
change has a real trap: today `cached_tokens` round-trips because the `alias` inside
`UnmarshalJSON` *inherits the `CachedTokens int json:"cached_tokens"` field*. Once that field is
gone (replaced by `Cache CacheStats`, whose inner tags are `cache_read_tokens`/`cache_write_tokens`),
the alias **no longer carries `cached_tokens`**, so the legacy key would be silently dropped. The
custom marshaler/unmarshaler must therefore name the legacy key explicitly.

`UnmarshalJSON` (unify here, seam 2) — `raw` gains an explicit legacy field plus the write key:

```go
type alias TokenUsage // strips methods; NOTE: no longer carries cached_tokens
var raw struct {
    alias
    PromptTokensDetails *struct{ CachedTokens int `json:"cached_tokens"` } `json:"prompt_tokens_details"`
    PromptCacheHitTokens    int `json:"prompt_cache_hit_tokens"`           // DeepSeek wire
    LegacyCachedTokens      int `json:"cached_tokens"`                     // our own persisted turns
    CacheWriteTokens        int `json:"cache_write_tokens"`               // our own persisted writes
    CompletionTokensDetails *struct{ ReasoningTokens int `json:"reasoning_tokens"` } `json:"completion_tokens_details"`
}
```

Read precedence for `Cache.ReadTokens` is **explicit and three-way, most-authoritative first**:
1. nested `prompt_tokens_details.cached_tokens` (OpenAI/Z.AI/OpenRouter wire), if `> 0`;
2. else top-level `prompt_cache_hit_tokens` (DeepSeek wire), if `> 0`;
3. else legacy `cached_tokens` (our own persisted turns), if `> 0`.

`Cache.WriteTokens` ← `cache_write_tokens` (our own persisted writes; providers never send it — the
Anthropic write enters via the adapter, §below). Reasoning lifting unchanged.

`MarshalJSON` (new) — emit a flat object whose **first five keys keep today's exact reflection
positions** and append the one new key **last**, so byte-identity is a guarantee, not a hope:

```
prompt_tokens, completion_tokens, total_tokens, cached_tokens(=Cache.ReadTokens,omitempty),
reasoning_tokens(omitempty), cache_write_tokens(=Cache.WriteTokens,omitempty)
```

When `WriteTokens==0`, `cache_write_tokens` is omitted and the keys are exactly
`prompt_tokens, completion_tokens, total_tokens, cached_tokens, reasoning_tokens` — **byte-identical
to today's reflection output** (the existing five keys never move). A golden-string test (§5) pins
this. `cache_write_tokens` appears only on Anthropic write turns, appended after `reasoning_tokens`.

> The `CacheStats` inner json tags (`cache_read_tokens`/`cache_write_tokens`) are used only if a
> `CacheStats` were ever marshaled standalone; inside `TokenUsage` the custom marshaler maps reads
> to the legacy `cached_tokens` key for byte-identity. Deliberate — see Open Question 1.

**Unify the two adapter parse sites (seams 3–4).** Anthropic and Gemini build `TokenUsage` directly;
they now populate `Cache:` instead of `CachedTokens:`. The Anthropic mapping is the lossless fix:

```go
// anthropicUsage.toTokenUsage
Cache: CacheStats{
    ReadTokens:  u.CacheReadInputTokens,
    WriteTokens: u.CacheCreationInputTokens, // was DISCARDED
},
```

(`PromptTokens` keeps summing all three input counters, unchanged.) Gemini sets
`Cache: CacheStats{ReadTokens: u.CachedContentTokenCount}` (no writes). OpenAI/DeepSeek/Z.AI/
OpenRouter all flow through the single unified `TokenUsage.UnmarshalJSON`.

### 2.2 Flow the read/write split through the pipeline (seams 5–11b)

Add a `…CacheWriteTokensIn` sibling next to every existing `…CachedTokensIn` (reads), so the split
is carried end-to-end. Reads keep their current field names (zero churn for read-only providers):

- `ModelStats.TotalCacheWriteTokensIn` (seam 5).
- `StatsSnapshot.TotalCacheWriteTokensIn` (seam 7) — added to `Add`, `Sub`, `Snapshot`, `Carry`,
  and to `IsReset`'s `< 0` chain (seam 8): a write-counter rewind must trip a reset or the monotonic
  accumulator would miss a connector rebuild.
- Population (seam 6, 3 sites, under the existing nil-guard at `:1501`): alongside
  `c.Stats.TotalCachedTokensIn += usage.CachedTokens()` add
  `c.Stats.TotalCacheWriteTokensIn += usage.Cache.WriteTokens`.
- `ConnectorStat.CacheWriteTokensIn` `json:"cache_write_tokens_in,omitempty"` (seam 9) — added to
  `Add`/`Sub` and to `FromSnapshot` (seam 10).
- **`writeConnectorCSV` (seam 11b):** add one line so the write count is NOT silently dropped at the
  one human-readable surface (goal #2): `set("cache_write_tokens_in", int64(c.CacheWriteTokensIn))`,
  placed right after the existing `cached_tokens_in` line (`stats.go:347`). Without this the field
  exists in the data model and JSON but vanishes from the CSV/text report.

`CacheHitPercent()` (seam 11) is unchanged — it stays **read-based** (cache reads ÷ input). The
carried `CacheWriteTokensIn` field is what #546 will render; no new percent helper is needed now.

### 2.3 Cost-weighted input for the budget (seams 12–13)

Cost weight lives in the **model** layer (the only layer that knows the provider/model). Multipliers
use the codebase's established **two-axis** capability pattern:

- **Per-provider default → `Capabilities`** (one value per api_type), with `0 ⇒ 1.0` fallback:
  ```go
  type Capabilities struct {
      // …existing…
      CacheControl         CacheControlKind // §2.4, declaration only
      CacheReadMultiplier  float64          // 0 ⇒ 1.0 (no discount)
      CacheWriteMultiplier float64          // 0 ⇒ 1.0
  }
  ```
- **Per-(provider,model) override → `ModelCaps`** (#543's layer), for discounts that vary *within* a
  provider — i.e. DeepSeek on `api_type: openai`. Pointer fields so the zero value still means
  "inherit the provider default", preserving #543's no-regression invariant:
  ```go
  type ModelCaps struct {
      RejectsSampling      bool       // existing
      CacheReadMultiplier  *float64   // nil ⇒ inherit Capabilities
      CacheWriteMultiplier *float64   // nil ⇒ inherit Capabilities
  }
  ```

**Concrete multipliers (no `discount` placeholders).** Resolution order per turn:
`ModelCaps override (if non-nil)` → `Capabilities default (if non-zero)` → `1.0`.

| provider (api_type) | home | read | write | note |
|---|---|---|---|---|
| anthropic | Capabilities | **0.10** | **1.25** | 5-min write breakpoint; 1-h = 2.0 deferred to #545 (TTL not known at this layer) |
| vertex-anthropic | Capabilities | **0.10** | **1.25** | same as direct Anthropic |
| openai | Capabilities | **0.50** | 1.0 | cached input ≈ 0.5× |
| vertex-native (Gemini) | Capabilities | **0.25** | 1.0 | context-cache read ≈ 0.25× input |
| zai (GLM) | Capabilities | **0.20** | 1.0 | PROVISIONAL — confirm against Z.AI/GLM pricing (Open Q3) |
| openrouter | Capabilities | **1.0** | 1.0 | passthrough: real discount depends on the underlying model (e.g. Anthropic ≈0.1, and OpenRouter exposes no write count). Not knowable per-api_type → priced at 1.0 = **documented known-inaccuracy**, conservative (never under-counts budget) |
| **deepseek** (`api_type: openai`) | **ModelCaps exact rows** | **0.10** | 1.0 | distinguishes DeepSeek's deeper discount from native OpenAI's 0.5 — the case `Capabilities` alone cannot express |

DeepSeek rows added to `model_overrides.go` as exact `(provider, model)` entries (exact tier beats
the provider default), e.g. `{provider: APITypeOpenAI, model: "deepseek-chat", caps: ModelCaps{CacheReadMultiplier: ptr(0.10)}}`
and `"deepseek-reasoner"`. (Per-model OpenRouter refinement — e.g. Anthropic-via-OpenRouter at 0.1 —
is a future additive row, not blocking; called out, not silently assumed.)

Cost formula (model package):

```go
func (c CacheStats) costWeightedInput(prompt int, readMult, writeMult float64) int {
    base := prompt - c.ReadTokens - c.WriteTokens // full-price remainder
    return int(math.Round(float64(base) +
        float64(c.ReadTokens)*orOne(readMult) +
        float64(c.WriteTokens)*orOne(writeMult)))
}
```
(`orOne(0)==1.0`; `math` is stdlib. Read/Write are subsets of Prompt, so `base ≥ 0`.)

Expose to the agent layer via an optional connector capability, mirroring `MaxTokensReporter`
(`connection.go:1181`, used at `user_session.go:1130`):

```go
type CacheCostReporter interface { CostWeightedInput(u TokenUsage) int }

func (c *ModelConnection) CostWeightedInput(u TokenUsage) int {
    rd, wr := c.caps().CacheReadMultiplier, c.caps().CacheWriteMultiplier      // provider default
    if mc := resolveModelCaps(c.APIType, c.ModelName); mc.CacheReadMultiplier != nil {
        rd = *mc.CacheReadMultiplier                                            // per-model override
    }
    if mc := resolveModelCaps(c.APIType, c.ModelName); mc.CacheWriteMultiplier != nil {
        wr = *mc.CacheWriteMultiplier
    }
    return u.Cache.costWeightedInput(u.PromptTokens, rd, wr)
}
```

Budget call site (`user_session.go:1037`):

```go
prompt := resp.Usage.PromptTokens
if r, ok := sess.Model.(model.CacheCostReporter); ok {
    prompt = r.CostWeightedInput(*resp.Usage)
}
agent.AddTokensUsed(prompt, resp.Usage.CompletionTokens)
```

`AddTokensUsed` (`agent.go:339`) is **unchanged** — we feed it a cost-weighted prompt rather than
threading multipliers into the agent package. **Stats vs budget are intentionally different
figures:** `recordConnectorUsage`/the stats pipeline keep counting **raw** `TotalTokensIn` (what the
TUI shows); only the **budget** uses the cost-weighted prompt. This is by design — the displayed
token counts stay literal while the budget tracks spend.

**Why the budget/maxsteps tests stay green — the real mechanism** (corrected): `CostWeightedInput`
is a method on `*ModelConnection`, so *every* `*ModelConnection` — including the bare
`NewModelConnection()`+`SetURL` connection in `task_loop_test.go` / `budget_test.go` — **does**
satisfy `CacheCostReporter`; the type assertion **succeeds**. The fallback to raw tokens happens
*inside* `CostWeightedInput`: a hand-built connection has no provider, so `c.caps()` returns the
empty `Capabilities` (`connection.go:983-988`) ⇒ multipliers 0 ⇒ `orOne` ⇒ 1.0, `resolveModelCaps`
finds no row ⇒ nil overrides, and the fake usage carries no cache tokens ⇒ `costWeightedInput`
returns exactly `PromptTokens`. The non-`*ModelConnection` fakes (`fakeStatsReporter`) don't
implement the interface at all ⇒ also raw. Both paths reduce to today. NOTE for the future: a test
that attaches a real provider *and* returns cached tokens *will* see a shifted (cost-weighted)
budget — that is correct new behavior, not a regression.

**Policy (documented in code + PR):** the budget switches to cost-weighted input (per the issue's
recommendation). The raw-token ceiling under-counts Anthropic writes (a real premium charge) and
over-counts cache-hit turns; cost-weighted tracks spend. For any connector with no multipliers it is
identical to today.

### 2.4 Cache-control capability flag (Capabilities) — DECLARATION only

```go
type CacheControlKind uint8
const (
    CacheControlNone          CacheControlKind = iota // automatic caching, no client directive
    CacheControlBreakpoints                           // Anthropic cache_control breakpoints
    CacheControlCachedContent                         // Gemini explicit cachedContent
)
```

Set per api_type: anthropic/vertex-anthropic → `Breakpoints`; vertex-native → `CachedContent`;
openai/zai/openrouter → `None` (zero). This is per-api_type (the directive is wire/adapter-specific —
e.g. an Anthropic model reached via OpenRouter still goes through the OpenAI-compat adapter, which
emits no breakpoints), so `Capabilities` is the right home, not `ModelCaps`. Declaration only; no
adapter emits a directive in this PR (that is #545/#547).

---

## 3. Exact files / functions touched (gogent only)

- `internal/model/connection.go` — add `CacheStats`; reshape `TokenUsage` (Cache field +
  `CachedTokens()` method); add `MarshalJSON`, rewrite `UnmarshalJSON` (explicit legacy
  `cached_tokens` + `cache_write_tokens` raw fields, 3-way read precedence); add
  `ModelStats.TotalCacheWriteTokensIn`; update 3 population sites (`:1133/:1503/:1582`); add
  `orOne`, `CacheStats.costWeightedInput`, `CacheCostReporter`, `ModelConnection.CostWeightedInput`.
- `internal/model/adapter.go` — `anthropicUsage.toTokenUsage` (retain write); `geminiUsageMetadata.toTokenUsage` (Cache.ReadTokens).
- `internal/model/connector.go` — `StatsSnapshot.TotalCacheWriteTokensIn` + `Add/Sub/Snapshot/Carry/IsReset`.
- `internal/model/provider.go` — `Capabilities` (+ `CacheReadMultiplier`/`CacheWriteMultiplier`, `CacheControl`); `CacheControlKind` enum.
- `internal/model/caps.go` — `ModelCaps` (+ `*float64` multiplier overrides; resolution already tiered).
- `internal/model/model_overrides.go` — DeepSeek exact rows (read 0.10) + tiny `ptr(float64)` helper.
- `internal/model/provider_anthropic.go`, `provider_openai.go`, `provider_vertex.go` — set per-provider multipliers + `CacheControl`.
- `internal/stats/stats.go` — `ConnectorStat.CacheWriteTokensIn` + `Add/Sub/FromSnapshot`; **`writeConnectorCSV` new `cache_write_tokens_in` line**. `CacheHitPercent` untouched.
- `internal/agent/user_session.go` — cost-weighted prompt at the `AddTokensUsed` call site (`:1037`).
- `internal/agent/agent.go` — **no change** (AddTokensUsed signature/behavior preserved).

**Tests touched by the `CachedTokens` field→method rename — COMPLETE enumeration** (compile-time
breaks; verified by grep, includes the two files the prior draft missed):

- `internal/model/cache_test.go` — literals + reads (`:69`, `:105`, `:56`); add new coverage (§5).
- `internal/model/gemini_adapter_test.go:475` — struct literal → `Cache: CacheStats{ReadTokens: 2}`.
- `internal/model/anthropic_test.go:198,216,345` — reads → `.CachedTokens()` / `.Cache.ReadTokens`; add write assertion.
- `internal/model/vertex_anthropic_test.go` — add `cache_creation_input_tokens` → write assertion.
- `internal/model/issue487_failure_persist_test.go:165` (literal), `:182` (read).
- `internal/gogent/issue487_failure_persist_test.go:61` (literal `CachedTokens: cached` → `Cache: CacheStats{ReadTokens: cached}`), `:228`, `:381` (reads).
- `internal/stats/stats_test.go`, `internal/stats/report_model_test.go` — add `CacheWriteTokensIn` field carry + CSV/report assertions.

**turbotui: no change.** turbotui is a sibling repo with zero coupling to gogent's cache model
(verified: no gogent imports; its "cache" references are UI render caches). gogent's own TUI
(`ui/tui/overall_stats.go`) is the only consumer and stays read-based (`CacheHitPct`) — new display
is #546. Persisted/serialized JSON stays additive (`cache_write_tokens*` keys, `omitempty`), so any
external reader is forward-compatible **as long as MarshalJSON preserves reflection key order** (§2.1,
pinned by golden test). The repo seam is respected.

---

## 4. The four design gates

**(1) GOAL MATCH.** Exactly the issue's ask: one normalized `CacheStats{Read,Write}`; parse unified
in `TokenUsage.UnmarshalJSON` (OpenAI/DeepSeek) + the two adapter builders (Anthropic/Gemini);
Anthropic write **no longer discarded**; read/write split carried through seams 5–11b; budget uses
cost-weighted input; `CacheControlKind` declared. The cost table now uses **concrete** multipliers
and resolves the DeepSeek-vs-OpenAI conflict through the per-model layer rather than claiming an
impossible per-provider distinction; Gemini/Z.AI/OpenRouter inaccuracies are pinned to numbers or
documented, not hidden behind `discount`. No scope creep — request-side control deferred to
#545/#547 (we only declare the flag and carry the field).

**(2) USABILITY.** Caching becomes a first-class, lossless read/write concept; the budget reflects
real cost. The previously-silent Anthropic write is retained **and surfaced** — including the new
`cache_write_tokens_in` line in the CSV/text report, so it is not silently dropped at the one
human-readable surface. `CacheHitPercent` keeps its established read-based meaning. Write data is
additive and ready for #546.

**(3) NO REGRESSIONS.** Read-only providers reduce byte-identically: JSON marshal is byte-identical
when `WriteTokens==0` (legacy `cached_tokens` key + pinned reflection order; `cache_write_tokens`
`omitempty`), and the rewritten unmarshal explicitly restores the legacy `cached_tokens` field that
the alias loses in the field→method change (the real trap, now handled, with 3-way precedence
specified). Cost weight with `0/nil ⇒ 1.0` fallback equals raw tokens for unconfigured/test
connectors — and the green-ness of `budget_test.go`/the maxsteps suite is explained by the *correct*
mechanism (interface satisfied; raw fallback is empty-caps + nil overrides + no cache tokens, not
non-implementation). `CacheHitPercent` semantics unchanged. `IsReset` extended to the write field to
preserve the monotonic-accumulator invariant. `recordConnectorUsage` carries the whole snapshot, so
the new field flows with zero change there. Rename footprint is **completely** enumerated in §3
(including both `issue487_failure_persist_test.go` files / 5 references) and is compile-time-caught.

**(4) HOLISTIC across gogent + turbotui.** Multipliers use the existing two-axis capability
architecture (`Capabilities` per api_type + `ModelCaps` per provider×model) rather than a bolt-on,
so adding/correcting a provider's cache pricing is a one-line data edit in the same place #543 put
RejectsSampling. Cost weight stays in the model layer and is surfaced via an optional interface that
mirrors `MaxTokensReporter`, keeping the agent package provider-agnostic. The stats pipeline keeps
its neutral `ConnectorStat` seam. turbotui is untouched; the JSON contract is additive and
forward-compatible (key order pinned). gogent-only, no new deps (`math` is stdlib), no `go.mod` bump.
Lands first in the cache cluster; rebase onto current origin/main at the gate (#542/#543 overlap in
`internal/model` handled by merge order, and #543's `ModelCaps` layer is a hard prerequisite we
build on).

---

## 5. Verification / test plan

- `TokenUsage` round-trips `CacheStats{Read,Write}` through marshal→unmarshal: write-only,
  read-only, and both; legacy `{"cached_tokens":N}` still loads into `Cache.ReadTokens` (back-compat);
  `{"cache_write_tokens":N}` loads into `Cache.WriteTokens`.
- **Golden byte-identity:** `json.Marshal` of a read-only `TokenUsage` produces the exact today-bytes
  `{"prompt_tokens":…,"completion_tokens":…,"total_tokens":…,"cached_tokens":…}` (pins reflection
  key order); a write turn appends `cache_write_tokens` after `reasoning_tokens` (existing keys unmoved).
- 3-way read precedence: nested `prompt_tokens_details.cached_tokens` > `prompt_cache_hit_tokens` >
  legacy `cached_tokens`.
- Per-provider normalization: OpenAI nested→read; DeepSeek `prompt_cache_hit_tokens`→read; Gemini
  `cachedContentTokenCount`→read; Anthropic `cache_read_input_tokens`→read **and**
  `cache_creation_input_tokens`→write (the previously-lost field — new assertion in
  `anthropic_test.go` + `vertex_anthropic_test.go`).
- `CostWeightedInput`: Anthropic prices reads 0.10× + writes 1.25×; OpenAI 0.50× read; **DeepSeek
  model on `api_type: openai` resolves to 0.10× via the ModelCaps override** (proves the per-model
  path beats the OpenAI default); a connector with no provider ⇒ raw `PromptTokens`.
- `StatsSnapshot` `Add`/`Sub`/`Carry` carry `TotalCacheWriteTokensIn`; `IsReset` trips on a negative
  write delta.
- `internal/stats`: `CacheHitPercent` unchanged; `FromSnapshot` maps the write field; **`writeConnectorCSV`
  emits a `cache_write_tokens_in` record** (the §2.2 renderer line makes this assertion pass);
  report test asserts the write count.
- Update existing literals/reads per the §3 enumeration (incl. both `issue487_failure_persist_test.go`).
- Gate: `gofmt`/`go build`/`go vet`/`golangci-lint` (whole-repo, 0 NEW) clean; `go test ./...` green
  (run without `-race` per Pi5 constraint) — pre-existing `TestUserSessionSendMessage` 404 is the
  only acceptable failure.

---

## 6. Open questions

1. **JSON encoding of the split.** Recommended: flat with the legacy `cached_tokens` key for reads
   (byte-identical persistence, simplest back-compat; `CacheStats` inner tags then unused in this
   path). Alternative: nested `"cache":{…}` — cleaner data model but NOT byte-identical and needs
   custom struct omitempty. Going flat unless the maintainer prefers nested.
2. **`CachedTokens` field→method.** Recommended per the issue ("computed alias"), at the cost of a
   wide-but-mechanical, fully-enumerated rename. Lower-churn alternative: keep `CachedTokens int` as
   the stored read field and add a sibling `CacheWriteTokens int` (no method, no custom marshal,
   smaller diff, but diverges from the issue's stated shape). Going with the method form; flag if the
   footprint is unwanted.
3. **Z.AI/GLM read multiplier (0.20 provisional)** and **OpenRouter (1.0, documented-inaccurate).**
   Z.AI's exact cache-hit price needs confirmation; OpenRouter's true discount is per-underlying-model
   and unknowable at the api_type layer (per-model `ModelCaps` rows can refine it later). Confirm the
   Z.AI number or accept the provisional value.
4. **Anthropic write multiplier 1.25 vs 2.0.** The response can't distinguish 5-min vs 1-h cache, so
   the foundation uses 1.25 (5-min default breakpoint). TTL-aware pricing is deferred to #545.
5. **Budget policy.** Recommended: switch the budget to cost-weighted input (per issue). If the
   maintainer wants the raw ceiling preserved, the cost-weighted figure can instead be exposed as a
   separate reported metric without feeding `AddTokensUsed`.
