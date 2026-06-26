# Design — Issue #481: Decouple agent turns from the HTTP request context

Goal: a daemon agent turn must run to completion (or until `POST /stop`)
regardless of whether any TUI client is connected. A client disconnect must not
cancel an in-flight turn; progress + final answer must be recoverable by a
reconnecting client.

This is the **design phase** — no code is written yet. Line numbers below were
verified against the current tree and are exact at time of writing.

---

## 1. Verified current state (what actually couples the turn to the connection)

The daemon-side HTTP handlers pass a **request-derived context** straight into
the synchronous Gogent turn entrypoint, which threads it down to the model HTTP
call:

| Handler | File:line | ctx source | what runs |
|---|---|---|---|
| `Send` (subtask/agent branch) | `internal/server/messages.go:42` | `r.Context()` | `runCommandOverride` → `RunCommandSubtask` (full sub-agent turn) |
| `Send` (root branch) | `internal/server/messages.go:49` | `r.Context()` | `SendMessageToSessionWithModelAndEffort` (full turn) |
| `Stream` | `internal/server/messages.go:131,134` | `stream.Context()` | `runCommandOverride` / `SendMessage…` (full turn) |
| `ApprovePlan` | `internal/server/sessions.go:230` | `r.Context()` | `ExecuteApprovedPlan` (full turn) |

Path into the model call (confirmed):
`SendMessageToSessionWithModelAndEffort(ctx,…)` (`internal/gogent/gogent.go:2493`)
→ `userSession.ExecuteTaskLoopWithModel(ctx,…)` (`internal/agent/user_session.go:2383`)
→ `ExecuteTaskLoop(ctx,…)` (`:787`)
→ `s.runLoop(ctx,…)` (`:827`)
→ `ctx,cancel := context.WithCancel(ctx)` (`:1159`, child **inherits** the HTTP ctx) →
`agent.setCancel(cancel)` → model round-trips. When the TCP connection dies the
HTTP server cancels `r.Context()`/`stream.Context()`, which propagates here and
aborts the turn mid-stream.

**Embedded path is already correct** (`cmd/embedded_handlers.go:41,56,302,426`):
`OnSend`/`OnApprovePlan`/`OnSendCommand` each run
`go func(){ g.SendMessageToSessionWithModelAndEffort(context.Background(), …) }()`
— a background goroutine under `context.Background()`. **It is not changed.** The
whole point of this issue is to bring the daemon/HTTP path to the same model the
embedded path already uses.

**Stop is already decoupled** (`internal/server/sessions.go:138` → `us.StopAgent("root")`
→ `agent.Cancel()` at `internal/agent/agent.go:271`). `runLoop` creates its *own*
cancellable child (`:1159`) and publishes the cancel func via `setCancel`
(`:1160`); `Cancel()` invokes that stored func directly, independent of the parent
context. So launching under `context.Background()` keeps Stop working. **No change.**

**Hub already fans out to reconnecting clients** (`internal/server/hub.go`): the
session observer (`createSession`, `internal/server/api.go:305`) delivers every
`SessionEvent` to per-session and global subscribers; terminal events
(final/error/plan) get a short blocking send so they are not dropped
(`hub.go:93-108`); a bounded ring (`notificationRingSize = 50`, `hub.go:28`)
replays missed completion/error notifications to a reconnecting `/events`
subscriber (`subscribeGlobal`, `hub.go:236`). **No change.**

---

## 2. Hard constraint discovered: the framework cannot emit literal `202`

The issue asks handlers to "return 202 Accepted". The `webapi` framework
(`github.com/hobbestherat/webapi@v0.1.0`, a **separate module** — not gogent, not
turbotui) **cannot set an arbitrary success status code**:

- A handler's first arg must be exactly `*http.Request`
  (`webapi.go:697`); there is no way to inject `http.ResponseWriter`, so a handler
  cannot call `WriteHeader(202)` itself.
- `processResults` (`webapi.go:915`) writes `204` for a `nil` result, an error
  code via `HTTPError` (`:938`), and otherwise marshals JSON with the **default
  `200`** (`:1042-1051`). The only special response types are `BinaryResponse`,
  `StreamResponse`, `CookieResponse`, `EventStreamResponse` — **none carries a
  status code**.

Honoring "no new dependencies / no go.mod bump" means we **cannot** patch
`webapi` to add a status-code response. **Decision: return `200 OK` with an
`acceptedView{TurnID}` JSON body.** The behavioral contract the issue actually
cares about — *the handler returns immediately with a turn ID instead of blocking
until the turn completes* — is fully met; only the numeric status differs (200
vs 202). This is called out in **Open questions** so the maintainer can decide
whether a webapi change is worth it later.

---

## 3. Design

### 3.1 Turn IDs on `SessionEvent` (`internal/agent/user_session.go`)

Add one field to `SessionEvent` (`:100`):

```go
// TurnID correlates every event of one dispatched turn back to the POST that
// started it. Empty for legacy/embedded turns and non-turn events (#481).
TurnID string
```

**Propagation — via context value, no signature churn.** `runLoop` already
receives `ctx`; the embedded and dispatch paths differ only in what's in it:

- New unexported key + accessor in `internal/agent` (stdlib `context` only):
  ```go
  type turnIDKey struct{}
  func WithTurnID(ctx context.Context, id string) context.Context // exported for gogent
  func turnIDFrom(ctx context.Context) string
  ```
- In `runLoop` (`:1151`), after the root/sub-agent `emit` selection (`:1201-1204`),
  wrap the root emitter to stamp the id read from ctx:
  ```go
  turnID := turnIDFrom(ctx)
  if turnID != "" {
      base := emit
      emit = func(ev SessionEvent) { ev.TurnID = turnID; base(ev) }
  }
  ```
  Every event the loop emits (thinking, tool call/result, usage, **final**,
  **error**) carries the turn id. Sub-agent loops keep their no-op emitter
  (`:1202-1204`), so nothing changes for them.

Why context value, not a new parameter: it touches **only** `runLoop`, leaves
`ExecuteTaskLoop`/`ExecuteTaskLoopWithModel`/`SendMessageToSessionWithModelAndEffort`
signatures **unchanged** (embedded path passes `context.Background()` → empty id,
exactly as today), and naturally flows to any sub-call that re-reads ctx.

### 3.2 Async dispatch in the Gogent core (`internal/gogent/gogent.go`)

Add three thin async wrappers **alongside** the existing synchronous methods
(which are retained verbatim — the embedded path keeps calling them directly):

```go
// DispatchMessage mints a turn id, then runs the existing synchronous turn in a
// daemon-owned goroutine under context.Background()+turnID, so the turn's lifetime
// is independent of any HTTP connection. onDone runs when the turn goroutine
// returns (success or error) — the caller uses it to release the busy gate. The
// final answer/error reach clients as SessionEventFinal/Error via the observer→hub.
func (g *Gogent) DispatchMessage(sessionID, agentID, message, modelName, effort string, onDone func()) (turnID string, err error)
func (g *Gogent) DispatchApprovedPlan(sessionID, agentID string, onDone func()) (turnID string, err error)
func (g *Gogent) DispatchCommandSubtask(sessionID, agentID, message string, onDone func()) (turnID string, err error)
```

Each:
1. Validates the session/agent exists **synchronously** (so a 404/500 can be
   returned before any goroutine starts and `onDone` is *not* leaked); reuses the
   existing `SessionNotFoundError` paths.
2. Mints `turnID` with a **stdlib-only** generator: `"turn_" + hex(crypto/rand
   16 bytes)`. (No ULID dependency — honors no-new-deps. A small helper in the
   gogent package; `crypto/rand`+`encoding/hex` are stdlib.)
3. `ctx := agent.WithTurnID(context.Background(), turnID)`.
4. `go func(){ defer onDone(); … existing sync method (ctx) … }()`.
5. Returns `turnID, nil` immediately.

`DispatchCommandSubtask` must reproduce what `runCommandOverride`
(`messages.go:62`) does today: after `RunCommandSubtask` returns it surfaces the
sub-agent's result as the **session's** `SessionEventFinal` (the subtask's own
final is not the root session's final). Today the *server* does this via
`hub.deliver`. In the async core the goroutine can't reach the server hub, so it
emits through the **session observer** instead (same destination — the hub *is*
the observer, wired at `api.go:305`). Add a tiny exported helper on `UserSession`:
```go
func (s *UserSession) EmitFinal(turnID, text string) // emit{Type:Final,Text,TurnID}
func (s *UserSession) EmitError(turnID string, err error)
```
and have `DispatchCommandSubtask`'s goroutine call `EmitFinal`/`EmitError`
(stamped with `turnID`). The server's `runCommandOverride` hub.deliver shim is
then removed from the async path (the blocking Stream path may keep its own; see
3.4).

> **Optional, not v1-blocking:** a `map[turnID]{sessionID,status,startedAt}`
> registry on `Gogent`, populated at dispatch and cleared in the `onDone` defer,
> to back a future `GET /sessions/:id/turns/:turnID`. The SSE hub + transcript
> refresh already cover disconnect recovery, so this is a convenience only and is
> **deferred**. The `onDone` callback is the busy-release mechanism regardless
> (it composes cleanly with the registry if added later).

### 3.3 HTTP handlers return immediately (`internal/server/messages.go`, `sessions.go`)

New response type in `internal/server/wire.go`:
```go
// acceptedView is the non-blocking send/approve response: the turn id of the
// dispatched turn. Returned with 200 (webapi can't emit 202; see design §2).
type acceptedView struct {
    TurnID string `json:"turnId"`
}
```

**`Send` (`messages.go:18`)** — keep the existence check (`:22`) and `markBusy`
(`:27`), but **drop the handler-level `defer release()`** (`:31`). The release now
fires on turn completion. Plan-mode toggle (`:34-37`) likewise must restore on
**completion**, not on handler return (otherwise async restores plan mode before
the turn even runs). Compose both into one `onDone`:
```go
release, ok := svc.s.markBusy(id); if !ok { 409 }
planOn := req.Mode == "plan"; if planOn { svc.s.g.SetPlanMode(id, true) }
onDone := func(){ if planOn { svc.s.g.SetPlanMode(id, false) }; release() }

var turnID string; var err error
if req.Subtask || req.Agent != "" {
    turnID, err = svc.s.g.DispatchCommandSubtask(id, req.Agent, req.Message, onDone)
} else {
    turnID, err = svc.s.g.DispatchMessage(id, "root", req.Message, req.Model, req.Effort, onDone)
}
if err != nil { onDone(); return 500 }     // dispatch failed before goroutine; release gate
return acceptedView{TurnID: turnID}, nil
```

**`ApprovePlan` (`sessions.go:223`)** — same shape, dispatching
`DispatchApprovedPlan`. Note: ApprovePlan does **not** call `markBusy` today
(`sessions.go:223-235` has no busy claim); going async it now **acquires
`markBusy`** so the plan-execution turn holds the busy gate for its full duration
(a concurrent send gets 409 mid-execution). This is a small, intended behavioral
tightening — without it an async plan turn would race a concurrent send.

### 3.4 `Stream` endpoint (`messages.go:74`) — keep, but decouple

Kept for backwards compatibility, but the in-flight turn must no longer die on
client disconnect, and the busy gate must no longer release on disconnect:

- The producer goroutine currently runs the turn under `stream.Context()`
  (`:131,134`) and ties `defer release()` to producer lifetime (`:117`). Change to
  **dispatch the turn async** (via the same `Dispatch*` methods, under
  `context.Background()`), with `onDone = release`. The producer then only
  *subscribes and forwards* events to the connected client (`:138-151`).
- On client disconnect (`<-stream.Context().Done()`, `:140`) the producer
  returns and `unsub()`s (`:118`) — but the turn keeps running and `release`
  fires later from `onDone`. So a reconnecting client that sends gets 409 until
  the turn really finishes.
- Plan-mode restore (`:119-121`) likewise moves into `onDone`.

This makes Stream a thin async-dispatch wrapper: it streams to whoever's
connected and stops streaming on disconnect, without affecting turn lifetime or
the busy gate.

### 3.5 Client (`ui/tui/api_client.go`, `ui/tui/remote_handlers.go`)

The client already runs sends on `context.Background()` goroutines
(`remote_handlers.go:531,561,791`) and already consumes progress + final answer
over the global SSE stream (`RemoteClient.consume`, `:199`). Two adjustments:

1. **Response shape (`api_client.go`).** `SendMessage`/`SendMessageWithOverrides`
   (`:415,424`) and `ApprovePlan` (`:496`) today decode a `MessageDTO` used only
   to detect failure. The body is now `acceptedView{turnId}`. JSON decoding is
   lenient (unknown fields ignored), so the existing decode into `MessageDTO`
   keeps working untouched; we will additionally decode `turnId` (so a future
   correlation feature has it) and may change the return to surface the turn id.
   Minimal-churn: keep signatures, just stop relying on `Content`.

2. **Suppress false "turn failed" errors while disconnected (`remote_handlers.go`).**
   Today a failed POST calls `rc.emitErr` (`:532,562,792`), which would now fire a
   spurious error in the window when the POST fails *because the connection
   dropped* — even though the daemon turn is still running. Track a disconnected
   flag on `RemoteClient` (an `atomic.Bool`, set in `notifyLost` `:314`, cleared
   in `notifyRestored` `:320` — these already bracket the disconnect modal), and
   in the three send goroutines, when the send errors **while disconnected**,
   `log.Printf` instead of `emitErr`. A genuine server-side dispatch failure
   (HTTP 4xx/5xx received while connected) still surfaces via `emitErr`.

The disconnect modal + `OnConnectionRestored` (`:45`, re-fetches `/sessions` +
transcripts) already cover recovery UX; the final answer is in the transcript and
arrives live as `SessionEventFinal` for a connected client.

---

## 4. The four design gates

### (1) Goal match
Exactly the issue's ask — a behavior fix, no scope creep:
- **Turns survive disconnect:** turn runs under `context.Background()` in a
  daemon goroutine; `r.Context()`/`stream.Context()` no longer reach the model
  call. ✔
- **Non-blocking send/approve:** handlers call `Dispatch*` and return
  `acceptedView{TurnID}` immediately (200, not literal 202 — §2). ✔
- **Progress recoverable:** unchanged hub fan-out + ring replay + transcript
  refresh; events now carry `TurnID` for correlation. ✔
- **Stop still works:** untouched — `agent.Cancel()` is ctx-independent. ✔
- **Busy-state correct:** `markBusy` acquired before dispatch, released in
  `onDone` on turn completion (not on handler/producer return). ✔
- **Embedded unchanged:** synchronous methods retained; embedded path not
  touched. ✔

### (2) Usability
- Connected TUI behaves exactly as before: live events stream, final answer
  arrives as `SessionEventFinal`. The user drives input identically.
- Disconnected/reconnecting TUI: no spurious "turn failed" error (emitErr
  suppressed while disconnected); on reconnect the disconnect modal clears and
  `OnConnectionRestored` jumps to present (sessions + transcript refetch), so the
  final answer is surfaced, not silent. Missed completion notifications replay
  from the hub ring.
- The non-blocking send is invisible to the user (they keep typing/watching SSE,
  same as today's embedded experience) — the right thing is surfaced over SSE,
  not hidden in a now-discarded HTTP response body.

### (3) No regressions
- **`runLoop` semantics unchanged** except an extra emit-stamp when a turn id is
  present; sub-agents keep their no-op emitter. Checkpointer Begin/Commit, hooks,
  `persistSession`, token accounting all run unchanged inside the goroutine
  (identical to how the embedded path already runs them).
- **Busy gate:** release moves from handler-`defer` to `onDone`; net effect is
  the gate is held for the *full* turn even across disconnect — strictly more
  correct, and `HasBackgroundWork` gating (`api.go:330`) is unaffected.
- **Plan-mode restore** moves to `onDone` (must, else async would restore before
  the turn runs) — covered.
- **Tests:** `TestSendMessageBlocking` (`server_test.go:180`) asserts 200 +
  `messageView.Content` containing "fake model"; it **will need updating** to the
  new async contract (assert 200 + non-empty `acceptedView.TurnID`, and, if it
  wants the answer, subscribe to SSE and await `SessionEventFinal`). This is an
  intended contract change, not a silent break. `TestMarkBusyRejectsSecondClaim`
  (`:202`) is unaffected. The pre-existing environmental `TestUserSessionSendMessage`
  404 (no model endpoint) remains the only acceptable failure.
- gofmt/build/vet clean; golangci-lint v2 whole-repo 0 *new* issues; `go test
  ./...` (no `-race`, Pi5) green per the dev gate.

### (4) Holistic across both repos
- **turbotui:** untouched. Verified the coupling and all edits live in gogent
  (`internal/server`, `internal/gogent`, `internal/agent`, `ui/tui`). No turbotui
  file referenced; no go.mod bump.
- **webapi seam:** the inability to emit literal 202 is a webapi limitation; we
  respect the seam by staying on 200 + body rather than vendoring/patching a
  third module. Surfaced as an open question.
- **Right place for each change:** turn-id stamping lives in the agent layer
  (where events are emitted); async dispatch + id minting in the gogent core
  (where the embedded path's equivalent already lives); 202-style return + busy
  release in the server layer; response-shape + disconnect-suppression in the TUI
  client. Each concern sits at its natural layer; the core stays unaware of HTTP
  and of the server's busy map (it only takes an `onDone` callback).

---

## 5. Tests to add (per the issue)

In `internal/server` (httptest against the in-memory server + fake model):
- **Survives disconnect:** dispatch a turn, cancel the request context / drop the
  client, assert the turn still reaches `SessionEventFinal` on the hub.
- **Stop cancels an async turn:** dispatch, `POST /stop`, assert the turn ends
  with cancellation.
- **Busy held for full turn:** with a turn in flight, a second `POST
  …/messages` gets 409; after completion a send succeeds again.
- **202-shape:** the send response carries a non-empty `turnId`.
- **Events carry the turn id:** assert dispatched-turn `SessionEvent`s have the
  returned `TurnID`.
- **No regression when connected:** turn completes normally; final answer arrives
  as `SessionEventFinal` over SSE.

Plus update `TestSendMessageBlocking` to the async contract (above).

---

## 6. Files affected (gogent only — turbotui untouched, no go.mod bump)

- `internal/agent/user_session.go` — `TurnID` field on `SessionEvent` (`:100`);
  `WithTurnID`/`turnIDFrom` + turn-id key; emit-stamp wrap in `runLoop` (`:1201`);
  `EmitFinal`/`EmitError` helpers.
- `internal/gogent/gogent.go` (+ `internal/gogent/commands.go`) — `DispatchMessage`/
  `DispatchApprovedPlan`/`DispatchCommandSubtask` async wrappers; stdlib turn-id
  minter; synchronous methods retained.
- `internal/server/messages.go` — `Send` + `Stream` dispatch async, return
  `acceptedView`, busy/plan-mode release on completion.
- `internal/server/sessions.go` — `ApprovePlan` dispatch async + acquire
  `markBusy`, return `acceptedView`.
- `internal/server/wire.go` — `acceptedView` type.
- `internal/server/server_test.go` (+ new test file) — update blocking-send test;
  add the disconnect/stop/busy/turn-id tests.
- `ui/tui/api_client.go` — decode `acceptedView` (turn id) for send/approve.
- `ui/tui/remote_handlers.go` — disconnected flag; suppress `emitErr` on a send
  error while disconnected.

---

## 7. Open questions

1. **200 vs literal 202.** webapi cannot emit a custom success status without a
   dep change (§2). Default chosen: **200 + `acceptedView{turnId}`**. Acceptable,
   or does the maintainer want a follow-up webapi change (separate repo/PR) to
   return a real 202? *Recommendation: ship 200 now; track 202 as a webapi
   enhancement.*
2. **Turn-id format.** `"turn_" + crypto/rand hex(16)` (stdlib, no ULID dep).
   Sortable-by-time is not provided; if a future registry wants time-ordering we
   can prefix a `time`-based component. OK for v1? *Recommendation: yes.*
3. **In-flight turn registry.** Deferred as optional convenience (§3.2). Build the
   `map[turnID]…` + `GET …/turns/:id` now, or wait until a consumer needs it?
   *Recommendation: defer; the `onDone` callback already covers busy release.*
4. **`DispatchCommandSubtask` final-event emit.** Move the "surface subtask result
   as session final" from the server (`runCommandOverride` hub.deliver) into the
   core via `UserSession.EmitFinal`, so the async path needs no server hub. Agreed
   as the right layering? *Recommendation: yes.*
5. **ApprovePlan now busy-gated.** It is not today (`sessions.go:223`). Adding
   `markBusy` is needed for a correct async busy gate but is a behavioral
   tightening. Confirm acceptable. *Recommendation: yes — it matches the
   send/turn busy model.*
