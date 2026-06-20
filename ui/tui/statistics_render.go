package ui

import (
	"fmt"
	"strings"

	"gogent/internal/stats"
)

// statisticsSection enumerates the Statistics view's tabs. The order matches the
// section Select options so the selected index maps directly to a section.
type statisticsSection int

const (
	statsOverview statisticsSection = iota
	statsSessions
	statsTools
	statsSkills
	statsModels
)

// statisticsSectionNames are the tab labels, in statisticsSection order.
var statisticsSectionNames = []string{"Overview", "Sessions", "Tools", "Skills", "Models"}

// renderStatistics returns the text shown in the Statistics view for one section
// of the report. It is pure (no UI dependencies) so it can be unit tested.
func renderStatistics(section statisticsSection, r stats.Report) string {
	switch section {
	case statsOverview:
		return renderStatsOverview(r)
	case statsSessions:
		return renderStatsSessions(r)
	case statsTools:
		return renderStatsTools(r)
	case statsSkills:
		return renderStatsSkills(r)
	case statsModels:
		return renderStatsModels(r)
	}
	return ""
}

// renderStatsOverview renders the grand-total counters plus the primary and fast
// model backend connector stats.
func renderStatsOverview(r stats.Report) string {
	var b strings.Builder
	b.WriteString("Overview\n========\n\n")
	t := r.Totals
	writeKV(&b, "Sessions", fmt.Sprintf("%d", t.Sessions), "Turns", fmt.Sprintf("%d", t.Turns))
	writeKV(&b, "Tokens in", formatTokens(t.TokensIn), "Tokens out", formatTokens(t.TokensOut))
	writeKV(&b, "Tool calls", fmt.Sprintf("%d", t.ToolCalls), "Compactions", fmt.Sprintf("%d", t.Compactions))

	b.WriteString("\nPrimary model backend\n")
	writeConnector(&b, t.Primary)
	if t.Fast.Requests != 0 || t.Fast.TokensIn != 0 || t.Fast.TokensOut != 0 {
		b.WriteString("\nFast model backend (compression / auxiliary)\n")
		writeConnector(&b, t.Fast)
	}

	if len(r.Models) > 0 {
		b.WriteString("\nModels (tokens)\n")
		writeConnectorModelSummary(&b, r.Models)
	}
	return b.String()
}

// renderStatsSessions renders one aligned row per session.
func renderStatsSessions(r stats.Report) string {
	var b strings.Builder
	b.WriteString("Sessions\n========\n\n")
	if len(r.Sessions) == 0 {
		b.WriteString("No sessions.\n")
		return b.String()
	}
	const (
		cID, cTurns, cTok, cTools, cCtx, cReq, cErr, cComp = 20, 6, 13, 6, 5, 5, 5, 6
	)
	fmt.Fprintf(&b, "%s %s %s %s %s %s %s %s\n",
		padName("ID", cID), padName("Turns", cTurns), padName("Tok in/out", cTok),
		padName("Tools", cTools), padName("Ctx%", cCtx), padName("Reqs", cReq),
		padName("Errs", cErr), padName("Comp", cComp))
	for _, s := range r.Sessions {
		fmt.Fprintf(&b, "%s %s %s %s %s %s %s %s\n",
			padName(s.ID, cID),
			padName(fmt.Sprintf("%d", s.Turns), cTurns),
			padName(formatTokens(s.TokensIn)+"/"+formatTokens(s.TokensOut), cTok),
			padName(fmt.Sprintf("%d", s.ToolCalls), cTools),
			padName(fmt.Sprintf("%d%%", contextPercent(s.ContextTokens, s.ContextWindow)), cCtx),
			padName(fmt.Sprintf("%d", s.Primary.Requests), cReq),
			padName(fmt.Sprintf("%d", s.Primary.Errors), cErr),
			padName(fmt.Sprintf("%d", s.Compactions), cComp),
		)
	}
	b.WriteString("\nReqs/Errs are the session's primary-model backend requests/errors.\n")
	return b.String()
}

// renderStatsTools renders one aligned row per registered tool.
func renderStatsTools(r stats.Report) string {
	var b strings.Builder
	b.WriteString("Tools\n=====\n\n")
	if len(r.Tools) == 0 {
		b.WriteString("No tools registered.\n")
		return b.String()
	}
	const (
		cName, cCalls, cOK, cFail, cAvg = 22, 7, 7, 7, 9
	)
	fmt.Fprintf(&b, "%s %s %s %s %s\n",
		padName("Name", cName), padName("Calls", cCalls), padName("OK", cOK),
		padName("Fail", cFail), padName("Avg ms", cAvg))
	for _, t := range r.Tools {
		fmt.Fprintf(&b, "%s %s %s %s %s\n",
			padName(t.Name, cName),
			padName(fmt.Sprintf("%d", t.Invocations), cCalls),
			padName(fmt.Sprintf("%d", t.Success), cOK),
			padName(fmt.Sprintf("%d", t.Failure), cFail),
			padName(formatMs(t.AvgMs()), cAvg),
		)
	}
	return b.String()
}

// renderStatsSkills renders one aligned row per skill.
func renderStatsSkills(r stats.Report) string {
	var b strings.Builder
	b.WriteString("Skills\n======\n\n")
	if len(r.Skills) == 0 {
		b.WriteString("No skills loaded.\n")
		return b.String()
	}
	const (
		cName, cOK, cFail, cTotal = 22, 7, 7, 7
	)
	fmt.Fprintf(&b, "%s %s %s %s\n",
		padName("Name", cName), padName("OK", cOK), padName("Fail", cFail), padName("Total", cTotal))
	for _, s := range r.Skills {
		fmt.Fprintf(&b, "%s %s %s %s\n",
			padName(s.Name, cName),
			padName(fmt.Sprintf("%d", s.Success), cOK),
			padName(fmt.Sprintf("%d", s.Failure), cFail),
			padName(fmt.Sprintf("%d", s.TotalCalls), cTotal),
		)
	}
	return b.String()
}

// renderStatsModels renders per-model token attribution.
func renderStatsModels(r stats.Report) string {
	var b strings.Builder
	b.WriteString("Models\n======\n\n")
	if len(r.Models) == 0 {
		b.WriteString("No per-model usage yet. Tokens are attributed to a model once a\nsession sends a turn with it selected.\n")
		return b.String()
	}
	const (
		cName, cIn, cOut = 24, 14, 14
	)
	fmt.Fprintf(&b, "%s %s %s\n", padName("Model", cName), padName("Tokens in", cIn), padName("Tokens out", cOut))
	for _, m := range r.Models {
		fmt.Fprintf(&b, "%s %s %s\n",
			padName(m.Name, cName),
			padName(formatTokens(m.TokensIn), cIn),
			padName(formatTokens(m.TokensOut), cOut),
		)
	}
	b.WriteString("\nCache-hit % is shown per backend in the Overview. Cost and TTFT are not yet tracked (follow-ups).\n")
	return b.String()
}

// writeKV writes two aligned key/value pairs on one line, each as "label: value".
func writeKV(b *strings.Builder, k1, v1, k2, v2 string) {
	const (
		labelW = 14
		valueW = 12
	)
	fmt.Fprintf(b, "  %s %s   %s %s\n",
		padName(k1+":", labelW), padName(v1, valueW),
		padName(k2+":", labelW), padName(v2, valueW))
}

// writeConnector writes a connector stat block (requests/success/errors, tokens,
// average latency and the error breakdown).
func writeConnector(b *strings.Builder, c stats.ConnectorStat) {
	fmt.Fprintf(b, "  %s %s %s\n",
		padName("Requests:", 14), padName(fmt.Sprintf("%d", c.Requests), 10),
		padName("Success: "+fmt.Sprintf("%d", c.Success), 16))
	fmt.Fprintf(b, "  %s %s %s\n",
		padName("Tokens in:", 14), padName(formatTokens(c.TokensIn), 10),
		padName("Errors: "+fmt.Sprintf("%d", c.Errors), 16))
	fmt.Fprintf(b, "  %s %s %s\n",
		padName("Tokens out:", 14), padName(formatTokens(c.TokensOut), 10),
		padName("Avg latency: "+formatMs(c.AvgLatencyMs()), 20))
	fmt.Fprintf(b, "  %s %s %s\n",
		padName("Cached in:", 14), padName(formatTokens(c.CachedTokensIn), 10),
		padName(fmt.Sprintf("Cache hit: %d%%", c.CacheHitPercent()), 20))
	fmt.Fprintf(b, "  Errors: timeouts=%d overflows=%d refusals=%d generic=%d\n",
		c.Timeouts, c.ContextOverflows, c.Refusals, c.GenericErrors)
}

// writeConnectorModelSummary writes the per-model token rows under the overview.
func writeConnectorModelSummary(b *strings.Builder, models []stats.ModelStat) {
	for _, m := range models {
		fmt.Fprintf(b, "  %s %s\n", padName(m.Name+":", 18),
			padName(formatTokens(m.TokensIn)+"/"+formatTokens(m.TokensOut)+" in/out", 22))
	}
}

// formatMs renders a millisecond duration compactly (e.g. "820 ms" or "12.3 s").
func formatMs(ms int64) string {
	if ms <= 0 {
		return "-"
	}
	if ms >= 10_000 {
		return fmt.Sprintf("%.1f s", float64(ms)/1000.0)
	}
	return fmt.Sprintf("%d ms", ms)
}
