package ui

import (
	tv "github.com/hobbestherat/turbotui/turbotv"
)

// buttonChrome is the number of columns turbotui draws around a button's label:
// the "[ " prefix and " ]" suffix (see turbotui/turbotv/widget_button.go). A
// button needs at least len(label without mnemonic) + buttonChrome columns to
// render without clipping its text.
const buttonChrome = 4

// buttonLabelWidth returns the column width a turbotui button needs to display
// label without clipping: the label with its '&' mnemonic marker removed, plus
// the button chrome ("[ " ... " ]"). It mirrors the rendering in
// turbotui/turbotv/widget_button.go so declared widths match what is drawn.
func buttonLabelWidth(label string) int {
	return cleanMnemonicRunes(label) + buttonChrome
}

// cleanMnemonicRunes returns the number of visible runes in label after removing
// '&' mnemonic markers, matching turbotv.parseMnemonic: a single '&' before
// another rune marks a hotkey and is dropped; "&&" is a literal '&'; a trailing
// '&' with no following rune is treated as literal.
func cleanMnemonicRunes(label string) int {
	runes := []rune(label)
	n := 0
	for i := 0; i < len(runes); i++ {
		if runes[i] == '&' && i+1 < len(runes) {
			if runes[i+1] == '&' {
				n++ // literal '&'
				i++
			}
			continue // mnemonic marker dropped
		}
		n++
	}
	return n
}

// footerButtonRects lays out a right-aligned row of action buttons for a dialog
// footer. Each button is sized to its rendered label width (buttonLabelWidth),
// the group is right-aligned so the last button's right edge sits on rightX, and
// neighbouring buttons are separated by gap blank columns. All rects share row y
// and are clamped to [leftX, rightX] so they never start before leftX, run past
// rightX, or carry a negative width — even when a too-narrow dialog cannot hold
// the whole group.
//
// labels are taken left-to-right; the last label ends up rightmost. Pass them in
// display order, e.g. []string{"Export &CSV", "Export &JSON", "Close"}. The
// caller is expected to size its dialog so the group fits (the statistics dialog
// floors its width well above the group's needs); the clamping is a safety net.
func footerButtonRects(labels []string, leftX, rightX, y, gap int) []tv.Rect {
	rects := make([]tv.Rect, len(labels))
	rightEdge := rightX
	for i := len(labels) - 1; i >= 0; i-- {
		w := buttonLabelWidth(labels[i])
		x := rightEdge - w + 1
		rects[i] = clampDialogRect(tv.Rect{X: x, Y: y, W: w, H: 1}, leftX, rightX)
		rightEdge = x - gap - 1
	}
	return rects
}

// clampDialogRect keeps r inside [leftX, rightX]: the left edge never crosses
// leftX, the right edge never crosses rightX, and the width is never negative.
// It is the guard that lets a too-narrow dialog degrade to clipping instead of
// producing garbled (negative-width or border-crossing) rectangles.
func clampDialogRect(r tv.Rect, leftX, rightX int) tv.Rect {
	if r.X < leftX {
		r.X = leftX
	}
	if right := r.X + r.W - 1; right > rightX {
		r.W = rightX - r.X + 1
	}
	if r.W < 0 {
		r.W = 0
	}
	return r
}
