# gogent

A small Go coding agent with streaming model support, an agent tree with
sub-agents, a Turbo-Vision-style multi-session TUI, and an HTTP API for
headless/remote use.

gogent gates every side-effecting tool through a single resource+action
permission service: workspace file ops are allowed without prompting, while
shell, out-of-workspace access, network, and sub-agent spawns prompt
interactively (Allow once / Always / Deny), with "Always" persisted to
`~/.gogent/permissions.json`. In headless runs there is no one to ask, so any
"ask" decision is **denied by default** — keeping automated runs safe.

> The shell guardrail is best-effort, not a sandbox: a shell is Turing-complete
> and a determined command can still reach outside the workspace. Treat it as a
> seatbelt against accidental damage, not as containment.

---

## Quickstart

```sh
go build -o gogent ./cmd
./gogent
```

Configuration lives in `~/.gogent/config.json` (a full sample with every option
is at [`config.sample.json`](config.sample.json)). The default LAN endpoint
honors the `GOGENT_MODEL_URL` environment variable.

Run headless (HTTP API only):

```sh
./gogent -no-tui
```

See [Getting started](docs/getting-started.md) for prerequisites, all CLI flags,
environment variables, and the runtime directory layout.

---

## Documentation

| Document | What it covers |
|---|---|
| [Getting started](docs/getting-started.md) | Prerequisites, build & install, CLI flags, environment variables, run modes, the `~/.gogent/` layout, first-run setup. |
| [Configuration](docs/configuration.md) | The complete `config.json` schema — every field of every config struct, with defaults. |
| [Model providers](docs/providers.md) | The `api_type` values (openai, zai, anthropic, openrouter), auth, reasoning models, prompt-cache, streaming, retries, the model editor. |
| [Using the TUI](docs/usage-tui.md) | Menus, command palette, keybindings, sidebar, window management, transcript navigation, @-mentions, saved sessions, export, statistics, theme editor, notifications, model editor, status line. |
| [Running headless](docs/usage-headless.md) | Headless HTTP mode, loopback binding gate, auth modes, session keying, the `/approvals` bridge. |
| [HTTP API](docs/api.md) | The full endpoint reference: legacy form endpoints + the `/api` REST+SSE surface, auth scopes, SSE event protocol. |
| [Architecture](docs/architecture.md) | Layering, the annotated package map, the ReAct loop, sub-agents, fan-out limits, budgets, plan mode, compaction, the model layer, MCP, persistence, system context. |
| [Tools & permissions](docs/tools-and-permissions.md) | Every tool, every permission Action, default posture, persisted grants, checkpoints/undo/rewind, the edit-review gate. |
| [Development](docs/development.md) | Building & testing, the CI pipeline, cross-repo deps, how to add a tool / provider / skill. |

For outstanding work, see [`TODO.md`](TODO.md).

## Status

Early / experimental (0.x). The model-connection layer (`internal/model`) is
intended to be split out into its own library later.
