# Issue #217 — Streaming thinking (live, then folded)

## Goal
Add an opt-in option to display the model's chain-of-thought (reasoning) tokens
**live** in the transcript as they stream, then **fold** (collapse) the thinking
entry once the turn's thinking completes. No-op when the model doesn't stream
reasoning. Off by default; gated like the other experimental features.

## Current state
- Agent loop (`runLoop` → `modelRoundTrip` → `ModelSession.SendWithToolsCtx` →
  `ModelConnection.CompleteWithToolsCtx`) is fully **blocking** — no streaming.
- A separate streaming path exists (`CompleteStream` / `completeStream` /
  `parseOpenAIStream`) but isn't used by the loop and ignores reasoning deltas.
- Reasoning for GLM (Z.AI, OpenAI-compatible) arrives in streamed deltas as
  `reasoning_content` (OpenRouter: `reasoning`); Anthropic as `thinking_delta`.
- The transcript already renders foldable "thought" entries (`kindThinking`,
  collapsed) and supports in-place append (`appendLine`) and fold
  (`setCollapsed`).

## Design — a streaming-thinking event through agent→UI observer path

### 1. Model layer (`internal/model`)
- `StreamResponse`: add `Reasoning string` (incremental reasoning delta).
- `streamDelta`: parse `reasoning_content` and `reasoning`. `parseOpenAIStream`
  emits `StreamResponse{Reasoning: …}` for each. Anthropic adapter: parse
  `thinking_delta` likewise. (No behaviour change for content/tool-call paths.)
- New `ReasoningSink func(delta string)` type.
- New optional interface `StreamingToolCompleter` with
  `CompleteWithToolsStreamCtx(ctx, messages, tools, onReasoning) (*CompletionResponse, error)`.
  Implemented on `*ModelConnection`: drives `completeStream`, forwards reasoning
  deltas to the sink, and assembles a `*CompletionResponse` (content + tool calls
  + usage) identical to the blocking path. Backends that don't stream reasoning
  simply never call the sink (no-op).
- `ModelSession`: refactor `SendWithToolsCtx` onto a shared `sendCtx` core; add
  `SendWithToolsStreamCtx(ctx, messages, tools, onReasoning)`. When a sink is
  given AND the backend is a `StreamingToolCompleter`, it streams; otherwise it
  uses the existing blocking call. Same transcript/history/token bookkeeping.

### 2. Agent layer (`internal/agent/user_session.go`)
- New event types `SessionEventThinkingDelta` (Text = chunk) and
  `SessionEventThinkingDone` (fold signal).
- Session flag `streamThinking` + `SetStreamThinking`/`StreamThinking`.
- `modelRoundTrip` takes an `onReasoning model.ReasoningSink`; nil → blocking
  path unchanged. `runLoop` builds the sink (only root agent, only when enabled),
  emits `ThinkingDelta` per reasoning chunk, and `ThinkingDone` after each
  round-trip so the live entry folds when the turn's thinking is over.

### 3. Config (`internal/config`)
- `ExperimentalConfig.StreamThinking bool` (`json:"stream_thinking"`), off by
  default. Wired in `Gogent.CreateUserSession` via `SetStreamThinking`.

### 4. UI (`ui/tui`)
- `SessionWindow`: `liveThought *transcriptRecord` + `liveThoughtBuf` line buffer.
  `apply` handles the two new events: lazily create an expanded `kindThinking`
  record, append completed lines live, and on done (or busy→idle safety net)
  flush + relabel "thought" + collapse.
- `/thinking [on|off]` command toggles it live via a new `StreamThinking` handler
  hook wired in `cmd/main.go` to the session's `SetStreamThinking`.

## Constraints
No new deps; gofmt; golangci-lint 0; tests run without `-race`. Tests written by
GLM partner — not here.
