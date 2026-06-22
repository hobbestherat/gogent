# HTTP API

gogent exposes an HTTP API on a single server that is **always started**, in both TUI and headless modes. The server has two handler trees:

- the **`/api`** REST + SSE surface, built on [`github.com/hobbestherat/webapi`](https://pkg.go.dev/github.com/hobbestherat/webapi), and
- the **legacy** form-encoded endpoints (`/health`, `/message`, `/status`, `/exit`).

The default bind address is `127.0.0.1:8080`. Server tunables:

| Tunable             | Value                          | Notes                                            |
|---------------------|--------------------------------|--------------------------------------------------|
| `ReadHeaderTimeout` | 10s                            |                                                  |
| `ReadTimeout`        | 30s                            |                                                  |
| `IdleTimeout`        | 120s                           |                                                  |
| `MaxRequestBody`     | 1 MiB                          | Applied to request bodies.                       |
| `WriteTimeout`       | **unset**                      | Intentionally left open for long model loops.    |

See [usage-headless.md](usage-headless.md) for how the run mode selects the server, and [configuration.md](configuration.md) for the bind address, password, and token settings.

---

## Authentication & scopes

`composingProvider.GetSession` resolves identity in this order:

1. **Loopback** — requests from `127.0.0.1` / `::1` are granted the `human` scope with no credential.
2. **Password cookie** — the `gogent_session` cookie, HMAC-SHA256 signed with a 24h TTL, issued by `POST /api/auth/login`.
3. **Bearer token** — an `Authorization: Bearer <token>` header matched against the token map (populated from `GOGENT_HTTP_TOKEN`).

If none match, the request is **anonymous** and gets `401 Unauthorized`.

### Scopes

| Scope   | Capabilities                                                                 |
|---------|------------------------------------------------------------------------------|
| `human` | Full API surface.                                                            |
| `peer`  | Session, message, and event surface only. Cannot change settings, list sessions, or shut down. |

`requireHuman` is a second, handler-level check that gates sensitive endpoints, returning `403 Forbidden` for `peer` callers.

### Password & token sources

- **Password**: `-http-password` flag takes precedence over the `GOGENT_HTTP_PASSWORD` environment variable.
- **Token**: `GOGENT_HTTP_TOKEN` environment variable (may be comma-separated for multiple tokens).

### Loopback binding gate

A non-loopback bind address is **refused** unless a password or token is configured. This prevents accidentally exposing an unauthenticated server on a public interface.

---

## Legacy endpoints

These are form-encoded endpoints mounted at the root, outside the `/api` tree.

| Method | Path       | Auth                          | Description                                                                                                                                                                          | Response                                                                 |
|--------|------------|-------------------------------|-------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|--------------------------------------------------------------------------|
| GET    | `/health`  | Public                        | Liveness probe.                                                                                                                                                                     | `{"status":"healthy"}`                                                   |
| POST   | `/message` | Auth-gated (non-loopback)     | Send a message. Form fields: `message`, `model`, `session`. Body capped at 1 MiB. Runs the full agent loop; client disconnect cancels it.                                            | `{"success":true,"message":"..."}` or `{"success":false,"error":"..."}` |
| GET    | `/status`  | Auth-gated                    | Per-session status. Session resolved via `X-Gogent-Session` header, `gogent_session` cookie, or `session` form/query param.                                                          | `{"tool_logs":[...],"stats":{...}}`                                      |
| POST   | `/exit`    | Self-gated (see below)        | Initiate graceful shutdown. Allowed only from loopback, or when `X-Gogent-Token` matches `GOGENT_HTTP_TOKEN` (constant-time compare); otherwise `403`.                              | `{"success":true,"message":"Shutdown initiated"}`                        |

### Session keying (legacy)

The session key resolution order is: `X-Gogent-Session` header → `gogent_session` cookie → `session` form/query parameter → `"default"`. Keys are sanitized, per-client isolated, kept in an LRU cache capped at 256 entries with a 30-minute TTL.

---

## `/api` surface

Base path: `/api`. All routes are `AuthRequired` **except** `/api/auth/login` and `/api/health`, which are `AuthNone`.

Handler methods are bound positionally: the first parameter receives path parameters, the second receives the request body. SSE endpoints return a `*webapi.EventStreamResponse`.

The **Auth** column denotes the minimum scope: `human` (requires `requireHuman`), `any` (any authenticated scope), `none` (`AuthNone`).

### Sessions

| Method | Path                                  | Auth  | Body / Query            | Description                                                                                              | Response                                                                 |
|--------|---------------------------------------|-------|-------------------------|----------------------------------------------------------------------------------------------------------|--------------------------------------------------------------------------|
| GET    | `/api/sessions`                       | human | —                       | List sessions.                                                                                           | `[]sessionView`                                                          |
| POST   | `/api/sessions`                       | human | `createSessionRequest`  | Create a session.                                                                                        | `sessionView` (id `sess_<hex>`)                                          |
| GET    | `/api/sessions/:id`                   | any   | —                       | Fetch one session. `404` if not found.                                                                   | `sessionView`                                                            |
| DELETE | `/api/sessions/:id`                   | human | —                       | Close + archive. `404` if not found.                                                                     | `null`                                                                   |
| GET    | `/api/sessions/:id/transcript`        | any   | `?agent=root`           | Message transcript. `404` if not found.                                                                  | `[]messageView`                                                          |
| GET    | `/api/sessions/:id/stats`             | any   | —                       | Live session stats. `404` if not live.                                                                   | `sessionStatsView`                                                       |
| POST   | `/api/sessions/:id/stop`              | any   | —                       | Stop the root agent. `404` if not found.                                                                 | —                                                                        |
| POST   | `/api/sessions/:id/inject`            | any   | `injectRequest`         | Inject a user note. `404` if not found; `400` if message empty.                                          | —                                                                        |
| POST   | `/api/sessions/:id/undo`              | any   | —                       | Undo last turn. `400` on error.                                                                          | `{"result":...}`                                                         |
| POST   | `/api/sessions/:id/rewind`            | any   | `rewindRequest`         | Rewind N turns (`turns ≤ 0` → `1`). `400` on error.                                                      | `{"result":...}`                                                         |
| GET    | `/api/sessions/:id/plan-mode`         | any   | —                       | Plan-mode status. `404` if not found.                                                                    | `{"enabled":bool}`                                                       |
| PUT    | `/api/sessions/:id/plan-mode`         | human | `planModeRequest`       | Toggle plan mode.                                                                                        | `{"enabled":bool}`                                                       |
| GET    | `/api/sessions/:id/plan`              | any   | —                       | Pending plan. `404` if no pending plan.                                                                  | `{"plan":string}`                                                        |
| POST   | `/api/sessions/:id/plan/approve`      | human | —                       | Execute the approved plan. `400` on error.                                                              | `messageView`                                                            |
| POST   | `/api/sessions/:id/plan/reject`       | human | —                       | Discard the pending plan.                                                                               | `null`                                                                   |

**`sessionView`**: `{id, title, created_at, state, primary_model, persisted, agents[]}`.
**`createSessionRequest`**: `{title, persisted, model}`.
**`messageView`**: `{role, content}`.
**`sessionStatsView`**: `{turns, tokens_in, tokens_out, tool_calls, context_tokens, context_window}`.
**`injectRequest`**: `{message}`.
**`rewindRequest`**: `{turns}`.
**`planModeRequest`**: `{enabled}`.

### Messages

| Method | Path                                  | Auth | Body                  | Description                                                                                                                                                                                                              | Response / Errors                                                              |
|--------|---------------------------------------|------|-----------------------|--------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|--------------------------------------------------------------------------------|
| POST   | `/api/sessions/:id/messages`          | any  | `sendMessageRequest`  | **Blocking.** Runs the full ReAct loop and returns the final answer. Per-session busy lock: `409` if busy. `400` if message empty; `404` if session not found; `409` if busy; `500` on loop error.                       | `messageView`                                                                  |
| POST   | `/api/sessions/:id/messages/stream`   | any  | `sendMessageRequest`  | **SSE.** Returns immediately with SSE headers and emits every `SessionEvent` (as `eventView` JSON; SSE `event:` = event type) until final/error. Client disconnect cancels the loop. `409` if busy.                      | `text/event-stream`                                                            |

**`sendMessageRequest`**: `{message, model, effort, mode}` where `mode` is `normal` (default) or `plan`.

### Events

| Method | Path                          | Auth | Description                                                                                                       |
|--------|-------------------------------|------|-------------------------------------------------------------------------------------------------------------------|
| GET    | `/api/sessions/:id/events`    | any  | **SSE.** Live `SessionEvent` stream for one session. `404` if not found. Disconnect cancels.                      |
| GET    | `/api/events`                 | any  | **SSE.** Global event stream — every session's events wrapped as `globalEventView{session_id, event:{...}}`.      |

### Approvals

| Method | Path                          | Auth | Body                       | Description                                                                                                                                                                                                                          | Response / Errors                                              |
|--------|-------------------------------|------|----------------------------|--------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|----------------------------------------------------------------|
| GET    | `/api/approvals`              | any  | —                          | List pending approvals.                                                                                                                                                                                                              | `[]approvalView`                                               |
| POST   | `/api/approvals/:aid/decision`| any  | `approvalDecisionRequest`  | Resolve an approval. **Permission** decisions: `allow` / `always` / `always_deny` / `deny` (unknown → `deny`). **Edit-review** decisions: `approve` / `approve_all` / `reject` (unknown → `reject`). `404` if not found; `409` if already resolved; `400` if unknown kind. | `{"id":aid,"status":"resolved"}`                               |

**`approvalView`**: `{id, kind, session_id, agent_id, permission?, edit_review?, created_at}`. `kind` is `permission` or `edit_review`.
- `permission`: `{action, resource, detail?}`
- `edit_review`: `{path, op, diff}`

**`approvalDecisionRequest`**: `{decision}`.

> **Note:** The approval bridge (`InstallApprovalGates`) is installed **only in headless mode**. In TUI mode the workbench modals handle prompts, but these endpoints still exist. The safe default on a 5-minute timeout is to **deny** permission requests and **reject** edit reviews.

### Auth

| Method | Path                  | Auth   | Body            | Description                                                                                                                                                                                          | Response / Errors                                                       |
|--------|-----------------------|--------|-----------------|------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|-------------------------------------------------------------------------|
| POST   | `/api/auth/login`     | none   | `loginRequest`  | If no password is configured: no-op `{"authenticated":true,"scope":"human"}`. If the password matches: sets a signed `gogent_session` cookie (`HttpOnly`, `SameSite=Lax`, `Path=/`, `Secure` off by design for loopback). `401` on mismatch. | `{"authenticated":true,"scope":"human"}`                                |
| POST   | `/api/auth/logout`    | req.   | —               | Issue a deletion cookie.                                                                                                                                                                             | —                                                                       |
| GET    | `/api/auth/me`        | req.   | —               | Current identity. `401` if not authenticated.                                                                                                                                                        | `{"authenticated":true,"scope":"<human|peer>","user_id":<id>}`          |

**`loginRequest`**: `{password}`.

### Models

All model endpoints require `human`.

| Method | Path                       | Body                  | Description                                                                                              | Response / Errors                              |
|--------|----------------------------|-----------------------|----------------------------------------------------------------------------------------------------------|------------------------------------------------|
| GET    | `/api/models`              | —                     | List configured models. API keys are redacted; `has_api_key` is reported instead.                        | `[]modelView`                                  |
| PUT    | `/api/models/:name`        | `updateModelRequest`  | Update a model config. An empty `api_key` preserves the existing key. `404` if not found.                | `modelView`                                    |
| POST   | `/api/models/:name/scan`   | —                     | Probe the backend model list. `404` if not found; `502` on scan error.                                   | `{"models":[...]}`                             |

**`updateModelRequest`** embeds `config.ModelConfig`.

### Tools

| Method | Path                          | Auth  | Body                  | Description                                              | Response / Errors                          |
|--------|-------------------------------|-------|-----------------------|----------------------------------------------------------|--------------------------------------------|
| GET    | `/api/tools`                  | any   | —                     | List tools.                                              | `[]toolView`                               |
| PUT    | `/api/tools/:name/enabled`    | human | `setEnabledRequest`   | Enable/disable a tool. `404` if not found.               | `{"name":name,"enabled":bool}`             |

**`toolView`**: `{name, description, input_schema, enabled, invocations, read_only}`.
**`setEnabledRequest`**: `{enabled}`.

### Skills

| Method | Path                          | Auth  | Body                  | Description                                              | Response / Errors                                                              |
|--------|-------------------------------|-------|-----------------------|----------------------------------------------------------|--------------------------------------------------------------------------------|
| GET    | `/api/skills`                 | any   | —                     | List skills.                                             | `[]skillView`                                                                  |
| PUT    | `/api/skills/:name/active`    | human | `setEnabledRequest`   | Activate/deactivate a skill. `404` if not found.         | `{"name":name,"active":bool}`                                                  |
| GET    | `/api/skills/:name`           | any   | —                     | Full skill content. `404` if not found.                  | `{"name":...,"description":...,"content":"<full SKILL.md>"}`                   |

**`skillView`**: `{name, description, active, success, failure, total_calls}`.

### Settings

All settings endpoints require `human`.

| Method | Path                              | Body             | Description                                  | Response                  |
|--------|-----------------------------------|------------------|---------------------------------------------|---------------------------|
| GET    | `/api/settings`                   | —                | Read settings.                              | `settingsView`            |
| PUT    | `/api/settings`                   | `settingsView`   | Update + persist settings.                  | `settingsView`            |
| GET    | `/api/settings/notifications`     | —                | Read notification config.                   | `config.NotifyConfig`     |
| PUT    | `/api/settings/notifications`     | `config.NotifyConfig` | Update notification config.            | `config.NotifyConfig`     |
| GET    | `/api/settings/review-edits`      | —                | Read edit-review toggle.                    | `{"enabled":bool}`        |
| PUT    | `/api/settings/review-edits`      | `reviewEditsView`| Update edit-review toggle.                  | `{"enabled":bool}`        |

**`settingsView`**: `{sub_agents, timeouts, budget, review_edits}`.
**`reviewEditsView`**: `{enabled}`.

### System

| Method | Path             | Auth  | Description                                              | Response                                  |
|--------|------------------|-------|---------------------------------------------------------|-------------------------------------------|
| GET    | `/api/health`    | none  | Liveness probe.                                         | `{"status":"healthy"}`                    |
| GET    | `/api/workspace` | any   | Workspace root and git status.                          | `{root, git?:{branch,dirty}}`             |
| GET    | `/api/stats`     | human | Aggregate stats across all sessions.                    | `stats.Report`                            |

---

## SSE event protocol

SSE endpoints use `text/event-stream`. One event is emitted per `SessionEvent` (and per approval). The SSE `event:` field is set to the event **type**, so a browser `EventSource` can route by name; the `data:` field is JSON.

### `eventView` fields

```
{type, step, text, tool, args, result, call_id, error, stats, agent_id, name, status, kind, todos, plan, session_id}
```

### Event types

`thinking`, `thinking_delta`, `thinking_done`, `assistant_step`, `tool_call`, `tool_result`, `final`, `error`, `sub_agent`, `compaction`, `usage`, `todo`, `plan`.

### Global stream

The global event stream (`GET /api/events`) wraps each event as:

```
globalEventView{session_id, event:{...}}
```

### Delivery semantics

- **Terminal events** (`final`, `error`, `plan`) get a 250ms blocking-send grace period before being dropped.
- **Non-terminal events** are best-effort: dropped if a subscriber is slow or its buffer is full.
- A **client disconnect cancels** the in-flight agent loop.

---

## Event hub

The event hub fans live `agent.SessionEvent`s out to SSE subscribers. It maintains:

- a set of **per-session** subscribers (buffered channel, capacity 64), and
- a **global** subscriber set (buffered channel, capacity 128).

When the server creates or loads a session, it installs `hub.sessionObserver(id)` via `UserSession.SetObserver`. This is the **same typed event stream the TUI consumes**, delivered verbatim to API clients — so SSE clients and the TUI see identical, consistent events.
