# Design — Remote-TUI approval decisions fail silently (gogent #560)

Branch: `pair2/remote-tui-approval-decisions-fail-silen` · base HEAD `34cd2c3`
Scope: **gogent-only**, remote/attached mode only (`gogent --connect unix://|ssh://|tcp://`).
Embedded mode and turbotui are untouched. stdlib-first, no new deps, no go.mod bump.

> Revised after design review. Resolutions to the six raised concerns are inlined and
> tagged **[R1]–[R6]**.

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

2. **The error bleeds into the screen (Symptom B).** On a failed POST,
   `RemoteClient.decide()` (`ui/tui/remote_handlers.go:611-613`) calls `log.Printf`.
   The stdlib `log` package still writes to `os.Stderr` (fd 2), which gogent never
   redirects in attach mode, while turbotui holds the alternate screen (`ESC[?1049h`,
   *defined* `app.go:922`, *written* `app.go:981`). The line paints over the frame, then
   the next redraw erases it — the "flash". This affects **all 13** `log.Printf` sites in
   `remote_handlers.go` (`:449,613,678,683,688,695,727,731,932,957,1112,1117`).

3. **Bell-but-no-dialog (Symptom C).** There is no server-pushed approval SSE
   notification; the badge/dialog rely on the poller + local bell. This is the trigger
   that lets the modal sit unanswered long enough for defect (1) to fire. Addressing it
   is **optional** (see Open questions); it is not in the acceptance gate.

## Confirmed cause

Both removal paths (`approvals.go` 5-min connected auto-deny `:197-199`; double-consumer
where one client's `resolve`+`defer remove` deletes the entry before the slower client's
POST) end identically: `get(aid) == nil` in `approvalsSvc.Decide`, hard 404, decision +
persist both lost. The fix is made **cause-agnostic** by reconciling a late decision on
the daemon rather than racing it, so we need not prove which cause dominates in a given
session. (Repro during implementation will note which one we saw, per the issue.)

---

## Fix 1 — stop diagnostics bleeding onto the alternate screen (Symptom B)

**[R1] Corrected premise.** The original draft claimed Fix 1 "reuses the existing diag
sink, matching `cmd/main.go:111`." That is **false for attach mode.** `main()` calls
`runAttached(...)` and `return`s at `cmd/main.go:91` — *before* the
`diag.NewFile(...); g.SetLogger(lg)` block at `:111-117`. The attach path builds its own
`g := gogent.NewGogent(homeDir)` (`attach.go:147`) and **never** installs a file logger,
so the local core's logger stays at the default `diag.Stderr()` (`gogent.go:190`), which
writes to **fd 2**. Therefore attach mode has **two** bleed sources, not one:
  - the stdlib `log` package (the 13 `remote_handlers.go` sites), and
  - the local core's **`diag` logger** (`g.Warnf`/`Errorf` during theme/skill/rules/
    layout work in attach mode, e.g. `installPresentationHandlers`'s `SaveLayout` warn).

The `diag` package exists precisely so "diagnostics never corrupt the alternate screen
(issue #17)" (`logger.go:1-4`); redirecting only stdlib `log` would leave that door half
open. **Both must be redirected.**

**Plan.** In `runAttached`, immediately before launching `go wb.Run()` (`attach.go:235`),
open the gogent.log append file **once** and point both sinks at that single handle:
- `internal/diag/logger.go` — add `func OpenLogFile(path string) (*os.File, error)`
  wrapping the existing unexported `openAppend` (same 0600/0700 perms), so a caller can
  obtain the underlying `*os.File` to share between `diag.New` and stdlib `log`.
- `cmd/attach.go`:
  - `f, err := diag.OpenLogFile(filepath.Join(homeDir, ".gogent", "gogent.log"))`
  - on success: `g.SetLogger(diag.New(f))` (closes the diag bleed; mirrors `main.go:111-113`
    and `daemon.go:379-380`) **and** `log.SetOutput(f)` (closes the stdlib bleed).
  - on open failure: leave both defaults (degrade gracefully, matching `main.go`).
  - Pre-TUI setup logs (e.g. `remoteModelConfigs` `:267`) run before this point and still
    reach stderr while the normal screen is up — correct.
- **[R6] Restore stderr on exit.** After `wb.Run()` returns inside the goroutine
  (`attach.go:236`) the alternate screen is already torn down, so reset `log.SetOutput(os.Stderr)`
  *before* the existing `log.Printf("TUI error: %v", err)` (`:237`). A TUI exit error is
  then visible to the user instead of vanishing into the file.

turbotui owns the alternate screen and assumes the host app keeps fd 2 clean; it does not
import stdlib `log` and exposes no log-capture API, so a global `log.SetOutput` cannot
collide with anything it does. The fix belongs on the gogent side — **no turbotui change.**

---

## Fix 2 — a remote permission decision always reaches the daemon (Symptom A)

Two layers: the daemon must make a *late* sticky grant stick (so "always allow" survives
the race), and the client must retry blips and surface a genuinely-lost decision.

### Fix 2a — daemon: idempotent, persist-on-late-arrival decision endpoint

- `internal/server/approvals.go`:
  - On `remove(id)`, before deleting, copy a compact record into a small bounded ring
    `recent map[string]concludedApproval` (+ FIFO eviction slice), where
    `concludedApproval` keeps `{kind, sessionID, agentID, permission *permissionDetail}`.
    Cap ~64 (well above any realistic in-flight count); oldest evicted. Guarded by the
    existing `b.mu`. This is the only added daemon state.
  - Add `func (b *approvalBridge) recall(id string) (concludedApproval, bool)`.
- `internal/server/approvals_handlers.go` `approvalsSvc.Decide`:
  - `get(aid) != nil`, `resolve` succeeds → unchanged in-time path (tool goroutine wakes,
    returns the decision, `CheckWithContext` persists + fires the audit sink). Returns
    `{id, status:"resolved"}`.
  - `get(aid) != nil` but `resolve(aid, d) == false` (a faster double-consumer already
    delivered) → return idempotent 200 `{id, status:"resolved"}` instead of the current
    `409`: the winning POST already applied + persisted, so this is benign, not a conflict.
  - `get(aid) == nil` → call `recall(aid)`:
    - found, `kind == "permission"`, decision ∈ {`always`,`always_deny`} → persist directly
      via the new `Persist` below and return `{id, status:"late"}` (HTTP 200).
    - found, non-sticky (allow/deny/edit) → return `{id, status:"late"}` (the tool already
      concluded with the default; nothing to persist).
    - not found (truly unknown id, or evicted) → keep the existing **404** so the client
      learns it was genuinely lost (→ Fix 2b retries then surfaces). The existing
      `TestDecideUnknownApproval404` uses `apr_bogus` (never registered → never recalled),
      so it still 404s — **no test breaks.**
- **[R5] Late grants must hit the audit trail.** The in-time path records via
  `CheckWithContext`'s `defer sink(rc, a, resource, allowed)` (`permission.go:383-385`).
  A direct public-`Persist` call would bypass that, leaving a security-relevant grant
  off-record (`audit.log`, issue #51). Resolution — `internal/permission/permission.go`
  adds:
  ```
  func (s *Service) Persist(rc RequestContext, a Action, resource string, d Decision)
  ```
  which (1) delegates to the existing unexported `persist` (same `network:<host>` key, same
  disk flush) **and** (2) fires the `AuditSink` once with `allowed = d == DecisionAlways`,
  using `rc` from the recalled `{sessionID, agentID}`. Only the late path calls `Persist`;
  the in-line cascade uses the unexported `persist`, so there is **no double-audit**.
  `effect()`'s persisted-decisions cascade (`permission.go:330-343`) then returns
  `EffectAllow` for every subsequent path on that host — host-scoped (`ActionNetwork` is
  not a path action, so descendants never broaden scope), surviving restart via
  `permissions.json` (criteria 1–3).

Persisting on the *late* path even though the original `web_fetch` already used
default-deny is intentional: criterion 1 only requires *subsequent* fetches to be
suppressed. Only `always`/`always_deny` are persisted there, so a one-shot `allow` never
broadens.

### Fix 2b — client: retry, then surface a genuinely-lost decision

- **[R2] Retry without status classification.** `do()` flattens any non-2xx into a string
  error (`api_client.go:216-218`) — there is no typed status code to distinguish 5xx from
  4xx, and refactoring `do()` would touch every `api_client` caller. We avoid that
  entirely: **because Fix 2a makes the endpoint idempotent** (re-POSTing an already-applied
  or late decision returns 200, never double-applies), a *blind* bounded retry is safe on
  **any** error. `ui/tui/remote_handlers.go` `decide(aid, decision)`:
  - retry up to 2× with short backoff (e.g. 200ms, 500ms), bounded by `rc.ctx`, on any
    `DecideApproval` error;
  - return the final `error` (and the decoded response status, see below) to its caller
    `handleApproval`. `decide` becomes transport-only; it does **not** itself surface.
  - The existing `log.Printf` stays as the diagnostic record (now safely in the log file
    via Fix 1), but is no longer the only signal. A true 404 (unknown/evicted id) is
    retried 2× then surfaced — cheap and bounded.
- `ui/tui/api_client.go` `DecideApproval` decodes the response body into a small
  `{Status string}` (currently passes `out=nil`) and returns the status, so the client can
  tell an in-time `"resolved"` from a `"late"` reconcile. **[R6]** Update the `:838` doc
  comment: a 404/409 is no longer "ignore"; late decisions return idempotent 200, and a
  remaining failure is retried then surfaced.

### Surface seam — [R3] and [R4]

**[R3] Do not extend `Approver`.** `rc.approver` is the local `Approver` interface
(`remote_handlers.go:31`) whose contract is "presents an interactive gate and returns the
user's verdict" — a fire-and-forget note is not a gate, and it is nil in many tests. The
correct seam is the existing fire-and-forget `rc.sink` (`EventSink` = `wb.EmitSessionEvent`),
exactly as `emitErr` (`remote_handlers.go:1097-1104`) already does (nil-guarded). But
`emitErr` posts `SessionEventError`, which renders as a red `kindError` "error:" record
(`session_window.go:1830 → addError → :2735`); **no** `SessionEventType` routes to the
`[System]`/`kindSystem` record. So:
- `internal/agent/user_session.go` — add `SessionEventNotice SessionEventType = "notice"`
  (an informational `[System]` line; the `Text` field carries the message).
- `ui/tui/session_window.go` `apply` (`:1764`) — add a `case agent.SessionEventNotice`
  that appends a `kindSystem` record (header `"[System]"`, `colorInfo`/`roleInfo`),
  reusing the existing `addNote` (`:1899`) rendering. This is emitted **locally** via
  `rc.sink`, not pushed over SSE, so it adds no wire surface.
- `ui/tui/remote_handlers.go` — add `emitNotice(sessionID, text)`, parallel to `emitErr`
  (same `rc.sink == nil` guard).

**[R4] Kind-aware messaging, decided in `handleApproval` (which knows the kind), not in
the shared `decide`.** `decide` is called from both the permission (`:593`) and
`edit_review` (`:605`) branches, so a permission-specific string in `decide` would be
wrong for edits. `handleApproval` inspects `decide`'s returned `(status, err)`:
- `err != nil` (definitive failure after retries):
  - permission → `emitNotice`: "Couldn't deliver your approval to the daemon; the tool
    used the safe default. 'Always allow' did not take effect — please try again."
  - edit_review → `emitNotice`: "Couldn't deliver your edit-review decision to the daemon;
    the edit was not applied — please try again."
- `err == nil && status == "late"` and the decision was a sticky permission grant →
  `emitNotice` (informational, low-noise): "The request that prompted this already used
  the safe default; your 'always allow' for <host> will apply to future requests." This
  closes the **[R2-usability]** gap where a Cause-2 timeout silently persisted the grant
  while the in-flight fetch had already failed — the user now learns both facts.
- `err == nil && status == "resolved"` → silent (the common happy path; no noise).

---

## Fix 3 — make permission persistence failures diagnosable (defense-in-depth)

`internal/permission/permission.go`:
- `persist` (`:424`) does `_ = s.write(data)`. Route `write`/`MarshalIndent`/`load` errors
  (`:419,424,448,462-471`) to a diagnostics logger so a failed grant write is visible in
  `gogent.log` instead of dropped.
- The `Service` holds no logger today. Add nil-safe `SetLogger(*diag.Logger)`.
  **[R3-regression] Wire it on the path that actually persists.** In remote mode the
  *daemon* persists, and the daemon already installs a file logger
  (`cmd/daemon.go:379-380` → `g.SetLogger`). So `gogent.SetLogger` must **propagate** to
  the permission `Service` (call `g.permissions.SetLogger(lg)` inside `Gogent.SetLogger`),
  ensuring the daemon's file logger reaches it; the embedded/attach cores get the same
  propagation for free. A nil logger stays a safe no-op. Fix 3 is **droppable** without
  affecting criteria 1–5.

---

## User-facing behavior (after the fix)

- Granting **Always allow** for `example.com` writes `network:example.com → always` to the
  daemon's `~/.gogent/permissions.json` and suppresses the dialog for every subsequent
  `https://example.com/<any-path>` fetch in the session — **even if** the original prompt
  had already timed out or raced another client (the daemon reconciles the late grant and
  records it on the audit trail). It survives a daemon restart. `other.com` is unaffected.
- **No flash and no diagnostics on screen**: every `remote_handlers.go` `log.Printf`
  **and** every attach-mode `diag` warning now land in `gogent.log`, never fd 2, while the
  TUI is up; stderr is restored once the screen is down so an exit error stays visible.
- If a decision genuinely cannot be delivered, the user sees a kind-appropriate `[System]`
  note in the session. If a sticky grant landed late, the user is told the in-flight
  request already used the default but the grant applies going forward — never silent.

## Criterion-by-criterion

**(1) Goal match.** A *fix*, not a feature: remote decisions reach the daemon, per-host
`always` sticks across session + restart and is host-scoped, all 13 bleed sites are
silenced (stdlib `log` **and** `diag`), and a failed POST is retried-then-surfaced. The
two previously-unimplementable specifics are resolved: **[R1]** redirect both sinks (not a
nonexistent shared one); **[R2]** blind idempotent retry (no status-classification that
`do()` cannot provide). Optional Symptom-C SSE push is deliberately out of scope.

**(2) Usability.** Same modal drives the decision; the answer now takes effect reliably.
Failures and late-successes are both surfaced with kind-correct text **[R4]**, via the
correct fire-and-forget seam **[R3]**, gated so the happy path stays silent.

**(3) No regressions.**
- Embedded mode untouched: it installs the `*Workbench` as prompter directly
  (`cmd/main.go:209`), never the bridge/`decide` path; persists synchronously via
  `CheckWithContext`. Fix 2a only affects daemon/headless mode.
- Cascade + host-scoping unchanged; `Persist` reuses `persist`/`effect` and only writes
  `always`/`always_deny`. **[R5]** late grants now also hit the audit sink — closing an
  audit-completeness gap rather than opening one.
- In-time decision path is byte-for-byte unchanged; only `get==nil` and `resolve==false`
  branches change (404/409 → idempotent 200) — strictly more permissive toward the user's
  *already-expressed* intent, never the agent. `TestDecideUnknownApproval404` (`apr_bogus`)
  still 404s; no approval test asserts 409 (they call `bridge.resolve` directly), so none
  breaks. `:838` doc comment updated to match **[R6]**.
- `SessionEventNotice` is additive; existing `apply` cases and SSE consumers ignore an
  unknown type. `gofmt`/`build`/`vet`/`golangci-lint`/`go test ./...` stay green; the
  pre-existing `TestUserSessionSendMessage` 404 and the load-induced `internal/daemon`
  `TestStopGracefulAndForced` flake (passes in isolation) remain the only acceptable
  pre-existing failures. `ui/tui` forbidden-import set stays clean (only stdlib +
  existing `agent` types added).

**(4) Holistic across both repos.** gogent-only. The alternate-screen contract is
turbotui's; the bug was gogent leaving *both* stdlib `log` and its own `diag` logger on
fd 2 in attach mode — fixed on the gogent side, the correct repo, with no turbotui API
change (verified: turbotui owns stdout/alt-screen, never touches fd 2, no log-capture API,
does not import stdlib `log`). The daemon↔client seam is respected: the daemon owns
persistence + cross-client reconcile + audit (the only place that *can* coordinate two
clients); the client owns retry + the local `[System]` surface. No new dep, no go.mod bump.

## Regression risks & mitigations

- **Recall ring growth** → fixed cap (~64) + FIFO eviction under `b.mu`; entries tiny. An
  evicted late POST → 404 → retried → surfaced (acceptable).
- **Double-persist race** (handler late-`Persist` + tool-goroutine in-time `persist`) →
  idempotent (same key/value); the late path only runs when `get==nil`, so they do not
  overlap for the same id in practice.
- **Idempotent 200 hiding a real error** → only `get==nil` *with a recall hit* and
  `resolve==false` become 200; a truly unknown id still 404s, so a real bug still surfaces.
- **Surface noise** → gated to definitive failures + sticky-late successes only; happy path
  and non-sticky lates are silent.
- **Blind retry on a true 404** → bounded (2 retries, ~0.7s total), harmless because the
  endpoint is idempotent.
- **log redirect swallowing startup diagnostics / exit errors** → redirect only at the
  `wb.Run()` seam; stderr restored post-loop **[R6]**.

## Files touched (summary)

gogent:
- `cmd/attach.go` — open gogent.log once; `g.SetLogger(diag.New(f))` + `log.SetOutput(f)`
  before `wb.Run()`; restore `os.Stderr` after Run.
- `internal/diag/logger.go` — add `OpenLogFile`.
- `internal/server/approvals.go` — `recent` ring + `recall`; record on `remove`.
- `internal/server/approvals_handlers.go` — late-arrival reconcile; idempotent decide
  (404→late-persist/200, 409→200); status strings.
- `internal/permission/permission.go` — `Persist(rc, a, resource, d)` (writes + audits);
  nil-safe `SetLogger`; error logging in `persist`/`write`/`load` (Fix 3).
- `internal/gogent/gogent.go` — `SetLogger` propagates to `g.permissions` (Fix 3).
- `internal/agent/user_session.go` — add `SessionEventNotice`.
- `ui/tui/api_client.go` — `DecideApproval` decodes `{Status}`; update `:838` doc.
- `ui/tui/remote_handlers.go` — `decide` retry + return `(status, err)`; `emitNotice`;
  kind-aware surface in `handleApproval`.
- `ui/tui/session_window.go` — `apply` case for `SessionEventNotice` → `[System]` note.

turbotui: **none.**

Tests (criterion 6):
- `internal/server` — remote-approval `always` round-trip: bridge → decide → persist →
  reload (new `permission.Service` from same dir) → `effect(ActionNetwork, host) ==
  EffectAllow`; **late-arrival** variant: `remove` first, then decide `always` → assert
  persisted + status `"late"` + audit sink fired; assert `other.com` still asks; assert
  `resolve==false` returns 200.
- `cmd` (small testable helper around the redirect) — after the attach log redirect,
  `log.Writer() != os.Stderr` (stdlib log → diag file), and the local `g`'s logger is no
  longer the stderr default.

## Open questions

1. **Symptom C (approval SSE push).** A server-pushed approval notification
   (`internal/server/notify.go` + `ui/tui/tui.go:2686`) would make the badge/dialog appear
   instantly instead of within one 750ms poll, shrinking the window in which defect (1)
   fires. **Optional**, not in the gate. Recommend deferring to keep this PR a tight fix —
   confirm acceptable.
2. **Connected-timeout tuning.** Also stop counting the 5-min connected auto-deny clock
   until the prompt is actually presented (a client→daemon "presented" signal)? Attacks the
   dominant cause directly but needs a new endpoint/state = scope creep. The recall/idempotent
   reconcile makes it unnecessary for correctness; leaving the timeout as-is unless the
   maintainer prefers it.
3. **Surface vs. re-arm.** Criterion 5 allows a "re-armed prompt" instead of a `[System]`
   note. Going with the note (simpler, non-disruptive) unless re-arm is preferred.
