---
name: parallel-research
description: Delegate independent research and verification to sub-agents in parallel to cut wall-clock latency
---

# Parallel Research

Use this skill whenever a task has two or more INDEPENDENT lookups — investigating
several files or modules, running multiple checks, or researching while you keep
working. Delegating them to sub-agents runs them concurrently and cuts wall-clock
latency. Doing such work inline, one step after another, is the slow path; make
delegation your default disposition for independent research and verification.

The mechanism is `spawn_subagent`: a SINGLE call whose `subtasks` array runs every
entry CONCURRENTLY and returns only when the slowest finishes. One call — never one
call per part, and never separate spawns across turns (those run serially with no
speed-up). Each one-shot sub-agent ends its result with `SUCCESS:` or `FAILURE:`.

## Recipes

- **Audit a codebase** — delegate one sub-agent per subsystem, each mapping its
  module's key types, entry points, and how it is wired, then synthesize the
  results.
- **Validate a change** — delegate `diagnostics` + `verify` + a targeted `grep` in
  parallel: one runs the compiler/linter, one exercises the behavior, one checks
  for other call sites that need the same edit.
- **Investigate a bug** — delegate parallel reads of the candidate files (each
  sub-agent reading and reasoning about one suspect area) while you reason about
  the report yourself.

## Worked example

To probe three modules at once instead of reading them serially, make ONE call:

```json
{"tool":"spawn_subagent","args":{"subtasks":[
  {"name":"agent","task":"Map internal/agent: key types and how the task loop runs. Report SUCCESS with a summary."},
  {"name":"gogent","task":"Map internal/gogent: the tool registry and the spawn_subagent flow. Report SUCCESS with a summary."},
  {"name":"verify","task":"Run diagnostics and the internal/agent tests; report SUCCESS with results or FAILURE with the failures."}
]}}
```

## Granularity

- **Delegate** when a subtask is at least two tool calls (e.g. read several files,
  or run a check and interpret it) — that is where concurrency pays off.
- **Do it inline** for a trivial single-step action: one `read`, one `grep`, one
  `glob`. Spawning a sub-agent for a single lookup adds overhead without a
  latency win.
- Put every independent part into the one call's `subtasks` array so they run at
  once; use a lone `name`/`task` pair only when there is genuinely just one task.
