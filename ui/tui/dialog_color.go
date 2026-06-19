package ui

import (
	"math"

	tui "github.com/hobbestherat/turbotui"
	tv "github.com/hobbestherat/turbotui/turbotv"
)

// minDialogContrast is the WCAG contrast ratio dialog text must clear (WCAG AA
// for normal text). A foreground that falls short is stepped toward black or
// white until it clears this, preserving as much of the original hue as possible.
const minDialogContrast = 4.5

// dialogTextFG returns fg adjusted, when necessary, so it stays legible against
// the active dialog background. The semantic transcript colours — e.g. the bright
// yellow used for tool/shell commands — are tuned for the dark chat background;
// painted straight onto the default theme's light-gray dialog they collapse to
// near-invisible (issue #98). Any foreground below minDialogContrast is darkened
// on a light background or lightened on a dark one in steps that keep the hue,
// falling back to pure black or white for the maximum possible contrast.
//
// It reads tv.DefaultTheme at call time — dialog bodies are built on the UI
// thread after ApplyTheme has installed the active theme — so the adjustment
// tracks the resolved theme, including NO_COLOR (terminal-default passthrough)
// and the high-contrast preset (already high-contrast, left untouched).
func dialogTextFG(fg tui.Color) tui.Color {
	bg := tv.DefaultTheme.DialogBG
	// Terminal-default colours are unknown, so contrast cannot be reasoned about;
	// trust whatever the terminal renders (NO_COLOR lands here).
	if fg.Mode == tui.ColorDefault || bg.Mode == tui.ColorDefault {
		return fg
	}
	if contrastRatio(fg, bg) >= minDialogContrast {
		return fg
	}

	cr, cg, cb := colorRGB(fg)
	// The WCAG ratio rewards shrinking the darker colour far more than growing
	// the lighter one, so the winning direction is set by the background alone:
	// darken toward black on a light/medium background, lighten toward white only
	// on a very dark one. Lightening can never beat darkening once the background
	// clears ~0.18 luminance (black on such a background already clears the
	// threshold); the threshold is that crossover. Stepping away from the
	// background keeps the hue, so the first shade to clear it is the
	// least-shifted acceptable one.
	toward := uint8(0)
	if relativeLuminance(bg) < 0.18 {
		toward = 255
	}
	best := fg
	for i := 1; i <= 20; i++ {
		f := float64(i) / 20
		cand := tui.RGBColor(blendChannel(cr, toward, f), blendChannel(cg, toward, f), blendChannel(cb, toward, f))
		if contrastRatio(cand, bg) >= minDialogContrast {
			return cand
		}
		best = cand
	}
	return best // pure black or white: the most contrast this pair can reach
}

// contrastRatio is the WCAG contrast ratio between two colours: (lighter+0.05) /
// (darker+0.05), ranging from 1 (identical) to 21 (black on white).
func contrastRatio(a, b tui.Color) float64 {
	la, lb := relativeLuminance(a), relativeLuminance(b)
	if la < lb {
		la, lb = lb, la
	}
	return (la + 0.05) / (lb + 0.05)
}

// relativeLuminance is the WCAG relative luminance of a colour (0..1), weighting
// channels by perceived brightness and gamma-decoding the sRGB values.
func relativeLuminance(c tui.Color) float64 {
	r, g, b := colorRGB(c)
	return 0.2126*lin(float64(r)/255) + 0.7152*lin(float64(g)/255) + 0.0722*lin(float64(b)/255)
}

// lin gamma-decodes one normalised sRGB channel (0..1) to its linear value.
func lin(c float64) float64 {
	if c <= 0.03928 {
		return c / 12.92
	}
	return math.Pow((c+0.055)/1.055, 2.4)
}

// blendChannel interpolates between from and to by factor f in [0,1].
func blendChannel(from, to uint8, f float64) uint8 {
	return uint8(math.Round(float64(from)*(1-f) + float64(to)*f))
}

// colorRGB returns the 8-bit RGB of a concrete colour. ANSI colours resolve via
// the canonical 16-colour table and the xterm 256-cube/grayscale ramp defined in
// theme.go, so an ANSI-index foreground is measured against its rendered colour
// rather than its index.
func colorRGB(c tui.Color) (r, g, b uint8) {
	switch c.Mode {
	case tui.ColorRGB:
		return uint8(c.Value >> 16), uint8(c.Value >> 8), uint8(c.Value)
	case tui.ColorANSI:
		idx := uint8(c.Value)
		if idx < 16 {
			col := ansi16RGB[idx]
			return col[0], col[1], col[2]
		}
		return xterm256ToRGB(idx)
	default:
		return 0, 0, 0
	}
}
