# Issue #404 — stop volatile per-turn context from defeating prompt caching

## Problem
Volatile per-turn context (live git status + todo checklist) is appended to the
tail of the system prompt = `messages[0]`. Prefix caching matches from the start,
so a change there invalidates the whole transcript that follows. The direct
`anthropic` provider also emits no `cache_control` at all.

## Part A — reorder so volatile context is NOT in the cacheable prefix

Wire order goal: `[stable system][transcript...][small volatile tail]`.

1. `internal/gogent/syscontext.go` — `buildSystemContext(sessionID) (stable, volatile string)`:
   - **stable**: AGENTS.md instructions + repo map + skills index.
   - **volatile**: `## Git status` (vcs.StatusSummary) + todo checklist (RenderTodos).
2. `internal/agent/user_session.go`:
   - `systemContextFn func(sessionID string) (stable, volatile string)`, plus
     `SetSystemContextProvider` / `systemContext()` updated to the 2-value shape.
   - `refreshSystemPrompt`: `sess.SetSystemPrompt(base + stable)` and
     `sess.SetVolatileContext(volatile)`.
3. `internal/model/model_session.go`:
   - new field `VolatileContext string` + `SetVolatileContext(string)`.
   - in `sendCtx`, after appending `s.Transcript`, append a TRAILING
     `Message{Role: RoleUser, Content: VolatileContext, Volatile: true}` to the
     per-request `fullMessages` ONLY — never to the persisted `s.Transcript`
     (mirrors how the system prompt is kept out of the transcript).
4. `internal/model/connection.go` — `Message` gains `Volatile bool` (`json:"-"`),
   an internal per-request flag the Anthropic adapter reads to keep the cache
   breakpoint at the end of the cacheable prefix (it never persists or marshals).

### Adapter merge caveat
A trailing user volatile message merges cleanly into the prior user turn when the
last transcript message is a tool/function result (Anthropic `anthropicBlocks` /
Gemini `geminiParts` both map tool results to a `user` turn and merge same-role
messages). For Anthropic that yields `[tool_result..., text]` — tool_result stays
first, which the API requires.

## Part B — explicit caching on direct `anthropic`

`internal/model/adapter.go` `anthropicAdapter.buildBody`:
- Remove the `a.vertex` gate on the system block + last-turn `cache_control`.
- System is emitted as a one-element `[]anthropicSystemBlock` carrying a
  `cache_control{ephemeral}` breakpoint for BOTH direct + vertex (Anthropic
  accepts cache_control only on a system *block*, not a scalar string; the direct
  Messages API accepts the block-array `system` form).
- Last-turn breakpoint lands on the last block of the last NON-volatile message
  (end of the cacheable prefix), tracked during the message loop, so for
  vertex-anthropic it moves off the volatile tail onto the last transcript msg.
- `CacheReadInputTokens → CachedTokens` mapping (anthropicUsage.toTokenUsage)
  already present — unchanged.

## Docs
- `docs/providers.md` — correct the stable→volatile ordering claim (:172) and
  document direct-anthropic caching.

## Part C — DEFERRED (not implemented).

## Tests (partner writes them) — files that need updating
- `internal/model/vertex_anthropic_test.go` — invert
  `TestVertexAnthropicDirectAnthropicBodyUnchanged` (now asserts breakpoints
  PRESENT + block-array system); add CacheReadInputTokens→CachedTokens coverage.
- `internal/model/anthropic_test.go:78` — `got.System` is now a block array, not a
  scalar string (consequence of Part B).
- `internal/agent/todo_injection_timing_test.go:39,100` — provider signature is now
  `func(string) (string, string)`.
- new issue404 tests per the task brief.
