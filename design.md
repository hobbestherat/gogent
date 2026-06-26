# Design — gogent #472: capture-mode prompt clips the "Backspace clear" unbind hint

## Problem (one paragraph)

In the *Customize Keybindings* dialog, pressing `Enter` on an action enters capture
mode and shows the prompt
`Press a key for "<action>"…  (Esc cancel · Backspace clear)` in a **single-row**
status `Label` (`keybinding_customizer.go:117`, `H: 1`, width `width-4`). The dialog's
inner status width is 58 cells at `PreferredW` (62) and 54 at `MinW` (58). The prompt is
51 cells with an empty name and grows with the action name (up to 79 cells for the
28-char "Set / show goal (supervisor)"). Because turbotui's `Label.draw` renders only
`min(H, len(rows))` wrapped rows (`turbotv/widget_label.go:66`), a one-row label silently
**clips** everything past the first line — and the clipped tail is always the trailing
`(Esc cancel · Backspace clear)`, i.e. the only place in the whole UI that advertises that
a key can be *unbound*.

## Chosen fix

Make the status `Label` **two rows tall** so turbotui's existing `Wrap=true` path
(already set by `NewLabel`, used via `dialogLabel`) renders the second wrapped line. No
turbotui change, no string change, no dialog-width/spec change.

The key realisation from tracing the current vertical budget is that **a blank row already
exists** between the list and the status, so the second status row costs nothing — neither
the list height nor the footer position changes.

### Current vertical layout (traced)

```
listY  = 3
listH  = height - listY - 4   = height - 7
```

| Row            | Content                          |
|----------------|----------------------------------|
| `0`            | top border                       |
| `1`            | "Select an action…" title label  |
| `2`            | blank                            |
| `3 .. height-5`| list (`listH = height-7` rows)   |
| `height-4`     | **blank** (the reclaimable row)  |
| `height-3`     | status label (`H:1`)             |
| `height-2`     | footer buttons                   |
| `height-1`     | bottom border                    |

The `-4` in `listH` budgets four rows below the list; today those are
`blank + status + footer + border`. The list bottom is `3 + listH - 1 = height-5`, so
`height-4` is genuinely unused.

### After the fix

Move the status up one row and make it two rows tall, consuming the blank `height-4` row:

```go
const keybindStatusRows = 2 // capture prompt + unbind hint wrap onto a second line (#472)
...
status := dialogLabel(keybindCustomizerIdleHint,
    tv.Rect{X: 2, Y: height - 2 - keybindStatusRows, W: width - 4, H: keybindStatusRows})
```

`Y = height - 2 - keybindStatusRows = height - 4`, `H = 2` → status occupies `height-4`
and `height-3`. Footer stays at `height-2`, border at `height-1`, list is unchanged.

| Row            | Content (after)                  |
|----------------|----------------------------------|
| `3 .. height-5`| list (`listH = height-7`, **unchanged**) |
| `height-4`     | status row 1 (capture prompt)    |
| `height-3`     | status row 2 (`…  (Esc cancel · Backspace clear)`) |
| `height-2`     | footer buttons (**unchanged**)   |
| `height-1`     | bottom border                    |

`listH = height - listY - 4` is **unchanged**: the `-4` budget is still correct, only its
breakdown shifts from `blank+status(1)+footer+border` to `status(2)+footer+border`. The
comment on that line is updated to say so.

### Files / functions touched (gogent only)

- `ui/tui/keybinding_customizer.go`
  - Add `const keybindStatusRows = 2` next to `keybindCustomizerIdleHint`.
  - `showKeybindingCustomizer` (~L117): status `Rect` `Y: height-3, H: 1` →
    `Y: height-2-keybindStatusRows, H: keybindStatusRows`.
  - Update the `listH` comment (~L105) to reflect the 2-row status replacing the blank
    margin (the `-4` value itself does not change).
  - Update the `captureRefusalMessage` doc comment (L52–58): it currently asserts the
    status is a "single-row status Label" on which a refusal is "never wrapped onto an
    unrendered second row." With a 2-row label that is stale. Refusal messages (≤54 cells
    with the "✗ " prefix) still fit on the first row and are now strictly *safer* (a
    slightly-too-long refusal wraps and stays visible instead of clipping), so this is a
    comment-only correction, not a behaviour change.
- `ui/tui/keybinding_customizer_phase4b_test.go` — add the fit test (below).

**No** change to `ui/tui/dialog_sizing.go` (`keybindingsDialogSpec`), turbotui, the prompt
string, the unbind mechanism, or any other dialog.

### Why a 2-row status is sufficient at every width

The longest prompt is 79 cells ("Set / show goal (supervisor)"). The status inner width
is `width-4`, ranging from `MinW-4 = 54` to `MaxW-4 = 72`. `WrapLabelRunes`
(`turbotv/measure.go:81`, word-wrap with hard-split fallback) yields:

- inner 54 (MinW 58): row1 breaks at the space after `(Esc` (~53 cells), row2 =
  `cancel · Backspace clear` (~25 cells) → **2 rows**.
- inner 58 (PreferredW 62): 79 cells → **2 rows**.
- inner 72 (MaxW 76): 79 > 72 → still **2 rows**.

Since `79 ≤ 2 × 54`, two rows hold the longest prompt at every width in `[MinW, MaxW]`, and
`Label.draw` renders both (`H=2`). The `…`/`·` glyphs are width-1 (confirmed in the issue
and matching `RuneWidth`). The idle hint (`keybindCustomizerIdleHint`, ~55 cells) also
benefits: it currently clips at MinW and will now wrap cleanly onto row 2.

## Test plan (mirrors `keybinding_customizer_phase4b_test.go`)

Primary, faithful fit test — `WrapLabelRunes` is turbotui's *documented* height predictor
(its doc: "`len(WrapLabelRunes(runes, w))` predicts a Wrap-enabled Label's height at width
w"), so `len(rows) ≤ keybindStatusRows` is exactly the no-clip condition `Label.draw` uses:

```
TestKeybindingCustomizerCapturePromptFitsTwoRows
  - find the longest-named action via w.rebindable() (guards future longer names,
    not a hardcoded string), and also assert that name is "Set / show goal (supervisor)"
    as a catalog sanity anchor.
  - prompt := capturePrompt(longest); require strings.Contains(prompt, "Backspace clear").
  - for width in {58 (MinW), 62 (PreferredW), 76 (MaxW)}:
      rows := tv.WrapLabelRunes([]rune(prompt), width-4)
      require len(rows) <= keybindStatusRows           // no clip
      reconstruct text from rows; require it still contains
        "Backspace clear" and the full "(Esc cancel · Backspace clear)" suffix
        (catches a silent truncation as well as a clip).
```

Layout/usability guard (non-empty list, non-overlapping rows after the status grows) at the
`MinH` floor and `PrefH`:

```
TestKeybindingCustomizerStatusLayoutLeavesRoom
  - for height in {16 (MinH), 34 (PrefH)} derive the same rects the dialog uses:
      listY=3; listH=height-listY-4; statusY=height-2-keybindStatusRows; footerY=height-2
    assert: listH >= 1 (non-empty list)
            statusY + keybindStatusRows - 1 < footerY      (status does not reach footer)
            footerY < height-1                             (footer above the border)
            list bottom (listY+listH-1) < statusY          (list does not overlap status)
```

(To avoid re-deriving the formulas in the test, the recommended supporting change is to
keep the constant `keybindStatusRows` as the single source of truth referenced by both the
dialog and the test. If we prefer zero formula duplication, optionally extract the vertical
math into a tiny pure helper `keybindCustomizerVRows(height)` returning
`listY, listH, statusY, footerY` and call it from both `showKeybindingCustomizer` and the
test — low-risk and makes acceptance-criterion #3 directly testable. Default: the constant
+ the two tests above, which is sufficient.)

The existing #461 sizing/footer tests
(`keybinding_customizer_issue461_test.go`) and the phase-4b capture/unbind tests are left
untouched and must stay green; they exercise spec width, footer non-overlap, resize
path-independence, and the unbind mechanism — none of which this change moves.

## Criterion-by-criterion

### (1) Goal match
The change does exactly what the issue asks: it makes the full capture prompt — including
`Backspace clear` — visible (not clipped, not silently truncated) for every rebindable
action at every width from `MinW` (58) to `MaxW` (76). It is a rendering fix, not a feature
or refactor: no string change, no unbind-mechanism change, no spec change. Scope is one
rect, one constant, two comment corrections, and one new test — the issue's recommended
"two-row status label" approach, no more.

### (2) Usability
The user now sees the complete instruction set — crucially that `Backspace` *clears
(unbinds)* the action, the only advertisement of unbinding in the UI. Nothing is silently
hidden off the dialog's right edge. The user still drives input the same way (Enter to
capture, then the gestures the prompt names). The list keeps its full height (no row lost),
the footer buttons keep their positions and never overlap, and the dialog still
centres/re-centres on resize (unchanged spec → `dialog.Fit` path is untouched). One
cosmetic note: the status now sits flush under the list (the former blank separator row
becomes the prompt's first line) instead of with a 1-row gap; this is the issue-sanctioned
trade and keeps the list as tall as possible — preferred over shrinking `listH` at the
`MinH=16` floor where every list row matters. See Open questions.

### (3) No regressions
- `listH` formula and value are unchanged → list stays non-empty (`listH = height-7 ≥ 9` at
  `MinH=16`; the existing `if listH < 3` guard remains a backstop).
- Footer `y = height-2` and `footerButtonRects` call are unchanged → the #461 footer
  non-overlap invariant and `keybinding_customizer_issue461_test.go` sizing tests stay
  green (none of MinW/MaxW/PreferredW/MinH/PrefH/MaxH change).
- The status label's first row still shows the same idle hint and refusal messages; with
  `H=2` they can no longer clip (strictly safer). The `captureRefusalMessage` comment is
  corrected to match.
- No change to `clearBinding`/`unboundChord`/`"none"` persistence, the `?` cheatsheet, the
  welcome dialog, or the browsing idle hint (all explicitly out of scope).

### (4) Holistic design across gogent + turbotui
The fix lives entirely on gogent's side, which is the correct seam: turbotui's `Label`
already supports multi-row wrapping when `H ≥ 2` (`NewLabel` sets `Wrap = true`;
`Label.draw` renders `min(H, len(rows))` rows). gogent was under-allocating the label's
height; the fix gives the widget the height it needs. We deliberately do **not** take the
optional turbotui hardening (truncate-with-ellipsis for one-row labels) — the issue marks
it out of scope and the two-row fix needs no toolkit change. No downstream effect on
turbotui or on other gogent dialogs (the spec is shared by nobody else and is untouched).

## Open questions
1. **Flush vs. gap:** the recommended approach consumes the blank `height-4` row, so the
   status sits directly under the list (no separator). The alternative — shrink `listH` by
   1 (`height - listY - 5`) to keep a 1-row gap — costs one list row at every height,
   including the `MinH=16` floor. I recommend the flush layout (no list shrink); confirm
   that's acceptable, or switch to the gap variant if visual separation is preferred.
2. **Layout-helper extraction:** whether to extract `keybindCustomizerVRows(height)` purely
   for testability (zero formula duplication in the layout test) or keep the inline math
   plus the shared `keybindStatusRows` constant. Default is the latter (smaller diff); the
   former is a clean optional follow-on.
