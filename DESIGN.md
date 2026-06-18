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
internal/fileops/      Path resolution, file I/O, file mutation
internal/command/      Shell exec + internal commands (calc/echo/help)
internal/skill/        SKILL.md loader + registry (wired via syscontext + skill tool)
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
- **Tools** (`internal/tool`, `internal/fileops`, `internal/web`, `internal/vcs`) —
  `read`, `write`, `edit`, `shell`, `web_fetch`, `git`, `spawn_subagent`,
  agent-control tools, and `structured_output`. File ops resolve paths against the
  workspace root (the launch cwd) and run through a keyed-mutex file mutator.
  `web_fetch` downloads an http(s) URL and returns readability-style Markdown
  (size-capped, short-TTL cached, gated per domain via `ActionNetwork`); the
  HTML→Markdown reduction is a dependency-free, stdlib-only extractor
  (`internal/web`). `git` is a dispatched wrapper over the git binary
  (`status`/`diff`/`log`/`commit`/`create_branch`/`restore`) backed by
  `internal/vcs`, which runs git with explicit argument vectors (no shell, no
  injection surface) and disabled interactive prompts; mutating operations are
  gated via `ActionShell`, read-only ones run freely.
- **TUI** (`ui/tui`) — `Workbench` desktop hosting draggable `SessionWindow`s,
  each with a foldable transcript (turbotv `TextView`), per-session model select,
  status line, a right-hand sidebar (session/sub-agent tree), and Config / model
  / settings dialogs. The **Session** menu manages many windows: rename, pin
  (favorite, floats to the top with a ★), move up/down to reorder, and
  close-others / close-all. The desktop layout (order, titles, pin state, window
  bounds) is persisted to `~/.gogent/workbench_layout.json` and restored on the
  next launch. Each transcript is backed by an indexed model (`transcript_model.go`)
  that keys entries by event kind, so the **View** menu (and the focused-transcript
  keys `/`, `a`/`t`/`r`/`e`, `f`/`u`, `Esc`) can search and filter over the model
  and rebuild the view rather than scanning rendered cells.

## Persistence

- Config: `~/.gogent/config.json` (models, optional `fast_model` + `model_roles`
  for auxiliary tasks, timeouts, sub-agent settings).
- Sessions: JSONL in the sessions directory — live as
  `<iso>_<id>_session.jsonl`, renamed to `..._session_archived.jsonl` on close.
  Non-archived files are restored on startup (crash recovery); re-loading an
  archived session is a rename back.
- Workbench layout: `~/.gogent/workbench_layout.json` — the desktop arrangement
  (sidebar order, per-session titles, pin/favorite state, window bounds and
  minimized flag). Re-applied on startup after `RestoreSessions`, so renamed,
  pinned, reordered and moved/resized windows come back where the user left
  them. The title here is a UI concern decoupled from the session id; the
  transcript itself lives in the session JSONL above.

## Entry points

- Default: interactive TUI (`./gogent`).
- Headless HTTP server mode for API-only use (`-http -no-tui`).
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
  where the registered tools (and, once MCP lands, MCP servers) are browsed and toggled.

## Known gaps

The permission gate (`internal/permission`) authorizes every side-effecting tool:
workspace file ops are allowed; shell and any out-of-workspace access prompt
interactively (Allow once / Always / Deny), with "Always" persisted to
`~/.gogent/permissions.json`. The shell external-path check is a best-effort
guardrail, not a sandbox; OS-level sandboxing is still future work.

