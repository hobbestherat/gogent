package ui

import (
	"strconv"
	"strings"
	"testing"

	tv "github.com/hobbestherat/turbotui/turbotv"
	"gogent/internal/agent"
)

// threeTodos is a checklist spanning every todo status, reused across the
// region / draw tests.
func threeTodos() []agent.TodoItem {
	return []agent.TodoItem{
		{Content: "Read README.md", Status: agent.TodoCompleted},
		{Content: "Implement feature", Status: agent.TodoInProgress},
		{Content: "Write tests", Status: agent.TodoPending},
	}
}

// TestApplyTodoStoresNotTreeChildren is the central regression guard for issue
// #190: a todo update is recorded in s.todos (the source of truth) but is NOT
// appended to the session node's children. Previously applyTodo called
// parent.Add, interleaving checklist rows under the session in the tree; the
// rows now live in their own middle region. A sub-agent, by contrast, still
// becomes a tree child, so the test pins the difference.
func TestApplyTodoStoresNotTreeChildren(t *testing.T) {
	s := newTestSidebar()
	s.addSession("s1", "Session 1", false)

	s.applyTodo("s1", threeTodos())

	if got := len(s.todos["s1"]); got != 3 {
		t.Fatalf("s.todos[s1] = %d items, want 3", got)
	}
	// No todo nodes were attached to the session: the tree half is agents only.
	if n := len(s.sessions["s1"].Children); n != 0 {
		t.Fatalf("session node has %d children after applyTodo, want 0 (todos must not be tree children)", n)
	}
	if len(s.agents) != 0 {
		t.Fatalf("s.agents = %d, want 0 (todos must not register as agents)", len(s.agents))
	}
	// Sanity check the other direction: a sub-agent is still a tree child, so
	// the test is actually exercising the todo path's distinctness.
	s.applySubAgent("s1", agent.SessionEvent{AgentID: "a1", Name: "worker", Status: agent.StatusRunning})
	if n := len(s.sessions["s1"].Children); n != 1 {
		t.Fatalf("sub-agent should add 1 tree child, got %d", n)
	}
}

// TestApplyTodoTreeChildrenStayZeroAcrossUpdates ensures repeated todo updates
// (the live checklist changing as the agent works) never accumulate tree
// children, since each update now only overwrites s.todos.
func TestApplyTodoTreeChildrenStayZeroAcrossUpdates(t *testing.T) {
	s := newTestSidebar()
	s.addSession("s1", "Session 1", false)

	s.applyTodo("s1", []agent.TodoItem{{Content: "a", Status: agent.TodoPending}})
	s.applyTodo("s1", []agent.TodoItem{{Content: "b", Status: agent.TodoInProgress}})
	s.applyTodo("s1", threeTodos())

	if got := len(s.todos["s1"]); got != 3 {
		t.Fatalf("s.todos[s1] = %d, want 3 (last update wins)", got)
	}
	if n := len(s.sessions["s1"].Children); n != 0 {
		t.Fatalf("tree children = %d after repeated updates, want 0", n)
	}
}

// TestApplyTodoUnknownSessionIgnored mirrors the documented guard: a todo event
// for a session that was never added (or already removed) is dropped silently.
func TestApplyTodoUnknownSessionIgnored(t *testing.T) {
	s := newTestSidebar()
	s.applyTodo("ghost", threeTodos())
	if _, ok := s.todos["ghost"]; ok {
		t.Fatal("todos recorded for an unknown session")
	}
	if len(s.todos) != 0 {
		t.Fatalf("s.todos = %v, want empty", s.todos)
	}
}

// TestApplyTodoEmptyClearsEntry verifies an empty checklist deletes the session's
// stored todos, which is what hides the middle region again (no blank block).
func TestApplyTodoEmptyClearsEntry(t *testing.T) {
	s := newTestSidebar()
	s.addSession("s1", "Session 1", false)
	s.applyTodo("s1", threeTodos())
	if len(s.todos["s1"]) != 3 {
		t.Fatal("precondition: todos not stored")
	}
	s.applyTodo("s1", nil)
	if _, ok := s.todos["s1"]; ok {
		t.Fatal("nil todo list should delete the stored entry")
	}
	s.applyTodo("s1", []agent.TodoItem{})
	if _, ok := s.todos["s1"]; ok {
		t.Fatal("zero-length todo list should delete the stored entry")
	}
}

// TestApplyTodoDefensiveCopy ensures applyTodo stores a copy, so the caller
// mutating its own slice after the call cannot corrupt the sidebar's state.
// (UserSession.SetTodos already copies; this guards the sidebar side too.)
func TestApplyTodoDefensiveCopy(t *testing.T) {
	s := newTestSidebar()
	s.addSession("s1", "Session 1", false)
	items := []agent.TodoItem{{Content: "original", Status: agent.TodoPending}}
	s.applyTodo("s1", items)

	items[0].Content = "mutated"
	if got := s.todos["s1"][0].Content; got != "original" {
		t.Fatalf("stored todo mutated after caller edit: %q", got)
	}
}

// TestApplyTodoReplacesNotAccumulates pins that each update replaces (not
// appends to) the session's slice, matching "applyTodo replaces a session's
// slice on every update".
func TestApplyTodoReplacesNotAccumulates(t *testing.T) {
	s := newTestSidebar()
	s.addSession("s1", "Session 1", false)
	s.applyTodo("s1", []agent.TodoItem{
		{Content: "old-1", Status: agent.TodoPending},
		{Content: "old-2", Status: agent.TodoPending},
	})
	s.applyTodo("s1", threeTodos())
	got := s.todos["s1"]
	if len(got) != 3 {
		t.Fatalf("len = %d, want 3 (replace, not accumulate)", len(got))
	}
	for _, it := range got {
		if strings.HasPrefix(it.Content, "old-") {
			t.Fatalf("stale item survived replace: %+v", it)
		}
	}
}

// TestFocusSessionDrivesRegion verifies the middle region follows the focused
// session: todoLineCount reflects whichever session was last focused, and is 0
// before any focus.
func TestFocusSessionDrivesRegion(t *testing.T) {
	s := newTestSidebar()
	s.addSession("s1", "Session 1", false)
	s.addSession("s2", "Session 2", false)
	s.applyTodo("s1", threeTodos()) // 3
	five := []agent.TodoItem{
		{Content: "x1", Status: agent.TodoPending},
		{Content: "x2", Status: agent.TodoPending},
		{Content: "x3", Status: agent.TodoPending},
		{Content: "x4", Status: agent.TodoPending},
		{Content: "x5", Status: agent.TodoPending},
	}
	s.applyTodo("s2", five)

	if s.todoLineCount() != 0 {
		t.Fatalf("todoLineCount before focus = %d, want 0", s.todoLineCount())
	}

	s.focusSession("s1")
	if got := s.todoLineCount(); got != 3 {
		t.Fatalf("todoLineCount focused s1 = %d, want 3", got)
	}
	if got := s.todosRegionHeight(); got != 1+3 {
		t.Fatalf("region height focused s1 = %d, want 4", got)
	}

	s.focusSession("s2")
	if got := s.todoLineCount(); got != 5 {
		t.Fatalf("todoLineCount focused s2 = %d, want 5", got)
	}
}

// TestTodoLineCountCapsAtMax pins the middle region's row cap so a long
// checklist cannot crowd out the session tree (the region drops first).
func TestTodoLineCountCapsAtMax(t *testing.T) {
	s := newTestSidebar()
	s.addSession("s1", "Session 1", false)
	many := make([]agent.TodoItem, maxTodoRegionItems+5)
	for i := range many {
		many[i] = agent.TodoItem{Content: "item", Status: agent.TodoPending}
	}
	s.applyTodo("s1", many)
	s.focusSession("s1")

	if got := s.todoLineCount(); got != maxTodoRegionItems {
		t.Fatalf("todoLineCount = %d, want cap %d", got, maxTodoRegionItems)
	}
	wantH := todoRegionTitleLines + maxTodoRegionItems
	if got := s.todosRegionHeight(); got != wantH {
		t.Fatalf("todosRegionHeight = %d, want %d", got, wantH)
	}
}

// TestTodosRegionHeightZeroCases covers the two ways the middle region hides:
// no focused session, and a focused session with an empty checklist.
func TestTodosRegionHeightZeroCases(t *testing.T) {
	s := newTestSidebar()
	s.addSession("s1", "Session 1", false)
	s.applyTodo("s1", threeTodos())

	// No focus at all.
	if got := s.todosRegionHeight(); got != 0 {
		t.Fatalf("region height without focus = %d, want 0", got)
	}
	// Focused, but the focused session has no todos.
	s.addSession("s2", "Session 2", false)
	s.focusSession("s2")
	if got := s.todosRegionHeight(); got != 0 {
		t.Fatalf("region height for empty focused session = %d, want 0", got)
	}
}

// TestSidebarTodosFollowActiveWindow is the regression guard for the focus
// wiring: the middle TODO region must follow the active top-most window through
// the real session-open path (openWindow -> AddLayer -> refreshOverall), WITHOUT
// any manual focusSession call or a Workbench.Focus hop. An earlier revision set
// s.focused only from Workbench.Focus, so a freshly opened session (the common
// case) never surfaced its todos; the fix resolves focus from the same top
// window the Overall band uses (refreshOverall -> focusSession(ActiveID)). This
// test would fail against that earlier revision.
func TestSidebarTodosFollowActiveWindow(t *testing.T) {
	w := newTestWorkbench(t)

	// Opening a window makes it the active top window; the sidebar's focus must
	// follow it without an explicit focusSession / Focus call.
	w.openWindow("s1", "Session 1")
	if w.sidebar.focused != "s1" {
		t.Fatalf("after opening s1: focused = %q, want s1 (must follow active window)", w.sidebar.focused)
	}
	if w.sidebar.focused != w.ActiveID() {
		t.Fatalf("focused %q diverges from active window %q (must mirror the Overall band)", w.sidebar.focused, w.ActiveID())
	}

	// A second open becomes the new top window; focus tracks it.
	w.openWindow("s2", "Session 2")
	if w.sidebar.focused != "s2" {
		t.Fatalf("after opening s2: focused = %q, want s2", w.sidebar.focused)
	}

	// Workbench.Focus (sidebar/menu/cycle path) still updates the region's focus
	// now that the explicit focusSession call lives in refreshOverall.
	w.Focus("s1")
	if w.sidebar.focused != "s1" {
		t.Fatalf("after Focus(s1): focused = %q, want s1", w.sidebar.focused)
	}

	// The region renders the focused/active session's todos only.
	w.sidebar.applyTodo("s1", threeTodos())
	w.sidebar.applyTodo("s2", threeTodos())
	w.Focus("s2")
	if w.sidebar.focused != "s2" || w.sidebar.todoLineCount() != 3 {
		t.Errorf("focused s2: focused=%q todoLineCount=%d, want s2/3", w.sidebar.focused, w.sidebar.todoLineCount())
	}
	w.Focus("s1")
	if w.sidebar.focused != "s1" || w.sidebar.todoLineCount() != 3 {
		t.Errorf("focused s1: focused=%q todoLineCount=%d, want s1/3", w.sidebar.focused, w.sidebar.todoLineCount())
	}
}

// TestRemoveSessionClearsTodosAndFocus verifies removeSession drops the closed
// session's todos and, when it was the focused one, resets focus so the middle
// region does not point at a removed session.
func TestRemoveSessionClearsTodosAndFocus(t *testing.T) {
	s := newTestSidebar()
	s.addSession("s1", "Session 1", false)
	s.applyTodo("s1", threeTodos())
	s.focusSession("s1")

	s.removeSession("s1")

	if _, ok := s.todos["s1"]; ok {
		t.Fatal("todos for removed session were not cleared")
	}
	if s.focused != "" {
		t.Fatalf("focused = %q after removing the focused session, want empty", s.focused)
	}
	if s.todosRegionHeight() != 0 {
		t.Fatal("region height should be 0 after the focused session is removed")
	}
}

// TestRemoveSessionKeepsFocusWhenOtherRemoved ensures focus is only cleared when
// the focused session itself goes away, not when a sibling is closed.
func TestRemoveSessionKeepsFocusWhenOtherRemoved(t *testing.T) {
	s := newTestSidebar()
	s.addSession("s1", "Session 1", false)
	s.addSession("s2", "Session 2", false)
	s.focusSession("s1")

	s.removeSession("s2")

	if s.focused != "s1" {
		t.Fatalf("focused = %q, want s1 (unrelated remove must not clear focus)", s.focused)
	}
}

// TestTodoStatusIconsDistinctFromAgentIcons is the glyph-disambiguation guard
// from the issue: even though todos and sub-agents now live in separate regions,
// the todo glyphs must be a set disjoint from the sub-agent glyphs so a glance is
// unambiguous. It also pins the exact intended glyphs.
func TestTodoStatusIconsDistinctFromAgentIcons(t *testing.T) {
	agentGlyphs := map[string]bool{}
	for _, st := range []agent.AgentStatus{
		agent.StatusIdle, agent.StatusRunning, agent.StatusWaiting,
		agent.StatusCompleted, agent.StatusFailed,
	} {
		agentGlyphs[statusIcon(st)] = true
	}
	for _, st := range []agent.TodoStatus{
		agent.TodoPending, agent.TodoInProgress, agent.TodoCompleted,
	} {
		if g := todoStatusIcon(st); agentGlyphs[g] {
			t.Errorf("todo glyph %q (status %q) collides with a sub-agent glyph", g, st)
		}
	}
	// Pin the exact glyphs so a future "tidy" does not silently re-merge them.
	cases := []struct {
		status agent.TodoStatus
		glyph  string
	}{
		{agent.TodoPending, "☐"},
		{agent.TodoInProgress, "◐"},
		{agent.TodoCompleted, "☑"},
	}
	for _, tc := range cases {
		if got := todoStatusIcon(tc.status); got != tc.glyph {
			t.Errorf("todoStatusIcon(%q) = %q, want %q", tc.status, got, tc.glyph)
		}
	}
	// An unknown/zero status defaults to the pending glyph (must not be blank).
	if got := todoStatusIcon(agent.TodoStatus("nonsense")); got != "☐" {
		t.Errorf("todoStatusIcon(unknown) = %q, want %q", got, "☐")
	}
}

// TestTodoLabel covers one rendered checklist row: the status glyph prefix and
// content, whitespace trimming, and the "(empty)" placeholder for blank content.
func TestTodoLabel(t *testing.T) {
	for _, tc := range []struct {
		name string
		item agent.TodoItem
		want string
	}{
		{"pending", agent.TodoItem{Content: "Read docs", Status: agent.TodoPending}, "☐ Read docs"},
		{"in-progress", agent.TodoItem{Content: "Coding", Status: agent.TodoInProgress}, "◐ Coding"},
		{"completed", agent.TodoItem{Content: "Done", Status: agent.TodoCompleted}, "☑ Done"},
		{"trims surrounding whitespace", agent.TodoItem{Content: "  padded  ", Status: agent.TodoPending}, "☐ padded"},
		{"blank content shows placeholder", agent.TodoItem{Content: "   ", Status: agent.TodoPending}, "☐ (empty)"},
		{"empty content shows placeholder", agent.TodoItem{Content: "", Status: agent.TodoPending}, "☐ (empty)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := todoLabel(tc.item); got != tc.want {
				t.Errorf("todoLabel = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestTodoRowClippedToContentWidth verifies drawTodos clips each row to the
// content width via truncateRunes, so a long task can never run into the divider
// or the panel edge. drawTodos uses contentW = panelWidth - 3.
func TestTodoRowClippedToContentWidth(t *testing.T) {
	for _, panelW := range []int{32, 24, minSidebarWidth, 14} {
		contentW := panelW - 3
		long := agent.TodoItem{Content: strings.Repeat("A", contentW+10), Status: agent.TodoPending}
		rendered := truncateRunes(todoLabel(long), contentW)
		if rc := len([]rune(rendered)); rc > contentW {
			t.Errorf("panelW=%d: rendered row = %d runes, want <= %d (%q)", panelW, rc, contentW, rendered)
		}
		// The full over-long content must not survive clipping.
		if strings.Contains(rendered, strings.Repeat("A", contentW+1)) {
			t.Errorf("panelW=%d: rendered row exceeded the content width: %q", panelW, rendered)
		}
	}
}

// --- render-path tests (drive the real DrawFn via the desktop) ---------------

// renderSidebarRows lays the sidebar panel out at rect, renders the whole
// desktop into the workbench's app buffer (Apply is a no-op before Run, so this
// is silent under `go test`), and returns the panel's rows as strings — one rune
// per column — so a test can assert where content lands without depending on
// coordinates outside the panel.
func renderSidebarRows(t *testing.T, w *Workbench, rect tv.Rect) []string {
	t.Helper()
	// The workbench's app is buffer-backed under `go test` (Apply is a no-op
	// before Run, and reads/writes are OOB-safe), so this is silent and
	// panic-free. rect is kept inside the default 80x25 fallback buffer.
	w.sidebar.panel.SetBounds(rect)
	w.desktop.Redraw()
	abs := w.sidebar.panel.AbsoluteBounds()
	rows := make([]string, abs.H)
	for y := 0; y < abs.H; y++ {
		var b strings.Builder
		for x := 0; x < abs.W; x++ {
			ch := w.app.ReadCell(abs.X+x, abs.Y+y).Ch
			if ch == 0 {
				ch = ' '
			}
			b.WriteRune(ch)
		}
		rows[y] = b.String()
	}
	return rows
}

// rowWith returns the first row index whose trimmed content contains sub, or -1.
func rowWith(rows []string, sub string) int {
	for i, r := range rows {
		if strings.Contains(strings.TrimSpace(r), sub) {
			return i
		}
	}
	return -1
}

// anyRowInRangeContains reports whether any row in [from,to) contains sub.
func anyRowInRangeContains(rows []string, from, to int, sub string) bool {
	if from < 0 {
		from = 0
	}
	if to > len(rows) {
		to = len(rows)
	}
	for i := from; i < to; i++ {
		if strings.Contains(rows[i], sub) {
			return true
		}
	}
	return false
}

// todoGlyphs is the disambiguated glyph set the tree region must never show.
var todoGlyphs = []string{"☐", "◐", "☑"}

// joinRows is a compact dump for failure messages: one "NN|<row>|" per line.
func joinRows(rows []string) string {
	var b strings.Builder
	for i, r := range rows {
		b.WriteString(strconv.Itoa(i))
		b.WriteByte('|')
		b.WriteString(strings.TrimRight(r, " "))
		b.WriteByte('|')
		b.WriteByte('\n')
	}
	return b.String()
}

// TestSidebarDrawsTodosInMiddleRegion renders a session with both a sub-agent
// and a checklist, then asserts the three regions land top→bottom (tree /
// TODOs / Overall), the checklist rows sit between the "TODOs" header and the
// "Overall" band, and — the core #190 requirement — no todo glyph leaks into the
// session tree above the header.
func TestSidebarDrawsTodosInMiddleRegion(t *testing.T) {
	w := newTestWorkbench(t)
	s := w.sidebar
	s.addSession("s1", "Session 1", false)
	s.applySubAgent("s1", agent.SessionEvent{AgentID: "a1", Name: "worker", Status: agent.StatusRunning})
	s.applyTodo("s1", threeTodos())
	s.focusSession("s1")

	rows := renderSidebarRows(t, w, tv.Rect{X: 0, Y: 0, W: 40, H: 24})

	treeIdx := rowWith(rows, "Session 1")
	if treeIdx < 0 {
		t.Fatalf("session row missing in tree region; rows:\n%s", joinRows(rows))
	}
	todosIdx := rowWith(rows, "TODOs")
	if todosIdx < 0 {
		t.Fatalf("TODOs header missing; rows:\n%s", joinRows(rows))
	}
	overallIdx := rowWith(rows, "Overall")
	if overallIdx < 0 {
		t.Fatalf("Overall band missing; rows:\n%s", joinRows(rows))
	}

	// Region order top -> bottom.
	if !(treeIdx < todosIdx && todosIdx < overallIdx) {
		t.Fatalf("region order wrong: tree=%d todos=%d overall=%d", treeIdx, todosIdx, overallIdx)
	}
	// A checklist row lands in the middle region (between the header and the band).
	if !anyRowInRangeContains(rows, todosIdx, overallIdx, "Read README.md") {
		t.Errorf("focused todo content missing from middle region; rows:\n%s", joinRows(rows))
	}
	// The tree region (title row .. TODOs header) must contain no todo glyph:
	// todos are not interleaved under the session node.
	for i := 1; i < todosIdx; i++ {
		for _, g := range todoGlyphs {
			if strings.Contains(rows[i], g) {
				t.Errorf("todo glyph %q leaked into tree region row %d: %q", g, i, rows[i])
			}
		}
	}
}

// TestSidebarDrawsOnlyFocusedSessionTodos verifies the middle region renders the
// focused session's checklist only: focusing s1 shows s1's content and hides
// s2's, and switching focus swaps them.
func TestSidebarDrawsOnlyFocusedSessionTodos(t *testing.T) {
	w := newTestWorkbench(t)
	s := w.sidebar
	s.addSession("s1", "Session 1", false)
	s.addSession("s2", "Session 2", false)
	s.applyTodo("s1", []agent.TodoItem{{Content: "s1-unique-task", Status: agent.TodoPending}})
	s.applyTodo("s2", []agent.TodoItem{{Content: "s2-unique-task", Status: agent.TodoPending}})

	s.focusSession("s1")
	rows := renderSidebarRows(t, w, tv.Rect{X: 0, Y: 0, W: 40, H: 24})
	if rowWith(rows, "s1-unique-task") < 0 {
		t.Errorf("focused s1 todo missing; rows:\n%s", joinRows(rows))
	}
	if rowWith(rows, "s2-unique-task") >= 0 {
		t.Errorf("non-focused s2 todo leaked into region; rows:\n%s", joinRows(rows))
	}

	s.focusSession("s2")
	rows = renderSidebarRows(t, w, tv.Rect{X: 0, Y: 0, W: 40, H: 24})
	if rowWith(rows, "s2-unique-task") < 0 {
		t.Errorf("focused s2 todo missing after swap; rows:\n%s", joinRows(rows))
	}
	if rowWith(rows, "s1-unique-task") >= 0 {
		t.Errorf("s1 todo lingered after focusing s2; rows:\n%s", joinRows(rows))
	}
}

// TestSidebarEmptyTodosHidesRegion verifies a focused session with no checklist
// renders no "TODOs" header and reserves no middle region (no blank block).
func TestSidebarEmptyTodosHidesRegion(t *testing.T) {
	w := newTestWorkbench(t)
	s := w.sidebar
	s.addSession("s1", "Session 1", false)
	s.focusSession("s1") // focused but no todos

	rows := renderSidebarRows(t, w, tv.Rect{X: 0, Y: 0, W: 40, H: 24})
	if rowWith(rows, "TODOs") >= 0 {
		t.Errorf("TODOs header shown for empty checklist; rows:\n%s", joinRows(rows))
	}
	if s.todosBandH != 0 {
		t.Errorf("todosBandH = %d, want 0 for empty checklist", s.todosBandH)
	}
}

// TestSidebarDrawsNoTodoRegionBeforeFocus verifies that with todos stored but no
// session focused yet, the middle region stays hidden (the Overall band still
// draws at the bottom).
func TestSidebarDrawsNoTodoRegionBeforeFocus(t *testing.T) {
	w := newTestWorkbench(t)
	s := w.sidebar
	s.addSession("s1", "Session 1", false)
	s.applyTodo("s1", threeTodos())

	rows := renderSidebarRows(t, w, tv.Rect{X: 0, Y: 0, W: 40, H: 24})
	if rowWith(rows, "TODOs") >= 0 {
		t.Errorf("TODOs header shown before any focus; rows:\n%s", joinRows(rows))
	}
	if rowWith(rows, "Overall") < 0 {
		t.Errorf("Overall band missing; rows:\n%s", joinRows(rows))
	}
}

// TestSidebarTodoRowRenderedClipped drives the real draw path on a narrow panel
// and asserts the over-long content is truncated, not wrapped or run past the
// divider / edge.
func TestSidebarTodoRowRenderedClipped(t *testing.T) {
	w := newTestWorkbench(t)
	s := w.sidebar
	s.addSession("s1", "Session 1", false)
	long := strings.Repeat("Z", 60)
	s.applyTodo("s1", []agent.TodoItem{{Content: long, Status: agent.TodoPending}})
	s.focusSession("s1")

	const panelW = 14
	contentW := panelW - 3
	rows := renderSidebarRows(t, w, tv.Rect{X: 0, Y: 0, W: panelW, H: 24})
	if rowWith(rows, "TODOs") < 0 {
		t.Fatalf("TODOs header missing on narrow panel; rows:\n%s", joinRows(rows))
	}
	// No row may carry more Zs than the content width.
	for i, r := range rows {
		if n := strings.Count(r, "Z"); n > contentW {
			t.Errorf("row %d has %d Zs, content width is %d: %q", i, n, contentW, r)
		}
	}
}
