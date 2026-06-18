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
internal/tool/         Tool registry, shell tool, structured-output tool
internal/fileops/      Path resolution, file I/O, file mutation
internal/command/      Shell exec + internal commands (calc/echo/help)
internal/skill/        SKILL.md loader + registry (see TODO: not wired in)
internal/permission/   Resource+action permission gate (shell/file/external/...)
internal/compression/  Context compression
internal/http/         HTTP client; cmd also has a headless HTTP server mode
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
  main chat; sub-agent detail is kept for the monologue popup.
- **Sub-agents** — two execution models: *one-shot* (blocking, must end in
  `SUCCESS:`/`FAILURE:`) and *interactive* (async, may return `CLARIFY`). Mode,
  recursion depth, and fan-out limits are configurable. Batched
  `spawn_subagent` calls run concurrently.
- **Model layer** (`internal/model`) — split into small capability interfaces
  (`Completer`, streaming, tools, stats) so it can later be extracted into its
  own library. A provider abstraction (`provider.go`, `APIType`) maps each
  configured backend to its endpoint layout, so the same OpenAI-compatible
  transport serves generic servers (`openai`) and the Z.AI platform (`zai`); a
  bare base URL is normalized into the concrete endpoints. Connections are
  rebuilt per send, so model/endpoint edits take effect on the next turn.
- **Tools** (`internal/tool`, `internal/fileops`) — `read`, `write`, `edit`,
  `shell`, `spawn_subagent`, agent-control tools, and `structured_output`. File
  ops resolve paths against the workspace root (the launch cwd) and run through a
  keyed-mutex file mutator.
- **TUI** (`ui/tui`) — `Workbench` desktop hosting draggable `SessionWindow`s,
  each with a foldable transcript (turbotv `TextView`), per-session model select,
  status line, a right-hand sidebar (session/sub-agent tree), and Config / model
  / settings dialogs.

## Persistence

- Config: `~/.gogent/config.json` (models, timeouts, sub-agent settings).
- Sessions: JSONL in the sessions directory — live as
  `<iso>_<id>_session.jsonl`, renamed to `..._session_archived.jsonl` on close.
  Non-archived files are restored on startup (crash recovery); re-loading an
  archived session is a rename back.

## Entry points

- Default: interactive TUI (`./gogent`).
- Headless HTTP server mode for API-only use (`-http -no-tui`).
- `-verbose` for extra startup/diagnostic logging.

## Known gaps

Skills are loaded but not yet wired into the agent — see `TODO.md`.

The permission gate (`internal/permission`) authorizes every side-effecting tool:
workspace file ops are allowed; shell and any out-of-workspace access prompt
interactively (Allow once / Always / Deny), with "Always" persisted to
`~/.gogent/permissions.json`. The shell external-path check is a best-effort
guardrail, not a sandbox; OS-level sandboxing is still future work.

