# Issue #204 — Live theme re-apply for open session windows

## Problem
Changing the theme (Config → Theme…, or editing `config.json`) only repaints the
gogent-drawn chrome (desktop, sidebar) and the menu. Open `SessionWindow`s keep
their construction-time colours:

1. **Transcript records freeze colours by value.** `transcriptRecord.color` and
   `styledLine.color` capture a resolved `tui.Color` when the record is created,
   and `renderOne`/`appendLine` paint from those frozen values. A re-render alone
   would still draw old messages in old colours.
2. **Window chrome is seeded once.** The status label, model/effort labels,
   selects, Send/running buttons, input box and window frame take their colours
   at construction. The error-red Stop button, the divider rule and the
   `effortLabelEnabledFG` snapshot are gogent-set; the rest seed from turbotui's
   `activeTheme`.
3. **`ApplyTheme` never installed the turbotui chrome theme.** It set
   `tv.DefaultTheme` (read only by gogent dialogs) but never called
   `tv.SetTheme`, so turbotui's `activeTheme` — what windows/menu/widgets seed
   from — was frozen at the stock palette even across a restart.
4. **`RefreshTheme` only rebuilt the menu.**

## Fix
### A. Resolve transcript colours by role at render time (`transcript_model.go`)
- Add a `colorRole` enum (`roleUser/Agent/Note/Tool/Result/Info/Error`, plus
  `roleNone`) and `roleColor(role)` that maps a role to the *current* package
  palette variable.
- Add a `role` field to `transcriptRecord` and `styledLine`. Add
  `headerColor()` / `effectiveColor()` helpers returning `roleColor(role)` when a
  role is set, else the stored `color` (back-compat for non-palette lines such as
  dialog body text). `renderOne`/`appendLine` paint via these helpers.
- Keep the `color` field populated (existing tests assert it equals the palette
  colour at creation), but the role is now what survives a theme change.
- Tag every add site in `session_window.go` with its role (`addUser`,
  `addNote`, `addAssistant`, `addThought`, `addCompaction`, tool call/result
  builders, `addError`, the budget note, the system-ready line and `restore`).
  Rich-Markdown bodies already recompute via the `mdPaletteGen` cache.

### B. Install the turbotui chrome theme (`theme.go`)
- `ApplyTheme` calls `tv.SetTheme(tv.DefaultTheme)` after choosing the dialog
  chrome, so `activeTheme` tracks the active palette. This makes freshly built
  widgets, the rebuilt menu bar and re-seeded windows draw in the new palette and
  makes a restart genuinely consistent.

### C. Re-skin every open window live (`session_window.go` + `tui.go`)
- New `(*SessionWindow).refreshTheme()`:
  - re-render the transcript (picks up role colours via A);
  - re-seed window frame + content surface, model/effort labels, both selects,
    all four input-row buttons and the input box from `tv.ActiveTheme()`;
  - restore gogent accents: Stop button error-red, divider rule `chromeDivider`,
    `effortLabelEnabledFG`, the effort label's enabled/disabled colour;
  - `refreshStatus()` to recolour the status line.
  - read-only analysis windows refresh only transcript + frame.
- The greyed-state guard closures (`guardEffortSelect`, `guardInterjectButton`)
  read the enabled colour from `tv.ActiveTheme()` live instead of a captured
  construction-time value, so they no longer pin a stale colour.
- `(*Workbench).RefreshTheme()` rebuilds the menu, then walks `w.sessions`
  calling `refreshTheme()` on each, then forces `desktop.Redraw()`.

## Tests (GLM)
After a live default→high-contrast/dark switch via the SetTheme path: an open
window's transcript records (including ones added *before* the switch) and chrome
(status/Stop/divider/labels) report the new palette colours. Assert the colours
changed, not just the desktop.
