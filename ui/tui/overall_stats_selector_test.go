package ui

import (
	"testing"

	"gogent/internal/config"
	"gogent/internal/stats"
)

// reportForSelectorTests builds a report with a known grand total and two
// per-model breakdowns so buildOverallStats's scoping can be exercised.
func reportForSelectorTests() stats.Report {
	return stats.Report{
		Totals: stats.Totals{Primary: stats.ConnectorStat{
			Requests: 100, Errors: 5, TokensIn: 1000, TokensOut: 200, CachedTokensIn: 100,
		}},
		Models: []stats.ModelStat{
			{Name: "groq", Sessions: 1, SubAgents: 0, TokensIn: 300, TokensOut: 60,
				Connector: stats.ConnectorStat{Requests: 30, Errors: 1, TokensIn: 300, TokensOut: 60, CachedTokensIn: 30}},
			{Name: "glm", Sessions: 2, SubAgents: 3, TokensIn: 700, TokensOut: 140,
				Connector: stats.ConnectorStat{Requests: 70, Errors: 4, TokensIn: 700, TokensOut: 140, CachedTokensIn: 70}},
		},
	}
}

// TestBuildOverallStats_SelectedModelScopesMetrics is the core #191 feature test:
// when a model is selected, every metric below the selector is scoped to that
// model's ModelStat (tokens/requests/errors/cache from its Connector, plus its
// attributed sessions/sub-agents), NOT the grand total.
func TestBuildOverallStats_SelectedModelScopesMetrics(t *testing.T) {
	rep := reportForSelectorTests()
	glmCfg := &config.ModelConfig{Name: "glm", DisplayName: "GLM", Connection: "glm"}

	got := buildOverallStats(rep, 0, 0, glmCfg, "glm")

	if got.Requests != 70 || got.Errors != 4 || got.TokensIn != 700 || got.TokensOut != 140 {
		t.Errorf("scoped metrics = %+v, want glm's {Req:70 Err:4 In:700 Out:140}", got)
	}
	if got.Sessions != 2 || got.SubAgents != 3 {
		t.Errorf("scoped counts = Sessions %d SubAgents %d, want glm's 2/3", got.Sessions, got.SubAgents)
	}
	// cache hit = 70/700 = 10%.
	if got.CacheHitPct != 10 {
		t.Errorf("CacheHitPct = %d, want 10 (70/700)", got.CacheHitPct)
	}
	// The model row describes the selected model's backend.
	if got.Model != "GLM" {
		t.Errorf("Model = %q, want GLM display name", got.Model)
	}
}

// TestBuildOverallStats_AllModelsShowsGrandTotal pins the back-compat aggregate
// view: an empty selection ("all models") must reproduce today's grand total
// (Totals.Primary) plus the sidebar's own session/sub-agent counts, even when a
// per-model breakdown is present.
func TestBuildOverallStats_AllModelsShowsGrandTotal(t *testing.T) {
	rep := reportForSelectorTests()

	got := buildOverallStats(rep, 4, 6, nil, "")

	if got.Requests != 100 || got.Errors != 5 || got.TokensIn != 1000 || got.TokensOut != 200 {
		t.Errorf("aggregate metrics = %+v, want grand total {Req:100 Err:5 In:1000 Out:200}", got)
	}
	if got.Sessions != 4 || got.SubAgents != 6 {
		t.Errorf("aggregate counts = Sessions %d SubAgents %d, want the passed 4/6", got.Sessions, got.SubAgents)
	}
	// 100/1000 = 10% cache hit.
	if got.CacheHitPct != 10 {
		t.Errorf("CacheHitPct = %d, want 10", got.CacheHitPct)
	}
}

// TestBuildOverallStats_SelectedModelNotFound degrades cleanly: a selection that
// does not match any ModelStat shows zeros (ok=false leaves the zero struct),
// never the grand total or another model's numbers.
func TestBuildOverallStats_SelectedModelNotFound(t *testing.T) {
	rep := reportForSelectorTests()
	got := buildOverallStats(rep, 4, 6, nil, "does-not-exist")
	if got.Requests != 0 || got.TokensIn != 0 || got.Sessions != 0 || got.Errors != 0 {
		t.Errorf("unknown-model selection = %+v, want all zeros (not the grand total or a sibling model)", got)
	}
}

// TestOverallBandHeight_IncludesSelector reaffirms the band height reservation
// independently of the rendering test: the model selector adds exactly one row
// on top of the separator + title + metric rows.
func TestOverallBandHeight_IncludesSelector(t *testing.T) {
	if overallBandHeight != overallSelectorLines+overallMetricLines+2 {
		t.Fatalf("overallBandHeight = %d, want selector(%d)+metrics(%d)+2",
			overallBandHeight, overallSelectorLines, overallMetricLines)
	}
	if overallSelectorLines != 1 {
		t.Errorf("overallSelectorLines = %d, want 1", overallSelectorLines)
	}
	// The selector must have grown the band beyond the pre-#191 metric+2 height.
	if overallBandHeight <= overallMetricLines+2 {
		t.Errorf("overallBandHeight %d did not grow for the selector row", overallBandHeight)
	}
}

// --- sidebar / workbench selector wiring ---

// TestSidebar_RebuildModelOptions confirms the selector offers the aggregate
// option followed by every model's display name, with the parallel config-name
// keys (empty for the aggregate) the report is keyed on.
func TestSidebar_RebuildModelOptions(t *testing.T) {
	w := newTestWorkbench(t)
	w.SetModels([]*config.ModelConfig{
		{Name: "groq-free", DisplayName: "Groq"},
		{Name: "glm", DisplayName: "GLM"},
	})

	s := w.sidebar
	wantOpts := []string{overallAllModelsOption, "Groq", "GLM"}
	if len(s.overallSelect.Options) != len(wantOpts) {
		t.Fatalf("options = %v, want %v", s.overallSelect.Options, wantOpts)
	}
	for i, want := range wantOpts {
		if s.overallSelect.Options[i] != want {
			t.Errorf("option[%d] = %q, want %q", i, s.overallSelect.Options[i], want)
		}
	}
	wantKeys := []string{"", "groq-free", "glm"}
	for i, want := range wantKeys {
		if i >= len(s.overallModelKeys) || s.overallModelKeys[i] != want {
			t.Errorf("key[%d] = %q, want %q", i, s.overallModelKeys[i], want)
		}
	}
}

// TestSidebar_RebuildModelOptions_FallsBackToName covers the no-display-name
// path: the config Name is used as the label.
func TestSidebar_RebuildModelOptions_FallsBackToName(t *testing.T) {
	w := newTestWorkbench(t)
	w.SetModels([]*config.ModelConfig{{Name: "bare-model"}}) // no DisplayName
	s := w.sidebar
	if len(s.overallSelect.Options) != 2 || s.overallSelect.Options[1] != "bare-model" {
		t.Errorf("options = %v, want [all models, bare-model]", s.overallSelect.Options)
	}
}

// TestSidebar_SelectionRoundTrip covers setSelected/selected for a real model and
// for the aggregate.
func TestSidebar_SelectionRoundTrip(t *testing.T) {
	w := newTestWorkbench(t)
	w.SetModels([]*config.ModelConfig{{Name: "groq-free"}, {Name: "glm"}})
	s := w.sidebar

	if got := s.selectedOverallModel(); got != "" {
		t.Errorf("initial selection = %q, want empty (aggregate)", got)
	}
	s.setSelectedOverallModel("glm")
	if got := s.selectedOverallModel(); got != "glm" {
		t.Errorf("after selecting glm = %q, want glm", got)
	}
	s.setSelectedOverallModel("")
	if got := s.selectedOverallModel(); got != "" {
		t.Errorf("after selecting aggregate = %q, want empty", got)
	}
}

// TestSidebar_SelectionPreservedAcrossRebuild ensures refreshing the model list
// (e.g. after editing models) does not silently re-scope the panel: the current
// selection is preserved by config name when the model is still present.
func TestSidebar_SelectionPreservedAcrossRebuild(t *testing.T) {
	w := newTestWorkbench(t)
	w.SetModels([]*config.ModelConfig{{Name: "groq-free"}, {Name: "glm"}})
	s := w.sidebar
	s.setSelectedOverallModel("glm")

	// Rebuild with the same set (simulates a re-sync).
	w.SetModels([]*config.ModelConfig{{Name: "groq-free"}, {Name: "glm"}})
	if got := s.selectedOverallModel(); got != "glm" {
		t.Errorf("selection after rebuild = %q, want glm preserved", got)
	}
}

// TestSidebar_SelectionFallsBackWhenModelRemoved covers the graceful-degradation
// path: a selection whose model was renamed/removed falls back to the aggregate
// instead of pointing at a stale index.
func TestSidebar_SelectionFallsBackWhenModelRemoved(t *testing.T) {
	w := newTestWorkbench(t)
	w.SetModels([]*config.ModelConfig{{Name: "groq-free"}, {Name: "glm"}})
	s := w.sidebar
	s.setSelectedOverallModel("glm")

	// glm is removed in the new list.
	w.SetModels([]*config.ModelConfig{{Name: "groq-free"}})
	if got := s.selectedOverallModel(); got != "" {
		t.Errorf("selection after model removed = %q, want empty (aggregate fallback)", got)
	}
}

// TestWorkbench_OverallModelLayoutRoundTrip covers acceptance item #3: the
// selector choice is captured into the layout and restored on apply, surviving a
// capture/apply round-trip across two workbenches (simulating a restart).
func TestWorkbench_OverallModelLayoutRoundTrip(t *testing.T) {
	models := []*config.ModelConfig{{Name: "groq-free"}, {Name: "glm"}}

	w := newTestWorkbench(t)
	w.SetModels(models)
	w.sidebar.setSelectedOverallModel("glm")

	layout := w.captureLayout()
	if layout.OverallModel != "glm" {
		t.Fatalf("captureLayout.OverallModel = %q, want glm", layout.OverallModel)
	}

	// Apply on a fresh workbench (the restart path).
	w2 := newTestWorkbench(t)
	w2.SetModels(models)
	w2.applyLayout(layout)
	if got := w2.sidebar.selectedOverallModel(); got != "glm" {
		t.Errorf("after applyLayout: selection = %q, want glm restored", got)
	}
}

// TestWorkbench_OverallModelLayoutAggregateDefault confirms the default capture
// is the aggregate (empty), so a fresh run persists "all models".
func TestWorkbench_OverallModelLayoutAggregateDefault(t *testing.T) {
	w := newTestWorkbench(t)
	w.SetModels([]*config.ModelConfig{{Name: "glm"}})
	layout := w.captureLayout()
	if layout.OverallModel != "" {
		t.Errorf("default OverallModel = %q, want empty (aggregate)", layout.OverallModel)
	}
}

// TestWorkbench_RefreshOverallScopesToSelection drives the full refresh path:
// with a model selected, the rendered Overall band reflects that model's scoped
// metrics from the Statistics report; with the aggregate selected, it reflects
// the grand total.
func TestWorkbench_RefreshOverallScopesToSelection(t *testing.T) {
	rep := reportForSelectorTests()
	w := newTestWorkbench(t)
	w.SetModels([]*config.ModelConfig{
		{Name: "groq-free", DisplayName: "Groq"},
		{Name: "glm", DisplayName: "GLM"},
	})
	w.handlers.GetStatistics = func() stats.Report { return rep }

	// Scoped to glm.
	w.sidebar.setSelectedOverallModel("glm")
	w.refreshOverall()
	if got := w.sidebar.overall; got.Requests != 70 || got.TokensIn != 700 || got.Errors != 4 || got.Sessions != 2 {
		t.Errorf("scoped overall = %+v, want glm's {Req:70 In:700 Err:4 Sessions:2}", got)
	}

	// Aggregate.
	w.sidebar.setSelectedOverallModel("")
	w.refreshOverall()
	// No session windows open, so the aggregate Sessions/SubAgents come from the
	// sidebar's own (empty) counts; the traffic figures come from Totals.Primary.
	if got := w.sidebar.overall; got.Requests != 100 || got.TokensIn != 1000 || got.Errors != 5 {
		t.Errorf("aggregate overall = %+v, want grand total {Req:100 In:1000 Err:5}", got)
	}
}

// TestWorkbench_modelByName covers the config-name lookup refreshOverall uses to
// resolve the selected model (distinct from the display-name lookup).
func TestWorkbench_modelByName(t *testing.T) {
	w := newTestWorkbench(t)
	w.SetModels([]*config.ModelConfig{
		{Name: "groq-free", DisplayName: "Groq"},
		{Name: "glm", DisplayName: "GLM"},
	})
	if m := w.modelByName("glm"); m == nil || m.Name != "glm" {
		t.Errorf("modelByName(glm) = %+v, want the glm config", m)
	}
	if m := w.modelByName("Groq"); m != nil {
		t.Errorf("modelByName(display name) = %+v, want nil (matches config name only)", m)
	}
	if m := w.modelByName("missing"); m != nil {
		t.Errorf("modelByName(missing) = %+v, want nil", m)
	}
}
