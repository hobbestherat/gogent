package ui

import (
	tui "github.com/hobbestherat/turbotui"
	tv "github.com/hobbestherat/turbotui/turbotv"
)

// drawDialogVScrollbar paints a 1-column vertical scrollbar over track: a full-height
// │ line with ▲/▼ caps and a single █ thumb whose position reflects offset within
// [0, total-visible]. It mirrors turbotui's shared (but unexported) text-view/tree
// scrollbar so a hand-rolled scroll viewport's affordance looks and behaves like the
// rest of the UI. It is shared by the theme editor (showThemeEditor) and the question
// dialog (buildTopicPanel), both of which host interactive child widgets inside a
// scrolling tv.Component and so cannot reuse the read-only TextView's internal bar.
// With nothing to scroll (total <= visible) only the track and caps are drawn.
func drawDialogVScrollbar(surface tv.Surface, track tv.Rect, total, visible, offset int, fg, bg tui.Color) {
	if track.H < 1 {
		return
	}
	x := track.X
	for row := 0; row < track.H; row++ {
		surface.SetCell(x, track.Y+row, tui.Cell{Ch: '│', FG: fg, BG: bg})
	}
	surface.SetCell(x, track.Y, tui.Cell{Ch: '▲', FG: fg, BG: bg})
	surface.SetCell(x, track.Bottom(), tui.Cell{Ch: '▼', FG: fg, BG: bg})
	span := total - visible
	inner := track.H - 2 // rows between the two arrow caps
	if span <= 0 || inner <= 0 {
		return
	}
	if offset < 0 {
		offset = 0
	}
	if offset > span {
		offset = span
	}
	thumb := offset * (inner - 1) / span
	if thumb < 0 {
		thumb = 0
	}
	if thumb > inner-1 {
		thumb = inner - 1
	}
	surface.SetCell(x, track.Y+1+thumb, tui.Cell{Ch: '█', FG: fg, BG: bg})
}

// buttonChrome is the number of columns turbotui draws around a button's label:
// the "[ " prefix and " ]" suffix (see turbotui/turbotv/widget_button.go). A
// button needs at least StringWidth(label without mnemonic) + buttonChrome
// columns to render without clipping its text. Button widths themselves come from
// tv.ButtonLabelWidth (which also floors short captions); this constant is kept
// only for the chrome-aware label elision in fitButtonLabel.
const buttonChrome = 4

// footerButtonRects lays out a right-aligned row of action buttons for a dialog
// footer. Each button is sized to its rendered label width (tv.ButtonLabelWidth),
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
		w := tv.ButtonLabelWidth(labels[i])
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
