# Architecture

gogent is a hierarchical coding agent written in Go. A single process hosts one **Gogent** singleton that owns any number of **UserSessions**; each session owns a tree of **Agents** (a root agent plus spawned sub-agents). Agents drive a multi-turn ReAct loop against an OpenAI-compatible (or Anthropic-native) model endpoint, calling tools until they produce a final answer. A Turbo-Vision-style TUI (built on [turbotui](https://github.com/charmbracelet)) renders one window per session; an HTTP API exposes the same surface remotely.

This document describes the system architecture: package layering, the agent runtime, sub-agent fan-out, token/rate budgets, plan mode, compaction, the model layer, MCP, persistence, system context, diagnostics/audit, and checkpoints. For configuration see [configuration.md](configuration.md); for provider detail see [providers.md](providers.md); for the tool and permission model see [tools-and-permissions.md](tools-and-permissions.md); for the HTTP API see [api.md](api.md); for the terminal UI see [usage-tui.md](usage-tui.md).

## Layering

Dependencies flow strictly top → bottom. `cmd/main.go` is the entrypoint and only wires components together; it owns no business logic.

```
cmd/main.go   — flags, signal handling, wires components
  ├─► ui/tui (Workbench + SessionWindows)        — presentation/session UI
  │      uses: internal/agent, internal/gogent, internal/config, internal/clipboard, internal/notify, internal/stats
  ├─► internal/server (HTTP API)                    — remote control surface
  │      uses: internal/gogent, internal/agent
  └─► internal/gogent (Gogent singleton)           — composition root
         │  assembles & owns: model connection, permission service, tool registry, audit sink, stats, MCP clients, skill loader
         ├─► internal/agent (UserSession / Agent tree)  — orchestration: ReAct loop, tool-call dispatch, sub-agent spawn & interactive, plan mode, compaction gating
         │      ├─► internal/model (ModelConnection/adapter)  — LLM I/O: streaming, retries, provider adapters, token usage
         │      ├─► internal/tool (ToolRegistry)              — model-callable tools, each gated by internal/permission
         │      │      ├─► internal/fileops   (read/write/edit + checkpoints/undo)
         │      │      ├─► internal/vcs       (git tool + status)
         │      │      ├─► internal/diagnostics (compiler/linter tool)
         │      │      ├─► internal/verify     (test runner tool)
         │      │      ├─► internal/web        (HTML→Markdown for web fetch)
         │      │      ├─► internal/shell      (shell command + external-root guard)
         │      │      └─► internal/mcp         (external MCP server tools)
         │      ├─► internal/permission         — allow/deny + path containment
         │      ├─► internal/compression         — transcript summarization
         │      ├─► internal/diag (Audit)        — append-only security log
         │      └─► internal/skill               — loads skills/ SKILL.md files
         ├─► internal/config                   — config schema & loading
         ├─► internal/stats                     — usage aggregation (UI + API)
         ├─► internal/notify / internal/clipboard — UI-facing side channels
         ├─► internal/command                   — internal slash commands (/calc etc.)
         ├─► internal/mathexpr                  — shared arithmetic eval
         └─► internal/http                      — low-level HTTP client helper
```

**Key invariants:**

- `internal/gogent` is the sole composition root; `cmd/main.go` only wires it + the UI + the HTTP server.
- `internal/agent` is the orchestration layer and depends downward on `model`, `tool`, `permission`, `compression`, `diag`, and `skill` — never upward on `ui` or `server`.
- `ui/tui` and `internal/server` are parallel consumers of the gogent/agent layer; neither is required by the other.
- Leaf utility packages (`mathexpr`, `http`, `clipboard`, `notify`, `diff`, `stats`, `config`, `diag`) have no dependencies on the agent/tool layers.

## Annotated package map

Every internal package and its purpose:

- **internal/agent** — Core agent runtime: Agent tree, UserSession, ReAct loop, tool-call dispatch, sub-agent spawn/interactive, plan mode, compaction triggers.
- **internal/clipboard** — OSC 52 + native-utility clipboard copy (UI-agnostic); backs `Board.Copy`.
- **internal/command** — Internal slash-command registry + shell command runner (uses `mathexpr` for `/calc`).
- **internal/compression** — Conversation summarization: a stateless `CompressionAgent` turns older turns into a digest.
- **internal/config** — JSON config loading (models, permissions, notify, sub-agent execution model, etc.).
- **internal/diag** — Logging (`slog` Logger) + append-only Audit log for permission decisions & tool calls.
- **internal/diagnostics** — Runs the configured compiler/linter (`go vet ./...`), parses to structured `file:line:col` findings; backs the diagnostics tool.
- **internal/diff** — Stdlib-only unified-diff generator for write/edit previews.
- **internal/fileops** — File read/write/edit tools + per-turn Checkpoint snapshots for undo (FIFO bounded).
- **internal/gogent** — Top-level singleton `Gogent`: assembles model connection, permission service, tool registry, audit, stats, MCP clients; creates UserSessions.
- **internal/http** — Minimal HTTP client helper (baseURL/timeout, JSON get/post).
- **internal/mathexpr** — Safe recursive-descent arithmetic evaluator (`+,-,*,/,`, parens, float64); shared by the `/calc` command & the calc tool.
- **internal/mcp** — Model Context Protocol client: JSON-RPC over streamable-HTTP & stdio transports; `initialize`/`ListTools`/`CallTool`.
- **internal/model** — LLM connector: OpenAI-shaped request/response types, provider adapters, streaming, retries/backoff, tool-calls, token usage & stats.
- **internal/notify** — Desktop/terminal notifications (bell, OSC 9/777, native notifier) gated by Reason + config toggles.
- **internal/permission** — Path-aware permission service: allow/deny rules, a path-containment predicate, and external/read/write/diagnostics actions.
- **internal/server** — HTTP API server: webapi handlers over Gogent sessions, auth, streaming, session management.
- **internal/shell** — Best-effort scanner for filesystem paths in shell commands that escape the workspace (guardrail, not sandbox).
- **internal/skill** — Skill loader: parses YAML frontmatter (`name`/`description`) from `SKILL.md` files into callable skills.
- **internal/stats** — Usage `Report`/`ModelStat`/`ConnectorStat` aggregation; per-model lookup for the Overall stats panel.
- **internal/tool** — `ToolRegistry`: registers all model-callable tools (read/write/edit/shell/git/diagnostics/verify/web/...), with permission gating per tool.
- **internal/vcs** — Safe git wrapper: explicit arg vectors, timeout, no pager/prompts; backs the native git tool + system-prompt git status.
- **internal/verify** — Runs the configured test command (`go test ./...`), parses to structured per-test/per-package failures; backs the verify tool.
- **internal/web** — Dependency-free HTML→Markdown extractor (title + body) for the web-fetch tool.

Top-level non-internal packages:

- **ui/tui** — Multi-session terminal UI: Workbench desktop, menu bar, draggable SessionWindows, foldable transcript, model selector, dialogs, `@`-file mentions, clipboard/notify hooks.
- **cmd/** — Main package: entrypoint; parses flags, wires the Gogent singleton + Workbench TUI + HTTP server, signal handling.
- **skills/** — Built-in agent skills (`SKILL.md` frontmatter, no Go): `code-review`, `debugging`, `git-commit`, `parallel-research`.

## The ReAct loop

`runLoop` (`internal/agent/user_session.go`) is the shared multi-turn tool-calling loop for **both** the root task loop and sub-agents.

**Per-loop setup.** Each loop scopes a cancellable `context.Context`, published onto `agent.cancel` so `StopAgent` or session-close aborts in-flight work. Before every round-trip `refreshSystemPrompt` re-evaluates `systemContext`, splitting it into a **stable** bucket (AGENTS.md instructions, repo map, skills index) installed on the system prompt and a **volatile** bucket (live git status + todo checklist) threaded through `SetVolatileContext` as a trailing per-request message after the transcript. Both reflect the latest state and survive compaction; the split keeps the volatile content out of the cacheable prefix so editing a file no longer invalidates the cached transcript (issue #404).

**Loop body, in order:**

1. Check `ctx.Err()` (cancellation).
2. Check `BudgetExceeded` via `stopForBudget`.
3. `collectToolCalls(resp)`.
4. If no tool calls: either preamble-nudge **or** break (final answer).
5. Otherwise execute the tool calls (concurrent or serial), append tool results, splice any queued user note at the turn boundary, and advance.

**Tool-call collection.** `collectToolCalls` prefers native `resp.ToolCalls`; it falls back to extracting JSON objects from assistant text for small/local models without native tool-calling. A `{"response":...,"final":true}` JSON object is an explicit final — it must terminate immediately and is never preamble-nudged.

**Tool execution fast paths:**

- `allSpawnSubAgent` (≥2 `spawn_subagent` calls) → `runToolCallsConcurrent` with `RunSubAgentsBounded`.
- `allReadOnly` (≥2 read-only tools) → `runToolCallsConcurrent` with `runBoundedTools` (semaphore, max 8).
- Otherwise → `runToolCallsSerial` (writes/shell/mixed/unknown; preserves order).

**Resilience.**

- *Panic containment:* a loop-wide `recover` turns any panic into a `SessionEventError`; `runAndEmitResult` also recovers per-tool.
- *Event pairing:* `toolEventID` pairs `ToolCall`↔`ToolResult` events.
- *Preamble recovery:* a tool-free turn that looks like a preamble gets **one** continuation nudge (`maxContinuationNudges=1`; reset on a real tool call).
- *Empty-final recovery:* a terminal turn with empty content surfaces the last non-empty assistant text.

Only the root agent emits thinking/tool events into the session window; sub-agent loops are silent.

## Sub-agents

Two execution models:

- **One-shot** (`SpawnSubAgent`): blocking; the child must end in `SUCCESS:` / `FAILURE:`.
- **Interactive** (`LaunchInteractiveAgent`): async, fire-and-forget; returns an `agent_id` immediately and may return `CLARIFY`.

Three coordinator prompt shapes exist: one-shot only, interactive only, and both (the default).

**Sub-agent primer.** Before launching a child, the parent builds a bounded context primer (≤20 paths, ≤8 searches, ≤1500 bytes) from its own already-gathered read/edit/list/grep/glob calls, so the child does not re-discover the same files.

## Fan-out, depth & concurrency limits

`SubAgentConfig` governs recursion:

- **MaxSubAgents** (default 4) per parent — only *non-terminal* children count.
- **MaxDepth** (default 3).
- **MaxConcurrent** (default 8) — global concurrent sub-agents.
- **AllowRecursive** (default false) — when false, the child gets a registry cloned *without* `spawn`/`coordinate` tools.

Structural limits compose multiplicatively (`MaxSubAgents^MaxDepth`), so `SubAgentLimiter` (a counting semaphore) is the global cap. It uses a **tryAcquire-or-run-inline** pattern: a spawn that cannot grab a slot runs inline as backpressure, which is deadlock-free under recursion. Interactive launches also count against the *same* limiter; a failed `tryAcquire` **rejects** the launch and the coordinator retries. For tool calls, `runBoundedTools` caps independent read-only calls at 8.

## Token budgets & rate limiting

- **Token budget.** `Agent.TokenBudget` is cumulative prompt+completion; `AddTokensUsed` accumulates per round-trip. When `BudgetExceeded()` is true, `stopForBudget` folds a `BUDGET_EXCEEDED` notice (preserving partial progress) and breaks the loop. `subAgentOutcome` classifies `BUDGET_EXCEEDED` as `StatusFailed`.
- **Step cap.** Per-task `maxSteps` (`DefaultMaxSteps=100`; `≤0` = unlimited), shared by the root agent and all sub-agent/interactive loops. On a cap exit whose final round-trip still carries unexecuted tool calls, `stopForStepLimit` folds a visible `STEP_LIMIT_REACHED` notice (preserving partial progress) and `finalizeTranscriptToolCalls` balances the orphaned tool calls so the persisted transcript stays valid for resume (issue #449).
- **Rate limiter.** A token bucket that refills continuously at a rate-per-second with `burst=capacity`. `Wait` blocks until a permit is available or the context is cancelled. It is process-wide and shared across sessions; `modelRoundTrip` calls `waitRateLimit` before every send.

## Plan mode

`SetPlanMode` causes the next root-agent turn to run against a **write-free** tool set (`CloneForPlanMode` keeps read-only tools + `todo` + `structured_output` + `spawn_subagent` for read-only delegation). `planModeSystemPromptWith` adds read-only planning instructions. `recordPlan` captures the final answer as a `pendingPlan` and emits `SessionEventPlan` (approval-gated). `ExecuteApprovedPlan` re-runs with the full tool set once approved.

## Todo tool

`TodoItem{Content, Status (pending|in_progress|completed), Note}`. `SetTodos` replaces the list and emits `SessionEventTodo`. `RenderTodos` produces a compact markdown checklist that is injected as the **recurring volatile per-request message** (the trailing message appended after the transcript, alongside the live git status) — it survives compaction because it is rebuilt every loop and is excluded from the compactable transcript. It is kept out of the cacheable system prompt so per-turn checklist updates do not invalidate the cached prefix (issue #404).

## Compaction

Triggered before each round-trip when `sess.NeedsCompression()` (threshold-based, with hysteresis; disabled without a context window). `compression.SafeSplit` partitions the transcript into an older portion (to summarize) and a recent portion (kept verbatim, `DefaultKeepRecentTurns=3`), never splitting a tool-call from its results and splitting at a user-message boundary. `Summarize` runs on a **stateless** completion — the configured fast/compression completer when set, else the session's primary model — so it never pollutes the live transcript. The digest is spliced back as a user message. On failure the transcript is left untouched.

## The model layer

`internal/model` is split into small capability interfaces (`Completer`, streaming, tools, stats) so it can later be extracted into its own library.

**Provider abstraction.** `provider.go` (`APIType`) maps each backend to its endpoint layout *and* a wire-format adapter (`adapter.go`). OpenAI-compatible providers (`openai`, `zai`, `openrouter`) share one adapter; Anthropic (`anthropic`/`claude`) speaks the native Messages protocol through its own adapter.

**Portable tool schemas.** `tool.NormalizeSchema` guarantees an object root and strips keywords strict providers reject; `tool_choice` is a typed `ToolChoice` each adapter serializes.

**Structured output.** `CompleteStructuredCtx` takes a `ResponseFormat`. OpenAI-compatible providers get the real `json_schema` constraint; Anthropic drops it and relies on strict tools + `tool_choice` forcing.

**Live reconfiguration.** Connections are rebuilt per send, so model/endpoint edits take effect on the next turn. See [providers.md](providers.md) for full provider detail.

## MCP client

`internal/mcp` is a stdlib-only Model Context Protocol client speaking JSON-RPC 2.0 over two transports: a launched stdio subprocess and streamable-HTTP. Servers are declared in the `mcp_servers` config. At startup each server is dialed (gated through `ActionMCP`), its `tools/list` is wrapped, and every remote tool is registered under `mcp__<server>__<tool>`. A denied, disabled, or unreachable server is skipped with a warning rather than blocking startup.

## Persistence

`internal/gogent/session_store.go` implements sharded JSONL persistence.

- **Layout.** A live session occupies a base prefix `<iso>_<id>_session` with `<base>.index` (meta + shard table; the source of truth; written last via temp+rename) plus `<base>.0000.jsonl`, `.0001.jsonl`, … shards. Shards roll at 5000 records **or** 10 MiB.
- **Index.** Holds `SessionID`, `Title`, `CreatedAt`, a summary (`Turns`/`TokensIn`/`TokensOut`/`Model`), and an ordered `Shards` table.
- **Listing.** `ListSessions` reads only the tiny index files, so listing is O(sessions).
- **Saving.** After the first save, `Save` appends only **new** messages (delta); a full atomic rewrite is reserved for the first save or compaction (`TranscriptEpoch` advanced). A title change only rewrites the index.
- **Restore.** `ListActive` reads every live index plus the *current* shard only; `Adopt` re-attaches a restored session to its on-disk shards.
- **Archive.** Renames the base from `_session` to `_session_archived`.
- **Durability.** Data writes are synchronous; `fsync` is batched off the critical path (debounced 250ms). Shard cache is LRU, cap 16. Non-archived sessions are restored on startup (crash recovery).

## System context (AGENTS.md + repo map + skills)

Built by `internal/gogent/syscontext.go` and re-evaluated each loop.

- **AGENTS.md.** Discovered by walking from the workspace root up to the filesystem root (outermost first, nearest last) plus the global `~/.gogent/AGENTS.md`, concatenated with source headers, size-capped at 32 KB.
- **Repo map** (`repomap.go`). Walks workspace Go files (cap 2000 files), parses each, extracts top-level declarations, ranks Aider-style by inbound-reference count, size-capped at 16 KB. Go-only today.
- **Skills.** Progressive disclosure — only `name`+`description` are injected; the `skill` tool loads the full `SKILL.md` on demand. Skills load from `~/.gogent/skills` and `./skills`. The trust boundary is enforced: symlinks are not followed, path-containment is verified, depth is limited to 16, and file size to 1 MiB.
- **Git status.** A live `git status --short --branch` is injected each loop when the workspace is a repo.

## Diagnostics, logging & audit

`internal/diag` provides:

- **Logger** — wraps `log/slog` (structured: timestamp, level, typed key/value). In TUI mode diagnostics append to `~/.gogent/gogent.log` (a file, so they cannot corrupt the alternate screen); headless mode goes to stderr.
- **Audit** — a separate append-only stream for security-relevant events: every resolved permission decision and every tool call, written to `~/.gogent/audit.log` in both modes. The permission `Service` emits decisions via an `AuditSink`; the per-session tool callback emits tool calls (arguments omitted, since they may carry secrets).
- **`diag.Secret`** — wraps API keys/tokens so they redact to `[REDACTED]`.

## Checkpoints

In-memory, per-session shadow copies of files touched by `write`/`edit` (`internal/fileops/checkpoint.go`). The active turn's checkpoint accumulates one snapshot per file (first mutation wins) and is committed at end of turn. `/undo` reverts the last turn; `/rewind [n]` reverts several (oldest snapshot wins per file). Bounded to 100 turns (FIFO). This is a safety net for the running process only — restarting clears checkpoints, and shell-driven changes are not captured.

## Entry points

- **Default:** interactive TUI (`./gogent`).
- **Headless HTTP server mode:** `./gogent -no-tui` for API-only use.
- **`-verbose`:** extra startup/diagnostic logging.

## Known gaps

The permission gate authorizes every side-effecting tool: workspace file ops are allowed; shell and any out-of-workspace access prompt interactively (*Allow once* / *Always* / *Deny*), with *Always* persisted to `~/.gogent/permissions.json`. The shell external-path check is a best-effort guardrail, **not** a sandbox; OS-level sandboxing is still future work.
