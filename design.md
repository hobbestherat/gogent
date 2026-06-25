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
| 4 | Resize reflow (size **and content layout**) | `layer.OnResize` re-resolves the rect, repositions chrome, and resizes `Tabs`; the cascade re-derives each panel's `visibleRows`/`itemW`/scrollbar/`scrollY` via `LayoutFn`; `keepFocusVisible` runs after | Yes |
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
`tv.NewComponent` panel. We keep that loop verbatim but record, for each child, a
**`scrollRow{comp, logicalY, h}`** (its build-time `Y` is the logical row, `h` its
row span — 1 for labels/checkboxes/text, 3 for a textarea). The scrollbar is **not**
a `scrollRow` (it is geometry, not content). Then add:

- **`scrollY`** — first visible logical row (starts 0).
- **`visibleRows`, `itemW`** — derived live inside `panel.LayoutFn` from the panel's
  own `Bounds` (`visibleRows = Bounds.H`, `itemW = Bounds.W - 2*margin`). Crucial
  detail: `Tabs.layoutContent` resizes each tab's content to fill the content area
  via `SetBounds`, which fires the panel's `LayoutFn` (verified `component.go:199`).
  So `panel.LayoutFn` is the single place that re-derives **all** live geometry and is
  what makes resize self-healing without the dialog reaching into panel internals
  (`Tabs` is the layer the theme editor lacks). `LayoutFn` does, in order:
  1. Guard `Bounds.H <= 0` / `Bounds.W <= 0` → keep the build-time seeds (resolves the
     §8 `visibleRows==0` risk).
  2. `visibleRows = Bounds.H`; `itemW = max(1, Bounds.W - 2*margin)`.
  3. Reposition the scrollbar child to the live `Bounds`:
     `bar.SetBounds({X: Bounds.W-1, Y: 0, W: 1, H: Bounds.H})` — **this is what keeps
     the bar from going stale on resize** (it is a panel child, repositioned every
     layout from live bounds, so a dialog-level relayout never needs to know about
     it). (Resolves critic Defect A.)
  4. `scrollY = clampScroll(scrollY)` against the new `maxScroll`.
  5. `reflow()`.
  Because `LayoutFn` runs on `SetBounds` **and** at the start of every `Draw`
  (`component.go:280`), the viewport self-heals on both resize and ordinary redraws.
- **`contentH`** — the panel's full logical height (the final `y` from the build
  loop), captured once at build. `maxScroll = max(0, contentH - visibleRows)`.
- **`reflow()`** — for every `scrollRow r`: set `r.comp.Bounds = {X: margin, Y:
  r.logicalY - scrollY, W: itemW, H: r.h}` (so a horizontal resize re-widens **and**
  re-narrows every field from the live `itemW` — resolves critic Defect B, satisfying
  criterion 4's "recompute … content layout"), and `r.comp.Visible =
  (r.logicalY + r.h > scrollY) && (r.logicalY < scrollY + visibleRows)` —
  intersection, not strict containment, so a partially-scrolled 3-row textarea stays
  visible/focusable (turbotui clips its off-window rows). Off-window children are not
  drawn, hit-tested, or focus-navigated (`collectFocusable` skips `!Visible`,
  verified `component.go:358`).
- **`scrollTo(y)`** — clamp, set `scrollY`, `reflow()`, `keepFocusVisible()`, redraw.
- **`keepFocusVisible()`** — if the focused child was just hidden, move focus to the
  first still-visible focusable in the panel (mirrors `theme_editor.go:816–844`).
  Prevents key-routing dead-ends (`desktop` drops keys to `!visibleInTree` widgets).
  Exposed on the handle so the dialog's resize path can call it too (see §5).
- **`ensureVisible(target *tv.VisualComponent)`** — scroll the minimum amount to bring
  a focusable into the window. The panel builds a `focusRows map[*tv.VisualComponent]
  struct{y, h int}` during the loop (keyed by each focusable's `Root()`, value its
  logical top row + span — this is the component→logical-row source the critic flagged
  as missing, resolving Defect D). `ensureVisible` looks `target` up, then: `if y <
  scrollY → scrollTo(y)`; `if y+h > scrollY+visibleRows → scrollTo(y + h -
  visibleRows)`. Used by required validation (criterion 3) and by Enter-advance-focus
  so advancing onto a below-the-fold field reveals it before focus lands there.
- **Scrollbar** — a 1-column panel child whose `X`/`H` track the live bounds via
  `LayoutFn` (step 3). Items end at `margin + itemW - 1 = Bounds.W-3`, leaving column
  `Bounds.W-2` as a gap and the bar alone at `Bounds.W-1`, so no field underlaps it.
  Its `DrawFn` draws nothing when `maxScroll == 0` and otherwise calls the shared
  vertical-scrollbar helper with `(contentH, visibleRows, scrollY)`.
- **Mouse wheel** — `panel.OnScrollFn = scrollTo(scrollY - event.Delta)` (same delta
  convention as TextView/theme editor); always works, including inside a textarea.

### Keyboard scroll routing (dialog level)

The dialog's existing `dialog.Root().OnTypeFn` (Escape + Ctrl+Enter) gains, in the
**bubble** path (only keys the focused child declined reach it):

- `PageUp`/`PageDown` → active panel `scrollTo(scrollY ∓ visibleRows)`.
- `Up`/`Down` → active panel `scrollTo(scrollY ∓ 1)`.

Verified key-consumption against the pinned turbotui:
- `TextBox` handles only Left/Right (`widget_textbox.go:146-148`) → **all of
  Up/Down/PageUp/PageDown bubble** ✔.
- `MultiLineInput` **always** consumes Up/Down (`widget_multiline_input.go:242-249`
  return `true` even when `moveUp` no-ops at the top edge) but declines PageUp/PageDown
  (`:259`) → from a focused textarea, **only PageUp/PageDown bubble**; Up/Down never
  scroll the panel while a textarea holds focus. This asymmetry is stated plainly
  rather than glossed: the guaranteed, focus-independent scroll affordances are the
  **mouse wheel** and **PageUp/PageDown** (both work from every field, including a
  textarea); Up/Down are an extra convenience that works only when the focused field
  declines them (TextBox, checkbox). There is no edge-triggered auto-scroll out of a
  textarea, so the surfaced hint (§6) advertises PageUp/PageDown + wheel explicitly so
  a textarea-focused user is never left guessing how to scroll.

We route the bubbled keys to `tabs.Active()`'s panel via the per-tab handle slice.
This matches the theme-editor gate: only scroll when the active panel's `maxScroll >
0`, otherwise return `false` and let the desktop's focus navigation keep the arrows
(so a short, non-overflowing form behaves exactly as today).

### Panel handle (the new return shape)

`buildTopicPanel` returns a `topicPanel` struct (replacing the bare `tv.Widget`):

```go
type topicPanel struct {
    widget           tv.Widget          // added to tabs.AddTab
    fields           []questionField
    firstFocus       *tv.VisualComponent
    scrollBy         func(rows int)     // PageUp/Down (±visibleRows) and arrows (±1)
    canScroll        func() bool        // maxScroll > 0 — the routing gate
    ensureVisible    func(c *tv.VisualComponent)
    keepFocusVisible func()             // called by the dialog's resize path (§5)
}
```

`showQuestionDialog` keeps `panels []topicPanel` indexed by tab so the key router, the
validation path, and the resize path can address the active/offending tab. `fields`
is still flattened into the dialog-level `fields` slice for validation; each
`questionField` already carries `tabIndex`, so `submit` maps an offending field →
`panels[f.tabIndex].ensureVisible(f.focus)` → `desktop.SetFocus(f.focus)` (scroll
first so the focused field is never invisible). `ensureVisible` resolves `f.focus`
through the panel's `focusRows` map, so no component→row lookup is left undefined.

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
    // recompute errorY/btnY/tabsH from nh; reposition summary/indicator, errLabel,
    // and Cancel/Prev/Next/Submit; resize the Tabs widget.
    tabs.Root().SetBounds(Rect{1, tabsY, nw-2, newTabsH})  // cascades to panels →
                                                           // layoutContent → panel
                                                           // LayoutFn (bar + itemW +
                                                           // visibleRows + reflow)
    panels[tabs.Active()].keepFocusVisible()               // active tab may have
                                                           // hidden the focused field
    desktop.Redraw()
}
```

Because resizing `Tabs` cascades through `layoutContent → SetBounds → panel.LayoutFn`,
each per-panel viewport recomputes `visibleRows`, re-derives `itemW` (so fields
re-widen/re-narrow horizontally — criterion 4's "content layout"), repositions its own
scrollbar from live bounds (no stale bar), re-clamps `scrollY`, and re-`reflow`s — all
for free, so `relayout` **never reaches into panel internals**. The one thing the
cascade cannot decide is focus: a shrink can hide the focused field, so `relayout`
calls `panels[active].keepFocusVisible()` after the cascade (mirrors
`theme_editor.go:991`; only the active tab can hold focus, so only it is checked).
(Resolves critic Defects A/B/C.)

For the button row, the question buttons are left-packed (Cancel, optional Prev/Next)
with a right-anchored Submit; `relayout` re-runs the same placement math (`bx`
accumulation + `width-3-submitW+1`) against the new width, reusing `clampDialogRect`
as the narrow-dialog safety net so Cancel/Prev/Next/Submit never overlap or cross the
border on a small terminal (a latent improvement, since today they are pinned).

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
  the key hints. The hint text reads e.g. **"Ctrl+Enter submit · PgUp/PgDn or wheel
  scroll · Tab next"** — it advertises PageUp/PageDown + wheel explicitly because (per
  §2) those are the only scroll affordances that work from a focused textarea. This
  keeps the existing `OnTabChange` error-clearing behaviour (we extend the same
  callback).
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
mouse). Every field is reachable: by wheel + PageUp/PageDown **from any field
including a textarea**, by Up/Down when the focused field declines them, and by
validation/Enter-advance auto-scroll bringing the target into view. The textarea
Up/Down asymmetry is acknowledged plainly (§2) and mitigated by surfacing PgUp/PgDn +
wheel in the always-visible hint, so a textarea-focused user is never stuck. A
scrollbar signals overflow; the Topic n/N indicator tells the user where they are.
Nothing overflows silently. The user drives all input; auto-scroll only ever *reveals*
a field, never changes a value.

**(3) NO REGRESSIONS.** The answer-collection contract (`questionField.answer`,
required validation, `finish`/`onResult`, Cancelled-on-escape/shutdown) is untouched.
Short forms that fit get `maxScroll == 0` → no scrollbar, scroll keys decline and fall
through to focus nav → behaviour identical to today. The button shift aligns with the
4 other modals' tested layout. The resize path is fully specified (§5): the bar
re-tracks live bounds, fields re-width, focus is kept visible — the four resize defects
the critic raised (stale bar / no horizontal reflow / missing `keepFocusVisible` /
undefined `ensureVisible` row lookup) are each resolved in §4–§5. The existing
`question_dialog_issue406_test.go` tests call only `showQuestionDialog` (never
`buildTopicPanel`), so the `topicPanel` return-type change is internal and safe; the
`issue406` render tests guard the `maxScroll==0` byte-identical path (the §8
`visibleRows==0` risk is the one to watch). The shared-scrollbar rename is
behaviour-preserving and keeps the theme editor identical (its scroll-math tests guard
it). The dev gate (build/vet/gofmt/lint/test, no `-race` on Pi5) runs before hand-off.

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

- **`visibleRows`/`itemW` read too early / zero.** If `LayoutFn` runs before the panel
  is first sized, they could be 0 and hide/clip everything. Mitigation: seed
  `visibleRows` from the `tabsH-1` argument and `itemW` from the build-time width, and
  in `LayoutFn` guard `Bounds.H<=0`/`Bounds.W<=0` → keep the seeds. The `issue406`
  render tests guard this byte-identical path.
- **Stale scrollbar on resize (critic Defect A) — RESOLVED.** The bar is a panel child
  repositioned from live `Bounds` in `LayoutFn` step 3, so the `Tabs` resize cascade
  re-places it with no dialog-level plumbing.
- **No horizontal field reflow (critic Defect B) — RESOLVED.** `reflow` sets each
  row's `X`/`W` from the live `itemW`, not just `Y`/`Visible`, so fields re-width on a
  horizontal resize (criterion 4's "content layout").
- **Focus stranded on a hidden field.** Covered by `keepFocusVisible()` after every
  scroll, after `relayout`'s cascade (critic Defect C — RESOLVED), and by
  `ensureVisible()` before every programmatic `SetFocus` — the guard the theme editor
  relies on.
- **`ensureVisible` row lookup (critic Defect D) — RESOLVED.** The panel's `focusRows`
  map (`*VisualComponent → {logicalY, h}`, built in the item loop) is the
  component→row source; `ensureVisible` scrolls the minimum to fit `[y, y+h)`.
- **Enter-advance-focus onto a below-fold field.** The existing `OnSubmit` wiring
  (`textBoxes`) jumps focus by index; wrap it to `ensureVisible(next)` first so the
  panel scrolls before the focus lands, else the new focus would be invisible.
- **Textarea Up/Down never scroll (critic minor) — ACCEPTED + MITIGATED.** Up/Down are
  consumed by `MultiLineInput`; scroll from a textarea is via PgUp/PgDn + wheel only.
  Stated in §2 and advertised in the §6 hint so it is discoverable, not silent.
- **Scroll keys swallowing focus navigation on short forms.** Gate all scroll routing
  on the active panel's `maxScroll > 0` (`canScroll`), returning `false` otherwise —
  identical to the theme editor's gate — so a one-screen form is byte-for-byte
  unchanged.
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
