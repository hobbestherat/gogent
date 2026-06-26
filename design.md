# Design — Issue #484: Fold finished sub-agents into the session line after a TTL (+ per-session status bar)

## Summary

The sidebar's "Sessions & Agents" tree (`ui/tui/sidebar.go`) is currently
append-only: `applySubAgent` creates one `*tv.TreeNode` per sub-agent and never
removes it, so finished sub-agents (kept in the agent tree on purpose, issue
#280) accumulate forever and bury the few active ones.

This change adds, **entirely inside the sidebar UI mirror**:

1. A **per-session status bar** — a synthetic, always-first, leaf child node
   under each session showing bracketed counts by state
   (`[▶2 ‖1 ✓5 ✗1]`, zero counts omitted) using the existing `statusIcon`
   glyphs.
2. A **TTL fold**: a `StatusCompleted` sub-agent stays a normal visible child
   row for **60s** (clock-injectable), then is moved under a synthetic
   collapsed **finished bucket** node (`▸ [✓ N]`, the second child), hidden by
   the tree's existing collapse mechanic. The status bar's `✓` count still
   includes folded agents.
3. **Failed agents never auto-fold** — they stay visible until the user
   manually dismisses them (a menu action on the focused session); the status
   bar `✗` count tracks undismissed failures.
4. Everything is **per-session** and reuses the existing 1s status ticker to
   expire TTLs — no per-agent timers, no new goroutines.

No change to `internal/agent`, `internal/server`, `ui/tui/remote_handlers.go`,
`ui/tui/api_client.go`, or `turbotui`. (See "Orchestration / cross-repo".)

---

## How the existing pieces constrain the design (verified in source)

- `turbotv.Tree` (`turbotui/turbotv/widget_tree.go`): `flatten()` is
  depth-first and **skips the children of a collapsed node**. A node renders the
  `▾`/`▸` marker **iff `len(Children) > 0`**; a leaf renders a **blank** marker
  column. Expand/collapse is driven by marker-click (`handleClick`,
  `event.X <= markerCol`) and, when the tree has keyboard focus, Space / ← / →
  (`handleType`). Indent = `depth*2`. → A synthetic **leaf** node gives us the
  "blank marker column" status bar for free; a synthetic node **with children**
  gives us the `▸`/`▾` bucket for free; **moving** a node under the (collapsed)
  bucket hides it with no new widget API. This is the Gogent-only mechanism the
  cross-repo note asks for — **no turbotui change is needed.**
- The tree handlers `OnSelect` / `OnSelectMouse` / `OnActivate`
  (`sidebar.go:288-338`) all begin with `ref, ok := n.Data.(nodeRef); if !ok { return }`.
  → A synthetic node whose `Data` is **not** a `nodeRef` is inert in all three
  paths automatically (no focus-raise, no monologue, no crash). Folded
  **real** agent rows keep their `nodeRef`, so a revealed finished agent still
  opens its monologue via the unchanged handlers.
- `applySubAgent` (`sidebar.go:866`) is the single hook for every sub-agent
  lifecycle event, called only from `deliverSessionEvent` (`tui.go:2255`) on the
  UI thread via `desktop.Post`. → finish-timestamp capture and fold-state
  transitions live here; the "delivered on the UI thread" requirement is already
  satisfied.
- `runStatusTicker` → `tickBusyStatuses` (`tui.go:2685`, `2701`) already posts
  to the UI thread **every `statusTickInterval` (1s), unconditionally**, and
  already redraws when a sidebar reconcile reports a change. → This is the
  fold-expiry driver: add one `sidebar.tickFolds()` call to the same sweep.
- Session children today are **append-order** and can already interleave
  attached-watcher nodes (`setWatchers` → `parent.Add`, `sidebar.go:953`). →
  The status bar / bucket must be **pinned at indices 0 and 1**; an insert
  helper maintains that invariant, and watcher add/detach (append / identity-
  filtered rebuild) preserves it.
- `RestoredSession` (`tui.go:379`) carries **no** sub-agent data, and the
  embedded restore path (`AdoptSession` → `openWindow` → `OnCreate` →
  `SetObserver`, `cmd/embedded_handlers.go:28`) does **not** replay sub-agent
  events. → A restored session simply has no sub-agent nodes until fresh live
  events arrive; fold state is reconstructed by the same `applySubAgent` path.
  See "Restore / re-render" for the remote-replay case and its accepted limit.

---

## Data model (all new state in `ui/tui/sidebar.go`)

```go
// Clock + TTL, injectable for tests; production defaults below.
type sidebar struct {
    ...
    folds map[string]*sessionFold // sessionID -> fold bookkeeping (lazy)
    now   func() time.Time        // defaults to time.Now
    ttl   time.Duration           // defaults to subAgentFoldTTL (60s)
}

const subAgentFoldTTL = 60 * time.Second

// sessionFold is the per-session UI-only fold bookkeeping. Created lazily when a
// session gets its first sub-agent.
type sessionFold struct {
    statusNode *tv.TreeNode          // synthetic child[0] — the "[▶ ‖ ✓ ✗]" bar (leaf)
    bucketNode *tv.TreeNode          // synthetic child[1] — the "[✓ N]" finished bucket
    entries    map[string]*foldEntry // agent key -> entry (same key applySubAgent uses)
}

type foldEntry struct {
    status     agent.AgentStatus
    finishedAt time.Time // set once, when status first becomes StatusCompleted
    folded     bool      // moved under bucketNode
    dismissed  bool      // failed-and-manually-dismissed
}

// Data payload for synthetic rows so the nodeRef handlers stay inert on them.
type syntheticRef struct {
    sessionID string
    bucket    bool // false = status bar, true = finished bucket
}
```

`s.agents` (the existing `key -> *tv.TreeNode` map) is **kept** as-is and stays
the node lookup used by `applySubAgent`, `removeSession`, and the Overall
`len(s.agents)` count. `foldEntry` holds only the *fold metadata*; the node
itself is reached via `s.agents[key]`.

`newSidebar` initialises `folds`, `now = time.Now`, `ttl = subAgentFoldTTL`
(so `newSidebar(&Workbench{})` in `newTestSidebar` keeps working).

---

## Control flow

### Node layout per session

```
○ Session 1                 (session node, existing)
    [▶2 ‖1 ✓2 ✗1]           child[0] statusNode  (leaf, blank marker)
  ▸ [✓ 2]                   child[1] bucketNode  (collapsed; ▾ when expanded)
    ▶ worker-a              child[2..] visible agent rows (active / in-TTL ✓ / ✗)
    ✗ worker-b
        ✓ old-1            (under bucketNode when expanded; hidden when collapsed)
        ✓ old-2
```

### `applySubAgent(sessionID, ev)` (rewritten, holistic)

1. Look up `parent := s.sessions[sessionID]`; bail if nil (unchanged guard).
2. `ensureFold(sessionID, parent)` — lazily create `statusNode` + `bucketNode`
   and **prepend** them (indices 0,1), shifting any existing watcher children
   right. `statusNode.Data = syntheticRef{sessionID}`,
   `bucketNode.Data = syntheticRef{sessionID, bucket:true}`. `bucketNode`
   starts with no children (so it renders as a plain leaf with a blank marker
   until the first agent folds in — only then does it gain the `▸`).
3. Resolve `key` exactly as today (`ev.AgentID`, else `sessionID+"/"+ev.Name`).
4. Create-or-relabel the agent node (existing logic), inserting **after**
   `statusNode`/`bucketNode` (index ≥ 2) for a new node.
5. Update the `foldEntry`:
   - record `status = ev.Status`;
   - `StatusCompleted` & `finishedAt` zero → set `finishedAt = s.now()` (TTL
     clock starts at delivery, per acceptance criteria); if it had been folded
     under a stale state, keep it visible until the TTL elapses;
   - status **leaves** `StatusCompleted` (re-run edge) → clear `finishedAt`;
     if folded, unfold (move node back to the visible list);
   - `dismissed` is never set here (only by the dismiss action).
6. `refreshFoldChrome(sessionID)` — recompute the status-bar label and bucket
   label (below).

### `tickFolds() bool` (new; called from `tickBusyStatuses`)

For each session's `foldEntry` with `status == StatusCompleted && !folded &&
s.now().Sub(finishedAt) >= s.ttl`: move its node from `parent.Children` to
`bucketNode.Children`, set `folded = true`, and on the **empty→non-empty**
transition set `bucketNode.Expanded = false` (collapsed by default; later folds
do not override a user's manual expand). Returns whether anything moved (and
relabels affected status/bucket nodes). `tickBusyStatuses` folds its result into
its existing `redraw` decision so an otherwise-idle tick still repaints on a fold
edge (this is what makes S4's idle session fold).

### Counts / labels (`refreshFoldChrome`)

Iterate `entries`:
- `▶` running = `status == StatusRunning`
- `‖` waiting = `status == StatusWaiting`
- `✓` finished = `status == StatusCompleted` (visible **and** folded)
- `✗` failed = `status == StatusFailed && !dismissed`

`statusNode.Label` = `"["` + space-joined non-zero `glyph+count` + `"]"` (reuse
`statusIcon`). `bucketNode.Label` = `"[✓ N]"` where N = folded completed count;
the tree supplies the `▸`/`▾`. If **all** counts are zero **and** no folded
agents remain (e.g. the only agent was a dismissed failure), remove both
synthetic nodes and the `sessionFold` entry, returning the row to its clean
pre-agent state.

### Manual dismiss of failed agents

`dismissFailed(sessionID)`: for every `entry` with `status == StatusFailed &&
!dismissed`, set `dismissed = true`, remove its node from `parent.Children`,
`delete(s.agents, key)`, and prune the `clarifyWaiting` key (mirroring
`removeSession`). Then `refreshFoldChrome`.

**Affordance:** a View-menu item **"Dismiss &Failed Sub-agents"**
(`tui.go viewItems`) that calls `s.dismissFailed(s.focused)` and redraws — a
discoverable click action that does not depend on the sidebar tree holding
keyboard focus (the tree is mouse-driven in practice; see Usability). It clears
all failed agents of the focused session at once (issue allows per-agent or
clear-all). `dismissFailed` is exported sidebar-internal so tests drive it
directly.

### `removeSession` (updated)

Also iterate `folds[id].entries` (covers folded agents now living under the
bucket, which the current `node.Children` walk would miss) to delete `s.agents`
keys and prune `clarifyWaiting`, then `delete(s.folds, id)`. This keeps a
re-adopted session with the same id starting clean.

---

## Files / functions touched

**gogent (only):**
- `ui/tui/sidebar.go`
  - `type sidebar` — add `folds`, `now`, `ttl`.
  - `newSidebar` — init the three fields.
  - new: `sessionFold`, `foldEntry`, `syntheticRef`, `const subAgentFoldTTL`.
  - new: `ensureFold`, `refreshFoldChrome`, `statusBarLabel`, `bucketLabel`,
    `foldAgent`, `unfoldAgent`, `tickFolds`, `dismissFailed`.
  - rewrite: `applySubAgent` (timestamp + entry + ordering).
  - update: `removeSession` (clear fold state via `entries`).
- `ui/tui/tui.go`
  - `tickBusyStatuses` — call `s.tickFolds()` and OR it into `redraw`.
  - `viewItems` — add the "Dismiss Failed Sub-agents" menu item.
- `ui/tui/agent_monolog.go` — **read-only**; no change needed (folded→revealed
  agents keep their `nodeRef`, so `showAgentMonolog` works unchanged).

**turbotui:** none. The synthetic-node + existing-collapse approach is provably
sufficient (see "How the existing pieces constrain the design"); no
`tv.Tree`/`TreeNode` API gap, so no `go.mod` bump.

**Read-only context (NOT edited):** `internal/agent/*`, `internal/server/*`,
`ui/tui/remote_handlers.go`, `ui/tui/api_client.go`,
`internal/agent/user_session.go` (consumed: `SessionEventSubAgent`,
`AgentStatus` consts, `SessionEvent` fields).

---

## Gate (1) — Goal match

- Completed sub-agent visible as a normal row for 60s, then folded into
  `▸ [✓ N]` pinned immediately after the status bar — `applySubAgent` stamps
  `finishedAt = now`, `tickFolds` folds at `now-finishedAt >= ttl`.
- Per-session status bar `[▶ ‖ ✓ ✗]` always visible (leaf, never folded), counts
  folded `✓` and undismissed `✗` — `refreshFoldChrome` counts from `entries`,
  independent of node visibility.
- Failed agents never auto-fold (`tickFolds` only touches `StatusCompleted`) +
  manual dismiss (`dismissFailed` / menu).
- Per-session independence — one `sessionFold` per id; bucket `Expanded` and
  `entries` are per-session.
- Reveal/refold via the bucket `▸`/`▾` marker — driven by the unchanged tree
  collapse mechanic; no new control invented.
- Exactly a feature in the sidebar; no scope creep into agent tree / API /
  watcher folding / transcript cap (those are explicitly out of scope).

## Gate (2) — Usability

- Status bar is bracketed and sits in the blank-marker column, visually distinct
  from real agent rows (which lead with a single glyph) — per the issue's
  "visually distinct" requirement.
- The bucket marker is the single fold/unfold affordance: **marker-click**
  (mouse) is the primary, always-available control; **Space / ← / →** also work
  when the tree holds keyboard focus. (Today the sidebar tree is navigated by
  mouse — `refreshTheme` notes it "never takes keyboard focus" — so marker-click
  is the dependable path and is what S2/S3 lean on.)
- Revealed finished agents remain selectable and open the monologue (they keep
  their `nodeRef`; handlers unchanged).
- Failed-agent dismiss is a labelled, discoverable menu action on the focused
  session — surfaced, not silent — and does not hijack the agent-row click
  (which still opens the monologue).
- Synthetic rows are inert to select/activate (non-`nodeRef` `Data`), so a stray
  click on the status bar or bucket body never pops a window or raises the wrong
  thing.

## Gate (3) — No regressions

- `sessionStatusGlyph` / `sessionLabelState` / `sessionLabel`,
  approval (`⏳`) and clarify (`❓`) badges: untouched — fold work only adds/moves
  **child** nodes and never rewrites the session node's own label.
- Monologue popups: unchanged path; folded-then-revealed agents keep `nodeRef`.
- Watcher nodes (free + attached): still appended as children; the insert helper
  keeps them after the two synthetic nodes, and `detachWatcherNode`'s identity-
  filtered rebuild preserves the synthetic nodes' positions. Watcher folding is
  out of scope and untouched.
- Agent-tree retention / `ActiveSubAgentCount` / `ListAllAgents` / slot counting:
  untouched — folding is a pure **visibility** move of UI nodes; the shared agent
  tree's parent/child structure is never mutated.
- `clarifyWaiting` dedup: `dismissFailed` and the updated `removeSession` prune
  the same key the existing code does, so no dangling entries.
- Overall panel `len(s.agents)`: a folded agent stays in `s.agents` (count
  unchanged); only an explicitly **dismissed** failed agent leaves it (intended
  — it is gone from the UI).
- Existing tests: `newSidebar(&Workbench{})` still works (new fields default
  in the constructor); session-label / badge / watcher / busy tests are
  unaffected because the session node's own label and the watcher reconcile are
  unchanged. Sub-agent tests that count `parent.Children` directly will now see
  two leading synthetic nodes — those are mine to add/adjust, none exist today
  that assert a raw child count on a sub-agent parent (verified: existing
  sub-agent tests drive the handlers with hand-built nodes, not child counts).
- gofmt / build / vet / golangci-lint (0 new) / `go test ./...` green; the
  pre-existing environmental `TestUserSessionSendMessage` 404 is the only
  accepted failure (no `-race` on Pi5).

## Gate (4) — Holistic / cross-repo

- All fold/TTL/status-bar state lives in the sidebar UI mirror — the issue's own
  stated preference and the orchestration constraint's requirement. The shared
  agent tree (read by `internal/server/wire.go sessionToView`) is the wrong
  place for time/visibility state and is not touched, so the concurrent #481
  work on `user_session.go` / `internal/server/*` / `remote_handlers.go` /
  `api_client.go` stays file-disjoint and merge-clean.
- The seam to turbotui is respected by **using** its tree primitives (collapse,
  child reparenting, marker rendering) rather than extending them: no new
  widget API, no filter/hidden-node API invented, no `go.mod` bump. A
  Gogent-only solution is sufficient and is what is implemented.
- Downstream effect on turbotui: none. If a future requirement needed
  hidden-node filtering or a per-row affordance the widget can't express, that
  would be a separate turbotui-first task — not triggered here.

---

## Restore / re-render

- **Re-render / redraw / theme switch:** tree nodes persist across redraws;
  fold state lives in `folds` (not rebuilt on draw), so folded stays folded and
  the bucket's `Expanded` is preserved. `refreshTheme` only reseeds colours.
- **Embedded restore (`AdoptSession`):** no sub-agent replay (verified), so a
  restored session shows no sub-agent rows until fresh live events; nothing to
  mis-expand. `removeSession` fully clears `folds[id]`, so close+reopen starts
  clean.
- **Remote reconnect replay:** the daemon/remote path (which we must not edit)
  re-delivers events through `applySubAgent`. Because `SessionEvent` carries no
  finish timestamp and we are forbidden from adding one (orchestration
  constraint), a replayed `StatusCompleted` re-stamps `finishedAt = now`, so a
  long-finished agent re-appears as a visible row and folds 60s later rather
  than appearing pre-folded. This is the one deviation from "show folded
  immediately on restore," and it is a direct consequence of the read-only
  boundary, not a design miss. It is self-healing (folds within one TTL) and
  affects only the remote replay path. See Open questions.

---

## Tests to add (`ui/tui/`)

All use the injectable clock: set `s.now` to a controllable func (or set
`s.ttl` small) so no test sleeps 60s.

1. **TTL fold timing** — completed agent is a visible child `< ttl`; after
   advancing `now` past `ttl` and calling `tickFolds`, it moves under the bucket
   and the bucket shows `[✓ 1]`; status bar still shows `✓1`.
2. **Failed no-fold + dismiss** — failed agent stays visible across `tickFolds`;
   status bar shows `✗1`; `dismissFailed` removes the row and drops `✗` to 0.
3. **Per-session independence** — two sessions each with completed agents;
   folding/expanding one bucket leaves the other's nodes and `Expanded` state
   untouched.
4. **Expand-to-reveal** — after fold, expand the bucket; the finished agent is
   again a (depth-2) visible row whose `nodeRef` drives `OnSelectMouse` →
   `showAgentMonolog`.
5. **Restore/rebuild** — `removeSession` clears `folds`; a re-`addSession` +
   re-`applySubAgent` reconstructs a clean status bar/bucket (no stale counts).
6. **Status-bar counts** — mixed running/waiting/completed(folded)/failed counts
   render correctly with zero counts omitted and folded `✓` included.

---

## Open questions

1. **Remote-replay pre-folding.** Accepting the "re-appears then folds in 60s"
   behaviour on remote reconnect (above) keeps us inside the read-only boundary.
   The clean fix is a finish timestamp on `SessionEvent` (or a replay flag),
   which is #481's territory and explicitly forbidden here. Confirm the accepted
   tradeoff, or schedule a follow-up that adds an event timestamp once #481
   lands.
2. **Dismiss granularity.** Proposed: clear-all-failed for the focused session
   via a menu item. If per-agent dismiss is preferred, the only mouse target the
   widget offers is the agent row itself (already bound to the monologue), so
   per-agent dismiss would need either a modifier-click convention or a turbotui
   affordance — i.e. it would pull in a turbotui change. Clear-all keeps it
   Gogent-only. Confirm clear-all is acceptable.
3. **Status bar when the only agent is dismissed.** Proposed: remove the status
   bar + bucket entirely (row returns to clean). Alternative: keep an empty
   `[]`. Clean-removal is assumed unless told otherwise.
