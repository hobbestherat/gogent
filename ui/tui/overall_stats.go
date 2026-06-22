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
	// subAgents and perModel mirror the SessionRow fields of the same name so a
	// closed session is re-attributed to the exact models it used (not just its final
	// model) and re-emitted faithfully. perModel is nil for rows recorded without a
	// split; the closed-session path then falls back to attributing primary to
	// primaryModel, matching the pre-split behaviour.
	subAgents int
	perModel  []stats.SessionModelStat
}

// newLifetimeStats returns an empty process-lifetime accumulator.
func newLifetimeStats() *lifetimeStats {
	return &lifetimeStats{sessions: make(map[string]sessionTally)}
}

// fold records the live report's open-session tallies into the lifetime store and
// returns a copy of the report whose grand totals, per-session rows and per-model
// connector stats are augmented with the contributions of sessions that have since
// closed.
//
// The live report already sums the currently-open sessions correctly, so fold takes
// it as the baseline and only adds back the last-known tally of every session that
// is remembered but no longer present (i.e. closed). The result: a still-open
// session keeps updating live; closing a session keeps its tokens / requests /
// errors / cache-hits in the totals — and keeps its per-session row in the Sessions
// list — instead of erasing them; a new session simply adds on top of the
// accumulated figure.
//
// Closed sessions are re-emitted as stats.SessionRow entries from the stored tally
// (id, turns, tokens, tool calls, compactions, primary model, primary/fast
// connector). They are appended after the live (open) rows in stable id-sorted
// order. ContextTokens/ContextWindow are live context-window metrics with no
// lifetime meaning, so closed rows carry zeros (the Sessions renderer handles
// contextPercent(0, 0)). This makes the Statistics dialog show cross-session
// history once it consumes the folded report (issue #277).
//
// The live "what's on screen" count Totals.Sessions (and each model's
// Sessions/SubAgents) passes through unchanged: it tracks the currently-open set
// and is meant to drop when a session closes, so it keeps matching the sidebar
// window count. The re-emitted closed rows are historical and intentionally not
// counted there (so Totals.Sessions can be < len(report.Sessions) after a close).
// Tool and skill breakdowns also pass through unchanged (global registries).
//
// Because it only augments the report's own totals (never recomputes them from the
// per-session rows), fold is robust to reports whose totals are not a strict sum of
// their session rows, and is a no-op until the first session closes. It does not
// mutate its input report: a fresh Sessions slice is built rather than appending
// into the caller's backing array, and Totals is a value copy.
//
// Must be called on the UI thread: it reads and writes the unguarded sessions map.
// Its callers, Workbench.refreshOverall and Workbench.showStatisticsDialog, run on
// the UI thread (the stats ticker marshals via desktop.Post), so no locking is
// needed.
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
			subAgents:    s.SubAgents,
			perModel:     s.PerModel,
		}
	}

	// Every remembered session absent from the live report has closed. Collect them
	// in a stable id order so both the re-emitted rows and the per-model add-backs
	// are deterministic (a map range order would vary run to run).
	closedIDs := make([]string, 0, len(l.sessions))
	for id := range l.sessions {
		if !open[id] {
			closedIDs = append(closedIDs, id)
		}
	}
	sort.Strings(closedIDs)

	// Add back each closed session so its counters persist in the grand totals and
	// per-model breakdown, and re-emit its per-session row for the Sessions list.
	totals := report.Totals
	perModel := make(map[string]stats.ConnectorStat)
	closedRows := make([]stats.SessionRow, 0, len(closedIDs))
	for _, id := range closedIDs {
		t := l.sessions[id]
		totals.Turns += t.turns
		totals.TokensIn += t.tokensIn
		totals.TokensOut += t.tokensOut
		totals.ToolCalls += t.toolCalls
		totals.Compactions += t.compactions
		totals.Primary = totals.Primary.Add(t.primary)
		totals.Fast = totals.Fast.Add(t.fast)
		// Re-attribute per-model connector to the EXACT models the session used when
		// its split was recorded (so a closed model-switching session keeps modelA's
		// share on A and modelB's on B, matching the live report). Fall back to the
		// final model when no split is available (rows recorded without PerModel).
		if len(t.perModel) > 0 {
			for _, m := range t.perModel {
				perModel[m.Name] = perModel[m.Name].Add(m.Connector)
			}
		} else if t.primaryModel != "" {
			perModel[t.primaryModel] = perModel[t.primaryModel].Add(t.primary)
		}
		closedRows = append(closedRows, stats.SessionRow{
			ID:           id,
			Turns:        t.turns,
			TokensIn:     t.tokensIn,
			TokensOut:    t.tokensOut,
			ToolCalls:    t.toolCalls,
			Compactions:  t.compactions,
			PrimaryModel: t.primaryModel,
			Primary:      t.primary,
			Fast:         t.fast,
			SubAgents:    t.subAgents,
			PerModel:     t.perModel,
			// ContextTokens/ContextWindow are live-only; closed rows carry zeros.
		})
	}

	// Build a fresh Sessions slice (open rows verbatim, then closed rows) so the
	// caller's backing array is never touched. Skip the copy entirely when nothing
	// has closed, leaving the input slice referenced unchanged.
	if len(closedRows) > 0 {
		sessions := make([]stats.SessionRow, len(report.Sessions), len(report.Sessions)+len(closedRows))
		copy(sessions, report.Sessions)
		report.Sessions = append(sessions, closedRows...)
	}

	report.Totals = totals
	report.Models = mergeModelLifetime(report.Models, perModel)
	return report
}

// phantomDefaultSessionID is the id of the always-present HTTP/API fallback session
// created at startup (cmd/main.go's CreateUserSession("default", …)). It is the
// shared headless session and is never opened as a TUI window, so it must not be
// counted among the sessions the TUI user actually sees (issue #278). It is not
// flagged ephemeral in the backend (it is created via CreateUserSession, not
// NewEphemeralSession), so it is matched by id rather than by SessionRow.Ephemeral.
const phantomDefaultSessionID = "default"

// isPhantomSession reports whether a report row is a backend session with no TUI
// window: the shared "default" session or any on-demand ephemeral HTTP/API session
// (SessionRow.Ephemeral, set by Gogent.Statistics). These are the sessions the TUI
// must hide from its Statistics surfaces (issue #278).
func isPhantomSession(s stats.SessionRow) bool {
	return s.ID == phantomDefaultSessionID || s.Ephemeral
}

// filterPhantomSessions returns a copy of the report with every windowless backend
// session removed — the shared "default" session and any ephemeral HTTP/API session
// (see isPhantomSession). Each removed row is dropped and its full contribution —
// including the Sessions count — is subtracted from the grand Totals. This makes the
// TUI's Statistics dialog and Overall panel count only the sessions the user sees as
// windows, fixing the off-by-one (off-by-N with live HTTP clients) against the
// "Sessions & Agents" sidebar (issue #278).
//
// It is applied TUI-side only, before fold, so a phantom is never remembered as a
// closed session (it would otherwise be re-emitted forever). The backend
// Statistics() report is left untouched, so the headless GET /stats endpoint still
// reports every session — there "default" is the real session the API talks to.
//
// It does not mutate the input report: Totals is a value copy and fresh Sessions /
// Models slices are built only when a phantom row is actually present.
func filterPhantomSessions(report stats.Report) stats.Report {
	kept := make([]stats.SessionRow, 0, len(report.Sessions))
	var removed []stats.SessionRow
	for _, s := range report.Sessions {
		if isPhantomSession(s) {
			report.Totals.Sessions--
			report.Totals.Turns -= s.Turns
			report.Totals.TokensIn -= s.TokensIn
			report.Totals.TokensOut -= s.TokensOut
			report.Totals.ToolCalls -= s.ToolCalls
			report.Totals.Compactions -= s.Compactions
			report.Totals.Primary = report.Totals.Primary.Sub(s.Primary)
			report.Totals.Fast = report.Totals.Fast.Sub(s.Fast)
			removed = append(removed, s)
			continue
		}
		kept = append(kept, s)
	}
	if len(removed) == 0 {
		return report
	}
	report.Sessions = kept
	// A phantom can carry real per-model traffic (an HTTP request routed to the
	// shared "default" or an ephemeral session while the TUI runs), so back it out of
	// report.Models too — otherwise the Models tab and the Overview "Models (tokens)"
	// summary keep counting it and the per-model rows no longer match the filtered
	// grand total (issue #278). subtractModelTraffic uses each row's exact per-model
	// split and also backs out its per-model session/sub-agent node counts.
	report.Models = subtractModelTraffic(report.Models, removed)
	return report
}

// subtractModelTraffic returns the per-model rows with each removed session's
// contribution backed out, so the breakdown stays consistent with the filtered grand
// totals and the Overall panel's model-scoped counts (issue #278). For each removed
// session it subtracts:
//
//   - the connector and token attribution, per model. When the row carries a
//     PerModel split (the real backend shape), each model's slice is backed out of
//     exactly that model — so a session that switched models does not leave traffic
//     stranded on one model or drive another negative. When PerModel is empty (rows
//     built without a split, e.g. in tests), it falls back to attributing the
//     aggregate Primary to PrimaryModel, matching the earlier single-model behaviour.
//   - the node counts (Sessions/SubAgents) from the session's PrimaryModel, mirroring
//     how Gogent.Statistics() keys those counts.
//
// The token and node-count back-outs are floored at zero so a row whose fields were
// not seeded (synthetic input) cannot go negative; on a real report they are exact.
//
// It does not mutate the input slice (it copies before adjusting) and does not prune
// rows that fall to zero, matching the rest of the stats pipeline.
func subtractModelTraffic(models []stats.ModelStat, removed []stats.SessionRow) []stats.ModelStat {
	type delta struct {
		conn      stats.ConnectorStat
		tokensIn  int
		tokensOut int
		sessions  int
		subAgents int
	}
	deltas := make(map[string]delta)
	addTraffic := func(model string, conn stats.ConnectorStat, tin, tout int) {
		d := deltas[model]
		d.conn = d.conn.Add(conn)
		d.tokensIn += tin
		d.tokensOut += tout
		deltas[model] = d
	}
	for _, s := range removed {
		if len(s.PerModel) > 0 {
			// Exact: back each model's slice out of that same model.
			for _, m := range s.PerModel {
				addTraffic(m.Name, m.Connector, m.TokensIn, m.TokensOut)
			}
		} else if s.PrimaryModel != "" {
			// Fallback: no split available, attribute the aggregate to the final model.
			addTraffic(s.PrimaryModel, s.Primary, s.TokensIn, s.TokensOut)
		}
		// Node counts are keyed by the session's primary model only (matching
		// Gogent.Statistics()'s mt.Sessions++ / mt.SubAgents += per primaryModel).
		if s.PrimaryModel != "" {
			d := deltas[s.PrimaryModel]
			d.sessions++
			d.subAgents += s.SubAgents
			deltas[s.PrimaryModel] = d
		}
	}
	if len(deltas) == 0 {
		return models
	}
	out := make([]stats.ModelStat, len(models))
	copy(out, models)
	for i := range out {
		if d, ok := deltas[out[i].Name]; ok {
			out[i].Connector = out[i].Connector.Sub(d.conn)
			out[i].TokensIn = clampZero(out[i].TokensIn - d.tokensIn)
			out[i].TokensOut = clampZero(out[i].TokensOut - d.tokensOut)
			out[i].Sessions = clampZero(out[i].Sessions - d.sessions)
			out[i].SubAgents = clampZero(out[i].SubAgents - d.subAgents)
		}
	}
	return out
}

// clampZero returns n, or 0 when n is negative. It floors the token and node-count
// back-outs so a model row whose fields were not seeded (synthetic input) cannot
// render a negative value; on a real report every back-out is exact and the floor is
// a no-op.
func clampZero(n int) int {
	if n < 0 {
		return 0
	}
	return n
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
