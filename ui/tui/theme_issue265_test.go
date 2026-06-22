package ui

import (
	"reflect"
	"testing"

	"gogent/internal/config"

	tui "github.com/hobbestherat/turbotui"
	tv "github.com/hobbestherat/turbotui/turbotv"
)

// This file exercises issue #265: the button and input-box foreground/background
// colours are promoted to first-class, overridable theme roles — ButtonFG/ButtonBG/
// ButtonFocusFG/ButtonFocusBG and InputFG/InputBG/InputFocusFG/InputFocusBG —
// mirroring exactly what #260 did for dropdowns and #243 for the menu bar. Unlike
// dropdowns (whose closed control has no slot in turbotui's Theme and so is carried
// in gogent package vars), buttons and inputs DO have slots in tv.Theme that every
// freshly built widget and the live reseed path read, so #265 needs no gogent package
// vars: ApplyTheme installs the roles onto tv.DefaultTheme's Button*/Input* slots and
// tv.SetTheme propagates them. The roles flow through the full pipeline (palette →
// ResolveTheme degrade → applyOverrides → ApplyTheme install → tv.SetTheme → live
// reseed in SessionWindow.refreshTheme), with paletteContrast extended to cover them.
//
// The acceptance criteria the suite pins:
//   - the DEFAULT install's button/input appearance is unchanged (the new roles equal
//     turbotui's stock baseTVTheme values, which the default preset inherited before);
//   - a button_bg / input_bg override applies at startup AND on a live theme switch;
//   - the editor exposes the RESTING pairs (button_fg/bg, input_fg/bg) and they
//     round-trip through Save/Reopen, while the focus pairs stay config-only;
//   - the contrast audit covers the new pairs.
//
// Groups A–H mirror the #260 layout. The roles are restored via issue204RestoreTheme:
// #265 adds no new package vars (only tv.DefaultTheme / tv.ActiveTheme slots, which
// that helper already snapshots), so no #265-specific restore helper is needed.

// ----------------------------------------------------------------------------
// Shared helpers.
// ----------------------------------------------------------------------------

// buttonInputFields maps each of the eight #265 role override-keys to the colour the
// given Theme paints for it — the single place the role→field wiring is named, so the
// wiring tests stay terse and a mis-wired accessor is caught unambiguously.
func buttonInputFields(t Theme) map[string]tui.Color {
	return map[string]tui.Color{
		"button_fg":       t.ButtonFG,
		"button_bg":       t.ButtonBG,
		"button_focus_fg": t.ButtonFocusFG,
		"button_focus_bg": t.ButtonFocusBG,
		"input_fg":        t.InputFG,
		"input_bg":        t.InputBG,
		"input_focus_fg":  t.InputFocusFG,
		"input_focus_bg":  t.InputFocusBG,
	}
}

// tvButtonInputSlots maps the same eight role keys to the corresponding slot on a
// turbotui tv.Theme, so a test can assert ApplyTheme installs each role onto the
// matching slot (the bridge to every freshly built / reseeded widget).
func tvButtonInputSlots(d tv.Theme) map[string]tui.Color {
	return map[string]tui.Color{
		"button_fg":       d.ButtonFG,
		"button_bg":       d.ButtonBG,
		"button_focus_fg": d.ButtonFocusFG,
		"button_focus_bg": d.ButtonFocusBG,
		"input_fg":        d.InputFG,
		"input_bg":        d.InputBG,
		"input_focus_fg":  d.InputFocusFG,
		"input_focus_bg":  d.InputFocusBG,
	}
}

// ----------------------------------------------------------------------------
// Group A: the eight roles exist and are populated by every built-in palette, and
// the DEFAULT palette equals turbotui's stock values (appearance-unchanged guarantee).
// ----------------------------------------------------------------------------

// TestIssue265ButtonInputRolesPopulatedByEveryPalette checks each built-in palette
// fills all eight roles with a concrete colour and that every resting/focus pair is
// legible (fg != bg) and the focus background stands out from the resting one. A
// palette that left a role unset (the zero Color, which renders as uninitialised
// ANSI 0 black rather than the terminal default) would fail here.
func TestIssue265ButtonInputRolesPopulatedByEveryPalette(t *testing.T) {
	for name, pal := range map[string]func() Theme{
		themeDefault:      defaultPalette,
		themeHighContrast: highContrastPalette,
		themeDark:         darkPalette,
	} {
		t.Run(name, func(t *testing.T) {
			p := pal()
			for role, c := range buttonInputFields(p) {
				if reflect.DeepEqual(c, tui.Color{}) {
					t.Errorf("%s: %s is the zero Color — the role is not populated", name, role)
				}
				if c.Mode == tui.ColorDefault {
					t.Errorf("%s: %s is the terminal default — a built-in palette must carry a concrete colour", name, role)
				}
			}
			// Legibility / distinctness invariants per pair.
			if p.ButtonFG == p.ButtonBG {
				t.Errorf("%s: ButtonFG == ButtonBG (%+v) — illegible resting button", name, p.ButtonFG)
			}
			if p.ButtonFocusFG == p.ButtonFocusBG {
				t.Errorf("%s: ButtonFocusFG == ButtonFocusBG (%+v) — illegible focused button", name, p.ButtonFocusFG)
			}
			if p.ButtonFocusBG == p.ButtonBG {
				t.Errorf("%s: ButtonFocusBG == ButtonBG (%+v) — focus does not stand out from the resting button", name, p.ButtonFocusBG)
			}
			if p.InputFG == p.InputBG {
				t.Errorf("%s: InputFG == InputBG (%+v) — illegible resting input", name, p.InputFG)
			}
			if p.InputFocusFG == p.InputFocusBG {
				t.Errorf("%s: InputFocusFG == InputFocusBG (%+v) — illegible focused input", name, p.InputFocusFG)
			}
			if p.InputFocusBG == p.InputBG {
				t.Errorf("%s: InputFocusBG == InputBG (%+v) — focus does not stand out from the resting input", name, p.InputFocusBG)
			}
		})
	}
}

// TestIssue265DefaultPaletteMatchesStockChrome is the appearance-unchanged guarantee:
// before #265 the default preset's buttons/inputs inherited turbotui's stock baseTVTheme
// values (the default branch in ApplyTheme assigned tv.DefaultTheme = baseTVTheme and
// did nothing else to those slots). The acceptance criterion is that promoting them to
// roles must NOT change the default look, so each of the eight default-palette roles must
// equal the matching baseTVTheme slot. A drift here (e.g. a guessed "stock" value that
// isn't really stock) would silently recolour every default-theme button and input.
func TestIssue265DefaultPaletteMatchesStockChrome(t *testing.T) {
	p := defaultPalette()
	stock := tvButtonInputSlots(baseTVTheme)
	for role, c := range buttonInputFields(p) {
		if c != stock[role] {
			t.Errorf("default palette %s = %+v, but baseTVTheme carries %+v — #265 must default to today's values so the default install looks identical",
				role, c, stock[role])
		}
	}
}

// TestIssue265DefaultPaletteValues pins the concrete authored default-palette values so a
// silent swap is caught even if baseTVTheme itself were to change underneath us: a green
// resting button with white text, a black resting input with white text, and the cyan
// (ANSI 6) focus fill shared with the dropdown/focus treatment.
func TestIssue265DefaultPaletteValues(t *testing.T) {
	p := defaultPalette()
	for _, tc := range []struct {
		role string
		got  tui.Color
		want tui.Color
	}{
		{"ButtonFG", p.ButtonFG, tui.ANSIColor(15)},
		{"ButtonBG", p.ButtonBG, tui.ANSIColor(2)},
		{"ButtonFocusFG", p.ButtonFocusFG, tui.ANSIColor(0)},
		{"ButtonFocusBG", p.ButtonFocusBG, tui.ANSIColor(6)},
		{"InputFG", p.InputFG, tui.ANSIColor(15)},
		{"InputBG", p.InputBG, tui.ANSIColor(0)},
		{"InputFocusFG", p.InputFocusFG, tui.ANSIColor(0)},
		{"InputFocusBG", p.InputFocusBG, tui.ANSIColor(6)},
	} {
		if tc.got != tc.want {
			t.Errorf("default %s = %+v, want %+v", tc.role, tc.got, tc.want)
		}
	}
}

// TestIssue265BlackCanvasPresetsDeriveFromRoles checks the two black-canvas presets keep
// their resting/focus button/input derivations expressed via the roles, so blackCanvasTVTheme
// sourcing them is a no-op and an override flows through. The two presets now diverge on the
// resting background: the high-contrast preset seats resting buttons/inputs on its pure-black
// panel (== PanelBG), while the dark preset seats them on its #262626 menu-bar panel
// (== MenuBarBG, lifted off the pure-black canvas so they read as cohesive chrome rather than
// black islands). Both keep focus == Accent, matching the focused dropdown. A regression that
// hardcoded the slots again (ignoring the roles) would diverge here.
func TestIssue265BlackCanvasPresetsDeriveFromRoles(t *testing.T) {
	// restingBG is the background each preset seats resting buttons/inputs on.
	restingBG := map[string]tui.Color{
		themeHighContrast: highContrastPalette().PanelBG,   // pure black
		themeDark:         darkPalette().MenuBarBG,         // #262626
	}
	for _, p := range []Theme{highContrastPalette(), darkPalette()} {
		t.Run(p.Name, func(t *testing.T) {
			wantBG := restingBG[p.Name]
			// Resting button/input sit on the preset's resting panel (PanelBG or MenuBarBG).
			if p.ButtonBG != wantBG || p.InputBG != wantBG {
				t.Errorf("%s: resting Button/Input BG %+v/%+v != expected resting BG %+v", p.Name, p.ButtonBG, p.InputBG, wantBG)
			}
			if p.ButtonFG != p.PanelFG || p.InputFG != p.PanelFG {
				t.Errorf("%s: resting Button/Input FG %+v/%+v != PanelFG %+v", p.Name, p.ButtonFG, p.InputFG, p.PanelFG)
			}
			// Focus uses the accent, matching the focused dropdown.
			if p.ButtonFocusBG != p.Accent || p.InputFocusBG != p.Accent {
				t.Errorf("%s: focus BG %+v/%+v != Accent %+v", p.Name, p.ButtonFocusBG, p.InputFocusBG, p.Accent)
			}
			// blackCanvasTVTheme must source its slots from the roles (the install is a no-op).
			cv := blackCanvasTVTheme(p)
			for role, want := range buttonInputFields(p) {
				if got := tvButtonInputSlots(cv)[role]; got != want {
					t.Errorf("%s: blackCanvasTVTheme %s = %+v, want the role value %+v (it must source from the role, not hardcode)", p.Name, role, got, want)
				}
			}
		})
	}
}

// ----------------------------------------------------------------------------
// Group B: ResolveTheme degrades all eight roles to the terminal's fidelity.
// ----------------------------------------------------------------------------

// TestIssue265ResolveThemeDegradesButtonInput verifies ResolveTheme degrades all eight
// roles with the terminal's ColorLevel, exactly like the #243/#260 roles: NO_COLOR
// collapses them to the terminal default; truecolor preserves the palette values; the RGB
// presets quantise to ANSI at 256 and 16 colours. A missing degrade() line would emit a
// colour under NO_COLOR or an out-of-gamut RGB on a 16-colour terminal.
func TestIssue265ResolveThemeDegradesButtonInput(t *testing.T) {
	t.Run("NO_COLOR collapses every role to the terminal default", func(t *testing.T) {
		for _, name := range []string{themeDefault, themeHighContrast, themeDark} {
			got := ResolveTheme(config.ThemeConfig{Name: name, NoColor: true}, truecolorEnv, false)
			for role, c := range buttonInputFields(got) {
				if c != tui.DefaultColor() {
					t.Errorf("%s NO_COLOR: %s = %+v, want terminal default", name, role, c)
				}
			}
		}
	})

	t.Run("truecolor preserves the palette values", func(t *testing.T) {
		for _, name := range []string{themeDefault, themeHighContrast, themeDark} {
			pal := paletteByName(name)
			got := ResolveTheme(config.ThemeConfig{Name: name}, truecolorEnv, false)
			for role, want := range buttonInputFields(pal) {
				if g := buttonInputFields(got)[role]; g != want {
					t.Errorf("%s truecolor: %s = %+v, want the palette %+v", name, role, g, want)
				}
			}
		}
	})

	t.Run("RGB presets degrade to ANSI at 256 and 16 colours", func(t *testing.T) {
		for _, name := range []string{themeHighContrast, themeDark} {
			for _, env := range []func(string) string{color256Env, color16Env} {
				got := ResolveTheme(config.ThemeConfig{Name: name}, env, false)
				for role, c := range buttonInputFields(got) {
					if c.Mode != tui.ColorANSI {
						t.Errorf("%s: %s = %+v, want a quantised ANSI colour (RGB must degrade)", name, role, c)
					}
				}
			}
		}
	})

	t.Run("default-palette ANSI values are fidelity-invariant", func(t *testing.T) {
		pal := defaultPalette()
		for _, env := range []func(string) string{color16Env, color256Env, truecolorEnv} {
			got := ResolveTheme(config.ThemeConfig{}, env, false)
			for role, want := range buttonInputFields(pal) {
				if g := buttonInputFields(got)[role]; g != want {
					t.Errorf("default %s at 16/256/truecolor = %+v, want %+v", role, g, want)
				}
			}
		}
	})
}

// ----------------------------------------------------------------------------
// Group C: the roles are overridable via config (applyOverrides), and the RESTING
// pairs are present in the theme editor (themeRoles); the focus pairs are config-only.
// ----------------------------------------------------------------------------

// TestIssue265ButtonInputOverridesApply checks each of the eight config override keys sets
// the matching Theme field (a missing applyOverrides case would silently drop an override —
// the exact mistake that made the menu bar non-editable in #243). It also covers graceful
// degradation: ANSI specs, case/whitespace variants, unparseable values, and unknown names.
func TestIssue265ButtonInputOverridesApply(t *testing.T) {
	marker, ok := parseColor("#12EFA0") // a distinctive RGB no preset equals
	if !ok {
		t.Fatalf("setup: parseColor(#12EFA0) failed")
	}
	for key := range buttonInputFields(defaultPalette()) {
		t.Run(key+" applies", func(t *testing.T) {
			got := paletteByName(themeDefault)
			applyOverrides(&got, map[string]string{key: "#12EFA0"})
			if buttonInputFields(got)[key] != marker {
				t.Errorf("applyOverrides({%q:#12EFA0}) left %q at %+v, want %+v — the override is silently dropped (missing applyOverrides case?)",
					key, key, buttonInputFields(got)[key], marker)
			}
		})
	}

	t.Run("each key sets ONLY its own field", func(t *testing.T) {
		// Guards against a copy-paste mis-wire (e.g. case "button_bg": t.ButtonFG = c).
		for key := range buttonInputFields(defaultPalette()) {
			got := paletteByName(themeDefault)
			before := buttonInputFields(got)
			applyOverrides(&got, map[string]string{key: "#12EFA0"})
			after := buttonInputFields(got)
			for other := range before {
				if other == key {
					continue
				}
				if after[other] != before[other] {
					t.Errorf("override %q also changed %q: %+v -> %+v — the cases are cross-wired", key, other, before[other], after[other])
				}
			}
		}
	})

	t.Run("ANSI specs apply", func(t *testing.T) {
		got := paletteByName(themeDefault)
		applyOverrides(&got, map[string]string{"button_bg": "9"})
		if got.ButtonBG != tui.ANSIColor(9) {
			t.Errorf("button_bg=9 -> %+v, want ANSI 9", got.ButtonBG)
		}
	})

	t.Run("key is case/whitespace insensitive", func(t *testing.T) {
		got := paletteByName(themeDefault)
		applyOverrides(&got, map[string]string{"  Input_BG ": "5"})
		if got.InputBG != tui.ANSIColor(5) {
			t.Errorf("normalised input_bg -> %+v, want ANSI 5", got.InputBG)
		}
	})

	t.Run("invalid value ignored", func(t *testing.T) {
		got := paletteByName(themeDefault)
		before := got.ButtonBG
		applyOverrides(&got, map[string]string{"button_bg": "nope"})
		if got.ButtonBG != before {
			t.Errorf("invalid button_bg overrode the value: %+v -> %+v", before, got.ButtonBG)
		}
	})

	t.Run("unknown name does not leak into a button/input field", func(t *testing.T) {
		got := paletteByName(themeDefault)
		before := buttonInputFields(got)
		applyOverrides(&got, map[string]string{"button": "#123456", "input": "#123456", "focus_bg": "#123456"}) // wrong keys
		for role, c := range buttonInputFields(got) {
			if c != before[role] {
				t.Errorf("unknown override key leaked into %q: %+v -> %+v", role, before[role], c)
			}
		}
	})
}

// TestIssue265RestingRolesInEditor verifies the four RESTING roles are exposed in the
// editor's themeRoles list with accessors that read the correct Theme fields. A missing
// entry would mean the role is silently not editable; a mis-wired accessor would edit the
// wrong colour.
func TestIssue265RestingRolesInEditor(t *testing.T) {
	roles := issue243RoleByKey(t)
	accessors := map[string]func(Theme) tui.Color{
		"button_fg": func(t Theme) tui.Color { return t.ButtonFG },
		"button_bg": func(t Theme) tui.Color { return t.ButtonBG },
		"input_fg":  func(t Theme) tui.Color { return t.InputFG },
		"input_bg":  func(t Theme) tui.Color { return t.InputBG },
	}
	for key, access := range accessors {
		r, ok := roles[key]
		if !ok {
			t.Errorf("themeRoles is missing %q — the resting role is not editable in the editor", key)
			continue
		}
		for _, preset := range []string{themeDefault, themeHighContrast, themeDark} {
			p := paletteByName(preset)
			if r.get(p) != access(p) {
				t.Errorf("%s: editor accessor for %q = %+v, but the Theme field = %+v (mis-wired accessor)", preset, key, r.get(p), access(p))
			}
		}
	}
}

// TestIssue265FocusRolesNotInEditor pins the documented #265 layout decision: the four FOCUS
// roles are deliberately kept OUT of the editor (a fourth row per column would grow the
// dialog past the 24-row terminal's centred ceiling), while remaining first-class config
// roles. If a future change adds them to themeRoles without growing the layout safely, this
// test flags the design divergence so the layout math is re-checked.
func TestIssue265FocusRolesNotInEditor(t *testing.T) {
	roles := issue243RoleByKey(t)
	for _, key := range []string{"button_focus_fg", "button_focus_bg", "input_focus_fg", "input_focus_bg"} {
		if _, ok := roles[key]; ok {
			t.Errorf("themeRoles unexpectedly exposes %q — #265 keeps the focus pairs out of the editor for the 24-row layout ceiling; if this is intended, re-verify the dialog height/row math", key)
		}
	}
}

// TestIssue265RestingOverrideRoundTrip checks the editor's "save then reopen" stability for
// the resting roles across every preset: a config built by the editor, reloaded via
// editedTheme and rebuilt, reproduces itself. A break here means a button/input edit would
// drift each time the dialog is reopened.
func TestIssue265RestingOverrideRoundTrip(t *testing.T) {
	for _, name := range []string{themeDefault, themeHighContrast, themeDark} {
		specs := specsFor(paletteByName(name))
		specs["button_bg"] = "#0ABCD1"
		specs["input_bg"] = "#1A1A1A"
		specs["button_fg"] = "9"
		cfg := buildThemeConfig(name, false, false, specs)

		reopened := buildThemeConfig(cfg.Name, cfg.NoColor, false, specsFor(editedTheme(cfg)))
		if !reflect.DeepEqual(reopened, cfg) {
			t.Errorf("preset %q: button/input override did not round-trip:\n got %+v\nwant %+v", name, reopened, cfg)
		}
		if reopened.Overrides["button_bg"] != "#0ABCD1" {
			t.Errorf("preset %q: round-trip lost/changed button_bg: %+v", name, reopened.Overrides)
		}
		if reopened.Overrides["input_bg"] != "#1A1A1A" {
			t.Errorf("preset %q: round-trip lost/changed input_bg: %+v", name, reopened.Overrides)
		}
	}
}

// ----------------------------------------------------------------------------
// Group D: ApplyTheme installs the eight roles onto tv.DefaultTheme's Button*/Input*
// slots (and tv.SetTheme propagates them to tv.ActiveTheme), for every preset and end
// to end through an override, plus NO_COLOR.
// ----------------------------------------------------------------------------

// TestIssue265ApplyThemeInstallsButtonInputRoles proves ApplyTheme is the bridge from the
// resolved Theme to the live widget chrome: for every preset the tv.DefaultTheme (and the
// propagated tv.ActiveTheme) Button*/Input* slots equal the theme's roles. A missing install
// would leave freshly built / live-reseeded buttons and inputs on the old palette.
func TestIssue265ApplyThemeInstallsButtonInputRoles(t *testing.T) {
	for _, th := range []Theme{issue204Default(), issue204HighContrast(), issue204Dark()} {
		t.Run(th.Name, func(t *testing.T) {
			issue204RestoreTheme(t)
			ApplyTheme(th)
			want := buttonInputFields(th)
			for role, w := range want {
				if got := tvButtonInputSlots(tv.DefaultTheme)[role]; got != w {
					t.Errorf("tv.DefaultTheme.%s = %+v, want the role %+v", role, got, w)
				}
				if got := tvButtonInputSlots(tv.ActiveTheme())[role]; got != w {
					t.Errorf("tv.ActiveTheme().%s = %+v, want %+v (tv.SetTheme must propagate the install)", role, got, w)
				}
			}
		})
	}
}

// TestIssue265ApplyThemeDefaultPresetInstallsOverStockChrome is the core #265 fix: under the
// DEFAULT preset, ApplyTheme selects the stock baseTVTheme chrome and then installs the roles
// over it — so a button_bg / input_bg override reaches tv.DefaultTheme even though the default
// branch starts from the library defaults. Before #265 the default preset ignored such
// overrides (the very gap this issue closes). It also confirms a pristine default install
// leaves the stock values intact (appearance unchanged) and does not mutate baseTVTheme.
func TestIssue265ApplyThemeDefaultPresetInstallsOverStockChrome(t *testing.T) {
	issue204RestoreTheme(t)

	t.Run("pristine default install equals stock and leaves baseTVTheme pristine", func(t *testing.T) {
		ApplyTheme(issue204Default())
		stock := tvButtonInputSlots(baseTVTheme)
		for role, got := range tvButtonInputSlots(tv.DefaultTheme) {
			if got != stock[role] {
				t.Errorf("pristine default tv.DefaultTheme.%s = %+v, want stock %+v (appearance changed)", role, got, stock[role])
			}
		}
		// baseTVTheme is the pristine snapshot the switchback path restores; the install
		// must not have mutated it through aliasing.
		if baseTVTheme.ButtonBG != tui.ANSIColor(2) || baseTVTheme.InputBG != tui.ANSIColor(0) {
			t.Errorf("baseTVTheme mutated by ApplyTheme: ButtonBG=%+v InputBG=%+v", baseTVTheme.ButtonBG, baseTVTheme.InputBG)
		}
	})

	t.Run("override on the default preset reaches tv.DefaultTheme", func(t *testing.T) {
		red := tui.RGBColor(0xFF, 0x00, 0x00)
		blue := tui.RGBColor(0x00, 0x00, 0xFF)
		th := ResolveTheme(config.ThemeConfig{
			Name:      themeDefault,
			Overrides: map[string]string{"button_bg": "#FF0000", "input_bg": "#0000FF"},
		}, truecolorEnv, false)
		if th.ButtonBG != red || th.InputBG != blue {
			t.Fatalf("setup: ResolveTheme ButtonBG=%+v InputBG=%+v, want red/blue", th.ButtonBG, th.InputBG)
		}
		ApplyTheme(th)
		if tv.DefaultTheme.ButtonBG != red {
			t.Errorf("button_bg override did not reach tv.DefaultTheme on the default preset: got %+v, want red — #265's whole point", tv.DefaultTheme.ButtonBG)
		}
		if tv.DefaultTheme.InputBG != blue {
			t.Errorf("input_bg override did not reach tv.DefaultTheme on the default preset: got %+v, want blue", tv.DefaultTheme.InputBG)
		}
		if tv.ActiveTheme().ButtonBG != red {
			t.Errorf("button_bg override did not propagate to tv.ActiveTheme(): got %+v, want red", tv.ActiveTheme().ButtonBG)
		}
	})
}

// TestIssue265ApplyThemeNoColorNeutralizes confirms that under NO_COLOR the button/input
// roles degrade to the terminal default and ApplyTheme does not fight that — the neutral
// chrome's Button*/Input* slots stay the terminal default rather than emitting a colour.
func TestIssue265ApplyThemeNoColorNeutralizes(t *testing.T) {
	issue204RestoreTheme(t)
	th := ResolveTheme(config.ThemeConfig{NoColor: true}, truecolorEnv, false)
	ApplyTheme(th)
	for role, c := range tvButtonInputSlots(tv.DefaultTheme) {
		if c != tui.DefaultColor() {
			t.Errorf("NO_COLOR tv.DefaultTheme.%s = %+v, want terminal default", role, c)
		}
	}
}

// ----------------------------------------------------------------------------
// Group E: a live theme switch and an override both recolour an already-built button
// and input — the no-restart acceptance criterion — via SessionWindow.refreshTheme.
// ----------------------------------------------------------------------------

// TestIssue265LiveSwitchRecolorsButtonsAndInput is the core #265 live-refresh property: a
// session window's buttons and input box built under the default palette are recoloured when
// the theme switches to high-contrast and refreshTheme re-runs — no restart. The sentinel
// (green default button → black high-contrast button) proves the switch took effect.
func TestIssue265LiveSwitchRecolorsButtonsAndInput(t *testing.T) {
	issue204RestoreTheme(t)
	ApplyTheme(issue204Default())
	w := newTestWorkbench(t)
	sw := w.openWindow("s", "S")

	defButtonBG := sw.sendButton.BG
	defInputBG := sw.input.BG

	hc := issue204HighContrast()
	ApplyTheme(hc)
	sw.refreshTheme()

	if sw.sendButton.BG != hc.ButtonBG {
		t.Errorf("send button BG = %+v after refresh, want the high-contrast ButtonBG %+v", sw.sendButton.BG, hc.ButtonBG)
	}
	if sw.sendButton.FocusBG != hc.ButtonFocusBG {
		t.Errorf("send button FocusBG = %+v after refresh, want %+v", sw.sendButton.FocusBG, hc.ButtonFocusBG)
	}
	if sw.input.BG != hc.InputBG {
		t.Errorf("input BG = %+v after refresh, want the high-contrast InputBG %+v", sw.input.BG, hc.InputBG)
	}
	if sw.input.FocusBG != hc.InputFocusBG {
		t.Errorf("input FocusBG = %+v after refresh, want %+v", sw.input.FocusBG, hc.InputFocusBG)
	}
	if sw.sendButton.BG == defButtonBG || sw.input.BG == defInputBG {
		t.Fatalf("button/input did not change across the switch (button %+v, input %+v) — live recolour is broken", sw.sendButton.BG, sw.input.BG)
	}
}

// TestIssue265LiveOverrideAppliesWithoutRestart is the end-to-end acceptance test: a
// button_bg / input_bg override on the DEFAULT preset reaches a real session window's
// widgets through the live ApplyTheme; refreshTheme chain (the path the SetTheme handler
// runs), so a user sees the override without restarting. This is the scenario the issue
// names ("set button_bg and input_bg ... and see it applied at startup AND on a live
// theme switch with no restart").
func TestIssue265LiveOverrideAppliesWithoutRestart(t *testing.T) {
	issue204RestoreTheme(t)
	ApplyTheme(issue204Default())
	w := newTestWorkbench(t)
	sw := w.openWindow("s", "S")

	const buttonHex, inputHex = "#262626", "#1A1A1A"
	wantButton, _ := parseColor(buttonHex)
	wantInput, _ := parseColor(inputHex)

	override := ResolveTheme(config.ThemeConfig{
		Name:      themeDefault,
		Overrides: map[string]string{"button_bg": buttonHex, "input_bg": inputHex},
	}, truecolorEnv, false)
	ApplyTheme(override)
	w.RefreshTheme()

	if sw.sendButton.BG != wantButton {
		t.Errorf("send button BG = %+v after a live button_bg override, want %+v — the override did not reach the live widget without a restart", sw.sendButton.BG, wantButton)
	}
	if sw.input.BG != wantInput {
		t.Errorf("input BG = %+v after a live input_bg override, want %+v", sw.input.BG, wantInput)
	}
	// A freshly built widget AFTER the override is installed must also carry it (the
	// "applied at startup" half — widgets seed from tv.ActiveTheme() at construction).
	fresh := newButton("Fresh", tv.Rect{}, nil)
	if fresh.BG != wantButton {
		t.Errorf("a button built after the override has BG %+v, want %+v — newly built widgets must carry the override too", fresh.BG, wantButton)
	}
}

// ----------------------------------------------------------------------------
// Group F: paletteContrast covers the four new pairings against their own backgrounds,
// each at its declared tier, and every built-in palette clears it.
// ----------------------------------------------------------------------------

// TestIssue265PaletteContrastCoversButtonInputPairs verifies the contrast audit was extended
// (#202): the button, button-focus, input and input-focus findings exist, are checked against
// the background each role actually paints on (its own fill, not the window background), and
// clear their declared minimum for every built-in palette. The resting "button" pair is held
// to the non-text/large floor (minContrastText is unreachable for the stock white-on-green the
// appearance-unchanged criterion pins, at 3.11:1); the other three are body-text tier.
func TestIssue265PaletteContrastCoversButtonInputPairs(t *testing.T) {
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
			want := map[string]struct {
				fg, bg tui.Color
				min    float64
			}{
				"button":       {p.ButtonFG, p.ButtonBG, minContrastLarge},
				"button-focus": {p.ButtonFocusFG, p.ButtonFocusBG, minContrastText},
				"input":        {p.InputFG, p.InputBG, minContrastText},
				"input-focus":  {p.InputFocusFG, p.InputFocusBG, minContrastText},
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
				if f.Min != w.min {
					t.Errorf("%s %q Min = %g, want %g", name, role, f.Min, w.min)
				}
				if !f.OK() {
					t.Errorf("%s %q fails its contrast minimum: %.2f:1 < %.1f — a low-contrast button/input pair", name, role, f.Ratio, f.Min)
				}
			}
		})
	}
}

// TestIssue265DefaultButtonContrastIsTheDocumentedGamutLimit pins the documented reason the
// resting "button" pair is held to minContrastLarge rather than the body-text tier: the stock
// white-on-green (ANSI 15 on ANSI 2) the appearance-unchanged criterion fixes scores 3.11:1,
// which clears the 3:1 non-text floor but falls short of 4.5:1. This guards the rationale: if
// a palette change lifted it to clear minContrastText, the finding's tier should be revisited;
// if it dropped below 3:1 the audit would already fail.
func TestIssue265DefaultButtonContrastIsTheDocumentedGamutLimit(t *testing.T) {
	p := defaultPalette()
	ratio := contrastRatio(p.ButtonFG, p.ButtonBG)
	if ratio < minContrastLarge {
		t.Errorf("default resting button %.2f:1 < %.1f — below even the non-text floor", ratio, minContrastLarge)
	}
	if ratio >= minContrastText {
		t.Errorf("default resting button now %.2f:1 ≥ %.1f — it clears the body-text tier, so the 'button' finding should be promoted from minContrastLarge to minContrastText", ratio, minContrastText)
	}
}

// ----------------------------------------------------------------------------
// Group G: the editor's two-column layout math holds with the #265 roles added — every
// label fits its column cell and the rows clear the "Spec:" hint row on a 24-row terminal.
// ----------------------------------------------------------------------------

// TestIssue265EditorLayoutFits replays the showThemeEditor column/row math over the live
// themeRoles list and asserts: each role's label (plus its trailing ":") fits its column's
// label cell — so no label is clipped on screen — the right column's swatch ends within the
// content area, and the last role row clears the "Spec:" hint at height-4. The button/input
// resting roles grew each column by one row (#265), so this guards the layout the issue's
// step (6) calls out. The constants mirror showThemeEditor; if they drift there, update here.
func TestIssue265EditorLayoutFits(t *testing.T) {
	const width = 80
	const height = 22
	const fieldW, swatchW = 7, 7
	columns := [...]struct{ x, labelW int }{
		{2, 20},                 // left column
		{40, themeEditorLabelW}, // right column (longest labels)
	}
	half := (len(themeRoles) + 1) / 2

	maxRow := 0
	for i, role := range themeRoles {
		col, row := 0, 4+i
		if i >= half {
			col, row = 1, 4+i-half
		}
		if row > maxRow {
			maxRow = row
		}
		lx, labelW := columns[col].x, columns[col].labelW
		// Label cell must hold the descriptive label plus its trailing ":".
		if got := len(role.label) + 1; got > labelW {
			t.Errorf("role %q label %q+\":\" is %d cols but column %d cell is only %d wide — it would clip on screen",
				role.key, role.label, got, col, labelW)
		}
		// The swatch must end within the dialog's content area (relative cols 0..width-3,
		// i.e. inside the right border at width-2).
		swatchEnd := lx + labelW + 1 + fieldW + 1 + swatchW - 1
		if swatchEnd > width-3 {
			t.Errorf("role %q swatch ends at col %d, past the content area (max %d) — it would be clipped by the border",
				role.key, swatchEnd, width-3)
		}
	}

	// The "Spec:" hint sits at height-4; the last role row must stay above it, and the
	// dialog must clear the always-on-top menu bar when centred on a 24-row terminal.
	if maxRow >= height-4 {
		t.Errorf("last role row %d collides with the Spec hint at height-4=%d — grow the height or rebalance the columns", maxRow, height-4)
	}
	const minTerminalRows = 24
	if y := (minTerminalRows - height) / 2; y < 1 {
		t.Errorf("centred dialog top y=%d on a %d-row terminal does not clear the menu bar (row 0) — height %d is too tall",
			y, minTerminalRows, height)
	}
}

// ----------------------------------------------------------------------------
// Group H: invariants the #265 change must not break — the stock-chrome switchback and
// the read-only window's input-less refresh both still hold with roles installed.
// ----------------------------------------------------------------------------

// TestIssue265SwitchbackRestoresStockButtonInput checks the #204 switchback invariant
// survives #265: after default→high-contrast→default the tv.ActiveTheme Button*/Input*
// slots are back on the stock values (the default palette equals stock), not stuck on the
// high-contrast black/accent. A reinstall bug that cached the coloured preset would fail here.
func TestIssue265SwitchbackRestoresStockButtonInput(t *testing.T) {
	issue204RestoreTheme(t)
	ApplyTheme(issue204HighContrast())
	hcButtonBG := tv.ActiveTheme().ButtonBG

	ApplyTheme(issue204Default())
	stock := tvButtonInputSlots(baseTVTheme)
	for role, got := range tvButtonInputSlots(tv.ActiveTheme()) {
		if got != stock[role] {
			t.Errorf("after switchback to default, tv.ActiveTheme().%s = %+v, want stock %+v", role, got, stock[role])
		}
	}
	if tv.ActiveTheme().ButtonBG == hcButtonBG && hcButtonBG != tui.ANSIColor(2) {
		t.Errorf("button BG stuck on the high-contrast value %+v after switchback", hcButtonBG)
	}
}

// TestIssue265ReadOnlyWindowRefreshNoPanic checks a read-only analysis window (no input
// chrome) survives a live switch that recolours button/input roles: refreshTheme returns
// before the nil input/button widgets without panicking, and its frame still follows the
// new palette. Guards the input/button reseed block sitting after the read-only return.
func TestIssue265ReadOnlyWindowRefreshNoPanic(t *testing.T) {
	issue204RestoreTheme(t)
	ApplyTheme(issue204Default())
	w := newTestWorkbench(t)
	ro := newSessionWindow(w, "analysis-1", "Saved", tv.Rect{}, true)

	if ro.input != nil || ro.sendButton != nil {
		t.Fatalf("setup: read-only window should have no input/button chrome")
	}

	ApplyTheme(issue204HighContrast())
	ro.refreshTheme() // must not panic on the nil input/button widgets

	th := tv.ActiveTheme()
	if ro.window.Content.Background.BG != th.WindowBG {
		t.Errorf("read-only frame not reseeded across the switch: content BG = %+v, want %+v", ro.window.Content.Background.BG, th.WindowBG)
	}
}

// ----------------------------------------------------------------------------
// Group I — editor Save must not destroy config-only overrides it has no field for.
// The #265 focus pairs (button_focus_*/input_focus_*) are first-class config roles
// (applyOverrides understands them) but are deliberately kept OUT of themeRoles for the
// editor's layout ceiling, so they are the first overridable-but-not-editable keys. The
// editor's save rebuilds Overrides from themeRoles alone and Gogent.SetTheme persists the
// result wholesale (g.config.Theme = t, no merge), so without a carry-forward step a Save
// — even one made for an unrelated reason — would silently erase a hand-set focus override.
// The driver closed this with carryUnexposedOverrides; this group models the real save path
// and pins both the fix and the helper's edge semantics so the regression cannot return.
// ----------------------------------------------------------------------------

// editorSave models showThemeEditor.save with no user edits: the spec fields are seeded
// from editedTheme(cur) over themeRoles (loadFields), buildThemeConfig rebuilds the config
// from those specs, then carryUnexposedOverrides carries forward any prior override the
// editor has no field for. This is the exact two-step the save closure runs before SetTheme.
func editorSave(cur config.ThemeConfig) config.ThemeConfig {
	specs := specsFor(editedTheme(cur))
	cfg := buildThemeConfig(cur.Name, cur.NoColor, cur.NoShadow, specs)
	return carryUnexposedOverrides(cfg, cur.Overrides)
}

// TestIssue265FocusOverrideSurvivesEditorSave asserts a hand-set focus-pair override
// survives a no-edit "open the editor, Save" cycle (the desired behaviour). It does NOT
// require the focus pairs to be editable — only that an existing one is not destroyed.
// The override must also still be honoured end-to-end through ResolveTheme after the save,
// proving it is genuinely preserved, not merely retained as dead config text.
func TestIssue265FocusOverrideSurvivesEditorSave(t *testing.T) {
	for _, key := range []string{"button_focus_bg", "input_focus_bg", "button_focus_fg", "input_focus_fg"} {
		t.Run(key, func(t *testing.T) {
			const spec = "#ABCDEF"
			cur := config.ThemeConfig{Name: themeDefault, Overrides: map[string]string{key: spec}}

			// Sanity: the override is honoured by the pipeline today (it's a real role).
			want, _ := parseColor(spec)
			if got := buttonInputFields(ResolveTheme(cur, truecolorEnv, false))[key]; got != want {
				t.Fatalf("setup: %s override not honoured by ResolveTheme — got %+v, want %+v", key, got, want)
			}

			saved := editorSave(cur)
			if saved.Overrides[key] != spec {
				t.Errorf("editor Save dropped the config-only %s override: saved.Overrides=%+v — a hand-set focus override must survive an unrelated Save (carryUnexposedOverrides)",
					key, saved.Overrides)
			}
			// And it must still resolve to the configured colour after the round-trip.
			if got := buttonInputFields(ResolveTheme(saved, truecolorEnv, false))[key]; got != want {
				t.Errorf("after editor Save, %s no longer resolves to the override: got %+v, want %+v", key, got, want)
			}
		})
	}
}

// TestIssue265EditorSaveCarryEdgeCases is the adversarial companion: it pins
// carryUnexposedOverrides' semantics so the carry-forward neither loses unexposed keys nor
// resurrects edited-away exposed ones.
func TestIssue265EditorSaveCarryEdgeCases(t *testing.T) {
	t.Run("exposed key is NOT carried from prior — the field is the source of truth", func(t *testing.T) {
		// The user had a button_bg override but clears the field back to the preset default
		// in the editor. buildThemeConfig drops it (field == base); the carry must not
		// resurrect the stale prior value, or an edit-away could never take effect.
		cur := config.ThemeConfig{Name: themeDefault, Overrides: map[string]string{"button_bg": "#FF0000"}}
		// Model the field being reset to the preset default (specsFor(default palette)).
		specs := specsFor(paletteByName(themeDefault))
		cfg := buildThemeConfig(cur.Name, cur.NoColor, cur.NoShadow, specs)
		cfg = carryUnexposedOverrides(cfg, cur.Overrides)
		if _, ok := cfg.Overrides["button_bg"]; ok {
			t.Errorf("carry resurrected an edited-away exposed override button_bg: %+v — exposed keys must come from the field, not prior", cfg.Overrides)
		}
	})

	t.Run("a differently-cased exposed key is not carried alongside the field value", func(t *testing.T) {
		// applyOverrides normalises keys, so "BUTTON_BG" is the same role as the field. The
		// carry must dedupe it against the exposed set rather than smuggle a second entry in.
		cur := config.ThemeConfig{Name: themeDefault, Overrides: map[string]string{"BUTTON_BG": "#FF0000"}}
		specs := specsFor(paletteByName(themeDefault)) // field at default → no button_bg override
		cfg := buildThemeConfig(cur.Name, cur.NoColor, cur.NoShadow, specs)
		cfg = carryUnexposedOverrides(cfg, cur.Overrides)
		if _, ok := cfg.Overrides["BUTTON_BG"]; ok {
			t.Errorf("carry smuggled a cased duplicate of an exposed key: %+v", cfg.Overrides)
		}
	})

	t.Run("multiple unexposed keys carried together, including odd casing, verbatim", func(t *testing.T) {
		cur := config.ThemeConfig{
			Name: themeDefault,
			Overrides: map[string]string{
				"button_focus_bg": "#111111",
				"Input_Focus_FG":  "9",       // unexposed, odd case — carried with its original key
				"button_bg":       "#FF0000", // exposed → dropped (field is source of truth)
			},
		}
		// The user also edited an exposed field (input_bg) to a non-default value.
		specs := specsFor(paletteByName(themeDefault))
		specs["input_bg"] = "#222222"
		cfg := buildThemeConfig(cur.Name, cur.NoColor, cur.NoShadow, specs)
		cfg = carryUnexposedOverrides(cfg, cur.Overrides)

		if cfg.Overrides["button_focus_bg"] != "#111111" {
			t.Errorf("unexposed button_focus_bg not carried: %+v", cfg.Overrides)
		}
		if cfg.Overrides["Input_Focus_FG"] != "9" {
			t.Errorf("unexposed odd-cased Input_Focus_FG not carried verbatim: %+v", cfg.Overrides)
		}
		if cfg.Overrides["input_bg"] != "#222222" {
			t.Errorf("edited exposed input_bg lost: %+v", cfg.Overrides)
		}
		if got := cfg.Overrides["button_bg"]; got != "" {
			t.Errorf("exposed button_bg (edited away to default) should not be carried, got %q", got)
		}
	})

	t.Run("nil prior overrides is a no-op", func(t *testing.T) {
		cfg := buildThemeConfig(themeDefault, false, false, specsFor(paletteByName(themeDefault)))
		got := carryUnexposedOverrides(cfg, nil)
		if len(got.Overrides) != 0 {
			t.Errorf("carry with nil prior produced overrides: %+v", got.Overrides)
		}
	})

	t.Run("full editor round-trip with mixed exposed+unexposed overrides is stable", func(t *testing.T) {
		// A real config: a resting override (exposed) and a focus override (unexposed).
		cur := config.ThemeConfig{
			Name: themeDefault,
			Overrides: map[string]string{
				"button_bg":      "#0ABCD1",
				"input_focus_bg": "#654321",
			},
		}
		first := editorSave(cur)
		second := editorSave(first) // reopen + save again with no edits
		if !reflect.DeepEqual(first, second) {
			t.Errorf("editor save is not idempotent across reopen:\n first=%+v\nsecond=%+v", first, second)
		}
		if first.Overrides["button_bg"] != "#0ABCD1" || first.Overrides["input_focus_bg"] != "#654321" {
			t.Errorf("mixed round-trip lost an override: %+v", first.Overrides)
		}
	})
}

// TestIssue265StopButtonStaysErrorRedOnRoleSwitch guards a cross-cutting invariant: the Stop
// button keeps the gogent error-red foreground (issue #201) even after refreshTheme reseeds
// every button from the #265 roles. Its BG follows the role, but its FG/FocusFG must not be
// clobbered by the role install.
func TestIssue265StopButtonStaysErrorRedOnRoleSwitch(t *testing.T) {
	issue204RestoreTheme(t)
	ApplyTheme(issue204Default())
	w := newTestWorkbench(t)
	sw := w.openWindow("s", "S")

	ApplyTheme(issue204HighContrast())
	sw.refreshTheme()
	th := tv.ActiveTheme()

	if sw.stopButton.FG != colorError || sw.stopButton.FocusFG != colorError {
		t.Errorf("stop button should stay error-red after a #265 role reseed: FG=%+v FocusFG=%+v, want %+v", sw.stopButton.FG, sw.stopButton.FocusFG, colorError)
	}
	if sw.stopButton.BG != th.ButtonBG {
		t.Errorf("stop button BG = %+v, want the reseeded ButtonBG %+v", sw.stopButton.BG, th.ButtonBG)
	}
}
