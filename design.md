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

Updating `confirmQuit()` therefore covers all three *confirmable* quit gestures.
One quit path deliberately bypasses it: the disconnect modal's "Quit" button
(`disconnect_modal.go:93`) calls `w.QuitFunc()` directly. That is intentional and
unchanged — the disconnect modal's own body already states the daemon survives,
and re-confirming inside a blocking "connection lost" modal would be hostile, so
this design leaves that path alone. Today `confirmQuit` is:

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

### `ui/tui/message_dialog.go` (layout helper + one return-value change)
- **Add** `quitButtonRow(width, btnY int, labels ...string) []tv.Rect` next to
  `confirmButtonRow`/`disconnectButtonRow`. Centres N (2 or 3) buttons on row
  `btnY`, each sized via `tv.ButtonLabelWidth`, constant `gap = 4`, each rect
  clamped to `[2, width-3]` via `clampDialogRect` (same idiom as the existing
  two helpers). Returns one rect per label, in order.
- **Change `newMessageLayer` to also return the body `*tv.TextView`.** Today it
  returns `(*tv.Dialog, *tv.Layer, int, int)` and the body TextView it builds at
  `message_dialog.go:105` is unreachable by callers — so the quit dialog could
  not enrich it in place (see §5). New signature:
  `(*tv.Dialog, *tv.Layer, *tv.TextView, int, int)` (dialog, layer, body, width,
  bodyH). The two existing callers — `showConfirm` (`L140`) and `showProgress`
  (`L186`) — are updated to discard the new value
  (`dialog, layer, _, width, bodyH := …` / `_, layer, _, _, _ := …`); both are
  behaviourally unchanged (they never enrich their body). This keeps the shared
  content-sizing + resize-reflow in one place rather than forcing the quit dialog
  to hand-build its body like `showDisconnectModal` does (which would lose that
  reuse). This is the *only* turbotui-adjacent surface touched — and it is in
  gogent, not turbotui (turbotui already exposes `TextView.AllText()` for tests).

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
2. **`buildQuitModel(mode, report, haveReport bool, host, addr string, canStop, canStart bool) quitDialogModel`**
   — pure function deciding title/body/buttons/default from inputs. `canStop` =
   `Handlers.StopDaemon != nil`, `canStart` = `Handlers.StartDaemon != nil`.
   `host` is the display label (`reconnectHost`) used in "The daemon at {host}…";
   **`addr` is the connect string** (`ReconnectAddress()`, "" when nil/unset)
   used in the `gogent --connect {addr}` re-attach line — the two are distinct
   parameters precisely so §4's "do not conflate" rule is enforced by the
   signature, and so the pure test can assert the `--connect {addr}` line. When
   `addr == ""` the re-attach line is omitted. Body text built by small pure
   helpers (see §4). **No UI, no goroutines** → unit tested directly for all six
   states + fallbacks.
3. **`pluralSessions/pluralWatchers/pluralServers`** (or one
   `countLine(n int, noun string) string`) helper: "1 live session" vs
   "N live sessions" (the spec notes `formatDaemonStatus` does not pluralise; new
   helper added here, not retrofitted into the status dialog — no #500 overlap).
4. **`quitSizingBody(mode, host, addr string) string`** (pure) — the enriched
   body *shape* with placeholder counts, used only to size the dialog so a later
   in-place enrich never clips the re-attach line (see §5). For embedded/nil
   modes (no enrichment) it returns the same body the model renders.
5. **`reconnectAddress() string`** — nil-safe accessor:
   `if w.handlers.ReconnectAddress != nil { return w.handlers.ReconnectAddress() }`
   else `""`. Keeps the call sites and the pure model free of nil checks.
6. **`showQuitDialog()`** — the wiring: builds the layer via `newMessageLayer`
   (sized via `quitSizingBody`), renders the fallback body, lays out buttons via
   `quitButtonRow`, wires actions, installs Escape, sets default focus, then
   kicks the **non-blocking** status fetch (see §5).
7. **`quitButtonAction(kind)`** closures for the three semantics (see §6),
   reusing `showProgress`/`dismissDaemonHandoffProgress`/`showConfirm`.

### `ui/tui/tui.go`
- Rewrite `confirmQuit()` to branch (nil → old box; else `showQuitDialog`).
- **Add `Handlers.ReconnectAddress func() string`** (cheap, synchronous, like
  `ConnectionLabel`; may be nil) returning the raw `--connect` argument for the
  attached-remote re-attach line. Required for SSH correctness — see §7. The
  field declaration lives in `ui/tui/tui.go` (the `Handlers` struct); a new
  `w.reconnectAddr` field is *not* added (the closure is enough).

### `cmd/` (wiring only — populate the new handler)
- Where `wb.SetReconnectControls(hostLabel(addr), …)` is called
  (`cmd/attach.go:183`, `cmd/handoff.go:282`), also set
  `ReconnectAddress: func() string { return addr }` so the re-attach line shows
  the *actual* connect string. `addr` is already in scope at both sites. This is
  the only edit outside `ui/tui`; it adds no dependency and changes no existing
  behaviour (the field is otherwise unread). ui/tui stays free of
  `internal/daemon`/`internal/server`.

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

**Button-label `&` escaping (render-correctness — required).** The displayed
labels above contain a literal ampersand: `Stop daemon & quit`,
`Start daemon & quit`. turbotui's `tv.Button` runs every label through
`ParseMnemonic` (`turbotv/measure.go:46`), which treats a lone `&` as a mnemonic
marker — it strips the `&` and flags the *next* rune as the hotkey. So the string
passed to `newButton` must escape it as `&&` (verified: `measure_test.go:58`
`"a&&b"` → `"a&b"`). The labels we construct are therefore
`"Stop daemon && quit"` and `"Start daemon && quit"`; they render as the single
literal `&` the issue specifies. (No mnemonic is added — see Open questions Q1.)
`Quit client`, `Quit (stops all)`, `Cancel` contain no `&` and pass through
unchanged. `tv.ButtonLabelWidth` measures the *rendered* width, so the row
helper sizes correctly regardless.

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

`{host}` (display) vs `{addr}` (connect string) are **different sources** — do
not conflate them:
- `{host}` in "The daemon at {host} keeps running:" → `w.reconnectHost` (the
  human display label `hostLabel(addr)`, same value the disconnect modal shows).
- `{addr}` in "Re-attach later with:  gogent --connect {addr}" →
  `Handlers.ReconnectAddress()` (the *raw* connect argument).

`hostLabel` (`cmd/handoff.go:529`) returns only `u.Host` — for an
`ssh://user@host` attach it yields the bare `host`, dropping the `ssh://` scheme
and the user, so `gogent --connect <reconnectHost>` would be parsed as TCP and be
**wrong**. Hence `{addr}` must come from `ReconnectAddress`, not `reconnectHost`
(see §7). **Omit the entire re-attach line** when `ReconnectAddress` is nil or
returns `""` (per spec); also omit "The daemon at {host}" host phrasing when
`reconnectHost == ""`, falling back to "The daemon keeps running:".

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
update-in-place.

**Sizing — "size for the enriched shape, render the fallback into it"
(prevents the clip).** The enriched body (intro + 3 count bullets + re-attach
line, ~8 rows) is taller than the fallback (~5 rows). If we sized the dialog to
the fallback and later poured the enriched body in, the extra rows would overflow
the body viewport and — since `rewriteBody` does `ScrollToTop` — the **bottom**
line (`gogent --connect {addr}`, the headline re-attach command) would scroll out
of view, exactly in the enriched remote state where it matters most.
`installResizeReflow` (`dialog_sizing.go:201`) only resizes the outer window, not
the inner body/buttons, so a resize won't rescue it either. We therefore **size
the dialog at open to the enriched shape** and render the (shorter) fallback into
that taller viewport, so the layout can stay frozen (focus/buttons never move)
*and* the enriched body always fits. Concretely, `showQuitDialog` passes a
**sizing string** = the enriched-shaped body for this mode built with placeholder
counts (`quitSizingBody(mode, host, addr)`); the displayed body is then
overwritten with the fallback text. Placeholder vs real counts differ by at most
a couple of digits, so the line count is identical and the longest-line width is
unchanged — the sizing is exact. `messageMaxHeight=24` leaves ~10 rows of
headroom over the ~14-row (body+chrome) dialog, so nothing is capped.

```go
func (w *Workbench) showQuitDialog() {
    mode := w.handlers.DaemonMode()
    host := w.reconnectHost
    addr := w.reconnectAddress() // nil-safe: "" when Handlers.ReconnectAddress == nil
    canStop, canStart := w.handlers.StopDaemon != nil, w.handlers.StartDaemon != nil

    model := buildQuitModel(mode, DaemonStatusReport{}, false /*haveReport*/, host, addr, canStop, canStart)

    // Size to the enriched shape so a later in-place enrich never clips the
    // re-attach line; for embedded/nil the sizing string == the body (no enrich).
    sizing := quitSizingBody(mode, host, addr) // == model.Body for non-enriching modes
    // newMessageLayer hands back the body TextView (see §2) and sizes to `sizing`.
    dialog, layer, body, width, bodyH := w.newMessageLayer(model.Title, sizing, "quit-dialog")
    rewriteBody(body, model.Body) // render the fallback into the enriched-sized box
    w.quitDialogLayer = layer
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
                enriched := buildQuitModel(mode, report, true, host, addr, canStop, canStart)
                rewriteBody(body, enriched.Body)         // Clear()+AddLine loop, ScrollToTop — fits, no clip
                w.desktop.RequestRedraw()
            })
        }()
    }
}
```

Guards:
- The fetch **never** blocks the quit; the box is interactive the instant it
  opens. If the daemon is unreachable / slow / errors, the fallback text stays.
- `quitSizingBody` keeps `newMessageLayer`'s captured reflow message at the
  enriched height, so a terminal resize *after* enrichment also stays tall — no
  regression of the no-clip guarantee on resize.
- Cosmetic trade-off (acknowledged): while the fallback is shown in the
  enriched-sized box, a few blank rows sit between the text and the button row.
  This is brief (enrichment normally lands sub-second) and, in the
  fetch-failed-permanently case, a minor empty gap — strictly better than hiding
  the re-attach command. No content is ever clipped.
- A small `w.quitDialogLayer *tv.Layer` field tracks the live quit dialog (like
  `disconnectLayer`/`daemonHandoffLayer`); set on open, cleared on dismiss. The
  enrichment callback no-ops if the user already clicked a button (layer gone or
  replaced). Button labels/focus/positions are **not** rebuilt on enrichment —
  only the body text is rewritten (counts never change which buttons exist, and
  the box is pre-sized for the enriched body), so focus and layout stay stable.
  Touched only on the UI thread (open + `Post`ed callback).
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

`{host}`/`{addr}`: **add `Handlers.ReconnectAddress func() string`** and use it
for `{addr}` in `gogent --connect {addr}`; keep `w.reconnectHost` for the
human-readable `{host}` only. This is settled, not optional: `reconnectHost` is a
*display label* (`hostLabel` → bare `u.Host`), which is wrong as a `--connect`
argument for SSH attachments (scheme + user dropped) and unverified for bare TCP.
`ReconnectAddress` returns the verbatim attach `addr` (e.g. `ssh://user@host`,
`host:port`), which is exactly what `--connect` accepts. It is cheap and
synchronous (a closure over `addr`), mirroring `ConnectionLabel`. When it is nil
or returns `""`, **omit the re-attach line** (the fallback bodies in states 2/4
already read sensibly without it). This is the single new `Handlers` field; its
cmd-side wiring is in §2.

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
behaviour identical (same messages, same #478 single-dialog guarantee). The
`newMessageLayer` return-value widening is absorbed by its two callers (discard),
behaviourally inert; the new `ReconnectAddress` cmd wiring is an otherwise-unread
closure, so it cannot regress any existing path. gofmt /
go vet / build / `golangci-lint` (0 NEW) / `go test ./...` (no `-race`, per the
Pi5 dev gate) must stay green; pre-existing `TestUserSessionSendMessage` 404 is
the only accepted failure.

### (4) Holistic design (both repos)
The change sits where the seam dictates: the behaviour is in `ui/tui`
(`quit_dialog.go` + a layout helper and a one-line return-value widening in
`message_dialog.go` + the `confirmQuit` rewrite in `tui.go`), reusing the
existing `newMessageLayer`/`showProgress`/`dismissDaemonHandoffProgress`/handoff
primitives. The **only** edit outside `ui/tui` is wiring the new
`ReconnectAddress` closure in `cmd/attach.go`/`cmd/handoff.go` where `addr` is
already in scope — the backend, not the UI, is the right owner of the connect
string, so the seam is respected (ui/tui still consumes only the
`Handlers`/`DaemonStatusReport` UI-facing types and stays free of
`internal/daemon`/`internal/server`, exactly as `daemon_menu.go` does). turbotui
needs nothing — its `Dialog`/`Button`/`ModalLayer`/`WrapText`/`TextView.AllText`
primitives already cover a 3-button content-sized, enrichable dialog — so there
is no downstream effect on turbotui and no `go.mod` bump.

### Regression risks (called out)
- **Orchestration:** #500g/#501g also edit `ui/tui` (`tui.go`/`theme.go`).
  Keeping the bulk in a new `quit_dialog.go` and touching only `confirmQuit` in
  `tui.go` minimises the rebase surface; tasks are serialized, rebase at gate.
- **Narrow terminal:** mitigated by the documented middle-button drop; tested.
- **Enrichment race:** mitigated by the `w.quitDialogLayer == layer` guard +
  UI-thread-only mutation.
- **`{addr}` correctness:** resolved — `ReconnectAddress` returns the verbatim
  attach `addr`, so the re-attach command is copy-pasteable for SSH and TCP; the
  line is omitted when no address is available rather than shown wrong.
- **`&` in labels:** resolved — labels escape the literal ampersand as `&&` so
  `ParseMnemonic` renders one `&` instead of consuming it as a mnemonic marker.
- **`newMessageLayer` signature change:** two in-repo callers updated to discard
  the new body return; both behaviourally unchanged (covered by existing
  showConfirm/showProgress and #478 tests).
- **Enriched body clipping the re-attach line:** resolved — the dialog is sized
  for the enriched shape at open (`quitSizingBody`), so the taller enriched body
  fits without scrolling and the `--connect {addr}` line stays visible (§5);
  resize keeps the enriched height because the reflow message is the sizing
  string. Cosmetic cost: a brief blank gap below the fallback text.

---

## Tests (new `ui/tui/quit_dialog_test.go`; mirror
`daemon_lifecycle_issue358_test.go` / `daemon_handoff_dialog_issue478_test.go`)

- **Pure `buildQuitModel` table test** over all six states: assert Title, Body
  (enriched and fallback), ordered Button labels, and `DefaultIdx`. Covers
  pluralisation ("1 live session" vs "3 live sessions"); the remote enriched case
  passes a non-empty `addr` and asserts the body contains
  `gogent --connect {addr}` (and that a "" `addr` omits the re-attach line) and
  that `{host}` uses the display `host`, not `addr` — the two-param signature
  makes both directly assertable.
- **Button-omission:** `StopDaemon==nil` (local) / `StartDaemon==nil` (embedded)
  drop the middle button; AttachedRemote never has Stop.
- **Nil `DaemonMode`:** `confirmQuit` builds the unchanged `confirm-dialog`
  Yes/No box, default No (existing behaviour preserved).
- **Non-blocking fetch:** with a blocking/slow/erroring `DaemonStatusInfo`, the
  dialog is up immediately with fallback text; on a released successful fetch the
  body is rewritten to enriched (assert via the body TextView's `AllText()`); on
  error the fallback text remains.
- **No-clip sizing:** in the remote enriched state, assert the dialog/body height
  is sized for the enriched body (computed from `quitSizingBody`) so the final
  `gogent --connect {addr}` line is within the body viewport
  (`messageBodyRows(enriched) <= bodyH`) — i.e. not scrolled off after enrich.
- **Stop/Start-daemon-&-quit:** wire a channel-blocked `StopDaemon`/`StartDaemon`;
  assert the `daemon-progress` modal shows during the handoff, then on **success**
  `w.quit` is invoked (spy func), and on **failure** a result `confirm-dialog`
  shows and `w.quit` is **not** called (STAY ALIVE).
- Keep existing quit/showConfirm tests passing. Decoupling from
  `internal/daemon`/`internal/server` is preserved by construction (the new code
  consumes only `Handlers`/`DaemonStatusReport`) and verified at the dev gate via
  `go list -deps ./ui/tui` / review — there is **no** existing import-guard test
  to extend (grep over `ui/tui/*_test.go` finds none). Adding one is optional and
  out of scope for this issue.

---

## Open questions

1. **Mnemonics on the buttons.** Since the labels already contain a literal `&`
   (escaped to `&&`), adding an Alt-mnemonic marker would mean a *second*,
   un-escaped `&` (e.g. `S&top daemon && quit` → mnemonic on "t", literal " & ").
   Workable but fiddly. Default: **no mnemonics** — buttons are reachable by
   Tab/click/Enter/Escape, matching `confirmButtonRow`/`disconnectButtonRow`
   which also ship without per-button mnemonics. Trivial to add later if desired.
2. **Embedded + no `StartDaemon`:** the body still advises "start the daemon
   first" while no Start button is offered (it was omitted because
   `StartDaemon==nil`). Keep the copy (still true advice — the daemon can be
   started by relaunching) or trim that sentence when `StartDaemon==nil`?
   Default: keep the issue's exact copy.

*(The earlier `{addr}`-source and `&`-rendering questions are now resolved in the
body of the design — `ReconnectAddress` and `&&` escaping respectively.)*
