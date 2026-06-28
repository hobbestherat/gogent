# Design — Surface ALL models.dev-defined aspects in the Catalog review form

Issue gogent #542 (supersedes / also closes #541). DRIVER: pair1, branch
`pair1/catalog-review-form-surface-all-modelsde`.

## Summary

The "Add from Catalog…" wizard's review step (`showCatalogReviewStep` in
`ui/tui/model_catalog_dialog.go`) fetches a rich per-provider/model fact set from
models.dev but surfaces almost none of it: the only catalog-derived indicator is
the read-only `API type: <type> (from catalog)` label. Everything else is either
silently dropped (context window, cost, capabilities, env var, docs, reasoning
options, thinking-toggle relevance) or used to seed an editable field without
telling the user it came from the catalog (endpoint, max tokens, reasoning
effort, temperature).

This change makes the review form **communicate provenance and capability**
consistently, for every aspect A–J in the issue checklist. It is **display /
clarity only**: routing, persistence and validation semantics are untouched. No
new deps, no `go.mod` bump, no turbotui change — the existing read-only-label /
`Select` / `TextBox` primitives are sufficient.

The work is two small read-only helpers (one in `internal/model`, capability/cost
formatting in `internal/modelsdev`) plus a re-layout of `showCatalogReviewStep`.
`ToModelConfig` and all routing code are **unchanged**, so the draft persisted
for a derive-base provider still has an empty `Endpoint`.

## Grounding (what the code does today)

- `ToModelConfig` (`internal/modelsdev/transform.go:64`) already fills `Name`,
  `DisplayName`, `APIType`, `Model`, `Temperature=0.7`, `MaxTokens=Limit.Output`,
  `ContextWindow=Limit.Context`, `Free=(Cost==0)`, and `ReasoningEffort` +
  `EffortOptions` from `reasoning_options[type=effort]`. For
  `deriveBaseAPITypes` (`anthropic, zai, openrouter, vertex, vertex-native,
  vertex-anthropic`) it leaves `Endpoint=""`; otherwise it copies `p.API`. This
  stays as-is.
- The resolved default base URLs live as literals on each provider's
  `endpoints` resolver in `internal/model`:
  - `staticBaseEndpoints.defaultBaseURL`: anthropic `https://api.anthropic.com`
    (`provider_anthropic.go:16`), zai `https://api.z.ai/api/paas/v4`, openrouter
    `https://openrouter.ai/api/v1` (`provider_openai.go:38,48`).
  - vertex / vertex-native / vertex-anthropic build the base from
    project+location via `baseURLFunc` (`vertexOpenAIBaseURL` /
    `vertexNativeBaseURL`) and have **no** static default — i.e. the base is not
    knowable until the user enters Project + Location.
- `config.ModelConfig` (`internal/config/config.go:18`) carries
  `ContextWindow`, `MaxTokens`, `Free`, `ReasoningEffort`, `EffortOptions`,
  `Thinking *bool`. It has **no** fields for tool-call/vision/temperature
  capability — these are display-only and will not be persisted.
- `modelsdev.HasThinkingToggle(m)` exists (`transform.go:100`) but
  `showCatalogReviewStep` never calls it.
- `ui/tui` already imports `internal/model` (`model_editor.go`) and
  `internal/modelsdev`, so no new package edges from the dialog. `internal/model`
  does **not** import `internal/modelsdev` (the registry comment only references
  it), so adding a `modelsdev`→`model` edge would be cycle-free — but we avoid it
  by calling `model.ResolvedBaseURL` from the dialog directly.
- Tests inspect the **rendered grid text** (`modelsDialogHasText`,
  `models_dialog_test.go:35`), so every indicator must render as visible text.

## Helpers to add (read-only, behaviour-preserving)

### 1. `internal/model`: resolve a derive-base provider's base URL + thinking capability

```go
// ResolvedBaseURL reports the request base URL a provider uses when the config
// leaves Endpoint empty. base is the static default for static-base derive-base
// providers (anthropic/zai/openrouter); fromProjectLocation is true for the
// vertex* family, whose base is built from Project+Location and so is unknown
// until entered. For NON-derive-base providers (generic "openai" and unknown
// gateways) it returns ("", false) — the caller uses p.API instead.
// Reuses the registry's defaultBaseURL literals — no duplication.
func ResolvedBaseURL(apiType APIType) (base string, fromProjectLocation bool)

// SupportsThinking reports whether the provider actually emits the `thinking`
// request parameter (caps.SupportsThinking). Lets the review form annotate the
// Thinking selector truthfully instead of trusting the catalog toggle alone.
func SupportsThinking(apiType APIType) bool
```

`ResolvedBaseURL` implementation — **the `derivesBase` guard comes first**, before
any type-switch:

```go
p := providerFor(apiType)
if !p.derivesBase {            // generic "openai" + unknown gateways: caller uses p.API
    return "", false           // NB: generic openai is staticBaseEndpoints with a
}                              // localhost defaultBaseURL — the guard prevents leaking it
switch e := p.endpoints.(type) {
case staticBaseEndpoints:
    if e.defaultBaseURL != "" { // anthropic/zai/openrouter
        return e.defaultBaseURL, false
    }
    return "", true             // vertex OpenAI shim (baseURLFunc only)
case modelURLEndpoints:         // vertex native / vertex-anthropic
    return "", true
}
return "", false
```

The guard is essential: the generic `openai` provider is itself a
`staticBaseEndpoints{defaultBaseURL:"http://localhost:8080/v1"}` with
`derivesBase:false` (`provider_openai.go:22`), so a bare type-switch would return
the localhost placeholder for `openai` — breaking the gateway indicator (it would
read `derived: http://localhost:8080/v1`) and failing the `openai → ("",false)`
unit test. Only the six `deriveBaseAPITypes` reach the switch, where
anthropic/zai/openrouter have a non-empty `defaultBaseURL` and the vertex* trio
are `baseURLFunc`/`modelURLEndpoints`. This stays the single source of truth used
by routing, so the indicator can never drift from the real base.

`SupportsThinking` reads `providerFor(apiType).caps.SupportsThinking` — true for
`zai`/`vertex-anthropic`, false for `anthropic`/`openrouter`/`openai`/vertex
(gemini). This is what makes aspect G's annotation honest (see G below).

### 2. `internal/modelsdev`: capability / cost formatting (pure)

Small pure helpers over the already-decoded `Model` (keeps `ui/tui` terse and
gives unit-test seams):

```go
func ReasoningCapable(m Model) bool   // m.Reasoning || len(effortOptions(m))>0 || HasThinkingToggle(m)
func CapabilityLabels(m Model) []string // {"reasoning","tool calling","vision","custom temperature"} as present
func CostSummary(m Model) string        // "Free" | "$3 in / $15 out per M"
```

`ReasoningCapable` honors `Model.Reasoning` directly (issue note 2: a
`reasoning:true` model with only a toggle/no effort option is still flagged).
`CapabilityLabels` maps `Reasoning`, `ToolCall`, `Attachment` (vision),
`Temperature` (accepts custom temperature). These are display-only — no
`ModelConfig` fields added.

## Review-form re-layout (`showCatalogReviewStep`)

Provenance pattern, matching the existing `(from catalog)` treatment:

- **Read-only catalog facts** → static `dialogLabel`s with `(from catalog)`.
- **Catalog-seeded editable fields** → keep the editable control, show the source
  as a short read-only hint to the right (box narrowed to make room) and, where a
  closed option set exists, constrain to a `Select`.
- **Required credential** → carry the provider `Env` hint into the form.

Per-aspect treatment:

| # | Aspect | Treatment |
|---|--------|-----------|
| A | Endpoint (derive-base) | **Keep the editable box, leave it empty** (so the persisted endpoint stays empty — invariant preserved); narrow it and render a read-only hint from `model.ResolvedBaseURL`: static base → `(derived: https://api.anthropic.com)`; vertex* (`fromProjectLocation==true`) → `(derived from Project + Location)`. The branch point is `ResolvedBaseURL` itself: it returns `("",false)` for non-derive-base providers (the `derivesBase` guard, see helper 1), so OpenAI-compatible gateways take the **unchanged** path — full-width editable box prefilled with `p.API`, no hint. |
| B | Context window | New read-only row `Context: 200K  (from catalog)` (reuses `tokensShort`). |
| C | Output cap / Max tokens | Keep editable box (seeded with `Limit.Output`); right-side hint `(from catalog output limit)`. |
| D | Free / pricing | New read-only row `Cost: Free (from catalog)` or `Cost: $3 in / $15 out per M (from catalog)` (`CostSummary`). |
| E | Reasoning-effort options | Replace the bare `Reasoning:` TextBox with a `Select` whose options are `["(none)"] + draft.EffortOptions`, with the catalog default (`draft.ReasoningEffort`, = `EffortOptions[0]`) preselected. `(none)` maps to `ReasoningEffort=""` so the user can still **opt out** of reasoning (preserving the old TextBox-can-be-emptied behavior, which flips `IsReasoningModel()` false — config.go:76). When `EffortOptions` is empty, fall back to the current editable TextBox (no regression for providers without an effort set). Hint `(from catalog)`. |
| F | Reasoning-capable | Surfaced via `ReasoningCapable` in the capabilities row. |
| G | Thinking-toggle relevance | Keep the `Thinking` `Select` present, **normal-coloured and editable** (no lockout), and annotate it with a plain-text hint. Relevance = `model.SupportsThinking(apiType) && HasThinkingToggle(m)`: `(supported)` when both true, else `(no effect for this model)`. ANDing with the provider cap is what makes the annotation truthful — the direct `anthropic` provider has `caps:{}` (`SupportsThinking` false), so a Claude model with a catalog toggle is correctly shown as no-op rather than a misleading `(supported)` (gogent drops the param at connection.go:1303). Do **not** grey the value: `dropdownDisabledFG` is the codebase's "this control is inert" signal (its only use, `guardEffortSelect`, swallows clicks/keys — session_window.go:852), so greying an editable Select would invent a contradictory "greyed but works" semantic. Value defaults to `default` → `Thinking` stays `nil` unless the user changes it. |
| H | Provider env var | Carry `p.Env` into the form as a read-only hint next to the API-key field: `(env: ANTHROPIC_API_KEY)`. For vertex (ADC, no key) show the existing project/location path; the env hint is shown only when an API key is required. |
| I | Capabilities (tool/vision/temperature) | New read-only row `Capabilities: reasoning · tool calling · vision · custom temperature` (`CapabilityLabels`). Display-only; no persisted fields. Covers latent issue #1 (temperature capability now visible) without changing the hardcoded 0.7. |
| J | Docs URL | New read-only row `Docs: <p.Doc>` when `p.Doc != ""`. |

### Layout / height — committed plan (not deferred)

`ResolveDialogRect` applies the `Min` floor **last** (after the 85%-of-screen
cap), so `MinH:21` yields 21 rows on both an 80×24 terminal and the 80×25 test
default — the prior "~20 rows" worry was a miscalculation. **The plan is
`MinH/MaxH:21, PrefH:21`** (up from 18). Row budget, rendering **only the rows
that apply to the selected model**:

Always: Name, API type, Display name, Endpoint, Model id, Temperature, Max
tokens, Reasoning, Thinking = 9. Credential: API key **or** Project+Location,
never both — API key (1 row) for key providers, or Project+Location (2 rows) for
vertex. Read-only catalog block: **Context+Cost merged onto one line** (1),
Capabilities (1), Docs (1, only when `p.Doc != ""`). That is **13 content rows**
for the anthropic key case (9 + 1 key + 3 block; Project/Location omitted as
non-vertex), ~14 for vertex. With the button row + 2 borders that is ≈ 16–17 <
21 — fits with headroom. (Project/Location were always-shown before; rendering
them vertex-only is a display-only clarity gain — non-vertex configs leave both
empty regardless, so persistence is unchanged.)

**Width** is the real squeeze, not height. On an 80-wide terminal `PreferredW`
clamps to `MinW`, so **`MinW` is raised 70→76** (≈ the usable width at margin 2)
to widen the field area to ~55 cells (`width − boxX(18) − 3`). The
`(derived: https://api.anthropic.com)` hint is ~35 chars; the override box for
derive-base is deliberately **narrow** (≈8 cols — enough to begin typing an
override, since it is empty by default and rarely used), leaving ~46 cells for the
hint. The env-var hint joins multiple `p.Env` with `, `; if it would overflow the
field area it truncates gracefully (single-var providers — the common case — never
truncate). These row/width budgets are re-checked against the test workbench and
an explicit 80×24 resize in the layout test.

## The 4 design gates

### (1) Goal match
Every aspect A–J is indicated/surfaced per the checklist: derive-base endpoint
shows a non-empty derived indicator (and stays overridable + empty-persisting),
gated by `ResolvedBaseURL`'s `derivesBase` guard so gateways keep the `p.API`
path; effort is a constrained `Select` that still allows opt-out via `(none)`;
thinking relevance is honored truthfully (`HasThinkingToggle` AND provider
`SupportsThinking`); the env var is carried forward; capabilities, context, cost,
reasoning-capable and docs are surfaced. Scope is strictly display/clarity — no
routing/persistence/validation change, no scope creep (temperature handling and
reasoning-detection routing are left as-is; only their *display* is added).

### (2) Usability
The empty Endpoint box is no longer confusing — the derived base (or the
"derived from Project + Location" reason) is shown read-only beside it, while the
box stays editable for proxy/gateway override. Provenance is consistent
(`(from catalog)` everywhere, mirroring the existing API-type label). Catalog
info *informs* but never locks the user out: every seeded editable field stays
editable; the effort/thinking Selects default to the catalog/provider value. The
OpenAI-compatible gateway path (Groq/Together/DeepSeek/…) is visually and
behaviourally unchanged.

Two convention risks are resolved (not left open):
- **Thinking selector stays normal-coloured and editable, annotated by plain
  text** (`(supported)` / `(no effect for this model)`). It does **not** reuse
  `dropdownDisabledFG`, whose sole established meaning is "this control is inert"
  (`guardEffortSelect`, session_window.go:852 swallows clicks/keys). Greying an
  editable Select would invent a contradictory "greyed but works" semantic, so
  relevance is signalled by the hint alone — no lockout, no convention collision.
- **Effort `Select` keeps the opt-out** via a leading `(none)` option mapping to
  `ReasoningEffort=""`. The constrained valid set does not silently make every
  catalog model permanently reasoning (the old free-text box could be emptied);
  `(none)` preserves that escape and keeps the form honest about the closed set.

### (3) No regressions
- `ToModelConfig`, `deriveBaseAPITypes`, `derivesBase`, `validateRoutableConfig`
  are **not touched** → existing `transform_test`, provider, config and server
  tests stay green; persisted derive-base configs still have empty `Endpoint`
  unless the user types an override.
- `ResolvedBaseURL`/`SupportsThinking` are new read-only functions over the
  existing registry; they change no routing. `ResolvedBaseURL`'s `derivesBase`
  guard makes `openai → ("",false)` (its own unit test passes, and the gateway
  indicator path is correct) — without the guard it would leak the localhost
  placeholder, which is why the guard is mandatory, not optional.
- New `modelsdev` helpers are pure additions.
- The Endpoint box for gateways keeps prefilling `p.API` (asserted today by
  `TestToModelConfigOpenAIGatewayEndpointResolvesCorrectly`, unaffected since the
  transform is unchanged).
- Effort row degrades to the current TextBox when `EffortOptions` is empty, so
  models without an effort set behave as before.
- Layout width (not height) is the one real squeeze → the committed row/width
  budget (MinH:21, MinW:76, applicable-only rows) fits 80×24 and the 80×25 test
  default; verified by a layout test (see Layout/height). The review step is
  currently untested (no existing test asserts `MinH:18` or that `Reasoning:` is a
  TextBox), so the height bump and effort→`Select` swap break nothing today — this
  change adds the step's first tests. `gofmt/vet/build/golangci-lint` clean;
  `go test ./...` green (pre-existing `TestUserSessionSendMessage` 404 the only
  accepted failure).

### (4) Holistic (gogent ↔ turbotui)
gogent-only. The read-only base-URL literals are reused from `internal/model`'s
provider registry (not duplicated) via `ResolvedBaseURL`; capability/cost
formatting lives in `internal/modelsdev` next to the data it reads; the dialog
orchestrates both (it already imports both packages). The seam to turbotui is
respected: read-only facts render with the existing `dialogLabel` and closed
option sets use the existing `Select` (which exposes `SetOptions`/`SetSelected`/
`GetSelected`/`Value` — sufficient as-is) — **no turbotui change and no `go.mod`
bump**. We deliberately do **not** reuse the `dropdownDisabledFG` theme slot for
the (still-editable) Thinking selector: that slot means "inert control" in gogent
(`guardEffortSelect`), so relevance is signalled by a plain-text hint instead,
keeping the one existing convention intact. (The only conceivable turbotui ask
would be a first-class read-only/disabled `TextBox`; we avoid it by using a label
+ an empty editable box, which also happens to be exactly what preserves the
empty-endpoint persistence invariant.)

## Files touched

- `ui/tui/model_catalog_dialog.go` — re-layout `showCatalogReviewStep`
  (endpoint hint, context/cost/capabilities/docs rows, effort `Select`, thinking
  annotation, env-var hint, max-tokens hint); bump the review `DialogSpec`
  height.
- `internal/model/provider.go` (or a small new `internal/model/base_url.go`) —
  add `ResolvedBaseURL` (with the `derivesBase` guard) and `SupportsThinking`.
- `internal/modelsdev/transform.go` — add `ReasoningCapable`, `CapabilityLabels`,
  `CostSummary`.
- Tests: extend `ui/tui` catalog review-form tests (derived endpoint for
  anthropic + vertex, gateway prefill unchanged, context/cost/max-tokens
  provenance, effort `Select` constrained to `EffortOptions` + `(none)` opt-out,
  reasoning-capable, thinking relevance, env-var hint, capability indicators
  present/absent); unit-test `model.ResolvedBaseURL`/`model.SupportsThinking` per
  api_type and the new `modelsdev` helpers.
  `ui/tui` tests stay free of `internal/daemon`/`internal/server` imports.

## Test plan (gogent)

1. `internal/model`: `ResolvedBaseURL("anthropic")` → `("https://api.anthropic.com", false)`; zai/openrouter → their defaults; `vertex`/`vertex-native`/`vertex-anthropic` → `("", true)`; **`openai` → `("", false)`** (the `derivesBase` guard prevents leaking the `http://localhost:8080/v1` placeholder — this is the regression test for Defect 1). `SupportsThinking`: true for `zai`/`vertex-anthropic`, false for `anthropic`/`openrouter`/`openai`/vertex.
2. `internal/modelsdev`: `ReasoningCapable`/`CapabilityLabels`/`CostSummary` over a seeded `Model` (reasoning+tools+vision+temp+free, and a bare model) — present where the flag is set, absent otherwise; `ReasoningCapable` true for `reasoning:true` with only a toggle.
3. `ui/tui` review form: drive the wizard to the review step for
   - an **anthropic** catalog model → grid contains `derived: https://api.anthropic.com`, an **empty** editable Endpoint box (not a prefilled one), `Context: …`, `Cost: …`, `Capabilities: …`, `(env: ANTHROPIC_API_KEY)`, effort `Select` options = `["(none)"] + EffortOptions`, thinking annotation `(no effect for this model)` (direct-Anthropic: `SupportsThinking` false even with a catalog toggle);
   - a **zai** model with a catalog toggle → thinking annotation `(supported)` (both caps and toggle true);
   - a **vertex** model → `(derived from Project + Location)`;
   - an **OpenAI-compatible gateway** (e.g. Groq) → Endpoint box prefilled with `p.API`, **no** derived hint (guard exercised end-to-end);
   - a **bare** model (no reasoning/cost/effort) → effort row degrades to TextBox; capability indicators absent or blank.
4. Regression: existing catalog/model/config/server tests unchanged; no
   `routable_config_validation` behavior change.

## Open questions

(The Defect-1 guard, the Thinking grey-vs-annotate choice, the effort opt-out, the
truthful thinking annotation, and the row/width budget are all now decided in the
sections above — they are no longer open.)

1. **Docs row under width pressure.** Aspect J is marked OPTIONAL in the issue.
   The plan renders `Docs: <url>` only when `p.Doc != ""`; if even that one line
   is unwanted, it can degrade to omission. Confirm J may be dropped silently when
   `p.Doc` is empty (the only case the plan drops it) — no objection expected.
2. **Multi-env-var truncation.** When `p.Env` lists more than one variable the
   right-side hint joins them with `, ` and may truncate at the field edge on a
   narrow terminal. Single-var providers (the common case) never truncate. Accept
   truncation, or fall back to a dedicated full-width line when `len(p.Env) > 1`?
   (Lean: accept truncation — multi-var providers are rare and the picker step
   already showed the full list.)
