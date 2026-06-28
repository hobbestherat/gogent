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
2. `buildRequest` copies `c.ModelName` to `reqBody.Model` verbatim — no normalization, so even
   an old on-disk `config.json` that bypassed validation sends the bare name.
3. The 400 body is a **JSON array** `[{"error":{"message":"…"}}]`. `extractProviderMessage`
   (#558) only understands the object form `{"error":{"message":…}}`; the array falls through
   to its raw-body fallback, dumping the whole array JSON as the reason instead of the clean
   message.

This is the **vertex shim only**. `vertex-native` (#573, the `geminiAdapter` :generateContent
route) names models **bare** and is unaffected by the wire-format issue.

## Change (gogent only; stdlib-first; no new deps; no go.mod bump)

Three layered fixes, each at an existing seam. The MODEL/CONFIG lane only —
`internal/model/*` + the existing call sites in `internal/gogent/gogent.go` (unchanged).

### 1. Validate at save & load — `internal/model/connection.go` (`validateRoutableConfig`)

Add a third rule to `validateRoutableConfig` (the body of the exported `ValidateModelConfig`,
already invoked at every save/load/default-resolve site — `gogent.go:2006/2045/2663/3429/3559`).
No new call sites, no new exported surface:

```go
// 3. Vertex OpenAI-compat shim requires a publisher-qualified model id (google/<model>).
//    The shim addresses Gemini as "google/gemini-…"; a bare name reaches Vertex and is
//    rejected with an opaque 400. Caught here so it fails at save/load instead (issue #574).
if resolved == APITypeVertex {
    m := strings.TrimSpace(cfg.Model)
    if m != "" && !strings.Contains(m, "/") {
        return &ModelError{
            Type: ErrorGeneric,
            Message: fmt.Sprintf(
                "model %q is misconfigured: api_type \"vertex\" requires a publisher-qualified "+
                    "model id like \"google/gemini-3.5-flash\" (got %q). Use api_type "+
                    "\"vertex-native\" (alias \"gemini\") for bare Gemini model names.",
                configModelName(cfg), m),
        }
    }
}
```

- Scoped to `resolved == APITypeVertex` (the shim) — `vertex-native`/`gemini`,
  `vertex-anthropic`, and every other provider are untouched.
- Guarded on `m != ""`: an empty model is left to the existing rules / provider validation,
  so this rule does exactly one thing — reject a present-but-unqualified name. (See Open
  questions on whether empty should also be rejected.)
- Reuses `configModelName` for the model-naming convention shared by the other two rules.
- Flows through `ErrModelInvalid` → HTTP 400 at the server seam and `showConfirm` in the
  editor, identical to every other #532 rejection. No new plumbing.

### 2. Normalize at request-build (defense in depth) — `provider_vertex.go` + `buildRequest`

For a config that bypassed validation (old on-disk `config.json`, hand-edit), auto-prefix
`google/` at the request-build choke point so the request either works or — if still wrong —
produces a *clean* 400 (handled by fix 3) instead of a bare-name 400.

Add a per-provider model-id normalizer, mirroring the lister's existing `format` field, so the
rule lives next to `googlePublisherModelID` and is applied at the single send seam:

- New field on `provider` (`provider.go`): `normalizeModelID func(string) string` (nil = identity).
- `provider_vertex.go`: a tiny helper that **reuses `googlePublisherModelID`** (criterion-4
  "reuse" gate) but only when the id is not already publisher-qualified:

```go
// ensureGooglePublisher qualifies a bare model id for the Vertex OpenAI-compat shim,
// which addresses Gemini as "google/<model>". It is the request-build counterpart of the
// lister's googlePublisherModelID: it applies the SAME rule the Model-Garden lister applies
// to discovered ids, but only when the id lacks a publisher (so an already-correct
// "google/gemini-…" — or another publisher — is left untouched). Defense in depth for a
// config that bypassed ValidateModelConfig (issue #574).
func ensureGooglePublisher(id string) string {
    if id == "" || strings.Contains(id, "/") {
        return id
    }
    return googlePublisherModelID(id)
}
```

  Wire it on the shim provider: `normalizeModelID: ensureGooglePublisher`.

- `buildRequest` (`connection.go:1498`) applies it when setting the model:

```go
if c.ModelName != "" {
    reqBody.Model = c.ModelName
    if c.provider != nil && c.provider.normalizeModelID != nil {
        reqBody.Model = c.provider.normalizeModelID(reqBody.Model)
    }
}
```

`buildRequest` stays provider-agnostic — it just calls the provider's optional hook; the
vertex-specific knowledge stays in `provider_vertex.go`. Every non-vertex provider leaves
`normalizeModelID` nil → byte-identical request. The vertex-shim adapter is `openAIAdapter`
(shared with openai/zai/openrouter), so the hook belongs on the **provider**, not the adapter.

**vertex-native:** also given `normalizeModelID: stripPublisherPrefix` — a symmetric
defense-in-depth strip that reuses the existing `lastPathSegment` helper
(`"google/gemini-3.5-flash"` → `"gemini-3.5-flash"`, bare names unchanged). This is the
documented criterion-4 rule for "google/… on vertex-native" → **stripped to bare**. It mirrors
the native lister's `bareModelID` formatting and turns a would-be broken
`publishers/google/models/google/gemini-…` path into a working request. Valid native configs
(already bare) are identity-mapped, so native behavior for valid configs is unchanged.

```go
// stripPublisherPrefix bares a model id for the native Gemini route, which names models
// bare and interpolates them into publishers/google/models/<id>:generateContent. Mirrors
// the native lister's bareModelID; reuses lastPathSegment. Identity for an already-bare id.
func stripPublisherPrefix(id string) string { return lastPathSegment(id) }
```

### 3. Clearer error mapping — `internal/model/provider_error.go` (`extractProviderMessage`)

The #558 extractor handles object-form bodies; extend it to handle the **array form**
`[{"error":{…}}]` Vertex returns for this 400. Add an array branch before the raw-body
fallback (case 4):

```go
// Vertex returns some errors as a JSON ARRAY of error objects: [{"error":{"message":"…"}}].
// Recurse into the first element that yields a message so the clean reason surfaces instead
// of the whole array JSON (issue #574 / #558 follow-up).
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

Placed so a recognized array element (object with `error.message`) returns the clean message;
a degenerate array still falls through to the bounded raw-body fallback. Bounded by
`boundedReason`/`modelErrReasonMaxRunes` exactly like every other path.

Confirmed wiring: this 400 reaches `analyzeError`. The message contains neither "context" nor
"length", so it does **not** trip the case-400 context-overflow branch; it falls through to the
generic tail (`withReason("unexpected error: status 400", reason)`), now carrying the clean
`Malformed publisher model …` reason. `HTTPStatusCode: 400` and full `RawResponse` preserved;
400 stays non-retryable (`isRetryableStatus`), so it fails fast.

### Cross-repo seam (turbotui)

turbotui has **zero** references to `vertex` / `api_type` / `ValidateModelConfig` /
publisher logic (verified by grep over `$HOME/work/turbotui`). It is purely the frontend: it
renders the config form and displays whatever error string gogent returns. The validation
rejection travels gogent → `ErrModelInvalid` → HTTP 400 → turbotui's existing error display
with no schema or contract change. **No turbotui changes; the seam is respected.**

## Tests — `internal/model/vertex_test.go`, `internal/model/routable_config_validation_test.go`

1. `ValidateModelConfig` rejects `{api_type:"vertex", model:"gemini-3.5-flash"}` with the
   actionable message; accepts `{api_type:"vertex", model:"google/gemini-3.5-flash"}`.
2. `ValidateModelConfig` does **not** reject a bare-name `vertex-native`/`gemini` config
   (no false positive — the rule is shim-scoped); does not reject other providers.
3. Request-build normalization: a `vertex` connection with `ModelName:"gemini-3.5-flash"`
   produces `reqBody.Model == "google/gemini-3.5-flash"`; an already-qualified
   `google/gemini-3.5-flash` is left unchanged; a non-vertex provider is unchanged.
4. vertex-native strip: a `vertex-native` connection with `ModelName:"google/gemini-3.5-flash"`
   builds the model as bare `gemini-3.5-flash` (documented rule); a bare name is unchanged.
5. `analyzeError`/`extractProviderMessage`: the array-form body
   `[{"error":{"message":"Malformed publisher model (`model`: 'gemini-3.5-flash') …"}}]`
   surfaces the message in `ModelError.Message` with `HTTPStatusCode == 400`; object-form
   bodies still extract as before (regression guard).

## Design criteria

**(1) Goal match.** Exactly the issue's ask — a *fix*, no feature/refactor: catch the missing
publisher prefix at save/load (criterion 1), normalize at request-build (criterion 2), surface
the array-form 400 reason (criterion 3), tests (criterion 4). Scoped to the vertex shim; no
scope creep. Reuses `ValidateModelConfig`/`validateRoutableConfig` (#532),
`googlePublisherModelID` + `lastPathSegment` (the lister), and the #558 extractor.

**(2) Usability.** The user gets an actionable local error naming the exact fix
(`google/gemini-3.5-flash`, or switch to `vertex-native`) at save/load — when they can act on
it — instead of an opaque 400 on the first message. Defense-in-depth normalization means an old
on-disk config silently *works* rather than failing. If a request still 400s, the clean Vertex
reason is surfaced prominently rather than swallowed. Nothing silent.

**(3) No regressions.** New validation rule is gated on `resolved == APITypeVertex` **and**
`model != "" && !strings.Contains(model,"/")` — valid vertex configs, vertex-native,
vertex-anthropic, and all other providers pass unchanged. `normalizeModelID` is an opt-in
provider hook (nil for every non-vertex provider → byte-identical request body); the
already-qualified and bare-native cases are identity-mapped, so valid configs are untouched.
The array branch in `extractProviderMessage` runs only for `[`-leading bodies and preserves the
object-form path and its empty-body `""` contract. 400 stays non-retryable. Existing
session/transcript invariants are untouched (no Message/wire shape change).

**(4) Holistic / both repos.** Right place: validation in the one routability function all
save/load paths already share; normalization at the single `buildRequest` model-set seam via a
provider hook next to the lister rule it mirrors; reason extraction in the shared extractor.
The gogent↔turbotui seam is respected — validation is server-side, surfaced as an error string
turbotui already renders; no frontend change and no contract change. CONFLICTS with #573 (both
touch `connection.go` + vertex): serialize — land after #573 merges (or take the model lane
when free) and rebase onto current `origin/main` at the gate. gogent-only; no new deps; no
go.mod bump.

## Open questions

1. **Empty model on `vertex`.** The new rule ignores an empty `model` (`m != ""`) to stay
   focused on the reported bare-name case and avoid overlapping the existing hosted-gateway
   empty-model rule. An empty model on the vertex shim is also invalid, but is arguably a
   separate concern; I lean to **leaving it out** of this fix. Reject empty too if preferred.
2. **vertex-native: strip vs reject.** I chose **strip-to-bare** at request-build (defense in
   depth, mirrors `bareModelID`, keeps native "unchanged" for valid configs) over a validation
   rejection of `google/…` on native. A symmetric *validation* rejection (native must be bare)
   is the alternative; I judged the silent auto-fix friendlier and lower-risk, but can add the
   rejection instead if the maintainer prefers strictness symmetry with the shim rule.
