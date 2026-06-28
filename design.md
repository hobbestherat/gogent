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

### 1. Path getter (no cache — read the live handler each refresh)

- Add to `Handlers` (`ui/tui/tui.go`):
  ```go
  // GetWorkspaceRoot returns the session's working directory (the immutable
  // WorkspaceRoot where !-prefixed and agent shell commands run), shown
  // right-aligned on the status line (issue #551). May be nil — when unwired
  // (remote/daemon handlers, analysis windows) the status line omits the path.
  GetWorkspaceRoot func() string
  ```
- Wire in embedded mode (`cmd/embedded_handlers.go`, beside `ListWorkspaceFiles`):
  ```go
  GetWorkspaceRoot: func() string { return g.GetWorkspaceRoot() },
  ```
- **No cache.** Add a thin nil-safe accessor on `Workbench` that reads the *current*
  handler every time:
  ```go
  // WorkspaceRoot returns the live session working directory for the status line
  // (issue #551), or "" when the getter is unwired. Read on each status refresh
  // rather than memoised: the underlying getter (g.GetWorkspaceRoot) is a single
  // struct-field return, and the Handlers can be swapped at runtime (SetHandlers
  // is called from cmd/attach.go and cmd/handoff.go during a daemon attach/handoff),
  // so a cache would risk showing a stale root after a handoff for no measurable gain.
  func (w *Workbench) WorkspaceRoot() string {
      if w.handlers.GetWorkspaceRoot == nil {
          return ""
      }
      return w.handlers.GetWorkspaceRoot()
  }
  ```
  This was the original design's one real defect: an earlier draft memoised on first
  read and justified it with "the handlers cannot swap after caching." That is **false**
  — `SetHandlers` is invoked at runtime by `cmd/attach.go:208` and twice in
  `cmd/handoff.go` (`:284`, `:387`), so a daemon attach/handoff *does* replace the
  handlers after the first render. A memo would then pin the pre-handoff root and the
  status line would point at the wrong directory. Since `g.GetWorkspaceRoot()` is a free
  field read (`internal/gogent/gogent.go:357`), dropping the cache removes the
  staleness hazard at zero cost. Called only on the UI thread, like the rest of the
  per-render state, so no lock is needed.

### 2. `shortenPath` helper (pure, testable, cross-platform)

`internal/`-free, in `ui/tui` (alongside `formatStatusLine`). Pure function, `home`
passed explicitly so tests are hermetic (no `os.Getenv` inside):

```go
// shortenPath renders path for the status line within maxW display columns:
// it first normalises separators to "/" and collapses a home prefix to "~", and
// if the result still exceeds the budget keeps the first segment plus "…/" plus
// the trailing two segments (~/code/gogent/internal/agent -> ~/…/internal/agent),
// shortening the tail to a single segment and finally width-truncating it down to
// a small floor. Returns "" when maxW is below the floor so the caller can omit
// the path entirely.
func shortenPath(path, home string, maxW int) string
```

**Cross-platform first.** The stored root comes from `os.Getwd()` /
`filepath.Join(...)` (`internal/gogent/gogent.go:172-176`), so it carries OS separators
— backslashes on Windows, which gogent supports (`signal_windows.go`,
`shell_windows_test.go`). An earlier draft claimed the root is "always slash-formed" and
split on `/`; that is **false on Windows** — the `~`-collapse and segment split would both
fail and the line would show a raw, overflowing `C:\Users\...`. The helper therefore
normalises **both** `path` and `home` with `filepath.ToSlash` up front and operates purely
on `/`-separated strings thereafter. (Display is `/`-formed even on Windows, which is the
conventional, readable form for this kind of affordance.)

Stages (each guarded by `tui.StringWidth <= maxW`, returning early):
1. `ToSlash` both `path` and `home`.
2. `~`-collapse: if `path == home` or `strings.HasPrefix(path, home+"/")`, replace the
   `home` prefix with `~`. (Empty `home` skips this — never collapse on `""`.)
3. If it fits, done.
4. Head + `…/` + last **two** segments.
5. Head + `…/` + last **one** segment.
6. Last resort: width-truncate the final segment with `tv.Truncate(seg, maxW, "…")` — the
   turbotui width-aware ellipsis helper (`turbotv/surface.go:116`), so the floor fallback
   shows a trailing `…` consistent with the `…/` middle stages rather than a bare clip.
   Below the ~8-col floor, return "".

(Note: gogent's local `truncateToWidth`, `permission_dialog.go:595`, is a **bare** clip
with no ellipsis — it is deliberately *not* used here; `tv.Truncate` is the right tool for
the ellipsis we want.) No new dependency — `path/filepath` and `tv` are already imported.

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
        pb := pathBudget(W)                           // clamp(W/3, pathFloor=8, pathMax)
        p := shortenPath(root, homeDir(), pb)         // NB: `budget` is already the BudgetConfig
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
- **Dead `Wrap`/mnemonic state, accepted explicitly.** `tv.NewLabel` defaults `Wrap = true`
  and computes a mnemonic, but the custom `DrawFn` supersedes `Label.draw` entirely, so
  for `sw.status` both become inert. This is correct *for this widget*: it is a single row
  (`H == 1`, so wrapping never applied anyway) and its text (`idle`, `working...`, stats)
  contains no `&` mnemonic. The reviewer should sign off that the status label no longer
  honours `Label.Wrap` — it never meaningfully did at one row, and no caller toggles it.
- `status.FG` is still set by `refreshStatus` via `statusColorFor` — **the left-side
  severity/idle/working/background colour logic is entirely unchanged**; only the path
  gets the independent `colorInfo`.
- `colorInfo` and the theme reseed: `reseedLabel` leaves `DrawFn` intact, and `colorInfo`
  is updated by `ApplyTheme` (`theme.go:1180`), so a live theme switch recolours both
  halves correctly.

**Path colour vs. the left colour — the one collision state, accepted.** The path uses
`colorInfo` (cyan), which is brighter/less-dim than the `colorNote` grey of the idle left
content — the contrast the issue asks for. There is exactly **one** state where the two
match: a session running *only* background sub-agents, where `statusColorFor`
(`session_window.go:3120-3126`) already substitutes `colorInfo` for the whole left line
(the `background && !busy && c == colorNote` branch). In that state the path is the same
cyan as the left text. This is **accepted, not overlooked**: (a) it is transient and rare
(background-only, no foreground turn, no budget/context severity); (b) the path stays
legible — it is separated by the reserved `statusPathGap` and is distinct text flush-right;
(c) the path is an *awareness* affordance, not a severity signal, so it never needs to win
attention over the left content. In every other state — idle (grey), working (green),
approaching/over budget or context (amber/red) — the cyan path is clearly distinct. If a
reviewer wants zero collision, the alternative is a dedicated theme role (e.g. a
`StatusPath` colour) instead of reusing `colorInfo`; that is a larger surface (new Theme
field, override key, degrade line, contrast audit) and is deferred unless requested (see
Open questions).

### Constants (with the other status-line tunables near `formatStatusLine`)

- `statusPathGap = 2` — min visible columns between left content and the path.
- `pathFloor = 8` — minimum path budget (the "~/…/x" floor).
- `pathMax` (~28) — cap so the path never eats more than its share on very wide terminals.
- `minLeftWidth` (~12) — the left content keeps at least the state plus a stat.
- `minStatusWidthForPath` (~24) — below this the path is omitted entirely.

(Exact values finalised during implementation against the wide/narrow test fixtures.)

## Files touched

**gogent (only):**
- `ui/tui/tui.go` — add `Handlers.GetWorkspaceRoot`; add the thin nil-safe
  `Workbench.WorkspaceRoot()` accessor (no cache — reads the live handler).
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
- Read-only analysis windows (no status chrome) and remote/daemon mode (getter nil) show
  no path — no error, no change to those surfaces. **Known gap, not a feature:** an
  attached/remote user — arguably the population that most wants a "where am I" cue — gets
  nothing in v1, because the remote handlers do not currently expose the daemon's
  workspace root. Surfacing it there means threading the root through the remote
  `Handlers` (an RPC field), which is out of scope for this gogent-only, no-protocol-change
  change; tracked as a follow-up (see Open questions).

## Design criteria

**(1) Goal match.** Does exactly what #551 asks: right-aligned, `colorInfo`-cyan,
shortened `WorkspaceRoot` on the status line; left content unchanged; narrow truncation
with floor. No scope creep — the `!`-prefix shell-out dispatch (a future addition to the
`submit` closure at `session_window.go:400`; there is no `!`-dispatch in `submit` today),
the per-command transcript echo, and any path-change tracking are explicitly **out of
scope** (the root is immutable). No refactor of the status pipeline beyond the width split.

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
the render is identical to today. **No stale-root hazard:** the getter is read live on
each refresh (not memoised), so a runtime `SetHandlers` swap from a daemon attach/handoff
(`cmd/attach.go:208`, `cmd/handoff.go:284`/`:387`) is reflected immediately. **Windows
safe:** `shortenPath` normalises separators via `filepath.ToSlash`, so a `\`-separated
root collapses and shortens correctly rather than overflowing. Existing status-line tests
keep passing (left content is the same string they assert when the path is absent/narrow;
none of the ~80 `SetHandlers` test sites wire `GetWorkspaceRoot`, so all see the
path-absent path). Nil getter (remote/daemon, analysis window) → no path, no panic.
`gofmt`/`build`/`vet`/`golangci-lint` clean; `go test ./...` green (pre-existing
`TestUserSessionSendMessage` 404 the only acceptable failure).

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
- **Stale root after a daemon attach/handoff** → eliminated by *not* caching: `WorkspaceRoot()`
  reads the live handler each refresh, so a runtime `SetHandlers` swap is picked up. The
  getter is a single field read, so the live call is effectively free; `shortenPath` is a
  handful of string ops on a short path per refresh (same order as the existing segment
  formatting).
- **Windows `\`-separated root** → `shortenPath` `ToSlash`-normalises `path` and `home`
  before collapse/split, so the home-collapse and segment logic work on Windows; covered by
  a Windows-path test fixture.
- **Wide-glyph / multibyte paths** → all width math goes through `tui.StringWidth` and the
  width-aware `WriteString` / `tv.Truncate`, matching the rest of the status line.
- **Floor ellipsis consistency** → the floor fallback uses `tv.Truncate(seg, maxW, "…")`
  (not the bare-clip `truncateToWidth`), so a truncated tail shows a trailing `…` like the
  `…/` middle stages.

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
   per render for the live path; the helper takes `home` as a param for hermetic tests.
   Assumed acceptable; flag if a different home convention is wanted (e.g. respecting a
   configured workspace home).
4. **Path colour: reuse `colorInfo`, or a dedicated role?** The design reuses `colorInfo`
   and accepts the single background-only state where the left content is also `colorInfo`
   (legible via the gap + flush-right position). A zero-collision alternative is a new
   `StatusPath` theme role (new Theme field + override key + degrade line + contrast audit
   against `WindowBG`). Reusing `colorInfo` is the lighter, recommended call; confirm before
   I add a whole new role.
5. **Remote/attach follow-up.** Surfacing the daemon's workspace root to attached/remote
   users needs the remote `Handlers` to carry the root (an RPC/protocol field). Out of scope
   here (gogent-only, no protocol change). Worth a follow-up issue — confirm that's the right
   disposition rather than blocking v1 on it.
