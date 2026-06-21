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

// statisticsDialogSize picks a size that fills a good chunk of a large terminal
// while still fitting (and staying useful) on a small one. It mirrors the
// Resources browser sizing so the two read-only explorers feel consistent.
func statisticsDialogSize(screenW, screenH int) (width, height int) {
	width, height = 80, 24
	if w := screenW - 2; width > w {
		width = w
	}
	if h := screenH - 2; height > h {
		height = h
	}
	if width < 60 {
		width = 60
	}
	if height < 14 {
		height = 14
	}
	return width, height
}

// showStatisticsDialog opens the Statistics view (issue #57): a sectioned,
// read-only breakdown of the counters gogent already collects — grand totals,
// per-session, per-tool, per-skill and per-model rows — with CSV/JSON export.
//
// The report is a point-in-time snapshot captured when the dialog opens. The
// underlying counters are in-memory (durable history arrives with the audit
// stream, issue #51).
func (w *Workbench) showStatisticsDialog() {
	if w.handlers.GetStatistics == nil {
		w.showConfirm("Statistics", "Statistics are unavailable.", nil)
		return
	}
	report := w.handlers.GetStatistics()

	width, height := statisticsDialogSize(w.app.Width(), w.app.Height())
	x, y := centeredDialog(w, width, height)

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
