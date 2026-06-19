package ui

import (
	"fmt"

	"gogent/internal/agent"

	tui "github.com/hobbestherat/turbotui"
	tv "github.com/hobbestherat/turbotui/turbotv"
)

// sidebarWidth is the number of columns reserved on the right of the desktop for
// the always-visible session / sub-agent tree.
const sidebarWidth = 32

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

	// approvals tracks which sessions currently have a permission prompt waiting,
	// so their node keeps the "needs approval" badge across unrelated relabels
	// (rename/pin). globalApprovals is the total number of in-flight prompts,
	// rendered as a header indicator. Both are read/written on the UI thread.
	approvals       map[string]bool
	globalApprovals int
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
		wb:        wb,
		sessions:  make(map[string]*tv.TreeNode),
		agents:    make(map[string]*tv.TreeNode),
		approvals: make(map[string]bool),
	}

	panel := tv.NewComponent(tv.Rect{})
	tree := tv.NewTree(tv.Rect{})
	panel.AddChild(tree.Root())
	panel.DrawFn = func(c *tv.VisualComponent, surface tv.Surface) {
		abs := c.AbsoluteBounds()
		surface.Fill(abs, tui.Cell{Ch: ' ', FG: tui.ANSIColor(7), BG: tui.ANSIColor(0)})
		// Left divider + title.
		for y := 0; y < abs.H; y++ {
			surface.SetCell(abs.X, abs.Y+y, tui.Cell{Ch: '│', FG: tui.ANSIColor(8), BG: tui.ANSIColor(0)})
		}
		surface.WriteString(abs.X+2, abs.Y, "Sessions & Agents", tui.Cell{FG: tui.ANSIColor(15), BG: tui.ANSIColor(0)})
		// Global "needs approval" indicator: a bright badge + count, drawn at the
		// far right of the title row so a wide glyph cannot shift the title.
		if s.globalApprovals > 0 {
			ind := fmt.Sprintf("%s%d", approvalBadge, s.globalApprovals)
			x := abs.X + abs.W - len([]rune(ind)) - 1
			if x < abs.X+20 {
				x = abs.X + 20
			}
			surface.WriteString(x, abs.Y, ind, tui.Cell{FG: tui.ANSIColor(11), BG: tui.ANSIColor(0)})
		}
	}
	panel.LayoutFn = func(c *tv.VisualComponent) {
		w := c.Bounds.W
		h := c.Bounds.H
		if w < 3 || h < 2 {
			return
		}
		// Leave the first column for the divider and the first row for the title.
		tree.Root().SetBounds(tv.Rect{X: 2, Y: 1, W: w - 3, H: h - 1})
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

	s.panel = panel
	s.tree = tree
	s.layer = tv.NewWindowLayer("sidebar", panel)
	return s
}

// reposition pins the sidebar to the right edge of the desktop.
func (s *sidebar) reposition(screenW, screenH int) {
	w := sidebarWidth
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
