# Design — Issue #462: Theme editor clearer colour/line association + save custom themes as copies

Branch: `pair2/gogent-issue-462-fix-ux-authored-by-main`
Scope: **Gogent only.** No turbotui change required (confirmed below).

---

## 0. Summary of the two changes

1. **Goal 1 (UX/spacing).** Reorder each role row so the colour swatch *leads* the
   line (`<swatch> <label:> <field>`), making the colour the visual anchor, and add a
   blank separator row between sections so groups are distinct.
2. **Goal 2 (feature).** Add named, persisted custom themes. A "Save As…" action
   copies the current editor state into a `NamedTheme`, leaving the source untouched.
   Saved themes appear in the preset dropdown (prefixed `★`), are editable in place,
   persist to `config.json`, and never mutate the read-only built-ins. The **active**
   theme records *which* saved theme it is a copy of (`ThemeConfig.SavedName`), so on
   reopen the editor re-selects that `★` entry and a later Save writes back to it —
   not to the parent built-in.

Both reuse the existing override/resolve/apply pipeline — no new colour machinery.

---

## Goal 1 — Clearer colour/line association & spacing

### Files / functions touched
- `ui/tui/theme_editor.go`
  - `cellRect` (closure in `showThemeEditor`, ≈ line 679): reorder cells to
    `swatch → label → field`.
  - `themeEditorColumnRows` (≈ line 326): count one separator row between adjacent
    sections in a column.
  - The row-building loop in `showThemeEditor` (≈ line 707): advance `logical` by one
    extra row after each group except the last in its column (the separator).
  - `checkThemeEditorLayout` (≈ line 368) / `resolveThemeEditorLayout` (≈ line 267):
    **no geometry math change required** — see below — only comment updates.
- `ui/tui/theme_issue267_test.go` and `ui/tui/theme_editor_test.go`: adjust row-count
  expectations and add a swatch-left placement assertion.

### Why the geometry barely moves
The per-row total width is `labelW + fieldW + swatchW + 2` (two single-column gaps),
**independent of the internal cell order**. So:
- `resolveThemeEditorLayout`'s `rightX` (left-column total + 1 gap) is unchanged.
- `checkThemeEditorLayout`'s `swatchEnd = col.x + labelW + fieldW + swatchW + 2 - 1`
  collision invariant is unchanged.
- `themeSectionHeader` still spans `cellRect(col, rowHeader)` = the same full width.

New `cellRect` mapping (gaps stay at 1 column each):
```
rowSwatch: x = col.x,                              w = swatchW
rowLabel:  x = col.x + swatchW + 1,                w = labelW
rowField:  x = col.x + swatchW + 1 + labelW + 1,   w = fieldW
rowHeader: x = col.x,                              w = swatchW + 1 + labelW + 1 + fieldW
```
The label-fit invariant in `checkThemeEditorLayout` (`len(label)+1 <= col.labelW`) is
untouched because `labelW` is unchanged.

### Separator rows (1b)
- **Between sections only**, *not* between roles within a section. Rationale: the
  swatch-leading reorder already makes every individual line visually distinct
  (each starts with a coloured block), so the "roles no longer touching" criterion is
  met by 1a; adding *intra-section* blank rows would break the existing contiguity
  invariant (`TestIssue267RolesGroupedContiguouslyInOneColumn`, which asserts a group's
  roles occupy `header+1, +2, …` with no gaps) and force a larger test rewrite for no
  extra clarity. Section separators alone give the requested breathing room.
- Implementation: in the build loop, after finishing a group's roles, if it is not the
  last group in that column, `logical++` (a logical row with **no component** — the
  viewport simply renders blank there). `reflow()` already tolerates arbitrary
  `logical` values, so nothing else changes.
- `themeEditorColumnRows(col)` must mirror this:
  `rows = Σ(1 + len(g.roles)) + (len(col.groups) - 1)`.
  This keeps `themeEditorContentRows()` / `maxScroll()` / the scrollbar thumb correct.
- `checkThemeEditorLayout`'s `placed == len(themeRoles)` count is unaffected
  (separators are not roles), and the scroll-reveal invariant
  (`contentRows - maxScroll <= visibleRows`) holds by construction of `maxScroll`.

Net effect at the 80×22 floor: content is a few rows taller, so it scrolls a little
more — explicitly allowed by the issue (the viewport already tolerates extra rows).

### User-facing behaviour (Goal 1)
Each role line reads left-to-right as: a live colour swatch (`▉▉ Aa`), then the role
label, then the editable spec field. Sections are separated by a blank row. The swatch
is still the interactive colour-picker (click / Enter / Space) and still tracks the
field live; only its on-screen position moved.

---

## Goal 2 — Copy a theme and modify it, keeping the original

### Data model (`internal/config/config.go`)
Add (after `ThemeConfig`, and a field on `Config` beside `Theme`):
```go
// NamedTheme is a user-saved theme: a display name plus the theme config it stores.
type NamedTheme struct {
    Name  string      `json:"name"`
    Theme ThemeConfig `json:"theme"`
}
```
On `Config` (beside `Theme ThemeConfig`, ≈ line 557):
```go
SavedThemes []NamedTheme `json:"saved_themes,omitempty"`
```
`omitempty` ⇒ an old `config.json` with no key loads to a nil slice = no behaviour
change. A saved theme's `Theme.Name` points at its parent built-in (so
`paletteByName`/`ResolveTheme` still resolve a base palette); its `Overrides` carry the
customisations. **Built-ins stay hardcoded and read-only.**

**Active↔saved back-link (fixes the cross-reopen gap).** Add one field to
`ThemeConfig` itself:
```go
// SavedName, when non-empty, records that this (active) theme is the user's saved
// theme of that name (issue #462). It lets the editor re-select the matching ★ entry
// on reopen so a later Save writes back to that saved entry rather than to the parent
// built-in. It is empty for a plain built-in selection. It is metadata only —
// ResolveTheme/applyOverrides ignore it — and is NOT stored inside a NamedTheme.Theme
// (a saved theme does not point at itself; the entry's own Name is the identity).
SavedName string `json:"saved_name,omitempty"`
```
**Why on `ThemeConfig`, not a new `Config` field:** `SetTheme(ThemeConfig)` already
persists the active `ThemeConfig` wholesale via `SaveConfig()`, so the pointer rides
along for free — no new handler, no `SetTheme` signature change, no extra wiring across
`tui.go`/`attach.go`/`embedded_handlers.go`. `buildThemeConfig` does **not** set it
(it stays `""`, so the existing `reflect.DeepEqual` tests are unaffected); only the
editor sets it when constructing the *active* cfg it hands to `SetTheme` (see Save /
Save As below). The configs stored in `SavedThemes` are built **without** `SavedName`.

### Persistence (`internal/gogent/gogent.go`)
Add, mirroring `Theme()`/`SetTheme()` (≈ lines 3119–3140), under the same `g.mu` lock
and `SaveConfig()` flush:
```go
func (g *Gogent) SavedThemes() []config.NamedTheme        // returns a copy
func (g *Gogent) SetSavedThemes(t []config.NamedTheme)    // replaces + SaveConfig()
```
`SavedThemes()` returns a defensive copy (the slice/maps are mutable) so callers can't
alias the live config.

### Handlers (`ui/tui/tui.go`, `cmd/attach.go`, `cmd/embedded_handlers.go`)
- `tui.go` (≈ line 92, beside `GetTheme`/`SetTheme`):
  ```go
  GetSavedThemes func() []config.NamedTheme
  SetSavedThemes func([]config.NamedTheme)
  ```
  Both may be nil — in that case the editor shows built-ins only and hides "Save As…",
  so older/embedded wiring without these handlers degrades gracefully.
- `cmd/attach.go` (≈ line 197) and `cmd/embedded_handlers.go` (≈ line 134): wire
  `GetSavedThemes`/`SetSavedThemes` to `g.SavedThemes()`/`g.SetSavedThemes(...)`.
  `SetSavedThemes` only persists; it does **not** re-apply the live palette (applying is
  the job of `SetTheme`, see below).

### Editor UI (`ui/tui/theme_editor.go`, `showThemeEditor`)
The preset dropdown becomes a list of **entries**, not just built-ins. Introduce a
local entry type built once when the dialog opens:
```go
type presetEntry struct {
    label      string // shown in dropdown; saved themes prefixed "★ "
    parent     string // canonical built-in name to resolve against
    savedIndex int    // index into the saved slice, or -1 for a built-in
}
```
- Built-in entries: `{label: p.label, parent: p.name, savedIndex: -1}` from
  `themePresets`.
- Saved entries: `{label: "★ "+nt.Name, parent: canonicalThemeName(nt.Theme.Name),
  savedIndex: i}` from `GetSavedThemes()`.

A local helper `loadSaved := func() []config.NamedTheme` reads via the handler (nil-safe).
`rebuildPresetOptions()` recomputes `entries`, sets `preset.SetOptions(labels)`, and is
called after Save As / Delete / Rename. (`tv.Select.SetOptions` exists and preserves
selection by value / clamps — verified in turbotui `widget_select.go`; **no turbotui
change needed**.)

**Selecting an entry** — a single `seedFromEntry(entry)` helper used by both
`preset.OnChange` *and* the programmatic re-selects after Save As / Delete (necessary
because `tv.Select.SetSelected` does **not** fire `OnChange` — verified in
`widget_select.go:72-77`). It seeds the whole state uniformly so there is no asymmetry
(fixes C2.3):
- colours:
  - built-in: `loadFields(paletteByName(entry.parent))`
  - saved: `loadFields(editedTheme(saved[entry.savedIndex].Theme))`
- checkboxes: `noColor`/`noShadow` are seeded from the entry's config for **every**
  selection — `false`/`false` for a built-in (built-ins carry no Colour/Shadow flags),
  the saved config's values for a saved entry. So selecting Default after a
  colours-disabled custom theme resets the toggles instead of silently leaving them on.
- `seedOverrides` (the carry source for `carryUnexposedOverrides`, see Save): set per
  selection — `saved[entry.savedIndex].Theme.Overrides` for a saved entry, else
  `cur.Overrides` for a built-in (preserving today's behaviour of carrying the
  active-at-open theme's unexposed focus-pair overrides).

**Save** (`save` closure, ≈ line 906): build the **entry config**
`entryCfg = carryUnexposedOverrides(buildThemeConfig(entry.parent, noColor, noShadow,
specs), seedOverrides)` (note `entry.parent`, the built-in name, never the `★` display
label; and `seedOverrides` from the current selection, not always `cur.Overrides`).
Then route:
- selected entry is a **saved** theme: update only that entry —
  `saved[savedIndex].Theme = entryCfg; SetSavedThemes(saved)`. Then make it the active
  theme so the live UI reflects it and it survives restart, **with the back-link set**:
  `activeCfg := entryCfg; activeCfg.SavedName = entry.name; w.handlers.SetTheme(activeCfg)`.
  Built-ins are never written.
- selected entry is a **built-in**: apply with the back-link cleared so a reopen selects
  the built-in: `activeCfg := entryCfg; activeCfg.SavedName = ""; SetTheme(activeCfg)`
  — i.e. exactly today's behaviour plus an explicit empty `SavedName`.

**Save As…** (new button): `showInputDialog("Save Theme As", "&Name:", "", onResult)`.
On confirm with a non-empty (trimmed) name:
- build `entryCfg` from the current fields + the selected entry's `parent` (same
  `buildThemeConfig`+`carryUnexposedOverrides` as Save).
- **Duplicate-name handling (fixes C2.2 — consistent destructiveness):** if a saved
  theme of that name already exists (case-insensitive trim), do **not** silently
  overwrite. Show `showConfirm("Save Theme As", "A theme named \"X\" already exists.
  Overwrite it?", …)`; on decline, abort (the user can re-Save-As with a new name). This
  matches Delete's confirm-before-destroy. Auto-suffixing (`X (2)`) is the rejected
  alternative — overwrite-with-confirm keeps the dropdown unambiguous and matches the
  "edit a copy" mental model. A brand-new name appends `NamedTheme{Name, Theme: entryCfg}`.
- `SetSavedThemes(saved)`, `rebuildPresetOptions()`, then `seedFromEntry(newEntry)` and
  `preset.SetSelected(newIndex)` (manual seed because `SetSelected` won't fire
  `OnChange`), and `SetTheme(activeCfg)` with `activeCfg.SavedName = name` so the copy is
  applied live (a "Save As" that didn't apply would feel inert) **and** reopen re-selects
  it. The source theme is untouched (built-ins are never written; an existing saved
  source is only touched if the user confirmed an overwrite of that same name).

**Delete** (new button, included): enabled only when a **saved** entry is selected
(disabled/no-op for built-ins). Confirm via `showConfirm`, then remove the entry,
`SetSavedThemes(saved)`, `rebuildPresetOptions()`. If the deleted entry was the live
**active** theme (`cur.SavedName == deleted name`, or it is the one currently selected),
reset the active theme to its parent built-in so no dangling `SavedName` survives:
`base := buildThemeConfig(entry.parent, …); base.SavedName = ""; SetTheme(base)`, and
select that built-in in the dropdown; otherwise just rebuild and keep the current
selection. Built-ins are non-deletable by construction (no `savedIndex`).

**Rename** (optional / stretch): `showInputDialog(..., withSelectAll())` seeded with the
current name; on confirm update `saved[i].Name` and rebuild. Documented as optional in
the issue; include if time permits, otherwise Delete + re-Save-As covers the need.

**On open** (replaces the current `preset.SetSelected(presetIndex(cur.Name))` +
`loadFields(editedTheme(cur))` seed, ≈ lines 898–901): if `cur.SavedName != ""` and a
saved entry of that name exists, select that `★` entry and `seedFromEntry` it; otherwise
fall back to the built-in via `presetIndex(cur.Name)` and seed from `cur` (today's
behaviour). A `cur.SavedName` pointing at a now-deleted/renamed entry degrades
gracefully to the parent built-in. This is the fix for the cross-reopen gap: after a
Save As (or editing a saved theme), reopening shows `★ MyCopy` selected, so a later Save
routes back to that saved entry instead of the built-in.

#### Button layout
Bottom row currently: `Reset(x=2,w=9)`, `Save(x=width-24,w=9)`, `Cancel(x=width-13,w=10)`.
Add on the left, clear of the right cluster (at the 80 floor the left group ends ≈ x=46,
Save starts at x=56):
```
Reset   x=2,  w=9
Save As x=12, w=11   ("Save As…")
Delete  x=24, w=10
(Save / Cancel unchanged on the right)
```
`relayout()` must reposition the two new buttons on resize (left-anchored, so their X is
constant; only add them to the relayout button block for completeness).

### Theme resolution (`ui/tui/theme.go`)
**No change.** A saved theme is resolved by the existing
`editedTheme` (`paletteByName(parent) + applyOverrides(saved.Overrides)`) in the editor,
and applied live through the existing `SetTheme` handler →
`ApplyTheme(ResolveTheme(cfg, …))` → `RefreshTheme()`. `paletteByName` /
`canonicalThemeName` / `applyOverrides` are unchanged.

### User-facing behaviour (Goal 2)
- Open the editor → dropdown lists `Default`, `High-contrast…`, `Dark…`, then any
  `★ <custom name>`.
- Pick a built-in → **Save As…** → type a name → a new `★ name` entry is created,
  selected, and applied. The built-in is unchanged.
- Edit the custom theme's colours/toggles → **Save** → only that saved entry (and the
  active theme) update; built-ins never change.
- **Delete** removes a selected custom theme after confirmation.
- Restart → custom themes reappear (persisted under `saved_themes`).

---

## Criterion 1 — GOAL MATCH
- Goal 1 does exactly the recommended "1a lead with the colour" + "1b section
  separators"; no unrelated layout rewrite (the geometry resolver/guard are unchanged
  because row width is order-independent).
- Goal 2 implements the recommended config model (`NamedTheme` + `SavedThemes`),
  persistence handlers, dropdown listing, "Save As…", saved-vs-built-in Save routing,
  and built-in immutability — the issue's explicit asks. Delete is included (issue lists
  it optional but it is the natural counterpart to Save As); Rename is scoped as
  optional/stretch. No scope creep beyond the issue (e.g. no turbotui optgroup work).

## Criterion 2 — USABILITY
- The colour now anchors each line and sections are spaced — directly the reported pain.
- The user drives every input: `showInputDialog` for naming/renaming (free text, the
  same primitive used elsewhere), the dropdown for selection, real buttons for
  Save As / Delete.
- **Identity survives across sessions (resolved C2.1).** `ThemeConfig.SavedName` links
  the active theme to its saved entry; the editor re-selects that `★` entry on open, so
  "edit a copy across restarts" — the core of the issue — works: a Save after reopen
  writes back to the saved entry, not the parent built-in.
- **Consistent destructiveness (resolved C2.2).** Both destructive paths confirm:
  Delete via `showConfirm`, and Save-As-onto-an-existing-name via `showConfirm` before
  overwriting. No silent data loss.
- **Symmetric state seeding (resolved C2.3).** `seedFromEntry` seeds colours **and** the
  `noColor`/`noShadow` checkboxes for *every* selection (built-in ⇒ `false`/`false`), so
  switching presets never leaves a stale toggle the user didn't set.
- The right thing is surfaced, not silent: "Save As…" creates a *visible* new entry and
  applies it; editing a saved theme writes to that entry; built-ins visibly cannot be
  deleted (Delete is inert on them). Saved themes are visually distinguished by the `★`
  prefix (gogent-only path the issue recommends).
- Graceful degradation: with `GetSavedThemes`/`SetSavedThemes` unset, the editor behaves
  exactly as today (built-ins only, no Save As/Delete).

## Criterion 3 — NO REGRESSIONS
- **Layout guard** `checkThemeEditorLayout` runs at package init / every test; its math
  is unchanged (row width order-independent), so it still passes. Only
  `themeEditorColumnRows` gains the separator count — kept in lockstep with the build
  loop so `maxScroll`/scrollbar stay correct.
- **Swatch/picker live tracking** (`swatchStyle`, `DrawFn`, `armPicker`, `OnChange`,
  `pickerCommitSentinel`) is untouched — only the swatch's `cellRect` position changes.
- **Flatten invariant**: `themeRoles`/`themeGroups` membership and order are *not*
  touched, so `fields[i]`/`pickers[i]` stay keyed by the flat index; the #267 flatten
  and #265 `carryUnexposedOverrides` suites still hold.
- **`SetTheme` wholesale-replace** semantics preserved; `carryUnexposedOverrides` still
  runs on every Save (now sourced from the *selected entry's* overrides for saved
  themes, so unexposed keys are preserved per theme — covered by a new test).
- **Config backward-compat**: `saved_themes` and `saved_name` are both `omitempty`;
  absent keys ⇒ nil slice / empty string ⇒ no behaviour change. `LoadConfig` is a plain
  `json.Unmarshal` with no version gate, so old files load untouched.
  `buildThemeConfig` never sets `SavedName`, so the existing `reflect.DeepEqual`
  assertions in `TestBuildThemeConfig`/`…RoundTrip` still pass unchanged.
- **`seedOverrides` is pinned** (was underspecified): saved entry ⇒ that entry's
  overrides; built-in ⇒ `cur.Overrides`. So `carryUnexposedOverrides` preserves the
  #265 focus pairs per-theme and never bleeds the active theme's unexposed keys onto a
  saved one.
- **Tests to update/add**:
  - Update `TestIssue267*` row-count / placement expectations for the separator rows and
    swatch-left order; add an assertion that the swatch column precedes the label column.
    (`TestIssue267FieldsSeededInPlacementOrder` still works: the field remains the first
    token *after* the label cell.)
  - Add config round-trip test: old JSON without `saved_themes`/`saved_name` loads;
    save/select/edit/persist of a custom theme; built-in immutability after editing a
    saved copy; `carryUnexposedOverrides` across a saved-theme Save.
  - Add **back-link reopen** test (the C2.1 fix): Save As → reopen the editor → assert
    the `★` entry is selected (not the parent built-in) → edit + Save → assert the saved
    entry updated and the built-in unchanged. And: `SavedName` pointing at a deleted
    entry falls back to the parent built-in on open.
  - Add **`SavedName` isolation** test: configs in `SavedThemes` carry no `SavedName`;
    only the active `Config.Theme` does.

## Criterion 4 — HOLISTIC DESIGN across both repos
- **Right place / seam respected.** All logic lives in gogent: config model + persistence
  in `internal/{config,gogent}`, UI in `ui/tui`, wiring in `cmd/`. The gogent↔turbotui
  seam is unchanged — gogent keeps owning theme semantics and only consumes turbotui
  widget primitives.
- **Turbotui: no change.** The one possible reason (grouped dropdown entries) is avoided
  via the issue-recommended `★`-prefixed labels. `tv.Select` already exposes
  `SetOptions` (preserves selection by value, clamps, closes popup) — verified in
  `$HOME/work/turbotui/turbotv/widget_select.go` — so the dropdown can be repopulated
  after Save As/Delete with no upstream work. No paired turbotui issue is opened.
- **Downstream effects considered.** `SetOptions` is already covered by turbotui tests
  (preserves selection by value, clamps, closes the popup); `SetSelected` does **not**
  fire `OnChange`, which is why the editor seeds fields manually after a programmatic
  re-select. The live-apply path (`ApplyTheme`/`RefreshTheme`/`reseedSelect`) is reused
  unchanged, so a saved theme recolours the same way a built-in does.
- **Tab order follows the new visual order — safe, but note the dependency.** turbotui's
  `sortFocusOrder` (`desktop.go:633-647`) orders focus by TabIndex then reading order
  (Y, then X). All theme-editor widgets share the default TabIndex, so after the swatch
  moves to the left of the field, Tab traverses swatch→field by ascending X automatically
  — no zig-zag, no gogent change. This relies on turbotui's spatial tiebreak; if upstream
  ever switched to explicit-TabIndex ordering, gogent would need to assign TabIndex to
  preserve order. Documented so the dependency is visible.

---

## Regression risks (watchlist)
1. **`themeEditorColumnRows` vs. build loop drift.** If the separator count in
   `themeEditorColumnRows` doesn't exactly match the extra `logical++` in the build loop,
   the scrollbar thumb and `maxScroll` desync (last roles unreachable or extra blank
   scroll). Mitigation: single rule — "one separator between adjacent groups in a column,
   none after the last" — applied in both places; `checkThemeEditorLayout`'s reveal
   invariant catches a mismatch at init.
2. **`carryUnexposedOverrides` source.** Using the *active* theme's `cur.Overrides` when
   saving a *saved* theme would bleed unexposed keys across themes. Mitigation: carry the
   selected entry's own overrides; add a test.
3. **Dropdown index ↔ entry mapping.** After Save As/Delete the entries list changes;
   `save`/`OnChange` must read the freshly-rebuilt `entries`, not a stale capture.
   Mitigation: `entries` is a closure variable rebuilt in place by
   `rebuildPresetOptions()`; all closures read it live.
4. **`buildThemeConfig` parent name.** Must pass the entry's `parent` (built-in), never
   the `★`-prefixed display label, or `canonicalThemeName` falls back to Default and the
   overrides are computed against the wrong base. Mitigation: entries carry `parent`
   explicitly.
5. **Test buffer width.** The headless test app is 80-wide; the two new bottom buttons
   must fit left of `Save` at the floor (they do: left group ends ≈ x=34, Save at x=56).
6. **Dangling `SavedName`.** A `SavedName` pointing at a deleted/renamed entry must not
   strand the editor. Mitigation: open-time selection and Delete both fall back to the
   parent built-in when no matching saved entry is found; Delete of the active theme
   resets `SavedName` to `""`.
7. **`SavedName` must not leak into the saved list.** The configs stored in
   `SavedThemes` are built without `SavedName` (only the active `Config.Theme` carries
   it); a saved entry is identified by its `NamedTheme.Name`. Mitigation: set `SavedName`
   only on the `activeCfg` handed to `SetTheme`, never on `entryCfg`.

---

## Open questions (non-blocking; sensible defaults chosen)
1. **Rename inclusion.** Included as optional/stretch (via `showInputDialog` +
   `withSelectAll`, updating `NamedTheme.Name` + `rebuildPresetOptions`). Confirm whether
   it ships in this cut or is deferred — Delete + Save-As already cover the core need.
2. **`★` prefix glyph.** Using `★` to mark saved themes; if a plain-ASCII marker is
   preferred for narrow/legacy terminals, swap to e.g. `* ` — purely cosmetic, no design
   impact.

(Previously-open items now **resolved** in the design: Save As applies live *and* sets
the `SavedName` back-link so the association survives reopen; duplicate Save-As name
confirms-before-overwrite rather than silently overwriting.)
