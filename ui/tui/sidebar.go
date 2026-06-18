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

	// approvals records which sessions have a permission prompt awaiting a
	// decision (issue #55); pending is the total across all sessions, shown as a
	// global indicator in the panel title. Both are read while rendering session
	// labels so a badged session stays badged across renames/pins.
	approvals map[string]bool
	pending   int
}

// nodeRef identifies what a tree node points at: a session (agentID empty) or a
// sub-agent within a session. For session nodes, pinned mirrors the workbench's
// favorite flag so the label can be re-rendered (e.g. on an approval badge
// change) without re-reading workbench state.
type nodeRef struct {
	sessionID string
	agentID   string
	name      string
	pinned    bool
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
		title := "Sessions & Agents"
		titleFG := tui.ANSIColor(15)
		if s.pending > 0 {
			// Global "approval needed" indicator: count + an attention colour.
			title = fmt.Sprintf("⏳ %d  Sessions & Agents", s.pending)
			titleFG = tui.ANSIColor(11)
		}
		surface.WriteString(abs.X+2, abs.Y, title, tui.Cell{FG: titleFG, BG: tui.ANSIColor(0)})
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
	ref := nodeRef{sessionID: id, name: title, pinned: pinned}
	node := tv.NewTreeNode(s.sessionNodeLabel(ref))
	node.Expanded = true
	node.Data = ref
	s.sessions[id] = node
	s.tree.AddRoot(node)
}

// sessionNodeLabel renders a session row from its ref, folding in the live
// approval-pending state so the ⏳ badge survives renames and pin toggles.
func (s *sidebar) sessionNodeLabel(ref nodeRef) string {
	return sessionLabel(ref.name, ref.pinned, s.approvals[ref.sessionID])
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
	ref, _ := node.Data.(nodeRef)
	ref.name = title
	ref.pinned = pinned
	node.Data = ref
	node.Label = s.sessionNodeLabel(ref)
}

// setApprovalPending sets whether a session has a permission prompt awaiting a
// decision and re-renders its label. It returns true if the state changed (so
// the caller can avoid a redundant redraw).
func (s *sidebar) setApprovalPending(id string, pending bool) bool {
	node := s.sessions[id]
	if node == nil || s.approvals[id] == pending {
		return false
	}
	if pending {
		s.approvals[id] = true
	} else {
		delete(s.approvals, id)
	}
	if ref, ok := node.Data.(nodeRef); ok {
		node.Label = s.sessionNodeLabel(ref)
	}
	return true
}

// setGlobalApprovals updates the total pending-approval count shown in the panel
// title. It returns true if the count changed.
func (s *sidebar) setGlobalApprovals(n int) bool {
	if s.pending == n {
		return false
	}
	s.pending = n
	return true
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

// sessionLabel renders a top-level session row. A session with a permission
// prompt awaiting a decision is prefixed with an ⏳ "approval needed" badge
// (issue #55); a pinned (favorite) session carries a ★ marker so favorites are
// visible at a glance.
func sessionLabel(title string, pinned, approvalPending bool) string {
	badge := ""
	if approvalPending {
		badge = "⏳ "
	}
	if pinned {
		return fmt.Sprintf("%s%s %s %s", badge, statusIcon(agent.StatusIdle), "★", title)
	}
	return fmt.Sprintf("%s%s %s", badge, statusIcon(agent.StatusIdle), title)
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
