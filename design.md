# Design — Fix #476: fresh windows orphaned after embedded→daemon "Start daemon" handoff

## Summary

When a user starts the daemon from inside the TUI (Daemon → Start daemon), the
embedded→daemon handoff in `cmd/handoff.go` `Start()` swaps the Workbench handlers
to the remote HTTP/SSE implementation but never creates the already-open windows'
sessions on the daemon. Persisted/restored windows survive (the daemon's
`RestoreSessions()` rebuilds them from disk); a **fresh, never-messaged window**
was never written to disk, so the daemon has no record of it and the first
`POST /sessions/:id/messages` returns `404 Not Found: session not found`.

The fix makes `Start()` symmetric with `Stop()`: after the daemon is confirmed
healthy and before the handlers are switched, explicitly create each open
window's session on the daemon — the remote equivalent of the `Stop()` path's
`bindWindowSession`. This guarantees the invariant both handoff directions are
meant to uphold: **after either handoff, every open window has a live backend
session**.

## Root cause (confirmed against source)

- `Start()` — `cmd/handoff.go:143`. Sequence: `cancelInflightTurns` + `SyncStore`
  (persist) → spawn daemon + `waitRunning` → build `APIClient` + `client.Health()`
  (lines 184–192) → `dc.switchToRemote(g, client, addr)` (line 193) → shut source
  down (`g.StopWatchers()` line 203). **No per-window session creation anywhere.**
- `Stop()` — `cmd/handoff.go:285`. After building a fresh embedded core it loops
  `for _, id := range dc.wb.SessionIDs() { bindWindowSession(g, dc.wb, id) }`
  (lines 319–321). `bindWindowSession` (line 431) does `GetUserSession(id)` and, if
  nil, `NewSession(id)` — recreating any missing session. This is the correct,
  symmetric reference implementation that `Start()` lacks.
- Auto-attach is unaffected: `RemoteClient.Handlers().OnCreate`
  (`ui/tui/remote_handlers.go:517`) already calls `c.CreateSession(sessionID, title,
  true)` per window. Our fix is the handoff-time equivalent of that same call.
- Server side is safe/idempotent: `sessionsSvc.Create` (`internal/server/sessions.go:62`)
  honours a caller-supplied id and applies the title; `Server.createSession`
  (`internal/server/api.go:295`) returns the existing `UserSession` when one already
  exists. So re-creating a restored window's session is a no-op, not a duplicate or
  error. Loopback callers get human scope (`requireHuman`,
  `internal/server/approvals.go:337`), so the create is authorized.

## Changes

### 1. gogent `ui/tui/tui.go` — export a title accessor (API gap)

`sessionTitle(id)` is unexported (`ui/tui/tui.go:1267`), so `cmd/handoff.go` cannot
read a window's title. Add a public accessor that **delegates** to it (single
source of truth, mirrors the existing `SessionIDs()` accessor at line 814):

```go
// SessionTitle returns the window title for an open session id, or "" when the
// id is unknown. The handoff controller uses it to preserve a window's title when
// creating its session on the daemon during an embedded->daemon handoff, mirroring
// what OnCreate carries on the auto-attach path.
func (w *Workbench) SessionTitle(id string) string {
    return w.sessionTitle(id)
}
```

This is a gogent change. `ui/tui` is package `ui` in module `gogent`; it only
*imports* turbotui as a dependency. **turbotui is not touched.**

### 2. gogent `cmd/handoff.go` — create each window's session on the daemon

Add a small package-level helper that mirrors `bindWindowSession` and reuses the
exact "user-facing windows only" filter already established in
`liveUserSessionCount` (`cmd/handoff.go:443`):

```go
// createDaemonWindowSessions creates each open window's session on the freshly
// started daemon, so an embedded->daemon handoff leaves every window with a live
// backend session (issue #476). It is the remote equivalent of Stop()'s
// bindWindowSession: a fresh, never-messaged window was never persisted, so the
// daemon's RestoreSessions cannot rebuild it and the next OnSend would 404. The
// backend-only "default" and "watcher:"-prefixed sessions are excluded (they are
// not user windows). createSession on the server is idempotent, so windows already
// restored from disk are re-created harmlessly. A create failure is logged and
// degrades to the pre-fix behaviour for that one window (it would 404 on send) so a
// single failure never aborts the whole handoff.
func createDaemonWindowSessions(client *tuipkg.APIClient, wb *tuipkg.Workbench) {
    for _, id := range wb.SessionIDs() {
        if id == "default" || strings.HasPrefix(id, "watcher:") {
            continue
        }
        title := wb.SessionTitle(id)
        if _, err := client.CreateSession(id, title, true); err != nil {
            log.Printf("handoff: create session %s on daemon: %v", id, err)
        }
    }
}
```

Call it in `Start()` immediately after `client.Health()` succeeds and **before**
`dc.switchToRemote(...)` (between current lines 192 and 193):

```go
    if err := client.Health(); err != nil {
        dc.rollbackSpawn(spawned)
        return fmt.Errorf("daemon not reachable after start: %w", err)
    }
    // Recreate every open window's session on the daemon before switching handlers,
    // so a fresh never-messaged window (not on disk, so RestoreSessions cannot
    // rebuild it) has a live backend session and its next send does not 404
    // (issue #476). Symmetric with Stop()'s bindWindowSession loop.
    createDaemonWindowSessions(client, dc.wb)
    rc, err := dc.switchToRemote(g, client, addr)
```

Placement rationale: the daemon is healthy (sessions are creatable) and the source
embedded core is still fully live, so a create here is safe and, if it somehow
failed catastrophically, nothing has been torn down. It is before the handlers
switch and well before `g.StopWatchers()`, preserving the
persist→spawn→**create**→switch→shutdown order. `strings` and `log` are already
imported in `cmd/handoff.go`.

`tuipkg` is the existing import alias for `gogent/ui/tui` in `cmd/handoff.go`.

## Tests (new) — `cmd/handoff_start_session_test.go`

Harness (established pattern: `internal/server/background_state_issue353_test.go:159`
uses `httptest.NewServer(srv.Handler())`; loopback → human scope):

- Build a real core `gogent.NewGogent(t.TempDir())` and `server.NewServer(g, …)`.
- `httpSrv := httptest.NewServer(srv.Handler())`; `client, _ := tuipkg.NewAPIClient(httpSrv.URL, "")`
  (`http://` scheme is accepted, `api_client.go:105`).
- Build a `Workbench` via `tuipkg.NewWorkbench(...)`, open fresh windows with
  `wb.NewSession()` (yields id `session-N`, title `Session N`, no backend — exactly
  the orphaned-window scenario since no `OnCreate` handler is wired).

Cases:
1. **Fresh window (the bug):** one `NewSession()` → `session-1`. Run
   `createDaemonWindowSessions(client, wb)`. Assert `client.GetSession("session-1")`
   returns no error (200), proving the session exists on the daemon **before any
   message is sent**. (Optionally also assert a `SendMessage`/POST does not return a
   404 "session not found".)
2. **Title preserved:** assert the returned `SessionDTO.Title == "Session 1"`
   (validates the public `SessionTitle` accessor end-to-end).
3. **Regression — restored/already-conversed window stays working:** second window
   `session-2`; pre-create it on the daemon (`g.NewSession("session-2")`, mirroring a
   disk-restored session). Run the helper. Assert no error, `session-2` still
   present and not broken (idempotent create), confirming windows that *were*
   restored survive.
4. **Backend-only ids skipped:** confirm the filter — the helper must not attempt
   to create `default`/`watcher:`-prefixed ids. (Covered by asserting only the user
   windows exist; can be reinforced by a direct filter unit check if `NewSession`
   makes injecting such ids awkward.)

Existing source-text guard `TestIssue358StartHandoffDoesNotShutSourceBeforeRemoteReady`
keys on the substrings `dc.switchToRemote(g, client, addr)` and `g.StopWatchers()`
and their order — both are unchanged and our insertion sits before them, so it
stays green.

## Design-criteria assessment

**(1) Goal match.** Pure bug fix, exactly the issue's ask: make `Start()` create
each open window's session on the daemon so a fresh window's first send runs the
turn instead of 404ing. No feature creep, no refactor beyond the one minimal
accessor the issue itself calls for. Mirrors the auto-attach `OnCreate` and the
`Stop`/`bindWindowSession` reference.

**(2) Usability.** Behaviour is silent-but-correct by design — the user does
nothing differently; after "Start daemon" the window simply works (message
delivered, progress streams back over SSE) exactly as on auto-attach. The window's
**title is preserved** via the public accessor (vs. the rejected fallback of
sending an empty title and letting the daemon use the id). A per-window create
failure is surfaced via `log.Printf` (consistent with the existing remote
`OnClose`/`OnStop` handlers) rather than aborting the whole handoff for one window.

**(3) No regressions.**
- Restored / already-conversed windows: server `createSession` is idempotent
  (`api.go:295`) — re-creating returns the existing session; the title re-applied is
  the same window title. No duplication, no error.
- Auto-attach path: untouched.
- `Stop` direction: untouched.
- Handoff ordering invariants (persist→spawn→switch→shutdown) preserved; rollback
  semantics unchanged (create runs after the spawn is already committed to and the
  source is still live, before the handlers switch). Existing source-text ordering
  tests still pass.
- `SessionTitle` delegates to `sessionTitle` (no duplicated logic, single source of
  truth); `SessionIDs()` already filters to live, non-read-only windows.

**(4) Holistic / cross-repo.** The change lives entirely in gogent: the handoff
controller (`cmd/handoff.go`) and gogent's own `Workbench` (`ui/tui`, package `ui`
of module `gogent`). turbotui is a dependency that `ui/tui` imports for widgets; no
turbotui type or API is modified, no `go.mod` bump, no new dependency. The seam is
respected — `Workbench` is gogent's composition layer over turbotui, and we only
add a gogent accessor and reuse an existing gogent client method
(`APIClient.CreateSession`). The new public API (`SessionTitle`) is minimal and
consistent with the sibling `SessionIDs` accessor.

## Regression risks (call-outs)

- **Title overwrite on restored sessions.** `Create` calls `SetSessionTitle` when a
  title is supplied. For a restored window the window title equals the session
  title, so this is a no-op-equivalent; the helper never sends an empty title that
  could blank a name. Low risk, covered by test case 3.
- **Create-before-RestoreSessions race.** The daemon may still be restoring when we
  create; because `createSession` is idempotent on id, order does not matter —
  whichever runs second adopts the existing session. No risk.
- **A window id colliding with `default`/`watcher:`.** Excluded by the same filter
  `liveUserSessionCount` uses; `SessionIDs()` already omits read-only windows.

## Open questions

None blocking. One minor judgement call already resolved per the issue's stated
preference: title handling uses the **public `SessionTitle` accessor** (preserve
the user's window title) rather than the empty-title fallback.
