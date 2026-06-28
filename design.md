# Design — Remote TUI permission ⏳ badge lost on disconnect / connected-timeout (issue #569)

> Closes #569. gogent-only. stdlib-first, no new deps, no go.mod bump. Embedded
> mode untouched. Remote/attached mode (`gogent attach ssh://|unix://|tcp://`) only.

## 1. Root cause

In attached mode the badge + dialog are driven exclusively by
`RemoteClient.handleApproval` (`ui/tui/remote_handlers.go:667`), which only runs
once the poller (`scanApprovals`, every 750 ms, or a reconnect `kickApprovals`)
observes the approval in `GET /api/approvals`. The badge is set inside the
`Workbench.AskPermission` that `handleApproval` calls (`markApproval(+1)`,
`permission_dialog.go:127`). So **if the pending approval is removed from the
daemon before any client fetch observes it, `handleApproval` never runs → no
badge, no dialog, and the tool silently receives `DecisionDeny`.**

The daemon removes a pending approval prematurely because of how the
connected-timeout is charged (`internal/server/approvals.go:191` `wait`):

- The **connected clock** (5 min, "connected-but-unresponsive auto-deny") starts
  accruing the instant `wait()` begins ticking — i.e. at `alloc()`, **before any
  client has fetched/presented the prompt**. The clock's *semantic* is "a human
  is connected and could answer but is unresponsive," yet it is charged against
  time during which no human was ever shown the prompt.
- Consequences matching the two failure modes in the issue:
  - **Half-open / stalled link:** `clientCount() > 0` (the SSE subscriber is
    still registered) but the client cannot actually poll. The 5-min connected
    clock runs to completion → auto-deny → removal → badge never appears. The
    health monitor (`monitorHealth`, 2 failed pings) mitigates but races it.
  - **Discovery-vs-removal race:** any path where the first successful
    `/approvals` fetch lands after the connected clock has elapsed (a long
    series of brief reconnects, a slow/long-blocked poll, a backlogged UI
    thread) loses the prompt before it is ever surfaced.

A clean transient disconnect is *already* mostly handled (clientCount→0 →
unattended 1 h clock; reconnect `kickApprovals` re-scans), but it still depends
on the prompt surviving until the next fetch, and on the 750 ms poll/kick
catching it — exactly the dependency acceptance criterion 3 forbids.

## 2. Fix overview (two layers, both gogent-only)

**Layer A — daemon: don't charge un-presented time against the connected clock
(load-bearing correctness fix).** Track per-approval `observed` state. The
connected (5-min) auto-deny clock only accrues once a client has actually
fetched the approval via `GET /approvals`. Until observed, a *connected* prompt
is treated as un-attended (governed by the long 1 h safety bound), so it is
**never auto-denied before a human client has had the chance to surface it.**
This alone guarantees the prompt is still pending whenever the poll, the
reconnect kick, or the push (Layer B) finally fetches it → the badge reliably
appears. It directly implements the issue's primary suggested direction ("only
start the connected-timeout once a client has actually observed/presented the
prompt").

**Layer B — daemon→client: push a lightweight "approval pending" SSE signal so
the badge/dialog appear immediately, not on the next 750 ms tick (latency +
robustness).** `alloc()` broadcasts a dedicated `approval` SSE frame on the
global stream; the attached client treats it as an out-of-band
`approvalKick` (immediate `scanApprovals`). Best-effort (drop-on-full,
non-blocking) — correctness still rests on Layer A + the authoritative
`/approvals` fetch; the push only removes the up-to-750 ms latency and the
"poll endpoint flaky while stream is up" edge.

**Layer C — client: never silently deny (usability).** Build on #560's
`decide()`/`reportDecision` surface path: extend `reportDecision` so a `late`
status on a one-shot `allow`/`deny` (not just `always*`) also emits a `[System]`
notice, so if the connected-timeout legitimately fires *after* presentation
(user genuinely unresponsive 5 min) and the user answers afterward, they are
told the tool used the safe default rather than seeing nothing.

Layer A satisfies acceptance criteria 1–3 on its own; B and C complete criteria
2 (explicit notice alternative), 3 (no poll dependence) and 5 (SSE path test).

## 3. Exact changes

### gogent — `internal/server/approvals.go` (Layer A, exclusive lane)
- Add `observed bool` to `pendingApproval` (guarded by `b.mu`).
- `list()`: mark every returned approval `observed = true` under `b.mu`. `list()`
  is the discovery API — its only real caller is the `GET /approvals` handler
  (`approvals_handlers.go:16`); a successful list **is** the client observing the
  prompt. (Verified: no non-client internal caller of `list()`.)
- Add helper `func (b *approvalBridge) isObserved(id string) bool` (reads under
  `b.mu`).
- `wait()`: change the connected branch so the connected clock accrues **only
  when the approval is observed**. Effective rule per tick:
  - connected **and** observed → `connectedTimeout` governs (reset
    `unattendedFor`). *(unchanged from today, but the clock now starts at
    observation, not creation.)*
  - connected **and not** observed → treat as unattended: accrue
    `unattendedTimeout` (1 h safety), hold `connectedFor = 0`. The prompt is
    never auto-denied by the 5-min clock before a human client has fetched it.
  - not connected → `unattendedTimeout` (unchanged).
  - `connectedFor` resets on any transition out of (connected ∧ observed), so the
    "fresh connected window on reconnect" invariant (#358) is preserved.
- `now`/clock injection unchanged. The fast-path (both bounds ≤ 0 → block
  forever) is unchanged.

### gogent — `internal/server/hub.go` + `events.go` + `approvals.go` (Layer B)
- `hub`: add `broadcastApprovalSignal()` — a non-blocking, drop-on-full fan-out
  to global subscribers of a `taggedEvent` tagged as an approval signal (new
  `approvalSignal bool` on `taggedEvent`, mutually exclusive with `notif`). **Not
  ring-buffered**: a reconnecting client already runs `kickApprovals`, and
  `/approvals` is authoritative, so no replay buffer is needed (and buffering
  would risk a stale wake after the approval resolved — harmless but pointless).
- `events.go` `globalSSE`: emit an approval-signal frame under a new SSE event
  name `approval` with a minimal body (`{"id": "..."}` for diagnostics; the
  client ignores the body and just re-scans).
- `approvals.go` `alloc()`: after registering the pending approval, call
  `b.hub.broadcastApprovalSignal(id)` (nil-hub guarded, as the bridge already is
  in `wait`).

### gogent — `ui/tui/api_client.go` + `remote_handlers.go` (Layer B, client)
- `api_client.go`: add `SetApprovalSignalHandler(func())` (mirrors
  `SetNotificationHandler`, mutex-guarded). In the `StreamEvents` reader loop,
  recognize the `approval` event name and invoke the handler, `continue`
  (analogous to the existing `notification` branch). Add `const
  approvalEventName = "approval"`.
- `remote_handlers.go`: in `StartGated`, when `rc.approver != nil`, wire
  `rc.client.SetApprovalSignalHandler(rc.kickApprovals)` alongside launching
  `pollApprovals`. This keeps the wiring self-contained in `ui/tui` — **no
  `cmd/attach.go` change** — reusing the existing `approvalKick` channel and
  `seen`-dedup so a push and a concurrent poll/reconnect can never double-present
  (the poller goroutine remains the sole owner of `seen`).

### gogent — `ui/tui/remote_handlers.go` `reportDecision` (Layer C, client)
- Extend the `status == "late"` branch so a one-shot `allow`/`deny` also emits a
  concise `[System]` notice (e.g. "Your decision arrived after the prompt
  closed; the tool used the safe default (deny)."). The existing `always*` notice
  is unchanged. No new endpoint; rides #560's `decide()` return.

### Not touched
- `cmd/daemon.go`, `cmd/main.go`, `cmd/handoff.go`, `cmd/attach.go` — bridge
  wiring + timeouts unchanged; the client-side signal handler self-wires in
  `ui/tui`.
- `internal/server/notify.go` `notificationForEvent` — **left as-is.** An
  earlier suggested direction was to add an approval case here, but approvals are
  not `agent.SessionEvent`s, and routing them through the *notification* path
  would (a) ring-buffer them as desktop notifications and (b) double-notify,
  since `Workbench.AskPermission` already raises the `approval` desktop
  notification when `handleApproval` runs. The dedicated `approval` SSE signal
  (Layer B) is the correct seam instead.
- `permission_dialog.go`, `sidebar.go`, `tui.go` — badge/dialog rendering is
  unchanged; we only change *when* `handleApproval` runs and *when* the daemon
  auto-denies.

## 4. User-facing behavior

- A permission prompt raised while the attached TUI is briefly disconnected stays
  pending on the daemon (un-observed ⇒ not auto-denied; or clientCount→0 ⇒ 1 h
  unattended bound) and is surfaced as a ⏳ badge + dialog on reconnect — via the
  reconnect `kickApprovals`, and immediately via the `approval` push once the
  stream is back.
- A prompt that sits across the would-be 5-min connected window before any client
  fetch is **no longer auto-denied** before presentation: the 5-min clock only
  starts once a client has fetched it. The user always sees the badge + dialog.
- Once the user *has* been shown the dialog, the normal 5-min
  connected-but-unresponsive auto-deny still applies (unchanged). If it fires and
  the user answers afterward, they now get an explicit `[System]` notice that the
  safe default was used (Layer C) — never a silent deny.
- Badge appears for every remote approval regardless of poll timing, because the
  approval cannot vanish before it is observed.

## 5. Design-criteria assessment

**(1) Goal match.** It is a *fix*, not a feature/refactor: it makes the existing
badge/dialog path reliable by (A) not charging un-presented time against the
connected auto-deny and (B) pushing discovery off the SSE stream. No new UI, no
scope creep; the dialog and badge widgets are unchanged. Directly implements the
issue's named directions.

**(2) Usability.** The badge + the same modal the embedded path uses appear for
every prompt; the user drives the decision exactly as before. The connected
auto-deny only counts time after the user could actually see the prompt. When a
decision genuinely can't be delivered in time, Layer C surfaces a `[System]`
notice — the user is told, never silently denied.

**(3) No regressions.**
- All existing `approvals_issue358_test.go` tests stay green: each discovers the
  pending approval via `bridge.list()` (which now marks `observed`) *before*
  asserting connected-timeout behavior, so the connected clock starts as those
  tests expect. Walked through all 7 cases — connected-auto-deny, unattended
  wait, shorter-unattended-cap-ignored, disconnect-switches-to-unattended,
  reconnect-fresh-window, unattended-answer-after-reconnect, unattended-safety —
  none change outcome. The behavior change manifests *only* for a connected
  client that has **not** yet called `list()`, a path no existing test exercises.
- #560 idempotent decide/recall path is unchanged; Layer C only adds a notice on
  an already-returned `late` status.
- SSE push is non-blocking drop-on-full → no agent/daemon stall; a dropped signal
  is backstopped by the 750 ms poll and the reconnect kick. The `seen` dedup
  keeps push+poll from double-presenting.
- Embedded/local path (Workbench is the Prompter directly) never touches the
  bridge or the remote client — entirely unchanged. Host-scoping/auth on
  `/approvals` unchanged.

**(4) Holistic / repo seam.** turbotui (the widget library: `Desktop`, `Dialog`,
`Layer`, badge glyph) needs **zero changes** — the seam is respected: we change
gogent's daemon approval lifecycle and gogent's remote client orchestration, not
any widget. The reviewed read-only `$HOME/work/turbotui` clone confirms the
dialog/badge primitives are generic and unaware of approvals. No new deps, no
go.mod bump. Change lives in the right place: the *correctness* fix is in the
daemon that owns the auto-deny timer (`internal/server/approvals.go`); the
*latency* fix bridges the existing hub→SSE→client seam already used for
notifications; the *notice* lives where #560 put decision-surfacing.

## 6. Regression risks (called out)
- **`list()` now has a side effect (marks observed).** Acceptable: its only real
  caller is `GET /approvals`; a peer-scoped automation polling it would start the
  5-min clock early, which merely reverts to today's behavior (clock from
  observation) — strictly no worse, and the human TUI is the normal caller.
- **A connected client that fetches but never presents** would keep a prompt
  alive up to the 1 h unattended bound instead of 5 min. This is the *intended*
  semantic (no human was shown it), and the 1 h safety bound still denies
  eventually. Documented in the `wait()` comment.
- **Concurrency:** `observed` is read in `wait()`'s tick loop and written in
  `list()`; both go under `b.mu` (`wait` already locks `h.mu` via `clientCount()`
  each tick, so one more short `b.mu` read is negligible at the ≤1 s poll
  interval). `taggedEvent.approvalSignal` is mutually exclusive with `notif`;
  per-session subscribers never receive it (global-only, like notifications).

## 7. Tests to add (acceptance criterion 5)
- `internal/server/approvals_issue569_test.go`:
  - Connected client (`subscribeGlobal`) + short `connectedTimeout`, **never call
    `list()`** → assert the approval is still pending well past
    `connectedTimeout`; then call `list()` (observe) and assert it now auto-denies
    within `connectedTimeout` (the connected clock starts at observation).
  - `alloc()` broadcasts an `approval` signal frame to a global subscriber
    (assert the frame is received on the subscriber channel).
- `internal/server/events_*_test.go` (or extend existing): the `approval` frame
  serializes under SSE event name `approval` and is not delivered to per-session
  subscribers.
- `ui/tui/remote_handlers_*_test.go`:
  - An `approval` signal handler invocation triggers an immediate `scanApprovals`
    that presents a prompt raised "during a disconnect" (badge/dialog surfaced
    without waiting for the poll tick).
  - Badge/approval surfacing across a simulated disconnect → reconnect
    (`kickApprovals`) and across the connected-timeout boundary, using a fake
    `Approver` to assert `AskPermission` is called exactly once (dedup holds).
  - `reportDecision` emits a `[System]` notice on a `late` one-shot `allow`/`deny`.

## 8. Gate / sequencing
- Spans `ui/tui` (`remote_handlers.go`, `api_client.go`) **and** `internal/server`
  (`approvals.go`, `hub.go`, `events.go`, `approvals_handlers.go`) → exclusive in
  both lanes. Serialize **after** #562 (shared `remote_handlers.go`,
  internal/server) and run sequentially vs #570 (shared `remote_handlers.go` /
  `api_client.go`). Rebase onto current `origin/main` at the gate.
- Gate: gofmt/build/vet clean; golangci-lint whole-repo 0 NEW; `go test ./...`
  green (tests without `-race`, per the Pi5 dev gate; pre-existing
  `TestUserSessionSendMessage` 404 and the load-induced `internal/daemon`
  `TestStopGracefulAndForced` flake are the only acceptable failures). `ui/tui`
  free of forbidden imports.

## 9. Open questions
- **Forcibly dismissing an already-open dialog on a real auto-deny.** With Layer
  A, auto-deny before presentation no longer happens; the only remaining auto-deny
  is *after* presentation (user unresponsive 5 min), which Layer C surfaces when
  the user eventually answers. Actively tearing down a still-open modal from the
  daemon side (e.g. on a daemon-pushed "approval resolved" frame) is a larger UX
  change and is **out of primary scope** — flagging in case the maintainer wants
  the dialog to auto-dismiss + show the notice the instant the daemon auto-denies,
  rather than only when the user clicks. Recommend deferring unless requested.
- **Observation granularity.** We mark observed on `GET /approvals` (list fetch),
  not on actual dialog presentation. A dedicated "presented" ack endpoint would be
  more precise but adds API surface + a client round-trip and overlaps the
  contested `remote_handlers.go`. List-fetch observation is sufficient (the client
  *will* present once it has fetched) and minimal; confirm this granularity is
  acceptable.
