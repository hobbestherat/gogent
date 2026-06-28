# Design — #567: Anthropic strict-tool nullable-union + `enum` 400

Branch: `pair2/anthropic-strict-tool-nullable-enum-400`
Scope: **gogent-only**, stdlib-first, **no new deps, no go.mod bump**. Provider-scoped to
`api_type` **anthropic** and **vertex-anthropic** only.

## Problem (root cause)

Anthropic strict-tool schema validation rejects any property that combines a **nullable
union type** (`"type": ["string","null"]`) with an **`enum`** array. The Messages API returns:

```
HTTP 400 invalid_request_error
tools.N.custom: Invalid schema: Enum value 'content' does not match declared type '["string","null"]'
```

The request is rejected at the tool-definition layer (`strict:true`) *before* the model runs,
so **every first turn to a `claude-*` model dies** — the default model is unusable.

The only core tool that currently trips it is **grep**, whose `output_mode` property
(`internal/gogent/gogent.go:692`) is `{"type":["string","null"], "enum":[...]}`. But the
Anthropic adapter (`internal/model/adapter.go`, `anthropicAdapter.buildBody`, line ~446)
assigns `InputSchema: schema` **verbatim** — no deep copy, no normalization — so the same
bug recurs for **any** strict tool with this pattern, including user-supplied **MCP tools**.
The Gemini adapter already has a deep-copy normalizer (`geminiSchema` / `uppercaseSchemaTypes`,
adapter.go:1211-1256); the Anthropic path has no equivalent.

### Why the union exists (and why a source-only fix is wrong)

Every gogent strict tool lists **all** properties in `required` and expresses "optional" via a
`["T","null"]` union — the OpenAI structured-outputs idiom (OpenAI strict mandates that every
property appear in `required`; nullability is the union, not omission). Verified across all four
strict tools (read, grep, glob, list — adapter source). gogent leans on this idiom on purpose.

Patterns verified by curl (from the issue) — only nullable-union **+** enum fails on Anthropic:

| schema | Anthropic strict |
|---|---|
| `["string","null"]` + `enum` | **400** ← grep.output_mode |
| `["string","null"]` no enum | 200 |
| `["integer\|boolean\|array","null"]` (+items) no enum | 200 |
| plain `"string"` + `enum` | **200** |

The error text ("enum value 'content' does not match `["string","null"]`") shows Anthropic's
validator simply **does not support union-type + enum** — it cannot match a scalar enum member
against a union. So adding `null` *into* the enum does not help (a `null` enum member can't match
a `"string"` type either, and re-introducing the union re-triggers the original reject). The only
Anthropic-strict-valid shape that keeps the enum constraint is **plain `"string"` + `enum`**.

## Chosen fix — adapter-level normalizer (PRIMARY only)

Add a deep-copy JSON-Schema normalizer on the Anthropic/Vertex-Anthropic path, mirroring the
existing `geminiSchema` pattern. **Decline the optional source-level grep change** (justified below).

### Files / functions touched (gogent)

- `internal/model/adapter.go`
  - **New** `anthropicSchema(v interface{}) interface{}` — deep-copies `v` via a JSON round-trip
    (`json.Marshal` → `json.Unmarshal` into `interface{}`), exactly like `geminiSchema`, so it
    never mutates the caller's shared `Parameters` map (concurrent-reuse safe). Returns `nil` for
    `nil`, and falls back to the original value if it is not JSON-encodable. Then walks the copy
    with `normalizeAnthropicSchemaTypes`.
  - **New** `normalizeAnthropicSchemaTypes(v interface{}) interface{}` — recursive walk:
    - On a `map[string]interface{}` node: if the node has an `"enum"` key **and** its `"type"`
      is a `[]interface{}` (union) **containing `"null"`**, rewrite `"type"` by dropping every
      `"null"` member. Collapse to the **single scalar string** when exactly one non-null member
      remains (the grep case → `"string"`); keep it an array if >1 non-null members remain;
      leave the original untouched if 0 remain (degenerate `["null"]`+enum — never happens in
      practice, defensive). `"enum"`, `"required"`, and all sibling keys are left untouched.
    - Recurse into **all** child values so nested tool schemas are covered: object `properties`
      (each property), `items` (object or array form), and the combinators `anyOf` / `allOf` /
      `oneOf` / `not`, plus `$defs` / `definitions`. Implemented generically by recursing into
      every map value and every array element (same shape as `uppercaseSchemaTypes`), so the rule
      applies wherever it appears — no key whitelist needed.
  - **Wire it in** `anthropicAdapter.buildBody`, the `for _, t := range req.Tools` loop
    (adapter.go ~437-449): after the existing `if schema == nil { … }` default, set
    `schema = anthropicSchema(schema)` before `InputSchema: schema`. Applies on **both** the
    direct (`a.vertex == false`) and Vertex (`a.vertex == true`) paths — the same adapter serves
    both, so both are fixed by one wiring point. No other adapter calls this; OpenAI/Z.AI/
    OpenRouter (`openAIAdapter.buildBody` = `encodeJSON(buf, req)`) and Gemini paths are untouched.

### Result for grep (representative)

`output_mode` goes out to Anthropic as `{"type":"string","enum":["content","files_with_matches",
"count"],"description":…}` — a verified-200 shape. It **stays in `required`** (preserving the
all-required strict invariant), so the model now always passes one of the three modes. grep
keeps `Strict:true`; the execute handler is unchanged and remains tolerant (missing/empty/null →
default `content` via `stringArg`). grep's other nullable fields (`path`, `include`,
`case_insensitive`, `max_results`) have **no enum**, so the normalizer leaves them exactly as-is
(they already return 200).

### Why NOT also patch the grep source (the optional SECONDARY fix) — rejected

The source schema is shared across **all** providers (OpenAI/Z.AI/OpenRouter send it verbatim).
Any source edit that satisfies Anthropic regresses OpenAI:
- Collapse to `"string"`+enum at the source → OpenAI model can no longer send `null`/omit
  output_mode (a needless cross-provider behavior change on a path that works today).
- Drop `output_mode` from `required` → violates OpenAI strict's **all-properties-required** rule
  → 400 on OpenAI.

The bug is provider-specific (only Anthropic's validator rejects union+enum), so the fix belongs
in the provider-scoped adapter, not in the shared source. The generic normalizer also covers MCP
and any future strict tool automatically — a source edit would not. Keeping the source as-is
means OpenAI continues to accept `null`/omit on output_mode; only the Anthropic wire tightens it
to a required enum (invisible to the user, handler-tolerant either way).

## User-facing behavior

- **Before:** every turn to a direct-Anthropic or Vertex-Anthropic `claude-*` model fails on the
  first message with HTTP 400 — the default model is dead on arrival.
- **After:** `claude-*` turns succeed (HTTP 200) on the default tool set. grep is fully functional;
  the model selects an explicit `output_mode` (content/files_with_matches/count). No visible change
  on OpenAI/Z.AI/OpenRouter/Gemini. No new flags, prompts, or config — fixed out of the box.

## The four design gates

**(1) Goal match.** Exactly the issue's ask: a FIX that stops the Anthropic/Vertex-Anthropic
strict path from emitting nullable-union+enum, so `claude-*` turns return 200. grep stays
`Strict:true` and functional. No feature, no refactor, no scope creep. Implemented as the
issue's PRIMARY recommendation (adapter normalizer); the optional source fix is deliberately
declined with a documented cross-provider rationale.

**(2) Usability.** Default model usable out of the box with no user action. The interaction the
user actually drives (a turn to Claude) now succeeds instead of erroring. The fix is generic, so
user-supplied MCP strict tools with the same pattern are silently corrected too rather than
failing mid-session. Nothing the user must configure; nothing surfaced that should be silent.

**(3) No regressions.** Deep copy (JSON round-trip) means the caller's shared `Parameters` map is
never mutated — concurrent reuse across turns/providers stays safe. Only the Anthropic/Vertex path
calls the normalizer; OpenAI/Z.AI/OpenRouter (`encodeJSON` pass-through) and Gemini (`geminiSchema`)
are byte-identical to before. Non-enum nullable unions (path/include/etc.) are untouched, so the
existing 200 behavior for those is preserved. The grep execute handler and its external contract
(accepts the three modes; tolerates missing/null → default content) are unchanged. Existing tests
in `internal/model` and the session/transcript invariants are unaffected (no transcript, message,
or cache-breakpoint code is touched). gofmt/build/vet/golangci-lint expected clean; `go test ./...`
expected green (pre-existing `TestUserSessionSendMessage` 404 and the load-flaky
`internal/daemon TestStopGracefulAndForced` are the only tolerated exceptions per the issue).

**(4) Holistic across both repos.** The change lives entirely in gogent's model/adapter layer,
the correct home for provider-specific wire normalization (it sits beside the analogous Gemini
normalizer). **turbotui has zero stake**: a grep of `$HOME/work/turbotui` for
`input_schema`/`InputSchema`/`output_mode`/`buildBody`/`anthropicTool` returns nothing — turbotui
renders UI and never constructs or inspects tool JSON schemas. The gogent↔turbotui seam (tool
schemas are owned by gogent, the provider wire is internal to gogent) is respected; no turbotui
change is needed or warranted, and there are no downstream effects on it. Mirrors the existing
Gemini deep-copy style; no new deps; no go.mod bump.

## Tests (added in `internal/model`, new `anthropic_strict_schema_issue567_test.go`)

All unit-level (no live endpoint on the Pi5 — none required by the issue):

1. **grep schema through buildBody.** Build a `CompletionRequest` carrying the real grep tool
   schema (strict), run `anthropicAdapter.buildBody`, unmarshal the body, and assert the emitted
   `output_mode` `type` is the scalar `"string"` (no `"null"`) while `enum` is intact — i.e. no
   property anywhere combines a null-bearing union `type` with an `enum`. Assert `strict:true`
   survives and `output_mode` is still in `required`. Run for **both** `vertex=false` and
   `vertex=true` adapters.
2. **Generic helper assertion** (`anthropicSchema` directly): a synthetic node with
   `{"type":["string","null"],"enum":[...]}` → `type=="string"`, enum preserved.
3. **MCP-style regression:** a synthetic strict tool whose nullable-union+enum property is nested
   under `properties` → `items` → `properties` (deep). Assert the deep property is normalized,
   proving recursion covers MCP/future tools.
4. **Non-enum nullable left alone:** a `["string","null"]` property with no enum is emitted
   unchanged (union preserved) — guards against over-normalizing the 200-path fields.
5. **No shared-map mutation:** keep a reference to the original `Parameters` map, run buildBody,
   assert the original still contains the union `["string","null"]` for `output_mode` (deep copy
   proven), so the registry's shared schema is intact for the next/other-provider turn.

A small helper walks the decoded schema and fails if any node has both a null-bearing union
`type` and an `enum` — the precise "would Anthropic strict reject this" predicate (acceptance #1).

## Gate / rollout

Rebase onto current `origin/main` at the gate. gogent-only, conflict-free with the TUI/server
work on #562 (disjoint files: model/adapter.go vs ui/tui + internal/server). PR body: `Closes #567`.

## Open questions

1. **`anyOf` alternative.** A theoretically cleaner null-preserving shape is
   `{"anyOf":[{"type":"string","enum":[...]},{"type":"null"}]}`, but it is **not** curl-verified
   against Anthropic strict and may itself be rejected. The chosen collapse-to-`"string"` shape
   **is** verified (200). Decision: ship the verified shape; revisit anyOf only if a future tool
   genuinely needs a wire-level nullable enum (none does today — handler tolerance covers it).
2. **All-required assumption.** The design assumes Anthropic strict (like OpenAI strict) mandates
   every property be in `required`, which is why the normalizer keeps `output_mode` required
   rather than dropping it to re-allow omission. This is inferred from gogent's uniform
   all-required strict tools + the union-null idiom; not independently curl-verified for Anthropic.
   It is the conservative choice (keeping a field required can't newly break a request, whereas
   dropping it from `required` could). If Anthropic does **not** require all-required, the fix is
   still correct — just slightly stricter than necessary on `output_mode`.
