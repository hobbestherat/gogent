# Design — gogent layout change for issue #477

Theme-editor dialog: widen the gap between the two colour-role columns 1 → 4 spaces,
raise inter-section vertical spacing, and lift the dialog width floor 80 → 83 to absorb
the 3 extra columns. **gogent-only**; turbotui is not touched.

The issue's "Required changes" list is correct as far as it goes but **misses two
load-bearing reconciliations** that this design adds (both verified against source):
1. the #471 content-pinned sizing spec in `dialog_sizing.go` shares the same gutter and
   must move in lockstep, or the spec breaks (`MinW > MaxW`);
2. a latent `chromeH` off-by-one in that same spec, masked today but exposed once the
   content grows to 22 rows — without the fix the "grown" editor is one row short and an
   existing test (`TestThemeEditorNoScrollWhenGrown`) regresses.

---

## 1. What the code does today (verified against current source)

`ui/tui/theme_editor.go` draws the colour roles as two columns inside a scrolling
viewport. Geometry is resolved at runtime by `resolveThemeEditorLayout(width, height)`;
the floor is `themeEditorDialogW × themeEditorDialogH = 80×22`.

- **Inter-column gap (horizontal)** — `resolveThemeEditorLayout` (~line 300):
  `rightX := leftX + leftLabelW + themeEditorFieldW + themeEditorSwatchW + 2 + 1`.
  The trailing `+ 1` is the single gap column between the left column's swatch and the
  right column's label → `rightX = 2+20+7+7+2+1 = 39` at the floor. The `+ 2` is the two
  intra-row single-space gaps (swatch→name, name→code) — **not** the inter-column gap.
  `cellRect` (swatch → label → field, #462) is independent of `rightX`.

- **Section spacing (vertical)** — each `themeEditorColumn` carries `sectionPad`; the
  row-building loop in `showThemeEditor` and `themeEditorColumnRows` both advance
  `1 + col.sectionPad` blank rows per inter-section gap. Today left `sectionPad = 1`
  (2 blank rows/gap), right `sectionPad = 0` (1 blank row/gap). The left's extra row is
  the cross-column alignment lever: its first section (Session output, 8 rows) is one row
  shorter than the right's (Controls, 9 rows), so the two columns need `pad_L = pad_R + 1`
  for their second sections (UI chrome / Buttons and inputs) to share a logical row.

- **Width floor** — `const themeEditorDialogW = 80`. Derived: `themeEditorScrollbarX =
  themeEditorDialogW - 3` (= 77); `themeEditorVisibleRows = themeEditorDialogH - 3 -
  themeEditorButtonGap - themeEditorContentTop` (= 22-3-1-3 = **15**, height-driven).

- **#471 content-pinned sizing (NOT in the issue's change list)** —
  `ui/tui/dialog_sizing.go` `themeEditorDialogSpec()` pins width to a content footprint:
  `contentW = 2 + leftCol(36) + 1 + rightCol(38) + 1 + 2 = 80`, where the middle `+ 1` is
  the **same inter-column gutter**, and sets `MinW = themeEditorDialogW`,
  `MaxW = PreferredW = contentW`. Height: `PrefH = themeEditorContentRows() + chromeH`
  with `chromeH = 3 + themeEditorContentTop = 6`.

- **Content height & scroll** — `themeEditorContentRows()` = tallest column = **21**
  today; floor `maxScroll = 21 - 15 = 6`; `PrefH = 21 + 6 = 27`.

- **Latent `chromeH` bug (found by tracing).** The resolver computes
  `visibleRows = height - 3 - themeEditorButtonGap - themeEditorContentTop` (= height − 7),
  but `chromeH = 3 + themeEditorContentTop` (= 6) **omits `themeEditorButtonGap`** (the
  inline comment even mis-states the formula as `height-3-contentTop`). So at `PrefH` the
  grown viewport is `PrefH − 7 = contentRows − 1` — one row short of the content. Today
  this is invisible because `code_bg` (the role `TestThemeEditorNoScrollWhenGrown` checks)
  sits in the **right** column, which is only 20 rows, ≤ the 20-row grown viewport; the
  one role that actually overflows is the left column's last row, which no grown-size test
  checks. Once #477 makes both columns 22 rows, `code_bg` lands at the very bottom and the
  off-by-one becomes a visible, test-failing regression.

---

## 2. The change

### gogent source — `ui/tui/theme_editor.go`

1. **Width-floor const** (`const` block ~line 238): `themeEditorDialogW = 80` → `83`.
   `themeEditorScrollbarX` (→ 80) and `themeEditorVisibleRows` (stays 15) re-derive.

2. **Inter-column gap** (`resolveThemeEditorLayout`): `... + 2 + 1` → `... + 2 + 4`.
   At the floor `rightX = 2+20+7+7+2+4 = 42`.

3. **Section padding** (the `columns` literal): left `sectionPad 1 → 2`, right
   `sectionPad 0 → 1`. `themeEditorColumnRows` needs **no** change (it already counts
   `1 + col.sectionPad` per gap).

### gogent source — `ui/tui/dialog_sizing.go` (the #471 reconciliation)

4. **`contentW` gutter** `2 + leftCol + 1 + rightCol + 1 + 2` → `... + leftCol + 4 +
   rightCol ...` so `contentW = 2+36+4+38+1+2 = 83 == themeEditorDialogW`. **Mandatory:**
   if `themeEditorDialogW` moves to 83 while `contentW` stays 80, the spec becomes
   `MinW(83) > MaxW(80)` — a malformed DialogSpec.

5. **`chromeH` off-by-one fix** `chromeH = 3 + themeEditorContentTop` (6) →
   `chromeH = 3 + themeEditorButtonGap + themeEditorContentTop` (**7**), so it mirrors the
   resolver's `visibleRows` formula exactly. Then `PrefH = 22 + 7 = 29` and
   `visibleRows(29) = 29 − 7 = 22 = contentRows` → every role fits the grown editor with no
   scroll, with the buttonGap blank row preserved. Fix the comment too (it currently drops
   buttonGap). *This keeps `TestThemeEditorNoScrollWhenGrown` green — without it the grown
   editor is one row short and that test fails.*

### Stale-comment pass (these become FALSE after the change — reword in the same commit)

- `theme_editor.go` const-block doc — "Width floors at 80 so the dialog fits a standard
  80-column terminal" → floors at **83**, and explicitly note it no longer fits a standard
  80-column terminal (clips below 83; see §Usability).
- `theme_editor.go` `themeEditorScrollbarX` doc — "80-wide minimum" → "83-wide minimum".
- `resolveThemeEditorLayout` doc — floor example `{x:39,labelW:22}` → `{x:42,labelW:22}`;
  "clears the right by **one** gap column" → "by **four** gap columns"; "≥ 80" → "≥ 83".
  Inline "One gap column …" → "Four gap columns …".
- `themeEditorColumn` doc block — rewrite the 1/0 narrative to the 2/1 regime (right column
  now 2 blank rows/gap, left 3; the left's extra row is the alignment lever).
- The scattered "80×22 floor/minimum" mentions in `theme_editor.go` → "83×22".
- `dialog_sizing.go` — "documented 80-column floor"/"80×22 floor" → "83-…"; the gutter
  comment "gutter (1)"/"Equals … (80)" → "(4)"/"(83)"; and the `chromeH` comment fixed.

### Verified arithmetic at the new 83-wide floor

- `extra = 83 − 83 = 0` → `leftLabelW = 20`, `rightLabelW = 22`, `rightX = 42`.
- **Inter-column gap** = `rightX − (leftX + leftLabelW + fieldW + swatchW + 2)`
  = `42 − 38` = **4** ✓ (3 additional). On a wider dialog `extra/2` grows each label cell,
  so the gap stays 4 while the columns spread — matches "grows correctly on wider terminals".
- **Right-column collision:** swatchEnd = `42+22+7+7+2−1 = 79`; `scrollbarX = 80`; limit
  `scrollbarX−1 = 79`; `79 ≤ 79` ✓ (field at cols 73–79, scrollbar 80, border 81–82).
- **Left clears right:** swatchEnd = `2+20+7+7+2−1 = 37`; limit `rightX−1 = 41`; `37 ≤ 41` ✓.
- **Section alignment:** left UI chrome at `9 + pad_L = 11`; right Buttons at
  `10 + pad_R = 11` → aligned ✓; both first sections still at row 0.
- **Column rows:** left `= 8 + (1+2) + 11 = 22`; right `= 9 + (1+1) + 7 + (1+1) + 2 = 22`;
  tallest = **22** (was 21). `maxScroll = 22 − 15 = 7` (was 6).
- **Reveal invariant** (`contentRows − maxScroll ≤ visibleRows`): `22 − 7 = 15 ≤ 15` ✓.
- **#471 spec:** `contentW = 83 == MinW`; `PrefH = 22 + 7 = 29`; grown height 29.

### gogent tests (partner writes them; this is the complete target list)

- **`theme_issue462_test.go`** `TestThemeEditorColumnRowsCountsSectionSeparators`:
  `left.sectionPad` 1 → **2**, `right.sectionPad` 0 → **1** (+ narrative comments). The
  `wantRows` helper and the second-section alignment check re-derive from `sectionPad`.
- **`dialog_issue317_test.go`**: `TestThemeEditorContentRowsGeometryIndependent` rows
  21 → **22**; `TestThemeEditorOpensFlooredAndGrows` `wantW 80 → 83`, grown `wantH 27 → 29`;
  any `resolveThemeEditorLayout(80,…)` / sweep that hard-codes the 77 scrollbar or asserts
  the swatch-flush invariant must start at the **83** floor (below it the renderer never
  feeds the resolver).
- **`dialog_sizing_test.go`** `TestThemeEditorFlooredAndGrows`: `wantW 80 → 83` (incl. the
  sub-floor `{70,…}`/`{80,…}` rows, which now resolve **wider than screen** — annotate that
  it's the accepted below-floor clip, not an accident), grown `wantH 27 → 29`.
- **New assertions** (`theme_editor_test.go` or `theme_issue477_test.go`):
  - gap: `rightX − (leftX + leftLabelW + fieldW + swatchW + 2) == 4`;
  - `themeEditorColumns()[0].sectionPad == 2 && [1].sectionPad == 1`, second sections on the
    same logical row;
  - `themeEditorDialogW == 83`; `themeEditorContentRows() == 22`; floor `maxScroll == 7`;
  - **PrefH/no-scroll:** `themeEditorDialogSpec().PrefH == 29` and a grown editor shows
    `code_bg` at `scrollY == 0`;
  - **cross-file invariant:** `themeEditorDialogSpec()` returns
    `MinW == MaxW == PreferredW == themeEditorDialogW` (the only test-time link between the
    bare const and the `contentW` sum).

Auto-tracking (no change): `theme_issue265_test.go`, `theme_issue279_291_extra_test.go`
derive from `themeEditorColumns()`/`cellRect`; `maxScroll` references are dynamic;
`checkThemeEditorLayout` and `themeEditorColumnRows` need no code edit.

**Known stale test the partner must retarget:** `TestIssue243DialogFitsEightyColumnTerminal`
renders the dialog into an 80-wide buffer and asserts it fits in 80 columns — exactly the
guarantee #477 repeals. It must be re-pointed at an ≥83-wide buffer (or dropped). It is a
test-side fix, not a code defect.

---

## 3. The four design criteria

### (1) Goal match
Exactly the three asks, no scope creep: one term in `rightX` (gap 1→4), the existing
`sectionPad` lever (no new mechanism), one const (floor 80→83). Row-internal order
(swatch→name→code), the scrolling model and the colour/override round-trip are untouched.
The two additions beyond the issue's literal list — `dialog_sizing.go contentW` and
`chromeH` — are **mandatory for internal consistency / no-regressions**, not features: the
first prevents `MinW > MaxW`, the second keeps the "grown editor shows every role" contract
(and its test) true once the content grows to 22 rows.

### (2) Usability
- **≥ 83 cols:** 4-col gutter visually separates the columns; right column shows 2 blank
  rows between sections, both second sections aligned at row 11; dialog still centres,
  stays width-pinned at 83 (no balloon), height grows to PrefH 29 so **every role is
  visible without scrolling**; at the floor the viewport scrolls and `maxScroll = 7` brings
  every role into view. Keyboard/wheel/scrollbar paths unchanged.
- **< 83 cols (standard 80-col terminal): surfaced regression, accept-and-document.**
  turbotui's `resolveDimension` applies `MinW` **last** ("honours its floor even if that
  slightly exceeds the screen", `turbotv/dialog_spec.go`), so `MinW=MaxW=PreferredW=83`
  resolves to **83 wide on an 80-col screen**, centred at `x → 0`; the rightmost 3 cols —
  scrollbar (80) and right border (81-82), and the last role's field value's final glyph —
  clip off-screen. This is the issue's explicit mechanism ("raise the floor 80→83") and is
  consistent with the dialog's **pre-existing** below-floor clipping (today a 70-col
  terminal already clips the 80-wide floor). The change only moves the clip threshold
  80 → 83; the editor's effective minimum terminal width becomes 83. Keyboard Up/Down/
  PageUp/PageDown and the wheel still scroll (not geometry-bound), so the role list stays
  reachable. The design (a) re-words the now-false "fits a standard 80-column terminal"
  comment, (b) annotates the enshrining test rows, (c) raises it for sign-off in Open
  Questions. A responsive gutter (shrink 4→1 on a narrow terminal) would preserve 80-col
  fit but contradicts the issue's fixed "+3 gutter/floor", so it is out of scope unless
  the maintainer asks.

### (3) No regressions
- `checkThemeEditorLayout` (init guard) reads `themeEditorColumns()` at the new floor; the
  collision (79 ≤ 79, 37 ≤ 41) and reveal (15 ≤ 15) arithmetic hold → no panic (confirm by
  running tests/binary).
- `themeEditorColumnRows`/`contentRows`/`maxScroll`/scrollbar thumb stay consistent because
  the same `1 + sectionPad` count drives the build loop and the row-count helper.
- The `chromeH` fix is precisely what prevents a regression in `TestThemeEditorNoScrollWhenGrown`
  / `TestThemeEditorResizeReflowsColumnsAndScroll` (both assert `code_bg` visible on a grown
  dialog). Omitting it would leave them red.
- Comment hygiene is part of this: five+ prose sites narrate the old 80/1-gap regime and
  become active falsehoods; all are in the stale-comment pass.
- `go vet ./...`, `go test ./...` (no `-race` on Pi5), gofmt, build, golangci-lint green.

### (4) Holistic (gogent ↔ turbotui)
Change lives entirely in gogent's editor geometry, computed from turbotui primitives whose
API is unchanged (`tv.NewDialog`, `tv.DialogSpec`/`ResolveDialogRect`, `tv.NewComponent`,
`ColorPicker`, `TextBox`). We feed the resolver larger floor/footprint numbers; add no
primitive, call nothing new. **No turbotui edit, no `go.mod` bump.** Downstream effect on
turbotui: none — `ResolveDialogRect` just receives `MinW = MaxW = 83`. The one cross-file
coupling is *inside gogent* (`theme_editor.go` ↔ `dialog_sizing.go` share the gutter); both
gutter terms move together so `contentW == themeEditorDialogW` stays invariant, and a new
test asserts the spec resolves `MinW==MaxW==PreferredW==83` so the two can't drift.

### Regression-risk callouts
- **Forgetting `dialog_sizing.go contentW`** → `MinW(83) > MaxW(80)`, broken spec. Primary risk.
- **The `chromeH` off-by-one** → grown editor one row short; two existing tests go red. Easy
  to miss because the issue's "+1 row" prose implies PrefH just becomes 28; the correct
  value is 29.
- **Left `sectionPad` must be `pad_R + 1`** for alignment; equal pads silently stagger the
  second sections (caught by the #462 alignment test).
- **Scope discipline:** confine edits to `theme_editor.go` + `dialog_sizing.go` (+ tests).

---

## 4. Open questions

- **"2 blank rows on BOTH columns" vs alignment.** Alignment forces `pad_L = pad_R + 1`, so
  blank-rows-per-gap can never be equal: the issue's explicit pads give the **right column 2
  blank rows and the left column 3** (the left's third row is the alignment lever, same
  structure as today's 2-vs-1). I implement the issue's explicit `sectionPad` values
  (left 2, right 1) — they're unambiguous and backed by the issue's own alignment math and
  22/22 row counts. Literal "2 on both" would require dropping the alignment requirement;
  the two conflict.
- **Raising the floor 80→83 drops standard-80-col fit (needs sign-off).** Implemented as the
  issue's explicit mechanism (accept-and-document), consistent with existing below-floor
  clipping. Flagged only because "fit a standard 80-column terminal" was a stated design
  value being knowingly repealed. Alternative if the maintainer prefers: a responsive gutter
  (out of scope as written).
- **Follow-up (not done here):** derive `themeEditorDialogW` from the column-width + gutter
  expression so the const and `contentW` can't drift; closed at the test layer for now.
- No other ambiguity: all numbers cross-checked against source and the init-guard arithmetic.
