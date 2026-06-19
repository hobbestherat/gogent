# gogent — System Design Summary

A compact overview of how gogent is put together. For day-to-day usage see
`README.md`; for outstanding work see `TODO.md`.

## Overview

gogent is a hierarchical coding agent in Go. A process hosts one `Gogent`
singleton that owns any number of `UserSession`s; each session owns a tree of
`Agent`s (a root plus spawned sub-agents). Agents drive a multi-turn ReAct loop
against an OpenAI-compatible model endpoint, calling tools (file read/write/edit,
shell, sub-agent spawn) until they produce a final answer. A Turbo-Vision-style
TUI (built on `turbotui`) renders one window per session.

## Layers

```
cmd/main.go            wiring + flags (TUI / HTTP / verbose), skill loading
ui/tui/                Workbench desktop, per-session windows, sidebar, dialogs
internal/gogent/       Gogent singleton: sessions, tool registry, persistence
internal/agent/        UserSession + Agent tree, ReAct task loop, sub-agents
internal/model/        Connector interfaces + HTTP model connection + session
internal/tool/         Tool registry, shell/git/web tools, structured-output tool
internal/vcs/          Thin, safe git wrapper (backs the git tool + git-status context)
internal/diagnostics/  Runs the project compiler/linter; parses file:line:col errors
internal/fileops/      Path resolution, file I/O, file mutation
internal/command/      Shell exec + internal commands (calc/echo/help)
internal/skill/        SKILL.md loader + registry (wired via syscontext + skill tool)
internal/permission/   Resource+action permission gate (shell/file/external/...)
internal/compression/  Context compression
internal/http/         HTTP client; cmd also has a headless HTTP server mode
internal/notify/       Desktop/terminal notifications (bell + OSC 9/777 + native)
internal/clipboard/    System clipboard (OSC 52 + native pbcopy/xclip/wl-copy)
```

## Core components

- **Gogent** (`internal/gogent`) — UI-agnostic singleton. Owns sessions, the tool
  registry, config, and JSONL session persistence. Exposes the API the UI and
  HTTP server call into.
- **UserSession / Agent** (`internal/agent`) — an agent tree per session. The
  task loop uses native OpenAI tool-calling when available and falls back to
  parsing a JSON tool call out of assistant text. Emits a typed `SessionEvent`
  stream (`thinking`, `assistant_step`, `tool_call`, `tool_result`, `final`,
  `error`) so the UI renders incrementally. Only root-agent events surface in the
  main chat; sub-agent detail is kept for the monologue popup. When a turn
  returns several independent tool calls, two fast-paths run them concurrently to
  cut wall-clock latency: an all-`spawn_subagent` batch (bounded by the
  `SubAgentLimiter`), and an all-read-only batch (`read`/`grep`/`glob`/`list`/
  `calc`/`web_fetch`, bounded by a fixed tool semaphore, default 8). Read-only is
  a per-tool property (`Tool.ReadOnly`); any write/shell, mixed, or unknown (e.g.
  MCP) tool in the batch keeps the whole turn on the serial path so side effects
  keep their requested order. Either way results are reassembled in call order
  before being fed back (issue #50).
- **Sub-agents** — two execution models: *one-shot* (blocking, must end in
  `SUCCESS:`/`FAILURE:`) and *interactive* (async, may return `CLARIFY`). Mode,
  recursion depth, and fan-out limits are configurable. Batched
  `spawn_subagent` calls run concurrently, but a process-wide `SubAgentLimiter`
  (`max_concurrent`, default 8) caps how many sub-agent loops run at once so the
  multiplicative fan-out (`max_subagents ^ max_depth`) cannot spawn an unbounded
  goroutine herd against the backend; overflow tasks run inline as backpressure.
  Cost is governed two further ways (issue #28): each sub-agent gets a per-agent
  token budget (`sub_agents.token_budget`, opt-in) and stops gracefully with a
  `BUDGET_EXCEEDED` result once it is reached, instead of looping to the step cap
  with no token ceiling; and a process-wide token-bucket `RateLimiter`
  (`rate_limit.requests_per_minute`/`burst`, opt-in) paces every model round-trip
  so a wide fan-out — or several cluster nodes — cannot stampede a provider into
  429s. Both default off, preserving prior behavior.
- **Model layer** (`internal/model`) — split into small capability interfaces
  (`Completer`, streaming, tools, stats) so it can later be extracted into its
  own library. A provider abstraction (`provider.go`, `APIType`) maps each
  configured backend to its endpoint layout *and* a wire-format adapter
  (`adapter.go`): the request/response/stream translation. OpenAI-compatible
  providers (generic servers `openai`, the Z.AI platform `zai`) share one
  adapter and differ only in endpoint layout; Anthropic (`anthropic`/`claude`)
  speaks the genuinely different Messages protocol (`POST /v1/messages`,
  `x-api-key` + `anthropic-version`, a top-level system prompt, content-block
  message arrays and `input_schema`/`tool_use`/`tool_result` tools) through its
  own adapter that translates to and from gogent's OpenAI-shaped internal types.
  Tool schemas are kept portable so one definition serves every provider:
  `tool.NormalizeSchema` (applied once at `Register`) guarantees an object root
  with a `properties` map and strips keywords strict providers reject (`$ref`,
  `default`, `allOf`, …); `tool_choice` is a typed `ToolChoice`
  (auto/none/required/force-a-tool) each adapter serializes to its own encoding
  (OpenAI string-or-object, Anthropic `{type:…}`). A bare base URL is normalized
  into the concrete endpoints. Connections are rebuilt per send, so
  model/endpoint edits take effect on the next turn.
- **Tools** (`internal/tool`, `internal/fileops`, `internal/web`, `internal/vcs`,
  `internal/diagnostics`) —
  `read`, `write`, `edit`, `grep`, `glob`, `list`, `shell`, `web_fetch`, `git`,
  `diagnostics`, `spawn_subagent`, agent-control tools, and `structured_output`.
  File ops resolve
  paths against the workspace root (the launch cwd) and run through a keyed-mutex
  file mutator. `grep`/`glob`/`list` are read-only, workspace-confined search tools
  built on the `FileSystem` primitives: they run without a permission prompt
  (unlike the same searches routed through the shell) and `grep` returns `file:line`
  references the model can feed back into `read`.
  `web_fetch` downloads an http(s) URL and returns readability-style Markdown
  (size-capped, short-TTL cached, gated per domain via `ActionNetwork`); the
  HTML→Markdown reduction is a dependency-free, stdlib-only extractor
  (`internal/web`). `git` is a dispatched wrapper over the git binary
  (`status`/`diff`/`log`/`commit`/`create_branch`/`restore`) backed by
  `internal/vcs`, which runs git with explicit argument vectors (no shell, no
  injection surface) and disabled interactive prompts; mutating operations are
  gated via `ActionShell`, read-only ones run freely. `diagnostics` runs the
  project's compiler/linter and parses its output into structured
  file:line:column findings (default `go vet ./...`, configurable); it runs a
  fixed command pinned to the workspace but executes build-time code, so it is
  gated through a dedicated `ActionDiagnostics` (an *always* grant scopes to
  diagnostics alone, never the shell) and backed by `internal/diagnostics`.
- **MCP client** (`internal/mcp`) — a stdlib-only Model Context Protocol client
  speaking JSON-RPC 2.0 over two transports: a launched stdio subprocess and
  streamable-HTTP (plain JSON or SSE replies). Servers are declared in the
  `mcp_servers` section of `config.json`; at startup each is dialed (gated through
  `ActionMCP`), its `tools/list` is wrapped, and every remote tool is registered
  under an `mcp__<server>__<tool>` name so `ToolRegistry.ExecuteToolCall`
  dispatches to `tools/call` transparently. A denied, disabled or unreachable
  server is skipped with a warning rather than blocking startup.
- **TUI** (`ui/tui`) — `Workbench` desktop hosting draggable `SessionWindow`s,
  each with a foldable transcript (turbotv `TextView`), per-session model select,
  status line, a right-hand sidebar (session/sub-agent tree plus an Overall
  aggregate-stats panel), and Config / model
  / settings dialogs. The **Session** menu manages many windows: rename, pin
  (favorite, floats to the top with a ★), move up/down to reorder, and
  close-others / close-all. The desktop layout (order, titles, pin state, window
  bounds) is persisted to `~/.gogent/workbench_layout.json` and restored on the
  next launch. Each transcript is backed by an indexed model (`transcript_model.go`)
  that keys entries by event kind, so the **View** menu (and the focused-transcript
  keys `/`, `a`/`t`/`r`/`e`, `f`/`u`, `y`, `Esc`) can search, filter, fold and
  yank over the model and rebuild the view rather than scanning rendered cells.
  The yank actions copy the last answer (or its fenced code) to the system
  clipboard via `internal/clipboard` (OSC 52, SSH-safe, with a native
  `pbcopy`/`xclip`/`wl-copy` fallback); the **Session → Export Markdown/JSON**
  items render the full transcript — read from the same data the restored-session
  view uses — to a file under `~/.gogent`.
  next launch. A `notify.Notifier` watches the `SessionEvent` stream for terminal
  states (final, error, sub-agent clarify) and the permission-prompt path
  (approval) and emits a bell / OSC desktop notification / native notifier so a
  user can step away; per-event and per-channel toggles live under Config →
  Notifications. A permission prompt carries a `RequestContext` (the requesting
  session/agent) so the prompter badges that session's sidebar node with a ⏳
  marker — plus a global ⏳N count in the sidebar header — for the whole life of
  the prompt, including time spent queued behind another modal (issue #55). The
  dialog names the requesting session, and the badged node stays clickable so the
  user can jump straight to the session that is asking. Each window's status line shows a live usage readout (issue
  #63): state, elapsed + output throughput while a turn generates, cumulative
  tokens/turns, and a context-window gauge that turns amber near the compaction
  threshold and red at it; a `budget` block raises a token-budget alert. The
  gauge/elapsed refresh on every `SessionEventUsage` and once per second (a UI
  ticker that only redraws while some session is busy) via `refreshStatus`. The
  sidebar's bottom **Overall** panel (issue #53) shows cluster-wide totals
  (sessions, sub-agents, tokens in/out, requests, errors, prompt-cache hit %)
  drawn from the `Statistics` report's grand totals joined with the sidebar's own
  node counts; it refreshes on the `SessionEvent` stream, coalesced to ~250 ms
  (the redraw note in #22), with the per-second status ticker as a floor while
  any session is busy.

## Persistence

- Config: `~/.gogent/config.json` (models, optional `fast_model` + `model_roles`
  for auxiliary tasks, timeouts, sub-agent settings, notification settings,
  token-budget settings).
- Sessions: sharded JSONL in the sessions directory. A live session occupies a
  base prefix `<iso>_<id>_session` laid out as `<base>.index` (the session meta
  and shard table — the source of truth) plus `<base>.0000.jsonl`,
  `<base>.0001.jsonl`, … each capped at ~5 000 records / ~10 MiB so no single
  file grows unboundedly. Closing a session renames the base to
  `..._session_archived` across all its files. Listing reads only the tiny index
  files (plus the current shard for restore), never replaying the whole history;
  non-archived sessions are restored on startup (crash recovery). Durability
  (`fsync`) is batched off the turn path; a graceful shutdown should flush via
  `SessionStore.Sync`/`Close`.
- Workbench layout: `~/.gogent/workbench_layout.json` — the desktop arrangement
  (sidebar order, per-session titles, pin/favorite state, window bounds and
  minimized flag). Re-applied on startup after `RestoreSessions`, so renamed,
  pinned, reordered and moved/resized windows come back where the user left
  them. The title here is a UI concern decoupled from the session id; the
  transcript itself lives in the session JSONL above.
- Diagnostics: warnings and errors go through `internal/diag`, never raw
  `fmt.Printf` to stdout. In TUI mode they append to `~/.gogent/gogent.log` (a
  file, so they can't corrupt the alternate screen); in headless mode they go to
  stderr. Skill-load and session-encode failures are surfaced here instead of
  being swallowed.
- Checkpoints: in-memory, per-session shadow copies of files touched by
  `write`/`edit` (`internal/fileops/checkpoint.go`). The active turn's
  checkpoint accumulates one snapshot per file (first mutation wins), committed
  at the end of `SendMessageToSessionWithModel`; `/undo` reverts the last turn,
  `/rewind [n]` reverts several (oldest snapshot wins per file). They are a
  safety net for the running process only — restarting clears them (the
  transcript still recovers from the JSONL store), and shell-driven changes are
  not captured.



## Entry points

- Default: interactive TUI (`./gogent`).
- Headless HTTP server mode for API-only use (`-http -no-tui`). `POST /message`
  routes each client to its own `UserSession` keyed by the `X-Gogent-Session`
  header (or a `gogent_session` cookie / `session` form field), so concurrent
  clients are isolated rather than multiplexed onto one shared transcript; no id
  falls back to a shared `default` session. These per-client sessions are
  ephemeral (never persisted/restored) and bounded by an idle-TTL + LRU cap.
- `-verbose` for extra startup/diagnostic logging.

## System context (AGENTS.md + repo map + skills)

Each task loop injects a system-context block built by `internal/gogent`:

- **AGENTS.md** project instructions are discovered (`syscontext.go`) by walking from
  the workspace root up to the filesystem root (outermost first, nearest last) plus a
  global `~/.gogent/AGENTS.md`, concatenated with source headers and size-capped.
- **Repo map** (`repomap.go`) is a ranked symbol skeleton built at startup: the
  workspace is walked, top-level Go declarations are extracted with `go/parser`, and
  each is ranked Aider-style by how often its name is referenced across the tree. The
  map is grouped by file (most-referenced first) and size-capped, giving the model a
  cheap project overview without reading every file. Currently Go-only; other languages
  are a follow-up.
- **Skills** use progressive disclosure: an index of *active* skills (name +
  description) is listed in the prompt, and a `skill` tool loads a skill's full
  `SKILL.md` on demand, recording per-skill usage. Skills load from `~/.gogent/skills`
  and `./skills`; they can be toggled in the TUI (Config → Skills…).
- **Git status** is injected when the workspace is a git repo (detected once at
  startup): a live `git status --short --branch` summary (`internal/vcs`) is added
  each loop, so the model always sees the current branch and working-tree state and
  can checkpoint with the `git` tool without first having to ask.
  and `./skills`; they can be toggled in the TUI (Config → Resources…), which is also
  where the registered tools (including those exposed by MCP servers) are browsed
  and toggled.

## Known gaps

The permission gate (`internal/permission`) authorizes every side-effecting tool:
workspace file ops are allowed; shell and any out-of-workspace access prompt
interactively (Allow once / Always / Deny), with "Always" persisted to
`~/.gogent/permissions.json`. The shell external-path check is a best-effort
guardrail, not a sandbox; OS-level sandboxing is still future work.

