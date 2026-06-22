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
// user (issue #55). It is appended to the requesting session's node label only;
// there is no global header counter (issue #230 removed the phantom aggregate).
const approvalBadge = "⏳"

// clarifyBadge marks a session whose interactive sub-agent is blocked waiting
// for the user (a CLARIFY question, issue #207). It is the clarify counterpart
// of approvalBadge: a distinct glyph appended to the owning session's node label.
// Like approvalBadge it has no global header counter (issue #230); it marks only
// the owning row.
const clarifyBadge = "❓"

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
	// so their node keeps the "needs approval" ⏳ badge across unrelated relabels
	// (rename/pin). Read/written on the UI thread. There is deliberately no global
	// header counter for it (issue #230): a single aggregate digit could outlive or
	// mis-count the per-session badges, so attention is shown only per row.
	approvals map[string]bool

	// busy tracks which sessions currently have a turn in flight (issue #236), so
	// their node shows the active marker (● vs the idle ○) and keeps it across
	// unrelated relabels (rename/pin/approval/clarify). It mirrors SessionWindow.busy
	// and is reconciled on the UI thread by syncBusy from Workbench.tickBusyStatuses.
	busy map[string]bool

	// clarify tracks which sessions currently have at least one sub-agent blocked
	// waiting for the user (a CLARIFY question, issue #207), so their node keeps the
	// "needs input" badge across unrelated relabels (rename/pin). A session can host
	// several interactive sub-agents, so clarifyCount is a balanced reference count
	// (setClarify does +1 on enter-waiting, -1 on resolve) and clarify[id] is the
	// derived "count > 0" membership the row/header read: the badge persists until
	// the LAST waiting sub-agent resolves, not the first. The count equals the number
	// of waiting sub-agents only because EmitSessionEvent collapses the raw event
	// stream into one balanced transition per sub-agent (keyed by SessionEvent.AgentID)
	// before calling setClarify — see clarifyWaiting on Workbench. The raw stream is
	// itself unbalanced (a multi-round CLARIFY re-emits StatusWaiting with no resume
	// event between rounds), so feeding it to setClarify directly would drift; that
	// dedup is the caller's contract, not setClarify's. All are read/written on the UI
	// thread, mirroring the approvals plumbing (issue #55). There is no global header
	// counter (issue #230); the badge marks only the owning row.
	clarify      map[string]bool
	clarifyCount map[string]int

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
	// front of the tree so it claims clicks landing on column 0. Its DrawFn paints
	// a ↔ grip on the header row to advertise that it is grabbable (issue #314).
	divider *tv.VisualComponent
	// dividerActive is set while a divider drag is in progress (issue #314): the
	// divider's OnClickFn sets it on press/drag and clears it on release, and the
	// divider's DrawFn brightens the column while it is true so the user sees the
	// handle respond to the grab. Touched only on the UI thread.
	dividerActive bool
	// pinToggle is the clickable pin/unpin glyph in the sidebar header (issue
	// #314): ▣ when the sidebar boundary is pinned, □ when unpinned (the codebase's
	// filled=active convention, as on the window maximize button). Clicking it
	// calls Workbench.ToggleSidebarPin — the same path as View → Pin/Unpin Sidebar.
	// It is added LAST among the panel's children so HitTestDeep (last child first)
	// routes clicks on its header cell to it rather than to the tree behind it.
	pinToggle *tv.VisualComponent

	// overallSelect is the model-selector dropdown at the top of the Overall band
	// (issue #191): it scopes every metric below it to one model. Its options are
	// [overallAllModelsOption] + the same model display names offered in each session
	// window's model row. overallModelKeys is the parallel slice of model config
	// names (the keys the Statistics report is keyed on), with "" for the "all models"
	// aggregate option, so the selected label maps back to a report key. Both run on
	// the UI thread.
	overallSelect    *tv.Select
	overallModelKeys []string
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
		busy:         make(map[string]bool),
		clarify:      make(map[string]bool),
		clarifyCount: make(map[string]int),
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
		// No global "needs attention" counter on the title row (issue #230). A single
		// right-aligned aggregate digit could outlive or mis-count the per-session
		// badges (a closed session, a headless requester, or a font-blank glyph all
		// left a lone phantom number with no matching row). Attention is now shown only
		// per row: the ⏳/❓ badges in sessionLabel and the ○/● idle/active marker
		// (issue #236) live on the session nodes themselves, where they always point at
		// a real session.
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
		// Split the panel below the title row (h-1) into the tree, the middle TODO
		// region and the bottom Overall band, with a strict precedence so the tree
		// always wins: drop the per-session TODO region first, then the Overall
		// band, whenever a region would push the tree below minSidebarTreeHeight.
		// Dropping todos before the band keeps the persistent global summary as the
		// last region standing and makes the drop monotonic — shrinking the sidebar
		// never makes the todos reappear after the band is gone. With an empty
		// checklist todosH is 0, so this reduces to the original band-only split.
		treeAvail := h - 1
		bandH := overallBandHeight
		todosH := s.todosRegionHeight()
		if treeAvail-bandH-todosH < minSidebarTreeHeight {
			todosH = 0
		}
		if treeAvail-bandH-todosH < minSidebarTreeHeight {
			bandH = 0
		}
		s.overallBandH = bandH
		s.todosBandH = todosH
		// Leave the first column for the divider and the first row for the title.
		tree.Root().SetBounds(tv.Rect{X: 2, Y: 1, W: w - 3, H: treeAvail - bandH - todosH})
		// The drag handle overlays the left-edge divider glyph (col 0, full height).
		if s.divider != nil {
			s.divider.SetBounds(tv.Rect{X: 0, Y: 0, W: 1, H: h})
		}
		// The pin/unpin glyph sits one cell in from the right edge of the header row
		// (issue #314), clear of the title text and the divider column.
		if s.pinToggle != nil {
			s.pinToggle.SetBounds(tv.Rect{X: w - 2, Y: 0, W: 1, H: 1})
		}
		// Model selector just below the band's top separator (issues #191, #233), or
		// hidden when the band was dropped (sidebar too short). The band occupies the
		// bottom bandH rows; its top row holds the divider, so the selector sits one row
		// down at panel-relative Y = h-bandH+overallSeparatorLines.
		if s.overallSelect != nil {
			if bandH > 0 {
				s.overallSelect.Root().Visible = true
				s.overallSelect.Root().SetBounds(tv.Rect{X: 2, Y: h - bandH + overallSeparatorLines, W: w - 3, H: 1})
			} else {
				s.overallSelect.Root().Visible = false
			}
		}
	}

	// Selecting (navigating to) a node raises the owning session. Activating a
	// sub-agent node (Enter, or a repeat click) opens its internal-monologue
	// popup; activating a session node just raises it.
	tree.OnSelect = func(n *tv.TreeNode) {
		ref, ok := n.Data.(nodeRef)
		if !ok {
			return
		}
		// Only raise the window for a SESSION node. For a sub-agent node, Focus would
		// snap the tree selection back to the parent session row (issue #302),
		// blocking keyboard navigation onto the sub-agent and the Enter/OnActivate
		// path — so leave the selection where the user put it instead.
		if ref.agentID == "" && ref.sessionID != "" {
			s.wb.Focus(ref.sessionID)
		}
	}
	// A single MOUSE click on a sub-agent row opens its monologue (issue #302).
	// OnSelectMouse fires only on a pointer click, not on keyboard traversal, so
	// arrowing through the tree never pops a window; showAgentMonolog replaces any
	// open popup. Keyboard users reach it via Enter (OnActivate) below.
	tree.OnSelectMouse = func(n *tv.TreeNode) {
		if ref, ok := n.Data.(nodeRef); ok && ref.agentID != "" && ref.sessionID != "" {
			s.wb.showAgentMonolog(ref.sessionID, ref.agentID, ref.name)
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
		// The column body stays the plain border glyph; while a drag is in progress
		// the whole handle brightens to the accent colour (bold) so the grab reads as
		// "live" (issue #314).
		barFG := chromeDivider
		if s.dividerActive {
			barFG = chromeAccent
		}
		for y := 0; y < abs.H; y++ {
			surface.SetCell(abs.X, abs.Y+y, tui.Cell{Ch: '│', FG: barFG, BG: chromePanelBG, Bold: s.dividerActive})
		}
		// Grip marker on the header row advertises that the divider is draggable
		// (issue #314): ↔ (U+2194) is single-cell, widely supported and reads as
		// "drag horizontally". Drawn in the accent colour over the top of the column.
		if abs.H > 0 {
			surface.SetCell(abs.X, abs.Y, tui.Cell{Ch: '↔', FG: chromeAccent, BG: chromePanelBG, Bold: true})
		}
	}
	divider.OnClickFn = func(c *tv.VisualComponent, event tui.ClickEvent) bool {
		// Track the drag so the handle can highlight (issue #314): press and every
		// drag-motion report carry Down=true, the release carries Down=false.
		s.dividerActive = event.Down
		if !event.Down {
			return true
		}
		s.wb.dragSidebarWidth(event.X)
		return true
	}
	panel.AddChild(divider)

	// Model selector near the top of the Overall band, just below its divider (issues
	// #191, #233). It is a real, focusable/clickable component (its popup is
	// desktop-owned so it is never clipped by the narrow panel), positioned by LayoutFn.
	// Picking a model scopes the metrics below and persists the choice in the layout
	// store (consistent with the sidebar width, issue #175).
	overallSelect := newSelect(wb.desktop, []string{overallAllModelsOption}, tv.Rect{})
	overallSelect.OnChange = func(int) {
		s.wb.persistLayout()
		s.wb.scheduleOverallRefresh()
	}
	panel.AddChild(overallSelect.Root())

	// Clickable pin/unpin glyph at the right edge of the header row (issue #314).
	// It mirrors the divider pattern: a 1-cell child with its own DrawFn and
	// OnClickFn. The glyph reflects the live state each frame (▣ pinned / □
	// unpinned, the codebase's filled=active convention), and a click drives the
	// same ToggleSidebarPin path as the View menu. Added LAST so HitTestDeep (last
	// child first) routes clicks on its cell here rather than to the tree.
	pinToggle := tv.NewComponent(tv.Rect{})
	pinToggle.DrawFn = func(c *tv.VisualComponent, surface tv.Surface) {
		abs := c.AbsoluteBounds()
		if abs.W < 1 || abs.H < 1 {
			return
		}
		glyph := '□'
		if s.wb.IsSidebarPinned() {
			glyph = '▣'
		}
		surface.SetCell(abs.X, abs.Y, tui.Cell{Ch: glyph, FG: chromeAccent, BG: chromePanelBG, Bold: true})
	}
	pinToggle.OnClickFn = func(c *tv.VisualComponent, event tui.ClickEvent) bool {
		// Toggle once on the fresh press only: Down stays true through any drag
		// motion the terminal reports, so gate on a non-drag press so a click that
		// jitters cannot double-toggle. ToggleSidebarPin rebuilds the menu and
		// redraws, so the glyph repaints to its new state.
		if event.Down && !event.Drag {
			s.wb.ToggleSidebarPin()
		}
		return true
	}
	panel.AddChild(pinToggle)

	s.panel = panel
	s.tree = tree
	s.divider = divider
	s.pinToggle = pinToggle
	s.overallSelect = overallSelect
	s.overallModelKeys = []string{""}
	s.layer = tv.NewWindowLayer("sidebar", panel)
	s.rebuildModelOptions()
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
	node := tv.NewTreeNode(sessionLabel(title, s.busy[id], pinned, s.approvals[id], s.clarify[id]))
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
			// Prune the Workbench's clarify dedup entry for this sub-agent: one still
			// in StatusWaiting at close emits no terminal event, so without this its
			// key would linger (issue #207). The key is reconstructed exactly as
			// EmitSessionEvent derives it (agent id, else session/name).
			if s.wb != nil && s.wb.clarifyWaiting != nil {
				key := ref.agentID
				if key == "" {
					key = id + "/" + ref.name
				}
				delete(s.wb.clarifyWaiting, key)
			}
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
	delete(s.busy, id)
	delete(s.clarify, id)
	delete(s.clarifyCount, id)
	// No global header counter to resync on close (issue #230): the per-session
	// ⏳/❓ badges and the ○/● marker are dropped with the node above, so closing a
	// session that was waiting for input or mid-turn can no longer strand a phantom
	// aggregate digit with no matching row.
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
	// Move the tree highlight bar to the active session too (issue #206). The
	// highlight is the Tree's own selection index, which only its key/mouse
	// handlers used to touch — so before SelectNode (turbotui) any focus change
	// driven from outside the tree (new session, Ctrl+] cycle, close, Session
	// menu) left the bar stranded on the previous row. focusSession is the funnel
	// every active-session change passes through (via refreshOverall), so setting
	// it here keeps the bar in sync on all of those paths.
	if node := s.sessions[id]; node != nil {
		s.tree.SelectNode(node)
	}
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
		node.Label = sessionLabel(title, s.busy[id], pinned, pending, s.clarify[id])
	}
}

// setClarify adjusts the "needs input" badge on a session node (issue #207). A
// session may host several interactive sub-agents, so this is a reference count,
// not a toggle: waiting=true records one more waiting sub-agent and waiting=false
// records one resolving. The badge (and the clarify[id] membership a relabel
// reads) is shown while the count is positive and cleared only when the last
// waiting sub-agent resolves. The caller (EmitSessionEvent) collapses repeated
// same-state events per sub-agent so the count stays balanced. The node's live
// approval badge is preserved; it is a no-op on the node for unknown sessions but
// still records intent so a node added later (out of order) picks the badge up.
func (s *sidebar) setClarify(id, title string, pinned, waiting bool) {
	if waiting {
		s.clarifyCount[id]++
	} else if s.clarifyCount[id] > 0 {
		s.clarifyCount[id]--
	}
	if s.clarifyCount[id] > 0 {
		s.clarify[id] = true
	} else {
		delete(s.clarify, id)
		delete(s.clarifyCount, id)
	}
	if node := s.sessions[id]; node != nil {
		node.Label = sessionLabel(title, s.busy[id], pinned, s.approvals[id], s.clarify[id])
	}
}

// setBusy toggles the idle/active marker (○/●, issue #236) on a session node and
// records the busy state so a later relabel (rename/pin/approval/clarify) keeps the
// marker correct. It mirrors setApproval/setClarify: a no-op on the node for an
// unknown session, but the intent is still recorded so a node added later (out of
// order) picks up the marker. Runs on the UI thread.
func (s *sidebar) setBusy(id, title string, pinned, busy bool) {
	if busy {
		s.busy[id] = true
	} else {
		delete(s.busy, id)
	}
	if node := s.sessions[id]; node != nil {
		node.Label = sessionLabel(title, busy, pinned, s.approvals[id], s.clarify[id])
	}
}

// syncBusy reconciles every session's idle/active marker against busyIDs (the set
// of session ids with a turn in flight) and reports whether any marker moved. Only
// nodes whose state actually changed are relabeled — the common all-idle tick
// touches nothing. The current pin state is read from the workbench so a marker
// flip never drops the ★. It is the funnel Workbench.tickBusyStatuses drives once
// per tick; the returned bool lets the caller redraw exactly on a transition,
// including the busy→idle edge that empties the set (which the tick's own
// "anything busy?" check would otherwise skip). Runs on the UI thread.
func (s *sidebar) syncBusy(busyIDs map[string]bool) bool {
	changed := false
	for id, node := range s.sessions {
		want := busyIDs[id]
		if want == s.busy[id] {
			continue
		}
		title := id
		if ref, ok := node.Data.(nodeRef); ok {
			title = ref.name
		}
		s.setBusy(id, title, s.wb.IsPinned(id), want)
		changed = true
	}
	return changed
}

// refreshOverallStats rebuilds the Overall panel's data from a Statistics report
// joined with the sidebar's own session / sub-agent node counts and the focused
// session's active model config (issue #107's model / endpoint rows). It only
// updates the stored struct; the caller owns the redraw (mirrors SessionWindow's
// refreshStatus contract). Runs on the UI thread.
func (s *sidebar) refreshOverallStats(report stats.Report, model *config.ModelConfig, selectedModel string) {
	s.overall = buildOverallStats(report, len(s.sessions), len(s.agents), model, selectedModel)
}

// rebuildModelOptions resyncs the Overall band's model-selector options with the
// workbench's current model list (issue #191): the aggregate "all models" entry
// followed by each model's display name, with overallModelKeys carrying the parallel
// config names ("" for the aggregate). The current selection is preserved by config
// name when possible, so refreshing the list (e.g. after editing models) does not
// silently re-scope the panel. Runs on the UI thread.
func (s *sidebar) rebuildModelOptions() {
	if s.overallSelect == nil {
		return
	}
	prev := s.selectedOverallModel()
	options := []string{overallAllModelsOption}
	keys := []string{""}
	for _, m := range s.wb.models {
		display := m.DisplayName
		if display == "" {
			display = m.Name
		}
		options = append(options, display)
		keys = append(keys, m.Name)
	}
	s.overallSelect.Options = options
	s.overallModelKeys = keys
	s.setSelectedOverallModel(prev)
}

// selectedOverallModel returns the config name of the model the Overall band is
// scoped to, or "" for the aggregate "all models" view. Runs on the UI thread.
func (s *sidebar) selectedOverallModel() string {
	if s.overallSelect == nil {
		return ""
	}
	i := s.overallSelect.GetSelected()
	if i < 0 || i >= len(s.overallModelKeys) {
		return ""
	}
	return s.overallModelKeys[i]
}

// setSelectedOverallModel selects the option whose model config name is key, or the
// aggregate "all models" option when key is empty or no longer present (a renamed/
// removed model gracefully falls back to the aggregate). Runs on the UI thread.
func (s *sidebar) setSelectedOverallModel(key string) {
	if s.overallSelect == nil {
		return
	}
	for i, k := range s.overallModelKeys {
		if k == key {
			s.overallSelect.SetSelected(i)
			return
		}
	}
	s.overallSelect.SetSelected(0)
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

// drawOverall renders the bottom aggregate-stats band, top to bottom: a thin
// separator dividing it from the content above (issue #233), the model-selector
// row (drawn by the framework over its reserved row), the "Overall" title and the
// formatted metric rows. It is a no-op when LayoutFn resolved the band height to 0
// (sidebar too short). Each metric row is clipped to the content width so a wide
// value can never run into the divider or the panel edge. Runs on the UI thread.
func (s *sidebar) drawOverall(surface tv.Surface, abs tv.Rect) {
	bandH := s.overallBandH
	if bandH <= 0 || bandH >= abs.H {
		return
	}
	contentW := abs.W - 3 // leave the divider (col 0) and a right margin
	if contentW < 1 {
		return
	}
	// Band layout, top to bottom (issues #191, #233): a thin divider on the band's
	// top row, then the model selector (a real component drawn over its row by the
	// framework), then the title and metric rows.
	bandTop := abs.Y + abs.H - bandH
	// Thin horizontal rule dividing the Overall band from the content above it
	// (issue #233), in the theme divider colour.
	for x := 1; x < abs.W-1; x++ {
		surface.SetCell(abs.X+x, bandTop, tui.Cell{Ch: '─', FG: chromeDivider, BG: chromePanelBG})
	}
	// Title sits below the divider and the selector row.
	titleY := bandTop + overallSeparatorLines + overallSelectorLines
	surface.WriteString(abs.X+2, titleY, "Overall", tui.Cell{FG: chromeTitle, BG: chromePanelBG})
	// Metric rows. A non-zero error count is highlighted red so it stands out.
	for i, line := range formatOverallStats(s.overall) {
		fg := chromePanelFG
		if s.overall.Errors > 0 && i == overallErrLineIdx {
			fg = colorError
		}
		surface.WriteString(abs.X+2, titleY+1+i, truncateRunes(line, contentW),
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

// todoLabel renders one checklist row: a status glyph, the content, and an
// optional note in parentheses (issue #263), e.g. "☐ Read main.go (found the bug
// on line 42)".
func todoLabel(it agent.TodoItem) string {
	content := strings.TrimSpace(it.Content)
	if content == "" {
		content = "(empty)"
	}
	label := fmt.Sprintf("%s %s", todoStatusIcon(it.Status), content)
	if note := strings.TrimSpace(it.Note); note != "" {
		label += fmt.Sprintf(" (%s)", note)
	}
	return label
}

// todoStatusIcon maps a todo status to a compact glyph for the middle TODO
// region. The glyphs are deliberately distinct from statusIcon's sub-agent set
// (▶ ‖ ✓ ✗ •) so a checklist row is unambiguous at a glance even though the two
// kinds now live in separate regions (issue #190): pending ☐, in-progress ◐,
// completed ✔. The completed glyph is the HEAVY check mark ✔ (U+2714), kept
// deliberately distinct from the sub-agent completed icon ✓ (U+2713) so the two
// never collide (issue #315).
func todoStatusIcon(status agent.TodoStatus) string {
	switch status {
	case agent.TodoInProgress:
		return "◐"
	case agent.TodoCompleted:
		return "✔"
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
	node.Label = sessionLabel(title, s.busy[id], pinned, s.approvals[id], s.clarify[id])
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

// sessionLabel renders a top-level session row. The leading glyph is the session's
// own idle/active marker (○/●, issue #236) — never a sub-agent lifecycle glyph. A
// pinned (favorite) session is prefixed with a ★ marker so favorites are visible at
// a glance; a session with a permission prompt waiting gets a trailing ⏳ badge
// (issue #55) and a session whose sub-agent is waiting for input gets a trailing ❓
// badge (issue #207). Both badges are appended last (clarify after pending) so a
// wide glyph cannot shift the status icon or title columns.
//
// Param order mirrors the rendered left-to-right layout: busy (leading ○/●), then
// pinned (★), then the trailing pending/clarify badges. NOTE this deliberately puts
// busy before pinned, which is the reverse of the setBusy/setApproval/setClarify
// setters — those take (id, title, pinned, <feature flag>). Keep that in mind when
// adding a call site so the two adjacent bools are not transposed.
func sessionLabel(title string, busy, pinned, pending, clarify bool) string {
	var label string
	if pinned {
		label = fmt.Sprintf("%s %s %s", sessionStatusIcon(busy), "★", title)
	} else {
		label = fmt.Sprintf("%s %s", sessionStatusIcon(busy), title)
	}
	if pending {
		label += " " + approvalBadge
	}
	if clarify {
		label += " " + clarifyBadge
	}
	return label
}

// sessionStatusIcon maps a session's busy flag to its leading row glyph (issue
// #236): ● when a turn is in flight (its own loop or a spawned sub-agent), ○ when
// idle. These are deliberately distinct from statusIcon's sub-agent lifecycle set
// (▶ ‖ ✓ ✗ •) so a session row — even one coordinating a working sub-agent — reads
// as a session, never as a sub-agent. The sub-agent triangle (▶) therefore never
// appears on a session row.
func sessionStatusIcon(busy bool) string {
	if busy {
		return "●"
	}
	return "○"
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
