package ui

import (
	"io"
	"testing"

	"gogent/internal/agent"
	"gogent/internal/config"

	tui "github.com/hobbestherat/turbotui"
	tv "github.com/hobbestherat/turbotui/turbotv"
)

type issue379ThemeGlobals struct {
	colors     [9]tui.Color
	tvTheme    tv.Theme
	active     tv.Theme
	chrome     [5]tui.Color
	desktop    [2]tui.Color
	dropdown   [5]tui.Color
	noShadow   bool
	dialogHead tui.Color
	dialogBody tui.Color
}

func issue379SaveTheme(t *testing.T) {
	t.Helper()
	saved := issue379ThemeGlobals{
		colors:     snapshotColors(),
		tvTheme:    tv.DefaultTheme,
		active:     tv.ActiveTheme(),
		chrome:     [5]tui.Color{chromePanelFG, chromePanelBG, chromeTitle, chromeDivider, chromeAccent},
		desktop:    [2]tui.Color{chromeDesktopFG, chromeDesktopBG},
		dropdown:   [5]tui.Color{dropdownFG, dropdownBG, dropdownFocusFG, dropdownFocusBG, dropdownDisabledFG},
		noShadow:   shadowsEnabled,
		dialogHead: colorDialogHeader,
		dialogBody: colorDialogDetail,
	}
	t.Cleanup(func() {
		restoreColors(saved.colors)
		tv.DefaultTheme = saved.tvTheme
		tv.SetTheme(saved.active)
		chromePanelFG, chromePanelBG, chromeTitle, chromeDivider, chromeAccent =
			saved.chrome[0], saved.chrome[1], saved.chrome[2], saved.chrome[3], saved.chrome[4]
		chromeDesktopFG, chromeDesktopBG = saved.desktop[0], saved.desktop[1]
		dropdownFG, dropdownBG, dropdownFocusFG, dropdownFocusBG, dropdownDisabledFG =
			saved.dropdown[0], saved.dropdown[1], saved.dropdown[2], saved.dropdown[3], saved.dropdown[4]
		shadowsEnabled = saved.noShadow
		colorDialogHeader, colorDialogDetail = saved.dialogHead, saved.dialogBody
	})
}

func issue379ResolveTheme(name string) Theme {
	return ResolveTheme(config.ThemeConfig{Name: name}, envOf(map[string]string{
		"TERM":      "xterm",
		"COLORTERM": "truecolor",
	}), false)
}

func issue379NewWorkbench() *Workbench {
	w := NewWorkbench([]*config.ModelConfig{{Name: "m", DisplayName: "Model", Model: "m"}})
	w.app = tui.NewWithSize(80, 25, io.Discard)
	w.desktop = tv.NewDesktop(w.app)
	w.sidebar = newSidebar(w)
	w.sidebar.reposition(w.app.Width(), w.app.Height())
	w.desktop.AddLayer(w.sidebar.layer)
	return w
}

func issue379RenderSidebar(t *testing.T, w *Workbench) {
	t.Helper()
	w.sidebar.addSession("s1", "Session 1", true)
	w.sidebar.applySubAgent("s1", agent.SessionEvent{
		AgentID: "a1",
		Name:    "Agent 1",
		Status:  agent.StatusRunning,
		Kind:    agent.KindInteractive,
	})
	w.sidebar.applyTodo("s1", []agent.TodoItem{{Content: "first", Status: agent.TodoPending}})
	w.sidebar.focusSession("s1")
	w.sidebar.reposition(w.app.Width(), w.app.Height())
	w.desktop.Redraw()
}

func issue379AssertCell(t *testing.T, w *Workbench, name string, x, y int, wantFG, wantBG tui.Color) {
	t.Helper()
	cell := w.app.ReadCell(x, y)
	if cell.FG != wantFG || cell.BG != wantBG {
		t.Fatalf("%s cell (%d,%d) = ch %q FG %+v BG %+v, want FG %+v BG %+v",
			name, x, y, cell.Ch, cell.FG, cell.BG, wantFG, wantBG)
	}
}

func TestIssue379SidebarRenderedChromeFollowsResolvedPresets(t *testing.T) {
	issue379SaveTheme(t)

	for _, tc := range []struct {
		name  string
		theme string
	}{
		{name: "default", theme: themeDefault},
		{name: "dark", theme: themeDark},
		{name: "high-contrast", theme: themeHighContrast},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resolved := issue379ResolveTheme(tc.theme)
			ApplyTheme(resolved)
			w := issue379NewWorkbench()
			issue379RenderSidebar(t, w)

			left := w.sidebar.panel.AbsoluteBounds().X
			// Header/title and surrounding blank panel fill read the gogent chrome roles
			// directly at draw time.
			issue379AssertCell(t, w, "header title", left+2, 1, resolved.Title, resolved.PanelBG)
			issue379AssertCell(t, w, "header fill", left+20, 1, resolved.PanelFG, resolved.PanelBG)
			issue379AssertCell(t, w, "divider grip", left, 1, resolved.Accent, resolved.PanelBG)
			issue379AssertCell(t, w, "pin toggle", left+defaultSidebarWidth-2, 1, resolved.Accent, resolved.PanelBG)

			// The tree is the frozen-widget path that caused #379. The focused
			// session row is the tree's unfocused selection bar; child rows and empty
			// row fill use WindowFG/WindowBG. Both paths must be reseeded.
			issue379AssertCell(t, w, "session selected row text", left+2, 2, resolved.WindowFG, w.sidebar.tree.SelBGUnfocused)
			issue379AssertCell(t, w, "session selected row fill", left+20, 2, resolved.WindowFG, w.sidebar.tree.SelBGUnfocused)
			issue379AssertCell(t, w, "agent row text", left+4, 3, resolved.WindowFG, resolved.WindowBG)

			// TODO and Overall bands are drawn by the sidebar panel itself.
			abs := w.sidebar.panel.AbsoluteBounds()
			todoTop := abs.Y + abs.H - w.sidebar.overallBandH - w.sidebar.todosBandH
			overallTop := abs.Y + abs.H - w.sidebar.overallBandH
			overallTitleY := overallTop + overallSeparatorLines + overallSelectorLines
			issue379AssertCell(t, w, "todos title", left+2, todoTop, resolved.Title, resolved.PanelBG)
			issue379AssertCell(t, w, "todo row", left+2, todoTop+todoRegionTitleLines, resolved.PanelFG, resolved.PanelBG)
			issue379AssertCell(t, w, "overall separator", left+1, overallTop, resolved.Divider, resolved.PanelBG)
			issue379AssertCell(t, w, "overall title", left+2, overallTitleY, resolved.Title, resolved.PanelBG)

			if tc.theme == themeDark {
				if got := w.app.ReadCell(left+2, 1).BG; got == tui.ANSIColor(4) {
					t.Fatalf("dark sidebar header background stayed default ANSI-4 blue")
				}
				if got := w.app.ReadCell(left+20, 2).BG; got == tui.ANSIColor(4) {
					t.Fatalf("dark sidebar tree row background stayed default ANSI-4 blue")
				}
			}
		})
	}
}

func TestIssue379WorkbenchRefreshThemeReseedsSidebarFrozenWidgets(t *testing.T) {
	issue379SaveTheme(t)
	ApplyTheme(issue379ResolveTheme(themeDefault))
	w := issue379NewWorkbench()

	if w.sidebar.tree.BG != tv.ActiveTheme().WindowBG {
		t.Fatalf("setup: sidebar tree BG = %+v, want default active WindowBG %+v", w.sidebar.tree.BG, tv.ActiveTheme().WindowBG)
	}

	resolved := issue379ResolveTheme(themeDark)
	ApplyTheme(resolved)
	w.RefreshTheme()

	if w.sidebar.tree.FG != tv.ActiveTheme().WindowFG || w.sidebar.tree.BG != tv.ActiveTheme().WindowBG {
		t.Fatalf("sidebar tree was not reseeded by Workbench.RefreshTheme: FG/BG = %+v/%+v, want %+v/%+v",
			w.sidebar.tree.FG, w.sidebar.tree.BG, tv.ActiveTheme().WindowFG, tv.ActiveTheme().WindowBG)
	}
	if w.sidebar.tree.SelFG != tv.ActiveTheme().SelectionFG || w.sidebar.tree.SelBG != tv.ActiveTheme().SelectionBG {
		t.Fatalf("sidebar tree selection was not reseeded by Workbench.RefreshTheme: FG/BG = %+v/%+v, want %+v/%+v",
			w.sidebar.tree.SelFG, w.sidebar.tree.SelBG, tv.ActiveTheme().SelectionFG, tv.ActiveTheme().SelectionBG)
	}
	if w.sidebar.overallSelect.FG != dropdownFG || w.sidebar.overallSelect.BG != dropdownBG {
		t.Fatalf("sidebar Overall selector was not reseeded by Workbench.RefreshTheme: FG/BG = %+v/%+v, want %+v/%+v",
			w.sidebar.overallSelect.FG, w.sidebar.overallSelect.BG, dropdownFG, dropdownBG)
	}

	issue379RenderSidebar(t, w)
	left := w.sidebar.panel.AbsoluteBounds().X
	issue379AssertCell(t, w, "live dark header title", left+2, 1, resolved.Title, resolved.PanelBG)
	issue379AssertCell(t, w, "live dark selected tree row", left+20, 2, resolved.WindowFG, w.sidebar.tree.SelBGUnfocused)
	if got := w.app.ReadCell(left+20, 2).BG; got == tui.ANSIColor(4) {
		t.Fatalf("live dark sidebar tree row stayed default ANSI-4 blue after Workbench.RefreshTheme")
	}
}
