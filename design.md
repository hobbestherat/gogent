# Design — Remote-TUI approval decisions fail silently (gogent #560)

Branch: `pair2/remote-tui-approval-decisions-fail-silen` · base HEAD `34cd2c3`
Scope: **gogent-only**, remote/attached mode only (`gogent --connect unix://|ssh://|tcp://`).
Embedded mode and turbotui are untouched. stdlib-first, no new deps, no go.mod bump.

## Problem (one bug, three symptoms)

When a user answers a remote permission prompt, the attached TUI POSTs the decision
to the daemon (`POST /api/approvals/:aid/decision`). Two independent defects make
that fail silently:

1. **The decision can be lost (Symptom A).** By the time the POST lands, the daemon's
   `approvalBridge` may have already removed the pending approval (5-min connected
   auto-deny under a deferred/typing modal #346, or a double-consumer race between two
   attached clients). `Decide` then returns `404 approval not found`. Because the lost
   answer was an `always` grant, `permission.Service.persist()` is never reached, no
   `network:<host>` key is written, and the next `web_fetch` to the same host
   re-prompts. The "always allow" never sticks.

2. **The error bleeds into the screen (Symptom B).** `RemoteClient.decide()`
   (`ui/tui/remote_handlers.go:611-613`) is fire-and-forget: on failure it calls
   `log.Printf`. The stdlib `log` package still writes to `os.Stderr` (fd 2), which
   gogent never redirects in attach mode, while turbotui holds the alternate screen
   (`ESC[?1049h`). The line paints over the frame, then the next redraw erases it — the
   "flash". This affects **all 13** `log.Printf` sites in `remote_handlers.go`
   (`:449,613,678,683,688,695,727,731,932,957,1112,1117`), not just the approval one.

3. **Bell-but-no-dialog (Symptom C).** There is no server-pushed approval SSE
   notification; the badge/dialog rely on the poller + local bell. This is the trigger
   that lets the modal sit unanswered long enough for defect (1) to fire. Addressing it
   is **optional** (see Open questions); it is not required by the acceptance gate.

## Confirmed cause

Both removal paths (`approvals.go` 5-min connected auto-deny `:197-199`; double-consumer
where one client's `resolve`+`defer remove` deletes the entry before the slower client's
POST) end identically: `get(aid) == nil` in `approvalsSvc.Decide`, hard 404, decision +
persist both lost. The fix is made **cause-agnostic** by reconciling a late decision on
the daemon rather than trying to win the race, so we do not have to prove which cause
dominates in a given session. (Repro during implementation will note which one we saw,
per the issue, but the fix covers both.)

## The fix — three coordinated changes

### Fix 1 — stop stdlib `log` bleeding onto the alternate screen (Symptom B)

Redirect the stdlib `log` package off `os.Stderr` to the existing diagnostics file sink
(`~/.gogent/gogent.log`, the same file `cmd/main.go:111-113` already uses for
`diag.NewFile`) in the attach entry path, before the TUI takes the alternate screen.

Files/functions:
- `internal/diag/logger.go` — add a tiny exported helper
  `func OpenLogFile(path string) (*os.File, error)` wrapping the existing unexported
  `openAppend` (same 0600/0700 perms). This lets a caller obtain the *same* append sink
  the `diag.Logger` writes to and point stdlib `log` at it, satisfying the "reuse the
  existing diagnostics logger sink" holistic gate without exposing `Logger` internals.
- `cmd/attach.go` — in `runAttached`, immediately before launching the `go wb.Run()`
  goroutine (`:235`), open the gogent.log file via `diag.OpenLogFile(filepath.Join(homeDir,
  ".gogent","gogent.log"))` and call `log.SetOutput(f)`. On open failure, leave the
  default (degrade gracefully, matching `main.go`'s fallback). The pre-TUI setup
  `log.Printf` calls (e.g. `remoteModelConfigs` `:267`) run before this point and still
  reach stderr while the normal screen is up — correct.
  - The post-loop `log.Printf("TUI error: %v", err)` (`attach.go:237`) fires *after*
    `wb.Run()` returns and the alternate screen is torn down; it will now land in the
    log file. Acceptable (it is a diagnostic, not user-facing); we will not restore
    stderr because the process is exiting anyway.

Why this is the right altitude: turbotui legitimately owns the alternate screen and
assumes the host app keeps fd 2 clean while it renders. gogent was violating that
assumption. The fix belongs on the gogent side — **no turbotui change**.

### Fix 2 — a remote permission decision always reaches the daemon (Symptom A)

Two layers, both needed: the daemon must make a *late* sticky grant stick (so "always
allow" survives the race), and the client must retry transient blips and surface a
genuinely-lost decision so the user is never misled.

**Fix 2a — daemon: idempotent, persist-on-late-arrival decision endpoint.**

- `internal/server/approvals.go`:
  - On `remove(id)`, before deleting, record a compact record of the concluded approval
    into a small bounded ring `recent map[string]concludedApproval` (+ FIFO eviction
    list), where `concludedApproval` keeps `{kind, sessionID, permission *permissionDetail}`.
    Cap ~64 entries (well above any realistic in-flight count); oldest evicted. This is
    the *only* added daemon state and it is guarded by the existing `b.mu`.
  - Add `func (b *approvalBridge) recall(id string) (concludedApproval, bool)`.
- `internal/server/approvals_handlers.go` `approvalsSvc.Decide`:
  - When `get(aid) == nil`: call `recall(aid)`.
    - found **and** `kind == "permission"` **and** decision parses to
      `DecisionAlways`/`DecisionAlwaysDeny` → persist it directly via
      `svc.s.g.GetPermissionService().Persist(permission.Action(rec.permission.Action),
      rec.permission.Resource, d)` and return a benign idempotent
      `{id, status:"resolved"}` (HTTP 200).
    - found but non-sticky (allow/deny/edit) → return idempotent 200 (the tool already
      concluded with the default; nothing to persist).
    - not found (truly unknown id, or evicted) → keep the existing `404` so the client
      learns it was genuinely lost (→ Fix 2b surfaces it).
  - When `get(aid) != nil` but `resolve(aid, d) == false` (a faster double-consumer
    already delivered): return idempotent 200 instead of the current `409` — the winning
    POST already applied and persisted the decision, so this is benign, not a conflict.
- `internal/permission/permission.go`:
  - Add `func (s *Service) Persist(a Action, resource string, d Decision)` that delegates
    to the existing unexported `persist`. This is the public seam the daemon uses to make
    a late `always` grant stick exactly as an in-time answer would. It writes the same
    `network:<host>` key, so `effect()`'s persisted-decisions cascade (`permission.go:330-343`)
    returns `EffectAllow` for every subsequent path on that host — host-scoped, all paths
    (criteria 1 & 3). The normal in-time path is unchanged: `CheckWithContext` still calls
    `persist` itself when the tool goroutine's `wait()` returns `Always`.

Why persist on the *late* path even though the original `web_fetch` already used the
default-deny: criterion 1 only requires *subsequent* fetches to be suppressed. The user's
intent ("always allow this host") is honored from the next request on, and survives a
daemon restart (criterion 2, via `permissions.json`). Persisting only `always`/`always_deny`
on this path preserves host-scoping and never broadens a one-shot `allow`.

**Fix 2b — client: retry transient failures, surface a genuinely-lost decision.**

- `ui/tui/remote_handlers.go`:
  - `decide(aid, decision string)` (`:611`): classify the `DecideApproval` error.
    - Transient (connection error / 5xx) → retry up to 2× with short backoff
      (e.g. 200ms, 500ms) before giving up. Bounded by `rc.ctx`.
    - With Fix 2a, a late sticky decision now returns 200, so the common 404 disappears.
      A *remaining* definitive failure (true 404 for an unknown id, or retries exhausted)
      → surface to the user (see below) instead of only `log.Printf`. The `log.Printf`
      stays as the diagnostic record (now safely in the log file via Fix 1), but it is no
      longer the *only* signal.
  - Thread `ap.SessionID` into the surface call (decide is called from `handleApproval`,
    which has it).
- Surface mechanism (user-visible, criterion 5): post a `[System]` note into the
  originating session window — "Approval could not be delivered to the daemon; the tool
  used the safe default and 'always allow' did not take effect. Try again." Add a small
  method on the `*Workbench` (e.g. `SystemNote(sessionID, text string)`) that marshals
  onto the event-loop thread and calls the existing `SessionWindow.addNote`
  (`session_window.go:1899`), and extend the `Approver` interface (or a sibling
  one-method interface the RemoteClient already holds via `wb`) with it. This reuses the
  existing `[System]` transcript rendering (`kindSystem`) and the workbench's thread
  marshaling — no new UI primitive, no turbotui change.

### Fix 3 — make permission persistence failures diagnosable (defense-in-depth)

`internal/permission/permission.go`:
- `persist` (`:424`) currently does `_ = s.write(data)`. Route `write`'s error to the
  diagnostics logger so a failed grant write is visible in `gogent.log` instead of
  silently dropped. The `Service` does not currently hold a `*diag.Logger`; add an
  optional `SetLogger(*diag.Logger)` (nil-safe, like the rest of diag) wired at
  construction (`internal/gogent/gogent.go:249` region, where the service is built) and
  log a `Warn` on `write`/`load`/`MarshalIndent` errors (`:424,448,462-471`). Keep it
  best-effort: a logging failure never changes permission behavior. Pure additive; if it
  threatens scope/cleanliness it is droppable without affecting criteria 1–5.

## User-facing behavior (after the fix)

- Granting **Always allow** for `example.com` writes `network:example.com → always` to
  `~/.gogent/permissions.json` on the daemon and suppresses the dialog for every
  subsequent `https://example.com/<any-path>` fetch in the session — **even if** the
  original prompt had already timed out or raced another client (the daemon reconciles
  the late grant). It survives a daemon restart. `other.com` is unaffected.
- **No screen flash**: every `remote_handlers.go` `log.Printf` now lands in `gogent.log`,
  never fd 2, while the TUI is up.
- If a decision genuinely cannot be delivered, the user sees a `[System]` note in the
  session telling them it failed and the safe default was used — never silent.

## Criterion-by-criterion

**(1) Goal match.** This is a *fix*, not a feature. It does exactly what #560 asks:
remote permission decisions reach the daemon, per-host `always` sticks across the session
+ restart and is host-scoped, no `log.Printf` bleeds into the TUI, and a failed POST is
retried-then-surfaced. The optional approval SSE push (Symptom C) is deliberately left
out of the required set to avoid scope creep.

**(2) Usability.** The user drives the decision through the same modal; the answer now
takes effect reliably. Failures are surfaced (`[System]` note) rather than swallowed.
No flash. The surface fires only on genuine loss (Fix 2a removes the spurious 404), so
it is not noisy.

**(3) No regressions.**
- Embedded mode is untouched: it installs the `*Workbench` as the prompter directly
  (`cmd/main.go:209`), never the `approvalBridge`/`decide` path, and persists
  synchronously via `CheckWithContext`. Fix 2a only affects daemon/headless mode.
- The permission cascade and host-scoping are unchanged; `Persist` reuses the existing
  `persist`/`effect` machinery and only ever writes `always`/`always_deny`.
- The in-time decision path is byte-for-byte the same; only the `get==nil` and
  `resolve==false` branches change (404/409 → idempotent), which is strictly more
  permissive toward the *user's already-expressed* intent, never toward the agent.
- `gofmt`/`build`/`vet`/`golangci-lint`/`go test ./...` must stay green; the pre-existing
  `TestUserSessionSendMessage` 404 and the load-induced `internal/daemon`
  `TestStopGracefulAndForced` flake (passes in isolation) are the only acceptable
  pre-existing failures. `ui/tui` keeps its forbidden-import set clean (we add no new
  imports there beyond stdlib `time`/existing ones; the surface uses existing types).

**(4) Holistic across both repos.** gogent-only. The alternate-screen contract is
turbotui's; the bug was gogent leaving stdlib `log` on fd 2 — fixed on the gogent side,
the correct repo, with no turbotui API change. The daemon↔client seam is respected: the
daemon owns persistence and now reconciles late decisions (the right place for
cross-client coordination, which the client cannot do); the client owns retry + the
local `[System]` surface (the right place for transport-level resilience and UX). No new
dependency, no go.mod bump; the log redirect reuses the existing diag sink.

## Regression risks & mitigations

- **Recall ring leak / unbounded growth** → fixed cap (~64) with FIFO eviction, under
  `b.mu`; entries are tiny. An evicted late POST falls back to 404 → surfaced (acceptable).
- **Double-persist race** (handler late-persist + tool-goroutine in-time persist) →
  `persist` is idempotent (same key, same value); harmless.
- **Idempotent 200 hiding a real error** → only `get==nil` *with a recall hit* and
  `resolve==false` become 200; a truly unknown id still 404s, so a real bug still
  surfaces.
- **Surface noise** → gated to genuine, non-recoverable failures only.
- **log redirect swallowing useful startup diagnostics** → redirect happens only at the
  `wb.Run()` seam; pre-TUI logs still reach stderr.

## Files touched (summary)

gogent:
- `cmd/attach.go` — `log.SetOutput` to gogent.log before `wb.Run()`.
- `internal/diag/logger.go` — add `OpenLogFile`.
- `internal/server/approvals.go` — `recent` ring + `recall`; record on `remove`.
- `internal/server/approvals_handlers.go` — late-arrival reconcile + idempotent decide.
- `internal/permission/permission.go` — `Persist` (public), optional `SetLogger` + error
  logging in `persist`/`write`/`load`.
- `internal/gogent/gogent.go` — wire the permission-service logger (Fix 3 only).
- `ui/tui/remote_handlers.go` — `decide` retry + surface; pass sessionID.
- `ui/tui/<workbench>.go` — add `SystemNote(sessionID, text)`; extend approver seam.

turbotui: **none**.

Tests (criterion 6):
- `internal/server` — remote-approval `always` round-trip: bridge → decide → persist →
  reload (new `permission.Service` from same dir) → `effect(ActionNetwork, host) ==
  EffectAllow`; and the **late-arrival** variant: `remove` the approval first, then
  decide `always` → assert persisted + idempotent 200; assert `other.com` still asks.
- `cmd` (or a small testable helper) — assert that after the attach log redirect,
  `log.Writer() != os.Stderr` (stdlib log goes to the diagnostics file, not fd 2).

## Open questions

1. **Symptom C (approval SSE push).** Adding a server-pushed approval notification
   (`internal/server/notify.go` + `ui/tui/tui.go:2686`) would make the badge/dialog appear
   instantly instead of within one 750ms poll, shrinking the window in which defect (1)
   can fire. It is **optional** per the issue and not in the acceptance gate. Recommend
   deferring to a follow-up to keep this PR a tight fix — confirm that's acceptable.
2. **Connected-timeout tuning.** Should we *also* stop counting the 5-min connected
   auto-deny clock until the prompt has actually been presented (a client→daemon
   "presented" signal)? That attacks the dominant cause directly but needs a new
   endpoint/state = scope creep. The recall/idempotent reconcile makes it unnecessary
   for correctness; leaving the timeout as-is. Flagging in case the maintainer prefers
   the timeout approach instead.
3. **Surface vs. re-arm.** Criterion 5 allows "re-armed prompt" as an alternative to a
   `[System]` note. A `[System]` note is simpler and non-disruptive; re-arming would
   re-present the modal. Going with the note unless the maintainer wants re-arm.
