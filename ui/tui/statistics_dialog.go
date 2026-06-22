package ui

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"gogent/internal/stats"

	tui "github.com/hobbestherat/turbotui"
	tv "github.com/hobbestherat/turbotui/turbotv"
)

// showStatisticsDialog opens the Statistics view (issue #57): a sectioned,
// read-only breakdown of the counters gogent already collects — grand totals,
// per-session, per-tool, per-skill and per-model rows — with CSV/JSON export.
//
// The report is a point-in-time snapshot captured when the dialog opens. It is
// folded through the process-lifetime accumulator (the same path the Overall
// panel uses) so the view shows cross-session totals and keeps the per-session
// rows of sessions that have closed during the run (issue #277), and the phantom
// backend "default" session — which has no TUI window — is filtered out first so
// the Sessions count matches the sidebar (issue #278). The underlying counters are
// in-memory (durable history arrives with the audit stream, issue #51).
func (w *Workbench) showStatisticsDialog() {
	if w.handlers.GetStatistics == nil {
		w.showConfirm("Statistics", "Statistics are unavailable.", nil)
		return
	}
	report := w.overallLifetime.fold(filterPhantomSessions(w.handlers.GetStatistics()))

	// Large by default (≈85% of the terminal) with a 60×14 floor so the two-pane
	// browser stays usable on a small terminal; the list/detail split is derived
	// from width below, so the panes grow with the dialog (issue #299).
	spec := tv.DialogSpec{MinW: 60, MinH: 14, PreferredW: w.app.Width() * 85 / 100}
	x, y, width, height := w.dialogRect(spec)

	dialog := tv.NewDialog("Statistics", x, y, width, height)
	applyWindowShadow(dialog.Window) // honour the NoShadow theme setting (issue #215)
	dialog.Window.ShowClose = false

	listX := 2
	headerY := 3
	listY := 4
	// The footer is two rows: the keyboard hint on its own row (so it can never
	// overlap the buttons) and the action-button row beneath it.
	hintY := height - 4
	buttonY := height - 3
	paneH := hintY - listY // detail fills every row up to the hint
	if paneH < 3 {
		paneH = 3
	}

	dialog.Window.AddContent(dialogLabel("Section:", tv.Rect{X: 2, Y: 1, W: 8, H: 1}))
	sel := newSelect(w.desktop, statisticsSectionNames, tv.Rect{X: 11, Y: 1, W: 14, H: 1})
	dialog.Window.AddContent(sel)

	dialog.Window.AddContent(dialogLabel("Detail", tv.Rect{X: listX, Y: headerY, W: width - 4, H: 1}))

	detail := tv.NewTextView("", tv.Rect{X: listX, Y: listY, W: width - 4, H: paneH})
	detail.Wrap = true
	detail.FG = tv.DefaultTheme.DialogFG
	detail.BG = tv.DefaultTheme.DialogBG
	dialog.Window.AddContent(detail)

	// The hint sits on its own row above the buttons, spanning the full content
	// width, so it never collides with the action buttons (issue #104).
	dialog.Window.AddContent(dialogLabel("Tab move · Esc close",
		tv.Rect{X: 2, Y: hintY, W: width - 4, H: 1}))

	var layer *tv.Layer
	closeFn := func() { w.desktop.RemoveLayer(layer) }

	// render refreshes the detail pane for the selected section.
	render := func(section statisticsSection) {
		detail.SetText(renderStatistics(section, report))
		// Re-anchor at the top on every section change so a re-selection always
		// shows the start of the report (issue #174).
		detail.ScrollToTop()
		w.desktop.Redraw()
	}

	exportCSV := func() {
		path, err := writeStatisticsExport(report, "csv")
		msg := "Wrote CSV to:\n" + path
		if err != nil {
			msg = "CSV export failed:\n" + err.Error()
		}
		w.showConfirm("Export", msg, nil)
	}
	exportJSON := func() {
		path, err := writeStatisticsExport(report, "json")
		msg := "Wrote JSON to:\n" + path
		if err != nil {
			msg = "JSON export failed:\n" + err.Error()
		}
		w.showConfirm("Export", msg, nil)
	}

	// Action buttons are sized from their rendered labels and right-aligned to
	// the dialog interior, so they stay a clean, non-overlapping row at any width
	// (issue #104) instead of the previous hand-tuned fixed offsets.
	footer := footerButtonRects(
		[]string{"Export &CSV", "Export &JSON", "Close"},
		listX, width-3, buttonY, 2)
	dialog.Window.AddContent(newButton("Export &CSV", footer[0], exportCSV))
	dialog.Window.AddContent(newButton("Export &JSON", footer[1], exportJSON))
	dialog.Window.AddContent(newButton("Close", footer[2], closeFn))

	sel.OnChange = func(idx int) { render(statisticsSection(idx)) }

	dialog.Root().OnTypeFn = func(_ *tv.VisualComponent, event tui.TypeEvent) bool {
		if event.Key == tui.KeyEscape {
			closeFn()
			return true
		}
		return false
	}

	layer = tv.NewModalLayer("statistics-dialog", dialog)
	w.desktop.AddLayer(layer)
	dialog.Fit(spec) // re-resolve the rect when the terminal is resized (issue #299)
	render(statsOverview)
	w.desktop.SetFocus(sel)
}

// statsExporter decouples the dialog from the filesystem so the export path can
// be unit tested with an in-memory writer. It defaults to writing a real file.
var statsExporter = func(path, data string) error {
	return os.WriteFile(path, []byte(data), 0o600)
}

// writeStatisticsExport renders the report in the given format ("csv" or "json")
// and writes it to a timestamped file under ~/.gogent/, returning the path.
func writeStatisticsExport(report stats.Report, format string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home dir: %w", err)
	}
	dir := filepath.Join(home, ".gogent")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return "", fmt.Errorf("create export dir: %w", err)
	}

	var data string
	switch format {
	case "csv":
		s, err := report.CSV()
		if err != nil {
			return "", fmt.Errorf("render stats csv: %w", err)
		}
		data = s
	default: // "json"
		s, err := report.JSON()
		if err != nil {
			return "", fmt.Errorf("render stats json: %w", err)
		}
		data = s
		format = "json"
	}

	path := filepath.Join(dir, "gogent-stats-"+time.Now().Format("20060102-150405")+"."+format)
	if err := statsExporter(path, data); err != nil {
		return "", err
	}
	return path, nil
}
