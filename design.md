# Design — gogent #459: make the `ask_user` question dialog usable

**Branch:** `pair1/gogent-issue-459-fix-authored-by-maintai`
**Scope decision (fixed up front):** GOGENT-ONLY. No turbotui dependency bump. The
primary button-clip bug is gogent-side. Scrolling reuses the hand-rolled viewport
pattern already proven in `ui/tui/theme_editor.go` (the issue explicitly permits
this fallback; a turbotui `Viewport` widget is "recommended, not required"). Tab
overflow is handled gogent-side. All edits land in `ui/tui/question_dialog.go`.

---

## 1. What the issue actually asks (and what we will do)

This is a **FIX**, not a feature or a refactor. Five acceptance criteria:

| # | Criterion | Our change | Blocking? |
|---|-----------|-----------|-----------|
| 1 | Buttons render & are usable | Shift bottom chrome up one row (`btnY = height-3`) | **Yes (primary)** |
| 2 | Scrollable topic content + scrollbar | Port the `theme_editor.go` scroll viewport into each topic panel | Yes |
| 3 | Required-field reachability under scroll | `ensureVisible(field)` before focusing the offending field in `submit` | Yes |
| 4 | Resize reflow | Add `layer.OnResize` that re-resolves the rect and re-lays the interior | Yes |
| 5 | Tab-label overflow reachability | Keep Prev/Next + Alt-arrows (already wrap **all** tabs) as the guaranteed reachability path; add an always-visible "Topic n/N" indicator; elide long tab titles | Yes (reachability), visual scroll deferred to turbotui |

No other behaviour is touched. The tool semantics, parsing, the neutral bridge
(`internal/agent/question.go`), `serializePrompt`/`presentBackgroundModal`/badge
machinery, and the answer-collection contract are all unchanged — confirmed out of
scope by the issue.

---

## 2. Files & functions touched

### gogent (the only repo we change)

- **`ui/tui/question_dialog.go`** — all changes:
  - `showQuestionDialog` (sizing block ~L124–130): the **primary** off-by-one fix,
    plus wiring the scroll-key routing, the resize hook, and the topic indicator.
  - `buildTopicPanel` (L265–404): becomes a self-contained scroll viewport (the
    bulk of the new code). Returns a richer handle (see §4) instead of a bare
    `tv.Widget` so the dialog can drive per-tab scroll/ensure-visible/relayout.
  - `submit` closure (L168–196): call `ensureVisible` on the offending field's
    panel before `desktop.SetFocus`.
  - One new small helper for tab-title elision (or extend `topicTabTitle`).
  - Likely one new `drawQuestionScrollbar` thin wrapper, **or** reuse the existing
    `drawThemeEditorScrollbar` by promoting it to a shared, dialog-neutral name.
    (Decision in §4; leaning toward a small shared `drawDialogVScrollbar`.)

### turbotui

**No changes.** A read-only clone is at `$HOME/work/turbotui`; it stays pinned at
`v0.3.1-0.20260624105607-52604ee61507`. We rely only on already-public API that the
theme editor already depends on: `VisualComponent.Bounds`/`SetBounds`/`Visible`,
`LayoutFn`, `OnScrollFn`, `AddChild`, and the `Tabs` focus contract. No new public
surface is required, so there is nothing to upstream and no seam to renegotiate.

---

## 3. Primary fix (blocking, lands first) — render the buttons

`turbotv/window.go`'s `layout()` insets content by 1; for a dialog of height `H` the
last *visible* content-relative row is `H-3` (row `H-1` is the bottom border, drawn
over by `turbotv/component.go`'s draw-time clip). Today:

```go
errorY := height - 3
btnY   := height - 2   // lands on the bottom border → clipped → invisible
tabsH  := errorY - tabsY
```

becomes:

```go
errorY := height - 4   // one row above the buttons
btnY   := height - 3   // last visible interior row (matches every other modal)
tabsH  := errorY - tabsY
```

This matches `review_dialog.go`, `permission_dialog.go`, `disconnect_modal.go`,
`message_dialog.go`. `tabsH` shrinks by exactly one row (the error row moved up into
what was the buttons' phantom row); the `tabsH < 3` floor already guards a tiny
dialog. This alone makes Cancel/Prev/Next/Submit visible and clickable, and is the
single change that makes the dialog usable. It is independent of the scroll work and
can be reviewed/committed on its own.

**Key-hint note (from the issue):** plain Enter in a text field only advances focus
within the tab; the user reaches Submit via Tab/Shift+Tab then Enter/Space, or
Ctrl+Enter from anywhere. Once buttons are visible this is worth a one-line hint; we
will surface it in the dialog (see §5 indicator row) rather than leave it implicit.

---

## 4. Scrolling — port the `theme_editor.go` viewport into each topic panel

### Why this pattern (not the `TextView` house pattern)

The house scroll pattern (`permission_dialog.go` etc.) wraps **text** in a read-only
`TextView`. The question dialog hosts **interactive** widgets (TextBox /
MultiLineInput / Checkbox) that must take focus and input, so `TextView` cannot be
reused. `theme_editor.go` already solves exactly this — a hand-rolled viewport
hosting focusable TextBox + ColorPicker children — and is the issue's sanctioned
fallback. We mirror it, scoped to one topic panel.

### Mechanism (per topic panel)

`buildTopicPanel` already stacks items at increasing logical `Y` into a
`tv.NewComponent` panel. We keep that loop verbatim but treat each child's build-time
`Y` as its **logical** row, then add:

- **`scrollY`** — first visible logical row (starts 0).
- **`visibleRows`** — read live from the panel's own `Bounds.H`. Crucial detail:
  `Tabs.layoutContent` resizes each tab's content to fill the content area
  (`tabsH-1`) via `SetBounds`, which fires the panel's `LayoutFn`. So we set
  `panel.LayoutFn` to `(1)` read `visibleRows = c.Bounds.H`, `(2)` clamp `scrollY`
  to the new `maxScroll`, `(3)` `reflow()`. Because `LayoutFn` also runs at the start
  of every `Draw`, the viewport self-heals on **both** resize and ordinary redraws
  with no dialog-level plumbing into the panel internals. This is strictly cleaner
  than the theme editor's explicit `relayout`, which it needs only because it is not
  inside a `Tabs`.
- **`contentH`** — the panel's full logical height (the final `y` from the build
  loop). `maxScroll = max(0, contentH - visibleRows)`.
- **`reflow()`** — for each child: `Bounds.Y = logicalY - scrollY`; `Visible =
  logicalY >= scrollY && logicalY < scrollY+visibleRows`. Identical to
  `theme_editor.go:808–815`. Off-window children are not drawn, hit-tested, or
  focus-navigated (`collectFocusable` skips `!Visible`, verified).
- **`scrollTo(y)`** — clamp, set, reflow, `keepFocusVisible()`, redraw.
- **`keepFocusVisible()`** — if the focused child was just hidden, move focus to the
  first still-visible focusable in the panel (mirrors `theme_editor.go:816–844`).
  Prevents key-routing dead-ends (`desktop` drops keys to `!visibleInTree` widgets).
- **`ensureVisible(target)`** — scroll the minimum amount to bring a child's logical
  row inside the window (`if logical < scrollY → scrollY = logical`; `if logical >=
  scrollY+visibleRows → scrollY = logical - visibleRows + 1`). Used by required
  validation (§criterion 3) and by Enter-advance-focus so advancing onto a
  below-the-fold field reveals it.
- **Scrollbar** — a 1-column child at panel-relative `x = width-1` (items already end
  at `width-2`; `itemW` stays `width - 2*margin`, leaving the last column free) with
  a `DrawFn` that draws nothing when `maxScroll == 0` and otherwise calls the shared
  vertical-scrollbar helper. We reserve `itemW`'s right edge so no field underlaps the
  bar.
- **Mouse wheel** — `panel.OnScrollFn = scrollTo(scrollY - event.Delta)` (same delta
  convention as TextView/theme editor).

### Keyboard scroll routing (dialog level)

The dialog's existing `dialog.Root().OnTypeFn` (Escape + Ctrl+Enter) gains, in the
**bubble** path (only keys the focused child declined reach it):

- `PageUp`/`PageDown` → active panel `scrollTo(scrollY ∓ visibleRows)`.
- `Up`/`Down` → active panel `scrollTo(scrollY ∓ 1)`.

Verified key-consumption against the pinned turbotui:
- `TextBox` handles only Left/Right → **Up/Down/PageUp/PageDown bubble** ✔.
- `MultiLineInput` consumes Up/Down (caret move) but **not** PageUp/PageDown → at
  least PageUp/PageDown always bubble out of a textarea ✔, and the mouse wheel always
  works.

So every field is reachable by **wheel** and **PageUp/PageDown** unconditionally, and
by **Up/Down** except inside a textarea (where they correctly move the caret). We
route to `tabs.Active()`'s panel via the per-tab handle slice. This matches the
theme-editor gate: only scroll when `maxScroll > 0`, otherwise return `false` and let
the desktop's focus navigation keep the arrows (so a short form behaves exactly as
today).

### Panel handle (the new return shape)

`buildTopicPanel` returns a `topicPanel` struct (replacing the bare `tv.Widget`):

```go
type topicPanel struct {
    widget      tv.Widget               // added to tabs.AddTab
    fields      []questionField
    firstFocus  *tv.VisualComponent
    scrollTo    func(y int)
    pageBy      func(rows int)          // visibleRows-relative
    ensureVisible func(c *tv.VisualComponent)
}
```

`showQuestionDialog` keeps `panels []topicPanel` indexed by tab so the key router and
the validation path can address the active/offending tab. `fields` is still flattened
into the dialog-level `fields` slice for validation; each `questionField` already
carries `tabIndex`, so `submit` maps an offending field → `panels[f.tabIndex]` →
`ensureVisible(f.focus)` → `desktop.SetFocus(f.focus)`.

### Shared scrollbar helper

`theme_editor.go` has `drawThemeEditorScrollbar` (a near-copy of turbotui's unexported
`drawVScrollbar`). Rather than copy it a third time, promote it to a dialog-neutral
`drawDialogVScrollbar(surface, track, total, visible, offset, fg, bg)` in a shared ui
file and have the theme editor call it too. This is a tiny, behaviour-preserving
rename (one moved function, one call-site update) that avoids a duplicated copy — and
keeps the scroll affordance visually identical across both dialogs. If we want to
keep the diff minimal/low-risk for the first landing, the alternative is a local copy
in `question_dialog.go`; the rename is preferred but flagged as optional.

---

## 5. Resize reflow (criterion 4)

The question dialog's interior is absolutely positioned (tabs, error label, buttons
by `width`/`height`), so `installResizeReflow`/`dialog.Fit` alone — which only
re-sizes the outer frame — is insufficient, exactly as it was for the theme editor.
We add a `relayout` closure modelled on `theme_editor.go:974`:

```go
layer.OnResize = func(tv.Rect) {
    nx, ny, nw, nh := tv.ResolveDialogRect(spec, app.Width(), app.Height())
    dialog.Window.Component.SetBounds(Rect{nx, ny, nw, nh})
    // recompute errorY/btnY/tabsH from nh; reposition summary, tabs, errLabel,
    // Cancel/Prev/Next/Submit; resize the Tabs widget.
    tabs.Root().SetBounds(Rect{1, tabsY, nw-2, newTabsH})  // cascades to panels →
                                                           // layoutContent → panel
                                                           // LayoutFn → reflow
    desktop.Redraw()
}
```

Because resizing `Tabs` cascades through `layoutContent → SetBounds → panel.LayoutFn
→ reflow`, the per-panel viewports recompute `visibleRows` and re-clamp `scrollY`
for free — we do **not** reach into panel internals from `relayout`. The button row
re-flows with `reviewButtonRow`-style clamping is not needed here because the question
buttons are left-packed with a right-anchored Submit; we re-run the same placement
math (`bx` accumulation + `width-3-submitW+1`) against the new width, reusing
`clampDialogRect` as the narrow-dialog safety net so Cancel/Prev/Next/Submit never
overlap or cross the border on a small terminal (a latent improvement, since today
they are pinned).

The `spec` (`{MinW:50, MaxW:110, MinH:14}`) is pure Min/Max floors — path-independent
— so re-resolving on each resize matches a fresh open at the new size.

---

## 6. Tab-label overflow (criterion 5) — gogent-only

turbotui's `Tabs.labelSpans()` lays labels left→right and `break`s at the strip's
right edge, dropping overflow labels (no horizontal scroll). We cannot change its
rendering from gogent without a turbotui bump, which is out of scope. But:

- **Reachability is already guaranteed** by the existing Prev/Next buttons and
  Alt+Left/Right, both of which `switchBy`/`SetActive` cycle **all** tabs with
  wraparound regardless of which labels the strip painted. So no topic is unreachable
  today via keyboard/buttons even with a dropped label — the defect is that the user
  can't *see* which/where they are. We make that explicit:
- **Add an always-visible "Topic n/N — Title" indicator** (a 1-row label updated in
  `tabs.OnTabChange`, placed on the summary row or just under it). This tells the user
  the current topic and total even when the strip clips, and is where we also surface
  the "Tab/Ctrl+Enter to submit" hint. This keeps the existing `OnTabChange` error-
  clearing behaviour (we extend the same callback).
- **Elide long tab titles** (`topicTabTitle`) to a per-tab budget so the common
  multi-topic case fits the strip; combined with the indicator and Prev/Next, the
  overflow case stays fully usable.

We explicitly **defer** true horizontal label scrolling to a future turbotui
`Tabs` enhancement (the issue's "recommended" turbotui work), and say so in a code
comment so the seam is documented rather than silently worked around.

---

## 7. Criteria self-assessment

**(1) GOAL MATCH.** Every change maps to a stated acceptance criterion; nothing
beyond them. It is a fix: the primary defect is a one-row shift; scrolling/resize/tab
handling reuse existing in-repo patterns rather than inventing new abstractions. No
schema, parsing, or bridge change. No turbotui change — honouring the chosen
gogent-only scope.

**(2) USABILITY.** Buttons are visible and reachable (Tab/Enter/Space, Ctrl+Enter,
mouse). Every field is reachable: by wheel + PageUp/PageDown unconditionally, Up/Down
outside textareas, and validation/Enter-advance auto-scroll the target into view. A
scrollbar signals overflow; an always-visible Topic n/N indicator + submit hint tells
the user where they are and how to submit. Nothing overflows silently. The user
drives all input; auto-scroll only ever *reveals* a field, never changes a value.

**(3) NO REGRESSIONS.** The answer-collection contract (`questionField.answer`,
required validation, `finish`/`onResult`, Cancelled-on-escape/shutdown) is untouched.
Short forms that fit get `maxScroll == 0` → no scrollbar, scroll keys decline and fall
through to focus nav → behaviour identical to today. The button shift aligns with the
4 other modals' tested layout. The shared-scrollbar rename is behaviour-preserving and
keeps the theme editor identical. Risk items called out in §8. Existing tests:
`question_dialog`-specific tests (if any) plus the theme-editor scroll-math tests must
stay green; the dev gate (build/vet/gofmt/lint/test, no `-race` on Pi5) runs before
hand-off.

**(4) HOLISTIC across both repos.** The change is in the right place: the button bug,
the scroll viewport, resize, and tab handling are all *presentation* concerns owned by
gogent's `ui/tui`. The turbotui seam is respected — we add no public API, bump no
version, and lean only on the same `VisualComponent` primitives the theme editor
already proves are sufficient. Downstream effect on turbotui: none now; we document
the two genuinely-turbotui-shaped follow-ups (generic `Viewport` widget; `Tabs`
horizontal label scroll) as deferred, so the cross-repo backlog is explicit rather
than absorbed as a gogent hack pretending to be complete.

---

## 8. Regression risks & mitigations

- **`visibleRows` read too early / zero.** If `LayoutFn` runs before the panel is
  first sized, `visibleRows` could be 0 and hide everything. Mitigation: seed
  `visibleRows` from the `tabsH-1` argument at build time (currently the ignored 4th
  param) and only overwrite from `Bounds.H` when `Bounds.H > 0`.
- **Focus stranded on a hidden field.** Covered by `keepFocusVisible()` after every
  scroll and `ensureVisible()` before every programmatic `SetFocus` — the same guard
  the theme editor relies on.
- **Enter-advance-focus onto a below-fold field.** The existing `OnSubmit` wiring
  (`textBoxes`) jumps focus by index; wrap it to `ensureVisible(next)` first so the
  panel scrolls before the focus lands, else the new focus would be invisible.
- **Scroll keys swallowing focus navigation on short forms.** Gate all scroll routing
  on `maxScroll > 0` (per active panel), returning `false` otherwise — identical to
  the theme editor's gate — so a one-screen form is byte-for-byte unchanged.
- **Button row on a narrow/short dialog.** Reuse `clampDialogRect` for the left-packed
  buttons in both initial layout and `relayout`; the `tabsH >= 3` floor already
  guards the vertical squeeze.
- **Shared-scrollbar rename.** Touches the theme editor. Keep it a pure move+rename
  with no logic change; the theme-editor scroll-math tests guard it. If review wants
  zero theme-editor churn for this issue, fall back to a private copy in
  `question_dialog.go` (noted as the conservative option).

---

## 9. Open questions

1. **Shared scrollbar vs. local copy.** Preference is to promote
   `drawThemeEditorScrollbar` → `drawDialogVScrollbar` and have both dialogs use it
   (no third copy). Acceptable to instead keep a local copy in `question_dialog.go`
   if we want the #459 diff to leave `theme_editor.go` untouched. Which does the
   maintainer prefer?
2. **Topic indicator placement.** Put the "Topic n/N — Title (Ctrl+Enter to submit)"
   line on the existing summary row (consuming it when a summary is present) or add a
   dedicated row above the tabs (costs one content row)? Leaning: dedicated row only
   when there is >1 topic *or* a clipped strip; otherwise fold into the summary area.
3. **Tab-title elision budget.** Fixed cap (e.g. 16 cols) vs. strip-width-derived
   share? Leaning fixed-cap for predictability; revisit if it reads poorly with 2–3
   short topics.
4. **Per-tab vs. shared scroll position.** Each topic panel owns its own `scrollY`
   (independent scroll per tab). Assumed desirable; flagging in case a single shared
   offset is wanted.

None of these block the **primary** button fix, which is self-contained and lands
first.
