package ui

import (
	"strconv"

	"gogent/internal/stats"
)

// overallStats is the typed aggregate the right-hand "Overall" panel renders
// (issue #53): cluster-wide totals across every open session. It is built from
// the Statistics report's grand totals joined with the sidebar's own session /
// sub-agent node counts, so the panel renders one small typed struct rather than
// an untyped map (cross-ref the GetStats typing point in issue #6).
type overallStats struct {
	Sessions    int // open sessions (sidebar nodes)
	SubAgents   int // sub-agents tracked across all sessions (sidebar nodes)
	TokensIn    int // primary-model input tokens, summed across sessions
	TokensOut   int // primary-model output tokens, summed across sessions
	Requests    int // primary-model backend requests
	Errors      int // primary-model backend errors
	CacheHitPct int // prompt-cache hit share of input tokens, whole-number %
}

// overallMetricLines is the number of metric rows formatOverallStats emits
// beneath the "Overall" title. overallBandHeight derives from it, and a test pins
// the two together so a formatter change cannot silently desync the reserved
// band height.
const overallMetricLines = 7

// overallBandHeight is the number of sidebar rows the Overall panel reserves at
// the bottom: one separator, one title and the metric rows.
const overallBandHeight = overallMetricLines + 2

// overallErrLineIdx is the metric-row index of the "errors" line, used to colour
// a non-zero error count red in the rendered band. Pinned by a test so a
// formatter reorder cannot silently move it.
const overallErrLineIdx = 5

// overallLabelWidth is the label column width; values align one space after it.
const overallLabelWidth = 10

// buildOverallStats assembles the panel's view from the Statistics report's grand
// totals plus the sidebar's own session / sub-agent node counts (the
// authoritative "what is on screen" counts). It is pure and unit tested.
func buildOverallStats(report stats.Report, sessions, subAgents int) overallStats {
	// The headline traffic figures come from the primary model backend's
	// connector snapshot so tokens / requests / errors / cache-hit are all drawn
	// from one consistent source. The auxiliary (fast/compression) backend is
	// reported separately in the Statistics view to avoid double counting and is
	// intentionally left out of this at-a-glance total.
	prim := report.Totals.Primary
	return overallStats{
		Sessions:    sessions,
		SubAgents:   subAgents,
		TokensIn:    prim.TokensIn,
		TokensOut:   prim.TokensOut,
		Requests:    prim.Requests,
		Errors:      prim.Errors,
		CacheHitPct: prim.CacheHitPercent(),
	}
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
	}
}
