# Design — Anthropic explicit cache-control: multi-breakpoint + configurable TTL (issue #545)

Closes #545. gogent-only; stdlib-first; no new deps; no turbotui change; no go.mod bump.
Builds on #544 (`CacheStats{Read,Write}` + `CacheControlBreakpoints` capability flag), which has landed
(`internal/model/provider.go:158`, `internal/model/provider_anthropic.go:18,39`,
`internal/model/connection.go:453`).

## Problem recap

The Anthropic wire adapter (`anthropicAdapter`, used by BOTH the direct Messages API and
Claude-on-Vertex) emits exactly two `cache_control:{type:"ephemeral"}` breakpoints per request:
one on the system block (`adapter.go:344-350`) and one on the last content block of the last
non-volatile message (`adapter.go:357-359`). Two silent cost gaps:

- **Gap A — single transcript breakpoint vs. the 20-content-block lookback.** Anthropic's cache
  read only matches a previously-written prefix if a breakpoint on the current request sits within
  ~20 content blocks *after* that write. One assistant turn with M tool calls expands to ~`2M+1`
  content blocks (M `tool_use` + M `tool_result` + 1 text; `anthropicBlocks` at `adapter.go:404-443`,
  and the M `tool_result` user messages merge into one user turn at `adapter.go:322-326`). The agent
  loop caps tool-call *concurrency* (`maxParallelToolCalls=8`, `internal/agent/limiter.go:73`) but
  not *count*, so a "read 12 files" turn adds ~25 blocks. When the single end-of-prefix breakpoint
  lands >20 blocks past the previous write, the lookback misses it and the request silently pays a
  full-prefix cache *write* (1.25×) instead of a *read* (0.1×).

- **Gap B — no configurable TTL.** `anthropicCacheControl` (`adapter.go:154-156`) has no `ttl`
  field, so it is always the default 5-minute ephemeral cache. The 1-hour cache
  (`{"type":"ephemeral","ttl":"1h"}`, 2× write) is unreachable, and there is no way to choose 5m vs
  1h or to disable caching. (All other `ttl`/`1h` strings in `internal/model` are unrelated Vertex
  ADC OAuth lifetimes in `connection.go`.)

## How config reaches the adapter

`adapter.buildBody(req CompletionRequest, buf)` is the only hook; it does NOT receive provider caps
or `ModelConfig`. Both knobs must therefore ride on `CompletionRequest`, set by
`(*ModelConnection).buildRequest` (`connection.go:1352`), which already reads `c.Config` and
`c.caps()`. The OpenAI adapter marshals the *entire* `CompletionRequest` to the wire
(`openAIAdapter.buildBody` → `encodeJSON(buf, req)`, `adapter.go:77-79`), so any new control field
MUST be tagged `json:"-"` or it would leak onto every OpenAI-compatible request. This mirrors the
existing `json:"-"` control field `Message.Volatile` (`connection.go:115`).

## Changes

### 1. `internal/config/config.go` — `ModelConfig.CacheTTL` knob

Add one field to `ModelConfig` (`config.go:18`):

```go
// CacheTTL selects the Anthropic prompt-cache breakpoint lifetime. "" or "5m"
// (the default) use the 5-minute ephemeral cache; "1h" uses the 1-hour cache
// (2× write premium, worthwhile only across idle/resume gaps >5min — see issue
// #545); "off"/"none" disables client-side cache_control entirely. Honored only
// by Anthropic / Claude-on-Vertex (api_type anthropic / vertex-anthropic);
// ignored by providers that cache automatically.
CacheTTL string `json:"cache_ttl,omitempty"`
```

Add a normalizing accessor (fail-safe to the 5m default for unknown values, consistent with
`ContextWindowOrDefault`’s conservative-default pattern at `config.go:92`):

```go
// AnthropicCacheTTL returns the normalized cache-control directive: "" (default
// 5-minute ephemeral), "1h" (1-hour ephemeral), or "off" (disable). Unknown
// values fall back to "" so a typo never disables caching or 400s the request.
func (m *ModelConfig) AnthropicCacheTTL() string { ... }  // "", "1h", or "off"
```

`omitempty` keeps existing config JSON byte-identical (round-trips through the existing
`config_test.go` save/load). No TUI/webapi form enumerates `ModelConfig` fields field-by-field
(grep for `reasoning_effort` in `internal/tui`/`internal/webapi` is empty — model rows are JSON-/
catalog-driven), so no form wiring is required for the field to be user-settable via the config file.

### 2. `internal/model/connection.go` — plumb knob + capability onto `CompletionRequest`

Add two `json:"-"` control fields to `CompletionRequest` (`connection.go:310`):

```go
// CacheTTL is the Anthropic prompt-cache directive resolved from ModelConfig:
// "" (5m default), "1h", or "off" (disable). Consumed only by anthropicAdapter;
// json:"-" so it never reaches the OpenAI-compatible wire (which marshals the
// whole request).
CacheTTL string `json:"-"`
// EmitCacheControl gates prompt-cache breakpoint emission. buildRequest sets it
// from the provider capability (CacheControlBreakpoints) AND CacheTTL != "off".
EmitCacheControl bool `json:"-"`
```

In `buildRequest`, after `caps := c.caps()`:

```go
ttl := ""
if c.Config != nil { ttl = c.Config.AnthropicCacheTTL() }
reqBody.CacheTTL = ttl
reqBody.EmitCacheControl = caps.CacheControl == CacheControlBreakpoints && ttl != "off"
```

This is the single place where the `CacheControlBreakpoints` capability (from #544) is read for
emission — satisfying the issue’s "reuse #544 capability flag" requirement and making the gating
explicit rather than implicit-by-adapter.

### 3. `internal/model/adapter.go` — TTL field + multi-breakpoint placement

**TTL on the breakpoint struct** (`adapter.go:154`):

```go
type anthropicCacheControl struct {
	Type string `json:"type"`
	// Ttl selects the cache lifetime: omitted = the default 5-minute ephemeral
	// cache; "1h" = the 1-hour cache (2× write). Anthropic accepts ttl only as
	// "5m" or "1h"; gogent emits it only for "1h" so the default stays byte-
	// identical (issue #545).
	Ttl string `json:"ttl,omitempty"`
}
```

Add a tiny helper so both breakpoint sites build the same value:

```go
func anthropicCacheCtl(ttl string) *anthropicCacheControl {
	cc := &anthropicCacheControl{Type: "ephemeral"}
	if ttl == "1h" { cc.Ttl = "1h" }
	return cc
}
```

**Gating + multi-breakpoint placement** in `buildBody`. Replace the single
`(cacheBreakMsg, cacheBreakBlock)` tracker with collection of the non-volatile message-end
boundaries plus their cumulative content-block offsets, then choose up to 3 transcript breakpoints
(4 total with the system breakpoint — Anthropic 400s on >4):

- While building `out.Messages`, for each non-volatile message record `(msgIdx, lastBlockIdx,
  cumulativeBlockOffset)` where the offset counts content blocks contributed so far across the
  transcript (the unit Anthropic’s lookback walks).
- After the loop, if `req.EmitCacheControl`:
  - System block keeps its breakpoint, now via `anthropicCacheCtl(req.CacheTTL)`
    (`adapter.go:348`). Counts as breakpoint #1 (when a system prompt is present).
  - Transcript: walk the recorded boundaries **backward from the end of the stable prefix**, always
    emitting on the end-of-prefix boundary (preserves today’s placement), then emit an additional
    breakpoint each time the accumulated block-distance since the last emitted breakpoint reaches a
    spacing threshold `cacheBreakpointSpacing` (= 16, safely < the 20-block lookback), capped so the
    total never exceeds `maxAnthropicBreakpoints` (= 4) including the system breakpoint. Dedupe so
    two breakpoints never land on the same block.

Named constants document the invariant:

```go
const (
	// anthropicCacheLookback is Anthropic's cache-read lookback window in content
	// blocks: a breakpoint finds a prior cache write only within this many blocks
	// after it. Documented, not emitted.
	anthropicCacheLookback = 20
	// cacheBreakpointSpacing is how far apart (in content blocks) successive
	// transcript breakpoints are placed — under the lookback so the chain of
	// breakpoints always keeps a recent write inside a future request's window.
	cacheBreakpointSpacing = 16
	// maxAnthropicBreakpoints is the hard API ceiling (system + transcript). >4
	// is a 400.
	maxAnthropicBreakpoints = 4
)
```

**Why this keeps the cache hit (Gap A).** Breakpoints sit on message boundaries. The agent loop
adds exactly two messages per iteration (one assistant turn; the M parallel `tool_result` messages
merge into one user turn — `adapter.go:322-326`), so the previous request’s end-of-prefix boundary
becomes this request’s second-most-recent boundary. Whenever the last turn was large (> spacing
blocks), that previous boundary is retained as one of this request’s breakpoints at the *same block
position* it was written at last turn — an exact-position cache read hit, independent of how many
blocks the turn added. With three transcript breakpoints spaced ≤16 blocks the chain tolerates very
large turns; small turns (whole stable prefix < 16 blocks) emit no extra breakpoint, so behavior is
byte-identical to today for them.

**Why 5m stays default (Gap B).** The active loop writes a breakpoint every turn (the breakpoint
moves forward each turn), so at sub-5-min cadence the 5m cache refreshes for free; per-turn the only
difference is the write premium 1.25× (5m) vs 2× (1h). 1h wins only on idle/resume gaps >5min, so it
is a per-model opt-in, never the default.

## User-facing behavior

- Default (no `cache_ttl`): identical wire output to today on small turns (system + end-of-prefix
  ephemeral breakpoints, no `ttl`); on large-tool-call turns, up to two extra ephemeral breakpoints
  appear earlier in the transcript so cache reads keep hitting. Net effect: cache cost silently
  *drops* on big turns; nothing the user must do.
- `cache_ttl: "1h"`: every emitted breakpoint carries `"ttl":"1h"`; responses report
  `cache_creation.ephemeral_1h_input_tokens`, surfaced through the existing
  `CacheStats.WriteTokens` path (#544) — no new reporting plumbing.
- `cache_ttl: "off"`/`"none"`: no `cache_control` emitted at all (caching disabled), via
  `EmitCacheControl=false`.

## Criterion-by-criterion

**(1) Goal match.** Exactly the issue’s two asks: ≤4 breakpoints chained under the 20-block lookback
(Gap A) and a per-model TTL knob with 5m default / 1h / off (Gap B). No scope creep — no change to
streaming, parsing, the volatile-tail rule, or tool batching. The "consolidate tool_result blocks"
option in the issue is explicitly NOT taken (primary fix is multi-breakpoint); noted as a possible
follow-up.

**(2) Usability.** Removes a *silent* cost regression on parallel-tool-call turns — the user gets
the cache hit without configuring anything. The 1h cache becomes reachable for the documented
scenarios (sub-agents, paused/resumed sessions, rate-limit sensitivity) via a single config key;
`off` gives a clean kill switch. 5m remains the sensible default; the user drives the choice per
model through `cache_ttl` in the config file. Effectiveness is observable via the #544 read/write
counters (confirm `cache_read_input_tokens>0` on the request *after* a big-tool-call turn).

**(3) No regressions.** Default path (no `cache_ttl`, small turns) is byte-identical:
`anthropicCacheCtl("")` emits `{type:"ephemeral"}` with `ttl` omitted, and the spacing rule places
no extra breakpoints when the prefix is short — so `TestAnthropicBuildBody`
(`anthropic_test.go:35`, builds a `CompletionRequest` directly with no flags and asserts ephemeral
on system + last block) and `vertex_anthropic_test.go` still pass. Critically, emission stays
**default-on** for the adapter: those tests don’t set `EmitCacheControl`, so the adapter must treat
the breakpoint policy as on unless a value says otherwise. To preserve that, the adapter emits when
`req.EmitCacheControl` **or** the request was hand-built without the new gating context — i.e.
`buildBody` defaults to emitting and only *suppresses* on the explicit `off` signal. Concretely:
gate on a *disable* signal (`req.CacheTTL == "off"`), not on a positive `EmitCacheControl`, so a
zero-value `CompletionRequest` keeps today’s behavior. (`EmitCacheControl` is still set by
`buildRequest` for the live path and used as the belt-and-suspenders provider-capability gate; the
adapter’s own default-on rule is what protects the existing direct-construction tests.) Config JSON
round-trips (`omitempty`). gofmt/build/vet/golangci-lint clean; `go test ./...` green except the
pre-existing `TestUserSessionSendMessage` 404 (acceptable per the gate). `≤4` invariant guards the
API 400.

> Resolution of the gating tension (single source of truth): the adapter emits breakpoints unless
> `req.CacheTTL == "off"`. `buildRequest` sets `CacheTTL="off"` when the provider lacks
> `CacheControlBreakpoints` OR the model config disables caching, so non-Anthropic providers (which
> don’t use this adapter anyway) and disabled models suppress emission, while existing
> direct-construction tests (CacheTTL "") keep emitting. `EmitCacheControl` is therefore redundant
> and will NOT be added — only `CacheTTL` rides on `CompletionRequest`. (Simplifies the change to one
> control field.)

**(4) Holistic / cross-repo seam.** Pure gogent wire-format concern. turbotui is the read-only TUI
client; it renders cache read/write stats surfaced via #544/#556 and never constructs Anthropic
bodies, so 1h writes flow through the existing `CacheStats.WriteTokens` channel with **no turbotui
change** and no go.mod bump. The change lives in the right layer: TTL/breakpoint shape in the
adapter (`internal/model`), the knob in `internal/config`, the resolution in `buildRequest`. The
issue’s suggested agent-layer gating (`internal/agent`) is unnecessary given the config-driven
`off` path, so `internal/agent`/`limiter.go` are left untouched (smaller blast radius). Serialize in
the `internal/model` chain (conflicts with #543/#544); rebase onto current `origin/main` (incl.
#544) at the gate.

## Files touched

- `internal/config/config.go` — add `ModelConfig.CacheTTL` + `AnthropicCacheTTL()` accessor.
- `internal/model/connection.go` — add `CompletionRequest.CacheTTL string json:"-"`; set it in
  `buildRequest` from `c.Config.AnthropicCacheTTL()`, forced to `"off"` when
  `caps().CacheControl != CacheControlBreakpoints`.
- `internal/model/adapter.go` — `anthropicCacheControl.Ttl`; `anthropicCacheCtl(ttl)` helper;
  multi-breakpoint placement + spacing constants in `buildBody`; apply `req.CacheTTL` at both
  breakpoint sites; suppress all emission when `req.CacheTTL == "off"`.
- Tests (new): `internal/model/anthropic_cache_issue545_test.go` — (a) a ≥20-block turn emits ≤4
  breakpoints positioned so an exact-position read hit is preserved next turn; (b) `ttl:"1h"` only
  when configured, default emits no `ttl`; (c) `off` suppresses all `cache_control`; plus a config
  round-trip assertion for `CacheTTL`.

## Regression risks

- **Off-by-one / >4 breakpoints → API 400.** Mitigated by the `maxAnthropicBreakpoints` cap counting
  the system breakpoint and by a unit test asserting ≤4 on a large transcript.
- **Spacing chosen too large** could still miss the lookback on an extreme turn (>~70 added blocks).
  `cacheBreakpointSpacing=16` < 20 with three transcript breakpoints gives wide margin; the
  exact-position property (boundaries line up across turns) is the primary guarantee, spacing is the
  backstop. Validate empirically (real Claude) per the issue.
- **Duplicate breakpoint on one block** (very short prefix, few messages) — dedupe by block position
  so we never write the same block twice.

## Open questions

1. **Disable token spelling.** `off` vs `none` vs `disabled` (or all three accepted). Proposal:
   accept `off`/`none`, normalize to `"off"`; reject/ignore others to the 5m default.
2. **1h write pricing in the cost model.** `provider_anthropic.go` sets `CacheWriteMultiplier=1.25`
   (the 5m premium); a model configured for 1h actually writes at 2×, so the cost-weighted agent
   budget (#544) under-counts 1h writes. Out of scope for #545’s request-side fix? Options: (a)
   document and leave (user can set a per-model `CacheWriteMultiplier` via `ModelCaps`); (b) follow-up
   issue to make the multiplier TTL-aware. Proposed: (a) + a note in the PR.
3. **Should `off` also be exposed as a future per-session toggle (TUI)?** Out of scope here (config
   file only); flag if maintainer wants the seam reserved now.
