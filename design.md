# Design — gogent issue #505: misconfigured model entry must fail fast with a clear, model-named error

## Issue recap

A `ModelConfig` that cannot be routed — `api_type` AND `endpoint` both empty (or `model`
empty for a hosted gateway) — is silently sent to the generic OpenAI provider's placeholder
default base URL `http://localhost:8080/v1/chat/completions`. The first agent turn then fails
with an opaque `model round-trip: complete with tools: generic: unexpected error: status 404`
(a gogent daemon often answers on :8080; connection-refused otherwise).

Goal: such an entry fails fast on first use with a clear, actionable, **model-named** error,
e.g.

```
model "openrouter-glm-5.2" is misconfigured: api_type and endpoint are both empty (cannot determine where to send requests)
```

## Root cause (confirmed in code)

1. `StringToAPIType("")` → `APITypeOpenAI` (`internal/model/provider.go:91`).
2. OpenAI provider default base URL is the localhost placeholder
   (`internal/model/provider_openai.go:22` → `staticBaseEndpoints{defaultBaseURL:"http://localhost:8080/v1", chatPath:"/chat/completions"}`).
3. Empty `Endpoint` → that default is used verbatim
   (`staticBaseEndpoints.endpoints` `provider.go:275` → `normalizeBaseURL("", default, …)`).
4. No routability validation at build time: `NewModelConnectionFromConfig`
   (`connection.go:736`) hands raw config straight to the provider; only Vertex has a
   deferred `validate`.
5. The POST 404s; `analyzeError` (`connection.go:1637`) has no 404 case → catch-all
   `ErrorGeneric "unexpected error: status 404"`; 404 is non-retryable
   (`isRetryableStatus` `connection.go:1706`) so it fails fast but opaquely.

## Chosen approach

Add **deferred config validation** in `NewModelConnectionFromConfig`, reusing the exact
mechanism Vertex already uses: set `conn.configErr` so the error surfaces cleanly on the
first completion/scan call rather than as an opaque HTTP failure. This matches the Vertex
`vertexValidate` precedent (`provider_vertex.go:63`, wired at `connection.go:758`).

### Files / functions touched (gogent only)

- **`internal/model/provider.go`**
  - Add a `derivesBase bool` field to the `provider` struct (`provider.go:181`). This makes
    the provider registry the **single source of truth within `model`** for "this api_type
    synthesizes its own base URL", instead of a separate hardcoded list (resolves the critic's
    drift hazard — a future provider author sets the flag at its own registration).
  - Add `func configModelName(cfg *config.ModelConfig) string` → `cfg.Name`, else
    `cfg.DisplayName`, else `"<unnamed>"`. Names the model in the error.
- **`internal/model/provider_openai.go`** — set `derivesBase: true` on the `zai` (`:27`) and
  `openrouter` (`:43`) registrations; leave the generic `openai` registration (`:9`) false
  (its localhost default is a placeholder, not a synthesized real base).
- **`internal/model/provider_anthropic.go`** — set `derivesBase: true` on `anthropic` (`:30`).
- **`internal/model/provider_vertex.go`** — set `derivesBase: true` on `vertex` and
  `vertex-native` (`:44`,`:56`); and on `vertex-anthropic` (registered in
  `provider_anthropic.go`). Result: exactly the 6 deriving types
  `{zai, openrouter, anthropic, vertex, vertex-native, vertex-anthropic}` carry the flag —
  the same set as `modelsdev.deriveBaseAPITypes` (`internal/modelsdev/transform.go:18`).
- **`internal/model/connection.go`**
  - In `NewModelConnectionFromConfig`, alongside the existing `p.validateConfig` block, run a
    new `validateRoutableConfig(cfg)` and set `conn.configErr` when it fails (standalone
    function, directly unit-testable).
  - Gate `doJSON` (`connection.go:235`) on `configErr`: `if c.configErr != nil { return nil, c.configErr }`
    at the top. This extends fail-fast coverage to the model-listing/Scan path
    (`ListModels` → `doJSON`), so `ScanModels` (`gogent.go:3420`) on a misconfigured entry
    also returns the clear message instead of dialing `localhost:8080/v1/models`. One line,
    no behavior change for valid configs (their `configErr` is nil).
- *(Optional polish)* `analyzeError` — add a dedicated `404` case (see Secondary).
- **`internal/gogent/gogent.go`** — **not touched.** Both production callers route through
  `NewModelConnectionFromConfig`: `buildConnection` (`gogent.go:1540`) and the Scan path
  (`gogent.go:3420`), so they inherit the check. Putting validation in `buildConnection`
  would miss the Scan caller and diverge from the Vertex precedent.
- **turbotui** — **not touched**, no `go.mod` bump (see Criterion 4).

### Validation rule (precise)

`validateRoutableConfig(cfg)`:

```
endpointEmpty := strings.TrimSpace(cfg.Endpoint) == ""
rawType       := strings.ToLower(strings.TrimSpace(cfg.APIType))
resolved      := StringToAPIType(cfg.APIType)
```

**1. Routability — reject when the request has nowhere to go.**
Reject when `endpointEmpty` AND the entry has no built-in base URL, where "has a built-in
base URL" means:
- `providerFor(resolved).derivesBase` is true (the 6 deriving types — they synthesize their
  base from the api_type / project+location), **OR**
- `rawType == "openai"` — the generic OpenAI provider's localhost default is a *documented,
  intentional* local-server default (`provider_openai.go:20-21` "Neutral local default";
  gogent's daemon and the `local-lan`/`localhost`/`qemu-host` workflow all target a local
  `:8080`). An operator who explicitly selects `api_type:"openai"` and omits the endpoint is
  on that supported local path, so this is **accepted**.

Therefore reject exactly when `endpointEmpty && !derivesBase(resolved) && rawType != "openai"`,
i.e. the api_type is **empty** or an **unrecognized token** (both silently fall back to the
localhost placeholder, which is never the user's intent here):
- `rawType == ""` (the issue's headline case):
  `model "<name>" is misconfigured: api_type and endpoint are both empty (cannot determine where to send requests)`
- `rawType` non-empty but unrecognized (e.g. a typo `"opnai"`):
  `model "<name>" is misconfigured: endpoint is empty and api_type "<rawType>" is unrecognized (set an explicit endpoint or use a known api_type)`
  — uses the **raw** `cfg.APIType` string (not the resolved type) so a typo is visible to the
  user rather than masked as "openai".

This is the maintainer-specified shape: *"reject entries that have neither an explicit
endpoint NOR a base-URL-deriving api_type"* — consulting the deriving set — while **not**
over-rejecting the one legitimate edge the critic flagged (explicit `openai` + empty
endpoint). Compared with the previous draft, explicit-`openai`-empty-endpoint is now
**accepted**, not rejected. (Rejecting a typo'd/unknown token is retained: it is unroutable
and was never a supported path; surfacing it is strictly better UX than a silent 404. The
maintainer can narrow to empty-`api_type`-only — see Open Q 1.)

**2. Hosted-gateway empty model (scoped narrowly).**
Reject when `cfg.Model` is empty AND `resolved ∈ {APITypeOpenRouter, APITypeZAI}` (known
hosted gateways where an empty model is almost certainly a mistake):
`model "<name>" is misconfigured: model is empty (api_type "<rawType>" requires a model name)`.
- **Not** a blanket empty-model reject: `api_type:"openai"` + explicit endpoint + empty model
  stays valid (some local servers ignore/auto-select the model). Vertex empty-model is left to
  its own path. Avoids over-rejection.

Routability (check 1) is evaluated first; the two are mutually exclusive in practice (gateways
derive their base, so they never hit check 1). `configErr` is a `*ModelError{Type:
ErrorGeneric, Message: …}`, mirroring `vertexValidate` (no `HTTPStatusCode` — it is a config
error, not an HTTP status).

### Reuse note (why a registry flag, not an imported set)

The canonical `deriveBaseAPITypes` lives in `internal/modelsdev`, which already **imports**
`internal/model` (`transform_test.go:8`), so `model → modelsdev` would create an import cycle
and break `go test ./internal/modelsdev`. Rather than mirror the set as a third hardcoded map,
the deriving knowledge is attached to each provider at registration (`derivesBase`), making
the `model` registry self-describing and drift-proof. `modelsdev` keeps its own transform-side
copy (unchanged, out of scope). The strictly-single-source refactor (export
`model.DerivesBaseURL`, have `modelsdev` consume it) is a follow-up (Open Q 3).

## Secondary (optional polish): dedicated 404 in `analyzeError`

Add a `case 404:` to the `analyzeError` switch (`connection.go:1644`) returning an
`ErrorGeneric` with a clearer message (e.g. `not found (status 404): the endpoint or model
path is wrong — check api_type/endpoint/model`). **Does not** alter retryability:
`isRetryableStatus` already omits 404 and stays unchanged. Low-risk; included only if trivial.

## User-facing behavior

- Before: silent localhost target → `... generic: unexpected error: status 404` on turn 1
  (and the same opaque failure when Scanning a misconfigured entry).
- After: an unroutable entry returns
  `model "<name>" is misconfigured: <missing field(s)> ...` on first use — covering **both**
  the completion path (`complete`/`completeStream`) and the model-listing/Scan path
  (`doJSON`/`ListModels`). The user sees exactly which entry and which field to fix. The error
  flows through the normal error path the TUI already renders.

## The four design gates

**(1) Goal match.** Exactly the issue's ask: a fix (deferred validation), not a feature or
refactor. Rule 1 now matches the maintainer's literal spec — empty `api_type` + empty endpoint
is rejected; explicit `api_type:"openai"` + empty endpoint (the documented local default) is
**not** rejected, eliminating the prior draft's scope creep. Deriving providers are consulted
via the registry, as instructed. No change beyond `internal/model`.

**(2) Usability.** Error names the model (`cfg.Name`/`DisplayName`) and the precise missing
field(s); it is actionable; the unrecognized-api_type variant echoes the user's raw string so
typos are visible. It surfaces on first completion like the Vertex precedent, and — by gating
`doJSON` — on the Scan/ListModels path too, so coverage is honest and broad rather than
completion-only. The user drives the fix via their config entry.

**(3) No regressions.**
- Valid configs unaffected: local `openai` with explicit endpoint (all seeds set `Endpoint`
  via `DefaultEndpoint()`, `config.go:1061`) build unchanged; explicit `openai` with *empty*
  endpoint still builds (documented local default); deriving providers
  (`zai/openrouter/anthropic/vertex/vertex-native/vertex-anthropic`) with empty endpoint still
  build (registry `derivesBase`).
- Existing `internal/model` tests survive: the empty-config conns in
  `parallel_tool_calls_issue282_test.go`, `tool_choice_test.go`, `transport_test.go` only call
  `buildRequest` / inspect `client.Transport` — they never drive `complete`/`completeStream`/
  `doJSON`, so a set `configErr` does not affect them. `ListModels` tests use valid configs
  (`configErr` nil), so gating `doJSON` is inert for them.
- Scoped model-empty check only touches `openrouter`/`zai`; `openai`-with-endpoint + empty
  model and Vertex are unaffected.
- `NewModelConnection()` (bare default, `connection.go:718`) does not run the check — the
  intentional library default still works.
- 404 retryability unchanged; `analyzeError`'s other cases untouched; Vertex's
  `validateConfig`/`configErr` flow preserved (orthogonal — Vertex derives its base so it
  never trips check 1).
- Adding the `derivesBase` field is additive: the zero value is `false`, so any provider whose
  registration is not updated simply behaves as "non-deriving" (correct for `openai`).
- Expected green: `gofmt`/`go build`/`go vet`/`golangci-lint` (0 new) and `go test ./...`,
  except the pre-existing, unrelated environmental `TestUserSessionSendMessage` 404 in
  `internal/agent` (no live model endpoint) — not in scope, not "fixed" here.

**(4) Holistic across both repos.** Change lives in the right place — `internal/model`, which
owns provider routing and already has the `configErr` deferred-error seam — not in `gogent.go`
(which would miss the Scan caller, proving `buildConnection`-only placement is wrong) and not
in `modelsdev` (import cycle; out of scope). The deriving knowledge is the registry's, not a
third hardcoded map. The gogent↔turbotui seam is respected: the new error is an ordinary
`error` from the existing completion/scan paths; `grep` of `$HOME/work/turbotui` for
`unexpected error: status`, `configErr`, `localhost:8080`, `DefaultModelURL` shows **zero**
coupling, so turbotui renders the improved message with **no code change and no `go.mod`
bump**.

## Test plan (`internal/model`, new test file)

Drive `NewModelConnectionFromConfig` and assert deferred behavior via `configErr` (inspect it
directly and/or assert the first completion returns it — matching how Vertex's deferred error
is tested):

- empty `api_type` + empty `endpoint` → **rejected**: `configErr` set; first completion returns
  it; message contains the model name and "api_type and endpoint are both empty"; assert it is
  **not** a localhost 404.
- unrecognized `api_type` (e.g. `"opnai"`) + empty `endpoint` → **rejected**; message echoes the
  raw `"opnai"` token (documents the typo-visibility behavior of rule 1).
- explicit `api_type:"openai"` + empty `endpoint` → **accepted** (no `configErr`) — guards the
  documented local-default path against over-rejection.
- each deriving provider `{zai, openrouter, vertex, vertex-native, vertex-anthropic, anthropic}`
  with empty `endpoint` → **accepted** from the new check (Vertex still needs project/location
  via its own validate — set them, or assert specifically that the *new* routability check did
  not fire).
- local `openai` + explicit `endpoint` (+ empty `model`) → **accepted**; **negative control:**
  assert `configErr == nil`.
- hosted gateway (`openrouter`/`zai`) + empty `model` → **rejected** with a model-named message
  naming the empty model field.
- Scan/list coverage: a misconfigured entry's `ListModels`/`doJSON` returns the same
  `configErr` (not a localhost dial) — covers the `doJSON` gate.

Keep all existing `internal/model` tests passing.

## Open questions

1. **Unrecognized-vs-empty api_type.** Default rejects both empty and unrecognized api_type
   (with empty endpoint), while accepting explicit `"openai"`. If the maintainer wants the
   strictest literal reading (reject **only** `api_type == ""`), drop the unrecognized-token
   branch — trivial. Default here catches typos with a clear message.
2. **Gateways for the empty-model check.** Scoped to `{openrouter, zai}`. Include `anthropic`
   (also a hosted gateway needing a model)? Left out to stay conservative; trivial to add.
3. **Single source of truth for the deriving set.** Now registry-derived within `model`;
   `modelsdev` keeps its own copy. A follow-up could export `model.DerivesBaseURL` and have
   `modelsdev` consume it — out of this issue's scope.
