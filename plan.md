# Issue #202 — Contrast audit of the default theme

## Problem
System notes (`addNote`, the "[System] … ready" banner, restored system messages)
are painted in `colorInfo` = ANSI 12 (bright blue). The transcript and status line
render on the turbotui **window background = ANSI 4 (blue)** (`tv.DefaultTheme.WindowBG`,
seeded into every `TextView`/`Label`). Bright-blue-on-blue is ~2.6:1 — unreadable.
This is the same class of bug as #193 (which only recoloured the running status line).

The broader ask: audit every default-palette foreground against the background it is
actually drawn on and guarantee a documented minimum contrast.

## Backgrounds each foreground is drawn on (post-#200, reconciled palette)
- Transcript semantic roles (User/Agent/Note/Tool/Result/Info/Error) and the status
  line → window content background = `tv.DefaultTheme.WindowBG` = **ANSI 4 (blue)**.
- Sidebar body/title/divider/indicator → `chromePanelBG` = **ANSI 4 (blue)** (#200).
- Desktop hint → `chromeDesktopBG` = **ANSI 4 (blue)**.

## Audit of the current default palette (WCAG ratio on ANSI-4 blue, canonical 16-colour RGB)
| role | colour | ratio | verdict |
|------|--------|-------|---------|
| User | 14 cyan | 10.8 | ok |
| Agent | 10 green | 10.0 | ok |
| Note | 8 dim grey | **1.78** | FAIL (thoughts/idle status/disabled) |
| Tool | 11 yellow | 12.5 | ok |
| Result | 13 magenta | 5.1 | ok |
| Info | 12 bright blue | **2.61** | FAIL (the culprit) |
| Error | 9 bright red | 4.23 | marginal (< 4.5, > 3.0) |
| chrome panel/desktop FG | 7 grey | 5.7 | ok |
| chrome title | 15 white | 13.3 | ok |
| chrome divider | 8 dim grey | **1.78** | FAIL (borders, non-text) |
| chrome accent | 11 yellow | 12.5 | ok |

## Thresholds (documented)
- `minContrastText = 4.5` — WCAG AA for normal-weight body text (the target).
- `minContrastLarge = 3.0` — WCAG AA for large/bold text and non-text UI
  components (borders, indicators). The floor **every** role must clear. Terminal
  cells are large and transcript headers are bold, so a role the 16-colour gamut
  pins just under 4.5 (the red error role) still reads clearly at this tier.

## Fixes (default palette only — dark/high-contrast unchanged)
- `colorNote` ANSI 8 → **ANSI 7** (light grey): the dim/secondary/disabled role.
  1.78 → 5.7. Grey stays the right convention for disabled controls and idle status.
- `colorInfo` ANSI 12 → **ANSI 6** (cyan): nearest readable cool hue to the original
  blue (honours "same hue family"), distinct from grey note and bright-cyan user.
  2.61 → 4.64 (clears AA body).
- `chromeDivider` ANSI 8 → **ANSI 7** (light grey): visible sidebar borders. 1.78 → 5.7.
- `colorError` ANSI 9 (bright red) **kept**: it is the reddest hue the 16-colour gamut
  offers (a purer/darker red scores worse; a lighter salmon degrades back to ANSI 9 on
  16-colour terminals). 4.23:1 clears the large/bold tier (error headers are bold).

Resulting distinct semantic indices: 14, 10, 7, 11, 13, 6, 9 (all pairwise distinct).

## New source interfaces (ui/tui/theme.go)
- `colorRGB(c tui.Color) (r,g,b uint8, ok bool)` — resolve ANSI/RGB to RGB (ANSI via
  the existing canonical table); ok=false for the terminal default.
- `relativeLuminance(r,g,b uint8) float64` — WCAG relative luminance.
- `contrastRatio(fg, bg tui.Color) float64` — WCAG 2.x ratio (1.0–21.0); 0 if either
  side is the unknowable terminal default.
- `minContrastText`, `minContrastLarge` constants.
- `contrastFinding{Role, FG, BG, Ratio, Min}` with `OK() bool`.
- `paletteContrast(t Theme, windowBG tui.Color) []contrastFinding` — the audit:
  one finding per painted foreground, each against its real background and tier.

## What the tester (GLM) targets
- `addNote` paints in `colorInfo`; assert the resolved default `colorInfo` (now ANSI 6)
  has contrast ≥ `minContrastLarge` (ideally ≥ `minContrastText`) on the window
  background (ANSI 4), and is **not** the old low-contrast ANSI 12.
- `paletteContrast(defaultPalette(), tui.ANSIColor(4))` (or `baseTVTheme.WindowBG`):
  every finding's `OK()` is true — the default palette passes the documented minimum.
- Optionally: `contrastRatio` sanity (white/black = 21, equal colours = 1), and that
  the high-contrast palette also passes on its black background.
