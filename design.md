# Design — Commands dialog UX overhaul (gogent issue #455)

## Scope & intent

A focused **usability pass** on the Custom Commands editor (`/commands`). It is a
UX fix, not a feature add and not a refactor: the data model
(`config.CommandDef.Template string`), the backend
(`internal/command`, `internal/gogent/commands.go`), expansion
(`command_expand.go`) and the history dialog are all left untouched. Every change
lives in the dialog presentation layer.

**All changes are in `gogent`.** No `turbotui` change is required (verified below).

Files touched:

| File | Change |
|------|--------|
| `ui/tui/commands_dialog.go` | `showCommandsDialog`: template field → `MultiLineInput`; reframed preview; delete confirmation; Enter-advance focus; insert-at-caret; widened labels; new row math; optional dirty guard. |
| `ui/tui/dialog_sizing.go` | `commandsDialogSpec()`: raise `PrefH`/`MaxH` for the taller template. |
| `ui/tui/commands_dialog_issue448_test.go` | Update the pinned spec numbers and the inner-geometry row math; **preserve the invariants**, not the literals. |
| `ui/tui/commands_dialog_test.go` (new or existing) | Add round-trip and delete-confirm tests (see Verification). |

No `internal/`, no `turbotui/` edits.

---

## Key prior finding: Tab navigation is already provided by turbotui

The issue (P4) says "there is no Tab/Shift-Tab handling". In fact turbotui's
`Desktop.dispatchType` already handles it globally (`desktop.go:907-915`):
`KeyTab → moveFocus(true)`, `KeyBackTab → moveFocus(false)`, cycling all
`Focusable` widgets in the top layer in TabIndex-then-reading-order. This runs
**after** the focused widget declines the key. Confirmed both `TextBox`
(`widget_textbox.go:146` returns `false` for Tab) and `MultiLineInput`
(`widget_multiline_input.go:259` returns `false` for Tab) do *not* consume Tab,
so focus already escapes the template editor with Tab.

**Consequence:** Tab/Shift-Tab field cycling works today for any focusable widget
we add — including the new `MultiLineInput`. We must **not** add a dialog-root Tab
handler (it would double-fire with the desktop's). The only genuine P4 gap is the
*Enter-advances-focus* convenience on single-line fields (the `question_dialog.go`
pattern), which we add. This is the holistic-correct placement: focus traversal is
turbotui's responsibility and already correct; gogent only adds the per-field
`OnSubmit` affordance.

---

## Detailed design

### 1. Multi-line template editor (P1)

Replace `tmplBox := mkField("Tmpl:")` (`commands_dialog.go:79`) with a
`*tv.MultiLineInput`:

```go
tmplInput := tv.NewMultiLineInput("", tv.Rect{X: boxX, Y: row, W: boxW, H: tmplH})
tmplInput.WordWrap = true        // prompts are prose; match session_window.go
// OnSubmit left nil → Enter inserts a newline (Save is a footer button).
```

with `tmplH = 6` and a left-aligned `Template:` label on the box's top row. Add it
via the window content list, not `mkField` (which is for 1-row boxes); a small
local `mkMultiField(label, h)` or inline placement keeps the `row` counter honest.

- `currentDef()` (line 158) → `Template: tmplInput.GetText()` (same API as TextBox).
- `loadForm()` (line 202) → `tmplInput.SetText(def.Template)`.
- `MultiLineInput.handlePaste` (widget_multiline_input.go:427) already splits on
  `\n`, so multi-line paste works out of the box — this is the core P1 fix.
- `MultiLineInput` provides scrolling, word-wrap, selection, hardware caret — all
  inherited.

### 2. Live preview reframed as expansion-only (P2)

`wrapLive` (line 180) currently takes a `*tv.TextBox`. `MultiLineInput` exposes the
same `Component.OnTypeFn` seam, so generalise it to wrap a
`*tv.VisualComponent` (`box.Component` / `tmplInput.Component`) rather than a
concrete widget type. Also wrap `Component.OnPasteFn` for the template so a
multi-line paste refreshes the preview immediately (a paste fires no type event):

```go
wrapLive := func(c *tv.VisualComponent) {
    baseType := c.OnTypeFn
    c.OnTypeFn = func(vc *tv.VisualComponent, ev tui.TypeEvent) bool {
        handled := false
        if baseType != nil { handled = baseType(vc, ev) }
        refreshPreview()
        return handled
    }
    basePaste := c.OnPasteFn
    c.OnPasteFn = func(vc *tv.VisualComponent, s string) bool {
        handled := false
        if basePaste != nil { handled = basePaste(vc, s) }
        refreshPreview()
        return handled
    }
}
wrapLive(tmplInput.Component)
wrapLive(argsBox.Component)
```

Both base handlers are non-nil (`TextBox` and `MultiLineInput` each wire
`OnTypeFn` *and* `OnPasteFn` in their constructors — `widget_textbox.go:37-38`,
`widget_multiline_input.go:87-88`), so the wrapper always delegates the actual edit
before refreshing; it never swallows a keystroke or a paste.

`refreshPreview()` (line 164) is reframed so it never echoes the template verbatim:

```go
def := currentDef()
args := strings.Fields(argsBox.GetText())
if len(args) == 0 && !templateHasRefs(def.Template) {
    preview.FG = colorDialogDetail            // dim hint, not body colour
    preview.SetText("enter sample args (positional or name=value) to preview the expanded prompt")
} else {
    preview.FG = tv.DefaultTheme.DialogFG
    out, err := expandTemplate(def, args)
    if err != nil { preview.SetText("(" + err.Error() + ")") } else { preview.SetText(out) }
}
```

`templateHasRefs(string) bool` is a tiny local helper reusing the existing
`cmdBraceRefRe`/`cmdBareRefRe` regexes from `command_expand.go`
(`return cmdBraceRefRe.MatchString(t) || cmdBareRefRe.MatchString(t)`). The
template text now appears only once on screen (in the editor); the preview shows
only the *expanded* result, which is the de-duplication the issue asks for.

### 3. Delete confirmation (P3)

Wrap the existing `deleteAction` body (lines 327-345) in `showConfirm` (verified
signature `showConfirm(title, message string, onResult func(bool))`,
`message_dialog.go:94`; a non-nil `onResult` yields a Yes/No dialog):

```go
deleteAction := func() {
    if originalName == "" { setStatus("Nothing to delete."); return }
    if w.handlers.DeleteCommand == nil { setStatus("Delete is unavailable."); return }
    w.showConfirm("Commands",
        fmt.Sprintf("Delete command %q and all its version history? This cannot be undone.", originalName),
        func(ok bool) {
            if !ok { return }
            if err := w.handlers.DeleteCommand(originalName); err != nil {
                setStatus("Delete failed: " + err.Error()); return
            }
            clearForm(); renderList(); setStatus("Deleted.")
        })
}
```

The pre-checks (no selection / unavailable handler) stay *outside* the confirm so
the user is not prompted for a no-op.

### 4. Keyboard field navigation (P4)

Tab/Shift-Tab already cycles all focusable widgets via the desktop (see finding
above) — **no dialog-root Tab handler is added**. We add only the Enter-advance
convenience, mirroring `question_dialog.go:387-396`: each single-line `TextBox`
gets an `OnSubmit` that `w.desktop.SetFocus(next)`, following the natural visual
(top-to-bottom) order:

```
name  --Enter-->  desc  --Enter-->  template (focus the editor)
model --Enter-->  agent --Enter-->  subtask
```

The template `MultiLineInput` keeps `OnSubmit == nil` so Enter inserts a newline;
the user leaves it with Tab (or by clicking). `argsBox.OnSubmit` re-runs
`refreshPreview` (Enter on the sample-args box previews — useful, since Args has no
"next field" worth jumping to). Tab remains the universal forward/back traversal
for the widgets Enter does not chain (paramList, the param buttons, footer).

Focus order is correct **without** assigning any `TabIndex`: `sortFocusOrder`
(`desktop.go:636`) falls back to reading order (Y then X) for equal TabIndex, and
every widget defaults to TabIndex 0, so the resolved Tab cycle is
`list → name → desc → template → model → agent → subtask → paramList → insert →
+Add → Edit → Del → args → Preview → New → History → Delete → Save → Cancel` — the
expected order. We rely on this rather than hand-numbering.

The dialog-root `OnTypeFn` keeps **only** its existing Escape handler (line 399)
(plus the optional dirty-guard reroute in §7).

### 5. Layout polish (P5)

- `labelW` 7 → **10** so `Template:` (9 chars) fits; `boxX = detailX + labelW`,
  `boxW = detailW - labelW` recompute automatically. Labels become full words:
  `Name:`, `Description:` (or keep `Desc:` — 10 fits `Template:`/`Model:`/`Agent:`
  comfortably; `Description:` is 12 so use `Desc:` or widen to 12 — see Open
  questions).
- `paramsH` 3 → **4** (a touch more room, per P5).
- `tmplH = 6`.
- New top-to-bottom `row` math (detailX=26, listW=22 unchanged):

  | Field | Y | Height |
  |-------|---|--------|
  | Name | 2 | 1 |
  | Desc | 3 | 1 |
  | Template (label left, box) | 4 | 6 (rows 4–9) |
  | Model | 10 | 1 |
  | Agent | 11 | 1 |
  | Subtask | 12 | 1 |
  | Parameters label | 13 | 1 |
  | paramList | 14 | 4 (rows 14–17) |
  | Insert/+Add/Edit/Del | 18 | 1 |
  | Args + Preview btn | 19 | 1 |
  | Preview | 20 | `height-24` |

  `previewBottom = 20 + (height-24) - 1 = height-5`, always `< height-4` (hint
  row) — the footer-collision invariant holds at every resolved height.

- `commandsDialogSpec()` (`dialog_sizing.go:66`): width is **unchanged** (the
  template grows vertically, not horizontally; `boxW` at PreferredW 112 is
  `112-28-10 = 74`, ample). Height grows:

  ```
  MinW 84, MaxW 140, PreferredW 112   // unchanged
  MinH 26, MaxH 40,  PrefH 34         // was MaxH 34 / PrefH 28
  ```

  `MaxH 40 < 42` keeps the anti-balloon guard. `MinH 26` is retained: at the floor
  the preview collapses to its 2-row minimum (`height-24 = 2`) while the template
  still shows all 6 rows — the small-terminal floor stays usable.

### 6. Insert-at-caret (P6)

`insertSel.OnChange` (line 307) currently appends. `MultiLineInput` exposes
`Lines`, `CursorX`, `CursorY` as **public fields**, so insertion at the caret is
done entirely in gogent (no turbotui change):

```go
insertSel.OnChange = func(index int) {
    if index <= 0 || index > len(params) { return }
    ph := "${" + params[index-1].Name + "}"
    y := tmplInput.CursorY
    line := []rune(tmplInput.Lines[y])
    x := tmplInput.CursorX
    if x > len(line) { x = len(line) }
    tmplInput.Lines[y] = string(line[:x]) + ph + string(line[x:])
    tmplInput.CursorX = x + len([]rune(ph))
    insertSel.SetSelected(0)
    w.desktop.SetFocus(tmplInput)   // return focus to the editor to keep typing
    refreshPreview()
}
```

Caveat: directly editing `Lines`/`CursorX` does not touch the widget's private
selection anchor. If a selection were active when Insert fired, that anchor would
be stale — but `Insert` is driven from the dropdown (not a keystroke in the
editor), the caret is placed inside the inserted text, and the *next* click or
keystroke re-derives/clears selection (`handleClick`/`extendOrClear`), so no
corruption is observable. This is the same public-field path the issue sanctions
("`MultiLineInput` exposes `CursorX`/`CursorY`; insertion should happen at the
caret"). A turbotui `MultiLineInput.Insert(string)` would be the clean long-term
home, but we deliberately keep all changes in gogent (see criterion 4).

### 7. Optional: dirty flag + unsaved-changes guard (P6 / criterion 7)

Recommended but isolable. Keep a `baseline CommandDef` snapshot set in
`loadForm`/`clearForm`/after a successful save. Add
`isDirty() bool { return !defsEqual(currentDef(), baseline) }`. Route **all** close
paths through one `attemptClose()`:

```go
attemptClose := func() {
    if isDirty() {
        w.showConfirm("Commands", "Discard unsaved changes?", func(ok bool) { if ok { closeFn() } })
        return
    }
    closeFn()
}
```

and wire it to all three close paths:

- **Cancel footer button** — replace `closeFn` (the last entry in the `actions`
  slice, line 393) with `attemptClose`.
- **Escape** — the dialog-root `OnTypeFn` (line 400) calls `attemptClose` instead
  of `closeFn`.
- **Title-bar ✕** — set `dialog.Window.OnClose = func(*tv.Window) { attemptClose() }`
  (replacing the line-398 reassignment). This is **vetoable**: turbotui's
  `closeButtonPressed` (`window.go:142-150`) routes the ✕ *through `OnClose` only*
  and does **not** remove the layer itself when an `OnClose` is installed
  ("a confirmation step can veto the close"). So `attemptClose` can pop the confirm
  and leave the dialog open; the layer is removed only when `closeFn` finally runs.
  Because `OnClose` is reassigned after the widgets exist, `attemptClose`'s closure
  over `isDirty`/`currentDef` is valid (it is defined near the end, alongside the
  footer wiring).

`defsEqual` compares the scalar fields plus the param slice element-wise. The
baseline is (re)set in `loadForm`/`clearForm` and after a successful Save/Delete,
so a freshly-loaded or just-saved form reads as clean. If the maintainer prefers a
smaller PR, this section can ship separately; the P1–P6 core stands alone.

---

## Criterion-by-criterion assessment

### (1) GOAL MATCH
The change does exactly what #455 asks: it converts the single-line template box
to the existing `MultiLineInput` (P1, the headline), reframes the preview to show
only the expansion (P2), gates delete behind a confirm (P3), adds Enter-advance
focus while relying on the framework's existing Tab traversal (P4), widens labels
and grows the dialog height (P5), and inserts placeholders at the caret (P6). The
optional dirty guard (criterion 7) is included but isolable. **No scope creep:**
backend, expansion, history dialog, and the param sub-editor flow
(`showCommandParamDialog`) are left as-is — only the param list's height is
touched, which P5 explicitly permits. Nothing in `internal/` or `turbotui/`
changes.

**Deferred, matching the issue's own stated priority** (P6's two "Lower
priority"/"Low priority" sub-bullets): the structured per-param "sample args"
editor is *not* built — instead the `name=value` syntax is documented in the
preview's dim hint (the issue's stated fallback: "or at least document the
`name=value` syntax"), and the single-line `argsBox` is kept. The status row stays
single-line (the issue marks multi-line status "Low priority"); long errors still
truncate as today — no regression, just not improved here. Both are called out so
they are explicitly scoped out, not overlooked.

### (2) USABILITY
- Multi-line templates become authorable through the UI for the first time
  (typing newlines via Enter, multi-line paste via `handlePaste`) — the user can
  finally drive the input the data model already supports.
- The redundant second copy of the template text is gone; the preview now
  surfaces the *expanded* prompt, with a dim hint instead of an empty/echoed box
  when there is nothing to expand — the right thing is surfaced, not silent.
- Destructive delete (which destroys all version history) now requires explicit
  confirmation — the irreversible action is no longer silent.
- Tab/Shift-Tab (framework) plus Enter-advance (added) give full keyboard drive;
  the mouse is no longer mandatory.
- Insert-at-caret behaves the way a user expects in a multi-line editor (the
  placeholder lands where they are typing, not at the far end of a long template).
- Labels read as words (`Template:` not the cryptic `Tmpl:`).

### (3) NO REGRESSIONS
- **Same widget API surface:** `MultiLineInput.GetText/SetText/Clear` match
  `TextBox`, so `currentDef`/`loadForm`/`clearForm`/save all keep working.
- **`wrapLive` generalised** to wrap `*VisualComponent` instead of `*TextBox`;
  same wrap-then-refresh semantics, so the live preview keeps refreshing per
  keystroke (and now per paste).
- **Tab is NOT double-handled:** we add no root Tab handler, so the desktop's
  existing `moveFocus` is untouched. Escape still closes (or, with the optional
  guard, prompts).
- **Expansion/back-end unchanged:** `expandTemplate`/`templateWarnings` are called
  exactly as before; multi-line templates already round-trip through them
  (`command_history_dialog.go`'s `lineDiff` is already line-oriented).
- **Tests that must be updated — exact numbers (invariants preserved):**
  - `TestCommandsDialogSpecShape`: `PrefH 28→34`, `MaxH 34→40`. Ordering
    (`MinH 26 < PrefH 34 < MaxH 40`) and the anti-balloon `MaxH < 42` (40 < 42)
    both still hold.
  - `TestCommandsDialogSize`: roomy `200×50` `28→34` (= new PrefH, since the 42-row
    share exceeds it); `120×40` `28→34`; `120×30` **stays `26`** (85%·30 ≈ 25 <
    MinH, floored — MinH unchanged); `300×80` `28→34`; tiny `40×16` stays `84×26`.
    **The guard at line 173 flips:** it currently asserts `gotH >= 34` is an error
    ("PrefH 28 bounds it under the cap"); with PrefH 34 the dialog now resolves to
    exactly 34, so this must become `gotH >= 40` (the new MaxH). The `gotH >= 42`
    balloon guard (line 165) is unaffected.
  - `TestCommandsDialogOpensContentDriven`: the two `200×50` rows `28→34`;
    `120×30`→`96×26` and `48×14`→`84×26` unchanged.
  - `TestCommandsDialogResizePathIndependent`: post-resize expectation `112×28 →
    112×34` (two assertions); the `80×24 → 84×26` open-floor is unchanged.
  - `TestCommandsDialogInnerGeometryFitsAtFloorAndRoomy`: the `labelW` constant
    `7→10`, the `row` decomposition, and the `previewH` formula change. New
    expected values:

    | metric | formula | floor `84×26` | roomy `112×34` |
    |--------|---------|---------------|----------------|
    | detailW | width−28 | 56 | 84 |
    | boxW | detailW−labelW(10) | 46 | 74 |
    | paneH | height−7 | 19 | 27 |
    | preview Y (`row`) | 2+2+6+2+1+1+4+1+1 | 20 | 20 |
    | previewH | height−24 | 2 | 10 |
    | previewBottom | row+previewH−1 | 21 | 29 |
    | hint row | height−4 | 22 | 30 |

    `previewBottom < hint row` holds in both columns, and every region clears its
    collapse guard (`detailW≥30`, `boxW≥12`, `paneH≥6`, `previewH≥2`) — at the
    floor `previewH` is exactly its 2-row minimum.
  - Footer tests (`TestCommandsDialogFooterFitsAtMinWidth`,
    `…FooterRendersFullWidth`, `…SpecFloorsAboveFooterNeed`): **no change** —
    `commandsFooterLabels`, width, and `MinW 84` are untouched.
- **Footer unchanged:** labels and `commandsFooterLabels` are untouched, so the
  footer-width tests (`TestCommandsDialogFooterFitsAtMinWidth`,
  `…FooterRendersFullWidth`) keep passing.
- **Intended behaviour change — arrow keys inside the template.** The old
  single-line `TextBox` *declined* Up/Down (`widget_textbox.go:109-145` has no
  Up/Down case → returns `false`), so those arrows fell through to the desktop's
  spatial field-navigation (`moveFocusDirection`). The new `MultiLineInput`
  *consumes* Up/Down to move the caret between rows
  (`widget_multiline_input.go:242-249`). So arrow-key navigation no longer crosses
  *through* the template field — which is the correct, expected behaviour for a
  multi-line editor, and Tab/Shift-Tab (and the new Enter-advance) still traverse
  every field. This is a deliberate consequence of P1, not a defect; called out so
  it is not mistaken for an oversight. No other field's arrow behaviour changes
  (the remaining single-line `TextBox`es still decline Up/Down).

### (4) HOLISTIC DESIGN across gogent + turbotui
The seam is respected and the change sits in the right repo. `MultiLineInput`
already provides multi-line editing, word-wrap, scrolling, selection, multi-line
paste, `SubmitMode`, and exported `CursorX`/`CursorY`/`Lines` — it is already the
main chat input (`session_window.go`) and the question dialog's textarea
(`question_dialog.go`), a proven drop-in. Tab traversal already lives in turbotui's
`Desktop`. Therefore **every change is a consumer-side change in gogent**; turbotui
is not modified. Downstream effect on turbotui: none. The only conceivable turbotui
additions — a `MultiLineInput.Insert(string)` convenience (we instead insert via
the public fields) and a shared focus-order helper (the desktop already cycles
focus) — are both unnecessary, matching the issue's "no turbotui changes required".
Reaching into `Lines`/`CursorX`/`CursorY` for insert-at-caret uses the documented
public API surface, so it does not breach encapsulation.

---

## Regression risks (call-outs)

1. **Inner-geometry test is a literal mirror** of the layout math; it WILL fail
   until updated to the §5 table. This is expected and required by the issue.
2. **`wrapLive` signature change** — every caller must pass `.Component`. Both
   call sites (`tmplInput`, `argsBox`) are updated together.
3. **Optional dirty guard must cover all three close paths** (Cancel, Escape,
   title-bar ✕) or it is half-applied; baseline must be reset after Save/Delete or
   it will false-positive. Mitigated by routing all three through `attemptClose`
   (the ✕ is interceptable because `window.go:142-150` fires `OnClose` *instead of*
   self-removing the layer). If the guard ships, the footer-test helpers and the
   "Escape closes" expectations are unaffected (Escape still closes a *clean*
   form with no extra prompt).
4. **Insert-at-caret touches public fields directly** — must clamp `CursorX` to
   the line length (done) to avoid a slice out-of-range if the caret state is
   stale.
5. **Preview FG toggling** between dim-hint and body colour must reset on every
   `refreshPreview` (both branches set `preview.FG`) so a hint colour does not
   stick to a later real expansion.
6. **Height floor at MinH 26** leaves the preview at its 2-row minimum; verified
   `previewBottom = height-5 < height-4`, so no footer collision even at the floor.

---

## Verification plan

- `go test ./ui/tui/...` after updating the pinned numbers.
- New test: a multi-line template (`"line1\nline2\n${x}"`) round-trips —
  `loadForm` then `tmplInput.GetText()` equals the input, and `expandTemplate`
  with sample args produces the expected multi-line preview text.
- New test: `deleteAction` opens a confirm dialog (top layer is a message dialog,
  not an immediate `DeleteCommand` call) and only invokes `DeleteCommand` when the
  callback receives `ok == true` (spy handler counts calls under both branches).
- Manual: paste a multi-line block into the template (newlines preserved), insert a
  `${param}` mid-line, Tab through every field, and confirm the preview shows the
  expanded prompt (and the dim hint when empty).

---

## Open questions

1. **`Description:` label width.** `Template:` (9) fits `labelW 10`, but
   `Description:` (12) does not. Plan keeps the short `Desc:` (and `Name:`) to hold
   `labelW 10`, OR we bump `labelW` to 12 and shrink `boxW` by 2 (still ≥ 12 at the
   floor). Recommended: `labelW 10`, keep `Desc:`. Confirm the maintainer's
   preference.
2. **Ship the optional dirty guard (§7) in this PR or separately?** It is the one
   piece with cross-cutting close-path risk; P1–P6 are self-contained without it.
3. **Template height fixed at 6 vs. terminal-proportional.** Plan uses a fixed 6
   rows (matches the issue's "5–6 rows"); a proportional height would complicate
   the row math and the geometry test for little gain. Recommend fixed 6.
4. **`MinH` floor.** Plan keeps `MinH 26` (preview collapses to 2 rows at the
   floor). If the maintainer prefers the template never to crowd the preview,
   raise `MinH` to ~30 — but that shrinks usability on a 24-row terminal. Recommend
   keeping 26.
