package ui

import (
	"reflect"
	"testing"

	"gogent/internal/config"

	tui "github.com/hobbestherat/turbotui"
)

// TestColorSpec covers rendering a colour back to an editable spec string and
// round-tripping it through parseColor.
func TestColorSpec(t *testing.T) {
	cases := []struct {
		name string
		c    tui.Color
		want string
	}{
		{"ansi", tui.ANSIColor(14), "14"},
		{"ansi 0", tui.ANSIColor(0), "0"},
		{"ansi 255", tui.ANSIColor(255), "255"},
		{"rgb", tui.RGBColor(0xE6, 0x9F, 0x00), "#E69F00"},
		{"rgb black", tui.RGBColor(0, 0, 0), "#000000"},
		{"default", tui.DefaultColor(), "default"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := colorSpec(c.c)
			if got != c.want {
				t.Fatalf("colorSpec(%+v) = %q, want %q", c.c, got, c.want)
			}
			// The spec must parse back to the original colour.
			back, ok := parseColor(got)
			if !ok || back != c.c {
				t.Errorf("parseColor(%q) = %+v, %v; want %+v", got, back, ok, c.c)
			}
		})
	}
}

// specsFor seeds a full role→spec map from a Theme, mirroring what the editor's
// fields hold before any edit.
func specsFor(t Theme) map[string]string {
	m := make(map[string]string, len(themeRoles))
	for _, role := range themeRoles {
		m[role.key] = colorSpec(role.get(t))
	}
	return m
}

// TestBuildThemeConfig covers preset selection, pristine palettes (no
// overrides), targeted edits, the NO_COLOR toggle and invalid specs.
func TestBuildThemeConfig(t *testing.T) {
	defaultSpecs := specsFor(paletteByName(themeDefault))

	t.Run("pristine default has no overrides", func(t *testing.T) {
		got := buildThemeConfig("default", false, false, defaultSpecs)
		want := config.ThemeConfig{Name: themeDefault}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("got %+v, want %+v", got, want)
		}
	})

	t.Run("pristine high-contrast has no overrides", func(t *testing.T) {
		specs := specsFor(paletteByName(themeHighContrast))
		got := buildThemeConfig("high-contrast", false, false, specs)
		want := config.ThemeConfig{Name: themeHighContrast}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("got %+v, want %+v", got, want)
		}
	})

	t.Run("changed role becomes an override", func(t *testing.T) {
		specs := cloneSpecs(defaultSpecs)
		specs["user"] = "#FF0000"
		got := buildThemeConfig("default", false, false, specs)
		want := config.ThemeConfig{Name: themeDefault, Overrides: map[string]string{"user": "#FF0000"}}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("got %+v, want %+v", got, want)
		}
	})

	t.Run("default spec on a coloured role is recorded", func(t *testing.T) {
		specs := cloneSpecs(defaultSpecs)
		specs["error"] = "default"
		got := buildThemeConfig("default", false, false, specs)
		if got.Overrides["error"] != "default" {
			t.Fatalf("expected error override 'default', got %+v", got.Overrides)
		}
	})

	t.Run("no-color flag is carried through", func(t *testing.T) {
		got := buildThemeConfig("default", true, false, defaultSpecs)
		if !got.NoColor {
			t.Fatalf("expected NoColor=true, got %+v", got)
		}
		if len(got.Overrides) != 0 {
			t.Fatalf("expected no overrides, got %+v", got.Overrides)
		}
	})

	t.Run("invalid spec is ignored", func(t *testing.T) {
		specs := cloneSpecs(defaultSpecs)
		specs["tool"] = "not-a-colour"
		got := buildThemeConfig("default", false, false, specs)
		if len(got.Overrides) != 0 {
			t.Fatalf("expected invalid spec ignored, got %+v", got.Overrides)
		}
	})

	t.Run("alias preset name is canonicalised", func(t *testing.T) {
		got := buildThemeConfig("colorblind", false, false, specsFor(paletteByName(themeHighContrast)))
		if got.Name != themeHighContrast {
			t.Fatalf("expected canonical %q, got %q", themeHighContrast, got.Name)
		}
	})
}

func cloneSpecs(m map[string]string) map[string]string {
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// TestBuildThemeConfigRoundTrip checks that a config produced by the editor,
// when resolved and re-loaded into the editor's fields, reproduces the same
// config — the property that makes "save then reopen" stable.
func TestBuildThemeConfigRoundTrip(t *testing.T) {
	// Start from the default palette, recolour two roles.
	specs := specsFor(paletteByName(themeDefault))
	specs["agent"] = "#00FF00"
	specs["panel_bg"] = "16"
	cfg := buildThemeConfig("default", false, false, specs)

	// Reopen: the editor seeds fields from editedTheme(cfg); rebuilding must
	// yield the identical config.
	reopened := buildThemeConfig(cfg.Name, cfg.NoColor, false, specsFor(editedTheme(cfg)))
	if !reflect.DeepEqual(reopened, cfg) {
		t.Fatalf("round-trip mismatch:\n got %+v\nwant %+v", reopened, cfg)
	}
}

// TestPresetIndex covers mapping canonical and alias palette names to their
// dropdown index, defaulting to the default palette.
func TestPresetIndex(t *testing.T) {
	cases := []struct {
		name string
		want int
	}{
		{"default", 0},
		{"", 0},
		{"unknown", 0},
		{"high-contrast", 1},
		{"colorblind", 1},
		{"high_contrast", 1},
	}
	for _, c := range cases {
		if got := presetIndex(c.name); got != c.want {
			t.Errorf("presetIndex(%q) = %d, want %d", c.name, got, c.want)
		}
	}
}
