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
// built-in tool, an MCP server (or one of its tools), or a skill. The detail
// field carries the fully-rendered side-pane text so the widget code never
// inspects kinds.
type resourceItem struct {
	kind      resourceKind
	name      string
	desc      string
	detail    string
	enabled   bool
	canToggle bool
	usage     string
	// group marks an MCP server-header row (as opposed to one of its tool rows).
	// It is only set on resourceMCP items and governs how the row is labelled and
	// how the MCP-aware filter preserves server grouping.
	group bool
}

// showResourcesDialog opens the unified Resources browser: a single explorer for
// everything the agent can use. Three tabs — Tools, MCP and Skills — share a
// filterable list and a detail pane; Enter toggles a tool or skill on and off.
//
// The Tools tab is generated from the ToolRegistry (so tool docs stop being
// hardcoded in the UI); the Skills tab generalizes the former skills dialog and
// adds the full SKILL.md preview; the MCP tab lists the connected MCP servers and
// the tools each one advertises (via tools/list), derived from the registered
// mcp__<server>__<tool> tools, with the same description + input-schema detail as
// the Tools tab.
func (w *Workbench) showResourcesDialog() {
	// Large by default (≈85% of the terminal), floored so it stays usable on a
	// small terminal; the list/detail split is derived from width below (#299).
	x, y, width, height := w.dialogRect(w.browserDialogSpec())

	dialog := tv.NewDialog("Resources", x, y, width, height)
	applyWindowShadow(dialog.Window) // honour the NoShadow theme setting (issue #215)
	dialog.Window.ShowClose = false

	listX := 2
	headerY := 3
	listY := 4
	paneH := height - listY - 4 // hint/button row + bottom margin + border
	if paneH < 3 {
		paneH = 3
	}
	// The list only needs room for the checkbox + name + a short usage tail; cap
	// it so the wider, line-wrapping detail pane gets the surplus width.
	listW := width/2 - 2
	if listW > 34 {
		listW = 34
	}
	if listW < 18 {
		listW = 18
	}
	detailX := listX + listW + 1
	detailW := width - detailX - 2
	if detailW < 24 {
		detailW = 24
	}

	// Category tabs + search box.
	dialog.Window.AddContent(dialogLabel("Category:", tv.Rect{X: 2, Y: 1, W: 9, H: 1}))
	catSel := newSelect(w.desktop, []string{"Tools", "MCP", "Skills"}, tv.Rect{X: 12, Y: 1, W: 14, H: 1})
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
	list.FG = tv.DefaultTheme.ListFG
	list.BG = tv.DefaultTheme.ListBG
	// The stock default theme paints the selection with the same colours as the
	// list background, so the focused row is invisible; fall back to an
	// inverted list bar in that case (themes with an already-distinct selection
	// pass through unchanged).
	list.SelFG, list.SelBG = selectionColorsFor(
		tv.DefaultTheme.ListFG, tv.DefaultTheme.ListBG,
		tv.DefaultTheme.SelectionFG, tv.DefaultTheme.SelectionBG)
	dialog.Window.AddContent(list)

	detail := tv.NewTextView("", tv.Rect{X: detailX, Y: listY, W: detailW, H: paneH})
	detail.Wrap = true
	detail.FG = tv.DefaultTheme.DialogFG
	detail.BG = tv.DefaultTheme.DialogBG
	dialog.Window.AddContent(detail)

	// Selecting (arrows/click) only moves focus and updates the detail pane;
	// toggling on/off is a distinct Space/Enter gesture, so the hint says so.
	dialog.Window.AddContent(dialogLabel("↑↓ select · Space/Enter toggle · Esc close",
		tv.Rect{X: 2, Y: height - 3, W: width - 16, H: 1}))

	var layer *tv.Layer
	closeFn := func() { w.desktop.RemoveLayer(layer) }
	dialog.Window.AddContent(newButton("Close",
		tv.Rect{X: width - 11, Y: height - 3, W: 9, H: 1}, closeFn))

	// --- browser state -----------------------------------------------------

	var (
		curKind   = resourceTools
		allTools  = loadToolItems(w.handlers.GetTools)
		allSkills = loadSkillItems(w.handlers.GetSkills)
		allMCP    = loadMCPItems(w.handlers.GetTools)
	)

	currentItems := func() []resourceItem {
		switch curKind {
		case resourceTools:
			return allTools
		case resourceMCP:
			return allMCP
		case resourceSkills:
			return allSkills
		default:
			return nil
		}
	}

	// render rebuilds the list from the current tab + search query and points the
	// detail pane at the (clamped) selection. Redraw is synchronous, so reading
	// Selected() right after it reflects the post-clamp highlight.
	render := func() {
		// MCP rows are a two-tier server/tool list; a group-aware filter keeps a
		// server header attached to its matching tools (and supports searching by
		// server name) instead of orphaning rows the way the flat filter would.
		var items []resourceItem
		if curKind == resourceMCP {
			items = filterMCPItems(allMCP, searchBox.GetText())
		} else {
			items = filterResources(currentItems(), searchBox.GetText())
		}
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
		// Re-anchor at the top whenever the detail pane is repopulated so a
		// re-selection always shows the start (issue #174).
		detail.ScrollToTop()
		w.desktop.Redraw()
	}

	list.OnSelect = func(n *tv.TreeNode) {
		if n == nil {
			return
		}
		if it, ok := n.Data.(resourceItem); ok {
			detail.SetText(it.detail)
			detail.ScrollToTop()
			w.desktop.Redraw()
		}
	}

	// toggleSelected flips the selected item's on/off state via the backend, then
	// reloads and re-renders. It no-ops for a non-togglable item or empty list, so
	// it is safe to bind to both Space and Enter.
	toggleSelected := func() {
		n := list.Selected()
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
		allMCP = loadMCPItems(w.handlers.GetTools)
		render()
	}

	// Selecting (arrows/click) only moves focus; toggling on/off is a distinct,
	// explicit Space/Enter gesture. The tree fires OnActivate on Enter and on a
	// repeat click of the already-selected row, so leaving OnActivate nil makes a
	// click select without toggling, while Space/Enter are intercepted to toggle.
	bindToggleKeys(list, toggleSelected)
	catSel.OnChange = func(idx int) {
		curKind = resourceKind(idx)
		allTools = loadToolItems(w.handlers.GetTools)
		allSkills = loadSkillItems(w.handlers.GetSkills)
		allMCP = loadMCPItems(w.handlers.GetTools)
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
	// PreferredW is a share of the terminal, so re-resolve against the live
	// terminal on resize rather than the stale spec dialog.Fit would remember
	// (issue #299).
	installResizeReflow(w.desktop, dialog, layer, w.browserDialogSpec)
	render()
	w.desktop.SetFocus(list)
}

// selectionColorsFor returns the list's selection colours given the dialog's and
// the theme's selection colours. turbotui's stock default theme sets SelectionBG
// and SelectionFG to the same values as DialogBG and DialogFG, so a selected row
// would render invisibly; when the two collide we invert the dialog colours for a
// guaranteed-contrast highlight bar. Themes whose selection already differs from
// the dialog — the high-contrast accent, or the no-colour default — pass through
// unchanged.
func selectionColorsFor(dialogFG, dialogBG, selFG, selBG tui.Color) (tui.Color, tui.Color) {
	if selFG == dialogFG && selBG == dialogBG {
		return dialogBG, dialogFG
	}
	return selFG, selBG
}

// bindToggleKeys makes Space and Enter on the list invoke toggle (the Resources
// browser's explicit on/off action), while delegating every other key — arrow
// navigation, and Escape so it can bubble up to close the dialog — to the tree's
// own handler. Splitting it out keeps the key binding independent of the dialog's
// closures so it can be exercised directly.
func bindToggleKeys(list *tv.Tree, toggle func()) {
	treeType := list.Component.OnTypeFn
	list.Component.OnTypeFn = func(_ *tv.VisualComponent, event tui.TypeEvent) bool {
		if event.Key == tui.KeyEnter || (event.Key == tui.KeyRune && event.Rune == ' ') {
			toggle()
			return true
		}
		return treeType(list.Component, event)
	}
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
			detail:    skillDetail(s.Name, s.Description, s.Path, s.Active, s.Success, s.Failure, s.TotalCalls, s.Content),
			enabled:   s.Active,
			canToggle: true,
			usage:     skillUsage(s.Success, s.Failure),
		})
	}
	sortResourceItems(out)
	return out
}

// mcpToolPrefix is the namespace gogent gives every registered MCP tool:
// mcp__<server>__<tool> (see internal/gogent/newMCPTool). It is the contract the
// MCP tab parses to recover which server a tool came from, since the registered
// tool list is the only MCP-origin signal the UI receives.
const mcpToolPrefix = "mcp__"

// loadMCPItems derives the MCP tab from the registered tool list: it selects the
// mcp__<server>__<tool> tools, groups them by server, and emits a two-tier list —
// one server-header row followed by that server's tool rows, servers and tools
// each sorted by name. A server appears only when it advertised at least one tool
// (i.e. it connected and tools/list returned results); a denied, unreachable or
// zero-tool server is not represented here. A nil getter yields no items.
//
// The result is built already grouped and sorted, so it must NOT be passed
// through sortResourceItems (that would reorder headers away from their tools).
func loadMCPItems(get func() []ToolInfo) []resourceItem {
	if get == nil {
		return nil
	}
	// Preserve first-seen tool order per server before the per-server sort below;
	// the server iteration order is taken from a sorted key list for determinism.
	type mcpTool struct{ name, desc, schema string }
	byServer := map[string][]mcpTool{}
	var servers []string
	for _, t := range get() {
		server, tool, ok := splitMCPToolName(t.Name)
		if !ok {
			continue
		}
		if _, seen := byServer[server]; !seen {
			servers = append(servers, server)
		}
		byServer[server] = append(byServer[server], mcpTool{name: tool, desc: t.Description, schema: t.InputSchema})
	}
	sort.Strings(servers)

	var out []resourceItem
	for _, server := range servers {
		tools := byServer[server]
		sort.Slice(tools, func(i, j int) bool { return tools[i].name < tools[j].name })
		names := make([]string, len(tools))
		for i, mt := range tools {
			names[i] = mt.name
		}
		out = append(out, resourceItem{
			kind:      resourceMCP,
			name:      server,
			group:     true,
			canToggle: false,
			usage:     pluralTools(len(tools)),
			detail:    mcpServerDetail(server, names),
		})
		for _, mt := range tools {
			out = append(out, resourceItem{
				kind:      resourceMCP,
				name:      mt.name,
				desc:      cleanMCPDescription(mt.desc),
				canToggle: false,
				detail:    mcpToolDetail(server, mt.name, mt.desc, mt.schema),
			})
		}
	}
	return out
}

// splitMCPToolName parses a registered tool name of the form
// mcp__<server>__<tool> into its server and tool parts. The tool part may itself
// contain "__" (so the split is on the first separator after the prefix); a name
// without the prefix or without a second "__" is not an MCP tool and returns
// ok=false.
func splitMCPToolName(name string) (server, tool string, ok bool) {
	rest, found := strings.CutPrefix(name, mcpToolPrefix)
	if !found {
		return "", "", false
	}
	server, tool, found = strings.Cut(rest, "__")
	if !found || server == "" || tool == "" {
		return "", "", false
	}
	return server, tool, true
}

// pluralTools renders the server-header tool count ("1 tool" / "3 tools").
func pluralTools(n int) string {
	if n == 1 {
		return "1 tool"
	}
	return fmt.Sprintf("%d tools", n)
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

// filterMCPItems filters the two-tier MCP list while preserving server grouping.
// items is the grouped, ordered output of loadMCPItems (a server header followed
// by its tool rows). An empty query matches everything. Otherwise a whole server
// group is kept when the server name matches the query; for other groups, the
// tool rows that match (name or description) are kept together with their server
// header, so a matching tool is never shown orphaned from its server. A group
// that contributes neither a server-name match nor any tool match is dropped
// entirely (header included).
func filterMCPItems(items []resourceItem, query string) []resourceItem {
	q := strings.ToLower(strings.TrimSpace(query))
	if q == "" {
		return items
	}
	out := make([]resourceItem, 0, len(items))
	i := 0
	for i < len(items) {
		header := items[i]
		// Collect this server's tool rows (everything up to the next header).
		j := i + 1
		for j < len(items) && !items[j].group {
			j++
		}
		tools := items[i+1 : j]

		serverMatch := strings.Contains(strings.ToLower(header.name), q)
		matched := make([]resourceItem, 0, len(tools))
		for _, t := range tools {
			if serverMatch ||
				strings.Contains(strings.ToLower(t.name), q) ||
				strings.Contains(strings.ToLower(t.desc), q) {
				matched = append(matched, t)
			}
		}
		if serverMatch || len(matched) > 0 {
			out = append(out, header)
			out = append(out, matched...)
		}
		i = j
	}
	return out
}

// resourceListLabel renders one list row: a leading on/off checkbox for
// togglable items (so enabled/disabled is obvious at a glance), the padded
// name, and an optional usage tail. Toggling itself is a separate Space/Enter
// gesture, so a click on the row only selects it.
func resourceListLabel(it resourceItem) string {
	const nameW = 22
	if it.kind == resourceMCP {
		return mcpListLabel(it)
	}
	box := "   " // non-togglable: a blank slot keeps names aligned with checkbox rows
	if it.canToggle {
		if it.enabled {
			box = "[x]"
		} else {
			box = "[ ]"
		}
	}
	label := box + " " + padName(it.name, nameW)
	if it.usage != "" {
		label += " " + it.usage
	}
	return label
}

// mcpListLabel renders an MCP row as a two-tier list: a server-header row shows
// the server name and its tool-count tail as a section heading, while a tool row
// is indented under it and shows only the bare tool name. Keeping the server out
// of the tool row leaves the full name-column budget for the tool name (which
// would otherwise be truncated away under a long server prefix like
// "chrome-devtools-mcp/").
func mcpListLabel(it resourceItem) string {
	if it.group {
		label := it.name
		if it.usage != "" {
			label += "  (" + it.usage + ")"
		}
		return label
	}
	return "  " + it.name
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

// skillDetail renders the side-pane text for a skill: header, state, usage, the
// on-disk path and the full SKILL.md preview.
func skillDetail(name, desc, path string, active bool, success, failure, total int, content string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Skill: %s\n", name)
	if active {
		b.WriteString("State: active\n")
	} else {
		b.WriteString("State: inactive\n")
	}
	fmt.Fprintf(&b, "Usage: %d ok / %d fail (%d total)\n", success, failure, total)
	if p := strings.TrimSpace(path); p != "" {
		fmt.Fprintf(&b, "File: %s\n", p)
	}
	b.WriteString("\nDescription\n")
	b.WriteString(strings.TrimSpace(desc))
	if c := strings.TrimSpace(content); c != "" {
		b.WriteString("\n\nSKILL.md\n")
		b.WriteString(c)
		b.WriteByte('\n')
	}
	return b.String()
}

// mcpServerDetail renders the side-pane text for an MCP server-header row: the
// server name, the tools it advertises and a note that they are registered under
// the mcp__<server>__ namespace and callable by the agent.
func mcpServerDetail(server string, tools []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "MCP server: %s\n", server)
	fmt.Fprintf(&b, "Tools: %d\n", len(tools))
	if len(tools) > 0 {
		b.WriteString("\nAdvertised tools\n")
		for _, t := range tools {
			fmt.Fprintf(&b, " - %s\n", t)
		}
	}
	fmt.Fprintf(&b, "\nThese tools are registered as %s%s__<tool> and are callable by the agent.\n",
		mcpToolPrefix, server)
	return b.String()
}

// mcpToolDetail renders the side-pane text for one MCP tool, mirroring
// toolDetail's layout: the tool name, its server, the agent-callable namespaced
// name, the (cleaned) description and the input schema.
func mcpToolDetail(server, tool, desc, schema string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "MCP tool: %s\n", tool)
	fmt.Fprintf(&b, "Server: %s\n", server)
	fmt.Fprintf(&b, "Registered as: %s%s__%s\n", mcpToolPrefix, server, tool)
	b.WriteString("\nDescription\n")
	b.WriteString(cleanMCPDescription(desc))
	if s := strings.TrimSpace(schema); s != "" {
		b.WriteString("\n\nInput schema\n")
		b.WriteString(s)
		b.WriteByte('\n')
	}
	return b.String()
}

// mcpDescriptionTrailer is the model-targeted sentence newMCPTool appends to
// every MCP tool description (instructing the model to call the tool by its full
// namespaced name). It is noise in the human-facing detail pane — the namespaced
// name is surfaced explicitly as a "Registered as:" line instead — so
// cleanMCPDescription trims it.
const mcpDescriptionTrailer = "\n\nWhen calling this tool, use its full name"

// cleanMCPDescription strips the model-targeted "use its full name" trailer
// newMCPTool appends, returning the server's own tool description trimmed of
// surrounding whitespace. It is a no-op (beyond trimming) when the trailer is
// absent, so it never drops real description text.
func cleanMCPDescription(desc string) string {
	if i := strings.Index(desc, mcpDescriptionTrailer); i >= 0 {
		desc = desc[:i]
	}
	return strings.TrimSpace(desc)
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

// mcpPlaceholder explains the MCP tab's empty state: no Model Context Protocol
// server is currently connected. A server appears here once it is configured
// (the mcp_servers config key), its launch is permitted, and it advertises at
// least one tool via tools/list.
func mcpPlaceholder() string {
	return "MCP servers\n\nNo MCP servers are configured or connected.\n\n" +
		"Add servers under the \"mcp_servers\" config key. Once a server is " +
		"configured, permitted to launch and advertises tools (via tools/list), " +
		"it appears here with the tools it exposes."
}
