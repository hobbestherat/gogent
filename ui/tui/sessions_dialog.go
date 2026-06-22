package ui

import (
	"fmt"
	"sort"
	"strings"
	"time"

	tui "github.com/hobbestherat/turbotui"
	tv "github.com/hobbestherat/turbotui/turbotv"
)

// showSessionsDialog opens the Saved Sessions browser (issue #58): a picker for
// every persisted session, listed from index metadata only (no transcript
// replay). Each row shows the title, date, turns and message count; the detail
// pane shows the full metadata. A session can be opened read-only in an
// analysis window (side-by-side comparison) or continued (re-opened live).
//
// The dialog stays open after an open/continue so several sessions can be
// opened at once; Esc or Close dismisses it.
func (w *Workbench) showSessionsDialog() {
	if w.handlers.ListSavedSessions == nil || w.handlers.OpenSavedSession == nil {
		w.showConfirm("Saved Sessions", "Saved-session browsing is unavailable.", nil)
		return
	}

	// Large by default (≈85% of the terminal), floored so it stays usable on a
	// small terminal; the list/detail split is derived from width below (#299).
	x, y, width, height := w.dialogRect(w.browserDialogSpec())

	dialog := tv.NewDialog("Saved Sessions", x, y, width, height)
	applyWindowShadow(dialog.Window) // honour the NoShadow theme setting (issue #215)
	dialog.Window.ShowClose = false

	listX := 2
	headerY := 3
	listY := 4
	paneH := height - listY - 4 // hint/button row + bottom margin + border
	if paneH < 3 {
		paneH = 3
	}
	listW := width/2 - 2
	if listW < 24 {
		listW = 24
	}
	detailX := listX + listW + 1
	detailW := width - detailX - 2
	if detailW < 20 {
		detailW = 20
	}

	// Search box (filters by title/id/model as the user types + submits).
	dialog.Window.AddContent(dialogLabel("Search:", tv.Rect{X: 2, Y: 1, W: 7, H: 1}))
	searchBoxX := 10
	searchBoxW := width - searchBoxX - 2
	if searchBoxW < 12 {
		searchBoxW = 12
		searchBoxX = width - searchBoxW - 2
	}
	searchBox := tv.NewTextBox("", tv.Rect{X: searchBoxX, Y: 1, W: searchBoxW, H: 1})
	dialog.Window.AddContent(searchBox)

	dialog.Window.AddContent(dialogLabel("Sessions", tv.Rect{X: listX, Y: headerY, W: listW, H: 1}))
	dialog.Window.AddContent(dialogLabel("Detail", tv.Rect{X: detailX, Y: headerY, W: detailW, H: 1}))

	list := tv.NewTree(tv.Rect{X: listX, Y: listY, W: listW, H: paneH})
	list.FG = tv.DefaultTheme.DialogFG
	list.BG = tv.DefaultTheme.DialogBG
	list.SelFG = tv.DefaultTheme.SelectionFG
	list.SelBG = tv.DefaultTheme.SelectionBG
	dialog.Window.AddContent(list)

	detail := tv.NewTextView("", tv.Rect{X: detailX, Y: listY, W: detailW, H: paneH})
	detail.Wrap = true
	detail.FG = tv.DefaultTheme.DialogFG
	detail.BG = tv.DefaultTheme.DialogBG
	dialog.Window.AddContent(detail)

	dialog.Window.AddContent(dialogLabel("Tab move · Enter open (analysis) · Esc close",
		tv.Rect{X: 2, Y: height - 3, W: width - 40, H: 1}))

	var layer *tv.Layer
	closeFn := func() { w.desktop.RemoveLayer(layer) }

	// --- browser state -----------------------------------------------------

	all := loadSessionItems(w.handlers.ListSavedSessions)

	// selectedMeta returns the meta behind the current list selection, if any.
	selectedMeta := func() (SessionMeta, bool) {
		if n := list.Selected(); n != nil {
			if m, ok := n.Data.(SessionMeta); ok {
				return m, true
			}
		}
		return SessionMeta{}, false
	}

	// render rebuilds the list from the current search query and points the
	// detail pane at the (clamped) selection.
	render := func() {
		items := filterSessions(all, searchBox.GetText())
		nodes := make([]*tv.TreeNode, 0, len(items))
		for i := range items {
			n := tv.NewTreeNode(formatSessionRow(items[i]))
			n.Data = items[i]
			nodes = append(nodes, n)
		}
		list.Roots = nodes
		if m, ok := selectedMeta(); ok {
			detail.SetText(formatSessionDetail(m))
		} else {
			detail.SetText(emptySessionsDetail(len(items), strings.TrimSpace(searchBox.GetText())))
		}
		// Re-anchor at the top whenever the detail pane is repopulated so a
		// re-selection always shows the start (issue #174).
		detail.ScrollToTop()
		w.desktop.Redraw()
	}

	list.OnSelect = func(n *tv.TreeNode) {
		if n == nil {
			return
		}
		if m, ok := n.Data.(SessionMeta); ok {
			detail.SetText(formatSessionDetail(m))
			detail.ScrollToTop()
			w.desktop.Redraw()
		}
	}

	// openAnalysis loads the session read-only and opens an analysis window,
	// leaving the browser open so several can be opened side-by-side.
	openAnalysis := func() {
		m, ok := selectedMeta()
		if !ok {
			return
		}
		rs, ok := w.handlers.OpenSavedSession(m.File, false)
		if !ok {
			w.showConfirm("Saved Sessions", "Could not load that session.", nil)
			return
		}
		w.OpenAnalysisSession(rs)
	}
	// continueSession re-opens the session live so the user can keep typing.
	continueSession := func() {
		m, ok := selectedMeta()
		if !ok {
			return
		}
		rs, ok := w.handlers.OpenSavedSession(m.File, true)
		if !ok {
			w.showConfirm("Saved Sessions",
				"That session is already open or could not be loaded.", nil)
			return
		}
		w.AdoptSession(rs)
	}

	list.OnActivate = func(*tv.TreeNode) { openAnalysis() }
	searchBox.OnSubmit = func() { render() }

	dialog.Window.AddContent(newButton("&Open (analysis)",
		tv.Rect{X: width - 38, Y: height - 3, W: 17, H: 1}, openAnalysis))
	dialog.Window.AddContent(newButton("&Continue",
		tv.Rect{X: width - 20, Y: height - 3, W: 10, H: 1}, continueSession))
	dialog.Window.AddContent(newButton("Close",
		tv.Rect{X: width - 9, Y: height - 3, W: 7, H: 1}, closeFn))

	dialog.Root().OnTypeFn = func(_ *tv.VisualComponent, event tui.TypeEvent) bool {
		if event.Key == tui.KeyEscape {
			closeFn()
			return true
		}
		return false
	}

	layer = tv.NewModalLayer("sessions-dialog", dialog)
	w.desktop.AddLayer(layer)
	// PreferredW is a share of the terminal, so re-resolve against the live
	// terminal on resize rather than the stale spec dialog.Fit would remember
	// (issue #299).
	installResizeReflow(w.desktop, dialog, layer, w.browserDialogSpec)
	render()
	w.desktop.SetFocus(list)
}

// loadSessionItems fetches the persisted-session metadata and orders it newest
// first (the order a browser user expects), tie-broken by id for stability. A
// nil getter yields no items.
func loadSessionItems(get func() []SessionMeta) []SessionMeta {
	if get == nil {
		return nil
	}
	items := get()
	sortSessionsNewestFirst(items)
	return items
}

// sortSessionsNewestFirst orders sessions by CreatedAt descending. CreatedAt is
// persisted as UTC RFC3339, so a lexical compare is chronological; the id
// tie-break keeps the order deterministic.
func sortSessionsNewestFirst(items []SessionMeta) {
	sort.SliceStable(items, func(i, j int) bool {
		if items[i].CreatedAt != items[j].CreatedAt {
			return items[i].CreatedAt > items[j].CreatedAt
		}
		return items[i].ID < items[j].ID
	})
}

// filterSessions returns the sessions whose title, id or model contain the query
// (case-insensitive). An empty query matches everything.
func filterSessions(items []SessionMeta, query string) []SessionMeta {
	q := strings.ToLower(strings.TrimSpace(query))
	out := make([]SessionMeta, 0, len(items))
	for _, m := range items {
		if q == "" ||
			strings.Contains(strings.ToLower(m.Title), q) ||
			strings.Contains(strings.ToLower(m.ID), q) ||
			strings.Contains(strings.ToLower(m.Model), q) {
			out = append(out, m)
		}
	}
	return out
}

// sessionRowTitleWidth is the column width reserved for the title in a list row.
const sessionRowTitleWidth = 26

// formatSessionRow renders one row of the list: a padded title followed by the
// date, turn count and message count.
func formatSessionRow(m SessionMeta) string {
	return fmt.Sprintf("%s %s  %dt %dm",
		padName(fallbackTitle(m), sessionRowTitleWidth),
		formatSessionDate(m.CreatedAt),
		m.Turns, m.Messages)
}

// formatSessionDetail renders the side-pane metadata for the selected session.
func formatSessionDetail(m SessionMeta) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Title: %s\n", fallbackTitle(m))
	fmt.Fprintf(&b, "ID: %s\n", m.ID)
	fmt.Fprintf(&b, "Created: %s\n", formatSessionDate(m.CreatedAt))
	fmt.Fprintf(&b, "Turns: %d\n", m.Turns)
	fmt.Fprintf(&b, "Messages: %d\n", m.Messages)
	fmt.Fprintf(&b, "Tokens: %s / %s\n", formatTokens(m.TokensIn), formatTokens(m.TokensOut))
	if m.Model != "" {
		fmt.Fprintf(&b, "Model: %s\n", m.Model)
	}
	return b.String()
}

// emptySessionsDetail is the placeholder shown when the list has no rows: an
// invitation when there are no saved sessions at all, or a no-match note while
// searching.
func emptySessionsDetail(count int, query string) string {
	if count == 0 && query == "" {
		return "No saved sessions yet.\n\nSessions are saved automatically as you " +
			"chat; reopen them here for analysis or to continue."
	}
	return "No matching sessions."
}

// fallbackTitle returns the session title, or its id when the title is empty —
// the same fallback the restored-session path uses.
func fallbackTitle(m SessionMeta) string {
	if m.Title != "" {
		return m.Title
	}
	return m.ID
}

// formatSessionDate renders a persisted RFC3339 timestamp as "YYYY-MM-DD HH:MM",
// falling back to the raw string when it cannot be parsed (so a non-standard
// timestamp still shows something rather than vanishing).
func formatSessionDate(created string) string {
	if created == "" {
		return "unknown"
	}
	if ts, err := time.Parse(time.RFC3339, created); err == nil {
		return ts.UTC().Format("2006-01-02 15:04")
	}
	return created
}
