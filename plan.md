# Issue #215 — Disable drop shadows toggle

## Goal
Add a persisted, live-applied "Disable shadows" setting to the theme dialog that
removes drop shadows from every gogent-built surface (windows/dialogs, menu bar,
buttons). Default unchanged (shadows on).

## Design

Shadows are per-widget booleans in turbotui (`Window.Shadow`, `Button.Shadow`,
`MenuBar.Shadow`), all defaulting to `true` at construction; there is no upstream
global toggle and we add no deps. So gogent carries a single preference and sets
each widget's `Shadow` flag from it — at construction *and* on the live
theme-apply path so a toggle takes effect without a restart.

### 1. Config (`internal/config/config.go`)
- Add `NoShadow bool` to `ThemeConfig`, json `no_shadow,omitempty` (default
  false = shadows on), documented next to `NoColor`.

### 2. Theme plumbing (`ui/tui/theme.go`)
- Add `NoShadow bool` to the resolved `Theme`; `ResolveTheme` copies it from
  `cfg.NoShadow`.
- Package var `shadowsEnabled = true`; `ApplyTheme` sets
  `shadowsEnabled = !t.NoShadow` (same runtime apply path as the palette, which
  #204 wired to `tv.SetTheme` + `RefreshTheme`).
- Helpers consulting `shadowsEnabled`:
  - `applyWindowShadow(*tv.Window)`
  - `applyButtonShadow(*tv.Button)`
  - `applyMenuBarShadow(*tv.MenuBar)`
  - `newButton(...)` — wraps `tv.NewButton` then `applyButtonShadow`, so every
    gogent button honours the preference uniformly.

### 3. Theme editor (`ui/tui/theme_editor.go`)
- Add a "Disable &shadows" checkbox mirroring the NO_COLOR toggle (seeded from
  `cur.NoShadow`, cleared on Reset).
- `buildThemeConfig(preset, noColor, noShadow, specs)` records `NoShadow`.

### 4. Apply at every surface
- Session windows (`session_window.go`): `applyWindowShadow` at construction +
  in `refreshTheme`; buttons via `newButton` + `reseedButton` sets `b.Shadow`.
- Menu bar (`tui.go` build): `applyMenuBarShadow(bar)` (rebuilt by `RefreshTheme`).
- Every `tv.NewDialog` site + the monologue window: `applyWindowShadow`.
- All `tv.NewButton(` → `newButton(` so dialog buttons honour it too.

## Tests (GLM partner)
- Config round-trip of `NoShadow`.
- After `ApplyTheme(ResolveTheme(ThemeConfig{NoShadow:true},…))`: a built window,
  the menu bar and a `newButton` have `Shadow == false`; default keeps them true.
- `refreshTheme`/`reseedButton` clear flags on live session windows/buttons.

## Constraints
gofmt, golangci-lint 0, no new deps, Go tests without `-race`.
