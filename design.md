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
  The status bar is **pinned at index 0**; the bucket, **when present**, sits at
  index 1 (see "Empty-bucket" below — it is attached only once something folds).
  An insert helper maintains that invariant, and watcher add/detach (append /
  identity-filtered rebuild) preserves it.
- **No event-replay on any restore path (verified, not assumed).**
  `RestoredSession` (`tui.go:379`) carries **no** sub-agent data, and the
  embedded restore path (`AdoptSession` → `openWindow` → `OnCreate` →
  `SetObserver`, `cmd/embedded_handlers.go:28`) does **not** replay sub-agent
  events. The **remote** path is also *jump-to-present, not a replay*:
  `RemoteClient.reconnect` (`remote_handlers.go:289`) returns a brand-new
  `StreamEvents` with **no buffered backlog**, and its `notifyRestored`
  re-fetches `/sessions` + transcripts only — historical `SessionEventSubAgent`s
  are never re-delivered. (The "buffered for replay" at `cmd/daemon.go:300` is
  *notifications* for the local notifier, **not** the SSE event stream.)
  `sidebar.applySubAgent` (driven by `EmitSessionEvent`) is the **only** node
  creator and only ever sees live events. → A restored/reconnected session shows
  no sub-agent rows until fresh live events arrive; **nothing re-delivers a
  historical completion**, so a long-finished agent can never be wrongly
  "re-expanded." `removeSession` clears fold state so close+reopen starts clean.
  This satisfies the restore acceptance criterion directly.

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
    statusNode *tv.TreeNode          // synthetic child[0] — the "[▶ ‖ ✓ ✗]" bar (leaf), created with the fold
    bucketNode *tv.TreeNode          // synthetic child[1] — the "[✓ N]" bucket; nil until the first agent folds (see Empty-bucket)
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

In-TTL (S1) — **no bucket row** until something folds:

```
○ Session 1                 (session node, existing)
    [▶2 ‖1 ✓2 ✗1]           child[0] statusNode  (leaf, blank marker)
    ▶ worker-a              child[1..] visible agent rows (active / in-TTL ✓ / ✗)
    ✗ worker-b
```

After ≥1 fold (S2/S3) — bucket attached at index 1:

```
○ Session 1
    [▶2 ‖1 ✓2 ✗1]           child[0] statusNode
  ▸ [✓ 2]                   child[1] bucketNode  (collapsed; ▾ when expanded)
    ▶ worker-a              child[2..] visible agent rows
    ✗ worker-b
        ✓ old-1            (under bucketNode; shown only when expanded)
        ✓ old-2
```

**Empty-bucket rule (resolves the S1 contradiction).** `flatten()` renders one
row per node — a childless node is **not** invisible. So the bucket must be
**absent**, not empty, while nothing is folded. Therefore `ensureFold` creates
**only** the `statusNode`; `bucketNode` is created and attached at index 1 by the
**first** `foldAgent` call and detached again if it ever drops to zero children
(re-run edge / session removal). A `[✓ 0]` row is never rendered, matching the
S1 mockup exactly.

### `applySubAgent(sessionID, ev)` (rewritten, holistic)

1. Look up `parent := s.sessions[sessionID]`; bail if nil (unchanged guard).
2. `ensureFold(sessionID, parent)` — lazily create `statusNode` **only** and
   insert it at **index 0** (shifting any existing watcher children right).
   `statusNode.Data = syntheticRef{sessionID}`. The bucket is **not** created
   here (see Empty-bucket rule).
3. Resolve `key` exactly as today (`ev.AgentID`, else `sessionID+"/"+ev.Name`).
4. Create-or-relabel the agent node (existing logic). A **new** node is inserted
   after `statusNode` and after `bucketNode` if present — i.e. at the first index
   ≥ 1 that is past the synthetic prefix (helper `insertVisibleAgent`).
5. Update the `foldEntry`:
   - record `status = ev.Status`;
   - `StatusCompleted` & `finishedAt` zero → set `finishedAt = s.now()` (TTL
     clock starts at delivery, per acceptance criteria);
   - status **leaves** `StatusCompleted` (re-run edge) → clear `finishedAt`;
     if folded, `unfoldAgent` (move node back to the visible list);
   - `dismissed` is never set here (only by the dismiss action).
6. `refreshFoldChrome(sessionID)` — recompute labels (below).

### `foldAgent` / `unfoldAgent` (node movement helpers)

- `foldAgent(fold, parent, key)`: if `fold.bucketNode == nil`, create it
  (`Data = syntheticRef{sessionID, bucket:true}`, `Expanded = false`) and insert
  at index 1 (right after `statusNode`). Move the agent node from
  `parent.Children` to `bucketNode.Children`; set `folded = true`. (Subsequent
  folds leave `Expanded` as the user last set it — only the create sets it false,
  giving "collapsed by default once non-empty".)
- `unfoldAgent(fold, parent, key)`: move the node from `bucketNode.Children` back
  into the visible list (via `insertVisibleAgent`); if `bucketNode.Children` is
  now empty, detach `bucketNode` from `parent.Children` and set it nil.

### `tickFolds() bool` (new; called from `tickBusyStatuses`)

Capture the currently selected node first: `sel := s.tree.Selected()`. For each
session's `foldEntry` with `status == StatusCompleted && !folded &&
s.now().Sub(finishedAt) >= s.ttl`: `foldAgent(...)`. After the sweep, **re-anchor
the selection by identity** (selection stability): if `sel != nil`, call
`s.tree.SelectNode(sel)`; if that returns false (the selected row was just folded
out of sight), `SelectNode(fold.bucketNode)` for the owning session so the
highlight lands on the bucket that absorbed it rather than drifting to an
unrelated row (the tree is index-based and `draw()` only `clampSelection`s, so
without this a background tick could silently move the highlight). Both calls use
existing public tree API (`Selected`/`SelectNode`) — no turbotui change. Returns
whether anything moved, relabelling affected nodes; `tickBusyStatuses` ORs the
result into its existing `redraw` decision so an otherwise-idle tick still
repaints on a fold edge (this is what makes S4's idle session fold).

### Counts / labels (`refreshFoldChrome`)

Iterate `entries`:
- `▶` running = `status == StatusRunning`
- `‖` waiting = `status == StatusWaiting`
- `✓` finished = `status == StatusCompleted` (visible **and** folded)
- `✗` failed = `status == StatusFailed && !dismissed`

`statusNode.Label` = `"["` + space-joined non-zero `glyph+count` + `"]"` (reuse
`statusIcon`). `bucketNode.Label` = `"[✓ N]"` where N = folded completed count;
the tree supplies the `▸`/`▾`. If **all** counts are zero (every agent was a
dismissed failure, leaving no running/waiting/completed/undismissed-failed and an
empty/absent bucket), remove the `statusNode` (and the already-detached bucket)
and `delete(s.folds, id)`, returning the row to its clean pre-agent state.

### Manual dismiss of failed agents

`dismissFailed(sessionID)`: for every `entry` with `status == StatusFailed &&
!dismissed`, set `dismissed = true`, remove its node from `parent.Children`,
`delete(s.agents, key)`, and prune the `clarifyWaiting` key (mirroring
`removeSession`). Then `refreshFoldChrome`.

**Affordance:** a View-menu item **"Dismiss &Failed Sub-agents"**
(`tui.go viewItems`) that calls `s.dismissFailed(w.ActiveID())` and redraws.
It targets `ActiveID()` — the canonical "session the user is viewing", matching
every other View-menu action (`withActiveTranscript`, `tui.go:1188`) — **not**
`sidebar.focused` (which is semantically the TODO-region focus, `sidebar.go:95`,
and only coincides with `ActiveID()` via the active-layer reconcile; using it
here would be a latent wrong-session footgun). A menu item is chosen over a
per-agent click because the sidebar tree's only per-row mouse target is the agent
row itself (already bound to the monologue) and the tree never holds keyboard
focus (`sidebar.go:466`), so both per-agent click and a row keybinding would need
a turbotui affordance — clear-all-via-menu keeps it Gogent-only (the issue allows
per-agent **or** clear-all). `dismissFailed` is sidebar-internal so tests drive
it directly.

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
  - new: `ensureFold`, `insertVisibleAgent`, `refreshFoldChrome`, `foldAgent`,
    `unfoldAgent`, `tickFolds`, `dismissFailed`.
  - rewrite: `applySubAgent` (timestamp + entry + ordering).
  - update: `removeSession` (clear fold state via `entries`).
- `ui/tui/tui.go`
  - `tickBusyStatuses` — call `s.tickFolds()` and OR it into `redraw`.
  - `viewItems` — add the "Dismiss &Failed Sub-agents" menu item
    (`s.dismissFailed(w.ActiveID())` + redraw).
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
- Failed-agent dismiss is a labelled, discoverable menu action on the **active**
  session (`ActiveID()`) — surfaced, not silent — and does not hijack the
  agent-row click (which still opens the monologue).
- Synthetic rows are inert to select/activate (non-`nodeRef` `Data`), so a stray
  click on the status bar or bucket body never pops a window or raises the wrong
  thing.
- **Selection stability:** a ticker-driven fold can hide the highlighted row;
  `tickFolds` re-anchors `t.selected` by node identity (`Selected`/`SelectNode`),
  falling back to the bucket when the selected agent was the one folded — so a
  background tick never silently drifts the highlight onto an unrelated row.
- **Overall-count vs sidebar (intentional):** the Overall band reads
  `len(s.agents)`, which still includes folded agents (folding is visibility, not
  pruning), so it may read "12 agents" while the tree shows 2 rows + `[✓ 10]`.
  This is deliberate and consistent with the agent tree's own retention; only a
  dismissed failure leaves `s.agents`.

## Gate (3) — No regressions

- `sessionStatusGlyph` / `sessionLabelState` / `sessionLabel`,
  approval (`⏳`) and clarify (`❓`) badges: untouched — fold work only adds/moves
  **child** nodes and never rewrites the session node's own label.
- Monologue popups: unchanged path; folded-then-revealed agents keep `nodeRef`.
- Watcher nodes (free + attached): still appended as children; `insertVisibleAgent`
  keeps agent rows after the synthetic prefix (statusNode, and bucket when
  present), and `detachWatcherNode`'s identity-filtered rebuild preserves the
  synthetic nodes' positions (covered by test 3). Watcher folding is out of scope
  and untouched.
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
  unchanged. **One existing assertion changes and must be updated** (correcting
  the earlier draft's wrong "no child-count tests exist" claim):
  `TestApplyTodoStoresNotTreeChildren` (`ui/tui/sidebar_todos_test.go:47`)
  asserts `len(s.sessions["s1"].Children) == 1` after a single `applySubAgent`;
  with the always-present `statusNode` prepended it becomes **2** (statusNode +
  agent row; no bucket since nothing folded). Update that assertion (and its
  comment) to expect 2. Its sibling `…StayZeroAcrossUpdates` only calls
  `applyTodo` (never creates synthetic nodes) and is unaffected. This is listed
  in "Tests to add/update" below.
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
- **Remote reconnect (verified: jump-to-present, NOT replay):**
  `RemoteClient.reconnect` (`remote_handlers.go:289`) opens a fresh
  `StreamEvents` with no buffered backlog and `notifyRestored` re-fetches
  `/sessions`/transcripts only; the daemon's "buffered for replay"
  (`cmd/daemon.go:300`) is for *notifications*, not the SSE event stream.
  Historical `SessionEventSubAgent`s are therefore **never** re-delivered to
  `applySubAgent`, so there is no path that re-stamps a long-finished agent's
  TTL and no way for it to be wrongly re-expanded. The acceptance criterion
  ("long-finished agents show folded, not re-expanded") holds on both paths —
  with no need to touch any read-only file. (Earlier draft wrongly assumed a
  replay here; that limitation is retracted.)

---

## Tests to add / update (`ui/tui/`)

All new tests use the injectable clock: set `s.now` to a controllable func (or
set `s.ttl` small) so no test sleeps 60s.

1. **TTL fold timing** — completed agent is a visible child `< ttl` (and no
   bucket node exists yet, per the empty-bucket rule); after advancing `now` past
   `ttl` and calling `tickFolds`, the bucket is attached at index 1 showing
   `[✓ 1]`, the agent row moves under it, and the status bar still shows `✓1`.
2. **Failed no-fold + dismiss** — failed agent stays visible across `tickFolds`;
   status bar shows `✗1`; `dismissFailed` removes the row and drops `✗` to 0
   (and, if it was the only agent, the status bar is removed).
3. **Per-session independence (incl. watcher interleave)** — two sessions each
   with completed agents; folding/expanding one bucket leaves the other's nodes
   and `Expanded` untouched. Also attach a watcher child to one session, then
   detach it, and assert `statusNode` stays at index 0 and the bucket (when
   present) at index 1 — pinning the ordering invariant against the watcher
   identity-filtered rebuild.
4. **Expand-to-reveal** — after fold, set `bucketNode.Expanded = true`; the
   finished agent is again a (depth-2) visible row whose `nodeRef` drives
   `OnSelectMouse` → `showAgentMonolog`.
5. **Restore/rebuild** — `removeSession` clears `folds`; a re-`addSession` +
   re-`applySubAgent` reconstructs a clean status bar (no stale counts, no
   leftover bucket).
6. **Status-bar counts** — mixed running/waiting/completed(folded)/failed counts
   render correctly with zero counts omitted and folded `✓` included.
7. **Selection stability** — select the in-TTL completed agent row, advance the
   clock, `tickFolds`; assert the selection re-anchors (to the bucket, since the
   row was folded away) rather than landing on an unrelated node.

**Update (existing):** `TestApplyTodoStoresNotTreeChildren`
(`sidebar_todos_test.go:47`) — expected child count after one `applySubAgent`
goes 1 → 2 (leading `statusNode`).

---

## Open questions

None blocking. Two decisions are taken as defaults (override if desired):

1. **Dismiss granularity** — clear-all-failed for the **active** session via a
   View-menu item. Per-agent dismiss is rejected because the only per-row mouse
   target is the monologue-bound agent row and the tree never holds keyboard
   focus, so it would require a turbotui affordance (out of the Gogent-only
   boundary). The issue explicitly allows per-agent **or** clear-all.
2. **Status bar when no agents remain** (e.g. the sole agent was a dismissed
   failure) — remove the `statusNode`/bucket entirely and drop `folds[id]`, so
   the session row returns to its clean pre-agent state (no lingering `[]`).

(The earlier "remote-replay pre-folding" open question is removed — verification
showed there is no replay path, so the concern does not arise.)
