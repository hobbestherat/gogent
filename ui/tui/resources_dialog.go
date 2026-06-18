package ui

import (
	"fmt"
	"sort"
	"strings"

	tui "github.com/hobbestherat/turbotui"
	tv "github.com/hobbestherat/turbotui/turbotv"
)

// resourceKind enumerates the Resources browser tabs. The order matches the
// category Select options (Tools, MCP, Skills) so the selected index maps
// directly to a kind.
type resourceKind int

const (
	resourceTools resourceKind = iota
	resourceMCP
	resourceSkills
)

// resourceItem is the browser's uniform view of one browsable resource: a
// built-in tool, an (eventual) MCP server, or a skill. The detail field carries
// the fully-rendered side-pane text so the widget code never inspects kinds.
type resourceItem struct {
	kind      resourceKind
	name      string
	desc      string
	detail    string
	enabled   bool
	canToggle bool
	usage     string
}

// showResourcesDialog opens the unified Resources browser: a single explorer for
// everything the agent can use. Three tabs — Tools, MCP and Skills — share a
// filterable list and a detail pane; Enter toggles a tool or skill on and off.
//
// The Tools tab is generated from the ToolRegistry (so tool docs stop being
// hardcoded in the UI); the Skills tab generalizes the former skills dialog and
// adds the full SKILL.md preview; the MCP tab is a placeholder until MCP client
// support lands (#36), at which point it will list configured servers and their
// discovered tools.
func (w *Workbench) showResourcesDialog() {
	width, height := resourcesDialogSize(w.app.Width(), w.app.Height())
	x, y := centeredDialog(w, width, height)

	dialog := tv.NewDialog("Resources", x, y, width, height)
	dialog.Window.ShowClose = false

	listX := 2
	headerY := 3
	listY := 4
	paneH := height - listY - 4 // hint/button row + bottom margin + border
	if paneH < 3 {
		paneH = 3
	}
	listW := width/2 - 2
	if listW < 18 {
		listW = 18
	}
	detailX := listX + listW + 1
	detailW := width - detailX - 2
	if detailW < 18 {
		detailW = 18
	}

	// Category tabs + search box.
	dialog.Window.AddContent(dialogLabel("Category:", tv.Rect{X: 2, Y: 1, W: 9, H: 1}))
	catSel := tv.NewSelect(w.desktop, []string{"Tools", "MCP", "Skills"}, tv.Rect{X: 12, Y: 1, W: 14, H: 1})
	dialog.Window.AddContent(catSel)
	searchLabelX := 30
	searchBoxX := 38
	searchBoxW := width - searchBoxX - 2
	if searchBoxW < 8 {
		searchBoxW = 8
		searchBoxX = width - searchBoxW - 2
		searchLabelX = searchBoxX - 8
	}
	dialog.Window.AddContent(dialogLabel("Search:", tv.Rect{X: searchLabelX, Y: 1, W: 7, H: 1}))
	searchBox := tv.NewTextBox("", tv.Rect{X: searchBoxX, Y: 1, W: searchBoxW, H: 1})
	dialog.Window.AddContent(searchBox)

	dialog.Window.AddContent(dialogLabel("Items", tv.Rect{X: listX, Y: headerY, W: listW, H: 1}))
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

	dialog.Window.AddContent(dialogLabel("Tab move · Enter toggle · Esc close",
		tv.Rect{X: 2, Y: height - 3, W: width - 16, H: 1}))

	var layer *tv.Layer
	closeFn := func() { w.desktop.RemoveLayer(layer) }
	dialog.Window.AddContent(tv.NewButton("Close",
		tv.Rect{X: width - 11, Y: height - 3, W: 9, H: 1}, closeFn))

	// --- browser state -----------------------------------------------------

	var (
		curKind   = resourceTools
		allTools  = loadToolItems(w.handlers.GetTools)
		allSkills = loadSkillItems(w.handlers.GetSkills)
	)

	currentItems := func() []resourceItem {
		switch curKind {
		case resourceTools:
			return allTools
		case resourceSkills:
			return allSkills
		default:
			return nil // MCP: no servers until #36
		}
	}

	// render rebuilds the list from the current tab + search query and points the
	// detail pane at the (clamped) selection. Redraw is synchronous, so reading
	// Selected() right after it reflects the post-clamp highlight.
	render := func() {
		items := filterResources(currentItems(), searchBox.GetText())
		nodes := make([]*tv.TreeNode, 0, len(items))
		for i := range items {
			n := tv.NewTreeNode(resourceListLabel(items[i]))
			n.Data = items[i]
			nodes = append(nodes, n)
		}
		list.Roots = nodes
		w.desktop.Redraw()
		if n := list.Selected(); n != nil {
			if it, ok := n.Data.(resourceItem); ok {
				detail.SetText(it.detail)
			}
		} else {
			detail.SetText(emptyDetail(curKind, len(items), strings.TrimSpace(searchBox.GetText())))
		}
		w.desktop.Redraw()
	}

	list.OnSelect = func(n *tv.TreeNode) {
		if n == nil {
			return
		}
		if it, ok := n.Data.(resourceItem); ok {
			detail.SetText(it.detail)
			w.desktop.Redraw()
		}
	}
	list.OnActivate = func(n *tv.TreeNode) {
		if n == nil {
			return
		}
		it, ok := n.Data.(resourceItem)
		if !ok || !it.canToggle {
			return
		}
		switch it.kind {
		case resourceTools:
			if w.handlers.SetToolEnabled != nil {
				w.handlers.SetToolEnabled(it.name, !it.enabled)
			}
		case resourceSkills:
			if w.handlers.SetSkillActive != nil {
				w.handlers.SetSkillActive(it.name, !it.enabled)
			}
		}
		allTools = loadToolItems(w.handlers.GetTools)
		allSkills = loadSkillItems(w.handlers.GetSkills)
		render()
	}
	catSel.OnChange = func(idx int) {
		curKind = resourceKind(idx)
		allTools = loadToolItems(w.handlers.GetTools)
		allSkills = loadSkillItems(w.handlers.GetSkills)
		render()
	}
	searchBox.OnSubmit = func() { render() }

	dialog.Root().OnTypeFn = func(_ *tv.VisualComponent, event tui.TypeEvent) bool {
		if event.Key == tui.KeyEscape {
			closeFn()
			return true
		}
		return false
	}

	layer = tv.NewModalLayer("resources-dialog", dialog)
	w.desktop.AddLayer(layer)
	render()
	w.desktop.SetFocus(list)
}

// resourcesDialogSize picks a browser size that fills a good chunk of a large
// terminal while still fitting (and staying useful) on a small one.
func resourcesDialogSize(screenW, screenH int) (width, height int) {
	width, height = 78, 22
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

// loadToolItems maps the backend's tool list into browser items, sorted by name
// for a stable display. A nil getter yields no items.
func loadToolItems(get func() []ToolInfo) []resourceItem {
	if get == nil {
		return nil
	}
	infos := get()
	out := make([]resourceItem, 0, len(infos))
	for _, t := range infos {
		out = append(out, resourceItem{
			kind:      resourceTools,
			name:      t.Name,
			desc:      t.Description,
			detail:    toolDetail(t.Name, t.Description, t.InputSchema, t.Enabled, t.Invocations, t.LastUsed),
			enabled:   t.Enabled,
			canToggle: true,
			usage:     toolUsage(t.Invocations),
		})
	}
	sortResourceItems(out)
	return out
}

// loadSkillItems maps the backend's skill list into browser items, sorted by
// name. A nil getter yields no items.
func loadSkillItems(get func() []SkillInfo) []resourceItem {
	if get == nil {
		return nil
	}
	infos := get()
	out := make([]resourceItem, 0, len(infos))
	for _, s := range infos {
		out = append(out, resourceItem{
			kind:      resourceSkills,
			name:      s.Name,
			desc:      s.Description,
			detail:    skillDetail(s.Name, s.Description, s.Active, s.Success, s.Failure, s.TotalCalls, s.Content),
			enabled:   s.Active,
			canToggle: true,
			usage:     skillUsage(s.Success, s.Failure),
		})
	}
	sortResourceItems(out)
	return out
}

// sortResourceItems orders items by kind then name so each tab is alphabetical.
func sortResourceItems(items []resourceItem) {
	sort.Slice(items, func(i, j int) bool {
		if items[i].kind != items[j].kind {
			return items[i].kind < items[j].kind
		}
		return items[i].name < items[j].name
	})
}

// filterResources returns the items whose name or description contain the query
// (case-insensitive). An empty query matches everything.
func filterResources(items []resourceItem, query string) []resourceItem {
	q := strings.ToLower(strings.TrimSpace(query))
	out := make([]resourceItem, 0, len(items))
	for _, it := range items {
		if q == "" ||
			strings.Contains(strings.ToLower(it.name), q) ||
			strings.Contains(strings.ToLower(it.desc), q) {
			out = append(out, it)
		}
	}
	return out
}

// resourceListLabel renders one row of the list: a padded name, an optional
// usage tail, and an "(off)" marker when a togglable item is disabled.
func resourceListLabel(it resourceItem) string {
	const nameW = 22
	label := padName(it.name, nameW)
	if it.usage != "" {
		label += " " + it.usage
	}
	if it.canToggle && !it.enabled {
		label += "  (off)"
	}
	return label
}

// padName truncates or right-pads name with spaces to width columns (by rune).
func padName(name string, width int) string {
	r := []rune(truncateRunes(name, width))
	if len(r) >= width {
		return string(r)
	}
	return string(r) + strings.Repeat(" ", width-len(r))
}

func toolUsage(invocations int) string {
	if invocations <= 0 {
		return ""
	}
	return fmt.Sprintf("used:%d", invocations)
}

func skillUsage(success, failure int) string {
	return fmt.Sprintf("ok:%d fail:%d", success, failure)
}

// toolDetail renders the side-pane text for a tool: header, state, usage,
// description and the input schema.
func toolDetail(name, desc, schema string, enabled bool, invocations int, lastUsed string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Tool: %s\n", name)
	if enabled {
		b.WriteString("State: enabled\n")
	} else {
		b.WriteString("State: disabled\n")
	}
	if lastUsed != "" {
		fmt.Fprintf(&b, "Used: %d (last %s)\n", invocations, lastUsed)
	} else {
		fmt.Fprintf(&b, "Used: %d\n", invocations)
	}
	b.WriteString("\nDescription\n")
	b.WriteString(strings.TrimSpace(desc))
	if s := strings.TrimSpace(schema); s != "" {
		b.WriteString("\n\nInput schema\n")
		b.WriteString(s)
		b.WriteByte('\n')
	}
	return b.String()
}

// skillDetail renders the side-pane text for a skill, including the full
// SKILL.md preview.
func skillDetail(name, desc string, active bool, success, failure, total int, content string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Skill: %s\n", name)
	if active {
		b.WriteString("State: active\n")
	} else {
		b.WriteString("State: inactive\n")
	}
	fmt.Fprintf(&b, "Usage: %d ok / %d fail (%d total)\n", success, failure, total)
	b.WriteString("\nDescription\n")
	b.WriteString(strings.TrimSpace(desc))
	if c := strings.TrimSpace(content); c != "" {
		b.WriteString("\n\nSKILL.md\n")
		b.WriteString(c)
		b.WriteByte('\n')
	}
	return b.String()
}

// emptyDetail is the placeholder shown when a tab has no visible items.
func emptyDetail(kind resourceKind, count int, query string) string {
	if count == 0 && strings.TrimSpace(query) == "" {
		switch kind {
		case resourceMCP:
			return mcpPlaceholder()
		case resourceTools:
			return "No tools are registered."
		case resourceSkills:
			return "No skills are loaded.\nAdd SKILL.md folders under ~/.gogent/skills or ./skills."
		}
	}
	return "No matching items."
}

// mcpPlaceholder explains the MCP tab's empty state and points at the issue that
// will fill it in.
func mcpPlaceholder() string {
	return "MCP servers\n\nNo MCP servers are configured yet.\n\n" +
		"Model Context Protocol client support lands with issue #36; once it " +
		"does, configured servers, their connection status and the tools each " +
		"one exposes (via tools/list) will appear here."
}
