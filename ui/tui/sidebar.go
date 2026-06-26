package ui

import (
	"fmt"
	"strings"
	"time"

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

// watcherGlyph is the shared badge for a scheduled watcher in the session tree
// (issue #329 Phase 4): ◷ (U+25F7, "clock with three o'clock"). It is a single
// cell and deliberately distinct from every other sidebar glyph set — the session
// idle/active markers (○/●), the sub-agent lifecycle icons (▶/‖/✓/✗/•) and the
// todo icons (☐/◐/✔) — so a watcher row reads as a watcher at a glance, whether it
// is a free-running top-level entry or an attached child node. Both placements use
// this same glyph; only the tree position (root vs nested) tells the two kinds
// apart, mirroring the dialog's Target column.
const watcherGlyph = "◷"

// clarifyBadge marks a session whose interactive sub-agent is blocked waiting
// for the user (a CLARIFY question, issue #207). It is the clarify counterpart
// of approvalBadge: a distinct glyph appended to the owning session's node label.
// Like approvalBadge it has no global header counter (issue #230); it marks only
// the owning row.
const clarifyBadge = "❓"

// subAgentFoldTTL is how long a successfully-completed sub-agent stays a normal
// visible child row before it is folded into the session's collapsed "finished"
// bucket (issue #484). It is measured from the moment the completion event is
// delivered to the sidebar (sidebar.now, set when StatusCompleted first arrives
// in applySubAgent). Failed sub-agents are never auto-folded; they stay visible
// until manually dismissed. The live value is sidebar.ttl, defaulted to this and
// overridable in tests for a fast, deterministic fold (no real 60s sleep).
const subAgentFoldTTL = 60 * time.Second

// sessionFold is the per-session UI-only bookkeeping for TTL folding of finished
// sub-agents (issue #484). It is created lazily (ensureFold) when a session gets
// its first sub-agent and lives entirely in the sidebar mirror — the shared agent
// tree (internal/agent) is never touched, so folding is a pure visibility concern
// that cannot affect ActiveSubAgentCount / ListAllAgents / slot counting.
//
// statusNode is the always-visible, always-first synthetic child rendering the
// bracketed per-state counts ("[▶2 ‖1 ✓5 ✗1]"). It is a leaf, so the tree paints
// a blank marker column — visually distinct from real agent rows (which lead with
// a single status glyph) and from the bucket (which leads with ▸/▾).
//
// bucketNode is the synthetic SECOND child collecting folded completed agents
// ("[✓ N]"). It is nil until the first agent folds in (a childless bucket would
// still render a stray "[✓ 0]" row, so it must be absent — not empty — while
// nothing is folded) and is detached again if it ever drops back to zero
// children. Moving an agent node under this (collapsed) node is what hides it,
// reusing the tree's existing collapse mechanic with no new widget API.
type sessionFold struct {
	statusNode *tv.TreeNode
	bucketNode *tv.TreeNode
	entries    map[string]*foldEntry // agent key -> fold metadata (same key applySubAgent derives)
}

// foldEntry is the per-sub-agent fold metadata mirrored UI-side. The agent's tree
// node itself is reached via sidebar.agents[key]; this holds only the state the
// fold logic needs.
type foldEntry struct {
	status     agent.AgentStatus
	finishedAt time.Time // when status first became StatusCompleted (TTL clock start); zero otherwise
	folded     bool      // moved under bucketNode
	dismissed  bool      // failed-and-manually-dismissed (excluded from the ✗ count)
}

// syntheticRef is the Data payload on the status-bar and finished-bucket nodes.
// It is deliberately NOT a nodeRef, so the tree's OnSelect / OnSelectMouse /
// OnActivate handlers (which all type-assert nodeRef and bail otherwise) treat
// these synthetic rows as inert: a click never pops a monologue or raises a
// window. bucket distinguishes the two for any future synthetic-aware logic.
type syntheticRef struct {
	sessionID string
	bucket    bool
}

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
	// watchers maps a watcher id to its tree node, and watcherParents records each
	// watcher node's placement (issue #329 Phase 4): "" for a free-running watcher
	// rendered as a top-level root, or the target session id for an attached
	// watcher rendered as a child of that session's node. setWatchers reconciles
	// both against the live watcher list; the parent map is what lets a placement
	// change (or a vanished watcher) be detached from the right container.
	watchers       map[string]*tv.TreeNode
	watcherParents map[string]string
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

	// background tracks which sessions currently have async sub-agents running in the
	// background (issue #353), so their node shows the third "background" marker (◐ vs
	// busy ● / idle ○) and keeps it across unrelated relabels. It mirrors
	// SessionWindow.background and is reconciled on the UI thread by syncBackground from
	// Workbench.tickBusyStatuses, exactly like busy. busy takes precedence: a session
	// that is both shows ● (a foreground turn dominates the glyph).
	background map[string]bool

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

	// folds holds per-session TTL-fold bookkeeping for finished sub-agents (issue
	// #484), keyed by session id and created lazily on a session's first sub-agent.
	// All fold/status-bar state lives here in the UI mirror — never in the shared
	// agent tree — so folding is purely a visibility concern (see sessionFold).
	folds map[string]*sessionFold
	// now is the clock the fold TTL is measured against; it is time.Now in
	// production and overridden in tests so a fold can be exercised without sleeping
	// the real TTL. ttl is the live fold delay, defaulted to subAgentFoldTTL.
	now func() time.Time
	ttl time.Duration
}

// nodeRef identifies what a tree node points at: a session (agentID empty), a
// sub-agent within a session, or a watcher (watcher set). For a watcher node
// sessionID is the session the watcher reports into — the target session for an
// attached watcher (so selecting it focuses the parent window) or the dedicated
// watcher:<name> session for a free-running one (issue #329 Phase 4).
type nodeRef struct {
	sessionID string
	agentID   string
	name      string
	watcher   bool
}

// newSidebar builds the pinned sidebar panel and its tree.
func newSidebar(wb *Workbench) *sidebar {
	s := &sidebar{
		wb:             wb,
		sessions:       make(map[string]*tv.TreeNode),
		agents:         make(map[string]*tv.TreeNode),
		watchers:       make(map[string]*tv.TreeNode),
		watcherParents: make(map[string]string),
		todos:          make(map[string][]agent.TodoItem),
		approvals:      make(map[string]bool),
		busy:           make(map[string]bool),
		background:     make(map[string]bool),
		clarify:        make(map[string]bool),
		clarifyCount:   make(map[string]int),
		overallBandH:   overallBandHeight,
		folds:          make(map[string]*sessionFold),
		now:            time.Now,
		ttl:            subAgentFoldTTL,
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
		// (issue #314), clear of the title text and the divider column. Note column
		// w-2 is also the tree's last content column (the tree spans X:2..w-2 above),
		// but the glyph is row-disjoint from it — the glyph is on the header row (Y:0)
		// while the tree starts at Y:1 — so they never share a cell and a header-edge
		// click routes to the glyph, never the tree. This row separation is the
		// invariant that keeps the shared column safe: if the header ever grew past a
		// single row, or the tree moved up to Y:0, this would have to move with it.
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
		// path — so leave the selection where the user put it instead. A watcher node
		// is excluded for the same reason (an attached watcher child would snap to its
		// parent session): its window is opened on click/Enter below, not on traversal.
		if ref.agentID == "" && !ref.watcher && ref.sessionID != "" {
			s.wb.Focus(ref.sessionID)
		}
	}
	// A single MOUSE click on a sub-agent row opens its monologue, and on a watcher
	// row opens (or raises) the watcher's session window (issue #302, #329 Phase 4).
	// OnSelectMouse fires only on a pointer click, not on keyboard traversal, so
	// arrowing through the tree never pops a window; keyboard users use Enter
	// (OnActivate) below.
	tree.OnSelectMouse = func(n *tv.TreeNode) {
		ref, ok := n.Data.(nodeRef)
		if !ok {
			return
		}
		if ref.watcher {
			s.wb.openWatcherSession(ref.sessionID)
			return
		}
		if ref.agentID != "" && ref.sessionID != "" {
			s.wb.showAgentMonolog(ref.sessionID, ref.agentID, ref.name)
		}
	}
	tree.OnActivate = func(n *tv.TreeNode) {
		ref, ok := n.Data.(nodeRef)
		if !ok {
			return
		}
		if ref.watcher {
			s.wb.openWatcherSession(ref.sessionID)
			return
		}
		if ref.sessionID == "" {
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
	// Seed the tree / dropdown colours from the active palette explicitly, so the
	// sidebar is correct on a fresh start regardless of whether ApplyTheme ran
	// before construction, and so the live-switch reseed has a single funnel
	// (issue #379).
	s.refreshTheme()
	return s
}

// refreshTheme re-seeds the sidebar's already-built turbotui widgets from the
// active palette so a live theme switch recolours them without a restart (issue
// #379). The panel fill, the "Sessions & Agents"/"TODOs"/"Overall" titles, the
// dividers, the TODO/Overall bands and the resize handle all read the package
// chrome vars (chromePanelBG/FG, chromeTitle, chromeDivider, chromeAccent) at
// draw time, so they already follow the theme on a redraw. But the session /
// sub-agent / watcher tree and the Overall band's model dropdown froze their
// colours at construction — tv.NewTree seeds its row FG/BG/selection from the
// active theme once, and newSelect seeds the closed control once — so after a
// default→dark switch the tree's row region kept its stale fill while the rest of
// the panel recoloured (the reported "stays blue"). Reseeding them here keeps the
// whole sidebar in lockstep, mirroring SessionWindow.refreshTheme's reseed of the
// frozen transcript view (issue #204). Runs on the UI thread, from
// Workbench.RefreshTheme and once at construction.
func (s *sidebar) refreshTheme() {
	th := tv.ActiveTheme()
	if s.tree != nil {
		// The tree's plain row fill IS sidebar body text on the sidebar background, so
		// it follows the gogent PANEL roles — the exact package vars panel.DrawFn fills
		// the surrounding panel and drawTodos/drawOverall paint their body rows with —
		// NOT turbotui's WindowFG/WindowBG that tv.NewTree happens to seed (issue #379).
		// Those coincide with Panel* only because the built-in presets keep
		// WindowBG == PanelBG and (for dark/high-contrast) WindowFG == PanelFG; that
		// coincidence is exactly what masked the bug, and it breaks when a custom theme
		// repoints panel_fg/panel_bg independently of window_fg/window_bg (the theme
		// editor exposes both). Sourcing the fill from the panel chrome makes the tree
		// blend with the panel under every preset and every override, unifies the row
		// text with the TODO/Overall body (which already use chromePanelFG — the tree
		// was the lone hold-out on the brighter WindowFG), and is the pair the
		// paletteContrast "panel-body" audit (PanelFG/PanelBG) actually certifies.
		s.tree.FG, s.tree.BG = chromePanelFG, chromePanelBG
		// Selection bars. The sidebar tree never takes keyboard focus — every
		// SetFocus target is a session window's input, never the tree — so the
		// FOCUSED bar (SelFG/SelBG) is never actually painted here; it is still
		// seeded from the theme's Selection* roles for correctness should that ever
		// change. What the user always sees on the active session's row is the
		// UNFOCUSED bar, which tv.NewTree hard-codes as WindowFG over ANSI 8 — a fixed
		// grey that ignores the sidebar palette, follows window chrome rather than
		// panel chrome when a custom theme repoints them independently, and emits a
		// stray ANSI 8 under NO_COLOR (issue #379). Reseed it instead as a reverse-
		// video of the panel chrome: panel-background text on a panel-foreground bar.
		// That keeps the highlight on the sidebar's own palette (so it tracks panel_fg/
		// panel_bg and every preset), stays distinct from the plain PanelBG fill so the
		// active row still reads as selected, is legible by the same symmetry the
		// paletteContrast "panel-body" audit already certifies for PanelFG/PanelBG, and
		// flattens to the terminal default under NO_COLOR (no stray ANSI 8) — the bar
		// simply disappears there, as a colour-based highlight must.
		s.tree.SelFG, s.tree.SelBG = th.SelectionFG, th.SelectionBG
		s.tree.SelFGUnfocused, s.tree.SelBGUnfocused = chromePanelBG, chromePanelFG
	}
	// The Overall band's model selector is a closed control seeded once from the
	// dropdown palette (issue #260); reseed it like every other live Select.
	reseedSelect(s.overallSelect, th)
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
	node := tv.NewTreeNode(sessionLabelState(title, s.busy[id], s.background[id], pinned, s.approvals[id], s.clarify[id]))
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
	// Drop every sub-agent's node bookkeeping via the fold entries, which cover
	// both visible rows and agents folded under the finished bucket (issue #484) —
	// a plain node.Children walk would miss the folded ones. The fold entry key is
	// exactly the s.agents key and the EmitSessionEvent clarify dedup key (agent
	// id, else session/name), so it prunes both correctly. One sub-agent still in
	// StatusWaiting at close emits no terminal event, so without the clarify prune
	// its key would linger (issue #207).
	if fold := s.folds[id]; fold != nil {
		for key := range fold.entries {
			delete(s.agents, key)
			s.pruneClarifyWaiting(id, key)
		}
		delete(s.folds, id)
	}
	// Drop bookkeeping for the session's attached watcher children (issue #329
	// Phase 4): the nodes themselves vanish with the parent node removed below, but
	// their entries in the watcher maps would otherwise dangle. Free-running
	// watchers are independent roots and are left untouched.
	for wid, parent := range s.watcherParents {
		if parent == id {
			delete(s.watchers, wid)
			delete(s.watcherParents, wid)
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
	delete(s.background, id)
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
		node.Label = sessionLabelState(title, s.busy[id], s.background[id], pinned, pending, s.clarify[id])
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
		node.Label = sessionLabelState(title, s.busy[id], s.background[id], pinned, s.approvals[id], s.clarify[id])
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
		node.Label = sessionLabelState(title, busy, s.background[id], pinned, s.approvals[id], s.clarify[id])
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

// setBackground toggles the "working in background" marker (◐) on a session node and
// records the state so a later relabel keeps the marker correct (issue #353). It is
// the background-state twin of setBusy: a no-op on the node for an unknown session,
// but the intent is still recorded so a node added later picks up the marker. busy
// takes precedence in the rendered glyph (sessionStatusGlyph), so a session that is
// both shows ●. Runs on the UI thread.
func (s *sidebar) setBackground(id, title string, pinned, background bool) {
	if background {
		s.background[id] = true
	} else {
		delete(s.background, id)
	}
	if node := s.sessions[id]; node != nil {
		node.Label = sessionLabelState(title, s.busy[id], background, pinned, s.approvals[id], s.clarify[id])
	}
}

// syncBackground reconciles every session's background marker against bgIDs (the set
// of session ids with async sub-agents running) and reports whether any marker moved.
// It mirrors syncBusy and is driven by Workbench.tickBusyStatuses on the same tick;
// the returned bool lets the caller redraw exactly on a transition, including the
// →idle edge that empties the set. Runs on the UI thread.
func (s *sidebar) syncBackground(bgIDs map[string]bool) bool {
	changed := false
	for id, node := range s.sessions {
		want := bgIDs[id]
		if want == s.background[id] {
			continue
		}
		title := id
		if ref, ok := node.Data.(nodeRef); ok {
			title = ref.name
		}
		s.setBackground(id, title, s.wb.IsPinned(id), want)
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

// applySubAgent inserts or updates a sub-agent node from a lifecycle event and
// maintains the per-session TTL-fold state (issue #484): it lazily builds the
// session's status-bar node, records when a sub-agent first completes (the TTL
// clock start), unfolds an agent that leaves the completed state (a re-run edge),
// and refreshes the status-bar / bucket labels. The actual time-based fold happens
// later in tickFolds; this only stamps the finish time. Runs on the UI thread
// (called from Workbench.deliverSessionEvent).
func (s *sidebar) applySubAgent(sessionID string, ev agent.SessionEvent) {
	parent := s.sessions[sessionID]
	if parent == nil {
		return
	}
	fold := s.ensureFold(sessionID, parent)
	key := ev.AgentID
	if key == "" {
		key = sessionID + "/" + ev.Name
	}
	node := s.agents[key]
	if node == nil {
		node = tv.NewTreeNode("")
		node.Data = nodeRef{sessionID: sessionID, agentID: ev.AgentID, name: ev.Name}
		s.agents[key] = node
		s.insertVisibleAgent(fold, parent, node)
	} else if ref, ok := node.Data.(nodeRef); ok {
		// Keep the name in sync (it may have been empty on first sight).
		ref.name = ev.Name
		node.Data = ref
	}
	node.Label = agentLabel(ev.Name, ev.Status, ev.Kind)

	entry := fold.entries[key]
	if entry == nil {
		entry = &foldEntry{}
		fold.entries[key] = entry
	}
	entry.status = ev.Status
	switch ev.Status {
	case agent.StatusCompleted:
		// Start the fold TTL on the first completion only; a duplicate completion
		// event must not reset the clock.
		if entry.finishedAt.IsZero() {
			entry.finishedAt = s.now()
		}
	default:
		// The agent left the completed state (e.g. re-run): cancel its TTL and pull
		// it back into the visible list if it had already folded.
		entry.finishedAt = time.Time{}
		if entry.folded {
			s.unfoldAgent(fold, parent, node)
			entry.folded = false
		}
	}
	s.refreshFoldChrome(sessionID)
}

// ensureFold returns the session's fold bookkeeping, creating it (and the
// always-first status-bar node) on first use. The status-bar node is inserted at
// child index 0, shifting any existing children (e.g. attached watchers) right.
// The bucket node is NOT created here — it is attached lazily by foldAgent so a
// session with nothing folded never renders a stray "[✓ 0]" row.
func (s *sidebar) ensureFold(sessionID string, parent *tv.TreeNode) *sessionFold {
	if fold := s.folds[sessionID]; fold != nil {
		return fold
	}
	status := tv.NewTreeNode("")
	status.Data = syntheticRef{sessionID: sessionID}
	parent.Children = append([]*tv.TreeNode{status}, parent.Children...)
	fold := &sessionFold{
		statusNode: status,
		entries:    make(map[string]*foldEntry),
	}
	s.folds[sessionID] = fold
	return fold
}

// insertVisibleAgent adds a real (un-folded) agent node to the session's visible
// child list, after the synthetic prefix (the status bar, and the bucket when
// present) so the synthetic rows stay pinned at the front regardless of insert/
// detach order with watcher nodes.
func (s *sidebar) insertVisibleAgent(fold *sessionFold, parent, node *tv.TreeNode) {
	at := s.syntheticPrefixLen(fold, parent)
	parent.Children = append(parent.Children, nil)
	copy(parent.Children[at+1:], parent.Children[at:])
	parent.Children[at] = node
}

// syntheticPrefixLen is the number of leading synthetic nodes (status bar +
// bucket-when-present) currently at the front of the session's child list. It
// reads positions rather than assuming a fixed count so it stays correct even if
// a node was momentarily reordered.
func (s *sidebar) syntheticPrefixLen(fold *sessionFold, parent *tv.TreeNode) int {
	n := 0
	if fold.statusNode != nil && len(parent.Children) > n && parent.Children[n] == fold.statusNode {
		n++
	}
	if fold.bucketNode != nil && len(parent.Children) > n && parent.Children[n] == fold.bucketNode {
		n++
	}
	return n
}

// foldAgent moves a completed agent's node out of the visible list and under the
// finished bucket, creating and attaching the bucket node (collapsed) on the
// first fold. Later folds leave the bucket's expand state as the user last set it,
// giving "collapsed by default once non-empty" without overriding a manual expand.
func (s *sidebar) foldAgent(fold *sessionFold, parent, node *tv.TreeNode) {
	if fold.bucketNode == nil {
		bucket := tv.NewTreeNode("")
		bucket.Data = syntheticRef{sessionID: refSessionID(node), bucket: true}
		bucket.Expanded = false
		// Insert immediately after the status bar (index 1 when the status bar is
		// child 0).
		at := 0
		if fold.statusNode != nil && len(parent.Children) > 0 && parent.Children[0] == fold.statusNode {
			at = 1
		}
		parent.Children = append(parent.Children, nil)
		copy(parent.Children[at+1:], parent.Children[at:])
		parent.Children[at] = bucket
		fold.bucketNode = bucket
	}
	removeChild(parent, node)
	fold.bucketNode.Children = append(fold.bucketNode.Children, node)
}

// unfoldAgent moves an agent node from under the finished bucket back into the
// visible child list, detaching (and clearing) the bucket if it becomes empty so
// no "[✓ 0]" row lingers.
func (s *sidebar) unfoldAgent(fold *sessionFold, parent, node *tv.TreeNode) {
	if fold.bucketNode == nil {
		return
	}
	removeChild(fold.bucketNode, node)
	s.insertVisibleAgent(fold, parent, node)
	if len(fold.bucketNode.Children) == 0 {
		removeChild(parent, fold.bucketNode)
		fold.bucketNode = nil
	}
}

// tickFolds folds every completed sub-agent whose TTL has elapsed into its
// session's finished bucket and returns whether anything moved (so the caller
// redraws only on a real fold edge). It is driven once per status tick by
// Workbench.tickBusyStatuses — no per-agent timers, no extra goroutine. The
// selection is re-anchored by node identity across the fold so a background tick
// never silently drifts the highlight off the user's row (the tree's selection is
// index-based). Runs on the UI thread.
func (s *sidebar) tickFolds() bool {
	now := s.now()
	sel := s.tree.Selected()
	selFolded := false
	changed := false
	for sessionID, fold := range s.folds {
		parent := s.sessions[sessionID]
		if parent == nil {
			continue
		}
		sessionChanged := false
		for key, entry := range fold.entries {
			if entry.folded || entry.status != agent.StatusCompleted || entry.finishedAt.IsZero() {
				continue
			}
			if now.Sub(entry.finishedAt) < s.ttl {
				continue
			}
			node := s.agents[key]
			if node == nil {
				continue
			}
			if node == sel {
				selFolded = true
			}
			s.foldAgent(fold, parent, node)
			entry.folded = true
			sessionChanged = true
		}
		if sessionChanged {
			s.refreshFoldChrome(sessionID)
			changed = true
		}
	}
	if changed && sel != nil {
		// Re-anchor the highlight: if the selected row survived, keep it; if it was
		// the row just folded away, land on the bucket that absorbed it rather than
		// drift to an unrelated node.
		if !s.tree.SelectNode(sel) && selFolded {
			if fold := s.foldOf(sel); fold != nil && fold.bucketNode != nil {
				s.tree.SelectNode(fold.bucketNode)
			}
		}
	}
	return changed
}

// foldOf returns the fold whose status/bucket/agent nodes own node, or nil. Used
// to find the bucket a just-folded selected agent landed under.
func (s *sidebar) foldOf(node *tv.TreeNode) *sessionFold {
	if ref, ok := node.Data.(nodeRef); ok {
		return s.folds[ref.sessionID]
	}
	if ref, ok := node.Data.(syntheticRef); ok {
		return s.folds[ref.sessionID]
	}
	return nil
}

// refreshFoldChrome recomputes the session's status-bar and finished-bucket
// labels from its fold entries. The ✓ count includes folded agents; the ✗ count
// excludes dismissed failures. When the session has no live counts and nothing
// folded (e.g. its only agent was a dismissed failure), the synthetic nodes are
// torn down and the fold entry dropped, returning the row to its clean pre-agent
// state. Runs on the UI thread.
func (s *sidebar) refreshFoldChrome(sessionID string) {
	fold := s.folds[sessionID]
	if fold == nil {
		return
	}
	parent := s.sessions[sessionID]
	if len(fold.entries) == 0 {
		// No tracked sub-agents remain (e.g. the only agent was a dismissed failure):
		// drop the synthetic rows and the bookkeeping so the session row returns to
		// its clean pre-agent state. Keyed on the entry set rather than on the visible
		// counts so an agent in a non-counted transient status never orphans its node.
		if parent != nil {
			if fold.bucketNode != nil {
				removeChild(parent, fold.bucketNode)
			}
			if fold.statusNode != nil {
				removeChild(parent, fold.statusNode)
			}
		}
		delete(s.folds, sessionID)
		return
	}
	var running, waiting, completed, failed int
	for _, e := range fold.entries {
		switch {
		case e.status == agent.StatusRunning:
			running++
		case e.status == agent.StatusWaiting:
			waiting++
		case e.status == agent.StatusCompleted:
			completed++
		case e.status == agent.StatusFailed && !e.dismissed:
			failed++
		}
	}
	if fold.statusNode != nil {
		fold.statusNode.Label = statusBarLabel(running, waiting, completed, failed)
	}
	if fold.bucketNode != nil {
		// The tree supplies the ▸/▾ marker for a node with children; the label is
		// just the folded-completed count.
		fold.bucketNode.Label = fmt.Sprintf("[%s %d]", statusIcon(agent.StatusCompleted), len(fold.bucketNode.Children))
	}
}

// statusBarLabel renders the bracketed per-state count row using the same glyphs
// as agent rows (statusIcon). Zero counts are omitted; the brackets make it
// visually distinct from real agent rows (which lead with a single status glyph).
func statusBarLabel(running, waiting, completed, failed int) string {
	var b strings.Builder
	b.WriteByte('[')
	first := true
	add := func(status agent.AgentStatus, n int) {
		if n == 0 {
			return
		}
		if !first {
			b.WriteByte(' ')
		}
		fmt.Fprintf(&b, "%s%d", statusIcon(status), n)
		first = false
	}
	add(agent.StatusRunning, running)
	add(agent.StatusWaiting, waiting)
	add(agent.StatusCompleted, completed)
	add(agent.StatusFailed, failed)
	b.WriteByte(']')
	return b.String()
}

// dismissFailed manually clears every undismissed failed sub-agent of a session:
// each failed agent's row is removed, its node bookkeeping dropped, and the
// status-bar ✗ count updated. Failed agents are never auto-folded (issue #484), so
// this is the user's only way to clear them. It mirrors removeSession's per-agent
// cleanup (clarifyWaiting dedup key). A no-op for an unknown session or one with
// no failed agents. Runs on the UI thread.
func (s *sidebar) dismissFailed(sessionID string) {
	fold := s.folds[sessionID]
	if fold == nil {
		return
	}
	parent := s.sessions[sessionID]
	dismissed := false
	for key, entry := range fold.entries {
		if entry.status != agent.StatusFailed || entry.dismissed {
			continue
		}
		entry.dismissed = true
		if node := s.agents[key]; node != nil && parent != nil {
			removeChild(parent, node)
		}
		delete(s.agents, key)
		delete(fold.entries, key)
		s.pruneClarifyWaiting(sessionID, key)
		dismissed = true
	}
	if dismissed {
		s.refreshFoldChrome(sessionID)
	}
}

// pruneClarifyWaiting drops the Workbench's clarify dedup entry for a sub-agent,
// reconstructing the key exactly as EmitSessionEvent derives it (agent id, else
// session/name). It is the shared helper for removeSession and dismissFailed so a
// removed sub-agent never leaves a dangling waiting key (issue #207).
func (s *sidebar) pruneClarifyWaiting(sessionID, key string) {
	if s.wb == nil || s.wb.clarifyWaiting == nil {
		return
	}
	delete(s.wb.clarifyWaiting, key)
}

// refSessionID extracts the owning session id from a real agent node's nodeRef.
func refSessionID(node *tv.TreeNode) string {
	if ref, ok := node.Data.(nodeRef); ok {
		return ref.sessionID
	}
	return ""
}

// removeChild detaches child from parent.Children by pointer identity, preserving
// the relative order of the remaining children. A no-op if child is not present.
func removeChild(parent, child *tv.TreeNode) {
	kids := parent.Children[:0]
	for _, c := range parent.Children {
		if c != child {
			kids = append(kids, c)
		}
	}
	parent.Children = kids
}

// setWatchers reconciles the sidebar's watcher nodes against the current watcher
// list (issue #329 Phase 4): free-running watchers render as top-level roots and
// each session's attached watchers render as children of that session's node. It
// is incremental — nodes that vanished or moved are detached, surviving nodes have
// their busy marker relabelled in place, and new ones are inserted — so it can run
// every status tick without churning the tree's selection or expansion state. It
// returns whether any node was added, removed, re-placed or relabelled, so the
// caller redraws only on a real change. An attached watcher whose target session
// has no node yet is skipped (it appears once the session node exists). Runs on
// the UI thread.
func (s *sidebar) setWatchers(free []WatcherInfo, attached map[string][]WatcherInfo) bool {
	// Desired placement: watcher id -> (info, parent session id; "" = top-level).
	type placement struct {
		info   WatcherInfo
		parent string
	}
	desired := make(map[string]placement, len(free))
	for _, info := range free {
		// Avoid a double entry: a free-running watcher's dedicated watcher:<name>
		// session is normally not open as a window (the watcher shows as a ◷ root),
		// but it CAN be opened (the dialog/sidebar Open-session path adopts it from
		// disk). When that window is open its own session row already represents the
		// watcher, so suppress the separate ◷ root; it reappears when the window is
		// closed (issue #329 Phase 4).
		if info.SessionID != "" && s.sessions[info.SessionID] != nil {
			continue
		}
		desired[info.ID] = placement{info, ""}
	}
	for sid, list := range attached {
		if s.sessions[sid] == nil {
			continue // cannot nest under a session that has no node yet
		}
		for _, info := range list {
			desired[info.ID] = placement{info, sid}
		}
	}

	changed := false
	// Detach watcher nodes that are gone, or whose parent placement changed (an
	// attached watcher re-pointed, or a kind flip) — they are re-added below.
	for id, node := range s.watchers {
		if p, ok := desired[id]; !ok || p.parent != s.watcherParents[id] {
			s.detachWatcherNode(id, node)
			changed = true
		}
	}
	// Add new nodes and relabel survivors in place.
	for id, p := range desired {
		ref := nodeRef{sessionID: p.info.SessionID, name: p.info.Name, watcher: true}
		label := watcherLabel(p.info)
		if node, ok := s.watchers[id]; ok {
			if node.Label != label {
				node.Label = label
				changed = true
			}
			node.Data = ref // keep the focus target / name fresh
			continue
		}
		node := tv.NewTreeNode(label)
		node.Data = ref
		if p.parent == "" {
			s.tree.AddRoot(node)
		} else {
			s.sessions[p.parent].Add(node)
		}
		s.watchers[id] = node
		s.watcherParents[id] = p.parent
		changed = true
	}
	return changed
}

// detachWatcherNode removes a watcher node from its container (the tree roots for
// a free-running watcher, or its parent session's children for an attached one)
// and drops its bookkeeping. Caller has already decided the node should go.
func (s *sidebar) detachWatcherNode(id string, node *tv.TreeNode) {
	if parent := s.watcherParents[id]; parent == "" {
		roots := s.tree.Roots[:0]
		for _, r := range s.tree.Roots {
			if r != node {
				roots = append(roots, r)
			}
		}
		s.tree.Roots = roots
	} else if pnode := s.sessions[parent]; pnode != nil {
		kids := pnode.Children[:0]
		for _, c := range pnode.Children {
			if c != node {
				kids = append(kids, c)
			}
		}
		pnode.Children = kids
	}
	delete(s.watchers, id)
	delete(s.watcherParents, id)
}

// watcherLabel renders a watcher tree row: the shared ◷ glyph, a live busy marker
// (●) while a fire runs, then the watcher's name. Free-running watchers are shown
// with their watcher:<name> session label (matching their dedicated session id);
// attached watchers use the bare name. The placement in the tree (root vs child),
// not the label, is what distinguishes the two kinds.
func watcherLabel(info WatcherInfo) string {
	name := info.Name
	if info.Free {
		name = "watcher:" + info.Name
	}
	if info.Running {
		return fmt.Sprintf("%s %s %s", watcherGlyph, sessionStatusIcon(true), name)
	}
	return fmt.Sprintf("%s %s", watcherGlyph, name)
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
	node.Label = sessionLabelState(title, s.busy[id], s.background[id], pinned, s.approvals[id], s.clarify[id])
}

// reorder reorders the tree's roots to match order. Sessions absent from order
// keep their relative positions at the tail; unknown ids in order are skipped.
// Free-running watcher roots (issue #329 Phase 4) are always preserved at the
// tail — they are keyed on their watcher:<name> session id, which order never
// lists, and the watcher flag keeps them even in the pathological case where a
// real session id collides with one.
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
		if ref, ok := node.Data.(nodeRef); ok && (ref.watcher || !seen[ref.sessionID]) {
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
	return sessionLabelState(title, busy, false, pinned, pending, clarify)
}

// sessionLabelState is sessionLabel with the third "working in background" state
// (issue #353) made explicit: the leading glyph is the tri-state ●/◐/○ from
// sessionStatusGlyph. sessionLabel is a thin wrapper (background=false) so existing
// call sites and tests are unaffected; the sidebar's own call sites pass the
// session's background flag so a row coordinating only async sub-agents shows ◐.
func sessionLabelState(title string, busy, background, pinned, pending, clarify bool) string {
	var label string
	if pinned {
		label = fmt.Sprintf("%s %s %s", sessionStatusGlyph(busy, background), "★", title)
	} else {
		label = fmt.Sprintf("%s %s", sessionStatusGlyph(busy, background), title)
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
// appears on a session row. It is the two-state form retained for callers that have
// no background notion; sessionStatusGlyph adds the third (◐) state.
func sessionStatusIcon(busy bool) string {
	return sessionStatusGlyph(busy, false)
}

// sessionStatusGlyph maps a session's busy/background state to its leading row glyph
// (issue #353): ● when a foreground turn is in flight, ◐ when only async sub-agents
// run in the background (the main loop is idle), ○ when fully idle. busy dominates —
// a session that is both shows ● — so the glyph always reflects the strongest signal.
func sessionStatusGlyph(busy, background bool) string {
	switch {
	case busy:
		return "●"
	case background:
		return "◐"
	default:
		return "○"
	}
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
