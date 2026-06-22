# Getting started

gogent is a Go coding agent with a Turbo-Vision-style terminal UI. It drives an OpenAI-compatible model to read, edit, and run code in your workspace, with permission prompts and diff review routed through the TUI. This guide gets you from a fresh clone to a running session in a few minutes.

## Prerequisites

- **Go 1.25.11.** The module is `gogent` with a `go 1.25.11` directive in `go.mod`; older toolchains will refuse to build.
- **A capable terminal.** The TUI renders best with truecolor; gogent detects this from `COLORTERM=truecolor` or `COLORTERM=24bit`. A `256`-suffixed `TERM` selects 256-colour mode, and a missing or `dumb` `TERM` disables colour entirely.
- **An OpenAI-compatible model endpoint.** This can be a local llama.cpp-style server or a hosted provider — anything that speaks the `/v1/chat/completions` shape.

## Build & install

There is no Makefile. Build the binary with the standard Go toolchain:

```sh
go build -o gogent ./cmd
```

The test suite and CI use the equivalent entry-point form and `go build ./...` respectively:

```sh
go build -o gogent ./cmd/main.go   # as test.sh does
go build ./...                     # as CI does
```

Then run it:

```sh
./gogent          # default = interactive TUI mode
```

> A prebuilt `gogent` binary (~18 MB) sits at the repo root, but you should build your own from source to match your platform and Go version.

## CLI flags

Flags are defined in `cmd/main.go` via the `flag` package:

| Flag | Default | Description |
| --- | --- | --- |
| `-verbose` | `false` | Enable verbose output. Prints a config dump (home dir, skills dir, config dir, model URL, built-in command count, active skills) only when **both** `-verbose` and `-no-tui` are set; always prints the active-skill count. |
| `-http-host` | `127.0.0.1` | HTTP server host. Loopback by default; a non-loopback host is refused unless a password or token is set. |
| `-http-port` | `8080` | HTTP server port. |
| `-no-tui` | `false` | Disable the TUI (headless HTTP API mode). Note: the flag is `-no-tui`, **not** `-disable-tui`. |
| `-no-color` | `false` | Disable coloured output (also honoured via the `NO_COLOR` env var). |
| `-http-password` | `""` | Password for HTTP API login (env `GOGENT_HTTP_PASSWORD`). Setting one authorizes binding to a non-loopback host. |

## Environment variables

| Variable | Purpose |
| --- | --- |
| `GOGENT_MODEL_URL` | Overrides the default model chat-completions endpoint (used by the built-in `local-lan` model entry, shown as "Local LAN (env: GOGENT_MODEL_URL)"). Fallback if unset: `http://192.168.1.88:8080/v1/chat/completions`. |
| `GOGENT_HTTP_PASSWORD` | HTTP API login password; used as fallback when `-http-password` is empty. A non-empty value authorizes binding to a non-loopback host. |
| `GOGENT_HTTP_TOKEN` | Passed as the server token **and** the `/exit` kill-switch token. A non-empty value also authorizes a non-loopback bind. Remote `/exit` callers must present a matching `X-Gogent-Token` header (compared in constant time). |
| `NO_COLOR` | Any non-empty value disables colour. Equivalent to `-no-color` and the `theme.no_color` config key. |
| `COLORTERM` | `truecolor` or `24bit` selects 24-bit truecolor. |
| `TERM` | Missing or `dumb` disables colour; a `256`-suffixed value selects 256-colour mode. |
| `HOME` | Underpins the `~/.gogent` directory layout. |

## Run modes

**TUI mode (default):** `./gogent`. Builds the multi-session Workbench, routes permission prompts and diff-review approvals to workbench modals, and applies the configured colour theme. On startup it prints:

```
TUI enabled. Press Ctrl+C to exit.
```

**Headless / HTTP mode:** `./gogent -no-tui`. The HTTP API bridge is the only prompter/reviewer; interactive prompts are answered over `/api/approvals`, and diagnostics go to stderr (no log-file redirect).

In **both** modes the HTTP server is always started — the `/api` surface plus the legacy `/message`, `/status`, `/exit`, and `/health` endpoints. The server shuts down gracefully on `SIGINT`/`SIGTERM`, or on an authorized `POST /exit`.

## Runtime config directory

Everything lives under `$HOME/.gogent` (the directory is created with mode `0750`; secret files are `0600`):

| Path | Description |
| --- | --- |
| `config.json` | Main configuration (see [configuration.md](configuration.md)). If absent, the built-in default config is used. |
| `gogent.log` | Diagnostic log (warnings/errors) in TUI mode only; headless mode keeps stderr. |
| `audit.log` | Security audit trail — permission decisions and tool calls — always file-backed in both modes. |
| `permissions.json` | Persisted permission grants/decisions (directory `0700`, file `0600`). |
| `workbench_layout.json` | Saved TUI window/desktop layout. |
| `AGENTS.md` | Optional global project instructions. |
| `skills/` | User skill directory (built-in skills are also loaded from `<workspaceRoot>/skills`). |
| `sessions/` | Persisted session transcripts — sharded `.jsonl` files plus index files. |

## First-run setup

1. **Build the binary.**

   ```sh
   go build -o gogent ./cmd
   ```

2. **(Optional) point at a model.** Either set `GOGENT_MODEL_URL` to a local model server, or edit `~/.gogent/config.json` to add a hosted provider with an `api_key`. See [providers.md](providers.md) for provider-specific examples.

3. **Run it.**

   ```sh
   ./gogent
   ```

   A default session window opens; type a message and press Enter.

4. **Approve the first tool use.** The first time the agent runs a shell command or touches a file outside the workspace, a permission modal appears with **Allow once** / **Always** / **Deny** choices. Choosing **Always** persists the grant to `permissions.json` so you won't be asked again for that action.

## Next steps

- [configuration.md](configuration.md) — full config reference for `~/.gogent/config.json`.
- [providers.md](providers.md) — connecting local and hosted model providers.
- [usage-tui.md](usage-tui.md) — driving the Turbo-Vision-style Workbench.
- [usage-headless.md](usage-headless.md) — the HTTP API and headless workflow.
- [architecture.md](architecture.md) — internals: agent loop, sessions, tools, and permissions.
