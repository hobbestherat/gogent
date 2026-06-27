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
- `refreshAfterReconnect` (`disconnect_modal.go:147`): already dedups via an `open[rs.ID]` snapshot — open ids get `sw.reload(rs.Messages)` **inline**, only genuinely-new ids get `AdoptSession`. This is today the *only* thing preventing a dup window. Its existence-check is the template for the guard; its reload action is **not** copied into `AdoptSession` (provenance differs — see below).

## Concurrency / call-thread facts (verified)

- `w.mu` guards `w.sessions`, `w.order`, `w.nextNum`, `w.nextAnalysis`. Existing code acquires it briefly, releases, then touches the desktop/widgets (e.g. `Focus`, `openWindowAny` itself). The fix keeps that discipline — **single lock acquisition per call, released before any callback or UI mutation** (no new lock-ordering hazards, no nested locks).
- Every `AdoptSession` caller runs on the UI thread: the startup loop (before the event loop starts, single-threaded) and `refreshAfterReconnect`/`openWatcherSession` (which wrap the call in `w.desktop.Post`). So `reload`/`restore`, which mutate the transcript view, are safe on the guard path.
- `reload(msgs)` (`session_window.go:2745`) = `records = nil; restore(msgs)`, single compose via `addAll` (issue #519) — relevant only to `refreshAfterReconnect`'s reload path, which this fix leaves untouched (see below).
- **`AdoptSession` is a public method fed from two provenances.** `refreshAfterReconnect` (disconnect_modal.go:153) supplies `rs` from `Restore()` = the daemon's **live** state. But the Saved Sessions **"Continue"** button (`continueSession`, sessions_dialog.go:160-173) calls `AdoptSession(rs)` where `rs.Messages` came from `OpenSavedSession(m.File, true)` — a **file read** that can be **staler** than a live open window. So the dedup branch must not assume `rs.Messages` is current.
- **`refreshAfterReconnect` reloads open ids _inline_** (disconnect_modal.go:156-163, `sw.reload(rs.Messages)`) and only routes **genuinely-new** ids through `AdoptSession` (line 167). So changing `AdoptSession`'s dedup branch does **not** touch the §7 jump-to-present reload — that path never reaches `AdoptSession` for an already-open id.

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
    // Already open: raise the SAME window and return it — never a second one,
    // never a transcript mutation. This is the faithful mirror of
    // openWatcherSession's already-open branch (w.Focus(id); return), which the
    // issue names as the correct pattern.
    w.Focus(rs.ID)
    return existing
}
// ...unchanged: openWindow + restore + model preselect + OnCreate...
```

Decisions:
- **Focus, NOT reload** (revised after critique D1). Earlier the branch reloaded
  `rs.Messages`; that was wrong on two counts, both reachable via the user-driven
  "Continue" button (`continueSession`):
  1. **Stale-snapshot clobber (regression).** `continueSession`'s `rs.Messages` is a
     file read from `OpenSavedSession`. If the existing window is a *live* session
     the user has been typing into, an unconditional `reload` would replace the live
     transcript with the last-saved snapshot and the user's recent messages would
     vanish from the view — strictly worse than today's orphan-on-dup (which at least
     leaves the live transcript intact). The `refreshAfterReconnect` analogy did not
     hold: that path reloads from **live** daemon state, not a file, and reaches its
     reload **inline** (not via `AdoptSession`).
  2. **No raise (usability).** Reload left the existing window where it was, so
     clicking "Continue" on an already-open session did nothing visible. `Focus`
     raises it to the top of the z-stack and focuses its input — what the user
     expects, and exactly what `openWatcherSession` does.
- **Skip `OnCreate` on the dedup path.** `OnCreate` attaches the live event observer
  to the backend; it already ran when the window was first opened. Re-invoking it
  risks a double-attached observer. (`Focus` touches no backend handler.)
- **Skip model-preselect on the dedup path.** The open window already reflects the
  user's current dropdown choice; re-seeding from `rs.Model` would clobber a live
  user selection. Preselect stays on the first-open path only.
- **No `readOnly` concern.** `AdoptSession` never opens read-only windows, and
  `analysis-N` ids never collide with session ids, so an `existing` under `rs.ID` is
  always a live window; `Focus` is correct for it and mutates no transcript.
- **`Focus` is headless-safe** — exercised headless by existing tests
  (`close_session_flash_test.go:128`, `sidebar_todos_test.go`,
  `watchers_dialog_test.go:335`), so the dedup test can drive and assert it.

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
`OpenAnalysisSession`, `ForkSession`) would re-render onto the existing window
rather than a phantom — acceptable for a should-never-happen path, and strictly
better than today's orphan.

Adds `"log"` to `tui.go`'s import block (stdlib; the package already uses
`log.Printf` in `remote_handlers.go`, so this matches house style — no new dep).

## User-facing behavior

- **Normal single open** (fresh restore, new session, fork, analysis): unchanged —
  one window, full preselect/OnCreate path.
- **Adopt an already-open id** (e.g. reconnect race, watcher re-open, or the Saved
  Sessions "Continue" button on a session whose handler-level open-check disagrees
  with the UI map): the existing window is **raised and focused** — its live
  transcript is left untouched. No second window, no flicker, no split-brain, and no
  in-flight messages clobbered by a stale file snapshot.
- **§7 jump-to-present (reconnect) is unchanged:** `refreshAfterReconnect` still
  reloads each open window from the daemon's live state inline; only new ids go
  through `AdoptSession`, so this fix does not alter that reload.
- **Programming-error double-open** via `openWindowAny`: existing window returned, a
  tripwire line is logged (surfaced, not silent) so the upstream bug is diagnosable.

## Design criteria

**(1) Goal match.** Exactly the issue's ask: a fix, not a feature. Creating a window
for an already-open id becomes no-op-or-reload at all three named sites
(`AdoptSession`, startup loop, `openWindowAny`), mirroring `openWatcherSession`. No
scope creep — no new UI, no dialog, no behavior change to single-open.

**(2) Usability.** No split-brain windows; an already-open session is **raised and
focused** (`w.Focus`), which is what a user expects when re-opening — including on the
user-driven "Continue" path, which previously would have done nothing visible. This is
the faithful mirror of `openWatcherSession`, the pattern the issue names as correct.
The `openWindowAny` collision is surfaced via a log tripwire rather than failing
silently. No new user input surface is needed — this is an internal invariant,
correctly kept internal.

**(3) No regressions.**
- Single-open flow untouched (guard branches entered only when a window already exists).
- **No stale-snapshot clobber:** the dedup branch no longer reloads, so a live window
  the user is typing into is never overwritten by a saved-file snapshot from the
  "Continue" path. (This was the critique's material concern.)
- Reconnect dedup (`refreshAfterReconnect`) intact: its inline live-state reload of open
  windows is unchanged, and only new ids route through `AdoptSession`.
- `nextNum` advancement preserved on the dedup path (restored-id counter stays correct).
- No read-only window is mutated (the guard only `Focus`es, and ids never collide there).
- Lock discipline matches existing code: one short critical section, released before
  UI/callbacks — gofmt/vet/build/lint clean expected; `go test ./...` green except the
  known-acceptable `TestUserSessionSendMessage` 404.

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

1. **Duplicate adopt** — `AdoptSession({ID:"s1",...})` twice. Assert: `len(SessionIDs())==1`, second call returns the *same* `*SessionWindow` pointer, and — guarding against the reverted reload — that the **first call's transcript is preserved** (the second call's messages do NOT replace it, since the branch now Focuses, not reloads). Optionally assert the window was raised (top of z-order) after the second call.
2. **Duplicate startup** — exercise the loop body over a duplicate-id slice (mirror `for _, rs := range orderByLayout(...) { if seen[...]{continue}; ...; w.AdoptSession(rs) }`) with two entries for the same id → exactly one window. `Run()` blocks headless, so the test reproduces the loop's guarded body rather than calling `Run()`; this covers the "duplicate startup → one window" acceptance and exercises both the `seen` skip and A's idempotence.
3. **`openWindowAny` collision** — call it twice for one id. Assert: single `w.sessions`/`order` entry, no orphan layer, same window returned. Assert the tripwire via a captured `log.SetOutput(buf)` if convenient, else assert the single-map-entry invariant.
4. Keep `ui/tui` free of `internal/daemon`/`internal/server` imports (unchanged).

## Regression risks (called out)

- **Stale-snapshot clobber** (critique D1) — eliminated: the dedup branch `Focus`es, never reloads, so `rs.Messages` from a file read can never overwrite a live transcript.
- **Double observer attach** — mitigated by skipping `OnCreate` on the dedup path. Residual (D2): on the C tripwire path a caller still proceeds to `restore`/`SetFocus`/`OnCreate`/`OnFork` on the returned existing window (`NewSession`, `ForkSession`, `OpenAnalysisSession`, and a narrow A→C race). C is a should-never-happen tripwire; the log line is the signal. Acceptable, documented in Open Q1; not worth refactoring all call sites.
- **Clobbering a live model selection** — mitigated by skipping model-preselect on the dedup path.
- **Lock ordering** — no new nesting; single critical section per call, released before UI/callbacks, identical to existing `Focus`/`openWatcherSession` discipline.
- **Read-only windows** — never mutated; the guard only `Focus`es and ids never collide there.

## Open questions

1. **`openWindowAny` dedup-path re-restore (D2).** On the should-never-happen C path
   the caller may still call `sw.restore(...)`/`OnCreate`/`OnFork` on the returned
   existing window (`NewSession`, `ForkSession`, `OpenAnalysisSession`). That
   re-renders onto the existing window rather than a phantom — acceptable and strictly
   better than today's orphan, but C is not fully inert. Proposal: leave as-is (A
   prevents the realistic path; C is a tripwire), out of scope to refactor callers.
2. **Tripwire severity.** `log.Printf` (matches `remote_handlers.go`) vs. a louder
   signal. Proposal: `log.Printf` — a developer diagnostic, no user action to take.
3. **B vs A semantics (D3).** A now `Focus`es (idempotent, no mutation) and B skips;
   both protect the same invariant, so B is technically redundant once A lands. The
   task explicitly requires B as a defensive guard, so we keep it — it also documents
   the loop's intent and cheaply handles a duplicate id appearing twice inside
   `restored` before A is even reached. Proposal: keep B.
