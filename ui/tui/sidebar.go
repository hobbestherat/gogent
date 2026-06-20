package ui

import (
	"fmt"
	"strings"

	"gogent/internal/agent"
	"gogent/internal/config"
	"gogent/internal/stats"

	tui "github.com/hobbestherat/turbotui"
	tv "github.com/hobbestherat/turbotui/turbotv"
)

// defaultSidebarWidth is the number of columns reserved on the right of the
// desktop for the always-visible session / sub-agent tree when no width has been
// persisted. The live width is mutable (issue #175): it is stored on the
// Workbench and changed by dragging the sidebar's left-edge divider.
const defaultSidebarWidth = 32

// minSidebarWidth / minWorkAreaWidth bound the draggable sidebar width (issue
// #175): the sidebar may not shrink below minSidebarWidth (so the tree and the
// Overall stats band stay legible — cf. minSidebarTreeHeight) nor grow so wide
// that the work area left of it drops below minWorkAreaWidth.
const (
	minSidebarWidth  = 24
	minWorkAreaWidth = 40
)

// minSidebarTreeHeight is the smallest tree region worth showing. When the
// Overall stats band (and/or the TODO region) would leave the tree shorter than
// this, those regions are dropped so a very short terminal keeps the session
// list usable.
const minSidebarTreeHeight = 4

// maxTodoRegionItems caps how many checklist rows the middle TODO region renders
// for the focused session, so a long todo list cannot crowd out the session tree
// (the region drops entirely before the tree shrinks below minSidebarTreeHeight).
const maxTodoRegionItems = 8

// todoRegionTitleLines is the fixed overhead of the middle TODO region: a single
// "TODOs" header row above the per-session checklist rows.
const todoRegionTitleLines = 1

// approvalBadge marks a session that has a permission prompt waiting for the
// user (issue #55). It is appended to the requesting session's node label and,
// with a count, to the sidebar header as a global indicator.
const approvalBadge = "⏳"

// sidebar is the right-hand panel that shows every open session and, nested
// underneath each one, its sub-agents and their live status. Selecting a node
// brings the owning session's window to the front.
type sidebar struct {
	wb    *Workbench
	panel *tv.VisualComponent
	tree  *tv.Tree
	layer *tv.Layer

	sessions map[string]*tv.TreeNode // sessionID -> root node
	agents   map[string]*tv.TreeNode // agentID  -> sub-agent node
	// todos is the source of truth for each session's checklist (issue #43),
	// keyed by session id. It is no longer embedded in the session tree (issue
	// #190): the focused session's list is drawn in its own middle region by
	// drawTodos. applyTodo replaces a session's slice on every update.
	todos map[string][]agent.TodoItem
	// focused is the session whose checklist the middle TODO region shows. It
	// follows the raised session (set by focusSession from Workbench.Focus),
	// mirroring how the Overall band's model/api rows follow the focused session
	// (issue #107). Empty before any session is raised.
	focused string

	// approvals tracks which sessions currently have a permission prompt waiting,
	// so their node keeps the "needs approval" badge across unrelated relabels
	// (rename/pin). globalApprovals is the total number of in-flight prompts,
	// rendered as a header indicator. Both are read/written on the UI thread.
	approvals       map[string]bool
	globalApprovals int

	// overall is the bottom "Overall" aggregate-stats panel's current data (issue
	// #53), refreshed from the Statistics report (coalesced) on the UI thread.
	// overallBandH is the band height resolved by LayoutFn (0 when the sidebar is
	// too short to show the panel); DrawFn reads it to place the band.
	overall      overallStats
	overallBandH int
	// todosBandH is the middle TODO region's height resolved by LayoutFn (issue
	// #190): todoRegionTitleLines + the focused session's (capped) item count, or
	// 0 when the focused session has no todos or the sidebar is too short. DrawFn
	// reads it to place the region directly above the Overall band.
	todosBandH int

	// divider is a 1-column drag handle pinned to the sidebar's left edge (issue
	// #175). Dragging it left/right changes the live sidebar width; it sits in
	// front of the tree so it claims clicks landing on column 0.
	divider *tv.VisualComponent
}

// nodeRef identifies what a tree node points at: a session (agentID empty) or a
// sub-agent within a session.
type nodeRef struct {
	sessionID string
	agentID   string
	name      string
}

// newSidebar builds the pinned sidebar panel and its tree.
func newSidebar(wb *Workbench) *sidebar {
	s := &sidebar{
		wb:           wb,
		sessions:     make(map[string]*tv.TreeNode),
		agents:       make(map[string]*tv.TreeNode),
		todos:        make(map[string][]agent.TodoItem),
		approvals:    make(map[string]bool),
		overallBandH: overallBandHeight,
	}

	panel := tv.NewComponent(tv.Rect{})
	tree := tv.NewTree(tv.Rect{})
	panel.AddChild(tree.Root())
	panel.DrawFn = func(c *tv.VisualComponent, surface tv.Surface) {
		abs := c.AbsoluteBounds()
		surface.Fill(abs, tui.Cell{Ch: ' ', FG: chromePanelFG, BG: chromePanelBG})
		// Left divider + title.
		for y := 0; y < abs.H; y++ {
			surface.SetCell(abs.X, abs.Y+y, tui.Cell{Ch: '│', FG: chromeDivider, BG: chromePanelBG})
		}
		surface.WriteString(abs.X+2, abs.Y, "Sessions & Agents", tui.Cell{FG: chromeTitle, BG: chromePanelBG})
		// Global "needs approval" indicator: a bright badge + count, drawn at the
		// far right of the title row so a wide glyph cannot shift the title.
		if s.globalApprovals > 0 {
			ind := fmt.Sprintf("%s%d", approvalBadge, s.globalApprovals)
			x := abs.X + abs.W - len([]rune(ind)) - 1
			if x < abs.X+20 {
				x = abs.X + 20
			}
			surface.WriteString(x, abs.Y, ind, tui.Cell{FG: chromeAccent, BG: chromePanelBG})
		}
		// Middle per-session TODO region (issue #190), then the bottom "Overall"
		// aggregate-stats panel (issue #53). Both read heights resolved by LayoutFn.
		s.drawTodos(surface, abs)
		s.drawOverall(surface, abs)
	}
	panel.LayoutFn = func(c *tv.VisualComponent) {
		w := c.Bounds.W
		h := c.Bounds.H
		if w < 3 || h < 2 {
			return
		}
		// Reserve the bottom band for the Overall stats panel when there is room
		// for both it and a usable tree; otherwise drop the band so a very short
		// sidebar keeps the session list.
		bandH := overallBandHeight
		if h-1-bandH < minSidebarTreeHeight {
			bandH = 0
		}
		// Reserve the middle TODO region (issue #190) above the band, sized to the
		// focused session's checklist. It drops before the band does: a session
		// list that still has room for the band but not the todos keeps the band.
		// With an empty checklist todosH is 0, so the band split above is unchanged.
		todosH := s.todosRegionHeight()
		if h-1-bandH-todosH < minSidebarTreeHeight {
			todosH = 0
		}
		s.overallBandH = bandH
		s.todosBandH = todosH
		// Leave the first column for the divider and the first row for the title.
		tree.Root().SetBounds(tv.Rect{X: 2, Y: 1, W: w - 3, H: h - 1 - bandH - todosH})
		// The drag handle overlays the left-edge divider glyph (col 0, full height).
		if s.divider != nil {
			s.divider.SetBounds(tv.Rect{X: 0, Y: 0, W: 1, H: h})
		}
	}

	// Selecting (navigating to) a node raises the owning session. Activating a
	// sub-agent node (Enter, or a repeat click) opens its internal-monologue
	// popup; activating a session node just raises it.
	tree.OnSelect = func(n *tv.TreeNode) {
		if ref, ok := n.Data.(nodeRef); ok && ref.sessionID != "" {
			s.wb.Focus(ref.sessionID)
		}
	}
	tree.OnActivate = func(n *tv.TreeNode) {
		ref, ok := n.Data.(nodeRef)
		if !ok || ref.sessionID == "" {
			return
		}
		if ref.agentID != "" {
			s.wb.showAgentMonolog(ref.sessionID, ref.agentID, ref.name)
			return
		}
		s.wb.Focus(ref.sessionID)
	}

	// Draggable left-edge divider (issue #175), ported from turbotui's chat-demo
	// pattern. It is a 1-column child overlaying the panel's divider glyph; its
	// OnClickFn maps the drag X to a new width (screen-right-edge minus X), clamps
	// it and reflows. Added last so HitTestDeep (last child first) routes clicks on
	// column 0 here rather than to the tree. The desktop's mouse-capture keeps
	// delivering the move/release events to it during a drag even past its column.
	divider := tv.NewComponent(tv.Rect{})
	divider.DrawFn = func(c *tv.VisualComponent, surface tv.Surface) {
		abs := c.AbsoluteBounds()
		for y := 0; y < abs.H; y++ {
			surface.SetCell(abs.X, abs.Y+y, tui.Cell{Ch: '│', FG: chromeDivider, BG: chromePanelBG})
		}
	}
	divider.OnClickFn = func(c *tv.VisualComponent, event tui.ClickEvent) bool {
		if !event.Down {
			return true
		}
		s.wb.dragSidebarWidth(event.X)
		return true
	}
	panel.AddChild(divider)

	s.panel = panel
	s.tree = tree
	s.divider = divider
	s.layer = tv.NewWindowLayer("sidebar", panel)
	return s
}

// reposition pins the sidebar to the right edge of the desktop, using the
// workbench's live (persisted, draggable) sidebar width (issue #175).
func (s *sidebar) reposition(screenW, screenH int) {
	w := s.wb.sidebarWidth()
	if w > screenW {
		w = screenW
	}
	x := screenW - w
	s.panel.SetBounds(tv.Rect{X: x, Y: 1, W: w, H: screenH - 1})
}

// addSession registers a top-level session node.
func (s *sidebar) addSession(id, title string, pinned bool) {
	if _, ok := s.sessions[id]; ok {
		return
	}
	node := tv.NewTreeNode(sessionLabel(title, agent.StatusIdle, pinned, s.approvals[id]))
	node.Expanded = true
	node.Data = nodeRef{sessionID: id, name: title}
	s.sessions[id] = node
	s.tree.AddRoot(node)
}

// removeSession removes a session node and any of its sub-agent nodes.
func (s *sidebar) removeSession(id string) {
	node := s.sessions[id]
	if node == nil {
		return
	}
	for _, child := range node.Children {
		if ref, ok := child.Data.(nodeRef); ok {
			delete(s.agents, ref.agentID)
		}
	}
	roots := s.tree.Roots[:0]
	for _, r := range s.tree.Roots {
		if r != node {
			roots = append(roots, r)
		}
	}
	s.tree.Roots = roots
	delete(s.sessions, id)
	delete(s.approvals, id)
	delete(s.todos, id)
	// Drop the middle TODO region's focus when its session goes away, so it does
	// not point at a removed session (the next Focus re-sets it).
	if s.focused == id {
		s.focused = ""
	}
}

// focusSession records which session's checklist the middle TODO region shows
// (issue #190). It mirrors the Overall band's "follows the focused session"
// behaviour (issue #107) so both bottom regions describe one session. Called
// from Workbench.Focus on the UI thread.
func (s *sidebar) focusSession(id string) {
	s.focused = id
}

// setApproval toggles the "needs approval" badge on a session node (issue #55)
// and keeps the pending set in sync so a later relabel preserves it. It is a
// no-op for unknown sessions but still records intent so a node added later (out
// of order) picks the badge up.
func (s *sidebar) setApproval(id, title string, pinned, pending bool) {
	if pending {
		s.approvals[id] = true
	} else {
		delete(s.approvals, id)
	}
	if node := s.sessions[id]; node != nil {
		node.Label = sessionLabel(title, agent.StatusIdle, pinned, pending)
	}
}

// setGlobalApprovals updates the header indicator's count of in-flight prompts.
func (s *sidebar) setGlobalApprovals(n int) {
	if n < 0 {
		n = 0
	}
	s.globalApprovals = n
}

// refreshOverallStats rebuilds the Overall panel's data from a Statistics report
// joined with the sidebar's own session / sub-agent node counts and the focused
// session's active model config (issue #107's model / endpoint rows). It only
// updates the stored struct; the caller owns the redraw (mirrors SessionWindow's
// refreshStatus contract). Runs on the UI thread.
func (s *sidebar) refreshOverallStats(report stats.Report, model *config.ModelConfig) {
	s.overall = buildOverallStats(report, len(s.sessions), len(s.agents), model)
}

// todoLineCount is the number of checklist rows the middle region renders for
// the focused session: its item count, capped at maxTodoRegionItems. It is 0
// when there is no focused session or the focused session has no todos, which is
// what hides the region entirely (no blank block).
func (s *sidebar) todoLineCount() int {
	n := len(s.todos[s.focused])
	if n > maxTodoRegionItems {
		n = maxTodoRegionItems
	}
	return n
}

// todosRegionHeight is the middle TODO region's desired height: the title row
// plus the (capped) checklist rows, or 0 when the focused session has no todos.
// LayoutFn reserves this much above the Overall band, dropping it first when the
// sidebar is too short for a usable tree.
func (s *sidebar) todosRegionHeight() int {
	n := s.todoLineCount()
	if n == 0 {
		return 0
	}
	return todoRegionTitleLines + n
}

// drawTodos renders the middle per-session TODO region (issue #190): a "TODOs"
// header row over a separator, then the focused session's checklist, placed
// directly above the Overall band. It is a no-op when LayoutFn resolved the
// region height to 0 (no focused todos, or the sidebar is too short). Each row
// is clipped to the content width so a long task can never run into the divider
// or the panel edge. Runs on the UI thread.
func (s *sidebar) drawTodos(surface tv.Surface, abs tv.Rect) {
	todosH := s.todosBandH
	if todosH <= 0 || todosH >= abs.H {
		return
	}
	contentW := abs.W - 3 // leave the divider (col 0) and a right margin
	if contentW < 1 {
		return
	}
	top := abs.Y + abs.H - s.overallBandH - todosH
	// Header row: a separator under the tree with the "TODOs" title over it, so
	// the region reads as a labelled section in a single line.
	for x := 1; x < abs.W-1; x++ {
		surface.SetCell(abs.X+x, top, tui.Cell{Ch: '─', FG: chromeDivider, BG: chromePanelBG})
	}
	surface.WriteString(abs.X+2, top, "TODOs", tui.Cell{FG: chromeTitle, BG: chromePanelBG})
	items := s.todos[s.focused]
	for i := 0; i < todosH-todoRegionTitleLines && i < len(items); i++ {
		surface.WriteString(abs.X+2, top+todoRegionTitleLines+i,
			truncateRunes(todoLabel(items[i]), contentW), tui.Cell{FG: chromePanelFG, BG: chromePanelBG})
	}
}

// drawOverall renders the bottom aggregate-stats band: a separator under the
// session tree, the "Overall" title and the formatted metric rows. It is a no-op
// when LayoutFn resolved the band height to 0 (sidebar too short). Each metric
// row is clipped to the content width so a wide value can never run into the
// divider or the panel edge. Runs on the UI thread.
func (s *sidebar) drawOverall(surface tv.Surface, abs tv.Rect) {
	bandH := s.overallBandH
	if bandH <= 0 || bandH >= abs.H {
		return
	}
	contentW := abs.W - 3 // leave the divider (col 0) and a right margin
	if contentW < 1 {
		return
	}
	top := abs.Y + abs.H - bandH
	// Separator under the session tree.
	for x := 1; x < abs.W-1; x++ {
		surface.SetCell(abs.X+x, top, tui.Cell{Ch: '─', FG: chromeDivider, BG: chromePanelBG})
	}
	// Title.
	surface.WriteString(abs.X+2, top+1, "Overall", tui.Cell{FG: chromeTitle, BG: chromePanelBG})
	// Metric rows. A non-zero error count is highlighted red so it stands out.
	for i, line := range formatOverallStats(s.overall) {
		fg := chromePanelFG
		if s.overall.Errors > 0 && i == overallErrLineIdx {
			fg = colorError
		}
		surface.WriteString(abs.X+2, top+2+i, truncateRunes(line, contentW),
			tui.Cell{FG: fg, BG: chromePanelBG})
	}
}

// applySubAgent inserts or updates a sub-agent node from a lifecycle event.
func (s *sidebar) applySubAgent(sessionID string, ev agent.SessionEvent) {
	parent := s.sessions[sessionID]
	if parent == nil {
		return
	}
	key := ev.AgentID
	if key == "" {
		key = sessionID + "/" + ev.Name
	}
	node := s.agents[key]
	if node == nil {
		node = tv.NewTreeNode("")
		node.Data = nodeRef{sessionID: sessionID, agentID: ev.AgentID, name: ev.Name}
		s.agents[key] = node
		parent.Add(node)
	} else if ref, ok := node.Data.(nodeRef); ok {
		// Keep the name in sync (it may have been empty on first sight).
		ref.name = ev.Name
		node.Data = ref
	}
	node.Label = agentLabel(ev.Name, ev.Status, ev.Kind)
}

// applyTodo records a session's checklist from a todo update (issue #43). Since
// issue #190 the todos are no longer embedded as session-tree children: this
// only updates s.todos (the source of truth), and the focused session's list is
// drawn in the middle region by drawTodos. An empty list clears the entry.
// Unknown sessions are ignored. Runs on the UI thread (called from
// EmitSessionEvent).
func (s *sidebar) applyTodo(sessionID string, items []agent.TodoItem) {
	if s.sessions[sessionID] == nil {
		return
	}
	if len(items) == 0 {
		delete(s.todos, sessionID)
		return
	}
	cp := make([]agent.TodoItem, len(items))
	copy(cp, items)
	s.todos[sessionID] = cp
}

// todoLabel renders one checklist row: a status glyph followed by the content.
func todoLabel(it agent.TodoItem) string {
	content := strings.TrimSpace(it.Content)
	if content == "" {
		content = "(empty)"
	}
	return fmt.Sprintf("%s %s", todoStatusIcon(it.Status), content)
}

// todoStatusIcon maps a todo status to a compact glyph for the middle TODO
// region. The glyphs are deliberately distinct from statusIcon's sub-agent set
// (▶ ‖ ✓ ✗ •) so a checklist row is unambiguous at a glance even though the two
// kinds now live in separate regions (issue #190): pending ☐, in-progress ◐,
// completed ☑.
func todoStatusIcon(status agent.TodoStatus) string {
	switch status {
	case agent.TodoInProgress:
		return "◐"
	case agent.TodoCompleted:
		return "☑"
	default:
		return "☐"
	}
}

// relabelSession updates a session node's title (rename) and pin marker. It is
// a no-op for unknown sessions.
func (s *sidebar) relabelSession(id, title string, pinned bool) {
	node := s.sessions[id]
	if node == nil {
		return
	}
	if ref, ok := node.Data.(nodeRef); ok {
		ref.name = title
		node.Data = ref
	}
	node.Label = sessionLabel(title, agent.StatusIdle, pinned, s.approvals[id])
}

// reorder reorders the tree's roots to match order. Sessions absent from order
// keep their relative positions at the tail; unknown ids in order are skipped.
func (s *sidebar) reorder(order []string) {
	if len(order) == 0 {
		return
	}
	roots := make([]*tv.TreeNode, 0, len(s.tree.Roots))
	seen := make(map[string]bool, len(order))
	for _, id := range order {
		node := s.sessions[id]
		if node == nil || seen[id] {
			continue
		}
		roots = append(roots, node)
		seen[id] = true
	}
	for _, node := range s.tree.Roots {
		if ref, ok := node.Data.(nodeRef); ok && !seen[ref.sessionID] {
			roots = append(roots, node)
		}
	}
	s.tree.Roots = roots
}

// sessionLabel renders a top-level session row. A pinned (favorite) session is
// prefixed with a ★ marker so favorites are visible at a glance; a session with
// a permission prompt waiting gets a trailing ⏳ badge (issue #55), appended last
// so a wide glyph cannot shift the status icon or title columns.
func sessionLabel(title string, status agent.AgentStatus, pinned, pending bool) string {
	var label string
	if pinned {
		label = fmt.Sprintf("%s %s %s", statusIcon(status), "★", title)
	} else {
		label = fmt.Sprintf("%s %s", statusIcon(status), title)
	}
	if pending {
		label += " " + approvalBadge
	}
	return label
}

// agentLabel renders a sub-agent row with a status icon and mode marker.
func agentLabel(name string, status agent.AgentStatus, kind agent.SubAgentKind) string {
	mark := ""
	switch kind {
	case agent.KindInteractive:
		mark = " (i)"
	case agent.KindTool:
		mark = " (1)"
	}
	if name == "" {
		name = "sub-agent"
	}
	return fmt.Sprintf("%s %s%s", statusIcon(status), name, mark)
}

// statusIcon maps a lifecycle status to a compact glyph for the tree.
func statusIcon(status agent.AgentStatus) string {
	switch status {
	case agent.StatusRunning:
		return "▶"
	case agent.StatusWaiting:
		return "‖"
	case agent.StatusCompleted:
		return "✓"
	case agent.StatusFailed:
		return "✗"
	default:
		return "•"
	}
}
