# Design — gogent #463: Keybindings customizer lists only a small subset of actions

## Problem recap

The Customize Keybindings dialog lists only the ~21 catalog entries that carry a
non-empty `action.actionID`. Frequent keyboard-driven session operations — the user's
example **"Rename session"**, plus Previous session, Pin/unpin, Move up/down, Switch
model, Export MD/JSON, etc. — have no `actionID`, so they are filtered out of the
customizer (`rebindable()`), never registered (`rebuildBindings`), never persisted
(`buildKeybindingsConfig`), and show no shortcut hint on their menu items. The fix is to
**promote these session/window operations to first-class rebindable actions** using the
existing #401 machinery.

This is a **FIX**, scoped to gogent's `ui/tui` package. No turbotui change is required
(confirmed below).

## Chosen approach — Option A (promote the missing non-slash operations)

Option A from the issue is the right altitude: it satisfies the acceptance criteria with
the smallest change and reuses the catalog → registry → customizer → persistence →
menu-hint pipeline end-to-end. Option B (auto-list every `run != nil` entry) is rejected:
it needs synthesized stable actionIDs for persistence anyway and risks freshly-listed
actions grabbing keys — more surface, more risk, no extra user value. Option C (rebindable
slash commands) is deferred as a product decision, per the issue.

Each promoted entry gets a stable `tv.ActionID`, a `scope`, and a `deflt`. The session
operations all act on `ActiveID()` regardless of focus, so they are **`ScopeGlobal`**
(matching the existing menu accelerators), with one exception (Copy last code block) that
is a transcript action and is **`ScopeFocus`**.

### "Unbound by default" representation (key design decision)

Most promoted operations have no conventional key (Pin, Move up/down, Switch model,
Export, Close others/all, Saved sessions). The issue says they may "ship unbound-by-default
… the customizer still lists them so the user can assign one." The correct way to express
"rebindable but bound to nothing by default" is:

```go
deflt: unboundChord   // the existing sentinel {Key: KeyF12, Rune: ''}
```

Why this is the right sentinel and not the zero chord:

- A **zero** `tv.Chord{}` is catastrophic as a default: `rebuildBindings` would register
  it, and `Chord.Matches` treats `Key==KeyUnknown` + `Rune==0` as a wildcard that matches
  **every unmodified keypress** — it would swallow all plain keys globally. Must not use it.
- `unboundChord` threads cleanly through every site that already special-cases it:
  - `rebuildBindings` / `registerTranscriptBindings` — `if chord == unboundChord { skip }`
    → no live binding (verified at keybindings.go:233 and session_window.go:2635).
  - `chordFor` with no override returns `deflt == unboundChord`.
  - `conflictHolder` / `firstCollisionVictim` — explicitly skip `unboundChord`, so an
    unbound-default action never falsely collides with anything.
  - `buildKeybindingsConfig` — `sameChord(cur, deflt)` is true, so it persists nothing
    until the user actually binds a key; then the override differs from `deflt` and round-
    trips via `formatChordSpec`/`parseChordSpec` (`"none"` ⇄ `unboundChord` already
    handled).
  - `resetBinding` → `applyBinding(id, unboundChord)` → `recordOverride` deletes the
    override → back to the clean unbound state.

The small set with a real conventional key gets a real `deflt` instead (see table).

## Exact changes

### gogent — `ui/tui/command_palette.go`

**1. New ActionID constants** (in the `const (...)` block, ~line 68):

```go
actionSessionPrev        tv.ActionID = "session.prev"
actionSessionCloseOthers tv.ActionID = "session.closeOthers"
actionSessionCloseAll    tv.ActionID = "session.closeAll"
actionSessionRename      tv.ActionID = "session.rename"
actionSessionPin         tv.ActionID = "session.pin"
actionSessionMoveUp      tv.ActionID = "session.moveUp"
actionSessionMoveDown    tv.ActionID = "session.moveDown"
actionSessionSwitchModel tv.ActionID = "session.switchModel"
actionSessionExportMD    tv.ActionID = "session.exportMarkdown"
actionSessionExportJSON  tv.ActionID = "session.exportJSON"
actionSessionsBrowser    tv.ActionID = "session.browser"
actionTranscriptCopyCode tv.ActionID = "transcript.copyCode"
```

**2. Attach `actionID`/`scope`/`deflt` to the existing `rawActions()` entries** (no new
entries, no behavior change to `run`):

| Catalog entry | actionID | scope | deflt | Rationale |
|---|---|---|---|---|
| Previous session | `actionSessionPrev` | Global | **Ctrl+P** | mirrors Next (Ctrl+]); see note on Ctrl+[ |
| Rename session | `actionSessionRename` | Global | **F2** | conventional rename key, conflict-free, deliverable |
| Pin / unpin session | `actionSessionPin` | Global | `unboundChord` | no conventional key |
| Move session up | `actionSessionMoveUp` | Global | `unboundChord` | no conventional key |
| Move session down | `actionSessionMoveDown` | Global | `unboundChord` | no conventional key |
| Switch model | `actionSessionSwitchModel` | Global | `unboundChord` | no conventional key |
| Export transcript (Markdown) | `actionSessionExportMD` | Global | `unboundChord` | gated on `GetTranscript`; `run` guards |
| Export transcript (JSON) | `actionSessionExportJSON` | Global | `unboundChord` | gated; `run` guards |
| Close other sessions | `actionSessionCloseOthers` | Global | `unboundChord` | destructive ⇒ no default key |
| Close all sessions | `actionSessionCloseAll` | Global | `unboundChord` | destructive ⇒ no default key |
| Saved sessions browser | `actionSessionsBrowser` | Global | `unboundChord` | gated on `ListSavedSessions`; `run` guards |
| Copy last code block | `actionTranscriptCopyCode` | **Focus** | `unboundChord` | transcript action; pairs with Copy-answer |

**Why these defaults are safe.** `F2` is a named key and `Ctrl+P` carries Ctrl, so both
pass `validateScopeRule` (Global forbids only plain runes). Both are deliverable
(`tui.Deliverability`), and neither collides with the existing global/fixed set
(Ctrl+N/]/W/Q/comma, Ctrl+Shift+V/H/G/D/M, Ctrl+K/F, Ctrl+C/X). The `unboundChord`
defaults register nothing and collide with nothing.

> **Deliverability note (corrects the issue's suggested default):** the issue proposes
> `Ctrl+[` for Previous session "to mirror Next's Ctrl+]". `Ctrl+[` is byte-identical to
> **Esc** in a terminal and is rejected by `tui.Deliverability`, so it can never fire and
> would be refused by `validateCapture`. We use **Ctrl+P** (mnemonic, deliverable,
> conflict-free) instead. (Alternatives in Open Questions.)

**3. Shorten two display names** so the customizer's 26-cell name column does not truncate
them, and to align with the menu labels:

- `"Export transcript (Markdown)"` → `"Export Markdown"`
- `"Export transcript (JSON)"` → `"Export JSON"`

All other promoted names are ≤ 26 cells and need no change. (Names are shared with the
palette/cheatsheet, so this is a small, intentional palette-text change — see Regressions.)

### gogent — `ui/tui/session_window.go`

`registerTranscriptBindings` registers Focus actions by an **explicit hardcoded list**,
not generically. (The issue's claim that Focus actions "auto-register" is only true for
the nine that already have a `focus(...)` line.) So promoting the one Focus action requires
adding one line after the Copy-answer registration (session_window.go:2656):

```go
focus(actionTranscriptCopyCode, func() bool { sw.copyLastCode(); return true })
```

### gogent — `ui/tui/tui.go` (menu migration → live shortcut hints)

In `rebuildMenu()`, migrate the Session-menu items from `tv.NewMenuItem(...)` to
`w.menuActionItem(label, id)` so each shows its live chord (`chordFor`) and fires the same
catalog `run`:

- `"Close &Others"` → `menuActionItem("Close &Others", actionSessionCloseOthers)`
- `"Close Al&l"` → `menuActionItem("Close Al&l", actionSessionCloseAll)`
- `"Saved &Sessions…"` → `menuActionItem("Saved &Sessions…", actionSessionsBrowser)`
- `"&Rename Active…"` → `menuActionItem("&Rename Active…", actionSessionRename)` *(acceptance criterion 3)*
- pin item (dynamic label) → `menuActionItem(pinLabel, actionSessionPin)`
- `"Move Active &Up"` → `menuActionItem(..., actionSessionMoveUp)`
- `"Move Active &Down"` → `menuActionItem(..., actionSessionMoveDown)`
- `"Export &Markdown…"` → `menuActionItem(..., actionSessionExportMD)`
- `"Export &JSON…"` → `menuActionItem(..., actionSessionExportJSON)`

In `viewItems()`, migrate `"Copy Last &Code Block"` → `menuActionItem(..., actionTranscriptCopyCode)`.

The catalog `run` for each is byte-identical to the closure the menu item carries today
(verified), so click behavior is unchanged; only the shortcut hint is added. The dynamic
pin label is passed through `menuActionItem`'s `label` argument, so "Pin/Unpin Active"
toggling is preserved. **Previous session and Switch model have no existing Session-menu
item** — they gain rebindability via the palette + customizer only (no menu change needed).

### gogent — `ui/tui/keybinding_customizer.go` (display of unbound defaults)

The customizer's status messages render `displayChord(a.deflt)` directly. For an
`unboundChord` default that prints the sentinel as "F12", which is wrong. Route those
through the existing `chordLabel` (renders `unboundChord` as "—", and delegates to
`displayChord` for real chords, so existing behavior is unchanged):

- line ~274 "is already at its default (%s)" → `chordLabel(a.deflt)`
- line ~284 "%s reset to %s" → `chordLabel(a.deflt)`
- line ~279 "default %s is in use by %q" → `chordLabel(a.deflt)` (defensive; an unbound
  default is skipped by `conflictHolder`, so this branch is unreachable for them — fixed
  for consistency).

No other customizer change is needed: `keybindRowText` already uses `chordLabel`, so an
unbound-default action renders as `Pin / unpin session   —   (unbound)`.

### turbotui — no change

The `BindingRegistry` / `ActionID` / `Chord` / `Scope` / `Deliverable` primitives are
sufficient: we only add gogent catalog metadata and one Focus registration. Confirmed
against `$HOME/work/turbotui/turbotv/binding.go`. The seam is respected — gogent owns the
catalog and scope/default policy; turbotui owns dispatch, matching, and deliverability.

## User-facing behavior

- Customize Keybindings now lists **Rename session** under "Session" (criterion 1), plus
  Previous session, Pin/unpin, Move up/down, Switch model, Export Markdown/JSON, Close
  others/all, Saved sessions, and Copy last code block.
- Rename shows `F2  (default)`; Previous shows `Ctrl+P  (default)`; the keyless ones show
  `—  (unbound)`. The user drives the input exactly as for the existing 21: Enter →
  capture → the conflict/self-lockout/deliverability pipeline → live apply + persist.
- Binding e.g. `Ctrl+R` to Rename fires `RenameSession(ActiveID())` immediately in every
  window and after restart via `SetKeybindings` (criterion 2). Global rebinds with a plain
  letter are refused with the existing scope-rule message ("use a Ctrl/Alt combo or
  function key") — correct, surfaced, not silent.
- The Session menu's "Rename Active…" (and Pin/Move/Export/Close-others/all/Saved) now show
  their live chord as a shortcut hint (criterion 3 & 4).
- Side effect in palette/cheatsheet: promoted entries now display their chord ("F2") or "—"
  where they previously showed nothing. For Rename this is a discoverability win; the "—"
  rows truthfully signal "rebindable but unbound."

## Criteria self-assessment

1. **GOAL MATCH.** Does exactly what the issue asks — promotes the missing non-slash
   session/window operations to rebindable actions via the existing pipeline. No new dialog,
   no refactor, no slash-command rebinding (deferred per issue). Acceptance criteria 1–5 are
   each addressed above.
2. **USABILITY.** The action lands in the right dialog/category; the user drives capture
   exactly as before; defaults are conventional (F2) or honestly "unbound" with a visible
   "—"; the scope-rule rejection is surfaced with an actionable message; the menu hint makes
   the bound key discoverable. Long Export names are shortened so the row column isn't
   truncated.
3. **NO REGRESSIONS.** The 21 existing rebindable actions and the slash-command palette
   entries are untouched (we only *add* metadata). `run` closures are byte-identical to the
   menu's, so click behavior is unchanged. `unboundChord` defaults register/collide/persist
   as nothing, so they cannot steal a key or pollute the config. `chordLabel` substitution
   is behavior-preserving for real chords. The #461 row-width contract is preserved (row
   stays ≤ 54 cells: name col 26 + chord col 14 + 10-cell tag; the new "(unbound)" tag ≤ 10).
   No transcript/session invariants change — `RenameSession`/`TogglePin`/`MoveSession`/
   `exportActive` already no-op safely on an unknown/absent active session, so a global
   binding firing with no session is safe.
4. **HOLISTIC across both repos.** Change is confined to gogent `ui/tui` where the catalog
   lives; turbotui needs nothing and its API gap is none. The terminal-level constraint
   (Ctrl+[ == Esc) — which is *turbotui/terminal* knowledge — is respected by choosing a
   deliverable default. The pre-existing turbotui limitation (no `Unregister`, so closed
   windows leak inert Focus bindings) is unchanged in character; adding one Focus action
   (Copy code) adds at most one more inert leaked binding per closed window, consistent with
   the nine that already do this — not worsened by design.

## Regression risks / watch-list

- **Ctrl+P stolen from text inputs.** Global bindings fire before the focused widget, so
  Ctrl+P will no longer reach a text input's cursor movement (if any). Consistent with the
  already-global Ctrl+N. If undesirable, ship Previous unbound (Open Questions).
- **Palette text change** for the two Export entries ("Export transcript (Markdown)" →
  "Export Markdown"). Any test asserting the old literal name would need updating. Grep
  found none.
- **"unbound" tag semantics widen** from "user-cleared" to also "default-unbound". Visible
  only as `—  (unbound)`; both states mean "no key", which is truthful. No code depends on
  the distinction.
- **Gated globals registered when handler is wired.** Export/Saved-sessions register only
  once the user binds a key (until then `unboundChord` ⇒ skipped). Their `run` already
  guards an unwired handler (same contract as Sub-agent settings), so firing is safe.
- **Tests to add** (mirroring `keybindings_issue401_test.go`,
  `keybinding_registry_phase4a_test.go`, `keybinding_customizer_phase4b_test.go`):
  `actionSessionRename` appears in `rebindable()`; binding a chord registers a live Global
  binding and round-trips through `buildKeybindingsConfig`/`LoadKeybindings`; the Rename
  menu item carries the ActionID tag and tracks `chordFor`; an `unboundChord`-default
  action persists nothing until bound and resets back to unbound; the existing 21 are
  unaffected. Run the dev gate (build/vet/gofmt/lint/test, no `-race`).

## Open questions

1. **Previous session default.** Recommend **Ctrl+P**. Alternatives if stealing Ctrl+P from
   text inputs is a concern: ship Previous **unbound** (acceptance criterion 4 wants "a
   sensible default", but unbound + listed still lets the user assign one), or use a function
   key (e.g. F-key) to avoid touching any control letter. Decision affects one line.
2. **Copy last code block default.** Proposed `unboundChord` to avoid surprising a `'c'`
   keystroke while a transcript is focused. If a discoverable default is wanted, `'c'`
   (Focus scope, conflict-free vs the existing a/t/r/e/f/u/y//Esc) is the natural pick.
3. **Scope of "etc."** Close-others / Close-all / Saved-sessions are promoted for holistic
   coverage but are arguably dialog/destructive actions; if the maintainer prefers a tighter
   diff, they can be left palette-only without affecting the user's headline ask (Rename and
   the core session ops). Recommend keeping them — the issue explicitly calls the dialog
   "extremely limited" and asks for holistic coverage.
4. **Export entry rename.** Shortening to "Export Markdown"/"Export JSON" is to avoid the
   26-cell customizer-column truncation. The alternative is to widen the name column, but the
   #461 contract has zero slack at MinW=58, so renaming is the lower-risk choice. Confirm the
   palette-text change is acceptable.
