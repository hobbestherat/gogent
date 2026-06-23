package ui

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"gogent/internal/config"

	tui "github.com/hobbestherat/turbotui"
	tv "github.com/hobbestherat/turbotui/turbotv"
)

// This file exercises issue #327: a dedicated list background/foreground theme
// role (list_bg / list_fg) for the four filterable dialog lists — the Resources
// browser, Saved Sessions browser, command palette and @-mention popup — wired
// through the same role machinery code_bg/#265 established (palette → ResolveTheme
// degrade → applyOverrides → ApplyTheme install onto tv.DefaultTheme's List* slots
// → tv.SetTheme), exposed (list_bg only) in the editor, carried (list_fg) by
// carryUnexposedOverrides, and audited by paletteContrast.
//
// IMPORTANT: the merged turbotui side intentionally did NOT make NewTree seed from
// ListBG (ListBG defaults to DialogBG == SelectionBG under the stock DefaultTheme,
// which would make an un-recoloured tree's selection invisible). gogent therefore
// reads tv.DefaultTheme.ListBG/ListFG DIRECTLY at the four sites. These tests never
// assert "NewTree seeds from ListBG"; they pin the gogent-side wiring and the four
// sites' end-to-end behaviour.
//
// The acceptance criteria the suite pins:
//   - with no override every preset's list reads exactly as its dialog chrome did
//     before the role existed (ListBG == DialogBG, ListFG == DialogFG), end to end;
//   - list_bg / list_fg round-trip as config overrides and reach tv.DefaultTheme;
//   - the four dialog lists paint their background from the role, not DialogBG;
//   - the Saved Sessions focused row is visible (the selectionColorsFor fix, #5);
//   - paletteContrast audits the list pair and every preset clears it;
//   - the editor exposes list_bg only, and a config-only list_fg survives a Save.

// ----------------------------------------------------------------------------
// Shared helpers.
// ----------------------------------------------------------------------------

// listFields maps the two #327 override keys to the colour the given Theme paints
// for them — the single place the role→field wiring is named, so the wiring tests
// stay terse and a mis-wired accessor is caught unambiguously.
func listFields(t Theme) map[string]tui.Color {
	return map[string]tui.Color{
		"list_bg": t.ListBG,
		"list_fg": t.ListFG,
	}
}

// tvListSlots maps the same two keys to the corresponding slot on a turbotui
// tv.Theme, so a test can assert ApplyTheme installs each role onto the matching
// slot (the bridge the four dialog lists read at construction).
func tvListSlots(d tv.Theme) map[string]tui.Color {
	return map[string]tui.Color{
		"list_bg": d.ListBG,
		"list_fg": d.ListFG,
	}
}

// issue327BufferHasBG reports whether any cell in the rendered app buffer carries
// the given background colour. The four dialog lists are local variables that
// AddContent stores only by component (the *tv.Tree is unrecoverable), so the sites
// are exercised end to end through a real Draw: a tv.Tree fills its whole rect with
// its BG, so a distinct ListBG painted anywhere proves the list read the role.
func issue327BufferHasBG(w *Workbench, c tui.Color) bool {
	for y := 0; y < w.app.Height(); y++ {
		for x := 0; x < w.app.Width(); x++ {
			if w.app.ReadCell(x, y).BG == c {
				return true
			}
		}
	}
	return false
}

// issue327ApplyListBG resolves the default preset with a single list_bg override to
// the given (truecolor) hex, installs it, and returns the resolved colour. It pins
// the precondition that the override actually reaches tv.DefaultTheme and is
// DISTINCT from DialogBG, so a passing site test genuinely proves the list reads
// ListBG rather than coincidentally matching the dialog chrome.
func issue327ApplyListBG(t *testing.T, hex string) tui.Color {
	t.Helper()
	issue204RestoreTheme(t)
	want, ok := parseColor(hex)
	if !ok {
		t.Fatalf("setup: parseColor(%q) failed", hex)
	}
	ApplyTheme(ResolveTheme(config.ThemeConfig{Overrides: map[string]string{"list_bg": hex}}, truecolorEnv, false))
	if tv.DefaultTheme.ListBG != want {
		t.Fatalf("setup: ApplyTheme left tv.DefaultTheme.ListBG = %+v, want %+v", tv.DefaultTheme.ListBG, want)
	}
	if tv.DefaultTheme.ListBG == tv.DefaultTheme.DialogBG {
		t.Fatalf("setup: list_bg override %+v equals DialogBG — the site test could not distinguish list from dialog", want)
	}
	return want
}

// ----------------------------------------------------------------------------
// Group A: the two roles exist, are populated by every preset, and default to the
// dialog chrome so the look is unchanged from before the role existed.
// ----------------------------------------------------------------------------

// TestIssue327ListRolesPopulatedByEveryPalette checks each built-in palette fills
// both roles with a concrete colour (never the zero Color, which renders as ANSI 0
// black, nor the terminal default) and that the pair is legible (fg != bg). An
// unset role would make a list paint black-on-black or vanish into the terminal.
func TestIssue327ListRolesPopulatedByEveryPalette(t *testing.T) {
	for name, pal := range map[string]func() Theme{
		themeDefault:      defaultPalette,
		themeHighContrast: highContrastPalette,
		themeDark:         darkPalette,
	} {
		t.Run(name, func(t *testing.T) {
			p := pal()
			for role, c := range listFields(p) {
				if reflect.DeepEqual(c, tui.Color{}) {
					t.Errorf("%s: %s is the zero Color — the role is not populated", name, role)
				}
				if c.Mode == tui.ColorDefault {
					t.Errorf("%s: %s is the terminal default — a built-in palette must carry a concrete colour", name, role)
				}
			}
			if p.ListFG == p.ListBG {
				t.Errorf("%s: ListFG == ListBG (%+v) — illegible list", name, p.ListFG)
			}
		})
	}
}

// TestIssue327PaletteListMatchesDialogChrome is the appearance-unchanged guarantee
// at the palette level: every preset's ListBG/ListFG must equal the dialog chrome
// the preset's lists painted on before #327 (the four sites previously read
// DialogBG/DialogFG). The default preset's chrome is the stock baseTVTheme; the two
// black-canvas presets derive theirs from blackCanvasTVTheme. A drift here would
// silently recolour every default-preset list.
func TestIssue327PaletteListMatchesDialogChrome(t *testing.T) {
	t.Run("default == stock dialog chrome", func(t *testing.T) {
		d := defaultPalette()
		if d.ListBG != baseTVTheme.DialogBG {
			t.Errorf("default ListBG = %+v, want stock DialogBG %+v (look must be unchanged)", d.ListBG, baseTVTheme.DialogBG)
		}
		if d.ListFG != baseTVTheme.DialogFG {
			t.Errorf("default ListFG = %+v, want stock DialogFG %+v", d.ListFG, baseTVTheme.DialogFG)
		}
		// Pin the concrete authored values too, so a swap is caught even if baseTVTheme
		// itself changed underneath us (light grey on ANSI 0, == the stock dialog).
		if d.ListBG != tui.ANSIColor(7) || d.ListFG != tui.ANSIColor(0) {
			t.Errorf("default List* = (%+v,%+v), want (ANSI 7, ANSI 0)", d.ListBG, d.ListFG)
		}
	})
	for _, p := range []Theme{highContrastPalette(), darkPalette()} {
		t.Run(p.Name+" == black-canvas dialog chrome", func(t *testing.T) {
			cv := blackCanvasTVTheme(p)
			if p.ListBG != cv.DialogBG {
				t.Errorf("%s ListBG = %+v, want its DialogBG %+v", p.Name, p.ListBG, cv.DialogBG)
			}
			if p.ListFG != cv.DialogFG {
				t.Errorf("%s ListFG = %+v, want its DialogFG %+v", p.Name, p.ListFG, cv.DialogFG)
			}
		})
	}
}

// TestIssue327ApplyThemeListEqualsDialogByDefault is the end-to-end unchanged
// guarantee: for every preset, after ApplyTheme the installed tv.DefaultTheme.ListBG/
// ListFG equal tv.DefaultTheme.DialogBG/DialogFG — so the four sites, which now read
// the List* slots, paint exactly what they painted reading DialogBG/DialogFG before.
func TestIssue327ApplyThemeListEqualsDialogByDefault(t *testing.T) {
	for _, th := range []Theme{issue204Default(), issue204HighContrast(), issue204Dark()} {
		t.Run(th.Name, func(t *testing.T) {
			issue204RestoreTheme(t)
			ApplyTheme(th)
			if tv.DefaultTheme.ListBG != tv.DefaultTheme.DialogBG {
				t.Errorf("%s: tv.DefaultTheme.ListBG %+v != DialogBG %+v — the default list look changed",
					th.Name, tv.DefaultTheme.ListBG, tv.DefaultTheme.DialogBG)
			}
			if tv.DefaultTheme.ListFG != tv.DefaultTheme.DialogFG {
				t.Errorf("%s: tv.DefaultTheme.ListFG %+v != DialogFG %+v", th.Name, tv.DefaultTheme.ListFG, tv.DefaultTheme.DialogFG)
			}
		})
	}
}

// ----------------------------------------------------------------------------
// Group B: ResolveTheme degrades both roles to the terminal's fidelity (mirrors the
// #265 degrade contract — a missing degrade() line emits a colour under NO_COLOR or
// an out-of-gamut RGB on a 16-colour terminal).
// ----------------------------------------------------------------------------

func TestIssue327ResolveThemeDegradesListRoles(t *testing.T) {
	t.Run("NO_COLOR collapses both roles to the terminal default", func(t *testing.T) {
		for _, name := range []string{themeDefault, themeHighContrast, themeDark} {
			got := ResolveTheme(config.ThemeConfig{Name: name, NoColor: true}, truecolorEnv, false)
			for role, c := range listFields(got) {
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
			for role, want := range listFields(pal) {
				if g := listFields(got)[role]; g != want {
					t.Errorf("%s truecolor: %s = %+v, want the palette %+v", name, role, g, want)
				}
			}
		}
	})

	t.Run("RGB presets degrade to ANSI at 256 and 16 colours", func(t *testing.T) {
		for _, name := range []string{themeHighContrast, themeDark} {
			for _, env := range []func(string) string{color256Env, color16Env} {
				got := ResolveTheme(config.ThemeConfig{Name: name}, env, false)
				for role, c := range listFields(got) {
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
			for role, want := range listFields(pal) {
				if g := listFields(got)[role]; g != want {
					t.Errorf("default %s at 16/256/truecolor = %+v, want %+v", role, g, want)
				}
			}
		}
	})
}

// ----------------------------------------------------------------------------
// Group C: applyOverrides honours both keys (a missing case silently drops an
// override — the menu-bar mistake of #243).
// ----------------------------------------------------------------------------

func TestIssue327ListOverridesApply(t *testing.T) {
	marker, ok := parseColor("#12EFA0") // a distinctive RGB no preset equals
	if !ok {
		t.Fatalf("setup: parseColor(#12EFA0) failed")
	}

	for key := range listFields(defaultPalette()) {
		t.Run(key+" applies", func(t *testing.T) {
			got := paletteByName(themeDefault)
			applyOverrides(&got, map[string]string{key: "#12EFA0"})
			if listFields(got)[key] != marker {
				t.Errorf("applyOverrides({%q:#12EFA0}) left %q at %+v, want %+v — the override is silently dropped (missing case?)",
					key, key, listFields(got)[key], marker)
			}
		})
	}

	t.Run("each key sets ONLY its own field", func(t *testing.T) {
		// Guards a copy-paste mis-wire (e.g. case "list_bg": t.ListFG = c).
		for key := range listFields(defaultPalette()) {
			got := paletteByName(themeDefault)
			before := listFields(got)
			applyOverrides(&got, map[string]string{key: "#12EFA0"})
			after := listFields(got)
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

	t.Run("ANSI spec applies", func(t *testing.T) {
		got := paletteByName(themeDefault)
		applyOverrides(&got, map[string]string{"list_bg": "9"})
		if got.ListBG != tui.ANSIColor(9) {
			t.Errorf("list_bg=9 -> %+v, want ANSI 9", got.ListBG)
		}
	})

	t.Run("key is case/whitespace insensitive", func(t *testing.T) {
		got := paletteByName(themeDefault)
		applyOverrides(&got, map[string]string{"  List_FG ": "5"})
		if got.ListFG != tui.ANSIColor(5) {
			t.Errorf("normalised list_fg -> %+v, want ANSI 5", got.ListFG)
		}
	})

	t.Run("invalid value is ignored", func(t *testing.T) {
		got := paletteByName(themeDefault)
		before := got.ListBG
		applyOverrides(&got, map[string]string{"list_bg": "nope"})
		if got.ListBG != before {
			t.Errorf("invalid list_bg overrode the value: %+v -> %+v", before, got.ListBG)
		}
	})

	t.Run("unknown name does not leak into a list field", func(t *testing.T) {
		got := paletteByName(themeDefault)
		before := listFields(got)
		applyOverrides(&got, map[string]string{"list": "#123456", "list_background": "#123456"}) // wrong keys
		for role, c := range listFields(got) {
			if c != before[role] {
				t.Errorf("unknown override key leaked into %q: %+v -> %+v", role, before[role], c)
			}
		}
	})
}

// ----------------------------------------------------------------------------
// Group D: ApplyTheme installs both roles onto tv.DefaultTheme's List* slots and
// tv.SetTheme propagates them — for every preset, end to end through an override,
// and NO_COLOR neutralises rather than emitting a colour.
// ----------------------------------------------------------------------------

func TestIssue327ApplyThemeInstallsListRoles(t *testing.T) {
	for _, th := range []Theme{issue204Default(), issue204HighContrast(), issue204Dark()} {
		t.Run(th.Name, func(t *testing.T) {
			issue204RestoreTheme(t)
			ApplyTheme(th)
			want := listFields(th)
			for role, w := range want {
				if got := tvListSlots(tv.DefaultTheme)[role]; got != w {
					t.Errorf("tv.DefaultTheme.%s = %+v, want the role %+v", role, got, w)
				}
				if got := tvListSlots(tv.ActiveTheme())[role]; got != w {
					t.Errorf("tv.ActiveTheme().%s = %+v, want %+v (tv.SetTheme must propagate the install)", role, got, w)
				}
			}
		})
	}
}

// TestIssue327ApplyThemeOverrideReachesTVDefaultTheme is the core of the wiring: a
// list_bg / list_fg override on the DEFAULT preset must reach tv.DefaultTheme (and
// the propagated tv.ActiveTheme) even though the default branch starts from the
// stock chrome — the same install-over-chrome pattern #265 uses for buttons/inputs.
// It also confirms a pristine default install does not mutate baseTVTheme.
func TestIssue327ApplyThemeOverrideReachesTVDefaultTheme(t *testing.T) {
	issue204RestoreTheme(t)

	red := tui.RGBColor(0xFF, 0x00, 0x00)
	green := tui.RGBColor(0x00, 0xFF, 0x00)
	th := ResolveTheme(config.ThemeConfig{
		Name:      themeDefault,
		Overrides: map[string]string{"list_bg": "#FF0000", "list_fg": "#00FF00"},
	}, truecolorEnv, false)
	if th.ListBG != red || th.ListFG != green {
		t.Fatalf("setup: ResolveTheme ListBG=%+v ListFG=%+v, want red/green", th.ListBG, th.ListFG)
	}
	ApplyTheme(th)
	if tv.DefaultTheme.ListBG != red {
		t.Errorf("list_bg override did not reach tv.DefaultTheme on the default preset: got %+v, want red", tv.DefaultTheme.ListBG)
	}
	if tv.DefaultTheme.ListFG != green {
		t.Errorf("list_fg override did not reach tv.DefaultTheme on the default preset: got %+v, want green", tv.DefaultTheme.ListFG)
	}
	if tv.ActiveTheme().ListBG != red {
		t.Errorf("list_bg override did not propagate to tv.ActiveTheme(): got %+v, want red", tv.ActiveTheme().ListBG)
	}
	// The install must not have mutated the pristine stock snapshot via aliasing.
	if baseTVTheme.ListBG != tui.ANSIColor(7) || baseTVTheme.ListFG != tui.ANSIColor(0) {
		t.Errorf("baseTVTheme mutated by ApplyTheme: ListBG=%+v ListFG=%+v", baseTVTheme.ListBG, baseTVTheme.ListFG)
	}
}

// TestIssue327ApplyThemeNoColorNeutralizes confirms that under NO_COLOR both roles
// degrade to the terminal default and ApplyTheme installs that default rather than
// emitting a colour onto the neutral chrome.
func TestIssue327ApplyThemeNoColorNeutralizes(t *testing.T) {
	issue204RestoreTheme(t)
	th := ResolveTheme(config.ThemeConfig{NoColor: true}, truecolorEnv, false)
	ApplyTheme(th)
	for role, c := range tvListSlots(tv.DefaultTheme) {
		if c != tui.DefaultColor() {
			t.Errorf("NO_COLOR tv.DefaultTheme.%s = %+v, want terminal default", role, c)
		}
	}
}

// TestIssue327SwitchbackRestoresStockList checks the #204 switchback invariant holds
// for the new roles: after default→high-contrast→default the tv.ActiveTheme List*
// slots are back on the stock values, not stuck on the high-contrast black/white. A
// reinstall bug that cached the coloured preset would fail here.
func TestIssue327SwitchbackRestoresStockList(t *testing.T) {
	issue204RestoreTheme(t)
	ApplyTheme(issue204HighContrast())
	hcListBG := tv.ActiveTheme().ListBG

	ApplyTheme(issue204Default())
	if tv.ActiveTheme().ListBG != tui.ANSIColor(7) || tv.ActiveTheme().ListFG != tui.ANSIColor(0) {
		t.Errorf("after switchback to default, tv.ActiveTheme List* = (%+v,%+v), want stock (ANSI 7, ANSI 0)",
			tv.ActiveTheme().ListBG, tv.ActiveTheme().ListFG)
	}
	if tv.ActiveTheme().ListBG == hcListBG && hcListBG != tui.ANSIColor(7) {
		t.Errorf("list BG stuck on the high-contrast value %+v after switchback", hcListBG)
	}
}

// ----------------------------------------------------------------------------
// Group E: the neutral and black-canvas chrome builders source their List* slots
// from the roles (so an override flows through and the install is a no-op there).
// ----------------------------------------------------------------------------

// TestIssue327ChromeBuildersSourceListFromRoles drives both builders with a marked
// theme and asserts they copy the role values verbatim rather than hardcoding the
// slots — a regression that hardcoded them would ignore an override.
func TestIssue327ChromeBuildersSourceListFromRoles(t *testing.T) {
	marked := defaultPalette()
	marked.ListBG = tui.RGBColor(0x0A, 0x0B, 0x0C)
	marked.ListFG = tui.RGBColor(0x1A, 0x1B, 0x1C)

	for name, build := range map[string]func(Theme) tv.Theme{
		"neutralTVTheme":     neutralTVTheme,
		"blackCanvasTVTheme": blackCanvasTVTheme,
	} {
		t.Run(name, func(t *testing.T) {
			cv := build(marked)
			if cv.ListBG != marked.ListBG {
				t.Errorf("%s ListBG = %+v, want the role value %+v (it must source from the role, not hardcode)", name, cv.ListBG, marked.ListBG)
			}
			if cv.ListFG != marked.ListFG {
				t.Errorf("%s ListFG = %+v, want the role value %+v", name, cv.ListFG, marked.ListFG)
			}
		})
	}
}

// ----------------------------------------------------------------------------
// Group F: paletteContrast audits the list pair against its own background at the
// body-text tier, and every preset clears it.
// ----------------------------------------------------------------------------

// TestIssue327PaletteContrastCoversListPair verifies the audit was extended: the
// "list" finding exists, is checked against the background the list actually paints
// on (ListBG, not the window), held to the body-text tier, and clears for every
// built-in palette. It also pins the emission position (last, after the input
// roles) so it stays consistent with the ordered findings contract.
func TestIssue327PaletteContrastCoversListPair(t *testing.T) {
	for name, pal := range map[string]func() Theme{
		themeDefault:      defaultPalette,
		themeHighContrast: highContrastPalette,
		themeDark:         darkPalette,
	} {
		t.Run(name, func(t *testing.T) {
			p := pal()
			findings := paletteContrast(p, baseTVTheme.WindowBG)
			f := findingByRole(t, findings, "list")
			if f.FG != p.ListFG {
				t.Errorf("%s list FG = %+v, want ListFG %+v", name, f.FG, p.ListFG)
			}
			if f.BG != p.ListBG {
				t.Errorf("%s list BG = %+v, want ListBG %+v — the role must be audited against the background it paints on", name, f.BG, p.ListBG)
			}
			if f.Min != minContrastText {
				t.Errorf("%s list Min = %g, want minContrastText %g (list rows are body text)", name, f.Min, minContrastText)
			}
			if !f.OK() {
				t.Errorf("%s list fails its contrast minimum: %.2f:1 < %.1f — an illegible list pair", name, f.Ratio, f.Min)
			}
			// Position: paletteContrast emits "list" last (the ordered-findings contract
			// in theme_issue202_test.go depends on this).
			if last := findings[len(findings)-1]; last.Role != "list" {
				t.Errorf("%s: last finding is %q, want \"list\" (it is emitted after the input roles)", name, last.Role)
			}
		})
	}
}

// TestIssue327PaletteContrastListIndeterminateUnderNoColor confirms the defensive
// design extends to the new role: under NO_COLOR the list pair flattens to the
// terminal default and its audit is indeterminate (ratio 0, not OK) rather than
// silently "passing" a pairing it cannot measure.
func TestIssue327PaletteContrastListIndeterminateUnderNoColor(t *testing.T) {
	none := ResolveTheme(config.ThemeConfig{}, noColorEnv, false)
	f := findingByRole(t, paletteContrast(none, baseTVTheme.WindowBG), "list")
	if f.Ratio != 0 {
		t.Errorf("NO_COLOR list finding ratio = %g, want 0 (indeterminate)", f.Ratio)
	}
	if f.OK() {
		t.Errorf("NO_COLOR list finding unexpectedly OK; an indeterminate pair must not pass")
	}
}

// ----------------------------------------------------------------------------
// Group G: the theme editor exposes list_bg ONLY; list_fg stays a config-only role
// carried by carryUnexposedOverrides.
// ----------------------------------------------------------------------------

// TestIssue327EditorExposesListBgOnly verifies list_bg is in the editor's role list
// with an accessor that reads the right field across presets, and that list_fg is
// deliberately NOT exposed (matching code_bg's minimalism).
func TestIssue327EditorExposesListBgOnly(t *testing.T) {
	roles := issue243RoleByKey(t)

	r, ok := roles["list_bg"]
	if !ok {
		t.Fatalf("themeRoles is missing list_bg — the list background is not editable")
	}
	if r.label != "List background" {
		t.Errorf("list_bg label = %q, want %q", r.label, "List background")
	}
	for _, preset := range []string{themeDefault, themeHighContrast, themeDark} {
		p := paletteByName(preset)
		if r.get(p) != p.ListBG {
			t.Errorf("%s: editor accessor for list_bg = %+v, but Theme.ListBG = %+v (mis-wired accessor)", preset, r.get(p), p.ListBG)
		}
	}

	if _, ok := roles["list_fg"]; ok {
		t.Errorf("themeRoles unexpectedly exposes list_fg — #327 exposes list_bg only (code_bg minimalism); list_fg stays config-only and carried by carryUnexposedOverrides")
	}
}

// TestIssue327ListBgOverrideRoundTrip checks the editor's save-then-reopen stability
// for list_bg across every preset: a config built by the editor, reloaded via
// editedTheme and rebuilt, reproduces itself with the override intact.
func TestIssue327ListBgOverrideRoundTrip(t *testing.T) {
	for _, name := range []string{themeDefault, themeHighContrast, themeDark} {
		specs := specsFor(paletteByName(name))
		specs["list_bg"] = "#0ABCD1"
		cfg := buildThemeConfig(name, false, false, specs)

		reopened := buildThemeConfig(cfg.Name, cfg.NoColor, false, specsFor(editedTheme(cfg)))
		if !reflect.DeepEqual(reopened, cfg) {
			t.Errorf("preset %q: list_bg override did not round-trip:\n got %+v\nwant %+v", name, reopened, cfg)
		}
		if reopened.Overrides["list_bg"] != "#0ABCD1" {
			t.Errorf("preset %q: round-trip lost/changed list_bg: %+v", name, reopened.Overrides)
		}
	}
}

// TestIssue327ListFgSurvivesEditorSave asserts a hand-set, config-only list_fg
// override survives a no-edit "open the editor, Save" cycle (carryUnexposedOverrides),
// and still resolves to the configured colour afterwards — so it is genuinely
// preserved, not retained as dead config text. editorSave models the real save path
// (defined in theme_issue265_test.go).
func TestIssue327ListFgSurvivesEditorSave(t *testing.T) {
	const spec = "#ABCDEF"
	cur := config.ThemeConfig{Name: themeDefault, Overrides: map[string]string{"list_fg": spec}}

	// Sanity: the override is honoured by the pipeline today (it is a real role).
	want, _ := parseColor(spec)
	if got := ResolveTheme(cur, truecolorEnv, false).ListFG; got != want {
		t.Fatalf("setup: list_fg override not honoured by ResolveTheme — got %+v, want %+v", got, want)
	}

	saved := editorSave(cur)
	if saved.Overrides["list_fg"] != spec {
		t.Errorf("editor Save dropped the config-only list_fg override: saved.Overrides=%+v — it must survive an unrelated Save", saved.Overrides)
	}
	if got := ResolveTheme(saved, truecolorEnv, false).ListFG; got != want {
		t.Errorf("after editor Save, list_fg no longer resolves to the override: got %+v, want %+v", got, want)
	}
}

// TestIssue327ListBgEditedAwayNotResurrected is the companion edge case: list_bg is
// exposed, so the editor field is the source of truth — clearing it back to the
// preset default must drop the override, and carryUnexposedOverrides must not
// resurrect a stale prior value (or an edit-away could never take effect).
func TestIssue327ListBgEditedAwayNotResurrected(t *testing.T) {
	cur := config.ThemeConfig{Name: themeDefault, Overrides: map[string]string{"list_bg": "#FF0000"}}
	// The field is reset to the preset default (specsFor of the default palette).
	specs := specsFor(paletteByName(themeDefault))
	cfg := buildThemeConfig(cur.Name, cur.NoColor, cur.NoShadow, specs)
	cfg = carryUnexposedOverrides(cfg, cur.Overrides)
	if _, ok := cfg.Overrides["list_bg"]; ok {
		t.Errorf("carry resurrected an edited-away exposed list_bg: %+v — exposed keys come from the field, not prior", cfg.Overrides)
	}
}

// ----------------------------------------------------------------------------
// Group H: the four dialog lists paint their background from the list role, not
// DialogBG (acceptance criteria #2/#4). Each site is opened with a list_bg DISTINCT
// from DialogBG and rendered through the real Draw path; the distinct colour must
// appear in the buffer (a tv.Tree fills its rect with its BG). A site still reading
// DialogBG would never paint the distinct colour, failing the test.
// ----------------------------------------------------------------------------

// TestIssue327CommandPaletteListUsesRole drives the command palette (always
// populated by the built-in commands).
func TestIssue327CommandPaletteListUsesRole(t *testing.T) {
	u := issue327ApplyListBG(t, "#123457")
	w := newTestWorkbench(t)

	// Before opening any dialog the distinct list colour is nowhere on screen.
	w.desktop.Redraw()
	if issue327BufferHasBG(w, u) {
		t.Fatalf("the distinct list_bg appeared before the palette opened — pick a more distinctive colour")
	}

	w.showCommandPalette()
	w.desktop.Redraw()
	if !issue327BufferHasBG(w, u) {
		t.Errorf("command palette list did not paint from tv.DefaultTheme.ListBG — it still reads DialogBG")
	}
}

// TestIssue327ResourcesListUsesRole drives the Resources browser with a few tools so
// its list has rows.
func TestIssue327ResourcesListUsesRole(t *testing.T) {
	u := issue327ApplyListBG(t, "#123457")
	w := newTestWorkbench(t)
	w.SetHandlers(Handlers{GetTools: func() []ToolInfo {
		return []ToolInfo{{Name: "alpha", Enabled: true}, {Name: "beta"}, {Name: "gamma"}}
	}})

	w.showResourcesDialog()
	w.desktop.Redraw()
	if !issue327BufferHasBG(w, u) {
		t.Errorf("Resources browser list did not paint from tv.DefaultTheme.ListBG — it still reads DialogBG")
	}
}

// TestIssue327SessionsListUsesRole drives the Saved Sessions browser with a few
// sessions so its list has rows.
func TestIssue327SessionsListUsesRole(t *testing.T) {
	u := issue327ApplyListBG(t, "#123457")
	w := newTestWorkbench(t)
	w.SetHandlers(Handlers{
		ListSavedSessions: func() []SessionMeta {
			return []SessionMeta{{ID: "a", Title: "A", File: "a"}, {ID: "b", Title: "B", File: "b"}, {ID: "c", Title: "C", File: "c"}}
		},
		OpenSavedSession: func(string, bool) (RestoredSession, bool) { return RestoredSession{}, false },
	})

	w.showSessionsDialog()
	w.desktop.Redraw()
	if !issue327BufferHasBG(w, u) {
		t.Errorf("Saved Sessions browser list did not paint from tv.DefaultTheme.ListBG — it still reads DialogBG")
	}
}

// TestIssue327MentionPopupListUsesRole inspects the @-mention popup's list directly
// (the completer keeps a reference to it, unlike the other three sites): its BG/FG
// must equal the installed List* slots (distinct from DialogBG/DialogFG), and its
// selection must come from selectionColorsFor over the list colours.
func TestIssue327MentionPopupListUsesRole(t *testing.T) {
	u := issue327ApplyListBG(t, "#123457")
	w := newTestWorkbench(t)
	sw := w.openWindow("s", "S")
	mc := newMentionCompleter(sw)
	mc.render([]string{"a.go", "b.go", "c.go"})

	if mc.list == nil {
		t.Fatal("mention completer did not build its list")
	}
	if mc.list.BG != u {
		t.Errorf("mention list BG = %+v, want the installed ListBG %+v (not DialogBG)", mc.list.BG, u)
	}
	if mc.list.FG != tv.DefaultTheme.ListFG {
		t.Errorf("mention list FG = %+v, want the installed ListFG %+v", mc.list.FG, tv.DefaultTheme.ListFG)
	}
	// The list reads the list role, not the dialog chrome.
	if mc.list.BG == tv.DefaultTheme.DialogBG {
		t.Errorf("mention list BG equals DialogBG %+v — the site did not move to the list role", tv.DefaultTheme.DialogBG)
	}
	// Selection comes from selectionColorsFor over the LIST colours (so the
	// invisible-selection fallback inverts the list, not the dialog).
	wantSelFG, wantSelBG := selectionColorsFor(
		tv.DefaultTheme.ListFG, tv.DefaultTheme.ListBG,
		tv.DefaultTheme.SelectionFG, tv.DefaultTheme.SelectionBG)
	if mc.list.SelFG != wantSelFG || mc.list.SelBG != wantSelBG {
		t.Errorf("mention list selection = (%+v,%+v), want selectionColorsFor over the list pair (%+v,%+v)",
			mc.list.SelFG, mc.list.SelBG, wantSelFG, wantSelBG)
	}
}

// TestIssue327SessionsFocusedRowVisible pins acceptance criterion #5: the Saved
// Sessions browser's focused row must be visible even when the selection colours
// collide with the list background — the pre-existing inconsistency #327 fixes by
// switching this site from a direct SelectionFG/SelectionBG assignment to
// selectionColorsFor. A collision is forced by overriding list_bg/list_fg to the
// same colours the focused selection resolves to (dropdown_select_*), so a buggy
// site (SelBG = SelectionBG = ListBG) would paint the focused row invisibly; the
// fix inverts it, so a DISTINCT selection-bar background appears in the buffer.
func TestIssue327SessionsFocusedRowVisible(t *testing.T) {
	issue204RestoreTheme(t)
	listBG, _ := parseColor("#111155")
	listFG, _ := parseColor("#995511")
	ApplyTheme(ResolveTheme(config.ThemeConfig{Overrides: map[string]string{
		"list_bg": "#111155", "list_fg": "#995511",
		// Make the focused selection collide with the list colours.
		"dropdown_select_bg": "#111155", "dropdown_select_fg": "#995511",
	}}, truecolorEnv, false))

	// Precondition: the collision really is set up (else the test would pass vacuously).
	if tv.DefaultTheme.SelectionBG != listBG || tv.DefaultTheme.SelectionFG != listFG {
		t.Fatalf("setup: Selection* (%+v,%+v) do not collide with List* (%+v,%+v)",
			tv.DefaultTheme.SelectionFG, tv.DefaultTheme.SelectionBG, listFG, listBG)
	}

	w := newTestWorkbench(t)
	w.SetHandlers(Handlers{
		ListSavedSessions: func() []SessionMeta {
			return []SessionMeta{{ID: "a", Title: "A", File: "a"}, {ID: "b", Title: "B", File: "b"}, {ID: "c", Title: "C", File: "c"}}
		},
		OpenSavedSession: func(string, bool) (RestoredSession, bool) { return RestoredSession{}, false },
	})
	w.showSessionsDialog() // focuses the list at the end
	w.desktop.Redraw()

	// The list rows paint listBG; the focused row, had it kept SelBG == SelectionBG,
	// would be listBG too (invisible). The fix inverts the colliding pair, so the
	// focused row's background becomes listFG — a colour that must now appear.
	if !issue327BufferHasBG(w, listBG) {
		t.Fatalf("the sessions list did not render (list background %+v absent)", listBG)
	}
	if !issue327BufferHasBG(w, listFG) {
		t.Errorf("the focused row is invisible: the inverted selection-bar background %+v is absent — sessions still assigns SelectionFG/SelectionBG directly instead of selectionColorsFor (#327 criterion #5)", listFG)
	}
}

// ----------------------------------------------------------------------------
// Group I: config documentation lists the new override keys.
// ----------------------------------------------------------------------------

// TestIssue327DocsListOverrideKeys is a lightweight guard that docs/configuration.md
// advertises list_bg and list_fg as override keys (Part 6). A role that is wired but
// undocumented is invisible to users.
func TestIssue327DocsListOverrideKeys(t *testing.T) {
	// The package lives at ui/tui; the docs are two directories up.
	data, err := os.ReadFile(filepath.Join("..", "..", "docs", "configuration.md"))
	if err != nil {
		t.Skipf("could not read docs/configuration.md: %v", err)
	}
	text := string(data)
	for _, key := range []string{"list_bg", "list_fg"} {
		if !strings.Contains(text, key) {
			t.Errorf("docs/configuration.md does not document the %q override key (Part 6)", key)
		}
	}
}
