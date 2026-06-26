# Design — Fix #478: stacked daemon dialogs on Start/Stop daemon

## Summary

Starting (or stopping) the local daemon from the TUI's **Daemon** menu shows
**two** modal dialogs stacked: an interim "Migrating…" progress dialog that is
never dismissed, plus the result dialog on top of it. The user must close both.

Root cause (confirmed in source): `ui/tui/daemon_menu.go`
`startDaemonFromMenu` (line 99) and `stopDaemonFromMenu` (line 120) each call
`w.showConfirm(...)` twice — once for the interim "Migrating…" message and again
in the `w.desktop.Post(...)` completion callback for the result. `showConfirm`
(`ui/tui/message_dialog.go:94`) creates a fresh `tv.NewModalLayer("confirm-dialog", …)`
and calls `w.desktop.AddLayer(layer)` every time, and turbotui's
`Desktop.AddLayer` (`turbotv/desktop.go:270`) **appends** to the layer stack — it
never replaces the top. No reference to the first layer is kept, so it cannot be
removed. The interim dialog therefore lingers under the result.

The fix makes the result dialog **replace** the progress dialog: track the
interim layer and `RemoveLayer` it before showing the result, on both the success
and error paths, for both Start and Stop. This mirrors the existing
`disconnect_modal.go` tracked-layer pattern exactly.

## Why "replace", not "delete"

The "Migrating…" dialog is real feedback during the up-to-15 s readiness window in
`cmd/handoff.go` `Start` (spawns a process, waits for socket bind + `/health`). It
must stay visible while the handoff runs and be torn down only when the result is
ready — i.e. replaced by exactly one result dialog, never both at once.

## Changes (gogent only)

### 1. `ui/tui/message_dialog.go` — single layer-creation path + a progress modal

Extract the dialog/body/modal-layer scaffolding shared by every message dialog
into one private builder, so layer creation has a **single source of truth**:

```go
// newMessageLayer builds and registers the modal dialog scaffold shared by
// showConfirm and showProgress: a content-sized dialog (issues #299/#309), its
// wrapped body TextView, and the modal layer (resize-reflowed). The caller adds
// its own buttons/escape/focus. It returns the dialog, the live layer, and the
// resolved content width + body height the caller needs to place controls.
func (w *Workbench) newMessageLayer(title, message, layerName string) (*tv.Dialog, *tv.Layer, int, int)
```

It contains exactly today's `showConfirm` body up to and including
`installResizeReflow`, parameterised on `layerName`.

- **`showConfirm`** is refactored to call `newMessageLayer(title, message,
  "confirm-dialog")`, then add its OK / Yes-No buttons, the Escape handler, the
  `dismiss` closure (`w.desktop.RemoveLayer(layer)`), and `SetFocus(focus)` —
  byte-for-byte the same observable behavior (same layer name, `NoEnterGrace`,
  reflow, body, buttons). No signature change; all existing callers untouched.

- **New `showProgress`** — the interim feedback modal:

```go
// showProgress opens a non-dismissable informational modal used as interim
// feedback for a background operation (the daemon handoff). Unlike showConfirm it
// has NO buttons and NO Escape handler: it blocks input while the operation runs
// and is torn down programmatically (RemoveLayer) when the result is ready, so the
// result dialog REPLACES it rather than stacking on top (issue #478). It returns
// the layer so the caller can dismiss it. Mirrors the disconnect modal's
// programmatic-only lifecycle (disconnect_modal.go).
func (w *Workbench) showProgress(title, message string) *tv.Layer {
    _, layer, _, _ := w.newMessageLayer(title, message, "daemon-progress")
    w.desktop.SetFocus(nil) // no focusable control; the modal swallows input
    return layer
}
```

A button-less modal is safe: `Desktop.SetFocus(nil)` is a no-op-safe path
(`turbotv/desktop.go:1388`), the modal layer blocks underlying input regardless of
focus, and `RemoveLayer` restores focus on teardown. Distinct layer name
`"daemon-progress"` (vs `"confirm-dialog"`) keeps the progress modal
unambiguously identifiable in tests.

### 2. `ui/tui/tui.go` — one tracked field (next to `disconnectLayer`, ~line 619)

```go
// daemonHandoffLayer is the live interim "Migrating…" progress modal during a
// Start/Stop daemon handoff, nil when none is in flight. The result dialog
// replaces it (RemoveLayer) so the two never stack (issue #478). UI thread only.
daemonHandoffLayer *tv.Layer
```

Start (embedded only) and Stop (attached-local only) are mode-exclusive and the
progress modal blocks the menu, so they never run concurrently — one field
suffices.

### 3. `ui/tui/daemon_menu.go` — track + replace, both paths, both directions

```go
func (w *Workbench) startDaemonFromMenu() {
    if w.handlers.StartDaemon == nil || w.daemonHandoffLayer != nil {
        return // guard: no double handoff (mirrors disconnect modal's guard)
    }
    w.daemonHandoffLayer = w.showProgress("Start daemon",
        "Migrating to the local daemon…\nIn-flight turns are cancelled; their partial output is preserved and reappears after reattach.")
    go func() {
        err := w.handlers.StartDaemon()
        w.desktop.Post(func() {
            w.dismissDaemonHandoffProgress() // replace: progress gone before result
            w.rebuildMenu()
            if err != nil {
                w.showConfirm("Start daemon", "Could not start the daemon:\n"+err.Error(), nil)
                return
            }
            w.showConfirm("Start daemon", "The local daemon is running and this TUI is now attached to it. Closing the terminal no longer stops your sessions or watchers.", nil)
        })
    }()
}

// dismissDaemonHandoffProgress removes the interim progress modal if present.
// Idempotent and UI-thread-only, mirroring dismissDisconnectModal.
func (w *Workbench) dismissDaemonHandoffProgress() {
    if w.daemonHandoffLayer == nil {
        return
    }
    w.desktop.RemoveLayer(w.daemonHandoffLayer)
    w.daemonHandoffLayer = nil
}
```

`stopDaemonFromMenu` gets the identical treatment (guard, `showProgress` →
`w.daemonHandoffLayer`, `dismissDaemonHandoffProgress()` first in the completion
callback, then the single success/error `showConfirm`). The progress text and the
two result messages are unchanged from today.

`RemoveLayer(nil)` and removing an absent layer are both safe
(`turbotv/desktop.go:475`), so the dismiss is robust even if the layer was already
gone.

## User-facing behavior

- Click **Daemon → Start daemon**: one "Migrating to the local daemon…" modal
  appears and blocks input for the readiness window; when the handoff finishes it
  is replaced by exactly one result dialog (the attached-success message, or the
  error). Never two at once.
- **Stop daemon**: identical — one "Migrating back to in-process…" modal, then one
  result dialog.
- Error paths show a single error dialog (progress dismissed first).

## Design-criteria assessment

**(1) Goal match.** Pure bug fix: exactly one daemon dialog at a time — progress
while running, single result after — for both Start and Stop, both success and
error. No feature creep; the only refactor (`newMessageLayer`) exists solely to
keep layer creation single-source as the issue requests.

**(2) Usability.** Interim feedback is preserved (replaced, not deleted) across the
15 s window; the result (success **and** error) surfaces as one dialog. The
progress modal blocks input so a second handoff cannot be triggered mid-flight,
and it is button-less because the user has nothing to decide until it completes —
it is status, dismissed programmatically (the disconnect-modal idiom).

**(3) No regressions.** `showConfirm` keeps its exact behavior (same name, buttons,
escape, focus, reflow) — only its internals are factored into `newMessageLayer`,
so every other dialog in the app is unaffected. `RemoveLayer` is nil-safe and
absent-safe. Existing `daemon_lifecycle_issue358_test.go` (menu mode-awareness,
disconnect modal) is untouched by this change. Builds against current `main`
(#476 already merged at HEAD; this fix touches different files —
`daemon_menu.go`, `message_dialog.go`, one `tui.go` field — so no reconciliation).

**(4) Holistic / cross-repo.** gogent-only. The turbotui `Desktop`
`AddLayer`/`RemoveLayer`/`TopLayer`/`SetFocus` + `*tv.Layer` API already provides
everything; **turbotui is not modified**, no `go.mod` bump, no new dependency. The
fix lives in the right place (the menu handlers that own the dialogs) and mirrors
the canonical `disconnect_modal.go` tracked-layer pattern (`disconnectLayer` +
`dismissDisconnectModal`), keeping the two repos' seam intact.

## Testability (partner writes the tests)

Observable seam: the tracked field `w.daemonHandoffLayer` plus
`w.desktop.TopLayer()` (`.Name`). The async completion is drivable with the
existing `drainPostedEventually`/`drainPosted` helpers
(`focus_deferral_issue346_348_test.go`, which drain `app.postQueue`). A test can:

- Build `NewWorkbench(...)`, `SetHandlers{DaemonMode: …, StartDaemon/StopDaemon: …}`.
- Call `startDaemonFromMenu()`; assert `w.daemonHandoffLayer != nil`,
  `TopLayer() == w.daemonHandoffLayer`, `.Name == "daemon-progress"`.
- `drainPostedEventually(w)`; assert `w.daemonHandoffLayer == nil` (progress gone)
  and `TopLayer().Name == "confirm-dialog"` (single result shown), proving the
  result replaced the progress rather than stacking.
- Cover Start + Stop × success + error (handler returns nil vs an error); in the
  error case assert the result dialog is still the lone layer and the progress is
  gone.

## Regression risks (call-outs)

- **`showConfirm` refactor.** The only risk surface; mitigated by making
  `newMessageLayer` carry today's exact construction so `showConfirm`'s output is
  identical. Existing dialog tests guard sizing/behavior.
- **Button-less modal focus.** A modal with no focusable widget + `SetFocus(nil)`:
  verified safe (modal blocks input; focus restored on `RemoveLayer`). If any
  doubt arises in implementation, the zero-risk fallback is to keep an OK button on
  the progress modal (delegating `showProgress` to `showConfirm(...,nil)` and
  returning the layer) — the tracked-field replace still fixes the stack; a manual
  OK click would merely dismiss early (a safe no-op at completion).
- **Double-trigger.** Guarded by `if w.daemonHandoffLayer != nil { return }`, and
  the modal blocks the menu anyway.

## Open questions

None blocking. One implementer's-discretion point, already resolved above: the
progress modal is **button-less** (status-only, programmatic dismissal) to match
the disconnect-modal idiom; the documented fallback (retain the OK button) remains
available if the button-less modal proves awkward in turbotui at implementation
time.
