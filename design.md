# Design — Widen sub-agent summary bar to 2-space entries & swap waiting glyph ‖ → ⏸

Issue #515 (kloune). Follow-up to #510. Two cosmetic changes to the per-session
sub-agent summary bar in the sidebar "Sessions & Agents" tree.

## Summary of change

| | before | after |
|---|---|---|
| waiting glyph | `‖` U+2016 DOUBLE VERTICAL LINE | `⏸` U+23F8 DOUBLE VERTICAL BAR (pause) |
| inter-entry gap | 1 space | 2 spaces |
| bar shape | `\|▶2 ‖1 ✓5 ✗1\|` | `\|▶2  ⏸1  ✓5  ✗1\|` |

The glyph swap happens in **one place** — `statusIcon(agent.StatusWaiting)` — so it
propagates to *both* the summary bar (which calls `statusIcon`) and the individual
sub-agent row icons. This is intended and consistent. The spacing change is local to
`statusBarLabel`.

**gogent-only. No turbotui change. No new deps. No go.mod bump.**

## Files & functions touched (gogent — all under `ui/tui/`)

### Production code — `ui/tui/sidebar.go`
1. **`statusIcon`** (~:1589-1594) — `case agent.StatusWaiting: return "⏸"` (was `"‖"`).
   Single source of the glyph for both bar and rows.
2. **`statusBarLabel`** (~:1239-1245) — change the four single-space gaps in the
   format string to double-space:
   `"\|%s%d  %s%d  %s%d  %s%d\|"`. Glyph args unchanged (still
   `statusIcon(...)` for each of running/waiting/completed/failed). Closing `|` and the
   suffix mechanism are untouched.
3. **Doc/comment literals** mentioning `‖` or the `\|▶0 ‖0 ✓0 ✗0\|` shape →
   `⏸` / `\|▶0  ⏸0  ✓0  ✗0\|`:
   - `:54` lifecycle-icon list `(▶/‖/✓/✗/•)` → `(▶/⏸/✓/✗/•)`
   - `:84` and `:87` block comment examples (`"\|▶2 ‖1 ✓5 ✗1\|"`, `"\|▶0 ‖0 ✓0 ✗0\|"`)
   - `:607`, `:1169` prose `\|▶0 ‖0 ✓0 ✗0\|`
   - `:1234` statusBarLabel doc `(▶running ‖waiting …)` → `⏸waiting`, and note 2-space sep
   - `:1451`, `:1550` prose lists `(▶ ‖ ✓ ✗ •)` → `(▶ ⏸ ✓ ✗ •)`
   These are comments only — no behavioral effect, but kept truthful (the repo treats
   these glyph lists as authoritative documentation of the icon set).

### Tests — update every literal that pins the bar text or the `‖` glyph
Real test names verified against the files (the earlier draft used wrong names — fixed):
- `ui/tui/sidebar_summary_issue510_test.go` (authoritative `statusBarLabel` pin):
  - `allZeroBar` const (`"\|▶0 ‖0 ✓0 ✗0\|"` → `"\|▶0  ⏸0  ✓0  ✗0\|"`).
  - `TestStatusBarLabel_AllFourAlwaysPresentIncludingZeros` (:41) — table `want` strings.
  - `TestStatusBarLabel_FixedOrder` (:68) — order slice element `"‖1"`→`"⏸1"`.
  - `TestStatusBarLabel_PipesNotBrackets` (:86) — **the exactly-two-`\|` guard**;
    **comment only** ‖→⏸, the `Count(bar,"\|")==2` assertion is UNCHANGED and must still
    pass. (This is the load-bearing pipe-sentinel regression guard.)
  - `TestStatusBarLabel_NeverEmitsIdleGlyph` (:105) — unchanged (still asserts no `•`).
  - `TestStatusBarLabel_MultiDigitCounts` (:121) — a **second** `Count(bar,"\|")==2`
    guard plus multi-digit `want` strings; update the `want` literals, leave the
    pipe-count assertion intact.
  - `TestSummary_AlwaysOnSurvivesFullLifecycle` (:206) and
    `TestSummary_AlwaysOnRenderEverySession` (:278) — `check(...)` bar literals.
  - `TestSummary_BarFitsAtMinSidebarWidth` (:453) — passes **transitively** via the
    `allZeroBar` const update; no literal edit needed, but listed because it is the test
    that resolves the min-width truncation question (see Open questions).
  - `TestSyncFoldSuffixes_PipeSentinelNotWaitingGlyph` (:522) — `wantBar` literal +
    comment naming the glyph; purpose unchanged.
  - Dismiss-failed assertions (~:603, :614) — bar literals.
- `ui/tui/sidebar_fold_issue484_test.go`: header doc; the suffix-after-bracket helper
  comment (`‖` U+2016 → `⏸` U+23F8); and every literal bar assertion / `seg` slice
  element (`‖`→`⏸`, single→double space).
- `ui/tui/sidebar_busy_test.go`: `subAgentGlyphs` slice element `"‖"`→`"⏸"`; comment.
- `ui/tui/sidebar_watchers_test.go`: comment glyph list.
- Any snapshot/integration test pinning the label — scan
  `ui/tui/issue245_render_integration_test.go` and the whole `ui/tui` tree for `‖` and
  `\|▶` and update. (Implementation step will `grep -rn '‖'` across `ui/tui` to ensure
  nothing is missed rather than relying on the line numbers above, which are advisory.)

### Not touched
turbotui (read-only sibling); `go.mod`/`go.sum`; the three other glyphs (`▶ ✓ ✗`), the
`+`/`-` fold-suffix glyphs, the wrapping pipes, and the always-on behavior from #510.

## User-facing behavior
In the "Sessions & Agents" sidebar, every session row carries a fixed-width summary
child like `\|▶2  ⏸1  ✓5  ✗1\|`. After this change the "waiting" entry shows a pause
glyph `⏸` instead of `‖`, and the four entries are separated by two spaces instead of
one (slightly more breathing room, easier to scan). Individual waiting sub-agent rows
also lead with `⏸` instead of `‖`. The bar grows by exactly 3 columns (three extra
inter-entry spaces; the glyph swap adds 0). The trailing fold suffix (`+`/`-`/``) still
sits immediately after the closing `|`.

## Design criteria

**(1) Goal match.** Exactly the two cosmetic changes the issue asks: glyph swap in
`statusIcon(StatusWaiting)` (propagating to bar + rows, as the issue intends) and
1→2-space inter-entry spacing in `statusBarLabel`. No scope creep — no change to the
other glyphs, the pipes, the suffix, or the always-on logic.

**(2) Usability.** This is a render-only cosmetic change; there is no dialog/input to
drive. `⏸` (pause) is a clearer mnemonic for "waiting" than the ambiguous `‖`. The
wider spacing improves legibility. The bar stays a single fixed-width line; the leading
`|` stays at the session-name first-char column. Width budget: the bar grows +3 cells —
need to confirm it isn't clipped by the widget's `Truncate(…,"…")` at the sidebar
minimum width, especially behind long session names. Mitigated by the fact that the
summary node is a child row rendered on its own line (not appended to the session name),
so it has the full sidebar width; +3 cells on a `\|▶N  ⏸N  ✓N  ✗N\|`-style line is well
within typical sidebar width. **Resolved, not merely flagged:** the new bar is 16 cells
(`|▶0  ⏸0  ✓0  ✗0|`; was 13) on its own indented child line, and `minSidebarWidth = 24`
(sidebar.go:27), so it fits with headroom even behind the child indent. This is locked
by the existing `TestSummary_BarFitsAtMinSidebarWidth` (:453), which passes transitively
once the `allZeroBar` const is updated.

**(3) No regressions.** Two invariants are the load-bearing ones, both preserved:
  - *Pipe-sentinel contract.* `syncFoldSuffixes` re-derives the fold suffix by
    `strings.LastIndexByte(base, '|')` to find the closing pipe (sidebar.go:1225). `⏸`
    is bytes `E2 8F B8` — contains no `0x7C` (`|`) — so the closing pipe is still the
    last `|`. Guarded by `TestStatusBarLabel_PipesNotBrackets` (:86) and
    `TestStatusBarLabel_MultiDigitCounts` (:121) — both assert `Count(bar,"|")==2`,
    assertions unchanged — and `TestSyncFoldSuffixes_PipeSentinelNotWaitingGlyph` (:522,
    literal updated, purpose unchanged).
  - *Alignment / fixed width.* `⏸` measures width-1 in turbotui (verified: U+23F8 is
    absent from `wideRanges` in `turbotui/width.go` — it sits in the gap between the
    hourglass `0x23F3` and `0x25FD`, so `RuneWidth` returns 1, same as `‖`). The only
    width delta is +3 from the extra spaces — uniform across every bar, so columns stay
    aligned.
  All literal-pinning tests across the `ui/tui` suite are updated in lockstep. Build,
  `gofmt`, `go vet`, and whole-repo `golangci-lint` (0 new issues) must stay clean;
  `go test ./...` green except the pre-existing `TestUserSessionSendMessage` 404, which
  is the only acceptable failure.

**(4) Holistic / cross-repo seam.** The change lives entirely on the gogent side of the
seam. The seam contract is: gogent emits label *strings*; turbotui measures and paints
them via `RuneWidth`. The swap is safe *because* `RuneWidth('⏸') == RuneWidth('‖') == 1`
— so the fixed-width bar that #510 established stays aligned with no framework change.
No go.mod bump (turbotui's API is unchanged and we depend on no new behavior). Downstream
effect on turbotui: none. The one risk at the seam is **emoji presentation**: a bare
`⏸` is text-presentation width-1, but if a *real terminal* renders it as a colored
width-2 emoji, turbotui's cell model (which says width-1) desyncs and the line shifts.
We use **plain `⏸` (no variation selector)**. We must NOT append the emoji VS `⏸️`
(U+FE0F) — that explicitly requests emoji presentation. If a target terminal is observed
to render plain `⏸` as wide emoji, the remediation is the **text** VS `⏸︎` (U+FE0E,
which turbotui folds to width-0 via the `0xFE00–0xFE0F` rule, keeping total width 1) —
never the emoji VS. This is flagged, not preemptively applied.

  *Nuance:* turbotui folds **both** variation selectors (U+FE0E *and* U+FE0F) to width-0
  in its cell *model* (`isZeroWidth` covers `0xFE00–0xFE0F`, width.go:51-53). So the
  `⏸️` (FE0F) hazard is purely a *real-terminal presentation* problem (terminal paints
  width-2 while the model says width-1) — not a width-table disagreement. The text VS
  `⏸︎` (FE0E) is the safe fallback precisely because it nudges terminals toward
  text presentation while the model already treats it as width-0.

## Orchestration note
Pure `ui/tui/sidebar.go` — collides with any other `ui/tui` task; must not run in
parallel with #509. At the gate: rebase onto current `origin/main` (will include #509 +
already-merged #510) and resolve any incidental `sidebar.go` overlap before merging.

## Verification checklist (for the build phase)
- `grep -rn '‖' ui/tui` returns nothing after the change (catches missed literals).
- `grep -rn 'U+FE0F\|️' ui/tui/sidebar.go` confirms no emoji VS was introduced.
- `go build ./...`, `gofmt -l`, `go vet ./...`, `golangci-lint run` clean.
- `go test ./ui/tui/...` green; `go test ./...` green modulo the known 404.
- `TestStatusBarLabel_PipesNotBrackets`, `TestStatusBarLabel_MultiDigitCounts`, and
  `TestSyncFoldSuffixes_PipeSentinelNotWaitingGlyph` pass (the pipe-count guards).
- `TestSummary_BarFitsAtMinSidebarWidth` passes.

## Open questions
- **Terminal emoji rendering of `⏸`:** none of our tooling can prove every end-user
  terminal renders plain `⏸` as text width-1. We proceed with plain `⏸` per the issue
  and document the `⏸︎` (text VS) fallback. No action unless a width-2 rendering is
  reported — flagged, not applied.
- ~~Truncation at minimum sidebar width~~ — **resolved** (not an open question): 16-cell
  bar vs `minSidebarWidth = 24`, locked by `TestSummary_BarFitsAtMinSidebarWidth`. No
  follow-up needed.
