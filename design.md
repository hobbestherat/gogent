# Design — Show working directory on the status line (gogent #551)

**Branch:** `pair2/status-line-show-working-directory-for-g`
**Issue:** #551 (kloune) — "Show working directory on the status line (right-aligned, less-dim color)"
**Type:** Feature (awareness affordance). gogent-only. stdlib-first. No new deps, no `go.mod` bump, **no turbotui change**.

## Goal (restated)

Render the session's `WorkspaceRoot` (the immutable directory where `!`-prefixed and
agent shell commands run, set once at launch) on the existing TUI status-line row,
**right-aligned**, in a **less-dim color** (`colorInfo` cyan) that stands out from the
dim idle-grey left content but stays subordinate to the green/yellow/red severity
colours. Left content (the existing `formatStatusLine` output) is unchanged; on narrow
terminals the left content truncates first (its existing segment-drop order) and the
path shortens via a `shortenPath` helper down to a small floor (~8 cols), and below a
hard minimum width the path is simply omitted so the narrowest layouts are byte-for-byte
unchanged.

Target render (wide, idle):

```
idle · 2.3s · 45 tok/s · 1.2k/890 tok · 3 turns · ctx ▰▰▱▱▱▱ 38%          ~/code/gogent
```

Narrow:

```
idle · ctx ▰▰▱▱▱ 38%                                  ~/…/agent
```

## Current state (verified in code)

- **WorkspaceRoot** is set once in `NewGogent` (`internal/gogent/gogent.go:172`) from
  `os.Getwd()` (fallback `~/.gogent/workspace`), exposed by
  `GetWorkspaceRoot()` (`gogent.go:356`). Immutable for the session.
- **Status line** is a single `*tv.Label` `sw.status`, constructed at
  `ui/tui/session_window.go:286` with `FG = colorNote`, laid out as a 1-row label
  (`session_window.go:355`). Its text is composed each refresh by `refreshStatus`
  (`session_window.go:1643`), which sets `sw.status.FG = statusColorFor(...)` and
  `sw.status.SetText(formatStatusLine(state, …, bounds.W))`. `formatStatusLine`
  (`session_window.go:3003`) fits the left content to the **full** label width;
  `statusSegments` (`:3029`) already defines the narrow-terminal drop order.
- **turbotui `Label.draw`** (`turbotv/widget_label.go:56`) paints the whole text in
  one `tui.Cell{FG: l.FG, BG: l.BG}` — there is **no per-span / styled-segment API**.
  The single primitive it uses is `surface.WriteString(x, y, text, style)`
  (`turbotv/surface.go:66`), which is public and width-aware. A component's painter is
  its `Component.DrawFn` (`turbotv/component.go:5`); turbotui already lets callers
  replace it (gogent does so elsewhere, e.g. `session_window.go:541`, `:1136`).
- **Theme reseed** on a live theme switch: `refreshTheme` calls `reseedLabel(sw.status, th)`
  (sets FG/BG/HotFG) then `sw.refreshStatus()` (`session_window.go:1255-1258`).
  `reseedLabel` does **not** touch `DrawFn`, so a custom `DrawFn` survives theme switches.
  `colorInfo` is the package var `t.Info` installed by `ApplyTheme`, so the path
  recolours live like the rest of the chrome.
- **Handlers** (`ui/tui/tui.go:36`) holds the thin backend closures; embedded wiring is
  in `cmd/embedded_handlers.go` (e.g. `ListWorkspaceFiles` at `:390`). The `Workbench`
  (`tui.go:530`) stores `handlers Handlers` and may be swapped (remote/daemon).

## Design

### 1. Path getter + cache (immutable → cache once)

- Add to `Handlers` (`ui/tui/tui.go`):
  ```go
  // GetWorkspaceRoot returns the session's working directory (the immutable
  // WorkspaceRoot where !-prefixed and agent shell commands run), shown
  // right-aligned on the status line (issue #551). May be nil — when unwired
  // the status line simply omits the path.
  GetWorkspaceRoot func() string
  ```
- Wire in embedded mode (`cmd/embedded_handlers.go`, beside `ListWorkspaceFiles`):
  ```go
  GetWorkspaceRoot: func() string { return g.GetWorkspaceRoot() },
  ```
- **Cache once on the Workbench**, not per-render: add an unexported memo on `Workbench`
  and a small accessor:
  ```go
  // workspaceRoot is the cached, immutable session working directory (issue #551),
  // resolved once on first use from handlers.GetWorkspaceRoot so the hot status
  // refresh path never re-queries the backend. "" when unwired (path is hidden).
  workspaceRoot     string
  workspaceRootOnce bool
  ```
  ```go
  func (w *Workbench) WorkspaceRoot() string {
      if !w.workspaceRootOnce {
          w.workspaceRootOnce = true
          if w.handlers.GetWorkspaceRoot != nil {
              w.workspaceRoot = w.handlers.GetWorkspaceRoot()
          }
      }
      return w.workspaceRoot
  }
  ```
  Touched only on the UI thread (same as the rest of the workbench's per-render state),
  so no lock is needed. Justified deviation from call-each-time: the value is immutable
  and the status line is a hot path. (If a future `SetHandlers` swap could change the
  root we'd re-resolve; today it cannot — the root is per-process and remote handlers
  leave the getter nil, so the cache is correct.)

### 2. `shortenPath` helper (pure, testable, shared)

`internal/`-free, in `ui/tui` (alongside `formatStatusLine`). Pure function, `home`
passed explicitly so tests are hermetic (no `os.Getenv` inside):

```go
// shortenPath renders path for the status line within maxW display columns:
// it first collapses a $HOME prefix to "~", and if the result still exceeds the
// budget keeps the first segment plus "…/" plus the trailing two segments
// (~/code/gogent/internal/agent -> ~/…/internal/agent), shortening the tail to
// a single segment and finally width-truncating it down to a small floor. Returns
// "" when maxW is below the floor so the caller can omit the path entirely.
func shortenPath(path, home string, maxW int) string
```

Stages (each guarded by `tui.StringWidth <= maxW`, returning early):
1. `~`-collapse: if `path == home` or `strings.HasPrefix(path, home+"/")`, replace the
   `home` prefix with `~`.
2. If it fits, done.
3. Head + `…/` + last **two** segments.
4. Head + `…/` + last **one** segment.
5. As a last resort, width-truncate the final segment (reusing `truncateToWidth`, the
   existing width-aware ellipsis helper) to `maxW`; below the ~8-col floor, return "".

Uses `path/filepath` only for `Separator`/splitting semantics on the stored path; splits
on `/` (the stored root is always slash-formed). No new dependency.

### 3. Two-colour render via a custom `DrawFn` (no new widget, no turbotui change)

`refreshStatus` keeps composing the **left** content exactly as today, but against a
**reduced** width that reserves room for the path; it stashes the shortened path on the
window for the painter to read:

```go
func (sw *SessionWindow) refreshStatus() {
    ...
    W := sw.status.Component.Bounds.W
    leftW := W
    sw.statusPath = ""
    if root := sw.wb.WorkspaceRoot(); root != "" && W >= minStatusWidthForPath {
        budget := pathBudget(W)                       // clamp(W/3, pathFloor=8, pathMax)
        p := shortenPath(root, homeDir(), budget)
        if pw := tui.StringWidth(p); pw > 0 && W-pw-statusPathGap >= minLeftWidth {
            sw.statusPath = p
            leftW = W - pw - statusPathGap            // statusPathGap = 2 (>=1 visible gap)
        }
    }
    sw.status.SetText(formatStatusLine(state, sw.statusStats, live, budget, leftW))
    ...
}
```

Reserving `leftW = W - pathW - gap` means the left content truncates (via the existing
segment-drop / `truncateToWidth` logic in `formatStatusLine`) to leave a guaranteed gap
before the path — **the left/path columns can never collide**. When `W` is below
`minStatusWidthForPath`, or the path would crowd the left below `minLeftWidth`, the path
is dropped and the left content uses the full width — i.e. **byte-for-byte the pre-#551
behaviour** on the narrowest windows.

A `statusPath string` field is added to `SessionWindow`. The painter is installed once at
construction (right after `status := tv.NewLabel(...)`), replacing the label's default
single-colour `DrawFn`:

```go
status.Component.DrawFn = func(c *tv.VisualComponent, surface tv.Surface) {
    abs := c.AbsoluteBounds()
    if abs.W < 1 || abs.H < 1 {
        return
    }
    // Left content in the severity/idle colour the label already owns.
    surface.WriteString(abs.X, abs.Y, status.GetText(), tui.Cell{FG: status.FG, BG: status.BG})
    // Right-aligned path in the less-dim chrome colour (issue #551).
    if sw.statusPath != "" {
        x := abs.X + abs.W - tui.StringWidth(sw.statusPath)
        surface.WriteString(x, abs.Y, sw.statusPath, tui.Cell{FG: colorInfo, BG: status.BG})
    }
}
```

Notes:
- `surface.WriteString` is exactly the primitive `Label.draw` uses, and it clips to the
  surface, so the path can never overdraw a neighbouring widget.
- The status label carries no mnemonic and is a single row, so dropping `Label.draw`'s
  Wrap/mnemonic branches loses nothing for this widget.
- `status.FG` is still set by `refreshStatus` via `statusColorFor` — **the left-side
  severity/idle/working/background colour logic is entirely unchanged**; only the path
  gets the independent `colorInfo`.
- `colorInfo` and the theme reseed: `reseedLabel` leaves `DrawFn` intact, and `colorInfo`
  is updated by `ApplyTheme`, so a live theme switch recolours both halves correctly.

### Constants (with the other status-line tunables near `formatStatusLine`)

- `statusPathGap = 2` — min visible columns between left content and the path.
- `pathFloor = 8` — minimum path budget (the "~/…/x" floor).
- `pathMax` (~28) — cap so the path never eats more than its share on very wide terminals.
- `minLeftWidth` (~12) — the left content keeps at least the state plus a stat.
- `minStatusWidthForPath` (~24) — below this the path is omitted entirely.

(Exact values finalised during implementation against the wide/narrow test fixtures.)

## Files touched

**gogent (only):**
- `ui/tui/tui.go` — add `Handlers.GetWorkspaceRoot`; add `Workbench.workspaceRoot`
  memo + `WorkspaceRoot()` accessor.
- `cmd/embedded_handlers.go` — wire `GetWorkspaceRoot: g.GetWorkspaceRoot`.
- `ui/tui/session_window.go` — add `SessionWindow.statusPath`; install the custom status
  `DrawFn` at construction; reserve `leftW` and set `sw.statusPath` in `refreshStatus`;
  add `shortenPath`, `pathBudget`, `homeDir` helpers and the constants.
- Tests (new `*_test.go` in `ui/tui`): `shortenPath` cases; wide/narrow status render;
  severity-unchanged guard; embedded `GetWorkspaceRoot` wiring.

**turbotui:** none. The design uses the existing public `surface.WriteString` primitive
and the existing `Component.DrawFn` override seam.

## User-facing behaviour

- Every live session window shows its working directory right-aligned on the status row,
  in cyan, always visible and stable for the session.
- It is readable but subordinate: severity (budget/context) colours still own the left
  content; the path stays cyan regardless.
- No layout shift: the path lives on the existing status row; no new widget, no extra row,
  the input/transcript geometry is untouched.
- On narrow windows the left stats drop first (unchanged order); the path shortens to
  `~/…/tail` and then disappears below the minimum width — never overlapping the left text.
- Read-only analysis windows (no status chrome) and remote/daemon mode (getter nil) simply
  show no path — no error, no change to those surfaces.

## Design criteria

**(1) Goal match.** Does exactly what #551 asks: right-aligned, `colorInfo`-cyan,
shortened `WorkspaceRoot` on the status line; left content unchanged; narrow truncation
with floor. No scope creep — the `!`-prefix shell-out dispatch (`handleBangCommand`), the
per-command transcript echo, and any path-change tracking are explicitly **out of scope**
(the root is immutable). No refactor of the status pipeline beyond the width split.

**(2) Usability.** The cwd is always visible (the awareness affordance the upcoming
`!`-shell-out and `@`-mentions need), readable yet subordinate to severity colours, with
no layout shift and graceful narrow-terminal degradation. It is purely informational —
nothing to drive — so there is no input/interaction share to get wrong; the right thing is
surfaced rather than left silent.

**(3) No regressions.** No new widget/component; `statusColorFor` and the left-side
severity/idle/working/background logic are untouched; `formatStatusLine`'s signature and
behaviour are unchanged (it is merely called with a reserved width). `reseedLabel` leaves
`DrawFn` intact so theme switches still work; `colorInfo` recolours live via `ApplyTheme`.
The path painter clips via `surface.WriteString`, so no overdraw. Below the minimum width
the render is identical to today. Existing status-line tests keep passing (left content is
the same string they assert when the path is absent/narrow). Nil getter (remote/daemon,
analysis window) → no path, no panic. `gofmt`/`build`/`vet`/`golangci-lint` clean;
`go test ./...` green (pre-existing `TestUserSessionSendMessage` 404 the only acceptable
failure).

**(4) Holistic across both repos.** The change is entirely in gogent, in the right layer:
the getter is a thin `Handlers` closure mirroring `ListWorkspaceFiles`/`ReadWorkspaceFile`;
the render reuses turbotui's existing `Component.DrawFn` seam and the public
`surface.WriteString` primitive. **The repo seam is respected — turbotui needs no new API**
(it already exposes `DrawFn` replacement and `WriteString`, the same primitive its own
`Label.draw`, `menu.go`, and widgets use). No `go.mod` bump, no new dep. Downstream effect
on turbotui: none. The two-colour-on-one-label pattern is consistent with how gogent
already overrides `DrawFn` for window/button/separator chrome.

## Regression risks (and mitigations)

- **Left/path collision** → eliminated by reserving `leftW = W - pathW - gap`; the left
  content is truncated by the existing width-aware logic before it can reach the path.
- **Theme switch dropping the custom colour** → `reseedLabel` never touches `DrawFn`, and
  `colorInfo` is a live theme var; verified by reading `refreshTheme`.
- **Existing status-line tests asserting full-width left content** → the path is reserved
  only when wired and wide enough; tests that build a `SessionWindow` without a
  `GetWorkspaceRoot` handler, or at narrow widths, see the unchanged string. New tests
  cover the with-path case explicitly.
- **Hot-path cost** → the root is cached once on the Workbench; `shortenPath` is a handful
  of string ops on a short path per refresh (cheap, same order as the existing segment
  formatting).
- **Wide-glyph / multibyte paths** → all width math goes through `tui.StringWidth` and the
  width-aware `WriteString`/`truncateToWidth`, matching the rest of the status line.

## Open questions

1. **Path budget share on very wide terminals.** Proposed `pathBudget = clamp(W/3, 8, ~28)`.
   `~28` keeps a deep path like `~/…/internal/agent` intact while never letting the path
   dominate. Happy to tune the cap, but a cap is needed so the path doesn't crowd stats on
   ultra-wide terminals (note recent #559 added a `comfortableMaxWidth=120` cap for
   windows — the path cap is independent of that and lives on the status row).
2. **Tail depth.** Design keeps the **last two** segments (`~/…/internal/agent`). Issue art
   shows `~/…/agent` (last one) at the narrow floor — both are covered by the staged
   `shortenPath` (two segments when they fit, one when they don't). Confirm two-segment
   tail is the preferred wide form.
3. **`~`-collapse home source.** Use `os.UserHomeDir()` (falling back to `$HOME`) resolved
   once for the live render; the helper takes `home` as a param for hermetic tests. Assumed
   acceptable; flag if a different home convention is wanted (e.g. respecting a configured
   workspace home).
