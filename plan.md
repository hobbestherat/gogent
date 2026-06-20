# Issue #190 — Split the "Sessions & Agents" sidebar into three regions

## Goal
Reorganise `ui/tui/sidebar.go` into three vertical regions, top → bottom:
1. **TOP** — session tree + sub-agent nodes, **minus** TODO nodes.
2. **MIDDLE** — per-session TODO checklist for the **focused** session only.
3. **BOTTOM** — existing Overall stats band (unchanged content).

## Key facts (grounding)
- `VisualComponent.Draw` runs `LayoutFn` *before* `DrawFn` on every redraw, so a
  content-dependent region height computed in `LayoutFn` is always fresh at draw
  time. No manual relayout needed when todos/focus change.
- `s.todos` is the source of truth; today it holds `*tv.TreeNode`s only so they
  can be embedded under the session node. Retire that — store `[]agent.TodoItem`.
- Overall band counts use `len(s.sessions)` / `len(s.agents)` — TODOs already
  excluded; keep that.

## Changes

### `sidebar.go`
1. **`s.todos` type** → `map[string][]agent.TodoItem` (data, not tree nodes).
   Add `focused string` (focused session id) and `todosBandH int` (resolved
   middle-region height, mirror `overallBandH`).
2. **`applyTodo`** — stop mutating `parent.Children`; just store/clear
   `s.todos[sessionID]`. Keep the unknown-session guard. Delete `excludeNodes`
   (now unused) and the tree-node `todoLabel` stays (reused for drawing).
3. **`focusSession(id)`** — setter for the focused session; the middle region
   follows it (mirrors the Overall band's focus-following, issue #107).
4. **`removeSession`** — also clear `s.focused` if it was the removed session.
5. **Region sizing helpers** —
   - `maxTodoRegionItems` cap constant.
   - `todoLineCount()` = `min(len(focused todos), cap)`; 0 when empty.
   - `todosRegionHeight()` = `0` when empty, else `1 (title) + todoLineCount()`.
6. **`LayoutFn`** — extend the split. Compute `bandH` exactly as today (band
   dropped if tree would fall below `minSidebarTreeHeight`), then compute
   `todosH` and drop it if the tree would fall below the minimum after both
   regions. Tree height = `h - 1 - bandH - todosH`. Precedence: tree wins, TODO
   region drops before the band. With empty todos `todosH==0`, so existing
   band-split behaviour is byte-for-byte unchanged.
7. **`drawTodos`** in `DrawFn` (next to `drawOverall`) — title row "TODOs" plus
   one clipped row per item (`truncateRunes(todoLabel(it), contentW)`), placed
   directly above the Overall band. No-op when `todosBandH<=0`.
8. **`todoStatusIcon`** — distinct glyphs from sub-agent `statusIcon`:
   pending `☐`, in-progress `◐`, completed `☑` (vs sub-agent `▶ ‖ ✓ ✗ •`).

### `tui.go`
- `Workbench.Focus` calls `w.sidebar.focusSession(id)` so the middle region
  tracks the raised session.

## Tests (GLM partner writes them)
Interfaces to target: `s.todos` (now `[]agent.TodoItem`), `s.focused` /
`focusSession`, `s.todosBandH`, `todosRegionHeight()`, `applyTodo` not touching
`parent.Children`, Overall sub-agent count still `len(s.agents)`. Update
`TestSidebarOverallBandReservation` for the third region; add: todos render in
the middle (not as tree children); short sidebar drops todo/overall first.
