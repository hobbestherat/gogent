# Design — Surface connection/daemon status on the menu bar (gogent half of issue #500)

## Summary

Add an always-visible, right-aligned connection/daemon **status indicator** at the top-right of
the gogent menu line, reflecting this TUI instance's `DaemonMode` (+ remote target + transient
disconnect phase), and render the existing **`&Daemon`** top-level menu **right-aligned** (flush
right, immediately left of the indicator). The indicator updates live on Start/Stop handoff,
remote disconnect/reconnect, and terminal resize. No backend/protocol change; `ui/tui` stays
decoupled from `internal/daemon`, `internal/server`, `internal/sshtunnel`.

This is the **consumer** half. The turbotui capability (PR #42, commit `0ff08b27…`) is already
merged and provides everything we need — we only consume it.

## Dependency bump (first implementation step, not part of this design doc)

```
go get github.com/hobbestherat/turbotui@0ff08b27a8c24503eb55fc406dece62ab732d4b1
go mod tidy
```

Current `go.mod` pins `…20260626190220-877fd6224b7d` (PR #41). The bump moves to PR #42.
(PR #43, the paste-chip work, is the next commit on turbotui main but we pin exactly #42 to
avoid pulling unrelated changes.)

## turbotui API we consume (verified in `$HOME/work/turbotui/turbotv/menu.go`)

PR #42 added, on `tv.MenuBar`:
- Field `StatusText string` — right-anchored status label, measured by display width, rendered
  literally (no `&`-mnemonic parsing), truncated with `…` then hidden when space is tight.
- Field `StatusFG`, `StatusBG tui.Color` — colour the whole status string; a **zero** `tui.Color`
  falls back to the bar's `FG`/`BG`.
- Method `SetStatus(text string)` — pure state mutation (no repaint); caller pairs it with a redraw.
- Method `SetStatusColors(fg, bg tui.Color)` — same contract; zero restores defaults.

And on `tv.MenuItem` (top-level entries only):
- Field `RightAligned bool` and fluent method `AlignRight() *MenuItem` — packs that top-level menu
  from the right edge inward, to the left of the status slot. Ignored on child items. Zero value
  keeps the historical left-pack, byte-for-byte.

Layout precedence (from `layoutTopRects`): `[left menus] … gutter … [right menus] [status slot]`.
Left menus own their cells unconditionally; the status slot yields first (shrinks then hides),
right menus yield next (never start left of the left menus). So a narrow terminal degrades
gracefully with no work on our side.

Key consequence for us: the menu bar's bounds are re-applied and `layoutTopRects` recomputes on
**every desktop draw** (`desktop.go` re-bounds `d.menuBar` to `app.Width()` each frame). The bar
is therefore *re-laid-out* on resize, not rebuilt — `StatusText`/`RightAligned` are persistent
fields on the bar object, so once set they survive resize automatically. (The task brief says
"bar is rebuilt on resize"; the precise mechanism is "re-laid-out each draw" — same net effect:
the slot survives resize with no extra code.)

## gogent changes

All gogent changes live in `ui/tui` plus a single `cmd/handoff.go` seam. No change to
`cmd/embedded_handlers.go` is actually required (see "Wiring the remote target" below).

### 1. Surface the remote target into `ui/tui` without breaking the import boundary

`ui/tui` must not import `internal/sshtunnel`/`daemon`/`server`, and the indicator must be
derivable **synchronously and cheaply** on the UI thread (it is set on every rebuild/resize).
`DaemonStatusInfo()` is unsuitable — it does a blocking HTTP round-trip. So:

**Add a new `Handlers` field** (`ui/tui/tui.go`, in the `Handlers` struct, next to `DaemonMode`):

```go
// ConnectionLabel returns the terse remote target shown in the menu-bar status
// indicator when DaemonMode reports attached-remote, e.g. "ssh:user@host" (or a
// "host:port" for a plain --connect). It is cheap and synchronous (no round-trip),
// read on the UI thread. Empty for embedded/attached-local (the indicator derives
// those labels from DaemonMode alone). May be nil.
ConnectionLabel func() string
```

Chosen over extending `DaemonStatusReport.RemoteTarget` because the report is built by a blocking
round-trip; an always-visible indicator refreshed on resize needs a lock-free getter, which a
function handler gives us. `DaemonStatusReport` stays unchanged.

**Wire it in `cmd/handoff.go`:**
- `installMenuHandlers(h *tuipkg.Handlers)` already sets `h.DaemonMode = dc.Mode` and the four
  callbacks. Add `h.ConnectionLabel = dc.Label`. This single seam covers **all** daemon-wired
  paths (embedded-with-wiring, attached-local, attached-remote), since `dc` is mode-aware.
- Add method `(dc *daemonController) Label() string`: lock, read `dc.mode` + `dc.connect`, unlock;
  return `""` for embedded/attached-local; for attached-remote parse `dc.connect`
  (`url.Parse`) — `ssh://user@host:port` → `"ssh:" + user@host` (drop the port for terseness;
  reuse the existing `url`/`hostLabel` style already in the file); a bare `host:port` tcp connect
  → that `host:port`. This keeps all SSH/tcp knowledge in `cmd/`, none in `ui/tui`.

The pure-embedded path (`cmd/embedded_handlers.go`, no daemon wiring) leaves both `DaemonMode`
and `ConnectionLabel` nil → indicator shows `"● embedded"`, no Daemon menu. No edit needed there.

### 2. Status-string + colour derivation (new, pure, testable) — `ui/tui/daemon_menu.go`

Add a connection phase enum and a **pure** indicator function (unit-testable without a UI):

```go
type connPhase int
const (
    connHealthy connPhase = iota
    connDisconnected            // remote stream just dropped (attempt <= 1)
    connReconnecting            // backoff actively retrying (attempt > 1)
)

// daemonIndicatorText derives the terse top-right status string. Pure.
func daemonIndicatorText(mode DaemonMode, remoteLabel string, phase connPhase) string
```

Mapping (matches the issue's "Indicator content"):

| Condition                                   | String                |
|---------------------------------------------|-----------------------|
| `DaemonModeEmbedded`                         | `● embedded`          |
| `DaemonModeAttachedLocal`                    | `● daemon`            |
| `DaemonModeAttachedRemote`, healthy          | `● ` + `remoteLabel` (e.g. `● ssh:user@host`); falls back to `● ssh` / `● remote` if label empty |
| remote, `connDisconnected`                   | `○ disconnected`      |
| remote, `connReconnecting`                   | `○ reconnecting…`     |
| remote, restored                             | back to `● ssh:user@host` (= healthy) |

The disconnected→reconnecting split is derived from the existing
`Reconnector.OnConnectionLost(attempt)` signal with **no new detection**: first loss
(`attempt <= 1`) → `connDisconnected`; subsequent backoff attempts (`attempt > 1`) →
`connReconnecting`. The disconnect phase only ever applies in remote mode (the only mode with a
reconnect loop). It is purely presentational — the blocking modal still owns "attempt N" and the
Retry/Quit affordances; the indicator never duplicates them.

Colour helper (also in `daemon_menu.go`), reusing the **existing** theme-tracked package colour
vars in `theme.go` (`colorAgent` = green, `colorTool` = amber/yellow, `colorError` = red,
`colorNote` = dim grey) so **`theme.go` is not edited at all** (zero overlap with concurrent
#501's PasteChip roles, and it already respects live theme switches + NO_COLOR degrade because
`ApplyTheme` reassigns those vars):

```go
func daemonIndicatorColors(mode DaemonMode, phase connPhase) (fg, bg tui.Color)
```
- embedded → `colorNote` (dim)
- attached-local / attached-remote healthy → `colorAgent` (green)
- `connDisconnected` → `colorError` (red)
- `connReconnecting` → `colorTool` (amber)
- `bg` always returns the zero `tui.Color` → turbotui falls back to the bar's `MenuBarBG`, so the
  slot reads as part of the bar on every theme.

### 3. Workbench glue — `ui/tui/tui.go`

- Add fields to `Workbench`: `menuBar *tv.MenuBar` (the live bar, so we can update the slot
  without a full rebuild) and `connPhase connPhase` (touched only on the UI thread — same
  discipline as `disconnectLayer`). Default zero value = `connHealthy`, correct for startup.
- `connectionIndicator()` method: read `mode := DaemonModeEmbedded` (or `w.handlers.DaemonMode()`
  if non-nil), `label := ""` (or `w.handlers.ConnectionLabel()` if non-nil), `phase := w.connPhase`
  (forced to `connHealthy` when not remote), then call the pure `daemonIndicatorText` /
  `daemonIndicatorColors`. Returns `(text string, fg, bg tui.Color)`.
- `refreshConnectionStatus()` method: the light update path — if `w.menuBar != nil`, compute the
  indicator and `w.menuBar.SetStatus(text)` + `w.menuBar.SetStatusColors(fg, bg)`, then
  `w.desktop.RequestRedraw()`. **No menu rebuild.** UI-thread only.

### 4. `rebuildMenu()` — `ui/tui/tui.go` (~L961, ~L976)

- Keep the existing `if w.handlers.DaemonMode != nil { subMenus = append(subMenus, tv.NewSubMenu("&Daemon", …)) }`
  guard. **Mark that submenu `.AlignRight()`** so it right-packs. The `&Help` menu currently
  appended after Daemon must stay **left**-aligned, so the visual order becomes:
  `File Edit Session View Config Help … [right:] Daemon  ● status`.
  (Help is a left menu; only Daemon is `RightAligned`. Left-menu order/mnemonics are untouched.)
- After `bar := tv.NewMenuBar(…)` and `applyMenuBarShadow(bar)`: store `w.menuBar = bar`, then set
  the status slot from `connectionIndicator()` (`bar.SetStatus` / `bar.SetStatusColors`) **before**
  `w.desktop.SetMenuBar(bar)` so the first paint already shows it. Because a fresh bar is built
  each rebuild, the slot must be (re)seeded here every time.

When `DaemonMode == nil`: no Daemon menu, and the indicator still shows `"● embedded"` (the
recommended behaviour in the brief) — a pure-embedded build with no daemon wiring still gets a
dim `● embedded` marker and no menu.

### 5. Update points

| Trigger | Path | Action |
|---|---|---|
| Start handoff done | `startDaemonFromMenu` → `Post` → already calls `rebuildMenu()` | rebuild reseeds slot (mode now attached-local) — confirm, no new call needed |
| Stop handoff done | `stopDaemonFromMenu` → `Post` → already calls `rebuildMenu()` | rebuild reseeds slot (mode now embedded) — confirm |
| Remote stream drop | `OnConnectionLost(attempt)` (`disconnect_modal.go`) | inside its existing `Post`, set `w.connPhase = connDisconnected`/`connReconnecting` from `attempt`, then `w.refreshConnectionStatus()` |
| Remote restored | `OnConnectionRestored()` (`disconnect_modal.go`) | inside its existing `Post`, set `w.connPhase = connHealthy`, then `w.refreshConnectionStatus()` |
| Terminal resize | desktop re-lays-out the persistent bar each draw | `StatusText`/`RightAligned` persist on `w.menuBar` → slot survives automatically; confirm, no new code |

`refreshConnectionStatus()` is preferred over `rebuildMenu()` for the disconnect hooks because it
avoids rebuilding the whole (dynamic, session-list-bearing) menu on every backoff tick.

## User-facing behaviour

- Fresh embedded run: top-right shows dim `● embedded`; no Daemon menu (when daemon wiring absent).
- After `Daemon → Start`: indicator flips to green `● daemon`; the right-aligned Daemon menu now
  offers `S&top daemon`.
- After `Daemon → Stop`: back to `● embedded`; Daemon menu offers `&Start daemon`.
- `gogent --connect ssh://user@host`: green `● ssh:user@host`; on drop → red `○ disconnected`
  then amber `○ reconnecting…` as backoff climbs; back to green `● ssh:user@host` on reconnect.
  The blocking "Connection lost" modal still owns Retry/Quit and the attempt count; the indicator
  stays visible **above** the modal (the menu bar draws on top of all layers — unchanged).
- Narrow terminal: turbotui truncates the status (`…`) then hides it, and shrinks/clamps the
  right-aligned Daemon menu, with left menus always intact. We pass a terse string
  (`ssh:user@host`, port dropped) to keep it short.

## Tests — `ui/tui` (no `internal/daemon`/`internal/server` imports)

Extend `daemon_lifecycle_issue358_test.go` and `daemon_handoff_dialog_issue478_test.go`:
- **Pure indicator string** per mode + phase via `daemonIndicatorText`:
  embedded → `● embedded`; attached-local → `● daemon`; attached-remote healthy
  (`remoteLabel="ssh:user@host"`) → `● ssh:user@host`; remote disconnected → `○ disconnected`;
  remote reconnecting → `○ reconnecting…`; remote restored → `● ssh:user@host`.
- **Colour helper** returns the dim/green/red/amber var per phase (assert equality to
  `colorNote`/`colorAgent`/`colorError`/`colorTool`).
- **Right-aligned Daemon menu**: build a `Workbench` with `DaemonMode != nil`, call
  `rebuildMenu()`, find the `&Daemon` top-level item in `w.menuBar.Menus` and assert
  `RightAligned == true`; assert the left menus (`&File`…`&Help`) have `RightAligned == false`
  and keep their order/mnemonics.
- **Slot set on rebuild**: after `rebuildMenu()`, assert `w.menuBar.StatusText` equals the
  expected per-mode string (via `SetHandlers` with a stub `DaemonMode`/`ConnectionLabel`).
- **Refresh on disconnect phases**: drive `OnConnectionLost(1)` / `OnConnectionLost(3)` /
  `OnConnectionRestored()` (each marshals through `Post`; tests already pump the desktop) and
  assert `w.menuBar.StatusText` transitions `○ disconnected` → `○ reconnecting…` → healthy.

Existing tests (`TestIssue358DaemonMenuItemsAreModeAware`, handoff dialog tests) stay green —
`daemonItems()` is unchanged; only the top-level menu gains `AlignRight()` and the bar gains a slot.

## Design criteria

**(1) Goal match.** Exactly the issue's ask: a right-aligned, always-visible status indicator
reflecting `DaemonMode` live (Start/Stop, disconnect/reconnect, resize), with the `Daemon` menu
moved right-aligned, left of the indicator; left menus unchanged. No backend/protocol work, no
new lifecycle commands/endpoints, no new keybindings, no touch to `formatStatusLine` or sidebar
folds. Out-of-scope items (embedded-local-daemon hint, click-to-open-status) are excluded.

**(2) Usability.** Indicator is purely presentational — the blocking modal keeps Retry/Quit and
the attempt count; the indicator never duplicates them. Colours are theme-tracked (green healthy
/ amber reconnecting / red disconnected / dim embedded) and degrade under NO_COLOR via the shared
`color*` vars. It stays visible above the disconnect modal (menu bar is always-on-top — unchanged).
Terse strings keep narrow terminals clean (turbotui truncates/hides gracefully).

**(3) No regressions.** No backend/protocol change. `ui/tui` gains only a `func() string` handler
and reads existing theme vars — no `internal/daemon`/`server`/`sshtunnel` import. `daemonItems()`
and the four daemon callbacks are untouched. Left menus keep order + mnemonics (only Daemon gets
`AlignRight()`). The new `Workbench` fields are UI-thread-only, same discipline as the existing
disconnect modal state. `rebuildMenu` already runs on Start/Stop/resize; the extra slot-seed is a
pure state set. With no `RightAligned` item and empty status, turbotui's layout is byte-for-byte
the historical left-pack — so any build without daemon wiring renders identically except for the
dim `● embedded` marker (acceptable per brief; can be suppressed if undesired — see Open questions).
gofmt/vet/build/lint clean; `go test ./...` green (pre-existing env `TestUserSessionSendMessage`
404 is the only accepted failure; no `-race` on Pi5).

**(4) Holistic / seam across both repos.** The reusable mechanism (status slot + right-align)
lives in **turbotui** (PR #42, already merged) where it belongs — gogent only consumes it via the
documented `SetStatus`/`SetStatusColors`/`AlignRight` API; no turbotui edit in this half. The
gogent↔internal seam is respected: SSH/tcp parsing stays in `cmd/handoff.go` (`dc.Label`),
surfaced to `ui/tui` through a cheap `ConnectionLabel func() string`, mirroring how `DaemonMode`
is already surfaced. Change is confined to `ui/tui` (+ the one `cmd/handoff.go` seam). Theme edits:
**none** (reuse existing semantic colour vars) — minimising overlap with concurrent #501.

## Regression risks

- **Bar rebuilt each `rebuildMenu`**: the new `*tv.MenuBar` discards the previous one's
  `StatusText`. Mitigated by reseeding the slot inside `rebuildMenu` from `connectionIndicator()`
  every time, and `refreshConnectionStatus` updates the *current* `w.menuBar` only.
- **Help-menu placement**: Help is appended after Daemon in `subMenus`. Since only Daemon is
  `RightAligned`, Help stays left; verify visually that Help remains the last *left* menu and
  Daemon sits right. (turbotui preserves `Menus` index order; right-pack iterates in reverse so
  declared order reads left→right within the right group — only Daemon is there, so trivial.)
- **Disconnect phase only meaningful for remote**: `connectionIndicator` forces `connHealthy`
  when mode != attached-remote, so a stale `connPhase` (e.g. after a Stop handoff that left
  remote) can't leak a `○` marker into embedded/local. Reset `connPhase` to `connHealthy` on
  restore and rely on the mode guard.
- **Threading**: all setters run on the UI thread (`Post` in the Reconnector hooks; `rebuildMenu`
  on resize/handoff). `SetStatus`/`SetStatusColors` are pure mutations paired with a redraw, per
  turbotui's documented contract — no background-goroutine mutation of the bar.

## Open questions

1. **Embedded marker when no daemon wiring**: the brief recommends showing `● embedded` even when
   `DaemonMode == nil`. That means *every* gogent build (including pure-embedded with no daemon
   feature) gains a top-right `● embedded`. Confirm this is wanted globally; the alternative is to
   show the indicator only when `DaemonMode != nil` (i.e. only daemon-capable builds). Going with
   the brief's recommendation (always show `● embedded`) unless told otherwise.
2. **tcp `--connect` (non-ssh) label**: brief specifies the ssh case. For a plain `host:port`
   `--connect`, I'll show `● host:port`. Acceptable, or prefer a `tcp:` prefix for symmetry with
   `ssh:`? Defaulting to bare `host:port`.
3. **disconnected vs reconnecting granularity** — *resolved*: `remote_handlers.go reconnect()`
   loops `for attempt := 1; ; attempt++` and calls `notifyLost(attempt)` at the top of each
   iteration, so `attempt==1` is the fresh drop (→ `○ disconnected`) and `attempt>1` is an active
   backoff retry after a failed re-open (→ `○ reconnecting…`). The `attempt<=1` / `>1` split maps
   cleanly with no new detection and no ambiguity. No open issue remains here.
