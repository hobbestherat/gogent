package ui

import (
	"strings"
	"testing"

	tui "github.com/hobbestherat/turbotui"
	tv "github.com/hobbestherat/turbotui/turbotv"
	"gogent/internal/config"
)

// Tests for issue #500: the always-visible, right-anchored connection/daemon status
// indicator on the menu bar, plus the right-aligned Daemon menu. They cover the four
// design gates — goal match (per-mode text, live updates, right-aligned menu, left
// menus unchanged), usability (presentational only, themed, visible), no regressions
// (no internal imports, nil-safety, menu order/mnemonics), and holistic (the seam
// reuses the merged turbotui MenuBar capability).
//
// One test below (TestIssue500IndicatorVisibleAgainstMenuBarBG /
// TestIssue500EmbeddedIndicatorCellIsVisible) intentionally FAILS against the current
// implementation: the embedded indicator is coloured with colorNote, which in the
// shipping default theme equals MenuBarBG (both ANSI 7), so "● embedded" renders
// grey-on-grey and is invisible. That is a real usability defect the suite exists to
// surface.

// menuLabels returns the top-level menu labels in declared order (mnemonics intact).
func menuLabels(bar *tv.MenuBar) []string {
	if bar == nil {
		return nil
	}
	out := make([]string, 0, len(bar.Menus))
	for _, m := range bar.Menus {
		out = append(out, m.Label)
	}
	return out
}

// rightAlignedLabels returns the labels of the top-level menus marked RightAligned.
func rightAlignedLabels(bar *tv.MenuBar) []string {
	if bar == nil {
		return nil
	}
	var out []string
	for _, m := range bar.Menus {
		if m.RightAligned {
			out = append(out, m.Label)
		}
	}
	return out
}

// --- pure indicator string --------------------------------------------------

func TestIssue500IndicatorTextPerModeAndPhase(t *testing.T) {
	cases := []struct {
		name  string
		mode  DaemonMode
		label string
		phase connPhase
		want  string
	}{
		{"embedded healthy", DaemonModeEmbedded, "", connHealthy, "● embedded"},
		{"attached-local healthy", DaemonModeAttachedLocal, "", connHealthy, "● daemon"},
		{"remote healthy with label", DaemonModeAttachedRemote, "ssh:user@host", connHealthy, "● ssh:user@host"},
		{"remote healthy label absent falls back", DaemonModeAttachedRemote, "", connHealthy, "● remote"},
		{"remote disconnected", DaemonModeAttachedRemote, "ssh:user@host", connDisconnected, "○ disconnected"},
		{"remote reconnecting", DaemonModeAttachedRemote, "ssh:user@host", connReconnecting, "○ reconnecting…"},
		// Restored is just healthy again.
		{"remote restored equals healthy", DaemonModeAttachedRemote, "ssh:user@host", connHealthy, "● ssh:user@host"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := daemonIndicatorText(tc.mode, tc.label, tc.phase)
			if got != tc.want {
				t.Errorf("daemonIndicatorText(%v, %q, %v) = %q, want %q", tc.mode, tc.label, tc.phase, got, tc.want)
			}
		})
	}
}

// TestIssue500IndicatorTextNeverDuplicatesAttemptOrRetry pins the presentational
// contract (gate 2): the indicator must NOT echo the reconnect attempt count or offer
// a Retry affordance — the blocking disconnect modal owns those.
func TestIssue500IndicatorTextNeverDuplicatesAttemptOrRetry(t *testing.T) {
	for _, phase := range []connPhase{connHealthy, connDisconnected, connReconnecting} {
		got := daemonIndicatorText(DaemonModeAttachedRemote, "ssh:user@host", phase)
		if strings.Contains(got, "attempt") {
			t.Errorf("indicator %q leaks the attempt count (modal owns it)", got)
		}
		if strings.Contains(got, "Retry") || strings.Contains(got, "retry") {
			t.Errorf("indicator %q offers a retry affordance (modal owns it)", got)
		}
	}
}

// TestIssue500IndicatorTextIgnoresPhaseWhenNotRemote verifies both defence layers: a
// stale disconnect phase can never leak a hollow ○ marker into embedded/attached-local
// mode (gate 1/3 — the phase is only meaningful for a remote attachment).
func TestIssue500IndicatorTextIgnoresPhaseWhenNotRemote(t *testing.T) {
	for _, mode := range []DaemonMode{DaemonModeEmbedded, DaemonModeAttachedLocal} {
		for _, phase := range []connPhase{connDisconnected, connReconnecting} {
			got := daemonIndicatorText(mode, "ssh:user@host", phase)
			if strings.Contains(got, "○") {
				t.Errorf("mode %v + stale phase %v leaked a disconnect marker: %q", mode, phase, got)
			}
		}
	}
	if got := daemonIndicatorText(DaemonModeEmbedded, "", connReconnecting); got != "● embedded" {
		t.Errorf("embedded+reconnecting = %q, want ● embedded", got)
	}
	if got := daemonIndicatorText(DaemonModeAttachedLocal, "", connDisconnected); got != "● daemon" {
		t.Errorf("local+disconnected = %q, want ● daemon", got)
	}
}

// --- colour contract --------------------------------------------------------

// TestIssue500IndicatorColorsContract locks the themed colour mapping (gate 2): green
// for a healthy attach, red for a drop, amber for an active retry, and a zero
// background so turbotui falls back to the bar's own MenuBarBG (the slot reads as part
// of the bar on every theme).
func TestIssue500IndicatorColorsContract(t *testing.T) {
	if fg, _ := daemonIndicatorColors(DaemonModeAttachedLocal, connHealthy); fg != colorAgent {
		t.Errorf("local healthy fg = %+v, want colorAgent (green)", fg)
	}
	if fg, _ := daemonIndicatorColors(DaemonModeAttachedRemote, connHealthy); fg != colorAgent {
		t.Errorf("remote healthy fg = %+v, want colorAgent (green)", fg)
	}
	if fg, _ := daemonIndicatorColors(DaemonModeAttachedRemote, connDisconnected); fg != colorError {
		t.Errorf("remote disconnected fg = %+v, want colorError (red)", fg)
	}
	if fg, _ := daemonIndicatorColors(DaemonModeAttachedRemote, connReconnecting); fg != colorTool {
		t.Errorf("remote reconnecting fg = %+v, want colorTool (amber)", fg)
	}
	for _, mode := range []DaemonMode{DaemonModeEmbedded, DaemonModeAttachedLocal, DaemonModeAttachedRemote} {
		for _, phase := range []connPhase{connHealthy, connDisconnected, connReconnecting} {
			if _, bg := daemonIndicatorColors(mode, phase); bg != (tui.Color{}) {
				t.Errorf("mode %v phase %v bg = %+v, want zero Color (fall back to MenuBarBG)", mode, phase, bg)
			}
		}
	}
}

// TestIssue500IndicatorVisibleAgainstMenuBarBG is the regression guard for a real
// usability defect (gate 2): an indicator whose foreground equals the menu bar
// background is invisible. The embedded state is the DEFAULT on a fresh run, so an
// invisible marker defeats the "always-visible" goal.
//
// This FAILS today: daemonIndicatorColors(DaemonModeEmbedded) returns colorNote, which
// in the shipping default theme equals MenuBarBG (both ANSI 7). The other states
// (green/red/amber) all contrast on the grey bar and pass.
func TestIssue500IndicatorVisibleAgainstMenuBarBG(t *testing.T) {
	withThemeRestore(t) // ApplyTheme below mutates global colour vars; keep the test hermetic
	// Install the shipping default theme so the package colour vars reflect what a
	// user actually sees on a fresh install.
	ApplyTheme(ResolveTheme(config.ThemeConfig{}, envOf(map[string]string{"TERM": "xterm"}), false))
	barBG := tv.DefaultTheme.MenuBarBG

	cases := []struct {
		name  string
		mode  DaemonMode
		phase connPhase
	}{
		{"embedded", DaemonModeEmbedded, connHealthy},
		{"attached-local", DaemonModeAttachedLocal, connHealthy},
		{"remote-healthy", DaemonModeAttachedRemote, connHealthy},
		{"remote-disconnected", DaemonModeAttachedRemote, connDisconnected},
		{"remote-reconnecting", DaemonModeAttachedRemote, connReconnecting},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fg, _ := daemonIndicatorColors(c.mode, c.phase)
			if fg == barBG {
				t.Errorf("indicator fg %+v == MenuBarBG %+v in the default theme: state %q is "+
					"invisible (grey-on-grey). Use a colour that contrasts with the bar (e.g. MenuBarFG).",
					fg, barBG, c.name)
			}
		})
	}
}

// --- rebuildMenu: status slot + right alignment + left order ----------------

func TestIssue500RebuildMenuStatusPerMode(t *testing.T) {
	cases := []struct {
		name  string
		mode  DaemonMode
		label string
		want  string
	}{
		{"embedded", DaemonModeEmbedded, "", "● embedded"},
		{"attached-local", DaemonModeAttachedLocal, "", "● daemon"},
		{"attached-remote", DaemonModeAttachedRemote, "ssh:user@host", "● ssh:user@host"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w := NewWorkbench([]*config.ModelConfig{{Name: "m", Model: "m"}})
			w.SetHandlers(Handlers{
				DaemonMode:      func() DaemonMode { return c.mode },
				ConnectionLabel: func() string { return c.label },
			})
			w.rebuildMenu()
			if w.menuBar == nil {
				t.Fatal("w.menuBar not retained after rebuildMenu")
			}
			if got := w.menuBar.StatusText; got != c.want {
				t.Errorf("StatusText = %q, want %q", got, c.want)
			}
		})
	}
}

// TestIssue500RebuildMenuRightAlignsDaemonOnly verifies gate 1: the Daemon menu is
// marked right-aligned, it keeps its place in declared order, and EVERY left
// navigation menu keeps its order + mnemonic and stays left-aligned.
func TestIssue500RebuildMenuRightAlignsDaemonOnly(t *testing.T) {
	w := NewWorkbench([]*config.ModelConfig{{Name: "m", Model: "m"}})
	w.SetHandlers(Handlers{
		DaemonMode:      func() DaemonMode { return DaemonModeAttachedLocal },
		ConnectionLabel: func() string { return "" },
	})
	w.rebuildMenu()
	if w.menuBar == nil {
		t.Fatal("w.menuBar not retained")
	}

	wantOrder := []string{"&File", "&Edit", "&Session", "&View", "&Config", "&Daemon", "&Help"}
	if got := menuLabels(w.menuBar); strings.Join(got, "|") != strings.Join(wantOrder, "|") {
		t.Errorf("top-level menu order = %v\nwant %v", got, wantOrder)
	}

	right := rightAlignedLabels(w.menuBar)
	if len(right) != 1 || right[0] != "&Daemon" {
		t.Errorf("right-aligned menus = %v, want exactly [&Daemon]", right)
	}
}

// TestIssue500RebuildMenuEmbeddedShowsIndicatorButNoDaemonMenu verifies the
// recommended no-wiring behaviour (gate 1): with DaemonMode == nil there is no Daemon
// menu and nothing is right-aligned, yet the indicator still shows "● embedded".
func TestIssue500RebuildMenuEmbeddedShowsIndicatorButNoDaemonMenu(t *testing.T) {
	w := NewWorkbench([]*config.ModelConfig{{Name: "m", Model: "m"}})
	// No daemon wiring: DaemonMode == nil, ConnectionLabel == nil.
	w.rebuildMenu()
	if w.menuBar == nil {
		t.Fatal("w.menuBar not retained")
	}
	if got := w.menuBar.StatusText; got != "● embedded" {
		t.Errorf("StatusText = %q, want ● embedded (pure embedded still shows the marker)", got)
	}
	for _, m := range w.menuBar.Menus {
		if m.Label == "&Daemon" {
			t.Errorf("&Daemon menu present with no daemon wiring; menus = %v", menuLabels(w.menuBar))
		}
		if m.RightAligned {
			t.Errorf("no menu should be right-aligned without daemon wiring; %q is", m.Label)
		}
	}
	// Left nav order is intact (no Daemon slot spliced in).
	want := []string{"&File", "&Edit", "&Session", "&View", "&Config", "&Help"}
	if got := menuLabels(w.menuBar); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("left nav order with no daemon = %v, want %v", got, want)
	}
}

// TestIssue500StatusReflectsModeFlipAfterHandoff verifies the indicator follows a
// Start/Stop handoff (gate 1): the controller swaps Handlers then rebuilds the menu,
// and the slot must re-derive from the new mode.
func TestIssue500StatusReflectsModeFlipAfterHandoff(t *testing.T) {
	w := NewWorkbench([]*config.ModelConfig{{Name: "m", Model: "m"}})
	mode := DaemonModeEmbedded
	w.SetHandlers(Handlers{
		DaemonMode:      func() DaemonMode { return mode },
		ConnectionLabel: func() string { return "" },
	})
	w.rebuildMenu()
	if w.menuBar.StatusText != "● embedded" {
		t.Fatalf("pre-handoff = %q, want ● embedded", w.menuBar.StatusText)
	}

	// Start handoff completes: Handlers swapped to attached-local, menu rebuilt.
	mode = DaemonModeAttachedLocal
	w.rebuildMenu()
	if w.menuBar.StatusText != "● daemon" {
		t.Errorf("post-Start = %q, want ● daemon", w.menuBar.StatusText)
	}

	// Stop handoff completes: back to embedded.
	mode = DaemonModeEmbedded
	w.rebuildMenu()
	if w.menuBar.StatusText != "● embedded" {
		t.Errorf("post-Stop = %q, want ● embedded", w.menuBar.StatusText)
	}
}

// --- live disconnect/reconnect refresh --------------------------------------

// TestIssue500RefreshOnDisconnectPhases drives the real Reconnector seam and asserts
// the slot updates in place (via refreshConnectionStatus) without a full menu rebuild:
// healthy → ○ disconnected → ○ reconnecting… → back to healthy.
func TestIssue500RefreshOnDisconnectPhases(t *testing.T) {
	w := NewWorkbench([]*config.ModelConfig{{Name: "m", Model: "m"}})
	w.SetHandlers(Handlers{
		DaemonMode:      func() DaemonMode { return DaemonModeAttachedRemote },
		ConnectionLabel: func() string { return "ssh:user@host" },
	})
	w.rebuildMenu()
	if w.menuBar.StatusText != "● ssh:user@host" {
		t.Fatalf("healthy = %q, want ● ssh:user@host", w.menuBar.StatusText)
	}

	// Fresh drop (attempt 1) → disconnected.
	w.OnConnectionLost(1)
	drainPostedEventually(t, w)
	if got := w.menuBar.StatusText; got != "○ disconnected" {
		t.Errorf("after OnConnectionLost(1) = %q, want ○ disconnected", got)
	}

	// Active backoff retry (attempt > 1) → reconnecting.
	w.OnConnectionLost(3)
	drainPostedEventually(t, w)
	if got := w.menuBar.StatusText; got != "○ reconnecting…" {
		t.Errorf("after OnConnectionLost(3) = %q, want ○ reconnecting…", got)
	}

	// Restored → healthy.
	w.OnConnectionRestored()
	drainPostedEventually(t, w)
	if got := w.menuBar.StatusText; got != "● ssh:user@host" {
		t.Errorf("after OnConnectionRestored = %q, want ● ssh:user@host", got)
	}
}

// TestIssue500RefreshLeavesModalResponsibleForAttempts confirms the refresh does not
// tear down or duplicate the blocking disconnect modal (gate 2/3): the modal still
// owns the attempt count, and restoring dismisses exactly one modal layer.
func TestIssue500RefreshLeavesModalResponsibleForAttempts(t *testing.T) {
	w := NewWorkbench([]*config.ModelConfig{{Name: "m", Model: "m"}})
	w.SetReconnectControls("ssh:user@host", func() {})
	w.SetHandlers(Handlers{
		DaemonMode:      func() DaemonMode { return DaemonModeAttachedRemote },
		ConnectionLabel: func() string { return "ssh:user@host" },
	})
	w.rebuildMenu()

	w.OnConnectionLost(3)
	drainPostedEventually(t, w)
	if w.disconnectLayer == nil {
		t.Fatal("disconnect modal not raised by OnConnectionLost")
	}
	if top := w.desktop.TopLayer(); top != w.disconnectLayer {
		t.Fatalf("top layer = %v, want the disconnect modal on top", top)
	}

	w.OnConnectionRestored()
	drainPostedEventually(t, w)
	if w.disconnectLayer != nil {
		t.Fatal("disconnect modal not dismissed by OnConnectionRestored")
	}
}

// --- error / edge handling --------------------------------------------------

// TestIssue500RefreshConnectionStatusNilSafeBeforeRebuild pins the guard: a refresh
// before the first rebuildMenu (menuBar == nil) must be a no-op, never a panic. The
// Reconnector hooks can fire before the menu is built in narrow startup orderings.
func TestIssue500RefreshConnectionStatusNilSafeBeforeRebuild(t *testing.T) {
	w := NewWorkbench([]*config.ModelConfig{{Name: "m", Model: "m"}})
	w.menuBar = nil             // force the pre-rebuild path
	w.refreshConnectionStatus() // must not panic
}

// TestIssue500ConnectionIndicatorForcesHealthyWhenNotRemote verifies a stale remote
// phase cannot leak into embedded/local via the workbench path: even with connPhase
// planted at connReconnecting, a non-remote mode shows its healthy marker.
func TestIssue500ConnectionIndicatorForcesHealthyWhenNotRemote(t *testing.T) {
	w := NewWorkbench([]*config.ModelConfig{{Name: "m", Model: "m"}})
	w.SetHandlers(Handlers{
		DaemonMode:      func() DaemonMode { return DaemonModeAttachedLocal },
		ConnectionLabel: func() string { return "ssh:ignored-in-local-mode" },
	})
	w.connPhase = connReconnecting
	w.rebuildMenu()
	if got := w.menuBar.StatusText; got != "● daemon" {
		t.Errorf("local with stale reconnecting phase = %q, want ● daemon", got)
	}
}

// TestIssue500ConnectionLabelNilSafeInEmbedded confirms a nil ConnectionLabel (the
// pure-embedded build) never panics and yields the embedded marker.
func TestIssue500ConnectionLabelNilSafeInEmbedded(t *testing.T) {
	w := NewWorkbench([]*config.ModelConfig{{Name: "m", Model: "m"}})
	w.SetHandlers(Handlers{
		DaemonMode:      func() DaemonMode { return DaemonModeEmbedded },
		ConnectionLabel: nil,
	})
	w.rebuildMenu()
	if got := w.menuBar.StatusText; got != "● embedded" {
		t.Errorf("embedded with nil ConnectionLabel = %q, want ● embedded", got)
	}
}

// --- render-based: the indicator is actually painted, right-anchored --------

// menuBarRow renders the frame and returns row 0 (the menu bar) as a string.
func menuBarRow(t *testing.T, w *Workbench) string {
	t.Helper()
	w.desktop.Redraw()
	var b strings.Builder
	for x := 0; x < w.app.Width(); x++ {
		ch := w.app.ReadCell(x, 0).Ch
		if ch == 0 {
			ch = ' '
		}
		b.WriteRune(ch)
	}
	return b.String()
}

// TestIssue500IndicatorRenderedRightAnchoredLeftOfDaemon proves the actual paint
// (gate 1): the status text is on the menu bar, right-anchored, with Help to the left
// of the right-aligned Daemon menu and Daemon to the left of the indicator.
func TestIssue500IndicatorRenderedRightAnchored(t *testing.T) {
	w := NewWorkbench([]*config.ModelConfig{{Name: "m", Model: "m"}})
	w.SetHandlers(Handlers{
		DaemonMode:      func() DaemonMode { return DaemonModeAttachedRemote },
		ConnectionLabel: func() string { return "ssh:user@host" },
	})
	w.rebuildMenu()

	row := menuBarRow(t, w)
	width := w.app.Width()

	status := "● ssh:user@host"
	statusIdx := strings.Index(row, status)
	if statusIdx < 0 {
		t.Fatalf("status indicator %q not rendered on the menu bar row:\n%s", status, row)
	}
	if statusIdx < width/2 {
		t.Errorf("indicator at col %d, expected right-anchored (width=%d):\n%s", statusIdx, width, row)
	}

	helpIdx := strings.Index(row, "Help")
	daemonIdx := strings.Index(row, "Daemon")
	if helpIdx < 0 {
		t.Errorf("Help menu not rendered on the bar:\n%s", row)
	}
	if daemonIdx < 0 {
		t.Errorf("Daemon menu not rendered on the bar:\n%s", row)
	}
	// Help (last left menu) must be left of Daemon (right group), which must be left
	// of the indicator.
	if helpIdx >= 0 && daemonIdx >= 0 && helpIdx >= daemonIdx {
		t.Errorf("expected Help left of Daemon; Help@%d Daemon@%d:\n%s", helpIdx, daemonIdx, row)
	}
	if daemonIdx >= 0 && daemonIdx >= statusIdx {
		t.Errorf("expected Daemon left of the indicator; Daemon@%d status@%d:\n%s", daemonIdx, statusIdx, row)
	}
}

// TestIssue500EmbeddedIndicatorCellIsVisible renders the default embedded indicator
// and checks the painted ● cell is actually readable (FG != BG). This is the render-
// level view of the defect guarded structurally above: with colorNote == MenuBarBG in
// the default theme, "● embedded" is painted grey-on-grey and is invisible to a user.
//
// This FAILS today for the same reason as TestIssue500IndicatorVisibleAgainstMenuBarBG.
func TestIssue500EmbeddedIndicatorCellIsVisible(t *testing.T) {
	withThemeRestore(t) // ApplyTheme below mutates global colour vars; keep the test hermetic
	ApplyTheme(ResolveTheme(config.ThemeConfig{}, envOf(map[string]string{"TERM": "xterm"}), false))
	w := NewWorkbench([]*config.ModelConfig{{Name: "m", Model: "m"}})
	w.rebuildMenu()
	w.desktop.Redraw()

	var found int
	for x := 0; x < w.app.Width(); x++ {
		c := w.app.ReadCell(x, 0)
		if c.Ch != '●' {
			continue
		}
		found++
		if c.FG == c.BG {
			t.Errorf("embedded ● at col %d has FG==BG (%+v): invisible grey-on-grey in the default theme", x, c.FG)
		}
	}
	if found == 0 {
		t.Fatal("● marker not rendered on the menu bar row")
	}
}

// TestIssue500IndicatorSurvivesAcrossRebuilds confirms a rebuild (e.g. after a session
// list change or theme switch) re-seeds the slot on the fresh bar — the indicator must
// not vanish after rebuildMenu builds a new MenuBar (regression risk noted in design).
func TestIssue500IndicatorSurvivesAcrossRebuilds(t *testing.T) {
	w := NewWorkbench([]*config.ModelConfig{{Name: "m", Model: "m"}})
	w.SetHandlers(Handlers{
		DaemonMode:      func() DaemonMode { return DaemonModeAttachedRemote },
		ConnectionLabel: func() string { return "ssh:user@host" },
	})
	w.rebuildMenu()
	first := w.menuBar
	if first.StatusText != "● ssh:user@host" {
		t.Fatalf("first rebuild StatusText = %q", first.StatusText)
	}
	// A second rebuild builds a brand-new bar; the slot must be reseeded there.
	w.rebuildMenu()
	if w.menuBar == first {
		t.Fatal("rebuildMenu did not build a fresh bar")
	}
	if got := w.menuBar.StatusText; got != "● ssh:user@host" {
		t.Errorf("second rebuild StatusText = %q, want reseeded ● ssh:user@host", got)
	}
	// And a subsequent in-place refresh targets the live bar.
	w.connPhase = connDisconnected
	w.refreshConnectionStatus()
	if got := w.menuBar.StatusText; got != "○ disconnected" {
		t.Errorf("refresh after rebuild = %q, want ○ disconnected", got)
	}
}

// indicatorCell renders an embedded workbench and returns the ● marker cell on the
// menu bar (row 0), or the zero Cell and found=false if it is not painted.
func indicatorCell(t *testing.T, w *Workbench) (c tui.Cell, found bool) {
	t.Helper()
	w.desktop.Redraw()
	for x := 0; x < w.app.Width(); x++ {
		if cell := w.app.ReadCell(x, 0); cell.Ch == '●' {
			return cell, true
		}
	}
	return tui.Cell{}, false
}

// TestIssue500IndicatorFollowsLiveThemeSwitch verifies gate 2 ("colours themed"): a
// live theme switch (ApplyTheme + the rebuild RefreshTheme performs) must reseed the
// menu bar, so the indicator cell is visible on EVERY theme and its painted background
// actually changes with the theme. It is fully observable (reads rendered cells) so it
// does not couple to turbotui's internal activeTheme plumbing.
func TestIssue500IndicatorFollowsLiveThemeSwitch(t *testing.T) {
	withThemeRestore(t) // cycles ApplyTheme across default/dark/high-contrast; restore after
	w := NewWorkbench([]*config.ModelConfig{{Name: "m", Model: "m"}})
	w.SetHandlers(Handlers{
		DaemonMode:      func() DaemonMode { return DaemonModeEmbedded },
		ConnectionLabel: func() string { return "" },
	})
	truecolor := envOf(map[string]string{"TERM": "xterm", "COLORTERM": "truecolor"})

	bgByTheme := make(map[string]tui.Color, 3)
	for _, cfg := range []string{"default", "dark", "high-contrast"} {
		t.Run(cfg, func(t *testing.T) {
			ApplyTheme(ResolveTheme(config.ThemeConfig{Name: cfg}, truecolor, false))
			w.rebuildMenu() // mirrors RefreshTheme's menu rebuild on a theme switch

			cell, found := indicatorCell(t, w)
			if !found {
				t.Fatalf("theme %q: ● marker not rendered on the menu bar", cfg)
			}
			if cell.FG == cell.BG {
				t.Errorf("theme %q: embedded ● invisible (FG==BG %+v) after a live switch", cfg, cell.FG)
			}
			bgByTheme[cfg] = cell.BG
		})
	}
	// The switch must have actually propagated to the painted bar: the default (grey)
	// and dark (black) bar backgrounds differ. Guards against a stale bar surviving a
	// theme change.
	if bgByTheme["default"] == bgByTheme["dark"] {
		t.Errorf("theme switch did not change the bar background: default BG %+v == dark BG %+v",
			bgByTheme["default"], bgByTheme["dark"])
	}
}

// TestIssue500LocalDisconnectKeepsHealthyMarker pins the per-spec behaviour that the
// hollow disconnect phase is REMOTE-only (gate 1): a local-socket drop still raises the
// blocking disconnect modal, but the indicator must not flip to a ○ marker in
// attached-local mode. It also confirms the modal UX is independent of the indicator.
func TestIssue500LocalDisconnectKeepsHealthyMarker(t *testing.T) {
	w := NewWorkbench([]*config.ModelConfig{{Name: "m", Model: "m"}})
	w.SetReconnectControls("local daemon", func() {})
	w.SetHandlers(Handlers{
		DaemonMode:      func() DaemonMode { return DaemonModeAttachedLocal },
		ConnectionLabel: func() string { return "" },
	})
	w.rebuildMenu()
	if got := w.menuBar.StatusText; got != "● daemon" {
		t.Fatalf("pre-disconnect = %q, want ● daemon", got)
	}

	// An attached-local run still owns a RemoteClient, so a socket drop fires the hook.
	w.OnConnectionLost(3)
	drainPostedEventually(t, w)

	if got := w.menuBar.StatusText; got != "● daemon" {
		t.Errorf("local-disconnect indicator = %q, want ● daemon (disconnect phase is remote-only)", got)
	}
	if w.disconnectLayer == nil {
		t.Error("disconnect modal was not raised on a local drop (modal UX must be independent of the indicator)")
	}

	// Restoring clears the modal; the local marker was already healthy throughout.
	w.OnConnectionRestored()
	drainPostedEventually(t, w)
	if w.disconnectLayer != nil {
		t.Error("disconnect modal not dismissed on restore")
	}
	if got := w.menuBar.StatusText; got != "● daemon" {
		t.Errorf("post-restore local indicator = %q, want ● daemon", got)
	}
}
