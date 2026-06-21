package ui

import (
	"reflect"
	"testing"

	"gogent/internal/config"

	tui "github.com/hobbestherat/turbotui"
	tv "github.com/hobbestherat/turbotui/turbotv"
)

// This file exercises issue #260: the dropdown combo boxes (tv.Select) are promoted
// to first-class, editable theme roles — DropdownFG/DropdownBG (closed control),
// DropdownFocusFG/DropdownFocusBG (focused closed control) and DropdownSelectFG/
// DropdownSelectBG (highlighted row in the open popup) — mirroring the #243 menu-bar
// roles. The closed control's background defaults to the menu-bar background so a
// closed dropdown reads as prominent chrome rather than the old dark Input box, and
// the new roles flow through the full pipeline (palette → ResolveTheme degrade →
// applyOverrides → ApplyTheme install → newSelect/reseedSelect seed → live refresh),
// with the contrast audit (paletteContrast) extended to cover them.
//
// The suite is organised in groups A–J. Groups A–I verify the requirements end to
// end and guard the #231/#243/#204/#215 invariants the change must preserve. Group J
// is a DEFECT test: under the default theme the disabled effort selector's value is
// grey-on-grey (colorNote == dropdownBG), because #260 changed the closed-control
// background to the grey menu-bar colour but left the disabled colour as that same
// grey — the test fails for the default palette, exposing the regression.

// ----------------------------------------------------------------------------
// Shared helpers.
// ----------------------------------------------------------------------------

// issue260RestoreTheme snapshots the theme globals (via issue204RestoreTheme) AND the
// dropdown closed-control package vars ApplyTheme mutates, restoring both on cleanup.
// The dropdown vars are a new #260 global surface the #204 helper does not cover, so
// this keeps the dropdown tests hermetic: a test that switches palettes cannot leak a
// stale dropdown palette into later tests.
func issue260RestoreTheme(t *testing.T) {
	t.Helper()
	issue204RestoreTheme(t)
	fg, bg, ffg, fbg := dropdownFG, dropdownBG, dropdownFocusFG, dropdownFocusBG
	t.Cleanup(func() {
		dropdownFG, dropdownBG, dropdownFocusFG, dropdownFocusBG = fg, bg, ffg, fbg
	})
}

// dropdownFields maps each dropdown role name to the colour the given Theme paints
// for it — the single place the role→field wiring is named, so the wiring tests stay
// terse and a mis-wired accessor is caught unambiguously.
func dropdownFields(t Theme) map[string]tui.Color {
	return map[string]tui.Color{
		"dropdown_fg":        t.DropdownFG,
		"dropdown_bg":        t.DropdownBG,
		"dropdown_focus_fg":  t.DropdownFocusFG,
		"dropdown_focus_bg":  t.DropdownFocusBG,
		"dropdown_select_fg": t.DropdownSelectFG,
		"dropdown_select_bg": t.DropdownSelectBG,
	}
}

// ----------------------------------------------------------------------------
// Group A: the six roles exist and are populated by every built-in palette.
// ----------------------------------------------------------------------------

// TestIssue260DropdownRolesPopulatedByEveryPalette checks each built-in palette
// fills all six dropdown roles with a concrete colour, that the closed control's
// background equals the menu-bar background (the core requirement of #260), and that
// every role is legible/distinct from the fill it paints on. A palette that left a
// role unset, or set DropdownBG to something other than MenuBarBG, would fail here.
func TestIssue260DropdownRolesPopulatedByEveryPalette(t *testing.T) {
	for name, pal := range map[string]func() Theme{
		themeDefault:      defaultPalette,
		themeHighContrast: highContrastPalette,
		themeDark:         darkPalette,
	} {
		t.Run(name, func(t *testing.T) {
			p := pal()
			for role, c := range dropdownFields(p) {
				if reflect.DeepEqual(c, tui.Color{}) {
					t.Errorf("%s: %s is the zero Color — the role is not populated", name, role)
				}
				if c.Mode == tui.ColorDefault {
					t.Errorf("%s: %s is the terminal default — the role must carry a concrete colour", name, role)
				}
			}
			// Core requirement: the closed control carries the menu bar's background.
			if p.DropdownBG != p.MenuBarBG {
				t.Errorf("%s: DropdownBG %+v != MenuBarBG %+v — #260 requires the closed dropdown to carry the bar background",
					name, p.DropdownBG, p.MenuBarBG)
			}
			// Legibility / distinctness invariants per role.
			if p.DropdownFG == p.DropdownBG {
				t.Errorf("%s: DropdownFG == DropdownBG (%+v) — illegible closed control", name, p.DropdownFG)
			}
			if p.DropdownFocusBG == p.DropdownBG {
				t.Errorf("%s: DropdownFocusBG == DropdownBG (%+v) — focus does not stand out from the closed control", name, p.DropdownFocusBG)
			}
			if p.DropdownFocusFG == p.DropdownFocusBG {
				t.Errorf("%s: DropdownFocusFG == DropdownFocusBG (%+v) — illegible focused control", name, p.DropdownFocusFG)
			}
			if p.DropdownSelectFG == p.DropdownSelectBG {
				t.Errorf("%s: DropdownSelectFG == DropdownSelectBG (%+v) — illegible highlighted row", name, p.DropdownSelectFG)
			}
		})
	}
}

// TestIssue260DefaultPaletteDropdownValues pins the authored default-palette values
// (the ones the issue's acceptance criteria describe): a black-on-grey closed control
// matching the stock menu bar, with a cyan focus/select highlight. A silent swap of
// the highlight hue (e.g. to the ANSI-4 blue that collides with the window) is caught.
func TestIssue260DefaultPaletteDropdownValues(t *testing.T) {
	p := defaultPalette()
	if p.DropdownFG != tui.ANSIColor(0) {
		t.Errorf("default DropdownFG = %+v, want ANSI 0 (black on the grey bar)", p.DropdownFG)
	}
	if p.DropdownBG != tui.ANSIColor(7) {
		t.Errorf("default DropdownBG = %+v, want ANSI 7 (the menu-bar grey)", p.DropdownBG)
	}
	// The highlight must NOT be the ANSI-4 blue window background, or a highlighted
	// dropdown row collides with the surrounding window chrome.
	for _, c := range []tui.Color{p.DropdownFocusBG, p.DropdownSelectBG} {
		if c == tui.ANSIColor(4) {
			t.Errorf("default dropdown highlight %+v is the ANSI-4 blue window background — it must stay distinct", c)
		}
	}
}

// ----------------------------------------------------------------------------
// Group B: ResolveTheme degrades the roles to the terminal's fidelity.
// ----------------------------------------------------------------------------

// TestIssue260ResolveThemeDegradesDropdown verifies ResolveTheme degrades all six
// roles with the terminal's ColorLevel, exactly like the #243 menu roles: NO_COLOR
// collapses them to the terminal default; truecolor preserves the palette values;
// the RGB presets quantise to ANSI at 256 and 16 colours. A missing degrade() line
// would emit a colour under NO_COLOR or an out-of-gamut RGB on a 16-colour terminal.
func TestIssue260ResolveThemeDegradesDropdown(t *testing.T) {
	t.Run("NO_COLOR collapses every role to the terminal default", func(t *testing.T) {
		for _, name := range []string{themeDefault, themeHighContrast, themeDark} {
			got := ResolveTheme(config.ThemeConfig{Name: name, NoColor: true}, truecolorEnv, false)
			for role, c := range dropdownFields(got) {
				if c != tui.DefaultColor() {
					t.Errorf("%s NO_COLOR: Dropdown%s = %+v, want terminal default", name, role, c)
				}
			}
		}
	})

	t.Run("truecolor preserves the palette values", func(t *testing.T) {
		for _, name := range []string{themeDefault, themeHighContrast, themeDark} {
			pal := paletteByName(name)
			got := ResolveTheme(config.ThemeConfig{Name: name}, truecolorEnv, false)
			for role, want := range dropdownFields(pal) {
				if got := dropdownFields(got)[role]; got != want {
					t.Errorf("%s truecolor: Dropdown%s = %+v, want the palette %+v", name, role, got, want)
				}
			}
		}
	})

	t.Run("RGB presets degrade to ANSI at 256 and 16 colours", func(t *testing.T) {
		for _, name := range []string{themeHighContrast, themeDark} {
			for _, env := range []func(string) string{color256Env, color16Env} {
				got := ResolveTheme(config.ThemeConfig{Name: name}, env, false)
				for role, c := range dropdownFields(got) {
					if c.Mode != tui.ColorANSI {
						t.Errorf("%s: Dropdown%s = %+v, want a quantised ANSI colour (RGB must degrade)", name, role, c)
					}
				}
			}
		}
	})

	t.Run("default-palette ANSI values are fidelity-invariant", func(t *testing.T) {
		// The default palette is authored in 16-colour indices, so every level keeps it.
		pal := defaultPalette()
		for _, env := range []func(string) string{color16Env, color256Env, truecolorEnv} {
			got := ResolveTheme(config.ThemeConfig{}, env, false)
			for role, want := range dropdownFields(pal) {
				if got := dropdownFields(got)[role]; got != want {
					t.Errorf("default Dropdown%s at 16/256/truecolor = %+v, want %+v", role, got, want)
				}
			}
		}
	})
}

// TestIssue260DegradeHelper exercises the degrade() path the roles route through, so
// a regression in the shared quantiser is caught directly (the RGB→ANSI path the
// high-contrast/dark dropdown colours depend on).
func TestIssue260DegradeHelper(t *testing.T) {
	amber := tui.RGBColor(0xE0, 0xAF, 0x68) // dark-palette accent / DropdownSelectBG
	if got := degrade(amber, ColorNone); got != tui.DefaultColor() {
		t.Errorf("degrade(amber, ColorNone) = %+v, want terminal default", got)
	}
	if got := degrade(amber, ColorTrue); got != amber {
		t.Errorf("degrade(amber, ColorTrue) = %+v, want unchanged", got)
	}
	if got := degrade(amber, Color256); got.Mode != tui.ColorANSI {
		t.Errorf("degrade(amber, Color256) = %+v, want an ANSI index", got)
	}
	if got := degrade(amber, Color16); got.Mode != tui.ColorANSI {
		t.Errorf("degrade(amber, Color16) = %+v, want an ANSI index", got)
	}
	// An in-palette ANSI colour (the default theme's cyan highlight) passes through.
	cyan := tui.ANSIColor(6)
	if got := degrade(cyan, Color16); got != cyan {
		t.Errorf("degrade(cyan, Color16) = %+v, want unchanged ANSI 6", got)
	}
}

// ----------------------------------------------------------------------------
// Group C: the roles are overridable via config (applyOverrides) and present in the
// theme editor (themeRoles), round-tripping through the editor's builders.
// ----------------------------------------------------------------------------

// TestIssue260DropdownOverridesApply checks each of the six config override keys sets
// the matching Theme field (a missing applyOverrides case would silently drop an
// override — the exact mistake that made the menu bar non-editable in #243). It also
// covers graceful degradation: a case/whitespace variant applies, an unparseable
// value is ignored, and an unknown name never leaks into a dropdown field.
func TestIssue260DropdownOverridesApply(t *testing.T) {
	marker, ok := parseColor("#12EFA0") // a distinctive RGB no preset equals
	if !ok {
		t.Fatalf("setup: parseColor(#12EFA0) failed")
	}
	for key := range dropdownFields(defaultPalette()) {
		t.Run(key+" applies", func(t *testing.T) {
			got := paletteByName(themeDefault)
			applyOverrides(&got, map[string]string{key: "#12EFA0"})
			if dropdownFields(got)[key] != marker {
				t.Errorf("applyOverrides({%q:#12EFA0}) left %q at %+v, want %+v — the override is silently dropped (missing applyOverrides case?)",
					key, key, dropdownFields(got)[key], marker)
			}
		})
	}

	t.Run("ANSI specs apply", func(t *testing.T) {
		got := paletteByName(themeDefault)
		applyOverrides(&got, map[string]string{"dropdown_select_bg": "9"})
		if got.DropdownSelectBG != tui.ANSIColor(9) {
			t.Errorf("dropdown_select_bg=9 -> %+v, want ANSI 9", got.DropdownSelectBG)
		}
	})

	t.Run("key is case/whitespace insensitive", func(t *testing.T) {
		got := paletteByName(themeDefault)
		applyOverrides(&got, map[string]string{"  Dropdown_Select_BG ": "5"})
		if got.DropdownSelectBG != tui.ANSIColor(5) {
			t.Errorf("normalised dropdown_select_bg -> %+v, want ANSI 5", got.DropdownSelectBG)
		}
	})

	t.Run("invalid value ignored", func(t *testing.T) {
		got := paletteByName(themeDefault)
		before := got.DropdownBG
		applyOverrides(&got, map[string]string{"dropdown_bg": "nope"})
		if got.DropdownBG != before {
			t.Errorf("invalid dropdown_bg overrode the value: %+v -> %+v", before, got.DropdownBG)
		}
	})

	t.Run("unknown name does not leak into a dropdown field", func(t *testing.T) {
		got := paletteByName(themeDefault)
		before := dropdownFields(got)
		applyOverrides(&got, map[string]string{"dropdown": "#123456", "select_bg": "#123456"}) // wrong keys
		for role, c := range dropdownFields(got) {
			if c != before[role] {
				t.Errorf("unknown override key leaked into %q: %+v -> %+v", role, before[role], c)
			}
		}
	})
}

// TestIssue260DropdownRolesInEditor verifies the six roles are exposed in the editor's
// themeRoles list with accessors that read the correct Theme fields. A missing entry
// would mean the role is silently not editable; a mis-wired accessor would edit the
// wrong colour.
func TestIssue260DropdownRolesInEditor(t *testing.T) {
	roles := issue243RoleByKey(t)
	accessors := map[string]func(Theme) tui.Color{
		"dropdown_fg":        func(t Theme) tui.Color { return t.DropdownFG },
		"dropdown_bg":        func(t Theme) tui.Color { return t.DropdownBG },
		"dropdown_focus_fg":  func(t Theme) tui.Color { return t.DropdownFocusFG },
		"dropdown_focus_bg":  func(t Theme) tui.Color { return t.DropdownFocusBG },
		"dropdown_select_fg": func(t Theme) tui.Color { return t.DropdownSelectFG },
		"dropdown_select_bg": func(t Theme) tui.Color { return t.DropdownSelectBG },
	}
	for key, access := range accessors {
		r, ok := roles[key]
		if !ok {
			t.Errorf("themeRoles is missing %q — the role is not editable in the editor", key)
			continue
		}
		// The editor accessor must read the same field as direct Theme access, for
		// every preset (so an edit changes the colour the user sees).
		for _, preset := range []string{themeDefault, themeHighContrast, themeDark} {
			p := paletteByName(preset)
			if r.get(p) != access(p) {
				t.Errorf("%s: editor accessor for %q = %+v, but the Theme field = %+v (mis-wired accessor)", preset, key, r.get(p), access(p))
			}
		}
	}
}

// TestIssue260DropdownOverrideRoundTrip checks the editor's "save then reopen"
// stability for the new roles across every preset: a config built by the editor,
// reloaded via editedTheme and rebuilt, reproduces itself. A break here means a
// dropdown edit would drift each time the dialog is reopened.
func TestIssue260DropdownOverrideRoundTrip(t *testing.T) {
	for _, name := range []string{themeDefault, themeHighContrast, themeDark} {
		specs := specsFor(paletteByName(name))
		specs["dropdown_bg"] = "#0ABCD1"
		specs["dropdown_select_fg"] = "9"
		cfg := buildThemeConfig(name, false, false, specs)

		reopened := buildThemeConfig(cfg.Name, cfg.NoColor, false, specsFor(editedTheme(cfg)))
		if !reflect.DeepEqual(reopened, cfg) {
			t.Errorf("preset %q: dropdown override did not round-trip:\n got %+v\nwant %+v", name, reopened, cfg)
		}
		if reopened.Overrides["dropdown_bg"] != "#0ABCD1" {
			t.Errorf("preset %q: round-trip lost/changed dropdown_bg: %+v", name, reopened.Overrides)
		}
	}
}

// ----------------------------------------------------------------------------
// Group D: ApplyTheme installs the roles — closed-control package vars and the open
// popup's Selection* — including an override flowing end-to-end and NO_COLOR.
// ----------------------------------------------------------------------------

// TestIssue260ApplyThemeInstallsDropdownRoles proves ApplyTheme is the bridge from the
// resolved Theme to the live dropdown colours: for every preset the closed-control
// package vars equal the theme's Dropdown roles and tv.DefaultTheme (and the active
// theme drawPopup reads) carry the DropdownSelect roles on Selection*. A missing
// install would leave freshly built / live-reseeded dropdowns on the old palette.
func TestIssue260ApplyThemeInstallsDropdownRoles(t *testing.T) {
	for _, th := range []Theme{issue204Default(), issue204HighContrast(), issue204Dark()} {
		t.Run(th.Name, func(t *testing.T) {
			issue260RestoreTheme(t)
			ApplyTheme(th)
			if dropdownFG != th.DropdownFG {
				t.Errorf("dropdownFG = %+v, want %+v", dropdownFG, th.DropdownFG)
			}
			if dropdownBG != th.DropdownBG {
				t.Errorf("dropdownBG = %+v, want %+v", dropdownBG, th.DropdownBG)
			}
			if dropdownFocusFG != th.DropdownFocusFG {
				t.Errorf("dropdownFocusFG = %+v, want %+v", dropdownFocusFG, th.DropdownFocusFG)
			}
			if dropdownFocusBG != th.DropdownFocusBG {
				t.Errorf("dropdownFocusBG = %+v, want %+v", dropdownFocusBG, th.DropdownFocusBG)
			}
			// Open-popup highlighted row: installed onto Selection*, which drawPopup reads
			// from the active theme at draw time.
			if tv.DefaultTheme.SelectionFG != th.DropdownSelectFG {
				t.Errorf("tv.DefaultTheme.SelectionFG = %+v, want DropdownSelectFG %+v", tv.DefaultTheme.SelectionFG, th.DropdownSelectFG)
			}
			if tv.DefaultTheme.SelectionBG != th.DropdownSelectBG {
				t.Errorf("tv.DefaultTheme.SelectionBG = %+v, want DropdownSelectBG %+v", tv.DefaultTheme.SelectionBG, th.DropdownSelectBG)
			}
			if tv.ActiveTheme().SelectionBG != th.DropdownSelectBG {
				t.Errorf("tv.ActiveTheme().SelectionBG = %+v, want %+v (tv.SetTheme must propagate the install)", tv.ActiveTheme().SelectionBG, th.DropdownSelectBG)
			}
		})
	}
}

// TestIssue260ApplyThemeDropdownOverrideFlowsThrough is the end-to-end test for the
// newly-editable closed control and popup highlight: a dropdown_bg / dropdown_select_bg
// override configured in the editor flows through ResolveTheme → ApplyTheme onto both
// the closed-control package var and tv.DefaultTheme.Selection*. This is the path the
// editor's Save runs.
func TestIssue260ApplyThemeDropdownOverrideFlowsThrough(t *testing.T) {
	issue260RestoreTheme(t)
	red := tui.RGBColor(0xFF, 0x00, 0x00)
	green := tui.RGBColor(0x00, 0xFF, 0x00)
	th := ResolveTheme(config.ThemeConfig{
		Name: themeDefault,
		Overrides: map[string]string{
			"dropdown_bg":        "#FF0000",
			"dropdown_select_bg": "#00FF00",
		},
	}, truecolorEnv, false)
	if th.DropdownBG != red || th.DropdownSelectBG != green {
		t.Fatalf("setup: ResolveTheme DropdownBG=%+v DropdownSelectBG=%+v, want red/green", th.DropdownBG, th.DropdownSelectBG)
	}
	ApplyTheme(th)
	if dropdownBG != red {
		t.Errorf("dropdown_bg override did not reach the closed-control var: got %+v, want red", dropdownBG)
	}
	if tv.DefaultTheme.SelectionBG != green {
		t.Errorf("dropdown_select_bg override did not reach SelectionBG: got %+v, want green", tv.DefaultTheme.SelectionBG)
	}
}

// TestIssue260ApplyThemeNoColorNeutralizes confirms that under NO_COLOR the dropdown
// roles degrade to the terminal default and ApplyTheme does not fight that — the
// closed-control vars and the Selection* slots are left neutral.
func TestIssue260ApplyThemeNoColorNeutralizes(t *testing.T) {
	issue260RestoreTheme(t)
	th := ResolveTheme(config.ThemeConfig{NoColor: true}, truecolorEnv, false)
	ApplyTheme(th)
	for role, c := range map[string]tui.Color{
		"dropdownFG": dropdownFG, "dropdownBG": dropdownBG,
		"dropdownFocusFG": dropdownFocusFG, "dropdownFocusBG": dropdownFocusBG,
	} {
		if c != tui.DefaultColor() {
			t.Errorf("NO_COLOR %s = %+v, want terminal default", role, c)
		}
	}
	if tv.DefaultTheme.SelectionFG != tui.DefaultColor() || tv.DefaultTheme.SelectionBG != tui.DefaultColor() {
		t.Errorf("NO_COLOR Selection* = FG %+v BG %+v, want terminal default", tv.DefaultTheme.SelectionFG, tv.DefaultTheme.SelectionBG)
	}
}

// ----------------------------------------------------------------------------
// Group E: the open-popup decision is intentional (Selection install, dialog body
// untouched, no-op for the black-canvas presets) and the default highlight is now
// visually distinct from the body.
// ----------------------------------------------------------------------------

// TestIssue260OpenPopupDecision pins the documented #260 design:
//   - the popup body keeps dialog chrome (Dialog*), only the highlighted row follows
//     the dropdown palette via Selection*;
//   - for the black-canvas presets the Selection install is a no-op (their chrome
//     already uses the accent for Selection, which equals DropdownSelectBG);
//   - under the default theme the highlight (cyan) differs from the body (grey), so
//     the highlighted row is visible — previously SelectionBG == DialogBG and the
//     highlight was bold-only.
func TestIssue260OpenPopupDecision(t *testing.T) {
	t.Run("black-canvas Selection install is a no-op", func(t *testing.T) {
		for _, th := range []Theme{issue204HighContrast(), issue204Dark()} {
			cv := blackCanvasTVTheme(th)
			if cv.SelectionBG != th.DropdownSelectBG {
				t.Errorf("%s: blackCanvas SelectionBG %+v != DropdownSelectBG %+v — the install should be a no-op", th.Name, cv.SelectionBG, th.DropdownSelectBG)
			}
			if cv.SelectionFG != th.DropdownSelectFG {
				t.Errorf("%s: blackCanvas SelectionFG %+v != DropdownSelectFG %+v", th.Name, cv.SelectionFG, th.DropdownSelectFG)
			}
		}
	})

	t.Run("default popup highlight is distinct from the body", func(t *testing.T) {
		issue260RestoreTheme(t)
		ApplyTheme(issue204Default())
		if tv.DefaultTheme.SelectionBG == tv.DefaultTheme.DialogBG {
			t.Errorf("default popup highlight SelectionBG %+v == body DialogBG %+v — the highlighted row is not visually distinct (the pre-#260 bold-only highlight regressed)",
				tv.DefaultTheme.SelectionBG, tv.DefaultTheme.DialogBG)
		}
	})

	t.Run("popup body keeps dialog chrome, not the dropdown closed-control colour", func(t *testing.T) {
		issue260RestoreTheme(t)
		// Under the default theme the closed control is grey (DropdownBG) and the dialog
		// body is grey (DialogBG) too, so compare the highlight against the body, and
		// confirm the body is driven by Dialog*, not by the closed-control role.
		ApplyTheme(issue204Default())
		th := issue204Default()
		// DropdownSelectBG drives Selection; the body is DialogBG. They differ (cyan vs grey).
		if tv.DefaultTheme.DialogBG == th.DropdownSelectBG {
			t.Errorf("default DialogBG %+v == DropdownSelectBG %+v — the body would follow the dropdown palette instead of dialog chrome", tv.DefaultTheme.DialogBG, th.DropdownSelectBG)
		}
	})
}

// ----------------------------------------------------------------------------
// Group F: newSelect / reseedSelect seed the closed control from the dropdown roles
// (not the Input* slots), so #260 recolours every combo box through one wrapper.
// ----------------------------------------------------------------------------

// TestIssue260NewSelectSeedsFromDropdownRoles proves the construction wrapper seeds
// the closed control from the dropdown package vars, and — crucially for the default
// theme — NOT from the Input* slots (the old dark ANSI-0 box). Under the default
// theme DropdownBG (grey) differs from InputBG (black), so the assertion is concrete.
func TestIssue260NewSelectSeedsFromDropdownRoles(t *testing.T) {
	issue260RestoreTheme(t)
	ApplyTheme(issue204Default())
	def := issue204Default()

	t.Run("default theme", func(t *testing.T) {
		s := newTestSelect(t)
		if s.FG != def.DropdownFG || s.BG != def.DropdownBG || s.FocusFG != def.DropdownFocusFG || s.FocusBG != def.DropdownFocusBG {
			t.Fatalf("newSelect did not seed from the dropdown roles: FG=%+v BG=%+v FocusFG=%+v FocusBG=%+v",
				s.FG, s.BG, s.FocusFG, s.FocusBG)
		}
		// The source really changed: the closed control's background is the grey menu-bar
		// colour, not the Input* black the old code seeded from.
		th := tv.ActiveTheme()
		if s.BG == th.InputBG {
			t.Errorf("newSelect BG == InputBG %+v; #260 must seed from the dropdown roles (grey), not Input* (black)", th.InputBG)
		}
	})

	t.Run("tracks the active palette", func(t *testing.T) {
		hc := issue204HighContrast()
		ApplyTheme(hc)
		s := newTestSelect(t)
		if s.FG != hc.DropdownFG || s.BG != hc.DropdownBG || s.FocusFG != hc.DropdownFocusFG || s.FocusBG != hc.DropdownFocusBG {
			t.Errorf("newSelect did not seed from the high-contrast dropdown roles: FG=%+v BG=%+v FocusFG=%+v FocusBG=%+v",
				s.FG, s.BG, s.FocusFG, s.FocusBG)
		}
	})
}

// TestIssue260ReseedSelectSeedsFromDropdownRoles proves the live theme-apply path
// re-seeds an already-built Select from the dropdown roles (so a theme switch
// recolours it without a restart), overriding whatever colours were cached at
// construction or corrupted since.
func TestIssue260ReseedSelectSeedsFromDropdownRoles(t *testing.T) {
	issue260RestoreTheme(t)
	ApplyTheme(issue204Default())
	s := newTestSelect(t)

	// Corrupt the colours to prove reseed overwrites them, then reseed under default.
	s.FG, s.BG, s.FocusFG, s.FocusBG = tui.ANSIColor(1), tui.ANSIColor(2), tui.ANSIColor(3), tui.ANSIColor(4)
	reseedSelect(s, tv.ActiveTheme())
	def := issue204Default()
	if s.FG != def.DropdownFG || s.BG != def.DropdownBG || s.FocusFG != def.DropdownFocusFG || s.FocusBG != def.DropdownFocusBG {
		t.Errorf("reseedSelect did not restore the default dropdown roles: FG=%+v BG=%+v FocusFG=%+v FocusBG=%+v",
			s.FG, s.BG, s.FocusFG, s.FocusBG)
	}

	// reseedSelect ignores its theme arg and reads the package vars, so it must follow an
	// ApplyTheme switch even when called with a stale theme handle.
	hc := issue204HighContrast()
	ApplyTheme(hc)
	reseedSelect(s, tv.ActiveTheme()) // th is the new theme, but the vars are what matter
	if s.BG != hc.DropdownBG || s.FocusBG != hc.DropdownFocusBG {
		t.Errorf("reseedSelect did not follow the ApplyTheme switch to high-contrast: BG=%+v FocusBG=%+v", s.BG, s.FocusBG)
	}
}

// ----------------------------------------------------------------------------
// Group G: a live theme switch recolours an already-built Select (the #204 refresh
// chain), at both the wrapper and the real session-window level.
// ----------------------------------------------------------------------------

// TestIssue260LiveSwitchRecolorsAlreadyBuiltSelect is the core #260 live-refresh
// property: a Select built under one palette is recoloured (closed control AND open
// popup highlight) when the theme switches and reseedSelect re-runs — no restart.
func TestIssue260LiveSwitchRecolorsAlreadyBuiltSelect(t *testing.T) {
	issue260RestoreTheme(t)
	ApplyTheme(issue204Default())
	s := newTestSelect(t)
	defBG, defFocusBG := s.BG, s.FocusBG
	defSelectionBG := tv.ActiveTheme().SelectionBG

	hc := issue204HighContrast()
	ApplyTheme(hc)
	reseedSelect(s, tv.ActiveTheme())

	if s.BG != hc.DropdownBG {
		t.Errorf("closed-control BG not recoloured: got %+v, want %+v", s.BG, hc.DropdownBG)
	}
	if s.FocusBG != hc.DropdownFocusBG {
		t.Errorf("focused-control BG not recoloured: got %+v, want %+v", s.FocusBG, hc.DropdownFocusBG)
	}
	if s.BG == defBG || s.FocusBG == defFocusBG {
		t.Fatalf("the select did not change across the switch (BG %+v, FocusBG %+v) — live recolour is broken", s.BG, s.FocusBG)
	}
	// The open popup's highlighted row recolours too (Selection* follows ApplyTheme).
	if tv.ActiveTheme().SelectionBG != hc.DropdownSelectBG {
		t.Errorf("open-popup SelectionBG not recoloured: got %+v, want %+v", tv.ActiveTheme().SelectionBG, hc.DropdownSelectBG)
	}
	if tv.ActiveTheme().SelectionBG == defSelectionBG {
		t.Errorf("open-popup SelectionBG did not change across the switch")
	}
}

// TestIssue260RefreshThemeRecolorsSessionSelects runs the real #204 refresh path on a
// session window: after a live switch the model and effort selects are re-seeded to
// the new dropdown palette (proving the change reaches gogent's own header selects,
// not just the bare wrapper).
func TestIssue260RefreshThemeRecolorsSessionSelects(t *testing.T) {
	issue260RestoreTheme(t)
	ApplyTheme(issue204Default())
	w := newTestWorkbench(t)
	sw := w.openWindow("s", "S")
	defModelBG := sw.modelSelect.BG
	defEffortBG := sw.effortSelect.BG

	hc := issue204HighContrast()
	ApplyTheme(hc)
	sw.refreshTheme()

	for _, c := range []struct {
		name   string
		got    tui.Color
		before tui.Color
	}{
		{"model select BG", sw.modelSelect.BG, defModelBG},
		{"effort select BG", sw.effortSelect.BG, defEffortBG},
	} {
		if c.got != hc.DropdownBG {
			t.Errorf("%s = %+v after refresh, want the high-contrast dropdown BG %+v", c.name, c.got, hc.DropdownBG)
		}
		if c.got == c.before {
			t.Errorf("%s did not change across the switch (still %+v) — refreshTheme did not reseed it", c.name, c.before)
		}
	}
}

// ----------------------------------------------------------------------------
// Group H: paletteContrast covers the three new pairings against their own
// backgrounds, at the body-text tier, and every built-in palette clears it.
// ----------------------------------------------------------------------------

// TestIssue260PaletteContrastCoversDropdownPairs verifies the contrast audit was
// extended (#202): the dropdown, dropdown-focus and dropdown-select findings exist,
// are checked against the background each role actually paints on (its own fill, not
// the window background), are held to the body-text tier, and clear it for every
// built-in palette — so a palette change cannot silently reintroduce a low-contrast
// dropdown pair.
func TestIssue260PaletteContrastCoversDropdownPairs(t *testing.T) {
	for name, pal := range map[string]func() Theme{
		themeDefault:      defaultPalette,
		themeHighContrast: highContrastPalette,
		themeDark:         darkPalette,
	} {
		t.Run(name, func(t *testing.T) {
			p := pal()
			findings := paletteContrast(p, baseTVTheme.WindowBG)
			byRole := make(map[string]contrastFinding, len(findings))
			for _, f := range findings {
				byRole[f.Role] = f
			}
			want := map[string]struct{ fg, bg tui.Color }{
				"dropdown":        {p.DropdownFG, p.DropdownBG},
				"dropdown-focus":  {p.DropdownFocusFG, p.DropdownFocusBG},
				"dropdown-select": {p.DropdownSelectFG, p.DropdownSelectBG},
			}
			for role, w := range want {
				f, ok := byRole[role]
				if !ok {
					t.Errorf("paletteContrast has no %q finding — the new pairing is not audited (#202)", role)
					continue
				}
				if f.FG != w.fg {
					t.Errorf("%s %q FG = %+v, want %+v", name, role, f.FG, w.fg)
				}
				if f.BG != w.bg {
					t.Errorf("%s %q BG = %+v, want %+v — the role must be audited against the background it paints on", name, role, f.BG, w.bg)
				}
				if f.Min != minContrastText {
					t.Errorf("%s %q Min = %g, want minContrastText %g (dropdown label text)", name, role, f.Min, minContrastText)
				}
				if !f.OK() {
					t.Errorf("%s %q fails its contrast minimum: %.2f:1 < %.1f — a low-contrast dropdown pair", name, role, f.Ratio, f.Min)
				}
			}
		})
	}
}

// ----------------------------------------------------------------------------
// Group I: preserve the #231 (shadow) and #243 (menu bar) invariants alongside #260.
// ----------------------------------------------------------------------------

// TestIssue260PreservesSelectShadow re-asserts the #231 invariant still holds now that
// newSelect/reseedSelect also seed dropdown colours: the popup shadow follows the
// NoShadow preference at construction and on the live reseed path.
func TestIssue260PreservesSelectShadow(t *testing.T) {
	t.Run("NoShadow clears the popup shadow at construction", func(t *testing.T) {
		issue260RestoreTheme(t)
		ApplyTheme(noShadowTheme())
		if newTestSelect(t).Shadow {
			t.Errorf("Select.Shadow = true under NoShadow (#231 regressed)")
		}
	})
	t.Run("reseed re-applies the toggle live", func(t *testing.T) {
		issue260RestoreTheme(t)
		ApplyTheme(defaultShadowTheme())
		s := newTestSelect(t)
		ApplyTheme(noShadowTheme())
		reseedSelect(s, tv.ActiveTheme())
		if s.Shadow {
			t.Errorf("Select.Shadow = true after a live NoShadow reseed (#231 regressed)")
		}
	})
}

// ----------------------------------------------------------------------------
// Group J — DEFECT: the disabled effort selector's value is invisible under the
// default theme (#260 changed the closed-control background to grey but left the
// disabled colour as the same grey).
// ----------------------------------------------------------------------------

// TestIssue260DisabledEffortValueLegibleOnDropdownBackground guards the contrast of a
// disabled effort selector's "(default)" value. #260 moved the closed control onto
// DropdownBG (the menu-bar grey), so painting the disabled value as the dim Note grey
// would be grey-on-grey (contrast ~1.0:1, invisible) in the default palette. The
// implementation therefore resolves the disabled foreground through dropdownDisabledColor
// (ApplyTheme installs it into dropdownDisabledFG): the dim Note grey is kept only where
// it still clears the contrast floor on DropdownBG (the black-canvas presets), and falls
// back to the legible DropdownFG on the light default control. This test asserts that
// rendered colour — dropdownDisabledFG on dropdownBG — clears minContrastText for every
// preset, so a future palette change that reintroduced the grey-on-grey regression trips.
func TestIssue260DisabledEffortValueLegibleOnDropdownBackground(t *testing.T) {
	issue260RestoreTheme(t)
	for _, th := range []Theme{issue204Default(), issue204HighContrast(), issue204Dark()} {
		t.Run(th.Name, func(t *testing.T) {
			ApplyTheme(th)
			// guardEffortSelect's disabled branch paints effortSelect.FG = dropdownDisabledFG
			// (resolved by ApplyTheme from dropdownDisabledColor) on dropdownBG. Assert that
			// actual pair clears the inactive-control floor — not the raw Note grey, which the
			// disabled control no longer uses on the light default background. The floor is
			// minContrastLarge: a disabled value is an inactive UI component (WCAG 1.4.3 exempt
			// from the 4.5 body-text minimum), and dropdownDisabledColor itself gates the dim
			// Note grey on minContrastLarge, falling back to the legible DropdownFG otherwise.
			ratio := contrastRatio(dropdownDisabledFG, dropdownBG)
			if ratio < minContrastLarge {
				t.Errorf("%s theme: a disabled effort selector's value is illegible — dropdownDisabledFG %+v on dropdownBG %+v is %.2f:1 (< %.1f). "+
					"#260 made the closed-control background the menu-bar grey, so dropdownDisabledColor must drop the dim Note grey to a legible foreground rather than render grey-on-grey.",
					th.Name, dropdownDisabledFG, dropdownBG, ratio, minContrastLarge)
			}
		})
	}
}

// TestIssue260DisabledEffortSelectSitsOnDropdownBackground is the reachability companion
// to the contrast defect above: it builds a REAL session window whose model offers no
// effort options, confirms the effort selector is in the disabled state, and confirms
// its closed-control background is the dropdown role (dropdownBG) — the very fill the
// disabled colorNote value is painted on. This removes any doubt that the illegible
// pairing is actually rendered (not just a theoretical colour overlap): the disabled
// effort control is reachable for any model without EffortOptions, and it carries the
// menu-bar-grey background that makes colorNote invisible under the default theme.
func TestIssue260DisabledEffortSelectSitsOnDropdownBackground(t *testing.T) {
	issue260RestoreTheme(t)
	ApplyTheme(issue204Default())

	// A single model with no EffortOptions disables the effort selector (issue #177).
	w := NewWorkbench([]*config.ModelConfig{
		{Name: "plain", DisplayName: "Plain", Model: "plain"},
	})
	sw := w.NewSession()

	if sw.effortEnabled {
		t.Fatalf("setup: effort selector should be disabled for a model with no effort options")
	}
	// The closed control's background is the dropdown role (seeded by newSelect).
	if sw.effortSelect.BG != dropdownBG {
		t.Fatalf("disabled effort select BG = %+v, want dropdownBG %+v — the disabled value is not painted on the dropdown background",
			sw.effortSelect.BG, dropdownBG)
	}
	// guardEffortSelect paints the disabled value as colorNote (this is the colour the
	// DrawFn installs on every render when !effortEnabled). Under the default theme that
	// is the same grey as dropdownBG, hence the defect.
	if colorNote == dropdownBG {
		t.Logf("CONFIRMED: under the default theme colorNote %+v == dropdownBG %+v, so a disabled effort selector's value is grey-on-grey (see the contrast test above)", colorNote, dropdownBG)
	}
}
