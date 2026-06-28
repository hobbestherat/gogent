# Design — Surface cache read/write breakdown in the TUI (issue #546)

Branch: `pair1/surface-cache-read-write-breakdown-in-tu` · depends on #544 (merged).

## TL;DR — what reality looks like vs. the issue framing

Two facts discovered up front reshape the plan:

1. **#544 is already merged** (`050823a`, `63d38b2`, and report-level work in `bc5232c`).
   `stats.ConnectorStat.CacheWriteTokensIn` already exists and flows end to end:
   `model.StatsSnapshot.TotalCacheWriteTokensIn` → `stats.FromSnapshot` →
   `ConnectorStat.{Add,Sub}` → and the **CSV column `cache_write_tokens_in` is already
   emitted** by `writeConnectorCSV` (`internal/stats/stats.go:355`). So the issue's stated
   gogent deliverable ("add a CSV column `cache_write_tokens_in`") is **already done**.
   Re-adding it would create a duplicate column — a regression to avoid.

2. **`ui/tui/overall_stats.go` lives in gogent, not turbotui.** The issue (and the orchestration
   note) describe a cross-repo, gogent-first sequence where "turbotui renders it
   (ui/tui/overall_stats.go)". That file is in **this repo** (`gogent/ui/tui/overall_stats.go`).
   The sibling `turbotui` is a generic terminal-widget primitive library (`cell.go`, `screen.go`,
   `width.go`, Buttons, `tv.Tree`) — it has **no `internal/stats` dependency and no Overall
   panel**. Rendering a read/write text row needs only existing primitives. **No turbotui change
   is required, and no `go.mod` bump.** This issue is, in practice, **gogent-only.**

The net remaining work is therefore: add a small report-level convenience method in
`internal/stats`, and surface the read/write split in two gogent TUI renderers.

## Goal (restated)

The Overall panel currently shows one row, `cache hit  78%`
(`overallStats.CacheHitPct`, rendered at `overall_stats.go:520`). It hides whether a high
input-token count is cheap cache *reads* or expensive cache *writes* (the #545 lookback-miss
symptom). We want the panel to show both the read effectiveness and the write spend, degrade
cleanly to `0%` for providers that never write (OpenAI/DeepSeek/Gemini/Z.AI/OpenRouter), and
make per-session/per-connector numbers reachable.

## Exact changes

### A. gogent — `internal/stats/stats.go` (report-level surface this issue owns)

- **Add `func (c ConnectorStat) CacheWritePercent() int`**, mirroring the existing
  `CacheHitPercent()` (`stats.go:199`): `CacheWriteTokensIn / TokensIn * 100`, guarded by
  `TokensIn <= 0 → 0`. This is the clean, unit-testable surface the display layer reads,
  exactly as it already reads `CacheHitPercent()`. (Field, `Add`, `Sub`, `FromSnapshot`, and the
  CSV column already exist from #544 — **no edits there**, to avoid a duplicate CSV column.)

No other `internal/stats` change. `SessionModelStat.Connector` (`stats.go:103`) and
`ModelStat.Connector` (`stats.go:248`) already carry `CacheWriteTokensIn`, so per-session and
per-model write numbers are already in the report and already in the CSV
(`writeConnectorCSV` is called per `session:<id>` and per `model:<name>`).

### B. gogent — `ui/tui/overall_stats.go` (the Overall panel — "renders it")

The issue's illustrative one-liner `cache: 78% read (12.4k) · 9% write` is **~34 cols wide**
and will not fit: the sidebar is `defaultSidebarWidth = 32` / `minSidebarWidth = 24`
(`sidebar.go:20,27`) and `drawOverall` clips each metric row to `contentW = abs.W - 3`
(`sidebar.go:936`) via `truncateRunes` (`:958`). So split into **two narrow rows** that replace
the single `cache hit` row:

```
cache rd   78% 12.4k
cache wr   9% 1.1k
```

**Exact width budget.** At the `minSidebarWidth = 24` floor the clip ceiling is
`contentW = 24 − 3 = 21` cols (not 24). Each row is `padName(label, overallLabelWidth=10)` + `" "`
+ value. Worst-case value is `"100% 9.9M"` = 9 cols, so the row is `10 + 1 + 9 = 20 ≤ 21` —
**1 col of slack**, no truncation. (The earlier "safely inside 24" was loose; the real ceiling is
21 and the margin is one column.)

**Label choice — `cache rd` / `cache wr` (decided).** These are the standard I/O read/write
abbreviations and keep the value column aligned at `overallLabelWidth=10` with 2 cols of label
slack. We **reject** the clearer `cache read` / `cache write`: `"cache write"` is 11 cols, one over
`overallLabelWidth`, which both shifts the write row's value column out of alignment with the read
row *and* pushes the worst case to `11 + 1 + 9 = 21` = the clip ceiling exactly (zero slack at the
24-col floor). Width forces the abbreviation; `rd`/`wr` is the least-ambiguous pair that fits.

Concretely:

- **`overallStats` struct** (`:22`): keep `CacheHitPct int` semantics as the read %, but rename
  for clarity to `CacheReadPct int` *only if cheap*; otherwise keep `CacheHitPct` and **add**
  `CacheWritePct int`, `CacheReadTokens int`, `CacheWriteTokens int`. (Decision: **keep
  `CacheHitPct`** — it is referenced by name in tests at `overall_stats_test.go:30,92,212` — and
  add the three new fields. Minimizes churn.)
- **`buildOverallStats`** (`:440`): populate the new fields from `prim` and, in the
  model-scoped branch, from `ms.Connector` — `CachedTokensIn`, `CacheWriteTokensIn`,
  `CacheWritePercent()`.
- **`formatOverallStats`** (`:509`): replace the single `kv("cache hit", …)` row (`:520`) with
  two rows: `kv("cache rd", pct+"% "+formatTokens(read))` and
  `kv("cache wr", pct+"% "+formatTokens(write))`.
- **Bump `overallMetricLines` 9 → 10** (`:38`) and update its doc comment. `overallBandHeight`
  (`:54`) derives from it automatically. `overallErrLineIdx = 5` (`:65`) is **unchanged** — the
  `errors` row keeps its position; the two cache rows are appended after it, before
  `model`/`api`.

### C. gogent — `ui/tui/statistics_render.go` (per-connector full detail; goal #2)

`writeConnector` (`:185`) renders the full per-backend block in the Statistics dialog Overview.
**It is called only for the cluster aggregate** `Totals.Primary` / `Totals.Fast`
(`statistics_render.go:54,57`) — **never per session**. It already prints `Cached in:` and
`Cache hit:` (`:195–197`). **Add a parallel `Cache wr: <tokens>` / `Cache wr %: <pct>%`** on the
same block so the dialog shows write tokens and write share next to the read figures.

**Be precise about what goal #2 actually buys in the TUI:** this adds a *cluster-aggregate*
read/write block to the dialog, plus the model-selector scoping already in the Overall panel
(section B reads `ms.Connector` per selected model). **True per-session cache write is reachable
only via the CSV** — `writeConnectorCSV` is emitted per `session:<id>` (`stats.go:307`), so the
data is exported per session, but the dialog renders it aggregated. That meets the issue's stated
minimum ("per-session numbers reachable") but does **not** put a per-session cache figure in the
Sessions table. The `Sessions` table (`renderStatsSessions:78`) is already 8 columns wide and
width-constrained, so **no new column there** — the CSV remains the per-session path. If the
maintainer wants per-session *on screen*, that is a follow-up (a 9th column or a detail expander),
called out in Open questions.

### D. Tests to update (in the build phase, not now)

- `ui/tui/overall_stats_test.go`: the `want` slice and the `"cache hit  25%"` literal
  (`:102`) become the two new rows; the line-count assertion vs `overallMetricLines`
  (`:170,176`) stays green automatically once the const is bumped to 10 and two rows are emitted.
- New `internal/stats` test: `CacheWritePercent()` arithmetic + `TokensIn==0` guard.
- New `ui/tui` test: write==0 provider renders `cache wr   0% 0` cleanly (degrade gate).

## The four design gates

**(1) Goal match.** Overall panel gains an explicit read **and** write breakdown (effectiveness +
spend); `internal/stats` exposes `CacheWritePercent()`; the Statistics dialog surfaces the
*cluster-aggregate* read/write block, with model-scoped numbers via the Overall panel's selector.
Per-session write is reachable **via the CSV** (`writeConnectorCSV` per `session:<id>`), the issue's
stated minimum — not as an on-screen Sessions-table figure (a deliberate, width-driven scope line;
see section C and OQ#4). No scope creep — no new cost model, no provider changes (those were
#544/#556). The CSV column the issue asked for already exists, so we deliberately *don't* re-add it.

**(2) Usability.** Two short rows fit at the 24-col minimum sidebar: the clip ceiling there is
`contentW = 24 − 3 = 21` and each row is ≤ 20 cols (worst-case `"100% 9.9M"` value), so **1 col of
slack, no truncation** — not the looser "inside 24" stated earlier. The user sees `cache rd` (is
caching saving money?) and `cache wr` (am I paying a write premium every turn — the #545 symptom?);
`rd`/`wr` are the standard read/write abbreviations, chosen because the spelled-out `cache write`
overflows `overallLabelWidth` and breaks both alignment and the width budget (section B). Both rows
draw from one consistent source (the primary connector). Nothing is silent: write tokens get their
own labelled row.

**(3) No regressions.** Risks and mitigations:
- *Band desync.* `overallMetricLines` must equal the emitted row count. It is pinned by
  `overall_stats_test.go:170,176`; bumping the const to 10 and emitting two cache rows keeps both
  sides equal. `overallBandHeight` derives, so it follows automatically.
- *errLineIdx drift.* `overallErrLineIdx = 5` points at `errors`; cache rows are inserted **after**
  it, so it does not move. Pinned by `:179`.
- *Duplicate CSV column.* `cache_write_tokens_in` already exists — we must **not** touch
  `writeConnectorCSV`. Flagged so the build phase doesn't re-add it.
- *Width clipping.* Worst-case row 20 cols ≤ 21-col clip ceiling at the 24-col floor
  (`contentW = sidebarW − 3`, `sidebar.go:936`); verified against `overallLabelWidth=10`.
- *Non-write providers.* `CacheWriteTokensIn==0 → CacheWritePercent()==0 → cache wr 0% 0`. Clean.
- *`TestBuildOverallStats` struct-equality* (`overall_stats_test.go:31`) stays green by
  coincidence-of-zero: the test report sets no write tokens, so the three new `overallStats` fields
  are zero on both the produced and expected struct. No edit needed there — but the build phase
  should add a case with write tokens set to actually exercise the new fields, not rely on the zero.
- Existing `CacheHitPct`-named test references are preserved by keeping the field name.

**(4) Holistic / cross-repo seam.** The decisive finding: **turbotui needs no change.** The "data
in gogent, render in turbotui" split in the issue is based on a misread of where `overall_stats.go`
lives (it is gogent's). turbotui is a provider-agnostic widget library with no stats dependency, so
there is nothing to render there and no `go.mod` bump. Keeping the cache-percent math in
`internal/stats` (`CacheWritePercent()` beside `CacheHitPercent()`) puts the logic in the neutral
package both the panel and the dialog already read — the right seam. The change stays inside
`internal/stats` + `ui/tui`, loosely coupled to the #542 (ui/tui) and model-cache work as the
orchestration note anticipated.

> **BLOCKING ALIGNMENT ITEM (only the maintainer can settle).** This gogent-only shape is
> technically correct but **departs from the task's written acceptance gate**, which demands a
> turbotui PR, a recorded turbotui merge SHA, and a `go.mod` bump. We assert the cross-repo half is
> moot because the target file is gogent's and turbotui has no stats code — but that inverts a stated
> deliverable, so it must be confirmed by kloune (the issue author) before building. See OQ#1 for the
> two concrete contingency plans.

## Verification / gate

- `gofmt`, `go build ./...`, `go vet ./...`, `golangci-lint` (v2, 0 new), `go test ./...`
  (gogent: pre-existing `TestUserSessionSendMessage` 404 is the only tolerated failure).
- New stats test (write %), new overall_stats test (write==0 degrade), updated band/row tests.
- No turbotui build needed (out of scope); local gogent gate is authoritative.

## PR / merge plan

**Conditional on OQ#1.** Default (path 1a, recommended): single gogent PR, `Closes #546`, no
turbotui PR. Rebase onto `origin/main` at the gate. Because the gogent half is self-contained (no
dependency on an unmerged turbotui widget), the gogent-first sequencing concern in the orchestration
note is moot — there is no second repo to sequence against. If path 1b is chosen instead, revert to
the orchestration note's sequence: turbotui widget PR → merge → record SHA → gogent `go.mod` bump →
gogent display PR, with the gogent PR `Refs #546` until both land.

## Open questions

1. **[BLOCKING] Is turbotui involvement actually intended, or is the gogent-only shape accepted?**
   This is the one item that must close before building — it inverts the written acceptance gate.
   Two concrete contingency plans:
   - **(1a) Maintainer confirms gogent-only (our recommendation, expected).** Build sections A–D as
     written; single gogent PR `Closes #546`; no turbotui PR, no SHA to record, no `go.mod` bump.
     Update the issue/acceptance note to reflect that the file was always gogent's.
   - **(1b) Maintainer wants a reusable widget in turbotui.** Then turbotui must stay
     stats-agnostic: add a small primitive (e.g. a `CacheBreakdown`/two-row renderer taking plain
     `(readPct, readTokens, writePct, writeTokens int)` ints, **not** `stats.ConnectorStat`), merge
     it to turbotui main first, record its SHA, bump gogent's `go.mod` to it, then have
     `formatOverallStats` call the new primitive. This is the original gogent-first cross-repo
     sequence — larger, and only justified if the breakdown is wanted by other turbotui consumers,
     of which there are currently none.
2. **Per-session cache *on screen* (vs. CSV-only).** Goal #2 is met at the issue's minimum via the
   CSV + model-selector scoping. If kloune wants a per-session figure visible in the TUI Sessions
   table (`renderStatsSessions`), that needs a 9th column or a row detail-expander — a follow-up, not
   in this change. Flag for preference.
3. **One packed row vs. two rows.** We chose two rows for width safety. If the maintainer prefers the
   issue's single `cache: 78% read · 9% write` line, it only fits if we drop the read-token
   magnitude; flag for preference. (Two rows keeps the magnitude and `overallErrLineIdx` stable.)
4. **Field naming.** Keep `CacheHitPct` (minimal churn, test-referenced) vs. rename to
   `CacheReadPct` for symmetry with new `CacheWritePct`. Defaulting to keep; trivial to revisit.
5. **Write magnitude on row.** We show `formatTokens(write)` on the `cache wr` row (write *spend* is
   the point of the issue). Drop it if the column proves too tight in practice.
