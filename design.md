# Design — gogent #521: Coalesced-redraw hot path on connect/restore/open

**Branch:** `pair2/coalesced-redraw-hotpath-fix-for-gogent`
**Scope:** pure gogent `ui/tui/`. No turbotui change, no new deps, no `go.mod` bump.
**Issue:** "Hot connect/restore path uses synchronous `Redraw()` instead of coalesced
`RequestRedraw()`."

---

## 1. Mechanism (what the two repos actually do)

The seam between the repos is the turbotui `Desktop` redraw contract. Reading the
source settles exactly what is and isn't ours to change:

- **`Desktop.Redraw()`** (turbotui `turbotv/desktop.go:663`) — synchronous: `compose()`
  (draw every layer of every window) + `updateCursor()` + `app.Apply()` (blocking
  terminal write). One full repaint, in the caller's stack.
- **`Desktop.RequestRedraw()`** (`:679`) — only sets `app.dirty = true`. The run loop's
  `flushDirty()` (turbotui `app.go:379`, called once per loop iteration after
  `drainPosts()`/event dispatch) does **one** `compose+Apply` for the whole iteration
  (issue #17). A burst collapses to a single repaint.
- **`Desktop.Post(fn)`** (`turbotv/desktop.go:125`) wraps `fn` and then calls
  `app.RequestRedraw()` itself. **Anything that runs inside a `Post` callback is already
  coalesced** — the run loop repaints once after draining the whole mailbox. A
  synchronous `Redraw()` inside a `Post` callback is therefore *redundant*: it forces an
  extra mid-drain `compose+Apply` and then the loop's `flushDirty()` is the real (latest)
  frame. This is the key fact the audit leans on.
- **`Desktop.AddLayer`** (`:270`) ends in a synchronous `d.Redraw()`. This is structural
  turbotui behaviour we **cannot** change (no turbotui edits) and there is no
  `AddLayerNoRedraw`/batch API. So opening a window always costs one synchronous compose
  from the toolkit; our job is to stop *adding* gogent-side synchronous composes on top
  of it during a burst.
- **Loop start** `app.Run` (`app.go:818-819`) does `invalidateFront()` + `Apply()` — one
  guaranteed full repaint of the final state when the event loop begins. This is why a
  `RequestRedraw()` issued *before* the loop runs (pre-loop restore) is never a lost
  frame.

### The hot path today

`Run()` (`tui.go:2677-2696`) restores sessions **before** `desktop.Run()` starts:

```
for rs := range restored:  AdoptSession(rs)
                              └─ openWindow → openWindowAny
                                   ├─ desktop.AddLayer(sw.layer)   ← turbotui sync Redraw (structural)
                                   ├─ sidebar.addSession(...)
                                   └─ w.refreshOverall()           ← (C) per-window stats recompute
                              └─ sw.restore(msgs)                  (no redraw; batched addAll, #519)
                              └─ rebuildMenu()                     ← ends in sync desktop.Redraw()  (A)
applyLayout(...)                                                   (no redraw)
```

The reconnect burst (`disconnect_modal.go:147 refreshAfterReconnect`) does the same work
but **inside `desktop.Post(...)`** callbacks — so each `AdoptSession`/`reload` is already
under Post's implicit coalescing, except the synchronous `Redraw()`s inside `rebuildMenu`
(and the toolkit's `AddLayer`) puncture it with mid-drain flushes.

`refreshOverall()` (`tui.go:2800`) is the expensive part of (C): it calls
`handlers.GetStatistics()`, folds it through the lifetime accumulator, and pushes it to
the sidebar — once **per restored window**. `scheduleOverallRefresh()` (`tui.go:2753`) is
the existing 250 ms coalescer already used for live session events
(`deliverSessionEvent:2496`).

**Note:** the live-streaming path is *already* coalesced — `deliverSessionEvent` issues no
synchronous `Redraw()`; it rides Post's implicit redraw. So "flooded stream" needs no
change; the gap is purely connect/restore/open.

---

## 2. The change

### A + C — the load-bearing hot-path fix (`ui/tui/tui.go`)

**(C) `openWindowAny` (~`tui.go:1733`)** — defer the expensive Overall recompute:

```go
// Keep the sidebar's focus/TODO tracking current immediately (cheap: no stats),
// but defer the expensive Overall recompute (GetStatistics + fold) so a connect/
// restore burst coalesces N recomputes into one ~250ms after it settles, matching
// how live session events refresh the panel (issue #521 / #53).
if w.sidebar != nil {
    w.sidebar.focusSession(w.ActiveID())
}
w.scheduleOverallRefresh()
```

This replaces the single `w.refreshOverall()` call. **Why split it:** `refreshOverall`
does `sidebar.focusSession(...)` *before* its `GetStatistics == nil` guard, whereas
`scheduleOverallRefresh` early-returns when `GetStatistics == nil`. A blind swap would
drop the focus/TODO highlight update on window-open when no stats handler is wired — a
regression. `focusSession` is cheap (sets `s.focused`, selects the tree node; no stats),
so it stays immediate per-open; only the costly stats recompute is deferred. The deferred
`scheduleOverallRefresh` later settles the final aggregate against the final `ActiveID()`.

**(A) `rebuildMenu` trailing redraw (~`tui.go:1039`)** — `w.desktop.Redraw()` →
`w.desktop.RequestRedraw()`. This is the "per-window redraw during restore"
(`AdoptSession`/`OpenAnalysisSession` call `rebuildMenu` once per window). `rebuildMenu`
only rebuilds the menu-bar model; it never precedes a blocking read, so a deferred paint
is always correct. Serviced in every context it runs:
- pre-loop restore → folded into `app.Run`'s initial `Apply`;
- reconnect (Post) → the post-drain `flushDirty`;
- in-loop user actions (rename/pin/close/reorder) → the event-dispatch `flushDirty`.

### B — bounded, documented audit of the other burst-prone sites

**Converted (each runs inside a `Post`, so the synchronous `Redraw()` is redundant and
anti-coalescing; all are status/sidebar updates — B's explicit target category):**

| Site | Why safe to defer |
|---|---|
| `tickBusyStatuses` busy/background/fold/watcher redraw (`tui.go:~2933`) | Runs via `desktop.Post(w.tickBusyStatuses)` (`:2880`). Post already requests a coalesced redraw; the trailing `Redraw()` forces an extra mid-drain flush while many `deliverSessionEvent` posts drain alongside it. Status markers; final frame correct. |
| `tickBusyStatuses` per-tick status + Overall redraw (`tui.go:~2944`) | Same Post context; same justification. |
| `scheduleOverallRefresh` AfterFunc (`tui.go:~2761`) | Body runs inside `desktop.Post(...)`; the `Redraw()` after `refreshOverall()` is redundant. This is the coalesced terminus that (C) now routes window-open through, so deferring it keeps the whole path consistently loop-flushed. |

**Left synchronous (must-paint-now or not connect/restore-burst — documented, NOT
converted):**

- `Focus` (`tui.go:1770`) — a discrete user-driven raise; the user expects the raised
  window on screen now. The restore loop uses `openWindow`, not `Focus`, so it is not on
  the burst path. **Keep `Redraw()`** (also the over-conversion guard in tests).
- `agent_monolog` open (`agent_monolog.go:83`), model-dropdown batch refresh
  (`tui.go:817`, already one batched `Redraw` for all windows), theme switch
  (`tui.go:1235`), `dismissFailed` (`tui.go:1361`), `transcriptDo` (`tui.go:1395`),
  sidebar-pin toggle (`tui.go:2089`) — each is a single discrete action, one compose,
  not a burst. Keep.
- All dialog files (`command_palette.go`, `commands_dialog.go`, `question_dialog.go`,
  `permission_dialog.go`, `resources_dialog.go`, `sessions_dialog.go`,
  `settings_dialog.go`, `watchers_dialog.go`, `theme_editor.go`,
  `keybinding_customizer.go`, `statistics_dialog.go`, `command_history_dialog.go`,
  `mention_completer.go`, `session_window.go promptFind`, `tiling.go`) — user-input-paced,
  in-loop, one compose per discrete keystroke/click; the modal prompts must show before
  the user answers. Out of the connect/restore scope; converting them is scope creep with
  no burst to coalesce. **Keep.**

Total functional change: **2 hot-path edits + 3 documented audit conversions, all in
`tui.go`.** No file outside `tui.go` changes (audit conclusion: leave).

---

## 3. The four gates

**(1) Goal match.** Exactly the issue's ask, no scope creep: the connect/restore/open
path requests coalesced redraws (`rebuildMenu` → `RequestRedraw`), `refreshOverall` in a
burst is coalesced via `scheduleOverallRefresh` (C), synchronous `Redraw()` is reserved
for must-paint-now/single-action sites, and the wider audit is bounded and documented
rather than a blanket convert. It is a fix, not a feature or refactor. Uses the turbotui
`RequestRedraw` that already exists.

**(2) Usability.** Fewer full repaints during a connect burst: opening N restored windows
drops from `N` `AddLayer` composes **+ N** `rebuildMenu` composes **+ N** stats recomputes
to `N` `AddLayer` composes **+ 1** coalesced flush **+ 1** stats recompute. No new dialog
or input surface — the user drives nothing differently. The only visible timing change is
the Overall-panel *aggregate* lagging ≤250 ms after a window opens, which already matches
how live session events refresh that panel (`deliverSessionEvent`). The window itself, its
transcript, and its sidebar row still appear immediately (`AddLayer` + `addSession` +
immediate `focusSession`). Final frame after the burst is the correct latest state.

**(3) No regressions.** Must-paint-now paths (`Focus`, modal dialogs, theme/transcript
single-actions) keep `Redraw()`. The `GetStatistics == nil` focus-tracking regression is
explicitly avoided by keeping `focusSession` immediate. `rebuildMenu`'s `RequestRedraw` is
serviced in all three contexts it runs (pre-loop initial `Apply`, reconnect Post-drain,
in-loop dispatch) — no lost frame. The batched restore (`addAll`, #519) and the
duplicate-window guards (#518) are untouched. `ui/tui` keeps its clean boundary (no
`internal/daemon`/`internal/server` imports — verified). gofmt/build/vet/golangci-lint and
`go test ./...` expected green (pre-existing `TestUserSessionSendMessage` 404 aside).

**(4) Holistic / repo seam.** All edits are gogent `ui/tui/tui.go`, consuming the existing
turbotui `RequestRedraw`/`Post`/`scheduleOverallRefresh` contract. turbotui is unchanged:
`AddLayer`'s structural synchronous redraw stays (no `AddLayerNoRedraw` exists and we may
not add one), and we deliberately do **not** push the coalescing down into the toolkit.
Downstream effect on turbotui: none. No new deps, no `go.mod` bump.

---

## 4. Tests (`ui/tui`, mirroring existing style; no daemon/server imports)

The desktop field is a concrete `*tv.Desktop`, so we spy the way the existing
`close_session_flash_test.go` does — a passive bottom **frame-spy** layer whose `DrawFn`
fires once per `compose()`. Each synchronous `Redraw()`/`AddLayer` = one `DrawFn` tick;
`RequestRedraw()` ticks zero (no compose without a running loop). This counts real
composes without touching the app writer and needs only `tv` + `config` + `gogent` layout
types already used by sibling tests.

1. **Coalesced restore burst.** Build a Workbench, install the frame-spy, restore/open N
   windows (`AdoptSession`×N). Assert composes ≈ N (the unavoidable per-`AddLayer` paint),
   **not** ~2N — proving `rebuildMenu` no longer composes synchronously per window.
2. **`refreshOverall` deferred (C).** Wire a `GetStatistics` handler that counts calls.
   Open N windows; assert the counter did **not** advance synchronously (the recompute is
   deferred to the 250 ms timer), i.e. `openWindowAny` does not compute stats per window.
   Pair with an assertion that the sidebar focus highlight *did* update immediately (the
   preserved cheap `focusSession`), guarding the `GetStatistics == nil` path.
3. **Must-paint-now preserved (over-conversion guard).** Assert a deliberately-synchronous
   site still composes in-call: a direct `w.desktop.Redraw()` ticks the spy by exactly one,
   and `Focus(id)` (kept synchronous) still produces synchronous composes — so a future
   blanket-convert would fail this test.
4. **`rebuildMenu` defers.** Call `rebuildMenu()` in isolation (no running loop) and assert
   the spy did not tick (it armed `RequestRedraw`, not `Redraw`).

These pin both directions: the burst coalesces, and the reserved synchronous sites stay
synchronous.

---

## 5. Open questions

1. **`scheduleOverallRefresh` / `tickBusyStatuses` conversions (B):** included because they
   are provably-redundant in-`Post` status/sidebar redraws squarely in the issue's stated
   target set. They are low-risk but *not* load-bearing for the headline fix. If the
   reviewer prefers the absolute minimal diff, they can be dropped without affecting gates
   1–4; the A+C edits alone satisfy the acceptance criteria. Leaning **include** (consistent
   coalescing), open to dropping.
2. **Overall-count latency on a single `Ctrl+N`:** (C) makes a lone new-session's Overall
   aggregate update ≤250 ms later instead of synchronously. This matches the existing
   live-event coalescing and is judged correct, not a regression — flagging in case product
   wants the single-open case to stay instant (would require a "burst-only" heuristic, which
   adds state for little gain).
