# gogent Web API — Design

A comprehensive HTTP+SSE API that turns gogent into a server a browser or
**another gogent** can talk to over the network. Built on `hobbestherat/webapi`
(stdlib-only, reflection-bound handlers, first-class SSE). This document
specifies the contract.

## 1. Goals

1. **Web client** — drive gogent from a browser or other device: send messages,
   watch a turn stream live (thinking / tool calls / results / final), and
   **approve interactive prompts** (permissions + edit reviews) that currently
   only reach the in-process TUI.
2. **gogent-to-gogent** — a gogent on machine A delegates a one-shot task to a
   gogent on machine B and reads the streamed result back, as a *remote*
   sub-agent.
3. **Comprehensive** — expose the full capability surface the TUI already uses
   (the `tuipkg.Handlers` struct), not just send-message.
4. **Clean & tested** — `webapi` is dependency-free and battle-tested; adopt its
   routing/binding/auth/SSE rather than hand-rolling.

## 2. Design principles

- **One server, two client types.** A browser and a peer gogent hit the *same*
  endpoints. Federation is just a programmatic HTTP+SSE client — **no separate
  "federation" protocol**.
- **Sessions are first-class resources.** Today sessions are keyed by a header;
  in the REST API they live in the path (`/api/sessions/:id`). The per-client
  LRU+TTL isolation stays; it just moves from a header into a resource.
- **The typed `SessionEvent` stream *is* the SSE wire format.** The backend
  already emits it to the TUI; the API fans it out unchanged (JSON-serialized).
- **Interactive gates become async.** `permission.Prompter` and
  `gogent.EditReviewer` are synchronous blocking interfaces today. A small
  adapter bridges them to an async request id + SSE event + decision POST, so a
  remote client can answer them. Safe default preserved: *no client connected →
  deny* (matches today's headless behavior).
- **Backwards compatible.** The legacy `POST /message` (form-encoded) endpoint
  stays; the new surface lives under `/api`.

## 3. Authentication & authorization

Three strategies, **all via `webapi.SessionProvider`** — which is exactly what
makes adding a mode trivial: `GetSession(r *http.Request)` is the single
chokepoint, so a new credential type is ~30 lines. A composing provider resolves
them in order (loopback → password cookie → bearer token → anonymous):

| Strategy | Mechanism | Scope | Use |
|---|---|---|---|
| **Loopback** | request from `127.0.0.1`/`::1` → local user, no credential | `human` | same-machine TUI/headless |
| **Password** | shared password → login → signed session **cookie** | `human` | a simple identity gate for a network-exposed instance |
| **Bearer token** | `Authorization: Bearer <token>` | `human` **or** `peer` (per token) | programmatic clients; peer gogents |

The strategies **compose**: a deployment can use a password for a browser *and*
bearer tokens for peer gogents at once — the provider just tries each.

**Scopes** map to `webapi` `Permissions` (enforced via a `PermissionChecker`):

- `human` — full UI surface (create sessions, change settings, resolve prompts).
- `peer` — restricted to the session/message/event surface; **cannot** change
  settings, list other sessions, or shut the server down. Password login always
  grants `human`; only bearer tokens carry `peer` (so federation stays token-based).

### 3.1 Password mode

A shared password is the simplest way to gate a network-exposed instance without
standing up PKI. The password is set in one of (priority order): `--http-password`
flag, `GOGENT_HTTP_PASSWORD` env, or `config.json` `http_password`. **Setting a
password or token is what authorizes a non-loopback bind**; without one the
listener stays loopback-only.

It is a textbook `webapi.SessionProvider`:

```go
type passwordProvider struct{ secret []byte } // per-process random signing key

func (p passwordProvider) GetSession(r *http.Request) (webapi.Session, error) {
    c, err := r.Cookie("gogent_session")
    if err != nil { return anonSession{}, nil }           // not logged in -> anonymous
    uid, exp, ok := p.verify(c.Value)                     // HMAC-SHA256(token.exp, secret)
    if !ok || time.Now().After(exp) { return anonSession{}, nil }
    return userSession{id: uid, state: webapi.UserStateComplete}, nil
}
```

Login validates the password (`crypto/subtle.ConstantTimeCompare`, matching the
existing `/exit` token check) and sets the cookie:

```
POST /api/auth/login   { "password": "..." }
  -> 200 + Set-Cookie: gogent_session=<signed>; HttpOnly; SameSite=Lax; Path=/
     (401 on mismatch)
POST /api/auth/logout  -> 204 (clears cookie)
GET  /api/auth/me      -> 200 { "authenticated": true, "scope": "human" }  | 401
```

**Why a cookie, not a bearer token:** `EventSource` (browser SSE) **cannot set
headers**, so a session cookie authenticates the event streams (§5.5) with zero
extra plumbing — this resolves the SSE-auth question in §11. The cookie is a
random opaque token, HMAC-signed with a per-process key, so the server is
stateless and a restart simply forces a re-login.

> **Security:** password mode guards *identity*, not *confidentiality*. Over
> plain HTTP the credential is sent in the clear, so front the server with TLS
> (a reverse proxy, terminating TLS, is the usual arrangement) or otherwise
> secure the transport before relying on it. The cookie is `HttpOnly` +
> `SameSite=Lax`; it is intentionally **not** `Secure` by default so it also
> works over plain HTTP during local development — flip `Secure` when TLS fronts
> the server.

> Server shutdown (`/api/system/shutdown`) keeps the **existing** loopback-IP
> gate (or token) — never exposed to `peer` scope, and not reachable via password
> alone either.

## 4. Wire envelope

All JSON. Successful bodies are the resource directly; errors use `webapi.HTTPError`
with optional `details`:

```json
{ "error": "session not found", "details": { "code": "not_found" } }
```

Status codes: `200` data, `201` created, `204` no content, `400` bad request,
`401` unauthenticated, `403` forbidden / denied, `404` not found, `405` wrong
method, `409` conflict (e.g. send while busy), `415` wrong content-type, `500`
server error, `503` no prompter client connected (interactive gate timed out).

## 5. Endpoint surface

`webapi.API{ BasePath: "/api" }`. Path params (`:id`, `:name`) are bound
positionally by webapi; bodies are the 2nd handler param (struct).

### 5.1 Sessions

| Method | Path | Body | Returns | Notes |
|---|---|---|---|---|
| `GET` | `/sessions` | — | `Session[]` | saved + live sessions (index metadata) |
| `POST` | `/sessions` | `CreateSession` | `201 Session` | `persisted` flag: live (default) vs restorable |
| `GET` | `/sessions/:id` | — | `Session` | meta + agent tree |
| `DELETE` | `/sessions/:id` | — | `204` | close + (archive if persisted) |
| `GET` | `/sessions/:id/transcript` | — | `Message[]` | root agent transcript (query `?agent=`) |
| `GET` | `/sessions/:id/stats` | — | `SessionStats` | `UserSession.Snapshot()` + per-model |

```jsonc
// Session
{
  "id": "abc123",
  "title": "Refactor API",
  "created_at": "2026-01-01T12:00:00Z",
  "state": "idle",                 // idle|thinking|waiting_prompt|waiting_review
  "primary_model": "zai-glm",
  "persisted": true,
  "agents": [ { "id": "root", "status": "idle", "kind": "root" } ]
}
// CreateSession
{ "title": "Refactor API", "persisted": true, "model": "zai-glm" }
```

### 5.2 Messages (the core)

| Method | Path | Body | Returns |
|---|---|---|---|
| `POST` | `/sessions/:id/messages` | `SendMessage` | `Message` (final answer) |
| `POST` | `/sessions/:id/messages/stream` | `SendMessage` | **SSE** event stream |

```jsonc
// SendMessage
{
  "message": "Add a streaming endpoint",
  "model": "zai-glm",              // optional override
  "effort": "high",                // optional reasoning_effort override
  "mode": "normal"                 // "normal" (default) | "plan"
}
```

- **Blocking** `messages` runs the full ReAct loop and returns only the final
  answer (today's behavior, kept for simple clients).
- **`messages/stream`** is the live path: returns immediately with SSE
  headers, then emits every `SessionEvent` the loop produces until `final`/`error`.
  Client disconnect cancels the loop (SSE context). `409` if already busy.

> `/messages/stream` is the highest-value endpoint in this design and the one
> that justifies adopting `webapi` (its `EventStreamResponse`).

### 5.3 Session control

| Method | Path | Body | Notes |
|---|---|---|---|
| `POST` | `/sessions/:id/stop` | — | cancel in-flight turn (`StopAgent`) |
| `POST` | `/sessions/:id/inject` | `{ "message": "..." }` | mid-turn clarification (`InjectUserNote`) |
| `POST` | `/sessions/:id/undo` | — | revert last turn (`UndoLastTurn`) |
| `POST` | `/sessions/:id/rewind` | `{ "turns": 3 }` | revert N turns (`Rewind`) |

### 5.4 Plan mode

| Method | Path | Body | Notes |
|---|---|---|---|
| `GET` | `/sessions/:id/plan-mode` | — | `{ "enabled": bool }` |
| `PUT` | `/sessions/:id/plan-mode` | `{ "enabled": true }` | toggle |
| `GET` | `/sessions/:id/plan` | — | the proposed plan (`pendingPlan`); `404` if none |
| `POST` | `/sessions/:id/plan/approve` | — | re-run with full tools (`ExecuteApprovedPlan`) |
| `POST` | `/sessions/:id/plan/reject` | — | discard (`RejectPlan`) |

### 5.5 Events (SSE)

| Method | Path | Notes |
|---|---|---|
| `GET` | `/sessions/:id/events` | live `SessionEvent` stream for one session |
| `GET` | `/events` | live events across **all** sessions (carries `session_id`) |

These are read-only subscriptions; `messages/stream` is the combined
"send + subscribe" convenience for a single turn.

### 5.6 Approval gates (interactive prompts) ★

This is what makes a remote client actually usable. The backend's two blocking
gates surface as async approvals:

| Method | Path | Body | Notes |
|---|---|---|---|
| `GET` | `/approvals` | — | `Approval[]` (all pending) |
| `POST` | `/approvals/:aid/decision` | `Decision` | resolve a pending approval |

```jsonc
// Approval (emitted on the session + global event streams too)
{
  "id": "apr_8f3",
  "kind": "permission",            // "permission" | "edit_review"
  "session_id": "abc123",
  "agent_id": "root",
  "permission": {                  // present when kind == "permission"
    "action": "shell",
    "resource": "rm -rf /tmp/x",
    "detail": "rm -rf /tmp/x"
  },
  "edit_review": {                 // present when kind == "edit_review"
    "path": "internal/server/handlers.go",
    "op": "edit",
    "diff": "--- a/...\n+++ b/..."
  }
}
// Decision
{ "decision": "allow" }            // permission: allow|deny|always|always_deny
                                  // edit_review: approve|reject|approve_all
```

**Bridge design.** A single `pendingApprovalRegistry` adapter implements both
`permission.Prompter` and `gogent.EditReviewer`. On a prompt it: assigns an id,
registers a `chan decision`, emits an `approval` SSE event, then **blocks** on
the channel. `POST /approvals/:aid/decision` delivers the answer and unblocks it.
If no client is connected / a timeout expires → **deny** (safe default).

Both gates also ride the session event stream (as an `approval` event), so a
client watching `/sessions/:id/events` sees its own prompts inline.

### 5.7 Settings

| Method | Path | Body | Notes |
|---|---|---|---|
| `GET` | `/settings` | — | `{ sub_agents, timeouts, budget, review_edits }` |
| `PUT` | `/settings` | partial `Settings` | merge + persist (`SaveConfig`) |
| `GET`/`PUT` | `/settings/notifications` | `NotifyConfig` | |
| `GET`/`PUT` | `/settings/theme` | `ThemeConfig` | (cosmetic for web; harmless) |
| `GET`/`PUT` | `/settings/review-edits` | `{ "enabled": bool }` | |

`human` scope only. Bodies map 1:1 to the `config.*` structs (Section 7).

### 5.8 Models

| Method | Path | Body | Notes |
|---|---|---|---|
| `GET` | `/models` | — | `ModelConfig[]` |
| `PUT` | `/models/:name` | `ModelConfig` | update + persist (`UpdateModel`) |
| `POST` | `/models/:name/scan` | — | `string[]` (`ScanModels`, probes endpoint) |
| `GET` | `/models/:name/models` | — | `ModelInfo[]` (`ListBackendModels`) |

> `ModelConfig.APIKey` is **write-only**: never echoed back in `GET` responses
> (redacted), even to `human` scope.

### 5.9 Tools & skills

| Method | Path | Notes |
|---|---|---|
| `GET` | `/tools` | `Tool[]` (name, description, schema, enabled, invocations) |
| `PUT` | `/tools/:name/enabled` | `{ "enabled": bool }` |
| `GET` | `/skills` | `Skill[]` (name, description, active, stats) |
| `PUT` | `/skills/:name/active` | `{ "active": bool }` |
| `GET` | `/skills/:name` | full `SKILL.md` content |

### 5.10 Auth (password mode)

| Method | Path | Body | Notes |
|---|---|---|---|
| `POST` | `/auth/login` | `{ "password": "..." }` | `200` + cookie, `401` mismatch |
| `POST` | `/auth/logout` | — | `204` clears cookie |
| `GET` | `/auth/me` | — | `200 {authenticated, scope}` or `401` |

`AuthNone` endpoints, so the login flow itself isn't caught in the auth gate.
All other endpoints are `AuthRequired`; `/auth/me` tells a web client whether it
has a live cookie before attempting protected calls.

### 5.11 System

| Method | Path | Notes |
|---|---|---|
| `GET` | `/health` | `{ "status": "healthy" }` (also kept at `/health`) |
| `GET` | `/workspace` | `{ "root": "/path", "git": { "branch": "...", "dirty": bool } }` |
| `GET` | `/stats` | aggregate `Statistics()` across all sessions |
| `POST` | `/system/shutdown` | graceful shutdown (loopback/token gated; **never** `peer`) |

## 6. SSE event protocol

`text/event-stream`. One event per `SessionEvent` (and `approval`), `event:`
set to the type so a browser `EventSource` can route by name. `data:` is JSON.

```
event: thinking
data: {"type":"thinking","step":0}

event: tool_call
data: {"type":"tool_call","step":1,"tool":"read","call_id":"rc_42","args":{"path":"main.go"}}

event: tool_result
data: {"type":"tool_result","step":1,"tool":"read","call_id":"rc_42","result":"package main..."}

event: usage
data: {"type":"usage","stats":{"turns":1,"tokens_in":1200,"tokens_out":80,"context_tokens":5000,"context_window":200000}}

event: approval
data: {"type":"approval","id":"apr_8f3","kind":"permission",...}

event: final
data: {"type":"final","text":"Done — added /messages/stream."}

event: error
data: {"type":"error","error":"model round-trip: context deadline exceeded"}
```

`stream.Context()` cancellation (client disconnect) cancels the in-flight loop —
strictly better than today's blocking handler.

## 7. Type mapping (config → wire)

Wire types are thin JSON-tagged views over the existing structs (no logic
changes needed upstream):

| Wire type | Source |
|---|---|
| `SessionStats` | `agent.SessionStats` |
| `SessionEvent` | `agent.SessionEvent` (serialize `Err` as a string; drop zero fields) |
| `ModelConfig` | `config.ModelConfig` (`api_key` write-only) |
| `Settings.sub_agents` | `config.SubAgentConfig` |
| `Settings.timeouts` | `config.TimeoutConfig` |
| `Settings.budget` | `config.BudgetConfig` |
| `NotifyConfig` | `config.NotifyConfig` |
| `ThemeConfig` | `config.ThemeConfig` |
| `Permission`/`Decision` | `permission.Request` / `permission.Decision` |
| `EditReview`/`Decision` | `gogent.EditReviewRequest` / `gogent.EditReviewDecision` |

## 8. gogent-to-gogent (federation)

**No new server endpoint.** A peer gogent is a programmatic client of §5. A
`spawn_remote_agent` tool on the *calling* gogent:

1. `POST /api/sessions` on the peer → `session_id`.
2. `POST /api/sessions/:id/messages/stream` with the delegated task.
3. Reads the SSE stream to completion; the peer's `final` event carries
   `SUCCESS:`/`FAILURE:` (the peer's own agent loop already emits this framing).
4. `DELETE /api/sessions/:id`.

The peer authenticates with a `peer`-scoped token (§3), so it can't escape the
session surface. Fan-out/depth/budget limits on the *caller* bound cost exactly
as local sub-agents do today.

**Discovery.** Peer endpoints live in a new `config.json` block (mirrors the
existing `mcp_servers` shape):

```jsonc
"peers": [
  { "name": "laptop", "base_url": "http://10.0.0.5:8080", "token": "..." }
]
```

The caller-side client (`internal/http`) gains an SSE reader; the tool wraps it
as a one-shot sub-agent result. This is the only net-new code for federation.

## 9. Server-side structure (for the implementation pass)

```
internal/server/
  api.go          // builds *webapi.API, wires SessionProvider + Permissions
  auth.go         // token SessionProvider + scope PermissionChecker
  approvals.go    // pendingApprovalRegistry: Prompter + EditReviewer bridge
  handlers_*.go   // one file per resource group (sessions, models, ...)
  wire.go         // JSON view types (§7)
```

`cmd/main.go`'s inline HTTP code moves here; `startHTTPServer` shrinks to
mounting the `webapi.API` + keeping the legacy `/message` shim. The
`httpSessionRegistry` (LRU+TTL) is reused as-is.

## 10. Migration & rollout

1. **Vendor `webapi`** + build `/api/health`, `/api/sessions`, and
   `/api/sessions/:id/messages/stream` (SSE) end-to-end. *Highest value, proves
   the integration.*
2. Add the approval bridge (§5.6) — unblocks a usable remote client.
3. Fill out the REST surface (settings/models/tools/skills/plan).
4. Legacy `POST /message` becomes a thin adapter onto the new session endpoint.
5. Federation client + `spawn_remote_agent` tool (§8).

## 11. Open questions

- ~~**Streaming auth for SSE:** `EventSource` can't set headers~~ → **resolved
  by §3.1:** the password session cookie authenticates the event streams with no
  extra plumbing. For bearer-token clients (peer gogents) that *can* set
  headers, the token goes in the normal `Authorization` header; only a pure
  browser bearer flow would need `?token=` — not needed once cookie login exists.
- **Session persistence default:** ephemeral (today's HTTP) or persisted
  (resumable). Lean persisted + `POST /sessions?persisted=false`.
- **Approval timeout:** default deny after N seconds with no connected client.
  Configurable under `settings`.
- **Multi-user:** out of scope for v1 (single password / single token); the
  `webapi` `User`/`Permissions` model leaves room to grow per-user passwords.
