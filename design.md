# Design — Issue #490 (gogent half): collapse the sub-agent summary into ONE line with a trailing `+`/`-`

## Summary of the change

Today a session with sub-agents grows **two** synthetic rows:

1. `statusNode` — an always-first **leaf** `[▶2 ‖1 ✓5 ✗1]` (the bracketed totals), and
2. `bucketNode` — a separate `[✓ N]` row with a native `▸`/`▾` marker that *parents* the
   TTL-folded completed agents.

Both summarise the same completed set, so the `✓` total is printed twice. Issue #490 collapses
them into **one** line: the totals bracket itself becomes the parent of the archived (folded)
completed agents, carries **no** leading `▸`/`▾` marker, and gains a **trailing** `+` (collapsed,
archived hidden) / `-` (expanded, archived shown) suffix — with **no** suffix while it has no
archived children.

The change is confined to:

- `ui/tui/sidebar.go` — the fold data structure + its helpers.
- `ui/tui/sidebar_fold_issue484_test.go` — rewritten to the single-node + suffix model.
- `go.mod` / `go.sum` — bump turbotui to the merged #490 commit.
- `ui/tui/keybindings_issue401_test.go:175` — the turbotui pin-guard string (mechanical).

`internal/agent`, `server`, and the remote handlers are **untouched** (the #484 invariant: folding
is a pure UI-mirror visibility concern; the shared agent tree, slot counting, `ActiveSubAgentCount`,
and `ListAllAgents` never see it).

## Build step (run first, before any implementation)

```
go get github.com/hobbestherat/turbotui@877fd6224b7ddb9c5ecf099d78afbb5381a71714 && go mod tidy
```

Then update the **pin-guard** test to the pseudo-version `go.mod` now records. The old string lives
in exactly one place:

- `ui/tui/keybindings_issue401_test.go:175` — `"github.com/hobbestherat/turbotui v0.3.1-0.20260626065139-7db1e2fafccc"`

The commit `877fd62` is dated `2026-06-26T21:02:20+02:00` = `19:02:20Z`, so `go.mod` will record
`v0.3.1-0.20260626190220-877fd6224b7d`. **Do not hard-code from this note** — read the exact string
`go.mod` writes after `go get`/`go mod tidy` and paste that verbatim into the guard test (and the
`go.sum` lines update automatically).

## turbotui API confirmed after the bump (read from the read-only clone @ 877fd62)

`turbotv/widget_tree.go` shipped exactly the two primitives this design needs:

- **`TreeNode.HideMarker bool`** — when a node *has* children, suppresses its leading `▸`/`▾`,
  painting a blank leading column while keeping indentation. It does **not** affect `flatten()`
  (`Expanded` + `Children` still drive show/hide); it only changes the marker glyph. Leaves render
  blank regardless. (`draw()` guard: `if len(r.node.Children) > 0 && !r.node.HideMarker { … }`.)

- **`Tree.OnToggle func(node *TreeNode, ev tui.ClickEvent) bool`** — offered on each committed row
  click **before** the default marker-column logic. Returning **true** consumes the click as a
  toggle: the host flips `Expanded` itself, and the widget then skips its own marker toggle **and**
  the repeat-click `OnActivate`. Returning **false** falls through to default behaviour. It is
  offered for **every** row click, so a handler must self-filter (e.g. on `node.Data` /
  `node.HideMarker`).

Important keyboard fact (from `handleType`): `Left`/`Right`/`Space` flip `Expanded` **natively**
for any node with children and **do not** call `OnToggle` (no host hook on the keyboard path). The
widget only knows how to paint `▸`/`▾` — which we hide — so after a keyboard toggle the host's
`+`/`-` suffix would go stale unless the host re-derives it. See "Suffix refresh" below.

## gogent changes — file `ui/tui/sidebar.go`

### 1. `sessionFold` struct
Replace `statusNode` + `bucketNode` with a single `summaryNode *tv.TreeNode`:

```go
type sessionFold struct {
    summaryNode *tv.TreeNode          // always-first synthetic child: totals bracket + parent of archived agents
    entries     map[string]*foldEntry
}
```

`foldEntry` is unchanged (`status`, `finishedAt`, `folded`, `dismissed`).

### 2. `syntheticRef`
Drop the now-meaningless `bucket bool` field — there is only one synthetic node per session now, so
`syntheticRef{sessionID}` alone marks the summary row. Its role is unchanged: it is **not** a
`nodeRef`, so the existing `OnSelect` / `OnSelectMouse` / `OnActivate` handlers (which type-assert
`nodeRef` and bail otherwise) keep treating the row as **inert** — preserving the issue #302
contract that a body click never pops a monologue / raises a window.

### 3. `ensureFold(sessionID, parent)`
Create the single summary node at child index 0:

```go
summary := tv.NewTreeNode("")
summary.Data = syntheticRef{sessionID: sessionID}
summary.HideMarker = true   // never paint a leading ▸/▾, even once it parents archived agents
summary.Expanded = false    // collapsed by default; childless so no suffix yet
parent.Children = append([]*tv.TreeNode{summary}, parent.Children...)
```

It is a **parent** node, initially childless. While childless it renders as just the totals bracket
with **no** suffix (empty-bucket rule). The separate bucket node is gone.

### 4. `syntheticPrefixLen` / `insertVisibleAgent`
The synthetic prefix is now exactly the summary node (archived agents live *under* it, not as
siblings), so `syntheticPrefixLen` returns `1` when `summaryNode` is `parent.Children[0]`, else `0`.
`insertVisibleAgent` inserts visible agent rows right after it (unchanged logic, simpler prefix).
The session's visible child list is therefore `[summaryNode, …visibleAgents, …watchers]`, and
archived completed agents are `summaryNode.Children`.

### 5. `foldAgent(fold, parent, node)`
Move the completed agent's node **under `summaryNode`** instead of a separate bucket:

```go
firstFold := len(fold.summaryNode.Children) == 0
removeChild(parent, node)
fold.summaryNode.Children = append(fold.summaryNode.Children, node)
if firstFold {
    fold.summaryNode.Expanded = false   // collapsed by default once non-empty (→ "+" suffix)
}
```

Only the first fold forces collapse; later folds leave the user's last expand state alone (matches
current bucket behaviour). No node creation/attachment here — the summary node already exists.

### 6. `unfoldAgent(fold, parent, node)`
Move the agent back to the visible list:

```go
removeChild(fold.summaryNode, node)
s.insertVisibleAgent(fold, parent, node)
```

**Do not** detach the summary node when it empties — unlike the old bucket, the summary node is also
the always-present status bar. A now-childless summary simply reverts to a plain bracket with **no**
suffix (handled by `refreshFoldChrome`). Teardown of the summary node happens only in
`refreshFoldChrome` when *no entries remain at all*.

### 7. `refreshFoldChrome(sessionID)`
- If `len(fold.entries) == 0`: tear down the single `summaryNode` (`removeChild(parent, summaryNode)`)
  and `delete(s.folds, sessionID)` — returns the row to its clean pre-agent state. (Same trigger as
  today, one node instead of two.)
- Else: compute `running/waiting/completed/failed` exactly as today (✓ **includes** folded; ✗
  **excludes** dismissed), then:

  ```go
  fold.summaryNode.Label = statusBarLabel(running, waiting, completed, failed) + summarySuffix(fold.summaryNode)
  ```

  Drop the separate `[✓ N]` label entirely — the totals line **is** the summary.

New helper:

```go
// summarySuffix returns "" when the summary parents no archived agents, "+" when it has archived
// children and is collapsed, "-" when expanded. The +/- is the only expand affordance (the leading
// ▸/▾ is hidden via HideMarker).
func summarySuffix(n *tv.TreeNode) string {
    if len(n.Children) == 0 { return "" }
    if n.Expanded { return "-" }
    return "+"
}
```

(Append directly, no space: `[▶2 ‖1 ✓5 ✗1]+`, matching the issue title `[...]+/[...]-`.)

### 8. Click toggle — `tree.OnToggle` (added in `newSidebar`)
```go
tree.OnToggle = func(n *tv.TreeNode, _ tui.ClickEvent) bool {
    ref, ok := n.Data.(syntheticRef)
    if !ok {
        return false              // real rows: default marker / activate behaviour
    }
    if len(n.Children) == 0 {
        return false              // childless summary: inert, nothing to toggle
    }
    n.Expanded = !n.Expanded
    s.refreshFoldChrome(ref.sessionID)   // flips the +/- suffix immediately
    return true                   // consume: suppress marker toggle + repeat-click OnActivate
}
```

A click anywhere on the summary row toggles it (the row is otherwise inert — it has no monologue to
open), and the visible `+`/`-` advertises the affordance. Real session rows (which keep a genuine
`▸`/`▾`) and agent rows fall through to default behaviour untouched. `OnSelect`/`OnSelectMouse`/
`OnActivate` are unchanged and still bail on `syntheticRef`, so the body click never opens a
monologue or raises a window (issue #302 preserved). `Enter` on the summary row is a no-op
(`OnActivate` bails on `syntheticRef`).

### 9. Suffix refresh on keyboard / programmatic toggle — draw-time reconcile
Keyboard `Left`/`Right`/`Space` flip `Expanded` with no host hook, and the widget can only repaint
the (hidden) `▸`/`▾`. So the `+`/`-` suffix must be re-derived from the live `Expanded` at paint
time. Add a tiny reconcile at the **top of `panel.DrawFn`** (the panel draws before its tree child,
so labels are correct before the tree reads them):

```go
for _, fold := range s.folds {
    if fold.summaryNode == nil { continue }
    base := fold.summaryNode.Label
    if i := strings.LastIndexByte(base, ']'); i >= 0 { base = base[:i+1] } // strip any old suffix
    fold.summaryNode.Label = base + summarySuffix(fold.summaryNode)
}
```

This makes the suffix track `Expanded` regardless of *how* it changed (click, keyboard, or a test
flipping `Expanded` directly), with no structural mutation and no teardown — it only rewrites a
label string. (In production the sidebar tree never takes keyboard focus, so the keyboard path is
largely theoretical, but this keeps the acceptance criterion honest and the suffix never stale.)

> Implementation note: factor the "counts → label" so `refreshFoldChrome` and this reconcile share
> `summarySuffix`. The reconcile only fixes the suffix; counts are still recomputed only on events
> (`applySubAgent` / `tickFolds` / `dismissFailed` / `OnToggle`).

### 10. `tickFolds` selection re-anchor
Unchanged except the "land on the bucket that absorbed it" target becomes the summary node:

```go
if !s.tree.SelectNode(sel) && selFolded {
    if fold := s.foldOf(sel); fold != nil && fold.summaryNode != nil {
        s.tree.SelectNode(fold.summaryNode)
    }
}
```

When a selected completed agent folds under a *collapsed* summary its row vanishes, so the highlight
lands on the summary node (same UX as the old bucket).

### 11. Untouched paths that "just work" against the single node
`applySubAgent` (re-run unfold via the `default` branch), `dismissFailed` (failed rows are visible
siblings of the parent, never under the summary — `removeChild(parent, node)` as today),
`removeSession` (iterates `fold.entries` to prune `s.agents`; the summary node + children vanish with
the parent node), and `foldOf` (maps both `nodeRef` and `syntheticRef` to a session) all continue to
work; only their references to `statusNode`/`bucketNode` collapse to `summaryNode`.

## User-facing behaviour

| Situation | Sidebar row(s) |
|---|---|
| Session with running/failed agents, nothing archived | one line `[▶2 ✗1]`, no marker, no suffix |
| First completed agent ages out (TTL) | same line gains `+`: `[▶2 ✓1 ✗1]+`, archived hidden |
| User clicks the row / presses Right/Space | flips to `-`: `[▶2 ✓1 ✗1]-`, archived agents appear as indented children |
| User clicks again / Left | back to `+`, archived hidden |
| Last archived agent re-runs (unfold) | suffix drops back to none: `[▶3 ✗1]` |
| All agents gone (or only-a-dismissed-failure) | summary line torn down; clean session row |

Exactly **one** summary line, ever; the `✓` total is printed once and always counts archived agents.

## The 4 design gates

### (1) Goal match
A targeted UI collapse — **no** scope creep. It does precisely what #490 asks: one summary line
(never two); no leading `▸`/`▾` (via `HideMarker`); a trailing `+`/`-` that appears **iff** there are
archived agents (childless ⇒ no suffix); click + keyboard toggle it and show/hide the archived
children; totals always include archived `✓`; failed agents are never auto-archived (still counted in
`✗` until dismissed). No behaviour is added beyond the merge of the two rows.

### (2) Usability
The expand affordance is surfaced, not silent: the `+`/`-` glyph tells the user the line is
expandable and which state it is in. Clicking the (otherwise inert) summary row toggles it — the user
drives it directly — and keyboard `Left`/`Right`/`Space` work too, with the suffix re-derived at
paint time so it never lies. A body click does **not** pop a monologue or raise a window
(`syntheticRef` inert, issue #302), and `Enter` is a no-op on the summary. The duplicate `✓` that
confused the old two-row layout is gone.

### (3) No regressions
All #484 invariants are preserved against the single node: TTL fold driven by
`tickBusyStatuses`→`tickFolds` (60s, clock-injectable); re-run unfold; manual dismiss of failures;
session teardown; per-session independence; selection re-anchoring; and `len(s.agents)` (the Overall
band) unchanged by a fold (visibility-only). The synthetic-prefix ordering with watchers still holds
(`summaryNode` stays child[0]; watchers stay visible siblings). `sidebar_todos_test.go` only asserts
`Children[0].Data` is a `syntheticRef` — still true — so it stays green (its `statusNode` mention is
a comment). The shared agent tree / server / remote are untouched, so transcript and session
invariants are unaffected. `gofmt`/`go build`/`go vet`/`golangci-lint`/`go test ./...` per the dev
gate (no `-race` on the Pi5); the pre-existing environmental `TestUserSessionSendMessage` 404 is the
only accepted failure.

### (4) Holistic design across both repos
The repo seam is respected. turbotui owns the *generic* tree primitives — marker rendering
(`HideMarker`) and the *mechanism* to toggle a marker-less row from a body click (`OnToggle`) — both
already shipped in #490's turbotui half (`877fd62`), reusable by any host, with no gogent-specific
logic. gogent owns the *policy*: which node is synthetic, what the `+`/`-` means, and how the label is
composed. The summary node consumes `HideMarker` (rendering) + `OnToggle` (click) and re-derives its
own suffix on the keyboard/redraw path — nothing about the fold semantics leaks into turbotui. The
only cross-repo coupling is the `go.mod` bump + the pin-guard string, both called out above. No
downstream turbotui change is required by this gogent half.

## Regression risks & mitigations
- **Stale `+`/`-` after a keyboard toggle** (no `OnToggle` on the keyboard path). Mitigated by the
  draw-time suffix reconcile (§9), which re-derives the suffix from `Expanded` every paint.
- **Tearing down the summary on unfold-to-empty.** The old bucket detached when empty; the summary
  must **not** (it is also the status bar). `unfoldAgent` never removes the summary; only
  `refreshFoldChrome` does, and only when `entries` is empty. Covered by a test asserting the summary
  survives an unfold-to-childless with the suffix dropping to "".
- **`OnToggle` over-consuming real rows.** The handler self-filters on `syntheticRef` and returns
  `false` for everything else (and for a childless summary), so session/agent rows keep their default
  marker/activate behaviour. Covered by a test that a session row still expands/collapses normally.
- **Draw order assumption** (panel `DrawFn` runs before the tree child draws). True today
  (`drawTodos`/`drawOverall` already paint from `panel.DrawFn` and the tree is an added child);
  verified by the render-based test reading the painted `+`/`-` cell. If it ever changed, the click
  path's explicit `refreshFoldChrome` still keeps the click case correct.

## Tests — `ui/tui/sidebar_fold_issue484_test.go` (rewrite to single-node + suffix)
The `[✓ N]` bucket-row assertions become assertions on the summary line's label/suffix. Concretely:

- **Single summary, never two:** after a fold, the session has exactly one synthetic child
  (`summaryNode`); no second synthetic row exists.
- **Created + pinned + inert leaf-marker:** `summaryNode` is `Children[0]`, `Data` is `syntheticRef`,
  `HideMarker == true`; while childless it shows the counts and **no** `+`/`-`.
- **No leading marker, trailing suffix:** render the sidebar (cell read, as in
  `TestFold_RenderEmptyBucketRule`) and assert no `▸`/`▾` is painted on the summary row and the
  trailing glyph is absent (childless) / `+` (collapsed) / `-` (expanded).
- **Suffix iff archived children:** none before TTL; `+` once the first agent folds; `""` again after
  an unfold-to-empty (and the summary node *survives*).
- **TTL fold** (clock-injected `s.now`/`s.ttl`, no real sleep): completed agent moves under
  `summaryNode` at/after TTL; `summaryNode.Expanded == false`; `✓` still counts it.
- **Click toggle:** invoke `tree.OnToggle(summaryNode, ev)` → returns true, `Expanded` flips, label
  suffix flips `+`↔`-`, and the archived child becomes visible/hidden via `flatten()`. A real
  session/agent node passed to `OnToggle` returns false.
- **Keyboard/programmatic toggle:** flip `summaryNode.Expanded` directly, render, assert the painted
  suffix tracks it (covers the draw-time reconcile).
- **Totals include archived `✓`;** **failed stays visible + counted in `✗` until dismissed;**
  **duplicate completion doesn't reset TTL;** **re-run unfold;** **dismissFailed** (clears row, keeps
  an existing archive, tears down when none remain); **removeSession** teardown; **per-session
  independence;** **selection re-anchors to `summaryNode`** when the selected completed agent folds;
  **`len(s.agents)` unchanged by fold;** **watcher interleave** keeps `summaryNode` at child[0] with
  the watcher a visible sibling and the folded agent under `summaryNode`. Production defaults
  (`subAgentFoldTTL == 60s`, `s.now != nil`) pinned as today.

## Open questions
1. **Toggle target — whole row vs. just the `+`/`-` column.** The issue text endorses "suffix/row
   click" and turbotui's `OnToggle` fires for the whole row, so this design toggles on a click
   **anywhere** on the (inert) summary row, with `+`/`-` as the visible cue. If product wants only the
   suffix *cell* to be clickable, we'd gate the `OnToggle` body on `ev.X` against the row's last
   painted column — more code and brittle against truncation, and arguably worse UX. Defaulting to
   whole-row unless told otherwise.
2. **Suffix spacing.** Rendering `[…]+` with no space (matching the issue title `[...]+`). If a space
   reads better (`[…] +`) it is a one-character change localized to `summarySuffix`.
