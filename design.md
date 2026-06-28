# Design — VERTEX-NATIVE-GEMINI-ADAPTER-BROKEN-3X (gogent issue #573)

Fix the `vertex-native` (alias `gemini`) adapter so a full agent loop on Gemini 3.x
(thinking enabled by default) completes with **zero HTTP 400s** from Vertex. Two
independent defects, both in `internal/model`, each yielding a 400:

1. **Nullable-union `type` arrays** → `"Proto field is not repeating, cannot start list"`
   (fires on the *first* turn, because the default `grep` tool ships a
   `{"type":["string","null"],"enum":[...]}` `output_mode`).
2. **Dropped `thoughtSignature` round-trip** → `"Function call is missing a thought_signature
   in functionCall parts"` (fires on the *second* turn, when prior `functionCall`s are
   echoed back into conversation history).

Scope is `api_type` **`vertex-native`/`gemini` only**. The OpenAI-compat `vertex` shim
(#574) is untouched; Anthropic/OpenAI/Z.AI/OpenRouter are untouched. gogent-only,
stdlib-only, no new deps, no `go.mod` bump.

---

## Current behavior (verified in code)

- `geminiSchema(v)` (`adapter.go:1220`) deep-copies via a JSON round-trip and calls
  `uppercaseSchemaTypes` (`adapter.go:1239`). That walker upper-cases `type` **only when it
  is a plain string**; when `type` is an array it falls through to the generic recursion and
  the array is returned **unchanged** (its elements are bare strings, not under a `type` key,
  so they are also untouched). The doc comment claims "Gemini expresses nullability via the
  `nullable` field" — but the code never sets `nullable`. That is the bug: the array reaches
  the wire and Vertex rejects the repeated `type`. Called from `geminiBuildContents`
  (`adapter.go:1004`) for every tool's `Parameters`.
- The default `grep` tool (`internal/gogent/gogent.go:691-695`) and `read`
  (`gogent.go:390-392`) declare exactly these nullable-union types; `grep.output_mode`
  (`gogent.go:692`) additionally carries an `enum`, so it is the precise trigger named in the
  acceptance criteria.
- An adjacent precedent already exists: #567's `anthropicSchema` / `normalizeAnthropicSchemaTypes`
  / `dropNullFromType` (`adapter.go:1272-1345`) collapse the *same* nullable-union shape for the
  Anthropic strict validator. We mirror its intent but emit the **Gemini** proto form
  (scalar `type` + `nullable:true`), and apply it to **all** union types, not only enum-bearing
  ones (Gemini's `type` is a scalar enum regardless of whether an `enum` is present).
- On the response side, `geminiRespPart` (`adapter.go:1364`) decodes only `text`/`thought`/
  `functionCall` and **silently drops** the part-level `thoughtSignature`. `geminiToolCall`
  (`adapter.go:1445`) builds a `ToolCall` without it. When history is re-serialized,
  `geminiParts` (`adapter.go:1118-1134`) emits a `functionCall` part with **no** signature, so
  Vertex 400s on the next turn. `ToolCall` (`connection.go:42`) has no field to carry it.

---

## Fix 1 — Collapse nullable-union `type` to scalar + `nullable:true`

**Files/functions:** `internal/model/adapter.go` — `uppercaseSchemaTypes` (`:1239`, the walker
behind `geminiSchema`). No call-site change; `geminiSchema` and `geminiBuildContents` are
unchanged. The existing `dropNullFromType` (`:1328`) is reused as-is.

**Change:** in the `map[string]interface{}` branch, when the value under key `"type"` is a
`[]interface{}` union, normalize it instead of leaving it:

1. Run `dropNullFromType(union)` to strip every `"null"` member (returns a scalar when one
   non-null member remains, the trimmed array otherwise, plus a `changed` flag).
2. If a `"null"` member was present (i.e. it was nullable), set `t["nullable"] = true`.
   OR with any pre-existing `nullable` rather than clobbering it.
3. Apply the existing scalar upper-casing to the surviving `type`: if it collapsed to a single
   string, store `strings.ToUpper(scalar)`; the (rare, out-of-our-schemas) multi-non-null case
   is left as the trimmed array and logged-by-comment as unrepresentable in the Gemini proto —
   Gemini would still reject it, but no gogent default tool produces one, so we do not expand
   scope to `anyOf` here.
4. Keep recursing into every other map value and array element (`properties`, `items`, nested
   objects), exactly as today, so the rule applies at any depth.

`dropNullFromType` is reused **with no signature change**: its `changed` return already encodes
nullability (`changed==true` ⟺ a `null` was dropped *and* ≥1 non-null member survives), so it
doubles as the "set `nullable:true`" signal. Do **not** alter `dropNullFromType` — it is shared
with the Anthropic caller (`normalizeAnthropicSchemaTypes`, `:1304`) and any signature change
would ripple there.

Note `geminiSchema` is also the normalizer for the structured-output `ResponseSchema`
(`adapter.go:1083`), so Fix 1 *uniformly* also fixes any structured-output schema that carries a
nullable union — consistent and correct (acceptance criterion 3, structured output still works),
not a separate code path.

Because `geminiSchema` already deep-copies via the JSON round-trip before walking, mutating the
decoded map is safe — the caller's shared `Parameters` map is never touched (criterion 4 /
no-shared-mutation gate). Setting the new `"nullable"` key while ranging the map is safe in Go
(it is a bool, never recursed).

Result for `grep.output_mode`:
`{"type":["string","null"],"enum":[...]}` → `{"type":"STRING","enum":[...],"nullable":true}` —
a valid Gemini `Schema` (scalar `type` enum + `nullable` flag + value `enum`).

I will keep the transformation **inside the existing `uppercaseSchemaTypes` walker** (renaming
its doc comment to describe the now-implemented union handling) rather than adding a parallel
pass, so there is a single Gemini schema normalizer and the doc no longer lies about behavior.

---

## Fix 2 — Round-trip `thoughtSignature` on functionCall parts

`thoughtSignature` is a **part-level** field in the Gemini/Vertex wire (sibling to
`functionCall`/`text`/`thought`), base64 string in JSON. For the tool round-trip, the only one
Vertex *requires* echoed back is the signature attached to a `functionCall` part, so we carry it
on the tool call.

**Files/functions:**

- `internal/model/adapter.go`
  - `geminiRespPart` (`:1364`): add `ThoughtSignature string \`json:"thoughtSignature,omitempty"\``
    so parse captures it.
  - `geminiToolCall` (`:1445`): take the signature (new param, e.g.
    `geminiToolCall(fc *geminiRespFunctionCall, thoughtSig string)`) and set it on the returned
    `ToolCall`. Both call sites pass `p.ThoughtSignature`.
  - `parseResponse` loop (`:1418-1419`) and `parseStream` loop (`:1517-1521`): pass
    `p.ThoughtSignature` into `geminiToolCall`.
  - `geminiPart` (request struct, `:886`): add
    `ThoughtSignature string \`json:"thoughtSignature,omitempty"\``.
  - `geminiParts` (`:1118-1134`): when emitting the `functionCall` part for a tool call, set
    `ThoughtSignature: tc.ThoughtSignature` on the part (alongside the existing `FunctionCall`).
    `omitempty` means non-Gemini-origin tool calls (empty signature) emit nothing — byte-identical
    to today.

- `internal/model/connection.go`
  - `ToolCall` (`:42`): add `ThoughtSignature string \`json:"-"\``. **Off the wire** (`json:"-"`),
    exactly mirroring `Message.ThinkingSignature` (`:88`), the analogous Anthropic-on-Vertex
    signal. This keeps it out of the OpenAI/Z.AI/OpenRouter `tool_calls` wire (where a stray field
    can be rejected by strict APIs) **and** out of the persisted transcript, matching the existing
    Anthropic precedent precisely.

**Carry across the agent loop:** the agent loop already appends the assistant turn's
`resp.ToolCalls` into the in-memory `Messages` history and replays it through `geminiParts` on the
next request. With the field populated at parse and re-emitted at build, the signature survives
every in-loop turn with no change to the loop itself.

**Scope containment:** every change is gated by the field being non-empty, which only happens on
the Gemini parse path. `omitempty`/`json:"-"` mean other adapters serialize identically to today.

---

## User-facing behavior

No new UI, flags, prompts, or config. The only observable change is that a `vertex-native`
session that previously died with a Vertex 400 on the first (or second) tool-using turn now runs
the full loop to a final answer. Plain turns, structured output, and the default tool set behave
exactly as before on every other provider. Nothing is silenced: malformed-args and
no-candidates error paths (`geminiToolCall`, `parseResponse`) are unchanged, so genuine bad
provider output still surfaces as an error rather than a blank turn.

---

## Criterion-by-criterion

**(1) Goal match.** Exactly the two defects named in #573, nothing more. Fix 1 is a normalizer
extension (not a rewrite); Fix 2 is field capture/carry/re-emit. No new feature, no refactor of
the adapter, no `anyOf` generalization, no change to tool definitions. `grep`/`read` schemas in
`gogent.go` are referenced only as the trigger and are left untouched.

**(2) Usability.** The default model + default tool set become usable out of the box on
`vertex-native` with thinking on (the default) — which is the whole point of the issue. The user
drives nothing new; the fix is transparent. Errors that *should* surface still do.

**(3) No regressions.**
- Other providers: Fix 1 lives in `geminiSchema` only (Anthropic uses `anthropicSchema`, OpenAI
  sends raw schema); Fix 2's `geminiPart`/`geminiRespPart` are Gemini-only structs and the
  `ToolCall.ThoughtSignature` field is `json:"-"` + `omitempty`, so OpenAI/Z.AI/OpenRouter wire
  bytes are unchanged. The `vertex` OpenAI-compat shim (#574) shares no code path here.
- Deep-copy preserved: `geminiSchema` already JSON-round-trips before the walker mutates, so the
  caller's shared `Parameters` map is never mutated (concurrency-safe).
- Transcript invariants: `ThoughtSignature` is `json:"-"`, so the persisted transcript format and
  existing session/transcript round-trip tests are byte-unaffected; old transcripts load
  identically.
- Existing Gemini tests (`gemini_adapter_test.go`: build-body, parse-response, parse-stream,
  tool-choice, function-response) keep passing — emitted bodies are unchanged except where a
  schema *had* a union type (now collapsed) or a tool call *had* a signature (now re-emitted).
- **One existing test must be rewritten in lockstep with Fix 1** (it currently encodes the #573
  bug): `TestIssue567GeminiDoesNotCollapseNullableEnum` in
  `anthropic_strict_schema_issue567_test.go:628-662`. It runs `grepToolDef()` through
  `geminiAdapter{}.buildBody` and asserts (`:649-651`) that `output_mode.type` survives as a
  **union array still containing `"null"`** — exactly the falsehood Fix 1 corrects. After Fix 1
  the type collapses to scalar `"STRING"` + `nullable:true`, so this assertion fatals. It is **not**
  a regression to suppress: the test's premise was wrong. The fix rewrites it to assert the new
  correct Gemini shape (`type:"STRING"`, `nullable:true`, enum `[content,files_with_matches,count]`
  preserved, no surviving `null`) and corrects its now-stale comment (`:629-632`, which currently
  claims "Gemini accepts the union"). This edit lives in the #567 test file (different file from
  `gemini_adapter_test.go`), which is why it is called out explicitly here.
- gofmt/build/vet/golangci-lint clean; `go test ./...` green (with the rewrite above) except the
  pre-existing `TestUserSessionSendMessage` 404 (the one acceptable failure).

**(4) Holistic / two-repo seam.** Both fixes sit in `internal/model`, the correct layer (wire
adapter). `thoughtSignature` is `json:"-"`, so it never crosses into the transcript or out to
**turbotui** (confirmed: a grep of `$HOME/work/turbotui` for `ToolCall`/`thoughtSignature`/
`vertex-native`/`geminiSchema` returns nothing — turbotui does not touch this seam). No turbotui
change is needed or wanted; the repo boundary is respected. We reuse `dropNullFromType` and extend
the single existing `geminiSchema` normalizer rather than adding a parallel path, conceptually
coordinating with #567 (Anthropic) while emitting the Gemini-proto form. At the gate we rebase
onto `origin/main` (which carries #567's Anthropic normalizer in a different section of
`adapter.go`) and serialize against #574 (which also touches `connection.go`/vertex).

---

## Test plan (unit-level, no live API)

In `internal/model` (new file, e.g. `gemini_vertex_native_issue573_test.go`):

- **Fix 1:** call `geminiSchema` on a schema containing `{"type":["string","null"],"enum":[...]}`
  (the `grep.output_mode` shape) plus a nested `properties` entry and an `items` union; assert the
  output has scalar `"type":"STRING"`, `"nullable":true`, the `enum` preserved, recursion applied
  at depth, and **no remaining array-typed `type`**. Assert the input map is **not mutated**
  (deep-copy gate). Cover `["integer","null"]` and `["boolean","null"]` too (read/grep fields).
- **Fix 2 (round-trip):** feed a `geminiResponse` JSON with a `functionCall` part carrying
  `thoughtSignature` through `parseResponse`; assert `ToolCall.ThoughtSignature` is captured. Put
  that `ToolCall` into a `Message`, run the build path, and assert the emitted `functionCall`
  part re-emits the same `thoughtSignature`. Assert a signature-less tool call emits no
  `thoughtSignature` key (`omitempty`).
- **Structured-output guard:** call `geminiSchema` on a `ResponseSchema` carrying a nullable
  union (the same path as `adapter.go:1083`) and assert the same scalar+`nullable` collapse, so
  structured output keeps working on `vertex-native`.
- **Regression guard:** assert a non-Gemini `ToolCall` marshals with no `thought_signature`/
  `thoughtSignature` key (it is `json:"-"`).

**Existing test to rewrite (mandatory, same PR):**
`TestIssue567GeminiDoesNotCollapseNullableEnum` (`anthropic_strict_schema_issue567_test.go:628`)
— flip its assertions from "union preserved with `null`" to the new correct Gemini shape
(`output_mode.type == "STRING"`, `nullable == true`, enum preserved, no `null`), and rewrite the
stale doc comment (`:629-632`) to describe the collapse. Without this edit `go test ./...` is red
at the gate.

---

## Open questions

1. **Session-reopen across a mid-loop tool call.** `ThoughtSignature` is `json:"-"`, so a session
   reopened *between* a tool call and the model's reply loses the signature and could 400 on
   resume — exactly the same limitation the existing Anthropic `ThinkingSignature` (`json:"-"`)
   already has. I am matching that precedent (consistent, no transcript-format change, satisfies
   the in-memory acceptance test). If we want resume-safety, the alternative is a persisted-but-
   stripped-on-send field (like `Reasoning`), which is more invasive. **Proceeding with
   `json:"-"` unless told otherwise.**
2. **Multi-non-null unions** (e.g. `{"type":["string","integer"]}` with no `null`) are not in any
   default tool schema and remain unrepresentable in the scalar Gemini proto. I will collapse the
   nullable case and leave a true multi-type union as the trimmed array with an explanatory
   comment, rather than expanding scope to `anyOf`. Flagging in case a future tool needs it.
3. **Signatures on thinking/text parts.** Gemini can attach `thoughtSignature` to non-functionCall
   parts too, but #573 only requires the functionCall round-trip (reasoning text is not replayed
   to Gemini in history). I am scoping capture/re-emit to the functionCall path only.
