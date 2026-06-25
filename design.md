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

`refreshPreview()` (line 164) is reframed so it never echoes the template verbatim:

```go
def := currentDef()
args := strings.Fields(argsBox.GetText())
if len(args) == 0 && !templateHasRefs(def.Template) {
    preview.FG = colorDialogDetail            // dim hint, not body colour
    preview.SetText("enter sample args to preview the expanded prompt")
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
(`name → desc → model → agent`) gets an `OnSubmit` that
`w.desktop.SetFocus(next)`. The template `MultiLineInput` keeps `OnSubmit == nil`
so Enter inserts a newline. Suggested Enter chain:
`name→desc→template`, `model→agent→model` (a short wrap among the remaining
single-liners), with Tab as the universal traversal. Build the focus list after
all widgets exist (closure over the slice), exactly as `question_dialog.go` does.

The dialog-root `OnTypeFn` keeps **only** its existing Escape handler (line 399).

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

and wire it to: the Cancel footer button, the dialog-root Escape handler, and
`dialog.Window.OnClose` (the title-bar ✕). `defsEqual` compares the scalar fields
plus the param slice element-wise. Risk: every close path must go through
`attemptClose`, or the guard is half-applied — called out as a regression risk
below. If the maintainer prefers a smaller PR, this section can ship separately;
P1–P6 core stand alone.

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
- **Tests that must be updated (numbers, not invariants):**
  `TestCommandsDialogSpecShape` (PrefH 28→34, MaxH 34→40),
  `TestCommandsDialogSize` / `TestCommandsDialogOpensContentDriven` /
  `TestCommandsDialogResizePathIndependent` (roomy height 28→34; 120×40 height
  28→34; the 120×30 floor stays 26 since MinH is unchanged),
  `TestCommandsDialogInnerGeometryFitsAtFloorAndRoomy` (the field row math and
  `previewH` formula change to the table in §5). The structural invariants the
  tests encode — floors < preferred < caps, `MaxH < 42`, footer fits at MinW,
  preview bottom above the hint row — are all preserved by construction.
- **Footer unchanged:** labels and `commandsFooterLabels` are untouched, so the
  footer-width tests (`TestCommandsDialogFooterFitsAtMinWidth`,
  `…FooterRendersFullWidth`) keep passing.

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
   it will false-positive. Mitigated by routing all closes through `attemptClose`.
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
