package ui

import (
	"testing"

	"gogent/internal/config"

	tui "github.com/hobbestherat/turbotui"
	tv "github.com/hobbestherat/turbotui/turbotv"
)

// This file tests issue #215: a persisted, live-applied "Disable shadows" setting
// that removes drop shadows from every surface gogent builds — session/monologue
// windows, dialogs, the menu bar and buttons. The three behaviours the issue's
// acceptance criteria require are each covered:
//
//   - config round-trip of NoShadow — see internal/config/theme_noshadow_test.go.
//   - toggling it clears the Shadow flag on windows/menus/buttons — here, at
//     construction time, via the live refreshTheme/RefreshTheme path, and for the
//     newButton/apply*Shadow helpers directly.
//   - default keeps shadows — every "NoShadow" test is paired with a "default"
//     counterpart asserting Shadow stays true.
//
// The design (plan.md): ApplyTheme installs the resolved Theme.NoShadow into a
// package var `shadowsEnabled` (true = shadows on); every surface seeds its
// turbotui Shadow flag from it via applyWindowShadow / applyButtonShadow /
// applyMenuBarShadow and the newButton wrapper, both at construction and on the
// live theme-apply path (#204's ApplyTheme; RefreshTheme chain) so a toggle takes
// effect without a restart.

// issue215RestoreTheme snapshots the theme globals (including shadowsEnabled) for
// the test and restores them on cleanup. The shared snapshot now covers
// shadowsEnabled (see TestIssue215SnapshotRestoresShadowsEnabled), so
// issue204RestoreTheme already restores it; this helper additionally pins it
// explicitly as belt-and-suspenders so these tests stay hermetic even if the
// shared snapshot regresses.
func issue215RestoreTheme(t *testing.T) {
	t.Helper()
	issue204RestoreTheme(t)
	saved := shadowsEnabled
	t.Cleanup(func() { shadowsEnabled = saved })
}

// resolve215 resolves a ThemeConfig at truecolor fidelity (the form ApplyTheme
// consumes), carrying NoShadow through.
func resolve215(cfg config.ThemeConfig) Theme {
	return ResolveTheme(cfg, truecolorEnv, false)
}

// noShadowTheme is the default palette with NoShadow on (shadows off).
func noShadowTheme() Theme { return resolve215(config.ThemeConfig{NoShadow: true}) }

// defaultShadowTheme is the default palette with shadows on (the unchanged default).
func defaultShadowTheme() Theme { return resolve215(config.ThemeConfig{}) }

// newTestMenuBar builds a minimal menu bar for helper-level shadow assertions
// (turbotui's desktop stores its menu bar in an unexported field with no getter,
// so the live menu-bar flag is asserted here at the helper level gogent controls).
func newTestMenuBar() *tv.MenuBar {
	return tv.NewMenuBar(tv.Rect{X: 0, Y: 0, W: 40, H: 1},
		tv.NewSubMenu("File", tv.NewMenuItem("x", nil)))
}

// --------------------------------------------------------------------------
// Test isolation: the shared theme-global snapshot must cover shadowsEnabled.
// --------------------------------------------------------------------------

// TestIssue215SnapshotRestoresShadowsEnabled proves the SHARED theme-global
// snapshot/restore (snapshotThemeGlobals/restoreThemeGlobals, the machinery
// withThemeRestore wires up and the whole suite relies on) captures the issue-215
// shadowsEnabled package var. Without it, any test that flips NoShadow via
// ApplyTheme leaks shadowsEnabled=false into later tests — a latent cross-test
// contamination defect that was present in the #215 PR's own test diff (the driver
// added the global but did not extend the snapshot). This test fails against the
// unextended snapshot and passes once shadowsEnabled is restored.
func TestIssue215SnapshotRestoresShadowsEnabled(t *testing.T) {
	ApplyTheme(defaultShadowTheme())
	before := shadowsEnabled // true (default keeps shadows on)
	if !before {
		t.Fatalf("setup: shadowsEnabled = false under default theme, want true")
	}

	saved := snapshotThemeGlobals()
	ApplyTheme(noShadowTheme()) // flips shadowsEnabled to false
	if shadowsEnabled == before {
		t.Fatalf("setup: ApplyTheme(NoShadow) did not flip shadowsEnabled (still %v)", shadowsEnabled)
	}

	restoreThemeGlobals(saved)
	if shadowsEnabled != before {
		t.Fatalf("restoreThemeGlobals did not restore shadowsEnabled: got %v, want %v — the shared snapshot leaks the #215 global across tests", shadowsEnabled, before)
	}
}

// --------------------------------------------------------------------------
// ResolveTheme carries NoShadow through to the resolved Theme.
// --------------------------------------------------------------------------

// TestIssue215ResolveThemeCarriesNoShadow checks ResolveTheme copies the config's
// NoShadow into the resolved Theme for every preset, so ApplyTheme can install it.
// A missing copy would leave Theme.NoShadow always false and the toggle inert.
func TestIssue215ResolveThemeCarriesNoShadow(t *testing.T) {
	for _, name := range []string{"", themeDefault, themeHighContrast, themeDark} {
		if got := ResolveTheme(config.ThemeConfig{Name: name, NoShadow: true}, truecolorEnv, false); !got.NoShadow {
			t.Errorf("ResolveTheme(name=%q, NoShadow:true).NoShadow = false, want true", name)
		}
		if got := ResolveTheme(config.ThemeConfig{Name: name}, truecolorEnv, false); got.NoShadow {
			t.Errorf("ResolveTheme(name=%q).NoShadow = true, want false", name)
		}
	}
}

// --------------------------------------------------------------------------
// ApplyTheme installs NoShadow into shadowsEnabled; the helpers read it.
// --------------------------------------------------------------------------

// TestIssue215ApplyThemeTogglesShadows proves ApplyTheme is the single switch:
// under a NoShadow theme, a freshly-built window, menu bar and button all end up
// Shadow=false; under the default theme they are Shadow=true. The "default" case
// also guards that the helpers actually run (turbotui seeds Shadow=true itself, so
// a helper that did nothing would pass the NoShadow case vacuously).
func TestIssue215ApplyThemeTogglesShadows(t *testing.T) {
	t.Run("turbotui seeds Shadow=true (sanity)", func(t *testing.T) {
		issue215RestoreTheme(t)
		w := tv.NewWindow("t", tv.Rect{X: 0, Y: 0, W: 10, H: 5}, tui.LineSingle)
		if !w.Shadow {
			t.Errorf("fresh tv.Window.Shadow = false, want true (sanity)")
		}
		if b := tv.NewButton("ok", tv.Rect{X: 0, Y: 0, W: 4, H: 1}, nil); !b.Shadow {
			t.Errorf("fresh tv.Button.Shadow = false, want true (sanity)")
		}
		if bar := newTestMenuBar(); !bar.Shadow {
			t.Errorf("fresh tv.MenuBar.Shadow = false, want true (sanity)")
		}
	})

	t.Run("NoShadow clears window, button and menu bar flags", func(t *testing.T) {
		issue215RestoreTheme(t)
		ApplyTheme(noShadowTheme())

		w := tv.NewWindow("t", tv.Rect{X: 0, Y: 0, W: 10, H: 5}, tui.LineSingle)
		applyWindowShadow(w)
		if w.Shadow {
			t.Errorf("window.Shadow = true under NoShadow, want false")
		}
		b := newButton("ok", tv.Rect{X: 0, Y: 0, W: 4, H: 1}, nil)
		if b.Shadow {
			t.Errorf("newButton.Shadow = true under NoShadow, want false")
		}
		bar := newTestMenuBar()
		applyMenuBarShadow(bar)
		if bar.Shadow {
			t.Errorf("menu bar.Shadow = true under NoShadow, want false")
		}
	})

	t.Run("default keeps window, button and menu bar flags on", func(t *testing.T) {
		issue215RestoreTheme(t)
		ApplyTheme(defaultShadowTheme())

		w := tv.NewWindow("t", tv.Rect{X: 0, Y: 0, W: 10, H: 5}, tui.LineSingle)
		applyWindowShadow(w)
		if !w.Shadow {
			t.Errorf("window.Shadow = false under default theme, want true")
		}
		b := newButton("ok", tv.Rect{X: 0, Y: 0, W: 4, H: 1}, nil)
		if !b.Shadow {
			t.Errorf("newButton.Shadow = false under default theme, want true")
		}
		bar := newTestMenuBar()
		applyMenuBarShadow(bar)
		if !bar.Shadow {
			t.Errorf("menu bar.Shadow = false under default theme, want true")
		}
	})
}

// TestIssue215ShadowHelpersAreNilSafe ensures the helpers tolerate a nil widget
// (defensive guards), so a partially-built surface or a read-only window without
// input buttons cannot panic the apply path. The implementation guards each with a
// nil check; this pins it.
func TestIssue215ShadowHelpersAreNilSafe(t *testing.T) {
	issue215RestoreTheme(t)
	// Must not panic under either preference.
	for _, th := range []Theme{noShadowTheme(), defaultShadowTheme()} {
		ApplyTheme(th)
		applyWindowShadow(nil)
		applyButtonShadow(nil)
		applyMenuBarShadow(nil)
	}
}

// --------------------------------------------------------------------------
// Construction-time surfaces honour the preference (shadows off from the start).
// --------------------------------------------------------------------------

// TestIssue215ConstructionHonoursNoShadow verifies a fully-built session window —
// the frame plus all four input-row buttons (Send/Queue/Interject/Stop) — seeds
// Shadow=false from a NoShadow theme at construction, and Shadow=true under the
// default theme. newButton is the wrapper every gogent button goes through, so all
// four must agree.
func TestIssue215ConstructionHonoursNoShadow(t *testing.T) {
	buttons := func(sw *SessionWindow) []*tv.Button {
		return []*tv.Button{sw.sendButton, sw.queueButton, sw.interjectButton, sw.stopButton}
	}

	t.Run("NoShadow clears window and buttons", func(t *testing.T) {
		issue215RestoreTheme(t)
		ApplyTheme(noShadowTheme())

		w := newTestWorkbench(t)
		sw := w.openWindow("s", "S")
		if sw.window.Shadow {
			t.Errorf("session window.Shadow = true, want false under NoShadow")
		}
		for i, b := range buttons(sw) {
			if b.Shadow {
				t.Errorf("input button %d Shadow = true, want false under NoShadow", i)
			}
		}
	})

	t.Run("default keeps window and button shadows", func(t *testing.T) {
		issue215RestoreTheme(t)
		ApplyTheme(defaultShadowTheme())

		w := newTestWorkbench(t)
		sw := w.openWindow("s", "S")
		if !sw.window.Shadow {
			t.Errorf("session window.Shadow = false, want true by default")
		}
		for i, b := range buttons(sw) {
			if !b.Shadow {
				t.Errorf("input button %d Shadow = false, want true by default", i)
			}
		}
	})
}

// TestIssue215DialogSurfaceHonoursNoShadow covers the dialog surface (one of the
// four the issue names): every dialog site does applyWindowShadow(dialog.Window)
// right after tv.NewDialog. A fresh dialog under NoShadow must end up Shadow=false,
// and Shadow=true under the default theme. (Dialogs are transient, so this is a
// construction-time check rather than a live-refresh one.)
func TestIssue215DialogSurfaceHonoursNoShadow(t *testing.T) {
	t.Run("NoShadow clears dialog window", func(t *testing.T) {
		issue215RestoreTheme(t)
		ApplyTheme(noShadowTheme())
		d := tv.NewDialog("D", 0, 0, 30, 10)
		applyWindowShadow(d.Window)
		if d.Window.Shadow {
			t.Errorf("dialog.Window.Shadow = true under NoShadow, want false")
		}
	})
	t.Run("default keeps dialog shadow", func(t *testing.T) {
		issue215RestoreTheme(t)
		ApplyTheme(defaultShadowTheme())
		d := tv.NewDialog("D", 0, 0, 30, 10)
		applyWindowShadow(d.Window)
		if !d.Window.Shadow {
			t.Errorf("dialog.Window.Shadow = false under default theme, want true")
		}
	})
}

// --------------------------------------------------------------------------
// Live re-apply (no restart): refreshTheme flips Shadow on the frame + buttons.
// --------------------------------------------------------------------------

// TestIssue215RefreshThemeReappliesShadowLive is the core live-toggle test. A
// window built with shadows ON, then switched to NoShadow and refreshed, flips
// Shadow to false on the frame and every button WITHOUT a rebuild — and switching
// back restores them. This is the window half of the exact chain the runtime
// SetTheme handler runs (ApplyTheme; sw.refreshTheme).
func TestIssue215RefreshThemeReappliesShadowLive(t *testing.T) {
	issue215RestoreTheme(t)
	ApplyTheme(defaultShadowTheme())

	w := newTestWorkbench(t)
	sw := w.openWindow("s", "S")

	buttons := []*tv.Button{sw.sendButton, sw.queueButton, sw.interjectButton, sw.stopButton}
	assertShadows := func(label string, wantWindow, wantButtons bool) {
		t.Helper()
		if sw.window.Shadow != wantWindow {
			t.Errorf("%s: window.Shadow = %v, want %v", label, sw.window.Shadow, wantWindow)
		}
		for i, b := range buttons {
			if b.Shadow != wantButtons {
				t.Errorf("%s: input button %d Shadow = %v, want %v", label, i, b.Shadow, wantButtons)
			}
		}
	}

	// Precondition: shadows on at construction.
	assertShadows("construction", true, true)

	// Toggle shadows OFF live, then refresh.
	ApplyTheme(noShadowTheme())
	sw.refreshTheme()
	assertShadows("after disabling", false, false)

	// Toggle back ON live, then refresh — symmetric round-trip.
	ApplyTheme(defaultShadowTheme())
	sw.refreshTheme()
	assertShadows("after re-enabling", true, true)
}

// TestIssue215RefreshThemeKeepsStopButtonErrorRed confirms the shadow re-seed does
// not clobber the gogent accent applied right after it in refreshTheme: the Stop
// button keeps its error-red foreground even though its Shadow flag now follows the
// preference. (A regression here would mean reseedButton/applyButtonShadow was
// reordered ahead of the accent override, or vice-versa.)
func TestIssue215RefreshThemeKeepsStopButtonErrorRed(t *testing.T) {
	issue215RestoreTheme(t)
	ApplyTheme(defaultShadowTheme())

	w := newTestWorkbench(t)
	sw := w.openWindow("s", "S")

	ApplyTheme(noShadowTheme())
	sw.refreshTheme()

	if sw.stopButton.Shadow {
		t.Errorf("stop button Shadow = true under NoShadow, want false")
	}
	if sw.stopButton.FG != colorError || sw.stopButton.FocusFG != colorError {
		t.Errorf("stop button lost its error-red accent after refresh: FG=%+v FocusFG=%+v, want %+v",
			sw.stopButton.FG, sw.stopButton.FocusFG, colorError)
	}
}

// TestIssue215RefreshThemeReadOnlyWindowFrame checks a read-only analysis window
// (no input chrome) still re-applies the shadow toggle to its frame via the live
// path — refreshTheme runs applyWindowShadow before the read-only early return.
func TestIssue215RefreshThemeReadOnlyWindowFrame(t *testing.T) {
	issue215RestoreTheme(t)
	ApplyTheme(defaultShadowTheme())

	w := newTestWorkbench(t)
	ro := newSessionWindow(w, "analysis-1", "Saved", tv.Rect{}, true)
	if !ro.window.Shadow {
		t.Fatalf("read-only window.Shadow = false at construction, want true")
	}

	ApplyTheme(noShadowTheme())
	ro.refreshTheme()
	if ro.window.Shadow {
		t.Errorf("read-only window.Shadow = true after refresh under NoShadow, want false")
	}

	ApplyTheme(defaultShadowTheme())
	ro.refreshTheme()
	if !ro.window.Shadow {
		t.Errorf("read-only window.Shadow = false after re-enabling, want true")
	}
}

// --------------------------------------------------------------------------
// Live re-apply (no restart): Workbench.RefreshTheme flips Shadow across windows.
// --------------------------------------------------------------------------

// TestIssue215WorkbenchRefreshThemeAppliesAcrossWindows runs the full live path
// the SetTheme handler uses (ApplyTheme; wb.RefreshTheme) and checks every OPEN
// session window's frame and buttons flip together, not just the active one — and
// that switching back re-enables them. (The menu bar is rebuilt by RefreshTheme via
// rebuildMenu, which calls applyMenuBarShadow; turbotui exposes no getter for the
// desktop's bar, so the menu flag itself is covered by the helper test above.)
func TestIssue215WorkbenchRefreshThemeAppliesAcrossWindows(t *testing.T) {
	issue215RestoreTheme(t)
	ApplyTheme(defaultShadowTheme())

	w := newTestWorkbench(t)
	sws := []*SessionWindow{w.openWindow("a", "A"), w.openWindow("b", "B")}

	ApplyTheme(noShadowTheme())
	w.RefreshTheme()
	for _, sw := range sws {
		if sw.window.Shadow {
			t.Errorf("window %q Shadow = true after RefreshTheme under NoShadow, want false", sw.title)
		}
		for i, b := range []*tv.Button{sw.sendButton, sw.queueButton, sw.interjectButton, sw.stopButton} {
			if b.Shadow {
				t.Errorf("window %q button %d Shadow = true, want false", sw.title, i)
			}
		}
	}

	// Switch back: shadows re-enabled across all windows.
	ApplyTheme(defaultShadowTheme())
	w.RefreshTheme()
	for _, sw := range sws {
		if !sw.window.Shadow {
			t.Errorf("window %q Shadow = false after re-enabling, want true", sw.title)
		}
	}
}

// TestIssue215WorkbenchRefreshThemeEmptyNoPanic checks RefreshTheme is safe with no
// open sessions when NoShadow is on (the rebuildMenu + empty window loop + redraw
// must not panic).
func TestIssue215WorkbenchRefreshThemeEmptyNoPanic(t *testing.T) {
	issue215RestoreTheme(t)
	ApplyTheme(noShadowTheme())

	w := newTestWorkbench(t)
	w.RefreshTheme() // must not panic
	if got := len(w.sessions); got != 0 {
		t.Errorf("expected zero sessions, got %d", got)
	}
}

// --------------------------------------------------------------------------
// Orthogonality: NoShadow is independent of NoColor at runtime.
// --------------------------------------------------------------------------

// TestIssue215NoShadowIndependentOfNoColorAtApply confirms the two preferences are
// orthogonal: a NO_COLOR-only theme (NoShadow unset) keeps shadows ON, and a theme
// with both still drops shadows. NO_COLOR must not imply NoShadow (the issue scopes
// them as separate settings).
func TestIssue215NoShadowIndependentOfNoColorAtApply(t *testing.T) {
	issue215RestoreTheme(t)

	// NO_COLOR alone must keep shadows on.
	ApplyTheme(resolve215(config.ThemeConfig{NoColor: true}))
	if b := newButton("ok", tv.Rect{X: 0, Y: 0, W: 4, H: 1}, nil); !b.Shadow {
		t.Errorf("newButton.Shadow = false under NO_COLOR-only, want true (NoColor must not imply NoShadow)")
	}

	// Both on: shadows off even with colour off.
	ApplyTheme(resolve215(config.ThemeConfig{NoColor: true, NoShadow: true}))
	if b := newButton("ok", tv.Rect{X: 0, Y: 0, W: 4, H: 1}, nil); b.Shadow {
		t.Errorf("newButton.Shadow = true under NO_COLOR+NoShadow, want false")
	}
}

// --------------------------------------------------------------------------
// Editor: buildThemeConfig records NoShadow and round-trips through reopen.
// --------------------------------------------------------------------------

// TestIssue215BuildThemeConfigNoShadow checks the editor assembles NoShadow from
// the toggle state (mirroring NoColor), alone and alongside NoColor.
func TestIssue215BuildThemeConfigNoShadow(t *testing.T) {
	specs := specsFor(paletteByName(themeDefault))
	if got := buildThemeConfig("default", false, true, specs); !got.NoShadow {
		t.Errorf("buildThemeConfig(noShadow=true).NoShadow = false, want true")
	}
	if got := buildThemeConfig("default", false, false, specs); got.NoShadow {
		t.Errorf("buildThemeConfig(noShadow=false).NoShadow = true, want false")
	}
	// It rides alongside NoColor without interference.
	if got := buildThemeConfig("default", true, true, specs); !got.NoColor || !got.NoShadow {
		t.Errorf("buildThemeConfig(noColor=true,noShadow=true) = %+v, want both true", got)
	}
}

// TestIssue215EditorSeedsNoShadowFromConfig verifies the editor round-trips the
// toggle: a saved config with NoShadow=true seeds the checkbox true, and rebuilding
// the config from that state preserves NoShadow (the "save then reopen" stability
// property the existing TestBuildThemeConfigRoundTrip checks for colours).
func TestIssue215EditorSeedsNoShadowFromConfig(t *testing.T) {
	cur := config.ThemeConfig{Name: themeDefault, NoShadow: true}
	if reopened := buildThemeConfig(cur.Name, cur.NoColor, cur.NoShadow, specsFor(editedTheme(cur))); !reopened.NoShadow {
		t.Errorf("editor round-trip lost NoShadow: got %+v, want NoShadow=true", reopened)
	}

	cur = config.ThemeConfig{Name: themeHighContrast, NoShadow: false}
	if reopened := buildThemeConfig(cur.Name, cur.NoColor, cur.NoShadow, specsFor(editedTheme(cur))); reopened.NoShadow {
		t.Errorf("editor round-trip flipped NoShadow on: got %+v, want NoShadow=false", reopened)
	}
}
