# Issue #209 — Close-path session flash

## Problem

`Ctrl+W` → `Workbench.CloseActive()` → `Workbench.CloseSession(id)` (`ui/tui/tui.go`)
tears the active session down as a sequence of toolkit calls, several of which
synchronously flush a full frame:

1. `desktop.RemoveLayer(sw.layer)` — paints the **z-stack neighbour** that sat
   directly beneath the closed window (`Desktop.TopLayer()`), which is *wrong*
   whenever the z-stack order differs from `w.order`.
2. `rebuildMenu()` — paints again (ends in `desktop.Redraw`).
3. `Focus(last)` — finally raises `w.order`'s tail, the intended session.

So the desktop momentarily paints a session the user did not ask for (frame 1),
then snaps to the final one (frame 3): the visible flash.

The root cause is that two orderings are maintained independently: the desktop
**z-stack** (reordered on every Focus/click) and `w.order` (the sidebar order,
moved only by pin/reorder/layout). `RemoveLayer` reveals the z-stack neighbour;
the final `Focus` raises the `w.order` tail. They disagree once the user clicks
between sessions.

## Decision: which session becomes active after close

**The tail of `w.order` (the last entry in the sidebar order).** This is the
same session the old code eventually settled on — we keep *which* session wins
unchanged and only fix *when/how* it is shown, so persisted layout and existing
behaviour stay stable. This task is the close-path sibling of #206; the general
"sidebar highlight follows focus" fix lives there.

## Fix (no toolkit changes, no new deps)

The toolkit exposes no batch/suspend-redraw API, so instead of collapsing the
frames we make **every** frame paint the intended target:

1. Compute `target` (the `w.order` tail) **up front**, under the lock, right
   after pruning `id` from `w.order` — before any teardown. The choice never
   depends on the transient z-stack the removal would otherwise expose.
2. If `target != ""`, call `w.Focus(target)` **before** `RemoveLayer(sw.layer)`.
   `Focus` re-adds the target layer, raising it to the **top of the z-stack,
   above the still-present closing window**. From this point target is the
   top-most window, so:
   - the Focus redraw paints target (correct),
   - the subsequent `RemoveLayer(sw.layer)` can only reveal target — never the
     arbitrary z-neighbour — because the closing layer now sits *below* target,
   - `rebuildMenu`'s redraw still paints target.
   `Focus` also routes the sidebar's focus (Overall/TODO regions, via
   `refreshOverall` → `sidebar.focusSession`) onto target in the same step.
3. Then remove the closed layer, prune the sidebar node, refresh, persist, and
   rebuild the menu as before.

Net: the only visible transition is closing-window → target. No third,
wrong-session frame is ever painted, for any divergence between the z-stack and
`w.order`.

Empty case (closing the last session): `target == ""`, `Focus` is skipped,
`RemoveLayer` leaves an empty desktop — a single correct frame.

## Files

- `ui/tui/tui.go` — restructure `CloseSession`. No interface changes; the public
  methods `CloseSession(id)`, `CloseActive()`, `ActiveID()`, `Focus(id)` keep
  their signatures and contracts.

## Tests (GLM partner writes these)

With the z-stack order differing from `w.order`, closing the active session must
select the intended next session (`w.order` tail) with **no intermediate
different-session frame**: assert the raised/focused window after `CloseSession`
is the target, and that no wrong window is ever raised mid-operation (e.g. via a
desktop spy recording the sequence of top-layer raises). Target a Workbench with
≥3 sessions whose z-stack has been reordered (Focus) away from `w.order`.
Ignore only the environmental `TestUserSessionSendMessage` 404.
