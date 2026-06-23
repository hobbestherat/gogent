package ui

import (
	"fmt"
	"sort"
	"strings"

	tui "github.com/hobbestherat/turbotui"
	tv "github.com/hobbestherat/turbotui/turbotv"
)

// watchersFooterLabels are the action-button captions showWatchersDialog hands
// footerButtonRects, in display order (left to right). They are a package var so a
// test can reuse the exact labels rather than hard-coding (and re-typing) them.
//
// Six buttons, deliberately. Seven (with a footer "Close") need ~89 columns, which
// cannot fit on an 80-column terminal at all — so Close moved to the window's
// title-bar [■] (and Esc), and "Open Session" was shortened to "Open", leaving a
// row that fits a standard terminal once the dialog width is floored to it (see
// showWatchersDialog). The order is fixed: Open, then the schedule/run controls.
var watchersFooterLabels = []string{
	"&Open", "&Enable", "&Disable", "&Run", "S&top", "De&lete",
}

// showWatchersDialog opens the Watchers dialog (issue #329 Phase 4): the
// authoritative list of scheduled watchers with a detail pane and the full set of
// watcher controls. Each list row shows the watcher's name, schedule, next-fire
// and status (with a live ● busy marker while a fire runs) plus a Target column —
// the owning session id for an attached watcher, or a "free" badge for a free-
// running one. The detail pane shows the selected watcher's configured task text,
// its target and its last-run/result/error.
//
// The footer buttons drive the workbench's watcher handlers: Open (open/raise the
// watcher's session window), Enable/Disable (re-arm / stop the schedule), Run (fire
// now), Stop (cancel the in-flight fire) and Delete (remove entirely). The dialog
// is dismissed by Esc or the title-bar [■] (there is no footer Close — seven
// controls do not fit an 80-column terminal). Every action re-renders the list so
// the new state is reflected immediately.
//
// The dialog lists exactly what ListWatchers returns for the active session: every
// free-running watcher plus that session's attached watchers. It stays open after
// an action so several can be managed in one sitting.
func (w *Workbench) showWatchersDialog() {
	if w.handlers.ListWatchers == nil {
		w.showConfirm("Watchers", "Watcher management is unavailable.", nil)
		return
	}

	// Floor the dialog width at the footer's need so the action row never clamps
	// into overlap on a narrow terminal (#321) — the same pattern the Statistics
	// dialog uses. The content spec's 80%-of-screen resolution can fall below the
	// footer width on a standard terminal, so a local copy with a raised MinW is
	// used for sizing (the watchersDialogSpec method itself is left untouched, since
	// its resolved size is asserted directly). dialog.Fit re-applies this same spec
	// on resize, so the floor holds there too.
	spec := w.watchersDialogSpec()
	if need := footerRowMinWidth(watchersFooterLabels, tv.DefaultButtonGap); spec.MinW < need {
		spec.MinW = need
	}
	x, y, width, height := w.dialogRect(spec)

	dialog := tv.NewDialog("Watchers", x, y, width, height)
	applyWindowShadow(dialog.Window) // honour the NoShadow theme setting (issue #215)
	// The footer has no Close button (it would not fit a standard terminal with the
	// other six controls), so the window keeps its title-bar [■] close affordance;
	// Esc also dismisses. Its OnClose tears down the modal layer (wired below once
	// closeFn exists).
	dialog.Window.ShowClose = true

	listX := 2
	headerY := 1
	listY := 2
	// Two footer rows: the keyboard hint on its own row above the action buttons, so
	// a wide button group can never overlap the hint (mirrors the Sessions dialog,
	// issue #321).
	hintY := height - 4
	buttonY := height - 3
	paneH := height - listY - 5
	if paneH < 3 {
		paneH = 3
	}
	listW := width/2 - 2
	if listW < 30 {
		listW = 30
	}
	detailX := listX + listW + 1
	detailW := width - detailX - 2
	if detailW < 20 {
		detailW = 20
	}

	dialog.Window.AddContent(dialogLabel("Watchers", tv.Rect{X: listX, Y: headerY, W: listW, H: 1}))
	dialog.Window.AddContent(dialogLabel("Detail", tv.Rect{X: detailX, Y: headerY, W: detailW, H: 1}))

	list := tv.NewTree(tv.Rect{X: listX, Y: listY, W: listW, H: paneH})
	list.FG = tv.DefaultTheme.ListFG
	list.BG = tv.DefaultTheme.ListBG
	// Fall back to an inverted bar when the theme paints the selection the same as
	// the list background (matching the other dialog lists, issue #327).
	list.SelFG, list.SelBG = selectionColorsFor(
		tv.DefaultTheme.ListFG, tv.DefaultTheme.ListBG,
		tv.DefaultTheme.SelectionFG, tv.DefaultTheme.SelectionBG)
	dialog.Window.AddContent(list)

	detail := tv.NewTextView("", tv.Rect{X: detailX, Y: listY, W: detailW, H: paneH})
	detail.Wrap = true
	detail.FG = tv.DefaultTheme.DialogFG
	detail.BG = tv.DefaultTheme.DialogBG
	dialog.Window.AddContent(detail)

	dialog.Window.AddContent(dialogLabel("Tab move · Enter open · Esc close",
		tv.Rect{X: 2, Y: hintY, W: width - 4, H: 1}))

	var layer *tv.Layer
	closeFn := func() { w.desktop.RemoveLayer(layer) }
	// Route the title-bar [■] through closeFn so it removes the modal layer.
	dialog.Window.OnClose = func(*tv.Window) { closeFn() }

	// selectedWatcher returns the WatcherInfo behind the current list selection.
	selectedWatcher := func() (WatcherInfo, bool) {
		if n := list.Selected(); n != nil {
			if info, ok := n.Data.(WatcherInfo); ok {
				return info, true
			}
		}
		return WatcherInfo{}, false
	}

	// render rebuilds the list from a fresh watcher snapshot and points the detail
	// pane at the (clamped) selection. It re-reads the handler each call so an
	// action's result is reflected without closing the dialog.
	render := func() {
		items := loadWatcherItems(w.handlers.ListWatchers, w.ActiveID())
		nodes := make([]*tv.TreeNode, 0, len(items))
		for i := range items {
			n := tv.NewTreeNode(formatWatcherRow(items[i]))
			n.Data = items[i]
			nodes = append(nodes, n)
		}
		list.Roots = nodes
		if info, ok := selectedWatcher(); ok {
			detail.SetText(formatWatcherDetail(info))
		} else {
			detail.SetText(emptyWatchersDetail(len(items)))
		}
		detail.ScrollToTop()
		w.desktop.Redraw()
	}

	list.OnSelect = func(n *tv.TreeNode) {
		if n == nil {
			return
		}
		if info, ok := n.Data.(WatcherInfo); ok {
			detail.SetText(formatWatcherDetail(info))
			detail.ScrollToTop()
			w.desktop.Redraw()
		}
	}

	// openSession opens (or raises) the selected watcher's session window — the
	// target session for an attached watcher, the watcher:<name> session for a
	// free-running one. openWatcherSession adopts a not-yet-open watcher session
	// from disk rather than no-opping, and treats an empty id as a no-op.
	openSession := func() {
		if info, ok := selectedWatcher(); ok {
			w.openWatcherSession(info.SessionID)
		}
	}
	// act runs a watcher control (by id) over the current selection, reporting the
	// outcome and re-rendering so the list reflects the new state. A nil handler or
	// no selection is a friendly no-op.
	act := func(verb string, fn func(string) error) {
		info, ok := selectedWatcher()
		if !ok {
			return
		}
		if fn == nil {
			w.showConfirm("Watchers", "That action is unavailable.", nil)
			return
		}
		if err := fn(info.ID); err != nil {
			w.showConfirm("Watchers", fmt.Sprintf("Could not %s watcher %q: %v", verb, info.Name, err), nil)
			return
		}
		render()
	}

	list.OnActivate = func(*tv.TreeNode) { openSession() }

	// Action buttons are sized from their labels and right-aligned to the dialog
	// interior so they never overlap or clip, at any width (issue #321 pattern); the
	// dialog width is floored to their total above so the group fits even on a narrow
	// terminal. Order matches watchersFooterLabels: Open, Enable, Disable, Run, Stop,
	// Delete (Close lives on the window chrome).
	footer := footerButtonRects(watchersFooterLabels, 2, width-3, buttonY, tv.DefaultButtonGap)
	dialog.Window.AddContent(newButton(watchersFooterLabels[0], footer[0], openSession))
	dialog.Window.AddContent(newButton(watchersFooterLabels[1], footer[1], func() { act("enable", w.handlers.EnableWatcher) }))
	dialog.Window.AddContent(newButton(watchersFooterLabels[2], footer[2], func() { act("disable", w.handlers.DisableWatcher) }))
	dialog.Window.AddContent(newButton(watchersFooterLabels[3], footer[3], func() { act("run", w.handlers.RunWatcher) }))
	dialog.Window.AddContent(newButton(watchersFooterLabels[4], footer[4], func() { act("stop", w.handlers.StopWatcher) }))
	dialog.Window.AddContent(newButton(watchersFooterLabels[5], footer[5], func() { act("delete", w.handlers.DeleteWatcher) }))

	dialog.Root().OnTypeFn = func(_ *tv.VisualComponent, event tui.TypeEvent) bool {
		if event.Key == tui.KeyEscape {
			closeFn()
			return true
		}
		return false
	}

	layer = tv.NewModalLayer("watchers-dialog", dialog)
	w.desktop.AddLayer(layer)
	dialog.Fit(spec) // static, content-driven spec: Fit re-resolves it on resize
	render()
	w.desktop.SetFocus(list)
}

// footerRowMinWidth is the smallest dialog width at which footerButtonRects can lay
// labels out (separated by gap) without clamping any button into overlap: the sum
// of the buttons' rendered widths plus the inter-button gaps, plus the 4 columns
// footerButtonRects reserves at the dialog edges (the leftX=2 inset and the width-3
// right margin). showWatchersDialog floors the dialog width at this so the action
// row always fits, mirroring how the Statistics dialog floors its width above its
// button group (issue #321).
func footerRowMinWidth(labels []string, gap int) int {
	total := 0
	for i, l := range labels {
		if i > 0 {
			total += gap
		}
		total += tv.ButtonLabelWidth(l)
	}
	return total + 4
}

// loadWatcherItems fetches the watchers visible to sessionID and orders them
// free-running first, then by name, so the list order is stable across re-renders
// (a status flip must not reshuffle rows). A nil getter yields no items.
func loadWatcherItems(get func(string) []WatcherInfo, sessionID string) []WatcherInfo {
	if get == nil {
		return nil
	}
	// Copy before sorting: the UI must not mutate the slice the handler returned
	// (it is sorted in place otherwise), even though the backend currently hands
	// back a fresh snapshot — the contract shouldn't depend on that.
	items := append([]WatcherInfo(nil), get(sessionID)...)
	sort.SliceStable(items, func(i, j int) bool {
		if items[i].Free != items[j].Free {
			return items[i].Free // free-running first
		}
		if items[i].Name != items[j].Name {
			return items[i].Name < items[j].Name
		}
		return items[i].ID < items[j].ID
	})
	return items
}

// watcherRowNameWidth is the column width reserved for the watcher name in a row.
const watcherRowNameWidth = 18

// formatWatcherRow renders one list row: a live busy marker (● while a fire runs,
// blank otherwise), the padded name, the schedule, the next-fire time, the status
// token and the Target column (the session id for an attached watcher, or "free").
func formatWatcherRow(info WatcherInfo) string {
	busy := " "
	if info.Running {
		busy = sessionStatusIcon(true) // ●
	}
	sched := info.Schedule
	if sched == "" {
		sched = "—"
	}
	next := info.NextFire
	if next == "" {
		next = "—"
	}
	status := info.Status
	if !info.Enabled {
		status += "/off"
	}
	return fmt.Sprintf("%s %s %s  next:%s  %s  [%s]",
		busy, padName(info.Name, watcherRowNameWidth), sched, next, status, watcherTarget(info))
}

// formatWatcherDetail renders the side-pane detail for the selected watcher: its
// metadata followed by the full configured task text (the quick-read of the
// instructions, no need to open the session).
func formatWatcherDetail(info WatcherInfo) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Name: %s\n", info.Name)
	fmt.Fprintf(&b, "Target: %s\n", watcherTarget(info))
	if info.Schedule != "" {
		fmt.Fprintf(&b, "Schedule: %s\n", info.Schedule)
	}
	state := info.Status
	if !info.Enabled {
		state += " (disabled)"
	}
	fmt.Fprintf(&b, "Status: %s\n", state)
	if info.NextFire != "" {
		fmt.Fprintf(&b, "Next fire: %s\n", info.NextFire)
	}
	if info.LastRun != "" {
		fmt.Fprintf(&b, "Last run: %s\n", info.LastRun)
	}
	if info.LastResult != "" {
		fmt.Fprintf(&b, "Last result: %s\n", info.LastResult)
	}
	if info.LastError != "" {
		fmt.Fprintf(&b, "Last error: %s\n", info.LastError)
	}
	task := strings.TrimSpace(info.Task)
	if task == "" {
		task = "(no task configured)"
	}
	fmt.Fprintf(&b, "\nTask:\n%s\n", task)
	return b.String()
}

// watcherTarget renders the Target column / field: the owning session id for an
// attached watcher, or the "free" badge for a free-running one — the explicit
// attached-vs-free distinction the sidebar conveys only by tree placement.
func watcherTarget(info WatcherInfo) string {
	if info.Free {
		return "free"
	}
	if info.TargetSession == "" {
		return "(attached)"
	}
	return info.TargetSession
}

// emptyWatchersDetail is the placeholder shown when no watcher is selected (an
// empty list): an invitation explaining how watchers appear here.
func emptyWatchersDetail(count int) string {
	if count == 0 {
		return "No watchers.\n\nFree-running watchers are declared in config; " +
			"attached watchers are created from a session via the create_watcher tool."
	}
	return "Select a watcher to see its task and last run."
}
