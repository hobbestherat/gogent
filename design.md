# Design — CONNECT-ORDER-SSE-AFTER-RESTORE (gogent issue #516)

> Attach: the global SSE stream opens before the first `Restore()` completes,
> flooding the UI on first connect.

## Summary of the chosen approach

**Direction A — gate the *delivery* of the live stream to the UI until the initial
`Restore()` completes.** Concretely: split `RemoteClient.Start` so the
fail-fast `openStream()` still happens synchronously (an unreachable/denied daemon
must still abort before the TUI launches), but `consume()` — the goroutine that
actually pumps daemon events into `Workbench.EmitSessionEvent` → `desktop.Post` —
is **not launched until the workbench signals that its initial restore loop has
finished.** `cmd/attach.go` wires that signal: `Run()` invokes an
`afterRestore` hook at the end of its restore block, and that hook is the closure
that begins consuming.

This is Direction A from the brief (order the connect sequence), not Direction B
(coalesce/drop early events): ordering the start of consumption is simpler, has a
smaller blast radius, and fully removes the flood at the source rather than
filtering it downstream.

### Why this is safe even though the socket opens early

The daemon hub (`internal/server/hub.go:86 deliver`) sends **non-terminal events
non-blocking with drop-on-full** (global subscriber buffer = 128,
`hub.go:237`; client-side `StreamEvents` buffer = 64, `api_client.go:873`).
Terminal events get a bounded 250ms blocking attempt then drop. So a stream that
is *open but undrained* during `Restore()`:

- **never back-pressures the daemon or other connected clients** (drop-on-full),
- buffers at most ~`128 + 64` events, after which excess non-terminal events are
  dropped server-side ("best-effort live" — by design),
- yields a **bounded** post-restore burst (≤ ~192) that `consume()` drains *after*
  Restore is done, i.e. while the UI thread is free and all windows already exist.

The remaining buffered events are real deltas that occurred during restore;
draining them brings the just-restored windows to present — no double-apply,
because they are distinct live events that post-date the `/sessions` snapshot.

## Exact files / functions touched (gogent only)

### 1. `ui/tui/remote_handlers.go`
- **Refactor `Start(parent)` internals** (currently `~:212`, `startOnce.Do` does:
  lifetime goroutine → `openStream()` + `go consume()` + `go monitorHealth()` →
  `go pollApprovals()`). Extract the connect work into a shared path that returns a
  `begin` closure which launches `go rc.consume(events)` + `go rc.monitorHealth()`.
- **`Start` keeps its exact current behavior**: it runs the shared connect path and
  then calls `begin()` immediately. *All existing tests that call `rc.Start(ctx)`
  and expect `consume`/reconnect to run stay green unchanged.*
- **New method `StartGated(parent context.Context) (begin func(), err error)`**:
  runs the lifetime goroutine + synchronous `openStream()` (fail-fast, stashes the
  channel in a new field `initialEvents`) + `go pollApprovals()`, but **defers**
  `consume` + `monitorHealth`, returning them packaged in `begin`. `begin` is
  idempotent (guarded by a new `consumeOnce sync.Once`) and safe to call from any
  goroutine. If `sink == nil`, `begin` is a no-op (mirrors today's
  `if rc.sink != nil` guard).
- **New fields**: `initialEvents <-chan GlobalEventDTO`, `consumeOnce sync.Once`.
  (`sync` is already imported via `startOnce`; no new deps.)
- `consume()`, `reconnect()`, `openStream()`, `dropStream()`, `monitorHealth()`,
  `notifyRestored()`, `kickApprovals()` — **bodies untouched.** Only *when*
  `consume` starts changes. `monitorHealth` is deferred into `begin` precisely so it
  cannot `dropStream()` the stashed-but-undrained stream before consumption starts.

### 2. `ui/tui/tui.go`
- **New field `afterRestore func()`** on `Workbench` + setter
  `SetAfterRestore(fn func())`.
- In **`Run()`** (`~:2782`), after the restore block + the empty-session fallback
  (`~:2837`, so the workbench is fully populated) and before `SetUnhandledKeyFn`,
  add: `if w.afterRestore != nil { w.afterRestore() }`. Nil hook → no-op, so the
  embedded (non-attach) path is unaffected.

### 3. `cmd/attach.go`
- Replace `rc.Start(ctx)` (`:217`) with
  `begin, err := rc.StartGated(ctx)`; identical error handling
  (`rc.Close()`; wrap "start remote event stream").
- Before launching the `wb.Run()` goroutine (`:223`), add
  `wb.SetAfterRestore(begin)`.
- The header comment block (`:211-216`) is updated: the *connect* still happens
  before the loop (fail-fast preserved); *consumption* now begins after the first
  Restore.

### turbotui
**No change.** turbotui owns the desktop/`Post` event loop and dialog rendering;
gogent owns the connect ordering. The seam is respected — we reduce *what gogent
posts during restore*, we do not touch how turbotui drains the post queue. No
`go.mod` bump, no new dependency.

## User-facing behavior

- On `gogent --connect` (incl. `ssh://`): the first frame paints from the restored
  sessions promptly; keystrokes are responsive immediately because the UI thread is
  no longer contending with a live event flood while `Restore()` grinds through slow
  sequential round-trips. Live updates begin streaming a moment later, once restore
  has finished — visually a brief, expected catch-up rather than a freeze.
- A genuinely unreachable/denied daemon still fails fast with the same error and the
  TUI never launches (the synchronous `openStream()` in `StartGated`).
- No new UI surface, prompt, or setting — this is a sequencing fix, not a feature.

## Test plan (`ui/tui/connect_order_issue516_test.go`, mirrors existing style)

Uses an httptest SSE server in the style of `ssh_tunnel_issue482_test.go` /
`daemon_lifecycle_issue358_test.go`; keeps `ui/tui` free of
`internal/daemon/server` imports.

1. **Gating / ordering (the core pin).** Build `rc` with a recording sink (mutex +
   ordered log). Call `begin, err := rc.StartGated(ctx)`. Have the server emit an
   event. Assert the sink is **not** called (count stays 0 across a short poll) —
   the stream is open but delivery is gated. Then call `begin()` and `waitFor` the
   sink to receive it. This pins "Restore-done (begin) precedes first sink
   delivery," exactly the issue's acceptance.
2. **Reconnect intact on the gated path.** After `begin()`, close the server's
   first stream to force a drop; assert the reconnector's `OnConnectionRestored`
   fires and a post-reconnect event is delivered (jump-to-present). Confirms gating
   does not regress reconnect.
3. **Regression coverage by reuse.** The existing reconnect suites
   (`ssh_tunnel_issue482_test.go`, `daemon_lifecycle_issue358_test.go`,
   `reconnect_skip_unchanged_issue520_test.go`) call `rc.Start(ctx)`, whose behavior
   is unchanged, so they continue to exercise re-subscribe + jump-to-present and
   must stay green.

(The `Run()` → `afterRestore` → `begin` wiring is not unit-tested headlessly — the
desktop post-queue has no headless drain — but the gating contract is pinned at the
`StartGated`/`begin` seam per the issue's guidance, and the wiring is covered by
build/vet + manual reasoning.)

## Design criteria

**(1) Goal match.** Exactly the issue's ask: the global SSE stream's *delivery to
the UI* is gated until the initial `Restore()` completes. No scope creep (pure
sequencing change; `consume`/`reconnect` bodies untouched), nothing missed (fail-
fast and reconnect both preserved).

**(2) Usability.** First connect becomes interactive promptly; no keystroke lag from
event flooding. Failure modes are still surfaced (fail-fast error unchanged;
reconnect modal unchanged). Nothing is silently swallowed on the *client* — the only
drops are the daemon's pre-existing best-effort-live drops, bounded and by design.

**(3) No regressions.** `Start()` behavior is byte-for-byte preserved, so every
existing `Start`-based test stays green. Reconnect re-subscribe + jump-to-present is
untouched (only consumption *start time* moves). Embedded path unaffected (nil
`afterRestore`). The gated undrained stream cannot stall the daemon or other clients
(hub drop-on-full). `gofmt`/`build`/`vet`/whole-repo `golangci-lint`/`go test ./...`
expected green (pre-existing `TestUserSessionSendMessage` 404 the only accepted
failure, per [[dev-gate]]).

**(4) Holistic across both repos.** Change lives entirely in the right place —
gogent's connect/run handshake (`cmd/attach.go` + `ui/tui/remote_handlers.go` +
`ui/tui/tui.go`). turbotui (the rendering/desktop lib) is untouched and the seam is
respected: gogent decides *when to start emitting*; turbotui decides *how to drain*.
No new deps, no `go.mod` bump, no turbotui change. Downstream effect on turbotui
(post-queue pressure during restore) is *reduced*, never increased.

## Regression risks (and mitigations)

- **Existing `Start`-based tests break.** *Mitigated:* `Start` is unchanged; only a
  new `StartGated`/`begin` path is added.
- **`begin` never fires → live updates never start.** *Mitigated:* `afterRestore` is
  invoked unconditionally at the end of `Run`'s restore block (covers even an
  empty/no-session restore); `begin` is `sync.Once`-idempotent.
- **`monitorHealth` dropping the stashed stream before consume.** *Mitigated:*
  `monitorHealth` is deferred into `begin`, so it only runs once consumption starts.
- **Approvals during the restore window.** `pollApprovals` deliberately stays in the
  `StartGated` (early) path — it is a *bounded poll*, not a flood, and the issue is
  scoped to the SSE event stream. An approval pending at connect is still picked up
  promptly. (Called out as a deliberate scope decision, not an oversight.)
- **Buffered stale burst after restore.** *Mitigated:* bounded by hub/client buffer
  sizes (drop-on-full); drained while the UI thread is free and windows exist.

## Open questions

1. **Hook naming/shape.** `Workbench.SetAfterRestore(func())` vs. adding the
   callback to the `Handlers` struct (e.g. `Handlers.AfterInitialRestore`). I chose a
   dedicated `Workbench` setter because `Handlers` is assembled in `cmd/attach.go`
   *before* `StartGated` produces `begin`, so a `Handlers` field would force a
   re-`SetHandlers` after `StartGated`; a setter is lower-friction and embedded-safe
   (nil → no-op). Open to folding it into `Handlers` if reviewers prefer one struct.
2. **Reuse the stashed stream vs. close-and-reopen-fresh after restore.** I reuse the
   already-opened (fail-fast) stream and drain it post-restore — simplest, and the
   buffered events are real deltas worth applying. An alternative is to close the
   fail-fast probe stream and open a *fresh* one after restore (a clean
   jump-to-present, zero backlog), at the cost of a second subscribe. Given the
   bounded backlog, reuse seems clearly better; flagging in case a strict
   jump-to-present-on-first-connect is desired.
3. **`StartGated` vs. a flag on `Start`.** A `rc.DeferInitialConsume()` flag +
   unchanged `Start` signature is an alternative, but a distinct `StartGated`
   returning `begin` keeps the deferral explicit and the existing `Start` contract
   pristine. Confirm the separate-method API is acceptable.
