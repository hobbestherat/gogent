package ui

import (
	"net/url"
	"sort"
	"strconv"

	"gogent/internal/config"
	"gogent/internal/stats"
)

// overallStats is the typed aggregate the right-hand "Overall" panel renders
// (issue #53): cluster-wide totals across every open session. It is built from
// the Statistics report's grand totals joined with the sidebar's own session /
// sub-agent node counts, so the panel renders one small typed struct rather than
// an untyped map (cross-ref the GetStats typing point in issue #6).
//
// Model and APIEndpoint (issue #107) identify the focused session's backend so
// the global state is visible at a glance: the model's display name and a short
// host/provider label for the endpoint it talks to.
type overallStats struct {
	Sessions    int    // open sessions (sidebar nodes)
	SubAgents   int    // sub-agents tracked across all sessions (sidebar nodes)
	TokensIn    int    // primary-model input tokens, summed across sessions
	TokensOut   int    // primary-model output tokens, summed across sessions
	Requests    int    // primary-model backend requests
	Errors      int    // primary-model backend errors
	CacheHitPct int    // prompt-cache hit share of input tokens, whole-number %
	Model       string // focused session's model display name ("" before one is open)
	APIEndpoint string // focused session's endpoint host or provider label
}

// overallMetricLines is the number of metric rows formatOverallStats emits
// beneath the "Overall" title. overallBandHeight derives from it, and a test pins
// the two together so a formatter change cannot silently desync the reserved
// band height.
const overallMetricLines = 9

// overallSeparatorLines is the height of the thin horizontal rule drawn at the very
// top of the Overall band (issue #233): one row, above the model selector, that
// visually divides the bottom stats region from the content above it. It is drawn in
// the theme divider colour by drawOverall.
const overallSeparatorLines = 1

// overallSelectorLines is the height of the model-selector dropdown rendered just
// below the top separator (issue #191): one row, above the title, so every metric
// below it can be scoped to the selected model.
const overallSelectorLines = 1

// overallBandHeight is the number of sidebar rows the Overall panel reserves at the
// bottom, top to bottom: one separator (issue #233), the model selector, one title
// and the metric rows (the trailing +1 is the title row).
const overallBandHeight = overallSeparatorLines + overallSelectorLines + overallMetricLines + 1

// overallAllModelsOption is the model-selector label for the aggregate view: every
// metric below it shows the cluster-wide grand total across all models, reproducing
// the pre-#191 behaviour. It maps to an empty model key.
const overallAllModelsOption = "all models"

// overallErrLineIdx is the metric-row index of the "errors" line, used to colour
// a non-zero error count red in the rendered band. Pinned by a test so a
// formatter reorder cannot silently move it.
const overallErrLineIdx = 5

// overallLabelWidth is the label column width; values align one space after it.
const overallLabelWidth = 10

// lifetimeStats accumulates the Overall panel's traffic figures over the entire
// gogent process lifetime (issue #232). The Statistics report sums only the
// currently-open sessions (it is rebuilt each refresh by iterating the live
// session map), so closing a session would otherwise erase the tokens, requests,
// errors and cache-hits it had already burned — the panel's totals would shrink.
// lifetimeStats remembers each session's most recent cumulative tally, keyed by
// session ID, and keeps it in the grand total after the session drops out of the
// live report, so the lifetime figures only ever grow over the run.
//
// It is owned by the Workbench and folded on the UI thread before each Overall
// refresh, so it needs no locking.
type lifetimeStats struct {
	// sessions is the last-known cumulative tally for every session ID ever seen,
	// open or closed. Per-session counters are monotonic for the life of a session,
	// so storing the latest snapshot (rather than diffing) is enough: the lifetime
	// total is just the sum of every session's last-known tally.
	//
	// It grows by one entry per session ID for the life of the process and is never
	// evicted. That is bounded by the number of sessions a user opens in one run
	// (tens to low hundreds, ~tens of bytes each), so no eviction policy is needed;
	// revisit only if sessions become cheap and unbounded (e.g. programmatic spawn).
	sessions map[string]sessionTally
}

// sessionTally is the most recent cumulative snapshot seen for one session: its
// primary/auxiliary connector counters plus the session-layer totals the Overall
// and Statistics views surface.
type sessionTally struct {
	primaryModel string
	turns        int
	tokensIn     int
	tokensOut    int
	toolCalls    int
	compactions  int
	primary      stats.ConnectorStat
	fast         stats.ConnectorStat
}

// newLifetimeStats returns an empty process-lifetime accumulator.
func newLifetimeStats() *lifetimeStats {
	return &lifetimeStats{sessions: make(map[string]sessionTally)}
}

// fold records the live report's open-session tallies into the lifetime store and
// returns a copy of the report whose grand totals and per-model connector stats are
// augmented with the contributions of sessions that have since closed.
//
// The live report already sums the currently-open sessions correctly, so fold takes
// it as the baseline and only adds back the last-known tally of every session that
// is remembered but no longer present (i.e. closed). The result: a still-open
// session keeps updating live; closing a session keeps its tokens / requests /
// errors / cache-hits in the totals instead of erasing them; a new session simply
// adds on top of the accumulated figure.
//
// Only the lifetime-sensitive aggregates are touched — Totals' connector/token
// counters and each model's Connector. The per-session rows, tool and skill
// breakdowns, and the live "what's on screen" node counts (Totals.Sessions, and each
// model's Sessions/SubAgents) pass through unchanged: those track the currently-open
// set and are meant to drop when a session closes.
//
// Because it only augments the report's own totals (never recomputes them from the
// per-session rows), fold is robust to reports whose totals are not a strict sum of
// their session rows, and is a no-op until the first session closes.
//
// Must be called on the UI thread: it reads and writes the unguarded sessions map.
// Its sole caller, Workbench.refreshOverall, runs on the UI thread (the stats ticker
// marshals via desktop.Post), so no locking is needed.
func (l *lifetimeStats) fold(report stats.Report) stats.Report {
	open := make(map[string]bool, len(report.Sessions))
	for _, s := range report.Sessions {
		open[s.ID] = true
		l.sessions[s.ID] = sessionTally{
			primaryModel: s.PrimaryModel,
			turns:        s.Turns,
			tokensIn:     s.TokensIn,
			tokensOut:    s.TokensOut,
			toolCalls:    s.ToolCalls,
			compactions:  s.Compactions,
			primary:      s.Primary,
			fast:         s.Fast,
		}
	}

	// Add back every closed session — remembered but absent from the live report — so
	// its counters persist in the grand totals and per-model breakdown.
	totals := report.Totals
	perModel := make(map[string]stats.ConnectorStat)
	for id, t := range l.sessions {
		if open[id] {
			continue
		}
		totals.Turns += t.turns
		totals.TokensIn += t.tokensIn
		totals.TokensOut += t.tokensOut
		totals.ToolCalls += t.toolCalls
		totals.Compactions += t.compactions
		totals.Primary = totals.Primary.Add(t.primary)
		totals.Fast = totals.Fast.Add(t.fast)
		if t.primaryModel != "" {
			perModel[t.primaryModel] = perModel[t.primaryModel].Add(t.primary)
		}
	}

	report.Totals = totals
	report.Models = mergeModelLifetime(report.Models, perModel)
	return report
}

// mergeModelLifetime returns the report's per-model rows with the closed-session
// connector contributions in extra added onto each matching model, plus a row for
// any model whose sessions have all closed (so per-model scoping keeps showing its
// accumulated traffic). The live node counts (Sessions/SubAgents) on the existing
// rows are left untouched; closed-only models carry zero node counts because they
// have no open sessions. It does not mutate the input slice and returns it unchanged
// when there is nothing to add back.
//
// Only the per-model Connector is accumulated to lifetime — that is the source the
// Overall panel scopes its metrics from (buildOverallStats reads ms.Connector.*).
// The session-layer token attribution (ModelStat.TokensIn/TokensOut) is deliberately
// left at its live, open-session-only value: a closed session's tokens are recovered
// from its Connector, and the panel never reads ModelStat.TokensIn. A future consumer
// wanting lifetime per-model session-layer tokens should read Connector, not these.
func mergeModelLifetime(models []stats.ModelStat, extra map[string]stats.ConnectorStat) []stats.ModelStat {
	if len(extra) == 0 {
		return models
	}
	out := make([]stats.ModelStat, 0, len(models)+len(extra))
	seen := make(map[string]bool, len(models))
	for _, m := range models {
		seen[m.Name] = true
		if c, ok := extra[m.Name]; ok {
			m.Connector = m.Connector.Add(c)
		}
		out = append(out, m)
	}
	// Append closed-only models in a stable name-sorted order so the folded report is
	// deterministic (ranging extra directly would vary run to run and could flake any
	// future snapshot test on Models).
	closedOnly := make([]string, 0, len(extra))
	for name := range extra {
		if !seen[name] {
			closedOnly = append(closedOnly, name)
		}
	}
	sort.Strings(closedOnly)
	for _, name := range closedOnly {
		out = append(out, stats.ModelStat{Name: name, Connector: extra[name]})
	}
	return out
}

// buildOverallStats assembles the panel's view from the Statistics report joined
// with the focused/selected model config (issue #107). It is pure and unit tested.
//
// selectedModel scopes the metrics (issue #191). When empty (the "all models"
// option) the panel shows the cluster-wide grand total: the report's primary-backend
// connector totals plus the sidebar's own session / sub-agent node counts (the
// authoritative "what is on screen" counts). When a model config name is given,
// every metric below the selector is scoped to that model from the report's
// per-model breakdown — tokens, requests, errors and cache-hit from its connector
// stats, and the session / sub-agent counts attributed to it.
func buildOverallStats(report stats.Report, sessions, subAgents int, model *config.ModelConfig, selectedModel string) overallStats {
	// The headline traffic figures come from the primary model backend's
	// connector snapshot so tokens / requests / errors / cache-hit are all drawn
	// from one consistent source. The auxiliary (fast/compression) backend is
	// reported separately in the Statistics view to avoid double counting and is
	// intentionally left out of this at-a-glance total.
	prim := report.Totals.Primary
	o := overallStats{
		Sessions:    sessions,
		SubAgents:   subAgents,
		TokensIn:    prim.TokensIn,
		TokensOut:   prim.TokensOut,
		Requests:    prim.Requests,
		Errors:      prim.Errors,
		CacheHitPct: prim.CacheHitPercent(),
	}
	if selectedModel != "" {
		// Scope every metric to the selected model. A model with no recorded usage
		// yet simply shows zeros (ok=false leaves the zero-valued struct).
		ms, _ := report.ModelByName(selectedModel)
		o.Sessions = ms.Sessions
		o.SubAgents = ms.SubAgents
		o.TokensIn = ms.Connector.TokensIn
		o.TokensOut = ms.Connector.TokensOut
		o.Requests = ms.Connector.Requests
		o.Errors = ms.Connector.Errors
		o.CacheHitPct = ms.Connector.CacheHitPercent()
	}
	if model != nil {
		// Prefer the human-friendly display name, falling back to the stable
		// config Name when no display name was set.
		o.Model = model.DisplayName
		if o.Model == "" {
			o.Model = model.Name
		}
		o.APIEndpoint = formatEndpoint(model.Endpoint, model.APIType)
	}
	return o
}

// formatEndpoint renders a short label for the Overall panel's "api" row (issue
// #107): the URL host[:port] when an explicit endpoint is set, otherwise the
// provider (api_type) that supplies its own base URL (e.g. "zai",
// "openrouter"), defaulting to the OpenAI-compatible convention. Stripping the
// scheme/path keeps the row inside the narrow sidebar. It is pure (no UI
// dependencies) so it can be unit tested directly.
func formatEndpoint(endpoint, apiType string) string {
	if endpoint == "" {
		if apiType != "" {
			return apiType
		}
		return "openai"
	}
	if u, err := url.Parse(endpoint); err == nil && u.Host != "" {
		return u.Host
	}
	return endpoint
}

// formatOverallStats renders the panel's metric rows. Each row is "label value"
// with the label padded so the values line up in a tidy left column. It is pure
// (no UI dependencies) so it can be unit tested directly.
func formatOverallStats(s overallStats) []string {
	kv := func(label, value string) string {
		return padName(label, overallLabelWidth) + " " + value
	}
	return []string{
		kv("sessions", strconv.Itoa(s.Sessions)),
		kv("sub-agents", strconv.Itoa(s.SubAgents)),
		kv("tokens in", formatTokens(s.TokensIn)),
		kv("tokens out", formatTokens(s.TokensOut)),
		kv("requests", strconv.Itoa(s.Requests)),
		kv("errors", strconv.Itoa(s.Errors)),
		kv("cache hit", strconv.Itoa(s.CacheHitPct)+"%"),
		kv("model", overallValue(s.Model)),
		kv("api", overallValue(s.APIEndpoint)),
	}
}

// overallValue returns v, or a compact "-" placeholder when empty, so a metric
// row keeps a value column even before an active model is known (no session open
// yet) without varying the panel's row count.
func overallValue(v string) string {
	if v == "" {
		return "-"
	}
	return v
}
