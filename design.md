# Design — gogent issue #505: misconfigured model entry must fail fast with a clear, model-named error

## Issue recap

A `ModelConfig` that cannot be routed — `api_type` AND `endpoint` both empty (or `model`
empty for a hosted gateway) — is silently sent to the generic OpenAI provider's
placeholder default base URL `http://localhost:8080/v1/chat/completions`. The first agent
turn then fails with an opaque `model round-trip: complete with tools: generic: unexpected
error: status 404` (a gogent daemon often answers on :8080; connection-refused otherwise).

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
first `CompleteWithTools` / streaming call (both `complete` `connection.go:1284` and
`completeStream` `connection.go:1419` short-circuit on `configErr`) rather than as an opaque
HTTP failure. This is the maintainer-preferred location and matches the Vertex
`vertexValidate` precedent (`provider_vertex.go:63`, wired at `connection.go:758`).

### Files / functions touched (gogent only)

- **`internal/model/provider.go`** — add a small predicate identifying api_types that derive
  their own base URL, and a config-name helper:
  - `baseURLDerivingAPITypes` (a `map[APIType]bool`) **or** `func derivesBaseURL(t APIType) bool`
    covering `{zai, openrouter, anthropic, vertex, vertex-native, vertex-anthropic}` —
    the model-package mirror of `modelsdev.deriveBaseAPITypes`
    (`internal/modelsdev/transform.go:18`). Placed next to `StringToAPIType`/`stringToAPITypeMap`
    so the api_type knowledge stays in one file. A comment cross-references
    `deriveBaseAPITypes` and notes the two must stay in sync (see "Reuse note" below).
  - `func configModelName(cfg *config.ModelConfig) string` → `cfg.Name`, else
    `cfg.DisplayName`, else `"<unnamed>"`. Used to name the model in the error.
- **`internal/model/connection.go`** — in `NewModelConnectionFromConfig`, *before* (or
  alongside) the existing `p.validateConfig` block, run the new routability/model check and
  set `conn.configErr` when it fails. A standalone `func validateRoutableConfig(cfg *config.ModelConfig) error`
  keeps the constructor terse and is directly unit-testable.
- *(Optional polish)* `internal/model/connection.go` `analyzeError` — add a dedicated `404`
  case (see Secondary).
- **`internal/gogent/gogent.go`** — **not touched**. `buildConnection` (`gogent.go:1540`)
  already routes through `NewModelConnectionFromConfig`, so it inherits the check for free;
  putting validation there instead would miss the other `NewModelConnectionFromConfig`
  callers and diverge from the Vertex precedent.
- **turbotui** — **not touched**, no `go.mod` bump (see Criterion 4).

### Validation rule (precise)

Let `apiType = StringToAPIType(cfg.APIType)` and `endpointEmpty = strings.TrimSpace(cfg.Endpoint) == ""`.

1. **Routability (api_type + endpoint both unroutable):**
   Reject when `endpointEmpty && !derivesBaseURL(apiType)`.
   - Empty `api_type` resolves to `openai` (localhost placeholder) and is **not** in the
     deriving set → rejected. ✔ primary case.
   - `zai/openrouter/anthropic/vertex*` derive their base → **accepted** with empty endpoint. ✔
   - An explicit endpoint (non-empty) → accepted regardless of api_type. ✔
   - Message, named by `configModelName(cfg)` and naming the missing field(s):
     - `api_type` empty:
       `model "<name>" is misconfigured: api_type and endpoint are both empty (cannot determine where to send requests)`
     - `api_type` non-empty but non-deriving (e.g. `"openai"` or an unknown token, which also
       falls back to the localhost placeholder):
       `model "<name>" is misconfigured: endpoint is empty and api_type "<api_type>" has no built-in base URL (set an explicit endpoint)`
   - Note: this *also* rejects explicit `api_type:"openai"` + empty endpoint, since that too
     silently hits the localhost placeholder. This is intentional and regression-safe — see
     Criterion 3 (all seeded local entries carry an explicit `Endpoint`, and the bare
     `NewModelConnection()` default path is untouched). Flagged in Open Questions in case the
     maintainer wants to narrow it to empty-`api_type`-only.

2. **Hosted-gateway empty model (scoped narrowly):**
   Reject when `cfg.Model` is empty **and** `apiType ∈ {openrouter, zai}` (known hosted
   gateways where an empty model is almost certainly a mistake).
   - **Not** a blanket empty-model reject: `api_type:"openai"` + explicit endpoint + empty
     model stays valid (some local servers ignore/auto-select the model). Vertex empty-model
     is left to its existing path. ✔ avoids over-rejection.
   - Message: `model "<name>" is misconfigured: model is empty (api_type "<api_type>" requires a model name)`.

If both checks fire (not reachable today — gateways derive their base, so endpoint-empty
never co-occurs with the gateway model check), the routability error takes precedence. The
checks are evaluated routability-first, returning the first failure.

`configErr` is a `*ModelError{Type: ErrorGeneric, Message: …}`, mirroring `vertexValidate`
(no `HTTPStatusCode` — it is a config error, not an HTTP status).

### Reuse note (why the deriving set is mirrored, not imported)

The canonical `deriveBaseAPITypes` lives in `internal/modelsdev`, but `modelsdev` already
**imports** `internal/model` (e.g. `transform_test.go:8`), so `model` importing `modelsdev`
would create an import cycle. The set is therefore mirrored in `model` (its natural home —
that package owns the providers and their base-URL resolvers) with a cross-reference comment.
The strictly-correct single-source-of-truth refactor (export `model.DerivesBaseURL` and have
`modelsdev` consume it) is out of this issue's stated scope (confined to `internal/model`);
called out in Open Questions.

## Secondary (optional polish): dedicated 404 in `analyzeError`

Add a `case 404:` to `analyzeError` (`connection.go:1644` switch) returning an `ErrorGeneric`
with a more descriptive message (e.g. `not found (status 404): the endpoint or model path is
wrong — check api_type/endpoint/model`). **Does not** alter retryability: `isRetryableStatus`
already omits 404, and that stays. Low-risk, purely a message improvement for genuine
wrong-endpoint 404s that slip past config validation. Will include only if it stays trivial.

## User-facing behavior

- Before: silent localhost target → `... generic: unexpected error: status 404` on turn 1.
- After: turn 1 immediately returns
  `model "<name>" is misconfigured: <missing field(s)> ...`, surfaced through the normal
  completion error path the TUI already renders. The user can see exactly which entry and
  which field to fix.

## The four design gates

**(1) Goal match.** Exactly the issue's ask: a fix (deferred validation), not a feature or
refactor. Unroutable config → clear model-named error via `configErr`; no scope creep beyond
`internal/model`. The localhost silent-fallback for from-config connections is closed.

**(2) Usability.** Error names the model (`cfg.Name`/`DisplayName`) and the precise missing
field(s); it is actionable ("set an explicit endpoint" / "requires a model name"). It
surfaces on first completion exactly like the Vertex precedent — not as an opaque HTTP error,
not silently. The user drives the fix via their config entry.

**(3) No regressions.**
- Valid configs unaffected: local `openai` with explicit endpoint (all seeded defaults set
  `Endpoint` via `DefaultEndpoint()`, `config.go:1061`) build unchanged; deriving providers
  (`zai/openrouter/anthropic/vertex/vertex-native/vertex-anthropic`) with empty endpoint still
  build (they're excluded from the routability check).
- Scoped model-empty check only touches `openrouter`/`zai`, so `openai`-with-endpoint +
  empty model and Vertex are unaffected.
- `NewModelConnection()` (bare default, `connection.go:718`) does not run the check — the
  intentional library default still works.
- 404 retryability is unchanged. `analyzeError`'s other cases untouched.
- Vertex's existing `validateConfig`/`configErr` flow is preserved (both can set `configErr`;
  the new check runs first but is orthogonal — Vertex derives its base so it never trips the
  routability rule).
- Expected green: `gofmt`/`go build`/`go vet`/`golangci-lint` (0 new) and `go test ./...`,
  except the pre-existing, unrelated environmental `TestUserSessionSendMessage` 404 in
  `internal/agent` (no live model endpoint), which is not in scope and not "fixed" here.

**(4) Holistic across both repos.** Change lives in the right place — `internal/model`, the
package that owns provider routing and already has the `configErr` deferred-error seam — not
in `gogent.go` (which would miss callers) and not in `modelsdev` (import-cycle; out of scope).
The gogent↔turbotui seam is respected: the new error is an ordinary `error` returned from the
existing completion path; `grep` of `$HOME/work/turbotui` shows no coupling to the old
`"unexpected error: status 404"` string or to `configErr`, so turbotui renders the improved
message with **no code change and no `go.mod` bump**. The `deriveBaseAPITypes` knowledge is
reused (mirrored, with a sync comment, because of the import direction).

## Test plan (`internal/model`, new test file)

Drive `NewModelConnectionFromConfig` and assert first-completion behavior via `configErr`
(call a completion with a stub/no network, or inspect that `configErr` is set and that the
completion returns it — matching how Vertex's deferred error is tested):

- empty `api_type` + empty `endpoint` → **rejected**: `configErr` set; first completion
  returns the error; message contains the model name and "api_type and endpoint are both
  empty" (assert it is **not** a localhost 404).
- each deriving provider `{zai, openrouter, vertex, vertex-native, vertex-anthropic,
  anthropic}` with empty `endpoint` → **accepted** (no `configErr` from the new check; Vertex
  still needs project/location via its own validate — set them or assert only the new check
  didn't fire).
- local `openai` + explicit `endpoint` (and empty `model`) → **accepted**.
- hosted gateway (`openrouter`/`zai`) + empty `model` → **rejected** with a model-named
  message naming the empty model field.
- (guard) explicit `api_type:"openai"` + empty `endpoint` → rejected with the
  "no built-in base URL" message (documents the intentional behavior from rule 1).

Keep all existing `internal/model` tests passing.

## Open questions

1. **Scope of the routability check on explicit `openai` + empty endpoint.** The design
   rejects it (it silently hits localhost). The issue's headline case is empty-`api_type`. If
   the maintainer wants the localhost dev-server fallback preserved for *explicitly* chosen
   `openai`, narrow rule 1 to `cfg.APIType == ""` only. Default here: reject (safer, matches
   "the localhost fallback is never intended").
2. **Gateways for the empty-model check.** Scoped to `{openrouter, zai}`. Should `anthropic`
   (also a hosted gateway requiring a model) be included? Left out to stay conservative;
   trivial to add if desired.
3. **Single source of truth for the deriving set.** Mirrored in `model` to avoid an import
   cycle. A follow-up could export `model.DerivesBaseURL` and have `modelsdev` consume it —
   out of this issue's scope.
