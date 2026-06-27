# Design — Issue #533: Capitalise the "All models" selector option

## Summary

The model-selector dropdown atop the bottom-right **Overall** stats band renders its
aggregate option as `all models` (lowercase). Issue #533 asks to render it as
`All models` (capital A) so it reads consistently with the capitalised **Overall**
title. This is a **pure display-label change** — text only, no behaviour, no scope creep.

The label lives in a single named constant, `overallAllModelsOption`, in
`ui/tui/overall_stats.go`. Both consumers (`sidebar.go`) read that constant, so the
one-line change propagates to the live selector and to the rebuilt option list with no
call-site edits.

## Exact files / functions touched

### gogent (github.com/hobbestherat/gogent)

1. **`ui/tui/overall_stats.go` (~line 58)** — the only production change:
   - `const overallAllModelsOption = "all models"` → `"All models"`.
   - Touch up the doc comment on that const (lines 55–58) and, optionally, the
     prose comments at line 56 ("across all models") and line 431
     (`// selectedModel scopes the metrics ... When empty (the "all models" option)`)
     only where wording now reads awkwardly. These are comments; they have no runtime
     effect — keep edits minimal and confined to wording that references the *label*.
     The phrase "all models" used descriptively (not as the label) can stay lowercase.

2. **`ui/tui/overall_stats_selector_test.go`** — test-literal upkeep only:
   - Line 114: `wantOpts := []string{overallAllModelsOption, ...}` already references the
     constant → **auto-updates**, no edit.
   - Line 138: error-message string `want [all models, bare-model]` — cosmetic text in a
     `t.Errorf` format, not an assertion. The actual assertion on line 137 checks
     `Options[1] != "bare-model"` (a real model, not the aggregate), so it is unaffected.
     Update the message text to `[All models, bare-model]` for accuracy/consistency.
   - Lines 53, 219: descriptive comments — leave as-is or align wording; no functional
     impact.
   - Grep `ui/tui/*_test.go` for the literal `"all models"` before finalising; only
     change a *string-equality assertion* against the aggregate label (currently none
     exist — all such checks go through the constant).

### Consumers that pick up the change automatically (no edit)

- `ui/tui/sidebar.go:477` — `newSelect(wb.desktop, []string{overallAllModelsOption}, ...)`
- `ui/tui/sidebar.go:828` — `rebuildModelOptions`: `options := []string{overallAllModelsOption}`

### turbotui (github.com/hobbestherat/turbotui)

**No change.** The selector widget (`newSelect` / `Select`) is a generic
string-list dropdown; it renders whatever option strings gogent passes. The label
string is owned entirely by gogent. The repo seam is respected — nothing crosses it.

## User-facing behaviour

- Before: dropdown's first/aggregate entry reads `all models`.
- After: it reads `All models`.
- Everything else is identical: it is still the default (index 0) selection, still maps
  to config name `""`, still scopes the band to the cluster-wide grand total, and the
  dropdown ordering and per-model entries are unchanged.

## The 4 design gates

**(1) Goal match.** Exactly the issue's ask: one visible label gains a capital A for
consistency with the "Overall" title. It is a fix, not a feature or refactor — a single
constant value changes. No scope creep (no other labels, no metric-row labels like
"sessions"/"tokens in" which the issue does not mention).

**(2) Usability.** Improves visual consistency with the capitalised "Overall" header.
The user still drives the same dropdown identically; default selection, navigation, and
scoping behaviour are unchanged. The aggregate view is still surfaced as the first,
clearly-labelled option.

**(3) No regressions.** The persisted layout (`Layout.OverallModel`) is keyed on the
**config name**, not the display label: `sidebar.go` keeps an index-parallel
`overallModelKeys` slice (`""` at index 0 for the aggregate) and
`selectedOverallModel`/`setSelectedOverallModel` read/write *keys*, never the label
string (sidebar.go:823–870). So capitalising the label cannot touch
`Layout.OverallModel`, the round-trip, or restore. Layout tests
`TestWorkbench_OverallModelLayoutRoundTrip` and
`TestWorkbench_OverallModelLayoutAggregateDefault` key on config name → stay green
(do not modify). `buildOverallStats` branches on `selectedModel == ""` (the key), not
the label (overall_stats.go:454) → aggregate logic unchanged. Selector test
`wantOpts` references the constant → auto-updates. Only the `t.Errorf` message text is
touched. gofmt/build/vet/golangci-lint/`go test ./...` expected clean; the only
acceptable pre-existing failure is `TestUserSessionSendMessage` (404).

**(4) Holistic / cross-repo.** Change is in the right place — the single constant that
owns the label, with both consumers already routed through it (no duplicated literals to
drift). gogent-only; no turbotui change, no new deps, no go.mod bump. Downstream effect
on turbotui: none — it renders the passed string verbatim. The data/config seam
(label = presentation, config name = identity) is preserved exactly.

## Regression risks (and why they don't bite)

- *Layout key drift* — mitigated: keys are config names, label is cosmetic; verified in
  sidebar.go:845–870 and the layout tests.
- *Hidden literal assertion on the label* — mitigated by grepping test files; all
  current checks route through `overallAllModelsOption`.
- *Serialization with #534* — both edit `overall_stats.go`; must NOT run concurrently.
  Disjoint from #532. At the gate, rebase onto current origin/main.

## Open questions

- The mixed-case prose ("across all models", "the 'all models' option") in comments: I
  will capitalise only where the comment is naming the *visible label*; descriptive
  English usage stays lowercase. If the maintainer prefers literal consistency
  everywhere the phrase appears, that is a trivial follow-up — flag at review. No
  blocker.
