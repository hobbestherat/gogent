package ui

import (
	"reflect"
	"testing"

	"gogent/internal/config"

	tui "github.com/hobbestherat/turbotui"
	tv "github.com/hobbestherat/turbotui/turbotv"
)

// This file exercises issue #243 — four Theme-editor fixes:
//
//  1. STALE SWATCH: the live swatch now tracks its field's *current* text on every
//     render (driven through swatchStyle), not a colour cached when the dialog
//     opened. swatchStyle is the single source the swatch is computed from, so the
//     suite pins its contract: a valid spec paints the sample in the parsed colour,
//     an unparseable spec paints "invalid", and the disable-colours toggle wins.
//     A sequence test proves successive specs yield independent results — i.e. no
//     stale cached value survives — and a widget data-flow test runs the real
//     swatchStyle through the exact read-field→compute→set-Text/FG transform the
//     editor's updateSwatch performs.
//  2. MISSING EDITABLE COLOURS: MenuBarFG/MenuBarBG are first-class roles. The
//     suite checks they are present in themeRoles with correct accessors, that
//     every themeRoles key is honoured by applyOverrides (a missing switch case
//     would silently drop an override — a real defect), the editor round-trips a
//     menu override (applyOverrides/editedTheme/buildThemeConfig), ResolveTheme
//     populates and degrades them, ApplyTheme installs them onto tv.DefaultTheme
//     (MenuBarFG/BG and MenuHotBG) — including an override flowing end-to-end and
//     NO_COLOR neutralising the bar — and blackCanvasTVTheme reads the roles
//     rather than the old hardcoded white/black.
//  3. CRYPTIC LABELS: the labels are the screen-anchored, readable strings.
//  4. CRAMPED LAYOUT: the dialog's width/height are function-local consts the
//     editor does not expose, so they are not asserted here; the count of roles
//     (which drives the two-column layout math) is pinned instead.

// ----------------------------------------------------------------------------
// Issue #1 — swatchStyle: the field's current text drives the swatch.
// ----------------------------------------------------------------------------

// issue243DialogFG reads the swatchStyle fallback foreground at call time. swatchStyle
// reads tv.DefaultTheme.DialogFG, so capture it immediately before the call and the
// comparison stays correct regardless of whichever theme a prior test installed.
func issue243DialogFG() tui.Color { return tv.DefaultTheme.DialogFG }

// TestIssue243SwatchSampleConstant pins the glyph string a valid/neutral swatch
// paints. A silent change to it (e.g. dropping the letter pair) would alter every
// swatch render; swatchStyle returns it verbatim, so this guards the contract the
// editor seeds the Label with.
func TestIssue243SwatchSampleConstant(t *testing.T) {
	if swatchSample != "▉▉ Aa" {
		t.Fatalf("swatchSample = %q, want %q", swatchSample, "▉▉ Aa")
	}
}

// TestIssue243SwatchStyleValidSpecDrivesFG proves the core of the stale-swatch fix:
// for a valid spec the swatch paints swatchSample in the *parsed* colour, and the
// colour is exactly the one parseColor yields — so changing the field's text changes
// the swatch's foreground. This is the property the editor now relies on each render.
func TestIssue243SwatchStyleValidSpecDrivesFG(t *testing.T) {
	cases := []struct {
		name string
		spec string
		want tui.Color
	}{
		{"ansi index", "9", tui.ANSIColor(9)},
		{"ansi zero", "0", tui.ANSIColor(0)},
		{"ansi 255 boundary", "255", tui.ANSIColor(255)},
		{"ansi leading zeros", "007", tui.ANSIColor(7)},
		{"ansi with whitespace is trimmed", " 14 ", tui.ANSIColor(14)},
		{"rgb hex", "#FF0000", tui.RGBColor(0xFF, 0x00, 0x00)},
		{"rgb hex lowercase", "#abcdef", tui.RGBColor(0xAB, 0xCD, 0xEF)},
		{"rgb hex black", "#000000", tui.RGBColor(0x00, 0x00, 0x00)},
		{"default keyword", "default", tui.DefaultColor()},
		{"none keyword", "none", tui.DefaultColor()},
		{"uppercase DEFAULT", "DEFAULT", tui.DefaultColor()},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			text, fg := swatchStyle(c.spec, false)
			if text != swatchSample {
				t.Errorf("swatchStyle(%q,false).text = %q, want %q", c.spec, text, swatchSample)
			}
			if fg != c.want {
				t.Errorf("swatchStyle(%q,false).fg = %+v, want %+v (the colour must track the field)", c.spec, fg, c.want)
			}
			// The swatch foreground must equal what parseColor produces for the spec,
			// i.e. swatchStyle delegates parsing rather than inventing a colour.
			parsed, ok := parseColor(c.spec)
			if !ok || parsed != fg {
				t.Errorf("swatchStyle(%q,false).fg = %+v diverges from parseColor %+v", c.spec, fg, parsed)
			}
		})
	}
}

// TestIssue243SwatchStyleInvalidSpecShowsInvalid covers the error path: anything
// parseColor rejects paints the literal "invalid" in the dialog foreground — never
// a coloured sample, never a panic. These are the specs a user can type into a field.
func TestIssue243SwatchStyleInvalidSpecShowsInvalid(t *testing.T) {
	wantFG := issue243DialogFG()
	invalid := []string{
		"",
		" ",
		"not-a-colour",
		"red",
		"#FFF",     // 3-digit hex is not accepted (only #RRGGBB)
		"FFF",      // bare 3-char hex
		"0x1A",     // C-style hex
		"#GGGGGG",  // non-hex chars
		"#12345",   // 5 chars
		"#1234567", // 7 chars
		"256",      // one past the ANSI ceiling
		"-1",       // negative
		"1.5",      // fractional
		"0x",       // bare prefix
		"#",
	}
	for _, spec := range invalid {
		text, fg := swatchStyle(spec, false)
		if text != "invalid" {
			t.Errorf("swatchStyle(%q,false).text = %q, want %q", spec, text, "invalid")
		}
		if fg != wantFG {
			t.Errorf("swatchStyle(%q,false).fg = %+v, want dialog FG %+v", spec, fg, wantFG)
		}
	}
}

// TestIssue243SwatchStyleNoColorWins confirms the disable-colours toggle dominates:
// with noColor on, the swatch is always the neutral sample in the dialog foreground —
// even for an otherwise-valid red spec and for an unparseable spec. (The switch tests
// noColor before parseColor, so colour is fully suppressed.)
func TestIssue243SwatchStyleNoColorWins(t *testing.T) {
	wantFG := issue243DialogFG()
	for _, spec := range []string{"#FF0000", "9", "default", "", "not-a-colour", "256"} {
		text, fg := swatchStyle(spec, true)
		if text != swatchSample {
			t.Errorf("swatchStyle(%q,true).text = %q, want %q", spec, text, swatchSample)
		}
		if fg != wantFG {
			t.Errorf("swatchStyle(%q,true).fg = %+v, want dialog FG %+v", spec, fg, wantFG)
		}
	}
}

// TestIssue243SwatchStyleTracksCurrentSpecNoCache is the direct test of the
// stale-swatch bug: calling swatchStyle for a sequence of changing specs must return
// the colour of the *current* spec every time, never a value cached from an earlier
// call. (Before the fix the swatch held the colour captured at open; swatchStyle is a
// pure function of the current text, so it cannot carry stale state.)
func TestIssue243SwatchStyleTracksCurrentSpecNoCache(t *testing.T) {
	steps := []struct {
		spec string
		want tui.Color
	}{
		{"#FF0000", tui.RGBColor(0xFF, 0x00, 0x00)}, // red
		{"#00FF00", tui.RGBColor(0x00, 0xFF, 0x00)}, // green — must replace red
		{"9", tui.ANSIColor(9)},                     // ANSI bright red
		{"", tui.RGBColor(0, 0, 0)},                 // invalid → fg is dialog FG, NOT a cached colour
		{"#0000FF", tui.RGBColor(0x00, 0x00, 0xFF)}, // blue — recovers after the invalid step
	}
	wantFG := issue243DialogFG()
	for i, s := range steps {
		text, fg := swatchStyle(s.spec, false)
		if s.spec == "" {
			// The invalid step proves no colour leaks through.
			if text != "invalid" || fg != wantFG {
				t.Errorf("step %d (%q): want invalid sample, got text=%q fg=%+v", i, s.spec, text, fg)
			}
			continue
		}
		if text != swatchSample || fg != s.want {
			t.Errorf("step %d swatchStyle(%q): got text=%q fg=%+v, want text=%q fg=%+v — the swatch did not track the current field value",
				i, s.spec, text, fg, swatchSample, s.want)
		}
	}
}

// TestIssue243SwatchUpdateReflectsFieldChange runs the real swatchStyle through the
// exact transform the editor's updateSwatch closure performs (read the field's
// current text → swatchStyle → set the Label's Text/FG) against real turbotv widgets.
// It proves a field change, re-applied, repaints the swatch Label — the data flow the
// live DrawFn relies on. (The editor's updateSwatch closure is local to
// showThemeEditor and not externally reachable, so the contract is exercised via the
// shared helper it delegates to; the glue is the trivial SetText/FG pair below.)
func TestIssue243SwatchUpdateReflectsFieldChange(t *testing.T) {
	withThemeRestore(t)

	field := tv.NewTextBox("", tv.Rect{X: 0, Y: 0, W: 12, H: 1})
	sw := tv.NewLabel(swatchSample, tv.Rect{X: 0, Y: 1, W: 8, H: 1})
	sw.BG = tv.DefaultTheme.DialogBG

	apply := func(noColor bool) {
		text, fg := swatchStyle(field.GetText(), noColor)
		sw.SetText(text)
		sw.FG = fg
	}

	// Seed red, apply, then change the field to green and apply again: the Label's
	// foreground must follow the *new* field text, not the red it first showed.
	field.SetText("#FF0000")
	apply(false)
	if sw.FG != tui.RGBColor(0xFF, 0x00, 0x00) {
		t.Fatalf("after #FF0000: swatch FG = %+v, want red", sw.FG)
	}
	if sw.GetText() != swatchSample {
		t.Fatalf("after #FF0000: swatch text = %q, want %q", sw.GetText(), swatchSample)
	}

	field.SetText("#00FF00")
	apply(false)
	if sw.FG != tui.RGBColor(0x00, 0xFF, 0x00) {
		t.Fatalf("after field change to #00FF00: swatch FG = %+v, want green (swatch must track the field)", sw.FG)
	}

	// An unparseable value flips the text to "invalid" and the FG to the dialog FG.
	field.SetText("oops")
	apply(false)
	if sw.GetText() != "invalid" {
		t.Errorf("after invalid spec: swatch text = %q, want %q", sw.GetText(), "invalid")
	}

	// Toggling colour off neutralises a valid spec immediately.
	field.SetText("#0000FF")
	apply(true)
	if sw.GetText() != swatchSample || sw.FG != issue243DialogFG() {
		t.Errorf("after noColor: swatch text=%q fg=%+v, want neutral sample in dialog FG", sw.GetText(), sw.FG)
	}
}

// ----------------------------------------------------------------------------
// Issue #2 — MenuBarFG/MenuBarBG are first-class, editable roles.
// ----------------------------------------------------------------------------

// issue243RoleByKey indexes themeRoles by key for lookup in the tests below.
func issue243RoleByKey(t *testing.T) map[string]themeRole {
	t.Helper()
	m := make(map[string]themeRole, len(themeRoles))
	for _, r := range themeRoles {
		if _, dup := m[r.key]; dup {
			t.Fatalf("duplicate themeRole key %q", r.key)
		}
		m[r.key] = r
	}
	return m
}

// TestIssue243ThemeRolesCount pins the number of exposed roles. The editor lays them
// out in two columns sized from len(themeRoles) (half := (len+1)/2), so a wrong count
// either overflows the widened dialog or leaves empty rows — a layout regression.
func TestIssue243ThemeRolesCount(t *testing.T) {
	if got, want := len(themeRoles), 17; got != want {
		t.Fatalf("len(themeRoles) = %d, want %d (the two-column layout is sized from this)", got, want)
	}
}

// TestIssue243ThemeRolesExposeMenuBar verifies the two newly-promoted roles are in
// the editor's list with accessors that read the right Theme fields. A missing or
// wrongly-wired accessor would mean the role is silently not editable (the original
// bug) or edits the wrong colour.
func TestIssue243ThemeRolesExposeMenuBar(t *testing.T) {
	roles := issue243RoleByKey(t)

	fg, ok := roles["menu_bar_fg"]
	if !ok {
		t.Fatalf("themeRoles is missing menu_bar_fg — the menu bar foreground is not editable (issue #2)")
	}
	bg, ok := roles["menu_bar_bg"]
	if !ok {
		t.Fatalf("themeRoles is missing menu_bar_bg — the menu bar background is not editable (issue #2)")
	}

	// Distinct Themes must yield distinct accessor results, proving each accessor
	// reads its own field rather than (say) both reading PanelBG.
	a := defaultPalette()
	b := defaultPalette()
	b.MenuBarFG = tui.ANSIColor(5)
	b.MenuBarBG = tui.ANSIColor(6)
	if fg.get(a) == fg.get(b) {
		t.Errorf("menu_bar_fg.get does not reflect Theme.MenuBarFG: %+v == %+v", fg.get(a), fg.get(b))
	}
	if bg.get(a) == bg.get(b) {
		t.Errorf("menu_bar_bg.get does not reflect Theme.MenuBarBG: %+v == %+v", bg.get(a), bg.get(b))
	}
	// And the two accessors are independent (fg ≠ bg wiring).
	c := defaultPalette()
	c.MenuBarFG = tui.ANSIColor(3)
	c.MenuBarBG = tui.ANSIColor(4)
	if fg.get(c) == bg.get(c) {
		t.Errorf("menu_bar_fg and menu_bar_bg accessors are not independent: both %+v", fg.get(c))
	}
}

// TestIssue243EveryRoleKeyHonouredByApplyOverrides is the key defect-hunter for #2:
// for every role the editor exposes, applyOverrides must actually set the matching
// Theme field. A role present in themeRoles but missing from applyOverrides' switch
// (the exact mistake that made the menu bar non-editable) would silently drop the
// override — the editor would show and accept a value that never round-trips.
// Each override is a distinctive RGB no default equals, so the check is unambiguous.
func TestIssue243EveryRoleKeyHonouredByApplyOverrides(t *testing.T) {
	roles := issue243RoleByKey(t)
	marker, ok := parseColor("#12EFA0") // a distinctive RGB unlikely to be any preset default
	if !ok {
		t.Fatalf("setup: parseColor(#12EFA0) failed")
	}
	for key, role := range roles {
		t.Run(key, func(t *testing.T) {
			base := defaultPalette()
			got := paletteByName(themeDefault) // fresh copy
			applyOverrides(&got, map[string]string{key: "#12EFA0"})
			if role.get(got) != marker {
				t.Errorf("applyOverrides({%q:#12EFA0}) did not set the role: got %+v, want %+v — "+
					"this role is exposed in the editor but its override is silently dropped (missing applyOverrides case?)", key, role.get(got), marker)
			}
			if role.get(got) == role.get(base) {
				t.Errorf("applyOverrides({%q:#12EFA0}) left the role at its default %+v", key, role.get(base))
			}
		})
	}
}

// TestIssue243ApplyOverridesMenuBar covers the menu override paths directly: both
// roles apply, they are independent, and an unparseable value or unknown name is
// ignored rather than corrupting the Theme (graceful degradation on a typo).
func TestIssue243ApplyOverridesMenuBar(t *testing.T) {
	t.Run("both roles apply independently", func(t *testing.T) {
		tt := defaultPalette()
		applyOverrides(&tt, map[string]string{
			"menu_bar_fg": "#0000AA",
			"menu_bar_bg": "#AA0000",
		})
		if tt.MenuBarFG != tui.RGBColor(0x00, 0x00, 0xAA) {
			t.Errorf("MenuBarFG = %+v, want #0000AA", tt.MenuBarFG)
		}
		if tt.MenuBarBG != tui.RGBColor(0xAA, 0x00, 0x00) {
			t.Errorf("MenuBarBG = %+v, want #AA0000", tt.MenuBarBG)
		}
	})

	t.Run("ANSI specs apply too", func(t *testing.T) {
		tt := defaultPalette()
		applyOverrides(&tt, map[string]string{"menu_bar_bg": "0"})
		if tt.MenuBarBG != tui.ANSIColor(0) {
			t.Errorf("MenuBarBG = %+v, want ANSI 0", tt.MenuBarBG)
		}
	})

	t.Run("invalid value ignored", func(t *testing.T) {
		tt := defaultPalette()
		before := tt.MenuBarBG
		applyOverrides(&tt, map[string]string{"menu_bar_bg": "nope"})
		if tt.MenuBarBG != before {
			t.Errorf("invalid menu_bar_bg overrode the value: %+v -> %+v", before, tt.MenuBarBG)
		}
	})

	t.Run("unknown name ignored", func(t *testing.T) {
		tt := defaultPalette()
		before := tt.MenuBarBG
		applyOverrides(&tt, map[string]string{"menu_bar": "#123456"}) // wrong key
		if tt.MenuBarBG != before {
			t.Errorf("unknown key leaked into MenuBarBG: %+v -> %+v", before, tt.MenuBarBG)
		}
	})

	t.Run("name is case/whitespace insensitive", func(t *testing.T) {
		tt := defaultPalette()
		applyOverrides(&tt, map[string]string{"  Menu_Bar_BG ": "5"})
		if tt.MenuBarBG != tui.ANSIColor(5) {
			t.Errorf("normalised menu_bar_bg key did not apply: MenuBarBG = %+v, want ANSI 5", tt.MenuBarBG)
		}
	})
}

// TestIssue243EditedThemeAppliesMenuBarOverride checks the editor's "current theme"
// builder layers a saved menu override on top of each preset palette.
func TestIssue243EditedThemeAppliesMenuBarOverride(t *testing.T) {
	for _, name := range []string{themeDefault, themeHighContrast, themeDark} {
		cfg := config.ThemeConfig{Name: name, Overrides: map[string]string{
			"menu_bar_fg": "#112233",
			"menu_bar_bg": "#FEDCBA",
		}}
		got := editedTheme(cfg)
		if got.MenuBarFG != tui.RGBColor(0x11, 0x22, 0x33) {
			t.Errorf("preset %q: editedTheme MenuBarFG = %+v, want #112233", name, got.MenuBarFG)
		}
		if got.MenuBarBG != tui.RGBColor(0xFE, 0xDC, 0xBA) {
			t.Errorf("preset %q: editedTheme MenuBarBG = %+v, want #FEDCBA", name, got.MenuBarBG)
		}
		// Other roles stay at the preset's own values (override is scoped).
		base := paletteByName(name)
		if got.User != base.User {
			t.Errorf("preset %q: menu override leaked into User", name)
		}
	}
}

// TestIssue243BuildThemeConfigMenuBarOverride covers the editor assembling an override
// for the new roles: a changed menu spec becomes an override, while a spec equal to
// the preset's own value records nothing (a pristine preset stays pristine).
func TestIssue243BuildThemeConfigMenuBarOverride(t *testing.T) {
	t.Run("changed menu_bar_bg is recorded", func(t *testing.T) {
		specs := specsFor(paletteByName(themeDefault))
		specs["menu_bar_bg"] = "#FF0000"
		specs["menu_bar_fg"] = "#00FF00"
		got := buildThemeConfig("default", false, false, specs)
		if got.Overrides["menu_bar_bg"] != "#FF0000" {
			t.Errorf("menu_bar_bg override = %q, want #FF0000", got.Overrides["menu_bar_bg"])
		}
		if got.Overrides["menu_bar_fg"] != "#00FF00" {
			t.Errorf("menu_bar_fg override = %q, want #00FF00", got.Overrides["menu_bar_fg"])
		}
	})

	t.Run("pristine default records no menu override", func(t *testing.T) {
		specs := specsFor(paletteByName(themeDefault))
		got := buildThemeConfig("default", false, false, specs)
		if _, ok := got.Overrides["menu_bar_bg"]; ok {
			t.Errorf("pristine default recorded menu_bar_bg override: %+v", got.Overrides)
		}
		if _, ok := got.Overrides["menu_bar_fg"]; ok {
			t.Errorf("pristine default recorded menu_bar_fg override: %+v", got.Overrides)
		}
	})

	t.Run("menu spec equal to the preset value records nothing", func(t *testing.T) {
		// high-contrast MenuBarBG is pure black; seeding the same spec is a no-op.
		specs := specsFor(paletteByName(themeHighContrast))
		got := buildThemeConfig("high-contrast", false, false, specs)
		if _, ok := got.Overrides["menu_bar_bg"]; ok {
			t.Errorf("preset-value menu_bar_bg should not be an override: %+v", got.Overrides)
		}
	})
}

// TestIssue243MenuBarRoundTrip checks the "save then reopen" stability property for
// the new roles across every preset: a config built by the editor, reloaded via
// editedTheme and rebuilt, reproduces itself. A break here means a menu edit would
// drift each time the dialog is reopened.
func TestIssue243MenuBarRoundTrip(t *testing.T) {
	for _, name := range []string{themeDefault, themeHighContrast, themeDark} {
		specs := specsFor(paletteByName(name))
		specs["menu_bar_bg"] = "#0ABCD1"
		cfg := buildThemeConfig(name, false, false, specs)

		reopened := buildThemeConfig(cfg.Name, cfg.NoColor, false, specsFor(editedTheme(cfg)))
		if !reflect.DeepEqual(reopened, cfg) {
			t.Errorf("preset %q: menu override did not round-trip:\n got %+v\nwant %+v", name, reopened, cfg)
		}
		if reopened.Overrides["menu_bar_bg"] != "#0ABCD1" {
			t.Errorf("preset %q: round-trip lost/changed menu_bar_bg: %+v", name, reopened.Overrides)
		}
	}
}

// ----------------------------------------------------------------------------
// Issue #2 — ResolveTheme populates and degrades the menu roles.
// ----------------------------------------------------------------------------

// TestIssue243PalettesPopulateMenuBar checks every built-in palette fills the new
// roles (so the bar is never blank) and that the bar reads as part of its canvas:
// on the black-canvas presets MenuBarFG/BG equal PanelFG/PanelBG, and the foreground
// is distinct from the background so the bar is legible.
func TestIssue243PalettesPopulateMenuBar(t *testing.T) {
	t.Run("default matches the stock black-on-grey bar", func(t *testing.T) {
		p := defaultPalette()
		if p.MenuBarFG != tui.ANSIColor(0) {
			t.Errorf("default MenuBarFG = %+v, want ANSI 0 (black text)", p.MenuBarFG)
		}
		if p.MenuBarBG != tui.ANSIColor(7) {
			t.Errorf("default MenuBarBG = %+v, want ANSI 7 (light-grey bar)", p.MenuBarBG)
		}
	})

	for name, pal := range map[string]func() Theme{
		themeDefault:      defaultPalette,
		themeHighContrast: highContrastPalette,
		themeDark:         darkPalette,
	} {
		t.Run(name+" bar is populated and legible", func(t *testing.T) {
			p := pal()
			// No palette leaves the bar unset.
			if reflect.DeepEqual(p.MenuBarFG, tui.Color{}) || reflect.DeepEqual(p.MenuBarBG, tui.Color{}) {
				t.Fatalf("preset %q left a menu role at the zero Color", name)
			}
			// Foreground must differ from background or the bar text is invisible.
			if p.MenuBarFG == p.MenuBarBG {
				t.Errorf("preset %q: MenuBarFG == MenuBarBG (%+v) — illegible bar", name, p.MenuBarFG)
			}
			switch name {
			case themeHighContrast:
				// The pure-black high-contrast canvas blends the bar into the panel
				// chrome (white-on-black, MenuBar == Panel).
				if p.MenuBarFG != p.PanelFG {
					t.Errorf("preset %q: MenuBarFG %+v != PanelFG %+v", name, p.MenuBarFG, p.PanelFG)
				}
				if p.MenuBarBG != p.PanelBG {
					t.Errorf("preset %q: MenuBarBG %+v != PanelBG %+v", name, p.MenuBarBG, p.PanelBG)
				}
			case themeDark:
				// The dark preset lifts the bar onto a distinct #262626 dark-grey
				// panel off the pure-black canvas (the bar text still matches the
				// panel foreground).
				if p.MenuBarFG != p.PanelFG {
					t.Errorf("preset %q: MenuBarFG %+v != PanelFG %+v", name, p.MenuBarFG, p.PanelFG)
				}
				if want := tui.RGBColor(0x26, 0x26, 0x26); p.MenuBarBG != want {
					t.Errorf("preset %q: MenuBarBG = %+v, want %+v (distinct #262626 off the black canvas)", name, p.MenuBarBG, want)
				}
				if p.MenuBarBG == p.PanelBG {
					t.Errorf("preset %q: MenuBarBG equals the black PanelBG; the bar should be lifted off the canvas", name)
				}
			}
		})
	}
}

// TestIssue243ResolveThemeDegradesMenuBar verifies ResolveTheme degrades the menu
// roles with the terminal's fidelity: NO_COLOR collapses them to the terminal
// default, while truecolor preserves the palette values (so a real terminal shows
// the authored bar). A missing degrade line would emit ANSI/RGB under NO_COLOR.
func TestIssue243ResolveThemeDegradesMenuBar(t *testing.T) {
	t.Run("NO_COLOR degrades to terminal default", func(t *testing.T) {
		for _, name := range []string{themeDefault, themeHighContrast, themeDark} {
			got := ResolveTheme(config.ThemeConfig{Name: name, NoColor: true}, truecolorEnv, false)
			if got.MenuBarFG != tui.DefaultColor() {
				t.Errorf("preset %q NO_COLOR: MenuBarFG = %+v, want default", name, got.MenuBarFG)
			}
			if got.MenuBarBG != tui.DefaultColor() {
				t.Errorf("preset %q NO_COLOR: MenuBarBG = %+v, want default", name, got.MenuBarBG)
			}
		}
	})

	t.Run("truecolor preserves the palette values", func(t *testing.T) {
		for _, name := range []string{themeDefault, themeHighContrast, themeDark} {
			pal := paletteByName(name)
			got := ResolveTheme(config.ThemeConfig{Name: name}, truecolorEnv, false)
			if got.MenuBarFG != pal.MenuBarFG {
				t.Errorf("preset %q truecolor: MenuBarFG = %+v, want %+v", name, got.MenuBarFG, pal.MenuBarFG)
			}
			if got.MenuBarBG != pal.MenuBarBG {
				t.Errorf("preset %q truecolor: MenuBarBG = %+v, want %+v", name, got.MenuBarBG, pal.MenuBarBG)
			}
		}
	})

	t.Run("override survives degrade at truecolor", func(t *testing.T) {
		got := ResolveTheme(config.ThemeConfig{
			Name:      themeDefault,
			Overrides: map[string]string{"menu_bar_bg": "#0A0A0A"},
		}, truecolorEnv, false)
		if got.MenuBarBG != tui.RGBColor(0x0A, 0x0A, 0x0A) {
			t.Errorf("degraded MenuBarBG = %+v, want #0A0A0A", got.MenuBarBG)
		}
	})
}

// ----------------------------------------------------------------------------
// Issue #2 — ApplyTheme installs the menu roles onto tv.DefaultTheme.
// ----------------------------------------------------------------------------

// TestIssue243ApplyThemeInstallsMenuBarRoles proves ApplyTheme is the bridge from the
// resolved Theme to the live turbotui chrome: for every preset the bar's foreground,
// background and hot-key background come from the theme roles. A missing install
// would leave the bar on turbotui's library default and ignore an override.
func TestIssue243ApplyThemeInstallsMenuBarRoles(t *testing.T) {
	for _, th := range []Theme{issue204Default(), issue204HighContrast(), issue204Dark()} {
		t.Run(th.Name, func(t *testing.T) {
			issue204RestoreTheme(t)
			ApplyTheme(th)
			if got := tv.DefaultTheme.MenuBarFG; got != th.MenuBarFG {
				t.Errorf("tv.DefaultTheme.MenuBarFG = %+v, want %+v", got, th.MenuBarFG)
			}
			if got := tv.DefaultTheme.MenuBarBG; got != th.MenuBarBG {
				t.Errorf("tv.DefaultTheme.MenuBarBG = %+v, want %+v", got, th.MenuBarBG)
			}
			// The hot-key cell stays flush with the bar (issue #243 plan).
			if got := tv.DefaultTheme.MenuHotBG; got != th.MenuBarBG {
				t.Errorf("tv.DefaultTheme.MenuHotBG = %+v, want MenuBarBG %+v", got, th.MenuBarBG)
			}
		})
	}
}

// TestIssue243ApplyThemeMenuBarOverrideFlowsThrough is the end-to-end test for the
// newly-editable bar: a menu_bar_bg override configured in the editor flows through
// ResolveTheme → ApplyTheme onto tv.DefaultTheme's bar background (and the hot-key
// background that follows it). This is the exact path the editor's Save runs.
func TestIssue243ApplyThemeMenuBarOverrideFlowsThrough(t *testing.T) {
	issue204RestoreTheme(t)

	red := tui.RGBColor(0xFF, 0x00, 0x00)
	th := ResolveTheme(config.ThemeConfig{
		Name:      themeDefault,
		Overrides: map[string]string{"menu_bar_bg": "#FF0000"},
	}, truecolorEnv, false)
	if th.MenuBarBG != red {
		t.Fatalf("setup: ResolveTheme MenuBarBG = %+v, want red", th.MenuBarBG)
	}
	ApplyTheme(th)
	if tv.DefaultTheme.MenuBarBG != red {
		t.Errorf("override did not reach tv.DefaultTheme.MenuBarBG: got %+v, want red", tv.DefaultTheme.MenuBarBG)
	}
	if tv.DefaultTheme.MenuHotBG != red {
		t.Errorf("override did not reach tv.DefaultTheme.MenuHotBG: got %+v, want red", tv.DefaultTheme.MenuHotBG)
	}
	// The foreground role is independent of the background override.
	if tv.DefaultTheme.MenuBarFG != th.MenuBarFG {
		t.Errorf("MenuBarFG drifted: got %+v, want %+v", tv.DefaultTheme.MenuBarFG, th.MenuBarFG)
	}
}

// TestIssue243ApplyThemeNoColorNeutralizesMenuBar confirms that under NO_COLOR the
// bar degrades to the terminal default and ApplyTheme does not fight that by
// re-installing a colour — the neutral bar is left untouched.
func TestIssue243ApplyThemeNoColorNeutralizesMenuBar(t *testing.T) {
	issue204RestoreTheme(t)

	th := ResolveTheme(config.ThemeConfig{Name: themeDefault, NoColor: true}, truecolorEnv, false)
	ApplyTheme(th)
	if tv.DefaultTheme.MenuBarFG != tui.DefaultColor() {
		t.Errorf("NO_COLOR MenuBarFG = %+v, want terminal default", tv.DefaultTheme.MenuBarFG)
	}
	if tv.DefaultTheme.MenuBarBG != tui.DefaultColor() {
		t.Errorf("NO_COLOR MenuBarBG = %+v, want terminal default", tv.DefaultTheme.MenuBarBG)
	}
}

// ----------------------------------------------------------------------------
// Issue #2 — blackCanvasTVTheme reads the menu roles (regression guard).
// ----------------------------------------------------------------------------

// TestIssue243BlackCanvasThemeReadsMenuBarRoles guards the central refactor of #2:
// the high-contrast/dark chrome builder must source the bar colours from the
// (overridable) MenuBarFG/BG roles, not the old hardcoded white/black constants. A
// regression would make a menu override apply to tv.DefaultTheme but not survive the
// black-canvas chrome switch, so the bar would ignore the user's edit.
func TestIssue243BlackCanvasThemeReadsMenuBarRoles(t *testing.T) {
	for _, th := range []Theme{issue204HighContrast(), issue204Dark()} {
		cv := blackCanvasTVTheme(th)
		if cv.MenuBarFG != th.MenuBarFG {
			t.Errorf("%s: blackCanvasTVTheme MenuBarFG = %+v, want role %+v (not the old hardcoded white)", th.Name, cv.MenuBarFG, th.MenuBarFG)
		}
		if cv.MenuBarBG != th.MenuBarBG {
			t.Errorf("%s: blackCanvasTVTheme MenuBarBG = %+v, want role %+v (not the old hardcoded black)", th.Name, cv.MenuBarBG, th.MenuBarBG)
		}
		if cv.MenuHotBG != th.MenuBarBG {
			t.Errorf("%s: blackCanvasTVTheme MenuHotBG = %+v, want MenuBarBG %+v", th.Name, cv.MenuHotBG, th.MenuBarBG)
		}

		// Override the role on an otherwise-pristine theme and confirm the chrome
		// builder picks it up — the property an editor edit depends on.
		edited := th
		edited.MenuBarBG = tui.RGBColor(0x01, 0x23, 0x45)
		edited.MenuBarFG = tui.RGBColor(0x67, 0x89, 0xAB)
		cv2 := blackCanvasTVTheme(edited)
		if cv2.MenuBarBG != edited.MenuBarBG || cv2.MenuBarFG != edited.MenuBarFG {
			t.Errorf("%s: blackCanvasTVTheme ignored a menu role override: got FG %+v BG %+v", th.Name, cv2.MenuBarFG, cv2.MenuBarBG)
		}
	}
}

// ----------------------------------------------------------------------------
// Issue #3 — labels are readable, screen-anchored descriptions.
// ----------------------------------------------------------------------------

// issue243WantLabels is the exact set of descriptive labels the issue specifies,
// keyed by role. Asserting the literal strings catches a typo or a missed rename.
var issue243WantLabels = map[string]string{
	"user":        "User messages",
	"agent":       "Agent replies",
	"note":        "Thoughts / idle",
	"tool":        "Tool calls",
	"result":      "Tool results",
	"info":        "System notes",
	"error":       "Errors",
	"desktop_fg":  "Desktop hint text",
	"desktop_bg":  "Desktop background",
	"panel_fg":    "Sidebar text",
	"panel_bg":    "Sidebar background",
	"title":       "Panel titles",
	"divider":     "Borders / dividers",
	"accent":      "Indicators / badges",
	"menu_bar_fg": "Menu bar text",
	"menu_bar_bg": "Menu bar background",
	"code_bg":     "Code block background",
}

// issue243CrypticLabels are the terse pre-#243 labels a rename must move away from.
var issue243CrypticLabels = map[string]bool{
	"User": true, "Agent": true, "Note": true, "Tool": true, "Result": true,
	"Info": true, "Error": true, "Desktop FG": true, "Desktop BG": true,
	"Panel FG": true, "Panel BG": true, "Title": true, "Divider": true,
	"Accent": true, "Code BG": true,
}

// TestIssue243ThemeRolesLabelsReadable pins every role's label to the descriptive
// text the issue requires.
func TestIssue243ThemeRolesLabelsReadable(t *testing.T) {
	for _, r := range themeRoles {
		want, ok := issue243WantLabels[r.key]
		if !ok {
			t.Errorf("no expected label for role key %q (add it to issue243WantLabels)", r.key)
			continue
		}
		if r.label != want {
			t.Errorf("role %q label = %q, want %q", r.key, r.label, want)
		}
		if r.label == "" {
			t.Errorf("role %q has an empty label", r.key)
		}
		if issue243CrypticLabels[r.label] {
			t.Errorf("role %q still uses the cryptic label %q", r.key, r.label)
		}
	}
}

// TestIssue243ThemeRolesLabelsUnique verifies no two roles share a label, so the
// two-column list distinguishes every colour at a glance (the point of the rename).
func TestIssue243ThemeRolesLabelsUnique(t *testing.T) {
	seen := make(map[string]string, len(themeRoles))
	for _, r := range themeRoles {
		if prev, dup := seen[r.label]; dup {
			t.Errorf("label %q used by both %q and %q", r.label, prev, r.key)
		}
		seen[r.label] = r.key
	}
}

// TestIssue243ThemeRolesLabelsFitColumn is the regression guard for the label-clipping
// defect found in round 1 (the code_bg label "Code block background:" was 22 columns but
// the label cell was only 20, so a 1-row wrapped Label dropped "background:" and showed
// "Code block"). It asserts every role label (plus its trailing ":") fits the label-cell
// width the editor allocates (themeEditorLabelW), so a longer label or a narrowed cell
// cannot reintroduce the clip. It runs off the public const, so it tracks the editor.
func TestIssue243ThemeRolesLabelsFitColumn(t *testing.T) {
	var longest string
	for _, r := range themeRoles {
		n := len(r.label) + 1 // label + ":"
		if n > len(longest) {
			longest = r.label + ":"
		}
		if n > themeEditorLabelW {
			t.Errorf("role %q label %q is %d columns with ':', but the label cell (themeEditorLabelW) "+
				"is only %d wide — a 1-row wrapped Label would clip it on screen",
				r.key, r.label, n, themeEditorLabelW)
		}
	}
	// The const is documented to hold the longest label; pin that it actually does,
	// so a future label edit that needs more width fails here rather than silently clipping.
	if want := len(longest); themeEditorLabelW < want {
		t.Errorf("themeEditorLabelW = %d is narrower than the longest label %q (%d cols)",
			themeEditorLabelW, longest, want)
	}
}

// TestIssue243CodeBlockLabelRendersFully is the render-verified regression guard for the
// round-1 clipping defect: the code_bg label "Code block background:" (22 cols) must
// render in full, not be wrapped/clipped to "Code block". It opens the real editor,
// renders it, and asserts the full label text is on screen. (Previously this was a
// failing defect test; the driver widened the label cell via themeEditorLabelW=22, so it
// now passes and guards against a regression.)
func TestIssue243CodeBlockLabelRendersFully(t *testing.T) {
	issue204RestoreTheme(t)
	w := newTestWorkbench(t)
	w.SetHandlers(Handlers{
		GetTheme: func() config.ThemeConfig { return config.ThemeConfig{} },
		SetTheme: func(config.ThemeConfig) {},
	})
	w.showThemeEditor()
	screen := screenText(w)

	if !containsOnScreen(screen, "Code block") {
		t.Fatalf("setup: the code_bg label did not render at all — screen lacks 'Code block'")
	}
	// "Code block background" (21 cols) cannot fit in any cell narrower than 21, so its
	// presence proves the label cell is wide enough and the label is not clipped.
	if !containsOnScreen(screen, "Code block background") {
		t.Errorf("REGRESSION: the 'Code block background' label is clipped on screen — "+
			"themeEditorLabelW (%d) is too narrow for the 22-column label, so the wrapped Label "+
			"dropped the 'background:' half", themeEditorLabelW)
	}
}

// TestIssue243DialogFitsEightyColumnTerminal guards the round-1 width fix for the second
// round-1 concern: the dialog was widened to 84, which overflowed a standard 80-column
// terminal (centeredDialog only clamps the origin; it does not scale an oversized dialog,
// so the right column and the Cancel button were clipped). The driver reduced the width to
// 80. This renders the real editor in the 80-wide test buffer and asserts the whole dialog
// — borders, the right column's labels/fields/swatches, and the Cancel button — is visible
// within 80 columns (the right border is present at the last column, not overwritten).
func TestIssue243DialogFitsEightyColumnTerminal(t *testing.T) {
	issue204RestoreTheme(t)
	w := newTestWorkbench(t)
	w.SetHandlers(Handlers{
		GetTheme: func() config.ThemeConfig { return config.ThemeConfig{} },
		SetTheme: func(config.ThemeConfig) {},
	})
	w.showThemeEditor()
	w.desktop.Redraw()

	width := w.app.Width()
	if width < 80 {
		t.Fatalf("setup: test app is only %d cols wide; need ≥80 to validate an 80-col fit", width)
	}

	// The dialog's right border must be present on its rows. A right-column widget that
	// overflowed the content area would overwrite the border cell, so an intact border at
	// the last column proves nothing spills past the dialog. Find the dialog's top border
	// row (the one beginning with '╔') and the right border column, then check every dialog
	// row has a vertical border there.
	rowAt := func(y int) []rune {
		out := make([]rune, width)
		for x := 0; x < width; x++ {
			ch := w.app.ReadCell(x, y).Ch
			if ch == 0 {
				ch = ' '
			}
			out[x] = ch
		}
		return out
	}
	// Locate the right border column from the top frame.
	borderCol := -1
	topRow := -1
	for y := 0; y < w.app.Height(); y++ {
		r := rowAt(y)
		if len(r) > 0 && r[0] == '╔' {
			topRow = y
			for x := len(r) - 1; x > 0; x-- {
				if r[x] == '╗' {
					borderCol = x
					break
				}
			}
			break
		}
	}
	if topRow < 0 || borderCol < 0 {
		t.Fatalf("could not locate the dialog's top frame (╔…╗) in the render")
	}
	// Every body row from just under the top frame down to the bottom frame must carry the
	// vertical border at borderCol (║), i.e. no right-column widget overwrote it.
	for y := topRow + 1; y < w.app.Height(); y++ {
		r := rowAt(y)
		if r[0] == '╚' { // bottom frame reached
			if r[borderCol] != '╝' {
				t.Errorf("bottom-right corner missing at col %d: got %q, want '╝'", borderCol, string(r[borderCol]))
			}
			break
		}
		if r[borderCol] != '║' {
			t.Errorf("dialog row %d: right border at col %d is %q, want '║' — a right-column widget overflowed the dialog",
				y, borderCol, string(r[borderCol]))
		}
	}

	// The right-column code_bg row must show its full label, field and swatch inside the border.
	// Button captions (not the bracketed literal): since turbotui#259 pins the [ ] brackets to
	// the face bounds, a button whose bounds are wider than its label (e.g. Save's W:9 vs
	// buttonWidth("Save")=8) renders the caption centred between flush brackets ("[ Save  ]"),
	// so assert the caption is on screen rather than a contiguous "[ Save ]". Clipping/overflow
	// is already guarded by the right-border integrity check above.
	screen := screenText(w)
	for _, needle := range []string{"Code block background", "Sidebar text", "Cancel", "Save"} {
		if !containsOnScreen(screen, needle) {
			t.Errorf("expected %q to be visible inside the 80-col dialog; it was clipped or absent", needle)
		}
	}
}

// TestIssue243AllRoleLabelsRenderFully is a black-box guard for the round-2 asymmetric
// columns (left label cell 19, right 22): it renders the real editor and asserts every
// role's full readable label is on screen. The left column carries the shorter labels
// in a narrower cell, so a label growing past 19 — or a themeRoles reorder that moves a
// long label into the left half — would clip with no other signal. Checking each label's
// full text (not its prefix) catches clipping in either column without coupling to the
// private column widths/offsets.
func TestIssue243AllRoleLabelsRenderFully(t *testing.T) {
	issue204RestoreTheme(t)
	w := newTestWorkbench(t)
	w.SetHandlers(Handlers{
		GetTheme: func() config.ThemeConfig { return config.ThemeConfig{} },
		SetTheme: func(config.ThemeConfig) {},
	})
	w.showThemeEditor()
	screen := screenText(w)

	for _, r := range themeRoles {
		want, ok := issue243WantLabels[r.key]
		if !ok {
			t.Errorf("no expected label for role %q", r.key)
			continue
		}
		if !containsOnScreen(screen, want) {
			t.Errorf("role %q full label %q is not visible on screen — it is clipped by its (column-specific) label cell; "+
				"check the label fits the cell it lands in (left column is narrower than themeEditorLabelW)",
				r.key, want)
		}
	}
}
