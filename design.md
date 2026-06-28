# Design — Comfortable max-width cap (gogent issue #552)

## Issue

> Windows and dialogs balloon to unusable widths on very large terminals (e.g. 766 columns)

On very wide terminals new session windows and three dialogs span nearly the
whole screen (~660-col panes on 766 cols). There is no comfortable upper width
cap: new windows open at 90% of the available area, and the three uncapped
dialogs fall back to turbotui's 80%-of-screen default (~612 cols on 766).

The fix: introduce a single comfortable maximum (`comfortableMaxWidth = 120`)
that caps **initial / automatic** sizing of session windows and the three
uncapped dialogs. Every user-driven path (manual resize, drag, maximize,
maximize-all, tiling, terminal resize, sidebar clamps) stays exactly as it is —
those must still reach full terminal width.

This is a **fix**, gogent-only. No turbotui change, no new deps, no `go.mod`
bump. It reuses the existing `tv.DialogSpec.MaxW` field that ~22/25 gogent
dialogs already use.

## Root cause (verified against current source)

1. **New windows** — `openWindowAny` (`ui/tui/tui.go:1779`):
   `width := avail * 90 / 100` where `avail = app.Width() - sidebarWidth()`.
   On 766 cols with a 32-col sidebar → `avail=734`, `width=660`. No upper cap.
   Feeds `clampWindowRect`, which is min/screen-bounds only. Covers NewSession,
   ForkSession, AdoptSession, OpenAnalysisSession (all route through
   `openWindow`/`openWindowReadOnly` → `openWindowAny`).
2. **Restored windows** — `applyLayout` (`ui/tui/tui.go:2459`): each restored
   `e.W` is only passed through `clampWindowRect(..., area.W, area.H, ...)`. With
   an unpinned sidebar the work area is the full width, so a wide saved layout
   restores wide. A wide-monitor restart re-introducing 660 is the reported
   annoyance.
3. **Dialogs** — `ResolveDialogRect` (turbotui) defaults to ~80% width when a
   spec has `PreferredW==0` and no `MaxW`. The three offenders:
   - **Resources browser** — `browserDialogSpec` (`ui/tui/dialog_sizing.go:31`):
     `PreferredW: w.app.Width() * 85 / 100`, no `MaxW` → ~651 on 766.
   - **Command palette** — `showCommandPalette` (`ui/tui/command_palette.go:566`):
     `DialogSpec{MinW:40, MinH:10, MaxH:...}`, no `PreferredW`/`MaxW` → ~612.
   - **Help / Keybindings overlay** — `showHelpOverlay`
     (`ui/tui/command_palette.go:665`): `DialogSpec{MinW:44, MinH:12, MaxH:...}`,
     no `PreferredW`/`MaxW` → ~612.

Not affected (intentional, left untouched): `maximizedWindowRect`
(`session_window.go:3238`, fills area — that's the point of maximize),
`clampWindowSize`/`constrainWindowToBounds` (resize-drag, boundary-only),
`clampWindowRect` (move/drag/restore boundary, boundary-only), tiling
(`tiling.go`), sidebar clamps (already floor `minWorkAreaWidth`), and the
already-capped dialogs (Sessions 160, Commands 140, Watchers 130, Statistics/
Permission/Question 110, Message 80, etc.).

## The value: `comfortableMaxWidth = 120`

Matches the existing 120-col precedent for code-bearing panes — review viewer
(`review_dialog.go:73` `MaxW: 120`) and agent monologue
(`agent_monolog.go:33` `MaxW: 120`). 120 holds a 100-col code line plus chrome
slack, sits under the readability ceiling (~130), and is a no-op under typical
tmux pane splits (≤120) so it only bites the genuine wide-terminal sprawl case.
It **only fills the missing-cap gap** — it never overrides a dialog's explicit
(larger) `MaxW`.

## Changes (gogent only)

### 1. New file `ui/tui/sizing.go`

```go
package ui

// comfortableMaxWidth caps the default/open width of session windows and the
// three otherwise-uncapped dialogs (Resources browser, Command palette, Help
// overlay) on very wide terminals (issue #552). It governs INITIAL/auto sizing
// only — manual resize/drag/maximize/maximize-all/tiling still fill the area.
// Matches the review-viewer / agent-monologue 120-col precedent for
// code-bearing panes.
const comfortableMaxWidth = 120
```

(Own file keeps the constant discoverable and avoids churn in tui.go's
already-busy header; package is `ui` to match the directory.)

### 2. `openWindowAny` (`ui/tui/tui.go`)

After computing `width` (and after the existing `width < 50` floor so the floor
can't be re-inflated), before building the rect:

```go
if width > comfortableMaxWidth {
    width = comfortableMaxWidth
}
```

Initial open only. Subsequent user resize/drag/maximize are unaffected (they go
through other functions). Cascade offset, height, and the narrow-terminal
fallbacks are unchanged.

### 3. `applyLayout` (`ui/tui/tui.go`) — **ceiling only (option b)**

Clamp each restored `e.W` down to the comfortable max *before* `clampWindowRect`:

```go
w := e.W
if w > comfortableMaxWidth {
    w = comfortableMaxWidth
}
bounds := clampWindowRect(tv.Rect{X: e.X, Y: e.Y, W: w, H: e.H},
    area.W, area.H, sw.window.MinWidth, sw.window.MinHeight)
```

Ceiling semantics: a saved width ≤120 restores verbatim; a width >120 clamps to
120. Re-saving may forget the previously-wide value — accepted per the issue.
`clampWindowRect` itself stays boundary-only.

### 4. Three dialog specs — add `MaxW: comfortableMaxWidth`

- `browserDialogSpec` (`ui/tui/dialog_sizing.go:31`):
  `tv.DialogSpec{MinW: 60, MinH: 14, MaxW: comfortableMaxWidth, PreferredW: w.app.Width() * 85 / 100}`.
  Keeping the percentage `PreferredW` is harmless — `MaxW` wins above 120, and on
  narrower terminals the percentage keeps it responsive. (Leaving PreferredW in
  preserves small/medium-terminal behavior exactly.)
- `showCommandPalette` (`ui/tui/command_palette.go:566`): add `MaxW: comfortableMaxWidth`.
- `showHelpOverlay` (`ui/tui/command_palette.go:665`): add `MaxW: comfortableMaxWidth`.

No turbotui change: `ResolveDialogRect` already honors `MaxW`.

### Optional bonus — **SKIP**

A `Layout.MaxWindowWidth` override field is out of scope; the constant suffices
and keeps the fix tight. Not implementing.

## User-facing behavior

- On a terminal ≥ ~400 cols, a newly opened session window is ≤120 wide until
  the user resizes/maximizes.
- Resources browser, Command palette, and Help overlay open ≤120 on a 766-col
  terminal (instead of ~612–651).
- Restoring a session persisted wider than 120 clamps down to 120 on restore.
- Manual resize, drag, maximize, maximize-all, and tiling still reach full
  terminal width — the cap is invisible to user-driven sizing.
- On terminals ≤ ~133 cols (after sidebar/chrome) nothing changes — the 90% /
  80% math already lands under 120, so the cap is a no-op.

## Criterion 1 — Goal match

Exactly the issue's ask: a comfortable upper width cap on initial/auto sizing of
the ballooning windows + dialogs. It's a sizing fix, not a feature or refactor.
No scope creep (Layout override skipped). Already-capped dialogs and the sidebar
are untouched. The cap fills only the missing-cap gap.

## Criterion 2 — Usability

120 is a comfortable reading/code width matching existing precedent. The user
remains fully in control: any manual resize/drag/maximize/maximize-all/tile
fills the screen — the cap only governs the automatic first paint and
layout-restore. Nothing is silently hidden; windows/dialogs are simply centered
at a sane width. No interaction is removed.

## Criterion 3 — No regressions

- Boundary-only functions stay boundary-only: `maximizedWindowRect`,
  `clampWindowSize`, `clampWindowRect`, tiling `tileArea`, sidebar clamps — none
  touched.
- The cap is applied in exactly two window paths (`openWindowAny`,
  `applyLayout`) and three dialog specs; every other path is exempt by
  construction.
- Explicit larger dialog caps (Sessions 160, Commands 140, Watchers 130)
  unaffected — they set their own `MaxW`; we add `MaxW` only where it was absent.
- Existing tests expected to stay green: `dialog_sizing_test.go`,
  `dialog_issue317_test.go`, model-selector/session dialog width tests,
  `maximize_test.go`, `sidebar_resize_test.go`. The 90%-window math is unchanged
  below 120, so existing small/medium-terminal assertions hold.
- New tests will assert: window cap at open (wide terminal), window ceiling on
  restore via `applyLayout`, and ≤120 for the 3 dialogs on a 766-col terminal;
  plus a no-op assertion on a narrow terminal.
- Gates: `gofmt`/`go build`/`go vet` clean; `golangci-lint` 0 NEW; `go test ./...`
  green (pre-existing `TestUserSessionSendMessage` 404 is the only acceptable
  failure).

## Criterion 4 — Holistic (gogent ↔ turbotui seam)

The seam is respected: turbotui already owns dialog-sizing policy and exposes
`DialogSpec.MaxW`; ~22/25 gogent dialogs already use it. The three offenders
simply forgot to set it — a gogent caller bug, fixed gogent-side. turbotui's
only consumer is gogent, and its own `dialog_spec_test.go` asserts the 80%-fill
default, so changing the turbotui default would break turbotui for no gain. The
window cap is purely gogent layout policy (`openWindowAny`/`applyLayout`),
correctly placed in gogent. No new deps, no `go.mod` bump, no turbotui edit.

## Files touched (gogent only)

- `ui/tui/sizing.go` — **new**, holds `comfortableMaxWidth`.
- `ui/tui/tui.go` — `openWindowAny` (cap), `applyLayout` (ceiling).
- `ui/tui/dialog_sizing.go` — `browserDialogSpec` (+MaxW).
- `ui/tui/command_palette.go` — `showCommandPalette`, `showHelpOverlay` (+MaxW).
- New test file(s) for the window cap + dialog caps.
- **turbotui: no change.**

## Rebase note

Conflicts with the in-flight ui/tui chain (#548, #549 gogent half, #551).
Serialize in the ui/tui lane; at the gate rebase onto current `origin/main` and
resolve incidental `tui.go` overlap. PR body: "Closes #552".

## Open questions

1. **Restore semantics** — design uses the issue-specified ceiling (option b):
   clamp restored width down to 120, accept that re-saving may drop a wider
   value. Confirmed by the task brief; no alternative pursued.
2. **`browserDialogSpec` PreferredW** — kept the existing `Width()*85/100`
   alongside the new `MaxW` (MaxW wins above 120, percentage preserves
   narrow-terminal behavior). Dropping it would also satisfy the cap but would
   change medium-terminal sizing slightly; keeping it is the lower-risk choice.
   Flagging in case the maintainer prefers a clean content-driven `PreferredW`.
