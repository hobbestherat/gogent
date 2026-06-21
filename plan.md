# Issue #214 — polish the #201 running-turn buttons

Three sub-fixes in `ui/tui/session_window.go` (+ small helpers in `ui/tui/theme.go` reuse).

## 1. Uniform button width
- Keep `buttonWidth(label) = StringWidth(label)+4` as the per-label primitive (used by Send too).
- Add `uniformButtonWidth(labels...) = max(buttonWidth(label))` — the common width the three
  running buttons share so they read as one set. Full mode → 13 (Interject, the widest);
  glyph mode → 5 (all glyphs already equal).
- Lay out all three running buttons at that single width. turbotui's Button centres its
  `[ … ]` caption within its bounds, so a wider box just pads/centres the shorter labels —
  no new drawing code.
- Redefine `runningButtonsWidth` as `3*uniformButtonWidth + 2*gap + margin` so the
  glyph-degrade threshold reflects the real (uniform) footprint. New full footprint = 42
  (was 37); glyph footprint stays 18.

## 2. Vertical alignment with the prompt box
- The buttons are 1 row tall in a 3-row (`inputH`) input area. Add
  `buttonRowY(top, inputH) = top + (inputH-1)/2` to centre the button row on the prompt's
  middle line instead of floating at the top edge. Apply to the running buttons and the
  idle Send button (same visual slot) so they don't jump when busy toggles. The input box
  itself keeps the full `inputH` height anchored at `top`.

## 3. Readable, theme-aware Interject colour (enabled + disabled)
- Enabled → `ActiveTheme().ButtonFG` (matches Queue, distinct from Stop's `colorError`,
  read live so #204 live-theme switches recolour it). On the default green button this is
  white at 3.11:1 — clears the 3:1 large-text floor.
- Disabled (empty input) → de-emphasised but legible. `colorNote` (the project-wide disabled
  grey) washes to ~1.3:1 on the default theme's green button — the bug. New helper
  `interjectButtonFG(enabled)`:
  - keep `colorNote` where it still clears `minContrastLarge` (3:1) against `ButtonBG`
    (the dark button canvas of the dark/high-contrast presets) **or** where the bg is the
    terminal default (NO_COLOR, contrast undeterminable — colorNote is itself the default);
  - otherwise fall back to `mostReadableOn(ButtonBG)` = higher-contrast of black/white. On the
    default green button that is black (6.75:1) — clearly readable, visibly recessed from the
    bright-white enabled label, distinct from Stop's red.
- `guardInterjectButton`'s per-draw hook routes both states through `interjectButtonFG`.
  Ties into the #202 contrast machinery (`contrastRatio`, `minContrastLarge`) rather than a
  one-off colour.

## Interfaces the tester targets
- `buttonWidth`, `uniformButtonWidth`, `runningButtonsWidth`, `buttonRowY` (package funcs).
- `layoutInputRow` → equal `.W` for the three buttons (full + glyph); equal centred `.Y`;
  buttons line up with the prompt box; new degrade flip at wd=63 (input == minInputWidth).
- `interjectButtonFG(bool)` and `mostReadableOn(tui.Color)` for the colour assertions.

## NOTE for tester
The pre-#214 `running_buttons_test.go` encodes the old ragged widths and will need updating:
- `runningButtonsWidth(full)` 37 → 42; glyph stays 18.
- per-button `.W` now all == `uniformButtonWidth(...)` (13 full / 5 glyph), not per-label.
- degrade flip point wd 58 → 63 (input == minInputWidth at the flip).
