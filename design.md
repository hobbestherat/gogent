# Design — gogent #471: Theme editor sizes to content, not the 80%×85% balloon

## Problem

`showThemeEditor` (`ui/tui/theme_editor.go`) hands the shared resolver a **floor-only**
spec:

```go
spec := tv.DialogSpec{MinW: themeEditorDialogW, MinH: themeEditorDialogH} // {80, 22}
```

With no `PreferredW`/`MaxW`/`PrefH`/`MaxH`, turbotui's `ResolveDialogRect`
(`turbotv/dialog_spec.go`) falls back to the **percentage default**: 80% wide / 85%
tall. On a 200×50 terminal the editor balloons to **160×42** — far larger than its
two-column swatch/label/field content, which fits in **80 columns** and **26 rows**.

The theme editor is the *only* content-driven dialog that still relies on the default.
Every other one (`sessionsDialogSpec`, `commandsDialogSpec`, `keybindingsDialogSpec`,
`statisticsDialogSpec`, settings/notifications) declares a content-driven
`PreferredW`/`MaxW`/`PrefH`/`MaxH` in `ui/tui/dialog_sizing.go`. This issue brings the
theme editor in line.

## The content footprint (why 80×26)

The editor's geometry is fully determined by the existing per-column constants
(`resolveThemeEditorLayout`). The dialog's natural width is exactly the documented
**80-column floor** — the floor was *designed* as the tight two-column fit:

| span | cols | source |
|---|---|---|
| left border + gap | 2 | `leftX = 2` |
| left column footprint | 36 | `swatchW(7) + 1 + leftLabelW(20) + 1 + fieldW(7)` |
| inter-column gutter | 1 | the one-gap clearance in `resolveThemeEditorLayout` |
| right column footprint | 38 | `swatchW(7) + 1 + maxLabelW(22) + 1 + fieldW(7)` |
| scrollbar column | 1 | `scrollbarX = width-3` |
| gap + right border | 2 | dialog chrome |
| **total** | **80** | `== themeEditorDialogW` |

Growing *wider* than 80 is actively undesirable: `resolveThemeEditorLayout` spreads the
surplus (`extra/2`) into the two label cells, which is exactly the spacing/association
cascade the issue says is **tracked separately**. So width is **pinned at the content
footprint** — `PreferredW == MaxW == MinW == 80` — mirroring how settings pins its
height (`MinH == MaxH == 20`).

Height: the tallest column is `themeEditorContentRows() == 20` logical rows, and the
chrome above/below the viewport is a constant 6 rows
(`themeEditorDialogH - themeEditorVisibleRows == 22 - 16 == 6`). So **26 rows** shows
every role with no scrolling. `PrefH == MaxH == 26`; `MinH == 22` keeps the documented
floor. Below ~31 terminal rows the height is capped by the 85% rule down toward the 22
floor, where the existing scroll viewport takes over — unchanged behaviour.

Resolved sizes under the new spec:

| terminal | resolved | note |
|---|---|---|
| 80×24 | 80×22 | floor held; content scrolls (16-row viewport) |
| 200×50 | **80×26** | capped far below the 160×42 balloon; content fully visible |
| 120×40 | 80×26 | |
| 300×80 | 80×26 | width/height caps hold on ultrawide |
| 30×8 | 80×22 | floor honoured past the screen edge, centred at origin 0 |

## Changes

### gogent — `ui/tui/dialog_sizing.go` (new helper)

Add a `themeEditorDialogSpec()` method mirroring the house pattern, deriving the caps
from the column constants so it can never drift from `resolveThemeEditorLayout`:

```go
// themeEditorDialogSpec is the content-driven size of the modal theme editor (issue
// #471). Like sessionsDialogSpec/keybindingsDialogSpec it expresses a content
// footprint rather than a share of the terminal: two columns of
// swatch+label+field rows plus the inter-column gutter and the scrollbar column come
// to exactly the documented 80-column floor, so width is PINNED there (growing wider
// only spreads the label cells — the spacing cascade tracked separately). PrefH 26
// shows all themeEditorContentRows() roles without scrolling; MinH 22 keeps the
// documented floor (the scrolling viewport takes over below it). Capped so a 200×50
// terminal resolves to 80×26, never the 160×42 percentage balloon. The spec is static
// (no terminal-share term), so the dialog re-centres on resize via the existing
// relayout()/OnResize path.
func (w *Workbench) themeEditorDialogSpec() tv.DialogSpec {
    const (
        leftCol  = themeEditorSwatchW + 1 + themeEditorLeftLabelW + 1 + themeEditorFieldW // 36
        rightCol = themeEditorSwatchW + 1 + themeEditorLabelW + 1 + themeEditorFieldW     // 38
        // left border+gap (2) + leftCol + gutter (1) + rightCol + scrollbar (1) + gap+border (2)
        contentW = 2 + leftCol + 1 + rightCol + 1 + 2 // == themeEditorDialogW (80)
        // chrome above+below the viewport is constant: preset/toggle rows, borders, button row.
        chromeH = themeEditorDialogH - themeEditorVisibleRows // 6
    )
    prefH := themeEditorContentRows() + chromeH // 26 — all roles visible, no scroll
    return tv.DialogSpec{
        MinW: themeEditorDialogW, MaxW: contentW, PreferredW: contentW,
        MinH: themeEditorDialogH, MaxH: prefH, PrefH: prefH,
    }
}
```

`contentW` is a compile-time const that equals `themeEditorDialogW` by construction; if
a future label widens a column, `themeEditorLeftLabelW`/`themeEditorLabelW` change and
`contentW` tracks them. (A one-line guard `if contentW != themeEditorDialogW { panic }`
could be added to `checkThemeEditorLayout`, but the existing collision invariants
already pin the geometry, so I'll assert the equality in a test instead of at init.)

### gogent — `ui/tui/theme_editor.go` (`showThemeEditor`)

Replace the inline floor-only literal with the helper:

```go
spec := w.themeEditorDialogSpec()
```

Nothing else in `showThemeEditor` changes. The `spec` variable already flows into both
`w.dialogRect(spec)` at open and `relayout()` (`w.dialogRect(spec)` on resize), and the
spec is static/path-independent, so **centre + re-centre on resize is preserved for
free**. The renderer keeps reading the resolved live bounds via
`resolveThemeEditorLayout(width, height)`; at 80×26, `extra == 0`, columns stay at the
floor positions and the viewport simply gains 4 rows so the content fits.

### turbotui — none

`ResolveDialogRect` already honours `PreferredW`/`MaxW`/`PrefH`/`MaxH` exactly as the
other dialogs use it (verified in `turbotv/dialog_spec.go`: the percentage is an upper
*cap*, a content-driven preferred below it is honoured, Max only tightens). The seam is
respected — this is a pure gogent spec change. **No turbotui edit.**

## Tests (`ui/tui/`)

### Update pinned-geometry tests to the new caps

- **`dialog_sizing_test.go` → `TestThemeEditorFlooredAndGrows`**: this test currently
  pins the *balloon* (`{200,50,160,42}`, `{120,40,96,34}`, `{300,80,240,68}`) and
  asserts "must actually GROW" past the floor. Rewrite it against
  `w.themeEditorDialogSpec()` (not the bare floor literal) to pin the new caps:
  `{80,24 → 80,22}`, `{200,50 → 80,26}`, `{120,40 → 80,26}`, `{300,80 → 80,26}`,
  `{30,8 → 80,22}`. Replace the "must grow" assertion with the new invariant: width is
  capped at 80 (`< 160`) on every roomy terminal, height never exceeds 26, never below
  the 80×22 floor, centred throughout. Rename to e.g.
  `TestThemeEditorSizedToContent`.
- **`dialog_issue317_test.go` → `TestThemeEditorOpensFlooredAndGrows`**: opens the REAL
  editor; update expectations `{200,50 → 80,26}`, `{120,40 → 80,26}`, `{80,24 → 80,22}`.
- **`dialog_issue317_test.go` → `TestThemeEditorColumnsSpreadOnWideTerminal`**: its
  premise (columns spread into surplus width) is now intentionally *false* — width is
  pinned, so the "Controls" header sits at the **same** screen column on 80-wide and
  200-wide. Invert it to `TestThemeEditorColumnsStableAcrossWidth` asserting the column
  position is identical (the cap holds; no spread into label cells).

### Add the issue's acceptance assertion

- New `TestThemeEditorWidthCappedOnWideTerminal` (in `dialog_issue317_test.go` or a new
  `theme_issue471_test.go`): on 200×50 the resolved dialog width is `< 160` (and `== 80`),
  driving the real `w.showThemeEditor()` so it catches drift between the documented spec
  and what the editor hands the resolver. Also assert `themeEditorDialogSpec().PreferredW
  == themeEditorDialogW` (the footprint derivation equals the floor) and `PrefH == 26`.

### Thorough suite — happy path + unhappy paths

- **Happy path**: open on 200×50 → 80×26, centred at `((200-80)/2,(50-26)/2)`, all roles
  including `Code block background:` visible at `scrollY==0` (no scroll). This is the
  existing `TestThemeEditorNoScrollWhenGrown` — confirm it still passes at the new 26-row
  height (visibleRows 20 == contentRows 20).
- **Tiny-terminal floor**: 80×24 → 80×22; menu-bar clearance held
  (`TestThemeEditorClearsMenuBarAtFloor`); content still scrolls to reach `code_bg`
  (existing floor scroll tests).
- **Ultrawide cap**: 300×80 → 80×26 (both caps bite).
- **Resize re-centre**: open at 80×24, resize to 200×50, assert bounds == a fresh open
  at 200×50 (`TestThemeEditorOuterBoundsReResolveOnResize`,
  `TestThemeEditorReflowsContentOnResize`) — both now compare 80×26 vs 80×26; still pass
  unchanged. Shrink-back-to-floor re-enables scrolling
  (`TestThemeEditorResizeReflowsColumnsAndScroll`) — still valid.

Stdlib-only `testing`; no new deps. Re-run the baseline:
`go test ./ui/tui/ -run 'Issue462|Issue267|Issue317|ThemeEditor'`.

## Design-criteria assessment

**(1) Goal match.** Exactly the ask: make the theme editor's `DialogSpec`
content-driven so it no longer defaults to 80%×85%. `PreferredW`/`MaxW` = 80 (the two
columns + gutter + scrollbar footprint), `PrefH`/`MaxH` = 26 (content rows + chrome). On
200×50 width resolves to 80 < 160. No scope creep: the role set, save/save-as/delete,
colour picker, scrolling model, preset dropdown are untouched; the label-cell spreading
and gutter/association are explicitly left to their separate issues (and, by pinning
width, are simply no longer triggered).

**(2) Usability.** The dialog now sizes to its content instead of swallowing the screen.
On a roomy terminal it gains exactly the rows it needs (26) so every role is visible
without scrolling; on a small terminal it floors at 80×22 and the existing scroll
viewport still reaches every role. It centres on open and re-centres on resize via the
preserved `relayout()`/`OnResize` path. The user still drives every input — fields,
pickers, toggles, buttons — unchanged.

**(3) No regressions.** `checkThemeEditorLayout` (init guard) and `themeEditorColumns()`
assert invariants at the 80×22 floor and are geometry-independent of the new caps —
unaffected, re-run. `resolveThemeEditorLayout` is unchanged; at 80×26 `extra==0` keeps
the floor column positions, and the resolver-level grow tests
(`TestResolveThemeEditorLayoutGrows/Floor/Monotonic`, the scroll-helper tests) call
`resolveThemeEditorLayout` with explicit dims and still pass. The only tests that change
are the three that *pinned the balloon*, which this issue deliberately reverses; they're
updated to the new caps. Save/override round-trip, carry-override, picker sentinel, and
saved-themes logic are not touched.

**(4) Holistic / cross-repo.** The change lives entirely in gogent, in the right place:
a `themeEditorDialogSpec()` helper alongside the other content-driven specs in
`ui/tui/dialog_sizing.go`, plus the one-line call swap in `showThemeEditor`. The seam is
respected — turbotui's `ResolveDialogRect` already implements the cap/preferred policy
the other five gogent dialogs lean on; no turbotui change is required or made. The
footprint is derived from the existing geometry constants so the spec and the renderer
cannot drift.

## Open questions

1. **Pin width vs. allow modest growth.** I pin `MaxW == PreferredW == 80` because any
   surplus width is spread into the label cells — the exact cascade the issue defers to a
   separate issue. An alternative is `PreferredW 80, MaxW ~96` to give a little air, but
   that would re-introduce the spreading the issue wants to avoid. I'm going with the pin
   (mirrors settings' pinned height). Flag if a small growth band is preferred.
2. **Helper as method vs. free function.** `themeEditorDialogSpec()` ignores `w` (no
   terminal-share term), but I make it a `*Workbench` method to match
   `sessionsDialogSpec`/`statisticsDialogSpec`/`keybindingsDialogSpec`. A free function
   would read marginally cleaner; house consistency wins unless you'd rather it be free.
