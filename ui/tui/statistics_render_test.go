package ui

import (
	"path/filepath"
	"strings"
	"testing"

	"gogent/internal/stats"

	tv "github.com/hobbestherat/turbotui/turbotv"
)

// sampleStatsReport is a representative report reused across the render/export
// tests.
func sampleStatsReport() stats.Report {
	return stats.Report{
		GeneratedAt: 1_700_000_000,
		Totals: stats.Totals{
			Sessions: 2, Turns: 5, TokensIn: 1234567, TokensOut: 42300,
			ToolCalls: 9, Compactions: 1,
			Primary: stats.ConnectorStat{
				Requests: 150, Success: 148, Errors: 2, TokensIn: 1234567,
				TokensOut: 42300, TotalTimeMs: 123000, ContextOverflows: 1, GenericErrors: 1,
			},
			Fast: stats.ConnectorStat{Requests: 3, TokensIn: 200, TokensOut: 40},
		},
		Sessions: []stats.SessionRow{
			{ID: "session-1", Turns: 3, TokensIn: 800000, TokensOut: 30000, ToolCalls: 6,
				ContextTokens: 6000, ContextWindow: 8000, Compactions: 1,
				Primary: stats.ConnectorStat{Requests: 90, Errors: 1}},
			{ID: "session-2", Turns: 2, TokensIn: 434567, TokensOut: 12300, ToolCalls: 3,
				ContextTokens: 2000, ContextWindow: 8000,
				Primary: stats.ConnectorStat{Requests: 60, Errors: 1}},
		},
		Tools: []stats.ToolStat{
			{Name: "calc", Invocations: 4, Success: 4, TotalMs: 80, ResultBytes: 42},
			{Name: "shell", Invocations: 5, Success: 3, Failure: 2, TotalMs: 12000, ResultBytes: 4096},
		},
		Skills: []stats.SkillStat{
			{Name: "writer", Success: 2, Failure: 0, TotalCalls: 2},
		},
		Models: []stats.ModelStat{
			{Name: "opus", TokensIn: 1100000, TokensOut: 38000},
			{Name: "haiku", TokensIn: 134567, TokensOut: 4300},
		},
	}
}

// TestRenderStatisticsOverview covers the overview header, the grand totals, the
// primary/fast connector blocks and the per-model token summary.
func TestRenderStatisticsOverview(t *testing.T) {
	got := renderStatistics(statsOverview, sampleStatsReport())
	for _, want := range []string{
		"Overview",
		"Sessions:",
		"Turns:",
		"Primary model backend",
		"Requests:",
		"Avg latency:",
		"Fast model backend",
		"Models (tokens)",
		"opus:",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("overview missing %q:\n%s", want, got)
		}
	}
	// The error breakdown line lists the four error categories.
	if !strings.Contains(got, "timeouts=") || !strings.Contains(got, "overflows=") {
		t.Errorf("overview missing error breakdown:\n%s", got)
	}
}

// TestRenderStatisticsOverviewOmitsFastWhenZero verifies the fast-model block is
// hidden when there was no fast-model usage (no fast model configured).
func TestRenderStatisticsOverviewOmitsFastWhenZero(t *testing.T) {
	r := sampleStatsReport()
	r.Totals.Fast = stats.ConnectorStat{}
	got := renderStatistics(statsOverview, r)
	if strings.Contains(got, "Fast model backend") {
		t.Errorf("overview should omit fast block when unused:\n%s", got)
	}
}

// TestRenderStatisticsSessions covers the sessions table header and one row per
// session, plus the empty-state message.
func TestRenderStatisticsSessions(t *testing.T) {
	got := renderStatistics(statsSessions, sampleStatsReport())
	for _, want := range []string{
		"Sessions", "ID", "Turns", "Tok in/out", "Tools", "Ctx%", "Reqs", "Errs", "Comp",
		"session-1", "session-2",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("sessions view missing %q:\n%s", want, got)
		}
	}

	empty := renderStatistics(statsSessions, stats.Report{})
	if !strings.Contains(empty, "No sessions.") {
		t.Errorf("empty sessions view = %q, want no-sessions message", empty)
	}
}

// TestRenderStatisticsTools covers the tools table and the avg-ms formatting
// (seconds for long durations).
func TestRenderStatisticsTools(t *testing.T) {
	got := renderStatistics(statsTools, sampleStatsReport())
	for _, want := range []string{"Tools", "Name", "Calls", "OK", "Fail", "Avg ms", "Bytes", "calc", "shell"} {
		if !strings.Contains(got, want) {
			t.Errorf("tools view missing %q:\n%s", want, got)
		}
	}
	// shell: 12000ms / 5 = 2400ms -> "2400 ms"; calc: 80/4 = 20 -> "20 ms".
	if !strings.Contains(got, "20 ms") {
		t.Errorf("tools view missing calc avg 20 ms:\n%s", got)
	}
	if !strings.Contains(got, "42") || !strings.Contains(got, "4096") {
		t.Errorf("tools view missing result byte counts:\n%s", got)
	}
}

// TestRenderStatisticsSkills covers the skills table and empty state.
func TestRenderStatisticsSkills(t *testing.T) {
	got := renderStatistics(statsSkills, sampleStatsReport())
	for _, want := range []string{"Skills", "Name", "OK", "Fail", "Total", "writer"} {
		if !strings.Contains(got, want) {
			t.Errorf("skills view missing %q:\n%s", want, got)
		}
	}
	empty := renderStatistics(statsSkills, stats.Report{})
	if !strings.Contains(empty, "No skills loaded.") {
		t.Errorf("empty skills view = %q, want no-skills message", empty)
	}
}

// TestRenderStatisticsModels covers the per-model table and empty state, and the
// deferred-cost note.
func TestRenderStatisticsModels(t *testing.T) {
	got := renderStatistics(statsModels, sampleStatsReport())
	for _, want := range []string{"Models", "opus", "haiku", "Tokens in", "Tokens out"} {
		if !strings.Contains(got, want) {
			t.Errorf("models view missing %q:\n%s", want, got)
		}
	}
	empty := renderStatistics(statsModels, stats.Report{})
	if !strings.Contains(empty, "No per-model usage yet") {
		t.Errorf("empty models view = %q, want no-models message", empty)
	}
}

// TestFormatMs covers the compact latency formatting: zero/negative -> "-",
// sub-10s -> "N ms", >=10s -> "N.N s".
func TestFormatMs(t *testing.T) {
	for _, tc := range []struct {
		ms   int64
		want string
	}{
		{0, "-"},
		{-5, "-"},
		{820, "820 ms"},
		{9999, "9999 ms"},
		{10000, "10.0 s"},
		{125000, "125.0 s"},
	} {
		if got := formatMs(tc.ms); got != tc.want {
			t.Errorf("formatMs(%d) = %q, want %q", tc.ms, got, tc.want)
		}
	}
}

// TestStatisticsDialogSize covers the content-driven statistics dialog footprint:
// it uses a static 100x24 preferred size with 110x36 caps and 60x14 floors, not
// the browser dialog's terminal-share spec that ballooned to 160x42 on 200x50.
func TestStatisticsDialogSize(t *testing.T) {
	wb := newTestWorkbench(t)
	spec := wb.statisticsDialogSpec()
	if spec.PreferredW != 100 || spec.PrefH != 24 {
		t.Fatalf("statistics preferred size = %dx%d, want 100x24", spec.PreferredW, spec.PrefH)
	}
	if spec.MinW != 60 || spec.MinH != 14 || spec.MaxW != 110 || spec.MaxH != 36 {
		t.Fatalf("statistics spec = %+v, want floors 60x14 and caps 110x36", spec)
	}

	for _, tc := range []struct {
		name             string
		screenW, screenH int
		wantW, wantH     int
	}{
		{"roomy terminal uses content size", 200, 50, 100, 24},
		{"wide terminal stays capped to content", 300, 80, 100, 24},
		{"120 wide terminal clamps width to 80 percent cap", 120, 40, 96, 24},
		{"short terminal clamps height above floor", 120, 16, 96, 14},
		{"tiny terminal floors both dimensions", 50, 12, 60, 14},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, gotW, gotH := tv.ResolveDialogRect(spec, tc.screenW, tc.screenH)
			if gotW != tc.wantW || gotH != tc.wantH {
				t.Errorf("statistics size(%d,%d) = %dx%d, want %dx%d",
					tc.screenW, tc.screenH, gotW, gotH, tc.wantW, tc.wantH)
			}
		})
	}
}

func TestStatisticsDialogOpensContentDriven(t *testing.T) {
	for _, tc := range []struct {
		name             string
		screenW, screenH int
		wantW, wantH     int
	}{
		{"roomy terminal is not browser balloon", 200, 50, 100, 24},
		{"120 wide terminal respects percentage cap", 120, 40, 96, 24},
		{"tiny terminal floors both dimensions", 50, 12, 60, 14},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := newTestWorkbench(t)
			w.handlers.GetStatistics = func() stats.Report { return sampleStatsReport() }
			w.app.Resize(tc.screenW, tc.screenH)
			w.showStatisticsDialog()

			top := w.desktop.TopLayer()
			if top == nil || top.Name != "statistics-dialog" {
				t.Fatalf("top layer = %v, want statistics-dialog", top)
			}
			b := dialogBounds(w)
			if b.W != tc.wantW || b.H != tc.wantH {
				t.Fatalf("statistics dialog on %dx%d = %dx%d, want %dx%d",
					tc.screenW, tc.screenH, b.W, b.H, tc.wantW, tc.wantH)
			}
			if b.W == 160 && b.H == 42 {
				t.Fatal("statistics dialog still opened as the 160x42 browser balloon")
			}
			wantX, wantY := (tc.screenW-b.W)/2, (tc.screenH-b.H)/2
			if wantX < 0 {
				wantX = 0
			}
			if wantY < 0 {
				wantY = 0
			}
			if b.X != wantX || b.Y != wantY {
				t.Errorf("statistics origin = (%d,%d), want centered (%d,%d)", b.X, b.Y, wantX, wantY)
			}
		})
	}
}

func TestStatisticsDialogResizePathIndependent(t *testing.T) {
	resized := newTestWorkbench(t)
	resized.handlers.GetStatistics = func() stats.Report { return sampleStatsReport() }
	resized.app.Resize(80, 24)
	resized.showStatisticsDialog()
	before := dialogBounds(resized)

	resized.app.Resize(200, 50)
	got := dialogBounds(resized)

	fresh := newTestWorkbench(t)
	fresh.handlers.GetStatistics = func() stats.Report { return sampleStatsReport() }
	fresh.app.Resize(200, 50)
	fresh.showStatisticsDialog()
	want := dialogBounds(fresh)

	if before.W != 64 || before.H != 20 {
		t.Fatalf("statistics opened on 80x24 = %dx%d, want 64x20", before.W, before.H)
	}
	if got.W != want.W || got.H != want.H {
		t.Fatalf("statistics after resize = %dx%d, want fresh-open size %dx%d", got.W, got.H, want.W, want.H)
	}
	if got.W != 100 || got.H != 24 {
		t.Fatalf("statistics after resize to 200x50 = %dx%d, want 100x24", got.W, got.H)
	}
	if got.X != (200-got.W)/2 || got.Y != (50-got.H)/2 {
		t.Errorf("statistics not re-centered after resize: origin (%d,%d)", got.X, got.Y)
	}
}

func TestStatisticsDialogUnavailableUsesCompactFallback(t *testing.T) {
	w := newTestWorkbench(t)
	w.app.Resize(200, 50)
	w.showStatisticsDialog()

	top := w.desktop.TopLayer()
	if top == nil {
		t.Fatal("expected an unavailable confirmation dialog")
	}
	if top.Name == "statistics-dialog" {
		t.Fatal("showStatisticsDialog opened statistics dialog without a GetStatistics handler")
	}
	b := dialogBounds(w)
	if b.W >= 100 || b.H >= 24 {
		t.Errorf("unavailable fallback size = %dx%d, want compact confirmation smaller than statistics dialog", b.W, b.H)
	}
}

// TestWriteStatisticsExport verifies the export writes a file with the expected
// extension and content via an in-memory writer (so the test does not touch the
// user's home directory).
func TestWriteStatisticsExport(t *testing.T) {
	// Redirect the file writer to capture the written path + data instead of
	// touching disk under the real home directory.
	var writtenPath, writtenData string
	orig := statsExporter
	statsExporter = func(path, data string) error {
		writtenPath, writtenData = path, data
		return nil
	}
	t.Cleanup(func() { statsExporter = orig })

	for _, tc := range []struct {
		format string
		ext    string
	}{
		{"csv", ".csv"},
		{"json", ".json"},
	} {
		t.Run(tc.format, func(t *testing.T) {
			writtenPath, writtenData = "", ""
			path, err := writeStatisticsExport(sampleStatsReport(), tc.format)
			if err != nil {
				t.Fatalf("writeStatisticsExport(%s): %v", tc.format, err)
			}
			if path != writtenPath {
				t.Errorf("returned path %q != written path %q", path, writtenPath)
			}
			if ext := filepath.Ext(path); ext != tc.ext {
				t.Errorf("path ext = %q, want %q (path=%s)", ext, tc.ext, path)
			}
			if writtenData == "" {
				t.Errorf("%s export wrote empty data", tc.format)
			}
		})
	}
}

// TestWriteStatisticsExportUnknownFormatDefaultsJSON verifies an unknown format
// falls back to JSON rather than writing an empty/garbage file.
func TestWriteStatisticsExportUnknownFormatDefaultsJSON(t *testing.T) {
	var writtenPath string
	orig := statsExporter
	statsExporter = func(path, data string) error { writtenPath = path; return nil }
	t.Cleanup(func() { statsExporter = orig })

	if _, err := writeStatisticsExport(sampleStatsReport(), "xml"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ext := filepath.Ext(writtenPath); ext != ".json" {
		t.Errorf("unknown format ext = %q, want .json fallback", ext)
	}
}
