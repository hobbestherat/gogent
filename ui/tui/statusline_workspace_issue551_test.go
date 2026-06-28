package ui

// Tests for the issue #551 status-line working-directory affordance: a
// right-aligned, colorInfo-cyan, shortened WorkspaceRoot painted on the existing
// status row via a custom DrawFn, with the left content truncated to a reserved
// width so the two never collide and narrow windows degrade to the pre-#551
// full-width render.
//
// Coverage maps to the four design gates:
//   - shortenPath / pathBudget pure logic (goal match + no regressions)
//   - Workbench.WorkspaceRoot nil-safety + live (uncached) read across a
//     SetHandlers swap (no regressions — the round-1 stale-cache hazard)
//   - refreshStatus width thresholds + reservation invariant (no regressions)
//   - headless render: right-aligned cyan path, severity-coloured left content,
//     gap, no overlap; omission when unwired; the accepted background-state
//     colour collision; end-to-end ~-collapse (usability + goal match)

import (
	"runtime"
	"strings"
	"testing"

	tui "github.com/hobbestherat/turbotui"
	tv "github.com/hobbestherat/turbotui/turbotv"
	"gogent/internal/agent"
	"gogent/internal/config"
)

// TestShortenPath covers the pure rendering helper: home collapse, the staged
// head/…/tail elision, the floor truncation, the "" contract, and cross-platform
// separator handling. "…" and "·" are single-cell under tui.StringWidth.
func TestShortenPath(t *testing.T) {
	for _, tc := range []struct {
		name        string
		path        string
		home        string
		maxW        int
		want        string
		wantNonzero bool // when true, only assert a non-empty, in-budget result
	}{
		{name: "empty path", path: "", home: "/h", maxW: 20, want: ""},
		{name: "below floor omitted", path: "/a/b", home: "/h", maxW: 7, want: ""},
		{name: "exact home collapses to tilde", path: "/home/u", home: "/home/u", maxW: 20, want: "~"},
		{name: "under home fits as-is", path: "/home/u/code/gogent", home: "/home/u", maxW: 80, want: "~/code/gogent"},
		{name: "not under home fits as-is", path: "/opt/gogent", home: "/home/u", maxW: 80, want: "/opt/gogent"},
		{name: "empty home skips collapse", path: "/opt/gogent", home: "", maxW: 80, want: "/opt/gogent"},
		{name: "canonical deep -> two-tail", path: "/home/u/code/gogent/internal/agent", home: "/home/u", maxW: 20, want: "~/…/internal/agent"},
		{name: "absolute deep keeps leading slash", path: "/opt/gogent/internal/agent", home: "/home/u", maxW: 20, want: "/…/internal/agent"},
		{name: "deep -> one-tail", path: "/home/u/code/gogent/internal/agent", home: "/home/u", maxW: 14, want: "~/…/agent"},
		{name: "deep -> floor trunc with ellipsis", path: "/home/u/a/b/verylongname", home: "/home/u", maxW: 14, want: "~/…/verylongn…"},
		{name: "shallow -> whole-path floor trunc", path: "/home/u/verylongprojectname", home: "/home/u", maxW: 12, want: "~/verylongp…"},
		{name: "partial segment no false collapse", path: "/usersfoo/x", home: "/users", maxW: 80, want: "/usersfoo/x"},
		{name: "single relative segment", path: "gogent", home: "/h", maxW: 80, want: "gogent"},
		{name: "wide-glyph path fits", path: "/home/u/项目/代码", home: "/home/u", maxW: 80, want: "~/项目/代码"},
		{name: "wide-glyph path floor trunc", path: "/home/u/项目/代码", home: "/home/u", maxW: 8, wantNonzero: true}, // "~/…/代…", width 8
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := shortenPath(tc.path, tc.home, tc.maxW)
			if tc.wantNonzero {
				if got == "" {
					t.Fatalf("shortenPath(%q,%q,%d) = \"\", want a non-empty in-budget result", tc.path, tc.home, tc.maxW)
				}
				if tui.StringWidth(got) > tc.maxW {
					t.Errorf("shortenPath(%q,%q,%d) = %q (width %d), exceeds budget %d",
						tc.path, tc.home, tc.maxW, got, tui.StringWidth(got), tc.maxW)
				}
				if !strings.HasPrefix(got, "~/…/") {
					t.Errorf("shortenPath(%q,%q,%d) = %q, want a ~/…/ truncation", tc.path, tc.home, tc.maxW, got)
				}
				return
			}
			if got != tc.want {
				t.Errorf("shortenPath(%q, %q, %d) = %q, want %q", tc.path, tc.home, tc.maxW, got, tc.want)
			}
		})
	}
}

// TestShortenPathBackslashPaths covers Windows-style backslash roots. The
// implementation normalises separators with filepath.ToSlash, which only
// converts the HOST OS separator — so a backslash root is normalised on a
// Windows build (where Separator == '\\') but NOT on a Linux/macOS build (where
// Separator == '/', making ToSlash a no-op on '\\').
//
// The live feature is unaffected on every OS — os.Getwd() returns a path using
// the host separator, which ToSlash handles correctly on that host — but the
// helper's stated "cross-platform" normalisation does not actually normalise a
// foreign-separator path off-Windows. This test pins both behaviours so the gap
// is visible in-tree: on Windows it asserts the full collapse/shorten, and on
// other OSes it documents that backslashes are currently retained (a more
// robust fix would replace '\\' unconditionally rather than via ToSlash).
func TestShortenPathBackslashPaths(t *testing.T) {
	collapse := struct {
		path, home string
		m          int
		want       string
	}{
		`C:\Users\yves\code\gogent`, `C:\Users\yves`, 80, "~/code/gogent",
	}
	driveDeep := struct {
		path, home string
		m          int
		want       string
	}{
		`C:\opt\gogent\internal\agent`, `C:\Users\yves`, 20, "C:/…/internal/agent",
	}
	noHome := struct {
		path, home string
		m          int
		want       string
	}{
		`C:\a\b\c\d`, "", 8, "C:/…/c/d",
	}

	if runtime.GOOS == "windows" {
		check := func(p, h string, m int, want string) {
			if got := shortenPath(p, h, m); got != want {
				t.Errorf("shortenPath(%q,%q,%d)=%q, want %q", p, h, m, got, want)
			}
		}
		check(collapse.path, collapse.home, collapse.m, collapse.want)
		check(driveDeep.path, driveDeep.home, driveDeep.m, driveDeep.want)
		check(noHome.path, noHome.home, noHome.m, noHome.want)
		return
	}

	// Non-Windows: filepath.ToSlash leaves '\\' untouched, so the home-collapse
	// (which keys on home+"/") and the '/' segment split never fire. The backslash
	// path is returned largely as-is (flat-truncated when over budget). Pin that
	// current behaviour and the width invariant.
	for _, tc := range []struct {
		name, path, home string
		m                int
	}{
		{"collapse not applied", `C:\Users\yves\code\gogent`, `C:\Users\yves`, 80},
		{"drive deep flat-truncated", `C:\opt\gogent\internal\agent`, `C:\Users\yves`, 20},
		{"no home flat-truncated", `C:\a\b\c\d`, "", 8},
	} {
		got := shortenPath(tc.path, tc.home, tc.m)
		if !strings.ContainsRune(got, '\\') {
			t.Errorf("shortenPath(%q,%q,%d)=%q: backslash not retained (filepath.ToSlash is a no-op off-Windows)",
				tc.path, tc.home, tc.m, got)
		}
		if strings.HasPrefix(got, "~/") {
			t.Errorf("shortenPath(%q,%q,%d)=%q: ~-collapse must not fire for a backslash root off-Windows",
				tc.path, tc.home, tc.m, got)
		}
		if w := tui.StringWidth(got); w > tc.m {
			t.Errorf("shortenPath(%q,%q,%d)=%q width %d exceeds %d", tc.path, tc.home, tc.m, got, w, tc.m)
		}
	}
}

// TestShortenPathFitsInBudget is the invariant sweep: for any path/home and
// every maxW, the result is either "" (only when path is empty or maxW is below
// the floor) or a string whose display width never exceeds maxW. This would
// catch an overflow that lets the path crowd the left content.
func TestShortenPathFitsInBudget(t *testing.T) {
	cases := []struct{ path, home string }{
		{"/home/u/code/gogent/internal/agent", "/home/u"},
		{"/opt/a/b/c/d/e/f", ""},
		{`C:\Users\me\deep\nested\project`, `C:\Users\me`},
		{"/home/u/项目/文件夹/深度", "/home/u"},
		{"/home/u", "/home/u"},
	}
	for _, c := range cases {
		for maxW := 0; maxW <= 60; maxW++ {
			got := shortenPath(c.path, c.home, maxW)
			switch {
			case got == "":
				if c.path != "" && maxW >= pathFloor {
					t.Errorf("shortenPath(%q,%q,%d) = \"\" but should fit (path non-empty, maxW>=floor)",
						c.path, c.home, maxW)
				}
			default:
				if w := tui.StringWidth(got); w > maxW {
					t.Errorf("shortenPath(%q,%q,%d) = %q (width %d) exceeds maxW",
						c.path, c.home, maxW, got, w)
				}
			}
		}
	}
}

// TestPathBudget pins the clamp(W/3, pathFloor, pathMax) used to size the path.
func TestPathBudget(t *testing.T) {
	for _, tc := range []struct {
		w    int
		want int
	}{
		{0, pathFloor}, {23, pathFloor}, {24, pathFloor}, // 24/3=8
		{30, 10}, {83, 27}, {84, pathMax}, {85, pathMax}, {90, pathMax}, {300, pathMax},
	} {
		if got := pathBudget(tc.w); got != tc.want {
			t.Errorf("pathBudget(%d) = %d, want %d", tc.w, got, tc.want)
		}
	}
}

// TestWorkbenchWorkspaceRootNilSafeAndLive guards the no-cache contract: the
// accessor is nil-safe ("" when unwired) and reads the LIVE handler, so a runtime
// SetHandlers swap — a daemon attach/handoff — is reflected immediately. This is
// the regression test for the round-1 stale-memo hazard.
func TestWorkbenchWorkspaceRootNilSafeAndLive(t *testing.T) {
	w := newTestWorkbench(t)
	if got := w.WorkspaceRoot(); got != "" {
		t.Fatalf("WorkspaceRoot() with no getter = %q, want \"\"", got)
	}

	w.SetHandlers(Handlers{GetWorkspaceRoot: func() string { return "/first/root" }})
	if got := w.WorkspaceRoot(); got != "/first/root" {
		t.Fatalf("WorkspaceRoot() after first wiring = %q, want /first/root", got)
	}

	// A handoff swaps the handlers; the live read must pick up the new root, not
	// a value memoised from the first handler set.
	w.SetHandlers(Handlers{GetWorkspaceRoot: func() string { return "/second/root" }})
	if got := w.WorkspaceRoot(); got != "/second/root" {
		t.Errorf("WorkspaceRoot() after handoff swap = %q, want /second/root (live read, no cache)", got)
	}

	// Swapping to an unwired set hides the path again.
	w.SetHandlers(Handlers{})
	if got := w.WorkspaceRoot(); got != "" {
		t.Errorf("WorkspaceRoot() after swap to unwired = %q, want \"\"", got)
	}
}

// TestRefreshStatusPathReservation drives refreshStatus at controlled status
// widths and asserts the path appears/omits at the right thresholds, the left
// content is reserved a non-overlapping width, and the unwired case leaves the
// pre-#551 full-width behaviour untouched.
func TestRefreshStatusPathReservation(t *testing.T) {
	// A short root that fits without shortening, so statusPath is exact and the
	// reservation math is unambiguous.
	const shortRoot = "/proj"

	setWidth := func(sw *SessionWindow, w int) {
		sw.status.Component.SetBounds(tv.Rect{X: 0, Y: 0, W: w, H: 1})
		sw.refreshStatus()
	}

	t.Run("wide shows exact path and reserves left width", func(t *testing.T) {
		w := newTestWorkbench(t)
		w.SetHandlers(Handlers{GetWorkspaceRoot: func() string { return shortRoot }})
		sw := w.openWindow("s", "S")
		setWidth(sw, 100)

		if sw.statusPath != shortRoot {
			t.Fatalf("statusPath = %q, want %q", sw.statusPath, shortRoot)
		}
		pw := tui.StringWidth(sw.statusPath)
		// The reserved left width is exactly W - pathW - gap; the left content
		// (truncated by formatStatusLine) must stay within it.
		leftW := 100 - pw - statusPathGap
		if got := tui.StringWidth(sw.status.GetText()); got > leftW {
			t.Errorf("left content width %d exceeds reserved leftW %d (gap=%d)", got, leftW, statusPathGap)
		}
	})

	t.Run("threshold shows at minStatusWidthForPath", func(t *testing.T) {
		w := newTestWorkbench(t)
		w.SetHandlers(Handlers{GetWorkspaceRoot: func() string { return shortRoot }})
		sw := w.openWindow("s", "S")
		setWidth(sw, minStatusWidthForPath)
		if sw.statusPath == "" {
			t.Errorf("statusPath empty at W=%d (minStatusWidthForPath), want the path shown", minStatusWidthForPath)
		}
		// Invariant: showing the path always leaves the left content its floor.
		if leftW := minStatusWidthForPath - tui.StringWidth(sw.statusPath) - statusPathGap; leftW < minLeftWidth {
			t.Errorf("reserved leftW=%d < minLeftWidth=%d while a path is shown", leftW, minLeftWidth)
		}
	})

	t.Run("below threshold omits path", func(t *testing.T) {
		w := newTestWorkbench(t)
		w.SetHandlers(Handlers{GetWorkspaceRoot: func() string { return shortRoot }})
		sw := w.openWindow("s", "S")
		setWidth(sw, minStatusWidthForPath-1)
		if sw.statusPath != "" {
			t.Errorf("statusPath = %q below minStatusWidthForPath, want \"\" (pre-#551 render)", sw.statusPath)
		}
	})

	t.Run("long root is shortened within budget", func(t *testing.T) {
		longRoot := "/workspace/gogent/projects/agent/internal/pkg"
		w := newTestWorkbench(t)
		w.SetHandlers(Handlers{GetWorkspaceRoot: func() string { return longRoot }})
		sw := w.openWindow("s", "S")
		setWidth(sw, 60)
		if sw.statusPath == "" {
			t.Fatal("statusPath empty for a long root at a wide status, want a shortened path")
		}
		if tui.StringWidth(sw.statusPath) > pathBudget(60) {
			t.Errorf("statusPath %q width %d exceeds pathBudget(60)=%d",
				sw.statusPath, tui.StringWidth(sw.statusPath), pathBudget(60))
		}
		if !strings.Contains(sw.statusPath, "…") {
			t.Errorf("statusPath %q for a long root should contain an ellipsis", sw.statusPath)
		}
	})

	t.Run("unwired getter omits path at every width", func(t *testing.T) {
		w := newTestWorkbench(t) // no GetWorkspaceRoot
		sw := w.openWindow("s", "S")
		for _, width := range []int{0, 10, minStatusWidthForPath - 1, minStatusWidthForPath, 80, 200} {
			setWidth(sw, width)
			if sw.statusPath != "" {
				t.Fatalf("statusPath = %q at W=%d with no getter, want \"\" always", sw.statusPath, width)
			}
		}
	})
}

// --- headless render helpers ---

// statusRowCells reads the painted cells of the status row from the app buffer
// after a Redraw, together with the label's absolute bounds.
func statusRowCells(t *testing.T, w *Workbench, sw *SessionWindow) ([]tui.Cell, tv.Rect) {
	t.Helper()
	abs := sw.status.Component.AbsoluteBounds()
	if abs.W < 1 {
		t.Fatalf("status label not laid out: abs=%+v", abs)
	}
	cells := make([]tui.Cell, abs.W)
	for x := 0; x < abs.W; x++ {
		cells[x] = w.app.ReadCell(abs.X+x, abs.Y)
	}
	return cells, abs
}

func cellString(cells []tui.Cell) string {
	var b strings.Builder
	for _, c := range cells {
		b.WriteRune(c.Ch)
	}
	return b.String()
}

// allFG reports whether every cell in the slice carries the given foreground.
func allFG(cells []tui.Cell, fg tui.Color) bool {
	for _, c := range cells {
		if c.FG != fg {
			return false
		}
	}
	return true
}

// TestStatusLineRendersRightAlignedCyanPath is the core acceptance render test:
// the path paints flush-right in colorInfo, the left content paints in its
// severity colour, the two are separated by the reserved gap, and they never
// overlap.
func TestStatusLineRendersRightAlignedCyanPath(t *testing.T) {
	const root = "/proj"
	w := newTestWorkbench(t)
	w.SetHandlers(Handlers{GetWorkspaceRoot: func() string { return root }})
	w.app.Resize(120, 40)
	sw := w.openWindow("s", "S")
	w.desktop.Redraw()

	cells, abs := statusRowCells(t, w, sw)
	if sw.statusPath != root {
		t.Fatalf("statusPath = %q, want %q", sw.statusPath, root)
	}
	// Sanity: cyan is distinct from the idle grey so the affordance reads.
	if colorInfo == colorNote {
		t.Fatal("colorInfo must differ from colorNote for the path to stand out")
	}

	pw := tui.StringWidth(sw.statusPath)
	pathCells := cells[len(cells)-pw:]
	if got, want := cellString(pathCells), sw.statusPath; got != want {
		t.Errorf("painted path = %q, want %q", got, want)
	}
	if !allFG(pathCells, colorInfo) {
		t.Errorf("painted path FG = %v, want colorInfo (cyan) for all %d cells", pathCells, pw)
	}

	// Left content "idle" paints at the left edge in the idle/severity colour,
	// NOT colorInfo.
	left := sw.status.GetText()
	lw := tui.StringWidth(left)
	if lw < 1 {
		t.Fatalf("left content empty; status.Text=%q", sw.status.Text)
	}
	leftCells := cells[:lw]
	if got := cellString(leftCells); got != left {
		t.Errorf("painted left = %q, want %q", got, left)
	}
	wantLeftFG := statusColorFor(sw.busy, sw.background, sw.statusStats, sw.wb.budgetConfig())
	if !allFG(leftCells, wantLeftFG) {
		t.Errorf("painted left FG = %v, want severity colour (statusColorFor) %v", leftCells, wantLeftFG)
	}
	if allFG(leftCells, colorInfo) {
		t.Errorf("left content painted colorInfo in the idle state; it should be the dim idle colour")
	}

	// No overlap: left text + gap + path fit within the row.
	if lw+statusPathGap+pw > abs.W {
		t.Errorf("left(%d)+gap(%d)+path(%d) = %d exceeds status width %d (collision)",
			lw, statusPathGap, pw, lw+statusPathGap+pw, abs.W)
	}
}

// TestStatusLineRendersShortenedPath verifies a too-long root is shortened and
// still painted flush-right in colorInfo within the budget.
func TestStatusLineRendersShortenedPath(t *testing.T) {
	const root = "/workspace/gogent/projects/agent/internal/pkg"
	w := newTestWorkbench(t)
	w.SetHandlers(Handlers{GetWorkspaceRoot: func() string { return root }})
	w.app.Resize(100, 40)
	sw := w.openWindow("s", "S")
	w.desktop.Redraw()

	cells, abs := statusRowCells(t, w, sw)
	if sw.statusPath == "" {
		t.Fatal("statusPath empty for a long root, want a shortened path")
	}
	if sw.statusPath == root {
		t.Errorf("statusPath = full root %q, want it shortened to fit the budget", root)
	}
	if !strings.Contains(sw.statusPath, "…") {
		t.Errorf("shortened statusPath %q should carry an ellipsis", sw.statusPath)
	}
	pw := tui.StringWidth(sw.statusPath)
	if pw > pathBudget(abs.W) {
		t.Errorf("path width %d exceeds pathBudget(%d)=%d", pw, abs.W, pathBudget(abs.W))
	}
	pathCells := cells[len(cells)-pw:]
	if got := cellString(pathCells); got != sw.statusPath {
		t.Errorf("painted path = %q, want %q", got, sw.statusPath)
	}
	if !allFG(pathCells, colorInfo) {
		t.Errorf("shortened path not painted colorInfo: %v", pathCells)
	}
}

// TestStatusLineOmitsPathWhenNoHandler verifies the unwired getter leaves the
// pre-#551 render: no colorInfo path paints, and the left content uses the row.
func TestStatusLineOmitsPathWhenNoHandler(t *testing.T) {
	w := newTestWorkbench(t) // no GetWorkspaceRoot
	w.app.Resize(120, 40)
	sw := w.openWindow("s", "S")
	w.desktop.Redraw()

	cells, abs := statusRowCells(t, w, sw)
	if sw.statusPath != "" {
		t.Fatalf("statusPath = %q with no getter, want \"\"", sw.statusPath)
	}
	// No cyan path at the right edge.
	if c := cells[len(cells)-1]; c.FG == colorInfo {
		t.Errorf("rightmost status cell is colorInfo with no getter; no path should paint: %+v", c)
	}
	// Left content still paints at the left edge.
	left := sw.status.GetText()
	if got := cellString(cells[:tui.StringWidth(left)]); got != left {
		t.Errorf("painted left = %q, want %q", got, left)
	}
	// Without a path the full row width is available to the left content.
	if abs.W < minStatusWidthForPath {
		t.Errorf("status width %d unexpectedly below minStatusWidthForPath at 120 cols", abs.W)
	}
}

// TestStatusLinePathColourCollisionInBackgroundAccepted pins the documented
// behaviour: in the background-only state statusColorFor already colours the
// whole left line colorInfo, so the path matches it. The affordance must still
// render right-aligned without panicking — the collision is accepted, not a bug.
func TestStatusLinePathColourCollisionInBackgroundAccepted(t *testing.T) {
	const root = "/proj"
	w := newTestWorkbench(t)
	w.SetHandlers(Handlers{GetWorkspaceRoot: func() string { return root }})
	w.app.Resize(120, 40)
	sw := w.openWindow("s", "S")
	sw.background = true
	sw.busy = false
	sw.refreshStatus()
	w.desktop.Redraw()

	// Precondition: the left line is itself colorInfo in this state.
	if got := statusColorFor(sw.busy, sw.background, sw.statusStats, sw.wb.budgetConfig()); got != colorInfo {
		t.Fatalf("precondition: background-only left colour = %v, want colorInfo", got)
	}
	if sw.statusPath == "" {
		t.Fatal("statusPath empty in background state, want the path still shown")
	}

	cells, _ := statusRowCells(t, w, sw)
	pw := tui.StringWidth(sw.statusPath)
	pathCells := cells[len(cells)-pw:]
	if got := cellString(pathCells); got != sw.statusPath {
		t.Errorf("painted path = %q, want %q", got, sw.statusPath)
	}
	if !allFG(pathCells, colorInfo) {
		t.Errorf("background-state path not colorInfo: %v", pathCells)
	}
	// Left content is ALSO colorInfo here (the accepted collision).
	left := sw.status.GetText()
	leftCells := cells[:tui.StringWidth(left)]
	if !allFG(leftCells, colorInfo) {
		t.Errorf("background-state left content = %v, want colorInfo too (documented collision)", leftCells)
	}
}

// TestStatusLineTildeCollapseRendered proves the ~-collapse survives the full
// refreshStatus → DrawFn pipeline end-to-end by pinning $HOME for the test.
func TestStatusLineTildeCollapseRendered(t *testing.T) {
	t.Setenv("HOME", "/home/testuser")
	const root = "/home/testuser/code/gogent"
	w := newTestWorkbench(t)
	w.SetHandlers(Handlers{GetWorkspaceRoot: func() string { return root }})
	w.app.Resize(120, 40)
	sw := w.openWindow("s", "S")
	w.desktop.Redraw()

	if sw.statusPath != "~/code/gogent" {
		t.Fatalf("statusPath = %q, want ~/code/gogent (home-collapsed)", sw.statusPath)
	}
	cells, _ := statusRowCells(t, w, sw)
	pw := tui.StringWidth(sw.statusPath)
	if got := cellString(cells[len(cells)-pw:]); got != "~/code/gogent" {
		t.Errorf("painted path = %q, want ~/code/gogent", got)
	}
}

// TestStatusLineSeverityColoursPreserved is the criterion-3 guard: the path
// addition does not perturb the left-side severity colour logic. The left
// content still maps each state to its pre-#551 colour (pinned by value, not
// re-derived) while the path stays colorInfo throughout. Severity is driven
// through the channels refreshStatus actually reads — sw.statusStats and the
// workbench budget config — so the assertion exercises the real colour path.
func TestStatusLineSeverityColoursPreserved(t *testing.T) {
	const root = "/proj"
	for _, tc := range []struct {
		name       string
		busy       bool
		background bool
		stats      agent.SessionStats
		budget     config.BudgetConfig
		wantLeft   tui.Color
	}{
		{"idle", false, false, agent.SessionStats{}, config.BudgetConfig{}, colorNote},
		{"working", true, false, agent.SessionStats{}, config.BudgetConfig{}, colorAgent},
		{"context critical red", false, false, agent.SessionStats{ContextTokens: 85000, ContextWindow: 100000}, config.BudgetConfig{}, colorError},
		{"context warn amber", false, false, agent.SessionStats{ContextTokens: 65000, ContextWindow: 100000}, config.BudgetConfig{}, colorTool},
		{"budget exceeded red", false, false, agent.SessionStats{TokensIn: 600, TokensOut: 400}, config.BudgetConfig{TokenBudget: 1000}, colorError},
		{"background cyan", false, true, agent.SessionStats{}, config.BudgetConfig{}, colorInfo},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := newTestWorkbench(t)
			w.SetHandlers(Handlers{GetWorkspaceRoot: func() string { return root }})
			w.app.Resize(120, 40)
			sw := w.openWindow("s", "S")
			sw.wb.SetBudgetConfig(tc.budget)
			sw.busy = tc.busy
			sw.background = tc.background
			sw.statusStats = tc.stats
			sw.refreshStatus()
			w.desktop.Redraw()

			if sw.status.FG != tc.wantLeft {
				t.Fatalf("status.FG = %v, want %v for %s", sw.status.FG, tc.wantLeft, tc.name)
			}
			// The path is always colorInfo, independent of the left severity.
			if sw.statusPath == "" {
				t.Fatalf("statusPath empty, want the path shown alongside the %s colour", tc.name)
			}
			cells, _ := statusRowCells(t, w, sw)
			pw := tui.StringWidth(sw.statusPath)
			if !allFG(cells[len(cells)-pw:], colorInfo) {
				t.Errorf("path not colorInfo in %s state: %v", tc.name, cells[len(cells)-pw:])
			}
		})
	}
}

// TestRefreshStatusResetsStatusPathAcrossWidths guards the per-call reset of
// statusPath (refreshStatus clears it before recomputing). Reusing one window
// across a wide→narrow→wide sweep, the narrow step must clear the path the wide
// step set — a missing reset would leak a wide path onto a narrow row.
func TestRefreshStatusResetsStatusPathAcrossWidths(t *testing.T) {
	const root = "/proj"
	w := newTestWorkbench(t)
	w.SetHandlers(Handlers{GetWorkspaceRoot: func() string { return root }})
	sw := w.openWindow("s", "S")

	set := func(wd int) {
		sw.status.Component.SetBounds(tv.Rect{X: 0, Y: 0, W: wd, H: 1})
		sw.refreshStatus()
	}

	set(100)
	if sw.statusPath == "" {
		t.Fatal("wide: statusPath empty, want the path shown")
	}
	wide := sw.statusPath

	set(minStatusWidthForPath - 1) // narrow: below the threshold
	if sw.statusPath != "" {
		t.Errorf("narrow: statusPath = %q, want \"\" (reset must clear the wide path %q)", sw.statusPath, wide)
	}

	set(100)
	if sw.statusPath != wide {
		t.Errorf("wide again: statusPath = %q, want %q (recomputed after reset)", sw.statusPath, wide)
	}
}

// TestStatusLinePathSurvivesThemeSwitch is the criterion-3 regression guard for
// the design's reliance on reseedLabel NOT touching DrawFn: a live theme switch
// (ApplyTheme + Workbench.RefreshTheme) must keep the custom two-colour painter
// in place and recolour the path to the live colorInfo — not drop the path or
// leave it on the pre-switch colour.
func TestStatusLinePathSurvivesThemeSwitch(t *testing.T) {
	withThemeRestore(t) // ApplyTheme mutates the global colour vars; restore after
	const root = "/proj"
	w := newTestWorkbench(t)
	w.SetHandlers(Handlers{GetWorkspaceRoot: func() string { return root }})
	w.app.Resize(120, 40)
	sw := w.openWindow("s", "S")
	w.desktop.Redraw()
	if sw.statusPath == "" {
		t.Fatal("precondition: path should render before the theme switch")
	}
	before := colorInfo

	// Switch palettes the way the app does on a live theme change.
	truecolor := envOf(map[string]string{"TERM": "xterm", "COLORTERM": "truecolor"})
	ApplyTheme(ResolveTheme(config.ThemeConfig{Name: "high-contrast"}, truecolor, false))
	w.RefreshTheme() // reseeds labels + refreshStatus; must NOT reset DrawFn (Redraws)

	if sw.statusPath == "" {
		t.Fatal("statusPath cleared by theme switch; the path affordance must survive")
	}
	cells, _ := statusRowCells(t, w, sw)
	pw := tui.StringWidth(sw.statusPath)
	pathCells := cells[len(cells)-pw:]
	if got := cellString(pathCells); got != sw.statusPath {
		t.Errorf("post-switch painted path = %q, want %q (custom DrawFn dropped?)", got, sw.statusPath)
	}
	// The DrawFn reads the live colorInfo var, so the path must match whatever the
	// new theme installed.
	if !allFG(pathCells, colorInfo) {
		t.Errorf("post-switch path FG = %v, want live colorInfo %v (DrawFn must read the recoloured var)",
			pathCells, colorInfo)
	}
	// If the theme actually moved colorInfo, the paint must have moved with it —
	// proving a real recolour, not a stale pre-switch paint.
	if before != colorInfo && allFG(pathCells, before) {
		t.Errorf("path still painted in the pre-switch colorInfo %v after a live theme change", before)
	}
}

// TestStatusLineLongLeftContentCoexistsWithPath proves the reservation actually
// clips a long left content to make room for the path: at a narrow width with
// full stats the left line is bounded by leftW (not the full row), so it never
// reaches the right-aligned path.
func TestStatusLineLongLeftContentCoexistsWithPath(t *testing.T) {
	const root = "/proj"
	w := newTestWorkbench(t)
	w.SetHandlers(Handlers{GetWorkspaceRoot: func() string { return root }})
	sw := w.openWindow("s", "S")
	sw.statusState = "working..."
	sw.statusStats = agent.SessionStats{
		TokensIn: 12300, TokensOut: 4100, Turns: 7,
		ContextTokens: 38000, ContextWindow: 100000,
	}
	const W = 30
	sw.status.Component.SetBounds(tv.Rect{X: 0, Y: 0, W: W, H: 1})
	sw.refreshStatus()

	if sw.statusPath == "" {
		t.Fatal("statusPath empty at W=30 with full stats; the path should still reserve room")
	}
	pw := tui.StringWidth(sw.statusPath)
	leftW := W - pw - statusPathGap
	if leftW >= W {
		t.Errorf("no room reserved for the path: leftW=%d == W=%d", leftW, W)
	}
	left := sw.status.GetText()
	if lw := tui.StringWidth(left); lw > leftW {
		t.Errorf("left content %q (width %d) exceeds reserved leftW %d at W=%d — would collide with the path",
			left, lw, leftW, W)
	}
	if tui.StringWidth(left)+statusPathGap+pw > W {
		t.Errorf("left(%d)+gap(%d)+path(%d) > W(%d): collision",
			tui.StringWidth(left), statusPathGap, pw, W)
	}
}
