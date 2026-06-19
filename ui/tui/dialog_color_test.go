package ui

import (
	"math"
	"testing"

	tui "github.com/hobbestherat/turbotui"
	tv "github.com/hobbestherat/turbotui/turbotv"
)

// TestRelativeLuminance checks the WCAG luminance of the two reference colours
// and that ordering by perceived brightness holds for the palette extremes.
func TestRelativeLuminance(t *testing.T) {
	cases := []struct {
		name string
		c    tui.Color
		want float64
	}{
		{"black is 0", tui.RGBColor(0, 0, 0), 0},
		{"white is 1", tui.RGBColor(255, 255, 255), 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := relativeLuminance(tc.c); math.Abs(got-tc.want) > 1e-9 {
				t.Errorf("relativeLuminance(%v) = %f, want %f", tc.c, got, tc.want)
			}
		})
	}
	// Ordering: green (high perceived weight) > red > blue; all are brighter than black.
	black := relativeLuminance(tui.RGBColor(0, 0, 0))
	green := relativeLuminance(tui.RGBColor(0, 255, 0))
	red := relativeLuminance(tui.RGBColor(255, 0, 0))
	blue := relativeLuminance(tui.RGBColor(0, 0, 255))
	if !(green > red && red > blue && blue > black) {
		t.Errorf("luminance ordering broken: green=%f red=%f blue=%f black=%f", green, red, blue, black)
	}
}

// TestContrastRatio covers the reference ratios: identical colours ratio 1, and
// black/white ratio 21.
func TestContrastRatio(t *testing.T) {
	black := tui.RGBColor(0, 0, 0)
	white := tui.RGBColor(255, 255, 255)
	if got := contrastRatio(black, black); math.Abs(got-1) > 1e-9 {
		t.Errorf("contrastRatio(black,black) = %f, want 1", got)
	}
	if got := contrastRatio(black, white); math.Abs(got-21) > 1e-9 {
		t.Errorf("contrastRatio(black,white) = %f, want 21", got)
	}
	// Order-independent.
	if contrastRatio(white, black) != contrastRatio(black, white) {
		t.Error("contrastRatio is not symmetric")
	}
}

// TestDialogTextFG checks the readability fix for issue #98. Each case sets the
// dialog background the way ApplyTheme would, then asserts the property that
// matters: the returned foreground clears minDialogContrast (or passes through
// untouched when it is a terminal default), while a foreground that already meets
// the threshold is never altered.
func TestDialogTextFG(t *testing.T) {
	// Snapshot and restore the shared theme so the global is left untouched for
	// the rest of the suite.
	saved := tv.DefaultTheme
	t.Cleanup(func() { tv.DefaultTheme = saved })

	// Spot-check the reported defect: bright yellow tool text on the default
	// theme's light-gray background must come back darkened (different colour)
	// and readable.
	t.Run("yellow on light gray is darkened and readable", func(t *testing.T) {
		tv.DefaultTheme.DialogBG = tui.ANSIColor(7)
		got := dialogTextFG(tui.ANSIColor(11))
		if got == tui.ANSIColor(11) {
			t.Fatal("bright yellow was not adjusted on light gray")
		}
		if cr := contrastRatio(got, tv.DefaultTheme.DialogBG); cr < minDialogContrast {
			t.Errorf("adjusted yellow contrast %0.2f < %0.1f", cr, minDialogContrast)
		}
	})

	for _, tc := range []struct {
		name     string
		dialogBG tui.Color
		fg       tui.Color
	}{
		{"bright green on light gray", tui.ANSIColor(7), tui.ANSIColor(10)},
		{"bright cyan on light gray", tui.ANSIColor(7), tui.ANSIColor(14)},
		{"bright blue on light gray", tui.ANSIColor(7), tui.ANSIColor(12)},
		{"bright red on light gray", tui.ANSIColor(7), tui.ANSIColor(9)},
		// Black body text on light gray already passes; left unchanged.
		{"black on light gray unchanged", tui.ANSIColor(7), tui.ANSIColor(0)},
		// High-contrast preset: bright Okabe colours on black already read well.
		{"okabe orange on black unchanged", tui.RGBColor(0, 0, 0), okabeOrange},
		{"okabe yellow on black unchanged", tui.RGBColor(0, 0, 0), okabeYellow},
		{"white on black unchanged", tui.RGBColor(0, 0, 0), tui.RGBColor(255, 255, 255)},
		// Dark text that would be lost on a dark background is lightened.
		{"black on black lightened", tui.RGBColor(0, 0, 0), tui.RGBColor(0, 0, 0)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tv.DefaultTheme.DialogBG = tc.dialogBG
			got := dialogTextFG(tc.fg)
			concrete := tc.fg.Mode != tui.ColorDefault && tc.dialogBG.Mode != tui.ColorDefault
			alreadyOK := concrete && contrastRatio(tc.fg, tc.dialogBG) >= minDialogContrast
			switch {
			case !concrete:
				if got != tc.fg {
					t.Errorf("default colour adjusted: got %v want %v", got, tc.fg)
				}
			case alreadyOK:
				if got != tc.fg {
					t.Errorf("already-readable colour changed: got %v want %v", got, tc.fg)
				}
			default:
				if cr := contrastRatio(got, tc.dialogBG); cr < minDialogContrast {
					t.Errorf("contrast %0.2f < %0.1f for %v on %v (got %v)", cr, minDialogContrast, tc.fg, tc.dialogBG, got)
				}
			}
		})
	}
}

// TestDialogTextFGPreservesHue checks the adjustment shifts the colour toward the
// opposite extreme rather than snapping to black/white outright, so a yellow
// command stays recognisably yellow (olive) rather than becoming black.
func TestDialogTextFGPreservesHue(t *testing.T) {
	saved := tv.DefaultTheme
	t.Cleanup(func() { tv.DefaultTheme = saved })
	tv.DefaultTheme.DialogBG = tui.ANSIColor(7) // light gray

	got := dialogTextFG(tui.RGBColor(255, 255, 0)) // pure yellow
	r, g, b := colorRGB(got)
	// Yellow kept its red+green channels above blue (still a warm/olive tone),
	// and was darkened rather than lightened.
	if r <= b || g <= b {
		t.Errorf("adjusted colour lost its warm hue: r=%d g=%d b=%d", r, g, b)
	}
	if r > 200 && g > 200 {
		t.Errorf("yellow was not darkened: r=%d g=%d b=%d", r, g, b)
	}
}

// TestDialogTextFGNoColorPassthrough confirms NO_COLOR foregrounds (terminal
// default) are returned untouched regardless of the background.
func TestDialogTextFGNoColorPassthrough(t *testing.T) {
	saved := tv.DefaultTheme
	t.Cleanup(func() { tv.DefaultTheme = saved })
	tv.DefaultTheme.DialogBG = tui.ANSIColor(7)
	if got := dialogTextFG(tui.DefaultColor()); got != tui.DefaultColor() {
		t.Errorf("default fg adjusted: got %v", got)
	}
	tv.DefaultTheme.DialogBG = tui.DefaultColor()
	if got := dialogTextFG(tui.ANSIColor(11)); got != tui.ANSIColor(11) {
		t.Errorf("fg adjusted under default bg: got %v", got)
	}
}
