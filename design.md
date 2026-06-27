# Design — Blank the model/api rows when "All models" is selected (gogent #534)

## Problem

The bottom-right "Overall" stats band carries a model selector (issue #191). When
the aggregate option **"All models"** (`selectedModel == ""`) is chosen, the band
shows cluster-wide grand totals — but the two backend rows `model` and `api` still
render the *focused session's* model display name and API endpoint (issue #107 /
PR #107 behaviour, threaded via `refreshOverall` in `tui.go`). For a cluster-wide
total that single session's backend is meaningless and misleading.

**Goal:** in the aggregate view render the `model` and `api` *values* empty (blank),
while keeping the rows present (labels intact, value column aligned, panel height /
row count unchanged). When a **specific** model is selected the two rows continue to
show that model's display name + endpoint exactly as today.

## Scope

gogent-only, pure UI/TUI. Confined to:

- `ui/tui/overall_stats.go` — two coordinated edits.
- `ui/tui/overall_stats_test.go` — update affected expectations + add a focused test.

No `tui.go` edit (see "Caller / tui.go" below), no turbotui change, no new deps, no
`go.mod` bump.

## Current behaviour (the two seams)

1. `buildOverallStats` (`overall_stats.go:439`) unconditionally populates
   `o.Model` / `o.APIEndpoint` from the passed `*config.ModelConfig` whenever
   `model != nil` (lines 467–475), regardless of `selectedModel`. The caller
   `refreshOverall` (`tui.go:2954`) always passes a non-nil focused-session model
   config in the aggregate case, so the rows always fill.

2. `formatOverallStats` (`overall_stats.go:501`) renders those two rows through the
   helper `overallValue` (`overall_stats.go:521`), which substitutes a `"-"`
   placeholder for an empty value. Crucially, `overallValue` is called from **only
   two call sites** — the `model` row (line 513) and the `api` row (line 514). Every
   other metric row formats its own numeric value directly (`strconv.Itoa`,
   `formatTokens`). So any change to the empty-value rendering is *inherently scoped*
   to exactly the model/api rows; it cannot leak into the numeric rows.

## Change

### Edit 1 — `buildOverallStats`: only fill model/api when a model is scoped

Guard the existing population block on `selectedModel`:

```go
// Identify the backend only when a specific model is scoped. In the aggregate
// "All models" view (selectedModel == "") the model/api rows describe nothing
// meaningful — a cluster-wide total has no single backend — so they are left
// empty and the formatter renders them blank (issue #534). This narrows the
// #107 "show the focused session's backend" behaviour to the model-scoped view.
if selectedModel != "" && model != nil {
    o.Model = model.DisplayName
    if o.Model == "" {
        o.Model = model.Name
    }
    o.APIEndpoint = formatEndpoint(model.Endpoint, model.APIType)
}
```

Result: aggregate → `o.Model == ""` and `o.APIEndpoint == ""`; specific model →
unchanged from today.

### Edit 2 — `formatOverallStats`: render empty model/api values as blank, not "-"

After Edit 1, `overallValue`'s `"-"` placeholder is no longer wanted for these rows
in any state (aggregate *or* first-frame "no model yet"): the intended empty
rendering is now blank. Since `overallValue` is used **only** by the model/api rows,
collapsing it to identity makes it a no-op wrapper — so the minimal, clean change is
to **remove `overallValue`** and pass the raw values directly:

```go
kv("model", s.Model),
kv("api",   s.APIEndpoint),
```

`kv` = `padName(label, overallLabelWidth) + " " + value`, so an empty value yields the
padded label followed by a blank value column (e.g. `"model"` + padding + trailing
space). The label and the value-column position are preserved; only the value is
empty. The slice still returns exactly 9 rows.

Rationale for removing the helper rather than changing it to `return v`: a guard that
returns `""` for empty and `v` otherwise *is* the identity function, so keeping it
would be dead indirection. Its doc comment explicitly promised a `"-"` placeholder;
leaving a renamed/rewritten stub would be more churn and a stale contract than
inlining. (If a reviewer prefers to keep the symbol, the equivalent minimal form is
to change its body to `return v` and update the doc — functionally identical.)

Net: the `"-"` placeholder for model/api disappears in **all** empty states. The
"no session open yet" first frame now shows blank model/api instead of `-`; this is
consistent with the aggregate view and is an accepted consequence (the existing
placeholder test is updated to match).

## Caller / tui.go — intentionally untouched

`refreshOverall` (`tui.go:2986–2987`) keeps passing the focused-session model config.
After Edit 1 that argument is simply ignored when `selected == ""`, so no behavioural
change is needed there and we avoid editing `tui.go` (minimises diff and conflict
surface with sibling tasks #532, which may also touch `tui.go`).

One side effect: the comment at `tui.go:2969–2972` ("the aggregate 'all models' view
keeps following the focused session's model (issue #107)") becomes **stale** — it now
describes behaviour we are removing. I will **not** edit it (the task explicitly
prefers leaving `tui.go` alone to reduce conflict surface), but it is flagged as a
known stale comment; a one-line comment fix is the cleanest follow-up once #532 lands.
See Open questions.

## Tests (`ui/tui/overall_stats_test.go`)

Update expectations to the new aggregate contract; keep the specific-model path
covered.

- **`TestBuildOverallStats`** (line 16): passes `selectedModel == ""` with a Groq
  model config and asserts `Model=="Groq"`/`APIEndpoint=="api.groq.com"`. With the
  fix the aggregate yields **empty** Model/APIEndpoint. Update `want` to
  `Model:""`,`APIEndpoint:""`. (The numeric assertions are unchanged.)
- **`TestBuildOverallStatsModel`** (line 47): exercises model/endpoint *derivation*
  (display-name fallback, `formatEndpoint`) but currently passes `selectedModel==""`.
  To keep covering the derivation path, change the call to pass a **non-empty**
  `selectedModel` (e.g. the config's `Name`) so the populate block runs. (Otherwise
  this test would now assert empty and stop covering derivation.)
- **`TestFormatOverallStatsPlaceholders`** (line 114): currently asserts
  `"model      -"` / `"api        -"`. Update to expect **blank** values — the label
  padded with the trailing value-column space and no value (exact string includes the
  trailing space; assert on the row prefix + emptiness rather than a visually
  ambiguous trailing-space literal where practical).
- **`TestSidebarRefreshOverallStats`** (line 177): calls
  `refreshOverallStats(report, cfg, "")` and asserts `Model=="Groq"`/
  `APIEndpoint=="api.groq.com"`. Update for the aggregate case → expect empty
  Model/APIEndpoint. (Numeric/node-count assertions unchanged.)
- **`TestFormatOverallStats`** (line 84): **unchanged** — it builds an
  `overallStats` with `Model:"Groq"`/`APIEndpoint:"api.groq.com"` directly, so it
  must still render `"model      Groq"` / `"api        api.groq.com"`. This is the
  specific-model regression guard.
- **New focused test** (e.g. `TestBuildOverallStatsAggregateBlanksBackend`):
  `buildOverallStats(report, …, &config.ModelConfig{…}, "")` returns empty
  `Model`/`APIEndpoint`; then `formatOverallStats` on that result emits exactly
  `overallMetricLines` rows, the last two have the `model`/`api` labels with empty
  values, and (sanity) a specific-model build of the same config renders the name +
  endpoint. This pins both halves of the fix together.

## Invariants preserved

- `overallMetricLines` (=9) and `overallBandHeight` are **not touched**;
  `formatOverallStats` still returns exactly 9 rows.
  `TestOverallBandHeightInvariant` (line 155) keeps passing.
- `overallErrLineIdx` (=5) row ordering unchanged — no row added/removed/reordered.
- Selector / layout round-trip tests key on config name, not label — unaffected.

## Design criteria

**(1) Goal match.** Exactly the issue's ask: aggregate "All models" → model/api rows
blank (not `-`, not the focused session's backend); specific model → name/endpoint as
today. Pure fix/behaviour-narrowing of #107, no feature creep, no refactor beyond
removing the now-dead `overallValue` helper.

**(2) Usability.** Row count and panel height are identical between aggregate and
specific selection (still 9 metric rows; `overallBandHeight` constant). Labels remain
and the value column stays aligned (only the value string is empty). The user drives
this via the existing model selector — selecting "All models" now correctly surfaces
*nothing* for a per-session-only field rather than a misleading single session's
backend, which is the expected behaviour for a cluster total.

**(3) No regressions.** The empty-value change is structurally scoped to the model/api
rows only (`overallValue` had no other caller; numeric rows format independently), so
no numeric row can be blanked. `overallMetricLines`/`overallBandHeight`/
`overallErrLineIdx` untouched → height/invariant/error-highlight tests pass.
`TestFormatOverallStats` (specific model) unchanged and still green. `tui.go` untouched
→ no risk to refresh/focus plumbing. Gates: gofmt/build/vet clean, golangci-lint 0 new,
`go test ./...` green (pre-existing `TestUserSessionSendMessage` 404 the only accepted
failure).

**(4) Holistic across both repos.** Change lives entirely in gogent's TUI render/build
layer (`overall_stats.go`), the correct seam: `overall_stats.go` owns the typed view
and its formatting; the aggregate-vs-scoped decision already lives there
(`selectedModel`), so the guard belongs there too. turbotui is the generic widget
toolkit and has no knowledge of "model"/"api" rows or aggregate semantics — it
receives already-formatted strings — so no turbotui change is correct and required.
No dep / `go.mod` impact. Downstream: the only cross-file effect is the now-stale
`tui.go` comment (flagged, not edited).

## Regression risks

- **Trailing whitespace in blank rows.** `kv("model", "")` produces a line with a
  trailing space (label padding + the `" "` separator). Harmless for rendering; the
  updated placeholder test must encode the exact (trailing-space) expectation or
  assert structurally to avoid a brittle invisible-char mismatch.
- **`TestBuildOverallStatsModel` losing coverage.** If left passing `selectedModel==""`
  it would now assert empty and silently stop testing model derivation. Mitigation:
  switch it to a non-empty `selectedModel` so the derivation path still runs.
- **First-frame "no model yet" now blank instead of `-`.** Accepted and consistent;
  covered by the updated placeholder test.
- **Rebase conflicts.** Serializes after #533 (same file: the `overallAllModelsOption`
  capitalisation / constants). At the gate, rebase onto current `origin/main`
  (includes #533, possibly #532) and resolve the incidental `overall_stats.go` overlap
  around the constants block; our edits are in `buildOverallStats`/`formatOverallStats`,
  away from the #533 constant change, so overlap should be mechanical.

## Open questions

1. **Stale `tui.go:2969–2972` comment.** Leave it (zero `tui.go` diff, per task
   guidance) or apply a one-line comment fix? Recommendation: leave for this PR, fix
   as a trivial follow-up after #532 lands to keep conflict surface minimal.
2. **Keep vs remove `overallValue`.** Recommendation: remove (it becomes identity).
   If the reviewer prefers symbol stability, the equivalent minimal alternative is to
   change its body to `return v` and update its doc comment — functionally identical;
   either satisfies the gates.
