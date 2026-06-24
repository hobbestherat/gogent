package ui

import (
	"fmt"
	"strings"

	tui "github.com/hobbestherat/turbotui"
	tv "github.com/hobbestherat/turbotui/turbotv"
)

// Custom-command version history + diff browser (issue #403). The left list shows
// every stored version (newest first); the right pane shows a line-by-line
// template diff plus a structural parameter diff between a chosen base version and
// the selected target version. Restore copies the selected version's content into
// the command as a new version (the restore is itself recorded), then notifies the
// editor to repaint.

var commandHistoryFooterLabels = []string{"Set &Base", "&Restore", "&Close"}

// showCommandHistory opens the history browser for the named command. onChange is
// invoked after a successful restore so the caller can refresh its view.
func (w *Workbench) showCommandHistory(name string, onChange func()) {
	if w.handlers.GetCommandHistory == nil {
		w.showConfirm("History", "Command history is unavailable.", nil)
		return
	}
	versions, err := w.handlers.GetCommandHistory(name)
	if err != nil {
		w.showConfirm("History", "Could not load history: "+err.Error(), nil)
		return
	}
	if len(versions) == 0 {
		w.showConfirm("History", "This command has no recorded versions.", nil)
		return
	}

	spec := tv.DialogSpec{MinW: 80, MinH: 22, PreferredW: 92}
	if need := footerRowMinWidth(commandHistoryFooterLabels, tv.DefaultButtonGap); spec.MinW < need {
		spec.MinW = need
	}
	x, y, width, height := w.dialogRect(spec)

	var layer *tv.Layer
	closeFn := func() { w.desktop.RemoveLayer(layer) }
	dialog := newCloseableDialog("History: "+name, x, y, width, height, closeFn)

	listX, listY := 2, 2
	buttonY := height - 3
	hintY := height - 4
	paneH := height - listY - 5
	if paneH < 4 {
		paneH = 4
	}
	listW := 26
	detailX := listX + listW + 2
	detailW := width - detailX - 2
	if detailW < 24 {
		detailW = 24
	}

	dialog.Window.AddContent(dialogLabel("Versions", tv.Rect{X: listX, Y: 1, W: listW, H: 1}))
	dialog.Window.AddContent(dialogLabel("Diff (base → target)", tv.Rect{X: detailX, Y: 1, W: detailW, H: 1}))

	list := tv.NewTree(tv.Rect{X: listX, Y: listY, W: listW, H: paneH})
	list.FG = tv.DefaultTheme.ListFG
	list.BG = tv.DefaultTheme.ListBG
	list.SelFG, list.SelBG = selectionColorsFor(
		tv.DefaultTheme.ListFG, tv.DefaultTheme.ListBG,
		tv.DefaultTheme.SelectionFG, tv.DefaultTheme.SelectionBG)
	dialog.Window.AddContent(list)

	diff := tv.NewTextView("", tv.Rect{X: detailX, Y: listY, W: detailW, H: paneH})
	diff.Wrap = false
	diff.FG = tv.DefaultTheme.DialogFG
	diff.BG = tv.DefaultTheme.DialogBG
	dialog.Window.AddContent(diff)

	status := dialogLabel("", tv.Rect{X: 2, Y: hintY, W: width - 4, H: 1})
	dialog.Window.AddContent(status)

	latest := len(versions) - 1
	// baseIdx defaults to the previous version (so the default view is "what
	// changed in the selected version"); targetIdx tracks the list selection.
	baseIdx := latest - 1
	if baseIdx < 0 {
		baseIdx = 0
	}
	targetIdx := latest

	// Versions render newest first; map a list row back to its versions index.
	rowToIndex := func(row int) int { return latest - row }

	renderDiff := func() {
		base := versions[baseIdx]
		target := versions[targetIdx]
		var b strings.Builder
		fmt.Fprintf(&b, "base v%d  →  target v%d\n\n", base.Version, target.Version)
		b.WriteString("Template:\n")
		b.WriteString(lineDiff(base.Template, target.Template))
		b.WriteString("\nParameters:\n")
		b.WriteString(paramDiff(base.Parameters, target.Parameters))
		if base.Model != target.Model {
			fmt.Fprintf(&b, "\n- model: %q\n+ model: %q\n", base.Model, target.Model)
		}
		if base.Agent != target.Agent {
			fmt.Fprintf(&b, "- agent: %q\n+ agent: %q\n", base.Agent, target.Agent)
		}
		if base.Subtask != target.Subtask {
			fmt.Fprintf(&b, "- subtask: %v\n+ subtask: %v\n", base.Subtask, target.Subtask)
		}
		diff.SetText(b.String())
		diff.ScrollToTop()
		status.SetText(fmt.Sprintf("Base v%d → Target v%d · Set Base pins the comparison's left side", base.Version, target.Version))
		w.desktop.Redraw()
	}

	nodes := make([]*tv.TreeNode, 0, len(versions))
	for row := 0; row < len(versions); row++ {
		v := versions[rowToIndex(row)]
		label := fmt.Sprintf("v%d", v.Version)
		if rowToIndex(row) == latest {
			label += " (latest)"
		}
		if v.SavedAt != "" {
			label += " — " + v.SavedAt
		}
		n := tv.NewTreeNode(label)
		n.Data = rowToIndex(row)
		nodes = append(nodes, n)
	}
	list.Roots = nodes
	list.OnSelect = func(n *tv.TreeNode) {
		if n == nil {
			return
		}
		if idx, ok := n.Data.(int); ok {
			targetIdx = idx
			renderDiff()
		}
	}

	setBase := func() {
		if n := list.Selected(); n != nil {
			if idx, ok := n.Data.(int); ok {
				baseIdx = idx
				renderDiff()
			}
		}
	}
	restore := func() {
		if w.handlers.RestoreCommandVer == nil {
			status.SetText("Restore is unavailable.")
			w.desktop.Redraw()
			return
		}
		v := versions[targetIdx].Version
		if err := w.handlers.RestoreCommandVer(name, v); err != nil {
			status.SetText("Restore failed: " + err.Error())
			w.desktop.Redraw()
			return
		}
		closeFn()
		if onChange != nil {
			onChange()
		}
	}

	footer := footerButtonRects(commandHistoryFooterLabels, 2, width-3, buttonY, tv.DefaultButtonGap)
	for i, lbl := range commandHistoryFooterLabels {
		fn := []func(){setBase, restore, closeFn}[i]
		dialog.Window.AddContent(newButton(lbl, footer[i], fn))
	}

	dialog.Window.OnClose = func(*tv.Window) { closeFn() }
	dialog.Root().OnTypeFn = func(_ *tv.VisualComponent, event tui.TypeEvent) bool {
		if event.Key == tui.KeyEscape {
			closeFn()
			return true
		}
		return false
	}

	layer = tv.NewModalLayer("command-history-dialog", dialog)
	w.desktop.AddLayer(layer)
	dialog.Fit(spec)
	renderDiff()
	w.desktop.SetFocus(list)
}

// lineDiff renders a line-by-line diff of two texts as '-'/'+'/' ' prefixed
// lines using a longest-common-subsequence alignment, so unchanged lines are
// shown once and edits are localized. Identical inputs report "(no change)".
func lineDiff(a, b string) string {
	if a == b {
		return "  (no change)\n"
	}
	al := strings.Split(a, "\n")
	bl := strings.Split(b, "\n")
	// LCS table over lines.
	n, m := len(al), len(bl)
	lcs := make([][]int, n+1)
	for i := range lcs {
		lcs[i] = make([]int, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if al[i] == bl[j] {
				lcs[i][j] = lcs[i+1][j+1] + 1
			} else if lcs[i+1][j] >= lcs[i][j+1] {
				lcs[i][j] = lcs[i+1][j]
			} else {
				lcs[i][j] = lcs[i][j+1]
			}
		}
	}
	var out strings.Builder
	i, j := 0, 0
	for i < n && j < m {
		switch {
		case al[i] == bl[j]:
			fmt.Fprintf(&out, "  %s\n", al[i])
			i++
			j++
		case lcs[i+1][j] >= lcs[i][j+1]:
			fmt.Fprintf(&out, "- %s\n", al[i])
			i++
		default:
			fmt.Fprintf(&out, "+ %s\n", bl[j])
			j++
		}
	}
	for ; i < n; i++ {
		fmt.Fprintf(&out, "- %s\n", al[i])
	}
	for ; j < m; j++ {
		fmt.Fprintf(&out, "+ %s\n", bl[j])
	}
	return out.String()
}

// paramDiff renders a structural diff of two parameter lists: parameters only in
// base are '-', only in target are '+', and changed attributes (required/default/
// description) show both sides. Order follows the target then base remainder.
func paramDiff(base, target []CommandParam) string {
	if len(base) == 0 && len(target) == 0 {
		return "  (none)\n"
	}
	baseByName := map[string]CommandParam{}
	for _, p := range base {
		baseByName[p.Name] = p
	}
	targetByName := map[string]CommandParam{}
	for _, p := range target {
		targetByName[p.Name] = p
	}
	var out strings.Builder
	changed := false
	for _, p := range target {
		if bp, ok := baseByName[p.Name]; !ok {
			fmt.Fprintf(&out, "+ %s\n", formatParamRow(p))
			changed = true
		} else if bp != p {
			fmt.Fprintf(&out, "- %s\n+ %s\n", formatParamRow(bp), formatParamRow(p))
			changed = true
		}
	}
	for _, p := range base {
		if _, ok := targetByName[p.Name]; !ok {
			fmt.Fprintf(&out, "- %s\n", formatParamRow(p))
			changed = true
		}
	}
	if !changed {
		return "  (no change)\n"
	}
	return out.String()
}
