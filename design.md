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

Clamp each restored `e.W` down to the comfortable max *before* `clampWindowRect`.
Note `applyLayout` is `func (w *Workbench) applyLayout`, so the local **must not**
be named `w` (it would shadow the receiver — a footgun and a likely shadow-lint
hit under the repo's `golangci-lint` gate). Use `restoredW`:

```go
restoredW := e.W
if restoredW > comfortableMaxWidth {
    restoredW = comfortableMaxWidth
}
bounds := clampWindowRect(tv.Rect{X: e.X, Y: e.Y, W: restoredW, H: e.H},
    area.W, area.H, sw.window.MinWidth, sw.window.MinHeight)
```

Ceiling semantics: a saved width ≤120 restores verbatim; a width >120 clamps to
120. Re-saving may forget the previously-wide value — accepted per the issue.
`clampWindowRect` itself stays boundary-only.

### 4. Three dialog specs — add `MaxW: comfortableMaxWidth`

- `browserDialogSpec` (`ui/tui/dialog_sizing.go:31`):
  `tv.DialogSpec{MinW: 60, MinH: 14, MaxW: comfortableMaxWidth, PreferredW: w.app.Width() * 85 / 100}`.
  The percentage `PreferredW` is already functionally **inert**: `resolveDimension`
  (turbotui `dialog_spec.go:86-88`) clamps the 85% request down to the 80%
  percentDefault, so it never reaches the output today. With `MaxW: 120` added it
  stays inert (MaxW is the binding constraint above 120). It is kept solely
  because `TestBrowserPreferredWidthClamped` reads `spec.PreferredW` directly — so
  removing it would be a second, unrelated test churn. Keeping it is the
  lower-churn choice; its doc-comment is updated to say the 85% is dead and 120 is
  the real cap (no "keeps it responsive" claim).
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

Two minor notes (neither a blocker):
- **Resources browser is the one prose reader among the three dialogs.** Unlike
  the list-driven palette/help, it renders arbitrarily-long `SKILL.md` / input-
  schema text, so 120 trades a little reading width versus the old ~80%. It is
  still a comfortable prose column and the issue explicitly names Resources as a
  ballooning offender, so 120 is the right default; a maintainer who wants the
  Resources reader slightly wider could give it its own `MaxW` (e.g. 130–140)
  without touching the constant. Default stays 120 for consistency — flagged in
  Open questions.
- **New windows still cascade from the top-left** (`x := 2 + offset*3`,
  `tui.go:1787`). A 120-wide window on a 766-col terminal leaves dead space to the
  right. That is pre-existing positioning, out of scope for a width fix, and not
  changed here.

## Criterion 3 — No regressions

- Boundary-only functions stay boundary-only: `maximizedWindowRect`,
  `clampWindowSize`, `clampWindowRect`, tiling `tileArea`, sidebar clamps — none
  touched.
- The cap is applied in exactly two window paths (`openWindowAny`,
  `applyLayout`) and three dialog specs; every other path is exempt by
  construction.
- Explicit larger dialog caps (Sessions 160, Commands 140, Watchers 130)
  unaffected — they set their own `MaxW`; we add `MaxW` only where it was absent.
- **Window-sizing tests stay green.** `maximize_test.go`, `sidebar_resize_test.go`,
  `dialog_issue317_test.go`, model-selector/session dialog width tests are
  unaffected — they exercise boundary-only paths or run at the default 80×25
  where the 90% window math (=72) is already below 120, so the cap is a no-op.
- **Three `dialog_sizing_test.go` assertions DO change and MUST be updated** — they
  run at 200 cols where the old 80% default (160) exceeds 120, so the new `MaxW`
  intentionally bites. These tests codify the *old uncapped* behavior; updating
  them to the new ≤120 expectation is part of this change, not a regression:
  1. `TestDialogsSizedToContent` / "list-driven dialogs stay wide…"
     (`dialog_sizing_test.go:165`): asserts palette+help width `== defW` (160) at
     200×50. New result is **120**. Update: assert `b.W == comfortableMaxWidth`
     (120) at 200 cols, and keep the height-cap/centering assertions as-is. The
     "stays wide" intent is preserved in spirit — wide but capped — so the
     subtest name/comment should be reworded to "…stay wide but cap width to the
     comfortable max."
  2. `TestDialogReResolvesOnResize` / "percentage-driven palette grows…"
     (`dialog_sizing_test.go:269`): palette uses `dialog.Fit(spec)`
     (`command_palette.go:650`), so the capped spec re-resolves on resize. The
     `after.W > before.W` "grows" check still holds (before is at a narrow
     terminal < 120), but `after.W == 160` becomes **120**. Update that assertion
     to `comfortableMaxWidth` and reword to "…grows on resize but stops at the
     comfortable cap."
  3. `TestBrowserPreferredWidthClamped` (`dialog_sizing_test.go:397`): for
     `screenW ∈ {120,160,200}` asserts `gotW == screenW*80/100`. With `MaxW:120`
     the browser resolves to **120** at 160/200 (96 at 120 cols is unchanged).
     Update the `want` to `min(screenW*80/100, comfortableMaxWidth)`, and drop the
     now-false `gotW >= spec.PreferredW` sub-assertion (or recompute it against
     the cap). The doc-comment explaining the dead 85% is updated to note 120 is
     now the binding cap.
- **New tests** assert the additive behavior: window cap at open on a wide
  terminal (e.g. 766×50 → window W == 120), window ceiling on restore via
  `applyLayout` (persisted W=300 → restored ≤120; persisted W=80 → unchanged), and
  each of the 3 dialogs ≤120 on a 766-col terminal; plus a no-op assertion on a
  narrow terminal (e.g. 100 cols → window/dialog widths follow the old %, cap does
  not bite).
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
   alongside the new `MaxW`. It is already functionally inert (clamped to the 80%
   default by turbotui today) and stays inert under the cap; it is retained only
   to avoid a second unrelated test churn in `TestBrowserPreferredWidthClamped`,
   which reads it directly. The maintainer could instead drop the dead
   `PreferredW` and simplify that test — flagged as a non-blocking cleanup.
3. **Resources reader width** — design uses the shared 120. If the maintainer
   judges the prose reader deserves more room than code panes, it could carry its
   own larger `MaxW` (130–140). Default kept at 120 for consistency with the
   established precedent; raising it is a one-line follow-up if desired.
