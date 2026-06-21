# Issue #195 — Delineate the status line from the transcript

## Problem
In the live session window `LayoutFn` (`ui/tui/session_window.go`), the transcript
(history) ends at row `ht-inputH-2` and the status label starts on the very next
row (`ht-inputH-1`) with no gap, rule or border. The status line therefore reads
as a continuation of the last assistant message.

## Design
Fence the **controls region** (status line + input row) with a horizontal divider
rule drawn on its own row directly above the status line. The window frame already
draws the left, right and bottom borders of that region, so a single horizontal
rule across the inner width supplies the missing top edge — effectively a box
around the controls region (the issue's preferred option) at the cost of one row,
which works at small sizes and with the resizable window.

The rule is a dedicated `*tv.Label` (`separator`) whose text is a full-width run of
`─` (box-drawing horizontal) in the chrome divider colour, matching the separators
already used in the sidebar. It is re-sized to the window width on every layout, so
it tracks resizes.

### Layout (live branch)
- Header row: `Y=0` (unchanged).
- History: `Y=1, H = ht-inputH-3` (one row shorter to free the rule row).
- Separator rule: `Y = ht-inputH-2, W=wd, H=1` (NEW — the controls-region top border).
- Status: `Y = ht-inputH-1` (unchanged position relative to the input).
- Input row: `Y = ht-inputH` (unchanged), buttons via `layoutInputRow`.
- Min-height guard bumped `ht < 6` → `ht < 7` so history keeps `H >= 1`.

The separator carries zero bounds on the read-only (analysis) window — it is only
created/added/laid out on the live window, so read-only windows are untouched.

## Interfaces a tester must target
- `sw.separator` — the `*tv.Label` divider; non-nil on live windows, nil on
  read-only. Its bounds row sits between the history bottom and the status row.
- After a live layout: `history.bottom + 1 == separator.Y`,
  `separator.Y + 1 == status.Y`, separator spans the full width, and its text is a
  run of `─`. The status row is no longer flush against the transcript.

## Constraints
- No new dependencies; `gofmt`; lint at 0; tests without `-race`.
- GLM partner writes the tests.
