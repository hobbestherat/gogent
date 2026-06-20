# Issue #191 — Stable per-model Overall stats + model selector

## Problem (bug)
The Overall panel reads `report.Totals.Primary`, built in `Gogent.Statistics()` from
`UserSession.ConnectorStats()`, which **sums `StatsSnapshot()` over every agent's live
`*ModelConnection`**. That connector is:
- rebuilt & zeroed every turn (`buildConnection` + `ThoughtTrain.Resume`), kept cumulative
  only by the fragile `ModelStats.Carry` hack;
- **shared** by sub-agents (same pointer) → summed once per agent → **double-counted**;
- in-memory only → **resets to 0 on restart**.

So counters jump around / snap to zero.

## Fix — stable per-model accumulator on UserSession
Mirror the always-monotonic `tokenCountIn/Out` / `perModelTokens += ` pattern, but for the
connector-only metrics (requests/errors/cache/timeouts/latency).

- **`model.StatsSnapshot.Sub`** (new): element-wise subtraction, for per-read deltas.
- **`UserSession.perModelConn map[string]model.StatsSnapshot`** + `lastConnSnap`: stable,
  monotonic per-model connector accumulator keyed by `primaryModel`.
- **`recordConnectorUsage(conn)`**: read `conn.StatsSnapshot()` once, `delta = cur - lastConnSnap`,
  fold `delta` into `perModelConn[primaryModel]`, set `lastConnSnap = cur`. A negative delta
  (connector was rebuilt without carry) is treated as a fresh baseline (`delta = cur`) so the
  accumulator only ever grows. Called from `modelRoundTrip` (the single choke point used by the
  root loop *and* every sub-agent), so:
  - sub-agents fold their own deltas live while running → **no double-count** (one connector read,
    not N agent reads);
  - model switch: `Carry` keeps `cur` monotonic; the carried base produces a zero delta and new
    activity is attributed to the new `primaryModel`;
  - `Carry` is left untouched → `model_session_resume_test.go` stays valid.
- **`ConnectorStats()`** now returns the aggregate over `perModelConn` (sum of all model buckets)
  instead of summing live connectors → no reset, no double-count. `FastConnectorStats` unchanged.
- **`ModelConnectorStats(name)`** + **`PerModelStats() []ModelUsage`** expose per-model data
  (tokens from `perModelTokens`, connector from `perModelConn`).

Restart note: like `tokenCountIn`, the in-memory accumulator is not persisted, so a restored
session starts fresh (out of scope here; documented). The other three reset causes are fixed.

## Report — per-model filtering
- `stats.ModelStat` gains `Sessions`, `SubAgents`, `Connector ConnectorStat` (+ CSV rows).
- `stats.SessionRow` gains `PrimaryModel`.
- `stats.Report.ModelByName(name) (ModelStat, bool)`.
- `Statistics()` sets `SessionRow.PrimaryModel`, and builds `rep.Models` with per-model tokens +
  connector (from `PerModelStats`) and per-model session/sub-agent counts (keyed by each session's
  current `primaryModel`). `Totals.Primary` stays the grand total (back-compat aggregate).

## UI — model selector at top of Overall band
- `overall_stats.go`: `buildOverallStats(report, sessions, subAgents, modelCfg, selectedModel)`.
  `selectedModel == ""` → aggregate (today's behaviour, `Totals.Primary` + passed counts);
  otherwise scope tokens/requests/errors/cache + sessions/sub-agents from `ModelByName`.
  `overallSelectorLines = 1`; `overallBandHeight = overallSelectorLines + overallMetricLines + 2`.
- `sidebar.go`: a `tv.Select` (`["all models", <model display names>]`) added as a panel child,
  laid out on the band's top row above the separator; `drawOverall` shifts separator/title/metrics
  down by one. Maps display label ↔ config name via parallel `overallModelNames`. `OnChange`
  persists + reschedules the Overall refresh. `refreshOverallStats(report, modelCfg, selectedKey)`.
- `tui.go`: `refreshOverall` resolves the scoped model config + config-name key and passes them in.
- persistence: `Layout.OverallModel string` (config name; "" = all), captured/restored like
  `SidebarWidth` (#175).

## Tests (GLM writes): counters monotonic across switch + sub-agent (no reset/double-count);
selector scopes to one model & "all" = total; selection persists via layout round-trip; keep
`model_session_resume_test.go` cumulative-across-switches; keep `TestOverallBandHeight` in sync.
