# Design — Duplicate-window guard (gogent issue #518)

**Branch:** `pair2/duplicate-window-guard-fix-for-gogent-is`
**Scope:** pure `ui/tui` (gogent). No turbotui change, no new deps, no `go.mod` bump.

## Problem restated

`Workbench` keeps a 1:1 map `w.sessions[id] -> *SessionWindow`. Three entry points
create a window for a session id **without** first checking whether one already
exists, so a second call for an already-open id overwrites the map entry and
orphans the first on-screen window (its layer stays on the desktop, but nothing
points at it). Result: two visible windows, one map entry, one backend session —
split-brain.

The correct guard already lives in `openWatcherSession` (`tui.go:2959`):

```go
w.mu.Lock()
open := w.sessions[sessionID] != nil
w.mu.Unlock()
if open { w.Focus(sessionID); return }
```

This design mirrors that pattern at the three unguarded sites.

## Evidence (current code)

- `AdoptSession` (`tui.go:1600`): `sw := w.openWindow(rs.ID, title); sw.restore(rs.Messages)` — no existence check.
- `openWindowAny` (`tui.go:1674`): `sw := newSessionWindow(...); w.sessions[id] = sw; w.order = append(...)` — no collision check; unconditional overwrite + duplicate `order` entry.
- Startup restore loop (`Run`, `tui.go:2656`): `for _, rs := range orderByLayout(restored, layout) { w.AdoptSession(rs) }` — unconditional.
- `refreshAfterReconnect` (`disconnect_modal.go:147`): already dedups via an `open[rs.ID]` snapshot — open ids get `sw.reload(rs.Messages)`, only genuinely-new ids get `AdoptSession`. This is today the *only* thing preventing a dup window, and is the behavioral template for the fix.

## Concurrency / call-thread facts (verified)

- `w.mu` guards `w.sessions`, `w.order`, `w.nextNum`, `w.nextAnalysis`. Existing code acquires it briefly, releases, then touches the desktop/widgets (e.g. `Focus`, `openWindowAny` itself). The fix keeps that discipline — **single lock acquisition per call, released before any callback or UI mutation** (no new lock-ordering hazards, no nested locks).
- Every `AdoptSession` caller runs on the UI thread: the startup loop (before the event loop starts, single-threaded) and `refreshAfterReconnect`/`openWatcherSession` (which wrap the call in `w.desktop.Post`). So `reload`/`restore`, which mutate the transcript view, are safe on the guard path.
- `reload(msgs)` (`session_window.go:2745`) = `records = nil; restore(msgs)`, and `restore` composes the view **exactly once** via `addAll` (issue #519). So "reload on adopt-existing" is a single render — it does **not** double-render and does not regress #519/#520.

## Changes — A + B + C (one consistent guard)

### A. `AdoptSession` (`tui.go:1600`)

After the existing `nextNum` bump (keep that — restored ids must still advance the
counter even on the dedup path), check the map under `w.mu`:

```go
w.mu.Lock()
if n := parseSessionNum(rs.ID); n > w.nextNum { w.nextNum = n }
existing := w.sessions[rs.ID]
w.mu.Unlock()
if existing != nil {
    // Already open: refresh its transcript to the daemon's current copy and
    // return the SAME window — never a second one. Mirrors the already-open
    // branch of refreshAfterReconnect. reload() is a single compose (#519).
    if !existing.readOnly {
        existing.reload(rs.Messages)
    }
    return existing
}
// ...unchanged: openWindow + restore + model preselect + OnCreate...
```

Decisions:
- **reload, not restore** — keeps the window current (issue's stated preference);
  matches `refreshAfterReconnect`.
- **Skip `OnCreate` on the dedup path.** `OnCreate` attaches the live event
  observer to the backend; it already ran when the window was first opened.
  Re-invoking it risks a double-attached observer. The already-open branch of
  `refreshAfterReconnect` likewise only reloads.
- **Skip model-preselect on the dedup path.** The open window already reflects the
  user's current dropdown choice; re-seeding from `rs.Model` would clobber a live
  user selection. Preselect stays on the first-open path only.
- **`readOnly` guard on reload** — an analysis window (read-only) should never be
  mutated; this also keeps `AdoptSession` from ever touching one (it never opens
  read-only windows itself, but the guard is cheap and defensive).

### B. Startup restore loop (`Run`, `tui.go:2656`)

Skip ids already materialized this pass, defending against a duplicate id inside
`restored` and against a concurrent reconnect-triggered restore that already
opened a window:

```go
seen := make(map[string]bool, len(restored))
for _, rs := range orderByLayout(restored, layout) {
    if seen[rs.ID] {
        continue
    }
    seen[rs.ID] = true
    w.AdoptSession(rs)
}
```

`AdoptSession` (A) is already idempotent, so B is belt-and-suspenders, but it makes
the loop's intent explicit and cheap. The real race protection is the
under-`w.mu` check in A and C; `seen` only handles in-list duplicates.

### C. `openWindowAny` (`tui.go:1674`)

Collision check at the top, under the same lock, **before** `w.sessions[id] = sw`
and the `w.order` append, so no overwrite and no orphan:

```go
w.mu.Lock()
if existing := w.sessions[id]; existing != nil {
    w.mu.Unlock()
    // A window for this id already exists. The callers (AdoptSession, NewSession,
    // OpenAnalysisSession) all generate or guard ids that should be unique here,
    // so reaching this is a programming error upstream — log a tripwire and
    // return the existing window rather than silently orphaning it.
    log.Printf("tui: openWindowAny: window for session %q already exists; returning existing (duplicate-open guard, issue #518)", id)
    return existing
}
// ...unchanged: cascade geometry, newSessionWindow, map+order register, unlock,
//    AddLayer, sidebar.addSession, refreshOverall...
```

This is the last line of defense (A already stops the normal dup-adopt path before
it reaches here). Returning the existing window keeps the `*SessionWindow` contract
so callers don't nil-panic; the caller's subsequent `sw.restore(...)` (e.g.
`OpenAnalysisSession`, `forkWindow`) would re-render onto the existing window
rather than a phantom — acceptable for a should-never-happen path, and strictly
better than today's orphan.

Adds `"log"` to `tui.go`'s import block (stdlib; the package already uses
`log.Printf` in `remote_handlers.go`, so this matches house style — no new dep).

## User-facing behavior

- **Normal single open** (fresh restore, new session, fork, analysis): unchanged —
  one window, full preselect/OnCreate path.
- **Adopt an already-open id** (e.g. reconnect race, watcher re-open, a saved-session
  re-open of a live session): the existing window is reused and its transcript
  refreshed to the latest messages; focus/order/sidebar unchanged. No second window,
  no flicker, no split-brain.
- **Programming-error double-open** via `openWindowAny`: existing window returned, a
  tripwire line is logged (surfaced, not silent) so the upstream bug is diagnosable.

## Design criteria

**(1) Goal match.** Exactly the issue's ask: a fix, not a feature. Creating a window
for an already-open id becomes no-op-or-reload at all three named sites
(`AdoptSession`, startup loop, `openWindowAny`), mirroring `openWatcherSession`. No
scope creep — no new UI, no dialog, no behavior change to single-open.

**(2) Usability.** No split-brain windows; an already-open session is raised/reused
and its transcript kept current (reload), which is what a user expects on
reconnect/re-open. The `openWindowAny` collision is surfaced via a log tripwire
rather than failing silently. No new user input surface is needed — this is an
internal invariant, correctly kept internal.

**(3) No regressions.**
- Single-open flow untouched (guard branches are entered only when a window already exists).
- Reconnect dedup (`refreshAfterReconnect`) intact and now consistent with `AdoptSession`.
- `reload` is a single compose, so #519/#520 batched-render behavior is preserved (no double render).
- `nextNum` advancement preserved on the dedup path (restored-id counter stays correct).
- Read-only analysis windows never reloaded/mutated by the guard.
- Lock discipline matches existing code: one short critical section, released before UI/callbacks — gofmt/vet/build/lint clean expected; `go test ./...` green except the known-acceptable `TestUserSessionSendMessage` 404.

**(4) Holistic / cross-repo seam.** Entirely within gogent `ui/tui` (`tui.go`, and
`disconnect_modal.go` only as the behavioral reference — no edit needed there). The
turbotui seam is untouched: turbotui owns the desktop/layer/widget primitives;
window-identity bookkeeping (`w.sessions`/`w.order`) is a gogent concern, so the fix
belongs here and nowhere else. No turbotui change, no new deps, no `go.mod` bump.
The orphaned-layer symptom is a misuse of turbotui's `AddLayer` (adding a second
layer for the same logical session); fixing the gogent-side guard removes the misuse
without needing any turbotui API change.

## Files / functions touched

- `ui/tui/tui.go`
  - `AdoptSession` (A) — dedup guard + reload-and-return.
  - `Run` startup restore loop (B) — `seen` skip.
  - `openWindowAny` (C) — collision tripwire + return existing.
  - import block — add `"log"`.
- `ui/tui/disconnect_modal.go` — **reference only**, no change.
- No turbotui files.

## Tests (new `*_test.go` in `ui/tui`, mirroring `restore_model_issue266_test.go` style; uses `NewWorkbench(...)` + `SessionIDs()`)

1. **Duplicate adopt** — `AdoptSession({ID:"s1",...})` twice. Assert: `len(SessionIDs())==1`, second call returns the *same* `*SessionWindow` pointer as the first, and (reload chosen) the transcript reflects the second call's messages.
2. **Duplicate startup** — drive the restore-loop guard with a `restored` slice containing the same id twice (test the `seen`-skip logic; either via a Restore handler + the loop, or a focused helper) → exactly one window.
3. **`openWindowAny` collision** — call it twice for one id. Assert: single `w.sessions`/`order` entry, no orphan layer, same window returned. Assert the tripwire via a captured `log.SetOutput(buf)` if convenient, else assert the single-map-entry invariant.
4. Keep `ui/tui` free of `internal/daemon`/`internal/server` imports (unchanged).

## Regression risks (called out)

- **Double observer attach** — mitigated by skipping `OnCreate` on the dedup path.
- **Clobbering a live model selection** — mitigated by skipping model-preselect on the dedup path.
- **Double render** — mitigated by using `reload` (single compose, #519).
- **Lock ordering** — no new nesting; single critical section per call, released before UI/callbacks, identical to existing `Focus`/`openWatcherSession` discipline.
- **Read-only windows** — guarded; never reloaded.

## Open questions

1. **`openWindowAny` dedup-path re-restore.** On the should-never-happen C path the
   caller may still call `sw.restore(...)` on the returned existing window
   (`OpenAnalysisSession`, `forkWindow`). That re-renders onto the existing window
   rather than a phantom — acceptable and strictly better than today, but if we want
   C to be fully inert we'd have to change call sites too. Proposal: leave as-is (A
   prevents the realistic path; C is a tripwire), out of scope to refactor callers.
2. **Tripwire severity.** `log.Printf` (matches `remote_handlers.go`) vs. a louder
   signal. Proposal: `log.Printf` — it's a developer diagnostic, not a user-facing
   error, and there's no user action to take.
3. **Test 2 harness reach.** Whether to exercise the real `Run` startup loop (needs a
   Restore handler wired) or a thin extracted helper. Proposal: wire a `Restore`
   handler returning a duplicate id and assert one window, keeping the loop logic in
   `Run` (no production refactor just for testability) — fall back to asserting via
   `AdoptSession`-twice (test 1) if `Run` proves hard to drive headless.
