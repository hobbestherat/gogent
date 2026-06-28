# Design — Per-model capability layer (issue #543)

**Branch:** `pair1/per-model-capability-layer-bug-fix-refac`
**Closes #543.** This is a **fix** (every direct-Anthropic `claude-opus-4-8` request 400s) carried on a small, well-contained **refactor** (introduce a `(provider × model)` capability seam) that the fix needs in order to live in the right place.

---

## 1. The bug, precisely

A direct-Anthropic config (`api_type: anthropic`, `temperature: 0.7`) for `claude-opus-4-8` 400s every request:

```
{"error":{"type":"invalid_request_error","message":"`temperature` is deprecated for this model."}}
```

The *mere presence* of `temperature` (even `0`) triggers it on current-gen Claude. Two locations conspire:

1. **`internal/model/connection.go` `buildRequest` (~L1290).** The sampling gate is
   `if !reasoning || !caps.ReasoningRejectsTemperature`. For this config `IsReasoningModel()` is **false** (no `reasoning_effort`, no `thinking` toggle — just a temperature), so `!reasoning` is `true` and the gate **always passes** — `temperature` (and `top_p`) are placed on the request *regardless of any capability flag*. The sampling decision is **fused to the reasoning concept**; "modern Claude rejects temperature" is not a reasoning property, so there is nowhere to express it. (Even registering `ReasoningRejectsTemperature: true` on the direct provider would not help: `!reasoning` short-circuits first.)

2. **`internal/model/adapter.go` `anthropicAdapter.buildBody` (~L257-261).** The non-Vertex branch forwards `out.Temperature = req.Temperature; out.TopP = req.TopP` verbatim. The Vertex branch ~10 lines above already drops both ("modern Claude rejects temperature/top_p"). Same model family, same wire format, **two different behaviors** — that drift is the bug.

The Vertex adapter proves the knowledge already exists in the codebase; it is just trapped in an adapter-instance branch (`if a.vertex`) instead of being data the direct path can read too.

## 2. Root architectural gap (why a capability *layer*, not a one-liner)

gogent has a clean **per-provider** layer (`provider` composes `adapter`/`caps`/`endpoints`/`auth`/`lister`/`validate`, keyed by `api_type`; `Capabilities` is 6 flags read uniformly by `buildRequest`). It has **no per-model axis**: `providerFor(APIType)` never sees a model name, and `Capabilities` is one struct per provider. The only per-model signal is the config-derived `IsReasoningModel()` boolean, which is fused to reasoning.

So model-family quirks are smuggled three inconsistent ways: (1) fused to a config boolean (`max_completion_tokens`, temperature drop gated on `IsReasoningModel()`); (2) adapter-instance branches (`anthropicAdapter{vertex}` drops sampling, rewrites `thinking`); (3) comments with no code (`2.5 Pro cannot fully disable thinking`). A genuine per-model quirk like "opus rejects temperature" has nowhere to go short of a whole-provider flag (hits every sibling model) or a hard-coded branch. The minimal fix that puts the decision in the *right place* is a `(provider, model)` capability lookup — which is exactly what the issue asks for.

Capability data is keyed on **(provider × model)**, not model alone: the empirical scan in `modelsdev-cache.json` shows the same model id carrying different capability signatures across providers. The lookup mirrors the models.dev structure `data[provider].models[model]`.

---

## 3. Proposed design

### 3.1 New types — `internal/model/caps.go`

```go
// ModelCaps reports per-(provider,model) wire quirks that the per-provider
// Capabilities struct cannot express, because they vary by model within one
// provider. The zero value means "no known quirk — inherit every default",
// so a model with no entry behaves byte-identically to today.
type ModelCaps struct {
    // RejectsSampling drops temperature AND top_p from the request. Current-gen
    // Claude rejects the mere presence of either (a 400, even temperature:0),
    // independent of whether reasoning is enabled.
    RejectsSampling bool
    // (room to grow as quirks migrate in: thinking-rewrite mode, token-param,
    //  disable-thinking-unsupported, …  — see §6 / steps 2-3, out of step-1 scope)
}
```

`ModelCaps` is deliberately a **separate** struct from `Capabilities`: `Capabilities` is per-provider and read off `provider.caps`; `ModelCaps` is resolved per request from `(api_type, model)`. Keeping them separate avoids implying every flag has a per-model override and keeps the empty-value semantics ("inherit") crisp.

### 3.2 Resolution — `resolveModelCaps` in `caps.go`

```go
func resolveModelCaps(apiType APIType, model string) ModelCaps
```

**Tiered, authoritative → default, most-specific → least-specific:**

1. **Curated override table** (`internal/model/model_overrides.go`) — the **authoritative** tier. Within it, matched most-specific first:
   - **(provider, model)** exact — most specific.
   - **(provider, "")** provider-wildcard — "every model on this provider". This is the data row that *replaces* the `if a.vertex` blanket sampling drop (see §3.4).
   - **("", model)** model-only — applies **across providers** (one `claude-opus-4-8` row fixes both `anthropic` and `vertex-anthropic`).
2. **models.dev catalog booleans** — the **default** tier (step 3, §6). Override beats catalog *on purpose*: the catalog carries a `temperature` bool but **no top_p, no deprecation status**, and is stale on fresh deprecations (its `claude-opus-4-1` entry still says `temperature:true`). No vendor publishes a machine-readable per-parameter accepted/rejected schema — hence the curated table is authoritative.
3. **Empty `ModelCaps`** — nothing matched; inherit every default.

For **step 1** the function consults **only** the override table (tier 1); tiers 2 is wired in step 3 and is out of the minimum scope.

### 3.3 The override table — `internal/model/model_overrides.go`

Step-1 seed expresses exactly the known quirk, "current-gen Claude rejects sampling params":

```go
var modelOverrides = []modelOverride{
    // Current-gen Claude rejects temperature/top_p (a 400 on mere presence,
    // even temperature:0). Model-only rows apply across BOTH the direct
    // Anthropic API and Claude-on-Vertex, so direct == Vertex for the same model.
    {model: "claude-opus-4-8", caps: ModelCaps{RejectsSampling: true}},
    // … the other current-gen Claude families gogent targets (opus-4-x,
    //   sonnet-4-x, haiku-4-x, 3-7-*) as confirmed-deprecated rows …

    // Provider-wildcard: every Claude served over Vertex drops sampling,
    // replacing the former `if a.vertex` adapter branch verbatim so no
    // Vertex model regresses regardless of generation (see §3.4 / §5).
    {provider: APITypeVertexAnthropic, caps: ModelCaps{RejectsSampling: true}},
}
```

Matching detail (documented in code): match is by exact model id; a dated snapshot (`claude-opus-4-8@20260…`, `…-4-5@20251101`) is normalized to its base id before lookup so a pinned snapshot inherits its family's row. Lookup is linear over a handful of rows — trivial cost, no map ceremony, and it keeps the "one data row" readability the DoD asks for.

### 3.4 `buildRequest` change (`connection.go` ~L1287)

Resolve once and fold the model quirk into the existing gate:

```go
mc := resolveModelCaps(c.providerAPIType(), c.ModelName)

// Drop sampling params when EITHER the model rejects them outright (current-gen
// Claude — true regardless of reasoning) OR this is a reasoning model on a
// provider that rejects a custom temperature (OpenAI o-series). Pointer fields,
// so a deliberate temperature:0 still survives on models that accept it.
dropSampling := mc.RejectsSampling || (reasoning && caps.ReasoningRejectsTemperature)
if !dropSampling {
    t := temperature
    reqBody.Temperature = &t
    if topP > 0 {
        p := topP
        reqBody.TopP = &p
    }
}
```

`c.providerAPIType()` is a tiny accessor returning `c.provider.apiType` (falling back to `c.APIType`); `c.ModelName` already holds `modelConfig.Model` (set at `connection.go:756`). This is the `(provider, model)` pair the lookup needs — no signature change to `buildRequest`, no plumbing through call sites.

**Empty-resolution invariant:** when `resolveModelCaps` returns the zero value, `dropSampling` reduces to exactly the old `reasoning && caps.ReasoningRejectsTemperature`, i.e. the gate is byte-identical to today for every model without a row.

### 3.5 `adapter.go` change — remove the branch, forward uniformly

The decision now lives in `buildRequest`, so `anthropicAdapter.buildBody` forwards whatever it is handed on **both** paths (a nil pointer is omitted by `omitempty`, so a dropped param simply does not appear):

```go
out.Temperature = req.Temperature   // nil for sampling-rejecting models → omitted
out.TopP = req.TopP
if a.vertex {
    out.AnthropicVersion = vertexAnthropicVersion
    if req.Thinking != nil && req.Thinking.Type == "enabled" {
        out.Thinking = &anthropicThinking{Type: "adaptive", Display: "summarized"}
    }
} else {
    out.Model = req.Model
}
```

The Vertex `// modern Claude rejects temperature/top_p — dropped` branch is **deleted**; "opus rejects temperature" is now one data row, not an adapter branch. The provider-wildcard row (§3.3) guarantees every Vertex Claude still drops sampling, so production wire output for Vertex is unchanged.

> The `thinking{enabled}→{adaptive}` rewrite stays in the adapter for step 1 (it is a wire-shape translation, not a sampling quirk); migrating it to data is step 2 (§6), explicitly optional.

---

## 4. Design criteria

### (1) Goal match — exactly the ask, no more
- A `(provider, model)` capability lookup (`ModelCaps` + `resolveModelCaps`) consulted by `buildRequest` now **exists**. ✔
- Direct `claude-opus-4-8` with temperature/top_p set resolves to a wire body with **no** temperature/top_p → no 400. ✔
- Direct == Vertex for the same model (both routed through the same data + the same uniform adapter forwarding). ✔
- The `anthropicAdapter{vertex}` sampling branch is **removed**; the decision is data, not a branch. ✔
- Scope is held to step 1 (the mandatory core). Steps 2-3 are described but flagged optional and only land if they fit the gate. No unrelated feature creep; no turbotui change; no `go.mod` bump; no new deps (stdlib + existing types only).

### (2) Usability — right thing surfaced, user drives input
- **Out of the box:** Opus 4.8 over direct Anthropic now works with the user's existing `temperature: 0.7` config — no config edit required, no silent failure. The user's input (the config) is honored where it is valid and *correctly ignored* (not forwarded into a 400) where the model forbids it.
- **No surprising silence:** dropping a deprecated param is the documented, vendor-recommended behavior (steer via prompting), and matches what Vertex already did invisibly. We are removing a hard failure, not hiding a meaningful user choice — a deprecated `temperature` has no effect on these models anyway.
- **Extensible by data:** a future model quirk is a one-line table row, not a new adapter branch — the maintainer drives capability data the same way across providers.
- The optional editor surfacing ("this model ignores temperature") is **explicitly out of scope** (a separate turbotui follow-up); this PR changes only wire behavior.

### (3) No regressions
- **Empty-resolution byte-identity:** any model without an override row (every non-Claude model today) hits `mc == ModelCaps{}`, so `buildRequest`'s sampling gate and the adapter output are byte-identical to current `main`. The `reasoning && caps.ReasoningRejectsTemperature` path (OpenAI o-series) is preserved verbatim.
- **Vertex unchanged in production:** the provider-wildcard row keeps every Vertex Claude dropping sampling exactly as the deleted branch did. `TestVertexAnthropicEndToEndADCAndWire` (which goes through `Complete → buildRequest`) and `TestVertexAnthropicAdaptiveThinking` stay green.
- **One intended test edit, not a regression:** `TestVertexAnthropicBodyShape` calls `buildBody` **directly** with `Temperature` set and asserts the body omits it (lines 159-164). That assertion tests a decision that, by design, **no longer lives in the adapter**. Those two assertions move up to a `buildRequest`/`resolveModelCaps`-level test (the new regression test below); the rest of `TestVertexAnthropicBodyShape` (model omitted, `anthropic_version`, cache breakpoints) is untouched. This is the expected consequence of "the decision now lives in data," called out explicitly so it is not mistaken for breakage.
- `TestDirectAnthropicBodyEmitsPromptCacheBreakpointsIssue404` calls `buildBody` directly with a temperature and asserts it **is** present (line 329-331). Since the adapter still forwards whatever it is handed, and that test hands it a non-nil temperature, it **stays green** — good: it confirms the adapter itself is now purely a forwarder.
- Cache breakpoints (#404), strict tools (#359), reasoning encoding (#402), parallel-tool-calls invariant (#282) untouched.
- Does **not** regress #550's catalog review-form indicators (`transform.go` `CapabilityLabels`/`ReasoningCapable`): step 1 does not touch `transform.go` at all; step 3 (if done) only *adds* a consumer of the existing booleans.

### (4) Holistic — right place, seam between repos respected
- **gogent-only.** The capability layer lives entirely in the wire-format layer (`internal/model`). turbotui is a TUI client that sends model *configs*; it has no knowledge of per-model wire quirks and needs none. The read-only `$HOME/work/turbotui` clone confirms the seam: turbotui posts `ModelConfig` JSON and renders responses; sampling-param admissibility is a server-side wire concern. **No turbotui change.**
- **Right place in gogent:** the quirk lands next to the other wire capabilities (`internal/model`), read by the same `buildRequest` that already gates every other param, rather than in the adapter (too late, per-instance) or in `config` (a config boolean would re-fuse it to user intent, repeating the original mistake).
- **Downstream:** no API/route/transcript-shape change; sessions and transcripts are unaffected (sampling params were never persisted in a way that changes). The editor-surfacing follow-up is the only turbotui-facing effect and is deliberately deferred.

---

## 5. Files touched

| File | Change |
|---|---|
| `internal/model/caps.go` | **NEW.** `ModelCaps` struct, `resolveModelCaps(apiType, model)`, snapshot-id normalization helper. |
| `internal/model/model_overrides.go` | **NEW.** Curated `modelOverrides` table (authoritative tier): current-gen Claude `RejectsSampling` rows + the `vertex-anthropic` provider-wildcard row. |
| `internal/model/connection.go` | `buildRequest`: resolve `ModelCaps`, fold `RejectsSampling` into the sampling gate. Tiny `providerAPIType()` accessor. |
| `internal/model/adapter.go` | `anthropicAdapter.buildBody`: forward `Temperature`/`TopP` on both paths; **delete** the Vertex sampling-drop branch. |
| `internal/model/vertex_anthropic_test.go` | Relocate the two direct-`buildBody` sampling-omission assertions in `TestVertexAnthropicBodyShape` to the new buildRequest-level test (the adapter no longer owns that decision). |
| `internal/model/caps_test.go` (or add to `reasoning_test.go`) | **NEW.** `resolveModelCaps` tiering test + direct-Anthropic Opus regression test. |
| `internal/modelsdev/transform.go` | **Step 3 only (optional):** feed catalog `temperature`/`reasoning`/`limit` booleans into the default tier instead of discarding them. |

### Tests (DoD)
- **Regression (the fix):** build a connection for `{APIType:"anthropic", Model:"claude-opus-4-8", Temperature:0.7, TopP:0.9}` via the existing `buildRequestFor` helper; assert `req.Temperature == nil` **and** `req.TopP == nil`. Mirrors `vertex_anthropic_test.go`'s sampling assertion but at the resolved-request level. A sibling case asserts the Vertex path for the same model also yields no sampling params (direct == Vertex).
- **`resolveModelCaps` tiering:** override beats catalog; catalog used when no override (step 3); model-only row applies across both `anthropic` and `vertex-anthropic`; provider-wildcard applies to an arbitrary Vertex Claude; unknown model → empty `ModelCaps`.
- **No-regression guard:** a non-Claude model (`gpt-4o`) still emits temperature/top_p unchanged (already covered by `TestBuildRequestReasoningParams`; add an explicit "empty caps ⇒ unchanged" assertion if useful).

---

## 6. Optional follow-on steps (only if they fit the gate cleanly)

- **Step 2 — migrate remaining smuggled quirks to data, one at a time:** the `thinking{enabled}→{adaptive}` rewrite, the `max_completion_tokens` gating, and the "2.5 Pro cannot fully disable thinking" comment each become a `ModelCaps` field + row. Independent; each shippable alone.
- **Step 3 — thread models.dev booleans into the default tier:** `transform.go` stops discarding `temperature`/`reasoning`/`limit` and feeds them as tier-2 defaults under override. **Import-direction caveat below.**

Step 1 alone fixes Opus and proves the seam, and is the mandatory deliverable.

---

## 7. Regression risks (called out)

1. **Vertex behavior change for non-current-gen Claude.** Removing the blanket `if a.vertex` drop *could* re-enable sampling for older Vertex Claude. **Mitigated** by the `vertex-anthropic` provider-wildcard row, which reproduces the blanket drop exactly. Risk reduces to "is the wildcard row present and matched first" — covered by a tiering test.
2. **`TestVertexAnthropicBodyShape` direct-buildBody assertions** necessarily move (the adapter no longer decides). This is intended; flagged so a reviewer does not read it as breakage.
3. **Snapshot ids.** A dated/pinned model id (`claude-opus-4-8@2026…`) must still match its family row — handled by base-id normalization before lookup; tested.
4. **#542/#550 overlap in `transform.go`.** Step 1 does not touch `transform.go`, so no conflict. If step 3 is attempted, rebase onto current `origin/main` first and build on (do not duplicate) the existing `CapabilityLabels`/`ReasoningCapable` helpers.

---

## 8. Open questions

1. **Which Claude families get explicit `RejectsSampling` rows on the *direct* path?** The provider-wildcard covers Vertex. For direct Anthropic, the authoritative truth is "current-gen Claude rejects temperature." Proposal: seed confirmed-deprecated families (`opus-4-x`, `sonnet-4-x`, `haiku-4-x`, `3-7-*`) as model-only rows; older direct Claude (`3-5-*` and earlier) keep sending temperature (they accept it). Is there a maintained list of exactly which generations deprecate it, or do we treat "4-x and 3-7" as the line? **Recommendation:** model-only rows for 4-x + 3-7; revisit as new models land. (This is the only judgment call; everything else is mechanical.)
2. **Match semantics — exact vs prefix.** Exact-id rows (+ snapshot normalization) are the least surprising and what step 1 uses. A prefix/family match (`claude-opus-4-*`) would auto-cover future snapshots but risks over-matching. **Recommendation:** exact + snapshot-normalization now; defer family-prefix matching unless the row list grows unwieldy.
3. **Step 3 import direction.** `internal/modelsdev` already imports `internal/model` (in tests today). Having `internal/model` import `internal/modelsdev` at runtime for the catalog default tier risks a cycle. **Recommendation if step 3 is pursued:** do *not* import the catalog into `model`; instead bake the resolved catalog booleans into `ModelConfig` at config-creation time in `transform.go` (which already produces `ModelConfig`) and have `resolveModelCaps` read them from the config the connection already holds — keeping the dependency arrow `modelsdev → model` intact. This keeps step 3 truly additive and cycle-free, but is out of the step-1 minimum.
