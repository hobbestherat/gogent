# Design — TALL-BUTTON-MODELS-DIALOG (gogent half, Phase 2) — issue #529

> "Models… dialog: 2-row-tall footer buttons." Make the six Models… footer action
> buttons render **2 rows tall** for breathing room, consuming the already-merged
> turbotui Phase-1 tall-button support. Every **other** gogent dialog stays 1 row
> (opt-in only).

## Summary of the change

gogent's Models… dialog lays its footer buttons out with its own helper
`footerButtonRects` (in `ui/tui/dialog_buttons.go`) and constructs each button with
`newButton` (in `ui/tui/theme.go`) — it does **not** use turbotui's `NewButtonRow`
for this dialog. So the tall-button seam in gogent is simply the `Rect.H` we hand to
each button:

```
footerButtonRects(...) -> []tv.Rect{H:1}  ->  newButton(label, rect)  ->  tv.NewButton  ->  Button.draw (honours any bounds.H)
```

Phase-1 turbotui already made `Button.draw` render a solid `[ … ]` block at any
`bounds.H`, with the caption + focus chevrons on the vertically-centred row, and
`ButtonHeight(bounds)` defaulting to 1 when `H<=0` (verified in
`$HOME/work/turbotui/turbotv/widget_button.go` and `measure.go`). At `H==1` the draw
is byte-for-byte identical to the old one-row button. **No turbotui change is needed
in Phase 2** — we only bump the dependency and pass `H:2` from gogent.

## Files / functions touched (gogent only)

1. **`go.mod` / `go.sum`** — bump turbotui to the Phase-1 release SHA:
   ```
   go get github.com/hobbestherat/turbotui@1cdd5ba1098245b5093dc285638601e1423dea7b
   go mod tidy
   ```
   No `replace` directive. After the bump go.mod must reference the pseudo-version
   resolved from that SHA (replacing the current
   `v0.3.1-0.20260627095617-54dc4b757884`).

2. **`ui/tui/keybindings_issue401_test.go:175`** — this is the existing turbotui
   version-pin guard. It asserts the exact string
   `github.com/hobbestherat/turbotui v0.3.1-0.20260627095617-54dc4b757884`. It must
   be updated to the new pseudo-version produced by step 1, or it will fail. (This is
   the only place that pins the literal version.)

3. **`ui/tui/dialog_buttons.go` — `footerButtonRects`** — add a height-aware variant
   *without* changing the existing signature so no other caller is touched:
   ```go
   // existing signature unchanged — defaults to H:1 (all current callers keep it)
   func footerButtonRects(labels []string, leftX, rightX, y, gap int) []tv.Rect {
       return footerButtonRectsH(labels, leftX, rightX, y, gap, 1)
   }

   // new variant: h<=0 collapses to 1 (mirrors turbotui's ButtonHeight contract)
   func footerButtonRectsH(labels []string, leftX, rightX, y, gap, h int) []tv.Rect {
       if h < 1 { h = 1 }
       // ... identical layout loop, but Rect{..., H: h}
   }
   ```
   The layout math (right-alignment, gap, `clampDialogRect`) is unchanged — only the
   rect height is parameterised. Default path (`h==0 ⇒ 1`) is the regression guard
   for every other dialog.

4. **`ui/tui/model_dialog.go` — `showModelsDialog`** — two edits:
   - `height0 := paneRows + 7` → `height0 := paneRows + 8` (one extra row for the
     taller footer; keep the existing hint/border comment math).
   - The footer call `footerButtonRects(labels, 2, width-3, buttonY, tv.DefaultButtonGap)`
     → `footerButtonRectsH(labels, 2, width-3, buttonY, tv.DefaultButtonGap, 2)`.

   Nothing else in the function changes: `listX/listW/listY`, `hintY = height-4`,
   `buttonY = height-3`, `paneH = height - listY - 5` all keep their formulas and
   re-resolve against the new `height0`. `dialog.Fit(spec)` re-runs the same rect
   resolution on resize, so the reflow path is covered for free.

### Geometry trace at `height0 = paneRows + 8` (with 2-row buttons)

Borders at rows `0` and `height-1`; interior is rows `1 .. height-2`.

| element            | formula          | row(s)                       |
|--------------------|------------------|------------------------------|
| top border         | 0                | 0                            |
| list pane          | `listY=2`, `paneH=height-7=paneRows+1` | `2 .. paneRows+2` |
| blank gap          | —                | `paneRows+3`                 |
| hint               | `hintY=height-4` | `paneRows+4`                 |
| footer buttons     | `buttonY=height-3`, H=2 | `paneRows+5 .. paneRows+6` |
| bottom border      | `height-1`       | `paneRows+7`                 |

The button now occupies the bottom two interior rows. `paneH` resolves to
`paneRows+1` (one trailing blank list row) — harmless: the Tree only paints its
`paneRows` nodes; selection/scroll/`OnActivate` are unaffected. This matches the
task's prescription (`+1`, keep `buttonY=height-3`).

## User-facing behaviour

- The six actions (**Add from Catalog…**, **Add Empty…**, **Edit…**, **Remove**,
  **Set Default**, **Done**) render as solid two-row `[ … ]` blocks. Caption +
  focus chevrons sit on the centred (lower) row; the box outline frames both rows —
  visually weightier, less cramped footer.
- Buttons still flow on a **single physical row** when the terminal is wide enough
  (the right-aligned layout in `footerButtonRectsH` is unchanged); the change is
  *height*, not row count, exactly as the issue's resolved open question requires.
- **Empty-list state** (zero models → only Add Empty + Done, plus Catalog when
  available): identical layout logic, now at the taller height — both buttons render
  2 rows tall, placeholder + hint unchanged.
- **Resize**: `dialog.Fit(spec)` recomputes the rect with the same `MinH=MaxH=height0`,
  so `buttonY/hintY/paneH` stay consistent at the new height after a terminal resize.

## Criterion-by-criterion

**(1) Goal match.** Exactly the issue's ask: the six Models… footer buttons become
2 rows tall via the merged turbotui tall-button API; go.mod bumped to the Phase-1
SHA; dialog grows by one row. No scope creep — single dialog, opt-in height, no new
widget, no behavioural change to add/edit/remove/set-default flows.

**(2) Usability.** Footer reads as a weightier two-row block instead of a thin
strip; the dialog still drives entirely from the keyboard/mouse as before (Tab move,
Enter edit, Esc close, mnemonics). The right actions are surfaced per state
(empty-list vs populated) unchanged. Nothing goes silent.

**(3) No regressions.**
- Every other caller of `footerButtonRects` keeps the original signature → `H:1`
  path → identical output. A focused regression test will pin `H==1` on the default
  path.
- turbotui `Button.draw` at `H==1` is documented and tested (Phase-1) to render
  identically to the prior one-row button, so all 1-row dialogs are byte-identical.
- `model_selector_width_test.go` asserts only *width* behaviour (floor + growth),
  untouched by a height change.
- The version-pin guard test is updated in lockstep with the go.mod bump (item 2).
- **Risk — button shadow vs. bottom border (default/shadow-on theme):** with
  `buttonY=height-3` and `H=2`, the button bottom lands on `height-2` (last interior
  row) and turbotui's `DrawShadow` paints its bottom band one row lower, at
  `height-1` — the **bottom border row**. On the default theme (`shadowsEnabled`)
  this overpaints the border under the button width with shadow glyphs. With
  NoShadow it is a non-issue. See Open Questions for the fix options; flagging now
  because it is the one real visual-regression surface of the prescribed arithmetic.

**(4) Holistic / repo seam.** All Phase-2 work is gogent-only and confined to
`ui/tui` (`dialog_buttons.go`, `model_dialog.go`) + tests + the go.mod/go.sum bump +
the version-pin test. We **consume** the turbotui Phase-1 contract (tall `Button.draw`,
`ButtonHeight` default-1) rather than re-implement it — the seam is respected: gogent
owns "which dialog opts into H:2"; turbotui owns "how a button of height H draws."
No new deps, no `replace`. `ui/tui` stays free of `internal/daemon|server` imports
(tests use `Handlers` stubs only). Downstream effect on turbotui: none (read-only
consumer of a merged, released API).

## Tests (to be written in the implement phase)

In `ui/tui/models_dialog_test.go`:
- Assert the Models… footer button rects carry `H == 2` (drive the real
  `showModelsDialog`, inspect the constructed button bounds / rendered grid).
- Assert the dialog height is `paneRows + 8` (was `+7`) for a known model count.
- Assert the **empty-list** state still has only Add/Done (+Catalog when wired)
  enabled and lays out at the taller height without panic/clipping.
- Existing model-selector *width* tests remain unchanged and green.

In `ui/tui/dialog_buttons_test.go` (regression guard):
- Assert `footerButtonRects(...)` (default / no-height path) yields `H == 1` for
  every rect — proving other callers are unaffected.
- (Optional) assert `footerButtonRectsH(..., 0)` collapses to `H == 1`.

`go mod tidy` clean; gofmt/build/vet/golangci-lint clean; `go test ./...` green
(pre-existing `TestUserSessionSendMessage` 404 the only acceptable failure).

## Open questions

1. **Shadow-on-border (default theme).** The prescribed `height0=paneRows+8` with
   `buttonY=height-3` puts a 2-row button flush against the bottom interior row, so
   its drop shadow lands on the bottom border row (default `shadowsEnabled` theme).
   Three resolutions, in order of least deviation from the brief:
   - **(a)** Accept it — likely how Phase-1 was validated via the rendered-grid
     test; the band is subtle. (Matches the brief literally.)
   - **(b)** Keep `+8` but move the footer up one row (`buttonY = height-4`) so the
     button occupies `height-4..height-3` and the shadow falls on the blank
     `height-2` interior row — clean gap, but deviates from "keep buttonY=height-3".
   - **(c)** Bump `height0 = paneRows + 9` (two extra rows), keep `buttonY=height-3`,
     giving a blank interior row below the button for the shadow — deviates from
     "one extra row" and makes the dialog one row taller.

   **Recommendation:** confirm with the maintainer (kloune). Default to **(a)** to
   honour the explicit arithmetic in the brief unless the shadow band on the border
   is judged a visible regression in the implement-phase render test, in which case
   **(b)** is the cheapest clean fix (one extra row total, clean shadow gap).

2. **Trailing blank list row.** `paneH` resolves to `paneRows+1` under the `+8` math,
   so the Tree has one unused row. Benign (no extra node drawn). No action proposed
   unless the maintainer prefers clamping `paneH` to the node count.
