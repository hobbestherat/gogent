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

**Plumb `TurnID` onto the wire (else the stamping is dead weight).** Stamping the
field is pointless unless a client can read it, and the issue explicitly requires
"SessionEvents carry the turn ID". Today the SSE shape is `eventView`
(`internal/server/wire.go:229`), built by `eventToView` (`:483`), and consumed
client-side as `EventDTO` (`ui/tui/api_client.go:190`) → `eventDTOToSessionEvent`
(`ui/tui/remote_handlers.go:347`). None carries a turn id (verified: a grep for
`TurnID`/`turn_id` across `internal/server` and `ui/tui` returns nothing). So:
- add `TurnID string \`json:"turn_id,omitempty"\`` to `eventView` and set it in
  `eventToView`;
- add the matching field to `EventDTO` and copy it through
  `eventDTOToSessionEvent`.

This makes the per-event stamping observable end-to-end — it is what the
"SessionEvents carry the turn ID" test actually asserts, and it is what lets the
Stream producer (§3.4) match the turn it dispatched. Without this plumbing the
§3.1 stamping would be invisible to every real consumer; we are not shipping it
half-wired.

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
(stamped with `turnID`).

**Single final-event source — no double final.** Once `DispatchCommandSubtask`
emits the subtask's final through the observer (→ hub → all subscribers), the old
server-side shim in `runCommandOverride` (`messages.go:67`,
`svc.s.hub.deliver(id, …Final…)`) becomes redundant. **`runCommandOverride` is
retired entirely** — *both* `Send` and `Stream` route subtask/agent turns through
`DispatchCommandSubtask`, so there is exactly one final emit (via the observer)
for every path. There is no "Stream keeps its own shim" option: keeping it would
make a streamed subtask emit two `SessionEventFinal`s (one from `EmitFinal`, one
from `hub.deliver`). This removes the earlier ambiguity.

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
client disconnect, and the busy gate must no longer release on disconnect. The
current producer (`:104-152`) does three things wrongly coupled to the client:
runs the turn under `stream.Context()` (`:131,134`), ties `defer release()` to
producer lifetime (`:117`), and uses a local `done` channel that closes when the
*turn goroutine it owns* returns (`:125-136`). All three move off the connection:

- **Dispatch async, not connection-scoped.** Replace the in-producer
  `go func(){ … SendMessage(stream.Context()) }()` with a call to the same
  `Dispatch*` methods (under `context.Background()`), passing
  `onDone = func(){ planRestore(); release() }`. Turn lifetime + busy gate are now
  owned by the daemon, not the stream.
- **Completion detection = terminal event on the subscription, not a local
  `done` channel.** The producer can no longer watch a goroutine it owns. Instead
  it watches its hub subscription (`sub`, from `subscribeSession`, `:89`) for the
  turn's terminal event. This is safe because **`runLoop` emits a terminal event
  on every exit path** (verified): `SessionEventFinal` on completion
  (`user_session.go:1645`), `SessionEventError` on `ctx.Err()` cancellation
  (`:1348-1349`) and on loop errors (`:1263,:1296`). The busy gate guarantees one
  turn per session at a time, so the next `final`/`error` on this session's
  subscription is this turn's; for precision the producer matches the dispatched
  `TurnID` (now on the wire, §3.1) before treating it as terminal. On that event
  the producer drains remaining buffered events (`drainRemaining`, `:158`) and
  returns.
- **Disconnect path unchanged in shape, decoupled in effect.** On
  `<-stream.Context().Done()` (`:140`) the producer returns and `unsub()`s
  (`:118`) — but the turn keeps running and `release` fires later from `onDone`.
  So a reconnecting client that sends gets 409 until the turn really finishes.
- **No `runCommandOverride` shim** (see §3.2): the subtask/agent branch
  (`:128-133`) dispatches via `DispatchCommandSubtask`; its final reaches the
  producer's subscription like any other event — no second emit.

This makes Stream a thin async-dispatch wrapper: it streams to whoever's
connected and stops streaming on disconnect (when it sees the turn's terminal
event, or when the client goes away), without affecting turn lifetime or the busy
gate.

### 3.5 Client (`ui/tui/api_client.go`, `ui/tui/remote_handlers.go`)

The client already runs sends on `context.Background()` goroutines
(`remote_handlers.go:531,561,791`) and already consumes progress + final answer
over the global SSE stream (`RemoteClient.consume`, `:199`). Two adjustments:

1. **Response shape (`api_client.go`).** `SendMessage`/`SendMessageWithOverrides`
   (`:415,424`) and `ApprovePlan` (`:496`) today decode a `MessageDTO` used only
   to detect failure. The body is now `acceptedView{turnId}`. Decode it into a
   small `acceptedDTO{TurnID string \`json:"turnId"\`}` and **return the turn id**
   (the callers in `remote_handlers.go` ignore the value today and branch only on
   `err`, so changing the return type is safe and gives a future correlation
   feature the id). The `EventDTO` gains the matching `turn_id` field (§3.1) so
   streamed events the client already consumes carry the correlation id.

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
- **Tests broken by the contract change (full accounting — these MUST be
  migrated, with how):**
  1. `TestSendMessageBlocking` (`server_test.go:180`) — asserts 200 +
     `messageView.Content` containing "fake model". Migrate to: 200 + non-empty
     `acceptedView.TurnID`; subscribe to the session SSE *before* sending and
     await the `SessionEventFinal` (assert its text + that it carries the returned
     `TurnID`). Doubles as the "events carry the turn id" + "connected client gets
     final over SSE" coverage.
  2. `TestSendRejectsNewTurnWhileAsyncSpawnRunsInBackground`
     (`background_state_issue353_test.go:71`) — today relies on `serveOne(first)`
     **blocking until the foreground turn completes** before it checks the 409.
     After the change `first` returns instantly and the foreground turn outlives
     the test, racing `defer backend.Close()`/`defer close(releaseChild)` and
     leaking a goroutine. Migrate to make the #353 invariant explicit: subscribe
     to SSE; POST `first` (now 200+turnId); await `childArrived`; **await the
     foreground turn's `SessionEventFinal` over SSE** (proves `onDone` fired and
     `release()` ran, so the only remaining hold is `HasBackgroundWork`); THEN
     POST `second` and assert 409 — this now precisely tests the
     background-work busy gate the test is named for. Finally `close(releaseChild)`
     and drain the child's terminal event before returning, so no goroutine
     outlives teardown.
  3. `TestStreamRejectsConcurrentTurnWhileForegroundRuns`
     (`background_state_issue353_test.go:135`) — the busy assertion still holds
     (release moves from producer-lifetime to `onDone`, and the turn is blocked on
     `releaseFirstModel`, so 409 stands), **but its `<-r.Context().Done()` branch
     (`:149`) becomes unreachable via client disconnect** once the model call runs
     under `context.Background()`. Migrate by: keeping it as the Stream busy-gate
     regression test (drop the now-dead `r.Context().Done()` expectation / leave
     it only as the backend's unblock-on-shutdown path), and add the new
     disconnect-survival Stream test in §5 to cover what this one no longer can.
  4. `ui/tui/remote_client_phase2_test.go:47,81-83` — the fake daemon returns
     `MessageDTO{Content:"ok"}` and asserts `msg.Content == "ok"`. Update the fake
     to return `{"turnId":"turn_…"}` and assert the client surfaces that turn id
     (the real contract), since `SendMessage` now returns the turn id rather than
     message content.
  `TestMarkBusyRejectsSecondClaim` (`server_test.go:202`),
  `TestStopEndpointCancelsAsyncBackgroundSubAgents`
  (`background_state_issue353_test.go:202`, drives the session API directly, not
  the blocking send) and the other `background_state` state tests are unaffected.
  The pre-existing environmental `TestUserSessionSendMessage` 404 (no model
  endpoint) remains the only acceptable failure.
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
- **Layering note (accepted):** `UserSession.EmitFinal`/`EmitError` add a second
  final-emit path alongside the embedded subtask path's `wb.EmitSessionEvent`
  (`cmd/embedded_handlers.go:446`). Both ultimately drive the same observer/hub
  fan-out, so this is a cosmetic duplication, not a divergence; called out so a
  later cleanup can unify them. No behavioral difference in v1.

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

Stream-path coverage (the endpoint with the largest behavioral change — §3.4 —
currently has only the busy-gate test):
- **Stream survives client disconnect:** open `POST …/messages/stream`, await the
  foreground model arriving, **close the client connection mid-turn**, and assert
  via an independent `/events` subscriber that the turn still reaches
  `SessionEventFinal` and that a concurrent `POST …/messages` stays 409 until that
  final, then succeeds — i.e. busy released on turn completion, not on disconnect.
- **Stream subtask emits exactly one final:** a subtask/agent stream produces a
  single `SessionEventFinal` (guards the §3.2 double-final removal).

Plus the four migrations in §4(3) (`TestSendMessageBlocking`,
`TestSendRejectsNewTurnWhileAsyncSpawnRunsInBackground`,
`TestStreamRejectsConcurrentTurnWhileForegroundRuns`, and the
`remote_client_phase2_test.go` Content assertion).

---

## 6. Files affected (gogent only — turbotui untouched, no go.mod bump)

- `internal/agent/user_session.go` — `TurnID` field on `SessionEvent` (`:100`);
  `WithTurnID`/`turnIDFrom` + turn-id key; emit-stamp wrap in `runLoop` (`:1201`);
  `EmitFinal`/`EmitError` helpers.
- `internal/gogent/gogent.go` (+ `internal/gogent/commands.go`) — `DispatchMessage`/
  `DispatchApprovedPlan`/`DispatchCommandSubtask` async wrappers; stdlib turn-id
  minter; synchronous methods retained.
- `internal/server/messages.go` — `Send` + `Stream` dispatch async, return
  `acceptedView`, busy/plan-mode release on completion; **delete
  `runCommandOverride`** (subtask final now emitted by the core via the observer).
- `internal/server/sessions.go` — `ApprovePlan` dispatch async + acquire
  `markBusy`, return `acceptedView`.
- `internal/server/wire.go` — `acceptedView` type; `TurnID` field on `eventView`
  + set it in `eventToView`.
- `internal/server/server_test.go`, `internal/server/background_state_issue353_test.go`
  (+ a new Stream test file) — migrate the four affected tests (§4(3)); add the
  disconnect/stop/busy/turn-id and Stream-disconnect/single-final tests (§5).
- `ui/tui/api_client.go` — decode `acceptedView` (return turn id) for
  send/approve; `TurnID`/`turn_id` on `EventDTO`.
- `ui/tui/remote_handlers.go` — disconnected flag (`atomic.Bool` set in
  `notifyLost`/cleared in `notifyRestored`); suppress `emitErr` on a send error
  while disconnected; copy `turn_id` through `eventDTOToSessionEvent`.
- `ui/tui/remote_client_phase2_test.go` — update fake response + assertion to the
  `{turnId}` contract.

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
   (This is the *only* genuinely-optional piece; the turn-id wire plumbing and the
   single-final emit are now in scope, not deferred.)
4. **ApprovePlan now busy-gated.** It is not today (`sessions.go:223`). Adding
   `markBusy` is needed for a correct async busy gate but is a behavioral
   tightening. Confirm acceptable. *Recommendation: yes — it matches the
   send/turn busy model.*
