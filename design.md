# Design — Always-on agent-summary bar (gogent issue #510)

**Branch:** `pair2/sidebar-agent-summary-bar-feature-for-go`
**Scope:** gogent only — `ui/tui/sidebar.go` + its tests. **No turbotui change. No go.mod bump. No new deps.**

## The ask (verbatim intent)

Three cosmetic/UX changes to the per-session sub-agent **summary row** in the sidebar
"Sessions & Agents" tree:

- **G1 — Always-on, all four states (incl. 0).** Every session row gets a summary line
  showing all four lifecycle states in fixed order `▶running ‖waiting ✓completed ✗failed`,
  each with its integer count **including 0** — even a session that has never spawned a
  sub-agent (renders `|▶0 ‖0 ✓0 ✗0|`). No more appearing/disappearing/reshaping.
- **G2 — Pipe brackets.** Wrap the bar in straight pipes `|…|` instead of `[…]`. The
  trailing `+`/`-` expand-collapse suffix is unchanged and sits immediately after the
  closing `|`.
- **G3 — Preserve alignment.** The leading `|` must stay at the same column as the
  session name's first character. No indent/padding added; no turbotui no-indent option.

## Why the alignment already works (verified, not assumed)

turbotui's `turbotv/widget_tree.go` paints **every** visible row as
`marker + " " + label` at `x = abs.X + depth*2`, where `marker` is exactly one column —
a glyph (`▾`/`▸`) for an expanded/collapsed parent, or a blank space for a leaf **or** a
`HideMarker` parent (`widget_tree.go:242–264`). Consequences, with cols measured from the
sidebar content origin:

- **Session node** (depth 0), label from `sessionLabelState` = `"○ title"`:
  `marker`(col0) + space(col1) + `○`(col2) + space(col3) + **`title[0]` at col4**.
- **Summary node** (depth 1, `HideMarker=true`), label `"|▶0 …|"`:
  indent(cols 0–1) + blank-marker(col2) + space(col3) + **`|` at col4**.

So the summary's leading bracket already lands at col4 = the session name's first char.
`[`→`|` is a 1-for-1 swap that does not move the column. **G3 holds for free** as long as
we add **no** leading spaces to the label and **no** indent option. (The new render test
pins this.)

This is also why the alignment is already correct for sessions that have sub-agents today —
issue #510 only extends the same row to zero-agent sessions.

## Files / functions touched (gogent only)

All in `ui/tui/sidebar.go`:

1. **`statusBarLabel(running, waiting, completed, failed int) string`** (~L1235) — the core
   change. Drop the `first`/separator logic and the `if n == 0 { return }` zero-omit
   early-out; emit a fixed four-segment string wrapped in pipes:
   ```go
   func statusBarLabel(running, waiting, completed, failed int) string {
       return fmt.Sprintf("|%s%d %s%d %s%d %s%d|",
           statusIcon(agent.StatusRunning), running,
           statusIcon(agent.StatusWaiting), waiting,
           statusIcon(agent.StatusCompleted), completed,
           statusIcon(agent.StatusFailed), failed)
   }
   ```
   Glyphs stay sourced from `statusIcon` (▶ ‖ ✓ ✗); `•` idle is still never emitted here.

2. **`refreshFoldChrome(sessionID)`** (~L1160) — remove the `len(fold.entries)==0` teardown
   branch (~L1166–1175) that did `removeChild(parent, summaryNode)` + `delete(s.folds, …)`
   + `return`. Falling through, the existing running/waiting/completed/failed loop naturally
   yields all-zeros and paints `|▶0 ‖0 ✓0 ✗0|`; the node persists. This is what makes the
   bar **always-on** for sessions whose only agent was dismissed/never existed.

3. **`addSession(id, title, pinned)`** (~L596) — after `s.sessions[id]=node` /
   `s.tree.AddRoot(node)`, eagerly create + paint the summary so it exists before any
   sub-agent event:
   ```go
   s.ensureFold(id, node)
   s.refreshFoldChrome(id)
   ```
   `ensureFold` is safe pre-event — it only builds the node + bookkeeping (verified L1017).
   With the teardown removed (#2), `refreshFoldChrome` now paints `|▶0 ‖0 ✓0 ✗0|` instead
   of tearing the node down.

4. **`syncFoldSuffixes()`** (~L1218) — change the sentinel
   `strings.LastIndexByte(base, ']')` → `'|'`. Left as `']'` it returns −1 and the suffix
   would be appended to a clobbered/mis-sliced label every frame. Update the inline comment
   ("everything up to and including the last `]`" → `|`).

5. **Doc/comments** — update stale teardown/bracket prose so the file stays self-describing:
   - `sessionFold` doc block (~L83–93): `"[▶2 ‖1 ✓5 ✗1]"` → `"|▶2 ‖1 ✓5 ✗1|"`; drop the
     "torn down only when no entries remain" clause (no longer true — always-on).
   - `refreshFoldChrome` doc (~L1153–1159): remove the "when the session has no tracked
     sub-agents … the summary node is torn down" sentence.
   - `unfoldAgent` doc (~L1075–1085) and `foldAgent` doc (~L1057): the "teardown happens
     only when no entries remain" / "always-present status bar" lines — reword to "never
     torn down; reverts to the all-zero bar."
   - `statusBarLabel` doc (~L1232–1234): "Zero counts are omitted … wrapped in `[ ]`" →
     "All four states always shown including 0 … wrapped in `| |`."

**Not touched:** `statusIcon` glyphs, `summarySuffix` (its childless-⇒`""` rule is unchanged
and still correct), `foldAgent`/`unfoldAgent`/`tickFolds` logic, agent-counting rules,
`removeSession` (still deletes the fold wholesale when the session closes), pin handling.

## User-facing behavior

- Open a fresh session with no sub-agents → a second row appears beneath it reading
  `|▶0 ‖0 ✓0 ✗0|`, its `|` aligned under the session name's first letter.
- As sub-agents come and go the four counts update in place; the bar never reshapes or
  disappears. The `+`/`-` suffix still appears only when the summary parents archived
  (TTL-folded) completed agents, and still drives expand/collapse.
- **Intended visible side effect:** every session node now has a child, so the session row
  paints a `▾`/`▸` expand marker (col0) even with zero agents. This does **not** shift the
  session name (col0 is independent of col4) — it only fills the previously-blank marker
  column. Consistent with the alignment math; worth calling out to the maintainer as the
  one new pixel.
- A zero-agent summary stays inert: `summarySuffix` returns `""` (childless), so there is
  no `+`/`-` and a body-click is a no-op (no toggle target).

## Tests (update + add) — `ui/tui/`

- **`sidebar_fold_issue484_test.go`** (the only test file referencing summary internals):
  - `suffixAfterBracket` helper (~L138): `LastIndexByte(label, ']')` → `'|'` (optionally
    rename `suffixAfterBar`; the suffix-stability tests at ~L786–808 ride on it).
  - `TestFold_StatusBarCountsMixed` (~L960, label re-checked ~L977/996): **invert** the
    zero-omit assertions — all four segments (incl. zeros) now always present; `•` still
    never present.
  - "only ✓1 ✗1, no ▶/‖" (~L1020–1024) and "▶1-only, no ✓" (~L1174) → "all four present."
  - Any `[`/`]` literal in expected labels → `|`.
- **NEW alignment render test** (in `sidebar_fold_issue484_test.go`, reusing the existing
  `renderSidebar` + `app.ReadCell` helpers): assert the summary's leading `|` column ==
  the session name's first-char column (both col4 for a non-pinned session). Guards G3
  against future indent/padding regressions.
- **`sidebar_busy_test.go`**: no `[`/`]` literal assertions on the summary today (verified —
  it asserts session markers ○/●/◐, badges, and `subAgentGlyphs`, all unaffected). Most
  assertions use `rowWith(rows, title)` (content search), so they survive an extra row.
  **Re-run and fix any test that asserts an absolute/relative row index** now that
  zero-agent sessions carry a summary row (e.g. multi-session render at ~L563–581).
- **`issue245_render_integration_test.go`** and any other snapshot/render test pinning the
  summary label or exact row layout: update expected labels/rows.
- **Row-count rule:** any test asserting "N rows for N sessions" gains **+1 row per
  session** (every session now has a summary row). Tests for sessions that already had
  sub-agents are unaffected (they already had the summary row).

## Design criteria

**(1) Goal match.** Exactly the issue: always-on summary on every session row; all four
states in fixed order each with count incl. 0; `|…|` brackets; `+`/`-` suffix unchanged and
re-derived against the `|` sentinel. No scope creep — counting rules, glyphs, fold mechanics
untouched.

**(2) Usability.** Leading `|` aligned with the session name's first char (new render test
proves it); no reshaping; inert all-zero rows; the bar is now a stable, always-readable
at-a-glance status the user expects rather than a row that blinks in and out. The one new
affordance — the `▾`/`▸` on previously-childless sessions — is consistent and harmless.

**(3) No regressions.** `refreshFoldChrome`'s counting loop is unchanged; only the teardown
short-circuit is removed. `summarySuffix`'s childless rule still yields `""`, so empty
summaries stay suffix-less. `removeSession` still tears the fold down on close (no leak).
`syncFoldSuffixes` sentinel updated in lockstep with the bracket so the suffix never
clobbers the label. Width: `|▶0 ‖0 ✓0 ✗0|` ≈ 13 cells + 4 indent + 1 suffix ≈ 18, under
`minSidebarWidth`=24 (2-digit counts ≈ 22, still fits; turbotui `Truncate` handles any
overflow gracefully). All affected tests enumerated and updated. gofmt/build/vet/lint clean,
`go test ./...` green (pre-existing `TestUserSessionSendMessage` 404 is the only accepted
failure).

**(4) Holistic / two-repo seam.** The change is purely in gogent's sidebar mirror; the
shared `internal/agent` tree is never touched (folding stays a visibility concern).
turbotui's tree already reserves the marker column identically for leaf, parent, and
`HideMarker` parent rows (`widget_tree.go:242–264`), so alignment is a turbotui-provided
invariant we consume — **no turbotui edit, no no-indent option** (either would break the
col4 alignment, not fix it). No go.mod bump, no new deps; stdlib `fmt`/`strings` only.

## Regression risks

- **Row-index test fragility.** Zero-agent sessions gain a row → any test asserting absolute
  or relative (`agentIdx == sessionIdx+1`) row positions for such sessions breaks. Mitigated
  by preferring `rowWith` content lookups and by auditing render tests (enumerated above).
- **Stale teardown callers.** Only `refreshFoldChrome` performed the empty teardown; all
  other call sites (`dismissFailed`, `tickFolds`, `unfoldAgent`) route through it and now
  correctly leave the all-zero bar standing. `removeSession` deletes the fold directly on
  close — unaffected. Verified no other path depends on the summary node vanishing.
- **Suffix sentinel desync.** If `syncFoldSuffixes` (and `suffixAfterBracket` in tests) are
  not flipped to `'|'` together with `statusBarLabel`, the suffix logic silently corrupts
  the label. Both flipped in the same change.
- **Glyph cell width.** ▶ ‖ ✓ ✗ are the same glyphs already rendered in agent rows and the
  old bracket bar, so no new width behavior is introduced.

## Open questions

1. **`▾`/`▸` on zero-agent sessions** — making every session a parent means the session row
   now always shows an expand marker. This is the intended consequence of an eager summary
   node and does not affect alignment, but it is a visible change the issue does not
   explicitly mention. Proceeding as designed; flagging for the maintainer (kloune) in the
   PR description in case a `HideMarker`-on-the-session tweak is preferred (out of scope as
   written).
2. **Pinned sessions (★).** `sessionLabelState` prefixes `"○ ★ title"`, pushing the name to
   col6 while the summary `|` stays at col4 — a pre-existing 2-col offset, unchanged by this
   work. Out of scope per the issue; will not "fix" it. Confirm the maintainer agrees it
   stays as-is.
