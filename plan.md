# Issue #200 — Theme-ignoring regions + new black-background "dark" theme

## Problem
1. **Code blocks** (`markdown.go`) paint `mdPalette.codeBG = tui.ANSIColor(0)` (hardcoded
   black) regardless of theme → black-on-blue islands under the default theme.
2. **Sidebar / Overall panel** (`sidebar.go`) paints with `chromePanelBG`, which the
   default palette sets to `ANSIColor(0)` (black) while `chromeDesktopBG` is `ANSIColor(4)`
   (blue) → a black rectangle inside the blue UI.
3. There is no built-in plain **black-background dark** theme (only `default` blue and
   `high-contrast` Okabe–Ito).

## Design

### Part 1 — make the two regions follow the theme
- Add a new theme role **`CodeBG`** to `Theme` (the fenced-code background), threaded
  through `ResolveTheme` (degrade), `applyOverrides` (`code_bg` key) and the editor
  (`themeRoles`). `applyMarkdownPalette` reads `t.CodeBG` instead of the hardcoded ANSI 0.
- **Panel bg reconciliation:** the sidebar already reads `chromePanelBG` (= `t.PanelBG`),
  so the *only* change needed is the palette. Set the default palette's `PanelBG` to
  `ANSIColor(4)` (the same blue as `DesktopBG`) so the sidebar belongs to the desktop
  chrome. No `sidebar.go` edits.
- Default `CodeBG = ANSIColor(4)` (blue, matches chrome → cohesive, not a black island).
  High-contrast keeps `hasCodeBG=false` (pure-black UI), so its `CodeBG` is unused but set
  to black for completeness.
- Update the pre-ApplyTheme initial values (`chromePanelBG` var, `mdPalette` literal) to
  match the new default palette.

### Part 2 — new `dark` theme
- `themeDark = "dark"` constant; `darkPalette()` alongside `defaultPalette()` /
  `highContrastPalette()`. A pure-black background with a cohesive, muted (Tokyo-Night /
  One-Dark-ish) RGB palette: cool blues/greens for prose + semantic roles, a warm amber
  accent, soft-white titles, a dark-grey `CodeBG` so code stands apart on black.
- Wire-up: `canonicalThemeName` (aliases `dark`/`midnight`/`black`), `paletteByName`,
  `ApplyTheme` chrome switch (black-canvas dialog chrome, shared with high-contrast — the
  helper is renamed `blackCanvasTVTheme`), `applyMarkdownPalette` (dark gets a code bg),
  the editor `themePresets`, and the `config.ThemeConfig.Name` doc.

## Touch points
- `ui/tui/theme.go` — `CodeBG` field, degrade, override, `themeDark`, `darkPalette`,
  `canonicalThemeName`, `paletteByName`, `ApplyTheme` switch, `blackCanvasTVTheme` rename,
  default `PanelBG`/`CodeBG`, initial `chromePanelBG` var.
- `ui/tui/markdown.go` — `applyMarkdownPalette` uses `t.CodeBG`; initial literal.
- `ui/tui/theme_editor.go` — `code_bg` role + `Dark` preset.
- `internal/config/config.go` — `ThemeConfig.Name` doc mentions `dark`.
- `ui/tui/sidebar.go` — **no change** (already reads the themed `chromePanelBG`).

## Round 2 — fixes after review/critique
Verified against turbotui source that the default session-window background is
`WindowBG = ANSIColor(4)` (blue), so the round-1 `CodeBG = ANSI(4)` painted code
blocks blue-on-blue (invisible). Fixes applied:
1. **Default `CodeBG` → `RGBColor(0x10,0x14,0x50)`** (dark navy): a subtle shade of
   the desktop blue, distinct from the blue window. Degrades to a dark-blue 256 index
   (ANSI 17) and to black (ANSI 0) at 16 colours — never to ANSI 8, so it never
   collides with the grey comment foreground. Initial `mdPalette` literal mirrors it.
2. **`hasCodeBG` predicate** → `t.CodeBG != t.PanelBG` (post-degrade), replacing the
   `t.Name != themeHighContrast` special-case. Now suppression is principled: it fires
   for any black-canvas palette where the code panel would vanish — covering both
   high-contrast (always) and dark at 16 colours (where `0x262626`→ANSI 0 == black bg).
3. **`blackCanvasTVTheme`** now populates `DialogMnemonicFG` and the menu chrome
   (`MenuBarFG/BG`, `MenuHotFG/BG`, `MenuSelectFG/BG`, `MenuShadow`) so the dark (and
   high-contrast) menu bar/dropdowns are styled, not terminal-default on black.

Two tester-owned tests assert the old invisible-blue behaviour and now fail (left in
place per the lane rule, flagged for the tester to update):
- `TestIssue200DefaultPaletteNoHardcodedBlack` — asserts `CodeBG == DesktopBG`/`ANSI(4)`.
- `TestIssue200CodeBGDegrade` (l.633) — asserts default 16-colour `CodeBG == ANSI(4)`.
Both should instead assert `CodeBG != PanelBG/WindowBG`.

## Test targets (for GLM)
- `defaultPalette().CodeBG` / `.PanelBG` are not `ANSIColor(0)` and differ across palettes.
- `applyMarkdownPalette` sets `mdPalette.codeBG` from `t.CodeBG` (per-theme).
- `paletteByName("dark")` / canonical aliases return a complete `darkPalette()`; it is
  selectable and `ApplyTheme` installs a black-canvas chrome for it.
