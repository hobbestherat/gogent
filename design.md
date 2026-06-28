# Design — #574: vertex (OpenAI-compat shim) silently accepts a bare model name → opaque 400

## Problem

`api_type "vertex"` is Gemini via Vertex's **OpenAI-compatible shim** (`openAIAdapter`,
route `/endpoints/openapi/chat/completions`). On that surface the `model` field must be
**publisher-qualified** — `google/<model>` (e.g. `google/gemini-3.5-flash`). A bare
`gemini-3.5-flash` is accepted at save/load and sent verbatim in the request body; ADC
auth succeeds, the request reaches Vertex, and Vertex rejects with an opaque HTTP 400:

```
Malformed publisher model (`model`: 'gemini-3.5-flash') for the 'openapi' request
endpoint ID; expected '<publisher>/<model>'.
```

Three gaps let this through:

1. `validateRoutableConfig` (the #532 hardening) has no vertex-shim publisher-prefix rule,
   so the bad config saves/loads cleanly.
2. `buildRequest` copies `c.ModelName` to `reqBody.Model` verbatim — no normalization, so a
   bare name reaches the shim wire unmodified.
3. The 400 body is a **JSON array** `[{"error":{"message":"…"}}]`. `extractProviderMessage`
   (#558) only understands the object form `{"error":{"message":…}}`; the array fails its
   single `json.Unmarshal` (`provider_error.go:46`) and dumps the whole array JSON via the
   raw-body fallback (`:69`) instead of the clean message.

This is the **vertex shim only**. `vertex-native` (#573, the `geminiAdapter` :generateContent
route) names models **bare** and carries the model in the **URL path**, not the body.

## How the model actually reaches the wire (verified)

This drives every seam choice below:

- **Shim (`openAIAdapter`):** `buildBody` is `encodeJSON(buf, req)` (`adapter.go:77`); the model
  travels in the body as `CompletionRequest.Model` (`json:"model,omitempty"`, `connection.go:340`),
  set from `c.ModelName` in `buildRequest` (`connection.go:1498`). **So body normalization works
  for the shim.**
- **Native (`geminiAdapter`):** `buildBody` emits `geminiRequest{Contents,…}` and **never** `Model`
  (`adapter.go:1029`). The native model is baked into `c.URL` at **construction** —
  `NewModelConnectionFromConfig` → `modelURLEndpoints.endpoints` → `vertexNativeChatURL(base, cfg.Model)`
  (`provider.go:405`, `connection.go:875`). **So `buildRequest` normalization is a no-op for native**
  — it would mutate a field that is never sent. Native must be handled at validation (or the URL
  builder), not at `buildRequest`.
- **`configErr` gates `buildRequest`:** `complete` returns `c.configErr` *before* calling
  `buildRequest` (`connection.go:1546`). `NewModelConnectionFromConfig` sets `configErr` from
  `validateRoutableConfig` (`connection.go:895`). So once Fix #1 lands, a bare shim config sets
  `configErr` and `buildRequest` (hence Fix #2) **never runs for it**. Fix #1 and Fix #2 are
  therefore *not* additive on the config-file path — validation wins. Fix #2's real role is stated
  honestly below.

## Change (gogent only; stdlib-first; no new deps; no go.mod bump)

MODEL/CONFIG lane only: `internal/model/{connection.go, provider.go, provider_vertex.go, provider_error.go}`.
The save/load/default call sites in `internal/gogent/gogent.go` already invoke `ValidateModelConfig`
and are **not** edited.

### 1. Validate at save & load — `connection.go` (`validateRoutableConfig`)

Add a vertex-family rule to `validateRoutableConfig` (the body of exported `ValidateModelConfig`,
already called at every save/load/default-resolve site — `gogent.go:2006/2045/2663/3429/3559` — and
at connection construction). One rule, two symmetric halves:

```go
// 3. Vertex model-id shape. The OpenAI-compat shim ("vertex") addresses Gemini as
//    "google/<model>"; the native route ("vertex-native"/"gemini") names it bare and
//    interpolates it into the URL path. A mismatch reaches Vertex and 400s opaquely
//    (shim) or builds a broken URL (native), so reject it here with an actionable
//    message at save/load instead (issue #574).
switch resolved {
case APITypeVertex:
    if m := strings.TrimSpace(cfg.Model); m != "" && !strings.Contains(m, "/") {
        return &ModelError{Type: ErrorGeneric, Message: fmt.Sprintf(
            "model %q is misconfigured: api_type \"vertex\" requires a publisher-qualified "+
                "model id like \"google/gemini-3.5-flash\" (got %q). Use api_type "+
                "\"vertex-native\" (alias \"gemini\") for bare Gemini model names.",
            configModelName(cfg), m)}
    }
case APITypeVertexNative:
    if m := strings.TrimSpace(cfg.Model); strings.Contains(m, "/") {
        return &ModelError{Type: ErrorGeneric, Message: fmt.Sprintf(
            "model %q is misconfigured: api_type \"vertex-native\" requires a bare model id "+
                "like \"gemini-3.5-flash\" (got %q). Use api_type \"vertex\" for "+
                "publisher-qualified model ids.",
            configModelName(cfg), m)}
    }
}
```

- **Shim half** guarded on `m != ""` — an empty model is left to the existing rules / provider
  validation (one job: reject a *present-but-unqualified* name).
- **Native half** is the chosen, concrete answer to criterion 4's "google/… on vertex-native →
  rejected or stripped": **rejected**, symmetric to the shim, in the same seam, touching **no**
  request-building code. It catches a config that today silently builds a broken
  `…/publishers/google/models/google/gemini-…:generateContent` URL. Valid bare native configs are
  unaffected (no `/`), so "vertex-native unchanged" holds for every *valid* config — only the
  already-broken qualified-on-native case changes (opaque failure → clear local error). I chose
  reject over strip because strip would force a real change to the native URL builder; see Open
  questions for the strip alternative.
- `vertex-anthropic` is deliberately untouched (its bare-model `publishers/anthropic/models/<model>`
  path is correct; not in scope).
- Reuses `configModelName`, flows through `ErrModelInvalid` → HTTP 400 / editor `showConfirm` like
  every other #532 rejection. No new plumbing, no new exported surface.

**Error precedence (explicit decision).** `validateRoutableConfig` runs **before** the provider's
own `vertexValidate` (project/location), both at construction (`connection.go:895-898`) and as the
*only* validator at save/load. So for a bare-shim config that *also* lacks project/location, the
publisher-prefix error now surfaces first, shadowing "project and location are required". This is
intentional: a bare model on the shim is an api_type-misuse — the more fundamental thing to fix —
and it is now caught even at save/load, where `vertexValidate` never ran. Test impact is handled in
§Tests.

### 2. Normalize at request-build — shim only — `provider.go` + `provider_vertex.go` + `buildRequest`

Acceptance criterion 2 / the gate require request-build normalization. Add a per-provider model-id
normalizer, mirroring the lister's `format` field, applied at the single send seam:

- New optional field on `provider` (`provider.go:229`): `normalizeModelID func(string) string`
  (nil = identity; safe — all providers use keyed init).
- `provider_vertex.go`: a helper that **reuses `googlePublisherModelID`** (the criterion-4 reuse
  gate) but only when the id is not already publisher-qualified:

```go
// ensureGooglePublisher qualifies a bare model id for the Vertex OpenAI-compat shim. It is the
// request-build counterpart of the lister's googlePublisherModelID — the same rule the Model-Garden
// lister applies to discovered ids — but only when the id lacks a publisher, so an already-correct
// "google/gemini-…" (or another publisher) is left untouched (issue #574).
func ensureGooglePublisher(id string) string {
    if id == "" || strings.Contains(id, "/") {
        return id
    }
    return googlePublisherModelID(id)
}
```

  Wire it on the **shim** provider only: `normalizeModelID: ensureGooglePublisher`. **Native gets no
  normalizer** (it would be a dead no-op — model is in the URL, see above; native is handled by Fix
  #1's validation half instead).

- `buildRequest` (`connection.go:1498`) applies the hook when setting the model:

```go
if c.ModelName != "" {
    reqBody.Model = c.ModelName
    if c.provider != nil && c.provider.normalizeModelID != nil {
        reqBody.Model = c.provider.normalizeModelID(reqBody.Model)
    }
}
```

`buildRequest` stays provider-agnostic; the vertex knowledge lives in `provider_vertex.go`. Every
non-shim provider leaves `normalizeModelID` nil → byte-identical request body. The shim belongs on
the **provider**, not the adapter (`openAIAdapter` is shared with openai/zai/openrouter).

**Honest scope of Fix #2 (resolves the "mutually defeating" concern).** Because Fix #1 sets
`configErr` for a bare shim config and `complete` short-circuits before `buildRequest`, this
normalization does **not** rescue on-disk `config.json` files — those are caught earlier and *better*
by Fix #1 (a clear, user-facing save/load error; and `sweepUnroutableModels` quarantines them at
load). Fix #2 is the **last-line send invariant**: it guarantees no bare model can leave the shim
send path even for a connection constructed *outside* the config-validation layers — a hand-built /
library connection (`NewModelConnection` + manual `ModelName`/provider), a test, or any future caller
that builds a request bypassing `validateRoutableConfig`. The design no longer claims it "silently
fixes" on-disk configs; its value is defense-in-depth against a future code path that drops a
validation layer, keeping the "never send bare to Vertex" invariant local to the send seam.

### 3. Clearer error mapping — `provider_error.go` (`extractProviderMessage`)

Extend the #558 extractor to handle the **array form** `[{"error":{…}}]`. Add an array branch before
the raw-body fallback:

```go
// Vertex returns some errors as a JSON ARRAY of error objects: [{"error":{"message":"…"}}].
// Recurse into the first element that yields a message so the clean reason surfaces instead of
// the whole array JSON (issue #574 / #558 follow-up).
if trimmed[0] == '[' {
    var arr []json.RawMessage
    if json.Unmarshal([]byte(trimmed), &arr) == nil {
        for _, el := range arr {
            if msg := extractProviderMessage(string(el)); msg != "" {
                return msg
            }
        }
    }
}
```

Safe (`trimmed != ""` already guaranteed above; object bodies never start with `[`, so the existing
path and its empty-body `""` contract are untouched). A recognized array element returns the clean
`error.message`; a degenerate array still falls through to the bounded raw fallback. Result is bounded
by `boundedReason`/`modelErrReasonMaxRunes` exactly like every other path.

**Confirmed classification.** This 400 reaches `analyzeError`. Its case-400 branch classifies on the
**raw** body for the substrings `"context"`/`"length"` (`connection.go:1935`); the realistic Vertex
body `[{"error":{"code":400,"message":"Malformed publisher model …","status":"INVALID_ARGUMENT"}}]`
contains neither, so it falls through to the generic tail
(`withReason("unexpected error: status 400", reason)`), now carrying the clean reason, as
`ErrorGeneric` with `HTTPStatusCode: 400` and full `RawResponse`. 400 stays non-retryable
(`isRetryableStatus`) → fails fast. The test pins `Type == ErrorGeneric` to guard against any future
body whose other fields might trip the heuristic (§Tests #5). The 400 context/length heuristic itself
is pre-existing and left unchanged (out of scope).

### Cross-repo seam (turbotui)

turbotui has **zero** references to `vertex` / `api_type` / `ValidateModelConfig` / publisher logic
and does not import `gogent/internal` (verified by grep over `$HOME/work/turbotui`). It is purely the
frontend: it renders the config form and displays whatever error string gogent returns. The rejection
travels gogent → `ErrModelInvalid` → HTTP 400 → turbotui's existing error display with no schema or
contract change. **No turbotui changes; the seam is respected.**

## Tests — `internal/model/vertex_test.go`, `internal/model/routable_config_validation_test.go`

**New:**

1. `ValidateModelConfig` rejects `{api_type:"vertex", model:"gemini-3.5-flash"}` with the actionable
   shim message; accepts `{api_type:"vertex", model:"google/gemini-3.5-flash"}`.
2. `ValidateModelConfig` rejects `{api_type:"vertex-native", model:"google/gemini-3.5-flash"}` with
   the bare-name message; accepts `{api_type:"vertex-native", model:"gemini-3.5-flash"}` and
   `{api_type:"gemini", model:"gemini-3.5-flash"}`. (Documents the chosen native rule: **bare
   required**.) Confirms the shim/native rules do not fire for `vertex-anthropic` or other providers.
3. Request-build normalization (shim): a connection on the shim provider with
   `ModelName:"gemini-3.5-flash"` and `configErr == nil` (built by setting the fields directly so
   the test exercises the send-path invariant Fix #2 guards, independent of validation) yields
   `buildRequest(...).Model == "google/gemini-3.5-flash"`; an already-qualified
   `google/gemini-3.5-flash` is unchanged; a non-shim provider's `reqBody.Model` is unchanged.
4. `ensureGooglePublisher` unit: bare → `google/…`; qualified → unchanged; empty → empty.
5. `analyzeError`/`extractProviderMessage`: the array-form body
   `` [{"error":{"code":400,"message":"Malformed publisher model (`model`: 'gemini-3.5-flash') for the 'openapi' request endpoint ID; expected '<publisher>/<model>'.","status":"INVALID_ARGUMENT"}}] ``
   yields `ModelError.Type == ErrorGeneric`, `HTTPStatusCode == 400`, and `Message` containing
   `Malformed publisher model`. A regression case asserts the object form
   `{"error":{"message":"…"}}` still extracts identically.

**Existing tests that MUST be updated (they encode the bug as valid):**

6. `routable_config_validation_test.go` `TestRoutableValidation_ValidConfigs_Accepted`: the
   `vertexReady` helper (`:213`) builds `Model:"gemini-2.5-flash"` (bare) for all three vertex
   api_types. With Fix #1 the **shim** case (`vertexReady("vertex")`, `:235`) now rejects. Fix:
   make the shim fixture use `google/gemini-2.5-flash` while `vertex-native`/`vertex-anthropic`
   stay bare (e.g. give `vertexReady` a per-api_type model, or split the shim case out).
7. `TestRoutableValidation_VertexMissingProjectLocation_StillUsesVertexValidate` (`:255`): its
   `{api_type:"vertex", model:"gemini-2.5-flash"}` now hits the new publisher rule *before*
   `vertexValidate`, so `Message` no longer contains `"project and location are required"` and now
   contains `"is misconfigured"` — flipping both asserts. Fix: change the fixture model to
   `google/gemini-2.5-flash` so the publisher rule passes and the test still exercises the
   project/location precedence it was written for. (Add a *separate* new case asserting the
   bare-shim publisher error to lock the new precedence.)
8. Sweep the suite for any other bare-shim or qualified-native vertex fixtures before merge
   (`grep -rn 'APIType: *"vertex"' internal` etc.); update or relabel as above.

## Design criteria

**(1) Goal match.** A *fix*, scoped to the vertex family the issue names: shim bare-name caught at
save/load (criterion 1) and normalized as a send invariant (criterion 2), native mismatch caught at
save/load (criterion 4's documented native rule), array-form 400 reason surfaced (criterion 3), tests
incl. the existing-fixture updates. No feature/refactor; reuses `validateRoutableConfig`/
`ValidateModelConfig` (#532), `googlePublisherModelID` (the lister), and `extractProviderMessage`
(#558). The earlier draft's native body-normalization was a dead no-op (model is URL-baked) and is
**replaced** by the native validation rule.

**(2) Usability.** The user gets an actionable local error naming the exact fix — `google/…` for the
shim, bare for native, with the correct alternate api_type — at save/load, when they can act on it,
instead of an opaque Vertex 400 (or a silently broken native URL) on the first message. The array-400
reason is now surfaced prominently rather than dumped as raw JSON. Honest behavior: a pre-existing bad
on-disk config is **quarantined with a clear error** (sweep at load + `configErr` on use), not
silently auto-fixed — the design no longer overclaims a silent rescue. Nothing fails silently.

**(3) No regressions.** Validation rules are gated on `resolved == APITypeVertex` (require `/`,
non-empty) / `APITypeVertexNative` (forbid `/`) — every valid config of those types, and every other
provider, is untouched; the production default (`config.go`, `google/gemini-2.5-flash` on the shim)
already complies. `normalizeModelID` is an opt-in provider hook, nil everywhere except the shim, where
qualified/empty are identity-mapped → byte-identical request for all valid configs. The array branch
runs only for `[`-leading bodies and preserves the object-form path + empty-body contract; 400 stays
non-retryable and `ErrorGeneric`. The two existing tests that encoded the bug as valid are explicitly
identified and updated (§Tests 6–7), and the new error-precedence (publisher-prefix shadows
project/location) is documented, not silent. No `Message`/wire-shape change → session/transcript
invariants intact.

**(4) Holistic / both repos.** Right seams: validation in the one routability function all save/load
paths share; the shim send-invariant at the single `buildRequest` model-set seam via a provider hook
next to the lister rule it mirrors; native handled at validation (not the dead body seam); reason
extraction in the shared extractor. gogent↔turbotui seam respected — server-side validation surfaced
as an error string turbotui already renders; no frontend or contract change. CONFLICTS with #573 (both
touch `connection.go` + vertex): serialize — land after #573, rebase onto current `origin/main` at the
gate. gogent-only; no new deps; no go.mod bump.

## Open questions

1. **Empty model on `vertex`.** The shim rule ignores an empty `model` (`m != ""`) to stay focused on
   the reported bare-name case. An empty shim model is also invalid; I lean to leaving it out of this
   fix (separate concern). Reject empty too if preferred.
2. **Native: reject vs strip.** I chose **reject `google/…` at validation** (symmetric, single seam,
   no native request-path change, valid configs unchanged). The alternative — **strip to bare** — must
   live at the URL-build seam (`vertexNativeChatURL`/`StreamURL`, or normalize `cfg.Model` before
   `endpoints(cfg)` in `NewModelConnectionFromConfig`), since `buildRequest` cannot affect the native
   URL. Strip is a friendlier silent auto-fix but touches the native request path; reject is
   lower-risk and consistent with the #532 "validate at save/load" theme. Swap if the maintainer
   prefers the auto-fix.
3. **Error precedence.** Publisher-prefix now shadows missing project/location for a doubly-broken
   shim config (§Fix 1). If the maintainer prefers project/location first, reorder the new rule after
   `vertexValidate` — but note `vertexValidate` does not run at save/load, so only the construction-time
   message order would change; save/load would still report the publisher error.
