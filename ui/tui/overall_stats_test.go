package ui

import (
	"strings"
	"testing"

	tv "github.com/hobbestherat/turbotui/turbotv"
	"gogent/internal/agent"
	"gogent/internal/config"
	"gogent/internal/stats"
)

// TestBuildOverallStats covers the mapping from a Statistics report + the
// sidebar's own node counts into the panel's typed view, including the
// whole-number cache-hit percentage derived from the connector snapshot.
func TestBuildOverallStats(t *testing.T) {
	report := stats.Report{Totals: stats.Totals{
		Primary: stats.ConnectorStat{
			Requests: 42, Errors: 3, TokensIn: 1000, TokensOut: 500, CachedTokensIn: 250,
		},
		// The auxiliary (fast/compression) backend must NOT bleed into the headline
		// totals, to avoid double counting.
		Fast: stats.ConnectorStat{Requests: 9, TokensIn: 9999, TokensOut: 9999},
	}}
	// Issue #534: the aggregate view (selectedModel == "") leaves the model/api
	// rows empty even though a focused-session model config is threaded through.
	got := buildOverallStats(report, 3, 5,
		&config.ModelConfig{Name: "groq-free", DisplayName: "Groq", Endpoint: "https://api.groq.com/openai/v1/chat/completions"}, "")
	want := overallStats{Sessions: 3, SubAgents: 5, TokensIn: 1000, TokensOut: 500,
		Requests: 42, Errors: 3, CacheHitPct: 25, Model: "", APIEndpoint: ""}
	if got != want {
		t.Fatalf("buildOverallStats = %+v, want %+v", got, want)
	}
}

// TestBuildOverallStatsEmpty verifies a zero report with no active model yields a
// safe zero view (the panel's first frame before any traffic / session, and the
// "no statistics handler" path).
func TestBuildOverallStatsEmpty(t *testing.T) {
	got := buildOverallStats(stats.Report{}, 0, 0, nil, "")
	if got != (overallStats{}) {
		t.Fatalf("empty report should yield zero view, got %+v", got)
	}
}

// TestBuildOverallStatsModel covers the model / endpoint derivation (issue #107):
// the display name falls back to the config Name, and the endpoint is reduced to
// its host (or provider label) by formatEndpoint. Issue #534 gates the populate
// block on a non-empty selectedModel, so the model's own Name is passed as the
// selection to keep the derivation path under test (otherwise the aggregate would
// blank the rows and stop exercising it).
func TestBuildOverallStatsModel(t *testing.T) {
	for _, tc := range []struct {
		name  string
		model *config.ModelConfig
		// zero report so only the model-derived fields are under test
		wantModel string
		wantAPI   string
	}{
		{
			name:      "display name preferred",
			model:     &config.ModelConfig{Name: "local-lan", DisplayName: "Local LAN", Endpoint: "http://127.0.0.1:8080/v1/chat/completions"},
			wantModel: "Local LAN",
			wantAPI:   "127.0.0.1:8080",
		},
		{
			name:      "falls back to config name",
			model:     &config.ModelConfig{Name: "zai-glm", APIType: "zai"},
			wantModel: "zai-glm",
			wantAPI:   "zai",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := buildOverallStats(stats.Report{}, 0, 0, tc.model, tc.model.Name)
			if got.Model != tc.wantModel {
				t.Errorf("Model = %q, want %q", got.Model, tc.wantModel)
			}
			if got.APIEndpoint != tc.wantAPI {
				t.Errorf("APIEndpoint = %q, want %q", got.APIEndpoint, tc.wantAPI)
			}
		})
	}
}

// TestFormatOverallStats pins the rendered metric rows: label column alignment,
// compact token formatting, the cache-hit percentage and the model / endpoint
// rows (issue #107). Exact lines document the alignment contract the band relies
// on.
func TestFormatOverallStats(t *testing.T) {
	got := formatOverallStats(overallStats{
		Sessions: 3, SubAgents: 5, TokensIn: 1234, TokensOut: 567,
		Requests: 42, Errors: 0, CacheHitPct: 25,
		Model: "Groq", APIEndpoint: "api.groq.com",
	})
	want := []string{
		"sessions   3",
		"sub-agents 5",
		"tokens in  1.2k",
		"tokens out 567",
		"requests   42",
		"errors     0",
		"cache hit  25%",
		"model      Groq",
		"api        api.groq.com",
	}
	if len(got) != len(want) {
		t.Fatalf("formatOverallStats produced %d lines, want %d (%q)", len(got), len(want), got)
	}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("line %d = %q, want %q", i, got[i], w)
		}
	}
}

// TestFormatOverallStatsPlaceholders verifies the model / api rows render BLANK
// (empty value column) in the aggregate / no-active-model view (issue #534), so
// the row count is stable while nothing misleading is shown. The rows keep their
// labels so the value column stays aligned.
func TestFormatOverallStatsPlaceholders(t *testing.T) {
	got := formatOverallStats(overallStats{})
	// Value column is empty, but the labels survive (rows stay present + aligned).
	if v := got[len(got)-2][overallLabelWidth+1:]; v != "" {
		t.Errorf("model row value = %q, want blank (aggregate view, issue #534)", v)
	}
	if !strings.HasPrefix(got[len(got)-2], "model") {
		t.Errorf("model row = %q, lost its label", got[len(got)-2])
	}
	if v := got[len(got)-1][overallLabelWidth+1:]; v != "" {
		t.Errorf("api row value = %q, want blank (aggregate view, issue #534)", v)
	}
	if !strings.HasPrefix(got[len(got)-1], "api") {
		t.Errorf("api row = %q, lost its label", got[len(got)-1])
	}
}

// TestFormatOverallStatsTokenSuffixes covers the compact token formatting at the
// thousand / million boundaries, exercised through the formatter.
func TestFormatOverallStatsTokenSuffixes(t *testing.T) {
	for _, tc := range []struct {
		name       string
		tokensIn   int
		wantInLine string
	}{
		{"under a thousand", 999, "tokens in  999"},
		{"exact thousand", 1000, "tokens in  1.0k"},
		{"millions", 1234567, "tokens in  1.2M"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lines := formatOverallStats(overallStats{TokensIn: tc.tokensIn})
			found := false
			for _, l := range lines {
				if l == tc.wantInLine {
					found = true
					break
				}
			}
			if !found {
				t.Errorf("expected line %q among %q", tc.wantInLine, lines)
			}
		})
	}
}

// TestOverallBandHeightInvariant pins the relationship between the formatter's
// line count, the reserved band height and the error-line index, so a formatter
// change cannot silently desync the band layout or the red-error highlighting.
func TestOverallBandHeightInvariant(t *testing.T) {
	lines := formatOverallStats(overallStats{})
	if len(lines) != overallMetricLines {
		t.Fatalf("formatOverallStats line count = %d, want overallMetricLines %d",
			len(lines), overallMetricLines)
	}
	// The band reserves the metric rows plus a separator, a title and the model
	// selector row at the top (issue #191).
	if got := overallBandHeight - overallSelectorLines - 2; got != overallMetricLines {
		t.Fatalf("overallBandHeight-overallSelectorLines-2 = %d, want overallMetricLines %d", got, overallMetricLines)
	}
	if overallErrLineIdx >= len(lines) {
		t.Fatalf("overallErrLineIdx %d out of range (%d lines)", overallErrLineIdx, len(lines))
	}
	if !strings.HasPrefix(lines[overallErrLineIdx], "errors") {
		t.Fatalf("line %d = %q, want the errors row", overallErrLineIdx, lines[overallErrLineIdx])
	}
}

// TestSidebarRefreshOverallStats verifies the sidebar joins a report with its own
// session / sub-agent node counts, so addSession / applySubAgent move the panel's
// counts even when the report itself is unchanged.
func TestSidebarRefreshOverallStats(t *testing.T) {
	s := newTestSidebar()
	s.addSession("s1", "Session 1", false)
	s.addSession("s2", "Session 2", false)
	s.applySubAgent("s1", agent.SessionEvent{
		Type: agent.SessionEventSubAgent, AgentID: "a1", Name: "worker", Status: agent.StatusRunning,
	})

	report := stats.Report{Totals: stats.Totals{Primary: stats.ConnectorStat{
		Requests: 10, Errors: 1, TokensIn: 500, TokensOut: 50, CachedTokensIn: 100,
	}}}
	s.refreshOverallStats(report, &config.ModelConfig{Name: "groq-free", DisplayName: "Groq", Endpoint: "https://api.groq.com/openai/v1/chat/completions"}, "")

	if s.overall.Sessions != 2 {
		t.Errorf("Sessions = %d, want 2", s.overall.Sessions)
	}
	if s.overall.SubAgents != 1 {
		t.Errorf("SubAgents = %d, want 1", s.overall.SubAgents)
	}
	if s.overall.Requests != 10 || s.overall.Errors != 1 {
		t.Errorf("Requests/Errors = %d/%d, want 10/1", s.overall.Requests, s.overall.Errors)
	}
	if s.overall.CacheHitPct != 20 {
		t.Errorf("CacheHitPct = %d, want 20", s.overall.CacheHitPct)
	}
	// Issue #534: even though a non-nil (focused-session) model config is threaded
	// through by refreshOverallStats, the aggregate view (selectedModel == "") must
	// leave the model/api rows blank — a cluster-wide total names no single backend.
	if s.overall.Model != "" || s.overall.APIEndpoint != "" {
		t.Errorf("Model/API = %q/%q, want blank (aggregate view ignores the passed config, issue #534)",
			s.overall.Model, s.overall.APIEndpoint)
	}
}

// TestSidebarOverallBandReservation covers the LayoutFn split: the bottom band is
// reserved (and the tree shrunk) when the sidebar is tall enough, and dropped so
// a short terminal keeps a usable session tree. SetBounds invokes LayoutFn.
func TestSidebarOverallBandReservation(t *testing.T) {
	for _, tc := range []struct {
		name      string
		h         int
		wantBand  int
		wantTreeH int
	}{
		{"tall reserves band", overallBandHeight + 20, overallBandHeight, overallBandHeight + 20 - 1 - overallBandHeight},
		{"just enough for band + min tree", overallBandHeight + 5, overallBandHeight, minSidebarTreeHeight},
		{"one short drops band", overallBandHeight + 4, 0, overallBandHeight + 4 - 1},
		{"very short drops band", 12, 0, 11},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestSidebar()
			s.panel.SetBounds(tv.Rect{X: 0, Y: 0, W: defaultSidebarWidth, H: tc.h})
			if s.overallBandH != tc.wantBand {
				t.Errorf("overallBandH = %d, want %d", s.overallBandH, tc.wantBand)
			}
			got := s.tree.Root().Bounds
			if got.H != tc.wantTreeH {
				t.Errorf("tree height = %d, want %d", got.H, tc.wantTreeH)
			}
			if got.W != defaultSidebarWidth-3 || got.X != 2 || got.Y != 1 {
				t.Errorf("tree rect = %+v, want W=%d X=2 Y=1", got, defaultSidebarWidth-3)
			}
		})
	}
}

// TestSidebarTodosRegionReservation covers the issue #190 third region in
// LayoutFn: with a focused session holding a checklist, the middle TODO region is
// reserved above the Overall band and the tree shrinks by exactly that much. It
// also pins the drop precedence — the TODO region drops before the band as the
// sidebar shrinks (tree wins) — and the maxTodoRegionItems cap. With an empty
// checklist this is all a no-op (todosBandH == 0), which the existing
// TestSidebarOverallBandReservation already pins byte-for-byte.
func TestSidebarTodosRegionReservation(t *testing.T) {
	// 3 checklist items -> todoLineCount 3 -> region height 1 (title) + 3.
	const todosH = todoRegionTitleLines + 3
	for _, tc := range []struct {
		name      string
		h         int
		wantBand  int
		wantTodos int
		wantTreeH int
	}{
		{"tall keeps band and todos", 30, overallBandHeight, todosH, 30 - 1 - overallBandHeight - todosH},
		{"just enough for both regions + min tree", 21, overallBandHeight, todosH, minSidebarTreeHeight},
		{"one short drops todos, keeps band", 20, overallBandHeight, 0, 20 - 1 - overallBandHeight},
		{"very short drops both regions", 8, 0, 0, 8 - 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestSidebar()
			s.addSession("s1", "Session 1", false)
			s.applyTodo("s1", threeTodos())
			s.focusSession("s1")
			s.panel.SetBounds(tv.Rect{X: 0, Y: 0, W: defaultSidebarWidth, H: tc.h})

			if s.overallBandH != tc.wantBand {
				t.Errorf("overallBandH = %d, want %d", s.overallBandH, tc.wantBand)
			}
			if s.todosBandH != tc.wantTodos {
				t.Errorf("todosBandH = %d, want %d", s.todosBandH, tc.wantTodos)
			}
			got := s.tree.Root().Bounds
			if got.H != tc.wantTreeH {
				t.Errorf("tree height = %d, want %d", got.H, tc.wantTreeH)
			}
		})
	}
}

// TestSidebarTodosRegionCap pins that a checklist longer than maxTodoRegionItems
// reserves only the capped height, so a long list cannot crowd out the tree.
func TestSidebarTodosRegionCap(t *testing.T) {
	s := newTestSidebar()
	s.addSession("s1", "Session 1", false)
	many := make([]agent.TodoItem, maxTodoRegionItems+4)
	for i := range many {
		many[i] = agent.TodoItem{Content: "item", Status: agent.TodoPending}
	}
	s.applyTodo("s1", many)
	s.focusSession("s1")
	s.panel.SetBounds(tv.Rect{X: 0, Y: 0, W: defaultSidebarWidth, H: 40})

	want := todoRegionTitleLines + maxTodoRegionItems
	if s.todosBandH != want {
		t.Errorf("todosBandH = %d, want capped %d", s.todosBandH, want)
	}
}

// TestSidebarRegionDropKeepsTreeMinimum is the "tree wins" invariant: no matter
// the sidebar height or checklist size, a shown region (Overall band or TODOs)
// never pushes the session tree below minSidebarTreeHeight — the region is
// dropped first. This is the acceptance criterion "a very short sidebar drops the
// TODO region (and/or the Overall band) before shrinking the session tree".
func TestSidebarRegionDropKeepsTreeMinimum(t *testing.T) {
	s := newTestSidebar()
	s.addSession("s1", "Session 1", false)
	s.applyTodo("s1", threeTodos())
	s.focusSession("s1")
	for h := 2; h <= 40; h++ {
		s.panel.SetBounds(tv.Rect{X: 0, Y: 0, W: defaultSidebarWidth, H: h})
		treeH := s.tree.Root().Bounds.H
		if s.todosBandH > 0 && treeH < minSidebarTreeHeight {
			t.Errorf("h=%d: TODO region shown (todosBandH=%d) but tree %d < min %d",
				h, s.todosBandH, treeH, minSidebarTreeHeight)
		}
		if s.overallBandH > 0 && treeH < minSidebarTreeHeight {
			t.Errorf("h=%d: Overall band shown (overallBandH=%d) but tree %d < min %d",
				h, s.overallBandH, treeH, minSidebarTreeHeight)
		}
	}
}

// TestSidebarRegionDropMonotonic is the regression guard for the drop
// precedence: as the sidebar shrinks, the per-session TODO region must disappear
// before (and never outlive) the Overall band, so shrinking never makes the
// todos reappear after the band is gone and the persistent global summary is the
// last region standing. An earlier revision computed bandH independently of
// todosH, which produced a non-monotonic window (h≈9–15 for 3 todos) where the
// band was dropped while the TODO region was still shown; the fix cascades the
// drops (todos first, then band). This test pins both the corrected boundary
// sequence and the strong invariant across a full height sweep.
func TestSidebarRegionDropMonotonic(t *testing.T) {
	// Corrected boundary sequence with a 3-item checklist (region height 4).
	for _, tc := range []struct {
		h         int
		wantBand  int
		wantTodos int
		note      string
	}{
		{21, overallBandHeight, todoRegionTitleLines + 3, "both regions shown"},
		{20, overallBandHeight, 0, "todos dropped, band kept"},
		{17, overallBandHeight, 0, "todos dropped, band still kept at the edge"},
		{16, 0, 0, "both dropped once the band no longer fits"},
		{12, 0, 0, "both dropped (no todos-without-band inversion)"},
		{8, 0, 0, "both dropped on a short sidebar"},
	} {
		s := newTestSidebar()
		s.addSession("s1", "Session 1", false)
		s.applyTodo("s1", threeTodos())
		s.focusSession("s1")
		s.panel.SetBounds(tv.Rect{X: 0, Y: 0, W: defaultSidebarWidth, H: tc.h})
		if s.overallBandH != tc.wantBand || s.todosBandH != tc.wantTodos {
			t.Errorf("h=%d (%s): overallBandH=%d todosBandH=%d, want band=%d todos=%d",
				tc.h, tc.note, s.overallBandH, s.todosBandH, tc.wantBand, tc.wantTodos)
		}
	}

	// Strong invariant across every height: the TODO region is never shown
	// without the Overall band (todos drops first / with the band, never after).
	s := newTestSidebar()
	s.addSession("s1", "Session 1", false)
	s.applyTodo("s1", threeTodos())
	s.focusSession("s1")
	for h := 2; h <= 40; h++ {
		s.panel.SetBounds(tv.Rect{X: 0, Y: 0, W: defaultSidebarWidth, H: h})
		if s.todosBandH > 0 && s.overallBandH == 0 {
			t.Errorf("h=%d: TODO region shown (todosBandH=%d) while Overall band dropped — todos must not outlive the band",
				h, s.todosBandH)
		}
	}
}

// TestSidebarOverallCountExcludesTodos is the regression guard from the issue:
// the Overall band's session / sub-agent counts are drawn from len(s.sessions)
// and len(s.agents), so adding a checklist never inflates the sub-agent count.
// (Before #190, todos shared the tree but were never counted here either; this
// pins that nothing regressed when todos moved out of the tree.)
func TestSidebarOverallCountExcludesTodos(t *testing.T) {
	s := newTestSidebar()
	s.addSession("s1", "Session 1", false)
	s.applySubAgent("s1", agent.SessionEvent{
		Type: agent.SessionEventSubAgent, AgentID: "a1", Name: "worker", Status: agent.StatusRunning,
	})
	s.applyTodo("s1", threeTodos())

	s.refreshOverallStats(stats.Report{}, nil, "")

	if s.overall.Sessions != 1 {
		t.Errorf("Sessions = %d, want 1", s.overall.Sessions)
	}
	if s.overall.SubAgents != 1 {
		t.Errorf("SubAgents = %d, want 1 (todos must not be counted as sub-agents)", s.overall.SubAgents)
	}
	// Direct source checks: todos live in s.todos, never in s.agents.
	if len(s.agents) != 1 {
		t.Errorf("len(s.agents) = %d, want 1", len(s.agents))
	}
	if len(s.todos) != 1 {
		t.Errorf("len(s.todos) = %d, want 1 (one session's checklist)", len(s.todos))
	}
	// A second session with its own todos must not move the sub-agent count.
	s.addSession("s2", "Session 2", false)
	s.applyTodo("s2", threeTodos())
	s.refreshOverallStats(stats.Report{}, nil, "")
	if s.overall.Sessions != 2 {
		t.Errorf("Sessions = %d, want 2", s.overall.Sessions)
	}
	if s.overall.SubAgents != 1 {
		t.Errorf("SubAgents = %d, want 1 (still only the one sub-agent)", s.overall.SubAgents)
	}
}

// TestScheduleOverallRefreshSkipsWithoutHandler ensures the coalesced refresh is
// a no-op when the statistics handler is absent (e.g. the workbench used without
// a backend, or in tests), so it never arms a timer that would fire a Post onto
// a desktop that has nothing to show.
func TestScheduleOverallRefreshSkipsWithoutHandler(t *testing.T) {
	w := &Workbench{sidebar: newTestSidebar()} // no handlers.GetStatistics
	w.scheduleOverallRefresh()
	if w.statsRefresh != nil {
		t.Fatal("scheduled a refresh timer without a statistics handler")
	}
}

// TestFormatEndpoint covers the "api" row's endpoint -> host/provider rendering
// (issue #107): an explicit URL collapses to its host[:port], an empty endpoint
// falls back to the provider (api_type), defaulting to the OpenAI-compatible
// convention so the row is never blank.
func TestFormatEndpoint(t *testing.T) {
	for _, tc := range []struct {
		name     string
		endpoint string
		apiType  string
		want     string
	}{
		{"https host drops scheme and path", "https://api.groq.com/openai/v1/chat/completions", "", "api.groq.com"},
		{"http host keeps port", "http://127.0.0.1:8080/v1/chat/completions", "", "127.0.0.1:8080"},
		{"empty endpoint uses provider", "", "zai", "zai"},
		{"empty endpoint + openrouter", "", "openrouter", "openrouter"},
		{"empty everything defaults to openai", "", "", "openai"},
		{"unparseable endpoint falls back to raw", "not a url at all", "", "not a url at all"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := formatEndpoint(tc.endpoint, tc.apiType); got != tc.want {
				t.Errorf("formatEndpoint(%q, %q) = %q, want %q", tc.endpoint, tc.apiType, got, tc.want)
			}
		})
	}
}
