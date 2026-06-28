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

### 1. `internal/model`: resolve a derive-base provider's base URL

```go
// ResolvedBaseURL reports the request base URL a provider uses when the config
// leaves Endpoint empty. base is the static default for static-base providers
// (anthropic/zai/openrouter); fromProjectLocation is true for the vertex* family,
// whose base is built from Project+Location and so is unknown until entered.
// Reuses the registry's defaultBaseURL literals — no duplication.
func ResolvedBaseURL(apiType APIType) (base string, fromProjectLocation bool)
```

Implementation: `providerFor(apiType)` then type-switch on `p.endpoints`:
`staticBaseEndpoints` with non-empty `defaultBaseURL` → `(defaultBaseURL, false)`;
`staticBaseEndpoints` with only a `baseURLFunc` (vertex OpenAI shim) or
`modelURLEndpoints` (vertex native/anthropic) → `("", true)`. This is the single
source of truth already used for routing, so the indicator can never drift from
the real base. Only called for `deriveBaseAPITypes`; OpenAI-compatible gateways
keep using `p.API` and never reach this.

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
| A | Endpoint (derive-base) | **Keep the editable box, leave it empty** (so the persisted endpoint stays empty — invariant preserved); narrow it and render a read-only hint from `model.ResolvedBaseURL`: static base → `(derived: https://api.anthropic.com)`; vertex* → `(derived from Project + Location)`. OpenAI-compatible gateways are **unchanged**: full-width editable box prefilled with `p.API`. |
| B | Context window | New read-only row `Context: 200K  (from catalog)` (reuses `tokensShort`). |
| C | Output cap / Max tokens | Keep editable box (seeded with `Limit.Output`); right-side hint `(from catalog output limit)`. |
| D | Free / pricing | New read-only row `Cost: Free (from catalog)` or `Cost: $3 in / $15 out per M (from catalog)` (`CostSummary`). |
| E | Reasoning-effort options | Replace the bare `Reasoning:` TextBox with a `Select` whose options are `draft.EffortOptions` (default `draft.ReasoningEffort` preselected). When `EffortOptions` is empty, fall back to the current editable TextBox (no regression for providers without an effort set). Hint `(from catalog)`. |
| F | Reasoning-capable | Surfaced via `ReasoningCapable` in the capabilities row. |
| G | Thinking-toggle relevance | Keep the `Thinking` `Select` always present and editable (no lockout), but annotate it from `HasThinkingToggle(m)`: `(supported)` vs `(no effect for this model)`, greying the value with `dropdownDisabledFG` when not relevant. Value still defaults to `default` → `Thinking` stays `nil` unless the user changes it. |
| H | Provider env var | Carry `p.Env` into the form as a read-only hint next to the API-key field: `(env: ANTHROPIC_API_KEY)`. For vertex (ADC, no key) show the existing project/location path; the env hint is shown only when an API key is required. |
| I | Capabilities (tool/vision/temperature) | New read-only row `Capabilities: reasoning · tool calling · vision · custom temperature` (`CapabilityLabels`). Display-only; no persisted fields. Covers latent issue #1 (temperature capability now visible) without changing the hardcoded 0.7. |
| J | Docs URL | New read-only row `Docs: <p.Doc>` when `p.Doc != ""`. |

### Layout / height

Today the form uses rows 1–12 with `MaxH:18`. The additions are **3 new
read-only rows** (Context+Cost can share one line; Capabilities; Docs) plus
right-side hints that reuse existing rows (Endpoint, Max tokens, API key) by
narrowing those boxes, and the Reasoning row becomes a `Select`. Net: ~3 extra
rows. The spec grows to roughly `MinH/MaxH:21, PrefH:21`; `dialogRect`
(`ResolveDialogRect`) already centers and clamps to the terminal. **Risk: on an
80×24 terminal the dialog caps near ~20 rows.** Mitigations, in order of
preference: (1) merge Context+Cost onto one line and drop the Docs row to a hint;
(2) only render rows that apply to the selected model (e.g. omit Docs when
`p.Doc==""`, omit the effort `Select` row when there are no options) so the form
is as short as the model's facts require. Final row math is validated against the
test workbench size and an 80×24 resize during implementation.

## The 4 design gates

### (1) Goal match
Every aspect A–J is indicated/surfaced per the checklist: derive-base endpoint
shows a non-empty derived indicator (and stays overridable + empty-persisting);
effort is a constrained `Select`; `HasThinkingToggle` is honored; the env var is
carried forward; capabilities, context, cost, reasoning-capable and docs are
surfaced. Scope is strictly display/clarity — no routing/persistence/validation
change, no scope creep (temperature handling and reasoning-detection routing are
left as-is; only their *display* is added).

### (2) Usability
The empty Endpoint box is no longer confusing — the derived base (or the
"derived from Project + Location" reason) is shown read-only beside it, while the
box stays editable for proxy/gateway override. Provenance is consistent
(`(from catalog)` everywhere, mirroring the existing API-type label). Catalog
info *informs* but never locks the user out: every seeded editable field stays
editable; the effort/thinking Selects default to the catalog/provider value. The
OpenAI-compatible gateway path (Groq/Together/DeepSeek/…) is visually and
behaviourally unchanged.

### (3) No regressions
- `ToModelConfig`, `deriveBaseAPITypes`, `derivesBase`, `validateRoutableConfig`
  are **not touched** → existing `transform_test`, provider, config and server
  tests stay green; persisted derive-base configs still have empty `Endpoint`
  unless the user types an override.
- `ResolvedBaseURL` is a new read-only function over the existing registry; it
  changes no routing.
- New `modelsdev` helpers are pure additions.
- The Endpoint box for gateways keeps prefilling `p.API` (asserted today by
  `TestToModelConfigOpenAIGatewayEndpointResolvesCorrectly`, unaffected since the
  transform is unchanged).
- Effort row degrades to the current TextBox when `EffortOptions` is empty, so
  models without an effort set behave as before.
- Layout growth is the one real risk → validated against the test terminal and
  80×24 (see Layout/height). `gofmt/vet/build/golangci-lint` clean; `go test
  ./...` green (pre-existing `TestUserSessionSendMessage` 404 the only accepted
  failure).

### (4) Holistic (gogent ↔ turbotui)
gogent-only. The read-only base-URL literals are reused from `internal/model`'s
provider registry (not duplicated) via `ResolvedBaseURL`; capability/cost
formatting lives in `internal/modelsdev` next to the data it reads; the dialog
orchestrates both (it already imports both packages). The seam to turbotui is
respected: read-only facts render with the existing `dialogLabel`, closed option
sets use the existing `Select`, and the "disabled/read-only value" look reuses
the existing `dropdownDisabledFG` theme slot — **no turbotui change and no
`go.mod` bump**. (The only conceivable turbotui ask would be a first-class
read-only/disabled `TextBox`; we deliberately avoid it by using a label + an
empty editable box, which also happens to be exactly what preserves the
empty-endpoint persistence invariant.)

## Files touched

- `ui/tui/model_catalog_dialog.go` — re-layout `showCatalogReviewStep`
  (endpoint hint, context/cost/capabilities/docs rows, effort `Select`, thinking
  annotation, env-var hint, max-tokens hint); bump the review `DialogSpec`
  height.
- `internal/model/provider.go` (or a small new `internal/model/base_url.go`) —
  add `ResolvedBaseURL`.
- `internal/modelsdev/transform.go` — add `ReasoningCapable`, `CapabilityLabels`,
  `CostSummary`.
- Tests: extend `ui/tui` catalog review-form tests (derived endpoint for
  anthropic + vertex, gateway prefill unchanged, context/cost/max-tokens
  provenance, effort `Select` constrained to `EffortOptions`, reasoning-capable,
  thinking relevance, env-var hint, capability indicators present/absent);
  unit-test `model.ResolvedBaseURL` per api_type and the new `modelsdev` helpers.
  `ui/tui` tests stay free of `internal/daemon`/`internal/server` imports.

## Test plan (gogent)

1. `internal/model`: `ResolvedBaseURL("anthropic")` → `("https://api.anthropic.com", false)`; zai/openrouter → their defaults; `vertex`/`vertex-native`/`vertex-anthropic` → `("", true)`; `openai` → `("", false)` (caller uses `p.API`).
2. `internal/modelsdev`: `ReasoningCapable`/`CapabilityLabels`/`CostSummary` over a seeded `Model` (reasoning+tools+vision+temp+free, and a bare model) — present where the flag is set, absent otherwise; `ReasoningCapable` true for `reasoning:true` with only a toggle.
3. `ui/tui` review form: drive the wizard to the review step for
   - an **anthropic** catalog model → grid contains `derived: https://api.anthropic.com`, an **empty** editable Endpoint box (not a prefilled one), `Context: …`, `Cost: …`, `Capabilities: …`, `(env: ANTHROPIC_API_KEY)`, effort `Select` options = `EffortOptions`, thinking annotation;
   - a **vertex** model → `(derived from Project + Location)`;
   - an **OpenAI-compatible gateway** (e.g. Groq) → Endpoint box prefilled with `p.API`, no derived hint;
   - a **bare** model (no reasoning/cost/effort) → capability/effort indicators absent or blank.
4. Regression: existing catalog/model/config/server tests unchanged; no
   `routable_config_validation` behavior change.

## Open questions

1. **Height on small terminals.** Preferred fallback if 80×24 can't fit all rows
   is "render only rows that apply" + merge Context+Cost. Acceptable to drop the
   optional Docs row (aspect J is marked OPTIONAL) to a single shared hint line if
   space is tight — confirm J may degrade to optional under space pressure.
2. **Thinking Select when not relevant.** Plan keeps it editable + annotated +
   greyed (no lockout). Alternative is to fully disable it. Annotate-only is the
   lower-regression choice and matches "or annotate it"; flag if a hard disable is
   preferred.
3. **Env-var hint placement.** Plan puts `(env: ANTHROPIC_API_KEY)` to the right
   of the API-key box (narrowing it). If a provider lists multiple env vars
   (`p.Env` is a slice) the hint joins them with `, ` and may be truncated on a
   narrow dialog — acceptable, or move to a dedicated line.
