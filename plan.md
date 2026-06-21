# Issue #207 — "Waiting for input" badge on the session row (Option A)

Reuse the #55 approval-badge machinery for a second, parallel **clarify** badge
that marks a session whose interactive sub-agent has gone to `StatusWaiting`
(asked a `CLARIFY:` question), plus a global `❓N` header count. No upstream
dep; mirrors `approvals`/`setApproval`/`setGlobalApprovals`/`approvalBadge`.

## ui/tui/sidebar.go

1. **Constant**: `clarifyBadge = "❓"` next to `approvalBadge = "⏳"`.
2. **State**: `clarify map[string]bool` + `globalClarify int` on `sidebar`,
   alongside `approvals`/`globalApprovals`; init `clarify` in `newSidebar`.
3. **`sessionLabel`**: gains a trailing `clarify bool` param; the `❓` badge is
   appended *after* the `⏳` pending badge, i.e. LAST, so a wide glyph can't
   shift the status-icon / title columns.
4. **Callers carry both flags** so the badge survives rename/pin:
   - `addSession`   → `sessionLabel(title, …, s.approvals[id], s.clarify[id])`
   - `setApproval`  → `sessionLabel(title, …, pending,         s.clarify[id])`
   - `relabelSession` → `sessionLabel(title, …, s.approvals[id], s.clarify[id])`
5. **New methods** mirroring the approval ones:
   - `setClarify(id, title string, pinned, waiting bool)` — toggles `clarify[id]`
     and relabels the node (preserving the live `approvals[id]` flag).
   - `setGlobalClarify(n int)` — clamps `<0` to 0, stores `globalClarify`.
   - `removeSession` also `delete(s.clarify, id)` so closed sessions don't leak.
6. **Header**: in `panel.DrawFn`, when `globalClarify > 0` draw `❓N`
   right-aligned just left of the existing `⏳N`, sharing the `abs.X+20` clamp.

## ui/tui/tui.go — `EmitSessionEvent`

When a `SessionEventSubAgent` arrives (UI thread, inside the existing
`desktop.Post`):
- `waiting := ev.Status == agent.StatusWaiting` (the same predicate
  `eventNotification` keys CLARIFY on).
- `setClarify(id, title, pinned, waiting)` — set on enter-waiting, clear on the
  next non-waiting lifecycle event (running/resumed/completed/failed).
- `setGlobalClarify(len(sidebar.clarify))` — count of sessions currently
  flagged (a session shows at most one clarify badge).

Grab `title`/`pinned` under `w.mu` next to the existing `sw` lookup.

## Constraints / test surface for GLM

- No new deps; gofmt; golangci-lint clean; tests run WITHOUT `-race` (Pi5).
- Public interfaces a tester targets: `clarifyBadge`, `sidebar.clarify`,
  `sidebar.globalClarify`, `sidebar.setClarify`, `sidebar.setGlobalClarify`,
  and `sessionLabel(title, status, pinned, pending, clarify)`.
- Existing `TestSidebarApprovalBadge` is untouched (approval methods keep their
  signatures). `TestSessionLabelBadge`'s single direct `sessionLabel(...)` call
  gets one mechanical `false` arg to stay compiling.
