# Running headless

gogent can run without a TUI as a pure HTTP server — useful for automation,
scripting, remote control, or driving gogent from another program. Run it with:

```
./gogent -no-tui
```

The HTTP server is **always** started in both modes. In TUI mode it runs
alongside the workbench; in headless (`-no-tui`) mode it is the *only*
interface — there is no workbench, no terminal UI, just the HTTP surface.

This guide covers the headless experience: starting the server, the loopback
binding gate, authentication, session keying, the legacy form-encoded
endpoints, the `/api` REST surface, and the approvals bridge that makes
interactive permission prompts work without a human at a terminal.

For the complete endpoint reference, see [api.md](api.md). For configuration
details, see [configuration.md](configuration.md). To get gogent installed
and running the first time, see [getting-started.md](getting-started.md).

## Starting headless

```
./gogent -no-tui
```

Defaults: host `127.0.0.1`, port `8080`. Relevant flags:

| Flag             | Purpose                                            |
|------------------|----------------------------------------------------|
| `-http-host`     | Bind address for the HTTP server.                  |
| `-http-port`     | Bind port for the HTTP server.                     |
| `-http-password` | Password for cookie-based login (see Auth modes). |
| `-verbose`       | Verbose diagnostics.                              |
| `-no-color`      | Disable ANSI color in diagnostics.                |

In headless mode, diagnostics go to **stderr** — there is no log file
redirect. (TUI mode, by contrast, writes `~/.gogent/gogent.log`.)

## Loopback binding gate

A non-loopback host — anything other than `""`, `127.0.0.1`, `::1`,
`localhost`, or a loopback IP — is **refused** unless a password *or* a token
is set. This prevents accidentally exposing gogent to the network without
authentication.

Set one via:

- the `-http-password` flag, or the `GOGENT_HTTP_PASSWORD` environment
  variable (password), or
- the `GOGENT_HTTP_TOKEN` environment variable (bearer token).

If you bind to a non-loopback address without either, the server will not
start.

## Authentication modes

The `/api` surface resolves identity in this order (`composingProvider.GetSession`):

1. **Loopback** — a request from `127.0.0.1` or `::1` is treated as the local
   user, scope `human`, no credential required.
2. **Password cookie** — a `gogent_session` cookie, HMAC-SHA256 signed, 24-hour
   TTL, with a per-process random key. It is issued by `POST /api/auth/login`
   when the supplied password matches (constant-time compare). An empty
   password disables this path entirely.
3. **Bearer token** — an `Authorization: Bearer <token>` header, looked up in
   the token map. `GOGENT_HTTP_TOKEN` maps to its scope (default `human` if
   unset).

If nothing matches, the request is anonymous and gets **401** on protected
endpoints.

### Scopes

- **human** — the full surface: create sessions, change settings, resolve
  prompts.
- **peer** — restricted to the session/message/event surface. Cannot change
  settings, list other sessions, or shut down.

Password login always grants `human`. Only bearer tokens can carry `peer`.

### Credential sources

- **Password**: `-http-password` flag takes precedence over the
  `GOGENT_HTTP_PASSWORD` env var.
- **Token**: `GOGENT_HTTP_TOKEN` env var.

## Session keying (legacy endpoints)

The legacy `/message` and `/status` handlers derive a per-client session id in
priority order:

1. `X-Gogent-Session` header
2. `gogent_session` cookie
3. `session` form/query field
4. fallback `"default"`

The id is sanitized (alphanumerics plus `-_.`, max 128 chars). Each client
session id maps to its own isolated backend `UserSession` — concurrent clients
neither serialize against each other nor see each other's transcript.

The legacy registry bounds these sessions: a **30-minute idle TTL** and an
**LRU cap of 256**.

> The `/api` surface is different: it uses explicit session ids created via
> `POST /api/sessions` (random `sess_<hex>` ids) and does **not** apply
> LRU/TTL eviction. Those sessions live until explicitly shut down.

## Legacy endpoints (form-encoded)

These are the original, form-encoded endpoints. They predate the `/api`
surface but remain fully supported.

### `GET /health`

Public — no auth. Returns:

```json
{"status":"healthy"}
```

### `POST /message`

Auth-gated on non-loopback. Form-encoded fields:

- `message` (required)
- `model` (optional; also accepted via the `X-Gogent-Model` header)
- `session` (optional)

Body cap is 1 MiB. Runs the full agent task loop on the client's session; a
client disconnect cancels in-flight model work. Returns:

```json
{"success": true, "message": "<final assistant output>"}
```

or on failure:

```json
{"success": false, "error": "..."}
```

Responds `405` if not POST, `400` if the form is missing.

### `GET /status`

Auth-gated. Session is resolved via the client session id (see
[Session keying](#session-keying-legacy-endpoints)). Returns:

```json
{"tool_logs": [...], "stats": {...}}
```

### `POST /exit`

Self-gated: loopback is always allowed; a remote request must present an
`X-Gogent-Token` header matching `GOGENT_HTTP_TOKEN` (constant-time compare),
otherwise `403`. Returns:

```json
{"success": true, "message": "Shutdown initiated"}
```

and triggers a graceful shutdown — the same path as `SIGINT`/`SIGTERM`.
Responds `405` if not POST.

## The `/api` REST surface

A full REST + SSE surface lives under `/api` (built on the `webapi` package).
For the complete endpoint reference, see [api.md](api.md). Highlights:

- `POST /api/sessions` — create a session.
- `POST /api/sessions/:id/messages/stream` — SSE live turn stream.
- `GET /api/sessions/:id/events` — per-session SSE event stream.
- `GET /api/events` — global SSE event stream.
- `GET` / `POST /api/approvals` — interactive prompt approvals.
- `GET` / `PUT /api/settings` — read/update settings.
- `GET` / `PUT /api/models` — read/update model configuration.
- `GET /api/tools` — list available tools.
- `GET /api/skills` — list available skills.

All JSON; SSE is used for anything that streams.

## Interactive prompts headlessly (the `/approvals` bridge)

In headless mode, the approval bridge (`internal/server/approvals.go`) is
installed as **both** the permission prompter *and* the edit reviewer. When
the agent hits a permission prompt or an edit-review gate, it:

1. registers a pending approval (discovered by clients polling
   `GET /api/approvals` — there is no SSE push for approvals), and
2. **blocks** until a client `POST`s a decision to
   `/api/approvals/:aid/decision`.

Decisions:

- **Permission**: `allow` / `always` / `always_deny` / `deny`
  (unknown → `deny`).
- **Edit review**: `approve` / `approve_all` / `reject`
  (unknown → `reject`).

**Safe default.** When a prompt goes unanswered, it is **denied** (permission)
or **rejected** (edit review) — matching the headless deny-by-default posture.
The wait before that safe default depends on whether a client is connected
(issue #358 §8):

- **A client is connected but unresponsive** → denied after the approval
  timeout (**5 minutes** by default; `0` = block forever). The clock counts
  only continuous connected time and resets if the client disconnects.
- **No client is connected** → the prompt **waits** up to the longer
  `unattended_approval_timeout` safety cap (**1 hour** by default; `0` = wait
  forever), so a daemon whose TUI briefly drops does not get its long watcher
  turns killed. A client that reconnects sees the prompt via
  `GET /api/approvals` and can still answer it.

> In TUI mode the bridge is **not** installed; the workbench modals remain
> the prompter/reviewer. The `/approvals` endpoints still exist, however.

## A quick curl example

This logs in (only if a password is set), creates a session, and sends a
blocking message — reusing the cookie jar across requests:

```bash
# 1. Log in (skip if no password is set / you're on loopback).
curl -c cookies.txt -sS -X POST http://127.0.0.1:8080/api/auth/login \
  -d 'password=YOUR_PASSWORD'

# 2. Create a session.
SESSION=$(curl -b cookies.txt -sS -X POST http://127.0.0.1:8080/api/sessions \
  -H 'Content-Type: application/json' \
  -d '{}' | jq -r .id)

# 3. Send a message (blocking — waits for the full turn).
curl -b cookies.txt -sS -X POST \
  "http://127.0.0.1:8080/api/sessions/$SESSION/messages" \
  -H 'Content-Type: application/json' \
  -d '{"message":"Summarize the files in the current directory."}'
```

For a live, streaming turn, use the SSE path instead of the blocking one:

```bash
curl -b cookies.txt -N -X POST \
  "http://127.0.0.1:8080/api/sessions/$SESSION/messages/stream" \
  -H 'Content-Type: application/json' \
  -d '{"message":"Summarize the files in the current directory."}'
```

The `stream` endpoint emits server-sent events as the agent thinks, calls
tools, and produces its final answer — ideal for piping into another program
or a custom UI.
