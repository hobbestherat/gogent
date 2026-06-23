// Package stats defines the aggregated statistics report gogent surfaces in its
// Statistics view (issue #57). It is a pure data layer: internal/gogent builds a
// Report by joining the per-session, per-tool and per-skill counters it already
// collects (but only partially shows), and the UI renders the report and exports
// it to CSV/JSON.
//
// The package depends only on internal/model (to convert its connector snapshot
// into a neutral type) so it can be reused by both the backend and the UI without
// pulling in agent/gogent.
package stats

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"gogent/internal/model"
)

// Report is a point-in-time snapshot of everything the Statistics view shows.
type Report struct {
	// GeneratedAt is when the report was assembled (unix seconds).
	GeneratedAt int64 `json:"generated_at"`
	// Totals is the grand total across every active session.
	Totals Totals `json:"totals"`
	// Sessions is one row per active session, in creation order.
	Sessions []SessionRow `json:"sessions"`
	// Tools is the per-tool usage/duration breakdown across the process.
	Tools []ToolStat `json:"tools"`
	// Skills is the per-skill success/failure breakdown across the process.
	Skills []SkillStat `json:"skills"`
	// Models is the per-model token attribution aggregated across sessions.
	Models []ModelStat `json:"models"`
}

// Totals is the grand total across all sessions.
type Totals struct {
	Sessions    int `json:"sessions"`
	Turns       int `json:"turns"`
	TokensIn    int `json:"tokens_in"`
	TokensOut   int `json:"tokens_out"`
	ToolCalls   int `json:"tool_calls"`
	Compactions int `json:"compactions"`
	// Primary is the aggregated connector stats for the primary model backends
	// (request counts, token totals, latency, error breakdown).
	Primary ConnectorStat `json:"primary"`
	// Fast is the aggregated connector stats for the auxiliary/fast model backend
	// (e.g. context compression), reported separately so it is not double counted
	// against the primary model.
	Fast ConnectorStat `json:"fast"`
}

// SessionRow is the per-session slice of the totals.
type SessionRow struct {
	ID            string `json:"id"`
	Turns         int    `json:"turns"`
	TokensIn      int    `json:"tokens_in"`
	TokensOut     int    `json:"tokens_out"`
	ToolCalls     int    `json:"tool_calls"`
	ContextTokens int    `json:"context_tokens"`
	ContextWindow int    `json:"context_window"`
	Compactions   int    `json:"compactions"`
	// PrimaryModel is the config name of the model the session currently routes its
	// primary turns through. It lets the Overall panel attribute a session (and its
	// sub-agents) to a model when scoping metrics per model (issue #191). Empty when
	// no model has been selected yet.
	PrimaryModel string        `json:"primary_model,omitempty"`
	Primary      ConnectorStat `json:"primary"`
	Fast         ConnectorStat `json:"fast"`
	// Ephemeral marks a session the backend never persists — the shared HTTP/API
	// sessions created on demand by NewEphemeralSession (issue #25). It is purely
	// informational in the report (the headless GET /stats endpoint still lists
	// every session); the TUI uses it to drop sessions that have no window from its
	// Statistics surfaces so the count matches the sidebar (issue #278). Omitted from
	// JSON when false so the common, persisted-session shape is unchanged.
	Ephemeral bool `json:"ephemeral,omitempty"`
	// SubAgents is the session's sub-agent count, attributed to PrimaryModel in the
	// grand Models breakdown (issue #191). It is carried per row so a consumer that
	// excludes this session can back the node count out of that model (issue #278).
	SubAgents int `json:"sub_agents,omitempty"`
	// PerModel is this session's usage split across every model it routed turns
	// through. The grand Models breakdown is the sum of all sessions' PerModel, so a
	// consumer excluding a session (the TUI dropping a windowless phantom, issue
	// #278) can back out its exact per-model contribution rather than mis-attributing
	// the whole aggregate Primary to the final model. Empty for rows assembled
	// without a split; consumers then fall back to attributing Primary to
	// PrimaryModel. Omitted from JSON when empty.
	PerModel []SessionModelStat `json:"per_model,omitempty"`
}

// SessionModelStat is one model's slice of a single session's usage: the connector
// counters and session-layer token attribution for the turns the session routed
// through that model. A session that switched models carries several of these (see
// SessionRow.PerModel). It mirrors the per-model fields of ModelStat without the
// node counts, which are session-level (attributed to the session's primary model).
type SessionModelStat struct {
	Name      string        `json:"name"`
	TokensIn  int           `json:"tokens_in"`
	TokensOut int           `json:"tokens_out"`
	Connector ConnectorStat `json:"connector"`
}

// ConnectorStat mirrors the low-level model connector counters (see
// model.StatsSnapshot) in this neutral package, so the UI and export code need
// not depend on the model package's internals.
type ConnectorStat struct {
	Requests         int   `json:"requests"`
	Success          int   `json:"success"`
	Errors           int   `json:"errors"`
	TokensIn         int   `json:"tokens_in"`
	CachedTokensIn   int   `json:"cached_tokens_in"`
	TokensOut        int   `json:"tokens_out"`
	TotalTimeMs      int64 `json:"total_time_ms"`
	Timeouts         int   `json:"timeouts"`
	ContextOverflows int   `json:"context_overflows"`
	Refusals         int   `json:"refusals"`
	GenericErrors    int   `json:"generic_errors"`
}

// FromSnapshot converts a model connector snapshot into the neutral ConnectorStat.
func FromSnapshot(s model.StatsSnapshot) ConnectorStat {
	return ConnectorStat{
		Requests:         s.RequestCount,
		Success:          s.SuccessCount,
		Errors:           s.ErrorCount,
		TokensIn:         s.TotalTokensIn,
		CachedTokensIn:   s.TotalCachedTokensIn,
		TokensOut:        s.TotalTokensOut,
		TotalTimeMs:      s.TotalTimeMs,
		Timeouts:         s.TimeoutCount,
		ContextOverflows: s.ContextWindowOverflowCount,
		Refusals:         s.RefusalCount,
		GenericErrors:    s.GenericErrorCount,
	}
}

// Add returns the element-wise sum of two connector stats, used to aggregate
// across sessions into a total.
func (c ConnectorStat) Add(other ConnectorStat) ConnectorStat {
	return ConnectorStat{
		Requests:         c.Requests + other.Requests,
		Success:          c.Success + other.Success,
		Errors:           c.Errors + other.Errors,
		TokensIn:         c.TokensIn + other.TokensIn,
		CachedTokensIn:   c.CachedTokensIn + other.CachedTokensIn,
		TokensOut:        c.TokensOut + other.TokensOut,
		TotalTimeMs:      c.TotalTimeMs + other.TotalTimeMs,
		Timeouts:         c.Timeouts + other.Timeouts,
		ContextOverflows: c.ContextOverflows + other.ContextOverflows,
		Refusals:         c.Refusals + other.Refusals,
		GenericErrors:    c.GenericErrors + other.GenericErrors,
	}
}

// Sub returns the element-wise difference of two connector stats (c minus
// other). It is the inverse of Add, used to back out a session's contribution
// from a grand total — e.g. excluding the phantom backend "default" session from
// the TUI's Statistics totals (issue #278).
func (c ConnectorStat) Sub(other ConnectorStat) ConnectorStat {
	return ConnectorStat{
		Requests:         c.Requests - other.Requests,
		Success:          c.Success - other.Success,
		Errors:           c.Errors - other.Errors,
		TokensIn:         c.TokensIn - other.TokensIn,
		CachedTokensIn:   c.CachedTokensIn - other.CachedTokensIn,
		TokensOut:        c.TokensOut - other.TokensOut,
		TotalTimeMs:      c.TotalTimeMs - other.TotalTimeMs,
		Timeouts:         c.Timeouts - other.Timeouts,
		ContextOverflows: c.ContextOverflows - other.ContextOverflows,
		Refusals:         c.Refusals - other.Refusals,
		GenericErrors:    c.GenericErrors - other.GenericErrors,
	}
}

// AvgLatencyMs is the mean per-request latency in milliseconds, or 0 when there
// were no requests.
func (c ConnectorStat) AvgLatencyMs() int64 {
	if c.Requests == 0 {
		return 0
	}
	return c.TotalTimeMs / int64(c.Requests)
}

// CacheHitPercent is the share of prompt (input) tokens served from the
// provider's prompt cache, as a whole-number percentage of TokensIn (0 when no
// input tokens have been processed). It is the headline prompt-caching metric:
// the higher it is, the more of the stable prefix was reused at the discounted
// cache-read price.
func (c ConnectorStat) CacheHitPercent() int {
	if c.TokensIn <= 0 {
		return 0
	}
	return int(float64(c.CachedTokensIn) / float64(c.TokensIn) * 100)
}

// ToolStat is the per-tool usage and duration breakdown.
type ToolStat struct {
	Name        string `json:"name"`
	Invocations int    `json:"invocations"`
	Success     int    `json:"success"`
	Failure     int    `json:"failure"`
	TotalMs     int64  `json:"total_ms"`
	// ResultBytes is the cumulative serialized size of this tool's results, a
	// diagnostic for which tools dump the most context (issue #361).
	ResultBytes int64 `json:"result_bytes"`
}

// AvgMs is the mean execution time per invocation in milliseconds, or 0 when the
// tool has never been invoked.
func (t ToolStat) AvgMs() int64 {
	if t.Invocations == 0 {
		return 0
	}
	return t.TotalMs / int64(t.Invocations)
}

// SkillStat is the per-skill success/failure breakdown.
type SkillStat struct {
	Name       string `json:"name"`
	Success    int    `json:"success"`
	Failure    int    `json:"failure"`
	TotalCalls int    `json:"total_calls"`
}

// ModelStat is the per-model usage aggregated across sessions (issue #191). It
// carries the session-layer token attribution (TokensIn/TokensOut) joined with the
// per-model connector metrics (Connector) and the count of sessions / sub-agents
// currently using the model, so the Overall panel can scope every metric below its
// selector to one model. Sessions and SubAgents are keyed by each session's current
// primary model; tokens and Connector are attributed to the model that actually
// incurred them (a session that switched models contributes to several entries).
type ModelStat struct {
	Name      string        `json:"name"`
	TokensIn  int           `json:"tokens_in"`
	TokensOut int           `json:"tokens_out"`
	Sessions  int           `json:"sessions,omitempty"`
	SubAgents int           `json:"sub_agents,omitempty"`
	Connector ConnectorStat `json:"connector"`
}

// ModelByName returns the aggregated breakdown for a model (by config name) and
// whether it was found, so the Overall panel can scope its metrics to the selected
// model. An empty name (the "all models" option) never matches a real entry; the
// caller uses the report's grand totals for the aggregate view instead.
func (r Report) ModelByName(name string) (ModelStat, bool) {
	for _, m := range r.Models {
		if m.Name == name {
			return m, true
		}
	}
	return ModelStat{}, false
}

// JSON returns the report as pretty-printed JSON. It is the structured export
// format for the Statistics view.
func (r Report) JSON() (string, error) {
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return "", fmt.Errorf("marshal report: %w", err)
	}
	return string(b), nil
}

// CSV returns the report as long-format CSV: one row per metric, with the columns
// section, name, metric, value. Long format keeps a single uniform schema across
// the differently-shaped sections (totals, sessions, tools, skills, models) so
// the output loads cleanly into a spreadsheet or pandas DataFrame.
func (r Report) CSV() (string, error) {
	var buf bytes.Buffer
	w := csv.NewWriter(&buf)
	if err := w.Write([]string{"section", "name", "metric", "value"}); err != nil {
		return "", fmt.Errorf("write csv header: %w", err)
	}

	row := func(section, name, metric string, value int64) {
		_ = w.Write([]string{section, name, metric, strconv.FormatInt(value, 10)})
	}

	t := r.Totals
	row("total", "", "sessions", int64(t.Sessions))
	row("total", "", "turns", int64(t.Turns))
	row("total", "", "tokens_in", int64(t.TokensIn))
	row("total", "", "tokens_out", int64(t.TokensOut))
	row("total", "", "tool_calls", int64(t.ToolCalls))
	row("total", "", "compactions", int64(t.Compactions))
	writeConnectorCSV(w, "total", "primary", t.Primary)
	writeConnectorCSV(w, "total", "fast", t.Fast)

	for _, s := range r.Sessions {
		row("session", s.ID, "turns", int64(s.Turns))
		row("session", s.ID, "tokens_in", int64(s.TokensIn))
		row("session", s.ID, "tokens_out", int64(s.TokensOut))
		row("session", s.ID, "tool_calls", int64(s.ToolCalls))
		row("session", s.ID, "context_tokens", int64(s.ContextTokens))
		row("session", s.ID, "context_window", int64(s.ContextWindow))
		row("session", s.ID, "compactions", int64(s.Compactions))
		writeConnectorCSV(w, "session:"+s.ID, "primary", s.Primary)
		writeConnectorCSV(w, "session:"+s.ID, "fast", s.Fast)
	}
	for _, tl := range r.Tools {
		row("tool", tl.Name, "invocations", int64(tl.Invocations))
		row("tool", tl.Name, "success", int64(tl.Success))
		row("tool", tl.Name, "failure", int64(tl.Failure))
		row("tool", tl.Name, "total_ms", tl.TotalMs)
		row("tool", tl.Name, "avg_ms", tl.AvgMs())
		row("tool", tl.Name, "result_bytes", tl.ResultBytes)
	}
	for _, sk := range r.Skills {
		row("skill", sk.Name, "success", int64(sk.Success))
		row("skill", sk.Name, "failure", int64(sk.Failure))
		row("skill", sk.Name, "total_calls", int64(sk.TotalCalls))
	}
	for _, m := range r.Models {
		row("model", m.Name, "tokens_in", int64(m.TokensIn))
		row("model", m.Name, "tokens_out", int64(m.TokensOut))
		row("model", m.Name, "sessions", int64(m.Sessions))
		row("model", m.Name, "sub_agents", int64(m.SubAgents))
		writeConnectorCSV(w, "model:"+m.Name, "connector", m.Connector)
	}

	w.Flush()
	if err := w.Error(); err != nil {
		return "", fmt.Errorf("flush csv: %w", err)
	}
	return strings.TrimRight(buf.String(), "\n"), nil
}

// writeConnectorCSV emits the connector counters for one section/name pair.
func writeConnectorCSV(w *csv.Writer, section, name string, c ConnectorStat) {
	s := section
	if name != "" {
		s = section + ":" + name
	}
	rec := []string{s, "", "", ""}
	set := func(metric string, value int64) {
		rec[2] = metric
		rec[3] = strconv.FormatInt(value, 10)
		_ = w.Write(rec)
	}
	set("requests", int64(c.Requests))
	set("success", int64(c.Success))
	set("errors", int64(c.Errors))
	set("tokens_in", int64(c.TokensIn))
	set("cached_tokens_in", int64(c.CachedTokensIn))
	set("cache_hit_percent", int64(c.CacheHitPercent()))
	set("tokens_out", int64(c.TokensOut))
	set("total_time_ms", c.TotalTimeMs)
	set("avg_latency_ms", c.AvgLatencyMs())
	set("timeouts", int64(c.Timeouts))
	set("context_overflows", int64(c.ContextOverflows))
	set("refusals", int64(c.Refusals))
	set("generic_errors", int64(c.GenericErrors))
}
