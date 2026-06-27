# Design — Daemon-aware quit dialog (gogent issue #503)

> Make the exit/quit confirmation **daemon-aware** (Option 2: three-button,
> status-enriched). The dialog must tell the user what quitting will *actually*
> do — close the client while the daemon survives (attached), or kill everything
> (embedded) — and offer the relevant explicit handoff, **without changing quit
> semantics** (`w.quit()` stays the only teardown; the daemon is never started or
> stopped implicitly).

gogent-only. **No turbotui changes**, **no new deps**, **no `go.mod` bump.** All
edits land in `ui/tui`.

---

## 1. Scope & entry point

`ui/tui/tui.go` `confirmQuit()` (currently `~L1452`) is the single choke point
for all three quit gestures:

- Ctrl+C (`~L2557`), File→Exit `actionAppQuit` (`~L958`), command-palette "Quit"
  (`command_palette.go:277`) all call `confirmQuit()`.

Updating `confirmQuit()` therefore covers every gesture. Today it is:

```go
func (w *Workbench) confirmQuit() {
    w.showConfirm("Quit Gogent", "Are you sure you want to quit?", func(yes bool) {
        if yes && w.quit != nil { w.quit() }
    })
}
```

We replace the body so that:
- **`Handlers.DaemonMode == nil`** → byte-for-byte the existing
  `showConfirm("Quit Gogent","Are you sure you want to quit?", …)` Yes/No box,
  default **No**. (State 6, graceful degradation — keeps existing tests green.)
- **otherwise** → a new daemon-aware quit dialog (`w.showQuitDialog(...)`).

No quit gesture, no `w.quit()` call site, and no `showConfirm` signature changes.

---

## 2. Files & functions touched

### `ui/tui/message_dialog.go` (layout helper only)
- **Add** `quitButtonRow(width, btnY int, labels ...string) []tv.Rect` next to
  `confirmButtonRow`/`disconnectButtonRow`. Centres N (2 or 3) buttons on row
  `btnY`, each sized via `tv.ButtonLabelWidth`, constant `gap = 4`, each rect
  clamped to `[2, width-3]` via `clampDialogRect` (same idiom as the existing
  two helpers). Returns one rect per label, in order.
- No change to `newMessageLayer`/`showConfirm`/`showProgress` — they are reused
  verbatim.

### `ui/tui/quit_dialog.go` (NEW file — keeps the quit path localized & off the
contended `tui.go`/`theme.go` lines that #500g/#501g also touch)
Holds everything new, all in `package ui`:

1. **`quitDialogModel` (pure presentation struct)** — the testable core,
   mirroring how `formatDaemonStatus`/`daemonIndicatorText` are pure:
   ```go
   type quitDialogModel struct {
       Title      string
       Body       string
       Buttons    []quitButton // ordered; each has Label + action kind
       DefaultIdx int          // index into Buttons that receives focus
   }
   type quitButtonKind int // quitClient, stopAndQuit, startAndQuit, cancel
   ```
2. **`buildQuitModel(mode, report, haveReport bool, host string, canStop, canStart bool) quitDialogModel`**
   — pure function deciding title/body/buttons/default from inputs. `canStop` =
   `Handlers.StopDaemon != nil`, `canStart` = `Handlers.StartDaemon != nil`.
   Body text built by small pure helpers (see §4). **No UI, no goroutines** → unit
   tested directly for all six states + fallbacks.
3. **`pluralSessions/pluralWatchers/pluralServers`** (or one
   `countLine(n int, noun string) string`) helper: "1 live session" vs
   "N live sessions" (the spec notes `formatDaemonStatus` does not pluralise; new
   helper added here, not retrofitted into the status dialog — no #500 overlap).
4. **`showQuitDialog()`** — the wiring: builds the layer via `newMessageLayer`,
   lays out buttons via `quitButtonRow`, wires actions, installs Escape, sets
   default focus, then kicks the **non-blocking** status fetch (see §5).
5. **`quitButtonAction(kind)`** closures for the three semantics (see §6),
   reusing `showProgress`/`dismissDaemonHandoffProgress`/`showConfirm`.

### `ui/tui/tui.go`
- Rewrite `confirmQuit()` to branch (nil → old box; else `showQuitDialog`).
- **No new `Handlers` field unless required** — see §7 (`ReconnectAddress` is an
  open question; default plan reuses `reconnectHost`).

### turbotui
- **None.** `Dialog`, `ModalLayer`, `Button`, `newMessageLayer`,
  `ButtonLabelWidth`, `WrapText` already exist. Seam respected.

---

## 3. The six render states (branch on `DaemonMode()` + snapshot arrival)

| # | Mode | Snapshot | Title | Buttons (order) | Default |
|---|------|----------|-------|-----------------|---------|
| 1 | AttachedLocal | yes | `Quit Gogent (daemon stays running)` | Quit client / Stop daemon & quit / Cancel | Cancel |
| 2 | AttachedLocal | no/slow/fail | same | same (un-enriched body) | Cancel |
| 3 | AttachedRemote | yes | `Quit Gogent (daemon stays running)` | Quit client / Cancel (NO Stop) | Cancel |
| 4 | AttachedRemote | no/slow/fail | same | same (un-enriched body) | Cancel |
| 5 | Embedded | n/a | `Quit Gogent — stops all sessions` | Quit (stops all) / Start daemon & quit / Cancel | Cancel |
| 6 | `DaemonMode == nil` | n/a | `Quit Gogent` | Yes / No (unchanged box) | No |

Button omission (graceful degradation):
- Omit **Stop daemon & quit** if `Handlers.StopDaemon == nil` (state 1/2 → two
  buttons: Quit client / Cancel).
- Omit **Start daemon & quit** if `Handlers.StartDaemon == nil` (state 5 → two
  buttons: Quit (stops all) / Cancel).
- AttachedRemote never offers Stop (Stop only drives the **local** daemon).

Default focus is always the safe/no-op choice (Cancel, or No for nil) per the
usability gate.

---

## 4. Body copy (exact, from the issue)

**AttachedLocal-enriched** (state 1):
```
Quitting closes this TUI client only.
The local daemon keeps running:

  • {N} live session(s)
  • {M} watcher(s)
  • {K} MCP server(s)

Re-attach later with:  gogent
```
`{N}/{M}/{K}` from `report.LiveSessions / report.Watchers / len(report.MCPServers)`,
pluralised ("1 live session", "2 live sessions", …).

**AttachedLocal-fallback** (state 2):
```
Quitting closes this TUI client only.
The local daemon keeps running — your sessions, watchers and
MCP servers continue in the background.

Re-attach later with:  gogent
```

**AttachedRemote-enriched** (state 3): as local-enriched but
`The daemon at {host} keeps running:` and the re-attach line
`Re-attach later with:  gogent --connect {addr}`.

**AttachedRemote-fallback** (state 4): `…your sessions and watchers continue in
the background.` + the `--connect {addr}` re-attach line.

`{host}`/`{addr}`: reuse `w.reconnectHost` (already wired by
`SetReconnectControls`, same source the disconnect modal uses). **Omit the
re-attach line entirely if `reconnectHost == ""`** (per spec). See §7 / open
questions re. `{addr}` vs `{host}`.

**Embedded** (state 5):
```
You are running embedded (no daemon).
Quitting stops ALL sessions and watchers in this process;
in-flight turns are cancelled.

To keep your work running after you leave, start the
daemon first.
```

**Nil** (state 6): `Are you sure you want to quit?` (unchanged).

All bodies fit within `messageMaxWidth = 80`; the body TextView wraps/scrolls as
`newMessageLayer` already handles.

---

## 5. Non-blocking status fetch (the critical usability requirement)

The dialog **opens immediately** with the mode-based **fallback** body (state
2/4 text for attached; state 5 for embedded; states with no round-trip need
nothing). For AttachedLocal/AttachedRemote we then fetch enrichment off the UI
thread, mirroring `showDaemonStatusDialog` and the disconnect modal's
update-in-place:

```go
func (w *Workbench) showQuitDialog() {
    mode := w.handlers.DaemonMode()
    model := buildQuitModel(mode, DaemonStatusReport{}, false /*haveReport*/, w.reconnectHost,
        w.handlers.StopDaemon != nil, w.handlers.StartDaemon != nil)
    dialog, layer, width, bodyH := w.newMessageLayer(model.Title, model.Body, "quit-dialog")
    body := /* the dialog's body TextView (kept, like w.disconnectBody) */
    // ... lay out model.Buttons via quitButtonRow(width, bodyH+2, labels...),
    //     wire each to its action, install Escape=Cancel, SetFocus(default).

    // Enrich in place, only for attached modes and only if status is wired.
    if (mode == DaemonModeAttachedLocal || mode == DaemonModeAttachedRemote) &&
        w.handlers.DaemonStatusInfo != nil {
        go func() {
            report, err := w.handlers.DaemonStatusInfo()
            w.desktop.Post(func() {
                if err != nil { return }                 // keep fallback text
                if w.quitDialogLayer != layer { return } // user already acted / dialog gone
                enriched := buildQuitModel(mode, report, true, w.reconnectHost, …)
                rewriteBody(body, enriched.Body)         // Clear()+AddLine loop, ScrollToTop
                w.desktop.RequestRedraw()
            })
        }()
    }
}
```

Guards:
- The fetch **never** blocks the quit; the box is interactive the instant it
  opens. If the daemon is unreachable / slow / errors, the fallback text stays.
- A small `w.quitDialogLayer *tv.Layer` field tracks the live quit dialog (like
  `disconnectLayer`/`daemonHandoffLayer`); set on open, cleared on dismiss. The
  enrichment callback no-ops if the user already clicked a button (layer gone or
  replaced). Button labels/focus are **not** rebuilt on enrichment — only the
  body text is rewritten (counts never change which buttons exist), so focus and
  layout stay stable. Touched only on the UI thread (open + `Post`ed callback).
- The quit box stacks **above** an open disconnect modal exactly as today (it is
  just another modal layer); we do not special-case that.

---

## 6. Button semantics (quit semantics unchanged)

- **Quit client** / **Quit (stops all)** → dismiss layer, then `w.quit()` (the
  only teardown; daemon untouched in the attached case).
- **Stop daemon & quit** (AttachedLocal only) → dismiss the quit layer, then run
  the **existing** handoff: `w.daemonHandoffLayer = w.showProgress("Stop daemon",
  "Migrating back to in-process…")`, `go w.handlers.StopDaemon()`, and in the
  `Post`ed completion `dismissDaemonHandoffProgress()`; **on success `w.quit()`**;
  **on failure** `showConfirm("Stop daemon","Could not stop the daemon:\n"+err, nil)`
  and **STAY ALIVE**. (Reuses `stopDaemonFromMenu`'s proven idiom — factor the
  shared handoff into a small helper, e.g. `runStopDaemon(after func())`, so the
  menu path passes its "back to embedded" message and the quit path passes
  `w.quit` as the success continuation. No behavioural change to the menu path.)
- **Start daemon & quit** (Embedded only) → same shape with
  `showProgress("Start daemon","Migrating to the local daemon…")` +
  `Handlers.StartDaemon()`; on success `w.quit()` (daemon stays up, work
  survives), on failure error + STAY ALIVE.
- **Cancel** / **Escape** → dismiss the layer, do nothing.

Because Stop/Start run the real `showProgress` handoff and only call `w.quit()`
on success, "no implicit daemon start/stop" holds — the daemon transitions
*only* via the explicit button the user pressed.

---

## 7. Button-row layout & narrow-terminal degradation

`quitButtonRow(width, btnY, labels...)` centres the row; total = sum of
`ButtonLabelWidth(label)` + `gap*(n-1)`. **Narrow-terminal fallback:** if the
three-button total would exceed the usable interior (`width-4`), drop the
**middle** button (Stop/Start) and lay out the remaining two. The handoff stays
reachable from the **Daemon menu**, so nothing is lost — this is the documented
degradation. `showQuitDialog` computes this from the resolved `width` and the
already-known labels, so the dropped-button decision and the wiring agree (we
only wire actions for buttons we actually place).

`{host}`/`{addr}`: **plan A (default)** reuse `w.reconnectHost` for both the
`{host}` in the body and the `{addr}` in `gogent --connect {addr}`. It is the
attach target already surfaced to the user in the disconnect modal. **Plan B**
(only if A proves wrong — e.g. `reconnectHost` is a display label that is not a
valid `--connect` argument) add `Handlers.ReconnectAddress func() string`
(cheap, synchronous, like `ConnectionLabel`) and use it for `{addr}` only. This
is the *single* candidate new `Handlers` field and is gated on need — see Open
questions.

---

## Design criteria

### (1) Goal match
Does exactly what #503 asks: a **feature** that makes the quit confirmation
branch on `DaemonMode` into the six states with the specified
titles/bodies/buttons/default-focus. Attached warns the daemon survives (local
also offers Stop & quit); embedded warns quitting kills everything (offers Start
daemon & quit); nil falls back to today's box. No scope creep — quit semantics,
the menu-bar indicator (#500), and turbotui are untouched. No refactor beyond
extracting the shared Stop/Start-handoff helper (pure win, no behaviour change).

### (2) Usability
Opens **instantly** with mode-correct fallback text and **enriches in place**
when the snapshot arrives — never blocks on a daemon round-trip (the explicit
requirement). Default focus is the safe choice (Cancel; No for nil) so a reflex
Enter cancels. Only **explicit** buttons start/stop the daemon — no implicit
lifecycle change. The user can drive every path (mouse/Enter/mnemonic via
`tv.Button`; Escape = Cancel). The right thing is surfaced, not silent: live
counts (or an honest fallback) and the exact re-attach command. Theme-aware via
the `newMessageLayer` body palette (`DialogFG/BG`) — same as `showConfirm`.

### (3) No regressions
`w.quit()` is still the only teardown and is reached by all three gestures
through the unchanged `confirmQuit` choke point. `DaemonMode == nil` reproduces
today's Yes/No box (existing `confirmQuit`/`showConfirm` tests stay green).
Graceful degradation when `DaemonStatusInfo`/`StartDaemon`/`StopDaemon` are nil
(fallback body; button omitted). The status fetch is `Post`-marshalled and
no-ops if the dialog is gone, so no off-thread mutation and no use-after-dismiss.
The shared handoff helper keeps `stopDaemonFromMenu`/`startDaemonFromMenu`
behaviour identical (same messages, same #478 single-dialog guarantee). gofmt /
go vet / build / `golangci-lint` (0 NEW) / `go test ./...` (no `-race`, per the
Pi5 dev gate) must stay green; pre-existing `TestUserSessionSendMessage` 404 is
the only accepted failure.

### (4) Holistic design (both repos)
The change sits where the seam dictates: **all** behaviour in `ui/tui`
(`quit_dialog.go` + a layout helper in `message_dialog.go` + the `confirmQuit`
rewrite in `tui.go`), reusing the existing `newMessageLayer`/`showProgress`/
`dismissDaemonHandoffProgress`/handoff primitives. ui/tui stays free of
`internal/daemon` and `internal/server` (it consumes only the
`Handlers`/`DaemonStatusReport` UI-facing types, exactly as the status dialog
does). turbotui needs nothing — its `Dialog`/`Button`/`ModalLayer`/`WrapText`
primitives already cover a 3-button content-sized dialog — so the repo seam is
respected and there is no downstream effect on turbotui and no `go.mod` bump.

### Regression risks (called out)
- **Orchestration:** #500g/#501g also edit `ui/tui` (`tui.go`/`theme.go`).
  Keeping the bulk in a new `quit_dialog.go` and touching only `confirmQuit` in
  `tui.go` minimises the rebase surface; tasks are serialized, rebase at gate.
- **Narrow terminal:** mitigated by the documented middle-button drop; tested.
- **Enrichment race:** mitigated by the `w.quitDialogLayer == layer` guard +
  UI-thread-only mutation.
- **`{addr}` correctness:** if `reconnectHost` is not a valid `--connect` arg the
  re-attach line could mislead — Open question Q1; Plan B (`ReconnectAddress`)
  is the fallback.

---

## Tests (new `ui/tui/quit_dialog_test.go`; mirror
`daemon_lifecycle_issue358_test.go` / `daemon_handoff_dialog_issue478_test.go`)

- **Pure `buildQuitModel` table test** over all six states: assert Title, Body
  (enriched and fallback), ordered Button labels, and `DefaultIdx`. Covers
  pluralisation ("1 live session" vs "3 live sessions") and the host/addr lines.
- **Button-omission:** `StopDaemon==nil` (local) / `StartDaemon==nil` (embedded)
  drop the middle button; AttachedRemote never has Stop.
- **Nil `DaemonMode`:** `confirmQuit` builds the unchanged `confirm-dialog`
  Yes/No box, default No (existing behaviour preserved).
- **Non-blocking fetch:** with a blocking/slow/erroring `DaemonStatusInfo`, the
  dialog is up immediately with fallback text; on a released successful fetch the
  body is rewritten to enriched (assert via the body TextView's `AllText()`); on
  error the fallback text remains.
- **Stop/Start-daemon-&-quit:** wire a channel-blocked `StopDaemon`/`StartDaemon`;
  assert the `daemon-progress` modal shows during the handoff, then on **success**
  `w.quit` is invoked (spy func), and on **failure** a result `confirm-dialog`
  shows and `w.quit` is **not** called (STAY ALIVE).
- Keep existing quit/showConfirm tests passing; assert ui/tui imports neither
  `internal/daemon` nor `internal/server` (an import-guard test already exists in
  the suite pattern — extend or rely on it).

---

## Open questions

1. **`{addr}` vs `{host}` for the remote re-attach line.** Is `w.reconnectHost`
   a valid `gogent --connect` argument (host:port / ssh target), or only a
   display label? If the latter, adopt Plan B (`Handlers.ReconnectAddress`).
   Default assumption: reuse `reconnectHost`; omit the line when empty. *(Does
   not block design or the pure-model tests, which take `host` as a parameter.)*
2. **Mnemonics on the three buttons.** `tv.Button` derives a mnemonic from `&`
   in the label. The issue's labels ("Quit client", "Stop daemon & quit", …)
   have no `&`. Add Alt-mnemonics (e.g. `&Quit client`, `S&top daemon & quit`)?
   Low-risk nicety; default is to follow the issue's exact label text (no `&`)
   unless you want mnemonics — easy to add.
3. **Embedded + no `StartDaemon`:** body still warns "start the daemon first"
   while no Start button is offered. Keep the copy (it is still true advice — the
   daemon can be started another way) or trim the last sentence when
   `StartDaemon==nil`? Default: keep the issue's exact copy.
